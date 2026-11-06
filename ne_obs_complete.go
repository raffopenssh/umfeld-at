// ne_obs_complete.go — is the observed layer complete? Per KG the observed
// layer (NEO1, validated at POST time against the declared layer of *that
// day*) is compared with the declared VIEW of today (log → bev → lu,
// ne_change_declared.go): cells of the view without an observed row are
// `missing`, observed rows for cells the view no longer holds are `extra`.
// Both drift with every re-baseline, bevdirect fix (v0.3.6 edge cells) or
// observer that disagrees on a KG's edge — a few cells per KG, never land use.
//
// Cost: one merge of two sorted id lists per KG (view rows from the chunk LRU,
// observed rows inflated once); a national pass is ~2 min on two cores the
// first time and seconds afterwards — per KG the result is memoised under
// (view identity, obs digest) and only recomputed when either moved. The pass
// runs in the background (neObsCompleteLoop, yields to tile renders), never
// inside a request; readers get the last snapshot.
//
// Exposed: /ne/revisit kind `obs_missing` (peers: KGs to re-post NEO1 on the
// current view, worst first), totals on the revisit answer, size line
// `ne_obs_complete` on debug/inflight.
package main

import (
	"encoding/binary"
	"fmt"
	"sort"
	"sync"
	"time"
)

type neObsCompleteRow struct {
	KG         string `json:"kg"`
	ViewN      int    `json:"view_n"`
	ObsN       int    `json:"obs_n"`
	MissingN   int    `json:"missing_n"`
	ExtraN     int    `json:"extra_n"`
	BlankN     int    `json:"blank_n"`            // observed rows that observe nothing: obs_lu all zero, h_mean nodata, no flight — placeholders the peer's view_rekey filled in for view cells its pipeline never ran on (80002: 23 269 of 380 837, 2026-11-04). 0 when the post is swept.
	NodataN    int    `json:"nodata_n"`           // the same rows under a post whose header asserts view_rekey.coverage="swept": the pipeline ran, the ALS DTM/DSM has no data there (open water — Lake Constance, Neusiedler See — and border seams). Not a gap; never listed for re-post on its own.
	Coverage   string `json:"coverage,omitempty"` // "swept" when the stored NEO1 header carries view_rekey.coverage="swept"
	ViewSrc    string `json:"view_source"`
	ObsAt      string `json:"obs_at"`
	ObsDecl    string `json:"obs_declared_source,omitempty"`
	BBox       string `json:"bbox,omitempty"`
	identity   string // view identity + obs digest the row was computed for
	computedAt time.Time
}

var neObsComplete struct {
	mu                                   sync.Mutex
	rows                                 map[string]*neObsCompleteRow
	passAt                               time.Time
	passMS                               int64
	passes                               int
	revision                             int // bumps on every (re)computed row — the manifest keys obs{blank_n, nodata_n} on it
	computed                             int // rows (re)computed in the last pass
	kgsNoObs                             int
	kgsMissing, cellsMissing, cellsExtra int
	kgsBlank, cellsBlank                 int
	kgsNodata, cellsNodata               int
}

// neObsCoverageSwept — the one value of the NEO1 header field
// view_rekey.coverage: the observer ran its pipeline on every cell of the
// body and the blank rows are ALS nodata, not padding.
const neObsCoverageSwept = "swept"

// neObsCoverageOf — view_rekey.coverage of an NEO1 header ("" when absent).
func neObsCoverageOf(h map[string]interface{}) string {
	vr, _ := h["view_rekey"].(map[string]interface{})
	c, _ := vr["coverage"].(string)
	return c
}

const neObsCompleteEvery = 3 * time.Hour

