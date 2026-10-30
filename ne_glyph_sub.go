// ne_glyph_sub.go — the landing glyph's INVEKOS subsidy layer: the share of
// each cell claimed as an AMA Schlag, its class, bio flag, drawing
// reliability and the lineage of the claim against the previous year —
// aggregated per H3 cell by the peer {SUB_NAME}
// (docs/invekos-subsidy-layer.md) and painted here as one more overlay of
// the full-resolution picture (v=sub wash under declared/observed, v=subc
// lineage strokes under change), plus the res-7 overview for the dots.
//
//	GET /api/v1/ne/glyph/tile?lod=&i=&j=&v=sub|subc   paletted PNG like the other views (204 while the peer is down)
//	GET /api/v1/ne/glyph/sub                           res-7 overview columns in cells[] order of /ne/glyph
//	GET /api/v1/ne/glyph/cell?cell=&sub=1              adds sub{} to the cell doc (from the bin LRU)
//
// Rules: fetched server-side from the peer's format=bin hot path (one
// ?block=1 request per aligned 0.1° box = 16 bins, ≤ 8 in flight, 8 s timeout, their
// Cache-Control honoured), kept only in a bounded LRU (ne_glyph_sub, 32 MB,
// size line on the debug inflight page); nothing on disk, the raw columns
// are never re-served — only pictures, sums and one cell's reading leave.
package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/uber/h3-go/v4"
)

const (
	neSubPolyUnits    = 4096            // IVP1: 1 unit = bin width / 4096 (≈ 0.46 m E-W, 0.68 m N-S)
	neSubBinDeg       = 0.025           // the peer's stored tile = our LOD-0 grid
	neSubTimeout      = 8 * time.Second // a cold bin on the peer takes up to ~2.3 s; a fine LOD-2 tile asks for 32 at once
	neSubMaxInflight  = 8               // a fine LOD-2 tile wants 32 bodies; the peer answers a warm bin in ~5 ms, the rest is the wire
	neSubMaxBody      = 4 << 20
	neSubBlockMaxBody = 16 << 20 // 16 bins in one body (Alm outline bins run to 200 KB each)
	neSubDefaultTTL   = 24 * time.Hour
	neSubRelDubious   = 150 // rel below this = dubious hand-drawn outline (peer's rule)
)

// neSubAttribution — the peer's required attribution line, carried on every
// response that shows the layer (X-Data-Attribution, legend, card).
var (
	neSubPeer        = peers().SubAPI + "/api/v1/invekos/h3"
	neSubPolyPeer    = peers().SubAPI + "/api/v1/invekos/poly"
	neSubPeerName    = peers().SubName
	neSubAttribution = "INVEKOS Schläge © AMA / BML (CC BY 4.0) · per cell and simplified 0.5 m by " + neSubPeerName
)

var neSubClass = [8]string{"none", "acker", "gruenland", "alm", "wein", "dauerkultur", "gloez", "sonst"}
var neSubChg = [6]string{"same", "modified", "split_merged", "partial", "new", "gone"}

// neSubBin — one parsed bin (or the res-7 overview): cells sorted by id and
// the six byte columns.
type neSubBin struct {
	Year, Prev int
	Release    string
	N          int
	Cells      []uint64
	Cls, Cov   []byte
	Bio, Rel   []byte
	Chg        []byte
	Dcov       []int8
	Cls2, Shr  []byte // res-7 only (peer v1.2): second class by area, dominant class share ×255
	// res-12 only (peer v2.3, 2026-10-30): dominant / second Schlagnutzungsart code (0 none),
	// ÖPUL measure bits (bit 0 = bio), AMA land-use category — read by the filter index (ne_glyph_fidx.go)
	Snar, Snar2 []uint16
	Oepul, Nutz []byte
	ETag        string
}

// neSubRelease — the last release string a peer answer carried (state hash input of the filter index).
var neSubRelease atomic.Value

// Persisted (data/ne_fidx/sub_release) like the srtm epoch: a restart must
// not move every filter-index bin's state and force rebuilds.
func neSubReleaseSeen() string {
	if v := neSubRelease.Load(); v != nil {
		return v.(string)
	}
	rl := ""
	if b, err := os.ReadFile(filepath.Join(neFidxDiskDir, "sub_release")); err == nil {
		rl = strings.TrimSpace(string(b))
	}
	neSubRelease.Store(rl)
	return rl
}

func neSubReleaseStore(rl string) {
	if rl == "" || rl == neSubReleaseSeen() {
		return
	}
	neSubRelease.Store(rl)
	os.MkdirAll(neFidxDiskDir, 0o755)
	os.WriteFile(filepath.Join(neFidxDiskDir, "sub_release"), []byte(rl+"\n"), 0o644)
}

func (b *neSubBin) find(id uint64) int {
	k := sort.Search(len(b.Cells), func(i int) bool { return b.Cells[i] >= id })
	if k < len(b.Cells) && b.Cells[k] == id {
		return k
	}
	return -1
}

var (
	neSubLRU     = newNELRU(32 << 20) // H3 bins (~250 KB raw each; a fine LOD-2 tile reads 16–25 of them)
	neSubPolyLRU = newNELRU(24 << 20) // IVP1 outline bins (raw, ~1–200 KB each)
	neSubSem     = make(chan struct{}, neSubMaxInflight)
	neSubClient  = &http.Client{Timeout: neSubTimeout}
	neSubMu      sync.Mutex
	neSubFlight  = map[string]*neSubCall{}
	neSubBlocks  atomic.Int64 // block requests answered by the peer (200/304)
	neSubDown    atomicTime   // last failure; the overview and the tiles answer 204 for a short while
	errNeSubDown = errors.New("subsidy peer unavailable")
)

