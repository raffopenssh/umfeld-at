package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
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

// packSection mirrors ne_cells.pack.pack_section (test helper).
func packSection(magic string, hdr map[string]interface{}, raw []byte) []byte {
	sum := sha256.Sum256(raw)
	hdr["digest"] = hex.EncodeToString(sum[:])[:16]
	hdr["fmt_ver"] = 1
	hj, _ := json.Marshal(hdr)
	var zb bytes.Buffer
	zw := zlib.NewWriter(&zb)
	zw.Write(raw)
	zw.Close()
	out := []byte(magic)
	out = append(out, 1)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(hj)))
	out = append(out, hj...)
	out = binary.LittleEndian.AppendUint32(out, uint32(zb.Len()))
	return append(out, zb.Bytes()...)
}

// neSameSections — same sections (magic, header bytes, inflated record block);
// the deflate stream is not contractual (docs/ne-cells.md: compare digests).
func neSameSections(t *testing.T, a, b []byte) bool {
	t.Helper()
	sa, ea := parseNESections(a)
	sb, eb := parseNESections(b)
	if ea != nil || eb != nil || len(sa) != len(sb) {
		t.Logf("sections: %v %v %d %d", ea, eb, len(sa), len(sb))
		return false
	}
	for i := range sa {
		ra, _ := neInflate(sa[i].Body)
		rb, _ := neInflate(sb[i].Body)
		// a stored NEO1 gains declared_source/declared_digest on POST (digest-neutral)
		for _, h := range []map[string]interface{}{sa[i].Header, sb[i].Header} {
			delete(h, "declared_source")
			delete(h, "declared_digest")
		}
		ha, _ := json.Marshal(sa[i].Header)
		hb, _ := json.Marshal(sb[i].Header)
		if sa[i].Magic != sb[i].Magic || !bytes.Equal(ra, rb) || !bytes.Equal(ha, hb) {
			t.Logf("section %d differs", i)
			return false
		}
	}
	return true
}

