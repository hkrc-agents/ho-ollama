#!/usr/bin/env python3
"""Serialize HAP builds and reject source changes during compilation."""
import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import subprocess

ADAPTER = Path(__file__).resolve().parents[1]
APP = ADAPTER/'hap'

def inputs():
    result = {}
    for directory, dirs, files in os.walk(APP):
        dirs[:] = [d for d in dirs if d not in {'build', 'oh_modules', 'node_modules', '.hvigor', '.cxx', '.git'}]
        for name in sorted(files):
            path = Path(directory)/name
            result[str(path.relative_to(APP))] = hashlib.sha256(path.read_bytes()).hexdigest()
    return result

def main():
    global APP
    parser = argparse.ArgumentParser()
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--app', type=Path, help='Isolated diagnostic source copy, not the normal APP')
    parser.add_argument('--mode', choices=['debug', 'release'], default='debug')
    parser.add_argument('--tools-dir', type=Path, default=Path(os.environ.get('HARMONY_TOOLS_BIN', 'command-line-tools/bin')))
    parser.add_argument('--java-home', type=Path, default=Path(os.environ.get('JAVA_HOME', os.environ.get('JAVA_HOME', '/usr/lib/jvm/default-java'))))
    parser.add_argument('--sdk-root', type=Path)
    args = parser.parse_args()
    if args.app: APP = args.app.resolve()
    args.out.mkdir(parents=True, exist_ok=True)
    lock_path = Path.home()/'.cache/ho-ollama/hap-build.lock'
    lock_path.parent.mkdir(parents=True, exist_ok=True)
    with lock_path.open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        build(args)

def build(args):
    java = args.java_home.resolve()
    env = dict(os.environ, JAVA_HOME=str(java), PATH=str(java/'bin')+':'+os.environ['PATH'])
    if args.sdk_root:
        env['DEVECO_SDK_HOME'] = str(args.sdk_root.resolve())
    with (args.out/'ohpm.log').open('w') as log:
        subprocess.run([str(args.tools_dir.resolve()/'ohpm'), 'install'], cwd=APP, env=env, stdout=log, stderr=subprocess.STDOUT, check=True, timeout=300)
    before = inputs()
    with (args.out/'build.log').open('w') as log:
        result = subprocess.run([str(args.tools_dir.resolve()/'hvigorw'), '--no-daemon', '-p', 'buildMode='+args.mode, 'assembleHap'],
                                cwd=APP, env=env, stdout=log, stderr=subprocess.STDOUT, timeout=1200)
    after = inputs()
    unchanged = before == after
    evidence = {'inputs': before, 'inputs_unchanged': unchanged, 'build_exit': result.returncode, 'mode': args.mode}
    hap = APP/'entry/build/default/outputs/default/entry-default-unsigned.hap'
    if result.returncode == 0 and unchanged:
        evidence['unsigned_hap_sha256'] = hashlib.sha256(hap.read_bytes()).hexdigest()
    (args.out/'identity.json').write_text(json.dumps(evidence, indent=2)+'\n')
    if result.returncode or not unchanged:
        raise SystemExit('HAP build failed or source changed during build; candidate rejected')

if __name__ == '__main__': main()
