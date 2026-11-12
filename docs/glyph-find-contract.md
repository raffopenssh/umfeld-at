# Glyph find box — server contracts (addresses · admin outlines)

Working notes for the "find anything" extension of the landing glyph's find box (`/`).
The client (`ne_glyph.go` JS) consumes exactly these shapes. Frame units = the glyph viewBox
(`neGlyphFrame`: x = (lon·kx − minx)·scale + pad, y = (−lat − miny)·scale + pad, see `neGlyphKGIndex`).

## 1. Addresses — BEV Adressregister (CC BY 4.0)

Source: "Adresse Relationale Tabellen – Stichtagsdaten", https://data.bev.gv.at/download/Adressregister/Adresse_Relationale_Tabellen_Stichtagsdaten.zip
(latest Stichtag; dated copies under `Archiv_Adressregister/…_YYYYMMDD.zip`). Licence: CC BY 4.0
(https://www.bev.gv.at/Services/Downloads/Produktbezogene-Downloads/Unentgeltliche-Produkte/Adressregister.html);
required notice: `© Österreichisches Adressregister, Stichtagsdaten vom TT.MM.JJJJ` (the Stichtag is
in `Aktualitaetsstand.txt` inside the zip). CSV: UTF-8 with BOM, `;`-separated, quoted strings.
Files used: `ADRESSE.csv` (ADRCD;GKZ;OKZ;PLZ;SKZ;…;HAUSNRTEXT(7);…;HNR_ADR_ZUSAMMEN(14);…;HOFNAME(16);RW(17);HW(18);EPSG(19)),
`STRASSE.csv` (SKZ;STRASSENNAME;STRASSENNAMENZUSATZ;SZUSADRBEST;GKZ;ZUSTELLORT;ZUSTELLORT_ID),
`GEMEINDE.csv` (GKZ;GEMEINDENAME), `ORTSCHAFT.csv` (GKZ;OKZ;ORTSNAME). EPSG ∈ {31254,31255,31256}
= MGI GK West/Central/East (lon0 10°20′ / 13°20′ / 16°20′, false N −5 000 000) → generalise
`mgi_transform.go` (`GKCentralToWGS84` is M31 only). ~2.53 M addresses, 138 k streets.

Store: `data/addresses.db` (SQLite, never committed; keep it SMALL — target ≤ 150 MB):
- `str(skz INTEGER PK, name TEXT, gkz TEXT, gemeinde TEXT, ort TEXT, plz TEXT, lon REAL, lat REAL, n INT)` (centroid + count)
- `str_fts` FTS5 over `name, ort, gemeinde, plz` (unicode61 remove_diacritics 2, `prefix='2 3 4'`) — **streets only**, no FTS over 2.5 M rows
- `adr(skz INT, hnr TEXT, hof TEXT, plz INT, lon INT (1e6·deg), lat INT)` with index `(skz, hnr)`; no rtree
- `meta(key,value)`: `stichtag` (YYYY-MM-DD), `source_url`, `rows`.
Import: `umfeld-at -addresses-import /path/to/unzipped-dir` (separate process), streaming, batched inserts in one transaction per 50 k rows, VACUUM at the end. Delete the zip/CSVs afterwards (disk discipline).

Endpoint (public tier, tokenless; add to `publicExactPaths` + `publicAllowListGolden`; `cacheablePath` ok):
```
GET /api/v1/addresses/search?q=<text>&bbox=w,s,e,n&c=lon,lat&limit=8
```
Matching lives in `addresses_index.go` (in-memory, one entry per street; `str_fts` is no longer
read): every street answers to canonical keys — name tokens, a split-off street-type suffix, glued
forms, place tokens (Gemeinde, Zustellort, Ortschaft). Canonical = lower case, diacritics off,
ß→ss, ae/oe/ue→a/o/u, ph→f, th→t, ck→k, y→i, ai→ei, doubled letters single, `…strasse`→`…str`,
`sankt`→`st`, `doktor`→`dr`, so the usual misspellings are exact matches ("handel-mazetti-promenade",
"mariahilferstrasse", "getreidegase", "stefansplatz"). Every word is a prefix match (AND). Stages,
first answer wins: 1 strict · 2 two adjacent words glued · 3 fuzzy (edit distance 1, 2 from 7
letters; only the last word may be an unfinished prefix) · 4 PLZ dropped · 5 one word unanswered
(the street must still be found by its name). Stages 3–5 set `approx` on the hits and the answer.
Numbers: the last 4-digit token that is a PLZ of the register is the PLZ (it also admits every
street of a Gemeinde that PLZ serves — "Hauptstraße 12, 4020 Linz" lives in 4040); the first other
number behind a word is the house number (`adr` rows of the 120 best streets, exact then prefix;
none like it → the street rows). Ranking: in_view first, then text penalty in buckets (first name
token > later token; whole name covered = `full`), then distance to `c` scaled by the size of the
Gemeinde — without `c` the bigger place first (the client sends no `c` when zoomed out to the
country). **OSM fallback:** named roads from `data/osm_proximity.db` (no motorways, no service
ways) that no register street of the same name lies within ~5 km of become street rows with
`src:"osm"`, one per name and place, labelled with the place of the nearest register street and
"· OpenStreetMap" in `sub` (ODbL; `attribution_osm` on the answer). Size line `addresses_index`
on `$UMFELD_PRIVATE_PREFIX/api/v1/debug/inflight` (~40 MB, fixed). Default `limit` 8, max 20.
q < 2 chars → empty results, 200. Tests: `addresses_index_test.go` (synthetic),
`ADDR_Q=<file|text> go test -tags fts5 -run TestAddressesLive -v .` prints answers from the local store.
Client (`ne_glyph.go` `remote`/`assemble`): 25 ms after a pause, 110 ms inside a burst; a text that
spells a whole street (`full`, ≥ 8 letters or two words, not also exactly a KG / admin unit) puts the
addresses on top; approx rows sit under "addresses · closest match".
Response (compact JSON, `writeJSON`-style, `Cache-Control: public, max-age=300`, CORS `*`):
```json
{"q":"hauptstr 12 inns","results":[
  {"kind":"address","label":"Hauptstraße 12","sub":"6020 Innsbruck","street":"Hauptstraße","hnr":"12",
   "plz":"6020","ort":"Innsbruck","gemeinde":"Innsbruck","gkz":"70101","lon":11.39,"lat":47.26,"in_view":true,"dist_km":0.4},
  {"kind":"street","label":"Hauptstraße","sub":"6020 Innsbruck · 231 addresses","street":"…","plz":"…","ort":"…","gemeinde":"…","gkz":"…","lon":…,"lat":…,"in_view":false,"dist_km":12.1}
 ],"stichtag":"2026-10-01","attribution":"© Österreichisches Adressregister, Stichtagsdaten vom 01.10.2026","license":"CC-BY-4.0","license_url":"https://creativecommons.org/licenses/by/4.0/"}
```
`sub` for a Hofname-only address: label = Hofname, sub = "plz ort". When `data/addresses.db` is absent: 200 with `results:[]` and `"unavailable":true` (the client hides the section). Licence row in `api_license.go` (`dataSourceLicenses`, id `bev_addresses`, Stichtag read from meta) + `/lizenzen` paragraph (`api_legal_pages.go`) + `llm.txt` line + `setLicenseHeaders` for the path. Register a size line on `$UMFELD_PRIVATE_PREFIX/api/v1/debug/inflight` if any cache is added (AGENTS.md inv. 10).

## 2. Outline collections — Bundesland · Bezirk · Gemeinde · Natura 2000 (`ne_glyph_admin.go`)

Anything with a name and a polygon can be selected in the find box exactly like a KG: the white
veil cut to it (every mode, no fill or stroke — the un-veiled land is the selection), the same card
(declared · observed · change · subsidy over the cells inside, see `docs/ne-glyph.md` §Area
selection), a kind tag on the row and the chip, `#adm=<code>` in the share link, a "recent" entry. Today: Statistik Austria
Gemeinden (CC BY 4.0, `data/gemeinden_v2.geojson.gz`, code = 5-digit GKZ), Bezirke (GKZ[0:3];
Wien 901–923 = Gemeindebezirke) and Bundesländer (GKZ[0]) dissolved from them, and the 353
Natura 2000 sites (EEA, `data/natura2000_at.geojson.gz`, code = SITECODE `AT…`, `t` = A birds /
B habitats / C both, `ha`).

Geometry is built **offline** by `scripts/ne_glyph_admin_build.py` (shapely: `buffer(0)`, sliver
closing `buffer(+2e-4).buffer(−2e-4)`, `simplify(tol, preserve_topology)`, min-area filters for
polygons and holes) into `data/ne_glyph_admin.json.gz` (committed, ≈1.6 MB gz): one key per
collection, rings in lon/lat. The server projects to frame units lazily once (`neGlyphAdminLoad`)
and memoises path strings per code (bounded 256, on `$UMFELD_PRIVATE_PREFIX/api/v1/debug/inflight` as `ne_glyph_admin`).

```
GET /api/v1/ne/glyph/admin                    {"bl":[{c,n,bbox}],"bz":[{c,n,bl,bbox}],"gem":[{c,n,bz,bbox}],
                                               "n2k":[{c,n,t,ha,bbox}],"kinds":{"bl":{kind,notice},…},"notice":…}   (1 h)
GET /api/v1/ne/glyph/admin/outline?c=701      {c,kind,n,d:"M…Z",bbox,notice}   d evenodd, frame units   (24 h)
```

### Adding a collection (e.g. national parks, Landschaftsschutzgebiete, Zählsprengel)

1. **Licence first** — a row in `api_license.go` (`dataSourceLicenses`) naming the `/ne/glyph/admin`
   endpoints, `/lizenzen` renders it. Non-commercial sources (GADM, WDPA) are out.
2. **Build**: one block in `scripts/ne_glyph_admin_build.py` → `out['<key>'] = [{c, n, bbox, r, …extras}]`.
   `c` must be unique across all collections (prefix it if numeric codes could collide). Pick a
   simplification tolerance by typical size (Gemeinde 0.0004° · Bezirk 0.0008° · Land 0.0012°);
   keep the file small — the client downloads the whole index (names + bboxes, not rings) once.
3. **Server**: one row in `neGlyphAdminKinds` (`ne_glyph_admin.go`): `Key`, `Kind` (label in the
   outline answer and the client's tag), `Code` regexp (what `c=` may look like), `Notice`, optional
   `Name` (when the name lives elsewhere, like Bezirke in `siDistrictNames`). Extras the client
   should see go on `neGlyphAdminUnit` + the index `row` struct. Done — the index and the outline
   endpoint are generic.
4. **Client** (`ne_glyph.go` JS, `admGet`/`admSearch`/`selectAdm`): nothing to wire — every array
   in the index becomes searchable and selectable, the kind label falls back to `kinds{}`. Optional
   polish in `ADM_UI[<key>]`: `k` pretty label, `o` rank offset, `sub(u)` sub-line, and an icon in
   `ICO` + `rowHTML` if the kind deserves one. The hash regexp in `applyHash` (`adm=`) must accept
   the new code shape.
5. **Docs**: `api_docs_public_ne.go` (ne.md endpoint lines + find-box paragraph), this file.

### Combining outlines

KGs and admin units / sites combine into one selection (`#kg=a,b;adm=c,d`, ≤ 50 members, ≤ 300 KGs): the
**+** at the end of the settled box adds, every tag and every row of the add list removes. Points never
combine. Code map: `docs/ne-glyph.md` §Area selection (*Combined selection*).

## 3. Points — addresses and names in the box

An address (`/addresses/search`) or a DLM toponym (`/toponyms/search?near=`) is a **point**: the
client flies to it (`goPoint`), draws a dot that stays (`g.pt`, screen-sized via `ptSync`, the
tour's one-shot ping on arrival), asks `/ne/glyph/cell?lon&lat` for the res-12 cell's `kgs{}` and
selects the dominant KG around it (`selectKG(code, false, keep=true)` — outline and card, **no veil**
(`veilSync` skips `sel` while `pt` is set: a point is browsing, not a selection), but the
history entry and the hash stay the point's: `#pt=lon,lat;pn=<label | sub>`). Until the KG is
known, or if the lookup fails, the res-7 hexagon under the point stands in.

## 4. Landing rule

Every find landing (KG, admin unit, point — typed or from a bare `#kg=`/`#adm=`/`#pt=` link) ends in
full resolution when the fit is deep enough (`landFull`: waits for the animation and any outline
refit, then `hiOn()` if zoom ≥ `HI_MIN`, unless the overview was asked for via ⤢/`;nofull` or the
user moved meanwhile). Small units lift `ZMAX` to `HI_ZMAX` before the fit like `selectKG` does.
