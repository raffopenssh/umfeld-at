package main

// Bin content generations (docs/ne-filter.md §8.1c).
//
// The peer's manifest carries one gen per 0.1° BLOCK, and the block gen moves
// when any of its 16 bins was re-run there — also when the re-run reproduced the
// same bytes. Keyed on the block gen alone, a bin's state moved with every such
// rebuild: 2026-11-24 one block changed gen 13 times, its bins were rebuilt and
// written 24 times, 20 of them byte-identical in every column, and the 2 GB disk
// cache turned over with them (browser copies and veil tiles too).
//
// So every parsed block body is digested PER BIN (ids + fixed columns = D, the
// apex lists = T), and a bin's state reads the *effective* gen: the block gen
// under which its present content first appeared. A new block gen with the same
// D (and T) leaves the effective gen — and the state, the disk file, the ETag —
// where it was.
//
//   - Until the body of a new block gen has been parsed the table does not cover
//     it and the state falls back to the block gen (it moves, exactly as before);
//     neFidxGet then fetches the block BEFORE it builds and looks again — an
//     unchanged bin is back on its old state and is served from the LRU / disk.
//   - A built bin is filed under the effective gen of the body it was built FROM
//     (neFidxSrtm.effC / effT), not under whatever the table says when the build
//     ends — a body parsed meanwhile cannot mislabel it.
//   - Persisted in data/ne_fidx/bin_eff.json (a restart moves no state); losing
//     the file costs one rebuild per bin whose effective gen was not the block's.
//
// Bound: one entry per bin the peer serves (17 k); above neFidxEffMax new bins stay on the block gen.

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const neFidxEffMax = 60000

type neFidxEffEnt struct {
	Gen  string `json:"g"`            // block gen of the last body this bin was seen in (either field set)
	D    string `json:"d"`            // digest of ids + fixed columns
	EffC string `json:"e"`            // block gen under which D first appeared
	GenT string `json:"gt,omitempty"` // block gen of the last body WITH the apex lists
	T    string `json:"t,omitempty"`  // digest of the lists
	EffT string `json:"et,omitempty"` // block gen under which (D, T) first appeared
}

var (
	neFidxEffMu     sync.RWMutex
	neFidxEff       = map[[2]int]*neFidxEffEnt{}
	neFidxEffLoaded bool
	neFidxEffTimer  *time.Timer
	neFidxEffSame   atomic.Int64 // a new block gen, this bin's content unchanged (the rebuilds no longer made)
	neFidxEffMoved  atomic.Int64 // a new block gen and new content
	neFidxEffNew    atomic.Int64 // first sight of a bin
)

func neFidxEffFile() string { return filepath.Join(neFidxDiskDir, "bin_eff.json") }

// neFidxEffReset — forget the table (the disk dir moved: tests).
func neFidxEffReset() {
	neFidxEffMu.Lock()
	neFidxEff, neFidxEffLoaded = map[[2]int]*neFidxEffEnt{}, false
	if neFidxEffTimer != nil {
		neFidxEffTimer.Stop()
		neFidxEffTimer = nil
	}
	neFidxEffMu.Unlock()
}

func neFidxEffLoad() {
	neFidxEffMu.RLock()
	ok := neFidxEffLoaded
	neFidxEffMu.RUnlock()
	if ok {
		return
	}
	neFidxEffMu.Lock()
	defer neFidxEffMu.Unlock()
	if neFidxEffLoaded {
		return
	}
	neFidxEffLoaded = true
	b, err := os.ReadFile(neFidxEffFile())
	if err != nil {
		return
	}
	var d map[string]*neFidxEffEnt
	if json.Unmarshal(b, &d) != nil {
		return
	}
	for k, e := range d {
		var i, j int
		if _, err := fmt.Sscanf(k, "%d/%d", &i, &j); err == nil && e != nil {
			neFidxEff[[2]int{i, j}] = e
		}
	}
}

func neFidxEffSave() {
	neFidxEffMu.Lock()
	neFidxEffTimer = nil
	d := make(map[string]neFidxEffEnt, len(neFidxEff))
	for k, e := range neFidxEff {
		d[fmt.Sprintf("%d/%d", k[0], k[1])] = *e
	}
	file := neFidxEffFile()
	neFidxEffMu.Unlock()
	b, _ := json.Marshal(d)
	os.MkdirAll(filepath.Dir(file), 0o755)
	tmp := file + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, file)
	}
}

// neFidxSrtmDigests — D over ids + the fixed columns we ask for, T over the apex lists.
func neFidxSrtmDigests(v *neFidxSrtm) (d, t string) {
	h := sha256.New()
	var tmp [8]byte
	binary.LittleEndian.PutUint64(tmp[:], uint64(len(v.ids)))
	h.Write(tmp[:])
	for _, id := range v.ids {
		binary.LittleEndian.PutUint64(tmp[:], id)
		h.Write(tmp[:])
	}
	// exactly the columns a bin reads (neFidxBuild) — a column only one of the two field sets carries, or one the
	// peer adds, must not make the two bodies of one gen look like different content
	names := strings.Split(neFidxSrtmFlds, ",")
	sort.Strings(names)
	for _, n := range names {
		if v.cols[n] == nil {
			continue
		}
		binary.LittleEndian.PutUint64(tmp[:], uint64(len(v.cols[n])))
		h.Write([]byte(n))
		h.Write(tmp[:])
		h.Write(v.cols[n])
	}
	d = hex.EncodeToString(h.Sum(nil))[:16]
	if !v.thSet {
		return d, ""
	}
	h = sha256.New()
	if v.th != nil {
		for _, o := range v.th.Off {
			binary.LittleEndian.PutUint32(tmp[:], o)
			h.Write(tmp[:4])
		}
		h.Write(v.th.H)
	}
	return d, hex.EncodeToString(h.Sum(nil))[:16]
}

