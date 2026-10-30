// api_tiers.go — three API tiers on one mux (docs/api-tiers.md).
//
//	PUBLIC   /api/v1/…        tokenless overlays and the NE read API, keyed by
//	                          point / bbox / admin code (publicExactPaths,
//	                          publicPrefixPaths). The only advertised surface.
//	CONTRIB  /contrib/…       NE reports, manifest, revisit and observed-layer
//	                          upload for peers (contributorPathAllowed). Needs a
//	                          CONTRIBUTOR_TOKEN or a private token.
//	PRIVATE  <prefix>/…       everything the server has (NE ops, debug). The
//	                          prefix comes from UMFELD_PRIVATE_PREFIX (default
//	                          /k) and is never advertised: no token → the same
//	                          404 as an unknown route, never 401.
//
// Switches (environment, systemd EnvironmentFile):
//
//	UMFELD_PUBLIC_ONLY=1       enforce the public allow-list on /api/v1.
//	UMFELD_TOKEN=a,b,c         private-tier tokens (one per client so one can be
//	                           rotated alone). Unset → the private tier is closed.
//	UMFELD_TOKEN_NAMES=x,y,z   names parallel to UMFELD_TOKEN (request log, NE
//	                           attribution, NE_WRITER). Unset → token-1, token-2, …
//	CONTRIBUTOR_TOKEN=x,y      report-only peers (api_ne_reports.go).
//
// The tier layer is the OUTERMOST middleware: it strips the prefix before the
// cache / gzip / egress layers, so cacheablePath, setLicenseHeaders and the
// egress stats all see the path the handler sees. Handlers stay untouched; the
// only response rewriting is in the plain-text docs, where the example paths
// get the prefix so they can be pasted.
package main

import (
	"bytes"
	"crypto/subtle"
	"net/http"
	"os"
	"strings"
	"sync"
)

// privateTierPrefix — UMFELD_PRIVATE_PREFIX (default /k).
var privateTierPrefix = peers().PrivatePrefix

// tierHeader — internal request header carrying the resolved tier ("private" | "contrib").
const tierHeader = "X-Umfeld-Tier"

// contribTierPrefix — the CONTRIBUTE surface: NE epoch reports + manifest
// (contributorPathAllowed), nothing else. Same paths as the private tier, but
// this is the prefix peers are told about. CONTRIBUTOR_TOKEN or any
// UMFELD_TOKEN opens it; no token / other path → 404.
const contribTierPrefix = "/contrib"

// publicExactPaths — public tier allow-list, exact matches. Adding a route
// here is a licensing decision: it must not read the parcel index. Keep in
// sync with TestPublicAllowList and the public llm.txt (api_docs_public.go).
var publicExactPaths = map[string]bool{
	"/":                              true,
	"/llm.txt":                       true,
	"/api/v1/docs":                   true,
	"/api/v1/docs/llm.txt":           true,
	"/api/v1/docs/cache-client.py":   true,
	"/api/v1/docs/cache-client.js":   true,
	"/api/v1/docs/ne.md":             true, // NE contract in public vocabulary (api_docs_public_ne.go)
	"/api/v1/docs/ne-change.md":      true, // NE change protocol, same rendering
	"/api/v1/docs/stats-envelope.md": true, // stats-envelope/1: /ne/stats/unit + peers (api_ne_stats_unit.go)
	"/api/v1/docs/qgis-layer.md":     true, // cells as MVT / PMTiles for QGIS (ne_mvt.go)
	"/api/v1/license":                true,
	"/api/v1/cache/policy":           true,
	"/api/v1/feedback":               true,
	"/api/v1/context":                true, // one-call overlay bundle for a point (api_context.go)
	"/api/v1/lookup":                 true, // EDM register: gemeinde / plz / ortschaft / kg code+name, toponyms
	"/api/v1/kgs":                    true, // KG universe: codes, names, admin hierarchy, bbox (register keys; kg_universe.go)
	"/api/v1/kgs/bbox_changes":       true, // KGs whose bbox widened to the VGD envelope + slivers (kg_universe_bbox.go)
	"/api/v1/search/municipalities":  true, // Statistik Austria polygons
	"/api/v1/search/gadm":            true,
	"/api/v1/search/protected_area":  true, // WDPA sites
	"/api/v1/search/address_osm":     true, // OSM geocoding
	"/api/v1/addresses/search":       true, // BEV Adressregister (CC BY 4.0), glyph find box
	"/api/v1/osm/point":              true,
	"/api/v1/osm/geometry":           true,
	"/api/v1/natura2000/point":       true,
	"/api/v1/natura2000/search":      true,
	"/api/v1/legal/search":           true,
	"/api/v1/legal/stats":            true,
	"/api/v1/land_prices/predict":    true, // caller supplies the parcel features
	"/api/v1/land_prices/point":      true, // predict + Gemeinde resolved from lon/lat
	"/api/v1/toponyms":               true,
	"/api/v1/toponyms/search":        true,
	"/api/v1/toponyms/near":          true,
	"/api/v1/toponyms/stats":         true,
}

