// The brain behind the rover pilot. Everything that decides what the mech
// does each tick lives here: reading the world into tracked enemies, picking a
// target, choosing a movement mode (fight, retreat, loot, kite, and the rest),
// then aiming, firing, reloading and grabbing pickups.
//
// It's grown organically match by match, so the tuning knobs sit together in
// Tuning and most of the odd-looking constants are things I landed on from
// watching replays rather than anything principled.
//
// Author: Sparsh (Team 4141). Copyright (c) 2026, MIT licensed, see LICENSE and NOTICE.
package main

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Set MECH_DEBUG to anything non-empty to turn on the per-tick debug log.
var debug = os.Getenv("MECH_DEBUG") != ""

// Weapon / module kinds the bot knows how to use.
var knownKinds = map[string]bool{"gun": true, "laser": true, "laser_charge": true, "lightning": true,
	"boost": true, "jet": true, "mortar": true, "step": true}

// Preference ranking for weapon pickups (higher is better).
var weaponRank = map[string]int{"lightning": 3, "laser": 2, "mortar": 1}

// turn returns the signed angle a-b wrapped into [-pi, pi].
func turn(a, b float64) float64 {
	d := math.Mod(a-b, 2*math.Pi)
	if d > math.Pi {
		d -= 2 * math.Pi
	} else if d < -math.Pi {
		d += 2 * math.Pi
	}
	return d
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

func hyp(x, y float64) float64 { return math.Hypot(x, y) }

// ---------------------------------------------------------------------------
// xorshift64 RNG

type xorshift struct{ s uint64 }

func newXorshift(seed uint64) *xorshift { return &xorshift{seed*0x9e3779b97f4a7c15 | 1} }

func (r *xorshift) random() float64 {
	x := r.s
	x ^= x << 13
	x ^= x >> 7
	x ^= x << 17
	r.s = x
	return float64(x>>11) / (1 << 53)
}

// ---------------------------------------------------------------------------
// omap is an insertion-ordered map (deterministic iteration).

type omap[T any] struct {
	keys []int
	m    map[int]T
}

func newOmap[T any]() *omap[T] { return &omap[T]{m: map[int]T{}} }

func (o *omap[T]) get(k int) (T, bool) { v, ok := o.m[k]; return v, ok }

func (o *omap[T]) set(k int, v T) {
	if _, ok := o.m[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.m[k] = v
}

func (o *omap[T]) del(k int) {
	if _, ok := o.m[k]; !ok {
		return
	}
	delete(o.m, k)
	for i, x := range o.keys {
		if x == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			return
		}
	}
}

func (o *omap[T]) values() []T {
	out := make([]T, 0, len(o.keys))
	for _, k := range o.keys {
		out = append(out, o.m[k])
	}
	return out
}

// ---------------------------------------------------------------------------
// Tracked entities

// Contact is our memory of another mech or NPC.
type Contact struct {
	id       int
	kind     string
	pos      []float64
	vel      []float64
	aim      []float64
	hp       float64
	maxHP    float64
	cubes    float64
	weapons  []string
	charging bool
	seen     float64 // last time we saw it
	hitMe    float64 // last time it damaged us
	dist     float64
	los      bool
	closing  float64 // closing speed towards us
	engage   bool
	laserAt  float64 // when we last saw it fire a laser
}

func (t *Contact) has(k string) bool {
	for _, w := range t.weapons {
		if w == k {
			return true
		}
	}
	return false
}

// thing is a remembered crate / cube / pickup.
type thing struct {
	id   int
	kind string
	pos  []float64
	seen float64
}

// heard is a remembered sound.
type heard struct {
	kind    string
	bearing float64
	far     bool
	t       float64
}

type lootPick struct {
	kind string
	id   int
	pos  []float64
}

// ---------------------------------------------------------------------------
// Tunable behaviour parameters

type Tuning struct {
	retreatHP  float64
	recoverHP  float64
	gunRange   float64
	lightRange float64
	laserRange float64
	engageDist float64
	farmNPC    bool
	ghost      bool
	lootRadius float64
	crateUntil float64
	dodge      bool
	noMortar   bool
	peek       bool
	laserFirst bool
	crowdHide  bool
	practice   bool
	stick      float64
	hpw        float64
	lowBonus   float64
	maxTarget  float64
	loseRule   bool
	fightLoot  float64
	farmHP     float64
	laserSeek  float64
	laserUp    bool
	opening    bool

	// Practice-squad experiments, all off unless buildVariant turns them on.
	huntRange float64 // engage any visible mech this close while healthy (0 = off)
	huntHP    float64 // ... only above this share of our health
	seek      float64 // with nothing to do, walk to where a mech was seen at most this many seconds ago (0 = off)
	seekRange float64 // ... no farther than this
	seekHP    float64 // ... only above this share of our health
	farmCubes float64 // stop shooting crates and drones for cubes once we hold this many (0 = no cap)
	credit    bool    // a death right after the mech we were shooting dropped out of sight counts as ours
}

// buildVariant names the practice experiment this build plays: "base" (the build that scored 6.23),
// "hunt", "seek", "credit" or "all". hunt/seek/farmCubes only act in the practice stance, so
// free-for-all play is unchanged by them; credit also applies in a free-for-all (a death we probably
// caused no longer counts towards spotting one).
const buildVariant = "credit"

// rngSalt changes only the dice: builds that differ in it alone play the same game with other luck.
var rngSalt uint64 = 4

// variant applies buildVariant on top of the practice parameter set.
func variant(p Tuning) Tuning {
	switch buildVariant {
	case "hunt":
		p.huntRange, p.huntHP = 180, 0.6
	case "seek":
		p.seek, p.seekRange, p.seekHP = 20, 320, 0.7
	case "all":
		p.huntRange, p.huntHP = 180, 0.6
		p.seek, p.seekRange, p.seekHP = 20, 320, 0.7
		p.farmCubes = 6
	}
	return p
}

func defaults() Tuning {
	return Tuning{retreatHP: 0.38, recoverHP: 0.8, gunRange: 85, lightRange: 28, laserRange: 85, engageDist: 60,
		farmNPC: true, ghost: true, lootRadius: 90, crateUntil: 150,
		dodge: true, noMortar: true, peek: true, laserFirst: true, crowdHide: true, practice: true,
		credit: buildVariant == "credit" || buildVariant == "all"}
}

// squad is the parameter set used once the match is past the "practice" phase.
func squad(p Tuning) Tuning {
	p.ghost, p.peek, p.laserFirst = false, false, true
	p.laserRange, p.lootRadius, p.crateUntil, p.stick = 120, 150, 1e9, 60
	p.hpw, p.lowBonus, p.maxTarget, p.loseRule = 0.2, 100, 240, true
	p.fightLoot, p.farmHP, p.laserSeek, p.laserUp = 0, 0.5, 240, true
	for k, v := range squadOverride {
		switch k {
		case "retreatHP":
			p.retreatHP = v
		case "recoverHP":
			p.recoverHP = v
		case "gunRange":
			p.gunRange = v
		case "lightRange":
			p.lightRange = v
		case "laserRange":
			p.laserRange = v
		case "engageDist":
			p.engageDist = v
		case "lootRadius":
			p.lootRadius = v
		case "stick":
			p.stick = v
		case "hpw":
			p.hpw = v
		case "lowBonus":
			p.lowBonus = v
		case "maxTarget":
			p.maxTarget = v
		case "fightLoot":
			p.fightLoot = v
		case "farmHP":
			p.farmHP = v
		case "laserSeek":
			p.laserSeek = v
		case "loseRule":
			p.loseRule = v != 0
		case "crowdHide":
			p.crowdHide = v != 0
		case "noMortar":
			p.noMortar = v != 0
		case "dodge":
			p.dodge = v != 0
		}
	}
	return variant(p)
}

// ---------------------------------------------------------------------------
// Agent

type Agent struct {
	cfg   Config
	rng   *xorshift
	world *World
	grid  *Grid
	sight *Sight

	P    Tuning // active parameters
	base Tuning // parameters at match start

	baseSet     bool
	pracOn      bool
	openOn      bool
	notPractice bool
	startAlive  int
	kills       int
	otherDead   int
	deadSeen    int // mechs dead at the last tick
	credited    int // deaths put down to our own shooting without a kill event
	npcHurt     float64
	npcBearing  float64
	seekBan     map[int]float64 // mechs we went looking for and did not find, until when

	tracks  *omap[*Contact]
	npcs    *omap[*Contact]
	crates  *omap[*thing]
	cubes   *omap[*thing]
	pickups *omap[*thing]
	bad     map[int]float64 // loot ids that failed, with time

	now float64

	route     []int
	routeGoal *[2]float64
	routeAt   float64
	routeI    int

	strafe      float64
	strafeUntil float64
	retreat     bool

	coverAt     float64
	coverGoal   [2]float64
	coverOK     bool
	coverFailAt float64

	lastHurt    float64
	hurtBearing float64

	lootID    int
	lootSince float64

	hold      [3]float64 // zone circle (x, y, r) we are holding to
	holdStart *float64

	homeXY  [2]float64
	homeAt  float64
	homeSet bool

	stuckSince   float64
	unstuckUntil float64
	unstuckDir   [2]float64

	lastThreat float64

	esc       *escPlan
	escAt     float64
	escFlying bool

	stickID int
	stickAt float64
	loseAt  float64

	trail [][2]float64
	heard []heard
	mode  string

	ffaBeams int // observations showing a beam from one mech ending on another, far from us

	zg    [2]float64 // held zone goal
	zgKey [3]float64 // the next circle it was chosen for
}

// Ghost (free-for-all) retaliation window and the held zone goal. MECH_ARM overrides them for A/B
// tests only, e.g. MECH_ARM="revenge=4,zonefix=0"; the match container never sets it.
var (
	armRevenge    = 12.0
	armZoneFix    = 2.0
	armDodgeAlign = true
	armFFAOnly    = true
	armAntiLight  = true
	armLaserCD    = false
	armBeamFFA    = false
	armLateT      = 0.0
	navRefresh    = 2.5
)

// ffaFix: the fixes above run once a free-for-all is detected; practice keeps the scored play.
func (b *Agent) ffaFix() bool { return !armFFAOnly || b.notPractice }

func (b *Agent) revenge() float64 {
	if b.ffaFix() {
		return armRevenge
	}
	return 4
}

// squadOverride: practice squad parameters set by MECH_ARM "sq.<name>=v" (A/B tests only).
var squadOverride = map[string]float64{}

func init() {
	for _, kv := range strings.Split(os.Getenv("MECH_ARM"), ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			continue
		}
		if name, ok := strings.CutPrefix(k, "sq."); ok {
			squadOverride[name] = f
			continue
		}
		switch k {
		case "revenge":
			armRevenge = f
		case "zonefix":
			armZoneFix = f
		case "dodgealign":
			armDodgeAlign = f != 0
		case "ffaonly":
			armFFAOnly = f != 0
		case "antilight":
			armAntiLight = f != 0
		case "lasercd":
			armLaserCD = f != 0
		case "beamffa":
			armBeamFFA = f != 0
		case "latet":
			armLateT = f
		}
	}
}

