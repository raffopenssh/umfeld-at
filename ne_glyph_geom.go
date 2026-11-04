// ne_glyph_geom.go — the cell geometry behind a full-res tile, computed once
// per tile and shared by every view that paints every cell (base, obs, tree,
// dif) and by concurrent requests for the same tile.
//
// Profiling a cold LOD-2 tile (~280 k res-12 cells) put ~0.5 s of its ~0.8 s
// into H3 cgo: polygonToCells (~0.2 s) and cellToBoundary per cell (~0.3 s),
// data-independent work that the page then asked for three times (base + obs
// + the filter veil of the same tile). Here the cell list and the pixel centre
// of every cell are computed once (one cgo LatLng per cell, ~0.1 s) and the
// hexagon outline is the centre plus six offsets interpolated bilinearly from
// the true boundaries at the tile's four corners — within one tile (≤ 0.1°)
// the H3 hexagon rotates and scales by far less than a tile pixel. Cells the
// cache does not know (the subsidy painter's own ids) take the exact cgo path.
//
// Bound: neGlyphGeomLRU, 192 MB (~40 LOD-2 tiles, a 5×5 ring around a full-res viewport; 16 bytes per cell). Size line:
// `ne_glyph_geom` on the private inflight endpoint. Singleflight per tile.
package main

import (
	"container/list"
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/uber/h3-go/v4"
)

const neGlyphGeomPad = 0.0005 // cells whose centre sits just across the edge still paint their half of the seam

type neGlyphTileCells struct {
	ids  []uint64         // sorted, the tile box + neGlyphGeomPad
	c    []float32        // 2 per cell: centre in frame units relative to the tile's unbleeded top-left (x0, y0)
	off  [4][6][2]float32 // hexagon vertex offsets (frame units) at NW, NE, SW, SE, sorted by angle
	x0   float64          // frame top-left of the tile box (no bleed)
	y0   float64
	fw   float64 // frame width/height of the tile box
	fh   float64
	size int64
}

var (
	neGlyphGeomMu                                      sync.Mutex
	neGlyphGeomLL                                      = list.New()
	neGlyphGeomItems                                   = map[string]*list.Element{}
	neGlyphGeomSize                                    int64
	neGlyphGeomMax                                     int64 = 192 << 20
	neGlyphGeomFly                                           = map[string]chan struct{}{}
	neGlyphGeomHits, neGlyphGeomMiss, neGlyphGeomBuilt int64
)

type neGlyphGeomItem struct {
	key string
	v   *neGlyphTileCells
}

// neGlyphTileCellsGet — the tile's cells, from the LRU or built once.
func neGlyphTileCellsGet(lod neGlyphLOD, i, j int) *neGlyphTileCells {
	key := fmt.Sprintf("%d/%d/%d", lodIndex(lod), i, j)
	for {
		neGlyphGeomMu.Lock()
		if e, ok := neGlyphGeomItems[key]; ok {
			neGlyphGeomLL.MoveToFront(e)
			neGlyphGeomHits++
			v := e.Value.(*neGlyphGeomItem).v
			neGlyphGeomMu.Unlock()
			return v
		}
		if ch, ok := neGlyphGeomFly[key]; ok {
			neGlyphGeomMu.Unlock()
			<-ch
			continue
		}
		ch := make(chan struct{})
		neGlyphGeomFly[key] = ch
		neGlyphGeomMiss++
		neGlyphGeomMu.Unlock()
		v := neGlyphTileCellsBuild(lod, i, j)
		neGlyphGeomMu.Lock()
		delete(neGlyphGeomFly, key)
		close(ch)
		if v != nil && v.size <= neGlyphGeomMax/4 {
			neGlyphGeomBuilt++
			neGlyphGeomItems[key] = neGlyphGeomLL.PushFront(&neGlyphGeomItem{key, v})
			neGlyphGeomSize += v.size
			for neGlyphGeomSize > neGlyphGeomMax && neGlyphGeomLL.Len() > 1 {
				e := neGlyphGeomLL.Back()
				it := e.Value.(*neGlyphGeomItem)
				neGlyphGeomSize -= it.v.size
				delete(neGlyphGeomItems, it.key)
				neGlyphGeomLL.Remove(e)
			}
		}
		neGlyphGeomMu.Unlock()
		return v
	}
}

func NEGlyphGeomStats() map[string]interface{} {
	neGlyphGeomMu.Lock()
	defer neGlyphGeomMu.Unlock()
	return map[string]interface{}{"entries": neGlyphGeomLL.Len(), "bytes": neGlyphGeomSize, "max_bytes": neGlyphGeomMax,
		"hits": neGlyphGeomHits, "misses": neGlyphGeomMiss, "built": neGlyphGeomBuilt, "inflight": len(neGlyphGeomFly),
		"prewarm": map[string]interface{}{"queued": neGlyphGeomPreQLen(), "built": atomic.LoadInt64(&neGlyphGeomPreDone), "skipped": atomic.LoadInt64(&neGlyphGeomPreSkip)}}
}

