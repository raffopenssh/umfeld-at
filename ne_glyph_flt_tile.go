package main

// The filter veil as a glyph tile — `v=flt&f=<expr>` at every LOD, painted here
// from the fidx bins exactly like the declared / observed / subsidy tiles
// (docs/ne-filter.md §9): one PNG per LOD box (palette: transparent · veil ·
// accent hairline · page colour · border ink), ~5–30 KB on the wire instead of
// 16 × 0.4 MB bins to the phone, cached per canonical expression + bin state in
// the tile LRU, driven by the same hiTile / hiCull / hiEvict path as every other
// view. The sheet is veil, the matching cells are cut out of it, the outer
// boundary of matching clusters gets the hairline at LOD 0/1 (sub-pixel at
// LOD 2), and the state border is painted in (ne_glyph_border.go). Meta: n (cells), match,
// trees (apices of the matching cells — inside the height term when the bins carry apex lists, ne_tree_h.go; tx = cells counted from a list), bins, pending (bins that failed to build — the tile is served no-store and
// asked again on the next move).

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/uber/h3-go/v4"
)

// palette index 1 = veil, 2 = hairline, 3/4 = half/quarter veil (the feathered edge, neFltFeather); `th=d` = the dark theme's page colour.
// The veil is opaque in the bitmap; the page puts the opacity on the <g> that holds the tiles (so the tiles' bleed
// overlaps never double up — a translucent veil twice is a visible lighter band along every tile edge).
func neGlyphFltPalette(dark bool) color.Palette {
	veil := color.RGBA{255, 255, 255, 255}
	acc := color.RGBA{40, 110, 70, 230}
	if dark {
		veil = color.RGBA{20, 22, 26, 255}
		acc = color.RGBA{110, 190, 140, 230}
	}
	half := color.RGBA{veil.R / 2, veil.G / 2, veil.B / 2, 128}
	quarter := color.RGBA{veil.R / 4, veil.G / 4, veil.B / 4, 64}
	// 5..7 = the state border (ne_glyph_border.go): the page colour on the far side (the landing page's --bg),
	// the border ink and its soft edge (premultiplied)
	page := color.RGBA{251, 251, 248, 255}
	ink := color.RGBA{54, 58, 54, 225}
	if dark {
		page = color.RGBA{17, 19, 18, 255}
		ink = color.RGBA{170, 176, 170, 210}
	}
	inkHalf := color.RGBA{ink.R / 2, ink.G / 2, ink.B / 2, ink.A / 2}
	return color.Palette{color.RGBA{0, 0, 0, 0}, veil, acc, half, quarter, page, ink, inkHalf}
}

// neFltMinR — hole radius in tile px that makes a matching cell at least one screen pixel wide once the client has
// downscaled the tile by hl (screen px per tile px, 1..16): the picture then shows every match at the resolution the
// screen has — a cell narrower than a pixel becomes that pixel, never more — and the growth fades continuously with
// zoom (hl 16 → 1) instead of switching a style on. At hl=1 nothing grows.
func neFltMinR(hl int) float64 {
	if hl <= 1 {
		return 0
	}
	return float64(hl) / 2
}

// neFltGrow — grow every hole (matching cell, palette 0) to radius r: a chamfer (3-4) distance transform from the
// holes over the whole picture — O(pixels), independent of how many cells match — then every veil pixel within r
// joins the hole. Adjacent cells merge as they would on screen anyway; the hairline (2) stays.
func neFltGrow(img *image.Paletted, r float64) {
	if r < 0 {
		r = 0
	}
	w, h := img.Rect.Dx(), img.Rect.Dy()
	pix := img.Pix
	const inf = 1 << 30
	d := make([]int32, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if pix[y*img.Stride+x] == 0 {
				d[y*w+x] = 0
			} else {
				d[y*w+x] = inf
			}
		}
	}
	neChamfer(d, w, h)
	// … and one tile pixel of the accent hairline around the grown hole: at hl=1 a crisp 1-px ring (what the LOD 0/1
	// strokes draw), at hl=8 an eighth of a screen pixel — the client's downscale averages it into a faint tint,
	// so the ring fades with the zoom as continuously as the growth does; the hole itself never gains area from it
	r1 := int32(math.Round(r * 3))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			o := y*img.Stride + x
			if pix[o] != 1 {
				continue
			}
			if v := d[y*w+x]; v <= r1 {
				pix[o] = 0
			} else if v <= r1+3 {
				pix[o] = 2
			}
		}
	}
}