// neObsCompareKG — merge-compare the view with the observed rows. nil when the
// KG has no observed layer or no view.
func neObsCompareKG(kg string) *neObsCompleteRow {
	k := neStore.getOrConvert(kg)
	if k == nil {
		return nil
	}
	e := neEntry(kg)
	if e.Obs == nil {
		return nil
	}
	v := neDeclViewOf(kg)
	if v == nil {
		return nil
	}
	ident := v.Identity + "/" + e.Obs.Digest + "/" + e.Obs.Coverage
	neObsComplete.mu.Lock()
	if r := neObsComplete.rows[kg]; r != nil && r.identity == ident {
		neObsComplete.mu.Unlock()
		return r
	}
	neObsComplete.mu.Unlock()
	l := k.Hdr.Layers["obs"]
	if l == nil || l.RowBytes < 8 {
		return nil
	}
	obs, _, err := neStore.layerRows(k, "obs")
	if err != nil {
		return nil
	}
	if !neRowsSorted(obs, l.RowBytes) {
		obs = neSortRows(obs, l.RowBytes)
	}
	rb := l.RowBytes
	nv, no := v.CellsN(), len(obs)/rb
	row := &neObsCompleteRow{KG: kg, ViewN: nv, ObsN: no, ViewSrc: v.Source, ObsAt: e.Obs.BuiltAt, ObsDecl: e.Obs.DeclaredSource, Coverage: e.Obs.Coverage, identity: ident, computedAt: time.Now()}
	rec := ""
	if h := k.layerHeader("obs"); h != nil {
		rec, _ = h["record"].(string)
	}
	lay := neObsLayout(neObsFields(k), rec)
	blank := 0
	for j := 0; lay != nil && j < no && lay.rowBytes == rb; j++ {
		if neObsRowBlank(obs[j*rb:(j+1)*rb], lay) {
			blank++
		}
	}
	if row.Coverage == neObsCoverageSwept {
		row.NodataN = blank // asserted run-and-empty: ALS nodata, not padding
	} else {
		row.BlankN = blank
	}
	i, j := 0, 0
	for i < nv && j < no {
		a := binary.LittleEndian.Uint64(v.Rows[i*neLURowBytes:])
		b := binary.LittleEndian.Uint64(obs[j*rb:])
		switch {
		case a == b:
			i++
			j++
		case a < b:
			row.MissingN++
			i++
		default:
			row.ExtraN++
			j++
		}
	}
	row.MissingN += nv - i
	row.ExtraN += no - j
	neObsComplete.mu.Lock()
	if neObsComplete.rows == nil {
		neObsComplete.rows = map[string]*neObsCompleteRow{}
	}
	neObsComplete.rows[kg] = row
	neObsComplete.revision++
	neObsComplete.mu.Unlock()
	return row
}

// neObsRecomputeKG — one KG now, outside the 3 h pass (/ne/revisit
// ?kind=obs_missing&kg=<kg>&refresh=1): the memo decides whether anything
// moved (view identity, obs digest, coverage); the pass totals stay as they
// were until the next pass, the row and manifest obs{blank_n, nodata_n}
// follow at once. nil when the KG has no observed layer or no view.
func neObsRecomputeKG(kg string) *neObsCompleteRow {
	r := neObsCompareKG(kg)
	if r == nil {
		return nil
	}
	neObsComplete.mu.Lock()
	cp := *r
	neObsComplete.mu.Unlock()
	return &cp
}

// neObsCompletePass — every KG once; yields to renders between KGs.
func neObsCompletePass() {
	t0 := time.Now()
	boxes := map[string]string{}
	for _, b := range kgBoxSnapshot() {
		boxes[b.code] = fmt.Sprintf("%.4f,%.4f,%.4f,%.4f", b.minLon, b.minLat, b.maxLon, b.maxLat)
	}
	var noObs, kgsMissing, cellsMissing, cellsExtra, kgsBlank, cellsBlank, kgsNodata, cellsNodata, computed int
	seen := map[string]bool{}
	for _, kg := range neStore.kgCodes() {
		neRenderYield()
		neObsComplete.mu.Lock()
		before := neObsComplete.rows[kg]
		neObsComplete.mu.Unlock()
		r := neObsCompareKG(kg)
		if r == nil {
			noObs++
			continue
		}
		seen[kg] = true
		if before != r {
			computed++
		}
		r.BBox = boxes[kg]
		if r.MissingN > 0 {
			kgsMissing++
		}
		cellsMissing += r.MissingN
		cellsExtra += r.ExtraN
		if r.BlankN > 0 {
			kgsBlank++
		}
		cellsBlank += r.BlankN
		if r.NodataN > 0 {
			kgsNodata++
		}
		cellsNodata += r.NodataN
	}
	neObsComplete.mu.Lock()
	for kg := range neObsComplete.rows {
		if !seen[kg] {
			delete(neObsComplete.rows, kg)
		}
	}
	neObsComplete.passAt, neObsComplete.passMS, neObsComplete.passes = time.Now(), time.Since(t0).Milliseconds(), neObsComplete.passes+1
	neObsComplete.kgsNoObs, neObsComplete.kgsMissing, neObsComplete.cellsMissing, neObsComplete.cellsExtra, neObsComplete.computed = noObs, kgsMissing, cellsMissing, cellsExtra, computed
	neObsComplete.kgsBlank, neObsComplete.cellsBlank = kgsBlank, cellsBlank
	neObsComplete.kgsNodata, neObsComplete.cellsNodata = kgsNodata, cellsNodata
	neObsComplete.mu.Unlock()
}

// neObsCompleteKick — /ne/revisit?kind=obs_missing&pass=1 asks the loop for
// a full pass now (coalesced: one pending kick at a time, never two passes).
var neObsCompleteKick = make(chan struct{}, 1)

var neObsCompleteRunning bool // under neObsComplete.mu

// neObsCompleteRequestPass — start a full pass now unless one is running;
// reports whether the kick was accepted.
func neObsCompleteRequestPass() bool {
	select {
	case neObsCompleteKick <- struct{}{}:
		return true
	default:
		return false
	}
}

