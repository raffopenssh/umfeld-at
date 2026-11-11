package main

// ne_where.go — WHERE as an endpoint: one call names a spot (or a whole
// glyph share link) to an agent the way the ?dbg panel names it to a person.
// Public, tokenless, read-only, no SQL. Built so the observed-layer peer can
// debug itself through us: every number we hold for a cell, where it came
// from (obs row · fidx bin · the peer's own cells block), the filter verdict
// term by term, and the peer's own URLs for the same spot.
//
//	GET /api/v1/ne/where?pt=lon,lat | lon=&lat= | cell=<res-12 H3> | share=<glyph hash or full URL>
//	                     [&f=<expr>] [&format=txt|json] [&n=24] [&ar=<viewport h/w>] [&fields=all]
//
// share= is the landing page's hash (`#v=obs;full;f=…;at=x,y,z`, also a
// whole URL, also percent-encoded): the point is pt= > share pt= > at= centre
// > h3= > cell=x,y > the KG's centre; the view (zoom, LOD, bbox, tile keys)
// comes from at=/z=; f= (also `off:` held) is evaluated on the cell term by
// term and over the view (clipped to 0.1°) as a ranked list of matching
// cells with the terms' values. format=txt is the panel's text, line for line.

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/uber/h3-go/v4"
)

const (
	neWhereHiLOD0 = 110 // = HI_LOD0 / HI_LOD1 / ZMAX0 in ne_glyph.go
	neWhereHiLOD1 = 300
	neWhereZMAX0  = 24
	neWhereHiMin  = 8
	neWhereMaxN   = 200
)

// neWhereView — what a share link says.
type neWhereView struct {
	Hash    string            `json:"hash"`
	Profile string            `json:"profile"`
	Flags   []string          `json:"flags"`
	F       string            `json:"f,omitempty"`
	FHeld   bool              `json:"f_held,omitempty"`
	Unit    map[string]string `json:"unit,omitempty"`
	pt      *[2]float64
	tree    *[2]float64 // ;tree=lon,lat — the picked tree
	at      *[3]float64
	z       float64
	h3      string
	cellXY  *[2]float64
	kg      string
	dropped []string
}

// neWhereHash — the hash part of a share (full URL, `#`, `%23`, percent-encoded).
func neWhereHash(s string) string {
	s = strings.TrimSpace(s)
	if k := strings.Index(s, "#"); k >= 0 {
		s = s[k+1:]
	} else if k := strings.Index(s, "%23"); k >= 0 {
		s = s[k+3:]
	}
	if strings.Contains(s, "%3B") || strings.Contains(s, "%3b") || (!strings.Contains(s, ";") && !strings.Contains(s, "=")) {
		if d, err := url.QueryUnescape(s); err == nil {
			s = d
		}
	}
	s = strings.TrimPrefix(strings.TrimPrefix(s, "!"), "/")
	if !strings.Contains(s, ";") && strings.Contains(s, "&") {
		s = strings.ReplaceAll(s, "&", ";")
	}
	return s
}

func neWhereParseShare(raw string) neWhereView {
	v := neWhereView{Hash: neWhereHash(raw), Flags: []string{}}
	for _, t := range strings.Split(v.Hash, ";") {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		k, val, has := strings.Cut(t, "=")
		k = strings.ToLower(k)
		nums := func(n int) ([]float64, bool) {
			p := strings.Split(val, ",")
			if len(p) != n {
				return nil, false
			}
			out := make([]float64, n)
			for i := range p {
				x, err := strconv.ParseFloat(strings.TrimSpace(p[i]), 64)
				if err != nil {
					return nil, false
				}
				out[i] = x
			}
			return out, true
		}
		switch {
		case !has:
			switch k {
			case "full", "nofull", "trees", "relief", "change", "diff", "subsidy", "ortho", "roads", "screen", "open", "below", "export", "info":
				v.Flags = append(v.Flags, k)
			default:
				v.dropped = append(v.dropped, t)
			}
		case k == "v":
			if strings.EqualFold(val, "obs") {
				v.Profile = "obs"
			} else if strings.EqualFold(val, "chg") {
				v.Flags = append(v.Flags, "change")
			}
		case k == "diff" || k == "ortho" || k == "roads":
			v.Flags = append(v.Flags, k+"="+val)
		case k == "f":
			f := val
			if d, err := url.QueryUnescape(val); err == nil {
				f = d
			}
			if len(f) >= 4 && strings.EqualFold(f[:4], "off:") {
				v.FHeld, f = true, f[4:]
			}
			v.F = f
		case k == "pt":
			if a, ok := nums(2); ok {
				if a[0] > 40 && a[1] < 20 { // lat,lon the wrong way round
					a[0], a[1] = a[1], a[0]
				}
				v.pt = &[2]float64{a[0], a[1]}
				v.unit("pt", val)
			}
		case k == "tree":
			if a, ok := nums(2); ok {
				if a[0] > 40 && a[1] < 20 {
					a[0], a[1] = a[1], a[0]
				}
				v.tree = &[2]float64{a[0], a[1]}
				v.unit("tree", val)
			}
		case k == "at":
			if a, ok := nums(3); ok {
				v.at = &[3]float64{a[0], a[1], math.Min(a[2], 900)}
			}
		case k == "z":
			if a, ok := nums(1); ok {
				v.z = math.Min(a[0], 900)
			}
		case k == "h3":
			v.h3 = strings.ToLower(val)
			v.unit("h3", v.h3)
		case k == "cell":
			if a, ok := nums(2); ok {
				v.cellXY = &[2]float64{a[0], a[1]}
			}
		case k == "kg":
			v.kg = padKGCode(val)
			v.unit("kg", v.kg)
		case k == "adm", k == "q", k == "pn", k == "t", k == "card":
			v.unit(k, val)
		default:
			v.dropped = append(v.dropped, t)
		}
	}
	if v.Profile == "" {
		v.Profile = "declared"
	}
	return v
}

