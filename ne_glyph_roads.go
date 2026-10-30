package main

// The ROADS overlay of the landing glyph — v=rd tiles at LOD 0/1 (docs/ne-glyph.md).
//
// OSM roads and rail from data/osm_proximity.db (the same osm_lines the public
// /api/v1/osm/geometry serves: cat road|rail, ODbL) drawn server-side in the
// app's own hand: one achromatic warm-grey ink, width = rank, a pale casing
// under every line so it reads on a dark fill or the photo as well as on
// white, service / pedestrian ways dashed, rail as ink with white ties. No hue
// carries meaning (colour-blind safe by construction); the client weighs the
// whole layer with the same opacity popover the photo has (;roads[=op]).
//
// Anti-aliasing is a signed-distance stroke: every pixel of a segment's
// bounding box gets coverage clamp(hw + .5 − dist) and a pixel keeps the MAX
// over all segments of a pass, so joins and overlaps never darken; dashes are
// cut the same way along the arc (soft ends). Passes — casing, dashed minor
// ways, rail (+ ties), roads, major roads — are composited over an NRGBA
// image. Per-line attribute hook: neRoadAttr rides on every line and
// neRoadInk(cls, attr, lod) is the one place that turns it into ink.
//
// Remoteness is drawn as WEIGHT, never hue: every way samples the 250 m
// remoteness grid (osm_remote_grid.go — distance to the nearest major road,
// transit stop and settlement, 0 central … 100 remote) along its vertices and
// the ink's alpha runs from .5× the class base in a town to 1.5× in the back
// country (plus up to half a pixel of width). So the dense web of a city stays a quiet
// grey mesh while the one track that crosses a valley far from any bus stop is
// the darkest line on the map — the information sits where it is scarce. Rail
// and the casing are not tinted. Until the grid is built (seconds after the
// first start) attr.Remote = −1 and the ink is the class base.

import (
	"image"
	"image/color"
	"math"
	"strconv"
)

// line classes (rank order; drawn last = on top)
const (
	neRoadMinor = iota // service · living_street · pedestrian · busway — dashed
	neRoadRail
	neRoadRoad // tertiary · residential · unclassified
	neRoadMajor
	neRoadClasses
)

// neRoadAttr — per-line attributes the ink may read. Remote is the remoteness
// 0–100 of the grid (osm_remote_grid.go; −1 = not attached: grid not ready or
// the way lies outside its box), the mean over the chunk's vertices.
type neRoadAttr struct {
	Remote float32
}

// neRoadRemoteGain — remoteness → ink weight multiplier (alpha, and a little
// width): .5× in a city centre (≈ 5–15), ≈ .75× in a village (≈ 20–30), 1× at
// 50, 1.5× in the back country. Linear in the score, which is already
// sqrt-normalised — checked in greyscale: the far track must be the darkest
// line on the map, the town mesh the lightest.
func neRoadRemoteGain(remote float32) float64 {
	if remote < 0 {
		return 1
	}
	return 0.5 + float64(remote)/100
}

type neRoadLine struct {
	cls  uint8
	pts  [][2]float64 // tile pixels
	attr neRoadAttr
}

// neRoadStyle — one pass of ink.
type neRoadStyle struct {
	w         float64  // stroke width, px
	a         float64  // alpha 0–1
	dash, gap float64  // 0 = solid
	rgb       [3]uint8 // the ink
}

var neRoadInkRGB = [3]uint8{0x37, 0x33, 0x2e}  // warm near-black, desaturated
var neRoadCaseRGB = [3]uint8{0xfb, 0xfa, 0xf6} // the casing: paper white

// neRoadInk — class (+ attributes) → style at this LOD. The ONLY place the
// attribute hook is read. Widths are tuned for the 872-px tiles: LOD 1 ≈ .9 m/px,
// LOD 0 ≈ 2.1 m/px (scaled .8 so a residential road stays a hairline there).
// Roads (not rail) carry the remoteness weight: alpha × neRoadRemoteGain,
// width + up to .5 px toward the remote end; alpha is clamped to .92 so a
// remote major road never turns to solid black.
func neRoadInk(cls uint8, attr neRoadAttr, lod int) neRoadStyle {
	k := 1.0
	if lod == 0 {
		k = 0.8
	}
	s := neRoadStyle{rgb: neRoadInkRGB}
	switch cls {
	case neRoadMajor:
		s.w, s.a = 2.6*k, 0.66
	case neRoadRoad:
		s.w, s.a = 1.7*k, 0.52
	case neRoadRail:
		s.w, s.a = 1.5*k, 0.58
	default: // minor: dashed hairline
		s.w, s.a, s.dash, s.gap = 1.15*k, 0.46, 5.5*k, 3.5*k
		if lod == 0 {
			s.a = 0.38
		}
	}
	if cls != neRoadRail && attr.Remote >= 0 {
		gn := neRoadRemoteGain(attr.Remote)
		s.a = math.Min(0.92, s.a*gn)
		s.w += 0.5 * k * math.Max(0, gn-1) / 0.5
	}
	return s
}

