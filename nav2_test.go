package main

import (
	"fmt"
	"os"
	"strconv"
	"testing"
)

// NAV="sx,sy,tx,ty" go test -run TestNavOne -v
func TestNavOne(t *testing.T) {
	var p [4]float64
	n := 0
	for i, s := range splitComma(os.Getenv("NAV")) {
		p[i], _ = strconv.ParseFloat(s, 64)
		n++
	}
	if n < 4 {
		t.Skip()
	}
	w, _ := LoadWorld("arena")
	g := NewGrid(w)
	b := &Agent{world: w, grid: g, routeAt: -99}
	me := &Self{Pos: []float64{p[0], p[1], g.walkAt(p[0], p[1])}, Stats: map[string]float64{}}
	for i := 0; i < 300; i++ {
		b.now = float64(i) * 0.1
		x, y := me.Pos[0], me.Pos[1]
		if hyp(p[2]-x, p[3]-y) < 3 {
			fmt.Println("arrived", b.now)
			return
		}
		mx, my := b.goTo(me, p[2], p[3])
		if i%3 == 0 {
			fmt.Printf("t=%.1f pos=%.1f,%.1f move=%.2f,%.2f route=%d i=%d free=%v clr=%.1f\n", b.now, x, y, mx, my, len(b.route), b.routeI, g.free(x, y), g.clr[g.cell(x, y)])
		}
		step := 11.7 * 0.1
		for _, d := range [][2]float64{{mx, my}, {mx, 0}, {0, my}} {
			nx, ny := x+d[0]*step, y+d[1]*step
			if (d[0] != 0 || d[1] != 0) && g.free(nx, ny) && g.stepOK(g.cell(x, y), g.cell(nx, ny), true) {
				me.Pos[0], me.Pos[1], me.Pos[2] = nx, ny, g.walkAt(nx, ny)
				break
			}
		}
	}
	fmt.Println("not arrived")
}

func splitComma(s string) []string {
	var out []string
	cur := ""
	for _, c := range s {
		if c == ',' {
			out = append(out, cur)
			cur = ""
		} else {
			cur += string(c)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
