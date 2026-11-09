// api_ne_revisit.go — GET /contrib/api/v1/ne/revisit: read-only revisit hints
// for NE peers (docs/ne-change.md §"Revisit hints").
//
// Change is confirmed only when the same digest_ap is sighted twice. Peers pick
// KGs by their own schedule, so candidate versions sit pending for days and
// thousands of KGs hold a baseline nobody re-observed. This endpoint lists, from
// state we already hold, which KGs a peer should observe FIRST:
//
//	pending — KGs with chunk versions seen once (status candidate: not the
//	          baseline, not pipeline/jitter, lossy builds never enter the log),
//	          oldest first. Derived from {kg}.changes.jsonl (the delta rows the
//	          chunk log already produces, refreshed by neChangeSummaryOf).
//	stale   — KGs whose chunk log is not fully re-sighted: at least one chunk
//	          holds a single-sighting latest version (chunks_single > 0) and
//	          nothing is pending — the second pass is missing or partial there.
//	          Most single chunks first (then oldest baseline). Derived from the log's sightings roll-up in
//	          the summary line of {kg}.changes.jsonl (neChangeSightings) — NOT
//	          from the reports walk: a chunk-bearing report flagged
//	          layout_change (aligned build vs a legacy baseline) and an
//	          unchanged:true re-post both confirm chunks in the log, which the
//	          reports walk cannot see (01205, 2026-10-25…28: 109 chunks
//	          confirmed in the log, "reports:1 stale" every night).
//
// Nothing is written; the baseline, the comparison rule and the stored data are
// untouched. Memoised with StatsMemo (10 min, stale-while-revalidate), lists
// bounded by the universe (neRevisitMax), size on the debug inflight page.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	neRevisitMax     = 8000 // rows kept per list = the ?limit= ceiling (≥ the KG universe: ?all=1 is the whole list)
	neRevisitDefault = 100
	neRevisitTTL     = 10 * time.Minute
	neRevisitMemoKey = "ne_revisit"
)

// neRevisitPending — one KG with candidate (single-sighting) chunk versions.
type neRevisitPending struct {
	KG              string `json:"kg"`
	PendingVersions int    `json:"pending_versions"`
	PendingCells    int    `json:"pending_cells,omitempty"`
	CandidateM2     int    `json:"candidate_m2"`
	OldestSince     string `json:"oldest_since"`
	NewestSince     string `json:"newest_since"`
	LastObserver    string `json:"last_observer"`
	BaselineSource  string `json:"baseline_source"` // source of the version the oldest pending one is compared against
	BBox            string `json:"bbox,omitempty"`  // "w,s,e,n" of the last chunk-bearing report: cover at least this box so every pending chunk is in your report (since v0.3.3 any bbox is the same layout; a narrower one just omits edge chunks)
	observers       map[string]bool
}

// neRevisitStale — one KG whose chunk log was not re-sighted (see header).
type neRevisitStale struct {
	KG              string            `json:"kg"`
	BaselineAt      string            `json:"baseline_at"` // `since` of the log: when its first chunk rows arrived
	Reports         int               `json:"reports"`     // chunk-bearing, non-lossy reports stored (informational)
	LastObserver    string            `json:"last_observer"`
	LastAt          string            `json:"last_at"` // newest version/seen entry of the log
	Chunks          int               `json:"chunks"`
	ChunksConfirmed int               `json:"chunks_confirmed"` // chunks whose latest version was sighted twice
	ChunksSingle    int               `json:"chunks_single"`    // chunks whose latest version was sighted once — the missing second pass
	BBox            string            `json:"bbox,omitempty"`   // bbox of the last chunk-bearing report (same hint as pending)
	byObserver      map[string]string // observer → newest entry (the ?observer= filter)
}

