// ne_mvt_samp.go — the NATIONAL 1-in-49 SAMPLE behind "every filter term at
// every zoom" (docs/ne-filter.md §8).
//
// One row per res-10 cell of the country = its res-12 CENTRE CHILD (digits 11
// and 12 zero; nothing coarser, the row is a real cell with real columns), with
// every column of the filter catalogue (neFiltFields, same bytes as a fidx bin:
// declared view, NEO1 obs row, chunk log, the two peers' national samples,
// Natura 2000, remoteness, the road/water raster) + cont/het/edge from a
// union-find over the whole KG. ≈ 5.6 M rows. Built in the national pass
// (ne_mvt_nat.go: the same loop over 7 850 KGs feeds neSampBuilder.kg), so
// the sample and the exact d/o/chg table never disagree.
//
// Disk: data/ne_mvt/samp.bin = zstd of the planar image (≈ 70 MB); the two
// peer bodies as fetched (zstd, ETag kept next to them: a rebuild asks with
// If-None-Match and moves no bytes while the peer epoch stands).
// Memory: the planar image lives in one anonymous mmap (≈ 310 MB resident,
// zero Go heap, no GC scan) — size line ne_mvt.sample on the inflight page.
//
// Layout of the image (little endian):
//
//	"NSMP" u8 ver u32 hlen hdr(JSON)
//	ids9 u64[n9] · lon9 u16[n9] · lat9 u16[n9] · start9 u32[n9+1]   the res-9 parents (sorted) and their row runs
//	d10 u8[n]                                                       digit 10 of the row (row id = parent9 ⊕ d10 ⊕ centre child)
//	cols: planar, neFiltFields order (n2ks as u16: national site index)
//
// Rows are id-sorted, so res-9 (and res-7) folds are run scans, not maps.
// Queries (ne_glyph_fnat.go, api_ne_filter.go, api_ne_stats_unit.go) scale
// the counts by 49 and say approx/sample.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/uber/h3-go/v4"
)

const (
	neSampVer    = 1
	neSampMagic  = "NSMP"
	neSampK      = 49 // res-12 cells per res-10 cell: one sample stands for 49
	neSampLon0   = 9.0
	neSampLonW   = 9.0
	neSampLat0   = 46.0
	neSampLatH   = 4.0
	neSampMaxBox = 2.5 // bbox span (°) a res-9 fold answers; larger views use the res-7 fold
)

// neSampCols — the catalogue columns with the sample's dtypes (n2ks is a u16 here).
var neSampCols = func() []neFidxCol {
	out := make([]neFidxCol, len(neFiltFields))
	for i, f := range neFiltFields {
		dt := f.Dtype
		if f.ID == "n2ks" {
			dt = "u16"
		}
		out[i] = neFidxCol{ID: f.ID, DT: dt}
	}
	return out
}()

// neSampRec — record bytes and column offsets inside one row-major record (build only).
var neSampRec, neSampOff = func() (int, map[string]int) {
	off := map[string]int{}
	n := 0
	for _, c := range neSampCols {
		off[c.ID] = n
		n += neFidxColSize(c.DT)
	}
	return n, off
}()

type neSampHdr struct {
	State    string      `json:"state"`
	NatState string      `json:"nat_state"`
	Built    string      `json:"built"`
	N        int         `json:"n"`
	N9       int         `json:"n9"`
	Cols     []neFidxCol `json:"cols"`
	N2K      []string    `json:"n2k"`
	Obs      interface{} `json:"obs"`
	Sub      interface{} `json:"sub"`
	Dist     string      `json:"dist"`
	Ms       int64       `json:"ms"`
	KGs      int         `json:"kgs"`
	Cells    int         `json:"cells"`
}

// neSamp — the live store: slices into one anonymous mapping.
type neSamp struct {
	Hdr    neSampHdr
	data   []byte
	ids9   []uint64
	lon9   []uint16
	lat9   []uint16
	start9 []uint32
	d10    []byte
	cols   map[string][]byte
	built  time.Time
}

func (s *neSamp) col(id string) []byte { return s.cols[id] }
func (s *neSamp) n() int               { return s.Hdr.N }
func (s *neSamp) sites() []string      { return s.Hdr.N2K }

// termFn — the n2ks column is a u16 national site index here (a bin's is a
// u8 into its own list): the catalogue dtype does not apply, so the term is
// compiled over the store's own column.
func (s *neSamp) termFn(t neFiltTerm) (func(int) bool, bool) {
	if t.F != "n2ks" {
		return nil, false
	}
	c := s.cols["n2ks"]
	if c == nil {
		return nil, false
	}
	get := func(k int) int { return int(binary.LittleEndian.Uint16(c[2*k:])) }
	if t.Unk {
		return func(k int) bool { return get(k) == 65535 }, true
	}
	if t.Flag {
		return func(k int) bool { return get(k) != 65535 }, true
	}
	want := map[int]bool{}
	for i, code := range s.Hdr.N2K {
		for _, m := range t.Set {
			if m == code {
				want[i] = true
			}
		}
	}
	if len(want) == 0 {
		return func(int) bool { return false }, true
	}
	return func(k int) bool { v := get(k); return v != 65535 && want[v] }, true
}

// id — the res-12 id of row k (binary search of its res-9 run).
func (s *neSamp) id(k int) uint64 {
	q := sort.Search(len(s.ids9), func(i int) bool { return int(s.start9[i+1]) > k })
	return neSampRowID(s.ids9[q], s.d10[k])
}

// neSampRowID — parent9 ⊕ digit 10 ⊕ centre child at res 12.
func neSampRowID(p9 uint64, d10 byte) uint64 {
	return (p9 &^ (0xF << 52) &^ 0x3FFFF) | (12 << 52) | (uint64(d10&7) << 15) | 0x1FF
}

// neSampIsCentre — a res-12 id whose digits 11 and 12 are zero: the centre child of its res-10 parent.
func neSampIsCentre(id uint64) bool { return id&(0x3F<<9) == 0 }

func neSampQLon(lon float64) uint16 {
	return uint16(math.Max(0, math.Min(65535, math.Round((lon-neSampLon0)/neSampLonW*65535))))
}
func neSampQLat(lat float64) uint16 {
	return uint16(math.Max(0, math.Min(65535, math.Round((lat-neSampLat0)/neSampLatH*65535))))
}
func neSampLon(q uint16) float64 { return neSampLon0 + float64(q)/65535*neSampLonW }
func neSampLat(q uint16) float64 { return neSampLat0 + float64(q)/65535*neSampLatH }

