// api_docs_public.go — documentation for the PUBLIC tier (api_tiers.go).
//
// The public API serves context layers around a place in Austria and no
// cadastre. These docs are written from scratch for that surface; the full
// (private) reference in api_docs.go is served under the private prefix/ with its paths
// rewritten. Keep this text in sync with publicExactPaths/publicPrefixPaths.
package main

import (
	"net/http"
	"strings"
)

// publicBaseURL — scheme+host for examples; uses the request host so the text
// is right before and after the VM rename.
func publicBaseURL(r *http.Request) string {
	if r == nil || r.Host == "" {
		if u := publicURL(); u != "" {
			return u
		}
		return "http://localhost:8000"
	}
	scheme := "https"
	if strings.HasPrefix(r.Host, "localhost") || strings.HasPrefix(r.Host, "127.") {
		scheme = "http"
	}
	return scheme + "://" + r.Host
}

func publicLLMText(r *http.Request) string {
	rp := strings.NewReplacer("{NE}", publicNELLMSection, "{BASE}", publicBaseURL(r), "{STAGE_NOTE}", serviceStageNote, "{STAGE}", serviceStage)
	return neSpliceCoverage(rp.Replace(publicLLMTemplate))
}

const publicLLMTemplate = `# umfeld-at — context layers around a place in Austria (public API) — stage: {STAGE}
base: {BASE}      docs: /api/v1/docs/llm.txt (this file)      licence: /api/v1/license (per source)
stage {STAGE}: {STAGE_NOTE} — pin to what you read, send breakage to /api/v1/feedback.
JSON, CORS *, gzip, ETag + Cache-Control. No key, no login. Please keep ≤ 5 req/s sustained per client.

## What this is

This API answers "what is AROUND this point?" for any WGS84 coordinate in Austria:
  · municipality (Statistik Austria Gemeinde, Bezirk, Bundesland)
  · land price estimate (Statistik Austria Baugrundstücks-/Boden-/Pachtpreise, modelled)
  · Natura 2000 sites (EEA) and national parks / protected areas (WDPA)
  · OpenStreetMap proximity: nearest road, major road, rail, transit stop, water,
    settlement, remoteness score; raw OSM geometries for a bbox
  · official geographic names (BEV DLM-Namen: Almen, farmsteads, Rieden, peaks, waters)
  · legal references from RIS (Landesrecht) per Katastralgemeinde code
  · register lookups: Gemeinde / PLZ / Ortschaft / KG names and codes
  · NE cells: declared land use per H3 cell and LAND-USE CHANGE over time (section "NE cells" below —
    API, plus the landing page GET / whose every piece a client can rebuild: see "Landing page")

  The cadastre itself stays client-side: parcels, footprints and land use come straight
  from the official BEV vector tile cadastre (VTC, CC BY 4.0). vtcseamless
  (https://github.com/raffopenssh/vtcseamless, open source, MIT; Go module + CLI +
  bevdirect-serve) stitches the tiles into seamless parcel geometries in your client;
  Python: https://github.com/raffopenssh/vtcseamless-py (bevdirect-serve client + ne_cells + this API).
  You then send us a point plus the parcel features you already hold (see /api/v1/context).

## Keys and conventions

  lon, lat        WGS84 decimal degrees (lon 9–17.5, lat 46–49.1). lon first.
  gemeinde_code   5-digit Statistik Austria Gemeindekennziffer, string ("61630")
  district_code   first 3 digits of gemeinde_code ("616") · state code = first digit
  kg_code         5-digit zero-padded Katastralgemeinde code ("01503"). Only used as a
                  key for legal refs and toponyms; we never resolve a point to a KG.
  Every response: meta.license {license, license_url, attribution, dataset, modified}
                  + headers Link: <…>; rel="license" and X-Data-Attribution. They
                  differ per endpoint — store and show them with the data.
  Pagination:     limit / offset where listed; meta.total.
  ?pretty=1       indents JSON. Errors: {"error": "..."} with 4xx/5xx; 503 + Retry-After
                  means a dataset is still loading after a restart — retry, don't cache.

## The one call: GET /api/v1/context

  GET /api/v1/context?lon=15.0857&lat=47.0665
  GET /api/v1/context?lon=15.0857&lat=47.0665&area_sqm=812&building_count=1
        &footprint_area_sqm=140&landuse_areas=41:140,52:672&kg=63349
  GET /api/v1/context?lon=…&lat=…&include=municipality,natura2000&radius_m=800

  Everything below bundled for one point, one round trip. Blocks (include=,
  default all; legal only when kg= is given):
    municipality    {gemeinde_code, name, district_code, district_name, state}
    land_price      Statistik-Austria-calibrated estimate — needs area_sqm; honours
                    building_count, footprint_count, footprint_area_sqm,
                    building_period (pre1960|y1960_1990|post1991), compactness (0–1),
                    landuse_areas=NS:m²,… (your Benützungsart split) or landuse_codes=51,52,
                    typology (urban|suburban|rural_settled|rural_agrarian), year
    natura2000      {in_natura2000, sites[{sitecode, sitename, sitetype, site_type_label, area_ha, habitats[]}]}
    protected_area  {in_protected_area, sites[{name, desig, desig_eng, desig_type, iucn_cat, rep_area}]}
    osm             the osm{} metrics block (see OSM below); area_sqm refines road_on_parcel
    toponyms        {radius_m, total, rows[]} names within radius_m (default 500, max 2000),
                    filters layer=, f_code= as in /toponyms/near; limit (default 20, max 50)
    legal           RIS refs for kg_code (filters context=, type=, law=) + details link
  data.sources[]   which datasets answered; data.missing{block: reason} for blocks that
                   could not be served (dataset not loaded, outside Austria, area_sqm absent).
  A block is never a reason for a failed request. Typical latency 5–40 ms.

## Municipalities & register lookups (Statistik Austria, EDM)

GET /api/v1/search/municipalities?contains_lon=&contains_lat=     Gemeinde containing a point
GET /api/v1/search/municipalities?q=Gerasdorf | ?id=61630 | ?district=617 | ?state=Steiermark | ?list=all&limit=5000
    rows: gemeinde_code, name, district_code, district_name, state, lon, lat (centre)
    format=geojson → official Statistik Austria polygons
GET /api/v1/search/gadm?q= | ?contains_lon=&contains_lat=            GADM v4.1 (legacy, non-commercial licence)
GET /api/v1/lookup?q=Kofla | ?q=8580 | ?q=Wien&type=gemeinde | ?q=61630&type=kg | ?q=Pflegalm&type=toponym
    Diacritics-insensitive autocomplete across 2 092 Gemeinden, 7 850 KG names/codes,
    16 988 Ortschaften, 2 236 PLZ (federal EDM register). Shorthand: ?kg= ?plz= ?gemeinde= ?ortschaft=.
    rows: {type, code, name, plz[], gemeinde_code, gemeinde_name, kg_code, kg_number}.
    type=kg returns the KG's code and name only — it is a register row, not cadastre.
GET /api/v1/search/address_osm?q=Innsbruck+Hauptplatz&limit=3        Nominatim geocoding (OSM, 1 req/s)

## KG universe — GET /api/v1/kgs (the contract: kg_count = 7850)

GET /api/v1/kgs                      ALL Katastralgemeinden in ONE gzipped list (~520 KB gz / 1.8 MB).
    → data{kg_count: 7850, kg_count_contract: 7850, universe_hash, universe_hash_recipe,
           generated_at, peer_check{service: "{OBS_NAME}", agree, checked_at, …},
           kgs[{kg_code, kg_name, gemeinde_code, gemeinde_name, district_code, district_name,
                state_code, state_name, bbox[min_lon,min_lat,max_lon,max_lat], centroid[lon,lat]}]}
    universe_hash = sha256(join(sorted(kg_code), "\n")) hex — the fingerprint of the SET of
    codes; it is also the ETag of this response and the header X-KG-Universe-Hash. Recompute it
    from your own list: equal hash ⇒ same universe. {OBS_NAME} serves the same 7 850
    (/api/v1/kgs?processed_only=0 there); peer_check (re-verified hourly) and the header
    X-KG-Universe-Peer-Agree say whether the two agree right now. The ETag only changes when the
    SET of codes changes or the peers disagree — never because checked_at moved.
    Fetch once, revalidate with If-None-Match (304), Cache-Control max-age 3600.
    kg_count ≠ 7850 or a changed universe_hash means the universe changed — treat as a new epoch.
GET /api/v1/kgs?fields=codes         {codes:[…]} only (~60 KB) — enough to compute the hash
GET /api/v1/kgs?codes=63349,01004    subset (request order kept, unknown codes dropped → count)
                                     unpadded "1004" is accepted and answered as "01004"; kg_code in
                                     the list is ALWAYS 5 digits — pad before hashing (universe_hash)
GET /api/v1/kgs?district=Voitsberg | ?district_code=616 | ?gemeinde_code=61631 | ?state=Steiermark | ?state=6
    district= is an EXACT, case-insensitive Bezirk name (or the 3-digit code). Bezirk "Waidhofen
    an der Thaya" exists twice (322 + 325); district_code picks one.
    This replaces the old states → districts → /search/kg loop; register keys only, no parcel data.
    bbox (since 2026-10-27, data.bbox_revision = 2, header X-KG-BBox-Revision): the envelope of the
    BEV VGD Katastralgemeinde polygon (+0.0001°) unioned with the previous bbox — the same KG
    outline the declared NE layer (NEC1) is cut from. Earlier lists (revision 1) carried
    parcel-derived envelopes that were narrower for most KGs. Nothing shrank: bbox_new ⊇ bbox_old
    for every KG; kg_count and universe_hash are unchanged; the ETag gained a "-b2" suffix so a
    cached copy revalidates once. If you planned work from the old bbox, process only the slivers:
GET /api/v1/kgs/bbox_changes         every KG whose bbox widened: {kg_code, bbox_old, bbox_new,
                                     max_delta_deg, slivers[≤4 × [w,s,e,n]]} (slivers = bbox_new − bbox_old,
                                     disjoint rectangles). ?min_delta=0.001 keeps KGs where an edge moved
                                     ≥ that many degrees (0.001° ≈ 75–110 m; 6 239 KGs), ?codes=, ?state=,
                                     ?district_code=, ?gemeinde_code=, ?fields=codes. Header
                                     X-KG-BBox-Revision; data.from_generated_at = the list you had.

## Land prices (Statistik Austria, modelled)

GET /api/v1/land_prices/point?lon=&lat=&area_sqm=[&building_count=&footprint_area_sqm=&landuse_areas=&compactness=&year=]
    Gemeinde resolved from the point, then the same model as /predict. Returns
    {municipality, estimate{class, buy_eur_per_sqm, buy_total_eur, rent_eur_per_ha_year,
    rent_eur_per_year, baseline_eur_per_sqm, baseline_source, baseline_year, factors{size,
    shape, typology}, confidence, building{…} when footprint_area_sqm given}}.
GET /api/v1/land_prices/predict?gemeinde_code=61630&area_sqm=750&building_count=1&compactness=0.6
    Same model, Gemeinde given explicitly. Pure function of its parameters.
GET /api/v1/land_prices/gemeinde/61630 · /bezirk/616 · /bundesland/6 (or /bundesland/Steiermark, /AT)
    The published Statistik Austria series for that area: statistik_austria{value, year}
    for the requested ?year= (default 2024; interpolated/extrapolated with the fitted
    CAGR when the year is not observed), series{…: {year: value}} with every year we
    hold, bauland_cagr / trends_cagr. Gemeinde: Baugrundstückspreise. Bezirk: Bauland,
    Häuser, Wohnungen (+ by Bauperiode). Bundesland: Acker-/Grünland buy + rent.
    Model: class from your features (built → bauland_built; NS 40–47/91 unbuilt →
    bauland_zoned; 51/62 → ackerland; 50/52–55/58/61 → gruenland; 56/57 → wald), then
    price = SA baseline(year) × calibration × f_size × f_shape × f_typology. These are
    model outputs, not valuations; 89 % of Gemeinden within ±25 % of SA for 2024.

## Protected areas

GET /api/v1/natura2000/point?lon=&lat=                 EEA Natura 2000 sites containing the point
GET /api/v1/natura2000/site/AT1205A00[?geometry=1]     one site (+ MultiPolygon)
GET /api/v1/natura2000/search?q=Wachau | ?type=A|B|C | ?list=all&limit=400
    A = Birds Directive (SPA), B = Habitats Directive, C = both. habitats[] are tags
    inferred from the site name (moor, floodplain, river, lake, forest, alpine, …).
GET /api/v1/search/protected_area?contains_lon=&contains_lat= | ?q=Nationalpark | ?near_lon=&near_lat=
    43 WDPA areas (national parks etc.); format=geojson for polygons. WDPA terms: non-commercial.

## OpenStreetMap proximity (ODbL)

GET /api/v1/osm/point?lon=&lat=[&area_sqm=]
    osm{}: dist_road_m + road_fclass/name, road_on_parcel (needs area_sqm), dist_major_road_m
    + major_road_fclass/name/ref, dist_rail_m, dist_transit_m + transit_fclass/name,
    dist_train_station_m + train_station_name, dist_water_m + water_fclass/name,
    dist_settlement_m + settlement_fclass/name, remoteness 0..100 (0 central, 100 remote;
    weights road/5 km .30, major road/20 km .15, transit/10 km .30, settlement/15 km .25).
GET /api/v1/osm/geometry?bbox=west,south,east,north[&cat=road,rail,water,transit,place][&major=1][&limit=]
    GeoJSON FeatureCollection of raw OSM geometries to draw context fast. Line props:
    cat, fclass, name, ref, major; point props: cat, fclass, name, train_station,
    population. Lines are ≤24-vertex chunks sharing endpoints. Public tier: bbox only.
GET /api/v1/osm/stats                                   import date, counts
    Attribution: "© OpenStreetMap contributors", ODbL. A database you redistribute that
    merges osm{} fields becomes share-alike — keep OSM-derived columns separable.

## Geographic names — BEV DLM-Namen (CC BY 4.0)

233k official names in seven layers: siedlung (settlements down to single farms and
Alm huts), gebiet, gelaende (peaks, passes, valleys), gletscher, gewaesser, sonstige
(huts, chapels, castles, ruins, caves), ried (104k Riednamen — field names). Points,
positional accuracy ±50 m. Every row: id (BEV GLOBALID, stable), layer, f_code, f_name,
name, variant/minority-language names, elevation_m?, kg_code, kg_name, gemeinde_code,
gemeinde_name, district_*, state_*, lon, lat, stichtag, currency_year.
GET /api/v1/toponyms/near?lon=&lat=[&radius=500][&layer=siedlung,ried][&f_code=]     what is this place called
GET /api/v1/toponyms/search?q=Lignitzalpe[&layer=&kg=&gemeinde=&district=&state=&near=lon,lat&radius=&bbox=&min_score=]
    Fuzzy (trigram, stemmed: "Liegnitzalm" finds "Lignitzalpe"), score 1.0 exact … 0.55.
    Names repeat nationally ("Hochalm" = 1 099 rows) — constrain by area or near=.
GET /api/v1/addresses/search?q=<text>[&bbox=w,s,e,n][&c=lon,lat][&limit=8]      BEV Adressregister (CC BY 4.0, 2.5 M addresses)
    Free text: street [house number] [PLZ / Ort / Gemeinde], any order, prefixes ok ("maria theres 18 inns").
    results[] kind=address|street with label, sub, street, hnr, plz, ort, gemeinde, gkz, lon, lat, in_view
    (inside bbox), dist_km (to c). Ranked: in view → exact number → nearest. limit ≤ 20; q < 2 chars → [].
    Attribution travels in the body (attribution, stichtag) and in X-Data-Attribution.
GET /api/v1/toponyms/{id}                               full row (public tier: without the parcel block)
GET /api/v1/kg/{kg_code}/toponyms[?layer=ried]          everything named inside one KG, Rieden first
GET /api/v1/toponyms/stats                              stichtag, counts per layer
    format=geojson|csv on search/near. Attribution: "Datenquelle: BEV – Geographische Namen (DLM),
    CC BY 4.0, bearbeitet" with the Stichtag from /toponyms/stats.

## Legal references — RIS Landesrecht

GET /api/v1/legal/kg/{kg_code}[?context=national_park&type=listed|boundary_walk&law=Kalkalpen]
    Laws (Verordnungen) that name parcels of this KG: refs[{law, legal_context, parcel_list_type,
    grundstueck, ris_url, …}], total_refs, unique_laws, legal_contexts. The Grundstücksnummern
    here are quoted from the law text, not from a cadastre. Filter client-side by your gnr.
GET /api/v1/legal/search?q=Kalkalpen | ?context=water_protection&bundesland=Oberösterreich | ?type=boundary_walk&kg=50207
GET /api/v1/legal/stats
    contexts: national_park, nature_protection, landscape_protection, nature_park, water_protection,
    species_protection, admin_boundary, zoning, monument_protection, viticulture, hunting, forestry, other.

{NE}
## Meta

GET /api/v1/license          every dataset with licence, attribution text, modified-note, endpoints
GET /api/v1/cache/policy     client-side caching rules (per-source attribution, TTL recommendations)
GET /api/v1/docs/cache-client.py | .js    reference caching clients (ETag revalidation, ATTRIBUTION.txt)
POST /api/v1/feedback        {"title": "...", "agent": "...", "category": "bug|feature|improvement|question",
                             "severity": "critical|high|normal|low", "description": "...", "endpoint": "...", "context": {}}
GET /api/v1/feedback?status=open

## Recipe: a map client with its own cadastre ({MAP_NAME} pattern)

  1. Draw parcels/footprints/land use from the BEV VTC via vtcseamless / bevdirect-serve
     (or any BEV-tile assembler).
  2. On click, send the parcel's representative point + the features you already hold:
     GET /api/v1/context?lon=&lat=&area_sqm=&building_count=&footprint_area_sqm=&landuse_areas=&kg=
  3. Render data.municipality, data.land_price, data.natura2000/protected_area badges,
     data.osm distances, data.toponyms names, data.legal; show each block's attribution
     (meta.license plus the per-source notes in /api/v1/license).
  4. For the map background: /api/v1/osm/geometry?bbox=…&cat=road,water,rail&major=1 and
     /api/v1/toponyms/search?bbox=…&layer=siedlung,ried.
  If this API is down, only those overlays degrade — your map keeps working.
  0. (once) GET /api/v1/kgs → the 7 850-KG universe + universe_hash; key your per-KG state on
     kg_code and compare universe_hash with {OBS_NAME}'s list before joining the two.

## Attribution (short form; full texts in /api/v1/license)
  Statistik Austria: "Datenquelle: STATISTIK AUSTRIA" (CC BY 4.0; price estimates are modelled)
  EEA: "Source: European Environment Agency (EEA), Natura 2000"      WDPA: UNEP-WCMC & IUCN (non-commercial)
  OpenStreetMap: "© OpenStreetMap contributors" (ODbL)                RIS: "Datenquelle: RIS, Bundeskanzleramt"
  BEV DLM-Namen: "Datenquelle: BEV – Geographische Namen (DLM), CC BY 4.0, bearbeitet"
  NE cells: "© BEV (CC BY 4.0), bearbeitet · observed layer © {OBS_NAME} (CC BY 4.0)" — statistics per cell, no object geometry
  Subsidy layer (glyph only): "INVEKOS Schläge © AMA / BML (CC BY 4.0) · per cell by {SUB_NAME}"
  Sibling services: {OBS_NAME} ({OBS_API}/, observed layer per cell, CC BY 4.0) and
  {SUB_NAME} ({SUB_API}/, INVEKOS claims per cell, CC BY 4.0) — their own licence pages apply there.
  This service is not operated by, affiliated with, or endorsed by any of these bodies.
`

