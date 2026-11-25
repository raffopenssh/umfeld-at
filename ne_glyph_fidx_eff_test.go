package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fidxEffStage — a private fidx world: temp disk dir, one block gen in the manifest, no tree-height mode, a staged
// peer (content + gen per fetch) and a staged build that counts and can run a hook in the middle.
type fidxEffStage struct {
	t       *testing.T
	dir     string
	i, j    int
	content byte // what the peer holds for the bin
	down    bool // the peer does not answer
	builds  int
	fetches int
	mid     func() // runs inside the build, after the peer body was read
}

func newFidxEffStage(t *testing.T) *fidxEffStage {
	s := &fidxEffStage{t: t, dir: t.TempDir(), i: 473, j: 1890, content: 1} // block 118/472: no other test touches it (a refresh worker may still hold theirs)
	oldDir, oldPin, oldBuild, oldFetch := neFidxDiskDir, neTreeHPinned, neFidxBuildFn, neFidxSrtmFetchFn
	neFidxDiskDirSet(s.dir)
	neFidxLRU = newNELRU(48 << 20)
	neTreeHPinned = "0"
	neFidxSrtmEpochStore("2026-10")
	s.gen("g1")
	neFidxSrtmFetchFn = func(w, so, e, n float64) *neFidxSrtm {
		if bw, bs, _, _ := neFidxBox(s.i, s.j); w != bw || so != bs {
			return nil // a refresh worker left over from another test: not our bin
		}
		if s.down {
			return nil
		}
		s.fetches++
		v := &neFidxSrtm{epoch: "2026-10", gen: neFidxBlockGenRaw(s.i>>2, s.j>>2), ids: []uint64{7}, cols: map[string][]byte{"elev_mean": {s.content}, "not_read_by_the_build": {byte(s.fetches)}}}
		neFidxEffNote(s.i, s.j, v)
		return v
	}
	neFidxBuildFn = func(i, j int, st string) (*neFidxBin, error) {
		if i != s.i || j != s.j {
			return nil, fmt.Errorf("staged build: not the staged bin")
		}
		s.builds++
		w, so, e, n := neFidxBox(i, j)
		sr := neFidxSrtmFetchFn(w, so, e, n)
		bin := &neFidxBin{Cols: map[string][]byte{}, Hdr: neFidxHdr{State: st, I: i, J: j, N2K: []string{}}}
		if sr != nil {
			bin.effC, bin.effT = sr.effC, sr.effT
		}
		if s.mid != nil {
			s.mid()
		}
		return bin, nil
	}
	t.Cleanup(func() {
		neFidxBuildFn, neFidxSrtmFetchFn, neTreeHPinned = oldBuild, oldFetch, oldPin
		neFidxDiskDirSet(oldDir)
		neFidxBlocksMu.Lock()
		neFidxBlocksGen, neFidxBlocksLoaded = map[[2]int]string{}, false
		neFidxBlocksMu.Unlock()
		neFidxMu.Lock()
		neFidxFlight = map[string]*neFidxCall{}
		neFidxMu.Unlock()
	})
	return s
}

// gen — the manifest lists a new gen for the bin's block.
func (s *fidxEffStage) gen(g string) {
	neFidxBlocksMu.Lock()
	neFidxBlocksGen, neFidxBlocksLoaded = map[[2]int]string{{s.i >> 2, s.j >> 2}: g}, true
	neFidxBlocksMu.Unlock()
}

func (s *fidxEffStage) get() string {
	s.t.Helper()
	b, st, err := neFidxGet(s.i, s.j)
	if err != nil || b == nil {
		s.t.Fatalf("neFidxGet: %v", err)
	}
	bin, err := neFidxDecode(b)
	if err != nil || bin.Hdr.State != st {
		s.t.Fatalf("blob carries state %q, answered as %q (%v)", bin.Hdr.State, st, err)
	}
	n := 0
	neFidxMu.Lock()
	for k := range neFidxFlight {
		if strings.HasPrefix(k, "473/1890/") {
			n++
		}
	}
	neFidxMu.Unlock()
	if n != 0 {
		s.t.Fatalf("%d single-flight entries left behind", n)
	}
	return st
}

// files — bin blobs on disk, by state dir.
func (s *fidxEffStage) files() []string {
	m, _ := filepath.Glob(filepath.Join(s.dir, "*", "*.zst"))
	return m
}

