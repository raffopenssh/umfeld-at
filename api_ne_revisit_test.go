package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// neRevisitFixture — a temp NE dir with hand-written chunk logs (size only —
// the derived file's log_bytes matches, so nothing is decoded) and report files.
func neRevisitFixture(t *testing.T) (dir string, cleanup func()) {
	t.Helper()
	dir = t.TempDir()
	old := neDirOverride(dir)
	statsMemoMapMu.Lock()
	delete(statsMemoMap, neRevisitMemoKey)
	statsMemoMapMu.Unlock()
	neReportsScanMemo.Lock()
	neReportsScanMemo.data, neReportsScanMemo.kgs = nil, nil
	neReportsScanMemo.Unlock()
	row := func(chunk, cell, status, fromSrc, toAP, toAt, toObs string, dlu [9]int) string {
		b, _ := json.Marshal(map[string]interface{}{"cell": cell, "chunk": chunk, "status": status, "d_lu_m2": dlu,
			"from": map[string]string{"digest_ap": "aaaa", "at": "2026-10-01T00:00:00Z", "observer": "obs-base", "source": fromSrc},
			"to":   map[string]string{"digest_ap": toAP, "at": toAt, "observer": toObs, "source": "bevdirect@v0.3.4"}})
		return string(b)
	}
	writeLog := func(kg string, rows ...string) {
		log := []byte(strings.Repeat("x", 100+len(kg)))
		os.WriteFile(filepath.Join(dir, kg+".chunks.zst"), log, 0o644)
		sum, _ := json.Marshal(map[string]interface{}{"schema": neChangeSummarySchema, "kg": kg, "log_bytes": len(log), "versions": 1 + len(rows)})
		os.WriteFile(filepath.Join(dir, kg+".changes.jsonl"), []byte(string(sum)+"\n"+strings.Join(rows, "\n")+"\n"), 0o644)
	}
	// 51119: two pending versions (one old, one new), different observers, 2 cells each side
	writeLog("51119",
		row("c1", "e1", "candidate", "bevdirect@v0.3.3", "ap1", "2026-10-10T01:00:00Z", "peer-vm-1", [9]int{100, 0, 0, -100, 0, 0, 0, 0, 0}),
		row("c1", "e2", "jitter", "bevdirect@v0.3.3", "ap1", "2026-10-10T01:00:00Z", "peer-vm-1", [9]int{3, 0, 0, -3, 0, 0, 0, 0, 0}),
		row("c2", "e3", "candidate", "bevdirect@v0.3.3", "ap2", "2026-10-21T01:00:00Z", "map-peer", [9]int{0, 50, 0, -50, 0, 0, 0, 0, 0}))
	// 73510: one pending version, single observer
	writeLog("73510",
		row("c9", "e9", "candidate", "bevdirect@v0.3.3", "ap9", "2026-10-15T01:00:00Z", "map-peer", [9]int{0, 0, 0, -7, 7, 0, 0, 0, 0}))
	// 40014: confirmed only
	writeLog("40014",
		row("c5", "e5", "confirmed", "bevdirect@v0.3.3", "ap5", "2026-10-12T01:00:00Z", "peer-vm-2", [9]int{10, 0, 0, -10, 0, 0, 0, 0, 0}))
	// 42005: superseded candidate (chunk has a newer version that is confirmed) → nothing pending
	writeLog("42005",
		row("c7", "e7", "candidate", "bevdirect@v0.3.3", "ap7a", "2026-10-11T01:00:00Z", "peer-vm-3", [9]int{10, 0, 0, -10, 0, 0, 0, 0, 0}),
		row("c7", "e7", "confirmed", "bevdirect@v0.3.4", "ap7b", "2026-10-13T01:00:00Z", "peer-vm-4", [9]int{5, 0, 0, -5, 0, 0, 0, 0, 0}))
	// 01002: baseline only (log, no rows); pipeline-only 01004
	writeLog("01002")
	writeLog("01004", row("c8", "e8", "pipeline", "bevdirect@v0.3.3", "ap8", "2026-10-11T01:00:00Z", "peer-vm-3", [9]int{10, 0, 0, -10, 0, 0, 0, 0, 0}))
	// reports: 01002 one chunk-bearing report (stale, old), 01005 one but young, 51119 two, 01006 header-only
	rep := func(kg, at, obs string, chunks int) string {
		c := map[string]string{}
		for i := 0; i < chunks; i++ {
			c[fmt.Sprintf("8a%013x", i)] = "d"
		}
		b, _ := json.Marshal(map[string]interface{}{"received_at": at, "token": "peer", "report": map[string]interface{}{"kg": kg, "algo": "ne-cells-2",
			"source": "bevdirect@v0.3.4", "observer": obs, "chunk_res": 10, "cells_n": 10 * chunks, "chunks": c, "bbox": []float64{10.42, 47.1, 10.5, 47.18}}, "result": map[string]interface{}{}})
		return string(b)
	}
	w := func(kg string, lines ...string) {
		os.WriteFile(filepath.Join(dir, kg+".reports.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	}
	w("01002", rep("01002", "2026-10-02T00:00:00Z", "peer-vm-1", 3))
	w("01005", rep("01005", time.Now().UTC().Add(-time.Hour).Format(time.RFC3339), "map-peer", 3))
	w("51119", rep("51119", "2026-10-01T00:00:00Z", "peer-vm-1", 3), rep("51119", "2026-10-21T00:00:00Z", "map-peer", 3))
	w("01006", rep("01006", "2026-10-01T00:00:00Z", "peer-vm-1", 0))
	return dir, func() {
		neDirOverride(old)
		statsMemoMapMu.Lock()
		delete(statsMemoMap, neRevisitMemoKey)
		statsMemoMapMu.Unlock()
		neReportsScanMemo.Lock()
		neReportsScanMemo.data, neReportsScanMemo.kgs = nil, nil
		neReportsScanMemo.Unlock()
	}
}

func TestNERevisitHandler(t *testing.T) {
	_, cleanup := neRevisitFixture(t)
	defer cleanup()
	const tok = "0123456789abcdef0123"
	setTierConfigForTest(true, tok)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/ne/", handleNE)
	h := tierMiddleware(mux)
	get := func(path string) (int, map[string]interface{}) {
		rr := httptest.NewRecorder()
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		h.ServeHTTP(rr, r)
		var js map[string]interface{}
		json.Unmarshal(rr.Body.Bytes(), &js)
		return rr.Code, js
	}
	code, js := get("/k/api/v1/ne/revisit")
	if code != 200 {
		t.Fatalf("code %d %v", code, js)
	}
	pend, _ := js["pending"].([]interface{})
	if len(pend) != 2 {
		t.Fatalf("pending: %v", js["pending"])
	}
	p0, p1 := pend[0].(map[string]interface{}), pend[1].(map[string]interface{})
	if p0["kg"] != "51119" || p0["pending_versions"].(float64) != 2 || p0["pending_cells"].(float64) != 2 || p0["candidate_m2"].(float64) != 150 ||
		p0["oldest_since"] != "2026-10-10T01:00:00Z" || p0["newest_since"] != "2026-10-21T01:00:00Z" || p0["last_observer"] != "map-peer" || p0["baseline_source"] != "bevdirect@v0.3.3" {
		t.Fatalf("51119: %v", p0)
	}
	if p0["bbox"] != "10.4200,47.1000,10.5000,47.1800" { // layout the held versions were built under
		t.Fatalf("pending bbox: %v", p0)
	}
	if p1["kg"] != "73510" || p1["pending_versions"].(float64) != 1 {
		t.Fatalf("73510: %v", p1)
	}
	for _, k := range []string{"generated_at", "note", "stale", "totals"} {
		if _, ok := js[k]; !ok {
			t.Fatalf("missing %s", k)
		}
	}
	stale, _ := js["stale"].([]interface{})
	if len(stale) != 1 || stale[0].(map[string]interface{})["kg"] != "01002" || stale[0].(map[string]interface{})["reports"].(float64) != 1 || stale[0].(map[string]interface{})["last_observer"] != "peer-vm-1" || stale[0].(map[string]interface{})["bbox"] != "10.4200,47.1000,10.5000,47.1800" {
		t.Fatalf("stale: %v", js["stale"])
	}
	// min_age_days=0 → the young one appears too, oldest first; limit=1 clips
	_, js = get("/k/api/v1/ne/revisit?min_age_days=0")
	if stale, _ = js["stale"].([]interface{}); len(stale) != 2 || stale[1].(map[string]interface{})["kg"] != "01005" {
		t.Fatalf("stale young: %v", js["stale"])
	}
	_, js = get("/k/api/v1/ne/revisit?limit=1&min_age_days=0")
	if len(js["pending"].([]interface{})) != 1 || len(js["stale"].([]interface{})) != 1 || js["limit"].(float64) != 1 {
		t.Fatalf("limit: %v", js)
	}
	// observer filter: 73510's only pending version is map peer's own → dropped; 51119 (two observers) stays
	_, js = get("/k/api/v1/ne/revisit?observer=map-peer")
	pend, _ = js["pending"].([]interface{})
	if len(pend) != 1 || pend[0].(map[string]interface{})["kg"] != "51119" {
		t.Fatalf("observer filter: %v", js["pending"])
	}
	if s := NERevisitStats(); s["pending_rows"].(int) != 2 || s["stale_rows"].(int) != 2 {
		t.Fatalf("stats: %v", s)
	}
	if cacheablePath("/api/v1/ne/revisit") {
		t.Fatal("revisit must not be cacheable (live values)")
	}
}

func TestNERevisitTiers(t *testing.T) {
	_, cleanup := neRevisitFixture(t)
	defer cleanup()
	const tok = "0123456789abcdef0123"
	const ctok = "contribcontribcontrib01"
	setTierConfigForTest(true, tok)
	setContributorConfigForTest([]string{ctok}, []string{"peer"})
	defer setContributorConfigForTest(nil, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/ne/", handleNE)
	h := tierMiddleware(mux)
	get := func(path, bearer string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		r := httptest.NewRequest("GET", path, nil)
		r.RemoteAddr = "10.0.0.9:1234"
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		h.ServeHTTP(rr, r)
		return rr
	}
	for _, c := range []struct {
		path, tok string
		want      int
	}{
		{"/k/api/v1/ne/revisit", "", 404},
		{"/k/api/v1/ne/revisit", "wrongwrongwrongwrong", 404},
		{"/k/api/v1/ne/revisit", tok, 200},
		{"/contrib/api/v1/ne/revisit", ctok, 200},
		{"/contrib/api/v1/ne/revisit?observer=x&limit=5", tok, 200},
		{"/contrib/api/v1/ne/revisit", "", 404},
		{"/api/v1/ne/revisit", "", 404}, // public tier: never
		{"/api/v1/ne/revisit", ctok, 404},
	} {
		if rr := get(c.path, c.tok); rr.Code != c.want {
			t.Fatalf("%s tok=%q: want %d got %d (%s)", c.path, c.tok, c.want, rr.Code, rr.Body.String())
		} else if c.want == 200 && strings.Contains(rr.Body.String(), "/k/") {
			t.Fatalf("%s: answer must not advertise /k", c.path)
		}
	}
	if isPublicPath("/api/v1/ne/revisit") || !contributorPathAllowed("/api/v1/ne/revisit") {
		t.Fatal("revisit: private + contributor only")
	}
}
