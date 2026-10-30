// ne_glyph_subpoly.go — the subsidy layer at full resolution: the peer's
// IVP1 outline bins (GET …/invekos/poly?bbox=<aligned 0.025° bin>&format=bin,
// docs/invekos-subsidy-layer.md §outlines) decoded and rasterised into the
// v=sub / v=subc tiles of LOD 0 and 1, so a Schlag is painted as the farmer
// drew it instead of as a run of res-12 hexagons. The H3 bins stay the
// source of the per-cell sums and the cell card; the outlines add the
// Schlag counts and areas (X-NE-Tile pn/pchg/pha/pnew/pgone) and the Schlag
// under a cell (cell?sub=1 → sub.schlag). When the outline bin cannot be had
// the tile falls back to the hexagons, so the layer never goes dark for a
// missing endpoint. Nothing is stored on disk; the outlines never leave
// this process except as pictures.
package main

import (
	"encoding/binary"
	"errors"
	"image"
	"math"
	"sort"
)

// neSubPolyRec — one IVP1 record: attributes + rings in bin-local units
// (origin = bin SW corner, x east, y north, signed; outer ring first).
type neSubPolyRec struct {
	Cls, Bio, Rel, Chg, Flags byte
	Area, DArea               int64 // m² this year (0 for gone / continuation parts); Δ vs the best-linked predecessor
	Rings                     [][][2]int32
}

type neSubPolys struct {
	Year, Prev int
	Release    string
	Recs       []neSubPolyRec
}

func neSubUvarint(b []byte, o int) (uint64, int, bool) {
	v, n := binary.Uvarint(b[o:])
	if n <= 0 {
		return 0, o, false
	}
	return v, o + n, true
}

func neSubSvarint(b []byte, o int) (int64, int, bool) {
	v, o, ok := neSubUvarint(b, o)
	return int64(v>>1) ^ -int64(v&1), o, ok
}

// neSubPolyParse — decode an IVP1 body (layout in the peer's llm.txt §POLY).
func neSubPolyParse(raw *neSubRaw) (*neSubPolys, error) {
	b := raw.Body
	if len(b) < 8 || string(b[:4]) != "IVP1" {
		return nil, errors.New("not an IVP1 body")
	}
	n := int(binary.LittleEndian.Uint32(b[4:]))
	if n > 1<<20 {
		return nil, errors.New("IVP1 count")
	}
	out := &neSubPolys{Year: raw.Year, Prev: raw.Prev, Release: raw.Release, Recs: make([]neSubPolyRec, 0, n)}
	o := 8
	bad := errors.New("IVP1 truncated")
	for i := 0; i < n; i++ {
		if o+5 > len(b) {
			return nil, bad
		}
		r := neSubPolyRec{Cls: b[o] & 7, Bio: b[o+1], Rel: b[o+2], Chg: b[o+3], Flags: b[o+4]}
		if r.Chg > 5 {
			r.Chg = 0
		}
		o += 5
		var ok bool
		var a, nr uint64
		if a, o, ok = neSubUvarint(b, o); !ok {
			return nil, bad
		}
		r.Area = int64(a)
		if r.DArea, o, ok = neSubSvarint(b, o); !ok {
			return nil, bad
		}
		if nr, o, ok = neSubUvarint(b, o); !ok || nr > 4096 {
			return nil, bad
		}
		r.Rings = make([][][2]int32, 0, nr)
		for k := uint64(0); k < nr; k++ {
			var np uint64
			if np, o, ok = neSubUvarint(b, o); !ok || np > uint64(len(b)-o) {
				return nil, bad
			}
			ring := make([][2]int32, np)
			var x, y int64
			for p := uint64(0); p < np; p++ {
				var dx, dy int64
				if dx, o, ok = neSubSvarint(b, o); !ok {
					return nil, bad
				}
				if dy, o, ok = neSubSvarint(b, o); !ok {
					return nil, bad
				}
				x += dx
				y += dy
				ring[p] = [2]int32{int32(x), int32(y)}
			}
			if np >= 3 {
				r.Rings = append(r.Rings, ring)
			}
		}
		out.Recs = append(out.Recs, r)
	}
	if o != len(b) {
		return nil, errors.New("IVP1 trailing bytes")
	}
	return out, nil
}

