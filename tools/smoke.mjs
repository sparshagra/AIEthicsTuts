// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Sparsh (Team 4141). Original work by Sparsh, see LICENSE and NOTICE.
//
// Offline smoke test: our brain alone on the real map with the real zone schedule and a crude walker
// (no enemies). Catches getting stuck, zone deaths, pickup failures and slow decisions.
//   node tools/smoke.mjs KIT_DIR [runs]

import { readFileSync } from "node:fs"
import { gunzipSync } from "node:zlib"
import { World } from "../src/world.mjs"
import { Brain } from "../src/brain.mjs"

const KIT = process.argv[2], RUNS = +(process.argv[3] || 10)
const lines = gunzipSync(readFileSync(KIT + "/check/stream.jsonl.gz")).toString().trim().split("\n").map((l) => JSON.parse(l))
const hello = lines[0], cfg = hello.config
const w = new World(KIT + "/arena", cfg.mech)
let seed = 12345
const rnd = () => ((seed = (seed * 1103515245 + 12345) >>> 0) / 4294967296)

function zoneAt(t, zc) {
  // phases: hold then shrink, from the start circle through each phase radius
  let c = cfg.zone.start_center, r = cfg.zone.start_radius, t0 = 0
  for (let k = 0; k < zc.length; k++) {
    const ph = cfg.zone.phases[k], nc = zc[k], ss = t0 + ph.hold_s, se = ss + ph.shrink_s
    if (t < se) return { center: c, radius: r, next_center: nc, next_radius: ph.radius, shrink_start: ss, shrink_end: se, dps: ph.dps }
    c = nc; r = ph.radius; t0 = se
  }
  return { center: c, radius: 0, next_center: c, next_radius: 0, shrink_start: t0, shrink_end: t0 + 1, dps: 300 }
}

