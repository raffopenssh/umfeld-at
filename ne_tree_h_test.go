package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"testing"
)

// TestNETreeHCount — the range count on sorted lists, the empty cell, the clamp ends.
func TestNETreeHCount(t *testing.T) {
	// cells: [6 10 70 90], [], [40 40 41]
	cnt := []byte{4, 0, 0, 0, 3, 0}
	h := []byte{6, 10, 70, 90, 40, 40, 41}
	th := neTreeHFromCounts(cnt, h)
	if th == nil || th.n(0) != 4 || th.n(1) != 0 || th.n(2) != 3 {
		t.Fatal("from counts")
	}
	cases := []struct{ k, lo, hi, want int }{{0, 70, 90, 2}, {0, 1, 254, 4}, {0, 11, 69, 0}, {1, 1, 254, 0}, {2, 40, 40, 2}, {2, 41, 254, 1}, {0, 90, 90, 1}, {0, 91, 254, 0}}
	for _, c := range cases {
		if got := th.count(c.k, c.lo, c.hi); got != c.want {
			t.Errorf("count(%d,%d,%d)=%d want %d", c.k, c.lo, c.hi, got, c.want)
		}
	}
	if neTreeHFromCounts(cnt, h[:6]) != nil {
		t.Fatal("length mismatch accepted")
	}
	r := th.reorder([]int{2, 0, 1})
	if r.n(0) != 3 || r.H[0] != 40 || r.n(1) != 4 || r.H[3] != 6 || r.n(2) != 0 {
		t.Fatalf("reorder: %v %v", r.Off, r.H)
	}
	if string(r.counts()) != string([]byte{3, 0, 4, 0, 0, 0}) {
		t.Fatal("counts")
	}
}

// TestNEFiltApexRange — the height window of an expression in 0.5 m units.
func TestNEFiltApexRange(t *testing.T) {
	lo, hi, ok := neFiltApexRange(neFiltParse("ht=35..45,sl>=20"))
	if !ok || lo != 70 || hi != 90 {
		t.Fatalf("ht=35..45 → %d..%d ok=%v", lo, hi, ok)
	}
	if _, _, ok := neFiltApexRange(neFiltParse("sl>=20")); ok {
		t.Fatal("no ht term → no window")
	}
	lo, hi, ok = neFiltApexRange(neFiltParse("ht>=30"))
	if !ok || lo != 60 || hi != 254 {
		t.Fatalf("ht>=30 → %d..%d", lo, hi)
	}
	// counter: lists win, tn is the fallback, exactness reported
	th := neTreeHFromCounts([]byte{2, 0}, []byte{50, 80})
	c := neNewTreeCounter(th, []byte{9}, neFiltParse("ht=35..45"))
	if n, ex := c.at(0); n != 1 || !ex {
		t.Fatalf("range count %d %v", n, ex)
	}
	c = neNewTreeCounter(nil, []byte{9}, neFiltParse("ht=35..45"))
	if n, ex := c.at(0); n != 9 || ex {
		t.Fatalf("tn fallback %d %v", n, ex)
	}
	c = neNewTreeCounter(th, nil, neFiltParse("sl>=20"))
	if n, ex := c.at(0); n != 2 || !ex {
		t.Fatalf("whole-cell list count %d %v", n, ex)
	}
	// no expression at all (an unfiltered unit on the sample path) must not panic
	c = neNewTreeCounter(th, nil, nil)
	if n, ex := c.at(0); n != 2 || !ex {
		t.Fatalf("nil expr count %d %v", n, ex)
	}
}

