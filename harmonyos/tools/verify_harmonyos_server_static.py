#!/usr/bin/env python3
"""Bounded installed-APP standalone HTTP asset checks (explicit emulator only).

Execution uses HDC; preparing/compiling this script does not. Requires an open
normal APP, ready core, and a stopped standalone with its existing bounded config.
No installation, supervisor, downloads, generation, or model/config selection.
Runtime hashes are APP-recorded hashes compared with the supplied build identity;
they are not a new independent remote ELF hash measurement.
"""
import argparse
import fcntl
import hashlib
import json
from pathlib import Path
import re
import time
import urllib.error
import urllib.request
import zlib

from harmonyos_ui import HarmonyUI

FILES = 'data/storage/el2/base/haps/entry/files/'
LIMIT = 2 * 1024 * 1024
PATHS = ['/', '/index.html', '/sw.js', '/manifest.webmanifest', '/_app/version.json']
HASHES = {'ollama': 'bin/ollama', 'ollama-app': 'bin/ollama-app',
          'llama-server': 'lib/ollama/llama-server',
          'pico-service-launcher': 'bin/pico-service-launcher',
          'pico-storage-probe': 'bin/pico-storage-probe'}


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--runtime-identity', type=Path, required=True)
    parser.add_argument('--target', choices=['127.0.0.1:5555'], required=True)
    args = parser.parse_args()
    expected = json.loads(args.runtime_identity.read_text())['files']
    for relative in HASHES.values():
        if not re.fullmatch(r'[0-9a-f]{64}', expected[relative]):
            parser.error('Invalid expected runtime hash: '+relative)
    args.out.mkdir(parents=True, exist_ok=False)
    result = {'pass': False, 'scenario_pass': False, 'cleanup_pass': False,
              'target': args.target, 'requests': [], 'cleanup_errors': [],
              'scope': 'installed standalone HTTP assets; no browser/JS execution',
              'no_ui': {'status': 'blocked',
                        'reason': 'Private service config has no --no-ui or API-key field; not tested'},
              'runtime_hash_source': 'APP desktop/runtime-sha256.json',
              'generation_requested': False, 'configuration_changed_by_test': False}
    lock_path = Path.home()/'.cache/ho-ollama/emulator-client-test.lock'
    lock_path.parent.mkdir(parents=True, exist_ok=True)
    forwards = []
    token = ''
    original = None
    attempted_start = False
    ports = {}
    ui = HarmonyUI(args.out, target=args.target)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())

    def private(name):
        return ui.hdc('shell', '-b', 'com.pico.ollama', 'cat', FILES+name).strip()

    def http(port, path, *, body=None, headers=None, timeout=15):
        request = urllib.request.Request('http://127.0.0.1:'+str(port)+path,
            data=None if body is None else json.dumps(body).encode(),
            headers={'Accept-Encoding': 'gzip', 'Content-Type': 'application/json',
                     **(headers or {})})
        try:
            response = opener.open(request, timeout=timeout)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            raw = response.read(LIMIT+1)
            if len(raw) > LIMIT:
                raise RuntimeError('HTTP wire body exceeds 2 MiB: '+path)
            content = raw
            encoding = response.headers.get('Content-Encoding', '').lower()
            if encoding == 'gzip':
                decoder = zlib.decompressobj(16+zlib.MAX_WBITS)
                content = decoder.decompress(raw, LIMIT+1)
                if len(content) > LIMIT or not decoder.eof or decoder.unused_data:
                    raise RuntimeError('Oversized/incomplete/concatenated gzip: '+path)
            elif encoding not in ('', 'identity'):
                raise RuntimeError('Unsupported content encoding: '+encoding)
            return response.status, dict(response.headers), content, len(raw)

    def api(path='/api/harmony/services', body=None):
        code, _, content, _ = http(ports['app'], path, body=body,
                                  headers={'Cookie': 'token='+token}, timeout=110)
        if code != 200:
            raise RuntimeError('Private API '+path+' status '+str(code))
        return json.loads(content)

    def action(service, verb, config=None):
        return api(body={'service': service, 'action': verb, 'config': config or {}})

    def wait(service, wanted):
        deadline = time.monotonic()+35
        while True:
            state = api()
            if state[service]['state'] == wanted:
                return state
            if time.monotonic() >= deadline or (wanted == 'ready' and state[service]['state'] == 'failed'):
                raise RuntimeError('Service did not reach '+service+'/'+wanted)
            time.sleep(.3)

    def forward(name, remote):
        listing = ui.hdc('fport', 'ls')
        # HDC's listener need not share the WSL ephemeral-port namespace.
        # Use bounded dedicated ports, verify no existing ruler, and let HDC
        # report bind failure; a WSL bind(0) is not proof HDC can bind that port.
        local = {'app': 32335, 'core': 32334, 'standalone': 32382}[name]
        if 'tcp:'+str(local) in listing:
            raise RuntimeError('Allocated local forward already exists')
        output = ui.hdc('fport', 'tcp:'+str(local), 'tcp:'+str(remote))
        if 'OK' not in output:
            raise RuntimeError('Forward creation not confirmed: '+output)
        forwards.append((local, remote))
        ports[name] = local

    def core_health():
        code, _, data, _ = http(ports['core'], '/api/version')
        if code != 200:
            raise RuntimeError('Normal core HTTP is not ready')
        return json.loads(data)

    def hashes():
        actual = json.loads(private('desktop/runtime-sha256.json'))
        if any(actual.get(name) != expected[path] for name, path in HASHES.items()):
            raise RuntimeError('Installed runtime hash record differs from supplied identity')
        return actual

    with lock_path.open('a') as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            memory = ui.shell('cat /proc/meminfo')
            available = int(re.search(r'MemAvailable:\s+(\d+)', memory)[1])
            result['mem_available_kib'] = available
            if available < 1800*1024:
                raise RuntimeError('1800 MiB memory preflight failed')
            result['runtime_before'] = hashes()
            token = private('desktop/app-token')
            if not re.fullmatch(r'[0-9a-fA-F]{64}', token):
                raise RuntimeError('Unexpected private APP token format')
            forward('app', 11435)
            forward('core', 11434)
            original = api()
            result['before'] = original
            if original['ollama']['state'] != 'ready' or original['llama']['state'] != 'stopped':
                raise RuntimeError('Require ready normal core and stopped standalone; no active service takeover')
            result['core_version_before'] = core_health()
            config = original['llama']['config']
            if not (128 <= config['context'] <= 512 and 1 <= config['threads'] <= 3
                    and 1024 <= config['port'] <= 65535
                    and config['port'] not in (11434, 11435)):
                raise RuntimeError('Existing standalone configuration exceeds bounded test limits')
            if config['model'] not in [m['path'] for m in original['models']]:
                raise RuntimeError('Saved model is not in the active library inventory')
            # Restrict to the existing qualified 505 MiB fixture, not arbitrary models.
            if not config['model'].endswith('0f5b3fd77990f44761275bb8d990d7fa0860c9aa8ad56e0559e402a1f1b03f54'):
                raise RuntimeError('Saved model is not the bounded qualified Qwen fixture')
            for label, headers in [('missing-cookie', {}), ('wrong-cookie', {'Cookie': 'token=invalid'})]:
                code, _, _, _ = http(ports['app'], '/api/harmony/services', headers=headers)
                result['requests'].append({'endpoint': 'private-services', 'auth': label, 'status': code})
                if code != 403:
                    raise RuntimeError('Private service control is not protected')
            forward('standalone', config['port'])
            attempted_start = True
            action('llama', 'start', config)
            ready = wait('llama', 'ready')
            result['started'] = ready['llama']
            if ready['llama'].get('readiness_proof') not in (
                    'owned_child_bind_pipe_and_http', 'proc_socket_inode_and_http'):
                raise RuntimeError('Standalone lacks owned-instance readiness proof')
            logs = api('/api/harmony/services/logs')['log']
            (args.out/'standalone-start.log').write_text(logs)
            if 'PICO_SERVICE_LIMIT' not in logs or 'RLIMIT_AS=disabled' not in logs:
                raise RuntimeError('Launcher disabled-policy receipt missing')
            for label, headers in [('anonymous', {}), ('invalid-bearer', {'Authorization': 'Bearer invalid'})]:
                for index, path in enumerate(PATHS):
                    code, response_headers, content, wire_size = http(ports['standalone'], path, headers=headers)
                    (args.out/(label+'-'+str(index)+'.body')).write_bytes(content)
                    result['requests'].append({'path': path, 'auth': label, 'status': code,
                        'headers': response_headers, 'wire_bytes': wire_size, 'decoded_bytes': len(content),
                        'sha256': hashlib.sha256(content).hexdigest()})
            # Record all responses before evaluating, including failures/404s.
            assets = [r for r in result['requests'] if 'path' in r]
            if any(r['status'] != 200 or not r['decoded_bytes'] for r in assets):
                raise RuntimeError('Static asset status/body assertion failed; see individual responses')
            for label in ('anonymous', 'invalid-bearer'):
                for index in (0, 1):
                    if b'<!doctype html>' not in (args.out/(label+'-'+str(index)+'.body')).read_bytes().lower():
                        raise RuntimeError('Root/index did not return HTML')
            result['authentication_policy'] = 'Assets accessible anonymously and with invalid bearer; private controls require APP cookie'
            result['scenario_pass'] = True
        except Exception as error:
            result['error'] = str(error)
        finally:
            if attempted_start:
                try:
                    action('llama', 'stop')
                    state = wait('llama', 'stopped')
                    if state['llama']['config'] != original['llama']['config']:
                        # No separate config-save verb exists: restore via start/stop.
                        action('llama', 'start', original['llama']['config'])
                        wait('llama', 'ready')
                        action('llama', 'stop')
                        state = wait('llama', 'stopped')
                    if state['llama']['config'] != original['llama']['config']:
                        raise RuntimeError('Original standalone config not restored')
                    result['standalone_stopped_config_restored'] = True
                except Exception as error:
                    result['cleanup_errors'].append('standalone: '+str(error))
                try:
                    if api()['ollama']['state'] != 'ready':
                        action('ollama', 'start')
                    result['after'] = wait('ollama', 'ready')
                    result['core_version_after'] = core_health()
                    result['runtime_after'] = hashes()
                except Exception as error:
                    result['cleanup_errors'].append('normal core: '+str(error))
            for local, remote in reversed(forwards):
                try:
                    ui.hdc('fport', 'rm', 'tcp:'+str(local), 'tcp:'+str(remote))
                except Exception as error:
                    result['cleanup_errors'].append('forward: '+str(error))
            result['cleanup_pass'] = (attempted_start and not result['cleanup_errors']
                                      and result.get('standalone_stopped_config_restored', False)
                                      and 'core_version_after' in result)
            result['pass'] = result['scenario_pass'] and result['cleanup_pass']
            (args.out/'result.json').write_text(json.dumps(result, indent=2, ensure_ascii=False)+'\n')
    print(json.dumps({'pass': result['pass'], 'cleanup_pass': result['cleanup_pass'],
                      'no_ui': result['no_ui']}))
    return 0 if result['pass'] else 1


if __name__ == '__main__':
    raise SystemExit(main())
