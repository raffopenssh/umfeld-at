# umfeld-at — Agent Guide

One Go binary (`umfeld-at`; systemd unit `umfeld-at`; :8000). **This service holds no cadastre.**
No parcels, no building footprints, no Benützungsart polygons, no KG files, no parcel index.
Anyone who needs cadastre geometry (peers, us) assembles it client-side from BEV tiles with
**vtcseamless / bevdirect** (https://github.com/raffopenssh/vtcseamless, preset `bevdirect/`;
Python: https://github.com/raffopenssh/vtcseamless-py). Never try to serve, download or build
parcel data here.

The service does two things:

1. **NE cells** — H3-keyed declared/observed land-use product and **land-use change over
   time**, fed by peer reports. The core. `docs/ne-cells.md`.
2. **Public overlays** keyed by point / bbox / admin code: `/api/v1/context`, OSM
   lines/points/water areas, toponyms, Natura 2000 sites, WDPA, land prices per Gemeinde,
   RIS law refs per KG code, KG universe. Never keyed by parcel.

## Vocabulary

- **`/contrib/…`** = advertised prefix for **NE epoch reports**
  (`POST /contrib/api/v1/ne/{kg}/report`, `api_ne_reports.go`): digests of H3 chunks a peer
  assembled from BEV tiles with bevdirect. **"Contributions"** = those reports. Over time they
  are the **change** signal (per-chunk first-seen baseline + `since_last`, confirmed when a
  digest is sighted twice).
- **Peers** (all configured in the env file, `peers.go`): the **observed-layer peer**
  (`UMFELD_PEER_OBS_*`; LiDAR/satellite per cell; its observers report as `peer-vm-1`-style
  ids), the **subsidy peer** (`UMFELD_PEER_SUB_*`; INVEKOS claims per cell), the **map peer**
  (`UMFELD_PEER_MAP_NAME`; reports declared chunks in viewport-sized bursts). Peers hold a
  `CONTRIBUTOR_TOKEN`; the NE **writer** (`/{kg}/obs` NEC1+NEO1 upload) is whichever token is
  *named* `NE_WRITER` (`isNEWriter`), on `/contrib` or the private prefix.
- **Tiers:** public `/api/v1/*` (tokenless, allow-listed); private `$UMFELD_PRIVATE_PREFIX/…`
  (default `/k`; `UMFELD_TOKEN`; NE ops + debug + `osm/geometry?kg=`); `/contrib/…`
  (contributor token, NE report paths only).
- "Are contributions coming in / is change computed?" → `docs/ne-cells.md` §triage. One-liners,
  no code reading.

## Doc conventions

- AGENTS.md ≤ ~200 lines. New hard rule → one line under *Invariants* + pointer.
  Narrative/measurements → `docs/<topic>.md` + a routing-table row.
- Client-facing docs: `llm.txt` (`/api/v1/docs/llm.txt`, `api_docs_public.go`) and the embedded
  NE docs (`/api/v1/docs/ne.md`, `ne-change.md`, `stats-envelope.md`, `qgis-layer.md`;
  `api_docs_public_ne.go`). They are served through `peerText` — write `{OBS_NAME}`,
  `{SUB_NAME}`, `{MAP_NAME}`, `{PUBLIC_URL}` and `<private>` instead of names, hosts, prefixes.
  Update them when API behaviour changes.
- No host names, peer names, token names or machine paths anywhere in the repo.
- Commit with descriptive messages before returning to the user.

## Build · test · deploy

```
CGO_ENABLED=1 go build -tags fts5 -o umfeld-at .    # -tags fts5 is mandatory
go test -tags fts5 ./...                              # ~1–2 min
sudo systemctl restart umfeld-at                      # ready in seconds
journalctl -u umfeld-at -f
```
Deploy = build in place + restart (`scripts/install.sh` for a fresh machine). Startup log must
show `Search index ready: 7850 KGs`. Env: `/etc/umfeld-at.env` (0600; template in
`scripts/install.sh`): `UMFELD_PUBLIC_ONLY`, `UMFELD_PUBLIC_URL`, `UMFELD_PRIVATE_PREFIX`,
`UMFELD_TOKEN` + `UMFELD_TOKEN_NAMES` (parallel lists), `CONTRIBUTOR_TOKEN` +
`CONTRIBUTOR_TOKEN_NAMES`, `NE_WRITER`, `NE_WRITE_BUDGET_MB`, `NE_WRITE_BUDGET_MB_OVERRIDES`
(`name:MB,…`), `UMFELD_PEER_OBS_NAME/_API/_WEB`, `UMFELD_PEER_SUB_NAME/_API`,
`UMFELD_PEER_MAP_NAME`; the unit sets `PORT=8000`, `GOMEMLIMIT`. Long passes (`-ne-train-dict`,
`-osm-import`, `-toponyms-import`, `-kg-universe-build`) run as a separate process.

## Invariants

Tiers (`docs/api-tiers.md`)
1. A public route must be in `publicExactPaths`/`publicPrefixPaths` AND `publicAllowListGolden`,
   keyed by point/bbox/admin code — never by parcel, never echoing parcel ids/areas/geometry.
   Dual-tier handlers branch on `isPrivateTier(r)`. Attribution per source (`AttributionMetaFor`).
2. Private prefix without a valid token → 404 (never 401, no hints). `CONTRIBUTOR_TOKEN` opens only
   `contributorPathAllowed` paths (NE: root, manifest, reports, revisit, `{kg}`,
   `{kg}/report|chunks|obs|declared`, tiles), advertised as `/contrib/…` — never hand the private
   prefix to a contributor. Every peer write is bounded: body caps (report 8 MB, chunks 2 MB,
   obs 32 MB), 2 POST/s per token, `neWriteBudgetGate` (4 GiB/day per token, 429), string
   fields ≤ 64 chars; NE routes log via `apiMiddleware` (`peer=contrib:<name>`). `/{kg}/obs`
   additionally needs `isNEWriter`.
3. No handler reads parcel data; the retired cadastre paths are plain 404. `osm/geometry` takes
   `bbox=` (and `kg=` on the private tier); there is no `parcel=`, water is `cat=water,water_area`
   — clip client-side with bevdirect if parcel lines are wanted.
4. KG-level data is a peer contract: `kg` table (→ `kg_bbox_index.go`), `data/kg_universe.json.gz`,
   `GET /api/v1/kgs` with `kg_count=7850` and a stable `universe_hash` peers agree on.
   Changing the set = new epoch + `-kg-universe-build` + tell peers. Per-KG `bbox` = VGD envelope
   ∪ previous bbox (`bbox_revision`, never shrinks; `data/bev_vgd_admin.json.gz`); a bbox change =
   `-kg-universe-build` → `data/kg_universe_bbox_diff.json.gz` → `GET /api/v1/kgs/bbox_changes`
   (slivers for peers), `docs/api-tiers.md` §bbox revision.

NE (`docs/ne-cells.md`, `docs/ne-change.md`, `docs/ne-public-api.md`)
5. `ne_cells/` (python) is the ONLY algorithm implementation — never port it to Go.
   `ne_cells/{algo,canon,ns_groups,pack,__init__}.py` are frozen and mirrored byte-identically
   in vtcseamless-py → run `scripts/ne_cells_sync_check.sh` after touching `ne_cells/`.
6. Declared layers come only from peers' bevdirect builds (`NEC1+NEO1` reports). We build nothing
   locally. The bevdirect source string `bevdirect@<tag>` is contractual.
7. Public NE path (`api_ne_public.go`, `ne_store.go`, `ne_h3.go`, `api_ne_vocab.go`,
   `api_docs_public_ne.go`, `ne_change.go`, `api_ne_change.go`): word ban (`TestNEPublicWordBan`),
   no private prefix in public docs/answers, zero SQL, inputs `.nec/.obs` → `.nex` only via
   `neStore.convert` (verify → write → delete), binary layouts frozen, public K rows bucketed.
7a. Declared = the view (log → bev → lu, `ne_change_declared.go`): glyph, `layers=declared`, bare-NEO1
   validation, manifest `declared{}` — `lu` is the frozen fallback, never a dependency (ne-change.md §4b).
7b. Unit stats (`api_ne_stats_unit.go`) and `/ne/filter` count the SAME cells: exact H3 centres and
   `CellAreaM2`, membership from the declared view (kg), ancestry (h3, scan box padded for child spill),
   centre-in-box (bbox); `kg=`/`h3=` must agree with the observed-layer peer cell for cell. Export
   links carry `src=glyph`; the gate (`export_gate.go`) counts every export, enforces only with
   `EXPORT_GATE_ENFORCE=1` — keys, tiers and budgets stay out of public docs.
8. Change: `digest_ap` lives in `ne_cells/change.py` only (`pack.py` untouched); the chunk log
   stores rows with register bytes zero and never a K row; no delta reads `gk`/`n_parc`;
   `{kg}.chunks.zst` is append-only; public figures count `confirmed` only; lossy builds are
   not change (`neBestCoverage`, §coverage).
9. `data/ne_cells/` (`ne_index.db`, `.nex`, `.chunks.zst`, `.reports.jsonl`, `dict/`) is the only
   irreplaceable state of a deployment. Never rewrite `.nex` outside `neStore.convert`, never
   delete reports or chunks, never commit it (the shipped `dict/` is the exception).

Cross-cutting
10. Every process-wide cache needs a bound AND a size line on `$UMFELD_PRIVATE_PREFIX/api/v1/debug/inflight`
    (`docs/memory-contract.md`). Expensive stats use `StatsMemo` (stale-while-revalidate).
11. No per-row SQL in a loop — batch. No `LIKE '%x%'` on `kg` columns — `ResolveKGCodes` →
    `kg_code IN (…)`. KG lookups by bbox go through the in-memory KG bbox index.
12. `writeJSON`/`apiResponse`/`jsonEnc` (compact unless `?pretty=1`), `coord` (7 decimals), never
    set `Content-Encoding` in a handler (global gzip); live-value endpoints go in `cacheablePath`'s
    exclusion list (`docs/payload-caching-contract.md`).
13. KG codes are 5 digits, zero-padded, everywhere; a new KG-code param → `kgCodeParams`
    (`kg_code.go`). `plz`, `gemeinde`, `district` are NOT padded.
14. Licensing for BEV-derived data (NE, peers): the only legal source is
    `kataster.bev.gv.at/at.gv.bev.kataster/tiles/{kataster|symbole}/{z}/{x}/{y}.pbf` (CC BY 4.0
    VTC; parcels z15, footprints/landuse z16). The Katasterservice search/info JSON API is NOT
    CC BY — never ingest from it, tell peers the same. `BEVNotice()` carries the year.

## Routing table — read before touching

| Topic | Read | Code |
|---|---|---|
| NE cells: product, container, triage, observers, coverage rule | `docs/ne-cells.md` (§triage first) | `ne_cells/`, `api_ne.go`, `api_ne_manifest.go`, `tests_py/` |
| NE reports `/contrib/api/v1/ne/{kg}/report` (baseline, `since_last`, lossy) | `docs/ne-cells.md` §triage, §observers, §coverage | `api_ne_reports.go`, `peer_ne_ready.go`, `data/ne_cells/{kg}.reports.jsonl` |
| NE change (`{kg}.chunks.zst`, `digest_ap`, confirmed/candidate, `chg` layer, `/ne/stats`, landing chart) | `docs/ne-change.md` | `ne_change.go`, `ne_change_declared.go`, `ne_change_passes.go`, `api_ne_change.go`, `api_ne_revisit.go`, `ne_cells/change.py` |
| Glyph find box (`/`: KGs · Gemeinde/Bezirk/Land · Natura 2000 outlines · BEV addresses · toponyms; adding an outline collection) | `docs/glyph-find-contract.md` | `ne_glyph_admin.go`, `addresses.go`, `scripts/ne_glyph_admin_build.py`, find block in `ne_glyph.go` |
| Landing glyph (dots, hex faces, full-res tiles, views, trees, relief, subsidy, share links) · **soak test** | `docs/ne-glyph.md` (code map; §soak = one-line browser check of every state) · `docs/ne-change.md` §6a–6c | `ne_glyph.go`, `ne_glyph_tile.go`, `ne_glyph_dif.go`, `ne_glyph_sub.go`, `ne_glyph_subpoly.go`, `ne_glyph_roads.go`, `ne_glyph_soak.js` |
| **Glyph filter** (chips over cells at LOD 0/1, `;f=` grammar = `/api/v1/ne/filter`, `fidx` bins per 0.025°, Worker veil, JS/Go parity test) | `docs/ne-filter.md` (status + design), `docs/ne-glyph.md` row *FILTER* | `ne_filter_expr.go`, `ne_glyph_fidx.go`, `ne_glyph_filt.go`, `ne_glyph_filter.js`, `api_ne_filter.go`, `ne_filter_test.go`, `testdata/ne_filter_harness.js` |
| **Unit stats / export** (`/api/v1/ne/stats/unit?kg=|h3=|bbox=|adm=|pt=|share=&f=`, `stats-envelope/1`, observed + subsidy peers federated, flat/csv for sheets, the glyph's link ▾ menu) · **export gate** (count-only until `EXPORT_GATE_ENFORCE=1`; `EXPORT_KEYS` tiers; never in public docs) | `docs/stats-envelope.md` (public, served at `/api/v1/docs/stats-envelope.md`), `docs/ne-glyph.md` row *EXPORT* | `api_ne_stats_unit.go`, `export_gate.go`, `ne_glyph_export.go`, `api_ne_stats_unit_test.go` |
| **QGIS cell layer** (MVT `/api/v1/ne/mvt/{z}/{x}/{y}.pbf` + `mvt.json` + `mvt/status`, PMTiles `/ne/pmtiles?bbox=`, also under `/contrib` and the private prefix; res 7→9 from the national table `ne_mvt_nat.go` (data/ne_mvt/nat.bin, 62 MB RAM, ~4 min build, ≥ 6 h apart), res 10→12 from fidx bins; tile LRU, bin build queue, archive job worker; gate `NE_MVT_ACCESS` per name bbox + tiles/km² per day, counted until `NE_MVT_GATE_ENFORCE=1`; gates never in public docs) | `docs/qgis-layer.md` (public, `/api/v1/docs/qgis-layer.md`) | `ne_mvt.go`, `ne_mvt_nat.go`, `ne_mvt_enc.go`, `ne_mvt_gate.go`, `ne_mvt_jobs.go`, `ne_pmtiles.go`, `ne_mvt_test.go` |
| INVEKOS subsidy layer (subsidy-peer H3 + IVP1 outline contracts, `v=sub|subc` tiles, `/ne/glyph/sub`) | `docs/invekos-subsidy-layer.md` | `ne_glyph_sub.go`, `ne_glyph_subpoly.go` |
| NE public read API (H3-keyed, tokenless, `.nex`, word ban, K bucketing) | `docs/ne-public-api.md` | `ne_store.go`, `ne_h3.go`, `api_ne_public.go`, `api_ne_vocab.go`, `api_docs_public_ne.go` |
| Tiers, tokens, peers, `/api/v1/context`, public docs | `docs/api-tiers.md` | `api_tiers.go`, `peers.go`, `api_context.go`, `api_docs_public.go` |
| Impressum / Datenschutz / Lizenzen (`/impressum`, `/datenschutz`, `/lizenzen`) — keep in step with what the service really logs/stores | header comment of the file | `api_legal_pages.go`, `LICENSE` |
| KG universe `/api/v1/kgs` | `docs/api-tiers.md` (last section) | `kg_universe.go`, `kg_universe_bbox.go`, `kg_bbox_index.go` |
| Licensing, attribution, client caching | `docs/payload-caching-contract.md` | `api_license.go`, `api_cache_policy.go`, `clients/` |
| bevdirect / vtcseamless (external) | `clients/bevdirect/README.md`, `clients/vtcseamless-py/README.md` | — |
| Caches, LRUs, memos | `docs/memory-contract.md` | `stats_memo.go` |
| OSM lines/points/areas, geocoding | `docs/osm-proximity.md` | `osm_proximity.go`, `osm_remote_grid.go` (250 m remoteness grid for the roads ink), `osm_areas.go`, `api_osm_proximity.go`, `api_osm_geocode.go` |
| Toponyms (DLM-Namen) | `docs/toponyms.md` | `toponyms.go`, `api_toponyms.go`, `mgi_transform.go` |
| Natura 2000 sites | `docs/natura2000.md` | `natura2000.go` |
| Land prices (Gemeinde/Bezirk/Bundesland, predict from caller features) | `docs/land-prices.md` | `land_prices.go`, `land_prices_aggregate.go` |
| RIS law refs per KG code | `docs/legal-ris.md` | `api_legal_refs.go` |
| KG code padding | `docs/kg-codes.md` | `kg_code.go` |
| Latency / payload rules | `docs/latency-contract.md`, `docs/payload-caching-contract.md` | `api.go`, `api_compress.go`, `debug_pprof.go` |

## Quick reference

```
GET  /api/v1/context?lon=&lat=                                   public overlay bundle for a point
GET  /api/v1/ne/…                                                public NE (docs: /api/v1/docs/ne.md, ne-change.md)
GET  /api/v1/ne/stats/unit?kg=|h3=|adm=|share=#…&format=csv      one row per unit + peers (docs/stats-envelope.md)
GET  /api/v1/ne/mvt/{z}/{x}/{y}.pbf · /ne/pmtiles?bbox=           cells for QGIS, live tiles / offline archive (docs/qgis-layer.md)
POST /contrib/api/v1/ne/{kg}/report                              peer NE report (contributor or private token)
GET  /contrib/api/v1/ne/revisit?observer=&limit=                 revisit hints for peers: pending, stale, no_log, obs_on_index (ne-change.md §5b)
GET  /contrib/api/v1/ne/{kg}/declared                            the declared VIEW cell set (log → bev → lu) peers build bare NEO1 on (ne-change.md §4b)
GET  $P/api/v1/ne/manifest | $P/api/v1/ne/reports | $P/api/v1/ne/stats?kg=   private NE ops ($P = UMFELD_PRIVATE_PREFIX)
GET  $P/api/v1/debug/inflight                                    in-flight, pool, cache sizes
GET  /api/v1/osm/geometry?bbox=w,s,e,n&cat=road,rail,water,water_area      (private tier adds kg=)
GET  /api/v1/kgs · /api/v1/kgs/bbox_changes?min_delta= · /api/v1/lookup · /api/v1/search/municipalities?q= · /api/v1/search/protected_area
GET  /api/v1/toponyms/search?q= · /api/v1/toponyms/near · /api/v1/natura2000/point?lon=&lat=
GET  /api/v1/land_prices/gemeinde/{code} · /api/v1/land_prices/point · /api/v1/legal/kg/{kg}
GET  /api/v1/license · /api/v1/cache/policy · /api/v1/feedback
curl 'localhost:8000/debug/pprof/goroutine?debug=2'              wedge triage
```

## Data facts

- KG universe: exactly 7 850 KGs (`kgUniverseContract`), the same set at every peer.
- IDs: KG `63349` (5 digits, padded). Parcel/footprint ids exist only inside peers' bevdirect
  output and NE reports, never in our DBs or responses.
- DBs (built locally, never committed): `data/search_index.db` (tables `kg plz_kg bundesland
  index_meta landuse_types kg_landuse fts_admin*` only; `schema/search_index.sql`),
  `data/osm_proximity.db` (lines/points/areas + rtrees), `data/toponyms.db`, `data/addresses.db`,
  `data/feedback.db`, `data/ne_cells/` (the product), `data/ne_fidx/` (filter index cache,
  ≤ 2 GB, rebuildable — delete freely), `data/osm_remote_grid.bin` (3 MB, rebuilt in a second),
  `data/ne_mvt/nat.bin`.
- Shipped static data: `natura2000_at.geojson.gz`, `nationalparks_wdpa.geojson`, `gemeinden_v2.geojson.gz`,
  `statistik_austria*`, `edm_*.json`, `gadm41_AUT_3.json`, `kg_universe.json.gz`, `kg_universe_bbox_diff.json.gz`,
  `kg_to_gemeinde.json`, `kg_meta_dump.sql.gz`, `bev_vgd_admin.json.gz`, `legal_parcel_refs.csv`,
  `ne_glyph_admin.json.gz`, `ne_glyph_austria_r7.json`, `ne_cells/dict/lu-1.zd`.

## File map

| Concern | Files |
|---|---|
| Server / routing / tiers | `main.go`, `api.go`, `api_compress.go`, `api_tiers.go`, `peers.go`, `kg_code.go`, `debug_pprof.go`, `egress_stats.go`, `rate_limiter.go`, `sqlite_driver.go`, `search_index.go` |
| NE | `ne_cells/`, `api_ne*.go`, `ne_store.go`, `ne_h3.go`, `ne_change*.go`, `ne_glyph*.go`, `ne_filter_expr.go`, `ne_mvt*.go`, `ne_pmtiles.go`, `peer_ne_ready.go`, `stats_memo.go`, `tests_py/`, `scripts/ne_cells_sync_check.sh`, `scripts/ne_diff_declared_vs_observed.py` |
| Public overlays | `api_context.go`, `osm_proximity.go`, `osm_areas.go`, `osm_remote_grid.go`, `api_osm_*.go`, `toponyms.go`, `api_toponyms.go`, `addresses.go`, `mgi_transform.go`, `natura2000.go`, `land_prices*.go`, `api_legal_refs.go`, `kg_universe*.go`, `kg_bbox_index.go`, `api_search_municipalities.go`, `api_spatial_kgs.go`, `api_edm_lookup.go` |
| Docs / meta | `api_docs_public*.go`, `api_license.go`, `api_cache_policy.go`, `api_feedback.go`, `api_legal_pages.go`, `export_gate.go` |
| Ops | `umfeld-at.service`, `scripts/install.sh`, `scripts/parse_statistik_austria.py`, `scripts/ne_glyph_*.py` |

## Git policy

**Commit:** Go source, scripts, `docs/`, `data/*.json`, `data/*.geojson[.gz]`, `schema/`.
**Never commit:** `data/ne_cells/` (except `dict/`), `data/*.db*`, `data/bev_namen/`, binaries,
tokens or env files, anything > 100 MB.
