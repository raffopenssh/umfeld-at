package main

// Egress accounting — WHO is consuming outbound bandwidth, on WHICH endpoint,
// and whether they let us compress.
//
// Disk use was measured and budgeted (cache_budgets.go); bandwidth was not.
// The host graphs showed TX climbing 5–10× in a week with no way to attribute
// it: the request log recorded status + latency but not bytes, and not whether
// the client sent Accept-Encoding: gzip (a bare `curl`/`urllib` consumer pays
// ~4× the bytes of one that does). This file adds:
//
//   - a counting writer at the OUTERMOST layer (below gzip), so the number is
//     bytes on the wire, not bytes the handler produced;
//   - per-endpoint-family accumulators (ids/params collapsed) with wire bytes,
//     uncompressed bytes, hits, 304s, gzip-negotiated hits and the User-Agents
//     seen — all in RAM, zero per-request IO;
//   - `GET /api/v1/debug/egress` to read them, plus `bytes=` / `gz` / `304`
//     on the 📨 request log line.

import (
	"context"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// egressCounter travels with the request (context) so the inner logging
// middleware can print the wire byte count the outer writer measured.
type egressCounter struct {
	wire   int64 // bytes written to the socket (post-gzip)
	body   int64 // bytes the handler wrote (pre-gzip)
	gzip   bool  // client advertised gzip
	status int
}

type egressCtxKey struct{}

func egressFromContext(ctx context.Context) *egressCounter {
	c, _ := ctx.Value(egressCtxKey{}).(*egressCounter)
	return c
}

// wireCountingWriter counts bytes that reach the socket.
type wireCountingWriter struct {
	http.ResponseWriter
	c *egressCounter
}

func (w *wireCountingWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	atomic.AddInt64(&w.c.wire, int64(n))
	return n, err
}

func (w *wireCountingWriter) WriteHeader(code int) {
	w.c.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *wireCountingWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// bodyCountingWriter counts bytes the handler produced (installed by
// apiMiddleware, inside gzip). Used for the compression-ratio column.
func egressAddBody(ctx context.Context, n int) {
	if c := egressFromContext(ctx); c != nil {
		atomic.AddInt64(&c.body, int64(n))
	}
}

// ─── accumulators ─────────────────────────────────────────────────────────

type egressFamily struct {
	Hits       int64            `json:"hits"`
	NotMod     int64            `json:"status_304"`
	GzipHits   int64            `json:"gzip_negotiated"`
	WireBytes  int64            `json:"wire_bytes"`
	BodyBytes  int64            `json:"body_bytes"`
	UserAgents map[string]int64 `json:"user_agents,omitempty"`
}

var (
	egressMu       sync.Mutex
	egressFamilies = map[string]*egressFamily{}
	egressSince    = time.Now()
	egressTotal    egressFamily
)

var (
	// /63349-348/6/geometry → /{id}/geometry ; /63349_fp_9/parcels → /{id}/parcels
	reKGObj  = regexp.MustCompile(`/\d{5}-[^/]+(/\d+)*|/\d{5}_fp_\d+`)
	reNum    = regexp.MustCompile(`^\d+$`)      // whole numeric path segment → {n}
	reBraces = regexp.MustCompile(`/\{[^/]*\}`) // toponym GUIDs
)

// egressFamilyOf collapses ids so one endpoint is one row.
func egressFamilyOf(path string) string {
	p := reKGObj.ReplaceAllString(path, "/{id}")
	p = reBraces.ReplaceAllString(p, "/{id}")
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		if reNum.MatchString(seg) {
			segs[i] = "{n}"
		}
	}
	p = strings.Join(segs, "/")
	if len(p) > 80 {
		p = p[:80]
	}
	return p
}

// uaFamily shortens a User-Agent to its product token so the map stays small.
func uaFamily(ua string) string {
	if ua == "" {
		return "(none)"
	}
	if i := strings.IndexAny(ua, " ("); i > 0 {
		ua = ua[:i]
	}
	if len(ua) > 40 {
		ua = ua[:40]
	}
	return ua
}

func egressRecord(path, ua string, c *egressCounter) {
	fam := egressFamilyOf(path)
	uaf := uaFamily(ua)
	egressMu.Lock()
	defer egressMu.Unlock()
	f := egressFamilies[fam]
	if f == nil {
		if len(egressFamilies) > 500 {
			fam = "(other)"
			if f = egressFamilies[fam]; f == nil {
				f = &egressFamily{UserAgents: map[string]int64{}}
				egressFamilies[fam] = f
			}
		} else {
			f = &egressFamily{UserAgents: map[string]int64{}}
			egressFamilies[fam] = f
		}
	}
	for _, t := range []*egressFamily{f, &egressTotal} {
		t.Hits++
		t.WireBytes += c.wire
		t.BodyBytes += c.body
		if c.status == http.StatusNotModified {
			t.NotMod++
		}
		if c.gzip {
			t.GzipHits++
		}
	}
	if len(f.UserAgents) < 30 || f.UserAgents[uaf] > 0 {
		f.UserAgents[uaf]++
	}
}

// egressMiddleware is the OUTERMOST wrapper: it sees bytes after gzip.
func egressMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := &egressCounter{gzip: strings.Contains(r.Header.Get("Accept-Encoding"), "gzip"), status: 200}
		r = r.WithContext(context.WithValue(r.Context(), egressCtxKey{}, c))
		next.ServeHTTP(&wireCountingWriter{ResponseWriter: w, c: c}, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			egressRecord(r.URL.Path, r.Header.Get("User-Agent"), c)
		}
	})
}

