package main

import (
	"compress/gzip"
	"encoding/json"
	"os"
	"testing"
)

// Python h3 4.5.0 reference values (docs/ne-public-api.md §4).
func TestNEH3PythonAgreement(t *testing.T) {
	cases := []struct {
		lat, lon  float64
		cell, par string
	}{
		{47.12, 15.11, "8c1e108cc922dff", "891e108cc93ffff"},
		{48.2082, 16.3738, "8c1e15b712abdff", "891e15b712bffff"},
		{46.6, 13.85, "8c1e16399c447ff", "891e16399c7ffff"},
		{47.0665, 15.0857, "8c1e108dd0d21ff", "891e108dd0fffff"},
		{48.95, 15.2, "8c1e33ba30843ff", "891e33ba30bffff"},
	}
	for _, c := range cases {
		id, err := neLatLngToCell(c.lon, c.lat, 12)
		if err != nil || neCellHex(id) != c.cell {
			t.Errorf("%v,%v → %s want %s (%v)", c.lat, c.lon, neCellHex(id), c.cell, err)
		}
		if p := neCellHex(h3Parent(id, 9)); p != c.par {
			t.Errorf("parent %s want %s", p, c.par)
		}
		if _, res, err := neParseCell(c.cell); err != nil || res != 12 {
			t.Errorf("parse %s: res %d err %v", c.cell, res, err)
		}
	}
	if _, _, err := neParseCell("zz"); err == nil {
		t.Error("garbage must not parse")
	}
	if len(neGridDisk(0x8c1e108cc922dff, 1)) != 7 {
		t.Error("k=1 disk must have 7 cells")
	}
}

// Every res-12 id of the Kohlschwarz draft fixture must round-trip through
// centre → latLngToCell, and its res-9 parent must agree with the pure-int h3Parent.
func TestNEH3Fixture(t *testing.T) {
	f, err := os.Open("docs/ne-draft/kohlschwarz_ne.json.gz")
	if err != nil {
		t.Skip("fixture missing")
	}
	defer f.Close()
	zr, _ := gzip.NewReader(f)
	var doc struct {
		Cells map[string]json.RawMessage `json:"cells"`
	}
	if err := json.NewDecoder(zr).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	n := 0
	for hex := range doc.Cells {
		id, res, err := neParseCell(hex)
		if err != nil || res != 12 {
			t.Fatalf("%s: %v", hex, err)
		}
		lon, lat := neCellCentre(id)
		back, _ := neLatLngToCell(lon, lat, 12)
		if back != id {
			t.Fatalf("%s: centre round-trip gave %s", hex, neCellHex(back))
		}
		if a, b := h3Parent(id, 9), neChildren(h3Parent(id, 9), 12); len(b) != 343 || a == id {
			t.Fatalf("%s: parent/children", hex)
		}
		n++
		if n > 2000 {
			break
		}
	}
	chunks := neBBoxChunks(15.10, 47.12, 15.12, 47.14)
	if len(chunks) < 4 || len(chunks) > 120 {
		t.Fatalf("bbox chunks %d", len(chunks))
	}
}

// neBBoxCells (one polygon fill) must return exactly the cells the previous
// "children of every chunk whose centre is in the box" enumeration returned.
func TestNEBBoxCellsMatchesCentreRule(t *testing.T) {
	w, s, e, n := 15.10, 47.12, 15.12, 47.14
	var want []uint64
	for _, c9 := range neBBoxChunks(w, s, e, n) {
		for _, c := range neChildren(c9, neCellRes) {
			if neCentreInBBox(c, w, s, e, n) {
				want = append(want, c)
			}
		}
	}
	sortU64(want)
	got := neBBoxCells(w, s, e, n)
	if len(got) < 5000 || len(got) != len(want) {
		t.Fatalf("got %d cells, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("cell %d differs: %x vs %x", i, got[i], want[i])
		}
	}
}