// neKGReportStat — per-KG facts the reports walk collects for the revisit
// lists (api_ne_reports.go neScanReportFiles): chunk-bearing, non-lossy
// reports, plus the newest report of ANY kind per observer.
type neKGReportStat struct {
	ChunkReports int
	FirstAt      string
	LastAt       string
	LastObserver string
	LastBBox     string            // neBBoxKey of the last chunk-bearing report: the box a revisit should cover
	ByObserver   map[string]string // observer → received_at of its newest report (any kind, digest-only included)
	// Depth[n] = chunks of this KG whose deepest pass is exactly n distinct
	// observation days (n = 1..neEverDepthMax, the last bucket open-ended);
	// Chunks = their sum. The revisit `next_pass` list is derived from this —
	// the same walk, no extra disk read.
	Depth  [neEverDepthMax + 1]int
	Chunks int
}

// neRevisitNextPass — one KG whose chunks have not all reached `pass` distinct
// observation days (the reports walk's per-KG depth histogram, any depth —
// the chunk log stops counting at the second sighting, so passes ≥ 3 live
// here only). pass = the KG's shallowest chunk + 1: the visit that lifts every
// chunk of the KG by one. Ordered pass asc (finish the 2nd pass nationally
// before the 3rd), chunks_below desc, oldest visit first — so the list is a
// work queue a peer can take from the top with a fixed daily budget.
type neRevisitNextPass struct {
	KG           string            `json:"kg"`
	Pass         int               `json:"pass"`
	Chunks       int               `json:"chunks"`
	ChunksBelow  int               `json:"chunks_below"` // chunks with fewer than `pass` passes
	LastAt       string            `json:"last_at"`
	LastObserver string            `json:"last_observer"`
	BBox         string            `json:"bbox,omitempty"`
	byObserver   map[string]string // the ?observer= filter
}

// neRevisitNoLog — a KG without a chunk log: the declared picture there is
// still the index epoch; one report with chunk rows gives it a baseline.
type neRevisitNoLog struct {
	KG           string            `json:"kg"`
	HasObs       bool              `json:"has_obs"`
	Reports      int               `json:"reports"` // chunk-bearing non-lossy reports seen (none produced a log)
	LastReportAt string            `json:"last_report_at,omitempty"`
	LastObserver string            `json:"last_observer,omitempty"`
	BBox         string            `json:"bbox,omitempty"` // the KG's universe bbox; any aligned build is the same layout, so a digest-only earlier report with another bbox does not matter
	byObserver   map[string]string // observer → newest report (the ?observer= filter)
}

// neRevisitObsOnIndex — a KG whose observed layer was validated against the
// index epoch's cell set (bare NEO1 before the declared view existed).
type neRevisitObsOnIndex struct {
	KG         string `json:"kg"`
	ObsAt      string `json:"obs_at"`
	HasView    bool   `json:"has_view"` // a chunk-log/bev view exists → GET /{kg}/declared is the set to re-post on
	ViewSource string `json:"view_source,omitempty"`
	BBox       string `json:"bbox,omitempty"`
}

var neRevisitSize = struct {
	sync.Mutex
	pending, stale, noLog, obsOnIndex, nextPass int
	computeMS                                   int64
}{}

// neRevisitDeltaRow — the few fields of a derived delta row this needs.
type neRevisitDeltaRow struct {
	Cell   string `json:"cell"`
	Chunk  string `json:"chunk"`
	Status string `json:"status"`
	DLU    [9]int `json:"d_lu_m2"`
	From   struct {
		Source string `json:"source"`
	} `json:"from"`
	To struct {
		AP       string `json:"digest_ap"`
		At       string `json:"at"`
		Observer string `json:"observer"`
	} `json:"to"`
}

