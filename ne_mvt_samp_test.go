package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
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
	old := nePMTDir
	nePMTDir = dir // no peer sample files of the deployment
	defer func() { nePMTDir = old }()
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
	e := neFiltParse("d=forest")
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

// TestNESampSubParse — the subsidy peer's IVS1 sample body on a fixture.
func TestNESampSubParse(t *testing.T) {
	c9, _ := h3.LatLngToCell(h3.LatLng{Lat: 47.3, Lng: 13.1}, 9)
	kids, _ := c9.Children(10)
	ids := make([]uint64, len(kids))
	for i, k := range kids {
		ids[i] = uint64(k)
	}
	sortU64(ids)
	n := len(ids)
	b := []byte("IVS1")
	b = append(b, 10, 12)
	b = binary.LittleEndian.AppendUint32(b, uint32(n))
	prev := uint64(0)
	for _, id := range ids {
		b = binary.AppendUvarint(b, id-prev)
		prev = id
	}
	cols := "cls,cov,bio,rel,chg,dcov,snar,snar2,oepul,nutz"
	for k, name := range []string{"cls", "cov", "bio", "rel", "chg", "dcov", "snar", "snar2", "oepul", "nutz"} {
		sz := 1
		if name == "snar" || name == "snar2" {
			sz = 2
		}
		for r := 0; r < n; r++ {
			if sz == 1 {
				b = append(b, byte(k*10+r))
			} else {
				b = binary.LittleEndian.AppendUint16(b, uint16(k*100+r))
			}
		}
	}
	bin := neSampSubParse(b, cols)
	if bin == nil || bin.N != n {
		t.Fatal("parse")
	}
	for r := 0; r < n; r++ {
		if (bin.Cells[r]>>52)&0xF != 12 || !neSampIsCentre(bin.Cells[r]) || neH3ParentFast(bin.Cells[r], 10) != ids[r] {
			t.Fatalf("row %d id %x not the centre child of %x", r, bin.Cells[r], ids[r])
		}
		if bin.Cls[r] != byte(r) || bin.Cov[r] != byte(10+r) || bin.Snar[r] != uint16(600+r) || bin.Nutz[r] != byte(90+r) {
			t.Fatalf("row %d columns", r)
		}
	}
}

// TestNESubNoidToIDs — the subsidy peer's id-less bin body ↔ the id form (docs/ne-filter.md §8.1).
func TestNESubNoidToIDs(t *testing.T) {
	I, J := 130, 473 // block 13.0/47.3
	i, j := I*4, J*4
	w, s := float64(i)*neSubBinDeg, float64(j)*neSubBinDeg
	enum := neBBoxCells(w, s, w+neSubBinDeg, s+neSubBinDeg)
	nu := len(enum)
	var xu, xp uint64
	bm := make([]byte, (nu+7)/8)
	np := 0
	for q, id := range enum {
		xu ^= id
		if q%3 == 0 {
			bm[q>>3] |= 1 << uint(q&7)
			xp ^= id
			np++
		}
	}
	body := binary.LittleEndian.AppendUint32(nil, uint32(nu))
	body = binary.LittleEndian.AppendUint32(body, uint32(np))
	body = binary.LittleEndian.AppendUint64(body, xu)
	body = binary.LittleEndian.AppendUint64(body, xp)
	body = append(body, bm...)
	for c := 0; c < 12; c++ { // 6 u8 + snar/snar2 as 4 bytes + oepul nutz = 12 B/row in column order
		for r := 0; r < np; r++ {
			body = append(body, byte(c))
		}
	}
	raws := make([]*neSubRaw, 16)
	for k := range raws {
		raws[k] = &neSubRaw{Body: []byte{0, 0, 0, 0}}
	}
	raws[0] = &neSubRaw{Body: body}
	if !neSubLooksNoid(body) {
		t.Fatal("sniff")
	}
	if err := neSubBlockNoidToIDs(raws, I, J); err != nil {
		t.Fatal(err)
	}
	bin, err := neSubParse(raws[0].Body)
	if err != nil || bin.N != np || bin.Cells[0] != enum[0] || bin.Cells[1] != enum[3] || bin.Nutz == nil || bin.Nutz[0] != 11 {
		t.Fatalf("converted: %v n=%d", err, bin.N)
	}
	// a flipped bit → checksum mismatch → rejected
	body[24] ^= 2
	raws[0] = &neSubRaw{Body: body}
	if err := neSubBlockNoidToIDs(raws, I, J); err == nil {
		t.Fatal("corrupt bitmap accepted")
	}
}

