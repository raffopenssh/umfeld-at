package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/uber/h3-go/v4"
)

// TestNESampRowID — the row id maths against h3: centre child of every res-10 cell, nothing coarser.
func TestNESampRowID(t *testing.T) {
	c9, _ := h3.LatLngToCell(h3.LatLng{Lat: 47.3, Lng: 13.1}, 9)
	kids, _ := c9.Children(10)
	if len(kids) != 7 {
		t.Fatalf("children: %d", len(kids))
	}
	seen := 0
	for _, k := range kids {
		cc, _ := k.CenterChild(12)
		id := uint64(cc)
		if !neSampIsCentre(id) {
			t.Fatalf("%x not centre", id)
		}
		if got := neSampRowID(uint64(c9), byte((id>>15)&7)); got != id {
			t.Fatalf("rowID %x want %x", got, id)
		}
		all, _ := k.Children(12)
		if len(all) != neSampK {
			t.Fatalf("res-10 → res-12 children %d want %d", len(all), neSampK)
		}
		for _, a := range all {
			if neSampIsCentre(uint64(a)) {
				seen++
			}
		}
	}
	if seen != 7 {
		t.Fatalf("centre children seen %d want 7", seen)
	}
}

// TestNESampBuild — a fixture KG of one res-9 cell: the builder emits exactly its
// 7 centre children, columns land in the planar image, the image reloads from zstd.
func TestNESampBuild(t *testing.T) {
	c9, _ := h3.LatLngToCell(h3.LatLng{Lat: 47.3, Lng: 13.1}, 9)
	cells, _ := c9.Children(12)
	ids := make([]uint64, len(cells))
	for i, c := range cells {
		ids[i] = uint64(c)
	}
	sortU64(ids)
	rows := make([]byte, 0, len(ids)*neLURowBytes)
	for i, id := range ids {
		row := make([]byte, neLURowBytes)
		binary.LittleEndian.PutUint64(row, id)
		row[8] = 255 // whole cell in the KG
		g := 2       // grass
		if i%3 == 0 {
			g = 4 // forest
		}
		row[12+g] = 200
		rows = append(rows, row...)
	}
	dir := t.TempDir()
	b := neSampBegin()
	if b.failed != nil {
		t.Fatal(b.failed)
	}
	b.out = filepath.Join(dir, "samp.bin")
	b.kg("99999", rows, nil, nil, nil)
	s, err := b.finish("teststate")
	if err != nil {
		t.Fatal(err)
	}
	if s.Hdr.N != 7 || s.Hdr.N9 != 1 || s.ids9[0] != uint64(c9) {
		t.Fatalf("n=%d n9=%d ids9=%x", s.Hdr.N, s.Hdr.N9, s.ids9)
	}
	for k := 0; k < 7; k++ {
		id := s.id(k)
		if !neSampIsCentre(id) || neH3ParentFast(id, 9) != uint64(c9) {
			t.Fatalf("row %d id %x", k, id)
		}
		d := s.col("d")[k]
		if d != 2 && d != 4 {
			t.Fatalf("row %d d=%d", k, d)
		}
		if s.col("dsh")[k] != 254 {
			t.Fatalf("dsh %d", s.col("dsh")[k])
		}
		if s.col("o")[k] != 255 || s.col("hb")[k] != 255 || s.col("sub")[k] != 255 {
			t.Fatalf("missing layers must be 255/unknown: o=%d hb=%d sub=%d", s.col("o")[k], s.col("hb")[k], s.col("sub")[k])
		}
		// a centre child's ring is inside its res-10 cell → het/edge known; cont counts the component
		if s.col("het")[k] > 6 || s.col("edge")[k] > 6 || s.col("cont")[k]&127 == 0 {
			t.Fatalf("het=%d edge=%d cont=%d", s.col("het")[k], s.col("edge")[k], s.col("cont")[k])
		}
		if s.col("chg")[k] != 0 || s.col("n2k")[k] != 0 {
			t.Fatalf("chg/n2k must be 0: %d %d", s.col("chg")[k], s.col("n2k")[k])
		}
	}
	// a filter over the store
	e := neFiltParse("d=4")
	m := s.eval(e)
	f9 := s.fold9(e)
	n4 := 0
	for _, v := range m {
		n4 += int(v)
	}
	if n4 == 0 || int(f9[0]) != n4 {
		t.Fatalf("d=4: mask %d fold9 %d", n4, f9[0])
	}
	// reload
	if _, err := os.Stat(b.out); err != nil {
		t.Fatal(err)
	}
	r, err := neSampLoad(b.out)
	if err != nil {
		t.Fatal(err)
	}
	if r.Hdr.State != s.Hdr.State || r.Hdr.N != 7 || string(r.col("d")) != string(s.col("d")) {
		t.Fatalf("reload differs")
	}
	if cls := neSampQLon(neSampLon(neSampQLon(13.1))); cls != neSampQLon(13.1) {
		t.Fatalf("lon quantisation")
	}
}
