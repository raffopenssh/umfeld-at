package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uber/h3-go/v4"
)

func treeFeatJSON(t *testing.T, f *mvtFeature) map[string]interface{} {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteByte('{')
	mvtFeatureJSON(&buf, f)
	buf.WriteByte('}')
	var m map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("%v: %s", err, buf.String())
	}
	return m
}

// One figure set, three writers: the aggregate, the tile attributes, the unit-stats keys.
func TestNETreeAgg(t *testing.T) {
	// cells: [3 m, 5 m, 35 m, 45 m], [], [20 m, 20 m, 20.5 m], one ≥ 55 m
	th := neTreeHFromCounts([]byte{4, 0, 0, 0, 3, 0, 1, 0}, []byte{6, 10, 70, 90, 40, 40, 41, 120})
	var a neTreeAgg
	for k := 0; k < 4; k++ {
		a.addList(th, k, 0, 0, false)
	}
	if a.n != 8 || a.top != 120 || a.bands != [neTreeBands]int64{1, 1, 0, 0, 3, 0, 0, 1, 0, 1, 0, 1} || !a.detailed() {
		t.Fatalf("agg %+v", a)
	}
	if m, ok := a.meanM(); !ok || m != float64(6+10+70+90+40+40+41+120)*0.5/8 {
		t.Fatalf("mean %v", m)
	}
	// apices exactly on a band edge: a third of them is the band below's (0.25 m heights in 0.5 m steps)
	eg := neTreeHFromCounts([]byte{7, 0}, []byte{19, 20, 20, 20, 20, 20, 21})
	var ea neTreeAgg
	ea.addList(eg, 0, 0, 0, false)
	if o := ea.bandsOut(); ea.bands[1] != 1 || ea.bands[2] != 6 || o[1] != 3 || o[2] != 4 {
		t.Fatalf("edge split %v → %v", ea.bands, o)
	}
	// a height window counts only the apices inside it
	var w neTreeAgg
	for k := 0; k < 4; k++ {
		w.addList(th, k, 40, 70, true)
	}
	if w.n != 4 || w.top != 70 || w.bands[4] != 3 || w.bands[7] != 1 {
		t.Fatalf("window %+v", w)
	}
	// a cell without a list: the count stands, heights do not
	b := a
	b.addCount(2)
	if b.n != 10 || b.detailed() {
		t.Fatalf("mixed %+v", b)
	}
	l := newMVTLayer("x")
	f := l.feature(1)
	b.mvt(f, 2, false)
	if m := treeFeatJSON(t, f); m["tn"] != 10.0 || m["tn_ha"] != 5.0 || m["ht_mean"] != nil || m["tn_20"] != nil {
		t.Fatalf("mixed attrs %v", m)
	}
	f = l.feature(2)
	b.mvt(f, 2, true) // under a height term a count that is not list-exact is no figure
	if m := treeFeatJSON(t, f); len(m) != 0 {
		t.Fatalf("window attrs %v", m)
	}
	f = l.feature(3)
	a.mvt(f, 4, false)
	m := treeFeatJSON(t, f)
	if m["tn"] != 8.0 || m["tn_ha"] != 2.0 || m["ht_mean"] != 26.1 || m["tn_3"] != 1.0 || m["tn_20"] != 2.0 || m["tn_15"] != 1.0 || m["tn_55"] != 1.0 || m["tn_10"] != nil {
		t.Fatalf("attrs %v", m)
	}
	fields := map[string]string{}
	neMVTTreeFields(fields)
	for k := range m {
		if k != "tn" && fields[k] == "" {
			t.Errorf("attribute %s is not in the field table", k)
		}
	}
	// the sample: ×49, the tallest of a sample is not a figure
	s := a
	s.scale(neSampK)
	fg := map[string]interface{}{}
	s.figures(fg, 100, true)
	if fg["trees_height_n.20_25"] != int64(3*neSampK-33) || fg["trees.height_mean_m"] != 26.1 || fg["trees.per_ha"] != neRound(8*neSampK/100.0, 1) || fg["trees.tallest_m"] != nil {
		t.Fatalf("sample figures %v", fg)
	}
	fg = map[string]interface{}{"trees_n": 8, "ndvi.mean": 1, "kg.n": 1}
	a.figures(fg, 4, false)
	if fg["trees.tallest_m"] != 60.0 || len(fg) != 3+3+neTreeBands {
		t.Fatalf("figures %v", fg)
	}
	ks := strings.Join(neStatsSortedKeys(fg), " ")
	if !strings.HasPrefix(ks, "ndvi.mean trees_n trees.per_ha trees.height_mean_m trees.tallest_m trees_height_n.3_5 trees_height_n.5_10 trees_height_n.10_15 ") || !strings.HasSuffix(ks, " trees_height_n.50_55 trees_height_n.55_up kg.n") {
		t.Fatalf("order %s", ks)
	}
	fg = map[string]interface{}{}
	b.figures(fg, 4, false)
	if len(fg) != 0 {
		t.Fatalf("figures without lists %v", fg)
	}
}