// monoDocsCSS — the plain-text look shared with {OBS_NAME}'s landing page
// (same palette, same rules) so the two sibling services read as one family.
// Used by the landing page and the HTML rendering of /api/v1/license.
const monoDocsCSS = `:root{--bg:#fbfbf8;--fg:#1f2328;--mut:#6b7280;--rule:#d9d9d2;--acc:#0b5fa5;--code:#f1f1ec;--cov:#4e2a63;--new:#d9a21b;--newt:#8a5f00;--obs:#178a7a;--chg:#c2410c}
@media(prefers-color-scheme:dark){:root{--bg:#111312;--fg:#d7d9d4;--mut:#8a918c;--rule:#2c302d;--acc:#7ab4ea;--code:#1b1e1c;--cov:#d9a8d0;--new:#fff176;--newt:#e6d35a;--obs:#5ccfb8;--chg:#f0813a}}
*{box-sizing:border-box}
body{font:14px/1.6 ui-monospace,SFMono-Regular,Menlo,Consolas,"Liberation Mono",monospace;background:var(--bg);color:var(--fg);
  max-width:84ch;margin:3rem auto;padding:0 1.25rem;-webkit-font-smoothing:antialiased}
h1{font-size:1.1rem;font-weight:700;letter-spacing:.02em;margin:0 0 1rem;padding-bottom:.6rem;border-bottom:1px solid var(--rule)}
h1::before{content:"# ";color:var(--mut)}
h2{font-size:.95rem;font-weight:700;margin:2.4rem 0 .8rem;text-transform:uppercase;letter-spacing:.08em;color:var(--fg)}
h2::before{content:"## ";color:var(--mut)}
p{margin:.7rem 0}
a{color:var(--acc);text-decoration:none;border-bottom:1px solid transparent}a:hover{border-bottom-color:var(--acc)}
code{background:var(--code);padding:1px 5px;border-radius:3px;font-size:.95em}
pre{background:var(--code);border-left:3px solid var(--rule);padding:.8rem 1rem;margin:1rem 0;overflow:auto;line-height:1.5;font-size:.95em}
table{border-collapse:collapse;width:100%;margin:.8rem 0;table-layout:fixed}
th{text-align:left;font-weight:700;color:var(--mut);text-transform:uppercase;font-size:.78em;letter-spacing:.08em;padding:4px 10px 6px 0;border-bottom:1px solid var(--rule)}
td{padding:6px 10px 6px 0;border-bottom:1px dashed var(--rule);vertical-align:top;overflow-wrap:anywhere}
th:first-child,td:first-child{width:30%}
td:first-child{color:var(--mut)}
th:last-child:not(:nth-child(2)),td:last-child:not(:nth-child(2)){width:18%}
td:last-child:not(:nth-child(2)){color:var(--mut)}
td code{background:none;padding:0}
strong{font-weight:700}
.muted{color:var(--mut);font-size:.9em;margin-top:2.5rem;padding-top:.8rem;border-top:1px solid var(--rule)}
figure.contrib{margin:.8rem 0 1.2rem}
svg.contrib{display:block;width:100%;height:auto;overflow:visible}
figure.contrib .p0{fill:var(--cov);opacity:.3}figure.contrib .p1{fill:var(--cov);opacity:.6}figure.contrib .p2{fill:var(--cov);opacity:.85}
figure.contrib .p3{fill:var(--cov)}figure.contrib .p4{fill:var(--fg);opacity:.8}
figure.contrib .hatch{stroke:var(--bg);stroke-width:1;opacity:.6}figure.contrib .dot{fill:var(--bg);opacity:.7}
svg.contrib .swatch{fill:none;stroke:var(--rule);stroke-width:1;vector-effect:non-scaling-stroke}
svg.contrib .zero{fill:var(--rule)}svg.contrib .hit{fill:transparent}
svg.contrib g.day{outline:none}svg.contrib.js g.day{cursor:pointer}
svg.contrib g.day:hover .p0,svg.contrib g.hov .p0{opacity:.5}svg.contrib g.day:hover .hit,svg.contrib g.hov .hit,svg.contrib g.day:focus-visible .hit{fill:var(--code)}
svg.contrib .mark{fill:var(--acc);opacity:0}svg.contrib g.sel .mark{opacity:1}svg.contrib g.sel .hit{fill:var(--code)}
svg.contrib g.sel .p0{opacity:.5}svg.contrib .mark,svg.contrib .hit,svg.contrib .p0{transition:opacity .15s,fill .15s}
figure.contrib{position:relative}
.contrib-tip{position:absolute;left:0;top:0;z-index:2;pointer-events:none;width:max-content;max-width:min(100%,56ch);padding:.35rem .65rem .4rem;background:var(--bg);border:1px solid var(--rule);border-radius:3px;box-shadow:0 2px 10px rgba(0,0,0,.06);font-size:.86em;line-height:1.5;opacity:0;transition:opacity .14s ease,transform .18s cubic-bezier(.2,.7,.2,1)}
.contrib-tip.on{opacity:1}
.contrib-tip .wd{color:var(--mut);font-size:.82em;text-transform:uppercase;letter-spacing:.08em}.contrib-tip .mut{color:var(--mut)}
.contrib-tip .dh{white-space:nowrap}.contrib-tip .band{white-space:nowrap}.contrib-tip .sw{width:8px;height:8px;margin-right:.35em;vertical-align:0;outline:1px solid var(--rule)}
.contrib-tip .rk{color:var(--mut);font-size:.85em;margin-left:.2em}
.contrib-tip .sec{margin-top:.3rem;padding:.2rem 0 .25rem .6rem;border-left:2px solid var(--cov);white-space:normal}
.contrib-tip .sec.obs{border-left-color:var(--obs)}
.contrib-tip .lab{font-size:.8em;text-transform:uppercase;letter-spacing:.08em;color:var(--fg);white-space:normal}.contrib-tip .lab .mut{text-transform:none;letter-spacing:0;font-size:.95em}
.contrib-tip .src{display:inline-block;width:.55em;height:.55em;border-radius:50%;background:var(--cov);margin-right:.45em;vertical-align:.05em}.contrib-tip .sec.obs .src{background:var(--obs)}
.contrib-tip .kv{white-space:normal}
.contrib-tip .pb{display:flex;height:.5em;margin:.2rem 0 .1rem;gap:1px;background:var(--code);border-radius:1px;overflow:hidden}.contrib-tip .pb svg{display:block;height:100%}
.contrib-tip .pl{font-size:.9em;white-space:normal}
.contrib-tip .cmp{font-size:.92em;white-space:normal}
.contrib-tip .chip{display:inline-block;font-size:.78em;font-weight:700;letter-spacing:.03em;padding:0 .45em;border-radius:2px;vertical-align:1px;color:var(--bg);background:var(--chg)}
.contrib-tip .chip.cand{color:var(--mut);background:var(--code);outline:1px solid var(--rule)}
.contrib-tip .chip.qc{color:var(--bg);background:var(--mut)}
.contrib-tip .qc{margin-top:.2rem;font-size:.9em;color:var(--mut);white-space:normal}
.contrib-tip .ds{font-size:.88em;white-space:normal;overflow-wrap:anywhere;margin-top:.25rem;padding-top:.15rem;border-top:1px dashed var(--rule)}
svg.contrib.js.focus g.day:not(.hov):not(.sel){opacity:.45}svg.contrib g.day{transition:opacity .18s}
@media(prefers-reduced-motion:reduce){.contrib-tip{transition:none}}
svg.contrib .cap{fill:var(--chg)}svg.contrib .chev{fill:var(--chg)}svg.contrib g.flag .hit{fill:transparent}
.contrib-tip.dock{position:static;width:100%;max-width:100%;box-sizing:border-box;margin-top:.5rem}
.contrib-tip .dc{margin-top:.25rem;padding-top:.2rem;border-top:1px dashed var(--rule);white-space:normal}.contrib-tip .dc.none .qc{margin-top:.05rem}.contrib-tip .dc.none{color:var(--mut)}
.contrib-tip .dc .dt{color:var(--chg)}.contrib-tip .dc.cand .dt{color:var(--fg)}.contrib-tip .dc.cand .dt strong{color:var(--chg);opacity:.75}.contrib-tip .dc .tag{display:inline-block;font-size:.8em;font-weight:700;color:var(--bg);background:var(--chg);padding:0 .45em;border-radius:2px;vertical-align:1px;letter-spacing:.03em}
.contrib-tip .dc .st{color:var(--mut);font-size:.9em;margin-top:.1rem}
.contrib-tip .stk{margin-top:.3rem;font-size:.92em;line-height:1.45;white-space:normal}.contrib-tip .stk .sh{white-space:normal}
.contrib-tip .sb{display:flex;height:.7em;margin:.2rem 0 .15rem;background:var(--code);border-radius:1px;overflow:hidden;gap:1px}.contrib-tip .sb i{display:block;height:100%}
.contrib-tip .s0{background:var(--chg)}.contrib-tip .s1{background:var(--chg);opacity:.72}.contrib-tip .s2{background:var(--chg);opacity:.5}.contrib-tip .s3{background:var(--chg);opacity:.42}.contrib-tip .s4{background:var(--chg);opacity:.3}.contrib-tip .s5{background:var(--mut);opacity:.5}
.contrib-tip .stk.c .sb i{opacity:.35}.contrib-tip .stk.c .sw{opacity:.35}
.contrib-tip .sl .sw{display:inline-block;width:.6em;height:.6em;margin-right:.35em;border-radius:1px;vertical-align:-.02em}.contrib-tip .sl .band{white-space:nowrap}
.contrib-tip .ss{color:var(--fg)}.contrib-tip .ss strong{color:var(--chg)}.contrib-tip .stk.c .ss strong{color:var(--fg)}
@media(max-width:640px),(hover:none) and (pointer:coarse){.contrib-tip{position:static;transform:none!important;width:100%;max-width:100%;box-sizing:border-box;margin-top:.5rem;font-size:.92em}.contrib-tip .dh{white-space:normal}svg.contrib.js.focus g.day:not(.hov):not(.sel){opacity:.6}}
svg.contrib .grid{stroke:var(--rule);stroke-dasharray:3 4;stroke-width:1;vector-effect:non-scaling-stroke}
svg.contrib .axis,svg.contrib .tick{stroke:var(--rule);stroke-width:1;vector-effect:non-scaling-stroke}
svg.contrib .lbl{fill:var(--mut);font-size:11px;font-family:inherit}svg.contrib .val{fill:var(--fg);font-weight:700}
figure.contrib figcaption{color:var(--mut);font-size:.9em;margin-top:.4rem}
.stage{font-size:.65rem;font-weight:400;letter-spacing:.12em;text-transform:uppercase;color:var(--mut);border:1px solid var(--rule);border-radius:3px;padding:0 .4em;margin-left:.6em;vertical-align:middle}
`

