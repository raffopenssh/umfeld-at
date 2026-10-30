package main

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/paulmach/orb/encoding/mvt"
	"github.com/uber/h3-go/v4"
)

func TestMVTEncoderDecodesWithOrb(t *testing.T) {
	l := newMVTLayer("cells")
	f := l.feature(42)
	f.str("d", "wald")
	f.num("el", 512)
	f.num("sl", 12.5)
	f.flag("n2k", true)
	f.i64("n", -3)
	// a square given counter-clockwise on screen: encoder must flip it
	f.ring([][2]int32{{0, 0}, {0, 100}, {100, 100}, {100, 0}}, false)
	f.done()
	g := l.feature(7)
	g.str("d", "wald") // interned: same value index
	g.ring([][2]int32{{200, 200}, {300, 200}, {300, 300}, {200, 300}, {200, 200}}, false)
	g.done()
	b := mvtEncode(l)
	layers, err := mvt.Unmarshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) != 1 || layers[0].Name != "cells" || len(layers[0].Features) != 2 || layers[0].Extent != mvtExtent {
		t.Fatalf("layers %+v", layers)
	}
	if len(l.vals) != 5 {
		t.Errorf("values not interned: %d", len(l.vals))
	}
	p := layers[0].Features[0].Properties
	if p["d"] != "wald" || fmt.Sprint(p["el"]) != "512" || fmt.Sprint(p["sl"]) != "12.5" || p["n2k"] != true || fmt.Sprint(p["n"]) != "-3" {
		t.Errorf("props %#v", p)
	}
	for _, ft := range layers[0].Features {
		if ft.Geometry.GeoJSONType() != "Polygon" {
			t.Errorf("geometry %s", ft.Geometry.GeoJSONType())
		}
	}
	if mvtEncode(newMVTLayer("x")) != nil {
		t.Error("empty layer must encode to nothing")
	}
}

func TestPMTilesIDsAndWriter(t *testing.T) {
	for _, c := range []struct {
		z, x, y int
		id      uint64
	}{{0, 0, 0, 0}, {1, 0, 0, 1}, {1, 0, 1, 2}, {1, 1, 1, 3}, {1, 1, 0, 4}, {2, 0, 0, 5}} {
		if got := pmtTileID(c.z, c.x, c.y); got != c.id {
			t.Errorf("id(%d/%d/%d)=%d want %d", c.z, c.x, c.y, got, c.id)
		}
	}
	pw := newPMTWriter(9, 10, [4]float64{15, 47, 15.1, 47.1}, []byte(`{"name":"t"}`))
	a, b := pmtGzip([]byte("tile-a")), pmtGzip([]byte("tile-b"))
	pw.add(9, 10, 20, a)
	pw.add(10, 20, 40, b)
	pw.add(10, 21, 40, a) // identical content: stored once
	var buf bytes.Buffer
	if _, err := pw.write(&buf); err != nil {
		t.Fatal(err)
	}
	h := buf.Bytes()
	if string(h[:7]) != "PMTiles" || h[7] != 3 || h[99] != 1 || h[97] != 2 || h[98] != 2 {
		t.Fatalf("header %q", h[:8])
	}
	le := binary.LittleEndian
	if le.Uint64(h[72:]) != 3 || le.Uint64(h[80:]) != 3 || le.Uint64(h[88:]) != 2 || le.Uint64(h[64:]) != uint64(len(a)+len(b)) {
		t.Errorf("counts addressed=%d entries=%d contents=%d data=%d", le.Uint64(h[72:]), le.Uint64(h[80:]), le.Uint64(h[88:]), le.Uint64(h[64:]))
	}
	ro, rl := le.Uint64(h[8:]), le.Uint64(h[16:])
	zr, err := gzip.NewReader(bytes.NewReader(h[ro : ro+rl]))
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := io.ReadAll(zr)
	rd := func(b *[]byte) uint64 {
		v, n := binary.Uvarint(*b)
		*b = (*b)[n:]
		return v
	}
	n := rd(&dir)
	if n != 3 {
		t.Fatalf("entries %d", n)
	}
	ids := make([]uint64, n)
	var last uint64
	for i := range ids {
		last += rd(&dir)
		ids[i] = last
	}
	runs := []uint64{rd(&dir), rd(&dir), rd(&dir)}
	lens := []uint64{rd(&dir), rd(&dir), rd(&dir)}
	offs := []uint64{rd(&dir), rd(&dir), rd(&dir)}
	if ids[0] != pmtTileID(9, 10, 20) || ids[1] != pmtTileID(10, 20, 40) || ids[2] != pmtTileID(10, 21, 40) {
		t.Errorf("ids %v", ids)
	}
	if runs[0] != 1 || lens[0] != uint64(len(a)) || lens[1] != uint64(len(b)) || lens[2] != uint64(len(a)) {
		t.Errorf("runs %v lens %v", runs, lens)
	}
	if offs[0] != 1 || offs[1] != 0 || offs[2] != 1 { // first absolute(+1), second contiguous, third back to 0 (+1)
		t.Errorf("offs %v", offs)
	}
	do := le.Uint64(h[56:])
	if !bytes.Equal(h[do:do+uint64(len(a))], a) {
		t.Error("tile data mismatch")
	}
}

