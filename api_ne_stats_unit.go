// api_ne_stats_unit.go — ONE row per spatial unit: declared · observed · diff ·
// change · subsidy · protection aggregates of umfeld-at plus the same unit's
// envelopes from {OBS_NAME} and {SUB_NAME}, in the shared
// "stats envelope" (docs/stats-envelope.md). Built for spreadsheets
// (=IMPORTDATA, Power Query), agents and the glyph's export button.
//
//	GET /api/v1/ne/stats/unit?kg=63330[,63331…≤50] | h3=<res 5–12> | bbox=w,s,e,n | adm=<gemeinde 5 | natura AT…> | pt=lon,lat
//	                         [&f=<filter expr>] [&share=<glyph hash>] [&format=json|flat|csv] [&keys=a,b] [&peers=srtm,sub|none] [&year=]
//
// The unit keys are exactly what the landing page writes into its share
// link (`#kg= h3= adm= pt= f=`): `share=` takes that hash verbatim, so the
// export of what a user sees is the same URL logic as the link they copy.
// `f=` is the glyph filter (ne_filter_expr.go): figures then cover the
// matching cells only (unit_cells_n / unit_area_ha keep the whole unit).
//
// Membership: kg = the KG's declared-view cell set, border cells weighted by
// kg_share (as srtm's /kgs/summary); adm Gemeinde = union of its KGs; Natura
// 2000 = cell centre inside the site; h3 = descendants; bbox = centre inside
// (half-open); pt = the res-7 cell under the point. Every per-cell number
// comes from the filter index bins (ne_glyph_fidx.go) + the lu/obs rows of
// the same cells (neCollect), so filter and export agree cell for cell.
//
// Bounds: ≤ 48 bins (0.025°) per unit → 413 above; ≤ 50 units per call; the
// envelope memo (neStatsLRU, 16 MB, 10 min) and the peer memo share one LRU
// (line ne_stats on /debug/inflight); peers: 5 s timeout, 30 s backoff.
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/uber/h3-go/v4"
)

const (
	neStatsSchema  = "stats-envelope/1"
	neStatsService = "umfeld-at"
	neStatsMaxBins = 48
	neStatsMaxUnit = 50
	neStatsTTL     = 10 * time.Minute
	neStatsPeerTO  = 5 * time.Second
)

var (
	neStatsLRU    = newNELRU(16 << 20)
	neStatsClient = &http.Client{Timeout: neStatsPeerTO}
	neStatsDownMu sync.Mutex
	neStatsDown   = map[string]time.Time{}
	neStatsHits   [3]int64 // computed, peer fetches, peer errors (atomic via mutex below)
	neStatsCntMu  sync.Mutex
)

type neStatsPeer struct{ id, service, base string }

func neStatsPeers() []neStatsPeer {
	p := peers()
	return []neStatsPeer{
		{"srtm", p.ObsName, p.ObsWeb + "/api/v1/stats/unit"},
		{"sub", p.SubName, p.SubAPI + "/api/v1/stats/unit"},
	}
}

// NEStatsUnitStats — size line for /debug/inflight.
func NEStatsUnitStats() map[string]interface{} {
	m := neStatsLRU.stats()
	neStatsCntMu.Lock()
	m["computed"], m["peer_fetches"], m["peer_errors"] = neStatsHits[0], neStatsHits[1], neStatsHits[2]
	neStatsCntMu.Unlock()
	return m
}

func neStatsCount(i int) {
	neStatsCntMu.Lock()
	neStatsHits[i]++
	neStatsCntMu.Unlock()
}

// ── units ────────────────────────────────────────────────────────────────────

type neStatsUnit struct {
	kind, key, name string
	bbox            [4]float64
	scan            [4]float64 // the box the bins are walked over (⊇ bbox; h3 units pad for child spill)
	kgs             []string
	total           int    // expected cells (h3 units), 0 = unknown
	peerQ           string // query the peers understand ("" = not asked)
	note            string
	member          func(id uint64, lon, lat float64) float64
}

func neStatsBBoxOK(b [4]float64) bool {
	return b[0] < b[2] && b[1] < b[3] && b[2] >= 9.4 && b[0] <= 17.3 && b[3] >= 46.3 && b[1] <= 49.1
}

func neStatsKGBox(code string) ([4]float64, bool) {
	for _, b := range kgBoxSnapshot() {
		if b.code == code {
			return [4]float64{b.minLon, b.minLat, b.maxLon, b.maxLat}, true
		}
	}
	return [4]float64{}, false
}

func neStatsKGRow(code string) *kgUniverseRow {
	kgUniverseMu.RLock()
	defer kgUniverseMu.RUnlock()
	if kgUniverseCur == nil {
		return nil
	}
	if i, ok := kgUniverseIndex[code]; ok && i < len(kgUniverseCur.KGs) {
		return &kgUniverseCur.KGs[i]
	}
	return nil
}

// neStatsKGShares — the declared view's cell set with kg_share (1/255).
func neStatsKGShares(code string) map[uint64]uint8 {
	v := neDeclViewOf(code)
	if v == nil {
		return nil
	}
	n := v.CellsN()
	m := make(map[uint64]uint8, n)
	for i := 0; i < n; i++ {
		row := v.Rows[i*neLURowBytes:]
		m[binary.LittleEndian.Uint64(row)] = row[8]
	}
	return m
}

func neStatsUnitKG(code string) (*neStatsUnit, string) {
	code = padKGCode(code)
	if !neKGRe.MatchString(code) {
		return nil, "kg must be a 5-digit code"
	}
	bb, ok := neStatsKGBox(code)
	if !ok {
		return nil, "unknown kg " + code
	}
	u := &neStatsUnit{kind: "kg", key: code, bbox: bb, kgs: []string{code}, peerQ: "kg=" + code,
		note: "cells of the KG's declared view; border cells weighted by the KG's share (as srtm /kgs/summary)"}
	if r := neStatsKGRow(code); r != nil {
		u.name = r.KGName
	}
	sh := neStatsKGShares(code)
	u.member = func(id uint64, _, _ float64) float64 { return float64(sh[id]) / 255 }
	if sh == nil {
		u.note = "KG not built yet — no declared view"
	}
	return u, ""
}