func (v *neWhereView) unit(k, val string) {
	if v.Unit == nil {
		v.Unit = map[string]string{}
	}
	v.Unit[k] = val
}

func (v *neWhereView) has(flag string) bool {
	for _, f := range v.Flags {
		if f == flag || strings.HasPrefix(f, flag+"=") {
			return true
		}
	}
	return false
}

// neGlyphUnproj — viewBox → lon/lat (the page's lonlat()).
func neGlyphUnproj(x, y float64) (lon, lat float64) {
	fr := &neGlyphFrame
	return ((x-neGlyphPad)/fr.scale + fr.minx) / fr.kx, -((y-neGlyphPad)/fr.scale + fr.miny)
}

func neWhereLOD(z float64) int {
	if z >= neWhereHiLOD1 {
		return 1
	}
	if z >= neWhereHiLOD0 {
		return 0
	}
	return 2
}

var neWhereLODDeg = [3]float64{0.025, 0.01, 0.1}

func neWhereTileKey(lod int, lon, lat float64) (int, int) {
	L := neWhereLODDeg[lod]
	return int(math.Floor(lon / L)), int(math.Floor(lat / L))
}

// neWhereFieldVal — the physical value of one catalogue field for row k of a
// bin: number (scaled), member name, member list (bits), bool (flag), class
// label (bucket), site code; nil = no value held.
func neWhereFieldVal(fd *neFiltField, b *neFidxBin, k int) (interface{}, int, bool) {
	c := b.Cols[fd.colID()]
	if c == nil || k < 0 || (fd.Dtype == "u16" && 2*k+1 >= len(c)) || (fd.Dtype != "u16" && k >= len(c)) {
		return nil, 0, false
	}
	raw := neFiltRaw(fd, c, k)
	if raw == fd.Nodata {
		return nil, raw, true
	}
	switch fd.Kind {
	case nfFlag:
		return raw != 0, raw, true
	case nfSet:
		if raw >= 0 && raw < len(fd.Members) {
			return fd.Members[raw], raw, true
		}
		return raw, raw, true
	case nfBits:
		var out []string
		for i, m := range fd.Members {
			if raw&(1<<uint(i)) != 0 {
				out = append(out, m)
			}
		}
		return out, raw, true
	case nfSites:
		if raw >= 0 && raw < len(b.Hdr.N2K) {
			return b.Hdr.N2K[raw], raw, true
		}
		return raw, raw, true
	case nfBucket:
		if raw >= 0 && raw < len(fd.Edges) {
			if raw+1 < len(fd.Edges) {
				return fmt.Sprintf("%d..%d", fd.Edges[raw], fd.Edges[raw+1]), raw, true
			}
			return fmt.Sprintf("%d..", fd.Edges[raw]), raw, true
		}
		return raw, raw, true
	}
	if fd.Halo && raw&128 != 0 {
		return fmt.Sprintf(">=%d (halo)", raw&127), raw, true
	}
	if (fd.ID == "loss" || fd.ID == "snar" || fd.ID == "snar2") && raw == 0 {
		return nil, raw, true
	}
	sc := fd.Scale
	if sc == 0 {
		sc = 1
	}
	return math.Round((float64(raw)*sc+fd.Off)*1000) / 1000, raw, true
}

// neWhereBinRow — the fidx row of a cell: bin + ordinal (cgo once, lattice lookup).
func neWhereBinRow(id uint64) (*neFidxBin, int, string, string) {
	lon, lat := neCellCentre(id)
	i, j := neWhereTileKey(0, lon, lat)
	full, st, err := neFidxGet(i, j)
	if err != nil {
		return nil, -1, st, "bin unavailable: " + err.Error()
	}
	b, err := neFidxDecode(full)
	if err != nil {
		return nil, -1, st, "bin undecodable: " + err.Error()
	}
	oid, _, err := neParseCell(b.Hdr.Origin)
	if err != nil {
		return b, -1, st, "bin has no origin"
	}
	origin := h3.Cell(oid)
	oij, e1 := h3.CellToLocalIJ(origin, origin)
	cij, e2 := h3.CellToLocalIJ(origin, h3.Cell(id))
	if e1 != nil || e2 != nil {
		return b, -1, st, "cell outside the bin's IJ frame"
	}
	k, ok := b.lattice()[ijKey(int32(cij.I-oij.I), int32(cij.J-oij.J))]
	if !ok {
		return b, -1, st, "cell not in the bin (no declared row at index build)"
	}
	return b, int(k), st, ""
}