func TestNEContainerAndObsRoundtrip(t *testing.T) {
	dir := t.TempDir()
	old := neDirOverride(dir)
	defer neDirOverride(old)
	savedStore := neStore
	neResetStoreForTest()
	neStore.load()
	defer func() { neStore = savedStore }()

	// a tiny NEC1 with two LU records and one K record
	raw := make([]byte, 0, 89)
	rec := func(cid uint64) []byte {
		b := make([]byte, 30)
		binary.LittleEndian.PutUint64(b, cid)
		b[8], b[9], b[10] = 255, 255, 250
		b[12+4] = 200 // wald
		b[28], b[29] = 0xFF, 0xFF
		return b
	}
	raw = append(raw, rec(0x8c1e108cc0539ff)...)
	raw = append(raw, rec(0x8c1e108cc0515ff)...)
	k := make([]byte, 29)
	binary.LittleEndian.PutUint64(k, 0x8a1e108cc057fff)
	binary.LittleEndian.PutUint16(k[8:], 7)
	k[24] = 0xFF
	k[27], k[28] = 0xFF, 0xFF
	raw = append(raw, k...)
	nec := packSection("NEC1", map[string]interface{}{"kg": "63330", "algo": "ne-cells-1", "epoch": "2026-03", "source": "index-2026-03", "cells_n": 2, "kcells_n": 1}, raw)
	os.WriteFile(filepath.Join(dir, "63330.nec"), nec, 0o644)

	const srtm, ops = "0123456789abcdef0123", "fedcba9876543210fedc"
	setTierConfigForTest(true, srtm, ops)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/ne/", handleNE)
	h := tierMiddleware(mux)
	do := func(method, path, tok string, body []byte) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		h.ServeHTTP(rr, req)
		return rr
	}
	// reads are open to every tier since the public NE API; writes stay first-token only
	if rr := do("GET", "/k/api/v1/ne/63330", ops, nil); rr.Code != 200 {
		t.Fatalf("second token read: got %d", rr.Code)
	}
	if rr := do("POST", "/k/api/v1/ne/63330/obs", ops, []byte("x")); rr.Code != 404 {
		t.Fatalf("second token write must get 404, got %d", rr.Code)
	}
	if rr := do("POST", "/api/v1/ne/63330/obs", "", []byte("x")); rr.Code != 404 {
		t.Fatalf("public write must get 404, got %d", rr.Code)
	}
	rr := do("GET", "/k/api/v1/ne/63330", srtm, nil)
	if rr.Code != 200 || !neSameSections(t, rr.Body.Bytes(), nec) || rr.Header().Get("ETag") == "" {
		t.Fatalf("binary GET: code %d len %d etag %q", rr.Code, rr.Body.Len(), rr.Header().Get("ETag"))
	}
	if rr.Header().Get("X-NE-Layers-Missing") != "obs" {
		t.Fatalf("obs should be reported missing, got %q", rr.Header().Get("X-NE-Layers-Missing"))
	}
	rr = do("GET", "/k/api/v1/ne/63330?format=json&layers=lu", srtm, nil)
	var js map[string]interface{}
	json.Unmarshal(rr.Body.Bytes(), &js)
	secs := js["sections"].([]interface{})
	cells := secs[0].(map[string]interface{})["cells"].(map[string]interface{})
	c := cells["8c1e108cc0539ff"].(map[string]interface{})
	if c["lu"].([]interface{})[4].(float64) != 200 || c["orient_coh"] != nil {
		t.Fatalf("json decode wrong: %v", c)
	}
	// obs: wrong group order rejected, right one stored and served after lu
	bad := packSection("NEO1", map[string]interface{}{"kg": "63330", "algo": "obs-cells-0", "epoch": "2025-07", "source": "als", "h3_res": 12, "cells_n": 1,
		"groups": []string{"bau", "geb", "acker", "gruen", "wald", "wasser", "verkehr", "alpen", "sonst"}}, []byte("x"))
	if rr := do("POST", "/k/api/v1/ne/63330/obs", srtm, bad); rr.Code != 400 {
		t.Fatalf("bad groups: want 400, got %d %s", rr.Code, rr.Body.String())
	}
	groups := []string{"geb", "bau", "acker", "gruen", "wald", "wasser", "verkehr", "alpen", "sonst"}
	obsRaw := func(ids ...uint64) []byte {
		var out []byte
		for _, id := range ids {
			b := make([]byte, 29) // NEO1 obs-cells-1 row: cell_id + 21 B
			binary.LittleEndian.PutUint64(b, id)
			b[8] = 7
			out = append(out, b...)
		}
		return out
	}
	// cell set ≠ ours → 409
	wrong := packSection("NEO1", map[string]interface{}{"kg": "63330", "algo": "obs-cells-1", "epoch": "2026-10", "source": "obs-peer@2.4", "h3_res": 12, "cells_n": 2,
		"groups": groups}, obsRaw(0x8c1e108cc0539ff, 0x8c1e108cc0517ff))
	if rr := do("POST", "/k/api/v1/ne/63330/obs", srtm, wrong); rr.Code != 409 {
		t.Fatalf("wrong cell set: want 409, got %d %s", rr.Code, rr.Body.String())
	}
	good := packSection("NEO1", map[string]interface{}{"kg": "63330", "algo": "obs-cells-1", "epoch": "2026-10", "source": "obs-peer@2.4", "h3_res": 12, "cells_n": 2,
		"groups": groups}, obsRaw(0x8c1e108cc0515ff, 0x8c1e108cc0539ff))
	if rr := do("POST", "/k/api/v1/ne/63330/obs", srtm, good); rr.Code != 200 {
		t.Fatalf("good obs: %d %s", rr.Code, rr.Body.String())
	}
	rr = do("POST", "/k/api/v1/ne/63330/obs", srtm, good)
	json.Unmarshal(rr.Body.Bytes(), &js)
	if rr.Code != 200 || js["unchanged"] != true {
		t.Fatalf("same digest re-post: %d %s", rr.Code, rr.Body.String())
	}
	raw2 := obsRaw(0x8c1e108cc0515ff, 0x8c1e108cc0539ff)
	raw2[9] = 99
	good2 := packSection("NEO1", map[string]interface{}{"kg": "63330", "algo": "obs-cells-1", "epoch": "2026-11", "source": "obs-peer@2.4", "h3_res": 12, "cells_n": 2,
		"groups": groups}, raw2)
	rr = do("POST", "/k/api/v1/ne/63330/obs", srtm, good2)
	json.Unmarshal(rr.Body.Bytes(), &js)
	if rr.Code != 200 || js["replaced"] != true || js["unchanged"] != false {
		t.Fatalf("different digest re-post: %d %s", rr.Code, rr.Body.String())
	}
	// srtm v2.4: 41 B row declared via record_bytes + fields (never hard-coded server-side)
	row41 := func(ids ...uint64) []byte {
		var out []byte
		for _, id := range ids {
			b := make([]byte, 41)
			binary.LittleEndian.PutUint64(b, id)
			out = append(out, b...)
		}
		return out
	}
	hdr41 := map[string]interface{}{"kg": "63330", "algo": "obs-cells-2", "epoch": "2026-12", "source": "obs-peer@2.4", "h3_res": 12, "cells_n": 2,
		"groups": groups, "record_bytes": 41, "fields": []string{"cell_id:u64", "obs_lu:u8[9]", "pad:u8[24]"}}
	good3 := packSection("NEO1", hdr41, row41(0x8c1e108cc0515ff, 0x8c1e108cc0539ff))
	rr = do("POST", "/k/api/v1/ne/63330/obs", srtm, good3)
	json.Unmarshal(rr.Body.Bytes(), &js)
	if rr.Code != 200 || js["row_bytes"].(float64) != 41 || js["record_bytes_declared"] != true || len(js["fields"].([]interface{})) != 3 {
		t.Fatalf("41 B declared row: %d %s", rr.Code, rr.Body.String())
	}
	// declared record_bytes that disagrees with the body → 400 (not silently re-derived)
	hdr41["record_bytes"] = 40
	if rr := do("POST", "/k/api/v1/ne/63330/obs", srtm, packSection("NEO1", hdr41, row41(0x8c1e108cc0515ff, 0x8c1e108cc0539ff))); rr.Code != 400 {
		t.Fatalf("record_bytes mismatch: want 400, got %d %s", rr.Code, rr.Body.String())
	}
	good = good3
	// manifest: lu + obs listed, queue status for unbuilt
	os.WriteFile(filepath.Join(dir, "queue.json"), []byte(`{"updated_at":"`+time.Now().UTC().Format(time.RFC3339)+`","total":3,"next_index":1,"avg_seconds":40,"failed":["63332"]}`), 0o644)
	// the manifest's KG universe is the peer contract list (kg_universe.go), not a dir scan
	kgUniverseMu.RLock()
	oldU := kgUniverseCur
	kgUniverseMu.RUnlock()
	tu := &kgUniverse{Schema: "test"}
	for _, kg := range []string{"63330", "63331", "63332", "63333"} {
		tu.KGs = append(tu.KGs, kgUniverseRow{KGCode: kg})
	}
	tu.finish()
	setKGUniverse(tu, nil)
	defer func() {
		if oldU != nil {
			setKGUniverse(oldU, nil)
		}
	}()
	neManifest.refresh(true)
	rr = do("GET", "/k/api/v1/ne/manifest", srtm, nil)
	json.Unmarshal(rr.Body.Bytes(), &js)
	kgsM := js["kgs"].(map[string]interface{})
	e := kgsM["63330"].(map[string]interface{})
	if rr.Code != 200 || rr.Header().Get("ETag") == "" || e["status"] != "built" || e["lu"].(map[string]interface{})["digest"] == "" ||
		e["obs"].(map[string]interface{})["epoch"] != "2026-12" || e["obs"].(map[string]interface{})["record_bytes"].(float64) != 41 || e["bev"] != nil {
		t.Fatalf("manifest: %d %s", rr.Code, rr.Body.String()[:400])
	}
	if q := kgsM["63333"].(map[string]interface{}); q["status"] != "queued" || q["queue_position"].(float64) != 2 || q["retry_after_s"].(float64) != 120 {
		t.Fatalf("queued entry: %v", q)
	}
	if q := kgsM["63332"].(map[string]interface{}); q["status"] != "failed" {
		t.Fatalf("failed entry: %v", q)
	}
	if rr2 := do("GET", "/k/api/v1/ne/manifest", srtm, nil); rr2.Code != 200 {
		t.Fatalf("manifest 2nd: %d", rr2.Code)
	}
	req := httptest.NewRequest("GET", "/k/api/v1/ne/manifest", nil)
	req.Header.Set("Authorization", "Bearer "+srtm)
	req.Header.Set("If-None-Match", rr.Header().Get("ETag"))
	rr3 := httptest.NewRecorder()
	h.ServeHTTP(rr3, req)
	if rr3.Code != 304 {
		t.Fatalf("manifest If-None-Match: %d", rr3.Code)
	}
	// delta mode: ?since=<generated_at> → only kgs with updated_at ≥ since (+ counters), delta:true
	rr = do("GET", "/k/api/v1/ne/manifest?since="+time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), srtm, nil)
	js = map[string]interface{}{}
	json.Unmarshal(rr.Body.Bytes(), &js)
	dk, _ := js["kgs"].(map[string]interface{})
	if rr.Code != 200 || js["delta"] != true || js["kgs_total"].(float64) != 4 || js["built"].(float64) < 1 || js["generated_at"] == nil ||
		js["obs_posted"] == nil || dk["63330"] == nil || dk["63333"] != nil || dk["63330"].(map[string]interface{})["status"] != "built" {
		t.Fatalf("manifest delta: %d %s", rr.Code, rr.Body.String())
	}
	if et := rr.Header().Get("ETag"); et == "" || et == rr3.Header().Get("ETag") {
		t.Fatalf("delta ETag: %q", et)
	} else {
		req := httptest.NewRequest("GET", "/k/api/v1/ne/manifest?since="+time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), nil)
		req.Header.Set("Authorization", "Bearer "+srtm)
		req.Header.Set("If-None-Match", et)
		rr4 := httptest.NewRecorder()
		h.ServeHTTP(rr4, req)
		if rr4.Code != 304 {
			t.Fatalf("delta If-None-Match: %d", rr4.Code)
		}
	}
	// cursor = our own generated_at (the real client loop) → still a delta (inclusive boundary)
	rr = do("GET", "/k/api/v1/ne/manifest?since="+js["generated_at"].(string), srtm, nil)
	js = map[string]interface{}{}
	json.Unmarshal(rr.Body.Bytes(), &js)
	if rr.Code != 200 || js["delta"] != true {
		t.Fatalf("delta from generated_at: %d %s", rr.Code, rr.Body.String()[:200])
	}
	// older than retention or later than generated_at → full manifest, delta:false
	for _, since := range []string{time.Now().Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339), time.Now().Add(time.Hour).UTC().Format(time.RFC3339)} {
		rr = do("GET", "/k/api/v1/ne/manifest?since="+since, srtm, nil)
		js = map[string]interface{}{}
		json.Unmarshal(rr.Body.Bytes(), &js)
		if rr.Code != 200 || js["delta"] != false || js["kgs"].(map[string]interface{})["63333"] == nil || js["queue"] == nil {
			t.Fatalf("delta fallback since=%s: %d %s", since, rr.Code, rr.Body.String()[:200])
		}
	}
	if rr := do("GET", "/k/api/v1/ne/manifest?since=yesterday", srtm, nil); rr.Code != 400 {
		t.Fatalf("delta bad since: %d", rr.Code)
	}
	// unbuilt KG → 404 + Retry-After, never partial
	rr = do("GET", "/k/api/v1/ne/63333?layers=lu", srtm, nil)
	if rr.Code != 404 || rr.Header().Get("Retry-After") != "120" {
		t.Fatalf("unbuilt: %d Retry-After=%q %s", rr.Code, rr.Header().Get("Retry-After"), rr.Body.String())
	}
	if rr := do("GET", "/k/api/v1/ne/63332?layers=lu", srtm, nil); rr.Code != 404 || rr.Header().Get("Retry-After") != "" {
		t.Fatalf("failed kg must have no Retry-After: %d %q", rr.Code, rr.Header().Get("Retry-After"))
	}
	rr = do("GET", "/k/api/v1/ne/63330?layers=lu,obs", srtm, nil)
	if !neSameSections(t, rr.Body.Bytes(), append(append([]byte{}, nec...), good...)) {
		t.Fatalf("lu+obs concatenation wrong (%d bytes)", rr.Body.Len())
	}
	// epoch report: one chunk identical, one changed
	ours, _, _ := neChunkDigests("63330", "nec")
	if len(ours) != 1 {
		t.Fatalf("expected 1 res-10 chunk, got %v", ours)
	}
	var chunk string
	for k := range ours {
		chunk = k
	}
	if chunk != "8a1e108cc057fff" {
		t.Fatalf("parent wrong: %s", chunk)
	}
	rep, _ := json.Marshal(map[string]interface{}{"kg": "63330", "algo": "ne-cells-1", "epoch": "2026-10", "source": "bevdirect", "observer": "t",
		"h3_res": 12, "chunk_res": 10, "cells_n": 2, "digest": "deadbeefdeadbeef", "chunks": map[string]string{chunk: ours[chunk], "8a1e108cc05ffff": "0000000000000000"}})
	rr = do("POST", "/k/api/v1/ne/63330/report", srtm, rep)
	json.Unmarshal(rr.Body.Bytes(), &js)
	// source "bevdirect" ≠ our stored index source → not comparable (FIX 4), report kept
	if rr.Code != 200 || js["compared"] != false || js["identical_source"] != false {
		t.Fatalf("report cross-source: %d %s", rr.Code, rr.Body.String())
	}
	rep, _ = json.Marshal(map[string]interface{}{"kg": "63330", "algo": "ne-cells-1", "epoch": "2026-10", "source": "index-2026-03", "observer": "t",
		"h3_res": 12, "chunk_res": 10, "cells_n": 2, "digest": "deadbeefdeadbeef", "chunks": map[string]string{chunk: ours[chunk], "8a1e108cc05ffff": "0000000000000000"}})
	rr = do("POST", "/k/api/v1/ne/63330/report", srtm, rep)
	json.Unmarshal(rr.Body.Bytes(), &js)
	if rr.Code != 200 || js["compared"] != true || js["chunks_same"].(float64) != 1 || len(js["chunks_unknown_to_us"].([]interface{})) != 1 {
		t.Fatalf("report: %d %s", rr.Code, rr.Body.String())
	}
	rr = do("GET", "/k/api/v1/ne/63330/report", srtm, nil)
	json.Unmarshal(rr.Body.Bytes(), &js)
	if js["count"].(float64) != 2 {
		t.Fatalf("report list: %s", rr.Body.String())
	}
}

