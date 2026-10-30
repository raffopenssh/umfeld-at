// ne_mvt.go — the cell layer for GIS clients: every held H3 cell as a
// polygon with ALL its attributes (declared · observed · diff · change ·
// subsidy · LiDAR statistics incl. epochs · Natura 2000 · distances) as
// Mapbox Vector Tiles, and — ne_mvt_jobs.go — the same tiles bundled into a
// PMTiles archive per bbox for offline work. docs/qgis-layer.md.
//
//	GET /api/v1/ne/mvt/{z}/{x}/{y}.pbf        z 9–15, layer "cells"
//	GET /api/v1/ne/mvt/status?bbox=w,s,e,n     readiness of the area (bins ready / building / queued)
//	GET /api/v1/ne/mvt.json                    TileJSON (QGIS / MapLibre source description)
//
// Same source as the filter and the unit export: the filter index bins
// (ne_glyph_fidx.go, one 0.025° bin = every res-12 cell with ~50 quantised
// columns, peers {OBS_NAME} + {SUB_NAME} merged at build time). A tile
// reads the bins its box touches, so a cell carries the same numbers here, in
// `/ne/filter` and in `/ne/stats/unit` — cell for cell.
//
// Zoom → resolution: z 9 → res 7 … z ≥ 14 → res 12 (neMVTZoomRes). Below
// res 12 a feature is the parent cell with aggregates of its res-12 children
// (counts, dominant groups + shares, means, maxima) — never a sample. The
// parent is kept when its CENTRE lies in the tile; its children are gathered
// from the bins under the tile box padded by one parent radius, so the same
// parent gets the same figures in every tile that shows it.
//
// Loading: bins are built on demand (≈ 9 s each, peers included). A tile
// whose bins are all ready answers at once (LRU ne_mvt, gzip'd, ETag); a
// tile with ≤ neMVTWaitBins missing bins builds them inline (≤ neMVTWait);
// more than that → 202 + Retry-After + JSON progress, the bins go to the
// build queue (one worker, bounded, dedup'd) and the next request finds them.
// Renders are bounded by a semaphore (neMVTSem); beyond it callers wait ≤ 10 s
// then get 503 — a burst never takes the service down.
//
// Gate: ne_mvt_gate.go (bbox allow-list + daily tiles/km² per name, counted
// until NE_MVT_GATE_ENFORCE=1) in front of the export gate's identity.
//
// Transfer: tiles are gzip'd once at render time and served with
// Content-Encoding: gzip straight from the LRU (the one deliberate exception
// to "never set Content-Encoding" — the gzip middleware sees it and stands
// aside); the same bytes go into archives. ~8 k polygons × 50 attributes
// gzip to 150–400 KB at z 14.
package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/uber/h3-go/v4"
)

const (
	neMVTMinZ      = 9
	neMVTMaxZ      = 15
	neMVTLayerName = "cells"
	neMVTWaitBins  = 3
	neMVTWait      = 20 * time.Second
	neMVTSemWait   = 10 * time.Second
	neMVTQueueMax  = 4096
)

var (
	neMVTLRU      = newNELRU(32 << 20) // gzip'd tiles
	neMVTIDsLRU   = newNELRU(24 << 20) // per bin: res-12 ids (8 B/cell), keyed by bin state
	neMVTSem      = make(chan struct{}, 2)
	neMVTRendered atomic.Int64
	neMVTRenderMs atomic.Int64
	neMVTPending  atomic.Int64 // 202s handed out
	neMVTStateMu  sync.Mutex
	neMVTStateMm  = map[[2]int]neMVTStateMemo{}
)

type neMVTStateMemo struct {
	st string
	ep string // srtm epoch + subsidy release the state was computed with
	at time.Time
}

func neMVTStateEpochs() string { return neFidxSrtmEpoch() + "|" + neSubReleaseSeen() }

// NEMVTStats — /debug/inflight line.
func NEMVTStats() map[string]interface{} {
	m := neMVTLRU.stats()
	m["ids_lru"] = neMVTIDsLRU.stats()
	m["rendered"] = neMVTRendered.Load()
	m["render_ms_sum"] = neMVTRenderMs.Load()
	m["pending_202"] = neMVTPending.Load()
	m["queue"] = neFidxQueueStats()
	m["jobs"] = nePMTJobStats()
	m["national"] = NENatStats()
	return m
}

// neMVTZoomRes — the H3 resolution drawn at a zoom.
func neMVTZoomRes(z int) int {
	switch {
	case z <= 9:
		return 7
	case z >= 14:
		return 12
	}
	return z - 2 // 10→8 11→9 12→10 13→11
}

// neMVTResPadDeg — one cell circumradius of res r in degrees (lon, lat) at
// Austria's latitude, generous.
func neMVTResPadDeg(r int) (float64, float64) {
	edgeKm := []float64{1281, 483, 183, 69, 26, 9.85, 3.72, 1.41, 0.53, 0.2, 0.076, 0.029, 0.011, 0.004, 0.002, 0.001}[r]
	km := edgeKm * 1.15
	return km / 74.0, km / 111.0
}

// ── H3 without cgo ──────────────────────────────────────────────────────────

// neH3ParentFast — parent at res p by bit surgery (digits p+1…15 → 7).
func neH3ParentFast(id uint64, p int) uint64 {
	mask := (uint64(1) << uint(45-3*p)) - 1 // digit r sits at bits 45-3r … 47-3r
	return (id &^ (0xF << 52)) | (uint64(p) << 52) | mask
}

