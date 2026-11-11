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
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
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
	neStatsMaxKGs  = 300 // KGs behind one unit (a Bezirk, a combined selection)
	// a unit beyond neStatsMaxBins answers from the national sample at once (approx)
	// and — up to this many bins, unfiltered — is computed cell-exact in the
	// background (one worker, ≤ neStatsExactQueue waiting); the next call has it
	neStatsExactBins  = 640
	neStatsExactQueue = 8
	neStatsExactTTL   = 2 * time.Hour
	neStatsTTL        = 10 * time.Minute
	neStatsPeerTO     = 5 * time.Second
)

var (
	neStatsLRU    = newNELRU(16 << 20)
	neStatsClient = &http.Client{Timeout: neStatsPeerTO}
	// cell lists: upload of up to ~3 MB + the peer's aggregation; 202 polling budget
	neStatsCellsClient = &http.Client{Timeout: 20 * time.Second}
	neStatsCellsPollTO = 25 * time.Second
	neStatsDownMu      sync.Mutex
	neStatsDown        = map[string]time.Time{}
	neStatsHits        [3]int64 // computed, peer fetches, peer errors (atomic via mutex below)
	neStatsCntMu       sync.Mutex
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
	neStatsExactMu.Lock()
	m["exact_waiting"], m["exact_done"], m["exact_refused"] = len(neStatsExactWait), neStatsExactN[0], neStatsExactN[1]
	neStatsExactMu.Unlock()
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
	rings           [][][2]float64 // outline units (Natura 2000): membership by cell centre
	boxes           [][4]float64   // combined units: the members' boxes — the bins walked are their union, not the hull
	noExact         bool           // a view-clipped unit (the page's in-view legend): never queued for the background pass
}

// neStatsKGSet — the declared views of a list of KGs as ONE membership: a
// cell's weight is the sum of its shares over the listed KGs, capped at 1
// (a border cell between two listed KGs counts whole, once). Views are
// id-sorted row blobs out of the chunk LRU — looked up by binary search, no
// per-request maps. Every KG-based unit (kg, Gemeinde, Bezirk, combined) goes
// through here, so a KG, the Gemeinde holding it and their combination agree
// cell for cell.
type neStatsKGSet struct {
	rows  [][]byte
	boxes [][4]float64
	built int
}

func neStatsKGSetOf(kgs []string) *neStatsKGSet {
	s := &neStatsKGSet{}
	for _, kg := range kgs {
		b, ok := neStatsKGBox(kg)
		if !ok {
			continue
		}
		v := neDeclViewOf(kg)
		if v == nil || v.CellsN() == 0 {
			continue
		}
		s.built++
		s.rows = append(s.rows, v.Rows[:v.CellsN()*neLURowBytes])
		s.boxes = append(s.boxes, b)
	}
	return s
}

// a cell's centre lies within a cell radius (~10 m) of its KG's box; the pad is generous
const neStatsKGPad = 2e-3