func NewAgent(h *Hello) *Agent {
	seed := int(h.Seed)
	if seed == 0 {
		seed = h.Slot + 1
	}
	b := &Agent{cfg: h.Config, rng: newXorshift(uint64(seed) + rngSalt*0x2545f4914f6cdd1d), P: defaults(),
		tracks: newOmap[*Contact](), npcs: newOmap[*Contact](), crates: newOmap[*thing](), cubes: newOmap[*thing](), pickups: newOmap[*thing](),
		bad: map[int]float64{}, seekBan: map[int]float64{}, routeAt: -99, strafe: 1, coverAt: -99, coverFailAt: -99, lastHurt: -100, lootID: -1,
		homeAt: -99, stuckSince: -1, unstuckDir: [2]float64{1, 0}, lastThreat: -100, escAt: -99, stickID: -1, stickAt: -99, loseAt: -99,
		mode: "idle"}
	var w *World
	var err error
	for _, dir := range []string{h.Map.Dir, "/arena", "arena"} {
		if dir == "" {
			continue
		}
		if w, err = LoadWorld(dir); err == nil {
			break
		}
	}
	if w == nil {
		fmt.Fprintln(os.Stderr, "map setup failed:", err)
		return b
	}
	b.world = w
	b.sight = NewSight(w)
	b.grid = NewGrid(w)
	return b
}

// scale is the mech size multiplier for a given cube count.
func (b *Agent) scale(cubes float64) float64 {
	return 1 + cubes*b.cfg.num(b.cfg.Cubes, "size_per_cube", 0.05)
}

// center is the body centre of a tracked entity.
func (b *Agent) center(t *Contact) [3]float64 {
	var h float64
	if t.kind == "npc" {
		h = b.cfg.Npc["height"]
	} else {
		h = b.cfg.Mech["height"] * b.scale(t.cubes)
	}
	return [3]float64{t.pos[0], t.pos[1], t.pos[2] + h*0.5}
}

func (b *Agent) eyeOf(t *Contact) [3]float64 {
	return [3]float64{t.pos[0], t.pos[1], t.pos[2] + b.scale(t.cubes)*b.cfg.Mech["eye_height"]}
}

func (b *Agent) clear(a, c [3]float64) bool {
	if b.sight == nil {
		return true
	}
	return b.sight.clear(a, c)
}

func vec3(v []float64) []float64 {
	if len(v) >= 3 {
		return v
	}
	var out [3]float64
	copy(out[:], v)
	return out[:]
}

func vec2(v []float64) []float64 {
	if len(v) >= 2 {
		return v
	}
	var out [2]float64
	copy(out[:], v)
	return out[:]
}

// perceive folds a new observation into the bot's memory.
func (b *Agent) perceive(o *Obs) {
	stats := o.Self.Stats
	now := o.T
	me := o.Self.Pos
	eye := [3]float64{me[0], me[1], me[2] + get(stats, "eye_height", 8)}
	muzzle := [3]float64{me[0], me[1], me[2] + get(stats, "muzzle_height", 7)}

	for i := range o.Mechs {
		m := &o.Mechs[i]
		tr := b.tracks
		if m.Kind == "npc" {
			tr = b.npcs
		}
		t, ok := tr.get(m.ID)
		if !ok {
			t = &Contact{hitMe: -100}
			tr.set(m.ID, t)
		}
		maxHP := 350.0
		if m.Kind == "mech" {
			maxHP = 1400
		}
		t.id, t.kind, t.pos = m.ID, m.Kind, vec3(m.Pos)
		t.vel = []float64{0, 0, 0}
		if m.Vel != nil {
			t.vel = vec3(m.Vel)
		}
		t.aim = []float64{0, 0}
		if m.Aim != nil {
			t.aim = vec2(m.Aim)
		}
		t.hp, t.maxHP = opt(m.HP, maxHP), opt(m.MaxHP, maxHP)
		t.cubes, t.weapons, t.charging = opt(m.Cubes, 0), m.Weapons, m.Charging
		t.seen = now
		dx, dy := m.Pos[0]-me[0], m.Pos[1]-me[1]
		t.dist = hyp(dx, dy)
		t.los = b.clear(eye, b.center(t))
		if !t.los && m.Kind == "mech" {
			h := b.cfg.Mech["height"] * b.scale(t.cubes)
			t.los = b.clear(muzzle, [3]float64{m.Pos[0], m.Pos[1], m.Pos[2] + 0.85*h})
		}
		t.closing = 0
		if t.dist > 1 {
			t.closing = -((t.vel[0]-o.Self.Vel[0])*dx + (t.vel[1]-o.Self.Vel[1])*dy) / t.dist
		}
	}
	for _, t := range b.tracks.values() {
		if now-t.seen > 20 {
			b.tracks.del(t.id)
		}
	}
	for _, t := range b.npcs.values() {
		if now-t.seen > 120 {
			b.npcs.del(t.id)
		}
	}

	self := &o.Self
	upd := func(m *omap[*thing], items []Item) {
		for _, it := range items {
			m.set(it.ID, &thing{id: it.ID, kind: it.Kind, pos: vec3(it.Pos), seen: now})
		}
		// Forget things we should be able to see but no longer do.
		for _, id := range append([]int(nil), m.keys...) {
			t := m.m[id]
			if t.seen == now {
				continue
			}
			p := t.pos
			if hyp(p[0]-self.Pos[0], p[1]-self.Pos[1]) < 40 && now-t.seen > 0.35 &&
				b.clear(eye, [3]float64{p[0], p[1], p[2] + 1.5}) {
				m.del(id)
			}
		}
	}
	upd(b.crates, o.Crates)
	upd(b.cubes, o.Cubes)
	upd(b.pickups, o.Pickups)

	for _, e := range o.Events {
		if e.Type == "damaged" && e.Weapon == "npc" && e.Bearing != nil {
			b.npcHurt, b.npcBearing = now, *e.Bearing
		}
		if e.Type == "damaged" && e.By != nil {
			if t, ok := b.npcs.get(*e.By); ok {
				t.hitMe = now
			}
			b.lastHurt = now
			if e.Bearing != nil {
				b.hurtBearing = *e.Bearing
			}
			if t, ok := b.tracks.get(*e.By); ok {
				t.hitMe = now
			}
		} else if e.Type == "kill" {
			b.kills++
			if e.Target != nil {
				b.tracks.del(*e.Target)
			}
		}
	}

	b.readBeams(o)
	if !b.baseSet {
		b.base, b.baseSet = b.P, true
		b.startAlive = o.Alive
	}
	dead := b.startAlive - o.Alive
	if b.base.credit && dead > b.deadSeen && b.stickID >= 0 && now-b.stickAt < 6 {
		// A death with no kill event for us, moments after we were shooting at a mech that is not in
		// sight now: most likely ours (finished by splash or a drone, or the kill event is late). The
		// house squad does not kill its own, so counting it as somebody else's would drop the practice
		// stance for the rest of the match.
		if t, ok := b.tracks.get(b.stickID); !ok || t.seen != now {
			b.credited++
		}
	}
	if dead > b.deadSeen {
		b.deadSeen = dead
	}
	if c := dead - b.kills; b.credited > c {
		b.credited = max(0, c) // a kill event arrived for a death we had guessed: never count it twice
	}
	if dead-b.kills-b.credited > 0 {
		b.otherDead++
	} else {
		b.otherDead = 0
	}
	if b.otherDead >= 5 || armBeamFFA && b.ffaBeams >= 2 {
		b.notPractice = true
	}
	late := armLateT > 0 && (now >= armLateT || o.Zone.NextRadius > 0 && o.Zone.NextRadius <= 72 && now >= 180)
	prac := b.base.practice && !b.notPractice && now > 20 && !late
	if prac != b.pracOn {
		b.pracOn = prac
		if prac {
			b.P = squad(b.base)
		} else {
			b.P = b.base
		}
	}
	open := b.base.opening && !b.notPractice && !b.pracOn && b.startAlive-o.Alive-b.kills <= 0
	if open != b.openOn {
		b.openOn = open
		b.P = b.base
		if open {
			b.P.laserFirst, b.P.laserSeek, b.P.lootRadius = true, 240, 150
		} else if b.pracOn {
			b.P = squad(b.base)
		}
	}

	z := &o.Zone
	if b.holdStart == nil || *b.holdStart != z.ShrinkStart {
		s := z.ShrinkStart
		b.holdStart = &s
		b.hold = [3]float64{z.Center[0], z.Center[1], z.Radius}
	} else if z.ShrinkStart >= now {
		b.hold = [3]float64{z.Center[0], z.Center[1], z.Radius}
	}
}

// ---------------------------------------------------------------------------
// Zone (shrinking safe circle)

// zoneAt predicts the safe circle (x, y, r) at time t.
func (b *Agent) zoneAt(z *Zone, t float64) [3]float64 {
	if t <= z.ShrinkStart || z.ShrinkEnd <= z.ShrinkStart {
		return b.hold
	}
	f := clamp((t-z.ShrinkStart)/(z.ShrinkEnd-z.ShrinkStart), 0, 1)
	h := b.hold
	return [3]float64{h[0] + (z.NextCenter[0]-h[0])*f, h[1] + (z.NextCenter[1]-h[1])*f, h[2] + (z.NextRadius-h[2])*f}
}

func (b *Agent) curZone(z *Zone) [3]float64 {
	cur := [3]float64{z.Center[0], z.Center[1], z.Radius}
	if z.ShrinkStart >= b.now {
		return cur
	}
	if math.Abs(z.Radius-b.hold[2]) > 0.01 || math.Abs(cur[0]-b.hold[0]) > 0.01 {
		return cur
	}
	return b.zoneAt(z, b.now)
}

// zoneDeadline: the time at which (x, y) falls outside the shrinking zone (with margin m).
func (b *Agent) zoneDeadline(z *Zone, x, y, m float64) float64 {
	if z.NextRadius-m >= hyp(x-z.NextCenter[0], y-z.NextCenter[1]) {
		return math.Inf(1)
	}
	for i := 0; i < 25; i++ {
		t := z.ShrinkStart + (z.ShrinkEnd-z.ShrinkStart)*float64(i)/24
		c := b.zoneAt(z, t)
		if hyp(x-c[0], y-c[1]) > c[2]-m {
			return t
		}
	}
	return z.ShrinkEnd
}

// zonePenalty scores how bad standing at (x, y) is with respect to the zone.
func (b *Agent) zonePenalty(z *Zone, x, y float64) float64 {
	c := b.curZone(z)
	p := 0.0
	if d := hyp(x-c[0], y-c[1]) - c[2]; d > -5 {
		p += 150 + (d+5)*5
	}
	if d := hyp(x-z.NextCenter[0], y-z.NextCenter[1]) - z.NextRadius; d > -5 {
		u := clamp(1-(z.ShrinkStart-b.now)/25, 0.1, 1)
		p += u * (25 + (d+5)*0.8)
	}
	return p
}

// zoneGoal returns the nearest point to (x, y) safely inside the next zone.
func (b *Agent) zoneGoal(z *Zone, x, y float64) (float64, float64) {
	r, c := z.NextRadius, z.NextCenter
	if r <= 30 {
		return c[0], c[1]
	}
	m := clamp(0.3*r, 3, 30)
	dx, dy := x-c[0], y-c[1]
	d := hyp(dx, dy)
	if d < 1 {
		return x, y
	}
	lim := math.Max(0, r-m-5)
	if d <= lim {
		return x, y
	}
	return c[0] + dx/d*lim, c[1] + dy/d*lim
}

