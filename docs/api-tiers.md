# API tiers — public (point-keyed overlays + NE reads), contrib (peer writes), private (everything)

The **public** surface serves only context layers *around* a place: Statistik
Austria (Gemeinden, prices), EEA Natura 2000, WDPA, OSM proximity, BEV
DLM-Namen, RIS, EDM register lookups, the KG universe, and the H3-keyed NE
read API. **No cadastre** — no parcels, footprints, landuse polygons, EZ, GNR,
and no endpoint that reads parcel data. Clients that need the cadastre run
bevdirect (https://github.com/raffopenssh/vtcseamless) themselves and send us
points or H3 chunk reports.

Everything the server has is available **1:1 under the private prefix**
(`UMFELD_PRIVATE_PREFIX`, default `/k`) with a bearer token from `UMFELD_TOKEN`.
Report-only peers carry a `CONTRIBUTOR_TOKEN` that opens the NE report paths
only — and they reach them under the **advertised `/contrib/…` prefix**
(`contribTierPrefix`: same handlers, internal header `X-Umfeld-Tier: contrib`,
`contributorPathAllowed` only, 404 for everything else even with a private
token), so the private prefix is never written into a peer's code or docs.
Code: `api_tiers.go`, `peers.go`, `api_context.go`, `api_docs_public.go`,
public parts of `api_license.go` / `api_cache_policy.go`.

## Rules

1. **A route is public only if it is in `publicExactPaths`/`publicPrefixPaths`
   AND in `publicAllowListGolden` (`api_tiers_test.go`).** Adding one is a
   licensing decision: the handler must not read parcel data and must not echo
   parcel ids, areas or geometry.
2. **Public = point/area/code-keyed.** Inputs are `lon,lat`, bbox, H3 index,
   Gemeinde / Bezirk / Bundesland / KG *code*, or caller-supplied features
   (`area_sqm`, `building_count`, `footprint_area_sqm`, `landuse_areas=ns:m²`).
   Never add a `parcel=`/`parcel_id` parameter to a public route.
3. Handlers that exist on both tiers branch on `isPrivateTier(r)`:
   `land_prices/{gemeinde,bezirk,bundesland}` (public → Statistik Austria
   series only, `landPriceStatistikOnly`), `osm/geometry` (public → `bbox=`
   only; the private tier adds `kg=`; there is no `parcel=` and no
   `cat=water_parcels` on any tier), `toponyms/{id}` (public → no `parcel{}`
   block), `/license`, `/cache/policy`, `/docs*`, `/llm.txt`, `/`.
4. **Attribution is per source** (`sourceIDForPath` → `AttributionMetaFor`,
   `setLicenseHeaders`). A blanket "© BEV Kataster" is wrong on public
   responses; `apiResponse` uses the path-aware variant.
5. Private tier answers **404, never 401** without a valid token, with the same
   body as an unknown route. No redirects/hints from retired cadastre paths —
   they are plain 404 on every tier. `X-Robots-Tag: noindex` on the private
   prefix. Plain-text docs under the private prefix get the prefix prepended to
   every example path (`rewriteDocPrefixes`) — JSON is never rewritten, so
   links inside JSON bodies (`/api/v1/...`) need the prefix prepended by the
   client.
6. `tierMiddleware` is the OUTERMOST layer: it strips the prefix before cache /
   gzip / egress, so `cacheablePath`, ETags and egress stats key on the
   handler path.

## NE cells (exception to "no cadastre-derived data")

`/api/v1/ne/*` GET is public: per-H3-cell aggregates only (no id, number or
geometry recoverable; `docs/ne-public-api.md` §0b). Writes stay behind a token:
`/report`, `/chunks` for every private or contributor token; `/{kg}/obs` only
for the token *named* by `NE_WRITER` (`isNEWriter`) — normally the
observed-layer peer's contributor token.
Attribution source id `ne`.

### Contributor surface — what a leaked contributor token can do, and cannot

Reach: `contributorPathAllowed` (api_ne_reports.go) — NE root, manifest,
reports overview, revisit, `GET /{kg}`, `{kg}/report`, `{kg}/chunks`,
`{kg}/declared`, `{kg}/obs` (writer-named token only), the MVT/PMTiles tile
paths. Nothing else, `..` refused, same 404 body as an unknown route.
Everything it may persist is bounded:

| write | body cap | rate | budget |
|---|---|---|---|
| `POST {kg}/report` | 8 MB | 2/s per token, burst 30 | `neWriteBudgetGate`: 4 GiB/day per token (`NE_WRITE_BUDGET_MB`; per-name overrides via `NE_WRITE_BUDGET_MB_OVERRIDES`), 429 + Retry-After |
| `POST {kg}/chunks` | 2 MB | (needs a pending want from a report) | same bucket |
| `POST {kg}/obs` | 32 MB | same 2/s bucket | same bucket |

Stored strings (`algo`, `epoch`, `source`, `observer`, `ap_version`,
`tiles.hash`) ≤ 64 chars, one line. All NE routes pass `apiMiddleware`, so
every contributor request is in the journal: `journalctl -u umfeld-at | grep
'peer=contrib:'`. Reads under `/contrib` render like the private tier
(unbucketed K rows) — a data-exposure trade-off for trusted peers, not a
machine risk.

## Config (systemd `EnvironmentFile=/etc/umfeld-at.env`, mode 0600)

```
UMFELD_PUBLIC_ONLY=1            # enforce the allow-list on /api/v1 (0 = rollout: old paths still open)
UMFELD_PUBLIC_URL=https://example.org   # share links, attribution, UA
UMFELD_PRIVATE_PREFIX=/k        # private tier path prefix (never advertised)
UMFELD_TOKEN=tokA,tokB          # ≥16 chars each, one per client so one can be rotated alone
UMFELD_TOKEN_NAMES=name-a,name-b   # parallel labels (unset → token-1, token-2, …)
CONTRIBUTOR_TOKEN=tokX,tokY     # report-only peers: NE epoch reports + manifest (docs/ne-cells.md §observers)
CONTRIBUTOR_TOKEN_NAMES=peer-x,peer-y
NE_WRITER=peer-y                # token NAME allowed on /{kg}/obs (the observed-layer peer)
NE_WRITE_BUDGET_MB=4096         # optional; per-token daily disk-write budget
NE_WRITE_BUDGET_MB_OVERRIDES=peer-y:5120   # optional; per token-name overrides
UMFELD_PEER_OBS_NAME= / _API= / _WEB=      # observed-layer peer (peers.go)
UMFELD_PEER_SUB_NAME= / _API=              # subsidy-claims peer
UMFELD_PEER_MAP_NAME=                      # the map client reporting declared chunks
```
Token transport: `Authorization: Bearer <tok>` (preferred), `X-API-Key: <tok>`,
or `?token=<tok>` for download URLs. Rotate: edit the file, `systemctl restart
umfeld-at`. Startup log line: `🔐 API tiers: public_only=… private_tier(<prefix>)=…`.

## `/api/v1/context` — the public primitive

One call per clicked place: `?lon&lat[&area_sqm&building_count&footprint_area_sqm
&landuse_areas=41:140,52:672&kg=63349&include=…&radius_m=]` → `municipality`,
`land_price`, `natura2000`, `protected_area`, `osm`, `toponyms`, `legal` (legal
only with `kg=`; we never resolve a point to a KG — that would be cadastre).
Blocks fail independently (`data.missing{block: reason}`); all are in-memory
or PK lookups, so the Latency Contract holds trivially.
`land_price` reuses `PredictLandPrice` with `ParcelPricingInput.Split` built
from `landuse_areas` (source `"client"`), so classification is area-based from
caller-supplied features.

## Verifying a deploy

```
curl -s localhost:8000/api/v1/spatial/bbox | head -c 80                       # retired → {"error":"not_found"…}
curl -s -H "Authorization: Bearer $TOK" localhost:8000$UMFELD_PRIVATE_PREFIX/api/v1/ne/manifest | head -c 80
curl -s localhost:8000/api/v1/context?lon=15.0857\&lat=47.0665\&area_sqm=800 | jq .data.sources
curl -sI localhost:8000/api/v1/natura2000/point?lon=15.45\&lat=48.36 | grep -i x-data-attribution   # EEA, not BEV
go test -tags fts5 -run 'TestPublicAllowList|TestTier' .
```

## `/api/v1/kgs` — the KG universe (public, `kg_universe.go`)

All 7 850 Katastralgemeinden as one list, register keys only (code, name,
Gemeinde/Bezirk/Bundesland, bbox, centroid — the same fields the observed-layer
peer publishes). **Contract: `kg_count = 7850`**, `universe_hash =
sha256(join(sorted(kg_code), "\n"))` = ETag = `X-KG-Universe-Hash`. Source of
truth is the committed `data/kg_universe.json.gz` (served when the index or
the peer is down); startup falls back to the `kg` table if the file is missing.
A background loop (45 s after start, then hourly — `kgUniversePeerRecheck`)
compares against `$UMFELD_PEER_OBS_API/api/v1/kgs?processed_only=0` and reports
it as `data.peer_check` + header `X-KG-Universe-Peer-Agree` (memory only; skipped
when no peer is configured). The precomputed body is re-encoded on every check,
so the unfiltered path is always a byte copy; the ETag stays the universe hash —
only a DISAGREEMENT changes it (`-peer-disagree` suffix), `checked_at` moving
does not break peers' 304s.
**Padding:** `kg_code` is always 5 digits in the list (740 start with `0`);
`finish()` pads/dedupes every constructor (file, kg table), `KGUniverseHash`
pads before hashing (so a peer hashing `1004` gets the same hash as one hashing
`01004`), the peer compare pads the peer's codes, and `?codes=` is in `kgCodeParams`
(middleware pads 4-digit codes, sets `X-KG-Code-Normalized`; the handler also
accepts whitespace / shorter numeric spellings via `padKGCode`). `gemeinde_code`
(5) / `district_code` (3) / `state` (1) never have leading zeros — not padded.
`TestKGUniversePaddingInvariance` pins all of this.
`./umfeld-at -kg-universe-build` rebuilds the file, re-verifies, exits 1 on
disagreement. `TestKGUniverseFileContract` fails if the committed file drifts
from 7 850 or lacks an agreeing peer_check. The unfiltered body is served from a
pre-gzipped copy (handler sets `Content-Encoding`, the one exception to the
global-gzip rule). Attribution source id `kg_register`.

**bbox revision 2 (`kg_universe_bbox.go`):** the per-KG `bbox` used to be the
envelope of an older parcel index, narrower than the Katastralgemeinde for ~75 %
of KGs (e.g. 63308 Gallmannsegg lacked a 1.2 km W strip); the observed-layer
peer rasterises from it, so those strips were "not classified yet" in the
observed layer while the declared layer (NEC1, cut from the VGD polygon) covered
them. Now `bbox = union(old bbox, VGD envelope + 0.0001°)` from the committed
`data/bev_vgd_admin.json.gz` (the table bevdirect embeds; BEV VGD 1:50 000, CC BY 4.0,
4-decimal rounding → the pad). Nothing shrinks, codes/`universe_hash`/`kg_count`
unchanged, `generated_at` moves, `data.bbox_revision=2`, `X-KG-BBox-Revision`,
ETag suffix `-b2` (old tags refetch, `TestKGUniverseHandler`). The kg table's
min/max columns are updated in the same pass so the bbox index agrees.
`-kg-universe-build` also writes `data/kg_universe_bbox_diff.json.gz` (old, new,
`slivers` = new − old as ≤ 4 disjoint rectangles, `max_delta_deg`), served tokenless as
`GET /api/v1/kgs/bbox_changes?min_delta=&codes=&state=&district_code=&gemeinde_code=&fields=codes`
so peers queue sliver repairs for KGs they already processed. Re-running the build
keeps the diff's original baseline (`kgUniverseFromDiffBaseline`) — the sliver list
is not erased by a rebuild. `TestKGUniverseBBoxRevision2` pins: every bbox ⊇ VGD
envelope and ⊇ its `bbox_old`.

**Adoption:** peers key their per-KG state on this list and compare
`universe_hash`. Every request log line carries
`peer=public | private:<name> | contrib:<name>` (`requestPeerTag`), so peer
traffic is one grep:
```
journalctl -u umfeld-at --since -1h | grep '/api/v1/kgs' | grep -o 'peer=[^ ]*' | sort | uniq -c
```