type neGlyphFltMeta struct {
	n, match, trees, tx, bins, pending int // tx: matching cells whose trees came from an apex list (= match → the figure is exact, inside the height term if one is set)
}

func (m *neGlyphFltMeta) String() string {
	return "match=" + strconv.Itoa(m.match) + ";trees=" + strconv.Itoa(m.trees) + ";tx=" + strconv.Itoa(m.tx) + ";bins=" + strconv.Itoa(m.bins) + ";pend=" + strconv.Itoa(m.pending)
}

// neFidxEdgeMap — for each IJ neighbour direction the hexagon edge (vertex index)
// facing it. A bijection: directions and edges are both sorted by angle and paired
// cyclically at the rotation with the least angular error — a nearest-edge pick per
// direction can map two directions onto one edge and leave two sides unstroked.
func neFidxEdgeMap(b *neFidxBin) [6]int {
	var E [6]int
	a, hex := b.Hdr.A, b.Hdr.Hex
	if len(hex) < 6 || len(a) < 6 {
		return E
	}
	ax, ay := 4, 5 // y = a[3] + a[4] i + a[5] j (affine); a[6] + a[7] i + a[8] j (quadratic)
	if len(a) >= 12 {
		ax, ay = 7, 8
	}
	type ang struct {
		i int
		t float64
	}
	var dirs, edges [6]ang
	for d := 0; d < 6; d++ {
		dx := a[1]*float64(neFidxNbr[d][0]) + a[2]*float64(neFidxNbr[d][1])
		dy := a[ax]*float64(neFidxNbr[d][0]) + a[ay]*float64(neFidxNbr[d][1])
		dirs[d] = ang{d, math.Atan2(dy, dx)}
		w := (d + 1) % 6
		edges[d] = ang{d, math.Atan2((hex[d][1]+hex[w][1])/2, (hex[d][0]+hex[w][0])/2)}
	}
	less := func(v []ang) func(i, j int) bool { return func(i, j int) bool { return v[i].t < v[j].t } }
	sort.Slice(dirs[:], less(dirs[:]))
	sort.Slice(edges[:], less(edges[:]))
	best, br := math.Inf(1), 0
	for r := 0; r < 6; r++ {
		sum := 0.0
		for k := 0; k < 6; k++ {
			dt := math.Abs(dirs[k].t - edges[(k+r)%6].t)
			if dt > math.Pi {
				dt = 2*math.Pi - dt
			}
			sum += dt
		}
		if sum < best {
			best, br = sum, r
		}
	}
	for k := 0; k < 6; k++ {
		E[dirs[k].i] = edges[(k+br)%6].i
	}
	return E
}

// neFltFeather — soften the veil's edge: transparent pixels next to the veil get a
// half veil, the next ring a quarter. The cells underneath come from another tile
// (the declared base, its own bleed and resampling), so the two pictures may sit a
// screen pixel apart on some browsers; a soft edge reads as shading, a hard one as a
// white sliver beside the matching cell.
func neFltFeather(img *image.Paletted) {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	pix := img.Pix
	at := func(x, y int) uint8 {
		if x < 0 || y < 0 || x >= w || y >= h {
			return 0 // beyond the picture is not veil: a 1 would feather every image edge into a light seam across matching cells where two tiles' rings overlap
		}
		return pix[y*img.Stride+x]
	}
	// pass 0: transparent next to veil/hairline → half; pass 1: transparent next to half → quarter
	for pass := 0; pass < 2; pass++ {
		hit := func(v uint8) bool {
			if pass == 0 {
				return v == 1 || v == 2
			}
			return v == 3
		}
		var mark []int
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				if at(x, y) != 0 {
					continue
				}
				if hit(at(x-1, y)) || hit(at(x+1, y)) || hit(at(x, y-1)) || hit(at(x, y+1)) {
					mark = append(mark, y*img.Stride+x)
				}
			}
		}
		for _, o := range mark {
			pix[o] = uint8(3 + pass)
		}
	}
}

