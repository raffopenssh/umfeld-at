// ne_glyph_fidx.go — the glyph filter's index: per 0.025° bin (= LOD-0 tile =
// the peers' aligned bin), every held res-12 cell with ~50 quantised bytes, so
// a client (or /api/v1/ne/filter) evaluates any AND/OR expression locally.
// docs/ne-filter.md §3.
//
//	GET /api/v1/ne/glyph/fidx?i=&j=[&p=core|ext|all][&e=<state>]   application/vnd.ne-fidx
//
// Layout (NFX1, little endian):
//
//	"NFX1" u8 ver  u16 ncol  u32 n  u32 hlen  hdr[hlen] (JSON)
//	u16 nrow, per row: i16 j, u16 nruns, per run: i16 i0, u16 len     ← cells row-major in H3 local IJ
//	cols: ncol × n×size bytes, planar, header order
//
// hdr: {state, bbox, origin, a:[6|12] (i,j → frame x,y), hex:[[dx,dy]×6], cols:[{id,dt}],
//
//	n2k:[codes], sub:{year,release}|null, srtm:{epoch}|null, built_ms, unk}
//
// Cells are addressed by local IJ from a bin-centre origin: row runs replace
// per-cell ids (~1 KB a bin) and give the client adjacency for free. The pixel
// centre is the header polynomial; the server verifies the residual and
// upgrades affine → quadratic when any cell is off by > 0.3 px at LOD 1.
//
// Build: neCollect(lu+obs, bevFirst) of the bin plus a one-cell halo → subsidy
// bin (neSubLRU / peer) → srtm cells bin (one fetch per bin per epoch) → N2K
// membership (rasterised rings, 25 m grid) → road / water distance (OSM rtree,
// chamfer transform on the same grid) → change state (chunk log) → cont / het /
// edge (flood fill over bin + halo) → IJ + polynomial fit.
//
// Caches (both on the debug inflight page): RAM LRU ne_fidx (48 MB, the full
// blob per bin, planes are sliced on the way out), disk data/ne_fidx/<state>/
// <i>_<j>.zst (derived, rebuildable, capped at 2 GB, oldest state dirs first),
// single-flight per bin. Immutable when `e=` matches the bin's state.
package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
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
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/uber/h3-go/v4"
)

const (
	neFidxVer      = 2 // 2: bins built before e8d7338 (declared view serving log-only res-9 chunks) lacked those cells and their state hash did not move
	neFidxBinDeg   = 0.025
	neFidxHaloDeg  = 0.0005 // > one res-12 cell (≈ 16 m ≈ 0.0002°)
	neFidxDiskCap  = 2 << 30
	neFidxDiskDir  = "data/ne_fidx"
	neFidxMaxCells = 1 << 17
	neFidxGridM    = 25.0 // N2K / distance raster
	neFidxSrtmFlds = "bld_h_max,bld_h_p50,stories_max,roof_dom,canopy_h_p90,aspect_deg,slope_mean,slope_p90,elev_mean"
)

var (
	neFidxLRU      = newNELRU(48 << 20)
	neFidxMu       sync.Mutex
	neFidxFlight   = map[string]*neFidxCall{}
	neFidxBuilt    atomic.Int64
	neFidxBuildMs  atomic.Int64
	neFidxDiskHits atomic.Int64
	neFidxSrtmEp   atomic.Value // string: last X-Cells-Epoch seen (state hash input)
	neFidxSrtmDown atomicTime
	neFidxClient   = &http.Client{Timeout: 20 * time.Second}
	neFidxZEnc, _  = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	neFidxZDec, _  = zstd.NewReader(nil)
	neFidxDiskN    atomic.Int64 // writes since the last sweep
)

type neFidxCall struct {
	wg  sync.WaitGroup
	b   []byte
	err error
}

// NEFidxStats — size line for /api/v1/debug/inflight.
func NEFidxStats() map[string]interface{} {
	m := neFidxLRU.stats()
	m["built"] = neFidxBuilt.Load()
	m["build_ms_sum"] = neFidxBuildMs.Load()
	m["disk_hits"] = neFidxDiskHits.Load()
	m["disk_bytes"] = neFidxDiskSize()
	m["srtm_epoch"] = neFidxSrtmEpoch()
	m["warm"] = neFidxWarmStats()
	return m
}

// neFidxSrtmEpoch — the last srtm cells epoch seen. Persisted in
// data/ne_fidx/srtm_epoch so a restart does not move every bin's state (and
// rebuild every bin on first touch); a new epoch from the peer overwrites it.
func neFidxSrtmEpoch() string {
	if v := neFidxSrtmEp.Load(); v != nil {
		return v.(string)
	}
	ep := ""
	if b, err := os.ReadFile(filepath.Join(neFidxDiskDir, "srtm_epoch")); err == nil {
		ep = strings.TrimSpace(string(b))
	}
	if ep == "None" || ep == "null" {
		ep = ""
	}
	neFidxSrtmEp.Store(ep)
	return ep
}

func neFidxSrtmEpochStore(ep string) {
	// the peer answers 404 (no cells in that box) with `X-Cells-Epoch: None`;
	// that is not an epoch and must not flip every bin's state back and forth
	if ep == "" || ep == "None" || ep == "null" || ep == neFidxSrtmEpoch() {
		return
	}
	neFidxSrtmEp.Store(ep)
	os.MkdirAll(neFidxDiskDir, 0o755)
	os.WriteFile(filepath.Join(neFidxDiskDir, "srtm_epoch"), []byte(ep+"\n"), 0o644)
}

// ── the bin ──────────────────────────────────────────────────────────────────

type neFidxHdr struct {
	State   string       `json:"state"`
	BBox    [4]float64   `json:"bbox"`
	Origin  string       `json:"origin"`
	A       []float64    `json:"a"`
	Hex     [][2]float64 `json:"hex"`
	Cols    []neFidxCol  `json:"cols"`
	N2K     []string     `json:"n2k"`
	Sub     interface{}  `json:"sub"`
	Srtm    interface{}  `json:"srtm"`
	BuiltMs int64        `json:"built_ms"`
	Unk     int          `json:"unk"`
	ResPx   float64      `json:"res_px"`
	I, J    int          `json:"-"`
}

type neFidxCol struct {
	ID string `json:"id"`
	DT string `json:"dt"`
}

// neFidxBin — the parsed index of one bin.
type neFidxBin struct {
	Hdr  neFidxHdr
	N    int
	IJ   []int32 // 2n: i, j of cell k
	Cols map[string][]byte
	idx  map[uint32]int32 // (i<<16|j) → k, built lazily
}

func (b *neFidxBin) col(id string) []byte { return b.Cols[id] }
func (b *neFidxBin) n() int               { return b.N }
func (b *neFidxBin) sites() []string      { return b.Hdr.N2K }

func ijKey(i, j int32) uint32 { return uint32(uint16(i))<<16 | uint32(uint16(j)) }

func (b *neFidxBin) lattice() map[uint32]int32 {
	if b.idx == nil {
		b.idx = make(map[uint32]int32, b.N)
		for k := 0; k < b.N; k++ {
			b.idx[ijKey(b.IJ[2*k], b.IJ[2*k+1])] = int32(k)
		}
	}
	return b.idx
}

