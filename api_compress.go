// api_compress.go — transparent response compression + JSON output tuning.
//
// Why this exists
// ───────────────
// Viewport responses are big: a single
// `/api/v1/spatial/parcels?west=…` over Graz returns ~8.2 MB of
// pretty-printed JSON. Measured breakdown for one such response:
//
//	pretty-printed (what we used to send) : 2 563 885 B … 8 200 000 B
//	compact (no indent)                   : 1 198 734 B  (-53 %)
//	compact + gzip                        :   188 879 B  (-93 %)
//
// Every byte of that travelled uncompressed to the browser because the
// handlers wrote directly to the raw http.ResponseWriter and Go's net/http
// does NOT compress automatically. On a typical 20 Mbit uplink an 8 MB
// payload is ~3 s of pure transfer — which is exactly the "still feels slow"
// the map UI ({MAP_NAME}) was seeing even though the server-side
// handler finished in 150 ms.
//
// Two fixes, both here:
//  1. gzipMiddleware — negotiated Content-Encoding for compressible types.
//  2. compact JSON by default; pretty-printing is opt-in via ?pretty=1.
package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// ─── gzip ────────────────────────────────────────────────────────────────

// gzipWriterPool reuses gzip.Writers (each holds a ~256 KB window+tables;
// allocating one per request would add real GC pressure at our request rates).
var gzipWriterPool = sync.Pool{
	New: func() interface{} {
		w, _ := gzip.NewWriterLevel(io.Discard, gzipLevel)
		return w
	},
}

// compressibleContentType reports whether a Content-Type benefits from gzip.
// Deliberately excludes GeoPackage (SQLite, already binary) and any image.
func compressibleContentType(ct string) bool {
	ct = strings.ToLower(ct)
	switch {
	case strings.HasPrefix(ct, "application/json"),
		strings.HasPrefix(ct, "application/geo+json"),
		strings.HasPrefix(ct, "application/vnd.ne-rows"), // NE bin rows gzip ~6×; zstd / vnd.ne-cells are already compressed and NOT listed
		strings.HasPrefix(ct, "application/vnd.ne-fidx"), // filter index bins: byte-planar columns, gzip ~4–8× (ne_glyph_fidx.go)
		strings.HasPrefix(ct, "application/javascript"),
		strings.HasPrefix(ct, "text/"):
		return true
	}
	return false
}

// gzipResponseWriter defers the compress-or-not decision until the handler
// writes its first byte, because that's when Content-Type is finally known.
type gzipResponseWriter struct {
	http.ResponseWriter
	gz         *gzip.Writer
	decided    bool
	compress   bool
	status     int
	minSize    int
	buf        []byte // holds the first write while we decide
	bufFlushed bool
}

func (g *gzipResponseWriter) WriteHeader(code int) {
	g.status = code
	g.decide()
	g.ResponseWriter.WriteHeader(code)
}

// decide inspects Content-Type once and, if compressible, installs the gzip
// writer and rewrites the headers (Content-Length becomes unknowable).
func (g *gzipResponseWriter) decide() {
	if g.decided {
		return
	}
	g.decided = true
	h := g.ResponseWriter.Header()
	if !compressibleContentType(h.Get("Content-Type")) {
		return
	}
	if h.Get("Content-Encoding") != "" {
		return // handler already encoded (e.g. serving a .gz blob)
	}
	g.compress = true
	h.Set("Content-Encoding", "gzip")
	h.Add("Vary", "Accept-Encoding")
	h.Del("Content-Length")
	gz := gzipWriterPool.Get().(*gzip.Writer)
	gz.Reset(g.ResponseWriter)
	g.gz = gz
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	if !g.decided {
		// Small bodies (errors, tiny stat blobs) aren't worth a gzip frame:
		// buffer the first chunk so we can tell small from large.
		if !g.bufFlushed && len(g.buf)+len(b) < g.minSize {
			g.buf = append(g.buf, b...)
			return len(b), nil
		}
		g.decide()
		g.ResponseWriter.WriteHeader(g.statusOrOK())
	}
	if len(g.buf) > 0 && !g.bufFlushed {
		g.bufFlushed = true
		pending := g.buf
		g.buf = nil
		if _, err := g.rawWrite(pending); err != nil {
			return 0, err
		}
	}
	return g.rawWrite(b)
}

