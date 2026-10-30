# NE cells — contract (algo `ne-cells-2`, container `NEC1`/`NEO1` fmt 1)

The NE product: per H3 res-12 cell (~307 m²) the **declared** land-use mix of
the surveyed register in nine groups, structure cover and count, boundary
density and orientation; per res-10 cell a K record with unit-size percentiles;
an **observed** layer for the same cells supplied by the observed-layer peer
(`{OBS_NAME}`). The server computes nothing: peers build cells from BEV
CC BY tiles with bevdirect + the frozen Python reference `ne_cells/`, report
digests (and, on request, rows), and upload the observed layer; the server
validates, stores, joins and serves (`api_ne*.go`, `ne_store.go`). Change over
time is derived from the reports: `docs/ne-change.md`. Public read API:
`docs/ne-public-api.md`.

## Coverage rule — lossy builds are not change {#coverage}
Aligned 0.02° cells from bevdirect-serve are byte-identical across versions
≥ v0.2.0 and reproducible across hosts and days. What is *not* stable is the
**request layout**: a multi-cell `/viewport` on bevdirect-serve < v0.3.2 kept
one truncated copy of a unit wider than cell + pad, whereas `ne_cells` unions
all truncated copies → `cells_n` drops 0.6–6 %, hundreds of chunk digests around
those units shift. Land use never differs.

Server rule (`neCoverageRef`, `api_ne_reports.go`; bbox-independent): per
(source class, algo, chunk_res) the reference for a report is the most
`cells_n` among comparable reports whose bbox is **contained in its own** (the
identical bbox included) — a build's `cells_n` is monotone in its bbox, so a
wider box must hold every cell of a narrower one. A report more than
`neCoverageTol` (0.1 %) below that is **lossy** → `coverage.status:"lossy"`
(`coverage.reference:"contained_bbox"`), `change_suspect:"coverage_lossy"`,
never a baseline nor a "last" digest (skipped in `neReportHistory`), no
`want_chunks`, its `since_last` diffs counted as `chunks_changed_suspect` on
`/ne/reports` and excluded from the public change figures. Old lines are
re-classified on read (`neMarkLossy`; a good report whose `since_last` was
taken against a lossy one is `previous_lossy`). A viewport *narrower* than
every earlier bbox has only itself as reference; that case is covered per
chunk by the log rule below. Land-use change never reduces `cells_n`, so the
rule cannot hide real change. A sub-cell KG (bbox narrower than 0.02° in one
axis — about half of all KGs) is a normal whole-cell build, **not** lossy.

**Per-chunk coverage (`neVersionsCoverage`, `ne_change.go`):** a complete build
emits a fixed cell set per res-10 chunk (land does not vanish or appear; since
bevdirect v0.3.3 the bbox only selects chunks). Two consecutive versions of a
chunk in `{kg}.chunks.zst` with *different cell sets* are therefore a fetch or
domain difference — a build truncated by a missing tile or two observers that
disagree on the KG's edge cells — never change: every delta of that pair is
status `coverage` (`qc.cells_coverage`, `qc_by_day`), listed, never cumulated,
never candidate/confirmed, never `pending` on `/ne/revisit`. The next version
compares against the latest one by the same rule, so a complete build after a
truncated one re-baselines instead of "gaining land". Needs no reference bbox
and no new peer field.

**Edge-cell history:** bevdirect-serve < v0.3.6 floored `lon/0.02` without
epsilon, so for ~7 % of columns/rows an aligned single-cell `/viewport`
resolved to two cells and compose kept the *neighbour's* truncated copy of a
wide unit — which copy won depended on the requester's cell block, i.e. on the
KG. Fixed in vtcseamless **v0.3.6** (`cellEps`, `TestCellsForAlignedIsSingleCell`).
Affected cells change **once** when a peer first builds on v0.3.6 — a candidate
the next pass confirms; the per-chunk rule keeps the ping-pong out of the figure.
The bevdirect tag is **not** the classifier: `neBevEquivalentTags` keeps
v0.2.1…v0.3.6 in one class (v0.3.4 added tile hashes only; v0.3.5 a TLS trust
fix for the BEV host plus `/health` `upstream{}`; v0.3.6 the cellOf fix — none
changes assembly).

**Layout rule (`neLayoutKey`, `neLayoutSuspect`):** from bevdirect v0.3.3 on the
input domain is the union of aligned cells, so the report `bbox` is a stable
**cell block** and only selects which chunks are reported (fully inside), never
a digest — one layout class `aligned`. For **pre-v0.3.3** (unaligned) builds the
request layout alone moves chunk digests, so those are keyed by bbox and a
`since_last` across different keys is `change_suspect:"layout_change"`
(`since_last.changed_previous_bbox_differs`; retro on read in `neMarkLossy`) —
a re-baseline, not change. Measured: aligned→aligned pairs under a *different*
bbox differ on 0.04 % of shared chunks (no edge concentration), same bbox
0.12 %; legacy→aligned 3.8 %, half within 0.01° of the bbox edge. Only
cell-block vs cell-block, non-lossy `since_last` diffs reach the landing-page
figure. A report carries one digest per res-10 chunk, so that figure says
**"N chunks (within ≈ X ha)"** — the count is the measurement, the hectares bound
where it lies; m² per group come from the chunk log (`docs/ne-change.md`).

