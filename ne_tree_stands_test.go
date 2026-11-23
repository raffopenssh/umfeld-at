package main

import (
	"strings"
	"testing"
)

func TestNEStandsPolygons(t *testing.T) {
	sq := func(x0, y0, x1, y1 float64) [][2]float64 {
		return [][2]float64{{x0, y0}, {x1, y0}, {x1, y1}, {x0, y1}} // open: must come back closed
	}
	// an outer ring, a hole in it, an island in the hole, and a second polygon
	got := neStandsPolygons([][][2]float64{sq(1, 1, 3, 3), sq(0, 0, 10, 10), sq(20, 0, 21, 1), sq(1.5, 1.5, 2, 2)})
	if len(got) != 3 {
		t.Fatalf("polygons = %d, want 3 (outer, island, second)", len(got))
	}
	holes := 0
	for _, p := range got {
		holes += len(p) - 1
		for _, r := range p {
			if r[0] != r[len(r)-1] {
				t.Fatalf("ring not closed: %v", r)
			}
		}
	}
	if holes != 1 {
		t.Fatalf("holes = %d, want 1", holes)
	}
}

func TestTreesSpark(t *testing.T) {
	tot := neStandsTotal{Trees: 100, TallestM: 60, EdgesM: []float64{3, 5, 10, 15}, Bands: []int64{4, 50, 40, 6}}
	s := treesSpark(tot)
	if strings.Count(s, "<rect") != 4 || !strings.Contains(s, "most are 5–10 m") || !strings.Contains(s, "3–60 m") {
		t.Fatalf("spark: %s", s)
	}
	if treesSpark(neStandsTotal{}) != "" {
		t.Fatal("no bands must give no sparkline")
	}
	if g := groupThousands(547891818); g != "547\u202f891\u202f818" {
		t.Fatalf("groupThousands: %q", g)
	}
}
