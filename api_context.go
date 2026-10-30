// api_context.go — point-keyed overlays for clients that hold their own
// cadastre ({MAP_NAME} via bevdirect, https://github.com/raffopenssh/vtcseamless). docs/api-tiers.md.
//
//	GET /api/v1/context?lon=&lat=[&area_sqm=&building_count=&footprint_area_sqm=
//	                    &landuse_areas=51:420,52:1100&kg=63349&include=…&radius_m=]
//	GET /api/v1/land_prices/point?lon=&lat=&area_sqm=…   (same inputs, price only)
//
// Nothing here touches the parcel index: the caller supplies the parcel's
// representative point and, optionally, the features bevdirect already
// computed (area, building count, Benützungsart areas). We resolve the
// Gemeinde from Statistik Austria polygons, then attach every non-cadastre
// overlay in ONE round trip:
//
//	municipality   Statistik Austria Gemeinde (code, name, district, state)
//	land_price     Statistik Austria-calibrated estimate (needs area_sqm)
//	natura2000     EEA sites containing the point
//	protected_area WDPA sites containing the point (Nationalparks)
//	osm            OSM proximity metrics (roads, transit, water, remoteness)
//	toponyms       DLM-Namen within radius_m (default 500, max 2000)
//	legal          RIS references for the KG code (only when ?kg= is given —
//	               the KG code comes from the client's cadastre, we never
//	               resolve it from the point)
//
// Each block is independent: a missing dataset yields block:null plus a
// `missing` entry, never a failed request. All blocks are pure in-memory or
// PK lookups only (Latency Contract rule 1 trivially).
package main

import (
	"math"
	"net/http"
	"strconv"
	"strings"
)

// `ne` (NE cell of the point, api_ne_public.go) is opt-in via include=ne.
var contextBlocks = []string{"municipality", "land_price", "natura2000", "protected_area", "osm", "toponyms", "legal"}

// pointParams — shared parsing for /context and /land_prices/point.
type pointParams struct {
	Lon, Lat   float64
	KG         string
	Pricing    ParcelPricingInput
	HasPricing bool
}

func parsePointParams(r *http.Request) (*pointParams, string) {
	q := r.URL.Query()
	lon, e1 := strconv.ParseFloat(q.Get("lon"), 64)
	lat, e2 := strconv.ParseFloat(q.Get("lat"), 64)
	if e1 != nil || e2 != nil {
		return nil, "lon and lat required (WGS84 decimal degrees)"
	}
	if lon < 9 || lon > 17.5 || lat < 46 || lat > 49.1 {
		return nil, "point outside Austria (lon 9–17.5, lat 46–49.1)"
	}
	p := &pointParams{Lon: lon, Lat: lat}
	if kg := q.Get("kg"); kg != "" {
		p.KG = NormalizeKGCode(kg)
	}
	in := &p.Pricing
	in.AreaSqm, _ = strconv.ParseFloat(q.Get("area_sqm"), 64)
	in.BuildingCount, _ = strconv.Atoi(q.Get("building_count"))
	in.FootprintCt, _ = strconv.Atoi(q.Get("footprint_count"))
	in.Compactness, _ = strconv.ParseFloat(q.Get("compactness"), 64)
	in.FootprintAreaSqm, _ = strconv.ParseFloat(q.Get("footprint_area_sqm"), 64)
	in.BuildingPeriod = q.Get("building_period")
	in.LanduseCodes = q.Get("landuse_codes")
	in.Typology = q.Get("typology")
	if in.FootprintCt == 0 && in.FootprintAreaSqm > 0 {
		in.FootprintCt = 1
	}
	if in.BuildingCount == 0 && in.FootprintCt > 0 {
		in.BuildingCount = in.FootprintCt
	}
	// landuse_areas=51:420,52:1100 — bevdirect's per-parcel Benützungsart split.
	if la := q.Get("landuse_areas"); la != "" {
		split := &LanduseAreaSplit{Areas: map[string]float64{}, Source: "client"}
		var codes []string
		for _, kv := range strings.Split(la, ",") {
			k, v, ok := strings.Cut(strings.TrimSpace(kv), ":")
			if !ok {
				continue
			}
			a, err := strconv.ParseFloat(v, 64)
			if err != nil || a <= 0 {
				continue
			}
			split.Areas[k] += a
			split.CoveredSqm += a
			codes = append(codes, k)
			if a > split.DominantSqm {
				split.DominantSqm, split.DominantCode = a, k
			}
		}
		if split.CoveredSqm > 0 {
			split.ParcelAreaM2 = in.AreaSqm
			if in.AreaSqm > 0 {
				split.CoverageFrac = math.Min(1, split.CoveredSqm/in.AreaSqm)
			}
			split.DominantFrac = split.DominantSqm / split.CoveredSqm
			in.Split = split
			if in.LanduseCodes == "" {
				in.LanduseCodes = strings.Join(codes, ",")
			}
		}
	}
	p.HasPricing = in.AreaSqm > 0
	return p, ""
}

