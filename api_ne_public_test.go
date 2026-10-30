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
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// nePublicEnv — temp store with 63330 (lu+bev) and the adjacent pair
// 01002/01004 (border cells), public-only tier, one token.
type nePublicEnv struct {
	dir string
	h   http.Handler
	tok string
}

func newNEPublicEnv(t *testing.T) (*nePublicEnv, func()) {
	t.Helper()
	dir, cleanup := neTestStore(t)
	for _, kg := range []string{"01002", "01004"} {
		if b := neFixtureInput(kg, "lu"); len(b) > 0 {
			os.WriteFile(filepath.Join(dir, kg+".nec"), b, 0o644)
		}
	}
	if n := neStore.convertPending(); n < 1 {
		t.Fatalf("converted %d", n)
	}
	const tok = "0123456789abcdef0123"
	setTierConfigForTest(true, tok)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/ne", handleNE)
	mux.HandleFunc("/api/v1/ne/", handleNE)
	mux.HandleFunc("/api/v1/docs/ne.md", serveNEPublicDoc)
	mux.HandleFunc("/api/v1/docs/ne-change.md", serveNEChangeDoc)
	mux.HandleFunc("/api/v1/docs/llm.txt", servePublicLLM)
	RegisterContextAPI(mux)
	saved := kgBoxes
	kgBoxes = []kgBox{{code: "63330", minLon: 15.0712, minLat: 47.0902, maxLon: 15.1517, maxLat: 47.1553}}
	kgBoxLoaded = true
	return &nePublicEnv{dir: dir, h: tierMiddleware(mux), tok: tok}, func() { kgBoxes = saved; cleanup() }
}

func (e *nePublicEnv) do(method, path string, body []byte, hdr ...string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.RemoteAddr = "10.0.0.1:1234"
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	if strings.HasPrefix(path, "/k/") {
		req.Header.Set("Authorization", "Bearer "+e.tok)
	}
	e.h.ServeHTTP(rr, req)
	return rr
}

func neSrcRows(t *testing.T, kg string) (lu, k []byte) {
	t.Helper()
	kk := neStore.get(kg)
	if kk == nil {
		t.Fatalf("%s not in store", kg)
	}
	lu, k, err := neStore.layerRows(kk, "lu")
	if err != nil {
		t.Fatal(err)
	}
	return lu, k
}

