// api_toponyms.go — HTTP surface for the BEV DLM-Namen toponym layer
// (feedback #17). Data + ranking live in toponyms.go.
//
//	GET /api/v1/toponyms/search?q=&layer=&f_code=&kg=&gemeinde=&district=&state=&near=lon,lat&radius=&stem=&min_score=
//	GET /api/v1/toponyms/near?lon=&lat=&radius=&layer=&f_code=
//	GET /api/v1/toponyms/stats
//	GET /api/v1/toponyms/{id}                — full row + currency
//	GET /api/v1/kg/{kg_code}/toponyms        — everything named inside one KG (Rieden first)
//
// Plus hooks: layers=toponyms on /spatial/bbox and /spatial/point,
// /lookup?type=toponym (+ fall-through), /search/text fall-through,
// /stats.toponyms, /export/geojson?layers=toponyms.
package main

import (
	"net/http"
	"strconv"
	"strings"
)

func RegisterToponymAPI(mux *http.ServeMux) {
	mux.Handle("/api/v1/toponyms/search", apiMiddleware(handleToponymSearch))
	mux.Handle("/api/v1/toponyms/near", apiMiddleware(handleToponymNear))
	mux.Handle("/api/v1/toponyms/stats", apiMiddleware(handleToponymStats))
	mux.Handle("/api/v1/toponyms/", apiMiddleware(handleToponymByID))
	mux.Handle("/api/v1/toponyms", apiMiddleware(handleToponymSearch))
	mux.Handle("/api/v1/kg/", apiMiddleware(handleKGSubresource))
}

func toponymsUnavailable(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "60")
	apiError(w, "toponym layer not loaded — operator must run `./umfeld-at -toponyms-import` (BEV DLM-Namen)", http.StatusServiceUnavailable)
}