// neFidxEffNote — a bin of a parsed block body (v.gen = the body's X-Cells-Gen, v.thSet = it answered the
// tree field set). Updates the table and stamps v with the effective gens a bin built from it is filed under.
// changed = the entry moved in a way a state can see.
func neFidxEffNote(i, j int, v *neFidxSrtm) (changed bool) {
	if v == nil || v.gen == "" {
		return false
	}
	d, t := neFidxSrtmDigests(v)
	neFidxEffLoad()
	neFidxEffMu.Lock()
	defer neFidxEffMu.Unlock()
	k := [2]int{i, j}
	e := neFidxEff[k]
	if e == nil {
		if len(neFidxEff) >= neFidxEffMax {
			return false // full (3× the bins the peer serves): this bin stays on the block gen, v unstamped
		}
		e = &neFidxEffEnt{}
		neFidxEff[k] = e
		neFidxEffNew.Add(1)
	}
	before := *e
	sameD := e.D == d && e.EffC != ""
	if !sameD {
		// new fixed columns: whatever lists were on record belonged to the old ones
		*e = neFidxEffEnt{D: d, EffC: v.gen}
	}
	if before.Gen != "" && before.Gen != v.gen {
		if sameD {
			neFidxEffSame.Add(1)
		} else {
			neFidxEffMoved.Add(1)
		}
	}
	e.Gen = v.gen
	if v.thSet {
		if !(sameD && e.T == t && e.EffT != "") {
			e.T, e.EffT = t, v.gen
		}
		e.GenT = v.gen
		v.effT = e.EffT
	}
	v.effC = e.EffC
	if *e == before {
		return false
	}
	if neFidxEffTimer == nil {
		neFidxEffTimer = time.AfterFunc(5*time.Second, neFidxEffSave)
	}
	return true
}

// neFidxBinBlk — the ";blk=" part of a bin's state: the effective gen when the table covers the gen the peer
// serves for the block now, else that block gen itself (unknown content: the state moves until the body is parsed).
func neFidxBinBlk(i, j int, th string) string {
	g := neFidxBlockGenOfBin(i, j)
	if g == "" {
		return g
	}
	neFidxEffLoad()
	neFidxEffMu.RLock()
	defer neFidxEffMu.RUnlock()
	e := neFidxEff[[2]int{i, j}]
	if e == nil {
		return g
	}
	if th == "" {
		if e.Gen == g && e.EffC != "" {
			return e.EffC
		}
		return g
	}
	if e.GenT == g && e.EffT != "" {
		return e.EffT
	}
	return g
}

// neFidxEffCovers — does the table know the content of bin (i,j) at the gen the peer serves now (for the
// field set `th` stands for)? false = a fetch of the block may put the bin back on a state we hold.
func neFidxEffCovers(i, j int, th string) bool {
	g := neFidxBlockGenOfBin(i, j)
	if g == "" {
		return true // no manifest: nothing to learn
	}
	neFidxEffLoad()
	neFidxEffMu.RLock()
	defer neFidxEffMu.RUnlock()
	e := neFidxEff[[2]int{i, j}]
	if e == nil {
		return false
	}
	if th == "" {
		return e.Gen == g
	}
	return e.GenT == g
}

// neFidxBlockEff — what a box state reads for block (bi,bj): the block gen while every bin of it sits on that
// gen (or is unknown), else a digest of the 16 effective gens — so a block rebuilt to the same bytes moves no tile.
func neFidxBlockEff(bi, bj int, th string) string {
	g := neFidxBlockGen(bi, bj)
	if g == "" {
		return g
	}
	same := true
	var bl [16]string
	for a := 0; a < 4; a++ {
		for c := 0; c < 4; c++ {
			b := neFidxBinBlk(4*bi+a, 4*bj+c, th)
			bl[4*a+c] = b
			same = same && b == g
		}
	}
	if same {
		return g
	}
	h := sha256.New()
	for _, b := range bl {
		h.Write([]byte(b + ","))
	}
	return hex.EncodeToString(h.Sum(nil))[:24]
}

func neFidxEffStats() map[string]interface{} {
	neFidxEffLoad()
	neFidxEffMu.RLock()
	n, off := len(neFidxEff), 0
	for _, e := range neFidxEff {
		if e.EffC != e.Gen {
			off++
		}
	}
	neFidxEffMu.RUnlock()
	return map[string]interface{}{"bins": n, "cap": neFidxEffMax, "on_older_gen": off, // bins whose content outlived ≥ 1 block rebuild
		"regen_same": neFidxEffSame.Load(), "regen_moved": neFidxEffMoved.Load(), "first_seen": neFidxEffNew.Load()}
}
