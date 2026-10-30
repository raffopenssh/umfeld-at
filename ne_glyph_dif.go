// ne_glyph_dif.go — the DIFF overlay of the landing glyph: declared (the view)
// vs observed (srtm's NEO1 row) of the SAME cell, today. Not change (change is
// declared over time, ne_change.go); its time axis is the lidar epoch.
//
// Painting rule = docs/ne-change.md §6d (B1), the three subsets that proved
// clean, each corroborated by the observed row itself; everything else —
// 38 % of cells disagree on vocabulary alone — is not drawn. The noise filters
// are on by default; raw=1 drops the corroboration test (kinds 2 and 3 then
// paint every cell of their kind), never the status filter.
//
//	1 declared_built_absent   declared structures ≥ 20 %, observed cover < 5 %      "structures declared · none in the LiDAR"
//	2 built_undeclared ∧ rise observed cover ≥ 20 %, declared < 5 %, nDSM ≥ +2 m    "structures in the LiDAR · none declared"
//	3 forest_declared_open ∧ cut  declared forest, observed open, loss year or nDSM ≤ −2 m,
//	                          and no QA'd tree apex in the cell                      "declared forest · currently no trees"
//
// Kind 3 is not a finding against the register: a cut or a young stand is
// forest in the register by design. It says what stands there today.
//
// Status filter (always): land ≥ 90 %, kg_share ≥ ½, not superseded, declared
// use known, observed verdict known and a height; the dominant test (≥ ½ of
// the cell each side) applies to kind 3 only, as in the script.
//
// This is a threshold rule on two frozen rows, read for the picture — not an
// algorithm (ne_cells/ stays the only one; invariant 5).
package main

import (
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"io"
	"math"
	"net/http"
	"sync"
	"time"
)

// one hue; the mark is the kind (solid · chequer · ring), the legend names it
var neGlyphDifRGB = [3]uint8{0xa8, 0x2c, 0x8f}

const (
	neDifKindNone      = 0
	neDifKindDeclared  = 1  // declared structures the lidar does not see
	neDifKindAppeared  = 2  // structures the lidar sees, undeclared, risen between the flights
	neDifKindCut       = 3  // declared forest, observed open, a cut the lidar or Hansen saw
	neDifBldHi         = 51 // 20 % of 255
	neDifBldLo         = 13 // 5 % of 255
	neDifDHRise        = 8  // +2 m in ¼-m units
	neDifDHDrop        = -8
	neDifVerdictUnkn   = 6
	neDifPeerTTL       = time.Hour // revalidated by ETag — a 304 is a few hundred bytes
	neDifPeerRetry     = 10 * time.Minute
	neDifPeerMaxBody   = 8 << 20
	neDifMixedShareMin = 0.9 // below: the KG's newest flight year is not one year (§6d: 934 KGs)
)

var neDifKindName = [4]string{"", "declared_built_absent", "built_undeclared", "forest_declared_open"}

func neGlyphDifPalette() color.Palette {
	c := neGlyphDifRGB
	return color.Palette{color.NRGBA{0, 0, 0, 0},
		color.NRGBA{c[0], c[1], c[2], 200}, // kind 1 solid
		color.NRGBA{c[0], c[1], c[2], 255}, // kind 2 chequer (dense dots read lighter)
		color.NRGBA{c[0], c[1], c[2], 235}, // kind 3 ring
	}
}

var neDifPeerURL = peers().ObsAPI + "/api/v1/kgs?fields=codes,als&processed_only=0" // var: tests point it at a stub

// ── the lidar epoch per KG (srtm's als{} block, one bulk GET, 24 h) ─────────

type neDifAls struct {
	Newest, Oldest, Dominant int
	Share                    float64 // dominant_share of the newest flight year
	Precision                string
}

func (a neDifAls) mixed() bool { return a.Newest > 0 && a.Share > 0 && a.Share < neDifMixedShareMin }

