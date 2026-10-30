// api.go — Core API v1 router, middleware, and response helpers.
//
// Provides:
//   - RegisterAPIv1: wires all /api/v1/* route groups onto an *http.ServeMux
//   - Middleware: CORS (permissive for research use), request logging, format negotiation
//   - APIContext: parses common query parameters (format, limit, offset, fields, callback)
//   - Meta: pagination/stats metadata struct
//   - Response helpers: apiResponse, apiError, apiGeoJSONResponse, apiCSVResponse
//
// Route handlers for each group are implemented in separate files; this file
// declares the handler function signatures as package-level vars so that each
// route group file can assign its implementation at init() time.
package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ═══════════════════════════════════════════════════════════════════════════════
// Route handler stubs — assigned by init() in the respective route-group files.
// Until assigned, each returns 501 Not Implemented.
// ═══════════════════════════════════════════════════════════════════════════════

var (
	handleV1SearchProtectedArea  http.HandlerFunc = notImplemented("search/protected_area")
	handleV1SearchGADM           http.HandlerFunc = notImplemented("search/gadm")
	handleV1SearchMunicipalities http.HandlerFunc = notImplemented("search/municipalities")
	handleV1SearchAddressOSM     http.HandlerFunc = notImplemented("search/address_osm")
	handleV1Feedback             http.HandlerFunc = notImplemented("feedback")
	// handleV1Lookup is defined in api_edm_lookup.go
	handleV1Docs    http.HandlerFunc = notImplemented("docs")
	handleV1DocsLLM http.HandlerFunc = notImplemented("docs/llm.txt")
)

// notImplemented returns a handler that responds with 501 for unregistered routes.
func notImplemented(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		apiError(w, fmt.Sprintf("endpoint %q not yet implemented", name), http.StatusNotImplemented)
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// Route registration
// ═══════════════════════════════════════════════════════════════════════════════

// RegisterAPIv1 registers all /api/v1/* routes on the given ServeMux.
// Each route is wrapped with the standard middleware chain (CORS → logging →
// format negotiation). Handler implementations are expected to be assigned
// to the package-level handler vars above before the server starts.
func RegisterAPIv1(mux *http.ServeMux) {
	v1 := func(pattern string, handler http.HandlerFunc) {
		mux.Handle(pattern, apiMiddleware(handler))
	}

	// KG bbox index + admin datasets
	v1("/api/v1/spatial/kgs", handleV1SpatialKGs)
	v1("/api/v1/search/protected_area", handleV1SearchProtectedArea)
	v1("/api/v1/search/gadm", handleV1SearchGADM)
	v1("/api/v1/search/municipalities", handleV1SearchMunicipalities)
	v1("/api/v1/search/address_osm", handleV1SearchAddressOSM)
	v1("/api/v1/addresses/search", handleV1AddressesSearch)

	// Feedback
	v1("/api/v1/feedback", handleV1Feedback)
	v1("/api/v1/feedback/{id}", handleV1Feedback)

	// Documentation
	// Lookup
	v1("/api/v1/lookup", handleV1Lookup)
	v1("/api/v1/kgs", handleV1KGs)                         // kg_universe.go — all 7 850 KGs, one list, universe_hash
	v1("/api/v1/kgs/bbox_changes", handleV1KGsBBoxChanges) // kg_universe_bbox.go — KGs whose bbox widened (slivers for peers)

	v1("/api/v1/license", handleV1License)
	v1("/api/v1/cache/policy", handleV1CachePolicy)
	v1("/api/v1/docs/cache-client.py", handleV1CacheClientPy)
	v1("/api/v1/docs/cache-client.js", handleV1CacheClientJS)
	v1("/api/v1/docs", handleV1Docs)
	v1("/api/v1/docs/llm.txt", handleV1DocsLLM)
	v1("/api/v1/docs/ne.md", serveNEPublicDoc)
	v1("/api/v1/docs/ne-change.md", serveNEChangeDoc)
	v1("/api/v1/docs/stats-envelope.md", serveNEStatsEnvelopeDoc) // the shared unit-stats answer shape (api_ne_stats_unit.go)
	v1("/api/v1/docs/qgis-layer.md", serveNEQGISLayerDoc)         // cells as MVT / PMTiles for QGIS (ne_mvt.go)

	log.Println("📡 API v1 routes registered under /api/v1/")
}

// ═══════════════════════════════════════════════════════════════════════════════
// Middleware
// ═══════════════════════════════════════════════════════════════════════════════

// apiMiddleware chains CORS, request logging, and format negotiation.
func apiMiddleware(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ── CORS ─────────────────────────────────────────────────────
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Accept, Authorization, X-Requested-With")
		w.Header().Set("Access-Control-Max-Age", "86400")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// ── Data license disclosure (api_license.go) ─────────────────
		setLicenseHeaders(w, r.URL.Path)

		// ── Request logging ──────────────────────────────────────────
		start := time.Now()
		lrw := &loggingResponseWriter{ResponseWriter: w, statusCode: http.StatusOK, pretty: wantPretty(r)}

		// ── Format negotiation ────────────────────────────────────────
		// Determine output format from ?format= query param, falling back
		// to the Accept header, then defaulting to JSON.
		format := negotiateFormat(r)
		r = r.WithContext(r.Context()) // keep original context
		// Store the negotiated format in a header so handlers can read it.
		r.Header.Set("X-Negotiated-Format", format)

		// ── KG-code canonicalisation ──────────────────────────────────
		// KG codes are five digits and 740 of them start with a zero.
		// `?kg=1503` used to match nothing at all (silently, with HTTP 200)
		// while `?kg=01503` worked — and /api/v1/lookup used to hand out the
		// short form. One central rewrite makes both spellings equivalent for
		// every endpoint; see kg_code.go.
		normalizeKGParams(r, lrw)

		next.ServeHTTP(lrw, r)

		// Body bytes are pre-gzip (the wire count is only final after the
		// outer gzip writer closes — see /api/v1/debug/egress for that).
		egressAddBody(r.Context(), lrw.bytes)
		gz := "plain"
		if c := egressFromContext(r.Context()); c != nil && c.gzip {
			gz = "gz"
		}
		// peer= names the caller by tier + token slot (api_tiers.go
		// requestPeerTag) so "who still runs the district loop?" is one grep.
		log.Printf("📨 %s %s → %d (%s) [%s] %dB %s ua=%s peer=%s",
			r.Method, r.URL.String(), lrw.statusCode,
			format, time.Since(start).Round(time.Microsecond),
			lrw.bytes, gz, uaFamily(r.Header.Get("User-Agent")), requestPeerTag(r))
	})
}

