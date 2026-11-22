// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Sparsh (Team 4141). Original work, see LICENSE and NOTICE.
//
// Ghost-mode bridge: plays our bot in live public matches on play.joinbuilders.org (real engine, real
// players, bots in the empty seats). The site speaks the same hello/obs/action messages over a WebSocket.
//
//   node --experimental-websocket tools/ghost.mjs --arena PATH/TO/kit/arena [--games 3] [--cmd "python -u bot.py"] [--env K=V]
//
// One JSON line per finished match is appended to tools/ghost-log.jsonl.

import { spawn } from "node:child_process"
import { appendFileSync, mkdirSync } from "node:fs"
import { fileURLToPath } from "node:url"
import { dirname, join } from "node:path"
import { randomBytes } from "node:crypto"

const here = dirname(fileURLToPath(import.meta.url))
const args = process.argv.slice(2)
const opt = (k, d) => { const i = args.indexOf("--" + k); return i >= 0 ? args[i + 1] : d }
const ARENA = opt("arena", process.env.ARENA || "/arena")
const GAMES = +opt("games", 1)
const FULL = args.includes("--full")
// a fresh random name per run, so nothing in a public room points back at us
const WORDS = ["toaster", "skill_issue", "bonk", "lag_spike", "touchgrass", "ratio", "npc_energy", "goofy", "copium", "sus_robot", "yeet", "bruh"]
const NAME = opt("name", null) ?? `${WORDS[randomBytes(1)[0] % WORDS.length]}${100 + (randomBytes(2).readUInt16LE() % 900)}`
const HOST = opt("host", "play.joinbuilders.org")
const extraEnv = Object.fromEntries(args.flatMap((a, i) => (args[i - 1] === "--env" ? [a.split("=")] : [])))
let VER = "?"
VER = opt("tag", "?")
const log = (...a) => console.log(new Date().toISOString().slice(11, 19), ...a)

async function pickRoom() {
  try {
    const r = await fetch(`https://${HOST}/mm/pick?lead=8`, { signal: AbortSignal.timeout(4000), cache: "no-store" })
    const j = await r.json()
    if (typeof j.room === "number") return j.room
  } catch (e) { log("pick failed", e.message) }
  return 1
}

