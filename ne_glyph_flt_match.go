package main

// The filter as a cell test for the overlay tiles — `v=chg&f=<expr>` (ne_glyph_tile.go):
// with a filter on, the change layer sits ABOVE the veil on the page (chgZ) and paints
// only the cells that match, the way the tree rings do (apKeep in ne_glyph.go: 1 match ·
// 0 veiled · 2 the bin is not built yet → shown, like an unveiled tile). The test is the
// fidx bin's own mask (neFidxBin.eval, the same mask the veil tile cuts its holes from),
// looked up by the cell's local IJ around the bin's origin; bins load lazily, a tile touches
// at most the handful under it.

import (
	"image"
	"math"

	"github.com/uber/h3-go/v4"
)

type neFltMatchBin struct {
	b    *neFidxBin
	mask []uint8
	org  h3.Cell
	oi   int
	oj   int
	lat  map[uint32]int32
}

type neFltMatcher struct {
	expr    *neFiltExpr
	stamp   int64
	bins    map[[2]int]*neFltMatchBin
	Pending int // bins still building (their cells are shown)
	Out     int // cells dropped as veiled
}

func neFltMatcherFor(expr *neFiltExpr, stamp int64) *neFltMatcher {
	if expr == nil {
		return nil
	}
	return &neFltMatcher{expr: expr, stamp: stamp, bins: map[[2]int]*neFltMatchBin{}}
}

func (f *neFltMatcher) bin(bi, bj int) *neFltMatchBin {
	k := [2]int{bi, bj}
	if mb, ok := f.bins[k]; ok {
		return mb
	}
	raw, _, ok := neFidxPeek(bi, bj, f.stamp)
	if !ok {
		f.Pending++
		f.bins[k] = nil
		return nil
	}
	mb := &neFltMatchBin{}
	if raw != nil {
		if b, err := neFidxDecode(raw); err == nil && b.N > 0 {
			if o, err := neParseCellHex(b.Hdr.Origin); err == nil {
				org := h3.Cell(o)
				if oij, err := h3.CellToLocalIJ(org, org); err == nil {
					mb.b, mb.mask, mb.org, mb.oi, mb.oj, mb.lat = b, b.eval(f.expr), org, oij.I, oij.J, b.lattice()
				}
			}
		}
	}
	f.bins[k] = mb
	return mb
}

// match — 1 the cell matches · 0 veiled (or unknown to the index) · 2 the bin is pending (show).
func (f *neFltMatcher) match(id uint64, lon, lat float64) uint8 {
	if f == nil {
		return 1
	}
	mb := f.bin(int(math.Floor(lon/neFidxBinDeg)), int(math.Floor(lat/neFidxBinDeg)))
	if mb == nil {
		return 2
	}
	if mb.b == nil {
		f.Out++
		return 0
	}
	ij, err := h3.CellToLocalIJ(mb.org, h3.Cell(id))
	if err != nil {
		f.Out++
		return 0
	}
	if k, ok := mb.lat[ijKey(int32(ij.I-mb.oi), int32(ij.J-mb.oj))]; ok && mb.mask[k] != 0 {
		return 1
	}
	f.Out++
	return 0
}

// neChgGrow — a change mark is one res-12 cell: at LOD 2 two tile px wide, and at LOD 0/1 zoomed out the
// client's downscale averages it away. hl is the client's downscale factor (tile px per screen px, like the
// veil's, ne_glyph_flt_tile.go neFltMinR): every painted pixel is grown to radius r = hl·2.2 tile px, so a
// mark reads like a tree ring — ~5 screen px across with a dark 1-screen-px rim (neGlyphIdxChgHalo) — at any
// zoom; the growth fades with the zoom (hl 16 → 1) and at LOD 0/1 stops once a cell is wider than a pixel (hl 1).
// Chamfer distance transform with label propagation, so a grown pixel takes the tone and age of its nearest mark.
func neChgGrow(img *image.Paletted, hl int) { neMarkGrow(img, hl, neGlyphIdxChgHalo, 2.2, 1.0) }

// neMarkGrow — neChgGrow for any mark palette: halo is the palette index of the rim, rad/rim the radii in
// screen px (× hl tile px). The sparse change marks grow to 2.2 px dots with a 1 px rim; the dense diff marks
// (ne_glyph_dif.go) only to the screen's pixel (0.6 + 0.3; hl runs ≈ 1.4 × the downscale) so that they keep their density at every zoom.
func neMarkGrow(img *image.Paletted, hl int, halo uint8, rad, rim float64) {
	if hl < 1 {
		return
	}
	r := int32(math.Round(float64(hl) * rad * 3))  // ×3: chamfer units
	rh := r + int32(math.Round(float64(hl)*rim*3)) // the rim outside
	w, h := img.Rect.Dx(), img.Rect.Dy()
	pix := img.Pix
	const inf = 1 << 30
	d := make([]int32, w*h)
	lab := make([]uint8, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if p := pix[y*img.Stride+x]; p != 0 {
				lab[y*w+x] = p
			} else {
				d[y*w+x] = inf
			}
		}
	}
	try := func(o, src int, c int32) {
		if v := d[src] + c; v < d[o] {
			d[o] = v
			lab[o] = lab[src]
		}
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			o := y*w + x
			if x > 0 {
				try(o, o-1, 3)
			}
			if y > 0 {
				try(o, o-w, 3)
				if x > 0 {
					try(o, o-w-1, 4)
				}
				if x+1 < w {
					try(o, o-w+1, 4)
				}
			}
		}
	}
	for y := h - 1; y >= 0; y-- {
		for x := w - 1; x >= 0; x-- {
			o := y*w + x
			if x+1 < w {
				try(o, o+1, 3)
			}
			if y+1 < h {
				try(o, o+w, 3)
				if x+1 < w {
					try(o, o+w+1, 4)
				}
				if x > 0 {
					try(o, o+w-1, 4)
				}
			}
		}
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			o := y*w + x
			if pix[y*img.Stride+x] != 0 {
				continue
			}
			if d[o] <= r {
				pix[y*img.Stride+x] = lab[o]
			} else if d[o] <= rh {
				pix[y*img.Stride+x] = halo
			}
		}
	}
}
