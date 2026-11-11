package main

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestNEStatsParseShare(t *testing.T) {
	k, v, f := neStatsParseShare("#kg=63330;v=obs;f=h%3E%3D8,sl%3D15..30;full;at=1,2,3")
	if k != "kg" || v != "63330" || f != "h>=8,sl=15..30" {
		t.Fatalf("got %q %q %q", k, v, f)
	}
	k, v, _ = neStatsParseShare("h3=871e108d9ffffff;trees")
	if k != "h3" || v != "871e108d9ffffff" {
		t.Fatalf("got %q %q", k, v)
	}
	if k, _, _ := neStatsParseShare("#v=obs;change"); k != "" {
		t.Fatalf("view-only hash must give no unit, got %q", k)
	}
}

func TestNEStatsUnitsFromShare(t *testing.T) {
	q := url.Values{"share": {"#h3=871e108d9ffffff;f=forest"}}
	units, msg, _ := neStatsUnits(q)
	if msg != "" || len(units) != 1 || units[0].kind != "h3" || units[0].key != "871e108d9ffffff" {
		t.Fatalf("units %v msg %q", units, msg)
	}
	if q.Get("f") != "forest" {
		t.Fatalf("filter not lifted from the hash: %q", q.Get("f"))
	}
	u := units[0]
	if u.total != 16807 {
		t.Fatalf("res 7 → 7^5 cells expected, got %d", u.total)
	}
	if !(u.scan[0] < u.bbox[0] && u.scan[2] > u.bbox[2]) {
		t.Fatalf("scan box must pad the hexagon bbox: %v vs %v", u.scan, u.bbox)
	}
	if u.member(0x8c1e108d92403ff, 0, 0) != 1 || u.member(0x8c1e108cc922dff, 0, 0) != 0 {
		t.Fatal("membership = h3 ancestry")
	}
	for _, bad := range []url.Values{{"h3": {"zz"}}, {"bbox": {"1,2,3"}}, {"bbox": {"5,40,5.1,40.1"}}, {}} {
		if _, msg, code := neStatsUnits(bad); msg == "" || code == 0 {
			t.Fatalf("%v must fail", bad)
		}
	}
	if _, msg, code := neStatsUnits(url.Values{"bbox": {"15.0,47.0,15.05,47.05"}}); msg != "" || code != 0 {
		t.Fatalf("bbox unit: %q", msg)
	}
}

func TestNEStatsInRings(t *testing.T) {
	sq := [][][2]float64{{{0, 0}, {1, 0}, {1, 1}, {0, 1}}, {{0.4, 0.4}, {0.6, 0.4}, {0.6, 0.6}, {0.4, 0.6}}} // square with a hole
	for _, c := range []struct {
		x, y float64
		in   bool
	}{{0.2, 0.2, true}, {0.5, 0.5, false}, {1.5, 0.5, false}, {0.9, 0.1, true}} {
		if got := neStatsInRings(sq, c.x, c.y); got != c.in {
			t.Errorf("(%v,%v) in=%v want %v", c.x, c.y, got, c.in)
		}
	}
}

func TestNEStatsPeerSum(t *testing.T) {
	us := []map[string]interface{}{
		{"unit": map[string]interface{}{"key": "a"}, "coverage": map[string]interface{}{"complete": true}, "figures": map[string]interface{}{"area_ha": 100.0, "claimed_ha": 10.0, "farms_n": 3.0, "bio_pct": 50.0, "top_snar.1.ha": 5.0, "gemeinde.total_eur": 100.0, "terrain.elev_mean_m": 500.0}},
		{"unit": map[string]interface{}{"key": "b"}, "coverage": map[string]interface{}{"complete": false}, "figures": map[string]interface{}{"area_ha": 300.0, "claimed_ha": 30.0, "farms_n": 4.0, "bio_pct": 10.0, "top_snar.1.ha": 7.0, "gemeinde.total_eur": 50.0, "terrain.elev_mean_m": 700.0}},
	}
	out := neStatsPeerSum(us, "61630")
	fg := out["figures"].(map[string]interface{})
	if fg["area_ha"] != 400.0 || fg["claimed_ha"] != 40.0 || fg["farms_n"] != 7.0 || fg["units_n"] != 2 {
		t.Fatalf("sums wrong: %v", fg)
	}
	if fg["claimed_pct"] != 10.0 {
		t.Fatalf("claimed_pct recomputed from area: %v", fg["claimed_pct"])
	}
	for _, k := range []string{"bio_pct", "top_snar.1.ha", "gemeinde.total_eur", "terrain.elev_mean_m"} {
		if _, has := fg[k]; has {
			t.Errorf("%s must not be summed", k)
		}
	}
	if c := out["coverage"].(map[string]interface{}); c["complete"] != false {
		t.Fatal("one incomplete member → incomplete")
	}
}

