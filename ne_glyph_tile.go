// ne_glyph_tile.go — the landing glyph's "full resolution" layer: the
// declared land-use product drawn at its native H3 res 12, one hexagon per
// cell coloured by its dominant land-use group (second group as an inner
// hexagon when it holds ≥ 25 % of the land). Public, tokenless, no SQL.
//
//	GET /api/v1/ne/glyph/tile?lod=0|1|2&i=&j=[&v=obs|tree|chg|sub|subc]  PNG tile (lon/lat grid, see neGlyphLODs; f= is accepted and ignored since 2026-10-30)
//	GET /api/v1/ne/glyph/cell?lon=&lat=                  the res-12 cell under a point: public
//	                                                     cell doc + its hexagon in viewBox units
//
// Rule: the tile is a picture of the same rows /ne/cells serves — the
// colours are a function of lu[9] only (ns_groups order), nothing else is
// read; the declared row is the KG's `bev` row (the peer's current bevdirect
// build, the cell set its obs rows carry) and the index-epoch `lu` row only
// where bev lacks the cell (neReq.bevFirst — a cell the March index never
// held, e.g. 63235 parcel 27, is otherwise a hole in BOTH pictures, since
// obs is joined onto the declared ids); cells whose declared use is unknown get one neutral colour so no
// held cell is ever blank. The two view overlays paint the same hexagons:
// v=obs — the OBSERVED land use {OBS_NAME} posted for the cell
// (obs_lu[9] of the NEO1 row, same palette and dominance/second-group rule
// as the declared picture, so declared and observed read side by side;
// cells without an observed row stay transparent and the dimmed declared
// base shows through; the tile meta adds the trees: count, tallest, and a
// histogram of the tallest tree per cell, QA-filtered like srtm's own tree
// index — neObsTrees); v=tree — the canopy layer over the observed picture:
// each wooded cell in the observed teal, deeper with the band of its
// tallest tree (neGlyphTreeAlpha, the legend's ramp); cells without a
// QA-passing tree stay transparent so their observed use shows; same meta
// as v=obs; v=chg — the cell's cumulative change state
// (confirmed / candidate) from the chunk log.
//
//	GET /api/v1/ne/glyph/kg?kg=                          the KG's outline at native res 12 (the union of its
//	                                                     majority cells) as an SVG path in viewBox units — the
//	                                                     full-resolution veil mask, exact instead of res-7 hexagons.
//
// Rendered tiles and outlines sit in one bounded LRU (size line ne_glyph_tiles).
package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/uber/h3-go/v4"
)

// neGlyphLOD — a tile grid in degrees + pixel density (px per viewBox unit;
// 1 unit ≈ 87 m). LOD 0 for the overview of a few km, LOD 1 once single
// cells should read (a res-12 hexagon ≈ 16 m across → 19 px).
type neGlyphLOD struct {
	deg float64
	px  float64
}

var neGlyphLODs = []neGlyphLOD{{0.025, 40}, {0.01, 100}, {0.1, 10}}

// neGlyphGroups — ns_groups GROUP_ORDER, the index of lu[9] / obs_lu[9].
var neGlyphGroups = [9]string{"geb", "bau", "acker", "gruen", "wald", "wasser", "verkehr", "alpen", "sonst"}

// group colours — the landing page's g-* palette (ns_groups GROUP_ORDER).
var neGlyphGroupRGB = [9][3]uint8{
	{0x3d, 0x3d, 0x3d}, // geb
	{0xc4, 0xa4, 0x84}, // bau
	{0xe8, 0xc8, 0x4a}, // acker
	{0x9f, 0xd3, 0x7a}, // gruen
	{0x27, 0x6b, 0x38}, // wald
	{0x4f, 0x93, 0xd6}, // wasser
	{0x8c, 0x8c, 0x8c}, // verkehr
	{0xe8, 0xdc, 0xcf}, // alpen
	{0xc0, 0xc4, 0xc8}, // sonst
}

const neGlyphTileShades = 4 // alpha steps per group (dominance)

// neGlyphTileAlphaMin — the faintest dominance shade; high enough that the
// light groups (alpine, other) never read as blank cells.
const neGlyphTileAlphaMin = 0.62

// neGlyphUnknownRGB — cells held but with no declared use (cov = 0,
// declared_unknown): one neutral colour, listed in the legend as such.
var neGlyphUnknownRGB = [3]uint8{0xd6, 0xd2, 0xca}

// overlay colours = the page's --chg / --obs (light scheme).
var neGlyphChgRGB = [3]uint8{0xc2, 0x41, 0x0c}
var neGlyphAshRGB = [3]uint8{0x8a, 0x86, 0x82} // the muted ink old change ashes toward
var neGlyphObsRGB = [3]uint8{0x17, 0x8a, 0x7a}

// v=tree — the whole cell in the observed teal at the legend's ramp
// (g-tr0…4: opacity .4 · .58 · .74 · .88 · 1 over the page), pre-blended
// over white so the map matches the swatches exactly and every wooded cell
// reads as teal while the observed picture under the rest stays untouched; bands
// < 5 · 5–15 · 15–25 · 25–35 · 35+ m.
var neGlyphTreeAlpha = [5]float64{0.4, 0.58, 0.74, 0.88, 1}

// palette indexes of the overlays (after transparent 0)
const (
	neGlyphIdxUnknown  = 1 + 9*neGlyphTileShades
	neGlyphIdxFaded    = neGlyphIdxUnknown + 1 // + group: superseded cells (index/bev row only), faded — never blank
	neGlyphFadedAlpha  = 0.28
	neGlyphIdxChgCand  = 1 // v=chg: candidate (one sighting)
	neGlyphIdxChgConf  = 2 // v=chg: confirmed, small move
	neGlyphIdxChgConf2 = 3 // v=chg: confirmed, ≥ 10 % of the cell moved
	neGlyphAgeSteps    = 8 // v=chg: age steps per tone (fresh → ashed)
	neGlyphAgeFull     = 45.0
	neGlyphAgeGone     = 365.0
	neGlyphIdxObs      = 1 // v=obs
)

var neGlyphTileLRU = newNELRU(24 << 20) // ~24 MB ≈ 150–300 tiles

