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
// units: the union of its `ht` (tallest tree) terms; ok=false when there is
// none (every apex of a matching cell counts). A bare `ht=unk` yields an empty
// window.
func neFiltApexRange(e *neFiltExpr) (lo, hi int, ok bool) {
	fd := neFiltByID["ht"]
	lo, hi = math.MaxInt32, math.MinInt32
	for _, g := range e.Groups {
		for _, t := range g {
			if t.F != "ht" {
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

// ── the mode gate ────────────────────────────────────────────────────────────

const (
	neTreeHField   = "tree_h"
	neTreeHProbeDL = 6 * time.Second // a prebuilt block answers in well under a second
	neTreeHSlow    = 8 * time.Second // a tree-set fetch slower than this in service → off
	neTreeHOffFor  = time.Hour
	neTreeHProbeIv = time.Hour
)

var (
	neTreeHOn       atomic.Bool
	neTreeHOffUntil atomicTime
	neTreeHProbeOK  atomic.Int64 // consecutive fast probes
	neTreeHProbes   atomic.Int64
	neTreeHProbeMu  sync.Mutex
	neTreeHLastBlk  atomic.Value // string "bi/bj": the last block a user fetch asked for (the probe's target)
	neTreeHPinned   = os.Getenv("NE_TREE_H")
)

// neTreeHMode — whether bin / sample fetches ask for the tree lists now.
func neTreeHMode() bool {
	switch neTreeHPinned {
	case "1":
		return true
	case "0":
		return false
	}
	return neTreeHOn.Load() && time.Since(neTreeHOffUntil.get()) >= 0
}

// neTreeHFields — the peer field list for the current mode.
func neTreeHFields() string {
	if neTreeHMode() {
		return neFidxSrtmFlds + "," + neTreeHField
	}
	return neFidxSrtmFlds
}

// neTreeHSlowFetch — a tree-set fetch in service took too long or failed: back off.
func neTreeHSlowFetch(d time.Duration, err error) {
	if neTreeHPinned != "" || !neTreeHOn.Load() {
		return
	}
	neTreeHOn.Store(false)
	neTreeHProbeOK.Store(0)
	neTreeHOffUntil.set(time.Now().Add(neTreeHOffFor))
	log.Printf("tree_h: fetch took %.1fs (%v) → whole-cell counts for %s", d.Seconds(), err, neTreeHOffFor)
}

// neTreeHProbe — one block with the tree set under a short deadline; two fast
// answers in a row turn the mode on. The target is the block a user last
// needed (so the probe's body is useful: it lands in the block LRU), else a
// random block of the country.
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
		if resp.StatusCode == 404 {
			ok = true // no cells there: nothing to build, counts as fast
		} else if resp.StatusCode == 200 && len(body) > 8 && string(body[:4]) == "NEXB" {
			bins, _, good := neFidxBlockParse(body, resp.Header.Get("X-Cells-Epoch"), bi, bj)
			ok = good
			for _, b := range bins {
				if len(b.ids) > 0 && b.th == nil {
					ok = false // 200 without the column: the peer does not serve it (yet)
				}
			}
		}
	}
	d := time.Since(t0)
	if ok {
		n := neTreeHProbeOK.Add(1)
		log.Printf("tree_h probe %d/%d: ok in %.1fs (%d/2)", bi, bj, d.Seconds(), n)
		if n >= 2 {
			neTreeHOn.Store(true)
			log.Printf("tree_h: on — bins rebuild with apex lists on their next use")
		}
		return
	}
	neTreeHProbeOK.Store(0)
	log.Printf("tree_h probe %d/%d: not ready (%.1fs, %v)", bi, bj, d.Seconds(), err)
}

// neTreeHLoop — the hourly probe (a second one soon after the first success so the mode flips within minutes of the peer being ready).
func neTreeHLoop() {
	time.Sleep(2 * time.Minute)
	for {
		neTreeHProbe()
		if neTreeHProbeOK.Load() == 1 {
			time.Sleep(5 * time.Minute)
			continue
		}
		time.Sleep(neTreeHProbeIv)
	}
}

// NETreeHStats — size line for /api/v1/debug/inflight.
func NETreeHStats() map[string]interface{} {
	return map[string]interface{}{"on": neTreeHMode(), "pinned": neTreeHPinned, "probes": neTreeHProbes.Load(), "probe_ok": neTreeHProbeOK.Load(), "off_until": neTreeHOffUntil.get().Format(time.RFC3339)}
}