// neLineIdx — 1-px DDA line in palette index ci.
func neLineIdx(img *image.Paletted, xa, ya, xb, yb float64, ci uint8) {
	dx, dy := xb-xa, yb-ya
	n := int(math.Ceil(math.Max(math.Abs(dx), math.Abs(dy))))
	if n < 1 {
		n = 1
	}
	bd := img.Bounds()
	for i := 0; i <= n; i++ {
		x, y := int(math.Floor(xa+dx*float64(i)/float64(n))), int(math.Floor(ya+dy*float64(i)/float64(n)))
		if x < bd.Min.X || y < bd.Min.Y || x >= bd.Max.X || y >= bd.Max.Y {
			continue
		}
		img.Pix[(y-bd.Min.Y)*img.Stride+(x-bd.Min.X)] = ci
	}
}

// neGlyphFltRender — the veil tile for one LOD box and expression.
// hl: the client's downscale factor (screen px per tile px, 1..16) — sizes the minimum hole (neFltMinR).
func neGlyphFltRender(g *neGlyphTileGeom, w, s, e, n float64, expr *neFiltExpr, dark bool, hl int, m *neGlyphFltMeta) []byte {
	img := image.NewPaletted(image.Rect(0, 0, g.pw, g.ph), neGlyphFltPalette(dark))
	// the whole sheet is veil; the matching cells are cut out of it — a cell the index does not know is veiled,
	// never a gap; the far side of the state border is covered client-side (fltBorder) so the country's edge is
	// the border line, not the hexagon stairs of the border cells
	for i := range img.Pix {
		img.Pix[i] = 1
	}
	px := g.lod.px
	// the bins of the box plus the neighbours the ring reaches into: a tile paints every cell whose centre lies in its
	// picture + mg, including the neighbours' — so two tiles' rings carry the same cells and the browser's resampled
	// image edges (half-transparent for one screen pixel) always sit over a fully painted neighbour. ~1 unit ≈ 0.00115°.
	mg := neGlyphFltBleed(g.lod)/px + 0.15 // frame units beyond the picture: the ring plus one hexagon
	ring := (neGlyphFltBleed(g.lod)/px + mg) * 0.00115
	bi0, bi1 := int(math.Floor((w-ring)/neFidxBinDeg+1e-9)), int(math.Floor((e+ring)/neFidxBinDeg-1e-9))
	bj0, bj1 := int(math.Floor((s-ring)/neFidxBinDeg+1e-9)), int(math.Floor((n+ring)/neFidxBinDeg-1e-9))
	pad := 0.6 // px: the holes grown so neighbouring matches fuse and a sub-pixel match still marks
	// cells whose centre lies up to a hexagon outside the picture (incl. its bleed ring) are painted too, so the ring is a
	// full sheet and two tiles' rings overlap without a seam (a res-12 hexagon spans ≈ 0.11 frame units: 0.1° ≈ 87 units)
	x0, y0 := g.x0-mg, g.y0-mg
	x1, y1 := g.x0+float64(g.pw)/px+mg, g.y0+float64(g.ph)/px+mg
	// the counts (n, match) take only cells whose centre lies in the box itself — the ring is paint, not sum
	cx0, cy0 := neGlyphProj(w, n)
	cx1, cy1 := neGlyphProj(e, s)
	strokes := g.lod.px >= 40
	// the hairline: every matching cell's edges towards a non-matching neighbour. Edges towards a matching cell of
	// ANOTHER bin cancel too (the twin edge from that bin's lattice, snapped with tolerance as in the vector veil):
	// at LOD 0 a bin is a tile, so without the cancel every tile margin wore a zig-zag of hairlines along the cells
	// whose matching neighbour sits in the next bin — a thin seam crossing the matching clusters.
	var vec *neFltVec
	if strokes {
		vec = &neFltVec{edges: make(map[[2]neFltVecPt]int, 1024)}
	}
	stamp := time.Now().UnixNano()
	var poly [6][2]float64
	polyS := poly[:]
	for bi := bi0; bi <= bi1; bi++ {
		for bj := bj0; bj <= bj1; bj++ {
			m.bins++
			raw, _, ok := neFidxPeek(bi, bj, stamp)
			if !ok {
				// still building: this bin is painted from the national sample (res-10 faces) and the
				// client asks again — the user keeps the map, the veil sharpens as the bins land
				m.pending++
				neFltPaintSample(img, g, bi, bj, expr, pad, x0, y0, x1, y1, cx0, cy0, cx1, cy1, m)
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
			var hx, hy [6]float64
			for v := 0; v < 6; v++ {
				hx[v], hy[v] = hex[v][0]*px, hex[v][1]*px
				if hx[v] > 0 {
					hx[v] += pad
				} else {
					hx[v] -= pad
				}
				if hy[v] > 0 {
					hy[v] += pad
				} else {
					hy[v] -= pad
				}
			}
			any := false
			for k := 0; k < b.N; k++ {
				fx, fy := b.pos(k)
				if fx < x0 || fx > x1 || fy < y0 || fy > y1 {
					continue
				}
				if fx >= cx0 && fx < cx1 && fy >= cy0 && fy < cy1 {
					m.n++
					if mask[k] != 0 {
						m.match++
						v, exact := tc.at(k)
						m.trees += v
						if exact {
							m.tx++
						}
					}
				}
				if mask[k] == 0 {
					continue
				}
				any = true
				cx, cy := (fx-g.x0)*px, (fy-g.y0)*px
				for v := 0; v < 6; v++ {
					poly[v] = [2]float64{cx + hx[v], cy + hy[v]}
				}
				neFillPoly(img, polyS, 0)
			}
			if !strokes || !any {
				continue
			}
			lat, E := b.lattice(), neFidxEdgeMap(b)
			for k := 0; k < b.N; k++ {
				if mask[k] == 0 {
					continue
				}
				fx, fy := b.pos(k)
				if fx < x0 || fx > x1 || fy < y0 || fy > y1 {
					continue
				}
				cx, cy := (fx-g.x0)*px, (fy-g.y0)*px
				ci, cj := b.IJ[2*k], b.IJ[2*k+1]
				for d := 0; d < 6; d++ {
					q, ok := lat[ijKey(ci+neFidxNbr[d][0], cj+neFidxNbr[d][1])]
					if ok && mask[q] != 0 {
						continue
					}
					v0 := E[d]
					v1 := (v0 + 1) % 6
					vec.add(cx+hex[v0][0]*px, cy+hex[v0][1]*px, cx+hex[v1][0]*px, cy+hex[v1][1]*px, !ok)
				}
			}
		}
	}
	if vec != nil {
		for _, ed := range vec.list {
			if ed.used {
				continue
			}
			neLineIdx(img, float64(ed.a.x)/neFltVecQ, float64(ed.a.y)/neFltVecQ, float64(ed.b.x)/neFltVecQ, float64(ed.b.y)/neFltVecQ, 2)
		}
	}
	neFltGrow(img, neFltMinR(hl))
	neFltFeather(img)
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.DefaultCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil
	}
	return buf.Bytes()
}