// holdZoneGoal is zoneGoal made stable for path following. The nearest safe point slides as we walk,
// and re-planning after every slide flips the A* route between the two ways round a building.
// armZoneFix 1 holds one goal per circle; 2 also moves it to a walkable cell on the safe circle.
func (b *Agent) holdZoneGoal(z *Zone, x, y float64) (float64, float64) {
	zx, zy := b.zoneGoal(z, x, y)
	if armZoneFix == 0 || !b.ffaFix() || hyp(zx-x, zy-y) <= 2 {
		return zx, zy
	}
	key := [3]float64{z.NextCenter[0], z.NextCenter[1], z.NextRadius}
	if b.zgKey == key && hyp(b.zg[0]-zx, b.zg[1]-zy) < 30 {
		return b.zg[0], b.zg[1]
	}
	if armZoneFix >= 2 && b.grid != nil && !b.grid.reachable(zx, zy) {
		zx, zy = b.walkableOnCircle(z.NextCenter, zx, zy)
	}
	b.zg, b.zgKey = [2]float64{zx, zy}, key
	return zx, zy
}

// walkableOnCircle: the main-area cell nearest (zx, zy) around the circle through it about c, or a
// little inside it (up to 0.6 rad either way). Returns (zx, zy) if there is none.
func (b *Agent) walkableOnCircle(c []float64, zx, zy float64) (float64, float64) {
	a0 := math.Atan2(zy-c[1], zx-c[0])
	r := hyp(zx-c[0], zy-c[1])
	for k := 0; k <= 24; k++ {
		a := a0 + float64((k+1)/2)*0.05*float64(1-2*(k%2))
		for _, rr := range [3]float64{r, r - 6, r - 12} {
			px, py := c[0]+math.Cos(a)*rr, c[1]+math.Sin(a)*rr
			if rr > 0 && b.grid.reachable(px, py) {
				return px, py
			}
		}
	}
	return zx, zy
}

// ---------------------------------------------------------------------------
// Movement

// goTo returns a unit move direction from our position towards (tx, ty),
// going straight when possible and following a cached A* route otherwise.
func (b *Agent) goTo(me *Self, tx, ty float64) (float64, float64) {
	x, y := me.Pos[0], me.Pos[1]
	d := hyp(tx-x, ty-y)
	if d < 0.8 {
		return 0, 0
	}
	dir := func(px, py float64) (float64, float64) {
		n := hyp(px-x, py-y)
		if n < 1e-6 {
			return 0, 0
		}
		return (px - x) / n, (py - y) / n
	}
	g := b.grid
	if g == nil {
		return dir(tx, ty)
	}
	if me.Pos[2]-b.world.groundH(x, y) > 3 && g.blocked[g.cell(x, y)] {
		return dir(tx, ty) // up on something unwalkable: just head there
	}
	if d < 45 && g.lineOK(x, y, tx, ty) {
		return dir(tx, ty)
	}
	if b.route == nil || b.routeGoal == nil || hyp(tx-b.routeGoal[0], ty-b.routeGoal[1]) > 5 ||
		b.now-b.routeAt > navRefresh {
		b.route = g.route(x, y, tx, ty)
		b.routeGoal, b.routeAt, b.routeI = &[2]float64{tx, ty}, b.now, 0
	}
	if len(b.route) == 0 {
		return dir(tx, ty)
	}

	// Re-sync to the closest route node in a short window ahead.
	best, bi := 1e18, b.routeI
	for i := b.routeI; i < min(len(b.route), b.routeI+10); i++ {
		px, py := g.pos(b.route[i])
		if d := hyp(px-x, py-y); d < best {
			best, bi = d, i
		}
	}
	b.routeI = bi
	if best > 12 {
		b.route = nil
		return dir(tx, ty)
	}
	// Cut corners: head for the furthest node we can walk to directly.
	last := len(b.route) - 1
	for j := min(bi+14, last); j > bi; j-- {
		px, py := g.pos(b.route[j])
		if g.lineOK(x, y, px, py) {
			if j == last && hyp(px-tx, py-ty) < 4 && g.lineOK(x, y, tx, ty) {
				return dir(tx, ty)
			}
			return dir(px, py)
		}
	}
	if bi+1 < len(b.route) {
		return dir(g.pos(b.route[bi+1]))
	}
	return dir(tx, ty)
}

func held(s *Self) map[string]*Weapon {
	m := map[string]*Weapon{}
	for _, w := range s.Weapons {
		if w != nil {
			m[w.Kind] = w
		}
	}
	return m
}

// prefRange is the distance we want to keep from target t.
func (b *Agent) prefRange(me *Self, t *Contact) float64 {
	w := held(me)
	r := b.P.gunRange
	if _, ok := w["lightning"]; ok && me.Energy > 12 {
		r = b.P.lightRange
	} else if _, ok := w["laser"]; ok {
		r = b.P.laserRange
	}
	if t.kind == "npc" {
		return 58
	}
	if _, ok := w["lightning"]; !ok {
		if t.has("lightning") {
			r = math.Max(r, 64)
		}
		if t.has("mortar") {
			r = math.Max(r, 80)
		}
	}
	return r
}

// combatStep picks a move direction while engaging target t.
// aggr > 0 means we want line of sight (attack), aggr < 0 means we want to break it.
// foes are the other known enemies (their sight lines are penalised).
func (b *Agent) combatStep(o *Obs, t *Contact, aggr float64, foes []*Contact) (float64, float64) {
	me := &o.Self
	x, y := me.Pos[0], me.Pos[1]
	g := b.grid
	want := b.prefRange(me, t)
	dx, dy := t.pos[0]-x, t.pos[1]-y
	d := hyp(dx, dy) + 1e-9
	ux, uy := dx/d, dy/d
	if b.now >= b.strafeUntil {
		if b.rng.random() < 0.6 {
			b.strafe = -b.strafe
		}
		b.strafeUntil = b.now + 0.3 + 0.7*b.rng.random()
	}
	sx, sy := -uy*b.strafe, ux*b.strafe
	tc := b.center(t)
	mh := get(me.Stats, "muzzle_height", 7)

	// Nearest cube on our level within 40: drift towards it.
	var cube *thing
	for _, c := range b.cubes.values() {
		cd := hyp(c.pos[0]-x, c.pos[1]-y)
		if cd < 40 && math.Abs(c.pos[2]-me.Pos[2]) < 4 &&
			(cube == nil || hyp(cube.pos[0]-x, cube.pos[1]-y) > cd) {
			cube = c
		}
	}

	bestS := math.Inf(-1)
	bdx, bdy := 0.0, 0.0
	strafeBlocked := false
	rings := 1
	if b.P.peek {
		rings = 2
	}
	for k := -1; k < rings*16; k++ {
		// k == -1 is "stay put"; otherwise 16 directions on rings of radius 6 and 12.
		px, py, r, cx, cy := x, y, 0.0, 0.0, 0.0
		if k >= 0 {
			a := float64(k%16) * math.Pi / 8
			r = float64(k/16+1) * 6
			cx, cy = math.Cos(a), math.Sin(a)
			px, py = x+cx*r, y+cy*r
			if g != nil && (!g.lineOK(x, y, px, py) || !g.free(px, py)) {
				if k < 16 && sx*cx+sy*cy > 0.9 {
					strafeBlocked = true
				}
				continue
			}
		}
		pz := me.Pos[2]
		if g != nil {
			pz = g.walkAt(px, py)
		}
		td := hyp(t.pos[0]-px, t.pos[1]-py)
		s := -1.1*math.Abs(td-want) - 0.4*r
		vis := b.clear([3]float64{px, py, pz + mh}, tc)
		if vis {
			s += 45 * aggr
		}
		// Peeking: prefer spots one step away from flipping visibility.
		if b.P.peek && k < 16 && vis == (aggr > 0) && g != nil {
			for j := 0; j < 16; j += 2 {
				qx, qy := px+7*math.Cos(float64(j)*math.Pi/8), py+7*math.Sin(float64(j)*math.Pi/8)
				if g.free(qx, qy) && b.clear([3]float64{qx, qy, g.walkAt(qx, qy) + mh}, tc) != vis {
					s += 8
					break
				}
			}
		}
		body := [3]float64{px, py, pz + 0.6*get(me.Stats, "height", 9)}
		for _, f := range foes {
			if f == t || hyp(f.pos[0]-px, f.pos[1]-py) > 170 {
				continue
			}
			if b.clear(b.eyeOf(f), body) {
				s -= 30
			}
		}
		s -= b.zonePenalty(&o.Zone, px, py)
		if cube != nil {
			s += 1.5 * (hyp(cube.pos[0]-x, cube.pos[1]-y) - hyp(cube.pos[0]-px, cube.pos[1]-py))
		}
		if g != nil {
			if c := g.clr[g.cell(px, py)]; c < 4 {
				s -= 2.5 * (4 - c)
			}
		}
		if aggr > 0 {
			s += 9 * (sx*cx + sy*cy)
		}
		if k < 0 {
			s -= 8
			if aggr < 0 && !vis {
				s += 14
			}
		}
		if s > bestS {
			bestS, bdx, bdy = s, cx, cy
		}
	}
	if strafeBlocked {
		b.strafe = -b.strafe
		b.strafeUntil = b.now + 0.5
	}
	return bdx, bdy
}

// takeCover looks for a nearby spot hidden from the closest enemies and moves there.
// It returns ok=false when no cover is available.
func (b *Agent) takeCover(o *Obs, foes []*Contact) (float64, float64, bool) {
	me := &o.Self
	x, y := me.Pos[0], me.Pos[1]
	g := b.grid
	if g == nil {
		return 0, 0, false
	}
	var near []*Contact
	for _, f := range foes {
		if hyp(f.pos[0]-x, f.pos[1]-y) < 230 {
			near = append(near, f)
		}
	}
	sort.SliceStable(near, func(i, j int) bool {
		return hyp(near[i].pos[0]-x, near[i].pos[1]-y) < hyp(near[j].pos[0]-x, near[j].pos[1]-y)
	})
	if len(near) > 4 {
		near = near[:4]
	}
	if len(near) == 0 {
		return 0, 0, false
	}
	h := get(me.Stats, "height", 9)
	eyes := make([][3]float64, len(near))
	for i, f := range near {
		eyes[i] = b.eyeOf(f)
	}
	hidden := func(px, py float64) bool {
		z := g.walkAt(px, py)
		for _, e := range eyes {
			if b.clear(e, [3]float64{px, py, z + h}) || b.clear(e, [3]float64{px, py, z + 0.5*h}) {
				return false
			}
		}
		return true
	}

	// Re-use the last cover goal for a short while.
	if b.coverOK && b.now-b.coverAt < 0.4 {
		mx, my := b.goToCover(me)
		return mx, my, true
	}
	if !b.coverOK && b.now-b.coverFailAt < 0.5 {
		return 0, 0, false
	}
	b.coverAt = b.now
	if hidden(x, y) && b.zonePenalty(&o.Zone, x, y) == 0 {
		b.coverOK, b.coverGoal = true, [2]float64{x, y}
		return 0, 0, true
	}

	start := g.nearestFree(g.cell(x, y))
	if start < 0 {
		return 0, 0, false
	}
	dist := map[int]int{start: 0}
	q := []int{start}
	best, bestS := -1, math.Inf(1)
	evals := 0
	for i := 0; i < len(q) && evals < 350; i++ {
		c := q[i]
		d := dist[c]
		if d > 50 || (best >= 0 && float64(2*d) > bestS+30) {
			break
		}
		cx, cy := c%g.nx, c/g.nx
		if (cx+cy)%2 == 0 && d > 0 { // evaluate on a checkerboard to save sight tests
			px, py := g.pos(c)
			evals++
			if hidden(px, py) {
				s := float64(2*d) + b.zonePenalty(&o.Zone, px, py)
				m := math.Inf(1)
				for _, f := range near {
					m = math.Min(m, hyp(f.pos[0]-px, f.pos[1]-py))
				}
				s -= 0.25 * math.Min(m, 80)
				if s < bestS {
					best, bestS = c, s
				}
			}
		}
		for k, dd := range dirs {
			nx, ny := cx+dd[0], cy+dd[1]
			if nx < 0 || nx >= g.nx || ny < 0 || ny >= g.ny {
				continue
			}
			n := ny*g.nx + nx
			if _, ok := dist[n]; ok || !g.stepOK(c, n, k >= 4) {
				continue
			}
			if k >= 4 && (g.blocked[cy*g.nx+nx] || g.blocked[ny*g.nx+cx]) {
				continue
			}
			dist[n] = d + 1
			q = append(q, n)
		}
	}
	if best < 0 {
		b.coverOK = false
		b.coverFailAt = b.now
		return 0, 0, false
	}
	px, py := g.pos(best)
	b.coverOK, b.coverGoal = true, [2]float64{px, py}
	mx, my := b.goToCover(me)
	return mx, my, true
}