// TestNEFidxBlockParseV2 — the observed-layer peer's id-less NEXB v2 block on a fixture.
func TestNEFidxBlockParseV2(t *testing.T) {
	bi, bj := 130, 473
	i, j := bi*4+1, bj*4+2
	w, s := float64(i)*neFidxBinDeg, float64(j)*neFidxBinDeg
	enum := neBBoxCells(w, s, w+neFidxBinDeg, s+neFidxBinDeg)
	nu := len(enum)
	bm := make([]byte, (nu+7)/8)
	var xp uint64
	np := 0
	for q, id := range enum {
		if q%2 == 0 {
			bm[q>>3] |= 1 << uint(q&7)
			xp ^= id
			np++
		}
	}
	hdr := map[string]interface{}{"v": 2, "ids": "bitmap", "present_bytes": len(bm),
		"columns": []map[string]interface{}{{"name": "bld_h_max", "dtype": "u8", "bytes": np}, {"name": "elev_mean", "dtype": "u16", "bytes": 2 * np}},
		"bins":    []map[string]interface{}{{"i": i, "j": j, "n": np, "row0": 0, "ids_xor": fmt.Sprintf("%016x", xp)}}}
	hj, _ := json.Marshal(hdr)
	body := []byte("NEXB")
	body = binary.LittleEndian.AppendUint32(body, uint32(len(hj)))
	body = append(body, hj...)
	body = append(body, bm...)
	for r := 0; r < np; r++ {
		body = append(body, 7)
	}
	for r := 0; r < np; r++ {
		body = binary.LittleEndian.AppendUint16(body, 1234)
	}
	bins, ver, ok := neFidxBlockParse(body, "2026-10", bi, bj)
	if !ok || ver != 2 || len(bins) != 1 {
		t.Fatalf("ok=%v ver=%d bins=%d", ok, ver, len(bins))
	}
	v := bins[fmt.Sprintf("%d/%d", i, j)]
	if v == nil || len(v.ids) != np || v.ids[1] != enum[2] || v.cols["bld_h_max"][3] != 7 || binary.LittleEndian.Uint16(v.cols["elev_mean"][2:]) != 1234 {
		t.Fatal("rows")
	}
	body[8+len(hj)] ^= 1 // flip a present bit → checksum mismatch
	if _, ver, ok := neFidxBlockParse(body, "2026-10", bi, bj); ok || ver != 2 {
		t.Fatal("corrupt bitmap accepted")
	}
}

// TestNESampFnat — the sample's res-7 fold (what fnat?f= serves) against a
// brute-force count over the rows, on a fixture of three res-9 cells in two
// res-7 hexagons; plus the national count helpers.
func TestNESampFnat(t *testing.T) {
	if err := neGlyphLoadFrame(); err != nil {
		t.Skip("frame:", err)
	}
	dir := t.TempDir()
	old := nePMTDir
	nePMTDir = dir
	defer func() { nePMTDir = old }()
	b := neSampBegin()
	b.out = filepath.Join(dir, "samp.bin")
	var kg int
	for _, ll := range []h3.LatLng{{Lat: 47.3, Lng: 13.1}, {Lat: 47.3, Lng: 13.108}, {Lat: 48.2, Lng: 16.4}} {
		c9, _ := h3.LatLngToCell(ll, 9)
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
			row[8] = 255
			g := 2
			if i%2 == 0 {
				g = 4
			}
			row[12+g] = 240
			rows = append(rows, row...)
		}
		kg++
		b.kg(fmt.Sprintf("%05d", kg), rows, nil, nil, nil)
	}
	s, err := b.finish("fnatfixture")
	if err != nil {
		t.Fatal(err)
	}
	if s.Hdr.N9 != 3 || s.Hdr.N != 21 {
		t.Fatalf("n9=%d n=%d", s.Hdr.N9, s.Hdr.N)
	}
	e := neFiltParse("d=forest")
	res := neFnatSample(e, s)
	if res.Sample != neSampK || res.Exact || len(res.Share) != len(neGlyphFrame.cells) {
		t.Fatalf("result shape: sample=%d exact=%v share=%d frame=%d groups=%d", res.Sample, res.Exact, len(res.Share), len(neGlyphFrame.cells), len(e.Groups))
	}
	// brute force per frame hexagon
	mask := s.eval(e)
	want := map[uint64][2]int{}
	for k := 0; k < s.n(); k++ {
		p := neH3ParentFast(s.id(k), 7)
		v := want[p]
		v[1]++
		if mask[k] != 0 {
			v[0]++
		}
		want[p] = v
	}
	if len(want) < 2 {
		t.Fatalf("fixture spans %d res-7 hexagons, want ≥ 2", len(want))
	}
	rows := 0
	for i, c := range neGlyphFrame.cells {
		v, ok := want[c.id]
		if !ok {
			if res.Share[i] != 255 || res.N[i] != 0 {
				t.Fatalf("hex %x: share %d n %d without rows", c.id, res.Share[i], res.N[i])
			}
			continue
		}
		rows++
		if res.N[i] != v[1]*neSampK {
			t.Fatalf("hex %x: n %d want %d", c.id, res.N[i], v[1]*neSampK)
		}
		if exp := int(254*float64(v[0])/float64(v[1]) + .5); res.Share[i] != exp {
			t.Fatalf("hex %x: share %d want %d", c.id, res.Share[i], exp)
		}
	}
	if rows != len(want) || res.Rows != len(want) || res.Cells != 21*neSampK {
		t.Fatalf("rows %d/%d cells %d", rows, res.Rows, res.Cells)
	}
	// k>= alone: nothing folds, listed as skipped
	if r := neFnatSample(neFiltParse("k>=5"), s); r.Share != nil || len(r.Skipped) != 1 {
		t.Fatalf("k>= alone: share=%v skipped=%v", r.Share != nil, r.Skipped)
	}
	// the res-9 binary and the national counts
	bin := neFnat9(e, s, 13.0, 47.2, 13.2, 47.4)
	if string(bin[:4]) != "NSF9" || binary.LittleEndian.Uint32(bin[4:]) != 2 {
		t.Fatalf("fnat9: %q n=%d", bin[:4], binary.LittleEndian.Uint32(bin[4:]))
	}
	cells, match, _, byD := neSampCountIn(s, e, 13.0, 47.2, 13.2, 47.4, nil)
	if cells != 14*neSampK || match != 8*neSampK || byD[4] != 8*neSampK {
		t.Fatalf("count in box: cells %d match %d byD %v", cells, match, byD)
	}
	h := neSampHist(s)
	if hd, ok := h["d"].(map[string]interface{}); !ok || hd["known"].(int) != 21*neSampK {
		t.Fatalf("hist d: %v", h["d"])
	}
}