// neGlyphTilePalette — transparent + 9 groups × 4 shades + unknown.
func neGlyphTilePalette() color.Palette {
	p := color.Palette{color.NRGBA{0, 0, 0, 0}}
	for g := 0; g < 9; g++ {
		for s := 0; s < neGlyphTileShades; s++ {
			a := neGlyphTileAlphaMin + (1-neGlyphTileAlphaMin)*float64(s)/float64(neGlyphTileShades-1)
			c := neGlyphGroupRGB[g]
			p = append(p, color.NRGBA{c[0], c[1], c[2], uint8(math.Round(255 * a))})
		}
	}
	u := neGlyphUnknownRGB
	p = append(p, color.NRGBA{u[0], u[1], u[2], 255})
	for g := 0; g < 9; g++ { // faded: a superseded cell keeps its colour at low alpha
		c := neGlyphGroupRGB[g]
		p = append(p, color.NRGBA{c[0], c[1], c[2], uint8(math.Round(255 * neGlyphFadedAlpha))})
	}
	return p
}

// neGlyphOverlayPalette — transparent + the view's shades.
func neGlyphOverlayPalette(view string) color.Palette {
	p := color.Palette{color.NRGBA{0, 0, 0, 0}}
	if view == "obs" {
		c := neGlyphObsRGB
		return append(p, color.NRGBA{c[0], c[1], c[2], 190})
	}
	if view == "tree" {
		c := neGlyphObsRGB
		for _, a := range neGlyphTreeAlpha {
			bl := func(v uint8) uint8 { return uint8(math.Round(255 - (255-float64(v))*a)) }
			p = append(p, color.NRGBA{bl(c[0]), bl(c[1]), bl(c[2]), 255})
		}
		return p
	}
	// v=chg: three tones (candidate · confirmed · confirmed, ≥ 10 % moved) ×
	// neGlyphAgeSteps age steps — fresh change in the signal red, ashing out
	// toward the muted ink as it ages (neGlyphAge), never below the floor.
	c := neGlyphChgRGB
	for _, a := range [3]uint8{120, 205, 255} {
		for st := 0; st < neGlyphAgeSteps; st++ {
			age := 1 - float64(st)/float64(neGlyphAgeSteps-1)
			mix := func(v, m uint8) uint8 { return uint8(math.Round(float64(v)*age + float64(m)*(1-age))) }
			p = append(p, color.NRGBA{mix(c[0], neGlyphAshRGB[0]), mix(c[1], neGlyphAshRGB[1]), mix(c[2], neGlyphAshRGB[2]), uint8(math.Round(float64(a) * (0.18 + 0.82*age)))})
		}
	}
	return p
}

// neGlyphAgeScale — the age scale adapts to how much history there is
// (same rule as the glyph's tlScale): with a year of change behind the head
// a change stays bright for six weeks and ashes out over the year; while the
// record is young the span itself is the scale (full = 12 % of it, ≥ 1 day;
// gone = the span, ≥ 10 days), so the early weeks already read as history.
// span = head − first day with change in the daily tracker.
func neGlyphAgeScale(head string) (full, gone float64) {
	full, gone = neGlyphAgeFull, neGlyphAgeGone
	first := neChangeEpochDay()
	if len(first) < 10 || len(head) < 10 {
		return
	}
	f, e1 := time.Parse("2006-01-02", first[:10])
	h, e2 := time.Parse("2006-01-02", head[:10])
	if e1 != nil || e2 != nil {
		return
	}
	span := h.Sub(f).Hours() / 24
	if span < neGlyphAgeGone {
		gone = math.Max(10, span)
		full = math.Max(1, math.Min(neGlyphAgeFull, 0.12*span))
	}
	return
}

// neChangeEpochDay — the first day the daily tracker saw a changed chunk
// (memoised with the report scan).
func neChangeEpochDay() string {
	// StatsMemo (stale-while-revalidate): tile renders never wait on the
	// report scan's lock; the epoch moves only when the first logs appear
	m := StatsMemo("ne_change_epoch", 30*time.Minute, false, func() map[string]interface{} {
		daily, _ := neScanReportFiles()["daily"].([]neDayStat)
		for _, d := range daily {
			if d.Changed > 0 {
				return map[string]interface{}{"day": d.Day}
			}
		}
		if len(daily) > 0 {
			return map[string]interface{}{"day": daily[0].Day}
		}
		return map[string]interface{}{"day": ""}
	})
	d, _ := m["day"].(string)
	return d
}

// neGlyphAge — how fresh a change is at the head of the window: 1 up to
// `full` days, easing (smoothstep) to 0 at `gone` (same curve as the glyph's tlAge).
func neGlyphAge(day, head string, full, gone float64) float64 {
	if len(day) < 10 || len(head) < 10 {
		return 1
	}
	d, e1 := time.Parse("2006-01-02", day[:10])
	h, e2 := time.Parse("2006-01-02", head[:10])
	if e1 != nil || e2 != nil {
		return 1
	}
	a := h.Sub(d).Hours() / 24
	if a <= full {
		return 1
	}
	if a >= gone {
		return 0
	}
	x := (a - full) / (gone - full)
	return 1 - x*x*(3-2*x)
}

// neGlyphChgIdx — palette index of a change tone at an age.
func neGlyphChgIdx(tone int, age float64) uint8 {
	st := int(math.Round((1 - age) * float64(neGlyphAgeSteps-1)))
	return uint8(1 + (tone-1)*neGlyphAgeSteps + st)
}

// neGlyphHead — the head of a change window: its last day, else today.
func neGlyphHead(win neChangeWin) string {
	if win.D2 != "" {
		return win.D2
	}
	return time.Now().UTC().Format("2006-01-02")
}

func neGlyphTileIdx(g, shade int) uint8 { return uint8(1 + g*neGlyphTileShades + shade) }

// neGlyphProj — lon/lat → viewBox units (the frame's forward projection).
func neGlyphProj(lon, lat float64) (float64, float64) {
	fr := &neGlyphFrame
	return (lon*fr.kx-fr.minx)*fr.scale + neGlyphPad, (-lat-fr.miny)*fr.scale + neGlyphPad
}