// ── bins under a tile ──────────────────────────────────────────────────────

// neMVTBinState — neFidxState with a 30 s memo (a low-zoom tile asks for hundreds).
func neMVTBinState(i, j int) string {
	ep := neMVTStateEpochs()
	neMVTStateMu.Lock()
	if m, ok := neMVTStateMm[[2]int{i, j}]; ok && m.ep == ep && time.Since(m.at) < 30*time.Second {
		neMVTStateMu.Unlock()
		return m.st
	}
	neMVTStateMu.Unlock()
	st := neFidxState(i, j)
	neMVTStateMu.Lock()
	if len(neMVTStateMm) > 20000 {
		neMVTStateMm = map[[2]int]neMVTStateMemo{}
	}
	neMVTStateMm[[2]int{i, j}] = neMVTStateMemo{st, ep, time.Now()}
	neMVTStateMu.Unlock()
	return st
}

func neMVTStateForget(i, j int) {
	neMVTStateMu.Lock()
	delete(neMVTStateMm, [2]int{i, j})
	neMVTStateMu.Unlock()
}

// neFidxReady — the bin's blob if it exists (LRU or disk) at the current state; never builds.
func neFidxReady(i, j int) ([]byte, string, bool) {
	st := neMVTBinState(i, j)
	key := fmt.Sprintf("%d/%d/%s", i, j, st)
	if b, ok := neFidxLRU.get(key); ok {
		return b, st, true
	}
	if b := neFidxDiskRead(st, i, j); b != nil {
		neFidxDiskHits.Add(1)
		neFidxLRU.put(key, b)
		return b, st, true
	}
	return nil, st, false
}

// neMVTBins — bin indices under a lon/lat box, clipped to Austria.
func neMVTBins(w, s, e, n float64) [][2]int {
	w, s = math.Max(w, neMVTAustria[0]), math.Max(s, neMVTAustria[1])
	e, n = math.Min(e, neMVTAustria[2]), math.Min(n, neMVTAustria[3])
	if !(w < e && s < n) {
		return nil
	}
	i0, i1 := int(math.Floor(w/neFidxBinDeg)), int(math.Floor((e-1e-9)/neFidxBinDeg))
	j0, j1 := int(math.Floor(s/neFidxBinDeg)), int(math.Floor((n-1e-9)/neFidxBinDeg))
	out := make([][2]int, 0, (i1-i0+1)*(j1-j0+1))
	for i := i0; i <= i1; i++ {
		for j := j0; j <= j1; j++ {
			out = append(out, [2]int{i, j})
		}
	}
	return out
}

// ── build queue ───────────────────────────────────────────────────────────────

var neFidxQ struct {
	mu      sync.Mutex
	once    sync.Once
	set     map[[2]int]bool
	list    [][2]int
	busy    [2]int
	isBusy  bool
	done    atomic.Int64
	dropped atomic.Int64
	wake    chan struct{}
}

func neFidxQueueStats() map[string]interface{} {
	neFidxQ.mu.Lock()
	defer neFidxQ.mu.Unlock()
	m := map[string]interface{}{"queued": len(neFidxQ.list), "max": neMVTQueueMax, "built": neFidxQ.done.Load(), "dropped": neFidxQ.dropped.Load()}
	if neFidxQ.isBusy {
		m["building"] = fmt.Sprintf("%d/%d", neFidxQ.busy[0], neFidxQ.busy[1])
	}
	return m
}

// neFidxEnqueue — ask the worker for a bin; duplicates and overflow are dropped.
func neFidxEnqueue(i, j int) {
	neFidxQ.once.Do(func() {
		neFidxQ.set = map[[2]int]bool{}
		neFidxQ.wake = make(chan struct{}, 1)
		go neFidxQueueWorker()
	})
	k := [2]int{i, j}
	neFidxQ.mu.Lock()
	if neFidxQ.set[k] || (neFidxQ.isBusy && neFidxQ.busy == k) {
		neFidxQ.mu.Unlock()
		return
	}
	if len(neFidxQ.list) >= neMVTQueueMax {
		neFidxQ.dropped.Add(1)
		neFidxQ.mu.Unlock()
		return
	}
	neFidxQ.set[k] = true
	neFidxQ.list = append(neFidxQ.list, k)
	neFidxQ.mu.Unlock()
	select {
	case neFidxQ.wake <- struct{}{}:
	default:
	}
}

// neFidxQueued — position of a bin in the queue (-1 = not queued, 0 = building).
func neFidxQueued(i, j int) int {
	k := [2]int{i, j}
	neFidxQ.mu.Lock()
	defer neFidxQ.mu.Unlock()
	if neFidxQ.isBusy && neFidxQ.busy == k {
		return 0
	}
	if !neFidxQ.set[k] {
		return -1
	}
	for p, q := range neFidxQ.list {
		if q == k {
			return p + 1
		}
	}
	return -1
}