// egressStatsHandler — GET /api/v1/debug/egress
func egressStatsHandler(w http.ResponseWriter, r *http.Request) {
	type row struct {
		Family string `json:"endpoint"`
		egressFamily
		WireMB    float64 `json:"wire_mb"`
		AvgWireKB float64 `json:"avg_wire_kb"`
		GzipPct   float64 `json:"gzip_pct"`
		Ratio     float64 `json:"wire_to_body_ratio"`
		SharePct  float64 `json:"share_of_egress_pct"`
	}
	egressMu.Lock()
	rows := make([]row, 0, len(egressFamilies))
	for k, f := range egressFamilies {
		cp := *f
		cp.UserAgents = map[string]int64{}
		for ua, n := range f.UserAgents {
			cp.UserAgents[ua] = n
		}
		rr := row{Family: k, egressFamily: cp}
		rr.WireMB = float64(f.WireBytes) / 1e6
		if f.Hits > 0 {
			rr.AvgWireKB = float64(f.WireBytes) / float64(f.Hits) / 1e3
			rr.GzipPct = 100 * float64(f.GzipHits) / float64(f.Hits)
		}
		if f.BodyBytes > 0 {
			rr.Ratio = float64(f.WireBytes) / float64(f.BodyBytes)
		}
		if egressTotal.WireBytes > 0 {
			rr.SharePct = 100 * float64(f.WireBytes) / float64(egressTotal.WireBytes)
		}
		rows = append(rows, rr)
	}
	total := egressTotal
	since := egressSince
	egressMu.Unlock()
	sort.Slice(rows, func(i, j int) bool { return rows[i].WireBytes > rows[j].WireBytes })
	if n := 40; len(rows) > n {
		rows = rows[:n]
	}
	hours := time.Since(since).Hours()
	out := map[string]interface{}{
		"since":             since.UTC().Format(time.RFC3339),
		"hours":             hours,
		"total_hits":        total.Hits,
		"total_wire_mb":     float64(total.WireBytes) / 1e6,
		"total_body_mb":     float64(total.BodyBytes) / 1e6,
		"avg_wire_mb_per_h": float64(total.WireBytes) / 1e6 / maxf(hours, 1e-9),
		"status_304":        total.NotMod,
		"gzip_negotiated":   total.GzipHits,
		"endpoints":         rows,
		"note": "wire_bytes = after gzip, body_bytes = as produced by the handler. " +
			"A family with gzip_pct << 100 is a client that does not send Accept-Encoding: gzip and costs ~4x.",
	}
	writeJSON(w, 200, out)
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