Peers: build from aligned cells (`vtcseamless observe` does; own pipelines must
fetch cells, not viewports). bevdirect-serve < v0.3.3 emitted `null` for an
empty layer and the frozen `canon.py` crashes on it — fixed in bevdirect-serve
v0.3.3 (`[]`) and vtcseamless-py 0.1.1 (normalises). BEV regenerates all tiles
nightly (~00:54Z); with a 24 h tile TTL a peer can mix tile generations in one
build (one edge column) — the QC fields of `docs/ne-change.md` §5a catch it.

## Triage in 60 s — "are reports coming in / is change computed?" {#triage}
Change in m²: `docs/ne-change.md` — `curl localhost:8000/api/v1/ne/stats`
(national `change{confirmed,candidate,by_day}`), `?kg=63330`;
`ls data/ne_cells/*.chunks.zst` = KGs with a chunk-version log;
`journalctl … | grep '🧾 NE chunks'` = step-2 uploads. Peers need vtcseamless-py
≥ 0.2.0 (sends `chunks_ap`); older clients keep working, they just never feed
the change series.
"Grey/white index-epoch artefacts in the glyph for a KG?" → the glyph reads the
chunk log's latest confirmed rows before `bev`/`lu` (`docs/ne-change.md` §4b):
`data/ne_cells/<kg>.chunks.zst` exists → fixed; else no peer has reported rows
there yet.
"How many KGs still ride on the index epoch?" → `curl -s localhost:8000/api/v1/ne/manifest | python3 -c "import json,sys;
d=json.load(sys.stdin); print(d['kgs_obs_on_index'], 'obs on index;', sum(1 for e in d['kgs'].values() if (e.get('declared') or {}).get('source','').startswith('index')), 'declared = lu')"`
(the lists peers work off: `revisit.no_log`, `revisit.obs_on_index`).
```
TOK=<a private token>
curl -s -H "Authorization: Bearer $TOK" localhost:8000<private>/api/v1/ne/reports | python3 -m json.tool | less
curl -s -H "Authorization: Bearer $TOK" 'localhost:8000/contrib/api/v1/ne/revisit?limit=20' | head -c 2000   # which KGs peers should re-observe first
#   live        = per token×observer since process start (empty after a restart — normal)
#   on_disk     = scan of data/ne_cells/*.reports.jsonl (memo 60 s): reports, became_baseline, compared,
#                 chunks_changed (vs epoch 0), chunks_changed_since_last, kgs_with_changes
sudo journalctl -u umfeld-at --no-pager | grep '🧾 NE report' | tail      # one line per POST
python3 - <<'PY'                                                            # per-report table from disk
import json,glob
for f in sorted(glob.glob('data/ne_cells/*.reports.jsonl')):
    for l in open(f):
        d=json.loads(l); r=d['report']; x=d.get('result',{}); sl=x.get('since_last') or {}
        print(f[-19:-14], d['received_at'][5:16], d.get('token'), r['observer'][:20], r['source'], 'chunks',len(r.get('chunks') or {}),
              'bbox',r.get('bbox'), '|', x.get('baseline'), 'cmp',x.get('compared'), 'same',x.get('chunks_same'),
              'chg',len(x.get('chunks_changed') or []), 'new',x.get('chunks_baselined_now'), 'sl_chg',len(sl.get('changed') or []))
PY
```
Fastest answer to "are contributions coming in from every peer?": the
`observers[]` array of `/ne/reports` → `on_disk` (token × observer: reports,
kgs, sources, first/last_at, compared, chunks_changed_since_last).

**Peer note — revisit order:** at the start of each pass fetch
`GET /contrib/api/v1/ne/revisit?observer=<your observer name>&limit=50`, observe
the KGs in `pending[].kg` first (oldest first; single-sighting change your pass
would confirm or supersede), then `stale[].kg` (baseline only), then
`no_log[].kg` (no chunk rows ever — the declared picture there is still the
index epoch) and, observed-layer peer only, `obs_on_index[].kg` (re-post NEO1
built on `GET /contrib/api/v1/ne/{kg}/declared`), then continue your normal
order. `?observer=` drops KGs whose pending versions you posted yourself — a
re-post of your own digest inside 20 h is not a sighting. The answer is a
snapshot ≤ 10 min old.

**Reading rules learnt from the first passes:**
- Almost everything in a first pass is `baseline:this_report` — expected;
  change needs the *same* chunks reported twice.
- **Cross-observer "change" within hours is suspect, not land use.** BEV does
  not change in two hours; such candidates are host-side assembly differences.
  A changed chunk counts as land-use change only when reported by the same
  observer across BEV epochs (months), or when two observers agree on the new
  digest (confirmed).
- **Cross-version one-class-flips-wholesale = build difference**, not land use.
  Verify a new bevdirect tag from the report log: if cross-version comparisons
  show *less* noise than same-version ones, one equivalence class is right. If
  a second observer on the new tag confirms the identical bulk delta, treat
  the tag as a new source-equivalence class (baseline per `(kg, class)`) and do
  not read the "confirmed" burst as change.