func neFidxQueueWorker() {
	for {
		neFidxQ.mu.Lock()
		if len(neFidxQ.list) == 0 {
			neFidxQ.mu.Unlock()
			<-neFidxQ.wake
			continue
		}
		k := neFidxQ.list[0]
		neFidxQ.list = neFidxQ.list[1:]
		delete(neFidxQ.set, k)
		neFidxQ.busy, neFidxQ.isBusy = k, true
		neFidxQ.mu.Unlock()
		if _, _, ok := neFidxReady(k[0], k[1]); !ok {
			if _, _, err := neFidxGet(k[0], k[1]); err == nil {
				neFidxQ.done.Add(1)
			}
			neMVTStateForget(k[0], k[1])
		}
		neFidxQ.mu.Lock()
		neFidxQ.isBusy = false
		neFidxQ.mu.Unlock()
	}
}

// ── cells of a bin ───────────────────────────────────────────────────────────

// neMVTBinIDs — the res-12 id of every cell in a bin (cgo once per cell, then LRU).
func neMVTBinIDs(b *neFidxBin, st string) []uint64 {
	key := fmt.Sprintf("%d/%d/%s", b.Hdr.I, b.Hdr.J, st)
	if raw, ok := neMVTIDsLRU.get(key); ok && len(raw) == 8*b.N {
		ids := make([]uint64, b.N)
		for k := range ids {
			ids[k] = binary.LittleEndian.Uint64(raw[8*k:])
		}
		return ids
	}
	origin, err := neParseCellHex(b.Hdr.Origin)
	if err != nil {
		return nil
	}
	ids := make([]uint64, b.N)
	raw := make([]byte, 8*b.N)
	for k := range ids {
		if id, ok := b.cell(h3.Cell(origin), k); ok {
			ids[k] = id
		}
		binary.LittleEndian.PutUint64(raw[8*k:], ids[k])
	}
	neMVTIDsLRU.put(key, raw)
	return ids
}

func neParseCellHex(s string) (uint64, error) {
	v, err := strconv.ParseUint(s, 16, 64)
	return v, err
}

// neMVTBinLonLat — cell centre from the header polynomial (no cgo).
func neMVTBinLonLat(b *neFidxBin, k int) (float64, float64) {
	x, y := b.pos(k)
	fr := &neGlyphFrame
	lon := ((x-neGlyphPad)/fr.scale + fr.minx) / fr.kx
	lat := -((y-neGlyphPad)/fr.scale + fr.miny)
	return lon, lat
}

// ── attributes ───────────────────────────────────────────────────────────────

// neMVTCellAttrs — every catalogue field of cell k, decoded (res 12).
func neMVTCellAttrs(f *mvtFeature, b *neFidxBin, k int) {
	for i := range neFiltFields {
		fd := &neFiltFields[i]
		c := b.Cols[fd.ID]
		if c == nil {
			continue
		}
		v := neFiltRaw(fd, c, k)
		if v == fd.Nodata {
			continue
		}
		switch fd.Kind {
		case nfNum:
			if fd.ID == "cont" {
				f.i64("cont", int64(v&127))
				if v >= 128 {
					f.flag("cont_open", true)
				}
				continue
			}
			if (fd.ID == "loss" || fd.ID == "snar" || fd.ID == "snar2") && v == 0 {
				continue
			}
			f.num(fd.ID, neRound(float64(v)*fd.Scale+fd.Off, 2))
		case nfSet:
			if v >= 0 && v < len(fd.Members) && fd.Members[v] != "" {
				f.str(fd.ID, fd.Members[v])
			}
		case nfFlag:
			f.flag(fd.ID, v != 0)
		case nfBits:
			var parts []string
			for q, m := range fd.Members {
				if v&(1<<uint(q)) != 0 {
					parts = append(parts, m)
				}
			}
			if len(parts) > 0 {
				f.str(fd.ID, strings.Join(parts, ","))
			}
		case nfBucket:
			if v >= 0 && v < len(fd.Edges) {
				lo := fd.Edges[v]
				if v+1 < len(fd.Edges) {
					f.str(fd.ID, strconv.Itoa(lo)+"-"+strconv.Itoa(fd.Edges[v+1]))
				} else {
					f.str(fd.ID, strconv.Itoa(lo)+"+")
				}
				f.i64(fd.ID+"_min", int64(lo))
			}
		case nfSites:
			if v >= 0 && v < len(b.Hdr.N2K) {
				f.str(fd.ID, b.Hdr.N2K[v])
			}
		}
	}
}

// neMVTEpochs — the peer epochs a bin was built with.
func neMVTEpochs(f *mvtFeature, b *neFidxBin) {
	if m, ok := b.Hdr.Srtm.(map[string]interface{}); ok {
		if e, ok := m["epoch"].(string); ok && e != "" {
			f.str("srtm_epoch", e)
		}
	}
	if m, ok := b.Hdr.Sub.(map[string]interface{}); ok {
		if y, ok := m["year"].(float64); ok && y > 0 {
			f.i64("sub_year", int64(y))
		}
		if rel, ok := m["release"].(string); ok && rel != "" {
			f.str("sub_release", rel)
		}
	}
}

// neMVTAgg — the aggregate of a parent cell's res-12 children.
type neMVTAgg struct {
	n           int
	d, o        [10]int // 9 = unknown
	both, diff  int
	chg         [3]int
	sub         [8]int
	subKnown    int
	scov        float64
	bio, bioN   int
	n2k, n2kN   int
	el, elN     float64
	sl, slN     float64
	hb, ht, h   int // max raw
	bn          int
	bcov, bcovN float64
	can, canN   float64
	ndvi, ndviN float64
	loss        int
	ver         [8]int
	bin         *neFidxBin
}

