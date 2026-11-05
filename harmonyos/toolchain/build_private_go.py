#!/usr/bin/env python3
"""Rebuild the opt-in candidate in fresh private ext4 storage; never edit stock."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

PINNED_STOCK_GO = '61e7455a40a2fdfcdab99e881cd30ba10e216e3d0f32ab5f8e59d10cac4ecf57'


def sha(path):
    h = hashlib.sha256()
    with path.open('rb') as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b''):
            h.update(block)
    return h.hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--stock', required=True, type=Path, help='Pinned Go 1.26.0 tree')
    parser.add_argument('--private-parent', required=True, type=Path, help='Existing private ext4 parent')
    parser.add_argument('--jobs', type=int, choices=range(1, 5), default=4)
    args = parser.parse_args()
    stock, parent = args.stock.resolve(strict=True), args.private_parent.resolve(strict=True)
    if sha(stock/'bin/go') != PINNED_STOCK_GO:
        parser.error('stock bin/go SHA does not match the pinned bootstrap')
    version = subprocess.check_output([stock/'bin/go', 'version'], text=True).strip()
    if version != 'go version go1.26.0 linux/amd64':
        parser.error('expected Go 1.26.0 linux/amd64 bootstrap')
    # GNU stat reports ext4 as ext2/ext3. Reject drvfs/9p/overlay/shared mounts.
    fs = subprocess.check_output(['stat', '-f', '-c', '%T', str(parent)], text=True).strip()
    if fs not in ('ext2/ext3', 'ext4'):
        parser.error('private parent must be ext4; observed '+fs)
    root = Path(tempfile.mkdtemp(prefix='go-gd-r3-rebuild-', dir=parent))
    candidate = root/'go'
    patch = Path(__file__).resolve().with_name('go1.26.0-ohos-amd64-gd-tls.patch')
    sources = [line[6:] for line in patch.read_text().splitlines() if line.startswith('+++ b/')]
    tools = ['bin/go', 'pkg/tool/linux_amd64/asm', 'pkg/tool/linux_amd64/compile',
             'pkg/tool/linux_amd64/link', 'pkg/tool/linux_amd64/cgo']
    identity = {'stock': str(stock), 'stock_version': version, 'private_root': str(root),
        'stock_tools': {p: sha(stock/p) for p in tools}, 'patch_sha256': sha(patch),
        'status': 'preparing', 'commands': [], 'production_qualified': False}
    try:
        shutil.copytree(stock, candidate, symlinks=True)
        shutil.copy2(patch, root/patch.name)
        for command in [['git', 'apply', '--check', str(root/patch.name)],
                        ['git', 'apply', str(root/patch.name)]]:
            identity['commands'].append({'argv': command, 'cwd': str(candidate)})
            subprocess.run(command, cwd=candidate, check=True)
        identity['source_files'] = {p: {'stock_sha256': sha(stock/p) if (stock/p).is_file() else None,
                                      'candidate_sha256': sha(candidate/p)} for p in sources}
        env = os.environ.copy()
        for key in ['GOROOT', 'GOOS', 'GOARCH', 'GOEXPERIMENT', 'GOTOOLDIR', 'GOFLAGS']:
            env.pop(key, None)
        settings = {'GOROOT_BOOTSTRAP': str(stock), 'GOMAXPROCS': str(args.jobs),
                    'GOFLAGS': '-p='+str(args.jobs), 'GOTOOLCHAIN': 'local',
                    'GOCACHE': str(root/'gocache'), 'GOENV': 'off'}
        env.update(settings)
        identity['commands'].append({'argv': ['bash', 'make.bash'],
            'cwd': str(candidate/'src'), 'environment': settings})
        with (root/'bootstrap.log').open('wb') as log:
            run = subprocess.run(['bash', 'make.bash'], cwd=candidate/'src', env=env,
                                 stdout=log, stderr=subprocess.STDOUT)
        identity['bootstrap_exit_code'] = run.returncode
        identity['bootstrap_log_sha256'] = sha(root/'bootstrap.log')
        if run.returncode:
            raise RuntimeError('bootstrap failed; inspect '+str(root/'bootstrap.log'))
        identity['candidate_tools'] = {p: sha(candidate/p) for p in tools}
        identity['license_sha256'] = sha(candidate/'LICENSE')
        if identity['stock_tools'] != {p: sha(stock/p) for p in tools}:
            raise RuntimeError('stock identity changed during build')
        identity['status'] = 'built_unqualified'
    except Exception as error:
        identity['status'], identity['error'] = 'failed', str(error)
        raise
    finally:
        (root/'identity.json').write_text(json.dumps(identity, indent=2)+'\n')
        print(root/'identity.json', flush=True)


if __name__ == '__main__':
    main()