- A confirmed delta whose source tile hashes also moved is real source change;
  one whose hashes stayed equal is `pipeline` (`docs/ne-change.md` §5a).
- The map peer posts **viewport-sized** reports (2–6 km bbox, 70–700 res-10
  chunks, bursts when a user browses) — not whole KGs; the per-chunk baseline
  (§observers) exists for exactly that.
- Stored line layout: `{received_at, token, unchanged?, report{kg,algo,epoch,source,observer,chunk_res,cells_n,digest,bbox,chunks{}}, result{…}}`;
  dedupe stubs (`unchanged:true`) carry no chunks. Files on disk are `{kg}.nex`
  (`ne_store.go`) — `.nec/.obs` are converted away, `ls *.nec` is expected to be
  empty. To test a comparison live without polluting a peer's history: replay a
  stored report with a private token (different token ⇒ no dedupe), then
  `head -n -1` the line back out of the jsonl.

## FREEZE
`algo ne-cells-2`, `ns_table ns-groups-1`, `lu` = `source index-2026-03`,
`epoch 2026-03`, `bev` baseline = `bevdirect@v0.2.0`; `frozen.until` is served
in every `/ne` and `/ne/manifest` answer as `frozen{}`. **Cell-equivalent
bevdirect tags: `v0.2.0 ≡ v0.2.1 ≡ v0.3.0 ≡ … ≡ v0.3.6`** (`neBevEquivalentTags`,
`api_ne.go`): v0.2.1 adds the 24 h tile-cache sweeper; v0.3.0 is the same
assembly moved to the public module github.com/raffopenssh/vtcseamless; v0.3.1
RAM-only tile cache; v0.3.2 multi-cell `/viewport` dedupe fix (below); v0.3.3
aligned cells + `[]` for empty layers; v0.3.4 tile hashes; v0.3.5 TLS trust;
v0.3.6 `cellEps`.

### Aligned cells only (bevdirect < v0.3.2) — multi-cell viewport duplicates
`ne_cells`-comparable builds **must come from aligned 0.02° cell requests**
(`west=ix*0.02, south=iy*0.02, east=west+0.02, north=south+0.02`, `ix=floor(lon/0.02)`)
on any bevdirect-serve < v0.3.2. `compose()` deduped structure/landuse pieces by
their per-tile id (`z_x_y#i`); a structure crossing a z16 tile edge inside a
neighbour cell's 0.004° pad was seam-merged whole in the cell holding both tiles
but emitted as a clipped fragment under a different id by the cell holding only
one, so a multi-cell (unaligned) viewport carried both (`bld_n` +1 along z16
tile columns, digests shift). Units were never affected (stable `kg-gnr` id +
`complete`); landuse neither. Fixed in vtcseamless **v0.3.2**: pieces carry
`complete` and seam-merged structures list `members`; compose dedups by member
id with the unit rule (complete copy wins, then larger area). Single-cell docs
are byte-identical apart from the 2 new fields (`ne_cells/canon.py` ignores
them), so per-cell digests are unchanged and v0.3.2 is cell-equivalent;
multi-cell viewports now equal the union of their cells
(`TestComposeEqualsCellUnion`). An independent host running the public module
reproduces a reference build bit for bit (63330: `594025647ec3557e`, 1163/1163
chunks same).

Rules:
- A KG's `lu` digest **never changes under the same (algo, source, epoch)**.
- A rebuild (new input epoch, new algo) is a NEW `(algo, source, epoch)` triple,
  written only after the manifest's `frozen.until`, or earlier only with the
  observed-layer peer's agreement — either way it shows up in the manifest
  (`lu.epoch`/`lu.algo`/`lu.digest` change, `updated_at` moves).
- `ne_cells/algo.py`, `ns_groups.py`, `canon.py`, `pack.py` are frozen; a change
  bumps `ALGO` to `ne-cells-3` and is NOT deployed during the freeze.
Reference implementation: `ne_cells/` (python, **pinned shapely 2.1.2 + h3
4.5.0**), mirrored byte-identically in vtcseamless-py
(`scripts/ne_cells_sync_check.sh`).

## Reference KG 63330 — reproduce these digests
| section | source | epoch | digest | cells / K | build inputs |
|---|---|---|---|---|---|
| `?layers=lu` | `index-2026-03` | 2026-03 | `1233f3e4f9d07c27` | 52 476 / 643 | strict fetch, bbox = input_bbox + 0.002° |
| `?layers=bev` | `bevdirect@v0.2.0` | 2026-10 | `594025647ec3557e` | 52 478 / 649 | bevdirect-serve v0.2.0 cells x 753–757 × y 2354–2357 (0.02° grid, `x=floor(lon/0.02)`), `--input-bbox 15.0712,47.0902,15.1517,47.1553` |
| 4-cell check (755/2356 755/2357 756/2356 756/2357, `--input-bbox 15.102,47.122,15.138,47.1486`) | `bevdirect@v0.2.0` | 2026-10 | `9966afd37f026edb` | 19 772 / 155 | report vs `bev`: 385/385 chunks same |
| same, v0.1.0 tarball | `bevdirect@v0.1.0` (pass `--source`) | 2026-10 | `1bedf6778c78c65d` | 19 772 / 155 | not comparable to `bev` (different tag) |
`input_bbox` of 63330 = `15.0712,47.0902,15.1517,47.1553` (derived, see §domain).
Two independent machines agree on these; everything that ever differed was
input extent.