type neSubCall struct {
	wg  sync.WaitGroup
	raw *neSubRaw
	err error
}

type atomicTime struct {
	mu sync.Mutex
	t  time.Time
}

func (a *atomicTime) set(t time.Time) { a.mu.Lock(); a.t = t; a.mu.Unlock() }
func (a *atomicTime) get() time.Time  { a.mu.Lock(); defer a.mu.Unlock(); return a.t }

// NEGlyphSubStats — size line for /api/v1/debug/inflight.
func NEGlyphSubStats() map[string]interface{} {
	m := neSubLRU.stats()
	m["poly"] = neSubPolyLRU.stats()
	neSubMu.Lock()
	m["inflight"] = len(neSubFlight)
	neSubMu.Unlock()
	m["block_requests"] = neSubBlocks.Load()
	if d := neSubDown.get(); !d.IsZero() {
		m["last_failure"] = d.UTC().Format(time.RFC3339)
	}
	return m
}

// neSubParse — the peer's bin layout: LE u32 n, n×u64 cell, then one byte
// column per cell: cls cov bio rel chg dcov, and (res 7, v1.2) cls2 shr.
// cols = X-Invekos-Cols; the number of columns is taken from the length.
func neSubParse(b []byte) (*neSubBin, error) {
	if len(b) < 4 {
		return nil, errors.New("short bin")
	}
	n := int(binary.LittleEndian.Uint32(b))
	if n == 0 {
		return &neSubBin{}, nil
	}
	if len(b) < 4+n*14 || (len(b)-4-8*n)%n != 0 {
		return nil, fmt.Errorf("bin length %d for n=%d", len(b), n)
	}
	ncol := (len(b) - 4 - 8*n) / n
	bin := &neSubBin{N: n, Cells: make([]uint64, n)}
	for i := 0; i < n; i++ {
		bin.Cells[i] = binary.LittleEndian.Uint64(b[4+8*i:])
	}
	o := 4 + 8*n
	col := func(k int) []byte { return b[o+k*n : o+(k+1)*n] }
	bin.Cls, bin.Cov, bin.Bio, bin.Rel, bin.Chg = col(0), col(1), col(2), col(3), col(4)
	bin.Dcov = make([]int8, n)
	for i, v := range col(5) {
		bin.Dcov[i] = int8(v)
	}
	switch ncol {
	case 8, 10:
		// res-7 overview: cls2, shr (+ snar u16 since v2.3)
		bin.Cls2, bin.Shr = col(6), col(7)
	case 12:
		// res-12 v2.3: snar u16, snar2 u16, oepul, nutz
		bin.Snar, bin.Snar2 = make([]uint16, n), make([]uint16, n)
		s1, s2 := b[o+6*n:o+8*n], b[o+8*n:o+10*n]
		for i := 0; i < n; i++ {
			bin.Snar[i] = binary.LittleEndian.Uint16(s1[2*i:])
			bin.Snar2[i] = binary.LittleEndian.Uint16(s2[2*i:])
		}
		bin.Oepul, bin.Nutz = col(10), col(11)
	}
	return bin, nil
}

// neSubRaw — one peer body (an H3 bin, the res-7 overview or an IVP1
// outline bin) with the headers the painter needs.
type neSubRaw struct {
	Year, Prev int
	Release    string
	ETag       string
	Body       []byte
}

// neSubLRUFor — outline bins (key "p/…") live in their own LRU: a bin of
// Alm outlines is 50–200 KB raw, ten of them would push every H3 bin out.
func neSubLRUFor(key string) *neLRU {
	if strings.HasPrefix(key, "p/") {
		return neSubPolyLRU
	}
	return neSubLRU
}

// LRU entry = "year;prev;release;expiry;etag\n" + raw body. stale=true when
// past the peer's max-age: still usable as a fallback, revalidated with
// If-None-Match.
func neSubCached(key string) (raw *neSubRaw, stale bool) {
	b, ok := neSubLRUFor(key).get(key)
	if !ok {
		return nil, false
	}
	k := strings.IndexByte(string(b[:min(len(b), 200)]), '\n')
	if k < 0 {
		return nil, false
	}
	f := strings.Split(string(b[:k]), ";")
	if len(f) != 5 {
		return nil, false
	}
	raw = &neSubRaw{Release: f[2], ETag: f[4], Body: b[k+1:]}
	raw.Year, _ = strconv.Atoi(f[0])
	raw.Prev, _ = strconv.Atoi(f[1])
	exp, _ := strconv.ParseInt(f[3], 10, 64)
	return raw, time.Now().Unix() > exp
}