// neRevisitPendingOf — the pending record of one KG from its derived delta
// rows, nil when nothing is pending. A version counts when it is the LATEST
// version of its chunk (a superseded candidate cannot be confirmed any more)
// and at least one of its delta rows is a plain candidate.
func neRevisitPendingOf(kg string) (*neRevisitPending, *neChangeSummary) {
	sum, ok := neChangeSummaryOf(kg) // refreshes the derived file when the log grew
	if !ok {
		return nil, nil
	}
	f, err := os.Open(neFilePath(kg, neChangeDerived))
	if err != nil {
		return nil, &sum
	}
	defer f.Close()
	type ver struct {
		at, observer, src string
		cells, m2         int
		candidate         bool
	}
	latest := map[string]*ver{} // chunk → latest version seen in the rows
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	first := true
	for sc.Scan() {
		if first { // summary line
			first = false
			continue
		}
		var r neRevisitDeltaRow
		if json.Unmarshal(sc.Bytes(), &r) != nil || r.Chunk == "" {
			continue
		}
		v := latest[r.Chunk]
		if v == nil || r.To.At > v.at {
			v = &ver{at: r.To.At, observer: r.To.Observer, src: r.From.Source}
			latest[r.Chunk] = v
		} else if r.To.At < v.at {
			continue // an older version of this chunk
		}
		if r.Status != "candidate" {
			continue
		}
		v.candidate = true
		v.cells++
		for _, d := range r.DLU {
			if d > 0 {
				v.m2 += d
			}
		}
	}
	p := &neRevisitPending{KG: kg, observers: map[string]bool{}}
	for _, v := range latest {
		if !v.candidate {
			continue
		}
		p.PendingVersions++
		p.PendingCells += v.cells
		p.CandidateM2 += v.m2
		p.observers[v.observer] = true
		if p.OldestSince == "" || v.at < p.OldestSince {
			p.OldestSince, p.BaselineSource = v.at, v.src
		}
		if v.at > p.NewestSince {
			p.NewestSince, p.LastObserver = v.at, v.observer
		}
	}
	if p.PendingVersions == 0 {
		return nil, &sum
	}
	return p, &sum
}