## Licence answer
Declared cells are a derivative of BEV Kataster (CC BY 4.0) and are served
under CC BY 4.0 with the fixed attribution in every header. The **observed**
layer (NEO1) is the observed-layer peer's; by posting it to `/obs` the peer
agrees it is redistributed under the same CC BY 4.0 in the same product. A
field that must not be redistributed is simply not posted — the join key (H3
id) lets the peer serve it from its own side.

## Endpoints (private tier; `/obs` writes only for the token named by `NE_WRITER`, reachable as `/contrib/api/v1/ne/{kg}/obs`; all others → tier 404)
Public, tokenless, H3-keyed read path: `docs/ne-public-api.md`.
```
GET  <private>/api/v1/ne                               endpoints, frozen pin
GET  <private>/api/v1/ne/manifest                      all KGs: {kgs:{kg:{lu,bev,obs,updated_at,status,…}}, queue, frozen}; gzip + ETag/304
GET  <private>/api/v1/ne/manifest?since=<generated_at> delta: kgs with updated_at ≥ since + counters, delta:true (§manifest)
GET  <private>/api/v1/ne/{kg}                          binary container, all stored layers (NEC1 then NEO1)
GET  <private>/api/v1/ne/{kg}?layers=lu|bev|obs        one layer (lu = index epoch, bev = bevdirect baseline); X-NE-Layers-Missing lists absent ones
GET  <private>/api/v1/ne/{kg}?format=json              decoded (also on Accept: application/json) — debugging
POST <private>/api/v1/ne/{kg}/obs                      body = one NEO1 section (≤ 32 MB) → stored, replaces previous;
                                                       or NEC1+NEO1 = peer's bevdirect declared build + obs (§NEO1)
POST <private>/api/v1/ne/{kg}/report                   epoch report (digests only), answers changed chunks
GET  <private>/api/v1/ne/{kg}/report                   reports received
```
The reference package is `ne_cells/` + `pyproject.toml` in this repository (and
inside vtcseamless-py). ETag (`"ne-<sha256[:16]>"`, 304 on If-None-Match),
`Cache-Control: private, max-age=3600`. Binary is served as-is
(`application/vnd.ne-cells`, already deflated — gzip would add nothing); JSON
goes through the global gzip.

### Coverage / unbuilt KG
`GET /ne/{kg}?layers=lu` is **whole or nothing**: a built KG answers the full
container (~265 KB median), an unbuilt one answers **404** with body
`{status, queue_position, retry_after_s, manifest}` and — when `status` is
`queued` (or `stalled`) — a `Retry-After` header = `(queue_position+1) ×
avg_seconds` of the running build (≥ 30 s). `status` values: `built`,
`queued`, `stalled` (queue file older than 1 h, Retry-After 3600), `failed`
(no Retry-After), `not_in_index` (not one of the 7 850 KGs; see §kg-count).
Fleet pattern: ≤ 1 req/s, one GET per KG at processing start, `If-None-Match`
on re-reads; honour `Retry-After` rather than polling.

### Manifest — `GET <private>/api/v1/ne/manifest`
```
{ "frozen": {algo, ns_table, lu_source, lu_epoch, from, until, bev_source, rule},
  "kgs_total": 7850, "built": n, "obs_posted": n,
  "queue": {updated_at, total, next_index, current, done, avg_seconds, failed[], finished},
  "kgs": { "63330": { "lu":  {digest, epoch, algo, source, cells_n, bytes, built_at},
                      "bev": {…} | null,  "obs": {digest, epoch, algo, source, cells_n, bytes, built_at} | null,
                      "updated_at": "<latest built_at>", "status": "built" },
           "63333": { "lu": null, "bev": null, "obs": null, "status": "queued", "queue_position": 212, "retry_after_s": 10600 },
           … every KG … },
  "generated_at": … }
```
Built from the section headers on disk (no inflate), rescanned every 30 s
(`?refresh=1` forces), `ETag "nem-…"` over `kgs`+`queue` (not `generated_at`),
`Cache-Control: public, max-age=60`, gzip by the global middleware (~2.1 MB →
~260 KB). Detect rebuilds by `lu.digest`/`lu.epoch` per KG, or cheaply by the
ETag. Size line: `/api/v1/debug/inflight` → `ne_manifest`. The full body always
carries `delta:false`.