// publicPrefixPaths — allow-listed subtrees.
var publicPrefixPaths = []string{
	"/api/v1/feedback/",
	"/api/v1/natura2000/site/",
	"/api/v1/legal/kg/", // RIS refs per KG code (the code is an admin key, the content is law text)
	"/api/v1/land_prices/gemeinde/",
	"/api/v1/land_prices/bezirk/",
	"/api/v1/land_prices/bundesland/",
	"/api/v1/toponyms/",
	"/api/v1/ne/", // NE cells, H3-keyed reads (api_ne_public.go); writes excluded below
}

// isPublicPath reports whether p is served on the public tier.
func isPublicPath(p string) bool {
	if publicExactPaths[p] || p == "/api/v1/ne" {
		return true
	}
	// NE: the reference-package share and the peer write paths stay private
	// (docs/ne-public-api.md §7). The handler enforces the same split.
	if strings.HasPrefix(p, "/api/v1/ne/") &&
		(p == "/api/v1/ne/reports" || p == "/api/v1/ne/revisit" || strings.HasSuffix(p, "/obs") || strings.HasSuffix(p, "/report") || strings.HasSuffix(p, "/chunks") || strings.HasSuffix(p, "/declared")) {
		return false
	}
	for _, pre := range publicPrefixPaths {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	// /api/v1/kg/{kg}/toponyms is the only public /kg/ sub-resource.
	if strings.HasPrefix(p, "/api/v1/kg/") && strings.HasSuffix(p, "/toponyms") {
		return true
	}
	return false
}

var (
	tierOnce      sync.Once
	tierPublic    bool
	tierTokens    [][]byte
	tierNames     []string // UMFELD_TOKEN_NAMES, parallel to tierTokens
	tierTokensSet bool
)

// tierDefaultNames — used when UMFELD_TOKEN_NAMES is unset: tokens are then
// named token-1, token-2, … (tokenName).
var tierDefaultNames []string

func loadTierConfig() {
	tierOnce.Do(func() {
		v := strings.ToLower(strings.TrimSpace(os.Getenv("UMFELD_PUBLIC_ONLY")))
		tierPublic = v == "1" || v == "true" || v == "yes"
		for _, t := range strings.Split(os.Getenv("UMFELD_TOKEN"), ",") {
			if t = strings.TrimSpace(t); len(t) >= 16 {
				tierTokens = append(tierTokens, []byte(t))
			}
		}
		tierTokensSet = len(tierTokens) > 0
		if v := strings.TrimSpace(os.Getenv("UMFELD_TOKEN_NAMES")); v != "" {
			for _, n := range strings.Split(v, ",") {
				tierNames = append(tierNames, strings.TrimSpace(n))
			}
		} else {
			tierNames = tierDefaultNames
		}
	})
}

// publicOnly — is the public allow-list enforced on /api/v1?
func publicOnly() bool { loadTierConfig(); return tierPublic }

// privateTierOpen — is any token configured?
func privateTierOpen() bool { loadTierConfig(); return tierTokensSet }

// requestToken extracts the bearer token: Authorization: Bearer, X-API-Key,
// or ?token= (for download-style URLs).
func requestToken(r *http.Request) string {
	if a := r.Header.Get("Authorization"); len(a) > 7 && strings.EqualFold(a[:7], "Bearer ") {
		return strings.TrimSpace(a[7:])
	}
	if k := r.Header.Get("X-API-Key"); k != "" {
		return k
	}
	return r.URL.Query().Get("token")
}

// tokenIndex returns the position of tok in the configured token list, -1 if
// none (constant-time per candidate, like tokenValid).
func tokenIndex(tok string) int {
	loadTierConfig()
	if tok == "" {
		return -1
	}
	b := []byte(tok)
	for i, t := range tierTokens {
		if len(t) == len(b) && subtle.ConstantTimeCompare(t, b) == 1 {
			return i
		}
	}
	return -1
}

func tokenValid(tok string) bool {
	loadTierConfig()
	if tok == "" {
		return false
	}
	b := []byte(tok)
	for _, t := range tierTokens {
		if len(t) == len(b) && subtle.ConstantTimeCompare(t, b) == 1 {
			return true
		}
	}
	return false
}

// tierNotFound — the one body every refused request gets, identical to an
// unknown route so the private prefix is not discoverable.
func tierNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"error":"not_found","docs":"/api/v1/docs/llm.txt"}` + "\n"))
}

// tierMiddleware — see file header.
func tierMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == contribTierPrefix || strings.HasPrefix(p, contribTierPrefix+"/") {
			inner := strings.TrimPrefix(p, contribTierPrefix)
			tok := requestToken(r)
			if !contributorPathAllowed(inner) || (contributorIndex(tok) < 0 && !(privateTierOpen() && tokenValid(tok))) {
				tierNotFound(w)
				return
			}
			w.Header().Set("X-Robots-Tag", "noindex, nofollow")
			w.Header().Add("Vary", "Authorization")
			r2 := r.Clone(r.Context())
			r2.URL.Path = inner
			if r2.URL.RawPath != "" {
				r2.URL.RawPath = strings.TrimPrefix(r2.URL.RawPath, contribTierPrefix)
			}
			r2.Header.Set(tierHeader, "contrib")
			next.ServeHTTP(w, r2)
			return
		}
		if p == privateTierPrefix || strings.HasPrefix(p, privateTierPrefix+"/") {
			if !privateTierOpen() || !tokenValid(requestToken(r)) {
				// CONTRIBUTOR_TOKEN (api_ne_reports.go): NE report paths only,
				// same 404 everywhere else — never the cadastre surface.
				if contributorIndex(requestToken(r)) < 0 || !contributorPathAllowed(strings.TrimPrefix(p, privateTierPrefix)) {
					tierNotFound(w)
					return
				}
			}
			w.Header().Set("X-Robots-Tag", "noindex, nofollow")
			w.Header().Add("Vary", "Authorization")
			r2 := r.Clone(r.Context())
			r2.URL.Path = strings.TrimPrefix(p, privateTierPrefix)
			if r2.URL.Path == "" {
				r2.URL.Path = "/"
			}
			if r2.URL.RawPath != "" {
				r2.URL.RawPath = strings.TrimPrefix(r2.URL.RawPath, privateTierPrefix)
			}
			r2.Header.Set(tierHeader, "private")
			if isDocsTextPath(r2.URL.Path) {
				// Rewrite example paths in the plain-text docs so they paste.
				pw := &prefixRewriteWriter{ResponseWriter: w}
				next.ServeHTTP(pw, r2)
				pw.flush()
				return
			}
			next.ServeHTTP(w, r2)
			return
		}
		if publicOnly() && !isPublicPath(p) {
			tierNotFound(w)
			return
		}
		// The tier header is set by this middleware only; never trust a
		// client-supplied one on the public path.
		r.Header.Del(tierHeader)
		next.ServeHTTP(w, r)
	})
}

// requestPeerTag — "public" | "private:<name>" | "contrib:<name>" for the
// request log. Name = tokenName (api_ne_reports.go): UMFELD_TOKEN_NAMES,
// contributor tokens by CONTRIBUTOR_TOKEN_NAMES.
func requestPeerTag(r *http.Request) string {
	tier := r.Header.Get(tierHeader)
	if tier != "private" && tier != "contrib" {
		return "public"
	}
	_, name := reporterID(r)
	if name == "" {
		name = "?"
	}
	return tier + ":" + name
}

// isPrivateTier — handlers that render differently per tier (docs, licence,
// cache policy, land_prices areas, osm/geometry, toponyms/{id}, root) ask this.
// Reached via the private prefix → private. On a public path → public
// rendering even while UMFELD_PUBLIC_ONLY=0.
func isPrivateTier(r *http.Request) bool {
	if t := r.Header.Get(tierHeader); t == "private" || t == "contrib" {
		// contrib reaches only contributorPathAllowed paths (tierMiddleware),
		// rendered exactly as under the private prefix.
		return true
	}
	return !publicOnly() && !isPublicPath(r.URL.Path)
}

// tierPrefixFor — prefix to put in front of API paths echoed back to this
// caller ("/contrib" for contributors, the private prefix, "" public).
func tierPrefixFor(r *http.Request) string {
	switch r.Header.Get(tierHeader) {
	case "contrib":
		return contribTierPrefix
	case "private":
		return privateTierPrefix
	}
	return ""
}

func isDocsTextPath(p string) bool {
	return p == "/llm.txt" || p == "/api/v1/docs/llm.txt" || p == "/api/v1/docs/examples" || p == "/api/v1/docs/ne.md" || p == "/api/v1/docs/ne-change.md" ||
		p == "/api/v1/docs/cache-client.py" || p == "/api/v1/docs/cache-client.js"
}

// prefixRewriteWriter buffers a docs body and prepends the private prefix to every absolute
// API path ("/api/", "/llm/", "/llm.txt", "/debug/") that is not already
// prefixed. Docs only — JSON bodies are never rewritten.
type prefixRewriteWriter struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (p *prefixRewriteWriter) WriteHeader(code int)        { p.status = code }
func (p *prefixRewriteWriter) Write(b []byte) (int, error) { return p.buf.Write(b) }
func (p *prefixRewriteWriter) flush() {
	body := rewriteDocPrefixes(p.buf.Bytes())
	h := p.ResponseWriter.Header()
	h.Del("Content-Length")
	if p.status == 0 {
		p.status = http.StatusOK
	}
	p.ResponseWriter.WriteHeader(p.status)
	_, _ = p.ResponseWriter.Write(body)
}

func rewriteDocPrefixes(b []byte) []byte {
	var out bytes.Buffer
	out.Grow(len(b) + len(b)/32)
	prefixes := [][]byte{[]byte("/api/"), []byte("/llm/"), []byte("/llm.txt"), []byte("/debug/")}
	for i := 0; i < len(b); i++ {
		if b[i] == '/' && !bytes.HasSuffix(b[:i], []byte(privateTierPrefix)) && !bytes.HasSuffix(b[:i], []byte(contribTierPrefix)) {
			for _, pre := range prefixes {
				if bytes.HasPrefix(b[i:], pre) {
					out.WriteString(privateTierPrefix)
					break
				}
			}
		}
		out.WriteByte(b[i])
	}
	return out.Bytes()
}

// setTierConfigForTest overrides the env-derived config (tests only).
func setTierConfigForTest(public bool, tokens ...string) {
	tierOnce.Do(func() {})
	tierPublic = public
	tierTokens = nil
	// slot 0 = the NE writer, slot 1 = the operator (the layout the NE tests assume)
	tierNames = []string{neWriterName(), "ops"}
	for _, t := range tokens {
		tierTokens = append(tierTokens, []byte(t))
	}
	tierTokensSet = len(tierTokens) > 0
}