// municipalityAtPoint — Statistik Austria Gemeinde containing the point.
func municipalityAtPoint(lon, lat float64) map[string]interface{} {
	loadStatAustriaMunicipalities()
	for i := range statAustriaMunicipalities {
		m := &statAustriaMunicipalities[i]
		if !pointInAnyPolygon(lon, lat, m.Polygons) {
			continue
		}
		dist, state := gemeindeCodeParts(m.GemeindeCode)
		return map[string]interface{}{
			"gemeinde_code": m.GemeindeCode,
			"name":          m.Name,
			"district_code": dist,
			"district_name": districtName(dist),
			"state":         state,
			"source":        "statistik_austria",
		}
	}
	return nil
}

// protectedAreasAtPoint — WDPA features containing the point.
func protectedAreasAtPoint(lon, lat float64) []map[string]interface{} {
	out := []map[string]interface{}{}
	for i := range protectedAreaFeatures {
		pf := &protectedAreaFeatures[i]
		if !pointInAnyPolygon(lon, lat, pf.Polygons) {
			continue
		}
		out = append(out, map[string]interface{}{
			"name": pf.Name, "desig": pf.Desig, "desig_eng": pf.DesigEng,
			"desig_type": pf.DesigType, "iucn_cat": pf.IUCNCat, "rep_area": pf.RepArea,
		})
	}
	return out
}

// pricePoint — PredictLandPrice with the Gemeinde resolved from the point.
// Returns (estimate, gemeinde, reason-if-nil).
func pricePoint(p *pointParams, year int) (*LandPriceEstimate, map[string]interface{}, string) {
	muni := municipalityAtPoint(p.Lon, p.Lat)
	if muni == nil {
		return nil, nil, "no Gemeinde contains this point"
	}
	if !p.HasPricing {
		return nil, muni, "area_sqm required for a price estimate"
	}
	in := p.Pricing
	in.GemeindeCode = muni["gemeinde_code"].(string)
	in.StateCode = string(in.GemeindeCode[0])
	est := PredictLandPrice(in, year)
	if est == nil {
		return nil, muni, "land_prices data not available"
	}
	return est, muni, ""
}

// GET /api/v1/land_prices/point
func handleLandPricePoint(w http.ResponseWriter, r *http.Request) {
	p, msg := parsePointParams(r)
	if msg != "" {
		apiError(w, msg, http.StatusBadRequest)
		return
	}
	if !p.HasPricing {
		apiError(w, "area_sqm required (plus optional building_count, footprint_area_sqm, landuse_areas=51:420,52:1100 or landuse_codes, compactness)", http.StatusBadRequest)
		return
	}
	est, muni, reason := pricePoint(p, parseYearParam(r, 2024))
	if est == nil {
		code := http.StatusNotFound
		if strings.Contains(reason, "not available") {
			code = http.StatusServiceUnavailable
		}
		apiError(w, reason, code)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"lon": p.Lon, "lat": p.Lat,
		"municipality": muni,
		"estimate":     est,
		"meta": map[string]interface{}{
			"license": AttributionMapFor("/api/v1/land_prices/point", false),
			"inputs":  "caller-supplied (area_sqm, building_count, footprint_area_sqm, landuse_areas); nothing read from a parcel index",
		},
	})
}