// neObsCompleteLoop — first pass a few minutes after start (after the declared
// warm-up), then every neObsCompleteEvery or on a kick. Started from main.
func neObsCompleteLoop() {
	for first := true; ; first = false {
		wait := neObsCompleteEvery
		if first {
			wait = 4 * time.Minute
		}
		select {
		case <-time.After(wait):
		case <-neObsCompleteKick:
		}
		neObsComplete.mu.Lock()
		neObsCompleteRunning = true
		neObsComplete.mu.Unlock()
		neObsCompletePass()
		neObsComplete.mu.Lock()
		neObsCompleteRunning = false
		neObsComplete.mu.Unlock()
	}
}

// neObsBlankRule — the blank-row rule as peers read it (revisit
// obs_complete.blank_rule, docs/ne-change.md §5b). The observed-layer peer
// mirrors it when it decides which rows a sweep must still run (its commit
// 820e626); keep the string and neObsRowBlank in step.
const neObsBlankRule = "blank = obs_lu[9] all 0 && h_mean == 255 (nodata) && flight_years == 0 (when the layout has it); elev_mean and every other field are ignored"

// neObsRowBlank — an observed row that observes nothing: every obs_lu share 0,
// h_mean nodata (255) and no flight year (neObsBlankRule). elev_mean is NOT
// consulted: the DTM has a height almost everywhere, padding rows carry one
// too. A real cell of bare rock still has an h_mean and a flight; these rows
// are the padding a peer's view_rekey writes for view cells it never ran its
// pipeline on.
func neObsRowBlank(row []byte, lay *neObsLayoutT) bool {
	for _, b := range row[lay.luOff : lay.luOff+9] {
		if b != 0 {
			return false
		}
	}
	if lay.hMean >= 0 && row[lay.hMean] != 255 {
		return false
	}
	if lay.flight >= 0 && row[lay.flight] != 0 {
		return false
	}
	return true
}

// neObsRowWantsRepost — a KG belongs on the obs_missing list when the view
// drifted (missing or extra cells) or the post still holds unswept blank
// padding. Swept nodata rows alone never list a KG: re-posting an identical
// body changes nothing (the ~35 k open-water and seam cells, 2026-11-06).
func neObsRowWantsRepost(r *neObsCompleteRow) bool {
	return r.MissingN > 0 || r.ExtraN > 0 || r.BlankN > 0
}

// neObsMissingRows — the snapshot's KGs that want a re-post, most
// (missing + extra + blank) first (ties by code). Copies, safe to hand out.
func neObsMissingRows() []neObsCompleteRow {
	neObsComplete.mu.Lock()
	out := make([]neObsCompleteRow, 0, 64)
	for _, r := range neObsComplete.rows {
		if neObsRowWantsRepost(r) {
			out = append(out, *r)
		}
	}
	neObsComplete.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].MissingN+out[i].ExtraN+out[i].BlankN, out[j].MissingN+out[j].ExtraN+out[j].BlankN
		if a != b {
			return a > b
		}
		return out[i].KG < out[j].KG
	})
	return out
}

// NEObsCompleteStats — totals for debug/inflight and the revisit answer.
func NEObsCompleteStats() map[string]interface{} {
	neObsComplete.mu.Lock()
	defer neObsComplete.mu.Unlock()
	at := ""
	if !neObsComplete.passAt.IsZero() {
		at = neObsComplete.passAt.UTC().Format(time.RFC3339)
	}
	return map[string]interface{}{"rows": len(neObsComplete.rows), "kgs_no_obs": neObsComplete.kgsNoObs, "kgs_obs_missing": neObsComplete.kgsMissing,
		"cells_obs_missing": neObsComplete.cellsMissing, "cells_obs_extra": neObsComplete.cellsExtra,
		"kgs_obs_blank": neObsComplete.kgsBlank, "cells_obs_blank": neObsComplete.cellsBlank,
		"kgs_obs_nodata": neObsComplete.kgsNodata, "cells_obs_nodata": neObsComplete.cellsNodata,
		"blank_rule": neObsBlankRule, "pass_at": at, "pass_ms": neObsComplete.passMS, "passes": neObsComplete.passes, "revision": neObsComplete.revision, "running": neObsCompleteRunning, "computed_last_pass": neObsComplete.computed, "every_s": int(neObsCompleteEvery.Seconds())}
}

// neObsCompleteCounts — blank_n / nodata_n of the last pass for one KG
// (manifest.obs) and when that row was (re)computed (manifest obs.counts_at;
// bumps updated_at so a ?since= delta lists a recount — the view can move
// without the obs file moving); ok=false before the pass has seen it.
func neObsCompleteCounts(kg string) (blank, nodata int, at time.Time, ok bool) {
	neObsComplete.mu.Lock()
	defer neObsComplete.mu.Unlock()
	r := neObsComplete.rows[kg]
	if r == nil {
		return 0, 0, time.Time{}, false
	}
	return r.BlankN, r.NodataN, r.computedAt, true
}