// neGlyphTileBox — the tile's lon/lat box.
func neGlyphTileBox(lod neGlyphLOD, i, j int) (w, s, e, n float64) {
	return float64(i) * lod.deg, float64(j) * lod.deg, float64(i+1) * lod.deg, float64(j+1) * lod.deg
}

// neGlyphTileBleed — the picture carries one extra pixel ring so stretched
// edges never show a seam; the client widens the image rect by 1/px.
const neGlyphTileBleed = 1

// neGlyphTileMeta — what rides in X-NE-Tile beside the picture: cell count,
// per-group land-use sums (1/255 cell-area units), unknown cells, pending
// KGs; for the overlays the counts they painted (obs; chg confirmed /
// candidate) and, for v=chg, the signed Δ land use per group (m²).
type neGlyphTileMeta struct {
	n        int
	lus      [9]int64
	unknown  int
	sup      int // declared base: superseded cells painted faded (index/bev row, no logged build holds the cell)
	pending  []string
	obs      int
	trees    int64  // v=obs: Σ tree_n over the observed cells
	treeMax  int    // v=obs: tallest tree in the tile, 0.5 m units
	treeHist [5]int // v=obs: cells by tallest tree: <5 · 5–15 · 15–25 · 25–35 · 35+ m
	conf     int
	cand     int
	dlu      [9]int64
	first    string // v=chg: earliest / latest digest timestamp among the painted cells (day)
	last     string
	win      neChangeWin      // v=chg: the window the tile was painted for (echoed as win=)
	sub      neGlyphSubMeta   // v=sub|subc
	dif      neGlyphDifMeta   // v=dif
	rd       neGlyphRoadsMeta // v=rd
}

func (m *neGlyphTileMeta) String(ms int64, view string) string {
	lu := make([]string, 9)
	for g := range m.lus {
		lu[g] = strconv.FormatInt(m.lus[g], 10)
	}
	s := fmt.Sprintf("n=%d;ms=%d;lu=%s;unk=%d;pending=%s", m.n, ms, strings.Join(lu, ","), m.unknown, strings.Join(m.pending, ","))
	if m.sup > 0 {
		s += fmt.Sprintf(";sup=%d", m.sup)
	}
	switch view {
	case "obs", "tree":
		th := make([]string, 5)
		for b := range m.treeHist {
			th[b] = strconv.Itoa(m.treeHist[b])
		}
		s += fmt.Sprintf(";obs=%d;trees=%d;tmax=%d;th=%s", m.obs, m.trees, m.treeMax, strings.Join(th, ","))
	case "chg":
		d := make([]string, 9)
		for g := range m.dlu {
			d[g] = strconv.FormatInt(m.dlu[g], 10)
		}
		s += fmt.Sprintf(";chg=%d,%d;dlu=%s", m.conf, m.cand, strings.Join(d, ","))
		if m.first != "" {
			s += ";at=" + m.first + "," + m.last
		}
		if !m.win.zero() {
			s += ";win=" + m.win.String()
		}
	case "sub", "subc":
		s += m.sub.String()
	case "dif":
		s += fmt.Sprintf(";obs=%d", m.obs) + m.dif.String()
	case "rd":
		s += m.rd.String()
	}
	return s
}

// neGlyphTileGeom — the tile's pixel frame + a cell → polygon mapper.
type neGlyphTileGeom struct {
	lod    neGlyphLOD
	x0, y0 float64
	pw, ph int
}

func neGlyphTileGeomOf(lod neGlyphLOD, i, j int) (g neGlyphTileGeom, w, s, e, n float64, ok bool) {
	w, s, e, n = neGlyphTileBox(lod, i, j)
	x0, y0 := neGlyphProj(w, n) // top-left in viewBox units (y grows south)
	x1, y1 := neGlyphProj(e, s)
	pw, ph := int(math.Ceil((x1-x0)*lod.px))+2*neGlyphTileBleed, int(math.Ceil((y1-y0)*lod.px))+2*neGlyphTileBleed
	if pw < 1 || ph < 1 || pw*ph > 4<<20 {
		return g, w, s, e, n, false
	}
	return neGlyphTileGeom{lod: lod, x0: x0 - neGlyphTileBleed/lod.px, y0: y0 - neGlyphTileBleed/lod.px, pw: pw, ph: ph}, w, s, e, n, true
}

// poly — the cell's hexagon in tile pixels (+ centroid); false when H3 fails.
func (g *neGlyphTileGeom) poly(id uint64, poly *[8][2]float64) (np int, cx, cy float64, ok bool) {
	bd, err := h3.Cell(id).Boundary()
	if err != nil || len(bd) < 3 {
		return 0, 0, 0, false
	}
	np = len(bd)
	if np > 8 {
		np = 8
	}
	for v := 0; v < np; v++ {
		px, py := neGlyphProj(bd[v].Lng, bd[v].Lat)
		poly[v] = [2]float64{(px - g.x0) * g.lod.px, (py - g.y0) * g.lod.px}
		cx += poly[v][0]
		cy += poly[v][1]
	}
	return np, cx / float64(np), cy / float64(np), true
}

// neGlyphTileRender — the PNG and its meta for one tile of a view.
func neGlyphTileRender(lod neGlyphLOD, i, j int, view string) ([]byte, neGlyphTileMeta) {
	return neGlyphTileRenderOpt(lod, i, j, view, false)
}

// neGlyphTileRenderOpt — raw: v=dif without the corroboration filters (§6d).
func neGlyphTileRenderOpt(lod neGlyphLOD, i, j int, view string, raw bool) ([]byte, neGlyphTileMeta) {
	return neGlyphTileRenderWin(lod, i, j, view, raw, neChangeWin{})
}