func TestH3ParentFast(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 200; i++ {
		lon, lat := 9.5+rng.Float64()*7.5, 46.4+rng.Float64()*2.6
		c, _ := h3.LatLngToCell(h3.NewLatLng(lat, lon), 12)
		for p := 5; p <= 12; p++ {
			want, _ := c.Parent(p)
			if got := neH3ParentFast(uint64(c), p); got != uint64(want) {
				t.Fatalf("parent(%x,%d)=%x want %x", uint64(c), p, got, uint64(want))
			}
		}
	}
	for z, r := range map[int]int{8: 7, 9: 7, 10: 8, 11: 9, 12: 10, 13: 11, 14: 12, 15: 12} {
		if neMVTZoomRes(z) != r {
			t.Errorf("zoom %d → res %d", z, neMVTZoomRes(z))
		}
	}
	// tile maths round-trips
	w, s, e, n := mvtTileBox(14, 8876, 5732)
	x0, y0, x1, y1 := mvtTilesInBox(14, w+1e-6, s+1e-6, e-1e-6, n-1e-6)
	if x0 != 8876 || x1 != 8876 || y0 != 5732 || y1 != 5732 {
		t.Errorf("tiles in box %d %d %d %d", x0, y0, x1, y1)
	}
	px, py := mvtProject(14, 8876, 5732, w, n)
	if px < -0.01 || px > 0.01 || py < -0.01 || py > 0.01 {
		t.Errorf("project corner %f %f", px, py)
	}
}

