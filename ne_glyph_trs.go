package main

// The trees as a cell sheet — `v=tree&x=p|l|d` (docs/ne-glyph.md §Trees at full resolution).
//
// With the full-resolution tiles on, the trees between the overview and the crowns (all of
// LOD 2) used to be hexagons of the peer's stand index, res 8–10: 49–2401 cells to a mark,
// beside a declared / observed picture and a filter veil that are cell-exact. This is the
// same sheet at the cells' own resolution: one res-12 hexagon per cell that holds a tree
// top, painted here into a tile that rides the page's tile path (hiTile / hiCull / ds=)
// like the veil — so zoom, hand-over and downscale behave as they do for every other layer,
// and plain and height colours cost the page the same (an <image>).
//
//	x=p   plain: the crown ramp's band of the cell's tallest tree (neGlyphTreeRGB)
//	x=l   heights, light page: neGlyphThRampL in 16 steps     (ground veil = the page's, client side)
//	x=d   heights, dark page:  neGlyphThRamp
//
// Source: the observed-layer peer's 0.1° blocks with the field set `tree_n,tree_h_max`
// (585 KB raw a block; the block gen is the manifest's, ne_glyph_fidx_blocks.go) — no filter
// index bin is built for a tree tile. A parsed block is kept as the wooded cells only
// (frame position, tallest, count) in a byte-bounded LRU (line ne_tree_sheet on
// debug/inflight). A block the peer does not answer in time is painted from the national
// res-9 table (ne_mvt_nat_trees.go) as a dither of the same tone and cover, served
// no-store with pend=1; the page asks again.
//
// The bitmap is opaque (palette); the page puts the sheet's weight on the group, so the
// rings two tiles share composite once. The ring (neGlyphFltBleed) is painted with the
// neighbour blocks' cells.

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/uber/h3-go/v4"
)

const (
	neTrsFields  = "tree_n,tree_h_max"
	neTrsMaxMB   = 96
	neTrsStale   = 10 * time.Minute // a block whose gen is not the manifest's is asked again after this
	neTrsSteps   = 16               // height colours (the page's TH_N)
	neTrsDitherD = 65.0             // tree tops per ha at which 1-1/e of the cells hold one (the page's TRO_D)
)

type neTrsBlock struct {
	gen    string
	at     time.Time
	ox, oy float64  // frame position of the block's north-west corner
	x, y   []uint16 // wooded cells only: frame units past (ox, oy), 1/neTrsQ
	ht     []uint8  // tallest apex, 0.5 m (255 unknown)
	tn     []uint8
	hex    [6][2]float64 // hexagon offsets in frame units
	cells  int           // all cells of the block
}

func (b *neTrsBlock) bytes() int { return 112 + 6*cap(b.x) }

const neTrsQ = 400.0 // position steps per frame unit (a block is < 160 units: fits 16 bits; 1/4 px at the deepest LOD)

var (
	neTrsMu      sync.Mutex
	neTrsLRU     = map[[2]int]*neTrsBlock{}
	neTrsOrder   [][2]int
	neTrsBytes   int
	neTrsFlight  = map[[2]int]chan struct{}{}
	neTrsFail    = map[[2]int]time.Time{} // not before
	neTrsSem     = make(chan struct{}, 6)
	neTrsFetches atomic.Int64
	neTrsHits    atomic.Int64
	neTrsPend    atomic.Int64
	neTrsTiles   atomic.Int64
	neTrsClient  = &http.Client{Timeout: 12 * time.Second}
	neTrsBase    = func() string { return peers().ObsAPI + "/api/v1/cells" } // var: tests point it at a stub
)

// NETrsStats — size line for debug/inflight.
func NETrsStats() map[string]interface{} {
	neTrsMu.Lock()
	defer neTrsMu.Unlock()
	return map[string]interface{}{"blocks": len(neTrsLRU), "bytes": neTrsBytes, "cap_bytes": neTrsMaxMB << 20, "in_flight": len(neTrsFlight),
		"parked": len(neTrsFail), "fetches": neTrsFetches.Load(), "hits": neTrsHits.Load(), "tiles_rendered": neTrsTiles.Load(), "tiles_pending": neTrsPend.Load()}
}

