package main

import (
	"testing"

	h3 "github.com/uber/h3-go/v4"
)

// A tree passes exactly where its cell passes the filter: the matcher's verdict
// at every cell centre (and at a point off-centre in the same cell) equals the
// bin's own mask — the cells the veil tiles and /ne/filter count.
func TestNETreePass(t *testing.T) {
	if err := neGlyphLoadFrame(); err != nil {
		t.Skip("frame:", err)
	}
	b := newNEFiltTestBin(5)
	ids := neMVTBinIDs(b, "t")
	expr := neFiltParse("d=wald,sl>=10")
	mask := b.eval(expr)
	pb := neTreePassBinOf(b, expr)
	if pb == nil {
		t.Fatal("no bin")
	}
	p := &neTreePass{expr: expr, bins: map[[2]int]*neTreePassBin{}, memo: map[h3.Cell]bool{}}
	for k := range ids {
		lon, lat := neCellCentre(ids[k])
		i, j := neWhereTileKey(0, lon, lat)
		p.bins[[2]int{i, j}] = pb
	}
	in, out := 0, 0
	for k := range ids {
		if ids[k] == 0 {
			continue
		}
		lon, lat := neCellCentre(ids[k])
		for _, d := range []float64{0, 0.00002} { // the centre, and ~2 m beside it
			if got, want := p.pass(lon+d, lat), mask[k] != 0; got != want {
				t.Fatalf("cell %x (+%g): pass %v, mask %v", ids[k], d, got, want)
			}
		}
		if mask[k] != 0 {
			in++
		} else {
			out++
		}
	}
	if in == 0 || out == 0 {
		t.Fatalf("fixture splits nothing: %d in, %d out", in, out)
	}
	if p.pass(16.0, 48.0) { // no bin there
		t.Error("a tree outside every bin passed")
	}
	if q, err := neTreePassFor("", 10, 47, 10.01, 47.01); q != nil || err != nil {
		t.Error("no filter must be no matcher")
	}
}