// neGlyphTileRenderWin — win: v=chg restricted to a pass / detection-day window (ne_change_passes.go).
func neGlyphTileRenderWin(lod neGlyphLOD, i, j int, view string, raw bool, win neChangeWin) ([]byte, neGlyphTileMeta) {
	var m neGlyphTileMeta
	m.win = win
	g, w, s, e, n, ok := neGlyphTileGeomOf(lod, i, j)
	if !ok {
		return nil, m
	}
	kgs := KGCodesInBBoxMem(w, s, e, n)
	m.pending = nePendingKGs(kgs)
	const padDeg = 0.0005 // cells whose centre sits just across the edge still paint their half of the seam
	ids := neBBoxCells(w-padDeg, s-padDeg, e+padDeg, n+padDeg)
	var img image.Image
	var poly [8][2]float64
	switch view {
	case "rd":
		// OSM roads + rail in the app's hand (ne_glyph_roads.go); no cells are read
		img = neGlyphRoadsRender(&g, w, s, e, n, &m.rd)
		if m.rd.down {
			return nil, m
		}
	case "sub", "subc":
		// the subsidy layer (ne_glyph_sub.go): the Schläge as drawn, or their
		// lineage under the change view; nil = the peer is down
		var ok bool
		img, ok = neGlyphSubRender(&g, lodIndex(lod), w, s, e, n, view == "subc", &m.sub)
		if !ok {
			m.sub.down = true
			return nil, m
		}
		m.n = len(ids)
	case "dif":
		// declared vs observed of the same cell, the three corroborated kinds (ne_glyph_dif.go)
		img = neGlyphDifRender(&g, ids, kgs, raw, &m)
	case "obs", "tree":
		// the observed land use per cell (obs_lu[9] of srtm's NEO1 row), drawn
		// exactly like the declared base: dominant group, alpha = dominance,
		// second group as an inner hexagon when ≥ 25 %. The rows are read
		// with the private request; the shares and the tree fields are all
		// that is looked at. v=tree paints the tree band instead of the use.
		res := neCollect(nil, ids, []string{"lu", "obs"}, &neReq{public: false, format: "bin", bevFirst: true})
		m.n = len(res.ids)
		tree := view == "tree"
		var pimg *image.Paletted
		if tree {
			pimg = image.NewPaletted(image.Rect(0, 0, g.pw, g.ph), neGlyphOverlayPalette(view))
		} else {
			pimg = image.NewPaletted(image.Rect(0, 0, g.pw, g.ph), neGlyphTilePalette())
		}
		img = pimg
		lay := neObsLayout(res.obsFlds, res.obsRec)
		for k, id := range res.ids {
			row := res.obs[k]
			if row == nil || lay == nil || len(row) != lay.rowBytes {
				continue
			}
			np, cx, cy, ok := g.poly(id, &poly)
			if !ok {
				continue
			}
			m.obs++
			if tree {
				neGlyphLUSum(row[lay.luOff:lay.luOff+9], &m) // the use sums still ride in the meta, the picture is the trees
			} else {
				neGlyphPaintLU(pimg, row[lay.luOff:lay.luOff+9], poly[:np], np, cx, cy, &m)
			}
			if h, n, ok := neObsTrees(row, lay); ok {
				m.trees += int64(n)
				if h > m.treeMax {
					m.treeMax = h
				}
				b := neGlyphTreeBand(h)
				m.treeHist[b]++
				if tree {
					neFillPoly(pimg, poly[:np], uint8(1+b))
				}
			}
		}
	case "chg":
		pimg := image.NewPaletted(image.Rect(0, 0, g.pw, g.ph), neGlyphOverlayPalette(view))
		img = pimg
		m.n = len(ids)
		seen := map[uint64]bool{}
		head := neGlyphHead(win)
		full, gone := neGlyphAgeScale(head)
		for _, kg := range kgs {
			c := neChangeGet(kg)
			if c == nil {
				continue
			}
			d, err := c.deltasOf()
			if err != nil {
				continue
			}
			for id, cu := range c.cumWin(d, win) {
				if seen[id] || cu.Share < 128 || (!cu.Confirmed && !cu.Candidate) {
					continue
				}
				lon, lat := neCellCentre(id)
				if lon < w-padDeg || lon > e+padDeg || lat < s-padDeg || lat > n+padDeg {
					continue
				}
				np, _, _, ok := g.poly(id, &poly)
				if !ok {
					continue
				}
				seen[id] = true
				if f := cu.FirstAt; len(f) >= 10 {
					if f = f[:10]; m.first == "" || f < m.first {
						m.first = f
					}
				}
				if l := cu.AnyAt; len(l) >= 10 && l[:10] > m.last {
					m.last = l[:10]
				}
				ci, at := neGlyphIdxChgCand, cu.AnyAt
				if cu.Confirmed {
					m.conf++
					ci, at = neGlyphIdxChgConf, cu.LastAt
					moved := 0
					for q := 0; q < 9; q++ {
						m.dlu[q] += int64(cu.DLU[q])
						if cu.DLU[q] > 0 {
							moved += cu.DLU[q]
						}
					}
					if float64(moved) >= 0.1*neCellAreaM2(id) || cu.DN != 0 {
						ci = neGlyphIdxChgConf2
					}
				} else {
					m.cand++
				}
				neFillPoly(pimg, poly[:np], neGlyphChgIdx(ci, neGlyphAge(at, head, full, gone)))
			}
		}
	default:
		res := neCollect(nil, ids, []string{"lu"}, &neReq{public: true, format: "bin", bevFirst: true})
		m.n = len(res.ids)
		pimg := image.NewPaletted(image.Rect(0, 0, g.pw, g.ph), neGlyphTilePalette())
		img = pimg
		for k, id := range res.ids {
			np, cx, cy, ok := g.poly(id, &poly)
			if !ok {
				continue
			}
			if res.lu[k][11]&neFlagSuperseded != 0 {
				m.sup++
				neGlyphPaintLUFaded(pimg, res.lu[k][12:21], poly[:np], &m)
				continue
			}
			neGlyphPaintLU(pimg, res.lu[k][12:21], poly[:np], np, cx, cy, &m)
		}
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.DefaultCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, m
	}
	return buf.Bytes(), m
}