func neTrsInAustria(bi, bj int) bool {
	w, s := float64(bi)*0.1, float64(bj)*0.1
	return !(w+0.1 < 9.4 || w > 17.3 || s+0.1 < 46.3 || s > 49.1)
}

func neTrsFresh(b *neTrsBlock, bi, bj int) bool {
	g := neFidxBlockGenRaw(bi, bj)
	return g == "" && time.Since(b.at) < 15*time.Minute || g == b.gen || time.Since(b.at) < neTrsStale
}

func neTrsPut(k [2]int, b *neTrsBlock) {
	neTrsMu.Lock()
	defer neTrsMu.Unlock()
	if old := neTrsLRU[k]; old != nil {
		neTrsBytes -= old.bytes()
	} else {
		neTrsOrder = append(neTrsOrder, k)
	}
	neTrsLRU[k] = b
	neTrsBytes += b.bytes()
	for neTrsBytes > neTrsMaxMB<<20 && len(neTrsOrder) > 1 {
		o := neTrsOrder[0]
		neTrsOrder = neTrsOrder[1:]
		if v := neTrsLRU[o]; v != nil && o != k {
			neTrsBytes -= v.bytes()
			delete(neTrsLRU, o)
		} else if o == k {
			neTrsOrder = append(neTrsOrder, o)
		}
	}
}

// neTrsGet — the block's wooded cells; nil = not at hand in time (the caller paints the fallback).
func neTrsGet(bi, bj int, wait time.Duration) *neTrsBlock {
	k := [2]int{bi, bj}
	if !neTrsInAustria(bi, bj) {
		return &neTrsBlock{}
	}
	neTrsMu.Lock()
	if b := neTrsLRU[k]; b != nil && neTrsFresh(b, bi, bj) {
		neTrsMu.Unlock()
		neTrsHits.Add(1)
		return b
	}
	old := neTrsLRU[k]
	if time.Now().Before(neTrsFail[k]) {
		neTrsMu.Unlock()
		return old // the peer said later: the previous generation stands meanwhile
	}
	ch := neTrsFlight[k]
	if ch == nil {
		ch = make(chan struct{})
		neTrsFlight[k] = ch
		go func() {
			neTrsSem <- struct{}{}
			b, retry := neTrsFetch(bi, bj)
			<-neTrsSem
			if b != nil {
				neTrsPut(k, b)
			}
			neTrsMu.Lock()
			if b == nil {
				neTrsFail[k] = time.Now().Add(retry)
				if len(neTrsFail) > 4096 {
					for q, t := range neTrsFail {
						if time.Now().After(t) {
							delete(neTrsFail, q)
						}
					}
				}
			} else {
				delete(neTrsFail, k)
			}
			delete(neTrsFlight, k)
			neTrsMu.Unlock()
			close(ch)
		}()
	}
	neTrsMu.Unlock()
	select {
	case <-ch:
	case <-time.After(wait):
		return old
	}
	neTrsMu.Lock()
	defer neTrsMu.Unlock()
	if b := neTrsLRU[k]; b != nil {
		return b
	}
	return nil
}