// parseCSVSet splits a comma list into a set (empty → nil).
func parseCSVSet(s string, canon func(string) string) map[string]bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	out := map[string]bool{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if canon != nil {
			p = canon(p)
		}
		out[p] = true
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func parseToponymLayers(r *http.Request) map[string]bool {
	q := r.URL.Query()
	v := q.Get("layer")
	if v == "" {
		v = q.Get("layers")
	}
	return parseCSVSet(v, canonicalToponymLayer)
}

// parseToponymSubLayers reads ?toponym_layer= (the spatial endpoints already
// use ?layers= for parcels/buildings/toponyms).
func parseToponymSubLayers(r *http.Request) map[string]bool {
	q := r.URL.Query()
	v := q.Get("toponym_layer")
	if v == "" {
		v = q.Get("toponym_layers")
	}
	return parseCSVSet(v, canonicalToponymLayer)
}

func parseFCodes(s string) map[int]bool {
	set := parseCSVSet(s, nil)
	if set == nil {
		return nil
	}
	out := map[int]bool{}
	for k := range set {
		if n, err := strconv.Atoi(k); err == nil {
			out[n] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// kgSetFromAdmin turns a gemeinde/district value (code OR name) into the set
// of KG codes, via the in-memory KG name index (no LIKE on SQL, Query-Cost
// Contract rule 2). Returns (set, resolved). resolved=false ⇒ leave unfiltered.
func kgSetFromAdmin(f KGCodeFilter) (map[string]bool, bool) {
	codes, ok := ResolveKGCodes(f)
	if !ok {
		return nil, false
	}
	set := make(map[string]bool, len(codes))
	for _, c := range codes {
		set[c] = true
	}
	return set, true
}

// parseToponymQuery builds the shared filter from query params.
func parseToponymQuery(r *http.Request) (*ToponymQuery, string) {
	q := r.URL.Query()
	tq := &ToponymQuery{
		Q:        strings.TrimSpace(q.Get("q")),
		Layers:   parseToponymLayers(r),
		FCodes:   parseFCodes(q.Get("f_code")),
		Stem:     true,
		MinScore: toponymsDefaultMinScore,
	}
	if v := q.Get("stem"); v == "0" || v == "false" {
		tq.Stem = false
	}
	if v := q.Get("min_score"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f <= 1 {
			tq.MinScore = f
		}
	}
	if v := q.Get("abandoned"); v != "" {
		b := v == "1" || v == "true"
		tq.Abandoned = &b
	}
	// KG: explicit codes (middleware already zero-padded ?kg=).
	if v := q.Get("kg"); v != "" {
		tq.KG = parseCSVSet(v, NormalizeKGCode)
	}
	// gemeinde / district: code or name → KG set (intersected with ?kg= if both).
	gem := q.Get("gemeinde")
	if gem == "" {
		gem = q.Get("gemeinde_code")
	}
	dis := q.Get("district")
	if dis == "" {
		dis = q.Get("district_code")
	}
	if gem != "" || dis != "" {
		if set, ok := kgSetFromAdmin(KGCodeFilter{Gemeinde: gem, District: dis}); ok {
			if tq.KG == nil {
				tq.KG = set
			} else {
				for k := range tq.KG {
					if !set[k] {
						delete(tq.KG, k)
					}
				}
			}
			if len(tq.KG) == 0 {
				tq.KG = map[string]bool{"": true} // matches nothing
			}
		} else {
			// index not loaded: fall back to exact-code filters only
			if isNumericCode(gem) {
				tq.Gemeinde = map[string]bool{gem: true}
			}
			if isNumericCode(dis) {
				tq.District = map[string]bool{dis: true}
			}
		}
	}
	if v := q.Get("state"); v != "" {
		tq.State = normalizeToponym(v)
	}
	if v := q.Get("near"); v != "" {
		parts := strings.Split(v, ",")
		if len(parts) != 2 {
			return nil, "near must be lon,lat"
		}
		lon, e1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
		lat, e2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if e1 != nil || e2 != nil {
			return nil, "near must be lon,lat (decimal degrees)"
		}
		tq.HasNear, tq.NearLon, tq.NearLat = true, lon, lat
		if rad := q.Get("radius"); rad != "" {
			if f, err := strconv.ParseFloat(rad, 64); err == nil && f > 0 {
				tq.RadiusM = f
			}
		}
	} else if q.Get("lon") != "" && q.Get("lat") != "" {
		lon, e1 := strconv.ParseFloat(q.Get("lon"), 64)
		lat, e2 := strconv.ParseFloat(q.Get("lat"), 64)
		if e1 == nil && e2 == nil {
			tq.HasNear, tq.NearLon, tq.NearLat = true, lon, lat
			if rad := q.Get("radius"); rad != "" {
				if f, err := strconv.ParseFloat(rad, 64); err == nil && f > 0 {
					tq.RadiusM = f
				}
			}
		}
	}
	if bb := q.Get("bbox"); bb != "" {
		parts := strings.Split(bb, ",")
		if len(parts) == 4 {
			var b [4]float64
			okAll := true
			for i, p := range parts {
				f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
				if err != nil {
					okAll = false
				}
				b[i] = f
			}
			if okAll {
				tq.BBox = &b
			}
		}
	}
	return tq, ""
}

// handleToponymSearch — GET /api/v1/toponyms/search
func handleToponymSearch(w http.ResponseWriter, r *http.Request) {
	if !ToponymsReady() {
		toponymsUnavailable(w)
		return
	}
	ac := ParseAPIContext(r)
	tq, perr := parseToponymQuery(r)
	if perr != "" {
		apiError(w, perr, 400)
		return
	}
	if tq.Q == "" && tq.KG == nil && tq.Gemeinde == nil && tq.District == nil && tq.State == "" && !tq.HasNear && tq.BBox == nil && tq.Layers == nil && tq.FCodes == nil {
		apiError(w, "q required (or at least one of kg/gemeinde/district/state/near/bbox/layer/f_code to list)", 400)
		return
	}
	if ac.Limit > 5000 {
		ac.Limit = 5000
	}
	rows, total, err := SearchToponyms(tq, ac.Limit, ac.Offset)
	if err != nil {
		apiError(w, err.Error(), 500)
		return
	}
	respondSearchMeta(w, r, ac, rows, total, "toponyms.csv", func(m *Meta) {
		m.License = toponymLicenseMeta()
		m.ToponymNote = toponymSearchNote(tq, total)
		if tq.QueryUsed != "" {
			m.QueryUsed = tq.QueryUsed
		}
	})
}

func toponymSearchNote(tq *ToponymQuery, total int) string {
	n := "names are duplicated nationally (dozens of 'Hochalm'); constrain with kg/gemeinde/district or near=lon,lat. " +
		"Coordinates are BEV DLM points, ±50 m, not parcel-precise. score: 1.0 exact, ≥0.92 prefix, ≥0.85 whole word, else trigram similarity" +
		" (min_score=" + strconv.FormatFloat(tq.MinScore, 'f', 2, 64) + ")"
	if tq.Stem {
		n += "; generic suffixes (alm/alpe/hütte/hof…) are stripped on both sides (stem=0 disables)"
	}
	if total == 0 && tq.Q != "" {
		n += ". No hit: try a shorter stem (the register has no 'Liegnitz' but has 'Lignitz'), lower min_score, or drop filters"
	}
	return n
}

// handleToponymNear — GET /api/v1/toponyms/near?lon=&lat=&radius=
func handleToponymNear(w http.ResponseWriter, r *http.Request) {
	if !ToponymsReady() {
		toponymsUnavailable(w)
		return
	}
	ac := ParseAPIContext(r)
	q := r.URL.Query()
	lon, e1 := strconv.ParseFloat(q.Get("lon"), 64)
	lat, e2 := strconv.ParseFloat(q.Get("lat"), 64)
	if e1 != nil || e2 != nil {
		apiError(w, "lon and lat required", 400)
		return
	}
	radius := toponymsNearDefaultM
	if v := q.Get("radius"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			radius = f
		}
	}
	if radius > toponymsNearMaxM {
		radius = toponymsNearMaxM
	}
	rows, total, err := ToponymsNear(lon, lat, radius, parseToponymLayers(r), parseFCodes(q.Get("f_code")), ac.Limit, ac.Offset)
	if err != nil {
		apiError(w, err.Error(), 500)
		return
	}
	respondSearchMeta(w, r, ac, rows, total, "toponyms_near.csv", func(m *Meta) {
		m.License = toponymLicenseMeta()
		m.ToponymNote = "sorted by distance_m from (" + q.Get("lon") + "," + q.Get("lat") + "), radius " + strconv.FormatFloat(radius, 'f', 0, 64) + " m (max 10000)"
	})
}

// handleToponymStats — GET /api/v1/toponyms/stats
func handleToponymStats(w http.ResponseWriter, r *http.Request) {
	ac := ParseAPIContext(r)
	meta := NewMeta(ac, 1)
	meta.License = toponymLicenseMeta()
	apiResponse(w, r, ToponymStats(), meta)
}

// handleToponymByID — GET /api/v1/toponyms/{id}
func handleToponymByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/toponyms/")
	id = strings.TrimSuffix(id, "/")
	if id == "" {
		handleToponymSearch(w, r)
		return
	}
	if !ToponymsReady() {
		toponymsUnavailable(w)
		return
	}
	ac := ParseAPIContext(r)
	row, err := ToponymByID(id)
	if err != nil {
		apiError(w, err.Error(), 500)
		return
	}
	if row == nil {
		apiError(w, "toponym not found: "+id, 404)
		return
	}
	row["positional_accuracy_m"] = toponymsPositionalAccM
	meta := NewMeta(ac, 1)
	meta.License = toponymLicenseMeta()
	if ac.Format == "geojson" {
		apiGeoJSONResponse(w, r, []map[string]interface{}{toponymFeature(row)}, meta)
		return
	}
	apiResponse(w, r, row, meta)
}

// handleKGSubresource — GET /api/v1/kg/{kg_code}/toponyms
func handleKGSubresource(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/kg/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 2 || parts[1] != "toponyms" {
		apiError(w, "usage: /api/v1/kg/{kg_code}/toponyms", 404)
		return
	}
	kg := NormalizeKGCode(parts[0])
	if !ToponymsReady() {
		toponymsUnavailable(w)
		return
	}
	ac := ParseAPIContext(r)
	if ac.Limit == defaultLimit && r.URL.Query().Get("limit") == "" {
		ac.Limit = 1000 // a KG rarely has more than a few hundred names
	}
	rows, total, err := ToponymsForKG(kg, parseToponymLayers(r), ac.Limit, ac.Offset)
	if err != nil {
		apiError(w, err.Error(), 500)
		return
	}
	respondSearchMeta(w, r, ac, rows, total, "kg_"+kg+"_toponyms.csv", func(m *Meta) {
		m.License = toponymLicenseMeta()
		m.ToponymNote = "Rieden (layer=ried) are BEV's own KG_NUMMER assignment (kg_code_source=bev); other layers are assigned by the NE declared cell set (ne) or the KG bbox (bbox), ±50 m"
	})
}

// toponymLicenseMeta is the BEV block with the DLM-Namen dataset named.
func toponymLicenseMeta() *LicenseMeta {
	m := AttributionMeta(false)
	m.Dataset = "BEV DLM – Geographische Namen (DLM 7000 NAMEN), " + BEVLicenseSPDX + "; see /api/v1/license id=bev_dlm_namen"
	return m
}

// ToponymRowsForKGExport is the /export/geojson layers=toponyms provider.
func ToponymRowsForKGExport(kg string) []map[string]interface{} {
	rows, _, err := ToponymsForKG(kg, nil, 0, 0)
	if err != nil {
		return nil
	}
	return rows
}

// toponymFallbackRows is the shared "0 rows → try toponyms" path for
// /lookup and /search/text. Returns rows tagged type/source=toponym.
func toponymFallbackRows(q string, limit int) []map[string]interface{} {
	if !ToponymsReady() || strings.TrimSpace(q) == "" {
		return nil
	}
	rows, _, err := SearchToponyms(&ToponymQuery{Q: q, Stem: true, MinScore: toponymsDefaultMinScore}, limit, 0)
	if err != nil {
		return nil
	}
	for _, r := range rows {
		r["type"] = "toponym"
		r["source"] = "toponym"
	}
	return rows
}
