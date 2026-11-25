package main

// Peer block generations (docs/ne-filter.md §8.1a).
//
// {OBS_NAME} rebuilds its 0.1° NEXB blocks independently of the product epoch
// (X-Cells-Epoch "2026-10" stayed while 199 sweep-strip bins got their obs rows,
// 2026-11-10). Keying the srtm part of a bin's state on the epoch alone left
// every bin built before such a rebuild stale until an obs POST happened to
// move the KG digest. The peer's manifest GET /api/v1/cells/blocks lists one
// `gen` per block (the X-Cells-Gen / ETag of its body). We poll it every
// neFidxBlocksEvery with If-None-Match (45 KB gz, 304 when nothing moved),
// keep bi/bj → gen in memory + data/ne_fidx/blocks.json (a restart does not
// move every bin's state), and
//
//   - put the gen of the block a bin lies in into neFidxState (";blk=<gen>"):
//     a rebuilt block = a new state for its 16 bins, nothing else;
//   - treat a parsed block in neFidxSrtmBlk whose gen is not the manifest's as
//     a miss (no TTL wait, no extra request);
//   - queue a refresh build of every bin of a changed block we hold (on disk at
//     any state, or asked for in the last 24 h): one pool worker at a time,
//     after every live request, so the refetch happens now and not at the
//     next tile that happens to touch the bin. Bins nobody ever asked for stay
//     unbuilt — they are fetched on first touch like before.
//
// Bandwidth: one block body per changed block (only ones we hold), one 304
// every 10 min. The peer without the manifest (404) → epoch + 15 min TTL as before.

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const neFidxBlocksEvery = 10 * time.Minute

var (
	neFidxBlocksMu      sync.RWMutex
	neFidxBlocksGen     = map[[2]int]string{} // (bi,bj) → gen
	neFidxBlocksAll     string                // the manifest's `generation` (all blocks)
	neFidxBlocksETag    string
	neFidxBlocksAt      time.Time // last poll that answered 200/304
	neFidxBlocksLoaded  bool
	neFidxBlocksChanged atomic.Int64 // blocks whose gen moved since start
	neFidxBlocksPolls   atomic.Int64
	neFidxBlocksRefresh atomic.Int64 // bins queued for a refresh build
	neFidxBlocksRefDone atomic.Int64
)

func neFidxBlocksFile() string { return filepath.Join(neFidxDiskDir, "blocks.json") }

type neFidxBlocksDisk struct {
	Generation string            `json:"generation"`
	ETag       string            `json:"etag"`
	At         time.Time         `json:"at"`
	Gen        map[string]string `json:"gen"` // "bi/bj" → gen
}

func neFidxBlocksLoad() {
	neFidxBlocksMu.Lock()
	defer neFidxBlocksMu.Unlock()
	if neFidxBlocksLoaded {
		return
	}
	neFidxBlocksLoaded = true
	b, err := os.ReadFile(neFidxBlocksFile())
	if err != nil {
		return
	}
	var d neFidxBlocksDisk
	if json.Unmarshal(b, &d) != nil {
		return
	}
	for k, g := range d.Gen {
		var bi, bj int
		if _, err := fmt.Sscanf(k, "%d/%d", &bi, &bj); err == nil {
			neFidxBlocksGen[[2]int{bi, bj}] = g
		}
	}
	neFidxBlocksAll, neFidxBlocksETag = d.Generation, d.ETag
}

func neFidxBlocksSave() {
	neFidxBlocksMu.RLock()
	d := neFidxBlocksDisk{Generation: neFidxBlocksAll, ETag: neFidxBlocksETag, At: neFidxBlocksAt, Gen: make(map[string]string, len(neFidxBlocksGen))}
	for k, g := range neFidxBlocksGen {
		d.Gen[fmt.Sprintf("%d/%d", k[0], k[1])] = g
	}
	neFidxBlocksMu.RUnlock()
	b, _ := json.Marshal(d)
	os.MkdirAll(neFidxDiskDir, 0o755)
	tmp := neFidxBlocksFile() + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, neFidxBlocksFile())
	}
}

// neFidxBlockGenRaw — the manifest's gen of block (bi,bj); "" when unknown (no manifest, or a block the peer does not list).
func neFidxBlockGenRaw(bi, bj int) string {
	neFidxBlocksLoad()
	neFidxBlocksMu.RLock()
	g := neFidxBlocksGen[[2]int{bi, bj}]
	neFidxBlocksMu.RUnlock()
	return g
}

