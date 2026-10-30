package main

// land_prices.go — Per-parcel land price/rent prediction calibrated against
// Statistik Austria (statistik.at) published averages.
//
// Data sources (committed under data/statistik_austria/):
//   • Baugrundstückspreise<YYYY>.ods   — Bauland (buildable land) Ø EUR/m² per Gemeinde, 2015–2024
//   • SonderauswertungImmobilien…ods   — Grund/Haus/Wohnung Ø EUR/m² per Bezirk, 2015–2024
//   • Bodenpreise/Pachtpreise *.csv    — Agricultural land buy + rent per Bundesland, 2022–2024
//
// Classification of a parcel ignores the noisy NS landuse code alone:
//   has buildings/footprints                            → bauland_built
//   any NS in BAU_NS (40-47, 91) and no buildings       → bauland_zoned
//   majority NS in ACK_NS (51, 62)                      → ackerland
//   majority NS in GRU_NS (48, 50, 52-55, 58, 61)           → gruenland
//   majority NS in WALD_NS (56, 57)                     → wald
//   else                                                → other (Gewässer, Fels, …)
//
// Per-parcel prediction:
//   Bauland: SA gemeinde baseline × calibration × f_size × f_shape × f_typology
//            The calibration constant is chosen so the geometric mean over the
//            local Bauland sample matches the SA gemeinde value. Empirical fit
//            (100k random parcels) achieves median predicted/SA = 0.94 and
//            88.7% of gemeinden within ±25%.
//   Agricultural: SA state-level Ackerland/Grünland EUR/m² (and EUR/ha/yr rent).
//                 Aggregated by state matches SA exactly by construction.
//   Wald: literature fallback per Bundesland (1–3 €/m²) — no SA source.
//
// Public API:
//   GetLandPrice(parcel, year) → *LandPriceEstimate
//   handleV1LandPrice           parcel-level endpoint
//   handleV1LandPriceGemeinde   aggregated municipality view + SA validation
//   handleV1LandPriceStats      class shares + agreement-with-SA report

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ── Data tables ──────────────────────────────────────────────────────────

type yearMap map[int]float64

type landPriceData struct {
	BaulandByGemeinde map[string]yearMap // gemeinde_code (5 digits) → year → EUR/m²
	BaulandByBezirk   map[string]yearMap // bezirk_code (3 digits)
	HausByBezirk      map[string]yearMap // house Ø EUR/m² (informational)
	WohnungByBezirk   map[string]yearMap // apartment Ø EUR/m² (informational)
	AckerlandByState  map[string]yearMap // state_code ("1"…"9", "AT") → year → EUR/m²
	GruenlandByState  map[string]yearMap
	AckerlandRentBL   map[string]yearMap // EUR/ha/year
	GruenlandRentBL   map[string]yearMap
	// Per-Bezirk × Bauperiode Häuserpreise (EUR/m² Wohnfläche).
	// Indexed by bezirk_code → year → period (pre1960|y1960_1990|post1991).
	HausByBezirkPeriod map[string]map[int]map[string]float64
	Meta               map[string]any

	// Log-linear trends (per-year growth rate) computed at load time.
	// trend == 0 means "no trend information — hold value flat".
	BaulandTrendGemeinde map[string]float64 // per gemeinde
	BaulandTrendState    map[string]float64 // per Bundesland (1–9, "AT")
	AckerlandBuyTrend    map[string]float64
	GruenlandBuyTrend    map[string]float64
	AckerlandRentTrend   map[string]float64
	GruenlandRentTrend   map[string]float64
	BaulandLatestYear    int
}

var (
	lpData *landPriceData
	lpOnce sync.Once
	lpErr  error
)

// LoadLandPrices parses the Statistik-Austria JSON bundle. Idempotent.
func LoadLandPrices() (*landPriceData, error) {
	lpOnce.Do(func() {
		lpData, lpErr = loadLandPricesFromDisk("data/statistik_austria/parsed/land_prices.json")
		if lpErr != nil {
			log.Printf("⚠️  land_prices: %v", lpErr)
			return
		}
		log.Printf("💶 land_prices loaded: %d gemeinden, %d bezirke, %d bezirke×period haus, agri %d states",
			len(lpData.BaulandByGemeinde), len(lpData.BaulandByBezirk),
			len(lpData.HausByBezirkPeriod), len(lpData.AckerlandByState))
	})
	return lpData, lpErr
}

func loadLandPricesFromDisk(path string) (*landPriceData, error) {
	abs, _ := filepath.Abs(path)
	f, err := os.Open(abs)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", abs, err)
	}
	defer f.Close()

	// raw → parsed structure (years are JSON strings)
	var raw struct {
		Bau     map[string]map[string]float64            `json:"bauland_eur_per_sqm_by_gemeinde"`
		Bau2    map[string]map[string]float64            `json:"bauland_eur_per_sqm_by_bezirk"`
		Haus    map[string]map[string]float64            `json:"haus_eur_per_sqm_by_bezirk"`
		HausP   map[string]map[string]map[string]float64 `json:"haus_eur_per_sqm_by_bezirk_period"`
		Whg     map[string]map[string]float64            `json:"wohnung_eur_per_sqm_by_bezirk"`
		AgriBuy map[string]map[string]map[string]float64 `json:"agri_eur_per_sqm_by_state"`
		AgriRnt map[string]map[string]map[string]float64 `json:"agri_rent_eur_per_ha_per_year_by_state"`
		Meta    map[string]any                           `json:"_meta"`
	}
	if err := json.NewDecoder(f).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode %s: %w", abs, err)
	}

	conv := func(in map[string]map[string]float64) map[string]yearMap {
		out := make(map[string]yearMap, len(in))
		for k, ys := range in {
			m := yearMap{}
			for y, v := range ys {
				if iy, err := strconv.Atoi(y); err == nil {
					m[iy] = v
				}
			}
			out[k] = m
		}
		return out
	}

	// Convert haus-by-bezirk-period: keys are bezirk_code → yearStr → periodStr → EUR/m².
	hausP := make(map[string]map[int]map[string]float64, len(raw.HausP))
	for bez, ys := range raw.HausP {
		inner := map[int]map[string]float64{}
		for y, periods := range ys {
			iy, err := strconv.Atoi(y)
			if err != nil {
				continue
			}
			inner[iy] = periods
		}
		hausP[bez] = inner
	}

	lp := &landPriceData{
		BaulandByGemeinde:  conv(raw.Bau),
		BaulandByBezirk:    conv(raw.Bau2),
		HausByBezirk:       conv(raw.Haus),
		HausByBezirkPeriod: hausP,
		WohnungByBezirk:    conv(raw.Whg),
		AckerlandByState:   conv(raw.AgriBuy["ackerland"]),
		GruenlandByState:   conv(raw.AgriBuy["gruenland"]),
		AckerlandRentBL:    conv(raw.AgriRnt["ackerland"]),
		GruenlandRentBL:    conv(raw.AgriRnt["gruenland"]),
		Meta:               raw.Meta,
	}
	lp.computeTrends()
	return lp, nil
}

