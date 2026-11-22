# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Sparsh (Team 4141). See LICENSE and NOTICE.
"""Entry point: practice_brain plays the house squad; once a free-for-all is detected, ffa_brain plays.

Both brains see every observation, so the free-for-all brain's memory and detection stay current.
"""
import importlib.util
import math
import os

import mechfight as mf

HERE = os.path.dirname(os.path.abspath(__file__))
TRACE = bool(os.environ.get("BOT_TRACE"))


def _load(name, filename):
    spec = importlib.util.spec_from_file_location(name, os.path.join(HERE, filename))
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


_ffa = _load("ffa_brain", "ffa_brain.py")
_prac = _load("practice_brain", "practice_brain.py")


class Bot:
    name = "toaster"

    def __init__(self, hello):
        self.ffa = _ffa.Bot(hello)
        self.prac = _prac.JBrain(hello, self.ffa.W)
        self.ffa_logged = False

    def act(self, obs):
        a_ffa = self.ffa.act(obs)          # always run: keeps detection + memory current
        if TRACE and obs["tick"] % 10 == 0:
            me = obs["self"]
            ms = sorted(round(math.dist(me["pos"], m["pos"])) for m in obs["mechs"] if m["kind"] == "mech")
            hits = [ev.get("by") for ev in obs["events"] if ev["type"] == "damaged"]
            mf.log(f"t={obs['t']} hp={me['hp']:.0f} cubes={me.get('cubes')} w={[w and w['kind'] for w in me['weapons']]}"
                   f" alive={obs['alive']} near={ms[:4]} hits={hits} dbg={getattr(self.ffa, 'dbg', {})}")
        if self.ffa.not_practice:
            if not self.ffa_logged:
                self.ffa_logged = True
                mf.log(f"free-for-all detected t={obs['t']} alive={obs.get('alive')}")
                # its stuck/progress trackers compared its own wishes with the practice brain's movement
                f = self.ffa
                f.stuck_t, f.unstick_until, f.want_since, f.pos_hist, f.steer_lock = 0.0, -1.0, None, [], -99.0
                f.anchor = None
            return a_ffa
        a = self.prac.act(obs)
        a["tick"] = obs["tick"]
        return a


if __name__ == "__main__":
    mf.run(Bot)