// neWhereVals — field → {v, raw, src} for row k (every field, or the named ones).
func neWhereVals(b *neFidxBin, k int, only map[string]bool) map[string]interface{} {
	out := map[string]interface{}{}
	for i := range neFiltFields {
		fd := &neFiltFields[i]
		if only != nil && !only[fd.ID] {
			continue
		}
		v, raw, held := neWhereFieldVal(fd, b, k)
		if !held {
			continue
		}
		out[fd.ID] = map[string]interface{}{"v": v, "raw": raw, "unit": fd.Unit, "src": fd.Source}
	}
	if b.TH != nil && len(b.TH.Off) == b.N+1 && (only == nil || only["ht"]) {
		a, e := b.TH.Off[k], b.TH.Off[k+1]
		l := make([]float64, 0, e-a)
		for _, h := range b.TH.H[a:e] {
			l = append(l, float64(h)*0.5)
		}
		out["ht_list"] = map[string]interface{}{"v": l, "unit": "m", "src": "srtm tree_h apex list (ht matches any apex in range)"}
	}
	return out
}

// neWhereTerms — the filter verdict of row k, term by term.
func neWhereTerms(b *neFidxBin, k int, e *neFiltExpr) ([]map[string]interface{}, bool) {
	var out []map[string]interface{}
	for gi, g := range e.Groups {
		for _, t := range g {
			r := map[string]interface{}{"term": t.String(), "group": gi, "field": t.F}
			if fd := neFiltByID[t.F]; fd != nil && b != nil && k >= 0 {
				v, raw, held := neWhereFieldVal(fd, b, k)
				r["value"], r["raw"], r["held"] = v, raw, held
				r["pass"] = neFiltTermFn(b, t)(k)
			} else {
				r["pass"] = false
			}
			out = append(out, r)
		}
	}
	match := false
	if b != nil && k >= 0 {
		match = b.eval(e)[k] != 0
	}
	return out, match
}

// neWhereMatches — matching cells of a box (clipped to 0.1° about the point),
// nearest first, with the terms' values.
func neWhereMatches(bw, bs, be, bn, plon, plat float64, e *neFiltExpr, n int) map[string]interface{} {
	clipped := false
	if be-bw > 0.1 {
		bw, be, clipped = plon-0.05, plon+0.05, true
	}
	if bn-bs > 0.1 {
		bs, bn, clipped = plat-0.05, plat+0.05, true
	}
	fr := &neGlyphFrame
	only := map[string]bool{"d": true, "o": true}
	for _, g := range e.Groups {
		for _, t := range g {
			if fd := neFiltByID[t.F]; fd != nil {
				only[fd.ID] = true
				if fd.Col != "" {
					only[fd.Col] = true
				}
			}
		}
	}
	type hit struct {
		id   uint64
		b    *neFidxBin
		k    int
		dist float64
	}
	var hits []hit
	cells, match, partial, bins := 0, 0, 0, 0
	i0, i1 := int(math.Floor(bw/neFidxBinDeg)), int(math.Floor((be-1e-9)/neFidxBinDeg))
	j0, j1 := int(math.Floor(bs/neFidxBinDeg)), int(math.Floor((bn-1e-9)/neFidxBinDeg))
	kx := math.Cos(plat * math.Pi / 180)
	for i := i0; i <= i1; i++ {
		for j := j0; j <= j1; j++ {
			bins++
			full, _, err := neFidxGet(i, j)
			if err != nil {
				partial++
				continue
			}
			b, err := neFidxDecode(full)
			if err != nil {
				partial++
				continue
			}
			m := b.eval(e)
			oid, _, err := neParseCell(b.Hdr.Origin)
			if err != nil {
				continue
			}
			origin := h3.Cell(oid)
			for k := 0; k < b.N; k++ {
				x, y := b.pos(k)
				lon := ((x-neGlyphPad)/fr.scale + fr.minx) / fr.kx
				lat := -((y-neGlyphPad)/fr.scale + fr.miny)
				if lon < bw-2e-3 || lon >= be+2e-3 || lat < bs-2e-3 || lat >= bn+2e-3 {
					continue
				}
				id, ok := b.cell(origin, k)
				if !ok {
					continue
				}
				lon, lat = neCellCentre(id)
				if lon < bw || lon >= be || lat < bs || lat >= bn {
					continue
				}
				cells++
				if m[k] == 0 {
					continue
				}
				match++
				dx, dy := (lon-plon)*kx*111320, (lat-plat)*111320
				hits = append(hits, hit{id, b, k, math.Sqrt(dx*dx + dy*dy)})
			}
		}
	}
	sort.Slice(hits, func(a, c int) bool { return hits[a].dist < hits[c].dist })
	if len(hits) > n {
		hits = hits[:n]
	}
	list := make([]map[string]interface{}, 0, len(hits))
	for _, h := range hits {
		lon, lat := neCellCentre(h.id)
		vals := map[string]interface{}{}
		for f, v := range neWhereVals(h.b, h.k, only) {
			vals[f] = v.(map[string]interface{})["v"]
		}
		list = append(list, map[string]interface{}{"cell": neCellHex(h.id), "lon": math.Round(lon*1e6) / 1e6, "lat": math.Round(lat*1e6) / 1e6, "dist_m": math.Round(h.dist), "vals": vals})
	}
	return map[string]interface{}{"bbox": []float64{neRound(bw, 5), neRound(bs, 5), neRound(be, 5), neRound(bn, 5)}, "clipped": clipped, "bins": bins, "partial_bins": partial, "cells": cells, "match": match, "listed": len(list), "nearest": list}
}