func neStatsUnitH3(s string) (*neStatsUnit, string) {
	id, res, err := neParseCell(s)
	if err != nil || res < 5 || res > 12 {
		return nil, "h3 must be a cell id of res 5–12"
	}
	bd, err := h3.Cell(id).Boundary()
	if err != nil || len(bd) == 0 {
		return nil, "bad h3"
	}
	bb := [4]float64{180, 90, -180, -90}
	for _, p := range bd {
		bb[0], bb[1], bb[2], bb[3] = math.Min(bb[0], p.Lng), math.Min(bb[1], p.Lat), math.Max(bb[2], p.Lng), math.Max(bb[3], p.Lat)
	}
	// children spill beyond the parent's outline (H3's hierarchy is approximate, ≈ 7 % of the span at res 7→12): pad the scan box
	px, py := (bb[2]-bb[0])*0.12, (bb[3]-bb[1])*0.12
	key := neCellHex(id)
	u := &neStatsUnit{kind: "h3", key: key, bbox: bb, scan: [4]float64{bb[0] - px, bb[1] - py, bb[2] + px, bb[3] + py}, peerQ: "h3=" + key, note: fmt.Sprintf("H3 res %d cell: every res-12 descendant", res)}
	u.total = 1
	for r := res; r < neCellRes; r++ {
		u.total *= 7
	}
	ures := uint(res)
	u.member = func(c uint64, _, _ float64) float64 {
		if h3Parent(c, ures) == id {
			return 1
		}
		return 0
	}
	return u, ""
}

func neStatsUnitBBox(s string) (*neStatsUnit, string) {
	p := strings.Split(s, ",")
	if len(p) != 4 {
		return nil, "bbox=w,s,e,n"
	}
	var bb [4]float64
	for k := range p {
		x, err := strconv.ParseFloat(strings.TrimSpace(p[k]), 64)
		if err != nil {
			return nil, "bbox=w,s,e,n"
		}
		bb[k] = x
	}
	if !neStatsBBoxOK(bb) {
		return nil, "bbox outside Austria or empty"
	}
	key := fmt.Sprintf("%.6g,%.6g,%.6g,%.6g", bb[0], bb[1], bb[2], bb[3])
	u := &neStatsUnit{kind: "bbox", key: key, bbox: bb, peerQ: "bbox=" + key, note: "cells whose centre lies inside the box (half-open)"}
	u.member = func(_ uint64, lon, lat float64) float64 {
		if lon >= bb[0] && lon < bb[2] && lat >= bb[1] && lat < bb[3] {
			return 1
		}
		return 0
	}
	return u, ""
}

func neStatsUnitPt(s string) (*neStatsUnit, string) {
	p := strings.Split(s, ",")
	if len(p) != 2 {
		return nil, "pt=lon,lat"
	}
	lon, e1 := strconv.ParseFloat(strings.TrimSpace(p[0]), 64)
	lat, e2 := strconv.ParseFloat(strings.TrimSpace(p[1]), 64)
	if e1 != nil || e2 != nil {
		return nil, "pt=lon,lat"
	}
	id, err := neLatLngToCell(lon, lat, 7)
	if err != nil {
		return nil, "pt outside the grid"
	}
	u, msg := neStatsUnitH3(neCellHex(id))
	if u != nil {
		u.note = "pt= → the res-7 cell under the point; " + u.note
	}
	return u, msg
}

// neStatsUnitAdm — a find-box unit: Gemeinde (5 digits, union of its KGs)
// or a Natura 2000 site (centre inside the rings). Bezirk / Land are too
// large for one row — the caller gets the hint to pass kg= lists.
func neStatsUnitAdm(code string) (*neStatsUnit, string) {
	code = strings.TrimSpace(code)
	if len(code) == 5 && neKGRe.MatchString(code) {
		kgs, _ := ResolveKGCodes(KGCodeFilter{Gemeinde: code})
		if len(kgs) == 0 {
			return nil, "unknown gemeinde " + code
		}
		sort.Strings(kgs)
		bb := [4]float64{180, 90, -180, -90}
		shares := map[uint64]int{}
		for _, kg := range kgs {
			if b, ok := neStatsKGBox(kg); ok {
				bb[0], bb[1], bb[2], bb[3] = math.Min(bb[0], b[0]), math.Min(bb[1], b[1]), math.Max(bb[2], b[2]), math.Max(bb[3], b[3])
			}
			for id, s := range neStatsKGShares(kg) {
				shares[id] += int(s)
			}
		}
		if !neStatsBBoxOK(bb) {
			return nil, "gemeinde without KG boxes"
		}
		u := &neStatsUnit{kind: "gemeinde", key: code, bbox: bb, kgs: kgs, note: fmt.Sprintf("union of the Gemeinde's %d KGs (declared-view cell sets, shares summed and capped)", len(kgs))}
		if len(kgs) <= neStatsMaxUnit {
			u.peerQ = "kg=" + strings.Join(kgs, ",")
		}
		if r := neStatsKGRow(kgs[0]); r != nil {
			u.name = r.GemeindeName
		}
		u.member = func(id uint64, _, _ float64) float64 { return math.Min(1, float64(shares[id])/255) }
		return u, ""
	}
	if err := neGlyphAdminLoad(); err != nil {
		return nil, "admin outlines unavailable"
	}
	au := neGlyphAdmin.by[code]
	if au == nil {
		return nil, "unknown adm code (Gemeinde 5 digits or Natura 2000 AT… site)"
	}
	if au.kind == nil || au.kind.Key != "n2k" {
		return nil, "adm= takes a Gemeinde or a Natura 2000 site — for a Bezirk or Land pass kg= lists (≤ 50 per call)"
	}
	rings := au.R
	u := &neStatsUnit{kind: "natura2000", key: code, name: au.N, bbox: au.BBox, note: "cells whose centre lies inside the site outline (EEA Natura 2000)"}
	u.member = func(_ uint64, lon, lat float64) float64 {
		if neStatsInRings(rings, lon, lat) {
			return 1
		}
		return 0
	}
	return u, ""
}