func TestNEPublicWordBan(t *testing.T) {
	for _, f := range []string{"api_ne_public.go", "ne_h3.go", "ne_store.go", "api_docs_public_ne.go", "ne_change.go", "api_ne_change.go", "ne_glyph.go", "ne_glyph_filt.go", "ne_glyph_fidx.go", "ne_filter_expr.go", "api_ne_filter.go", "ne_glyph_filter.js", "api_ne_stats_unit.go", "ne_glyph_export.go", "docs/stats-envelope.md"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if bad := nePublicBannedWords(string(b)); len(bad) > 0 {
			t.Errorf("%s contains banned words %v", f, bad)
		}
	}
	if bad := nePublicBannedWords(publicNELLMSection); len(bad) > 0 {
		t.Errorf("llm.txt NE section: %v", bad)
	}
	if bad := nePublicBannedWords(publicNEHTMLSection); len(bad) > 0 {
		t.Errorf("landing page NE section: %v", bad)
	}
	if html := publicDocsHTMLText(); !strings.Contains(html, "/api/v1/ne/cells") || !strings.Contains(html, "/api/v1/docs/ne.md") || strings.Contains(html, "{NE}") {
		t.Errorf("landing page must document NE cells and link the contract")
	}
	if bad := nePublicBannedWords(nePublicDocText()); len(bad) > 0 {
		t.Errorf("/api/v1/docs/ne.md: %v", bad)
	}
	if bad := nePublicBannedWords(nePublicChangeDocText()); len(bad) > 0 {
		t.Errorf("/api/v1/docs/ne-change.md: %v", bad)
	}
	// the public docs never mention the private prefix
	for name, txt := range map[string]string{"ne.md": nePublicDocText(), "llm.txt": publicLLMText(nil), "index.html": publicDocsHTMLText(), "ne-change.md": nePublicChangeDocText()} {
		if strings.Contains(txt, "/k/") || strings.Contains(txt, "UMFELD_TOKEN") {
			t.Errorf("%s mentions the private prefix", name)
		}
	}
	env, cleanup := newNEPublicEnv(t)
	defer cleanup()
	lu, _ := neSrcRows(t, "63330")
	id := neCellHex(neRowID(lu, 1000, 30))
	c9 := neCellHex(h3Parent(neRowID(lu, 1000, 30), 9))
	k10 := neCellHex(h3Parent(neRowID(lu, 1000, 30), 10))
	paths := []string{"/api/v1/ne", "/api/v1/ne/manifest", "/api/v1/ne/manifest?fields=digests", "/api/v1/ne/63330?format=json&layers=lu",
		"/api/v1/ne/63330/head", "/api/v1/ne/chunk/" + c9 + "?format=json", "/api/v1/ne/cell/" + id, "/api/v1/ne/cell/" + k10,
		"/api/v1/ne/cells?ids=" + id + "," + k10 + "&format=json", "/api/v1/ne/cells?bbox=15.10,47.12,15.11,47.13&format=json",
		"/api/v1/ne/cells?bbox=15.10,47.12,15.11,47.13&list=1", "/api/v1/ne/cells?lon=15.11&lat=47.12&k=1&format=json",
		"/api/v1/context?lon=15.11&lat=47.12&include=ne", "/api/v1/docs/ne.md", "/api/v1/docs/ne-change.md"}
	for _, p := range paths {
		rr := env.do("GET", p, nil)
		if rr.Code != 200 {
			t.Errorf("%s: %d %s", p, rr.Code, rr.Body.String()[:min(200, rr.Body.Len())])
			continue
		}
		var hb bytes.Buffer
		rr.Header().Write(&hb)
		if bad := nePublicBannedWords(rr.Body.String() + hb.String()); len(bad) > 0 {
			t.Errorf("%s: banned words %v", p, bad)
		}
		if strings.Contains(rr.Body.String(), "/k/") {
			t.Errorf("%s: public answer mentions the private prefix", p)
		}
	}
	// the rewritten container header too
	rr := env.do("GET", "/api/v1/ne/63330?layers=lu", nil)
	secs, err := parseNESections(rr.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	hj, _ := json.Marshal(secs[0].Header)
	if bad := nePublicBannedWords(string(hj)); len(bad) > 0 {
		t.Errorf("public container header: %v", bad)
	}
	if _, ok := secs[0].Header["input_rule"]; ok || secs[0].Header["digest"] != "1233f3e4f9d07c27" {
		t.Errorf("public header rewrite: %v", secs[0].Header["inputs"])
	}
	// private tier keeps the stored header verbatim
	rr = env.do("GET", "/k/api/v1/ne/63330?layers=lu", nil)
	secs, _ = parseNESections(rr.Body.Bytes())
	if _, ok := secs[0].Header["input_rule"]; !ok {
		t.Error("private header must be verbatim")
	}
}

func TestNEPublicWholeAndHead(t *testing.T) {
	env, cleanup := newNEPublicEnv(t)
	defer cleanup()
	rr := env.do("GET", "/api/v1/ne/63330", nil)
	if rr.Code != 200 || rr.Header().Get("Content-Type") != "application/vnd.ne-cells" || !strings.HasPrefix(rr.Header().Get("ETag"), `"ne-1233f3e4f9d07c27-`) {
		t.Fatalf("%d %q %q", rr.Code, rr.Header().Get("Content-Type"), rr.Header().Get("ETag"))
	}
	if rr.Header().Get("X-NE-Layers-Missing") != "obs" || !strings.Contains(rr.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("headers %v", rr.Header())
	}
	etag := rr.Header().Get("ETag")
	if rr2 := env.do("GET", "/api/v1/ne/63330", nil, "If-None-Match", etag); rr2.Code != 304 {
		t.Fatalf("304 expected, got %d", rr2.Code)
	}
	full := rr.Body.Bytes()
	rr3 := env.do("GET", "/api/v1/ne/63330", nil, "Range", "bytes=100-199")
	if rr3.Code != 206 || !bytes.Equal(rr3.Body.Bytes(), full[100:200]) {
		t.Fatalf("range: %d %d", rr3.Code, rr3.Body.Len())
	}
	// bev layer present, obs → missing; public obs request = missing by policy
	rr = env.do("GET", "/api/v1/ne/63330?layers=lu,bev,obs", nil)
	secs, _ := parseNESections(rr.Body.Bytes())
	if len(secs) != 2 || rr.Header().Get("X-NE-Layers-Missing") != "obs" {
		t.Fatalf("lu+bev: %d sections, missing %q", len(secs), rr.Header().Get("X-NE-Layers-Missing"))
	}
	rr = env.do("GET", "/api/v1/ne/63330/head", nil)
	var js map[string]interface{}
	json.Unmarshal(rr.Body.Bytes(), &js)
	if rr.Code != 200 || js["lu"].(map[string]interface{})["digest"] != "1233f3e4f9d07c27" || js["obs"] != nil {
		t.Fatalf("head: %d %s", rr.Code, rr.Body.String()[:200])
	}
	// unbuilt
	rr = env.do("GET", "/api/v1/ne/63331", nil)
	if rr.Code != 404 || rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unbuilt: %d", rr.Code)
	}
	// manifest digests
	rr = env.do("GET", "/api/v1/ne/manifest?fields=digests", nil)
	json.Unmarshal(rr.Body.Bytes(), &js)
	if rr.Code != 200 || js["kgs"].(map[string]interface{})["63330"].(map[string]interface{})["lu"] != "1233f3e4f9d07c27" {
		t.Fatalf("digests: %d %s", rr.Code, rr.Body.String()[:200])
	}
	// dict
	rr = env.do("GET", "/api/v1/ne/dict/lu-1", nil)
	if rr.Code != 200 || rr.Body.Len() != 16384 {
		t.Fatalf("dict: %d %d", rr.Code, rr.Body.Len())
	}
}

