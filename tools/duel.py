import json, sys, os, collections
here = os.path.dirname(os.path.abspath(__file__))
ver = sys.argv[1]
recs = [json.loads(l) for l in open(os.path.join(here, "ghost-log.jsonl"), encoding="utf-8") if l.strip()]
recs = [r for r in recs if r.get("valid") and r.get("trace") and r.get("ver") == ver and r.get("place") != 1]
for r in recs:
    obs = [json.loads(l) for l in open(r["trace"], encoding="utf-8")]
    obs = [o for o in obs if "results" not in o]
    tend = obs[-1]["t"]
    late = [(o["t"], e) for o in obs for e in (o.get("ev") or []) if e["type"] == "damaged" and o["t"] > tend - 10]
    c = collections.Counter(e.get("by") for _, e in late)
    K = c.most_common(1)[0][0]
    took = sum(e["amount"] for o in obs for e in (o.get("ev") or []) if e["type"] == "damaged" and e.get("by") == K)
    first_hit_me = next((o["t"] for o in obs for e in (o.get("ev") or []) if e["type"] == "damaged" and e.get("by") == K), None)
    my_hits = [(o["t"], e) for o in obs for e in (o.get("ev") or []) if e["type"] == "hit" and e.get("target") == K]
    gave = sum(e["amount"] for _, e in my_hits)
    first_hit_it = my_hits[0][0] if my_hits else None
    seen = [(o["t"], m) for o in obs for m in o["mechs"] if m[0] == K]
    first_seen = seen[0][0] if seen else None
    last = seen[-1][1] if seen else None
    byw = collections.Counter()
    for _, e in my_hits: byw[e["weapon"]] += e["amount"]
    tw = collections.Counter()
    for o in obs:
        for e in o.get("ev") or []:
            if e["type"] == "damaged" and e.get("by") == K: tw[e["weapon"]] += e["amount"]
    dists = [m[1] for t, m in seen if t > tend - 10]
    others_hit = sum(e["amount"] for o in obs if o["t"] > tend - 30 for e in (o.get("ev") or []) if e["type"] == "damaged" and e.get("by") != K)
    print(f"place {r['place']:>2} t {tend:5.0f} K={K} saw@{first_seen} K-hit-me@{first_hit_me} I-hit-K@{first_hit_it} | I gave {gave:.0f} {dict(byw)} | took {took:.0f} {dict(tw)} | K hp at end {last[2] if last else '?'} K w={last[3] if last else '?'} | dist last10 {min(dists) if dists else '-'}-{max(dists) if dists else '-'} | other dmg last30 {others_hit:.0f} | me w={obs[-1]['w']} cubes={obs[-1]['cubes']}")