// logLinearTrend returns the slope b of log(price) = a + b·year, i.e. the
// continuous-time growth rate. Needs at least 2 points; returns 0 otherwise.
func logLinearTrend(d yearMap) float64 {
	if len(d) < 2 {
		return 0
	}
	var n, sx, sy, sxx, sxy float64
	for y, v := range d {
		if v <= 0 {
			continue
		}
		lx := float64(y)
		ly := math.Log(v)
		n++
		sx += lx
		sy += ly
		sxx += lx * lx
		sxy += lx * ly
	}
	denom := n*sxx - sx*sx
	if n < 2 || denom == 0 {
		return 0
	}
	return (n*sxy - sx*sy) / denom // slope in log-space ≈ CAGR (small rates)
}

// computeTrends pre-computes log-linear growth rates per gemeinde/state and
// state-level fallbacks. Per-gemeinde trends are clipped to keep noisy
// short-series outliers from blowing up extrapolation.
func (lp *landPriceData) computeTrends() {
	lp.BaulandTrendGemeinde = make(map[string]float64, len(lp.BaulandByGemeinde))
	lp.BaulandTrendState = make(map[string]float64, 10)

	// State-level Bauland trend (geometric mean of gemeinden inside the state)
	type acc struct {
		logsum [16]float64
		count  [16]int
	}
	stateAcc := map[string]*acc{}
	latest := 0
	yearIdx := func(y int) int { return y - 2015 }

	for g, ys := range lp.BaulandByGemeinde {
		lp.BaulandTrendGemeinde[g] = clipTrend(logLinearTrend(ys), -0.04, 0.18)
		if len(g) >= 1 {
			st := g[:1]
			a, ok := stateAcc[st]
			if !ok {
				a = &acc{}
				stateAcc[st] = a
			}
			for y, v := range ys {
				if y < 2015 || y > 2030 || v <= 0 {
					continue
				}
				i := yearIdx(y)
				if i < 0 || i >= len(a.logsum) {
					continue
				}
				a.logsum[i] += math.Log(v)
				a.count[i]++
				if y > latest {
					latest = y
				}
			}
		}
	}
	lp.BaulandLatestYear = latest
	for st, a := range stateAcc {
		series := yearMap{}
		for i := range a.logsum {
			if a.count[i] > 0 {
				series[2015+i] = math.Exp(a.logsum[i] / float64(a.count[i]))
			}
		}
		lp.BaulandTrendState[st] = clipTrend(logLinearTrend(series), -0.03, 0.12)
	}
	// National fallback
	if _, ok := lp.BaulandTrendState["AT"]; !ok {
		all := yearMap{}
		type ac struct {
			sum float64
			n   int
		}
		acc := map[int]*ac{}
		for _, ys := range lp.BaulandByGemeinde {
			for y, v := range ys {
				if v <= 0 {
					continue
				}
				x, ok := acc[y]
				if !ok {
					x = &ac{}
					acc[y] = x
				}
				x.sum += math.Log(v)
				x.n++
			}
		}
		for y, x := range acc {
			if x.n > 0 {
				all[y] = math.Exp(x.sum / float64(x.n))
			}
		}
		lp.BaulandTrendState["AT"] = clipTrend(logLinearTrend(all), -0.03, 0.12)
	}

	// Agricultural trends (short series, but still useful)
	lp.AckerlandBuyTrend = trendsByState(lp.AckerlandByState, -0.10, 0.20)
	lp.GruenlandBuyTrend = trendsByState(lp.GruenlandByState, -0.10, 0.20)
	lp.AckerlandRentTrend = trendsByState(lp.AckerlandRentBL, -0.10, 0.20)
	lp.GruenlandRentTrend = trendsByState(lp.GruenlandRentBL, -0.10, 0.20)
}

func trendsByState(in map[string]yearMap, lo, hi float64) map[string]float64 {
	out := make(map[string]float64, len(in))
	for k, v := range in {
		out[k] = clipTrend(logLinearTrend(v), lo, hi)
	}
	return out
}

func clipTrend(x, lo, hi float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return 0
	}
	if x < lo {
		return lo
	}
	if x > hi {
		return hi
	}
	return x
}

// lookupWithTrend resolves the value of `d` at `year`.
//   - exact hit          → return as-is
//   - gap inside series  → log-linear interpolation between bracketing points
//   - outside series     → extrapolate from nearest point along `trend`
//     (per-year log slope; 0 keeps the value flat)
//
// The second return value is the source year used (0 = derived from trend).
func lookupWithTrend(d yearMap, year int, trend float64) (float64, int) {
	if d == nil || len(d) == 0 {
		return 0, 0
	}
	if v, ok := d[year]; ok && v > 0 {
		return v, year
	}
	// Find nearest earlier and later points.
	prevY, nextY := -1, -1
	for y, v := range d {
		if v <= 0 {
			continue
		}
		if y <= year && y > prevY {
			prevY = y
		}
		if y >= year && (nextY == -1 || y < nextY) {
			nextY = y
		}
	}
	if prevY != -1 && nextY != -1 && prevY != nextY {
		// log-linear interpolation
		la, lb := math.Log(d[prevY]), math.Log(d[nextY])
		t := float64(year-prevY) / float64(nextY-prevY)
		return math.Exp(la + t*(lb-la)), year
	}
	// Extrapolate from whichever endpoint we have.
	anchor := prevY
	if anchor == -1 {
		anchor = nextY
	}
	if anchor != -1 {
		return d[anchor] * math.Exp(trend*float64(year-anchor)), anchor
	}
	return 0, 0
}

// lookup keeps the old call-site signature (no trend information).
func lookup(d yearMap, year int) (float64, int) { return lookupWithTrend(d, year, 0) }

// ── NS code → land class ──────────────────────────────────────────────────

// NS code → pricing class, using the CORRECTED BEV code table
// (bev_landuse_codes.go). The previous sets were built on the invented
// table and were therefore wrong in both directions: 48 ("Äcker, Wiesen oder
// Weiden", the most common code in Austria) was believed to be Verkehrsfläche,
// while 83 ("Gebäudenebenflächen", a genuine Baufläche) was believed to be
// Fels/Geröll. See feedback #11.
var (
	// Bauflächen (BEV Benützungsart "Bauflächen") + explicitly zoned building land.
	nsBauland = map[string]struct{}{"41": {}, "83": {}}
	// Äcker / Dauerkulturen.
	nsAckerland = map[string]struct{}{"40": {}, "48": {}}
	// Grünland-ish agricultural and open land priced off the agricultural table.
	nsGruenland = map[string]struct{}{"52": {}, "53": {}, "54": {}, "57": {}}
	// Wald (BEV Benützungsart "Wald").
	nsWald = map[string]struct{}{"55": {}, "56": {}, "58": {}}
	// Codes that carry no meaningful land value of their own (roads, rail,
	// water, rock, glacier, industrial yards, cemeteries, leisure).
	nsOther = map[string]struct{}{
		"42": {}, "59": {}, "60": {}, "61": {}, "62": {}, "63": {}, "64": {},
		"65": {}, "72": {}, "84": {}, "87": {}, "88": {}, "92": {}, "95": {}, "96": {},
	}
)

