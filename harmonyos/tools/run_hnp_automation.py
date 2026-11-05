import os
"""HDC submits argv to independent Client HAP; HDC never executes the tested CLI."""
import argparse
import base64
import fcntl
import json
from pathlib import Path
import re
import shlex
import subprocess
import time
import uuid
from harmonyos_ui import HarmonyUI

HDC=os.environ.get('HDC', 'hdc')
BUNDLE='com.pico.hnpclient'
PUBLIC_RUNTIME_TOOLS={'ollama','llama-cli','llama-bench','llama-simple','llama-quantize','pico-client-fixture'}

def validate_case(c):
    if not isinstance(c,dict) or not isinstance(c.get('name'),str):raise ValueError('case name required')
    a=c.get('argv')
    if not isinstance(a,list) or not 1<=len(a)<=32 or any(not isinstance(v,str) or '\0' in v or len(v.encode())>8192 for v in a):raise ValueError('invalid argv')
    if not a[0] or ('/' in a[0] and not a[0].startswith('/')):raise ValueError('empty/relative executable')
    if not isinstance(c.get('stdin',''),str) or len(c.get('stdin','').encode())>65536:raise ValueError('stdin too large')
    if type(c.get('timeout_ms',30000)) is not int or not 100<=c.get('timeout_ms',30000)<=120000:raise ValueError('invalid timeout')
    if type(c.get('expect_exit',0)) is not int:raise ValueError('invalid expected exit')
    for key in ['stdout_contains','stderr_contains']:
        if not isinstance(c.get(key,[]),list) or any(not isinstance(v,str) for v in c.get(key,[])):raise ValueError('invalid output expectations')
    for key in ['stdout_base64_equals','stderr_base64_equals']:
        if key in c and not isinstance(c[key],str):raise ValueError('invalid raw output expectation')
    if 'cancel_after_ms' in c and (type(c['cancel_after_ms']) is not int or not 1<=c['cancel_after_ms']<c.get('timeout_ms',30000)):raise ValueError('invalid cancellation delay')

def check_result(c,r,owner_uid):
    failures=[]
    if r.get('schema')!=1 or r.get('complete') is not True:failures.append('incomplete/schema')
    if not isinstance(r.get('uid'),int) or r['uid'] in {0,2000,owner_uid}:failures.append('not independent app actor')
    # procfs may include the terminating NUL in this security-label read.
    # Preserve raw evidence, normalize only the textual terminator for comparison.
    if r.get('domain','').rstrip('\0\r\n')!='u:r:debug_hap:s0':failures.append('unexpected client domain')
    child_uid=re.search(r'^Uid:\s+(\d+)\s+(\d+)\s+(\d+)\s+(\d+)\s*$',r.get('child_status',''),re.M)
    if not child_uid or any(int(v)!=r.get('uid') for v in child_uid.groups()):failures.append('child UID missing/different')
    if r.get('child_domain','').rstrip('\0\r\n')!='u:r:debug_hap:s0':failures.append('child domain missing/different')
    if r.get('spawn_error')!=0 or r.get('waited_pid')!=r.get('child_pid') or r.get('child_pid',-1)<=1:failures.append('spawn/reap')
    if c['argv'][0] in PUBLIC_RUNTIME_TOOLS:
        package='pico_client_guard.org/' if c['argv'][0]=='pico-client-fixture' else 'ollama.org/'
        if r.get('entry')!='/data/service/hnp/bin/'+c['argv'][0] or not r.get('resolved_entry','').startswith('/data/service/hnp/'+package):failures.append('not installed public runtime')
    elif 'expect_entry' not in c or r.get('entry')!=c['expect_entry'] or not r.get('resolved_entry','').startswith('/'):
        failures.append('generic executable needs matching expect_entry')
    if r.get('output_or_io_limited') or not r.get('stdout_eof') or not r.get('stderr_eof'):failures.append('I/O incomplete/limited')
    if r.get('timed_out')!=c.get('expect_timeout',False):failures.append('timeout')
    if r.get('cancelled')!=c.get('expect_cancelled',False):failures.append('cancel')
    if not c.get('expect_cancelled') and not c.get('expect_timeout') and (r.get('exit_code')!=c.get('expect_exit',0) or r.get('signal')!=0):failures.append('exit/signal')
    outputs={}
    for key in ['stdout','stderr']:
        try:outputs[key]=base64.b64decode(r[key+'_base64'],validate=True).decode('utf-8',errors='replace')
        except (KeyError,ValueError,TypeError):failures.append(key+' invalid base64');outputs[key]=''
        for expected in c.get(key+'_contains',[]):
            if expected not in outputs[key]:failures.append(key+' missing '+repr(expected))
        if key+'_base64_equals' in c and r.get(key+'_base64')!=c[key+'_base64_equals']:failures.append(key+' raw bytes differ')
    if f"PICO_RESOURCE_GUARD pid={r.get('child_pid')} " not in outputs['stderr'] or 'RLIMIT_AS=disabled' not in outputs['stderr']:failures.append('resource guard missing')
    return failures,outputs

