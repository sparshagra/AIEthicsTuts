package main

import (
	"fmt"
	"math"
	"testing"
)

// Walks a point mech with goTo between random reachable pairs on the real map grid.
// go test -run TestNav -v
func TestNav(t *testing.T) {
	w, err := LoadWorld("arena")
	if err != nil {
		t.Skip("no arena")
	}
	g := NewGrid(w)
	rng := newXorshift(7)
	var pairs [][4]float64
	for len(pairs) < 300 {
		ax, ay := w.bminx+rng.random()*(w.bmaxx-w.bminx), w.bminy+rng.random()*(w.bmaxy-w.bminy)
		bx, by := w.bminx+rng.random()*(w.bmaxx-w.bminx), w.bminy+rng.random()*(w.bmaxy-w.bminy)
		d := hyp(bx-ax, by-ay)
		if d < 60 || d > 250 || !g.reachable(ax, ay) || !g.reachable(bx, by) {
			continue
		}
		pairs = append(pairs, [4]float64{ax, ay, bx, by})
	}
	run := func(name string) {
		var tot, fails, revs float64
		for _, p := range pairs {
			b := &Agent{world: w, grid: g, routeAt: -99}
			me := &Self{Pos: []float64{p[0], p[1], g.walkAt(p[0], p[1])}, Stats: map[string]float64{}}
			arrived := false
			var pmx, pmy float64
			for i := 0; i < 900; i++ {
				b.now = float64(i) * 0.1
				x, y := me.Pos[0], me.Pos[1]
				if hyp(p[2]-x, p[3]-y) < 3 {
					tot += b.now
					arrived = true
					break
				}
				mx, my := b.goTo(me, p[2], p[3])
				if mx*pmx+my*pmy < -0.5 {
					revs++
				}
				pmx, pmy = mx, my
				step := 11.7 * 0.1
				moved := false
				for _, c := range [][2]float64{{mx, my}, {mx, 0}, {0, my}} {
					nx, ny := x+c[0]*step, y+c[1]*step
					if (c[0] != 0 || c[1] != 0) && g.free(nx, ny) && g.stepOK(g.cell(x, y), g.cell(nx, ny), true) {
						me.Pos[0], me.Pos[1], me.Pos[2] = nx, ny, g.walkAt(nx, ny)
						moved = true
						break
					}
				}
				_ = moved
			}
			if !arrived {
				fails++
				tot += 90
			}
		}
		fmt.Printf("%s: mean time %.1f s, fails %.0f/%d, reversals %.0f\n", name, tot/float64(len(pairs)), fails, len(pairs), revs)
	}
	run(fmt.Sprintf("goTo routeRefresh=%.1f", navRefresh))
	old := navRefresh
	for _, r := range []float64{1e9} {
		navRefresh = r
		run(fmt.Sprintf("goTo routeRefresh=%.1f", r))
	}
	navRefresh = old
	_ = math.Pi
}

func TestNavZone(t *testing.T) {
	w, err := LoadWorld("arena")
	if err != nil {
		t.Skip("no arena")
	}
	g := NewGrid(w)
	rng := newXorshift(11)
	type cs struct{ sx, sy, cx, cy, r float64 }
	var cases []cs
	for len(cases) < 300 {
		cx, cy := w.bminx+rng.random()*(w.bmaxx-w.bminx), w.bminy+rng.random()*(w.bmaxy-w.bminy)
		r := 30 + rng.random()*120
		a := rng.random() * 2 * math.Pi
		d := r + 40 + rng.random()*120
		sx, sy := cx+math.Cos(a)*d, cy+math.Sin(a)*d
		if sx < w.bminx || sx > w.bmaxx || sy < w.bminy || sy > w.bmaxy || !g.reachable(sx, sy) || !g.reachable(cx, cy) {
			continue
		}
		cases = append(cases, cs{sx, sy, cx, cy, r})
	}
	for _, fix := range []float64{0, 1, 2} {
		armZoneFix, armFFAOnly = fix, false
		var tot, fails, revs float64
		for _, c := range cases {
			b := &Agent{world: w, grid: g, routeAt: -99}
			z := &Zone{Center: []float64{c.cx, c.cy}, Radius: 1e4, NextCenter: []float64{c.cx, c.cy}, NextRadius: c.r, ShrinkStart: 1e9, ShrinkEnd: 1e9 + 1}
			me := &Self{Pos: []float64{c.sx, c.sy, g.walkAt(c.sx, c.sy)}, Stats: map[string]float64{}}
			arrived := false
			var pmx, pmy float64
			for i := 0; i < 900; i++ {
				b.now = float64(i) * 0.1
				x, y := me.Pos[0], me.Pos[1]
				if rx, ry := b.zoneGoal(z, x, y); hyp(rx-x, ry-y) <= 2 {
					tot += b.now
					arrived = true
					break
				}
				zx, zy := b.holdZoneGoal(z, x, y)
				mx, my := b.goTo(me, zx, zy)
				if mx*pmx+my*pmy < -0.5 {
					revs++
				}
				pmx, pmy = mx, my
				step := 11.7 * 0.1
				for _, d := range [][2]float64{{mx, my}, {mx, 0}, {0, my}} {
					nx, ny := x+d[0]*step, y+d[1]*step
					if (d[0] != 0 || d[1] != 0) && g.free(nx, ny) && g.stepOK(g.cell(x, y), g.cell(nx, ny), true) {
						me.Pos[0], me.Pos[1], me.Pos[2] = nx, ny, g.walkAt(nx, ny)
						break
					}
				}
			}
			if !arrived {
				fails++
				tot += 90
			}
		}
		fmt.Printf("zonefix=%.0f: mean time %.1f s, fails %.0f/%d, reversals %.0f\n", fix, tot/float64(len(cases)), fails, len(cases), revs)
	}
	armZoneFix = 2
}
