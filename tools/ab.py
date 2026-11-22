import json,statistics as s,sys
groups={}
for l in open('ghost-log.jsonl'):
    r=json.loads(l)
    if r.get('valid') and r.get('place'): groups.setdefault(r['ver'],[]).append(r['place'])
for v in sys.argv[1:]:
    vs=v.split('+'); p=sum((groups.get(x,[]) for x in vs),[])
    if not p: continue
    pts=[16-q for q in p]
    print(f"{v:10} n={len(p):3} mean={s.mean(pts):.2f} se={s.stdev(pts)/len(p)**.5:.2f} wins={p.count(1)/len(p):.0%} top3={sum(q<=3 for q in p)/len(p):.0%} bottom-half={sum(q>8 for q in p)/len(p):.0%}")