// Observer reports (api_ne_reports.go): any private token may report; the
// first report of a source becomes the baseline; identical re-posts are
// stored as stubs; the overview is private-only; obs stays first-token.
func TestNEObserverReports(t *testing.T) {
	dir := t.TempDir()
	defer neDirOverride(neDirOverride(dir))
	const srtm, ops, mapTok = "0123456789abcdef0123", "fedcba9876543210fedc", "mappeer0000000000000000"
	setTierConfigForTest(true, srtm, ops)
	setContributorConfigForTest([]string{mapTok}, []string{"map-peer"})
	defer setContributorConfigForTest(nil, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/ne/", handleNE)
	h := tierMiddleware(mux)
	do := func(method, path, tok string, body interface{}) (*httptest.ResponseRecorder, map[string]interface{}) {
		var b []byte
		if body != nil {
			b, _ = json.Marshal(body)
		}
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, bytes.NewReader(b))
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		h.ServeHTTP(rr, req)
		var js map[string]interface{}
		json.Unmarshal(rr.Body.Bytes(), &js)
		return rr, js
	}
	rep := func(observer, digest string, chunks map[string]string) map[string]interface{} {
		return map[string]interface{}{"kg": "63331", "algo": "ne-cells-2", "epoch": "2026-10", "source": "bevdirect@v0.2.1", "observer": observer,
			"h3_res": 12, "chunk_res": 10, "cells_n": 2, "digest": digest, "chunks": chunks}
	}
	c1, c2 := "8a1e108cc057fff", "8a1e108cc05ffff"
	// public / no token → 404; third token → allowed
	if rr, _ := do("POST", "/api/v1/ne/63331/report", "", rep("x", "aaaaaaaaaaaaaaaa", map[string]string{c1: "1111111111111111"})); rr.Code != 404 {
		t.Fatalf("public report: %d", rr.Code)
	}
	rr, js := do("POST", "/k/api/v1/ne/63331/report", mapTok, rep("map-peer-vm-1", "aaaaaaaaaaaaaaaa", map[string]string{c1: "1111111111111111", c2: "2222222222222222"}))
	if rr.Code != 200 || js["compared"] != false || js["baseline"] != "this_report" || js["token"] != "map-peer" {
		t.Fatalf("first report: %d %s", rr.Code, rr.Body.String())
	}
	// same digest again → unchanged stub
	rr, js = do("POST", "/k/api/v1/ne/63331/report", mapTok, rep("map-peer-vm-1", "aaaaaaaaaaaaaaaa", map[string]string{c1: "1111111111111111", c2: "2222222222222222"}))
	if rr.Code != 200 || js["unchanged"] != true {
		t.Fatalf("unchanged: %d %s", rr.Code, rr.Body.String())
	}
	// another observer (srtm token, equivalent tag) → compared against map peer's baseline
	r2 := rep("srtm-peer-3", "bbbbbbbbbbbbbbbb", map[string]string{c1: "1111111111111111", c2: "3333333333333333"})
	r2["source"] = "bevdirect@v0.2.0"
	rr, js = do("POST", "/k/api/v1/ne/63331/report", srtm, r2)
	if rr.Code != 200 || js["compared"] != true || js["baseline"] != "report" || js["baseline_observer"] != "map-peer-vm-1" ||
		js["chunks_same"].(float64) != 1 || len(js["chunks_changed"].([]interface{})) != 1 {
		t.Fatalf("second observer: %d %s", rr.Code, rr.Body.String())
	}
	// since_last: c2 was 2222 at baseline, 3333 in the latest report
	sl, _ := js["since_last"].(map[string]interface{})
	if sl == nil || sl["compared"].(float64) != 2 || len(sl["changed"].([]interface{})) != 1 {
		t.Fatalf("since_last of second observer: %s", rr.Body.String())
	}
	// a disjoint viewport in the same KG (chunk c3 only) → no overlap, becomes
	// part of the per-chunk baseline instead of being "unknown" forever
	c3 := "8a1e108cc067fff"
	rr, js = do("POST", "/k/api/v1/ne/63331/report", mapTok, rep("map-peer-vm-2", "dddddddddddddddd", map[string]string{c3: "4444444444444444"}))
	if rr.Code != 200 || js["compared"] != false || js["baseline"] != "this_report" || js["chunks_baselined_now"].(float64) != 1 || js["baseline_reports"].(float64) != 2 {
		t.Fatalf("disjoint viewport: %d %s", rr.Code, rr.Body.String())
	}
	// a viewport spanning c2+c3: c2 → compared vs epoch 0 (2222 → 3333 = changed) but
	// NOT changed since the previous observation (3333); c3 compared vs its own first digest
	rr, js = do("POST", "/k/api/v1/ne/63331/report", srtm, rep("srtm-peer-4", "eeeeeeeeeeeeeeee", map[string]string{c2: "3333333333333333", c3: "5555555555555555"}))
	chg, _ := js["chunks_changed"].([]interface{})
	sl, _ = js["since_last"].(map[string]interface{})
	if rr.Code != 200 || js["compared"] != true || js["baseline"] != "report" || len(chg) != 2 || js["chunks_same"].(float64) != 0 ||
		sl == nil || sl["compared"].(float64) != 2 || sl["same"].(float64) != 1 || len(sl["changed"].([]interface{})) != 1 || sl["changed"].([]interface{})[0] != c3 {
		t.Fatalf("spanning viewport: %d %s", rr.Code, rr.Body.String())
	}
	// bad shape → 400
	if rr, _ := do("POST", "/k/api/v1/ne/63331/report", mapTok, rep("v", "xyz", map[string]string{c1: "1111111111111111"})); rr.Code != 400 {
		t.Fatalf("bad digest: %d", rr.Code)
	}
	// stored lines: 5 (one stub without chunks)
	st := neReadReports("63331")
	if len(st) != 5 || !st[1].Unchanged || len(st[1].Report.Chunks) != 0 || st[0].Token != "map-peer" {
		t.Fatalf("stored: %+v", st)
	}
	// overview: private only, both observers listed
	if rr, _ := do("GET", "/api/v1/ne/reports", "", nil); rr.Code != 404 {
		t.Fatalf("public overview: %d", rr.Code)
	}
	rr, js = do("GET", "/k/api/v1/ne/reports", ops, nil)
	if rr.Code != 200 || len(js["live"].([]interface{})) < 2 || js["on_disk"].(map[string]interface{})["files"].(float64) != 1 {
		t.Fatalf("overview: %d %s", rr.Code, rr.Body.String())
	}
	// contributor token: obs write and the cadastre surface are 404; manifest is open
	if rr, _ := do("POST", "/k/api/v1/ne/63331/obs", mapTok, "x"); rr.Code != 404 {
		t.Fatalf("map peer obs: %d", rr.Code)
	}
	for _, p := range []string{"/k/api/v1/spatial/parcels?west=1&south=1&east=2&north=2", "/k/api/v1/ne/draft/", "/k/llm/kg/63330", "/k/api/v1/export/geojson?kg=63330"} {
		if rr, _ := do("GET", p, mapTok, nil); rr.Code != 404 {
			t.Fatalf("contributor must not reach %s: %d", p, rr.Code)
		}
	}
	if !contributorPathAllowed("/api/v1/ne/manifest") || !contributorPathAllowed("/api/v1/ne/63331/obs") /* path allowed, neWriteGate decides by name */ || contributorPathAllowed("/api/v1/spatial/parcels") || contributorPathAllowed("/api/v1/ne/../x/report") {
		t.Fatalf("contributorPathAllowed wrong")
	}
	// rate limit per token
	neReportBurst, neReportPerSec = 2, 0.001
	defer func() { neReportBurst, neReportPerSec = 30, 2 }()
	delete(neReportBuckets, 1000)
	codes := []int{}
	for i := 0; i < 3; i++ {
		rr, _ := do("POST", "/k/api/v1/ne/63331/report", mapTok, rep("map-peer-vm-1", "cccccccccccccccc", map[string]string{c1: "1111111111111111"}))
		codes = append(codes, rr.Code)
	}
	if codes[2] != 429 || codes[0] != 200 {
		t.Fatalf("rate: %v", codes)
	}
}

