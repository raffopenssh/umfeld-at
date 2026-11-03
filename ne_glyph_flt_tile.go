package main

// The filter veil as a glyph tile — `v=flt&f=<expr>` at every LOD, painted here
// from the fidx bins exactly like the declared / observed / subsidy tiles
// (docs/ne-filter.md §9): one PNG per LOD box (palette: transparent · veil ·
// accent hairline), ~5–30 KB on the wire instead of 16 × 0.4 MB bins to the
// phone, cached per canonical expression + bin state in the tile LRU, driven
// by the same hiTile / hiCull / hiEvict path as every other view. Cells that
// fail the expression get the veil; the outer boundary of matching clusters
// gets the hairline at LOD 0/1 (sub-pixel at LOD 2). Meta: n (cells), match,
// bins, pending (bins that failed to build — the tile is served no-store and
// asked again on the next move).

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"strconv"
)

// palette index 1 = veil, 2 = hairline; `th=d` = the dark theme's page colour.
func neGlyphFltPalette(dark bool) color.Palette {
	veil := color.RGBA{255, 255, 255, 255}
	acc := color.RGBA{40, 110, 70, 230}
	if dark {
		veil = color.RGBA{20, 22, 26, 255}
		acc = color.RGBA{110, 190, 140, 230}
	}
	return color.Palette{color.RGBA{0, 0, 0, 0}, veil, acc}
}

type neGlyphFltMeta struct {
	n, match, bins, pending int
}

func (m *neGlyphFltMeta) String() string {
	return "match=" + strconv.Itoa(m.match) + ";bins=" + strconv.Itoa(m.bins) + ";pend=" + strconv.Itoa(m.pending)
}

// neFidxEdgeMap — for each IJ neighbour direction the hexagon edge (vertex index) facing it.
func neFidxEdgeMap(b *neFidxBin) [6]int {
	var E [6]int
	a, hex := b.Hdr.A, b.Hdr.Hex
	if len(hex) < 6 || len(a) < 6 {
		return E
	}
	ax, ay := 1, 4
	if len(a) >= 12 {
		ax, ay = 7, 8
	}
	for d := 0; d < 6; d++ {
		dx := a[1]*float64(neFidxNbr[d][0]) + a[2]*float64(neFidxNbr[d][1])
		dy := a[ax]*float64(neFidxNbr[d][0]) + a[ay]*float64(neFidxNbr[d][1])
		best, bv := -2.0, 0
		for v := 0; v < 6; v++ {
			w := (v + 1) % 6
			mx, my := (hex[v][0]+hex[w][0])/2, (hex[v][1]+hex[w][1])/2
			dot := (mx*dx + my*dy) / math.Sqrt((mx*mx+my*my)*(dx*dx+dy*dy)+1e-12)
			if dot > best {
				best, bv = dot, v
			}
		}
		E[d] = bv
	}
	return E
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
func neGlyphFltRender(g *neGlyphTileGeom, w, s, e, n float64, expr *neFiltExpr, dark bool, m *neGlyphFltMeta) []byte {
	img := image.NewPaletted(image.Rect(0, 0, g.pw, g.ph), neGlyphFltPalette(dark))
	px := g.lod.px
	// the bins of the box plus the neighbours the ring reaches into: a tile paints every cell whose centre lies in its
	// picture + mg, including the neighbours' — so two tiles' rings carry the same cells and the browser's resampled
	// image edges (half-transparent for one screen pixel) always sit over a fully painted neighbour. ~1 unit ≈ 0.00115°.
	mg := neGlyphFltBleed(g.lod)/px + 0.15 // frame units beyond the picture: the ring plus one hexagon
	ring := (neGlyphFltBleed(g.lod)/px + mg) * 0.00115
	bi0, bi1 := int(math.Floor((w-ring)/neFidxBinDeg+1e-9)), int(math.Floor((e+ring)/neFidxBinDeg-1e-9))
	bj0, bj1 := int(math.Floor((s-ring)/neFidxBinDeg+1e-9)), int(math.Floor((n+ring)/neFidxBinDeg-1e-9))
	pad := 0.6 // px: grown so neighbours fuse into one sheet and sub-pixel cells still mark
	// cells whose centre lies up to a hexagon outside the picture (incl. its bleed ring) are painted too, so the ring is a
	// full sheet and two tiles' rings overlap without a seam (a res-12 hexagon spans ≈ 0.11 frame units: 0.1° ≈ 87 units)
	x0, y0 := g.x0-mg, g.y0-mg
	x1, y1 := g.x0+float64(g.pw)/px+mg, g.y0+float64(g.ph)/px+mg
	// the counts (n, match) take only cells whose centre lies in the box itself — the ring is paint, not sum
	cx0, cy0 := neGlyphProj(w, n)
	cx1, cy1 := neGlyphProj(e, s)
	strokes := g.lod.px >= 40
	var poly [6][2]float64
	polyS := poly[:]
	for bi := bi0; bi <= bi1; bi++ {
		for bj := bj0; bj <= bj1; bj++ {
			m.bins++
			raw, _, err := neFidxGet(bi, bj)
			if err != nil {
				m.pending++
				continue
			}
			b, err := neFidxDecode(raw)
			if err != nil || b.N == 0 || len(b.Hdr.Hex) < 6 {
				continue
			}
			mask := b.eval(expr)
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
					}
				}
				if mask[k] != 0 {
					any = true
					continue
				}
				cx, cy := (fx-g.x0)*px, (fy-g.y0)*px
				for v := 0; v < 6; v++ {
					poly[v] = [2]float64{cx + hx[v], cy + hy[v]}
				}
				neFillPoly(img, polyS, 1)
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
					if q, ok := lat[ijKey(ci+neFidxNbr[d][0], cj+neFidxNbr[d][1])]; ok && mask[q] != 0 {
						continue
					}
					v0 := E[d]
					v1 := (v0 + 1) % 6
					neLineIdx(img, cx+hex[v0][0]*px, cy+hex[v0][1]*px, cx+hex[v1][0]*px, cy+hex[v1][1]*px, 2)
				}
			}
		}
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.DefaultCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil
	}
	return buf.Bytes()
}