// inBox9 — res-9 indices whose centre lies in the box (half-open), one scan.
func (s *neSamp) inBox9(w, so, e, n float64) []int {
	qw, qs, qe, qn := neSampQLon(w), neSampQLat(so), neSampQLon(e), neSampQLat(n)
	var out []int
	for i := range s.ids9 {
		if lo, la := s.lon9[i], s.lat9[i]; lo >= qw && lo < qe && la >= qs && la < qn {
			out = append(out, i)
		}
	}
	return out
}

// ── live store ───────────────────────────────────────────────────────────────

var (
	neSampMu      sync.RWMutex
	neSampCur     *neSamp
	neSampQueries atomic.Int64
	neSampEvalMs  atomic.Int64
	neSampOnce    sync.Once
	// fold9 per canonical expression: one byte per res-9 parent (≤ 7 matching samples) — 0.8 MB each, ≤ 16 MB
	neSampFoldLRU = newNELRU(16 << 20)
)

func neSampPath() string { return filepath.Join(nePMTDir, "samp.bin") }

func neSampGet() *neSamp {
	neSampMu.RLock()
	defer neSampMu.RUnlock()
	return neSampCur
}

// neSampStart — load the file once (called from neNatStart).
func neSampStart() {
	neSampOnce.Do(func() {
		if s, err := neSampLoad(neSampPath()); err == nil {
			neSampSet(s)
			log.Printf("🎯 national sample loaded: %d rows, %d res-9 parents, state %s", s.Hdr.N, s.Hdr.N9, s.Hdr.State[:min(12, len(s.Hdr.State))])
		} else if !errors.Is(err, os.ErrNotExist) {
			log.Printf("national sample: %v (rebuilding with the table)", err)
		}
	})
}

func neSampSet(s *neSamp) {
	neSampMu.Lock()
	old := neSampCur
	neSampCur = s
	neSampMu.Unlock()
	neSampFoldLRU.dropPrefix("")
	if old != nil && old.data != nil {
		// readers hold the slices for a request at most; release the old image a little later
		go func(b []byte) { time.Sleep(2 * time.Minute); syscall.Munmap(b) }(old.data)
	}
}

// neSampAnon — n bytes outside the Go heap.
func neSampAnon(n int) ([]byte, error) {
	if n <= 0 {
		return nil, errors.New("empty")
	}
	return syscall.Mmap(-1, 0, n, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
}

// NESampStats — size line ne_mvt.sample (invariant 10).
func NESampStats() map[string]interface{} {
	s := neSampGet()
	m := map[string]interface{}{"loaded": s != nil, "queries": neSampQueries.Load(), "eval_ms_sum": neSampEvalMs.Load(), "fold_lru": neSampFoldLRU.stats(), "dist": NEDistStats()}
	if s != nil {
		m["bytes_mmap"] = len(s.data)
		m["rows"], m["res9"], m["state"], m["built"], m["build_ms"] = s.Hdr.N, s.Hdr.N9, s.Hdr.State, s.Hdr.Built, s.Hdr.Ms
		m["obs"], m["sub"], m["n2k_sites"] = s.Hdr.Obs, s.Hdr.Sub, len(s.Hdr.N2K)
		if st, err := os.Stat(neSampPath()); err == nil {
			m["bytes_disk"] = st.Size()
		}
	}
	m["peers"] = neSampPeerStats()
	return m
}

// ── image ↔ store ─────────────────────────────────────────────────────────────

// neSampAttach — slices over an image (validated); the image must outlive the store.
func neSampAttach(data []byte) (*neSamp, error) {
	if len(data) < 9 || string(data[:4]) != neSampMagic || data[4] != neSampVer {
		return nil, errors.New("not a national sample")
	}
	hl := int(binary.LittleEndian.Uint32(data[5:]))
	if 9+hl > len(data) {
		return nil, errors.New("truncated header")
	}
	s := &neSamp{data: data, cols: map[string][]byte{}}
	if err := json.Unmarshal(data[9:9+hl], &s.Hdr); err != nil {
		return nil, err
	}
	n, n9 := s.Hdr.N, s.Hdr.N9
	if n <= 0 || n9 <= 0 || n9 > n {
		return nil, errors.New("bad counts")
	}
	o := 9 + hl
	need := 8*n9 + 2*n9 + 2*n9 + 4*(n9+1) + n
	for _, c := range s.Hdr.Cols {
		need += n * neFidxColSize(c.DT)
	}
	if o+need != len(data) {
		return nil, fmt.Errorf("image length %d, want %d", len(data), o+need)
	}
	s.ids9 = make([]uint64, n9)
	for i := range s.ids9 {
		s.ids9[i] = binary.LittleEndian.Uint64(data[o+8*i:])
	}
	o += 8 * n9
	s.lon9 = make([]uint16, n9)
	s.lat9 = make([]uint16, n9)
	for i := range s.lon9 {
		s.lon9[i] = binary.LittleEndian.Uint16(data[o+2*i:])
		s.lat9[i] = binary.LittleEndian.Uint16(data[o+2*n9+2*i:])
	}
	o += 4 * n9
	s.start9 = make([]uint32, n9+1)
	for i := range s.start9 {
		s.start9[i] = binary.LittleEndian.Uint32(data[o+4*i:])
	}
	o += 4 * (n9 + 1)
	if s.start9[0] != 0 || int(s.start9[n9]) != n {
		return nil, errors.New("bad res-9 runs")
	}
	s.d10 = data[o : o+n]
	o += n
	for _, c := range s.Hdr.Cols {
		sz := n * neFidxColSize(c.DT)
		s.cols[c.ID] = data[o : o+sz]
		o += sz
	}
	s.built, _ = time.Parse(time.RFC3339, s.Hdr.Built)
	return s, nil
}

// neSampLoad — zstd file → anonymous mapping → store. (The small index slices
// above are copied onto the heap: 4 × 0.8 M entries ≈ 15 MB, the price of
// typed access; the 300 MB of columns stay in the mapping.)
func neSampLoad(path string) (*neSamp, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec, err := zstd.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	// the first frame header carries the image size (neSampSave sets it): map that much, stream into it
	var magic [8]byte
	if _, err := io.ReadFull(dec, magic[:]); err != nil {
		return nil, err
	}
	if string(magic[:4]) != neSampMagic || magic[4] != neSampVer {
		return nil, errors.New("not a national sample")
	}
	st, _ := f.Stat()
	size := neSampSizeHint(path)
	if size <= 0 {
		size = int(st.Size()) * 8
	}
	data, err := neSampAnon(size)
	if err != nil {
		return nil, err
	}
	copy(data, magic[:])
	n := 8
	for {
		if n == len(data) {
			// hint too small: grow (rare — only when the hint file is missing)
			big, err := neSampAnon(len(data) * 2)
			if err != nil {
				syscall.Munmap(data)
				return nil, err
			}
			copy(big, data)
			syscall.Munmap(data)
			data = big
		}
		r, err := dec.Read(data[n:])
		n += r
		if err == io.EOF {
			break
		}
		if err != nil {
			syscall.Munmap(data)
			return nil, err
		}
	}
	if n != len(data) {
		// trim to the exact image: remap (the kernel keeps the pages)
		exact, err := neSampAnon(n)
		if err != nil {
			syscall.Munmap(data)
			return nil, err
		}
		copy(exact, data[:n])
		syscall.Munmap(data)
		data = exact
	}
	s, err := neSampAttach(data)
	if err != nil {
		syscall.Munmap(data)
		return nil, err
	}
	return s, nil
}

func neSampSizeHint(path string) int {
	b, err := os.ReadFile(path + ".size")
	if err != nil {
		return 0
	}
	var n int
	fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &n)
	return n
}

