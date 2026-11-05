#!/usr/bin/env python3
"""Rebuild pinned upstream CPU runtime; keep all OHOS changes reviewable."""
import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

ADAPTER = Path(__file__).resolve().parents[1]
ROOT = ADAPTER.parent
LOCK = json.loads((ADAPTER / 'upstream-lock.json').read_text())
OLLAMA = LOCK['ollama']['commit']
RUNNER = LOCK['runner']['commit']
LIBRESSL_ARCHIVE_SHA256 = 'edf01aee24c65d69e6a9efcb9d44bcda682ff9d4f3bbbd95e794e1dfa90847b5'


def native_target(arch, native_only):
    if arch not in ('x86_64', 'arm64'):
        raise ValueError('unsupported OHOS native architecture')
    if arch != 'x86_64' and not native_only:
        raise ValueError('ARM64 native probe requires --native-only; Go/HAP qualification is separate')
    return {'arch': arch, 'triple': 'aarch64-linux-ohos' if arch == 'arm64' else 'x86_64-linux-ohos',
            'cmake_abi': 'arm64-v8a' if arch == 'arm64' else 'x86_64'}


def validate_native_output(arch, out):
    # An existing x86 payload can contain a Go CLI which --native-only would
    # otherwise leave behind and incorrectly label as an ARM64 bundle.
    if arch == 'arm64' and out.exists():
        raise ValueError('ARM64 compile probe requires a fresh output directory')


def run(*args, cwd=None, env=None):
    print('+', *map(str, args), flush=True)
    subprocess.run(list(map(str, args)), cwd=cwd, env=env, check=True)


def checkout(path, url, commit):
    if not path.exists():
        run('git', 'clone', '--filter=blob:none', '--no-checkout', url, path)
        run('git', 'checkout', '--detach', commit, cwd=path)
    actual = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=path, text=True).strip()
    if actual != commit:
        raise RuntimeError(f'{path}: expected {commit}, got {actual}; use a separate source directory')


