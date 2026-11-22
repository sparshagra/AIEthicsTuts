# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Sparsh (Team 4141). See LICENSE and NOTICE.
"""Static world model for the mechfight map: walkable surface, building heights, canopy, line of sight.

Grids are 1 m cells aligned with v2/terrain.i16 (index [j, i] -> x = minX + i, y = minY + j).
Uses numpy only. Built once at startup (~1 s).
"""
import json
import math
import os

import numpy as np

CANOPY_LN = math.log(1 / 0.25)  # overwritten from rules


def _poly_mask(ring, x0, y0, nx, ny):
    """Boolean mask over the bbox of `ring` (flat list) for cell centres inside it. Returns (i0, j0, mask)."""
    xs = np.asarray(ring[0::2], dtype=np.float64)
    ys = np.asarray(ring[1::2], dtype=np.float64)
    i0 = max(int(math.floor(xs.min() - x0)), 0)
    i1 = min(int(math.ceil(xs.max() - x0)) + 1, nx)
    j0 = max(int(math.floor(ys.min() - y0)), 0)
    j1 = min(int(math.ceil(ys.max() - y0)) + 1, ny)
    if i1 <= i0 or j1 <= j0:
        return i0, j0, np.zeros((0, 0), bool)
    cx = x0 + np.arange(i0, i1, dtype=np.float64)
    cy = y0 + np.arange(j0, j1, dtype=np.float64)
    X, Y = np.meshgrid(cx, cy)
    inside = np.zeros(X.shape, bool)
    n = len(xs)
    for k in range(n):
        xa, ya = xs[k], ys[k]
        xb, yb = xs[(k + 1) % n], ys[(k + 1) % n]
        if ya == yb:
            continue
        cond = (ya > Y) != (yb > Y)
        xint = xa + (Y - ya) * (xb - xa) / (yb - ya)
        inside ^= cond & (X < xint)
    return i0, j0, inside


