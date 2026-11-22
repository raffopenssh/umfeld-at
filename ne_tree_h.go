package main

// Per-cell tree apex height lists (docs/ne-filter.md §9). The observed-layer
// peer's bins carry, when `tree_h` is in fields=, a ragged column pair:
// `tree_h_n` (u16, apices with h ≥ 3 m per cell — the trees layer's population)
// auto-inserted before `tree_h` (u8, 0.5 m units, lists back to back in row
// order, ascending within a cell). With it the filter's tree figure counts only
// the apices inside the height term (two binary searches per matching cell)
// instead of every apex of the matching cells.
//
// The column costs ≈ 1 B per apex: it is asked for only once the peer serves
// it prebuilt — a block that is still built on demand takes seconds per bin.
// neTreeHMode gates the field list: a probe once an hour fetches one block
// with the tree set under a short deadline; two fast answers in a row switch
// the mode on (the bins' state hash moves, so bins rebuild with lists on their
// next use); a slow or failed tree-set fetch in normal service switches it
// off for an hour. NE_TREE_H=0|1 pins the mode. Until lists are there the
// figure falls back to Σ tree_n (whole-cell count, shown as ≈ when a height
// term is set).

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// neTreeH — apex height lists of n cells: cell k's apices are H[Off[k]:Off[k+1]].
type neTreeH struct {
	Off []uint32 // n+1
	H   []byte   // 0.5 m units, ascending within a cell
}

// neTreeHFromCounts — offsets from u16 LE counts; nil when the lengths disagree.
func neTreeHFromCounts(counts []byte, h []byte) *neTreeH {
	if len(counts)%2 != 0 {
		return nil
	}
	n := len(counts) / 2
	off := make([]uint32, n+1)
	var acc uint32
	for k := 0; k < n; k++ {
		off[k] = acc
		acc += uint32(binary.LittleEndian.Uint16(counts[2*k:]))
	}
	off[n] = acc
	if int(acc) != len(h) {
		return nil
	}
	return &neTreeH{Off: off, H: h}
}

// counts — u16 LE per cell (the stored form: 3× smaller than offsets under zstd).
func (t *neTreeH) counts() []byte {
	n := len(t.Off) - 1
	out := make([]byte, 2*n)
	for k := 0; k < n; k++ {
		binary.LittleEndian.PutUint16(out[2*k:], uint16(min(t.Off[k+1]-t.Off[k], 65535)))
	}
	return out
}

// n — apices of cell k.
func (t *neTreeH) n(k int) int { return int(t.Off[k+1] - t.Off[k]) }

// count — apices of cell k with lo ≤ h ≤ hi (raw 0.5 m units).
func (t *neTreeH) count(k int, lo, hi int) int {
	a, b := int(t.Off[k]), int(t.Off[k+1])
	if a == b || lo > hi {
		return 0
	}
	l := t.H[a:b]
	i := 0
	if lo > 0 {
		i = sort.Search(len(l), func(x int) bool { return int(l[x]) >= lo })
	}
	j := len(l)
	if hi < 254 {
		j = sort.Search(len(l), func(x int) bool { return int(l[x]) > hi })
	}
	if j < i {
		return 0
	}
	return j - i
}

// reorder — the lists in a new row order (perm[k] = old row of new row k).
func (t *neTreeH) reorder(perm []int) *neTreeH {
	off := make([]uint32, len(perm)+1)
	h := make([]byte, 0, len(t.H))
	for k, p := range perm {
		off[k] = uint32(len(h))
		h = append(h, t.H[t.Off[p]:t.Off[p+1]]...)
	}
	off[len(perm)] = uint32(len(h))
	return &neTreeH{Off: off, H: h}
}

