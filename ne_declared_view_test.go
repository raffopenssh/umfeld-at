package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNEDeclaredViewContract — the index-2026-03 `lu` layer is irrelevant
// everywhere (docs/ne-change.md §4b):
//  1. bare NEO1 validates against the declared VIEW cell set; the stored obs
//     header says chunks@<state>; GET /contrib/…/{kg}/declared serves the set
//  2. revisit lists no_log and obs_on_index
//  3. layers=declared on the public API = the view, lu unchanged
//  4. manifest per KG `declared{}` + kgs_obs_on_index
func TestNEDeclaredViewContract(t *testing.T) {
	dir := t.TempDir()
	old := neDirOverride(dir)
	defer neDirOverride(old)
	savedStore := neStore
	neResetStoreForTest()
	neStore.load()
	defer func() { neStore = savedStore }()
	neChangeLRU = &neChangeLRUT{kgs: map[string]*neChangeKG{}}
	neDeclStateMemo.m = map[string]*neDeclState{}
	neManifest.mu.Lock()
	neManifest.files, neManifest.body = map[string]neFileEntry{}, nil
	neManifest.mu.Unlock()

	luRec := func(cid uint64, group int) []byte {
		b := make([]byte, 30)
		binary.LittleEndian.PutUint64(b, cid)
		b[8], b[9], b[10] = 255, 255, 250
		b[12+group] = 200
		b[24], b[25] = 0xAB, 0xCD // register bytes
		return b
	}
	const a, b, c, k10 = uint64(0x8c1e108cc0539ff), uint64(0x8c1e108cc0515ff), uint64(0x8c1e108cc0517ff), uint64(0x8a1e108cc057fff)
	nec := func(kg, source string, ids ...uint64) []byte {
		var raw []byte
		for _, id := range ids {
			raw = append(raw, luRec(id, 3)...)
		}
		k := make([]byte, 29)
		binary.LittleEndian.PutUint64(k, k10)
		raw = append(raw, k...)
		return packSection("NEC1", map[string]interface{}{"kg": kg, "algo": "ne-cells-2", "epoch": "2026-03", "source": source, "h3_res": 12, "cells_n": len(ids), "kcells_n": 1}, raw)
	}
	groups := []string{"geb", "bau", "acker", "gruen", "wald", "wasser", "verkehr", "alpen", "sonst"}
	obs := func(epoch string, ids ...uint64) []byte {
		var raw []byte
		for _, id := range ids {
			row := make([]byte, 41)
			binary.LittleEndian.PutUint64(row, id)
			row[9] = byte(len(epoch))
			raw = append(raw, row...)
		}
		return packSection("NEO1", map[string]interface{}{"kg": "63330", "algo": "obs-cells-2", "epoch": epoch, "source": "obs-peer@2.5", "h3_res": 12,
			"cells_n": len(ids), "groups": groups, "record_bytes": 41}, raw)
	}
	// lu(63330) = {a,b}; 63331 = lu only, no log (→ no_log)
	os.WriteFile(filepath.Join(dir, "63330.nec"), nec("63330", "index-2026-03", a, b), 0o644)
	os.WriteFile(filepath.Join(dir, "63331.nec"), nec("63331", "index-2026-03", a), 0o644)
	if n := neStore.convertPending(); n != 2 {
		t.Fatalf("converted %d", n)
	}
	// the log: one baseline version of a's res-10 chunk with a (now forest) and a new cell c
	ver := append(neStrip(luRec(a, 4)), neStrip(luRec(c, 0))...)
	ver = neSortRows(ver, neLURowBytes)
	ch := neCellHex(h3Parent(a, 10))
	if h3Parent(c, 10) != h3Parent(a, 10) || h3Parent(b, 10) != h3Parent(a, 10) {
		t.Fatalf("fixture cells must share a res-10 chunk")
	}
	if err := neChangeAppend("63330", neChunkVersion{Observer: "map-peer-x", Source: "bevdirect@v0.3.6", At: "2026-10-20T00:00:00Z"}, []neChangeItem{{Chunk: ch, Digest: neD16(ver), AP: neAPDigest(ver), Rows: ver}}); err != nil {
		t.Fatal(err)
	}
	// ── logged chunks are authoritative (§4b rev. 2): b sits in lu but not in the
	// logged version of its chunk → not in the view set; served superseded while no
	// other KG holds it, served from the neighbour once one does
	v := neDeclViewOf("63330")
	if v == nil || v.CellsN() != 2 || v.FromLog != 2 || v.FromLU != 0 || v.Hidden != 1 || !strings.HasPrefix(v.Source, "chunks@") || v.ChunksN != 1 {
		t.Fatalf("view: %+v", v)
	}
	if set := v.cellSet(); set[b] || !set[a] || !set[c] {
		t.Fatalf("view set must be {a,c}: %v", set)
	}
	supRow := func() []byte {
		res := neCollect(nil, []uint64{b}, []string{"lu"}, &neReq{public: true, format: "bin", bevFirst: true})
		if len(res.ids) != 1 {
			t.Fatalf("b must never be a hole: %v", res.ids)
		}
		return res.lu[0]
	}
	if r := supRow(); r[11]&neFlagSuperseded == 0 {
		t.Fatalf("b must be served superseded (no KG's view holds it)")
	}
	if row, kgs, _ := neStore.declaredRow(b, true); row == nil || row[11]&neFlagSuperseded == 0 || kgs["63330"] != 255 {
		t.Fatalf("declaredRow b: %v %v", row, kgs)
	}
	if lrow, _, _ := neStore.declaredRow(b, false); lrow == nil || lrow[11]&neFlagSuperseded != 0 {
		t.Fatalf("lu path must never carry the flag")
	}
	if rr := neStore.get("63330"); rr != nil { // the stored row is untouched (serve time only)
		lu, _ := neSrcRows(t, "63330")
		for i := 0; i+neLURowBytes <= len(lu); i += neLURowBytes {
			if lu[i+11]&neFlagSuperseded != 0 {
				t.Fatalf("stored row must not carry the flag")
			}
		}
	}
	for i := 0; i < v.CellsN(); i++ {
		if r := v.Rows[i*neLURowBytes : (i+1)*neLURowBytes]; r[neRegOff0] != 0 || r[neRegOff1] != 0 {
			t.Fatalf("register bytes must be zero in the view")
		}
	}
	const srtm, ops, contrib = "0123456789abcdef0123", "fedcba9876543210fedc", "cccccccccccccccccccc"
	setTierConfigForTest(true, srtm, ops)
	tierNames = []string{peers().ObsName, "ops"} // slot 0 is the NE writer
	defer func() { tierNames = tierDefaultNames }()
	setContributorConfigForTest([]string{contrib}, []string{"map-peer"})
	defer setContributorConfigForTest(nil, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/ne", handleNE)
	mux.HandleFunc("/api/v1/ne/", handleNE)
	h := tierMiddleware(mux)
	do := func(method, path, tok string, body []byte) (*httptest.ResponseRecorder, map[string]interface{}) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.RemoteAddr = "10.0.0.1:1234"
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		h.ServeHTTP(rr, req)
		var js map[string]interface{}
		json.Unmarshal(rr.Body.Bytes(), &js)
		return rr, js
	}
	// ── 1. GET /{kg}/declared ──
	rr, _ := do("GET", "/contrib/api/v1/ne/63330/declared", contrib, nil)
	if rr.Code != 200 || rr.Header().Get("X-NE-Declared-Source") != v.Source {
		t.Fatalf("declared: %d %s", rr.Code, rr.Body.String())
	}
	secs, err := parseNESections(rr.Body.Bytes())
	if err != nil || len(secs) != 1 || secs[0].Magic != "NEC1" || secs[0].Header["cells_n"].(float64) != 2 || secs[0].Header["source"] != v.Source {
		t.Fatalf("declared section: %v %+v", err, secs)
	}
	if ids, err := neSectionCellIDs(&secs[0]); err != nil || !ids[a] || ids[b] || !ids[c] {
		t.Fatalf("declared ids: %v %v", err, ids)
	}
	etag := rr.Header().Get("ETag")
	if rr2, _ := do("GET", "/contrib/api/v1/ne/63330/declared?format=json", srtm, nil); rr2.Code != 200 || !strings.Contains(rr2.Body.String(), `"chunks_n":1`) {
		t.Fatalf("declared json: %d %s", rr2.Code, rr2.Body.String())
	}
	for _, p := range []string{"/api/v1/ne/63330/declared", "/contrib/api/v1/ne/63330/declared", "/k/api/v1/ne/63330/declared"} {
		if rr, _ := do("GET", p, "", nil); rr.Code != 404 {
			t.Fatalf("%s without token: %d", p, rr.Code)
		}
	}
	// bare NEO1 on the view set → stored, declared_source chunks@state
	rr, js := do("POST", "/contrib/api/v1/ne/63330/obs", srtm, obs("2026-10", a, c))
	if rr.Code != 200 || js["declared_layer"] != "declared" || js["declared_source"] != v.Source || js["declared_digest"] != v.Digest {
		t.Fatalf("bare NEO1 on view: %d %s", rr.Code, rr.Body.String())
	}
	if oh := neStore.get("63330").layerHeader("obs"); oh["declared_source"] != v.Source {
		t.Fatalf("stored obs header: %v", oh)
	}
	// bare NEO1 on the lu set → accepted (index source) with a hint; a third set → 409 naming the view
	rr, js = do("POST", "/contrib/api/v1/ne/63330/obs", srtm, obs("2026-11", a, b))
	if rr.Code != 200 || js["declared_layer"] != "lu" || js["declared_source"] != "index-2026-03" || js["hint"] == nil {
		t.Fatalf("bare NEO1 on lu: %d %s", rr.Code, rr.Body.String())
	}
	rr, js = do("POST", "/contrib/api/v1/ne/63330/obs", srtm, obs("2026-11", a, b, c))
	if rr.Code != 409 || js["view"] == nil || !strings.Contains(js["error"].(string), "/declared") {
		t.Fatalf("bare NEO1 on nothing: %d %s", rr.Code, rr.Body.String())
	}
	// ── 2. revisit: obs_on_index (63330, has_view) + no_log (63331) ──
	neManifest.refresh(true) // the POST's own refresh runs async; make the manifest state deterministic here
	rr, js = do("GET", "/contrib/api/v1/ne/revisit?refresh=1", contrib, nil)
	if rr.Code != 200 {
		t.Fatalf("revisit: %d %s", rr.Code, rr.Body.String())
	}
	oi, _ := js["obs_on_index"].([]interface{})
	nl, _ := js["no_log"].([]interface{})
	if len(oi) != 1 || oi[0].(map[string]interface{})["kg"] != "63330" || oi[0].(map[string]interface{})["has_view"] != true {
		t.Fatalf("obs_on_index: %v %s", oi, rr.Body.String())
	}
	if len(nl) != 1 || nl[0].(map[string]interface{})["kg"] != "63331" {
		t.Fatalf("no_log: %v", nl)
	}
	if tot := js["totals"].(map[string]interface{}); tot["kgs_no_log"].(float64) != 1 || tot["kgs_obs_on_index"].(float64) != 1 {
		t.Fatalf("totals: %v", tot)
	}
	// ── 3. public layered API ──
	rr, js = do("GET", fmt.Sprintf("/api/v1/ne/cell/%x?layers=lu", a), "", nil)
	luDom := js["lu"].(map[string]interface{})
	rr2, js2 := do("GET", fmt.Sprintf("/api/v1/ne/cell/%x?layers=declared", a), "", nil)
	if rr.Code != 200 || rr2.Code != 200 || rr.Header().Get("ETag") == rr2.Header().Get("ETag") {
		t.Fatalf("cell lu/declared: %d %d etags %q %q", rr.Code, rr2.Code, rr.Header().Get("ETag"), rr2.Header().Get("ETag"))
	}
	if fmt.Sprint(luDom["lu"]) == fmt.Sprint(js2["lu"].(map[string]interface{})["lu"]) {
		t.Fatalf("declared must differ from lu for cell a: %v vs %v", luDom["lu"], js2["lu"])
	}
	if rr, _ := do("GET", fmt.Sprintf("/api/v1/ne/cell/%x?layers=lu", c), "", nil); rr.Code != 404 {
		t.Fatalf("lu must not hold c: %d", rr.Code)
	}
	if rr, _ := do("GET", fmt.Sprintf("/api/v1/ne/cell/%x?layers=declared", c), "", nil); rr.Code != 200 {
		t.Fatalf("declared must hold c: %d %s", rr.Code, rr.Body.String())
	}
	if rr, _ := do("GET", fmt.Sprintf("/api/v1/ne/cell/%x?layers=lu,declared", a), "", nil); rr.Code != 400 {
		t.Fatalf("lu,declared: %d", rr.Code)
	}
	rr, js = do("GET", "/api/v1/ne/cells?bbox=15.11,47.10,15.13,47.115&format=json&layers=declared", "", nil)
	if rr.Code != 200 || js["declared"] != "view" || js["n"].(float64) != 3 || !strings.Contains(rr.Header().Get("Cache-Control"), "max-age=300") {
		t.Fatalf("cells declared: %d %s %s", rr.Code, rr.Header().Get("Cache-Control"), rr.Body.String())
	}
	if fl := js["lu"].(map[string]interface{})["flags"].([]interface{}); !strings.Contains(fmt.Sprint(fl), "superseded:true") {
		t.Fatalf("b must carry flags.superseded in the json: %v", fl)
	}
	if rr, js := do("GET", fmt.Sprintf("/api/v1/ne/cell/%x?layers=declared", b), "", nil); rr.Code != 200 || js["superseded"] != true {
		t.Fatalf("cell b superseded: %d %s", rr.Code, rr.Body.String())
	}
	if _, js := do("GET", fmt.Sprintf("/api/v1/ne/cell/%x?layers=declared", a), "", nil); js["superseded"] != nil {
		t.Fatalf("cell a must not be superseded")
	}
	rr, js = do("GET", "/api/v1/ne/63330?layers=declared&format=json", "", nil)
	if rr.Code != 200 || rr.Header().Get("X-NE-Declared-Source") != v.Source {
		t.Fatalf("whole declared: %d %s", rr.Code, rr.Body.String())
	}
	sec := js["sections"].([]interface{})[0].(map[string]interface{})
	if sec["header"].(map[string]interface{})["source"] != v.Source || len(sec["cells"].(map[string]interface{})) != 2 {
		t.Fatalf("whole declared section: %v", sec["header"])
	}
	rr, _ = do("GET", "/api/v1/ne/63330?layers=declared", "", nil)
	if secs, err := parseNESections(rr.Body.Bytes()); err != nil || len(secs) != 1 || secs[0].Header["digest"] != v.Digest {
		t.Fatalf("whole declared nec: %v", err)
	}
	if rr, _ := do("GET", "/api/v1/ne/63330?layers=lu&format=json", "", nil); !strings.Contains(rr.Body.String(), `"source":"index-2026-03"`) {
		t.Fatalf("lu must stay the index epoch: %s", rr.Body.String()[:200])
	}
	if rr, _ := do("GET", "/api/v1/ne/chunk/"+neCellHex(h3Parent(a, 9))+"?layers=declared&format=zstd", "", nil); rr.Code != 400 {
		t.Fatalf("chunk zstd declared: %d", rr.Code)
	}
	if rr, _ := do("GET", "/api/v1/ne/chunk/"+neCellHex(h3Parent(a, 9))+"?layers=declared&format=json", "", nil); rr.Code != 200 || !strings.Contains(rr.Body.String(), neCellHex(c)) {
		t.Fatalf("chunk json declared: %d", rr.Code)
	}
	_ = etag
	// ── 4. manifest ──
	neManifest.refresh(true)
	rr, js = do("GET", "/contrib/api/v1/ne/manifest", contrib, nil)
	if rr.Code != 200 || js["kgs_obs_on_index"].(float64) != 1 {
		t.Fatalf("manifest: %d kgs_obs_on_index=%v", rr.Code, js["kgs_obs_on_index"])
	}
	e := js["kgs"].(map[string]interface{})["63330"].(map[string]interface{})
	d, _ := e["declared"].(map[string]interface{})
	if d == nil || d["source"] != v.Source || d["state"] != v.State || d["newest_at"] != "2026-10-20T00:00:00Z" || d["coverage"].(float64) != 1 {
		t.Fatalf("manifest declared: %v", e["declared"])
	}
	if d := js["kgs"].(map[string]interface{})["63331"].(map[string]interface{})["declared"].(map[string]interface{}); d["source"] != "index-2026-03" {
		t.Fatalf("63331 declared: %v", d)
	}
	// re-post on the view clears obs_on_index
	if rr, _ := do("POST", "/contrib/api/v1/ne/63330/obs", srtm, obs("2026-12", a, c)); rr.Code != 200 {
		t.Fatalf("re-post: %d %s", rr.Code, rr.Body.String())
	}
	neManifest.refresh(true)
	if _, js := do("GET", "/contrib/api/v1/ne/manifest", contrib, nil); js["kgs_obs_on_index"].(float64) != 0 {
		t.Fatalf("kgs_obs_on_index after re-post: %v", js["kgs_obs_on_index"])
	}
	// a neighbour KG 63332 whose lu holds b (share 120, no log) → b is its cell now, not superseded, kgs = {63332}
	os.WriteFile(filepath.Join(dir, "63332.nec"), nec("63332", "index-2026-03", b), 0o644)
	if n := neStore.convertPending(); n != 1 {
		t.Fatalf("converted %d", n)
	}
	neStore.load()
	if r := supRow(); r[11]&neFlagSuperseded != 0 {
		t.Fatalf("b held by the neighbour's view must not be superseded")
	}
	if _, kgs, _ := neStore.declaredRow(b, true); len(kgs) != 1 || kgs["63332"] == 0 {
		t.Fatalf("b must come from the neighbour only: %v", kgs)
	}
	if v2 := neDeclViewOf("63330"); v2.CellsN() != 2 || v2.Hidden != 1 {
		t.Fatalf("63330 view unchanged by the neighbour: %+v", v2)
	}
}
