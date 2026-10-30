package main

// land_prices_aggregate.go — Bezirk/Bundesland aggregates and batch prediction.
//
// These handlers are pure read-paths over the existing search index + the
// in-memory Statistik-Austria tables loaded by land_prices.go. No index
// rebuild is required when refreshing the SA data — just regenerate
// data/statistik_austria/parsed/land_prices.json and restart.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
)

// classBucket accumulates per-class statistics for an aggregate (Gemeinde,
// Bezirk, Bundesland, …). Identical semantics to the inline struct in
// handleLandPriceGemeinde — kept here so multiple aggregators reuse it.
type classBucket struct {
	N          int
	Area       float64
	BuySum     float64
	BuyLogSum  float64
	BuyCount   int
	RentTotal  float64
	ValueTotal float64
}

func (b *classBucket) add(est *LandPriceEstimate) {
	if est == nil {
		return
	}
	b.N++
	b.Area += est.AreaSqm
	if est.BuyEurPerSqm != nil && *est.BuyEurPerSqm > 0 {
		b.BuySum += *est.BuyEurPerSqm
		b.BuyLogSum += math.Log(*est.BuyEurPerSqm)
		b.BuyCount++
	}
	if est.BuyTotalEur != nil {
		b.ValueTotal += *est.BuyTotalEur
	}
	if est.RentEurPerYear != nil {
		b.RentTotal += *est.RentEurPerYear
	}
}

func serializeBuckets(buckets map[LandClass]*classBucket) map[string]any {
	order := []LandClass{ClassBaulandBuilt, ClassBaulandZoned, ClassAckerland,
		ClassGruenland, ClassWald, ClassOther}
	out := map[string]any{}
	for _, c := range order {
		b, ok := buckets[c]
		if !ok {
			continue
		}
		entry := map[string]any{
			"parcels":         b.N,
			"total_area_sqm":  b.Area,
			"total_value_eur": b.ValueTotal,
		}
		if b.BuyCount > 0 {
			entry["mean_buy_eur_per_sqm"] = b.BuySum / float64(b.BuyCount)
			entry["geomean_buy_eur_per_sqm"] = math.Exp(b.BuyLogSum / float64(b.BuyCount))
		}
		if b.RentTotal > 0 {
			entry["annual_rent_eur"] = b.RentTotal
		}
		out[string(c)] = entry
	}
	return out
}

// aggregateParcels iterates a parcel result-set and feeds it through the
// pricing model. The caller passes a prepared SELECT yielding the standard
// pricing input columns in the canonical order used elsewhere in this file.
func aggregateParcels(rows *sql.Rows, year int) map[LandClass]*classBucket {
	buckets := map[LandClass]*classBucket{}
	get := func(c LandClass) *classBucket {
		if b, ok := buckets[c]; ok {
			return b
		}
		b := &classBucket{}
		buckets[c] = b
		return b
	}
	for rows.Next() {
		in := ParcelPricingInput{}
		if err := scanParcelPricingRow(rows, &in); err != nil {
			continue
		}
		est := PredictLandPrice(in, year)
		if est == nil {
			continue
		}
		get(est.Class).add(est)
	}
	return buckets
}

// ── /api/v1/land_prices/bezirk/{district_code} ────────────────────────────