func (s *neStatsKGSet) share(id uint64, lon, lat float64) float64 {
	sum := 0
	for i, rows := range s.rows {
		if b := s.boxes[i]; len(s.rows) > 1 && (lon < b[0]-neStatsKGPad || lon > b[2]+neStatsKGPad || lat < b[1]-neStatsKGPad || lat > b[3]+neStatsKGPad) {
			continue
		}
		lo, hi := 0, len(rows)/neLURowBytes
		for lo < hi {
			m := (lo + hi) / 2
			if neRowID(rows, m, neLURowBytes) < id {
				lo = m + 1
			} else {
				hi = m
			}
		}
		if lo < len(rows)/neLURowBytes && neRowID(rows, lo, neLURowBytes) == id {
			sum += int(rows[lo*neLURowBytes+8])
		}
	}
	if sum >= 255 {
		return 1
	}
	return float64(sum) / 255
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
	sh := neStatsKGSetOf(u.kgs)
	u.member = sh.share
	if sh.built == 0 {
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
	if n := len(code); (n == 5 || n == 3 || n == 1) && isNumericCode(code) {
		kind, flt := "gemeinde", KGCodeFilter{Gemeinde: code}
		switch n {
		case 3:
			kind, flt = "bezirk", KGCodeFilter{District: code}
		case 1:
			kind, flt = "land", KGCodeFilter{State: code}
		}
		kgs, _ := ResolveKGCodes(flt)
		if len(kgs) == 0 {
			return nil, "unknown " + kind + " " + code
		}
		if len(kgs) > neStatsMaxKGs {
			return nil, fmt.Sprintf("%s %s holds %d KGs — a unit takes up to %d (pass its Bezirke or Gemeinden one by one)", kind, code, len(kgs), neStatsMaxKGs)
		}
		sort.Strings(kgs)
		bb := [4]float64{180, 90, -180, -90}
		for _, kg := range kgs {
			if b, ok := neStatsKGBox(kg); ok {
				bb[0], bb[1], bb[2], bb[3] = math.Min(bb[0], b[0]), math.Min(bb[1], b[1]), math.Max(bb[2], b[2]), math.Max(bb[3], b[3])
			}
		}
		if !neStatsBBoxOK(bb) {
			return nil, kind + " without KG boxes"
		}
		u := &neStatsUnit{kind: kind, key: code, bbox: bb, kgs: kgs, note: fmt.Sprintf("union of the %d KGs of the %s (declared-view cell sets, shares summed and capped)", len(kgs), kind)}
		if len(kgs) <= neStatsMaxUnit {
			u.peerQ = "kg=" + strings.Join(kgs, ",")
		}
		if r := neStatsKGRow(kgs[0]); r != nil {
			switch kind {
			case "gemeinde":
				u.name = r.GemeindeName
			case "bezirk":
				u.name = r.DistrictName
			default:
				u.name = r.StateName
			}
		}
		u.member = neStatsKGSetOf(kgs).share
		return u, ""
	}
	if err := neGlyphAdminLoad(); err != nil {
		return nil, "admin outlines unavailable"
	}
	au := neGlyphAdmin.by[code]
	if au == nil {
		return nil, "unknown adm code (Gemeinde 5 digits, Bezirk 3, Land 1, or a Natura 2000 AT… site)"
	}
	if au.kind == nil || au.kind.Key != "n2k" {
		return nil, "adm= takes a Gemeinde, Bezirk, Land or a Natura 2000 site"
	}
	rings := au.R
	u := &neStatsUnit{kind: "natura2000", key: code, name: au.N, bbox: au.BBox, rings: rings, note: "cells whose centre lies inside the site outline (EEA Natura 2000)"}
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
			// f=off:<expr> — the page's filter switched off but kept for the funnel: no filter here
			if len(f) >= 4 && strings.EqualFold(f[:4], "off:") {
				f = ""
			}
		}
	}
	return
}

