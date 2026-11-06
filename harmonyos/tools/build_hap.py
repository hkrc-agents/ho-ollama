#!/usr/bin/env python3
"""Serialize HAP builds and reject source changes during compilation."""
import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess

ADAPTER = Path(__file__).resolve().parents[1]
APP = ADAPTER/'hap'
GENERATED_DIRS = {'build', 'oh_modules', 'node_modules', '.hvigor', '.cxx', '.git'}

def inputs(app):
    result = {}
    for directory, dirs, files in os.walk(app):
        dirs[:] = [d for d in dirs if d not in GENERATED_DIRS]
        for name in sorted(files):
            path = Path(directory)/name
            result[str(path.relative_to(app))] = hashlib.sha256(path.read_bytes()).hexdigest()
    return result

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--app', type=Path, help='Isolated diagnostic source copy, not the normal APP')
    parser.add_argument('--mode', choices=['debug', 'release'], default='debug')
    parser.add_argument('--tools-dir', type=Path, default=Path(os.environ.get('HARMONY_TOOLS_BIN', 'command-line-tools/bin')))
    parser.add_argument('--java-home', type=Path, default=Path(os.environ.get('JAVA_HOME', os.environ.get('JAVA_HOME', '/usr/lib/jvm/default-java'))))
    parser.add_argument('--sdk-root', type=Path)
    args = parser.parse_args()
    args.out = args.out.resolve()
    if args.out.is_relative_to(APP):
        parser.error('--out must be outside the HAP source directory')
    args.out.mkdir(parents=True, exist_ok=True)
    lock_path = Path.home()/'.cache/ho-ollama/hap-build.lock'
    lock_path.parent.mkdir(parents=True, exist_ok=True)
    with lock_path.open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        app = args.app.resolve() if args.app else args.out/'app'
        if args.app is None:
            shutil.copytree(APP, app, ignore=shutil.ignore_patterns(*GENERATED_DIRS, 'oh-package-lock.json5'))
        build(args, app)

def build(args, app):
    java = args.java_home.resolve()
    env = dict(os.environ, JAVA_HOME=str(java), PATH=str(java/'bin')+':'+os.environ['PATH'])
    if args.sdk_root:
        env['DEVECO_SDK_HOME'] = str(args.sdk_root.resolve())
    with (args.out/'ohpm.log').open('w') as log:
        subprocess.run([str(args.tools_dir.resolve()/'ohpm'), 'install'], cwd=app, env=env, stdout=log, stderr=subprocess.STDOUT, check=True, timeout=300)
    before = inputs(app)
    with (args.out/'build.log').open('w') as log:
        result = subprocess.run([str(args.tools_dir.resolve()/'hvigorw'), '--no-daemon', '-p', 'buildMode='+args.mode, 'assembleHap'],
                                cwd=app, env=env, stdout=log, stderr=subprocess.STDOUT, timeout=1200)
    after = inputs(app)
    unchanged = before == after
    evidence = {'inputs': before, 'inputs_unchanged': unchanged, 'build_exit': result.returncode, 'mode': args.mode}
    hap = app/'entry/build/default/outputs/default/entry-default-unsigned.hap'
    if result.returncode == 0 and unchanged:
        unsigned = args.out/hap.name
        shutil.copyfile(hap, unsigned)
        evidence['unsigned_hap_sha256'] = hashlib.sha256(unsigned.read_bytes()).hexdigest()
    (args.out/'identity.json').write_text(json.dumps(evidence, indent=2)+'\n')
    if result.returncode or not unchanged:
        raise SystemExit('HAP build failed or source changed during build; candidate rejected')

if __name__ == '__main__': main()
