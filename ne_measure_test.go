package main

// TestNEMeasureDeclaredView — operator measurement on the real data dir
// (NE_MEASURE_DIR=data/ne_cells go test -tags fts5 -run TestNEMeasureDeclaredView -timeout 1h).
// Read-only: the index DB is opened mode=ro, nothing is written. Writes a JSON
// summary to NE_MEASURE_OUT (default /tmp/ne_measure.json).

import (
	"database/sql"
	"encoding/json"
	"os"
	"sort"
	"testing"
)

func neFindRowOK(rows []byte, id uint64) ([]byte, bool) {
	r := neFindRow(rows, neLURowBytes, id)
	return r, r != nil
}

func TestNEMeasureDeclaredView(t *testing.T) {
	dir := os.Getenv("NE_MEASURE_DIR")
	if dir == "" {
		t.Skip("NE_MEASURE_DIR unset")
	}
	neDirOverride(dir)
	db, err := sql.Open("sqlite3", neIndexDBPath()+"?mode=ro&_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	neStore.db = db
	n := neStore.load()
	t.Logf("loaded %d KGs", n)
	type kgKey struct {
		Bev, Obs, Log, View bool
		ObsDecl             string
	}
	byKey := map[kgKey]int{}
	var (
		kgViewed, cellsView, cellsSame, cellsDiff, cellsViewOnly, cellsLuOnlyInLoggedChunk, cellsLuOnlyOutside int
		cellsBevOnlyInLoggedChunk, supHeldByNeighbour, supOrphaned, supOrphanedKGs                             int
		supByKG                                                                                                = map[string]int{}
		kgDiff                                                                                                 int
		diffByKG                                                                                               = map[string]int{}
		obsKG, obsRows, obsNotInView, viewNoObs, obsKGwithGap                                                  int
		obsOnIndex, obsOnIndexWithLog, obsOnIndexGapCells, obsOnIndexGapKGs                                    int
		bevOlderThanLog                                                                                        int
	)
	for _, kg := range neStore.kgCodes() {
		k := neStore.get(kg)
		if k == nil {
			continue
		}
		key := kgKey{Bev: k.Hdr.Layers["bev"] != nil, Obs: k.Hdr.Layers["obs"] != nil}
		if _, err := os.Stat(neChangeLogPath(kg)); err == nil {
			key.Log = true
		}
		d := neDeclStateOf(kg)
		key.View = d != nil
		if oh := k.layerHeader("obs"); oh != nil {
			key.ObsDecl, _ = oh["declared_source"].(string)
		}
		byKey[key]++
		if !key.Bev && !key.View {
			continue
		}
		kgViewed++
		after := neDeclaredAfter(k)
		if d != nil && after != "" && d.newest > after {
			bevOlderThanLog++
		}
		// per res-9 chunk: view rows (chg → bev → lu per cell)
		c9s := map[uint64]int{}
		for i := range k.Hdr.Dir {
			c9s[k.Hdr.Dir[i].c9] = i
		}
		if d != nil {
			for c9 := range d.by9 {
				if _, ok := c9s[c9]; !ok {
					c9s[c9] = -1
				}
			}
		}
		view := map[uint64][]byte{}
		luAll := map[uint64][]byte{}
		loggedChunks := map[uint64]bool{}
		var supCand []uint64 // bev/lu cells inside logged chunks the log lacks
		for c9, di := range c9s {
			var lu, bev, chg []byte
			if di >= 0 {
				if k.Hdr.Layers["lu"] != nil {
					lu, _ = neStore.chunkRows(k, di, "lu")
				}
				if k.Hdr.Layers["bev"] != nil {
					bev, _ = neStore.chunkRows(k, di, "bev")
				}
			}
			chg = neDeclaredLogRows(kg, c9, after)
			for i := 0; i+neLURowBytes <= len(chg); i += neLURowBytes {
				id := neRowID(chg, i/neLURowBytes, neLURowBytes)
				view[id] = chg[i : i+neLURowBytes]
				loggedChunks[h3Parent(id, 10)] = true
			}
			var logged []uint64
			if chg != nil {
				logged = neDeclaredLoggedChunks(kg, c9, after)
			}
			for i := 0; i+neLURowBytes <= len(bev); i += neLURowBytes {
				id := neRowID(bev, i/neLURowBytes, neLURowBytes)
				if _, ok := view[id]; !ok {
					if neLoggedHas(logged, id) {
						cellsBevOnlyInLoggedChunk++
						supCand = append(supCand, id)
						continue
					}
					view[id] = bev[i : i+neLURowBytes]
				}
			}
			for i := 0; i+neLURowBytes <= len(lu); i += neLURowBytes {
				id := neRowID(lu, i/neLURowBytes, neLURowBytes)
				luAll[id] = lu[i : i+neLURowBytes]
				if _, ok := view[id]; !ok && neLoggedHas(logged, id) {
					if _, inBev := neFindRowOK(bev, id); !inBev {
						supCand = append(supCand, id)
					}
				}
			}
		}
		// superseded candidates: served by a neighbour KG's view, or orphaned (flagged superseded)
		if len(supCand) > 0 {
			sortU64(supCand)
			res := neCollect(nil, supCand, []string{"lu"}, &neReq{public: true, format: "bin", bevFirst: true})
			orph := 0
			for i, id := range res.ids {
				if res.lu[i][11]&neFlagSuperseded != 0 {
					orph++
				} else {
					supHeldByNeighbour++
				}
				_ = id
			}
			supOrphaned += orph
			if orph > 0 {
				supOrphanedKGs++
				supByKG[kg] = orph
			}
		}
		kgd := 0
		for id, row := range view {
			cellsView++
			lrow, ok := luAll[id]
			if !ok {
				cellsViewOnly++
				continue
			}
			if neDominantClass(row) == neDominantClass(lrow) {
				cellsSame++
			} else {
				cellsDiff++
				kgd++
			}
		}
		for id := range luAll {
			if _, ok := view[id]; ok {
				continue
			}
			if loggedChunks[h3Parent(id, 10)] {
				cellsLuOnlyInLoggedChunk++
			} else {
				cellsLuOnlyOutside++
			}
		}
		if kgd > 0 {
			kgDiff++
			diffByKG[kg] = kgd
		}
		// obs vs view set: a view cell = view row, or a lu cell outside logged chunks (fallthrough)
		if l := k.Hdr.Layers["obs"]; l != nil {
			obsKG++
			rows, _, err := neStore.layerRows(k, "obs")
			if err != nil {
				continue
			}
			obsIDs := map[uint64]bool{}
			for i := 0; i < l.Rows; i++ {
				obsIDs[neRowID(rows, i, l.RowBytes)] = true
			}
			obsRows += len(obsIDs)
			set := map[uint64]bool{}
			for id := range view {
				set[id] = true
			}
			for id := range luAll {
				if !loggedChunks[h3Parent(id, 10)] {
					set[id] = true
				}
			}
			gap := 0
			for id := range obsIDs {
				if !set[id] {
					gap++
				}
			}
			miss := 0
			for id := range set {
				if !obsIDs[id] {
					miss++
				}
			}
			obsNotInView += gap
			viewNoObs += miss
			if gap+miss > 0 {
				obsKGwithGap++
			}
			if key.ObsDecl == "index-2026-03" {
				obsOnIndex++
				if key.Log {
					obsOnIndexWithLog++
				}
				if gap+miss > 0 {
					obsOnIndexGapKGs++
					obsOnIndexGapCells += gap + miss
				}
			}
		}
	}
	type kv struct {
		KG string
		N  int
	}
	var top []kv
	for kg, n := range diffByKG {
		top = append(top, kv{kg, n})
	}
	sort.Slice(top, func(i, j int) bool { return top[i].N > top[j].N })
	if len(top) > 20 {
		top = top[:20]
	}
	var topSup []kv
	for kg, n := range supByKG {
		topSup = append(topSup, kv{kg, n})
	}
	sort.Slice(topSup, func(i, j int) bool { return topSup[i].N > topSup[j].N })
	if len(topSup) > 20 {
		topSup = topSup[:20]
	}
	keys := []map[string]interface{}{}
	for key, n := range byKey {
		keys = append(keys, map[string]interface{}{"bev": key.Bev, "obs": key.Obs, "log": key.Log, "view": key.View, "obs_declared_source": key.ObsDecl, "kgs": n})
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i]["kgs"].(int) > keys[j]["kgs"].(int) })
	out := map[string]interface{}{
		"kgs_loaded": n, "kgs_by_key": keys,
		"kgs_with_view_or_bev": kgViewed, "kgs_bev_older_than_log": bevOlderThanLog,
		"cells_view": cellsView, "cells_dominant_same_as_lu": cellsSame, "cells_dominant_differs_from_lu": cellsDiff,
		"cells_view_only_not_in_lu": cellsViewOnly, "cells_lu_only_inside_logged_chunk": cellsLuOnlyInLoggedChunk,
		"cells_lu_only_outside_logged_chunks": cellsLuOnlyOutside,
		"cells_bev_only_inside_logged_chunk":  cellsBevOnlyInLoggedChunk,
		"superseded_held_by_neighbour_kg":     supHeldByNeighbour, "superseded_orphaned": supOrphaned, "superseded_orphaned_kgs": supOrphanedKGs, "top_superseded_kgs": topSup,
		"kgs_with_dominant_diff": kgDiff, "top_diff_kgs": top,
		"obs_kgs": obsKG, "obs_rows": obsRows, "obs_rows_not_in_view_set": obsNotInView, "view_cells_without_obs": viewNoObs, "obs_kgs_with_gap": obsKGwithGap,
		"obs_on_index_kgs": obsOnIndex, "obs_on_index_kgs_with_log": obsOnIndexWithLog, "obs_on_index_gap_kgs": obsOnIndexGapKGs, "obs_on_index_gap_cells": obsOnIndexGapCells,
		"chunk_lru": neStore.chunkLRU.stats(), "change_lru": NEChangeStats(),
	}
	b, _ := json.MarshalIndent(out, "", " ")
	p := os.Getenv("NE_MEASURE_OUT")
	if p == "" {
		p = "/tmp/ne_measure.json"
	}
	os.WriteFile(p, b, 0o644)
	t.Log(string(b))
}