// classifyParcelByArea derives the class from the AREA-dominant Benützungsart
// rather than from symbol presence. This is the fix for feedback #11: symbol
// presence is not area dominance, so three stray glyphs on a 17.9 ha field made
// it `bauland_built` at 19.10 EUR/m² — a 3.4 m EUR error reported with
// confidence="high".
//
// Buildings still force bauland_built, but only when they are a material part
// of the parcel: a single barn on 17 ha of farmland must not reprice the field.
// The threshold is expressed against the clipped Baufläche share.
func classifyParcelByArea(buildings, footprints int, split *LanduseAreaSplit) (LandClass, string, float64) {
	if split == nil || split.CoveredSqm <= 0 {
		return "", "", 0
	}
	byClass := map[LandClass]float64{}
	for code, a := range split.Areas {
		switch {
		case setHas(nsBauland, code):
			byClass[ClassBaulandZoned] += a
		case setHas(nsAckerland, code):
			byClass[ClassAckerland] += a
		case setHas(nsGruenland, code):
			byClass[ClassGruenland] += a
		case setHas(nsWald, code):
			byClass[ClassWald] += a
		default:
			byClass[ClassOther] += a
		}
	}
	best, bestA := ClassOther, 0.0
	for c, a := range byClass {
		if a > bestA || (a == bestA && c < best) {
			best, bestA = c, a
		}
	}
	share := bestA / split.CoveredSqm
	baulandShare := byClass[ClassBaulandZoned] / split.CoveredSqm
	if (buildings > 0 || footprints > 0) && baulandShare >= baulandBuiltMinShare {
		return ClassBaulandBuilt, "area", byClass[ClassBaulandZoned] / split.CoveredSqm
	}
	if best == ClassBaulandZoned && (buildings > 0 || footprints > 0) {
		return ClassBaulandBuilt, "area", share
	}
	return best, "area", share
}

// baulandBuiltMinShare is how much of a parcel's mapped area must be Baufläche
// before the presence of a building reclassifies the WHOLE parcel as
// bauland_built. 63334-235 has 910 m² Baufläche on 179 116 m² (0.5 %).
const baulandBuiltMinShare = 0.25

func setHas(m map[string]struct{}, k string) bool {
	_, ok := m[k]
	return ok
}

// LandClass is the predicted parcel class for pricing purposes.
type LandClass string

const (
	ClassBaulandBuilt LandClass = "bauland_built"
	ClassBaulandZoned LandClass = "bauland_zoned"
	ClassAckerland    LandClass = "ackerland"
	ClassGruenland    LandClass = "gruenland"
	ClassWald         LandClass = "wald"
	ClassOther        LandClass = "other"
)