// neStatsShareVal — one key of a share hash ("" when absent).
func neStatsShareVal(h, key string) string {
	h = strings.TrimPrefix(strings.TrimSpace(h), "#")
	for _, t := range strings.Split(h, ";") {
		if k, v, ok := strings.Cut(t, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// neStatsUnitSet — several KG-based / outline units as ONE: the union of
// their cells. A cell is counted once whatever the overlap — a KG inside a
// listed Gemeinde, a site partly or wholly inside a listed KG, two sites
// across each other: weight = max(summed-and-capped share over the distinct
// KGs behind all KG-based members, 1 when its centre lies in any site).
func neStatsUnitSet(ms []*neStatsUnit) (*neStatsUnit, string) {
	var kgTok, admTok, names, kgs []string
	seenU, seenKG := map[string]bool{}, map[string]bool{}
	var sites []*neStatsUnit
	bb := [4]float64{180, 90, -180, -90}
	var boxes [][4]float64
	for _, m := range ms {
		if m.kgs == nil && m.rings == nil {
			return nil, "combine= takes kg= and adm= units (a " + m.kind + " does not combine)"
		}
		if seenU[m.kind+"/"+m.key] {
			continue
		}
		seenU[m.kind+"/"+m.key] = true
		if m.kind == "kg" {
			kgTok = append(kgTok, m.key)
		} else {
			admTok = append(admTok, m.key)
		}
		if m.name != "" {
			names = append(names, m.name)
		} else {
			names = append(names, m.key)
		}
		for _, kg := range m.kgs {
			if !seenKG[kg] {
				seenKG[kg] = true
				kgs = append(kgs, kg)
			}
		}
		if m.rings != nil {
			sites = append(sites, m)
		}
		b := m.bbox
		boxes = append(boxes, b)
		bb[0], bb[1], bb[2], bb[3] = math.Min(bb[0], b[0]), math.Min(bb[1], b[1]), math.Max(bb[2], b[2]), math.Max(bb[3], b[3])
	}
	if len(kgs) > neStatsMaxKGs {
		return nil, fmt.Sprintf("the combination holds %d KGs — up to %d", len(kgs), neStatsMaxKGs)
	}
	sort.Strings(kgTok)
	sort.Strings(admTok)
	sort.Strings(kgs)
	var key []string
	if len(kgTok) > 0 {
		key = append(key, "kg="+strings.Join(kgTok, ","))
	}
	if len(admTok) > 0 {
		key = append(key, "adm="+strings.Join(admTok, ","))
	}
	n := len(kgTok) + len(admTok)
	name := strings.Join(names, " + ")
	if len(names) > 3 {
		name = fmt.Sprintf("%s + %s + %d more", names[0], names[1], len(names)-2)
	}
	u := &neStatsUnit{kind: "combined", key: strings.Join(key, ";"), name: name, bbox: bb, boxes: boxes,
		note: fmt.Sprintf("union of %d units — every cell once, whatever the overlap (KG cells by their summed share, capped; site cells by their centre)", n)}
	if len(sites) == 0 {
		u.kgs = kgs
		if len(kgs) <= neStatsMaxUnit {
			u.peerQ = "kg=" + strings.Join(kgs, ",")
		}
	}
	ks := neStatsKGSetOf(kgs)
	u.member = func(id uint64, lon, lat float64) float64 {
		w := 0.0
		if len(ks.rows) > 0 {
			if w = ks.share(id, lon, lat); w >= 1 {
				return 1
			}
		}
		for _, m := range sites {
			if b := m.bbox; lon < b[0] || lon > b[2] || lat < b[1] || lat > b[3] {
				continue
			}
			if neStatsInRings(m.rings, lon, lat) {
				return 1
			}
		}
		return w
	}
	return u, ""
}

// neStatsUnits — resolve the request's units (≤ 50). A combined selection
// (share=#kg=a,b;adm=c — or combine=1 on kg=/adm= lists) is ONE unit, the
// union; combine=0 keeps a share's members apart (one row each, overlaps
// counted in each).
func neStatsUnits(q url.Values) ([]*neStatsUnit, string, int) {
	f := q.Get("f")
	combine := q.Get("combine") == "1"
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
			// a combined selection (`#kg=a,b;adm=c,d`): both lists are units
			if k == "kg" && q.Get("adm") == "" {
				if a := neStatsShareVal(s, "adm"); a != "" {
					q.Set("adm", a)
				}
			}
			// the page's selection is one area: several members = their union
			if (k == "kg" || k == "adm") && q.Get("combine") == "" {
				combine = true
			}
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
	if combine && len(units) > 1 {
		u, msg := neStatsUnitSet(units)
		if msg != "" {
			return nil, msg, 400
		}
		return []*neStatsUnit{u}, "", 0
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
	ids                                             []uint64 // the matching cells (filtered units: the peers get this list)
	scale                                           int      // 1, or neSampK when ids are sampled cells
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
	return neStatsComputeMax(u, expr, neStatsMaxBins)
}

func neStatsComputeMax(u *neStatsUnit, expr *neFiltExpr, maxBins int) (*neStatsAcc, int) {
	acc := &neStatsAcc{kgHa: map[string]float64{}}
	if err := neGlyphLoadFrame(); err != nil {
		return acc, 503
	}
	fr := &neGlyphFrame
	bb := u.scan
	if bb == [4]float64{} {
		bb = u.bbox
	}
	// the bins to walk: the unit's box — or, for a combined unit, the union of
	// its members' boxes (two KGs far apart stay cell-exact)
	boxes := u.boxes
	if len(boxes) == 0 {
		boxes = [][4]float64{bb}
	}
	var bins [][2]int
	binSeen := map[[2]int]bool{}
	for _, b := range boxes {
		i0, i1 := int(math.Floor(b[0]/neFidxBinDeg)), int(math.Floor((b[2]-1e-9)/neFidxBinDeg))
		j0, j1 := int(math.Floor(b[1]/neFidxBinDeg)), int(math.Floor((b[3]-1e-9)/neFidxBinDeg))
		if (i1-i0+1)*(j1-j0+1) > maxBins {
			return acc, 413
		}
		for i := i0; i <= i1; i++ {
			for j := j0; j <= j1; j++ {
				if !binSeen[[2]int{i, j}] {
					binSeen[[2]int{i, j}] = true
					bins = append(bins, [2]int{i, j})
				}
			}
		}
		if len(bins) > maxBins {
			return acc, 413
		}
	}
	filt := expr != nil && !expr.empty()
	type memb struct {
		k int
		w float64
	}
	seen := map[string]bool{}
	{
		for _, ij := range bins {
			i, j := ij[0], ij[1]
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
				if filt {
					acc.ids = append(acc.ids, id)
				}
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

// neStatsSample — the unit over the national 1-in-49 sample: every sampled
// cell inside the unit stands for 49 (docs/ne-filter.md §8). Same columns as
// the bins (asBin), declared/observed from the best group + share only.
func neStatsSample(sp *neSamp, u *neStatsUnit, expr *neFiltExpr) (*neStatsAcc, int) {
	acc := &neStatsAcc{kgHa: map[string]float64{}}
	bb := u.scan
	if bb == [4]float64{} {
		bb = u.bbox
	}
	filt := expr != nil && !expr.empty()
	var mask []uint8
	if filt {
		mask = sp.eval(expr)
	}
	bin := sp.asBin()
	d, dsh, o, osh := sp.col("d"), sp.col("dsh"), sp.col("o"), sp.col("osh")
	acc.states = []string{sp.Hdr.State}
	for _, k := range sp.rowsNear(bb[0], bb[1], bb[2], bb[3]) {
		id := sp.id(k)
		lon, lat := neCellCentre(id)
		w := u.member(id, lon, lat)
		if w <= 0 {
			continue
		}
		area, _ := h3.CellAreaM2(h3.Cell(id))
		wa := w * area * neSampK / 1e4
		acc.cells += neSampK
		acc.unitArea += wa
		if mask != nil && mask[k] == 0 {
			continue
		}
		acc.match += neSampK
		acc.area += wa
		if filt {
			acc.ids = append(acc.ids, id)
			acc.scale = neSampK
		}
		neStatsAddCols(acc, bin, k, wa)
		if d != nil && d[k] != 255 && int(d[k]) < 9 {
			acc.declared += neSampK
			sh := 1.0
			if dsh != nil && dsh[k] != 255 {
				sh = float64(dsh[k]) / 254
			}
			acc.dec[d[k]] += wa * sh
			acc.dec[9] += wa * (1 - sh)
		}
		if o != nil && o[k] != 255 && int(o[k]) < 9 {
			acc.observed += neSampK
			sh := 1.0
			if osh != nil && osh[k] != 255 {
				sh = float64(osh[k]) / 254
			}
			acc.obs[o[k]] += wa * sh
			acc.obs[9] += wa * (1 - sh)
		}
	}
	return acc, 0 // (kgHa stays empty: the sample carries no KG shares; the unit's own list stands)
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
// With a filter the matching cell list (the peers' unit, neStatsCellsBody)
// comes back too, memoised beside the envelope.
func neStatsOwn(u *neStatsUnit, expr *neFiltExpr) (*neStatsEnvelope, int, *neStatsCells) {
	canon := ""
	if expr != nil {
		canon = expr.Canonical()
	}
	key := "own/" + u.kind + "/" + u.key + "/" + canon
	filt := expr != nil && !expr.empty()
	// a large unit computed cell-exact in the background (below): kept longer,
	// served stale while it is recomputed
	if b, ok := neStatsLRU.get("x/" + key); ok && len(b) > 8 && !filt {
		ts := time.Unix(int64(binary.LittleEndian.Uint64(b)), 0)
		var e neStatsEnvelope
		if json.Unmarshal(b[8:], &e) == nil {
			e.AgeS = int(time.Since(ts).Seconds())
			if time.Since(ts) > neStatsExactTTL {
				neStatsExactAsk(key, u)
			}
			return &e, 0, nil
		}
	}
	if b, ok := neStatsLRU.get(key); ok && len(b) > 8 {
		if ts := int64(binary.LittleEndian.Uint64(b)); time.Since(time.Unix(ts, 0)) < neStatsTTL {
			var e neStatsEnvelope
			if json.Unmarshal(b[8:], &e) == nil {
				e.AgeS = int(time.Since(time.Unix(ts, 0)).Seconds())
				if !filt {
					return &e, 0, nil
				}
				if cb, ok := neStatsLRU.get("ids/" + key); ok && len(cb) > 5 {
					return &e, 0, &neStatsCells{body: cb[5:], scale: int(cb[0]), n: int(binary.LittleEndian.Uint32(cb[1:]))}
				}
			}
		}
	}
	acc, code := neStatsCompute(u, expr)
	approx, pending := false, false
	if code == 413 {
		if !filt && !u.noExact {
			pending = neStatsExactAsk(key, u)
		}
		// too large for the bins: the national 1-in-49 sample answers, ×49 (docs/ne-filter.md §8)
		if sp := neSampGet(); sp != nil {
			acc, code = neStatsSample(sp, u, expr)
			approx = code == 0
		}
	}
	if code != 0 {
		return nil, code, nil
	}
	neStatsCount(0)
	e := neStatsEnvelopeOf(u, expr, acc, approx)
	if pending {
		e.Coverage["exact"] = "pending"
		e.Coverage["note"] = e.Coverage["note"].(string) + "; the cell-exact figures are being computed — ask again in a minute"
	}
	now := time.Now().UTC()
	var cells *neStatsCells
	if filt {
		cells = &neStatsCells{body: neStatsCellsBody(acc.ids), scale: max(acc.scale, 1), n: len(acc.ids)}
	}
	if acc.partial == 0 && !pending {
		b, _ := json.Marshal(e)
		buf := make([]byte, 8, 8+len(b))
		binary.LittleEndian.PutUint64(buf, uint64(now.Unix()))
		neStatsLRU.put(key, append(buf, b...))
		if cells != nil {
			hdr := binary.LittleEndian.AppendUint32([]byte{byte(cells.scale)}, uint32(cells.n))
			neStatsLRU.put("ids/"+key, append(hdr, cells.body...))
		}
	}
	return e, 0, cells
}

// the background pass for large units: one worker, a bounded queue
var (
	neStatsExactMu   sync.Mutex
	neStatsExactWait = map[string]bool{}
	neStatsExactCh   chan func()
	neStatsExactN    [2]int64 // done, refused (queue full)
)

// neStatsExactAsk — queue the cell-exact pass of a unit beyond neStatsMaxBins.
// true = it is queued or running (the answer will come); false = not possible
// (too large even for the pass, queue full).
func neStatsExactAsk(key string, u *neStatsUnit) bool {
	neStatsExactMu.Lock()
	defer neStatsExactMu.Unlock()
	if neStatsExactWait[key] {
		return true
	}
	if neStatsExactCh == nil {
		neStatsExactCh = make(chan func(), neStatsExactQueue)
		go func() {
			for f := range neStatsExactCh {
				f()
			}
		}()
	}
	// a queued job must not pin the unit's declared views: it rebuilds the unit from its token
	tok := neStatsUnitToken(u)
	if tok != "" && tok != "h3="+u.key {
		u = nil
	}
	job := func() {
		u := u
		if u == nil {
			us, _, _ := neStatsUnits(url.Values{"share": {tok}})
			if len(us) != 1 {
				return
			}
			u = us[0]
		}
		defer func() {
			_ = recover()
			neStatsExactMu.Lock()
			delete(neStatsExactWait, key)
			neStatsExactMu.Unlock()
		}()
		acc, code := neStatsComputeMax(u, nil, neStatsExactBins)
		if code != 0 || acc.partial > 0 {
			if code == 413 { // never fits: remember, so it is not asked again and again
				neStatsLRU.put("xno/"+key, []byte{1})
			}
			return
		}
		e := neStatsEnvelopeOf(u, nil, acc, false)
		b, _ := json.Marshal(e)
		buf := make([]byte, 8, 8+len(b))
		binary.LittleEndian.PutUint64(buf, uint64(time.Now().Unix()))
		neStatsLRU.put("x/"+key, append(buf, b...))
		neStatsExactMu.Lock()
		neStatsExactN[0]++
		neStatsExactMu.Unlock()
	}
	if _, no := neStatsLRU.get("xno/" + key); no {
		return false
	}
	select {
	case neStatsExactCh <- job:
		neStatsExactWait[key] = true
		return true
	default:
		neStatsExactN[1]++
		return false
	}
}

// neStatsEnvelopeOf — the envelope of one computed unit.
func neStatsEnvelopeOf(u *neStatsUnit, expr *neFiltExpr, acc *neStatsAcc, approx bool) *neStatsEnvelope {
	canon := ""
	if expr != nil {
		canon = expr.Canonical()
	}
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
	if approx {
		e.Coverage["approx"] = true
		e.Coverage["sample"] = neSampK
		e.Coverage["note"] = u.note + fmt.Sprintf("; estimated from the national 1-in-%d sample (one res-12 cell per res-10 cell, counts ×%d): the unit is too large for the cell-exact index; declared/observed ha use the best group of each sampled cell; k>= does not apply", neSampK, neSampK)
	}
	if filt {
		e.Filter = map[string]interface{}{"f": canon, "words": expr.Words(), "ignored": expr.Ignored, "incomplete": expr.Incomplete, "note": "figures cover the matching cells; unit_cells_n / unit_area_ha are the whole unit; peers are asked for the same matching cells (cell list)"}
	}
	e.Figures = neStatsFigures(u, acc, filt)
	e.Link = neStatsLink(u, canon)
	return e
}

// neStatsCells — a filtered unit as the peers see it: the matching res-12
// cells, gzip'd `NEI1` (docs/stats-envelope.md §6), each standing for `scale` cells.
type neStatsCells struct {
	body  []byte
	scale int
	n     int
}

// neStatsCellsBody — "NEI1" · u32 n · n × u64 LE H3 res-12 ids, sorted, gzip'd.
func neStatsCellsBody(ids []uint64) []byte {
	ids = append([]uint64(nil), ids...)
	sortU64(ids)
	ids = slices.Compact(ids) // the sample's rowsNear may yield a cell twice at box seams
	raw := make([]byte, 0, 8+8*len(ids))
	raw = append(raw, "NEI1"...)
	raw = binary.LittleEndian.AppendUint32(raw, uint32(len(ids)))
	for _, id := range ids {
		raw = binary.LittleEndian.AppendUint64(raw, id)
	}
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	zw.Write(raw)
	zw.Close()
	return buf.Bytes()
}

// neStatsPeerFetchCells — the peer's envelope over an explicit cell list:
// POST {peer}/api/v1/stats/unit?format=json[&scale=49][&year=] with the
// NEI1 body. A peer that does not take cell lists yet answers 404/405/415 →
// a plain "not available" with the reason, never a whole-unit figure
// passed off as filtered.
func neStatsPeerFetchCells(p neStatsPeer, c *neStatsCells, year string) (map[string]interface{}, string) {
	if c == nil || c.n == 0 {
		return nil, "no matching cells"
	}
	u := p.base + "?format=json&unit=cells"
	if c.scale > 1 {
		u += "&scale=" + strconv.Itoa(c.scale)
	}
	if year != "" && p.id == "sub" {
		u += "&year=" + url.QueryEscape(year)
	}
	sum := sha256.Sum256(c.body)
	key := "peer/" + u + "#" + hex.EncodeToString(sum[:8])
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
	// Large lists: the observed-layer peer answers 202 {"status":"computing",
	// "retry_after_s":N} + Retry-After while it aggregates (≈ 15 s for 10⁵
	// ids); poll within neStatsCellsPollTO, then report "still computing".
	// A 202 that carries unit.key lets the polls be GET ?unit=cells&key=
	// (no re-upload); a peer without that form answers 400/404/405 → re-POST.
	var resp *http.Response
	var body []byte
	start := time.Now()
	pollKey := ""
	for {
		var req *http.Request
		if pollKey != "" {
			req, _ = http.NewRequest("GET", u+"&key="+url.QueryEscape(pollKey), nil)
		} else {
			req, _ = http.NewRequest("POST", u, bytes.NewReader(c.body))
			req.Header.Set("Content-Type", "application/octet-stream")
			req.Header.Set("Content-Encoding", "gzip")
			req.ContentLength = int64(len(c.body))
		}
		req.Header.Set("User-Agent", "umfeld-at/stats-unit")
		var err error
		resp, err = neStatsCellsClient.Do(req)
		if err != nil {
			neStatsCount(2)
			neStatsDownMu.Lock()
			neStatsDown[p.id] = time.Now()
			neStatsDownMu.Unlock()
			return nil, "peer unreachable"
		}
		body, _ = io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if pollKey != "" && (resp.StatusCode == 400 || resp.StatusCode == 404 || resp.StatusCode == 405) {
			pollKey = "" // the peer has no key= polling form: re-POST instead
			continue
		}
		if resp.StatusCode != 202 {
			break
		}
		var st struct {
			Key  string `json:"key"`
			Unit struct {
				Key string `json:"key"`
			} `json:"unit"`
		}
		if json.Unmarshal(body, &st) == nil {
			if st.Unit.Key != "" {
				pollKey = st.Unit.Key
			} else if st.Key != "" {
				pollKey = st.Key
			}
		}
		wait := 3 * time.Second
		if ra, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && ra > 0 {
			wait = time.Duration(min(ra, 10)) * time.Second
		}
		if time.Since(start)+wait > neStatsCellsPollTO {
			return nil, fmt.Sprintf("peer still computing this cell list (202) — retry in %ds", int(wait/time.Second))
		}
		time.Sleep(wait)
	}
	switch resp.StatusCode {
	case 200:
	case 404, 405, 415, 501:
		return nil, fmt.Sprintf("peer does not take cell lists yet (POST NEI1 → %d); its figures would cover the whole unit, so none are shown", resp.StatusCode)
	default:
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

func neStatsRoundBox(b [4]float64) []float64 {
	return []float64{neRound(b[0], 6), neRound(b[1], 6), neRound(b[2], 6), neRound(b[3], 6)}
}

// neStatsLink — the landing page showing the same unit (+ filter): the share
// link and the export are two renderings of one hash.
func neStatsLink(u *neStatsUnit, canon string) string {
	tok := neStatsUnitToken(u)
	if tok == "" {
		tok = fmt.Sprintf("pt=%.5f,%.5f;z=%d", (u.bbox[0]+u.bbox[2])/2, (u.bbox[1]+u.bbox[3])/2, neFilterZoomFor(u.bbox[2]-u.bbox[0]))
	}
	if canon != "" {
		// < > | percent-encoded: mail clients stop linkifying at them; the page decodes f=
		tok += ";f=" + strings.NewReplacer("<", "%3C", ">", "%3E", "|", "%7C", " ", "%20").Replace(canon)
	}
	return publicURL() + "/#" + tok
}

// neStatsUnitToken — the share-hash token that names the unit ("" for a box / point).
func neStatsUnitToken(u *neStatsUnit) string {
	var tok string
	switch u.kind {
	case "kg":
		tok = "kg=" + u.key
	case "h3":
		tok = "h3=" + u.key
	case "gemeinde", "bezirk", "land", "natura2000":
		tok = "adm=" + u.key
	case "combined":
		tok = u.key
	}
	return tok
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
		if av, _ := pe["available"].(bool); pe == nil || (!av && pe["error"] != nil) {
			put(p.id+".available", false)
			if pe != nil {
				put(p.id+".error", pe["error"])
			}
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
	q := neQuery(r)
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
		wantPeer["srtm"], wantPeer["sub"] = true, true
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
	askPeers := func(ui int, u *neStatsUnit, cells *neStatsCells) {
		for _, p := range neStatsPeers() {
			if !wantPeer[p.id] {
				continue
			}
			npeer++
			wg.Add(1)
			go func(ui int, p neStatsPeer, pq string) {
				defer wg.Done()
				var m map[string]interface{}
				var err string
				if filt { // a filtered unit = its matching cells: the peers get the list, never the whole unit
					m, err = neStatsPeerFetchCells(p, cells, year)
				} else {
					m, err = neStatsPeerFetch(p, pq, year)
				}
				peerOut <- peerRes{p, ui, m, err}
			}(ui, p, u.peerQ)
		}
	}
	if !filt {
		for ui, u := range units {
			if u.peerQ != "" {
				askPeers(ui, u, nil)
			}
		}
	}
	envs := make([]*neStatsEnvelope, 0, len(units))
	partial := false
	for ui, u := range units {
		e, code, cells := neStatsOwn(u, expr)
		if code == 0 && filt && cells != nil {
			askPeers(ui, u, cells)
		}
		if code == 413 {
			apiError(w, fmt.Sprintf("unit spans more than %d bins (0.025°) — pass kg= lists, a smaller bbox or a finer h3", neStatsMaxBins), 413)
			return
		}
		if code != 0 {
			apiError(w, "frame unavailable", 503)
			return
		}
		if c, _ := e.Coverage["complete"].(bool); !c || e.Coverage["exact"] == "pending" {
			partial = true // not to be cached: incomplete, or the cell-exact figures are on their way
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
	{ // a telling name whether downloaded (csv&download=1) or saved from the tab (ne_export_name.go)
		canon, disp, ext := "", "inline", format
		if filt {
			canon = expr.Canonical()
		}
		if format == "csv" && q.Get("download") != "" {
			disp = "attachment"
		}
		if format == "flat" {
			ext = "flat.json"
		}
		w.Header().Set("Content-Disposition", disp+"; filename=\""+neExportStem("stats", neExportUnitPlace(units), "", canon, "")+"."+ext+"\"")
	}
	if neNotModified(w, r, etag) {
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.WriteHeader(200)
	_, _ = w.Write(body)
	exportAccount(gateID, len(body))
}

// handleNEGlyphArea — GET /api/v1/ne/glyph/area?u=kg=a,b;adm=c — the card's
// area line for a selection: the SAME unit, cells and area the export
// (/ne/stats/unit?share=) answers with, taken from the same memo — the page
// and the sheet cannot disagree. Not an export: no rows, not gated as one.
func handleNEGlyphArea(w http.ResponseWriter, r *http.Request) {
	u := r.URL.Query().Get("u")
	if len(u) > 4096 {
		apiError(w, "u= too long", 400)
		return
	}
	units, msg, code := neStatsUnits(url.Values{"share": {u}})
	if msg != "" {
		apiError(w, msg, code)
		return
	}
	// clip=w,s,e,n — the part of the unit inside a box (the page's "in view"
	// with a selection): same cells, same weights, centre inside the box
	if cs := r.URL.Query().Get("clip"); cs != "" {
		cu, msg := neStatsUnitBBox(cs)
		if cu == nil {
			apiError(w, msg, 400)
			return
		}
		units[0] = neStatsClip(units[0], cu.bbox)
		if units[0] == nil {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Cache-Control", "public, max-age=600")
			writeJSON(w, 200, map[string]interface{}{"pending": false, "area_ha": 0, "cells_n": 0, "approx": false, "complete": true, "figures": map[string]interface{}{"cells_n": 0}})
			return
		}
	}
	e, code, _ := neStatsOwn(units[0], nil)
	if code != 0 || e == nil {
		w.Header().Set("Retry-After", "30")
		apiError(w, "not available", 503)
		return
	}
	approx, _ := e.Coverage["approx"].(bool)
	complete, _ := e.Coverage["complete"].(bool)
	pending := e.Coverage["exact"] == "pending"
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "public, max-age=600")
	if pending {
		w.Header().Set("Cache-Control", "no-store")
	}
	writeJSON(w, 200, map[string]interface{}{"pending": pending, "kind": units[0].kind, "key": units[0].key, "area_ha": e.Unit["area_ha"], "cells_n": e.Unit["cells_n"],
		"approx": approx, "complete": complete, "figures": e.Figures})
}

// neStatsClip — the unit restricted to a box (cell centre inside, half-open);
// nil when they do not meet. Its key carries the box, so the memo keeps it
// apart from the whole unit.
func neStatsClip(u *neStatsUnit, c [4]float64) *neStatsUnit {
	cut := func(b [4]float64) ([4]float64, bool) {
		o := [4]float64{math.Max(b[0], c[0]), math.Max(b[1], c[1]), math.Min(b[2], c[2]), math.Min(b[3], c[3])}
		return o, o[0] < o[2] && o[1] < o[3]
	}
	bb, ok := cut(u.bbox)
	if !ok {
		return nil
	}
	v := *u
	v.bbox, v.scan, v.boxes, v.noExact, v.peerQ, v.total = bb, [4]float64{}, nil, true, "", 0
	if u.scan != [4]float64{} {
		if sc, ok := cut(u.scan); ok {
			v.scan = sc
		}
	}
	for _, b := range u.boxes {
		if o, ok := cut(b); ok {
			v.boxes = append(v.boxes, o)
		}
	}
	if len(u.boxes) > 0 && len(v.boxes) == 0 {
		return nil
	}
	v.key = fmt.Sprintf("%s|clip=%.4f,%.4f,%.4f,%.4f", u.key, c[0], c[1], c[2], c[3])
	v.note = u.note + "; clipped to a box"
	inner := u.member
	v.member = func(id uint64, lon, lat float64) float64 {
		if lon < c[0] || lon >= c[2] || lat < c[1] || lat >= c[3] {
			return 0
		}
		return inner(id, lon, lat)
	}
	return &v
}
