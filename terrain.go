// Map geometry for the pilot agent. This file loads the level (prisms + a
// heightfield), bakes it into a walkability grid, and answers the spatial
// questions the brain keeps asking: can I stand here, can I see there, and
// what's the cheapest path to a goal.
//
// Author: Sparsh (Team 4141). Copyright (c) 2026, MIT licensed, see LICENSE and NOTICE.
package main

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
)

// span is a solid vertical interval [lo, hi] within a single map cell.
type span struct{ lo, hi float64 }

type edge struct{ x1, y1, x2, y2 float64 }

type prismJSON struct {
	Base  float64     `json:"base"`
	Top   float64     `json:"top"`
	Ring  []float64   `json:"ring"`
	Holes [][]float64 `json:"holes"`
}

// World holds the static map: a terrain height field plus extruded building prisms
// rasterised onto a 2-unit grid of solid spans.
type World struct {
	tminx, tminy, tstep float64
	tnx, tny            int
	tscale              float64
	th                  []int16

	bminx, bmaxx, bminy, bmaxy float64

	gx0, gy0 float64
	nx, ny   int
	solids   [][]span
	prisms   []prismJSON
}

func LoadWorld(dir string) (*World, error) {
	data, err := os.ReadFile(filepath.Join(dir, "v2", "world.json"))
	if err != nil {
		return nil, err
	}
	var raw struct {
		Bounds struct {
			MinX float64 `json:"minX"`
			MaxX float64 `json:"maxX"`
			MinY float64 `json:"minY"`
			MaxY float64 `json:"maxY"`
		} `json:"bounds"`
		Terrain struct {
			File  string   `json:"file"`
			MinX  float64  `json:"minX"`
			MinY  float64  `json:"minY"`
			Step  float64  `json:"step"`
			NX    int      `json:"nx"`
			NY    int      `json:"ny"`
			Scale *float64 `json:"scale"`
		} `json:"terrain"`
		Prisms []prismJSON `json:"prisms"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	t := raw.Terrain
	w := &World{tminx: t.MinX, tminy: t.MinY, tstep: t.Step, tnx: t.NX, tny: t.NY, tscale: 0.01, prisms: raw.Prisms}
	if t.Scale != nil {
		w.tscale = *t.Scale
	}
	file := t.File
	if file == "" {
		file = "terrain.i16"
	}
	b, err := os.ReadFile(filepath.Join(dir, "v2", file))
	if err != nil {
		return nil, err
	}
	w.th = make([]int16, len(b)/2)
	for i := range w.th {
		w.th[i] = int16(binary.LittleEndian.Uint16(b[2*i:]))
	}

	w.bminx, w.bmaxx, w.bminy, w.bmaxy = raw.Bounds.MinX, raw.Bounds.MaxX, raw.Bounds.MinY, raw.Bounds.MaxY
	w.gx0, w.gy0 = w.tminx, w.tminy
	w.nx = int(float64(w.tnx-1) * w.tstep / 2)
	w.ny = int(float64(w.tny-1) * w.tstep / 2)
	w.solids = make([][]span, w.nx*w.ny)
	for i := range raw.Prisms {
		w.raster(&raw.Prisms[i])
	}
	for i, s := range w.solids {
		if len(s) > 1 {
			sort.Slice(s, func(a, b int) bool {
				if s[a].lo != s[b].lo {
					return s[a].lo < s[b].lo
				}
				return s[a].hi < s[b].hi
			})
			w.solids[i] = s
		}
	}
	return w, nil
}

// groundH returns the bilinearly interpolated terrain height at (x, y).
func (w *World) groundH(x, y float64) float64 {
	fx := (x - w.tminx) / w.tstep
	fy := (y - w.tminy) / w.tstep
	ix := int(fx)
	iy := int(fy)
	if ix < 0 || iy < 0 || ix >= w.tnx-1 || iy >= w.tny-1 {
		ix = min(max(ix, 0), w.tnx-2)
		iy = min(max(iy, 0), w.tny-2)
		fx, fy = float64(ix), float64(iy)
	}
	tx, ty := fx-float64(ix), fy-float64(iy)
	n := w.tnx
	i := iy*n + ix
	h00, h10 := float64(w.th[i]), float64(w.th[i+1])
	h01, h11 := float64(w.th[i+n]), float64(w.th[i+n+1])
	h := (h00*(1-tx)+h10*tx)*(1-ty) + (h01*(1-tx)+h11*tx)*ty
	return h * w.tscale
}

// cell maps a world position onto the 2-unit solids grid, or -1 when outside.
func (w *World) cell(x, y float64) int {
	fx := (x - w.gx0) / 2
	ix := int(fx)
	fy := (y - w.gy0) / 2
	iy := int(fy)
	if ix < 0 || iy < 0 || ix >= w.nx || iy >= w.ny || fx <= -1 || fy <= -1 {
		return -1
	}
	return iy*w.nx + ix
}

// raster scan-converts one prism footprint (outer ring + holes) into solid spans.
func (w *World) raster(p *prismJSON) {
	lo, hi := p.Base, p.Top

	rings := append([][]float64{p.Ring}, p.Holes...)
	var edges []edge
	for _, r := range rings {
		n := len(r) / 2
		for i := 0; i < n; i++ {
			j := (i + 1) % n
			edges = append(edges, edge{r[2*i], r[2*i+1], r[2*j], r[2*j+1]})
		}
	}

	minY, maxY := math.Inf(1), math.Inf(-1)
	for i := 1; i < len(p.Ring); i += 2 {
		minY = math.Min(minY, p.Ring[i])
		maxY = math.Max(maxY, p.Ring[i])
	}

	j0 := max(0, int((minY-w.gy0)/2-0.5))
	j1 := min(w.ny-1, int((maxY-w.gy0)/2+0.5))
	var xs []float64
	for j := j0; j <= j1; j++ {
		y := w.gy0 + (float64(j)+0.5)*2
		xs = xs[:0]
		for _, e := range edges {
			if (e.y1 <= y && y < e.y2) || (e.y2 <= y && y < e.y1) {
				xs = append(xs, e.x1+(y-e.y1)*(e.x2-e.x1)/(e.y2-e.y1))
			}
		}
		sort.Float64s(xs)
		for k := 0; k+1 < len(xs); k += 2 {
			i0 := max(0, int(math.Ceil((xs[k]-w.gx0)/2-0.5)))
			i1 := min(w.nx-1, int(math.Floor((xs[k+1]-w.gx0)/2-0.5)))
			for i := i0; i <= i1; i++ {
				c := j*w.nx + i
				w.solids[c] = append(w.solids[c], span{lo, hi})
			}
		}
	}
}

// Sight is a 1-unit resolution occlusion map used for line-of-sight tests.
type Sight struct {
	x0, y0 float64
	nx, ny int
	top    []float32 // highest contiguous surface per cell (terrain + attached roofs)
	slabs  map[int][]span
}

func NewSight(w *World) *Sight {
	s := &Sight{x0: w.tminx, y0: w.tminy, nx: w.tnx, ny: w.tny, slabs: map[int][]span{}}
	n := s.nx * s.ny
	s.top = make([]float32, n)
	for i := range s.top {
		s.top[i] = float32(float64(w.th[i]) * w.tscale)
	}

	tmp := map[int][]span{}
	var keys []int
	for i := range w.prisms {
		p := &w.prisms[i]
		rings := [][]float64{p.Ring}
		for _, h := range p.Holes {
			if len(h) > 0 {
				rings = append(rings, h)
			}
		}

		minY, maxY := math.Inf(1), math.Inf(-1)
		for k := 1; k < len(p.Ring); k += 2 {
			minY = math.Min(minY, p.Ring[k])
			maxY = math.Max(maxY, p.Ring[k])
		}
		lo, hi := p.Base, p.Top
		j0 := max(0, int(math.Ceil(minY-s.y0)))
		j1 := min(s.ny-1, int(math.Floor(maxY-s.y0)))
		var xs []float64
		for j := j0; j <= j1; j++ {
			y := float64(j) + s.y0 + 1e-4
			xs = xs[:0]
			for _, r := range rings {
				m := len(r) / 2
				for a := 0; a < m; a++ {
					b := (a + 1) % m
					x1, y1, x2, y2 := r[2*a], r[2*a+1], r[2*b], r[2*b+1]
					if (y >= y1) != (y >= y2) {
						xs = append(xs, x1+(y-y1)/(y2-y1)*(x2-x1))
					}
				}
			}
			sort.Float64s(xs)
			row := j * s.nx
			for k := 0; k+1 < len(xs); k += 2 {
				i0 := max(0, int(math.Ceil(xs[k]-s.x0)))
				i1 := min(s.nx-1, int(math.Floor(xs[k+1]-s.x0)))
				for i := i0; i <= i1; i++ {
					c := row + i
					if _, ok := tmp[c]; !ok {
						keys = append(keys, c)
					}
					tmp[c] = append(tmp[c], span{lo, hi})
				}
			}
		}
	}

	for _, c := range keys {
		sp := tmp[c]
		sort.Slice(sp, func(a, b int) bool {
			if sp[a].lo != sp[b].lo {
				return sp[a].lo < sp[b].lo
			}
			return sp[a].hi < sp[b].hi
		})
		// Merge spans that sit on (or just above) the current top into it;
		// everything floating higher becomes an overhang slab.
		top := float64(s.top[c])
		for _, p := range sp {
			if p.hi <= top {
				continue
			}
			if p.lo <= top+1.5 {
				top = p.hi
				continue
			}
			s.slabs[c] = append(s.slabs[c], p)
		}
		s.top[c] = float32(top)
	}
	return s
}

// clear reports whether the straight segment a->b is unobstructed.
func (s *Sight) clear(a, b [3]float64) bool {
	dx, dy, dz := b[0]-a[0], b[1]-a[1], b[2]-a[2]
	d := math.Sqrt(dx*dx + dy*dy + dz*dz)
	n := int(d) + 1
	inv := 1 / float64(n)
	for k := 1; k < n; k++ {
		t := float64(k) * inv
		z := a[2] + dz*t
		ix := int(a[0] + dx*t - s.x0 + 0.5)
		iy := int(a[1] + dy*t - s.y0 + 0.5)
		if ix < 0 {
			ix = 0
		} else if ix >= s.nx {
			ix = s.nx - 1
		}
		if iy < 0 {
			iy = 0
		} else if iy >= s.ny {
			iy = s.ny - 1
		}
		c := iy*s.nx + ix
		if float64(s.top[c])-0.05 > z {
			return false
		}
		for _, sp := range s.slabs[c] {
			if z > sp.lo && z < sp.hi {
				return false
			}
		}
	}
	return true
}

// 4-neighbours first, then diagonals.
var dirs = [8][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}, {1, 1}, {1, -1}, {-1, 1}, {-1, -1}}

// Grid is the walkability / navigation grid (2-unit cells).
type Grid struct {
	w        *World
	x0, y0   float64
	nx, ny   int
	surf     []float64 // standing height per cell
	blocked  []bool
	clr      []float64 // distance to nearest blocked cell (capped at 20)
	comp     []int     // connected component id
	main     int       // id of the largest component
	mainNear []int     // nearest main-component cell for every cell (or -1)
}

func NewGrid(w *World) *Grid {
	g := &Grid{w: w, x0: w.tminx, y0: w.tminy, nx: (w.tnx + 1) / 2, ny: (w.tny + 1) / 2}
	n := g.nx * g.ny
	g.surf = make([]float64, n)
	g.blocked = make([]bool, n)
	for c := 0; c < n; c++ {
		x, y := g.pos(c)
		h, roof := g.standHeight(x, y)
		g.surf[c] = h
		g.blocked[c] = roof || !g.standable(x, y, h)
	}

	// Clearance: BFS distance from blocked cells.
	g.clr = make([]float64, n)
	q := make([]int, 0, n)
	for c := 0; c < n; c++ {
		if g.blocked[c] {
			g.clr[c] = 0
			q = append(q, c)
		} else {
			g.clr[c] = 1e9
		}
	}
	for i := 0; i < len(q); i++ {
		c := q[i]
		d := g.clr[c] + 2
		if d > 20 {
			continue
		}
		cx, cy := c%g.nx, c/g.nx
		for _, o := range dirs[:4] {
			x, y := cx+o[0], cy+o[1]
			if x < 0 || x >= g.nx || y < 0 || y >= g.ny {
				continue
			}
			k := y*g.nx + x
			if g.clr[k] > d {
				g.clr[k] = d
				q = append(q, k)
			}
		}
	}
	for i := range g.clr {
		if g.clr[i] > 20 {
			g.clr[i] = 20
		}
	}

	// Connected components; remember the biggest one.
	g.comp = make([]int, n)
	id, best, bestN := 0, 0, 0
	for c := 0; c < n; c++ {
		if g.blocked[c] || g.comp[c] != 0 {
			continue
		}
		id++
		g.comp[c] = id
		q = append(q[:0], c)
		head := 0
		for head < len(q) {
			cur := q[head]
			head++
			cx, cy := cur%g.nx, cur/g.nx
			for _, o := range dirs[:4] {
				x, y := cx+o[0], cy+o[1]
				if x < 0 || x >= g.nx || y < 0 || y >= g.ny {
					continue
				}
				k := y*g.nx + x
				if g.comp[k] == 0 && g.stepOK(cur, k, false) && g.stepOK(k, cur, false) {
					g.comp[k] = id
					q = append(q, k)
				}
			}
		}
		if head > bestN {
			bestN, best = head, id
		}
	}
	g.main = best
	g.buildMainNear()
	return g
}

// standHeight returns the height you would stand at in (x, y) and whether that is a roof
// (i.e. more than 1.5 above the terrain).
func (g *Grid) standHeight(x, y float64) (float64, bool) {
	w := g.w
	h := w.groundH(x, y)
	c := w.cell(x, y)
	if c < 0 || len(w.solids[c]) == 0 {
		return h, false
	}
	sp := w.solids[c]
	top := h
	for changed := true; changed; {
		changed = false
		for _, s := range sp {
			if s.hi > top && s.lo <= top+1.5 {
				top = s.hi
				changed = true
			}
		}
	}
	return top, top-h > 1.5
}

// standable checks the mech body (radius ~2.6, height 1.2..9) fits at (x, y, h).
func (g *Grid) standable(x, y, h float64) bool {
	w := g.w
	const m = 2.6
	if x < w.bminx+m || x > w.bmaxx-m || y < w.bminy+m || y > w.bmaxy-m {
		return false
	}
	lo, hi := h+1.2, h+9.0
	offs := [9][2]float64{{0, 0}, {2.6, 0}, {-2.6, 0}, {0, 2.6}, {0, -2.6}, {1.82, 1.82}, {-1.82, 1.82}, {1.82, -1.82}, {-1.82, -1.82}}
	for _, o := range offs {
		c := w.cell(x+o[0], y+o[1])
		if c < 0 {
			return false
		}
		for _, s := range w.solids[c] {
			if s.hi > lo && s.lo < hi {
				return false
			}
		}
	}
	return true
}

// solid reports whether point (x, y, z) is inside terrain or a building.
func (g *Grid) solid(x, y, z float64) bool {
	w := g.w
	if w.groundH(x, y) > z {
		return true
	}
	c := w.cell(x, y)
	if c < 0 {
		return false
	}
	for _, s := range w.solids[c] {
		if z > s.lo && z < s.hi {
			return true
		}
	}
	return false
}

// topAt returns the walkable surface height at (x, y) (single pass over spans).
func (g *Grid) topAt(x, y float64) float64 {
	w := g.w
	h := w.groundH(x, y)
	c := w.cell(x, y)
	if c >= 0 {
		for _, s := range w.solids[c] {
			if s.hi > h && s.lo <= h+1.5 {
				h = s.hi
			}
		}
	}
	return h
}

func (g *Grid) cell(x, y float64) int {
	ix := min(max(int(math.RoundToEven((x-g.x0)/2)), 0), g.nx-1)
	iy := min(max(int(math.RoundToEven((y-g.y0)/2)), 0), g.ny-1)
	return iy*g.nx + ix
}

func (g *Grid) pos(c int) (float64, float64) {
	return g.x0 + float64(c%g.nx)*2, g.y0 + float64(c/g.nx)*2
}

func (g *Grid) free(x, y float64) bool {
	return !g.blocked[g.cell(x, y)]
}

func (g *Grid) walkAt(x, y float64) float64 {
	c := g.cell(x, y)
	if !g.blocked[c] {
		return g.surf[c]
	}
	h, _ := g.standHeight(x, y)
	return h
}

// stepOK: can we step from cell a into cell b (max climb 2.2, or 3.1 diagonally)?
func (g *Grid) stepOK(a, b int, diag bool) bool {
	if g.blocked[b] {
		return false
	}
	lim := 2.2
	if diag {
		lim = 3.1
	}
	return g.surf[b]-g.surf[a] <= lim
}

// nearestFree returns c, or the closest unblocked cell within 20 rings of it (-1 if none).
func (g *Grid) nearestFree(c int) int {
	if !g.blocked[c] {
		return c
	}
	cx, cy := c%g.nx, c/g.nx
	for r := 1; r < 21; r++ {
		best, bestD := -1, 1<<30
		try := func(dx, dy int) {
			x, y := cx+dx, cy+dy
			if x >= 0 && x < g.nx && y >= 0 && y < g.ny && !g.blocked[y*g.nx+x] {
				if d := dx*dx + dy*dy; d < bestD {
					bestD, best = d, y*g.nx+x
				}
			}
		}
		for dy := -r; dy <= r; dy++ {
			if dy == -r || dy == r {
				for dx := -r; dx <= r; dx++ {
					try(dx, dy)
				}
			} else {
				try(-r, dy)
				try(r, dy)
			}
		}
		if best >= 0 {
			return best
		}
	}
	return -1
}

// lineOK walks the grid along a->b and fails on blocked cells or climbs > 3.1.
func (g *Grid) lineOK(ax, ay, bx, by float64) bool {
	dx, dy := bx-ax, by-ay
	n := int(math.Hypot(dx, dy)) + 1
	prev := g.cell(ax, ay)
	for i := 1; i <= n; i++ {
		t := float64(i) / float64(n)
		c := g.cell(ax+dx*t, ay+dy*t)
		if c == prev {
			continue
		}
		if g.blocked[c] || g.surf[c]-g.surf[prev] > 3.1 {
			return false
		}
		prev = c
	}
	return true
}

func (g *Grid) reachable(x, y float64) bool {
	c := g.cell(x, y)
	return !g.blocked[c] && g.comp[c] == g.main
}

func (g *Grid) offMain(x, y float64) bool {
	c := g.nearestFree(g.cell(x, y))
	return c < 0 || g.comp[c] != g.main
}

// exposure: average free sight distance over 24 horizontal rays at eye height, normalised to [0,1].
func (g *Grid) exposure(x, y float64) float64 {
	eye := g.walkAt(x, y) + 7
	w := g.w
	total := 0.0
	for k := 0; k < 24; k++ {
		a := float64(k) * math.Pi / 12
		c, s := math.Cos(a), math.Sin(a)
		d := 2.5
		for d < 200 {
			px, py := x+c*d, y+s*d
			if px <= w.bminx || px >= w.bmaxx || py <= w.bminy || py >= w.bmaxy || g.solid(px, py, eye) {
				break
			}
			d += 2.5
		}
		total += d
	}
	return total / 4800
}

// buildMainNear: for every cell, the nearest cell of the main component (BFS up to 20 steps).
func (g *Grid) buildMainNear() {
	n := g.nx * g.ny
	near := make([]int, n)
	dist := make([]uint8, n)
	q := make([]int, 0, n)
	for c := 0; c < n; c++ {
		near[c] = -1
		if !g.blocked[c] && g.comp[c] == g.main {
			near[c] = c
			q = append(q, c)
		}
	}
	for i := 0; i < len(q); i++ {
		c := q[i]
		if dist[c] >= 20 {
			continue
		}
		cx, cy := c%g.nx, c/g.nx
		for _, d := range dirs {
			x, y := cx+d[0], cy+d[1]
			if x < 0 || x >= g.nx || y < 0 || y >= g.ny {
				continue
			}
			k := y*g.nx + x
			if near[k] >= 0 {
				continue
			}
			near[k] = near[c]
			dist[k] = dist[c] + 1
			q = append(q, k)
		}
	}
	g.mainNear = near
}

// escPlan: walk to (lx,ly) then jump/fly to (mx,my) to get back onto the main area.
type escPlan struct{ lx, ly, mx, my float64 }

func (g *Grid) escapePlan(x, y float64) *escPlan {
	start := g.nearestFree(g.cell(x, y))
	if start < 0 {
		return nil
	}

	dist := map[int]int{start: 0}
	q := []int{start}
	best, bestCost := -1, 1e18
	for i := 0; i < len(q) && len(dist) < 20000; i++ {
		c := q[i]
		if m := g.mainNear[c]; m >= 0 {
			cx, cy := g.pos(c)
			mx, my := g.pos(m)
			if cost := float64(dist[c])*2 + 4*math.Hypot(mx-cx, my-cy); cost < bestCost {
				best, bestCost = c, cost
			}
		}
		cx, cy := c%g.nx, c/g.nx
		for k, d := range dirs {
			x, y := cx+d[0], cy+d[1]
			if x < 0 || x >= g.nx || y < 0 || y >= g.ny {
				continue
			}
			n := y*g.nx + x
			if _, ok := dist[n]; ok || !g.stepOK(c, n, k >= 4) {
				continue
			}
			if k >= 4 && (g.blocked[cy*g.nx+x] || g.blocked[y*g.nx+cx]) {
				continue
			}
			dist[n] = dist[c] + 1
			q = append(q, n)
		}
	}
	if best < 0 {
		return nil
	}

	lx, ly := g.pos(best)
	mx, my := g.pos(g.mainNear[best])
	if d := math.Hypot(mx-lx, my-ly); d > 0.1 {
		tx, ty := mx+(mx-lx)/d*5, my+(my-ly)/d*5
		w := g.w
		if tx > w.bminx && tx < w.bmaxx && ty > w.bminy && ty < w.bmaxy && g.reachable(tx, ty) {
			mx, my = tx, ty
		}
	}
	return &escPlan{lx, ly, mx, my}
}

type hitem struct {
	f float64
	c int
}

type minHeap []hitem

func (h minHeap) less(i, j int) bool {
	if h[i].f != h[j].f {
		return h[i].f < h[j].f
	}
	return h[i].c < h[j].c
}

func (h *minHeap) push(f float64, c int) {
	*h = append(*h, hitem{f, c})
	a := *h
	i := len(a) - 1
	for i > 0 {
		p := (i - 1) / 2
		if !a.less(i, p) {
			break
		}
		a[i], a[p] = a[p], a[i]
		i = p
	}
}

func (h *minHeap) pop() (float64, int) {
	a := *h
	top := a[0]
	n := len(a) - 1
	a[0] = a[n]
	i := 0
	for {
		l, r := 2*i+1, 2*i+2
		m := i
		if l < n && a.less(l, m) {
			m = l
		}
		if r < n && a.less(r, m) {
			m = r
		}
		if m == i {
			break
		}
		a[i], a[m] = a[m], a[i]
		i = m
	}
	*h = a[:n]
	return top.f, top.c
}

// route runs A* from (sx,sy) to (tx,ty) over the grid and returns the cell path.
// If the goal can't be reached it returns the path to the closest explored cell.
func (g *Grid) route(sx, sy, tx, ty float64) []int {
	s := g.nearestFree(g.cell(sx, sy))
	t := g.nearestFree(g.cell(tx, ty))
	if s < 0 || t < 0 {
		return nil
	}
	if s == t {
		return []int{s}
	}
	gx, gy := t%g.nx, t/g.nx
	h := func(c int) float64 {
		dx, dy := c%g.nx-gx, c/g.nx-gy
		if dx < 0 {
			dx = -dx
		}
		if dy < 0 {
			dy = -dy
		}
		hi, lo := max(dx, dy), min(dx, dy)
		return (float64(hi) + 0.4142*float64(lo)) * 2 * 1.15
	}

	gs := map[int]float64{s: 0}
	from := map[int]int{s: -1}
	closed := map[int]bool{}
	open := minHeap{{h(s), s}}
	bestH, best := h(s), s
	iter := 0
	for len(open) > 0 && iter < 22000 {
		_, c := open.pop()
		if closed[c] {
			continue
		}
		closed[c] = true
		iter++
		if c == t {
			best = c
			break
		}
		if hc := h(c); hc < bestH {
			bestH, best = hc, c
		}
		cx, cy := c%g.nx, c/g.nx
		gc := gs[c]
		for k, d := range dirs {
			x, y := cx+d[0], cy+d[1]
			if x < 0 || x >= g.nx || y < 0 || y >= g.ny {
				continue
			}
			n := y*g.nx + x
			diag := k >= 4
			if closed[n] || !g.stepOK(c, n, diag) {
				continue
			}
			step := 2.0
			if diag {
				if g.blocked[cy*g.nx+x] || g.blocked[y*g.nx+cx] {
					continue
				}
				step = 2.8284
			}
			if cl := g.clr[n]; cl < 4 {
				step *= 1 + 0.15*(4-cl)
			}
			ng := gc + step
			old, ok := gs[n]
			if !ok {
				old = 1e18
			}
			if ng < old {
				gs[n] = ng
				from[n] = c
				open.push(ng+h(n), n)
			}
		}
	}

	var path []int
	for c := best; c >= 0; c = from[c] {
		path = append(path, c)
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path
}