def patch(path, file):
    if subprocess.run(['git', 'apply', '--reverse', '--check', str(file)], cwd=path,
                      stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0:
        return
    run('git', 'apply', '--check', file, cwd=path)
    run('git', 'apply', file, cwd=path)


def materialize_source(objects, commit, destination):
    """Use only pinned Git objects; never copy tracked edits or untracked files."""
    actual = subprocess.check_output(['git', 'rev-parse', commit+'^{commit}'], cwd=objects, text=True).strip()
    if actual != commit or destination.exists():
        raise RuntimeError('Expected a pinned commit and a fresh source destination')
    run('git', 'clone', '--no-hardlinks', '--single-branch', '--no-checkout', objects, destination)
    run('git', 'checkout', '--detach', commit, cwd=destination)
    if subprocess.check_output(['git', 'status', '--porcelain'], cwd=destination):
        raise RuntimeError('Fresh source is unexpectedly dirty')
    return destination


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--cache', type=Path, default=Path.home() / '.cache/ho-ollama')
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--sdk', type=Path, required=True)
    parser.add_argument('--llama-port', type=Path, required=True, help='ho-llama.cpp checkout with harmonyos/ adapters')
    parser.add_argument('--ollama-objects', type=Path, default=ROOT, help='Git object source; working-tree changes are never copied')
    parser.add_argument('--runner-objects', type=Path, required=True, help='llama.cpp Git object source')
    parser.add_argument('--jobs', type=int, default=4)
    parser.add_argument('--prepare-only', action='store_true')
    parser.add_argument('--native-only', action='store_true')
    parser.add_argument('--arch', choices=['x86_64', 'arm64'], default='x86_64',
                        help='ARM64 is compile-only and requires --native-only; never deploys')
    parser.add_argument('--tls', action='store_true',
                        help='Candidate only: build upstream LibreSSL-backed HTTPS support')
    parser.add_argument('--router', action='store_true',
                        help='Candidate only: enable upstream subprocess/router support')
    parser.add_argument('--go', type=Path, help='Explicit host Go compiler for an isolated cache')
    options = parser.parse_args()
    try:
        target = native_target(options.arch, options.native_only)
    except ValueError as error:
        parser.error(str(error))
    cache, out = options.cache.resolve(), options.out.resolve()
    validate_native_output(options.arch, out)
    cache.mkdir(parents=True, exist_ok=True)
    build_lock = (cache / 'runtime-build.lock').open('a')
    fcntl.flock(build_lock, fcntl.LOCK_EX)
    out.mkdir(parents=True, exist_ok=False)
    sources = cache / 'sources'
    sources.mkdir(parents=True, exist_ok=True)
    ollama, runner = options.ollama_objects.resolve(), options.runner_objects.resolve()
    # The caches are object stores, not trusted prepared working trees. Builds
    # use a private clone so a previous experiment cannot silently become input.
    build_parent = cache / 'build'
    build_parent.mkdir(parents=True, exist_ok=True)
    private = Path(tempfile.mkdtemp(prefix='runtime-clean-', dir=build_parent))
    ollama = materialize_source(ollama, OLLAMA, private / 'ollama')
    runner = materialize_source(runner, RUNNER, private / 'runner')
    own_patch = ADAPTER / 'manifest-copy.patch'
    affinity = options.llama_port.resolve() / 'harmonyos/llama-affinity.patch'
    patch(ollama, own_patch)
    # These two hooks belong to upstream Ollama, not to our OHOS adaptation.
    for file in sorted((ollama / 'llama/compat').glob('*.patch')):
        patch(runner, file)
    patch(runner, affinity)
    if options.tls:
        patch(runner, options.llama_port.resolve()/'harmonyos/http-ca-store.patch')
        shutil.copy2(options.llama_port.resolve()/'harmonyos/http_ca_store.h', runner/'common/ohos-http-ca-store.h')
    (out / 'source-selection.json').write_text(json.dumps({
        'policy': 'pinned-git-objects-plus-owned-port-adapters', 'source_worktree_copied': False,
        'ollama': str(ollama), 'runner': str(runner),
        'ollama_commit': OLLAMA, 'runner_commit': RUNNER,
    }, indent=2) + '\n')
    if options.prepare_only:
        return
    build = private / 'runner-build'
    run('cmake', '-S', runner, '-B', build, '-G', ('Ninja' if shutil.which('ninja') else 'Unix Makefiles'),
        f'-DCMAKE_TOOLCHAIN_FILE={options.sdk}/build/cmake/ohos.toolchain.cmake',
        '-DOHOS_ARCH='+target['cmake_abi'], '-DOHOS_STL=c++_static', '-DCMAKE_BUILD_TYPE=Release',
        '-DBUILD_SHARED_LIBS=OFF', '-DGGML_OPENMP=OFF', '-DGGML_NATIVE=OFF',
        '-DGGML_AVX=OFF', '-DGGML_AVX2=OFF', '-DGGML_FMA=OFF', '-DGGML_F16C=OFF', '-DGGML_SSE42=OFF',
        '-DLLAMA_OPENSSL=OFF', '-DLLAMA_CURL=OFF',
        '-DLLAMA_BUILD_LIBRESSL='+('ON' if options.tls else 'OFF'),

        '-DLLAMA_SUBPROCESS='+('ON' if options.router else 'OFF'),
        '-DLLAMA_BUILD_TESTS=ON', '-DLLAMA_BUILD_EXAMPLES=ON', '-DLLAMA_BUILD_TOOLS=ON',
        '-DLLAMA_BUILD_SERVER=ON', '-DLLAMA_BUILD_APP=OFF', '-DLLAMA_BUILD_UI=OFF',
        '-DLLAMA_BUILD_HTML=OFF',
        f'-DPICO_OLLAMA_SOURCE={ollama}',
        f'-DCMAKE_PROJECT_INCLUDE={ADAPTER}/tools/runner_compat.cmake')
    if options.tls:
        archive = build / '_deps/libressl-subbuild/libressl-populate-prefix/src/libressl-4.3.2.tar.gz'
        if hashlib.sha256(archive.read_bytes()).hexdigest() != LIBRESSL_ARCHIVE_SHA256:
            raise RuntimeError('LibreSSL archive differs from the official SHA256; refusing compilation')
    run('cmake', '--build', build, '-j', options.jobs, '--target',
        'llama-server', 'llama-cli', 'llama-bench', 'llama-quantize', 'llama-simple')
    payload = out / 'payload'
    (payload / 'bin').mkdir(parents=True, exist_ok=True)
    (payload / 'lib/ollama').mkdir(parents=True, exist_ok=True)
    shutil.copy2(options.sdk / ('llvm/lib/'+target['triple']+'/libc++_shared.so'), payload / 'lib/libc++_shared.so')
    for name in ('llama-cli', 'llama-bench', 'llama-quantize', 'llama-simple'):
        shutil.copy2(build / 'bin' / name, payload / 'bin' / name)
    shutil.copy2(build / 'bin/llama-server', payload / 'lib/ollama/llama-server')
    sdk = out / 'sdk'
    (sdk / 'include').mkdir(parents=True, exist_ok=True)
    (sdk / 'lib').mkdir(parents=True, exist_ok=True)
    for source in [runner / 'include/llama.h', *(runner / 'ggml/include').glob('*.h')]:
        shutil.copy2(source, sdk / 'include' / source.name)
    for source in [build / 'src/libllama.a', *(build / 'ggml/src').glob('libggml*.a'),
                   build / 'vendor/hash/libvendor-hash.a']:
        shutil.copy2(source, sdk / 'lib' / source.name)
    if not options.native_only:
        env = dict(os.environ, PICO_GO=str(options.go or cache / 'toolchains/go1.26/bin/go'), HARMONY_NATIVE_SDK=str(options.sdk))
        run('python3', ADAPTER / 'tools/prepare_official_app.py', '--source', ollama, '--evidence', out / 'app-preparation')
        run('bash', ADAPTER / 'tools/build_cli.sh',
            ollama, payload / 'bin/ollama', env=env)
    for source, name in ((ollama / 'LICENSE', 'OLLAMA_LICENSE'), (runner / 'LICENSE', 'LLAMA_CPP_LICENSE')):
        shutil.copy2(source, payload / name)
    for source in (runner / 'vendor').rglob('LICENSE*'):
        if source.is_file():
            destination = payload / 'licenses' / source.relative_to(runner)
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(source, destination)
    if options.tls:
        notices = payload / 'licenses/LibreSSL'
        notices.mkdir(parents=True, exist_ok=True)
        shutil.copy2(build / '_deps/libressl-src/COPYING', notices / 'COPYING')
    identity = {'ollama_commit': OLLAMA, 'runner_commit': RUNNER,
                'arch': target['arch'], 'go_runtime': None if options.native_only else 'linux/amd64 + OHOS cgo, harmonyos,netcgo',
                'runtime_qualified': False, 'product_qualified': False,
                'platform_patches': {p.name: hashlib.sha256(p.read_bytes()).hexdigest()
                                     for p in (own_patch, affinity)},
                'sdk_files': {str(p.relative_to(sdk)): hashlib.sha256(p.read_bytes()).hexdigest()
                              for p in sdk.rglob('*') if p.is_file()},
                'features_requested': {'tls_libressl': options.tls, 'subprocess_router': options.router,

                                       'ohos_ca_store':options.tls},
                'tls_dependency_archive_sha256': LIBRESSL_ARCHIVE_SHA256 if options.tls else None,
                'limitations': ['ARM64 separate qualification'] +
                    ([] if options.tls else ['TLS off']) +
                    ([] if options.router else ['subprocess router off']),
                'files': {str(p.relative_to(payload)): hashlib.sha256(p.read_bytes()).hexdigest()
                          for p in payload.rglob('*') if p.is_file()}}
    (out / 'runtime-identity.json').write_text(json.dumps(identity, indent=2) + '\n')
    for name, source in (('ollama', ollama), ('runner', runner)):
        (out / f'{name}-tracked-diff.patch').write_bytes(subprocess.check_output(['git', 'diff', '--binary'], cwd=source))


if __name__ == '__main__':
    main()
