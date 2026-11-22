// rover — a mech-arena agent.
//
// This is the outward-facing half of the program: the stdin/stdout loop that
// speaks the server's newline-delimited JSON protocol, together with the wire
// types that protocol is built from. The decision-making lives in strategy.go
// and the map/geometry math in terrain.go.
//
// Author: Sparsh (Team 4141). Copyright (c) 2026, MIT licensed, see LICENSE and NOTICE.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

// main is the whole I/O loop: pull lines off stdin on a background goroutine,
// keep only the freshest observation, hand it to the agent, write the action.
func main() {
	lines := make(chan []byte, 1024)
	go func() {
		r := bufio.NewReaderSize(os.Stdin, 1<<20)
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 1 {
				lines <- line
			}
			if err != nil {
				close(lines)
				return
			}
		}
	}()

	out := bufio.NewWriterSize(os.Stdout, 1<<16)
	send := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			return
		}
		out.Write(b)
		out.WriteByte('\n')
		out.Flush()
	}

	var agent *Agent
	var lastAim [2]float64
	for {
		line, ok := <-lines
		if !ok {
			return
		}
		// Drain the queue so we only ever act on the newest observation.
		batch := [][]byte{line}
	drain:
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					break drain
				}
				batch = append(batch, l)
			default:
				break drain
			}
		}

		var latest []byte
		for _, raw := range batch {
			var head struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(raw, &head) != nil {
				continue
			}
			switch head.Type {
			case "hello":
				var h Hello
				if json.Unmarshal(raw, &h) == nil {
					agent = NewAgent(&h)
				}
				send(map[string]string{"type": "ready", "name": "rover"})
			case "obs":
				latest = raw
			case "end":
				return
			}
		}
		if latest == nil || agent == nil {
			continue
		}
		var o Obs
		if err := json.Unmarshal(latest, &o); err != nil {
			fmt.Fprintln(os.Stderr, "bad obs:", err)
			continue
		}
		act := agent.Act(&o, lastAim)
		lastAim = act.Aim
		send(act)
	}
}

// ---------------------------------------------------------------------------
// Wire types
//
// Everything below mirrors the JSON the server sends and expects back. A few of
// the config sections come through loosely typed, so num/get/opt below are the
// little accessors I use to pull numbers out of them with sane fallbacks.
// ---------------------------------------------------------------------------

// Config holds the per-match settings that arrive with the hello message.
type Config struct {
	Mech    map[string]float64            `json:"mech"`
	Npc     map[string]float64            `json:"npc"`
	Crates  map[string]float64            `json:"crates"`
	Cubes   map[string]json.RawMessage    `json:"cubes"`
	Loot    map[string]json.RawMessage    `json:"loot"`
	Weapons map[string]map[string]float64 `json:"weapons"`
}

// num pulls a number out of a raw-JSON config section, falling back to def.
func (c *Config) num(m map[string]json.RawMessage, key string, def float64) float64 {
	if raw, ok := m[key]; ok {
		var v float64
		if json.Unmarshal(raw, &v) == nil {
			return v
		}
	}
	return def
}

// get reads m[k] with a default.
func get(m map[string]float64, k string, def float64) float64 {
	if v, ok := m[k]; ok {
		return v
	}
	return def
}

// opt dereferences an optional number with a default.
func opt(p *float64, def float64) float64 {
	if p == nil {
		return def
	}
	return *p
}

type Hello struct {
	Type string  `json:"type"`
	Slot int     `json:"slot"`
	Seed float64 `json:"seed"`
	Map  struct {
		Dir string `json:"dir"`
	} `json:"map"`
	Config Config `json:"config"`
}

type Weapon struct {
	Kind     string             `json:"kind"`
	Cooldown float64            `json:"cooldown"`
	Ammo     *float64           `json:"ammo"`
	Reload   *float64           `json:"reload"`
	Charge   *float64           `json:"charge"`
	Stats    map[string]float64 `json:"stats"`
}