// neSubFetch — one body by cache key + peer URL: LRU → single-flight → peer
// (≤ 8 in flight, 8 s). After a failure (not a timeout) every caller gets errNeSubDown for
// 30 s instead of queuing on a dead peer.
func neSubFetch(key, url string) (*neSubRaw, error) {
	cached, stale := neSubCached(key)
	if cached != nil && !stale {
		return cached, nil
	}
	if d := neSubDown.get(); !d.IsZero() && time.Since(d) < 30*time.Second {
		if cached != nil {
			return cached, nil // stale beats nothing while the peer is down
		}
		return nil, errNeSubDown
	}
	neSubMu.Lock()
	if c, ok := neSubFlight[key]; ok {
		neSubMu.Unlock()
		c.wg.Wait()
		return c.raw, c.err
	}
	c := &neSubCall{}
	c.wg.Add(1)
	neSubFlight[key] = c
	neSubMu.Unlock()
	c.raw, c.err = neSubGet(key, url, cached)
	if c.err != nil && cached != nil {
		c.raw, c.err = cached, nil
	}
	neSubMu.Lock()
	delete(neSubFlight, key)
	neSubMu.Unlock()
	c.wg.Done()
	return c.raw, c.err
}

func neSubGet(key, url string, cached *neSubRaw) (*neSubRaw, error) {
	select {
	case neSubSem <- struct{}{}:
	case <-time.After(neSubTimeout):
		return nil, errNeSubDown
	}
	defer func() { <-neSubSem }()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "umfeld-at glyph")
	if cached != nil && cached.ETag != "" {
		req.Header.Set("If-None-Match", cached.ETag)
	}
	resp, err := neSubClient.Do(req)
	if err != nil {
		// a slow body (a cold bin on their side) is not a dead peer: the tile
		// fails, the back-off is for refused connections and 5xx only
		if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
			neSubDown.set(time.Now())
		}
		return nil, errNeSubDown
	}
	defer resp.Body.Close()
	if resp.StatusCode == 304 && cached != nil {
		// unchanged: extend the entry's life
		neSubStore(key, cached, neSubTTL(resp))
		neSubDown.set(time.Time{})
		return cached, nil
	}
	if resp.StatusCode != 200 {
		if resp.StatusCode >= 500 || resp.StatusCode == 429 {
			neSubDown.set(time.Now())
		}
		return nil, fmt.Errorf("peer %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, neSubMaxBody+1))
	if err != nil || len(body) > neSubMaxBody {
		neSubDown.set(time.Now())
		return nil, errNeSubDown
	}
	raw := &neSubRaw{Body: body, Release: resp.Header.Get("X-Invekos-Release"), ETag: resp.Header.Get("ETag")}
	if raw.Release != "" {
		neSubReleaseStore(raw.Release)
	}
	raw.Year, _ = strconv.Atoi(resp.Header.Get("X-Invekos-Year"))
	raw.Prev = raw.Year - 1
	if p := resp.Header.Get("X-Invekos-Prev-Year"); p != "" {
		raw.Prev, _ = strconv.Atoi(p)
	}
	// a body that does not parse is not cached (the peer changed shape; try again later)
	if strings.HasPrefix(key, "p/") {
		if _, err := neSubPolyParse(raw); err != nil {
			return nil, err
		}
	} else if _, err := neSubParse(body); err != nil {
		return nil, err
	}
	neSubStore(key, raw, neSubTTL(resp))
	neSubDown.set(time.Time{})
	return raw, nil
}

func neSubTTL(resp *http.Response) time.Duration {
	ttl := neSubDefaultTTL
	for _, d := range strings.Split(resp.Header.Get("Cache-Control"), ",") {
		d = strings.TrimSpace(d)
		if strings.HasPrefix(d, "max-age=") {
			if s, err := strconv.Atoi(d[8:]); err == nil && s > 0 {
				ttl = time.Duration(s) * time.Second
			}
		}
	}
	return ttl
}

// neSubStore — LRU put with the header line.
func neSubStore(key string, raw *neSubRaw, ttl time.Duration) {
	clean := func(s string) string { return strings.NewReplacer(";", "", "\n", "").Replace(s) }
	hdr := fmt.Sprintf("%d;%d;%s;%d;%s\n", raw.Year, raw.Prev, clean(raw.Release), time.Now().Add(ttl).Unix(), clean(raw.ETag))
	neSubLRUFor(key).put(key, append([]byte(hdr), raw.Body...))
}

// neSubBinOf — parse a cached/fetched H3 body into its columns.
func neSubBinOf(raw *neSubRaw, err error) (*neSubBin, error) {
	if err != nil {
		return nil, err
	}
	bin, err := neSubParse(raw.Body)
	if err != nil {
		return nil, err
	}
	bin.Year, bin.Prev, bin.Release, bin.ETag = raw.Year, raw.Prev, raw.Release, raw.ETag
	return bin, nil
}

// neSubBinAt — the aligned 0.025° bin (i, j) of the LOD-0 grid, filled from
// its 0.1° block (§block) unless single: a seam bin of a tile in another block
// is fetched alone — 16 cold outline bins for one pixel row would cost the
// peer ~2 s for nothing.
func neSubBinAt(i, j int, single bool) (*neSubBin, error) {
	return neSubBinOf(neSubBlockGet(false, i, j, single))
}

// neSubPolyAt — the IVP1 outline bin (i, j): every Schlag of the year whose
// outline intersects the bin, unclipped, plus the gone ones of prev_year.
func neSubPolyAt(i, j int, single bool) (*neSubPolys, error) {
	raw, err := neSubBlockGet(true, i, j, single)
	if err != nil {
		return nil, err
	}
	return neSubPolyParse(raw)
}

// ── block fetch (peer's ?block=1, 2026-10-20) ─────────────────────────────
//
// One request per aligned 0.1° box answers the 16 bins of a fine tile at once
// (`u32 n_bins=16`, then 16 × `u32 len` + the exact per-bin body, k = i*4+j,
// west→east outer, south→north inner). A bin miss fetches its block and fills
// all 16 per-bin LRU entries (keys unchanged: b/i/j, p/i/j), each carrying the
// block's ETag so a stale block revalidates with one If-None-Match. Nothing
// else changes: the painter still reads bins, the cell card still reads a bin.

const neSubBlockDeg = 0.1

func neSubBinURL(poly bool, w, s, size float64, block bool) string {
	base, extra := neSubPeer, "&res=12"
	if poly {
		base, extra = neSubPolyPeer, ""
	}
	u := fmt.Sprintf("%s?bbox=%.3f,%.3f,%.3f,%.3f&crs=4326%s&format=bin", base, w, s, w+size, s+size, extra)
	if block {
		u += "&block=1"
	}
	return u
}

func neSubBinKey(poly bool, i, j int) string {
	if poly {
		return fmt.Sprintf("p/%d/%d", i, j)
	}
	return fmt.Sprintf("b/%d/%d", i, j)
}

func floorDiv4(a int) int { return int(math.Floor(float64(a) / 4)) }

// neSubBlockGet — the bin (i, j) via LRU → single-flight on its block → peer
// (single: the bin alone through neSubFetch).
func neSubBlockGet(poly bool, i, j int, single bool) (*neSubRaw, error) {
	key := neSubBinKey(poly, i, j)
	cached, stale := neSubCached(key)
	if cached != nil && !stale {
		return cached, nil
	}
	if single {
		return neSubFetch(key, neSubBinURL(poly, float64(i)*neSubBinDeg, float64(j)*neSubBinDeg, neSubBinDeg, false))
	}
	if d := neSubDown.get(); !d.IsZero() && time.Since(d) < 30*time.Second {
		if cached != nil {
			return cached, nil
		}
		return nil, errNeSubDown
	}
	I, J := floorDiv4(i), floorDiv4(j)
	fkey := fmt.Sprintf("B%s/%d/%d", key[:1], I, J)
	neSubMu.Lock()
	if c, ok := neSubFlight[fkey]; ok {
		neSubMu.Unlock()
		c.wg.Wait()
	} else {
		c := &neSubCall{}
		c.wg.Add(1)
		neSubFlight[fkey] = c
		neSubMu.Unlock()
		c.err = neSubBlockFetch(poly, I, J)
		neSubMu.Lock()
		delete(neSubFlight, fkey)
		neSubMu.Unlock()
		c.wg.Done()
	}
	// whatever the block fetch did, the answer is what the LRU holds now
	if got, _ := neSubCached(key); got != nil {
		return got, nil
	}
	if cached != nil {
		return cached, nil
	}
	return nil, errNeSubDown
}

// neSubBlockFetch — GET the block (I, J), validate the 16 bodies, store them.
// A peer without ?block=1 (400/404/405) falls back to the 16 single bins
// through neSubGet so the layer keeps working either way.
func neSubBlockFetch(poly bool, I, J int) error {
	// the block's ETag is on every bin entry; revalidate only when all 16 are still here
	var all []*neSubRaw
	etag := ""
	for k := 0; k < 16; k++ {
		c, _ := neSubCached(neSubBinKey(poly, I*4+k/4, J*4+k%4))
		if c == nil || c.ETag == "" || (etag != "" && c.ETag != etag) {
			all = nil
			break
		}
		etag = c.ETag
		all = append(all, c)
	}
	select {
	case neSubSem <- struct{}{}:
	case <-time.After(neSubTimeout):
		return errNeSubDown
	}
	defer func() { <-neSubSem }()
	w, s := float64(I)*neSubBlockDeg, float64(J)*neSubBlockDeg
	req, err := http.NewRequest("GET", neSubBinURL(poly, w, s, neSubBlockDeg, true), nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "umfeld-at glyph")
	if all != nil {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := neSubClient.Do(req)
	if err != nil {
		if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
			neSubDown.set(time.Now())
		}
		return errNeSubDown
	}
	defer resp.Body.Close()
	neSubBlocks.Add(1)
	if resp.StatusCode == 304 && all != nil {
		ttl := neSubTTL(resp)
		for k, c := range all {
			neSubStore(neSubBinKey(poly, I*4+k/4, J*4+k%4), c, ttl)
		}
		neSubDown.set(time.Time{})
		return nil
	}
	if resp.StatusCode != 200 {
		if resp.StatusCode >= 500 || resp.StatusCode == 429 {
			neSubDown.set(time.Now())
			return fmt.Errorf("peer %d", resp.StatusCode)
		}
		// no block support: the 16 single bins (sem is held; release it for them)
		<-neSubSem
		defer func() { neSubSem <- struct{}{} }()
		var firstErr error
		for k := 0; k < 16; k++ {
			i, j := I*4+k/4, J*4+k%4
			key := neSubBinKey(poly, i, j)
			c, _ := neSubCached(key)
			bw, bs := float64(i)*neSubBinDeg, float64(j)*neSubBinDeg
			if _, err := neSubGet(key, neSubBinURL(poly, bw, bs, neSubBinDeg, false), c); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, neSubBlockMaxBody+1))
	if err != nil || len(body) > neSubBlockMaxBody {
		neSubDown.set(time.Now())
		return errNeSubDown
	}
	year, _ := strconv.Atoi(resp.Header.Get("X-Invekos-Year"))
	prev := year - 1
	if p := resp.Header.Get("X-Invekos-Prev-Year"); p != "" {
		prev, _ = strconv.Atoi(p)
	}
	raws, err := neSubBlockSplit(poly, body)
	if err != nil {
		return fmt.Errorf("block: %w", err)
	}
	ttl := neSubTTL(resp)
	if rl := resp.Header.Get("X-Invekos-Release"); rl != "" {
		neSubReleaseStore(rl)
	}
	for k, raw := range raws {
		raw.Year, raw.Prev, raw.Release, raw.ETag = year, prev, resp.Header.Get("X-Invekos-Release"), resp.Header.Get("ETag")
		neSubStore(neSubBinKey(poly, I*4+k/4, J*4+k%4), raw, ttl)
	}
	neSubDown.set(time.Time{})
	return nil
}

// neSubBlockSplit — the 16 per-bin bodies of a block, each one validated
// (a body that does not parse fails the whole block: nothing is cached).
func neSubBlockSplit(poly bool, b []byte) ([]*neSubRaw, error) {
	if len(b) < 4 || binary.LittleEndian.Uint32(b) != 16 {
		return nil, errors.New("n_bins != 16")
	}
	o := 4
	out := make([]*neSubRaw, 0, 16)
	for k := 0; k < 16; k++ {
		if o+4 > len(b) {
			return nil, errors.New("short")
		}
		n := int(binary.LittleEndian.Uint32(b[o:]))
		o += 4
		if n < 0 || o+n > len(b) || n > neSubMaxBody {
			return nil, errors.New("bad len")
		}
		raw := &neSubRaw{Body: b[o : o+n : o+n]}
		o += n
		if poly {
			if _, err := neSubPolyParse(raw); err != nil {
				return nil, err
			}
		} else if _, err := neSubParse(raw.Body); err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	if o != len(b) {
		return nil, errors.New("trailing bytes")
	}
	return out, nil
}

// neSubOverview — the national res-7 file (cls/cov/bio/dcov per cell).
func neSubOverview() (*neSubBin, error) {
	return neSubBinOf(neSubFetch("r7", neSubPeer+"?res=7&format=bin"))
}

// neSubCellAt — one res-12 cell's reading from its bin (nil, false when the
// peer is down; nil, true when the cell is not claimed).
func neSubCellAt(id uint64) (map[string]interface{}, bool) {
	lon, lat := neCellCentre(id)
	bin, err := neSubBinAt(int(math.Floor(lon/neSubBinDeg)), int(math.Floor(lat/neSubBinDeg)), false)
	if err != nil {
		return nil, false
	}
	k := bin.find(id)
	schlag := neSubSchlagAt(lon, lat) // the outline under the cell centre (nil: none, or no outline bin)
	if k < 0 || (bin.Cov[k] == 0 && bin.Chg[k] != 5) {
		o := map[string]interface{}{"claimed": false, "year": bin.Year, "release": bin.Release}
		if schlag != nil {
			o["schlag"] = schlag
		}
		return o, true
	}
	o := map[string]interface{}{
		"claimed": bin.Cov[k] > 0, "claimed_pct": int(math.Round(float64(bin.Cov[k]) / 255 * 100)),
		"class": neSubClass[bin.Cls[k]&7], "reliable": bin.Rel[k] >= neSubRelDubious,
		"vs_prev": neSubChg[min(int(bin.Chg[k]), 5)], "d_claimed_pct": int(math.Round(float64(bin.Dcov[k]) / 255 * 100)),
		"year": bin.Year, "prev_year": bin.Prev, "release": bin.Release,
	}
	if bin.Bio[k] == 255 {
		o["bio"] = nil
	} else {
		o["bio"] = bin.Bio[k] >= 128
	}
	if schlag != nil {
		o["schlag"] = schlag
	}
	return o, true
}

// ── painting ─────────────────────────────────────────────────────────────────

// neGlyphSubRGB — Okabe-Ito orange (the page's --gsub); one hue, alpha = cov.
var neGlyphSubRGB = [3]uint8{0xe6, 0x9f, 0x00}

const neGlyphSubShades = 8 // wash alpha .3 → .9 by cov

// palette: 0 transparent · 1..8 wash by cov · 9 stroke (new / gone rings, full) · 10 light stroke (redrawn hairline)
const neGlyphIdxSubStroke = 1 + neGlyphSubShades
const neGlyphIdxSubLight = 2 + neGlyphSubShades

func neGlyphSubPalette() color.Palette {
	p := color.Palette{color.NRGBA{0, 0, 0, 0}}
	c := neGlyphSubRGB
	for s := 0; s < neGlyphSubShades; s++ {
		a := 0.3 + 0.6*float64(s)/float64(neGlyphSubShades-1)
		p = append(p, color.NRGBA{c[0], c[1], c[2], uint8(math.Round(255 * a))})
	}
	return append(p, color.NRGBA{c[0], c[1], c[2], 245}, color.NRGBA{c[0], c[1], c[2], 130})
}

func neGlyphSubShade(cov byte) uint8 {
	s := int(cov) * neGlyphSubShades / 256
	return uint8(1 + s)
}

// neGlyphSubMeta — the layer's sums for X-NE-Tile.
type neGlyphSubMeta struct {
	n      int      // claimed cells painted
	cov    int64    // Σ cov (1/255 cell units, res-12 equivalents)
	cls    [8]int64 // Σ cov per class
	bio    int64    // Σ cov·bio/255 of cells with a known bio flag
	bioUnk int      // claimed cells with bio unknown
	dub    int      // claimed cells with rel < 150
	chg    [6]int
	dcov   int64
	cells  int // res-12 cells in the tile box (the "visible land")
	year   int
	prev   int
	rel    string
	down   bool
	// LOD 0/1 with the outline bins (ne_glyph_subpoly.go): Schläge counted once each
	poly  bool
	pn    int    // Schläge of the year painted (first vertex in the tile's box)
	pchg  [6]int // by lineage kind (gone = the year before)
	pha   int64  // Σ area_m2 of the year's Schläge
	pnew  int64  // Σ area_m2 of the new ones
	pgone int64  // Σ prev-year area_m2 of the gone ones
}

func (m *neGlyphSubMeta) String() string {
	cl := make([]string, 8)
	for i := range cl {
		cl[i] = strconv.FormatInt(m.cls[i], 10)
	}
	ch := make([]string, 6)
	for i := range ch {
		ch[i] = strconv.Itoa(m.chg[i])
	}
	s := fmt.Sprintf(";sub=%d;cov=%d;cls=%s;bio=%d,%d;dub=%d;chg=%s;dcov=%d;cells=%d;year=%d;prev=%d;rel=%s",
		m.n, m.cov, strings.Join(cl, ","), m.bio, m.bioUnk, m.dub, strings.Join(ch, ","), m.dcov, m.cells, m.year, m.prev, m.rel)
	if m.poly {
		pc := make([]string, 6)
		for i := range pc {
			pc[i] = strconv.Itoa(m.pchg[i])
		}
		s += fmt.Sprintf(";pn=%d;pchg=%s;pha=%d;pnew=%d;pgone=%d", m.pn, strings.Join(pc, ","), m.pha, m.pnew, m.pgone)
	}
	return s
}

// inner — the hexagon shrunk about its centroid.
func neSubInner(poly [][2]float64, np int, cx, cy, f float64, out *[8][2]float64) [][2]float64 {
	for v := 0; v < np; v++ {
		out[v] = [2]float64{cx + (poly[v][0]-cx)*f, cy + (poly[v][1]-cy)*f}
	}
	return out[:np]
}

// neGlyphSubRender — the v=sub / v=subc picture of one tile. ok=false when
// the peer is down (the handler answers 204).
// neGlyphSubRender — one cut per LOD, the same path at every LOD (LOD 2 = the
// 16 bins of the 0.1° box + seams, 2026-10-30; the res-7 wash it used to paint
// below ×40 flashed under the bins and is gone). lod is kept for the callers'
// symmetry with the other views.
func neGlyphSubRender(g *neGlyphTileGeom, lod int, w, s, e, n float64, strokes bool, m *neGlyphSubMeta) (*image.Paletted, bool) {
	_ = lod
	const padDeg = 0.0005
	img := image.NewPaletted(image.Rect(0, 0, g.pw, g.ph), neGlyphSubPalette())
	var poly, in [8][2]float64
	_ = in
	// the covering
	// bins of the padded box (the seam cells of the neighbours paint their half,
	// like the base tile)
	i0, i1 := int(math.Floor((w-padDeg)/neSubBinDeg)), int(math.Floor((e+padDeg)/neSubBinDeg))
	j0, j1 := int(math.Floor((s-padDeg)/neSubBinDeg)), int(math.Floor((n+padDeg)/neSubBinDeg))
	m.cells = len(neBBoxCells(w, s, e, n))
	var kinds map[uint64]uint8 // strokes: lineage kind class per cell (1 new, 2 gone, 3 redrawn) across the bins
	if strokes {
		kinds = make(map[uint64]uint8, 4096)
	}
	type binAt struct {
		bin   *neSubBin
		whole bool
	}
	var bins []binAt
	var pbins []neSubPolyBin // the outline bins; nil as soon as one cannot be had → hexagons
	// a LOD-2 tile covers 16 bins (+ seams): fetch them side by side
	// (neSubSem bounds the peer at 4) so a cold tile is not 32 round trips in a row
	type binRes struct {
		i, j   int
		single bool
		bin    *neSubBin
		err    error
		p      *neSubPolyBin
		perr   error
	}
	var res []*binRes
	// the tile's own 0.1° block comes as one ?block=1 body; a seam bin that
	// falls into a neighbouring block is fetched alone
	cI, cJ := floorDiv4(int(math.Floor((w+e)/2/neSubBinDeg))), floorDiv4(int(math.Floor((s+n)/2/neSubBinDeg)))
	for i := i0; i <= i1; i++ {
		for j := j0; j <= j1; j++ {
			res = append(res, &binRes{i: i, j: j, single: floorDiv4(i) != cI || floorDiv4(j) != cJ})
		}
	}
	var wg sync.WaitGroup
	for _, r := range res {
		wg.Add(1)
		go func(r *binRes) {
			defer wg.Done()
			var pw sync.WaitGroup
			pw.Add(1)
			go func() {
				defer pw.Done()
				if p, err := neSubPolyAt(r.i, r.j, r.single); err == nil {
					r.p = &neSubPolyBin{r.i, r.j, p}
				} else {
					r.perr = err
				}
			}()
			r.bin, r.err = neSubBinAt(r.i, r.j, r.single)
			pw.Wait()
		}(r)
	}
	wg.Wait()
	for _, r := range res {
		{
			i, j, bin := r.i, r.j, r.bin
			if r.err != nil {
				return nil, false
			}
			m.year, m.prev, m.rel = bin.Year, bin.Prev, bin.Release
			bins = append(bins, binAt{bin, i > i0 && i < i1 && j > j0 && j < j1})
			if pbins != nil || len(bins) == 1 {
				if r.p != nil {
					pbins = append(pbins, *r.p)
				} else {
					pbins = nil
				}
			}
			if strokes {
				for k, id := range bin.Cells {
					if kc := neSubKind(bin.Chg[k]); kc != 0 {
						kinds[id] = kc
					}
				}
			}
		}
	}
	for _, ba := range bins {
		bin := ba.bin
		for k, id := range bin.Cells {
			if !ba.whole {
				lon, lat := neCellCentre(id)
				if lon < w-padDeg || lon > e+padDeg || lat < s-padDeg || lat > n+padDeg {
					continue
				}
			}
			cov, chg := bin.Cov[k], bin.Chg[k]
			if cov == 0 && chg != 5 {
				continue
			}
			if lon0(id, w, s, e, n) {
				neSubSum(m, bin, k)
			}
			if pbins != nil {
				continue // the picture comes from the outlines below; the sums still count every cell
			}
			if strokes {
				// lineage as the OUTLINE of each run of same-kind cells (the
				// hexagons are a Voronoi tiling, so the union edge is the
				// Schlag's shape): new = 2 px solid, gone = dotted, redrawn =
				// 1 px light. Never a fill, so the change fills stay legible.
				kc := neSubKind(chg)
				if kc == 0 {
					continue
				}
				neSubOutline(img, g, id, kc, kinds)
				continue
			}
			if cov == 0 {
				continue
			}
			np, cx, cy, ok := g.poly(id, &poly)
			if !ok {
				continue
			}
			ci := neGlyphSubShade(cov)
			mask := 0
			if bin.Rel[k] < neSubRelDubious {
				mask = 1 // dubious outline: hatched
			}
			neFillPolyMask(img, poly[:np], ci, mask)
			if bin.Cls[k] == 6 { // gloez (hedges, verges): a thin ring, never a fill
				neFillPoly(img, neSubInner(poly[:np], np, cx, cy, 0.62, &in), 0)
			}
		}
	}
	if pbins != nil {
		neGlyphSubPaintPolys(img, g, pbins, w, s, e, n, strokes, m)
	}
	return img, true
}

// lon0 — centre strictly inside the tile's own (half-open) box: the sums
// count every cell once across tiles.
func lon0(id uint64, w, s, e, n float64) bool {
	lon, lat := neCellCentre(id)
	return lon >= w && lon < e && lat >= s && lat < n
}

// neSubSum — one cell into the sums (res-12 cells, each counted once).
func neSubSum(m *neGlyphSubMeta, bin *neSubBin, k int) {
	cov := int64(bin.Cov[k])
	if bin.Cov[k] > 0 {
		m.n++
		m.cov += cov
		m.cls[bin.Cls[k]&7] += cov
		if bin.Bio[k] == 255 {
			m.bioUnk++
		} else {
			m.bio += cov * int64(bin.Bio[k]) / 255
		}
		if bin.Rel[k] < neSubRelDubious {
			m.dub++
		}
	}
	m.chg[min(int(bin.Chg[k]), 5)]++
	m.dcov += int64(bin.Dcov[k])
}

// neFillPolyMask — neFillPoly with a pixel mask: 0 solid, 1 diagonal
// stripes (hatch), 2 chequer dots.
func neFillPolyMask(img *image.Paletted, p [][2]float64, ci uint8, mask int) {
	if mask == 0 {
		neFillPoly(img, p, ci)
		return
	}
	miny, maxy := math.Inf(1), math.Inf(-1)
	for _, v := range p {
		miny, maxy = math.Min(miny, v[1]), math.Max(maxy, v[1])
	}
	b := img.Bounds()
	y0, y1 := int(math.Ceil(miny-0.5)), int(math.Floor(maxy-0.5))
	if y0 < b.Min.Y {
		y0 = b.Min.Y
	}
	if y1 >= b.Max.Y {
		y1 = b.Max.Y - 1
	}
	var xs [8]float64
	for y := y0; y <= y1; y++ {
		sy := float64(y) + 0.5
		n := 0
		for i := range p {
			a, c := p[i], p[(i+1)%len(p)]
			if (a[1] <= sy) == (c[1] <= sy) {
				continue
			}
			xs[n] = a[0] + (sy-a[1])*(c[0]-a[0])/(c[1]-a[1])
			n++
			if n == 8 {
				break
			}
		}
		if n < 2 {
			continue
		}
		sort.Float64s(xs[:n])
		for k := 0; k+1 < n; k += 2 {
			xa, xb := int(math.Ceil(xs[k]-0.5)), int(math.Floor(xs[k+1]-0.5))
			if xa < b.Min.X {
				xa = b.Min.X
			}
			if xb >= b.Max.X {
				xb = b.Max.X - 1
			}
			row := img.Pix[(y-b.Min.Y)*img.Stride:]
			for x := xa; x <= xb; x++ {
				var on bool
				if mask == 1 {
					on = (x+y)&3 < 2
				} else {
					on = ((x>>1)+(y>>1))&1 == 0
				}
				if on {
					row[x-b.Min.X] = ci
				}
			}
		}
	}
}

// ── overview ─────────────────────────────────────────────────────────────────

// handleNEGlyphSub — GET /api/v1/ne/glyph/sub: the res-7 overview columns
// in the cells[] order of /ne/glyph (cov, cls, bio, dcov; 0 where the peer
// has no row), so the dots and hex faces can carry the layer without an H3
// library. 503 while the peer is down (no-store).
func handleNEGlyphSub(w http.ResponseWriter, r *http.Request) {
	if err := neGlyphLoadFrame(); err != nil {
		apiError(w, "glyph frame unavailable", 503)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("X-Data-Attribution", asciiFold(neSubAttribution))
	day := time.Now().UTC().Format("2006-01-02")
	key := "sub7/" + day
	if b, ok := neGlyphTileLRU.get(key); ok {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(b)
		return
	}
	ov, err := neSubOverview()
	if err != nil {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Retry-After", "30")
		apiError(w, "subsidy layer unavailable", 503)
		return
	}
	fr := &neGlyphFrame
	nc := len(fr.cells)
	cov, cls, bio, dcov := make([]int, nc), make([]int, nc), make([]int, nc), make([]int, nc)
	chg, cls2, shr := make([]int, nc), make([]int, nc), make([]int, nc)
	joined := 0
	for i, c := range fr.cells {
		k := ov.find(c.id)
		if k < 0 {
			bio[i] = 255
			continue
		}
		joined++
		cov[i], cls[i], bio[i], dcov[i], chg[i] = int(ov.Cov[k]), int(ov.Cls[k]), int(ov.Bio[k]), int(ov.Dcov[k]), int(ov.Chg[k])
		if ov.Cls2 != nil {
			cls2[i], shr[i] = int(ov.Cls2[k]), int(ov.Shr[k])
		}
	}
	b, _ := json.Marshal(map[string]interface{}{
		"year": ov.Year, "prev_year": ov.Prev, "release": ov.Release, "n": joined,
		"fields": []string{"cov", "cls", "bio", "dcov", "chg", "cls2", "shr"}, "classes": neSubClass[:],
		"cov": cov, "cls": cls, "bio": bio, "dcov": dcov, "chg": chg, "cls2": cls2, "shr": shr,
		"units":       "cov, bio in 1/255 of the cell (bio 255 = unknown, 254 = all); dcov = cov − cov(prev_year), int8; chg = share of the cell's claimed area with a lineage event vs prev_year ×255; cls dominant class, cls2 second (0 none), shr = dominant class share of the claimed area ×255; order = cells[] of /api/v1/ne/glyph",
		"attribution": neSubAttribution, "source": neSubPeer + "?res=7",
	})
	neGlyphTileLRU.put(key, b)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(b)
}

// neSubKind — lineage kind class of a chg byte: 1 new, 2 gone, 3 redrawn
// (modified / split_merged / partial), 0 same.
func neSubKind(chg byte) uint8 {
	switch chg {
	case 4:
		return 1
	case 5:
		return 2
	case 1, 2, 3:
		return 3
	}
	return 0
}

// neSubOutline — the edges of cell id whose neighbour is not of the same
// kind class, drawn in the kind's line style.
func neSubOutline(img *image.Paletted, g *neGlyphTileGeom, id uint64, kc uint8, kinds map[uint64]uint8) {
	edges, err := h3.Cell(id).DirectedEdges()
	if err != nil {
		return
	}
	for _, e := range edges {
		dst, err := e.Destination()
		if err == nil && kinds[uint64(dst)] == kc {
			continue
		}
		bd, err := e.Boundary()
		if err != nil || len(bd) < 2 {
			continue
		}
		ax, ay := neGlyphProj(bd[0].Lng, bd[0].Lat)
		bx, by := neGlyphProj(bd[len(bd)-1].Lng, bd[len(bd)-1].Lat)
		x0, y0 := (ax-g.x0)*g.lod.px, (ay-g.y0)*g.lod.px
		x1, y1 := (bx-g.x0)*g.lod.px, (by-g.y0)*g.lod.px
		switch kc {
		case 1:
			neDrawLine(img, x0, y0, x1, y1, neGlyphIdxSubStroke, 2, 0)
		case 2:
			neDrawLine(img, x0, y0, x1, y1, neGlyphIdxSubStroke, 1, 2)
		default:
			neDrawLine(img, x0, y0, x1, y1, neGlyphIdxSubLight, 1, 0)
		}
	}
}

// neDrawLine — DDA line, width 1 or 2 px, dash = on/off run length in px (0 solid).
func neDrawLine(img *image.Paletted, x0, y0, x1, y1 float64, ci uint8, width, dash int) {
	b := img.Bounds()
	dx, dy := x1-x0, y1-y0
	steps := int(math.Ceil(math.Max(math.Abs(dx), math.Abs(dy))))
	if steps < 1 {
		steps = 1
	}
	set := func(x, y int) {
		if x >= b.Min.X && x < b.Max.X && y >= b.Min.Y && y < b.Max.Y {
			img.Pix[(y-b.Min.Y)*img.Stride+(x-b.Min.X)] = ci
		}
	}
	for i := 0; i <= steps; i++ {
		if dash > 0 && (i/dash)&1 == 1 {
			continue
		}
		t := float64(i) / float64(steps)
		x, y := int(math.Floor(x0+dx*t)), int(math.Floor(y0+dy*t))
		set(x, y)
		if width > 1 {
			if math.Abs(dx) > math.Abs(dy) {
				set(x, y+1)
			} else {
				set(x+1, y)
			}
		}
	}
}
