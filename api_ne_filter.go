// api_ne_filter.go — the glyph filter as an endpoint (public, tokenless,
// docs/ne-filter.md §2c): the same grammar, the same fidx bins, the same
// evaluator the Worker runs — what a human builds with chips an agent pastes.
//
//	GET /api/v1/ne/filter?bbox=w,s,e,n&f=<expr>[&format=count|cells|h3|geojson]
//	GET /api/v1/ne/filter?kg=63330&f=…             the KG's bbox as the bulk key
//	GET /api/v1/ne/filter/fields                   the catalogue: fields, named words, presets, grammar
//
// count   {cells, match, ha, by_d[9], bins, partial_bins, expr, words, ignored, link}
// cells   + "cells":[hex ids] (≤ 65 536, else 413 → split the box)
// h3      + "h3":[compacted ids]
// geojson FeatureCollection of cell hexagons (≤ 4 096)
//
// bbox span ≤ 0.1° (16 bins). Public-tier rules: keyed by bbox / KG, no
// object-level words (TestNEPublicWordBan), NENotice + peer attributions.
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/uber/h3-go/v4"
)

const neFilterGrammar = "f=<group>(,<group>)*  group=<term>(|<term>)*  term=<field><op><value> · `,` AND between groups · `|` OR inside a group · ranges a..b / a.. / ..b (or >=a, <=b) · sets m1+m2 · flags bare · field=unk matches cells without a value · k>=n keeps match clusters of ≥ n cells · unknown fields and malformed terms are ignored one by one"

// handleNEFilterFields — GET /api/v1/ne/filter/fields
func handleNEFilterFields(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("X-NE-Notice", NENotice())
	presets := make([]map[string]string, 0, len(neFiltPresets))
	for _, p := range neFiltPresets {
		presets = append(presets, map[string]string{"label": p[0], "f": p[1]})
	}
	writeJSON(w, 200, map[string]interface{}{
		"version": neFidxVer,
		"grammar": neFilterGrammar,
		"fields":  neFiltFields,
		"words":   neFiltWords,
		"presets": presets,
		"bin_deg": neFidxBinDeg,
		"index":   "/api/v1/ne/glyph/fidx?i=&j=&p=core|ext|all  (w = i·0.025, s = j·0.025; application/vnd.ne-fidx, see docs/ne-filter.md §3b)",
		"notice":  NENotice(),
	})
}

type neFilterBinRes struct {
	i, j int
	bin  *neFidxBin
	m    []uint8
	ok   bool
}