def main():
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('--cases',type=Path,required=True);p.add_argument('--out',type=Path,required=True)
    p.add_argument('--build',type=Path);p.add_argument('--target',required=True)
    p.add_argument('--hdc-server',help='Optional owned loopback HDC server');a=p.parse_args()
    spec=json.loads(a.cases.read_text());cases=spec['cases']
    if spec.get('schema')!=1 or not cases:raise SystemExit('Invalid/empty test manifest')
    for c in cases:validate_case(c)
    out=a.out.resolve();out.mkdir(parents=True,exist_ok=False)
    # Reuse the loopback-server validation; retain explicit target on every call.
    HarmonyUI(out,target=a.target,server=a.hdc_server)
    lock_path=Path.home()/'.cache/ho-ollama/emulator-client-test.lock'
    lock_path.parent.mkdir(parents=True,exist_ok=True)
    lock=lock_path.open('a');fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
    (out/'cases.json').write_text(json.dumps(spec,indent=2));counter=0
    def command(args,label,timeout=35):
        nonlocal counter
        route=['-s',a.hdc_server] if a.hdc_server else []
        r=subprocess.run([HDC,*route,'-t',a.target,*args],capture_output=True,text=True,timeout=timeout)
        text=r.stdout+r.stderr;counter+=1;(out/f'{counter:04d}-{label}.log').write_text(text)
        if r.returncode or '[Fail]' in text or text.strip()=='Connect server failed':
            raise RuntimeError('HDC command failed: '+label+': '+text)
        return r.returncode,text
    _,mem=command(['shell','cat /proc/meminfo'],'memory')
    if int(re.search(r'MemAvailable:\s+(\d+)',mem)[1])<1800*1024:raise SystemExit('Insufficient memory')
    _,disk=command(['shell','df -k /data'],'disk');rows=[r.split() for r in disk.splitlines() if r.startswith('/')]
    if not rows or int(rows[-1][3])<1024*1024:raise SystemExit('Insufficient disk')
    _,before=command(['shell','ps -A -o UID,PID,NAME'],'process-before')
    owner=re.search(r'^\s*(\d+)\s+\d+\s+com.pico.ollama\s*$',before,re.M)
    if not owner:raise SystemExit('Open normal Ollama HAP first; this client does not cold-start it')
    owner_uid=int(owner[1]);_,identity_before=command(['shell','-b','com.pico.ollama','cat','data/storage/el2/base/haps/entry/files/desktop/runtime-sha256.json'],'runtime-before')
    if a.build:
        rc,text=command(['install','-r',str((a.build/'client.hap').resolve())],'install-client',100)
        if rc or 'install bundle successfully' not in text:raise SystemExit('Client installation failed')
    command(['shell','aa force-stop '+BUNDLE],'stop-old-client')
    reports=[]
    try:
        for c in cases:
            run_id='auto_'+uuid.uuid4().hex
            parts=['aa','start','-b',BUNDLE,'-a','EntryAbility','--ps','runId',run_id,'--ps','argv',json.dumps(c['argv'],ensure_ascii=True),'--ps','stdin',c.get('stdin',''),'--pi','timeoutMs',str(c.get('timeout_ms',30000))]
            _,text=command(['shell',shlex.join(parts)],'submit-'+run_id)
            if 'start ability successfully' not in text:raise RuntimeError('Client dispatch failed')
            deadline=time.monotonic()+c.get('timeout_ms',30000)/1000+25;submitted=time.monotonic();cancel_sent=False;r=None
            while time.monotonic()<deadline:
                if 'cancel_after_ms' in c and not cancel_sent and (time.monotonic()-submitted)*1000>=c['cancel_after_ms']:
                    cancel_sent=True;command(['shell',shlex.join(['aa','start','-b',BUNDLE,'-a','EntryAbility','--ps','action','cancel','--ps','runId',run_id])],'cancel-'+run_id)
                _,raw=command(['shell','-b',BUNDLE,'cat','data/storage/el2/base/haps/entry/files/'+run_id+'.result.json'],'result-'+run_id)
                try:r=json.loads(raw)
                except json.JSONDecodeError:r=None
                if r and r.get('complete'):break
                time.sleep(.1)
            if not r:raise RuntimeError('No structured result for '+run_id)
            if r.get('run_id')!=run_id:raise RuntimeError('Result belongs to a different run')
            (out/(run_id+'.json')).write_text(json.dumps(r,indent=2));failures,outputs=check_result(c,r,owner_uid)
            for channel,text in outputs.items():(out/(run_id+'.'+channel+'.txt')).write_text(text)
            report={'name':c['name'],'run_id':run_id,'pass':not failures,'failures':failures,'exit':r['exit_code'],'uid':r['uid']};reports.append(report)
            print(json.dumps(report),flush=True)
    finally:
        # Outstanding jobs need their own bounded termination, not force-stop first.
        command(['shell','ps -A -o UID,PID,NAME'],'process-after')
        _,identity_after=command(['shell','-b','com.pico.ollama','cat','data/storage/el2/base/haps/entry/files/desktop/runtime-sha256.json'],'runtime-after')
        identity_ok=identity_before==identity_after and 'ollama' in identity_after
        summary={'cases':reports,'target':a.target,'hdc_server':a.hdc_server,
                 'complete':len(reports)==len(cases),'normal_runtime_records_unchanged':identity_ok,
                 'pass':len(reports)==len(cases) and all(c['pass'] for c in reports) and identity_ok}
        (out/'summary.json').write_text(json.dumps(summary,indent=2))
    raise SystemExit(0 if summary['pass'] else 1)
if __name__=='__main__':main()