**Delta mode `?since=<generated_at>`** (for a peer polling every 10 min — the
full ETag moves with every build because `queue_position`s change):
```
{ "delta": true, "since": "<normalised cursor>", "generated_at": …, "kgs_total": 7850, "built": n, "obs_posted": n,
  "kgs_changed": m, "kgs": { only entries with updated_at ≥ since }, "retention_s": 604800 }
```
- Cursor = `generated_at` of the previous answer (full or delta). Selection is
  `updated_at ≥ since` at 1 s resolution, deliberately **inclusive**: a file
  written in the same second as the previous `generated_at` but after that
  scan's ReadDir would otherwise be lost forever. A client therefore re-sees
  the boundary entries — merge by KG code, it is idempotent.
- `updated_at` = latest `built_at` of the KG's layers, so a KG moving queued →
  built, a new obs post or a rebuild all show up; a KG that only moved in the
  queue does not — deltas carry no `queue`/`frozen`, read the full body for those.
- Fallback to the **full** manifest (`delta:false`, replace your copy): cursor
  older than `neManifestDeltaRetention` (7 d), or later than our current
  `generated_at` (foreign/clock-skewed cursor). Unparseable `since` → 400.
- ETag `"nem-…-s"` over the changed set + counters + cursor: an unchanged
  10-min poll is a 304. `Cache-Control: public, max-age=60`. Cost per request:
  one pass over the in-memory entries outside the lock, no disk, no SQL.

### KG count {#kg-count}
The universe holds **7 850** KGs (`GET /api/v1/kgs`, `docs/api-tiers.md`). Lists
with ~8 440 "parent KGs" include dissolved/merged KG numbers of the Statistik
Austria table. Any code not among ours answers `status: not_in_index`;
`/api/v1/lookup?kg=…` tells whether a code exists.

## Container (`ne_cells/pack.py`)
```
section := magic[4] fmt_ver:u8 hdr_len:u32le header:JSON(canonical,ascii) body_len:u32le body:zlib(records)
file    := section+          (GET concatenates NEC1 + NEO1; a POST body is exactly one section)
```
`header.digest = sha256(inflated record block)[:16]` — compare digests, never
deflate bytes. Header fields (NEC1): `magic, fmt_ver, algo, epoch, source,
h3_res=12, k_res=10, k=5, kg, cells_n, kcells_n, groups[9], ns_table,
attribution, notice, record_lu, record_k, coord_decimals=7, input_bbox[4],
coverage_bbox[4], input_pad_deg, input_rule, inputs[], stats{}, digest`.
`inputs[]` is provenance (file, sha256[:16], rows, endpoint, ready,
bevdirect_version, fetched_at) — informative, not covered by the digest.
`inputs[].sha256` is a content hash (`hash_rule: content-sorted-rows-1`): per
layer list, rows → canonical JSON (sorted keys, compact, ascii) → sorted →
`sha256("<layer>\n" + rows joined by "\n")[:16]`; `file_sha256` keeps the
bytes-as-fetched.

### LU record — 30 B, res 12 (~307 m²), one per cell touching the KG
| off | type | field | meaning |
|---|---|---|---|
| 0 | u64 | cell_id | H3 index |
| 8 | u8 | kg_share | share of CELL area covered by units of THIS KG (1/255) |
| 9 | u8 | land | share of cell that is surveyed land, any KG |
| 10 | u8 | cov | Σlu / land, capped at 1 |
| 11 | u8 | flags | bit0 `declared_unknown` (cov < 0.90 → no verdict), bit1 `normalised` (Σlu > land by ≥ 1/255, scaled down) |
| 12 | u8[9] | lu | declared mix in `groups` order: geb bau acker gruen wald wasser verkehr alpen sonst — 1/255 of CELL area |
| 21 | u8 | bld_cover | structure ground cover, 1/255 |
| 22 | u16 | bld_n | structure centroids in cell |
| 24 | u8 | gk | Grenzkataster share of land |
| 25 | u8 | n_parc | units touching (≤ 255) |
| 26 | u16 | line_m_ha | unit-boundary metres per ha of land (fabric density) |
| 28 | u8 | orient_coh | 0xFF unless bld_n ≥ 5; else coherence 1/255 |
| 29 | u8 | orient_axis | 0xFF unless bld_n ≥ 5; else mean axis / 2 (0..89) |

### Input domain (`input_bbox`, rule `geom-intersects-input_bbox`) — ne-cells-2 {#domain}
Cells are KG-independent, and the record block is a function of the domain
alone, not of how much a fetch returned:
1. `input_bbox` = bbox of the KG's own units (canonical geometry) padded by
   `input_pad_deg = 0.004°`, rounded OUTWARD to 4 decimals
   (`algo.default_input_bbox`). It is written to the header; a rebuild passes
   it back with `--input-bbox` (if omitted it is re-derived and must agree).
2. An object (unit, structure, landuse piece, any KG) is INPUT iff its
   canonical geometry intersects the closed box. Objects outside are dropped
   (`stats.*_outside_domain`), so a superset fetch is harmless.
3. A cell is emitted iff it touches the KG's own units AND lies fully inside
   `input_bbox` (`stats.cells_outside_domain`; 0 for a whole-KG build).
4. K cells (res 10) are emitted only if fully inside `coverage_bbox`
   (default = `input_bbox`).
