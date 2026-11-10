# Cells in QGIS — vector tiles and offline archives

The land-use cell product (declared · observed · change · subsidy · LiDAR
statistics) as **H3 cell polygons with every attribute**, in two forms:

| Form | URL | Use |
|---|---|---|
| **QGIS project** | `{PUBLIC_URL}/api/v1/ne/mvt.qgs?bbox=w,s,e,n` (or `kg=`, `adm=`, `share=`) | **start here**: one small `.qgs` — double-click and QGIS opens on that view with the live cell layer styled and a legend, basemaps underneath |
| Vector tiles (MVT) | `{PUBLIC_URL}/api/v1/ne/mvt/{z}/{x}/{y}.pbf` | live layer, always current (what the project uses) |
| PMTiles archive | `{PUBLIC_URL}/api/v1/ne/pmtiles?bbox=w,s,e,n` | one file per area, works offline |
| Style (QML) | `{PUBLIC_URL}/api/v1/ne/mvt.qml?for=tiles` / `?for=ogr` | the legend alone, for a layer you added yourself / for an archive |

TileJSON: `/api/v1/ne/mvt.json` (lists every field). Zoom 9–15. Same data at the
same zoom in both forms; an archive is just the tiles of one box, packed. Tiles
are seam-free: a hexagon on a tile edge is in both tiles, QGIS clips it.

If you have a key or token, send it as `?key=` / `X-Export-Key` / `Authorization:
Bearer` (QGIS: *Referer/HTTP headers* in the connection, or append `?key=…` to the
URL). Anonymous use works too.

## Where the data comes from, by zoom

Zooms 9–11 (res 7–9) are read from a **national table** — every res-9 cell of
Austria folded once from the declared/observed product and the change log,
refreshed a few times a day. They answer instantly anywhere and carry the
declared, observed, diff and change aggregates. Zooms 12–15 come from the
detailed index of the area (built on first use, kept) and add the peer columns:
subsidy, LiDAR, Natura 2000. So an *overview* archive can span the whole country;
a *fine* archive is one area at a time.

## Detail by zoom

| zoom | H3 res | cell edge | what a feature is |
|---|---|---|---|
| 9 | 7 | ~1.2 km | aggregate: counts, percentages, dominant classes of the res-12 cells inside |
| 10 | 8 | ~460 m | aggregate |
| 11 | 9 | ~174 m | aggregate |
| 12 | 10 | ~66 m | aggregate |
| 13 | 11 | ~25 m | aggregate |
| 14–15 | 12 | ~9.4 m | the cell itself with all attributes |

Layer id is `cells`. Every feature carries `h3` (string) and `res`.

**res 12 (z ≥ 14) attributes** — `d` declared group, `dsh` declared share,
`as` observed verdict, `sub` subsidy class, `scov` subsidy cover, `n2k`/`n2kt`
Natura 2000 flag/type, `chg` change (confirmed/candidate), `cont`/`cont_open`
contribution counters, `het`, `edge`, `nutz`, `rd`/`rd_min` road distance,
`wat`/`wat_min` water distance, plus epochs `srtm_epoch`, `sub_year`,
`sub_release`. Field meanings and units match the filter grammar in
`/api/v1/docs/ne.md` and the unit-stats envelope (`/api/v1/docs/stats-envelope.md`).

**aggregates (z 9–13)** — `n` cells (`n_all`, `match_pct`, `approx`, `sample` with `f=`, see below), `area_ha`, `d` dominant declared group +
`d_pct`, `d_known_pct`, one `d_<group>_pct` per group, `o` dominant observed +
`o_pct`, `o_known_pct`, `diff_pct` (declared ≠ observed), `chg_conf`, `chg_cand`,
`chg_pct`, `sub_pct`, and the same epochs.

## Filtered layers — the map's veil in QGIS

Append `f=<expr>` — the same expression as the map's filter chips, `/api/v1/ne/filter`
and `/api/v1/ne/stats/unit` (`;f=` of a share link; the link ▾ menu writes it for
you) — and the layer holds only what the map leaves un-veiled:

| zoom | with `f=` |
|---|---|
| 14–15 (res 12) | exactly the matching cells; the rest is absent |
| 12–13 (res 10–11) | hexagons with ≥ 1 matching cell; aggregates over the **matching** cells; `n` = matching, `n_all` = every cell, `match_pct`, `area_ha` = the matching share of the hexagon |
| 9–11 (res 7–9) | the same, folded from the national 1-in-49 sample (one res-12 cell per res-10 cell): counts ×49, `approx=true`, `sample=49`; every term applies, `k>=` not |

`/api/v1/ne/mvt.json?f=…` is a TileJSON with the filter in its tile URL (for
MapLibre and scripts; QGIS wants the `{z}/{x}/{y}` template, the project carries
it); `/api/v1/ne/pmtiles?…&f=…` (or a `share=` whose hash
carries `;f=`) builds the archive of the matching cells only — its metadata records
the filter. Tiles of different filters are cached apart, so a filtered layer costs the
same as the plain one once rendered.

## QGIS — the project (recommended)

`{PUBLIC_URL}/api/v1/ne/mvt.qgs?bbox=15.02,47.20,15.06,47.24` — save, double-click.
Instead of `bbox=` give `kg=`, `adm=` (Gemeinde / Natura 2000 site) or **`share=` with
the landing page's hash**: the link ▾ menu → *QGIS → project* writes exactly that.
A share hash carries the view (`;at=`), the profile (`;v=obs`) and the overlays
(`;change ;diff ;trees ;subsidy ;roads ;relief ;ortho`), and the project mirrors
them: it opens on the same extent, with the same things switched on.

What is in the file (a few KB of XML, nothing downloaded in advance):

- **cells** — the live vector-tile layer (`mvt/{z}/{x}/{y}.pbf`, zoom 9–15, the
  filter `?f=` and your `?key=` in the URL if you passed them). Its legend is the
  landing page's set of toggles, each a rule you tick on and off in the layer panel:
  *declared · group* (fill by `d`, every zoom), *observed · group* (fill by `o`,
  the `;v=obs` profile), *no declared use*, *change · confirmed / candidate* (red
  outlines, z ≥ 13; *in the area* on aggregates below), *declared ≠ observed*
  (violet outline, ≥ 25 % of an aggregate; the cell itself at z ≥ 13), *trees ·
  height classes* (green, by `ht`), *subsidy · class* (orange, by `sub`), *roads ·
  within 50 m* (`rd_min = 0`). Colours are the landing page's palette. The styles
  run to zoom 24: zoom in past 15 and QGIS keeps painting the z15 tile.
- **trees** — the landing page's tree marks, for a view of ≤ 3 km²: one GeoJSON
  fetch from {OBS_NAME} (`/api/v1/trees/bbox`, up to 20 000 apices), a disc per
  tree, diameter = crown, colour = the page's five height bands (< 5 · 5–15 ·
  15–25 · 25–35 · ≥ 35 m), translucent with a subtle rim; visible below 1:6 000.
  On with `;trees`. Fields `height_m crown_d_m crown_m2 species vitality cell kg_code`.
- **roads · OpenStreetMap**, **water · OpenStreetMap** — for a view of ≤ 400 km²,
  one fetch each from this service's `/api/v1/osm/geometry?bbox=&cat=road,rail`
  (`water_area`); rail dashed, major roads wider. Roads on with `;roads`, water off.
- **relief** — {OBS_NAME}'s hillshade tiles (on with `;relief`).
- **orthophoto** — basemap.at's aerial image (CC BY 4.0; on with `;ortho`, opacity
  from `;ortho=<0.1–1>`).
- **OpenStreetMap** — desaturated, on unless the photo is.

Project CRS is Web Mercator (EPSG:3857); measurement units m / ha; attribution in
the project and layer metadata. Identify (ⓘ) a hexagon to read every attribute;
the *Layer styling* panel edits the rules (add your own on any field below).

### A layer by hand