func neMVTAggAdd(a *neMVTAgg, b *neFidxBin, k int) {
	if a.bin == nil {
		a.bin = b
	}
	a.n++
	get := func(id string) int {
		if c := b.Cols[id]; c != nil {
			return neFiltRaw(neFiltByID[id], c, k)
		}
		return neFiltByID[id].Nodata
	}
	d, o := get("d"), get("o")
	if d < 9 {
		a.d[d]++
	} else {
		a.d[9]++
	}
	if o < 9 {
		a.o[o]++
	} else {
		a.o[9]++
	}
	if d < 9 && o < 9 {
		a.both++
		if d != o {
			a.diff++
		}
	}
	if c := get("chg"); c >= 0 && c < 3 {
		a.chg[c]++
	}
	if s := get("sub"); s >= 0 && s < 8 {
		a.sub[s]++
		a.subKnown++
		if s > 0 {
			if sc := get("scov"); sc != 255 {
				a.scov += float64(sc) * 100 / 255
			}
		}
	}
	if v := get("bio"); v != 255 {
		a.bioN++
		a.bio += v
	}
	if v := get("n2k"); v != 255 {
		a.n2kN++
		a.n2k += v
	}
	if v := get("el"); v != 65535 {
		a.el += float64(v)
		a.elN++
	}
	if v := get("sl"); v != 255 {
		a.sl += float64(v)
		a.slN++
	}
	if v := get("hb"); v != 255 && v > a.hb {
		a.hb = v
	}
	if v := get("ht"); v != 255 && v > a.ht {
		a.ht = v
	}
	if v := get("h"); v != 255 && v > a.h {
		a.h = v
	}
	if v := get("bn"); v != 255 {
		a.bn += v
	}
	if v := get("bcov"); v != 255 {
		a.bcov += float64(v) * 100 / 255
		a.bcovN++
	}
	if v := get("canopy"); v != 255 {
		a.can += float64(v) * 100 / 254
		a.canN++
	}
	if v := get("ndvi"); v != 255 {
		a.ndvi += float64(v)*2/254 - 1
		a.ndviN++
	}
	if v := get("loss"); v != 255 && v != 0 {
		a.loss++
	}
	if v := get("ver"); v >= 0 && v < 7 {
		a.ver[v]++
	}
}

func neMVTMode(c []int, n int) (int, int) {
	best, bn := -1, 0
	for i := 0; i < n; i++ {
		if c[i] > bn {
			best, bn = i, c[i]
		}
	}
	return best, bn
}

func neMVTAggAttrs(f *mvtFeature, a *neMVTAgg, areaHa float64) {
	f.i64("n", int64(a.n))
	f.num("area_ha", neRound(areaHa, 2))
	pct := func(x, tot int) float64 { return neRound(float64(x)*100/float64(max(tot, 1)), 1) }
	if g, k := neMVTMode(a.d[:], 9); g >= 0 {
		f.str("d", neGlyphGroups[g])
		f.num("d_pct", pct(k, a.n))
	}
	f.num("d_known_pct", pct(a.n-a.d[9], a.n))
	for g := 0; g < 9; g++ {
		if a.d[g] > 0 {
			f.num("d_"+neGlyphGroups[g]+"_pct", pct(a.d[g], a.n))
		}
	}
	if g, k := neMVTMode(a.o[:], 9); g >= 0 {
		f.str("o", neGlyphGroups[g])
		f.num("o_pct", pct(k, a.n))
	}
	f.num("o_known_pct", pct(a.n-a.o[9], a.n))
	if a.both > 0 {
		f.num("diff_pct", pct(a.diff, a.both))
	}
	f.i64("chg_conf", int64(a.chg[2]))
	f.i64("chg_cand", int64(a.chg[1]))
	f.num("chg_pct", pct(a.chg[2], a.n))
	if a.subKnown > 0 {
		claimed := a.subKnown - a.sub[0]
		f.num("sub_pct", pct(claimed, a.subKnown))
		if claimed > 0 {
			if g, _ := neMVTMode(a.sub[1:], 7); g >= 0 {
				f.str("sub", neSubClass[g+1])
			}
			f.num("scov", neRound(a.scov/float64(claimed), 1))
		}
	}
	if a.bioN > 0 {
		f.num("bio_pct", pct(a.bio, a.bioN))
	}
	if a.n2kN > 0 {
		f.num("n2k_pct", pct(a.n2k, a.n2kN))
	}
	if a.elN > 0 {
		f.num("el", neRound(a.el/a.elN, 0))
	}
	if a.slN > 0 {
		f.num("sl", neRound(a.sl/a.slN, 1))
	}
	if a.hb > 0 {
		f.num("hb", float64(a.hb)*0.5)
	}
	if a.ht > 0 {
		f.num("ht", float64(a.ht)*0.5)
	}
	if a.h > 0 {
		f.num("h", float64(a.h)*0.5)
	}
	f.i64("bn", int64(a.bn))
	if a.bcovN > 0 {
		f.num("bcov", neRound(a.bcov/a.bcovN, 1))
	}
	if a.canN > 0 {
		f.num("canopy", neRound(a.can/a.canN, 1))
	}
	if a.ndviN > 0 {
		f.num("ndvi", neRound(a.ndvi/a.ndviN, 2))
	}
	if a.loss > 0 {
		f.i64("loss_n", int64(a.loss))
	}
	if g, k := neMVTMode(a.ver[:], 7); g >= 0 {
		f.str("ver", neObsVerdict[g])
		f.num("ver_pct", pct(k, a.n))
	}
	if a.bin != nil {
		neMVTEpochs(f, a.bin)
	}
}