// Every tag published as cell-equivalent maps onto the frozen bev baseline, and
// the manifest advertises exactly the same set (docs/ne-cells.md §FREEZE).
func TestNEBevEquivalentTagsFrozen(t *testing.T) {
	for _, tag := range []string{"bevdirect@v0.2.1", "bevdirect@v0.3.0", "bevdirect@v0.3.1", "bevdirect@v0.3.2", "bevdirect@v0.3.3", "bevdirect@v0.3.4", "bevdirect@v0.3.5", "bevdirect@v0.3.6"} {
		if got := neBevEquivalent(tag); got != "bevdirect@v0.2.0" {
			t.Fatalf("%s → %s, want bevdirect@v0.2.0", tag, got)
		}
	}
	if neBevEquivalent("bevdirect@v0.1.0") != "bevdirect@v0.1.0" {
		t.Fatal("v0.1.0 differs by ~25 % of cells and must not be equivalent")
	}
	adv, _ := neFrozen["bev_equivalent_tags"].([]string)
	if len(adv) != len(neBevEquivalentTags) {
		t.Fatalf("manifest advertises %v, map has %v", adv, neBevEquivalentTags)
	}
	for _, tag := range adv {
		if _, ok := neBevEquivalentTags[tag]; !ok {
			t.Fatalf("manifest advertises %s but neBevEquivalentTags lacks it", tag)
		}
	}
}

