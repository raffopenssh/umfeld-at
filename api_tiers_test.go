package main

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"
)

// publicAllowListGolden — the complete public surface. A new route becomes
// public only by being added HERE as well as in api_tiers.go.
var publicAllowListGolden = []string{
	"/", "/llm.txt",
	"/impressum", "/imprint", "/datenschutz", "/privacy", "/lizenzen", "/licenses", "/license",
	"/api/v1/docs", "/api/v1/docs/llm.txt", "/api/v1/docs/cache-client.py", "/api/v1/docs/cache-client.js",
	"/api/v1/license", "/api/v1/cache/policy", "/api/v1/feedback", "/api/v1/feedback/12",
	"/api/v1/context", "/api/v1/lookup", "/api/v1/kgs", "/api/v1/kgs/bbox_changes",
	"/api/v1/search/municipalities", "/api/v1/search/gadm", "/api/v1/search/protected_area", "/api/v1/search/address_osm", "/api/v1/addresses/search",
	"/api/v1/osm/point", "/api/v1/osm/geometry",
	"/api/v1/natura2000/point", "/api/v1/natura2000/search", "/api/v1/natura2000/site/AT1205A00",
	"/api/v1/legal/search", "/api/v1/legal/stats", "/api/v1/legal/kg/49006",
	"/api/v1/land_prices/predict", "/api/v1/land_prices/point",
	"/api/v1/land_prices/gemeinde/61630", "/api/v1/land_prices/bezirk/616", "/api/v1/land_prices/bundesland/6",
	"/api/v1/toponyms", "/api/v1/toponyms/search", "/api/v1/toponyms/near", "/api/v1/toponyms/stats", "/api/v1/toponyms/{32B3}",
	"/api/v1/kg/63349/toponyms",
	"/api/v1/docs/ne.md", "/api/v1/docs/ne-change.md", "/api/v1/ne", "/api/v1/ne/manifest", "/api/v1/ne/glyph", "/api/v1/ne/glyph/kgs", "/api/v1/ne/glyph/tile", "/api/v1/ne/glyph/cell", "/api/v1/ne/glyph/kg", "/api/v1/ne/glyph/sub", "/api/v1/ne/glyph/fidx", "/api/v1/ne/glyph/filter.js", "/api/v1/ne/filter", "/api/v1/ne/filter/fields", "/api/v1/ne/stats/unit", "/api/v1/docs/stats-envelope.md", "/api/v1/docs/qgis-layer.md", "/api/v1/ne/glyph/passes", "/api/v1/ne/glyph/soak.js", "/api/v1/ne/63330", "/api/v1/ne/63330/head", "/api/v1/ne/dict/lu-1",
	"/api/v1/ne/chunk/891e108cc93ffff", "/api/v1/ne/cell/8c1e108cc922dff", "/api/v1/ne/cells",
}

// cadastrePathsMustBePrivate — anything that reads the parcel index.
var cadastrePathsMustBePrivate = []string{
	"/api/v1/spatial/parcels", "/api/v1/spatial/footprints", "/api/v1/spatial/landuse", "/api/v1/spatial/bbox",
	"/api/v1/spatial/point", "/api/v1/spatial/points", "/api/v1/spatial/polygon", "/api/v1/spatial/kgs",
	"/api/v1/search/parcel", "/api/v1/search/ez", "/api/v1/search/gnr", "/api/v1/search/kg", "/api/v1/search/text",
	"/api/v1/search/feature", "/api/v1/search/address", "/api/v1/search/district",
	"/api/v1/query", "/api/v1/query/protected_area", "/api/v1/stats",
	"/api/v1/parcels/63349-348/6/geometry", "/api/v1/parcels/geometry/batch", "/api/v1/footprints/geometry/batch",
	"/api/v1/export/geojson", "/api/v1/export/gpkg", "/api/v1/kg_versions", "/api/v1/landuse/codes", "/api/v1/landuse/distribution",
	"/api/v1/osm/parcel/63349-348/6", "/api/v1/osm/stats",
	"/api/v1/natura2000/parcel/12358/482", "/api/v1/natura2000/kg/63349", "/api/v1/natura2000/kgs",
	"/api/v1/natura2000/site_parcels/AT1205A00", "/api/v1/natura2000/stats",
	"/api/v1/legal/parcel/49006/213/1",
	"/api/v1/land_prices/parcel/01002-1219", "/api/v1/land_prices/batch", "/api/v1/land_prices/validate", "/api/v1/land_prices/stats",
	"/api/v1/kg/63349", "/api/v1/kg/63349/parcels",
	"/api/v1/prewarm", "/api/v1/mirror/status", "/api/inspire/process/63349",
	"/api/v1/debug/inflight", "/debug/pprof/", "/llm/kg/63349", "/llm/kgs", "/llm/manifest.json", "/api/search/parcels",
	"/api/v1/buildings/repair", "/api/v1/docs/examples",
	"/api/v1/ne/63330/obs", "/api/v1/ne/63330/report",
}