// neSampSave — the image as one zstd stream + its decoded size next to it.
func neSampSave(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path + ".tmp")
	if err != nil {
		return err
	}
	enc, err := zstd.NewWriter(f, zstd.WithEncoderLevel(zstd.SpeedBetterCompression), zstd.WithEncoderConcurrency(1))
	if err != nil {
		f.Close()
		return err
	}
	if _, err := enc.Write(data); err != nil {
		enc.Close()
		f.Close()
		return err
	}
	if err := enc.Close(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	os.WriteFile(path+".size", []byte(fmt.Sprintf("%d\n", len(data))), 0o644)
	return os.Rename(path+".tmp", path)
}

// ── builder (fed by the national pass) ───────────────────────────────────────

type neSampBuilder struct {
	f      *os.File
	w      *bufio.Writer
	path   string
	out    string // where finish saves (neSampPath(); tests redirect)
	ids    []uint64
	rec    []byte
	idx    map[uint64]int32 // KG rows by id (reused)
	kgs    int
	cells  int
	t0     time.Time
	failed error
}

func neSampBegin() *neSampBuilder {
	os.MkdirAll(nePMTDir, 0o755)
	p := filepath.Join(nePMTDir, "samp.rows.tmp")
	f, err := os.Create(p)
	if err != nil {
		return &neSampBuilder{failed: err}
	}
	return &neSampBuilder{f: f, w: bufio.NewWriterSize(f, 1<<20), path: p, out: neSampPath(), rec: make([]byte, neSampRec), idx: map[uint64]int32{}, t0: time.Now()}
}