// TestNEObsOnBevDeclared — POST /obs with NEC1+NEO1: the NEO1 is validated against
// the posted NEC1 (bevdirect declared build), which lands as the `bev` section; lu
// stays frozen and the answer carries a border-cell diff instead of a 409.
func TestNEObsOnBevDeclared(t *testing.T) {
	dir := t.TempDir()
	old := neDirOverride(dir)
	defer neDirOverride(old)
	savedStore := neStore
	neResetStoreForTest()
	neStore.load()
	defer func() { neStore = savedStore }()

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
	obs := func(epoch string, ids ...uint64) []byte {
		var raw []byte
		for _, id := range ids {
			row := make([]byte, 41)
			binary.LittleEndian.PutUint64(row, id)
			row[9] = byte(len(epoch)) // make the record block depend on the epoch string length
			raw = append(raw, row...)
		}
		return packSection("NEO1", map[string]interface{}{"kg": "63330", "algo": "obs-cells-2", "epoch": epoch, "source": "obs-peer@2.5", "h3_res": 12,
			"cells_n": len(ids), "groups": groups, "record_bytes": 41}, raw)
	}
	// lu = {a,b}; the peer's bevdirect build = {a,b,c} (one border cell more)
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
	// bare NEO1 on the bevdirect set → still 409 against lu (unchanged behaviour)
	if rr, _ := do(obs("2026-10", a, b, c)); rr.Code != 409 {
		t.Fatalf("bare NEO1 ≠ lu: want 409, got %d %s", rr.Code, rr.Body.String())
	}
	bev := nec("ne-cells-2", "bevdirect@v0.3.1", a, b, c)
	// NEC1 with a non-pinned source / wrong algo → 400
	if rr, _ := do(append(nec("ne-cells-2", "bevdirect@v0.1.0", a, b, c), obs("2026-10", a, b, c)...)); rr.Code != 400 {
		t.Fatalf("unpinned source: want 400, got %d %s", rr.Code, rr.Body.String())
	}
	if rr, _ := do(append(nec("ne-cells-1", "bevdirect@v0.3.1", a, b, c), obs("2026-10", a, b, c)...)); rr.Code != 400 {
		t.Fatalf("wrong algo: want 400, got %d %s", rr.Code, rr.Body.String())
	}
	// NEO1 ≠ posted NEC1 → 409 naming the bev layer
	if rr, js := do(append(bev, obs("2026-10", a, b)...)); rr.Code != 409 || js["declared_layer"] != "bev" {
		t.Fatalf("NEO1 ≠ NEC1: want 409/bev, got %d %s", rr.Code, rr.Body.String())
	}
	// the good one: stored, bev section created, lu diff reported, no 409
	rr, js := do(append(bev, obs("2026-10", a, b, c)...))
	bevDigest := js["declared_digest"]
	if rr.Code != 200 || js["declared_layer"] != "bev" || js["declared_source"] != "bevdirect@v0.3.1" || js["unchanged"] != false {
		t.Fatalf("NEC1+NEO1: %d %s", rr.Code, rr.Body.String())
	}
	diff, _ := js["lu_diff"].(map[string]interface{})
	if diff["lu_cells_n"].(float64) != 2 || diff["bev_cells_n"].(float64) != 3 || diff["missing_n"].(float64) != 0 || diff["extra_n"].(float64) != 1 {
		t.Fatalf("lu_diff: %v", diff)
	}
	if _, err := os.Stat(filepath.Join(dir, "63330.bev.nec")); !os.IsNotExist(err) {
		t.Fatalf("input .bev.nec should be folded into .nex and deleted")
	}
	kg := neStore.get("63330")
	if kg == nil || kg.Hdr.Layers["bev"] == nil || kg.Hdr.Layers["obs"] == nil || kg.Hdr.Layers["lu"] == nil || kg.Hdr.Layers["bev"].Rows != 3 {
		t.Fatalf("store layers: %+v", kg)
	}
	oh := kg.layerHeader("obs")
	if oh["declared_source"] != "bevdirect@v0.3.1" || oh["declared_digest"] != bevDigest || oh["digest"] == nil {
		t.Fatalf("stored obs header: %v", oh)
	}
	// same again (NEO1 and NEC1 both unchanged) → unchanged
	if rr, js := do(append(bev, obs("2026-10", a, b, c)...)); rr.Code != 200 || js["unchanged"] != true {
		t.Fatalf("re-post: %d %s", rr.Code, rr.Body.String())
	}
	// also accepted in NEO1,NEC1 order; a new NEO1 epoch replaces obs only (bev unchanged)
	if rr, js := do(append(obs("2026-11-x", a, b, c), bev...)); rr.Code != 200 || js["replaced"] != true || js["bev_replaced"] != nil {
		t.Fatalf("reverse order: %d %s", rr.Code, rr.Body.String())
	}
	// manifest: obs.declared_*, bev.{digest,cells_n,source}, kgs_obs_on_bev
	neManifest.refresh(true)
	rr2 := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/k/api/v1/ne/manifest", nil)
	req.Header.Set("Authorization", "Bearer "+srtm)
	h.ServeHTTP(rr2, req)
	var man map[string]interface{}
	json.Unmarshal(rr2.Body.Bytes(), &man)
	e := man["kgs"].(map[string]interface{})["63330"].(map[string]interface{})
	ob := e["obs"].(map[string]interface{})
	bv := e["bev"].(map[string]interface{})
	if man["kgs_obs_on_bev"].(float64) != 1 || ob["declared_source"] != "bevdirect@v0.3.1" || ob["declared_digest"] != bv["digest"] ||
		bv["cells_n"].(float64) != 3 || bv["source"] != "bevdirect@v0.3.1" || ob["epoch"] != "2026-11-x" {
		t.Fatalf("manifest: %s", rr2.Body.String())
	}
	// /ne/cell stays lu-keyed (the extra border cell is 404 there), obs joins on shared cells
	rr3 := httptest.NewRecorder()
	req = httptest.NewRequest("GET", fmt.Sprintf("/k/api/v1/ne/cell/%x", a), nil)
	req.Header.Set("Authorization", "Bearer "+srtm)
	h.ServeHTTP(rr3, req)
	if rr3.Code != 200 || !strings.Contains(rr3.Body.String(), "obs") {
		t.Fatalf("cell join: %d %s", rr3.Code, rr3.Body.String())
	}
}