// neFidxNbr — the six IJ neighbours (H3 local IJ axes are 120° apart).
var neFidxNbr = [6][2]int32{{1, 0}, {-1, 0}, {0, 1}, {0, -1}, {1, 1}, {-1, -1}}

// eval — the match mask incl. the `k` cluster post-filter.
func (b *neFidxBin) eval(e *neFiltExpr) []uint8 {
	m := neFiltEval(b, e)
	if e.K > 1 {
		neFidxClusterFilter(b, m, e.K)
	}
	return m
}

// neFidxClusterFilter — drop matching cells whose connected match cluster (6-neighbourhood) has fewer than kmin cells.
func neFidxClusterFilter(b *neFidxBin, m []uint8, kmin int) {
	lat := b.lattice()
	seen := make([]uint8, b.N)
	var stack []int32
	for k := 0; k < b.N; k++ {
		if m[k] == 0 || seen[k] != 0 {
			continue
		}
		stack = append(stack[:0], int32(k))
		seen[k] = 1
		var comp []int32
		for len(stack) > 0 {
			c := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			comp = append(comp, c)
			ci, cj := b.IJ[2*c], b.IJ[2*c+1]
			for _, d := range neFidxNbr {
				if q, ok := lat[ijKey(ci+d[0], cj+d[1])]; ok && m[q] != 0 && seen[q] == 0 {
					seen[q] = 1
					stack = append(stack, q)
				}
			}
		}
		if len(comp) < kmin {
			for _, c := range comp {
				m[c] = 0
			}
		}
	}
}

// cell — the H3 id of ordinal k (cgo; used by the agent endpoint only).
func (b *neFidxBin) cell(origin h3.Cell, k int) (uint64, bool) {
	c, err := h3.LocalIJToCell(origin, h3.CoordIJ{I: int(b.IJ[2*k]), J: int(b.IJ[2*k+1])})
	if err != nil {
		return 0, false
	}
	return uint64(c), true
}

// pos — frame coordinates of ordinal k from the header polynomial.
func (b *neFidxBin) pos(k int) (x, y float64) {
	i, j := float64(b.IJ[2*k]), float64(b.IJ[2*k+1])
	a := b.Hdr.A
	if len(a) >= 12 {
		return a[0] + a[1]*i + a[2]*j + a[3]*i*i + a[4]*i*j + a[5]*j*j, a[6] + a[7]*i + a[8]*j + a[9]*i*i + a[10]*i*j + a[11]*j*j
	}
	return a[0] + a[1]*i + a[2]*j, a[3] + a[4]*i + a[5]*j
}

// ── encode / decode ──────────────────────────────────────────────────────────

func neFidxColSize(dt string) int {
	if dt == "u16" {
		return 2
	}
	return 1
}

// encode — the blob for one plane ("core" | "ext" | "all"); cells must be row-major (j, then i).
func (b *neFidxBin) encode(plane string) []byte {
	hdr := b.Hdr
	hdr.Cols = nil
	for _, f := range neFiltFields {
		if plane != "all" && f.Plane != plane {
			continue
		}
		if b.Cols[f.ID] == nil {
			continue
		}
		hdr.Cols = append(hdr.Cols, neFidxCol{f.ID, f.Dtype})
	}
	hj, _ := json.Marshal(&hdr)
	var buf bytes.Buffer
	buf.Grow(64 + len(hj) + 4*b.N + len(hdr.Cols)*b.N*2)
	buf.WriteString("NFX1")
	buf.WriteByte(neFidxVer)
	var tmp [4]byte
	binary.LittleEndian.PutUint16(tmp[:], uint16(len(hdr.Cols)))
	buf.Write(tmp[:2])
	binary.LittleEndian.PutUint32(tmp[:], uint32(b.N))
	buf.Write(tmp[:4])
	binary.LittleEndian.PutUint32(tmp[:], uint32(len(hj)))
	buf.Write(tmp[:4])
	buf.Write(hj)
	// rows
	type run struct{ i0, n int32 }
	type row struct {
		j    int32
		runs []run
	}
	var rows []row
	for k := 0; k < b.N; k++ {
		i, j := b.IJ[2*k], b.IJ[2*k+1]
		if len(rows) == 0 || rows[len(rows)-1].j != j {
			rows = append(rows, row{j: j})
		}
		r := &rows[len(rows)-1]
		if len(r.runs) > 0 && r.runs[len(r.runs)-1].i0+r.runs[len(r.runs)-1].n == i {
			r.runs[len(r.runs)-1].n++
		} else {
			r.runs = append(r.runs, run{i, 1})
		}
	}
	binary.LittleEndian.PutUint16(tmp[:], uint16(len(rows)))
	buf.Write(tmp[:2])
	for _, r := range rows {
		binary.LittleEndian.PutUint16(tmp[:], uint16(int16(r.j)))
		buf.Write(tmp[:2])
		binary.LittleEndian.PutUint16(tmp[:], uint16(len(r.runs)))
		buf.Write(tmp[:2])
		for _, u := range r.runs {
			binary.LittleEndian.PutUint16(tmp[:], uint16(int16(u.i0)))
			buf.Write(tmp[:2])
			binary.LittleEndian.PutUint16(tmp[:], uint16(u.n))
			buf.Write(tmp[:2])
		}
	}
	for _, c := range hdr.Cols {
		buf.Write(b.Cols[c.ID])
	}
	return buf.Bytes()
}

// neFidxDecode — parse a blob (any plane).
func neFidxDecode(d []byte) (*neFidxBin, error) {
	if len(d) < 15 || string(d[:4]) != "NFX1" || d[4] != neFidxVer {
		return nil, errors.New("not NFX1")
	}
	ncol := int(binary.LittleEndian.Uint16(d[5:]))
	n := int(binary.LittleEndian.Uint32(d[7:]))
	hl := int(binary.LittleEndian.Uint32(d[11:]))
	if n > neFidxMaxCells || 15+hl > len(d) {
		return nil, errors.New("bad sizes")
	}
	b := &neFidxBin{N: n, Cols: map[string][]byte{}}
	if err := json.Unmarshal(d[15:15+hl], &b.Hdr); err != nil {
		return nil, err
	}
	if len(b.Hdr.Cols) != ncol {
		return nil, errors.New("ncol mismatch")
	}
	o := 15 + hl
	if o+2 > len(d) {
		return nil, errors.New("short rows")
	}
	nrow := int(binary.LittleEndian.Uint16(d[o:]))
	o += 2
	b.IJ = make([]int32, 0, 2*n)
	for r := 0; r < nrow; r++ {
		if o+4 > len(d) {
			return nil, errors.New("short row")
		}
		j := int32(int16(binary.LittleEndian.Uint16(d[o:])))
		nr := int(binary.LittleEndian.Uint16(d[o+2:]))
		o += 4
		for u := 0; u < nr; u++ {
			if o+4 > len(d) {
				return nil, errors.New("short run")
			}
			i0 := int32(int16(binary.LittleEndian.Uint16(d[o:])))
			ln := int(binary.LittleEndian.Uint16(d[o+2:]))
			o += 4
			for q := 0; q < ln; q++ {
				b.IJ = append(b.IJ, i0+int32(q), j)
			}
		}
	}
	if len(b.IJ) != 2*n {
		return nil, fmt.Errorf("runs cover %d cells, n=%d", len(b.IJ)/2, n)
	}
	for _, c := range b.Hdr.Cols {
		sz := neFidxColSize(c.DT) * n
		if o+sz > len(d) {
			return nil, errors.New("short col " + c.ID)
		}
		b.Cols[c.ID] = d[o : o+sz : o+sz]
		o += sz
	}
	return b, nil
}

