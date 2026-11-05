"""Prepared APP lifecycle regressions with required real native fixtures.

Does not prepare upstream, deploy, change permissions or run model inference.
Keep fixture setup and actual nonempty JSON test results in one evidence chain.
"""
import argparse
import collections
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import time

ROOT = Path(__file__).resolve().parents[1]


def python_suite_passed(output, minimum):
    count = re.search(r'Ran (\d+) tests?', output)
    return bool(count and int(count[1]) >= minimum and
                not re.search(r'(?:skipped|expected failures|unexpected successes)=[1-9]', output))


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--source', type=Path, required=True)
    p.add_argument('--go', type=Path, required=True)
    p.add_argument('--out', type=Path, required=True)
    a = p.parse_args(); os.environ['PICO_GO'] = str(a.go.resolve()); a.out.mkdir(parents=True, exist_ok=False)
    lock_path = Path.home()/'.cache/ho-ollama/runtime-build.lock'
    lock_path.parent.mkdir(parents=True, exist_ok=True)
    lock = lock_path.open('a')
    fcntl.flock(lock, fcntl.LOCK_EX)
    result = {'pass': False, 'commands': [], 'scope': 'Linux host APP/server tests, not device qualification'}
    def identity():
        return {str(path.relative_to(ROOT)): hashlib.sha256(path.read_bytes()).hexdigest()
                for path in sorted((ROOT/'app').glob('*.go'))}
    before = identity()
    def run(name, argv, cwd=ROOT, env=None):
        start = time.monotonic()
        with (a.out/(name+'.log')).open('wb') as log:
            r = subprocess.run(list(map(str, argv)), cwd=cwd, env=env, stdout=log, stderr=subprocess.STDOUT, timeout=240)
        result['commands'].append({'name': name, 'argv': list(map(str, argv)), 'exit': r.returncode, 'seconds': time.monotonic()-start})
        if r.returncode: raise RuntimeError(name+' failed; see raw log')
        minimum = {'native-bridge': 1, 'shutdown-contract': 3, 'packaging-contract': 3, 'documentation-contract': 3,
                   'native-transfer-contract': 2}.get(name)
        if minimum and not python_suite_passed((a.out/(name+'.log')).read_text(), minimum):
            raise RuntimeError(name+' did not execute the required nonskipped tests')
    try:
        prepared = {}
        for name in ['main.go', 'services_harmony.go', 'core_control_harmony.go',
                     'shutdown_harmony.go', 'shutdown_harmony_test.go']:
            canonical = ROOT/'app'/name
            actual = a.source/'app/cmd/harmony'/name
            digest = hashlib.sha256(actual.read_bytes()).hexdigest()
            assert digest == hashlib.sha256(canonical.read_bytes()).hexdigest(), 'Prepared source mismatch: '+name
            prepared[name] = digest
        result['prepared_sha256'] = prepared
        run('go-version', [a.go, 'version'], a.source)
        native = {'service-helper': ROOT/'testdata/service_helper_native.c',
                  'service-launcher': ROOT/'service_launcher.c',
                  'storage-probe': ROOT/'storage_probe.c'}
        for name, source in native.items():
            run(name, ['cc', '-O2', '-Wall', '-Wextra', '-Werror', source, '-o', (a.out/name).resolve()])
        env = dict(os.environ, GOTOOLCHAIN='local', GOOS='linux', GOARCH='amd64', CGO_ENABLED='1',
                   CC='cc', CXX='c++', GOFLAGS='-mod=readonly',
                   PICO_SERVICE_HELPER_BINARY=str((a.out/'service-helper').resolve()),
                   PICO_SERVICE_LAUNCHER_SOURCE=str(native['service-launcher']),
                   PICO_STORAGE_PROBE_BINARY=str((a.out/'storage-probe').resolve()),
                   PICO_STORAGE_PROBE_LAUNCHER=str((a.out/'service-launcher').resolve()))
        run('go-race', [a.go, 'test', '-json', '-race', '-count=1', '-tags=harmonyos,netcgo',
                        './app/cmd/harmony', './app/server'], a.source, env)
        counts = collections.Counter(); packages = {}; tests = set()
        for line in (a.out/'go-race.log').read_text().splitlines():
            try: row = json.loads(line)
            except ValueError: continue
            action = row.get('Action')
            if action not in {'pass', 'fail', 'skip'}: continue
            if row.get('Test'):
                counts[action] += 1; tests.add((row['Package'], row['Test']))
            else: packages[row['Package']] = action
        assert len(packages) == 2 and set(packages.values()) == {'pass'} and counts['pass'] > 0 and not counts['fail'] and not counts['skip']
        assert any(name == 'TestShutdownRetryDoesNotAcknowledgeFailure' for _, name in tests)
        run('portable-contracts', ['python3', '-m', 'unittest', 'discover', '-s', str(ROOT/'tests'), '-v'])
        if not python_suite_passed((a.out/'portable-contracts.log').read_text(), 22):
            raise RuntimeError('Portable contract tests did not execute')
        assert before == identity(), 'Source changed during testing'
        result.update(pass_=True, packages=packages, test_events_including_subtests=dict(counts), source_sha256=before)
        result['pass'] = result.pop('pass_')
    except Exception as error:
        result['error'] = str(error)
    (a.out/'result.json').write_text(json.dumps(result, indent=2)+'\n')
    print(json.dumps({k: v for k, v in result.items() if k not in {'source_sha256', 'commands'}}))
    raise SystemExit(0 if result['pass'] else 1)


if __name__ == '__main__': main()