// neFiltApexRange — the apex height window of an expression in raw 0.5 m
// units: the union of its `ht` / `htmax` terms; ok=false when there is
// none (every apex of a matching cell counts). A bare `ht=unk` yields an empty
// window.
func neFiltApexRange(e *neFiltExpr) (lo, hi int, ok bool) {
	lo, hi = math.MaxInt32, math.MinInt32
	if e == nil { // an unfiltered unit (stats sample without f=)
		return lo, hi, false
	}
	for _, g := range e.Groups {
		for _, t := range g {
			fd := neFiltByID[t.F]
			if fd == nil || fd.colID() != "ht" {
				continue
			}
			ok = true
			if t.Unk {
				continue
			}
			a, b := neFiltBounds(fd, t)
			if a < lo {
				lo = a
			}
			if b > hi {
				hi = b
			}
		}
	}
	if !ok {
		return 0, 0, false
	}
	if lo < 1 {
		lo = 1
	}
	if hi > 254 {
		hi = 254
	}
	return lo, hi, true
}

// neTreeCounter — the per-cell tree figure of a store for an expression:
// exact from the lists (range-limited when a height term is set), else the
// whole-cell tn (u8, 255 = unknown). exact reports whether cell k was counted
// from a list.
type neTreeCounter struct {
	th     *neTreeH
	tn     []byte
	lo, hi int
	rng    bool
}

func neNewTreeCounter(th *neTreeH, tn []byte, e *neFiltExpr) *neTreeCounter {
	c := &neTreeCounter{th: th, tn: tn}
	c.lo, c.hi, c.rng = neFiltApexRange(e)
	return c
}

func (c *neTreeCounter) at(k int) (n int, exact bool) {
	if c.th != nil {
		if c.rng {
			return c.th.count(k, c.lo, c.hi), true
		}
		return c.th.n(k), true
	}
	if c.tn != nil && c.tn[k] != 255 {
		return int(c.tn[k]), false
	}
	return 0, false
}

// ── the gate: capability + asynchronous lists ───────────────────────────────
//
// The mode says only "the peer serves the column" (a probe, or the marker of
// the last run). Latency never switches it: a slow tree-set answer used to park
// the whole mode for an hour and flip every bin's state twice, although the
// peer was just as slow without the column (cold block: 34 s with, > 40 s
// without, 2026-11-22). Now every block's tree-set fetch is its own detached
// request (neTreeHStart): the foreground waits for it for neTreeHHedge, then
// goes on with the prebuilt set, and the lists land when the peer has them —
// the bins built meanwhile are stored under the list-less state and rebuilt
// the moment the block arrives. Blame is per block (backoff after a failed
// tree-set fetch), and the wait itself adapts: after three lost races in a row
// the foreground does not wait at all until a tree-set answer is fast again.

const (
	neTreeHField    = "tree_h"
	neTreeHProbeDL  = 60 * time.Second  // capability, not speed: a cold block may take the peer half a minute
	neTreeHLateDL   = 120 * time.Second // a detached tree-set fetch may take this long
	neTreeHLateMax  = 4                 // detached tree-set fetches in flight
	neTreeHBackMin  = 2 * time.Minute   // per-block backoff after a failed tree-set fetch, doubling …
	neTreeHBackMax  = 30 * time.Minute  // … up to this
	neTreeHBusyWait = 15 * time.Second  // no free slot: the block's lists are owed, ask again then
	neTreeHOffFor   = time.Hour         // the peer answered without the column: off, probe again later
	neTreeHProbeIv  = 15 * time.Minute
	neTreeHMapMax   = 4096
)

type neTreeHLate struct {
	done chan struct{}
	ok   bool
	t0   time.Time
}

type neTreeHBack struct {
	until time.Time
	n     int
}

// neTreeHHedge — foreground wait for the tree set before the prebuilt set is asked (a var: tests shorten it).
var neTreeHHedge = 2500 * time.Millisecond

