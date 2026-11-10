package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
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