// Coverage rule (docs/ne-cells.md §coverage): for one bbox the build with the
// most cells is the complete one; a lossy build (unaligned/multi-cell
// viewports, 75110/73008 on 2026-10-07/08) neither forms a baseline nor a
// "last" digest, and its own diffs are change_suspect. The bevdirect tag is
// NOT the classifier — a v0.3.1 aligned build equals a v0.3.2 one.
func TestNEReportCoverageRule(t *testing.T) {
	bbox := []float64{12.7055, 46.6302, 12.8638, 46.7607}
	mk := func(src, obs string, cells int, chunks map[string]string) neStoredReport {
		return neStoredReport{ReceivedAt: "2026-10-07T06:00:00Z", Token: "srtm", Report: neReport{
			KG: "75110", Algo: "ne-cells-2", Epoch: "2026-10", Source: src, Observer: obs, ChunkRes: 10, CellsN: cells, BBox: bbox, Chunks: chunks},
			Result: map[string]interface{}{}}
	}
	lossy := mk("bevdirect@v0.3.1", "srtm-primary", 164258, map[string]string{"a": "1", "b": "1"})
	good := mk("bevdirect@v0.3.1", "srtm-peer-primary", 168243, map[string]string{"a": "2", "b": "1", "c": "1"})
	// a good report arriving after a lossy one: the lossy one is skipped → no baseline, no false change
	h := neReportHistory([]neStoredReport{lossy}, &good.Report)
	if h.Reports != 0 || h.Lossy != 1 || len(h.First) != 0 {
		t.Fatalf("lossy report must not form a baseline: %+v", h)
	}
	if cov := neCoverageOf([]neStoredReport{lossy}, &good.Report); cov["status"] != "best" {
		t.Fatalf("good report status %v", cov)
	}
	// a lossy report arriving after a good one: compared (diffs visible) but flagged
	h = neReportHistory([]neStoredReport{good}, &lossy.Report)
	if h.Reports != 1 || h.Lossy != 0 {
		t.Fatalf("good history: %+v", h)
	}
	cov := neCoverageOf([]neStoredReport{good}, &lossy.Report)
	if cov["status"] != "lossy" || cov["best_cells_n"] != 168243 {
		t.Fatalf("lossy status %v", cov)
	}
	// within tolerance (BEV edits jitter a few cells): not lossy
	near := mk("bevdirect@v0.3.2", "x", 168200, map[string]string{"a": "2"})
	if cov := neCoverageOf([]neStoredReport{good}, &near.Report); cov["status"] != "within_tolerance" {
		t.Fatalf("near status %v", cov)
	}
	// narrower bbox (viewport inside the block): only itself as reference → never lossy
	vp := mk("bevdirect@v0.3.2", "map-peer", 900, map[string]string{"a": "2"})
	vp.Report.BBox = []float64{12.71, 46.64, 12.73, 46.66}
	if cov := neCoverageOf([]neStoredReport{good}, &vp.Report); cov["status"] != "best" {
		t.Fatalf("viewport status %v", cov)
	}
	// Siedler scenario (2026-10-24, bbox-independent reference): a viewport
	// WIDER than srtm's cell block but with fewer cells = an incomplete fetch
	// (missing tile / partial cell set) → lossy against the contained block,
	// no baseline, no "last" digest. Equal or more cells under the wider box
	// (the normal case: map peer's cells_n ≥ srtm's in 1 534 KGs, never below) → best.
	wideBad := mk("bevdirect@v0.3.5", "map-peer", 166000, map[string]string{"a": "9", "b": "1", "c": "1"})
	wideBad.Report.BBox = []float64{12.70, 46.62, 12.88, 46.78}
	if cov := neCoverageOf([]neStoredReport{good}, &wideBad.Report); cov["status"] != "lossy" || cov["best_cells_n"] != 168243 {
		t.Fatalf("truncated viewport must be lossy against the contained cell block: %v", cov)
	}
	if h := neReportHistory([]neStoredReport{good, wideBad}, &near.Report); h.Lossy != 1 || h.First["a"].Digest != "2" || h.Last["a"].Digest != "2" {
		t.Fatalf("truncated viewport must be neither baseline nor last: %+v", h)
	}
	wideOK := mk("bevdirect@v0.3.5", "map-peer", 170100, map[string]string{"a": "9", "b": "1", "c": "1", "d": "1"})
	wideOK.Report.BBox = wideBad.Report.BBox
	if cov := neCoverageOf([]neStoredReport{good}, &wideOK.Report); cov["status"] != "best" {
		t.Fatalf("wider viewport with more cells: %v", cov)
	}
	// …and the block is not lossy against the wider viewport (its bbox is not contained)
	if cov := neCoverageOf([]neStoredReport{wideOK}, &good.Report); cov["status"] != "best" {
		t.Fatalf("cell block vs wider viewport: %v", cov)
	}
	if !neBBoxContains(wideOK.Report.BBox, bbox) || neBBoxContains(bbox, wideOK.Report.BBox) || !neBBoxContains(bbox, bbox) {
		t.Fatal("neBBoxContains")
	}
	// retro on read: the stored truncated viewport line gets the verdict too
	wideBad.Result["since_last"] = map[string]interface{}{"changed": []interface{}{"a"}}
	if rows := neMarkLossy([]neStoredReport{good, wideBad}); rows[1].Result["change_suspect"] != "coverage_lossy" || neStoredSuspect(&rows[0]) {
		t.Fatalf("retro map peer: %v / %v", rows[0].Result, rows[1].Result)
	}
	wideBad.Result = map[string]interface{}{}
	// retroactive marking for stored lines + daily aggregate excludes suspect diffs
	lossy.Result["since_last"] = map[string]interface{}{"changed": []interface{}{"a"}}
	good.Result["since_last"] = map[string]interface{}{"changed": []interface{}{"b"}}
	// stored order lossy→good (the real 75110/73008 case): the good report's
	// since_last was computed against the lossy one → previous_lossy
	if rows := neMarkLossy([]neStoredReport{lossy, good}); rows[0].Result["change_suspect"] != "coverage_lossy" || rows[1].Result["change_suspect"] != "previous_lossy" {
		t.Fatalf("lossy→good: %v / %v", rows[0].Result, rows[1].Result)
	}
	lossy.Result, good.Result = map[string]interface{}{"since_last": lossy.Result["since_last"]}, map[string]interface{}{"since_last": good.Result["since_last"]}
	rows := neMarkLossy([]neStoredReport{good, lossy})
	if !neStoredSuspect(&rows[1]) || neStoredSuspect(&rows[0]) {
		t.Fatalf("neMarkLossy: %v / %v", rows[0].Result, rows[1].Result)
	}
	var agg neDailyAgg
	for _, s := range rows {
		agg.add("75110", s)
	}
	if d := agg.rows(); len(d) == 0 || d[0].Changed != 1 {
		t.Fatalf("daily Changed should count only the good report's diff: %+v", d)
	}
}