var (
	neTreeHOn       atomic.Bool
	neTreeHOffUntil atomicTime
	neTreeHProbes   atomic.Int64
	neTreeHProbeOK  atomic.Int64
	neTreeHProbeMu  sync.Mutex
	neTreeHLastBlk  atomic.Value // string "bi/bj": the last block a user fetch asked for (the probe's target)
	neTreeHPinned   = os.Getenv("NE_TREE_H")

	neTreeHMu     sync.Mutex
	neTreeHFly    = map[string]*neTreeHLate{} // block "bi/bj" → detached tree-set fetch (≤ neTreeHLateMax)
	neTreeHBackM  = map[string]neTreeHBack{}  // block → lists owed until (≤ neTreeHMapMax)
	neTreeHWant   = map[[2]int]struct{}{}     // bins served without lists while owed: rebuilt when the block lands (≤ neTreeHMapMax)
	neTreeHSem    = make(chan struct{}, neTreeHLateMax)
	neTreeHLost   atomic.Int64 // consecutive races the tree set lost against the hedge
	neTreeHWon    atomic.Int64 // tree set answered inside the hedge
	neTreeHHedged atomic.Int64 // foreground went on without the lists
	neTreeHLanded atomic.Int64 // detached fetches that brought lists after the hedge
	neTreeHFailed atomic.Int64
	neTreeHClient = &http.Client{Timeout: neTreeHLateDL}

	// neTreeHFetch — the detached tree-set fetch of one block (ne_glyph_fidx.go: neFidxSrtmBlockGet with the
	// tree fields, quiet). ok = the block is in the LRU with lists; noCol = a 200 without the column.
	neTreeHFetch func(bi, bj int) (ok, noCol bool)
	// neTreeHLandedHook — bins to rebuild once a block's lists arrived late.
	neTreeHLandedHook func(bins [][2]int)
)

func init() {
	neTreeHFetch = func(bi, bj int) (ok, noCol bool) { return neFidxSrtmBlockGet(bi, bj, true) }
	neTreeHLandedHook = func(bins [][2]int) {
		now := time.Now().UnixNano()
		for _, b := range bins {
			neFidxQueue(b[0], b[1], now)
		}
	}
}

// neTreeHMarker — the mode survives a restart: a deploy must not drop the
// lists until the first probe and flip every bin's state hash twice.
func neTreeHMarker() string { return filepath.Join(neFidxDiskDir, "tree_h.on") }

func neTreeHRestore() {
	if neTreeHPinned != "" {
		return
	}
	if _, err := os.Stat(neTreeHMarker()); err == nil {
		neTreeHOn.Store(true)
		log.Printf("tree_h: on (restored from %s)", neTreeHMarker())
	}
}

