# Cells in QGIS — vector tiles and offline archives

The land-use cell product (declared · observed · change · subsidy · LiDAR
statistics) as **H3 cell polygons with every attribute**, in two forms:

| Form | URL | Use |
|---|---|---|
| **QGIS project** | `{PUBLIC_URL}/api/v1/ne/mvt.qgs?bbox=w,s,e,n` (or `kg=`, `adm=`, `share=`) | **start here**: one small `.qgs` — double-click and QGIS opens on that view with the live cell layer styled and a legend, basemaps underneath |
| Vector tiles (MVT) | `{PUBLIC_URL}/api/v1/ne/mvt/{z}/{x}/{y}.pbf` | live layer, always current (what the project uses) |
| PMTiles archive | `{PUBLIC_URL}/api/v1/ne/pmtiles?bbox=w,s,e,n` (`&format=zip`: with style and project) | one file per area, works offline |
| **Cells as a plain vector layer** | `{PUBLIC_URL}/api/v1/ne/mvt/cells.geojson?bbox=w,s,e,n` (`&f=` the matching cells · `&f=…&veil=1` the rest) | one view as GeoJSON polygons with every attribute: identify, attribute table, **processing tools** (in the project, off at first) |
| Companion layers | `…/ne/mvt/trees.geojson?bbox=` · `…/ne/mvt/trees/14/{x}/{y}.pbf` · `…/ne/mvt/relief/{z}/{x}/{y}.png` · `…/ne/mvt/pic/{declared\|observed\|sub\|subc\|roads}/{z}/{x}/{y}.png` | what the page draws around the cells: tree crowns, hillshade, the page's own pictures (used by the project) |
| Style (QML) | `{PUBLIC_URL}/api/v1/ne/mvt.qml?for=tiles` / `?for=ogr` (`&share=` for a state of the page) | the legend alone, for a layer you added yourself / for an archive |

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

What is in the file (some tens of KB of XML, nothing downloaded in advance).
**Every layer of ours is fetched from this service** — cells, pictures, trees,
relief — with your `?key=` on each URL if you passed one, so one identity covers
the whole map; without a key the same URLs work anonymously. Only the two foreign
basemaps (basemap.at, OpenStreetMap) are asked directly. Top to bottom:

- **trees · crowns** — every tree as a ring of its crown, as on the page (on with
  `;trees`, visible below 1:34 000). For a view up to 40 km² (the view plus a
  quarter around it) this is **one point layer**,
  `mvt/trees.geojson?bbox=w,s,e,n[&ht=lo..hi]`, fields `h` (m), `crown_d` (m),
  `crown_m2`, `species`, `vitality`, drawn with a **graduated renderer on `h`**:
  five classes in the page's bands (3–5 · 5–15 · 15–25 · 25–35 · ≥ 35 m) and
  greens. To recolour: *Layer styling → Colour ramp →* pick any ramp (Viridis,
  Magma, …) — the rings follow; change the class count or mode there too. The
  ring's diameter is the crown in metres on the ground (it grows as you zoom in,
  never under 1.3 mm); the size expression is on the symbol (*Size → data-defined*)
  — it multiplies `crown_d` by 1/cos(latitude) while the map is in EPSG:3857,
  because Web Mercator stretches the ground by that much. Larger views get the
  same rings from tiles (`mvt/trees/14/{x}/{y}.pbf`, zoom 14, layer `trees`), one
  legend entry per band. A `ht` term of the filter limits the trees as on the page.
- **roads · as on the map** — the page's road and rail ink as a picture
  (`mvt/pic/roads/{z}/{x}/{y}.png`, zoom 12–16), opacity from `;roads=<0.1–1>`.
  *roads · OpenStreetMap lines* (off) is the same network as vectors for a view of
  ≤ 400 km² (`/api/v1/osm/geometry?bbox=&cat=road,rail`), *water* likewise.
- **subsidy claims** — the Schläge as the page draws them (`mvt/pic/sub/…`; under
  `;change` their lineage, `mvt/pic/subc/…`). Pictures only: the outlines
  themselves are not handed out. On with `;subsidy`.
- **relief** — the hillshade of {OBS_NAME}, relayed (`mvt/relief/{z}/{x}/{y}.png`,
  zoom 8–17), flat ground keyed transparent and blended *overlay* like the page
  (`?raw=1` = the tile untouched). On with `;relief`.
