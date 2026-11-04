package main

// The filter veil as a VECTOR tile — `v=flt&vec=1&sp=<device px per tile px>` at LOD 0/1
// (docs/ne-filter.md §12). Near full zoom the raster veil is upscaled by the browser (1.7× css at z=900, ×
// the device pixel ratio): every hexagon edge became a 3–4 device-px staircase and the hairline a
// blurry band. This renders the same picture as an SVG the browser rasterises at the screen's own
// resolution: one even-odd path = the sheet minus the union outline of the matching clusters (edges shared
// by two matching cells cancel, across bin borders too), a veil-coloured stroke on the outline whose inner
// half is the feather (its outer half lies on opaque veil, invisible under the group opacity), and the accent
// hairline on top, both sized in tile px so they measure one device px at the client's scale. Same bins,
// same counts (neGlyphFltMeta), same cache key family, same <image> element on the client — the
// response is just image/svg+xml instead of image/png. A tile whose outline would exceed neFltVecMaxEdges
// edges (a fine checkerboard at LOD 0) falls back to the raster render: the client never has to know.

import (
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/uber/h3-go/v4"
)

const neFltVecMaxEdges = 40000

// neFltVecQ — vertex snap grid (tile px). Shared vertices of neighbouring cells of one bin agree to ~1e-3 px
// (same polynomial fit), across two bins' fits to a few hundredths; the ±1 cell search in neFltVecLink absorbs
// a rounding flip at a cell boundary.
const neFltVecQ = 8.0

type neFltVecPt struct{ x, y int32 }

type neFltVecEdge struct {
	a, b neFltVecPt
	used bool
}

// neFltVec — the edge soup of one tile: boundary edges keyed by their snapped endpoints; an edge seen a second time
// (from the matching cell on the other side) is a shared edge and cancels.
type neFltVec struct {
	edges map[[2]neFltVecPt]int // key (min pt, max pt) → index in list, -1 when cancelled
	list  []neFltVecEdge
	n     int // live edges
}

func neFltVecSnap(x, y float64) neFltVecPt {
	return neFltVecPt{int32(math.Round(x * neFltVecQ)), int32(math.Round(y * neFltVecQ))}
}

func neFltVecLess(a, b neFltVecPt) bool { return a.x < b.x || (a.x == b.x && a.y < b.y) }

// add one boundary edge; tol: the edge faces another bin (the neighbour is not in this bin's lattice), so the
// twin edge from the other side comes from another polynomial fit and may snap one grid cell away — look around.
func (v *neFltVec) add(x0, y0, x1, y1 float64, tol bool) {
	a, b := neFltVecSnap(x0, y0), neFltVecSnap(x1, y1)
	if a == b {
		return
	}
	if neFltVecLess(b, a) {
		a, b = b, a
	}
	key := [2]neFltVecPt{a, b}
	cancel := func(key [2]neFltVecPt) bool {
		k, ok := v.edges[key]
		if !ok {
			return false
		}
		if k >= 0 {
			v.list[k].used = true // cancelled: both sides match
			v.edges[key] = -1
			v.n--
		}
		return true
	}
	if cancel(key) {
		return
	}
	if tol {
		for dx := int32(-1); dx <= 1; dx++ {
			for dy := int32(-1); dy <= 1; dy++ {
				for ex := int32(-1); ex <= 1; ex++ {
					for ey := int32(-1); ey <= 1; ey++ {
						if dx == 0 && dy == 0 && ex == 0 && ey == 0 {
							continue
						}
						if cancel([2]neFltVecPt{{a.x + dx, a.y + dy}, {b.x + ex, b.y + ey}}) {
							return
						}
					}
				}
			}
		}
	}
	v.edges[key] = len(v.list)
	v.list = append(v.list, neFltVecEdge{a: a, b: b})
	v.n++
}