// kg — one KG of the national pass: rows = the declared view (id-sorted,
// neLURowBytes each), obs = its obs layer rows (id-sorted, lay.rowBytes each)
// or nil, cum = the change state. Emits the KG's centre-child rows.
func (b *neSampBuilder) kg(kg string, rows, obs []byte, lay *neObsLayoutT, cum map[uint64]*neCellCum) {
	if b.failed != nil {
		return
	}
	n := len(rows) / neLURowBytes
	if n == 0 {
		return
	}
	b.kgs++
	clear(b.idx)
	dAll := make([]uint8, n)
	idsKG := make([]uint64, n)
	for r := 0; r < n; r++ {
		row := rows[r*neLURowBytes:]
		id := binary.LittleEndian.Uint64(row)
		idsKG[r] = id
		b.idx[id] = int32(r)
		dAll[r] = 255
		if best, _, sum := neFidxLUBest(row[12:21]); sum > 0 {
			dAll[r] = uint8(best)
		}
	}
	// union-find over same-group neighbours; a neighbour outside the KG marks the component as touching the border
	parent := make([]int32, n)
	size := make([]int32, n)
	border := make([]bool, n)
	for r := range parent {
		parent[r], size[r] = int32(r), 1
	}
	var find func(int32) int32
	find = func(x int32) int32 {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	union := func(a, c int32) {
		ra, rc := find(a), find(c)
		if ra == rc {
			return
		}
		if size[ra] < size[rc] {
			ra, rc = rc, ra
		}
		parent[rc] = ra
		size[ra] += size[rc]
		border[ra] = border[ra] || border[rc]
	}
	v41 := lay != nil && lay.rowBytes == neObsRecordV41 && lay.luOff >= 0
	obsN := 0
	if obs != nil && lay != nil && lay.rowBytes > 0 {
		obsN = len(obs) / lay.rowBytes
	}
	type samp struct {
		r    int32
		orow []byte
	}
	var samples []samp
	oi := 0
	for r := 0; r < n; r++ {
		id := idsKG[r]
		if dAll[r] != 255 {
			nb, err := h3.Cell(id).GridDisk(1)
			if err == nil {
				for _, c := range nb {
					if uint64(c) == id {
						continue
					}
					q, ok := b.idx[uint64(c)]
					if !ok {
						border[find(int32(r))] = true
						continue
					}
					if dAll[q] == dAll[r] && q > int32(r) {
						union(int32(r), q)
					}
				}
			}
		}
		if !neSampIsCentre(id) || rows[r*neLURowBytes+8] < 128 {
			continue
		}
		var orow []byte
		for oi < obsN {
			oid := binary.LittleEndian.Uint64(obs[oi*lay.rowBytes:])
			if oid < id {
				oi++
				continue
			}
			if oid == id {
				orow = obs[oi*lay.rowBytes : (oi+1)*lay.rowBytes]
			}
			break
		}
		samples = append(samples, samp{int32(r), orow})
	}
	rec := b.rec
	for _, sm := range samples {
		r := int(sm.r)
		id := idsKG[r]
		row := rows[r*neLURowBytes : (r+1)*neLURowBytes]
		for i := range rec {
			rec[i] = 255
		}
		for _, f := range neFiltFields {
			if f.Dtype == "i8" {
				rec[neSampOff[f.ID]] = 0x80
			} else if f.Nodata == 0 {
				rec[neSampOff[f.ID]] = 0
			}
		}
		put := func(id string, v byte) { rec[neSampOff[id]] = v }
		put16 := func(id string, v uint16) { binary.LittleEndian.PutUint16(rec[neSampOff[id]:], v) }
		if best, _, sum := neFidxLUBest(row[12:21]); sum > 0 {
			put("d", uint8(best))
			put("dsh", uint8(min(int(row[12+best])*255/sum, 254)))
		}
		if ob := sm.orow; ob != nil {
			if best, _, sum := neFidxLUBest(ob[lay.luOff : lay.luOff+9]); sum > 0 {
				put("o", uint8(best))
				put("osh", uint8(min(int(ob[lay.luOff+best])*255/sum, 254)))
			}
			if v41 {
				for _, m := range neFidxObsMap {
					put(m.id, ob[m.off])
				}
				if ob[28] != 0 {
					put("als", ob[28])
				}
				put16("el", binary.LittleEndian.Uint16(ob[30:]))
				if ob[39] != 0 {
					put("loss", ob[39])
				}
			}
		}
		put("chg", 0)
		if cu := cum[id]; cu != nil && cu.Share >= 128 {
			if cu.Confirmed {
				put("chg", 2)
			} else if cu.Candidate {
				put("chg", 1)
			}
		}
		// cont / het / edge
		if dAll[r] != 255 {
			root := find(int32(r))
			sz := int(size[root])
			if sz > 64 {
				sz = 64
			}
			c := uint8(sz)
			if border[root] {
				c |= 128
			}
			put("cont", c)
			nb, err := h3.Cell(id).GridDisk(1)
			if err == nil {
				het, edge, miss := 0, 0, false
				var seen [10]bool
				for _, cc := range nb {
					if uint64(cc) == id {
						continue
					}
					q, ok := b.idx[uint64(cc)]
					if !ok {
						miss = true
						break
					}
					g := dAll[q]
					if g != dAll[r] {
						edge++
						gi := int(g)
						if gi > 9 {
							gi = 9
						}
						if !seen[gi] {
							seen[gi] = true
							het++
						}
					}
				}
				if !miss {
					put("het", uint8(het))
					put("edge", uint8(edge))
				}
			}
		}
		if _, err := b.w.Write(rec); err != nil {
			b.failed = err
			return
		}
		b.ids = append(b.ids, id)
		b.cells++
	}
}

// finish — sort, index by res-9 parent, transpose to planar, join the peers,
// Natura 2000, remoteness, road/water; save; return the live store.
func (b *neSampBuilder) finish(natState string) (*neSamp, error) {
	if b.failed != nil {
		return nil, b.failed
	}
	defer os.Remove(b.path)
	if err := b.w.Flush(); err != nil {
		return nil, err
	}
	if err := b.f.Close(); err != nil {
		return nil, err
	}
	nAll := len(b.ids)
	if nAll == 0 {
		return nil, errors.New("no sample rows")
	}
	perm := make([]int32, nAll)
	for i := range perm {
		perm[i] = int32(i)
	}
	sort.Slice(perm, func(a, c int) bool { return b.ids[perm[a]] < b.ids[perm[c]] })
	// a border cell built by two KGs appears twice (≈ 80 of 5.6 M): keep the first row
	// so every id is one row (peers' cell lists count unique ids; our counts must agree)
	perm = slices.CompactFunc(perm, func(a, c int32) bool { return b.ids[a] == b.ids[c] })
	n := len(perm)
	// res-9 runs
	var ids9 []uint64
	var start9 []uint32
	last := uint64(0)
	for k, p := range perm {
		p9 := neH3ParentFast(b.ids[p], 9)
		if k == 0 || p9 != last {
			ids9 = append(ids9, p9)
			start9 = append(start9, uint32(k))
			last = p9
		}
	}
	start9 = append(start9, uint32(n))
	n9 := len(ids9)
	// header (state is final below — reserve with the fields known now)
	hdr := neSampHdr{NatState: natState, Built: time.Now().UTC().Format(time.RFC3339), N: n, N9: n9, Cols: neSampCols, N2K: []string{}, KGs: b.kgs, Cells: b.cells}
	// peers (fetched to disk once per epoch; parsed here)
	obsS, obsMeta := neSampPeerObs()
	subS, subMeta := neSampPeerSub()
	hdr.Obs, hdr.Sub = obsMeta, subMeta
	dist := neDistGet()
	if dist != nil {
		hdr.Dist = dist.key
	}
	loadNatura2000()
	for si := range natura2000Sites {
		hdr.N2K = append(hdr.N2K, natura2000Sites[si].SiteCode)
	}
	state := neSampState(natState, obsMeta, subMeta, hdr.Dist)
	hdr.State = state
	hdrJSON, _ := json.Marshal(hdr)
	// the image
	colsBytes := 0
	for _, c := range neSampCols {
		colsBytes += n * neFidxColSize(c.DT)
	}
	total := 9 + len(hdrJSON) + 8*n9 + 4*n9 + 4*(n9+1) + n + colsBytes
	data, err := neSampAnon(total)
	if err != nil {
		return nil, err
	}
	copy(data, neSampMagic)
	data[4] = neSampVer
	binary.LittleEndian.PutUint32(data[5:], uint32(len(hdrJSON)))
	copy(data[9:], hdrJSON)
	o := 9 + len(hdrJSON)
	for i, id := range ids9 {
		binary.LittleEndian.PutUint64(data[o+8*i:], id)
	}
	o += 8 * n9
	for i, id := range ids9 {
		lon, lat := neCellCentre(id)
		binary.LittleEndian.PutUint16(data[o+2*i:], neSampQLon(lon))
		binary.LittleEndian.PutUint16(data[o+2*n9+2*i:], neSampQLat(lat))
	}
	o += 4 * n9
	for i, v := range start9 {
		binary.LittleEndian.PutUint32(data[o+4*i:], v)
	}
	o += 4 * (n9 + 1)
	for k, p := range perm {
		data[o+k] = byte((b.ids[p] >> 15) & 7)
	}
	o += n
	// transpose the row-major temp file (mapped read-only) into the planar columns
	tf, err := os.Open(b.path)
	if err != nil {
		syscall.Munmap(data)
		return nil, err
	}
	rows, err := syscall.Mmap(int(tf.Fd()), 0, nAll*neSampRec, syscall.PROT_READ, syscall.MAP_PRIVATE)
	tf.Close()
	if err != nil {
		syscall.Munmap(data)
		return nil, err
	}
	cols := map[string][]byte{}
	for _, c := range neSampCols {
		sz := neFidxColSize(c.DT)
		dst := data[o : o+n*sz]
		cols[c.ID] = dst
		off := neSampOff[c.ID]
		if sz == 1 {
			for k, p := range perm {
				dst[k] = rows[int(p)*neSampRec+off]
			}
		} else {
			for k, p := range perm {
				copy(dst[2*k:2*k+2], rows[int(p)*neSampRec+off:])
			}
		}
		o += n * sz
	}
	syscall.Munmap(rows)
	if o != total {
		syscall.Munmap(data)
		return nil, errors.New("image size mismatch")
	}
	// sorted ids (heap, transient) for the joins
	ids := make([]uint64, n)
	for k, p := range perm {
		ids[k] = b.ids[p]
	}
	perm = nil
	b.ids = nil
	put16 := func(id string, k int, v uint16) { binary.LittleEndian.PutUint16(cols[id][2*k:], v) }
	// observed-layer peer sample
	if obsS != nil && len(obsS.ids) > 0 {
		j := 0
		for k, id := range ids {
			for j < len(obsS.ids) && obsS.ids[j] < id {
				j++
			}
			if j >= len(obsS.ids) || obsS.ids[j] != id {
				continue
			}
			x := j
			if c := obsS.cols["bld_h_max"]; c != nil {
				cols["hb"][k] = c[x]
			}
			if c := obsS.cols["bld_h_p50"]; c != nil {
				cols["hbm"][k] = c[x]
			}
			if c := obsS.cols["stories_max"]; c != nil {
				cols["stor"][k] = c[x]
			}
			if c := obsS.cols["roof_dom"]; c != nil {
				cols["roof"][k] = c[x]
			}
			if c := obsS.cols["canopy_h_p90"]; c != nil {
				cols["hc"][k] = c[x]
			}
			if c := obsS.cols["aspect_deg"]; c != nil {
				if c[x] == 254 {
					cols["asd"][k] = 255
				} else {
					cols["asd"][k] = c[x]
				}
			}
			if cols["sl"][k] == 255 { // no obs row: terrain from the peer
				if c := obsS.cols["slope_mean"]; c != nil {
					cols["sl"][k] = c[x]
				}
				if c := obsS.cols["slope_p90"]; c != nil {
					cols["sl90"][k] = c[x]
				}
				if c := obsS.cols["elev_mean"]; c != nil {
					put16("el", k, binary.LittleEndian.Uint16(c[2*x:]))
				}
				if c := obsS.cols["aspect_deg"]; c != nil && c[x] != 255 {
					if c[x] == 254 {
						cols["as"][k] = 8
					} else {
						cols["as"][k] = uint8(((int(c[x])*2 + 22) / 45) % 8)
					}
				}
			}
		}
	}
	// subsidy peer sample: a row = claimed; the peer holding the sample and no row = not claimed
	if subS != nil {
		j := 0
		for k, id := range ids {
			for j < len(subS.Cells) && subS.Cells[j] < id {
				j++
			}
			if j < len(subS.Cells) && subS.Cells[j] == id && subS.Cov[j] != 0 {
				x := j
				cols["sub"][k] = subS.Cls[x] & 7
				cols["scov"][k] = subS.Cov[x]
				if subS.Bio[x] != 255 {
					cols["bio"][k] = b2u(subS.Bio[x] >= 128)
				}
				cols["srel"][k] = b2u(subS.Rel[x] < neSubRelDubious)
				cols["schg"][k] = uint8(min(int(subS.Chg[x]), 5))
				if subS.Snar != nil {
					put16("snar", k, subS.Snar[x])
					put16("snar2", k, subS.Snar2[x])
					cols["oepul"][k] = subS.Oepul[x]
					cols["nutz"][k] = subS.Nutz[x]
				}
			} else {
				cols["sub"][k], cols["scov"][k] = 0, 0
				if subS.Snar != nil {
					put16("snar", k, 0)
					put16("snar2", k, 0)
					cols["oepul"][k], cols["nutz"][k] = 0, 0
				}
			}
		}
	}
	// positions (transient) for the point lookups
	lon := make([]float32, n)
	lat := make([]float32, n)
	for k, id := range ids {
		lo, la := neCellCentre(id)
		lon[k], lat[k] = float32(lo), float32(la)
	}
	ids = nil
	// Natura 2000: every row inside a site's bbox is tested against the site's 25 m raster
	for k := 0; k < n; k++ {
		cols["n2k"][k], cols["n2kt"][k] = 0, 0
	}
	if natura2000Loaded {
		// rows by 0.1° cell → the candidates of a site are a few cells, not a scan per site
		cell := func(lo, la float32) int { return int((lo-9)*10)*40 + int((la-46)*10) }
		buckets := map[int][]int32{}
		for k := 0; k < n; k++ {
			c := cell(lon[k], lat[k])
			buckets[c] = append(buckets[c], int32(k))
		}
		for si := range natura2000Sites {
			st := &natura2000Sites[si]
			g := newNEGrid(st.MinLon-0.001, st.MinLat-0.001, st.MaxLon+0.001, st.MaxLat+0.001)
			for _, poly := range st.Polygons {
				g.fillRings(poly, 1)
			}
			typ := uint8(0)
			switch st.SiteType {
			case "A":
				typ = 1
			case "B":
				typ = 2
			case "C":
				typ = 3
			}
			for cx := int((st.MinLon - 9) * 10); cx <= int((st.MaxLon-9)*10); cx++ {
				for cy := int((st.MinLat - 46) * 10); cy <= int((st.MaxLat-46)*10); cy++ {
					for _, k32 := range buckets[cx*40+cy] {
						k := int(k32)
						x, y, ok := g.at(float64(lon[k]), float64(lat[k]))
						if !ok || g.v[y*g.gw+x] == 0 {
							continue
						}
						if cols["n2k"][k] == 0 || typ == 3 {
							cols["n2k"][k] = 1
							cols["n2kt"][k] = typ
						}
						if binary.LittleEndian.Uint16(cols["n2ks"][2*k:]) == 65535 {
							put16("n2ks", k, uint16(si))
						}
					}
				}
			}
		}
	}
	// remoteness, road / water classes
	rg := remoteGridGet()
	for k := 0; k < n; k++ {
		lo, la := float64(lon[k]), float64(lat[k])
		if rg != nil {
			if v := rg.At(lo, la); v >= 0 {
				cols["rem"][k] = uint8(v)
			}
		}
		if dist != nil {
			rd, wat := dist.At(lo, la)
			cols["rd"][k], cols["wat"][k] = rd, wat
		}
	}
	lon, lat = nil, nil
	s, err := neSampAttach(data)
	if err != nil {
		syscall.Munmap(data)
		return nil, err
	}
	s.Hdr.Ms = time.Since(b.t0).Milliseconds()
	if err := neSampSave(b.out, data); err != nil {
		log.Printf("national sample: save: %v", err)
	}
	return s, nil
}

// neSampState — what the store is keyed on.
func neSampState(natState string, obs, sub interface{}, dist string) string {
	b, _ := json.Marshal([]interface{}{neSampVer, natState, obs, sub, dist, neFidxVer})
	sum := sha256.Sum256(b)
	return "samp" + fmt.Sprint(neSampVer) + ":" + hex.EncodeToString(sum[:8])
}

// ── peers' national samples (one body per epoch, kept on disk) ──────────────

var (
	neSampPeerMu     sync.Mutex
	neSampPeerLast   = map[string]string{} // name → last check outcome
	neSampPeerClient = &http.Client{Timeout: 5 * time.Minute}
)

func neSampPeerStats() map[string]interface{} {
	neSampPeerMu.Lock()
	defer neSampPeerMu.Unlock()
	m := map[string]interface{}{}
	for k, v := range neSampPeerLast {
		m[k] = v
	}
	for _, name := range []string{"obs", "sub"} {
		if st, err := os.Stat(filepath.Join(nePMTDir, "samp_"+name+".src")); err == nil {
			m[name+"_bytes_disk"] = st.Size()
		}
		if b, err := os.ReadFile(filepath.Join(nePMTDir, "samp_"+name+".etag")); err == nil {
			m[name+"_etag"] = strings.TrimSpace(string(b))
		}
	}
	return m
}

// neSampPeerFetch — conditional GET of a peer's national sample into
// data/ne_mvt/samp_<name>.src (zstd) + .etag. Returns whether the file on
// disk changed. A peer without the endpoint (404), down or malformed leaves
// what is on disk (or nothing) — never an error for the build.
func neSampPeerFetch(name, url string) bool {
	if url == "" {
		return false
	}
	src := filepath.Join(nePMTDir, "samp_"+name+".src")
	etagP := src[:len(src)-4] + ".etag"
	etag := ""
	if b, err := os.ReadFile(etagP); err == nil {
		etag = strings.TrimSpace(string(b))
	}
	note := func(s string) {
		neSampPeerMu.Lock()
		neSampPeerLast[name] = time.Now().UTC().Format(time.RFC3339) + " " + s
		neSampPeerMu.Unlock()
	}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "umfeld-at/sample (+"+publicURL()+")")
	req.Header.Set("Accept-Encoding", "zstd")
	if etag != "" {
		if _, err := os.Stat(src); err == nil {
			req.Header.Set("If-None-Match", etag)
		}
	}
	resp, err := neSampPeerClient.Do(req)
	if err != nil {
		note("down: " + err.Error())
		return false
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 304:
		note("304 unchanged")
		return false
	case 200:
	default:
		note(fmt.Sprintf("%d (kept what is on disk)", resp.StatusCode))
		return false
	}
	var rd io.Reader = io.LimitReader(resp.Body, 512<<20)
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "zstd") {
		dec, err := zstd.NewReader(rd)
		if err != nil {
			note("zstd: " + err.Error())
			return false
		}
		defer dec.Close()
		rd = dec
	}
	os.MkdirAll(nePMTDir, 0o755)
	f, err := os.Create(src + ".tmp")
	if err != nil {
		return false
	}
	enc, _ := zstd.NewWriter(f, zstd.WithEncoderConcurrency(1))
	nb, err := io.Copy(enc, rd)
	enc.Close()
	f.Close()
	if err != nil || nb < 8 {
		os.Remove(src + ".tmp")
		note("body: " + fmt.Sprint(err))
		return false
	}
	os.Rename(src+".tmp", src)
	newTag := resp.Header.Get("ETag")
	meta := map[string]string{}
	for _, h := range []string{"X-Cells-Epoch", "X-Cells-Gen", "X-Invekos-Release", "X-Invekos-Prev-Release", "X-Invekos-Version", "X-Invekos-Cols", "X-Invekos-Year", "X-Invekos-Gone"} {
		if v := resp.Header.Get(h); v != "" {
			meta[h] = v
			if newTag == "" {
				newTag = v
			}
		}
	}
	mj, _ := json.Marshal(meta)
	os.WriteFile(src[:len(src)-4]+".meta", mj, 0o644)
	os.WriteFile(etagP, []byte(newTag+"\n"), 0o644)
	note(fmt.Sprintf("200 %d bytes, etag %s", nb, newTag))
	return newTag != etag
}