func (b *Agent) goToCover(me *Self) (float64, float64) {
	if hyp(b.coverGoal[0]-me.Pos[0], b.coverGoal[1]-me.Pos[1]) < 1.5 {
		return 0, 0
	}
	return b.goTo(me, b.coverGoal[0], b.coverGoal[1])
}

// wantPickup decides whether a weapon pickup of this kind is an upgrade.
func (b *Agent) wantPickup(me *Self, kind string) bool {
	ws := me.Weapons
	if kind == "gun" {
		return len(ws) > 0 && ws[0] == nil
	}
	for _, w := range ws {
		if w != nil && w.Kind == kind {
			return false
		}
	}
	if kind == "mortar" && b.P.noMortar {
		return false
	}
	rank := func(k string) int {
		if b.P.laserFirst && k == "laser" {
			return 4
		}
		return weaponRank[k]
	}
	cur := 0
	if len(ws) > 1 && ws[1] != nil {
		cur = rank(ws[1].Kind)
	}
	return rank(kind) > cur
}

// lead computes yaw/pitch from `from` to a moving target (pos, vel) for a projectile of
// the given speed, iterating the time of flight a few times. flat ignores vertical velocity.
func (b *Agent) lead(from [3]float64, pos, vel []float64, speed float64, flat bool) (float64, float64) {
	if speed <= 0 {
		speed = 252
	}
	t := 0.0
	var px, py, pz float64
	for i := 0; i < 4; i++ {
		t += 0.075
		px, py, pz = pos[0]+vel[0]*t, pos[1]+vel[1]*t, pos[2]
		if !flat {
			pz += vel[2] * t * 0.5
		}
		dx, dy, dz := px-from[0], py-from[1], pz-from[2]
		d := math.Sqrt(dx*dx + dy*dy + dz*dz)
		t = math.Max(0, d-3) / speed
	}
	return math.Atan2(py-from[1], px-from[0]), math.Atan2(pz-from[2], hyp(px-from[0], py-from[1]))
}

// mortar solves the low ballistic arc from `from` onto a (moving) target `to`.
// v is shell speed, g gravity. ok is false if out of range or the arc is obstructed.
func (b *Agent) mortar(from, to [3]float64, vel []float64, stats map[string]float64, v, g float64) (yaw, pitch float64, ok bool) {
	if v <= 0 || g <= 0 {
		return 0, 0, false
	}
	t := 0.0
	for i := 0; i < 3; i++ {
		t += 0.075
		px, py := to[0]+vel[0]*t, to[1]+vel[1]*t
		d := hyp(px-from[0], py-from[1])
		dz := to[2] - from[2]
		disc := math.Pow(v, 4) - g*(g*d*d+2*dz*v*v)
		if disc < 0 || d < 1 {
			return 0, 0, false
		}
		pitch = math.Atan((v*v - math.Sqrt(disc)) / (g * d))
		yaw = math.Atan2(py-from[1], px-from[0])
		t = d / (v * math.Cos(pitch))
	}
	if pitch < get(stats, "min_pitch", -0.7) || pitch > get(stats, "max_pitch", 1.3) {
		return 0, 0, false
	}
	if b.grid != nil {
		cp, sp := math.Cos(pitch), math.Sin(pitch)
		for s := 0.1; s < 0.9*t; s += 0.1 {
			h := cp * v * s
			if b.grid.solid(from[0]+math.Cos(yaw)*h, from[1]+math.Sin(yaw)*h, from[2]+sp*v*s-0.5*g*s*s) {
				return 0, 0, false
			}
		}
	}
	return yaw, pitch, true
}

// dodgeShell simulates incoming mortar shells and sidesteps the predicted impact.
func (b *Agent) dodgeShell(o *Obs) (float64, float64, bool) {
	me := &o.Self
	ws := b.cfg.Weapons["mortar"]
	grav := get(ws, "gravity", 22)
	if grav == 0 {
		grav = 22
	}
	danger := get(ws, "splash_radius", 7) + get(me.Stats, "radius", 2.5) + 2
	g := b.grid
	if g == nil {
		return 0, 0, false
	}
	for _, p := range o.Projectiles {
		if p.Kind != "mortar" {
			continue
		}
		px, py, pz := p.Pos[0], p.Pos[1], p.Pos[2]
		vz := p.Vel[2]
		for t := 0.0; t < 4; t += 0.05 {
			px += p.Vel[0] * 0.05
			py += p.Vel[1] * 0.05
			pz += vz * 0.05
			vz -= grav * 0.05
			if !g.solid(px, py, pz) {
				continue
			}
			// Impact point found.
			if d := hyp(px-me.Pos[0], py-me.Pos[1]); d < danger && t < 2.5 {
				ax, ay := me.Pos[0]-px, me.Pos[1]-py
				if d < 0.5 {
					ax, ay = -p.Vel[1], p.Vel[0]
					d = hyp(ax, ay)
					if d == 0 {
						d = 1
					}
				}
				ax, ay = ax/d, ay/d
				for _, a := range [5]float64{0, 0.7, -0.7, 1.4, -1.4} {
					c, s := math.Cos(a), math.Sin(a)
					dx, dy := ax*c-ay*s, ax*s+ay*c
					if g.lineOK(me.Pos[0], me.Pos[1], me.Pos[0]+dx*6, me.Pos[1]+dy*6) {
						return dx, dy, true
					}
				}
			}
			break
		}
	}
	return 0, 0, false
}

// dodge sidesteps incoming direct-fire projectiles and charging lasers.
// urgent is true when a laser charge (seen or heard) is aimed at us.
// (wx, wy) is where we mean to go: a laser sidestep works either way, so it takes that side.
func (b *Agent) dodge(o *Obs, wx, wy float64) (dx, dy float64, urgent bool) {
	me := &o.Self
	cx, cy, cz := me.Pos[0], me.Pos[1], me.Pos[2]+0.56*get(me.Stats, "height", 9)
	rad := get(me.Stats, "radius", 2.5) + 1.1
	ax, ay := 0.0, 0.0
	for _, p := range o.Projectiles {
		if p.Kind == "mortar" {
			continue
		}
		vx, vy, vz := p.Vel[0], p.Vel[1], p.Vel[2]
		v2 := vx*vx + vy*vy + vz*vz
		if v2 < 1 {
			continue
		}
		// time of closest approach
		t := ((cx-p.Pos[0])*vx + (cy-p.Pos[1])*vy + (cz-p.Pos[2])*vz) / v2
		if t <= 0 || t > 0.7 {
			continue
		}
		mx, my := p.Pos[0]+vx*t-cx, p.Pos[1]+vy*t-cy
		if hyp(mx, my) >= rad {
			continue
		}
		n := hyp(vx, vy)
		if n < 1 {
			continue
		}
		px, py := -vy/n, vx/n
		if mx*px+my*py > 0 {
			px, py = -px, -py
		}
		ax += px
		ay += py
	}
	for i := range o.Mechs {
		m := &o.Mechs[i]
		if m.Kind != "mech" || !m.Charging {
			continue
		}
		a := math.Atan2(cy-m.Pos[1], cx-m.Pos[0])
		if math.Abs(turn(vec2(m.Aim)[0], a)) > 0.15 {
			continue
		}
		sx, sy := b.sidestep(a, wx, wy)
		ax += 1.5 * sx
		ay += 1.5 * sy
		urgent = true
	}
	if bearing, ok := b.unseenCharge(); ok {
		sx, sy := b.sidestep(bearing+math.Pi, wx, wy)
		ax += 1.5 * sx
		ay += 1.5 * sy
		urgent = true
	}
	if n := hyp(ax, ay); n > 0.01 {
		return ax / n, ay / n, urgent
	}
	return 0, 0, false
}

// sidestep is the unit perpendicular to bearing a on the strafe side, or with armDodgeAlign the
// side that agrees with (wx, wy) when we are going somewhere.
func (b *Agent) sidestep(a, wx, wy float64) (float64, float64) {
	sx, sy := math.Cos(a+b.strafe*(math.Pi/2)), math.Sin(a+b.strafe*(math.Pi/2))
	if armDodgeAlign && b.ffaFix() && (wx != 0 || wy != 0) && sx*wx+sy*wy < 0 {
		sx, sy = -sx, -sy
	}
	return sx, sy
}

// escape handles being stranded off the main walkable area.
// It returns an action ("walk", "wait", "launch", "fly") and a walk target.
func (b *Agent) escape(me *Self) (string, [2]float64) {
	g := b.grid
	if g == nil {
		return "", [2]float64{}
	}
	x, y, z := me.Pos[0], me.Pos[1], me.Pos[2]
	if me.Grounded {
		if z-b.world.groundH(x, y) > 3 && g.blocked[g.cell(x, y)] {
			b.esc, b.escFlying = nil, false
			return "", [2]float64{}
		}
		if !g.offMain(x, y) {
			b.esc, b.escFlying = nil, false
			return "", [2]float64{}
		}
		b.escFlying = false
		if b.esc == nil || b.now-b.escAt > 6 {
			b.esc = g.escapePlan(x, y)
			b.escAt = b.now
			if b.esc == nil {
				return "", [2]float64{}
			}
		}
		if hyp(b.esc.lx-x, b.esc.ly-y) > 2.5 {
			return "walk", [2]float64{b.esc.lx, b.esc.ly}
		}
		if me.Energy < math.Min(70, 0.7*me.MaxEnergy) {
			return "wait", [2]float64{}
		}
		b.escFlying = true
		return "launch", [2]float64{}
	}
	if !b.escFlying || b.esc == nil {
		return "", [2]float64{}
	}
	return "fly", [2]float64{}
}