// The tile aggregate reads the lists of a bin; the cell carries count and mean.
func TestNEMVTTreeAttrs(t *testing.T) {
	th := neTreeHFromCounts([]byte{2, 0, 0, 0, 1, 0}, []byte{20, 60, 110})
	b := &neFidxBin{N: 3, Cols: map[string][]byte{"tn": {2, 0, 1}, "ht": {60, 255, 110}}, TH: th}
	a := neMVTAggNew(nil)
	for k := 0; k < 3; k++ {
		neMVTAggAdd(a, b, k)
	}
	l := newMVTLayer("x")
	f := l.feature(1)
	neMVTAggAttrs(f, a, 0.09)
	m := treeFeatJSON(t, f)
	if m["tn"] != 3.0 || m["ht"] != 55.0 || m["ht_mean"] != 31.7 || m["tn_10"] != 1.0 || m["tn_30"] != 1.0 || m["tn_55"] != 1.0 || m["tn_ha"] != 33.3 {
		t.Fatalf("aggregate %v", m)
	}
	e := neFiltParse("ht=25..40")
	a = neMVTAggNew(e)
	for k := 0; k < 3; k++ {
		neMVTAggAdd(a, b, k)
	}
	f = l.feature(2)
	neMVTAggAttrs(f, a, 0.09)
	if m := treeFeatJSON(t, f); m["tn"] != 1.0 || m["ht_mean"] != 30.0 {
		t.Fatalf("aggregate under ht=25..40 %v", m)
	}
	f = l.feature(3)
	neMVTCellAttrs(f, b, 0)
	if m := treeFeatJSON(t, f); m["tn"] != 2.0 || m["ht_mean"] != 20.0 || m["ht"] != 30.0 {
		t.Fatalf("cell %v", m)
	}
	f = l.feature(4)
	neMVTCellAttrs(f, b, 1)
	if m := treeFeatJSON(t, f); m["tn"] != 0.0 || m["ht_mean"] != nil {
		t.Fatalf("empty cell %v", m)
	}
}

// The peer's national table → file → hexagons at res 9, 8, 7.
func TestNENatTrees(t *testing.T) {
	c12, _ := h3.LatLngToCell(h3.LatLng{Lat: 47.2, Lng: 15.1}, 12)
	p8, _ := c12.Parent(8)
	kids, _ := p8.Children(9)
	ids := make([]uint64, 0, len(kids))
	for _, k := range kids[:4] {
		ids = append(ids, uint64(k))
	}
	sortU64(ids)
	n := len(ids)
	idc, top, hh := make([]byte, 8*n), make([]byte, n), make([]byte, 2*neTreeBands*n)
	var prev uint64
	for k, id := range ids {
		binary.LittleEndian.PutUint64(idc[8*k:], id-prev)
		prev = id
		top[k] = byte(40 + k)
		if k == 3 { // a hexagon without a tree: no row
			top[k] = 255
			continue
		}
		binary.LittleEndian.PutUint16(hh[2*(k*neTreeBands+2):], uint16(10*(k+1)))
		binary.LittleEndian.PutUint16(hh[2*(k*neTreeBands+3):], 5)
	}
	hdr, _ := json.Marshal(map[string]interface{}{"n": n, "res": 9, "gen": "g1", "epoch": "2026-10", "ids": "delta_u64",
		"tree_hh_bands": []string{"3-5", "5-10", "10-15", "15-20", "20-25", "25-30", "30-35", "35-40", "40-45", "45-50", "50-55", "55+"},
		"columns": []map[string]interface{}{{"name": "id", "dtype": "u64", "bytes": len(idc)}, {"name": "canopy_h_top", "dtype": "u8", "bytes": n}, {"name": "tree_hh", "dtype": "u16", "bytes": len(hh), "shape": []int{12}}}})
	body := append([]byte("NEX1"), binary.LittleEndian.AppendUint32(nil, uint32(len(hdr)))...)
	body = append(append(append(append(body, hdr...), idc...), top...), hh...)
	rows, h, err := neNatTreesParse(body)
	if err != nil || len(rows) != 3 || h.Trees != 10+20+30+15 || h.Gen != "g1" {
		t.Fatalf("parse: %v, %d rows, %+v", err, len(rows), h)
	}
	path := filepath.Join(t.TempDir(), "nat_trees.bin")
	if err := neNatTreesWrite(path, rows, h); err != nil {
		t.Fatal(err)
	}
	nt, err := neNatTreesOpen(path)
	if err != nil || nt.N != [3]int{3, 1, 1} || nt.state() != "+tg1" {
		t.Fatalf("open: %v %+v", err, nt)
	}
	var a neTreeAgg
	nt.fill(&a, 9, ids[1])
	if a.n != 25 || a.bands[2] != 20 || a.bands[3] != 5 || a.top != 41 || !a.est {
		t.Fatalf("res 9 %+v", a)
	}
	if m, ok := a.meanM(); !ok || neRound(m, 2) != neRound((20*neTreeBandCentM[2]+5*neTreeBandCentM[3])/25, 2) {
		t.Fatalf("mean %v", m)
	}
	a = neTreeAgg{}
	nt.fill(&a, 9, ids[3]) // no row: a hexagon without a tree, still a figure
	l := newMVTLayer("x")
	f := l.feature(1)
	a.mvt(f, 10, false)
	if m := treeFeatJSON(t, f); a.n != 0 || !a.est || m["tn"] != 0.0 || len(m) != 1 {
		t.Fatalf("empty hexagon %+v %v", a, m)
	}
	for _, res := range []int{8, 7} {
		a = neTreeAgg{}
		p, _ := c12.Parent(res)
		nt.fill(&a, res, uint64(p))
		if a.n != 75 || a.bands[2] != 60 || a.bands[3] != 15 || a.top != 42 {
			t.Fatalf("res %d %+v", res, a)
		}
	}
	var nilT *neNatTrees
	a = neTreeAgg{}
	nilT.fill(&a, 9, ids[0])
	if a.est || nilT.state() != "" {
		t.Fatal("no table: no figure")
	}
}
