package main

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestNoExternalAssets — every HTML page (and the CSS/JS inlined in it) we
// serve must load NOTHING from another origin: no web fonts (Google Fonts or
// otherwise), no external stylesheets, scripts, images or map tiles. System
// font stack only. The Datenschutz page promises exactly this ("keine
// Ressourcen von Drittanbietern"), so this test is what keeps that sentence
// true. Runtime fetch() targets in the glyph JS are pinned to an explicit
// allow-list (api_legal_pages.go §4 names them) so a new third-party host
// cannot slip in unnoticed.
func TestNoExternalAssets(t *testing.T) {
	pages := servedHTMLPages(t)
	if len(pages) < 8 {
		t.Fatalf("expected ≥ 8 served pages, got %d", len(pages))
	}

	// Resource-loading constructs that reach another origin. Anchors (<a href>)
	// are deliberately NOT matched: links are fine, loads are not.
	bad := []*regexp.Regexp{
		regexp.MustCompile(`(?i)<link\b[^>]*\bhref\s*=\s*["']?\s*(https?:)?//`),
		regexp.MustCompile(`(?i)<script\b[^>]*\bsrc\s*=\s*["']?\s*(https?:)?//`),
		regexp.MustCompile(`(?i)<(img|iframe|embed|object|source|video|audio|track)\b[^>]*\b(src|data|srcset)\s*=\s*["']?\s*(https?:)?//`),
		regexp.MustCompile(`(?i)@import\b`),
		regexp.MustCompile(`(?i)@font-face\b`),
		regexp.MustCompile(`(?i)url\(\s*["']?\s*(https?:)?//`),
		regexp.MustCompile(`(?i)fonts\.googleapis\.com|fonts\.gstatic\.com|fonts\.bunny\.net|use\.typekit\.net|cdnjs\.cloudflare\.com|unpkg\.com|jsdelivr\.net|cdn\.`),
		regexp.MustCompile(`(?i)<link\b[^>]*rel\s*=\s*["']?(preconnect|dns-prefetch|preload|modulepreload|stylesheet)\b`),
	}
	for name, body := range pages {
		for _, re := range bad {
			if m := re.FindString(body); m != "" {
				t.Errorf("%s loads an external resource: %q", name, m)
			}
		}
	}

	// Every absolute URL on a page must point at a host on this list. Anchors
	// are caught too, which is fine: anything new is added consciously.
	allowedHosts := map[string]bool{
		"obs.example":          true, // observed layer + trees + flags, from the browser (Datenschutz §4)
		"obs.example:8100":     true,
		"sub.example": true, // linked only; data is proxied server-side (ne_glyph_sub.go)
		"mapsneu.wien.gv.at":             true, // basemap.at orthophoto tiles, from the browser only with the photo layer on (Datenschutz §4)
		"basemap.at":                     true, // linked only (attribution)
		"umfeld.example":              true, // self (share links / canonical)
		"localhost:8000":                 true, // self, dev (soak script comment)
		"github.com":                     true,
		"creativecommons.org":            true,
		"opensource.org":                 true,
		"www.openstreetmap.org":          true,
		"www.bev.gv.at":                  true,
		"kataster.bev.gv.at":             true,
		"data.bev.gv.at":                 true,
		"www.statistik.at":               true,
		"www.eea.europa.eu":              true,
		"bio.discomap.eea.europa.eu":     true,
		"data.bka.gv.at":                 true,
		"www.protectedplanet.net":        true,
		"gadm.org":                       true,
		"download.geofabrik.de":          true,
		"nominatim.openstreetmap.org":    true,
		"h3geo.org":                      true,
		"exe.dev":                        true,
		"www.dsb.gv.at":                  true,
		"www.w3.org":                     true, // SVG / XHTML namespace strings, never fetched
	}
	hostRe := regexp.MustCompile(`https?://([a-zA-Z0-9.-]+(?::[0-9]+)?)`)
	for name, body := range pages {
		seen := map[string]bool{}
		for _, m := range hostRe.FindAllStringSubmatch(body, -1) {
			h := m[1]
			if allowedHosts[h] || seen[h] {
				continue
			}
			seen[h] = true
			t.Errorf("%s references host %q not on the external-host allow-list (add consciously + update /datenschutz §4)", name, h)
		}
	}

	// The browser-side fetch() targets are exactly the observed-layer peer (+ self).
	// If this list grows, /datenschutz §4 must name the new host.
	fetchRe := regexp.MustCompile(`fetch\(\s*([A-Za-z_$][A-Za-z0-9_$]*|'[^']*'|"[^"]*")`)
	varRe := regexp.MustCompile(`\b(OBS|KGC|SRTM|APEX)\s*=\s*(?:SRTM\s*\+\s*)?'([^']*)'`)
	landing := pages["/"]
	remoteVars := map[string]string{}
	for _, m := range varRe.FindAllStringSubmatch(landing, -1) {
		remoteVars[m[1]] = m[2]
	}
	if len(remoteVars) == 0 {
		t.Errorf("landing glyph: expected the the observed-layer peer endpoint vars (OBS/KGC/SRTM) — did they move? update this test and /datenschutz §4")
	}
	var remote []string
	for _, m := range fetchRe.FindAllStringSubmatch(landing, -1) {
		tgt := m[1]
		if strings.HasPrefix(tgt, "'") || strings.HasPrefix(tgt, "\"") {
			if strings.Contains(tgt, "://") {
				remote = append(remote, tgt)
			}
			continue
		}
		if v, ok := remoteVars[tgt]; ok && strings.Contains(v, "://") {
			remote = append(remote, v)
		}
	}
	sort.Strings(remote)
	for _, u := range remote {
		if h := hostRe.FindStringSubmatch(u); h == nil || !strings.HasPrefix(h[1], "obs.example") {
			t.Errorf("landing glyph fetches a host other than the observed-layer peer at runtime: %s", u)
		}
	}

	// And the Datenschutz text must name what the browser really does.
	ds := pages["/datenschutz"]
	for _, want := range []string{"obs.example", "localStorage", "keine Cookies", "Die IP-Adresse wird nicht protokolliert", "neg-rep", "keine externen Schriften"} {
		if !strings.Contains(ds, want) {
			t.Errorf("/datenschutz no longer mentions %q — keep it in step with what the pages do", want)
		}
	}
	// The "only on click" sentence must not cover the observed-layer peer, which the
	// glyph contacts live.
	for _, para := range strings.Split(ds, "<p>") {
		if strings.Contains(para, "erst bei Klick") && strings.Contains(para, "obs-peer") {
			t.Errorf("/datenschutz claims the observed-layer peer is contacted only on click; the glyph fetches it live")
		}
	}
}

// servedHTMLPages — every HTML (and inlined CSS/JS) document the binary can
// emit, rendered in-process: legal pages, landing glyph, public + private
// docs, examples, licence page, and the soak script the landing imports.
func servedHTMLPages(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	get := func(name string, h http.HandlerFunc, path string, private bool) {
		req := httptest.NewRequest("GET", path, nil)
		if private {
			req.Header.Set("X-Umfeld-Tier", "private")
		}
		rr := httptest.NewRecorder()
		h(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: status %d", name, rr.Code)
		}
		if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Fatalf("%s: content-type %q", name, ct)
		}
		out[name] = rr.Body.String()
	}
	legal := http.NewServeMux()
	handleLegalPages(legal)
	for _, p := range []string{"/impressum", "/imprint", "/datenschutz", "/privacy"} {
		get(p, legal.ServeHTTP, p, false)
	}
	get("/", handlePublicRoot, "/", false)
	get("/api/v1/docs", handleV1Docs, "/api/v1/docs", false)
	get("/k/api/v1/docs", handleV1Docs, "/api/v1/docs", true)
	get("/api/v1/license?format=html", handleV1License, "/api/v1/license?format=html", false)
	out["ne_glyph_soak.js"] = string(neGlyphSoakJS)
	return out
}
