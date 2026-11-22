# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Sparsh (Team 4141). See LICENSE and NOTICE.
"""Survivor bot for mechfight/1.

Scoring is placement (16 - place), so this bot plays for survival:
- sits inside the next zone circle, off-centre, standing still when it can (silent, regenerating)
- faces whoever hurts it (front armour), strafes while fighting, backs away when hurt
- shoots back with lead, uses picked-up laser / lightning / mortar
- dodges incoming mortar shells, avoids NPC drones and supply drops
"""
import math
import os
import random

import mechfight as mf

TAU = 2 * math.pi


def wrap(a):
    return (a + math.pi) % TAU - math.pi


def d2(a, b):
    return math.hypot(a[0] - b[0], a[1] - b[1])


def norm2(v):
    n = math.hypot(v[0], v[1])
    return (v[0] / n, v[1] / n) if n > 1e-9 else (0.0, 0.0)


UPGRADE_PREF = [
    "plating", "armor", "nanites", "laser_damage", "gun_rate", "laser_charge", "gun_damage", "lightning_range",
    "lightning_efficiency", "lightning_chain", "gyros", "reactor", "capacitor", "sensors", "servos", "jets",
    "gun_mag", "gun_velocity", "laser_pierce", "mortar_reload", "mortar_splash", "mortar_velocity",
]
WEAPON_PREF = {"laser": 3, "lightning": 2, "mortar": 1, "gun": 0}
LOADOUT_FFA = {"laser": 3, "gun": 2, "lightning": 1.6, "mortar": 0.5}       # free-for-all winners carry gun + laser
LOADOUT_PRACTICE = {"laser": 3, "lightning": 2.5, "gun": 1.5, "mortar": 0.5}  # practice A/B tests favour laser + lightning


