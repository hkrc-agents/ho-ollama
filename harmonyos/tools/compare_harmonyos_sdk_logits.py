"""Full-vocabulary teacher-forced SDK comparison; byte identity is the gate.

Always reports cosine, both KL directions and absolute error, even on failure.
Neither top-1 agreement nor close cosine is silently substituted for identity.
"""
import argparse
import json
from pathlib import Path
import numpy as np

def metrics(x,y):
    if x.shape!=y.shape or x.size==0 or np.isnan(x).any() or np.isnan(y).any() or np.isposinf(x).any() or np.isposinf(y).any():raise ValueError('invalid logit vectors')
    mask=np.isfinite(x)
    if not np.array_equal(mask,np.isfinite(y)) or not mask.any():raise ValueError('different/empty finite support')
    a,b=x[mask].astype(np.float64),y[mask].astype(np.float64)
    def logs(v):
        v=v-v.max();return v-np.log(np.exp(v).sum())
    la,lb=logs(a),logs(b);norm=np.linalg.norm(a)*np.linalg.norm(b)
    return {'byte_equal':x.tobytes()==y.tobytes(),'different_elements':int(np.count_nonzero(x!=y)),
            'max_abs':float(np.max(np.abs(a-b))),'cosine':float(a@b/norm) if norm else float(np.array_equal(a,b)),
            'kl_linux_ohos':float(np.sum(np.exp(la)*(la-lb))),
            'kl_ohos_linux':float(np.sum(np.exp(lb)*(lb-la)))}

def compare(left,right):
    l=json.loads((left/'result.json').read_text());r=json.loads((right/'result.json').read_text())
    if not l['pass'] or not r['pass']:raise ValueError('functional SDK suite failed')
    rows=[]
    for name in ['threads_1','threads_2']:
        a,b=l[name],r[name]
        for key in ['prompt_tokens','forced_tokens','vocab','threads']:
            if a[key]!=b[key]:raise ValueError('different inputs: '+name+' '+key)
        if len(a['logits'])!=4 or len(b['logits'])!=4:raise ValueError('missing teacher-forced steps')
        for step,(lp,rp) in enumerate(zip(a['logits'],b['logits'])):
            if Path(lp).name!=lp or Path(rp).name!=rp:raise ValueError('unsafe trace filename')
            x=np.fromfile(left/lp,dtype='<f4');y=np.fromfile(right/rp,dtype='<f4')
            if x.size!=a['vocab'] or y.size!=b['vocab']:raise ValueError('incomplete vocabulary')
            rows.append({'case':name,'step':step,'vocab':a['vocab'],**metrics(x,y)})
    return {'rows':rows,'pass':all(row['byte_equal'] for row in rows),'gate':'full FP32 logits byte identity','token_only_comparison':False}

def main():
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('--linux',type=Path,required=True);p.add_argument('--ohos',type=Path,required=True);p.add_argument('--out',type=Path,required=True);a=p.parse_args()
    result=compare(a.linux,a.ohos);a.out.write_text(json.dumps(result,indent=2)+'\n');print(json.dumps({'pass':result['pass'],'vectors':len(result['rows'])}));raise SystemExit(0 if result['pass'] else 1)

if __name__=='__main__':main()