The peer's duty: supply a COMPLETE copy of every object intersecting
`input_bbox` — fetch every bevdirect 0.02° cell that intersects it (the cell
pad 0.004° makes crossing objects complete in one cell). Verified: pad-0.004
and pad-0.008 fetches of 63330 → the same digest. A border cell appears in
every KG it touches with its `kg_share`, identical bytes otherwise — dedupe by
cell_id. Omitted: cells with land < 0.02 (slivers) or kg_share rounding to 0.

### K record — 29 B, res 10 (~15 000 m²), only where n_parc ≥ 5
`cell_id u64, n_parc u16, parc_p10 u32, parc_p50 u32, parc_p90 u32, ez_n u16,
parc_per_ez×10 u8 (0xFF none), bld_n u16, bld_p50 u16 (0xFFFF unless bld_n ≥ 5)`.
Unit size = BEV attribute m² (integer, lower-median percentiles — no float
statistics). A res-10 cell not fully inside the input's coverage bbox is
withheld (truncated population).

### NEO1 (the observed layer, `algo obs-cells-1`) — `POST /ne/{kg}/obs`
Header must carry `algo, epoch, source` (`<peer>@<version>`), `kg, h3_res=12,
cells_n, groups` (== the 9-group order) and `digest`. The server validates:
- exactly one NEO1 section, ≤ 32 MB, digest = sha256(inflated block)[:16];
- the record block is `cells_n` fixed-size rows (9..64 B, first 8 B = cell id).
  **Row size is read from the header**: `record_bytes` (+ informative `fields[]`,
  echoed in the POST response and the manifest's `obs` entry); body must be exactly
  `cells_n × record_bytes`. Without `record_bytes` the size is derived as
  body/cells_n. The current peer row is **41 B** (was 29 B in obs-cells-1) — we
  never hard-code it; rows are opaque beyond the cell id;
- the **cell set must equal our NEC1 `lu` cell set** for the KG (same ids, no
  duplicates) → otherwise **409** with `ours_n, yours_n, missing_sample,
  extra_sample, our_lu_digest`; `lu` not built yet → 409 too (post after the
  manifest says `built`).
- same `digest` as the stored obs → **200 `unchanged:true`**, nothing written;
  different → replaced atomically, **200 `replaced:true, previous_digest`**.

**NEC1 + NEO1 body — obs on a bev-equivalent declared layer.**
Why: a bevdirect build differs from `lu` (index epoch) by 1–3 border cells on
~half the KGs (63330: 52 478 vs 52 476), so an obs computed on the peer's own
declared build would 409 against `lu` for the whole freeze. Instead the peer
posts its NEC1 in front of (or after) the NEO1 in the same container:
- NEC1 header must have `algo == frozen.algo` (ne-cells-2) and `source` ∈
  `{frozen.bev_source} ∪ frozen.bev_equivalent_tags` (`neValidateBevNEC1`);
  anything else → 400. Record block `cells_n×30 + kcells_n×29`, no duplicate ids.
- the NEO1 cell set is compared against **that NEC1** — mismatch → 409
  `declared_layer:"bev"`. `lu` is never a 409 on this path.
- stored: NEC1 → the KG's `bev` section (replaces an existing one only if the
  digest differs, logged `bev_replaced`), NEO1 → `obs`. The stored NEO1 header
  gets `declared_source`/`declared_digest` of the layer it was validated
  against (also on the bare path: `index-2026-03` + lu digest). Digest-neutral
  — the section digest covers the record block only. If the posted header
  already carries `declared_*` they must agree (else 400).
- answer: `declared_layer, declared_source, declared_digest, bev{digest,cells_n,
  source,epoch}` and, when `lu` exists, `lu_diff {lu_cells_n, bev_cells_n,
  missing_n, extra_n}` (missing = in lu not in bev) — informational, surfaces
  border-cell drift. `unchanged:true` only when NEO1 digest, NEC1 digest and
  `declared_digest` are all unchanged.
- manifest (`api_ne_manifest.go`): `obs.declared_source`, `obs.declared_digest`,
  `bev.{digest,cells_n,source}`, counter `kgs_obs_on_bev` (obs whose
  declared_digest == bev.digest; also in `?since=` deltas). The peer's confirm
  step compares `obs.declared_digest` with `bev.digest` instead of lu `cells_n`.
- the declared **view** (`GET /contrib/api/v1/ne/{kg}/declared`, `docs/ne-change.md`
  §4b) is the cell set to build a bare NEO1 on once the chunk log holds rows.
- reads unchanged: `/ne/{kg}?layers=lu,bev,obs`; `/ne/cell/{h3}` stays
  lu-keyed (a bev-only border cell is 404 there) and joins obs by cell id.
Test: `TestNEObsOnBevDeclared`.
Rows are passed through untouched (we decode only the cell ids). The join is
`obs_bld_n/obs_bld_cover` vs our `bld_n/bld_cover` (verdict
`building_undeclared`). Point products of the peer (man-made objects with nDSM
height, tree apices) stay on the peer's side and are never posted.