// neGlyphHexOffsets — the six vertex offsets of the cell under (lon, lat), in
// frame units, sorted by angle so that the four corners interpolate vertex by vertex.
func neGlyphHexOffsets(lon, lat float64) ([6][2]float32, bool) {
	var out [6][2]float32
	c, err := h3.LatLngToCell(h3.NewLatLng(lat, lon), neCellRes)
	if err != nil {
		return out, false
	}
	bd, err := c.Boundary()
	if err != nil || len(bd) != 6 {
		return out, false
	}
	ll, _ := c.LatLng()
	cx, cy := neGlyphProj(ll.Lng, ll.Lat)
	type va struct {
		a    float64
		x, y float64
	}
	vs := make([]va, 6)
	for k, p := range bd {
		x, y := neGlyphProj(p.Lng, p.Lat)
		vs[k] = va{math.Atan2(y-cy, x-cx), x - cx, y - cy}
	}
	sort.Slice(vs, func(a, b int) bool { return vs[a].a < vs[b].a })
	for k := range vs {
		out[k] = [2]float32{float32(vs[k].x), float32(vs[k].y)}
	}
	return out, true
}

func neGlyphTileCellsBuild(lod neGlyphLOD, i, j int) *neGlyphTileCells {
	w, s, e, n := neGlyphTileBox(lod, i, j)
	t := &neGlyphTileCells{}
	t.x0, t.y0 = neGlyphProj(w, n)
	x1, y1 := neGlyphProj(e, s)
	t.fw, t.fh = x1-t.x0, y1-t.y0
	if t.fw <= 0 || t.fh <= 0 {
		return nil
	}
	corners := [4][2]float64{{w, n}, {e, n}, {w, s}, {e, s}}
	for k, c := range corners {
		off, ok := neGlyphHexOffsets(c[0], c[1])
		if !ok {
			return nil
		}
		t.off[k] = off
	}
	ids, cs := neBoxCellsCentres(w-neGlyphGeomPad, s-neGlyphGeomPad, e+neGlyphGeomPad, n+neGlyphGeomPad)
	t.ids = ids
	t.c = make([]float32, 2*len(ids))
	for k := range ids {
		x, y := neGlyphProj(cs[2*k], cs[2*k+1])
		t.c[2*k], t.c[2*k+1] = float32(x-t.x0), float32(y-t.y0)
	}
	t.size = int64(16*len(t.ids)) + 512
	return t
}

// poly — ordinal k's hexagon in the geom's pixel frame; false for a cell H3 could not place.
func (t *neGlyphTileCells) poly(g *neGlyphTileGeom, k int, poly *[8][2]float64) (np int, cx, cy float64, ok bool) {
	fx, fy := float64(t.c[2*k]), float64(t.c[2*k+1])
	if math.IsNaN(fx) {
		return 0, 0, 0, false
	}
	u, v := fx/t.fw, fy/t.fh
	u, v = math.Min(math.Max(u, 0), 1), math.Min(math.Max(v, 0), 1)
	px := (fx + t.x0 - g.x0) * g.lod.px
	py := (fy + t.y0 - g.y0) * g.lod.px
	w00, w10, w01, w11 := (1-u)*(1-v), u*(1-v), (1-u)*v, u*v
	for q := 0; q < 6; q++ {
		ox := w00*float64(t.off[0][q][0]) + w10*float64(t.off[1][q][0]) + w01*float64(t.off[2][q][0]) + w11*float64(t.off[3][q][0])
		oy := w00*float64(t.off[0][q][1]) + w10*float64(t.off[1][q][1]) + w01*float64(t.off[2][q][1]) + w11*float64(t.off[3][q][1])
		poly[q] = [2]float64{px + ox*g.lod.px, py + oy*g.lod.px}
	}
	return 6, px, py, true
}

// find — ordinal of id in the sorted cell list, -1 when absent.
func (t *neGlyphTileCells) find(id uint64) int {
	k := sort.Search(len(t.ids), func(q int) bool { return t.ids[q] >= id })
	if k < len(t.ids) && t.ids[k] == id {
		return k
	}
	return -1
}

