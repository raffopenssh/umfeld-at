package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A view_rekey padding row (all nodata) is blank; a real row with a height or a
// flight year is not, even when obs_lu is all zero.
func TestNEObsRowBlank(t *testing.T) {
	lay := neObsLayout(neObsFieldsV41, "<Q9sBBBBBBBBBBbBBHBBBBBbbBB")
	if lay == nil {
		t.Fatal("layout")
	}
	blank := make([]byte, lay.rowBytes)
	for i := lay.luOff + 9; i < len(blank); i++ {
		blank[i] = 255
	}
	blank[lay.flight] = 0
	if !neObsRowBlank(blank, lay) {
		t.Fatal("padding row not blank")
	}
	r := append([]byte(nil), blank...)
	r[lay.hMean] = 3
	if neObsRowBlank(r, lay) {
		t.Fatal("row with a height is not blank")
	}
	r = append([]byte(nil), blank...)
	r[lay.flight] = 1
	if neObsRowBlank(r, lay) {
		t.Fatal("row with a flight year is not blank")
	}
	r = append([]byte(nil), blank...)
	r[lay.luOff+4] = 255
	if neObsRowBlank(r, lay) {
		t.Fatal("row with a use is not blank")
	}
}

// view_rekey.coverage="swept": accepted (only that value), kept in the stored
// header, a digest-neutral flip is not `unchanged`, blank rows count as
// nodata_n instead of blank_n and the KG leaves obs_missing unless the view
// drifted; manifest obs{} carries coverage / view_rekey / blank_n / nodata_n.
func TestNEObsCoverageSwept(t *testing.T) {
	dir := t.TempDir()
	old := neDirOverride(dir)
	defer neDirOverride(old)
	savedStore := neStore
	neResetStoreForTest()
	neStore.load()
	defer func() { neStore = savedStore }()
	neObsComplete.mu.Lock()
	neObsComplete.rows = nil
	neObsComplete.mu.Unlock()

	luRec := func(cid uint64) []byte {
		b := make([]byte, 30)
		binary.LittleEndian.PutUint64(b, cid)
		b[8], b[9], b[10], b[16] = 255, 255, 250, 200
		b[28], b[29] = 0xFF, 0xFF
		return b
	}
	kRec := func(cid uint64) []byte {
		k := make([]byte, 29)
		binary.LittleEndian.PutUint64(k, cid)
		k[24], k[27], k[28] = 0xFF, 0xFF, 0xFF
		return k
	}
	const a, b, c, k10 = uint64(0x8c1e108cc0539ff), uint64(0x8c1e108cc0515ff), uint64(0x8c1e108cc0517ff), uint64(0x8a1e108cc057fff)
	nec := func(algo, source string, ids ...uint64) []byte {
		var raw []byte
		for _, id := range ids {
			raw = append(raw, luRec(id)...)
		}
		raw = append(raw, kRec(k10)...)
		return packSection("NEC1", map[string]interface{}{"kg": "63330", "algo": algo, "epoch": "2026-10", "source": source, "h3_res": 12, "cells_n": len(ids), "kcells_n": 1}, raw)
	}
	groups := []string{"geb", "bau", "acker", "gruen", "wald", "wasser", "verkehr", "alpen", "sonst"}
	lay := neObsLayout(neObsFieldsV41, "<Q9sBBBBBBBBBBbBBHBBBBBbbBB")
	fields := make([]interface{}, len(neObsFieldsV41))
	for i, f := range neObsFieldsV41 {
		fields[i] = f
	}
	// rows: a, b observed; c blank (all nodata, no flight)
	obs := func(rekey map[string]interface{}) []byte {
		var raw []byte
		for _, id := range []uint64{a, b, c} {
			row := make([]byte, lay.rowBytes)
			binary.LittleEndian.PutUint64(row, id)
			if id == c {
				for i := lay.luOff + 9; i < len(row); i++ {
					row[i] = 255
				}
				row[lay.flight] = 0
			} else {
				row[lay.luOff+4], row[lay.hMean], row[lay.flight] = 200, 7, 3
			}
			raw = append(raw, row...)
		}
		hdr := map[string]interface{}{"kg": "63330", "algo": "obs-cells-2", "epoch": "2026-11", "source": "obs-peer@2.5", "h3_res": 12,
			"cells_n": 3, "groups": groups, "record_bytes": lay.rowBytes, "fields": fields, "record": "<Q9sBBBBBBBBBBbBBHBBBBBbbBB"}
		if rekey != nil {
			hdr["view_rekey"] = rekey
		}
		return packSection("NEO1", hdr, raw)
	}
	os.WriteFile(filepath.Join(dir, "63330.nec"), nec("ne-cells-2", "index-2026-03", a, b), 0o644)
	neStore.convertPending()
	const srtm, ops = "0123456789abcdef0123", "fedcba9876543210fedc"
	setTierConfigForTest(true, srtm, ops)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/ne/", handleNE)
	h := tierMiddleware(mux)
	do := func(body []byte) (*httptest.ResponseRecorder, map[string]interface{}) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/k/api/v1/ne/63330/obs", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+srtm)
		h.ServeHTTP(rr, req)
		var js map[string]interface{}
		json.Unmarshal(rr.Body.Bytes(), &js)
		return rr, js
	}
	bev := nec("ne-cells-2", "bevdirect@v0.3.1", a, b, c)
	// unknown coverage value → 400
	if rr, _ := do(append(bev, obs(map[string]interface{}{"coverage": "partial"})...)); rr.Code != 400 {
		t.Fatalf("coverage=partial: want 400, got %d %s", rr.Code, rr.Body.String())
	}
	// unswept post: c is blank padding → listed on obs_missing
	rr, js := do(append(bev, obs(map[string]interface{}{"dropped": 0, "filled": 1, "borrowed": 0, "blank": 1})...))
	if rr.Code != 200 || js["coverage"] != nil {
		t.Fatalf("unswept post: %d %s", rr.Code, rr.Body.String())
	}
	neManifest.refresh(true) // the handler's refresh is asynchronous; neEntry must see the obs layer
	neObsCompletePass()
	r := neObsCompareKG("63330")
	if r == nil || r.BlankN != 1 || r.NodataN != 0 || r.Coverage != "" || r.MissingN != 0 || r.ExtraN != 0 {
		t.Fatalf("unswept row: %+v", r)
	}
	if rows := neObsMissingRows(); len(rows) != 1 || rows[0].KG != "63330" {
		t.Fatalf("unswept KG must be listed: %+v", rows)
	}
	st := NEObsCompleteStats()
	if st["cells_obs_blank"] != 1 || st["kgs_obs_blank"] != 1 || st["cells_obs_nodata"] != 0 {
		t.Fatalf("unswept stats: %v", st)
	}
	// same rows, swept: digest-neutral header rewrite, NOT unchanged
	rr, js = do(append(bev, obs(map[string]interface{}{"dropped": 0, "filled": 1, "borrowed": 0, "blank": 1, "coverage": "swept"})...))
	if rr.Code != 200 || js["unchanged"] != false || js["coverage"] != "swept" || js["coverage_changed"] != true || js["previous_coverage"] != "" {
		t.Fatalf("swept flip: %d %s", rr.Code, rr.Body.String())
	}
	if oh := neStore.get("63330").layerHeader("obs"); neObsCoverageOf(oh) != "swept" {
		t.Fatalf("stored header lost coverage: %v", oh)
	}
	// identical swept re-post → unchanged
	if rr, js := do(append(bev, obs(map[string]interface{}{"filled": 1, "blank": 1, "coverage": "swept"})...)); rr.Code != 200 || js["unchanged"] != true {
		t.Fatalf("swept re-post: %d %s", rr.Code, rr.Body.String())
	}
	neManifest.refresh(true)
	neObsCompletePass()
	r = neObsCompareKG("63330")
	if r == nil || r.BlankN != 0 || r.NodataN != 1 || r.Coverage != "swept" {
		t.Fatalf("swept row: %+v", r)
	}
	if rows := neObsMissingRows(); len(rows) != 0 {
		t.Fatalf("swept KG without drift must not be listed: %+v", rows)
	}
	st = NEObsCompleteStats()
	if st["cells_obs_blank"] != 0 || st["kgs_obs_blank"] != 0 || st["cells_obs_nodata"] != 1 || st["kgs_obs_nodata"] != 1 {
		t.Fatalf("swept stats: %v", st)
	}
	// real drift still lists a swept KG
	if !neObsRowWantsRepost(&neObsCompleteRow{Coverage: "swept", NodataN: 5, ExtraN: 1}) || !neObsRowWantsRepost(&neObsCompleteRow{Coverage: "swept", MissingN: 1}) ||
		neObsRowWantsRepost(&neObsCompleteRow{Coverage: "swept", NodataN: 5}) {
		t.Fatal("want-repost rule")
	}
	// manifest obs{}: coverage, view_rekey, blank_n, nodata_n
	neManifest.refresh(true)
	rr2 := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/k/api/v1/ne/manifest", nil)
	req.Header.Set("Authorization", "Bearer "+srtm)
	h.ServeHTTP(rr2, req)
	var man map[string]interface{}
	json.Unmarshal(rr2.Body.Bytes(), &man)
	ob := man["kgs"].(map[string]interface{})["63330"].(map[string]interface{})["obs"].(map[string]interface{})
	vr, _ := ob["view_rekey"].(map[string]interface{})
	if ob["coverage"] != "swept" || vr["blank"] != 1.0 || ob["blank_n"] != 0.0 || ob["nodata_n"] != 1.0 {
		t.Fatalf("manifest obs: %v", ob)
	}
}