function playOne() {
  return new Promise(async (resolve) => {
    const room = opt("room", null) ?? (await pickRoom())
    const sid = randomBytes(16).toString("hex")
    const url = `wss://${HOST}/r/${room}/play?token=&sid=${sid}&name=${encodeURIComponent(NAME)}`
    log("room", room)
    const ws = new WebSocket(url)
    let bot = null, hello = null, lastObs = null, endMsg = null, results = null, labels = {}, obsN = 0, startT = Date.now()
    let mode = "?", TRACE = null
    let done = false
    const finish = (why) => {
      if (done) return
      done = true
      try { bot?.stdin.end() } catch {}
      try { ws.close() } catch {}
      const rec = { valid: !!hello, at: new Date().toISOString(), room, why, slot: hello?.slot, place: endMsg?.place, kills: endMsg?.kills, t: lastObs?.t, alive: lastObs?.alive, hp: lastObs?.self?.hp, mode, ver: VER, trace: TRACE, labels, humans: results?.mechs?.filter?.((m) => m.human)?.length, env: extraEnv }
      appendFileSync(join(here, "ghost-log.jsonl"), JSON.stringify(rec) + "\n")
      log("done", JSON.stringify(rec))
      resolve(rec)
    }
    const timer = setTimeout(() => finish("timeout"), 15 * 60 * 1000)
    ws.onopen = () => log("connected")
    ws.onerror = (e) => log("ws error", e.message || "")
    ws.onclose = () => { clearTimeout(timer); if (!endMsg) finish("closed"); }
    ws.onmessage = (ev) => {
      let m
      try { m = JSON.parse(String(ev.data)) } catch { return }
      if (m.type === "lobby") {
        if (!m.you?.ready && m.phase !== "running") ws.send(JSON.stringify({ type: "ready", name: NAME }))
        // an idle room waits for someone to press Start; empty seats are filled with bots
        if (m.phase === "waiting" && !m.gather?.starting && Date.now() - lastStart > 5000) { lastStart = Date.now(); ws.send(JSON.stringify({ type: "start" })) }
        if (m.phase && m.phase !== lastPhase) { log("lobby", m.phase, m.next_at ?? "") ; lastPhase = m.phase }
        return
      }
      if (m.type === "hello") {
        hello = m
        const CMD = opt("cmd", null)
        bot = CMD ? spawn(CMD, { shell: true, env: { ...process.env, ...extraEnv }, stdio: ["pipe", "pipe", "pipe"] }) : spawn(process.execPath, [join(here, "../src/main.mjs")], { env: { ...process.env, ...extraEnv }, stdio: ["pipe", "pipe", "pipe"] })
        let ob = ""
        bot.stdout.on("data", (c) => {
          ob += c; const ls = ob.split("\n"); ob = ls.pop()
          for (const l of ls) {
            if (!l.trim()) continue
            let a; try { a = JSON.parse(l) } catch { continue }
            if (a.type === "ready") continue
            const lab = a.debug?.label; if (lab) { mode = lab[0]; const k = lab.slice(2).split(" ")[0]; labels[k] = (labels[k] || 0) + 1 }
            delete a.debug
            if (ws.readyState === 1) ws.send(JSON.stringify(a))
          }
        })
        mkdirSync(join(here, "traces"), { recursive: true })
        const tf = join(here, "traces", `${Date.now()}-r${room}.log`)
        TRACE = tf.replace(/\.log$/, ".obs.jsonl")
        bot.stderr.on("data", (c) => appendFileSync(tf, c))
        const h2 = { ...m, map: { ...m.map, dir: ARENA } }
        if (FULL) appendFileSync(TRACE.replace(".obs.jsonl", ".raw.jsonl"), JSON.stringify(h2) + "\n")
        bot.stdin.write(JSON.stringify(h2) + "\n")
        ws.send(JSON.stringify({ type: "loaded" }))
        log("hello slot", m.slot)
        return
      }
      if (m.type === "obs") {
        lastObs = m; obsN++
        if (bot) bot.stdin.write(JSON.stringify(m) + "\n")
        if (FULL && TRACE) appendFileSync(TRACE.replace(".obs.jsonl", ".raw.jsonl"), JSON.stringify(m) + "\n")
        if (TRACE) appendFileSync(TRACE, JSON.stringify({ t: m.t, hp: Math.round(m.self.hp), alive: m.alive, cubes: m.self.cubes, pos: m.self.pos.map((v) => Math.round(v)), w: m.self.weapons.map((w) => w?.kind ?? null), zone: [m.zone.radius | 0, m.self.in_zone], mechs: (m.mechs || []).filter((x) => x.kind === "mech").map((x) => [x.id, Math.round(Math.hypot(x.pos[0] - m.self.pos[0], x.pos[1] - m.self.pos[1])), x.hp | 0, (x.weapons || []).join("/")]), ev: m.events?.length ? m.events : undefined }) + "\n")
        if (obsN % 300 === 0) log(`t=${m.t} alive=${m.alive} hp=${m.self.hp | 0} cubes=${m.self.cubes} w=${m.self.weapons.map((w) => w?.kind).join("/")}`)
        return
      }
      if (m.type === "end") { endMsg = m; log("END place", m.place, "kills", m.kills); setTimeout(() => finish("end"), 1500); return }
      if (m.type === "results") { results = m; if (TRACE) appendFileSync(TRACE, JSON.stringify({ results: m }) + "\n"); return }
    }
    let lastPhase = "", lastStart = 0
  })
}

for (let g = 0; g < GAMES; g++) await playOne()
process.exit(0)