func TestNEPublicChunkAndCells(t *testing.T) {
	env, cleanup := newNEPublicEnv(t)
	defer cleanup()
	lu, krows := neSrcRows(t, "63330")
	// pick an interior chunk: the res-9 parent of row 20000
	id := neRowID(lu, 20000, 30)
	c9 := h3Parent(id, 9)
	var want []byte
	for i := 0; i < len(lu)/30; i++ {
		if h3Parent(neRowID(lu, i, 30), 9) == c9 {
			want = append(want, lu[i*30:(i+1)*30]...)
		}
	}
	// zstd pass-through: decode with the dictionary → source rows
	rr := env.do("GET", "/api/v1/ne/chunk/"+neCellHex(c9), nil)
	if rr.Code != 200 || rr.Header().Get("Content-Type") != "application/zstd" || rr.Header().Get("X-NE-Dict") != "lu-1" {
		t.Fatalf("chunk zstd: %d %v", rr.Code, rr.Header())
	}
	if !strings.HasPrefix(rr.Header().Get("X-NE-KGs"), "63330:1.00") {
		t.Fatalf("X-NE-KGs %q", rr.Header().Get("X-NE-KGs"))
	}
	got, err := neStore.decompress(rr.Body.Bytes(), len(want))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("zstd frame decode: %v %d vs %d", err, len(got), len(want))
	}
	etag := rr.Header().Get("ETag")
	if !strings.HasPrefix(etag, `"nec-`) || !strings.Contains(rr.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("etag %q cc %q", etag, rr.Header().Get("Cache-Control"))
	}
	if rr2 := env.do("GET", "/api/v1/ne/chunk/"+neCellHex(c9), nil, "If-None-Match", etag); rr2.Code != 304 {
		t.Fatalf("chunk 304: %d", rr2.Code)
	}
	// ETag stable across a store reload
	neResetStoreForTest()
	neStore.load()
	if rr3 := env.do("GET", "/api/v1/ne/chunk/"+neCellHex(c9), nil); rr3.Header().Get("ETag") != etag {
		t.Fatalf("etag changed across restart: %q vs %q", rr3.Header().Get("ETag"), etag)
	}
	// bin: rows byte-equal (lu), K rows follow
	rr = env.do("GET", "/api/v1/ne/chunk/"+neCellHex(c9)+"?format=bin", nil)
	if rr.Code != 200 || !bytes.HasPrefix(rr.Body.Bytes(), want) || !strings.HasPrefix(rr.Header().Get("X-NE-Rows"), fmt.Sprintf("lu=30:%d;k=29:", len(want)/30)) {
		t.Fatalf("chunk bin: %d %q", rr.Code, rr.Header().Get("X-NE-Rows"))
	}
	// json columnar with public names
	rr = env.do("GET", "/api/v1/ne/chunk/"+neCellHex(c9)+"?format=json", nil)
	var js map[string]interface{}
	json.Unmarshal(rr.Body.Bytes(), &js)
	cols := js["lu"].(map[string]interface{})
	if int(js["n"].(float64)) != len(want)/30 || cols["n_units"] == nil || cols["n_parc"] != nil || cols["str_cover"] == nil {
		t.Fatalf("chunk json: n=%v keys=%d", js["n"], len(cols))
	}
	// private tier: stored names
	rr = env.do("GET", "/k/api/v1/ne/chunk/"+neCellHex(c9)+"?format=json", nil)
	json.Unmarshal(rr.Body.Bytes(), &js)
	if js["lu"].(map[string]interface{})["n_parc"] == nil {
		t.Fatal("private json must use stored names")
	}
	// cell res 12
	rr = env.do("GET", "/api/v1/ne/cell/"+neCellHex(id), nil)
	json.Unmarshal(rr.Body.Bytes(), &js)
	if rr.Code != 200 || js["cell"] != neCellHex(id) || js["kgs"].(map[string]interface{})["63330"] == nil {
		t.Fatalf("cell: %d %s", rr.Code, rr.Body.String()[:200])
	}
	// res 9 → 302
	if rr := env.do("GET", "/api/v1/ne/cell/"+neCellHex(c9), nil); rr.Code != 302 || !strings.Contains(rr.Header().Get("Location"), "/api/v1/ne/chunk/") {
		t.Fatalf("res9 redirect: %d %q", rr.Code, rr.Header().Get("Location"))
	}
	// res 10 with a K record: public bucketed, private exact
	k10 := neRowID(krows, 0, 29)
	n := binary.LittleEndian.Uint16(krows[8:])
	p50 := binary.LittleEndian.Uint32(krows[14:])
	rr = env.do("GET", "/api/v1/ne/cell/"+neCellHex(k10), nil)
	json.Unmarshal(rr.Body.Bytes(), &js)
	kr := js["k_record"].(map[string]interface{})
	if rr.Code != 200 || kr["unit_p50"] == nil || uint32(kr["unit_p50"].(float64)) != neBucket(p50) {
		t.Fatalf("public K: %d %v (p50 %d bucket %d)", rr.Code, kr, p50, neBucket(p50))
	}
	if n < 10 && kr["unit_p10"] != nil {
		t.Fatalf("p10 must be withheld below 10 units: %v", kr)
	}
	rr = env.do("GET", "/k/api/v1/ne/cell/"+neCellHex(k10), nil)
	json.Unmarshal(rr.Body.Bytes(), &js)
	if kr := js["k_record"].(map[string]interface{}); uint32(kr["parc_p50"].(float64)) != p50 {
		t.Fatalf("private K must be exact: %v", kr)
	}
	// bucket sanity: 1/8 octave ≈ 9 % classes, monotone
	if neBucket(1000) < 950 || neBucket(1000) > 1050 || neBucket(999) > neBucket(1100) {
		t.Fatalf("bucket %d %d", neBucket(1000), neBucket(999))
	}
	// cells ids (bin default): lu rows then K rows (public, bucketed)
	rr = env.do("GET", "/api/v1/ne/cells?ids="+neCellHex(id)+","+neCellHex(k10), nil)
	if rr.Code != 200 || rr.Header().Get("Content-Type") != "application/vnd.ne-rows" || !strings.HasPrefix(rr.Header().Get("X-NE-Rows"), "lu=30:") {
		t.Fatalf("cells bin: %d %v", rr.Code, rr.Header())
	}
	rows := rr.Header().Get("X-NE-Rows")
	nLU, _ := strconv.Atoi(strings.TrimPrefix(strings.Split(rows, ";")[0], "lu=30:"))
	if nLU < 1 || !bytes.Equal(rr.Body.Bytes()[:30], neFindRow(lu, 30, id)) && nLU == 1 {
		t.Fatalf("cells bin rows: %q", rows)
	}
	// POST ids
	body, _ := json.Marshal(map[string][]string{"ids": {neCellHex(id), neCellHex(neRowID(lu, 5, 30))}})
	rr = env.do("POST", "/api/v1/ne/cells?format=json", body, "Content-Type", "application/json")
	json.Unmarshal(rr.Body.Bytes(), &js)
	if rr.Code != 200 || int(js["n"].(float64)) != 2 {
		t.Fatalf("post cells: %d %s", rr.Code, rr.Body.String()[:200])
	}
	// too many ids
	many := make([]string, 513)
	for i := range many {
		many[i] = neCellHex(id)
	}
	if rr := env.do("GET", "/api/v1/ne/cells?ids="+strings.Join(many, ","), nil); rr.Code != 413 {
		t.Fatalf("513 ids: %d", rr.Code)
	}
	// bbox: span too large → 413 split; 0.02° cell → ≤ 16 384 cells, list=1 gives chunks + etags
	rr = env.do("GET", "/api/v1/ne/cells?bbox=15.0,47.0,15.1,47.1", nil)
	json.Unmarshal(rr.Body.Bytes(), &js)
	if rr.Code != 413 || len(js["split"].([]interface{})) != 4 {
		t.Fatalf("split: %d %s", rr.Code, rr.Body.String())
	}
	rr = env.do("GET", "/api/v1/ne/cells?bbox=15.10,47.12,15.12,47.14&format=json", nil)
	json.Unmarshal(rr.Body.Bytes(), &js)
	if rr.Code != 200 || js["n"].(float64) < 5000 || js["n"].(float64) > 16384 || js["pending"] != nil {
		t.Fatalf("bbox: %d n=%v pending=%v", rr.Code, js["n"], js["pending"])
	}
	rr = env.do("GET", "/api/v1/ne/cells?bbox=15.10,47.12,15.12,47.14&list=1", nil)
	json.Unmarshal(rr.Body.Bytes(), &js)
	if rr.Code != 200 || len(js["chunks"].([]interface{})) < 20 || len(js["etags"].([]interface{})) != len(js["chunks"].([]interface{})) {
		t.Fatalf("list: %d %s", rr.Code, rr.Body.String()[:200])
	}
	// point + k-ring
	rr = env.do("GET", "/api/v1/ne/cells?lon=15.11&lat=47.12&k=2&format=json", nil)
	json.Unmarshal(rr.Body.Bytes(), &js)
	if rr.Code != 200 || int(js["n"].(float64)) != 19 {
		t.Fatalf("k-ring: %d n=%v", rr.Code, js["n"])
	}
	if rr := env.do("GET", "/api/v1/ne/cells?lon=15.11&lat=47.12&k=4", nil); rr.Code != 400 {
		t.Fatalf("k=4: %d", rr.Code)
	}
	// point + k-ring with the default (bin) format used to panic: columnar()
	// was called on a result collected without kgs. Regression guard.
	rr = env.do("GET", "/api/v1/ne/cells?lon=15.11&lat=47.12&k=1", nil)
	if rr.Code != 200 || rr.Header().Get("X-NE-Centre-Cell") == "" || rr.Header().Get("X-NE-N") != "7" {
		t.Fatalf("k-ring bin: %d centre=%q n=%q", rr.Code, rr.Header().Get("X-NE-Centre-Cell"), rr.Header().Get("X-NE-N"))
	}
	// LU rows identical on both tiers (bin)
	a := env.do("GET", "/api/v1/ne/cells?ids="+neCellHex(id), nil).Body.Bytes()[:30]
	b := env.do("GET", "/k/api/v1/ne/cells?ids="+neCellHex(id), nil).Body.Bytes()[:30]
	if !bytes.Equal(a, b) {
		t.Fatal("LU rows must be byte-identical on both tiers")
	}
	// context include=ne
	rr = env.do("GET", "/api/v1/context?lon=15.11&lat=47.12&include=ne", nil)
	json.Unmarshal(rr.Body.Bytes(), &js)
	data := js["data"].(map[string]interface{})
	if rr.Code != 200 || data["ne"] == nil || data["ne"].(map[string]interface{})["lu"] == nil {
		t.Fatalf("context ne: %d %s", rr.Code, rr.Body.String()[:300])
	}
}