// neFidxBlockGen — the gen every state hash reads: the gen the peer SERVES for
// the block. That is the manifest's, except while the block is being rebuilt
// there (neFidxPend): then it is the gen of the previous-generation body we
// were handed, so bins and tiles built from it are filed as what they are and
// everything moves once, when the current body arrives.
func neFidxBlockGen(bi, bj int) string {
	neFidxPendMu.Lock()
	p := neFidxPend[[2]int{bi, bj}]
	neFidxPendMu.Unlock()
	if p != nil {
		return p.gen
	}
	return neFidxBlockGenRaw(bi, bj)
}

// neFidxBlockGenOfBin — the gen of the block holding bin (i,j).
func neFidxBlockGenOfBin(i, j int) string { return neFidxBlockGen(i>>2, j>>2) }

// neFidxBlocksGeneration — the manifest's whole-table generation ("" without a manifest); part of the MVT state memo key.
func neFidxBlocksGeneration() string {
	neFidxBlocksLoad()
	neFidxBlocksMu.RLock()
	defer neFidxBlocksMu.RUnlock()
	return neFidxBlocksAll
}

// neFidxBoxBlockGens — ";blk=" + the gens of every block a box touches (neFidxBoxState).
func neFidxBoxBlockGens(w, s, e, n float64, th string) string {
	i0, i1 := int(w/0.1), int((e-1e-9)/0.1)
	j0, j1 := int(s/0.1), int((n-1e-9)/0.1)
	var sb strings.Builder
	sb.WriteString(";blk=")
	for bi := i0; bi <= i1; bi++ {
		for bj := j0; bj <= j1; bj++ {
			sb.WriteString(neFidxBlockEff(bi, bj, th))
			sb.WriteByte(',')
		}
	}
	return sb.String()
}

func NEFidxBlocksStats() map[string]interface{} {
	neFidxBlocksLoad()
	neFidxBlocksMu.RLock()
	n, gen, at := len(neFidxBlocksGen), neFidxBlocksAll, neFidxBlocksAt
	neFidxBlocksMu.RUnlock()
	return map[string]interface{}{"blocks": n, "generation": gen, "polled_at": at, "polls": neFidxBlocksPolls.Load(),
		"changed": neFidxBlocksChanged.Load(), "refresh_queued": neFidxBlocksRefresh.Load(), "refresh_done": neFidxBlocksRefDone.Load(), "refresh_pending": neFidxRefreshPending(), "rebuilding": neFidxPendStats()}
}

// neFidxBlocksLoop — poll the manifest; first poll right after start.
func neFidxBlocksLoop() {
	if os.Getenv("NE_FIDX_BLOCKS") == "0" {
		return
	}
	time.Sleep(15 * time.Second)
	for {
		if err := neFidxBlocksPoll(); err != nil {
			log.Printf("fidx blocks manifest: %v", err)
		}
		time.Sleep(neFidxBlocksEvery)
	}
}