var neDifEpoch struct {
	mu       sync.Mutex
	m        map[string]neDifAls
	at, last time.Time // at: data time (changes only with new data); last: last attempt
	fresh    time.Time // last 200 or 304 — the TTL clock
	busy     bool
	err      string
	bytes    int
	etag     string // srtm's ETag; sent back as If-None-Match, a 304 costs nothing
	n304     int
}

// neDifAlsOf — the KG's epoch, if known. Never blocks a tile: a missing or
// stale table is refreshed in the background (one fetch at a time), the
// first tiles after a restart simply carry no year.
func neDifAlsOf(kg string) (neDifAls, bool) {
	e := &neDifEpoch
	e.mu.Lock()
	a, ok := e.m[kg]
	stale := time.Since(e.fresh) > neDifPeerTTL
	retry := time.Since(e.last) > neDifPeerRetry
	if stale && retry && !e.busy {
		e.busy = true
		e.last = time.Now()
		go neDifFetch()
	}
	e.mu.Unlock()
	return a, ok
}

func neDifFetch() {
	e := &neDifEpoch
	e.mu.Lock()
	etag := e.etag
	if e.m == nil {
		etag = ""
	}
	e.mu.Unlock()
	m, n, tag, err := neDifFetchOnce(etag)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.busy = false
	if err != nil {
		e.err = err.Error()
		return
	}
	e.err, e.fresh = "", time.Now()
	if m == nil { // 304: the table we hold is current
		e.n304++
		return
	}
	e.m, e.at, e.bytes, e.etag = m, time.Now(), n, tag
}