// neRoadClassOf — osm_lines (cat, major, fclass) → class; false = not drawn.
func neRoadClassOf(cat string, major int, fclass string) (uint8, bool) {
	switch cat {
	case "rail":
		return neRoadRail, true
	case "road":
		if major == 1 {
			return neRoadMajor, true
		}
		switch fclass {
		case "service", "living_street", "pedestrian", "busway":
			return neRoadMinor, true
		}
		return neRoadRoad, true
	}
	return 0, false
}

// neGlyphRoadsMeta — what rides in X-NE-Tile for v=rd.
type neGlyphRoadsMeta struct {
	n    int                // lines drawn
	cls  [neRoadClasses]int // per class
	km   float64            // length inside the tile box, km (clipped, so tiles add up)
	rmKm float64            // road km (not rail) that carry a remoteness value
	rmW  float64            // Σ remoteness × km over those
	rmb  [4]float64         // road km by remoteness band: <25 · 25–50 · 50–75 · ≥75
	down bool               // the OSM db is not open
}

// remote — the length-weighted mean remoteness of the roads in the tile, −1 without any.
func (m *neGlyphRoadsMeta) remote() float64 {
	if m.rmKm <= 0 {
		return -1
	}
	return m.rmW / m.rmKm
}

func (m *neGlyphRoadsMeta) String() string {
	if m.down {
		return ";rd=0;down=1"
	}
	s := ";rd=" + strconv.Itoa(m.n) + ";rdc=" + strconv.Itoa(m.cls[neRoadMajor]) + "," + strconv.Itoa(m.cls[neRoadRoad]) + "," + strconv.Itoa(m.cls[neRoadMinor]) + "," + strconv.Itoa(m.cls[neRoadRail]) + ";km=" + strconv.FormatFloat(m.km, 'f', 1, 64)
	if r := m.remote(); r >= 0 {
		// rm = length-weighted mean remoteness of the roads in the box · rmb = road km by band <25,25–50,50–75,≥75
		s += ";rm=" + strconv.Itoa(int(r+0.5)) + ";rmb=" + strconv.FormatFloat(m.rmb[0], 'f', 1, 64) + "," + strconv.FormatFloat(m.rmb[1], 'f', 1, 64) + "," + strconv.FormatFloat(m.rmb[2], 'f', 1, 64) + "," + strconv.FormatFloat(m.rmb[3], 'f', 1, 64)
	}
	return s
}

// neRoadLoad — the lines of the box (plus a margin for the stroke width) as tile-pixel polylines.
func neRoadLoad(g *neGlyphTileGeom, w, s, e, n float64, m *neGlyphRoadsMeta) []neRoadLine {
	if osmDB == nil {
		m.down = true
		return nil
	}
	mx := 6.0 * (e - w) / float64(g.pw) // 6 px in degrees
	my := 6.0 * (n - s) / float64(g.ph)
	rows, err := osmDB.Query(`
		SELECT l.cat, l.major, l.fclass, l.coords
		FROM osm_lines_rtree rt JOIN osm_lines l ON l.id = rt.id
		WHERE rt.max_lon >= ? AND rt.min_lon <= ? AND rt.max_lat >= ? AND rt.min_lat <= ?
		  AND l.cat IN ('road','rail')`, w-mx, e+mx, s-my, n+my)
	if err != nil {
		m.down = true
		return nil
	}
	defer rows.Close()
	mLon, mLat := metresPerDeg((s + n) / 2)
	rg := remoteGridGet()
	var out []neRoadLine
	for rows.Next() {
		var cat, fclass string
		var major int
		var coords []byte
		if err := rows.Scan(&cat, &major, &fclass, &coords); err != nil {
			continue
		}
		cls, ok := neRoadClassOf(cat, major, fclass)
		if !ok {
			continue
		}
		pts := unpackCoords(coords)
		if len(pts) < 2 {
			continue
		}
		ln := neRoadLine{cls: cls, pts: make([][2]float64, len(pts)), attr: neRoadAttr{Remote: -1}}
		var rsum float32
		rn, inKm := 0, 0.0
		for i, p := range pts {
			px, py := neGlyphProj(p[0], p[1])
			ln.pts[i] = [2]float64{(px - g.x0) * g.lod.px, (py - g.y0) * g.lod.px}
			if i > 0 {
				inKm += clipLenM(pts[i-1][0], pts[i-1][1], p[0], p[1], w, s, e, n, mLon, mLat) / 1000
			}
			if rg != nil && cls != neRoadRail {
				if r := rg.At(p[0], p[1]); r >= 0 {
					rsum += r
					rn++
				}
			}
		}
		m.km += inKm
		if rn > 0 {
			ln.attr.Remote = rsum / float32(rn)
			m.rmKm += inKm
			m.rmW += float64(ln.attr.Remote) * inKm
			b := int(ln.attr.Remote / 25)
			if b > 3 {
				b = 3
			}
			m.rmb[b] += inKm
		}
		m.n++
		m.cls[cls]++
		out = append(out, ln)
	}
	return out
}

