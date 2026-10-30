package main

import (
	"math"
	"testing"

	"github.com/paulmach/orb"
)

// A river polygon with an island (hole) crossing the viewport edge must come
// back as a closed polygon ending at the edge, with the hole preserved.
func TestClipMultiPolygonToRectKeepsHoleAndCloses(t *testing.T) {
	shell := orb.Ring{{0, 0}, {10, 0}, {10, 4}, {0, 4}, {0, 0}}
	hole := orb.Ring{{2, 1}, {3, 1}, {3, 2}, {2, 2}, {2, 1}}
	mp := orb.MultiPolygon{{shell, hole}}
	rect := orb.Bound{Min: orb.Point{-1, -1}, Max: orb.Point{5, 5}}
	out := clipMultiPolygonToRect(mp, rect)
	if len(out) != 1 || len(out[0]) != 2 {
		t.Fatalf("want 1 polygon with 2 rings, got %d polygons %v", len(out), out)
	}
	for _, r := range out[0] {
		if r[0] != r[len(r)-1] {
			t.Fatalf("ring not closed: %v", r)
		}
	}
	area := math.Abs(planarAreaRing(out[0][0])) - math.Abs(planarAreaRing(out[0][1]))
	if math.Abs(area-(5*4-1)) > 1e-9 {
		t.Fatalf("clipped area %v, want 19", area)
	}
	// fully outside → nothing
	if got := clipMultiPolygonToRect(mp, orb.Bound{Min: orb.Point{20, 20}, Max: orb.Point{30, 30}}); len(got) != 0 {
		t.Fatalf("expected empty clip, got %v", got)
	}
}

func planarAreaRing(r orb.Ring) float64 {
	s := 0.0
	for i := 0; i < len(r)-1; i++ {
		s += r[i][0]*r[i+1][1] - r[i+1][0]*r[i][1]
	}
	return s / 2
}