// neSubPolyMap — bin units → tile pixels for bin (i, j) on tile frame g.
// The frame's projection is affine in lon/lat, so this is one multiply-add
// per axis.
type neSubPolyMap struct{ ax, bx, ay, by float64 }

func neSubPolyMapOf(g *neGlyphTileGeom, i, j int) neSubPolyMap {
	fr := &neGlyphFrame
	u := neSubBinDeg / neSubPolyUnits
	w, s := float64(i)*neSubBinDeg, float64(j)*neSubBinDeg
	// px = ((lon*kx - minx)*scale + pad - x0) * lod.px with lon = w + x*u
	ax := fr.kx * u * fr.scale * g.lod.px
	bx := ((w*fr.kx-fr.minx)*fr.scale + neGlyphPad - g.x0) * g.lod.px
	// py = ((-lat - miny)*scale + pad - y0) * lod.px with lat = s + y*u
	ay := -u * fr.scale * g.lod.px
	by := ((-s-fr.miny)*fr.scale + neGlyphPad - g.y0) * g.lod.px
	return neSubPolyMap{ax, bx, ay, by}
}

func (m neSubPolyMap) pt(p [2]int32) (float64, float64) {
	return m.ax*float64(p[0]) + m.bx, m.ay*float64(p[1]) + m.by
}

// neSubPolyRings — a record's rings in tile pixels; false when the whole
// outline lies outside the picture (the common case: unclipped bins carry
// every outline that touches them).
func neSubPolyRings(img *image.Paletted, m neSubPolyMap, r *neSubPolyRec, buf [][][2]float64) ([][][2]float64, bool) {
	b := img.Bounds()
	minx, miny, maxx, maxy := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	buf = buf[:0]
	for _, ring := range r.Rings {
		pr := make([][2]float64, len(ring))
		for k, p := range ring {
			x, y := m.pt(p)
			pr[k] = [2]float64{x, y}
			minx, miny, maxx, maxy = math.Min(minx, x), math.Min(miny, y), math.Max(maxx, x), math.Max(maxy, y)
		}
		buf = append(buf, pr)
	}
	if len(buf) == 0 || maxx < float64(b.Min.X)-1 || minx > float64(b.Max.X)+1 || maxy < float64(b.Min.Y)-1 || miny > float64(b.Max.Y)+1 {
		return buf, false
	}
	return buf, true
}

type neSubEdge struct{ y0, y1, x, dx float64 } // y0 < y1; x at y0; dx per unit y

// neFillRings — even-odd scanline fill of a polygon with holes (any vertex
// count, concave allowed), pixel centres, clipped to the picture; mask as
// neFillPolyMask. Edges are sorted once and swept with an active list, so a
// 10 000-vertex Alm outline costs its perimeter, not perimeter × rows.
func neFillRings(img *image.Paletted, rings [][][2]float64, ci uint8, mask int) {
	b := img.Bounds()
	var edges []neSubEdge
	miny, maxy := math.Inf(1), math.Inf(-1)
	for _, p := range rings {
		n := len(p)
		for i := 0; i < n; i++ {
			a, c := p[i], p[(i+1)%n]
			if a[1] == c[1] {
				continue
			}
			if a[1] > c[1] {
				a, c = c, a
			}
			edges = append(edges, neSubEdge{a[1], c[1], a[0], (c[0] - a[0]) / (c[1] - a[1])})
			miny, maxy = math.Min(miny, a[1]), math.Max(maxy, c[1])
		}
	}
	if len(edges) == 0 {
		return
	}
	sort.Slice(edges, func(i, j int) bool { return edges[i].y0 < edges[j].y0 })
	y0, y1 := int(math.Ceil(miny-0.5)), int(math.Floor(maxy-0.5))
	if y0 < b.Min.Y {
		y0 = b.Min.Y
	}
	if y1 >= b.Max.Y {
		y1 = b.Max.Y - 1
	}
	var active []int
	next := 0
	xs := make([]float64, 0, 16)
	for y := y0; y <= y1; y++ {
		sy := float64(y) + 0.5
		for next < len(edges) && edges[next].y0 <= sy {
			active = append(active, next)
			next++
		}
		xs = xs[:0]
		k := 0
		for _, ei := range active {
			e := &edges[ei]
			if e.y1 <= sy {
				continue // finished: drop
			}
			active[k] = ei
			k++
			if e.y0 <= sy {
				xs = append(xs, e.x+(sy-e.y0)*e.dx)
			}
		}
		active = active[:k]
		if len(xs) < 2 {
			continue
		}
		sort.Float64s(xs)
		row := img.Pix[(y-b.Min.Y)*img.Stride:]
		for q := 0; q+1 < len(xs); q += 2 {
			xa, xb := int(math.Ceil(xs[q]-0.5)), int(math.Floor(xs[q+1]-0.5))
			if xa < b.Min.X {
				xa = b.Min.X
			}
			if xb >= b.Max.X {
				xb = b.Max.X - 1
			}
			for x := xa; x <= xb; x++ {
				if mask == 0 || (mask == 1 && (x+y)&3 < 2) || (mask == 2 && ((x>>1)+(y>>1))&1 == 0) {
					row[x-b.Min.X] = ci
				}
			}
		}
	}
}