// handleNEWhere — GET /api/v1/ne/where
func handleNEWhere(w http.ResponseWriter, r *http.Request) {
	if err := neGlyphLoadFrame(); err != nil {
		apiError(w, "glyph frame unavailable", 503)
		return
	}
	q := r.URL.Query()
	fr := &neGlyphFrame
	var sv neWhereView
	hasShare := q.Get("share") != ""
	if hasShare {
		sv = neWhereParseShare(q.Get("share"))
	}
	// ── the point ──
	var lon, lat float64
	var id uint64
	src := ""
	parsePt := func(s string) (float64, float64, bool) {
		p := strings.Split(s, ",")
		if len(p) != 2 {
			return 0, 0, false
		}
		a, e1 := strconv.ParseFloat(strings.TrimSpace(p[0]), 64)
		b, e2 := strconv.ParseFloat(strings.TrimSpace(p[1]), 64)
		if e1 != nil || e2 != nil {
			return 0, 0, false
		}
		if a > 40 && b < 20 {
			a, b = b, a
		}
		return a, b, true
	}
	switch {
	case q.Get("cell") != "":
		v, res, err := neParseCell(q.Get("cell"))
		if err != nil || res != neCellRes {
			apiError(w, "cell must be a res-12 H3 id", 400)
			return
		}
		id = v
		lon, lat = neCellCentre(id)
		src = "cell="
	case q.Get("pt") != "":
		var ok bool
		if lon, lat, ok = parsePt(q.Get("pt")); !ok {
			apiError(w, "pt=lon,lat", 400)
			return
		}
		src = "pt="
	case q.Get("lon") != "" || q.Get("lat") != "":
		a, e1 := strconv.ParseFloat(q.Get("lon"), 64)
		b, e2 := strconv.ParseFloat(q.Get("lat"), 64)
		if e1 != nil || e2 != nil {
			apiError(w, "lon=&lat=", 400)
			return
		}
		lon, lat, src = a, b, "lon=&lat="
	case hasShare && sv.tree != nil:
		lon, lat, src = sv.tree[0], sv.tree[1], "share tree="
	case hasShare && sv.pt != nil:
		lon, lat, src = sv.pt[0], sv.pt[1], "share pt="
	case hasShare && sv.at != nil:
		lon, lat = neGlyphUnproj(sv.at[0], sv.at[1])
		src = "share at= (view centre)"
	case hasShare && sv.h3 != "":
		v, _, err := neParseCell(sv.h3)
		if err != nil {
			apiError(w, "share h3= unreadable", 400)
			return
		}
		lon, lat = neCellCentre(v)
		src = "share h3= (its centre)"
	case hasShare && sv.cellXY != nil:
		lon, lat = neGlyphUnproj(sv.cellXY[0], sv.cellXY[1])
		src = "share cell=x,y"
	case hasShare && sv.kg != "":
		b, ok := neStatsKGBox(sv.kg)
		if !ok {
			apiError(w, "share kg= unknown", 404)
			return
		}
		lon, lat = (b[0]+b[2])/2, (b[1]+b[3])/2
		src = "share kg= (bbox centre)"
	default:
		apiError(w, "pt=lon,lat | lon=&lat= | cell=<res-12> | share=<glyph hash> required", 400)
		return
	}
	if lon < 9.4 || lon > 17.3 || lat < 46.3 || lat > 49.1 {
		apiError(w, "point outside Austria", 400)
		return
	}
	if id == 0 {
		v, err := neLatLngToCell(lon, lat, neCellRes)
		if err != nil {
			apiError(w, "bad point", 400)
			return
		}
		id = v
	}
	n := 24
	if v, err := strconv.Atoi(q.Get("n")); err == nil && v >= 0 {
		n = min(v, neWhereMaxN)
	}
	pub := publicURL()
	pr := peers()
	px, py := neGlyphProj(lon, lat)
	out := map[string]interface{}{
		"point": map[string]interface{}{"lon": neRound(lon, 6), "lat": neRound(lat, 6), "from": src, "xy": []float64{math.Round(px*10) / 10, math.Round(py*10) / 10}},
		"asof":  time.Now().UTC().Format(time.RFC3339),
	}
	// ── the view ──
	var z float64
	var vbox *[4]float64
	view := map[string]interface{}{}
	if hasShare {
		view["hash"], view["profile"], view["flags"] = sv.Hash, sv.Profile, sv.Flags
		if sv.Unit != nil {
			view["unit"] = sv.Unit
		}
		if len(sv.dropped) > 0 {
			view["dropped"] = sv.dropped
		}
		ar := float64(fr.h) / float64(neGlyphW)
		if v, err := strconv.ParseFloat(q.Get("ar"), 64); err == nil && v > 0.1 && v < 10 {
			ar = v
		}
		cx, cy := px, py
		if sv.at != nil {
			cx, cy, z = sv.at[0], sv.at[1], sv.at[2]
		} else if sv.z > 0 {
			z = sv.z
		}
		if z > 0 {
			vw := float64(neGlyphW) / z
			vh := vw * ar
			w0, s0 := neGlyphUnproj(cx-vw/2, cy+vh/2)
			e0, n0 := neGlyphUnproj(cx+vw/2, cy-vh/2)
			vbox = &[4]float64{w0, s0, e0, n0}
			lod := neWhereLOD(z)
			full := z >= neWhereHiMin && (sv.has("full") || (z > neWhereZMAX0 && !sv.has("nofull")))
			view["zoom"], view["lod"], view["full"] = neRound(z, 2), lod, full
			view["aspect"] = neRound(ar, 4)
			view["bbox"] = []float64{neRound(w0, 5), neRound(s0, 5), neRound(e0, 5), neRound(n0, 5)}
			clon, clat := neGlyphUnproj(cx, cy)
			view["centre"] = []float64{neRound(clon, 6), neRound(clat, 6)}
			L := neWhereLODDeg[lod]
			i0, i1 := int(math.Floor(w0/L)), int(math.Floor(e0/L))
			j0, j1 := int(math.Floor(s0/L)), int(math.Floor(n0/L))
			view["tiles_in_view"] = map[string]interface{}{"lod": lod, "deg": L, "i": []int{i0, i1}, "j": []int{j0, j1}, "n": (i1 - i0 + 1) * (j1 - j0 + 1)}
			view["cannot_show"] = neWhereCannot(&sv, z)
		}
		if sv.F != "" {
			view["f"], view["f_held"] = sv.F, sv.FHeld
		}
		out["view"] = view
	}
	// ── tiles of the point ──
	vq := ""
	if sv.Profile == "obs" {
		vq = "obs"
	}
	tiles := map[string]interface{}{}
	for lod := 0; lod < 3; lod++ {
		i, j := neWhereTileKey(lod, lon, lat)
		tiles[strconv.Itoa(lod)] = map[string]interface{}{"key": fmt.Sprintf("%d/%d/%d", lod, i, j), "deg": neWhereLODDeg[lod], "url": fmt.Sprintf("%s/api/v1/ne/glyph/tile?lod=%d&i=%d&j=%d&v=%s", pub, lod, i, j, vq)}
	}
	out["tiles"] = tiles
	// ── the cell ──
	cell := neGlyphCellDbg(id)
	doc, held := neCellDoc(id, &neReq{public: true, bevFirst: true})
	cell["held"] = held
	if held {
		cell["kgs"] = doc["kgs"]
		cell["lu_decoded"] = doc["lu"]
		if doc["superseded"] != nil {
			cell["superseded"] = true
		}
		cell["chg"] = doc["chg"]
		cell["obs_decoded"] = doc["obs"]
		cell["pic_obs"] = neGlyphPicObs(id)
		kgs, _ := doc["kgs"].(map[string]uint8)
		if pd := neGlyphPicDif(id, kgs); pd != nil {
			cell["pic_dif"] = pd
		}
	} else if p := nePendingKGs(KGCodesNearPointMem(lon, lat, 0.005)); len(p) > 0 {
		cell["pending_kgs"] = p
	}
	out["cell"] = cell
	// ── the fidx row ──
	b, k, st, why := neWhereBinRow(id)
	bi, bj := neWhereTileKey(0, lon, lat)
	bin := map[string]interface{}{"key": fmt.Sprintf("%d/%d", bi, bj), "state": st, "url": fmt.Sprintf("%s/api/v1/ne/glyph/fidx?i=%d&j=%d", pub, bi, bj)}
	if b != nil {
		bin["n"], bin["unk"], bin["build_ms"] = b.N, b.Hdr.Unk, b.Hdr.BuiltMs
		bin["srtm"], bin["sub"], bin["n2k"] = b.Hdr.Srtm, b.Hdr.Sub != nil, b.Hdr.N2K
		bin["tree_lists"] = b.TH != nil
		bin["row"] = k
	}
	if why != "" {
		bin["note"] = why
	}
	if b != nil && k >= 0 {
		var only map[string]bool
		if q.Get("fields") != "all" && q.Get("fields") != "" {
			only = map[string]bool{}
			for _, f := range strings.Split(q.Get("fields"), ",") {
				only[strings.TrimSpace(f)] = true
			}
		}
		bin["vals"] = neWhereVals(b, k, only)
	}
	out["fidx"] = bin
	// the peer's cells block as we parsed it (LRU only — never a fetch on a read)
	sb := map[string]interface{}{"url": fmt.Sprintf("%s/api/v1/cells?bbox=%.3f,%.3f,%.3f,%.3f&format=bin&block=1&fields=%s", pr.ObsAPI, math.Floor(lon/0.1)*0.1, math.Floor(lat/0.1)*0.1, math.Floor(lon/0.1)*0.1+0.1, math.Floor(lat/0.1)*0.1+0.1, neTreeHFields())}
	sb["manifest_gen"] = neFidxBlockGen(int(math.Floor(lon/0.1)), int(math.Floor(lat/0.1)))
	if sr, ok := neFidxSrtmBlk.get(fmt.Sprintf("%d/%d", bi, bj)); ok && sr != nil {
		sb["cached"], sb["epoch"], sb["gen"], sb["n"] = true, sr.epoch, sr.gen, len(sr.ids)
		if x := sr.find(id); x >= 0 {
			raw := map[string]interface{}{}
			for name, c := range sr.cols {
				if x < len(c) {
					raw[name] = c[x]
				}
			}
			if sr.th != nil && x+1 < len(sr.th.Off) {
				l := []float64{}
				for _, h := range sr.th.H[sr.th.Off[x]:sr.th.Off[x+1]] {
					l = append(l, float64(h)*0.5)
				}
				raw["tree_h"] = l
			}
			sb["row"] = raw
		} else {
			sb["row"] = nil
		}
	} else {
		sb["cached"] = false
	}
	out["peer_block"] = sb
	// ── the filter ──
	fstr := q.Get("f")
	if fstr == "" {
		fstr = sv.F
	}
	if fstr != "" {
		e := neFiltParse(fstr)
		terms, match := neWhereTerms(b, k, e)
		fl := map[string]interface{}{"expr": e.Canonical(), "words": e.Words(), "ignored": e.Ignored, "incomplete": e.Incomplete, "held": sv.FHeld && q.Get("f") == "", "k": e.K, "terms": terms, "cell_match": match}
		if b == nil || k < 0 {
			fl["cell_match"] = nil
		}
		if n > 0 && !e.empty() {
			bw, bs, be, bn := lon-neFidxBinDeg/2, lat-neFidxBinDeg/2, lon+neFidxBinDeg/2, lat+neFidxBinDeg/2
			if vbox != nil {
				bw, bs, be, bn = vbox[0], vbox[1], vbox[2], vbox[3]
			}
			fl["view"] = neWhereMatches(bw, bs, be, bn, lon, lat, e, n)
		}
		out["filter"] = fl
	}
	// ── URLs ──
	bb12, _ := cell["bbox12"].([]float64)
	urls := map[string]interface{}{
		"glyph":      fmt.Sprintf("%s/#pt=%.5f,%.5f;v=obs;full;z=%d", pub, lon, lat, neWhereHiLOD1),
		"glyph_dbg":  fmt.Sprintf("%s/?dbg#pt=%.5f,%.5f;v=obs;full;z=%d", pub, lon, lat, neWhereHiLOD1),
		"where_txt":  fmt.Sprintf("%s/api/v1/ne/where?cell=%s&format=txt", pub, neCellHex(id)),
		"cell":       fmt.Sprintf("%s/api/v1/ne/cell/%s", pub, neCellHex(id)),
		"glyph_cell": fmt.Sprintf("%s/api/v1/ne/glyph/cell?cell=%s&dbg=1", pub, neCellHex(id)),
		"stats_unit": fmt.Sprintf("%s/api/v1/ne/stats/unit?pt=%.5f,%.5f", pub, lon, lat),
		"filter":     fmt.Sprintf("%s/api/v1/ne/filter?bbox=%.4f,%.4f,%.4f,%.4f&f=%s&format=cells", pub, lon-0.0125, lat-0.0125, lon+0.0125, lat+0.0125, url.QueryEscape(fstr)),
	}
	if len(bb12) == 4 {
		urls["peer_cell"] = fmt.Sprintf("%s/api/v1/landscape?include=cells&bbox=%.6f,%.6f,%.6f,%.6f", pr.ObsWeb, bb12[0], bb12[1], bb12[2], bb12[3])
	}
	if r7, ok := cell["r7"].(map[string]interface{}); ok {
		if bb7, ok := r7["bbox"].([]float64); ok && len(bb7) == 4 {
			urls["peer_r7"] = fmt.Sprintf("%s/api/v1/landscape?include=cells&bbox=%.5f,%.5f,%.5f,%.5f", pr.ObsWeb, bb7[0], bb7[1], bb7[2], bb7[3])
		}
	}
	var pk []string
	if kgs, ok := cell["kgs"].(map[string]uint8); ok {
		for code := range kgs {
			pk = append(pk, fmt.Sprintf("%s/api/v1/kg/%s/cells?format=json", pr.ObsWeb, code))
		}
		sort.Strings(pk)
	}
	if len(pk) > 0 {
		urls["peer_kg"] = pk
	}
	if hasShare {
		urls["share_stats"] = fmt.Sprintf("%s/api/v1/ne/stats/unit?share=%s&format=flat", pub, url.QueryEscape(sv.Hash))
	}
	out["urls"] = urls
	out["peer"] = pr.ObsName
	out["notice"] = NENotice()
	out["attribution"] = neFidxAttribution
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-NE-Notice", NENotice())
	if q.Get("format") == "txt" || (q.Get("format") == "" && strings.HasPrefix(r.Header.Get("Accept"), "text/plain")) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(neWhereText(out)))
		return
	}
	writeJSON(w, 200, out)
}