func classifyParcel(buildings, footprints int, landuseCodes string) (LandClass, []string) {
	codes := splitCSV(landuseCodes)
	if buildings > 0 || footprints > 0 {
		return ClassBaulandBuilt, codes
	}
	if len(codes) == 0 {
		return ClassOther, codes
	}
	freq := map[string]int{}
	for _, c := range codes {
		freq[c]++
	}
	// any explicit Bauland code beats other claims
	for c := range freq {
		if _, ok := nsBauland[c]; ok {
			return ClassBaulandZoned, codes
		}
	}
	top := mostFrequent(freq)
	if _, ok := nsAckerland[top]; ok {
		return ClassAckerland, codes
	}
	if _, ok := nsGruenland[top]; ok {
		return ClassGruenland, codes
	}
	if _, ok := nsWald[top]; ok {
		return ClassWald, codes
	}
	if _, has50 := freq["50"]; has50 {
		return ClassGruenland, codes
	}
	return ClassOther, codes
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func mostFrequent(m map[string]int) string {
	best := ""
	bn := 0
	for k, n := range m {
		if n > bn || (n == bn && k < best) {
			best, bn = k, n
		}
	}
	return best
}

// ── Pricing factors ───────────────────────────────────────────────────────

// CalibrationBauland was fitted on a 100k-parcel random sample: it brings the
// geometric mean of f_size·f_shape·f_typology back to 1 so per-gemeinde geom
// means agree with SA. See data/statistik_austria/parsed/calibration.md.
const CalibrationBauland = 1.096

func fSize(areaSqm float64) float64 {
	if areaSqm <= 0 {
		return 1.0
	}
	// Smaller plots fetch a per-m² premium; reference = 600 m² residential lot.
	f := math.Pow(600.0/areaSqm, 0.18)
	if f < 0.5 {
		return 0.5
	}
	if f > 1.6 {
		return 1.6
	}
	return f
}

func fShape(compactness float64) float64 {
	if compactness <= 0 {
		return 0.9
	}
	if compactness > 1 {
		compactness = 1
	}
	return 0.85 + 0.30*compactness
}

func fTypology(typ string) float64 {
	switch typ {
	case "urban":
		return 1.30
	case "suburban":
		return 1.08
	case "rural_settled":
		return 0.92
	case "rural_agrarian":
		return 0.78
	}
	return 1.0
}

// Wald fallback (EUR/m², per Bundesland) – literature estimate; SA does not publish.
var waldByState = map[string]float64{
	"1": 0.6, "2": 1.0, "3": 0.8, "4": 1.2, "5": 1.5,
	"6": 1.0, "7": 2.0, "8": 2.5, "9": 3.0,
}

// ── Prediction ───────────────────────────────────────────────────────────

// LandPriceEstimate is the unified result for one parcel.
type LandPriceEstimate struct {
	ParcelID          string             `json:"parcel_id"`
	GemeindeCode      string             `json:"gemeinde_code"`
	BezirkCode        string             `json:"bezirk_code,omitempty"`
	StateCode         string             `json:"state_code,omitempty"`
	Year              int                `json:"year"`
	Class             LandClass          `json:"class"`
	AreaSqm           float64            `json:"area_sqm"`
	BuyEurPerSqm      *float64           `json:"buy_eur_per_sqm,omitempty"`
	BuyTotalEur       *float64           `json:"buy_total_eur,omitempty"`
	RentEurPerHaYear  *float64           `json:"rent_eur_per_ha_year,omitempty"`
	RentEurPerYear    *float64           `json:"rent_eur_per_year,omitempty"`
	BaselineEurPerSqm *float64           `json:"baseline_eur_per_sqm,omitempty"`
	BaselineSource    string             `json:"baseline_source,omitempty"`
	BaselineYear      int                `json:"baseline_year,omitempty"`
	Factors           map[string]float64 `json:"factors,omitempty"`
	Calibration       float64            `json:"calibration,omitempty"`
	Confidence        string             `json:"confidence,omitempty"`
	Notes             string             `json:"notes,omitempty"`
	Trends            map[string]float64 `json:"trends,omitempty"` // log-linear per-year rates

	// Building value estimate (only populated when footprint geometry is
	// available via FootprintAreaSqm). Independent of land value.
	Building *BuildingValueEstimate `json:"building,omitempty"`
	// CombinedValueEur = BuyTotalEur + Building.ValueEur when both are present.
	CombinedValueEur *float64 `json:"combined_value_eur,omitempty"`

	// ── Provenance of `class` (feedback #11) ──
	// ClassSource is "area" when the class came from the exact per-Benützungs-
	// art AREA split, "symbol" when it fell back to symbol presence (the KG's
	// landuse-area cache is not warm yet). A consumer MUST treat "symbol" as
	// advisory: symbol presence is not area dominance, and the failure mode is
	// one to two orders of magnitude in value, not a few percent.
	ClassSource string `json:"class_source,omitempty"`
	// ClassShare is the dominant class's fraction of the mapped area (area
	// source only). Low share ⇒ genuinely mixed parcel.
	ClassShare float64 `json:"class_share,omitempty"`
	// LanduseAreas is the per-NS-code m² split the class was derived from.
	LanduseAreas []map[string]interface{} `json:"landuse_areas,omitempty"`
	// BuyTotalBlendedEur prices each Benützungsart at its own baseline and sums
	// them, instead of applying the dominant class to the whole parcel. For
	// mixed parcels this is the defensible number.
	BuyTotalBlendedEur *float64 `json:"buy_total_blended_eur,omitempty"`
	// BlendedParts explains that sum.
	BlendedParts []map[string]interface{} `json:"blended_parts,omitempty"`
}

// BuildingValueEstimate is the per-parcel building value derived from total
// footprint area × Bezirk-level Statistik-Austria Häuserpreise (EUR/m² Wohn-
// fläche). Wohnfläche is approximated as footprint area × a typology-derived
// story multiplier; the multiplier is conservative and documented.
type BuildingValueEstimate struct {
	FootprintCount   int     `json:"footprint_count"`
	FootprintAreaSqm float64 `json:"footprint_area_sqm"`
	StoryFactor      float64 `json:"story_factor"`
	WohnflaecheSqm   float64 `json:"wohnflaeche_sqm"`
	EurPerSqm        float64 `json:"eur_per_sqm"`
	ValueEur         float64 `json:"value_eur"`
	BezirkCode       string  `json:"bezirk_code"`
	BaselineSource   string  `json:"baseline_source"` // "bezirk_period" | "bezirk"
	BaselineYear     int     `json:"baseline_year"`
	Period           string  `json:"period,omitempty"` // pre1960|y1960_1990|post1991|mixed
	Confidence       string  `json:"confidence"`
	Notes            string  `json:"notes,omitempty"`
}

// ParcelPricingInput aggregates the per-parcel fields needed for pricing.
type ParcelPricingInput struct {
	ParcelID      string
	GemeindeCode  string
	BezirkCode    string // 3 digits; derived from gemeinde when empty
	StateCode     string
	AreaSqm       float64
	BuildingCount int
	FootprintCt   int
	Compactness   float64
	LanduseCodes  string
	Typology      string
	// Optional building-value inputs. When FootprintAreaSqm > 0 the prediction
	// also returns Building{} populated from Bezirk Häuserpreise.
	FootprintAreaSqm float64
	BuildingPeriod   string // pre1960|y1960_1990|post1991 (optional)

	// Split is the caller-supplied per-Benützungsart area split (optional).
	Split *LanduseAreaSplit
}

// LanduseAreaSplit — per-Benützungsart area split of one unit, supplied by the
// caller (e.g. /api/v1/context?landuse_areas=). Areas are m² per NS code.
type LanduseAreaSplit struct {
	Areas        map[string]float64 `json:"areas_sqm"`
	CoveredSqm   float64            `json:"covered_sqm"`
	ParcelAreaM2 float64            `json:"area_sqm"`
	CoverageFrac float64            `json:"coverage"`
	DominantCode string             `json:"dominant_code"`
	DominantSqm  float64            `json:"dominant_sqm"`
	DominantFrac float64            `json:"dominant_share"`
	Source       string             `json:"source,omitempty"`
	GeomAreaSqm  float64            `json:"geom_area_sqm,omitempty"`
}

// Breakdown lists the split per NS code, largest first.
func (s *LanduseAreaSplit) Breakdown() []map[string]interface{} {
	if s == nil || len(s.Areas) == 0 {
		return nil
	}
	codes := make([]string, 0, len(s.Areas))
	for c := range s.Areas {
		codes = append(codes, c)
	}
	sort.Slice(codes, func(i, j int) bool {
		if s.Areas[codes[i]] != s.Areas[codes[j]] {
			return s.Areas[codes[i]] > s.Areas[codes[j]]
		}
		return codes[i] < codes[j]
	})
	out := make([]map[string]interface{}, 0, len(codes))
	for _, c := range codes {
		e := map[string]interface{}{
			"code":           c,
			"type":           MapNutzungssymbol(c),
			"abbr":           MapNutzungAbbreviation(c),
			"total_area_sqm": s.Areas[c],
		}
		if s.CoveredSqm > 0 {
			e["share"] = round4(s.Areas[c] / s.CoveredSqm)
		}
		out = append(out, e)
	}
	return out
}

// kgOfParcelID extracts the KG code from a parcel id ("63334-235" → "63334").
func kgOfParcelID(parcelID string) string {
	if i := strings.IndexByte(parcelID, '-'); i > 0 {
		return parcelID[:i]
	}
	return ""
}

// PredictLandPrice computes per-parcel value/rent for the given year.
// year=0 picks the latest available baseline year.
func PredictLandPrice(in ParcelPricingInput, year int) *LandPriceEstimate {
	lp, _ := LoadLandPrices()
	if lp == nil {
		return nil
	}
	if year == 0 {
		year = 2024
	}

	// Prefer the caller-supplied per-Benützungsart AREA split over symbol
	// presence (the split is only CONSULTED here, never computed).
	var cls LandClass
	classSource, classShare := "", 0.0
	split := in.Split
	if c, srcKind, share := classifyParcelByArea(in.BuildingCount, in.FootprintCt, split); c != "" {
		cls, classSource, classShare = c, srcKind, share
		if split != nil && split.Source != "" {
			classSource = classSource + ":" + split.Source
		}
	} else {
		cls, _ = classifyParcel(in.BuildingCount, in.FootprintCt, in.LanduseCodes)
		classSource = "symbol"
	}
	if in.BezirkCode == "" && len(in.GemeindeCode) >= 3 {
		in.BezirkCode = in.GemeindeCode[:3]
	}
	if in.StateCode == "" && len(in.GemeindeCode) >= 1 {
		in.StateCode = in.GemeindeCode[:1]
	}
	out := &LandPriceEstimate{
		ParcelID:     in.ParcelID,
		GemeindeCode: in.GemeindeCode,
		BezirkCode:   in.BezirkCode,
		StateCode:    in.StateCode,
		Year:         year,
		Class:        cls,
		AreaSqm:      in.AreaSqm,
		ClassSource:  classSource,
		ClassShare:   round4(classShare),
	}
	if split != nil {
		out.LanduseAreas = split.Breakdown()
	}

	switch cls {
	case ClassBaulandBuilt, ClassBaulandZoned:
		base, baseYr, src := lookupBaulandBaseline(lp, in.GemeindeCode, year)
		if base <= 0 {
			out.Confidence = "missing"
			return out
		}
		sz := fSize(in.AreaSqm)
		sh := fShape(in.Compactness)
		ty := fTypology(in.Typology)
		price := base * CalibrationBauland * sz * sh * ty
		out.BuyEurPerSqm = &price
		total := price * in.AreaSqm
		out.BuyTotalEur = &total
		out.BaselineEurPerSqm = &base
		out.BaselineSource = src
		out.BaselineYear = baseYr
		out.Calibration = CalibrationBauland
		out.Factors = map[string]float64{"size": sz, "shape": sh, "typology": ty}
		out.Confidence = confidenceLevel(src, cls)
		gemT := lp.BaulandTrendGemeinde[in.GemeindeCode]
		stT := lp.BaulandTrendState[in.StateCode]
		if gemT != 0 || stT != 0 {
			out.Trends = map[string]float64{
				"buy_cagr_gemeinde": math.Exp(gemT) - 1,
				"buy_cagr_state":    math.Exp(stT) - 1,
			}
		}
	case ClassAckerland, ClassGruenland:
		var (
			tbl       map[string]yearMap
			rentTbl   map[string]yearMap
			buyTrend  float64
			rentTrend float64
		)
		if cls == ClassAckerland {
			tbl, rentTbl = lp.AckerlandByState, lp.AckerlandRentBL
			buyTrend, rentTrend = lp.AckerlandBuyTrend[in.StateCode], lp.AckerlandRentTrend[in.StateCode]
			if buyTrend == 0 {
				buyTrend = lp.AckerlandBuyTrend["AT"]
			}
			if rentTrend == 0 {
				rentTrend = lp.AckerlandRentTrend["AT"]
			}
		} else {
			tbl, rentTbl = lp.GruenlandByState, lp.GruenlandRentBL
			buyTrend, rentTrend = lp.GruenlandBuyTrend[in.StateCode], lp.GruenlandRentTrend[in.StateCode]
			if buyTrend == 0 {
				buyTrend = lp.GruenlandBuyTrend["AT"]
			}
			if rentTrend == 0 {
				rentTrend = lp.GruenlandRentTrend["AT"]
			}
		}
		base, baseYr := lookupWithTrend(tbl[in.StateCode], year, buyTrend)
		src := "state"
		if base == 0 {
			base, baseYr = lookupWithTrend(tbl["AT"], year, buyTrend)
			src = "national"
		}
		if base > 0 {
			out.BuyEurPerSqm = &base
			tot := base * in.AreaSqm
			out.BuyTotalEur = &tot
			out.BaselineEurPerSqm = &base
			out.BaselineSource = src
			out.BaselineYear = baseYr
			out.Confidence = "state_mean"
		}
		if rent, ry := lookupWithTrend(rentTbl[in.StateCode], year, rentTrend); rent > 0 {
			out.RentEurPerHaYear = &rent
			perYr := rent * in.AreaSqm / 10000.0
			out.RentEurPerYear = &perYr
			if out.BaselineYear == 0 {
				out.BaselineYear = ry
			}
		} else if rent, ry := lookupWithTrend(rentTbl["AT"], year, rentTrend); rent > 0 {
			out.RentEurPerHaYear = &rent
			perYr := rent * in.AreaSqm / 10000.0
			out.RentEurPerYear = &perYr
			out.Notes = "rent missing for state — national fallback"
			if out.BaselineYear == 0 {
				out.BaselineYear = ry
			}
		}
		if buyTrend != 0 || rentTrend != 0 {
			out.Trends = map[string]float64{
				"buy_cagr":  math.Exp(buyTrend) - 1,
				"rent_cagr": math.Exp(rentTrend) - 1,
			}
		}
	case ClassWald:
		if v, ok := waldByState[in.StateCode]; ok {
			price := v
			out.BuyEurPerSqm = &price
			tot := v * in.AreaSqm
			out.BuyTotalEur = &tot
			out.BaselineSource = "literature_state"
			out.Confidence = "low"
			out.Notes = "Statistik Austria does not publish forest land prices — value is an indicative literature range"
		}
	default:
		out.Notes = "infrastructure / water / barren — no market price assigned"
	}

	// Blended value: price each Benützungsart at its own baseline and sum.
	// For a mixed parcel this is the defensible figure — applying the dominant
	// class to 100 % of the area is what produced the 3.4 m EUR error in
	// feedback #11 even after the class itself is right.
	if split != nil {
		blendLandPrice(lp, in, year, split, out)
	}
	// Honesty about the class: a symbol-derived class can be off by orders of
	// magnitude, so it must never be reported as high confidence.
	if classSource == "symbol" {
		if out.Confidence == "high" {
			out.Confidence = "medium"
		}
		out.Notes = strings.TrimSpace(out.Notes + " class derived from landuse SYMBOLS " +
			"(area split not yet computed for this KG) — symbol presence is not area dominance; " +
			"retry shortly for an area-derived class")
	} else if classShare > 0 && classShare < 0.6 {
		if out.Confidence == "high" {
			out.Confidence = "medium"
		}
		out.Notes = strings.TrimSpace(out.Notes + fmt.Sprintf(
			" mixed parcel: dominant class covers only %.0f%% of the mapped area — "+
				"prefer buy_total_blended_eur", classShare*100))
	}

	// Building value (independent of land class — a barn or chapel on "other"
	// land still has structural value worth estimating).
	if in.FootprintAreaSqm > 0 && in.BezirkCode != "" {
		out.Building = estimateBuildingValue(lp, in, year)
	}
	if out.BuyTotalEur != nil && out.Building != nil && out.Building.ValueEur > 0 {
		comb := *out.BuyTotalEur + out.Building.ValueEur
		out.CombinedValueEur = &comb
	} else if out.Building != nil && out.Building.ValueEur > 0 && out.BuyTotalEur == nil {
		comb := out.Building.ValueEur
		out.CombinedValueEur = &comb
	}
	return out
}

// blendLandPrice values each Benützungsart in the split at its own baseline and
// sums them into BuyTotalBlendedEur, with a per-part explanation.
//
// The per-m² baselines are the same ones the single-class path uses, so the
// blended total of a pure parcel equals its unblended total (verified in
// land_prices_area_test.go); the two only diverge for genuinely mixed parcels,
// which is the point.
func blendLandPrice(lp *landPriceData, in ParcelPricingInput, year int, split *LanduseAreaSplit, out *LandPriceEstimate) {
	if split.CoveredSqm <= 0 {
		return
	}
	baulandUnit := 0.0
	if base, _, _ := lookupBaulandBaseline(lp, in.GemeindeCode, year); base > 0 {
		baulandUnit = base * CalibrationBauland * fSize(in.AreaSqm) * fShape(in.Compactness) * fTypology(in.Typology)
	}
	ackerUnit, _ := lookupWithTrend(pickState(lp.AckerlandByState, lp.AckerlandByState["AT"], in.StateCode), year,
		trendOr(lp.AckerlandBuyTrend, in.StateCode))
	gruenUnit, _ := lookupWithTrend(pickState(lp.GruenlandByState, lp.GruenlandByState["AT"], in.StateCode), year,
		trendOr(lp.GruenlandBuyTrend, in.StateCode))
	waldUnit := waldByState[in.StateCode]

	total := 0.0
	var parts []map[string]interface{}
	// Scale the split up to the attribute area when the nfl layer does not
	// cover the whole parcel, so the blend values the parcel we were asked
	// about rather than only its mapped portion.
	scale := 1.0
	if split.CoveredSqm > 0 && in.AreaSqm > 0 {
		scale = in.AreaSqm / split.CoveredSqm
	}
	for code, rawArea := range split.Areas {
		area := rawArea * scale
		var unit float64
		var cls LandClass
		switch {
		case setHas(nsBauland, code):
			unit, cls = baulandUnit, ClassBaulandZoned
		case setHas(nsAckerland, code):
			unit, cls = ackerUnit, ClassAckerland
		case setHas(nsGruenland, code):
			unit, cls = gruenUnit, ClassGruenland
		case setHas(nsWald, code):
			unit, cls = waldUnit, ClassWald
		default:
			unit, cls = 0, ClassOther
		}
		v := unit * area
		total += v
		parts = append(parts, map[string]interface{}{
			"code":          code,
			"type":          MapNutzungssymbol(code),
			"benutzungsart": MapBenutzungsart(code),
			"class":         cls,
			"area_sqm":      round2(area),
			"eur_per_sqm":   round2(unit),
			"value_eur":     round2(v),
		})
	}
	if len(parts) == 0 {
		return
	}
	sort.Slice(parts, func(i, j int) bool {
		return parts[i]["value_eur"].(float64) > parts[j]["value_eur"].(float64)
	})
	t := round2(total)
	out.BuyTotalBlendedEur = &t
	out.BlendedParts = parts
}

func pickState(tbl map[string]yearMap, fallback yearMap, state string) yearMap {
	if m, ok := tbl[state]; ok {
		return m
	}
	return fallback
}

func trendOr(tbl map[string]float64, state string) float64 {
	if v := tbl[state]; v != 0 {
		return v
	}
	return tbl["AT"]
}

// storyFactorForTypology approximates the ratio of Wohnfläche (gross living
// area) to footprint area. Urban tenement blocks typically have 4–6 floors
// of living space per footprint m²; suburban single-family houses average
// ≈1.6 (ground + part-upper); rural buildings often have only one storey
// of habitable space (the rest is barn/Wirtschaftsraum and is priced lower
// per m² by SA already). These factors are deliberately conservative; the
// per-Bezirk Häuserpreis already amortises non-living m² to some extent.
func storyFactorForTypology(typ string) float64 {
	switch typ {
	case "urban":
		return 2.4
	case "suburban":
		return 1.7
	case "rural_settled":
		return 1.4
	case "rural_agrarian":
		return 1.2
	}
	return 1.5
}

// lookupHausPrice resolves Bezirk Häuserpreis (EUR/m² Wohnfläche). It first
// tries the Bezirk×Bauperiode table (more precise) and falls back to the
// Bezirk-only Sonderauswertung average. Year handling is identical to other
// SA lookups: exact → log-linear interpolation → nearest-year extrapolation.
func lookupHausPrice(lp *landPriceData, bezirk, period string, year int) (eur float64, src, periodUsed string, baseYr int) {
	if bezirk == "" {
		return 0, "", "", 0
	}
	if tbl, ok := lp.HausByBezirkPeriod[bezirk]; ok && len(tbl) > 0 {
		periods := map[string]yearMap{}
		for y, perVals := range tbl {
			for p, v := range perVals {
				if periods[p] == nil {
					periods[p] = yearMap{}
				}
				periods[p][y] = v
			}
		}
		if period != "" {
			if ys, ok := periods[period]; ok && len(ys) > 0 {
				if v, yr := lookup(ys, year); v > 0 {
					return v, "bezirk_period", period, yr
				}
			}
		}
		var logSum float64
		n := 0
		var usedYr int
		for _, ys := range periods {
			if v, yr := lookup(ys, year); v > 0 {
				logSum += math.Log(v)
				n++
				if yr > usedYr {
					usedYr = yr
				}
			}
		}
		if n > 0 {
			return math.Exp(logSum / float64(n)), "bezirk_period", "mixed", usedYr
		}
	}
	if v, yr := lookup(lp.HausByBezirk[bezirk], year); v > 0 {
		return v, "bezirk", "", yr
	}
	return 0, "", "", 0
}

func estimateBuildingValue(lp *landPriceData, in ParcelPricingInput, year int) *BuildingValueEstimate {
	eur, src, periodUsed, baseYr := lookupHausPrice(lp, in.BezirkCode, in.BuildingPeriod, year)
	if eur <= 0 {
		return &BuildingValueEstimate{
			FootprintCount:   in.FootprintCt,
			FootprintAreaSqm: in.FootprintAreaSqm,
			BezirkCode:       in.BezirkCode,
			Confidence:       "missing",
			Notes:            "no Statistik-Austria Häuserpreis available for this Bezirk",
		}
	}
	story := storyFactorForTypology(in.Typology)
	wohnflaeche := in.FootprintAreaSqm * story
	value := wohnflaeche * eur
	conf := "medium"
	if src == "bezirk_period" && periodUsed != "" && periodUsed != "mixed" {
		conf = "high"
	}
	notes := ""
	if periodUsed == "" || periodUsed == "mixed" {
		notes = "building period unknown — averaged across Bauperioden"
	}
	return &BuildingValueEstimate{
		FootprintCount:   in.FootprintCt,
		FootprintAreaSqm: in.FootprintAreaSqm,
		StoryFactor:      story,
		WohnflaecheSqm:   wohnflaeche,
		EurPerSqm:        eur,
		ValueEur:         value,
		BezirkCode:       in.BezirkCode,
		BaselineSource:   src,
		BaselineYear:     baseYr,
		Period:           periodUsed,
		Confidence:       conf,
		Notes:            notes,
	}
}

func confidenceLevel(src string, cls LandClass) string {
	if src == "gemeinde" {
		if cls == ClassBaulandBuilt {
			return "high"
		}
		return "medium"
	}
	return "low"
}

func lookupBaulandBaseline(lp *landPriceData, gemeinde string, year int) (float64, int, string) {
	state := ""
	if len(gemeinde) >= 1 {
		state = gemeinde[:1]
	}
	trend := lp.BaulandTrendGemeinde[gemeinde]
	if trend == 0 {
		trend = lp.BaulandTrendState[state]
	}
	if trend == 0 {
		trend = lp.BaulandTrendState["AT"]
	}
	if v, yr := lookupWithTrend(lp.BaulandByGemeinde[gemeinde], year, trend); v > 0 {
		return v, yr, "gemeinde"
	}
	if len(gemeinde) >= 3 {
		bezTrend := lp.BaulandTrendState[state]
		if bezTrend == 0 {
			bezTrend = lp.BaulandTrendState["AT"]
		}
		if v, yr := lookupWithTrend(lp.BaulandByBezirk[gemeinde[:3]], year, bezTrend); v > 0 {
			return v, yr, "bezirk"
		}
	}
	return 0, 0, ""
}

// ── HTTP handlers ────────────────────────────────────────────────────────

// RegisterLandPriceAPI installs the /api/v1/land_prices/* routes.
func RegisterLandPriceAPI(mux *http.ServeMux) {
	mux.Handle("/api/v1/land_prices/stats", apiMiddleware(handleLandPriceStats))
	mux.Handle("/api/v1/land_prices/gemeinde/", apiMiddleware(handleLandPriceGemeinde))
	mux.Handle("/api/v1/land_prices/bezirk/", apiMiddleware(handleLandPriceBezirk))
	mux.Handle("/api/v1/land_prices/bundesland/", apiMiddleware(handleLandPriceBundesland))
	mux.Handle("/api/v1/land_prices/parcel/", apiMiddleware(handleLandPriceParcel))
	mux.Handle("/api/v1/land_prices/predict", apiMiddleware(handleLandPricePredict))
	mux.Handle("/api/v1/land_prices/batch", apiMiddleware(handleLandPriceBatch))
	mux.Handle("/api/v1/land_prices/validate", apiMiddleware(handleLandPriceValidate))
	log.Println("💶 land_prices API: /api/v1/land_prices/{stats,gemeinde/<gnr>,bezirk/<bnr>,bundesland/<sc>,parcel/<id>,predict,batch,validate}")
}

func parseYearParam(r *http.Request, def int) int {
	if y, err := strconv.Atoi(r.URL.Query().Get("year")); err == nil && y >= 2015 && y <= 2030 {
		return y
	}
	return def
}

// GET /api/v1/land_prices/stats — class shares + agreement-with-SA report
func handleLandPriceStats(w http.ResponseWriter, r *http.Request) {
	lp, err := LoadLandPrices()
	if err != nil || lp == nil {
		apiError(w, "land_prices data not available", http.StatusServiceUnavailable)
		return
	}
	year := parseYearParam(r, 2024)
	db := globalSearchIndex.db
	if db == nil {
		apiError(w, "search index not initialised", http.StatusServiceUnavailable)
		return
	}

	// Aggregate class counts via SQL — landuse and building flags are indexed.
	row := db.QueryRow(`SELECT COUNT(*) FROM parcels`)
	var total int64
	_ = row.Scan(&total)

	// State-level Bauland CAGR (years come from log-linear slope)
	stateTrends := map[string]float64{}
	for s, t := range lp.BaulandTrendState {
		stateTrends[s] = math.Exp(t) - 1 // convert log slope → CAGR
	}
	agriTrends := map[string]map[string]float64{
		"ackerland_buy":  cagrMap(lp.AckerlandBuyTrend),
		"gruenland_buy":  cagrMap(lp.GruenlandBuyTrend),
		"ackerland_rent": cagrMap(lp.AckerlandRentTrend),
		"gruenland_rent": cagrMap(lp.GruenlandRentTrend),
	}
	resp := map[string]any{
		"year":                  year,
		"total_parcels":         total,
		"gemeinde_count":        len(lp.BaulandByGemeinde),
		"bezirk_count":          len(lp.BaulandByBezirk),
		"agri_state_count":      len(lp.AckerlandByState),
		"latest_source_year":    lp.BaulandLatestYear,
		"sources":               lp.Meta,
		"calibration":           CalibrationBauland,
		"bauland_cagr_by_state": stateTrends,
		"agri_cagr_by_state":    agriTrends,
		"factors_documented": map[string]string{
			"size":     "smaller parcels carry a per-m² premium; reference 600 m² lot",
			"shape":    "more compact (closer to square/circle) → +up to 15%",
			"typology": "KG typology multiplier (urban 1.30 / suburban 1.08 / rural 0.92–0.78)",
		},
		"endpoints": []string{
			"/api/v1/land_prices/parcel/{parcel_id}",
			"/api/v1/land_prices/gemeinde/{gemeinde_code}",
			"/api/v1/land_prices/predict?gemeinde_code=&area_sqm=&building_count=&landuse_codes=51&compactness=0.6",
			"/api/v1/land_prices/validate?limit=400",
		},
	}
	writeJSON(w, http.StatusOK, resp)
}

// cagrMap converts a map of log-linear slopes into compound annual growth rates.
func cagrMap(in map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(in))
	for k, v := range in {
		out[k] = math.Exp(v) - 1
	}
	return out
}