// GET /api/v1/context
func handleV1Context(w http.ResponseWriter, r *http.Request) {
	ac := ParseAPIContext(r)
	p, msg := parsePointParams(r)
	if msg != "" {
		apiError(w, msg, http.StatusBadRequest)
		return
	}
	q := r.URL.Query()
	want := map[string]bool{}
	if inc := q.Get("include"); inc != "" {
		for _, b := range strings.Split(inc, ",") {
			want[strings.TrimSpace(b)] = true
		}
	} else {
		for _, b := range contextBlocks {
			want[b] = true
		}
		if p.KG == "" {
			delete(want, "legal")
		}
	}
	radius := floatParam(r, "radius_m", 500)
	if radius <= 0 || radius > 2000 {
		radius = 500
	}

	out := map[string]interface{}{"lon": p.Lon, "lat": p.Lat}
	missing := map[string]string{}
	sources := []string{}
	withOSM := false

	var muni map[string]interface{}
	if want["municipality"] || want["land_price"] {
		muni = municipalityAtPoint(p.Lon, p.Lat)
		if want["municipality"] {
			if muni == nil {
				out["municipality"] = nil
				missing["municipality"] = "no Gemeinde contains this point"
			} else {
				out["municipality"] = muni
				sources = append(sources, "statistik_austria")
			}
		}
	}
	if want["land_price"] {
		est, _, reason := pricePoint(p, parseYearParam(r, 2024))
		if est == nil {
			out["land_price"] = nil
			missing["land_price"] = reason
		} else {
			out["land_price"] = est
			sources = append(sources, "statistik_austria_prices")
		}
	}
	if want["natura2000"] {
		loadNatura2000()
		if !natura2000Loaded {
			out["natura2000"] = nil
			missing["natura2000"] = "natura2000 data not loaded"
		} else {
			sites := natura2000SitesForPoint(p.Lon, p.Lat)
			rows := make([]map[string]interface{}, 0, len(sites))
			for _, s := range sites {
				rows = append(rows, natura2000SiteToMap(s))
			}
			out["natura2000"] = map[string]interface{}{"in_natura2000": len(rows) > 0, "sites": rows}
			sources = append(sources, "eea_natura2000")
		}
	}
	if want["protected_area"] {
		if len(protectedAreaFeatures) == 0 {
			out["protected_area"] = nil
			missing["protected_area"] = "WDPA data not loaded"
		} else {
			pa := protectedAreasAtPoint(p.Lon, p.Lat)
			out["protected_area"] = map[string]interface{}{"in_protected_area": len(pa) > 0, "sites": pa}
			sources = append(sources, "wdpa")
		}
	}
	if want["osm"] {
		if !OSMProximityReady() {
			out["osm"] = nil
			missing["osm"] = "OSM proximity data not imported"
		} else if m := computeOSMMetrics(p.Lon, p.Lat, p.Pricing.AreaSqm); m != nil {
			out["osm"] = osmMetricsToMap(m)
			sources = append(sources, "openstreetmap")
			withOSM = true
		} else {
			out["osm"] = nil
			missing["osm"] = "metrics computation failed"
		}
	}
	if want["toponyms"] {
		if !ToponymsReady() {
			out["toponyms"] = nil
			missing["toponyms"] = "toponyms not loaded"
		} else {
			limit := ac.Limit
			if limit <= 0 || limit > 50 {
				limit = 20
			}
			rows, total, err := ToponymsNear(p.Lon, p.Lat, radius, parseToponymLayers(r), parseFCodes(q.Get("f_code")), limit, 0)
			if err != nil {
				out["toponyms"] = nil
				missing["toponyms"] = err.Error()
			} else {
				if rows == nil {
					rows = []map[string]interface{}{}
				}
				out["toponyms"] = map[string]interface{}{"radius_m": radius, "total": total, "rows": rows}
				sources = append(sources, "bev_dlm_namen")
			}
		}
	}
	if want["legal"] {
		if p.KG == "" {
			out["legal"] = nil
			missing["legal"] = "legal needs ?kg= (KG code from your cadastre source); the point is never resolved to a KG here"
		} else {
			loadLegalRefs()
			refs := filterLegalRefs(legalRefsByKG[p.KG], q.Get("context"), q.Get("type"), q.Get("law"))
			out["legal"] = map[string]interface{}{"kg_code": p.KG, "total_refs": len(refs), "refs": legalRefsToMaps(refs), "details": "/api/v1/legal/kg/" + p.KG}
			sources = append(sources, "ris")
		}
	}

	if want["ne"] {
		if blk, reason := neContextBlock(p.Lon, p.Lat, !isPrivateTier(r)); reason != "" {
			out["ne"] = nil
			missing["ne"] = reason
		} else {
			out["ne"] = blk
			sources = append(sources, "ne")
		}
	}

	meta := NewMeta(ac, 1)
	meta.License = AttributionMetaFor("/api/v1/context", withOSM)
	out["sources"] = sources
	if len(missing) > 0 {
		out["missing"] = missing
	}
	apiResponse(w, r, out, meta)
}

// RegisterContextAPI — /api/v1/context + /api/v1/land_prices/point.
func RegisterContextAPI(mux *http.ServeMux) {
	mux.Handle("/api/v1/context", apiMiddleware(handleV1Context))
	mux.Handle("/api/v1/land_prices/point", apiMiddleware(handleLandPricePoint))
}

// ── Public-tier area prices: Statistik Austria series only ───────────────────
//
// The private /land_prices/{gemeinde,bezirk,bundesland} handlers aggregate the
// parcel index (per-class parcel counts, areas, values). On the public tier
// the same paths return ONLY the published Statistik Austria series for that
// area (every year we hold, plus the fitted trend) — no parcel is touched.