class Bot:
    name = "toaster"

    def __init__(self, hello):
        self.cfg = hello["config"]
        self.rng = random.Random(hello.get("seed", 0))
        self.reach = self.cfg["loot"]["pickup_radius"]
        self.attackers = {}        # id -> (time last hurt us, bearing)
        self.last_dmg_t = -99.0
        self.last_dmg_bearing = None
        self.stuck_t = 0.0
        self.unstick_until = -1.0
        self.unstick_side = 1
        self.last_pos = None
        self.strafe_dir = 1
        self.strafe_until = 0.0
        self.anchor = None          # where we decided to sit
        self.anchor_zone = None
        self.enemy_seen = {}       # id -> last obs dict + t
        self.laser_charging = False
        self.last_enemy_t = -99.0   # last time any enemy mech was in view (or tracked)
        self.last_mech_dmg_t = -99.0
        self.my_id = hello.get("slot")
        self.hide_until = -1.0
        self.loot = {}             # key -> (pos, value): remembered cubes ("c"), crates ("x"), kill spots ("k"), death sounds ("d")
        self.loot_t = {}           # key -> time added
        self.prev_enemy = {}       # id -> last seen pos
        self.wp = None             # exploration waypoint
        self.last_enemy200_t = -99.0
        self.gcover = None
        self.gcover_t = -99.0
        # practice detection: house bots never kill each other, so in practice nobody dies except by our hand
        self.start_alive = None
        self.kills = 0
        self.others_dead_ticks = 0
        self.not_practice = False
        self.stick_id, self.stick_t = None, -99.0
        self.retreat = False
        self.last_threat_t = -99.0
        self.pick_mem = {}          # weapon pickups seen: id -> (kind, pos)
        self.loot_key = None        # current loot goal (commitment)
        self.steer_side = 0
        self.recover_cover = None
        self.recover_t = -99.0
        self.dodge_side = {}        # enemy id -> side we sidestep its laser to
        self.visited = set()        # 100 m grid cells we have been to (early exploration)
        self.steer_lock = -99.0     # keep detouring on the same side until then
        self.pos_hist = []          # (t, x, y) of the last ~3 s, for the progress watchdog
        self.want_since = None      # since when we keep wanting to move (outside fights)
        self.fstrafe, self.fstrafe_t = 1, 0.0
        self.wp_t = -99.0
        self.cover = None
        self.cover_t = -99.0
        # map model for cover (optional: without it the bot just backs off)
        self.W = None
        here = os.path.dirname(os.path.abspath(__file__))
        for d in ((hello.get("map") or {}).get("dir"), "/arena", os.path.join(here, "arena")):
            try:
                if d:
                    import world
                    self.W = world.load(d)
                    break
            except Exception as e:  # noqa: BLE001
                mf.log(f"map load failed for {d}: {e!r}")

    def steer_dir(self, pos, yaw0, t):
        """Clear heading near yaw0; once we detour to one side, keep that side for 3 s (no left-right dithering)."""
        W = self.W
        if W.on_roof(pos[0], pos[1], pos[2]):
            return yaw0
        j, i = W.idx(pos[0], pos[1])
        look = 6.0 if W.blocked[j, i] else 14.0
        side = self.steer_side if t < self.steer_lock else 0
        offs = [k * math.radians(18) for k in range(1, 10)]
        if side:
            order = [0.0] + [side * o for o in offs] + [-side * o for o in offs]
        else:
            order = [0.0]
            for o in offs:
                order += [o, -o]
        for off in order:
            if W.clear_ahead(pos[0], pos[1], yaw0 + off, look) >= look:
                if off != 0 and not side:
                    self.steer_side, self.steer_lock = (1 if off > 0 else -1), t + 3.0
                return yaw0 + off
        return yaw0

    # ------------------------------------------------------------------ cover
    def shot_clear(self, me, tgt):
        """True if no building/terrain is between our muzzle and the target (trees don't stop shots)."""
        a = (me["pos"][0], me["pos"][1], me["pos"][2] + me["stats"].get("muzzle_height", 7))
        b = (tgt["pos"][0], tgt["pos"][1], tgt["pos"][2] + 4.5)
        return self.W.los(a, b, canopy=False, step=1.5)

    def find_cover(self, me, threat):
        """Nearest reachable point where a building hides our whole body from `threat`."""
        W = self.W
        pos = me["pos"]
        tz = threat[2] + 8
        away = math.atan2(pos[1] - threat[1], pos[0] - threat[0])
        cands = []
        for r in (8, 14, 22, 32):
            for k in range(12):
                a = away + (k - 6) * (math.pi / 6)
                x, y = pos[0] + r * math.cos(a), pos[1] + r * math.sin(a)
                if not W.in_bounds(x, y, 10):
                    continue
                j, i = W.idx(x, y)
                if W.blocked[j, i] or W.clear_ahead(pos[0], pos[1], a, r - 2) < r - 4:
                    continue
                cands.append((x, y, r))
        if not cands:
            return None
        A, B = [], []
        for (x, y, r) in cands:
            gz = W.surface_at(x, y)
            for h in (2.0, 5.0, 9.5):
                A.append((threat[0], threat[1], tz))
                B.append((x, y, gz + h))
        vis = W.los_many(A, B, canopy=False, step=1.5)
        best = None
        for k, (x, y, r) in enumerate(cands):
            if not vis[3 * k:3 * k + 3].any():
                if best is None or r < best[2]:
                    best = (x, y, r)
        return best[:2] if best else None

    # ------------------------------------------------------------------ helpers
    def safe_spot(self, obs, me):
        """A point inside the next circle, between us and its centre, away from the crowded middle."""
        z = obs["zone"]
        nc, nr = z["next_center"], z["next_radius"]
        key = (round(nc[0], 1), round(nc[1], 1), round(nr, 1))
        if self.anchor is not None and self.anchor_zone == key:
            return self.anchor
        pos = me["pos"]
        dx, dy = pos[0] - nc[0], pos[1] - nc[1]
        dist = math.hypot(dx, dy)
        frac = 0.55 if nr > 100 else (0.4 if nr > 30 else 0.0)
        want = nr * frac
        if dist <= want and dist > 1e-6:
            spot = (pos[0], pos[1])          # already well inside: stay
        elif dist > 1e-6:
            spot = (nc[0] + dx / dist * want, nc[1] + dy / dist * want)
        else:
            spot = (nc[0], nc[1])
        if self.W is not None and nr <= 140:
            spot = self.staging_spot(nc, nr, pos) or spot
        self.anchor, self.anchor_zone = spot, key
        return spot

    def staging_spot(self, nc, nr, pos):
        """Late zone: a reachable point near the next circle whose surroundings see it the least."""
        W = self.W
        cands = []
        rng = random.Random(int(nc[0] * 7 + nc[1] * 13))
        for _ in range(40):
            a, r = rng.uniform(0, TAU), nr * 0.5 * math.sqrt(rng.random())
            x, y = nc[0] + r * math.cos(a), nc[1] + r * math.sin(a)
            if not W.in_bounds(x, y, 10):
                continue
            j, i = W.idx(x, y)
            if W.blocked[j, i]:
                continue
            cands.append((x, y))
            if len(cands) >= 24:
                break
        if not cands:
            return None
        A, B = [], []
        ring = [(math.cos(k * TAU / 12), math.sin(k * TAU / 12)) for k in range(12)]
        for (x, y) in cands:
            gz = W.surface_at(x, y)
            for (cx, cy) in ring:
                ox, oy = x + 70 * cx, y + 70 * cy
                A.append((ox, oy, W.surface_at(ox, oy) + 8))
                B.append((x, y, gz + 6))
        vis = W.los_many(A, B, canopy=True, step=2.0)
        best = None
        for k, (x, y) in enumerate(cands):
            expo = float(vis[12 * k:12 * k + 12].mean())
            sc = expo + 0.25 * math.hypot(x - pos[0], y - pos[1]) / max(nr, 1)
            if best is None or sc < best[0]:
                best = (sc, (x, y))
        return best[1]

    def lead(self, me, tgt, speed):
        mz = me["pos"][2] + me["stats"].get("muzzle_height", 7)
        cubes = tgt.get("cubes", 0) or 0
        tz_off = 4.5 * (1 + 0.05 * cubes) if tgt["kind"] == "mech" else 1.75
        p = [tgt["pos"][0], tgt["pos"][1], tgt["pos"][2] + tz_off]
        v = tgt.get("vel", [0, 0, 0])
        aim_p = p
        for _ in range(3):
            dist = math.dist((me["pos"][0], me["pos"][1], mz), aim_p)
            t = max(0.0, dist - 3.0) / speed
            aim_p = [p[0] + v[0] * t, p[1] + v[1] * t, p[2] + v[2] * t * 0.5]
        dx, dy, dz = aim_p[0] - me["pos"][0], aim_p[1] - me["pos"][1], aim_p[2] - mz
        h = math.hypot(dx, dy)
        return math.atan2(dy, dx), math.atan2(dz, max(h, 1e-6)), h

    def mortar_pitch(self, h, dz, v, g):
        """Low-arc pitch to hit horizontal distance h with height difference dz."""
        v2 = v * v
        disc = v2 * v2 - g * (g * h * h + 2 * dz * v2)
        if disc < 0:
            return None
        return math.atan2(v2 - math.sqrt(disc), g * h)

    def shell_threat(self, obs, me):
        """Return a horizontal escape vector if a mortar shell will land near us."""
        g = self.cfg["weapons"]["mortar"]["gravity"]
        r = self.cfg["weapons"]["mortar"]["splash_radius"] + 4
        px, py, pz = me["pos"]
        esc = [0.0, 0.0]
        hit = False
        for pr in obs["projectiles"]:
            if pr["kind"] != "mortar":
                continue
            x, y, z = pr["pos"]
            vx, vy, vz = pr["vel"]
            # time until shell reaches our height (downward)
            a, b, c = -0.5 * g, vz, z - (pz + 2)
            disc = b * b - 4 * a * c
            if disc < 0:
                continue
            t = (-b - math.sqrt(disc)) / (2 * a)
            if t < 0 or t > 6:
                continue
            lx, ly = x + vx * t, y + vy * t
            dd = math.hypot(lx - px, ly - py)
            if dd < r:
                hit = True
                ex, ey = norm2((px - lx, py - ly)) if dd > 0.5 else norm2((-vy, vx))
                esc[0] += ex
                esc[1] += ey
        return norm2(esc) if hit else None

    # ------------------------------------------------------------------ main
    def act(self, obs):
        LOADOUT_VAL = LOADOUT_FFA if self.not_practice else LOADOUT_PRACTICE
        t = obs["t"]
        me = obs["self"]
        pos = me["pos"]
        z = obs["zone"]
        act = {}

        # upgrades
        if obs.get("offer"):
            ch = obs["offer"]["choices"]
            best, bi = 999, 0
            for i, c in enumerate(ch):
                r = UPGRADE_PREF.index(c["id"]) if c["id"] in UPGRADE_PREF else 50
                if r < best:
                    best, bi = r, i
            act["pick"] = bi

        # damage bookkeeping
        for ev in obs["events"]:
            if ev["type"] == "damaged" and ev.get("by") is not None:
                self.attackers[ev["by"]] = (t, ev.get("bearing"))
                if ev["by"] < 100:   # a mech, not a drone
                    self.last_dmg_t = t
                    self.last_dmg_bearing = ev.get("bearing")
                    self.last_mech_dmg_t = t
            elif ev["type"] == "kill":
                self.kills += 1
        alive = obs.get("alive", 16)
        if self.start_alive is None:
            self.start_alive = alive
        self.others_dead_ticks = self.others_dead_ticks + 1 if self.start_alive - alive - self.kills > 0 else 0
        if self.others_dead_ticks >= 5:
            self.not_practice = True
        practice = not self.not_practice and t > 20
        self.attackers = {k: v for k, v in self.attackers.items() if t - v[0] < 6}

        enemies = [m for m in obs["mechs"] if m["kind"] == "mech"]
        npcs = [m for m in obs["mechs"] if m["kind"] == "npc"]
        for e in enemies:
            e["_d"] = math.dist(e["pos"], pos)
        if any(e["_d"] < 110 for e in enemies):
            self.last_enemy_t = t   # (far-off mechs are almost always in view; they don't stop farming)

        hp_frac = me["hp"] / max(me["max_hp"], 1)
        mech_hit = t - self.last_mech_dmg_t < 3
        # endgame guerilla: last circles (or few left) and we have collected enough power
        endgame = z["radius"] < 250 or obs.get("alive", 16) <= 5
        powered = (me.get("cubes") or 0) >= 3
        guerilla = endgame and powered
        near = [e for e in enemies if e["_d"] < 150]
        # survival rules: shoot only at enemies that are a danger to us, never join group fights
        for e in enemies:
            bu = math.atan2(pos[1] - e["pos"][1], pos[0] - e["pos"][0])
            aim_err = abs(wrap(e["aim"][0] - bu))
            ux, uy = norm2((pos[0] - e["pos"][0], pos[1] - e["pos"][1]))
            closing = e["vel"][0] * ux + e["vel"][1] * uy > 2
            e["_danger"] = (e["id"] in self.attackers or e["_d"] < 55
                            or (e.get("charging") and aim_err < 0.12 and e["_d"] < 280)
                            or (e["_d"] < 130 and closing and aim_err < 0.25))
        late = z["next_radius"] <= 70 or alive <= 3
        for e in enemies:
            e["_los"] = True if self.W is None or e["_d"] > 240 else self.shot_clear(me, e)
            if practice:
                bu = math.atan2(pos[1] - e["pos"][1], pos[0] - e["pos"][0])
                aim_err = abs(wrap(e["aim"][0] - bu))
                ux, uy = norm2((pos[0] - e["pos"][0], pos[1] - e["pos"][1]))
                closing = e["vel"][0] * ux + e["vel"][1] * uy > 3
                low = e.get("hp") is not None and e.get("max_hp") and e["hp"] < 0.4 * e["max_hp"]
                e["_danger"] = (e["_d"] < 60 or e["id"] in self.attackers or late or (closing and e["_d"] < 180)
                                or (low and e["_d"] < 160) or (aim_err < 0.22 and e["_d"] < 190))
        if any(e["_danger"] for e in enemies) or mech_hit:
            self.last_threat_t = t
        if practice:
            crowd = sum(1 for e in enemies if e["_d"] < 150 and (e["_danger"] or e["_los"]))
            group = crowd >= 3 or (crowd == 2 and hp_frac < 0.65)
        else:
            group = len(near) >= 2
        # retreat to cover when badly hurt, come back out once healed
        if not self.retreat and hp_frac < 0.38 and t - self.last_threat_t < 2:
            self.retreat = True
        if self.retreat and (hp_frac > 0.8 or (t - self.last_threat_t > 7 and hp_frac > 0.55)):
            self.retreat = False
        if any(e["_d"] < 200 for e in enemies):
            self.last_enemy200_t = t
        # nothing around for a while: farm drones, crates, cubes and supply drops (shooting is loud)
        clear = t - self.last_enemy200_t > 8 and not mech_hit and hp_frac > 0.4
        if self.not_practice:   # confirmed free-for-all (final): farm when nobody within ~110 m for 4 s, nothing hit us
            clear = t - self.last_enemy_t > 4 and not mech_hit and hp_frac > 0.5
        if practice:   # hunters come to us anyway: farm whenever nobody is threatening us right now
            clear = t - self.last_threat_t > 3 and hp_frac > 0.5
        early = not self.not_practice and t < 200 and (me.get("cubes") or 0) < 10 and hp_frac >= 0.5
        if early:
            clear = (t - self.last_mech_dmg_t > 4 and not any(e["_danger"] and e["_d"] < 150 for e in enemies))

        # ---------------- choose target
        target = None
        my_kinds = {w["kind"] for w in me["weapons"] if w}
        if enemies:
            def score(e):
                s = e["_d"]
                if e["id"] in self.attackers:
                    s -= 120
                if e["_danger"]:
                    s -= 150
                if not e["_los"]:
                    s += 200          # can't hit it from here
                if e["id"] == self.stick_id and t - self.stick_t < 2:
                    s -= 100          # keep the same target: every switch costs turning time
                bu_t = math.atan2(e["pos"][1] - pos[1], e["pos"][0] - pos[0])
                if abs(wrap(e["aim"][0] - (bu_t + math.pi))) > 1.75:
                    s -= 30           # facing away from us: rear hits do x1.5
                if "laser" in my_kinds and "mortar" in (e.get("weapons") or []):
                    s -= 40           # our laser cooks off its mortar: +60 %
                if "lightning" in my_kinds and e.get("charging") and e["_d"] < 45:
                    s -= 150          # our lightning cancels its laser charge
                s += 60 * abs(wrap(me["aim"][0] - bu_t))   # turning cost: up to ~0.8 s for a half turn
                if e["id"] in self.attackers and t - self.attackers[e["id"]][0] < 2:
                    s -= 100          # hitting us right now
                if e["_d"] < 45:
                    s -= 120          # in everyone's effective range
                if "lightning" in (e.get("weapons") or []) and e["_d"] < 55:
                    s -= 100          # lightning shreds at close range
                if e.get("charging"):
                    bu_ = math.atan2(pos[1] - e["pos"][1], pos[0] - e["pos"][0])
                    if abs(wrap(e["aim"][0] - bu_)) < 0.15:
                        s -= 100      # laser about to hit us
                if e.get("hp") is not None and e.get("max_hp") and e["hp"] <= 0.3 * e["max_hp"]:
                    s -= 100
                if e.get("hp") is not None and e.get("max_hp"):
                    s -= 60 * (1 - e["hp"] / e["max_hp"])
                return s
            target = min(enemies, key=score)
            self.stick_id, self.stick_t = target["id"], t
        cheap = (target is not None and target.get("hp") is not None and target.get("max_hp")
                 and target["hp"] / target["max_hp"] < 0.3 and len(near) <= 1 and target["_d"] < 150)
        hold = target is not None and (not (target["_danger"] or cheap) or not target["_los"])   # watch, don't shoot
        flee = False
        if not practice and target is not None and target["_d"] < 150 and not cheap and target.get("hp") is not None:
            my_w = sum(1 for w in me["weapons"] if w)
            th_w = max(1, len(target.get("weapons") or []))
            busy_me = 1.0 if any(k != target["id"] for k in self.attackers) else 0.0
            busy_them = 0.0
            for u in enemies:
                if u["id"] != target["id"] and d2(u["pos"], target["pos"]) < 160:
                    if abs(wrap(target["aim"][0] - math.atan2(u["pos"][1] - target["pos"][1], u["pos"][0] - target["pos"][0]))) < 0.2:
                        busy_them = 1.0
            zz = (1.5 * math.log(max(me["hp"], 1) / max(target["hp"], 1))
                  + 0.07 * (min(me.get("cubes") or 0, 10) - min(target.get("cubes") or 0, 10))
                  + 0.9 * (my_w - th_w) - 0.6 * (busy_me - busy_them))
            flee = 1 / (1 + math.exp(-zz)) < 0.25

        # ---------------- movement
        spot = self.safe_spot(obs, me)
        to_spot = (spot[0] - pos[0], spot[1] - pos[1])
        dist_spot = math.hypot(*to_spot)
        mv = [0.0, 0.0]
        urgent = not me["in_zone"]
        # will the shrinking zone catch us? estimate distance to the zone edge at shrink_end
        nc, nr = z["next_center"], z["next_radius"]
        dn = d2(pos, nc)
        if dn > nr * 0.9:
            time_left = z["shrink_end"] - t
            need = (dn - nr * 0.7) / me["stats"].get("walk_speed", 11.7)
            if time_left < need + 8:
                urgent = True
        if dist_spot > 4:
            d = norm2(to_spot)
            mv = [d[0], d[1]]

        threatened = target is not None and (target["id"] in self.attackers or target["_d"] < 90)
        engaged = threatened or (t - self.last_dmg_t < 3)

        has_laser = any(w and w["kind"] == "laser" for w in me["weapons"])
        # a lone enemy while we are healthy and laser-armed: close in to a good laser range
        if cheap and hp_frac > 0.5 and has_laser and 120 < target["_d"] < 200 and not urgent:
            hx, hy = norm2((target["pos"][0] - pos[0], target["pos"][1] - pos[1]))
            mv = [mv[0] * 0.3 + hx * 0.8, mv[1] * 0.3 + hy * 0.8]
        # hunt: close in on weakened enemies while healthy
        if cheap and not group and hp_frac > 0.6 and target.get("hp") is not None and target.get("max_hp") and target["hp"] / target["max_hp"] < 0.5 and target["_d"] > 40:
            hx, hy = norm2((target["pos"][0] - pos[0], target["pos"][1] - pos[1]))
            mv = [mv[0] * 0.3 + hx, mv[1] * 0.3 + hy]
        far_ok = early and target is not None and target["_d"] > 150 and target["id"] not in self.attackers
        if practice and target is not None and target["_danger"] and hp_frac > 0.55 and not group and not urgent and not far_ok:
            want = 110 if has_laser else 70
            if target["_d"] > want or not target["_los"]:
                hx, hy = norm2((target["pos"][0] - pos[0], target["pos"][1] - pos[1]))
                mv = [mv[0] * 0.2 + hx, mv[1] * 0.2 + hy]
        # kite away from close enemies when hurt / outnumbered, keep facing them
        if target is not None:
            ex, ey = norm2((pos[0] - target["pos"][0], pos[1] - target["pos"][1]))
            n_close = sum(1 for e in enemies if e["_d"] < 120)
            if (hp_frac < 0.55 or n_close >= 2) and target["_d"] < 140:
                mv = [mv[0] * 0.4 + ex, mv[1] * 0.4 + ey]
            elif not threatened and target["_d"] < 160:
                mv = [mv[0] + ex * 0.6, mv[1] + ey * 0.6]
        if (group or flee or (self.retreat and target is not None)) and not urgent and near:
            # a group nearby: get behind a building away from them (or back off) and stay quiet
            cx = sum(e["pos"][0] for e in near) / len(near)
            cy = sum(e["pos"][1] for e in near) / len(near)
            nearest = min(near, key=lambda e: e["_d"])
            goal = None
            if self.W is not None:
                if self.gcover is None or t - self.gcover_t > 1.5:
                    self.gcover = self.find_cover(me, nearest["pos"])
                    self.gcover_t = t
                if self.gcover is not None and d2(self.gcover, z["center"]) < z["radius"] - 10:
                    goal = self.gcover
            if goal is not None:
                gx, gy = goal[0] - pos[0], goal[1] - pos[1]
                mv = list(norm2((gx, gy))) if math.hypot(gx, gy) > 2 else [0.0, 0.0]
            elif target is not None and target["_d"] < 160 and target["_los"]:
                if t > self.fstrafe_t:
                    if self.rng.random() < 0.7:
                        self.fstrafe = -self.fstrafe
                    self.fstrafe_t = t + 1.0 + self.rng.random() * 1.6
                ux, uy = norm2((target["pos"][0] - pos[0], target["pos"][1] - pos[1]))
                radial = max(-1.0, min(1.0, (target["_d"] - 70) / 30))
                mv = [ux * radial - uy * 0.85 * self.fstrafe, uy * radial + ux * 0.85 * self.fstrafe]
            elif d2(pos, (cx, cy)) < 160:
                ex, ey = norm2((pos[0] - cx, pos[1] - cy))
                mv = [ex * 0.8, ey * 0.8]
            else:
                mv = [0.0, 0.0]

        recovering = False
        if (self.W is not None and not urgent and not group and hp_frac < (0.55 if practice else 0.7) and t - self.last_threat_t > 2
                and not any(e["_danger"] for e in enemies)):
            b = self.last_dmg_bearing
            if b is not None:
                if self.recover_cover is None or t - self.recover_t > 3:
                    self.recover_cover = self.find_cover(me, (pos[0] + 80 * math.cos(b), pos[1] + 80 * math.sin(b), pos[2]))
                    self.recover_t = t
                if self.recover_cover is not None and d2(self.recover_cover, z["center"]) < z["radius"] - 10:
                    gx, gy = self.recover_cover[0] - pos[0], self.recover_cover[1] - pos[1]
                    mv = list(norm2((gx, gy))) if math.hypot(gx, gy) > 2 else [0.0, 0.0]
                    recovering = True
            if not recovering:
                mv = [0.0, 0.0]      # no cover: stand still and silent while regenerating
                recovering = True
        if recovering:
            clear = False

        # farming when the coast is clear: cubes > supply drop > drone > crate
        farm = None          # thing to shoot: dict with pos/vel/kind/_d
        farm_goal = None     # where to walk
        farm_stand = 0.0     # stop this far from farm_goal
        on_drop = False
        if not clear and not urgent and not group and hp_frac < 0.6:
            for dr in obs.get("drops", []):
                dd = d2(pos, dr["pos"])
                contested = any(d2(e["pos"], dr["pos"]) < 90 for e in enemies)
                if (dd < 120 and not contested and dr.get("holder") in (None, self.my_id) and dr["lands_in"] < dd / 11.7 + 2
                        and d2(dr["pos"], z["center"]) < z["radius"] - 5):
                    farm_goal = dr["pos"]
                    on_drop = dd < dr["radius"] - 2.5
                    mv = [0.0, 0.0] if on_drop else list(norm2((dr["pos"][0] - pos[0], dr["pos"][1] - pos[1])))
                    break
        if clear and not urgent:
            zc, zr = z["center"], z["radius"]

            def inside(p, m=10):
                return d2(p, zc) < zr - m and d2(p, nc) < nr + 20
            cubes_ok = [c for c in obs["cubes"] if d2(pos, c["pos"]) < 70 and abs(c["pos"][2] - pos[2]) < 4 and inside(c["pos"])]
            if cubes_ok:
                c = min(cubes_ok, key=lambda c: d2(pos, c["pos"]))
                farm_goal = c["pos"]
            if farm_goal is None:
                for dr in obs.get("drops", []):
                    dd = d2(pos, dr["pos"])
                    contested = any(d2(e["pos"], dr["pos"]) < 90 for e in enemies)
                    if dd < 150 and not contested and dr.get("holder") in (None, self.my_id) and dr["lands_in"] < dd / 11.7 + 2 and inside(dr["pos"], 5):
                        farm_goal = dr["pos"]
                        on_drop = dd < dr["radius"] - 2.5
                        break
            drones = [n for n in npcs if d2(pos, n["pos"]) < 110 and inside(n["pos"])]
            if drones:
                n = min(drones, key=lambda n: d2(pos, n["pos"]))
                farm = dict(n)
                if farm_goal is None:
                    farm_goal, farm_stand = n["pos"], 38 if "lightning" in {w["kind"] for w in me["weapons"] if w} else 50
            elif farm_goal is None or not on_drop:
                crates = [c for c in obs["crates"] if d2(pos, c["pos"]) < (150 if practice else 70) and abs(c["pos"][2] - pos[2]) < 4 and inside(c["pos"])]
                if crates:
                    c = min(crates, key=lambda c: d2(pos, c["pos"]))
                    farm = {"id": c["id"], "kind": "crate", "pos": c["pos"], "vel": [0, 0, 0]}
                    if farm_goal is None:
                        farm_goal, farm_stand = c["pos"], 25
            if farm is not None:
                farm["_d"] = math.dist(farm["pos"], pos)
            if farm_goal is not None:
                dd = d2(pos, farm_goal)
                if on_drop:
                    mv = [0.0, 0.0]
                elif dd > farm_stand:
                    mv = list(norm2((farm_goal[0] - pos[0], farm_goal[1] - pos[1])))
                else:
                    mv = [0.0, 0.0]

        # ---- scavenging: remember cubes / crates / where mechs died, and go collect
        for c in obs["cubes"]:
            self.loot[("c", c["id"])] = (c["pos"], 1.0)
        for cr in obs["crates"]:
            if abs(cr["pos"][2] - pos[2]) < 4:
                self.loot[("x", cr["id"])] = (cr["pos"], 0.4)
        for n_ in npcs:
            self.loot[("n", n_["id"])] = (n_["pos"], 0.8)
        self.visited.add((int(pos[0] // 100), int(pos[1] // 100)))
        for ev in obs["events"]:
            if ev["type"] == "kill" and ev.get("target") in self.prev_enemy:
                self.loot[("k", ev["target"])] = (self.prev_enemy[ev["target"]], 3.0)
                self.loot_t[("k", ev["target"])] = t
        for s in obs["sounds"]:
            if False and s["kind"] == "death":
                r = 40 if s["dist"] == "near" else 110
                key = ("d", round(t - s.get("age", 0), 1))
                if key not in self.loot:
                    self.loot[key] = ((pos[0] + r * math.cos(s["bearing"]), pos[1] + r * math.sin(s["bearing"]), pos[2]), 1.5)
                    self.loot_t[key] = t
        seen = ({("c", c["id"]) for c in obs["cubes"]} | {("x", c["id"]) for c in obs["crates"]}
                | {("n", n_["id"]) for n_ in npcs})
        for k, (p, v) in list(self.loot.items()):
            dd = d2(pos, p)
            if (k[0] in ("c", "x", "n") and dd < 30 and k not in seen) or (k[0] in ("k", "d") and (dd < 12 or t - self.loot_t.get(k, t) > 60)):
                self.loot.pop(k, None)
                self.loot_t.pop(k, None)
        loot_run = None
        safe_enough = hp_frac > 0.35 and not (mech_hit and hp_frac < 0.6) and not urgent and not on_drop
        if practice and hp_frac > 0.5 and not urgent and not on_drop:
            safe_enough = True       # practice: grab nearby cubes even mid-fight
        if recovering:
            safe_enough = False
        if safe_enough and farm_goal is None and not group:
            bl = None
            for k, (p, v) in self.loot.items():
                if k[0] in ("x", "n") and not clear:
                    continue
                if practice and mech_hit and (k[0] not in ("c", "k") or d2(pos, p) > 70):
                    continue
                dd = d2(pos, p)
                if dd > 180 or d2(p, z["center"]) > z["radius"] - 12 or d2(p, nc) > nr + 40:
                    continue
                if any(d2(e["pos"], p) < 80 for e in enemies if e["id"] not in (k[1],)):
                    continue    # someone is standing on it
                sc = dd - 40 * v - (15 if k == self.loot_key else 0)   # stick with the current goal
                if bl is None or sc < bl[0]:
                    bl = (sc, k, p)
            self.loot_key = bl[1] if bl is not None else None
            if bl is not None:
                loot_run = bl[2]
                dd = d2(pos, loot_run)
                if (bl[1][0] == "x" and dd < 20) or (bl[1][0] == "n" and dd < 45):
                    mv = [0.0, 0.0]          # crate / drone: stand and let the farm code shoot it
                else:
                    mv = list(norm2((loot_run[0] - pos[0], loot_run[1] - pos[1])))
            elif early and clear:
                # explore: walk to the nearest unvisited 100 m cell inside the circle (spinning sees 200 m)
                if self.wp is None or d2(pos, self.wp) < 15 or t > self.wp_t + 30:
                    self.wp = None
                    bestc = None
                    ci, cj = int(pos[0] // 100), int(pos[1] // 100)
                    for di in range(-3, 4):
                        for dj in range(-3, 4):
                            cell = (ci + di, cj + dj)
                            if cell in self.visited:
                                continue
                            cand = (cell[0] * 100 + 50, cell[1] * 100 + 50)
                            if d2(cand, z["center"]) > z["radius"] - 30 or d2(cand, nc) > nr + 30:
                                continue
                            if self.W is not None and not self.W.in_bounds(cand[0], cand[1], 20):
                                continue
                            dd_ = d2(pos, cand)
                            if bestc is None or dd_ < bestc[0]:
                                bestc = (dd_, cand)
                    if bestc is not None:
                        self.wp, self.wp_t = bestc[1], t
                if self.wp is not None:
                    mv = list(norm2((self.wp[0] - pos[0], self.wp[1] - pos[1])))
                    loot_run = self.wp
        self.prev_enemy = {e["id"]: e["pos"] for e in enemies}

        # repel from NPC homes / drones and supply drops (fights happen there) - unless farming them
        for n in npcs if not clear and loot_run is None else []:
            dd = d2(pos, n["pos"])
            if dd < 60:
                ex, ey = norm2((pos[0] - n["pos"][0], pos[1] - n["pos"][1]))
                w = (60 - dd) / 60 * 1.5
                mv = [mv[0] + ex * w, mv[1] + ey * w]
        for dr in obs.get("drops", []) if not clear and loot_run is None else []:
            dd = d2(pos, dr["pos"])
            if dd < 45:
                ex, ey = norm2((pos[0] - dr["pos"][0], pos[1] - dr["pos"][1]))
                w = (45 - dd) / 45 * 1.2
                mv = [mv[0] + ex * w, mv[1] + ey * w]

        # weapons: aim for the top teams' loadout, laser + lightning (gun kept until we have both)
        slots = me["weapons"]
        have = {w["kind"] for w in slots if w}

        def swap_slot(kind):
            """Slot to put `kind` in (None = not an upgrade). Empty slot first, else replace the weakest weapon."""
            if kind in have:
                return None
            if slots[1] is None:
                return 1
            if slots[0] is None:
                return 0
            vals = [(LOADOUT_VAL.get(w["kind"], 0), i) for i, w in enumerate(slots)]
            worst_v, worst_i = min(vals)
            if LOADOUT_VAL.get(kind, 0) <= worst_v:
                return None
            w = slots[worst_i]
            if w["kind"] == "laser" and (w.get("charge") or 0) > 0:
                return None
            return worst_i

        armed = bool(have & {"laser", "lightning"})
        calm = not engaged and not urgent
        loot_goal = None
        best = None
        for p in obs["pickups"]:
            self.pick_mem[p["id"]] = (p["kind"], p["pos"])
        seen_p = {p["id"] for p in obs["pickups"]}
        for pid, (kind, pp) in list(self.pick_mem.items()):
            if pid not in seen_p and d2(pos, pp) < 30:
                del self.pick_mem[pid]
        danger_now = any(e["_danger"] for e in enemies) or mech_hit
        seek_range = 240 if "laser" not in have else (90 if not armed else 45)
        if (calm or (not armed and not (target is not None and target["_d"] < 60))) and not recovering and not danger_now:
            for pid, (kind, pp) in self.pick_mem.items():
                if swap_slot(kind) is None:
                    continue
                dd = d2(pos, pp)
                rng_k = seek_range if kind == "laser" else min(seek_range, 90)
                if dd < rng_k and abs(pp[2] - pos[2]) < 6 and d2(pp, z["center"]) < z["radius"] - 10 and d2(pp, nc) < nr + 40:
                    score = dd - 25 * LOADOUT_VAL.get(kind, 0)
                    if best is None or score < best[0]:
                        best = (score, pp)
            if best is None and "laser" not in have and t < 90 and loot_run is None and farm_goal is None:
                # scout for a laser: walk to a new spot inside the circle every ~20 s (spinning sees 200 m)
                if self.wp is None or d2(pos, self.wp) < 10 or t > self.wp_t + 20:
                    for _ in range(8):
                        a, r = self.rng.uniform(0, TAU), self.rng.uniform(60, 130)
                        cand = (pos[0] + r * math.cos(a), pos[1] + r * math.sin(a))
                        if d2(cand, nc) < nr * 0.8 and d2(cand, z["center"]) < z["radius"] - 25:
                            self.wp, self.wp_t = cand, t
                            break
                if self.wp is not None:
                    best = (0, self.wp)
        if calm:
            for c in obs["cubes"]:
                dd = d2(pos, c["pos"])
                if dd < 35 and abs(c["pos"][2] - pos[2]) < 6 and d2(c["pos"], nc) < nr:
                    if best is None or dd < best[0]:
                        best = (dd, c["pos"])
        if best and not on_drop:
            loot_goal = best[1]
            mv = list(norm2((loot_goal[0] - pos[0], loot_goal[1] - pos[1])))
        # take weapons in reach
        for p in obs["pickups"]:
            if d2(pos, p["pos"]) < self.reach - 0.3 and abs(p["pos"][2] - pos[2]) < 3.8:
                s = swap_slot(p["kind"])
                if s is None:
                    continue
                act["take"] = p["id"]
                if slots[s] is not None:
                    act["drop"] = s
                break

        # strafe while engaged (dodges bullets and telegraphed lasers)
        hiding = False
        if guerilla and target is not None and has_laser and target["_d"] < 200:
            lz = next(w for w in me["weapons"] if w and w["kind"] == "laser")
            txy = target["pos"]
            if lz["cooldown"] > 1.0 and t >= self.hide_until:
                # just fired: hide until the laser is back (longer if we need to regenerate)
                self.hide_until = t + lz["cooldown"] + (5.0 if hp_frac < 0.4 else 0.0)
                self.cover = None
            if t < self.hide_until:
                if self.W is not None and (self.cover is None or t - self.cover_t > 1.0):
                    self.cover = self.find_cover(me, txy)
                    self.cover_t = t
                if self.cover is not None and d2(self.cover, z["center"]) < z["radius"] - 8:
                    gx, gy = self.cover[0] - pos[0], self.cover[1] - pos[1]
                    mv = list(norm2((gx, gy))) if math.hypot(gx, gy) > 2 else [0.0, 0.0]
                    hiding = True
                else:   # no cover found: back off instead
                    ex, ey = norm2((pos[0] - txy[0], pos[1] - txy[1]))
                    mv = [mv[0] + ex, mv[1] + ey]
            elif self.W is not None and lz["cooldown"] <= 0.3 and not self.shot_clear(me, target):
                # laser ready but a building is in the way: peek out toward it
                hx, hy = norm2((txy[0] - pos[0], txy[1] - pos[1]))
                mv = [hx, hy]
        jink = False
        if (target is not None and (practice or (self.not_practice and target.get("_danger"))) and target.get("kind") == "mech" and not hold and not hiding and not group and not far_ok
                and not self.retreat and not urgent and not recovering
                and not (loot_run is not None and d2(pos, loot_run) < 40)):
            has_light = any(w and w["kind"] == "lightning" for w in me["weapons"])
            d = target["_d"]
            pref = 145 if has_laser else (28 if has_light else 75)
            if has_laser and has_light and d < 60:
                pref = 28
            if (target.get("hp") is not None and target.get("max_hp") and target["hp"] < 0.38 * target["max_hp"]
                    and len(near) <= 1):
                pref = 14            # finish it
            if t > self.fstrafe_t:
                if self.rng.random() < 0.7:
                    self.fstrafe = -self.fstrafe
                self.fstrafe_t = t + 1.0 + self.rng.random() * 1.6
            ux, uy = norm2((target["pos"][0] - pos[0], target["pos"][1] - pos[1]))
            radial = max(-1.0, min(1.0, (d - pref) / 30))
            s = 0.85 * self.fstrafe
            mv = [ux * radial - uy * s, uy * radial + ux * s]
            charging_ = any(w and w["kind"] == "laser" and (w.get("charge") or 0) > 0 for w in me["weapons"])
            jink = me["energy"] > 40 and not charging_
        elif mech_hit and target is not None and not hold and not hiding and loot_run is None:
            if t >= self.strafe_until:
                self.strafe_dir = -self.strafe_dir if self.rng.random() < 0.7 else self.strafe_dir
                self.strafe_until = t + 0.6 + self.rng.random() * 0.9
            ty = math.atan2(target["pos"][1] - pos[1], target["pos"][0] - pos[0])
            sx, sy = math.cos(ty + math.pi / 2) * self.strafe_dir, math.sin(ty + math.pi / 2) * self.strafe_dir
            w = 1.2 if target.get("charging") else 0.8
            mv = [mv[0] + sx * w, mv[1] + sy * w]

        dvx = dvy = 0.0
        for e in enemies:
            if not e.get("charging") or e["_d"] < 5 or e["_d"] > 290:
                continue
            ax, ay = math.cos(e["aim"][0]), math.sin(e["aim"][0])
            rx, ry = pos[0] - e["pos"][0], pos[1] - e["pos"][1]
            along = rx * ax + ry * ay
            cross = rx * ay - ry * ax
            if along <= 0 or abs(cross) > 7 + 0.03 * along:
                continue
            side = self.dodge_side.get(e["id"]) or (1 if cross >= 0 else -1)
            dx, dy = ay * side, -ax * side
            if self.W is not None and self.W.clear_ahead(pos[0], pos[1], math.atan2(dy, dx), 6) < 4:
                side, dx, dy = -side, -dx, -dy
            self.dodge_side[e["id"]] = side
            dvx += dx
            dvy += dy
        for p in obs["projectiles"]:
            if p["kind"] == "mortar":
                continue
            rx, ry = pos[0] - p["pos"][0], pos[1] - p["pos"][1]
            vv = p["vel"][0] ** 2 + p["vel"][1] ** 2
            if vv < 1:
                continue
            tc = (rx * p["vel"][0] + ry * p["vel"][1]) / vv
            if tc < 0.02 or tc > 0.45:
                continue
            cx, cy = p["pos"][0] + p["vel"][0] * tc - pos[0], p["pos"][1] + p["vel"][1] * tc - pos[1]
            cd = math.hypot(cx, cy)
            if cd < 3.8:
                l_ = max(0.3, cd)
                dvx += -cx / l_ * 0.5
                dvy += -cy / l_ * 0.5
        for snd in obs["sounds"]:
            if snd["kind"] != "laser_charge" or snd.get("age", 0) > 0.6:
                continue
            b_ = snd["bearing"]
            if any(abs(wrap(math.atan2(e["pos"][1] - pos[1], e["pos"][0] - pos[0]) - b_)) < 0.3 for e in enemies):
                continue        # we can see that charger: the aimed-laser dodge above handles it
            for sgn in (1, -1):
                py_ = b_ + sgn * math.pi / 2
                if self.W is None or self.W.clear_ahead(pos[0], pos[1], py_, 6) >= 4:
                    dvx += math.cos(py_) * 0.8
                    dvy += math.sin(py_) * 0.8
                    break
        dodge_boost = bool(dvx or dvy) and me["energy"] > 25
        if dvx or dvy:
            mv = [mv[0] * 0.5 + dvx * 1.5, mv[1] * 0.5 + dvy * 1.5]

        zapper = None
        if not any(w and w["kind"] == "lightning" for w in me["weapons"]):
            for e in enemies:
                if "lightning" in (e.get("weapons") or []) and e["_d"] < 50 and (zapper is None or e["_d"] < zapper["_d"]):
                    zapper = e
        if zapper is not None and not urgent:
            ex, ey = norm2((pos[0] - zapper["pos"][0], pos[1] - zapper["pos"][1]))
            mv = [mv[0] * 0.4 + ex * 1.2, mv[1] * 0.4 + ey * 1.2]

        # mortar dodge overrides
        esc = self.shell_threat(obs, me)
        if esc:
            mv = [esc[0] * 2 + mv[0] * 0.3, esc[1] * 2 + mv[1] * 0.3]
            urgent = True

        # never drift out of the zone: pull back in if near current edge
        dc = d2(pos, z["center"])
        if dc > z["radius"] - 15:
            ix, iy = norm2((z["center"][0] - pos[0], z["center"][1] - pos[1]))
            mv = [mv[0] + ix * 2, mv[1] + iy * 2]

        mvn = math.hypot(*mv)
        if mvn > 1:
            mv = [mv[0] / mvn, mv[1] / mvn]
        moving = math.hypot(*mv) > 0.3

        # stuck detection -> jump & jet over obstacles
        jump = False
        boost = urgent or (mech_hit and me["energy"] > 40) or (zapper is not None and me["energy"] > 30) or jink or dodge_boost
        hv = math.hypot(me["vel"][0], me["vel"][1])
        if moving and hv < 2.0 and me["grounded"]:
            self.stuck_t += 0.1
        else:
            self.stuck_t = max(0.0, self.stuck_t - 0.05)
        if self.stuck_t > 0.4:
            self.unstick_until = t + 1.6
            self.unstick_side = -self.unstick_side
            self.stuck_t = 0.0
        if t < self.unstick_until:
            if me["grounded"]:
                jump = True
            boost = me["energy"] > 5
            if me["energy"] < 15:
                # sidestep along the wall instead
                sx, sy = -mv[1] * self.unstick_side, mv[0] * self.unstick_side
                mv = [mv[0] * 0.5 + sx, mv[1] * 0.5 + sy]
        elif not me["grounded"] and moving and me["vel"][2] > -1 and t < self.unstick_until + 1.0:
            boost = me["energy"] > 5
        mag = math.hypot(*mv)
        # progress watchdog: wanting to travel for 3 s (outside fights) but barely moving -> jump/jet straight at it
        self.pos_hist.append((t, pos[0], pos[1]))
        self.pos_hist = [p for p in self.pos_hist if t - p[0] <= 3.0]
        travelling = mag > 0.3 and (target is None or hold or urgent) and not esc and not hiding
        if travelling:
            if self.want_since is None:
                self.want_since = t
        else:
            self.want_since = None
        if (travelling and self.want_since is not None and t - self.want_since >= 3.0 and t >= self.unstick_until
                and math.hypot(pos[0] - self.pos_hist[0][1], pos[1] - self.pos_hist[0][2]) < 6.0):
            self.unstick_until = t + 1.6
            self.want_since = t
            self.steer_lock = -99.0
        if self.W is not None and mag > 0.3 and not esc and t >= self.unstick_until:
            yaw0 = math.atan2(mv[1], mv[0])
            yaw1 = self.steer_dir(pos, yaw0, t)
            mv = [math.cos(yaw1) * mag, math.sin(yaw1) * mag]
        act["move"] = [round(mv[0], 3), round(mv[1], 3)]
        act["boost"] = bool(boost)
        act["jump"] = jump

        # ---------------- aim & fire
        fire = [False, False]
        gun = next((w for w in me["weapons"] if w and w["kind"] == "gun"), None)
        if practice and farm is None and (target is None or hold) and self.W is not None and hp_frac >= 0.7 and not recovering:
            mz = pos[2] + me["stats"].get("muzzle_height", 7)
            opp = None
            for c in obs["crates"]:
                dd = d2(pos, c["pos"])
                if dd < 120 and (opp is None or dd < opp[0]):
                    opp = (dd, {"id": c["id"], "kind": "crate", "pos": c["pos"], "vel": [0, 0, 0]})
            for n in npcs:
                dd = d2(pos, n["pos"])
                if dd < 110 and hp_frac > 0.5 and (opp is None or dd < opp[0]):
                    opp = (dd, dict(n))
            if opp is not None:
                op = opp[1]["pos"]
                if self.W.los((pos[0], pos[1], mz), (op[0], op[1], op[2] + 1.2), canopy=False, step=1.5):
                    farm = opp[1]
                    farm["_d"] = math.dist(op, pos)
        if farm is not None and (target is None or hold or (target["_d"] > 110 and target["id"] not in self.attackers)):
            target = farm
            hold = False
        if target is not None and hold:
            # watch them, don't give ourselves away
            dyaw, dpitch, _ = self.lead(me, target, 1e9)
            act["aim"] = [dyaw, dpitch]
            for i, w in enumerate(me["weapons"]):
                if w and w["kind"] == "laser" and (w.get("charge") or 0) > 0:
                    fire[i] = True      # keep holding a started charge rather than wasting it
        elif target is not None:
            g_speed = gun["stats"]["speed"] if gun else 252
            yaw, pitch, h = self.lead(me, target, g_speed)
            v_ = target.get("vel") or [0, 0, 0]
            fut = dict(target, pos=[target["pos"][0] + v_[0] * 0.12, target["pos"][1] + v_[1] * 0.12, target["pos"][2]])
            dyaw, dpitch, _ = self.lead(me, fut, 1e9)   # instant weapons: aim where it will be next tick
            aim = [yaw, pitch] if gun else [dyaw, dpitch]
            tol = math.atan2(2.2, max(h, 1)) + 0.02
            shoot_ok = target["_d"] < 190
            laser_busy = False
            for i, w in enumerate(me["weapons"]):
                if not w:
                    continue
                k, st = w["kind"], w["stats"]
                if k == "laser":
                    charge = w.get("charge", 0) or 0
                    if shoot_ok and h < st["range"] * 0.95:
                        aim = [dyaw, dpitch]
                        laser_busy = True
                        lerr = abs(wrap(me["aim"][0] - dyaw))
                        if charge < st["charge"] - 0.02:
                            fire[i] = w["cooldown"] <= 0.05 or charge > 0
                            if charge >= st.get("min_charge", 0.15) and target.get("kind") == "mech" and target.get("hp") is not None:
                                frac = st.get("min_frac", 0.35) + (1 - st.get("min_frac", 0.35)) * charge / st["charge"]
                                dmg = st["damage"] * frac * (1 + 0.04 * min(me.get("cubes") or 0, 10))
                                if "mortar" in (target.get("weapons") or []):
                                    dmg *= 1.6
                                bt_ = math.atan2(pos[1] - target["pos"][1], pos[0] - target["pos"][0])
                                if abs(wrap(target["aim"][0] - bt_)) > math.pi / 2:
                                    dmg *= 1.5           # its back is to us: rear hit
                                if dmg * 0.8 >= target["hp"] and lerr < tol * 0.9 and target.get("_los", True):
                                    fire[i] = False      # lethal already: release now
                        else:
                            fire[i] = lerr > tol * 1.5  # hold at full charge until on target, then release
                            if self.W is not None and target.get("kind") == "mech" and not self.shot_clear(me, target):
                                fire[i] = True          # behind cover: keep the charge until we have a clear shot
                    elif charge > 0:
                        fire[i] = False
            for i, w in enumerate(me["weapons"]):
                if not w:
                    continue
                k, st = w["kind"], w["stats"]
                if k == "gun":
                    yerr = abs(wrap(me["aim"][0] - yaw))
                    if shoot_ok and h < st["range"] * 0.9 and yerr < tol and w.get("reload", 0) == 0 and w.get("ammo", 1) > 0:
                        fire[i] = True
                elif k == "lightning":
                    yerr = abs(wrap(me["aim"][0] - dyaw))
                    if h < st["range"] - 2 and yerr < math.radians(st["cone_deg"]) * 0.8 and me["energy"] > st.get("energy", 3) + 5:
                        fire[i] = True
                        if not laser_busy:
                            aim = [dyaw, dpitch]
                elif k == "mortar" and not laser_busy:
                    if shoot_ok and 30 < h < 220 and w["cooldown"] <= 0:
                        mz = pos[2] + me["stats"].get("height", 9) + 0.5
                        mp = self.mortar_pitch(h, target["pos"][2] - mz, st["speed"], st["gravity"])
                        if mp is not None:
                            aim = [yaw, mp]
                            if abs(wrap(me["aim"][0] - yaw)) < 0.05 and abs(me["aim"][1] - mp) < 0.05:
                                fire[i] = True
            act["aim"] = aim
        else:
            # face the last attacker, else scan toward where enemies come from
            if t - self.last_dmg_t < 4 and self.last_dmg_bearing is not None:
                act["aim"] = [self.last_dmg_bearing, 0.0]
            else:
                heard = [s for s in obs["sounds"] if s["kind"] in ("gun", "laser_charge", "lightning", "step", "boost", "jet", "mortar") and s["dist"] != "far"]
                if heard:
                    act["aim"] = [heard[0]["bearing"], 0.0]
                else:
                    # spin the torso (4 rad/s = a full look-around every ~1.6 s; sightings are kept 5 s)
                    act["aim"] = [wrap(me["aim"][0] + 2.0), 0.0]
        act["fire"] = fire

        # reload when calm
        if gun and gun["kind"] == "gun" and gun.get("reload", 0) == 0:
            exposed_ = any(e["_los"] and e["_d"] < 130 for e in enemies)
            if gun.get("ammo", 12) == 0 or ((target is None or not exposed_) and gun.get("ammo", 12) < gun["stats"].get("mag", 12) * 0.5):
                act["reload"] = True

        self.dbg = {"hold": hold, "farm": farm_goal is not None, "farm_shoot": farm is not None and target is farm and any(fire), "guer": guerilla, "hide": hiding, "loot": loot_run is not None, "practice": practice, "early": early, "clear": clear, "retreat": self.retreat, "flee": flee, "recover": recovering}
        mf.draw.label(f"hp {me['hp']:.0f} {'HOLD' if hold else ''}{'FARM' if farm_goal is not None else ''}{'GUER' if guerilla else ''}")
        return act


if __name__ == "__main__":
    mf.run(Bot)