// GET /api/v1/land_prices/parcel/{parcel_id}
func handleLandPriceParcel(w http.ResponseWriter, r *http.Request) {
	pid := NormalizeObjectID(strings.TrimPrefix(r.URL.Path, "/api/v1/land_prices/parcel/"))
	if pid == "" {
		apiError(w, "missing parcel_id", http.StatusBadRequest)
		return
	}
	in, err := loadParcelPricingInput(pid)
	if err != nil {
		apiError(w, err.Error(), http.StatusNotFound)
		return
	}
	est := PredictLandPrice(*in, parseYearParam(r, 2024))
	writeJSON(w, http.StatusOK, est)
}

// parcelPricingSelectSQL is the canonical SELECT used by every aggregate query.
// Returns the columns in the exact order scanParcelPricingRow expects.
// The subquery sums footprint areas via footprint_parcel_link + footprint_geom
// (per-KG warm cache; the former footprint_metrics copy was dropped); parcels
// without footprints — or in a KG whose footprint geometry is not yet built —
// get a NULL that scans to 0.
const parcelPricingSelectSQL = `
SELECT p.parcel_id, p.gemeinde_code, k.district_code, k.state_code,
       p.area_sqm, p.building_count, p.footprint_count, p.compactness,
       p.landuse_codes, k.typology,
       COALESCE((SELECT SUM(fm.area_sqm)
                   FROM footprint_parcel_link fpl
                   JOIN footprint_geom fm ON fm.footprint_id = fpl.footprint_id
                  WHERE fpl.parcel_id = p.parcel_id
                    AND fpl.primary_parcel = 1), 0) AS footprint_area_sum
  FROM parcels p
  JOIN kg k ON k.kg_code = p.kg_code`

