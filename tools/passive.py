import json,re,os,collections,sys
recs=[json.loads(l) for l in open('ghost-log.jsonl') if '"ver":"b617"' in l]
tot=collections.Counter(); deaths=0; flagged=0
for r in recs:
    if not r.get('trace') or not os.path.exists(r['trace']): continue
    obs=[json.loads(l) for l in open(r['trace']) if '"results"' not in l]
    modes={}
    for l in open(r['trace'].replace('.obs.jsonl','.log'),errors='ignore'):
        m=re.match(r"t=([\d.]+) mode=(\S+)",l)
        if m: modes[round(float(m[1]),1)]=m[2]
    lasthit={}
    passive_t=collections.Counter()
    for o in obs:
        for e in o.get('ev') or []:
            if e['type']=='damaged' and e.get('by') is not None: lasthit[e['by']]=o['t']
        md=modes.get(round(o['t'],1))
        for m in o['mechs']:
            h=lasthit.get(m[0])
            if h is not None and 4<=o['t']-h<15 and m[1]<120:
                tot[md]+=1; passive_t[m[0]]+=1
    if r['place']!=1:
        deaths+=1
        late=collections.Counter(e.get('by') for o in obs if o['t']>obs[-1]['t']-10 for e in o.get('ev') or [] if e['type']=='damaged')
        K=late.most_common(1)[0][0] if late else None
        if passive_t.get(K,0)>=20: flagged+=1
print('ticks where a mech that hit us 4-15 s ago is visible <120 m, by our mode:',dict(tot.most_common()))
print(f'deaths where the killer had >=2 s of such time: {flagged}/{deaths}')
