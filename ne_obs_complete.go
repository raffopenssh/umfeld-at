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
	computed                             int // rows (re)computed in the last pass
	kgsNoObs                             int
	kgsMissing, cellsMissing, cellsExtra int
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
	ident := v.Identity + "/" + e.Obs.Digest
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
	row := &neObsCompleteRow{KG: kg, ViewN: nv, ObsN: no, ViewSrc: v.Source, ObsAt: e.Obs.BuiltAt, ObsDecl: e.Obs.DeclaredSource, identity: ident, computedAt: time.Now()}
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
	neObsComplete.mu.Unlock()
	return row
}

// neObsCompletePass — every KG once; yields to renders between KGs.
func neObsCompletePass() {
	t0 := time.Now()
	boxes := map[string]string{}
	for _, b := range kgBoxSnapshot() {
		boxes[b.code] = fmt.Sprintf("%.4f,%.4f,%.4f,%.4f", b.minLon, b.minLat, b.maxLon, b.maxLat)
	}
	var noObs, kgsMissing, cellsMissing, cellsExtra, computed int
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
	}
	neObsComplete.mu.Lock()
	for kg := range neObsComplete.rows {
		if !seen[kg] {
			delete(neObsComplete.rows, kg)
		}
	}
	neObsComplete.passAt, neObsComplete.passMS, neObsComplete.passes = time.Now(), time.Since(t0).Milliseconds(), neObsComplete.passes+1
	neObsComplete.kgsNoObs, neObsComplete.kgsMissing, neObsComplete.cellsMissing, neObsComplete.cellsExtra, neObsComplete.computed = noObs, kgsMissing, cellsMissing, cellsExtra, computed
	neObsComplete.mu.Unlock()
}

// neObsCompleteLoop — first pass a few minutes after start (after the declared
// warm-up), then every neObsCompleteEvery. Started from main.
func neObsCompleteLoop() {
	time.Sleep(4 * time.Minute)
	for {
		neObsCompletePass()
		time.Sleep(neObsCompleteEvery)
	}
}

// neObsMissingRows — the snapshot's KGs with missing cells, most missing first
// (ties by code). Copies, safe to hand out.
func neObsMissingRows() []neObsCompleteRow {
	neObsComplete.mu.Lock()
	out := make([]neObsCompleteRow, 0, 64)
	for _, r := range neObsComplete.rows {
		if r.MissingN > 0 {
			out = append(out, *r)
		}
	}
	neObsComplete.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].MissingN != out[j].MissingN {
			return out[i].MissingN > out[j].MissingN
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
		"pass_at": at, "pass_ms": neObsComplete.passMS, "passes": neObsComplete.passes, "computed_last_pass": neObsComplete.computed, "every_s": int(neObsCompleteEvery.Seconds())}
}