// neStrokeRings — every ring edge as a line; edges wholly off the picture
// are skipped (unclipped outlines reach far past a tile).
func neStrokeRings(img *image.Paletted, rings [][][2]float64, ci uint8, width, dash int) {
	b := img.Bounds()
	x0f, y0f, x1f, y1f := float64(b.Min.X)-1, float64(b.Min.Y)-1, float64(b.Max.X)+1, float64(b.Max.Y)+1
	for _, p := range rings {
		n := len(p)
		for i := 0; i < n; i++ {
			a, c := p[i], p[(i+1)%n]
			if (a[0] < x0f && c[0] < x0f) || (a[0] > x1f && c[0] > x1f) || (a[1] < y0f && c[1] < y0f) || (a[1] > y1f && c[1] > y1f) {
				continue
			}
			neDrawLine(img, a[0], a[1], c[0], c[1], ci, width, dash)
		}
	}
}

// shades of the sub palette used for the outlines (1..8 = alpha .3 → .9). It
// is an overlay: the declared / change picture must stay readable through
// every fill, so nothing goes past ≈ .64 and the outline edge carries the shape.
const (
	neSubShadeClaim   = 3 // declared view: a Schlag as drawn (≈ .47)
	neSubShadeNew     = 5 // change view: new claim (≈ .64) — the strongest, like a confirmed change cell
	neSubShadeRedrawn = 3 // change view: outline redrawn (≈ .47)
	neSubShadeGone    = 2 // change view: gone, hatched (≈ .39)
)

// neGlyphSubPaintPolys — paint the outline bins of a tile. bins = (i, j,
// polys) covering the padded box; w..n = the tile's own half-open box for
// the counts. Returns false when nothing could be painted (caller falls
// back to the hexagons).
func neGlyphSubPaintPolys(img *image.Paletted, g *neGlyphTileGeom, bins []neSubPolyBin, w, s, e, n float64, strokes bool, m *neGlyphSubMeta) {
	var buf [][][2]float64
	for _, pb := range bins {
		mp := neSubPolyMapOf(g, pb.i, pb.j)
		m.year, m.prev, m.rel = pb.p.Year, pb.p.Prev, pb.p.Release
		for k := range pb.p.Recs {
			r := &pb.p.Recs[k]
			if len(r.Rings) == 0 {
				continue
			}
			// counts: once per Schlag — in the tile whose box holds its first vertex,
			// from the bin that holds that vertex (a Schlag touching two bins is in both)
			if r.Flags&2 == 0 {
				p0 := r.Rings[0][0]
				if p0[0] >= 0 && p0[0] < neSubPolyUnits && p0[1] >= 0 && p0[1] < neSubPolyUnits {
					lon := (float64(pb.i) + float64(p0[0])/neSubPolyUnits) * neSubBinDeg
					lat := (float64(pb.j) + float64(p0[1])/neSubPolyUnits) * neSubBinDeg
					if lon >= w && lon < e && lat >= s && lat < n {
						m.pchg[r.Chg]++
						if r.Chg != 5 {
							m.pn++
							m.pha += r.Area
						}
						switch r.Chg {
						case 4:
							m.pnew += r.Area
						case 5:
							m.pgone -= r.DArea
						}
					}
				}
			}
			m.poly = true
			var ok bool
			if strokes {
				// lineage vs the year before, filled like the vermilion change cells but in the
				// layer's ochre: new = full wash + 2 px edge, redrawn = half wash + hairline,
				// gone = pale hatch + dotted edge; same = nothing
				kc := neSubKind(r.Chg)
				if kc == 0 {
					continue
				}
				if buf, ok = neSubPolyRings(img, mp, r, buf); !ok {
					continue
				}
				switch kc {
				case 1:
					neFillRings(img, buf, neSubShadeNew, 0)
					neStrokeRings(img, buf, neGlyphIdxSubStroke, 2, 0)
				case 2:
					neFillRings(img, buf, neSubShadeGone, 1)
					neStrokeRings(img, buf, neGlyphIdxSubStroke, 1, 2)
				default:
					neFillRings(img, buf, neSubShadeRedrawn, 0)
					neStrokeRings(img, buf, neGlyphIdxSubLight, 1, 0)
				}
				continue
			}
			if r.Chg == 5 {
				continue // gone: not claimed this year
			}
			if buf, ok = neSubPolyRings(img, mp, r, buf); !ok {
				continue
			}
			if r.Cls == 6 { // gloez (hedges, verges): the outline only, never a fill
				neStrokeRings(img, buf, neGlyphIdxSubStroke, 1, 0)
				continue
			}
			mask := 0
			if r.Rel < neSubRelDubious {
				mask = 1 // dubious hand-drawn outline: hatched
			}
			neFillRings(img, buf, neSubShadeClaim, mask)
			neStrokeRings(img, buf, neGlyphIdxSubStroke, 1, 0)
		}
	}
}