// loggingResponseWriter captures the status code for logging.
type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
	// pretty carries ?pretty=1 down to writeJSON/apiResponse, which only see
	// the ResponseWriter. Compact JSON is the default (halves payload size).
	pretty bool
	bytes  int // body bytes the handler wrote (pre-gzip)
}

// PrettyJSON implements prettyPreferrer (api_compress.go).
func (lrw *loggingResponseWriter) PrettyJSON() bool { return lrw.pretty }

// Flush propagates to the underlying writer (gzip middleware / net/http).
func (lrw *loggingResponseWriter) Flush() {
	if f, ok := lrw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (lrw *loggingResponseWriter) WriteHeader(code int) {
	if !lrw.wroteHeader {
		lrw.statusCode = code
		lrw.wroteHeader = true
	}
	lrw.ResponseWriter.WriteHeader(code)
}

func (lrw *loggingResponseWriter) Write(b []byte) (int, error) {
	if !lrw.wroteHeader {
		lrw.wroteHeader = true
	}
	lrw.bytes += len(b)
	return lrw.ResponseWriter.Write(b)
}

// negotiateFormat determines the response format from the request.
// Priority: ?format= query param > Accept header > default "json".
func negotiateFormat(r *http.Request) string {
	// Explicit query parameter takes precedence.
	if f := r.URL.Query().Get("format"); f != "" {
		switch strings.ToLower(f) {
		case "json":
			return "json"
		case "geojson":
			return "geojson"
		case "csv":
			return "csv"
		case "gpkg":
			return "gpkg"
		}
	}

	// Fall back to Accept header.
	accept := r.Header.Get("Accept")
	switch {
	case strings.Contains(accept, "application/geo+json"):
		return "geojson"
	case strings.Contains(accept, "text/csv"):
		return "csv"
	case strings.Contains(accept, "application/geopackage+sqlite3"):
		return "gpkg"
	}

	return "json"
}

// ═══════════════════════════════════════════════════════════════════════════════
// APIContext — common query parameter parsing
// ═══════════════════════════════════════════════════════════════════════════════

const (
	defaultLimit  = 100
	maxLimit      = 100000
	defaultOffset = 0
)

// APIContext holds common query parameters parsed from any API v1 request.
// Use ParseAPIContext(r) to create one from an incoming request.
type APIContext struct {
	// Format is the negotiated output format: json, geojson, csv, or gpkg.
	Format string `json:"format"`

	// Limit is the maximum number of results to return (default 100, max 100 000).
	Limit int `json:"limit"`

	// Offset is the pagination offset (default 0).
	Offset int `json:"offset"`

	// Fields is a set of requested field names for sparse fieldsets.
	// Empty means "return all fields".
	Fields map[string]bool `json:"-"`

	// FieldList is the raw comma-separated field list (empty = all).
	FieldList string `json:"fields,omitempty"`

	// Callback is an optional JSONP callback function name.
	Callback string `json:"callback,omitempty"`

	// RequestStart records when parsing began, for timing metadata.
	RequestStart time.Time `json:"-"`
}

// ParseAPIContext extracts common query parameters from r.
func ParseAPIContext(r *http.Request) *APIContext {
	q := r.URL.Query()

	ctx := &APIContext{
		Format:       r.Header.Get("X-Negotiated-Format"),
		Limit:        defaultLimit,
		Offset:       defaultOffset,
		Fields:       make(map[string]bool),
		RequestStart: time.Now(),
	}
	if ctx.Format == "" {
		ctx.Format = "json"
	}

	// Limit
	if s := q.Get("limit"); s != "" {
		if v, err := strconv.Atoi(s); err == nil {
			if v < 0 {
				v = defaultLimit
			}
			if v > maxLimit {
				v = maxLimit
			}
			ctx.Limit = v
		}
	}

	// Offset
	if s := q.Get("offset"); s != "" {
		if v, err := strconv.Atoi(s); err == nil {
			if v < 0 {
				v = 0
			}
			ctx.Offset = v
		}
	}

	// Fields (sparse fieldsets)
	if f := q.Get("fields"); f != "" {
		ctx.FieldList = f
		for _, name := range strings.Split(f, ",") {
			name = strings.TrimSpace(name)
			if name != "" {
				ctx.Fields[name] = true
			}
		}
	}

	// Callback (JSONP)
	ctx.Callback = q.Get("callback")

	return ctx
}

// HasField returns true if the caller requested this field (or if no field
// filter was specified, meaning "all fields").
func (ac *APIContext) HasField(name string) bool {
	if len(ac.Fields) == 0 {
		return true
	}
	return ac.Fields[name]
}

// FilterFields returns a copy of m containing only the requested fields.
// If no field filter is active, the original map is returned unchanged.
func (ac *APIContext) FilterFields(m map[string]interface{}) map[string]interface{} {
	if len(ac.Fields) == 0 {
		return m
	}
	filtered := make(map[string]interface{}, len(ac.Fields))
	for k, v := range m {
		if ac.Fields[k] {
			filtered[k] = v
		}
	}
	return filtered
}

// ElapsedMS returns milliseconds since RequestStart.
func (ac *APIContext) ElapsedMS() float64 {
	return float64(time.Since(ac.RequestStart).Microseconds()) / 1000.0
}

// ═══════════════════════════════════════════════════════════════════════════════
// Meta — pagination and query statistics
// ═══════════════════════════════════════════════════════════════════════════════

// Meta carries pagination and timing information in every API response.
type Meta struct {
	Total       int     `json:"total"`
	Limit       int     `json:"limit"`
	Offset      int     `json:"offset"`
	Format      string  `json:"format"`
	QueryTimeMS float64 `json:"query_time_ms"`

	// Toponym layer disclosure (feedback #17): scoring/accuracy caveats.
	ToponymNote string `json:"toponym_note,omitempty"`
	// QueryUsed reports a fallback rewrite of ?q= (e.g. organisational
	// stopwords stripped) when the literal query matched nothing.
	QueryUsed string `json:"query_used,omitempty"`

	// Data licensing (api_license.go). Filled by apiResponse/apiGeoJSONResponse
	// when nil: BEV CC BY 4.0 attribution + modified note; adds the ODbL note
	// when rows carry an osm{} block.
	License *LicenseMeta `json:"license,omitempty"`
}

// NewMeta constructs a Meta from an APIContext and a total result count.
func NewMeta(ac *APIContext, total int) Meta {
	return Meta{
		Total:       total,
		Limit:       ac.Limit,
		Offset:      ac.Offset,
		Format:      ac.Format,
		QueryTimeMS: ac.ElapsedMS(),
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// Response helpers
// ═══════════════════════════════════════════════════════════════════════════════

// apiResponse writes data + meta in the negotiated format.
// Currently supports JSON output; other formats fall back to JSON with a
// format indicator so callers can add format-specific handlers later.
func apiResponse(w http.ResponseWriter, r *http.Request, data interface{}, meta Meta) {
	// Ensure nil slices serialize as [] not null
	if data == nil {
		data = []interface{}{}
	}
	if meta.License == nil {
		meta.License = AttributionMetaFor(r.URL.Path, dataHasOSM(data))
	}
	payload := map[string]interface{}{
		"data": data,
		"meta": meta,
	}

	ac := ParseAPIContext(r)

	// JSONP wrapping
	if ac.Callback != "" {
		writeJSONP(w, ac.Callback, payload)
		return
	}

	writeJSON(w, http.StatusOK, payload)
}

// dataHasOSM reports whether any row of a []map payload carries an osm{} block
// (OSM-derived, ODbL) so the license note can say so. Checks the first 50 rows.
func dataHasOSM(data interface{}) bool {
	rows, ok := data.([]map[string]interface{})
	if !ok {
		return false
	}
	for i, row := range rows {
		if i >= 50 {
			break
		}
		if _, has := row["osm"]; has {
			return true
		}
	}
	return false
}

// apiError writes a structured error response as JSON.
func apiError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error":  msg,
		"status": code,
	})
}