// ── state ────────────────────────────────────────────────────────────────────

func neFidxBox(i, j int) (w, s, e, n float64) {
	return float64(i) * neFidxBinDeg, float64(j) * neFidxBinDeg, float64(i+1) * neFidxBinDeg, float64(j+1) * neFidxBinDeg
}

// neFidxState — sha256[:12] over everything the bin reads: the declared/obs
// state of every KG it touches, the subsidy release, the srtm cells epoch,
// the layout version.
func neFidxState(i, j int) string {
	w, s, e, n := neFidxBox(i, j)
	h := sha256.New()
	h.Write([]byte("nfx" + strconv.Itoa(neFidxVer) + ";" + neGlyphTileState(KGCodesInBBoxMem(w-neFidxHaloDeg, s-neFidxHaloDeg, e+neFidxHaloDeg, n+neFidxHaloDeg))))
	h.Write([]byte(";sub=" + neSubReleaseSeen() + ";srtm=" + neFidxSrtmEpoch()))
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// ── get: LRU → disk → build ──────────────────────────────────────────────────

// neFidxGet — the full blob (all columns) of a bin at the current state.
func neFidxGet(i, j int) ([]byte, string, error) {
	st := neFidxState(i, j)
	key := fmt.Sprintf("%d/%d/%s", i, j, st)
	if b, ok := neFidxLRU.get(key); ok {
		return b, st, nil
	}
	neFidxMu.Lock()
	if c := neFidxFlight[key]; c != nil {
		neFidxMu.Unlock()
		c.wg.Wait()
		return c.b, st, c.err
	}
	c := &neFidxCall{}
	c.wg.Add(1)
	neFidxFlight[key] = c
	neFidxMu.Unlock()
	defer func() {
		neFidxMu.Lock()
		delete(neFidxFlight, key)
		neFidxMu.Unlock()
		c.wg.Done()
	}()
	if b := neFidxDiskRead(st, i, j); b != nil {
		neFidxDiskHits.Add(1)
		neFidxLRU.put(key, b)
		c.b = b
		return b, st, nil
	}
	t0 := time.Now()
	bin, err := neFidxBuild(i, j, st)
	if err != nil {
		c.err = err
		return nil, st, err
	}
	bin.Hdr.BuiltMs = time.Since(t0).Milliseconds()
	neFidxBuilt.Add(1)
	neFidxBuildMs.Add(bin.Hdr.BuiltMs)
	b := bin.encode("all")
	// the state may have moved during the build (a srtm epoch learnt on the first fetch): key on what was built
	if st2 := neFidxState(i, j); st2 != st {
		bin.Hdr.State = st2
		b = bin.encode("all")
		st = st2
		key = fmt.Sprintf("%d/%d/%s", i, j, st)
	}
	neFidxLRU.put(key, b)
	neFidxDiskWrite(st, i, j, b)
	c.b = b
	return b, st, nil
}

// neFidxPlane — slice the stored blob down to one plane (re-encode: cheap, planar copy).
func neFidxPlane(full []byte, plane string) ([]byte, error) {
	if plane == "all" {
		return full, nil
	}
	b, err := neFidxDecode(full)
	if err != nil {
		return nil, err
	}
	return b.encode(plane), nil
}

// ── disk ─────────────────────────────────────────────────────────────────────

func neFidxDiskPath(st string, i, j int) string {
	return filepath.Join(neFidxDiskDir, st, strconv.Itoa(i)+"_"+strconv.Itoa(j)+".zst")
}

func neFidxDiskRead(st string, i, j int) []byte {
	z, err := os.ReadFile(neFidxDiskPath(st, i, j))
	if err != nil {
		return nil
	}
	b, err := neFidxZDec.DecodeAll(z, nil)
	if err != nil || len(b) < 15 {
		return nil
	}
	now := time.Now()
	_ = os.Chtimes(neFidxDiskPath(st, i, j), now, now) // LRU by mtime
	return b
}

func neFidxDiskWrite(st string, i, j int, b []byte) {
	p := neFidxDiskPath(st, i, j)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	z := neFidxZEnc.EncodeAll(b, nil)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, z, 0o644); err == nil {
		_ = os.Rename(tmp, p)
	}
	if neFidxDiskN.Add(1)%64 == 0 {
		go neFidxDiskSweep()
	}
}

func neFidxDiskSize() int64 {
	var sz int64
	_ = filepath.Walk(neFidxDiskDir, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			sz += fi.Size()
		}
		return nil
	})
	return sz
}

// neFidxDiskSweep — hold the cap: state dirs other than the ones touched in
// the last day go first (oldest mtime), then files by mtime.
func neFidxDiskSweep() {
	if neFidxDiskSize() <= neFidxDiskCap {
		return
	}
	type f struct {
		p  string
		t  time.Time
		sz int64
	}
	var files []f
	_ = filepath.Walk(neFidxDiskDir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && strings.HasSuffix(p, ".zst") {
			files = append(files, f{p, fi.ModTime(), fi.Size()})
		}
		return nil
	})
	sort.Slice(files, func(a, b int) bool { return files[a].t.Before(files[b].t) })
	sz := int64(0)
	for _, x := range files {
		sz += x.sz
	}
	for _, x := range files {
		if sz <= neFidxDiskCap*9/10 {
			break
		}
		if os.Remove(x.p) == nil {
			sz -= x.sz
		}
	}
	ents, _ := os.ReadDir(neFidxDiskDir)
	for _, e := range ents {
		if e.IsDir() {
			_ = os.Remove(filepath.Join(neFidxDiskDir, e.Name())) // only succeeds when empty
		}
	}
}

// ── build ────────────────────────────────────────────────────────────────────

type neFidxCell struct {
	id   uint64
	i, j int32
	lon  float64
	lat  float64
	x, y float64 // frame units (neGlyphProj), set by neFidxFit
	halo bool
	lu   []byte
	obs  []byte
}