// scanParcelPricingRow populates a ParcelPricingInput from one DB row.
func scanParcelPricingRow(scanner interface{ Scan(...any) error }, in *ParcelPricingInput) error {
	var lu, typ, dc, sc *string
	var fpArea sql.NullFloat64
	if err := scanner.Scan(&in.ParcelID, &in.GemeindeCode, &dc, &sc,
		&in.AreaSqm, &in.BuildingCount, &in.FootprintCt, &in.Compactness,
		&lu, &typ, &fpArea); err != nil {
		return err
	}
	if lu != nil {
		in.LanduseCodes = *lu
	}
	if typ != nil {
		in.Typology = *typ
	}
	if dc != nil {
		in.BezirkCode = *dc
	}
	if sc != nil {
		in.StateCode = *sc
	}
	if fpArea.Valid {
		in.FootprintAreaSqm = fpArea.Float64
	}
	return nil
}

func loadParcelPricingInput(parcelID string) (*ParcelPricingInput, error) {
	db := globalSearchIndex.db
	if db == nil {
		return nil, fmt.Errorf("index not initialised")
	}
	row := db.QueryRow(parcelPricingSelectSQL+` WHERE p.parcel_id = ?`, parcelID)
	in := &ParcelPricingInput{}
	if err := scanParcelPricingRow(row, in); err != nil {
		return nil, fmt.Errorf("parcel %s not found: %v", parcelID, err)
	}
	return in, nil
}