// neWhereCannot — toggles the page keeps in the hash but cannot show at this zoom.
func neWhereCannot(sv *neWhereView, z float64) []string {
	var out []string
	if z < neWhereHiLOD0 {
		for _, f := range []string{"trees", "diff", "ortho", "roads"} {
			if sv.has(f) {
				out = append(out, f+" (below 110×)")
			}
		}
	}
	if z > neWhereHiLOD1 && sv.has("relief") {
		out = append(out, "relief (past 300×)")
	}
	if z < neWhereHiMin && sv.has("full") {
		out = append(out, "full (below 8×)")
	}
	return out
}

// neWhereText — the panel's text, line for line.
func neWhereText(o map[string]interface{}) string {
	var sb strings.Builder
	p := func(f string, a ...interface{}) { sb.WriteString(fmt.Sprintf(f, a...)); sb.WriteByte('\n') }
	js := func(v interface{}) string { b, _ := json.Marshal(v); return string(b) }
	pt := o["point"].(map[string]interface{})
	p("WHERE  %s  (%s)", o["asof"], pt["from"])
	p("lon,lat  %.6f,%.6f   lat,lon %.6f,%.6f   xy %v", pt["lon"], pt["lat"], pt["lat"], pt["lon"], js(pt["xy"]))
	if v, ok := o["view"].(map[string]interface{}); ok {
		p("hash  #%s", v["hash"])
		if v["zoom"] != nil {
			p("view  z=%v lod=%v profile=%v full=%v flags=%s bbox=%s centre=%s tiles_in_view=%s", v["zoom"], v["lod"], v["profile"], v["full"], js(v["flags"]), js(v["bbox"]), js(v["centre"]), js(v["tiles_in_view"]))
			if c, _ := v["cannot_show"].([]string); len(c) > 0 {
				p("view  cannot show at this zoom: %s", strings.Join(c, ", "))
			}
		} else {
			p("view  profile=%v flags=%s (no at=/z=: no view box)", v["profile"], js(v["flags"]))
		}
		if v["unit"] != nil {
			p("unit  %s", js(v["unit"]))
		}
		if v["dropped"] != nil {
			p("hash dropped  %s", js(v["dropped"]))
		}
	}
	if t, ok := o["tiles"].(map[string]interface{}); ok {
		var ks []string
		for lod := 0; lod < 3; lod++ {
			if m, ok := t[strconv.Itoa(lod)].(map[string]interface{}); ok {
				ks = append(ks, fmt.Sprint(m["key"]))
			}
		}
		p("tiles  %s   tile url lod1: %v", strings.Join(ks, "  "), t["1"].(map[string]interface{})["url"])
	}
	c := o["cell"].(map[string]interface{})
	p("r12  %v  centre %.6f,%.6f  bbox12 %s  held=%v", c["cell"], c["lon"], c["lat"], js(c["bbox12"]), c["held"])
	if ps, ok := c["parents"].(map[string]string); ok {
		p("h3   r11 %s  r10 %s  r9 %s  r8 %s  r7 %s  chunk9 %v", ps["11"], ps["10"], ps["9"], ps["8"], ps["7"], c["chunk9"])
	}
	if r7, ok := c["r7"].(map[string]interface{}); ok {
		p("r7   %v frame#%v bbox7 %s", r7["cell"], r7["index"], js(r7["bbox"]))
	}
	if c["kgs"] != nil {
		p("kgs  %s", js(c["kgs"]))
	}
	if c["pending_kgs"] != nil {
		p("pending kgs  %s", js(c["pending_kgs"]))
	}
	if c["lu_decoded"] != nil {
		p("lu   %s", js(c["lu_decoded"]))
	}
	if c["chg"] != nil {
		p("chg  %s", js(c["chg"]))
	}
	if c["pic_obs"] != nil {
		p("pic_obs  %s", js(c["pic_obs"]))
	}
	if c["pic_dif"] != nil {
		p("pic_dif  %s", js(c["pic_dif"]))
	}
	if rows, ok := c["obs"].([]map[string]interface{}); ok && len(rows) > 0 {
		for _, r := range rows {
			p("OBS row kg=%v source=%v epoch=%v algo=%v built_at=%v digest=%v rows=%v record=%v", r["kg"], r["source"], r["epoch"], r["algo"], r["built_at"], r["digest"], r["rows"], r["record"])
			if r["row"] != nil {
				p("  %s", js(r["row"]))
			}
		}
	} else {
		p("OBS row  none held for this cell")
	}
	if b, ok := o["fidx"].(map[string]interface{}); ok {
		note := ""
		if b["note"] != nil {
			note = "  " + fmt.Sprint(b["note"])
		}
		p("fidx bin %v state=%v row=%v n=%v unk=%v build_ms=%v srtm=%s tree_lists=%v url=%v%s", b["key"], b["state"], b["row"], b["n"], b["unk"], b["build_ms"], js(b["srtm"]), b["tree_lists"], b["url"], note)
		if vals, ok := b["vals"].(map[string]interface{}); ok {
			ks := make([]string, 0, len(vals))
			for k := range vals {
				ks = append(ks, k)
			}
			sort.Strings(ks)
			parts := make([]string, 0, len(ks))
			for _, k := range ks {
				m := vals[k].(map[string]interface{})
				if m["v"] == nil {
					parts = append(parts, k+"=∅")
				} else {
					parts = append(parts, k+"="+js(m["v"]))
				}
			}
			p("fidx vals  %s", strings.Join(parts, " "))
			p("fidx src   (∅ = no value) %s", func() string {
				var s []string
				for _, k := range ks {
					m := vals[k].(map[string]interface{})
					if m["v"] != nil {
						s = append(s, k+"←"+fmt.Sprint(m["src"]))
					}
				}
				return strings.Join(s, " · ")
			}())
		}
	}
	if s, ok := o["peer_block"].(map[string]interface{}); ok {
		if s["cached"] == true {
			p("peer block cached epoch=%v gen=%v manifest_gen=%v n=%v row=%s", s["epoch"], s["gen"], s["manifest_gen"], s["n"], js(s["row"]))
		} else {
			p("peer block not cached (fetched at the next fidx build; manifest_gen=%v — a gen change at the peer queues a refresh build of held bins)", s["manifest_gen"])
		}
		p("peer block url  %v", s["url"])
	}
	if f, ok := o["filter"].(map[string]interface{}); ok {
		p("FILTER %s  held=%v k=%v cell_match=%v ignored=%s incomplete=%s", f["expr"], f["held"], f["k"], f["cell_match"], js(f["ignored"]), js(f["incomplete"]))
		if terms, ok := f["terms"].([]map[string]interface{}); ok {
			for _, t := range terms {
				mark := "✗"
				if t["pass"] == true {
					mark = "✓"
				}
				val := "∅"
				if t["value"] != nil {
					val = js(t["value"])
				}
				p("  %s %-18s group %v  value=%s raw=%v", mark, t["term"], t["group"], val, t["raw"])
			}
		}
		if v, ok := f["view"].(map[string]interface{}); ok {
			p("view match  bbox=%s clipped=%v bins=%v partial=%v cells=%v match=%v listed=%v (nearest first)", js(v["bbox"]), v["clipped"], v["bins"], v["partial_bins"], v["cells"], v["match"], v["listed"])
			if l, ok := v["nearest"].([]map[string]interface{}); ok {
				for _, m := range l {
					p("  %v  %.6f,%.6f  %vm  %s", m["cell"], m["lon"], m["lat"], m["dist_m"], js(m["vals"]))
				}
			}
		}
	}
	if u, ok := o["urls"].(map[string]interface{}); ok {
		ks := make([]string, 0, len(u))
		for k := range u {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			if l, ok := u[k].([]string); ok {
				for _, x := range l {
					p("%-11s %s", k, x)
				}
			} else {
				p("%-11s %v", k, u[k])
			}
		}
	}
	p("peer  %v   notice  %v", o["peer"], o["notice"])
	return sb.String()
}

