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
	var sb, st strings.Builder
	v.loops(&sb, &st)
	d := sb.String()
	if strings.Count(d, "M") != 1 || strings.Count(d, "L") != 5 {
		t.Fatalf("want one polyline of 6 points, got %q", d)
	}
	if s := st.String(); strings.Count(s, "M") != 1 || strings.Count(s, "L") != 5 {
		t.Fatalf("stroke: want the same polyline, got %q", s)
	}
}

// A loop closed by silent clip edges (matching neighbours beyond the painted box) fills as one closed loop but
// is stroked only along its real outline: no seam along the tile margin (2026-11-07, second artefact).
func TestNEFltVecSilentEdgesNotStroked(t *testing.T) {
	v := &neFltVec{edges: make(map[[2]neFltVecPt]int)}
	// a square: left/top/bottom real, right side silent
	v.add(0, 0, 10, 0, false)
	v.addSilent(10, 0, 10, 10)
	v.add(10, 10, 0, 10, false)
	v.add(0, 10, 0, 0, false)
	var sb, st strings.Builder
	v.loops(&sb, &st)
	d, s := sb.String(), st.String()
	if strings.Count(d, "M") != 1 || strings.Count(d, "L") != 3 || !strings.HasSuffix(d, "Z") {
		t.Fatalf("fill: want one closed loop of 4 vertices, got %q", d)
	}
	if strings.Count(s, "M") != 1 || strings.Count(s, "L") != 3 || strings.Contains(s, "Z") {
		t.Fatalf("stroke: want one open polyline of 3 edges, got %q", s)
	}
	// the stroke must not contain the silent edge: both of its endpoints appear once, never adjacent as a segment
	if strings.Contains(s, "L10.00 0.00L10.00 10.00") || strings.Contains(s, "L10.00 10.00L10.00 0.00") ||
		strings.Contains(s, "M10.00 0.00L10.00 10.00") || strings.Contains(s, "M10.00 10.00L10.00 0.00") {
		t.Fatalf("stroke draws the silent edge: %q", s)
	}
	// the same square with the silent edge as the loop's LAST step (closing into the start vertex)
	v = &neFltVec{edges: make(map[[2]neFltVecPt]int)}
	v.add(10, 0, 10, 10, false)
	v.add(10, 10, 0, 10, false)
	v.add(0, 10, 0, 0, false)
	v.addSilent(0, 0, 10, 0)
	sb.Reset()
	st.Reset()
	v.loops(&sb, &st)
	if s := st.String(); strings.Count(s, "M") != 1 || strings.Count(s, "L") != 3 || strings.Contains(s, "0.00 0.00L10.00 0.00") || strings.Contains(s, "10.00 0.00L0.00 0.00") {
		t.Fatalf("stroke (silent last): got %q", s)
	}
}