// neStatsInRings — even-odd point in polygon over all rings.
func neStatsInRings(rings [][][2]float64, x, y float64) bool {
	in := false
	for _, r := range rings {
		n := len(r)
		for i, j := 0, n-1; i < n; j, i = i, i+1 {
			if (r[i][1] > y) != (r[j][1] > y) && x < (r[j][0]-r[i][0])*(y-r[i][1])/(r[j][1]-r[i][1])+r[i][0] {
				in = !in
			}
		}
	}
	return in
}

// neStatsParseShare — the landing page's hash (`#kg=…;f=…;v=obs;…`) → unit
// keys + filter. Unknown tokens are view state and ignored.
func neStatsParseShare(h string) (unitKey, unitVal, f string) {
	h = strings.TrimPrefix(strings.TrimSpace(h), "#")
	for _, t := range strings.Split(h, ";") {
		k, v, ok := strings.Cut(t, "=")
		if !ok {
			continue
		}
		switch k {
		case "kg", "h3", "adm", "pt":
			if unitKey == "" {
				unitKey, unitVal = k, v
			}
		case "f":
			if d, err := url.QueryUnescape(v); err == nil {
				f = d
			} else {
				f = v
			}
		}
	}
	return
}

// neStatsUnits — resolve the request's units (≤ 50).
func neStatsUnits(q url.Values) ([]*neStatsUnit, string, int) {
	f := q.Get("f")
	var units []*neStatsUnit
	add := func(u *neStatsUnit, msg string) string {
		if msg != "" {
			return msg
		}
		units = append(units, u)
		return ""
	}
	if s := q.Get("share"); s != "" {
		k, v, sf := neStatsParseShare(s)
		if f == "" {
			f = sf
		}
		if k != "" && q.Get(k) == "" {
			q.Set(k, v)
		}
	}
	q.Set("f", f)
	for _, code := range splitCSV(q.Get("kg")) {
		if msg := add(neStatsUnitKG(code)); msg != "" {
			return nil, msg, 404
		}
	}
	for _, s := range splitCSV(q.Get("h3")) {
		if msg := add(neStatsUnitH3(s)); msg != "" {
			return nil, msg, 400
		}
	}
	for _, s := range splitCSV(q.Get("adm")) {
		if msg := add(neStatsUnitAdm(s)); msg != "" {
			return nil, msg, 404
		}
	}
	if s := q.Get("bbox"); s != "" {
		if msg := add(neStatsUnitBBox(s)); msg != "" {
			return nil, msg, 400
		}
	}
	if s := q.Get("pt"); s != "" {
		if msg := add(neStatsUnitPt(s)); msg != "" {
			return nil, msg, 400
		}
	}
	if len(units) == 0 {
		return nil, "one of kg= h3= bbox= adm= pt= share= required", 400
	}
	if len(units) > neStatsMaxUnit {
		return nil, fmt.Sprintf("≤ %d units per call", neStatsMaxUnit), 413
	}
	return units, "", 0
}

// ── the figures ──────────────────────────────────────────────────────────────

type neStatsAcc struct {
	cells, match, declared, observed, bins, partial int
	unitArea, area                                  float64
	dec, obs                                        [10]float64 // 9 groups + undeclared/unknown
	ver                                             [7]float64
	chgN                                            [3]int
	chgHa                                           [3]float64
	sub                                             [8]float64 // claimed ha by class
	subArea, bio, bioKnown, n2k                     float64
	el, sl, h, canopy, ndvi                         [2]float64 // sum, n
	bn                                              int
	bcov                                            float64
	kgHa                                            map[string]float64
	states                                          []string
}

func (a *neStatsAcc) mean(v [2]float64, dec int) interface{} {
	if v[1] == 0 {
		return nil
	}
	return neRound(v[0]/v[1], dec)
}

func neRound(v float64, dec int) float64 {
	p := math.Pow10(dec)
	return math.Round(v*p) / p
}