// ── render ───────────────────────────────────────────────────────────────────

// neMVTProgress — readiness of a tile / area.
type neMVTProgress struct {
	Ready    int `json:"bins_ready"`
	Total    int `json:"bins_total"`
	Queued   int `json:"bins_queued"`
	Building int `json:"bins_building"`
	EtaS     int `json:"eta_s"`
}

// neMVTBinsStatus — look at every bin of the list: ready blobs + states, enqueue the rest.
func neMVTBinsStatus(bins [][2]int, enqueue bool) (blobs [][]byte, states []string, missing [][2]int, p neMVTProgress) {
	p.Total = len(bins)
	blobs = make([][]byte, len(bins))
	states = make([]string, len(bins))
	for q, k := range bins {
		b, st, ok := neFidxReady(k[0], k[1])
		states[q] = st
		if ok {
			blobs[q] = b
			p.Ready++
			continue
		}
		missing = append(missing, k)
	}
	for _, k := range missing {
		if enqueue {
			neFidxEnqueue(k[0], k[1])
		}
		switch pos := neFidxQueued(k[0], k[1]); {
		case pos == 0:
			p.Building++
		case pos > 0:
			p.Queued++
		}
	}
	if n := len(missing); n > 0 {
		per := 9.0
		if b, ms := neFidxBuilt.Load(), neFidxBuildMs.Load(); b > 3 {
			per = float64(ms) / float64(b) / 1000
		}
		p.EtaS = int(math.Ceil(float64(n) * per))
	}
	return
}

func neMVTGzip(raw []byte) []byte {
	if len(raw) == 0 {
		return nil
	}
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, 6)
	zw.Write(raw)
	zw.Close()
	return buf.Bytes()
}

// neMVTRenderNat — a res 7–9 tile from the national table (ne_mvt_nat.go):
// same layer, same aggregate attributes, peer columns absent.
func neMVTRenderNat(nat *neNat, z, x, y int, clip *[4]float64) []byte {
	t0 := time.Now()
	w, s, e, n := mvtTileBox(z, x, y)
	if clip != nil {
		w, s, e, n = math.Max(w, clip[0]), math.Max(s, clip[1]), math.Min(e, clip[2]), math.Min(n, clip[3])
		if !(w < e && s < n) {
			return nil
		}
	}
	res := neMVTZoomRes(z)
	l := nat.level(res)
	if l == nil {
		return nil
	}
	layer := newMVTLayer(neMVTLayerName)
	for _, q := range l.inBox(w, s, e, n) {
		p := l.IDs[q]
		f := layer.feature(p)
		f.str("h3", neCellHex(p))
		f.i64("res", int64(res))
		area, _ := h3.CellAreaM2(h3.Cell(p))
		neMVTAggAttrs(f, l.Rows[q].agg(), area/1e4)
		bd, err := h3.Cell(p).Boundary()
		if err != nil || len(bd) < 3 {
			continue
		}
		pts := make([][2]int32, 0, len(bd)+1)
		for _, b := range bd {
			px, py := mvtProject(z, x, y, b.Lng, b.Lat)
			pts = append(pts, [2]int32{int32(math.Round(px)), int32(math.Round(py))})
		}
		f.ring(pts, false)
		f.done()
	}
	gz := neMVTGzip(mvtEncode(layer))
	neMVTRendered.Add(1)
	neMVTRenderMs.Add(time.Since(t0).Milliseconds())
	return gz
}

// neMVTTileKey — LRU key / ETag input: z/x/y + every bin state (+ clip).
func neMVTTileKey(z, x, y int, states []string) string {
	h := sha256.New()
	fmt.Fprintf(h, "mvt1;%d/%d/%d;", z, x, y)
	for _, s := range states {
		h.Write([]byte(s))
		h.Write([]byte{';'})
	}
	return hex.EncodeToString(h.Sum(nil))[:20]
}

// neMVTTileBins — the bins a tile needs (box padded by one parent radius;
// an archive clips the box first).
func neMVTTileBins(z, x, y int, clip *[4]float64) [][2]int {
	w, s, e, n := mvtTileBox(z, x, y)
	if clip != nil {
		w, s, e, n = math.Max(w, clip[0]), math.Max(s, clip[1]), math.Min(e, clip[2]), math.Min(n, clip[3])
		if !(w < e && s < n) {
			return nil
		}
	}
	px, py := neMVTResPadDeg(neMVTZoomRes(z))
	if neMVTZoomRes(z) == 12 {
		px, py = 0.0004, 0.0003 // one cell: polygons of edge cells reach over the tile edge
	}
	return neMVTBins(w-px, s-py, e+px, n+py)
}

