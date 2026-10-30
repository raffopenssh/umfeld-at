# Stats envelope — one row per spatial unit, three services

`stats-envelope/1` is the shared answer shape of

| service | endpoint | figures |
|---|---|---|
| umfeld-at | `GET {PUBLIC_URL}/api/v1/ne/stats/unit` | declared · observed · diff · change · subsidy · protection per H3 cell set, **plus the two peers' envelopes for the same unit** |
| {OBS_NAME} | `GET {OBS_WEB}/api/v1/stats/unit` | observed cover, consistency, terrain, heights, canopy, ALS epochs |
| {SUB_NAME} | `GET {SUB_API}/api/v1/stats/unit` | INVEKOS claims, classes, crops, lineage vs previous year, Gemeinde payments |

Same unit keys, same top level, flat scalar figures, so a spreadsheet row, an agent or a chart reads
any of the three — or all three through umfeld-at — without walking cells.

## 1. Unit keys

```
?kg=63330[,63331…]      5-digit KG code(s), ≤ 50 → one envelope per code ("units":[…] when > 1)
?h3=871e108d9ffffff     H3 cell, res 5–12: every res-12 descendant (the hierarchy is exact here)
?bbox=w,s,e,n           WGS84; cells whose centre lies inside (half-open); span ≤ 0.1°
```
umfeld-at additionally takes the landing page's own keys, so **the share link and the export are one
URL logic** — whatever `#…` a user copies from the map is a valid `share=` and vice versa:
```
?adm=61630              Gemeinde (5 digits, union of its KGs) · ?adm=AT2205000  Natura 2000 site (centre inside)
?pt=15.1,47.12          the res-7 cell under a point
?share=%23kg%3D63330%3Bf%3Dforest   the map's hash verbatim (kg= h3= adm= pt= and f= are read, view tokens ignored)
?f=<expr>               the glyph filter (same grammar as /api/v1/ne/filter): figures cover the MATCHING cells,
                        unit_cells_n / unit_area_ha keep the whole unit; peers are then not asked unless peers= says so
```
Bezirk / Bundesland are too large for one row — pass `kg=` lists (≤ 50 per call) or Gemeinden.

## 2. Envelope