// early: opening phase of a practice/farm game.
func (b *Agent) early(o *Obs) bool { return !b.notPractice && o.T < 200 && o.Self.Cubes < 10 }

// pickLoot picks the best thing to go and collect (pickup, cube, crate or NPC to farm).
// hpRatio is our current health fraction; busy suppresses crate/NPC farming.
func (b *Agent) pickLoot(o *Obs, hpRatio float64, busy bool) *lootPick {
	me := &o.Self
	x, y := me.Pos[0], me.Pos[1]
	z := &o.Zone
	now := b.now
	cz := b.curZone(z)
	var best *lootPick
	bestS := 0.0
	g := b.grid
	consider := func(kind string, id int, pos []float64, val float64) {
		if until, ok := b.bad[id]; ok && until > now {
			return
		}
		d := hyp(pos[0]-x, pos[1]-y)
		if d > 260 || hyp(pos[0]-cz[0], pos[1]-cz[1]) > cz[2]-6 {
			return
		}
		if now+d*1.4/get(me.Stats, "walk_speed", 11.7)+5 > b.zoneDeadline(z, pos[0], pos[1], 6) {
			return
		}
		if pos[2]-b.world.groundH(pos[0], pos[1]) > 3.2 {
			return // on a roof
		}
		if g != nil {
			if c := g.nearestFree(g.cell(pos[0], pos[1])); c < 0 || g.comp[c] != g.main {
				return
			}
		}
		s := val / (18 + d)
		if id == b.lootID {
			s *= 1.35 // stickiness
		}
		if s > bestS {
			bestS, best = s, &lootPick{kind, id, pos}
		}
	}

	for _, p := range b.pickups.values() {
		r := b.P.lootRadius
		ls := b.P.laserSeek
		if b.notPractice { // in a ranked game it's worth walking further for a laser
			ls = math.Max(ls, 240)
		}
		seek := p.kind == "laser" && ls > r
		if seek {
			r = ls
		}
		if b.wantPickup(me, p.kind) && hyp(p.pos[0]-x, p.pos[1]-y) < r {
			v := 60.0
			if len(me.Weapons) > 1 && me.Weapons[1] == nil {
				v = 130
			}
			if seek {
				v = math.Max(v, 150)
			}
			consider("pickup", p.id, p.pos, v)
		}
	}

	statCap := b.cfg.num(b.cfg.Cubes, "stat_cap", 10)
	cv := 75.0
	if me.Cubes >= statCap {
		cv = 25
	}
	cr := math.Max(60, 0.7*b.P.lootRadius)
	if b.early(o) {
		cr = 150
	}
	var light *Weapon
	for i, w := range me.Weapons {
		if i < 2 && w != nil && w.Kind == "lightning" {
			light = w
		}
	}
	for _, c := range b.cubes.values() {
		if hyp(c.pos[0]-x, c.pos[1]-y) < cr && !b.lightGuarded(c.pos, light, me.Energy) {
			consider("cube", c.id, c.pos, cv)
		}
	}

	farm := b.P.farmHP
	if farm == 0 {
		farm = 0.7
	}
	if b.early(o) {
		farm = 0.4
	}
	farmCap := b.P.farmCubes <= 0 || me.Cubes < b.P.farmCubes // past it: hunt, don't farm
	if hpRatio > farm && !busy && b.P.crateUntil > now && farmCap {
		for _, c := range b.crates.values() {
			if hyp(c.pos[0]-x, c.pos[1]-y) < b.P.lootRadius {
				consider("crate", c.id, c.pos, 0.6*cv)
			}
		}
		if b.P.farmNPC && me.Cubes < statCap {
			for _, n := range b.npcs.values() {
				if now-n.seen < 12 && hyp(n.pos[0]-x, n.pos[1]-y) < b.P.lootRadius {
					consider("npc", n.id, n.pos, 85)
				}
			}
		}
	}

	if best == nil {
		b.lootID = -1
		return nil
	}
	if best.id != b.lootID {
		b.lootID, b.lootSince = best.id, now
	} else if now-b.lootSince > 30 {
		// Took too long: blacklist it for a minute.
		b.bad[best.id] = now + 60
		b.lootID = -1
		return nil
	}
	return best
}

// home picks (and keeps) a safe "home" spot inside the next zone: low exposure,
// away from enemies and drops, not too far from us.
func (b *Agent) home(o *Obs) (float64, float64, bool) {
	nc := o.Zone.NextCenter
	nr := o.Zone.NextRadius
	g := b.grid
	stale := !b.homeSet || hyp(b.homeXY[0]-nc[0], b.homeXY[1]-nc[1]) > 0.8*nr
	if g != nil && (stale || b.now-b.homeAt > 12) {
		b.homeAt = b.now
		bestC := math.Inf(1)
		var best *[2]float64
		w := b.world
		for i := 0; i < 40; i++ {
			a := b.rng.random() * 2 * math.Pi
			r := math.Sqrt(b.rng.random()) * nr * 0.7
			px, py := nc[0]+math.Cos(a)*r, nc[1]+math.Sin(a)*r
			if px <= w.bminx || px >= w.bmaxx || py <= w.bminy || py >= w.bmaxy || !g.reachable(px, py) {
				continue
			}
			if c := b.homeCost(o, px, py); c < bestC {
				bestC, best = c, &[2]float64{px, py}
			}
		}
		if best != nil && (stale || b.homeCost(o, b.homeXY[0], b.homeXY[1])-15 > bestC) {
			b.homeXY, b.homeSet = *best, true
		}
	}
	return b.homeXY[0], b.homeXY[1], b.homeSet
}

func (b *Agent) homeCost(o *Obs, x, y float64) float64 {
	me := &o.Self
	threat := 0.0
	for _, t := range b.tracks.values() {
		if b.now-t.seen < 20 {
			threat += math.Max(0, 80-hyp(t.pos[0]-x, t.pos[1]-y)) / 80
		}
	}
	for _, d := range o.Drops {
		threat += math.Max(0, 60-hyp(d.Pos[0]-x, d.Pos[1]-y)) / 60
	}
	edge := hyp(x-o.Zone.NextCenter[0], y-o.Zone.NextCenter[1]) / math.Max(30, o.Zone.NextRadius)
	exp := b.grid.exposure(x, y)
	return 0.25*hyp(x-me.Pos[0], y-me.Pos[1]) + 120*exp + 40*threat + 10*edge
}

// exploreGoal: during the early practice phase, wander to unvisited 100-unit grid
// points inside the zone (closest first).
func (b *Agent) exploreGoal(o *Obs, hpRatio float64, busy bool) (float64, float64, bool) {
	me := &o.Self
	if b.notPractice || busy || hpRatio <= 0.5 || o.T >= 200 || me.Cubes >= 10 || b.grid == nil {
		return 0, 0, false
	}
	z := &o.Zone
	cz := b.curZone(z)
	r := cz[2] - 30
	n := int(math.Floor(r / 100))
	x, y := me.Pos[0], me.Pos[1]
	var bx, by, bd float64
	found := false
	for i := -n; i <= n; i++ {
		for j := -n; j <= n; j++ {
			px, py := cz[0]+float64(i)*100, cz[1]+float64(j)*100
			if hyp(px-cz[0], py-cz[1]) > r || !b.grid.reachable(px, py) {
				continue
			}
			d := hyp(px-x, py-y)
			if b.now+d*1.4/get(me.Stats, "walk_speed", 11.7)+5 > b.zoneDeadline(z, px, py, 6) {
				continue
			}
			near := false
			for _, p := range b.trail {
				if hyp(px-p[0], py-p[1]) < 70 {
					near = true
					break
				}
			}
			if near {
				continue
			}
			d = hyp(px-x, py-y)
			if !found || d < bd {
				bx, by, bd, found = px, py, d, true
			}
		}
	}
	return bx, by, found
}

// seekGoal (practice stance, Tuning.seek > 0): with nothing to pick up and health to spare, head for
// the last known place of the nearest mech seen recently. Practice is scored by how many house bots
// are dead before us, and waiting for the next one to wander into view wastes the time in between.
func (b *Agent) seekGoal(o *Obs, hpR float64) (float64, float64, bool) {
	if b.P.seek <= 0 || !b.pracOn || hpR < b.P.seekHP {
		return 0, 0, false
	}
	me := &o.Self
	x, y := me.Pos[0], me.Pos[1]
	walk := get(me.Stats, "walk_speed", 11.7)
	var best *Contact
	bd := math.Inf(1)
	for _, t := range b.tracks.values() {
		if b.now-t.seen > b.P.seek {
			continue
		}
		if until, ok := b.seekBan[t.id]; ok && b.now < until {
			continue
		}
		d := hyp(t.pos[0]-x, t.pos[1]-y)
		if d > b.P.seekRange {
			continue
		}
		if d < 22 && t.seen != b.now {
			b.seekBan[t.id] = b.now + 20 // we are where it was and it is not here
			continue
		}
		if b.now+d*1.4/walk+8 > b.zoneDeadline(&o.Zone, t.pos[0], t.pos[1], 6) {
			continue // the zone would close on us there
		}
		if b.grid != nil && !b.grid.reachable(t.pos[0], t.pos[1]) {
			continue // on a roof or somewhere we cannot walk to
		}
		if d < bd {
			best, bd = t, d
		}
	}
	if best == nil {
		return 0, 0, false
	}
	return best.pos[0], best.pos[1], true
}

// ghostTrack is a fake target placed 80 units along the bearing we were last hit from.
func (b *Agent) ghostTrack(me *Self) *Contact {
	return &Contact{id: -1, kind: "mech", seen: b.now, hitMe: -100,
		pos: []float64{me.Pos[0] + 80*math.Cos(b.hurtBearing), me.Pos[1] + 80*math.Sin(b.hurtBearing), me.Pos[2]},
		vel: []float64{0, 0, 0}, aim: []float64{0, 0}}
}

// Act is the per-tick entry point: it never panics and never emits NaN/Inf.
func (b *Agent) Act(o *Obs, prevAim [2]float64) (act Action) {
	act.Tick = o.Tick
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintln(os.Stderr, "decide failed:", r)
			act = Action{Tick: o.Tick, Aim: prevAim}
		}
		if math.IsNaN(act.Aim[0]) || math.IsInf(act.Aim[0], 0) || math.IsNaN(act.Aim[1]) || math.IsInf(act.Aim[1], 0) {
			act.Aim = prevAim
		}
		if math.IsNaN(act.Move[0]) || math.IsInf(act.Move[0], 0) || math.IsNaN(act.Move[1]) || math.IsInf(act.Move[1], 0) {
			act.Move = [2]float64{}
		}
	}()
	b.decide(o, &act)
	if debug {
		fmt.Fprintf(os.Stderr, "t=%.1f mode=%s move=%.2f,%.2f aim=%.2f\n", o.T, b.mode, act.Move[0], act.Move[1], act.Aim[0])
	}
	return
}

// Upgrade preference order when the server offers a choice (lower index = better).
var upgrades = []string{"plating", "armor", "nanites", "servos", "jets", "gun_damage", "reactor", "gun_rate",
	"lightning_efficiency", "lightning_range", "laser_damage", "laser_charge", "gun_mag", "gun_velocity",
	"capacitor", "gyros", "lightning_chain", "mortar_reload", "mortar_splash", "mortar_velocity", "sensors", "laser_pierce"}