func TestNEPublicBorderAndPending(t *testing.T) {
	env, cleanup := newNEPublicEnv(t)
	defer cleanup()
	if neStore.get("01002") == nil || neStore.get("01004") == nil {
		t.Skip("01002/01004 inputs not available")
	}
	a, _ := neSrcRows(t, "01002")
	b, _ := neSrcRows(t, "01004")
	var border uint64
	for i := 0; i < len(a)/30 && border == 0; i++ {
		id := neRowID(a, i, 30)
		if neFindRow(b, 30, id) != nil {
			border = id
		}
	}
	if border == 0 {
		t.Skip("no shared cell")
	}
	rr := env.do("GET", "/api/v1/ne/cells?ids="+neCellHex(border)+"&format=json", nil)
	var js map[string]interface{}
	json.Unmarshal(rr.Body.Bytes(), &js)
	kgs := js["kgs"].([]interface{})[0].(map[string]interface{})
	if int(js["n"].(float64)) != 1 || len(kgs) != 2 {
		t.Fatalf("border cell: n=%v kgs=%v", js["n"], kgs)
	}
	// row served = largest share
	var sa, sb uint8 = neFindRow(a, 30, border)[8], neFindRow(b, 30, border)[8]
	got := uint8(js["lu"].(map[string]interface{})["kg_share"].([]interface{})[0].(float64))
	if got != max(sa, sb) {
		t.Fatalf("share %d want max(%d,%d)", got, sa, sb)
	}
	// the chunk answer carries both KGs' frames
	rr = env.do("GET", "/api/v1/ne/chunk/"+neCellHex(h3Parent(border, 9)), nil)
	if !strings.Contains(rr.Header().Get("X-NE-KGs"), "01002:") || !strings.Contains(rr.Header().Get("X-NE-KGs"), "01004:") || len(strings.Split(rr.Header().Get("X-NE-Frames"), ",")) != 2 {
		t.Fatalf("chunk kgs %q frames %q", rr.Header().Get("X-NE-KGs"), rr.Header().Get("X-NE-Frames"))
	}
	// unbuilt KG inside the bbox → 200 + pending_kgs + no-store
	saved := kgBoxes
	kgBoxes = append(kgBoxes, kgBox{code: "63331", minLon: 15.10, minLat: 47.12, maxLon: 15.13, maxLat: 47.15})
	defer func() { kgBoxes = saved }()
	rr = env.do("GET", "/api/v1/ne/cells?bbox=15.10,47.12,15.12,47.14&format=json", nil)
	json.Unmarshal(rr.Body.Bytes(), &js)
	if rr.Code != 200 || js["pending"] != true || rr.Header().Get("X-NE-Pending-KGs") != "63331" || rr.Header().Get("Cache-Control") != "no-store" || js["n"].(float64) < 5000 {
		t.Fatalf("pending: %d %v %q %q", rr.Code, js["pending"], rr.Header().Get("X-NE-Pending-KGs"), rr.Header().Get("Cache-Control"))
	}
}