func TestNEStatsFlatCSV(t *testing.T) {
	e := &neStatsEnvelope{Schema: neStatsSchema, Service: neStatsService,
		Unit:     map[string]interface{}{"kind": "kg", "key": "63330", "name": "Kohl, schwarz", "bbox": []float64{15, 47, 15.1, 47.1}},
		Coverage: map[string]interface{}{"complete": true},
		Figures:  map[string]interface{}{"cells_n": 5, "area_ha": 1.5, "declared_ha.wald": 1.0, "kg.name": "x"},
		Peers:    map[string]interface{}{"obs-peer": map[string]interface{}{"source": "srtm@2.4", "coverage": map[string]interface{}{"complete": true}, "figures": map[string]interface{}{"trees_n": 9.0}}},
		Link:     "https://umfeld.example/#kg=63330"}
	row, order := neStatsFlat(e, nil)
	if row["unit.name"] != "Kohl, schwarz" || row["srtm.trees_n"] != 9.0 || row["sub.available"] != false || row["srtm.available"] != true {
		t.Fatalf("flat row: %v", row)
	}
	if order[0] != "unit.kind" || order[len(order)-1] != "link" {
		t.Fatalf("order: %v", order)
	}
	if neStatsCSVCell("Kohl, schwarz") != "\"Kohl, schwarz\"" || neStatsCSVCell(1.5) != "1.5" || neStatsCSVCell(nil) != "" || neStatsCSVCell(true) != "true" {
		t.Fatal("csv cells")
	}
	row, _ = neStatsFlat(e, []string{"declared_ha", "srtm.trees_n"})
	if len(row) != 2 {
		t.Fatalf("keys= prefix filter: %v", row)
	}
	// figures stay scalar (spreadsheet contract)
	for k, v := range e.Figures {
		switch v.(type) {
		case int, float64, string, bool, nil:
		default:
			t.Errorf("%s: non-scalar figure %T", k, v)
		}
	}
}

func TestNEStatsAdditive(t *testing.T) {
	for k, want := range map[string]bool{"area_ha": true, "class_ha.acker": true, "schlaege_n": true, "gemeinde.total_eur": false, "claimed_pct": false, "top_snar.1.ha": false, "als.newest_year": false, "height.max_m": false, "consistency_n.consistent": true, "ndvi.mean": false} {
		if neStatsAdditive(k) != want {
			t.Errorf("%s additive=%v want %v", k, !want, want)
		}
	}
}

func TestNEStatsSortedKeys(t *testing.T) {
	ks := neStatsSortedKeys(map[string]interface{}{"terrain.x": 1, "cells_n": 1, "declared_ha.wald": 1, "area_ha": 1, "zzz": 1, "subsidy.claimed_ha": 1})
	if strings.Join(ks, " ") != "cells_n area_ha declared_ha.wald subsidy.claimed_ha terrain.x zzz" {
		t.Fatalf("order %v", ks)
	}
}