func (g *gzipResponseWriter) rawWrite(b []byte) (int, error) {
	if g.compress {
		return g.gz.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

func (g *gzipResponseWriter) statusOrOK() int {
	if g.status != 0 {
		return g.status
	}
	return http.StatusOK
}

// close flushes any buffered small body and returns the gzip.Writer to the pool.
func (g *gzipResponseWriter) close() {
	if len(g.buf) > 0 && !g.bufFlushed {
		// Never grew past minSize → send uncompressed, no Content-Encoding.
		g.bufFlushed = true
		g.decided = true
		g.ResponseWriter.WriteHeader(g.statusOrOK())
		_, _ = g.ResponseWriter.Write(g.buf)
		g.buf = nil
		return
	}
	if g.gz != nil {
		_ = g.gz.Close()
		gzipWriterPool.Put(g.gz)
		g.gz = nil
	}
}

func (g *gzipResponseWriter) Flush() {
	if g.gz != nil {
		_ = g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// gzipMinSize — below this, compression costs more (CPU + framing) than it saves.
const gzipMinSize = 1400

// gzipLevel — deliberately NOT DefaultCompression(6). A CPU profile under a
// 12-thread mixed load on this 2-core box attributed 32 % of all samples to
// flate at level 6. On a representative viewport payload:
//
//	level 1 → 242 656 B in 6 ms
//	level 3 → 228 089 B in 8 ms
//	level 6 → 188 867 B in 19 ms
//
// Combined with 7-decimal coordinates (see coord below) level 3 lands at
// ~170 KB — i.e. smaller than level 6 used to produce — for a third of the CPU.
const gzipLevel = 3

// gzipMiddleware wraps a handler with negotiated gzip compression.
func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponseWriter{ResponseWriter: w, minSize: gzipMinSize}
		defer gw.close()
		next.ServeHTTP(gw, r)
	})
}

// ─── JSON output tuning ──────────────────────────────────────────────────

// wantPretty reports whether the caller explicitly asked for indented JSON.
// Indentation roughly DOUBLES payload size (and encode CPU) on
// coordinate-heavy responses, so it is no longer the default.
func wantPretty(r *http.Request) bool {
	if r == nil {
		return false
	}
	switch strings.ToLower(r.URL.Query().Get("pretty")) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// prettyPreferrer lets the shared writeJSON/apiResponse helpers — which only
// receive an http.ResponseWriter, not the *http.Request — learn whether this
// particular request asked for indented output. apiMiddleware sets the flag on
// the wrapper it hands to the handler.
type prettyPreferrer interface{ PrettyJSON() bool }

func prettyRequested(w http.ResponseWriter) bool {
	if p, ok := w.(prettyPreferrer); ok {
		return p.PrettyJSON()
	}
	return false
}

// jsonEncoderFor returns an encoder that indents only when asked to.
func jsonEncoderFor(w http.ResponseWriter) *json.Encoder {
	enc := json.NewEncoder(w)
	if prettyRequested(w) {
		enc.SetIndent("", "  ")
	}
	return enc
}

// ─── Coordinate precision ────────────────────────────────────────────────
//
// Go's encoding/json prints float64 with full round-trip precision, so a
// longitude serialises as `15.401319265365601` — 17 significant digits for a
// value whose source (BEV cadastre, ETRS89) is accurate to centimetres. At
// 7 decimal places a degree of longitude is resolved to ~0.8 cm, which is
// finer than the input data.
//
// Measured on one Graz viewport response (479 parcels, full polygons):
//
//	full precision, compact : 1 198 734 B → 242 656 B gzipped
//	7 decimals,     compact :   880 532 B → 170 266 B gzipped  (-27 % / -30 %)
//
// coord implements json.Marshaler to emit exactly that, without the float
// formatting cost of strconv's shortest-representation search.
type coord [2]float64

func (c coord) MarshalJSON() ([]byte, error) {
	b := make([]byte, 0, 48)
	b = append(b, '[')
	b = strconv.AppendFloat(b, c[0], 'f', coordDecimals, 64)
	b = append(b, ',')
	b = strconv.AppendFloat(b, c[1], 'f', coordDecimals, 64)
	b = append(b, ']')
	return b, nil
}

// coordDecimals — 7 ≈ 0.8 cm at Austrian latitudes; the cadastre source is
// coarser than that, so nothing real is lost.
const coordDecimals = 7

// ─── Client-side caching ─────────────────────────────────────────────────
//
// Cadastre data changes when a KG is reprocessed — on the order of weeks, not
// seconds. But every response went out with no cache directives at all, so a
// panning map re-downloaded (and the server re-encoded + re-gzipped) byte-for-
// byte identical viewports on every pan-back, and a browser refresh threw away
// everything. Since the CPU profile shows we are encode/compress bound, the
// cheapest possible win is to let the client skip the request entirely.
//
// Two mechanisms, both conservative:
//   - Cache-Control: public, max-age=<apiCacheMaxAge> on idempotent GETs.
//   - ETag + 304 on the same responses, so even after max-age expires the
//     client revalidates with an empty body instead of re-downloading MBs.
//
// Anything that mutates state (POST/PUT/DELETE, /api/inspire/process,
// /feedback) is explicitly excluded, as is /debug.

const (
	// apiCacheMaxAge — long enough that panning/zooming a map reuses the
	// browser cache, short enough that a reprocessed KG shows up promptly.
	apiCacheMaxAge = 120
	// etagMaxBytes bounds the buffer we're willing to hold to compute an ETag.
	// Above this we skip the ETag (Cache-Control still applies) rather than
	// buffer an unbounded response.
	etagMaxBytes = 16 << 20
)

// cacheablePath reports whether responses for this path may be cached by clients.
func cacheablePath(p string) bool {
	switch {
	case strings.HasPrefix(p, "/debug/"),
		strings.HasPrefix(p, "/api/v1/debug/"),
		strings.HasPrefix(p, "/api/v1/feedback"),
		p == "/api/v1/ne/revisit",       // live revisit hints for peers (api_ne_revisit.go)
		p == "/api/v1/ne/glyph/soak.js": // test driver: must see the current build (docs/ne-glyph.md §soak)
		return false
	}
	return strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/llm/") || p == "/llm.txt"
}

// etagResponseWriter buffers a successful body so we can hash it, emit an
// ETag, and answer a matching If-None-Match with 304.
type etagResponseWriter struct {
	http.ResponseWriter
	status    int
	buf       []byte
	tooBig    bool
	headerSet bool
}

func (e *etagResponseWriter) WriteHeader(code int) {
	e.status = code
	// A handler that set its own ETag (export_cache.go: key-derived, known
	// before the body exists) has already handled If-None-Match; stream it
	// through instead of buffering megabytes to hash them again.
	// Non-200 (errors, 503 "still warming") must not be cached or hashed.
	if code != http.StatusOK || e.ResponseWriter.Header().Get("ETag") != "" {
		e.tooBig = true
		e.headerSet = true
		e.ResponseWriter.WriteHeader(code)
	}
}

func (e *etagResponseWriter) Write(b []byte) (int, error) {
	if e.tooBig {
		if !e.headerSet {
			e.headerSet = true
			e.ResponseWriter.WriteHeader(e.statusOr200())
		}
		return e.ResponseWriter.Write(b)
	}
	if len(e.buf) == 0 && e.ResponseWriter.Header().Get("ETag") != "" {
		e.tooBig = true // handler-managed ETag, see WriteHeader
		e.headerSet = true
		e.ResponseWriter.WriteHeader(e.statusOr200())
		return e.ResponseWriter.Write(b)
	}
	if len(e.buf)+len(b) > etagMaxBytes {
		// Give up on the ETag: flush what we have and stream the rest.
		e.tooBig = true
		e.headerSet = true
		e.ResponseWriter.WriteHeader(e.statusOr200())
		if len(e.buf) > 0 {
			if _, err := e.ResponseWriter.Write(e.buf); err != nil {
				return 0, err
			}
			e.buf = nil
		}
		return e.ResponseWriter.Write(b)
	}
	e.buf = append(e.buf, b...)
	return len(b), nil
}

func (e *etagResponseWriter) statusOr200() int {
	if e.status != 0 {
		return e.status
	}
	return http.StatusOK
}

func (e *etagResponseWriter) Flush() {}

// PrettyJSON forwards the pretty flag if the wrapped writer carries one.
func (e *etagResponseWriter) PrettyJSON() bool { return prettyRequested(e.ResponseWriter) }

// cacheMiddleware adds Cache-Control and ETag/304 handling to idempotent GETs.
// It must sit INSIDE gzipMiddleware so the ETag is computed over the
// uncompressed body (encoding-independent, and a 304 then needs no gzip work).
func cacheMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		if !cacheablePath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		ew := &etagResponseWriter{ResponseWriter: w}
		next.ServeHTTP(ew, r)

		if ew.tooBig {
			return // already streamed
		}

		sum := sha256.Sum256(etagNormalize(ew.buf))
		etag := `"` + hex.EncodeToString(sum[:16]) + `"`
		h := w.Header()
		h.Set("ETag", etag)
		if !strings.Contains(h.Get("Cache-Control"), "no-store") { // a handler's no-store (a tile rendered before its peer data arrived) wins
			h.Set("Cache-Control", "public, max-age="+strconv.Itoa(apiCacheMaxAge))
		}

		if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, etag) {
			h.Del("Content-Type")
			h.Del("Content-Length")
			w.WriteHeader(http.StatusNotModified)
			return
		}
		h.Set("Content-Length", strconv.Itoa(len(ew.buf)))
		w.WriteHeader(ew.statusOr200())
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(ew.buf)
	})
}