// A recount (view moved, obs file unchanged) must surface in the manifest
// ?since= delta: obs.counts_at rides on the entry and bumps updated_at
// (srtm 2026-11-06: 2 289 parents stuck on a stale blank_n).
func TestNEManifestDeltaListsRecount(t *testing.T) {
	neObsComplete.mu.Lock()
	saved := neObsComplete.rows
	neObsComplete.rows = map[string]*neObsCompleteRow{"11116": {KG: "11116", BlankN: 0, computedAt: time.Date(2026, 11, 6, 12, 51, 0, 0, time.UTC)}}
	neObsComplete.mu.Unlock()
	defer func() { neObsComplete.mu.Lock(); neObsComplete.rows = saved; neObsComplete.mu.Unlock() }()
	e := &neKGEntry{Obs: &neLayerInfo{BuiltAt: "2026-11-06T09:56:00Z"}, UpdatedAt: "2026-11-06T09:56:00Z"}
	if bn, nn, at, ok := neObsCompleteCounts("11116"); ok {
		e.Obs.BlankN, e.Obs.NodataN = &bn, &nn
		e.Obs.CountsAt = at.UTC().Format(time.RFC3339)
		if e.Obs.CountsAt > e.UpdatedAt {
			e.UpdatedAt = e.Obs.CountsAt
		}
	} else {
		t.Fatal("row not found")
	}
	if e.Obs.CountsAt != "2026-11-06T12:51:00Z" || e.UpdatedAt != "2026-11-06T12:51:00Z" {
		t.Fatalf("counts_at %q updated_at %q", e.Obs.CountsAt, e.UpdatedAt)
	}
	m := &neManifestCache{entries: map[string]*neKGEntry{"11116": e}, generatedAt: "2026-11-06T13:00:00Z", kgs: []string{"11116"}}
	body, _, ok := m.delta(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	if !ok || !strings.Contains(string(body), `"11116"`) || !strings.Contains(string(body), `"counts_at":"2026-11-06T12:51:00Z"`) {
		t.Fatalf("delta ok=%v body=%s", ok, body)
	}
}