// Layout rule (v0.3.3 rollout, docs/ne-cells.md §coverage): the first
// cell-block report of a KG compares against digests built under the old
// KG-bbox layout; its diffs are a re-baseline (change_suspect:"layout_change"),
// not change. Cell-block vs cell-block diffs stay counted — that is the only
// comparison the public m² change figure is built from.
func TestNEReportLayoutRule(t *testing.T) {
	kgBBox := []float64{12.7055, 46.6302, 12.8638, 46.7607}
	cellBBox := []float64{12.70, 46.62, 12.88, 46.78}
	mk := func(at, src string, bbox []float64, chunks map[string]string) neStoredReport {
		return neStoredReport{ReceivedAt: at, Token: "srtm", Report: neReport{
			KG: "75110", Algo: "ne-cells-2", Epoch: "2026-10", Source: src, Observer: "srtm", ChunkRes: 10, CellsN: 168243, BBox: bbox, Chunks: chunks},
			Result: map[string]interface{}{}}
	}
	old := mk("2026-10-05T06:00:00Z", "bevdirect@v0.3.2", kgBBox, map[string]string{"a": "1", "b": "1"})
	first33 := mk("2026-10-06T17:00:00Z", "bevdirect@v0.3.3", cellBBox, map[string]string{"a": "2", "b": "1"})
	sl := neSinceLast(neReportHistory([]neStoredReport{old}, &first33.Report), &first33.Report)
	if !neLayoutSuspect(sl) || sl["changed_previous_bbox_differs"] != 1 {
		t.Fatalf("KG-bbox → cell-block diff must be layout-suspect: %v", sl)
	}
	// same cell block again, a real diff: not suspect
	second33 := mk("2026-10-07T17:00:00Z", "bevdirect@v0.3.3", cellBBox, map[string]string{"a": "3", "b": "1"})
	sl = neSinceLast(neReportHistory([]neStoredReport{old, first33}, &second33.Report), &second33.Report)
	if neLayoutSuspect(sl) || len(sl["changed"].([]string)) != 1 {
		t.Fatalf("cell-block vs cell-block diff must count: %v", sl)
	}
	// aligned vs aligned under ANOTHER bbox (map peer's viewport over srtm's cell
	// block, 2026-10-24): one layout class — a real diff, a valid sighting
	wide := mk("2026-10-08T17:00:00Z", "bevdirect@v0.3.5", []float64{12.68, 46.60, 12.90, 46.80}, map[string]string{"a": "4", "b": "1"})
	sl = neSinceLast(neReportHistory([]neStoredReport{old, first33, second33}, &wide.Report), &wide.Report)
	if neLayoutSuspect(sl) || len(sl["changed"].([]string)) != 1 {
		t.Fatalf("aligned viewport vs aligned cell block must count: %v", sl)
	}
	// …but a legacy build under another bbox stays its own layout
	if !neAlignedSource("bevdirect@v0.3.3") || !neAlignedSource("bevdirect@v0.4.0") || !neAlignedSource("bevdirect@v1.0.0") ||
		neAlignedSource("bevdirect@v0.3.2") || neAlignedSource("bevdirect") || neAlignedSource("index-2026-03") {
		t.Fatal("neAlignedSource")
	}
	// retroactive marking + daily aggregate: only the same-layout diff is change
	old.Result["since_last"] = map[string]interface{}{"changed": []interface{}{}}
	first33.Result["since_last"] = map[string]interface{}{"changed": []interface{}{"a"}}
	second33.Result["since_last"] = map[string]interface{}{"changed": []interface{}{"a"}}
	rows := neMarkLossy([]neStoredReport{old, first33, second33})
	if rows[1].Result["change_suspect"] != "layout_change" || neStoredSuspect(&rows[0]) || neStoredSuspect(&rows[2]) {
		t.Fatalf("retro: %v / %v / %v", rows[0].Result, rows[1].Result, rows[2].Result)
	}
	// a line stored as layout_change under the old per-bbox rule (aligned vs aligned) is cleared on read
	wide.Result["since_last"] = map[string]interface{}{"changed": []interface{}{"a"}, "changed_previous_bbox_differs": 1}
	wide.Result["change_suspect"] = "layout_change"
	if r4 := neMarkLossy([]neStoredReport{old, first33, second33, wide}); neStoredSuspect(&r4[3]) {
		t.Fatalf("retro aligned/aligned must clear layout_change: %v", r4[3].Result)
	}
	var agg neDailyAgg
	for _, s := range rows {
		agg.add("75110", s)
	}
	d := agg.rows()
	if len(d) < 3 || d[1].Changed != 0 || d[2].Changed != 1 {
		t.Fatalf("daily Changed must exclude the layout switch: %+v", d)
	}
}