const res = []
for (let run = 0; run < RUNS; run++) {
  // zone centres: each next circle inside the previous one
  const zc = []
  let c = cfg.zone.start_center, r = cfg.zone.start_radius
  for (const ph of cfg.zone.phases) {
    for (;;) {
      const a = rnd() * Math.PI * 2, d = rnd() * (r - ph.radius)
      const nc = [c[0] + Math.cos(a) * d, c[1] + Math.sin(a) * d]
      const nn = w.node(nc[0], nc[1])
      if (ph.radius > 30 || (w.navOk[nn] && w.comp[nn] === w.mainComp)) { zc.push(nc); c = nc; r = ph.radius; break }
    }
  }
  // spawn on the main walkable component inside 0.8 of the start circle
  let pos
  for (;;) {
    const a = rnd() * Math.PI * 2, d = Math.sqrt(rnd()) * cfg.zone.start_radius * 0.8
    const x = cfg.zone.start_center[0] + Math.cos(a) * d, y = cfg.zone.start_center[1] + Math.sin(a) * d
    const n = w.node(x, y)
    if (w.navOk[n] && w.comp[n] === w.mainComp) { pos = [x, y, w.surf[w.idx(x, y)]]; break }
  }
  const items = []
  for (let k = 0; k < 40; k++) {
    for (;;) {
      const x = w.bounds.minX + rnd() * (w.bounds.maxX - w.bounds.minX), y = w.bounds.minY + rnd() * (w.bounds.maxY - w.bounds.minY)
      const n = w.node(x, y)
      if (!w.navOk[n]) continue
      items.push({ id: 8000 + k, kind: ["laser", "lightning", "mortar"][k % 3], pos: [x, y, w.surf[w.idx(x, y)]] }); break
    }
  }
  const b = new Brain({ ...hello, slot: 3, seed: run + 1 }, w, { mode: "practice" })
  let hp = 1400, vel = [0, 0], stuck = 0, outside = 0, maxMs = 0, sumMs = 0, n = 0, weapons = [{ kind: "gun", cooldown: 0, ammo: 12, reload: 0, stats: cfg.weapons.gun }, null]
  let t = 0, tick = 0, died = -1, took = []
  for (; t < 420; t += 0.1, tick += 2) {
    const z = zoneAt(t, zc)
    const f = Math.min(1, Math.max(0, (t - z.shrink_start) / (z.shrink_end - z.shrink_start)))
    const zcx = z.center[0] + (z.next_center[0] - z.center[0]) * f, zcy = z.center[1] + (z.next_center[1] - z.center[1]) * f, zr = z.radius + (z.next_radius - z.radius) * f
    const inZone = Math.hypot(pos[0] - zcx, pos[1] - zcy) <= zr
    const vis = items.filter((it) => Math.hypot(it.pos[0] - pos[0], it.pos[1] - pos[1]) < 120)
    const o = {
      type: "obs", tick, t: +t.toFixed(1), alive: 16,
      self: { id: 3, pos: [...pos], vel: [vel[0], vel[1], 0], aim: [0, 0], hp, max_hp: 1400, energy: 100, max_energy: 100, grounded: true, cubes: 0, upgrades: [], in_zone: inZone, weapons, stats: cfg.mech },
      mechs: [], projectiles: [], crates: [], cubes: [], pickups: vis, sounds: [], beams: [], events: [], drops: [], zone: z, offer: null,
    }
    const t0 = performance.now()
    const a = b.act(o)
    const ms = performance.now() - t0
    maxMs = Math.max(maxMs, ms); sumMs += ms; n++
    if (a.take != null) {
      const k = items.findIndex((it) => it.id === a.take)
      if (k >= 0 && Math.hypot(items[k].pos[0] - pos[0], items[k].pos[1] - pos[1]) <= 4) { const it = items.splice(k, 1)[0]; took.push(`${it.kind}@${t | 0}`); const slot = a.drop ?? (weapons[1] ? -1 : 1); if (slot >= 0) weapons[slot] = { kind: it.kind, cooldown: 1.65, charge: 0, stats: cfg.weapons[it.kind] } }
    }
    // crude walker: accelerate toward the commanded velocity, slide off walls
    const L = Math.hypot(a.move[0], a.move[1]) || 0
    const sp = (a.boost ? cfg.mech.boost_speed : cfg.mech.walk_speed) * Math.min(1, L)
    const want = L > 0 ? [a.move[0] / L * sp, a.move[1] / L * sp] : [0, 0]
    vel = [vel[0] + (want[0] - vel[0]) * 0.6, vel[1] + (want[1] - vel[1]) * 0.6]
    const tryMove = (dx, dy) => { const nx = pos[0] + dx, ny = pos[1] + dy; if (w.walkable(pos[0], pos[1], nx, ny)) { pos = [nx, ny, w.surf[w.idx(nx, ny)]]; return true } return false }
    if (!tryMove(vel[0] * 0.1, vel[1] * 0.1) && !tryMove(vel[0] * 0.1, 0) && !tryMove(0, vel[1] * 0.1)) { if (L > 0.5) stuck += 0.1; vel = [0, 0] }
    if (!inZone) { outside += 0.1; hp -= z.dps * 0.1; if (process.env.V && t < 400 && run === +process.env.V && (tick % 10 === 0)) console.log(run, t.toFixed(1), b.label, "d", Math.hypot(pos[0]-zcx,pos[1]-zcy).toFixed(0), "zr", zr.toFixed(0), "next", z.next_radius, "pos", pos.map(v=>v|0).join(","), "stuck", stuck.toFixed(1), "nc", z.next_center.map(v=>v|0).join(","), "path", b.path && b.path.slice(0,4).map(p=>p.map(v=>v|0).join(",")).join(" "), b.path && b.path.partial, "goal", b.pathGoal && b.pathGoal.map(v=>v|0).join(",")) }
    else if (hp < 1400) hp = Math.min(1400, hp + 4.5)
    if (hp <= 0) { died = t; break }
  }
  res.push({ run, died: died >= 0 ? +died.toFixed(0) : "-", outside: +outside.toFixed(1), stuck: +stuck.toFixed(1), meanMs: +(sumMs / n).toFixed(2), maxMs: +maxMs.toFixed(1), took: took.join(" ") })
}
console.table(res)
