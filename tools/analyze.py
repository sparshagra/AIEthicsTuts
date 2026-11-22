"""Summarise ghost-mode matches: place, death time, who and what killed us, mode history before death."""
import json, sys, re, collections, os

ver = sys.argv[1] if len(sys.argv) > 1 else None
here = os.path.dirname(os.path.abspath(__file__))
recs = [json.loads(l) for l in open(os.path.join(here, "ghost-log.jsonl"), encoding="utf-8") if l.strip()]
recs = [r for r in recs if r.get("valid") and r.get("trace") and (ver is None or r.get("ver") == ver)]
places, causes, modes_last, dmg_src = [], collections.Counter(), collections.Counter(), collections.Counter()
for r in recs:
    tr = r["trace"]
    if not os.path.exists(tr):
        continue
    obs, res = [], None
    for l in open(tr, encoding="utf-8"):
        j = json.loads(l)
        if "results" in j:
            res = j["results"]
        else:
            obs.append(j)
    modes = []
    log = tr.replace(".obs.jsonl", ".log")
    if os.path.exists(log):
        for l in open(log, encoding="utf-8", errors="ignore"):
            m = re.match(r"t=([\d.]+) mode=(\S+)", l)
            if m:
                modes.append((float(m.group(1)), m.group(2)))
    tend = obs[-1]["t"] if obs else 0
    taken = collections.Counter()
    late = []
    for o in obs:
        for e in o.get("ev") or []:
            if e["type"] == "damaged":
                taken[(e.get("weapon"), e.get("by"))] += e.get("amount", 0)
                if o["t"] > tend - 8:
                    late.append((o["t"], e.get("weapon"), e.get("by"), round(e.get("amount", 0))))
    for (w, _), a in taken.items():
        dmg_src[w] += a
    killer = collections.Counter()
    for _, w, by, a in late:
        killer[(w, by)] += a
    k = killer.most_common(1)[0][0] if killer else None
    lastm = collections.Counter(m for t, m in modes if t > tend - 10)
    for m, c in lastm.items():
        modes_last[m] += c
    allm = collections.Counter(m for _, m in modes)
    first_seen_mechs = next((o["t"] for o in obs if o["mechs"]), None)
    nkill = sum(1 for o in obs for e in o.get("ev") or [] if e["type"] == "kill")
    hp_series = [(round(o["t"]), o["hp"], o["alive"], len(o["mechs"])) for o in obs[:: max(1, len(obs) // 8)]]
    places.append(r["place"])
    if r["place"] != 1:
        causes[k[0] if k else "?"] += 1
    names = ""
    if res:
        ms = res.get("mechs") or res.get("standings") or []
        names = " ".join(f"{m.get('place')}:{m.get('name')}{'*' if m.get('human') else ''}" for m in sorted(ms, key=lambda m: m.get("place") or 99)[:6])
    print(f"room {r['room']:>3} place {r['place']:>2} t_end {tend:6.1f} kills {nkill} alive@end {obs[-1]['alive'] if obs else '?'} killer {k} w={obs[-1]['w'] if obs else ''}")
    print(f"    last10s modes {dict(lastm)}")
    print(f"    all modes {dict(allm.most_common(8))}")
    print(f"    late dmg {late[-6:]}")
    print(f"    hp/alive/visible {hp_series}")
    if names:
        print(f"    top6 {names}")
if places:
    print(f"\nN={len(places)} mean points {sum(16 - p for p in places) / len(places):.2f} places {sorted(places)}")
    print("death causes", dict(causes))
    print("damage taken by weapon", {k: round(v) for k, v in dmg_src.items()})
    print("modes in last 10 s before end", dict(modes_last.most_common(12)))
