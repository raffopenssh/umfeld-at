package main

import (
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestKGUniverseFileContract — the committed file IS the contract: 7 850 KGs,
// universe_hash consistent with the codes, every code 5 digits, unique.
func TestKGUniverseFileContract(t *testing.T) {
	u, err := LoadKGUniverseFile(kgUniversePath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if u.KGCount != kgUniverseContract || len(u.KGs) != kgUniverseContract {
		t.Fatalf("kg_count=%d len=%d want %d", u.KGCount, len(u.KGs), kgUniverseContract)
	}
	if u.UniverseHash != KGUniverseHash(u.codes()) {
		t.Fatalf("hash mismatch")
	}
	seen := map[string]bool{}
	for _, r := range u.KGs {
		if len(r.KGCode) != 5 || !isNumericCode(r.KGCode) {
			t.Fatalf("bad code %q", r.KGCode)
		}
		if seen[r.KGCode] {
			t.Fatalf("duplicate %s", r.KGCode)
		}
		seen[r.KGCode] = true
		if r.KGName == "" || r.DistrictCode == "" || r.StateCode == "" {
			t.Fatalf("incomplete row %+v", r)
		}
	}
	if u.PeerCheck == nil || !u.PeerCheck.Agree {
		t.Fatalf("file must carry an agreeing peer_check from the observed-layer peer (run -kg-universe-build)")
	}
}

func TestKGUniverseHashRecipe(t *testing.T) {
	// sha256("00001\n00002") — order-independent.
	a := KGUniverseHash([]string{"00002", "00001"})
	b := KGUniverseHash([]string{"00001", "00002"})
	if a != b || len(a) != 64 {
		t.Fatalf("hash %s / %s", a, b)
	}
}

func TestKGUniverseHandler(t *testing.T) {
	u, err := LoadKGUniverseFile(kgUniversePath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	setKGUniverse(u, nil)
	kgUniverseMu.Lock()
	kgUniverseLive = nil
	kgUniverseMu.Unlock()

	get := func(path string, hdr map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rr := httptest.NewRecorder()
		handleV1KGs(rr, req)
		return rr
	}

	// Unfiltered, gzip: pre-compressed body, ETag = universe_hash.
	rr := get("/api/v1/kgs", map[string]string{"Accept-Encoding": "gzip"})
	if rr.Code != 200 || rr.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("code %d enc %q", rr.Code, rr.Header().Get("Content-Encoding"))
	}
	if rr.Header().Get("ETag") != `"`+u.UniverseHash+bboxRevTag(u)+`"` || rr.Header().Get("X-KG-Count") != "7850" {
		t.Fatalf("etag %s count %s", rr.Header().Get("ETag"), rr.Header().Get("X-KG-Count"))
	}
	zr, err := gzip.NewReader(rr.Body)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Data struct {
			KGCount  int             `json:"kg_count"`
			Contract int             `json:"kg_count_contract"`
			Hash     string          `json:"universe_hash"`
			KGs      []kgUniverseRow `json:"kgs"`
		} `json:"data"`
		Meta struct {
			License *LicenseMeta `json:"license"`
		} `json:"meta"`
	}
	if err := json.NewDecoder(zr).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if doc.Data.KGCount != 7850 || doc.Data.Contract != 7850 || len(doc.Data.KGs) != 7850 || doc.Data.Hash != u.UniverseHash {
		t.Fatalf("body: count=%d contract=%d rows=%d", doc.Data.KGCount, doc.Data.Contract, len(doc.Data.KGs))
	}
	if doc.Meta.License == nil || strings.Contains(doc.Meta.License.Attribution, "Kataster") && !strings.Contains(doc.Meta.License.Attribution, "Katastralgemeinden") {
		t.Fatalf("attribution must be the kg_register source, got %+v", doc.Meta.License)
	}

	// 304 on If-None-Match.
	rr = get("/api/v1/kgs", map[string]string{"If-None-Match": `"` + u.UniverseHash + bboxRevTag(u) + `"`})
	if rr.Code != http.StatusNotModified {
		t.Fatalf("want 304, got %d", rr.Code)
	}
	// A pre-revision-2 tag (plain universe_hash) must NOT 304 — peers that
	// cached the parcel-derived bboxes have to refetch the widened ones.
	rr = get("/api/v1/kgs", map[string]string{"If-None-Match": `"` + u.UniverseHash + `"`})
	if rr.Code != http.StatusOK {
		t.Fatalf("old etag must refetch, got %d", rr.Code)
	}

	// fields=codes
	rr = get("/api/v1/kgs?fields=codes", nil)
	var codes struct {
		Data struct {
			Codes []string `json:"codes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &codes); err != nil || len(codes.Data.Codes) != 7850 {
		t.Fatalf("codes: %v %d", err, len(codes.Data.Codes))
	}

	// district exact name (Wien ≠ Wiener Neustadt) and district_code.
	var sub struct {
		Data struct {
			Count int             `json:"count"`
			KGs   []kgUniverseRow `json:"kgs"`
		} `json:"data"`
	}
	rr = get("/api/v1/kgs?district=Wien", nil)
	_ = json.Unmarshal(rr.Body.Bytes(), &sub)
	if sub.Data.Count == 0 {
		t.Fatalf("district=Wien empty")
	}
	for _, r := range sub.Data.KGs {
		if r.DistrictName != "Wien" {
			t.Fatalf("district exact match leaked %s", r.DistrictName)
		}
	}
	rr = get("/api/v1/kgs?district_code=616", nil)
	_ = json.Unmarshal(rr.Body.Bytes(), &sub)
	if sub.Data.Count == 0 || sub.Data.KGs[0].DistrictName != "Voitsberg" {
		t.Fatalf("district_code=616 → %d %+v", sub.Data.Count, sub.Data.KGs)
	}
	rr = get("/api/v1/kgs?codes=63349,1004,99999", nil)
	_ = json.Unmarshal(rr.Body.Bytes(), &sub)
	// request order is kept; unpadded "1004" is accepted; unknown codes are dropped
	if sub.Data.Count != 2 || sub.Data.KGs[0].KGCode != "63349" || sub.Data.KGs[1].KGCode != "01004" {
		t.Fatalf("codes → %+v", sub.Data.KGs)
	}
	if !isPublicPath("/api/v1/kgs") {
		t.Fatalf("/api/v1/kgs must be public")
	}
}

// TestKGUniversePaddingInvariance — the universe must treat "1004" and "01004"
// as the same KG everywhere: hash, loader, peer compare, ?codes= filter.
func TestKGUniversePaddingInvariance(t *testing.T) {
	// 1. hash: unpadded / padded / duplicated spellings → identical hash
	padded := KGUniverseHash([]string{"01004", "63349"})
	if KGUniverseHash([]string{"1004", "63349"}) != padded ||
		KGUniverseHash([]string{"63349", " 1004 ", "01004", ""}) != padded ||
		KGUniverseHash([]string{"4"}) != KGUniverseHash([]string{"00004"}) {
		t.Fatalf("KGUniverseHash is not padding-invariant")
	}
	// 2. a file carrying unpadded codes loads as canonical rows with the canonical hash
	u := &kgUniverse{KGs: []kgUniverseRow{
		{KGCode: "63349", KGName: "Köflach"},
		{KGCode: "1004", KGName: "Innere Stadt"},
		{KGCode: "01004"}, // duplicate spelling, poorer row → dropped
	}}
	dir := t.TempDir()
	path := dir + "/u.json.gz"
	u.UniverseHash = KGUniverseHash([]string{"1004", "63349"})
	if err := WriteKGUniverseFile(path, u); err != nil {
		t.Fatal(err)
	}
	got, err := LoadKGUniverseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.KGCount != 2 || got.KGs[0].KGCode != "01004" || got.KGs[0].KGName != "Innere Stadt" || got.KGs[1].KGCode != "63349" {
		t.Fatalf("loaded rows %+v", got.KGs)
	}
	if got.UniverseHash != padded || got.HashRecipe != kgUniverseRecipe {
		t.Fatalf("hash %s recipe %q", got.UniverseHash, got.HashRecipe)
	}
	// 3. request parameter: ?codes= is a KG-code list for the middleware
	if NormalizeRequestParam("codes", "1004,63349") != "01004,63349" {
		t.Fatalf("codes param not normalised")
	}

	// 4. handler: unpadded, whitespace-separated, duplicated codes resolve to one canonical row each
	full, err := LoadKGUniverseFile(kgUniversePath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	setKGUniverse(full, nil)
	for _, qs := range []string{"codes=1004", "codes=01004", "codes=1004,01004", "codes=%201004%20,+1004"} {
		req := httptest.NewRequest("GET", "/api/v1/kgs?"+qs, nil)
		rr := httptest.NewRecorder()
		normalizeKGParams(req, rr)
		handleV1KGs(rr, req)
		var sub struct {
			Data struct {
				Count int             `json:"count"`
				KGs   []kgUniverseRow `json:"kgs"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &sub); err != nil {
			t.Fatalf("%s: %v", qs, err)
		}
		if sub.Data.Count != 1 || sub.Data.KGs[0].KGCode != "01004" {
			t.Fatalf("%s → %+v", qs, sub.Data.KGs)
		}
	}
	req := httptest.NewRequest("GET", "/api/v1/kgs?codes=1004", nil)
	rr := httptest.NewRecorder()
	normalizeKGParams(req, rr)
	if rr.Header().Get(kgNormalizedHeader) != "true" {
		t.Fatalf("expected %s header for unpadded ?codes=", kgNormalizedHeader)
	}
}