func neTreeHPersist(on bool) {
	if on {
		os.MkdirAll(neFidxDiskDir, 0o755)
		os.WriteFile(neTreeHMarker(), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
		return
	}
	os.Remove(neTreeHMarker())
}

// neTreeHMode — whether the peer serves the apex lists (bins and the sample ask for them).
func neTreeHMode() bool {
	switch neTreeHPinned {
	case "1":
		return true
	case "0":
		return false
	}
	return neTreeHOn.Load()
}

// neTreeHFields — the peer field list for the current mode.
func neTreeHFields() string {
	if neTreeHMode() {
		return neFidxSrtmFlds + "," + neTreeHField
	}
	return neFidxSrtmFlds
}

// neTreeHNoColumn — a tree-set answer came back without the column: the peer
// does not serve it (any more). The only thing that switches the mode off.
func neTreeHNoColumn(what string) {
	if neTreeHPinned != "" || !neTreeHOn.Load() {
		return
	}
	neTreeHOn.Store(false)
	neTreeHPersist(false)
	neTreeHOffUntil.set(time.Now().Add(neTreeHOffFor))
	log.Printf("tree_h: %s answered without the column → whole-cell counts, next probe in %s", what, neTreeHOffFor)
}

// neTreeHHedgeNow — how long the foreground waits for the tree set: nothing
// while it keeps losing (the lists then always arrive behind the picture).
func neTreeHHedgeNow() time.Duration {
	if neTreeHLost.Load() >= 3 {
		return 0
	}
	return neTreeHHedge
}

func neTreeHBlockKey(i, j int) string { return fmt.Sprintf("%d/%d", i>>2, j>>2) }

// neTreeHOwed — the lists of bin (i,j)'s block are on their way or parked: a
// bin built now would come out without them, so the list-less bin on hand is
// the answer and nothing is queued.
func neTreeHOwed(i, j int) bool {
	if !neTreeHMode() {
		return false
	}
	if time.Since(neFidxSrtmDown.get()) < 2*time.Minute {
		return true // the peer is marked down: nothing would be fetched, a rebuild would repeat the bin on hand
	}
	k := neTreeHBlockKey(i, j)
	neTreeHMu.Lock()
	defer neTreeHMu.Unlock()
	if neTreeHFly[k] != nil {
		return true
	}
	return time.Until(neTreeHBackM[k].until) > 0
}

// neTreeHWantNote — bin (i,j) was served without lists: rebuild it when its block lands.
func neTreeHWantNote(i, j int) {
	neTreeHMu.Lock()
	if len(neTreeHWant) >= neTreeHMapMax {
		neTreeHWant = map[[2]int]struct{}{}
	}
	neTreeHWant[[2]int{i, j}] = struct{}{}
	neTreeHMu.Unlock()
}

// neTreeHStart — the detached tree-set fetch of block (bi,bj): the one in
// flight, a new one, or nil while the block is parked / no slot is free.
func neTreeHStart(bi, bj int) *neTreeHLate {
	k := fmt.Sprintf("%d/%d", bi, bj)
	neTreeHMu.Lock()
	defer neTreeHMu.Unlock()
	if l := neTreeHFly[k]; l != nil {
		return l
	}
	if time.Until(neTreeHBackM[k].until) > 0 {
		return nil
	}
	if len(neTreeHBackM) >= neTreeHMapMax {
		neTreeHBackM = map[string]neTreeHBack{}
	}
	select {
	case neTreeHSem <- struct{}{}:
	default:
		neTreeHBackM[k] = neTreeHBack{until: time.Now().Add(neTreeHBusyWait), n: neTreeHBackM[k].n}
		return nil
	}
	l := &neTreeHLate{done: make(chan struct{}), t0: time.Now()}
	neTreeHFly[k] = l
	go func() {
		ok, noCol := neTreeHFetch(bi, bj)
		<-neTreeHSem
		d := time.Since(l.t0)
		var bins [][2]int
		neTreeHMu.Lock()
		delete(neTreeHFly, k)
		if ok {
			delete(neTreeHBackM, k)
			for b := range neTreeHWant {
				if b[0]>>2 == bi && b[1]>>2 == bj {
					bins = append(bins, b)
					delete(neTreeHWant, b)
				}
			}
		} else {
			n := neTreeHBackM[k].n
			back := neTreeHBackMin << uint(n)
			if back > neTreeHBackMax || back <= 0 {
				back = neTreeHBackMax
			}
			neTreeHBackM[k] = neTreeHBack{until: time.Now().Add(back), n: n + 1}
		}
		neTreeHMu.Unlock()
		l.ok = ok
		close(l.done)
		switch {
		case noCol:
			neTreeHNoColumn(fmt.Sprintf("block %s", k))
		case !ok:
			neTreeHFailed.Add(1)
			log.Printf("tree_h block %s: no lists after %.1fs — parked, the prebuilt set serves", k, d.Seconds())
		case d <= neTreeHHedge:
			neTreeHLost.Store(0)
		default:
			neTreeHLanded.Add(1)
			log.Printf("tree_h block %s: lists landed after %.1fs, %d bins rebuild", k, d.Seconds(), len(bins))
		}
		if len(bins) > 0 {
			neTreeHLandedHook(bins)
		}
	}()
	return l
}

// neTreeHAwait — the foreground side of the race for block (bi,bj): true when
// the tree set is in the block LRU (lists and all), false when the caller
// should go on with the prebuilt set (the lists follow, or the block is parked).
func neTreeHAwait(bi, bj int) bool {
	l := neTreeHStart(bi, bj)
	if l == nil {
		return false
	}
	h := neTreeHHedgeNow()
	if h > 0 {
		t := time.NewTimer(h)
		defer t.Stop()
		select {
		case <-l.done:
		case <-t.C:
		}
	}
	select {
	case <-l.done:
		if l.ok {
			neTreeHWon.Add(1)
			neTreeHLost.Store(0)
		}
		return l.ok
	default:
	}
	neTreeHHedged.Add(1)
	if h > 0 {
		neTreeHLost.Add(1)
	}
	return false
}

// neTreeHProbe — capability only: one block with the tree set; an answer that
// carries the column turns the mode on, however long the peer took. The target
// is the block a user last needed (the body is useful: it lands in the block
// LRU), else a random block of the country.
func neTreeHProbe() {
	if neTreeHPinned != "" || neTreeHMode() || peers().ObsAPI == "" {
		return
	}
	if time.Since(neTreeHOffUntil.get()) < 0 {
		return
	}
	neTreeHProbeMu.Lock()
	defer neTreeHProbeMu.Unlock()
	neTreeHProbes.Add(1)
	var bi, bj int
	if v, _ := neTreeHLastBlk.Load().(string); v != "" {
		fmt.Sscanf(v, "%d/%d", &bi, &bj)
	} else {
		bi, bj = 110+rand.Intn(50), 470+rand.Intn(20) // 11–16° E, 47–49° N
	}
	w, s := float64(bi)*0.1, float64(bj)*0.1
	url := fmt.Sprintf("%s?bbox=%.3f,%.3f,%.3f,%.3f&format=bin&block=1&fields=%s,%s", peers().ObsAPI+"/api/v1/cells", w, s, w+0.1, s+0.1, neFidxSrtmFlds, neTreeHField)
	cl := &http.Client{Timeout: neTreeHProbeDL}
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "umfeld-at/fidx-probe (+"+publicURL()+")")
	t0 := time.Now()
	resp, err := cl.Do(req)
	ok := false
	if err == nil {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if resp.StatusCode == 200 && len(body) > 8 && string(body[:4]) == "NEXB" {
			bins, _, good := neFidxBlockParse(body, resp.Header.Get("X-Cells-Epoch"), bi, bj)
			ok = good && len(bins) > 0
			for _, b := range bins {
				if len(b.ids) > 0 && b.th == nil {
					ok = false // 200 without the column: the peer does not serve it (yet)
				}
			}
		}
	}
	d := time.Since(t0)
	if ok {
		neTreeHProbeOK.Add(1)
		neTreeHOn.Store(true)
		neTreeHPersist(true)
		log.Printf("tree_h probe %d/%d: column served (%.1fs) → on, bins rebuild with apex lists on their next use", bi, bj, d.Seconds())
		return
	}
	neTreeHLastBlk.Store("") // an empty or failing block proves nothing: another one next time
	log.Printf("tree_h probe %d/%d: not ready (%.1fs, %v)", bi, bj, d.Seconds(), err)
}