// TestNEFidxTreeHTail — the blob tail round-trips on plane all and is absent on core/ext.
func TestNEFidxTreeHTail(t *testing.T) {
	b := newNEFiltTestBin(3)
	h := make([]byte, 0)
	off := []uint32{0}
	for k := 0; k < b.N; k++ {
		for q := 0; q < k%3; q++ {
			h = append(h, byte(10+q))
		}
		off = append(off, uint32(len(h)))
	}
	b.TH = &neTreeH{Off: off, H: h}
	got, err := neFidxDecode(b.encode("all"))
	if err != nil || got.TH == nil || len(got.TH.H) != len(h) || got.TH.n(2) != 2 || got.TH.n(3) != 0 {
		t.Fatalf("all: %v th=%v", err, got != nil && got.TH != nil)
	}
	if c, _ := neFidxDecode(b.encode("core")); c.TH != nil || c.Hdr.TH {
		t.Fatal("core carries the tail")
	}
	// plane th: the tail alone, no column, same lattice
	th, err := neFidxDecode(b.encode("th"))
	if err != nil || th.TH == nil || len(th.Cols) != 0 || th.N != b.N || len(th.TH.H) != len(h) || th.TH.n(2) != 2 {
		t.Fatalf("th plane: %v cols=%d", err, len(th.Cols))
	}
}

// TestNEFiltTreeLists — §9a: `ht` with an upper bound is "a tree of that height in
// the cell" over the apex lists; `htmax` keeps the tallest-tree rule; without
// lists `ht` falls back to the column; lower bounds agree under every rule.
func TestNEFiltTreeLists(t *testing.T) {
	b := &neFidxBin{N: 4, Cols: map[string][]byte{}, IJ: []int32{0, 0, 1, 0, 2, 0, 3, 0}}
	// cell 0: tallest 50 m with a 40 m apex · cell 1: tallest 40 m alone · cell 2: tallest 50 m, no apex in 35..45 · cell 3: unknown
	b.Cols["ht"] = []byte{100, 80, 100, 255}
	b.TH = &neTreeH{Off: []uint32{0, 3, 1 + 3, 2 + 4, 2 + 4}, H: []byte{20, 80, 100, 80, 10, 100}}
	ev := func(f string) string {
		m := neFiltEval(b, neFiltParse(f))
		return fmt.Sprint(m)
	}
	for _, c := range []struct{ f, want string }{
		{"ht=35..45", "[1 1 0 0]"},
		{"htmax=35..45", "[0 1 0 0]"},
		{"tallesttree=35..45", "[0 1 0 0]"},
		{"ht>=45", "[1 0 1 0]"},
		{"htmax>=45", "[1 0 1 0]"},
		{"ht<=12", "[1 0 1 0]"}, // the 10 m apex of cell 0 (raw 20) and of cell 2 (raw 10 = 5 m)
		{"htmax<=12", "[0 0 0 0]"},
		{"ht=unk", "[0 0 0 1]"},
		{"htmax=unk", "[0 0 0 1]"},
		{"ht=35..45|htmax>=50", "[1 1 1 0]"},
	} {
		if got := ev(c.f); got != c.want {
			t.Errorf("%s: %s want %s", c.f, got, c.want)
		}
	}
	if !neFiltNeedsTH(neFiltParse("ht=35..45")) || neFiltNeedsTH(neFiltParse("ht>=20")) || neFiltNeedsTH(neFiltParse("htmax<=10")) || neFiltNeedsTH(neFiltParse("ht=unk")) {
		t.Error("needsTH: only ht with an upper bound")
	}
	// without lists: the column rule for both
	b.TH = nil
	if ev("ht=35..45") != "[0 1 0 0]" || ev("ht<=12") != "[0 0 0 0]" {
		t.Error("no lists: ht must fall back to the tallest-tree column")
	}
	// the counter's window spans both spellings
	if lo, hi, ok := neFiltApexRange(neFiltParse("htmax=35..45")); !ok || lo != 70 || hi != 90 {
		t.Errorf("apex range over htmax: %d %d %v", lo, hi, ok)
	}
}

