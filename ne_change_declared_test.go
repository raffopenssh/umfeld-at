package main

import (
	"bytes"
	"testing"
)

// TestNEDeclaredView — the declared layer as a view on the chunk log
// (ne_change_declared.go): confirmed-or-baseline per chunk, candidates never
// shown, versions older than the bev build skipped, lu untouched, ETag moves.
func TestNEDeclaredView(t *testing.T) {
	_, cleanup := newNEPublicEnv(t)
	defer cleanup()
	neChangeLRU = &neChangeLRUT{kgs: map[string]*neChangeKG{}}
	neDeclStateMemo.m = map[string]*neDeclState{}
	lu, _ := neSrcRows(t, "63330")
	order, rows := neTestChunks(t, lu, 2)
	k := neStore.get("63330")
	bevAt := neDeclaredAfter(k)
	if bevAt == "" {
		t.Fatal("fixture should hold a bev section")
	}
	// a declared cell of chunk 0; make a variant of the chunk with its use set to all-forest
	base := neStrip(rows[order[0]])
	var cell uint64
	var off int
	for i := 0; i+neLURowBytes <= len(base); i += neLURowBytes {
		if cell == 0 || base[i+8] > base[off+8] {
			cell, off = neRowID(base, i/neLURowBytes, neLURowBytes), i
		}
	}
	if cell == 0 {
		t.Fatal("no cell")
	}
	variant := func(group int) []byte {
		v := append([]byte(nil), base...)
		for g := 0; g < 9; g++ {
			v[off+12+g] = 0
		}
		v[off+12+group] = 255
		return v
	}
	dominant := func(row []byte) int { return neDominantClass(row) }
	declared := func() ([]byte, string) {
		res := neCollect(nil, []uint64{cell}, []string{"lu"}, &neReq{public: true, format: "bin", bevFirst: true})
		if len(res.ids) != 1 {
			t.Fatalf("cell not served: %+v", res.ids)
		}
		return res.lu[0], res.etag(&neReq{public: true, format: "bin", bevFirst: true})
	}
	plain := func() []byte {
		res := neCollect(nil, []uint64{cell}, []string{"lu"}, &neReq{public: true, format: "bin"})
		return res.lu[0]
	}
	row0, etag0 := declared()
	_ = bytes.Equal // row0: with no log the glyph reads bev/lu
	app := func(observer, at string, v []byte) {
		if err := neChangeAppend("63330", neChunkVersion{Observer: observer, Source: "bevdirect@v0.3.6", At: at}, []neChangeItem{{Chunk: neCellHex(order[0]), Digest: neD16(v), AP: neAPDigest(v), Rows: v}}); err != nil {
			t.Fatal(err)
		}
	}
	// 1. a version OLDER than the bev build: skipped, bev/lu row stays
	old := variant(5)
	app("obs-a", "2000-01-01T00:00:00Z", old)
	if r, _ := declared(); dominant(r) == 5 {
		t.Fatalf("a version older than bev must not win")
	}
	// 2. a later version is a candidate until a second observer sights it; then it shows
	v1 := variant(4)
	app("obs-a", "2099-01-01T00:00:00Z", v1)
	if r, _ := declared(); dominant(r) == 4 {
		t.Fatalf("candidate shown")
	}
	if n := neChangeConfirm("63330", "", "obs-b", "bevdirect@v0.3.6", "2026-10", "2099-01-01T01:00:00Z", map[string]string{neCellHex(order[0]): neAPDigest(v1)}); n != 1 {
		t.Fatalf("confirm %d", n)
	}
	r1, etag1 := declared()
	if dominant(r1) != 4 || etag1 == etag0 {
		t.Fatalf("confirmed version not shown: dom=%d etag same=%v", dominant(r1), etag1 == etag0)
	}
	if dominant(plain()) == 4 && dominant(row0) != 4 {
		t.Fatalf("lu must stay pure")
	}
	// 3. a later candidate (one sighting) is NOT shown; once confirmed it is
	v2 := variant(2)
	app("obs-a", "2099-01-02T00:00:00Z", v2)
	if r, _ := declared(); dominant(r) != 4 {
		t.Fatalf("candidate shown: dom=%d", dominant(r))
	}
	if n := neChangeConfirm("63330", "", "obs-b", "bevdirect@v0.3.6", "2026-10", "2099-01-03T00:00:00Z", map[string]string{neCellHex(order[0]): neAPDigest(v2)}); n != 1 {
		t.Fatalf("confirm %d", n)
	}
	r3, etag3 := declared()
	if dominant(r3) != 2 || etag3 == etag1 {
		t.Fatalf("confirmed version not shown: dom=%d", dominant(r3))
	}
	// 4. the cell card path agrees; chunk 1 (no log rows) still comes from bev/lu
	if row, _, _ := neStore.declaredRow(cell, true); dominant(row) != 2 {
		t.Fatalf("declaredRow: %d", dominant(row))
	}
	other := neRowID(rows[order[1]], 0, neLURowBytes)
	if row, _, _ := neStore.declaredRow(other, true); row == nil {
		t.Fatalf("chunk without log rows lost")
	}
	// 5. a chunk with a single (baseline) version newer than bev shows it at once
	b1 := neStrip(rows[order[1]])
	for g := 0; g < 9; g++ {
		b1[12+g] = 0
	}
	b1[12+7] = 255
	if err := neChangeAppend("63330", neChunkVersion{Observer: "obs-a", Source: "bevdirect@v0.3.6", At: "2099-01-05T00:00:00Z"}, []neChangeItem{{Chunk: neCellHex(order[1]), Digest: neD16(b1), AP: neAPDigest(b1), Rows: b1}}); err != nil {
		t.Fatal(err)
	}
	if row, _, _ := neStore.declaredRow(other, true); dominant(row) != 7 {
		t.Fatalf("baseline not shown: %d", dominant(row))
	}
	if NEDeclStateStats()["with_view"].(int) != 1 {
		t.Fatalf("stats: %v", NEDeclStateStats())
	}
}