// neStatsCompute — walk the bins of the unit's bbox, join lu/obs rows, sum.
func neStatsCompute(u *neStatsUnit, expr *neFiltExpr) (*neStatsAcc, int) {
	acc := &neStatsAcc{kgHa: map[string]float64{}}
	if err := neGlyphLoadFrame(); err != nil {
		return acc, 503
	}
	fr := &neGlyphFrame
	bb := u.scan
	if bb == [4]float64{} {
		bb = u.bbox
	}
	i0, i1 := int(math.Floor(bb[0]/neFidxBinDeg)), int(math.Floor((bb[2]-1e-9)/neFidxBinDeg))
	j0, j1 := int(math.Floor(bb[1]/neFidxBinDeg)), int(math.Floor((bb[3]-1e-9)/neFidxBinDeg))
	if (i1-i0+1)*(j1-j0+1) > neStatsMaxBins {
		return acc, 413
	}
	filt := expr != nil && !expr.empty()
	type memb struct {
		k int
		w float64
	}
	seen := map[string]bool{}
	for i := i0; i <= i1; i++ {
		for j := j0; j <= j1; j++ {
			acc.bins++
			full, st, err := neFidxGet(i, j)
			if err != nil {
				acc.partial++
				continue
			}
			b, err := neFidxDecode(full)
			if err != nil {
				acc.partial++
				continue
			}
			if !seen[st] {
				seen[st] = true
				acc.states = append(acc.states, st)
			}
			oid, _, err := neParseCell(b.Hdr.Origin)
			if err != nil {
				acc.partial++
				continue
			}
			origin := h3.Cell(oid)
			var mask []uint8
			if filt {
				mask = b.eval(expr)
			}
			ids := make([]uint64, 0, b.N/4)
			mem := make(map[uint64]memb, b.N/4)
			for k := 0; k < b.N; k++ {
				x, y := b.pos(k)
				lon := ((x-neGlyphPad)/fr.scale + fr.minx) / fr.kx
				lat := -((y-neGlyphPad)/fr.scale + fr.miny)
				if lon < bb[0]-2e-3 || lon > bb[2]+2e-3 || lat < bb[1]-2e-3 || lat > bb[3]+2e-3 {
					continue // coarse cut from the fitted position; membership below uses the exact centre
				}
				id, ok := b.cell(origin, k)
				if !ok {
					continue
				}
				lon, lat = neCellCentre(id)
				w := u.member(id, lon, lat)
				if w <= 0 {
					continue
				}
				area, _ := h3.CellAreaM2(h3.Cell(id))
				wa := w * area / 1e4
				acc.cells++
				acc.unitArea += wa
				if mask != nil && mask[k] == 0 {
					continue
				}
				acc.match++
				acc.area += wa
				ids = append(ids, id)
				mem[id] = memb{k, wa}
				neStatsAddCols(acc, b, k, wa)
			}
			if len(ids) == 0 {
				continue
			}
			res := neCollect(nil, ids, []string{"lu", "obs"}, &neReq{public: false, format: "json", bevFirst: true})
			lay := neObsLayout(res.obsFlds, res.obsRec)
			for q, id := range res.ids {
				m := mem[id]
				wa := m.w
				if lu := res.lu[q]; len(lu) >= 21 {
					sum := 0
					for g := 0; g < 9; g++ {
						acc.dec[g] += wa * float64(lu[12+g]) / 255
						sum += int(lu[12+g])
					}
					if sum > 0 {
						acc.declared++
					}
					if sum < 255 {
						acc.dec[9] += wa * float64(255-sum) / 255
					}
				}
				if q < len(res.kgs) {
					for kg, s := range res.kgs[q] {
						acc.kgHa[kg] += wa * float64(s) / 255
					}
				}
				if q < len(res.obs) && lay != nil && lay.luOff >= 0 && len(res.obs[q]) == lay.rowBytes {
					o := res.obs[q]
					sum := 0
					for g := 0; g < 9; g++ {
						acc.obs[g] += wa * float64(o[lay.luOff+g]) / 255
						sum += int(o[lay.luOff+g])
					}
					acc.observed++
					if sum < 255 {
						acc.obs[9] += wa * float64(255-sum) / 255
					}
				}
			}
		}
	}
	return acc, 0
}

// neStatsAddCols — the fidx columns of one cell, area-weighted.
func neStatsAddCols(acc *neStatsAcc, b *neFidxBin, k int, wa float64) {
	u8 := func(id string) (int, bool) {
		c := b.Cols[id]
		if c == nil || k >= len(c) || c[k] == 255 {
			return 0, false
		}
		return int(c[k]), true
	}
	if v, ok := u8("chg"); ok && v < 3 {
		acc.chgN[v]++
		acc.chgHa[v] += wa
	}
	if v, ok := u8("ver"); ok && v < 7 {
		acc.ver[v] += wa
	}
	if cls, ok := u8("sub"); ok && cls < 8 {
		acc.subArea += wa
		if cls > 0 {
			share := 1.0
			if cov, ok := u8("scov"); ok {
				share = float64(cov) / 255
			}
			acc.sub[cls] += wa * share
			if bio, ok := u8("bio"); ok {
				acc.bioKnown += wa * share
				if bio != 0 {
					acc.bio += wa * share
				}
			}
		}
	}
	if v, ok := u8("n2k"); ok && v != 0 {
		acc.n2k += wa
	}
	if c := b.Cols["el"]; c != nil && 2*k+1 < len(c) {
		if e := binary.LittleEndian.Uint16(c[2*k:]); e != 65535 {
			acc.el[0] += float64(e) * wa
			acc.el[1] += wa
		}
	}
	if v, ok := u8("sl"); ok {
		acc.sl[0] += float64(v) * wa
		acc.sl[1] += wa
	}
	if v, ok := u8("h"); ok {
		acc.h[0] += float64(v) * 0.5 * wa
		acc.h[1] += wa
	}
	if v, ok := u8("canopy"); ok {
		acc.canopy[0] += float64(v) * 100 / 254 * wa
		acc.canopy[1] += wa
	}
	if v, ok := u8("ndvi"); ok {
		acc.ndvi[0] += (float64(v)*2/254 - 1) * wa
		acc.ndvi[1] += wa
	}
	if v, ok := u8("bn"); ok {
		acc.bn += v
	}
	if v, ok := u8("bcov"); ok {
		acc.bcov += wa * float64(v) / 255
	}
}