```json
{"schema":"stats-envelope/1","service":"umfeld-at",
 "unit":{"kind":"kg|h3|bbox|gemeinde|natura2000","key":"63330","name":"Kohlschwarz","bbox":[w,s,e,n],"res":12,"cells_n":52478,"area_ha":1593.37,"kgs":["63330"]},
 "source":"umfeld-at@ne-fidx1","epoch":"YYYY-MM",
 "coverage":{"complete":true,"cells_known":52478,"cells_total":52478,"bins":12,"bins_missing":0,"note":"…"},
 "filter":{"f":"d=wald","words":"forest","ignored":[],"note":"…"},          ← only with f=
 "figures":{ flat, dotted keys, scalars only — §3 },
 "peers":{"{OBS_NAME}":{ their envelope },"{SUB_NAME}":{ their envelope }},   ← umfeld-at only
 "attribution":"…","license":"CC-BY-4.0","computed_at":"YYYY-MM-DDThh:mm:ssZ","age_s":0,
 "link":"{PUBLIC_URL}/#kg=63330"}
```
Rules every service keeps: `figures` is **flat** (dotted keys), values are `number | string | bool | null`;
the unit is in the key suffix (`_ha _n _m _deg _pct _eur _year`); no data → `200` with
`coverage.complete=false` and empty figures (rows never break); `area_ha` and `cells_n` mean the same
thing everywhere (umfeld-at and the observed-layer peer agree cell for cell on `kg=` and `h3=` — same res-12 set, border
cells weighted by the KG share; {SUB_NAME} clips its own polygons, so its `area_ha` is the unit
polygon's, not a cell sum).

## 3. umfeld-at figures

```
cells_n area_ha cells_declared_n cells_observed_n            [unit_cells_n unit_area_ha match_pct with f=]
declared_ha.<geb|bau|acker|gruen|wald|wasser|verkehr|alpen|sonst|undeclared>  declared.dominant     share-weighted from the declared view (log → bev → lu)
observed_ha.<same groups|unknown>                                                               share-weighted from the peer's observed rows
diff_ha.<consistent|forest_loss|forest_gain|sealed_new|structure_new|green_new|unknown>  diff.consistent_pct    register vs LiDAR, per cell today
change_n.<none|candidate|confirmed>  change_ha.<same>  change.log_versions  change.log_since   the register over time (confirmed = digest seen twice)
subsidy_ha.<acker|gruenland|alm|wein|dauerkultur|gloez|sonst>  subsidy.claimed_ha .claimed_pct .bio_ha .bio_pct .known_pct   INVEKOS class × claimed share per cell
natura2000_ha natura2000_pct  structures_n structures.cover_ha
terrain.elev_mean_m terrain.slope_mean_deg height.p90_mean_m canopy.cover_pct ndvi.mean       area-weighted means over cells with a value
kg.dominant kg.n kg.name gemeinde.code gemeinde.name bezirk.name bundesland.name             where the unit sits (h3 / bbox / Natura: the dominant KG)
```
Every per-cell number comes from the filter index bins (`/api/v1/ne/glyph/fidx`) joined with the cells'
declared/observed rows — `/api/v1/ne/filter`, the map's filter chips and this endpoint count the same
cells (exact H3 centres, exact cell areas).

Peer envelopes are passed through unchanged under `peers.<service>`. For a Gemeinde the peers answer
per KG; umfeld-at sums their additive figures (`_ha _n _eur`), recomputes `_pct` from `area_ha` and
drops means, codes and names (`coverage.note` says so; `units_n` counts the members).

## 4. Formats

```
format=json   the envelope (default); several units → {"schema","service","units":[…]}
format=flat   ONE flat object: unit.kind unit.key unit.name unit.bbox coverage.complete [filter] <figures> srtm.<key> sub.<key> link
format=csv    header row + one row per unit (text/csv, UTF-8, no BOM); &download=1 adds a filename
keys=a,b      only these keys (prefix match: keys=declared_ha,srtm.cover_ha,sub.claimed)
peers=srtm,sub|none   which peers to ask (default both; none with f=)      year=   passed to {SUB_NAME}
```
Google Sheets: `=IMPORTDATA("{PUBLIC_URL}/api/v1/ne/stats/unit?kg=63330&format=csv")`
(the map's ▾ next to the link button copies this formula for the current view). Excel: Power Query →
From Web, same URL. Several KGs in one sheet: `kg=63330,63331,…` → one row each.

Caching: `ETag`, `Cache-Control: max-age=600` (`no-store` while a bin is still building or a peer was
down), CORS `*`, gzip. First call on a cold area builds the index bins (seconds; a Gemeinde up to a
minute); afterwards milliseconds. Answers carry standard `RateLimit-*` headers.

## 5. Peer envelopes

Both peers answer the same keys (`h3= kg= bbox=`) with the same envelope, `format=json|flat|csv`,
`keys=`, and an unknown unit → 200 with `coverage.complete=false`. The observed-layer peer
({OBS_NAME}) carries `source "{OBS_NAME}@<version>"`, `unit.res 12`, `unit.cells_n`, shared
`cover_ha.<geb..sonst>`, `consistency_ha/_n.<verdict>`, terrain, heights and `als.*`. The subsidy
peer ({SUB_NAME}) carries `source "invekos@<release>"`, `epoch` = claim year, `class_ha.<cls>`,
`top_snar.<1..5>.*`, `lineage_ha.<…>` and `gemeinde.*` from the Transparenzdatenbank (dominant
Gemeinde + `gemeinde.n` for units spanning several). umfeld-at calls them with a 5 s timeout,
≤ 2 req/s, memoised 10 min; a peer that is down shows as `peers.<service>.available=false` and the
row keeps the other columns.
