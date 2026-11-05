"""Combine a frozen SDK HAP build with the exact pinned production runtime HNP."""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess
import zipfile
import io
import shutil
import stat
from pathlib import PurePosixPath

SDK = Path('sdk/default/openharmony')
ADAPTER = Path(__file__).resolve().parents[1]


def extract_payload(archive, destination):
    """Validate every path/type before extracting into a new owned directory."""
    destination = Path(destination)
    if destination.exists():
        raise ValueError('Archive destination must be a new directory')
    seen = set()
    for entry in archive.infolist():
        raw = entry.filename
        name = PurePosixPath(raw)
        mode = entry.external_attr >> 16
        if ('\0' in entry.orig_filename or '\\' in raw or name.is_absolute()
                or not raw.rstrip('/') or any(p in ('', '.', '..') for p in raw.rstrip('/').split('/'))):
            raise ValueError('Unsafe archive path')
        if str(name) in seen:
            raise ValueError('Duplicate archive path')
        seen.add(str(name))
        if stat.S_IFMT(mode) not in (0, stat.S_IFREG, stat.S_IFDIR) or mode & (stat.S_ISUID | stat.S_ISGID | stat.S_ISVTX):
            raise ValueError('Unsupported archive type or privileged mode')
        if not (destination / raw).resolve().is_relative_to(destination.resolve()):
            raise ValueError('Archive path escapes destination')
    archive.extractall(destination)
    for entry in archive.infolist():
        mode = entry.external_attr >> 16
        if mode:
            (destination / entry.filename).chmod(stat.S_IMODE(mode))


def check_shutdown_contract(native, identity):
    """Do not pair the new native exit predicate with an old Go APP."""
    required = any(name.endswith('/libollamabridge.so') and b'appShutdownStatus' in data
                   for name, data in native.items())
    if not required:
        return
    if not identity:
        raise ValueError('Shutdown ACK HAP requires --runtime with a verified matching Go APP')
    adapters = identity.get('source', {}).get('adapters', {})
    for name in ['main.go', 'shutdown_harmony.go', 'services_harmony.go']:
        expected = hashlib.sha256((ADAPTER/'app'/name).read_bytes()).hexdigest()
        if adapters.get(name) != expected:
            raise ValueError('Shutdown ACK runtime source mismatch: '+name)
    if any(name.endswith('/libollamabridge.so') and b'PICO_PARENT_LIFELINE_FD=' in data for name,data in native.items()):
        for name in ['parent_lifeline_harmony.go','wait_group_harmony.go','wait_executable_harmony.go','desired_elf_harmony.go']:
            if adapters.get(name)!=hashlib.sha256((ADAPTER/'app'/name).read_bytes()).hexdigest():
                raise ValueError('ELF supervision runtime source mismatch: '+name)
        if identity.get('source',{}).get('core_wait_patch_sha256')!=hashlib.sha256((ADAPTER/'core-wait.patch').read_bytes()).hexdigest():
            raise ValueError('ELF core restart drain hook missing/mismatched')
        if identity.get('source',{}).get('frontend_contract') is not True or not identity['source'].get('frontend_dist'):
            raise ValueError('ELF runtime requires rebuilt and qualified embedded frontend')


def check_embedded_frontend(data):
    markers=[b'Choose custom storage folder',b'Use default private folder',b'Storage mode:']
    if not all(marker in data for marker in markers) or b'Choose migration parent folder' in data:
        raise ValueError('stale or missing embedded frontend bytes')


def check_elf_build_mode(native, record):
    bridges=[data for name,data in native.items() if name.endswith('/libollamabridge.so') and b'PICO_PARENT_LIFELINE_FD=' in data]
    if not bridges:return
    mode=json.loads(native['module.json'])['app']['buildMode']
    if mode!=record.get('mode') or mode not in ['debug','release']:
        raise ValueError('ELF SDK/manifest build mode mismatch')
    disabled=b'diagnostic commands disabled in release' in bridges[0]
    debug=b'debug APP crash requested' in bridges[0]
    if (disabled,debug)!=((False,True) if mode=='debug' else (True,False)):
        raise ValueError('ELF actual Native build mode mismatch; stale SDK object')


def check_elf_native_identity(native, record):
    if any(n.endswith('/libollamabridge.so') and b'PICO_PARENT_LIFELINE_FD=' in data for n,data in native.items()):
        parts=['bridge.cpp','elf_supervision.h','storage_permission_audit.cpp','storage_permission_audit.h','CMakeLists.txt']
        expected=hashlib.sha256(''.join(record['inputs']['entry/src/main/cpp/'+part] for part in parts).encode('ascii')).hexdigest().encode('ascii')
        if not all(expected in data for n,data in native.items() if n.endswith('/libollamabridge.so')):
            raise ValueError('ELF native library does not embed the SDK-recorded bridge source identity')