// serviceStage — shown as a subtle label next to the title and in meta; the
// public surface is young and its contracts may still move.
const serviceStage = "alpha"

// serviceStageNote — the one sentence that explains the label (same wording as
// the sibling {OBS_NAME}, so peers read one message).
const serviceStageNote = "field names and limits may still move between epochs"

const publicDocsHTMLTemplate = `<!doctype html><html lang="en"><head><meta charset="utf-8">
<title>umfeld-at — context layers around a place in Austria</title>
<meta name="viewport" content="width=device-width,initial-scale=1,interactive-widget=resizes-content">
<style>{CSS}</style></head><body>
<h1>umfeld-at — context layers around a place in Austria <span class="stage" title="{STAGE} — {STAGE_NOTE}">{STAGE}</span></h1>
{GLYPH}<p>One coordinate in, everything <em>around</em> it out: municipality, land-price estimate, Natura 2000 /
national parks, OpenStreetMap proximity, official geographic names, legal references — and the declared
land-use statistics of the cell the point falls in. Austria-wide. JSON, CORS, no key.</p>
<p><strong>Cadastre stays client-side.</strong> Parcels, footprints and land use come straight from the official
BEV vector tile cadastre (VTC, CC BY 4.0) — <a href="https://github.com/raffopenssh/vtcseamless">vtcseamless</a>
(open source, MIT) stitches the tiles into seamless parcel geometries in your client. You send us a point plus
the parcel features you already hold and get the context layers around it back.</p>
<h2>Start here</h2>
<pre>GET /api/v1/context?lon=15.0857&amp;lat=47.0665
GET /api/v1/context?lon=15.0857&amp;lat=47.0665&amp;area_sqm=812&amp;building_count=1
    &amp;footprint_area_sqm=140&amp;landuse_areas=41:140,52:672&amp;kg=63349
GET /api/v1/ne/cells?lon=15.0857&amp;lat=47.0665&amp;k=1&amp;format=json</pre>
<table><tr><th>Need</th><th>Call</th><th>Source</th></tr>
<tr><td>Everything about a point</td><td><code>/api/v1/context?lon=&amp;lat=</code> (blocks via <code>include=</code>)</td><td>all below</td></tr>
<tr><td>Gemeinde at a point</td><td><code>/api/v1/search/municipalities?contains_lon=&amp;contains_lat=</code></td><td>Statistik Austria</td></tr>
<tr><td>Land price estimate</td><td><code>/api/v1/land_prices/point?lon=&amp;lat=&amp;area_sqm=</code> · <code>/predict</code> · <code>/gemeinde/{code}</code></td><td>Statistik Austria (modelled)</td></tr>
<tr><td>Natura 2000 / protected areas</td><td><code>/api/v1/natura2000/point?lon=&amp;lat=</code> · <code>/api/v1/search/protected_area?contains_lon=&amp;contains_lat=</code></td><td>EEA · WDPA</td></tr>
<tr><td>Roads, transit, water, remoteness</td><td><code>/api/v1/osm/point?lon=&amp;lat=</code> · <code>/api/v1/osm/geometry?bbox=</code></td><td>OpenStreetMap (ODbL)</td></tr>
<tr><td>Names of places, Almen, Rieden</td><td><code>/api/v1/toponyms/near?lon=&amp;lat=</code> · <code>/api/v1/toponyms/search?q=</code></td><td>BEV DLM-Namen (CC BY 4.0)</td></tr>
<tr><td>Laws naming parcels of a KG</td><td><code>/api/v1/legal/kg/{kg_code}</code></td><td>RIS</td></tr>
<tr><td>Declared land use per H3 cell (res 12, ~300 m²)</td><td><code>/api/v1/ne/cells?bbox=</code> · <code>/api/v1/ne/cells?lon=&amp;lat=&amp;k=1</code> · <code>/api/v1/ne/cell/{h3}</code></td><td>BEV (CC BY 4.0, aggregated)</td></tr>
<tr><td>Whole-KG cell file (download)</td><td><code>/api/v1/ne/{kg}</code> (<code>application/vnd.ne-cells</code> | <code>format=json</code>)</td><td>BEV (CC BY 4.0, aggregated)</td></tr>
<tr><td>Register autocomplete</td><td><code>/api/v1/lookup?q=</code></td><td>EDM / Statistik Austria</td></tr>
<tr><td>Coverage</td><td><code>/api/v1/ne</code> · <code>/api/v1/ne/manifest</code> · <code>/api/v1/license</code></td><td>—</td></tr></table>
{NE}<h2>Documentation</h2>
<p><a href="/api/v1/docs/llm.txt">Full reference (plain text, for humans and LLM agents)</a> ·
<a href="/api/v1/docs/ne.md">NE cells contract</a> ·
<a href="/api/v1/license">Licences &amp; attribution per source</a> (<a href="/api/v1/license?format=json">json</a>) ·
<a href="/api/v1/cache/policy">Caching policy</a> ·
<a href="/api/v1/feedback?status=open">Feedback</a> ·
<a href="/impressum">Impressum</a> · <a href="/datenschutz">Datenschutz</a> · <a href="/lizenzen">Lizenzen</a></p>
<p class="muted">Every response carries <code>meta.license</code> and an <code>X-Data-Attribution</code> header naming its own
source; obligations differ (CC BY 4.0, EEA, WDPA non-commercial, ODbL). Datenquelle: STATISTIK AUSTRIA ·
BEV – Geographische Namen (DLM) · © BEV (CC BY 4.0), bearbeitet · EEA Natura 2000 · UNEP-WCMC &amp; IUCN WDPA ·
© OpenStreetMap contributors · RIS, Bundeskanzleramt · observed layer © <a href="{OBS_API}/">{OBS_NAME}</a> (CC BY 4.0) ·
INVEKOS Schläge © AMA / BML (CC BY 4.0), per cell by <a href="{SUB_API}/">{SUB_NAME}</a> ·
orthophoto: <a href="https://basemap.at/">basemap.at</a> (CC BY 4.0, Datenquelle: basemap.at; fetched by your browser only with the photo layer on). Not operated by or affiliated with Statistik Austria,
BEV, AMA, BML, EEA, UNEP-WCMC, OpenStreetMap, basemap.at or the Bundeskanzleramt. Stage: {STAGE} — {STAGE_NOTE}.</p>
</body></html>`

// publicDocsHTMLText — the landing page with the NE block spliced in
// (publicNEHTMLSection lives in the ban-checked api_docs_public_ne.go).
func publicDocsHTMLText() string {
	r := strings.NewReplacer("{NE}", publicNEHTMLSection, "{CSS}", monoDocsCSS+neGlyphCSS, "{STAGE_NOTE}", serviceStageNote, "{STAGE}", serviceStage)
	return neSpliceCoverage(r.Replace(publicDocsHTMLTemplate))
}

// servePublicLLM / servePublicDocs — used by the docs handlers when the request
// is on the public tier.
func servePublicLLM(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write([]byte(peerText(publicLLMText(r))))
}

func servePublicDocs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Live coverage line inside: browsers must revalidate on every load
	// (no heuristic caching of a page that changes every few minutes).
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(peerText(publicDocsHTMLText())))
}

// handlePublicRoot — "/" on the public tier: landing page; anything else
// falling through the mux is a JSON 404.
func handlePublicRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		tierNotFound(w)
		return
	}
	servePublicDocs(w, r)
}

func init() {
	handleV1Docs = servePublicDocs
	handleV1DocsLLM = servePublicLLM
}