// loops — link the live edges into closed loops (undirected: pick any unused edge at the far vertex; a
// vertex seen ±1 snap cell away counts as the same vertex). Returns path data in tile px.
func (v *neFltVec) loops(sb *strings.Builder) {
	at := make(map[neFltVecPt][]int, len(v.list))
	for i := range v.list {
		e := &v.list[i]
		if e.used {
			continue
		}
		at[e.a] = append(at[e.a], i)
		at[e.b] = append(at[e.b], i)
	}
	next := func(p neFltVecPt) (int, bool) {
		for dx := int32(-1); dx <= 1; dx++ {
			for dy := int32(-1); dy <= 1; dy++ {
				for _, i := range at[neFltVecPt{p.x + dx, p.y + dy}] {
					if !v.list[i].used {
						return i, true
					}
				}
			}
		}
		return 0, false
	}
	put := func(p neFltVecPt) {
		sb.WriteString(strconv.FormatFloat(float64(p.x)/neFltVecQ, 'f', 2, 64))
		sb.WriteByte(' ')
		sb.WriteString(strconv.FormatFloat(float64(p.y)/neFltVecQ, 'f', 2, 64))
	}
	for i := range v.list {
		if v.list[i].used {
			continue
		}
		v.list[i].used = true
		start, cur := v.list[i].a, v.list[i].b
		sb.WriteByte('M')
		put(start)
		for n := 0; n < len(v.list); n++ {
			sb.WriteByte('L')
			put(cur)
			k, ok := next(cur)
			if !ok {
				break
			}
			v.list[k].used = true
			if v.list[k].a == cur || (abs32(v.list[k].a.x-cur.x) <= 1 && abs32(v.list[k].a.y-cur.y) <= 1) {
				cur = v.list[k].b
			} else {
				cur = v.list[k].a
			}
			if cur == start {
				break
			}
		}
		sb.WriteByte('Z')
	}
}

func abs32(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}

// neGlyphFltRenderVec — the vector veil for one LOD box; sp = device px per tile px at the client (1..8).
// Returns nil when the outline is too dense for a vector tile (caller falls back to the raster).
func neGlyphFltRenderVec(g *neGlyphTileGeom, w, s, e, n float64, expr *neFiltExpr, dark bool, sp float64, m *neGlyphFltMeta) []byte {
	px := g.lod.px
	mg := neGlyphFltBleed(g.lod)/px + 0.15
	ring := (neGlyphFltBleed(g.lod)/px + mg) * 0.00115
	bi0, bi1 := int(math.Floor((w-ring)/neFidxBinDeg+1e-9)), int(math.Floor((e+ring)/neFidxBinDeg-1e-9))
	bj0, bj1 := int(math.Floor((s-ring)/neFidxBinDeg+1e-9)), int(math.Floor((n+ring)/neFidxBinDeg-1e-9))
	x0, y0 := g.x0-mg, g.y0-mg
	x1, y1 := g.x0+float64(g.pw)/px+mg, g.y0+float64(g.ph)/px+mg
	cx0, cy0 := neGlyphProj(w, n)
	cx1, cy1 := neGlyphProj(e, s)
	stamp := time.Now().UnixNano()
	vec := &neFltVec{edges: make(map[[2]neFltVecPt]int, 4096)}
	for bi := bi0; bi <= bi1; bi++ {
		for bj := bj0; bj <= bj1; bj++ {
			m.bins++
			raw, _, ok := neFidxPeek(bi, bj, stamp)
			if !ok {
				m.pending++
				neFltVecSample(vec, g, bi, bj, expr, x0, y0, x1, y1, cx0, cy0, cx1, cy1, m)
				continue
			}
			if raw == nil {
				continue
			}
			b, err := neFidxDecode(raw)
			if err != nil || b.N == 0 || len(b.Hdr.Hex) < 6 {
				continue
			}
			mask := b.eval(expr)
			tc := neNewTreeCounter(b.TH, b.col("tn"), expr)
			hex := b.Hdr.Hex
			lat, E := b.lattice(), neFidxEdgeMap(b)
			for k := 0; k < b.N; k++ {
				fx, fy := b.pos(k)
				if fx < x0 || fx > x1 || fy < y0 || fy > y1 {
					continue
				}
				in := fx >= cx0 && fx < cx1 && fy >= cy0 && fy < cy1
				if in {
					m.n++
				}
				if mask[k] == 0 {
					continue
				}
				if in {
					m.match++
					v, exact := tc.at(k)
					m.trees += v
					if exact {
						m.tx++
					}
				}
				cx, cy := (fx-g.x0)*px, (fy-g.y0)*px
				ci, cj := b.IJ[2*k], b.IJ[2*k+1]
				for d := 0; d < 6; d++ {
					q, ok := lat[ijKey(ci+neFidxNbr[d][0], cj+neFidxNbr[d][1])]
					if ok && mask[q] != 0 {
						continue // shared with a matching neighbour of this bin: interior
					}
					v0 := E[d]
					v1 := (v0 + 1) % 6
					vec.add(cx+hex[v0][0]*px, cy+hex[v0][1]*px, cx+hex[v1][0]*px, cy+hex[v1][1]*px, !ok)
				}
				if vec.n > neFltVecMaxEdges {
					return nil
				}
			}
		}
	}
	veil, acc := "#fff", "#286e46"
	if dark {
		veil, acc = "#14161a", "#6ebe8c"
	}
	if sp < 1 {
		sp = 1
	}
	hair := 1.0 / sp            // one device px
	feather := 2.0 * 1.25 / sp  // 1.25 device px inside the hole (the outer half sits on opaque veil)
	var sb strings.Builder
	sb.Grow(64 + 24*vec.n)
	sb.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" width="`)
	sb.WriteString(strconv.Itoa(g.pw))
	sb.WriteString(`" height="`)
	sb.WriteString(strconv.Itoa(g.ph))
	sb.WriteString(`" viewBox="0 0 `)
	sb.WriteString(strconv.Itoa(g.pw))
	sb.WriteByte(' ')
	sb.WriteString(strconv.Itoa(g.ph))
	sb.WriteString(`" shape-rendering="geometricPrecision">`)
	var d strings.Builder
	vec.loops(&d)
	loops := d.String()
	sb.WriteString(`<path fill="` + veil + `" fill-rule="evenodd" d="M0 0H`)
	sb.WriteString(strconv.Itoa(g.pw))
	sb.WriteString("V")
	sb.WriteString(strconv.Itoa(g.ph))
	sb.WriteString("H0Z")
	sb.WriteString(loops)
	sb.WriteString(`"/>`)
	if loops != "" {
		sb.WriteString(`<path fill="none" stroke="` + veil + `" stroke-opacity=".45" stroke-linejoin="round" stroke-width="`)
		sb.WriteString(strconv.FormatFloat(feather, 'f', 3, 64))
		sb.WriteString(`" d="`)
		sb.WriteString(loops)
		sb.WriteString(`"/><path fill="none" stroke="` + acc + `" stroke-opacity=".9" stroke-linejoin="round" stroke-width="`)
		sb.WriteString(strconv.FormatFloat(hair, 'f', 3, 64))
		sb.WriteString(`" d="`)
		sb.WriteString(loops)
		sb.WriteString(`"/>`)
	}
	sb.WriteString(`</svg>`)
	return []byte(sb.String())
}