// neFltPaintSample — a bin that has no index yet, painted from the national 1-in-49 sample (ne_mvt_samp.go):
// every sample row whose centre lies in the bin stands for its res-10 parent (49 cells); the parent's face is
// cut out of the veil when the row passes the expression. The same approximation the mid band shows (docs/ne-filter.md §8),
// one level finer; the counts (×49) join n / match so the readout has a figure while the bins build.
func neFltPaintSample(img *image.Paletted, g *neGlyphTileGeom, bi, bj int, expr *neFiltExpr, pad, x0, y0, x1, y1, cx0, cy0, cx1, cy1 float64, m *neGlyphFltMeta) {
	sp := neSampGet()
	if sp == nil {
		return
	}
	mask := sp.mask(expr)
	w, s, e, n := neFidxBox(bi, bj)
	px := g.lod.px
	var poly [6][2]float64
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
		cxp, cyp := (fx-g.x0)*px, (fy-g.y0)*px
		for v := 0; v < 6; v++ {
			vx, vy := neGlyphProj(bd[v].Lng, bd[v].Lat)
			dx, dy := (vx-g.x0)*px-cxp, (vy-g.y0)*px-cyp
			if dx > 0 {
				dx += pad
			} else {
				dx -= pad
			}
			if dy > 0 {
				dy += pad
			} else {
				dy -= pad
			}
			poly[v] = [2]float64{cxp + dx, cyp + dy}
		}
		neFillPoly(img, poly[:], 0)
	}
}