func neSampPeerRead(name string) []byte {
	z, err := os.ReadFile(filepath.Join(nePMTDir, "samp_"+name+".src"))
	if err != nil {
		return nil
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil
	}
	defer dec.Close()
	b, err := dec.DecodeAll(z, nil)
	if err != nil {
		return nil
	}
	return b
}

func neSampPeerMeta(name string) map[string]string {
	m := map[string]string{}
	if b, err := os.ReadFile(filepath.Join(nePMTDir, "samp_"+name+".meta")); err == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

func neSampPeerTag(name string) string {
	b, _ := os.ReadFile(filepath.Join(nePMTDir, "samp_"+name+".etag"))
	return strings.TrimSpace(string(b))
}

func neSampObsURL() string {
	if p := peers().ObsAPI; p != "" {
		return p + "/api/v1/cells/sample?res=10&format=bin&fields=" + neFidxSrtmFlds // = the peer's "glyph" set
	}
	return ""
}

func neSampSubURL() string {
	if p := peers().SubAPI; p != "" {
		return p + "/api/v1/invekos/sample?res=10&format=bin"
	}
	return ""
}

// neSampPeersCheck — conditional GETs of both samples; true when either moved.
func neSampPeersCheck() bool {
	a := neSampPeerFetch("obs", neSampObsURL())
	b := neSampPeerFetch("sub", neSampSubURL())
	return a || b
}

// neSampPeerObs — the observed-layer peer's sample (ids normalised to the res-12 centre child, sorted) + meta for the header.
func neSampPeerObs() (*neFidxSrtm, interface{}) {
	body := neSampPeerRead("obs")
	if body == nil {
		return nil, nil
	}
	s := neFidxSrtmParse(body, neSampPeerTag("obs"))
	if s == nil {
		return nil, nil
	}
	neSampNormIDs(s.ids, s.cols)
	return s, map[string]interface{}{"epoch": s.epoch, "rows": len(s.ids)}
}

// neSampPeerSub — the subsidy peer's sample: "IVS1" u8 res u8 child_res u32 n,
// n LEB128 delta-coded res-10 ids (ascending), then one contiguous column per
// name of X-Invekos-Cols (cls cov bio rel chg dcov snar snar2 oepul nutz; u8
// except dcov i8 and snar/snar2 u16). Rows with cov 0 are gone claims.
func neSampPeerSub() (*neSubBin, interface{}) {
	body := neSampPeerRead("sub")
	if body == nil {
		return nil, nil
	}
	meta := neSampPeerMeta("sub")
	bin := neSampSubParse(body, meta["X-Invekos-Cols"])
	if bin == nil || bin.N == 0 {
		return nil, nil
	}
	if y, err := strconv.Atoi(meta["X-Invekos-Year"]); err == nil {
		bin.Year = y
	}
	return bin, map[string]interface{}{"release": neSampPeerTag("sub"), "rel": meta["X-Invekos-Release"], "rows": bin.N}
}

func neSampSubParse(b []byte, colsHdr string) *neSubBin {
	if len(b) < 10 || string(b[:4]) != "IVS1" {
		return nil
	}
	res, child := int(b[4]), int(b[5])
	n := int(binary.LittleEndian.Uint32(b[6:]))
	if res != 10 || child != 12 || n <= 0 || n > 50_000_000 {
		return nil
	}
	o := 10
	ids := make([]uint64, n)
	var acc uint64
	for k := 0; k < n; k++ {
		v, w := binary.Uvarint(b[o:])
		if w <= 0 {
			return nil
		}
		o += w
		acc += v
		ids[k] = acc
	}
	names := strings.Split(strings.ReplaceAll(colsHdr, " ", ""), ",")
	if colsHdr == "" {
		names = []string{"cls", "cov", "bio", "rel", "chg", "dcov", "snar", "snar2", "oepul", "nutz"}
	}
	cols := map[string][]byte{}
	for _, name := range names {
		sz := 1
		if name == "snar" || name == "snar2" {
			sz = 2
		}
		if o+n*sz > len(b) {
			return nil
		}
		cols[name] = b[o : o+n*sz]
		o += n * sz
	}
	for _, need := range []string{"cls", "cov", "bio", "rel", "chg"} {
		if cols[need] == nil {
			return nil
		}
	}
	perm := neSampNormIDs(ids, cols)
	_ = perm // columns were permuted in place with the ids
	bin := &neSubBin{N: n, Cells: ids, Cls: cols["cls"], Cov: cols["cov"], Bio: cols["bio"], Rel: cols["rel"], Chg: cols["chg"], Dcov: make([]int8, n)}
	if dc := cols["dcov"]; dc != nil {
		for k, v := range dc {
			bin.Dcov[k] = int8(v)
		}
	}
	if s1, s2 := cols["snar"], cols["snar2"]; s1 != nil && s2 != nil && cols["oepul"] != nil && cols["nutz"] != nil {
		bin.Snar, bin.Snar2 = make([]uint16, n), make([]uint16, n)
		for k := 0; k < n; k++ {
			bin.Snar[k] = binary.LittleEndian.Uint16(s1[2*k:])
			bin.Snar2[k] = binary.LittleEndian.Uint16(s2[2*k:])
		}
		bin.Oepul, bin.Nutz = cols["oepul"], cols["nutz"]
	}
	return bin
}

// neSampNormIDs — a peer may list the res-10 cell instead of its centre child:
// map to res 12, then make sure the rows are sorted (returns the permutation
// applied to the byte columns, nil when none was needed).
func neSampNormIDs(ids []uint64, cols map[string][]byte) []int {
	for k, id := range ids {
		if (id>>52)&0xF == 10 {
			ids[k] = neSampRowID(neH3ParentFast(id, 9), byte((id>>15)&7))
		}
	}
	if sort.SliceIsSorted(ids, func(a, b int) bool { return ids[a] < ids[b] }) {
		return nil
	}
	perm := make([]int, len(ids))
	for k := range perm {
		perm[k] = k
	}
	sort.Slice(perm, func(a, b int) bool { return ids[perm[a]] < ids[perm[b]] })
	sorted := make([]uint64, len(ids))
	for k, p := range perm {
		sorted[k] = ids[p]
	}
	copy(ids, sorted)
	for name, c := range cols {
		if len(c) == 0 || len(c)%len(perm) != 0 {
			continue
		}
		sz := len(c) / len(perm)
		nc := make([]byte, len(c))
		for k, p := range perm {
			copy(nc[k*sz:(k+1)*sz], c[p*sz:(p+1)*sz])
		}
		copy(c, nc)
		_ = name
	}
	return perm
}

// ── folds ────────────────────────────────────────────────────────────────────

// fold9 — matching samples per res-9 parent (0..7) for an expression; cached per state + canonical form.
func (s *neSamp) fold9(e *neFiltExpr) []uint8 {
	key := s.Hdr.State + "/" + e.Canonical()
	if b, ok := neSampFoldLRU.get(key); ok && len(b) == len(s.ids9) {
		return b
	}
	t0 := time.Now()
	mask := neFiltEval(s, e)
	out := make([]uint8, len(s.ids9))
	for i := range s.ids9 {
		c := uint8(0)
		for k := s.start9[i]; k < s.start9[i+1]; k++ {
			c += mask[k]
		}
		out[i] = c
	}
	neSampQueries.Add(1)
	neSampEvalMs.Add(time.Since(t0).Milliseconds())
	neSampFoldLRU.put(key, out)
	return out
}

// n9 — samples under res-9 parent i.
func (s *neSamp) n9(i int) int { return int(s.start9[i+1] - s.start9[i]) }

// fold7 — (match, samples) per res-7 cell of the glyph frame from a fold9.
func (s *neSamp) fold7(f9 []uint8) (match, n []int32) {
	fr := &neGlyphFrame
	match = make([]int32, len(fr.cells))
	n = make([]int32, len(fr.cells))
	last, fi := uint64(0), int32(-1)
	for i, id := range s.ids9 {
		p7 := neH3ParentFast(id, 7)
		if i == 0 || p7 != last {
			last = p7
			fi = -1
			if q, ok := fr.index[p7]; ok {
				fi = q
			}
		}
		if fi < 0 {
			continue
		}
		match[fi] += int32(f9[i])
		n[fi] += int32(s.n9(i))
	}
	return
}

// eval — the full mask (callers that need per-row membership: adm / bbox counts).
func (s *neSamp) eval(e *neFiltExpr) []uint8 {
	t0 := time.Now()
	m := neFiltEval(s, e)
	neSampQueries.Add(1)
	neSampEvalMs.Add(time.Since(t0).Milliseconds())
	return m
}

// rowsNear — row indices under the res-9 parents whose centre lies in the box
// padded by one res-9 cell (so every sample whose own centre is in the box is included).
func (s *neSamp) rowsNear(w, so, e, n float64) []int {
	const pad = 0.006
	var out []int
	for _, i := range s.inBox9(w-pad, so-pad, e+pad, n+pad) {
		for k := s.start9[i]; k < s.start9[i+1]; k++ {
			out = append(out, int(k))
		}
	}
	return out
}

// asBin — the store seen as a bin (neStatsAddCols reads b.Cols[id][k]); n2ks differs in width and is not read there.
func (s *neSamp) asBin() *neFidxBin {
	return &neFidxBin{N: s.Hdr.N, Cols: s.cols, Hdr: neFidxHdr{N2K: s.Hdr.N2K}}
}

// ── national figures from the sample (docs/ne-filter.md §8 step 4) ─────────

// neSampHist — the national histogram of every field over the sample: per
// field `known` (sampled cells with a value ×49), `unknown`, and `bins`: for
// set/bits/flag/bucket fields one count per member (bits: cells with the bit),
// for numbers 16 equal-width bins over [min,max] in raw units (`edges` raw).
// Cached per store state in neSampFoldLRU.
func neSampHist(sp *neSamp) map[string]interface{} {
	key := "hist/" + sp.Hdr.State
	if b, ok := neSampFoldLRU.get(key); ok {
		var m map[string]interface{}
		if json.Unmarshal(b, &m) == nil {
			return m
		}
	}
	out := map[string]interface{}{}
	n := sp.n()
	for _, f := range neFiltFields {
		c := sp.col(f.ID)
		if c == nil {
			continue
		}
		f := f
		sz := neFidxColSize(f.Dtype)
		h := map[string]interface{}{}
		known := 0
		switch f.Kind {
		case nfSet, nfBucket, nfFlag, nfSites:
			cnt := make([]int, len(f.Members)+1)
			if f.Kind == nfFlag || f.Kind == nfSites {
				cnt = make([]int, 2)
			}
			for k := 0; k < n; k++ {
				v := neFiltRaw(&f, c, k)
				if v == f.Nodata {
					continue
				}
				known++
				if f.Kind == nfFlag || f.Kind == nfSites {
					if v != 0 {
						cnt[1]++
					} else {
						cnt[0]++
					}
				} else if v < len(cnt) {
					cnt[v]++
				}
			}
			for i := range cnt {
				cnt[i] *= neSampK
			}
			h["bins"] = cnt
		case nfBits:
			cnt := make([]int, len(f.Members))
			for k := 0; k < n; k++ {
				v := int(c[k])
				if v == f.Nodata {
					continue
				}
				known++
				for b := range cnt {
					if v&(1<<uint(b)) != 0 {
						cnt[b]++
					}
				}
			}
			for i := range cnt {
				cnt[i] *= neSampK
			}
			h["bins"] = cnt
		default: // nfNum: raw min/max of the column, 16 bins
			lo, hi := math.MaxInt32, math.MinInt32
			for k := 0; k < n; k++ {
				if v := neFiltRaw(&f, c, k); v != f.Nodata {
					if v < lo {
						lo = v
					}
					if v > hi {
						hi = v
					}
				}
			}
			if lo > hi {
				lo, hi = 0, 0
			}
			const nb = 16
			cnt := make([]int, nb)
			edges := make([]float64, nb+1)
			sc := f.Scale
			if sc == 0 {
				sc = 1
			}
			for i := range edges {
				edges[i] = (float64(lo)+float64(hi-lo)*float64(i)/nb)*sc + f.Off
			}
			for k := 0; k < n; k++ {
				v := neFiltRaw(&f, c, k)
				if v == f.Nodata {
					continue
				}
				known++
				i := 0
				if hi > lo {
					i = (v - lo) * nb / (hi - lo)
				}
				if i < 0 {
					i = 0
				}
				if i >= nb {
					i = nb - 1
				}
				cnt[i]++
			}
			for i := range cnt {
				cnt[i] *= neSampK
			}
			h["bins"], h["edges"] = cnt, edges // edges in the field's unit
		}
		_ = sz
		h["known"], h["unknown"] = known*neSampK, (n-known)*neSampK
		out[f.ID] = h
	}
	b, _ := json.Marshal(out)
	neSampFoldLRU.put(key, b)
	return out
}

// neSampCountIn — the sample's cells (×49) inside a membership function, with
// the match of e and the declared-group split of the match.
func neSampCountIn(sp *neSamp, e *neFiltExpr, w, s, ee, n float64, member func(id uint64, lon, lat float64) float64) (cells, match int, ha float64, byD [9]int) {
	var mask []uint8
	if e != nil && !e.empty() {
		mask = sp.eval(e)
	}
	d := sp.col("d")
	for _, k := range sp.rowsNear(w, s, ee, n) {
		id := sp.id(k)
		lon, lat := neCellCentre(id)
		if member != nil && member(id, lon, lat) <= 0 {
			continue
		}
		if member == nil && (lon < w || lon >= ee || lat < s || lat >= n) {
			continue
		}
		cells += neSampK
		if mask != nil && mask[k] == 0 {
			continue
		}
		match += neSampK
		if a, err := h3.CellAreaM2(h3.Cell(id)); err == nil {
			ha += a * neSampK / 1e4
		}
		if d != nil && d[k] < 9 {
			byD[d[k]] += neSampK
		}
	}
	return
}