// neStatsFigures — the flat, dotted figure object (scalars only).
func neStatsFigures(u *neStatsUnit, acc *neStatsAcc, filt bool) map[string]interface{} {
	f := map[string]interface{}{}
	f["cells_n"] = acc.match
	f["area_ha"] = neRound(acc.area, 2)
	if filt {
		f["unit_cells_n"] = acc.cells
		f["unit_area_ha"] = neRound(acc.unitArea, 2)
		f["match_pct"] = neStatsPct(acc.area, acc.unitArea)
	}
	f["cells_declared_n"] = acc.declared
	f["cells_observed_n"] = acc.observed
	best, bestV := "", -1.0
	for g := 0; g < 9; g++ {
		f["declared_ha."+neGlyphGroups[g]] = neRound(acc.dec[g], 2)
		if acc.dec[g] > bestV {
			best, bestV = neGlyphGroups[g], acc.dec[g]
		}
	}
	f["declared_ha.undeclared"] = neRound(acc.dec[9], 2)
	if acc.declared > 0 {
		f["declared.dominant"] = best
	} else {
		f["declared.dominant"] = nil
	}
	for g := 0; g < 9; g++ {
		f["observed_ha."+neGlyphGroups[g]] = neRound(acc.obs[g], 2)
	}
	f["observed_ha.unknown"] = neRound(acc.obs[9], 2)
	var verTot float64
	for v := range neObsVerdict {
		f["diff_ha."+neObsVerdict[v]] = neRound(acc.ver[v], 2)
		verTot += acc.ver[v]
	}
	f["diff.consistent_pct"] = neStatsPct(acc.ver[0], verTot)
	for i, n := range []string{"none", "candidate", "confirmed"} {
		f["change_n."+n] = acc.chgN[i]
		f["change_ha."+n] = neRound(acc.chgHa[i], 2)
	}
	if len(u.kgs) > 0 {
		var conf, cand int
		since, vers := "", 0
		for _, kg := range u.kgs {
			if s, ok := neChangeSummaryOf(kg); ok {
				conf += s.Confirmed.Cells
				cand += s.Candidate.Cells
				vers += s.Versions
				if since == "" || (s.Since != "" && s.Since < since) {
					since = s.Since
				}
			}
		}
		f["change.log_versions"] = vers
		f["change.log_since"] = neStatsNullStr(since)
	}
	var claimed float64
	for c := 1; c < 8; c++ {
		f["subsidy_ha."+neSubClass[c]] = neRound(acc.sub[c], 2)
		claimed += acc.sub[c]
	}
	f["subsidy.claimed_ha"] = neRound(claimed, 2)
	f["subsidy.claimed_pct"] = neStatsPct(claimed, acc.area)
	f["subsidy.bio_ha"] = neRound(acc.bio, 2)
	f["subsidy.bio_pct"] = neStatsPct(acc.bio, acc.bioKnown)
	f["subsidy.known_pct"] = neStatsPct(acc.subArea, acc.area)
	f["natura2000_ha"] = neRound(acc.n2k, 2)
	f["natura2000_pct"] = neStatsPct(acc.n2k, acc.area)
	f["structures_n"] = acc.bn
	f["structures.cover_ha"] = neRound(acc.bcov, 2)
	f["terrain.elev_mean_m"] = acc.mean(acc.el, 1)
	f["terrain.slope_mean_deg"] = acc.mean(acc.sl, 1)
	f["height.p90_mean_m"] = acc.mean(acc.h, 2)
	f["canopy.cover_pct"] = acc.mean(acc.canopy, 2)
	f["ndvi.mean"] = acc.mean(acc.ndvi, 3)
	// KG composition (h3 / bbox / Natura units): dominant + n
	if len(acc.kgHa) > 0 {
		dom, domV := "", -1.0
		for kg, ha := range acc.kgHa {
			if ha > domV || (ha == domV && kg < dom) {
				dom, domV = kg, ha
			}
		}
		f["kg.dominant"] = dom
		f["kg.n"] = len(acc.kgHa)
		if r := neStatsKGRow(dom); r != nil {
			f["kg.name"] = r.KGName
			f["gemeinde.code"] = r.GemeindeCode
			f["gemeinde.name"] = r.GemeindeName
			f["bezirk.name"] = r.DistrictName
			f["bundesland.name"] = r.StateName
		}
	}
	if u.kind == "kg" {
		f["kg.name"] = u.name
	}
	if u.kind == "gemeinde" {
		f["gemeinde.code"], f["gemeinde.name"], f["gemeinde.kgs_n"] = u.key, u.name, len(u.kgs)
	}
	if u.kind == "natura2000" {
		f["natura2000.site"], f["natura2000.name"] = u.key, u.name
	}
	return f
}

func neStatsPct(a, tot float64) interface{} {
	if tot <= 0 {
		return nil
	}
	return neRound(100*a/tot, 2)
}