// handleNEFilter — GET /api/v1/ne/filter
func handleNEFilter(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var bw, bs, be, bn float64
	kg := q.Get("kg")
	if kg != "" {
		kg = padKGCode(kg)
		var found bool
		for _, b := range kgBoxSnapshot() {
			if b.code == kg {
				bw, bs, be, bn, found = b.minLon, b.minLat, b.maxLon, b.maxLat, true
				break
			}
		}
		if !found {
			apiError(w, "unknown kg", 404)
			return
		}
	} else {
		p := strings.Split(q.Get("bbox"), ",")
		if len(p) != 4 {
			apiError(w, "bbox=w,s,e,n or kg= required", 400)
			return
		}
		var v [4]float64
		for k := range p {
			x, err := strconv.ParseFloat(strings.TrimSpace(p[k]), 64)
			if err != nil {
				apiError(w, "bbox=w,s,e,n", 400)
				return
			}
			v[k] = x
		}
		bw, bs, be, bn = v[0], v[1], v[2], v[3]
	}
	if !(bw < be && bs < bn) || be < 9.4 || bw > 17.3 || bn < 46.3 || bs > 49.1 {
		apiError(w, "box outside Austria or empty", 400)
		return
	}
	if be-bw > 0.1001 || bn-bs > 0.1001 {
		apiError(w, "bbox span ≤ 0.1° per call (16 bins) — split the box", 413)
		return
	}
	format := q.Get("format")
	if format == "" {
		format = "count"
	}
	if format != "count" && format != "cells" && format != "h3" && format != "geojson" {
		apiError(w, "format=count|cells|h3|geojson", 400)
		return
	}
	expr := neFiltParse(q.Get("f"))
	if err := neGlyphLoadFrame(); err != nil {
		apiError(w, "frame unavailable", 503)
		return
	}
	fr := &neGlyphFrame
	i0, i1 := int(math.Floor(bw/neFidxBinDeg)), int(math.Floor((be-1e-9)/neFidxBinDeg))
	j0, j1 := int(math.Floor(bs/neFidxBinDeg)), int(math.Floor((bn-1e-9)/neFidxBinDeg))
	var bins []neFilterBinRes
	partial := 0
	for i := i0; i <= i1; i++ {
		for j := j0; j <= j1; j++ {
			res := neFilterBinRes{i: i, j: j}
			if full, _, err := neFidxGet(i, j); err == nil {
				if b, err := neFidxDecode(full); err == nil {
					res.bin, res.m, res.ok = b, b.eval(expr), true
				}
			}
			if !res.ok {
				partial++
			}
			bins = append(bins, res)
		}
	}
	// counts over cells whose EXACT centre lies in the box (the fitted
	// position is a coarse cut only — same membership as /ne/stats/unit)
	cells, match := 0, 0
	var ha float64
	var byD [9]int
	type hit struct {
		b *neFidxBin
		k int
	}
	var hits []hit
	for _, res := range bins {
		if !res.ok {
			continue
		}
		b := res.bin
		d := b.Cols["d"]
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
			if res.m[k] == 0 {
				continue
			}
			match++
			if a, err := h3.CellAreaM2(h3.Cell(id)); err == nil {
				ha += a / 1e4
			}
			if d != nil && d[k] < 9 {
				byD[d[k]]++
			}
			if format != "count" {
				hits = append(hits, hit{b, k})
			}
		}
	}
	limit := 65536
	if format == "geojson" {
		limit = 4096
	}
	if format != "count" && len(hits) > limit {
		apiError(w, fmt.Sprintf("%d matching cells > %d — split the box or use format=count", len(hits), limit), 413)
		return
	}
	link := fmt.Sprintf("%s/#pt=%.5f,%.5f;z=%d", publicURL(), (bw+be)/2, (bs+bn)/2, neFilterZoomFor(be-bw))
	if c := expr.Canonical(); c != "" {
		link += ";f=" + c
	}
	out := map[string]interface{}{
		"bbox": []float64{bw, bs, be, bn}, "expr": expr.Canonical(), "words": expr.Words(), "ignored": expr.Ignored,
		"cells": cells, "match": match, "ha": math.Round(ha*10) / 10, "by_d": neFilterByD(byD),
		"bins": len(bins), "partial_bins": partial, "link": link, "notice": NENotice(), "attribution": neFidxAttribution,
	}
	if kg != "" {
		out["kg"] = kg
		out["note"] = "kg= selects the KG's bounding box; cells of neighbouring KGs inside it count too"
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("X-NE-Notice", NENotice())
	w.Header().Set("X-Data-Attribution", asciiFold(neFidxAttribution))
	w.Header().Set("Cache-Control", "public, max-age=300")
	if partial > 0 {
		w.Header().Set("Cache-Control", "no-store")
	}
	if format == "count" {
		writeJSON(w, 200, out)
		return
	}
	ids := make([]uint64, 0, len(hits))
	origins := map[*neFidxBin]h3.Cell{}
	for _, h := range hits {
		o, ok := origins[h.b]
		if !ok {
			id, _, err := neParseCell(h.b.Hdr.Origin)
			if err != nil {
				continue
			}
			o = h3.Cell(id)
			origins[h.b] = o
		}
		if id, ok := h.b.cell(o, h.k); ok {
			ids = append(ids, id)
		}
	}
	sortU64(ids)
	switch format {
	case "cells":
		hx := make([]string, len(ids))
		for k, id := range ids {
			hx[k] = neCellHex(id)
		}
		out["cells_h3"] = hx
		writeJSON(w, 200, out)
	case "h3":
		cs := make([]h3.Cell, len(ids))
		for k, id := range ids {
			cs[k] = h3.Cell(id)
		}
		comp := cs
		if len(cs) > 0 {
			if c, err := h3.CompactCells(cs); err == nil {
				comp = c
			}
		}
		hx := make([]string, len(comp))
		for k, c := range comp {
			hx[k] = neCellHex(uint64(c))
		}
		out["h3"] = hx
		out["h3_note"] = "compacted (h3.compact): uncompact to res 12 for the cells"
		writeJSON(w, 200, out)
	case "geojson":
		feats := make([]map[string]interface{}, 0, len(ids))
		for _, id := range ids {
			bd, err := h3.Cell(id).Boundary()
			if err != nil {
				continue
			}
			ring := make([][]float64, 0, len(bd)+1)
			for _, p := range bd {
				ring = append(ring, []float64{math.Round(p.Lng*1e7) / 1e7, math.Round(p.Lat*1e7) / 1e7})
			}
			ring = append(ring, ring[0])
			feats = append(feats, map[string]interface{}{"type": "Feature", "properties": map[string]interface{}{"h3": neCellHex(id)}, "geometry": map[string]interface{}{"type": "Polygon", "coordinates": [][][]float64{ring}}})
		}
		delete(out, "notice")
		w.Header().Set("Content-Type", "application/geo+json")
		enc := json.NewEncoder(w)
		_ = enc.Encode(map[string]interface{}{"type": "FeatureCollection", "features": feats, "properties": out, "notice": NENotice()})
	}
}

func neFilterByD(b [9]int) map[string]int {
	m := make(map[string]int, 9)
	for k, g := range neGlyphGroups {
		if b[k] > 0 {
			m[g] = b[k]
		}
	}
	return m
}

// neFilterZoomFor — a glyph zoom that shows a box of the given span (the hash's z= is W/vb.w; 0.025° ≈ z 300).
func neFilterZoomFor(span float64) int {
	if span <= 0 {
		return 300
	}
	z := int(300 * 0.025 / span)
	if z < 12 {
		z = 12
	}
	if z > 900 {
		z = 900
	}
	return z
}

// neFilterLink — share link for an expression (used by docs/tests).
func neFilterLink(expr string) string {
	return publicURL() + "/#f=" + url.PathEscape(neFiltParse(expr).Canonical())
}
