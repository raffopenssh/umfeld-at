// ne_glyph.go — the "what we hold" glyph on the landing page and its data
// behind GET /api/v1/ne/glyph (public, docs/ne-change.md §glyph).
//
// Austria as one dot per H3 res-7 cell (16 424 cells whose centre is inside
// the GADM outline, data/ne_glyph_austria_r7.json). Every peer digest of the
// land in a cell adds a translucent layer (α = 0.13 × chunk coverage), so
// land digested 2, 3, 4× simply gets darker; reports younger than 6 h are
// a light blue tint (never stronger than the ink) fading with age; cells nobody digested yet are a hairline. Two
// more views of the same cells — observed layer posted by {OBS_NAME},
// confirmed change — are overlays in the same picture, switched by the view
// control (never a second figure).
//
// Cost: the aggregation rides on the two loops that already exist for the
// landing page — the report scan (neScanReportFiles, 10 min memo) feeds one
// small map per res-7 cell, the manifest loop (neCoverageStats, 5 min memo)
// marks the observed KGs. Nothing walks the reports a second time. The
// rendered SVG and the JSON body are built once per coverage memo and held
// in a single bounded slot (size on /api/v1/debug/inflight → ne_glyph).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/uber/h3-go/v4"
)

const (
	neGlyphRes       = 7
	neGlyphW         = 6680 // viewBox units = 0.1 CSS px at the 668 px design width
	neGlyphPad       = 40
	neGlyphDot       = 32  // stroke width (dot diameter) of a res-7 cell, viewBox units
	neGlyphChunksPer = 343 // res-10 chunks per res-7 cell (7³)
	neGlyphAlpha     = 0.13
	neGlyphFreshH    = 6.0
	neGlyphLat0      = 47.6 // cos(lat) of the x stretch
)

// ── static frame: cell → (x, y) ──────────────────────────────────────────────

type neGlyphPos struct {
	id       uint64
	x, y     int32
	lon, lat float64 // cell centre, for the KG bbox lookup of cells no report touched
}

var neGlyphFrame struct {
	once  sync.Once
	cells []neGlyphPos // res-7, sorted by id
	index map[uint64]int32
	h     int32
	// hex field: a res-7 cell at viewBox (x, y) is a regular hexagon of
	// circumradius hexR·(1,x,y) rotated by hexRot·(1,x,y) degrees (first
	// vertex from +x, clockwise on screen). Linear least squares over the
	// true H3 boundaries; residual < 0.02° / 0.2 % across Austria, so the
	// client can draw the exact tiling from six numbers.
	hexRot [3]float64
	hexR   [3]float64
	// inverse projection for the client: lon = ((x-pad)/scale + minx)/kx,
	// lat = -((y-pad)/scale + miny) — so a card can ask {OBS_NAME} for
	// the hexagon's bbox without the JSON carrying 16 k cell ids.
	scale, minx, miny, kx float64
	err                   error
}

func neGlyphLoadFrame() error {
	neGlyphFrame.once.Do(func() {
		f := &neGlyphFrame
		b, err := os.ReadFile(filepath.Join("data", "ne_glyph_austria_r7.json"))
		if err != nil {
			f.err = err
			return
		}
		var doc struct {
			Res   int      `json:"res"`
			Cells []string `json:"cells"`
		}
		if err := json.Unmarshal(b, &doc); err != nil || doc.Res != neGlyphRes || len(doc.Cells) == 0 {
			f.err = fmt.Errorf("ne_glyph_austria_r7.json: bad file (%v)", err)
			return
		}
		type pt struct {
			id       uint64
			x, y     float64
			lon, lat float64
		}
		pts := make([]pt, 0, len(doc.Cells))
		kx := math.Cos(neGlyphLat0 * math.Pi / 180)
		minx, maxx, miny, maxy := math.Inf(1), math.Inf(-1), math.Inf(1), math.Inf(-1)
		proj := func(id uint64) pt {
			ll, _ := h3.Cell(id).LatLng()
			return pt{id, ll.Lng * kx, -ll.Lat, ll.Lng, ll.Lat}
		}
		for _, s := range doc.Cells {
			id, err := strconv.ParseUint(s, 16, 64)
			if err != nil {
				continue
			}
			p := proj(id)
			pts = append(pts, p)
			minx, maxx, miny, maxy = math.Min(minx, p.x), math.Max(maxx, p.x), math.Min(miny, p.y), math.Max(maxy, p.y)
		}
		scale := float64(neGlyphW-2*neGlyphPad) / (maxx - minx)
		f.h = int32(math.Round((maxy-miny)*scale)) + 2*neGlyphPad
		f.scale, f.minx, f.miny, f.kx = scale, minx, miny, kx
		fit := func(p pt) neGlyphPos {
			return neGlyphPos{p.id, int32(math.Round((p.x-minx)*scale)) + neGlyphPad, int32(math.Round((p.y-miny)*scale)) + neGlyphPad, p.lon, p.lat}
		}
		sort.Slice(pts, func(i, j int) bool { return pts[i].id < pts[j].id })
		fitF := func(p pt) (float64, float64) { return (p.x-minx)*scale + neGlyphPad, (p.y-miny)*scale + neGlyphPad }
		samples := make([]neGlyphHexSample, 0, len(pts))
		for _, p := range pts {
			x, y := fitF(p)
			samples = append(samples, neGlyphHexSample{p.id, x, y})
		}
		f.hexRot, f.hexR = neGlyphHexFit(samples, func(id uint64) [][2]float64 {
			bd, err := h3.Cell(id).Boundary()
			if err != nil {
				return nil
			}
			out := make([][2]float64, 0, len(bd))
			for _, v := range bd {
				out = append(out, [2]float64{(v.Lng*kx-minx)*scale + neGlyphPad, (-v.Lat-miny)*scale + neGlyphPad})
			}
			return out
		})
		f.cells = make([]neGlyphPos, len(pts))
		f.index = make(map[uint64]int32, len(pts))
		for i, p := range pts {
			f.cells[i] = fit(p)
			f.index[p.id] = int32(i)
		}
	})
	return neGlyphFrame.err
}

// neGlyphHexFit — least-squares planes rot(x,y), r(x,y) of the cells' real
// H3 boundaries in viewBox coordinates (rotation as the circular mean of the
// six vertex angles mod 60°).
type neGlyphHexSample struct {
	id   uint64
	x, y float64
}

func neGlyphHexFit(pts []neGlyphHexSample, boundary func(uint64) [][2]float64) ([3]float64, [3]float64) {
	var ata [3][3]float64
	var atRot, atR [3]float64
	step := 1
	if len(pts) > 4000 {
		step = len(pts) / 4000
	}
	for i := 0; i < len(pts); i += step {
		x, y := pts[i].x, pts[i].y
		bd := boundary(pts[i].id)
		if len(bd) != 6 {
			continue
		}
		var sx, sy, sr float64
		for _, v := range bd {
			dx, dy := v[0]-x, v[1]-y
			a := math.Atan2(dy, dx) * 6 // ×6: 60° symmetry → full circle
			sx += math.Cos(a)
			sy += math.Sin(a)
			sr += math.Hypot(dx, dy)
		}
		rot := math.Mod(math.Atan2(sy, sx)/6*180/math.Pi+360, 60)
		r := sr / 6
		row := [3]float64{1, x, y}
		for a := 0; a < 3; a++ {
			for b := 0; b < 3; b++ {
				ata[a][b] += row[a] * row[b]
			}
			atRot[a] += row[a] * rot
			atR[a] += row[a] * r
		}
	}
	return neGlyphSolve3(ata, atRot), neGlyphSolve3(ata, atR)
}

// neGlyphSolve3 — Gaussian elimination with partial pivoting, 3×3.
func neGlyphSolve3(a [3][3]float64, b [3]float64) [3]float64 {
	for c := 0; c < 3; c++ {
		p := c
		for r := c + 1; r < 3; r++ {
			if math.Abs(a[r][c]) > math.Abs(a[p][c]) {
				p = r
			}
		}
		a[c], a[p] = a[p], a[c]
		b[c], b[p] = b[p], b[c]
		if a[c][c] == 0 {
			return [3]float64{}
		}
		for r := c + 1; r < 3; r++ {
			f := a[r][c] / a[c][c]
			for k := c; k < 3; k++ {
				a[r][k] -= f * a[c][k]
			}
			b[r] -= f * b[c]
		}
	}
	var x [3]float64
	for r := 2; r >= 0; r-- {
		s := b[r]
		for k := r + 1; k < 3; k++ {
			s -= a[r][k] * x[k]
		}
		x[r] = s / a[r][r]
	}
	return x
}

// ── aggregation (fed by the report scan) ─────────────────────────────────────

type neGlyphCellAgg struct {
	Reports int              // digests that touched the cell
	Last    string           // latest received_at
	KGs     map[string]int   // KG → chunks reported (the cell's dominant KG names the tooltip)
	Chunks  int              // distinct res-10 chunks held (filled from the daily chunk map)
	Passes  int              // Σ distinct observation days over those chunks
	Bands   [nePassBandN]int // chunks per pass band (1st · 2nd · 3rd–4th · 5th–9th · 10th+) — the card's depth bar
}

// neGlyphObsBoxes — the bboxes of the KGs that hold an observed layer.
func neGlyphObsBoxes(obs map[string]bool, boxes []kgBox) []kgBox {
	out := make([]kgBox, 0, len(obs))
	for _, b := range boxes {
		if obs[b.code] {
			out = append(out, b)
		}
	}
	return out
}

// neGlyphObsAt — the first observed KG whose bbox covers the point ("" = none).
// Strict (unpadded) containment: the res-7 cell is 5 km² and only a cell no
// report placed needs this; a border cell may read observed from its
// neighbour's envelope, which is harmless — srtm's lidar is country-wide.
func neGlyphObsAt(boxes []kgBox, lon, lat float64) string {
	for i := range boxes {
		b := &boxes[i]
		if lon >= b.minLon && lon <= b.maxLon && lat >= b.minLat && lat <= b.maxLat {
			return b.code
		}
	}
	return ""
}

type neGlyphAgg struct {
	cells map[uint64]*neGlyphCellAgg
	touch map[uint64]int // per-report scratch: parent → chunks
}

func newNEGlyphAgg() *neGlyphAgg {
	return &neGlyphAgg{cells: map[uint64]*neGlyphCellAgg{}, touch: map[uint64]int{}}
}

func neGlyphParent(chunk string, res int) (uint64, bool) {
	k, ok := neChunkKey(chunk)
	if !ok {
		return 0, false
	}
	c := h3.Cell(k)
	if c.Resolution() < res {
		return 0, false
	}
	p, err := c.Parent(res)
	return uint64(p), err == nil
}

// add — one stored report: every res-7 cell it touches gets a layer.
func (g *neGlyphAgg) add(kg string, s *neStoredReport) {
	for c := range s.Report.Chunks {
		if p, ok := neGlyphParent(c, neGlyphRes); ok {
			g.touch[p]++
		}
	}
	for p, n := range g.touch {
		a := g.cells[p]
		if a == nil {
			a = &neGlyphCellAgg{KGs: map[string]int{}}
			g.cells[p] = a
		}
		a.Reports++
		a.KGs[kg] += n
		if s.ReceivedAt > a.Last {
			a.Last = s.ReceivedAt
		}
		delete(g.touch, p)
	}
}

// fold — distinct chunks and their pass depth from the daily tracker's map
// (one entry per chunk, so no second walk of the reports). The tracker keeps
// one map per assembly class; a chunk digested under two classes is one
// chunk of land here, at its deepest pass — so "N of 343 chunks" stays ≤ 343.
func (g *neGlyphAgg) fold(d *neDailyAgg) {
	deep := map[uint64]uint16{}
	if len(d.chunks) == 1 {
		deep = nil // single class: nothing to merge
	}
	add := func(k uint64, n uint16) {
		p, err := h3.Cell(k).Parent(neGlyphRes)
		if err != nil {
			return
		}
		a := g.cells[uint64(p)]
		if a == nil {
			a = &neGlyphCellAgg{KGs: map[string]int{}}
			g.cells[uint64(p)] = a
		}
		a.Chunks++
		a.Passes += int(n)
		a.Bands[nePassBand(int(n))]++
	}
	for _, cm := range d.chunks {
		for k, o := range cm {
			if k>>63 != 0 {
				continue // test-only non-hex chunk keys
			}
			if deep == nil {
				add(k, o.n)
			} else if o.n > deep[k] {
				deep[k] = o.n
			}
		}
	}
	for k, n := range deep {
		add(k, n)
	}
}

// ── the product: per-cell rows, SVG, JSON ────────────────────────────────────

type neGlyphRow struct {
	X, Y    int32
	Chunks  int
	Passes  int // Σ distinct days
	Bands   [nePassBandN]int
	Reports int
	AgeH    float32 // hours since the latest report; <0 = never
	Obs     bool
	Chg     int // confirmed change cells (res-12) inside
	Cand    int // candidate change cells (one sighting, not yet confirmed) inside
	KG      int32
	Alpha   float32 // rendered ink 0..1
}

type neGlyph struct {
	AsOf    string
	Rows    []neGlyphRow // index-aligned with neGlyphFrame.cells
	KGs     []string     // tooltip names
	KGCodes []string     // index-aligned with KGs (the search box keys on these)
	KGIndex []byte       // /api/v1/ne/glyph/kgs: every KG of the universe with its viewBox bbox
	SVG     string       // main glyph (with the observed / change overlays)
	Held    int
	Obs     int
	Chg     int
	Cand    int
	Picks   []int // tour stops (row indices), neGlyphPicks
	JSON    []byte
	Bytes   int
}

var neGlyphSlot struct {
	sync.Mutex
	g *neGlyph
}

// neGlyphBuild — assembled inside the coverage memo: the scan's aggregate,
// the manifest's observed KGs, the derived change files of the KGs with
// confirmed change.
func neGlyphBuild(agg *neGlyphAgg, obsKGs []string, now time.Time) *neGlyph {
	if neGlyphLoadFrame() != nil || agg == nil {
		return nil
	}
	fr := &neGlyphFrame
	g := &neGlyph{AsOf: now.UTC().Format("2006-01-02 15:04 UTC"), Rows: make([]neGlyphRow, len(fr.cells))}
	obs := make(map[string]bool, len(obsKGs))
	for _, kg := range obsKGs {
		obs[kg] = true
	}
	// confirmed / candidate change cells → res-7 parents (only the few KGs
	// that have any; a cell with a confirmed row counts as confirmed only)
	chg, cand := map[uint64]int{}, map[uint64]int{}
	for _, kg := range neChangeKGs() {
		if s, ok := neChangeSummaryOf(kg); !ok || (s.Confirmed.Cells == 0 && s.Candidate.Cells == 0) {
			continue
		}
		b, err := os.ReadFile(neFilePath(kg, neChangeDerived))
		if err != nil {
			continue
		}
		conf, pend := map[string]bool{}, map[string]bool{}
		for _, ln := range bytes.Split(b, []byte{'\n'})[1:] {
			isConf := bytes.Contains(ln, []byte(`"status":"confirmed"`))
			if !isConf && !bytes.Contains(ln, []byte(`"status":"candidate"`)) {
				continue
			}
			var row struct {
				Cell   string `json:"cell"`
				Share  uint8  `json:"kg_share"`
				Absent bool   `json:"absent"`
			}
			// same dedupe rule as the summary and the full-res tiles (ne_change.go
			// summary, ne_glyph_tile.go v=chg): an edge cell counts for the KG that
			// holds its majority, a row whose cell left the layer is not change
			if json.Unmarshal(ln, &row) != nil || row.Cell == "" || row.Absent || row.Share < 128 {
				continue
			}
			if isConf {
				conf[row.Cell] = true
			} else {
				pend[row.Cell] = true
			}
		}
		for cell := range conf {
			if p, ok := neGlyphParent(cell, neGlyphRes); ok {
				chg[p]++
			}
		}
		for cell := range pend {
			if conf[cell] {
				continue
			}
			if p, ok := neGlyphParent(cell, neGlyphRes); ok {
				cand[p]++
			}
		}
	}
	kgIdx := map[string]int32{}
	names := kgNameSnapshot()
	nameOf := map[string]string{}
	for i := range names {
		nm := names[i].kgName
		if g := names[i].gemeindeName; g != "" && g != nm {
			nm += " · " + g
		}
		nameOf[names[i].code] = nm
	}
	// KGs with an observed layer, as bboxes: a cell no report touched (or whose
	// reports came from a KG without one) still paints observed when the bbox of
	// a KG that posted NEO1 covers its centre — the observed layer is the
	// peer's, not the reports'. Gablitz/Mauerbach 2026-10-29: .obs here, srtm
	// serving, no report ever → white hole under v=obs.
	obsBoxes := neGlyphObsBoxes(obs, kgBoxSnapshot())
	kgOf := func(kg string) int32 {
		ix, ok := kgIdx[kg]
		if !ok {
			ix = int32(len(g.KGs))
			kgIdx[kg] = ix
			nm := nameOf[kg]
			if nm == "" {
				nm = "KG " + kg
			}
			g.KGs = append(g.KGs, nm)
			g.KGCodes = append(g.KGCodes, kg)
		}
		return ix
	}
	for i, c := range fr.cells {
		r := neGlyphRow{X: c.x, Y: c.y, AgeH: -1, KG: -1, Chg: chg[c.id], Cand: cand[c.id]}
		if a := agg.cells[c.id]; a != nil && a.Chunks > 0 {
			r.Chunks, r.Passes, r.Reports, r.Bands = a.Chunks, a.Passes, a.Reports, a.Bands
			if t, err := time.Parse(time.RFC3339, a.Last); err == nil {
				r.AgeH = float32(now.Sub(t).Hours())
			}
			best, bn := "", 0
			for kg, n := range a.KGs {
				if n > bn || (n == bn && kg < best) {
					best, bn = kg, n
				}
				if obs[kg] {
					r.Obs = true
				}
			}
			if best != "" {
				r.KG = kgOf(best)
			}
			cov := math.Min(1, float64(a.Chunks)/neGlyphChunksPer)
			passes := math.Max(1, math.Round(float64(a.Passes)/float64(a.Chunks)))
			r.Alpha = float32(1 - math.Pow(1-neGlyphAlpha*cov, passes))
			g.Held++
		}
		if !r.Obs {
			if kg := neGlyphObsAt(obsBoxes, c.lon, c.lat); kg != "" {
				r.Obs = true
				if r.KG < 0 {
					r.KG = kgOf(kg)
				}
			}
		}
		if r.Obs {
			g.Obs++
		}
		if r.Chg > 0 {
			g.Chg++
		}
		if r.Cand > 0 {
			g.Cand++
		}
		g.Rows[i] = r
	}
	g.Picks = neGlyphPicks(g, 14)
	g.SVG = neGlyphSVG(g)
	g.JSON = neGlyphJSON(g)
	g.KGIndex = neGlyphKGIndex(names)
	g.Bytes = len(g.SVG) + len(g.JSON) + len(g.KGIndex) + len(g.Rows)*40
	neGlyphSlot.Lock()
	neGlyphSlot.g = g
	neGlyphSlot.Unlock()
	return g
}

func neGlyphCurrent() *neGlyph {
	neGlyphSlot.Lock()
	defer neGlyphSlot.Unlock()
	return neGlyphSlot.g
}

// neGlyphSVG — one <path> per ink bucket: 16 424 dots in ~25 DOM nodes
// (zero-length "h0" segments with round caps are dots of the stroke width).
func neGlyphSVG(g *neGlyph) string {
	const buckets = 16
	empty := &strings.Builder{}
	ink := make([]*strings.Builder, buckets)
	fresh := make([]*strings.Builder, 6)
	obs := make([]*strings.Builder, buckets)
	chg := make([]*strings.Builder, len(neGlyphChgBuckets))
	cand := make([]*strings.Builder, len(neGlyphCandBuckets))
	halo := &strings.Builder{}
	for i := range ink {
		ink[i] = &strings.Builder{}
		obs[i] = &strings.Builder{}
	}
	for i := range fresh {
		fresh[i] = &strings.Builder{}
	}
	for i := range chg {
		chg[i] = &strings.Builder{}
	}
	for i := range cand {
		cand[i] = &strings.Builder{}
	}
	dot := func(b *strings.Builder, r *neGlyphRow) {
		b.WriteString("M")
		b.WriteString(strconv.Itoa(int(r.X)))
		b.WriteByte(' ')
		b.WriteString(strconv.Itoa(int(r.Y)))
		b.WriteString("h0")
	}
	for i := range g.Rows {
		r := &g.Rows[i]
		k := int(r.Alpha * buckets)
		if k >= buckets {
			k = buckets - 1
		}
		if r.Chunks == 0 { // not digested: hairline, but the overlays still apply (k = 0)
			dot(empty, r)
		} else {
			dot(ink[k], r)
			if r.AgeH >= 0 && r.AgeH < neGlyphFreshH {
				dot(fresh[int(r.AgeH)], r)
			}
		}
		if r.Obs {
			dot(obs[k], r)
		}
		if r.Chg > 0 {
			dot(chg[neGlyphChgBucket(r.Chg)], r)
		} else if r.Cand > 0 {
			dot(cand[neGlyphChgBucket(r.Cand)], r)
		}
		if r.Chg > 0 || r.Cand > 0 {
			dot(halo, r)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="glyph" viewBox="0 0 %d %d" xmlns="http://www.w3.org/2000/svg" role="img" aria-label="Austria, one dot per H3 res-7 cell: %s of %s cells hold a declared layer digested by peers; darker = digested more often; blue = reported in the last 6 hours">`,
		neGlyphW, neGlyphFrame.h, neFmtInt(g.Held), neFmtInt(len(g.Rows)))
	fmt.Fprintf(&b, `<g class="dt" fill="none" stroke-linecap="round" stroke-width="%d">`, neGlyphDot)
	if empty.Len() > 0 {
		fmt.Fprintf(&b, `<path class="e" d="%s"/>`, empty.String())
	}
	for k, p := range ink {
		if p.Len() > 0 {
			fmt.Fprintf(&b, `<path class="h" opacity="%.2f" d="%s"/>`, (float64(k)+0.5)/buckets, p.String())
		}
	}
	for h, p := range fresh {
		if p.Len() > 0 {
			fmt.Fprintf(&b, `<path class="n" opacity="%.2f" d="%s"/>`, 0.42*(1-float64(h)/neGlyphFreshH), p.String())
		}
	}
	// view overlays (hidden until the view control selects them; stroke-opacity
	// carries the bucket so the CSS can switch the layer without touching it)
	// observed carries the same depth ramp as the declared ink (floored so a
	// once-digested cell still reads teal); change gets a soft halo under the
	// dot so the sparse cells show at country scale
	for k, p := range obs {
		if p.Len() > 0 {
			fmt.Fprintf(&b, `<path class="o" opacity="%.2f" d="%s"/>`, neGlyphObsOpacity(k, buckets), p.String())
		}
	}
	if halo.Len() > 0 {
		fmt.Fprintf(&b, `<path class="ch" d="%s"/>`, halo.String())
	}
	for k, p := range cand {
		if p.Len() > 0 {
			fmt.Fprintf(&b, `<path class="ca" stroke-opacity="%.2f" d="%s"/>`, neGlyphCandBuckets[k], p.String())
		}
	}
	for k, p := range chg {
		if p.Len() > 0 {
			fmt.Fprintf(&b, `<path class="c" stroke-opacity="%.2f" d="%s"/>`, neGlyphChgBuckets[k], p.String())
		}
	}
	b.WriteString(`</g><circle class="ping" r="22" cx="0" cy="0"/><path class="cur" d=""/></svg>`)
	return b.String()
}

// neGlyphObsOpacity — observed overlay opacity for ink bucket k of n: the
// declared depth ramp, floored at 0.22 and stretched over the lower half of
// the buckets (where nearly all cells sit) so the teal reads as depth, not as
// a flat fill (mirrored in the client's hex faces).
func neGlyphObsOpacity(k, n int) float64 {
	return 0.22 + 0.78*math.Min(1, (float64(k)+0.5)/(float64(n)/2))
}

// neGlyphChgBuckets — stroke/fill opacity of a change cell by how many
// res-12 cells inside it carry confirmed change: 1 · 2–4 · 5–9 · 10+.
var neGlyphChgBuckets = []float64{0.45, 0.65, 0.85, 1}

// neGlyphCandBuckets — the same split for cells that only carry candidate
// change (one sighting): the same hue, clearly lighter than any confirmed
// bucket (the card's g-cand swatch is the vermilion at .45).
var neGlyphCandBuckets = []float64{0.2, 0.26, 0.32, 0.38}

func neGlyphChgBucket(n int) int {
	switch {
	case n >= 10:
		return 3
	case n >= 5:
		return 2
	case n >= 2:
		return 1
	}
	return 0
}

// neGlyphJSON — the tooltip data: one compact array per cell
// [x, y, chunks, passes_sum, reports, age_h, obs, change_cells, kg_index, bands, candidate_cells]
// where bands = chunks per pass band [1st, 2nd, 3rd–4th, 5th–9th, 10th+]
// (trailing zeros dropped) — the same depth split the daily chart's card shows.
func neGlyphJSON(g *neGlyph) []byte {
	var b bytes.Buffer
	fr := &neGlyphFrame
	fmt.Fprintf(&b, `{"as_of":%q,"res":%d,"w":%d,"h":%d,"chunks_per_cell":%d,"fresh_hours":%d,"alpha":%g,"held":%d,"observed":%d,"change":%d,"candidate":%d,"hex":{"rot":[%.5g,%.5g,%.5g],"r":[%.5g,%.5g,%.5g]},"proj":%s,"fields":["x","y","chunks","passes_sum","reports","age_h","observed","change_cells","kg","bands","candidate_cells"],"bands":["1st","2nd","3rd–4th","5th–9th","10th+"],"kgs":`,
		g.AsOf, neGlyphRes, neGlyphW, fr.h, neGlyphChunksPer, int(neGlyphFreshH), neGlyphAlpha, g.Held, g.Obs, g.Chg, g.Cand,
		fr.hexRot[0], fr.hexRot[1], fr.hexRot[2], fr.hexR[0], fr.hexR[1], fr.hexR[2], neGlyphProjJSON())
	kj, _ := json.Marshal(g.KGs)
	b.Write(kj)
	b.WriteString(`,"kg_codes":`)
	kc, _ := json.Marshal(g.KGCodes)
	b.Write(kc)
	b.WriteString(`,"cells":[`)
	for i := range g.Rows {
		r := &g.Rows[i]
		if i > 0 {
			b.WriteByte(',')
		}
		if r.Chunks == 0 && !r.Obs && r.Chg == 0 && r.Cand == 0 { // not digested, nothing observed: position only (the card says so)
			fmt.Fprintf(&b, "[%d,%d]", r.X, r.Y)
			continue
		}
		age := strconv.Itoa(int(r.AgeH + 0.5))
		if r.AgeH < 0 {
			age = "-1"
		} else if r.AgeH < neGlyphFreshH {
			age = strconv.FormatFloat(float64(r.AgeH), 'f', 1, 32)
		}
		o := 0
		if r.Obs {
			o = 1
		}
		fmt.Fprintf(&b, "[%d,%d,%d,%d,%d,%s,%d,%d,%d,%s", r.X, r.Y, r.Chunks, r.Passes, r.Reports, age, o, r.Chg, r.KG, neGlyphBandsJSON(r.Bands))
		if r.Cand > 0 {
			fmt.Fprintf(&b, ",%d", r.Cand)
		}
		b.WriteByte(']')
	}
	b.WriteString("]}")
	return b.Bytes()
}

// neGlyphProjJSON — the inverse projection the card uses to turn a cell's
// viewBox (x, y) into lon/lat (see neGlyphFrame).
func neGlyphProjJSON() string {
	fr := &neGlyphFrame
	return fmt.Sprintf(`{"scale":%.6g,"minx":%.8g,"miny":%.8g,"kx":%.8g,"pad":%d}`, fr.scale, fr.minx, fr.miny, fr.kx, neGlyphPad)
}

// neGlyphBandsJSON — "[12,3,1]": pass-band chunk counts, trailing zeros dropped.
func neGlyphBandsJSON(b [nePassBandN]int) string {
	n := nePassBandN
	for n > 0 && b[n-1] == 0 {
		n--
	}
	var sb strings.Builder
	sb.WriteByte('[')
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.Itoa(b[i]))
	}
	sb.WriteByte(']')
	return sb.String()
}

// NEGlyphStats — size line for /api/v1/debug/inflight (memory contract).
func NEGlyphStats() map[string]interface{} {
	g := neGlyphCurrent()
	if g == nil {
		return map[string]interface{}{"built": false}
	}
	return map[string]interface{}{"built": true, "as_of": g.AsOf, "cells": len(g.Rows), "held": g.Held, "observed": g.Obs, "change": g.Chg, "candidate": g.Cand,
		"svg_bytes": len(g.SVG), "json_bytes": len(g.JSON), "kg_index_bytes": len(g.KGIndex), "bytes": g.Bytes, "slots": 1}
}

// handleNEGlyph — GET /api/v1/ne/glyph: the tooltip data (JSON, 5 min).
func handleNEGlyph(w http.ResponseWriter, r *http.Request) {
	neCoverageStats(false)
	g := neGlyphCurrent()
	if g == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{"error": "glyph_not_built", "retry_after_s": 30})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(g.JSON)
}

// neGlyphKGIndex — the search box's index: every KG of the universe as
// [code, name, gemeinde, district, x0, y0, x1, y1, gkz] with the bbox in viewBox
// units (same frame as the cells), so the client can find a KG by name or
// code, fly to it and pick the cells under it without a second lookup.
// Built with the glyph (names and boxes are the in-memory KG snapshots).
func neGlyphKGIndex(names []kgNameRow) []byte {
	fr := &neGlyphFrame
	box := map[string]kgBox{}
	for _, b := range kgBoxSnapshot() {
		box[b.code] = b
	}
	px := func(lon, lat float64) (int, int) {
		return int(math.Round((lon*fr.kx-fr.minx)*fr.scale)) + neGlyphPad, int(math.Round((-lat-fr.miny)*fr.scale)) + neGlyphPad
	}
	var b bytes.Buffer
	b.WriteString(`{"fields":["code","name","gemeinde","district","x0","y0","x1","y1","gkz"],"kgs":[`)
	n := 0
	for i := range names {
		r := &names[i]
		bb, ok := box[r.code]
		if !ok {
			continue
		}
		x0, y0 := px(bb.minLon, bb.maxLat)
		x1, y1 := px(bb.maxLon, bb.minLat)
		if n > 0 {
			b.WriteByte(',')
		}
		n++
		row, _ := json.Marshal([]interface{}{r.code, r.kgName, r.gemeindeName, r.districtName, x0, y0, x1, y1, r.gemeindeCode})
		b.Write(row)
	}
	b.WriteString("]}")
	return b.Bytes()
}

// handleNEGlyphKGs — GET /api/v1/ne/glyph/kgs: the search index (JSON, 1 h).
func handleNEGlyphKGs(w http.ResponseWriter, r *http.Request) {
	neCoverageStats(false)
	g := neGlyphCurrent()
	if g == nil || len(g.KGIndex) == 0 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{"error": "glyph_not_built", "retry_after_s": 30})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(g.KGIndex)
}

// ── landing page block ───────────────────────────────────────────────────────
//
// No heading, no prose: the glyph sits under the title and speaks for itself.
// Progressive enhancement — the SVG is static HTML (works without script); the
// script adds a pointer/keyboard cursor with a card, data fetched from
// /api/v1/ne/glyph on first touch only. The view control (declared · observed
// · change) switches overlays inside the one figure; without script the
// declared view shows.
func neGlyphHTML() string {
	g := neGlyphCurrent()
	if g == nil {
		return ""
	}
	var b strings.Builder
	fr := &neGlyphFrame
	b.WriteString(`<figure class="glyph" data-w="` + strconv.Itoa(neGlyphW) + `" data-h="` + strconv.Itoa(int(fr.h)) + `" data-per="` + strconv.Itoa(neGlyphChunksPer) + `" data-fresh="` + strconv.Itoa(int(neGlyphFreshH)) + `" data-alpha="` + strconv.FormatFloat(neGlyphAlpha, 'g', -1, 64) +
		fmt.Sprintf(`" data-hex="%.5g,%.5g,%.5g,%.5g,%.5g,%.5g" data-picks='`, fr.hexRot[0], fr.hexRot[1], fr.hexRot[2], fr.hexR[0], fr.hexR[1], fr.hexR[2]) + strings.ReplaceAll(neGlyphPicksJSON(g), "'", "&#39;") + `' data-proj='` + neGlyphProjJSON() + `'>
<div class="glyph-row"><div class="glyph-main" tabindex="0" aria-label="Austria as one dot per H3 res-7 cell; arrow keys move between cells, + and − zoom, Escape resets">` + g.SVG + `
<div class="glyph-hint" hidden></div><div class="glyph-peek" aria-hidden="true"></div></div><div class="glyph-tt" role="tooltip" hidden></div>
<div class="glyph-side"><span class="gt-dt" aria-live="polite" hidden></span><div class="glyph-tools"><div class="gt-s"><button type="button" class="gt-b gt-q" aria-label="find a KG, place or address" title="find a KG, Gemeinde, Bezirk, address or name ( / )"><svg viewBox="0 0 16 16" aria-hidden="true"><circle cx="6.8" cy="6.8" r="4.3"/><path d="M10 10l4 4"/></svg><span class="gt-t">find</span></button><span class="gt-ch" role="list" aria-label="selected" hidden></span><span class="gt-g" aria-hidden="true"></span><input type="search" class="gt-in" placeholder="KG, place, address or code" autocomplete="off" autocorrect="off" autocapitalize="off" spellcheck="false" enterkeyhint="go" aria-label="find a KG, place or address" aria-autocomplete="list" aria-expanded="false" tabindex="-1"><button type="button" class="gt-b gt-x" aria-label="close" tabindex="-1"><svg viewBox="0 0 16 16" aria-hidden="true"><path d="M4 4l8 8M12 4l-8 8"/></svg></button></div>` + neGlyphFiltBtn + `<button type="button" class="gt-b gt-h" aria-label="recent places" title="recent"><svg viewBox="0 0 16 16" aria-hidden="true"><path d="M2.5 8a5.5 5.5 0 1 0 1.6-3.9"/><path d="M2.2 1.8v3.2h3.2"/><path d="M8 5v3.3l2.2 1.3"/></svg><span class="gt-t">recent</span></button>` + neGlyphExportBtn + `<button type="button" class="gt-b gt-d" hidden tabindex="-1" aria-pressed="false" aria-label="full resolution: every H3 res-12 cell coloured by its dominant land use" title="full resolution · every res-12 cell (≈300 m²) by dominant land use"><svg viewBox="0 0 24 24" aria-hidden="true" class="i-on"><path d="M3 7V5a2 2 0 0 1 2-2h2"/><path d="M17 3h2a2 2 0 0 1 2 2v2"/><path d="M21 17v2a2 2 0 0 1-2 2h-2"/><path d="M7 21H5a2 2 0 0 1-2-2v-2"/><circle cx="12" cy="12" r="3"/><path d="m16 16-1.9-1.9"/></svg><svg viewBox="0 0 24 24" aria-hidden="true" class="i-off"><path d="m14 10 7-7"/><path d="M20 10h-6V4"/><path d="m3 21 7-7"/><path d="M4 14h6v6"/></svg><span class="gt-t">full res</span></button><button type="button" class="gt-b gt-f" aria-pressed="false" aria-label="full screen" title="full screen ( F )"><svg viewBox="0 0 16 16" aria-hidden="true" class="i-on"><path d="M2.5 6V2.5H6"/><path d="M10 2.5h3.5V6"/><path d="M13.5 10v3.5H10"/><path d="M6 13.5H2.5V10"/></svg><svg viewBox="0 0 16 16" aria-hidden="true" class="i-off"><path d="M6 2.5V6H2.5"/><path d="M10 2.5V6h3.5"/><path d="M13.5 10H10v3.5"/><path d="M2.5 10H6v3.5"/></svg><span class="gt-t">full screen</span></button><ul class="gt-l" role="listbox" hidden></ul></div>` + neGlyphFiltHTML + `
<div class="glyph-view" aria-label="picture"><span class="gv-p" role="radiogroup" aria-label="profile"><button type="button" class="gv-b" data-v="" role="radio" aria-checked="true" title="declared land use digested by peers · darker = digested more often ( v )"><span class="h">declared</span><i class="ld" aria-hidden="true"><u>·</u><u>·</u><u>·</u></i></button><span class="gt-sep">·</span><button type="button" class="gv-b" data-v="obs" role="radio" aria-checked="false" title="observed land use · LiDAR &amp; satellite, posted by {OBS_NAME} ( v )"><span class="o">observed</span><i class="ld" aria-hidden="true"><u>·</u><u>·</u><u>·</u></i></button></span><span class="gv-o" role="group" aria-label="overlays"><button type="button" class="gv-t gv-chg" aria-pressed="false" aria-label="change" title="change · cells whose declared use moved between digests · darker = more cells confirmed · pale = candidate, seen once ( c )"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M22 12h-2.48a2 2 0 0 0-1.93 1.46l-2.35 8.36a.25.25 0 0 1-.48 0L9.24 2.18a.25.25 0 0 0-.48 0l-2.35 8.36A2 2 0 0 1 4.49 12H2"/></svg><span class="gt-t">change</span></button><button type="button" class="gv-t gv-dif" aria-pressed="false" aria-label="diff" title="diff · where the register and the LiDAR disagree about the same cell · solid = structures declared, none in the LiDAR · dots = structures in the LiDAR, none declared · ring = declared forest, currently no trees (full resolution) ( d )"><svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="18" cy="18" r="3"/><circle cx="6" cy="6" r="3"/><path d="M13 6h3a2 2 0 0 1 2 2v7"/><path d="M11 18H8a2 2 0 0 1-2-2V9"/></svg><span class="gt-t">diff</span></button><button type="button" class="gv-t gv-sub" aria-pressed="false" aria-label="subsidy claims" title="subsidy claims · INVEKOS Schläge 2025 as drawn · with change on: new / redrawn / gone vs 2024 ( s )"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="m10 11 11 .9a1 1 0 0 1 .8 1.1l-.665 4.158a1 1 0 0 1-.988.842H20"/><path d="M16 18h-5"/><path d="M18 5a1 1 0 0 0-1 1v5.573"/><path d="M3 4h8.129a1 1 0 0 1 .99.863L13 11.246"/><path d="M4 11V4"/><path d="M7 15h.01"/><path d="M8 10.1V4"/><circle cx="18" cy="18" r="2"/><circle cx="7" cy="15" r="5"/></svg></button><button type="button" class="gv-t gv-tree" aria-pressed="false" aria-label="tree heights" title="trees · every tree as a ring of its crown, greener = taller (full resolution) ( t )"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="m17 14 3 3.3a1 1 0 0 1-.7 1.7H4.7a1 1 0 0 1-.7-1.7L7 14h-.3a1 1 0 0 1-.7-1.7L9 9h-.2A1 1 0 0 1 8 7.3L12 3l4 4.3a1 1 0 0 1-.8 1.7H15l3 3.3a1 1 0 0 1-.7 1.7H17Z"/><path d="M12 22v-3"/></svg></button><button type="button" class="gv-t gv-relief" aria-pressed="false" aria-label="relief" title="relief · hillshade from the 25 m terrain model (full resolution) ( r )"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M2 19l6.5-11 4.2 7 2.6-3.6L22 19Z"/><path d="M7.2 13.4l1.3-1.8 1.3 1.8"/></svg></button><button type="button" class="gv-t gv-ortho" aria-pressed="false" aria-label="orthophoto" title="photo · basemap.at orthophoto beneath the cells (near tiles) · hold for the opacity ( o )"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M21.54 15H17a2 2 0 0 0-2 2v4.54"/><path d="M7 3.34V5a3 3 0 0 0 3 3a2 2 0 0 1 2 2c0 1.1.9 2 2 2a2 2 0 0 0 2-2c0-1.1.9-2 2-2h3.17"/><path d="M11 21.95V18a2 2 0 0 0-2-2a2 2 0 0 1-2-2v-1a2 2 0 0 0-2-2H2.05"/><circle cx="12" cy="12" r="10"/></svg></button><button type="button" class="gv-t gv-rd" aria-pressed="false" aria-label="roads" title="roads · OSM roads and rail in one quiet ink, darker the more remote (full resolution) · hold for the opacity ( w )"><svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="6" cy="19" r="3"/><path d="M9 19h8.5a3.5 3.5 0 0 0 0-7h-11a3.5 3.5 0 0 1 0-7H15"/><circle cx="18" cy="5" r="3"/></svg></button></span></div><div class="gv-op gv-op-rd" role="group" aria-label="roads opacity" hidden><span class="l">opacity</span><input type="range" min="10" max="100" step="5" value="70" aria-label="roads opacity"><span class="v">70 %</span></div><div class="gv-op gv-op-ot" role="group" aria-label="photo opacity" hidden><span class="l">opacity</span><input type="range" min="10" max="100" step="5" value="50" aria-label="photo opacity"><span class="v">50 %</span></div><span class="gt-n" aria-live="polite"></span></div></div>
<div class="glyph-time" hidden aria-label="change over time"></div>
<div class="glyph-tip" role="status" aria-live="polite"><div></div><div class="glyph-grip" aria-hidden="true" title="drag · double-click snaps back"><i></i></div><button type="button" class="glyph-dk" aria-pressed="false" aria-label="dock the card below the map" title="dock below the map"><svg viewBox="0 0 16 16" aria-hidden="true" class="i-dn"><rect x="2" y="2" width="12" height="12" rx="1.6"/><path d="M2 9.5h12"/></svg><svg viewBox="0 0 16 16" aria-hidden="true" class="i-up"><rect x="2" y="2" width="12" height="12" rx="1.6"/><rect x="7.2" y="7.2" width="5" height="3.8" rx=".7" fill="currentColor" stroke="none"/></svg></button><button type="button" class="glyph-x" aria-label="close and show all of Austria" title="close · esc">×</button><button type="button" class="glyph-more" aria-expanded="false" aria-label="show detail" title="more"><svg viewBox="0 0 16 16" aria-hidden="true"><path d="M4 3.5l4 4 4-4M4 8.5l4 4 4-4"/></svg></button></div>
<button type="button" class="glyph-fsx" aria-label="leave full screen" title="leave full screen ( esc )" tabindex="-1" hidden><svg viewBox="0 0 16 16" aria-hidden="true"><path d="M4 4l8 8M12 4l-8 8"/></svg></button>
<svg width="0" height="0" aria-hidden="true" style="position:absolute"><defs><pattern id="ng-h2" patternUnits="userSpaceOnUse" width="5" height="5" patternTransform="rotate(45)"><line x1="0" y1="0" x2="0" y2="5" class="hatch"/></pattern><pattern id="ng-h3" patternUnits="userSpaceOnUse" width="5" height="5" patternTransform="rotate(45)"><line x1="0" y1="0" x2="0" y2="5" class="hatch"/><line x1="0" y1="0" x2="5" y2="0" class="hatch"/></pattern><pattern id="ng-h4" patternUnits="userSpaceOnUse" width="4" height="4"><circle cx="2" cy="2" r=".9" class="dot"/></pattern><g id="ng-tr"><circle class="ht" r="8"/><circle class="gl" r="3.2"/><circle class="rg" r="3.2"/></g></defs></svg>
</figure>
` + peerText(neGlyphScript) + "\n")
	return b.String()
}

const neGlyphCSS = `
figure.glyph{margin:1rem 0 1.6rem;position:relative}
.glyph-row{display:flex;flex-wrap:wrap;align-items:flex-end;gap:6px 14px}
.glyph-main{flex:1 1 100%;min-width:0;border-radius:4px;outline:none;position:relative;-webkit-user-select:none;user-select:none;-webkit-touch-callout:none;-webkit-tap-highlight-color:transparent}
.glyph-main:focus-visible{box-shadow:0 0 0 2px var(--acc)}
figure.glyph{--gink:var(--cov);--gnew:var(--new);--gnewt:var(--newt);--gobs:var(--obs);--gchg:var(--chg);--gsub:#e69f00;--gdif:#a82c8f}@media(prefers-color-scheme:dark){figure.glyph{--gsub:#f0b429;--gdif:#e07ccb}}
/* subsidy layer (INVEKOS, {SUB_NAME}): ochre .s dots / faces under the ink, the ink steps back like under a view; full res: .ov-sub wash under declared/observed, .ov-subc lineage strokes under change */
svg.glyph .dt .s,svg.glyph .hx .s,svg.glyph .hi .ov-sub,svg.glyph .hi .ov-subc{display:none}svg.glyph.v-sub .dt .s{display:inline;stroke:var(--gsub)}svg.glyph.v-sub .hx .s{display:inline;fill:var(--gsub)}svg.glyph.v-sub .dt .h{opacity:.14}svg.glyph.v-sub .hx .h{fill-opacity:.14}svg.glyph.v-sub .dt .n,svg.glyph.v-sub .hx .n{display:none}
svg.glyph.v-sub:not(.v-chg) .hi .ov-sub,svg.glyph.v-sub.v-chg .hi .ov-subc{display:inline}svg.glyph.v-sub:not(.v-chg):not(.v-obs) .hi .b,svg.glyph.v-sub:not(.v-chg).v-obs .hi .ov-obs{opacity:.55}svg.glyph.v-sub.hi-near:not(.v-chg):not(.v-obs) .hi .b,svg.glyph.v-sub.sub-fine:not(.v-chg):not(.v-obs) .hi .b,svg.glyph.v-sub.hi-near:not(.v-chg).v-obs .hi .ov-obs,svg.glyph.v-sub.sub-fine:not(.v-chg).v-obs .hi .ov-obs{opacity:.8}
figure.glyph .s{color:var(--gsub)}.glyph-tip .g-sub,.glyph-peek .g-sub,.glyph-time .g-sub{fill:var(--gsub)}.glyph-tip .g-s1{fill:var(--gsub)}.glyph-tip .g-s2{fill:var(--gsub);opacity:.62}.glyph-tip .g-s3{fill:var(--gsub);opacity:.42}.glyph-tip .g-s4{fill:var(--gsub);opacity:.85}.glyph-tip .g-s5{fill:var(--gsub);opacity:.72}.glyph-tip .g-s6{fill:var(--gsub);opacity:.3}.glyph-tip .g-s7{fill:var(--gsub);opacity:.2}.glyph-tip .sh em.s{color:var(--gsub)}.glyph-tip .lg12 .sb .sh{margin-top:.55em;padding-top:.45em;border-top:1px solid var(--rule)}.glyph-tip .lg12>.sb:first-child .sh{margin-top:0;padding-top:0;border-top:0}.glyph-tip .lg12 .sh.ovh{margin-top:.55em;padding-top:.45em;border-top:1px solid var(--rule)}.glyph-tip .it.s{color:var(--gsub)}.glyph-tip .it.s .mut{color:var(--mut)}.glyph-tip .sw-ln{overflow:visible;outline:0;width:10px;height:10px;vertical-align:-1px}svg.glyph.v-sub .hi .ov-sub image,svg.glyph.v-sub .hi .ov-subc image{image-rendering:auto}
/* the res-7 wash of the wide tiles must not show through the near tiles (the Schlag fills leave the ground transparent) */
svg.glyph.hi-near .hi .ov-sub [data-lod="2"],svg.glyph.hi-near .hi .ov-subc [data-lod="2"]{display:none}
svg.glyph{display:block;width:100%;height:auto;overflow:hidden}
svg.glyph .e{stroke:var(--rule);opacity:.4}svg.glyph .h{stroke:var(--gink)}svg.glyph .n{stroke:var(--gnew)}
/* views: the observed / change overlays live in the same picture (dots, hex faces, full-res tiles) and the ink steps back while one is on */
svg.glyph .dt .o,svg.glyph .dt .c,svg.glyph .dt .ca,svg.glyph .dt .ch,svg.glyph .hx .o,svg.glyph .hx .c,svg.glyph .hx .ca,svg.glyph .hi .ov{display:none}
svg.glyph.v-obs .dt .h,svg.glyph.v-chg .dt .h{opacity:.14}svg.glyph.v-obs .hx .h,svg.glyph.v-chg .hx .h{fill-opacity:.14}
svg.glyph.v-obs .dt .n,svg.glyph.v-chg .dt .n,svg.glyph.v-obs .hx .n,svg.glyph.v-chg .hx .n{display:none}
svg.glyph.v-obs .dt .o{display:inline;stroke:var(--gobs)}svg.glyph.v-obs .hx .o{display:inline;fill:var(--gobs)}
svg.glyph.v-chg .dt .c,svg.glyph.v-chg .dt .ca{display:inline;stroke:var(--gchg)}svg.glyph.v-chg .hx .ca{display:inline;fill:var(--gchg)}svg.glyph.v-chg .dt .ch{display:inline;stroke:var(--gchg);stroke-width:120;opacity:.16}svg.glyph.v-chg .hx .c{display:inline;fill:var(--gchg)}
svg.glyph.v-obs .hi .ov-obs,svg.glyph.v-chg .hi .ov-chg,svg.glyph.v-dif.hi-near .hi .ov-dif,svg.glyph.v-rd.hi-near .hi .ov-rd{display:inline}svg.glyph .hi .b,svg.glyph .hi .ov-obs{transition:opacity .4s}svg.glyph.v-obs .hi .b{opacity:.3}svg.glyph.v-chg:not(.v-obs) .hi .b,svg.glyph.v-dif.hi-near:not(.v-obs) .hi .b{opacity:.4}svg.glyph.v-chg.v-obs .hi .ov-obs,svg.glyph.v-dif.hi-near.v-obs .hi .ov-obs{opacity:.4}svg.glyph .hi .ov-dif image{shape-rendering:crispEdges}
svg.glyph.v-tree .hi .ov-tree{display:inline}svg.glyph .hi .ov-tree .ap{opacity:0;transition:opacity .4s}svg.glyph .hi .ov-tree .ap.in{opacity:1}svg.glyph .hi .ov-tree .ap image{pointer-events:none;opacity:1;transition:none}svg.glyph .hi .ov-tree .ap use{stroke-width:2.2px;pointer-events:none}#ng-tr .ht{fill:none;stroke:none}#ng-tr .gl{fill:none;stroke:#000;stroke-opacity:.3;stroke-width:4.4px;vector-effect:non-scaling-stroke;pointer-events:none}#ng-tr .rg{fill:none;stroke:currentColor;stroke-width:inherit;vector-effect:non-scaling-stroke;pointer-events:none}svg.glyph .hi .ov-tree .ap use.sel{color:#fff}svg.glyph .hi .ov-tree .ap use.fl{color:var(--gchg)}svg.glyph .hi .ov-tree .ap use.ok{color:var(--gobs)}svg.glyph.v-tree .hi{cursor:default}.glyph-tip .fg{margin-top:.45em;font-size:.92em;line-height:1.5}.glyph-tip .fg-b,.glyph-tip .fg-c,.glyph-tip .fg-x,.glyph-tip .fg-s{appearance:none;-webkit-appearance:none;background:none;border:0;padding:0;font:inherit;color:var(--mut);cursor:pointer;line-height:inherit;pointer-events:auto;border-radius:3px;transition:color .15s,background .15s}.glyph-tip .fg-b:hover,.glyph-tip .fg-b:focus-visible{color:var(--fg);outline:none}.glyph-tip .fg-r{display:flex;flex-wrap:wrap;align-items:center;gap:.15em .25em}.glyph-tip .fg-q{color:var(--mut);margin-right:.2em}.glyph-tip .fg-c{padding:0 .45em;border:1px solid transparent}.glyph-tip .fg-c:hover,.glyph-tip .fg-c:focus-visible{color:var(--fg);border-color:var(--rule);outline:none}.glyph-tip .fg-c[aria-pressed=true]{color:var(--bg);background:var(--fg);border-color:var(--fg)}.glyph-tip .fg-x{margin-left:auto;padding:0 .3em;font-size:1.1em;line-height:1}.glyph-tip .fg-x:hover{color:var(--fg)}.glyph-tip .fg-n{margin-top:.25em;flex-wrap:nowrap}.glyph-tip .fg-n input{flex:1;min-width:0;appearance:none;-webkit-appearance:none;background:transparent;border:0;border-bottom:1px solid var(--rule);padding:.1em 0;font:inherit;color:var(--fg);outline:none;pointer-events:auto;border-radius:0}.glyph-tip .fg-n input:focus{border-bottom-color:var(--fg)}.glyph-tip .fg-n input::placeholder{color:var(--mut);opacity:.8}.glyph-tip .fg-s{color:var(--fg);padding:0 .45em;border:1px solid var(--rule)}.glyph-tip .fg-s:hover{background:var(--code)}.glyph-tip .fg-s:disabled{color:var(--mut);cursor:default;background:none}.glyph-tip .fg-e{color:var(--gchg);white-space:nowrap}.glyph-tip .fg-ok{color:var(--fg)}.glyph-tip .em.c,.glyph-tip .sh em.c{color:var(--gchg)}
.glyph-view{display:flex;align-items:center;gap:.3em;white-space:nowrap;margin-right:auto}.glyph-view .gv-p,.glyph-view .gv-o{display:inline-flex;align-items:center;gap:.3em}.glyph-view .gv-o{gap:.2em;margin-left:.9ch}
.gv-b{appearance:none;-webkit-appearance:none;background:none;border:0;padding:.1em .35em;margin:0;border-radius:3px;color:var(--mut);cursor:pointer;display:inline-flex;align-items:center;gap:.4em;font:inherit;line-height:inherit;transition:color .15s,background .15s;-webkit-tap-highlight-color:transparent}
.gv-b{position:relative}.glyph-view .gt-sep{display:inline-block;width:2ch;text-align:center;flex:0 0 auto}
.gv-b .ld{position:absolute;left:calc(100% + .3em);width:2ch;top:0;bottom:0;display:flex;align-items:center;justify-content:center;font-style:normal;letter-spacing:0;white-space:nowrap;opacity:0;transition:opacity .2s;pointer-events:none}
.gv-b .ld u{text-decoration:none;opacity:.25;animation:gdot 1.4s ease-in-out infinite;animation-play-state:paused}.gv-b .ld u:nth-child(2){animation-delay:.2s}.gv-b .ld u:nth-child(3){animation-delay:.4s}
.gv-b.busy .ld{opacity:1}.gv-b.busy .ld u{animation-play-state:running}.gv-b.busy+.gt-sep{opacity:0}.gt-sep{transition:opacity .2s}@keyframes gvpulse{0%,100%{opacity:1}50%{opacity:.35}}.glyph-view .gv-t.busy{animation:gvpulse 1.4s ease-in-out infinite}
figure.glyph .glyph-view .gv-b span{color:inherit;transition:color .15s}figure.glyph .glyph-view .gv-b[aria-checked=true] .h{color:var(--fg)}figure.glyph .glyph-view .gv-b[aria-checked=true] .o{color:var(--gobs)}figure.glyph .glyph-view .gv-b[aria-checked=true] .c{color:var(--gchg)}figure.glyph .gv-b.busy span{color:var(--mut)}
.gv-b:hover,.gv-b:focus-visible{color:var(--fg);background:var(--code);outline:none}.gv-b[aria-checked=true]{color:var(--fg)}
svg.glyph .cur{fill:none;stroke:var(--fg);stroke-width:1.6px;stroke-linejoin:round;vector-effect:non-scaling-stroke;opacity:0;transition:opacity .18s}svg.glyph.on .cur{opacity:.85}
svg.glyph .hx path{stroke:var(--bg);stroke-linejoin:round;vector-effect:non-scaling-stroke}svg.glyph .hx .h{fill:var(--gink)}svg.glyph .hx .n{fill:var(--gnew)}svg.glyph .hx .e{fill:none;stroke:var(--rule);stroke-width:.8px;stroke-opacity:.6}
svg.glyph .ping{fill:none;stroke:var(--fg);stroke-width:1px;vector-effect:non-scaling-stroke;opacity:0;transform-box:fill-box;transform-origin:center}
svg.glyph .ping.go{animation:gping 1.1s cubic-bezier(.2,.6,.3,1) forwards}
@keyframes gping{0%{opacity:.45;transform:scale(1)}100%{opacity:0;transform:scale(4.5)}}
.glyph-main.js{cursor:crosshair;touch-action:pan-y}.glyph-main.js.zoomed{cursor:grab;touch-action:none}.glyph-main.js.drag{cursor:grabbing}
@media(hover:none){figure.glyph:not(.fs) .glyph-main.js.zoomed{touch-action:pan-y}}
.glyph-main.live{overflow:hidden}svg.glyph{transform-origin:0 0}.glyph-main.live svg.glyph{will-change:transform}
figure.glyph .o{color:var(--gobs)}figure.glyph .c{color:var(--gchg)}figure.glyph .n{color:var(--gnewt)}
.glyph-hint{position:absolute;left:8px;bottom:8px;font-size:.74em;color:var(--mut);background:var(--bg);border:1px solid var(--rule);border-radius:3px;padding:.15em .5em;opacity:0;transition:opacity .25s;pointer-events:none}
.glyph-hint.on{opacity:.9}
/* floating card = frosted glass: the map reads through it (62 % page colour over a 14 px blur, colours kept saturated), a hairline border with a light inner edge, a soft lifted shadow; it sits 8 px inside the map box (place) so the map frames it */
.glyph-tip{--float:1;position:absolute;top:0;left:0;z-index:3;pointer-events:none;background:var(--bg);background:color-mix(in srgb,var(--bg) 62%,transparent);-webkit-backdrop-filter:blur(14px) saturate(1.3);backdrop-filter:blur(14px) saturate(1.3);border:1px solid color-mix(in srgb,var(--fg) 13%,transparent);border-radius:9px;padding:.55em .75em .6em;font-size:.82em;line-height:1.4;max-width:100%;white-space:normal;box-shadow:inset 0 0 0 1px rgba(255,255,255,.35),0 1px 2px rgba(0,0,0,.04),0 12px 32px -10px rgba(0,0,0,.22);opacity:0;transition:opacity .22s,transform .32s cubic-bezier(.2,.7,.3,1)}.glyph-tip.x:not(.tour){pointer-events:auto}
/* grip: a thin pill at the top; drag it (or the header) to move the card anywhere — off the map too; double-click snaps it back; the moved position holds until the card closes */
.glyph-tip .glyph-grip{position:absolute;top:0;left:0;right:0;height:1.1em;display:none;align-items:flex-start;justify-content:center;padding-top:.3em;cursor:grab;pointer-events:auto;touch-action:none;-webkit-user-select:none;user-select:none}
.glyph-tip .glyph-grip i{display:block;width:2.6em;height:3px;border-radius:2px;background:var(--mut);opacity:.28;transition:opacity .2s,width .2s}
.glyph-tip.x .glyph-grip{display:flex}.glyph-tip.x{padding-top:.8em}.glyph-tip .glyph-grip:hover i,.glyph-tip.moving .glyph-grip i{opacity:.6;width:3.2em}
.glyph-tip.x .hd,.glyph-tip.x .sub,.glyph-tip.x .sh{cursor:grab;pointer-events:auto}.glyph-tip.moving,.glyph-tip.moving .hd,.glyph-tip.moving .glyph-grip{cursor:grabbing}
.glyph-tip.moving{transition:opacity .22s;box-shadow:inset 0 0 0 1px rgba(255,255,255,.35),0 2px 4px rgba(0,0,0,.06),0 18px 44px -10px rgba(0,0,0,.3);-webkit-user-select:none;user-select:none}
.glyph-tip.moved{z-index:7}.glyph-tip.clip{display:flex;flex-direction:column}.glyph-tip.clip>div{flex:1 1 auto;min-height:0;overflow-y:auto;overscroll-behavior:contain;scrollbar-width:thin}
.glyph-tip .glyph-dk{position:absolute;top:.15em;right:2.05em;width:1.9em;height:1.9em;padding:0;border:0;border-radius:50%;background:transparent;color:var(--mut);font:inherit;font-size:1.15em;cursor:pointer;pointer-events:auto;opacity:0;transform:scale(.8);transition:opacity .2s,transform .2s,background .15s,color .15s;-webkit-tap-highlight-color:transparent;display:inline-flex;align-items:center;justify-content:center}
.glyph-tip .glyph-dk svg{width:1.05em;height:1.05em;fill:none;stroke:currentColor;stroke-width:1.5;stroke-linecap:round;stroke-linejoin:round}.glyph-tip .glyph-dk .i-up,.glyph-tip.dock .glyph-dk .i-dn{display:none}.glyph-tip.dock .glyph-dk .i-up{display:block}
.glyph-tip.x .glyph-dk{opacity:.55;transform:none}.glyph-tip.x .glyph-dk:hover,.glyph-tip.x .glyph-dk:focus-visible{opacity:1;color:var(--fg);background:var(--code);outline:none}.glyph-tip.x .hd{padding-right:4.8em}
/* docked: the card leaves the map and sits below it in the page flow — the same layout the narrow/touch view uses; the page grows by the card's height and nothing overlaps */
.glyph-tip.dock,.glyph-tip.dock.tour{--float:0;position:relative;transform:none!important;transition:opacity .25s;max-width:100%;white-space:normal;pointer-events:auto;margin-top:.45rem;min-height:7.6em;box-sizing:border-box;flex:1 1 100%;opacity:1;background:var(--bg);-webkit-backdrop-filter:none;backdrop-filter:none;box-shadow:none}
.glyph-tip.dock>div{width:auto}.glyph-tip.dock .glyph-grip{display:none!important}.glyph-tip.dock.x .hd{cursor:auto}.glyph-tip.dock.x{padding-top:.55em}
.glyph-tip.tour{transition:opacity .35s,transform .45s cubic-bezier(.2,.7,.3,1)}
.glyph-tip>div{overflow:hidden;transition:height .36s cubic-bezier(.2,.7,.3,1),opacity .16s ease}
.glyph-tip.on{opacity:1}.glyph-tip.follow{transition:opacity .22s}.glyph-tip.x .glyph-grip i{opacity:.4}
/* peek: the light hover tooltip while something is selected — the card stays, this one rides the cursor */
.glyph-peek{position:absolute;top:0;left:0;z-index:2;pointer-events:none;background:var(--bg);background:color-mix(in srgb,var(--bg) 82%,transparent);-webkit-backdrop-filter:blur(3px);backdrop-filter:blur(3px);border:1px solid var(--rule);border-radius:4px;padding:.3em .55em .35em;font-size:.78em;line-height:1.35;white-space:nowrap;box-shadow:0 1px 6px rgba(0,0,0,.08);opacity:0;transition:opacity .1s;will-change:transform}.glyph-peek.on{opacity:1}.glyph-peek b{color:var(--fg)}.glyph-peek .mut{color:var(--mut)}.glyph-peek .pl{font-size:.92em}.glyph-peek .sep{display:inline-block;width:.6em}.glyph-peek .pb{display:flex;width:11em;height:.4em;margin:.1em 0 .3em;gap:1px;background:var(--code);border-radius:2px;overflow:hidden}.glyph-peek .pb svg{display:block;height:100%;flex:0 0 auto}.glyph-peek .sw{width:.7em;height:.7em;vertical-align:-.05em;margin-right:.25em;outline:1px solid var(--rule)}.glyph-peek .o{color:var(--gobs)}.glyph-peek .c{color:var(--gchg)}.glyph-peek .s{color:var(--gsub)}.glyph-peek .pl.mut .lb{color:var(--mut)}.glyph-peek .pl.mut .mut{color:var(--mut)}.glyph-peek .pl.mut .sw{opacity:.75}
svg.glyph .hov{fill:none;stroke:var(--fg);stroke-width:1.2px;stroke-linejoin:round;vector-effect:non-scaling-stroke;opacity:0;pointer-events:none;transition:opacity .1s}svg.glyph .hov.on{opacity:.7}svg.glyph.hi .hov{stroke:#fff;stroke-width:1.5px;filter:drop-shadow(0 0 1px rgba(0,0,0,.8))}.glyph-tip .glyph-x{position:absolute;top:.15em;right:.15em;width:1.9em;height:1.9em;padding:0;border:0;border-radius:50%;background:transparent;color:var(--mut);font:inherit;font-size:1.15em;line-height:1;cursor:pointer;pointer-events:auto;opacity:0;transform:scale(.8);transition:opacity .2s,transform .2s,background .15s,color .15s;-webkit-tap-highlight-color:transparent}.glyph-tip.x .glyph-x{opacity:.55;transform:none}.glyph-tip.x .glyph-x:hover,.glyph-tip.x .glyph-x:focus-visible{opacity:1;color:var(--fg);background:var(--code);outline:none}.glyph-tip.x .glyph-x:active{transform:scale(.9)}.glyph-tip b{color:var(--fg)}.glyph-tip .mut{color:var(--mut)}
.glyph-tip>div{width:54ch;max-width:100%;font-variant-numeric:tabular-nums}.glyph-tip .mut{color:var(--mut)}
.glyph-tip .hd{display:flex;justify-content:space-between;align-items:baseline;gap:1em;white-space:nowrap}.glyph-tip .hd b{overflow:hidden;text-overflow:ellipsis}.glyph-tip .hd b:has(>.kgl){min-width:0}
/* the KG name in a cell card is a link to the KG itself: tap → the tour stops, the KG gets selected and the map flies to it (same as a find) */
.glyph-tip .kgl{appearance:none;border:0;background:none;font:inherit;font-weight:inherit;color:inherit;padding:0 .3em 0 0;margin:0;cursor:pointer;max-width:100%;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;text-decoration:underline dotted color-mix(in srgb,var(--mut) 55%,transparent);text-decoration-thickness:1px;text-underline-offset:.22em;border-radius:3px;transition:color .15s,text-decoration-color .15s,background .15s}
.glyph-tip .kgl::after{content:"\2315";font-weight:normal;color:var(--mut);font-size:.85em;margin-left:.4em;opacity:0;transition:opacity .15s}
.glyph-tip .kgl:hover,.glyph-tip .kgl:focus-visible{text-decoration-style:solid;text-decoration-color:var(--fg);outline:none}.glyph-tip .kgl:hover::after,.glyph-tip .kgl:focus-visible::after{opacity:1}
.glyph-tip .kgl:focus-visible{box-shadow:0 0 0 2px var(--code)}.glyph-tip .kgl:active{background:var(--code);color:var(--fg)}.glyph-tip .kgl.go{background:var(--code)}
@media(hover:none){.glyph-tip .kgl::after{opacity:.6}}.glyph-tip .age{color:var(--mut);font-size:.9em;flex:0 0 auto}.glyph-tip .age.n{color:var(--gnewt)}
.glyph-tip .sub{color:var(--mut);font-size:.9em;margin-top:-.1em;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.glyph-tip .sh{display:flex;justify-content:space-between;align-items:baseline;gap:1em;margin-top:.6em;padding-top:.45em;border-top:1px solid var(--rule);font-size:.78em;letter-spacing:.08em;text-transform:uppercase;color:var(--mut);white-space:nowrap}
.glyph-tip .sh.first{margin-top:0;padding-top:0;border-top:0}.glyph-tip .sh>span{color:var(--fg);overflow:hidden;text-overflow:ellipsis}.glyph-tip .sh i{font-style:normal;color:var(--mut);letter-spacing:0;text-transform:none;font-size:1.08em;margin-left:.5em}
.glyph-tip .sh em{font-style:normal;letter-spacing:0;text-transform:none;font-size:1.12em;color:var(--mut);flex:0 0 auto}.glyph-tip .sh em.o{color:var(--gobs)}
.glyph-tip .pb{display:flex;height:.55em;margin:.4em 0 .3em;gap:1px;background:color-mix(in srgb,var(--fg) 8%,transparent);border-radius:2px;overflow:hidden}.glyph-tip .pb svg{display:block;height:100%;flex:0 0 auto}.glyph-tip .pb .arr,.glyph-peek .pb .arr{width:1.1em;background:transparent;fill:none;stroke:var(--mut);stroke-width:1.4;stroke-linecap:round;stroke-linejoin:round;padding:0 .1em}.glyph-tip .lg .arw .arr{width:.8em;height:.8em;vertical-align:-.1em;fill:none;stroke:var(--mut);stroke-width:1.4;stroke-linecap:round;stroke-linejoin:round}.glyph-tip .pb.con{height:.3em;margin-top:.5em}
.glyph-tip .lg{display:flex;flex-wrap:wrap;gap:.1em .9em;font-size:.9em;line-height:1.45;white-space:normal}.glyph-tip .it{white-space:nowrap}.glyph-tip .it.lead{color:var(--mut)}
.glyph-tip .sw{width:8px;height:8px;margin-right:.35em;vertical-align:0;outline:1px solid var(--rule)}
.glyph-tip .nt.kv{margin-top:.25em;color:var(--mut)}.glyph-tip .nt{margin-top:.3em;font-size:.92em;line-height:1.45;white-space:normal}.glyph-tip .nt b{color:var(--fg)}.glyph-tip .sep::before{content:" · ";color:var(--mut)}
.glyph-tip .dots{display:inline-block;letter-spacing:.08em;margin-left:.1em}.glyph-tip .dots i{font-style:normal;opacity:.25;animation:gdot 1.4s ease-in-out infinite}.glyph-tip .dots i:nth-child(2){animation-delay:.2s}.glyph-tip .dots i:nth-child(3){animation-delay:.4s}
@keyframes gdot{0%,80%,100%{opacity:.25}40%{opacity:1}}
/* collapsed by default: header + one ellipsized line per source, so every card has the same height and the page never shifts when the srtm answer lands; chevrons-down expands, chevrons-up collapses */
.glyph-tip .rw{display:flex;align-items:baseline;gap:.7em;margin-top:.3em;white-space:nowrap;min-width:0}.glyph-tip .rw .lb{flex:0 0 5.2em;font-size:.78em;letter-spacing:.08em;text-transform:uppercase;color:var(--mut)}.glyph-tip .rw .vl{flex:1 1 auto;min-width:0;overflow:hidden;text-overflow:ellipsis}
.glyph-tip .rw.off{opacity:.75}.glyph-tip .rw.off .lb::before{content:'';display:inline-block;width:.45em;height:.45em;border-radius:50%;border:1px solid var(--mut);margin-right:.45em;vertical-align:baseline}.glyph-tip .rw .vl b{font-weight:600}.glyph-tip .rw .vl.wr{white-space:normal;text-overflow:clip;line-height:1.45}.glyph-tip .nw,.glyph-peek .nw{white-space:nowrap}
.glyph-tip .src+.src .rw{margin-top:.15em}.glyph-tip .hd+.src .rw,.glyph-tip .sub+.src .rw{margin-top:.5em;padding-top:.45em;border-top:1px solid var(--rule)}
.glyph-tip .src.up{animation:gfade .3s cubic-bezier(.2,.7,.3,1) both}
/* legend: the first few groups read at once, the rest unfold behind … so every bar segment has its name */
.glyph-tip .lg .lg-x[hidden]{display:none}.glyph-tip .lg .lg-x{display:contents}.glyph-tip .lg-m{appearance:none;border:0;background:none;color:var(--mut);font:inherit;line-height:1;padding:0 .3em;border-radius:3px;cursor:pointer;pointer-events:auto;letter-spacing:.1em;opacity:.8}.glyph-tip .lg-m:hover,.glyph-tip .lg-m:focus-visible{color:var(--fg);outline:none;opacity:1;background:var(--code)}
.glyph-tip .g-obs,.glyph-peek .g-obs,.glyph-time .g-obs{fill:var(--gobs)}.glyph-tip .g-tr0,.glyph-peek .g-tr0{fill:#f4fcea}.glyph-tip .g-tr1,.glyph-peek .g-tr1{fill:#dcf2c4}.glyph-tip .g-tr2,.glyph-peek .g-tr2{fill:#bfe7a3}.glyph-tip .g-tr3,.glyph-peek .g-tr3{fill:#9fda86}.glyph-tip .g-tr4,.glyph-peek .g-tr4{fill:#7ccb6e}

.glyph-view .gv-t{position:relative;appearance:none;-webkit-appearance:none;background:none;border:0;padding:.15em .2em;margin:0;color:var(--mut);cursor:pointer;display:inline-flex;align-items:center;justify-content:center;line-height:1;font:inherit;border-radius:3px;transition:color .15s,background .15s,opacity .55s ease;-webkit-tap-highlight-color:transparent}.glyph-view .glyph-view .gv-t.off{opacity:.3;cursor:default}.glyph-view .gv-t.off:hover{background:none}/* change + diff carry a word after the icon (same .gt-t rule as find: shown on phones / coarse pointers, icon only on a desktop bar); the word is sized to the icon — 1em text on a 1.05em glyph, gap and padding in the same em so the labelled button reads as one pill */.glyph-view .gv-t{gap:.3em}.glyph-view .gv-t .gt-t{font-size:1em;line-height:1.05;padding-right:.15em}.glyph-view .gv-t svg{width:1.05em;height:1.05em;flex:0 0 auto;stroke:currentColor;fill:none;stroke-width:1.5;stroke-linecap:round;stroke-linejoin:round;overflow:visible;transition:stroke-width .15s}.glyph-view .gv-t:hover,.glyph-view .gv-t:focus-visible{color:var(--fg);background:var(--code);outline:none}.glyph-view .glyph-view .gv-t span{color:inherit;transition:color .15s}.glyph-view .gv-chg[aria-pressed=true]{color:var(--gchg)}.glyph-view .gv-dif[aria-pressed=true]{color:var(--gdif)}.glyph-tip .g-dif,.glyph-peek .g-dif,.glyph-time .g-dif{fill:var(--gdif)}.glyph-tip .g-dif1{fill:var(--gdif);opacity:.78}.glyph-tip .sh em.d{color:var(--gdif)}.glyph-tip .lg12 .nt a.raw{color:inherit;text-decoration:underline dotted;cursor:pointer}.glyph-view .gv-sub[aria-pressed=true]{color:var(--gsub)}.glyph-view .gv-tree[aria-pressed=true]{color:var(--gobs)}.glyph-view .gv-relief[aria-pressed=true]{color:var(--fg)}.glyph-view .gv-ortho[aria-pressed=true],.glyph-view .gv-rd[aria-pressed=true]{color:var(--fg)}.glyph-view .gv-t[aria-pressed=true]::after{content:"";position:absolute;left:.2em;right:.2em;bottom:-.05em;border-bottom:1px solid currentColor;opacity:.6;border-radius:1px}.glyph-tip .g-chg,.glyph-peek .g-chg,.glyph-time .g-chg{fill:var(--gchg)}.glyph-tip .g-cand,.glyph-peek .g-cand,.glyph-time .g-cand{fill:var(--gchg);opacity:.45}.glyph-tip .ca{opacity:.45}.glyph-tip .g-none,.glyph-peek .g-none,.glyph-time .g-none{fill:var(--rule)}.glyph-tip .g-neg,.glyph-peek .g-neg,.glyph-time .g-neg{fill:var(--mut)}
.glyph-tip .dt,.glyph-tip .dti{display:none}.glyph-tip.open .dti{display:inline}.glyph-tip.open .sub.dti{display:block}.glyph-tip.open .dt{display:block}.glyph-tip.open .rw{display:none}
/* collapsed in-view legend: the land-use bar + its first groups and the headline figures only — band bars, notes and the source line wait behind the chevron */
.glyph-tip.x:not(.open) .lg12 .dx,.glyph-tip.x:not(.open) .lg12 .sh i{display:none}
.glyph-tip.swap.open .dt,.glyph-tip.swap:not(.open) .rw{animation:gfade .3s cubic-bezier(.2,.7,.3,1) both}@keyframes gfade{from{opacity:0;transform:translateY(-3px)}to{opacity:1;transform:none}}
.glyph-tip .glyph-more{display:none;width:100%;margin:.35em 0 -.25em;padding:.2em 0 .1em;border:0;border-radius:3px;background:transparent;color:var(--mut);font:inherit;line-height:1;text-align:center;cursor:pointer;pointer-events:auto;opacity:.9;transition:opacity .2s,color .15s,background .15s;-webkit-tap-highlight-color:transparent}
.glyph-tip.x .glyph-more{display:block}.glyph-tip .glyph-more svg{width:1.1em;height:1.1em;fill:none;stroke:currentColor;stroke-width:1.5;stroke-linecap:round;stroke-linejoin:round;vertical-align:middle;transition:transform .36s cubic-bezier(.2,.7,.3,1)}.glyph-tip.open .glyph-more svg{transform:rotate(180deg)}.glyph-tip .glyph-more:hover,.glyph-tip .glyph-more:focus-visible{opacity:1;color:var(--fg);background:var(--code);outline:none}
.glyph-tip.open .glyph-more{margin-top:.6em}
.glyph-tip .lg12{margin-top:.6em;padding-top:.45em;border-top:1px solid var(--rule)}.glyph-tip .lg12 .sh{margin-top:0;padding-top:0;border-top:0}.glyph-tip .lg12 .pb{margin-top:.35em}
.glyph-tip .p0{fill:var(--gink);opacity:.3}.glyph-tip .p1{fill:var(--gink);opacity:.6}.glyph-tip .p2{fill:var(--gink);opacity:.85}.glyph-tip .p3{fill:var(--gink)}.glyph-tip .p4{fill:var(--fg);opacity:.8}
figure.glyph .hatch{stroke:var(--bg);stroke-width:1;opacity:.6}figure.glyph .dot{fill:var(--bg);opacity:.7}
.glyph-tip .g-geb,.glyph-peek .g-geb,.glyph-time .g-geb{fill:#3d3d3d}.glyph-tip .g-bau,.glyph-peek .g-bau,.glyph-time .g-bau{fill:#c4a484}.glyph-tip .g-acker,.glyph-peek .g-acker,.glyph-time .g-acker{fill:#e8c84a}.glyph-tip .g-gruen,.glyph-peek .g-gruen,.glyph-time .g-gruen{fill:#9fd37a}.glyph-tip .g-wald,.glyph-peek .g-wald,.glyph-time .g-wald{fill:#276b38}.glyph-tip .g-wasser,.glyph-peek .g-wasser,.glyph-time .g-wasser{fill:#4f93d6}.glyph-tip .g-verkehr,.glyph-peek .g-verkehr,.glyph-time .g-verkehr{fill:#8c8c8c}.glyph-tip .g-alpen,.glyph-peek .g-alpen,.glyph-time .g-alpen{fill:#e8dccf}.glyph-tip .g-sonst,.glyph-peek .g-sonst,.glyph-time .g-sonst{fill:#c0c4c8}
.glyph-tip .g-unknown,.glyph-peek .g-unknown,.glyph-time .g-unknown{fill:#d6d2ca}.glyph-tip .g-consistent,.glyph-peek .g-consistent,.glyph-time .g-consistent{fill:var(--rule)}.glyph-tip .g-forest_loss,.glyph-peek .g-forest_loss{fill:#b0651a}.glyph-tip .g-forest_gain,.glyph-peek .g-forest_gain{fill:#276b38}.glyph-tip .g-sealed_new,.glyph-peek .g-sealed_new{fill:#3d3d3d}.glyph-tip .g-structure_new,.glyph-peek .g-structure_new{fill:#1f1f1f}.glyph-tip .g-green_new,.glyph-peek .g-green_new{fill:#9fd37a}
.glyph-tip .g-tree,.glyph-peek .g-tree,.glyph-time .g-tree{fill:#276b38}.glyph-tip .g-grass,.glyph-peek .g-grass,.glyph-time .g-grass{fill:#9fd37a}.glyph-tip .g-crop,.glyph-peek .g-crop,.glyph-time .g-crop{fill:#e8c84a}.glyph-tip .g-roof,.glyph-peek .g-roof,.glyph-time .g-roof{fill:#3d3d3d}.glyph-tip .g-road,.glyph-tip .g-parking,.glyph-peek .g-parking,.glyph-time .g-parking{fill:#8c8c8c}.glyph-tip .g-water,.glyph-peek .g-water,.glyph-time .g-water{fill:#4f93d6}.glyph-tip .g-garden,.glyph-tip .g-orchard,.glyph-tip .g-vineyard,.glyph-tip .g-shrub,.glyph-tip .g-hedge,.glyph-peek .g-hedge,.glyph-time .g-hedge{fill:#6fa84f}.glyph-tip .g-rock,.glyph-tip .g-bare_soil,.glyph-tip .g-glacier,.glyph-peek .g-glacier,.glyph-time .g-glacier{fill:#e8dccf}
@media(prefers-color-scheme:dark){.glyph-tip .g-geb,.glyph-tip .g-roof,.glyph-tip .g-sealed_new,.glyph-peek .g-sealed_new{fill:#bdbdbd}.glyph-tip .g-structure_new,.glyph-peek .g-structure_new{fill:#e8e8e8}}
@media(max-width:640px),(hover:none) and (pointer:coarse){.glyph-tip>div{width:auto!important}.glyph-tip .glyph-grip,.glyph-tip .glyph-dk{display:none!important}.glyph-tip.x .hd{cursor:auto;padding-right:1.6em}.glyph-tip,.glyph-tip.tour{--float:0;position:relative;transform:none!important;transition:opacity .25s;max-width:100%;white-space:normal;pointer-events:auto;margin-top:.45rem;min-height:7.6em;box-sizing:border-box}}
@media(prefers-reduced-motion:reduce){.gt-dt,.glyph-view .gv-t,.glyph-tip,.glyph-tip>div,svg.glyph .cur,.glyph-tip .glyph-more svg,svg.glyph .hi .b,.gv-b .ld,.gv-b span,.gt-sep{transition:none}.glyph-tip.swap .dt,.glyph-tip.swap .rw,.glyph-tip .src.up{animation:none}svg.glyph .ping.go{animation:none}.glyph-tip .dots i,.gv-b .ld u{animation:none;opacity:.6}.glyph-tip .glyph-x{transition:none}.glyph-view .gv-t.busy{animation:none;opacity:.6}}
.gt-dt{flex:0 1 auto;margin-right:auto;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-style:italic;color:var(--gchg);font-variant-numeric:tabular-nums;transition:opacity .3s}.gt-dt[hidden]{display:none}.glyph-side:has(.glyph-tools.open:not(.set)) .gt-dt{visibility:hidden}
.glyph-side{flex:1 1 100%;display:flex;flex-wrap:wrap;align-items:center;justify-content:flex-end;gap:.3em;min-width:0;font-size:.78em;color:var(--mut);position:relative}
.glyph-tools{flex:0 0 auto;position:relative;display:flex;align-items:center;gap:.3em;line-height:1.5;white-space:nowrap;-webkit-user-select:text;user-select:text}
.gt-sep{color:var(--mut)}
.gt-b{appearance:none;-webkit-appearance:none;background:none;border:0;padding:.1em .35em;margin:0;border-radius:3px;color:var(--mut);cursor:pointer;display:inline-flex;align-items:center;gap:.3em;font:inherit;line-height:inherit;transition:color .15s,background .15s;opacity:.85}.gt-b:hover{opacity:1}
.gt-t{display:none}.glyph-tools.open .gt-q{padding-right:.15em}.gt-k .ok{display:none}.gt-k.done .ok{display:inline-block;color:var(--acc,#2a6)}.gt-k.done .ln{display:none}
.gt-b svg{width:1.05em;height:1.05em;fill:none;stroke:currentColor;stroke-width:1.5;stroke-linecap:round;stroke-linejoin:round;flex:0 0 auto}
/* full resolution (res-12 tiles): lucide scan-search = load, minimize-2 = back; the count reads while tiles stream and for a moment after */
/* the full-res button is wired but never shown: full resolution is automatic (hiAuto) — the status slot .gt-n takes its room */.gt-d{display:none!important}.gt-d .i-off{display:none}.gt-d{transition:color .15s,background .15s,opacity .55s ease}.gt-d.off{opacity:.3;cursor:default}.gt-d.off:hover{background:none;opacity:.3}.gt-d[aria-pressed=true]{color:var(--acc)}.gt-d[aria-pressed=true] .i-on{display:none}.gt-d[aria-pressed=true] .i-off{display:inline-block}
.gt-f .i-off{display:none}.gt-f[aria-pressed=true] .i-on{display:none}.gt-f[aria-pressed=true] .i-off{display:inline-block}.gt-f[aria-pressed=true]{color:var(--fg)}
/* full screen: the figure takes the viewport (native Fullscreen API where it exists, a fixed box on iOS); the map fits the room left by the tools and the card */
figure.glyph.fs{position:fixed;top:0;left:0;right:0;bottom:0;width:auto;height:auto;z-index:60;margin:0;padding:.5rem max(.5rem,env(safe-area-inset-right)) max(.5rem,env(safe-area-inset-bottom)) max(.5rem,env(safe-area-inset-left));background:var(--bg);display:flex;flex-direction:column;box-sizing:border-box;overflow:hidden}
figure.glyph.fs .glyph-row{flex:1 1 auto;min-height:0;flex-wrap:nowrap;flex-direction:column;align-items:stretch;gap:6px}
figure.glyph.fs .glyph-main{flex:1 1 0;min-height:0;display:flex;align-items:center;justify-content:center}
figure.glyph.fs svg.glyph{flex:0 0 auto}
figure.glyph.fs .glyph-side{flex:0 0 auto}
/* full screen: the docked card is a sheet over the map's lower edge, aligned to the map's box (--fs-x/--fs-w/--fs-b from fsFit) — it never takes layout space, so the map never moves when it opens, grows or closes; the tool bar aligns to the same box */
figure.glyph.fs .glyph-tip.dock{position:absolute;top:auto;left:var(--fs-x,.5rem);width:var(--fs-w,auto);right:auto;bottom:var(--fs-b,3rem);z-index:4;flex:none;margin:0;min-height:0;max-height:38%;overflow-y:auto;overscroll-behavior:contain;box-sizing:border-box;background:color-mix(in srgb,var(--bg) 90%,transparent);-webkit-backdrop-filter:blur(6px);backdrop-filter:blur(6px);border-radius:6px 6px 4px 4px;box-shadow:0 -1px 12px rgba(0,0,0,.08)}
figure.glyph.fs .glyph-side{margin-left:var(--fs-x,0);width:var(--fs-w,auto);box-sizing:border-box}
html.glyph-fs,html.glyph-fs body{overflow:hidden}
/* full screen exit: a corner button that is never clipped (the bar may be cut on a narrow phone), sized for a thumb, inside the figure so native full screen shows it; Android back and Esc do the same */
.glyph-fsx{display:none;position:absolute;top:max(.5rem,env(safe-area-inset-top));right:max(.5rem,env(safe-area-inset-right));z-index:7;width:2.5rem;height:2.5rem;padding:0;margin:0;border:1px solid var(--rule);border-radius:50%;background:color-mix(in srgb,var(--bg) 85%,transparent);-webkit-backdrop-filter:blur(6px);backdrop-filter:blur(6px);color:var(--fg);cursor:pointer;align-items:center;justify-content:center;box-shadow:0 1px 8px rgba(0,0,0,.1);opacity:.9;transition:opacity .15s,transform .15s;-webkit-tap-highlight-color:transparent}
.glyph-fsx svg{width:1.05rem;height:1.05rem;fill:none;stroke:currentColor;stroke-width:1.7;stroke-linecap:round}.glyph-fsx:hover,.glyph-fsx:focus-visible{opacity:1;transform:scale(1.06)}.glyph-fsx:active{transform:scale(.94)}
figure.glyph.fs .glyph-fsx{display:flex}
/* the tool bar never runs off the figure: when its labels do not fit (narrow phone, full screen) the buttons after find drop to icons — measured, not guessed (toolsFit) */
.glyph-tools.tight .gt-b:not(.gt-q):not(.gt-x) .gt-t{display:none}.glyph-tools.tight .gt-b{padding:.1em .45em}
@media(max-width:640px),(hover:none) and (pointer:coarse){figure.glyph.fs .glyph-tip{--sheet:b;position:absolute;top:auto;left:var(--fs-x,.5rem);width:var(--fs-w,auto);right:auto;bottom:var(--fs-b,3rem);z-index:4;flex:none;margin:0;min-height:0;max-height:38%;overflow-y:auto;overscroll-behavior:contain;box-sizing:border-box;background:color-mix(in srgb,var(--bg) 90%,transparent);-webkit-backdrop-filter:blur(6px);backdrop-filter:blur(6px);border-radius:6px 6px 4px 4px;box-shadow:0 -1px 12px rgba(0,0,0,.08)}}
/* full screen on a phone turned sideways: the card is a side sheet over the map's left edge so the map keeps its height */
@media(hover:none) and (pointer:coarse) and (orientation:landscape) and (max-height:520px){figure.glyph.fs .glyph-tip{--sheet:l;top:var(--fs-t,.5rem);bottom:auto;left:var(--fs-x,.5rem);width:min(46%,26rem);max-height:calc(100% - var(--fs-t,.5rem) - var(--fs-b,3rem));border-radius:4px 6px 6px 4px}figure.glyph.fs .glyph-tip.dock{top:var(--fs-t,.5rem)}}
.gt-d.busy .i-on{animation:gspin 1.1s linear infinite}@keyframes gspin{to{transform:rotate(360deg)}}
/* full-res status: its own slot at the right end of the bar (tabular, right-aligned, fades) so nothing to its left ever shifts; the dots are a fixed 3ch box */
.gt-n{margin-left:auto;color:var(--mut);font-variant-numeric:tabular-nums;white-space:nowrap;text-align:right;opacity:0;transition:opacity .3s;pointer-events:none;padding-left:.6em}.gt-n.n{opacity:1}
.gt-n .dots{display:inline-block;width:3ch;text-align:left;letter-spacing:0;margin:0}.gt-n .dots i{font-style:normal;opacity:.25;animation:gdot 1.4s ease-in-out infinite}.gt-n .dots i:nth-child(2){animation-delay:.2s}.gt-n .dots i:nth-child(3){animation-delay:.4s}
svg.glyph .hi image{opacity:0;transition:opacity .45s ease}svg.glyph .hi image.in{opacity:1}svg.glyph .hi{transition:opacity .4s}svg.glyph{isolation:isolate}svg.glyph .hi .ov-rel{display:none;pointer-events:none;mix-blend-mode:overlay;opacity:.3;transition:opacity .3s}svg.glyph.v-rel .hi .ov-rel{display:inline}svg.glyph .hi .ov-rel image{shape-rendering:crispEdges}svg.glyph .hi.out{opacity:0}
/* ortho: basemap.at aerial image under the base, near tiles only (LOD 0/1 = hi-near); the group carries the reader's opacity inline, a touch of desaturation keeps the data colours in front */svg.glyph .hi .ov-ortho{display:none;pointer-events:none;filter:saturate(.8);transition:opacity .3s}svg.glyph.v-ortho.hi-near .hi .ov-ortho{display:inline}svg.glyph .hi .ov-ortho image{shape-rendering:crispEdges}/* the slider past the half: the profile fills step back (filter:opacity composes with their own opacity rules) so the photo comes through — --otf from setOrthoOp */svg.glyph.v-ortho.hi-near .hi .b,svg.glyph.v-ortho.hi-near .hi .ov-obs{filter:opacity(var(--otf,1))}
/* the opacity popover: floats over the bottom edge of the map, anchored to the earth button (left set by script), never in the flow of the bar */.gv-op{position:absolute;z-index:30;bottom:calc(100% + 4px);left:0;display:flex;align-items:center;gap:.5em;padding:.1em .5em;font-size:.86em;line-height:1;color:var(--mut);background:var(--bg);border:1px solid var(--rule);border-radius:3px;opacity:0;transform:translateY(2px);transition:opacity .16s ease,transform .16s ease;pointer-events:none;-webkit-user-select:none;user-select:none}.gv-op.on{opacity:.94;transform:none;pointer-events:auto}.gv-op[hidden]{display:none}.gv-op .v{font-variant-numeric:tabular-nums;min-width:3.4ch;text-align:right}/* the range is a hairline like the time strip: a 1 px rule, the ink filling it up to the head (--p), the head a short vertical bar (the time strip's hairline head); the input stays tall enough for a finger */.gv-op input[type=range]{-webkit-appearance:none;appearance:none;width:8em;height:1.5em;margin:0;padding:0;background:transparent;cursor:ew-resize;touch-action:none;--p:50%}.gv-op input[type=range]::-webkit-slider-runnable-track{height:1px;border:0;background:linear-gradient(90deg,var(--fg) var(--p),var(--rule) var(--p))}.gv-op input[type=range]::-moz-range-track{height:1px;border:0;background:var(--rule)}.gv-op input[type=range]::-moz-range-progress{height:1px;background:var(--fg)}.gv-op input[type=range]::-webkit-slider-thumb{-webkit-appearance:none;appearance:none;width:2px;height:11px;margin-top:-5px;border-radius:1px;background:var(--fg);border:0;transition:height .12s,margin-top .12s}.gv-op input[type=range]::-moz-range-thumb{width:2px;height:11px;border-radius:1px;background:var(--fg);border:0}.gv-op input[type=range]:hover::-webkit-slider-thumb,.gv-op input[type=range]:active::-webkit-slider-thumb{height:15px;margin-top:-7px}.gv-op input[type=range]:focus{outline:none}.gv-op input[type=range]:focus-visible::-webkit-slider-thumb{box-shadow:0 0 0 3px var(--code)}@media(pointer:coarse){.gv-op{font-size:.95em;padding:.3em .6em}.gv-op input[type=range]{width:10em;height:2.2em}.gv-op input[type=range]::-webkit-slider-thumb{width:3px;height:15px;margin-top:-7px}.gv-op input[type=range]::-moz-range-thumb{width:3px;height:15px}}.gv-op::after{content:"";position:absolute;left:var(--ax,50%);bottom:-4px;width:6px;height:6px;margin-left:-3px;background:var(--bg);border-right:1px solid var(--rule);border-bottom:1px solid var(--rule);transform:rotate(45deg)}.glyph-view .gv-ortho,.glyph-view .gv-rd{touch-action:manipulation;-webkit-touch-callout:none}@media(prefers-reduced-motion:reduce){.gv-op,.gv-op input[type=range]::-webkit-slider-thumb{transition:none}}}
svg.glyph .hi .ph{fill:var(--acc);opacity:.07;animation:gph 1.3s ease-in-out infinite;transition:opacity .3s}svg.glyph .hi .ph.gone{opacity:0;animation:none}@keyframes gph{50%{opacity:.16}}
svg.glyph .hx{transition:opacity .5s}svg.glyph.hi .hx{opacity:.14!important}svg.glyph.hi .hx .o,svg.glyph.hi .hx .c{display:none!important}svg.glyph.hi.v-obs .hx,svg.glyph.hi.v-chg .hx{opacity:0!important}svg.glyph.hi .sel,svg.glyph.veiled .sel{fill-opacity:0}svg.glyph.hi .selo.on,svg.glyph.veiled .selo.on{opacity:0}svg.glyph.hi.on .cur:not(.c12){opacity:.25}svg.glyph .veil{fill:#fff;opacity:0;pointer-events:none;transition:opacity .4s}svg.glyph .veil.on{opacity:.55}svg.glyph.hi .hx path{stroke:none}
svg.glyph .cur.cap{stroke:var(--fg);stroke-width:1.4px;fill:none;filter:drop-shadow(0 0 1.5px #fff)}svg.glyph .cur.c12{stroke:#fff;stroke-width:2px;fill:#fff;fill-opacity:.3;filter:drop-shadow(0 0 1.5px rgba(0,0,0,.9))}
@media(hover:none){svg.glyph .hi image,svg.glyph .hi .b{transition:none}svg.glyph .hi .ph{animation:none;opacity:.1}}
@media(prefers-reduced-motion:reduce){.glyph-peek,svg.glyph .hov{transition:none}.gt-d.busy .i-on{animation:none}svg.glyph .hi image,svg.glyph .hi,svg.glyph .hx,.gt-n{transition:none}.gt-n .dots i{animation:none;opacity:.6}svg.glyph .hi .ph{animation:none;opacity:.12}}
.gt-b:hover,.gt-b:focus-visible{color:var(--fg);background:var(--code);outline:none}.glyph-tools.hist .gt-h,.glyph-tools.open .gt-q{color:var(--fg)}
/* icon colour follows its row: an overlay that is on and showing takes the colour of the view it rides on (declared = ink, observed = teal, change = orange); on and not showing (the other row's subsidy, trees outside observed) it keeps the row grey at half strength and marks "on" by the heavier stroke alone; off = cannot show now */
.glyph-view .gv-t.off[aria-pressed],.glyph-view .gv-t.off[aria-pressed]:hover{color:var(--mut);opacity:.3}.glyph-view .gv-t.off:focus-visible{opacity:.6}
.glyph-view .gv-t:hover:not(.off),.glyph-view .gv-t:focus-visible{opacity:1}





.glyph-view .gv-t svg,.gt-b svg{transition:stroke-width .15s,transform .18s cubic-bezier(.2,.7,.3,1)}.glyph-view .gv-t:hover:not(.off) svg,.gt-b:hover:not(.off) svg{transform:translateY(-.5px)}
.glyph-tools .gt-b:not(.off):hover svg,.glyph-tools .gt-b:focus-visible svg{stroke-width:1.75}
/* tooltip: one shared bubble under the hovered control, the button's title moved into it while it shows (no native tooltip twins); 420 ms warm-up, instant while hopping along the bar, keyboard focus shows it too, a trailing "( x )" in the title becomes a key cap */
.glyph-tt{position:absolute;z-index:40;left:0;top:0;font-size:.72em;line-height:1.35;letter-spacing:0;color:var(--bg);background:var(--fg);padding:.34em .62em;border-radius:4px;max-width:24em;white-space:normal;text-align:left;pointer-events:none;opacity:0;transform:translateY(-2px);transition:opacity .14s ease,transform .14s ease;box-shadow:0 2px 10px rgba(0,0,0,.14);will-change:opacity,transform}
.glyph-tt.on{opacity:.96;transform:none}.glyph-tt.up{transform:translateY(2px)}.glyph-tt.up.on{transform:none}
.glyph-tt kbd{font:inherit;font-size:.86em;opacity:.72;margin-left:.5em;border:1px solid currentColor;border-radius:3px;padding:0 .32em;line-height:1.15;white-space:nowrap;vertical-align:.04em}
@media(prefers-reduced-motion:reduce){.glyph-tt,.glyph-view .gv-t svg,.gt-b svg{transition:none;transform:none!important}}
@media(hover:none) and (pointer:coarse){.glyph-tt{display:none!important}}
.gt-s{display:inline-flex;align-items:center;border:1px solid transparent;border-radius:5px;transition:border-color .2s,box-shadow .2s,background .2s;position:relative}
.gt-g{position:absolute;left:0;top:0;bottom:0;display:none;align-items:center;white-space:pre;pointer-events:none;overflow:hidden;color:var(--mut);font:inherit}.gt-g i{font-style:normal;visibility:hidden}.gt-g em{font-style:normal;opacity:.75}.gt-g em.hint{opacity:.6}.glyph-tools.open .gt-g{display:flex}
.gt-s .gt-x{display:none;padding:.1em .25em}
.glyph-tools.open .gt-s{position:absolute;right:0;bottom:-.3em;z-index:5;background:var(--bg);border-color:var(--rule);box-shadow:0 2px 12px rgba(0,0,0,.08)}
.glyph-tools.open .gt-q{background:none;cursor:default;pointer-events:none;opacity:1}.glyph-tools.open .gt-q .gt-t{display:none!important}.glyph-tools.open .gt-s .gt-x{display:inline-flex}
.gt-in{appearance:none;-webkit-appearance:none;border:0;background:none;outline:none;color:var(--fg);font:inherit;width:0;padding:0;margin:0;opacity:0;transition:width .28s cubic-bezier(.2,.7,.3,1),opacity .2s;min-width:0}
.gt-in::-webkit-search-cancel-button,.gt-in::-webkit-search-decoration{-webkit-appearance:none}.gt-in::placeholder{color:var(--mut);opacity:.8}
.glyph-tools.open .gt-in{width:17em;opacity:1}/* settled: the box sits inline and shows only the selection chips + ×; the input is folded away until the box is clicked (or / ) */.glyph-tools.set .gt-s{position:relative;right:auto;bottom:auto;border-radius:999px;padding:.02em .1em .02em .25em;border-color:color-mix(in srgb,var(--fg) 16%,transparent);background:color-mix(in srgb,var(--bg) 55%,transparent);-webkit-backdrop-filter:blur(8px);backdrop-filter:blur(8px);box-shadow:inset 0 0 0 1px rgba(255,255,255,.3);cursor:text;transition:border-color .15s,background .15s}.glyph-tools.set .gt-s:hover{border-color:color-mix(in srgb,var(--fg) 32%,transparent);background:color-mix(in srgb,var(--bg) 75%,transparent)}.glyph-tools.set .gt-in{width:0;opacity:0;padding:0}.glyph-tools.set .gt-g{display:none}.gt-ch:not([hidden])~.gt-in{margin-left:.45em}/* the box opens to the left of the find button by default; when that would leave the figure (full screen puts the tools at the left edge) it opens to the right instead (.lft, boxSide() in openSearch) */.glyph-tools.open.lft .gt-s{right:auto;left:0}.glyph-tools.lft .gt-l{right:auto;left:0}.glyph-tools.up .gt-l{top:auto;bottom:calc(100% + .45em)}/* selection tags: what is selected (KG · res-7 hexagon · res-12 cell · tree) reads in the box as a small hex glyph + plain name, muted code after it; several are separated by a dot; the box × clears them all, Backspace in the empty box drops the last */.gt-ch{display:inline-flex;align-items:center;gap:.45em;max-width:24em;min-width:0;margin-left:.2em;color:var(--fg)}.gt-ch[hidden]{display:none}.gt-cp{display:inline-flex;align-items:center;gap:.35em;min-width:0;max-width:14em;cursor:text;line-height:1.5}.gt-cp+.gt-cp:before{content:"\00b7";color:var(--mut);margin-right:.1em}.gt-cp svg.i{flex:none;width:.85em;height:.85em;fill:none;stroke:var(--fg);stroke-width:1.5;stroke-linejoin:round;stroke-linecap:round;opacity:.75}.gt-cp-h3 svg.i{fill:var(--fg);fill-opacity:.3}.gt-cp-kg svg.i{stroke:var(--acc);opacity:1}.gt-cp-tree svg.i{stroke:var(--gnew,#4a4);opacity:1}.gt-cp .n{overflow:hidden;text-overflow:ellipsis;white-space:nowrap;min-width:0}.gt-cp .s{color:var(--mut);white-space:nowrap;font-variant-numeric:tabular-nums}.gt-cp.out{opacity:0;transition:opacity .16s}.glyph-tools.set .gt-s .gt-x{opacity:.7}.glyph-tools.set .gt-s:hover .gt-x{opacity:1}
.gt-l{position:absolute;top:calc(100% + .45em);right:0;z-index:6;margin:0;padding:4px 0;list-style:none;min-width:min(22em,calc(100vw - 2rem));max-width:min(26em,calc(100vw - 2rem));max-height:min(19em,50vh);overflow:auto;background:var(--bg);border:1px solid var(--rule);border-radius:6px;box-shadow:0 4px 18px rgba(0,0,0,.1);font-size:1.02em;line-height:1.35;font-variant-numeric:tabular-nums;text-align:left}
.gt-l li{display:grid;grid-template-columns:1fr auto;grid-template-areas:"n s" "g s";column-gap:.7em;align-items:center;padding:.3em .75em;cursor:pointer;white-space:nowrap}
.gt-l li .n{grid-area:n;overflow:hidden;text-overflow:ellipsis;color:var(--fg)}.gt-l li .n b{font-weight:700}
.gt-l li .g{grid-area:g;overflow:hidden;text-overflow:ellipsis;color:var(--mut);font-size:.88em}.gt-l li .s{grid-area:s;color:var(--mut);font-size:.85em;text-align:right}
.gt-l li .s i{display:inline-block;width:7px;height:7px;border-radius:50%;margin-right:.45em;vertical-align:0;background:var(--fg);opacity:.75;border:1px solid transparent}.gt-l li .s i.n{background:var(--gnew);opacity:1}.gt-l li .s i.ob{background:var(--gobs);opacity:1}.gt-l li .s i.e{background:none;border-color:var(--rule)}
.gt-l li[aria-selected=true],.gt-l li:hover{background:var(--code)}.gt-l li.mut{color:var(--mut);cursor:default;display:block;white-space:normal}.gt-l li.hd{display:flex;justify-content:space-between;gap:1em;color:var(--mut);font-size:.78em;letter-spacing:.08em;text-transform:uppercase;cursor:default;padding-top:.35em}
.gt-l li.hd button{appearance:none;border:0;background:none;color:var(--mut);font:inherit;letter-spacing:0;text-transform:none;cursor:pointer;padding:0}.gt-l li.hd button:hover{color:var(--fg)}
.gt-l li .k{display:inline-block;color:var(--mut);font-size:.8em;border:1px solid var(--rule);border-radius:3px;padding:0 .35em;margin-right:.5em;vertical-align:.05em;line-height:1.4}
/* the merged list: a hairline + a small word opens the address and name sections (KGs and admin units need none — the kind tag says it); address rows carry a pin (street: a road) in the muted ink, "in view" sits where a KG shows its code; while the remote answers are on their way a thin accent line breathes at the foot of the list (after a 220 ms grace, so a fast answer never flickers) */
.gt-l li.sp{display:flex;align-items:center;gap:.6em;padding:.25em .75em 0;margin-top:.2em;cursor:default;color:var(--mut);font-size:.72em;letter-spacing:.08em;text-transform:uppercase;pointer-events:none}.gt-l li.sp:before{content:"";flex:1;border-top:1px solid var(--rule)}.gt-l li.sp span{order:-1}.gt-l li.sp:hover{background:none}
.gt-l li .n .ic{display:inline-block;width:.85em;height:.85em;margin-right:.45em;vertical-align:-.08em;fill:none;stroke:var(--mut);stroke-width:1.5;stroke-linecap:round;stroke-linejoin:round}.gt-l li.r-adr .s .k,.gt-l li.r-adm .s .k{margin-right:0}.gt-l li .s em.iv{font-style:normal;color:var(--acc);font-size:.92em}
.gt-l li.mut.wait{color:var(--mut);opacity:.7}
.gt-l.busy:after{content:"";position:sticky;display:block;bottom:0;height:2px;margin:2px .75em -2px;border-radius:2px;background:linear-gradient(90deg,transparent,var(--acc),transparent);background-size:40% 100%;background-repeat:no-repeat;opacity:0;animation:gt-busy 1.1s linear .22s infinite}@keyframes gt-busy{0%{opacity:.7;background-position:-40% 0}100%{opacity:.7;background-position:140% 0}}
/* the found point: a dot in the accent ink with a soft halo, sized in screen pixels (ptSync), the one-shot ping of the tour on arrival */
svg.glyph g.pt{opacity:0;transition:opacity .25s;pointer-events:none}svg.glyph g.pt.on{opacity:1}svg.glyph g.pt .ptd{fill:var(--acc);stroke:#fff;stroke-width:1.6px;vector-effect:non-scaling-stroke;filter:drop-shadow(0 1px 1.5px rgba(0,0,0,.35))}svg.glyph g.pt .pth{fill:var(--acc);opacity:.16}
.gt-cp-pt svg.i{stroke:var(--acc);opacity:1}.gt-cp-adm svg.i{stroke:var(--acc);opacity:1}
svg.glyph .sel{fill:var(--acc);fill-opacity:.13;stroke:none;opacity:0;transition:opacity .3s}svg.glyph .sel.on{opacity:1}
svg.glyph .selo{fill:none;stroke:var(--acc);stroke-width:3px;stroke-linejoin:round;vector-effect:non-scaling-stroke;opacity:0;transition:opacity .3s}svg.glyph .selo.on{opacity:.9}
@media(prefers-color-scheme:dark){.glyph-tools.open .gt-s,.gt-l{box-shadow:0 4px 18px rgba(0,0,0,.4)}.glyph-tip{box-shadow:inset 0 0 0 1px rgba(255,255,255,.06),0 1px 2px rgba(0,0,0,.3),0 12px 32px -10px rgba(0,0,0,.6)}.glyph-tip.moving{box-shadow:inset 0 0 0 1px rgba(255,255,255,.06),0 18px 44px -10px rgba(0,0,0,.7)}.glyph-tools.set .gt-s{box-shadow:inset 0 0 0 1px rgba(255,255,255,.04)}}
@media(max-width:640px),(hover:none) and (pointer:coarse){.gt-t{display:inline}.glyph-tools{position:static}.glyph-tools.open .gt-s{left:0;right:0;top:-.2em;bottom:-.2em}.glyph-tools.open .gt-s{font-size:16px}.glyph-tools.open .gt-in{width:100%;flex:1 1 auto}.glyph-tools.set .gt-s{left:auto;right:auto;top:auto;bottom:auto;font-size:inherit;min-width:0;flex:0 1 auto}.glyph-tools.set .gt-in{flex:0 0 0;width:0}/* settled on a small screen: the other tools drop their labels to icons so the chip has room; the chip itself shrinks (ellipsis) before anything leaves the figure — the bar is right-aligned, overflow would run off the left edge */.glyph-tools{min-width:0;max-width:100%}.glyph-tools.set .gt-b:not(.gt-q):not(.gt-x) .gt-t{display:none}.gt-ch{max-width:60vw;flex:0 1 auto;overflow:hidden}.glyph-tools.open:not(.set) .gt-ch{flex:0 0 auto}.gt-cp{max-width:10em;min-width:0;flex:0 1 auto}.gt-cp .s{display:none}.gt-l{left:0;right:0;max-width:none;min-width:0;top:calc(100% + .35em);font-size:14px;line-height:1.3;padding:2px 0;border-radius:5px;box-shadow:0 3px 12px rgba(0,0,0,.08);max-height:min(40vh,18em)}.gt-l li{padding:.4em .8em;column-gap:.5em}.gt-l li .n{font-weight:400}.gt-l li .g{font-size:.84em}.gt-l li .n .ic{width:.78em;height:.78em;margin-right:.38em}.gt-l li.sp{padding-top:.18em;margin-top:.1em}.gt-l li .n b{font-weight:600}}
@media(prefers-reduced-motion:reduce){.gt-in,.gt-s,svg.glyph .sel,svg.glyph .selo,svg.glyph .veil{transition:none}}
/* time strip (change over time by pass, docs/ne-change.md §6e): a hairline track with one mark per pass (or per detection period inside a pass) under the view bar while change is on; the map carries the picture, the strip only says where in time we are. Marks grow with the square root of the count; the selected range is the strong segment; an unfinished pass is a dashed segment filled to the share compared; the readout below names the window and shows the usual lost » gained bar. Everything moves in place (CSS transitions), the strip unfolds with the change toggle, a hairline shimmer while the window's tiles load. */
.glyph-time{flex:1 1 100%;width:100%;box-sizing:border-box;display:grid;grid-template-rows:0fr;grid-template-columns:minmax(0,1fr);opacity:0;margin:.15em 0 0;position:relative;text-align:left;transition:grid-template-rows .4s cubic-bezier(.2,.7,.2,1),opacity .35s ease,margin .4s}.glyph-time.on{grid-template-rows:1fr;opacity:1;margin-top:.4em}.glyph-time[hidden]{display:none}.glyph-time>*{min-height:0;min-width:0}
.glyph-time .tl-h{display:flex;align-items:center;gap:.4em;font-size:.82em;line-height:1.4;color:var(--mut);min-height:1.6em}.glyph-time .tl-t{flex:1 1 auto;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.glyph-time .tl-t b{color:var(--fg);font-weight:600}.glyph-time .tl-t .mut{color:var(--mut)}
.glyph-time .tl-c{display:inline-flex;align-items:center;gap:.05em;flex:0 0 auto}.glyph-time .tl-c .gt-b{padding:.1em .3em;font-size:1em;line-height:1.3}.glyph-time .tl-up[hidden]{display:none}.glyph-time .tl-all .gt-t{font-size:.95em}.glyph-time .tl-all.is{opacity:.45;pointer-events:none}.glyph-time .tl-st.end{opacity:.35}
.glyph-time .tl-hd{position:absolute;top:-.1em;bottom:-.1em;left:0;width:0;border-left:1.5px solid var(--gchg);opacity:0;pointer-events:none;transition:left .5s linear,opacity .3s}.glyph-time .tl-hd.on{opacity:1}.glyph-time.dg .tl-hd,.glyph-time.dg .tl-b .tl-m,.glyph-time.dg .tl-b .tl-k,.glyph-time.dg .tl-b{transition:none}.glyph-time .tl-s.hd-dg{cursor:grabbing}.glyph-time .tl-s.hd-dg .tl-b{cursor:grabbing}.glyph-time .tl-s.hd-dg .tl-hd{border-left-width:2.5px}.glyph-time .tl-hd span{position:absolute;bottom:calc(100% + .1em);left:0;transform:translateX(-50%);font-size:.68em;line-height:1.2;white-space:nowrap;color:var(--gchg);background:color-mix(in srgb,var(--bg) 85%,transparent);padding:0 .3em;border-radius:2px;font-variant-numeric:tabular-nums;transition:transform .3s}.glyph-time .tl-hd.r span{transform:translateX(-100%)}.glyph-time .tl-hd span:empty{display:none}.glyph-time .tl-cal[aria-pressed=true]{color:var(--fg);opacity:1}.glyph-time .tl-play svg .pl,.glyph-time .tl-play svg .pa{fill:currentColor;stroke:none}.glyph-time .tl-play .pa{display:none}.glyph-time .tl-play[aria-pressed=true] .pl{display:none}.glyph-time .tl-play[aria-pressed=true] .pa{display:inline}.glyph-time .tl-play[aria-pressed=true]{color:var(--gchg);opacity:1}.glyph-time .tl-play.paused{color:var(--gchg);opacity:.8}.glyph-time .tl-play.paused svg{animation:tl-pz 1.6s ease-in-out infinite}@keyframes tl-pz{50%{opacity:.45}}
.glyph-time .tl-s{display:flex;align-items:center;height:1.3em;margin:1.1em 0 1em;position:relative;touch-action:pan-y;cursor:ew-resize;-webkit-user-select:none;user-select:none;animation:tl-in .35s cubic-bezier(.2,.7,.2,1) both}.glyph-time .tl-s.dn{animation-name:tl-dn}.glyph-time .tl-s.up{animation-name:tl-up}@keyframes tl-in{from{opacity:0;transform:translateY(2px)}to{opacity:1;transform:none}}@keyframes tl-dn{from{opacity:0;transform:scaleX(.7)}to{opacity:1;transform:none}}@keyframes tl-up{from{opacity:0;transform:scaleX(1.2)}to{opacity:1;transform:none}}
.glyph-time .tl-b.in{cursor:grab}.glyph-time .tl-b{flex:1 1 0;min-width:4px;height:100%;position:relative;appearance:none;-webkit-appearance:none;background:none;border:0;padding:0;margin:0;cursor:pointer;-webkit-tap-highlight-color:transparent;outline:none;transition:opacity .35s}.glyph-time .tl-b.out{opacity:.45}
.glyph-time .tl-b i{position:absolute;display:block;pointer-events:none}.glyph-time .tl-b .tl-k{left:0;right:0;top:50%;height:1px;margin-top:-.5px;background:var(--rule);transition:background .25s,height .25s,margin .25s}.glyph-time .tl-b.open .tl-k{background:repeating-linear-gradient(90deg,var(--rule) 0 3px,transparent 3px 6px)}.glyph-time .tl-b .tl-f{left:0;top:50%;height:1px;margin-top:-.5px;width:0;background:var(--mut);opacity:.6;transition:width .7s cubic-bezier(.2,.7,.2,1)}
.glyph-time .tl-b.in .tl-k{background:var(--gchg);height:2px;margin-top:-1px}.glyph-time .tl-b.in .tl-f{display:none}.glyph-time .tl-b:hover .tl-k,.glyph-time .tl-b:focus-visible .tl-k{background:color-mix(in srgb,var(--gchg) 55%,var(--rule))}
.glyph-time .tl-b .tl-m{left:50%;top:50%;width:var(--d,4px);height:var(--d,4px);margin:calc(var(--d,4px)/-2) 0 0 calc(var(--d,4px)/-2);border-radius:50%;background:var(--gchg);box-shadow:0 0 0 2px var(--bg);transition:width .45s cubic-bezier(.2,.7,.2,1),height .45s cubic-bezier(.2,.7,.2,1),margin .45s cubic-bezier(.2,.7,.2,1),opacity .3s,transform .2s}.glyph-time .tl-b.ca .tl-m{opacity:.4}.glyph-time .tl-b.nil .tl-m{opacity:0}.glyph-time .tl-b:hover .tl-m{transform:scale(1.25)}
.glyph-time .tl-l{position:absolute;left:0;right:0;top:calc(100% - .05em);font-size:.68em;line-height:1.2;color:var(--mut);white-space:nowrap;overflow:hidden;text-overflow:clip;text-align:center;pointer-events:none;transition:color .2s,opacity .2s}.glyph-time .tl-b.in .tl-l{color:var(--fg)}.glyph-time .tl-s:has(.tl-b:nth-child(14)) .tl-b:not(:first-child):not(:last-child):not(.in):not(:hover):not(:nth-child(7n+1)) .tl-l{opacity:0}
.glyph-time .tl-r{font-size:.8em;line-height:1.45;color:var(--mut);min-height:1.45em;transition:opacity .22s ease,transform .22s ease}.glyph-time .tl-r.sw{opacity:0;transform:translateY(2px);transition:none}.glyph-time .tl-r .tl-k{color:var(--fg)}.glyph-time .tl-r .tl-v b{color:var(--fg)}.glyph-time .tl-r .c{color:var(--gchg)}.glyph-time .tl-r .ca{opacity:.45}.glyph-time .tl-r .tl-n{display:block}.glyph-time .tl-r .nw{white-space:nowrap}
.glyph-time .pb{display:flex;width:100%;height:.5em;margin:.35em 0 .15em;gap:1px;background:color-mix(in srgb,var(--fg) 8%,transparent);border-radius:2px;overflow:hidden}.glyph-time .pb svg{height:100%;display:block;flex:0 0 auto}.glyph-time .pb .arr{width:1.1em;fill:none;stroke:var(--mut);stroke-width:1.4;stroke-linecap:round;stroke-linejoin:round;padding:0 .1em}.glyph-time .lg{display:flex;flex-wrap:wrap;gap:.1em .9em;font-size:.92em;line-height:1.45}.glyph-time .it{white-space:nowrap}.glyph-time .lg .arw .arr{width:.8em;height:.8em;vertical-align:-.1em;fill:none;stroke:var(--mut);stroke-width:1.4;stroke-linecap:round;stroke-linejoin:round}.glyph-time .lg-m{appearance:none;border:0;background:none;color:var(--mut);font:inherit;line-height:1;padding:0 .3em;border-radius:3px;cursor:pointer;letter-spacing:.1em;opacity:.8}.glyph-time .lg-x{display:contents}.glyph-time .lg-x[hidden]{display:none}.glyph-time .sw{display:inline-block;width:.65em;height:.65em;vertical-align:-.05em;margin-right:.2em;outline:1px solid var(--rule)}
.glyph-time .tl-ld{position:absolute;left:0;right:0;bottom:-.3em;height:1px;background:linear-gradient(90deg,transparent,var(--gchg),transparent);background-size:40% 100%;background-repeat:no-repeat;opacity:0;transition:opacity .3s}.glyph-time.busy .tl-ld{opacity:.7;animation:tl-ld 1.4s ease-in-out infinite}@keyframes tl-ld{0%{background-position:-40% 0}100%{background-position:140% 0}}
svg.glyph .dt path.tl,svg.glyph .hx path.tl{--age:1;transition:opacity .6s ease,stroke .6s ease,fill .6s ease}svg.glyph.v-chg .dt path.ch.tl{opacity:.16}svg.glyph.v-chg .dt path.tl,svg.glyph.v-chg .dt path.ch.tl{stroke:color-mix(in srgb,var(--gchg) calc(var(--age)*100%),var(--mut))}svg.glyph.v-chg .hx path.tl{fill:color-mix(in srgb,var(--gchg) calc(var(--age)*100%),var(--mut))}
@media(prefers-reduced-motion:reduce){.glyph-time,.glyph-time *,.glyph-time .tl-s{transition:none!important;animation:none!important}svg.glyph .dt path.tl,svg.glyph .hx path.tl{transition:none}}
@media(hover:none){.glyph-time .tl-s{height:1.8em}.glyph-time .tl-hd{border-left-width:2px}.glyph-time .tl-b{min-width:6px}}@media(max-width:640px){.glyph-time .tl-x{display:none}}
.glyph.fs .glyph-time{flex:0 0 auto;width:auto;margin-left:var(--fs-x,0);margin-right:var(--fs-x,0)}.glyph.fs .glyph-time.on{margin-top:.2em}.glyph.fs .glyph-time .tl-s{margin-bottom:.9em}
` + neGlyphFiltCSS + neGlyphExportCSS

const neGlyphScript = `<script>(function(){
var fig=document.querySelector('figure.glyph');if(!fig||!window.fetch)return;
/* telemetry for scripted checks (window.neGlyph.log(), docs/ne-glyph.md §soak): JS errors, and every fetch of the page counted by class × status — no screenshot needed to see a 204/404 or a thrown error */
var tel={t0:Date.now(),err:[],n:{}},_fetch=window.fetch.bind(window);function telErr(m,s){if(tel.err.length<50)tel.err.push({t:Math.round((Date.now()-tel.t0)/100)/10,m:String(m||'').slice(0,300),at:s||''})}
window.addEventListener('error',function(e){telErr(e.message||e,(e.filename||'').replace(/.*\//,'')+':'+(e.lineno||0)+':'+(e.colno||0))});window.addEventListener('unhandledrejection',function(e){var r=e.reason;telErr(r&&(r.stack||r.message)||r,'promise')});
function telN(u,k){var c=u.indexOf('/glyph/tile')>=0?'tile':u.indexOf('/tiles/hillshade/')>=0?'relief':u.indexOf('/basemap/')>=0?'ortho':/^https?:\/\//.test(u)?'peer':'api',o=tel.n[c]||(tel.n[c]={});o[k]=(o[k]||0)+1}
/* fail-soft (docs/ne-glyph.md §offline): every fetch of the page may fail — offline, a peer's 5xx — without an uncaught promise or a stuck busy button. A failure is remembered per source for a moment (net.what), the hint says so once, every card shows one muted offline row for the sources that did not answer, and nothing failed is cached for good: the next interaction (pick, pan, toggle) asks again after NET_TTL. neGlyph.fault(mode) injects faults for the soak: 'offline' (every fetch rejects like a lost network), '5xx' (every fetch answers 503), 'peer' (only cross-origin peers answer 503), null = off */
var net={at:0,what:{},fault:null,hinted:0},NET_TTL=2500;
function netWhat(u){return u.indexOf('/glyph/tile')>=0?'the picture':u.indexOf('/hillshade/')>=0?'relief':u.indexOf('/basemap/')>=0?'the photo':u.indexOf('{OBS_NAME}')>=0?'{OBS_NAME}':u.indexOf('/glyph/sub')>=0?'subsidies':u.indexOf('/glyph/cell')>=0?'this cell':u.indexOf('/ne/stats')>=0?'change figures':u.indexOf('/glyph/kgs')>=0?'KG search':u.indexOf('/glyph/admin')>=0?'places':u.indexOf('/addresses/')>=0?'addresses':u.indexOf('/toponyms/')>=0?'names':/^https?:\/\//.test(u)?'a peer':'umfeld-at'}
function netFail(u){var w=netWhat(u),now=Date.now();net.at=now;net.what[w]=now;if(now-net.hinted>20000){net.hinted=now;if(typeof showHint==='function')showHint('offline \u00b7 '+w+' did not answer \u2014 retries on your next move')}fig.classList.add('offline');if(typeof picked!=='undefined'&&picked&&typeof recard==='function')recard()}
function netGroup(w){return w==='the photo'?'basemap.at':w==='relief'?'{OBS_NAME}':(w==='{OBS_NAME}'||w==='subsidies'||w==='a peer')?w:'umfeld-at'}
function netOk(u){var g=netGroup(netWhat(u)),hit=false,any=false;for(var k in net.what){if(netGroup(k)===g){delete net.what[k];hit=true}else any=true}if(!hit)return;if(!any)fig.classList.remove('offline');if(typeof picked!=='undefined'&&picked&&typeof recard==='function')recard()}
function netRecent(w){var t=w?net.what[w]:net.at;return!!t&&Date.now()-t<NET_TTL}
/* an interaction (pan, zoom, pick, view) is the retry: marks older than NET_TTL are dropped — whatever the move needs is asked again and marks itself if it fails; a mark nobody re-asked within 12 s fades on its own */
function netPrune(max){var ks=[],now=Date.now();for(var k in net.what){if(now-net.what[k]<(max||12000))ks.push(k);else delete net.what[k]}if(!ks.length)fig.classList.remove('offline');return ks}
function netTouch(){netPrune(NET_TTL)}
function offRow(){var ks=netPrune();if(!ks.length)return'';return'<div class="rw off"><span class="lb">offline</span><span class="vl mut">'+ks.join(' \u00b7 ')+' \u2014 retries on your next move</span></div>'}
function offTxt(w){return'<span class="mut">offline</span>'}
function jsonOf(r){if(!r.ok)throw new Error('HTTP '+r.status);return r.json()}
function fetch(u,o){u=String(u);var f=net.fault,peer=/^https?:\/\//.test(u);if(f==='offline'||f==='5xx'||(f==='peer'&&peer)){var sg=o&&o.signal;return new Promise(function(res,rej){var ab=function(){var x=new Error('aborted');x.name='AbortError';telN(u,'abort');rej(x)};if(sg&&sg.aborted)return ab();var T=setTimeout(function(){if(sg)sg.removeEventListener('abort',ab);telN(u,f==='offline'?'err':503);netFail(u);if(f==='offline')rej(new TypeError('Failed to fetch'));else res(new Response('upstream error',{status:503,headers:{'Content-Type':'text/plain'}}))},40+Math.random()*120);if(sg)sg.addEventListener('abort',function(){clearTimeout(T);ab()})})}
return _fetch(u,o).then(function(r){telN(u,r.status);if(r.status>=500)netFail(u);else netOk(u);return r},function(x){var ab=x&&x.name==='AbortError';telN(u,ab?'abort':'err');if(!ab)netFail(u);throw x})}
var main=fig.querySelector('.glyph-main'),svg=main.querySelector('svg.glyph'),tip=fig.querySelector('.glyph-tip'),inner=tip.firstElementChild,cur=svg.querySelector('.cur'),ping=svg.querySelector('.ping'),hint=fig.querySelector('.glyph-hint'),dots=svg.querySelector('.dt');
/* time strip state (block ── time ── below); declared here because applyHash runs before that block is reached */
var tl={on:false,d:null,kg:{},scope:'',win:null,lvl:0,k:0,bins:[],map:null,play:0,hov:-1,pt:null,pg:{},busy:false,ld:null,err:0};
var tlEl=fig.querySelector('.glyph-time');
var W=+fig.dataset.w,H=+fig.dataset.h,per=+fig.dataset.per||343,fresh=+fig.dataset.fresh||6,ALPHA=+fig.dataset.alpha||0.13,picks=[];try{picks=JSON.parse(fig.dataset.picks||'[]')}catch(e){}
var hex=(fig.dataset.hex||'').split(',').map(Number);if(hex.length!==6||hex.some(isNaN))hex=null;
var rm=matchMedia('(prefers-reduced-motion:reduce)').matches,coarse=matchMedia('(hover:none)').matches,data=null,load=null,grid={},G=120,cells=[],kgs=[];
var AR=H/W,YF=.5,vb={x:0,y:0,w:W,h:H},ZMAX=24,HEX0=1.7,HEX1=3.2,shown=-1,pinned=-1,hideT=0,hover=false,picked=null,pk=null,hexG=null,hexBuilt=false,view='',cg={on:false};/* view = the profile ('' declared | 'obs'); cg = the change overlay; sb / hi.tree / rl = the other overlays (docs/ne-glyph.md §view model) */
/* ── hex field: every res-7 cell is a regular hexagon whose rotation and radius are linear in (x, y); the exact H3 tiling from six numbers ── */
function hexAt(x,y){return[(hex[0]+hex[1]*x+hex[2]*y)*Math.PI/180,hex[3]+hex[4]*x+hex[5]*y]}
function hexD(x,y,k){var h=hexAt(x,y),s='';for(var i=0;i<6;i++){var a=h[0]+i*1.0471975512,r=h[1]*k;s+=(i?'L':'M')+(x+Math.cos(a)*r).toFixed(1)+' '+(y+Math.sin(a)*r).toFixed(1)}return s+'Z'}
function circD(x,y,r){return'M'+(x-r)+' '+y+'a'+r+' '+r+' 0 1 0 '+(2*r)+' 0a'+r+' '+r+' 0 1 0 '+(-2*r)+' 0Z'}
/* a cell's face is the Voronoi region of its centre clipped to a slightly enlarged regular hexagon: interior cells tile seamlessly from the centres alone, rim cells keep the hex outline */
var HR=1.06,cellD=[],cellP=[];
function clipHP(poly,c,n){var nx=n[0]-c[0],ny=n[1]-c[1],mx=(c[0]+n[0])/2,my=(c[1]+n[1])/2,out=[],L=poly.length;for(var i=0;i<L;i++){var p=poly[i],q=poly[(i+1)%L],dp=(p[0]-mx)*nx+(p[1]-my)*ny,dq=(q[0]-mx)*nx+(q[1]-my)*ny;if(dp<=0)out.push(p);if((dp<0&&dq>0)||(dp>0&&dq<0)){var t=dp/(dp-dq);out.push([p[0]+(q[0]-p[0])*t,p[1]+(q[1]-p[1])*t])}}return out}
function faceD(i){if(cellD[i])return cellD[i];var c=cells[i],h=hexAt(c[0],c[1]),R=h[1]*HR,poly=[],k;for(k=0;k<6;k++){var a=h[0]+k*1.0471975512;poly.push([c[0]+Math.cos(a)*R,c[1]+Math.sin(a)*R])}
 var gx=(c[0]/G)|0,gy=(c[1]/G)|0,lim=(2.3*R)*(2.3*R);for(var u=-1;u<=1;u++)for(var v=-1;v<=1;v++){var b=grid[(gx+u)+'_'+(gy+v)];if(!b)continue;for(k=0;k<b.length;k++){var j=b[k];if(j===i)continue;var n=cells[j],dx=n[0]-c[0],dy=n[1]-c[1];if(dx*dx+dy*dy<lim)poly=clipHP(poly,c,n)}}
 var d='';for(k=0;k<poly.length;k++)d+=(k?'L':'M')+poly[k][0].toFixed(1)+' '+poly[k][1].toFixed(1);cellP[i]=poly;return cellD[i]=d+'Z'}
function inHex(x,y,c){if(!hex)return true;var h=hexAt(c[0],c[1]),dx=x-c[0],dy=y-c[1],ir=h[1]*HR*0.8660254;for(var i=0;i<3;i++){var a=h[0]+0.5235987756+i*1.0471975512;if(Math.abs(dx*Math.cos(a)+dy*Math.sin(a))>ir)return false}return true}
/* ── data (only on first touch; the tour runs on the inlined picks) ── */
function get(){if(load)return load;load=fetch('/api/v1/ne/glyph',{credentials:'omit'}).then(jsonOf).then(function(d){data=d;cells=d.cells;kgs=d.kgs;fresh=d.fresh_hours;per=d.chunks_per_cell;if(d.alpha)ALPHA=d.alpha;if(d.hex&&d.hex.rot&&d.hex.r)hex=d.hex.rot.concat(d.hex.r);
 cells.forEach(function(c,i){var k=((c[0]/G)|0)+'_'+((c[1]/G)|0);(grid[k]=grid[k]||[]).push(i)});if(W/vb.w>HEX0)buildHex();else if(window.requestIdleCallback)requestIdleCallback(buildHex,{timeout:4000});if(sb.d){subBuild();hiLegend()}if(tl.d){tlMap();tlPaint(true)}gvBusy();return d}).catch(function(e){load=null;gvBusy();dbgLog('glyph load failed: '+(e&&e.message||e));throw e});gvBusy();return load}
/* exact pick: nearest centre, then the point must lie inside that hexagon (a regular hex tiling's Voronoi cell is the hex itself) */
function near(x,y){var gx=(x/G)|0,gy=(y/G)|0,best=-1,bd=hex?26*26:44*44;for(var i=-1;i<=1;i++)for(var j=-1;j<=1;j++){var b=grid[(gx+i)+'_'+(gy+j)];if(!b)continue;
 for(var k=0;k<b.length;k++){var c=cells[b[k]],dx=c[0]-x,dy=c[1]-y,d=dx*dx+dy*dy;if(d<bd){bd=d;best=b[k]}}}return best>=0&&inHex(x,y,cells[best])?best:-1}
/* ── hex layer: built once from the fetched cells (same ink buckets as the server's dots), crossfaded in between HEX0× and HEX1× ── */
function buildHex(){if(hexBuilt||!data||!hex)return;hexBuilt=true;var B=16,T=560,tiles={},i,c,k;
 for(i=0;i<cells.length;i++){c=cells[i];k=((c[0]/T)|0)+'_'+((c[1]/T)|0);var t=tiles[k];if(!t){t=tiles[k]={x:((c[0]/T)|0)*T,y:((c[1]/T)|0)*T,em:[],ink:[],fr:[],o:[],c:[[],[],[],[]],ca:[[],[],[],[]]};for(var j=0;j<B;j++){t.ink[j]=[];t.o[j]=[]}for(j=0;j<fresh;j++)t.fr[j]=[]}
  var d=faceD(i);if(c[7])t.c[chgB(c[7])].push(d);else if(c[10])t.ca[chgB(c[10])].push(d);if(!c[2]){t.em.push(d);if(c[6])t.o[0].push(d);continue}var cov=Math.min(1,c[2]/per),ps=Math.max(1,Math.round(c[3]/c[2])),al=1-Math.pow(1-ALPHA*cov,ps),b=Math.min(B-1,(al*B)|0);t.ink[b].push(d);if(c[5]>=0&&c[5]<fresh)t.fr[Math.min(fresh-1,c[5]|0)].push(d);if(c[6])t.o[b].push(d)}
 var s='<g class="hx" opacity="0" style="display:none">',M=20;
 for(k in tiles){var t=tiles[k];s+='<g data-b="'+(t.x-M)+' '+(t.y-M)+' '+(t.x+T+M)+' '+(t.y+T+M)+'">';
  for(i=0;i<B;i++)if(t.ink[i].length)s+='<path class="h" fill-opacity="'+((i+.5)/B).toFixed(2)+'" d="'+t.ink[i].join('')+'"/>';
  for(i=0;i<fresh;i++)if(t.fr[i].length)s+='<path class="n" fill-opacity="'+(0.42*(1-i/fresh)).toFixed(2)+'" d="'+t.fr[i].join('')+'"/>';
  if(t.em.length)s+='<path class="e" d="'+t.em.join('')+'"/>';for(i=0;i<B;i++)if(t.o[i].length)s+='<path class="o" fill-opacity="'+(0.22+0.78*Math.min(1,(i+.5)/(B/2))).toFixed(2)+'" d="'+t.o[i].join('')+'"/>';for(i=0;i<4;i++)if(t.ca[i].length)s+='<path class="ca" fill-opacity="'+CANB[i]+'" d="'+t.ca[i].join('')+'"/>';for(i=0;i<4;i++)if(t.c[i].length)s+='<path class="c" fill-opacity="'+CHGB[i]+'" d="'+t.c[i].join('')+'"/>';s+='</g>'}
 dots.insertAdjacentHTML('afterend',s+'</g>');hexG=svg.querySelector('.hx');if(hi.g)hexG.parentNode.insertBefore(hi.g,hexG.nextSibling);hexTiles=[].map.call(hexG.children,function(g){return{g:g,b:g.dataset.b.split(' ').map(Number),on:true}});if(typeof sb!=='undefined'&&sb.d)subHex();if(tl.win)tlPaint(true);blend()}
var hexTiles=[],CHGB=[0.45,0.65,0.85,1],CANB=[0.2,0.26,0.32,0.38];function chgB(n){return n>=10?3:n>=5?2:n>=2?1:0}
/* cull: only tiles that touch the view are kept in the render tree (one display write per tile per change) */
function cull(){for(var i=0;i<hexTiles.length;i++){var t=hexTiles[i],b=t.b,on=b[2]>vb.x&&b[0]<vb.x+vb.w&&b[3]>vb.y&&b[1]<vb.y+vb.h;if(on!==t.on){t.on=on;t.g.style.display=on?'':'none'}}}
var blendK=-1,gapW=0;
function blend(){var z=W/vb.w,t=Math.max(0,Math.min(1,(z-HEX0)/(HEX1-HEX0)));if(t>0&&!hexBuilt){if(data)buildHex();else get()}if(!hexG)t=0;if(t>0){cull();var px=(hex?hex[3]:16)*2*svg.getBoundingClientRect().width/vb.w,sw=Math.max(.5,Math.min(1.5,px*.045));if(sw!==gapW){gapW=sw;hexG.style.strokeWidth=sw+'px'}}if(t===blendK)return;blendK=t;
 dots.style.display=t>=1?'none':'';dots.setAttribute('opacity',(1-t).toFixed(2));if(hexG){hexG.style.display=t<=0?'none':'';hexG.setAttribute('opacity',t.toFixed(2))}}
/* ── card ── */
function ago(h){if(h==null||h<0)return'';if(h<1)return Math.max(1,Math.round(h*60))+' min ago';if(h<48)return Math.round(h)+' h ago';return Math.round(h/24)+' d ago'}
function fmt(n){return String(n).replace(/\B(?=(\d{3})+(?!\d))/g,'\u202f')}
var BL=['1st','2nd','3rd\u20134th','5th\u20139th','10th+'];
function km2(n){var a=n*0.0150475;return a<1?Math.round(a*100)+' ha':a<10?a.toFixed(1)+' km\u00b2':fmt(Math.round(a))+' km\u00b2'}
function pct(n,t){var p=100*n/t;return p>0&&p<0.5?'<1\u202f%':Math.round(p)+'\u202f%'}
/* the depth bar: the same pass-band split (1st · 2nd · 3rd–4th · 5th–9th · 10th+) the daily chart's card shows, for the chunks inside this cell — same classes, same hatches, so the two read as one */
/* ── observed · {OBS_NAME}: the LiDAR + satellite picture of the same hexagon, fetched live from the peer's public API for the hexagon's bbox (CORS, CC BY 4.0); one call per cell, debounced on hover, cached, aborted on move ── */
var proj=null;try{proj=JSON.parse(fig.dataset.proj||'null')}catch(e){}
var OBS='{OBS_WEB}/api/v1/landscape?include=cells&bbox=',obsC={},obsN=0,obsT=0,obsA=null;
var GL={geb:'buildings',bau:'building land',acker:'cropland',gruen:'grassland',wald:'forest',wasser:'water',verkehr:'traffic',alpen:'alpine',sonst:'other',unknown:'declared unknown'};
var CL={consistent:'consistent',forest_loss:'forest loss',forest_gain:'forest gain',sealed_new:'newly sealed',structure_new:'new structure',green_new:'newly green',unknown:'unknown'};
function lonlat(x,y){return[((x-proj.pad)/proj.scale+proj.minx)/proj.kx,-((y-proj.pad)/proj.scale+proj.miny)]}
function bboxOf(c){var r=hex?hexAt(c[0],c[1])[1]:16,a=lonlat(c[0]-r,c[1]+r),b=lonlat(c[0]+r,c[1]-r);return[a[0].toFixed(5),a[1].toFixed(5),b[0].toFixed(5),b[1].toFixed(5)].join(',')}
function ckey(c){return c.k||(c[0]+'_'+c[1])}
function ldots(){return'<span class="dots" aria-hidden="true"><i>.</i><i>.</i><i>.</i></span>'}
/* a KG selection spans more than one srtm tile (≤ 0.05° a side): split its bbox, ask every tile at once, fold the answers into one picture weighted by cell count */
var TILE=0.049,POOL=8;
function tilesOf(bb){var out=[],nx=Math.ceil((bb[2]-bb[0])/TILE),ny=Math.ceil((bb[3]-bb[1])/TILE),i,j;
 for(i=0;i<nx;i++)for(j=0;j<ny;j++){var w=bb[0]+i*(bb[2]-bb[0])/nx,e=bb[0]+(i+1)*(bb[2]-bb[0])/nx,s=bb[1]+j*(bb[3]-bb[1])/ny,n=bb[1]+(j+1)*(bb[3]-bb[1])/ny;out.push([w.toFixed(5),s.toFixed(5),e.toFixed(5),n.toFixed(5)].join(','))}return out}
function wadd(dst,src,w){for(var k in src)if(typeof src[k]==='number')dst[k]=(dst[k]||0)+src[k]*w}
function merge(parts){var ce={n:0,cover:{},consistency_hist:{},structures_n:0,trees_n:0,canopy:0,height:{p90:0}},lc={fractions:{}},tr={elevation_mean_m:0,slope_mean_deg:0},nc=0,nl=0,nt=0,ok=0,i,d,c,cw=0,hw=0;
 for(i=0;i<parts.length;i++){d=parts[i];if(!d||d.err)continue;ok++;
  if(d.terrain&&d.terrain.elevation_mean_m!=null){var tw=(d.coverage&&d.coverage.cells_valid)||1;tr.elevation_mean_m+=d.terrain.elevation_mean_m*tw;tr.slope_mean_deg+=(d.terrain.slope_mean_deg||0)*tw;nt+=tw}
  c=d.cells;if(c&&c.cover&&c.n){ce.n+=c.n;nc+=c.n;wadd(ce.cover,c.cover,c.n);if(c.consistency_hist)wadd(ce.consistency_hist,c.consistency_hist,1);ce.structures_n+=c.structures_n||0;ce.trees_n+=c.trees_n||0;
   if(c.canopy!=null){ce.canopy+=c.canopy*c.n;cw+=c.n}if(c.height&&c.height.p90!=null){ce.height.p90+=c.height.p90*c.n;hw+=c.n}}
  else if(d.landcover&&d.landcover.fractions){var lw=(d.coverage&&d.coverage.cells_valid)||1;wadd(lc.fractions,d.landcover.fractions,lw);nl+=lw}}
 if(!ok)return{err:'not processed yet'};var out={};
 if(nt){out.terrain={elevation_mean_m:tr.elevation_mean_m/nt,slope_mean_deg:tr.slope_mean_deg/nt}}
 if(nc){for(var k in ce.cover)ce.cover[k]/=nc;ce.canopy=cw?ce.canopy/cw:null;ce.height.p90=hw?+(ce.height.p90/hw).toFixed(1):null;if(!Object.keys(ce.consistency_hist).length)delete ce.consistency_hist;out.cells=ce;out.tiles=parts.length}
 else if(nl){for(var q in lc.fractions)lc.fractions[q]/=nl;out.landcover=lc}
 else return{err:'not processed yet'};return out}
function obsPut(k,v){if(obsN>400){obsC={};obsN=0}if(!(k in obsC))obsN++;obsC[k]=v}
var obsProg={},KGC='{OBS_WEB}/api/v1/kg/';
/* the whole-KG summary srtm publishes (exact to the KG boundary, one call, ~50 ms) → the same shape the tile roll-up has */
function fromKG(j){var m=j&&j.summary;if(!m||!m.cover_ha)return null;var ce={n:m.cells_observed||0,cover:{},consistency_hist:m.consistency_n||null,structures_n:m.structures_n||0,trees_n:m.trees_n||0,canopy:null,height:{p90:null}},t=0,k;
 for(k in m.cover_ha)t+=m.cover_ha[k];if(!t)return null;for(k in m.cover_ha)ce.cover[k]=m.cover_ha[k]/t;if(!ce.n)ce.n=Math.round(t*100/0.0316);var o={cells:ce,exact:true};
 if(m.terrain&&m.terrain.elev_min_m!=null)o.range=[m.terrain.elev_min_m,m.terrain.elev_max_m];if(j.kgs>1)o.exact=(j.kgs_processed<j.kgs?fmt(j.kgs_processed)+' of ':'all ')+fmt(j.kgs)+' KGs';return o}
function topUp(d,t){if(!d||!d.cells||!t||!t.cells)return d;var ce=d.cells,te=t.cells;if(ce.canopy==null)ce.canopy=te.canopy;if(ce.height.p90==null)ce.height.p90=te.height&&te.height.p90;if(!d.terrain&&t.terrain)d.terrain=t.terrain;d.tiles=t.tiles;return d}
/* run the tile requests through a small pool, nearest-the-centre first, so the first answers paint the picture early and nothing is ever skipped */
function pool(items,n,fn){var i=0;function next(){if(i>=items.length)return Promise.resolve();var it=items[i++];return fn(it).then(next)}var ws=[];for(var w=0;w<Math.min(n,items.length);w++)ws.push(next());return Promise.all(ws)}
function obsOne(bb,sig){return fetch(OBS+bb,{signal:sig,credentials:'omit'}).then(function(r){if(r.status>=500)throw new Error('HTTP '+r.status);return r.json().catch(function(){return null}).then(function(d){return r.ok?d:{err:(d&&d.ready===false)?'not processed yet':'unavailable'}})})
 .then(function(d){if(d&&d.ready===false&&!d.cells&&!(d.landcover&&d.landcover.fractions))d={err:'not processed yet'};return d})}
function obsFetch(c){var k=ckey(c);clearTimeout(obsT);if(obsC[k]===null&&!netRecent('{OBS_NAME}'))delete obsC[k];if(!proj||!window.AbortController||k in obsC||document.hidden)return;
 obsT=setTimeout(function(){if(obsA)obsA.abort();var a=obsA=new AbortController();if(c.filt||c.kgs){obsSet(c,k,a);return}var tiles=c.bb?tilesOf(c.bb):[bboxOf(c)];
  if(tiles.length>1){var cx=(c.bb[0]+c.bb[2])/2,cy=(c.bb[1]+c.bb[3])/2;tiles.sort(function(p,q){var u=p.split(','),v=q.split(',');return Math.hypot((+u[0]+ +u[2])/2-cx,(+u[1]+ +u[3])/2-cy)-Math.hypot((+v[0]+ +v[2])/2-cx,(+v[1]+ +v[3])/2-cy)})}
  var pr=obsProg[k]={n:0,t:tiles.length,parts:[],kg:null},raf=0,paint=function(){if(!raf)raf=requestAnimationFrame(function(){raf=0;if(obsProg[k]===pr)obsSwap(k)})};
  if(c.kg)fetch(KGC+c.kg+'/cells?format=json',{signal:a.signal,credentials:'omit'}).then(function(r){return r.json()}).then(function(j){pr.kg=fromKG(j)||false;paint()}).catch(function(){pr.kg=false});
  pool(tiles,POOL,function(t){return obsOne(t,a.signal).then(function(d){pr.n++;pr.parts.push(d);paint()})})
  .then(function(){cancelAnimationFrame(raf);delete obsProg[k];var m=pr.parts.length===1?pr.parts[0]:merge(pr.parts);obsPut(k,pr.kg?topUp(pr.kg,m):m);obsSwap(k)})
  .catch(function(e){delete obsProg[k];if(e.name==='AbortError')return;obsPut(k,pr.kg||null);obsSwap(k)})},shown>=0?260:60)}
/* an area = a KG set: srtm's merged summary (/api/v1/kgs/summary?gemeinde|district|state=, exact to the KG boundaries, ~80 ms even for a Bundesland); a filter srtm does not map (Wien Gemeindebezirke, Natura 2000 sites) falls back to codes= (≤ 500) */
var KGSUM='{OBS_WEB}/api/v1/kgs/summary?';
function obsSet(c,k,a){var pr=obsProg[k]={n:0,t:1,parts:[],kg:null},u=c.filt?KGSUM+c.filt:null,codes=(c.kgs||[]).slice(0,500);
 function byCodes(){if(!codes.length)return Promise.resolve({err:'no KG inside'});return fetch(KGSUM+'codes='+codes.join(','),{signal:a.signal,credentials:'omit'}).then(function(r){if(r.status>=500)throw 0;return r.json()})}
 (u?fetch(u,{signal:a.signal,credentials:'omit'}).then(function(r){if(r.status>=500)throw 0;return r.status===400?byCodes():r.json()}):byCodes())
 .then(function(j){delete obsProg[k];var d=j&&j.err?j:(fromKG(j)||{err:'not processed yet'});if(d.cells&&!d.cells.n)d={err:'not processed yet'};if(!c.filt&&typeof d.exact==='string')d.exact+=' touching';obsPut(k,d);obsSwap(k)})
 .catch(function(e){delete obsProg[k];if(e.name==='AbortError')return;obsPut(k,null);obsSwap(k)})}
/* swap only the observed part in when the answer lands, so the declared half never flickers */
function obsSwap(k,prog){var cur=picked;if(!cur||ckey(cur)!==k)return;swapIn('.src.ob',obsHTML(cur))}
/* an async part lands: only that part is replaced, and it settles in with the same fade the card uses */
function swapIn(sel,html){var o=inner.querySelector(sel);if(!o||o.innerHTML===html)return;o.innerHTML=html;o.classList.remove('up');void o.offsetWidth;o.classList.add('up');if(picked&&floating())place(picked)}
/* the one-line observed summary of the collapsed card: the two biggest cover groups and the consistent share */
function obsLine(d,pr,c){if(d===undefined){if(pr&&pr.kg)d=pr.kg;else if(pr&&pr.parts.length)d=merge(pr.parts);else return'<span class="mut">'+obsWait(c,pr)+'</span>'}
 if(d===null)return offTxt('{OBS_NAME}');if(d.err)return'<span class="mut">'+d.err+'</span>';
 var ce=d.cells,lc=d.landcover,m=ce&&ce.cover?ce.cover:lc&&lc.fractions,L=ce&&ce.cover?GL:CL,o=[];if(!m)return'<span class="mut">not processed yet</span>';
 var ks=Object.keys(m).filter(function(k){return m[k]>0}).sort(function(a,b){return m[b]-m[a]}),t=0,i;for(i=0;i<ks.length;i++)t+=m[ks[i]];
 if(ce&&ce.consistency_hist){var h=ce.consistency_hist,th=0,tu=0,k;for(k in h){th+=h[k];if(k!=='unknown')tu+=h[k]}if(tu)o.push('<b>'+pct(h.consistent||0,th)+'</b> consistent')}
 for(i=0;i<Math.min(2,ks.length);i++)o.push(sw('g-'+ks[i])+(L[ks[i]]||ks[i].replace(/_/g,' '))+' <span class="mut">'+pct(m[ks[i]],t)+'</span>');
 if(!o.length){if(ce&&ce.structures_n)o.push(fmt(ce.structures_n)+' structures');if(ce&&ce.canopy!=null)o.push('canopy '+Math.round(100*ce.canopy)+'\u202f%');if(d.terrain&&d.terrain.elevation_mean_m!=null)o.push(Math.round(d.terrain.elevation_mean_m)+'\u202fm a.s.l.');if(!o.length)o.push('<span class="mut">not classified yet</span>')}
 return o.join('<span class="sep"></span>')}
function obsWait(c,p){return(p&&p.kg?'canopy, heights, slope':p&&p.n?'combining':(c&&(c.filt||c.kgs)?'asking {OBS_NAME} for the whole area':'asking {OBS_NAME}'))+(p&&p.t>1?'<span class="sep"></span>'+p.n+'\u2009/\u2009'+p.t+' tiles':'')+ldots()}
function sw(cls,hatch){return'<svg class="sw" viewBox="0 0 10 10" aria-hidden="true"><rect width="10" height="10" class="'+cls+'"/>'+(hatch?'<rect width="10" height="10" fill="url(#'+hatch+')"/>':'')+'</svg>'}
function seg(cls,w,hatch){return'<svg style="width:'+(100*w).toFixed(1)+'%"><rect width="100%" height="100%" class="'+cls+'"/>'+(hatch?'<rect width="100%" height="100%" fill="url(#'+hatch+')"/>':'')+'</svg>'}
function item(cls,hatch,lab,p,title){return'<span class="it"'+(title?' title="'+title+'"':'')+'>'+sw(cls,hatch)+lab+' <span class="mut">'+p+'</span></span>'}
/* a legend list: the first max items read at once, the rest unfold behind … (so every segment of a bar has its name) */
function legend(items,max,lead){var n=max||5,s='<div class="lg">'+(lead||'');if(items.length<=n)return s+items.join('')+'</div>';
 return s+items.slice(0,n).join('')+'<button type="button" class="lg-m" aria-expanded="false" title="'+(items.length-n)+' more">\u2026</button><span class="lg-x" hidden>'+items.slice(n).join('')+'</span></div>'}
tip.addEventListener('click',function(e){var b=e.target.closest('.lg-m');if(!b)return;e.stopPropagation();var x=b.nextElementSibling,on=x.hidden;x.hidden=!on;b.setAttribute('aria-expanded',on?'true':'false');b.textContent=on?'\u2039':'\u2026';if(picked&&floating())place(picked)});
/* the depth bar: the same pass-band split (1st · 2nd · 3rd–4th · 5th–9th · 10th+) the daily chart's card shows, for the chunks inside this cell — same classes, same hatches, so the two read as one */
function bands(c){var n=c[2],b=c[9]||[],s='<div class="pb" role="img" aria-label="digest depth of the land in this cell">',l='',i,k;
 for(i=0;i<5;i++){k=b[i]||0;if(!k)continue;var h=i>=2?'ng-h'+i:null;s+=seg('p'+i,k/n,h);l+=item('p'+i,h,BL[i],pct(k,n),fmt(k)+' chunk'+(k==1?'':'s')+' \u00b7 '+km2(k))}
 if(!l)return'';var re=n-(b[0]||0);s+='</div><div class="lg">'+l+'</div>';
 s+='<div class="nt">'+(re>0?'<b>'+pct(re,n)+'</b> seen before \u2192 comparable for change':'<span class="mut">all first sight \u2014 nothing comparable yet</span>')+'</div>';return s}
function obsBar(m,cls,lab,max){var ks=Object.keys(m).filter(function(k){return m[k]>0}).sort(function(a,b){return m[b]-m[a]}),t=0,i;for(i=0;i<ks.length;i++)t+=m[ks[i]];if(!t)return'';
 var s='<div class="pb '+cls+'" role="img" aria-label="'+lab+'">',l=[];
 for(i=0;i<ks.length;i++){var k=ks[i],v=m[k]/t;s+=seg('g-'+k,v);l.push(item('g-'+k,null,GL[k]||CL[k]||k.replace(/_/g,' '),pct(v,1)))}
 return s+'</div>'+legend(l,max)}
function obsHTML(c){var k=ckey(c),d=obsC[k],s='<div class="rw"><span class="lb">observed</span><span class="vl">'+obsLine(d,obsProg[k],c)+'</span></div><div class="dt"><div class="sh"><span>observed <i>LiDAR &amp; satellite \u00b7 {OBS_NAME}</i></span>'+(c[6]?'<em class="o" title="observed rows for this land are posted to umfeld-at">\u25cf posted</em>':'')+'</div>';
 var pr=obsProg[k],wait=d===undefined?'<div class="nt mut ld" aria-live="off">'+obsWait(c,pr)+'</div>':'';
 if(d===undefined){if(pr&&pr.kg)d=pr.kg;else if(pr&&pr.parts.length){d=merge(pr.parts);if(d.err)return s+wait+'</div>'}else return s+wait+'</div>'}
 if(d===null)return s+'<div class="nt">'+offTxt('{OBS_NAME}')+'</div></div>';
 if(d.err)return s+'<div class="nt mut">'+d.err+'</div></div>';
 var ce=d.cells,lc=d.landcover,f=[];
 if(ce&&ce.cover){s+=obsBar(ce.cover,'cov','observed land cover by group');
  if(ce.consistency_hist){var h=ce.consistency_hist,t=0,k2;for(k2 in h)t+=h[k2];var ok=h.consistent||0;
   var ks=Object.keys(h).sort(function(a,b){return(a==='consistent'?-1:b==='consistent'?1:h[b]-h[a])});s+='<div class="pb con" role="img" aria-label="observed versus declared, per cell">';for(var q=0;q<ks.length;q++)if(h[ks[q]]>0)s+=seg('g-'+ks[q],h[ks[q]]/t);
   var dif=[],li=[item('g-consistent',null,'consistent',pct(ok,t))];for(k2 in h)if(k2!=='consistent'&&h[k2]>0)dif.push([k2,h[k2]]);dif.sort(function(a,b){return b[1]-a[1]});
   for(var j=0;j<dif.length;j++)li.push(item('g-'+dif[j][0],null,CL[dif[j][0]]||dif[j][0],pct(dif[j][1],t)));s+='</div>'+legend(li,4,'<span class="it lead">vs declared</span>')}
  if(d.exact)f.push(d.exact===true?'whole KG':d.exact);else if(d.tiles>1&&!wait)f.push(d.tiles+' tiles combined');if(ce.structures_n)f.push(fmt(ce.structures_n)+' structures');if(ce.trees_n)f.push(fmt(ce.trees_n)+' trees');if(ce.canopy!=null)f.push('canopy '+Math.round(100*ce.canopy)+'\u202f%');
  if(ce.height&&ce.height.p90!=null)f.push('h\u2089\u2080 '+ce.height.p90+'\u202fm')}
 else if(lc&&lc.fractions){s+=obsBar(lc.fractions,'cov lc','observed land cover by class');f.push('25\u202fm grid \u00b7 cells not published yet')}
 else return s+'<div class="nt mut">not processed yet</div></div>';
 if(d.terrain&&d.terrain.elevation_mean_m!=null)f.push(Math.round(d.terrain.elevation_mean_m)+'\u202fm a.s.l.',Math.round(d.terrain.slope_mean_deg)+'\u00b0 slope');else if(d.range)f.push(Math.round(d.range[0])+'\u2013'+fmt(Math.round(d.range[1]))+'\u202fm a.s.l.');
 if(f.length)s+='<div class="nt mut">'+f.join('<span class="sep"></span>')+'</div>';
 return s+wait+'</div>'}
/* the one-line declared summary of the collapsed card */
function decLine(n,b){if(!n)return'<span class="mut">not digested yet</span>';var re=n-((b||[])[0]||0),o=['<b>'+km2(n)+'</b>',re>0?'<b>'+pct(re,n)+'</b> seen before':'<span class="mut">first sight</span>'];return o.join('<span class="sep"></span>')}
function card(c){var n=c[2]||0,nm=(n||c[8]>=0)?(typeof c[8]==='string'?c[8]:(kgs[c[8]]||'')):'',i=nm.indexOf(' \u00b7 '),kg=i>0?nm.slice(0,i):nm,gm=i>0?nm.slice(i+3):'',s='';
 if(nm){var code=typeof c[8]==='number'&&data?data.kg_codes[c[8]]:(c.kg||''),e=ixBy[code];if(code&&!e&&!ix)ixGet().then(function(){if(pk&&pk.t==='cell')recard()});s+='<div class="hd"><b>'+kgLink(kg,code)+'</b><span class="age'+(n&&c[5]<fresh?' n':'')+'">'+(n?(c[5]<fresh?'\u25cf ':'')+ago(c[5]):'<span class="mut">no report yet</span>')+'</span></div>';var sub=e?placeOf(e,code):(gm&&norm(gm)!==norm(kg)?'Gemeinde '+gm:'');if(sub)s+='<div class="sub">'+esc(sub)+'</div>'}
 else s+='<div class="hd"><b class="mut">empty cell</b></div>';
 var dec='<div class="rw"><span class="lb">declared</span><span class="vl">'+decLine(n,c[9])+'</span></div>';
 dec+='<div class="dt"><div class="sh"><span>declared <i>survey land use \u00b7 BEV tiles via bevdirect</i></span></div>';
 dec+=(n?'<div class="nt kv"><b>'+km2(n)+'</b> digested<span class="sep"></span>'+fmt(n)+'\u2009/\u2009'+per+' chunks<span class="sep"></span>'+fmt(c[4])+' report'+(c[4]==1?'':'s')+'<span class="sep"></span>'+(c[3]/n).toFixed(1)+'\u00d7 per chunk</div>'+bands(c):'<div class="nt mut">not digested yet</div>')+'</div>';
 var code=typeof c[8]==='number'&&data?data.kg_codes[c[8]]:(c.kg||'');c.kgc=code;
 s+=srcs(dec,obsHTML(c),chgHTML(c));if(subVis()){var ix=c.ix!==undefined?c.ix:(c.ix=cells.indexOf(c));if(ix>=0)s+=subDotLine(ix)}obsFetch(c);chgFetch(c);return s+hiLegendHTML()}
/* the three sources in the order the view asks for: the one on the map reads first */
function srcs(dec,ob,ch){var a={dec:'<div class="src dec">'+dec+'</div>',ob:'<div class="src ob">'+ob+'</div>',chg:'<div class="src chg">'+ch+'</div>'},o=view==='obs'?['ob','dec','chg']:['dec','ob','chg'];if(cg.on){o.splice(o.indexOf('chg'),1);o.unshift('chg')}return a[o[0]]+a[o[1]]+a[o[2]]+offRow()}
/* ── change: the confirmed cells inside (from the glyph rows) at once, the KG's Δ land use per group fetched async from /api/v1/ne/stats?kg= and swapped in ── */
var chgC={},chgN=0,chgT=0,chgA=null;
function chgPut(k,v){if(chgN>300){chgC={};chgN=0}if(!(k in chgC))chgN++;chgC[k]=v}
function chgKey(c){return c.filt||c.kgs?c.k:(c.kgc||c.kg)}
function chgFetch(c){var kg=chgKey(c);clearTimeout(chgT);if(chgC[kg]===null&&!netRecent('change'))delete chgC[kg];if(!kg||kg in chgC||!window.AbortController||document.hidden)return;var set=!!(c.filt||c.kgs),q=c.filt?c.filt:set?'kg='+(c.kgs||[]).slice(0,1000).join(','):'kg='+kg;if(set&&!c.filt&&!(c.kgs||[]).length){chgPut(kg,false);return}
 chgT=setTimeout(function(){if(chgA)chgA.abort();var a=chgA=new AbortController(),k=ckey(c);fetch('/api/v1/ne/stats?'+q,{signal:a.signal,credentials:'omit'}).then(jsonOf).then(function(j){var p=set?(j&&j.kgs_with_log>0?j.change:null):(j&&j.per_kg&&j.per_kg[kg]);chgPut(kg,p||(j&&j.kgs_with_log===0?false:null));chgSwap(k)}).catch(function(e){if(e.name==='AbortError')return;chgPut(kg,null);chgSwap(k)})},shown>=0?220:60)}
function chgSwap(k){var cur=picked;if(!cur||ckey(cur)!==k)return;swapIn('.src.chg',chgHTML(cur))}
function ha(m2){return(m2<0?'\u2212':'+')+areaU(m2)}
/* Δ bar: |Δ| per group, legend signed */
var ARR='<svg class="arr" viewBox="0 0 10 10" aria-hidden="true"><path d="M2.5 2l3 3-3 3M6 2l3 3-3 3"/></svg>';
function dluBar(d,lab,max){var fr=[],to=[],t=0,i;for(i=0;i<9;i++)if(d[i]<0){fr.push(i);t-=d[i]}else if(d[i]>0){to.push(i);t+=d[i]}if(!t)return'';fr.sort(function(a,b){return d[a]-d[b]});to.sort(function(a,b){return d[b]-d[a]});
 var s='<div class="pb chg" role="img" aria-label="'+lab+' \u00b7 lost \u00bb gained">',l=[],one=function(g){s+=seg('g-'+GO[g],Math.abs(d[g])/t);l.push(item('g-'+GO[g],null,GL[GO[g]],ha(d[g])))};fr.forEach(one);if(fr.length&&to.length){s+=ARR;l.push('<span class="it arw" aria-hidden="true">'+ARR+'</span>')}to.forEach(one);return s+'</div>'+legend(l,max||4)}
function chgHTML(c){var n=c[7]||0,nc=c[10]||0,kg=chgKey(c),st=kg?chgC[kg]:false,line=[],s='',area=c.filt||c.kgs?'area':'KG';
 if(n)line.push('<span class="c">\u25cf</span> <b>'+fmt(n)+'</b> confirmed cell'+(n==1?'':'s'));if(nc)line.push('<span class="c ca">\u25cf</span> '+fmt(nc)+' candidate cell'+(nc==1?'':'s'));
 if(st===undefined)line.push('<span class="mut">asking for \u0394'+ldots()+'</span>');
 else if(st){var cf=st.confirmed||{},ca=st.candidate||{},d=cf.d_lu_m2||[],top=[],i;for(i=0;i<9;i++)if(d[i])top.push([i,Math.abs(d[i])]);top.sort(function(a,b){return b[1]-a[1]});for(i=0;i<Math.min(2,top.length);i++)line.push(sw('g-'+GO[top[i][0]])+GL[GO[top[i][0]]]+' <span class="mut">'+ha(d[top[i][0]])+'</span>');
  if(ca.cells&&!nc)line.push('<span class="mut">'+fmt(ca.cells)+' candidate'+(ca.cells==1?'':'s')+' in the '+area+'</span>');if(!line.length)line.push('<span class="mut">'+(st.versions>st.chunks?'digests agree \u2014 nothing moved':'one digest so far \u2014 nothing to compare')+'</span>')}
 else if(!line.length)line.push('<span class="mut">'+(c[2]?'nothing comparable yet':'not digested yet')+'</span>');
 s+='<div class="rw"><span class="lb">change</span><span class="vl">'+line.join('<span class="sep"></span>')+'</span></div>';
 s+='<div class="dt"><div class="sh"><span>change <i>between digests \u00b7 confirmed = seen twice</i></span>'+(n||nc?'<em class="c">'+(n?'\u25cf '+fmt(n)+' confirmed':'')+(n&&nc?' \u00b7 ':'')+(nc?'<span class="ca">\u25cf</span> '+fmt(nc)+' candidate':'')+'</em>':'')+'</div>';
 if(st===undefined)s+='<div class="nt mut ld">asking for the '+area+'\u2019s \u0394'+ldots()+'</div>';
 else if(st===null)s+='<div class="nt">'+offTxt('change')+'</div>';
 else if(st){var f=[];if(cf.cells)f.push('<b>'+fmt(cf.cells)+'</b> confirmed');if(ca.cells)f.push(fmt(ca.cells)+' candidate');if(cf.d_str_n)f.push('structures '+(cf.d_str_n>0?'+':'\u2212')+Math.abs(cf.d_str_n));if(cf.d_str_cover_m2)f.push('structure cover '+ha(cf.d_str_cover_m2));if(st.since)f.push('since '+esc(String(st.since).slice(0,10)));if(st.versions)f.push(fmt(st.versions)+' digests');
  var lead=c.filt||c.kgs?(c.kgn?fmt(c.kgn)+' KGs':'area'):'KG '+esc(kg);s+=(cf.cells?dluBar(cf.d_lu_m2||[],'confirmed \u0394 declared use by group, '+lead):'')+'<div class="nt mut"><span class="it lead">'+lead+'</span> '+(f.length?f.join('<span class="sep"></span>'):'nothing moved between the digests')+'</div>';
  var q=st.qc;if(q&&(q.cells_pipeline||q.cells_jitter||q.cells_coverage||q.cells_clustered||q.cells_isolated)){var qf=[];if(q.cells_pipeline)qf.push(fmt(q.cells_pipeline)+' pipeline');if(q.cells_jitter)qf.push(fmt(q.cells_jitter)+' jitter');if(q.cells_coverage)qf.push(fmt(q.cells_coverage)+' coverage');var ex=qf.length?qf.join(' + ')+' excluded':'';var cl=[];if(q.cells_clustered)cl.push(fmt(q.cells_clustered)+' clustered');if(q.cells_isolated)cl.push(fmt(q.cells_isolated)+' isolated');
   s+='<div class="nt mut" title="qc: a delta whose input tile hash did not move is pipeline, one below the quantisation floor is jitter, one between two digests holding different cell sets is coverage \u2014 none is counted; clustered = counted cells with a counted neighbour"><span class="it lead">qc</span> '+[ex,cl.join(' \u00b7 ')].filter(Boolean).join('<span class="sep"></span>')+(q.versions_with_tile_hash!=null&&q.versions?'<span class="sep"></span>'+pct(q.versions_with_tile_hash,q.versions)+' of digests carry a tile hash':'')+'</div>'}}
 else s+='<div class="nt mut">'+(c[2]?'no second digest yet \u2014 nothing to compare':'not digested yet')+'</div>';
 return s+'</div>'}
var curAt=null;
var curI=-1;
function drawCur(x,y,i){curAt=[x,y];curI=i>=0?i:-1;if(hi.on&&(hi.pin||hi.hov)){cur.setAttribute('d',polyD((hi.pin||hi.hov).poly));return}cur.setAttribute('d',!hex?circD(x,y,22):(curI>=0&&data&&W/vb.w>HEX0)?faceD(curI):hexD(x,y,W/vb.w>HEX0?1:1.25))}
function ring(c,i){drawCur(c[0],c[1],i);svg.classList.add('on')}
var swapT=0,swapH=0;
function floating(){return getComputedStyle(tip).getPropertyValue('--float').trim()!=='0'}
var moved=null,anchor=null,pendCard=null;/* moved: [x,y] in figure px once the user dragged the card; anchor: the automatic spot it last got, kept across selections, zooms and content swaps so its controls never run away; both cleared when it closes. pendCard: card= from a share link, in map-box fractions, applied once the card is up */
function putAt(x,y){var fr=fig.getBoundingClientRect(),w=tip.offsetWidth,h=tip.offsetHeight,vw=document.documentElement.clientWidth,vh=window.innerHeight;
 x=Math.max(-fr.left+4,Math.min(x,vw-fr.left-w-4));y=Math.max(-fr.top+4,Math.min(y,vh-fr.top-Math.min(h,40)));moved=[x,y];tip.style.transform='translate('+Math.round(x)+'px,'+Math.round(y)+'px)'}
function place(c,keep){if(!floating()){tip.style.transform='';tip.style.maxHeight='';tip.classList.remove('clip');return}
 var fr=fig.getBoundingClientRect();if(pendCard){var pc=pendCard;pendCard=null;tip.classList.add('moved');putAt(pc[0]*fr.width,pc[1]*fr.height);return}if(moved){putAt(moved[0],moved[1]);return}
 var r=svg.getBoundingClientRect(),sx=r.width/vb.w,px=r.left-fr.left+(c[0]-vb.x)*sx,py=r.top-fr.top+(c[1]-vb.y)*sx,R=Math.max(14,(c.rad||(hex?hex[3]:16)*HR)*sx+8),top=Math.max(0,r.top-fr.top),bot=r.bottom-fr.top;
 /* the card never runs past the map's bottom edge (where the tool bar sits): taller than the map, it clips and scrolls inside */
 var PM=8;top+=PM;bot-=PM;tip.style.maxHeight=Math.max(80,bot-top)+'px';tip.classList.add('clip');var w=tip.offsetWidth,h=tip.offsetHeight,x,y,i,X0=PM,X1=fr.width-PM;
 /* sticky: the spot it already has is kept (clamped to the map box) unless the selection is now under it — zooming and panning (keep) never move it */
 if(anchor){x=Math.max(X0,Math.min(anchor[0],X1-w));y=Math.max(top,Math.min(anchor[1],bot-h));var cov=px+R>x&&px-R<x+w&&py+R>y&&py-R<y+h;if(keep||!cov){anchor=[x,y];tip.style.transform='translate('+Math.round(x)+'px,'+Math.round(y)+'px)';return}}
 var cands=[[px+R,py-h/2],[px-R-w,py-h/2],[px-w/2,py+R],[px-w/2,py-R-h]];
 for(i=0;i<cands.length;i++){x=cands[i][0];y=cands[i][1];if(x>=X0&&x+w<=X1&&y>=top&&y+h<=bot)break}
 if(i===cands.length){/* nothing fits: take the roomier side, clamp — and if the selection is still under the card, pan the map just enough to bring it out beside the card (the card itself never moves for a pan) */
  var right=px<fr.width/2;x=right?X1-w:X0;x=Math.max(X0,x);y=Math.max(top,Math.min(py-h/2,bot-h));
  if(px+R>x&&px-R<x+w&&py+R>y&&py-R<y+h&&!keep&&!applying&&!animating&&X1-X0-w>=2*R){var tx=right?x-R:x+w+R;anchor=[x,y];tip.style.transform='translate('+Math.round(x)+'px,'+Math.round(y)+'px)';zoomTo(vb.x-(tx-px)/sx,vb.y,vb.w,rm?0:320);return}}
 anchor=[x,y];tip.style.transform='translate('+Math.round(x)+'px,'+Math.round(y)+'px)'}
function setHTML(html){clearTimeout(swapT);clearTimeout(swapH);if(rm||floating()){inner.innerHTML=html;return}var h0=inner.offsetHeight;inner.style.height=h0+'px';inner.style.opacity=0;
 swapT=setTimeout(function(){inner.innerHTML=html;inner.style.height='';var h1=inner.offsetHeight;inner.style.height=h0+'px';void inner.offsetHeight;inner.style.height=h1+'px';inner.style.opacity=1;
  swapH=setTimeout(function(){inner.style.height=''},380)},140)}
function reveal(c,html,isTour){clearTimeout(hideT);tip.setAttribute('aria-live',isTour?'off':'polite');tip.classList.toggle('tour',!!isTour);tip.classList.add('x');if(html!=null)setHTML(html);
 var first=!tip.classList.contains('on');tip.classList.add('on');place(c);if(first&&floating()){tip.style.transition='none';void tip.offsetWidth;tip.style.transition=''}}
function show(i){if(i<0){hide();return}var c=cells[i];reveal(c,i!==shown?card(c):null,false);shown=i;picked=c;pk={t:'cell',i:i};ring(c,i)}
/* the view changed: the same card, its sources reordered (async parts come back from their caches) */
function recard(){if(!picked||!pk)return;var h=pk.t==='cell'?card(cells[pk.i]):pk.t==='kg'||pk.t==='adm'?areaCard():pk.t==='h3'?hiCard(pk.d):pk.t==='tree'?apCard(pk.a):pk.t==='pick'?card(pk.c):pk.t==='off'?offCardHTML():null;if(h!=null)setHTML(h)}
var PH='<span class="mut">'+(coarse?'tap a cell · pinch to zoom':'hover a cell · click for its card · ctrl + scroll to zoom · / finds a KG, a place or an address')+'</span>';
function hide(){tip.classList.remove('follow');tagSync();if(hi.on&&hi.apx){return}if(hi.on&&hi.pin){hiShow(hi.pin,false);return}if(pinned>=0){show(pinned);return}if(sel||asel){showSel();return}svg.classList.remove('on');tip.classList.remove('x');if(floating()){tip.classList.remove('on');unmove();hideT=setTimeout(function(){shown=-1;picked=null},220);return}
 if(shown>=0||picked){shown=-1;picked=null;hideT=setTimeout(function(){setHTML(PH)},160)}}
function at(e){var r=svg.getBoundingClientRect();return[vb.x+(e.clientX-r.left)/r.width*vb.w,vb.y+(e.clientY-r.top)/r.height*vb.h]}
/* ── view: zoom + pan on the viewBox (one attribute write per frame; dots ↔ hexes crossfade by zoom) ── */
var anim=0,vbT=0;
/* AR = box height / width: Austria's own aspect, or the viewport's when full screen on a taller (portrait) screen — then the map fills the screen and Austria sits centred in the extra height */
function clamp(x,y,w){w=Math.max(W/ZMAX,Math.min(Math.max(W,H/AR),w));var h=w*AR;return{x:w>=W?(W-w)/2:Math.max(0,Math.min(W-w,x)),y:h>=H?(H-h)*YF:Math.max(0,Math.min(H-h,y)),w:w,h:h}}
function setVB(x,y,w){if(!live.in&&live.t)liveDrop();vb=clamp(x,y,w);svg.setAttribute('viewBox',vb.x+' '+vb.y+' '+vb.w+' '+vb.h);var z=W/vb.w>1.01;main.classList.toggle('zoomed',z);blend();hiView();if(typeof sb!=='undefined'&&subVis()&&!hi.on){clearTimeout(sb.lt);sb.lt=setTimeout(hiLegend,160)}availLazy();hiAuto();setHash();if(selO)selO.style.strokeWidth=Math.max(.8,3-.38*Math.log(W/vb.w)/Math.LN2).toFixed(2)+'px';if(curAt)drawCur(curAt[0],curAt[1],curI);if(ptG)ptSync();if(picked)place(picked,true);if(z){pause(0);if(!setVB.h){setVB.h=1;showHint(coarse?(coop()?'two fingers move the map · double-tap to reset':'drag to pan · double-tap to reset'):'drag to pan · esc resets')}}}
var animating=false,animT0=0;function stopAnim(){cancelAnimationFrame(anim);animating=false}
/* animated: the first frame is scheduled, never run inline — setVB → hiLegend → place may itself call zoomTo (pan the selection out from under the card), and a synchronous first step made that re-enter here, leaving a loop whose frame had been cancelled and the animating flag stuck */
function zoomTo(x,y,w,ms){if(DBG&&w>=W)dbgLog('zoomTo W '+(new Error().stack||'').split('\n').slice(2,4).join('|').replace(/https?:\S*\//g,''));stopAnim();var t=clamp(x,y,w);if(rm||!ms){navCommit();setVB(t.x,t.y,t.w);return}var s=live.t||vb,t0=performance.now();animating=true;animT0=t0;
 anim=requestAnimationFrame(function step(now){var k=Math.min(1,(now-t0)/ms);k=1-Math.pow(1-k,3);if(k<1){navLive(s.x+(t.x-s.x)*k,s.y+(t.y-s.y)*k,s.w+(t.w-s.w)*k);anim=requestAnimationFrame(step)}else{animating=false;live.t=t;navCommit();if(picked)place(picked,true)}})}
function zoomAt(px,py,f,ms){dbgLog('zoomAt f='+f);var b=live.t||vb,w=Math.max(W/ZMAX,Math.min(W,b.w/f)),r=w/b.w,x=px-(px-b.x)*r,y=py-(py-b.y)*r;if(ms)zoomTo(x,y,w,ms);else navLive(x,y,w)}
/* ── live transform. A viewBox change re-lays-out and re-rasterises every path and image in the SVG — 30–100 ms on a phone or an office PC at full resolution, which is why a pinch or a drag used to stutter and, with the backlog of moves, feel frozen. During a gesture (pinch, drag, wheel burst, zoom animation) the SVG is therefore moved with a CSS transform only: one composited layer, no layout, every frame lands. The real viewBox (setVB: tiles, hex faces, legend, hash, card) is committed at an adaptive interval — short while commits are cheap, up to ~1 s on a slow device — and as soon as the hand stops. at(e) stays exact meanwhile: the transformed rect shows the committed viewBox. ── */
var live={t:null,r:0,last:0,ms:160,tm:0,box:null,in:false};
function navLive(x,y,w){live.t=clamp(x,y,w);if(!live.r)live.r=requestAnimationFrame(livePaint);clearTimeout(live.tm);live.tm=setTimeout(navCommit,Math.max(80,live.ms*0.5))}
function livePaint(){live.r=0;var t=live.t;if(!t)return;if(performance.now()-live.last>=live.ms){navCommit();return}
 if(!svg.style.transform){live.box=svg.getBoundingClientRect();main.classList.add('live')}var b=live.box;if(!b||!b.width){navCommit();return}
 svg.style.transform='translate('+((vb.x-t.x)/t.w*b.width).toFixed(2)+'px,'+((vb.y-t.y)/t.h*b.height).toFixed(2)+'px) scale('+(vb.w/t.w).toFixed(5)+')'}
function liveDrop(){clearTimeout(live.tm);cancelAnimationFrame(live.r);live.r=0;live.t=null;svg.style.transform='';main.classList.remove('live')}
function navCommit(){var t=live.t;liveDrop();if(!t)return;var t0=performance.now();live.in=true;try{setVB(t.x,t.y,t.w)}finally{live.in=false}var d=performance.now()-t0;live.last=performance.now();live.ms=d>48?Math.min(1000,live.ms*1.5):d<14?Math.max(120,live.ms*0.85):live.ms}
/* the untransformed box of the SVG — gestures are measured against it, whatever the transform of the moment */
function navRect(){return live.t&&live.box?live.box:svg.getBoundingClientRect()}
function navVB(){return live.t||vb}
function zoomCenter(f){var c=picked&&pinned>=0?picked:null,cx=vb.x+vb.w/2,cy=vb.y+vb.h/2;if(c&&c[0]>vb.x&&c[0]<vb.x+vb.w&&c[1]>vb.y&&c[1]<vb.y+vb.h){cx=c[0];cy=c[1]}zoomAt(cx,cy,f,220)}
/* ?dbg: an on-screen trace of the gesture pipeline for phone triage — no devtools on a phone */
var DBG=/[?&]dbg\b/.test(location.search),dbgEl=null,dbgL=[];function dbgLog(s){dbgL.push((performance.now()/1000).toFixed(2)+' '+s+(vb?' z='+(W/vb.w).toFixed(1):'')+' ZMAX='+ZMAX+' k='+(typeof order==='object'&&order?order.length:0)+(typeof hi==='object'&&hi&&hi.on?' hi':'')+(typeof live==='object'&&live&&live.t?' live':''));if(dbgL.length>40)dbgL.shift();if(!DBG)return;if(!dbgEl){dbgEl=document.createElement('pre');dbgEl.style.cssText='position:fixed;left:0;bottom:0;z-index:99999;max-width:100vw;max-height:45vh;overflow:hidden;margin:0;padding:4px;font:10px/1.2 monospace;background:rgba(0,0,0,.8);color:#9f9;pointer-events:none;white-space:pre-wrap';document.body.appendChild(dbgEl)}dbgEl.textContent=dbgL.slice(-18).join('\n')}
if(DBG)['pointerdown','pointerup','pointercancel','lostpointercapture','touchstart','touchend','touchcancel','click','dblclick'].forEach(function(t){window.addEventListener(t,function(e){dbgLog(t+' '+(e.pointerType||'')+' '+(e.touches?e.touches.length:'')+' '+(e.clientX!=null?Math.round(e.clientX)+','+Math.round(e.clientY):'')+' '+(e.target&&e.target.tagName)+(e.target&&e.target.className&&e.target.className.baseVal==null?'.'+String(e.target.className).slice(0,16):''))},true)});
if(DBG)['blur','pagehide','resize','hashchange'].forEach(function(t){window.addEventListener(t,function(){dbgLog(t)},true)});
function reset(){dbgLog('RESET '+(new Error().stack||'').split('\n').slice(1,4).join('|').replace(/https?:\S*\//g,''));zoomTo(0,0,W,260);if(!(pinned>=0))pause(5000)}
/* × on any tooltip (hover, pinned, KG, tour): close it, stop the tour for good, then glide back to all of Austria */
var dismissT=0;function dismiss(){clearTimeout(dismissT);unmove();tour.on=false;pause(0);hover=false;pinned=-1;unpeek();clearSel(true);closeTools();if(hi.on){hi.pin=null;hi.hov=null;hiOff(false)}setHash();svg.classList.remove('on');hide();
 if(W/vb.w>1.01)dismissT=setTimeout(function(){if(pinned<0&&!sel)zoomTo(0,0,W,rm?0:900)},rm?0:280)}
fig.querySelector('.glyph-x').addEventListener('click',function(e){e.stopPropagation();dismiss();main.focus({preventScroll:true})});
/* drag: grip or header moves the card anywhere (off the map too); the position holds until the card closes; double-click on the grip snaps back to automatic placement */
var grip=fig.querySelector('.glyph-grip'),dg=null;
function unmove(){anchor=null;if(!moved)return;moved=null;tip.classList.remove('moved')}
function dragStart(e){if(e.button>0||!floating()||!tip.classList.contains('x'))return;if(e.target.closest('button,a,.lg-m'))return;e.preventDefault();e.stopPropagation();pause(0);
 var fr=fig.getBoundingClientRect(),r=tip.getBoundingClientRect();dg={id:e.pointerId,ox:e.clientX-(r.left-fr.left),oy:e.clientY-(r.top-fr.top),x0:e.clientX,y0:e.clientY,on:false};
 try{tip.setPointerCapture(e.pointerId)}catch(z){}}
function dragMove(e){if(!dg||e.pointerId!==dg.id)return;e.stopPropagation();if(!dg.on){if(Math.abs(e.clientX-dg.x0)+Math.abs(e.clientY-dg.y0)<3)return;dg.on=true;tip.classList.add('moving','moved')}putAt(e.clientX-dg.ox,e.clientY-dg.oy)}
function dragEnd(e){if(!dg||e.pointerId!==dg.id)return;e.stopPropagation();try{tip.releasePointerCapture(dg.id)}catch(z){}tip.classList.remove('moving');if(!dg.on)tip.classList.remove('moved');else setHash();dg=null}
tip.addEventListener('pointerdown',function(e){if(e.target.closest('.glyph-grip,.hd,.sub,.sh'))dragStart(e)});
tip.addEventListener('pointermove',dragMove);tip.addEventListener('pointerup',dragEnd);tip.addEventListener('pointercancel',dragEnd);
/* dock ⇄ float: below the map (page flow, like the narrow layout) or floating over it; the choice sticks */
var dk=fig.querySelector('.glyph-dk'),DK='neg-dock',mq=window.matchMedia('(max-width:640px),(hover:none) and (pointer:coarse)');
function setDock(on,save){var was=tip.classList.contains('dock');if(was===on)return;unmove();tip.classList.remove('moving');var hadCard=!!picked;tip.classList.toggle('dock',on);dk.setAttribute('aria-pressed',on?'true':'false');dk.setAttribute('aria-label',on?'float the card over the map':'dock the card below the map');dk.title=on?'float over the map':'dock below the map';
 if(on){tip.classList.add('on');tip.style.transform='';if(!hadCard)inner.innerHTML=PH}else if(!hadCard){tip.classList.remove('on');tip.classList.remove('x')}
 if(hadCard){tip.style.transition='none';place(picked);void tip.offsetWidth;tip.style.transition=''}
 if(save)try{localStorage.setItem(DK,on?'1':'')}catch(e){}}
dk.addEventListener('click',function(e){e.stopPropagation();setDock(!tip.classList.contains('dock'),true);setHash();main.focus({preventScroll:true})});
['pointerdown','pointerup','pointermove','dblclick'].forEach(function(t){dk.addEventListener(t,function(e){e.stopPropagation()})});
grip.addEventListener('dblclick',function(e){e.stopPropagation();unmove();if(picked)place(picked);setHash()});
['click','dblclick','wheel','touchstart','touchmove'].forEach(function(t){grip.addEventListener(t,function(e){e.stopPropagation()},{passive:t!=='click'&&t!=='dblclick'})});
window.addEventListener('resize',function(){if(moved&&picked)place(picked)});
/* chevrons-down opens the full detail (bars, legends, figures) and the choice sticks; collapsed is the default so the tour and the async srtm answer never move the page */
var more=fig.querySelector('.glyph-more'),OK='neg-open',openT;
/* FLIP on the card body: measure, flip the class, measure again, then let the height transition carry the card from one to the other while the freshly shown part fades in; the chevron rotates instead of being swapped */
function setOpen(on,save,anim){var was=tip.classList.contains('open');clearTimeout(openT);var h0=anim&&!rm&&was!==on?inner.offsetHeight:-1;
 tip.classList.toggle('open',on);more.setAttribute('aria-expanded',on?'true':'false');more.setAttribute('aria-label',on?'hide detail':'show detail');more.title=on?'less':'more';if(hi.on&&was!==on)hiLegend();
 if(h0>=0){inner.style.transition='none';inner.style.height='';var h1=inner.offsetHeight;inner.style.height=h0+'px';void inner.offsetHeight;inner.style.transition='';inner.style.height=h1+'px';tip.classList.add('swap');
  openT=setTimeout(function(){inner.style.height='';tip.classList.remove('swap')},380)}
 if(save)try{localStorage.setItem(OK,on?'1':'')}catch(e){}if(picked&&floating())place(picked)}
/* the KG name in the header is a link to the KG: a tap does what finding it does — the tour stops for good, the KG is selected, outlined and flown to, the find history gets it. The code rides on the button when the row knows it (glyph rows); a tour stop only carries the name, so the code is read off the nearest glyph row once the data is here. */
function kgLink(name,code){return '<button type="button" class="kgl"'+(code?' data-kg="'+esc(code)+'"':'')+' title="go to this KG \u00b7 outline and zoom">'+esc(name)+'</button>'}
/* hard stop: the tour never resumes, the card stays where it is (pause() would hide it) */
function freeze(){if(!tour.run&&!tour.on)return;tour.on=false;tour.run=false;clearTimeout(tour.t);clearTimeout(tour.idle);cancelAnimationFrame(glide);tip.classList.remove('tour');tip.setAttribute('aria-live','polite')}
tip.addEventListener('click',function(e){var b=e.target.closest('.kgl');if(!b||b.classList.contains('go'))return;e.stopPropagation();e.preventDefault();var c=picked;freeze();b.classList.add('go');
 var codeP=b.dataset.kg?Promise.resolve(b.dataset.kg):get().then(function(){if(!c)return'';var i=near(c[0],c[1]);return i>=0&&typeof cells[i][8]==='number'?data.kg_codes[cells[i][8]]:''});
 codeP.then(function(code){if(!code){b.classList.remove("go");return}return selectKG(code)}).then(function(){b.classList.remove('go');main.focus({preventScroll:true})},function(){b.classList.remove('go')})});
['pointerdown','dblclick'].forEach(function(t){tip.addEventListener(t,function(e){if(e.target.closest('.kgl'))e.stopPropagation()})});
/* during the tour the tap freezes the tour on this stop (the card stays, the cell gets pinned once the data is here) instead of pausing — pause() would hide the card */
more.addEventListener('click',function(e){e.stopPropagation();var c=picked;
 if(tour.run){freeze();
  if(c)get().then(function(){if(picked!==c||pinned>=0||sel)return;var i=near(c[0],c[1]);if(i>=0){pinned=i;shown=i;ring(c,i);setHash()}})}
 else pause(0);
 setOpen(!tip.classList.contains('open'),true,true);setHash()});
try{setOpen(localStorage.getItem(OK)==='1')}catch(e){}
/* tooltips: one bubble for every titled control in the figure (tool bar, view bar, card buttons). While it shows, the control's title lives in data-tt so the browser does not draw its own; code that rewrites titles meanwhile is picked up by an attribute observer, and code that reads one goes through ttl(). */
var tt=fig.querySelector('.glyph-tt'),ttEl=null,ttT=0,ttLast=0,ttMO=window.MutationObserver?new MutationObserver(function(){if(ttEl&&ttEl.title){ttEl.dataset.tt=ttEl.title;ttEl.removeAttribute('title');ttText();ttPlace()}}):null;
function ttl(el){return el.title||el.dataset.tt||''}
function ttText(){var s=ttEl?ttl(ttEl):'',m=/^(.*?)\s*\(\s*([^()\s]{1,3})\s*\)\s*$/.exec(s),h=esc(m?m[1]:s);if(m&&m[1])h+='<kbd>'+esc(m[2])+'</kbd>';tt.innerHTML=h}
function ttPlace(){if(!ttEl)return;var r=ttEl.getBoundingClientRect(),f=fig.getBoundingClientRect(),w=tt.offsetWidth,h=tt.offsetHeight,x=r.left-f.left+r.width/2-w/2,y=r.bottom-f.top+6,up=false;
 if(x<4)x=4;if(x+w>f.width-4)x=f.width-4-w;if(y+h>f.height-4&&r.top-f.top-h-6>0){y=r.top-f.top-h-6;up=true}tt.classList.toggle('up',up);tt.style.transform='';tt.style.left=Math.round(x)+'px';tt.style.top=Math.round(y)+'px'}
function ttShow(el){if(!el||el===ttEl)return;ttHide(true);if(!ttl(el))return;ttEl=el;if(el.title){el.dataset.tt=el.title;el.removeAttribute('title')}if(ttMO)ttMO.observe(el,{attributes:true,attributeFilter:['title']});var now=performance.now(),quick=now-ttLast<400;clearTimeout(ttT);
 ttT=setTimeout(function(){if(ttEl!==el)return;ttText();tt.hidden=false;ttPlace();tt.classList.add('on')},quick?40:420)}
function ttHide(keep){clearTimeout(ttT);if(!ttEl)return;var el=ttEl;ttEl=null;if(ttMO)ttMO.disconnect();if(el.dataset.tt!==undefined){if(!el.title)el.title=el.dataset.tt;delete el.dataset.tt}if(tt.classList.contains('on'))ttLast=performance.now();tt.classList.remove('on');if(!keep)setTimeout(function(){if(!ttEl)tt.hidden=true},160)}
function ttTarget(t){var el=t&&t.closest?t.closest('button[title],button[data-tt],.glyph-grip[title],.glyph-grip[data-tt]'):null;return el&&fig.contains(el)&&!main.contains(el)?el:null}
fig.addEventListener('pointerover',function(e){if(e.pointerType&&e.pointerType!=='mouse')return;var el=ttTarget(e.target);if(el)ttShow(el);else if(ttEl&&!ttEl.contains(e.target)&&!ttEl.matches(':focus-visible'))ttHide()});
fig.addEventListener('pointerout',function(e){if(!ttEl||e.pointerType!=='mouse')return;if(e.relatedTarget&&ttEl.contains(e.relatedTarget))return;if(ttEl.matches(':focus-visible'))return;ttHide()});
fig.addEventListener('focusin',function(e){var el=ttTarget(e.target);if(el&&el.matches(':focus-visible'))ttShow(el)});
fig.addEventListener('focusout',function(e){if(ttEl&&e.target===ttEl&&!ttEl.matches(':hover'))ttHide()});
/* a click changes what the control would say: hide, then say the new thing if the hand is still there */
fig.addEventListener('pointerdown',function(e){if(!ttEl)return;var el=ttEl;ttHide();if(e.pointerType==='mouse')setTimeout(function(){if(el.matches(':hover')&&ttl(el)){ttLast=0;ttShow(el)}},700)},true);
document.addEventListener('keydown',function(e){if(e.key==='Escape'&&ttEl)ttHide()},true);
window.addEventListener('scroll',function(){if(ttEl&&tt.classList.contains('on'))ttPlace()},{passive:true});
var hintT=0;function showHint(s){hint.textContent=s;hint.hidden=false;hint.classList.add('on');clearTimeout(hintT);hintT=setTimeout(function(){hint.classList.remove('on')},1600)}
/* ── pointer: hover, tap/click, double-tap, drag, pinch ── */
var ptrs={},order=[],pinch=null,drag=null,tapT=0,tapP=null,navR=0,coopT=0;
function keys(){return order.slice()}
/* Own every contact, not just the mouse. A lost up/capture used to leave two
   dead contacts in ptrs: every subsequent finger then moved an ignored third
   contact, so even switching to full screen could not unfreeze navigation.
   Defences, in order: a primary pointerdown resets; touchstart/touchend on the
   window reconcile the contact count with the browser's own touch list (that
   list is authoritative — pointer ups can be lost, touches.length cannot);
   resize/scroll re-anchor the gesture instead of killing it. */
function navCancel(){cancelAnimationFrame(navR);navR=0;var ids=keys();ptrs={};order=[];pinch=null;drag=null;tapT=0;tapP=null;main.classList.remove('drag');ids.forEach(function(id){try{main.releasePointerCapture(+id)}catch(e){}});if(whl){cancelAnimationFrame(whl.r);whl.r=0;whl.d=0;whl.e=null}stopAnim();navCommit()}
function navDrop(id){if(!ptrs[id])return;delete ptrs[id];order=order.filter(function(k){return k!==id});try{main.releasePointerCapture(+id)}catch(x){}}
/* cooperative gestures: embedded on a touch screen (not full screen) one finger belongs to the page — it scrolls — and two fingers move and zoom the map, the convention of every embedded map (Google, Mapbox, Leaflet); full screen gives the single finger to the map */
function coop(){return coarse&&!fsOn}
function navBase(moved){var k=keys(),r=navRect(),b=navVB();pinch=null;drag=null;
 if(k.length>=2){var a=ptrs[k[0]],c=ptrs[k[1]];pinch={d:Math.max(1,Math.hypot(a[0]-c[0],a[1]-c[1])),c:[(a[0]+c[0])/2,(a[1]+c[1])/2],vb:b,r:r};tapT=0;tapP=null;main.classList.remove('drag')}
 else if(k.length){var p=ptrs[k[0]];drag={x:p[0],y:p[1],vb:b,r:r,moved:!!moved,touch:ptrs[k[0]].t==='touch'};main.classList.toggle('drag',!!moved&&W/b.w>1.01&&!(drag.touch&&coop()))}}
/* Pointer hardware can deliver hundreds of samples per second. Paint only the
   newest positions once per frame (navLive: a transform, the viewBox follows). */
function navFlush(){cancelAnimationFrame(navR);navR=0;var k=keys();
 if(pinch&&k.length>=2){var r=pinch.r;if(!r.width||!r.height)return;var a=ptrs[k[0]],b=ptrs[k[1]],d=Math.max(1,Math.hypot(a[0]-b[0],a[1]-b[1])),c=[(a[0]+b[0])/2,(a[1]+b[1])/2];
  var w=Math.max(W/ZMAX,Math.min(W,pinch.vb.w*pinch.d/d)),sx=w/r.width,sy=w*AR/r.height,px=pinch.vb.x+(pinch.c[0]-r.left)/r.width*pinch.vb.w,py=pinch.vb.y+(pinch.c[1]-r.top)/r.height*pinch.vb.h;
  navLive(px-(c[0]-r.left)*sx,py-(c[1]-r.top)*sy,w);return}
 if(drag&&k.length){var r=drag.r;if(!r.width||!r.height)return;var p=ptrs[k[0]],dx=p[0]-drag.x,dy=p[1]-drag.y;if(!drag.moved){if(dx*dx+dy*dy<36)return;drag.moved=true;tapT=0;tapP=null;
   if(drag.touch&&coop()){coopHint();return}main.classList.toggle('drag',W/drag.vb.w>1.01)}
  if(drag.touch&&coop())return;
  if(W/drag.vb.w>1.01)navLive(drag.vb.x-dx*drag.vb.w/r.width,drag.vb.y-dy*drag.vb.h/r.height,drag.vb.w)}}
function coopHint(){var n=performance.now();if(n-coopT<1500)return;coopT=n;showHint(W/navVB().w>1.01?'two fingers move the map \u00b7 one finger scrolls the page':'pinch to zoom \u00b7 double-tap zooms in')}
function pinAt(p){netTouch();if(hi.on){hiPick(p);return}var done=function(){if(!data){offCard(p);return}var i=near(p[0],p[1]);if(i>=0&&pinned!==i)selectCell(i,false);else{pinned=-1;if(sel)clearSel();else{hide();setHash();pause(6000)}}};data?done():get().then(done)}
var userNav=0;['pointerdown','wheel','keydown'].forEach(function(t){main.addEventListener(t,function(){userNav++},{passive:true})});
main.addEventListener('pointerdown',function(e){if(e.button>0)return;if(e.isPrimary&&keys().length)navCancel();if(navR)navFlush();stopAnim();navCommit();clearTimeout(dismissT);get();pause(0);unpeek();
 if(!ptrs[e.pointerId])order.push(String(e.pointerId));var p=[e.clientX,e.clientY];p.t=e.pointerType;ptrs[e.pointerId]=p;if(order.length>2){navDrop(order[0])}navBase(keys().length>1);
 try{main.setPointerCapture(e.pointerId)}catch(x){}});
main.addEventListener('pointermove',function(e){
 if(ptrs[e.pointerId]){if(e.pointerType==='mouse'&&!e.buttons){navCancel();return}var p=[e.clientX,e.clientY];p.t=e.pointerType;ptrs[e.pointerId]=p;if(!navR)navR=requestAnimationFrame(navFlush);return}
 if(e.pointerType==='mouse'){hover=true;pause(0);mv=e;if(!mvR)mvR=requestAnimationFrame(onHover)}});
/* touch-model reconciliation (see navCancel). touchstart: more contacts of ours than the browser's → ours are stale. Two fingers: keep the page still (pan-y would otherwise let a two-finger drag scroll it). touchend with no touch left: the gesture is over whatever pointer events said. */
window.addEventListener('touchstart',function(e){var n=e.touches.length;if(order.length>n){var stale=order.slice(0,order.length-n);stale.forEach(navDrop);navBase(true)}if(n>=2&&main.contains(e.target)&&e.cancelable)e.preventDefault()},{passive:false,capture:true});
main.addEventListener('touchmove',function(e){if((e.touches.length>=2||(!coop()&&W/navVB().w>1.01))&&e.cancelable)e.preventDefault()},{passive:false});
['touchend','touchcancel'].forEach(function(t){window.addEventListener(t,function(e){if(!e.touches.length&&order.length){var ids=keys();ids.forEach(navDrop);pinch=null;drag=null;main.classList.remove('drag');navCommit()}},{passive:true,capture:true})});
['gesturestart','gesturechange'].forEach(function(t){main.addEventListener(t,function(e){e.preventDefault()},{passive:false})});
/* hover, one pass per frame: with a selection (KG, pinned cell, pinned res-12 cell) it only peeks — the card and the selection are the click's; without one the card follows the cursor (no transform easing while it follows) */
var peek=main.querySelector('.glyph-peek'),hov=null,peekK='',peekW=0,peekH=0,mv=null,mvR=0;
function selected(){return!!sel||pinned>=0||(hi.on&&!!(hi.pin||hi.apx))}
function hovRing(d){if(!hov){cur.insertAdjacentHTML('afterend','<path class="hov" d=""/>');hov=svg.querySelector('.hov')}if(!d){hov.classList.remove('on');return}if(hov.getAttribute('d')!==d)hov.setAttribute('d',d);hov.classList.add('on')}
function peekAt(e,key,html,ringD){if(key!==peekK){peekK=key;peek.innerHTML=html;peekW=peek.offsetWidth;peekH=peek.offsetHeight;hovRing(ringD)}
 var fr=fig.getBoundingClientRect(),cx=e.clientX-fr.left,cy=e.clientY-fr.top,cr=null;if(floating()&&tip.classList.contains('on')&&tip.classList.contains('x')){var ta=moved||anchor;if(ta)cr=[ta[0],ta[1],ta[0]+tip.offsetWidth,ta[1]+tip.offsetHeight]}
 var cs=[[cx+14,cy+18],[cx-peekW-10,cy+18],[cx+14,cy-peekH-10],[cx-peekW-10,cy-peekH-10]],x=cs[0][0],y=cs[0][1],i;
 for(i=0;i<cs.length;i++){var ax=cs[i][0],ay=cs[i][1];if(ax<4||ax+peekW>fr.width-4||ay<4||ay+peekH>fr.height-4)continue;if(cr&&ax<cr[2]&&ax+peekW>cr[0]&&ay<cr[3]&&ay+peekH>cr[1])continue;x=ax;y=ay;break}
 if(i===cs.length){if(cr&&cx>cr[0]&&cx<cr[2]&&cy>cr[1]&&cy<cr[3]){unpeek();return}x=Math.max(4,Math.min(cx+14,fr.width-peekW-4));y=Math.max(4,Math.min(cy+18,fr.height-peekH-4))}
 peek.style.transform='translate('+Math.round(x)+'px,'+Math.round(y)+'px)';peek.classList.add('on');clearTimeout(peekT);peekT=setTimeout(peekCheck,1200)}
function unpeek(){clearTimeout(peekT);if(!peekK&&!peek.classList.contains('on'))return;peekK='';peek.classList.remove('on');hovRing(null)}
/* watchdog: a peek that outlives the pointer (scroll under a still mouse, window blur, a missed pointerleave) goes away once the map is no longer hovered */
var peekT=0;function peekCheck(){clearTimeout(peekT);if(!peek.classList.contains('on'))return;if(main.matches(':hover')){peekT=setTimeout(peekCheck,1200);return}leaveMain()}
function leaveMain(){hover=false;unpeek();if(hi.on&&!hi.pin){hi.hov=null;clearTimeout(hiT)}if(!drag){hide();if(pinned<0)pause(4500)}}
window.addEventListener('blur',peekCheck);document.addEventListener('scroll',function(){if(peek.classList.contains('on')){clearTimeout(peekT);peekT=setTimeout(peekCheck,150)}},{passive:true,capture:true});
function peekHTML(c){var n=c[2]||0;if(!n&&!(c[8]>=0))return'<b class="mut">empty cell</b>';var nm=typeof c[8]==='string'?c[8]:(kgs[c[8]]||''),i=nm.indexOf(' \u00b7 '),kg=i>0?nm.slice(0,i):nm,gm=i>0?nm.slice(i+3):'';
 var x=[];if(c[6])x.push('<span class="o">\u25cf observed</span>');if(c[7])x.push('<span class="c">\u25cf '+fmt(c[7])+' changed</span>');else if(c[10])x.push('<span class="c ca">\u25cf '+fmt(c[10])+' candidate</span>');
 return'<b>'+kg+'</b>'+(gm?'<span class="mut"> \u00b7 '+gm+'</span>':'')+'<div class="pl">'+decLine(n,c[9])+'<span class="sep"></span><span class="mut">'+ago(c[5])+'</span></div>'+(x.length?'<div class="pl">'+x.join('<span class="sep"></span>')+'</div>':'')+(selected()||coarse?'':'<div class="pl mut">click for the card</div>')}
function onHover(){mvR=0;var e=mv;if(!e||!hover||drag||pinch)return;var p=at(e);
 if(hi.on){if(hi.tree){var ah=apAt(e);if(ah){peekAt(e,'ap'+ah.a.lon+ah.a.lat+(ah.a.fl?'f':''),apPeek(ah.a),null);return}}hiAt(p,function(d){if(!hi.on||!hover)return;if(!d||d.off||(hi.pin&&hi.pin.cell===d.cell)){unpeek();return}peekAt(mv,'h3'+d.cell+view+(cg.on?'c':'')+(hi.tree?'t':'')+(subVis()?'b':'')+(selected()?'s':''),hiPeekHTML(d),polyD(d.poly))});return}
 if(!data)return;var i=near(p[0],p[1]);
 if(i<0){unpeek();return}var c=cells[i];peekAt(e,'c'+i+(selected()?'s':''),peekHTML(c),!hex?circD(c[0],c[1],22):W/vb.w>HEX0?faceD(i):hexD(c[0],c[1],1.25))}
function up(e){if(!ptrs[e.pointerId])return;var cancelled=e.type!=='pointerup';if(navR){if(cancelled){cancelAnimationFrame(navR);navR=0}else navFlush()}var wd=drag,wasPinch=!!pinch;navDrop(String(e.pointerId));main.classList.remove('drag');
 if(!keys().length)navCommit();navBase(true);
 if(cancelled){tapT=0;tapP=null;if(wd&&wd.touch&&!wd.moved&&coop()&&e.type==='pointercancel')coopHint();return}if(wasPinch||keys().length||!wd||wd.moved)return;
 var now=performance.now(),p=at(e);
 if(hi.on&&hi.tree){var ah=apAt(e);if(ah){tapT=0;tapP=null;apPin(ah);if(e.pointerType==='mouse')main.focus({preventScroll:true});return}}
 if(tapP&&now-tapT<320&&Math.abs(tapP[0]-e.clientX)<24&&Math.abs(tapP[1]-e.clientY)<24){tapT=0;tapP=null;if(W/vb.w>ZMAX*0.7&&(hi.on||auto.opt||!proj))reset();else zoomAt(p[0],p[1],2.6,280);return}/* near the overview's deepest zoom a double-tap keeps going: zoomAt lands on ZMAX0 and hiAuto flows into full resolution, like the pinch — it only resets when the cap is final (full res on, overview opted, no projection) */
 tapT=now;tapP=[e.clientX,e.clientY];pinAt(p);if(e.pointerType==='mouse')main.focus({preventScroll:true})}
/* Window capture also catches releases over controls/outside the map if a
   browser drops capture. Cancel/lost capture never select a cell or double-tap. */
window.addEventListener('pointerup',up,true);window.addEventListener('pointercancel',up,true);main.addEventListener('lostpointercapture',up);
window.addEventListener('blur',navCancel);window.addEventListener('pagehide',navCancel);
/* resize mid-gesture (the Android URL bar folding away, a rotation, the keyboard) re-anchors the contacts to the new box instead of killing the gesture */
function navRebase(){if(!order.length)return;navCommit();navBase(true)}window.addEventListener('resize',navRebase);
document.addEventListener('visibilitychange',function(){if(document.hidden)navCancel()});
fig.addEventListener('pointerdown',function(e){if(!main.contains(e.target))navCancel()},true);
main.addEventListener('pointerenter',function(e){if(e.pointerType==='mouse'){get();hover=true;pause(0)}});
main.addEventListener('pointerleave',function(e){if(e.pointerType!=='mouse')return;if(e.relatedTarget&&tip.contains(e.relatedTarget)){unpeek();hovRing(null);return}leaveMain()});
tip.addEventListener('pointerleave',function(e){if(e.pointerType!=='mouse'||(e.relatedTarget&&main.contains(e.relatedTarget)))return;hover=false;hide();if(pinned<0&&!sel)pause(4500)});
/* wheel: coalesced per frame. On slow machines wheel events arrive in bursts long after the hand moved; applying each one (a full setVB) made the backlog keep zooming after the user stopped, and a corrective scroll the other way was played *after* the stale events — which read as the zoom going the wrong way. So: sum the deltas of one frame (opposite directions net out), zoom once from the latest cursor position, and drop events older than 250 ms outright */
var whl={d:0,e:null,r:0};
function wheelFlush(){whl.r=0;var d=whl.d,e=whl.e;whl.d=0;whl.e=null;if(!d||!e)return;pause(0);var p=at(e);zoomAt(p[0],p[1],Math.exp(-d*0.012),0)}
main.addEventListener('wheel',function(e){var z=W/vb.w;if(!(e.ctrlKey||e.metaKey)&&z<=1.01&&document.activeElement!==main){showHint('ctrl + scroll · double-click to zoom');return}
 e.preventDefault();if(e.timeStamp<1e12&&performance.now()-e.timeStamp>250)return;
 if(keys().length)navCancel();stopAnim();clearTimeout(dismissT);
 var d=e.deltaY*(e.deltaMode===1?16:e.deltaMode===2?400:1);whl.d=Math.max(-90,Math.min(90,whl.d+Math.max(-60,Math.min(60,d))));whl.e=e;
 if(!whl.r)whl.r=requestAnimationFrame(wheelFlush)},{passive:false});
main.addEventListener('dblclick',function(e){e.preventDefault()});
/* ── keyboard ── */
main.addEventListener('keydown',function(e){navCancel();clearTimeout(dismissT);var k=e.key,d={ArrowLeft:[-1,0],ArrowRight:[1,0],ArrowUp:[0,-1],ArrowDown:[0,1]}[k];
 if(k==='Escape'){e.preventDefault();if(hi.on&&(hi.pin||hi.apx)){hiUnpin();pause(0)}else if(hi.on){hiLeave(true)}else if(pinned>=0){pinned=-1;hide();setHash();pause(6000)}else if(sel)clearSel();else if(W/vb.w>1.01)reset();else if(fsOn&&!fsNative)fsToggle();else reset();return}
 if(k==='<'||k==='>'||k===','||k==='.'){e.preventDefault();if(!cg.on)setChg(true);tlStep(k==='>'||k==='.'?1:-1);return}if(k===' '&&cg.on&&tl.d){e.preventDefault();tlPlay();return}if(k==='v'||k==='V'){e.preventDefault();setView(view==='obs'?'':'obs');return}if(k==='c'||k==='C'){e.preventDefault();setChg(!cg.on);return}if(k==='d'||k==='D'){e.preventDefault();if(avail().diff||df.on)setDif(!df.on);else availHint('diff');return}if(k==='s'||k==='S'){e.preventDefault();setSub(!sb.on);return}if(k==='t'||k==='T'){e.preventDefault();if(avail().trees)setTree(!hi.tree);else availHint('trees');return}if(k==='r'||k==='R'){e.preventDefault();if(avail().relief||rl.on)setRelief(!rl.on);else availHint('relief');return}if(k==='o'||k==='O'){e.preventDefault();if(avail().ortho||ot.on)setOrtho(!ot.on);else availHint('ortho');return}if(k==='w'||k==='W'){e.preventDefault();if(avail().roads||rd.on)setRoads(!rd.on);else availHint('roads');return}if(k==='+'||k==='='){e.preventDefault();zoomCenter(1.6);return}if(k==='-'||k==='_'){e.preventDefault();zoomCenter(1/1.6);return}if(k==='0'){e.preventDefault();reset();return}
 if(!d)return;e.preventDefault();pause(0);if(hi.on){zoomTo(vb.x+d[0]*vb.w*0.18,vb.y+d[1]*vb.h*0.18,vb.w,160);return}var go=function(){var i=pinned>=0?pinned:shown,best=-1,bd=1e12,cx=vb.x+vb.w/2,cy=vb.y+vb.h/2;
  if(i<0){best=near(cx,cy);if(best<0)for(var j=0;j<cells.length;j++){var c=cells[j],dd=(c[0]-cx)*(c[0]-cx)+(c[1]-cy)*(c[1]-cy);if(dd<bd){bd=dd;best=j}}}
  else{var o=cells[i];for(var j=0;j<cells.length;j++){var c=cells[j],dx=c[0]-o[0],dy=c[1]-o[1],al=dx*d[0]+dy*d[1];if(al<=0)continue;var dd=dx*dx+dy*dy+(dx*d[1]-dy*d[0])*(dx*d[1]-dy*d[0])*4;if(dd<bd){bd=dd;best=j}}}
  if(best>=0){pinned=best;show(best);veilSync();var c=cells[best],m=vb.w*0.06;if(c[0]<vb.x+m||c[0]>vb.x+vb.w-m||c[1]<vb.y+m||c[1]>vb.y+vb.h-m)zoomTo(c[0]-vb.w/2,c[1]-vb.h/2,vb.w,220)}};data?go():get().then(go)});
main.addEventListener('blur',function(){if(pinned<0)hide()});
window.addEventListener('resize',function(){if(picked)place(picked)});
document.addEventListener('click',function(e){if(hi.on&&(hi.pin||hi.apx)&&!fig.contains(e.target)){hiUnpin();return}if(pinned>=0&&!fig.contains(e.target)){pinned=-1;hide();setHash();pause(6000)}});
/* ── teaser: glides through a few of the inlined picks once, so a first-time visitor sees the dots answer; yields to any touch and never comes back once done ── */
var tour={on:picks.length>1,run:false,i:-1,left:6,t:0,idle:0,vis:true},glide=0;
function pause(resumeMs){clearTimeout(tour.t);clearTimeout(tour.idle);cancelAnimationFrame(glide);if(tour.run){tour.run=false;if(pinned<0&&!hover)hide()}if(resumeMs&&tour.on)tour.idle=setTimeout(resume,resumeMs)}
function resume(){if(!tour.on||tour.run||!tour.vis||document.hidden||pinned>=0||sel||asel||pt||hover||W/vb.w>1.01||tl.play)return;tour.run=true;step()}
function step(){if(!tour.run)return;if(tour.left--<=0){tour.on=false;tour.run=false;svg.classList.remove('on');return}tour.i=(tour.i+1)%picks.length;var c=picks[tour.i],x0=curAt?curAt[0]:c[0],y0=curAt?curAt[1]:c[1],on=svg.classList.contains('on');
 var dist=Math.hypot(c[0]-x0,c[1]-y0),ms=(!on||rm)?0:Math.min(1100,350+dist*0.14),t0=performance.now();if(floating()&&ms>500)tip.classList.remove('on');
 svg.classList.add('on');
 (function fr(now){if(!tour.run)return;var k=ms?Math.min(1,(now-t0)/ms):1;k=k<.5?4*k*k*k:1-Math.pow(-2*k+2,3)/2;
  drawCur(x0+(c[0]-x0)*k,y0+(c[1]-y0)*k);
  if(k<1){glide=requestAnimationFrame(fr);return}
  ping.setAttribute('cx',c[0]);ping.setAttribute('cy',c[1]);ping.classList.remove('go');void ping.getBoundingClientRect();ping.classList.add('go');
  shown=-1;picked=c;pk={t:'pick',c:c};reveal(c,card(c),true);tour.t=setTimeout(step,rm?5200:3800)})(t0)}
if('IntersectionObserver'in window){new IntersectionObserver(function(es){tour.vis=es[0].isIntersecting;if(tour.vis)pause(600);else pause(0)},{threshold:.4}).observe(main)}
document.addEventListener('visibilitychange',function(){if(document.hidden)pause(0);else pause(800)});
/* ── the tour comes back: when the card has sat blank at the overview for a while with no hand on the page (nothing pinned, no KG, no full res, no find box, tab visible), the teaser runs again from where it stopped; any touch re-arms the clock. A hash view (#v=obs), a dismissed card or a finished round only stop it for now, not for good ── */
var IDLE_MS=rm?45000:30000,idleT=0;
function blank(){return !tour.run&&pinned<0&&!sel&&!asel&&!pt&&!hover&&!hi.on&&!hi.fly&&!animating&&(!load||data)&&W/vb.w<=1.01&&tour.vis&&!document.hidden&&!tools.classList.contains('open')&&(!tip.classList.contains('x')||tip.classList.contains('tour'))}
function idleArm(){clearTimeout(idleT);idleT=setTimeout(idleTick,IDLE_MS)}
function idleTick(){if(picks.length<2)return;if(blank()){tour.on=true;tour.left=Math.min(picks.length,6);resume();if(!tour.run)tour.on=false}idleArm()}
['pointerdown','pointermove','keydown','wheel','touchstart','scroll'].forEach(function(t){document.addEventListener(t,idleArm,{passive:true,capture:true})});idleArm();
/* ── find · recent: a KG by name or code flies the view to it and lights all its cells; a tap lights one cell; both land in a small history (localStorage) and in the URL hash ── */
var tools=fig.querySelector('.glyph-tools'),tX=fig.querySelector('.gt-x'),tIn=tools.querySelector('.gt-in'),tList=tools.querySelector('.gt-l'),tQ=tools.querySelector('.gt-q'),tH=tools.querySelector('.gt-h'),tK=tools.querySelector('.gt-k');
/* link: the URL of what is on screen (#kg=… or #cell=…) to the clipboard — the same state applyHash restores on load */
var kT=0;
/* the whole state of the figure in one string — documented for agents in llm.txt §7 (keep both in step):
   #kg=63349 · #cell=x,y · #h3=<res-12 id> · #q=<name> — then ;v=obs (profile; declared when absent) · ;change (overlay) · ;full (full resolution on) · ;nofull (the reader chose the overview at the deepest zoom: no automatic full resolution) · ;trees (crown rings, any profile) · ;relief (hillshade blended under full res) · ;ortho[=<opacity 0.1–1>] (basemap.at orthophoto under the near tiles, default .5) · ;screen (full screen) · ;z=<zoom> (read only: zoom about the selection) · ;at=x,y,z (centre + zoom once zoomed) · ;open (card expanded) · ;below (card docked below the map) · ;card=fx,fy (card dragged, in map-box fractions) */
function curHash(){var p=[],z=W/vb.w;if(hi.on&&hi.pin)p.push('h3='+hi.pin.cell);else if(pt)p.push('pt='+pt.lon.toFixed(5)+','+pt.lat.toFixed(5)+(pt.n?';pn='+encodeURIComponent(pt.n+(pt.sub?' | '+pt.sub:'')):''));else if(sel)p.push('kg='+sel.code);else if(asel)p.push('adm='+asel.c);else if(pinned>=0&&cells[pinned])p.push('cell='+cells[pinned][0]+','+cells[pinned][1]);
 var selN=p.length;if(view)p.push('v='+view);if(cg.on)p.push('change');if(df.on)p.push(df.raw?'diff=raw':'diff');if(hi.on)p.push('full');else if(auto.opt)p.push('nofull');if(hi.tree)p.push('trees');if(rl.on)p.push('relief');if(ot.on)p.push(ot.op===OT_OP?'ortho':'ortho='+ot.op);if(rd.on)p.push(rd.op===RD_OP?'roads':'roads='+rd.op);if(sb.on)p.push('subsidy');if(typeof flt!=='undefined'&&flt.f)p.push('f='+fltHashEnc(flt.f));if(tl.win)p.push('t='+tlStr());if(fsOn)p.push('screen');if(z>1.01&&!tour.run)p.push('at='+Math.round(vb.x+vb.w/2)+','+Math.round(vb.y+vb.h/2)+','+(z<10?z.toFixed(1):Math.round(z)));
 if(selN&&tip.classList.contains('open'))p.push('open');if(typeof tXM!=='undefined'&&tXM&&!tXM.hidden)p.push('export');if(selN&&tip.classList.contains('dock')&&!mq.matches)p.push('below');
 if(selN&&moved&&floating()){var fr=fig.getBoundingClientRect();if(fr.width&&fr.height)p.push('card='+(moved[0]/fr.width).toFixed(3)+','+(moved[1]/fr.height).toFixed(3))}return p.length?'#'+p.join(';'):''}
function copyLink(){var u=location.origin+location.pathname+curHash(),done=function(ok){tK.classList.toggle('done',ok);clearTimeout(kT);kT=setTimeout(function(){tK.classList.remove('done')},1400);showHint(ok?(curHash()?'link copied':'link to the map copied \u2014 pick a KG or cell to share one'):u)};
 if(navigator.clipboard&&navigator.clipboard.writeText)navigator.clipboard.writeText(u).then(function(){done(true)},function(){done(fallback(u))});else done(fallback(u))}
function fallback(u){var i=document.createElement('textarea');i.value=u;i.setAttribute('readonly','');i.style.cssText='position:fixed;opacity:0';document.body.appendChild(i);i.select();var ok=false;try{ok=document.execCommand('copy')}catch(e){}i.remove();return ok}
tK.addEventListener('click',function(){closeTools();copyLink()});
/* full screen: native API when the element can take it (desktop, Android), else a fixed box (iPhone); the svg is sized by hand so its box always matches its viewBox (the pointer math divides by it) */
var tF=tools.querySelector('.gt-f'),fsOn=false,fsNative=false,fsRO=null,W0=+fig.dataset.w,H0=+fig.dataset.h;
function fsFit(){var hk0=H0/W0;if(!fsOn){svg.style.width='';svg.style.height='';['--fs-x','--fs-t','--fs-w','--fs-b'].forEach(function(k){fig.style.removeProperty(k)});YF=.5;if(AR!==hk0){var cx0=vb.x+vb.w/2,cy0=vb.y+vb.h/2;AR=hk0;setVB(cx0-vb.w/2,cy0-vb.w*AR/2,vb.w)}toolsFit();return}var mw=main.clientWidth,mh=main.clientHeight;if(!mw||!mh){toolsFit();return}
 var hk=mh/mw;svg.style.width=mw+'px';svg.style.height=mh+'px';YF=hk>hk0?.42:.5;if(Math.abs(hk-AR)>1e-6){var cx=vb.x+vb.w/2,cy=vb.y+vb.h/2;AR=hk;setVB(cx-vb.w/2,cy-vb.w*AR/2,vb.w)}
 var sr=svg.getBoundingClientRect(),fr=fig.getBoundingClientRect();fig.style.setProperty('--fs-x',Math.round(sr.left-fr.left)+'px');fig.style.setProperty('--fs-t',Math.round(sr.top-fr.top)+'px');fig.style.setProperty('--fs-w',Math.round(sr.width)+'px');fig.style.setProperty('--fs-b',Math.round(fr.bottom-sr.bottom)+'px');if(picked)place(picked);toolsFit()}
/* the bar drops its labels (icons stay) only when they really do not fit the figure's width — never while the find box is open (it is wide by design) */
function toolsFit(){if(tools.classList.contains('open'))return;tools.classList.remove('tight');var side=tools.parentNode;if(tools.scrollWidth>tools.clientWidth+1||tools.getBoundingClientRect().right>side.getBoundingClientRect().right+1)tools.classList.add('tight')}
/* a soft keyboard in full screen: the figure is position:fixed, so on iOS (and on Chrome without interactive-widget=resizes-content) it keeps the whole screen and the bar at its bottom disappears behind the keyboard — follow the visual viewport instead while it is markedly shorter than the window */
function fsVV(){var vv=window.visualViewport;if(!vv)return;var kb=fsOn&&window.innerHeight-vv.height>120;fig.style.top=kb?Math.round(vv.offsetTop)+'px':'';fig.style.height=kb?Math.round(vv.height)+'px':'';fig.style.bottom=kb?'auto':'';if(kb!==!!fsKb){fsKb=kb;fig.classList.toggle('kb',kb);fsFit()}}
var fsKb=false;if(window.visualViewport){window.visualViewport.addEventListener('resize',fsVV);window.visualViewport.addEventListener('scroll',fsVV)}
function fsApply(on){if(on===fsOn)return;navCancel();fsOn=on;fig.classList.toggle('fs',on);if(!on){fig.style.top='';fig.style.height='';fig.style.bottom='';fsKb=false;fig.classList.remove('kb')}document.documentElement.classList.toggle('glyph-fs',on);tF.setAttribute('aria-pressed',on?'true':'false');tF.title=on?'leave full screen ( esc )':'full screen ( f )';setHash();
 if(on){if(window.ResizeObserver&&!fsRO){fsRO=new ResizeObserver(function(){fsFit()});fsRO.observe(main)}fsFit();requestAnimationFrame(fsFit)}else{if(fsRO){fsRO.disconnect();fsRO=null}fsFit();requestAnimationFrame(function(){var r=fig.getBoundingClientRect(),vh=window.innerHeight||0;if(vh&&(r.top<0||r.bottom>vh))try{fig.scrollIntoView({block:'start',behavior:rm?'auto':'smooth'})}catch(e){fig.scrollIntoView()}})}}
/* leaving full screen: entering pushes one history entry, so the phone's back gesture (and the browser's own exit from native full screen) lands back on the page instead of leaving it; every exit path (×, bar button, Esc, back, native exit) ends in fsLeave, which pops that entry without replaying the old hash (the view the visitor zoomed to in full screen stays) */
var fsPushed=false,fsPopping=false;
function fsLeave(){if(fsNative&&document.fullscreenElement===fig&&document.exitFullscreen)document.exitFullscreen().catch(function(){});fsNative=false;fsApply(false);if(fsPushed){fsPushed=false;fsPopping=true;try{history.back()}catch(e){fsPopping=false}}}
function fsToggle(){closeTools();if(fsOn){fsLeave();return}
 if(history.pushState){try{history.pushState({glyphFs:1},'',location.pathname+location.search+location.hash);fsPushed=true}catch(e){fsPushed=false}}
 var rf=fig.requestFullscreen||fig.webkitRequestFullscreen;if(rf){fsNative=true;try{var p=rf.call(fig,{navigationUI:'hide'});if(p&&p.then)p.catch(function(){fsNative=false});}catch(e){fsNative=false}}fsApply(true)}
tF.addEventListener('click',fsToggle);
var tFX=fig.querySelector('.glyph-fsx');tFX.hidden=false;tFX.addEventListener('click',function(e){e.stopPropagation();if(fsOn)fsLeave()});
['pointerdown','pointerup','pointermove','pointercancel','wheel','dblclick','touchstart','touchmove','touchend','keydown'].forEach(function(t){tFX.addEventListener(t,function(e){e.stopPropagation()},{passive:t==='wheel'||t==='touchstart'||t==='touchmove'||t==='touchend'})});
window.addEventListener('popstate',function(){var own=fsPopping;fsPopping=false;if(!fsOn&&!own)return;fsPushed=false;if(fsOn){if(fsNative&&document.fullscreenElement===fig&&document.exitFullscreen)document.exitFullscreen().catch(function(){});fsNative=false;fsApply(false)}if(history.replaceState){var h=curHash();if(location.hash!==h)history.replaceState(null,'',location.pathname+location.search+h)}});
document.addEventListener('fullscreenchange',function(){if(fsNative&&!document.fullscreenElement){fsNative=false;if(fsOn)fsLeave()}});
window.addEventListener('resize',fsFit);
var selP=null,selO=null,selM=null,sel=null,ix=null,ixLoad=null,ixBy={},agg=null,act=-1,mode='';
['pointerdown','pointerup','pointermove','pointercancel','wheel','dblclick','click','keydown','keyup','touchstart','touchmove'].forEach(function(t){tools.addEventListener(t,function(e){e.stopPropagation()},{passive:t==='wheel'||t==='touchstart'||t==='touchmove'})});
function norm(s){return String(s||'').toLowerCase().normalize('NFD').replace(/[\u0300-\u036f]/g,'').replace(/ß/g,'ss').replace(/\bst\.\s*/g,'sankt ').replace(/\bst\s+/g,'sankt ').replace(/[^a-z0-9 ]+/g,' ').replace(/\s+/g,' ').trim()}
function ixGet(){if(ixLoad)return ixLoad;ixLoad=fetch('/api/v1/ne/glyph/kgs',{credentials:'omit'}).then(jsonOf).then(function(d){ix=d.kgs;ix.forEach(function(e){e.n=norm(e[1]);e.g=norm(e[2]);e.d=norm(e[3]);e.w=(e.n+' '+e.g+' '+e.d+' '+e[0]).split(' ');ixBy[e[0]]=e});return ix}).catch(function(){ixLoad=null});return ixLoad}
/* per-KG roll-up of the cells (dominant KG per cell), once, after the big JSON is in */
/* one accumulator for every area: a KG (kgAgg), an admin unit, a Natura 2000 site, later any filtered cell set — aggNew() + aggAdd(a, i) per res-7 cell; a.idx is the cell set */
function aggNew(){return{idx:[],chunks:0,passes:0,reports:0,age:1e9,obs:0,chg:0,cand:0,bands:[0,0,0,0,0]}}
function aggAdd(a,i){var c=cells[i];a.idx.push(i);if(!c[2])return;a.chunks+=c[2];a.passes+=c[3];a.reports+=c[4];if(c[5]<a.age)a.age=c[5];if(c[6])a.obs=1;a.chg+=c[7]||0;a.cand+=c[10]||0;var b=c[9]||[];for(var j=0;j<b.length;j++)a.bands[j]+=b[j]}
function aggOf(idx){var a=aggNew();for(var i=0;i<idx.length;i++)aggAdd(a,idx[i]);if(!a.chunks)a.age=-1;return a}
function kgAgg(){if(agg||!data)return agg;agg={};var codes=data.kg_codes||[];for(var i=0;i<cells.length;i++){var c=cells[i];if(!c[2]||c[8]==null)continue;var k=codes[c[8]];if(!k)continue;aggAdd(agg[k]||(agg[k]=aggNew()),i)}return agg}
/* the KG codes of a cell set (dominant KG per res-7 cell) */
function kgsOf(idx){var o={},r=[],codes=data&&data.kg_codes||[];for(var i=0;i<idx.length;i++){var c=cells[idx[i]];if(c[8]==null)continue;var k=codes[c[8]];if(k&&!o[k]){o[k]=1;r.push(k)}}return r}
/* rings of an outline path ("M x y L x y … Z", frame units) → evenodd point-in-polygon; parsed once per path */
function ringsOf(d){var rs=[],m,re=/([MLZ])([^MLZ]*)/g,cur=null;while((m=re.exec(d))){if(m[1]==='Z'){if(cur&&cur.length>2)rs.push(cur);cur=null;continue}var v=m[2].trim().split(/[\s,]+/).map(Number);if(m[1]==='M')cur=[[v[0],v[1]]];else if(cur)cur.push([v[0],v[1]])}if(cur&&cur.length>2)rs.push(cur);return rs}
function inRings(rs,x,y){var inside=false;for(var r=0;r<rs.length;r++){var p=rs[r];for(var i=0,j=p.length-1;i<p.length;j=i++){var a=p[i],b=p[j];if((a[1]>y)!==(b[1]>y)&&x<(b[0]-a[0])*(y-a[1])/(b[1]-a[1])+a[0])inside=!inside}}return inside}
function cellsInPath(o){if(!o.rings)o.rings=ringsOf(o.d);var bb=o.bbox,out=[],i,c;for(i=0;i<cells.length;i++){c=cells[i];if(c[0]<bb[0]||c[0]>bb[2]||c[1]<bb[1]||c[1]>bb[3])continue;if(inRings(o.rings,c[0],c[1]))out.push(i)}return out}
function cellsInBox(e){var out=[],i,c;for(i=0;i<cells.length;i++){c=cells[i];if(c[0]>=e[4]&&c[0]<=e[6]&&c[1]>=e[5]&&c[1]<=e[7])out.push(i)}
 if(!out.length){var cx=(e[4]+e[6])/2,cy=(e[5]+e[7])/2,best=-1,bd=1e18;for(i=0;i<cells.length;i++){c=cells[i];var d=(c[0]-cx)*(c[0]-cx)+(c[1]-cy)*(c[1]-cy);if(d<bd){bd=d;best=i}}if(best>=0)out.push(best)}return out}
/* selection = faces filled + outline of the unshared edges (so the KG reads as one shape, not a pile of hexagons) */
/* full-res veil: everything outside the selected KG / pinned res-7 cell is whitened a little; the selection itself stays as it is (no outline at this level).
   For a KG the mask is its true outline at res 12 (/ne/glyph/kg, union of its cells, holes included → evenodd), fetched once per KG; the hex faces stand in until it is there */
var veilR=null,veilM=null,selD='',kgOut={};function kgOutline(code){var o=kgOut[code];if(o!==undefined)return o&&o.d?o:null;kgOut[code]=null;fetch('/api/v1/ne/glyph/kg?kg='+code,{credentials:'omit'}).then(function(r){if(r.status>=500)throw 0;return r.ok?r.json():null}).then(function(d){kgOut[code]=d&&d.d?d:false;if(sel&&sel.code===code)veilSync()}).catch(function(){delete kgOut[code]});return null}
function kgOutlineP(code){var o=kgOutline(code);if(o)return Promise.resolve(o);return new Promise(function(ok){var n=0,t=setInterval(function(){var x=kgOut[code];if(x!==null&&x!==undefined||++n>40){clearInterval(t);ok(x&&x.d?x:null)}},50)})}
function veilSync(){var d='',eo=false;if(sel){var o=kgOutline(sel.code);if(o){d=o.d;eo=true}else d=selD}else if(asel){d=asel.d;eo=true}else if(hi.on&&pinned>=0&&data)d=faceD(pinned);svg.classList.toggle('veiled',!!d&&!!(sel||asel));if(!d){if(veilR)veilR.classList.remove('on');return}
 if(!veilR){cur.insertAdjacentHTML('beforebegin','<mask id="veilm" maskUnits="userSpaceOnUse" x="0" y="0" width="'+W+'" height="'+H+'"><rect width="'+W+'" height="'+H+'" fill="#fff"/><path class="veilmp" d="" fill="#000"/></mask><rect class="veil" width="'+W+'" height="'+H+'" mask="url(#veilm)"/>');veilR=svg.querySelector('.veil');veilM=svg.querySelector('.veilmp')}
 if(veilM.getAttribute('d')!==d)veilM.setAttribute('d',d);veilM.setAttribute('fill-rule',eo?'evenodd':'nonzero');veilR.classList.add('on')}
function drawSel(idx){if(!selP){svg.insertAdjacentHTML('beforeend','<mask id="selm" maskUnits="userSpaceOnUse" x="0" y="0" width="'+W+'" height="'+H+'"><rect width="'+W+'" height="'+H+'" fill="#fff"/><path class="selmp" d="" fill="#000"/></mask><path class="sel" d=""/><path class="selo" d="" mask="url(#selm)"/>');selP=svg.querySelector('.sel');selO=svg.querySelector('.selo');selM=svg.querySelector('.selmp')}
 /* outline = the union's own stroke, masked by the union's area: only the outer half survives, every shared edge is covered — no vertex matching, so no hairline artefacts */
 var fill='',i,j,p,x0=1e9,y0=1e9,x1=-1e9,y1=-1e9;
 for(i=0;i<idx.length;i++){fill+=faceD(idx[i]);p=cellP[idx[i]];if(!p)continue;for(j=0;j<p.length;j++){var a=p[j];x0=Math.min(x0,a[0]);y0=Math.min(y0,a[1]);x1=Math.max(x1,a[0]);y1=Math.max(y1,a[1])}}
 selP.setAttribute('fill-rule','nonzero');selM.setAttribute('fill-rule','nonzero');selP.setAttribute('d',fill);selO.setAttribute('d',fill);selM.setAttribute('d',fill);selP.classList.add('on');selO.classList.add('on');selD=fill;return[x0,y0,x1,y1]}
/* an admin outline comes as one path with holes (evenodd): same fill + masked outer stroke as a KG */
function drawSelD(d){if(!selP)drawSel([]);selP.setAttribute('fill-rule','evenodd');selM.setAttribute('fill-rule','evenodd');selP.setAttribute('d',d);selO.setAttribute('d',d);selM.setAttribute('d',d);selP.classList.add('on');selO.classList.add('on');selD=d}
/* sheetY: in portrait full screen the docked card is a bottom sheet, so a target is centred in the room above it (a little above the box centre) */
function sheet(){return fsOn?getComputedStyle(tip).getPropertyValue('--sheet').trim():''}function sheetY(){return sheet()==='b'?0.13:0}function sheetX(){return sheet()==='l'?-0.2:0}
function fitBox(b,ms,side){var bw=b[2]-b[0],bh=b[3]-b[1],fl=side&&floating(),w=Math.max(bw,bh/AR)/(fl?0.4:0.55),cx=(b[0]+b[2])/2,cy=(b[1]+b[3])/2;w=Math.max(W/ZMAX,Math.min(W,w));if(fl)cx+=w*0.17;cx+=w*sheetX();cy+=w*AR*sheetY();zoomTo(cx-w/2,cy-w*AR/2,w,ms)}
/* the card row of an area: the glyph row shape [x,y,chunks,passes,reports,age,obs,chg,name,bands,cand] over its cells, plus .k (cache key), .kg (one KG) or .filt/.kgs (a KG set: srtm's set summary, /ne/stats filter), .bb (lon/lat box) */
function areaRow(s){var a=s.a,ctr=s.c2||s.c,r=[ctr[0],ctr[1],a?a.chunks:0,a?a.passes:0,a?a.reports:0,a?a.age:-1,a?a.obs:0,a?a.chg:0,s.name||(s.e&&s.e[1])||'',a?a.bands:[],a?a.cand:0];r.rad=s.r;r.k=(s.code?'kg_'+s.code:'adm_'+s.c);if(s.code)r.kg=s.code;else{r.filt=s.filt||'';r.kgs=s.kgs||[];r.kgn=s.kgn||r.kgs.length}
 if(proj&&s.b){var p=lonlat(s.b[0],s.b[3]),q=lonlat(s.b[2],s.b[1]);r.bb=[p[0],p[1],q[0],q[1]]}return r}
function selRow(){return areaRow(sel)}
function areaCard(){return sel?kgCard():asel?admCard():''}
/* the admin / site card: the same three sources as a KG over every cell inside; the sub-line names the kind, the parent and the KG count */
function admCard(){var s=asel,a=s.a&&s.a.chunks?s.a:null,n=s.idx.length,u=admBy[s.c],sub=[admKind(u?u.k:s.kind)],p=u?admSub(u):'';if(p)sub.push(p);if(s.kgn)sub.push(fmt(s.kgn)+' KG'+(s.kgn==1?'':'s'));
 var h='<div class="hd"><b>'+esc(s.n)+'</b>'+(a?'<span class="age'+(a.age<fresh?' n':'')+'">'+(a.age<fresh?'\u25cf ':'')+ago(a.age)+'</span>':'')+'</div><div class="sub">'+esc(sub.join(' \u00b7 '))+'</div>';
 var dec='<div class="rw"><span class="lb">declared</span><span class="vl">'+decLine(a?a.chunks:0,a&&a.bands)+'</span></div>';
 dec+='<div class="dt"><div class="sh"><span>declared <i>survey land use \u00b7 BEV tiles via bevdirect</i></span></div>';
 if(a){dec+='<div class="nt kv"><b>'+km2(a.chunks)+'</b> digested<span class="sep"></span>'+fmt(a.chunks)+' chunks<span class="sep"></span>'+fmt(a.reports)+' report'+(a.reports==1?'':'s')+'<span class="sep"></span>'+(a.passes/a.chunks).toFixed(1)+'\u00d7 per chunk</div>'+bands(s.row)}
 else dec+='<div class="nt mut">not digested yet</div>';
 dec+='<div class="nt mut">'+fmt(n)+' cell'+(n==1?'':'s')+' lit<span class="sep"></span>'+(s.kgs&&!s.filt?'cells by their centre':'cells by their KG')+'</div></div>';
 h+=srcs(dec,obsHTML(s.row),chgHTML(s.row));if(subVis())h+=subAreaLine(s.idx);obsFetch(s.row);chgFetch(s.row);return h+hiLegendHTML()}
function kgCard(){var s=sel,e=s.e,a=s.a,n=s.idx.length;
 var h='<div class="hd"><b>'+e[1]+'</b>'+(a?'<span class="age'+(a.age<fresh?' n':'')+'">'+(a.age<fresh?'\u25cf ':'')+ago(a.age)+'</span>':'')+'</div><div class="sub">'+esc(placeOf(e,e[0]))+'</div>';
 var dec='<div class="rw"><span class="lb">declared</span><span class="vl">'+decLine(a?a.chunks:0,a&&a.bands)+'</span></div>';
 dec+='<div class="dt"><div class="sh"><span>declared <i>survey land use \u00b7 BEV tiles via bevdirect</i></span></div>';
 if(a){dec+='<div class="nt kv"><b>'+km2(a.chunks)+'</b> digested<span class="sep"></span>'+fmt(a.chunks)+' chunks<span class="sep"></span>'+fmt(a.reports)+' report'+(a.reports==1?'':'s')+'<span class="sep"></span>'+(a.passes/a.chunks).toFixed(1)+'\u00d7 per chunk</div>'+bands(selRow())}
 else dec+='<div class="nt mut">not digested yet</div>';
 dec+='<div class="nt mut">'+n+' cell'+(n==1?'':'s')+' lit<span class="sep"></span>'+(coarse?'tap one for detail':'hover a cell to peek \u00b7 click to pin \u00b7 esc clears')+'</div></div>';
 h+=srcs(dec,obsHTML(s.row),chgHTML(s.row));if(subVis())h+=subAreaLine(s.idx);obsFetch(s.row);chgFetch(s.row);return h+hiLegendHTML()}
function showSel(){var s=sel||asel;if(!s)return;var r=s.row;tip.classList.remove('follow');svg.classList.remove('on');reveal(r,picked===r?null:areaCard(),false);shown=-1;picked=r;pk={t:sel?'kg':'adm'}}
/* keep: the KG is selected around a found point (goPoint) — the dot stays, the history entry and the hash are the point's */
var refits=0;function selectKG(code,fly,keep){collapse();var navAt=userNav,fitN=selectKG.n=(selectKG.n||0)+1;return Promise.all([get(),ixGet()]).then(function(){var e=ixBy[code];if(!e||!data){closeTools();return}var a=kgAgg()[code],idx=a?a.idx:cellsInBox(e);if(!idx.length)return;
 pinned=-1;pause(0);tour.on=false;if(!keep)ptClear();asel=null;var b=drawSel(idx);sel={code:code,e:e,a:a,idx:idx,b:b,c:[(b[0]+b[2])/2,(b[1]+b[3])/2],r:Math.max(b[2]-b[0],b[3]-b[1])/2};sel.row=selRow();
 if(fly!==false){if(typeof HI_ZMAX==='number'&&!hi.on&&!auto.opt&&W/(Math.max(b[2]-b[0],(b[3]-b[1])/AR)/0.55)>ZMAX0)ZMAX=HI_ZMAX;/* a small KG needs more than the overview's deepest zoom to fill the view: lift the cap so the fit lands in full resolution (hiAuto takes over on the way), unless the overview was asked for */
  fitBox(b,rm?0:Math.min(900,420+Math.hypot(sel.c[0]-vb.x-vb.w/2,sel.c[1]-vb.y-vb.h/2)*0.12),true);
  /* the res-7 box is coarse (a 1.6 km² KG sits inside ≈5 km² hexagons): once the res-12 outline is here, refine the fit to its real bbox — unless the user moved on */
  if(ZMAX>ZMAX0){refits++;kgOutlineP(code).then(function(o){refits--;if(!o||!o.bbox||fitN!==selectKG.n||!sel||sel.code!==code||userNav>navAt)return;var bb=o.bbox;if(bb[2]-bb[0]<=0)return;fitBox(bb,rm?0:420,true)},function(){refits--})}landFull(navAt)}showSel();if(keep)return;pushHist({k:'kg',c:code,n:e[1],g:e[2]!==e[1]?e[2]:e[3]});setHash()})}
function clearSel(quiet){if(!sel&&!asel)return;sel=null;asel=null;if(selP){selP.classList.remove('on');selO.classList.remove('on')}if(!quiet){closeTools();picked=null;hide();setHash()}}
function selectCell(i,fly){pinned=i;ptClear();if(sel||asel)clearSel(true);unpeek();tip.classList.remove('follow');show(i);veilSync();var c=cells[i];if(fly!==false){var r=hex?hexAt(c[0],c[1])[1]:16;fitBox([c[0]-r*4,c[1]-r*4,c[0]+r*4,c[1]+r*4],rm?0:600)}
 var nm=typeof c[8]==='string'?c[8]:(c[8]!=null?kgs[c[8]]:'')||'',i2=nm.indexOf(' \u00b7 ');var ce=ixBy[typeof c[8]==='number'&&data?data.kg_codes[c[8]]:''];pushHist({k:'cell',x:c[0],y:c[1],n:i2>0?nm.slice(0,i2):(nm||'not digested yet'),g:ce?placeOf(ce,'',false):(i2>0&&norm(nm.slice(i2+3))!==norm(nm.slice(0,i2))?'Gemeinde '+nm.slice(i2+3):'')});settle();setHash()}
/* ── hash: #kg=63349 · #cell=x,y · optional ;open = card expanded (applied on load, not saved as the preference) ── */
var hashT=0;function setHash(){veilSync();tagSync();clearTimeout(hashT);hashT=setTimeout(function(){if(!history.replaceState||applying)return;if(live.t||order.length){setHash();return}var h=curHash();if(location.hash!==h)history.replaceState(null,'',location.pathname+location.search+h)},120)}
var applying=false;
function applyHash(){dbgLog('applyHash '+location.hash);selectKG.n=(selectKG.n||0)+1;navCancel();var t=location.hash.replace(/^#/,'').split(';'),m=null,c=null,h=null,ad=null,pp=null,pn='',v='',full=false,nofull=false,change=false,diff=0,trees=false,relief=false,ortho=0,roads=0,subsidy=false,screen=false,at=null,tw=null,open=false,below=false,xport=false,q=null,zz=0,cardAt=null,fx=null,i;if(!t[0])return false;
 for(i=0;i<t.length;i++){var x=t[i],r;if(r=/^kg=(\d{5})$/.exec(x))m=r[1];else if(r=/^adm=(\d|\d{3}|\d{5}|AT[0-9A-Z]{7})$/.exec(x))ad=r[1];else if(r=/^pt=(-?\d+\.\d+),(-?\d+\.\d+)$/.exec(x))pp=[+r[1],+r[2]];else if(r=/^pn=(.+)$/.exec(x)){try{pn=decodeURIComponent(r[1])}catch(z){pn=''}}else if(r=/^q=(.+)$/.exec(x))q=decodeURIComponent(r[1]);else if(r=/^cell=(\d+),(\d+)$/.exec(x))c=[+r[1],+r[2]];else if(r=/^h3=([0-9a-f]{15})$/.exec(x))h=r[1];else if(r=/^v=(obs|chg)$/.exec(x)){if(r[1]==='chg')change=true;else v=r[1]}else if(x==='change')change=true;else if(x==='diff')diff=1;else if(x==='diff=raw')diff=2;else if(x==='ortho')ortho=OT_OP;else if(r=/^ortho=(0?\.\d+|1(?:\.0+)?)$/.exec(x))ortho=Math.max(.1,Math.min(1,+r[1]));else if(x==='roads')roads=RD_OP;else if(r=/^roads=(0?\.\d+|1(?:\.0+)?)$/.exec(x))roads=Math.max(.1,Math.min(1,+r[1]));else if(x==='full')full=true;else if(x==='nofull')nofull=true;else if(x==='trees')trees=true;else if(x==='relief')relief=true;else if(x==='subsidy')subsidy=true;else if(r=/^f=(.+)$/.exec(x)){try{fx=decodeURIComponent(r[1])}catch(z){fx=r[1]}}else if(x==='screen')screen=true;else if(r=/^z=(\d+(?:\.\d+)?)$/.exec(x))zz=+r[1];else if(r=/^at=(\d+),(\d+),(\d+(?:\.\d+)?)$/.exec(x))at=[+r[1],+r[2],+r[3]];else if(r=/^t=(.+)$/.exec(x)){tw=tlParse(r[1]);if(!tw)return false;change=true}else if(x==='open')open=true;else if(x==='below')below=true;else if(x==='export')xport=true;else if(r=/^card=(-?\d+(?:\.\d+)?),(-?\d+(?:\.\d+)?)$/.exec(x))cardAt=[+r[1],+r[2]];else return false}
 tour.on=false;pause(0);applying=true;unmove();pendCard=(m||c||h||q||pp)&&cardAt&&!below?cardAt:null;autoSet(nofull||(!full&&auto.opt),false);setView(v,true);setChg(change,true);if(tw)tl.lvl=!tw.d1?0:tw.k1?1:2;if(tw&&tw.k1)tl.k=tw.k1;setTime(tw,true);setDif(diff>0,true,diff===2);setTree(trees,true);setRelief(relief,true);if(ortho)setOrthoOp(ortho,true);setOrtho(ortho>0,true);if(roads)setRoadsOp(roads,true);setRoads(roads>0,true);setSub(subsidy,true);if((fx||'')!==flt.f)fltSet(fx||'',true);if(fx){flt.open=true;fltRow()}if(screen!==fsOn)fsToggle();if(open!==tip.classList.contains('open'))setOpen(open,false,false);if(!mq.matches&&below!==tip.classList.contains('dock'))setDock(below,false);
 /* a hash without ;full while full resolution is on: leave it (hiOff resets ZMAX and drops the res-12 pin) — only #h3= implies full res; with ;full just drop the pin */
 if(!h&&hi.on){var hadPin=!!hi.pin;if(!full)hiOff(false);else if(hadPin){apUnpin(true);hi.pin=null;hi.hov=null;cur.classList.remove('c12')}if(hadPin&&(m||c||q||ad||pp)){picked=null;shown=-1;pk=null}}
 if(!pp)ptClear();if(!m&&!c&&!h&&!ad&&!pp){pinned=-1;clearSel(true);if(hi.on){hi.pin=null;hi.hov=null;if(!full)hiOff(false)}hide();picked=null;shown=-1;pk=null;if(!at&&W/vb.w>1.01)zoomTo(0,0,W,0)}/* hide() before the reset: the inline card (phone) only clears while something is still shown */
 /* ;full lands after the fly: hiOn needs the zoom (W/vb.w ≥ HI_MIN) to be there, and a KG fit at z 17 never trips hiAuto (ZMAX0) — so wait for the animation, then switch */
 var hiLand=function(){return new Promise(function(res){(function w(){if(animating)requestAnimationFrame(w);else res()})()}).then(function(){if(full){hiOn();if(!hi.on)dbgLog('full asked but not reached z='+(W/vb.w).toFixed(1))}else if(!at&&!zz&&!auto.opt&&(m||ad||pp)&&W/vb.w>=HI_MIN){hiOn();if(hi.on){auto.by=true;hiOn.h=1}}/* a bare #kg= / #adm= / #pt= link lands like the find box does: full resolution whenever the fit is deep enough */})},done=function(e){if(e)dbgLog('applyHash failed: '+(e&&e.message||e)+(e&&e.stack?' @ '+String(e.stack).split('\n')[1]:''));if(typeof xOpen==='function'&&xport!==!tXM.hidden)xOpen(xport,true);applying=false;setHash()},place0=function(){if(at){if(full)ZMAX=HI_ZMAX;var w=W/Math.max(1,at[2]);zoomTo(at[0]-w/2,at[1]-w*AR/2,w,0)}},zoomZ=function(){if(!zz||at)return;if(full)ZMAX=HI_ZMAX;var w=W/Math.max(1,zz),cx=vb.x+vb.w/2,cy=vb.y+vb.h/2;if(sel){cx=sel.c[0];cy=sel.c[1]}else if(asel){cx=asel.c2[0];cy=asel.c2[1]}else if(pt){cx=pt.x;cy=pt.y}else if(pinned>=0&&cells[pinned]){cx=cells[pinned][0];cy=cells[pinned][1]}zoomTo(cx-w/2,cy-w*AR/2,w,0)};
 if(h)get().then(function(){place0();return hiGoto(h,!at)}).then(done,done);
 else if(q)Promise.all([get(),ixGet()]).then(function(){var hits=search(q);if(!hits.length){done();return}m=hits[0][0];place0();return selectKG(m,!at&&!zz).then(function(){zoomZ();return hiLand()})}).then(done,done);
 else if(m)get().then(function(){place0();return selectKG(m,!at&&!zz)}).then(function(){zoomZ();return hiLand()}).then(done,done);
 else if(ad)get().then(function(){place0();return selectAdm(ad,!at&&!zz)}).then(function(){zoomZ();return hiLand()}).then(done,done);
 else if(pp)get().then(function(){place0();var i2=pn.indexOf(' | ');return goPoint({t:'adr',n:i2>0?pn.slice(0,i2):(pn||'point'),sub:i2>0?pn.slice(i2+3):'',lon:pp[0],lat:pp[1]},!at&&!zz)}).then(function(){zoomZ();return hiLand()}).then(done,done);
 /* #cell=x,y without at=/z= is what a tap at the overview wrote (at= is only written once zoomed): restore the overview with the cell pinned, do not fly — a reload after tapping a tour stop must not land zoomed in */
 else get().then(function(){place0();if(c){var i=near(c[0],c[1]);if(i>=0)selectCell(i,false)}zoomZ();return hiLand()}).then(done,done);return true}
/* ── history ── */
var HK='neg-hist';function hist(){try{return JSON.parse(localStorage.getItem(HK)||'[]')}catch(e){return[]}}
function hk(x){return x.k+':'+(x.c!=null?x.c:x.lon!=null?x.lon+','+x.lat:x.x+','+x.y)}
function pushHist(it){try{var k=hk(it),h=hist().filter(function(x){return hk(x)!==k});it.t=Date.now();h.unshift(it);localStorage.setItem(HK,JSON.stringify(h.slice(0,10)))}catch(e){}}
function agoT(t){var h=(Date.now()-t)/36e5;return h<1/60?'now':ago(h)}
function esc(s){return String(s).replace(/[&<>"]/g,function(c){return{'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]})}
function renderHist(){var h=hist(),s='';if(!h.length)s='<li class="mut">nothing yet \u2014 find a place or tap a cell</li>';
 else{s='<li class="hd"><span>recent</span><button type="button" class="gt-c">clear</button></li>';h.forEach(function(it,i){s+='<li role="option" tabindex="0" data-i="'+i+'"><span class="n"><span class="k">'+(it.k==='kg'?'KG':it.k==='h3'?'res 12':it.k==='adm'?(it.kk||'place'):it.k==='pt'?(it.t==='adr'?(it.kind==='street'?'street':'address'):'name'):'cell')+'</span>'+esc(it.n)+'</span><span class="g">'+esc(it.g||'')+'</span><span class="s">'+agoT(it.t)+'</span></li>'})}
 tList.innerHTML=s;tList.hidden=false;mode='hist';act=-1}
function goHist(i){var it=hist()[i];if(!it)return;collapse();if(it.k==='kg')selectKG(it.c);else if(it.k==='adm')selectAdm(it.c);else if(it.k==='pt')goPoint({t:it.t,kind:it.kind,n:it.n,sub:it.g,lon:it.lon,lat:it.lat});else if(it.k==='h3')get().then(function(){hiGoto(it.c)});else get().then(function(){var j=near(it.x,it.y);if(j>=0)selectCell(j)})}
/* ── search: one box for every kind of place. KGs come from the local index (/ne/glyph/kgs); Gemeinde · Bezirk · Bundesland from the admin index (/ne/glyph/admin) and get the same treatment as a KG (filled selection, outer stroke, full-res veil); addresses from the BEV Adressregister (/addresses/search, ranked by what is on screen); names from the DLM toponyms. The local lists answer on every keystroke, the remote ones a beat later (140 ms debounce, aborted when the text moves on, cached per text + view). ↓↑ walk every row, Enter takes the active one (or the first), Tab/→ completes the ghost, Esc closes. ── */
var SRCH=8,ADM_N=4,ADR_N=5,TOP_N=3;
function score(e,qs){var sc=0,i,q,j,w,hit;for(i=0;i<qs.length;i++){q=qs[i];hit=1e9;if(e.n===q)hit=0;else if((' '+e.n+' ').indexOf(' '+q+' ')>=0)hit=0.6;else if(e.n.indexOf(q)===0)hit=1;else if(e[0].indexOf(q)===0&&/^\d+$/.test(q))hit=1;else{for(j=0;j<e.w.length;j++){w=e.w[j];if(w.indexOf(q)===0){hit=Math.min(hit,j<e.n.split(' ').length?2:(e.g.indexOf(q)>=0?3:4))}}}
 if(hit===1e9)return-1;if(e.g===q)hit-=0.3;sc+=hit}return sc}
function search(q){var qn=norm(q);if(!qn||!ix)return[];var qs=qn.split(' '),out=[],i,e,s,a=kgAgg()||{};for(i=0;i<ix.length;i++){e=ix[i];s=score(e,qs);if(s<0)continue;var g=a[e[0]];out.push([s-(g?.5:0)-(g&&g.age<fresh?.2:0)+e.n.length/200,e])}
 out.sort(function(x,y){return x[0]-y[0]});return out.slice(0,SRCH).map(function(x){return x[1]})}
/* outline collections (/ne/glyph/admin): Bundesland (1-digit code) · Bezirk (3) · Gemeinde (5) · Natura 2000 site (SITECODE). Generic over the index's kinds{} — a new collection on the server shows up here with its kind label; ADM_UI adds the pretty label, the sub-line and the icon */
var adm=null,admLoad=null,admBy={},admKinds={};
var ADM_UI={bl:{k:'Bundesland',o:.1},bz:{k:'Bezirk',o:.05},gem:{k:'Gemeinde',o:0},n2k:{k:'Natura 2000',o:.08,sub:function(u){return(u.t==='A'?'birds':u.t==='B'?'habitats':u.t==='C'?'birds \u00b7 habitats':'site')+(u.ha?' \u00b7 '+(u.ha>=1000?(u.ha/100).toFixed(0)+'\u202fkm\u00b2':u.ha+'\u202fha'):'')}}};
function admGet(){if(admLoad)return admLoad;admLoad=fetch('/api/v1/ne/glyph/admin',{credentials:'omit'}).then(jsonOf).then(function(d){adm=[];admKinds=d.kinds||{};Object.keys(d).forEach(function(k){if(k==='kinds'||k==='notice'||!Array.isArray(d[k]))return;d[k].forEach(function(u){u.k=k;u.nn=norm(u.n);u.w=u.nn.split(' ');admBy[u.c]=u;adm.push(u)})});return adm}).catch(function(){admLoad=null});return admLoad}
function admKind(k){var ui=ADM_UI[k];if(ui)return ui.k;var kk=admKinds[k];return kk&&kk.kind?kk.kind:(k==='bundesland'?'Bundesland':k==='bezirk'?'Bezirk':k==='gemeinde'?'Gemeinde':k==='natura2000'?'Natura 2000':k||'place')}
function admSub(u){var ui=ADM_UI[u.k];if(ui&&ui.sub)return ui.sub(u);var p=u.k==='bz'?admBy[u.bl]:u.k==='gem'?admBy[u.bz]:null;return p?p.n:''}
function admSearch(q){var qn=norm(q);if(!qn||!adm)return[];var qs=qn.split(' '),out=[],i,j,k;for(i=0;i<adm.length;i++){var u=adm[i],sc=0,ok=true;for(j=0;j<qs.length;j++){var t=qs[j],h=-1;if(u.nn===t)h=0;else if(u.nn.indexOf(t)===0)h=1;else if((' '+u.nn+' ').indexOf(' '+t+' ')>=0)h=1.4;else for(k=0;k<u.w.length;k++)if(u.w[k].indexOf(t)===0){h=2;break}
  if(h<0){if(/^\d+$/.test(t)&&u.c.indexOf(t)===0)h=1;else{ok=false;break}}sc+=h}
 if(!ok)continue;out.push([sc+((ADM_UI[u.k]||{}).o||0)+u.nn.length/100,u])}out.sort(function(a,b){return a[0]-b[0]});var r=out.slice(0,ADM_N).map(function(x){return x[1]}),n2=out.filter(function(x){return x[1].k==='n2k'&&r.indexOf(x[1])<0}).slice(0,2).map(function(x){return x[1]});/* protected sites keep two seats of their own: a Gemeinde list must not crowd them out */
 return n2.length?r.slice(0,ADM_N-n2.length).concat(n2):r}
/* remote: addresses + names, keyed by text and the view (so panning and retyping both refresh) */
var rq={t:0,ab:null,key:'',adr:[],top:[],cache:{},keys:[],busy:false};
function viewLL(){var a=lonlat(vb.x,vb.y),b=lonlat(vb.x+vb.w,vb.y+vb.h);return{bbox:[Math.min(a[0],b[0]),Math.min(a[1],b[1]),Math.max(a[0],b[0]),Math.max(a[1],b[1])],c:[(a[0]+b[0])/2,(a[1]+b[1])/2]}}
function rqBusy(b){rq.busy=b;tList.classList.toggle('busy',b)}
function topPick(rows){var out=[],seen={},i;for(i=0;i<rows.length&&out.length<TOP_N;i++){var r=rows[i];if(!r||!r.name||r.layer==='ried'||r.layer==='sonstige'||r.lon==null)continue;var k=norm(r.name)+'|'+(r.gemeinde_code||'');if(seen[k])continue;seen[k]=1;var sub=[r.f_name||r.layer];if(r.gemeinde_name&&norm(r.gemeinde_name)!==norm(r.name))sub.push(r.gemeinde_name);if(r.elevation_m)sub.push(r.elevation_m+'\u202fm');out.push({t:'top',n:r.name,sub:sub.join(' \u00b7 '),lon:+r.lon,lat:+r.lat})}return out}
function remote(q){var qn=norm(q);clearTimeout(rq.t);if(qn.length<2){if(rq.ab){rq.ab.abort();rq.ab=null}rq.adr=[];rq.top=[];rq.key='';rqBusy(false);return}var v=viewLL(),key=qn+'|'+v.bbox.map(function(x){return x.toFixed(2)}).join(',');if(key===rq.key)return;var c=rq.cache[key];if(c){rq.key=key;rq.adr=c.adr;rq.top=c.top;if(rq.ab){rq.ab.abort();rq.ab=null}rqBusy(false);return}
 rq.t=setTimeout(function(){if(rq.ab)rq.ab.abort();var ab=rq.ab=new AbortController(),bb=W/vb.w>1.3?v.bbox.map(function(x){return x.toFixed(4)}).join(','):'',cc=v.c[0].toFixed(4)+','+v.c[1].toFixed(4),soft=function(e){if(e&&e.name==='AbortError')throw e;return null},num=/\d/.test(q);rqBusy(true);
  Promise.all([fetch('/api/v1/addresses/search?q='+encodeURIComponent(q.trim())+'&bbox='+bb+'&c='+cc+'&limit='+ADR_N,{signal:ab.signal,credentials:'omit'}).then(jsonOf).then(function(d){return(d.results||[]).map(function(a){return{t:'adr',n:a.label,sub:a.sub,lon:a.lon,lat:a.lat,kind:a.kind,inv:!!bb&&a.in_view,km:bb?a.dist_km:null}})}).catch(soft),
   num?Promise.resolve([]):fetch('/api/v1/toponyms/search?q='+encodeURIComponent(q.trim())+'&near='+cc+'&limit=14',{signal:ab.signal,credentials:'omit'}).then(jsonOf).then(function(d){return topPick(d.data||[])}).catch(soft)])
  .then(function(r){if(ab!==rq.ab)return;rq.ab=null;rq.key=key;rq.adr=r[0]||[];rq.top=r[1]||[];if(r[0]&&r[1]){rq.cache[key]={adr:r[0],top:r[1]};rq.keys.push(key);if(rq.keys.length>80)delete rq.cache[rq.keys.shift()]}rqBusy(false);if(mode==='search'){renderSearch();if(rq.go&&rq.go===qn&&items.length){rq.go='';pick(items[0])}else rq.go=''}},function(){if(ab===rq.ab){rq.ab=null;rqBusy(false)}})},140)}
/* the merged list: KGs, then Gemeinde/Bezirk/Land, then addresses, then names — addresses first when the text carries a house number and no KG answers */
var items=[],res=[];
function assemble(q){var kg=search(q),ad=admSearch(q);if(kg.length>5&&(ad.length||(rq.adr||[]).length||(rq.top||[]).length))kg=kg.slice(0,5);var kgI=kg.map(function(e){return{t:'kg',e:e}}),adI=ad.map(function(u){return{t:'adm',u:u}}),adr=rq.adr||[],top=rq.top||[];
 var num=/\d/.test(q)&&!kgI.length,exact=adI.length&&adI[0].u.nn===norm(q);var g=num?[adr,adI,kgI,top]:exact?[adI,kgI,adr,top]:[kgI,adI,adr,top];var fi=typeof fltFindItem==='function'?fltFindItem(q):null;if(fi)g.unshift([fi]);items=[].concat.apply([],g);res=kg;return items}
function hl(name,q){var n=norm(name),qs=norm(q).split(' '),i,best=-1,bl=0;for(i=0;i<qs.length;i++){var p=n.indexOf(qs[i]);if(p>=0&&qs[i].length>bl){best=p;bl=qs[i].length}}
 if(best<0||n.length!==name.length)return esc(name);return esc(name.slice(0,best))+'<b>'+esc(name.slice(best,best+bl))+'</b>'+esc(name.slice(best+bl))}
var ghostFill='';
function strip(x){return String(x).normalize('NFD').replace(/[\u0300-\u036f]/g,'').toLowerCase()}
function itName(it){return it.t==='kg'?it.e[1]:it.t==='adm'?it.u.n:it.n}
function itSub(it){return it.t==='kg'?it.e[0]:it.t==='adm'?admKind(it.u.k):it.t==='adr'?(it.kind==='street'?'street':'address'):'name'}
function ghost(q){var g=tools.querySelector('.gt-g');ghostFill='';if(!items.length||!q){g.innerHTML='';return}var it=items[0],nm=itName(it),sq=strip(q),sn=strip(nm);
 if(sn.indexOf(sq)===0&&sn.length>sq.length){ghostFill=nm;g.innerHTML='<i>'+esc(q)+'</i><em>'+esc(nm.slice(q.length))+'</em>'+(items.length===1?'<em class="hint"> \u00b7 '+esc(itSub(it))+'</em>':'')}
 else if(items.length===1){g.innerHTML='<i>'+esc(q)+'</i><em class="hint"> \u2192 '+esc(nm)+' \u00b7 '+esc(itSub(it))+'</em>'}else g.innerHTML='';
 g.style.left=tIn.offsetLeft+'px';g.style.width=tIn.offsetWidth+'px'}
var ICO={adr:'<svg class="ic" viewBox="0 0 16 16" aria-hidden="true"><path d="M8 14.5s-4.5-4.6-4.5-8a4.5 4.5 0 0 1 9 0c0 3.4-4.5 8-4.5 8z"/><circle cx="8" cy="6.5" r="1.6"/></svg>',top:'<svg class="ic" viewBox="0 0 16 16" aria-hidden="true"><path d="M1.5 13.5l4.3-7.5 2.6 4.2 1.9-2.7 4.2 6z"/></svg>',str:'<svg class="ic" viewBox="0 0 16 16" aria-hidden="true"><path d="M3 14L6.5 2M13 14L9.5 2M8 3.5v2M8 7.5v2M8 11.5v2"/></svg>',n2k:'<svg class="ic" viewBox="0 0 16 16" aria-hidden="true"><path d="M13.5 2.5C8 2.5 3.5 6 3.5 11.5c0 .9.2 1.6.5 2 .5-3.5 3-6.5 6-8-2.5 2-4.5 5-4.8 8.1C10.5 14 13.5 10 13.5 2.5z"/></svg>'};
function rowHTML(it,i,q){var a=kgAgg()||{},inner;
 if(it.t==='kg'){var e=it.e,g=a[e[0]],dot=g&&g.obs?'<i class="ob" title="observed layer posted by {OBS_NAME}"></i>':'<i class="e"></i>',sub=[];if(e[2]&&norm(e[2])!==e.n)sub.push(e[2]);if(e[3])sub.push(e[3]);inner='<span class="n">'+hl(e[1],q)+'</span><span class="g">'+esc(sub.join(' \u00b7 '))+'</span><span class="s">'+dot+e[0]+'</span>'}
 else if(it.t==='adm'){var u=it.u,p=admSub(u);inner='<span class="n">'+(u.k==='n2k'?ICO.n2k:'')+hl(u.n,q)+'</span><span class="g">'+esc(p)+'</span><span class="s"><span class="k">'+admKind(u.k)+'</span></span>'}
 else if(it.t==='adr'){inner='<span class="n">'+(it.kind==='street'?ICO.str:ICO.adr)+hl(it.n,q)+'</span><span class="g">'+esc(it.sub)+'</span><span class="s">'+(it.inv?'<em class="iv">in view</em>':it.km!=null?esc(kmS(it.km)):'')+'</span>'}
 else if(it.t==='flt')inner='<span class="n">'+hl(it.n,q)+'</span><span class="g">'+esc(it.sub)+'</span><span class="s"><span class="k">filter</span></span>'; else inner='<span class="n">'+ICO.top+hl(it.n,q)+'</span><span class="g">'+esc(it.sub)+'</span><span class="s"></span>';
 return'<li role="option" id="gt-o'+i+'" class="r-'+it.t+'" data-i="'+i+'" aria-selected="'+(i===act)+'">'+inner+'</li>'}
function kmS(k){return k<1?Math.round(k*1000)+'\u202fm':k<10?k.toFixed(1)+'\u202fkm':Math.round(k)+'\u202fkm'}
var SEC={kg:'',adm:'',adr:'addresses',top:'names'};
function renderSearch(){var q=tIn.value,qn=norm(q);if(mode!=='search'&&!tools.classList.contains('open'))return;remote(q);assemble(q);ghost(q);var s='',i,last='';
 if(!ix)s='<li class="mut">loading the index \u2026</li>';else if(!qn)s='';else if(!items.length)s=rq.busy||(rq.key!==qn+'|'+viewLL().bbox.map(function(x){return x.toFixed(2)}).join(','))&&qn.length>=2?'<li class="mut wait">searching \u2026</li>':'<li class="mut">nothing found for \u201c'+esc(q)+'\u201d</li>';
 else for(i=0;i<items.length;i++){var it=items[i],sec=SEC[it.t]||'';if(i&&it.t!==items[i-1].t&&(sec||last))s+='<li class="sp" role="presentation">'+(sec?'<span>'+sec+'</span>':'')+'</li>';last=sec;s+=rowHTML(it,i,q)}
 if(act>=items.length)act=items.length?0:-1;tList.innerHTML=s;tList.hidden=!s;tIn.setAttribute('aria-expanded',s?'true':'false');mode='search'}
function setAct(i){act=i;var li=tList.querySelectorAll('li[data-i]'),j;for(j=0;j<li.length;j++){var on=+li[j].dataset.i===i;li[j].setAttribute('aria-selected',on);if(on&&li[j].scrollIntoView)li[j].scrollIntoView({block:'nearest'})}tIn.setAttribute('aria-activedescendant',i>=0?'gt-o'+i:'')}
function pick(it){if(!it)return;if(it.t==='flt'){fltAdd(it.f);flt.open=true;fltRow();return}if(it.t==='kg')selectKG(it.e[0]);else if(it.t==='adm')selectAdm(it.u.c);else goPoint(it)}
function openSearch(){pause(0);tools.classList.remove('hist','set');tIn.style.width='';tools.classList.add('open');mode='search';boxSide();tIn.tabIndex=0;tIn.focus();ixGet().then(function(){if(mode==='search'||tIn.value)renderSearch()});admGet().then(function(){if(mode==='search'&&tIn.value)renderSearch()});get().then(function(){if(tools.classList.contains('open')&&tIn.value)renderSearch()});renderSearch()}
/* ── a found point (address · name): flown to, marked with a dot that stays, its res-7 hexagon pinned like a tap — the card tells what the register holds there ── */
var pt=null,ptG=null;
function xyOf(lon,lat){return[(lon*proj.kx-proj.minx)*proj.scale+proj.pad,(-lat-proj.miny)*proj.scale+proj.pad]}
function ptSync(){if(!ptG)return;var z=W/vb.w,r=(fsOn?38:34)/Math.max(1,Math.pow(z,.92));ptG.querySelector('.ptd').setAttribute('r',r.toFixed(2));ptG.querySelector('.pth').setAttribute('r',(r*2.4).toFixed(2))}
function ptDraw(){if(!pt){if(ptG)ptG.classList.remove('on');return}if(!ptG){cur.insertAdjacentHTML('beforebegin','<g class="pt"><circle class="pth" r="0"/><circle class="ptd" r="0"/></g>');ptG=svg.querySelector('g.pt')}ptG.setAttribute('transform','translate('+pt.x.toFixed(2)+' '+pt.y.toFixed(2)+')');ptSync();ptG.classList.add('on')}
function ptClear(){if(!pt)return;pt=null;ptDraw()}
/* the point's KG comes from the res-12 cell under it (glyph/cell?lon&lat → kgs{code:share}): that KG is selected around the dot — outline, veil and card as if it had been searched for; until it is known (or if the lookup fails) the res-7 hexagon stands in */
function goPoint(it,fly){collapse();return get().then(function(){if(!proj||!data)return;var p=xyOf(it.lon,it.lat),j=near(p[0],p[1]);pause(0);tour.on=false;if(sel||asel)clearSel(true);unpeek();pt={x:p[0],y:p[1],n:it.n,sub:it.sub||'',lon:it.lon,lat:it.lat,t:it.t,kind:it.kind};
 if(j>=0){pinned=j;tip.classList.remove('follow');show(j)}else{pinned=-1;hide()}veilSync();ptDraw();
 fetch('/api/v1/ne/glyph/cell?lon='+it.lon.toFixed(6)+'&lat='+it.lat.toFixed(6),{credentials:'omit'}).then(jsonOf).then(function(d){if(!pt||pt.x!==p[0]||!d||!d.kgs)return;var best='',bs=-1,k;for(k in d.kgs)if(d.kgs[k]>bs){bs=d.kgs[k];best=k}if(!best)return;
  return selectKG(best,false,true).then(function(){if(pt&&pt.x===p[0]&&sel&&sel.code===best){veilSync();setHash()}})}).catch(function(){});
 var r=hex?hexAt(p[0],p[1])[1]:16,k=it.kind==='street'?2.4:1.7,ms=0;if(fly!==false){ms=rm?0:Math.min(900,420+Math.hypot(p[0]-vb.x-vb.w/2,p[1]-vb.y-vb.h/2)*0.12);fitBox([p[0]-r*k,p[1]-r*k,p[0]+r*k,p[1]+r*k],ms,true);landFull(userNav)}
 setTimeout(function(){if(!pt||pt.x!==p[0])return;ping.setAttribute('cx',p[0]);ping.setAttribute('cy',p[1]);ping.classList.remove('go');void ping.getBoundingClientRect();ping.classList.add('go')},ms?ms*0.9:0);
 pushHist({k:'pt',t:it.t,kind:it.kind,n:it.n,g:it.sub||'',lon:it.lon,lat:it.lat});settle();setHash()})}
/* ── an administrative unit (Gemeinde · Bezirk · Bundesland): selected like a KG — its outline filled and stroked, the full-res veil cut to it ── */
var asel=null,admOut={};
function admOutline(c){var o=admOut[c];if(o)return Promise.resolve(o);return fetch('/api/v1/ne/glyph/admin/outline?c='+c,{credentials:'omit'}).then(jsonOf).then(function(d){if(d&&d.d)admOut[c]=d;return d})}
/* the KG set of an admin unit by GKZ prefix (ix row [8]): Gemeinde 5 · Bezirk 3 · Land 1 digits; Wien Gemeindebezirke (901–923) and Natura 2000 sites have no KG set → their cells are the ones whose centre lies inside the outline, and the KGs those cells belong to stand in for the set */
function admKGs(u){if(!u||!ix)return null;var k=u.k,c=u.c,p=k==='gem'?5:k==='bz'?3:k==='bl'?1:0;if(!p||(k==='bz'&&c[0]==='9'))return null;var r=[];for(var i=0;i<ix.length;i++){var g=ix[i][8]||'';if(g.slice(0,p)===c)r.push(ix[i][0])}return r}
function admFilt(u){if(!u)return'';var k=u.k,c=u.c;return k==='gem'?'gemeinde='+c:k==='bz'&&c[0]!=='9'?'district='+c:k==='bl'?'state='+c:''}
function selectAdm(c,fly){collapse();var navAt=userNav;return Promise.all([get(),admGet(),admOutline(c),ixGet()]).then(function(r){var o=r[2];if(!o||!o.d||!data)return;var u=admBy[c];pinned=-1;ptClear();if(sel||asel)clearSel(true);pause(0);tour.on=false;picked=null;shown=-1;pk=null;
 var kgsIn=admKGs(u),idx=[],ag=kgAgg();if(kgsIn){for(var i=0;i<kgsIn.length;i++){var a1=ag[kgsIn[i]];if(a1)idx=idx.concat(a1.idx)}}else idx=cellsInPath(o);var filt=admFilt(u),kgset=kgsIn||kgsOf(idx),b=o.bbox;
 asel={c:c,n:o.n||(u&&u.n)||c,kind:o.kind,d:o.d,b:b,c2:[(b[0]+b[2])/2,(b[1]+b[3])/2],idx:idx,a:aggOf(idx),filt:filt,kgs:kgset,kgn:kgset.length,r:Math.max(b[2]-b[0],b[3]-b[1])/2,name:o.n||(u&&u.n)||c};asel.row=areaRow(asel);drawSelD(o.d);
 if(fly!==false&&userNav===navAt){var ob=o.bbox;if(typeof HI_ZMAX==='number'&&!hi.on&&!auto.opt&&W/(Math.max(ob[2]-ob[0],(ob[3]-ob[1])/AR)/0.55)>ZMAX0)ZMAX=HI_ZMAX;/* like selectKG: lift the cap so a small Gemeinde or site fits at full resolution */fitBox(o.bbox,rm?0:Math.min(900,420+Math.hypot(asel.c2[0]-vb.x-vb.w/2,asel.c2[1]-vb.y-vb.h/2)*0.12),true);landFull(navAt)}veilSync();showSel();
 pushHist({k:'adm',c:c,n:asel.n,g:u?admSub(u):'',kk:admKind(u?u.k:o.kind)});settle();setHash()})}
/* ── selection tags: whatever is selected sits in the box — KG (outline), res-7 hexagon, res-12 cell (filled hex), tree — as a small glyph + plain name, code muted. Several can hold at once (a KG and a cell inside it). The box × clears them all; Backspace in the empty box drops the last; clicking the box opens the input to replace. tagSync runs from setHash/hide so it follows every selection path ── */
var tagBox=tools.querySelector('.gt-ch'),tags=[],tagKey='';
var TAG_I={kg:'<svg class="i" viewBox="0 0 16 16" aria-hidden="true"><path d="M3 4.5l4-2.2 5.5 1.6 1 6.4-4.3 3.4-5.7-1.2z"/></svg>',adm:'<svg class="i" viewBox="0 0 16 16" aria-hidden="true"><path d="M2 5l3.5-2.5 4 1.5 4.5-1v8l-4 2.5-4.5-1.5L2 13z"/></svg>',pt:'<svg class="i" viewBox="0 0 16 16" aria-hidden="true"><path d="M8 14.5s-4.5-4.6-4.5-8a4.5 4.5 0 0 1 9 0c0 3.4-4.5 8-4.5 8z"/><circle cx="8" cy="6.5" r="1.6"/></svg>',hex:'<svg class="i" viewBox="0 0 16 16" aria-hidden="true"><path d="M8 1.6l5.5 3.2v6.4L8 14.4l-5.5-3.2V4.8z"/></svg>',h3:'<svg class="i" viewBox="0 0 16 16" aria-hidden="true"><path d="M8 1.6l5.5 3.2v6.4L8 14.4l-5.5-3.2V4.8z"/></svg>',tree:'<svg class="i" viewBox="0 0 16 16" aria-hidden="true"><path d="M8 1.5l4.2 6.3H9.8l3 4.2H3.2l3-4.2H3.8z"/><path d="M8 12v2.8"/></svg>'};
function tagList(){var o=[],c,nm,i2;if(typeof hi!=='object')return o;
 if(pt)o.push({t:'pt',n:pt.n,s:pt.t==='adr'?(pt.kind==='street'?'street':'address'):'name',x:function(){ptClear();if(sel)clearSel(true);hide();veilSync();setHash()}});
 if(sel)o.push({t:'kg',n:sel.e[1],s:sel.code,x:function(){clearSel()}});if(asel)o.push({t:'adm',n:asel.n,s:admKind(admBy[asel.c]?admBy[asel.c].k:asel.kind),x:function(){clearSel()}});
 if(pinned>=0&&cells[pinned]){c=cells[pinned];nm=typeof c[8]==='string'?c[8]:(c[8]!=null?kgs[c[8]]:'')||'';i2=nm.indexOf(' \u00b7 ');o.push({t:'hex',n:i2>0?nm.slice(0,i2):(nm||'hexagon'),s:'res 7',x:function(){pinned=-1;hide();setHash();pause(6000)}})}
 if(hi.on&&hi.pin){var d=hi.pin,hn=hiName(d);o.push({t:'h3',n:hn.name||'cell',s:d.cell.slice(-6),x:function(){hiUnpin()}})}
 if(hi.on&&hi.apx){var a=hi.apx.a,e=ixBy[a.kg_code];o.push({t:'tree',n:'tree '+apH(a.height_m),s:e?e[1]:'',x:function(){apUnpin()}})}
 return o}
function tagSync(){if(!tagBox)return;var o=tagList(),k=o.map(function(t){return t.t+':'+t.n+':'+t.s}).join('|');tags=o;if(k!==tagKey){tagKey=k;var h='';o.forEach(function(t,i){h+='<span class="gt-cp gt-cp-'+t.t+'" role="listitem" data-i="'+i+'" title="'+esc(t.n)+(t.s?' \u00b7 '+esc(t.s):'')+'">'+TAG_I[t.t]+'<span class="n">'+esc(t.n)+'</span>'+(t.s?'<span class="s">'+esc(t.s)+'</span>':'')+'</span>'});tagBox.innerHTML=h;tagBox.hidden=!h}
 var has=!!o.length,typing=mode==='search'||tools.classList.contains('hist');if(typing)return;tools.classList.toggle('set',has);tools.classList.toggle('open',has);tIn.tabIndex=has?0:-1;tIn.placeholder=has?'replace \u2026':'KG, place, address or code';tX.setAttribute('aria-label',has?'clear the selection':'close')}
/* settle = leave typing mode: the text, list and ghost go; the box stays if something is selected (shown as tags), else it folds away */
function settle(){tools.classList.remove('hist','lft','up');clearTimeout(rq.t);if(rq.ab){rq.ab.abort();rq.ab=null}rq.key='';rqBusy(false);tIn.value='';tIn.style.width='';tools.querySelector('.gt-g').innerHTML='';ghostFill='';res=[];mode='';act=-1;tList.hidden=true;tList.innerHTML='';tIn.setAttribute('aria-expanded','false');tIn.setAttribute('aria-activedescendant','');if(document.activeElement===tIn)tIn.blur();tagSync()}
function collapse(){settle()}
function closeTools(){settle()}
function boxSide(){var r=tools.getBoundingClientRect(),f=fig.getBoundingClientRect(),vh=window.visualViewport?window.visualViewport.height:window.innerHeight;var need=Math.max(0,parseFloat(getComputedStyle(tIn).fontSize)||13)*21;tools.classList.toggle('lft',r.right-need<4&&r.left+need<=window.innerWidth);tools.classList.toggle('up',vh-r.bottom<320&&r.top>vh-r.bottom)}
function clearAll(){ptClear();if(sel||asel)clearSel(true);pinned=-1;if(typeof hi==='object'&&hi.on){apUnpin(true);hi.pin=null;hi.hov=null;cur.classList.remove('c12')}picked=null;shown=-1;pk=null;hide();setHash();pause(6000)}
tQ.addEventListener('click',function(){if(!tools.classList.contains('open'))openSearch()});
tX.addEventListener('click',function(){var had=tags.length;settle();if(had)clearAll();main.focus({preventScroll:true})});
tools.querySelector('.gt-s').addEventListener('click',function(e){if(tools.classList.contains('set')&&!e.target.closest('.gt-x'))openSearch()});
tH.addEventListener('click',function(){if(tools.classList.contains('hist')){collapse();return}tools.classList.remove('open','set');tIn.tabIndex=-1;tools.classList.add('hist');pause(0);renderHist();get()});
tIn.addEventListener('input',function(){act=-1;tools.classList.remove('set');tIn.style.width='';renderSearch()});
tIn.addEventListener('focus',function(){if(tools.classList.contains('set'))openSearch()});
tIn.addEventListener('keydown',function(e){var k=e.key;if(k==='ArrowDown'||k==='ArrowUp'){e.preventDefault();if(!items.length)return;setAct((act+(k==='ArrowDown'?1:-1)+items.length)%items.length);return}
 if(k==='Enter'){e.preventDefault();var it=items[act>=0?act:0];if(it)pick(it);else if(rq.busy||rq.t)rq.go=norm(tIn.value);return}if(k==='Backspace'&&!tIn.value&&tags.length){e.preventDefault();var t=tags[tags.length-1];mode='';t.x();if(tagList().length)openSearch();else{settle();main.focus({preventScroll:true})}return}if(k==='Escape'){e.preventDefault();collapse();main.focus({preventScroll:true});return}
 if((k==='Tab'||k==='ArrowRight')&&ghostFill&&tIn.selectionStart===tIn.value.length){e.preventDefault();tIn.value=ghostFill;act=-1;renderSearch();return}if(k==='Tab')collapse()});
tList.addEventListener('pointerdown',function(e){e.preventDefault()});
tList.addEventListener('click',function(e){var x=e.target.closest('.gt-c');if(x){try{localStorage.removeItem(HK)}catch(z){}renderHist();return}var li=e.target.closest('li[data-i]');if(!li)return;var i=+li.dataset.i;if(mode==='hist')goHist(i);else pick(items[i])});
tList.addEventListener('keydown',function(e){if(mode!=='hist')return;var li=e.target.closest('li[data-i]');if(e.key==='Enter'&&li){e.preventDefault();goHist(+li.dataset.i)}else if(e.key==='Escape'){closeTools();tH.focus()}else if(e.key==='ArrowDown'||e.key==='ArrowUp'){e.preventDefault();var n=(e.key==='ArrowDown'?li.nextElementSibling:li.previousElementSibling);if(n&&n.dataset.i!=null)n.focus()}});
tList.addEventListener('pointermove',function(e){if(mode!=='search')return;var li=e.target.closest('li[data-i]');if(li&&+li.dataset.i!==act)setAct(+li.dataset.i)});
document.addEventListener('pointerdown',function(e){if(!tools.contains(e.target)&&(tools.classList.contains('open')||tools.classList.contains('hist')))collapse()});
document.addEventListener('keydown',function(e){if(e.key==='Escape'&&(sel||asel||pt)&&(e.target===document.body||e.target===document.documentElement)){ptClear();if(sel||asel)clearSel();else{hide();setHash()}return}if(e.key==='Escape'&&fsOn&&!fsNative&&(e.target===document.body||e.target===document.documentElement)){fsToggle();return}if((e.key==='f'||e.key==='F')&&!e.ctrlKey&&!e.metaKey&&!e.altKey&&tour.vis&&!/^(input|textarea|select)$/i.test(e.target.tagName)&&!e.target.isContentEditable){e.preventDefault();if(e.key==='F')fsToggle();else fltToggle();return}if(e.key==='/'&&!e.ctrlKey&&!e.metaKey&&!e.altKey&&tour.vis&&!/^(input|textarea|select)$/i.test(e.target.tagName)&&!e.target.isContentEditable){e.preventDefault();openSearch()}});
window.addEventListener('hashchange',function(){if(location.hash!==curHash())applyHash()});
/* ── picture = one PROFILE (declared · observed: what fills the cells) × independent OVERLAYS (change · subsidy · trees · relief: marks on top). A toggle never changes the profile and never moves the view; the card reorders its sources to match; every set* logs to dbg. docs/ne-glyph.md §view model ── */
var gv=fig.querySelector('.glyph-view'),VIEWS=['','obs'];
function setView(v,quiet){if(VIEWS.indexOf(v)<0)v='';if(v===view&&!quiet)return;dbgLog('profile '+(v||'declared'));view=v;svg.classList.toggle('v-obs',v==='obs');if(typeof sb!=='undefined')subSync();
 [].forEach.call(gv.querySelectorAll('.gv-b'),function(b){b.setAttribute('aria-checked',b.dataset.v===v?'true':'false')});gvBusy();
 if(quiet)return;pause(0);if(v)tour.on=false;get();if(hi.on)hiView(true);recard();hiLegend();setHash();
 if(v&&!setView.h){setView.h=1;showHint('teal = an observed layer was posted for the land')}}
function setChg(on,quiet){on=!!on;if(on===cg.on)return;dbgLog('change '+(on?'on':'off'));cg.on=on;svg.classList.toggle('v-chg',on);var b=gv.querySelector('.gv-chg');if(b){b.setAttribute('aria-pressed',on?'true':'false');b.title=on?'take the change marks off the map ( c )':'change \u00b7 cells whose declared use moved between digests \u00b7 darker = more cells confirmed \u00b7 pale = candidate, seen once ( c )'}if(typeof sb!=='undefined')subSync();gvBusy();
 tlSync();if(quiet)return;pause(0);if(on)tour.on=false;get();if(hi.on)hiView(true);recard();hiLegend();setHash();
 if(on&&!setChg.h){setChg.h=1;showHint('vermilion = change inside \u00b7 darker = more cells confirmed \u00b7 pale = candidate, seen once')}}
/* ── diff: where the register and the LiDAR disagree about the SAME cell, today (not change, which is the register over time). v=dif tiles at LOD 0/1 — one hue, the mark is the kind: solid = structures declared the flight did not see, dots = structures the flight saw that are undeclared and rose ≥ 2 m between the flights, hatch = declared forest the lidar (or Hansen) saw cut. Only kinds the observed row itself corroborates are drawn (docs/ne-change.md §6d); df.raw lifts that filter (;diff=raw). Its time axis is the lidar epoch per KG (srtm als{}), shown as an age. Hash ;diff · key d ── */
var df={on:false,raw:false};
function setDif(on,quiet,raw){on=!!on;var r=raw===undefined?df.raw:!!raw;if(on===df.on&&r===df.raw)return;dbgLog('diff '+(on?'on':'off')+(on&&r?' raw':''));df.on=on;df.raw=r;svg.classList.toggle('v-dif',on);var b=gv.querySelector('.gv-dif');if(b){b.setAttribute('aria-pressed',on?'true':'false');b.title=on?'take the diff marks off the map ( d )':'diff \u00b7 where the register and the LiDAR disagree about the same cell \u00b7 solid = structures declared, none in the LiDAR \u00b7 dots = structures in the LiDAR, none declared \u00b7 ring = declared forest, currently no trees (full resolution) ( d )';if(b.classList.contains('off')){b.dataset.t0=ttl(b);b.title=AVAIL.diff}}gvBusy();
 if(quiet)return;pause(0);if(on)tour.on=false;if(hi.on){hiCull();hiView(true)}recard();hiLegend();setHash();
 if(on&&!setDif.h){setDif.h=1;showHint('magenta = the LiDAR and the register disagree \u00b7 solid: structures declared, none in the LiDAR \u00b7 dots: structures in the LiDAR, none declared \u00b7 ring: declared forest, currently no trees')}}
gv.addEventListener('click',function(e){var t=e.target.closest('.gv-t,.gv-b');if(!t)return;main.focus({preventScroll:true});if(t.classList.contains('gv-b')){setView(t.dataset.v);return}
 if(t.classList.contains('off')){availHint(t.classList.contains('gv-tree')?'trees':t.classList.contains('gv-dif')?'diff':t.classList.contains('gv-ortho')?'ortho':t.classList.contains('gv-rd')?'roads':'relief');return}
 if(t.classList.contains('gv-ortho')){if(otPop.held()){return}setOrtho(!ot.on)}else if(t.classList.contains('gv-rd')){if(rdPop.held()){return}setRoads(!rd.on)}else if(t.classList.contains('gv-chg'))setChg(!cg.on);else if(t.classList.contains('gv-dif'))setDif(!df.on);else if(t.classList.contains('gv-sub'))setSub(!sb.on);else if(t.classList.contains('gv-tree'))setTree(!hi.tree);else if(t.classList.contains('gv-relief'))setRelief(!rl.on)});
/* the dot loader trails the profile's word while its tiles (or the first glyph fetch) are on the way; an overlay toggle pulses while its own tiles load (sub: subStatus, relief: relStatus) */
function gvBusy(){if(typeof sb!=='undefined')subStatus(false);var p={};if(load&&!data)p[view]=1;if(typeof hi!=='undefined'&&hi.on)for(var k in hi.tiles){var e=hi.tiles[k];if(!e.ok)p[e.v]=1}
 var gt=gv.querySelector('.gv-tree'),gc=gv.querySelector('.gv-chg'),gd=gv.querySelector('.gv-dif');if(gt)gt.classList.toggle('busy',!!p.apex&&hi.tree);if(gc)gc.classList.toggle('busy',!!p.chg&&cg.on);tlBusy(!!p.chg&&cg.on&&!!tl.win);if(gd)gd.classList.toggle('busy',!!p.dif&&df.on);var gw=gv.querySelector('.gv-rd');if(gw)gw.classList.toggle('busy',!!p.rd&&rd.on);
 [].forEach.call(gv.querySelectorAll('.gv-b'),function(b){var v=b.dataset.v;b.classList.toggle('busy',!!p[v]&&(v===view||v===''))})}
['pointerdown','pointerup','pointermove','wheel','dblclick','keydown','touchstart'].forEach(function(t){gv.addEventListener(t,function(e){e.stopPropagation()},{passive:t==='wheel'||t==='touchstart'})});
/* ── full resolution: the declared product at its native H3 res 12 — PNG tiles (one hexagon per cell, dominant land-use group; the second group as an inner hexagon when it holds ≥ 25 %) streamed for the view, the same card for a single cell.
   The view is honoured: the button never moves the view — it is only tappable once the view is zoomed past HI_MIN (zooming is the map's own gestures, find and the close button). ── */
var tD=tools.querySelector('.gt-d'),tN=fig.querySelector('.gt-n'),hi={on:false,g:null,l:{},tiles:{},order:[],bad:{},pin:null,hov:null,docs:[],a:null,t:0,n:0,t0:0,left:0,st:0,lod:0,vT:0,tree:false,apx:null};
var LOD=[[0.025,40],[0.01,100],[0.1,10]],BLEED=1,HI_MIN=8,HI_LOD0=110,HI_LOD1=300,HI_SUBF=40,HI_ZMAX=900,HI_TILES=48,DEVMEM=navigator.deviceMemory||(coarse?4:8),HI_KEEP=DEVMEM<=2?32:(DEVMEM<=4||coarse)?56:160,ZMAX0=ZMAX,GO=['geb','bau','acker','gruen','wald','wasser','verkehr','alpen','sonst'];
function px(lon){return(lon*proj.kx-proj.minx)*proj.scale+proj.pad}function py(lat){return(-lat-proj.miny)*proj.scale+proj.pad}
/* layer tree, bottom → top: .hi > .b (declared tiles, three LODs: coarse 0.1° under the near 0.025° and 0.01° ones) · .ov-obs (the observed profile) · .ov-chg · .ov-sub / .ov-subc · [.ov-rel, relLayer] · .ov-tree (crown marks); CSS shows the groups of the profile and the overlays on and steps the fill back */
function hiLayer(){if(hi.g)return;var ns='http://www.w3.org/2000/svg',g=document.createElementNS(ns,'g');g.setAttribute('class','hi');['','obs','chg','dif','sub','subc','rd','flt','tree'].forEach(function(v){var l=document.createElementNS(ns,'g');l.setAttribute('class',v?'ov ov-'+v:'b');if(v==='rd')l.style.opacity=rd.op.toFixed(2);hi.l[v]=[document.createElementNS(ns,'g'),document.createElementNS(ns,'g'),document.createElementNS(ns,'g')];hi.l[v].forEach(function(x,i){x.setAttribute('data-lod',i)});l.appendChild(hi.l[v][2]);l.appendChild(hi.l[v][0]);l.appendChild(hi.l[v][1]);g.appendChild(l)});dots.parentNode.insertBefore(g,hexG?hexG.nextSibling:dots.nextSibling);hi.g=g}
function hiLOD(){var z=W/vb.w;return z>=HI_LOD1?1:z>=HI_LOD0?0:2}
/* the subsidy layer's LOD-2 tiles come in two cuts: the res-7 overview wash below HI_SUBF, the Schläge themselves (f=1, the 16 bins of the 0.1° box) from HI_SUBF up — a field is a few pixels wide there, the base already shows them, the hexagons would hide them */
function subFine(){return hi.on&&hiLOD()===2&&W/vb.w>=HI_SUBF}
/* tiles touching the view, nearest the centre first, capped */
function hiWant(lod){var L=LOD[lod][0],a=lonlat(vb.x,vb.y+vb.h),b=lonlat(vb.x+vb.w,vb.y),i0=Math.floor(a[0]/L),i1=Math.floor(b[0]/L),j0=Math.floor(a[1]/L),j1=Math.floor(b[1]/L),cx=vb.x+vb.w/2,cy=vb.y+vb.h/2,out=[];
 for(var i=i0;i<=i1;i++)for(var j=j0;j<=j1;j++){var x0=px(i*L),x1=px((i+1)*L),y0=py((j+1)*L),y1=py(j*L);out.push({k:lod+'/'+i+'/'+j,lod:lod,i:i,j:j,x:x0,y:y0,w:x1-x0,h:y1-y0,d:Math.hypot((x0+x1)/2-cx,(y0+y1)/2-cy)})}
 out.sort(function(p,q){return p.d-q.d});return out.slice(0,HI_TILES)}
/* one tile of one view (v = '' land use, 'obs', 'chg'); the base tile carries the shimmer placeholder, an overlay just fades in over it */
function hiDrop(key){var e=hi.tiles[key];if(!e)return;delete hi.tiles[key];var p=hi.order.indexOf(key);if(p>=0)hi.order.splice(p,1);if(e.img&&e.img.parentNode)e.img.parentNode.removeChild(e.img);if(e.ph&&e.ph.parentNode)e.ph.parentNode.removeChild(e.ph);if(e.url)URL.revokeObjectURL(e.url);var nb=0;for(var q in hi.bad)nb++;if(nb>400)hi.bad={};hi.bad[key]=Date.now()}
function hiTile(t,v,f){v=v||'';f=!!f&&t.lod===2&&(v==='sub'||v==='subc');var raw=v==='dif'&&df.raw,win=v==='chg'?tlStr():'',key=v+'/'+t.k+(f?'~f':'')+(raw?'~r':'')+(win?'~'+win:''),e=hi.tiles[key];if(e){hi.order.splice(hi.order.indexOf(key),1);hi.order.push(key);return e}if(hi.bad[key]&&Date.now()-hi.bad[key]<NET_TTL)return null;delete hi.bad[key];
 e=hi.tiles[key]={t:t,v:v,f:f,raw:raw,win:win,img:null,ph:null,ok:false,on:true,n:0,lu:null,m:{}};hi.order.push(key);var L=hi.l[v][t.lod],ns='http://www.w3.org/2000/svg';
 if(!v){e.ph=document.createElementNS(ns,'rect');e.ph.setAttribute('class','ph');e.ph.setAttribute('x',t.x);e.ph.setAttribute('y',t.y);e.ph.setAttribute('width',t.w);e.ph.setAttribute('height',t.h);e.ph.style.animationDelay=(-(t.i*7+t.j*3)%13/10)+'s';L.appendChild(e.ph)}
 if(!hi.left)hi.t0=performance.now();hi.left++;hiStatus();
 fetch('/api/v1/ne/glyph/tile?lod='+t.lod+'&i='+t.i+'&j='+t.j+(v?'&v='+v:'')+(f?'&f=1':'')+(raw?'&raw=1':'')+(win?'&t='+win:''),{credentials:'omit',signal:hi.a.signal}).then(function(r){if(!r.ok)throw 0;var hd=r.headers.get('X-NE-Tile')||'';hd.split(';').forEach(function(kv){var p=kv.indexOf('=');if(p>0)e.m[kv.slice(0,p)]=kv.slice(p+1)});e.n=+e.m.n||0;e.lu=e.m.lu?e.m.lu.split(',').map(Number):null;if(r.status===204){e.down=e.m.down==='1';return null}return r.blob()}).then(function(b){
  if(hi.tiles[key]!==e){hi.left=Math.max(0,hi.left-1);hiStatus();return}if(!b){e.ok=true;hi.left--;hiStatus();if(!hi.left)hiCull();if(f&&e.down)setTimeout(function(){if(hi.tiles[key]!==e)return;delete hi.tiles[key];var p=hi.order.indexOf(key);if(p>=0)hi.order.splice(p,1);hiCull();hiView()},8000);return}e.url=URL.createObjectURL(b);
  hiDecode(e.url,function(){if(hi.tiles[key]!==e||!e.url){hi.left=Math.max(0,hi.left-1);hiStatus();return}var im=document.createElementNS(ns,'image'),bl=BLEED/LOD[t.lod][1];im.setAttribute('x',t.x-bl);im.setAttribute('y',t.y-bl);im.setAttribute('width',t.w+2*bl);im.setAttribute('height',t.h+2*bl);im.setAttribute('preserveAspectRatio','none');im.setAttribute('data-t',key);if(v==='sub'||v==='subc')im.style.clipPath='inset('+bl+'px)';im.setAttribute('href',e.url);im.setAttributeNS('http://www.w3.org/1999/xlink','href',e.url);
  im.addEventListener('load',function(){im.classList.add('in');if(e.ph){e.ph.classList.add('gone');setTimeout(function(){if(e.ph.parentNode)e.ph.parentNode.removeChild(e.ph)},400)}});L.insertBefore(im,L.firstChild);e.img=im;e.ok=true;if(!v)hi.n+=e.n;hi.left=Math.max(0,hi.left-1);hiStatus();if(f||!hi.left)hiCull()})})
 .catch(function(x){if(x&&x.name==='AbortError')return;hi.left=Math.max(0,hi.left-1);if(hi.tiles[key]===e)hiDrop(key);hiStatus()});
 hiEvict();return e}
/* eviction: the budget HI_KEEP is set from navigator.deviceMemory (32 / 56 / 160 tiles ≈ 1.1 MP each); off-screen tiles go first (oldest first), tiles on screen only when nothing else is left */
/* eviction never takes a tile the LOD in use wants (on screen or in its one-tile bleed): otherwise the under-LOD cull flips back and the tile is fetched again — a loop on small budgets with many layers on */
function hiEvict(){var over=hi.order.length-HI_KEEP;if(over<=0)return;var n=0,keep={};if(hi.on)hiWant(hiLOD()).forEach(function(t){keep[t.k]=1});for(var i=0;i<hi.order.length&&hi.order.length>HI_KEEP;){var k=hi.order[i],o=hi.tiles[k];/* a wanted base key protects every variant of the tile except change tiles of another time window — a scrub leaves dozens of those per tile */if(o&&(o.on||(keep[o.t.k]&&!(o.v==='chg'&&(o.win||'')!==tlStr())))){i++;continue}hi.order.splice(i,1);delete hi.tiles[k];n++;if(!o)continue;if(o.img&&o.img.parentNode)o.img.parentNode.removeChild(o.img);if(o.ph&&o.ph.parentNode)o.ph.parentNode.removeChild(o.ph);if(o.url){URL.revokeObjectURL(o.url);o.url=null}}hi.evicted=(hi.evicted||0)+n;if(n)dbgLog('evict '+n+' tiles (keep '+HI_KEEP+', left '+hi.order.length+')')}
/* cull: tiles out of view leave the render tree (kept in the cache) */
/* cull: a coarse LOD-2 subsidy tile stays on screen under the fine cut until its fine twin has arrived (the res-7 wash first, the Schläge a few seconds later), never the other way round */
/* under-LOD cull: once every wanted tile of the LOD in use (base + the view's overlay) is in, the tiles of the other two LODs that lie under it leave the paint — until then they stay as the backdrop the new LOD fades in over. Three stacked 1.1-MP bitmaps per screen became one; on a phone (WebView rasters on the app's UI thread) that was the difference between a scroll and an ANR dialog. */
function hiUnder(L){return !hiUnderMiss(L)}
/* dbg: the first wanted key of LOD L that is not in yet ('' when all are) — state().mem.underMiss */
function hiUnderMiss(L){var want=hiWant(L);if(!want.length)return 'none wanted';for(var i=0;i<want.length;i++){var b=hi.tiles['/'+want[i].k];if(!b||!b.ok)return '/'+want[i].k+(b?' pending':' absent');if(view){var o=hi.tiles[view+'/'+want[i].k];if(!o||!o.ok)return view+'/'+want[i].k+(o?' pending':' absent')}if(cg.on){var ck='chg/'+want[i].k+(tlStr()?'~'+tlStr():''),c=hi.tiles[ck];if(!c||!c.ok)return ck+(c?' pending':' absent')}if(df.on&&L!==2){var d=hi.tiles['dif/'+want[i].k+(df.raw?'~r':'')];if(!d||!d.ok)return 'dif/'+want[i].k+(d?' pending':' absent')}if(rd.on&&L!==2){var rr=hi.tiles['rd/'+want[i].k];if(!rr||!rr.ok)return 'rd/'+want[i].k+(rr?' pending':' absent')}}return ''}
function hiCull(){var chg=false,sf=subFine(),L=hiLOD(),und=hi.on&&hiUnder(L);svg.classList.toggle('hi-near',hi.on&&L!==2);svg.classList.toggle('sub-fine',sf);if(und!==!!hi.under)dbgLog('under-lod '+(und?'hidden':'shown')+' lod='+L);hi.under=und;for(var k in hi.tiles){var e=hi.tiles[k],t=e.t,on=t.x<vb.x+vb.w&&t.x+t.w>vb.x&&t.y<vb.y+vb.h&&t.y+t.h>vb.y;if(on&&t.lod===2&&(e.v==='sub'||e.v==='subc')&&!!e.f!==sf){var tw=hi.tiles[e.v+'/'+t.k+(sf?'~f':'')];on=!e.f&&!(tw&&tw.ok&&!tw.down)}if(on&&e.v==='dif'&&!!e.raw!==df.raw)on=false;if(on&&e.v==='chg'&&(e.win||'')!==tlStr()){var cw=hi.tiles['chg/'+t.k+(tlStr()?'~'+tlStr():'')];on=!(cw&&cw.ok)}if(on!==e.on){e.on=on;chg=true}var vis=on&&!(und&&t.lod!==L);e.hid=on&&!vis;if(e.img)e.img.style.display=vis?'':'none';if(e.ph&&e.ph.parentNode)e.ph.style.display=vis?'':'none'}if(chg){hiEvict();hiLegend()}}
/* decode queue: a tile's PNG is decoded off the main thread (HTMLImageElement.decode on the same blob URL — Blink shares the decoded frame by URL) before its <image> joins the SVG, at most DEC_MAX at a time, so a burst of 30 arriving tiles is spread over frames instead of landing in one paint. Without decode() the image is inserted at once (old behaviour). */
var dec={q:[],n:0,max:coarse?2:4};
function hiDecode(url,cb){if(!url||typeof Image==='undefined'||!('decode' in Image.prototype)){cb();return}dec.q.push([url,cb]);decPump()}
function decPump(){while(dec.n<dec.max&&dec.q.length)decOne(dec.q.shift())}
function decOne(j){dec.n++;var im=new Image(),done=function(){dec.n--;im=null;try{j[1]()}finally{decPump()}};im.src=j[0];im.decode().then(done,done)}
/* the legend is the picture's own key: the nine groups as they are mixed across the tiles on screen (of the LOD in use), so it is never a static key but a reading of the view */
/* sums over the tiles on screen of one view and the LOD in use: n cells, lu[9], unknown; obs count; chg confirmed/candidate + Δlu[9] */
function hiSum(v){v=v||'';var m={},first='',last='',n=0,unk=0,obs=0,conf=0,cand=0,dlu=[0,0,0,0,0,0,0,0,0],trees=0,tmax=0,th=[0,0,0,0,0],lod=hiLOD(),any=false,g,dif=[0,0,0],cmp=0,als0=0,als1=0,mix=0,km=0,rdn=0,rdc=[0,0,0,0],rmw=0,rmk=0;for(var k in hi.tiles){var e=hi.tiles[k];if(!e.on||!e.ok||e.t.lod!==lod||e.v!==v)continue;if(v==='chg'&&(e.win||'')!==tlStr())continue;any=true;if(e.m.km){km+=+e.m.km||0;rdn+=+e.m.rd||0;if(e.m.rdc){var r4=e.m.rdc.split(',');for(g=0;g<4;g++)rdc[g]+=+r4[g]||0}if(e.m.rm!=null&&e.m.rmb){var b4=e.m.rmb.split(','),bk=0;for(g=0;g<4;g++)bk+=+b4[g]||0;rmw+=(+e.m.rm||0)*bk;rmk+=bk}}if(e.m.dif){var d3=e.m.dif.split(',');for(g=0;g<3;g++)dif[g]+=+d3[g]||0;cmp+=+e.m.cmp||0;mix+=+e.m.mix||0;if(e.m.als){var a2=e.m.als.split(',');if(+a2[0]&&(!als0||+a2[0]<als0))als0=+a2[0];if(+a2[1]>als1)als1=+a2[1]}}n+=e.n;unk+=+e.m.unk||0;obs+=+e.m.obs||0;trees+=+e.m.trees||0;if(+e.m.tmax>tmax)tmax=+e.m.tmax;if(e.m.th){var t5=e.m.th.split(',');for(g=0;g<5;g++)th[g]+=+t5[g]||0}
  if(e.lu)for(g=0;g<9;g++)if(e.lu[g])m[GO[g]]=(m[GO[g]]||0)+e.lu[g];if(e.m.chg){var c=e.m.chg.split(',');conf+=+c[0]||0;cand+=+c[1]||0}if(e.m.dlu){var d=e.m.dlu.split(',');for(g=0;g<9;g++)dlu[g]+=+d[g]||0}if(e.m.at){var at=e.m.at.split(',');if(at[0]&&(!first||at[0]<first))first=at[0];if(at[1]&&at[1]>last)last=at[1]}}
 return any?{m:m,n:n,unk:unk,obs:obs,conf:conf,cand:cand,dlu:dlu,trees:trees,tmax:tmax,th:th,first:first,last:last,dif:dif,cmp:cmp,als:[als0,als1],mix:mix,km:km,rd:rdn,rdc:rdc,rm:rmk>0?rmw/rmk:-1}:null}
/* tallest tree per observed cell, five height bands (srtm tree_h_max, 0.5 m units) + the tallest in view */
var TH_L=['< 5 m','5\u201315 m','15\u201325 m','25\u201335 m','35 m +'];
/* trees on screen, from the apex tiles of the LOD in use (any profile): count, tallest, five height bands; null while none has answered */
function apSum(){var lod=hiLOD(),any=false,n=0,tmax=0,th=[0,0,0,0,0];for(var k in hi.tiles){var e=hi.tiles[k];if(e.v!=='apex'||!e.on||!e.ok||e.t.lod!==lod)continue;any=true;for(var i=0;i<e.tr.length;i++){var a=e.tr[i];if(a.x<vb.x||a.x>vb.x+vb.w||a.y<vb.y||a.y>vb.y+vb.h)continue;n++;var h=a.height_m||0;if(h*2>tmax)tmax=h*2;th[h<5?0:h<15?1:h<25?2:h<35?3:4]++}}return any?{n:n,trees:n,tmax:tmax,th:th,cells:n}:null}
function treeBar(o){var t=0,i;for(i=0;i<5;i++)t+=o.th[i];if(!t)return'';var s='<div class="pb" role="img" aria-label="trees by height band">',l=[];
 for(i=0;i<5;i++){if(!o.th[i])continue;s+=seg('g-tr'+i,o.th[i]/t);l.push('<span class="it">'+sw('g-tr'+i)+TH_L[i]+' <span class="mut">'+pct(o.th[i],t)+'</span></span>')}
 return'<div class="nt kv"><b>'+fmt(o.trees)+'</b> trees'+(o.cells===o.n?'<span class="sep"></span>by height':'<span class="sep"></span>tallest <b>'+(o.tmax/2).toFixed(1).replace(/\.0$/,'')+'\u202fm</b><span class="sep"></span>'+fmt(t)+' wooded cells')+'</div><div class="dx">'+s+'</div>'+legend(l,5)+'</div>'}
/* the in-view legend is a reading of the picture on screen: land use by group (+ unknown) · observed share · change confirmed/candidate with its Δ per group · collapsed cards name five groups, open ones all nine */
function lgMax(){return tip.classList.contains('open')||!tip.classList.contains('x')?9:5}
function hiLegendHTML(){var s=hiLegendHTML0();if(!s&&subVis())return'<div class="lg12">'+subLegendHTML()+'</div>';if(!s)return s;if(rd.on&&hi.on&&hiLOD()!==2)s=s.slice(0,-6)+rdLine()+'</div>';if(rl.on)s=s.slice(0,-6)+'<div class="nt mut rl">relief <i>hillshade, 25\u202fm terrain model \u00b7 {OBS_NAME}</i></div></div>';if(ot.on&&hi.on&&hiLOD()!==2)s=s.slice(0,-6)+'<div class="nt mut ot">photo <i>orthophoto \u00b7 basemap.at \u00b7 '+Math.round(ot.op*100)+'\u202f%</i></div></div>';if(subVis())s=s.slice(0,-6)+subLegendHTML()+'</div>';if(typeof fltLegend==='function'){var fl=fltLegend();if(fl)s=s.slice(0,-6)+fl+'</div>'}if(hi.tree)s=s.slice(0,-6)+treeBlock()+'</div>';return s}
function hiLegendHTML0(){if(!hi.on)return'';var s=hiSum(),o=view?hiSum(view):null,c=cg.on?hiSum('chg'):null,dd=df.on&&hiLOD()!==2?hiSum('dif'):null,hd=view==='obs'?'observed <i>dominant use per cell \u00b7 LiDAR &amp; satellite, {OBS_NAME}</i>':'declared <i>dominant use per cell</i>';
 var b='<div class="lg12"><div class="sh"><span>in view \u00b7 '+hd+'</span>'+(s?'<em>'+kfmt(s.n)+' cells</em>':'')+'</div>',body='',k;
 if(!s||(view&&!o)||(cg.on&&!c)||(df.on&&hiLOD()!==2&&!dd))return b+'<div class="nt mut">loading tiles'+ldots()+'</div></div>';
 if(!s.n)return b+'<div class="nt mut">nothing held here</div></div>';
 /* the profile block */
 if(view==='obs'){if(!o.obs)body+='<div class="nt mut">not observed yet \u00b7 the faint picture is the declared use</div>';else{var om={},k2;for(k2 in o.m)om[k2]=o.m[k2];if(o.unk)om.unknown=o.unk*255;
  body+=obsBar(om,'cov','observed land use in view',lgMax())+'<div class="nt mut dx">'+pct(o.obs,s.n)+' of the cells observed'+(o.obs<s.n?' \u00b7 the rest show their declared use, faint':'')+'</div>'}}
 else{var t=0;for(k in s.m)t+=s.m[k];if(!t&&!s.unk)body+='<div class="nt mut">no land use recorded here</div>';else{var m={};for(k in s.m)m[k]=s.m[k];if(s.unk)m.unknown=s.unk*255;body+=obsBar(m,'cov','declared land use in view',lgMax())}}
 /* overlay blocks, in z-order: change · (subsidy and relief are appended by hiLegendHTML) · trees */
 if(cg.on){var ch='<div class="sh ovh"><span>change <i>\u0394 declared use \u00b7 '+(tl.win?esc(tlLabel()):'between digests')+'</i></span>';if(!c.conf&&!c.cand)body+=ch+'</div><div class="nt mut">'+(tl.win?'nothing moved here on the '+esc(tlLabel()):'no change between the digests here')+'</div>';else{var tt=c.conf+c.cand,mv=chgMoved(c.dlu),when=perOf(c.first,c.last),isOpen=lgMax()>5,st=c.conf&&c.cand?fmt(c.conf)+' confirmed \u00b7 <span class="ca">\u25cf</span> '+fmt(c.cand)+' candidate':c.cand?'<span class="ca">\u25cf</span> all candidate, seen once':'all confirmed';
  body+=ch+'<em class="c">'+(when||fmt(tt)+' cells')+'</em></div>'+(mv?dluBar(c.dlu,'\u0394 declared use in view',lgMax()):'')+'<div class="nt mut"><span class="c">\u25cf</span> '+fmt(tt)+' cells'+(mv?' \u00b7 '+areaU(mv)+' redeclared':'')+(isOpen||!mv?'<span class="sep"></span>'+st:'')+(when&&c.first!==c.last&&isOpen?'<span class="sep"></span>sighted '+dayOf(c.first)+' \u2013 '+dayOf(c.last)+' '+c.last.slice(0,4):'')+'</div>'}}
 if(df.on)body+=difBlock(dd,s.n);
 return b+body+'</div>'}
/* the diff block of the in-view legend: three kinds, one hue, the mark names the kind; the lidar epoch as an age; the filter line toggles raw */
var DIF_L=['structures declared \u00b7 none in the LiDAR','structures in the LiDAR \u00b7 none declared','declared forest \u00b7 currently no trees'],DIF_T=['the register holds structures the flight did not see \u2014 most likely built after the flight','the LiDAR sees structures with no declaration, risen \u2265 2\u202fm between the two flights','declared forest with no tree cover today \u2014 a cut or a young stand (normal forestry; the register keeps it forest)'],DIF_K={declared_built_absent:0,built_undeclared:1,forest_declared_open:2};
function difSw(i){return i===0?sw('g-dif1'):i===1?sw('g-dif','ng-h4'):'<svg class="sw sw-ln" viewBox="0 0 10 10" aria-hidden="true"><path d="M5 1l3.5 2v4L5 9 1.5 7V3z" fill="none" stroke="var(--gdif)" stroke-width="1.8"/></svg>'}
function difAge(a){if(!a||!a[1])return'';var y=new Date().getUTCFullYear(),s=a[0]&&a[0]!==a[1]?'flown '+a[0]+'\u2013'+a[1]:'flown '+a[1],ag=y-a[1];return s+' \u00b7 '+(ag<=0?'this year':ag+'\u202fy ago')}
function difBlock(d,n){var hd='<div class="sh ovh"><span>diff <i>register vs LiDAR \u00b7 same cell, today</i></span>';if(hiLOD()===2)return hd+'</div><div class="nt mut">the diff comes with the near tiles \u00b7 zoom in</div>';if(!d)return hd+'</div><div class="nt mut">loading tiles'+ldots()+'</div>';
 var t=d.dif[0]+d.dif[1]+d.dif[2],age=difAge(d.als),li=[],i;for(i=0;i<3;i++)if(d.dif[i])li.push('<span class="it" title="'+DIF_T[i]+'">'+difSw(i)+DIF_L[i]+' <span class="mut">'+fmt(d.dif[i])+'</span></span>');
 var flt='<a class="raw" role="button" tabindex="0" data-raw="'+(df.raw?'0':'1')+'" title="'+(df.raw?'back to the cells the LiDAR itself corroborates (nDSM rise or drop, loss year, no apices)':'also the uncorroborated cells of these kinds \u2014 noisier')+'">'+(df.raw?'all cells of these kinds':'corroborated only')+'</a>';
 return hd+(age?'<em class="d">'+age+'</em>':'')+'</div>'+(t?legend(li,3):'<div class="nt mut">'+(d.cmp?'register and LiDAR agree here':'nothing to compare here yet')+'</div>')+'<div class="nt mut dx">'+(d.cmp?fmt(d.cmp)+' of '+fmt(n)+' cells compared':'')+(t?'<span class="sep"></span>'+fmt(t)+' marked':'')+'<span class="sep"></span>'+flt+(d.mix?'<span class="sep"></span>'+d.mix+' KG'+(d.mix>1?'s':'')+' with mixed flight years':'')+'</div>'}
/* the trees block of the in-view legend (last, the topmost layer): from the apex tiles on screen, any profile */
function treeBlock(){if(!hi.on||!hi.tree)return'';var ts=apSum(),tb=ts&&ts.n?treeBar(ts):'',body='';body+='<div class="sh ovh"><span>trees <i>crowns in view \u00b7 {OBS_NAME}</i></span>'+(tb?'<em class="o">'+(ts.tmax/2).toFixed(1).replace(/\.0$/,'')+'\u202fm tallest</em>':'')+'</div>'+(tb||'<div class="nt mut">'+(hiLOD()===2?'the trees come with the near tiles \u00b7 zoom in':ts?'no tree in view':'loading trees'+ldots())+'</div>');return body}
inner.addEventListener('click',function(e){var a=e.target.closest('a.raw');if(!a)return;e.preventDefault();e.stopPropagation();setDif(true,false,a.dataset.raw==='1')});
function hiLegend(){var o=inner.querySelector('.lg12');if(!hi.on&&!subVis()){if(o)o.parentNode.removeChild(o);return}if(!o){if(!picked&&floating())return;inner.insertAdjacentHTML('beforeend',hiLegendHTML());if(picked&&floating())place(picked);return}var d=document.createElement('div');d.innerHTML=hiLegendHTML();o.parentNode.replaceChild(d.firstChild,o);if(picked&&floating())place(picked)}
function hiView(now){if(!hi.on)return;var z=W/vb.w;if(z<HI_MIN&&!hi.fly){hiOff(false);return}hiCull();clearTimeout(hi.vT);hi.vT=setTimeout(function(){if(!hi.on)return;netTouch();var lod=hiLOD(),want=hiWant(lod);want.forEach(function(t){hiTile(t,'')});if(view)want.forEach(function(t){hiTile(t,view)});if(cg.on)want.forEach(function(t){hiTile(t,'chg')});if(df.on&&lod!==2)want.forEach(function(t){hiTile(t,'dif')});if(rd.on&&lod!==2)want.forEach(function(t){hiTile(t,'rd')});if(subVis()&&!sb.err){var sf=subFine();want.forEach(function(t){hiTile(t,subView(),sf)})}if(hi.tree&&lod!==2)want.forEach(function(t){hiApex(t)});hiCull();if(rl.on)relView();if(ot.on)otView();if(typeof fltView==='function')fltView(now)},now?0:110)}
/* the tree layer is the apices themselves, fetched per tile straight from {OBS_NAME} (CORS, no key): at LOD 1 the res-12 cell layer (/cells?bbox&layers=trees&format=columns&centres=1 — every apex ≥ 3 m with crown area, species and vitality, the fast path {MAP_NAME} uses), at LOD 0 the sampled giant index (/trees/bbox, the tallest few per ~0.1 ha, h ≥ 20 m).
   Drawn the way forestry-manager draws crowns: every tree is a hollow ring of its TRUE crown size (r = √(A/π), floor 2.4 px), hairline, so the picture underneath stays the information — the denser the stand, the thinner and fainter each ring (crowd = Σ circumference / area → f = 1/(1+(c/0.55)²)); a ring that stands alone gets a quiet glow. Always the light sprites, on every ground, so the layer reads the same everywhere. Colour = height, the legend's green ramp (lit, since it is added to the picture). The mass is one canvas per tile → <image>, re-rendered on zoom settle from the cached apices (never DOM per tree); only the picked and the flagged trees are SVG marks (#ng-tr). Hit test = nearest crown (≥ 7 px tap radius). srtm's open flags on the trees layer (its #flags) ride along ── */
var SRTM='{OBS_API}/api/v1',APEX=SRTM+'/trees/bbox',REP='';try{REP=localStorage.getItem('neg-rep')||'';if(!REP){REP=Math.random().toString(36).slice(2)+Date.now().toString(36);localStorage.setItem('neg-rep',REP)}}catch(e){}
function apBand(m){return m<5?0:m<15?1:m<25?2:m<35?3:4}
var AP_COL=['#f4fcea','#dcf2c4','#bfe7a3','#9fda86','#7ccb6e'];
function apSolo(c){return 1/(1+(c/0.55)*(c/0.55))}
function hiApex(t){var key='apex/'+t.k,e=hi.tiles[key];if(e){hi.order.splice(hi.order.indexOf(key),1);hi.order.push(key);if(e.ok)apNeed(e);return e}if(hi.bad[key]&&Date.now()-hi.bad[key]<NET_TTL)return null;delete hi.bad[key];
 e=hi.tiles[key]={t:t,v:'apex',img:null,ph:null,ok:false,n:0,lu:null,m:{},tr:[],fl:[],zr:0};hi.order.push(key);var L=hi.l.tree[t.lod],ns='http://www.w3.org/2000/svg',d=LOD[t.lod][0],g=document.createElementNS(ns,'g'),bb=(t.i*d).toFixed(4)+','+(t.j*d).toFixed(4)+','+((t.i+1)*d).toFixed(4)+','+((t.j+1)*d).toFixed(4);g.setAttribute('class','ap');g.dataset.k=key;L.appendChild(g);e.img=g;e.z0=(t.lod?HI_LOD1:HI_LOD0)*(svg.getBoundingClientRect().width||W)/W;
 var im=document.createElementNS(ns,'image');im.setAttribute('preserveAspectRatio','none');g.appendChild(im);e.im=im;e.mk=document.createElementNS(ns,'g');g.appendChild(e.mk);e.my=py(t.j*d)-py(t.j*d+1/111320);
 if(!hi.left)hi.t0=performance.now();hi.left++;hiStatus();
 var url=t.lod===1?SRTM+'/cells?bbox='+bb+'&layers=trees&format=columns&centres=1&fields=tree_n':APEX+'?bbox='+bb+'&limit=3000';
 fetch(url,{credentials:'omit',signal:hi.a.signal}).then(function(r){if(r.status===404&&t.lod===1)return null;if(!r.ok)throw 0;return r.json()}).then(function(j){if(!hi.tiles[key])return;var tr=[];
  if(t.lod===1){var c=j&&j.trees,n=c&&c.lon?c.lon.length:0,kg=j&&j.kg||[];for(var i=0;i<n;i++){if(c.h_m[i]<3)continue;tr.push({lon:c.lon[i],lat:c.lat[i],height_m:c.h_m[i],crown_m2:c.crown_m2?c.crown_m2[i]:null,species:c.species?c.species[i]:0,vitality:c.vitality?c.vitality[i]:0,kg_code:kg[c.cell_index[i]]||''})}}
  else tr=j&&j.trees||[];
  tr.forEach(function(a){a.x=px(a.lon);a.y=py(a.lat);var A=a.crown_m2!=null?a.crown_m2:a.crown_d_m?Math.PI*a.crown_d_m*a.crown_d_m/4:Math.PI*Math.pow(Math.max(2,a.height_m*0.2),2);a.rm=Math.sqrt(A/Math.PI);a.r=a.rm*e.my});e.tr=tr;e.n=tr.length;e.ok=true;hi.left--;hiStatus();apRender(e);requestAnimationFrame(function(){g.classList.add('in')});if(tr.length)apFlags(e,bb)}).catch(function(x){if(x&&x.name==='AbortError')return;hi.left=Math.max(0,hi.left-1);if(hi.tiles[key]===e)hiDrop(key);hiStatus()});return e}
/* re-render a tile when the zoom drifted > 20 % from the one it was drawn at (called on every settle via hiView → hiApex) */
window.__ngHi=function(){return hi};
function apNeed(e){var z=(svg.getBoundingClientRect().width||W)/vb.w;if(!e.zr||Math.abs(Math.log(z/e.zr))>0.18)apRender(e)}
function apRender(e){var t0=e.t,zpx=(svg.getBoundingClientRect().width||W)/vb.w,dpr=Math.min(2,window.devicePixelRatio||1),pad=14/zpx,t={x:t0.x-pad,y:t0.y-pad,w:t0.w+2*pad,h:t0.h+2*pad,k:t0.k},cw=t.w*zpx,ch=t.h*zpx,cap=Math.sqrt(3e6/Math.max(1,cw*ch*dpr*dpr));if(cap<1)dpr*=cap;e.zr=zpx;e.im.setAttribute('x',t.x);e.im.setAttribute('y',t.y);e.im.setAttribute('width',t.w);e.im.setAttribute('height',t.h);
 var c=e.cv||(e.cv=document.createElement('canvas')),Wc=Math.max(2,Math.ceil(cw*dpr)),Hc=Math.max(2,Math.ceil(ch*dpr));c.width=Wc;c.height=Hc;var g=c.getContext('2d');g.setTransform(dpr,0,0,dpr,0,0);g.clearRect(0,0,cw,ch);
 var sx=cw/t.w,sy=ch/t.h,ppm=sx*e.my,tr=e.tr,n=tr.length,i,a,r,circ=0;
 for(i=0;i<n;i++){a=tr[i];a.rp=Math.max(2.4,a.rm*ppm);circ+=2*Math.PI*a.rp}
 var crowd=circ/Math.max(1,cw*ch),f=apSolo(crowd),alpha=Math.min(0.92,0.55+0.4*f),lw=0.85+0.8*f,glow=Math.max(0,(f-0.5)/0.5);
 g.globalAlpha=alpha;
 if(glow>0){g.lineWidth=lw+3;for(var b=0;b<5;b++){g.strokeStyle=AP_COL[b];g.globalAlpha=alpha*0.22*glow;g.beginPath();for(i=0;i<n;i++){a=tr[i];if(apBand(a.height_m)!==b)continue;g.moveTo((a.x-t.x)*sx+a.rp,(a.y-t.y)*sy);g.arc((a.x-t.x)*sx,(a.y-t.y)*sy,a.rp,0,6.2832)}g.stroke()}g.globalAlpha=alpha}
 g.lineWidth=lw;for(var b=0;b<5;b++){g.strokeStyle=AP_COL[b];g.beginPath();for(i=0;i<n;i++){a=tr[i];if(apBand(a.height_m)!==b)continue;g.moveTo((a.x-t.x)*sx+a.rp,(a.y-t.y)*sy);g.arc((a.x-t.x)*sx,(a.y-t.y)*sy,a.rp,0,6.2832)}g.stroke()}
 g.globalAlpha=1;
 c.toBlob(function(bl){if(!bl||hi.tiles['apex/'+t.k]!==e)return;var u=URL.createObjectURL(bl);e.im.setAttribute('href',u);e.im.setAttributeNS('http://www.w3.org/1999/xlink','href',u);if(e.url)URL.revokeObjectURL(e.url);e.url=u},'image/png');apMarks(e)}
/* the few trees that are SVG: the picked one and the flagged ones (#ng-tr rings at the crown's size) */
function apMarks(e){var h='',k=1/e.z0,sel=hi.apx&&hi.apx.a;e.tr.forEach(function(a,i){var cl=a===sel?'sel':a.fl?(apTop(a.fl)==='looks_right'?'ok':'fl'):'';if(!cl)return;var rr=Math.max(a.r,7/e.zr);h+='<use href="#ng-tr" class="'+cl+'" data-i="'+i+'" transform="translate('+a.x.toFixed(3)+' '+a.y.toFixed(3)+') scale('+(rr/3.2).toFixed(5)+')"/>'});e.mk.innerHTML=h;if(sel&&hi.apx.e===e){var u=e.mk.querySelector('use.sel');if(u)hi.apx.el=u}}
/* open flags on srtm's trees layer inside the tile, matched to the apex nearest their cell (≤ 12 m — a res-12 cell is ~9 m across) */
function apFlags(e,bb){fetch(SRTM+'/flags?bbox='+bb+'&layer=trees&notes=0&limit=500',{credentials:'omit',signal:hi.a.signal}).then(function(r){return r.ok?r.json():null}).then(function(j){if(!j||!hi.tiles[e.t&&'apex/'+e.t.k])return;var rows=j.subjects||j.rows||j.flags||j.items||(Array.isArray(j)?j:[]);e.fl=rows;
  rows.forEach(function(f){if(f.lon==null)return;var best=-1,bd=12,cl=Math.cos(f.lat*Math.PI/180)*111320;e.tr.forEach(function(a,i){var dd=Math.hypot((a.lon-f.lon)*cl,(a.lat-f.lat)*111320);if(dd<bd){bd=dd;best=i}});if(best<0)return;e.tr[best].fl=f});apMarks(e)}).catch(function(){})}
function apTop(f){var c=f&&f.claims,b='',n=0;for(var k in c||{})if(c[k]>n){n=c[k];b=k}return b}
var CL={looks_right:'looks right',spurious:'not a tree',too_tall:'too tall',too_short:'too short',misplaced:'misplaced',wrong_height:'wrong height',missing:'missing',stale:'stale',artifact:'artefact',other:'other'};
/* nearest crown under the pointer: inside its ring, or within 7 px + a finger's slop; taller wins a tie */
function apAt(ev){var p=at(ev),zpx=(svg.getBoundingClientRect().width||W)/vb.w,slop=(ev.pointerType==='touch'?14:5)/zpx,best=null,bd=Infinity;for(var k in hi.tiles){var e=hi.tiles[k];if(e.v!=='apex'||!e.ok||!e.on)continue;var t=e.t,m=40/zpx;if(p[0]<t.x-m||p[0]>t.x+t.w+m||p[1]<t.y-m||p[1]>t.y+t.h+m)continue;
  for(var i=0;i<e.tr.length;i++){var a=e.tr[i],dd=Math.hypot(a.x-p[0],a.y-p[1]),rr=Math.max(a.r,7/zpx)+slop;if(dd>rr)continue;var sc=dd-a.height_m*0.002/zpx;if(sc<bd){bd=sc;best={a:a,e:e,el:null}}}}return best}
function apH(m){return m.toFixed(1).replace(/\.0$/,'')+'\u202fm'}
var SP=['','spruce','fir','pine','larch','beech','oak','maple','ash','birch','alder','poplar','willow','fruit tree','conifer','broadleaf'],VT=['','vital','normal','stressed','declining','dead'];
function apWhat(a){var w=[];if(a.species&&SP[a.species])w.push(SP[a.species]);if(a.vitality&&VT[a.vitality]&&a.vitality!==2)w.push(VT[a.vitality]);return w.join(' \u00b7 ')}
function apPeek(a){var f=a.fl,w=apWhat(a);return'<b>tree '+apH(a.height_m)+'</b><span class="mut"> \u00b7 '+(w||TH_L[apBand(a.height_m)])+(a.crown_d_m?' \u00b7 crown '+apH(a.crown_d_m):a.crown_m2?' \u00b7 crown '+Math.round(a.crown_m2)+'\u202fm\u00b2':'')+'</span>'+(f?'<div class="pl"><span class="c">\u2691</span> '+(f.n||1)+(f.n>1?' reports':' report')+' \u00b7 '+esc(CL[apTop(f)]||apTop(f))+'</div>':'')}
/* the tree card: the apex as srtm serves it, then the flag line — one quiet button that unfolds in place into the claims, a note and send; nothing leaves the card */
function apCard(a){var f=a.fl,nm=ixBy[a.kg_code];return'<div class="hd"><b>tree \u00b7 '+apH(a.height_m)+'</b><span class="age">apex \u00b7 {OBS_NAME}</span></div><div class="sub">'+(nm?esc(nm[1])+' \u00b7 ':'')+(a.kg_code?'KG '+a.kg_code+' \u00b7 ':'')+coordS(a.lon,a.lat)+'</div>'
 +'<div class="rw"><span class="lb">observed</span><span class="vl">'+sw('g-tr'+apBand(a.height_m))+TH_L[apBand(a.height_m)]+(apWhat(a)?'<span class="sep"></span>'+apWhat(a):'')+(a.crown_d_m?'<span class="sep"></span>crown '+apH(a.crown_d_m):a.crown_m2?'<span class="sep"></span>crown '+Math.round(a.crown_m2)+'\u202fm\u00b2':'')+(a.confidence!=null?'<span class="sep"></span><span class="mut">confidence '+Math.round(a.confidence*100)+'\u202f%</span>':'')+'</span></div>'
 +'<div class="dt"><div class="sh"><span>observed <i>LiDAR nDSM apex \u00b7 {OBS_NAME}</i></span>'+(f?'<em class="c" title="open reports on this tree">\u2691 '+(f.n||1)+'</em>':'')+'</div><div class="nt mut">'+(a.crown_m2!=null?'every apex \u2265 3\u202fm of the res-12 cell layer; every ring is a crown at its measured size':'the tallest few apices per \u22480.1\u202fha with h \u2265 20\u202fm, QA-filtered \u2014 the overview sample')+'</div></div>'+apFlagHTML(a)}
function coordS(lon,lat){return lat.toFixed(5)+', '+lon.toFixed(5)}
function apFlagHTML(a){var g=a.fg||{},f=a.fl,s='<div class="fg'+(g.open?' on':'')+(g.done?' done':'')+'">';
 if(g.done)return s+'<span class="fg-ok">\u2713 flagged \u00b7 '+esc(CL[g.claim]||g.claim)+'</span><span class="mut"> \u00b7 '+(g.done.n>1?g.done.n+' reports now':'thanks')+(g.done.folded?' \u00b7 folded into your earlier one':'')+'</span></div>';
 if(!g.open)return s+'<button type="button" class="fg-b" title="tell {OBS_NAME} this tree looks wrong \u2014 or right">\u2691 flag</button>'+(f?'<span class="mut"> \u00b7 '+(f.n||1)+(f.n>1?' reports':' report')+' \u00b7 '+esc(CL[apTop(f)]||apTop(f))+'</span>':'<span class="mut"> \u00b7 wrong, or right? one tap tells {OBS_NAME}</span>')+'</div>';
 var cs=['looks_right','spurious','too_tall','too_short','misplaced'];s+='<div class="fg-r"><span class="fg-q">\u2691</span>';cs.forEach(function(c){s+='<button type="button" class="fg-c" data-c="'+c+'" aria-pressed="'+(g.claim===c?'true':'false')+'">'+CL[c]+'</button>'});s+='<button type="button" class="fg-x" aria-label="close">\u00d7</button></div>';
 if(g.claim)s+='<div class="fg-r fg-n"><input type="text" maxlength="280" placeholder="what did you see? (optional, public)" value="'+esc(g.note||'')+'" aria-label="note" autocomplete="off"><button type="button" class="fg-s"'+(g.busy?' disabled':'')+'>'+(g.busy?'sending\u2026':'send')+'</button>'+(g.err?'<span class="fg-e">'+esc(g.err)+'</span>':'')+'</div>';
 return s+'</div>'}
function apFlagSwap(a){var o=inner.querySelector('.fg');if(!o)return;o.insertAdjacentHTML('afterend',apFlagHTML(a));o.parentNode.removeChild(o);var i=inner.querySelector('.fg-n input');if(i&&a.fg.focus){a.fg.focus=false;i.focus({preventScroll:true})}if(floating())place(picked)}
function apSend(a){var g=a.fg;if(!g||!g.claim||g.busy)return;g.busy=true;g.err='';apFlagSwap(a);var body={lon:+a.lon.toFixed(7),lat:+a.lat.toFixed(7),layer:'trees',claim:g.claim,source:'umfeld-at/glyph',observed:{h_m:a.height_m,crown_d_m:a.crown_d_m||undefined,crown_m2:a.crown_m2||undefined,species:a.species?SP[a.species]:undefined,confidence:a.confidence}};if(g.note)body.note=g.note;if(REP)body.reporter=REP;if(g.claim==='spurious')body.expected={absent:true};
 fetch(SRTM+'/flags',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)}).then(function(r){return r.json().then(function(j){if(!r.ok)throw new Error(j&&j.error||('HTTP '+r.status));return j})}).then(function(j){g.busy=false;g.open=false;g.done={n:j.tally&&j.tally.n||j.n||1,folded:!!j.folded};if(!a.fl)a.fl={claims:{}};a.fl.claims[g.claim]=(a.fl.claims[g.claim]||0)+1;a.fl.n=(a.fl.n||0)+1;if(hi.apx&&hi.apx.a===a)apMarks(hi.apx.e);apFlagSwap(a)}).catch(function(x){g.busy=false;g.err=x&&x.message&&x.message.indexOf('429')>=0?'too many for now \u2014 try later':'could not send';apFlagSwap(a)})}
function apPin(h){if(hi.apx&&hi.apx.a===h.a){apUnpin();return}apUnpin(true);hi.pin=null;hi.hov=null;hi.apx=h;apMarks(h.e);var a=h.a,r=[a.x,a.y,0,0,0,-1,0,0,null,[],0.13];r.k='ap_'+a.lon+'_'+a.lat;r.bb=[a.lon,a.lat,a.lon,a.lat];picked=r;shown=-1;pk={t:'tree',a:a};unpeek();reveal(r,apCard(a),false);cur.setAttribute('d',circD(a.x,a.y,Math.max(a.r,7/h.e.zr)*1.9));cur.classList.add('cap');curAt=[a.x,a.y];curI=-1;svg.classList.add('on')}
function apUnpin(quiet){if(!hi.apx)return;var ee=hi.apx.e;hi.apx=null;apMarks(ee);cur.classList.remove('cap');if(quiet)return;picked=null;shown=-1;pk=null;hide()}
tip.addEventListener('click',function(e){var b=e.target.closest('.fg-b,.fg-c,.fg-x,.fg-s');if(!b||!pk||pk.t!=='tree')return;e.stopPropagation();var a=pk.a,g=a.fg=a.fg||{};
 if(b.classList.contains('fg-b')){g.open=true}else if(b.classList.contains('fg-x')){g.open=false;g.claim=''}else if(b.classList.contains('fg-c')){g.claim=b.dataset.c;g.focus=true}else if(b.classList.contains('fg-s')){var i=inner.querySelector('.fg-n input');g.note=i?i.value.trim():'';apSend(a);return}apFlagSwap(a)});
tip.addEventListener('keydown',function(e){var i=e.target.closest&&e.target.closest('.fg-n input');if(!i||!pk||pk.t!=='tree')return;if(e.key==='Enter'){e.preventDefault();pk.a.fg.note=i.value.trim();apSend(pk.a)}else if(e.key==='Escape'){e.preventDefault();e.stopPropagation();pk.a.fg.open=false;pk.a.fg.claim='';apFlagSwap(pk.a)}});
/* ── trees: srtm's tree apices as rings over either profile (see hiApex); toggled from the view bar or the tree line of the legend, kept in the hash as ;trees ── */

function setTree(on,quiet){on=!!on;if(on===hi.tree)return;hi.tree=on;if(!on)apUnpin();svg.classList.toggle('v-tree',on);fig.classList.toggle('trees',on);var gvt=gv.querySelector('.gv-tree');if(gvt){gvt.setAttribute('aria-pressed',on?'true':'false');gvt.title=on?'take the trees off the map':'trees \u00b7 every tree as a ring of its crown, greener = taller (full resolution) ( t )';if(gvt.classList.contains('off')){gvt.dataset.t0=ttl(gvt);gvt.title=AVAIL.trees}}dbgLog('trees '+(on?'on':'off'));if(quiet)return;if(hi.on)hiView(true);hiLegend();if(picked&&pk&&pk.t==='h3')recard();setHash();
 if(on&&!setTree.h){setTree.h=1;showHint('every ring is one tree\u2019s crown at its real size \u00b7 greener = taller \u00b7 hover for the height, tap to flag')}}

/* ── subsidy: INVEKOS Schläge (AMA) per cell from {SUB_NAME} — overview: res-7 cov/cls/bio/dcov in cells[] order (/ne/glyph/sub, fetched once on the first toggle), ochre .s dots / hex faces by cov bucket under the ink; full resolution: v=sub tiles (LOD 0/1 each Schlag as drawn from the peer's IVP1 outline bins, hatched = dubious outline, gloez = outline only; LOD 2 the res-7 wash) under declared, v=subc lineage fills (new full, redrawn half, gone hatched + dotted edge; hexagon fallback when an outline bin is missing) under change. One hue (--gsub, Okabe-Ito orange); class, bio and lineage are named in legend and card, never coloured. Hash ;subsidy · key s */
var sb={on:false,d:null,ld:null,err:false,built:false,lt:0},SUB_CL=['none','acker','gruenland','alm','wein','dauerkultur','gloez','sonst'],SUB_CT={acker:'arable land (Acker)',gruenland:'grassland (Grünland)',alm:'alpine pasture (Alm)',wein:'vineyard',dauerkultur:'permanent crops',gloez:'landscape elements (GLÖZ: hedges, verges)',sonst:'other'},SUB_HA=307/10000,SUB_HA7=516.1,SUB_B=[64,128,192,256],SUB_A=[.3,.5,.7,.9];
var SUB_ATTR='';/* attribution lives in the footer and /api/v1/license (like srtm), not in the card */
function subBucket(c){return c>=192?3:c>=128?2:c>=64?1:0}
function subLoad(){if(sb.d||sb.ld)return sb.ld;subStatus(true);sb.ld=fetch('/api/v1/ne/glyph/sub',{credentials:'omit'}).then(function(r){if(!r.ok)throw 0;return r.json()}).then(function(d){sb.d=d;sb.err=false;sb.ld=null;subBuild();subStatus(false);hiLegend();if(picked)recard();if(hi.on)hiView(true);return d}).catch(function(){sb.ld=null;sb.err=true;subStatus(false);hiLegend();showHint('subsidy layer unavailable \u00b7 {SUB_NAME} is not answering');setTimeout(function(){sb.err=false},30000)});return sb.ld}
/* the overview marks: four cov buckets as .s paths, inserted under the ink (after .e) in the dot group and in every hex tile */
function subBuild(){if(sb.built||!sb.d||!cells.length)return;sb.built=true;var cov=sb.d.cov,b=[[],[],[],[]],i,s='';for(i=0;i<cells.length;i++)if(cov[i])b[subBucket(cov[i])].push('M'+cells[i][0]+' '+cells[i][1]+'h0');
 for(i=0;i<4;i++)if(b[i].length)s+='<path class="s" opacity="'+SUB_A[i]+'" d="'+b[i].join('')+'"/>';var e=dots.querySelector('.e');if(e)e.insertAdjacentHTML('afterend',s);else dots.insertAdjacentHTML('afterbegin',s);subHex()}
function subHex(){if(!sb.d||!hexG||hexG.dataset.sub)return;hexG.dataset.sub=1;var cov=sb.d.cov,T=560,m={},i,k;for(i=0;i<cells.length;i++){if(!cov[i])continue;var c=cells[i];k=((c[0]/T)|0)+'_'+((c[1]/T)|0);var t=m[k];if(!t)t=m[k]=[[],[],[],[]];t[subBucket(cov[i])].push(faceD(i))}
 [].forEach.call(hexG.children,function(g){var b=g.dataset.b.split(' ').map(Number),t=m[(((b[0]+20)/T)|0)+'_'+(((b[1]+20)/T)|0)];if(!t)return;var s='';for(var j=0;j<4;j++)if(t[j].length)s+='<path class="s" fill-opacity="'+SUB_A[j]+'" d="'+t[j].join('')+'"/>';var e=g.querySelector('.e');if(e)e.insertAdjacentHTML('afterend',s);else g.insertAdjacentHTML('afterbegin',s)})}
function subStatus(ld){var busy=ld||(subVis()&&hi.on&&subBusy());[].forEach.call(gv.querySelectorAll('.gv-sub'),function(b){b.classList.toggle('busy',!!busy)})}
function subBusy(){for(var k in hi.tiles){var e=hi.tiles[k];if(!e.ok&&(e.v==='sub'||e.v==='subc'))return true}return false}
function subView(){return cg.on?'subc':'sub'}
/* the layer paints over either profile: the claims wash / Schläge as drawn, or — with the change overlay on — the lineage vs the year before (subc) */
function subVis(){return sb.on}
function subSync(){svg.classList.toggle('v-sub',subVis());var b=gv.querySelector('.gv-sub');if(b){b.setAttribute('aria-pressed',sb.on?'true':'false');b.title=sb.on?'take the subsidy claims off the map ( s )':cg.on?'subsidy claims vs 2024 \u00b7 INVEKOS Schl\u00e4ge new, redrawn or gone ( s )':'subsidy claims \u00b7 INVEKOS Schl\u00e4ge 2025 as drawn, in the overview darker = more of the cell claimed ( s )'}}
function setSub(on,quiet){on=!!on;if(on===sb.on)return;dbgLog('subsidy '+(on?'on':'off'));sb.on=on;subSync();
 if(on){subLoad();if(hi.on)hiView(true)}else subStatus(false);hiLegend();if(picked)recard();setHash();if(on&&!quiet&&!setSub.h){setSub.h=1;showHint(hi.on?(cg.on?'ochre = subsidy claims that changed vs the year before, each Schlag as drawn \u00b7 full new, half redrawn, hatched gone':'ochre = land claimed for farm subsidies, each Schlag as drawn \u00b7 hatched = dubious outline'):'ochre = share of the cell claimed for farm subsidies (INVEKOS) \u00b7 darker = more')}}
/* sums of the subsidy tiles on screen (the LOD in use): claimed cells / share, class shares cov-weighted, bio, dubious, lineage counts, Δ claimed; down = the peer did not answer */
function subSum(){if(!subVis()||!hi.on)return null;var v=subView(),lod=hiLOD(),o={n:0,cov:0,cls:[0,0,0,0,0,0,0,0],bio:0,bunk:0,dub:0,chg:[0,0,0,0,0,0],dcov:0,cells:0,year:0,prev:0,rel:'',down:false,any:false,wait:false},k,g;
 for(k in hi.tiles){var e=hi.tiles[k];if(!e.on||e.t.lod!==lod||e.v!==v||(lod===2&&!!e.f!==subFine()))continue;if(!e.ok){o.wait=true;continue}o.any=true;if(e.f)o.fine=true;if(e.down){o.down=true;continue}var m=e.m;o.n+=+m.sub||0;o.cov+=+m.cov||0;o.dub+=+m.dub||0;o.dcov+=+m.dcov||0;o.cells+=+m.cells||0;if(m.cls){var c8=m.cls.split(',');for(g=0;g<8;g++)o.cls[g]+=+c8[g]||0}if(m.bio){var b2=m.bio.split(',');o.bio+=+b2[0]||0;o.bunk+=+b2[1]||0}if(m.chg&&m.sub!==undefined){var c6=m.chg.split(',');for(g=0;g<6;g++)o.chg[g]+=+c6[g]||0}if(m.year)o.year=+m.year;if(m.prev)o.prev=+m.prev;if(m.rel)o.rel=m.rel;if(m.pn!==undefined){o.poly=true;o.pn=(o.pn||0)+(+m.pn);o.pha=(o.pha||0)+(+m.pha||0);o.pnew=(o.pnew||0)+(+m.pnew||0);o.pgone=(o.pgone||0)+(+m.pgone||0);if(!o.pchg)o.pchg=[0,0,0,0,0,0];if(m.pchg){var p6=m.pchg.split(',');for(g=0;g<6;g++)o.pchg[g]+=+p6[g]||0}}if(m.chg7!==undefined){o.c7n=(o.c7n||0)+(+m.chg7)*(+m.cov||1);o.c7d=(o.c7d||0)+(+m.cov||1)}}if(o.c7d)o.chg7=Math.round(o.c7n/o.c7d);
 return o}
/* the overview sums: the res-7 cells inside the view box (cov in 1/255 of a cell) */
function subSumOv(idx){if(!subVis()||!sb.d)return null;var d=sb.d,o={n:0,cov:0,cls:[0,0,0,0,0,0,0,0],bio:0,bunk:0,dcov:0,chg:0,cells:0,year:d.year,prev:d.prev_year,rel:d.release},i,m=idx?idx.length:cells.length;for(var q=0;q<m;q++){i=idx?idx[q]:q;var c=cells[i];if(!idx&&(c[0]<vb.x||c[0]>vb.x+vb.w||c[1]<vb.y||c[1]>vb.y+vb.h))continue;o.cells++;var cv=d.cov[i];if(!cv)continue;o.n++;o.cov+=cv;if(d.shr&&d.shr[i]){var sh=d.shr[i]/255;o.cls[d.cls[i]&7]+=cv*sh;o.cls[(d.cls2?d.cls2[i]:0)&7]+=cv*(1-sh)}else o.cls[d.cls[i]&7]+=cv;if(d.bio[i]===255)o.bunk++;else o.bio+=cv*d.bio[i]/255;o.dcov+=d.dcov[i];if(d.chg)o.chg+=cv*d.chg[i]/255}return o}
function subSw(k,fill){var h='M5 1l3.5 2v4L5 9 1.5 7V3z';return'<svg class="sw sw-ln" viewBox="0 0 10 10" aria-hidden="true"><path d="'+h+'" fill="'+(fill?'currentColor':'none')+'"'+(fill?' fill-opacity="'+(k==='new'?.64:k==='redrawn'?.47:.39)+'"':'')+' stroke="currentColor" stroke-width="'+(k==='new'?2:1)+'"'+(k==='gone'?' stroke-dasharray="1.2 1.2"':k==='redrawn'&&!fill?' opacity=".6"':'')+'/></svg>'}
/* hectares from m² as text, signed */
function subHaM2(m2,sign){var h=Math.abs(m2)/1e4,t=(h<10?h.toFixed(1):fmt(Math.round(h)))+'\u202fha';return sign?(m2>0?'+':m2<0?'\u2212':'\u00b1')+t:t}
function subClsBar(cls,lead,max){var t=0,i,l=[],s='';for(i=1;i<8;i++)t+=cls[i];if(!t)return'';var ks=[];for(i=1;i<8;i++)if(cls[i])ks.push(i);ks.sort(function(a,b){return cls[b]-cls[a]});
 s='<div class="pb" role="img" aria-label="subsidy claims by class">';for(i=0;i<ks.length;i++){s+=seg('g-s'+ks[i],cls[ks[i]]/t);l.push(item('g-s'+ks[i],null,SUB_CL[ks[i]],pct(cls[ks[i]],t),SUB_CT[SUB_CL[ks[i]]]))}return s+'</div>'+legend(l,max||5,lead)}
function subLegendHTML(){if(!subVis())return'';var hd='<div class="sh"><span>subsidy <i>INVEKOS claims per cell'+(hi.on&&cg.on?' \u00b7 vs the year before':'')+'</i></span>',ha=function(dc,per){var h=dc/255*(per||SUB_HA);return(h>0?'+':h<0?'\u2212':'\u00b1')+(Math.abs(h)<10?Math.abs(h).toFixed(1):fmt(Math.round(Math.abs(h))))+'\u202fha'};
 if(sb.err)return'<div class="sb">'+hd+'</div><div class="nt mut">subsidy layer unavailable \u00b7 {SUB_NAME} is not answering</div>'+SUB_ATTR+'</div>';
 if(!hi.on){var v=subSumOv();if(!v)return'<div class="sb">'+hd+'</div><div class="nt mut">loading the subsidy overview'+ldots()+'</div></div>';if(!v.cells||!v.cov)return'<div class="sb">'+hd+'</div><div class="nt mut">no subsidy claims in view</div>'+SUB_ATTR+'</div>';
  return'<div class="sb">'+hd+'<em class="s">'+pct(v.cov,v.cells*255)+' claimed</em></div><div class="nt kv"><b>'+pct(v.cov,v.cells*255)+'</b> of the land in view claimed ('+v.year+(v.rel?'-'+String(v.rel).replace(/^\d{4}-?/,''):'')+')<span class="sep"></span>'+(v.bunk===v.n?'bio unknown':'bio <b>'+pct(v.bio,v.cov)+'</b>')+'<span class="sep"></span>'+(cg.on?'<b>'+pct(v.chg,v.cov)+'</b> of the claimed area moved vs '+v.prev:'\u0394 vs '+v.prev+' '+ha(v.dcov,SUB_HA7))+'</div>'+subClsBar(v.cls,'',lgMax())+SUB_ATTR+'</div>'}
 var o=subSum();if(!o||(!o.any&&o.wait))return'<div class="sb">'+hd+'</div><div class="nt mut">loading subsidy tiles'+ldots()+'</div></div>';if(o.down&&!o.n)return'<div class="sb">'+hd+'</div><div class="nt mut">subsidy layer unavailable \u00b7 {SUB_NAME} is not answering</div>'+SUB_ATTR+'</div>';
 if(cg.on){var tt=o.chg[0]+o.chg[1]+o.chg[2]+o.chg[3]+o.chg[4]+o.chg[5];if(hiLOD()===2&&!o.fine)return'<div class="sb">'+hd+'<em class="s">claims vs '+o.prev+'</em></div><div class="nt kv">'+(o.chg7!=null?'<b>'+o.chg7+'\u202f%</b> of the claimed area moved':'nothing moved')+'<span class="sep"></span>\u0394 claimed <b>'+ha(o.dcov)+'</b></div><div class="nt mut dx">darker = more of the claimed area moved \u00b7 the outlines of new / gone / redrawn claims come with the near tiles</div>'+SUB_ATTR+'</div>';if(!tt&&!o.pn)return'<div class="sb">'+hd+'</div><div class="nt mut">no subsidy claims in view</div>'+SUB_ATTR+'</div>';var rd=o.chg[1]+o.chg[2]+o.chg[3];
  if(o.poly){var pc=o.pchg,pt=pc[0]+pc[1]+pc[2]+pc[3]+pc[4]+pc[5],prd=pc[1]+pc[2]+pc[3],pl=[];if(!pt)return'<div class="sb">'+hd+'<em class="s">claims vs '+o.prev+'</em></div><div class="nt mut">no subsidy claims in view</div>'+SUB_ATTR+'</div>';
   if(pc[4])pl.push('<span class="it s" title="claimed this year, not the year before \u00b7 '+subHaM2(o.pnew)+'">'+subSw('new',1)+'new <span class="mut">'+fmt(pc[4])+' \u00b7 '+subHaM2(o.pnew,1)+'</span></span>');if(pc[5])pl.push('<span class="it s" title="claimed the year before, not this year \u00b7 '+subHaM2(o.pgone)+'">'+subSw('gone',1)+'gone <span class="mut">'+fmt(pc[5])+' \u00b7 '+subHaM2(-o.pgone,1)+'</span></span>');if(prd)pl.push('<span class="it s" title="outline redrawn: modified '+fmt(pc[1])+', split / merged '+fmt(pc[2])+', partial '+fmt(pc[3])+'">'+subSw('redrawn',1)+'redrawn <span class="mut">'+fmt(prd)+'</span></span>');pl.push('<span class="it">same <span class="mut">'+fmt(pc[0])+'</span></span>');
   return'<div class="sb">'+hd+'<em class="s">claims vs '+o.prev+'</em></div><div class="nt kv"><b>'+fmt(pc[4]+pc[5]+prd)+' of '+fmt(pt)+'</b> Schl\u00e4ge moved<span class="sep"></span>\u0394 claimed <b>'+ha(o.dcov)+'</b></div>'+legend(pl,4)+'<div class="nt mut dx">each Schlag as drawn \u00b7 full = new, half = redrawn, hatched = gone</div>'+SUB_ATTR+'</div>'}
  var li=[];if(o.chg[4])li.push('<span class="it s" title="claimed this year, not the year before">'+subSw('new')+'new <span class="mut">'+fmt(o.chg[4])+'</span></span>');if(o.chg[5])li.push('<span class="it s" title="claimed the year before, not this year">'+subSw('gone')+'gone <span class="mut">'+fmt(o.chg[5])+'</span></span>');if(rd)li.push('<span class="it s" title="outline redrawn: modified '+fmt(o.chg[1])+', split / merged '+fmt(o.chg[2])+', partial '+fmt(o.chg[3])+'">'+subSw('redrawn')+'redrawn <span class="mut">'+fmt(rd)+'</span></span>');li.push('<span class="it">same <span class="mut">'+fmt(o.chg[0])+'</span></span>');
  return'<div class="sb">'+hd+'<em class="s">claims vs '+o.prev+'</em></div><div class="nt kv"><b>'+pct(o.chg[4]+o.chg[5]+rd,tt)+'</b> of the claimed cells moved<span class="sep"></span>\u0394 claimed <b>'+ha(o.dcov)+'</b></div>'+legend(li,4)+'<div class="nt mut dx">outlines of each run of cells, the Schlag\u2019s shape</div>'+SUB_ATTR+'</div>'}
 if(!o.n)return'<div class="sb">'+hd+'</div><div class="nt mut">no subsidy claims in view</div>'+SUB_ATTR+'</div>';
 return'<div class="sb">'+hd+'<em class="s">'+pct(o.cov,o.cells*255)+' claimed</em></div><div class="nt kv"><b>'+pct(o.cov,o.cells*255)+'</b> of the land in view claimed ('+o.year+(o.rel?'-'+String(o.rel).replace(/^\d{4}-?/,''):'')+')<span class="sep"></span>'+(o.bunk>=o.n?'bio unknown':'bio <b>'+pct(o.bio,o.cov)+'</b>')+'<span class="sep"></span>dubious drawing <b>'+pct(o.dub,o.n)+'</b>'+(o.poly?'<span class="sep"></span><b>'+fmt(o.pn)+'</b> Schl\u00e4ge'+(o.pha?' \u00b7 '+subHaM2(o.pha):''):'')+'</div>'+subClsBar(o.cls,'',lgMax())+SUB_ATTR+'</div>'}
/* one line in the cards: res-12 cell (from the bin, via /ne/glyph/cell?sub=1), res-7 dot (from the overview arrays), KG (sum over its res-7 cells) */
/* the subsidy line of an area card: the overview roll-up over the area's cells (claimed share, bio, moved / Δ) */
function subAreaLine(idx){if(!subVis())return'';if(sb.err)return subRow('<span class="mut">subsidy layer unavailable</span>');var v=subSumOv(idx);if(!v)return subRow('<span class="mut">loading'+ldots()+'</span>');if(!v.cov)return subRow('<span class="mut">no claims ('+v.year+')</span>');
 var dh=v.dcov/255*SUB_HA7;return subRow(sw('g-sub')+'claimed <b>'+pct(v.cov,v.cells*255)+'</b> of the area ('+v.year+')<span class="sep"></span>'+(v.bunk===v.n?'bio unknown':'bio '+pct(v.bio,v.cov))+'<span class="sep"></span>'+(cg.on&&sb.d.chg?pct(v.chg,v.cov)+' moved vs '+v.prev:'\u0394 vs '+v.prev+' '+(dh>0?'+':dh<0?'\u2212':'\u00b1')+(Math.abs(dh)<10?Math.abs(dh).toFixed(1):fmt(Math.round(Math.abs(dh))))+'\u202fha'))}
function subRow(html){return'<div class="rw"><span class="lb">subsidy</span><span class="vl">'+html+'</span></div>'}
function subFetchCell(d,cb){if(d.subQ)return;d.subQ=1;fetch('/api/v1/ne/glyph/cell?cell='+d.cell+'&sub=1',{credentials:'omit'}).then(jsonOf).then(function(x){d.sub=x&&x.sub!==undefined?x.sub:null;if(x&&x.sub_unavailable)d.sub_unavailable=true}).catch(function(){d.sub_unavailable=true}).then(function(){if(pk&&pk.t==='h3'&&pk.d===d)recard();if(cb)cb()})}
function subCellLine(d){if(!subVis())return'';var s=d.sub;if(s===undefined&&!d.sub_unavailable)subFetchCell(d);if(d.sub_unavailable)return subRow('<span class="mut">subsidy layer unavailable</span>');if(s===undefined)return subRow('<span class="mut">loading'+ldots()+'</span>');if(!s||!s.claimed)return subRow('<span class="mut">not claimed ('+(s?s.year:'')+')</span>');
 var p=[sw('g-sub')+'claimed <b>'+s.claimed_pct+'\u202f%</b>',SUB_CL[SUB_CL.indexOf(s.class)>0?SUB_CL.indexOf(s.class):0]];if(s.bio===true)p.push('bio');else if(s.bio===null)p.push('<span class="mut">bio unknown</span>');p.push(s.reliable?'<span class="mut">drawn reliably</span>':'dubious drawing');var vp={same:'same',new:'new',gone:'gone',modified:'redrawn',split_merged:'split / merged',partial:'partly redrawn'}[s.vs_prev]||s.vs_prev;p.push('vs '+s.prev_year+': <b>'+vp+'</b>');var sg=s.schlag;if(sg)p.push(subSchlagTxt(sg));return subRow(p.join('<span class="sep"></span>'))}
/* the Schlag under the cell as drawn (from the outline bin): area, and vs the year before its own lineage + Δ area */
function subSchlagTxt(sg){var t='Schlag <b>'+(sg.vs_prev==='gone'?subHaM2(Math.round((sg.prev_area_ha||0)*1e4))+' gone':subHaM2(Math.round(sg.area_ha*1e4)))+'</b>';if(sg.d_area_ha)t+=' <span class="mut">('+subHaM2(Math.round(sg.d_area_ha*1e4),1)+')</span>';if(sg.clipped)t+=' <span class="mut">(clipped)</span>';return t}
function subDotLine(i){if(!subVis())return'';if(sb.err)return subRow('<span class="mut">subsidy layer unavailable</span>');if(!sb.d)return subRow('<span class="mut">loading'+ldots()+'</span>');var d=sb.d,cv=d.cov[i];if(!cv)return subRow('<span class="mut">not claimed ('+d.year+')</span>');var c=d.cls[i]&7,c2=d.cls2?d.cls2[i]&7:0,sh=d.shr?d.shr[i]:0,cl=c?SUB_CL[c]+(sh?' <span class="mut">'+pct(sh,255)+'</span>':''):'';if(c2&&sh)cl+='<span class="sep"></span>'+SUB_CL[c2]+' <span class="mut">'+pct(255-sh,255)+'</span>';return subRow(sw('g-sub')+'claimed <b>'+pct(cv,255)+'</b> of the cell<span class="sep"></span>'+cl+(d.bio[i]!==255&&d.bio[i]?'<span class="sep"></span>bio '+pct(d.bio[i],255):'')+(cg.on&&d.chg?'<span class="sep"></span>'+pct(d.chg[i],255)+' moved vs '+d.prev_year:d.dcov[i]?'<span class="sep"></span>'+(d.dcov[i]>0?'+':'\u2212')+pct(Math.abs(d.dcov[i]),255)+' vs '+d.prev_year:''))}
function subKGLine(idx){if(!subVis())return'';if(sb.err)return subRow('<span class="mut">subsidy layer unavailable</span>');if(!sb.d)return subRow('<span class="mut">loading'+ldots()+'</span>');var d=sb.d,cov=0,cls=[0,0,0,0,0,0,0,0],i;for(i=0;i<idx.length;i++){var j=idx[i],cv=d.cov[j];if(!cv)continue;cov+=cv;if(d.shr&&d.shr[j]){cls[d.cls[j]&7]+=cv*d.shr[j]/255;cls[(d.cls2?d.cls2[j]:0)&7]+=cv*(255-d.shr[j])/255}else cls[d.cls[j]&7]+=cv}if(!cov)return subRow('<span class="mut">not claimed ('+d.year+')</span>');var ks=[];for(i=1;i<8;i++)if(cls[i])ks.push(i);ks.sort(function(a,b){return cls[b]-cls[a]});var p=[sw('g-sub')+'claimed <b>'+pct(cov,idx.length*255)+'</b> of its cells'];for(i=0;i<Math.min(2,ks.length);i++)p.push(SUB_CL[ks[i]]+' <span class="mut">'+pct(cls[ks[i]],cov)+'</span>');return subRow(p.join('<span class="sep"></span>'))}
/* ── relief: {OBS_NAME}'s hillshade XYZ tiles (25 m DTM, z 10–17, CC BY 4.0, 1-year immutable cache) blended over the picture in full resolution, like {MAP_NAME} draws its ground — one tile per Web-Mercator square placed by its lon/lat corners (the glyph is plate carrée; across one tile the difference is below a pixel). The hillshade reads through the cells with the overlay blend (mid grey = untouched, lit slopes lighten, shaded slopes darken), its weight full through the near tiles and easing to half by the deepest zoom (srtm smooths the 25 m cell seams, so the shade reads at every level). Toggled from the view bar, kept in the hash as ;relief; the tiles are fetched directly from the peer with ≤ 6 in flight and Retry-After honoured. ── */
var REL=SRTM.replace(/\/api\/v1$/,'')+'/tiles/hillshade/',rl={on:false,g:null,z:{},tiles:{},order:[],q:[],inflight:0,wait:0,cur:-1,want:[],a:null,T:0};
var RL_MAX=6,RL_KEEP=240,RL_TILES=30;
function relLayer(){if(rl.g||!hi.g)return;var ns='http://www.w3.org/2000/svg',g=document.createElementNS(ns,'g');g.setAttribute('class','ov ov-rel');var tr=hi.l.tree&&hi.l.tree[0].parentNode;hi.g.insertBefore(g,tr||null);rl.g=g}
function relZG(z){var g=rl.z[z];if(g)return g;var ns='http://www.w3.org/2000/svg';g=rl.z[z]=document.createElementNS(ns,'g');g.setAttribute('data-z',z);rl.g.appendChild(g);return g}
/* Web-Mercator zoom whose 256 px tile roughly matches the screen: metres per screen pixel of the view against the tile pyramid at this latitude */
function relMpp(){var cw=svg.getBoundingClientRect().width||W,ll=lonlat(vb.x+vb.w/2,vb.y+vb.h/2),c=Math.cos(ll[1]*Math.PI/180);return{mpp:111320*c/(proj.scale*proj.kx)*vb.w/cw,c:c}}
function relZ(){var m=relMpp(),z=Math.round(Math.log(156543.03*m.c/m.mpp)/Math.LN2+(window.devicePixelRatio>1.5?0.5:0));return Math.max(10,Math.min(17,z))}
function mtLon(x,z){return x/Math.pow(2,z)*360-180}function mtLat(y,z){var n=Math.PI-2*Math.PI*y/Math.pow(2,z);return 180/Math.PI*Math.atan(0.5*(Math.exp(n)-Math.exp(-n)))}
function mtX(lon,z){return Math.floor((lon+180)/360*Math.pow(2,z))}function mtY(lat,z){var r=lat*Math.PI/180;return Math.floor((1-Math.log(Math.tan(r)+1/Math.cos(r))/Math.PI)/2*Math.pow(2,z))}
function relWant(z){return mtWant(z,RL_TILES)}
/* the Web-Mercator squares of zoom z that touch the view, nearest the centre first, at most n (shared by relief and ortho) */
function mtWant(z,n){var a=lonlat(vb.x,vb.y+vb.h),b=lonlat(vb.x+vb.w,vb.y),x0=mtX(a[0],z),x1=mtX(b[0],z),y0=mtY(b[1],z),y1=mtY(a[1],z),cx=vb.x+vb.w/2,cy=vb.y+vb.h/2,out=[];
 for(var x=x0;x<=x1;x++)for(var y=y0;y<=y1;y++){var X0=px(mtLon(x,z)),X1=px(mtLon(x+1,z)),Y0=py(mtLat(y,z)),Y1=py(mtLat(y+1,z));out.push({k:z+'/'+x+'/'+y,z:z,x:x,y:y,X:X0,Y:Y0,w:X1-X0,h:Y1-Y0,d:Math.hypot((X0+X1)/2-cx,(Y0+Y1)/2-cy)})}
 out.sort(function(p,q){return p.d-q.d});return out.slice(0,n)}
function relTile(t){var e=rl.tiles[t.k];if(e){rl.order.splice(rl.order.indexOf(t.k),1);rl.order.push(t.k);return e}
 e=rl.tiles[t.k]={t:t,img:null,ok:false,tries:0};rl.order.push(t.k);rl.q.push(e);relPump();
 while(rl.order.length>RL_KEEP){var k=rl.order.shift(),o=rl.tiles[k];delete rl.tiles[k];o.dead=true;if(o.img&&o.img.parentNode)o.img.parentNode.removeChild(o.img);if(o.url)URL.revokeObjectURL(o.url)}return e}
function relPump(){if(!rl.on||!hi.on)return;var now=Date.now();if(now<rl.wait){clearTimeout(rl.T);rl.T=setTimeout(relPump,rl.wait-now+20);return}
 while(rl.inflight<RL_MAX&&rl.q.length){var e=rl.q.shift();if(e.dead||rl.tiles[e.t.k]!==e)continue;relFetch(e)}relStatus()}
function relFetch(e){var t=e.t;rl.inflight++;e.tries++;if(!rl.a)rl.a=window.AbortController?new AbortController():{signal:undefined,abort:function(){}};
 fetch(REL+t.z+'/'+t.x+'/'+t.y+'.png',{credentials:'omit',signal:rl.a.signal}).then(function(r){
  if(e.dead||rl.tiles[t.k]!==e)return null;if(r.status===204){e.ok=true;e.empty=true;return null}
  if(r.status===429||r.status===503){var ra=+r.headers.get('Retry-After')||2;rl.wait=Math.max(rl.wait,Date.now()+Math.min(30,ra*Math.pow(2,e.tries-1))*1000);if(e.tries<4)rl.q.unshift(e);else e.ok=true;return null}
  if(!r.ok){e.ok=true;if(r.status>=500&&rl.tiles[t.k]===e)delete rl.tiles[t.k];return null}return r.blob()}).then(function(b){rl.inflight=Math.max(0,rl.inflight-1);if(!b){relPump();relSettle();return}if(e.dead||rl.tiles[t.k]!==e){relPump();return}
  var ns='http://www.w3.org/2000/svg',im=document.createElementNS(ns,'image');var bl=1/relScale();im.setAttribute('x',t.X-bl);im.setAttribute('y',t.Y-bl);im.setAttribute('width',t.w+2*bl);im.setAttribute('height',t.h+2*bl);im.setAttribute('preserveAspectRatio','none');e.url=URL.createObjectURL(b);im.setAttribute('href',e.url);im.setAttributeNS('http://www.w3.org/1999/xlink','href',e.url);
  im.addEventListener('load',function(){im.classList.add('in')});relZG(t.z).appendChild(im);e.img=im;e.ok=true;relPump();relSettle()})
 .catch(function(x){if(x&&x.name==='AbortError')return;dbgLog('relief tile '+t.k+' failed: '+x);rl.inflight=Math.max(0,rl.inflight-1);e.ok=true;if(rl.tiles[t.k]===e)delete rl.tiles[t.k];relPump();relSettle()})}
function relScale(){return(svg.getBoundingClientRect().width||W)/vb.w}
/* once every tile the view asks for at the current zoom has answered, the groups of the other zooms step out (they cover the wait) */
function relSettle(){var all=true;for(var i=0;i<rl.want.length;i++){var e=rl.tiles[rl.want[i].k];if(!e||!e.ok){all=false;break}}
 for(var z in rl.z)rl.z[z].style.display=(+z===rl.cur||!all)?'':'none';relStatus()}
function relStatus(){var b=gv.querySelector('.gv-relief');if(b)b.classList.toggle('busy',rl.on&&hi.on&&(rl.inflight>0||rl.q.length>0))}
/* the view moved: pick the zoom, ask for the tiles that touch it, hide the ones that left, weigh the layer for the zoom (the 25 m cells blur softly once they grow past a dozen screen pixels) */
/* weight: full through the near tiles (the 25 m cells are ~9 px at 300×, srtm smooths the cell seams since 2026-10), easing to half at the deepest zoom (26 px per cell — a soft gradient, kept as context) */
function relW(){var zz=W/vb.w;return zz<HI_LOD1?.42:.42*(1-.5*Math.min(1,(zz-HI_LOD1)/(HI_ZMAX-HI_LOD1)))}
function relView(){if(!rl.on||!hi.on||!proj)return;relLayer();var wt=relW();if(wt<0.02){rl.g.style.opacity='0';rl.want=[];relStatus();return}var z=relZ();rl.cur=z;var want=relWant(z);rl.want=want;want.forEach(relTile);
 for(var k in rl.tiles){var e=rl.tiles[k],t=e.t;if(!e.img)continue;var on=t.X<vb.x+vb.w&&t.X+t.w>vb.x&&t.Y<vb.y+vb.h&&t.Y+t.h>vb.y;e.img.style.display=on?'':'none'}
 var m=relMpp(),cell=25/m.mpp;rl.g.style.opacity=wt.toFixed(2);rl.g.style.filter=cell>4?'blur('+Math.min(14,cell/2.5).toFixed(1)/relScale()+'px)':'';relSettle()}
function relClear(){if(rl.a)rl.a.abort();rl.a=null;rl.q=[];rl.inflight=0;rl.want=[];for(var k in rl.tiles){var o=rl.tiles[k];if(o.url)URL.revokeObjectURL(o.url)}rl.tiles={};rl.order=[];for(var z in rl.z)if(rl.z[z].parentNode)rl.z[z].parentNode.removeChild(rl.z[z]);rl.z={};rl.cur=-1;relStatus()}
function setRelief(on,quiet){on=!!on;if(on===rl.on)return;dbgLog('relief '+(on?'on':'off'));rl.on=on;svg.classList.toggle('v-rel',on);var b=gv.querySelector('.gv-relief');if(b){b.setAttribute('aria-pressed',on?'true':'false');b.title=on?'take the relief off the map':'relief \u00b7 hillshade from the 25\u202fm terrain model (full resolution) ( r )';if(b.classList.contains('off')){b.dataset.t0=ttl(b);b.title=AVAIL.relief}}
 if(quiet){if(on&&hi.on)relView();else if(!on)relClear();return}
 if(on){if(!hi.on){if(proj&&W/vb.w>=HI_MIN){autoSet(false,true);hiOn()}else showHint(coarse?'the relief comes with full resolution \u00b7 pinch in':'the relief comes with full resolution \u00b7 zoom in')}if(hi.on)relView()}else relClear();
 hiLegend();setHash();if(on&&hi.on&&!setRelief.h){setRelief.h=1;showHint('relief \u00b7 lit from the north-west \u00b7 25\u202fm terrain model, {OBS_NAME}')}}
/* ── ortho: basemap.at's orthophoto (WMTS bmaporthofoto30cm, google3857 z ≤ 19, CC BY 4.0 'basemap.at', 14-day cache, CORS open) laid UNDER the base tiles of the near levels (LOD 0/1) so the aerial image reads through the translucent cells and fills the gaps — the picture stays the data, the photo is context. Zoom chosen like the relief's (256 px square ≈ the screen), tiles placed by their lon/lat corners, ≤ 6 in flight, Retry-After honoured, 404 = outside the coverage. Opacity (default .5) from the popover: a click toggles the layer and opens it, a hold / right-click / wheel on the button adjusts without toggling. Hash ;ortho[=op] · key o. The browser fetches the tiles directly (noted on /datenschutz §4). ── */
var ORTHO='https://mapsneu.wien.gv.at/basemap/bmaporthofoto30cm/normal/google3857/',ot={on:false,op:.5,g:null,z:{},tiles:{},order:[],q:[],inflight:0,wait:0,cur:-1,want:[],a:null,T:0};
var OT_MAX=6,OT_KEEP=240,OT_TILES=40,OT_OP=.5;
function otLayer(){if(ot.g||!hi.g)return;var ns='http://www.w3.org/2000/svg',g=document.createElementNS(ns,'g');g.setAttribute('class','ov ov-ortho');hi.g.insertBefore(g,hi.g.firstChild);ot.g=g;otApply()}
function otZG(z){var g=ot.z[z];if(g)return g;var ns='http://www.w3.org/2000/svg';g=ot.z[z]=document.createElementNS(ns,'g');g.setAttribute('data-z',z);ot.g.appendChild(g);return g}
/* one step finer than the relief's pick: a photo reads through the cells only when its texture is sharper than the hexagons (tiles are ~12 KB jpegs, the cost is small) */
function otZ(){var m=relMpp(),z=Math.round(Math.log(156543.03*m.c/m.mpp)/Math.LN2+(window.devicePixelRatio>1.5?0.7:0.6));return Math.max(12,Math.min(19,z))}
function otTile(t){var e=ot.tiles[t.k];if(e){ot.order.splice(ot.order.indexOf(t.k),1);ot.order.push(t.k);return e}
 e=ot.tiles[t.k]={t:t,img:null,ok:false,tries:0};ot.order.push(t.k);ot.q.push(e);otPump();
 while(ot.order.length>OT_KEEP){var k=ot.order.shift(),o=ot.tiles[k];delete ot.tiles[k];o.dead=true;if(o.img&&o.img.parentNode)o.img.parentNode.removeChild(o.img);if(o.url)URL.revokeObjectURL(o.url)}return e}
function otPump(){if(!ot.on||!hi.on)return;var now=Date.now();if(now<ot.wait){clearTimeout(ot.T);ot.T=setTimeout(otPump,ot.wait-now+20);return}
 while(ot.inflight<OT_MAX&&ot.q.length){var e=ot.q.shift();if(e.dead||ot.tiles[e.t.k]!==e)continue;otFetch(e)}otStatus()}
function otFetch(e){var t=e.t;ot.inflight++;e.tries++;if(!ot.a)ot.a=window.AbortController?new AbortController():{signal:undefined,abort:function(){}};
 fetch(ORTHO+t.z+'/'+t.y+'/'+t.x+'.jpeg',{credentials:'omit',signal:ot.a.signal}).then(function(r){
  if(e.dead||ot.tiles[t.k]!==e)return null;if(r.status===204||r.status===404){e.ok=true;e.empty=true;return null}
  if(r.status===429||r.status===503){var ra=+r.headers.get('Retry-After')||2;ot.wait=Math.max(ot.wait,Date.now()+Math.min(30,ra*Math.pow(2,e.tries-1))*1000);if(e.tries<4)ot.q.unshift(e);else e.ok=true;return null}
  if(!r.ok){e.ok=true;if(r.status>=500&&ot.tiles[t.k]===e)delete ot.tiles[t.k];return null}return r.blob()}).then(function(b){ot.inflight=Math.max(0,ot.inflight-1);if(!b){otPump();otSettle();return}if(e.dead||ot.tiles[t.k]!==e){otPump();return}
  var ns='http://www.w3.org/2000/svg',im=document.createElementNS(ns,'image');var bl=1/relScale();im.setAttribute('x',t.X-bl);im.setAttribute('y',t.Y-bl);im.setAttribute('width',t.w+2*bl);im.setAttribute('height',t.h+2*bl);im.setAttribute('preserveAspectRatio','none');e.url=URL.createObjectURL(b);im.setAttribute('href',e.url);im.setAttributeNS('http://www.w3.org/1999/xlink','href',e.url);
  im.addEventListener('load',function(){im.classList.add('in')});otZG(t.z).appendChild(im);e.img=im;e.ok=true;otPump();otSettle()})
 .catch(function(x){if(x&&x.name==='AbortError')return;dbgLog('ortho tile '+t.k+' failed: '+x);ot.inflight=Math.max(0,ot.inflight-1);e.ok=true;if(ot.tiles[t.k]===e)delete ot.tiles[t.k];otPump();otSettle()})}
function otSettle(){var all=true;for(var i=0;i<ot.want.length;i++){var e=ot.tiles[ot.want[i].k];if(!e||!e.ok){all=false;break}}
 for(var z in ot.z)ot.z[z].style.display=(+z===ot.cur||!all)?'':'none';otStatus()}
function otStatus(){var b=gv.querySelector('.gv-ortho');if(b)b.classList.toggle('busy',ot.on&&hi.on&&(ot.inflight>0||ot.q.length>0))}
function otView(){if(!ot.on||!hi.on||!proj)return;otLayer();if(hiLOD()===2){ot.want=[];otStatus();return}var z=otZ();ot.cur=z;var want=mtWant(z,OT_TILES);ot.want=want;want.forEach(otTile);
 for(var k in ot.tiles){var e=ot.tiles[k],t=e.t;if(!e.img)continue;var on=t.X<vb.x+vb.w&&t.X+t.w>vb.x&&t.Y<vb.y+vb.h&&t.Y+t.h>vb.y;e.img.style.display=on?'':'none'}otSettle()}
function otClear(){if(ot.a)ot.a.abort();ot.a=null;ot.q=[];ot.inflight=0;ot.want=[];for(var k in ot.tiles){var o=ot.tiles[k];if(o.url)URL.revokeObjectURL(o.url)}ot.tiles={};ot.order=[];for(var z in ot.z)if(ot.z[z].parentNode)ot.z[z].parentNode.removeChild(ot.z[z]);ot.z={};ot.cur=-1;otStatus()}
function setOrtho(on,quiet){on=!!on;if(on===ot.on)return;dbgLog('ortho '+(on?'on':'off'));ot.on=on;svg.classList.toggle('v-ortho',on);var b=gv.querySelector('.gv-ortho');if(b){b.setAttribute('aria-pressed',on?'true':'false');b.title=on?'take the photo off the map \u00b7 hold for the opacity ( o )':'photo \u00b7 basemap.at orthophoto beneath the cells (near tiles) \u00b7 hold for the opacity ( o )';if(b.classList.contains('off')){b.dataset.t0=ttl(b);b.title=AVAIL.ortho}}
 if(quiet){if(on&&hi.on)otView();else if(!on){otClear();otPop(false)}return}
 if(on){if(!hi.on){if(proj&&W/vb.w>=HI_MIN){autoSet(false,true);hiOn()}else showHint(coarse?'the photo comes with the near tiles \u00b7 pinch in':'the photo comes with the near tiles \u00b7 zoom in')}if(hi.on)otView();otPop(true,coarse?3600:2600)}else{otClear();otPop(false)}
 hiLegend();setHash();if(on&&hi.on&&!setOrtho.h){setOrtho.h=1;showHint('photo \u00b7 basemap.at orthophoto under the cells \u00b7 hold the button for the opacity')}}
/* opacity: 0.1–1, kept in the hash as ;ortho=<op> when it is not the default; nothing is re-fetched. Up to the half the photo only fades in beneath the cells; past it the profile fills themselves step back (to .35 at 1) so the photo reads through — one slider = how much photo */
function otApply(){if(ot.g)ot.g.style.opacity=ot.op.toFixed(2);svg.style.setProperty('--otf',(ot.op<=.5?1:1-(ot.op-.5)/.5*.65).toFixed(2))}
function setOrthoOp(v,quiet){v=Math.max(.1,Math.min(1,Math.round(v*20)/20));if(v===ot.op)return;ot.op=v;otApply();otPopSync();if(!quiet){dbgLog('ortho op '+v);hiLegend();setHash()}}
/* the opacity popover (shared by the photo and the roads — mkPop): a click on the button toggles the layer and shows it for a moment; a hold (380 ms), a right-click or the wheel on the button adjust without toggling; it stays while the pointer is inside or the slider has focus, leaves on Escape, an outside tap, or by itself. o = {btn, panel, on(), turnOn(), get(), set(v,quiet), commit()} */
function mkPop(o){var P=o.panel,R=P&&P.querySelector('input'),PT=0,hold=0,held=false,shown=false,api;
 function sync(){if(!R)return;var v=o.get();R.value=String(Math.round(v*100));R.style.setProperty('--p',((v-.1)/.9*100).toFixed(1)+'%');P.querySelector('.v').textContent=Math.round(v*100)+'\u202f%'}
 function pop(on,ms){if(!P)return;clearTimeout(PT);shown=!!on;if(!on){P.classList.remove('on');PT=setTimeout(function(){if(!shown)P.hidden=true},200);return}
  var b=gv.querySelector(o.btn);if(!b||!o.on())return;sync();P.hidden=false;var side=P.parentNode,sr=side.getBoundingClientRect(),br=b.getBoundingClientRect(),pw=P.offsetWidth||140,cx=br.left+br.width/2-sr.left,l=Math.max(0,Math.min(sr.width-pw,cx-pw/2));P.style.left=l+'px';P.style.bottom=(sr.bottom-br.top+4)+'px';/* above the button's own row — the bar wraps on phones */P.style.setProperty('--ax',Math.max(8,Math.min(pw-8,cx-l))+'px');
  requestAnimationFrame(function(){if(shown)P.classList.add('on')});/* shown guards the frame: a pop(false) in the same tick (a share link flipping the layer on and off) must not leave the panel lit */if(ms)PT=setTimeout(function(){if(P.matches(':hover')||document.activeElement===R){pop(true,ms);return}pop(false)},ms)}
 api=function(on,ms){pop(on,ms)};api.sync=sync;api.held=function(){var h=held;held=false;return h};
 if(R){R.addEventListener('input',function(){o.set(+R.value/100,true);clearTimeout(PT)});R.addEventListener('change',function(){o.set(+R.value/100,true);o.commit();pop(true,coarse?3200:1800)});R.addEventListener('keydown',function(e){if(e.key==='Escape'){e.preventDefault();pop(false);main.focus({preventScroll:true})}e.stopPropagation()});R.addEventListener('blur',function(){pop(true,900)});
  P.addEventListener('pointerleave',function(){if(document.activeElement!==R)pop(true,900)});P.addEventListener('pointerenter',function(){clearTimeout(PT)});['pointerdown','pointerup','pointermove','wheel','dblclick','touchstart','click'].forEach(function(t){P.addEventListener(t,function(e){e.stopPropagation()},{passive:t==='wheel'||t==='touchstart'})});
  document.addEventListener('pointerdown',function(e){if(P.classList.contains('on')&&!P.contains(e.target)&&!e.target.closest(o.btn))pop(false)},true);
  var ob=gv.querySelector(o.btn);if(ob){ob.addEventListener('pointerdown',function(e){if(e.button!==0)return;held=false;clearTimeout(hold);hold=setTimeout(function(){held=true;if(!o.on())o.turnOn();pop(true,0)},380)});
   ['pointerup','pointercancel','pointerleave'].forEach(function(t){ob.addEventListener(t,function(){clearTimeout(hold)})});
   ob.addEventListener('contextmenu',function(e){e.preventDefault();if(!o.on())o.turnOn();else pop(true,0)});
   ob.addEventListener('wheel',function(e){if(!o.on())return;e.preventDefault();o.set(o.get()+(e.deltaY<0?.05:-.05),true);pop(true,1400);clearTimeout(api.hT);api.hT=setTimeout(o.commit,400)},{passive:false});
   ob.addEventListener('pointerenter',function(){if(o.on()&&!coarse&&P.hidden)pop(true,1600)})}}
 return api}
var otPop=mkPop({btn:'.gv-ortho',panel:fig.querySelector('.gv-op-ot'),on:function(){return ot.on},turnOn:function(){setOrtho(true)},get:function(){return ot.op},set:setOrthoOp,commit:function(){dbgLog('ortho op '+ot.op);hiLegend();setHash()}});
function otPopSync(){otPop.sync()}
/* ── roads: OSM roads + rail from our own osm_proximity.db, drawn server-side as v=rd tiles (ne_glyph_roads.go) over the fills, subsidy and change marks, under the relief and the trees — one quiet achromatic ink (no hue carries meaning), width = rank, weight = remoteness (the 250 m grid of osm_remote_grid.go: lighter in a town, darkest far from any settlement, stop or main road — reads in greyscale), service ways dashed, rail with white ties, a pale casing under every line so it reads on a dark fill or the photo. Near tiles only (LOD 0/1; the tile is the same PNG at either). The whole layer is weighed by the same popover the photo has (default .7); hash ;roads[=op], key w, hook roads(on,op). ── */
var RD_OP=.7,rd={on:false,op:RD_OP};
function rdApply(){var g=hi.l&&hi.l.rd&&hi.l.rd[0].parentNode;if(g)g.style.opacity=rd.op.toFixed(2)}
function setRoadsOp(v,quiet){v=Math.max(.1,Math.min(1,Math.round(v*20)/20));if(v===rd.op)return;rd.op=v;rdApply();rdPop.sync();if(!quiet){dbgLog('roads op '+v);hiLegend();setHash()}}
function setRoads(on,quiet){on=!!on;if(on===rd.on)return;dbgLog('roads '+(on?'on':'off'));rd.on=on;svg.classList.toggle('v-rd',on);var b=gv.querySelector('.gv-rd');if(b){b.setAttribute('aria-pressed',on?'true':'false');b.title=on?'take the roads off the map \u00b7 hold for the opacity ( w )':'roads \u00b7 OSM roads and rail in one quiet ink, darker the more remote (full resolution) \u00b7 hold for the opacity ( w )';if(b.classList.contains('off')){b.dataset.t0=ttl(b);b.title=AVAIL.roads}}gvBusy();
 if(quiet){if(on&&hi.on){hiCull();hiView(true)}else if(!on){rdPop(false);if(hi.on)hiCull()}return}
 if(on){if(!hi.on){if(proj&&W/vb.w>=HI_MIN){autoSet(false,true);hiOn()}else showHint(coarse?'the roads come with full resolution \u00b7 pinch in further':'the roads come with full resolution \u00b7 zoom in further')}if(hi.on){hiCull();hiView(true)}rdPop(true,coarse?3600:2600)}else{rdPop(false);if(hi.on)hiCull()}
 hiLegend();setHash();if(on&&hi.on&&!setRoads.h){setRoads.h=1;showHint('roads \u00b7 darker = more remote (far from a town, a stop, a main road) \u00b7 wider = higher rank \u00b7 dashed = service ways \u00b7 ties = rail \u00b7 OpenStreetMap')}}
var rdPop=mkPop({btn:'.gv-rd',panel:fig.querySelector('.gv-op-rd'),on:function(){return rd.on},turnOn:function(){setRoads(true)},get:function(){return rd.op},set:setRoadsOp,commit:function(){dbgLog('roads op '+rd.op);hiLegend();setHash()}});
/* the legend line: what is in view (km inside the tiles on screen, by rank) */
function rdLine(){var r=hiSum('rd');if(!r)return'<div class="nt mut rd">roads <i>loading tiles'+ldots()+'</i></div>';var c=r.rdc,parts=[];if(c[0])parts.push(fmt(c[0])+' major');if(c[1])parts.push(fmt(c[1])+' roads');if(c[2])parts.push(fmt(c[2])+' service');if(c[3])parts.push(fmt(c[3])+' rail');return'<div class="nt mut rd">roads <i>'+(r.km?(r.km<10?r.km.toFixed(1):Math.round(r.km))+'\u202fkm of ways and rail in view'+(parts.length?' \u00b7 '+parts.join(', '):''):'no roads or rail in view')+' \u00b7 OpenStreetMap \u00b7 '+Math.round(rd.op*100)+'\u202f%</i></div>'}


function hiStatus(){tD.classList.toggle('busy',hi.left>0);gvBusy();hiLegend();if(!hi.on){tN.classList.remove('n');return}if(!hi.left&&!hi.under&&!hiUnderMiss(hiLOD())){dbgLog('under-lod cull missed, catching up');hiCull()}/* the last in-flight tile may end on a path without a cull (dropped, replaced, failed) — never leave the stack painted */clearTimeout(hi.st);
 var sm=hiSum(),n=sm?sm.n:0,s=hi.left>0?'loading'+(n?' \u00b7 '+kfmt(n)+' cells':''):kfmt(n)+' cells \u00b7 '+((performance.now()-hi.t0)/1000).toFixed(1)+' s';tN.innerHTML=hi.left>0?esc(s)+ldots():esc(s);tN.classList.add('n');
 if(!hi.left)hi.st=setTimeout(function(){tN.classList.remove('n')},4500)}
function kfmt(n){return n>=1000?(n/1000).toFixed(n<10000?1:0)+'\u202fk':String(n)}
function hiOn(){if(hi.on||!proj||W/vb.w<HI_MIN)return;if(!picked&&!floating())setHTML(PH+'<div class="lg12"></div>');hi.on=true;hi.n=0;hi.left=0;hi.t0=performance.now();hi.a=window.AbortController?new AbortController():{signal:undefined,abort:function(){}};hiLayer();hi.g.classList.remove('out');svg.classList.add('hi');ZMAX=HI_ZMAX;tD.setAttribute('aria-pressed','true');tD.title='back to the overview';veilSync();
 pause(0);tour.on=false;hiView();setHash();ixGet();
 if(!hiOn.h){hiOn.h=1;showHint(coarse?'every hexagon is one res-12 cell \u00b7 tap one':'every hexagon is one res-12 cell \u00b7 hover one')}}
/* ── auto full resolution: zooming the overview to its deepest level (ZMAX0, 24×) flows straight into full resolution — the same gesture keeps going (ZMAX lifts to HI_ZMAX mid-pinch / mid-scroll), no button press, a hint names the way back.
   Leaving by esc or the ⤢ button after an automatic entry is read as "I want the overview": the deepest zoom then stays coarse (hint once, button still a tap away), remembered for the session in localStorage and shared as ;nofull in the link; turning full resolution on by hand clears it.
   Zooming out below HI_MIN is not a decision (the next deep zoom flows again); a shared ;full or ;nofull link wins over the stored preference. ── */
var AK='neg-nofull',auto={opt:false,by:false,near:0};try{auto.opt=localStorage.getItem(AK)==='1'}catch(e){}
function autoSet(opt,save){auto.opt=!!opt;if(save)try{localStorage.setItem(AK,opt?'1':'')}catch(e){}}
function hiAuto(){if(!proj||applying||tour.run||hi.fly)return;var z=W/vb.w;if(hi.on)return;
 if(z>=ZMAX0-0.02){if(auto.opt){if(!auto.near){auto.near=1;showHint(coarse?'overview stays at this zoom \u00b7 \u2922 for full resolution':'overview stays at this zoom \u00b7 \u2922 or the button for full resolution',2600)}return}
  hiOn();if(!hi.on)return;auto.by=true;hiOn.h=1;showHint(coarse?'full resolution \u00b7 every hexagon is one cell \u00b7 \u2922 goes back':'full resolution \u00b7 every hexagon is one cell \u00b7 esc goes back',2600);return}
 if(!auto.opt&&z>=ZMAX0*0.55&&!auto.near){auto.near=1;showHint(coarse?'keep pinching \u2192 full resolution':'keep zooming \u2192 full resolution',2600)}}
/* a find landing (KG · admin unit · point) ends in full resolution whenever the fit is deep enough for it (HI_MIN) — unless the overview was asked for (auto.opt) or the user moved meanwhile */
function landFull(navAt){var n=landFull.n=(landFull.n||0)+1;(function w(){if(animating||refits>0){requestAnimationFrame(w);return}if(n!==landFull.n||applying||hi.on||auto.opt||userNav>navAt||!proj||W/vb.w<HI_MIN)return;hiOn();if(hi.on){auto.by=true;hiOn.h=1}})()}
function hiLeave(zoomOut){var by=auto.by;if(by)autoSet(true,true);hiOff(zoomOut);if(by){auto.near=1;showHint(coarse?'overview kept \u00b7 \u2922 brings full resolution back':'overview kept \u00b7 the \u2922 button brings full resolution back',2600)}}
function hiOff(zoomOut){if(!hi.on)return;hi.on=false;if(typeof fltClear==='function'){fltClear();fltReadout()}hi.fly=false;auto.by=false;clearTimeout(hi.fT);clearTimeout(hi.vT);clearTimeout(hi.st);if(hi.a)hi.a.abort();hi.left=0;tD.classList.remove('busy');tN.classList.remove('n');gvBusy();tD.setAttribute('aria-pressed','false');tD.title='full resolution \u00b7 every res-12 cell (\u2248300 m\u00b2) by dominant land use';hiBtn();setHash();
 svg.classList.remove('hi');veilSync();var g=hi.g;if(g){g.classList.add('out');setTimeout(function(){if(hi.on)return;for(var k in hi.tiles){var o=hi.tiles[k];if(o.url)URL.revokeObjectURL(o.url)}hi.tiles={};hi.order=[];for(var v in hi.l){hi.l[v][0].innerHTML='';hi.l[v][1].innerHTML='';hi.l[v][2].innerHTML=''}relClear();otClear()},rm?0:420)}
 var had=hi.pin||hi.hov||hi.apx;apUnpin(true);hi.pin=null;hi.hov=null;cur.classList.remove('c12');ZMAX=ZMAX0;if(had){picked=null;shown=-1;hide()}else if(!picked&&!floating())setHTML(PH+(subVis()?'<div class="lg12"></div>':''));hiLegend();if(zoomOut&&W/vb.w>ZMAX0)zoomTo(vb.x+vb.w/2-W/ZMAX0/2,vb.y+vb.h/2-H/ZMAX0/2,W/ZMAX0,rm?0:500)}
tD.addEventListener('click',function(){closeTools();if(tD.classList.contains('off')){availHint('full');main.focus({preventScroll:true});return}if(hi.on)hiLeave(false);else{autoSet(false,true);hiOn()}main.focus({preventScroll:true})});
/* ── availability: full resolution needs HI_MIN, the trees need the near tiles (HI_LOD0), the relief rides along from HI_MIN up — a control whose layer cannot show at this zoom fades to a third and stays where it is (nothing in the bar moves), a tap on it says what zoom it wants. The bar follows the zoom lazily: it settles ~0.4 s after the last gesture frame and eases over half a second, so the reader sees the change without the bar flickering mid-pinch. A toggle that is on stays on while faded (the hash keeps ;trees / ;relief) and comes back with the zoom. ── */
var AVAIL={full:'full resolution \u00b7 zoom in first',trees:'the trees come with full resolution \u00b7 zoom in further',diff:'the diff comes with the near tiles \u00b7 zoom in further',relief:'the relief comes with full resolution \u00b7 zoom in',ortho:'the photo comes with the near tiles \u00b7 zoom in further',roads:'the roads come with full resolution \u00b7 zoom in further'},availT=0,availS={};
function avail(){var z=proj?W/vb.w:0;return{full:z>=HI_MIN,trees:z>=HI_LOD0,diff:z>=HI_LOD0,relief:z>=HI_MIN&&relW()>=0.02,ortho:z>=HI_LOD0,roads:z>=HI_LOD0}}
function hiBtn(){if(!tD)return;var a=avail(),gt=gv.querySelector('.gv-tree'),gr=gv.querySelector('.gv-relief'),gd=gv.querySelector('.gv-dif'),go=gv.querySelector('.gv-ortho'),gw=gv.querySelector('.gv-rd');
 tD.classList.toggle('off',!a.full);tD.setAttribute('aria-disabled',a.full?'false':'true');if(!a.full&&!hi.on)tD.title=AVAIL.full;
  [[gt,'trees'],[gd,'diff'],[gr,'relief'],[go,'ortho'],[gw,'roads']].forEach(function(p){var b=p[0],k=p[1];if(!b)return;b.classList.toggle('off',!a[k]);b.setAttribute('aria-disabled',a[k]?'false':'true');if(!a[k]){b.dataset.t0=b.dataset.t0||ttl(b);b.title=AVAIL[k]}else if(b.dataset.t0){b.title=b.dataset.t0;delete b.dataset.t0}});availS=a}
function availLazy(){clearTimeout(availT);var a=avail();if(a.full===availS.full&&a.trees===availS.trees&&a.relief===availS.relief&&a.ortho===availS.ortho&&a.roads===availS.roads)return;availT=setTimeout(hiBtn,applying?0:420)}
function availHint(k){var z=W/vb.w,need=(k==='trees'||k==='diff'||k==='ortho'||k==='roads')?HI_LOD0:HI_MIN;showHint(AVAIL[k]+(z<need?' ('+(coarse?'pinch':'zoom')+' \u00d7'+Math.ceil(need/Math.max(1,z))+')':''))}
hiBtn();
/* a cell under the pointer: point-in-hexagon over the docs already here, else one small call; the card is the same card */
function inPoly(p,q){var c=false,n=q.length;for(var i=0,j=n-1;i<n;j=i++){var a=q[i],b=q[j];if((a[1]>p[1])!==(b[1]>p[1])&&p[0]<(b[0]-a[0])*(p[1]-a[1])/(b[1]-a[1])+a[0])c=!c}return c}
function hiFind(p){for(var i=hi.docs.length-1;i>=0;i--)if(inPoly(p,hi.docs[i].poly))return hi.docs[i];return null}
function hiFetch(url,sig){return fetch(url,{credentials:'omit',signal:sig}).then(jsonOf).then(function(d){if(!d||!d.poly)return null;for(var i=0;i<hi.docs.length;i++)if(hi.docs[i].cell===d.cell){hi.docs[i]=d;return d}hi.docs.push(d);if(hi.docs.length>400)hi.docs.splice(0,100);return d})}
var hiA=null,hiT=0;
function hiAt(p,cb){var d=hiFind(p);if(d){cb(d);return}clearTimeout(hiT);hiT=setTimeout(function(){if(hiA)hiA.abort();var a=hiA=new AbortController(),ll=lonlat(p[0],p[1]);
 hiFetch('/api/v1/ne/glyph/cell?'+(subVis()?'sub=1&':'')+'lon='+ll[0].toFixed(6)+'&lat='+ll[1].toFixed(6)+tlQ(),a.signal).then(function(d){if(a!==hiA||!hi.on)return;cb(d)}).catch(function(x){if(a!==hiA||!hi.on||(x&&x.name==='AbortError'))return;cb({off:true})})},55)}
/* the card when the cell document itself did not come: a place, the offline row, nothing invented — recard() redraws it, and the next pick asks again */
function offCard(p){var r=[p[0],p[1],0,0,0,-1,0,0,null,[],0.13];r.k='off';r.bb=lonlat(p[0],p[1]).concat(lonlat(p[0],p[1]));apUnpin(true);hi.pin=null;picked=r;shown=-1;pk={t:'off',p:p};unpeek();reveal(r,offCardHTML(),false)}
function offCardHTML(){var ll=pk&&pk.p?lonlat(pk.p[0],pk.p[1]):null;return'<div class="hd"><b class="mut">no answer</b>'+(ll?'<span class="age">'+ll[1].toFixed(4)+'\u00b0N '+ll[0].toFixed(4)+'\u00b0E</span>':'')+'</div>'+(offRow()||'<div class="rw off"><span class="lb">offline</span><span class="vl mut">back \u2014 pick again</span></div>')}
function hiRow(d){var r=[d.x,d.y,0,0,0,-1,0,0,null,[],0.13];r.k='h3_'+d.cell;var xs=d.poly.map(function(q){return q[0]}),ys=d.poly.map(function(q){return q[1]}),a=lonlat(Math.min.apply(0,xs),Math.max.apply(0,ys)),b=lonlat(Math.max.apply(0,xs),Math.min.apply(0,ys));r.bb=[a[0],a[1],b[0],b[1]];r.cell=d.cell;return r}
function polyD(q){var s='';for(var i=0;i<q.length;i++)s+=(i?'L':'M')+q[i][0].toFixed(3)+' '+q[i][1].toFixed(3);return s+'Z'}
/* where a place sits, every level named — "KG 42005 \u00b7 Gemeinde Gosau \u00b7 Bezirk Gmunden" — so nobody has to guess whether Gosau or Gmunden is the municipality; the Gemeinde is dropped when it only repeats the KG name shown in the header (KG Gosau → "KG 42005 \u00b7 Bezirk Gmunden"); withCode=false drops the KG number (peek, history) */
function placeOf(e,code,withCode){var p=[];code=code||(e&&e[0])||'';if(withCode!==false&&code)p.push('KG '+code);if(e){if(e[2]&&norm(e[2])!==norm(e[1]))p.push('Gemeinde '+e[2]);if(e[3])p.push('Bezirk '+e[3])}return p.join(' \u00b7 ')}
function hiName(d){var code='',best=-1;for(var k in(d.kgs||{}))if(d.kgs[k]>best){best=d.kgs[k];code=k}var e=ixBy[code];if(code&&!e&&!ix)ixGet().then(function(){if(picked&&pk&&pk.t==='h3')recard();tagSync()});return{code:code,e:e,name:e?e[1]:(code?'KG '+code:''),sub:placeOf(e,code),where:placeOf(e,code,false)}}
/* one cell's tallest tree (pic_obs.tree of /ne/glyph/cell): the band swatch, the height, the apex count */
function treeLine(tr){return tr?sw('g-tr'+tr.band)+'tallest tree <b>'+tr.h_m.toFixed(1).replace(/\.0$/,'')+'\u202fm</b> <span class="mut">'+TH_L[tr.band]+' \u00b7 '+tr.n+(tr.n==1?' apex':' apices')+'</span>':sw('g-none')+'<span class="mut">no tree in this cell</span>'}
/* the res-12 peek: one line per source for the cell under the cursor — a declared bar + its top groups and structures, the observed pixel in words (v=obs), the change state (v=chg); the KG is already named by the selection */
/* the peek follows the picture: its bar and named classes are what the view paints for this cell — declared mix · observed dominant use (+ tallest tree) · change Δ per group — the other sources one muted line; subsidy on → the claim line */
function peekLine(ks,m,t,max){var l=[],i;for(i=0;i<Math.min(max,ks.length);i++)l.push(sw('g-'+ks[i])+GL[ks[i]]+' <span class="mut">'+pct(m[ks[i]],t)+'</span>');return l}
function subPeekLine(d){if(!subVis())return'';var s=d.sub;if(s===undefined&&!d.sub_unavailable)subFetchCell(d,function(){if(hover&&hi.on&&mv){peekK='';onHover()}});if(d.sub_unavailable)return'<div class="pl"><span class="s">subsidy</span> <span class="mut">layer unavailable</span></div>';if(s===undefined)return'<div class="pl"><span class="s">subsidy</span> <span class="mut">loading'+ldots()+'</span></div>';
 var sg=s&&s.schlag;if(sg&&(!cg.on?sg.vs_prev!=='gone':true)){var cj=SUB_CL.indexOf(sg.class),q=[sw('g-sub')+(sg.vs_prev==='gone'?'<b>'+subHaM2(Math.round((sg.prev_area_ha||0)*1e4))+'</b> gone':'<b>'+subHaM2(Math.round(sg.area_ha*1e4))+'</b>'),SUB_CL[cj>0?cj:0]];if(sg.bio===true)q.push('bio');if(!sg.reliable)q.push('dubious drawing');
  if(cg.on){var vq={same:'same',new:'new',gone:'gone',modified:'redrawn',split_merged:'split / merged',partial:'partly redrawn'}[sg.vs_prev]||sg.vs_prev;if(vq)q.push('vs '+sg.prev_year+': <b>'+vq+'</b>'+(sg.d_area_ha?' <span class="mut">'+subHaM2(Math.round(sg.d_area_ha*1e4),1)+'</span>':''))}return'<div class="pl"><span class="s">subsidy</span> '+q.join('<span class="sep"></span>')+'</div>'}
 if(!s||!s.claimed)return'<div class="pl"><span class="s">subsidy</span> <span class="mut">not claimed'+(s?' ('+s.year+')':'')+'</span></div>';var ci=SUB_CL.indexOf(s.class),p=[sw('g-sub')+'claimed <b>'+s.claimed_pct+'\u202f%</b>',SUB_CL[ci>0?ci:0]];if(s.bio===true)p.push('bio');if(!s.reliable)p.push('dubious drawing');
 if(cg.on){var vp={same:'same',new:'new',gone:'gone',modified:'redrawn',split_merged:'split / merged',partial:'partly redrawn'}[s.vs_prev]||s.vs_prev;if(vp)p.push('vs '+s.prev_year+': <b>'+vp+'</b>')}return'<div class="pl"><span class="s">subsidy</span> '+p.join('<span class="sep"></span>')+'</div>'}
function hiPeekHTML(d){var lu=d.lu,s='';if(!selected()){var nm=hiName(d);if(nm.name)s+='<b>'+esc(nm.name)+'</b>'+(nm.where?'<span class="mut"> \u00b7 '+esc(nm.where)+'</span>':'')+'<div class="pl">'}if(!lu)return s+'<b class="mut">'+(d.pending_kgs?'not digested yet':'nothing held for this cell')+'</b>'+(s?'</div>':'');
 var m={},t=0,i;for(i=0;i<9;i++)if(lu.lu[i]>0){m[GO[i]]=lu.lu[i];t+=lu.lu[i]}var ks=Object.keys(m).sort(function(a,b){return m[b]-m[a]}),l=[],po=d.pic_obs,c=d.chg;
 var decMuted='<div class="pl mut"><span class="lb">declared</span> '+(t?peekLine(ks,m,t,2).join('<span class="sep"></span>'):sw('g-unknown')+'unknown')+'</div>';
 var prof='',chg='';
 if(view==='obs'){var dp=po&&!po.unknown?po.dom_pct/100:0;if(po){prof+='<div class="pb" role="img" aria-label="observed land use of this cell">';if(po.unknown)prof+=seg('g-unknown',1);else{prof+=seg('g-'+po.dom,dp);if(dp<1)prof+=seg(po.second?'g-'+po.second:'g-none',1-dp)}prof+='</div>'}
  prof+='<div class="pl"><span class="o">observed</span> '+(po?(po.unknown?sw('g-unknown')+'<span class="mut">unknown</span>':sw('g-'+po.dom)+GL[po.dom]+' <span class="mut">\u2248'+po.dom_pct+'\u202f%</span>'+(po.second?'<span class="sep"></span>'+sw('g-'+po.second)+GL[po.second]:'')):'<span class="mut">no observed row \u00b7 drawn in its declared use, faint</span>')+'</div>'}
 else{if(t){prof+='<div class="pb" role="img" aria-label="declared land use of this cell">';for(i=0;i<ks.length;i++)prof+=seg('g-'+ks[i],m[ks[i]]/t);prof+='</div>';l=peekLine(ks,m,t,3)}
  else l.push(sw('g-unknown')+'<span class="mut">declared unknown</span>');
  if(lu.str_n)l.push(lu.str_n+' structure'+(lu.str_n==1?'':'s'));prof+='<div class="pl">'+l.join('<span class="sep"></span>')+'</div>'}
 if(hi.tree&&po)prof+='<div class="pl">'+treeLine(po.tree)+'</div>';
 if(cg.on){var cl2=[];if(c&&c.status){var cl=d.cluster,many=cl&&cl.cells>1,dl=many?addV(cl.d_lu_m2,cl.d_lu_pending_m2):(c.d_lu_m2||c.d_lu),dm={},dt=0,top=[];if(dl)for(i=0;i<9;i++)if(dl[i]){dm[GO[i]]=Math.abs(dl[i]);dt+=Math.abs(dl[i]);top.push(i)}top.sort(function(a,b){return dm[GO[b]]-dm[GO[a]]});
   if(dt){chg+='<div class="pb" role="img" aria-label="\u0394 declared use'+(many?' across the cluster':' of this cell')+'">';for(i=0;i<top.length;i++)chg+=seg('g-'+GO[top[i]],dm[GO[top[i]]]/dt);chg+='</div>'}
   var mv=chgMoved(dl),when=perOf(many?cl.first:c.first_seen,many?cl.last:(c.last_seen||c.last_change));cl2.push('<span class="c'+((many?!cl.confirmed:c.status!=='confirmed')?' ca':'')+'">\u25cf</span> '+(mv?'<b>'+areaU(mv)+'</b> '+chgDir(dl):'<b>'+esc(c.status)+'</b>'));if(when)cl2.push(when);if(many)cl2.push('<span class="mut">'+fmt(cl.cells)+' cells</span>');else if(c.d_str_n)cl2.push('structures '+(c.d_str_n>0?'+':'\u2212')+Math.abs(c.d_str_n))}
  else cl2.push('<span class="c">change</span> <span class="mut">none between its digests</span>');chg+='<div class="pl">'+cl2.join('<span class="sep"></span>')+'</div>'}
 return s+chg+prof+(view==='obs'?decMuted:'')+subPeekLine(d)}
/* the res-12 card: header = KG · Gemeinde, declared = land-use mix of this one cell (+ structures, edges, units of its res-10 block, change), observed = the same srtm call for the hexagon */
function hiCard(d){var nm=hiName(d),lu=d.lu,r=hiRow(d),s='<div class="hd"><b>'+(nm.code?kgLink(nm.name,nm.code):esc(nm.name||'cell'))+'</b><span class="age">res 12 \u00b7 \u2248300\u202fm\u00b2</span></div><div class="sub">'+(nm.sub?esc(nm.sub)+'</div><div class="sub dti">cell '+d.cell:'cell '+d.cell)+'</div>';s+=fltCardRow(d);
 if(!lu){var dec0='<div class="rw"><span class="lb">declared</span><span class="vl"><span class="mut">'+(d.pending_kgs?'not digested yet \u00b7 KG '+d.pending_kgs.join(', '):'nothing held for this cell')+'</span></span></div><div class="dt"><div class="sh"><span>declared <i>survey land use \u00b7 BEV tiles via bevdirect</i></span></div><div class="nt mut">not digested yet</div></div>';return s+srcs(dec0,'',hiChgHTML(d))+subCellLine(d)+hiLegendHTML()}
 var m={},t=0,i;for(i=0;i<9;i++)if(lu.lu[i]>0){m[GO[i]]=lu.lu[i];t+=lu.lu[i]}var ks=Object.keys(m).sort(function(a,b){return m[b]-m[a]}),line=[];
 for(i=0;i<Math.min(2,ks.length);i++)line.push(sw('g-'+ks[i])+GL[ks[i]]+' <span class="mut">'+pct(m[ks[i]],t)+'</span>');if(!line.length)line.push(sw('g-unknown')+'<span class="mut">declared use unknown</span>');
 if(lu.str_n)line.push(lu.str_n+' structure'+(lu.str_n==1?'':'s'));
 var dec='<div class="rw"><span class="lb">declared</span><span class="vl">'+line.join('<span class="sep"></span>')+'</span></div>';
 dec+='<div class="dt"><div class="sh"><span>declared <i>survey land use \u00b7 BEV tiles via bevdirect</i></span>'+(lu.flags&&lu.flags.declared_unknown?'<em title="declared use unknown for part of this cell">?</em>':'')+'</div>';
 dec+=t?obsBar(m,'cov','declared land use of this cell'):'<div class="nt mut">'+sw('g-unknown')+'declared use unknown \u2014 the cell is held, its use is not recorded</div>';
 var f=[];if(lu.land<255)f.push(pct(lu.land,255)+' land');if(lu.str_cover)f.push('structures cover '+pct(lu.str_cover,255));if(lu.str_n)f.push(lu.str_n+' structure'+(lu.str_n==1?'':'s'));if(lu.edge_m_ha)f.push(fmt(lu.edge_m_ha)+'\u202fm/ha edges');if(lu.n_units)f.push(lu.n_units+' unit'+(lu.n_units==1?'':'s'));
 if(f.length)dec+='<div class="nt kv">'+f.join('<span class="sep"></span>')+'</div>';
 if(d.superseded)dec+='<div class="nt mut">index epoch only \u2014 the current build of this chunk no longer holds the cell; shown faded until a neighbour\u2019s rows take it</div>';
 var k=d.k;if(k){var kf=[];if(k.n_units)kf.push('<b>'+fmt(k.n_units)+'</b> units');if(k.unit_p50)kf.push('median '+fmt(k.unit_p50)+'\u202fm\u00b2');if(k.unit_p10&&k.unit_p90)kf.push(fmt(k.unit_p10)+'\u2013'+fmt(k.unit_p90)+'\u202fm\u00b2 (p10\u2013p90)');if(k.reg_n)kf.push(fmt(k.reg_n)+' registers');if(k.str_n)kf.push(fmt(k.str_n)+' structures');
  if(kf.length)dec+='<div class="nt mut"><span class="it lead">res-10 block (\u22481.5\u202fha)</span> '+kf.join('<span class="sep"></span>')+'</div>'}
 dec+='</div>';s+=srcs(dec,obsHTML(r),hiChgHTML(d))+difCellLine(d)+subCellLine(d);obsFetch(r);return s+hiLegendHTML()}
/* diff of one res-12 cell (pic_dif of its doc): the row reads "LiDAR sees grass · register says forest · flown 2018"; the open block names the kind, its corroboration and the epoch as an age */
function difCellLine(d){if(!df.on)return'';var p=d.pic_dif,row=function(v,dt){return'<div class="rw"><span class="lb">diff</span><span class="vl">'+v+'</span></div><div class="dt"><div class="sh"><span>diff <i>register vs LiDAR \u00b7 same cell, today</i></span></div><div class="nt">'+v+'</div>'+(dt||'')+'</div>'};
 if(!p)return row('<span class="mut">no observed row for this cell yet</span>','');
 var g=function(o){return o?sw('g-'+o.dom)+GL[o.dom]:'<span class="mut">unknown</span>'},ep=p.als?difAge([p.als.dominant_share<0.9?p.als.oldest:0,p.als.newest]):'',ki=p.kind?DIF_K[p.kind]:-1,kr=!p.kind&&p.kind_raw?DIF_K[p.kind_raw]:-1,f=[],v;
 if(p.status!=='ok')v='<span class="mut">not compared \u00b7 '+({edge:'cell on an edge',superseded:'cell superseded',obs_null:'LiDAR verdict unknown',unknown_declared:'declared use unknown'}[p.status]||p.status)+'</span>';
 else if(p.agree&&ki<0)v='<span class="mut">agree</span><span class="sep"></span>'+g(p.observed);
 else v='LiDAR sees '+g(p.observed)+'<span class="sep"></span>register says '+g(p.declared);
 if(ep)v+='<span class="sep"></span><span class="mut">'+ep+'</span>';
 if(ki>=0)f.push(difSw(ki)+'<b>'+DIF_L[ki]+'</b> \u2014 '+DIF_T[ki]);else if(kr>=0)f.push(difSw(kr)+'<span class="mut">'+DIF_L[kr]+', uncorroborated \u2014 drawn under \u201call cells\u201d only</span>');else if(p.status==='ok'&&!p.agree)f.push('<span class="mut">a vocabulary difference, not drawn (crop vs grass, alpine pasture, roads, water)</span>');
 var c=[];if(ki>=0||kr>=0){if(p.dh_m!=null&&p.dh_m)c.push('nDSM '+(p.dh_m>0?'+':'')+p.dh_m+'\u202fm between the flights');if(p.loss_year)c.push('forest loss '+p.loss_year+' (Hansen)')}if(p.als&&(ki>=0||kr>=0||p.als.mixed))c.push('LiDAR '+(p.als.mixed?p.als.oldest+'\u2013'+p.als.newest+' (mixed years in KG '+p.als.kg+')':'flown '+p.als.newest+(p.als.oldest&&p.als.oldest!==p.als.newest?', older flight '+p.als.oldest:'')));
 return row(v,(f.length?'<div class="nt">'+f.join('')+'</div>':'')+(c.length?'<div class="nt mut">'+c.join('<span class="sep"></span>')+'</div>':''))}
/* change of one res-12 cell: the chg block of its public doc + the cluster of touching cells with change around it. The reader cares what moved and when, less whether it is confirmed: the row says "13.7 ha alpine → forest · Oct 2026 · 452 cells", the open block adds the cluster bar, the sighting window and this cell's own line */
var MON=['Jan','Feb','Mar','Apr','May','Jun','Jul','Aug','Sep','Oct','Nov','Dec'];
function perOf(a,b){if(!a)return'';var A=new Date(a),B=new Date(b||a);if(isNaN(A))return'';if(isNaN(B))B=A;var y1=A.getUTCFullYear(),m1=A.getUTCMonth(),y2=B.getUTCFullYear(),m2=B.getUTCMonth(),q1=(m1/3|0)+1,q2=(m2/3|0)+1,mm=(y2-y1)*12+m2-m1;
 if(mm<=0)return MON[m1]+' '+y1;if(mm===1)return y1===y2?MON[m1]+'/'+MON[m2]+' '+y1:MON[m1]+' '+y1+'/'+MON[m2]+' '+y2;if(y1===y2&&q1===q2)return'Q'+q1+' '+y1;if(y1===y2)return'Q'+q1+'/Q'+q2+' '+y1;return'Q'+q1+' '+y1+'/Q'+q2+' '+y2}
function dayOf(a){var d=new Date(a);return isNaN(d)?'':d.getUTCDate()+' '+MON[d.getUTCMonth()]}
function areaU(m2){var a=Math.abs(m2);return a<10000?fmt(Math.round(a))+'\u202fm\u00b2':a<1e6?(a/10000).toFixed(1)+'\u202fha':a<1e8?fmt(Math.round(a/10000))+'\u202fha':(a/1e6).toFixed(1)+'\u202fkm\u00b2'}
/* from → to out of a Δ-by-group vector: the groups that lost land name the "from", those that gained the "to"; one swatch each, a "+n" for the rest */
function chgDir(dl){if(!dl)return'';var fr=[],to=[],i;for(i=0;i<9;i++)if(dl[i]<0)fr.push(i);else if(dl[i]>0)to.push(i);fr.sort(function(a,b){return dl[a]-dl[b]});to.sort(function(a,b){return dl[b]-dl[a]});var one=function(l){if(!l.length)return'';return sw('g-'+GO[l[0]])+GL[GO[l[0]]]+(l.length>1?' <span class="mut">+'+(l.length-1)+'</span>':'')};
 if(!fr.length&&!to.length)return'';return'<span class="nw">'+(!fr.length?'\u2192 '+one(to):!to.length?one(fr)+' \u2192 <span class="mut">less</span>':one(fr)+' <span class="mut">\u2192</span> '+one(to))+'</span>'}
function chgMoved(dl){var t=0,i;if(dl)for(i=0;i<9;i++)if(dl[i]>0)t+=dl[i];return t}
function addV(a,b){var o=[],i;for(i=0;i<9;i++)o.push((a&&a[i]||0)+(b&&b[i]||0));return o}
function hiChgHTML(d){var c=d.chg,cl=d.cluster,line=[],s='',i;
 if(c&&c.status){var dl=c.d_lu_m2||c.d_lu,cdl=cl?addV(cl.d_lu_m2,cl.d_lu_pending_m2):null,many=cl&&cl.cells>1,v=many?cdl:dl,mv=chgMoved(v),dot='<span class="c'+((many?!cl.confirmed:c.status!=='confirmed')?' ca':'')+'">\u25cf</span>',when=perOf(many?cl.first:c.first_seen,many?cl.last:(c.last_seen||c.last_change));
  line.push(dot+(mv?' <b>'+areaU(mv)+'</b> '+chgDir(v):' <b>'+esc(c.status)+'</b>'+(c.unknown?' <span class="mut">\u0394 not asserted</span>':'')));var tail=[];if(when)tail.push('<b>'+when+'</b>');if(many)tail.push('<span class="mut">'+fmt(cl.cells)+' cells'+(cl.capped?'+':'')+'</span>');if(tail.length)line[line.length-1]+='<span class="nw"><span class="sep"></span>'+tail.join('<span class="sep"></span>')+'</span>';if(!many&&c.d_str_n)line.push('structures '+(c.d_str_n>0?'+':'\u2212')+Math.abs(c.d_str_n))}
 else line.push('<span class="mut">'+(d.lu?'no change between its digests':'not digested yet')+'</span>');
 s+='<div class="rw"><span class="lb">change</span><span class="vl'+(c&&c.status?' wr':'')+'">'+line.join('<span class="sep"></span>')+'</span></div>';
 s+='<div class="dt"><div class="sh"><span>change <i>between digests \u00b7 confirmed = seen twice</i></span>'+(c&&c.status?'<em class="c">'+(when?esc(when):'\u25cf '+esc(c.status))+'</em>':'')+'</div>';
 if(c&&c.status){var f=[];
  if(many){var cb=dluBar(cdl,'\u0394 declared use across the cluster',lgMax());if(cb)s+=cb;
   var st=cl.confirmed&&cl.candidate?fmt(cl.confirmed)+' confirmed \u00b7 <span class="ca">\u25cf</span> '+fmt(cl.candidate)+' candidate':cl.candidate?'<span class="ca">\u25cf</span> all candidate, seen once':'all confirmed',win=cl.first&&cl.last&&cl.first.slice(0,10)!==cl.last.slice(0,10)?'sighted '+dayOf(cl.first)+' \u2013 '+dayOf(cl.last)+' '+new Date(cl.last).getUTCFullYear():cl.first?'sighted '+dayOf(cl.first)+' '+new Date(cl.first).getUTCFullYear():'';
   s+='<div class="nt mut">'+['the cluster covers '+areaU(cl.area_m2)+(cl.capped?' (cut at '+fmt(cl.cells)+' cells)':''),st,win,cl.d_str_n?'structures '+(cl.d_str_n>0?'+':'\u2212')+Math.abs(cl.d_str_n):''].filter(Boolean).join('<span class="sep"></span>')+'</div>';
   var me=[dot+' <b>'+esc(c.status)+'</b>'];var mm=chgMoved(dl);if(mm)me.push(areaU(mm)+' '+chgDir(dl));else if(c.unknown)me.push('\u0394 not asserted');if(c.d_str_n)me.push('structures '+(c.d_str_n>0?'+':'\u2212')+Math.abs(c.d_str_n));if(c.first_seen)me.push('<span class="mut">'+dayOf(c.first_seen)+(c.last_seen&&c.last_seen.slice(0,10)!==c.first_seen.slice(0,10)?' \u2013 '+dayOf(c.last_seen):'')+'</span>');
   s+='<div class="nt dx"><span class="it lead">this cell</span> '+me.join('<span class="sep"></span>')+'</div>'}
  else{if(c.first_seen)f.push('sighted '+dayOf(c.first_seen)+(c.last_seen&&c.last_seen.slice(0,10)!==c.first_seen.slice(0,10)?' \u2013 '+dayOf(c.last_seen):'')+' '+new Date(c.last_seen||c.first_seen).getUTCFullYear());f.push('<b>'+esc(c.status)+'</b>'+(c.status==='confirmed'?'':' \u00b7 seen once'));
   var dd=dl;if(dd){var m={},t=0,l=[];for(i=0;i<9;i++)if(dd[i]){m[GO[i]]=Math.abs(dd[i]);t+=Math.abs(dd[i])}if(t){s+='<div class="pb" role="img" aria-label="\u0394 declared use of this cell">';var ks=Object.keys(m).sort(function(a,b){return m[b]-m[a]});for(i=0;i<ks.length;i++){s+=seg('g-'+ks[i],m[ks[i]]/t);var g=GO.indexOf(ks[i]);l.push(item('g-'+ks[i],null,GL[ks[i]],(dd[g]>0?'+':'\u2212')+Math.abs(dd[g])+'\u202fm\u00b2'))}s+='</div>'+legend(l,4)}}
   if(cl&&cl.cells===1)f.push('alone \u2014 no touching cell moved')}
  if(c.d_str_cover_m2)f.push('structure cover '+(c.d_str_cover_m2>0?'+':'\u2212')+Math.abs(c.d_str_cover_m2)+'\u202fm\u00b2');if(c.d_edge_m_ha)f.push('edges '+(c.d_edge_m_ha>0?'+':'\u2212')+Math.abs(c.d_edge_m_ha)+'\u202fm/ha');if(c.unknown)f.push('declared use unknown on one side \u2014 \u0394 not asserted');if(c.candidate_pending)f.push('a newer digest awaits its second sighting');
  if(f.length)s+='<div class="nt mut'+(many?' dx':'')+'">'+f.join('<span class="sep"></span>')+'</div>'}
 else s+='<div class="nt mut">'+(d.lu?'its digests agree, or there is only one so far':'not digested yet')+'</div>';
 return s+'</div>'}
function hiShow(d,pin){apUnpin(true);var r=hiRow(d),same=picked&&picked.k===r.k;picked=r;shown=-1;pk={t:'h3',d:d};reveal(r,same?null:hiCard(d),false);cur.setAttribute('d',polyD(d.poly));cur.classList.add('c12');curAt=[d.x,d.y];curI=-1;svg.classList.add('on');if(pin){hi.pin=d;setHash();var nm=hiName(d);pushHist({k:'h3',c:d.cell,x:d.x,y:d.y,n:nm.name||'cell '+d.cell,g:nm.where})}else hi.hov=d}
function hiPick(p){hiAt(p,function(d){if(!hi.on)return;if(d&&d.off){offCard(p);return}if(!d||(hi.pin&&hi.pin.cell===d.cell)){apUnpin(true);hi.pin=null;hi.hov=null;hide();setHash();return}hiShow(d,true)})}
function hiUnpin(){apUnpin(true);hi.pin=null;hi.hov=null;cur.classList.remove('c12');picked=null;shown=-1;hide();setHash()}
function hiGoto(cell,fly){return hiFetch('/api/v1/ne/glyph/cell?cell='+cell+tlQ()).then(function(d){if(!d)return;if(fly!==false){var w=W/Math.max(HI_LOD1,W/vb.w);zoomTo(d.x+w*sheetX()-w/2,d.y+w*AR*sheetY()-w*AR/2,w,0)}hiOn();hiShow(d,true)}).catch(function(){})}
` + neGlyphFiltJS + neGlyphExportJS + `
main.classList.add('js');try{if(floating()&&localStorage.getItem(DK)==='1')setDock(true,false)}catch(e){}if(!floating()){inner.innerHTML=PH;tip.classList.add('on')}tour.idle=setTimeout(resume,1400);applyHash();
/* test hook (window.neGlyph): read the state, the card / legend / peek text, hover or pin a lon/lat — for scripted visual checks (docs/ne-change.md §6a testing); log()/logReset() = JS errors + fetch counts by class × status + the dbg trace (docs/ne-glyph.md §soak) */
/* ── time: change over time, by PASS (docs/ne-change.md §6e). The k-th sighting of a chunk is its pass k; a delta between the (k−1)-th and k-th digest belongs to pass k — the register moved somewhere between the two. Nationally a pass is complete when (nearly) every chunk has k sightings (the second ≈ 2 months after the first at the Oct 2026 pace); a KG's own passes close earlier, so the strip adapts: it reads the selected KG's passes, else the national ones. Inside a pass the calendar (detection days, adaptive bins: days · weeks · months · quarters) is the second level — labelled "detected", because within a pass the dates are the peers' crawl order. Window = {k1,k2[,d1,d2]} → hash ;t=2 · ;t=2..3 · ;t=2@2026-10-16..2026-10-22 (at=x,y,z stays the viewport); keys < > step, space plays; the chg tiles, dots, hex faces, legend and card all read the window. ── */
function tlStr(w){w=w===undefined?tl.win:w;if(!w)return'';var s='';if(w.k1){s=w.k1===w.k2?String(w.k1):w.k1+'..'+w.k2}if(w.d1){s+='@'+w.d1+(w.d2!==w.d1?'..'+w.d2:'')}return s}
function tlParse(s){var r=/^(?:(\d{1,3})(?:\.\.(\d{1,3}))?)?(?:@(\d{4}-\d{2}-\d{2})(?:\.\.(\d{4}-\d{2}-\d{2}))?)?$/.exec(s||'');if(!r||!s)return null;var w={k1:+r[1]||0,k2:+(r[2]||r[1])||0,d1:r[3]||'',d2:r[4]||r[3]||''};if(w.k1&&w.k1<2||w.k2<w.k1||w.d2<w.d1)return null;return w}
function tlQ(){return tl.win?'&t='+tlStr():''}
function tlHas(w,k,day){if(!w)return true;if(w.k1&&(k<w.k1||k>w.k2))return false;if(w.d1&&(day<w.d1||day>w.d2))return false;return true}
/* data: the national table once (5 min on the server), a KG's table per selection */
function tlLoad(){if(tl.d||tl.ld)return tl.ld||Promise.resolve(tl.d);tl.busy=true;tlRender();tl.ld=fetch('/api/v1/ne/glyph/passes',{credentials:'omit'}).then(jsonOf).then(function(d){tl.d=d;tl.ld=null;tl.busy=false;tl.err=0;tlMap();tlRender();tlPaint();return d}).catch(function(){tl.ld=null;tl.busy=false;tl.err=Date.now();tlRender()});return tl.ld}
function tlKGLoad(kg){var e=tl.kg[kg];if(e&&(e.d||e.p))return e.p||Promise.resolve(e.d);e=tl.kg[kg]={d:null,p:null};e.p=fetch('/api/v1/ne/glyph/passes?kg='+kg,{credentials:'omit'}).then(jsonOf).then(function(d){e.d=d;e.p=null;if(sel&&sel.code===kg){tlRender();recard()}return d}).catch(function(){delete tl.kg[kg]});return e.p}
function tlData(){if(sel&&tl.kg[sel.code]&&tl.kg[sel.code].d)return tl.kg[sel.code].d;return tl.d}
/* res-7 counts inside the window: idx → [confirmed, candidate] (a cell that moved on two passes counts once) */
function tlMap(){tl.map=null;if(!tl.d)return;var m={},rows=tl.d.cells||[],days=tl.d.days||[],i;for(i=0;i<rows.length;i++){var r=rows[i];if(tl.win&&!tlHas(tl.win,r[1],days[r[2]]))continue;var v=m[r[0]];if(!v)v=m[r[0]]=[0,0,0];v[0]+=r[3];v[1]+=r[4];if(r[2]>v[2])v[2]=r[2]}for(var k in m)if(m[k][0])m[k][1]=0;tl.map=m}
function tlVal(i){if(!tl.win||!tl.map)return null;return tl.map[i]||[0,0]}
/* bins of the current level: passes (level 0) or the detection days of pass tl.k (level 1), from the table in scope */
var TL_LBL=['','baseline','2nd','3rd','4th','5th','6th','7th','8th','9th','10th','11th','12th'];
function tlDay(s){return new Date(s+'T00:00:00Z')}function tlISO(d){return d.toISOString().slice(0,10)}
function tlBins(){var d=tlData();tl.bins=[];if(!d)return;var ps=d.passes||[],i,j;
 if(tl.lvl===0){for(i=0;i<ps.length;i++){var p=ps[i];tl.bins.push({k:p.k,w:{k1:p.k,k2:p.k,d1:'',d2:''},lab:TL_LBL[p.k]||p.k+'th',conf:p.confirmed,cand:p.candidate,dlu:p.d_lu_m2,str:p.d_str_n,share:p.share,done:p.done,first:p.first,eta:p.eta,etaDay:p.eta_day,rate:p.rate_per_day,from:p.from,to:p.to})}return}
 var p=null,R=[tl.k,tl.k],sel2=[];if(tl.lvl===2){R=tl.win&&tl.win.k1?[tl.win.k1,tl.win.k2]:[0,0];var fr='',to='',bd2={},cd2={};for(i=0;i<ps.length;i++){var q=ps[i];if(R[0]&&(q.k<R[0]||q.k>R[1]))continue;if(q.from&&(!fr||q.from<fr))fr=q.from;if(q.to>to)to=q.to;var dk;for(dk in q.by_day||{}){var t0=q.by_day[dk],t1=bd2[dk]||(bd2[dk]={cells:0,d_str_n:0,d_lu_m2:[0,0,0,0,0,0,0,0,0]});t1.cells+=t0.cells||0;t1.d_str_n+=t0.d_str_n||0;if(t0.d_lu_m2)for(j=0;j<9;j++)t1.d_lu_m2[j]+=t0.d_lu_m2[j]||0}for(dk in q.candidate_by_day||{})cd2[dk]=(cd2[dk]||0)+q.candidate_by_day[dk]}p={k:R[0],from:fr,to:to,by_day:bd2,candidate_by_day:cd2}}else{for(i=0;i<ps.length;i++)if(ps[i].k===tl.k)p=ps[i]}if(!p)return;var a=p.from||p.first||d.epoch,b=p.done||tlISO(new Date());/* a place with no history yet still gets the calendar (from the epoch): the window stays scrubbable, the bins read zero */if(p.to&&p.to>b)b=p.to;if(!a)return;var A=tlDay(a),B=tlDay(b),span=(B-A)/864e5,unit=span<=45?'d':span<=26*7?'w':span<=730?'m':'q',cur=new Date(A),bd=p.by_day||{},cd=p.candidate_by_day||{};
 var start=function(t){t=new Date(t);if(unit==='w'){var wd=(t.getUTCDay()+6)%7;t.setUTCDate(t.getUTCDate()-wd)}else if(unit==='m')t.setUTCDate(1);else if(unit==='q'){t.setUTCMonth(t.getUTCMonth()-t.getUTCMonth()%3);t.setUTCDate(1)}return t},next=function(t){t=new Date(t);if(unit==='d')t.setUTCDate(t.getUTCDate()+1);else if(unit==='w')t.setUTCDate(t.getUTCDate()+7);else if(unit==='m')t.setUTCMonth(t.getUTCMonth()+1);else t.setUTCMonth(t.getUTCMonth()+3);return t};
 cur=start(A);var n=0;while(cur<=B&&n++<400){var nx=next(cur),d1=tlISO(cur),d2=tlISO(new Date(nx-864e5)),bin={k:p.k,w:{k1:R[0],k2:R[1],d1:d1,d2:d2},unit:unit,conf:0,cand:0,dlu:[0,0,0,0,0,0,0,0,0],str:0};
  bin.lab=unit==='d'?cur.getUTCDate()+(cur.getUTCDate()===1||n===1?' '+MON[cur.getUTCMonth()]:''):unit==='w'?cur.getUTCDate()+' '+MON[cur.getUTCMonth()]:unit==='m'?MON[cur.getUTCMonth()]+(cur.getUTCMonth()===0||n===1?' '+cur.getUTCFullYear():''):'Q'+((cur.getUTCMonth()/3|0)+1)+' '+cur.getUTCFullYear();
  for(var dk in bd)if(dk>=d1&&dk<=d2){var t=bd[dk];bin.conf+=t.cells||0;bin.str+=t.d_str_n||0;if(t.d_lu_m2)for(j=0;j<9;j++)bin.dlu[j]+=t.d_lu_m2[j]||0}for(dk in cd)if(dk>=d1&&dk<=d2)bin.cand+=cd[dk]||0;tl.bins.push(bin);cur=nx}}
/* the window's figures (confirmed cells · Δ per group · from → to) summed over the bins it covers, for the readout and the KG card */
function tlSum(w,d){d=d||tlData();var o={conf:0,cand:0,dlu:[0,0,0,0,0,0,0,0,0],str:0,from:'',to:''};if(!d||!w)return o;var ps=d.passes||[],i,j;for(i=0;i<ps.length;i++){var p=ps[i];if(w.k1&&(p.k<w.k1||p.k>w.k2))continue;if(!w.d1){o.conf+=p.confirmed;o.cand+=p.candidate;o.str+=p.d_str_n||0;for(j=0;j<9;j++)o.dlu[j]+=p.d_lu_m2[j]||0;if(p.from&&(!o.from||p.from<o.from))o.from=p.from;if(p.to>o.to)o.to=p.to;continue}var bd=p.by_day||{},cd=p.candidate_by_day||{};for(var dk in bd)if(dk>=w.d1&&dk<=w.d2){var t=bd[dk];o.conf+=t.cells||0;o.str+=t.d_str_n||0;if(t.d_lu_m2)for(j=0;j<9;j++)o.dlu[j]+=t.d_lu_m2[j]||0;if(!o.from||dk<o.from)o.from=dk;if(dk>o.to)o.to=dk}for(dk in cd)if(dk>=w.d1&&dk<=w.d2)o.cand+=cd[dk]||0}return o}
function tlDate(s){if(!s)return'';var d=tlDay(s);return d.getUTCDate()+' '+MON[d.getUTCMonth()]+' '+d.getUTCFullYear()}function tlMonth(s){if(!s)return'';var d=tlDay(s+'-01');return MON[d.getUTCMonth()]+' '+d.getUTCFullYear()}
function tlSpan(a,b){if(!a)return'';if(!b||a===b)return tlDate(a);var A=tlDay(a),B=tlDay(b);if(A.getUTCFullYear()===B.getUTCFullYear()&&A.getUTCMonth()===B.getUTCMonth())return A.getUTCDate()+'\u2013'+B.getUTCDate()+' '+MON[A.getUTCMonth()]+' '+A.getUTCFullYear();if(A.getUTCFullYear()===B.getUTCFullYear())return A.getUTCDate()+' '+MON[A.getUTCMonth()]+' \u2013 '+B.getUTCDate()+' '+MON[B.getUTCMonth()]+' '+A.getUTCFullYear();return tlDate(a)+' \u2013 '+tlDate(b)}
/* label of the window, the way the legend and the card say it */
function tlLabel(w){w=w||tl.win;if(!w)return'';var d=tlData(),ps=d?d.passes||[]:[],p=null,i;for(i=0;i<ps.length;i++)if(ps[i].k===w.k1)p=ps[i];var s=!w.k1?'every pass':w.k1===w.k2?(TL_LBL[w.k1]||w.k1+'th')+' pass':(TL_LBL[w.k1]||w.k1)+'\u2013'+(TL_LBL[w.k2]||w.k2)+' pass';if(w.d1)s+=' \u00b7 detected '+tlSpan(w.d1,w.d2);else if(p&&p.from)s+=' \u00b7 detected '+tlSpan(p.from,p.to);return s}
function tlPassNote(p){if(!p)return'';if(p.done)return'complete '+tlDate(p.done);var s=pct0(p.share)+' of the land compared';if(p.eta)s+=' \u00b7 complete \u2248 '+tlMonth(p.eta)+' at this pace';return s}
function pct0(x){return Math.round((x||0)*100)+'\u202f%'}
/* ── render ── one row: head (scope · pass state · all · play) · strip of bins · readout. Updates in place so bars, fills and selection animate (CSS transitions); a change of level or scope swaps the strip with a short slide; the whole strip unfolds/folds with the change toggle (grid-rows transition on .glyph-time). */
function tlRender(){if(!tlEl)return;var show=cg.on&&(tl.d||tl.busy||tl.err);if(show&&tlEl.hidden){tlEl.hidden=false;requestAnimationFrame(function(){tlEl.classList.add('on')})}else if(!show){tlEl.classList.remove('on');tlDateSet('');clearTimeout(tl.hT);tl.hT=setTimeout(function(){if(!tlEl.classList.contains('on')){tlEl.hidden=true;tlEl.innerHTML='';tl.sig=''}},420);return}
 tlBins();var d=tlData(),scope=sel&&d&&d.scope==='kg'?sel.name||('KG '+sel.kg):'Austria',i;
 if(tl.busy&&!tl.d){tlSet('<div class="tl-h"><span class="tl-t mut">change over time'+ldots()+'</span></div>','wait');return}
 if(!d||!(d.passes||[]).length){tlSet('<div class="tl-h"><span class="tl-t mut">'+(tl.err?offTxt('change'):(sel?'no second pass yet in '+esc(scope):'no second pass yet'))+'</span></div>','none');return}
 var mx=1;for(i=0;i<tl.bins.length;i++)mx=Math.max(mx,tl.bins[i].conf+tl.bins[i].cand);var p2=null;for(i=0;i<d.passes.length;i++)if(d.passes[i].k===2)p2=d.passes[i];
 var sig=(d.scope||'')+'|'+(d.kg||'')+'|'+tl.lvl+'|'+tl.k+'|'+tl.bins.map(function(b){return b.lab}).join(','),fresh=sig!==tl.sig;
 if(fresh){var dir=tl.sig&&tl.sig.split('|')[2]<tl.lvl?'dn':tl.sig&&tl.sig.split('|')[2]>tl.lvl?'up':'';tl.sig=sig;var h='<div class="tl-h"><button type="button" class="gt-b tl-up" aria-label="back to the passes" title="back to the passes ( esc )"'+(tl.lvl?'':' hidden')+'><svg viewBox="0 0 16 16" aria-hidden="true"><path d="M10 3.5 5.5 8l4.5 4.5"/></svg></button><span class="tl-t"></span><span class="tl-c"><button type="button" class="gt-b tl-st" data-d="-1" aria-label="earlier" title="earlier ( &lt; )"><svg viewBox="0 0 16 16" aria-hidden="true"><path d="M9.5 4 5.5 8l4 4"/></svg></button><button type="button" class="gt-b tl-st" data-d="1" aria-label="later" title="later ( &gt; )"><svg viewBox="0 0 16 16" aria-hidden="true"><path d="m6.5 4 4 4-4 4"/></svg></button><button type="button" class="gt-b tl-cal" aria-label="calendar" title="by date, across the passes"><svg viewBox="0 0 16 16" aria-hidden="true"><rect x="2" y="3.5" width="12" height="10.5" rx="1.5"/><path d="M2 7h12M5.5 2v3M10.5 2v3"/></svg></button><button type="button" class="gt-b tl-all" aria-label="every pass" title="every pass"><svg viewBox="0 0 16 16" aria-hidden="true"><path d="M2.5 8a5.5 5.5 0 1 0 1.6-3.9"/><path d="M2.2 1.8v3.2h3.2"/></svg><span class="gt-t">all</span></button><button type="button" class="gt-b tl-play" aria-label="play" title="play the change through time \u00b7 recent change bright, old change fades to a trace ( space )"><svg viewBox="0 0 16 16" aria-hidden="true"><path class="pl" d="M4.5 2.8v10.4L13 8z"/><g class="pa"><rect x="3.5" y="2.8" width="3.2" height="10.4" rx=".8"/><rect x="9.3" y="2.8" width="3.2" height="10.4" rx=".8"/></g></svg></button></span></div><div class="tl-s'+(dir?' '+dir:'')+'" role="group" aria-label="'+(tl.lvl===0?'passes':'detection period')+'">';
  for(i=0;i<tl.bins.length;i++){var b=tl.bins[i];h+='<button type="button" class="tl-b" data-i="'+i+'"><i class="tl-k"></i>'+(tl.lvl===0?'<i class="tl-f"></i>':'')+'<i class="tl-m" style="--d:0px"></i><span class="tl-l">'+esc(b.lab)+'</span></button>'}
  h+='<i class="tl-hd" aria-hidden="true"><span></span></i></div><div class="tl-r"></div><i class="tl-ld" aria-hidden="true"></i>';tlSet(h,'');tlEl.offsetWidth}
 /* in place: head, every bin's bars + state, readout */
 var tt=tlEl.querySelector('.tl-t');if(tt){var ht=tl.lvl===0?'<b>'+esc(scope)+'</b> <span class="mut"><span class="tl-x">\u00b7 change by pass</span>'+(p2?' \u00b7 2nd pass '+(p2.done?'complete '+tlDate(p2.done):pct0(p2.share)+(p2.eta?' \u00b7 \u2248 '+tlMonth(p2.eta):'')):'')+'</span>':'<b>'+(tl.lvl===2?(tl.win&&tl.win.k1?(tl.win.k1===tl.win.k2?(TL_LBL[tl.win.k1]||tl.win.k1)+' pass':(TL_LBL[tl.win.k1]||tl.win.k1)+'\u2013'+(TL_LBL[tl.win.k2]||tl.win.k2)+' pass'):'every pass'):(TL_LBL[tl.k]||tl.k)+' pass')+'</b> <span class="mut">\u00b7 '+esc(scope)+' \u00b7 detected per '+({d:'day',w:'week',m:'month',q:'quarter'})[tl.bins.length?tl.bins[0].unit:'d']+'</span>';if(tt.innerHTML!==ht)tt.innerHTML=ht}
 var ba=tlEl.querySelector('.tl-all');if(ba)ba.classList.toggle('is',!tl.win);var bc=tlEl.querySelector('.tl-cal');if(bc)bc.setAttribute('aria-pressed',tl.lvl===2?'true':'false');var bp=tlEl.querySelector('.tl-play');if(bp){var run=tl.play&&!tl.play.paused;bp.setAttribute('aria-pressed',run?'true':'false');bp.classList.toggle('paused',!!(tl.play&&tl.play.paused));bp.title=run?'pause ( space )':tl.play?'resume ( space )':'play the change through time \u00b7 recent change bright, old change fades to a trace ( space )';bp.setAttribute('aria-label',run?'pause':tl.play?'resume':'play')}
 var bs=tlEl.querySelectorAll('.tl-b');for(i=0;i<bs.length&&i<tl.bins.length;i++){var b=tl.bins[i],el=bs[i],inW=tlIn(b),hc=Math.sqrt(b.conf/mx),ha2=Math.sqrt((b.conf+b.cand)/mx);el.classList.toggle('in',!!inW);el.classList.toggle('out',!!tl.win&&!inW);el.classList.toggle('open',tl.lvl===0&&!b.done);el.classList.toggle('nil',!b.conf&&!b.cand);el.setAttribute('aria-pressed',inW?'true':'false');
  el.title=(tl.lvl===0?b.lab+' pass \u00b7 '+(b.done?'complete '+tlDate(b.done):pct0(b.share)+' compared')+' \u00b7 ':b.lab+' \u00b7 ')+fmt(b.conf)+' confirmed'+(b.cand?' \u00b7 '+fmt(b.cand)+' candidate':'')+(chgMoved(b.dlu)?' \u00b7 '+areaU(chgMoved(b.dlu))+' redeclared':'');
  el.classList.toggle('ca',!b.conf&&!!b.cand);var f=el.querySelector('.tl-f');if(f)f.style.width=(b.done?0:Math.round(b.share*100))+'%';el.querySelector('.tl-m').style.setProperty('--d',(b.conf||b.cand?Math.round(3+6*ha2):0)+'px')}
 var hd=tlEl.querySelector('.tl-hd');if(hd){var hi=-1;if(tl.lvl&&tl.win&&tl.win.d2)for(i=0;i<tl.bins.length;i++)if(tl.bins[i].w.d2<=tl.win.d2)hi=i;hd.style.left=hi<0?'0%':((hi+1)/tl.bins.length*100)+'%';hd.classList.toggle('on',hi>=0);hd.classList.toggle('r',hi>=0&&(hi+1)/tl.bins.length>.8);var hb=hi>=0?tl.bins[hi]:null;if(hb){var u=hb.unit,D=tlDay(hb.w.d2),ht=u==='d'?D.getUTCDate()+' '+MON[D.getUTCMonth()]+' '+D.getUTCFullYear():u==='w'?'week of '+tlDate(hb.w.d1):u==='m'?MON[D.getUTCMonth()]+' '+D.getUTCFullYear():hb.lab;tlDateSet(ht)}else tlDateSet('')}
 tlReadoutSync()}
function tlIn(b){if(!tl.win)return false;if(tl.lvl===0)return!tl.win.d1&&tl.win.k1<=b.k&&b.k<=tl.win.k2;if(tl.lvl===2)return!!tl.win.d1&&tl.win.d1<=b.w.d2&&b.w.d1<=tl.win.d2;return tl.win.k1<=b.k&&b.k<=tl.win.k2&&(!tl.win.d1||(tl.win.d1<=b.w.d2&&b.w.d1<=tl.win.d2))}
/* the head date sits left-bound on the tools row (not above the marker, where it collided with the strip) */
var gtD=fig.querySelector('.gt-dt');function tlDateSet(t){if(!gtD)return;if(gtD.textContent!==t)gtD.textContent=t;gtD.hidden=!t}
function tlSet(h,mode){tlEl.innerHTML=h;tlEl.dataset.mode=mode}
/* the readout: what the window holds — cells, area, from → to, and the honesty line (how much of the land is compared / when the pass closed) */
function tlReadout(){var d=tlData();if(!d)return'';var w=tl.win,b=tl.hov>=0?tl.bins[tl.hov]:null,o,lab,note='';
 if(b){o=b;lab=tl.lvl===0?b.lab+' pass':'detected '+(b.unit==='d'?tlDate(b.w.d1):tlSpan(b.w.d1,b.w.d2));if(tl.lvl===0)note=tlPassNote(b)}
 else if(w){o=tlSum(w,d);lab=tlLabel(w);var ps=d.passes||[];for(var i=0;i<ps.length;i++)if(ps[i].k===w.k1&&w.k1===w.k2)note=tlPassNote(ps[i])}
 else{o=tlSum({k1:2,k2:99,d1:'',d2:''},d);lab='all passes';var p2=null;for(i=0;i<(d.passes||[]).length;i++)if(d.passes[i].k===2)p2=d.passes[i];if(p2)note=p2.done?'2nd pass complete '+tlDate(p2.done):'2nd pass '+tlPassNote(p2)}
 var mv=chgMoved(o.dlu),s='<span class="tl-k">'+esc(lab)+'</span> <span class="tl-v">\u00b7 <span class="c">\u25cf</span> '+fmt(o.conf)+' confirmed'+(o.cand?' \u00b7 <span class="c ca">\u25cf</span> '+fmt(o.cand)+' candidate':'')+(mv?' \u00b7 <b>'+areaU(mv)+'</b> redeclared':'')+(o.str?' \u00b7 structures '+(o.str>0?'+':'\u2212')+Math.abs(o.str):'')+'</span>'+(note?'<span class="tl-n">'+esc(note)+'</span>':'')+(mv?dluBar(o.dlu,'declared use moved',4):'');return s}
function tlReadoutSync(){var r=tlEl&&tlEl.querySelector('.tl-r');if(!r)return;var h=tlReadout();if(r.innerHTML===h)return;r.classList.add('sw');r.innerHTML=h;requestAnimationFrame(function(){r.classList.remove('sw')})}
function tlBusy(on){if(tlEl)tlEl.classList.toggle('busy',!!on)}
/* ── set the window: tiles, dots, faces, legend, card, hash ── */
function setTime(w,quiet){var s0=tlStr(),s1=tlStr(w);if(s0===s1)return;tl.win=w?{k1:w.k1,k2:w.k2,d1:w.d1||'',d2:w.d2||''}:null;dbgLog('time '+(s1||'all'));tlMap();hi.docs=[];if(!quiet&&tl.play&&!tl.play.auto)tlStop();
 if(w&&!cg.on)setChg(true,quiet);tlRender();tlPaint();if(tl.play&&tl.play.auto){tl.play.dirty=true;return}if(tl.dg){tl.dg.dirty=true;return}/* a drag paints the dots per step; tiles, card and legend catch up on release */tlCatchUp();if(!quiet)setHash()}
function tlCatchUp(){if(hi.on)hiView(true);if(hi.pin)tlRepin();else recard();hiLegend()}
function tlRepin(){var c=hi.pin.cell;hiFetch('/api/v1/ne/glyph/cell?cell='+c+tlQ()).then(function(d){if(d&&hi.pin&&hi.pin.cell===c&&pk&&pk.t==='h3'){pk.d=d;hi.pin=d;recard()}}).catch(function(){})}
/* dots and hex faces: once the pass table is in, the server's .c/.ca paths step aside for ours (every pass when no window, aged to today), one path per tone × detection day. Additive: a cell already painted stays put (a stronger tone is laid on top), only cells new to the window fade in — so play lets the change accumulate on the map; when a cell leaves the window (narrower window, other scope) the whole set is redrawn with a crossfade. Age: each group's opacity follows its distance from the head of the window (tlHead): full for six weeks, then easing out over a year to a faint, desaturated trace (opacity floor .18, colour mixed toward the muted ink by --age) — recent change bright, old change rests but is still there. */
var TL_FULL=45,TL_GONE=365;
/* the scale adapts to the history there is (mirrors neGlyphAgeScale): with less than a year behind the head the span itself is the scale — full = 12 % of it (≥ 1 d), gone = the span (≥ 10 d) */
function tlScale(head){var full=TL_FULL,gone=TL_GONE,ep=tl.d&&tl.d.epoch;if(ep&&head){var span=(tlDay(head)-tlDay(ep))/864e5;if(span<TL_GONE){gone=Math.max(10,span);full=Math.max(1,Math.min(TL_FULL,.12*span))}}return[full,gone]}
function tlHead(){if(!tl.d)return'';if(!tl.win)return tlISO(new Date());if(tl.win.d2)return tl.win.d2;var ps=tl.d.passes||[],t='';for(var i=0;i<ps.length;i++)if(ps[i].k>=tl.win.k1&&ps[i].k<=tl.win.k2&&ps[i].to>t)t=ps[i].to;return t}
function tlAge(day,head,sc){if(!head||!day)return 1;sc=sc||tlScale(head);var a=(tlDay(head)-tlDay(day))/864e5;if(a<=sc[0])return 1;if(a>=sc[1])return 0;var x=(a-sc[0])/(sc[1]-sc[0]);return 1-x*x*(3-2*x)}
function tlAgeSet(p,age){var o=((+p.dataset.b||1)*(.18+.82*age)).toFixed(3);if(p.style.opacity!==o){p.style.opacity=o;p.style.setProperty('--age',age.toFixed(3))}}
function tlPaint(force){var groups=[dots].concat(hexTiles.map(function(t){return t.g})),g,i,k,ix,days=tl.d?tl.d.days||[]:[];
 var live=cg.on&&tl.d&&tl.map,want={};if(live)for(ix in tl.map){var v=tl.map[ix];if(!cells[+ix])continue;want[ix]=(v[0]?'c':'ca')+chgB(v[0]||v[1])+'@'+v[2]}
 var add=!force&&tl.pt;if(add)for(ix in tl.pt)if(!want[ix]){add=false;break}
 if(!add){for(g=0;g<groups.length;g++){var G=groups[g];if(!G)continue;var old=G.querySelectorAll('path.tl');for(i=0;i<old.length;i++){(function(p){p.style.opacity='0';setTimeout(function(){if(p.parentNode)p.parentNode.removeChild(p)},380)})(old[i])}var orig=G.querySelectorAll('path.c:not(.tl),path.ca:not(.tl),path.ch:not(.tl)');for(i=0;i<orig.length;i++)orig[i].style.display=live?'none':''}tl.pt=live?{}:null;tl.pg={}}
 if(!live)return;var head=tlHead(),sc=tlScale(head),bk={},hl={},n=0;for(ix in want){if(tl.pt[ix]===want[ix])continue;tl.pt[ix]=want[ix];n++;var c=cells[+ix],key=want[ix],at=key.indexOf('@'),cls=key.charAt(1)==='a'?'ca':'c',bb=+key.slice(cls.length,at),di=+key.slice(at+1);
  var dd='M'+c[0]+' '+c[1]+'h0',fd=hexBuilt?faceD(+ix):'';(bk[key]=bk[key]||{d:[],f:{},cls:cls,b:bb,day:days[di]||''}).d.push(dd);(hl[di]=hl[di]||[]).push(dd);if(hexBuilt){var T=560,tk=((c[0]/T)|0)+'_'+((c[1]/T)|0);(bk[key].f[tk]=bk[key].f[tk]||[]).push(fd)}}
 var ns='http://www.w3.org/2000/svg',mk=function(G,B,d,stroke){var p=document.createElementNS(ns,'path');p.setAttribute('class',B.cls+' tl');if(B.cls!=='ch')p.setAttribute(stroke?'stroke-opacity':'fill-opacity',(B.cls==='c'?CHGB:CANB)[B.b]);p.setAttribute('d',d);p.dataset.day=B.day;p.dataset.b=B.cls==='ch'?'.16':'1';p.style.opacity='0';G.appendChild(p);(tl.pg[B.day]=tl.pg[B.day]||[]).push(p);requestAnimationFrame(function(){tlAgeSet(p,tlAge(B.day,head,sc))})};
 for(k in hl)mk(dots,{cls:'ch',day:days[+k]||''},hl[k].join(''),true);for(k in bk){var B=bk[k];mk(dots,B,B.d.join(''),true);if(hexBuilt)for(var tk in B.f){for(i=0;i<hexTiles.length;i++){var t=hexTiles[i];if(((t.b[0]+20)/560|0)+'_'+((t.b[1]+20)/560|0)===tk){mk(t.g,B,B.f[tk].join(''),false);break}}}}
 if(!n||add)tlAgeSync(head)}
function tlAgeSync(head){var day,i,sc=tlScale(head);for(day in tl.pg){var age=tlAge(day,head,sc),ps=tl.pg[day];for(i=ps.length-1;i>=0;i--){var p=ps[i];if(!p.parentNode){ps.splice(i,1);continue}tlAgeSet(p,age)}}}
/* ── play: the window grows bin by bin (cumulative) from the first bin, rests on the whole, then ends; space / the button pause and resume where the head stands; a hand on the window (click, drag, keys, all) ends the run ── */
function tlPlay(){if(tl.play){if(tl.play.paused){tlResume()}else{tlPause()}return}if(!tl.d)return;if(tl.lvl!==2){tl.lvl=2;tl.hov=-1;tlRender()}if(!tl.bins.length)return;var i=0;if(tl.win&&tl.win.d2){for(var j=0;j<tl.bins.length;j++)if(tl.bins[j].w.d2<=tl.win.d2)i=j+1;if(i>=tl.bins.length)i=0}dbgLog('time play');pause(0);if(typeof buildHex==='function')buildHex();tl.play={i:i,t:0,auto:false,paused:false,dirty:false};tlTick();tlRender()}
function tlTick(){var P=tl.play;if(!P||P.paused)return;var u=tl.bins.length?tl.bins[0].unit:'d',step=({d:520,w:760,m:950,q:1100})[u]||700;if(matchMedia('(hover:none)').matches)step*=1.25;if(matchMedia('(prefers-reduced-motion: reduce)').matches)step*=1.6;if(document.hidden){P.t=setTimeout(tlTick,step);return}
 if(P.i>=tl.bins.length){P.t=setTimeout(function(){tlStop()},step*1.4);return}var b0=tl.bins[0],b=tl.bins[P.i];P.auto=true;setTime({k1:b0.w.k1,k2:b0.w.k2,d1:b0.w.d1,d2:b.w.d2},true);setHash();P.auto=false;P.i++;P.t=setTimeout(tlTick,step)}
function tlPause(){if(!tl.play||tl.play.paused)return;clearTimeout(tl.play.t);tl.play.paused=true;dbgLog('time pause');tlRender();if(tl.play.dirty){tl.play.dirty=false;tlCatchUp()}}
function tlResume(){if(!tl.play||!tl.play.paused)return;tl.play.paused=false;dbgLog('time resume');tlRender();tlTick()}
function tlStop(){if(!tl.play)return;clearTimeout(tl.play.t);var d=tl.play.dirty;tl.play=0;dbgLog('time stop');tlRender();if(d)tlCatchUp();pause(12000)}
/* ── step with < > (and , .): the next bin at the current level; past the end → all ── */
function tlStep(dir){if(!tl.bins.length){if(cg.on)tlLoad();return}tlStop();var i=-1,n=tl.bins.length;for(var j=0;j<n;j++){var b=tl.bins[j];if(tl.win&&tlHas(tl.win,b.k,b.w.d1)&&(tl.lvl===0?!tl.win.d1:tl.win.d1<=b.w.d2&&b.w.d1<=tl.win.d2)){i=dir>0?j:(i<0?j:i)}}
 if(!tl.win)i=dir>0?-1:n;else i+=dir;if(i<0||i>=n){setTime(null);return}setTime(tl.bins[i].w)}
/* ── strip interaction: click = that bin, shift-click = extend, click the selected pass again / double-click / wheel down = into its days, crumb / wheel up = back ── */
if(tlEl){tlEl.addEventListener('click',function(e){if(tl.sup&&Date.now()-tl.sup<300){tl.sup=0;return}var t=e.target.closest('button');if(!t)return;if(t.classList.contains('tl-all')){tlStop();setTime(null);return}if(t.classList.contains('tl-st')){tlStep(+t.dataset.d);return}if(t.classList.contains('tl-play')){tlPlay();return}if(t.classList.contains('tl-up')){tlStop();tl.lvl=0;tl.hov=-1;tlRender();return}if(t.classList.contains('tl-cal')){tlStop();tl.lvl=tl.lvl===2?0:2;tl.hov=-1;tlRender();return}if(!t.classList.contains('tl-b'))return;var b=tl.bins[+t.dataset.i];if(!b)return;tlStop();
  if(e.shiftKey&&tl.win){var w=tl.lvl===0?{k1:Math.min(tl.win.k1,b.k),k2:Math.max(tl.win.k2,b.k),d1:'',d2:''}:{k1:b.k,k2:b.k,d1:tl.win.d1&&tl.win.d1<b.w.d1?tl.win.d1:b.w.d1,d2:tl.win.d2&&tl.win.d2>b.w.d2?tl.win.d2:b.w.d2};setTime(w);return}
  var same=tl.win&&tlStr(tl.win)===tlStr(b.w);if(same&&tl.lvl===0){tl.lvl=1;tl.k=b.k;tl.hov=-1;tlRender();return}setTime(b.w)});
 /* drag on the strip, three grips: the head hairline (finger within a thumb of it) drags the window's END — the head follows the finger
    continuously, the dots snap by bin, and it settles on the bin edge on release; a press inside the window SLIDES it (width kept);
    a press outside (or shift) selects a RANGE from the anchor bin. No transition while the finger is down. */
 function tlGrip(e){var t=e.target.closest('.tl-s');if(!t||e.button)return null;var b0=e.target.closest('.tl-b'),bs=t.querySelectorAll('.tl-b'),s0=-1,s1=-1,q;for(q=0;q<bs.length;q++)if(bs[q].classList.contains('in')){if(s0<0)s0=q;s1=q}var hd=t.querySelector('.tl-hd'),mode='range';if(hd&&hd.classList.contains('on')&&s0>=0&&!e.shiftKey){var hx=hd.getBoundingClientRect().left,bw=bs.length?bs[0].getBoundingClientRect().width:24,th=e.pointerType==='mouse'?Math.min(10,Math.max(5,bw*.5)):Math.min(22,Math.max(10,bw*.7));if(Math.abs(e.clientX-hx)<=th)mode='head'}if(mode==='range'&&!e.shiftKey&&b0&&s0>=0&&b0.classList.contains('in'))mode='slide';return {x:e.clientX,i:b0?+b0.dataset.i:-1,moved:false,id:e.pointerId,s0:s0,s1:s1,mode:mode,t:t,hd:hd}}
 function tlBinAt(t,x){var bs=t.querySelectorAll('.tl-b'),j;for(j=0;j<bs.length;j++){var r=bs[j].getBoundingClientRect();if(x>=r.left&&x<r.right)return j}return x<t.getBoundingClientRect().left?0:bs.length-1}
 tlEl.addEventListener('pointerdown',function(e){var D=tlGrip(e);if(!D)return;tl.dg=D;tlEl.classList.add('dg');if(D.mode==='head')D.t.classList.add('hd-dg');try{D.t.setPointerCapture(e.pointerId)}catch(x){}if(e.pointerType!=='mouse')e.preventDefault()});
 tlEl.addEventListener('pointermove',function(e){var D=tl.dg;if(!D||e.pointerId!==D.id)return;var t=D.t;if(!t.isConnected){tl.dg=null;return}if(!D.moved&&Math.abs(e.clientX-D.x)<(D.mode==='head'?1:4))return;D.moved=true;var n=t.querySelectorAll('.tl-b').length,i=tlBinAt(t,e.clientX);if(D.i<0)D.i=i;var a,b;
  if(D.mode==='head'){var tr=t.getBoundingClientRect(),f=(e.clientX-tr.left)/tr.width;/* the finger sits on the head, which stands at the END edge of a bin: the bin under the window's end is the one whose right edge is nearest */var hi=Math.round(f*n)-1;if(hi<D.s0)hi=D.s0;if(hi>n-1)hi=n-1;a=tl.bins[D.s0];b=tl.bins[hi];if(D.hd){var lo=(D.s0+1)/n,fx=Math.max(lo,Math.min(1,f));D.hd.style.left=(fx*100)+'%';D.hd.classList.toggle('r',fx>.8)}i=hi}
  else if(D.mode==='slide'){var sh=i-D.i;if(D.s0+sh<0)sh=-D.s0;if(D.s1+sh>n-1)sh=n-1-D.s1;a=tl.bins[D.s0+sh];b=tl.bins[D.s1+sh]}
  else{a=tl.bins[Math.min(D.i,i)];b=tl.bins[Math.max(D.i,i)]}
  if(!a||!b)return;tlStop();var w=tl.lvl===0?{k1:a.k,k2:b.k,d1:'',d2:''}:{k1:a.k,k2:a.k,d1:a.w.d1,d2:b.w.d2};if(tlStr(w)!==tlStr()){setTime(w,true);if(D.mode==='head'&&D.hd&&navigator.vibrate&&e.pointerType==='touch')try{navigator.vibrate(3)}catch(x){}}if(D.mode==='head'&&D.hd){var fx2=Math.max((D.s0+1)/n,Math.min(1,(e.clientX-t.getBoundingClientRect().left)/t.getBoundingClientRect().width));D.hd.style.left=(fx2*100)+'%'}tl.hov=i;tlReadoutSync()});
 function tlDragEnd(e){var D=tl.dg;if(!D||(e&&e.pointerId!==D.id))return;tl.dg=null;D.t.classList.remove('hd-dg');tlEl.classList.remove('dg');tlRender();if(D.dirty)tlCatchUp();if(D.moved){setHash();tl.sup=Date.now()}}
 tlEl.addEventListener('pointerup',tlDragEnd);tlEl.addEventListener('pointercancel',tlDragEnd);tlEl.addEventListener('lostpointercapture',function(e){if(tl.dg&&e.pointerId===tl.dg.id)tlDragEnd(e)});
 tlEl.addEventListener('dblclick',function(e){var t=e.target.closest('.tl-b');if(!t||tl.lvl!==0)return;var b=tl.bins[+t.dataset.i];if(!b)return;e.preventDefault();tl.lvl=1;tl.k=b.k;tl.hov=-1;tlRender()});
 tlEl.addEventListener('wheel',function(e){var t=e.target.closest('.tl-s');if(!t)return;e.preventDefault();var now=Date.now();if(now-(tl.wt||0)<400)return;tl.wt=now;if(e.deltaY>0&&tl.lvl===0){var b=e.target.closest('.tl-b');var bb=b?tl.bins[+b.dataset.i]:null;tl.lvl=bb?1:2;if(bb)tl.k=bb.k;tl.hov=-1;tlRender()}else if(e.deltaY<0&&tl.lvl){tl.lvl=0;tl.hov=-1;tlRender()}},{passive:false});
 tlEl.addEventListener('click',function(e){var b=e.target.closest('.lg-m');if(!b)return;e.stopPropagation();var x=b.nextElementSibling,on=x.hidden;x.hidden=!on;b.setAttribute('aria-expanded',on?'true':'false')},true);tlEl.addEventListener('pointerover',function(e){var t=e.target.closest('.tl-b');var i=t?+t.dataset.i:-1;if(i!==tl.hov){tl.hov=i;tlReadoutSync()}});tlEl.addEventListener('pointerleave',function(){if(tl.hov!==-1){tl.hov=-1;tlReadoutSync()}});
 tlEl.addEventListener('keydown',function(e){if(e.key==='ArrowLeft'||e.key==='ArrowRight'){e.preventDefault();tlStep(e.key==='ArrowRight'?1:-1)}else if(e.key===' '){e.preventDefault();tlPlay()}else if(e.key==='Escape'&&tl.lvl){e.preventDefault();tl.lvl=0;tlRender()}})}
/* the strip follows the change toggle (loads on first use) and the KG selection (its own table) */
function tlSync(){if(cg.on){tlLoad();if(sel)tlKGLoad(sel.code)}else if(tl.win){setTime(null,true)}tlMap();tlPaint(true);tlRender()}

function load_pending(){return !!load&&!data}
window.neGlyph={log:function(){return{errors:tel.err.slice(),fetch:JSON.parse(JSON.stringify(tel.n)),dbg:dbgL.slice()}},fault:function(m){if(arguments.length)net.fault=m||null;return net.fault},net:function(){netPrune();return{at:net.at,what:JSON.parse(JSON.stringify(net.what)),fault:net.fault,offline:fig.classList.contains('offline')}},logReset:function(){tel.err=[];tel.n={};dbgL.length=0},state:function(){var t=[];for(var k in hi.tiles){var e=hi.tiles[k];t.push({k:k,v:e.v,on:!!e.on,ok:!!e.ok,hid:!!e.hid,n:e.n,m:e.m})}var imgs=0,shown=0;hi.l&&Object.keys(hi.l).forEach(function(v){for(var i=0;i<3;i++){var g=hi.l[v][i];if(!g)continue;var im=g.querySelectorAll('image[data-t]');imgs+=im.length;for(var j=0;j<im.length;j++)if(im[j].style.display!=='none')shown++}});return{hash:location.hash,mem:{keep:HI_KEEP,devMem:DEVMEM,coarse:coarse,tiles:hi.order.length,images:imgs,shown:shown,evicted:hi.evicted||0,under:!!hi.under,underMiss:hi.on?hiUnderMiss(hiLOD()):'',decQ:dec.q.length,decN:dec.n,docs:hi.docs.length},view:view,profile:view||'declared',change:cg.on,diff:df.on,diffRaw:df.raw,overlays:{change:cg.on,diff:df.on,subsidy:sb.on,trees:hi.tree,relief:rl.on,ortho:ot.on,roads:rd.on},ortho:ot.on,orthoOp:ot.op,roads:rd.on,roadsOp:rd.op,orthoLeft:ot.q.length+ot.inflight,orthoZ:ot.cur,z:W/vb.w,vb:[vb.x,vb.y,vb.w,vb.h],full:hi.on,trees:hi.tree,relief:rl.on,subsidy:sb.on,subsidyLoaded:!!sb.d,time:tlStr(),timeLevel:tl.lvl,timePlaying:!!tl.play,timeLoaded:!!tl.d,reliefZ:rl.cur,reliefLeft:rl.q.length+rl.inflight,auto:{by:auto.by,opt:auto.opt},lod:hi.on?hiLOD():null,subFine:subFine(),left:hi.left,tiles:t,sel:sel?sel.code:null,pinned:pinned,pin:hi.pin?hi.pin.cell:null,loading:!!load&&!data,anchor:anchor,moved:moved,animating:animating,refits:refits,applying:applying,fly:!!hi.fly,tour:tour.run,animMs:animating?Math.round(performance.now()-animT0):0}},
 card:function(){return inner.innerText},legend:function(){var o=inner.querySelector('.lg12');return o?o.innerText:''},peek:function(){return peek.classList.contains('on')?peek.innerText:''},lonlat:function(x,y){return lonlat(x,y)},xy:function(lon,lat){return[px(lon),py(lat)]},
 client:function(lon,lat){var r=svg.getBoundingClientRect();return[r.left+(px(lon)-vb.x)/vb.w*r.width,r.top+(py(lat)-vb.y)/vb.h*r.height]},
 hover:function(lon,lat){var c=this.client(lon,lat),ev=new PointerEvent('pointermove',{clientX:c[0],clientY:c[1],pointerType:'mouse',pointerId:1,bubbles:true});main.dispatchEvent(new PointerEvent('pointerenter',{clientX:c[0],clientY:c[1],pointerType:'mouse',pointerId:1}));main.dispatchEvent(ev);return c},
 pin:function(lon,lat){pinAt([px(lon),py(lat)])},filter:function(f){if(arguments.length){fltSet(f||'');return}var s=fltSum();var a=lonlat(vb.x,vb.y+vb.h),b=lonlat(vb.x+vb.w,vb.y);return{f:flt.f,words:flt.words,open:flt.open,bins:s.bins,pending:s.pend,cells:s.n,match:s.m,ext:flt.ext,ignored:flt.ignored||[],readout:fN.textContent,bbox:[+a[0].toFixed(5),+a[1].toFixed(5),+b[0].toFixed(5),+b[1].toFixed(5)]}},view:function(v){if(v==='chg'){setView('');setChg(true)}else setView(v||'')},profile:function(v){setView(v==='observed'?'obs':v||'')},change:function(on){setChg(on)},diff:function(on,o){setDif(on,false,o&&o.raw)},trees:function(on){setTree(on)},relief:function(on){setRelief(on)},ortho:function(on,op){if(op!=null)setOrthoOp(op);setOrtho(on)},roads:function(on,op){if(op!=null)setRoadsOp(op);setRoads(on)},subsidy:function(on){setSub(on)},time:function(w){if(arguments.length){setTime(typeof w==='string'?tlParse(w):w);return this.settled()}return{win:tl.win,str:tlStr(),label:tlLabel(),scope:tlData()?tlData().scope:null,level:tl.lvl,k:tl.k,bins:tl.bins.map(function(b){return{lab:b.lab,w:tlStr(b.w),conf:b.conf,cand:b.cand,share:b.share,done:b.done}}),playing:!!tl.play,loaded:!!tl.d}},timeLevel:function(l,k){tl.lvl=l?1:0;if(k)tl.k=k;tlRender()},timeStep:function(d){tlStep(d)},timePlay:function(){tlPlay()},set:function(o){o=o||{};if('profile' in o)this.profile(o.profile);if('change' in o)setChg(o.change);if('diff' in o)setDif(o.diff,false,o.diffRaw);if('subsidy' in o)setSub(o.subsidy);if('trees' in o)setTree(o.trees);if('relief' in o)setRelief(o.relief);if('ortho' in o)setOrtho(o.ortho);if('orthoOp' in o)setOrthoOp(o.orthoOp);if('roads' in o)setRoads(o.roads);if('roadsOp' in o)setRoadsOp(o.roadsOp);return this.settled()},go:function(h){h=String(h||'');if(h.charAt(0)!=='#')h='#'+h;history.pushState(null,'',h);applyHash();return this.settled()},settled:function(ms){var self=this;return new Promise(function(res){var t0=Date.now();(function w(){var s=self.state();if((!applying&&!s.loading&&!s.left&&(!s.animating||Date.now()-t0>2500)&&!s.refits&&!(s.reliefLeft>0)&&!(s.orthoLeft>0)&&(!s.full||s.tiles.length)&&!(s.change&&!s.timeLoaded&&tl.ld)&&!load_pending())||Date.now()-t0>(ms||15000))return res(s);setTimeout(w,150)})()})}};
})();</script>`

// ── tour picks ───────────────────────────────────────────────────────────────
//
// neGlyphPicks — the cells the landing page's idle tour visits: scored by
// what makes a cell worth a look right now (a report in the last hours,
// confirmed change, land digested many times), then spread out so the tour
// sweeps the whole country instead of circling one hot spot, then ordered
// as a short nearest-neighbour path so the cursor glides rather than jumps.
// Deterministic for a given build; ~14 rows, inlined in the page (~1 KB).
func neGlyphPicks(g *neGlyph, n int) []int {
	type cand struct {
		i int
		s float64
	}
	var cs []cand
	for i := range g.Rows {
		r := &g.Rows[i]
		if r.Chunks == 0 {
			continue
		}
		s := 0.0
		if r.AgeH >= 0 && r.AgeH < neGlyphFreshH {
			s += 3 + (neGlyphFreshH-float64(r.AgeH))/neGlyphFreshH
		}
		if r.Chg > 0 {
			s += 3 + math.Min(2, float64(r.Chg)/5)
		}
		if depth := float64(r.Passes) / float64(r.Chunks); depth > 1 {
			s += math.Min(2.5, (depth-1)*0.6)
		}
		if r.Obs {
			s += 0.3
		}
		s += 0.5 * math.Min(1, float64(r.Chunks)/neGlyphChunksPer)
		if s > 0.6 {
			cs = append(cs, cand{i, s})
		}
	}
	sort.Slice(cs, func(a, b int) bool { return cs[a].s > cs[b].s || (cs[a].s == cs[b].s && cs[a].i < cs[b].i) })
	if len(cs) > 400 {
		cs = cs[:400]
	}
	// greedy: score + spread (min distance to what is already picked, in widths)
	var picked []int
	used := make([]bool, len(cs))
	usedKG := map[int32]bool{}
	for len(picked) < n && len(picked) < len(cs) {
		best, bv := -1, -1.0
		for k, c := range cs {
			if used[k] {
				continue
			}
			r := &g.Rows[c.i]
			md := 1.0
			for _, p := range picked {
				q := &g.Rows[p]
				dx, dy := float64(r.X-q.X)/neGlyphW, float64(r.Y-q.Y)/neGlyphW
				md = math.Min(md, math.Sqrt(dx*dx+dy*dy))
			}
			v := c.s + 12*md
			if usedKG[r.KG] {
				v -= 2
			}
			if v > bv {
				best, bv = k, v
			}
		}
		if best < 0 {
			break
		}
		used[best] = true
		picked = append(picked, cs[best].i)
		usedKG[g.Rows[cs[best].i].KG] = true
	}
	// nearest-neighbour path from the westernmost pick
	if len(picked) > 2 {
		start := 0
		for k, p := range picked {
			if g.Rows[p].X < g.Rows[picked[start]].X {
				start = k
			}
		}
		picked[0], picked[start] = picked[start], picked[0]
		for k := 1; k < len(picked)-1; k++ {
			a := &g.Rows[picked[k-1]]
			bi, bd := k, math.Inf(1)
			for j := k; j < len(picked); j++ {
				b := &g.Rows[picked[j]]
				d := float64(a.X-b.X)*float64(a.X-b.X) + float64(a.Y-b.Y)*float64(a.Y-b.Y)
				if d < bd {
					bi, bd = j, d
				}
			}
			picked[k], picked[bi] = picked[bi], picked[k]
		}
	}
	return picked
}

// neGlyphPicksJSON — the picks as the same compact rows the JSON body uses,
// with the KG name inlined instead of an index (the page has no kgs[] yet).
func neGlyphPicksJSON(g *neGlyph) string {
	var b bytes.Buffer
	b.WriteByte('[')
	for k, i := range g.Picks {
		r := &g.Rows[i]
		if k > 0 {
			b.WriteByte(',')
		}
		o := 0
		if r.Obs {
			o = 1
		}
		nm := ""
		if r.KG >= 0 {
			nm = g.KGs[r.KG]
		}
		nj, _ := json.Marshal(nm)
		fmt.Fprintf(&b, "[%d,%d,%d,%d,%d,%.1f,%d,%d,%s,%s", r.X, r.Y, r.Chunks, r.Passes, r.Reports, r.AgeH, o, r.Chg, nj, neGlyphBandsJSON(r.Bands))
		if r.Cand > 0 {
			fmt.Fprintf(&b, ",%d", r.Cand)
		}
		b.WriteByte(']')
	}
	b.WriteByte(']')
	return b.String()
}
