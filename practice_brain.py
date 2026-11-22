# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Sparsh (Team 4141). See LICENSE and NOTICE.
# Design follows JishnuMadav/mech-royale (MIT License, Copyright (c) 2026 JishnuMadav), see THIRD_PARTY_LICENSES.
"""Practice brain following the design of JISHNU's "Oval Sentinel" (JishnuMadav/mech-royale, MIT, 5.81 official
practice), re-implemented on our own map model (world.py). Design credit: JishnuMadav/mech-royale (MIT License).

One coherent policy per tick:
- target: any mech we have a clear shot at (distance + 0.045 * HP, small stickiness, bonus under 300 HP);
  drones only when no mech is near; crates when nothing is near.
- goal: hold ground; go for a second weapon / cubes / an uncontested supply drop; cover while healing (60 % -> 90 %);
  the next circle when the zone forces it.
- movement: score 12 headings (walkable, zone, inertia, keep a weapon-specific distance from enemies,
  dodge projectiles), fight at a weapon-specific range with a slow strafe.
- firing: aim per weapon; shoot when the torso will be aligned after one tick of slewing; laser released when full
  (allowing for 2-decimal rounding) or when a partial charge already kills.
"""
import math

TAU = 2 * math.pi


def wrap(a):
    return (a + math.pi) % TAU - math.pi


def unit(x, y):
    n = math.hypot(x, y)
    return (x / n, y / n) if n > 1e-9 else (0.0, 0.0)


SALT = 1   # re-roll knob: 0 plays exactly as before; other values change only the strafe rhythm

UPGRADE_RANK = {"laser_damage": 115, "laser_charge": 110, "lightning_efficiency": 108, "lightning_range": 106,
                "plating": 100, "gun_mag": 98, "servos": 95, "armor": 94, "gun_damage": 88, "gun_rate": 86,
                "gun_velocity": 78, "reactor": 77, "jets": 70, "capacitor": 60}