type neSubPolyBin struct {
	i, j int
	p    *neSubPolys
}

// neSubPolyContains — the record whose outline holds bin point (x, y)
// (even-odd over all rings, so a hole excludes).
func neSubPolyContains(r *neSubPolyRec, x, y float64) bool {
	in := false
	for _, ring := range r.Rings {
		n := len(ring)
		for i := 0; i < n; i++ {
			a, c := ring[i], ring[(i+1)%n]
			ay, cy := float64(a[1]), float64(c[1])
			if (ay <= y) == (cy <= y) {
				continue
			}
			xi := float64(a[0]) + (y-ay)*(float64(c[0])-float64(a[0]))/(cy-ay)
			if xi > x {
				in = !in
			}
		}
	}
	return in
}

// neSubSchlagAt — the Schlag under a point as the card reads it: this
// year's claim first, else a gone one of the year before; nil when none or
// when the outline bin cannot be had.
func neSubSchlagAt(lon, lat float64) map[string]interface{} {
	i, j := int(math.Floor(lon/neSubBinDeg)), int(math.Floor(lat/neSubBinDeg))
	p, err := neSubPolyAt(i, j, false)
	if err != nil {
		return nil
	}
	x := (lon/neSubBinDeg - float64(i)) * neSubPolyUnits
	y := (lat/neSubBinDeg - float64(j)) * neSubPolyUnits
	var hit, gone *neSubPolyRec
	for k := range p.Recs {
		r := &p.Recs[k]
		if !neSubPolyContains(r, x, y) {
			continue
		}
		if r.Chg == 5 {
			if gone == nil {
				gone = r
			}
			continue
		}
		if hit == nil || r.Area > hit.Area {
			hit = r
		}
	}
	if hit == nil {
		hit = gone
	}
	if hit == nil {
		return nil
	}
	ha := func(m2 int64) float64 { return math.Round(float64(m2)/100) / 100 }
	o := map[string]interface{}{
		"class": neSubClass[hit.Cls&7], "reliable": hit.Rel >= neSubRelDubious,
		"vs_prev": neSubChg[min(int(hit.Chg), 5)], "area_ha": ha(hit.Area),
		"year": p.Year, "prev_year": p.Prev,
	}
	if hit.Chg == 5 {
		o["prev_area_ha"] = ha(-hit.DArea)
	} else if hit.Chg != 4 && hit.Chg != 0 && hit.DArea != 0 {
		o["d_area_ha"] = ha(hit.DArea)
	}
	switch hit.Bio {
	case 0:
		o["bio"] = false
	case 1:
		o["bio"] = true
	default:
		o["bio"] = nil
	}
	if hit.Flags&1 != 0 {
		o["clipped"] = true
	}
	return o
}