func neStatsNullStr(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// ── envelope ────────────────────────────────────────────────────────────────

type neStatsEnvelope struct {
	Schema      string                 `json:"schema"`
	Service     string                 `json:"service"`
	Unit        map[string]interface{} `json:"unit"`
	Source      string                 `json:"source"`
	Epoch       string                 `json:"epoch"`
	Coverage    map[string]interface{} `json:"coverage"`
	Filter      map[string]interface{} `json:"filter,omitempty"`
	Figures     map[string]interface{} `json:"figures"`
	Peers       map[string]interface{} `json:"peers,omitempty"`
	Attribution string                 `json:"attribution"`
	License     string                 `json:"license"`
	ComputedAt  string                 `json:"computed_at"`
	AgeS        int                    `json:"age_s"`
	Link        string                 `json:"link,omitempty"`
}

// neStatsOwn — our envelope for one unit, memoised 10 min under unit+filter.
func neStatsOwn(u *neStatsUnit, expr *neFiltExpr) (*neStatsEnvelope, int) {
	canon := ""
	if expr != nil {
		canon = expr.Canonical()
	}
	key := "own/" + u.kind + "/" + u.key + "/" + canon
	if b, ok := neStatsLRU.get(key); ok && len(b) > 8 {
		if ts := int64(binary.LittleEndian.Uint64(b)); time.Since(time.Unix(ts, 0)) < neStatsTTL {
			var e neStatsEnvelope
			if json.Unmarshal(b[8:], &e) == nil {
				e.AgeS = int(time.Since(time.Unix(ts, 0)).Seconds())
				return &e, 0
			}
		}
	}
	acc, code := neStatsCompute(u, expr)
	if code != 0 {
		return nil, code
	}
	neStatsCount(0)
	filt := expr != nil && !expr.empty()
	now := time.Now().UTC()
	e := &neStatsEnvelope{Schema: neStatsSchema, Service: neStatsService, License: "CC-BY-4.0", ComputedAt: now.Format(time.RFC3339),
		Attribution: neFidxAttribution + " · aggregated by " + publicHost()}
	e.Unit = map[string]interface{}{"kind": u.kind, "key": u.key, "bbox": neStatsRoundBox(u.bbox), "res": neCellRes, "cells_n": acc.match, "area_ha": neRound(acc.area, 2)}
	if u.name != "" {
		e.Unit["name"] = u.name
	}
	kgs := u.kgs
	if kgs == nil {
		for kg := range acc.kgHa {
			kgs = append(kgs, kg)
		}
		sort.Strings(kgs)
	}
	e.Unit["kgs"] = kgs
	sort.Strings(acc.states)
	e.Source = "umfeld-at@ne-fidx" + strconv.Itoa(neFidxVer)
	e.Epoch = now.Format("2006-01")
	if ep := neFidxSrtmEpoch(); ep != "" {
		e.Epoch = ep
	}
	e.Coverage = map[string]interface{}{"complete": acc.partial == 0 && acc.cells > 0, "cells_known": acc.cells, "bins": acc.bins, "bins_missing": acc.partial, "note": u.note +
		"; declared/observed ha are share-weighted from the cell rows, diff/change/subsidy/protection from the filter index (same numbers as the glyph filter)"}
	if u.total > 0 {
		e.Coverage["cells_total"] = u.total
		if acc.cells < u.total {
			e.Coverage["complete"] = false
			e.Coverage["note"] = u.note + "; some cells are not held yet (KG not built or not reported)"
		}
	}
	if acc.cells == 0 && acc.partial == 0 {
		e.Coverage["note"] = u.note + "; no cells held for this unit yet"
	}
	if filt {
		e.Filter = map[string]interface{}{"f": canon, "words": expr.Words(), "ignored": expr.Ignored, "note": "figures cover the matching cells; unit_cells_n / unit_area_ha are the whole unit; peers are not filtered"}
	}
	e.Figures = neStatsFigures(u, acc, filt)
	e.Link = neStatsLink(u, canon)
	if acc.partial == 0 {
		b, _ := json.Marshal(e)
		buf := make([]byte, 8, 8+len(b))
		binary.LittleEndian.PutUint64(buf, uint64(now.Unix()))
		neStatsLRU.put(key, append(buf, b...))
	}
	return e, 0
}

func neStatsRoundBox(b [4]float64) []float64 {
	return []float64{neRound(b[0], 6), neRound(b[1], 6), neRound(b[2], 6), neRound(b[3], 6)}
}

// neStatsLink — the landing page showing the same unit (+ filter): the share
// link and the export are two renderings of one hash.
func neStatsLink(u *neStatsUnit, canon string) string {
	var tok string
	switch u.kind {
	case "kg":
		tok = "kg=" + u.key
	case "h3":
		tok = "h3=" + u.key
	case "gemeinde", "natura2000":
		tok = "adm=" + u.key
	default:
		tok = fmt.Sprintf("pt=%.5f,%.5f;z=%d", (u.bbox[0]+u.bbox[2])/2, (u.bbox[1]+u.bbox[3])/2, neFilterZoomFor(u.bbox[2]-u.bbox[0]))
	}
	if canon != "" {
		tok += ";f=" + canon
	}
	return publicURL() + "/#" + tok
}

// ── peers ───────────────────────────────────────────────────────────────────

// neStatsPeerFetch — one peer envelope (JSON map) for a unit; memoised 10 min,
// 30 s backoff per peer after a failure. nil + reason when unavailable.
func neStatsPeerFetch(p neStatsPeer, q string, year string) (map[string]interface{}, string) {
	u := p.base + "?" + q + "&format=json"
	if year != "" && p.id == "sub" {
		u += "&year=" + url.QueryEscape(year)
	}
	key := "peer/" + u
	if b, ok := neStatsLRU.get(key); ok && len(b) > 8 {
		if ts := int64(binary.LittleEndian.Uint64(b)); time.Since(time.Unix(ts, 0)) < neStatsTTL {
			var m map[string]interface{}
			if json.Unmarshal(b[8:], &m) == nil {
				return m, ""
			}
		}
	}
	neStatsDownMu.Lock()
	down := time.Since(neStatsDown[p.id]) < 30*time.Second
	neStatsDownMu.Unlock()
	if down {
		return nil, "peer unavailable (backoff)"
	}
	neStatsCount(1)
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", "umfeld-at/stats-unit")
	resp, err := neStatsClient.Do(req)
	if err != nil {
		neStatsCount(2)
		neStatsDownMu.Lock()
		neStatsDown[p.id] = time.Now()
		neStatsDownMu.Unlock()
		return nil, "peer unreachable"
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != 200 {
		neStatsCount(2)
		if resp.StatusCode >= 500 {
			neStatsDownMu.Lock()
			neStatsDown[p.id] = time.Now()
			neStatsDownMu.Unlock()
		}
		return nil, fmt.Sprintf("peer answered %d", resp.StatusCode)
	}
	var m map[string]interface{}
	if json.Unmarshal(body, &m) != nil {
		neStatsCount(2)
		return nil, "peer answer not JSON"
	}
	buf := make([]byte, 8, 8+len(body))
	binary.LittleEndian.PutUint64(buf, uint64(time.Now().Unix()))
	neStatsLRU.put(key, append(buf, body...))
	return m, ""
}

// neStatsPeerUnits — a peer's answer may be one envelope or {units:[…]} (kg
// lists); return the envelopes by unit key.
func neStatsPeerUnits(m map[string]interface{}) []map[string]interface{} {
	if m == nil {
		return nil
	}
	if us, ok := m["units"].([]interface{}); ok {
		out := make([]map[string]interface{}, 0, len(us))
		for _, x := range us {
			if e, ok := x.(map[string]interface{}); ok {
				out = append(out, e)
			}
		}
		return out
	}
	return []map[string]interface{}{m}
}

// neStatsPeerSum — fold a peer's per-KG envelopes into one: keys that are
// sums (_ha, _n, _m2, _eur, counts) add up, _pct recomputed where both
// numerator and the unit area are additive, everything else (means, codes,
// names) is dropped. The envelope says so in coverage.note.
func neStatsPeerSum(us []map[string]interface{}, key interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for k, v := range us[0] {
		out[k] = v
	}
	fig := map[string]interface{}{}
	complete := true
	var area float64
	for _, e := range us {
		if c, ok := e["coverage"].(map[string]interface{}); ok {
			if cc, _ := c["complete"].(bool); !cc {
				complete = false
			}
		}
		fg, _ := e["figures"].(map[string]interface{})
		for k, v := range fg {
			f, ok := v.(float64)
			if !ok {
				continue
			}
			if neStatsAdditive(k) {
				if cur, ok := fig[k].(float64); ok {
					fig[k] = cur + f
				} else {
					fig[k] = f
				}
				if k == "area_ha" {
					area += f
				}
			}
		}
	}
	for k, v := range fig {
		if f, ok := v.(float64); ok {
			fig[k] = neRound(f, 2)
			if base, found := strings.CutSuffix(k, "_ha"); found && area > 0 && base != "area" {
				if _, has := fig[base+"_pct"]; !has {
					fig[base+"_pct"] = neRound(100*f/area, 2)
				}
			}
		}
	}
	fig["units_n"] = len(us)
	out["figures"] = fig
	out["unit"] = map[string]interface{}{"kind": "kg_list", "key": key, "units_n": len(us)}
	out["coverage"] = map[string]interface{}{"complete": complete, "note": fmt.Sprintf("summed over %d KG envelopes: sums (_ha, _n, _eur) only, means and codes dropped, _pct recomputed from area_ha", len(us))}
	return out
}

func neStatsAdditive(k string) bool {
	if strings.HasSuffix(k, "_pct") || strings.HasSuffix(k, "_year") || strings.HasSuffix(k, "_m") || strings.HasSuffix(k, "_deg") || strings.Contains(k, ".mean") || strings.HasPrefix(k, "top_") || strings.HasPrefix(k, "gemeinde.") || strings.HasPrefix(k, "kg.") || k == "year" || k == "prev_year" {
		return false
	}
	for _, suf := range []string{"_ha", "_n", "_m2", "_eur"} {
		if strings.HasSuffix(k, suf) || strings.Contains(k, suf+".") {
			return true
		}
	}
	return false
}

func neStatsPeerKey(e map[string]interface{}) string {
	if u, ok := e["unit"].(map[string]interface{}); ok {
		if k, ok := u["key"].(string); ok {
			return k
		}
	}
	return ""
}

// ── flat / csv ──────────────────────────────────────────────────────────────

// neStatsFlat — one flat row: unit.*, our figures, peers prefixed srtm. / sub.
func neStatsFlat(e *neStatsEnvelope, keys []string) (map[string]interface{}, []string) {
	row := map[string]interface{}{}
	var order []string
	put := func(k string, v interface{}) {
		if len(keys) > 0 {
			ok := false
			for _, p := range keys {
				if k == p || strings.HasPrefix(k, p) {
					ok = true
					break
				}
			}
			if !ok {
				return
			}
		}
		if _, dup := row[k]; !dup {
			order = append(order, k)
		}
		row[k] = v
	}
	put("unit.kind", e.Unit["kind"])
	put("unit.key", e.Unit["key"])
	if n, ok := e.Unit["name"]; ok {
		put("unit.name", n)
	}
	if bb, ok := e.Unit["bbox"].([]float64); ok && len(bb) == 4 {
		put("unit.bbox", fmt.Sprintf("%g,%g,%g,%g", bb[0], bb[1], bb[2], bb[3]))
	}
	put("coverage.complete", e.Coverage["complete"])
	if e.Filter != nil {
		put("filter", e.Filter["f"])
	}
	for _, k := range neStatsSortedKeys(e.Figures) {
		put(k, e.Figures[k])
	}
	for _, p := range neStatsPeers() {
		pe, _ := e.Peers[p.service].(map[string]interface{})
		if pe == nil {
			put(p.id+".available", false)
			continue
		}
		put(p.id+".available", true)
		if s, ok := pe["source"]; ok {
			put(p.id+".source", s)
		}
		if c, ok := pe["coverage"].(map[string]interface{}); ok {
			put(p.id+".coverage.complete", c["complete"])
		}
		if fg, ok := pe["figures"].(map[string]interface{}); ok {
			for _, k := range neStatsSortedKeys(fg) {
				put(p.id+"."+k, fg[k])
			}
		}
	}
	put("link", e.Link)
	return row, order
}

// neStatsSortedKeys — stable column order: the figure families in the order
// they are documented, alphabetical inside.
func neStatsSortedKeys(m map[string]interface{}) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	fam := func(k string) int {
		for i, p := range []string{"cells_n", "area_ha", "unit_", "match_", "cells_", "declared", "observed", "diff", "change", "subsidy", "natura", "structures", "terrain", "height", "canopy", "ndvi", "kg.", "gemeinde", "bezirk", "bundesland"} {
			if strings.HasPrefix(k, p) {
				return i
			}
		}
		return 99
	}
	sort.Slice(ks, func(a, b int) bool {
		fa, fb := fam(ks[a]), fam(ks[b])
		if fa != fb {
			return fa < fb
		}
		return ks[a] < ks[b]
	})
	return ks
}

func neStatsCSVCell(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		if strings.ContainsAny(x, ",\"\n") {
			return "\"" + strings.ReplaceAll(x, "\"", "\"\"") + "\""
		}
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		b, _ := json.Marshal(x)
		s := string(b)
		if strings.ContainsAny(s, ",\"\n") {
			return "\"" + strings.ReplaceAll(s, "\"", "\"\"") + "\""
		}
		return s
	}
}