// clipLenM — length in metres of the segment part inside the lon/lat box (Liang–Barsky).
func clipLenM(x0, y0, x1, y1, w, s, e, n, mLon, mLat float64) float64 {
	dx, dy := x1-x0, y1-y0
	t0, t1 := 0.0, 1.0
	clip := func(p, q float64) bool {
		if p == 0 {
			return q >= 0
		}
		r := q / p
		if p < 0 {
			if r > t1 {
				return false
			}
			if r > t0 {
				t0 = r
			}
		} else {
			if r < t0 {
				return false
			}
			if r < t1 {
				t1 = r
			}
		}
		return true
	}
	if !clip(-dx, x0-w) || !clip(dx, e-x0) || !clip(-dy, y0-s) || !clip(dy, n-y0) {
		return 0
	}
	return math.Hypot(dx*(t1-t0)*mLon, dy*(t1-t0)*mLat)
}

// neRoadStroke — signed-distance stroke of one polyline into the coverage
// buffer (max-blend). dash/gap in px along the arc, 0 = solid; gain scales the
// coverage (0–1) so lines of one pass may carry different weights — at a join a
// heavier line simply wins.
func neRoadStroke(cov []float32, pw, ph int, pts [][2]float64, hw, dash, gap, gain float64) {
	pos := 0.0
	period := dash + gap
	fw, fh := float64(pw), float64(ph)
	r := hw + 1 // bbox pad: half width + AA ramp
	for i := 1; i < len(pts); i++ {
		ax, ay, bx, by := pts[i-1][0], pts[i-1][1], pts[i][0], pts[i][1]
		dx, dy := bx-ax, by-ay
		l2 := dx*dx + dy*dy
		seg := math.Sqrt(l2)
		if seg < 1e-6 {
			continue
		}
		x0, x1 := math.Min(ax, bx)-r, math.Max(ax, bx)+r
		y0, y1 := math.Min(ay, by)-r, math.Max(ay, by)+r
		if x1 < 0 || y1 < 0 || x0 > fw || y0 > fh {
			pos += seg
			continue
		}
		ix0, ix1 := int(math.Max(0, math.Floor(x0))), int(math.Min(fw-1, math.Ceil(x1)))
		iy0, iy1 := int(math.Max(0, math.Floor(y0))), int(math.Min(fh-1, math.Ceil(y1)))
		for py := iy0; py <= iy1; py++ {
			cy := float64(py) + 0.5
			row := py * pw
			for px := ix0; px <= ix1; px++ {
				cx := float64(px) + 0.5
				t := ((cx-ax)*dx + (cy-ay)*dy) / l2
				if t < 0 {
					t = 0
				} else if t > 1 {
					t = 1
				}
				qx, qy := ax+t*dx-cx, ay+t*dy-cy
				c := hw + 0.5 - math.Sqrt(qx*qx+qy*qy)
				if c <= 0 {
					continue
				}
				if c > 1 {
					c = 1
				}
				if period > 0 {
					// signed distance to the dash boundary along the arc: + inside a dash
					u := math.Mod(pos+t*seg, period)
					var sd float64
					if u < dash {
						sd = math.Min(u, dash-u)
					} else {
						sd = -math.Min(u-dash, period-u)
					}
					dc := sd + 0.5
					if dc <= 0 {
						continue
					}
					if dc < 1 {
						c *= dc
					}
				}
				if f := float32(c * gain); f > cov[row+px] {
					cov[row+px] = f
				}
			}
		}
		pos += seg
	}
}

