package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"testing"
)

// REPLAY=trace.raw.jsonl T0=160 T1=170 go test -run TestReplay
func TestReplay(t *testing.T) {
	f, err := os.Open(os.Getenv("REPLAY"))
	if err != nil {
		t.Skip("no REPLAY")
	}
	t0, _ := strconv.ParseFloat(os.Getenv("T0"), 64)
	t1, _ := strconv.ParseFloat(os.Getenv("T1"), 64)
	if t1 == 0 {
		t1 = 1e9
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<24), 1<<24)
	var a *Agent
	var last [2]float64
	for sc.Scan() {
		var head struct{ Type string }
		json.Unmarshal(sc.Bytes(), &head)
		if head.Type == "hello" {
			var h Hello
			json.Unmarshal(sc.Bytes(), &h)
			h.Map.Dir = "arena"
			a = NewAgent(&h)
			continue
		}
		if head.Type != "obs" || a == nil {
			continue
		}
		var o Obs
		if json.Unmarshal(sc.Bytes(), &o) != nil {
			continue
		}
		act := a.Act(&o, last)
		last = act.Aim
		if o.T < t0 || o.T > t1 {
			continue
		}
		me := o.Self.Pos
		zx, zy := a.zoneGoal(&o.Zone, me[0], me[1])
		direct := a.grid.lineOK(me[0], me[1], zx, zy)
		rg := [2]float64{}
		if a.routeGoal != nil {
			rg = *a.routeGoal
		}
		vis := 0
		for _, m := range o.Mechs {
			if m.Kind == "mech" {
				vis++
			}
		}
		ddx, ddy, durg := a.dodge(&o, act.Move[0], act.Move[1])
		fmt.Printf("dodge=%.2f,%.2f,%v strafe=%.0f ", ddx, ddy, durg, a.strafe)
		fmt.Printf("t=%.1f mode=%s pos=%.0f,%.0f move=%.2f,%.2f zgoal=%.0f,%.0f zd=%.0f direct=%v route=%d i=%d rgoal=%.0f,%.0f hp=%.0f fire=%v stick=%d retreat=%v vis=%d en=%.0f prac=%v np=%v zone=%.0f,%.0f r=%.0f next=%.0f,%.0f nr=%.0f shrink=%.0f-%.0f\n",
			o.T, a.mode, me[0], me[1], act.Move[0], act.Move[1], zx, zy, hyp(zx-me[0], zy-me[1]), direct, len(a.route), a.routeI, rg[0], rg[1],
			o.Self.HP, act.Fire, a.stickID, a.retreat, vis, o.Self.Energy,
			a.pracOn, a.notPractice, o.Zone.Center[0], o.Zone.Center[1], o.Zone.Radius, o.Zone.NextCenter[0], o.Zone.NextCenter[1], o.Zone.NextRadius, o.Zone.ShrinkStart, o.Zone.ShrinkEnd)
	}
}