func TestExportGateCountsOnly(t *testing.T) {
	exportLoadTiers()
	was := exportEnforce
	exportEnforce = false
	defer func() { exportEnforce = was }()
	for i := 0; i < 15; i++ { // > anon burst of 10 — still admitted, counted as limited
		rr := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/v1/ne/stats/unit?kg=63330&src=test", nil)
		r.Header.Set("X-Forwarded-For", "203.0.113.9")
		id, ok := exportGate(rr, r)
		if !ok || id != "ip:203.0.113.9" {
			t.Fatalf("i=%d ok=%v id=%s", i, ok, id)
		}
		if rr.Header().Get("RateLimit-Limit") == "" || rr.Header().Get("RateLimit-Policy") == "" {
			t.Fatal("rate-limit headers advertised on every answer")
		}
	}
	st := ExportGateStats()
	if st["limited"].(int64) < 5 || st["by_src"].(map[string]int64)["test"] < 15 {
		t.Fatalf("stats %v", st)
	}
	// a key that is one of our tokens → named identity, never echoed
	setTierConfigForTest(true, "0123456789abcdef0123")
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/v1/ne/stats/unit?kg=63330&key=0123456789abcdef0123", nil)
	id, _ := exportGate(rr, r)
	if !strings.HasPrefix(id, "token:") || strings.Contains(id, "0123456789") {
		t.Fatalf("id %q", id)
	}
	b, _ := json.Marshal(ExportGateStats())
	if strings.Contains(string(b), "0123456789abcdef0123") {
		t.Fatal("key leaked into stats")
	}
	rr = httptest.NewRecorder()
	exportGate(rr, httptest.NewRequest("GET", "/x", nil))
	if rr.Code == http.StatusTooManyRequests {
		t.Fatal("local caller limited")
	}
}

// A combined selection writes both lists into the hash: `#kg=a,b;adm=c`.
func TestNEStatsShareCombined(t *testing.T) {
	h := "#kg=63330,63331;adm=AT1103112,70101;v=obs;f=forest"
	if k, v, f := neStatsParseShare(h); k != "kg" || v != "63330,63331" || f != "forest" {
		t.Fatalf("got %q %q %q", k, v, f)
	}
	if a := neStatsShareVal(h, "adm"); a != "AT1103112,70101" {
		t.Fatalf("adm %q", a)
	}
	if a := neStatsShareVal("#kg=63330", "adm"); a != "" {
		t.Fatalf("adm %q", a)
	}
}

// neStatsKGSet: a cell's weight is its share summed over the listed KGs,
// capped at 1 — the same whether a KG is listed alone or with its neighbours.
func TestNEStatsKGSetShare(t *testing.T) {
	row := func(id uint64, sh uint8) []byte {
		b := make([]byte, neLURowBytes)
		binary.LittleEndian.PutUint64(b, id)
		b[8] = sh
		return b
	}
	a := append(append(row(10, 255), row(20, 100)...), row(30, 200)...)
	b := append(append(row(20, 155), row(30, 200)...), row(40, 51)...)
	box := [4]float64{14, 46, 15, 47}
	one := &neStatsKGSet{rows: [][]byte{a}, boxes: [][4]float64{box}}
	two := &neStatsKGSet{rows: [][]byte{a, b}, boxes: [][4]float64{box, box}}
	for _, c := range []struct {
		s    *neStatsKGSet
		id   uint64
		want float64
	}{{one, 10, 1}, {one, 20, 100.0 / 255}, {one, 15, 0}, {one, 99, 0}, {two, 20, 1}, {two, 30, 1}, {two, 40, 51.0 / 255}, {two, 10, 1}, {two, 5, 0}} {
		if got := c.s.share(c.id, 14.5, 46.5); math.Abs(got-c.want) > 1e-9 {
			t.Fatalf("share(%d) = %v, want %v", c.id, got, c.want)
		}
	}
	// a KG whose box is far from the cell is not consulted (several KGs only)
	far := &neStatsKGSet{rows: [][]byte{a, b}, boxes: [][4]float64{box, {16, 48, 16.1, 48.1}}}
	if got := far.share(20, 14.5, 46.5); math.Abs(got-100.0/255) > 1e-9 {
		t.Fatalf("far box consulted: %v", got)
	}
}