func TestNEPublicRateLimitAndTier(t *testing.T) {
	env, cleanup := newNEPublicEnv(t)
	defer cleanup()
	// /k/ without token → 404; obs/report/draft never public
	rr := httptest.NewRecorder()
	env.h.ServeHTTP(rr, httptest.NewRequest("GET", "/k/api/v1/ne/63330", nil))
	if rr.Code != 404 {
		t.Fatalf("/k without token: %d", rr.Code)
	}
	for _, p := range []string{"/api/v1/ne/63330/report", "/api/v1/ne/draft/ne_cells.tar.gz"} {
		if rr := env.do("GET", p, nil); rr.Code != 404 {
			t.Fatalf("%s public: %d", p, rr.Code)
		}
	}
	// token bucket: burst 60 then 429 within a millisecond, Retry-After set
	neRateMu.Lock()
	delete(neRateBuckets, "10.9.9.9")
	neRateMu.Unlock()
	now := time.Now()
	for i := 0; i < 60; i++ {
		if ok, _ := neRateAllow("10.9.9.9", now); !ok {
			t.Fatalf("request %d refused", i)
		}
	}
	t0 := time.Now()
	ok, wait := neRateAllow("10.9.9.9", now)
	if ok || wait < 1 || time.Since(t0) > time.Millisecond {
		t.Fatalf("61st: ok=%v wait=%d dt=%v", ok, wait, time.Since(t0))
	}
	req := httptest.NewRequest("GET", "/api/v1/ne", nil)
	req.RemoteAddr = "10.9.9.9:1"
	rr = httptest.NewRecorder()
	env.h.ServeHTTP(rr, req)
	if rr.Code != 429 || rr.Header().Get("Retry-After") == "" {
		t.Fatalf("429 expected: %d", rr.Code)
	}
	// nothing on the read path touches the search index
	if globalSearchIndex != nil {
		t.Fatal("test expects no search index")
	}
	// X-Forwarded-For wins over RemoteAddr; the LAST hop is the one the proxy saw
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 10.0.0.1")
	if neClientIP(req) != "10.0.0.1" {
		t.Fatal("xff")
	}
	req.Header.Del("X-Forwarded-For")
	req.RemoteAddr = "127.0.0.1:5"
	if neClientIP(req) != "" {
		t.Fatal("loopback without XFF must be exempt")
	}
	req.RemoteAddr = "[::1]:5"
	if neClientIP(req) != "" {
		t.Fatal("::1 without XFF must be exempt")
	}
	// word ban regex sanity
	if !regexp.MustCompile(`\bez\b`).MatchString("the ez value") || nePublicWordRe.MatchString("reg_n ez_n units") {
		t.Fatal("ban regex")
	}
}