type Self struct {
	ID        int                `json:"id"`
	Pos       []float64          `json:"pos"`
	Vel       []float64          `json:"vel"`
	Aim       []float64          `json:"aim"`
	HP        float64            `json:"hp"`
	MaxHP     float64            `json:"max_hp"`
	Energy    float64            `json:"energy"`
	MaxEnergy float64            `json:"max_energy"`
	Grounded  bool               `json:"grounded"`
	Cubes     float64            `json:"cubes"`
	Weapons   []*Weapon          `json:"weapons"`
	InZone    bool               `json:"in_zone"`
	Stats     map[string]float64 `json:"stats"`
}

type Mech struct {
	ID       int       `json:"id"`
	Kind     string    `json:"kind"`
	Pos      []float64 `json:"pos"`
	Vel      []float64 `json:"vel"`
	Aim      []float64 `json:"aim"`
	HP       *float64  `json:"hp"`
	MaxHP    *float64  `json:"max_hp"`
	Cubes    *float64  `json:"cubes"`
	Weapons  []string  `json:"weapons"`
	Charging bool      `json:"charging"`
}

type Proj struct {
	Kind string    `json:"kind"`
	Pos  []float64 `json:"pos"`
	Vel  []float64 `json:"vel"`
}

type Item struct {
	ID   int       `json:"id"`
	Kind string    `json:"kind"`
	Pos  []float64 `json:"pos"`
}

type Event struct {
	Type    string   `json:"type"`
	By      *int     `json:"by"`
	Bearing *float64 `json:"bearing"`
	Weapon  string   `json:"weapon"`
	Target  *int     `json:"target"`
}

type Zone struct {
	Center      []float64 `json:"center"`
	Radius      float64   `json:"radius"`
	NextCenter  []float64 `json:"next_center"`
	NextRadius  float64   `json:"next_radius"`
	ShrinkStart float64   `json:"shrink_start"`
	ShrinkEnd   float64   `json:"shrink_end"`
}

type Offer struct {
	Choices []struct {
		ID string `json:"id"`
	} `json:"choices"`
}

type Drop struct {
	ID      int       `json:"id"`
	Pos     []float64 `json:"pos"`
	Radius  float64   `json:"radius"`
	LandsIn float64   `json:"lands_in"`
	Holder  *int      `json:"holder"`
}

type Sound struct {
	Kind    string  `json:"kind"`
	Bearing float64 `json:"bearing"`
	Dist    string  `json:"dist"`
	Age     float64 `json:"age"`
}

// Beam is a laser beam or lightning arc fired by another mech that we could see.
type Beam struct {
	By   int         `json:"by"`
	Kind string      `json:"kind"`
	Path [][]float64 `json:"path"`
	Age  float64     `json:"age"`
}

// Obs is one observation frame from the server.
type Obs struct {
	Type        string  `json:"type"`
	Tick        int     `json:"tick"`
	T           float64 `json:"t"`
	Alive       int     `json:"alive"`
	Self        Self    `json:"self"`
	Mechs       []Mech  `json:"mechs"`
	Projectiles []Proj  `json:"projectiles"`
	Crates      []Item  `json:"crates"`
	Cubes       []Item  `json:"cubes"`
	Pickups     []Item  `json:"pickups"`
	Events      []Event `json:"events"`
	Zone        Zone    `json:"zone"`
	Offer       *Offer  `json:"offer"`
	Drops       []Drop  `json:"drops"`
	Sounds      []Sound `json:"sounds"`
	Beams       []Beam  `json:"beams"`
}

// Action is what we send back each tick.
type Action struct {
	Tick   int        `json:"tick"`
	Move   [2]float64 `json:"move"`
	Aim    [2]float64 `json:"aim"`
	Fire   [2]bool    `json:"fire"`
	Boost  bool       `json:"boost"`
	Jump   bool       `json:"jump"`
	Reload bool       `json:"reload,omitempty"`
	Pick   *int       `json:"pick,omitempty"`
	Take   *int       `json:"take,omitempty"`
	Drop   *int       `json:"drop,omitempty"`
}