func TestNEMVTGate(t *testing.T) {
	m := neMVTParseAccess("alice:16.2,48.1,16.6,48.4|15,47,15.5,47.3:5000:400; bob:*:20000; anon:*:100:50")
	if len(m) != 3 || len(m["alice"].boxes) != 2 || m["alice"].tiles != 5000 || m["alice"].km2 != 400 || len(m["bob"].boxes) != 0 || m["bob"].tiles != 20000 || m["bob"].km2 != neMVTDefKm2 {
		t.Fatalf("parse %+v", m)
	}
	if !neMVTBoxAllowed(m["alice"], 16.3, 48.2, 16.31, 48.21) || neMVTBoxAllowed(m["alice"], 13, 47, 13.1, 47.1) || !neMVTBoxAllowed(m["bob"], 13, 47, 13.1, 47.1) {
		t.Error("box allow-list")
	}
	if k := neMVTBoxKm2(15, 47, 16, 48); k < 8300 || k > 8500 {
		t.Errorf("km2 %f", k)
	}
	neMVTGateLoad()
	saveE, saveA := neMVTGateEnforce, neMVTAccessByNm
	defer func() { neMVTGateEnforce, neMVTAccessByNm = saveE, saveA; neMVTUse_ = map[string]*neMVTUse{} }()
	neMVTAccessByNm = m
	req := func(ip string) *http.Request {
		r := httptest.NewRequest("GET", "/api/v1/ne/mvt/14/1/1.pbf", nil)
		r.RemoteAddr = ip + ":1"
		return r
	}
	// not enforced: everything passes, denials are counted
	neMVTGateEnforce = false
	neMVTUse_ = map[string]*neMVTUse{}
	for i := 0; i < 120; i++ {
		w := httptest.NewRecorder()
		if _, ok := neMVTGate(w, req("10.0.0.1"), 1, 13, 47, 13.01, 47.01); !ok {
			t.Fatal("gate off must admit")
		}
		if w.Header().Get("RateLimit-Limit") != "100" {
			t.Fatalf("headers %v", w.Header())
		}
	}
	if neMVTUse_["ip:10.0.0.1"].denied == 0 {
		t.Error("over-budget must be counted while off")
	}
	// enforced: budget 429 with Retry-After, unknown name 403
	neMVTGateEnforce = true
	neMVTUse_ = map[string]*neMVTUse{}
	var last *httptest.ResponseRecorder
	for i := 0; i < 101; i++ {
		last = httptest.NewRecorder()
		neMVTGate(last, req("10.0.0.2"), 1, 13, 47, 13.01, 47.01)
	}
	if last.Code != 429 || last.Header().Get("Retry-After") == "" {
		t.Errorf("101st tile: %d %v", last.Code, last.Header())
	}
	delete(neMVTAccessByNm, "anon")
	w := httptest.NewRecorder()
	if _, ok := neMVTGate(w, req("10.0.0.3"), 1, 13, 47, 13.01, 47.01); ok || w.Code != 403 {
		t.Errorf("no entry: %d", w.Code)
	}
	if neMVTGateName("token:obs-peer") != "obs-peer" || neMVTGateName("ip:1.2.3.4") != "anon" || neMVTGateName("key:x") != "x" {
		t.Error("gate names")
	}
}

func TestNEMVTPublicAndContrib(t *testing.T) {
	for _, p := range []string{"/api/v1/ne/mvt/14/8876/5732.pbf", "/api/v1/ne/mvt.json", "/api/v1/ne/mvt/status", "/api/v1/ne/pmtiles", "/api/v1/ne/pmtiles/jobs"} {
		if !isPublicPath(p) {
			t.Errorf("%s must be public (key optional, like the unit export)", p)
		}
		if !contributorPathAllowed(p) {
			t.Errorf("%s must open under /contrib for a contributor token", p)
		}
	}
	for _, f := range []string{"ne_mvt.go", "ne_mvt_jobs.go", "ne_mvt_enc.go", "ne_pmtiles.go", "docs/qgis-layer.md"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if bad := nePublicBannedWords(string(b)); len(bad) > 0 {
			t.Errorf("%s: banned words %v", f, bad)
		}
	}
	// out-of-range zooms / outside Austria answer 204, never work
	for _, p := range []string{"/api/v1/ne/mvt/5/16/10.pbf", "/api/v1/ne/mvt/14/0/0.pbf"} {
		w := httptest.NewRecorder()
		handleNEMVT(w, httptest.NewRequest("GET", p, nil), p[len("/api/v1/ne/"):])
		if w.Code != 204 {
			t.Errorf("%s → %d", p, w.Code)
		}
	}
	w := httptest.NewRecorder()
	handleNEMVT(w, httptest.NewRequest("GET", "/api/v1/ne/mvt/14/x/y.pbf", nil), "mvt/14/x/y.pbf")
	if w.Code != 400 {
		t.Errorf("bad coords → %d", w.Code)
	}
}