// District codes in the cadastre index are 3 digits (e.g. "305" = Amstetten),
// matching the SA Bezirk Nummer used in the Sonderauswertung ODS.
func handleLandPriceBezirk(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/land_prices/bezirk/"), "/")
	if len(parts) < 1 || parts[0] == "" {
		apiError(w, "missing district_code", http.StatusBadRequest)
		return
	}
	code := parts[0]
	if !isPrivateTier(r) { // public tier: Statistik Austria series only (api_context.go)
		landPriceStatistikOnly(w, r, "bezirk", code)
		return
	}
	year := parseYearParam(r, 2024)
	lp, _ := LoadLandPrices()
	if lp == nil {
		apiError(w, "land_prices data not available", http.StatusServiceUnavailable)
		return
	}
	db := globalSearchIndex.db
	if db == nil {
		apiError(w, "index not initialised", http.StatusServiceUnavailable)
		return
	}

	// Bezirk metadata + sub-gemeinden
	var districtName, stateCode, stateName string
	db.QueryRow(`SELECT district_name, state_code, state_name FROM kg
	              WHERE district_code = ? LIMIT 1`, code).
		Scan(&districtName, &stateCode, &stateName)

	gemRows, _ := db.Query(`SELECT gemeinde_code, gemeinde_name,
		COUNT(*) AS kg_count, SUM(parcel_count) AS parcels
		FROM kg WHERE district_code = ?
		GROUP BY gemeinde_code ORDER BY gemeinde_name`, code)
	type gemRow struct {
		Code    string `json:"gemeinde_code"`
		Name    string `json:"gemeinde_name"`
		KGs     int    `json:"kg_count"`
		Parcels int    `json:"parcel_count"`
	}
	var gemList []gemRow
	if gemRows != nil {
		for gemRows.Next() {
			g := gemRow{}
			gemRows.Scan(&g.Code, &g.Name, &g.KGs, &g.Parcels)
			gemList = append(gemList, g)
		}
		gemRows.Close()
	}
	if len(gemList) == 0 {
		apiError(w, fmt.Sprintf("district %s not found in index", code), http.StatusNotFound)
		return
	}

	// Per-class aggregation over all parcels in the Bezirk
	includeAgg := !(strings.EqualFold(r.URL.Query().Get("aggregate"), "false"))
	var buckets map[LandClass]*classBucket
	if includeAgg {
		rows, err := db.Query(parcelPricingSelectSQL+` WHERE k.district_code = ?`, code)
		if err != nil {
			apiError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		buckets = aggregateParcels(rows, year)
		rows.Close()
	}

	// SA published values for this Bezirk
	sa := map[string]any{}
	if v, yr := lookup(lp.BaulandByBezirk[code], year); v > 0 {
		sa["bauland_eur_per_sqm"] = v
		sa["bauland_year"] = yr
	}
	if v, yr := lookup(lp.HausByBezirk[code], year); v > 0 {
		sa["haus_eur_per_sqm"] = v
		sa["haus_year"] = yr
	}
	if v, yr := lookup(lp.WohnungByBezirk[code], year); v > 0 {
		sa["wohnung_eur_per_sqm"] = v
		sa["wohnung_year"] = yr
	}

	resp := map[string]any{
		"district_code":     code,
		"district_name":     districtName,
		"state_code":        stateCode,
		"state_name":        stateName,
		"year":              year,
		"gemeinden":         gemList,
		"statistik_austria": sa,
	}
	if includeAgg {
		resp["by_class"] = serializeBuckets(buckets)
		// Validation: predicted Bauland geomean vs SA Bezirk
		if saB, ok := sa["bauland_eur_per_sqm"].(float64); ok {
			for _, c := range []LandClass{ClassBaulandBuilt, ClassBaulandZoned} {
				if b, ok := buckets[c]; ok && b.BuyCount > 0 {
					pred := math.Exp(b.BuyLogSum / float64(b.BuyCount))
					resp["validation"] = map[string]any{
						"sa_bauland_eur_per_sqm":        saB,
						"predicted_geomean_eur_per_sqm": pred,
						"predicted_minus_sa_pct":        100 * (pred - saB) / saB,
					}
					break
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// ── /api/v1/land_prices/bundesland/{state_code} ───────────────────────────

var bundeslandNameByCode = map[string]string{
	"1": "Burgenland", "2": "Kärnten", "3": "Niederösterreich",
	"4": "Oberösterreich", "5": "Salzburg", "6": "Steiermark",
	"7": "Tirol", "8": "Vorarlberg", "9": "Wien",
	"AT": "Österreich",
}

func handleLandPriceBundesland(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/land_prices/bundesland/"), "/")
	if len(parts) < 1 || parts[0] == "" {
		apiError(w, "missing state_code (1–9, or AT)", http.StatusBadRequest)
		return
	}
	code := parts[0]
	// Accept Bundesland names too (case-insensitive)
	if _, ok := bundeslandNameByCode[code]; !ok {
		for k, v := range bundeslandNameByCode {
			if strings.EqualFold(v, code) {
				code = k
				break
			}
		}
	}
	if !isPrivateTier(r) { // public tier: Statistik Austria series only (api_context.go)
		landPriceStatistikOnly(w, r, "bundesland", code)
		return
	}
	year := parseYearParam(r, 2024)
	lp, _ := LoadLandPrices()
	if lp == nil {
		apiError(w, "land_prices data not available", http.StatusServiceUnavailable)
		return
	}
	db := globalSearchIndex.db
	if db == nil {
		apiError(w, "index not initialised", http.StatusServiceUnavailable)
		return
	}

	// Resolve display name
	name := bundeslandNameByCode[code]
	if code != "AT" {
		db.QueryRow(`SELECT state_name FROM kg WHERE state_code = ? LIMIT 1`, code).Scan(&name)
	}

	// Per-class aggregation (heavy — gated behind ?aggregate=true)
	includeAgg := strings.EqualFold(r.URL.Query().Get("aggregate"), "true")
	var buckets map[LandClass]*classBucket
	var parcelCount int64
	if code != "AT" {
		db.QueryRow(`SELECT COALESCE(SUM(parcel_count),0) FROM kg WHERE state_code = ?`, code).Scan(&parcelCount)
	} else {
		db.QueryRow(`SELECT COALESCE(SUM(parcel_count),0) FROM kg`).Scan(&parcelCount)
	}
	if includeAgg && code != "AT" {
		rows, err := db.Query(parcelPricingSelectSQL+` WHERE k.state_code = ?`, code)
		if err != nil {
			apiError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		buckets = aggregateParcels(rows, year)
		rows.Close()
	}

	// Bauland state-mean from per-Gemeinde series (matches what the model
	// uses for the temporal CAGR fallback)
	stateMean := 0.0
	count := 0
	for g, ys := range lp.BaulandByGemeinde {
		if !strings.HasPrefix(g, code) && code != "AT" {
			continue
		}
		if v, _ := lookup(ys, year); v > 0 {
			stateMean += math.Log(v)
			count++
		}
	}
	if count > 0 {
		stateMean = math.Exp(stateMean / float64(count))
	} else {
		stateMean = 0
	}

	sa := map[string]any{}
	if stateMean > 0 {
		sa["bauland_geomean_eur_per_sqm"] = stateMean
		sa["bauland_geomean_basis"] = fmt.Sprintf("%d gemeinden", count)
	}
	if v, yr := lookup(lp.AckerlandByState[code], year); v > 0 {
		sa["ackerland_eur_per_sqm"] = v
		sa["ackerland_year"] = yr
	}
	if v, yr := lookup(lp.GruenlandByState[code], year); v > 0 {
		sa["gruenland_eur_per_sqm"] = v
		sa["gruenland_year"] = yr
	}
	if v, yr := lookup(lp.AckerlandRentBL[code], year); v > 0 {
		sa["ackerland_rent_eur_per_ha_year"] = v
		sa["ackerland_rent_year"] = yr
	}
	if v, yr := lookup(lp.GruenlandRentBL[code], year); v > 0 {
		sa["gruenland_rent_eur_per_ha_year"] = v
		sa["gruenland_rent_year"] = yr
	}

	trends := map[string]float64{}
	if t := lp.BaulandTrendState[code]; t != 0 {
		trends["bauland_cagr"] = math.Exp(t) - 1
	}
	if t := lp.AckerlandBuyTrend[code]; t != 0 {
		trends["ackerland_buy_cagr"] = math.Exp(t) - 1
	}
	if t := lp.GruenlandBuyTrend[code]; t != 0 {
		trends["gruenland_buy_cagr"] = math.Exp(t) - 1
	}
	if t := lp.AckerlandRentTrend[code]; t != 0 {
		trends["ackerland_rent_cagr"] = math.Exp(t) - 1
	}
	if t := lp.GruenlandRentTrend[code]; t != 0 {
		trends["gruenland_rent_cagr"] = math.Exp(t) - 1
	}

	resp := map[string]any{
		"state_code":        code,
		"state_name":        name,
		"year":              year,
		"parcel_count":      parcelCount,
		"statistik_austria": sa,
		"trends_cagr":       trends,
	}
	if includeAgg {
		resp["by_class"] = serializeBuckets(buckets)
	} else if code != "AT" {
		resp["hint"] = "add ?aggregate=true to compute per-class parcel aggregates (slower)"
	}
	writeJSON(w, http.StatusOK, resp)
}

// ── /api/v1/land_prices/batch ─────────────────────────────────────────────

// batchRequest accepts either parcel_ids (fast, DB-backed) or an array of
// ad-hoc pricing inputs (same fields as /predict). Both are size-limited
// to keep response times predictable.
type batchRequest struct {
	Year      int          `json:"year"`
	ParcelIDs []string     `json:"parcel_ids,omitempty"`
	Inputs    []batchInput `json:"inputs,omitempty"`
}

type batchInput struct {
	ID               string  `json:"id,omitempty"` // optional caller-supplied label
	GemeindeCode     string  `json:"gemeinde_code"`
	BezirkCode       string  `json:"bezirk_code,omitempty"`
	StateCode        string  `json:"state_code,omitempty"`
	AreaSqm          float64 `json:"area_sqm"`
	BuildingCount    int     `json:"building_count"`
	FootprintCt      int     `json:"footprint_count"`
	Compactness      float64 `json:"compactness"`
	LanduseCodes     string  `json:"landuse_codes,omitempty"`
	Typology         string  `json:"typology,omitempty"`
	FootprintAreaSqm float64 `json:"footprint_area_sqm,omitempty"`
	BuildingPeriod   string  `json:"building_period,omitempty"`
}

const batchMaxItems = 1000

func handleLandPriceBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		// Allow simple GET ?parcel_ids=a,b,c for shell-friendly use.
		q := r.URL.Query()
		ids := splitCSV(q.Get("parcel_ids"))
		year, _ := strconv.Atoi(q.Get("year"))
		runBatchParcels(w, ids, year)
		return
	}
	if r.Method != http.MethodPost {
		apiError(w, "use GET (?parcel_ids=) or POST (JSON body)", http.StatusMethodNotAllowed)
		return
	}
	var req batchRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&req); err != nil {
		apiError(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.ParcelIDs)+len(req.Inputs) > batchMaxItems {
		apiError(w, fmt.Sprintf("batch too large (max %d items)", batchMaxItems), http.StatusBadRequest)
		return
	}
	if len(req.ParcelIDs) > 0 && len(req.Inputs) > 0 {
		apiError(w, "send either parcel_ids OR inputs, not both", http.StatusBadRequest)
		return
	}
	if len(req.ParcelIDs) > 0 {
		runBatchParcels(w, req.ParcelIDs, req.Year)
		return
	}
	runBatchInputs(w, req.Inputs, req.Year)
}

func runBatchParcels(w http.ResponseWriter, ids []string, year int) {
	if len(ids) == 0 {
		apiError(w, "no parcel_ids supplied", http.StatusBadRequest)
		return
	}
	if len(ids) > batchMaxItems {
		apiError(w, fmt.Sprintf("batch too large (max %d items)", batchMaxItems), http.StatusBadRequest)
		return
	}
	if year == 0 {
		year = 2024
	}
	db := globalSearchIndex.db
	if db == nil {
		apiError(w, "index not initialised", http.StatusServiceUnavailable)
		return
	}

	// Build IN-clause; SQLite default SQLITE_MAX_VARIABLE_NUMBER is 32766.
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	query := parcelPricingSelectSQL + fmt.Sprintf(` WHERE p.parcel_id IN (%s)`, strings.Join(placeholders, ","))
	rows, err := db.Query(query, args...)
	if err != nil {
		apiError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	results := make(map[string]*LandPriceEstimate, len(ids))
	for rows.Next() {
		in := ParcelPricingInput{}
		if err := scanParcelPricingRow(rows, &in); err != nil {
			continue
		}
		results[in.ParcelID] = PredictLandPrice(in, year)
	}

	// Preserve caller order; mark misses explicitly so a downstream consumer
	// can distinguish "unknown parcel" from "unpriced class".
	ordered := make([]map[string]any, 0, len(ids))
	notFound := []string{}
	agg := classBucket{}
	for _, id := range ids {
		est, ok := results[id]
		if !ok {
			ordered = append(ordered, map[string]any{"parcel_id": id, "error": "not found"})
			notFound = append(notFound, id)
			continue
		}
		ordered = append(ordered, map[string]any{"parcel_id": id, "estimate": est})
		agg.add(est)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"year":      year,
		"requested": len(ids),
		"found":     len(ids) - len(notFound),
		"not_found": notFound,
		"results":   ordered,
		"summary":   summarizeBucket(&agg),
	})
}

func runBatchInputs(w http.ResponseWriter, inputs []batchInput, year int) {
	if len(inputs) == 0 {
		apiError(w, "no inputs supplied", http.StatusBadRequest)
		return
	}
	if year == 0 {
		year = 2024
	}
	results := make([]map[string]any, 0, len(inputs))
	agg := classBucket{}
	for i, bi := range inputs {
		in := ParcelPricingInput{
			GemeindeCode:     bi.GemeindeCode,
			BezirkCode:       bi.BezirkCode,
			StateCode:        bi.StateCode,
			AreaSqm:          bi.AreaSqm,
			BuildingCount:    bi.BuildingCount,
			FootprintCt:      bi.FootprintCt,
			Compactness:      bi.Compactness,
			LanduseCodes:     bi.LanduseCodes,
			Typology:         bi.Typology,
			FootprintAreaSqm: bi.FootprintAreaSqm,
			BuildingPeriod:   bi.BuildingPeriod,
		}
		if in.GemeindeCode != "" && in.StateCode == "" {
			in.StateCode = string(in.GemeindeCode[0])
		}
		id := bi.ID
		if id == "" {
			id = strconv.Itoa(i)
		}
		est := PredictLandPrice(in, year)
		results = append(results, map[string]any{"id": id, "estimate": est})
		agg.add(est)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"year":    year,
		"count":   len(inputs),
		"results": results,
		"summary": summarizeBucket(&agg),
	})
}

func summarizeBucket(b *classBucket) map[string]any {
	if b.N == 0 {
		return map[string]any{"parcels": 0}
	}
	out := map[string]any{
		"parcels":         b.N,
		"total_area_sqm":  b.Area,
		"total_value_eur": b.ValueTotal,
	}
	if b.BuyCount > 0 {
		out["mean_buy_eur_per_sqm"] = b.BuySum / float64(b.BuyCount)
		out["geomean_buy_eur_per_sqm"] = math.Exp(b.BuyLogSum / float64(b.BuyCount))
	}
	if b.RentTotal > 0 {
		out["annual_rent_eur"] = b.RentTotal
	}
	return out
}

var _ sql.NullFloat64 // imported indirectly via scanParcelPricingRow