1. *Layer → Add Layer → Add Vector Tile Layer* (or Browser panel → *Vector Tiles → New Generic Connection*).
2. Name: `umfeld-at cells`. URL:
   `{PUBLIC_URL}/api/v1/ne/mvt/{z}/{x}/{y}.pbf`
   Min zoom 9, max zoom 15. (A TileJSON URL is **not** accepted as the connection
   URL — paste the `{z}/{x}/{y}` template.)
3. Optional key: *Authentication* → add a configuration of type *HTTP header* with
   `X-Export-Key: <key>`, or put `?key=<key>` after `.pbf` in the URL.
4. Add; the layer opens on the whole world with one grey symbol — zoom to Austria
   (z ≥ 9) and load the style: *Layer styling → ⋯ → Load style* from
   `{PUBLIC_URL}/api/v1/ne/mvt.qml?for=tiles` saved as a file (add `&share=` with a
   hash to get its toggles pre-set). Or style by hand: `d` at z ≥ 14, `d` /
   `diff_pct` / `chg_pct` below; `res` tells aggregates (`res < 12`) from cells
   (`res = 12`).

Tiles are cached server-side; a tile whose source bins do not exist yet answers
`202` with a JSON progress body and `Retry-After` — QGIS retries by itself, the
area fills in within seconds to a minute. `/api/v1/ne/mvt/status?bbox=w,s,e,n`
tells you how much of an area is ready (`status: ready|warming|cold`, `eta_s`);
`&prepare=1` queues the missing parts before you open the layer.

## QGIS — offline PMTiles

1. Request the archive once:
   `{PUBLIC_URL}/api/v1/ne/pmtiles?bbox=15.02,47.20,15.06,47.24`
   Instead of `bbox=` you can give `kg=`, `h3=`, `adm=` or `share=` exactly as for
   `/api/v1/ne/stats/unit` (a share hash with a view and no unit = that view), or
   **`bbox=austria`** for the whole country.
   `minz=`/`maxz=` (default 9–14) trim the zoom range; `maxz=11` is the overview
   (res 7–9, ~30 MB for all of Austria, built in seconds); `maxz=15` doubles the
   size at the top level.
   A small archive (a view, a KG) comes back as the file on the first request
   (well under a second). A big one shows a progress page that polls every
   second and turns into the download by itself (JSON for scripts: `202` +
   `Retry-After`); keep the URL. Limits for the fine zooms (z ≥ 12): ≤ 1500 km²
   and ≤ 20 000 tiles → `413` with a hint to split or use `maxz=11`. Archives
   stay 48 h. The landing page's link ▾ menu builds these URLs for you.
2. Save the `.pmtiles` file, and next to it — same name, `.qml` — the style:
   `{PUBLIC_URL}/api/v1/ne/mvt.qml?for=ogr`. QGIS applies a sidecar `.qml` on open.
3. **Drag the `.pmtiles` file onto the QGIS map** (or *Layer → Add Vector Layer*).
   QGIS has no native PMTiles *vector-tile* source; the file opens through GDAL
   (≥ 3.8, in every QGIS ≥ 3.34) as a plain vector layer `cells` — one zoom level
   at a time, the finest in the file by default (`maxz`, res 12 with every
   attribute). Other zooms: *Layer → Add Vector Layer → open options* `ZOOM_LEVEL=11`
   (GDAL PMTiles driver), or build a second archive with `maxz=11`.
4. The legend comes from the `.qml` (one category per declared group). The file
   also holds the TileJSON metadata (fields, bounds, attribution) in its header.

`/api/v1/ne/pmtiles/jobs` lists your recent archives (status, progress, size).

## Notes

- Coordinates WGS 84 (EPSG:4326 source, Web Mercator tiles). Hex geometry is the
  exact H3 cell boundary.
- Tiles are gzip-encoded on the wire (`Content-Encoding: gzip`) — every tile client
  handles that; `curl --compressed` for scripts.
- Attribution: the BEV notice from `/api/v1/license`, {OBS_NAME},
  {SUB_NAME}, OpenStreetMap contributors, EEA Natura 2000. Include it on
  maps you publish.
- Heavy use: ask for a key via `/api/v1/feedback`; keys get a daily budget suited to
  the work (`429` + `Retry-After` when spent, `403` outside the agreed area).