// TestNEFidxBlockParseRagged — a v2 block with tree_h_n + ragged tree_h over two bins: each bin gets its own slice of the lists.
func TestNEFidxBlockParseRagged(t *testing.T) {
	bi, bj := 130, 473
	i1, j1 := bi*4+1, bj*4+2
	i2, j2 := bi*4+2, bj*4+2
	type binSpec struct {
		i, j int
		enum []uint64
		bm   []byte
		xp   uint64
		np   int
	}
	var specs []binSpec
	for _, ij := range [][2]int{{i1, j1}, {i2, j2}} {
		w, s := float64(ij[0])*neFidxBinDeg, float64(ij[1])*neFidxBinDeg
		enum := neBBoxCells(w, s, w+neFidxBinDeg, s+neFidxBinDeg)
		sp := binSpec{i: ij[0], j: ij[1], enum: enum, bm: make([]byte, (len(enum)+7)/8)}
		for q, id := range enum {
			if q%2 == 0 {
				sp.bm[q>>3] |= 1 << uint(q&7)
				sp.xp ^= id
				sp.np++
			}
		}
		specs = append(specs, sp)
	}
	// the bitmap spans both bins' enumerations back to back (bit offsets continue)
	nbits := len(specs[0].enum) + len(specs[1].enum)
	bm := make([]byte, (nbits+7)/8)
	for q := range specs[0].enum {
		if specs[0].bm[q>>3]&(1<<uint(q&7)) != 0 {
			bm[q>>3] |= 1 << uint(q&7)
		}
	}
	for q := range specs[1].enum {
		if specs[1].bm[q>>3]&(1<<uint(q&7)) != 0 {
			b := len(specs[0].enum) + q
			bm[b>>3] |= 1 << uint(b&7)
		}
	}
	rows := specs[0].np + specs[1].np
	var cnt, th []byte
	for r := 0; r < rows; r++ {
		n := r % 3
		cnt = binary.LittleEndian.AppendUint16(cnt, uint16(n))
		for q := 0; q < n; q++ {
			th = append(th, byte(r%200+q))
		}
	}
	hdr := map[string]interface{}{"v": 2, "ids": "bitmap", "present_bytes": len(bm),
		"columns": []map[string]interface{}{{"name": "bld_h_max", "dtype": "u8", "bytes": rows}, {"name": "tree_h_n", "dtype": "u16", "bytes": len(cnt)}, {"name": "tree_h", "dtype": "u8", "shape": "ragged", "counts": "tree_h_n", "bytes": len(th)}},
		"bins":    []map[string]interface{}{{"i": i1, "j": j1, "n": specs[0].np, "row0": 0, "ids_xor": fmt.Sprintf("%016x", specs[0].xp)}, {"i": i2, "j": j2, "n": specs[1].np, "row0": specs[0].np, "ids_xor": fmt.Sprintf("%016x", specs[1].xp)}}}
	hj, _ := json.Marshal(hdr)
	body := []byte("NEXB")
	body = binary.LittleEndian.AppendUint32(body, uint32(len(hj)))
	body = append(body, hj...)
	body = append(body, bm...)
	for r := 0; r < rows; r++ {
		body = append(body, 7)
	}
	body = append(body, cnt...)
	body = append(body, th...)
	bins, ver, ok := neFidxBlockParse(body, "2026-11", bi, bj)
	if !ok || ver != 2 || len(bins) != 2 {
		t.Fatalf("ok=%v ver=%d bins=%d", ok, ver, len(bins))
	}
	v1, v2 := bins[fmt.Sprintf("%d/%d", i1, j1)], bins[fmt.Sprintf("%d/%d", i2, j2)]
	if v1.th == nil || v2.th == nil || len(v1.th.Off) != specs[0].np+1 || len(v2.th.Off) != specs[1].np+1 {
		t.Fatal("lists missing")
	}
	// row r of the block has r%3 apices starting at r%200
	r0 := specs[0].np
	if v2.th.n(0) != r0%3 || v2.th.n(1) != (r0+1)%3 || (v2.th.n(1) > 0 && v2.th.H[v2.th.Off[1]] != byte((r0+1)%200)) {
		t.Fatalf("second bin's lists do not start at row %d", r0)
	}
	if int(v1.th.Off[specs[0].np])+int(v2.th.Off[specs[1].np]) != len(th) {
		t.Fatal("lists not fully distributed")
	}
	if v1.cols["tree_h"] != nil {
		t.Fatal("ragged column leaked into the planar map")
	}
}