// neRoadComposite — src-over of (rgb, alpha × coverage) onto the NRGBA image; clears cov.
func neRoadComposite(img *image.NRGBA, cov []float32, rgb [3]uint8, alpha float64) {
	sr, sg, sb := float64(rgb[0]), float64(rgb[1]), float64(rgb[2])
	for p, c := range cov {
		if c <= 0 {
			continue
		}
		cov[p] = 0
		as := float64(c) * alpha
		o := p * 4
		ad := float64(img.Pix[o+3]) / 255
		oa := as + ad*(1-as)
		if oa <= 0 {
			continue
		}
		wd := ad * (1 - as) / oa
		ws := as / oa
		img.Pix[o] = uint8(sr*ws + float64(img.Pix[o])*wd + 0.5)
		img.Pix[o+1] = uint8(sg*ws + float64(img.Pix[o+1])*wd + 0.5)
		img.Pix[o+2] = uint8(sb*ws + float64(img.Pix[o+2])*wd + 0.5)
		img.Pix[o+3] = uint8(oa*255 + 0.5)
	}
}

// neRoadPalette — every pixel of the picture is a mix of the two inks at some
// alpha, so the PNG goes out paletted: 32 alpha steps × 8 ink/casing mixes
// (index = a*8 + mix). ~5× smaller than the NRGBA it is composed in, the AA
// ramp keeps 32 steps.
var neRoadPalette = func() color.Palette {
	p := make(color.Palette, 256)
	for a := 0; a < 32; a++ {
		for mix := 0; mix < 8; mix++ {
			f := float64(mix) / 7
			c := color.NRGBA{
				uint8(float64(neRoadCaseRGB[0])*(1-f) + float64(neRoadInkRGB[0])*f + 0.5),
				uint8(float64(neRoadCaseRGB[1])*(1-f) + float64(neRoadInkRGB[1])*f + 0.5),
				uint8(float64(neRoadCaseRGB[2])*(1-f) + float64(neRoadInkRGB[2])*f + 0.5),
				uint8(a * 255 / 31)}
			if a == 0 {
				c = color.NRGBA{}
			}
			p[a*8+mix] = c
		}
	}
	return p
}()

// neRoadQuantize — NRGBA → the road palette (alpha 32 steps, ink share 8 steps).
func neRoadQuantize(src *image.NRGBA) *image.Paletted {
	dst := image.NewPaletted(src.Rect, neRoadPalette)
	span := float64(neRoadCaseRGB[0]) - float64(neRoadInkRGB[0])
	for p := 0; p < len(dst.Pix); p++ {
		o := p * 4
		a := int(src.Pix[o+3])
		if a < 4 {
			continue
		}
		ai := (a*31 + 127) / 255
		if ai == 0 {
			ai = 1
		}
		f := (float64(neRoadCaseRGB[0]) - float64(src.Pix[o])) / span
		mi := int(f*7 + 0.5)
		if mi < 0 {
			mi = 0
		} else if mi > 7 {
			mi = 7
		}
		dst.Pix[p] = uint8(ai*8 + mi)
	}
	return dst
}

// neGlyphRoadsRender — the v=rd picture of one tile.
func neGlyphRoadsRender(g *neGlyphTileGeom, w, s, e, n float64, m *neGlyphRoadsMeta) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, g.pw, g.ph))
	lines := neRoadLoad(g, w, s, e, n, m)
	if len(lines) == 0 {
		return image.NewPaletted(img.Rect, neRoadPalette)
	}
	lod := lodIndex(g.lod)
	cov := make([]float32, g.pw*g.ph)
	// 1 · casing: every line, a touch wider than its ink, paper-white, soft
	for _, ln := range lines {
		st := neRoadInk(ln.cls, ln.attr, lod)
		neRoadStroke(cov, g.pw, g.ph, ln.pts, st.w/2+1.1, 0, 0, 1)
	}
	neRoadComposite(img, cov, neRoadCaseRGB, 0.34)
	// 2 · ink by class, lowest rank first; each line's alpha (the remoteness
	// weight) rides in the coverage, so one composite per class still does
	for cls := uint8(0); cls < neRoadClasses; cls++ {
		var st neRoadStyle
		any := false
		for _, ln := range lines {
			if ln.cls != cls {
				continue
			}
			st = neRoadInk(cls, ln.attr, lod)
			neRoadStroke(cov, g.pw, g.ph, ln.pts, st.w/2, st.dash, st.gap, st.a)
			any = true
		}
		if !any {
			continue
		}
		neRoadComposite(img, cov, st.rgb, 1)
		if cls == neRoadRail && lod != 0 {
			// white ties over the ink (near tiles only — at LOD 0 a yard would turn to noise): short dashes narrower than the rail
			for _, ln := range lines {
				if ln.cls == cls {
					neRoadStroke(cov, g.pw, g.ph, ln.pts, st.w*0.28, 3*st.w, 3*st.w, 1)
				}
			}
			neRoadComposite(img, cov, neRoadCaseRGB, 0.9)
		}
	}
	return neRoadQuantize(img)
}