// The state moves in the middle of a build and later comes back (2026-11-25, b1ab64d: the single-flight entry of
// the state the build STARTED with was never removed; once the state returned to it every neFidxGet answered the
// finished call's bytes with no LRU or disk entry behind them, and the veil stayed 'indexing · N bins left').
// Staged with the peer not answering: the bin then has no body to be labelled by and takes the state at its end.
func TestFidxStateMovesMidBuildFlightNotLeaked(t *testing.T) {
	s := newFidxEffStage(t)
	s.down = true
	st0 := neFidxState(s.i, s.j)
	s.mid = func() { s.gen("g2") } // the manifest moved while we were building
	st := s.get()                  // get() fails on a flight entry left behind
	s.mid = nil
	if st == st0 || st != neFidxState(s.i, s.j) {
		t.Fatalf("the bin is filed under the state at the END of its build: got %s, started %s, now %s", st, st0, neFidxState(s.i, s.j))
	}
	if _, _, ok, _ := neFidxPeekL(s.i, s.j, time.Now().UnixNano()); !ok {
		t.Fatal("the moved bin must be a hit for the tile check")
	}
	// the state comes back to the one the first build started with: nothing is filed there — this must BUILD,
	// not hand back the earlier call's bytes under the other state
	s.gen("g1")
	if back := s.get(); back != st0 || s.builds != 2 {
		t.Fatalf("back on g1: state %s (want %s), builds %d (want 2)", back, st0, s.builds)
	}
	if _, _, ok, _ := neFidxPeekL(s.i, s.j, time.Now().UnixNano()); !ok {
		t.Fatal("a bin just answered by neFidxGet must be a hit for the tile check (pend stays > 0 otherwise)")
	}
	if len(s.files()) != 2 {
		t.Fatalf("one blob per state: %v", s.files())
	}
}

// A body parsed while a bin is building must not label it: the bin is filed under the content it was built FROM.
func TestFidxBodyParsedMidBuildDoesNotLabelBin(t *testing.T) {
	s := newFidxEffStage(t)
	s.mid = func() {
		// another request fetched the block again: new gen, new content, table updated
		s.gen("g2")
		s.content = 2
		w, so, e, n := neFidxBox(s.i, s.j)
		neFidxSrtmFetchFn(w, so, e, n)
	}
	st := s.get()
	s.mid = nil
	if st == neFidxState(s.i, s.j) {
		t.Fatal("a bin built from content 1 was filed under the state of content 2")
	}
	if st != neFidxStateBlk(s.i, s.j, "", "g1") {
		t.Fatalf("filed under %s, want the g1 state", st)
	}
	if st2 := s.get(); s.builds != 2 || st2 != neFidxState(s.i, s.j) || st2 == st {
		t.Fatalf("content 2 must be built: builds=%d", s.builds)
	}
}

