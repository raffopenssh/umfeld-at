// debug_pprof.go — runtime observability for the API process.
//
// The API wedged twice (Aug 2026) with every DB-touching endpoint hanging
// while static endpoints stayed at 120µs. That signature (HTTP loop healthy,
// DB handlers blocked) needs a goroutine dump to diagnose, so pprof and a
// cheap in-flight request tracker are always available.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/pprof"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type inflightEntry struct {
	Method string    `json:"method"`
	URL    string    `json:"url"`
	Start  time.Time `json:"start"`
}

var (
	inflightMu  sync.Mutex
	inflight    = map[uint64]*inflightEntry{}
	inflightSeq uint64
)

// TrackRequest registers a request as in-flight; the returned func clears it.
func TrackRequest(method, url string) func() {
	id := atomic.AddUint64(&inflightSeq, 1)
	e := &inflightEntry{Method: method, URL: url, Start: time.Now()}
	inflightMu.Lock()
	inflight[id] = e
	inflightMu.Unlock()
	return func() {
		inflightMu.Lock()
		delete(inflight, id)
		inflightMu.Unlock()
	}
}

// RegisterDebugAPI mounts pprof + /api/v1/debug/inflight.
func RegisterDebugAPI(mux *http.ServeMux) {
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	mux.HandleFunc("/api/v1/debug/egress", egressStatsHandler)
	mux.HandleFunc("/api/v1/debug/inflight", func(w http.ResponseWriter, r *http.Request) {
		type row struct {
			Method  string  `json:"method"`
			URL     string  `json:"url"`
			AgeSecs float64 `json:"age_secs"`
		}
		inflightMu.Lock()
		rows := make([]row, 0, len(inflight))
		for _, e := range inflight {
			rows = append(rows, row{e.Method, e.URL, time.Since(e.Start).Seconds()})
		}
		inflightMu.Unlock()
		sort.Slice(rows, func(i, j int) bool { return rows[i].AgeSecs > rows[j].AgeSecs })

		out := map[string]interface{}{
			"inflight_count":   len(rows),
			"requests":         rows,
			"goroutines":       runtime.NumGoroutine(),
			"kg_universe":      KGUniverseStats(),   // kg_universe.go: /api/v1/kgs body + gzip bytes, peer agreement
			"kg_bbox_diff":     KGBBoxDiffStats(),   // kg_universe_bbox.go: /api/v1/kgs/bbox_changes rows (≤ 7 850)
			"ne_manifest":      neManifestStats(),   // api_ne_manifest.go: header cache, build queue
			"ne_store":         NEStoreStats(),      // ne_store.go: .nex index, K rows, chunk/whole LRUs
			"ne_change":        NEChangeStats(),     // ne_change.go: parsed chunk-version logs LRU + pending wants
			"ne_declared_view": NEDeclStateStats(),  // ne_change_declared.go: per-KG state of the declared view on the chunk log
			"ne_reports":       neReportStats(),     // api_ne_reports.go: observer epoch reports
			"ne_revisit":       NERevisitStats(),    // api_ne_revisit.go: revisit-hint memo (≤ 1000 rows per list, 10 min)
			"ne_glyph":         NEGlyphStats(),      // ne_glyph.go: landing-page glyph SVG + tooltip JSON (1 slot)
			"ne_glyph_admin":   neGlyphAdminStats(), // ne_glyph_admin.go: admin rings (once) + outline memo (bounded)
			"osm_remote_grid":  RemoteGridStats(),   // osm_remote_grid.go: 250 m remoteness bytes for the roads ink (fixed, ~3 MB)
			"ne_glyph_tiles":   NEGlyphTileStats(),  // ne_glyph_tile.go: rendered res-12 PNG tiles (LRU, 24 MB)
			"ne_passes":        nePassMemoSize(),    // ne_change_passes.go: per-KG pass tables (≤ 512) + national memo age
			"ne_glyph_sub":     NEGlyphSubStats(),   // ne_glyph_sub.go: INVEKOS H3 bins (LRU 8 MB) + .poly outline bins (LRU 24 MB), 24 h TTL
			"ne_fidx":          NEFidxStats(),       // ne_glyph_fidx.go: filter index bins (LRU 48 MB + disk 2 GB)
			"ne_stats":         NEStatsUnitStats(),  // api_ne_stats_unit.go: envelope + peer memo (LRU 16 MB, 10 min)
			"ne_mvt":           NEMVTStats(),        // ne_mvt.go: cell tiles LRU, ids LRU, build queue, archive jobs
			"ne_mvt_gate":      NEMVTGateStats(),    // ne_mvt_gate.go: per-name tiles/km² budgets
			"export_gate":      ExportGateStats(),   // export_gate.go: identities, budgets, top callers
			"ne_glyph_dif":     NEGlyphDifStats(),   // ne_glyph_dif.go: lidar epoch per KG from the lidar peer (one table, 24 h)
		}
		if globalSearchIndex != nil {
			s := globalSearchIndex.DB().Stats()
			out["db"] = map[string]interface{}{
				"open_connections":  s.OpenConnections,
				"in_use":            s.InUse,
				"idle":              s.Idle,
				"max_open":          s.MaxOpenConnections,
				"wait_count":        s.WaitCount,
				"wait_duration_sec": s.WaitDuration.Seconds(),
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	})
}
