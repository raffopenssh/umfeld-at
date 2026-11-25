package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A rebuilt block at the peer (new gen, same epoch) moves the state of its 16
// bins only, drops the parsed block and queues a refresh of the bins we hold.
func TestFidxBlocksManifestGenMovesBinState(t *testing.T) {
	dir := t.TempDir()
	old := neFidxDiskDir
	t.Cleanup(func() { neFidxDiskDirSet(old) })
	neFidxDiskDirSet(dir)
	neFidxBlocksMu.Lock()
	neFidxBlocksGen, neFidxBlocksAll, neFidxBlocksETag, neFidxBlocksLoaded = map[[2]int]string{}, "", "", true
	neFidxBlocksMu.Unlock()

	gens := map[string]string{"117/469": "aaaa", "117/470": "bbbb"}
	etag := `"blocks-1"`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/cells/blocks" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(304)
			return
		}
		w.Header().Set("ETag", etag)
		m := map[string]interface{}{"epoch": "2026-10", "generation": etag, "blocks": map[string]interface{}{
			"bi": []int{117, 117}, "bj": []int{469, 470}, "gen": []string{gens["117/469"], gens["117/470"]}}}
		json.NewEncoder(w).Encode(m)
	}))
	defer srv.Close()
	pc := peers()
	pc.ObsAPI = srv.URL
	setPeersForTest(pc)
	defer setPeersForTest(peerConf{PublicURL: "https://umfeld.example", PrivatePrefix: "/k", ObsName: "obs-peer", ObsAPI: "https://obs.example:8100", ObsWeb: "https://obs.example", SubName: "sub-peer", SubAPI: "https://sub.example", MapName: "map-peer"})

	if err := neFidxBlocksPoll(); err != nil {
		t.Fatal(err)
	}
	if g := neFidxBlockGenOfBin(469, 1878); g != "aaaa" {
		t.Fatalf("gen of bin 469/1878 = %q, want aaaa", g)
	}
	st1, other1 := neFidxState(469, 1878), neFidxState(469, 1882) // 1882>>2 = 470
	// a parsed block in the LRU with the current gen is a hit, after the gen moves a miss
	neFidxSrtmBlk.put("469/1878", &neFidxSrtm{epoch: "2026-10", gen: "aaaa", cols: map[string][]byte{}})
	if _, ok := neFidxSrtmBlk.get("469/1878"); !ok {
		t.Fatal("block with the manifest gen should be a hit")
	}
	// a held bin: a blob on disk at the old state
	os.MkdirAll(filepath.Join(dir, st1), 0o755)
	os.WriteFile(filepath.Join(dir, st1, "469_1878.zst"), []byte("x"), 0o644)

	// 304 → nothing moves
	if err := neFidxBlocksPoll(); err != nil {
		t.Fatal(err)
	}
	if neFidxState(469, 1878) != st1 {
		t.Fatal("state moved on a 304")
	}
	gens["117/469"], etag = "cccc", `"blocks-2"`
	q0 := neFidxBlocksRefresh.Load()
	if err := neFidxBlocksPoll(); err != nil {
		t.Fatal(err)
	}
	if neFidxState(469, 1878) == st1 {
		t.Fatal("state of a bin in the rebuilt block did not move")
	}
	if neFidxState(469, 1882) != other1 {
		t.Fatal("state of a bin in an untouched block moved")
	}
	if _, ok := neFidxSrtmBlk.get("469/1878"); ok {
		t.Fatal("parsed block of the old gen still served")
	}
	if n := neFidxBlocksRefresh.Load() - q0; n != 1 { // the held bin, and only it (the pool may already have taken it)
		t.Fatalf("refresh builds queued = %d, want 1 (the held bin)", n)
	}
	// persisted: a fresh load sees the gens
	neFidxBlocksMu.Lock()
	neFidxBlocksGen, neFidxBlocksLoaded = map[[2]int]string{}, false
	neFidxBlocksMu.Unlock()
	if g := neFidxBlockGen(117, 469); g != "cccc" {
		t.Fatalf("persisted gen = %q, want cccc", g)
	}
}

