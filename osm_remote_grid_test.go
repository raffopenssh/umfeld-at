package main

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

// TestEDT2D — the squared distance transform against brute force on a small grid.
func TestEDT2D(t *testing.T) {
	w, h := 23, 17
	mark := make([]bool, w*h)
	for _, p := range [][2]int{{3, 4}, {20, 2}, {11, 15}, {0, 0}} {
		mark[p[1]*w+p[0]] = true
	}
	d := edt2D(mark, w, h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			best := math.MaxFloat64
			for _, p := range [][2]int{{3, 4}, {20, 2}, {11, 15}, {0, 0}} {
				dx, dy := float64(x-p[0]), float64(y-p[1])
				if v := dx*dx + dy*dy; v < best {
					best = v
				}
			}
			if math.Abs(float64(d[y*w+x])-best) > 1e-3 {
				t.Fatalf("(%d,%d): edt %v, brute %v", x, y, d[y*w+x], best)
			}
		}
	}
	// no feature at all: everything stays far
	d = edt2D(make([]bool, w*h), w, h)
	if d[0] < 1e9 {
		t.Fatalf("empty grid distance %v", d[0])
	}
}

// TestRemoteGridRoundTrip — At(), save/load, the gain curve.
func TestRemoteGridRoundTrip(t *testing.T) {
	g := newRemoteGrid()
	if g.w < 2000 || g.h < 1000 || len(g.v) != g.w*g.h {
		t.Fatalf("grid %d×%d", g.w, g.h)
	}
	if i, ok := g.idx(16.37, 48.21); ok {
		g.v[i] = 42
	} else {
		t.Fatal("Vienna outside the grid")
	}
	g.v[0] = rgNoData
	if v := g.At(16.37, 48.21); v != 42 {
		t.Fatalf("At = %v", v)
	}
	if v := g.At(rgBox[0], rgBox[1]); v != -1 {
		t.Fatalf("no-data cell = %v, want -1", v)
	}
	if v := g.At(2, 2); v != -1 {
		t.Fatalf("outside = %v", v)
	}
	p := filepath.Join(t.TempDir(), "rg.bin")
	if err := g.save(p); err != nil {
		t.Fatal(err)
	}
	g2, err := remoteGridLoad(p)
	if err != nil {
		t.Fatal(err)
	}
	if g2.At(16.37, 48.21) != 42 || g2.At(rgBox[0], rgBox[1]) != -1 {
		t.Fatal("round trip lost values")
	}
	b, _ := os.ReadFile(p)
	b[5] ^= 1
	os.WriteFile(p, b, 0o644)
	if _, err := remoteGridLoad(p); err == nil {
		t.Fatal("bad version accepted")
	}
	// gain: neutral without a value, monotone, within the documented span
	if neRoadRemoteGain(-1) != 1 {
		t.Fatal("gain(-1) != 1")
	}
	last := 0.0
	for r := float32(0); r <= 100; r += 5 {
		gn := neRoadRemoteGain(r)
		if gn <= last || gn < 0.5 || gn > 1.5 {
			t.Fatalf("gain(%v) = %v", r, gn)
		}
		last = gn
	}
	// the ink: a remote road is heavier and a touch wider than a town road; rail is untouched
	town := neRoadInk(neRoadRoad, neRoadAttr{Remote: 5}, 1)
	far := neRoadInk(neRoadRoad, neRoadAttr{Remote: 90}, 1)
	if far.a <= town.a || far.w <= town.w || far.a > 0.92 {
		t.Fatalf("town %+v far %+v", town, far)
	}
	if neRoadInk(neRoadRail, neRoadAttr{Remote: 90}, 1) != neRoadInk(neRoadRail, neRoadAttr{Remote: -1}, 1) {
		t.Fatal("rail tinted")
	}
	// stroke gain rides in the coverage
	cov := make([]float32, 40*20)
	neRoadStroke(cov, 40, 20, [][2]float64{{2, 10}, {38, 10}}, 1.5, 0, 0, 0.5)
	if c := cov[10*40+20]; math.Abs(float64(c)-0.5) > 1e-3 {
		t.Fatalf("gain .5 → coverage %v", c)
	}
}