- **cells** — the live vector-tile layer (`mvt/{z}/{x}/{y}.pbf`, zoom 9–15, the
  filter `?f=` in the URL). It carries the attributes (identify ⓘ a hexagon), the
  outlines — *change · confirmed / candidate* (z ≥ 13; *in the area* on aggregates
  below), *declared ≠ observed*, *subsidy cells · class* (off) — the *trees ·
  tallest* tint of the wooded hexagons at zoom 12–13, and the group fills
  (*declared · group* by `d`, *observed · group* by `o`). While the pictures below
  are in charge the fills cover the aggregates up to zoom 11 only; with a filter
  they paint the matching cells at every zoom. Styles run to zoom 24.
- **cells · hexagons (this view)** — off at first. The same cells as a **plain
  vector layer** (`mvt/cells.geojson?bbox=`, one fetch of the view): tick it and
  the identify tool ⓘ lists every attribute under its label and unit, the
  attribute table and *select by expression* work, and so do the processing
  tools — a vector-tile layer cannot be an input there. Legend = the page's
  toggles as rules (declared or observed groups by the profile of the link, the
  fill shaded by the group's share like the picture; change · confirmed /
  candidate; declared ≠ observed; subsidy cells off). Extra fields: `area_m2`,
  `lon`, `lat` (centre), `match`. Resolution follows the view: the cells
  themselves up to 16 km², res-11 aggregates up to 110 km², res-10 up to
  700 km² (`&res=10|11` asks for the coarser one); beyond that the layer is
  left out (`413`) — use the tiles or an archive.
- **veil · cells the filter leaves out (this view)** — only with a filter, off
  at first: `cells.geojson?bbox=&f=…&veil=1`, the complement, painted as the
  page's veil. The hexagon layer then holds the cells that pass (`match=true`),
  the veil the rest (`match=false`) — every cell of the view in exactly one, so
  *Intersection* with the hexagons or *Difference* with the veil cuts any layer
  (trees, your own polygons) to what the filter keeps. On aggregates a hexagon
  is in each layer with the cells of that side (`n`, `n_all`, `match_pct`).
- **cells · observed / declared, as on the map** — the page's own cell picture
  (`mvt/pic/observed|declared/{z}/{x}/{y}.png`, zoom 12–16): dominant group,
  shade = its share, the second group as an inner hexagon from 25 %. Opacities
  are the page's for the state of the link (profile, photo, subsidy); under a
  filter the picture steps back to a veil.
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
2. **Add `&format=zip`** (the link ▾ menu does) and the answer is a folder:
   `x.pmtiles`, **`x.qml`** — its style — and **`x.qgs`**, a project. Unpack, keep
   the three together. Add `&view=<share hash>` to style it by a state of the page.
   - Double-click `x.qgs`: the map as on the page, the cells read from the
     archive; trees, subsidy claims, roads, relief and photo are the live layers
     described above (they need a connection, the cells do not).
   - Or drag `x.pmtiles` into any project: QGIS applies the `.qml` of the same
     name — the same legend as rules (declared / observed groups, change,
     mismatch), not one flat symbol. Without the zip, save
     `{PUBLIC_URL}/api/v1/ne/mvt.qml?for=ogr` next to the archive under its name.
3. How QGIS reads it: there is no native PMTiles *vector-tile* source; the file
   opens through GDAL (≥ 3.8, in every QGIS ≥ 3.34) as a plain vector layer
   `cells` — one zoom level, the finest in the file by default (`maxz`, res 12
   with every attribute). Other zooms: *Add Vector Layer → open options*
   `ZOOM_LEVEL=11`, or a second archive with `maxz=11`.
4. **Tile seams.** A hexagon on a tile edge is stored in both tiles, and GDAL cuts
   each copy at the edge — with an outlined symbol the cut shows as a grid. The
   project opens the archive with the open option **`CLIP=NO`** (whole hexagons, no
   seam); when you add the file by hand set `CLIP=NO` in *Add Vector Layer → open
   options*. The `.qml` fills outline in their own colour, so a dragged-in archive
   shows no seam in the fills either. Edge cells are then listed twice (same `h3`).
5. The file also holds the TileJSON metadata (fields, bounds, attribution).

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
