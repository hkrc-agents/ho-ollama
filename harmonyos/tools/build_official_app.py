#!/usr/bin/env python3
"""Build upstream React/Go APP and add it to a qualified runtime payload."""
import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import time

ADAPTER = Path(__file__).resolve().parents[1]

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--source', type=Path, default=Path.home()/'.cache/ho-ollama/sources/ollama')
    parser.add_argument('--runtime-payload', type=Path, required=True)
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--skip-npm-ci', action='store_true')
    parser.add_argument('--preserve-cli', action='store_true',
                        help='Keep the hash-verified base CLI for an APP-only candidate')
    parser.add_argument('--base-app-identity', type=Path,
                        help='Explicit identity of a later APP payload layered on the pinned CPU runtime')
    args = parser.parse_args()
    source, output = args.source.resolve(), args.out.resolve()
    payload = args.runtime_payload.resolve()
    if output.exists():
        parser.error('--out must be a new directory')
    if not (payload/'bin/ollama').is_file() or output == payload or payload in output.parents:
        raise SystemExit('Expected a separate existing base payload and output directory')
    lock_path = Path.home()/'.cache/ho-ollama/runtime-build.lock'
    lock_path.parent.mkdir(parents=True, exist_ok=True)
    with lock_path.open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        build(args, source, output, payload)


def build(args, source, output, payload):
    base_identity_path = args.base_app_identity or payload.parent/'runtime-identity.json'
    base_identity = json.loads(base_identity_path.read_text())
    if args.base_app_identity:
        layered = base_identity
        metadata = layered['base_identity']
        base_identity = {key: metadata[key] for key in ('ollama_commit', 'runner_commit', 'arch')}
        base_identity.update(files=layered['files'], layered_app_identity={
            'path': str(base_identity_path.resolve()),
            'sha256': hashlib.sha256(base_identity_path.read_bytes()).hexdigest()})
    pins = json.loads((ADAPTER/'upstream-lock.json').read_text())
    if (base_identity.get('ollama_commit') != pins['ollama']['commit'] or
        base_identity.get('runner_commit') != pins['runner']['commit'] or
        base_identity.get('arch') != 'x86_64'):
        raise SystemExit('Base runtime identity does not match the pinned emulator target')
    for name, digest in base_identity['files'].items():
        path = payload/name
        if not path.is_relative_to(payload) or hashlib.sha256(path.read_bytes()).hexdigest() != digest:
            raise SystemExit('Base payload hash mismatch: '+name)
    output.mkdir(parents=True, exist_ok=True)
    runs = output/str(time.time_ns())
    runs.mkdir()
    def run(command, cwd=source, env=None, log='build.log'):
        with (runs/log).open('w') as handle:
            subprocess.run([str(c) for c in command], cwd=cwd, env=env,
                           stdout=handle, stderr=subprocess.STDOUT, check=True)
    run(['python3', ADAPTER/'tools/prepare_official_app.py', '--source', source,
         '--evidence', runs], cwd=ADAPTER, log='prepare.log')
    node_version = subprocess.check_output(['node', '--version'], text=True).strip()
    if int(node_version.lstrip('v').split('.')[0]) < 22:
        raise RuntimeError('Node 22+ is required; select it in PATH')
    web = source/'app/ui/app'
    if not args.skip_npm_ci:
        run(['npm', 'ci', '--no-audit', '--no-fund'], cwd=web, log='npm-ci.log')
    run(['node', '--input-type=module', '-e',
         'import {generator,getConfig} from "@tanstack/router-generator"; await generator(getConfig(),process.cwd());'],
        cwd=web, log='route-generation.log')
    run(['npm', 'run', 'build'], cwd=web, log='frontend-build.log')
    adaptation_path = runs/'source-adaptation.json'
    adaptation = json.loads(adaptation_path.read_text())
    dist = {str(f.relative_to(web/'dist')): hashlib.sha256(f.read_bytes()).hexdigest()
            for f in sorted((web/'dist').rglob('*')) if f.is_file()}
    js = b'\n'.join(f.read_bytes() for f in (web/'dist').rglob('*.js'))
    if not dist or not all(marker in js for marker in [b'Choose custom storage folder', b'Use default private folder', b'Storage mode:']):
        raise RuntimeError('Missing/stale frontend distribution')
    adaptation.update(frontend_dist=dist, frontend_contract=True)
    adaptation_path.write_text(json.dumps(adaptation, indent=2)+'\n')
    sdk = Path(os.environ['HARMONY_NATIVE_SDK'])
    go = Path(os.environ.get('PICO_GO', 'go'))
    env = dict(os.environ, GOOS='linux', GOARCH='amd64', CGO_ENABLED='1', GOTOOLCHAIN='local',
               CC=f'{sdk}/llvm/bin/clang --target=x86_64-linux-ohos --sysroot={sdk}/sysroot',
               CXX=f'{sdk}/llvm/bin/clang++ --target=x86_64-linux-ohos --sysroot={sdk}/sysroot')
    # Normally rebuild both. An APP-only candidate explicitly retains the
    # verified CLI, so it must remain compatible with that onboarding contract.
    run([go, 'build', '-tags=harmonyos,netcgo', '-o', runs/'ollama-app', './app/cmd/harmony'], env=env, log='go-app.log')
    if not args.preserve_cli:
        run([go, 'build', '-tags=harmonyos,netcgo', '-o', runs/'ollama', '.'], env=env, log='go-cli.log')
    target = output/'payload'
    shutil.copytree(payload, target, dirs_exist_ok=True)
    for name in ('ollama-app', 'ollama'):
        if name == 'ollama' and args.preserve_cli:
            continue
        shutil.copy2(runs/name, target/'bin'/name)
    launcher_source = ADAPTER/'service_launcher.c'
    run([sdk/'llvm/bin/clang', '--target=x86_64-linux-ohos23', '--sysroot='+str(sdk/'sysroot'),
         '-O2', '-Wall', '-Wextra', '-Werror', '-fPIE', '-pie', launcher_source,
         '-o', target/'bin/pico-service-launcher'], log='service-launcher.log')
    storage_probe_source = ADAPTER/'storage_probe.c'
    run([sdk/'llvm/bin/clang', '--target=x86_64-linux-ohos23', '--sysroot='+str(sdk/'sysroot'),
         '-O2', '-Wall', '-Wextra', '-Werror', '-fPIE', '-pie', storage_probe_source,
         '-o', target/'bin/pico-storage-probe'], log='storage-probe.log')
    files = {str(p.relative_to(target)): hashlib.sha256(p.read_bytes()).hexdigest()
             for p in sorted(target.rglob('*')) if p.is_file()}
    identity = {'source': json.loads((runs/'source-adaptation.json').read_text()),
                'cli_preserved_from_verified_base': args.preserve_cli,
                'service_launcher_source_sha256': hashlib.sha256(launcher_source.read_bytes()).hexdigest(),
                'storage_probe_source_sha256': hashlib.sha256(storage_probe_source.read_bytes()).hexdigest(),
                'base_payload': str(payload), 'base_identity': base_identity,
                'files': files, 'build_logs': str(runs),
                'qualification': 'build only; device acceptance required'}
    (output/'official-app-identity.json').write_text(json.dumps(identity, indent=2)+'\n')
    print(target)

if __name__ == '__main__':
    main()