// TestNEContribChart — the daily "observed by peers" tracker on the landing
// page: unit is the res-10 chunk, re-observed = seen on an earlier day, the
// block is ban-checked and never names the private prefix.
func TestNEContribChart(t *testing.T) {
	a := neDailyAgg{}
	rep := func(day, kg string, chunks ...string) neStoredReport {
		m := map[string]string{}
		for _, c := range chunks {
			m[c] = "d"
		}
		return neStoredReport{ReceivedAt: day + "T10:00:00Z", Report: neReport{KG: kg, Source: "bevdirect@v0.3.1", Chunks: m}}
	}
	a.add("63330", rep("2026-09-04", "63330", "a", "b"))
	a.add("63330", rep("2026-09-06", "63330", "b", "c"))
	a.add("73119", rep("2026-09-06", "73119", "c", "x")) // same day, other KG: c not re-observed twice
	for i := 7; i <= 16; i++ {                           // b every day 07..16 → its 3rd..12th pass
		a.add("63330", rep(fmt.Sprintf("2026-09-%02d", i), "63330", "b"))
	}
	rows := a.rows()
	if len(rows) < 13 || rows[0].Day != "2026-09-04" || rows[1].Chunks != 0 || rows[2].Chunks != 3 || rows[2].Reobserved != 1 || rows[2].KGs != 2 {
		t.Fatalf("rows: %+v", rows)
	}
	if rows[2].Passes != [nePassBandN]int{2, 1, 0, 0, 0} || rows[3].Passes != [nePassBandN]int{0, 0, 1, 0, 0} || rows[5].Passes != [nePassBandN]int{0, 0, 0, 1, 0} || rows[10].Passes != [nePassBandN]int{0, 0, 0, 0, 1} {
		t.Fatalf("pass bands: %+v %+v %+v %+v", rows[2].Passes, rows[3].Passes, rows[5].Passes, rows[10].Passes)
	}
	once, twice, deepest := a.everDepth()
	if once != 4 || twice != 1 || deepest != 12 {
		t.Fatalf("ever: %d %d %d", once, twice, deepest)
	}
	// tags: an equivalent tag continues the pass depth, a foreign class restarts at 1st
	eq := rep("2026-09-17", "63330", "b")
	eq.Report.Source = "bevdirect@v0.3.2"
	a.add("63330", eq)
	foreign := rep("2026-09-17", "63330", "a")
	foreign.Report.Source = "bevdirect@v0.1.0"
	a.add("63330", foreign)
	rows = a.rows()
	r := rows[13]
	if r.Passes != [nePassBandN]int{1, 0, 0, 0, 1} || r.Sources["bevdirect@v0.3.2"] != 1 || r.Sources["bevdirect@v0.1.0"] != 1 {
		t.Fatalf("class split: %+v", r)
	}
	if once, twice, deepest = a.everDepth(); once != 4 || twice != 1 || deepest != 13 {
		t.Fatalf("ever after class split (union, per-class depth): %d %d %d", once, twice, deepest)
	}
	if mix := neSourceMix(map[string]int{"bevdirect@v0.3.1": 9, "bevdirect@v0.3.2": 1, "bevdirect@v0.3.0": 1, "index-2026-03": 1}); mix != "bevdirect@v0.3.1 ×9, index-2026-03 ×1, bevdirect@v0.3.2 ×1, v0.3.0 ×1" {
		t.Fatalf("mix: %q", mix)
	}
	if txt := neContribSummaryText(rows[:14], map[string]int{"once": 4}); !strings.Contains(txt, "sources (7 d): bevdirect@v0.3.1 ×") || !strings.Contains(txt, "2 assembly classes") || strings.Contains(txt, "index-") {
		t.Fatalf("summary: %q", txt)
	}
	if svg := neContribChartSVG(rows[:14]); !strings.Contains(svg, "· bevdirect@v0.3.2 ×1, v0.1.0 ×1</title>") {
		t.Fatalf("tooltip lacks source mix")
	}
	svg := neContribChartSVG(rows[:13])
	for _, want := range []string{`class="p0"`, `class="p1"`, `class="p2"`, `class="p4"`, `url(#ne-h4)`, "2026-09-06 · ", "1st 0.0 km²", "10th+", "<defs>"} {
		if !strings.Contains(svg, want) {
			t.Fatalf("svg lacks %q: %s", want, svg)
		}
	}
	if strings.Count(svg, "<title>") != 13 {
		t.Fatalf("svg titles: %s", svg)
	}
	html := neContribChartHTML()
	if !strings.Contains(html, `class="contrib-tip"`) || !strings.Contains(html, `class="day" data-day=`) || !strings.Contains(html, "<script>") {
		t.Fatalf("contrib block lacks hover/pin tip: %s", html)
	}
	if rk := neContribRank(rows[:13], 2); !strings.Contains(rk, "busiest of") {
		t.Fatalf("rank: %q", rk)
	}
	cum13 := make([]int, 13)
	for i := range cum13 {
		cum13[i] = 5
	}
	card := neContribDetailHTML(rows[:13], cum13, map[string]neObsDay{rows[2].Day: {KGs: 3, Cells: 30000}})
	for _, want := range []string{`class="sec dec"`, `class="sec obs"`, "observed layer posted", "satellite &amp; LiDAR", "seen before", "first-sight to date"} {
		if !strings.Contains(card, want) {
			t.Fatalf("card lacks %q: %s", want, card)
		}
	}
	if bad := nePublicBannedWords(html); len(bad) > 0 || strings.Contains(html, "/k/") {
		t.Fatalf("contrib block: %v %q", bad, html)
	}
	if page := publicDocsHTMLText(); strings.Contains(page, "{CONTRIB}") || !strings.Contains(page, "Declared layer digested by peers") {
		t.Fatal("landing page must splice the tracker")
	}
}