// etagMatches implements the If-None-Match list comparison (RFC 9110 §13.1.2),
// tolerating weak validators and `*`.
func etagMatches(header, etag string) bool {
	for _, cand := range strings.Split(header, ",") {
		cand = strings.TrimSpace(cand)
		if cand == "*" {
			return true
		}
		cand = strings.TrimPrefix(cand, "W/")
		if cand == etag {
			return true
		}
	}
	return false
}

// etagNormalize returns the body with volatile timing fields removed so the
// ETag is stable across identical answers. meta.query_time_ms differs on every
// request; hashing it meant If-None-Match NEVER matched and the whole 304 path
// was dead. Only the hash input is altered — the body sent is unchanged.
func etagNormalize(b []byte) []byte {
	const key = `"query_time_ms":`
	i := bytes.Index(b, []byte(key))
	if i < 0 {
		return b
	}
	out := make([]byte, 0, len(b))
	for i >= 0 {
		j := i + len(key)
		for j < len(b) && (b[j] >= '0' && b[j] <= '9' || b[j] == '.' || b[j] == '-' || b[j] == 'e' || b[j] == '+') {
			j++
		}
		// Drop the field plus one adjacent comma to keep the slice JSON-shaped
		// (cosmetic; only hashed).
		if j < len(b) && b[j] == ',' {
			j++
		} else if i > 0 && b[i-1] == ',' {
			i--
		}
		out = append(out, b[:i]...)
		b = b[j:]
		i = bytes.Index(b, []byte(key))
	}
	return append(out, b...)
}