// The peer serves a block's previous generation while it rebuilds it (200 +
// X-Cells-Stale: 1) although its manifest already lists the new gen: the bins
// keep the state of the served gen, the body stays valid in the block LRU, a
// recheck goes out with If-None-Match (304 = still rebuilding), and the first
// current answer moves the state to the manifest gen. 503 + Retry-After parks
// the block without blaming the peer.
func TestFidxBlockPreviousGenerationWhileRebuilding(t *testing.T) {
	dir := t.TempDir()
	old := neFidxDiskDir
	t.Cleanup(func() { neFidxDiskDirSet(old) })
	neFidxDiskDirSet(dir)
	reset := func() {
		neFidxPendMu.Lock()
		neFidxPend, neFidxBuilding = map[[2]int]*neFidxPendEnt{}, map[[2]int]time.Time{}
		neFidxPendMu.Unlock()
	}
	reset()
	t.Cleanup(reset)
	neFidxBlocksMu.Lock()
	neFidxBlocksGen, neFidxBlocksAll, neFidxBlocksETag, neFidxBlocksLoaded = map[[2]int]string{{117, 469}: "new1", {118, 469}: "g503"}, "x", "", true
	neFidxBlocksMu.Unlock()
	t.Cleanup(func() {
		neFidxBlocksMu.Lock()
		neFidxBlocksGen, neFidxBlocksLoaded = map[[2]int]string{}, false
		neFidxBlocksMu.Unlock()
	})
	neFidxSrtmDown.set(time.Time{})
	neFidxSrtmNoBlock.set(time.Time{})

	// a v1 NEXB body without bins: every bin of the block is empty, which is all the state logic needs
	hdr := []byte(`{"bins":[{"i":469,"j":1878,"off":0,"len":0}]}`)
	body := append([]byte("NEXB"), byte(len(hdr)), 0, 0, 0)
	body = append(body, hdr...)
	var mode atomic.Value // "stale" | "current"
	mode.Store("stale")
	var inm atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Query().Get("bbox"), "11.800") {
			w.Header().Set("Retry-After", "45")
			w.WriteHeader(503)
			w.Write([]byte(`{"building":true}`))
			return
		}
		w.Header().Set("X-Cells-Epoch", "2026-10")
		if mode.Load() == "stale" {
			w.Header().Set("X-Cells-Stale", "1")
			w.Header().Set("X-Cells-Gen", "old0")
			w.Header().Set("X-Cells-Gen-Current", "new1")
			w.Header().Set("Retry-After", "120")
			w.Header().Set("ETag", `"B-old0"`)
			if r.Header.Get("If-None-Match") == `"B-old0"` {
				inm.Add(1)
				w.WriteHeader(304)
				return
			}
		} else {
			w.Header().Set("X-Cells-Gen", "new1")
			w.Header().Set("ETag", `"B-new1"`)
		}
		w.Write(body)
	}))
	defer srv.Close()
	pc := peers()
	pc.ObsAPI = srv.URL
	setPeersForTest(pc)
	defer setPeersForTest(peerConf{PublicURL: "https://umfeld.example", PrivatePrefix: "/k", ObsName: "obs-peer", ObsAPI: "https://obs.example:8100", ObsWeb: "https://obs.example", SubName: "sub-peer", SubAPI: "https://sub.example", MapName: "map-peer"})

	neFidxSrtmEpochStore("2026-10")
	i, j := 469, 1878 // block 117/469
	stNew := neFidxState(i, j)
	if ok, _ := neFidxSrtmBlockGet(117, 469, false); !ok {
		t.Fatal("stale answer must be taken")
	}
	if !neFidxPendIs(117, 469) || neFidxBlockGen(117, 469) != "old0" || neFidxBlockGenRaw(117, 469) != "new1" {
		t.Fatalf("pending=%v gen=%q raw=%q", neFidxPendIs(117, 469), neFidxBlockGen(117, 469), neFidxBlockGenRaw(117, 469))
	}
	stOld := neFidxState(i, j)
	if stOld == stNew {
		t.Fatal("a bin of a block served in its previous generation must not carry the manifest-gen state")
	}
	if v, ok := neFidxSrtmBlk.get("469/1878"); !ok || !v.stale || v.gen != "old0" {
		t.Fatal("the previous-generation body must stay a hit while the block is pending")
	}
	// recheck: 304, still pending, nothing moves
	if ok, _ := neFidxSrtmBlockGet(117, 469, false); !ok || inm.Load() != 1 || neFidxState(i, j) != stOld {
		t.Fatalf("recheck: inm=%d", inm.Load())
	}
	if d := neFidxPendDue(8); len(d) != 0 {
		t.Fatalf("nothing is due inside Retry-After, got %v", d)
	}
	// the peer finished: current body → state = manifest gen, the stale entry is gone
	mode.Store("current")
	if ok, _ := neFidxSrtmBlockGet(117, 469, false); !ok || neFidxPendIs(117, 469) {
		t.Fatal("current answer must end pending")
	}
	// the current body holds the same bytes as the previous generation did (empty bins): the bin keeps the state
	// it had (ne_glyph_fidx_eff.go) — the manifest-gen state would only be a second name for the same blob
	if neFidxState(i, j) != stOld || neFidxBinBlk(i, j, "") != "old0" || stNew == stOld {
		t.Fatalf("same bytes in the current body: the state must stay, blk=%q", neFidxBinBlk(i, j, ""))
	}
	if v, ok := neFidxSrtmBlk.get("469/1878"); !ok || v.stale || v.gen != "new1" {
		t.Fatal("the current body must be in the LRU")
	}
	// 503 + Retry-After: parked for the block only, the peer is not marked down
	if ok, _ := neFidxSrtmBlockGet(118, 469, false); ok {
		t.Fatal("503 is not an answer")
	}
	if d := neFidxBuildingFor(118, 469); d < 40*time.Second || d > 45*time.Second {
		t.Fatalf("building for %s, want ~45s", d)
	}
	if time.Since(neFidxSrtmDown.get()) < 2*time.Minute {
		t.Fatal("a 503 for one block must not mark the peer down")
	}
	if neFidxSrtmFetch(11.8, 46.9, 11.825, 46.925) != nil {
		t.Fatal("no fetch for a block the peer is building")
	}
}