// neFltVecSample — a bin without an index yet: its res-10 parents' faces from the national sample (as neFltPaintSample).
func neFltVecSample(vec *neFltVec, g *neGlyphTileGeom, bi, bj int, expr *neFiltExpr, x0, y0, x1, y1, cx0, cy0, cx1, cy1 float64, m *neGlyphFltMeta) {
	sp := neSampGet()
	if sp == nil {
		return
	}
	mask := sp.mask(expr)
	w, s, e, n := neFidxBox(bi, bj)
	px := g.lod.px
	for _, k := range sp.rowsNear(w, s, e, n) {
		id := sp.id(k)
		lon, lat := neCellCentre(id)
		if lon < w || lon >= e || lat < s || lat >= n {
			continue
		}
		fx, fy := neGlyphProj(lon, lat)
		if fx < x0 || fx > x1 || fy < y0 || fy > y1 {
			continue
		}
		hit := k < len(mask) && mask[k] != 0
		if fx >= cx0 && fx < cx1 && fy >= cy0 && fy < cy1 {
			m.n += 49
			if hit {
				m.match += 49
			}
		}
		if !hit {
			continue
		}
		p10, err := h3.Cell(id).Parent(10)
		if err != nil {
			continue
		}
		bd, err := p10.Boundary()
		if err != nil || len(bd) != 6 {
			continue
		}
		var p [6][2]float64
		for v := 0; v < 6; v++ {
			vx, vy := neGlyphProj(bd[v].Lng, bd[v].Lat)
			p[v] = [2]float64{(vx - g.x0) * px, (vy - g.y0) * px}
		}
		for v := 0; v < 6; v++ {
			q := (v + 1) % 6
			vec.add(p[v][0], p[v][1], p[q][0], p[q][1], false)
		}
	}
}
