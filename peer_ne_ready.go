package main

// Peer "observed layer" truth for the landing-page counter.
//
// {OBS_NAME} POSTs its observed layer (NEO1) to <private>/api/v1/ne/{kg}/obs hours
// BEFORE its own /api/v1/cells serves that KG (metered ingest, ~15 MB/h). So
// "KGs with an .obs here" (manifest obs_posted) overstates what a reader can
// actually fetch from the peer: on 2026-10-07 we held 1 712 .obs while srtm
// served 1 495 (217 "v2.4-pending"). The peer's /api/v1/kgs row field
// `ne_ready` is authoritative (its invariant: ne_ready ⇔ /api/v1/cells serves
// the KG completely; never compare product_version strings — some v1/v2.3
// rows are ne_ready:true). We harvest it from the same ~2 MB fetch the KG
// universe check already makes (CheckKGUniversePeer) and the public counter
// reports "live at {OBS_NAME}" = |our .obs ∩ peer ne_ready|, with the rest
// as "posted, awaiting publication" — pending means poll, not gap.
//
// Memory: one map of ≤ 7 850 five-byte strings (bounded by the universe),
// size on /api/v1/debug/inflight via KGUniverseStats (peer_ne_ready).

import (
	"sync"
	"time"
)

type peerNEState struct {
	CheckedAt      time.Time
	Reachable      bool
	Ready          map[string]bool // kg → ne_ready:true
	ServedReported int             // meta.kgs_ne_served as the peer reports it
	CellsGen       string          // meta.cells_generation
	ManifestGen    string          // meta.manifest_generation
}

var (
	peerNEMu     sync.RWMutex
	peerNEState_ *peerNEState
)

func setPeerNEState(s *peerNEState) {
	peerNEMu.Lock()
	// Keep the last reachable snapshot when a refresh fails: a counter that
	// drops to "unverified" for an hour because of one timeout is noise.
	if s != nil && !s.Reachable && peerNEState_ != nil && peerNEState_.Reachable {
		peerNEMu.Unlock()
		return
	}
	peerNEState_ = s
	peerNEMu.Unlock()
}

// PeerNEState — latest harvested snapshot (nil until the first peer check ran).
func PeerNEState() *peerNEState {
	peerNEMu.RLock()
	defer peerNEMu.RUnlock()
	return peerNEState_
}

// peerNEServedSplit — how many of our posted-obs KGs the peer actually serves.
// ok=false when no reachable snapshot exists (caller must word the counter as
// "posted", not "live").
func peerNEServedSplit(obsKGs []string) (served, pending int, st *peerNEState, ok bool) {
	st = PeerNEState()
	if st == nil || !st.Reachable {
		return 0, 0, st, false
	}
	for _, kg := range obsKGs {
		if st.Ready[kg] {
			served++
		} else {
			pending++
		}
	}
	return served, pending, st, true
}