// neTreeHLoop — the probe while the mode is off (a no-op while it is on).
func neTreeHLoop() {
	time.Sleep(2 * time.Minute)
	for {
		neTreeHProbe()
		if !neTreeHMode() && neTreeHProbes.Load() < 4 {
			time.Sleep(3 * time.Minute) // a cold block is built by the second ask
			continue
		}
		time.Sleep(neTreeHProbeIv)
	}
}

// NETreeHStats — size line for /api/v1/debug/inflight.
func NETreeHStats() map[string]interface{} {
	neTreeHMu.Lock()
	fly, parked, want := len(neTreeHFly), 0, len(neTreeHWant)
	for _, b := range neTreeHBackM {
		if time.Until(b.until) > 0 {
			parked++
		}
	}
	neTreeHMu.Unlock()
	return map[string]interface{}{"on": neTreeHMode(), "pinned": neTreeHPinned, "probes": neTreeHProbes.Load(), "probe_ok": neTreeHProbeOK.Load(), "off_until": neTreeHOffUntil.get().Format(time.RFC3339),
		"hedge_ms": neTreeHHedgeNow().Milliseconds(), "lost_streak": neTreeHLost.Load(), "won": neTreeHWon.Load(), "hedged": neTreeHHedged.Load(), "late_landed": neTreeHLanded.Load(), "late_failed": neTreeHFailed.Load(),
		"in_flight": fly, "in_flight_cap": neTreeHLateMax, "blocks_parked": parked, "bins_owed": want, "map_cap": neTreeHMapMax}
}