// The peer's debounce + content hash (2026-11-25, §8.1d): the manifest is read by served_gen, a held block (target
// gen moved, previous generation serves) moves nothing, a new served gen with the chash of our copy is taken over
// without a pull, and only a new chash drops the parsed block.
func TestFidxBlocksServedGenAndContentHash(t *testing.T) {
	dir := t.TempDir()
	old, oldPin := neFidxDiskDir, neTreeHPinned
	neFidxDiskDirSet(dir)
	neTreeHPinned = "0"
	reset := func() {
		neFidxBlocksMu.Lock()
		neFidxBlocksGen, neFidxBlocksCh, neFidxBlocksFH, neFidxBlocksAll, neFidxBlocksETag, neFidxBlocksLoaded = map[[2]int]string{}, map[[2]int]string{}, "", "", "", true
		neFidxBlocksMu.Unlock()
		neFidxPendMu.Lock()
		neFidxPend = map[[2]int]*neFidxPendEnt{}
		neFidxPendMu.Unlock()
	}
	reset()
	t.Cleanup(func() {
		reset()
		neFidxBlocksMu.Lock()
		neFidxBlocksLoaded, neFidxBlocksServed = false, false
		neFidxBlocksMu.Unlock()
		neTreeHPinned = oldPin
		neFidxDiskDirSet(old)
	})
	type row struct {
		gen, served, ch string
		hold            interface{}
	}
	cur := row{"g1", "g1", "c1", nil}
	var q atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q.Store(r.URL.RawQuery)
		json.NewEncoder(w).Encode(map[string]interface{}{"epoch": "2026-10", "generation": cur.gen + cur.served, "fh": "ab12", "blocks": map[string]interface{}{
			"bi": []int{116}, "bj": []int{468}, "gen": []string{cur.gen}, "served_gen": []string{cur.served}, "chash": []string{cur.ch}, "hold_until": []interface{}{cur.hold}}})
	}))
	defer srv.Close()
	pc := peers()
	pc.ObsAPI = srv.URL
	setPeersForTest(pc)
	defer setPeersForTest(peerConf{PublicURL: "https://umfeld.example", PrivatePrefix: "/k", ObsName: "obs-peer", ObsAPI: "https://obs.example:8100", ObsWeb: "https://obs.example", SubName: "sub-peer", SubAPI: "https://sub.example", MapName: "map-peer"})
	poll := func() {
		t.Helper()
		if err := neFidxBlocksPoll(); err != nil {
			t.Fatal(err)
		}
	}
	neFidxSrtmEpochStore("2026-10")
	i, j := 464, 1872 // block 116/468
	poll()
	if s, _ := q.Load().(string); s != "fields="+neFidxSrtmFlds {
		t.Fatalf("the manifest is asked for the field set we pull (chash is per field set), got %q", s)
	}
	v := &neFidxSrtm{epoch: "2026-10", gen: "g1", ids: []uint64{7}, cols: map[string][]byte{"elev_mean": {1}}}
	neFidxEffNote(i, j, v)
	neFidxSrtmBlk.put("464/1872", v)
	st1 := neFidxState(i, j)

	// held: the target moved, the previous generation serves — nothing to pull, nothing moves
	cur = row{"g2", "g1", "c1", float64(time.Now().Unix() + 1800)}
	ch0 := neFidxBlocksChanged.Load()
	poll()
	if neFidxState(i, j) != st1 || neFidxBlocksChanged.Load() != ch0 || neFidxBlockGenRaw(116, 468) != "g1" {
		t.Fatalf("a held block moved: raw=%q", neFidxBlockGenRaw(116, 468))
	}
	if NEFidxBlocksStats()["held_at_peer"] != 1 || !neFidxBlocksTellServed() {
		t.Fatalf("stats: %v", NEFidxBlocksStats())
	}
	// a stale answer on record for it (a fetch during the hold) goes when the manifest says the block flipped
	neFidxPendSet(116, 468, "g1", `"B-g1"`, false, neFidxRetryAfter("3600", true))

	// built to the same content: new served gen, same chash → our copy is the new body
	cur = row{"g2", "g2", "c1", nil}
	poll()
	if neFidxPendIs(116, 468) {
		t.Fatal("the manifest's served gen ends a pending entry")
	}
	if neFidxState(i, j) != st1 || neFidxBlocksChanged.Load() != ch0 || !neFidxEffCovers(i, j, "") {
		t.Fatal("same chash under a new gen: the state must stay and the table must cover the new gen without a fetch")
	}
	if got, ok := neFidxSrtmBlk.get("464/1872"); !ok || got.gen != "g2" || got.effC != "g1" || v.gen != "g1" {
		t.Fatal("the parsed bin is carried over to the new gen as a copy (readers of the old pointer see no write)")
	}

	// new content: the parsed block is dropped, the state is unknown until the pull
	cur = row{"g3", "g3", "c3", nil}
	poll()
	if _, ok := neFidxSrtmBlk.get("464/1872"); ok || neFidxState(i, j) == st1 || neFidxBlocksChanged.Load() != ch0+1 {
		t.Fatal("a new chash is a change")
	}

	// Retry-After: 10 min at most, but a hold is honoured in full (≤ 4 h 15 min)
	if d := neFidxRetryAfter("7200", false); d != 10*time.Minute {
		t.Fatalf("plain Retry-After clamp: %s", d)
	}
	if d := neFidxRetryAfter("7200", true); d != 2*time.Hour {
		t.Fatalf("hold Retry-After: %s", d)
	}
	if d := neFidxRetryAfter("99999", true); d != neFidxHoldMax {
		t.Fatalf("hold clamp: %s", d)
	}
}