// neObsTrees — the cell's trees after the same QA srtm applies to its own
// tree index (its /trees/bbox: masts, implausible heights and low-confidence
// apices are dropped): a LiDAR flight must cover the cell (flight_years ≠ 0),
// at least one apex, tallest ≤ 60 m, and from 20 m up some canopy
// (crown_frac ≥ 10 %) so a spike with no crown — mast, crane, pylon — never
// counts as a tree. 255 is nodata everywhere.
func neObsTrees(row []byte, lay *neObsLayoutT) (h, n int, ok bool) {
	if lay.treeH < 0 || lay.treeN < 0 {
		return 0, 0, false
	}
	h, n = int(row[lay.treeH]), int(row[lay.treeN])
	if h == 0 || h == 255 || n == 0 || n == 255 || h > 120 {
		return 0, 0, false
	}
	if lay.flight >= 0 {
		if fy := row[lay.flight]; fy == 0 || fy == 255 {
			return 0, 0, false
		}
	}
	if lay.crown >= 0 && h >= 40 {
		if cf := row[lay.crown]; cf == 255 || cf < 25 {
			return 0, 0, false
		}
	}
	return h, n, true
}

// neGlyphTreeBand — histogram band of a tallest-tree height (0.5 m units).
func neGlyphTreeBand(h int) int {
	switch {
	case h < 10:
		return 0
	case h < 30:
		return 1
	case h < 50:
		return 2
	case h < 70:
		return 3
	}
	return 4
}

// neGlyphLUSum — the cell's shares into the meta; dominant and second group.
func neGlyphLUSum(lu []byte, m *neGlyphTileMeta) (best, second, sum int) {
	best, second = -1, -1
	for q := 0; q < 9; q++ {
		v := int(lu[q])
		sum += v
		m.lus[q] += int64(v)
		if best < 0 || v > int(lu[best]) {
			second, best = best, q
		} else if second < 0 || v > int(lu[second]) {
			second = q
		}
	}
	if sum == 0 || best < 0 {
		m.unknown++
	}
	return
}

// neGlyphPaintLU — one cell from nine group shares (1/255 of the cell):
// dominant group, alpha = dominance, second group as an inner hexagon when
// it holds ≥ 25 %; Σ = 0 → the neutral "unknown" colour. Sums into m.
func neGlyphPaintLU(img *image.Paletted, lu []byte, poly [][2]float64, np int, cx, cy float64, m *neGlyphTileMeta) {
	best, second, sum := neGlyphLUSum(lu, m)
	if sum == 0 || best < 0 {
		neFillPoly(img, poly, neGlyphIdxUnknown)
		return
	}
	dom := float64(lu[best]) / float64(sum)
	shade := int(math.Floor((dom - 1.0/3) / (2.0 / 3) * neGlyphTileShades))
	if shade < 0 {
		shade = 0
	} else if shade >= neGlyphTileShades {
		shade = neGlyphTileShades - 1
	}
	neFillPoly(img, poly, neGlyphTileIdx(best, shade))
	if second >= 0 && int(lu[second])*4 >= sum {
		var in [8][2]float64
		for v := 0; v < np; v++ {
			in[v] = [2]float64{cx + (poly[v][0]-cx)*0.52, cy + (poly[v][1]-cy)*0.52}
		}
		neFillPoly(img, in[:np], neGlyphTileIdx(second, neGlyphTileShades-1))
	}
}

// neGlyphPaintLUFaded — a superseded cell: its dominant group at low alpha,
// no inner hexagon; unknown use stays the neutral colour. Never blank.
func neGlyphPaintLUFaded(img *image.Paletted, lu []byte, poly [][2]float64, m *neGlyphTileMeta) {
	best, _, sum := neGlyphLUSum(lu, m)
	if sum == 0 || best < 0 {
		neFillPoly(img, poly, neGlyphIdxUnknown)
		return
	}
	neFillPoly(img, poly, uint8(neGlyphIdxFaded+best))
}

// neObsLayoutT — where the fields the tile reads sit in a peer's NEO1 row,
// taken from the header's fields[] + struct format (never hard-coded).
type neObsLayoutT struct {
	rowBytes      int
	luOff         int // obs_lu[9]
	treeN, treeH  int // tree_n, tree_h_max (u8 each) or -1
	crown, flight int // crown_frac, flight_years (u8 each) or -1
	// the diff overlay (ne_glyph_dif.go): obs_bld_cover, h_mean, dh_mean (i8 ¼ m), verdict, hansen_loss_year — or -1
	bld, hMean, dh, verdict, hansen int
}

// neObsLayout — walks the python struct format ("<Q9sBBB…") beside fields[]
// and returns the offsets; nil when obs_lu[9] is not there.
func neObsLayout(fields []string, record string) *neObsLayoutT {
	if len(fields) == 0 || record == "" {
		return nil
	}
	lay := &neObsLayoutT{luOff: -1, treeN: -1, treeH: -1, crown: -1, flight: -1, bld: -1, hMean: -1, dh: -1, verdict: -1, hansen: -1}
	off, fi, i := 0, 0, 0
	if strings.ContainsRune("<>=!@", rune(record[0])) {
		i++
	}
	for i < len(record) {
		n := 0
		for i < len(record) && record[i] >= '0' && record[i] <= '9' {
			n = n*10 + int(record[i]-'0')
			i++
		}
		if i >= len(record) {
			break
		}
		c := record[i]
		i++
		if n == 0 {
			n = 1
		}
		var sz int
		switch c {
		case 'Q', 'q', 'd':
			sz = 8
		case 'I', 'i', 'f', 'L', 'l':
			sz = 4
		case 'H', 'h':
			sz = 2
		case 'B', 'b', 'c', '?':
			sz = 1
		case 's':
			if fi < len(fields) && fields[fi] == "obs_lu[9]" && n == 9 {
				lay.luOff = off
			}
			off += n
			fi++
			continue
		default:
			return nil
		}
		for r := 0; r < n; r++ {
			if fi < len(fields) && sz == 1 {
				switch fields[fi] {
				case "tree_n":
					lay.treeN = off
				case "tree_h_max":
					lay.treeH = off
				case "crown_frac":
					lay.crown = off
				case "flight_years":
					lay.flight = off
				case "obs_bld_cover":
					lay.bld = off
				case "h_mean":
					lay.hMean = off
				case "dh_mean":
					lay.dh = off
				case "verdict":
					lay.verdict = off
				case "hansen_loss_year":
					lay.hansen = off
				}
			}
			off += sz
			fi++
		}
	}
	if lay.luOff < 0 || lay.luOff+9 > off {
		return nil
	}
	lay.rowBytes = off
	return lay
}

