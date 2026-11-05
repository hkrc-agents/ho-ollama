"""Package a complete freshly built CPU payload, without a historical base HAP."""
import argparse
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import zipfile
from package_hap import extract_payload, check_shutdown_contract, check_elf_native_identity, check_elf_build_mode, check_embedded_frontend

def sha(data):
    return hashlib.sha256(data).hexdigest()

def verified_files(payload, expected):
    actual = {}
    for f in payload.rglob('*'):
        if f.is_symlink():
            raise ValueError('Payload symlinks are forbidden')
        if f.is_file():
            actual[str(f.relative_to(payload))] = sha(f.read_bytes())
    if actual != expected:
        raise ValueError('Payload differs from its identity')
    for name in ['bin/ollama', 'bin/ollama-app', 'bin/pico-service-launcher', 'bin/pico-storage-probe', 'lib/ollama/llama-server', 'bin/llama-cli', 'bin/llama-bench', 'bin/llama-quantize', 'bin/llama-simple']:
        if name not in actual:
            raise ValueError('Missing runtime component: ' + name)
    return actual

def main():
    p = argparse.ArgumentParser(description=__doc__)
    for key in ['native', 'build-record', 'runtime', 'sdk', 'java', 'out']:
        p.add_argument('--' + key, type=Path, required=True)
    a = p.parse_args()
    record = json.loads(a.build_record.read_text())
    identity = json.loads((a.runtime / 'official-app-identity.json').read_text())
    if identity.get('base_identity', {}).get('arch') != 'x86_64':
        raise ValueError('The full HAP route currently supports x86_64 only')
    if record.get('build_exit') != 0 or record.get('inputs_unchanged') is not True:
        raise ValueError('Successful unchanged-source HAP build required')
    if record['unsigned_hap_sha256'] != sha(a.native.read_bytes()):
        raise ValueError('HAP identity mismatch')
    files = verified_files(a.runtime / 'payload', identity['files'])
    a.out.mkdir(parents=True, exist_ok=False)
    stage = a.out / 'hap'
    with zipfile.ZipFile(a.native) as z:
        native = {n: z.read(n) for n in z.namelist() if not n.endswith('/')}
        extract_payload(z, stage)
    if any(n.endswith('.hnp') for n in native):
        raise ValueError('Expected a clean SDK HAP without a prepackaged runtime')
    module = json.loads(native['module.json'])
    if module['app']['bundleName'] != 'com.pico.ollama' or module['module']['hnpPackages'] != [{'package': 'ollama.hnp', 'type': 'public'}]:
        raise ValueError('Unexpected HAP runtime contract')
    check_shutdown_contract(native, identity)
    check_elf_native_identity(native, record)
    check_elf_build_mode(native, record)
    check_embedded_frontend((a.runtime / 'payload/bin/ollama-app').read_bytes())
    root = a.out / 'runtime'
    shutil.copytree(a.runtime / 'payload', root)
    (root / 'hnp.json').write_text(json.dumps({'type': 'hnp-config', 'name': 'ollama', 'version': '1.0',
        'install': {'links': [{'source': 'bin/' + n, 'target': n} for n in ['ollama', 'llama-cli', 'llama-bench', 'llama-quantize', 'llama-simple']]}}, indent=2) + '\n')
    packed = a.out / 'hnp-packed'
    packed.mkdir()
    subprocess.run([str(a.sdk / 'toolchains/hnpcli'), 'pack', '-i', str(root.resolve()), '-o', str(packed.resolve())], check=True, timeout=120)
    hnps = list(packed.glob('*.hnp'))
    if len(hnps) != 1:
        raise ValueError('Expected one HNP')
    with zipfile.ZipFile(hnps[0]) as z:
        configs = [n for n in z.namelist() if n == 'hnp.json' or n.endswith('/hnp.json')]
        if len(configs) != 1:
            raise ValueError('Unexpected HNP structure')
        prefix = configs[0][:-len('hnp.json')]
        for name, digest in files.items():
            if sha(z.read(prefix + name)) != digest:
                raise ValueError('Packed payload changed: ' + name)
    hnp_name = 'hnp/x86_64/ollama.hnp'
    target = stage / hnp_name
    target.parent.mkdir(parents=True, exist_ok=True)
    shutil.copy2(hnps[0], target)
    command = [str(a.java), '-jar', str(a.sdk / 'toolchains/lib/app_packing_tool.jar'), '--mode', 'hap']
    for key, path in [('json', stage/'module.json'), ('ets', stage/'ets'), ('lib', stage/'libs'), ('resources', stage/'resources'),
                      ('index', stage/'resources.index'), ('pack-info', stage/'pack.info'), ('hnp', stage/'hnp'), ('out', a.out/'ollama.hap')]:
        command += ['--' + key + '-path', str(path.resolve())]
    if (stage/'pkgSdkInfo.json').is_file():
        command += ['--pkg-sdk-info-path', str((stage/'pkgSdkInfo.json').resolve())]
    subprocess.run(command + ['--force', 'true'], check=True, timeout=120)
    with zipfile.ZipFile(a.out/'ollama.hap') as z:
        after = {n: z.read(n) for n in z.namelist() if not n.endswith('/')}
    if set(after) != set(native) | {hnp_name} or any(after[n] != data for n, data in native.items()) or after[hnp_name] != target.read_bytes():
        raise ValueError('Packaged HAP changed SDK content')
    (a.out/'identity.json').write_text(json.dumps({'hap_sha256': sha((a.out/'ollama.hap').read_bytes()), 'runtime_files': files,
        'signed': False, 'device_qualified': False, 'scope': 'Fresh CPU runtime packaging; no NPU integration'}, indent=2) + '\n')
    print(a.out/'ollama.hap')

if __name__ == '__main__':
    main()