// neRevisitCompute — the memo body: every KG with a chunk log, bounded lists.
func neRevisitCompute() map[string]interface{} {
	t0 := time.Now()
	var pending []*neRevisitPending
	var stale []neRevisitStale
	kgStats := neReportKGStats()
	var chunksSingle, chunksConfirmed int // across every log: the national second-pass figure
	for _, kg := range neChangeKGs() {
		p, sum := neRevisitPendingOf(kg)
		if sum != nil {
			chunksSingle += sum.Sightings.ChunksSingle
			chunksConfirmed += sum.Sightings.ChunksConfirmed
		}
		if p != nil {
			p.BBox = kgStats[kg].LastBBox
			pending = append(pending, p)
			continue
		}
		if sum == nil || sum.Sightings.ChunksSingle == 0 {
			continue // nothing logged, or every chunk of the log's latest picture is re-sighted
		}
		stale = append(stale, neRevisitStale{KG: kg, BaselineAt: sum.Since, Reports: kgStats[kg].ChunkReports,
			LastObserver: sum.Sightings.LastObserver, LastAt: sum.Sightings.LastAt, Chunks: sum.Chunks,
			ChunksConfirmed: sum.Sightings.ChunksConfirmed, ChunksSingle: sum.Sightings.ChunksSingle,
			BBox: kgStats[kg].LastBBox, byObserver: sum.Sightings.ByObserver})
	}
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].OldestSince != pending[j].OldestSince {
			return pending[i].OldestSince < pending[j].OldestSince
		}
		return pending[i].KG < pending[j].KG
	})
	totalPending, totalM2 := 0, 0
	for _, p := range pending {
		totalPending += p.PendingVersions
		totalM2 += p.CandidateM2
	}
	kgsPending := len(pending)
	if len(pending) > neRevisitMax {
		pending = pending[:neRevisitMax]
	}
	sort.Slice(stale, func(i, j int) bool { // most missing second-pass chunks first, then oldest baseline
		if stale[i].ChunksSingle != stale[j].ChunksSingle {
			return stale[i].ChunksSingle > stale[j].ChunksSingle
		}
		if stale[i].BaselineAt != stale[j].BaselineAt {
			return stale[i].BaselineAt < stale[j].BaselineAt
		}
		return stale[i].KG < stale[j].KG
	})
	kgsStale := len(stale)
	if len(stale) > neRevisitMax {
		stale = stale[:neRevisitMax]
	}
	// no_log / obs_on_index (docs/ne-change.md §5b): from the store's KG list
	// and the manifest's per-KG headers — no file walk of its own.
	logged := map[string]bool{}
	for _, kg := range neChangeKGs() {
		logged[kg] = true
	}
	// next_pass: every logged KG whose chunks are not all at the same depth as
	// the deepest national pass — from the reports walk's depth histogram
	var nextPass []neRevisitNextPass
	for kg, ks := range kgStats {
		if !logged[kg] || ks.Chunks == 0 {
			continue
		}
		lo := 1
		for lo < neEverDepthMax && ks.Depth[lo] == 0 {
			lo++
		}
		if lo >= neEverDepthMax { // every chunk at the open-ended top bucket: nothing to ask for
			continue
		}
		nextPass = append(nextPass, neRevisitNextPass{KG: kg, Pass: lo + 1, Chunks: ks.Chunks, ChunksBelow: ks.Depth[lo],
			LastAt: ks.LastAt, LastObserver: ks.LastObserver, BBox: ks.LastBBox, byObserver: ks.ByObserver})
	}
	sort.Slice(nextPass, func(i, j int) bool {
		a, b := nextPass[i], nextPass[j]
		if a.Pass != b.Pass {
			return a.Pass < b.Pass
		}
		if a.ChunksBelow != b.ChunksBelow {
			return a.ChunksBelow > b.ChunksBelow
		}
		if a.LastAt != b.LastAt {
			return a.LastAt < b.LastAt
		}
		return a.KG < b.KG
	})
	kgsNextPass := len(nextPass)
	passTarget := 0
	if len(nextPass) > 0 {
		passTarget = nextPass[0].Pass
	}
	if len(nextPass) > neRevisitMax {
		nextPass = nextPass[:neRevisitMax]
	}
	// national chunks by depth (≥ k) from the same walk
	ever, _ := neScanReportFiles()["chunks_ever"].(map[string]int)
	byPass := map[string]int{"1": ever["once"]}
	for k := 2; k <= neEverDepthMax; k++ {
		if n := ever["pass_"+strconv.Itoa(k)+"_or_more"]; n > 0 {
			byPass[strconv.Itoa(k)] = n
		}
	}
	boxes := map[string]string{}
	for _, b := range kgBoxSnapshot() {
		boxes[b.code] = fmt.Sprintf("%.4f,%.4f,%.4f,%.4f", b.minLon, b.minLat, b.maxLon, b.maxLat)
	}
	var noLog []neRevisitNoLog
	var obsIdx []neRevisitObsOnIndex
	luSource, _ := neFrozen["lu_source"].(string)
	for _, kg := range neStore.kgCodes() {
		e := neEntry(kg)
		if !logged[kg] {
			ks := kgStats[kg]
			row := neRevisitNoLog{KG: kg, HasObs: e.Obs != nil, Reports: ks.ChunkReports, BBox: boxes[kg], byObserver: ks.ByObserver}
			for obs, at := range ks.ByObserver {
				if at > row.LastReportAt {
					row.LastReportAt, row.LastObserver = at, obs
				}
			}
			noLog = append(noLog, row)
		}
		if e.Obs != nil && (e.Obs.DeclaredSource == luSource || e.Obs.DeclaredSource == "") {
			row := neRevisitObsOnIndex{KG: kg, ObsAt: e.Obs.BuiltAt, BBox: boxes[kg]}
			if logged[kg] {
				row.HasView = true
				row.ViewSource = "chunks"
			} else if e.Bev != nil {
				row.HasView, row.ViewSource = true, e.Bev.Source
			}
			obsIdx = append(obsIdx, row)
		}
	}
	sort.Slice(noLog, func(i, j int) bool { // KGs with an observed layer first (their obs waits on a declared set), then by code
		if noLog[i].HasObs != noLog[j].HasObs {
			return noLog[i].HasObs
		}
		return noLog[i].KG < noLog[j].KG
	})
	sort.Slice(obsIdx, func(i, j int) bool { // KGs that already have a view first: re-posting there needs no new report
		if obsIdx[i].HasView != obsIdx[j].HasView {
			return obsIdx[i].HasView
		}
		return obsIdx[i].KG < obsIdx[j].KG
	})
	kgsNoLog, kgsObsIdx := len(noLog), len(obsIdx)
	if len(noLog) > neRevisitMax {
		noLog = noLog[:neRevisitMax]
	}
	if len(obsIdx) > neRevisitMax {
		obsIdx = obsIdx[:neRevisitMax]
	}
	neRevisitSize.Lock()
	neRevisitSize.pending, neRevisitSize.stale, neRevisitSize.noLog, neRevisitSize.obsOnIndex, neRevisitSize.nextPass = len(pending), len(stale), len(noLog), len(obsIdx), len(nextPass)
	neRevisitSize.computeMS = time.Since(t0).Milliseconds()
	neRevisitSize.Unlock()
	return map[string]interface{}{"pending": pending, "stale": stale, "kgs_pending": kgsPending, "kgs_stale": kgsStale,
		"pending_versions": totalPending, "candidate_m2": totalM2, "chunks_single": chunksSingle, "chunks_confirmed": chunksConfirmed,
		"no_log": noLog, "obs_on_index": obsIdx, "kgs_no_log": kgsNoLog, "kgs_obs_on_index": kgsObsIdx,
		"next_pass": nextPass, "kgs_next_pass": kgsNextPass, "pass_target": passTarget, "chunks_by_pass": byPass}
}