// apiGeoJSONResponse writes a valid RFC 7946 GeoJSON FeatureCollection.
//
// Each element of features must be a map with at least:
//
//	"type":       "Feature"
//	"geometry":   { "type": "...", "coordinates": ... }   (or nil)
//	"properties": { ... }
//
// The function wraps them in a FeatureCollection envelope and appends meta
// as a top-level "meta" key (allowed by the GeoJSON spec as a foreign member).
func apiGeoJSONResponse(w http.ResponseWriter, r *http.Request, features []map[string]interface{}, meta Meta) {
	if features == nil {
		features = []map[string]interface{}{}
	}
	if meta.License == nil {
		withOSM := false
		for _, f := range features {
			if p, ok := f["properties"].(map[string]interface{}); ok {
				if _, has := p["osm"]; has {
					withOSM = true
					break
				}
			}
		}
		meta.License = AttributionMetaFor(r.URL.Path, withOSM)
	}

	// Ensure every feature has required fields.
	for i := range features {
		if _, ok := features[i]["type"]; !ok {
			features[i]["type"] = "Feature"
		}
		if _, ok := features[i]["geometry"]; !ok {
			features[i]["geometry"] = nil
		}
		if _, ok := features[i]["properties"]; !ok {
			features[i]["properties"] = map[string]interface{}{}
		}
	}

	collection := map[string]interface{}{
		"type":     "FeatureCollection",
		"features": features,
		"meta":     meta,
	}

	ac := ParseAPIContext(r)
	if ac.Callback != "" {
		writeJSONP(w, ac.Callback, collection)
		return
	}

	w.Header().Set("Content-Type", "application/geo+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = jsonEncoderFor(w).Encode(collection)
}

// apiCSVResponse writes tabular data as RFC 4180 CSV with a Content-Disposition
// header for the given filename.
func apiCSVResponse(w http.ResponseWriter, r *http.Request, headers []string, rows [][]string, filename string) {
	if filename == "" {
		filename = "export.csv"
	}
	if !strings.HasSuffix(filename, ".csv") {
		filename += ".csv"
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)

	cw := csv.NewWriter(w)
	// Write BOM for Excel compatibility.
	_, _ = w.Write([]byte{0xEF, 0xBB, 0xBF})

	_ = cw.Write(headers)
	for _, row := range rows {
		_ = cw.Write(row)
	}
	cw.Flush()
}

// ═══════════════════════════════════════════════════════════════════════════════
// Internal helpers
// ═══════════════════════════════════════════════════════════════════════════════

// writeJSON marshals v as pretty-printed JSON and writes it to w.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = jsonEncoderFor(w).Encode(v)
}