// neMVTRenderFromBlobs — the gzip'd tile from ready bins.
func neMVTRenderFromBlobs(z, x, y int, bins [][2]int, blobs [][]byte, states []string, clip *[4]float64) ([]byte, int) {
	t0 := time.Now()
	w, s, e, n := mvtTileBox(z, x, y)
	if clip != nil { // archive: only what lies in the requested box
		w, s, e, n = math.Max(w, clip[0]), math.Max(s, clip[1]), math.Min(e, clip[2]), math.Min(n, clip[3])
	}
	res := neMVTZoomRes(z)
	layer := newMVTLayer(neMVTLayerName)
	inTile := func(lon, lat float64) bool { return lon >= w && lon < e && lat >= s && lat < n }
	proj := func(f *mvtFeature, id uint64) {
		bd, err := h3.Cell(id).Boundary()
		if err != nil || len(bd) < 3 {
			return
		}
		pts := make([][2]int32, 0, len(bd)+1)
		for _, p := range bd {
			px, py := mvtProject(z, x, y, p.Lng, p.Lat)
			pts = append(pts, [2]int32{int32(math.Round(px)), int32(math.Round(py))})
		}
		f.ring(pts, false)
	}
	features := 0
	if res == 12 {
		for q := range bins {
			if blobs[q] == nil {
				continue
			}
			b, err := neFidxDecode(blobs[q])
			if err != nil {
				continue
			}
			b.Hdr.I, b.Hdr.J = bins[q][0], bins[q][1]
			ids := neMVTBinIDs(b, states[q])
			for k := 0; k < b.N; k++ {
				lon, lat := neMVTBinLonLat(b, k)
				if !inTile(lon, lat) || ids[k] == 0 {
					continue
				}
				f := layer.feature(ids[k])
				f.str("h3", neCellHex(ids[k]))
				f.i64("res", 12)
				neMVTCellAttrs(f, b, k)
				neMVTEpochs(f, b)
				proj(f, ids[k])
				f.done()
				features++
			}
		}
	} else {
		aggs := map[uint64]*neMVTAgg{}
		keep := map[uint64]bool{}
		for q := range bins {
			if blobs[q] == nil {
				continue
			}
			b, err := neFidxDecode(blobs[q])
			if err != nil {
				continue
			}
			b.Hdr.I, b.Hdr.J = bins[q][0], bins[q][1]
			ids := neMVTBinIDs(b, states[q])
			for k := 0; k < b.N; k++ {
				if ids[k] == 0 {
					continue
				}
				p := neH3ParentFast(ids[k], res)
				ok, seen := keep[p]
				if !seen {
					lon, lat := neCellCentre(p)
					ok = inTile(lon, lat)
					keep[p] = ok
				}
				if !ok {
					continue
				}
				a := aggs[p]
				if a == nil {
					a = &neMVTAgg{}
					aggs[p] = a
				}
				neMVTAggAdd(a, b, k)
			}
		}
		parents := make([]uint64, 0, len(aggs))
		for p := range aggs {
			parents = append(parents, p)
		}
		sortU64(parents)
		for _, p := range parents {
			a := aggs[p]
			f := layer.feature(p)
			f.str("h3", neCellHex(p))
			f.i64("res", int64(res))
			area, _ := h3.CellAreaM2(h3.Cell(p))
			neMVTAggAttrs(f, a, area/1e4)
			proj(f, p)
			f.done()
			features++
		}
	}
	gz := neMVTGzip(mvtEncode(layer))
	neMVTRendered.Add(1)
	neMVTRenderMs.Add(time.Since(t0).Milliseconds())
	return gz, features
}

// neMVTTile — the gzip'd tile, or progress when bins are missing.
// inline: build up to that many missing bins here (≤ neMVTWait for a live
// request; -1 = all of them, the archive worker's choice); the rest is queued.
func neMVTTile(z, x, y int, inline int, clip *[4]float64) (gz []byte, key string, p neMVTProgress, complete bool, err error) {
	if neMVTZoomRes(z) <= neNatRes {
		if nat := neNatGet(); nat != nil { // low zooms: the national table, never a bin
			key = fmt.Sprintf("nat/%s/%d/%d/%d", nat.State, z, x, y)
			if clip != nil {
				key += fmt.Sprintf("/clip%.5f,%.5f,%.5f,%.5f", clip[0], clip[1], clip[2], clip[3])
			}
			if b, ok := neMVTLRU.get(key); ok {
				return b, key, neMVTProgress{}, true, nil
			}
			gz = neMVTRenderNat(nat, z, x, y, clip)
			if gz == nil {
				gz = []byte{}
			}
			neMVTLRU.put(key, gz)
			return gz, key, neMVTProgress{}, true, nil
		}
	}
	bins := neMVTTileBins(z, x, y, clip)
	if len(bins) == 0 {
		return nil, "empty", neMVTProgress{}, true, nil
	}
	blobs, states, missing, prog := neMVTBinsStatus(bins, false)
	if len(missing) > 0 && (inline < 0 || len(missing) <= inline) {
		deadline := time.Now().Add(neMVTWait)
		for _, k := range missing {
			if inline >= 0 && time.Now().After(deadline) {
				break
			}
			neFidxGet(k[0], k[1])
			neMVTStateForget(k[0], k[1])
		}
		blobs, states, missing, prog = neMVTBinsStatus(bins, true)
	} else if len(missing) > 0 {
		blobs, states, missing, prog = neMVTBinsStatus(bins, true)
	}
	if len(missing) > 0 {
		return nil, "", prog, false, nil
	}
	key = neMVTTileKey(z, x, y, states)
	if clip != nil {
		key = neMVTTileKey(z, x, y, append(states, fmt.Sprintf("clip%.5f,%.5f,%.5f,%.5f", clip[0], clip[1], clip[2], clip[3])))
	}
	if b, ok := neMVTLRU.get(key); ok {
		return b, key, prog, true, nil
	}
	select {
	case neMVTSem <- struct{}{}:
	case <-time.After(neMVTSemWait):
		return nil, key, prog, true, errBusy
	}
	defer func() { <-neMVTSem }()
	if b, ok := neMVTLRU.get(key); ok { // rendered while we waited
		return b, key, prog, true, nil
	}
	if err := neGlyphLoadFrame(); err != nil {
		return nil, key, prog, true, err
	}
	gz, _ = neMVTRenderFromBlobs(z, x, y, bins, blobs, states, clip)
	if gz == nil {
		gz = []byte{}
	}
	neMVTLRU.put(key, gz)
	return gz, key, prog, true, nil
}

