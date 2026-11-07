package main

import (
	"strings"
	"testing"
)

// An open chain whose walk starts mid-chain must come out as ONE polyline (M a L b L c … Z), never as
// fragments each closed by a chord — those chords were the straight-line wedges in the veil (2026-11-07).
func TestNEFltVecOpenChainOnePolyline(t *testing.T) {
	v := &neFltVec{edges: make(map[[2]neFltVecPt]int)}
	pts := [][2]float64{{0, 0}, {10, 0}, {20, 5}, {30, 0}, {40, 5}, {50, 0}}
	// add out of order so the walk begins in the middle
	order := []int{2, 0, 4, 1, 3}
	for _, k := range order {
		v.add(pts[k][0], pts[k][1], pts[k+1][0], pts[k+1][1], false)
	}
	var sb strings.Builder
	v.loops(&sb)
	d := sb.String()
	if strings.Count(d, "M") != 1 || strings.Count(d, "L") != 5 {
		t.Fatalf("want one polyline of 6 points, got %q", d)
	}
}