// The churn of 2026-11-24: the peer rebuilds a block (new gen) to the same bytes for this bin, again and again.
// One build, one file, one state — until the bytes really change.
func TestFidxBlockRegeneratedSameBytesKeepsBinState(t *testing.T) {
	s := newFidxEffStage(t)
	st1 := s.get()
	if s.builds != 1 || len(s.files()) != 1 {
		t.Fatalf("builds=%d files=%v", s.builds, s.files())
	}
	box1 := neFidxBoxState(neFidxBox(s.i, s.j))
	spared := neFidxEffSpared.Load()
	for _, g := range []string{"g2", "g3", "g4", "g5"} {
		s.gen(g)
		if neFidxState(s.i, s.j) == st1 {
			t.Fatalf("%s: before the body is parsed the content is unknown — the state must move", g)
		}
		if st := s.get(); st != st1 {
			t.Fatalf("%s: same bytes, state %s want %s", g, st, st1)
		}
		if neFidxState(s.i, s.j) != st1 {
			t.Fatalf("%s: state did not settle on the old one", g)
		}
		if _, _, ok, _ := neFidxPeekL(s.i, s.j, time.Now().UnixNano()); !ok {
			t.Fatalf("%s: tile check misses", g)
		}
	}
	if s.builds != 1 || len(s.files()) != 1 || neFidxEffSpared.Load()-spared != 4 {
		t.Fatalf("4 regenerations to the same bytes: builds=%d (want 1) files=%d (want 1) spared=%d (want 4)", s.builds, len(s.files()), neFidxEffSpared.Load()-spared)
	}
	// the box state a veil tile is keyed on sees through the regeneration once all 16 bins of the block are known
	for a := 0; a < 4; a++ {
		for c := 0; c < 4; c++ {
			if i, j := s.i&^3+a, s.j&^3+c; i != s.i || j != s.j {
				neFidxEffNote(i, j, &neFidxSrtm{gen: "g1", cols: map[string][]byte{}})
				neFidxEffNote(i, j, &neFidxSrtm{gen: "g5", cols: map[string][]byte{}})
			}
		}
	}
	w, so, e, n := neFidxBox(s.i, s.j)
	boxA := neFidxBoxState(neFidxBox(s.i, s.j))
	s.gen("g6")
	for a := 0; a < 4; a++ {
		for c := 0; c < 4; c++ {
			if i, j := s.i&^3+a, s.j&^3+c; i != s.i || j != s.j {
				neFidxEffNote(i, j, &neFidxSrtm{gen: "g6", cols: map[string][]byte{}})
			}
		}
	}
	if neFidxBoxState(neFidxBox(s.i, s.j)) == boxA {
		t.Fatal("one bin of the block still unknown at g6: the box state must have moved")
	}
	neFidxSrtmFetchFn(w, so, e, n)
	if boxB := neFidxBoxState(neFidxBox(s.i, s.j)); boxB != boxA {
		t.Fatalf("box state moved on a regeneration to the same bytes: %s → %s (first %s)", boxA, boxB, box1)
	}
	// new bytes: a new state, a build, and the table survives a restart
	s.gen("g7")
	s.content = 9
	st7 := s.get()
	if st7 == st1 || s.builds != 2 {
		t.Fatalf("new bytes must build under a new state: st=%s builds=%d", st7, s.builds)
	}
	neFidxEffSave()
	neFidxEffReset()
	if neFidxState(s.i, s.j) != st7 {
		t.Fatal("a restart (table reloaded from bin_eff.json) moved the state")
	}
	if _, err := os.Stat(filepath.Join(s.dir, "bin_eff.json")); err != nil {
		t.Fatal(err)
	}
}

// The table is fed by the real block fetch: an (empty) block served under two gens leaves its bins on the first.
func TestFidxEffFedByBlockFetch(t *testing.T) {
	s := newFidxEffStage(t)
	neFidxSrtmDown.set(time.Time{})
	neFidxSrtmNoBlock.set(time.Time{})
	hdr := []byte(`{"bins":[{"i":473,"j":1890,"off":0,"len":0}]}`)
	body := append([]byte("NEXB"), byte(len(hdr)), 0, 0, 0)
	body = append(body, hdr...)
	gen := "g1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Query().Get("bbox"), "11.800,47.200") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-Cells-Epoch", "2026-10")
		w.Header().Set("X-Cells-Gen", gen)
		w.Write(body)
	}))
	defer srv.Close()
	pc := peers()
	pc.ObsAPI = srv.URL
	setPeersForTest(pc)
	defer setPeersForTest(peerConf{PublicURL: "https://umfeld.example", PrivatePrefix: "/k", ObsName: "obs-peer", ObsAPI: "https://obs.example:8100", ObsWeb: "https://obs.example", SubName: "sub-peer", SubAPI: "https://sub.example", MapName: "map-peer"})
	if ok, _ := neFidxSrtmBlockGet(118, 472, false); !ok {
		t.Fatal("block fetch")
	}
	st1 := neFidxState(s.i, s.j)
	gen = "g2"
	s.gen("g2")
	if neFidxState(s.i, s.j) == st1 {
		t.Fatal("unknown content: the state must move with the manifest")
	}
	if ok, _ := neFidxSrtmBlockGet(118, 472, false); !ok {
		t.Fatal("block fetch 2")
	}
	if neFidxState(s.i, s.j) != st1 || neFidxBinBlk(s.i, s.j, "") != "g1" {
		t.Fatalf("same bytes under g2: blk=%q", neFidxBinBlk(s.i, s.j, ""))
	}
	if v, ok := neFidxSrtmBlk.get("473/1890"); !ok || v.gen != "g2" || v.effC != "g1" {
		t.Fatal("the parsed body carries its own gen and the effective one")
	}
}