var errBusy = fmt.Errorf("busy")

// ── handlers ─────────────────────────────────────────────────────────────────

// handleNEMVT — /api/v1/ne/mvt/{z}/{x}/{y}.pbf, mvt/status, mvt.json.
func handleNEMVT(w http.ResponseWriter, r *http.Request, rest string) {
	switch rest {
	case "mvt.json":
		handleNEMVTJSON(w, r)
		return
	case "mvt/status":
		handleNEMVTStatus(w, r)
		return
	}
	p := strings.Split(strings.TrimSuffix(strings.TrimPrefix(rest, "mvt/"), ".pbf"), "/")
	if len(p) != 3 {
		apiError(w, "mvt/{z}/{x}/{y}.pbf", 404)
		return
	}
	z, e1 := strconv.Atoi(p[0])
	x, e2 := strconv.Atoi(p[1])
	y, e3 := strconv.Atoi(p[2])
	if e1 != nil || e2 != nil || e3 != nil || z < 0 || z > 22 || x < 0 || y < 0 || x >= 1<<uint(z) || y >= 1<<uint(z) {
		apiError(w, "mvt/{z}/{x}/{y}.pbf", 400)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	bw, bs, be, bn := mvtTileBox(z, x, y)
	if z < neMVTMinZ || z > neMVTMaxZ || be < neMVTAustria[0] || bw > neMVTAustria[2] || bn < neMVTAustria[1] || bs > neMVTAustria[3] {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	gateID, ok := neMVTGate(w, r, 1, bw, bs, be, bn)
	if !ok {
		return
	}
	gz, key, prog, complete, err := neMVTTile(z, x, y, neMVTWaitBins, nil)
	if err == errBusy {
		w.Header().Set("Retry-After", "3")
		w.Header().Set("Cache-Control", "no-store")
		apiError(w, "busy — retry in a few seconds", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		apiError(w, "tile unavailable", 503)
		return
	}
	if !complete {
		neMVTPending.Add(1)
		w.Header().Set("Retry-After", strconv.Itoa(max(5, min(prog.EtaS, 120))))
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusAccepted, map[string]interface{}{"status": "building", "tile": fmt.Sprintf("%d/%d/%d", z, x, y), "progress": prog,
			"hint": "the area is being prepared — reload the layer in a moment; /api/v1/ne/mvt/status?bbox= shows progress for a box"})
		return
	}
	etag := `"` + key + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=300, stale-while-revalidate=3600")
	w.Header().Add("Vary", "Accept-Encoding")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if len(gz) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.mapbox-vector-tile")
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(len(gz)))
		w.Write(gz)
		exportAccount(gateID, len(gz))
		return
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		apiError(w, "tile unavailable", 503)
		return
	}
	var raw bytes.Buffer
	raw.ReadFrom(zr)
	w.Header().Set("Content-Length", strconv.Itoa(raw.Len()))
	w.Write(raw.Bytes())
	exportAccount(gateID, raw.Len())
}

// neMVTParseBox — bbox=w,s,e,n inside Austria.
func neMVTParseBox(s string) ([4]float64, string) {
	p := strings.Split(s, ",")
	var b [4]float64
	if len(p) != 4 {
		return b, "bbox=w,s,e,n"
	}
	for k := range p {
		v, err := strconv.ParseFloat(strings.TrimSpace(p[k]), 64)
		if err != nil {
			return b, "bbox=w,s,e,n"
		}
		b[k] = v
	}
	if !(b[0] < b[2] && b[1] < b[3]) || b[2] < neMVTAustria[0] || b[0] > neMVTAustria[2] || b[3] < neMVTAustria[1] || b[1] > neMVTAustria[3] {
		return b, "box outside Austria or empty"
	}
	return b, ""
}