def check_native_runtime_manifest(identity, runtime, manifest_path):
    """Explicit, narrow native CPU update; default APP-only gate stays closed."""
    if manifest_path is None:return set()
    reference=identity.get('native_identity',{})
    relative=Path(reference.get('path',''))
    if relative.is_absolute() or '..' in relative.parts or not reference.get('path'):
        raise ValueError('Native manifest reference must be runtime-relative')
    if not manifest_path.resolve().is_relative_to(runtime.resolve()) or manifest_path.is_symlink():
        raise ValueError('Native manifest must be inside the explicit runtime artifact')
    if ((runtime/relative).resolve()!=manifest_path.resolve() or
        reference.get('sha256')!=hashlib.sha256(manifest_path.read_bytes()).hexdigest()):
        raise ValueError('Native runtime manifest binding mismatch')
    record=json.loads(manifest_path.read_text())
    if record.get('status')!='built' or record.get('arch')!='x86_64':
        raise ValueError('Complete x86_64 native runtime build required')
    for key in ['ca_store_only','native_cli_tools','upstream_tls_enabled','router_enabled']:
        if record.get(key) is not True:raise ValueError('Native runtime feature missing: '+key)
    if record.get('http_diagnostics') is not False:
        raise ValueError('Diagnostic runtime cannot be packaged as product')
    if record.get('tls_system_roots_requested') is not False:
        raise ValueError('Experimental NetStack runtime cannot be packaged as product')
    if record.get('ca_store_key')!='LLAMA_OHOS_CA_DIRECTORY':
        raise ValueError('Native CA provider ABI mismatch')
    lock=json.loads((ADAPTER/'upstream-lock.json').read_text())
    if record.get('runner_commit')!=lock['runner']['commit'] or record.get('ollama_commit')!=lock['ollama']['commit']:
        raise ValueError('Native runtime source pin mismatch')
    # The explicit runtime artifact carries its own source closure. Do not
    # assume a sibling checkout or reach outside a standalone Ollama fork.
    adapter=runtime/'native-tls-source'
    for field,name in [('adapter_sha256','http_ca_store.h'),('patch_sha256','http-ca-store.patch')]:
        if (adapter/name).is_symlink() or not (adapter/name).resolve().is_relative_to(runtime.resolve()):
            raise ValueError('Native source closure must be inside the runtime artifact')
        if record.get(field)!=hashlib.sha256((adapter/name).read_bytes()).hexdigest():
            raise ValueError('Native CA adapter source mismatch')
    if record.get('libressl_archive_sha256')!='edf01aee24c65d69e6a9efcb9d44bcda682ff9d4f3bbbd95e794e1dfa90847b5':
        raise ValueError('Unpinned TLS dependency')
    allowed={'lib/ollama/llama-server','bin/llama-cli','bin/llama-bench','bin/llama-simple','bin/llama-quantize',
             'licenses/LibreSSL/COPYING'}
    for name in allowed:
        path=runtime/'payload'/name
        digest=hashlib.sha256(path.read_bytes()).hexdigest()
        if identity['files'].get(name)!=digest or record.get('files',{}).get(name)!=digest:
            raise ValueError('Native runtime byte mismatch: '+name)
        if name!='licenses/LibreSSL/COPYING':
            with path.open('rb') as binary:header=binary.read(20)
            if header[:6]!=b'\x7fELF\x02\x01' or int.from_bytes(header[18:20],'little')!=62:
                raise ValueError('Native executable ABI mismatch: '+name)
    return allowed

def check_tls_native_provider(native,record):
    bridges=[data for name,data in native.items() if name.endswith('/libollamabridge.so')]
    if not bridges or not all(b'LLAMA_OHOS_CA_DIRECTORY=' in data for data in bridges):
        raise ValueError('Native HAP lacks the versioned CA provider')
    relative='entry/src/main/ets/services/AppRuntime.ets'
    expected=hashlib.sha256((ADAPTER/'hap'/relative).read_bytes()).hexdigest()
    if record.get('inputs',{}).get(relative)!=expected:
        raise ValueError('HAP CA bootstrap source is missing or stale')