// NERevisitStats — size line for the debug inflight page (Invariant 10).
func NERevisitStats() map[string]interface{} {
	neRevisitSize.Lock()
	defer neRevisitSize.Unlock()
	age, ok := StatsMemoAge(neRevisitMemoKey)
	return map[string]interface{}{"pending_rows": neRevisitSize.pending, "stale_rows": neRevisitSize.stale,
		"no_log_rows": neRevisitSize.noLog, "obs_on_index_rows": neRevisitSize.obsOnIndex, "next_pass_rows": neRevisitSize.nextPass, "max_rows": neRevisitMax,
		"last_compute_ms": neRevisitSize.computeMS, "memo_age_s": int(age.Seconds()), "memo_present": ok, "ttl_s": int(neRevisitTTL.Seconds())}
}

// handleNERevisit — GET /api/v1/ne/revisit (contributor or private token; the
// public tier never sees it).
//
//	?limit=<n>      rows per list (default 100, max neRevisitMax); ?all=1 = the whole list
//	?kind=a,b       only these lists (pending, stale, next_pass, no_log, obs_on_index, obs_missing); default all
//	?observer=<me>  per-peer view: drops pending KGs whose every pending version
//	                I posted myself, and stale / next_pass / no_log KGs I reported within
//	                min_age_days (my own re-post inside neSamePassWindow is not a
//	                sighting; after it the log confirms and the KG leaves by itself)
//	?min_age_days=  (default 7) stale: baseline older than this; with ?observer=
//	                also the "I was here recently" window on stale and no_log
//	?refresh=1      recompute the snapshot synchronously
//	?kg=<kg>        with kind=obs_missing and refresh=1: recompute that one KG's
//	                observed-vs-view row now (answer gains obs_kg{}), no 3 h wait
//	?pass=1         with kind=obs_missing: kick a full obs_complete pass in the
//	                background now (obs_pass_started true/false; obs_complete.running)
//
// ETag = hash of the body; If-None-Match → 304 (a nightly sweep re-reads the
// full no_log list at zero bytes when nothing moved).
func handleNERevisit(w http.ResponseWriter, r *http.Request) {
	if _, ok := neReportGate(w, r); !ok {
		return
	}
	q := r.URL.Query()
	limit := neRevisitDefault
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		limit = min(n, neRevisitMax)
	}
	if q.Get("all") == "1" {
		limit = neRevisitMax
	}
	minAge := 7.0
	if v, err := strconv.ParseFloat(q.Get("min_age_days"), 64); err == nil && v >= 0 {
		minAge = v
	}
	observer := q.Get("observer")
	kinds := map[string]bool{}
	if k := q.Get("kind"); k != "" {
		for _, x := range strings.Split(k, ",") {
			kinds[strings.TrimSpace(x)] = true
		}
	}
	want := func(k string) bool { return len(kinds) == 0 || kinds[k] }
	m := StatsMemo(neRevisitMemoKey, neRevisitTTL, q.Get("refresh") == "1", neRevisitCompute)
	cutoff := time.Now().Add(-time.Duration(minAge * float64(24*time.Hour))).UTC().Format(time.RFC3339)
	recent := func(by map[string]string) bool { // the asking observer was here within min_age_days
		return observer != "" && by[observer] > cutoff
	}
	out := map[string]interface{}{
		"generated_at":     m["stats_computed_at"],
		"totals":           map[string]interface{}{"kgs_pending": m["kgs_pending"], "pending_versions": m["pending_versions"], "candidate_m2": m["candidate_m2"], "kgs_stale": m["kgs_stale"], "chunks_single": m["chunks_single"], "chunks_confirmed": m["chunks_confirmed"], "kgs_next_pass": m["kgs_next_pass"], "pass_target": m["pass_target"], "chunks_by_pass": m["chunks_by_pass"], "kgs_no_log": m["kgs_no_log"], "kgs_obs_on_index": m["kgs_obs_on_index"], "kgs_obs_missing": NEObsCompleteStats()["kgs_obs_missing"], "cells_obs_missing": NEObsCompleteStats()["cells_obs_missing"], "kgs_obs_blank": NEObsCompleteStats()["kgs_obs_blank"], "cells_obs_blank": NEObsCompleteStats()["cells_obs_blank"], "kgs_obs_nodata": NEObsCompleteStats()["kgs_obs_nodata"], "cells_obs_nodata": NEObsCompleteStats()["cells_obs_nodata"]},
		"limit":            limit,
		"min_age_days":     minAge,
		"observer":         observer,
		"same_pass_window": neSamePassWindow.String(),
		"note": "Hints only, oldest first. pending = KGs holding chunk versions sighted once (candidate: not baseline, not pipeline/jitter/coverage; lossy builds are never logged) — a second sighting of the same digest_ap by another observer, or by the same one after same_pass_window, confirms them. " +
			"stale = KGs whose chunk log is not fully re-sighted (chunks_single > 0: chunks whose latest version was sighted once, nothing pending; most chunks_single first) — the second pass is missing or partial there; one report with chunk rows covering bbox by another observer, or by the same one after same_pass_window, confirms them. totals.chunks_single / chunks_confirmed is the same figure over every log. next_pass = the standing work queue for every later pass: KGs whose chunks have not all reached `pass` distinct observation days (pass = the KG's shallowest chunk + 1; chunks_below = chunks still under it), ordered pass asc (the nation finishes its 2nd pass before its 3rd), chunks_below desc, oldest visit first — take from the top with your daily budget, one report with chunk rows covering bbox lifts the whole KG by one pass; totals.pass_target = the lowest pass not complete nationally, totals.chunks_by_pass = chunks with ≥ k passes. Cheapest poll: ?kind=next_pass&observer=<me>&limit=<your daily KGs> with If-None-Match. no_log = KGs without any chunk log yet (declared picture still the index epoch; one report with chunk rows gives them a baseline — a digest-only earlier report is not a layout, chunk rows are requested at once). obs_on_index = KGs whose observed layer was validated against the index epoch's cell set: re-post NEO1 built on GET {kg}/declared (has_view) or NEC1+NEO1. obs_missing = KGs whose observed layer lacks cells of today's declared view (missing_n), holds rows the view no longer has (extra_n), or holds unswept rows that observe nothing (blank_n: obs_lu all 0, h_mean nodata (255), no flight year — elev_mean is not consulted; obs_complete.blank_rule states it — padding written for view cells the pipeline never ran on; they paint as unknown and count as unobserved) — the view moved since the NEO1 was validated (re-baseline, bevdirect edge fix, another observer's edge cells) or the rekey padded instead of observing: run the pipeline on every cell of GET {kg}/declared and re-post NEO1; most missing + extra + blank first, snapshot of a background pass every few hours (obs_complete.pass_at). Once a KG's sweep is closed, post it with the NEO1 header field view_rekey.coverage=\"swept\" (= every blank row in the body was run and has no ALS data: open water, seams): its blank rows then count as nodata_n (totals cells_obs_nodata / kgs_obs_nodata), never as blank_n, and the KG is listed only while missing_n or extra_n > 0 — an identical re-post is never asked for. coverage rides on each row and on manifest obs{coverage, view_rekey, blank_n, nodata_n}. Each row carries declared_etag = the ETag GET {kg}/declared answers for the view the row was computed on: send it as If-None-Match, a 304 means the view has not moved since this snapshot and your stored copy is current — no refetch, no HEAD. GET {kg}/report likewise answers an ETag and takes ?observer=&since=<your last received_at>&limit=N; nothing on these paths needs an unconditional hourly fetch. Order: pending, stale, next_pass, no_log, obs_on_index, obs_missing, then your normal order. ?observer=<me> hides what I reported within min_age_days; ?kind=no_log&all=1 is the whole list (ETag, 304). Nothing here changes baselines or the confirm rule; values are a snapshot ≤ 10 min old (?refresh=1 recomputes; ?kind=obs_missing&kg=<kg>&refresh=1 recomputes that KG's obs_missing row now → obs_kg{}, and manifest obs{blank_n, nodata_n} follow).",
	}
	if len(kinds) == 1 { // a filtered poll is a daily habit: the prose rides only on the full answer
		delete(out, "note")
		out["docs"] = "/api/v1/docs/ne-change.md"
	}
	if want("pending") {
		all, _ := m["pending"].([]*neRevisitPending)
		pending := make([]*neRevisitPending, 0, min(limit, len(all)))
		for _, p := range all {
			if observer != "" && len(p.observers) == 1 && p.observers[observer] {
				continue
			}
			pending = append(pending, p)
			if len(pending) == limit {
				break
			}
		}
		out["pending"] = pending
	}
	if want("stale") {
		all, _ := m["stale"].([]neRevisitStale)
		stale := make([]neRevisitStale, 0, min(limit, len(all)))
		for _, s := range all {
			if s.BaselineAt > cutoff || recent(s.byObserver) {
				continue
			}
			stale = append(stale, s)
			if len(stale) == limit {
				break
			}
		}
		out["stale"] = stale
	}
	if want("next_pass") {
		all, _ := m["next_pass"].([]neRevisitNextPass)
		np := make([]neRevisitNextPass, 0, min(limit, len(all)))
		for _, s := range all {
			if recent(s.byObserver) {
				continue
			}
			np = append(np, s)
			if len(np) == limit {
				break
			}
		}
		out["next_pass"] = np
	}
	if want("no_log") {
		all, _ := m["no_log"].([]neRevisitNoLog)
		noLog := make([]neRevisitNoLog, 0, min(limit, len(all)))
		for _, s := range all {
			if recent(s.byObserver) {
				continue
			}
			noLog = append(noLog, s)
			if len(noLog) == limit {
				break
			}
		}
		out["no_log"] = noLog
	}
	if want("obs_on_index") {
		all, _ := m["obs_on_index"].([]neRevisitObsOnIndex)
		out["obs_on_index"] = all[:min(limit, len(all))]
	}
	if want("obs_missing") { // ne_obs_complete.go: its own background pass, not the 10-min memo
		if kg := q.Get("kg"); kg != "" && q.Get("refresh") == "1" {
			if row := neObsRecomputeKG(kg); row != nil {
				out["obs_kg"] = row
			} else {
				out["obs_kg"] = nil
			}
		}
		if q.Get("pass") == "1" {
			out["obs_pass_started"] = neObsCompleteRequestPass()
		}
		all := neObsMissingRows()
		out["obs_missing"] = all[:min(limit, len(all))]
		out["obs_complete"] = NEObsCompleteStats()
	}
	body, err := json.Marshal(out)
	if err != nil {
		http.Error(w, "encode", 500)
		return
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:12]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache") // always revalidate; the ETag makes that free
	if strings.Contains(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(200)
	_, _ = w.Write(body)
}