func neFidxBuild(i, j int, st string) (*neFidxBin, error) {
	w, s, e, n := neFidxBox(i, j)
	if e < 9.4 || w > 17.3 || n < 46.3 || s > 49.1 {
		return nil, errors.New("bin outside Austria")
	}
	if err := neGlyphLoadFrame(); err != nil {
		return nil, err
	}
	ids := neBBoxCells(w-neFidxHaloDeg, s-neFidxHaloDeg, e+neFidxHaloDeg, n+neFidxHaloDeg)
	res := neCollect(nil, ids, []string{"lu", "obs"}, &neReq{public: false, format: "bin", bevFirst: true})
	lay := neObsLayout(res.obsFlds, res.obsRec)
	v41 := len(res.obsFlds) == len(neObsFieldsV41) && neObsLayoutV41(res.obsFlds)
	origin, err := h3.LatLngToCell(h3.NewLatLng((s+n)/2, (w+e)/2), neCellRes)
	if err != nil {
		return nil, err
	}
	cells := make([]neFidxCell, 0, len(res.ids))
	for k, id := range res.ids {
		lon, lat := neCellCentre(id)
		ij, err := h3.CellToLocalIJ(origin, h3.Cell(id))
		if err != nil || ij.I < -30000 || ij.I > 30000 || ij.J < -30000 || ij.J > 30000 {
			continue
		}
		c := neFidxCell{id: id, i: int32(ij.I), j: int32(ij.J), lon: lon, lat: lat, lu: res.lu[k]}
		c.halo = lon < w || lon >= e || lat < s || lat >= n
		if k < len(res.obs) && lay != nil && len(res.obs[k]) == lay.rowBytes {
			c.obs = res.obs[k]
		}
		cells = append(cells, c)
	}
	// row-major in IJ: j, then i
	sort.Slice(cells, func(a, b int) bool {
		if cells[a].j != cells[b].j {
			return cells[a].j < cells[b].j
		}
		return cells[a].i < cells[b].i
	})
	// declared group of every cell (bin + halo) for the derived terms
	dAll := make([]uint8, len(cells))
	for k := range cells {
		dAll[k] = 255
		if lu := cells[k].lu; len(lu) >= 21 {
			best, _, sum := neFidxLUBest(lu[12:21])
			if sum > 0 {
				dAll[k] = uint8(best)
			}
		}
	}
	lat := make(map[uint32]int32, len(cells))
	for k := range cells {
		lat[ijKey(cells[k].i, cells[k].j)] = int32(k)
	}
	// the bin's own cells, in order
	bin := &neFidxBin{Cols: map[string][]byte{}}
	bin.Hdr = neFidxHdr{State: st, BBox: [4]float64{w, s, e, n}, Origin: neCellHex(uint64(origin)), I: i, J: j, N2K: []string{}}
	ord := make([]int, 0, len(cells))
	for k := range cells {
		if !cells[k].halo {
			ord = append(ord, k)
		}
	}
	N := len(ord)
	if N > neFidxMaxCells {
		return nil, errors.New("too many cells")
	}
	bin.N = N
	bin.IJ = make([]int32, 2*N)
	for q, k := range ord {
		bin.IJ[2*q], bin.IJ[2*q+1] = cells[k].i, cells[k].j
	}
	col := func(id string) []byte {
		fd := neFiltByID[id]
		sz := neFidxColSize(fd.Dtype)
		c := make([]byte, N*sz)
		fill := byte(255)
		if fd.Dtype == "i8" {
			fill = 0x80
		}
		if fd.Nodata == 0 {
			fill = 0
		}
		for q := range c {
			c[q] = fill
		}
		bin.Cols[id] = c
		return c
	}
	for _, f := range neFiltFields {
		col(f.ID)
	}
	put16 := func(id string, q int, v uint16) { binary.LittleEndian.PutUint16(bin.Cols[id][2*q:], v) }
	// declared · obs
	unk := 0
	for q, k := range ord {
		c := &cells[k]
		if len(c.lu) >= 21 {
			best, _, sum := neFidxLUBest(c.lu[12:21])
			if sum > 0 {
				bin.Cols["d"][q] = uint8(best)
				bin.Cols["dsh"][q] = uint8(min(int(c.lu[12+best])*255/sum, 254))
			} else {
				unk++
			}
		}
		if c.obs == nil {
			continue
		}
		ob := c.obs
		best, _, sum := neFidxLUBest(ob[lay.luOff : lay.luOff+9])
		if sum > 0 {
			bin.Cols["o"][q] = uint8(best)
			bin.Cols["osh"][q] = uint8(min(int(ob[lay.luOff+best])*255/sum, 254))
		}
		if !v41 || len(ob) != neObsRecordV41 {
			continue
		}
		bin.Cols["bcov"][q] = ob[17]
		bin.Cols["bn"][q] = ob[18]
		bin.Cols["hmean"][q] = ob[19]
		bin.Cols["h"][q] = ob[20]
		bin.Cols["hmax"][q] = ob[21]
		bin.Cols["tn"][q] = ob[22]
		bin.Cols["ht"][q] = ob[23]
		bin.Cols["canopy"][q] = ob[24]
		bin.Cols["ndvi"][q] = ob[25]
		bin.Cols["ndva"][q] = ob[26]
		bin.Cols["dh"][q] = ob[27]
		if ob[28] != 0 {
			bin.Cols["als"][q] = ob[28]
		}
		bin.Cols["ver"][q] = ob[29]
		put16("el", q, binary.LittleEndian.Uint16(ob[30:]))
		bin.Cols["sl"][q] = ob[32]
		bin.Cols["sl90"][q] = ob[33]
		bin.Cols["as"][q] = ob[34]
		bin.Cols["rough"][q] = ob[35]
		bin.Cols["tri"][q] = ob[36]
		bin.Cols["tpi"][q] = ob[37]
		bin.Cols["dtm"][q] = ob[38]
		if ob[39] != 0 {
			bin.Cols["loss"][q] = ob[39]
		}
		bin.Cols["phen"][q] = ob[40]
	}
	bin.Hdr.Unk = unk
	// subsidy (peer bin, from the LRU or one fetch)
	if sb, err := neSubBinAt(i, j, false); err == nil && sb != nil {
		bin.Hdr.Sub = map[string]interface{}{"year": sb.Year, "release": sb.Release}
		for q, k := range ord {
			if x := sb.find(cells[k].id); x >= 0 {
				bin.Cols["sub"][q] = sb.Cls[x] & 7
				bin.Cols["scov"][q] = sb.Cov[x]
				if sb.Bio[x] != 255 {
					bin.Cols["bio"][q] = b2u(sb.Bio[x] >= 128)
				}
				bin.Cols["srel"][q] = b2u(sb.Rel[x] < neSubRelDubious)
				bin.Cols["schg"][q] = uint8(min(int(sb.Chg[x]), 5))
				if sb.Snar != nil {
					put16("snar", q, sb.Snar[x])
					put16("snar2", q, sb.Snar2[x])
					bin.Cols["oepul"][q] = sb.Oepul[x]
					bin.Cols["nutz"][q] = sb.Nutz[x]
				}
			} else {
				// the peer holds the bin and has no row for the cell: not claimed
				bin.Cols["sub"][q], bin.Cols["scov"][q] = 0, 0
				if sb.Snar != nil {
					put16("snar", q, 0)
					put16("snar2", q, 0)
					bin.Cols["oepul"][q], bin.Cols["nutz"][q] = 0, 0
				}
			}
		}
	}
	// srtm cells bin: structure heights, roof, canopy, aspect_deg; terrain for cells without an obs row
	if sr := neFidxSrtmFetch(w, s, e, n); sr != nil {
		bin.Hdr.Srtm = map[string]interface{}{"epoch": sr.epoch}
		for q, k := range ord {
			x := sr.find(cells[k].id)
			if x < 0 {
				continue
			}
			if c := sr.cols["bld_h_max"]; c != nil {
				bin.Cols["hb"][q] = c[x]
			}
			if c := sr.cols["bld_h_p50"]; c != nil {
				bin.Cols["hbm"][q] = c[x]
			}
			if c := sr.cols["stories_max"]; c != nil {
				bin.Cols["stor"][q] = c[x]
			}
			if c := sr.cols["roof_dom"]; c != nil {
				bin.Cols["roof"][q] = c[x]
			}
			if c := sr.cols["canopy_h_p90"]; c != nil {
				bin.Cols["hc"][q] = c[x]
			}
			if c := sr.cols["aspect_deg"]; c != nil {
				if c[x] == 254 {
					bin.Cols["asd"][q] = 255
				} else {
					bin.Cols["asd"][q] = c[x]
				}
			}
			if cells[k].obs == nil {
				if c := sr.cols["slope_mean"]; c != nil {
					bin.Cols["sl"][q] = c[x]
				}
				if c := sr.cols["slope_p90"]; c != nil {
					bin.Cols["sl90"][q] = c[x]
				}
				if c := sr.cols["elev_mean"]; c != nil {
					put16("el", q, binary.LittleEndian.Uint16(c[2*x:]))
				}
				if c := sr.cols["aspect_deg"]; c != nil && c[x] != 255 {
					if c[x] == 254 {
						bin.Cols["as"][q] = 8
					} else {
						bin.Cols["as"][q] = uint8(((int(c[x])*2 + 22) / 45) % 8)
					}
				}
			}
		}
	}
	// Natura 2000: rasterised rings on a 25 m grid, one lookup per cell centre
	neFidxN2K(bin, cells, ord, w, s, e, n)
	// distance to roads / water (OSM): chamfer transform on the same grid, 1 km margin
	neFidxDist(bin, cells, ord, w, s, e, n)
	// change state from the chunk log
	for _, kg := range KGCodesInBBoxMem(w, s, e, n) {
		c := neChangeGet(kg)
		if c == nil {
			continue
		}
		d, err := c.deltasOf()
		if err != nil {
			continue
		}
		cum := c.cumWin(d, neChangeWin{})
		if len(cum) == 0 {
			continue
		}
		for q, k := range ord {
			if cu := cum[cells[k].id]; cu != nil && cu.Share >= 128 {
				if cu.Confirmed {
					bin.Cols["chg"][q] = 2
				} else if cu.Candidate && bin.Cols["chg"][q] != 2 {
					bin.Cols["chg"][q] = 1
				}
			}
		}
	}
	for q := 0; q < N; q++ {
		if bin.Cols["chg"][q] == 255 {
			bin.Cols["chg"][q] = 0
		}
	}
	// derived: het / edge (exact, the halo supplies the neighbours), cont (flood fill, bit 7 = touches the halo)
	neFidxDerived(bin, cells, ord, dAll, lat)
	// geometry: polynomial (i, j) → frame, hexagon offsets
	if err := neFidxFit(bin, cells, ord, origin); err != nil {
		return nil, err
	}
	return bin, nil
}