// GET /api/v1/land_prices/gemeinde/{gemeinde_code}
// Aggregates per-class statistics across all parcels in the municipality and
// reports how the predicted Bauland geometric mean compares to the SA value.
func handleLandPriceGemeinde(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/land_prices/gemeinde/"), "/")
	if len(parts) < 1 || parts[0] == "" {
		apiError(w, "missing gemeinde_code", http.StatusBadRequest)
		return
	}
	gem := parts[0]
	if !isPrivateTier(r) { // public tier: Statistik Austria series only (api_context.go)
		landPriceStatistikOnly(w, r, "gemeinde", gem)
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

	rows, err := db.Query(parcelPricingSelectSQL+` WHERE p.gemeinde_code = ?`, gem)
	if err != nil {
		apiError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	buckets := aggregateParcels(rows, year)
	rows.Close()

	// Also surface a building-value subtotal across the municipality.
	var buildingValueTotal, buildingAreaTotal float64
	rows2, _ := db.Query(parcelPricingSelectSQL+` WHERE p.gemeinde_code = ?`, gem)
	if rows2 != nil {
		for rows2.Next() {
			in := ParcelPricingInput{}
			if err := scanParcelPricingRow(rows2, &in); err != nil {
				continue
			}
			if in.FootprintAreaSqm <= 0 {
				continue
			}
			est := PredictLandPrice(in, year)
			if est != nil && est.Building != nil {
				buildingValueTotal += est.Building.ValueEur
				buildingAreaTotal += est.Building.FootprintAreaSqm
			}
		}
		rows2.Close()
	}

	resp := map[string]any{}
	resp["gemeinde_code"] = gem
	resp["year"] = year
	resp["by_class"] = serializeBuckets(buckets)
	if buildingValueTotal > 0 {
		resp["buildings"] = map[string]any{
			"total_footprint_area_sqm": buildingAreaTotal,
			"total_value_eur":          buildingValueTotal,
		}
	}

	// Validation block vs Statistik Austria (Bauland geometric mean)
	saVal, saYr := lookup(lp.BaulandByGemeinde[gem], year)
	if saVal > 0 {
		validation := map[string]any{
			"sa_bauland_eur_per_sqm": saVal,
			"sa_year":                saYr,
		}
		var pred float64
		for _, c := range []LandClass{ClassBaulandBuilt, ClassBaulandZoned} {
			if b, ok := buckets[c]; ok && b.BuyCount > 0 {
				pred = math.Exp((b.BuyLogSum) / float64(b.BuyCount))
				validation["predicted_geomean_eur_per_sqm"] = pred
				validation["predicted_minus_sa_pct"] = 100 * (pred - saVal) / saVal
				break
			}
		}
		resp["validation"] = validation
	}
	writeJSON(w, http.StatusOK, resp)
}

// GET /api/v1/land_prices/predict?…
// Ad-hoc prediction for caller-supplied parcel attributes (no DB lookup).
func handleLandPricePredict(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	in := ParcelPricingInput{
		GemeindeCode: q.Get("gemeinde_code"),
		StateCode:    q.Get("state_code"),
		LanduseCodes: q.Get("landuse_codes"),
		Typology:     q.Get("typology"),
	}
	if in.GemeindeCode != "" && in.StateCode == "" {
		in.StateCode = string(in.GemeindeCode[0])
	}
	in.AreaSqm, _ = strconv.ParseFloat(q.Get("area_sqm"), 64)
	in.BuildingCount, _ = strconv.Atoi(q.Get("building_count"))
	in.FootprintCt, _ = strconv.Atoi(q.Get("footprint_count"))
	in.Compactness, _ = strconv.ParseFloat(q.Get("compactness"), 64)
	in.FootprintAreaSqm, _ = strconv.ParseFloat(q.Get("footprint_area_sqm"), 64)
	in.BuildingPeriod = q.Get("building_period")
	if b := q.Get("bezirk_code"); b != "" {
		in.BezirkCode = b
	}
	est := PredictLandPrice(in, parseYearParam(r, 2024))
	writeJSON(w, http.StatusOK, est)
}

// GET /api/v1/land_prices/validate?limit=500
// Sample-based validation against SA: how well does our predicted geometric mean
// match the Statistik-Austria gemeinde value?
func handleLandPriceValidate(w http.ResponseWriter, r *http.Request) {
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
	year := parseYearParam(r, 2024)
	limit := 500
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 5000 {
		limit = v
	}
	minPer := 8
	if v, err := strconv.Atoi(r.URL.Query().Get("min_parcels_per_gemeinde")); err == nil && v > 0 {
		minPer = v
	}

	rows, err := db.Query(parcelPricingSelectSQL+`
		 WHERE (p.building_count>0 OR p.footprint_count>0
		        OR p.landuse_codes LIKE '%40%' OR p.landuse_codes LIKE '%41%'
		        OR p.landuse_codes LIKE '%42%' OR p.landuse_codes LIKE '%46%'
		        OR p.landuse_codes LIKE '%91%')
		 ORDER BY RANDOM()
		 LIMIT ?`, limit*200)
	if err != nil {
		apiError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type acc struct {
		LogSum float64
		N      int
		SA     float64
		SAYear int
	}
	perGem := map[string]*acc{}
	for rows.Next() {
		in := ParcelPricingInput{}
		if err := scanParcelPricingRow(rows, &in); err != nil {
			continue
		}
		est := PredictLandPrice(in, year)
		if est == nil || est.BuyEurPerSqm == nil {
			continue
		}
		if est.Class != ClassBaulandBuilt && est.Class != ClassBaulandZoned {
			continue
		}
		a, ok := perGem[in.GemeindeCode]
		if !ok {
			saVal, sy := lookup(lp.BaulandByGemeinde[in.GemeindeCode], year)
			if saVal <= 0 {
				continue
			}
			a = &acc{SA: saVal, SAYear: sy}
			perGem[in.GemeindeCode] = a
		}
		a.LogSum += math.Log(*est.BuyEurPerSqm)
		a.N++
	}

	type row struct {
		Gemeinde  string  `json:"gemeinde_code"`
		N         int     `json:"n"`
		SA        float64 `json:"sa_eur_per_sqm"`
		Predicted float64 `json:"predicted_eur_per_sqm"`
		Ratio     float64 `json:"ratio_pred_over_sa"`
	}
	results := []row{}
	ratios := []float64{}
	for g, a := range perGem {
		if a.N < minPer {
			continue
		}
		pred := math.Exp(a.LogSum / float64(a.N))
		results = append(results, row{g, a.N, a.SA, pred, pred / a.SA})
		ratios = append(ratios, pred/a.SA)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Ratio < results[j].Ratio })

	summary := map[string]any{
		"year":              year,
		"gemeinden_checked": len(results),
	}
	if len(ratios) > 0 {
		sort.Float64s(ratios)
		median := ratios[len(ratios)/2]
		mean := 0.0
		for _, x := range ratios {
			mean += x
		}
		mean /= float64(len(ratios))
		w15, w25, w50 := 0, 0, 0
		for _, x := range ratios {
			if x >= 0.85 && x <= 1.15 {
				w15++
			}
			if x >= 0.75 && x <= 1.25 {
				w25++
			}
			if x >= 0.5 && x <= 2.0 {
				w50++
			}
		}
		summary["median_ratio"] = median
		summary["mean_ratio"] = mean
		summary["pct_within_15pct"] = 100 * float64(w15) / float64(len(ratios))
		summary["pct_within_25pct"] = 100 * float64(w25) / float64(len(ratios))
		summary["pct_within_50pct"] = 100 * float64(w50) / float64(len(ratios))
	}
	resp := map[string]any{
		"summary": summary,
		"results": results,
	}
	writeJSON(w, http.StatusOK, resp)
}