## Determinism (why bit-identity holds)
1. **Canonical input** (`canon.py`): GeoJSON → `set_precision(1e-7)` →
   `make_valid` → polygonal parts only. **Serialisation is part of the
   contract: 7 decimals** (`coord_decimals: 7` in the header). bevdirect-serve
   (`Options.Decimals = 7`, `/viewport` reports `coord_decimals`) emits 7; the
   1e-7 snap only removes float noise below that. A serialiser emitting 6
   decimals is a different geometry (double rounding flips ~5 % of last
   digits) and is NOT a valid NE input. Observers fetch from bevdirect-serve.
2. **Dedupe**: units by id — a *complete* copy wins (largest, then WKB, if
   several); if only truncated copies exist (`complete:false` in every fetched
   cell, i.e. a unit wider than the 0.004° pad) their **WKB-sorted union** is
   used — "largest copy wins" lost land inside the cells where the other copy
   was the larger one (63330 full build: 52 448 → 52 478 cells). Structures
   and landuse pieces by `(ns, WKB)`.
3. **Order-free**: every union is over a WKB-sorted list; every map iteration
   is sorted; the record block is sorted by cell id.
4. **No source attributes except** unit `area_sqm` (BEV attribute),
   `status/rstatus`, `ez`, `kg_code`, landuse `ns/code`. Structure area and
   orientation are computed from the geometry in the cell-local metre frame.
   Areas enter only as **ratios** (affine-invariant).
5. **Quantisation**: round-half-up on the same float, never banker's rounding.
6. Pinned GEOS: shapely 2.1.2 wheels bundle GEOS 3.13; a different GEOS can
   flip a last-ulp intersection. Install with `pip install ./ne_cells` (pyproject
   pins) or `pip install shapely==2.1.2 h3==4.5.0`.
Tests: `python3 tests_py/test_ne_cells.py` — shuffled/duplicated input and the
bevdirect-shaped input (split into two cells with `complete:false` copies) give
the identical record block.

## Decisions adopted from the review (ne-cells-0 → ne-cells-1 → ne-cells-2)
- res 12 for lu/bld_cover/obs; k≥5 fields → res 10 (K records).
- `bld_orient` only when bld_n ≥ 5 (was: any bld_n → 1° axis of one building).
- `bld_cover` 1/255 (was 1e-4 → exact structure area recoverable).
- cov normalised server-side (tile-overlap double count → scaled, flagged);
  cov < 0.9 → `declared_unknown`; land < 0.02 omitted.
- Fixed group order; NS→group table `ns-groups-1` frozen with the algo id.
- epoch = ISO month, source ∈ {`index-2026-03`, `bevdirect@<tag>`}; the header freezes the pair.
- Attribution + notice in every header; binary default, JSON variant; ETag.
- Border cells in every KG touched, with `kg_share`; stats KG-independent.
- Buildings win over landuse: ns=48 pieces are not cut around structures,
  so `lu` of non-geb groups is `piece − geb` before measuring.
- Input extent pinned (§domain) — digest independent of the fetch pad; the
  builder refuses `ready:false`/`truncated` inputs.
- Index epoch is not the change baseline: source-matched reports, `bev` baseline.

## Epochs — how new readings come back
Sources are pinned strings and never compared across each other:
`index-2026-03` (the one-off country-wide index build) and `bevdirect@<tag>`
(tile assembly; the tag is part of the result — v0.1.0 vs v0.2.0 differ in
~25 % of cells by the structure seam dissolve). bevdirect-serve ≥ v0.2.0 writes
`bevdirect_version` into every `/viewport` document and `ne_cells build`
derives `source` from it (mixed or missing → error; `--source bevdirect@v0.1.0`
to override for the old tarball). index vs bevdirect inside one bbox differ by
source noise (lu up to 8/255, `declared_unknown` 1 953 vs 0) — never read that
as change: `/report` answers `identical_source:false, compared:false` when we
hold no section with the report's exact source string.
```
python3 -m ne_cells build --kg 63330 --epoch 2026-10 \
    --bevdirect cell_755_2356.json --bevdirect cell_755_2357.json … \
    --input-bbox W,S,E,N -o today.nec     # bbox = interior where the fetch is complete; source from the docs
python3 -m ne_cells report today.nec --observer peer-vm-1 \
  | curl -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
      --data-binary @- {PUBLIC_URL}/contrib/api/v1/ne/63330/report
```
The report carries ONLY digests (whole build + one per res-10 chunk) and keeps
only chunks whose **every res-12 child** lies inside the bbox (default =
`coverage_bbox`; H3 children poke out of their parent, so testing the res-10
hexagon alone was wrong). The answer: `identical_source, compared,
chunks_compared, chunks_same, chunks_changed, chunks_unknown_to_us,
our_digest, our_sources`. Change is then confirmed by a second observer or by
the peer posting the differing chunks' rows (`docs/ne-change.md`). Reports:
`data/ne_cells/{kg}.reports.jsonl`. bevdirect-serve itself does not compute
cells (a Go port would break bit-identity; peers call the python package).

