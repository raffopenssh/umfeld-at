package main

import (
	"bytes"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The tree sheet's parts that need no peer: the block list of a tile (own blocks + edge neighbours, no
// diagonal), the inks, the downscale posteriser, the pic views' route and the QGIS project's stack.
func TestNETrs(t *testing.T) {
	L := neGlyphLODs[2]
	w, s, e, n := neGlyphTileBox(L, 147, 470)
	bl := neTrsBlocksOf(L, w, s, e, n)
	if len(bl) != 5 {
		t.Fatalf("LOD 2 tile reads %d blocks, want its own + 4 edge neighbours: %v", len(bl), bl)
	}
	for _, m := range []string{"p", "l", "d"} {
		p := neTrsPalette(m)
		want := 1 + len(neGlyphTreeRGB)
		if m != "p" {
			want = 1 + neTrsSteps
		}
		if len(p) != want {
			t.Errorf("palette %s: %d inks, want %d", m, len(p), want)
		}
		if lo, hi := neTrsInk(m, 0), neTrsInk(m, 200); lo != 1 || int(hi) != want-1 {
			t.Errorf("ink %s: %d..%d, want 1..%d", m, lo, hi, want-1)
		}
	}
	if neTrsInk("l", 2*24) == neTrsInk("l", 2*30) {
		t.Error("heights: 24 m and 30 m share an ink")
	}
	// a block the peer never answers: the tile comes back at once, pending, never kept
	old := neTrsBase
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(503)
	}))
	defer srv.Close()
	neTrsBase = func() string { return srv.URL }
	defer func() { neTrsBase = old }()
	if err := neGlyphLoadFrame(); err != nil {
		t.Skip("no frame:", err)
	}
	g, tw, ts, te, tn, ok := neGlyphTileGeomBleed(L, 147, 470, neGlyphTilePicBleed)
	if !ok {
		t.Fatal("geom")
	}
	var m neTrsMeta
	t0 := time.Now()
	neTrsWarm(&g, tw, ts, te, tn, 3*time.Second)
	img, gens := neTrsRender(&g, tw, ts, te, tn, "l", 0, &m)
	if img == nil || m.pend != len(gens) || m.pend == 0 {
		t.Fatalf("pending render: img=%v pend=%d gens=%v", img != nil, m.pend, gens)
	}
	if time.Since(t0) > 2500*time.Millisecond {
		t.Errorf("a refusing peer held the tile %v", time.Since(t0))
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if b := neTrsPosterise(neGlyphTileDownsample(buf.Bytes(), 4)); len(b) == 0 {
		t.Error("posterise: empty")
	}
	if st := NETrsStats(); st["cap_bytes"].(int) != neTrsMaxMB<<20 {
		t.Error("stats: no cap")
	}
}