// Same, but prioritising laser upgrades (used when Tuning.laserUp is set).
var laserUpgrades = []string{"plating", "armor", "nanites", "servos", "jets", "laser_damage", "laser_charge", "reactor",
	"gun_damage", "gun_rate", "lightning_efficiency", "lightning_range", "gun_mag", "gun_velocity",
	"capacitor", "gyros", "lightning_chain", "mortar_reload", "mortar_splash", "mortar_velocity", "sensors", "laser_pierce"}

// step is the per-tick brain: update memory, pick a target, pick a movement mode,
// then aim/fire, reload and pick things up.
func (b *Agent) decide(o *Obs, act *Action) {
	b.now = o.T
	now := o.T
	b.perceive(o)
	b.listen(o)
	me := &o.Self
	if len(b.trail) == 0 || hyp(b.trail[len(b.trail)-1][0]-me.Pos[0], b.trail[len(b.trail)-1][1]-me.Pos[1]) > 40 {
		b.trail = append(b.trail, [2]float64{me.Pos[0], me.Pos[1]})
	}
	stats := me.Stats
	x, y, z := me.Pos[0], me.Pos[1], me.Pos[2]
	zone := &o.Zone
	hpR := me.HP / math.Max(1, me.MaxHP)

	// --- upgrade offer -------------------------------------------------------
	if o.Offer != nil && len(o.Offer.Choices) > 0 {
		order := upgrades
		if b.P.laserUp {
			order = laserUpgrades
		}
		pick := 0
		best := 1 << 30
		for i, c := range o.Offer.Choices {
			r := len(order)
			for j, id := range order {
				if id == c.ID {
					r = j
					break
				}
			}
			if r < best {
				best, pick = r, i
			}
		}
		act.Pick = &pick
	}

	var gun, light, laser *Weapon
	for i, w := range me.Weapons {
		if w == nil || i >= 2 {
			continue
		}
		switch w.Kind {
		case "gun":
			gun = w
		case "lightning":
			light = w
		case "laser":
			laser = w
		}
	}
	endgame := o.Zone.NextRadius <= 70 || o.Alive <= 3

	// --- target selection ----------------------------------------------------
	bestS := math.Inf(1)
	maxT := b.P.maxTarget
	if maxT == 0 {
		maxT = 215
	}
	hpw := b.P.hpw
	if hpw == 0 {
		hpw = 0.3 / 7
	}
	var vis []*Contact     // enemies seen in the last 2.5s
	var tgt, near *Contact // best visible target / closest hidden engaging enemy
	for _, t := range b.tracks.values() {
		if now-t.seen > 2.5 {
			continue
		}
		t.engage = false
		vis = append(vis, t)
		if t.seen != now {
			t.los = false
			continue
		}
		facing := math.Abs(turn(t.aim[0], math.Atan2(y-t.pos[1], x-t.pos[0]))) < 0.22
		if b.P.ghost {
			// Outside practice I don't want to commit to someone who's just wandering
			// nearby — they have to be looking my way or actually coming at me.
			t.engage = t.dist < b.P.engageDist && (!b.notPractice || facing || t.closing > 1) ||
				now-t.hitMe < b.revenge() || facing && t.charging && t.dist < 120 ||
				t.hp < 350 && t.dist < 120 || o.Alive <= 2
		} else if t.dist < b.P.engageDist || now-t.hitMe < 6 || endgame {
			t.engage = true
		} else if t.closing > 3 && t.dist < 180 {
			t.engage = true
		} else if t.hp < 0.4*t.maxHP && t.dist < 160 {
			t.engage = true
		} else if facing && t.dist < 190 {
			t.engage = true
		} else if b.P.huntRange > 0 && t.los && t.dist < b.P.huntRange && hpR > b.P.huntHP {
			// practice: shoot it rather than wait for it to turn our way. Only with a clear line of
			// fire: an engaged mech behind a wall counts as a threat and would stop us looting.
			t.engage = true
		}
		if t.engage && !t.los && t.dist < maxT && (near == nil || near.dist > t.dist) {
			near = t
		}
		if !t.engage || !t.los || t.dist > maxT {
			continue
		}
		s := t.dist + hpw*t.hp
		if b.P.lowBonus != 0 && t.hp <= 0.3*t.maxHP {
			s -= b.P.lowBonus
		}
		if b.P.stick != 0 && b.stickID == t.id && now-b.stickAt < 2 {
			s -= b.P.stick
		}
		if now-t.hitMe < 4 {
			s -= 40
		}
		if b.backTo(t, x, y) {
			s -= 40
		}
		if t.closing > 3 {
			s -= 15
		}
		if s < bestS {
			bestS, tgt = s, t
		}
	}
	if tgt != nil {
		b.stickID, b.stickAt = tgt.id, now
	}
	foe := tgt
	if foe == nil && b.P.peek {
		foe = near
	}

	n, threat := 0, false
	for _, t := range vis {
		if t.engage || now-t.hitMe < 4 {
			threat = true
		}
		if t.seen == now && (t.engage || t.los && t.dist < 150) {
			n++
		}
	}
	if threat || now-b.lastHurt < 3 {
		b.lastThreat = now
	}
	strong := tgt != nil && tgt.dist < 120 && (tgt.hp < 200 || tgt.hp < 0.5*me.HP)
	kite := tgt != nil && tgt.has("laser") && laser == nil && tgt.dist > 55 && hpR < 0.8 && !b.laserSpent(tgt)
	crowd := b.P.crowdHide && (n >= 3 || n == 2 && hpR < 0.65 || kite)
	if !b.retreat && (hpR < b.P.retreatHP && now-b.lastThreat < 2 || crowd) {
		b.retreat, b.coverAt = true, -1
	}
	if b.retreat && !crowd && (hpR > b.P.recoverHP || now-b.lastThreat > 7 && hpR > 0.55) {
		b.retreat = false
	}
	if b.P.loseRule {
		n2 := 0
		for _, t := range vis {
			if t.seen == now && t.dist < 160 {
				n2++
			}
		}
		lose := n2 >= 2 && hpR < 0.55 || n2 >= 3 && hpR < 0.8
		if tgt != nil {
			th := tgt.hp / math.Max(1, tgt.maxHP)
			if hpR < 0.35 && th > hpR+0.1 {
				lose = true
			}
			if th < 0.25 && hpR > 0.2 && n2 <= 2 {
				lose = false
			}
		}
		if lose {
			b.loseAt = now
		}
		b.retreat = now-b.loseAt < 1.5 && b.loseAt > 0
	}

	// --- movement ------------------------------------------------------------
	zx, zy := b.holdZoneGoal(zone, x, y)
	zd := hyp(zx-x, zy-y)
	eta := zd*1.45/get(stats, "walk_speed", 11.7) + 4
	dl := b.zoneDeadline(zone, x, y, 8)
	urgent := !me.InZone || now+eta+3 >= dl
	arrive := now + eta

	var mode string
	var mx, my float64
	boost, launch := false, false
	var aimTrack *Contact // NPC we want to shoot (loot farming)
	var aimPt *[3]float64 // fixed point we want to shoot (crate)

	if escAct, escTo := b.escape(me); escAct != "" {
		switch escAct {
		case "walk":
			mx, my = b.goTo(me, escTo[0], escTo[1])
		case "launch", "fly":
			launch = escAct == "launch"
			tx, ty := b.esc.mx, b.esc.my
			dx, dy := tx-x, ty-y
			if d := hyp(dx, dy); d >= 1.5 {
				ux, uy := dx/d, dy/d
				top := math.Inf(-1)
				for i := 0; i <= int(math.Min(d, 30)); i++ {
					top = math.Max(top, b.grid.topAt(x+float64(i)*ux, y+float64(i)*uy))
				}
				mx, my = ux, uy
				boost = top+2.5 > z && me.Energy > 1
			}
		}
		mode = "escape"
	} else if b.unstuckUntil > now {
		mx, my = b.unstuckDir[0], b.unstuckDir[1]
		mode = "unstuck"
	} else if dx, dy, ok := b.dodgeShell(o); ok {
		mx, my, boost, mode = dx, dy, me.Energy > 15, "dodge"
	} else if urgent && zd > 2 {
		mx, my = b.goTo(me, zx, zy)
		boost = !(me.InZone && arrive < dl) && me.Energy > 5
		mode = "zone"
	} else if lt := b.lightningThreat(vis, light, endgame, hpR, me.Energy); lt != nil {
		// A lightning mech is on top of us and we can't answer in kind — peel away.
		mx, my = b.kiteAway(o, lt)
		boost = me.Energy > 5
		mode = "kite"
	} else if dx, dy, ok := b.duckGunOnly(o, vis, foe, laser == nil && light == nil, endgame || strong); ok {
		// Nothing but the gun against a far-off target: break line of sight instead
		// of plinking away and giving them a free read on us.
		mx, my = dx, dy
		boost = me.Energy > 30 && (mx != 0 || my != 0)
		mode = "duck"
	} else if b.retreat && !strong && !endgame {
		if cx, cy, ok := b.takeCover(o, vis); ok {
			mx, my = cx, cy
			boost = me.Energy > 10 && (mx != 0 || my != 0)
			mode = "cover"
		} else if tgt != nil {
			mx, my = b.combatStep(o, tgt, 1, vis)
			mode = "cornered"
		} else {
			mx, my = b.goTo(me, zx, zy)
			if b.npcHurt > 0 && now-b.npcHurt < 3 && mx == 0 && my == 0 {
				// circle-strafe an NPC that is hitting us
				s := 1.0
				if math.Mod(now, 3) > 1.5 {
					s = -1
				}
				mx, my = -math.Sin(b.npcBearing)*s, math.Cos(b.npcBearing)*s
			}
			mode = "flee"
		}
	} else if cx, cy, ok := b.ghostCover(o, me, vis, tgt, endgame, now); ok {
		mx, my = cx, cy
		boost = me.Energy > 30 && (mx != 0 || my != 0)
		mode = "cover"
	} else if foe != nil {
		gunOK := gun != nil && opt(gun.Ammo, 0) > 0 && opt(gun.Reload, 0) <= 0.25 && foe.dist <= gun.Stats["range"]-10
		laserOK := laser != nil && opt(laser.Charge, 0) >= laser.Stats["charge"]-0.3 && foe.dist <= laser.Stats["range"]-5
		armed := gunOK || laserOK ||
			light != nil && foe.dist <= light.Stats["range"]+6 && me.Energy >= 3*light.Stats["energy"]
		if !b.P.peek && (light != nil || laser != nil) {
			armed = true
		}
		aggr := -1.0
		if armed {
			aggr = 1
		}
		if b.P.peek && !laserOK {
			// someone is charging a laser at us: hide harder
			for _, t := range vis {
				if t.seen == now && t.charging && math.Abs(turn(t.aim[0], math.Atan2(y-t.pos[1], x-t.pos[0]))) < 0.3 {
					aggr = -1.6
				}
			}
		}
		mx, my = b.combatStep(o, foe, aggr, vis)
		boost = light != nil && foe.dist > 50 && me.Energy > 55 && aggr > 0
		if foe == tgt && foe.dist > 30 && me.Energy > 40 {
			boost = true
		}
		mode = "fight"
		if b.P.fightLoot > 0 {
			r := b.P.fightLoot
			if hpR <= 0.5 {
				r *= 0.57
			}
			var cp []float64
			for _, c := range b.cubes.values() {
				if d := hyp(c.pos[0]-x, c.pos[1]-y); d < r && !b.lightGuarded(c.pos, light, me.Energy) {
					r, cp = d, c.pos
				}
			}
			if cp != nil {
				mx, my = b.goTo(me, cp[0], cp[1])
				mode = "fight:cube"
			}
		}
	} else if now+eta+14 >= dl && zd > 2 {
		mx, my = b.goTo(me, zx, zy)
		mode = "zone"
	} else if lp := b.pickLoot(o, hpR, threat); lp != nil {
		mode = "loot:" + lp.kind
		switch lp.kind {
		case "npc":
			if t, ok := b.npcs.get(lp.id); ok && now-t.seen < 1 && t.los && t.dist < 75 {
				mx, my = b.combatStep(o, t, 1, vis)
				aimTrack = t
			} else {
				mx, my = b.goTo(me, lp.pos[0], lp.pos[1])
			}
		case "crate":
			d := hyp(lp.pos[0]-x, lp.pos[1]-y)
			c := [3]float64{lp.pos[0], lp.pos[1], lp.pos[2] + 0.5*b.cfg.Crates["height"]}
			if d < 50 && b.clear([3]float64{x, y, z + get(stats, "muzzle_height", 7)}, c) {
				if d < 12 { // back off a little so splash doesn't hit us
					mx, my = (x-lp.pos[0])/math.Max(d, 0.1), (y-lp.pos[1])/math.Max(d, 0.1)
				}
				aimPt = &c
			} else {
				mx, my = b.goTo(me, lp.pos[0], lp.pos[1])
			}
		default:
			mx, my = b.goTo(me, lp.pos[0], lp.pos[1])
		}
	} else if sx, sy, ok := b.seekGoal(o, hpR); ok {
		mx, my = b.goTo(me, sx, sy)
		mode = "seek"
	} else if ex, ey, ok := b.exploreGoal(o, hpR, threat); ok {
		mx, my = b.goTo(me, ex, ey)
		mode = "explore"
	} else if zd > 3 && (!b.P.ghost || o.Zone.NextRadius <= 30) {
		mx, my = b.goTo(me, zx, zy)
		mode = "zone-walk"
	} else if b.P.ghost {
		mode = "idle"
		if hx, hy, ok := b.home(o); ok && hyp(hx-x, hy-y) > 3 {
			mx, my = b.goTo(me, hx, hy)
			if !threat && now-b.lastHurt > 6 && !b.openOn {
				s := math.Min(1, 4.6/get(stats, "walk_speed", 11.7)) // sneak
				mx, my = mx*s, my*s
			}
			mode = "home"
		}
	} else {
		mode = "idle"
	}

	if launch {
		act.Jump = true
	}
	if (threat || now-b.lastHurt < 2.5) && (mx != 0 || my != 0) && me.Energy > 30 &&
		mode != "unstuck" && mode != "escape" {
		boost = true
	}
	if b.P.dodge && mode != "unstuck" && mode != "escape" {
		if dx, dy, _ := b.dodge(o, mx, my); dx != 0 || dy != 0 {
			mx, my = 0.4*mx+0.9*dx, 0.4*my+0.9*dy
			if n := hyp(mx, my); n > 0.01 {
				mx, my = mx/n, my/n
			}
			boost = boost || me.Energy > 25
		}
	}
	// stuck detection
	if mode == "escape" {
		b.stuckSince = -1
	} else if (mx != 0 || my != 0) && hyp(me.Vel[0], me.Vel[1]) < 1.2 && me.Grounded {
		if b.stuckSince < 0 {
			b.stuckSince = now
		} else if now-b.stuckSince > 0.7 {
			a := b.rng.random() * 2 * math.Pi
			b.unstuckDir = [2]float64{math.Cos(a), math.Sin(a)}
			b.unstuckUntil = now + 0.5
			b.stuckSince = -1
			b.route = nil
			act.Jump = true
		}
	} else {
		b.stuckSince = -1
	}

	// --- aiming & firing -----------------------------------------------------
	muzzle := [3]float64{x + me.Vel[0]*0.075, y + me.Vel[1]*0.075, z + get(stats, "muzzle_height", 7)}
	aim := [2]float64{me.Aim[0], 0}
	lock := false
	if tgt == nil && b.npcHurt > 0 && now-b.npcHurt < 3 {
		// an NPC is hitting us: lock on to the visible one in that direction
		var npcT *Contact
		bestE := 0.7
		for _, nt := range b.npcs.values() {
			if now-nt.seen > 0.5 || !nt.los || nt.dist > 150 {
				continue
			}
			e := math.Abs(turn(math.Atan2(nt.pos[1]-y, nt.pos[0]-x), b.npcBearing))
			if now-nt.hitMe < 3 {
				e = 0
			}
			if e < bestE {
				bestE, npcT = e, nt
			}
		}
		if npcT != nil {
			aimTrack, aimPt = npcT, nil
		}
		lock = npcT != nil
	}

	var ap, vel []float64 // aim point and its velocity
	var rad, hgt float64  // target size (for aim tolerances)
	if tgt != nil {
		s := b.scale(tgt.cubes)
		rad, hgt = b.cfg.Mech["radius"]*s, b.cfg.Mech["height"]*s
		p := [3]float64{tgt.pos[0], tgt.pos[1], tgt.pos[2] + 0.5*hgt}
		if !b.clear(muzzle, p) {
			p[2] = tgt.pos[2] + 0.85*hgt // aim at the head if the body is covered
		}
		ap, vel = p[:], tgt.vel
	} else if aimTrack != nil {
		rad, hgt = b.cfg.Npc["radius"], b.cfg.Npc["height"]
		ap = []float64{aimTrack.pos[0], aimTrack.pos[1], aimTrack.pos[2] + 0.5*hgt}
		vel = aimTrack.vel
	} else if aimPt != nil {
		rad, hgt = b.cfg.Crates["radius"], b.cfg.Crates["height"]
		p := *aimPt
		ap, vel = p[:], make([]float64, 3)
	}
	if tgt == nil && (b.retreat || hpR < 0.5) && !lock {
		ap = nil
	}

	aimed := false
	if ap != nil {
		aimed = true
		d := hyp(ap[0]-x, ap[1]-y)
		flat := math.Abs(vel[2]) < 0.5
		var gy, gp float64
		gunIn := false
		if gun != nil {
			gy, gp = b.lead(muzzle, ap, vel, gun.Stats["speed"], flat)
			gunIn = d <= 0.96*gun.Stats["range"]
		}
		lp := [3]float64{ap[0] + vel[0]*0.125, ap[1] + vel[1]*0.125, ap[2]}
		dy := math.Atan2(lp[1]-muzzle[1], lp[0]-muzzle[0])
		dp := math.Atan2(lp[2]-muzzle[2], hyp(lp[0]-muzzle[0], lp[1]-muzzle[1]))
		if gun != nil {
			aim = [2]float64{gy, gp}
		} else {
			aim = [2]float64{dy, dp}
		}
		yawTol := math.Atan2(0.75*rad, math.Max(d, 1))
		pitchTol := math.Atan2(0.4*hgt, math.Max(d, 1))
		onTarget := func(yaw, pitch float64) bool {
			ey := math.Max(0, math.Abs(turn(yaw, me.Aim[0]))-get(stats, "turn_rate", 4)*0.05)
			ep := math.Max(0, math.Abs(pitch-me.Aim[1])-get(stats, "pitch_rate", 3)*0.05)
			return ey < yawTol && ep < pitchTol
		}

		for i, w := range me.Weapons {
			if i >= 2 || w == nil {
				continue
			}
			switch w.Kind {
			case "gun":
				if gunIn && opt(w.Ammo, 0) > 0 && opt(w.Reload, 0) <= 0 && onTarget(gy, gp) {
					act.Fire[i] = true
				}
			case "lightning":
				cone := get(w.Stats, "cone_deg", 30) * math.Pi / 180
				if d < w.Stats["range"]-1.5 && math.Abs(turn(dy, me.Aim[0])) < 0.5*cone-0.05 && me.Energy >= w.Stats["energy"] {
					act.Fire[i] = true
				}
			case "laser":
				// laser: hold fire to charge, release to shoot
				if tgt == nil || d > 0.97*w.Stats["range"] {
					continue
				}
				full := w.Stats["charge"]
				if b.lethalRelease(me, w, tgt, dy, dp, yawTol, pitchTol) {
					aim = [2]float64{dy, dp}
					act.Fire[i] = false
				} else if opt(w.Charge, 0) < full-0.051 {
					act.Fire[i] = true
					if opt(w.Charge, 0) > 0.45*full {
						aim = [2]float64{dy, dp}
					}
				} else {
					aim = [2]float64{dy, dp}
					release := math.Abs(turn(dy, me.Aim[0])) < 0.9*yawTol && math.Abs(dp-me.Aim[1]) < pitchTol
					act.Fire[i] = !release
				}
			case "mortar":
				if tgt == nil || w.Cooldown > 0.25 || d < 16 || d > 200 {
					continue
				}
				from := [3]float64{muzzle[0], muzzle[1], z + get(stats, "height", 9) + 0.5}
				to := [3]float64{ap[0], ap[1], ap[2] - 0.5*hgt}
				if myaw, mp, ok := b.mortar(from, to, vel, stats, w.Stats["speed"], w.Stats["gravity"]); ok {
					if gun == nil || opt(gun.Reload, 0) > 0 || opt(gun.Ammo, 0) == 0 || !gunIn {
						aim = [2]float64{myaw, mp}
					}
					ey := math.Max(0, math.Abs(turn(myaw, me.Aim[0]))-get(stats, "turn_rate", 4)*0.05)
					ep := math.Max(0, math.Abs(mp-me.Aim[1])-get(stats, "pitch_rate", 3)*0.05)
					if ey < 0.04 && ep < 0.03 {
						act.Fire[i] = true
					}
				}
			}
		}
	}

	// Don't give away our position when hurt and the target is far.
	if tgt != nil && hpR < 0.6 && tgt.dist > 120 && now-b.lastHurt > 5 && tgt.hp > 0.3*tgt.maxHP {
		act.Fire = [2]bool{}
	}
	// Pre-charge the laser while peeking at a hidden enemy.
	if tgt == nil && foe != nil {
		for i, w := range me.Weapons {
			if i < 2 && w != nil && w.Kind == "laser" && w.Cooldown <= 0.05 && w.Stats["range"] > foe.dist {
				if opt(w.Charge, 0) < get(w.Stats, "hold_s", 2)+w.Stats["charge"]-0.35 {
					act.Fire[i] = true
				}
			}
		}
	}
	// Nothing to shoot: look somewhere useful.
	if !aimed {
		var nt *Contact
		for _, t := range vis {
			if !t.engage && now-t.hitMe >= 6 {
				continue
			}
			if nt == nil || t.dist < nt.dist {
				nt = t
			}
		}
		if nt != nil {
			aim[0] = math.Atan2(nt.pos[1]-y, nt.pos[0]-x)
		} else if b.npcHurt > 0 && now-b.npcHurt < 2.5 {
			aim[0] = b.npcBearing
		} else if now-b.lastHurt < 2.5 {
			aim[0] = b.hurtBearing
		} else if b.lastHeard() != nil {
			aim[0] = b.lastHeard().bearing
		} else if mx != 0 || my != 0 {
			aim[0] = math.Atan2(my, mx) + 1.15*math.Sin(1.3*now) // scan while walking
		} else {
			aim[0] = me.Aim[0] + 0.35 // spin in place
		}
		aim[1] = 0
	}
	aim[1] = clamp(aim[1], get(stats, "min_pitch", -0.7), get(stats, "max_pitch", 1.3))
	act.Aim = aim

	// Reload when it's quiet.
	if gun != nil && opt(gun.Reload, 0) <= 0 && opt(gun.Ammo, 0) < gun.Stats["mag"] {
		if tgt == nil && aimTrack == nil && aimPt == nil &&
			(opt(gun.Ammo, 0) <= 0.6*gun.Stats["mag"] || now-b.lastThreat > 3) {
			act.Reload = true
		}
	}

	// Pick up wanted weapons we're standing on.
	sec := ""
	if len(me.Weapons) > 1 && me.Weapons[1] != nil {
		sec = me.Weapons[1].Kind
	}
	pr := b.cfg.num(b.cfg.Loot, "pickup_radius", 4) - 0.3
	for _, p := range o.Pickups {
		if hyp(p.Pos[0]-x, p.Pos[1]-y) > pr || math.Abs(p.Pos[2]-z) > 3.6 {
			continue
		}
		if !b.wantPickup(me, p.Kind) {
			continue
		}
		id := p.ID
		act.Take = &id
		if sec != "" && me.Weapons[0] != nil {
			if sec == "laser" && opt(me.Weapons[1].Charge, 0) > 0 {
				act.Take = nil // don't drop a charging laser
				break
			}
			slot := 1
			act.Drop = &slot
		}
		break
	}

	act.Move = [2]float64{mx, my}
	act.Boost = boost && (mx != 0 || my != 0)
	b.mode = mode
}

