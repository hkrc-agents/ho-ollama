"""Freeze/test the new Native registry/control library and tagged Go client.

No SDK build, package install, model access, or default upstream modification.
Host mocks do not grant actual HarmonyOS API or model service qualification.
"""
import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

ROOT=Path(__file__).resolve().parents[1]
SOURCE=ROOT/'native-services'


def digest(path):return hashlib.sha256(path.read_bytes()).hexdigest()


def manifest(root):
    return {str(path.relative_to(root)):digest(path) for path in sorted(root.rglob('*'))
            if path.is_file() and path.suffix in {'.h','.cpp','.go','.mod','.txt'}}


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--out',required=True,type=Path)
    parser.add_argument('--go',type=Path,required=True)
    parser.add_argument('--sdk',type=Path,required=True)
    args=parser.parse_args()
    out=args.out.resolve();out.mkdir(parents=True,exist_ok=False)
    (out/'verifier.py').write_bytes(Path(__file__).read_bytes())
    record=dict(pass_host=False,commands=[],product_qualified=False)
    cache=Path.home()/'.cache/ho-ollama/native-services-host';cache.mkdir(parents=True,exist_ok=True)
    lockpath=Path.home()/'.cache/ho-ollama/runtime-build.lock';lockpath.parent.mkdir(parents=True,exist_ok=True)
    with lockpath.open('a') as lock:
        fcntl.flock(lock,fcntl.LOCK_EX)
        before=manifest(SOURCE)
        private=Path(tempfile.mkdtemp(prefix='run-',dir=cache))
        source=private/'source';shutil.copytree(SOURCE,source)
        if before!=manifest(source) or before!=manifest(SOURCE):raise RuntimeError('source changed during snapshot')
    (out/'source-files.json').write_text(json.dumps(before,indent=2)+'\n')
    record.update(private_source=str(source),source_manifest_sha256=digest(out/'source-files.json'),go_sha256=digest(args.go))
    env=dict(os.environ,GOTOOLCHAIN='local',GOWORK='off',GOFLAGS='',CGO_ENABLED='1')
    def run(argv,log,cwd=None):
        argv=list(map(str,argv));record['commands'].append(dict(argv=argv,cwd=str(cwd or ROOT),log=log))
        with (out/log).open('wb') as stream:
            subprocess.run(argv,cwd=cwd or ROOT,env=env,stdout=stream,stderr=subprocess.STDOUT,check=True,timeout=180)
        return (out/log).read_text(errors='replace')
    try:
        record['go_version']=run([args.go,'version'],'go-version.log').strip()
        for flavor,flags in [('release',[]),('asan',['-DCMAKE_CXX_FLAGS=-fsanitize=address,undefined -fno-omit-frame-pointer'])]:
            build=private/flavor
            run(['cmake','-S',source,'-B',build,'-DCMAKE_BUILD_TYPE=Release','-DBUILD_TESTING=ON',*flags],flavor+'-configure.log')
            run(['cmake','--build',build,'-j4'],flavor+'-build.log')
            run(['ctest','--test-dir',build,'--output-on-failure'],flavor+'-ctest.log')
            cases=run([build/'pico_service_registry_tests'],flavor+'-cases.log')
            names=[line.removeprefix('PASS ') for line in cases.splitlines() if line.startswith('PASS ')]
            if not names or len(names)!=len(set(names)):raise RuntimeError('missing/duplicate Native host cases')
            record[flavor+'_cases']=names
            record[flavor+'_binary_sha256']=digest(build/'pico_service_registry_tests')
            dynamic=run(['readelf','-d',build/'pico_service_registry_tests'],flavor+'-dynamic.log')
            if 'libchild_process' in dynamic or 'libability' in dynamic:raise RuntimeError('OHOS API dependency leaked into host target')
        env['PICO_NATIVE_CONTROL_HOST_FIXTURE']=str(private/'release/pico_service_control_fixture')
        raw=run([args.go,'test','-count=1','-race','-json','-tags=harmonyos','./...'],'go-race.jsonl',cwd=source/'go')
        events=[json.loads(line) for line in raw.splitlines() if line.startswith('{')]
        failures=[x for x in events if x.get('Action') in ('fail','skip')]
        parents=[x['Test'] for x in events if x.get('Action')=='pass' and x.get('Test') and '/' not in x['Test']]
        children=[x['Test'] for x in events if x.get('Action')=='pass' and '/' in x.get('Test','')]
        if failures or not parents:raise RuntimeError('Go race failure/skip/zero tests')
        record['go_parent_tests']=parents;record['go_child_tests']=children
        default=json.loads(run([args.go,'list','-e','-json','.'],'default-linux-go.json',cwd=source/'go'))
        if default.get('GoFiles') or default.get('CgoFiles') or 'client.go' not in default.get('IgnoredGoFiles',[]):raise RuntimeError('OHOS Go implementation leaked into default Linux build')
        record['default_linux_ignores_ohos_client']=True
        # Compile the exact registry, private transport, and API backend against
        # the real OHOS headers too. Host protocol success is not target build.
        target=private/'ohos-objects';target.mkdir()
        for name in ['registry.cpp','control_server.cpp','ohos_platform.cpp','supervisor.cpp']:
            run([args.sdk/'llvm/bin/clang++','--target=x86_64-linux-ohos23','--sysroot='+str(args.sdk/'sysroot'),
                 '-std=c++17','-O2','-Wall','-Wextra','-Werror','-fPIC','-c',source/name,'-o',target/(name+'.o')],name+'-ohos-build.log')
        record['ohos_object_sha256']={p.name:digest(p) for p in target.iterdir()}
        record['ohos_compiler_sha256']=digest(args.sdk/'llvm/bin/clang++')
        if before!=manifest(source) or before!=manifest(SOURCE):raise RuntimeError('source changed while testing')
        record['pass_host']=True
    except Exception as error:
        record['error']=str(error);raise
    finally:
        (out/'result.json').write_text(json.dumps(record,indent=2)+'\n')


if __name__=='__main__':main()
