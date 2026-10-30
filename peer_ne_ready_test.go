package main

import (
	"strings"
	"testing"
	"time"
)

// The landing-page "observed layer" counter must report what the observed-layer peer
// SERVES (its ne_ready), never merely what it posted here.
func TestPeerNEServedSplit(t *testing.T) {
	prev := PeerNEState()
	defer func() { peerNEMu.Lock(); peerNEState_ = prev; peerNEMu.Unlock() }()

	peerNEMu.Lock()
	peerNEState_ = nil
	peerNEMu.Unlock()
	if _, _, _, ok := peerNEServedSplit([]string{"63349"}); ok {
		t.Fatal("no snapshot must report ok=false (counter wording: posted, unverified)")
	}

	good := &peerNEState{CheckedAt: time.Now(), Reachable: true, Ready: map[string]bool{"63349": true, "01004": true}}
	setPeerNEState(good)
	served, pending, _, ok := peerNEServedSplit([]string{"63349", "01004", "80110"})
	if !ok || served != 2 || pending != 1 {
		t.Fatalf("got served=%d pending=%d ok=%v, want 2/1/true", served, pending, ok)
	}
	// An unreachable refresh keeps the last reachable snapshot.
	setPeerNEState(&peerNEState{CheckedAt: time.Now(), Reachable: false, Ready: map[string]bool{}})
	if st := PeerNEState(); st != good {
		t.Fatal("unreachable refresh must not evict the last good snapshot")
	}
}

func TestNECoverageTextObservedWording(t *testing.T) {
	c := map[string]interface{}{"kgs_total": 7850, "built": 7394, "obs_posted": 1712, "chunks": 1, "cells": 1,
		"as_of": "x", "obs_peer_verified": true, "obs_served": 1495, "obs_pending": 217, "obs_peer_as_of": "13:00 UTC"}
	txt := neCoverageObsText(c)
	if !strings.Contains(txt, "1\u202f495 with an observed layer live at obs-peer") || !strings.Contains(txt, "217 more") {
		t.Fatalf("verified wording wrong: %s", txt)
	}
	c["obs_peer_verified"] = false
	if txt = neCoverageObsText(c); !strings.Contains(txt, "1\u202f712") || !strings.Contains(txt, "not verified") {
		t.Fatalf("unverified wording wrong: %s", txt)
	}
}
