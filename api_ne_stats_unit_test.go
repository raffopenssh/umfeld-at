package main

import (
	"encoding/json"
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
	if exportEnforce {
		t.Skip("enforcement on in this environment")
	}
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