// A combined unit is the union: a cell inside two overlapping sites counts
// once, the key is canonical, and units that are not areas do not combine.
func TestNEStatsUnitSet(t *testing.T) {
	sq := func(w, s, e, n float64) [][][2]float64 {
		return [][][2]float64{{{w, s}, {e, s}, {e, n}, {w, n}, {w, s}}}
	}
	site := func(key string, w, s, e, n float64) *neStatsUnit {
		return &neStatsUnit{kind: "natura2000", key: key, name: key, bbox: [4]float64{w, s, e, n}, rings: sq(w, s, e, n)}
	}
	a, b := site("ATB", 14, 46, 14.2, 46.2), site("ATA", 14.1, 46.1, 14.3, 46.3)
	u, msg := neStatsUnitSet([]*neStatsUnit{a, b, a})
	if msg != "" || u.kind != "combined" || u.key != "adm=ATA,ATB" || len(u.boxes) != 2 {
		t.Fatalf("unit %+v msg %q", u, msg)
	}
	if u.bbox != [4]float64{14, 46, 14.3, 46.3} {
		t.Fatalf("bbox %v", u.bbox)
	}
	for _, c := range []struct {
		lon, lat, want float64
	}{{14.05, 46.05, 1}, {14.15, 46.15, 1}, {14.25, 46.25, 1}, {14.25, 46.05, 0}, {14.05, 46.25, 0}} {
		if got := u.member(0, c.lon, c.lat); got != c.want {
			t.Fatalf("member(%v,%v) = %v, want %v", c.lon, c.lat, got, c.want)
		}
	}
	if u.peerQ != "" || u.kgs != nil {
		t.Fatal("a union with an outline member has no KG list for the peers")
	}
	if tok := neStatsUnitToken(u); tok != "adm=ATA,ATB" {
		t.Fatalf("token %q", tok)
	}
	if _, msg := neStatsUnitSet([]*neStatsUnit{a, {kind: "bbox", key: "x"}}); msg == "" {
		t.Fatal("a box must not combine")
	}
	// the bins walked are the members' boxes, not the hull: two far-apart small units stay within the cell-exact cap
	far := site("ATC", 16.5, 48.5, 16.52, 48.52)
	u2, _ := neStatsUnitSet([]*neStatsUnit{site("ATD", 10, 47, 10.02, 47.02), far})
	if len(u2.boxes) != 2 || u2.boxes[1] != far.bbox {
		t.Fatalf("boxes %v", u2.boxes)
	}
}

// A view-clipped unit keeps the unit's weights inside the box, has its own
// memo key and is never queued for the background pass.
func TestNEStatsClip(t *testing.T) {
	u := &neStatsUnit{kind: "natura2000", key: "ATX", bbox: [4]float64{14, 46, 14.4, 46.4}, peerQ: "x",
		member: func(_ uint64, lon, lat float64) float64 { return 0.5 }}
	if neStatsClip(u, [4]float64{15, 47, 15.1, 47.1}) != nil {
		t.Fatal("no overlap must give nil")
	}
	c := neStatsClip(u, [4]float64{14.2, 46.2, 15, 47})
	if c == nil || c.bbox != [4]float64{14.2, 46.2, 14.4, 46.4} || !c.noExact || c.peerQ != "" || c.key == u.key || u.noExact {
		t.Fatalf("clip %+v", c)
	}
	if c.member(0, 14.3, 46.3) != 0.5 || c.member(0, 14.1, 46.3) != 0 || c.member(0, 14.3, 46.1) != 0 {
		t.Fatal("membership = unit ∩ box")
	}
	// a combined unit: member boxes are cut, those outside dropped
	s := &neStatsUnit{kind: "combined", key: "k", bbox: [4]float64{14, 46, 16, 48}, boxes: [][4]float64{{14, 46, 14.1, 46.1}, {15.9, 47.9, 16, 48}},
		member: func(_ uint64, _, _ float64) float64 { return 1 }}
	if cs := neStatsClip(s, [4]float64{14, 46, 14.05, 46.05}); cs == nil || len(cs.boxes) != 1 || cs.boxes[0] != [4]float64{14, 46, 14.05, 46.05} {
		t.Fatalf("combined clip %+v", cs)
	}
	if neStatsClip(s, [4]float64{15, 47, 15.1, 47.1}) != nil {
		t.Fatal("a box between the members holds nothing")
	}
}
