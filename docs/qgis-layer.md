# Cells in QGIS — vector tiles and offline archives

The land-use cell product (declared · observed · change · subsidy · LiDAR
statistics) as **H3 cell polygons with every attribute**, in two forms:

| Form | URL | Use |
|---|---|---|
| Vector tiles (MVT) | `{PUBLIC_URL}/api/v1/ne/mvt/{z}/{x}/{y}.pbf` | live layer, always current |
| PMTiles archive | `{PUBLIC_URL}/api/v1/ne/pmtiles?bbox=w,s,e,n` | one file per area, works offline |

TileJSON: `/api/v1/ne/mvt.json` (lists every field). Zoom 9–15. Same data at the
same zoom in both forms; an archive is just the tiles of one box, packed.

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

**aggregates (z 9–13)** — `n` cells, `area_ha`, `d` dominant declared group +
`d_pct`, `d_known_pct`, one `d_<group>_pct` per group, `o` dominant observed +
`o_pct`, `o_known_pct`, `diff_pct` (declared ≠ observed), `chg_conf`, `chg_cand`,
`chg_pct`, `sub_pct`, and the same epochs.

## QGIS — live vector tiles

1. *Layer → Add Layer → Add Vector Tile Layer* (or Browser panel → *Vector Tiles → New Generic Connection*).
2. Name: `umfeld-at cells`. URL:
   `{PUBLIC_URL}/api/v1/ne/mvt/{z}/{x}/{y}.pbf`
   Min zoom 9, max zoom 15. (Alternatively paste the TileJSON URL
   `{PUBLIC_URL}/api/v1/ne/mvt.json` where QGIS ≥ 3.32 accepts one.)
3. Optional key: *Authentication* → add a configuration of type *HTTP header* with
   `X-Export-Key: <key>`, or put `?key=<key>` after `.pbf` in the URL.
4. Add. Style by `d` at z ≥ 14 and by `d` / `diff_pct` / `chg_pct` below; the layer
   styling panel offers *Rule-based* with zoom ranges — use `res` to tell
   aggregates (`res < 12`) from cells (`res = 12`).

Tiles are cached server-side; a tile whose source bins do not exist yet answers
`202` with a JSON progress body and `Retry-After` — QGIS retries by itself, the
area fills in within seconds to a minute. `/api/v1/ne/mvt/status?bbox=w,s,e,n`
tells you how much of an area is ready (`status: ready|warming|cold`, `eta_s`);
`&prepare=1` queues the missing parts before you open the layer.

## QGIS — offline PMTiles

1. Request the archive once:
   `{PUBLIC_URL}/api/v1/ne/pmtiles?bbox=15.02,47.20,15.06,47.24`
   Instead of `bbox=` you can give `kg=`, `h3=`, `adm=` or `share=` exactly as for
   `/api/v1/ne/stats/unit`, or **`bbox=austria`** for the whole country.
   `minz=`/`maxz=` (default 9–14) trim the zoom range; `maxz=11` is the overview
   (res 7–9, ~30 MB for all of Austria, built in seconds); `maxz=15` doubles the
   size at the top level.
   While the archive is being built the URL shows a progress page (JSON for
   scripts: `202` + `Retry-After`) and reloads itself; keep the URL, it turns into
   the download. Limits for the fine zooms (z ≥ 12): ≤ 1500 km² and ≤ 20 000
   tiles → `413` with a hint to split or use `maxz=11`. Archives stay 48 h.
   The landing page's link ▾ menu builds these URLs for you (this place / all of
   Austria × overview / every cell).
2. Save the `.pmtiles` file.
3. QGIS ≥ 3.32: *Add Vector Tile Layer → Connection type: PMTiles → File*, pick the
   file. Older QGIS: Browser panel → *Vector Tiles → New PMTiles Connection*.
4. Style as above. The file holds the same `cells` layer and the TileJSON metadata
   (fields, bounds, attribution) in its header.

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