// ghostCover: if something unseen just hurt us, take cover from a phantom enemy
// placed along the hit bearing (plus the known foes).
func (b *Agent) ghostCover(o *Obs, me *Self, foes []*Contact, tgt *Contact, busy bool, now float64) (float64, float64, bool) {
	if tgt != nil || now-b.lastHurt >= 2.5 || busy || !b.P.ghost {
		return 0, 0, false
	}
	return b.takeCover(o, append([]*Contact{b.ghostTrack(me)}, foes...))
}

// listen remembers recent sounds that don't come from a visible mech.
func (b *Agent) listen(o *Obs) {
	keep := b.heard[:0]
	for _, h := range b.heard {
		if b.now-h.t < 3 {
			keep = append(keep, h)
		}
	}
	b.heard = keep
	for _, s := range o.Sounds {
		if !knownKinds[s.Kind] {
			continue
		}
		vis := false
		for i := range o.Mechs {
			m := &o.Mechs[i]
			if m.Kind == "mech" &&
				math.Abs(turn(math.Atan2(m.Pos[1]-o.Self.Pos[1], m.Pos[0]-o.Self.Pos[0]), s.Bearing)) < 0.3 {
				vis = true
				break
			}
		}
		if vis {
			continue
		}
		b.heard = append(b.heard, heard{s.Kind, s.Bearing, s.Dist == "far", b.now - s.Age})
	}
}