// neFidxBlocksPoll — one GET (If-None-Match) of the manifest; diff → invalidate + refresh.
func neFidxBlocksPoll() error {
	neFidxBlocksLoad()
	api := peers().ObsAPI
	if api == "" {
		return nil
	}
	req, _ := http.NewRequest("GET", api+"/api/v1/cells/blocks", nil)
	req.Header.Set("User-Agent", "umfeld-at/fidx-blocks (+"+publicURL()+")")
	neFidxBlocksMu.RLock()
	if neFidxBlocksETag != "" {
		req.Header.Set("If-None-Match", neFidxBlocksETag)
	}
	neFidxBlocksMu.RUnlock()
	resp, err := neFidxClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	neFidxBlocksPolls.Add(1)
	switch resp.StatusCode {
	case 304:
		neFidxBlocksMu.Lock()
		neFidxBlocksAt = time.Now()
		neFidxBlocksMu.Unlock()
		return nil
	case 200:
	case 404:
		return nil // peer without a manifest: epoch + TTL rule
	default:
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	var m struct {
		Generation string `json:"generation"`
		Epoch      string `json:"epoch"`
		Blocks     struct {
			BI  []int    `json:"bi"`
			BJ  []int    `json:"bj"`
			Gen []string `json:"gen"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return err
	}
	if len(m.Blocks.BI) != len(m.Blocks.BJ) || len(m.Blocks.BI) != len(m.Blocks.Gen) || len(m.Blocks.BI) == 0 {
		return fmt.Errorf("manifest lists %d/%d/%d blocks", len(m.Blocks.BI), len(m.Blocks.BJ), len(m.Blocks.Gen))
	}
	neFidxSrtmEpochStore(m.Epoch)
	var changed [][2]int
	neFidxBlocksMu.Lock()
	first := len(neFidxBlocksGen) == 0
	next := make(map[[2]int]string, len(m.Blocks.BI))
	for k := range m.Blocks.BI {
		key := [2]int{m.Blocks.BI[k], m.Blocks.BJ[k]}
		next[key] = m.Blocks.Gen[k]
		if old, ok := neFidxBlocksGen[key]; !first && old != m.Blocks.Gen[k] {
			_ = ok
			changed = append(changed, key)
		}
	}
	neFidxBlocksGen = next
	neFidxBlocksAll, neFidxBlocksETag, neFidxBlocksAt = m.Generation, resp.Header.Get("ETag"), time.Now()
	neFidxBlocksMu.Unlock()
	neFidxBlocksSave()
	neMVTStateMemoDrop()
	if first {
		log.Printf("fidx blocks manifest: %d blocks, generation %s (first; bins built without a block gen rebuild on their next use)", len(next), m.Generation)
		return nil
	}
	if len(changed) == 0 {
		return nil
	}
	neFidxBlocksChanged.Add(int64(len(changed)))
	held := neFidxHeldBins()
	q := 0
	for _, b := range changed {
		for a := 0; a < 4; a++ {
			for c := 0; c < 4; c++ {
				i, j := 4*b[0]+a, 4*b[1]+c
				neFidxSrtmBlk.drop(fmt.Sprintf("%d/%d", i, j))
				if held[[2]int{i, j}] {
					neFidxRefreshQueue(i, j)
					q++
				}
			}
		}
	}
	neFidxBlocksRefresh.Add(int64(q))
	log.Printf("fidx blocks manifest: generation %s, %d block(s) rebuilt at the peer → %d held bin(s) queued for a refresh build", m.Generation, len(changed), q)
	return nil
}

// neFidxHeldBins — bins with a blob on disk (any state) or asked for in the last 24 h.
func neFidxHeldBins() map[[2]int]bool {
	out := map[[2]int]bool{}
	_ = filepath.Walk(neFidxDiskDir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || !strings.HasSuffix(p, ".zst") {
			return nil
		}
		name := strings.TrimSuffix(filepath.Base(p), ".zst")
		if us := strings.IndexByte(name, '_'); us > 0 {
			i, e1 := strconv.Atoi(name[:us])
			j, e2 := strconv.Atoi(name[us+1:])
			if e1 == nil && e2 == nil {
				out[[2]int{i, j}] = true
			}
		}
		return nil
	})
	neFidxWarmMu.Lock()
	for k, t := range neFidxWarmSeen {
		if time.Since(t) <= 24*time.Hour {
			out[k] = true
		}
	}
	neFidxWarmMu.Unlock()
	return out
}

// ── refresh queue: drained by the build pool when no live request is queued, one worker at a time ──

var (
	neFidxRefreshQ    = map[[2]int]struct{}{}
	neFidxRefreshBusy bool
)

func neFidxRefreshQueue(i, j int) {
	neFidxPoolStart()
	neFidxPoolMu.Lock()
	if len(neFidxRefreshQ) < neFidxPoolMax {
		neFidxRefreshQ[[2]int{i, j}] = struct{}{}
	}
	neFidxPoolMu.Unlock()
	neFidxPoolCond.Signal()
}

func neFidxRefreshPending() int {
	neFidxPoolMu.Lock()
	defer neFidxPoolMu.Unlock()
	return len(neFidxRefreshQ)
}

// neFidxRefreshPick — under neFidxPoolMu: a refresh bin when the live queue is empty and no worker is refreshing; bins of one block adjacent.
func neFidxRefreshPick() ([2]int, bool) {
	if len(neFidxRefreshQ) == 0 || neFidxRefreshBusy {
		return [2]int{}, false
	}
	var pick [2]int
	first := true
	for k := range neFidxRefreshQ {
		if first || k[0]>>2 < pick[0]>>2 || (k[0]>>2 == pick[0]>>2 && (k[1]>>2 < pick[1]>>2 || (k[1]>>2 == pick[1]>>2 && (k[0] < pick[0] || (k[0] == pick[0] && k[1] < pick[1]))))) {
			pick, first = k, false
		}
	}
	delete(neFidxRefreshQ, pick)
	neFidxRefreshBusy = true
	return pick, true
}

// ── blocks the peer is rebuilding (docs/ne-filter.md §8.1b) ─────────────────
//
// The peer never builds inside a request. While a block is being rebuilt it
// answers the previous generation at once (200, X-Cells-Stale: 1, X-Cells-Gen =
// the body's gen, X-Cells-Gen-Current, Retry-After) — and its manifest already
// lists the NEW gen. Filed under the manifest gen, such a body would pass for
// current and never be replaced. So a stale answer makes the block *pending*:
// its served gen stands in for the manifest's in every state hash
// (neFidxBlockGen), the parsed body stays valid in the block LRU, and
// neFidxPendLoop asks again when Retry-After has passed (If-None-Match: a 304
// moves no bytes). The first current answer ends it: state moves, held bins
// are queued for a refresh build.
//
// Nothing stored for a block and field set → 503 + Retry-After: the block is
// *building* (neFidxBuilding); no request goes out for it until then, and the
// peer as a whole is not blamed.

type neFidxPendEnt struct {
	gen   string // X-Cells-Gen of the stale body the states are keyed on
	etag  string
	tree  bool // gen/etag are the tree set's
	until time.Time
	since time.Time
}

const (
	neFidxPendMax   = 4096
	neFidxPendTick  = 30 * time.Second
	neFidxPendBatch = 8 // rechecks per tick
)

var (
	neFidxPendMu    sync.Mutex
	neFidxPend      = map[[2]int]*neFidxPendEnt{}
	neFidxBuilding  = map[[2]int]time.Time{} // block → no request before (503 + Retry-After)
	neFidxPendSeen  atomic.Int64             // stale answers
	neFidxPendDone  atomic.Int64             // pending blocks that became current
	neFidxPend304   atomic.Int64
	neFidxBuild503  atomic.Int64
	neFidxPendCheck = func(bi, bj int, tree bool) { neFidxSrtmBlockGet(bi, bj, tree) }
)

// neFidxRetryAfter — the header in seconds, clamped to 30 s … 10 min; 2 min without one.
func neFidxRetryAfter(h string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(h))
	if err != nil || n <= 0 {
		return 2 * time.Minute
	}
	d := time.Duration(n) * time.Second
	if d < 30*time.Second {
		d = 30 * time.Second
	}
	if d > 10*time.Minute {
		d = 10 * time.Minute
	}
	return d
}

// neFidxPendSet — a stale body of block (bi,bj) was parsed. The tree set's gen wins over the prebuilt set's
// (should the two ever differ at the peer, the bins holding lists are the ones keyed exactly).
func neFidxPendSet(bi, bj int, gen, etag string, tree bool, ra time.Duration) {
	k := [2]int{bi, bj}
	neFidxPendSeen.Add(1)
	neFidxPendMu.Lock()
	p := neFidxPend[k]
	moved := false
	switch {
	case p == nil:
		if len(neFidxPend) >= neFidxPendMax {
			neFidxPend = map[[2]int]*neFidxPendEnt{}
		}
		neFidxPend[k] = &neFidxPendEnt{gen: gen, etag: etag, tree: tree, until: time.Now().Add(ra), since: time.Now()}
		moved = true
	case tree || !p.tree:
		moved = p.gen != gen
		p.gen, p.etag, p.tree, p.until = gen, etag, tree, time.Now().Add(ra)
	default:
		p.until = time.Now().Add(ra)
	}
	delete(neFidxBuilding, k)
	neFidxPendMu.Unlock()
	if moved {
		neMVTStateMemoDrop()
		log.Printf("fidx srtm block %d/%d: peer is rebuilding it, previous generation %s serves (manifest %s), next look in %s", bi, bj, gen, neFidxBlockGenRaw(bi, bj), ra)
	}
}

// neFidxPendTouch — 304 on the stale body's ETag: still rebuilding.
func neFidxPendTouch(bi, bj int, ra time.Duration) {
	neFidxPend304.Add(1)
	neFidxPendMu.Lock()
	if p := neFidxPend[[2]int{bi, bj}]; p != nil {
		p.until = time.Now().Add(ra)
	}
	neFidxPendMu.Unlock()
}

// neFidxPendClear — a current body of block (bi,bj) was parsed: the state moves to the manifest gen, held bins rebuild.
func neFidxPendClear(bi, bj int) {
	k := [2]int{bi, bj}
	neFidxPendMu.Lock()
	p := neFidxPend[k]
	delete(neFidxPend, k)
	delete(neFidxBuilding, k)
	neFidxPendMu.Unlock()
	if p == nil {
		return
	}
	neFidxPendDone.Add(1)
	neMVTStateMemoDrop()
	held, q := neFidxHeldBins(), 0
	for a := 0; a < 4; a++ {
		for c := 0; c < 4; c++ {
			if i, j := 4*bi+a, 4*bj+c; held[[2]int{i, j}] {
				neFidxRefreshQueue(i, j)
				q++
			}
		}
	}
	neFidxBlocksRefresh.Add(int64(q))
	log.Printf("fidx srtm block %d/%d: current again after %s on the previous generation → %d held bin(s) queued for a refresh build", bi, bj, time.Since(p.since).Round(time.Second), q)
}

// neFidxPendIs — is block (bi,bj) served from its previous generation right now?
func neFidxPendIs(bi, bj int) bool {
	neFidxPendMu.Lock()
	p := neFidxPend[[2]int{bi, bj}]
	neFidxPendMu.Unlock()
	return p != nil
}

// neFidxPendETag — the If-None-Match of a recheck (only for the field set the ETag belongs to).
func neFidxPendETag(bi, bj int, tree bool) string {
	neFidxPendMu.Lock()
	defer neFidxPendMu.Unlock()
	if p := neFidxPend[[2]int{bi, bj}]; p != nil && p.tree == tree {
		return p.etag
	}
	return ""
}

// neFidxBuildingSet — 503 + Retry-After: nothing stored at the peer for this block and field set.
func neFidxBuildingSet(bi, bj int, ra time.Duration) {
	neFidxBuild503.Add(1)
	neFidxPendMu.Lock()
	if len(neFidxBuilding) >= neFidxPendMax {
		neFidxBuilding = map[[2]int]time.Time{}
	}
	neFidxBuilding[[2]int{bi, bj}] = time.Now().Add(ra)
	neFidxPendMu.Unlock()
}

// neFidxBuildingFor — how long block (bi,bj) is still not to be asked (0 = ask).
func neFidxBuildingFor(bi, bj int) time.Duration {
	neFidxPendMu.Lock()
	defer neFidxPendMu.Unlock()
	t, ok := neFidxBuilding[[2]int{bi, bj}]
	if !ok {
		return 0
	}
	if d := time.Until(t); d > 0 {
		return d
	}
	delete(neFidxBuilding, [2]int{bi, bj})
	return 0
}

// neFidxPendDue — up to max pending blocks whose Retry-After has passed, longest overdue first; each is pushed
// one tick ahead so a slow recheck is not started twice.
func neFidxPendDue(max int) [][2]int {
	now := time.Now()
	neFidxPendMu.Lock()
	defer neFidxPendMu.Unlock()
	var due [][2]int
	for k, p := range neFidxPend {
		if !p.until.After(now) {
			due = append(due, k)
		}
	}
	sort.Slice(due, func(a, b int) bool { return neFidxPend[due[a]].until.Before(neFidxPend[due[b]].until) })
	if len(due) > max {
		due = due[:max]
	}
	for _, k := range due {
		neFidxPend[k].until = now.Add(2 * neFidxPendTick)
	}
	return due
}

// neFidxPendLoop — ask again for the blocks we hold in their previous generation (startBackground).
func neFidxPendLoop() {
	for {
		time.Sleep(neFidxPendTick)
		if time.Since(neFidxSrtmDown.get()) < 2*time.Minute {
			continue
		}
		for _, k := range neFidxPendDue(neFidxPendBatch) {
			neFidxPendCheck(k[0], k[1], neTreeHMode())
		}
	}
}

func neFidxPendStats() map[string]interface{} {
	neFidxPendMu.Lock()
	n, b := len(neFidxPend), len(neFidxBuilding)
	neFidxPendMu.Unlock()
	return map[string]interface{}{"pending": n, "building": b, "cap": neFidxPendMax, "stale_answers": neFidxPendSeen.Load(),
		"not_modified": neFidxPend304.Load(), "became_current": neFidxPendDone.Load(), "http_503": neFidxBuild503.Load()}
}
