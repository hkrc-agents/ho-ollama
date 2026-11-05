"""Build the independent HNP CLI test app with explicit SDK/tool inputs."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import zipfile
from hap_contract import extract_payload

ADAPTER = Path(__file__).resolve().parents[1]

def verify_elf(data, machine):
    if len(data) < 20 or data[:6] != b'\x7fELF\x02\x01' or int.from_bytes(data[18:20], 'little') != machine:
        raise ValueError('Wrong target ELF architecture')

def main():
    p = argparse.ArgumentParser(description=__doc__)
    for key in ['out', 'tools', 'java', 'sdk-root']:
        p.add_argument('--'+key, type=Path, required=True)
    p.add_argument('--arch', choices=['x86_64', 'arm64'], default='x86_64')
    a = p.parse_args()
    for key in ['out', 'tools', 'java', 'sdk_root']:
        setattr(a, key, getattr(a, key).resolve())
    a.out.mkdir(parents=True, exist_ok=False)
    app = a.out/'app'
    shutil.copytree(ADAPTER/'test-client', app, ignore=shutil.ignore_patterns('build', 'oh_modules', 'node_modules', '.hvigor', '.cxx', '__pycache__'))
    abi, triple, machine = {'x86_64':('x86_64','x86_64-linux-ohos23',62), 'arm64':('arm64-v8a','aarch64-linux-ohos23',183)}[a.arch]
    profile = app/'entry/build-profile.json5'
    d = json.loads(profile.read_text())
    d['buildOption']['externalNativeOptions']['abiFilters'] = [abi]
    profile.write_text(json.dumps(d, indent=2)+'\n')
    subprocess.run([sys.executable, str(ADAPTER/'tools/build_hap.py'), '--app', str(app), '--out', str(a.out/'hap-build'),
                    '--tools-dir', str(a.tools), '--java-home', str(a.java), '--sdk-root', str(a.sdk_root)], check=True)
    sdk = a.sdk_root/'default/openharmony'
    payload = a.out/'payload'
    (payload/'bin').mkdir(parents=True)
    for name, source in [('pico-client-guard',app/'guard/resource_guard.c'), ('pico-client-fixture',app/'entry/src/main/cpp/fixture.c')]:
        target = payload/'bin'/name
        subprocess.run([str(sdk/'native/llvm/bin/clang'), '--target='+triple, '--sysroot='+str(sdk/'native/sysroot'),
                        '-O2','-Wall','-Wextra','-Werror',str(source),'-fPIE','-pie','-o',str(target)], check=True, timeout=120)
        verify_elf(target.read_bytes()[:20], machine)
    (payload/'hnp.json').write_text(json.dumps({'type':'hnp-config','name':'pico_client_guard','version':'1.0',
        'install':{'links':[{'source':'bin/'+n,'target':n} for n in ['pico-client-guard','pico-client-fixture']]}},indent=2)+'\n')
    unsigned = app/'entry/build/default/outputs/default/entry-default-unsigned.hap'
    stage = a.out/'stage'
    with zipfile.ZipFile(unsigned) as z:
        extract_payload(z, stage)
        if 'libs/'+abi+'/libclienthnp.so' not in z.namelist():
            raise ValueError('Test HAP has no target native client library')
        for name in z.namelist():
            if name.endswith('.so'):
                verify_elf(z.read(name)[:20], machine)
    hnp_dir = stage/'hnp'/abi
    hnp_dir.mkdir(parents=True)
    subprocess.run([str(sdk/'toolchains/hnpcli'),'pack','-i',str(payload),'-o',str(hnp_dir)],check=True,timeout=120)
    module_path = stage/'module.json'
    d = json.loads(module_path.read_text())
    d['module']['hnpPackages'] = [{'package':'pico_client_guard.hnp','type':'public'}]
    module_path.write_text(json.dumps(d,indent=2)+'\n')
    command = [str(a.java/'bin/java'),'-jar',str(sdk/'toolchains/lib/app_packing_tool.jar'),'--mode','hap']
    for key, path in [('json',stage/'module.json'),('ets',stage/'ets'),('lib',stage/'libs'),('resources',stage/'resources'),
                      ('index',stage/'resources.index'),('pack-info',stage/'pack.info'),('hnp',stage/'hnp'),('out',a.out/'client.hap')]:
        command += ['--'+key+'-path',str(path)]
    if (stage/'pkgSdkInfo.json').exists():
        command += ['--pkg-sdk-info-path',str(stage/'pkgSdkInfo.json')]
    subprocess.run(command+['--force','true'],check=True,timeout=120)
    with zipfile.ZipFile(a.out/'client.hap') as z:
        if z.read('hnp/'+abi+'/pico_client_guard.hnp') != (hnp_dir/'pico_client_guard.hnp').read_bytes():
            raise ValueError('Packaged fixture HNP differs from the built package')
    (a.out/'identity.json').write_text(json.dumps({'arch':a.arch,'hap_sha256':hashlib.sha256((a.out/'client.hap').read_bytes()).hexdigest(),
        'signed':False,'device_tested':False},indent=2)+'\n')
    print(a.out/'client.hap')

if __name__ == '__main__':
    main()