## Observers — who may report, how it is watched {#observers}
`POST /contrib/api/v1/ne/{kg}/report` (`api_ne_reports.go`) is open to **every
private token and every contributor token**. `/contrib/…` is the prefix peers
are told about (same handlers; the private prefix keeps working as the
operator alias — never mention it to a contributor). The map peer
(`{MAP_NAME}`, a `CONTRIBUTOR_TOKEN`, no other access) and the observed-layer
peer's fleet (`{OBS_NAME}`, one contributor token, many observers) use the
identical protocol. Writes of the obs layer stay with the token *named* by
`NE_WRITER` (`isNEWriter`), so `POST /contrib/api/v1/ne/{kg}/obs` works for the
observed-layer peer and 404s for every other contributor. The obs POST also
takes `NEC1+NEO1` (§NEO1).

**Peer tooling: the public Python package https://github.com/raffopenssh/vtcseamless-py**
(`pip install git+…`, pointer `clients/vtcseamless-py/README.md`). `vtcseamless observe --kg K
--observer L` = resolve domain from the own units (admin bbox is only the guess) → fetch the
aligned 0.02° cells from a bevdirect-serve (`Server` downloads the release binary if none
runs) → `ne_cells build` → `POST /contrib/…/report`. Ships our `ne_cells` (frozen files
byte-identical, `scripts/ne_cells_sync_check.sh`; its CLI knows only `/contrib` and the public
paths). The Go engine stays the only assembler — a Python port would be a new, incomparable source.

Rules (same for every peer — one CLI command, one endpoint, nothing else to manage):
- **Attribution by token**, not by the self-declared `observer` string: the
  stored line and the answer carry `token` (name from `CONTRIBUTOR_TOKEN_NAMES`
  / `UMFELD_TOKEN_NAMES`); `observer` is the peer's own machine label (≤ 64
  chars, e.g. `peer-vm-1`).
- **Baseline rule** (`baseline` in every answer): `section:bev.nec` when we
  hold a section of that source — else the baseline is **per chunk**: over
  every earlier full report of the same `(source, algo, chunk_res)` for that
  KG, each res-10 chunk is pinned to the digest it was *first* reported with,
  by any observer. A report whose chunks overlap that union is compared
  (`baseline:"report"`, `chunks_same/changed`, `baseline_reports`,
  `baseline_chunks`, `baseline_observer` = first report); chunks never seen
  before are answered as `chunks_unknown_to_us` and become baseline right away
  (`chunks_baselined_now`); a report with no overlap at all is
  `baseline:"this_report", compared:false`. This matters because the map peer
  posts **viewport-sized** reports — a per-report baseline would leave every
  later viewport of the same KG "unknown" forever. So a country-wide observer
  bootstraps its own baseline on the first pass and gets `chunks_changed` from
  the second pass on — no operator build needed.
- **Change over time**: `chunks_changed` is always *vs epoch 0*; every answer
  additionally carries `since_last {compared, same, changed[], previous_received_at}`
  = the report vs the **most recent** digest we hold per chunk (from earlier
  reports of the same source, any observer). A chunk that changed once and
  then stayed stable is in `chunks_changed` but not in `since_last.changed`.
  `unchanged:true` stubs carry `since_last.same = all`. Counters/`on_disk`
  expose `chunks_changed_since_last`; each POST logs one `🧾 NE report`
  journal line.
- **Dedupe**: same digest+epoch as the token's previous report of that KG →
  `unchanged:true`, stored as a stub without chunks.
- **Rate**: 2 POST/s per token, burst 30 → `429` + `Retry-After` (never
  retry inside the Retry-After; the peer queues). Body ≤ 8 MB, ≤ 50 000 chunks,
  `digest`/chunk values 16 hex chars, chunk keys res-10 H3 hex. Daily write
  budget per token: `NE_WRITE_BUDGET_MB` (`docs/api-tiers.md`).
- **Watch**: `GET <private>/api/v1/ne/reports` (any private token) — `live`
  counters per token×observer since process start (reports, 400s, 429s,
  compared, became_baseline, unchanged, chunks_same/changed/unknown,
  last_note), `last_50` outcomes, `on_disk` scan of `*.reports.jsonl` (memo
  60 s, top 50 KGs by changed chunks). `ne_reports` line on
  `/api/v1/debug/inflight`. Per KG: `GET <private>/api/v1/ne/{kg}/report`.
- **Admin**: add a report-only peer = append a token to `CONTRIBUTOR_TOKEN` and
  its name to `CONTRIBUTOR_TOKEN_NAMES` in the env file, restart
  (`api_tiers.go` opens only `contributorPathAllowed` for it — the same 404 as
  no token anywhere else). Rotate one token alone; nothing per peer on disk.

## Build (operator)
The server never builds declared cells itself; the declared layer of every KG
arrives through peer reports (chunk rows) and NEC1 uploads. Operator passes:
```
./umfeld-at -ne-train-dict lu -ne-dict-n 1    # zstd dictionary data/ne_cells/dict/lu-1.zd (shipped)
./umfeld-at -ne-train-dict obs                # observed-layer dictionary from the local store
```
Files: `data/ne_cells/{kg}.nex` (store), `{kg}.chunks.zst` (chunk log),
`{kg}.reports.jsonl`, `queue.json`, `dict/`. Only `dict/` is committed.