class World:
    def __init__(self, map_dir):
        with open(os.path.join(map_dir, "v2", "world.json")) as f:
            w = json.load(f)
        t = w["terrain"]
        self.x0, self.y0, self.nx, self.ny = t["minX"], t["minY"], t["nx"], t["ny"]
        raw = np.fromfile(os.path.join(map_dir, "v2", t["file"]), dtype="<i2")
        self.terrain = (raw.reshape(self.ny, self.nx).astype(np.float32) * t.get("scale", 0.01))
        self.bounds = w["bounds"]
        rules = w.get("rules", {})
        self.canopy_density = rules.get("canopyDensity", 0.35)
        self.canopy_block_len = math.log(1 / rules.get("canopyBlockT", 0.25)) / self.canopy_density

        ny, nx = self.ny, self.nx
        self.btop = np.full((ny, nx), -1e4, np.float32)    # solid top (buildings/stadium)
        self.bbase = np.full((ny, nx), 1e4, np.float32)
        self.bridge_top = np.full((ny, nx), -1e4, np.float32)
        self.bridge_base = np.full((ny, nx), 1e4, np.float32)
        for p in w["prisms"]:
            i0, j0, m = _poly_mask(p["ring"], self.x0, self.y0, nx, ny)
            if m.size == 0:
                continue
            for h in p.get("holes") or []:
                hi0, hj0, hm = _poly_mask(h, self.x0, self.y0, nx, ny)
                if hm.size:
                    sub = m[hj0 - j0: hj0 - j0 + hm.shape[0], hi0 - i0: hi0 - i0 + hm.shape[1]]
                    hm = hm[: sub.shape[0], : sub.shape[1]]
                    sub &= ~hm
            sl = (slice(j0, j0 + m.shape[0]), slice(i0, i0 + m.shape[1]))
            if p["kind"] == "bridge":
                self.bridge_top[sl] = np.where(m, np.maximum(self.bridge_top[sl], p["top"]), self.bridge_top[sl])
                self.bridge_base[sl] = np.where(m, np.minimum(self.bridge_base[sl], p["base"]), self.bridge_base[sl])
            else:
                self.btop[sl] = np.where(m, np.maximum(self.btop[sl], p["top"]), self.btop[sl])
                self.bbase[sl] = np.where(m, np.minimum(self.bbase[sl], p["base"]), self.bbase[sl])
        self.is_bldg = self.btop > -1e3
        # walkable surface ignoring bridges: ground or roof
        self.surface = np.where(self.is_bldg, np.maximum(self.btop, self.terrain), self.terrain).astype(np.float32)

        # canopy: crown z-range per cell; trunks
        self.crown_lo = np.full((ny, nx), 1e4, np.float32)
        self.crown_hi = np.full((ny, nx), -1e4, np.float32)
        self.trunk_hi = np.full((ny, nx), -1e4, np.float32)
        tr = w["trees"]
        f = tr["fields"]
        d = tr["data"]
        nf = len(f)
        ix = {k: f.index(k) for k in f}
        self.trees = []
        for k in range(0, len(d), nf):
            row = d[k:k + nf]
            x, y, z = row[ix["x"]], row[ix["y"]], row[ix["z"]]
            hgt, cr, cb = row[ix["height"]], row[ix["crownR"]], row[ix["crownBase"]]
            cx = row[ix["crownX"]] if "crownX" in ix else x
            cy = row[ix["crownY"]] if "crownY" in ix else y
            lo, hi = z + cb, z + hgt
            self.trees.append((cx, cy, cr, lo, hi))
            i0 = max(int(math.floor(cx - cr - self.x0)), 0)
            i1 = min(int(math.ceil(cx + cr - self.x0)) + 1, nx)
            j0 = max(int(math.floor(cy - cr - self.y0)), 0)
            j1 = min(int(math.ceil(cy + cr - self.y0)) + 1, ny)
            if i1 > i0 and j1 > j0:
                X, Y = np.meshgrid(self.x0 + np.arange(i0, i1), self.y0 + np.arange(j0, j1))
                m = (X - cx) ** 2 + (Y - cy) ** 2 <= cr * cr
                sl = (slice(j0, j1), slice(i0, i1))
                self.crown_lo[sl] = np.where(m, np.minimum(self.crown_lo[sl], lo), self.crown_lo[sl])
                self.crown_hi[sl] = np.where(m, np.maximum(self.crown_hi[sl], hi), self.crown_hi[sl])
            ti, tj = int(round(x - self.x0)), int(round(y - self.y0))
            if 0 <= ti < nx and 0 <= tj < ny:
                self.trunk_hi[tj, ti] = max(self.trunk_hi[tj, ti], lo)
        self.has_crown = self.crown_hi > -1e3
        self.trees_np = np.asarray(self.trees, np.float32) if self.trees else np.zeros((0, 5), np.float32)
        # ground-level obstacle mask (buildings taller than a step above local terrain), dilated by ~mech radius
        tall = self.is_bldg & (self.btop > self.terrain + 1.5) & (self.bbase < self.terrain + 9)
        blk = tall.copy()
        for _ in range(3):
            g = blk.copy()
            g[1:, :] |= blk[:-1, :]
            g[:-1, :] |= blk[1:, :]
            g[:, 1:] |= blk[:, :-1]
            g[:, :-1] |= blk[:, 1:]
            blk = g
        # steep terrain steps also block
        dzx = np.abs(np.diff(self.terrain, axis=1)) > 1.2
        dzy = np.abs(np.diff(self.terrain, axis=0)) > 1.2
        blk[:, 1:] |= dzx
        blk[1:, :] |= dzy
        b = self.bounds
        X = self.x0 + np.arange(self.nx)
        Y = self.y0 + np.arange(self.ny)
        blk[:, (X < b["minX"] + 4) | (X > b["maxX"] - 4)] = True
        blk[(Y < b["minY"] + 4) | (Y > b["maxY"] - 4), :] = True
        self.blocked = blk
        # canopy cover score per cell (how much crown is overhead-ish around eye height), for hiding
        self.cover = self.has_crown.astype(np.float32)

    # ------------------------------------------------------------------ queries
    def idx(self, x, y):
        i = int(round(x - self.x0))
        j = int(round(y - self.y0))
        return min(max(j, 0), self.ny - 1), min(max(i, 0), self.nx - 1)

    def ground(self, x, y):
        """Bilinear terrain height."""
        fx, fy = x - self.x0, y - self.y0
        i, j = int(math.floor(fx)), int(math.floor(fy))
        i = min(max(i, 0), self.nx - 2)
        j = min(max(j, 0), self.ny - 2)
        ax, ay = fx - i, fy - j
        T = self.terrain
        return float((T[j, i] * (1 - ax) + T[j, i + 1] * ax) * (1 - ay) + (T[j + 1, i] * (1 - ax) + T[j + 1, i + 1] * ax) * ay)

    def surface_at(self, x, y):
        j, i = self.idx(x, y)
        if self.is_bldg[j, i]:
            return max(float(self.btop[j, i]), self.ground(x, y))
        return self.ground(x, y)

    def in_bounds(self, x, y, margin=0.0):
        b = self.bounds
        return b["minX"] + margin <= x <= b["maxX"] - margin and b["minY"] + margin <= y <= b["maxY"] - margin

    def _samples(self, a, b, step=1.0):
        n = max(2, int(math.dist(a, b) / step) + 1)
        tt = np.linspace(0.0, 1.0, n, dtype=np.float32)[1:-1]
        xs = a[0] + (b[0] - a[0]) * tt
        ys = a[1] + (b[1] - a[1]) * tt
        zs = a[2] + (b[2] - a[2]) * tt
        ii = np.clip(np.rint(xs - self.x0).astype(np.int32), 0, self.nx - 1)
        jj = np.clip(np.rint(ys - self.y0).astype(np.int32), 0, self.ny - 1)
        return zs, jj, ii, (math.dist(a, b) / max(n - 1, 1))

    def los(self, a, b, canopy=True, step=1.0):
        """True if the segment a->b (3D points) is clear of buildings/terrain (and dense canopy/trunks)."""
        zs, jj, ii, ds = self._samples(a, b, step)
        if zs.size == 0:
            return True
        bt = self.btop[jj, ii]
        bb = self.bbase[jj, ii]
        if np.any((zs < bt) & (zs > bb)):
            return False
        if np.any(zs < self.terrain[jj, ii] - 0.3):
            return False
        brt = self.bridge_top[jj, ii]
        brb = self.bridge_base[jj, ii]
        if np.any((zs < brt) & (zs > brb)):
            return False
        if canopy:
            inside = (zs >= self.crown_lo[jj, ii]) & (zs <= self.crown_hi[jj, ii])
            if inside.sum() * ds >= self.canopy_block_len:
                return False
            if np.any(zs < self.trunk_hi[jj, ii]):
                return False
        return True

    def los_many(self, A, B, canopy=True, step=1.5):
        """Vectorised LoS for K segments. A, B: (K,3) arrays. Returns bool (K,)."""
        A = np.asarray(A, np.float32)
        B = np.asarray(B, np.float32)
        K = A.shape[0]
        if K == 0:
            return np.zeros(0, bool)
        L = np.linalg.norm(B - A, axis=1)
        n = int(min(max(L.max() / step, 2), 400))
        tt = np.linspace(0, 1, n + 2, dtype=np.float32)[1:-1]
        P = A[:, None, :] + (B - A)[:, None, :] * tt[None, :, None]
        ii = np.clip(np.rint(P[..., 0] - self.x0).astype(np.int32), 0, self.nx - 1)
        jj = np.clip(np.rint(P[..., 1] - self.y0).astype(np.int32), 0, self.ny - 1)
        zs = P[..., 2]
        blk = ((zs < self.btop[jj, ii]) & (zs > self.bbase[jj, ii])).any(1)
        blk |= (zs < self.terrain[jj, ii] - 0.3).any(1)
        blk |= ((zs < self.bridge_top[jj, ii]) & (zs > self.bridge_base[jj, ii])).any(1)
        if canopy:
            ds = L / (n + 1)
            inside = ((zs >= self.crown_lo[jj, ii]) & (zs <= self.crown_hi[jj, ii])).sum(1) * ds
            blk |= inside >= self.canopy_block_len
            blk |= (zs < self.trunk_hi[jj, ii]).any(1)
        return ~blk

    def first_hit(self, a, d, max_len, step=1.0):
        """March from a along unit d; return distance to first building/terrain hit or max_len."""
        n = int(max_len / step)
        tt = np.arange(1, n + 1, dtype=np.float32) * step
        xs = a[0] + d[0] * tt
        ys = a[1] + d[1] * tt
        zs = a[2] + d[2] * tt
        ii = np.clip(np.rint(xs - self.x0).astype(np.int32), 0, self.nx - 1)
        jj = np.clip(np.rint(ys - self.y0).astype(np.int32), 0, self.ny - 1)
        hit = ((zs < self.btop[jj, ii]) & (zs > self.bbase[jj, ii])) | (zs < self.terrain[jj, ii] - 0.2)
        hit |= (zs < self.bridge_top[jj, ii]) & (zs > self.bridge_base[jj, ii])
        k = np.flatnonzero(hit)
        return float(tt[k[0]]) if k.size else max_len


    def clear_ahead(self, x, y, yaw, dist, step=2.0):
        """Distance we can walk at ground level along yaw before hitting a blocked cell."""
        c, s = math.cos(yaw), math.sin(yaw)
        d = step
        while d <= dist:
            px, py = x + c * d, y + s * d
            j, i = self.idx(px, py)
            if self.blocked[j, i]:
                return d - step
            d += step
        return dist

    def on_roof(self, x, y, z):
        j, i = self.idx(x, y)
        return bool(self.is_bldg[j, i] and z > self.btop[j, i] - 1.5 and self.btop[j, i] > self.terrain[j, i] + 1.5)

    def steer(self, x, y, z, goal_yaw, look=18.0, prefer=0):
        """Pick a heading near goal_yaw that is clear for `look` metres. Returns (yaw, clear, side)."""
        if self.on_roof(x, y, z):
            return goal_yaw, True, prefer
        j, i = self.idx(x, y)
        if self.blocked[j, i]:
            # we're inside the dilated margin: allow any heading leaving it
            look = min(look, 6.0)
        best = None
        order = [0]
        for k in range(1, 10):
            a = k * math.radians(18)
            order += ([a, -a] if prefer >= 0 else [-a, a])
        for off in order:
            yaw = goal_yaw + off
            cl = self.clear_ahead(x, y, yaw, look)
            if cl >= look:
                side = 0 if off == 0 else (1 if off > 0 else -1)
                return yaw, True, side
            if best is None or cl > best[0]:
                best = (cl, yaw, 0 if off == 0 else (1 if off > 0 else -1))
        return best[1], False, best[2]


_WORLD = {}


def load(map_dir):
    if map_dir not in _WORLD:
        _WORLD[map_dir] = World(map_dir)
    return _WORLD[map_dir]
