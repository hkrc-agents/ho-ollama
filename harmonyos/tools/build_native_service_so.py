#!/usr/bin/env python3
"""Build real opt-in managed Go SOs in a fresh private tree; never deploy.

Default upstream builds and the installed product are not edited. Native mode
uses official HAP creation for runners and disables GPU subprocess discovery
because this candidate is explicitly CPU-only.
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
import time

ADAPTER = Path(__file__).resolve().parents[1]
NATIVE = ADAPTER / 'native-services'
PIN = '8a971df3bacec93944ec894f2939fd2567afc5dd'


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def manifest(root):
    result = {}
    for directory, dirs, names in os.walk(root, followlinks=False):
        dirs[:] = sorted(v for v in dirs if v not in ('.git', 'node_modules', '__pycache__'))
        for name in sorted(names):
            path = Path(directory) / name
            if path.is_symlink():
                raise RuntimeError('non-self-contained source: ' + str(path))
            if path.is_file():
                result[str(path.relative_to(root))] = sha(path)
    return result


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--base', type=Path, required=True, help='Explicit pinned prepared Ollama source')
    p.add_argument('--out', type=Path, required=True, help='Fresh evidence directory')
    p.add_argument('--go', type=Path, required=True, help='Explicit R3 GD-TLS compiler')
    p.add_argument('--sdk', type=Path, required=True)
    p.add_argument('--component', choices=['core','app'], default='core')
    p.add_argument('--cache',type=Path,help='Explicit private cache to reuse across SO component builds')
    p.add_argument('--diagnostics',action='store_true',help='Include private startup worker checks; never a release candidate')
    p.add_argument('--experimental-so', action='store_true',
                   help='Explicitly opt into the archived, non-portable SO experiment (never production/export)')
    a = p.parse_args()
    if not a.experimental_so:
        p.error('SO service route is archived; --experimental-so is required. Use the ELF product builder.')
    base, out, go, sdk = (v.resolve() for v in (a.base, a.out, a.go, a.sdk))
    if out.is_relative_to(base):
        p.error('output must be outside source')
    out.mkdir(parents=True, exist_ok=False)
    identity = dict(status='preparing', component=a.component, product_qualified=False,
                    release_qualified=False, archived_experiment=True, public_export_allowed=False,
                    commands=[], started=time.time())
    (out/'builder.py').write_bytes(Path(__file__).read_bytes())
    def run(argv, name, cwd=None, env=None):
        argv=list(map(str,argv));identity['commands'].append(dict(argv=argv,cwd=str(cwd or out),log=name))
        with (out/name).open('wb') as log:
            subprocess.run(argv,cwd=cwd or out,env=env,stdout=log,stderr=subprocess.STDOUT,check=True)
        return (out/name).read_text(errors='replace')
    lockpath=Path.home()/'.cache/ho-ollama/runtime-build.lock';lockpath.parent.mkdir(parents=True,exist_ok=True)
    try:
        with lockpath.open('a') as lock:
            fcntl.flock(lock,fcntl.LOCK_EX)
            if subprocess.check_output(['git','rev-parse','HEAD'],cwd=base,text=True).strip()!=PIN:
                raise RuntimeError('unpinned prepared base')
            before=manifest(base)
            parent=Path.home()/'.cache/ho-ollama/build';parent.mkdir(parents=True,exist_ok=True)
            if subprocess.check_output(['stat','-f','-c','%T',parent],text=True).strip() not in ('ext2/ext3','ext4'):
                raise RuntimeError('private build parent must be ext4')
            private=Path(tempfile.mkdtemp(prefix='managed-service-so-',dir=parent))
            source=private/'source'
            shutil.copytree(base,source,ignore=shutil.ignore_patterns('.git','node_modules','__pycache__'))
            if before!=manifest(source) or before!=manifest(base):raise RuntimeError('base changed while copying')
            canonical=manifest(NATIVE)
            # New package has the upstream module path; no nested go.mod or
            # ad-hoc module replacement is added to the official dependency graph.
            target=source/'harmonyos/nativeprocess';target.mkdir(parents=True)
            for path in sorted((NATIVE/'go').glob('*.go')):shutil.copy2(path,target/path.name)
            shutil.copy2(NATIVE/'runner/runner_harmony.go',source/'llm/runner_managed_harmony.go')
            patch=NATIVE/'runner/managed-runner.patch'
            run(['git','apply','--check','--recount','--unidiff-zero',patch],'runner-patch-check.log',source)
            run(['git','apply','--recount','--unidiff-zero',patch],'runner-patch.log',source)
            entry=source/'cmd/nativecore';entry.mkdir()
            shutil.copy2(NATIVE/'core/entry.go',entry/'main.go')
            if a.component=='app':
                destinations={
                    'managed_store_paths.go':'app/store/managed_paths_harmony.go',
                    'managed_server_paths.go':'app/server/managed_paths_harmony.go'}
                for name in ['main.go','runtime_layout_harmony.go','runtime_layout_exec_harmony.go','runtime_managed_harmony.go','runtime_diagnostics_disabled.go','runtime_diagnostics_enabled.go',
                             'core_control_harmony.go','core_managed_harmony.go','services_harmony.go','server_managed_harmony.go',
                             'model_store_worker_harmony.go','worker_managed_harmony.go','storage_probe_harmony.go','storage_probe_managed_harmony.go',
                             'managed_store_paths.go','managed_server_paths.go']:
                    destination=source/destinations.get(name,'app/cmd/harmony/'+name)
                    shutil.copy2(ADAPTER/'app'/name,destination)
            discovery=source/'discover/llama_server.go'
            original=discovery.read_text()
            marker='\tstatus := llm.NewStatusWriter(llamaServerDiscoveryOutput(ctx))\n'
            if original.count(marker)!=1:raise RuntimeError('CPU discovery hook does not match pinned source')
            discovery.write_text(original.replace(marker,marker+
                '\t// Managed HarmonyOS candidate is CPU-only; do not exec GPU probes.\n'
                '\tif true { return nil, status, nil }\n',1))
            # Native mode must have no residual direct process fields in the
            # actual model runner after the explicit reviewable patch.
            runner=(source/'llm/llama_server.go').read_text()
            if 's.cmd.Process' in runner or 'cmd.Start()' in runner:raise RuntimeError('legacy runner process access remains')
            if before!=manifest(base) or canonical!=manifest(NATIVE):raise RuntimeError('build inputs changed')
            identity.update(base=str(base),private_source=str(source),ollama_commit=PIN,
                            native_source_files=canonical,base_manifest=before)
        inputs=manifest(source);(out/'source-files.json').write_text(json.dumps(inputs,indent=2)+'\n')
        identity['source_manifest_sha256']=sha(out/'source-files.json')
        goroot=go.parents[1]
        tools={str(path.relative_to(goroot)):sha(path) for path in sorted((goroot/'pkg/tool/linux_amd64').iterdir()) if path.is_file()}
        tools.update({str(path.relative_to(goroot)):sha(path) for path in sorted((goroot/'src/runtime').rglob('*')) if path.is_file()})
        tools['bin/go']=sha(go);(out/'go-toolchain-files.json').write_text(json.dumps(tools,indent=2)+'\n')
        identity['go_toolchain_manifest_sha256']=sha(out/'go-toolchain-files.json')
        cache=(a.cache.resolve() if a.cache else private/'go-cache')
        cache.mkdir(parents=True,exist_ok=True)
        env=dict(os.environ,GOOS='linux',GOARCH='amd64',CGO_ENABLED='1',GOTOOLCHAIN='local',GOWORK='off',GOFLAGS='',GOMAXPROCS='4',
                 GOCACHE=str(cache),CC=f'{sdk}/llvm/bin/clang --target=x86_64-linux-ohos --sysroot={sdk}/sysroot',
                 CXX=f'{sdk}/llvm/bin/clang++ --target=x86_64-linux-ohos --sysroot={sdk}/sysroot')
        identity['go_version']=run([go,'version'],'go-version.log',env=env).strip()
        so=out/('libpico_ollama_'+a.component+'.so')
        identity['status']='building'
        tags='harmonyos,netcgo,pico_native_services,pico_ohos_dynamic_tls'
        if a.diagnostics:tags+=',pico_native_diagnostic'
        identity['diagnostics']=a.diagnostics
        run([go,'build','-mod=readonly','-p=4','-buildmode=c-shared',
             '-tags='+tags,
             '-asmflags=all=-tls=GD','-gcflags=all=-tls=GD','-o',so,
             './cmd/nativecore' if a.component=='core' else './app/cmd/harmony'],
            'go-'+a.component+'.log',source,env)
        elf=run([sdk/'llvm/bin/llvm-readelf','-h','-d','--dyn-syms','--wide',so],'elf.log')
        if ' PicoServiceRun' not in elf or 'TEXTREL' in elf:raise RuntimeError('invalid managed Go SO exports/relocations')
        if inputs!=manifest(source) or canonical!=manifest(NATIVE):raise RuntimeError('sources changed during compilation')
        if any(sha(goroot/name)!=digest for name,digest in tools.items()):raise RuntimeError('Go toolchain changed')
        identity.update(status='built',so_sha256=sha(so),so_size=so.stat().st_size)
    except Exception as error:
        identity.update(status='failed',error=str(error));raise
    finally:
        identity['elapsed_seconds']=time.time()-identity['started']
        (out/'identity.json').write_text(json.dumps(identity,indent=2)+'\n')


if __name__=='__main__':main()