// neFillPoly — even-odd scanline fill of a convex polygon, pixel centres.
func neFillPoly(img *image.Paletted, p [][2]float64, ci uint8) {
	miny, maxy := math.Inf(1), math.Inf(-1)
	for _, v := range p {
		miny, maxy = math.Min(miny, v[1]), math.Max(maxy, v[1])
	}
	b := img.Bounds()
	y0, y1 := int(math.Ceil(miny-0.5)), int(math.Floor(maxy-0.5))
	if y0 < b.Min.Y {
		y0 = b.Min.Y
	}
	if y1 >= b.Max.Y {
		y1 = b.Max.Y - 1
	}
	var xs [8]float64
	for y := y0; y <= y1; y++ {
		sy := float64(y) + 0.5
		n := 0
		for i := range p {
			a, c := p[i], p[(i+1)%len(p)]
			if (a[1] <= sy) == (c[1] <= sy) {
				continue
			}
			xs[n] = a[0] + (sy-a[1])*(c[0]-a[0])/(c[1]-a[1])
			n++
			if n == 8 {
				break
			}
		}
		if n < 2 {
			continue
		}
		sort.Float64s(xs[:n])
		for k := 0; k+1 < n; k += 2 {
			xa, xb := int(math.Ceil(xs[k]-0.5)), int(math.Floor(xs[k+1]-0.5))
			if xa < b.Min.X {
				xa = b.Min.X
			}
			if xb >= b.Max.X {
				xb = b.Max.X - 1
			}
			row := img.Pix[(y-b.Min.Y)*img.Stride:]
			for x := xa; x <= xb; x++ {
				row[x-b.Min.X] = ci
			}
		}
	}
}

