# Land Price Prediction (Statistik Austria-calibrated)

Predicted purchase value (€/m² and € total) for a plot described by the caller
(area, landuse split, building counts) and — for agricultural land — annual
rent, calibrated against published Statistik Austria averages. The service holds
no parcels: the public routes are keyed by Gemeinde / Bezirk / Bundesland code
or take the plot's features as parameters (`/predict`, `/point`,
`/api/v1/context?area_sqm=&landuse_areas=`).

**Data sources** (committed under `data/statistik_austria/`):
- `Baugrundstueckspreise<YYYY>.ods` (2015–2024, complete) — Bauland Ø EUR/m² per Gemeinde
- `Haeuserpreise<YYYY>.ods` (2015–2024, complete; 2020 not published) —
  Bezirk × Bauperiode × Wohnflächenkategorie × Größenkategorie
- `SonderauswertungImmobilien….ods` — Grund/Haus/Wohnung Ø EUR/m² per Bezirk
- `Bodenpreise/Pachtpreise *.csv` — Agricultural buy + rent per Bundesland (2022–2024)

Parsed into `data/statistik_austria/parsed/land_prices.json` (2108 Gemeinden +
101 Bezirke + 9 Bundesländer; Wien synthesised from Bezirk averages).

**Class assignment** (NS landuse alone is too noisy):

| Class | Criterion |
|---|---|
| `bauland_built`  | `building_count>0` OR `footprint_count>0` |
| `bauland_zoned`  | unbuilt; any NS ∈ {40–47, 91} |
| `ackerland`      | majority NS ∈ {51, 62} |
| `gruenland`      | majority NS ∈ {48, 50, 52–55, 58, 61} (incl. Verkehrsfläche) |
| `wald`           | majority NS ∈ {56, 57} |
| `other`          | else (Verkehr, Gewässer, Fels, etc.) — no price |

**Bauland pricing model**:
```
price = SA_gemeinde_baseline(year) × 1.096 × f_size × f_shape × f_typology
  f_size      = clip(0.5, 1.6, (600/area)^0.18)   smaller plots premium
  f_shape     = 0.85 + 0.30 × compactness         square/circle bonus
  f_typology  = urban 1.30 / suburban 1.08 / rural_settled 0.92 / rural_agrarian 0.78
```
The calibration constant 1.096 was fitted on a 100k random parcel sample (from
a BEV-tile build) so that the geometric mean of factor products inverts to 1.0,
keeping per-Gemeinde geometric means aligned with the published SA value.

**Temporal model** (uses every published year, 2016–2024):
- Exact year present → use that SA observation.
- Gap year (e.g. 2019, missing for Bauland) → log-linear interpolation between
  bracketing observations.
- Outside the published window → extrapolate along a per-Gemeinde log-linear
  CAGR (clipped −4% … +18%); state-level CAGR (or AT fallback) is used when
  the per-Gemeinde series is too short.
- Same logic applied to agricultural buy/rent (state-level series 2022–2024).
- State CAGRs (Bauland 2016–2024): Bgld +7.3%, Kärnten +4.9%, NÖ +6.5%,
  OÖ +8.4%, Salzburg +8.3%, Stmk +6.5%, Tirol +7.5%, Vbg +8.4%, Wien +3.1%,
  AT +7.2%. Endpoint `/api/v1/land_prices/stats` returns all CAGRs.
- Predictions include a `trends` block (gemeinde + state CAGR for
  Bauland; buy + rent CAGR for agricultural).

**Validation** (100k sample, ~2000 Gemeinden with ≥8 Bauland parcels):
- 89% of Gemeinden within ±25% of SA price for 2024.
- 88.5% within ±25% for 2019 (now real data, no longer interpolated).
- 100% within ±50% in both cases.
- Worst over-prediction: Wien (sample bias toward central districts).

**Building value** (when the caller supplies `footprint_area_sqm` /
`building_count`): `value_eur = footprint_area × story_factor ×
Bezirk_Haus_EUR_per_sqm(year, period)`. Story factor by KG typology:
urban 2.4 / suburban 1.7 / rural_settled 1.4 / rural_agrarian 1.2. The
Bezirk × Bauperiode (pre1960 / y1960_1990 / post1991) house-price table is
parsed from the annual Häuserpreise ODS files (covers 103 Bezirke +
synthesised Wien aggregate `900`). A `combined_value_eur` field is
returned whenever both land and building values resolve. Use
`?building_period=` (or batch `building_period`) to get high-confidence
estimates; otherwise the model averages periods (confidence=medium).

**Agricultural prediction**: state-level SA EUR/m² used directly. Aggregated
per-state means equal SA exactly (verified across all 9 states).

**Rent (Pacht)**: agricultural only — SA Bundesland EUR/ha/year × area/10000.
Bauland rent is not published by SA.

**Wald**: SA does not publish forest land prices. We report an indicative
literature value per Bundesland (0.6–3.0 €/m²). Marked `confidence="low"`.

**Routes** (`land_prices.go`, `land_prices_aggregate.go`, `api_context.go`):
`/api/v1/land_prices/gemeinde/{code}`, `/bezirk/{code}`, `/bundesland/{code}`
(public → Statistik Austria series only, `landPriceStatistikOnly`; the private
tier adds model aggregates), `/predict?area_sqm=&landuse_areas=ns:m²&building_count=
&footprint_area_sqm=&gemeinde=&year=` (caller-supplied features), `/point?lon=&lat=&…`
(predict + Gemeinde resolved from the point), `/stats` (CAGRs). Attribution
source id `statistik_austria`.

**Code**: `LoadLandPrices()`, `PredictLandPrice(ParcelPricingInput, year)`,
`classifyParcel(…)`. Self-contained; refreshing the SA data = replace the JSON
and restart.

**Refreshing source data**: drop new ODS/CSV files into
`data/statistik_austria/`, then re-run the parser:
```
pip install --break-system-packages pyexcel-ods3
python3 scripts/parse_statistik_austria.py    # regenerate parsed/land_prices.json
sudo systemctl restart umfeld-at
```