// TestNEDeclaredViewLogOnlyChunk — a res-9 chunk the KG's .nex never held
// (an index-epoch hole) but a peer's logged build supplies: served by the
// cell paths (chunkHits → Dir = -1), not only by the whole-KG view. The
// white hexes of 04015 Klausenleopoldsdorf, 2026-10-30.
func TestNEDeclaredViewLogOnlyChunk(t *testing.T) {
	_, cleanup := newNEPublicEnv(t)
	defer cleanup()
	neChangeLRU = &neChangeLRUT{kgs: map[string]*neChangeKG{}}
	neDeclStateMemo.m = map[string]*neDeclState{}
	lu, _ := neSrcRows(t, "63330")
	order, rows := neTestChunks(t, lu, 1)
	k := neStore.get("63330")
	have := map[uint64]bool{}
	for i := range k.Hdr.Dir {
		have[k.Hdr.Dir[i].c9] = true
	}
	// a res-9 chunk outside the .nex: walk the ring of the first logged chunk's parent
	c9 := h3Parent(order[0], neChunkRes)
	var c9out uint64
	for _, c := range neGridDisk(c9, 3) {
		if !have[c] {
			c9out = c
			break
		}
	}
	if c9out == 0 {
		t.Fatal("no res-9 chunk outside the fixture")
	}
	if len(neStore.chunkHits(c9out)) != 0 {
		t.Fatal("chunk should be unheld before the log")
	}
	c10 := neChildren(c9out, neKRes)[0]
	tmpl := neStrip(rows[order[0]])[:neLURowBytes]
	var v []byte
	for _, id := range neChildren(c10, neCellRes) {
		r := append([]byte(nil), tmpl...)
		for i := 0; i < 8; i++ {
			r[i] = byte(id >> (8 * i))
		}
		for g := 0; g < 9; g++ {
			r[12+g] = 0
		}
		r[12+4] = 255
		v = append(v, r...)
	}
	v = neSortRows(v, neLURowBytes)
	if err := neChangeAppend("63330", neChunkVersion{Observer: "obs-a", Source: "bevdirect@v0.3.6", At: "2099-01-05T00:00:00Z"}, []neChangeItem{{Chunk: neCellHex(c10), Digest: neD16(v), AP: neAPDigest(v), Rows: v}}); err != nil {
		t.Fatal(err)
	}
	hits := neStore.chunkHits(c9out)
	if len(hits) != 1 || hits[0].Dir != -1 || hits[0].KG.KG != "63330" {
		t.Fatalf("log-only chunk not in chunkHits: %+v", hits)
	}
	cell := neRowID(v, 0, neLURowBytes)
	row, _, err := neStore.declaredRow(cell, true)
	if err != nil || row == nil || neDominantClass(row) != 4 {
		t.Fatalf("declaredRow of a log-only cell: row=%v err=%v", row != nil, err)
	}
	res := neCollect(nil, []uint64{cell}, []string{"lu"}, &neReq{public: true, format: "bin", bevFirst: true})
	if len(res.ids) != 1 || neDominantClass(res.lu[0]) != 4 {
		t.Fatalf("neCollect declared view: %d ids", len(res.ids))
	}
	if r := neCollect(nil, []uint64{cell}, []string{"lu"}, &neReq{public: true, format: "bin"}); len(r.ids) != 0 {
		t.Fatalf("lu must stay pure: %d ids", len(r.ids))
	}
	st := NEStoreStats()
	if st["log_only_chunks"].(int) != 1 {
		t.Fatalf("size line: %v", st["log_only_chunks"])
	}
}