// neShareViewBox — the lon/lat box a share hash's `at=cx,cy,z` (or `z=`)
// shows at the landing page's aspect ratio; nil when the hash carries no view.
// Used by the QGIS exports (`mvt.qgs`, `pmtiles`) when a share link has no
// unit key (kg/h3/adm/pt) — the user wants what they were looking at.
func neShareViewBox(raw string) *[4]float64 {
	sv := neWhereParseShare(raw)
	if sv.at == nil && sv.z <= 0 {
		return nil
	}
	if neGlyphLoadFrame() != nil { // the frame is lazy: a share link may be the first thing after start
		return nil
	}
	fr := &neGlyphFrame
	cx, cy, z := float64(neGlyphW)/2, float64(fr.h)/2, sv.z
	if sv.at != nil {
		cx, cy, z = sv.at[0], sv.at[1], sv.at[2]
	}
	if z <= 0 {
		return nil
	}
	vw := float64(neGlyphW) / z
	vh := vw * float64(fr.h) / float64(neGlyphW)
	w0, s0 := neGlyphUnproj(cx-vw/2, cy+vh/2)
	e0, n0 := neGlyphUnproj(cx+vw/2, cy-vh/2)
	if !(w0 < e0 && s0 < n0) {
		return nil
	}
	return &[4]float64{w0, s0, e0, n0}
}

// neQuery — r.URL.Query() that survives a raw ';' in a value. Go drops any
// pair containing a semicolon, which is exactly what an unencoded share hash
// (`share=v=obs;full;at=…`) pasted into a URL looks like; here ';' is
// re-encoded first so the pair reads as the caller meant.
func neQuery(r *http.Request) url.Values {
	if !strings.Contains(r.URL.RawQuery, ";") {
		return r.URL.Query()
	}
	q, err := url.ParseQuery(strings.ReplaceAll(r.URL.RawQuery, ";", "%3B"))
	if err != nil {
		return r.URL.Query()
	}
	return q
}