func yearMapOut(m yearMap) map[string]float64 {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]float64, len(m))
	for y, v := range m {
		out[strconv.Itoa(y)] = v
	}
	return out
}

func cagr(t float64) interface{} {
	if t == 0 {
		return nil
	}
	return math.Round((math.Exp(t)-1)*10000) / 10000
}

// landPriceStatistikOnly answers /land_prices/{level}/{code} without the index.
// Returns false when the level/code is unknown (caller 404s).
func landPriceStatistikOnly(w http.ResponseWriter, r *http.Request, level, code string) {
	lp, _ := LoadLandPrices()
	if lp == nil {
		apiError(w, "land_prices data not available", http.StatusServiceUnavailable)
		return
	}
	year := parseYearParam(r, 2024)
	resp := map[string]interface{}{
		"level": level, "code": code, "year": year, "tier": "public",
		"source": "Statistik Austria (published series); no parcel aggregation on the public tier — pass your own parcel features to /api/v1/land_prices/point or /predict",
	}
	sa := map[string]interface{}{}
	series := map[string]interface{}{}
	found := false
	switch level {
	case "gemeinde":
		if m := lp.BaulandByGemeinde[code]; len(m) > 0 {
			found = true
			v, yr := lookup(m, year)
			sa["bauland_eur_per_sqm"], sa["bauland_year"] = v, yr
			series["bauland_eur_per_sqm"] = yearMapOut(m)
			resp["bauland_cagr"] = cagr(lp.BaulandTrendGemeinde[code])
		}
		loadStatAustriaMunicipalities()
		for i := range statAustriaMunicipalities {
			if statAustriaMunicipalities[i].GemeindeCode == code {
				found = true
				resp["name"] = statAustriaMunicipalities[i].Name
				d, s := gemeindeCodeParts(code)
				resp["district_code"], resp["district_name"], resp["state"] = d, districtName(d), s
				break
			}
		}
		if len(code) >= 3 {
			resp["bezirk"] = "/api/v1/land_prices/bezirk/" + code[:3]
		}
	case "bezirk":
		for key, m := range map[string]yearMap{"bauland_eur_per_sqm": lp.BaulandByBezirk[code], "haus_eur_per_sqm": lp.HausByBezirk[code], "wohnung_eur_per_sqm": lp.WohnungByBezirk[code]} {
			if len(m) == 0 {
				continue
			}
			found = true
			v, yr := lookup(m, year)
			sa[key], sa[strings.TrimSuffix(key, "_eur_per_sqm")+"_year"] = v, yr
			series[key] = yearMapOut(m)
		}
		if p := lp.HausByBezirkPeriod[code]; len(p) > 0 {
			found = true
			series["haus_eur_per_sqm_by_period"] = p
		}
		resp["district_name"] = districtName(code)
		if len(code) >= 1 {
			resp["bundesland"] = "/api/v1/land_prices/bundesland/" + code[:1]
		}
	case "bundesland":
		for key, m := range map[string]yearMap{"ackerland_eur_per_sqm": lp.AckerlandByState[code], "gruenland_eur_per_sqm": lp.GruenlandByState[code], "ackerland_rent_eur_per_ha_year": lp.AckerlandRentBL[code], "gruenland_rent_eur_per_ha_year": lp.GruenlandRentBL[code]} {
			if len(m) == 0 {
				continue
			}
			found = true
			v, yr := lookup(m, year)
			sa[key], sa[key+"_year"] = v, yr
			series[key] = yearMapOut(m)
		}
		resp["name"] = bundeslandNameByCode[code]
		resp["trends_cagr"] = map[string]interface{}{
			"bauland_cagr":        cagr(lp.BaulandTrendState[code]),
			"ackerland_buy_cagr":  cagr(lp.AckerlandBuyTrend[code]),
			"gruenland_buy_cagr":  cagr(lp.GruenlandBuyTrend[code]),
			"ackerland_rent_cagr": cagr(lp.AckerlandRentTrend[code]),
			"gruenland_rent_cagr": cagr(lp.GruenlandRentTrend[code]),
		}
	}
	if !found {
		apiError(w, level+" "+code+" not found in the Statistik Austria series", http.StatusNotFound)
		return
	}
	resp["statistik_austria"] = sa
	resp["series"] = series
	resp["meta"] = map[string]interface{}{"license": AttributionMapFor("/api/v1/land_prices/", false)}
	writeJSON(w, http.StatusOK, resp)
}