// ── handler ─────────────────────────────────────────────────────────────────

// handleNEStatsUnit — GET /api/v1/ne/stats/unit
func handleNEStatsUnit(w http.ResponseWriter, r *http.Request, gateID string) {
	q := r.URL.Query()
	units, msg, code := neStatsUnits(q)
	if msg != "" {
		apiError(w, msg, code)
		return
	}
	format := q.Get("format")
	if format == "" {
		format = "json"
	}
	if format != "json" && format != "flat" && format != "csv" {
		apiError(w, "format=json|flat|csv", 400)
		return
	}
	var expr *neFiltExpr
	if f := q.Get("f"); f != "" {
		expr = neFiltParse(f)
	}
	filt := expr != nil && !expr.empty()
	wantPeer := map[string]bool{}
	switch ps := q.Get("peers"); {
	case ps == "none":
	case ps == "":
		if !filt { // a filtered unit has no peer equivalent — ask explicitly
			wantPeer["srtm"], wantPeer["sub"] = true, true
		}
	default:
		for _, p := range splitCSV(ps) {
			wantPeer[p] = true
		}
	}
	keys := splitCSV(q.Get("keys"))
	year := q.Get("year")

	// peers first (network), ours meanwhile
	type peerRes struct {
		peer neStatsPeer
		uid  int
		env  map[string]interface{}
		err  string
	}
	peerOut := make(chan peerRes, len(units)*len(neStatsPeers()))
	var wg sync.WaitGroup
	npeer := 0
	for ui, u := range units {
		if u.peerQ == "" {
			continue
		}
		for _, p := range neStatsPeers() {
			if !wantPeer[p.id] {
				continue
			}
			npeer++
			wg.Add(1)
			go func(ui int, p neStatsPeer, pq string) {
				defer wg.Done()
				m, err := neStatsPeerFetch(p, pq, year)
				peerOut <- peerRes{p, ui, m, err}
			}(ui, p, u.peerQ)
		}
	}
	envs := make([]*neStatsEnvelope, 0, len(units))
	partial := false
	for _, u := range units {
		e, code := neStatsOwn(u, expr)
		if code == 413 {
			apiError(w, fmt.Sprintf("unit spans more than %d bins (0.025°) — pass kg= lists, a smaller bbox or a finer h3", neStatsMaxBins), 413)
			return
		}
		if code != 0 {
			apiError(w, "frame unavailable", 503)
			return
		}
		if c, _ := e.Coverage["complete"].(bool); !c {
			partial = true
		}
		envs = append(envs, e)
	}
	wg.Wait()
	close(peerOut)
	for pr := range peerOut {
		e := envs[pr.uid]
		if e.Peers == nil {
			e.Peers = map[string]interface{}{}
		}
		if pr.env == nil {
			e.Peers[pr.peer.service] = map[string]interface{}{"available": false, "error": pr.err, "url": pr.peer.base}
			partial = true
			continue
		}
		pick := pr.env
		if us := neStatsPeerUnits(pr.env); len(us) > 1 { // kg lists (Gemeinde) → sum the additive figures
			pick = neStatsPeerSum(us, e.Unit["key"])
		}
		e.Peers[pr.peer.service] = pick
	}
	if filt && len(wantPeer) > 0 {
		for _, e := range envs {
			if e.Filter != nil {
				e.Filter["note"] = "figures cover the matching cells; unit_cells_n / unit_area_ha are the whole unit; peers were asked for the WHOLE unit (they do not filter)"
			}
		}
	}

	var body []byte
	ctype := "application/json; charset=utf-8"
	switch format {
	case "json":
		if len(envs) == 1 {
			body, _ = json.Marshal(envs[0])
		} else {
			body, _ = json.Marshal(map[string]interface{}{"schema": neStatsSchema, "service": neStatsService, "units": envs, "attribution": envs[0].Attribution, "license": "CC-BY-4.0"})
		}
	case "flat":
		rows := make([]map[string]interface{}, 0, len(envs))
		for _, e := range envs {
			row, _ := neStatsFlat(e, keys)
			rows = append(rows, row)
		}
		if len(rows) == 1 {
			body, _ = json.Marshal(rows[0])
		} else {
			body, _ = json.Marshal(rows)
		}
	case "csv":
		ctype = "text/csv; charset=utf-8"
		var cols []string
		seen := map[string]bool{}
		rows := make([]map[string]interface{}, 0, len(envs))
		for _, e := range envs {
			row, order := neStatsFlat(e, keys)
			rows = append(rows, row)
			for _, k := range order {
				if !seen[k] {
					seen[k] = true
					cols = append(cols, k)
				}
			}
		}
		var sb strings.Builder
		sb.WriteString(strings.Join(cols, ","))
		sb.WriteByte('\n')
		for _, row := range rows {
			for i, k := range cols {
				if i > 0 {
					sb.WriteByte(',')
				}
				sb.WriteString(neStatsCSVCell(row[k]))
			}
			sb.WriteByte('\n')
		}
		body = []byte(sb.String())
	}
	sum := sha256.Sum256(body)
	etag := "\"" + hex.EncodeToString(sum[:12]) + "\""
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("X-NE-Notice", NENotice())
	w.Header().Set("X-Data-Attribution", asciiFold(neFidxAttribution))
	w.Header().Set("X-Stats-Schema", neStatsSchema)
	w.Header().Set("ETag", etag)
	if partial {
		w.Header().Set("Cache-Control", "no-store")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=600")
	}
	if format == "csv" && q.Get("download") != "" {
		w.Header().Set("Content-Disposition", "attachment; filename=\"umfeld-stats-"+neStatsFileName(units)+".csv\"")
	}
	if neNotModified(w, r, etag) {
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.WriteHeader(200)
	_, _ = w.Write(body)
	exportAccount(gateID, len(body))
}

func neStatsFileName(units []*neStatsUnit) string {
	if len(units) == 1 {
		return units[0].kind + "-" + strings.NewReplacer(",", "_", ".", "p").Replace(units[0].key)
	}
	return fmt.Sprintf("%d-units", len(units))
}