func b2u(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

func neFidxLUBest(lu []byte) (best, second, sum int) {
	best, second = -1, -1
	for q := 0; q < 9; q++ {
		v := int(lu[q])
		sum += v
		if best < 0 || v > int(lu[best]) {
			second, best = best, q
		} else if second < 0 || v > int(lu[second]) {
			second = q
		}
	}
	return
}

// neFidxDerived — het, edge, cont over the bin + halo lattice.
func neFidxDerived(bin *neFidxBin, cells []neFidxCell, ord []int, d []uint8, lat map[uint32]int32) {
	comp := make([]int32, len(cells)) // cell → component id+1
	var size []int
	var halo []bool
	var stack []int32
	for q, k := range ord {
		c := &cells[k]
		dk := d[k]
		if dk == 255 {
			continue
		}
		var seen [9]bool
		het, edge := 0, 0
		for _, nb := range neFidxNbr {
			x, ok := lat[ijKey(c.i+nb[0], c.j+nb[1])]
			if !ok || d[x] == 255 {
				continue
			}
			if d[x] != dk {
				edge++
				if !seen[d[x]] {
					seen[d[x]] = true
					het++
				}
			}
		}
		bin.Cols["het"][q] = uint8(het)
		bin.Cols["edge"][q] = uint8(edge)
		if comp[k] == 0 {
			id := int32(len(size) + 1)
			n, touch := 0, false
			stack = append(stack[:0], int32(k))
			comp[k] = id
			for len(stack) > 0 {
				x := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				n++
				if cells[x].halo {
					touch = true
				}
				if n >= 64 && touch {
					continue // enough known
				}
				for _, nb := range neFidxNbr {
					y, ok := lat[ijKey(cells[x].i+nb[0], cells[x].j+nb[1])]
					if ok && comp[y] == 0 && d[y] == dk {
						comp[y] = id
						stack = append(stack, y)
					}
				}
			}
			size = append(size, n)
			halo = append(halo, touch)
		}
		cid := comp[k] - 1
		v := size[cid]
		if v > 64 {
			v = 64
		}
		if halo[cid] {
			v |= 128
		}
		bin.Cols["cont"][q] = uint8(v)
	}
}

// neFidxFit — least squares (i, j) → (x, y) in frame units; quadratic when the
// affine residual exceeds 0.3 px at LOD 1 (100 px per unit).
func neFidxFit(bin *neFidxBin, cells []neFidxCell, ord []int, origin h3.Cell) error {
	if len(ord) == 0 {
		bin.Hdr.A = []float64{0, 0, 0, 0, 0, 0}
		bin.Hdr.Hex = [][2]float64{}
		return nil
	}
	for _, q := range ord {
		c := &cells[q]
		c.x, c.y = neGlyphProj(c.lon, c.lat)
	}
	if err := neFidxFitXY(bin, cells, ord); err != nil {
		return err
	}
	// hexagon offsets of the origin cell, frame units
	bd, err := origin.Boundary()
	if err != nil || len(bd) < 6 {
		return errors.New("origin boundary")
	}
	ol, oa := neCellCentre(uint64(origin))
	cx, cy := neGlyphProj(ol, oa)
	bin.Hdr.Hex = make([][2]float64, 0, 6)
	for v := 0; v < len(bd) && v < 6; v++ {
		x, y := neGlyphProj(bd[v].Lng, bd[v].Lat)
		bin.Hdr.Hex = append(bin.Hdr.Hex, [2]float64{math.Round((x-cx)*1e5) / 1e5, math.Round((y-cy)*1e5) / 1e5})
	}
	for k := range bin.Hdr.A {
		bin.Hdr.A[k] = math.Round(bin.Hdr.A[k]*1e9) / 1e9
	}
	return nil
}

// neFidxFitXY — the least-squares part of neFidxFit over cells' (x, y): affine, or quadratic when
// the affine residual exceeds 0.3 px at LOD 1 (100 px per unit). Pure; testable without a frame.
func neFidxFitXY(bin *neFidxBin, cells []neFidxCell, ord []int) error {
	n := len(ord)
	if n == 0 {
		bin.Hdr.A = []float64{0, 0, 0, 0, 0, 0}
		return nil
	}
	step := 1
	if n > 2000 {
		step = n / 2000
	}
	fit := func(quad bool) ([]float64, float64) {
		m := 3
		if quad {
			m = 6
		}
		basis := func(i, j float64) []float64 {
			if quad {
				return []float64{1, i, j, i * i, i * j, j * j}
			}
			return []float64{1, i, j}
		}
		var ata [6][6]float64
		var atx, aty [6]float64
		for q := 0; q < n; q += step {
			c := &cells[ord[q]]
			b := basis(float64(c.i), float64(c.j))
			for r := 0; r < m; r++ {
				for s := 0; s < m; s++ {
					ata[r][s] += b[r] * b[s]
				}
				atx[r] += b[r] * c.x
				aty[r] += b[r] * c.y
			}
		}
		ax := neSolve(ata, atx, m)
		ay := neSolve(ata, aty, m)
		if ax == nil || ay == nil {
			return nil, math.Inf(1)
		}
		res := 0.0
		for q := 0; q < n; q += step {
			c := &cells[ord[q]]
			b := basis(float64(c.i), float64(c.j))
			px, py := 0.0, 0.0
			for r := 0; r < m; r++ {
				px += ax[r] * b[r]
				py += ay[r] * b[r]
			}
			if d := math.Hypot(px-c.x, py-c.y); d > res {
				res = d
			}
		}
		return append(ax, ay...), res
	}
	a, res := fit(false)
	if a == nil || res*100 > 0.3 {
		if a2, res2 := fit(true); a2 != nil && res2 < res {
			a, res = a2, res2
		}
	}
	if a == nil {
		return errors.New("fit failed")
	}
	bin.Hdr.A = a
	bin.Hdr.ResPx = math.Round(res*100*1000) / 1000
	return nil
}

// neSolve — Gaussian elimination with partial pivoting, m ≤ 6.
func neSolve(a [6][6]float64, b [6]float64, m int) []float64 {
	for c := 0; c < m; c++ {
		p := c
		for r := c + 1; r < m; r++ {
			if math.Abs(a[r][c]) > math.Abs(a[p][c]) {
				p = r
			}
		}
		if math.Abs(a[p][c]) < 1e-12 {
			return nil
		}
		a[c], a[p] = a[p], a[c]
		b[c], b[p] = b[p], b[c]
		for r := c + 1; r < m; r++ {
			f := a[r][c] / a[c][c]
			for s := c; s < m; s++ {
				a[r][s] -= f * a[c][s]
			}
			b[r] -= f * b[c]
		}
	}
	x := make([]float64, m)
	for r := m - 1; r >= 0; r-- {
		x[r] = b[r]
		for s := r + 1; s < m; s++ {
			x[r] -= a[r][s] * x[s]
		}
		x[r] /= a[r][r]
	}
	return x
}

// ── raster helpers (25 m grid over a lon/lat box) ────────────────────────────

type neGrid struct {
	w, s   float64
	dx, dy float64 // degrees per pixel
	gw, gh int
	v      []uint8
}

func newNEGrid(w, s, e, n float64) *neGrid {
	mLon, mLat := metresPerDeg((s + n) / 2)
	g := &neGrid{w: w, s: s, dx: neFidxGridM / mLon, dy: neFidxGridM / mLat}
	g.gw = int(math.Ceil((e-w)/g.dx)) + 1
	g.gh = int(math.Ceil((n-s)/g.dy)) + 1
	g.v = make([]uint8, g.gw*g.gh)
	return g
}

func (g *neGrid) at(lon, lat float64) (int, int, bool) {
	x, y := int(math.Floor((lon-g.w)/g.dx)), int(math.Floor((lat-g.s)/g.dy))
	return x, y, x >= 0 && y >= 0 && x < g.gw && y < g.gh
}

// fillRings — even-odd scanline fill of a set of rings (lon/lat) with value v.
func (g *neGrid) fillRings(rings [][][]float64, v uint8) {
	var xs []float64
	for y := 0; y < g.gh; y++ {
		cy := g.s + (float64(y)+0.5)*g.dy
		xs = xs[:0]
		for _, r := range rings {
			for k := 0; k < len(r); k++ {
				p, q := r[k], r[(k+1)%len(r)]
				if len(p) < 2 || len(q) < 2 || (p[1] > cy) == (q[1] > cy) {
					continue
				}
				xs = append(xs, p[0]+(cy-p[1])/(q[1]-p[1])*(q[0]-p[0]))
			}
		}
		if len(xs) < 2 {
			continue
		}
		sort.Float64s(xs)
		for k := 0; k+1 < len(xs); k += 2 {
			x0 := int(math.Ceil((xs[k]-g.w)/g.dx - 0.5))
			x1 := int(math.Floor((xs[k+1]-g.w)/g.dx - 0.5))
			if x0 < 0 {
				x0 = 0
			}
			if x1 >= g.gw {
				x1 = g.gw - 1
			}
			for x := x0; x <= x1; x++ {
				g.v[y*g.gw+x] = v
			}
		}
	}
}

// line — Bresenham over a lon/lat segment.
func (g *neGrid) line(x0, y0, x1, y1 float64, v uint8) {
	ax, ay := (x0-g.w)/g.dx, (y0-g.s)/g.dy
	bx, by := (x1-g.w)/g.dx, (y1-g.s)/g.dy
	steps := int(math.Max(math.Abs(bx-ax), math.Abs(by-ay))) + 1
	if steps > 100000 {
		return
	}
	for k := 0; k <= steps; k++ {
		t := float64(k) / float64(steps)
		x, y := int(math.Floor(ax+(bx-ax)*t)), int(math.Floor(ay+(by-ay)*t))
		if x >= 0 && y >= 0 && x < g.gw && y < g.gh {
			g.v[y*g.gw+x] = v
		}
	}
}

// chamfer — distance (in pixels, 3-4 chamfer / 3) to the nearest set pixel; inf = none.
func (g *neGrid) chamfer() []float32 {
	const inf = float32(1e9)
	d := make([]float32, len(g.v))
	for k, v := range g.v {
		if v != 0 {
			d[k] = 0
		} else {
			d[k] = inf
		}
	}
	W := g.gw
	mn := func(a, b float32) float32 {
		if a < b {
			return a
		}
		return b
	}
	for y := 0; y < g.gh; y++ {
		for x := 0; x < W; x++ {
			k := y*W + x
			if x > 0 {
				d[k] = mn(d[k], d[k-1]+1)
			}
			if y > 0 {
				d[k] = mn(d[k], d[k-W]+1)
				if x > 0 {
					d[k] = mn(d[k], d[k-W-1]+1.3333)
				}
				if x+1 < W {
					d[k] = mn(d[k], d[k-W+1]+1.3333)
				}
			}
		}
	}
	for y := g.gh - 1; y >= 0; y-- {
		for x := W - 1; x >= 0; x-- {
			k := y*W + x
			if x+1 < W {
				d[k] = mn(d[k], d[k+1]+1)
			}
			if y+1 < g.gh {
				d[k] = mn(d[k], d[k+W]+1)
				if x+1 < W {
					d[k] = mn(d[k], d[k+W+1]+1.3333)
				}
				if x > 0 {
					d[k] = mn(d[k], d[k+W-1]+1.3333)
				}
			}
		}
	}
	return d
}

// neFidxN2K — n2k (type 1..3 / 0), n2ks (site index into the header list).
func neFidxN2K(bin *neFidxBin, cells []neFidxCell, ord []int, w, s, e, n float64) {
	loadNatura2000()
	for q := range ord {
		bin.Cols["n2k"][q] = 0
		bin.Cols["n2kt"][q] = 0
	}
	if !natura2000Loaded {
		return
	}
	var g *neGrid
	for si := range natura2000Sites {
		st := &natura2000Sites[si]
		if st.MaxLon < w || st.MinLon > e || st.MaxLat < s || st.MinLat > n {
			continue
		}
		if g == nil {
			g = newNEGrid(w, s, e, n)
		} else {
			for k := range g.v {
				g.v[k] = 0
			}
		}
		for _, poly := range st.Polygons {
			g.fillRings(poly, 1)
		}
		idx := -1
		typ := uint8(0)
		switch st.SiteType {
		case "A":
			typ = 1
		case "B":
			typ = 2
		case "C":
			typ = 3
		}
		for q, k := range ord {
			x, y, ok := g.at(cells[k].lon, cells[k].lat)
			if !ok || g.v[y*g.gw+x] == 0 {
				continue
			}
			if idx < 0 {
				idx = len(bin.Hdr.N2K)
				bin.Hdr.N2K = append(bin.Hdr.N2K, st.SiteCode)
			}
			if bin.Cols["n2k"][q] == 0 || typ == 3 {
				bin.Cols["n2k"][q] = 1
				bin.Cols["n2kt"][q] = typ
			}
			if bin.Cols["n2ks"][q] == 255 && idx < 255 {
				bin.Cols["n2ks"][q] = uint8(idx)
			}
		}
	}
}

// neFidxDist — rd / wat distance classes from OSM lines + water areas, 1 km margin.
func neFidxDist(bin *neFidxBin, cells []neFidxCell, ord []int, w, s, e, n float64) {
	if osmDB == nil {
		return
	}
	mLon, mLat := metresPerDeg((s + n) / 2)
	px, py := 1050/mLon, 1050/mLat
	W, S, E, N := w-px, s-py, e+px, n+py
	rd, wt := newNEGrid(W, S, E, N), newNEGrid(W, S, E, N)
	rows, err := osmDB.Query(`
		SELECT l.cat, l.major, l.fclass, l.coords
		FROM osm_lines_rtree rt JOIN osm_lines l ON l.id = rt.id
		WHERE rt.max_lon >= ? AND rt.min_lon <= ? AND rt.max_lat >= ? AND rt.min_lat <= ?
		  AND l.cat IN ('road','water')`, W, E, S, N)
	if err != nil {
		return
	}
	any := false
	for rows.Next() {
		var cat, fclass string
		var major int
		var coords []byte
		if rows.Scan(&cat, &major, &fclass, &coords) != nil {
			continue
		}
		g := wt
		if cat == "road" {
			if cls, ok := neRoadClassOf(cat, major, fclass); !ok || cls == neRoadMinor {
				continue
			}
			g = rd
		}
		pts := unpackCoords(coords)
		for k := 1; k < len(pts); k++ {
			g.line(pts[k-1][0], pts[k-1][1], pts[k][0], pts[k][1], 1)
		}
		any = true
	}
	rows.Close()
	if OSMAreasReady() {
		if areas, _, err := OSMAreasInBBox(W, S, E, N, map[string]bool{"water": true}, false, 2000); err == nil {
			for _, a := range areas {
				for _, poly := range a.Geom {
					rings := make([][][]float64, 0, len(poly))
					for _, r := range poly {
						ring := make([][]float64, len(r))
						for k, p := range r {
							ring[k] = []float64{p[0], p[1]}
						}
						rings = append(rings, ring)
					}
					wt.fillRings(rings, 1)
				}
				any = true
			}
		}
	}
	_ = any
	dr, dw := rd.chamfer(), wt.chamfer()
	bucket := func(d float32) uint8 {
		m := float64(d) * neFidxGridM
		if d >= 1e8 {
			m = 1e9
		}
		return uint8(neFiltBucketOf(neFiltDistEdges, m))
	}
	for q, k := range ord {
		if x, y, ok := rd.at(cells[k].lon, cells[k].lat); ok {
			bin.Cols["rd"][q] = bucket(dr[y*rd.gw+x])
			bin.Cols["wat"][q] = bucket(dw[y*wt.gw+x])
		}
	}
}

// ── srtm cells bin ───────────────────────────────────────────────────────────

type neFidxSrtm struct {
	epoch string
	ids   []uint64
	cols  map[string][]byte
}

func (s *neFidxSrtm) find(id uint64) int {
	k := sort.Search(len(s.ids), func(i int) bool { return s.ids[i] >= id })
	if k < len(s.ids) && s.ids[k] == id {
		return k
	}
	return -1
}

// neFidxSrtmFetch — one aligned bin from {OBS_NAME} (NEX1: header JSON with
// columns[], ids as u64 deltas, then planar columns). nil when the peer is down
// (remembered for 2 min) or the body is not what we expect.
func neFidxSrtmFetch(w, s, e, n float64) *neFidxSrtm {
	if time.Since(neFidxSrtmDown.get()) < 2*time.Minute {
		return nil
	}
	url := fmt.Sprintf("%s?bbox=%.3f,%.3f,%.3f,%.3f&format=bin&fields=%s", peers().ObsAPI+"/api/v1/cells", w, s, e, n, neFidxSrtmFlds)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "umfeld-at/fidx (+"+publicURL()+")")
	resp, err := neFidxClient.Do(req)
	if err != nil {
		neFidxSrtmDown.set(time.Now())
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		neFidxSrtmEpochStore(resp.Header.Get("X-Cells-Epoch"))
		return &neFidxSrtm{epoch: resp.Header.Get("X-Cells-Epoch"), cols: map[string][]byte{}}
	}
	if resp.StatusCode != 200 {
		neFidxSrtmDown.set(time.Now())
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil || len(body) < 8 || string(body[:4]) != "NEX1" {
		return nil
	}
	hl := int(binary.LittleEndian.Uint32(body[4:]))
	if 8+hl > len(body) {
		return nil
	}
	var hdr struct {
		N       int    `json:"n"`
		Epoch   string `json:"epoch"`
		IDs     string `json:"ids"`
		Columns []struct {
			Name  string `json:"name"`
			Dtype string `json:"dtype"`
			Bytes int    `json:"bytes"`
		} `json:"columns"`
	}
	if json.Unmarshal(body[8:8+hl], &hdr) != nil {
		return nil
	}
	ep := resp.Header.Get("X-Cells-Epoch")
	if ep == "" {
		ep = hdr.Epoch
	}
	neFidxSrtmEpochStore(ep)
	out := &neFidxSrtm{epoch: ep, cols: map[string][]byte{}}
	o := 8 + hl
	for _, c := range hdr.Columns {
		if c.Bytes < 0 || o+c.Bytes > len(body) {
			return nil
		}
		seg := body[o : o+c.Bytes]
		o += c.Bytes
		if c.Name == "id" {
			if c.Bytes != 8*hdr.N {
				return nil
			}
			out.ids = make([]uint64, hdr.N)
			var acc uint64
			for k := 0; k < hdr.N; k++ {
				v := binary.LittleEndian.Uint64(seg[8*k:])
				if k == 0 || hdr.IDs != "delta_u64" {
					acc = v
				} else {
					acc += v
				}
				out.ids[k] = acc
			}
			continue
		}
		sz := 1
		if c.Dtype == "u16" || c.Dtype == "i16" {
			sz = 2
		}
		if c.Bytes != sz*hdr.N {
			continue
		}
		out.cols[c.Name] = seg
	}
	if out.ids == nil || !sort.SliceIsSorted(out.ids, func(a, b int) bool { return out.ids[a] < out.ids[b] }) {
		if out.ids == nil {
			return nil
		}
		// sort rows by id (keep the columns aligned)
		perm := make([]int, len(out.ids))
		for k := range perm {
			perm[k] = k
		}
		sort.Slice(perm, func(a, b int) bool { return out.ids[perm[a]] < out.ids[perm[b]] })
		ids := make([]uint64, len(perm))
		for k, p := range perm {
			ids[k] = out.ids[p]
		}
		out.ids = ids
		for name, c := range out.cols {
			sz := len(c) / len(perm)
			nc := make([]byte, len(c))
			for k, p := range perm {
				copy(nc[k*sz:(k+1)*sz], c[p*sz:(p+1)*sz])
			}
			out.cols[name] = nc
		}
	}
	return out
}

// ── handler ──────────────────────────────────────────────────────────────────

// handleNEGlyphFidx — GET /api/v1/ne/glyph/fidx?i=&j=[&p=core|ext|all][&e=]
func handleNEGlyphFidx(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	i, e1 := strconv.Atoi(q.Get("i"))
	j, e2 := strconv.Atoi(q.Get("j"))
	if e1 != nil || e2 != nil {
		apiError(w, "i=&j= required (0.025° bin: w = i·0.025, s = j·0.025)", 400)
		return
	}
	plane := q.Get("p")
	if plane == "" {
		plane = "core"
	}
	if plane != "core" && plane != "ext" && plane != "all" {
		apiError(w, "p=core|ext|all", 400)
		return
	}
	if bw, bs, be, bn := neFidxBox(i, j); be < 9.4 || bw > 17.3 || bn < 46.3 || bs > 49.1 {
		apiError(w, "bin outside Austria", 404)
		return
	}
	neFidxWarmNote(i, j)
	full, st, err := neFidxGet(i, j)
	if err != nil {
		apiError(w, "index unavailable: "+err.Error(), 503)
		return
	}
	etag := `"` + st + "-" + plane + `"`
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Expose-Headers", "ETag, X-NE-Fidx, X-NE-Fidx-State")
	w.Header().Set("ETag", etag)
	w.Header().Set("X-NE-Fidx-State", st)
	w.Header().Set("X-NE-Notice", NENotice())
	w.Header().Set("X-Data-Attribution", asciiFold(neFidxAttribution))
	if q.Get("e") == st {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=300")
	}
	if m := r.Header.Get("If-None-Match"); m != "" && strings.Contains(m, etag) {
		w.WriteHeader(304)
		return
	}
	b, err := neFidxPlane(full, plane)
	if err != nil {
		apiError(w, "index corrupt", 500)
		return
	}
	bin, _ := neFidxDecode(full)
	if bin != nil {
		w.Header().Set("X-NE-Fidx", fmt.Sprintf("n=%d;unk=%d;srtm=%d;sub=%d;n2k=%d;built_ms=%d;res_px=%g", bin.N, bin.Hdr.Unk, b2u(bin.Hdr.Srtm != nil), b2u(bin.Hdr.Sub != nil), len(bin.Hdr.N2K), bin.Hdr.BuiltMs, bin.Hdr.ResPx))
	}
	w.Header().Set("Content-Type", "application/vnd.ne-fidx")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	_, _ = w.Write(b)
}

var neFidxAttribution = peerText("declared: © BEV (CC BY 4.0) via peers' bevdirect builds · observed: {OBS_NAME} (CC BY 4.0) · subsidy: INVEKOS © AMA / BML (CC BY 4.0) via {SUB_NAME} · Natura 2000: EEA · roads/water: © OpenStreetMap contributors (ODbL)")

// ── warm-behind-tiles ────────────────────────────────────────────────────────
//
// Bins of the last 24 h of LOD 0/1 tile requests are prebuilt by one
// low-priority goroutine while no request is in flight (NE_FIDX_WARM=0 turns
// it off). Bounded by the caches; one bin at a time.

var (
	neFidxWarmMu   sync.Mutex
	neFidxWarmSeen = map[[2]int]time.Time{}
	neFidxWarmDone atomic.Int64
)

func neFidxWarmNote(i, j int) {
	neFidxWarmMu.Lock()
	if len(neFidxWarmSeen) < 4096 {
		neFidxWarmSeen[[2]int{i, j}] = time.Now()
	}
	neFidxWarmMu.Unlock()
}

// neFidxWarmNoteTile — called from the tile handler (LOD 0 or 1).
func neFidxWarmNoteTile(lod, i, j int) {
	if lod == 1 {
		i, j = int(math.Floor(float64(i)*0.01/neFidxBinDeg)), int(math.Floor(float64(j)*0.01/neFidxBinDeg))
	} else if lod != 0 {
		return
	}
	neFidxWarmNote(i, j)
}

func neFidxWarmStats() map[string]interface{} {
	neFidxWarmMu.Lock()
	n := len(neFidxWarmSeen)
	neFidxWarmMu.Unlock()
	return map[string]interface{}{"noted": n, "built": neFidxWarmDone.Load(), "on": os.Getenv("NE_FIDX_WARM") != "0"}
}

func neFidxWarmLoop() {
	if os.Getenv("NE_FIDX_WARM") == "0" {
		return
	}
	for {
		time.Sleep(20 * time.Second)
		inflightMu.Lock()
		busy := len(inflight) > 0
		inflightMu.Unlock()
		if busy {
			continue
		}
		var pick [2]int
		found := false
		neFidxWarmMu.Lock()
		for k, t := range neFidxWarmSeen {
			if time.Since(t) > 24*time.Hour {
				delete(neFidxWarmSeen, k)
				continue
			}
			st := neFidxState(k[0], k[1])
			if _, ok := neFidxLRU.get(fmt.Sprintf("%d/%d/%s", k[0], k[1], st)); ok {
				continue
			}
			if _, err := os.Stat(neFidxDiskPath(st, k[0], k[1])); err == nil {
				continue
			}
			pick, found = k, true
			break
		}
		neFidxWarmMu.Unlock()
		if !found {
			continue
		}
		if _, _, err := neFidxGet(pick[0], pick[1]); err == nil {
			neFidxWarmDone.Add(1)
		} else {
			log.Printf("fidx warm %d/%d: %v", pick[0], pick[1], err)
		}
	}
}

// neGlyphFilterJS — the filter Worker (ne_glyph_filter.js), served at
// /api/v1/ne/glyph/filter.js with a content ETag (cacheMiddleware), gzip.
//
//go:embed ne_glyph_filter.js
var neGlyphFilterJS []byte

func handleNEGlyphFilterJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_, _ = w.Write(neGlyphFilterJS)
}