// handleNEMVTStatus — readiness of a box: how many bins are ready / queued, an ETA.
func handleNEMVTStatus(w http.ResponseWriter, r *http.Request) {
	bq := r.URL.Query().Get("bbox")
	if bq == "" || bq == "austria" { // the country: only the national table matters
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, map[string]interface{}{"status": map[bool]string{true: "ready", false: "building"}[neNatGet() != nil], "bbox": neMVTAustria,
			"national": NENatStats(), "hint": "zooms 9–11 come from the national table everywhere; give bbox= for the fine zooms of an area"})
		return
	}
	b, msg := neMVTParseBox(bq)
	if msg != "" {
		apiError(w, msg, 400)
		return
	}
	bins := neMVTBins(b[0], b[1], b[2], b[3])
	if len(bins) > 4000 {
		apiError(w, "box too large for a status call (≤ 4000 bins ≈ 1° × 2.5°)", 413)
		return
	}
	warm := r.URL.Query().Get("prepare") == "1"
	if warm {
		if _, ok := neMVTGate(w, r, 0, b[0], b[1], b[2], b[3]); !ok {
			return
		}
	}
	_, _, _, prog := neMVTBinsStatus(bins, warm)
	w.Header().Set("Cache-Control", "no-store")
	st := "ready"
	if prog.Ready < prog.Total {
		st = "building"
		if !warm && prog.Queued+prog.Building == 0 {
			st = "cold"
		}
	}
	out := map[string]interface{}{"status": st, "bbox": b, "progress": prog, "queue": neFidxQueueStats(), "national": NENatStats(),
		"hint": "progress is for zooms 12+ (bins); prepare=1 queues the missing bins; those tiles answer 202 until their bins exist; zooms 9–11 come from the national table"}
	if isPrivateTier(r) && r.URL.Query().Get("debug") == "1" && len(bins) <= 64 {
		var rows []string
		for _, k := range bins {
			_, st, ok := neFidxReady(k[0], k[1])
			bw, bs, be, bn := neFidxBox(k[0], k[1])
			kgs := KGCodesInBBoxMem(bw-neFidxHaloDeg, bs-neFidxHaloDeg, be+neFidxHaloDeg, bn+neFidxHaloDeg)
			var dg []string
			for _, kg := range kgs {
				if x := neStore.get(kg); x != nil {
					o := ""
					if l := x.Hdr.Layers["obs"]; l != nil {
						o = l.Digest[:8]
					}
					dg = append(dg, kg+"="+neDeclaredDigest(x, true)[:8]+"/"+o)
				}
			}
			rows = append(rows, fmt.Sprintf("%d/%d memo=%s live=%s ready=%v sub=%s srtm=%s kgs=%v", k[0], k[1], st, neFidxState(k[0], k[1]), ok, neSubReleaseSeen(), neFidxSrtmEpoch(), dg))
		}
		out["bins"] = rows
	}
	writeJSON(w, 200, out)
}

// handleNEMVTJSON — TileJSON 3.0 for the layer.
func handleNEMVTJSON(w http.ResponseWriter, r *http.Request) {
	base := publicBaseURL(r)
	fields := map[string]string{"h3": "String", "res": "Number", "n": "Number", "area_ha": "Number", "srtm_epoch": "String", "sub_year": "Number", "sub_release": "String"}
	for _, f := range neFiltFields {
		t := "Number"
		switch f.Kind {
		case nfSet, nfBits, nfBucket, nfSites:
			t = "String"
		case nfFlag:
			t = "Boolean"
		}
		fields[f.ID] = t
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	writeJSON(w, 200, map[string]interface{}{
		"tilejson": "3.0.0", "name": "umfeld-at cells", "scheme": "xyz", "format": "pbf",
		"description": "H3 cells of the declared/observed land-use product with change, subsidy and LiDAR statistics; res 7 at z 9 to res 12 at z 14+ (aggregates below res 12)",
		"tiles":       []string{base + "/api/v1/ne/mvt/{z}/{x}/{y}.pbf"},
		"minzoom":     neMVTMinZ, "maxzoom": neMVTMaxZ, "bounds": neMVTAustria,
		"attribution":   BEVNotice() + " · " + peers().ObsName + " · " + peers().SubName + " · © OpenStreetMap contributors · EEA Natura 2000",
		"vector_layers": []map[string]interface{}{{"id": neMVTLayerName, "minzoom": neMVTMinZ, "maxzoom": neMVTMaxZ, "fields": fields}},
		"docs":          base + "/api/v1/docs/qgis-layer.md",
	})
}

// neMVTVectorLayersMeta — the PMTiles metadata JSON (same field table).
func neMVTVectorLayersMeta(box [4]float64, minZ, maxZ int) []byte {
	fields := map[string]string{"h3": "String", "res": "Number", "n": "Number", "area_ha": "Number", "srtm_epoch": "String", "sub_year": "Number", "sub_release": "String"}
	for _, f := range neFiltFields {
		t := "Number"
		switch f.Kind {
		case nfSet, nfBits, nfBucket, nfSites:
			t = "String"
		case nfFlag:
			t = "Boolean"
		}
		fields[f.ID] = t
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b, _ := json.Marshal(map[string]interface{}{
		"name": "umfeld-at cells", "format": "pbf", "type": "overlay", "version": "1",
		"description":   fmt.Sprintf("umfeld-at H3 cell layer, bbox %.4f,%.4f,%.4f,%.4f, z %d–%d, built %s", box[0], box[1], box[2], box[3], minZ, maxZ, time.Now().UTC().Format(time.RFC3339)),
		"attribution":   BEVNotice() + " · " + peers().ObsName + " · " + peers().SubName + " · © OpenStreetMap contributors · EEA Natura 2000",
		"vector_layers": []map[string]interface{}{{"id": neMVTLayerName, "minzoom": minZ, "maxzoom": maxZ, "fields": fields}},
		"bounds":        fmt.Sprintf("%.5f,%.5f,%.5f,%.5f", box[0], box[1], box[2], box[3]),
	})
	return b
}
