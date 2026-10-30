# umfeld-at

One Go binary serving **NE cells** — H3-keyed declared/observed land-use
statistics for Austria and their **change over time** — plus a set of tokenless
public overlays keyed by point, bounding box or administrative code.

The service holds **no cadastre**: no parcels, no building footprints, no
parcel index. Anyone who needs cadastral geometry assembles it client-side from
the BEV vector tiles (CC BY 4.0) with
[vtcseamless](https://github.com/raffopenssh/vtcseamless) (preset `bevdirect/`)
or [vtcseamless-py](https://github.com/raffopenssh/vtcseamless-py). Peers run
those builds and report the resulting H3 chunks here; the server aggregates the
reports into the NE product.

## What it serves

**NE cells** (`docs/ne-cells.md`, `docs/ne-change.md`, `docs/ne-public-api.md`)

- Declared land use per H3 cell (res 9 chunks, res 12 rows), derived by peers
  from BEV Kataster tiles with `bevdirect@<tag>`.
- Observed land cover per cell from the observed-layer peer (LiDAR / satellite),
  joined at serve time.
- Change over time: per-chunk first-seen baseline, `since_last` deltas,
  confirmed when a digest is sighted twice.
- Unit statistics (`/api/v1/ne/stats/unit`, `stats-envelope/1`), a filter
  grammar (`/api/v1/ne/filter`), MVT / PMTiles tiles for QGIS, and the landing
  glyph at `/`.

**Public overlays** (one call each, never keyed by parcel)

| Overlay | Source | Docs |
|---|---|---|
| `/api/v1/context?lon=&lat=` — bundle of everything below for a point | — | `docs/api-tiers.md` |
| OSM roads / rail / transit / water lines, points, water areas | OpenStreetMap (Geofabrik) | `docs/osm-proximity.md` |
| Toponyms (BEV DLM-Namen) | BEV | `docs/toponyms.md` |
| Natura 2000 sites | EEA | `docs/natura2000.md` |
| Protected areas | WDPA | — |
| Land prices per Gemeinde / Bezirk / Bundesland | Statistik Austria | `docs/land-prices.md` |
| RIS law references per Katastralgemeinde | RIS | `docs/legal-ris.md` |
| KG universe: codes, names, admin hierarchy, bbox | BEV | `docs/api-tiers.md` |
| EDM register lookup (Gemeinde / PLZ / Ortschaft / KG) | Statistik Austria / BEV | — |
| BEV address register search | BEV | `docs/glyph-find-contract.md` |

## Quick start

```bash
CGO_ENABLED=1 go build -tags fts5 -o umfeld-at .   # -tags fts5 is mandatory
go test -tags fts5 ./...                            # ~1–2 min
./umfeld-at                                         # listens on :8000 (PORT overrides)
curl localhost:8000/api/v1/docs/llm.txt
```

Production install (Debian/Ubuntu, systemd):

```bash
git clone https://github.com/raffopenssh/umfeld-at.git && cd umfeld-at
./scripts/install.sh
```

The script installs the build dependencies (Go ≥ 1.22, gcc, sqlite3, zstd),
builds the binary, writes a template `/etc/umfeld-at.env` (mode 0600) if none
exists, and installs/starts the `umfeld-at` systemd unit. The NE store
(`data/ne_cells/`) starts empty and fills from peer reports.

### Environment file (`/etc/umfeld-at.env`)

All deployment-specific values live in the environment; the code ships neutral
defaults and the repository names no host. See the header of `peers.go` and
`api_tiers.go`.

| Variable | Meaning |
|---|---|
| `UMFELD_PUBLIC_ONLY=1` | enforce the public allow-list on `/api/v1` |
| `UMFELD_PUBLIC_URL` | scheme+host of this instance (share links, attribution) |
| `UMFELD_PRIVATE_PREFIX` | path prefix of the private tier (default `/k`) |
| `UMFELD_TOKEN`, `UMFELD_TOKEN_NAMES` | private-tier tokens and their names (parallel, comma-separated) |
| `CONTRIBUTOR_TOKEN`, `CONTRIBUTOR_TOKEN_NAMES` | report-only peer tokens for `/contrib/…` |
| `NE_WRITER` | token name allowed to upload the observed layer (`/{kg}/obs`) |
| `NE_WRITE_BUDGET_MB`, `NE_WRITE_BUDGET_MB_OVERRIDES` | daily write budget per token (`name:MB,…`) |
| `UMFELD_PEER_OBS_NAME` / `_API` / `_WEB` | observed-layer peer: name, server-side base URL, browser base URL |
| `UMFELD_PEER_SUB_NAME` / `_API` | subsidy-claims peer (INVEKOS per cell) |
| `UMFELD_PEER_MAP_NAME` | the map client that reports declared chunks (docs only) |
| `PORT`, `GOMEMLIMIT` | listener port (default 8000), Go memory limit |

Leave the peer variables empty to run without peers; the NE product then shows
the declared layer only.

## Data imports

The overlay databases are built once with flags on the same binary (each runs
the import, then exits; see `main.go`):

| Flag | Builds |
|---|---|
| `-osm-import` | `data/osm_proximity.db` from `data/osm_geofabrik_data/austria.gpkg` (roads, rail, transit, water, places) |
| `-osm-import-areas` | water polygons into the same DB (keeps lines/points) |
| `-toponyms-import [-toponyms-gpkg F] [-toponyms-url U]` | `data/toponyms.db` from the BEV DLM-Namen GPKG (downloaded when missing) |
| `-addresses-import DIR` | `data/addresses.db` from an unzipped BEV Adressregister directory |
| `-kg-universe-build` | `data/kg_universe.json.gz` + `data/kg_universe_bbox_diff.json.gz` from the `kg` table ∪ VGD envelopes |
| `-ne-train-dict lu\|obs [-ne-dict-n N]` | the NE zstd dictionary `data/ne_cells/dict/{layer}-{N}.zd` |

Static overlays (Natura 2000, WDPA, Statistik Austria polygons and prices, EDM
registers, KG universe, RIS parcel refs) ship in `data/` and load at startup.

## API overview

Three tiers on one mux (`docs/api-tiers.md`, `api_tiers.go`):

| Tier | Prefix | Who | What |
|---|---|---|---|
| public | `/api/v1/…` | anyone, no token | allow-listed overlays + NE reads keyed by point / bbox / admin code / H3 |
| contrib | `/contrib/api/v1/ne/…` | peers with `CONTRIBUTOR_TOKEN` (or a private token) | NE reports, manifest, revisit, `{kg}/chunks`, `{kg}/declared`, `{kg}/obs` (writer only), tiles |
| private | `$UMFELD_PRIVATE_PREFIX/…` | `UMFELD_TOKEN` | everything, incl. NE ops and `/api/v1/debug/*`. No token → the same 404 as an unknown route |

Selected public routes:

```
GET  /api/v1/context?lon=&lat=                      overlay bundle for a point
GET  /api/v1/ne · /ne/manifest · /ne/{kg} · /ne/chunk/{h3} · /ne/cell/{h3} · /ne/cells
GET  /api/v1/ne/stats/unit?kg=|h3=|bbox=|adm=&f=    one row per unit (stats-envelope/1)
GET  /api/v1/ne/filter?…                            cells matching a filter expression
GET  /api/v1/ne/mvt/{z}/{x}/{y}.pbf · /ne/pmtiles   cells for QGIS
GET  /api/v1/osm/geometry?bbox=&cat= · /osm/point · /osm/stats
GET  /api/v1/toponyms/search?q= · /toponyms/near · /kg/{kg}/toponyms
GET  /api/v1/natura2000/point · /natura2000/search · /natura2000/site/{code}
GET  /api/v1/land_prices/gemeinde/{code} · /land_prices/point · /land_prices/predict
GET  /api/v1/legal/kg/{kg} · /legal/search · /legal/stats
GET  /api/v1/kgs · /kgs/bbox_changes · /lookup · /addresses/search
GET  /api/v1/search/municipalities · /search/gadm · /search/protected_area · /search/address_osm
GET  /api/v1/license · /cache/policy · /feedback
POST /contrib/api/v1/ne/{kg}/report                 peer NE report
```

Every JSON answer is compact unless `?pretty=1`; responses are gzip-compressed
and carry licence/attribution headers (`docs/payload-caching-contract.md`).

### Documentation served by the binary

| Path | Content |
|---|---|
| `/api/v1/docs/llm.txt` | plain-text API reference for agents and humans |
| `/api/v1/docs/ne.md` | NE cell contract in public vocabulary (`docs/ne-cells.md`) |
| `/api/v1/docs/ne-change.md` | NE change protocol (`docs/ne-change.md`) |
| `/api/v1/docs/stats-envelope.md` | the `stats-envelope/1` answer shape |
| `/api/v1/docs/qgis-layer.md` | cells as MVT / PMTiles for QGIS |
| `/api/v1/docs/cache-client.py`, `.js` | reference clients honouring `/api/v1/cache/policy` (`clients/`) |

Operator and contributor docs live in `docs/`; `AGENTS.md` is the guide for
agents working on the code.

## Repository layout

```
main.go, api*.go          server, tiers, overlays
ne_*.go, api_ne*.go       NE store, change, glyph, filter, tiles
ne_cells/                 the NE algorithm (Python; the only implementation, mirrored in vtcseamless-py)
tests_py/                 Python tests for ne_cells
docs/                     contracts and per-topic docs
clients/                  cache clients, bevdirect / vtcseamless notes
scripts/                  install.sh, imports, sync checks
data/                     shipped static overlays (large DBs are built locally and never committed)
```

## Licence

Code: MIT (see `LICENSE`).

Data redistributed by the API keeps its upstream licence; `GET /api/v1/license`
lists every source with licence, attribution and date (`api_license.go`):

- BEV Kataster, Adressregister, DLM-Namen, VGD — CC BY 4.0 (© BEV)
- OpenStreetMap (Geofabrik extract, Nominatim) — ODbL 1.0
- Statistik Austria (Gemeinden, land prices) — CC BY 4.0
- Natura 2000 (EEA) — EEA standard re-use policy (attribution)
- WDPA (UNEP-WCMC / IUCN) — WDPA terms (non-commercial, attribution)
- RIS — Rechtsinformationssystem des Bundes — CC BY 4.0
- INVEKOS (AMA / BML, aggregated by the subsidy peer) — CC BY 4.0
- basemap.at Orthofoto — CC BY 4.0
- GADM v4.1 — academic / non-commercial use

The only licensed source for BEV cadastral content is the public CC BY vector
tile service (`kataster.bev.gv.at/…/tiles/{kataster|symbole}/{z}/{x}/{y}.pbf`).
Nothing here is ingested from the Katasterservice search/info API.

Security reports: see `SECURITY.md`.