class JBrain:
    def __init__(self, hello, W):
        self.cfg = hello["config"]
        self.W = W
        self.side = 1 if (hello.get("slot", 0) + SALT) % 2 else -1
        self.strafe_period = 2.2 * (1 + 0.04 * (SALT // 2))
        self.target_id = None
        self.arc_resting = False
        self.healing = False
        self.cover_goal = None
        self.cover_after = -1.0
        self.pickup_mem = {}
        self.cube_mem = {}
        self.last_threat = (-100.0, 0.0)
        self.prev_pos = None
        self.stuck = 0
        self.jet_until = -1.0
        self.progress_pos = None
        self.progress_at = 0.0
        self.boosting = False

    # ---------------------------------------------------------------- geometry
    def clear_shot(self, a, b):
        return self.W is None or self.W.los(a, b, canopy=False, step=1.5)

    def walkable(self, pos, yaw):
        return self.W is None or self.W.clear_ahead(pos[0], pos[1], yaw, 6.0) >= 4.0

    def find_cover(self, pos, mechs, zone):
        """Nearest reachable point that the closest mechs cannot see (buildings), inside the zone."""
        W = self.W
        if W is None or not mechs:
            return None
        threats = sorted(mechs, key=lambda m: math.dist(pos, m["pos"]))[:3]
        cx = sum(m["pos"][0] for m in threats) / len(threats)
        cy = sum(m["pos"][1] for m in threats) / len(threats)
        away = math.atan2(pos[1] - cy, pos[0] - cx)
        cands = []
        for r in (8, 14, 22, 32, 45):
            for k in range(12):
                a = away + (k - 6) * (math.pi / 6)
                x, y = pos[0] + r * math.cos(a), pos[1] + r * math.sin(a)
                if not W.in_bounds(x, y, 10) or math.dist((x, y), zone["center"]) > zone["radius"] - 8:
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
            for m in threats:
                A.append((m["pos"][0], m["pos"][1], m["pos"][2] + 8))
                B.append((x, y, gz + 5))
        vis = W.los_many(A, B, canopy=False, step=1.5)
        n = len(threats)
        best = None
        for k, (x, y, r) in enumerate(cands):
            seen = int(vis[n * k:n * k + n].sum())
            sc = seen * 100 + r
            if best is None or sc < best[0]:
                best = (sc, (x, y))
        return best[1] if best and best[0] < 100 else None

    # ---------------------------------------------------------------- aiming
    def aim_solution(self, me, target, kind, st):
        scale = 1 + 0.05 * (target.get("cubes") or 0)
        h = 4.5 * scale if target.get("kind") == "mech" else (1.75 if target.get("kind") == "npc" else 1.0)
        origin = [me["pos"][0], me["pos"][1], me["pos"][2] + me["stats"].get("muzzle_height", 7)]
        center = [target["pos"][0], target["pos"][1], target["pos"][2] + h]
        vel = target.get("vel") or [0, 0, 0]
        if kind == "gun":
            point = list(center)
            for _ in range(3):
                tt = math.dist(origin, point) / max(1.0, st["speed"])
                point = [center[i] + vel[i] * tt for i in range(3)]
        elif kind == "mortar":
            origin[2] = me["pos"][2] + me["stats"].get("height", 9) + 0.5
            point, pitch = list(center), None
            for _ in range(3):
                dist = math.dist(origin[:2], point[:2])
                dz = point[2] - origin[2]
                v, g = st["speed"], st["gravity"]
                disc = v ** 4 - g * (g * dist ** 2 + 2 * dz * v * v)
                if disc < 0 or dist < 12:
                    return None
                pitch = math.atan2(v * v - math.sqrt(disc), g * dist)
                flight = dist / max(1.0, v * math.cos(pitch))
                point = [center[i] + vel[i] * flight for i in range(3)]
            if not me["stats"].get("min_pitch", -0.7) <= pitch <= me["stats"].get("max_pitch", 1.3):
                return None
            return [math.atan2(point[1] - origin[1], point[0] - origin[0]), pitch], origin, point
        else:
            point = center
        dx, dy, dz = point[0] - origin[0], point[1] - origin[1], point[2] - origin[2]
        return [math.atan2(dy, dx), math.atan2(dz, math.hypot(dx, dy))], origin, point

    def laser_release(self, me, target, w):
        st = w["stats"]
        charge = w.get("charge") or 0
        if charge + 0.01 >= st["charge"]:
            return True
        if charge < st.get("min_charge", 0.15) or target.get("hp") is None:
            return False
        dmg = st["damage"] * (1 + 0.04 * min(10, me.get("cubes") or 0))
        dmg *= st.get("min_frac", 0.35) + (1 - st.get("min_frac", 0.35)) * charge / st["charge"]
        if target.get("kind") == "mech":
            dmg *= 0.76                       # unknown enemy armour: assume the worst
            if "mortar" in (target.get("weapons") or []):
                dmg *= 1 + st.get("cookoff", 0.6)
        return dmg >= target["hp"] + 15

    def aligned(self, me, aim, dist, radius=2.0):
        ye = max(0.0, abs(wrap(aim[0] - me["aim"][0])) - me["stats"].get("turn_rate", 4) * 0.05)
        pe = max(0.0, abs(aim[1] - me["aim"][1]) - me["stats"].get("pitch_rate", 3) * 0.05)
        return ye <= max(0.018, math.atan2(radius, max(1.0, dist))) and pe <= max(0.02, math.atan2(3, max(1.0, dist)))

    # ---------------------------------------------------------------- main
    def act(self, obs):
        me, zone, now = obs["self"], obs["zone"], obs["t"]
        pos, stats = me["pos"], me["stats"]
        x, y, z = pos
        hp = me["hp"] / max(1, me["max_hp"])
        for ev in obs["events"]:
            if ev["type"] == "damaged" and ev.get("bearing") is not None:
                self.last_threat = (now, ev["bearing"])
        weapons = me["weapons"]
        act = {"fire": [False, False], "boost": False, "jump": False}
        by_kind = {w["kind"]: (i, w) for i, w in enumerate(weapons) if w}
        for key, mem in (("pickups", self.pickup_mem), ("cubes", self.cube_mem)):
            seen = {it["id"] for it in obs[key]}
            for it in obs[key]:
                mem[it["id"]] = (now, it)
            for iid, (ts, it) in list(mem.items()):
                if now - ts > 12 or (iid not in seen and math.dist(pos, it["pos"]) < 5):
                    del mem[iid]
        pickups = [it for _, it in self.pickup_mem.values()]
        cubes = [it for _, it in self.cube_mem.values()]
        max_e = me.get("max_energy") or self.cfg["mech"].get("energy", 100)
        if self.arc_resting:
            self.arc_resting = me["energy"] < min(65, max_e * 0.65)
        elif me["energy"] < 20:
            self.arc_resting = True
        mechs = [m for m in obs["mechs"] if m["kind"] == "mech"]
        nearest = min((math.dist(pos, m["pos"]) for m in mechs), default=1e9)
        close_count = sum(1 for m in mechs if math.dist(pos, m["pos"]) < 100)
        if hp > 0.90:
            self.healing = False
            self.cover_goal = None
        elif hp < 0.60:
            self.healing = True
        recovering = self.healing and nearest > 55

        if obs.get("offer"):
            ch = obs["offer"]["choices"]
            ranks = dict(UPGRADE_RANK, nanites=96 if hp < 0.55 else 80)
            act["pick"] = max(range(len(ch)), key=lambda i: ranks.get(ch[i]["id"], 25))

        # second weapon early (laser preferred); swaps only for the matchup
        pick_cands = []
        for p in pickups:
            if p["kind"] in by_kind or abs(p["pos"][2] - z) > 4:
                continue
            d = math.dist(pos, p["pos"])
            wanted, replace = any(w is None for w in weapons), None
            if not wanted and "mortar" in by_kind and p["kind"] in ("laser", "lightning"):
                wanted, replace = True, by_kind["mortar"][0]
            if not wanted and p["kind"] == "laser" and "lightning" in by_kind and nearest > 45:
                wanted, replace = True, by_kind["lightning"][0]
            if wanted:
                pick_cands.append((d / (1.8 if p["kind"] == "laser" else 1.1 if p["kind"] == "lightning" else 0.8), p))
                if d <= self.cfg["loot"]["pickup_radius"]:
                    act["take"] = p["id"]
                    if replace is not None and (weapons[replace].get("charge") or 0) == 0:
                        act["drop"] = replace

        origin = [x, y, z + stats.get("muzzle_height", 7)]
        targets = []
        for m in obs["mechs"]:
            d = math.dist(pos, m["pos"])
            if m["kind"] == "npc" and (nearest < 90 or d > 65):
                continue
            top = m["pos"][2] + (4.5 * (1 + 0.05 * (m.get("cubes") or 0)) if m["kind"] == "mech" else 1.75)
            if not self.clear_shot(origin, [m["pos"][0], m["pos"][1], top]):
                continue
            v = d + (m.get("hp") or 1400) * 0.045
            if m["kind"] == "npc":
                v += 45
            if m["id"] == self.target_id:
                v -= 18
            if (m.get("hp") or 1400) < 300:
                v -= 40
            targets.append((v, m))
        if nearest > 90 and not recovering:
            for c in obs["crates"]:
                d = math.dist(pos, c["pos"])
                if d < 65 and abs(c["pos"][2] - z) < 6 and self.clear_shot(origin, [c["pos"][0], c["pos"][1], c["pos"][2] + 1]):
                    targets.append((d + 65, dict(c, kind="crate", vel=[0, 0, 0])))
        target = min(targets, key=lambda q: q[0])[1] if targets else None
        self.target_id = target["id"] if target else None

        # goal: hold ground unless something is worth the trip
        goal = [x, y]
        if not self.healing and nearest > 80 and (len(by_kind) < 2 or (now < 45 and (me.get("cubes") or 0) < 3)):
            goal = list(zone["next_center"])
        seeking_loot = False
        to_next = max(0.0, math.dist(pos[:2], zone["next_center"]) - zone["next_radius"] + 12)
        urgent = not me["in_zone"] or (to_next > 0 and now + to_next / stats.get("walk_speed", 11.7) + 8 >= zone["shrink_end"])
        if urgent or zone["next_radius"] <= 30:
            goal = list(zone["next_center"])
        if not urgent and zone["next_radius"] > 30:
            if pick_cands and min(pick_cands, key=lambda q: q[0])[0] < 80 and nearest > 35:
                goal = list(min(pick_cands, key=lambda q: q[0])[1]["pos"][:2])
                seeking_loot = True
            elif cubes and nearest > 40 and (me.get("cubes") or 0) < 10:
                cb = min(cubes, key=lambda c: math.dist(pos, c["pos"]))
                if math.dist(pos, cb["pos"]) < (140 if nearest > 80 else 80) and abs(cb["pos"][2] - z) < 4:
                    goal = list(cb["pos"][:2])
                    seeking_loot = True
            elif target and target["kind"] != "mech":
                goal = list(target["pos"][:2])
        if self.healing and mechs and now >= self.cover_after:
            self.cover_goal = self.find_cover(pos, mechs, zone)
            self.cover_after = now + 2
        seeking_cover = self.healing and self.cover_goal is not None and not urgent
        if seeking_cover:
            goal = list(self.cover_goal)
        supply = None
        if hp < 0.85 or (me.get("cubes") or 0) < 10:
            opts = []
            for dr in obs.get("drops", []):
                d = math.dist(pos, dr["pos"])
                if d > (130 if hp < 0.7 else 85) or dr.get("holder") not in (None, me.get("id")):
                    continue
                if dr["lands_in"] > d / stats.get("walk_speed", 11.7) + 5:
                    continue
                if math.dist(dr["pos"][:2], zone["next_center"]) > zone["next_radius"] - 8:
                    continue
                opp = min((math.dist(dr["pos"], e["pos"]) for e in mechs), default=1e9)
                if opp < 55 and not (dr.get("holder") == me.get("id") and dr["capture_s"] - dr.get("progress", 0) < 1):
                    continue
                opts.append((d, dr))
            if opts:
                supply = min(opts, key=lambda q: q[0])[1]
                goal = list(supply["pos"][:2])
                seeking_loot, seeking_cover = True, False

        direction = unit(goal[0] - x, goal[1] - y)
        if target and target["kind"] == "mech" and not urgent and not seeking_loot and not seeking_cover and zone["next_radius"] > 20:
            ex, ey = unit(target["pos"][0] - x, target["pos"][1] - y)
            d = math.dist(pos, target["pos"])
            pref = (60 if self.arc_resting else 35) if "lightning" in by_kind else 90 if "laser" in by_kind else 60
            if "lightning" in (target.get("weapons") or []) and "laser" in by_kind:
                pref = 110
            if hp < 0.35 or close_count >= 3:
                pref += 40
            radial = max(-1.3, min(1.2, (d - pref) / 25))
            strafe = self.side * (1 if int(now / self.strafe_period) % 2 == 0 else -1)
            direction = unit(ex * radial - ey * strafe, ey * radial + ex * strafe)
        if math.dist(pos[:2], goal) < 2 and target is None:
            direction = (0.0, 0.0)
        if supply and math.dist(pos[:2], supply["pos"][:2]) < supply["radius"] - 3:
            direction = (0.0, 0.0)
        self.navigate(act, obs, direction)

        act["aim"] = [wrap(me["aim"][0] + 0.3), 0.0]
        if now - self.last_threat[0] < 2:
            act["aim"] = [self.last_threat[1], 0.0]
        elif mechs:
            w_ = min(mechs, key=lambda e: math.dist(pos, e["pos"]))
            act["aim"] = [math.atan2(w_["pos"][1] - y, w_["pos"][0] - x), 0.0]

        if target:
            d = math.dist(pos, target["pos"])
            chosen = by_kind.get("gun")
            laser, mortar = by_kind.get("laser"), by_kind.get("mortar")
            if laser and self.laser_release(me, target, laser[1]):
                chosen = laser
            elif mortar and mortar[1]["cooldown"] <= 0.05 and d > 25:
                chosen = mortar
            elif not chosen:
                chosen = next(iter(by_kind.values()), None)
            if chosen:
                slot, w = chosen
                sol = self.aim_solution(me, target, w["kind"], w["stats"])
                if sol:
                    aim, muzzle, point = sol
                    act["aim"] = aim
                    ok = self.aligned(me, aim, d)
                    can_fight = not (seeking_cover and math.dist(pos[:2], goal) < 8 and target["kind"] != "mech")
                    if w["kind"] == "gun":
                        act["fire"][slot] = bool(can_fight and ok and d < w["stats"]["range"] and (w.get("ammo") or 0) > 0
                                                 and self.clear_shot(muzzle, point))
                    elif w["kind"] == "mortar":
                        act["fire"][slot] = bool(can_fight and ok)
                    if laser and d < laser[1]["stats"]["range"] and can_fight:
                        charged = self.laser_release(me, target, laser[1])
                        act["fire"][laser[0]] = bool(not charged or not (w["kind"] == "laser" and ok))
                    lt = by_kind.get("lightning")
                    if lt:
                        ws = lt[1]["stats"]
                        yaw = math.atan2(target["pos"][1] - y, target["pos"][0] - x)
                        act["fire"][lt[0]] = bool(can_fight and not self.arc_resting and d < ws["range"]
                                                  and me["energy"] >= ws.get("energy", 3) + 8
                                                  and abs(wrap(yaw - me["aim"][0])) < math.radians(ws["cone_deg"]) * 0.85)
        if "gun" in by_kind:
            g = by_kind["gun"][1]
            act["reload"] = bool((g.get("ammo") or 0) == 0 or (target is None and nearest > 100 and (g.get("ammo") or 0) <= g["stats"].get("mag", 12) - 3))
        if self.healing and target is None and nearest > 55:
            act["fire"] = [False, False]
        return act

    def navigate(self, act, obs, direction):
        me, zone, now = obs["self"], obs["zone"], obs["t"]
        pos, stats = me["pos"], me["stats"]
        yaw = math.atan2(direction[1], direction[0])
        kinds = {w["kind"] for w in me["weapons"] if w}
        enemies = sorted((e for e in obs["mechs"] if e["kind"] == "mech" and (e.get("hp") or 1400) > 150),
                         key=lambda e: math.dist(pos, e["pos"]))[:4]
        sep = 30 if "lightning" in kinds and not self.arc_resting else 85 if "laser" in kinds else 55
        if self.healing:
            sep += 25
        walk = stats.get("walk_speed", 11.7)
        radius, height = stats.get("radius", 2.5), stats.get("height", 9)
        vel = me.get("vel") or [0, 0, 0]
        cands = []
        for off in (0, .35, -.35, .7, -.7, 1.2, -1.2, 1.7, -1.7, 2.3, -2.3, math.pi):
            dy_ = yaw + off
            d = (math.cos(dy_), math.sin(dy_))
            if not self.walkable(pos, dy_):
                continue
            fut = [pos[0] + d[0] * 10, pos[1] + d[1] * 10]
            sc = math.cos(off) - max(0.0, math.dist(fut, zone["center"]) - max(0.0, zone["radius"] - 5)) * 0.3
            sc += 0.15 * (d[0] * vel[0] + d[1] * vel[1]) / walk
            for e in enemies:
                dist = math.dist(fut, e["pos"][:2])
                sc -= 8 * max(0.0, 1 - dist / sep) ** 2
            for pr in obs["projectiles"][:30]:
                rel = [pr["pos"][i] - pos[i] for i in range(2)]
                rv = [pr["vel"][i] - d[i] * walk for i in range(2)]
                sp2 = rv[0] ** 2 + rv[1] ** 2
                tt = -(rel[0] * rv[0] + rel[1] * rv[1]) / max(1.0, sp2)
                if 0 < tt < 0.6:
                    gap = math.hypot(rel[0] + rv[0] * tt, rel[1] + rv[1] * tt)
                    pz = pr["pos"][2] + pr["vel"][2] * tt
                    if gap < radius + 2 and pos[2] <= pz <= pos[2] + height:
                        sc -= (radius + 2 - gap) * 0.35
            cands.append((sc, d))
        act["move"] = list(max(cands, key=lambda q: q[0])[1]) if cands else list(direction)
        if direction == (0.0, 0.0):
            act["move"] = [0.0, 0.0]
        if self.prev_pos is not None and math.dist(pos, self.prev_pos) < 0.12 and direction != (0.0, 0.0):
            self.stuck += 1
        else:
            self.stuck = 0
        self.prev_pos = list(pos)
        if self.progress_pos is None or direction == (0.0, 0.0):
            self.progress_pos, self.progress_at = list(pos), now
        elif now - self.progress_at >= 3:
            if math.dist(pos, self.progress_pos) < 4 and me["grounded"] and me["energy"] > 35:
                act["jump"] = True                      # dithering at a corner: go over it
                self.jet_until = now + 4
            self.progress_pos, self.progress_at = list(pos), now
        if (self.stuck > 10 or not cands) and me["grounded"] and me["energy"] > 60:
            act["jump"] = True
            self.jet_until = now + 4
        nearest = min((math.dist(pos, e["pos"]) for e in enemies), default=1e9)
        danger = not me["in_zone"] or me["hp"] < me["max_hp"] * 0.55 or ("laser" in kinds and nearest < 65)
        if self.boosting:
            self.boosting = danger and me["energy"] > 20
        else:
            self.boosting = danger and me["energy"] > 65
        act["boost"] = bool(self.boosting or (now < self.jet_until and me["energy"] > 10))