// lastHeard returns the most recent nearby (not "far") sound from the last 2 seconds.
func (b *Agent) lastHeard() *heard {
	var best *heard
	for i := range b.heard {
		h := &b.heard[i]
		if h.far || b.now-h.t > 2 {
			continue
		}
		if best == nil || h.t > best.t {
			best = h
		}
	}
	return best
}

// unseenCharge reports the bearing of a laser charge we heard in the last second.
func (b *Agent) unseenCharge() (float64, bool) {
	for i := len(b.heard) - 1; i >= 0; i-- {
		h := b.heard[i]
		if h.kind == "laser_charge" && b.now-h.t < 1 {
			return h.bearing, true
		}
	}
	return 0, false
}

// lethalRelease: release a partially charged laser early if it would already kill t.
func (b *Agent) lethalRelease(me *Self, w *Weapon, t *Contact, yaw, pitch, yawTol, pitchTol float64) bool {
	ch, full := opt(w.Charge, 0), w.Stats["charge"]
	if t == nil || full <= 0 || ch < get(w.Stats, "min_charge", 0.15) || ch >= full-0.051 {
		return false
	}
	mf := get(w.Stats, "min_frac", 0.35)
	dmg := (mf + (1-mf)*ch/full) * w.Stats["damage"]
	dmg *= 1 + b.cfg.num(b.cfg.Cubes, "damage_per_cube", 0.04)*math.Min(me.Cubes, b.cfg.num(b.cfg.Cubes, "stat_cap", 10))
	if t.has("mortar") {
		dmg *= 1 + get(w.Stats, "cookoff", 0.6)
	}
	if b.backTo(t, me.Pos[0], me.Pos[1]) {
		dmg *= get(b.cfg.Mech, "rear_mult", 1.5)
	}
	return 0.8*dmg >= t.hp && math.Abs(turn(yaw, me.Aim[0])) < 0.9*yawTol && math.Abs(pitch-me.Aim[1]) < pitchTol
}

// backTo: is (x, y) outside t's frontal arc (i.e. are we hitting its back)?
func (b *Agent) backTo(t *Contact, x, y float64) bool {
	if t == nil || t.kind != "mech" || len(t.aim) == 0 {
		return false
	}
	arc := get(b.cfg.Mech, "front_arc_deg", 180) * math.Pi / 360
	return math.Abs(turn(math.Atan2(y-t.pos[1], x-t.pos[0]), t.aim[0])) > arc+0.17
}

// lightningThreat returns the nearest lightning-carrying enemy we should step out of reach of.
// Lightning reaches 45 m while our gun reaches 230 m, so standing just outside its range costs us
// nothing. With armAntiLight this runs in practice too (the squad brings lightning) and keeps a pad
// beyond the configured range; otherwise only in a ranked game inside 60 m.
func (b *Agent) lightningThreat(foes []*Contact, light *Weapon, endgame bool, hpR, energy float64) *Contact {
	if endgame {
		return nil
	}
	if !armAntiLight {
		if !b.notPractice || light != nil {
			return nil
		}
	} else if light != nil && energy > 18 && hpR >= 0.55 {
		return nil
	}
	var best *Contact
	for _, t := range foes {
		if t.seen == b.now && t.has("lightning") && t.dist < b.lightSafe(t) && (best == nil || t.dist < best.dist) {
			best = t
		}
	}
	return best
}

// lightSafe is the distance at which t's lightning can no longer reach us.
func (b *Agent) lightSafe(t *Contact) float64 {
	if !armAntiLight {
		return 60
	}
	r := get(b.cfg.Weapons["lightning"], "range", 45) + 12
	if t.cubes >= 6 {
		r += 6 // it may have the range upgrade
	}
	return r
}

// lightGuarded: a cube at p sits inside the reach of a lightning carrier seen lately, and we cannot
// answer with lightning of our own.
func (b *Agent) lightGuarded(p []float64, light *Weapon, energy float64) bool {
	if !armAntiLight || light != nil && energy > 18 {
		return false
	}
	r := get(b.cfg.Weapons["lightning"], "range", 45) + 8
	for _, t := range b.tracks.values() {
		if b.now-t.seen < 4 && t.has("lightning") && hyp(p[0]-t.pos[0], p[1]-t.pos[1]) < r {
			return true
		}
	}
	return false
}

// laserSpent: in practice, t fired its laser moments ago and cannot fire again for cooldown + charge,
// so there is no laser to run from.
func (b *Agent) laserSpent(t *Contact) bool {
	if !armLaserCD || !b.pracOn || t.charging || t.laserAt <= 0 {
		return false
	}
	w := b.cfg.Weapons["laser"]
	return b.now-t.laserAt < get(w, "cooldown", 1.65)+get(w, "charge", 0.75)-0.4
}

// readBeams notes when each mech fires its laser, and counts beams from one mech that end on another
// mech well away from us: the house squad never fights itself, so that only happens in a free-for-all.
func (b *Agent) readBeams(o *Obs) {
	me := o.Self.Pos
	seen := false
	for _, bm := range o.Beams {
		if bm.By == o.Self.ID || len(bm.Path) < 2 {
			continue
		}
		if t, ok := b.tracks.get(bm.By); ok && bm.Kind == "laser" {
			t.laserAt = b.now - bm.Age
		}
		p0, end := bm.Path[0], bm.Path[len(bm.Path)-1]
		if len(p0) < 2 || len(end) < 2 || hyp(end[0]-me[0], end[1]-me[1]) < 25 || segDist(me[0], me[1], p0[0], p0[1], end[0], end[1]) < 22 {
			continue
		}
		for _, t := range b.tracks.values() {
			if t.id != bm.By && b.now-t.seen < 1 && hyp(t.pos[0]-end[0], t.pos[1]-end[1]) < 5 {
				seen = true
			}
		}
	}
	if seen {
		b.ffaBeams++
	}
}

func segDist(px, py, ax, ay, bx, by float64) float64 {
	dx, dy := bx-ax, by-ay
	l2 := dx*dx + dy*dy
	t := 0.0
	if l2 > 0 {
		t = clamp(((px-ax)*dx+(py-ay)*dy)/l2, 0, 1)
	}
	return hyp(px-ax-t*dx, py-ay-t*dy)
}

// kiteAway walks one 8-unit step away from t. It samples 16 headings and keeps
// the walkable one that points most directly away while staying clear of the zone.
func (b *Agent) kiteAway(o *Obs, t *Contact) (float64, float64) {
	x, y := o.Self.Pos[0], o.Self.Pos[1]
	dx, dy := x-t.pos[0], y-t.pos[1]
	ux, uy := 1.0, 0.0
	if d := hyp(dx, dy); d > 0.1 {
		ux, uy = dx/d, dy/d
	}
	best := math.Inf(-1)
	var bx, by float64
	for k := 0; k < 16; k++ {
		a := float64(k) * math.Pi / 8
		c, s := math.Cos(a), math.Sin(a)
		px, py := x+8*c, y+8*s
		if b.grid != nil && (!b.grid.lineOK(x, y, px, py) || !b.grid.free(px, py)) {
			continue
		}
		sc := ux*c + uy*s - b.zonePenalty(&o.Zone, px, py)/100
		if sc > best {
			best, bx, by = sc, c, s
		}
	}
	if math.IsInf(best, -1) {
		return ux, uy
	}
	return bx, by
}

// duckGunOnly sends us to cover when all we've got is the gun and the target is
// more than 90 units out. Skipped if we're pressing an advantage or in the endgame,
// where sitting behind a wall just wastes the clock.
func (b *Agent) duckGunOnly(o *Obs, foes []*Contact, t *Contact, gunOnly, pressing bool) (float64, float64, bool) {
	if !b.notPractice || !gunOnly || pressing || t == nil || t.dist <= 90 {
		return 0, 0, false
	}
	return b.takeCover(o, foes)
}