func TestPublicAllowList(t *testing.T) {
	for _, p := range publicAllowListGolden {
		if !isPublicPath(p) {
			t.Errorf("expected public: %s", p)
		}
	}
	for _, p := range cadastrePathsMustBePrivate {
		if isPublicPath(p) {
			t.Errorf("cadastre path leaked to public tier: %s", p)
		}
	}
	// Every exact entry in the code must be in the golden list (no silent additions).
	var missing []string
	for p := range publicExactPaths {
		found := false
		for _, g := range publicAllowListGolden {
			if g == p {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, p)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("publicExactPaths not in golden list: %v", missing)
	}
}

func tierTestHandler() http.Handler {
	return tierMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Seen-Path", r.URL.Path)
		w.Header().Set("X-Seen-Tier", r.Header.Get("X-Umfeld-Tier"))
		if isDocsTextPath(r.URL.Path) {
			_, _ = w.Write([]byte("see /api/v1/search/parcel and https://x.example/llm/kg/1 and /k/api/v1/already"))
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
}

func TestTierPublicOnlyBlocksCadastre(t *testing.T) {
	setTierConfigForTest(true, "0123456789abcdef0123")
	h := tierTestHandler()
	for _, p := range cadastrePathsMustBePrivate {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", p, nil))
		if rr.Code != 404 {
			t.Errorf("%s: want 404 on public tier, got %d", p, rr.Code)
		}
	}
	for _, p := range publicAllowListGolden {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", p, nil))
		if rr.Code != 200 {
			t.Errorf("%s: want 200 on public tier, got %d", p, rr.Code)
		}
	}
}

func TestTierPrivatePrefix(t *testing.T) {
	const tok = "0123456789abcdef0123"
	setTierConfigForTest(true, tok)
	h := tierTestHandler()

	// no token → identical 404 body as an unknown route
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/k/api/v1/spatial/parcels?west=1", nil))
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, httptest.NewRequest("GET", "/api/v1/nonexistent", nil))
	if rr.Code != 404 || rr.Body.String() != rr2.Body.String() {
		t.Fatalf("no-token: want indistinguishable 404, got %d %q vs %q", rr.Code, rr.Body.String(), rr2.Body.String())
	}
	// wrong token
	rr = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/k/api/v1/spatial/parcels", nil)
	req.Header.Set("Authorization", "Bearer wrongwrongwrongwrong")
	h.ServeHTTP(rr, req)
	if rr.Code != 404 {
		t.Fatalf("wrong token: want 404, got %d", rr.Code)
	}
	// bearer
	for _, mk := range []func() *http.Request{
		func() *http.Request {
			r := httptest.NewRequest("GET", "/k/api/v1/spatial/parcels?west=1", nil)
			r.Header.Set("Authorization", "Bearer "+tok)
			return r
		},
		func() *http.Request {
			return httptest.NewRequest("GET", "/k/api/v1/spatial/parcels?west=1&token="+tok, nil)
		},
		func() *http.Request {
			r := httptest.NewRequest("GET", "/k/api/v1/spatial/parcels?west=1", nil)
			r.Header.Set("X-API-Key", tok)
			return r
		},
	} {
		rr = httptest.NewRecorder()
		h.ServeHTTP(rr, mk())
		if rr.Code != 200 || rr.Header().Get("X-Seen-Path") != "/api/v1/spatial/parcels" || rr.Header().Get("X-Seen-Tier") != "private" {
			t.Fatalf("token: got %d path=%q tier=%q", rr.Code, rr.Header().Get("X-Seen-Path"), rr.Header().Get("X-Seen-Tier"))
		}
		if rr.Header().Get("X-Robots-Tag") == "" {
			t.Fatalf("private tier must be noindex")
		}
	}
	// private tier closed when no token configured
	setTierConfigForTest(true)
	rr = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/k/api/v1/license", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(rr, req)
	if rr.Code != 404 {
		t.Fatalf("closed private tier: want 404, got %d", rr.Code)
	}
	// rollout mode: public_only off → old paths still work, no tier header
	setTierConfigForTest(false, tok)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/spatial/parcels", nil))
	if rr.Code != 200 {
		t.Fatalf("rollout mode: want 200, got %d", rr.Code)
	}
}

func TestTierDocsRewrite(t *testing.T) {
	const tok = "0123456789abcdef0123"
	setTierConfigForTest(true, tok)
	h := tierTestHandler()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/k/api/v1/docs/llm.txt", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(rr, req)
	body := rr.Body.String()
	want := "see /k/api/v1/search/parcel and https://x.example/k/llm/kg/1 and /k/api/v1/already"
	if body != want {
		t.Fatalf("rewrite:\n got %q\nwant %q", body, want)
	}
	if strings.Contains(string(rewriteDocPrefixes([]byte("/k/api/x"))), "/k/k/") {
		t.Fatal("double prefix")
	}
}

func TestParsePointParamsLanduseAreas(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/v1/context?lon=15.08&lat=47.06&area_sqm=812&landuse_areas=41:140,52:672,bad,53:x&footprint_area_sqm=140&kg=1503", nil)
	p, msg := parsePointParams(r)
	if msg != "" {
		t.Fatal(msg)
	}
	if p.KG != "01503" {
		t.Fatalf("kg not padded: %q", p.KG)
	}
	in := p.Pricing
	if in.Split == nil || in.Split.CoveredSqm != 812 || in.Split.DominantCode != "52" || in.LanduseCodes != "41,52" {
		t.Fatalf("split: %+v codes=%q", in.Split, in.LanduseCodes)
	}
	if in.FootprintCt != 1 || in.BuildingCount != 1 {
		t.Fatalf("footprint_area_sqm should imply one footprint/building: %+v", in)
	}
	if _, msg := parsePointParams(httptest.NewRequest("GET", "/api/v1/context?lon=2&lat=47", nil)); msg == "" {
		t.Fatal("outside Austria must be rejected")
	}
}

func TestPublicLicenseHasNoKataster(t *testing.T) {
	lic := publicLicense()
	for _, s := range lic["sources"].([]dataSourceLicense) {
		if strings.Contains(strings.ToLower(s.Name), "parcel") {
			t.Fatalf("public licence must not list a parcel dataset: %s", s.ID)
		}
	}
	for _, p := range []string{"/api/v1/osm/point", "/api/v1/natura2000/point", "/api/v1/legal/kg/1", "/api/v1/land_prices/point", "/api/v1/toponyms/near", "/api/v1/search/protected_area"} {
		m := AttributionMetaFor(p, false)
		if strings.Contains(m.Attribution, "Eich- und Vermessungswesen") && !strings.Contains(m.Attribution, "Namen") {
			t.Errorf("%s attributed to the Kataster: %q", p, m.Attribution)
		}
	}
}


// /contrib/… — the advertised contribute prefix: NE report paths only,
// CONTRIBUTOR_TOKEN or UMFELD_TOKEN, 404 (indistinguishable) otherwise.
func TestTierContribPrefix(t *testing.T) {
	const tok = "0123456789abcdef0123"
	const ctok = "contribcontribcontrib01"
	setTierConfigForTest(true, tok)
	setContributorConfigForTest([]string{ctok}, []string{"peer"})
	defer setContributorConfigForTest(nil, nil)
	h := tierTestHandler()
	get := func(path, bearer string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		r := httptest.NewRequest("GET", path, nil)
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		h.ServeHTTP(rr, r)
		return rr
	}
	unknown := get("/api/v1/nonexistent", "").Body.String()
	for _, c := range []struct {
		path, tok string
		want      int
	}{
		{"/contrib/api/v1/ne/63330/report", "", 404},
		{"/contrib/api/v1/ne/63330/report", "wrongwrongwrongwrong", 404},
		{"/contrib/api/v1/ne/63330/report", ctok, 200},
		{"/contrib/api/v1/ne/63330/report", tok, 200},
		{"/contrib/api/v1/ne/manifest", ctok, 200},
		{"/contrib/api/v1/ne/reports", ctok, 200},
		{"/contrib/api/v1/spatial/parcels?west=1", ctok, 404}, // never the cadastre
		{"/contrib/api/v1/spatial/parcels?west=1", tok, 404},  // not even with the private token
		{"/contrib/api/v1/search/parcel?id=63349-348/6", tok, 404},
		{"/k/api/v1/ne/63330/report", ctok, 200}, // legacy alias keeps working
	} {
		rr := get(c.path, c.tok)
		if rr.Code != c.want {
			t.Fatalf("%s tok=%q: want %d got %d", c.path, c.tok, c.want, rr.Code)
		}
		if c.want == 404 && rr.Body.String() != unknown {
			t.Fatalf("%s: 404 body must be indistinguishable from unknown route", c.path)
		}
		if c.want == 200 {
			if strings.HasPrefix(c.path, "/contrib") && rr.Header().Get("X-Seen-Tier") != "contrib" {
				t.Fatalf("%s: tier header %q", c.path, rr.Header().Get("X-Seen-Tier"))
			}
			if strings.HasPrefix(rr.Header().Get("X-Seen-Path"), "/contrib") || rr.Header().Get("X-Robots-Tag") == "" {
				t.Fatalf("%s: path %q robots %q", c.path, rr.Header().Get("X-Seen-Path"), rr.Header().Get("X-Robots-Tag"))
			}
		}
	}
	// contrib tier renders like the private tier (same handlers, same answers)
	r := httptest.NewRequest("GET", "/api/v1/ne/manifest", nil)
	r.Header.Set("X-Umfeld-Tier", "contrib")
	if !isPrivateTier(r) {
		t.Fatal("contrib must render as private tier")
	}
}

// The NE writer gate follows the token NAME (neWriterName = the observed-layer peer),
// not the UMFELD_TOKEN slot: srtm holding only a contributor token can still
// post /contrib/…/{kg}/obs, every other contributor and a non-srtm private
// slot 0 get the tier 404. (srtm's private token was revoked 2026-10-23.)
func TestNEWriterByName(t *testing.T) {
	const ops = "fedcba9876543210fedc"
	const srtmC, mapTok = "obsc-contrib-token-0123456789", "mapc-contrib-token-01234567"
	setTierConfigForTest(true, ops)
	tierNames = []string{"ops"} // UMFELD_TOKEN_NAMES=ops
	defer func() { tierNames = tierDefaultNames }()
	setContributorConfigForTest([]string{mapTok, srtmC}, []string{"map-peer", "obs-peer"})
	defer setContributorConfigForTest(nil, nil)
	neReportRateMu.Lock()
	neReportBuckets = map[int]*neBucketT{} // other tests tighten the rate; start fresh
	neReportRateMu.Unlock()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/ne/", handleNE)
	h := tierMiddleware(mux)
	call := func(method, path, tok string) int {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader("junk"))
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		h.ServeHTTP(rr, req)
		return rr.Code
	}
	if n := tokenName(0); n != "ops" {
		t.Fatalf("slot 0 name: %q", n)
	}
	for _, c := range []struct {
		method, path, tok string
		want              int
	}{
		{"POST", "/contrib/api/v1/ne/63330/obs", srtmC, 400},   // reaches the handler (bad body), not 404
		{"POST", "/contrib/api/v1/ne/63330/obs", mapTok, 404}, // contributor, wrong name
		{"POST", "/contrib/api/v1/ne/63330/obs", ops, 404},     // private slot 0 is ops now
		{"POST", "/k/api/v1/ne/63330/obs", ops, 404},
		{"POST", "/api/v1/ne/63330/obs", srtmC, 404},                     // never on the public tier
		{"POST", "/contrib/api/v1/ne/63330/report", mapTok, 400},        // reports: any contributor (bad body → 400)
		{"GET", "/contrib/api/v1/ne/../../debug/inflight", mapTok, 404}, // no traversal through the allow-list
	} {
		if got := call(c.method, c.path, c.tok); got != c.want {
			t.Fatalf("%s %s tok=%.6s…: want %d got %d", c.method, c.path, c.tok, c.want, got)
		}
	}
}

// Per-token daily write budget: over budget → 429, bucket refills with time.
func TestNEWriteBudget(t *testing.T) {
	neWriteBudgetMu.Lock()
	neWriteBudgets = map[int]*neBucketT{}
	neWriteBudgetMu.Unlock()
	now := time.Now()
	ok, _ := neWriteBudgetAllow(7, int(neWriteBudgetBytes)-10, now)
	if !ok {
		t.Fatal("first write within budget must pass")
	}
	ok, wait := neWriteBudgetAllow(7, 1<<20, now)
	if ok || wait < 1 {
		t.Fatalf("over budget must fail with a wait: ok=%v wait=%d", ok, wait)
	}
	if ok, _ := neWriteBudgetAllow(8, 1<<20, now); !ok {
		t.Fatal("budget is per token")
	}
	if ok, _ := neWriteBudgetAllow(7, 1<<20, now.Add(24*time.Hour)); !ok {
		t.Fatal("a day later the bucket is full again")
	}
}