// writeJSONP wraps a JSON payload in a JSONP callback invocation.
func writeJSONP(w http.ResponseWriter, callback string, v interface{}) {
	// Sanitise callback name: allow only alphanumerics, dots, underscores.
	safe := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' {
			return r
		}
		return -1
	}, callback)
	if safe == "" {
		safe = "callback"
	}

	var b []byte
	var err error
	if prettyRequested(w) {
		b, err = json.MarshalIndent(v, "", "  ")
	} else {
		b, err = json.Marshal(v)
	}
	if err != nil {
		apiError(w, "json marshal error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "/**/ %s(%s);\n", safe, b)
}

// ═══════════════════════════════════════════════════════════════════════════════
// Convenience: build a GeoJSON Feature from properties + optional point coords.
// ═══════════════════════════════════════════════════════════════════════════════

// GeoJSONPointFeature creates a GeoJSON Feature map with a Point geometry
// (lon/lat in EPSG:4326). If lon and lat are both 0, geometry is set to nil.
func GeoJSONPointFeature(lon, lat float64, properties map[string]interface{}) map[string]interface{} {
	var geometry interface{}
	if lon != 0 || lat != 0 {
		geometry = map[string]interface{}{
			"type":        "Point",
			"coordinates": coord{lon, lat}, // 7 decimals, see api_compress.go
		}
	}
	return map[string]interface{}{
		"type":       "Feature",
		"geometry":   geometry,
		"properties": properties,
	}
}