// handleNEGlyphTile — GET /api/v1/ne/glyph/tile?lod=&i=&j=
func handleNEGlyphTile(w http.ResponseWriter, r *http.Request) {
	if err := neGlyphLoadFrame(); err != nil {
		apiError(w, "glyph frame unavailable", 503)
		return
	}
	q := r.URL.Query()
	lod, e1 := strconv.Atoi(q.Get("lod"))
	i, e2 := strconv.Atoi(q.Get("i"))
	j, e3 := strconv.Atoi(q.Get("j"))
	if e1 != nil || e2 != nil || e3 != nil || lod < 0 || lod >= len(neGlyphLODs) {
		apiError(w, "lod=0|1|2&i=&j= required", 400)
		return
	}
	view := q.Get("v")
	if view != "" && view != "obs" && view != "tree" && view != "chg" && view != "sub" && view != "subc" && view != "dif" && view != "rd" {
		apiError(w, "v=obs|tree|chg|sub|subc|dif|rd", 400)
		return
	}
	if (view == "dif" || view == "rd") && lod == 2 {
		apiError(w, "v="+view+" is drawn at lod=0|1 only (the marks need the near tiles)", 400)
		return
	}
	// raw=1 on a diff tile: the kinds without their corroboration (the noise filters of ne-change.md §6d off)
	raw := view == "dif" && q.Get("raw") == "1"
	sub := view == "sub" || view == "subc"
	// f=1 (the LOD-2 "fine cut" of 2026-10-20) is accepted and ignored: since 2026-10-30
	// every subsidy tile is painted from the bins, at every LOD, like the other views
	// t= on a change tile: only the deltas of a pass / detection-day window (ne_change_passes.go)
	win, okw := neParseWin(q.Get("t"))
	if !okw || (!win.zero() && view != "chg") {
		apiError(w, "t= is `k`, `k1..k2`, `k@YYYY-MM-DD..YYYY-MM-DD` or `@YYYY-MM-DD..YYYY-MM-DD`, on v=chg only", 400)
		return
	}

	L := neGlyphLODs[lod]
	neFidxWarmNoteTile(lod, i, j) // the filter index warms behind the near tiles (ne_glyph_fidx.go)
	if tw, ts, te, tn := neGlyphTileBox(L, i, j); te < 9.4 || tw > 17.3 || tn < 46.3 || ts > 49.1 {
		apiError(w, "tile outside Austria", 404)
		return
	}
	key := fmt.Sprintf("%s/%d/%d/%d", view, lod, i, j)
	noStore := false
	if raw {
		key += "/raw"
	}
	if !win.zero() {
		key += "/t=" + win.String()
	}
	if view == "dif" {
		st := neDifEpochStamp()
		key += "/e" + strconv.FormatInt(st, 36)
		noStore = st == 0 // no lidar year yet (table loading): never let a browser keep this one
	}
	if view == "rd" {
		// static OSM extract: no cell state in the key
		w.Header().Set("X-Data-Attribution", "(c) OpenStreetMap contributors (ODbL) - roads + rail")
	} else if !sub {
		// the key carries the state of every KG the tile reads (declared view on
		// the chunk log, bev, lu, obs digests), so a report or an upload that
		// changes the picture misses the cache instead of serving a stale tile
		tw, ts, te, tn := neGlyphTileBox(L, i, j)
		key += "/" + neGlyphTileState(KGCodesInBBoxMem(tw, ts, te, tn))
	}
	if sub || (view == "chg" && win.D2 == "") {
		key += "/" + time.Now().UTC().Format("2006-01-02") // the peer's release may move / change ages; a day of staleness at most
		w.Header().Set("X-Data-Attribution", asciiFold(neSubAttribution))
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=600")
	if view == "chg" {
		w.Header().Set("Cache-Control", "public, max-age=300") // moves with the chunk log
	}
	if noStore {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.Header().Set("X-NE-Notice", NENotice())
	if b, ok := neGlyphTileLRU.get(key); ok {
		// cached: n + ms + pending ride in a tiny trailer header
		meta, pngb := neGlyphTileSplit(b)
		w.Header().Set("X-NE-Tile", meta)
		w.Header().Set("Content-Length", strconv.Itoa(len(pngb)))
		_, _ = w.Write(pngb)
		return
	}
	t0 := time.Now()
	pngb, m := neGlyphTileRenderWin(L, i, j, view, raw, win)
	if pngb == nil && m.sub.down {
		// the subsidy peer is down: no picture, the legend says so
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-NE-Tile", "n=0;sub=0;down=1")
		w.WriteHeader(204)
		return
	}
	if pngb == nil && m.rd.down {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-NE-Tile", "n=0;rd=0;down=1")
		w.WriteHeader(204)
		return
	}
	if pngb == nil {
		apiError(w, "tile render failed", 500)
		return
	}
	meta := m.String(time.Since(t0).Milliseconds(), view)
	if len(m.pending) == 0 {
		neGlyphTileLRU.put(key, neGlyphTileJoin(meta, pngb))
	} else {
		w.Header().Set("Cache-Control", "public, max-age=60")
	}
	w.Header().Set("X-NE-Tile", meta)
	w.Header().Set("Content-Length", strconv.Itoa(len(pngb)))
	_, _ = w.Write(pngb)
}

// neGlyphTileState — sha256[:12] over the declared/obs state of the KGs a tile
// reads; cheap (in-memory headers + one stat per KG for the chunk-log view).
func neGlyphTileState(kgs []string) string {
	h := sha256.New()
	for _, kg := range kgs {
		k := neStore.get(kg)
		if k == nil {
			continue
		}
		h.Write([]byte(kg + ":" + neDeclaredDigest(k, true)))
		if l := k.Hdr.Layers["obs"]; l != nil {
			h.Write([]byte(":" + l.Digest))
		}
		h.Write([]byte(";"))
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

func lodIndex(l neGlyphLOD) int {
	for i, x := range neGlyphLODs {
		if x.deg == l.deg {
			return i
		}
	}
	return 0
}

func neGlyphTileJoin(meta string, pngb []byte) []byte {
	out := make([]byte, 0, len(meta)+1+len(pngb))
	out = append(out, meta...)
	out = append(out, '\n')
	return append(out, pngb...)
}

func neGlyphTileSplit(b []byte) (string, []byte) {
	k := bytes.IndexByte(b, '\n')
	if k < 0 {
		return "", b
	}
	return string(b[:k]), b[k+1:]
}

// NEGlyphTileStats — size line for /api/v1/debug/inflight.
func NEGlyphTileStats() map[string]interface{} { return neGlyphTileLRU.stats() }

// handleNEGlyphCell — GET /api/v1/ne/glyph/cell?lon=&lat=: the public
// res-12 cell doc under a point plus its hexagon in viewBox units, so the
// page can draw the cursor and the card without an H3 library.
func handleNEGlyphCell(w http.ResponseWriter, r *http.Request) {
	if err := neGlyphLoadFrame(); err != nil {
		apiError(w, "glyph frame unavailable", 503)
		return
	}
	q := r.URL.Query()
	var id uint64
	if s := q.Get("cell"); s != "" {
		v, res, err := neParseCell(s)
		if err != nil || res != neCellRes {
			apiError(w, "cell must be a res-12 H3 id", 400)
			return
		}
		id = v
	} else {
		lon, e1 := strconv.ParseFloat(q.Get("lon"), 64)
		lat, e2 := strconv.ParseFloat(q.Get("lat"), 64)
		if e1 != nil || e2 != nil {
			apiError(w, "lon=&lat= or cell= required", 400)
			return
		}
		v, err := neLatLngToCell(lon, lat, neCellRes)
		if err != nil {
			apiError(w, "bad point", 400)
			return
		}
		id = v
	}
	lon, lat := neCellCentre(id)
	w.Header().Set("Access-Control-Allow-Origin", "*")
	bd, _ := h3.Cell(id).Boundary()
	poly := make([][2]float64, 0, len(bd))
	for _, v := range bd {
		x, y := neGlyphProj(v.Lng, v.Lat)
		poly = append(poly, [2]float64{math.Round(x*1000) / 1000, math.Round(y*1000) / 1000})
	}
	clon, clat := neCellCentre(id)
	cx, cy := neGlyphProj(clon, clat)
	out := map[string]interface{}{"cell": neCellHex(id), "poly": poly, "x": math.Round(cx*1000) / 1000, "y": math.Round(cy*1000) / 1000, "lon": clon, "lat": clat, "notice": NENotice()}
	doc, ok := neCellDoc(id, &neReq{public: true, bevFirst: true})
	if ok {
		for k, v := range doc {
			out[k] = v
		}
		out["pic_obs"] = neGlyphPicObs(id)
		kgs, _ := doc["kgs"].(map[string]uint8)
		if pd := neGlyphPicDif(id, kgs); pd != nil {
			out["pic_dif"] = pd
		}
		if doc["chg"] != nil {
			kgs, _ := doc["kgs"].(map[string]uint8)
			if win, ok := neParseWin(q.Get("t")); ok && !win.zero() {
				// the card under a timeline window: the cell's change and its cluster inside the window only
				out["chg"] = neCellChangeWin(id, kgs, win)
				out["chg_all"] = doc["chg"]
				out["win"] = win.String()
				if out["chg"] != nil {
					out["cluster"] = neCellClusterWin(id, kgs, win)
				}
			} else {
				out["cluster"] = neCellCluster(id, kgs)
			}
		}
		w.Header().Set("Cache-Control", "public, max-age=300")
	}
	if q.Get("sub") == "1" {
		// the subsidy reading of this cell from the bin LRU (never the raw columns)
		if sv, up := neSubCellAt(id); up {
			out["sub"] = sv
		} else {
			out["sub"] = nil
			out["sub_unavailable"] = true
		}
		out["sub_attribution"] = neSubAttribution
		w.Header().Set("X-Data-Attribution", w.Header().Get("X-Data-Attribution")+" | "+asciiFold(neSubAttribution))
	}
	if !ok {
		out["held"] = false
		if p := nePendingKGs(KGCodesNearPointMem(lon, lat, 0.005)); len(p) > 0 {
			out["pending_kgs"] = p
		}
		w.Header().Set("Cache-Control", "no-store")
	}
	writeJSON(w, 200, out)
}

// neGlyphPicObs — what the v=obs picture shows for one cell, in words: the
// dominant observed group and its share (5 % steps — the tile's four shades
// are coarser), the second group when the inner hexagon is drawn (≥ 25 %).
// nil when no observed row is held. This is
// the pixel under the cursor, not the row: the row stays behind obs_public.
func neGlyphPicObs(id uint64) map[string]interface{} {
	oh, _ := neStore.cellRows(id, "obs")
	if len(oh) == 0 {
		return nil
	}
	k := neStore.get(oh[0].KG)
	if k == nil {
		return nil
	}
	rec := ""
	if h := k.layerHeader("obs"); h != nil {
		rec, _ = h["record"].(string)
	}
	lay := neObsLayout(neObsFields(k), rec)
	row := oh[0].Row
	if lay == nil || len(row) != lay.rowBytes {
		return nil
	}
	lu := row[lay.luOff : lay.luOff+9]
	best, second, sum := -1, -1, 0
	for q := 0; q < 9; q++ {
		v := int(lu[q])
		sum += v
		if best < 0 || v > int(lu[best]) {
			second, best = best, q
		} else if second < 0 || v > int(lu[second]) {
			second = q
		}
	}
	var o map[string]interface{}
	if sum == 0 {
		o = map[string]interface{}{"unknown": true}
	} else {
		o = map[string]interface{}{"dom": neGlyphGroups[best], "dom_pct": int(math.Round(float64(lu[best])/float64(sum)*20)) * 5}
		if second >= 0 && int(lu[second])*4 >= sum {
			o["second"] = neGlyphGroups[second]
		}
	}
	// the v=tree pixel: the band of the tallest tree (QA as the tile) + its height and the apex count
	if h, n, ok := neObsTrees(row, lay); ok {
		o["tree"] = map[string]interface{}{"band": neGlyphTreeBand(h), "h_m": float64(h) / 2, "n": n}
	}
	return o
}

// ── KG outline at native resolution ──────────────────────────────────────────

// neGlyphKGOutline — the KG as the union of its res-12 cells (kg_share ≥ ½,
// plus split border cells that lean in: ≥ 3 of 6 neighbours are majority
// cells), traced by H3 into rings and projected to viewBox units. Returns
// the SVG path (2 decimals, evenodd — holes are rings too), the cell count
// and the bbox.
func neGlyphKGOutline(k *neKG) (d string, cells int, bbox [4]float64, err error) {
	rows, _, err := neStore.layerRows(k, "lu")
	if err != nil {
		return "", 0, bbox, err
	}
	l := k.Hdr.Layers["lu"]
	rb := l.RowBytes
	set := make(map[uint64]bool, l.Rows)
	var border []uint64
	for i := 0; i < l.Rows; i++ {
		row := rows[i*rb : (i+1)*rb]
		id := neRowID(rows, i, rb)
		switch sh := row[8]; {
		case sh >= 128:
			set[id] = true
		case sh > 0:
			border = append(border, id)
		}
	}
	for _, id := range border {
		nb, err := h3.Cell(id).GridDisk(1)
		if err != nil {
			continue
		}
		in := 0
		for _, c := range nb {
			if uint64(c) != id && set[uint64(c)] {
				in++
			}
		}
		if in >= 3 {
			set[id] = true
		}
	}
	cs := make([]h3.Cell, 0, len(set))
	for id := range set {
		cs = append(cs, h3.Cell(id))
	}
	polys, err := h3.CellsToMultiPolygon(cs)
	if err != nil {
		return "", 0, bbox, err
	}
	var sb strings.Builder
	bbox = [4]float64{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)}
	ring := func(loop h3.GeoLoop) {
		for v, ll := range loop {
			x, y := neGlyphProj(ll.Lng, ll.Lat)
			if v == 0 {
				sb.WriteByte('M')
			} else {
				sb.WriteByte('L')
			}
			sb.WriteString(strconv.FormatFloat(x, 'f', 2, 64))
			sb.WriteByte(' ')
			sb.WriteString(strconv.FormatFloat(y, 'f', 2, 64))
			bbox[0], bbox[1] = math.Min(bbox[0], x), math.Min(bbox[1], y)
			bbox[2], bbox[3] = math.Max(bbox[2], x), math.Max(bbox[3], y)
		}
		sb.WriteByte('Z')
	}
	for _, p := range polys {
		ring(p.GeoLoop)
		for _, h := range p.Holes {
			ring(h)
		}
	}
	return sb.String(), len(cs), bbox, nil
}

// handleNEGlyphKG — GET /api/v1/ne/glyph/kg?kg=63330: the KG outline at
// res 12 as an SVG path in viewBox units; the full-resolution veil mask.
func handleNEGlyphKG(w http.ResponseWriter, r *http.Request) {
	if err := neGlyphLoadFrame(); err != nil {
		apiError(w, "glyph frame unavailable", 503)
		return
	}
	kg := r.URL.Query().Get("kg")
	if !neKGRe.MatchString(kg) {
		apiError(w, "kg= (5 digits) required", 400)
		return
	}
	k := neStore.get(kg)
	if k == nil || k.Hdr.Layers["lu"] == nil {
		w.Header().Set("Cache-Control", "no-store")
		apiError(w, "KG not held", 404)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("X-NE-Notice", NENotice())
	key := "kg/" + kg + "/" + k.Hdr.Layers["lu"].Digest
	if b, ok := neGlyphTileLRU.get(key); ok {
		_, _ = w.Write(b)
		return
	}
	t0 := time.Now()
	d, n, bb, err := neGlyphKGOutline(k)
	if err != nil {
		apiError(w, "outline failed", 500)
		return
	}
	b, _ := json.Marshal(map[string]interface{}{
		"kg": kg, "d": d, "cells": n, "res": neCellRes,
		"bbox":   []float64{math.Round(bb[0]*100) / 100, math.Round(bb[1]*100) / 100, math.Round(bb[2]*100) / 100, math.Round(bb[3]*100) / 100},
		"ms":     time.Since(t0).Milliseconds(),
		"notice": NENotice(),
	})
	neGlyphTileLRU.put(key, b)
	_, _ = w.Write(b)
}

// neGlyphSoakJS — the in-page soak test of the landing glyph (ne_glyph_soak.js,
// docs/ne-glyph.md §soak). Public and static: it only drives window.neGlyph
// and the public glyph endpoints. Served as an ES module so a browser driver
// can `import('/api/v1/ne/glyph/soak.js')` without pasting the script.
//
//go:embed ne_glyph_soak.js
var neGlyphSoakJS []byte

func handleNEGlyphSoakJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store") // a test driver must always get the current build
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_, _ = w.Write(neGlyphSoakJS)
}