// neBoxCellsCentres — the same set as neBBoxCells (res-12 cells whose centre
// lies in the box), sorted, with the centres (lon, lat pairs) alongside; the
// cells are enumerated as the children of a res-10 cover and tested on their
// centre: polygonToCells at res 12 costs ~0.23 s for a LOD-2 tile, this path
// ~0.05 s, and the centres come for free. Exactness is pinned by TestNEGlyphGeomExact.
func neBoxCellsCentres(w, s, e, n float64) ([]uint64, []float64) {
	const r10 = 0.0015 // > the res-10 cell radius (~75 m): every res-10 cell with a child centre in the box is in the cover
	poly := h3.GeoPolygon{GeoLoop: h3.GeoLoop{
		h3.NewLatLng(s-r10, w-r10), h3.NewLatLng(n+r10, w-r10), h3.NewLatLng(n+r10, e+r10), h3.NewLatLng(s-r10, e+r10),
	}}
	cover, err := h3.PolygonToCells(poly, 10)
	if err != nil {
		ids := neBBoxCellsPoly(w, s, e, n)
		cs := make([]float64, 2*len(ids))
		for k, id := range ids {
			cs[2*k], cs[2*k+1] = neCellCentre(id)
		}
		return ids, cs
	}
	type cell struct {
		id       uint64
		lon, lat float64
	}
	cells := make([]cell, 0, len(cover)*49)
	for _, c := range cover {
		kids, err := c.Children(neCellRes)
		if err != nil {
			continue
		}
		for _, k := range kids {
			ll, err := k.LatLng()
			if err != nil {
				continue
			}
			if ll.Lng >= w && ll.Lng <= e && ll.Lat >= s && ll.Lat <= n {
				cells = append(cells, cell{uint64(k), ll.Lng, ll.Lat})
			}
		}
	}
	sort.Slice(cells, func(a, b int) bool { return cells[a].id < cells[b].id })
	ids := make([]uint64, len(cells))
	cs := make([]float64, 2*len(cells))
	for k, c := range cells {
		ids[k], cs[2*k], cs[2*k+1] = c.id, c.lon, c.lat
	}
	return ids, cs
}

// ── prewarm ────────────────────────────────────────────────────────────────
//
// Every full-res (LOD 2) tile request queues the 8 tiles around it; one
// background worker builds the geometry for those not yet cached, newest
// request first, so a pan lands on warm tiles. One worker only (the machine
// has few cores and the live requests come first); the queue is bounded and
// deduplicated. Stats under inflight ne_glyph_geom.prewarm.

var (
	neGlyphGeomPreMu   sync.Mutex
	neGlyphGeomPreQ    [][3]int // lod index, i, j; newest last
	neGlyphGeomPreSet  = map[[3]int]bool{}
	neGlyphGeomPreCh   = make(chan struct{}, 1)
	neGlyphGeomPreOnce sync.Once
	neGlyphGeomPreDone int64
	neGlyphGeomPreSkip int64
)

const neGlyphGeomPreQMax = 64

// neGlyphGeomPrewarmAround — queue the ring-1 neighbours of tile (lod, i, j).
func neGlyphGeomPrewarmAround(lod neGlyphLOD, i, j int) {
	li := lodIndex(lod)
	if li != 2 {
		return // LOD 0/1 geometry builds in ~20 ms, not worth a queue
	}
	neGlyphGeomPreOnce.Do(func() { go neGlyphGeomPreWorker() })
	neGlyphGeomPreMu.Lock()
	for dj := 1; dj >= -1; dj-- {
		for di := -1; di <= 1; di++ {
			if di == 0 && dj == 0 {
				continue
			}
			k := [3]int{li, i + di, j + dj}
			if neGlyphGeomPreSet[k] {
				continue
			}
			w, s, e, n := neGlyphTileBox(lod, k[1], k[2])
			if e < 9.4 || w > 17.3 || n < 46.3 || s > 49.1 {
				continue
			}
			neGlyphGeomPreSet[k] = true
			neGlyphGeomPreQ = append(neGlyphGeomPreQ, k)
		}
	}
	for len(neGlyphGeomPreQ) > neGlyphGeomPreQMax {
		delete(neGlyphGeomPreSet, neGlyphGeomPreQ[0])
		neGlyphGeomPreQ = neGlyphGeomPreQ[1:]
	}
	neGlyphGeomPreMu.Unlock()
	select {
	case neGlyphGeomPreCh <- struct{}{}:
	default:
	}
}

func neGlyphGeomPreWorker() {
	for range neGlyphGeomPreCh {
		for {
			neGlyphGeomPreMu.Lock()
			if len(neGlyphGeomPreQ) == 0 {
				neGlyphGeomPreMu.Unlock()
				break
			}
			k := neGlyphGeomPreQ[len(neGlyphGeomPreQ)-1]
			neGlyphGeomPreQ = neGlyphGeomPreQ[:len(neGlyphGeomPreQ)-1]
			delete(neGlyphGeomPreSet, k)
			neGlyphGeomPreMu.Unlock()
			key := fmt.Sprintf("%d/%d/%d", k[0], k[1], k[2])
			neGlyphGeomMu.Lock()
			_, have := neGlyphGeomItems[key]
			_, fly := neGlyphGeomFly[key]
			neGlyphGeomMu.Unlock()
			if have || fly {
				atomic.AddInt64(&neGlyphGeomPreSkip, 1)
				continue
			}
			neGlyphTileCellsGet(neGlyphLODs[k[0]], k[1], k[2])
			atomic.AddInt64(&neGlyphGeomPreDone, 1)
		}
	}
}

func neGlyphGeomPreQLen() int {
	neGlyphGeomPreMu.Lock()
	defer neGlyphGeomPreMu.Unlock()
	return len(neGlyphGeomPreQ)
}