// neDifFetchOnce — GET with If-None-Match; (nil, 0, "", nil) on 304.
func neDifFetchOnce(etag string) (map[string]neDifAls, int, string, error) {
	cl := &http.Client{Timeout: 60 * time.Second}
	req, _ := http.NewRequest("GET", neDifPeerURL, nil)
	req.Header.Set("User-Agent", "umfeld-at glyph diff (lidar epoch per KG)")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, 0, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return nil, 0, "", nil
	}
	if resp.StatusCode != 200 {
		return nil, 0, "", fmt.Errorf("srtm kgs: %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, neDifPeerMaxBody))
	if err != nil {
		return nil, 0, "", err
	}
	var doc struct {
		KGs []struct {
			Code string `json:"kg_code"`
			Als  *struct {
				Newest, Oldest, Dominant int
				DominantShare            float64 `json:"dominant_share"`
				Precision                string
			} `json:"als"`
		} `json:"kgs"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, 0, "", err
	}
	m := make(map[string]neDifAls, len(doc.KGs))
	for _, k := range doc.KGs {
		if k.Als == nil || len(k.Code) != 5 {
			continue
		}
		m[k.Code] = neDifAls{Newest: k.Als.Newest, Oldest: k.Als.Oldest, Dominant: k.Als.Dominant, Share: k.Als.DominantShare, Precision: k.Als.Precision}
	}
	if len(m) == 0 {
		return nil, 0, "", fmt.Errorf("srtm kgs: no als block")
	}
	return m, len(b), resp.Header.Get("ETag"), nil
}

// neDifEpochStamp — the epoch table's data time (0 = not loaded yet), part of
// every v=dif tile key, so tiles rendered before the table arrived are not
// served without their year once it has. Triggers the fetch like neDifAlsOf.
func neDifEpochStamp() int64 {
	neDifAlsOf("")
	e := &neDifEpoch
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.at.IsZero() {
		return 0
	}
	return e.at.Unix()
}

// NEGlyphDifStats — the /debug/inflight line (invariant 10): one bounded table.
func NEGlyphDifStats() map[string]interface{} {
	e := &neDifEpoch
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[string]interface{}{"als_kgs": len(e.m), "als_bytes": e.bytes, "busy": e.busy, "bound": "7850 KGs × 1 struct, one conditional GET per hour", "etag": e.etag, "not_modified": e.n304}
	if !e.at.IsZero() {
		out["als_age_s"] = int(time.Since(e.at).Seconds())
	}
	if e.err != "" {
		out["err"] = e.err
	}
	return out
}

// ── the rule ────────────────────────────────────────────────────────────────

// neDifStatus — why a cell is not compared ("" = ok).
func neDifStatus(lu, obs []byte, lay *neObsLayoutT) string {
	switch {
	case lu[11]&neFlagSuperseded != 0:
		return "superseded"
	case lay.verdict >= 0 && obs[lay.verdict] == neDifVerdictUnkn, lay.hMean >= 0 && obs[lay.hMean] == 255:
		return "obs_null"
	case lu[11]&1 != 0:
		return "unknown_declared"
	case lu[9] < 230 || lu[8] < 128:
		return "edge"
	}
	return ""
}

func neDifDom(lu []byte) (dom int, top float64) {
	sum, best := 0, -1
	for q := 0; q < 9; q++ {
		sum += int(lu[q])
		if best < 0 || lu[q] > lu[best] {
			best = q
		}
	}
	if sum == 0 {
		return -1, 0
	}
	return best, float64(lu[best]) / float64(sum)
}

// neDifKind — the kind drawn for one cell (status already ok). raw drops the
// corroboration of kinds 2 and 3.
func neDifKind(lu, obs []byte, lay *neObsLayoutT, raw bool) int {
	dg, og := lu[12], obs[lay.luOff] // geb = group 0
	if og >= neDifBldHi && dg < neDifBldLo {
		if raw || (lay.dh >= 0 && int8(obs[lay.dh]) != -128 && int8(obs[lay.dh]) >= neDifDHRise) {
			return neDifKindAppeared
		}
		return neDifKindNone
	}
	if dg >= neDifBldHi && og < neDifBldLo {
		return neDifKindDeclared
	}
	dd, dt := neDifDom(lu[12:21])
	od, ot := neDifDom(obs[lay.luOff : lay.luOff+9])
	if dd == 4 && dt >= 0.5 && ot >= 0.5 && (od == 3 || od == 2 || od == 7 || od == 1 || od == 8) {
		if _, n, ok := neObsTrees(obs, lay); ok && n > 0 {
			return neDifKindNone // trees stand there (srtm's QA'd apices): not "no trees"
		}
		loss := lay.hansen >= 0 && obs[lay.hansen] != 0 && obs[lay.hansen] != 255
		drop := lay.dh >= 0 && int8(obs[lay.dh]) != -128 && int8(obs[lay.dh]) <= neDifDHDrop
		if raw || loss || drop {
			return neDifKindCut
		}
	}
	return neDifKindNone
}

// ── the tile ────────────────────────────────────────────────────────────────

type neGlyphDifMeta struct {
	raw      bool
	cmp      int    // cells compared (status ok, obs row present)
	kinds    [4]int // [1..3] painted
	alsMin   int    // flight years across the tile's KGs: earliest dominant year … latest newest year
	alsMax   int
	mixed    int // KGs whose newest year is mixed
	alsKnown bool
}

func (m *neGlyphDifMeta) String() string {
	s := fmt.Sprintf(";dif=%d,%d,%d;cmp=%d", m.kinds[1], m.kinds[2], m.kinds[3], m.cmp)
	if m.raw {
		s += ";raw=1"
	}
	if m.alsKnown {
		s += fmt.Sprintf(";als=%d,%d;mix=%d", m.alsMin, m.alsMax, m.mixed)
	}
	return s
}

func (m *neGlyphDifMeta) epoch(kgs []string) {
	for _, kg := range kgs {
		a, ok := neDifAlsOf(kg)
		if !ok || a.Newest == 0 {
			continue
		}
		y := a.Dominant
		if y == 0 {
			y = a.Newest
		}
		if !m.alsKnown || y < m.alsMin {
			m.alsMin = y
		}
		if !m.alsKnown || a.Newest > m.alsMax {
			m.alsMax = a.Newest
		}
		m.alsKnown = true
		if a.mixed() {
			m.mixed++
		}
	}
}

// neGlyphDifRender — v=dif: the marks over a transparent ground (the profile
// fill stays the ground, dimmed by CSS as under change).
func neGlyphDifRender(g *neGlyphTileGeom, ids []uint64, kgs []string, raw bool, m *neGlyphTileMeta) *image.Paletted {
	res := neCollect(nil, ids, []string{"lu", "obs"}, &neReq{public: false, format: "bin", bevFirst: true})
	m.n = len(res.ids)
	m.dif.raw = raw
	m.dif.epoch(kgs)
	img := image.NewPaletted(image.Rect(0, 0, g.pw, g.ph), neGlyphDifPalette())
	lay := neObsLayout(res.obsFlds, res.obsRec)
	if lay == nil {
		return img
	}
	var poly [8][2]float64
	for k, id := range res.ids {
		row, lu := res.obs[k], res.lu[k]
		if row == nil || len(row) != lay.rowBytes || len(lu) < 30 {
			continue
		}
		m.obs++
		if neDifStatus(lu, row, lay) != "" {
			continue
		}
		m.dif.cmp++
		kind := neDifKind(lu, row, lay, raw)
		if kind == neDifKindNone {
			continue
		}
		np, cx, cy, ok := g.poly(id, &poly)
		if !ok {
			continue
		}
		m.dif.kinds[kind]++
		switch kind {
		case neDifKindDeclared:
			neFillPoly(img, poly[:np], 1)
		case neDifKindAppeared:
			neFillPolyMask(img, poly[:np], 2, 2)
		case neDifKindCut:
			// a ring: the cell's hexagon with its inner half cleared — "declared, nothing standing"
			neFillPoly(img, poly[:np], 3)
			var in [8][2]float64
			neFillPoly(img, neSubInner(poly[:np], np, cx, cy, 0.5, &in), 0)
		}
	}
	return img
}

// ── the cell card ───────────────────────────────────────────────────────────

// neGlyphPicDif — what the diff overlay says about one cell, in words: status,
// the kind drawn (filtered and raw), lidar's dominant group vs the declared
// one, the corroboration figures and the KG's lidar epoch. nil without an
// observed row.
func neGlyphPicDif(id uint64, kgs map[string]uint8) map[string]interface{} {
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
	lu, _, _ := neStore.declaredRow(id, true)
	if len(lu) < 30 {
		return nil
	}
	o := map[string]interface{}{}
	dd, dt := neDifDom(lu[12:21])
	od, ot := neDifDom(row[lay.luOff : lay.luOff+9])
	if dd >= 0 {
		o["declared"] = map[string]interface{}{"dom": neGlyphGroups[dd], "dom_pct": int(math.Round(dt*20)) * 5}
	}
	if od >= 0 {
		o["observed"] = map[string]interface{}{"dom": neGlyphGroups[od], "dom_pct": int(math.Round(ot*20)) * 5}
	}
	if st := neDifStatus(lu, row, lay); st != "" {
		o["status"] = st
	} else {
		o["status"] = "ok"
		o["agree"] = dd >= 0 && dd == od
		if kd := neDifKind(lu, row, lay, false); kd != neDifKindNone {
			o["kind"] = neDifKindName[kd]
			o["kind_n"] = kd
		}
		if kd := neDifKind(lu, row, lay, true); kd != neDifKindNone {
			o["kind_raw"] = neDifKindName[kd]
		}
	}
	if lay.dh >= 0 && int8(row[lay.dh]) != -128 {
		o["dh_m"] = float64(int8(row[lay.dh])) * 0.25
	}
	if lay.hansen >= 0 && row[lay.hansen] != 0 && row[lay.hansen] != 255 {
		o["loss_year"] = 2000 + int(row[lay.hansen])
	}
	// the epoch of the KG holding most of the cell (§6d: mixed-epoch KGs are flagged, not resolved per chunk here)
	best, bs := oh[0].KG, uint8(0)
	for kg, sh := range kgs {
		if sh > bs {
			best, bs = kg, sh
		}
	}
	if a, ok := neDifAlsOf(best); ok && a.Newest > 0 {
		o["als"] = map[string]interface{}{"kg": best, "newest": a.Newest, "oldest": a.Oldest, "dominant_share": a.Share, "mixed": a.mixed(), "precision": a.Precision}
	}
	return o
}