def main():
    global SDK
    p = argparse.ArgumentParser(description=__doc__)
    for key in ['base', 'native', 'build-record', 'out']:
        p.add_argument('--'+key, type=Path, required=True)
    p.add_argument('--runtime', type=Path, help='Verified official APP rebuild; required by shutdown-ACK HAPs; only ollama-app may change')
    p.add_argument('--native-runtime-manifest',type=Path,help='Explicit pinned minimal native CPU build; never permits arbitrary runtime changes')
    p.add_argument('--sdk', type=Path, default=SDK)
    p.add_argument('--java', type=Path, default=Path.home()/'.cache/ho-ollama/toolchains/jdk17/bin/java')
    a = p.parse_args()
    if a.native_runtime_manifest and not a.runtime:p.error('--native-runtime-manifest requires --runtime')
    SDK = a.sdk.resolve()
    digest = lambda data: hashlib.sha256(data).hexdigest()
    record = json.loads(a.build_record.read_text())
    assert record['build_exit'] == 0 and record['inputs_unchanged']
    assert digest(a.native.read_bytes()) == record['unsigned_hap_sha256']
    a.out.mkdir(parents=True, exist_ok=False)
    stage = a.out/'stage'
    with zipfile.ZipFile(a.native) as z:
        native = {n: z.read(n) for n in z.namelist() if not n.endswith('/')}
        extract_payload(z, stage)
    check_elf_native_identity(native,record)
    check_elf_build_mode(native,record)
    with zipfile.ZipFile(a.base) as z:
        runtime = {n: z.read(n) for n in z.namelist() if n.endswith('.hnp')}
    assert list(runtime) == ['hnp/x86_64/ollama.hnp']
    changed_runtime_files = []
    runtime_identity = json.loads((a.runtime/'official-app-identity.json').read_text()) if a.runtime else None
    check_shutdown_contract(native, runtime_identity)
    if any(name.endswith('/libollamabridge.so') and b'PICO_PARENT_LIFELINE_FD=' in data for name,data in native.items()):
        check_embedded_frontend((a.runtime/'payload/bin/ollama-app').read_bytes())
    if a.runtime:
        identity = runtime_identity
        native_allowed=check_native_runtime_manifest(identity,a.runtime,a.native_runtime_manifest)
        if native_allowed:check_tls_native_provider(native,record)
        payload = a.out/'runtime-payload'
        with zipfile.ZipFile(io.BytesIO(runtime['hnp/x86_64/ollama.hnp'])) as z:
            extract_payload(z, payload)
        configs = list(payload.rglob('hnp.json'))
        assert len(configs) == 1
        root = configs[0].parent
        for name, expected in identity['files'].items():
            existed=name in {str(f.relative_to(root)) for f in root.rglob('*') if f.is_file()}
            assert existed or (name=='licenses/LibreSSL/COPYING' and name in native_allowed), name
            source = a.runtime/'payload'/name
            assert source.resolve().is_relative_to((a.runtime/'payload').resolve())
            assert digest(source.read_bytes()) == expected, name
            if not existed or digest((root/name).read_bytes()) != expected:
                assert name == 'bin/ollama-app' or name in native_allowed, 'Unexpected runtime change: '+name
                changed_runtime_files.append(name)
                (root/name).parent.mkdir(parents=True,exist_ok=True)
                shutil.copy2(source, root/name)
        packed = a.out/'runtime-packed'
        packed.mkdir()
        subprocess.run([str(SDK/'toolchains/hnpcli'), 'pack', '-i', str(root.resolve()), '-o', str(packed.resolve())], check=True)
        hnps = list(packed.glob('*.hnp'))
        assert len(hnps) == 1
        runtime['hnp/x86_64/ollama.hnp'] = hnps[0].read_bytes()
        with zipfile.ZipFile(io.BytesIO(runtime['hnp/x86_64/ollama.hnp'])) as z:
            config_name = next(n for n in z.namelist() if n.endswith('/hnp.json') or n == 'hnp.json')
            prefix = config_name[:-len('hnp.json')]
            for name, expected in identity['files'].items():
                assert digest(z.read(prefix+name)) == expected, 'Packed runtime mismatch: '+name
    for name, data in runtime.items():
        target = stage/name
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(data)
    module = json.loads(native['module.json'])
    assert module['app']['bundleName'] == 'com.pico.ollama'
    assert module['module']['hnpPackages'] == [{'package': 'ollama.hnp', 'type': 'public'}]
    command = [str(a.java.resolve()), '-jar',
               str(SDK/'toolchains/lib/app_packing_tool.jar'), '--mode', 'hap']
    for key, path in [('json', stage/'module.json'), ('ets', stage/'ets'), ('lib', stage/'libs'),
                      ('resources', stage/'resources'), ('index', stage/'resources.index'),
                      ('pack-info', stage/'pack.info'), ('hnp', stage/'hnp'), ('out', a.out/'ollama.hap')]:
        command += ['--'+key+'-path', str(path.resolve())]
    if 'pkgSdkInfo.json' in native:
        command += ['--pkg-sdk-info-path', str((stage/'pkgSdkInfo.json').resolve())]
    with (a.out/'pack.log').open('w') as log:
        subprocess.run(command+['--force', 'true'], stdout=log, stderr=subprocess.STDOUT, check=True)
    with zipfile.ZipFile(a.out/'ollama.hap') as z:
        after = {n: z.read(n) for n in z.namelist() if not n.endswith('/')}
    assert set(after) == set(native) | set(runtime)
    assert all(after[n] == data for n, data in native.items() if n not in runtime)
    assert all(after[n] == data for n, data in runtime.items())
    identity = {'hap_sha256': digest((a.out/'ollama.hap').read_bytes()),
                'base_hap_sha256': digest(a.base.read_bytes()),
                'runtime_hnps': {n: digest(data) for n, data in runtime.items()},
                'sdk_build_record': str(a.build_record.resolve()), 'runtime_unchanged': not changed_runtime_files,
                'changed_runtime_files': changed_runtime_files,
                'scope': 'SDK rebuild, optional verified official APP UI rebuild; device test required'}
    (a.out/'identity.json').write_text(json.dumps(identity, indent=2)+'\n')
    print(json.dumps(identity))


if __name__ == '__main__':
    main()