// neTrsFetch — one block from the peer. (nil, retry) when it has to be asked again.
func neTrsFetch(bi, bj int) (*neTrsBlock, time.Duration) {
	w, s := float64(bi)*0.1, float64(bj)*0.1
	url := fmt.Sprintf("%s?bbox=%.3f,%.3f,%.3f,%.3f&format=bin&block=1&fields=%s", neTrsBase(), w, s, w+0.1, s+0.1, neTrsFields)
	for attempt := 0; attempt < 2; attempt++ {
		if attempt == 1 {
			url += "&ids=1"
		}
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("User-Agent", "umfeld-at/tree-sheet (+"+publicURL()+")")
		resp, err := neTrsClient.Do(req)
		if err != nil {
			return nil, 20 * time.Second
		}
		neTrsFetches.Add(1)
		gen, ep := resp.Header.Get("X-Cells-Gen"), resp.Header.Get("X-Cells-Epoch")
		switch resp.StatusCode {
		case 200:
		case 404:
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if strings.Contains(strings.ToLower(string(body)), "block") {
				return nil, 10 * time.Minute // no block mode at the peer
			}
			return &neTrsBlock{gen: neFidxBlockGenRaw(bi, bj), at: time.Now()}, 0 // no cell in the block
		case 503, 429:
			resp.Body.Close()
			ra := neFidxRetryAfter(resp.Header.Get("Retry-After"), false)
			if ra > 2*time.Minute {
				ra = 2 * time.Minute
			}
			return nil, ra
		default:
			resp.Body.Close()
			return nil, time.Minute
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if err != nil || len(body) < 8 || string(body[:4]) != "NEXB" {
			return nil, time.Minute
		}
		bins, _, ok := neFidxBlockParse(body, ep, bi, bj)
		if !ok {
			continue
		}
		return neTrsBuild(bins, gen, w, s), 0
	}
	return nil, 5 * time.Minute
}

func neTrsBuild(bins map[string]*neFidxSrtm, gen string, w, s float64) *neTrsBlock {
	b := &neTrsBlock{gen: gen, at: time.Now()}
	b.ox, b.oy = neGlyphProj(w, s+0.1)
	b.ox, b.oy = b.ox-1, b.oy-1
	nw := 0
	for _, v := range bins {
		for _, t := range v.cols["tree_n"] {
			if t != 0 {
				nw++
			}
		}
	}
	b.x, b.y, b.ht, b.tn = make([]uint16, 0, nw), make([]uint16, 0, nw), make([]uint8, 0, nw), make([]uint8, 0, nw)
	q := func(v float64) uint16 { return uint16(math.Max(0, math.Min(65535, math.Round(v*neTrsQ)))) }
	for _, v := range bins {
		b.cells += len(v.ids)
		tn, ht := v.cols["tree_n"], v.cols["tree_h_max"]
		if len(tn) < len(v.ids) || len(ht) < len(v.ids) {
			continue
		}
		for k, id := range v.ids {
			if tn[k] == 0 {
				continue
			}
			lon, lat := neCellCentre(id)
			fx, fy := neGlyphProj(lon, lat)
			b.x = append(b.x, q(fx-b.ox))
			b.y = append(b.y, q(fy-b.oy))
			b.ht = append(b.ht, ht[k])
			b.tn = append(b.tn, tn[k])
		}
	}
	// one hexagon for the block: res-12 cells keep their shape and turn over 0.1°
	if c, err := h3.LatLngToCell(h3.NewLatLng(s+0.05, w+0.05), neCellRes); err == nil {
		if bd, err := c.Boundary(); err == nil && len(bd) == 6 {
			ll, _ := c.LatLng()
			cx, cy := neGlyphProj(ll.Lng, ll.Lat)
			for v := 0; v < 6; v++ {
				vx, vy := neGlyphProj(bd[v].Lng, bd[v].Lat)
				b.hex[v] = [2]float64{vx - cx, vy - cy}
			}
		}
	}
	return b
}

// neTrsPalette — 0 transparent, then the inks.
func neTrsPalette(mode string) color.Palette {
	p := color.Palette{color.NRGBA{0, 0, 0, 0}}
	if mode == "p" {
		for _, c := range neGlyphTreeRGB {
			p = append(p, color.NRGBA{c[0], c[1], c[2], 255})
		}
		return p
	}
	ramp := &neGlyphThRampL
	if mode == "d" {
		ramp = &neGlyphThRamp
	}
	for i := 0; i < neTrsSteps; i++ {
		c := neGlyphThRampAt(ramp, neGlyphThMinM+(neGlyphThMaxM-neGlyphThMinM)*float64(i)/float64(neTrsSteps-1))
		p = append(p, color.NRGBA{c[0], c[1], c[2], 255})
	}
	return p
}

// neTrsInk — palette index of a height (raw 0.5 m).
func neTrsInk(mode string, h int) uint8 {
	if mode == "p" {
		return uint8(1 + neGlyphTreeBand(h))
	}
	i := int(math.Round((float64(h)/2 - neGlyphThMinM) / (neGlyphThMaxM - neGlyphThMinM) * float64(neTrsSteps-1)))
	if i < 0 {
		i = 0
	}
	if i >= neTrsSteps {
		i = neTrsSteps - 1
	}
	return uint8(1 + i)
}

type neTrsMeta struct {
	n, cells, pend int
	trees          int64
	tmax           int
	th             [5]int
}

func (m *neTrsMeta) String(ms int64) string {
	th := make([]string, 5)
	for b := range th {
		th[b] = strconv.Itoa(m.th[b])
	}
	return fmt.Sprintf("n=%d;ms=%d;obs=%d;trees=%d;tmax=%d;th=%s;pend=%d", m.n, ms, m.n, m.trees, m.tmax, strings.Join(th, ","), m.pend)
}

// neTrsDither — a stable 0..1 threshold per GLOBAL pixel (two tiles' rings agree).
func neTrsDither(x, y int) float64 {
	h := uint32(x)*0x9E3779B1 ^ uint32(y)*0x85EBCA77
	h ^= h >> 15
	h *= 0x2C1B3C6D
	h ^= h >> 12
	return float64(h&0xFFFF) / 65536
}

// neTrsRender — the tile. Blocks not at hand are painted from the national res-9 table.
func neTrsRender(g *neGlyphTileGeom, w, s, e, n float64, mode string, wait time.Duration, m *neTrsMeta) (*image.Paletted, []string) {
	img := image.NewPaletted(image.Rect(0, 0, g.pw, g.ph), neTrsPalette(mode))
	px := g.lod.px
	mg := 0.15 // frame units: one hexagon beyond the picture
	x0, y0 := g.x0-mg, g.y0-mg
	x1, y1 := g.x0+float64(g.pw)/px+mg, g.y0+float64(g.ph)/px+mg
	cx0, cy0 := neGlyphProj(w, n)
	cx1, cy1 := neGlyphProj(e, s)
	type ent struct {
		bi, bj int
		b      *neTrsBlock
	}
	var ents []ent
	for _, k := range neTrsBlocksOf(g.lod, w, s, e, n) {
		ents = append(ents, ent{bi: k[0], bj: k[1]})
	}
	var wg sync.WaitGroup
	for k := range ents {
		wg.Add(1)
		go func(q *ent) { defer wg.Done(); q.b = neTrsGet(q.bi, q.bj, wait) }(&ents[k])
	}
	wg.Wait()
	gens := make([]string, 0, len(ents))
	const pad = 0.05
	var poly [6][2]float64
	for _, q := range ents {
		b := q.b
		if b == nil {
			m.pend++
			gens = append(gens, "?")
			neTrsPaintNat(img, g, q.bi, q.bj, mode, x0, y0, x1, y1, cx0, cy0, cx1, cy1, m)
			continue
		}
		gens = append(gens, b.gen)
		var hx, hy [6]float64
		for v := 0; v < 6; v++ {
			hx[v], hy[v] = b.hex[v][0]*px, b.hex[v][1]*px
			hx[v] += math.Copysign(pad, hx[v])
			hy[v] += math.Copysign(pad, hy[v])
		}
		// the block's own share of the cell count: its box ∩ the tile box
		if bw, bs := float64(q.bi)*0.1, float64(q.bj)*0.1; bw+1e-9 >= w && bw+0.1 <= e+1e-9 && bs+1e-9 >= s && bs+0.1 <= n+1e-9 {
			m.cells += b.cells
		}
		for k := range b.x {
			fx, fy := b.ox+float64(b.x[k])/neTrsQ, b.oy+float64(b.y[k])/neTrsQ
			if fx < x0 || fx > x1 || fy < y0 || fy > y1 {
				continue
			}
			h := int(b.ht[k])
			if h == 255 {
				h = 6
			}
			if fx >= cx0 && fx < cx1 && fy >= cy0 && fy < cy1 {
				m.n++
				m.trees += int64(b.tn[k])
				if h > m.tmax {
					m.tmax = h
				}
				m.th[neGlyphTreeBand(h)]++
			}
			cx, cy := (fx-g.x0)*px, (fy-g.y0)*px
			for v := 0; v < 6; v++ {
				poly[v] = [2]float64{cx + hx[v], cy + hy[v]}
			}
			neFillPoly(img, poly[:], neTrsInk(mode, h))
		}
	}
	return img, gens
}

// neTrsPaintNat — a block without its cells yet: every res-9 hexagon of the national table whose centre lies in
// the block, in the tone of its trees (plain: mean + 3.5 m; heights: the top height, q = .9), dithered to the
// share of its cells that would hold a tree. Counts join the meta so the page has a figure meanwhile.
func neTrsPaintNat(img *image.Paletted, g *neGlyphTileGeom, bi, bj int, mode string, x0, y0, x1, y1, cx0, cy0, cx1, cy1 float64, m *neTrsMeta) {
	nt := neNatTreesGet()
	if nt == nil {
		return
	}
	w, s := float64(bi)*0.1, float64(bj)*0.1
	const o = 0.004 // a res-9 hexagon whose centre is inside may have been dropped by the polygon's centre rule otherwise
	cs, err := h3.PolygonToCells(h3.GeoPolygon{GeoLoop: h3.GeoLoop{h3.NewLatLng(s-o, w-o), h3.NewLatLng(s+0.1+o, w-o), h3.NewLatLng(s+0.1+o, w+0.1+o), h3.NewLatLng(s-o, w+0.1+o)}}, 9)
	if err != nil {
		return
	}
	px := g.lod.px
	gx, gy := int(math.Round(g.x0*px)), int(math.Round(g.y0*px))
	bd := img.Bounds()
	for _, c := range cs {
		ll, err := c.LatLng()
		if err != nil || ll.Lng < w || ll.Lng >= w+0.1 || ll.Lat < s || ll.Lat >= s+0.1 {
			continue
		}
		var a neTreeAgg
		nt.fill(&a, 9, uint64(c))
		if a.n <= 0 {
			continue
		}
		fx, fy := neGlyphProj(ll.Lng, ll.Lat)
		if fx < x0-1 || fx > x1+1 || fy < y0-1 || fy > y1+1 {
			continue
		}
		mean := float64(a.sum) / float64(a.n) / 4 // m
		if fx >= cx0 && fx < cx1 && fy >= cy0 && fy < cy1 {
			m.trees += a.n
			if a.top/2 > m.tmax {
				m.tmax = a.top / 2
			}
		}
		hm := mean + 3.5
		if mode != "p" {
			hm = neStandsQ(a.bands[:], float64(a.top)/4, .9)
		}
		ink := neTrsInk(mode, int(hm*2))
		cov := 1 - math.Exp(-float64(a.n)/10.53/neTrsDitherD) // a res-9 hexagon is ~10.5 ha
		bnd, err := c.Boundary()
		if err != nil || len(bnd) < 6 {
			continue
		}
		var p [6][2]float64
		miny, maxy := math.Inf(1), math.Inf(-1)
		for v := 0; v < 6; v++ {
			vx, vy := neGlyphProj(bnd[v].Lng, bnd[v].Lat)
			p[v] = [2]float64{(vx - g.x0) * px, (vy - g.y0) * px}
			miny, maxy = math.Min(miny, p[v][1]), math.Max(maxy, p[v][1])
		}
		ya, yb := max(int(math.Ceil(miny-0.5)), bd.Min.Y), min(int(math.Floor(maxy-0.5)), bd.Max.Y-1)
		for y := ya; y <= yb; y++ {
			sy := float64(y) + 0.5
			xa, xb := math.Inf(1), math.Inf(-1)
			for i := 0; i < 6; i++ {
				u, v := p[i], p[(i+1)%6]
				if (u[1] <= sy) == (v[1] <= sy) {
					continue
				}
				x := u[0] + (sy-u[1])*(v[0]-u[0])/(v[1]-u[1])
				xa, xb = math.Min(xa, x), math.Max(xb, x)
			}
			if xa > xb {
				continue
			}
			ia, ib := max(int(math.Ceil(xa-0.5)), bd.Min.X), min(int(math.Floor(xb-0.5)), bd.Max.X-1)
			row := img.Pix[y*img.Stride:]
			for x := ia; x <= ib; x++ {
				if neTrsDither(gx+x, gy+y) < cov {
					row[x] = ink
				}
			}
		}
	}
}

// neTrsKey — the tile's cache key from the manifest's block gens ("" when a block has none: not kept).
func neTrsKey(L neGlyphLOD, lod, i, j int, mode string, ds int) (string, bool) {
	w, s, e, n := neGlyphTileBox(L, i, j)
	key := fmt.Sprintf("tree/x4%s/%d/%d/%d/ds%d", mode, lod, i, j, ds)
	ok := true
	for _, k := range neTrsBlocksOf(L, w, s, e, n) {
		g := "-"
		if neTrsInAustria(k[0], k[1]) {
			if g = neFidxBlockGenRaw(k[0], k[1]); g == "" && !neTrsManifest() {
				ok = false // no manifest yet: nothing says which generation this is
			}
		}
		key += "/" + g
	}
	return key, ok
}

func neTrsManifest() bool {
	neFidxBlocksLoad()
	neFidxBlocksMu.RLock()
	defer neFidxBlocksMu.RUnlock()
	return len(neFidxBlocksGen) > 0
}

// neTrsServe — handleNEGlyphTile's branch for v=tree&x=.
func neTrsServe(w http.ResponseWriter, r *http.Request, L neGlyphLOD, lod, i, j int, mode string, ds int) {
	key, keyed := neTrsKey(L, lod, i, j, mode, ds)
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=600")
	w.Header().Set("X-NE-Notice", NENotice())
	etag := `"t` + neGlyphTileHash(key) + `"`
	if inm := r.Header.Get("If-None-Match"); inm != "" && keyed && etagMatches(inm, etag) {
		atomic.AddInt64(&neGlyphTile304, 1)
		w.Header().Set("ETag", etag)
		w.Header().Del("Content-Type")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	send := func(b []byte) {
		meta, pngb := neGlyphTileSplit(b)
		w.Header().Set("ETag", etag)
		w.Header().Set("X-NE-Tile", meta)
		w.Header().Set("Content-Length", strconv.Itoa(len(pngb)))
		_, _ = w.Write(pngb)
	}
	if keyed {
		if b, ok := neGlyphTileGet(key); ok {
			send(b)
			return
		}
	}
	g, tw, ts, te, tn, ok := neGlyphTileGeomBleed(L, i, j, neGlyphTilePicBleed)
	if !ok {
		apiError(w, "tile geometry", 500)
		return
	}
	// the blocks are fetched before the gate: the wait is the wire, not a core
	t0 := time.Now()
	var m neTrsMeta
	wait := 6 * time.Second
	if r.URL.Query().Get("late") == "1" {
		wait = 2500 * time.Millisecond // a repoll: the fetch has been running since the first ask
	}
	type res struct {
		png  []byte
		gens []string
	}
	done := make(chan res, 1)
	go func() {
		// fetch first (no gate), then paint under the gate
		neTrsWarm(&g, tw, ts, te, tn, wait)
		select {
		case neGlyphRenderGate <- struct{}{}:
		case <-r.Context().Done():
			done <- res{}
			return
		}
		defer func() { <-neGlyphRenderGate }()
		p, gens := neTrsRender(&g, tw, ts, te, tn, mode, 0, &m)
		var buf bytes.Buffer
		enc := png.Encoder{CompressionLevel: png.DefaultCompression}
		if enc.Encode(&buf, p) != nil {
			done <- res{}
			return
		}
		done <- res{buf.Bytes(), gens}
	}()
	out := <-done
	if r.Context().Err() != nil {
		return
	}
	if out.png == nil {
		apiError(w, "tile render failed", 500)
		return
	}
	pngb := out.png
	if ds > 1 {
		pngb = neTrsPosterise(neGlyphTileDownsample(pngb, ds))
	}
	neTrsTiles.Add(1)
	meta := m.String(time.Since(t0).Milliseconds())
	// kept only when every block painted is the manifest's generation
	keep := keyed && m.pend == 0
	if keep {
		k2, _ := neTrsKey(L, lod, i, j, mode, ds)
		keep = k2 == key && neTrsGensMatch(L, i, j, out.gens)
	}
	if keep {
		w.Header().Set("ETag", etag)
		neGlyphTilePut(key, neGlyphTileJoin(meta, pngb))
	} else if m.pend > 0 {
		neTrsPend.Add(1)
		w.Header().Set("Cache-Control", "no-store")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=120")
	}
	w.Header().Set("X-NE-Tile", meta)
	w.Header().Set("Content-Length", strconv.Itoa(len(pngb)))
	_, _ = w.Write(pngb)
}

// neTrsBlocksOf — the blocks a tile reads: its own and the ones its ring reaches into across an EDGE. The ring's
// corners lie under the diagonal neighbour's own picture, so those blocks are not fetched for a few pixels.
func neTrsBlocksOf(L neGlyphLOD, w, s, e, n float64) (out [][2]int) {
	ring := (neGlyphTilePicBleed/L.px + 0.15) * 0.00115
	fl := func(v float64, up bool) int {
		if up {
			return int(math.Floor(v/0.1 - 1e-9))
		}
		return int(math.Floor(v/0.1 + 1e-9))
	}
	ci0, ci1, cj0, cj1 := fl(w, false), fl(e, true), fl(s, false), fl(n, true)
	for bi := fl(w-ring, false); bi <= fl(e+ring, true); bi++ {
		for bj := fl(s-ring, false); bj <= fl(n+ring, true); bj++ {
			if (bi < ci0 || bi > ci1) && (bj < cj0 || bj > cj1) {
				continue
			}
			out = append(out, [2]int{bi, bj})
		}
	}
	return out
}

func neTrsWarm(g *neGlyphTileGeom, w, s, e, n float64, wait time.Duration) {
	var wg sync.WaitGroup
	for _, k := range neTrsBlocksOf(g.lod, w, s, e, n) {
		wg.Add(1)
		go func(k [2]int) { defer wg.Done(); neTrsGet(k[0], k[1], wait) }(k)
	}
	wg.Wait()
}

func neTrsGensMatch(L neGlyphLOD, i, j int, gens []string) bool {
	w, s, e, n := neGlyphTileBox(L, i, j)
	bl := neTrsBlocksOf(L, w, s, e, n)
	if len(bl) != len(gens) {
		return false
	}
	for q, k := range bl {
		if neTrsInAustria(k[0], k[1]) && gens[q] != neFidxBlockGenRaw(k[0], k[1]) {
			return false
		}
	}
	return true
}

// neTrsPosterise — a downscaled sheet is a speckle of averaged colours and covers, which deflate badly (0.67 MB at
// ds=2). Colour to 4 bits a channel and cover to 8 steps: below what a screen pixel of a fine speckle shows, a
// third of the bytes.
func neTrsPosterise(pngb []byte) []byte {
	img, err := png.Decode(bytes.NewReader(pngb))
	if err != nil {
		return pngb
	}
	p, ok := img.(*image.NRGBA)
	if !ok {
		return pngb
	}
	for o := 0; o+3 < len(p.Pix); o += 4 {
		a := p.Pix[o+3]
		if a == 0 {
			p.Pix[o], p.Pix[o+1], p.Pix[o+2] = 0, 0, 0
			continue
		}
		q := (int(a)*8 + 127) / 255
		if q < 1 {
			q = 1
		}
		p.Pix[o+3] = uint8(q * 255 / 8)
		for k := 0; k < 3; k++ {
			p.Pix[o+k] = p.Pix[o+k]&0xF0 | p.Pix[o+k]>>4
		}
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.DefaultCompression}
	if enc.Encode(&buf, p) != nil {
		return pngb
	}
	return buf.Bytes()
}

// neTrsPic — the same sheet on any frame box (the QGIS picture tiles mvt/pic/trees|treesh, ne_mvt_layers.go):
// plain or the light page's height colours. pend = a block was not at hand (the caller does not keep the tile).
func neTrsPic(g *neGlyphTileGeom, w, s, e, n float64, heights bool) (img *image.Paletted, pend bool) {
	mode := "p"
	if heights {
		mode = "l"
	}
	var m neTrsMeta
	img, _ = neTrsRender(g, w, s, e, n, mode, 0, &m)
	return img, m.pend > 0
}
