package main

// The filter veil as a VECTOR tile — `v=flt&vec=1&sp=<device px per tile px>` at LOD 0/1
// (docs/ne-filter.md §12). Near full zoom the raster veil is upscaled by the browser (1.7× css at z=900, ×
// the device pixel ratio): every hexagon edge became a 3–4 device-px staircase and the hairline a
// blurry band. This renders the same picture as an SVG the browser rasterises at the screen's own
// resolution: one even-odd path = the sheet minus the union outline of the matching clusters (edges shared
// by two matching cells cancel, across bin borders too), painted over an accent stroke on the same outline
// whose outer half the sheet covers — what remains is one device px of the wald green inside every hole, on
// the cell colour, no feather, no halo (the browser anti-aliases the vector edge itself). Same bins,
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
	// walk from p along unused edges until the chain closes at stop or runs out; the visited vertices go to out.
	walk := func(p, stop neFltVecPt, out []neFltVecPt) ([]neFltVecPt, bool) {
		for n := 0; n < len(v.list); n++ {
			k, ok := next(p)
			if !ok {
				return out, false
			}
			v.list[k].used = true
			if v.list[k].a == p || (abs32(v.list[k].a.x-p.x) <= 1 && abs32(v.list[k].a.y-p.y) <= 1) {
				p = v.list[k].b
			} else {
				p = v.list[k].a
			}
			if p == stop || (abs32(p.x-stop.x) <= 1 && abs32(p.y-stop.y) <= 1) {
				return out, true
			}
			out = append(out, p)
		}
		return out, false
	}
	var fwd, back []neFltVecPt
	for i := range v.list {
		if v.list[i].used {
			continue
		}
		v.list[i].used = true
		start, cur := v.list[i].a, v.list[i].b
		fwd = append(fwd[:0], cur)
		fwd, closed := walk(cur, start, fwd)
		back = back[:0]
		if !closed {
			// an open chain (the walk began mid-chain): extend the other way from start so the
			// path is one polyline, never a chain broken into fragments each closed by a chord
			back, _ = walk(start, cur, back)
		}
		sb.WriteByte('M')
		if len(back) > 0 {
			put(back[len(back)-1])
			for k := len(back) - 2; k >= 0; k-- {
				sb.WriteByte('L')
				put(back[k])
			}
			sb.WriteByte('L')
		}
		put(start)
		for _, p := range fwd {
			sb.WriteByte('L')
			put(p)
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
						// shared with a matching neighbour of this bin: interior — unless that neighbour lies
						// outside the painted box (its own edges are never added): then the edge is the
						// picture's boundary, or the chain stays open and closes by a chord across the tile
						if qx, qy := b.pos(int(q)); qx >= x0 && qx <= x1 && qy >= y0 && qy <= y1 {
							continue
						}
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
	// the accent = the wald group colour (neGlyphGroupRGB[4]); the dark theme lifts it so it still reads on the dark veil
	veil, acc := "#fff", "#276b38"
	if dark {
		veil, acc = "#14161a", "#6ebe8c"
	}
	if sp < 1 {
		sp = 1
	}
	hair := 2.0 / sp // the stroke straddles the outline; the sheet painted AFTER it covers the outer half → one crisp device px inside the hole, on the cell colour, no halo
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
	if loops != "" {
		sb.WriteString(`<path fill="none" stroke="` + acc + `" stroke-linejoin="round" stroke-width="`)
		sb.WriteString(strconv.FormatFloat(hair, 'f', 3, 64))
		sb.WriteString(`" d="`)
		sb.WriteString(loops)
		sb.WriteString(`"/>`)
	}
	sb.WriteString(`<path fill="` + veil + `" fill-rule="evenodd" d="M0 0H`)
	sb.WriteString(strconv.Itoa(g.pw))
	sb.WriteString("V")
	sb.WriteString(strconv.Itoa(g.ph))
	sb.WriteString("H0Z")
	sb.WriteString(loops)
	sb.WriteString(`"/></svg>`)
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
