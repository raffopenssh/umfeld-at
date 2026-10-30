# NE change — coverage statistics per cell over time

**Status:** steps 1–6 shipped (`ne_cells/change.py`, `ne_change.go`, `api_ne_change.go`, public
`chg` layer + `/api/v1/ne/stats`, landing-page caption; vtcseamless-py 0.2.0 sends `chunks_ap`
and the wanted rows). Backward compatible: a report without `chunks_ap` gets today's answer
(`TestNEReportResponseGolden` inside `TestNEChangeProtocol`). Files: `data/ne_cells/{kg}.chunks.zst`
(log: skippable-frame JSON header + zstd frame per version, valid zstd stream), `{kg}.changes.jsonl`
(derived cache: line 1 summary incl. `by_day`, then delta rows). Ops: `GET …/api/v1/ne/{kg}/chunks[?chunk=&rows=1]` (peer prefix)
(audit), `ne_change` size line on `/debug/inflight`, `🧾 NE chunks` log lines. Review: §8.
**Measured (63330, 1 163 chunks, 52 478 rows):** report 43 330 B → 86 395 B with
`chunks_ap` (+43 KB, +99 % — the 16-hex digest doubles the per-chunk payload; gzip on the wire
halves it); step-2 body 1 588 302 B raw for the full baseline (3 POSTs ≤ 2 MB, 0.6–0.8 s each);
log `63330.chunks.zst` = 374 473 B (298 KB zstd frames via klauspost+dict, 76 KB headers;
`zstd -d -D lu-1.zd` on the file yields exactly the 1 574 340 raw row bytes) vs 216 911 B for
the same rows as the `bev` layer in `.nex` (res-9 frames, zstd CLI −19). Header-only report:
70 ms. Confirmation rule as shipped: a second observer confirms at once; the same observer only
on a later pass (≥ 20 h, `neSamePassWindow`) — a re-post of one build is not a sighting.

**Client rule (vtcseamless-py ≥ 0.2.0):** `observe` sends `chunks_ap` with every report and, when the answer lists `want_chunks`, posts the rows of
exactly those chunks (`NECH`, area+pattern rows only, from RAM) to `POST /contrib/api/v1/ne/{kg}/chunks?observer=`.
Older clients (no `chunks_ap`) keep working but never feed the change series. Expect every chunk to be wanted once (baseline fill, ≈ 1.6 MB per
52 k-cell KG, split in ≤ 2 MB bodies by the client) and then only chunks whose statistics moved.

NE cells are per-cell **coverage statistics** of the declared layer (`lu[9]` group shares,
`bld_cover`, `bld_n`, `line_m_ha` per res-12 cell, frozen `ne-cells-2`). NE *change* is the
same statistics with a time axis: per cell, how much more structure / road / built land / forest
there is between two passes. It is not the observed (`obs`) layer and not a new algorithm — it is
subtraction of two frozen LU records of the same cell. This doc fixes what peers send, what we
store, and what may be said in public, with bandwidth and disk as the binding constraints.

## 1. Where it stands

- State: `lu` (index, March 2026) for all 7 850 KGs in `.nex`; `bev` (peer's bevdirect build)
  for 2 KGs, overwritten on each post; `obs` for 2 077 KGs.
- Change signal today: report digests only (`sha256` of each res-10 chunk's LU records).
  939 full reports on disk, median 253 chunks / 9.9 KB, max 425 KB (39 B per chunk in JSON).
  They say *where/when* a chunk's statistics differ, not *what* — hence the landing-page figure
  "N chunks (within ≈ X ha)". Reliability rules (coverage, layout) in `docs/ne-cells.md` §coverage.
- `lu` vs `bev` differ by source as well as time (63330: 5 167 of 52 470 cells move a group
  share > 5 %), so content change is only asserted **within the bevdirect class**
  (`neBevEquivalentTags`), cell-block layout. `lu` remains epoch 0 for history, reported
  separately once its noise floor is measured.

## 2. The change unit — square metres and patterns

```
cell (res 12, ≈ 307 m²) · from (our date, digest) · to (our date, digest)
area:     Δlu[9]  in m² (1/255 of cell ≈ 1.2 m² quantum)   — structure, built, arable, green, forest, water, road, alpine, other
          Δbld_cover in m²                                   — roof area
pattern:  Δbld_n (structures touching the cell) · Δline_m_ha (boundary density) ·
          orient_axis/coh (built orientation, only where ≥ 5 structures — unchanged k-rule)
→ "green −180 m², built +180 m², roof +96 m², structures +1"; roll-ups cell → chunk → KG → Gemeinde → Austria
```
State at any time = baseline record ⊕ deltas; a cell without a delta row is unchanged.
Rendered text uses the public group labels (`neGroupLabels`: buildings, building land, arable,
grassland, forest, water, traffic, alpine, other) with a tooltip naming the Benützungsart class
behind each; the outline count/roof line is labelled **building outlines** so a reader does not
mistake a `buildings → building land` transition (declared use reclassified) for an outline
count. JSON keys (`transitions_m2{"structure>built"}`, `d_str_n`) are unchanged.
**The change product is land use only**: area shares by NS group, roof area, structure count
and texture — land facts measured from geometry. Everything else in the frozen record is
stripped before a row leaves the peer; it is not stored per version, not computed, not served.

## 3. What peers report — and what not

Peers already hold the full NEC1 of a KG in memory when they report. The protocol sends the
smallest thing that lets the server verify and store a delta:

| Step | Peer sends | Size | Server answers |
|---|---|---|---|
| 0 | `POST …/report` with header only (`digest`, `cells_n`, `bbox`, no `chunks`) | ~300 B | `unchanged:true` if the KG digest equals the last held for this class+bbox → **done** (today ≥ 90 % of passes) |
| 1 | else chunk digests (as today) | 39 B/chunk JSON, ~100 KB for a 2 500-chunk KG (binary `h3 u64 + digest u64` would be 16 B/chunk — optional later) | coverage/layout verdict + `want_chunks:[…]`: chunks whose digest is new to us and not lossy/layout-suspect, plus chunks we hold no records for (baseline fill) |
| 2 | `POST …/chunks`: **area+pattern rows** of exactly `want_chunks` — the LU record reduced to its area+pattern bytes (bytes 24–25 zeroed; nothing else sent) | 30 B × ~50 cells ≈ 1.5 KB per chunk (≈ 0.7 KB zlib) | verifies each chunk's sha256 **of the stripped rows** against a `digest_ap` the peer reports alongside the frozen digest (mismatch → 400, nothing stored), appends the rows, returns the deltas it computed |

`digest_ap` (area+pattern digest) is one extra 16-hex field per chunk in step 1: sha256 of
the LU records with bytes 24–25 zeroed. It is what the change series is keyed by;
the frozen full digest keeps its role for coverage/passes. Both are computed by
`ne_cells/pack.py` from the same build (one more hash per chunk, no new algorithm). A chunk
whose only difference lies outside the area+pattern bytes (no land-use effect) has an unchanged
`digest_ap` → no step 2, no delta row.

Step 0 is the big saving: a KG that has not changed costs one header, not a chunk list.
**Step-0 key** (same as the full path's `unchanged`): token + source class (`bevdirect@*` are
one class) + `algo` + `digest` + `epoch`, compared against the most recent report of that key
for the KG — whether it carried chunks or was itself stored stripped as `unchanged:true`.
Observer and bbox are *not* part of the key (fleet-wide dedupe per token; equal digest under a
new bbox is a no-op). A header answered `unchanged:true` is a sighting of every chunk of the
last chunk-bearing report under that digest: it confirms candidate versions by the §5 rule
(another observer at once, the same observer only on a later pass ≥ 20 h). Fixed:
the old implementation required a chunk-bearing report with the *same bbox*, so after a bbox
layout change (v0.3.3) every probe answered `202 need:"chunks"` forever.
**Confirmation needs no step 2**: a step-1 report whose `chunks_ap` equals the version we hold
(`want_chunks:[]`, `chunks_held:n`) is the second sighting (`chunks_confirmed:n`).
Expected traffic nationwide: ~1 000 passes/week × 300 B + changed KGs' chunk lists + ~60
changed chunks/week × 0.7 KB ≈ **a few MB/week**, dominated by the chunk lists of KGs that did
change. Baseline fill is a one-off: every chunk once ≈ 7 850 KGs × ~250 KB ≈ 2 GB spread over
the first full pass (the observed-layer peer's ramp already touches every KG; it is the NEC1 it builds anyway).

**Not reported:** anything outside the area+pattern bytes (never leaves the peer); the `obs` layer
(separate path, the observed-layer peer's product); unchanged chunks' rows; anything per object; `lu`-vs-`bev`
comparisons done client-side (the server is the reference for "previous"); multiple observers
of the same chunk re-sending identical rows (step 2 is asked once per new `digest_ap`).

## 4. What the server stores (disk)

- `data/ne_cells/{kg}.chunks.zst` — append-only log of **chunk versions**: `(received_at,
  observer, source, chunk, digest_ap, area+pattern rows)`, one zstd frame per chunk version with the
  existing `lu` dictionary. First version per chunk = baseline (bevdirect class); later versions
  only when the digest changed. Disk: baseline ≈ the `bev` layer we would store anyway
  (≈ 0.7 GB nationwide, 63330's NEC1 is 1.5 MB raw → ~0.3 MB zstd), then ≈ 50 KB/week of
  versions. The overwritten `bev` layer in `.nex` becomes a view on this log (latest version).
- `data/ne_cells/{kg}.changes.jsonl` — derived delta rows (cell, from, to, Δ fields,
  `status: candidate|confirmed`), rebuilt from the log on demand; small (changed cells only).
- Reports keep their role (coverage, passes, observer accounting); their `chunks{}` map could
  be dropped from the stored line once the chunk log holds the digests (–90 % of
  `reports.jsonl`), optional.

## 4b. The declared layer as a view on the log (`ne_change_declared.go`)

The sentence above is now true everywhere: the index-2026-03 `lu` layer is a frozen fallback, not
a dependency. the observed-layer peer's one-off pass posted bare NEO1 for most KGs (no NEC1 → no `bev`), {MAP_NAME}
never posts `/obs` — but every report with chunk rows left the peer's bevdirect LU rows in
`{kg}.chunks.zst`. So the declared row of a cell is, per KG: **log view → `bev` → `lu`**, where
the log view selects per res-10 chunk the latest *confirmed* version (two sightings, §5) and, with
no confirmed version yet, the baseline (first version); candidates never show; versions older
than the KG's `bev` build are skipped (`bev` is newer there); lossy builds never enter the log.
Register bytes (`gk`, `n_parc`) are zero in the log and are copied from the stored bev/lu row of
the cell where the glyph needs them; the whole-KG view (`neDeclView`) zeroes them.

**Measured before the step (`ne_measure_test.go`, `NE_MEASURE_DIR=`):**
7 850 KGs · 5 813 with a chunk log (all of them select a view) · 2 037 without (742 hold nothing but
`lu`, 1 256 hold an obs layer validated against `lu`, 39 others) · `bev`: 3 186 KGs. Obs: 4 496 KGs,
of which **2 555 validated against index-2026-03** (1 291 with a log, 1 256 without, 8 with `bev`),
1 941 against the peer's own NEC1. Cells: the view holds 230.9 M rows; **6.83 M (3.0 %) have a
different dominant group than `lu`**, 1.36 M exist only in the view (BEV added them since March),
13 k `lu`-only cells sit inside logged chunks (removed since March, still served by the per-cell
fallthrough), 459 k `lu` cells lie outside any logged chunk. Obs vs view set: 6 736 obs rows for
cells the view no longer holds; 760 k view cells without an obs row, 371 k of them in the 1 250
index-validated KGs that already have a view (5 823 KGs differ from `lu` somewhere; worst: 57306
35 k cells, 85103 27 k). Decision: structural — a third of the observed layer was pinned to a
frozen cell set with no way back (the observed-layer peer's bare NEO1 could only ever validate against `lu`), and
every layered-API answer came from that set. Go.

**What reads the view now** (same identity everywhere: `neDeclaredDigest(k, true)` =
`lu digest + bev digest + chg:<state>`, carried by every ETag and the tile cache key):
- Glyph tiles and cell card (`neReq.bevFirst`), as before.
- **Public layered API: `layers=declared`** on `/ne/cells`, `/ne/cell`, `/ne/chunk` (bin/json; zstd
  frames are stored sections → 400) and `/ne/{kg}` (one NEC1 section assembled from the view,
  header `source: chunks@<state>` | the bev/lu source, `view{state,newest_at,chunks_n,from_log,
  from_bev,from_lu}`, `register_bytes: zero`; `X-NE-Declared-Source`). JSON answers carry
  `declared:"view"`. `Cache-Control: max-age=300` (the view moves with reports); `lu` stays the
  frozen index epoch, `lu,declared` together → 400.
- **Bare NEO1 validation** (`api_ne.go handleNEObsPost`): the posted cell set is checked against the
  view first (stored header `declared_source: chunks@<state>`, `declared_digest` = digest of the
  view rows, `declared_layer: "declared"`), then against `lu` (stored as `index-2026-03`, answer
  carries `hint` + `view{}`), else 409 with `view{cells_n,digest,source,state,get}`.
  Peers fetch the set: **`GET /contrib/api/v1/ne/{kg}/declared`** (contributor or private token,
  never public; NEC1 section, `?format=json` for the header only; ETag = view identity,
  `max-age=300`; 404 when the KG holds nothing).
- **Manifest per KG `declared{source,state,newest_at,coverage}`** (`coverage` = rows of the
  selected log versions / declared cells, 0–1; `warming:true` until the startup pass memoised the
  state, 45 s + one log read per KG) and `kgs_obs_on_index` = obs layers still validated against
  the index epoch (the triage one-liner for "how many KGs still ride on March"). The manifest
  rebuilds when any chunk log grew (Σ log bytes in the scan).
- **Revisit** (§5b): `no_log[]` and `obs_on_index[]`.

RAM: assembled rows per res-9 chunk and the whole-KG view blob (`kg/view/<identity>`) in
`neStore.chunkLRU` (64 MB bound), one small state entry per KG in `neDeclStateMemo`
(`ne_declared_view` on `/debug/inflight`, `warmed`), validated by the log's size (append-only).
Nothing is written: no `.nec`, no `.nex`, no derived file. Tests: `TestNEDeclaredView`,
`TestNEDeclaredViewContract`. What remains on `lu`: the 2 037 KGs without a log (declared = `lu`
until any peer reports chunk rows there — `revisit.no_log`), and obs layers posted on the `lu` set
until the observed-layer peer re-posts them on `/declared` (`kgs_obs_on_index`, `revisit.obs_on_index`).

**Logged chunks are authoritative.** The per-cell fallthrough had one leak:
a `lu`/`bev` cell inside a res-10 chunk the log holds, which no peer's build of that chunk contains
any more (BEV moved a boundary, the cell now belongs to the neighbour KG, the domain shrank), kept
coming from `lu` forever — reports add/replace chunk rows, they never say "gone". Rule now, per KG
and res-10 chunk the view selects (`neDeclaredLoggedChunks`: selected version, not older than `bev`):
a cell absent from the log rows is **not** taken from that KG's `bev`/`lu`. Outside logged chunks
nothing changes. The cell is served if any other KG's view holds it (the normal kg_share merge —
the neighbour's rows now cover it); only when no KG's view holds it does the old `bev`/`lu` row come
back with flags bit 7 **`superseded`** (`neFlagSuperseded`, set at serve time, never written;
json `flags.superseded:true`, `/ne/cell` and the glyph cell doc add `superseded:true`), so no
tile, cell list or card ever shows a hole where a cell used to be; the glyph paints it faded
(`neGlyphPaintLUFaded`, `X-NE-Tile` `sup=`), the card says the index epoch is the only source.
It disappears for good when a neighbour's rows take it or the KG gets a `bev` (NEC1) without it.
The whole-KG view (`/{kg}/declared`, bare-NEO1 validation, manifest `declared{}`) excludes such
cells (`view.hidden` counts them); obs rows already stored for them stay and are served while the
row is served. Identity: `neDeclaredDigest` tags the log state `chg2:` — every ETag and tile key
moved once. Measured (`ne_measure_test.go`, `superseded_*`): 13 362 `lu`-only cells inside logged chunks (0 `bev`-only): **9 393 held by a neighbour KG's view** (hidden, served from the neighbour — the boundary moved), **3 969 orphaned → superseded** in 188 KGs (16107: 802, 77121: 307, 24203: 232); before, all 13 362 were served silently from `lu`. Test:
`TestNEDeclaredViewContract` (a `lu` cell missing from the logged version: hidden when a second
KG's view holds it, superseded when nobody does).

**Chunks only the log holds (example: 04015 Klausenleopoldsdorf).** The per-chunk paths
(tiles, `/ne/cells`, `/ne/cell`, cell card, fidx, stats) find the KGs of a res-9 chunk through
`neStore.chunkHits`, which was built from the `.nex` directory alone (`lu` + `bev` sections). A
res-9 chunk that index-2026-03 never held for the KG — 04015 has 122 739 `lu` cells, {MAP_NAME}'s
bevdirect build 134 430; four whole res-9 chunks around 15.98°E 48.12°N were simply absent from
the March index — was therefore never asked of the KG's log although `neDeclaredLogRows` had its
rows, and the glyph painted **white hexagons** (no row anywhere) inside an otherwise complete
forest. Only the whole-KG view (`neDeclViewBuild`, dir ∪ log chunks) saw them. Now
`neDeclStateOf` registers the chunks a KG's view holds that its `.nex` lacks
(`neStore.setLogChunks`, size line `log_only_chunks`/`log_only_kgs` on `/debug/inflight`) and
`chunkHits` returns them as hits with `Dir = -1` (stored sections answer nil there, the view
supplies the rows; `lu`-only requests still see nothing — `lu` stays pure). `neChangeAppend`
refreshes the state right after a write so a new report's chunks are visible at once. Test:
`TestNEDeclaredViewLogOnlyChunk`.

## 5. Reliability and legal rules

- **Confirmed vs candidate:** BEV regenerates tiles nightly and a 24 h client TTL can mix tile
  generations in one build. A delta is *candidate* at first sight, *confirmed* when the new
  digest is reported again (next pass or second observer). Public figures count confirmed only.
- Same class, same `algo`/`chunk_res`, cell-block layout, not lossy; `declared_unknown` cells
  do not assert `Δlu`; edge cells (`kg_share < 255`) are deduplicated by cell id across KGs.
- **§§ 76c–76e UrhG** (the sui generis right — there is no separate "Datenbankgesetz"; CC BY
  4.0 §1(c)/§4 covers it for the VTC product, `docs/payload-caching-contract.md`). The right
  protects the *database* — its records and their extraction/re-utilisation. The change series
  is built so that no stage of it holds a record or a fact about a record:
  1. **Input** is land-use/structure geometry from the CC BY tiles only (Invariant 18); the
     online survey register is never read.
  2. **Rows** are area integrals and texture statistics over a partition BEV never defined
     (H3 res 12), 1/255 quantised; the row is reduced to area+pattern before it leaves the peer.
     No geometry, id, area of any object, status of any object.
  3. **Deltas** are differences of such rows between *our* observation dates (never a date
     from any other source), confirmed by a second sighting. Structure count and roof m² are
     facts about the land.
  4. **Output** carries the NE vocabulary (word ban), both years in `NENotice()`,
     "bearbeitet", "not an official product", method + ns-group table published and the build
     reproducible by anyone from the same CC BY tiles (`docs/ne-cells.md`): a derived statistical
     product under the licence, not an extraction of the database.
  Res 12 is fine: the public state is already res 12 and a difference of two public states adds
  nothing. Patterns (`line_m_ha`, orientation) are textures with a k-rule where they derive from
  few objects; they stay as today.
- **Recommendation for the next algo epoch (`ne-cells-3`, whenever one is needed):** make the
  frozen LU record exactly the area+pattern bytes, so the *state* product matches the change
  product and the stripping/`digest_ap` step disappears. Not now — the record is frozen and
  the observed-layer peer bakes it.

## 5a. Quality control

Three checks separate "the land changed" from "the build changed", all on digests, hashes and
the stripped rows — nothing per object, nothing new stored about the source database:

- **Tile provenance.** A report may carry `tiles{expected, fetched, hash}` and `chunks_tiles{chunk:
  sha256[:16] over the bytes of the source tiles covering the chunk, sorted z/x/y}` (any 16–64
  hex is accepted, 16 kept). `fetched < expected` ⇒ `change_suspect:"tiles_incomplete"` — the
  lossy rule, no `want_chunks`. The hash travels with the chunk version (`want_tiles` in the
  stored report line, `tile_hash` in the log item). A version whose `digest_ap` differs from
  the previous one while its `tile_hash` is **equal** is a pipeline defect: delta status
  `pipeline`, `qc_same_input:n` in the step-1 answer, the version is stored (append-only, the
  next real change still has a baseline) but never cumulated. **Late attachment:** a report whose
  chunk is already held under the same `digest_ap` but whose held version carries no tile hash
  attaches the reported hash to that version (`kind:"tiles"` log entry, `tiles_attached:n` in the
  answer) — otherwise every KG baselined before peers sent provenance
  could never trigger the check. Peers: bevdirect-serve ≥ v0.3.4 + vtcseamless-py ≥ 0.3.0.
- **Cell-set coverage** (`neVersionsCoverage`, ne-cells.md §coverage): two consecutive
  versions of a chunk holding different cell *sets* (a build truncated by a missing tile or a partial
  cell set, two observers' domains at a KG edge) are status `coverage` — listed, never cumulated,
  never pending. Complements the report-level rule, whose reference is now the most cells among
  reports with a bbox contained in the report's own (bbox-independent; a viewport observer covering
  the block `revisit` names is checked against the observed-layer peer's block). Needs no peer field.
- **Quantisation floor** (`neQCFloorShare` = 5 % of the cell, ≈ 15 m² at res 12): a delta
  with no structure-count change, no dominant-class flip and every |Δ| below the floor is
  `jitter` — z16 vertex quantisation (~0.6 m) wobbling cell boundaries. One early
  candidate day (451 cells, 2 813 m² ⇒ ≈ 6 m² per cell) was almost entirely this.
- **Clustering.** Real change clusters (a site covers neighbouring cells); noise scatters.
  `summary.qc.cluster_share` = counted cells with ≥ 1 counted neighbour (res-12 ring 1) ÷
  counted cells, rolled up to `/ne/stats` nationally and per filter.

`summary.qc` = `{cells_pipeline, cells_jitter, cells_coverage, cells_clustered, cells_isolated, cluster_share,
versions_with_tile_hash, versions}`; `summary.qc_by_day` = `{day: {cells_pipeline, cells_jitter, cells_coverage}}`
keyed like `by_day` (the day the new version arrived) so the landing card can say what QC held back per day
(summary schema 5; older derived files are recomputed).
Not yet done (proposal §2, §3, §6, §7): cross-observer reproducibility rate, tag canary,
tile-hash-change as a confirmation requirement, external validation.

## 5b. Revisit hints (`api_ne_revisit.go`)

Confirmation needs a second sighting, but peers pick KGs by their own schedule: at one point 869
candidate versions (≈ 150 ha) sat pending and 4 102 KGs had never been re-observed. The server now
steers revisits — it does not touch the baseline, the confirm rule or any stored byte:

`GET /contrib/api/v1/ne/revisit` (contributor or private token; never public, not cacheable) →
`{generated_at, pending[], stale[], totals, note}`.
- `pending[] = {kg, pending_versions, pending_cells, candidate_m2, oldest_since, newest_since,
  last_observer, baseline_source, bbox}` — KGs whose latest chunk versions are sighted once (delta status
  `candidate`: not the baseline, not `pipeline`/`jitter`/`coverage`; lossy builds never enter the log; a
  superseded candidate is not pending), oldest `oldest_since` first. `candidate_m2` = positive
  Δlu of those cells. `baseline_source` = source of the version the oldest pending one is compared against.
- `stale[] = {kg, baseline_at, reports, last_observer, bbox}` — KGs with exactly one chunk-bearing,
  non-lossy report, oldest baseline first; `?min_age_days=` (default 7).
- `bbox` (both lists) = `"w,s,e,n"` of the last chunk-bearing report. A revisit should
  **cover** this box so every pending chunk is in the report; wider is fine — since bevdirect v0.3.3
  the request bbox is not a layout (`neLayoutKey`, ne-cells.md §coverage), a narrower one merely
  omits edge chunks. (A wider viewport is never a `layout_change`.) Covering the box also gives the revisit its coverage reference (ne-cells.md §coverage): a
  build truncated by an incomplete fetch is `coverage_lossy` against the block instead of entering
  the log; a narrower viewport is caught per chunk (`coverage` deltas).
- `no_log[] = {kg, has_obs, reports, bbox}` — KGs without any chunk log: their declared
  picture is still the index epoch (§4b); one report with chunk rows covering `bbox` (the KG's
  universe bbox) gives them a baseline. KGs with an observed layer first.
- `obs_on_index[] = {kg, obs_at, has_view, view_source, bbox}` — KGs whose observed layer was
  validated against the index epoch's cell set: re-post NEO1 built on `GET /contrib/api/v1/ne/{kg}/declared`
  (`has_view`), or NEC1+NEO1. `totals.kgs_no_log`, `totals.kgs_obs_on_index`.
- `?limit=` (default 100, max 1000; all four lists); `?observer=<name>` drops pending KGs whose every pending
  version that observer posted itself (its own re-post inside `neSamePassWindow` = 20 h is not a
  sighting); `?refresh=1` recomputes synchronously.
- Derived only from `{kg}.changes.jsonl` (refreshed via `neChangeSummaryOf` when a log grew) and
  the reports walk `neScanReportFiles` already does (per-KG facts ride on it, no second walk).
  `StatsMemo("ne_revisit", 10 min)`, stale-while-revalidate, warmed 45 s after start; cold ≈ 10 s,
  warm ≈ 0.2 s, ≈ 8 MB; both lists bounded to 1 000 rows; `ne_revisit` line on `the debug inflight page`.
- Peers: fetch `revisit?observer=<me>&limit=50` at the start of a pass, observe `pending`, `stale`,
  `no_log`, `obs_on_index` in that order, then the normal order (`docs/ne-cells.md` §triage peer note). No client change needed.

## 6. Public surface (after §3–4 exist)

- `.nex` gains a `chg` layer (cumulative Δ since baseline + last change date per cell), served
  through the existing `/chunk` and `/cell/{h3}` routes; `/ne/stats?kg=|gemeinde=` adds
  `change{since, confirmed: {group transitions ha, structures, cover ha}, candidate}`.
- Landing page: `confirmed change since <baseline date>: grassland → building land 3.4 ha ·
  forest → grassland 1.1 ha · roof +2.9 ha · building outlines +212 · in 48 KGs`; the hover tip
  gets the same fields per
  day. The "vs March 2026" line only with its measured noise floor.
- Daily chart cue (`api_docs_public_ne.go` `neChangeSignal`): a day is *flagged* (amber cap on
  the bar + chevron under the axis, `g.day.flag`) when its land-use change **rate** — m² that
  changed group (sum of paired from→to transitions of the cell deltas whose new version arrived
  that day, confirmed + candidate) per km² the day could compare (re-observed chunks) — is ≥ 3× the
  median rate of the other comparing days in the 28-day window with ≥ 1 ha moved, or whenever a
  confirmed delta landed that day. The card then shows the rate vs usual as text and ONE stacked
  bar per status (confirmed in colour, candidate muted, labelled) of the hectares by transition,
  plus the structure line (`+N new · roof ±ha`). Candidate figures are displayed only labelled as
  such; the figcaption totals stay confirmed-only. `candidate_by_day` sits beside `by_day` in
  `/ne/stats` and the derived `{kg}.changes.jsonl` (`schema` field; older files are recomputed).
  The chart itself is unchanged — change is never drawn as a bar segment (sub-pixel on the area
  scale) and never as a dot series.
- Card layout: two source blocks kept visibly apart — **declared** (blue rule: the
  bar's km² · KG · reports · digest shape, pass mix as a mini stacked bar, "seen before" =
  comparable share, then the change block with status chips `confirmed ✓ 2 sightings` /
  `candidate · 1 sighting` and a `QC held back · N cells jitter · M cells pipeline` line from
  `qc_by_day`) and **observed** (green rule: satellite & LiDAR layer {OBS_NAME} posted that
  day, from the manifest's `obs.built_at`, km² from cells × 307 m²). The observed layer is never
  in the bars or the change figures. The rank/median line sits in the header; sources and
  first-sight coverage to date in the footer. The public word is "digested"/"seen before", never
  "observed", for the declared side.
- Why this is state of the art rather than a compromise: Copernicus/CLC+ and Statistik Austria
  publish land cover change at 10 m–1 ha on multi-year cadence; this series is ≈ 20 m hex cells,
  declared use by NS group, roof area and structure count, at the cadence peers observe (days),
  with per-cell provenance (digests, source tag, observer, both dates), a frozen reproducible
  method, confirmed/candidate status and open formats — and it is the only series of declared
  use over time in the country.

## 6a. Landing-page glyph (`ne_glyph.go`)

`GET /api/v1/ne/glyph` + the figure under the page title: Austria as one dot per H3 res-7
cell (16 424 cells whose centre lies inside the GADM outline, interior slivers filled —
`scripts/ne_glyph_frame_fill_holes.py`,
`data/ne_glyph_austria_r7.json`; x = lon·cos 47.6°, 668 px design width, viewBox units =
0.1 px). Every peer digest of the land in a cell adds a translucent grey layer
(α = 0.13 × chunk coverage, raised to the mean pass depth of the cell's chunks), so land
digested 2, 3, 4× simply gets darker; cells with a report younger than 6 h carry a light blue tint (α ≤ 0.42, fading by age —
never stronger than the ink, so depth stays the dominant signal); cells nobody digested yet are a hairline. Two more
**views** of the same cells — observed layer posted by {OBS_NAME} (teal) and **confirmed**
change (vermilion, darker = more res-12 cells changed: 1 · 2–4 · 5–9 · 10+) — are overlays
inside the one picture, switched by the `declared · observed · change` control under the
figure (key `v` cycles); the ink steps back to 10 % while a view is on. Never a second figure.
Colour-blind safe: ink depth and panel carry the meaning,
hue only labels the panel; no red–green pair.

Cost rule: the aggregate rides on the two loops the page already runs — the report scan
(`neScanReportFiles`, per-cell map fed line by line, chunk pass depth folded from the daily
tracker's chunk map) and the manifest loop of `neCoverageStats` (observed KGs); confirmed
cells come from the few `{kg}.changes.jsonl` with `confirmed.cells > 0`. Never a second walk
of the reports. One bounded slot holds rows + SVG + JSON (`/api/v1/debug/inflight` →
`ne_glyph`), rebuilt with the 5-min coverage memo.

Rendering: ~25 `<path>` elements (one per ink bucket, zero-length `h0` segments with round
caps), ~120 KB gzipped page; no script needed. Script adds a cursor: pointer hover / tap
pins / arrow keys walk the cells / Escape clears; the card (KG · Gemeinde, chunks of 343,
reports, passes per chunk, last report age, observed, confirmed change cells) uses the JSON
fetched once on first pointer contact (`cells[]` =
`[x, y, chunks, passes_sum, reports, age_h, observed, change_cells, kg, bands[, candidate_cells]]`,
`[x, y]` only for cells not digested yet; ~200 KB gzipped, `max-age=300`). `change_cells`
counts res-12 cells with **confirmed** change, `candidate_cells` (present only when > 0)
those with candidate change only (one sighting, never confirmed); a cell is one or the other. Coarse pointers dock the card
below the glyph; `prefers-reduced-motion` drops the transitions.

Interaction: the glyph is zoomable *before* a tap — pinch, double-tap
(keeps zooming through the overview's cap into auto full resolution like the pinch; resets only when the cap is final — full res on, overview opted, or no projection), ctrl/⌘ + scroll (plain scroll zooms once the figure was clicked; otherwise
a transient hint), drag to pan when zoomed, `+`/`−`/`0`/Escape on the keyboard; all on the
SVG viewBox, no controls on the figure. `touch-action: pan-y` at 1× keeps page scrolling,
`none` once zoomed. The card floats beside the dot on desktop and docks under the figure on
narrow / coarse-pointer screens (height shift animated). No stats caption — the chart below
carries the figures. Teaser: the server picks ~14 cells worth a look (`neGlyphPicks`: fresh
report, confirmed change, deep passes; spread across the country, nearest-neighbour order)
and inlines them as `data-picks` (~1 KB); 1.4 s after the figure is in view the cursor glides
through six of them with a pulse and the card, then stops for good. Any pointer, wheel or key
pauses it; it resumes only while nothing is pinned and the view is at 1×. `aria-live` is off
while the teaser speaks. The big JSON still loads only on first touch.

## 6b. Full resolution, views, share links (`ne_glyph_tile.go`)

**Full resolution** (`scan-search` button, `full`): the declared product at its native res 12
as paletted PNG tiles on a lon/lat grid, `GET /api/v1/ne/glyph/tile?lod=0|1|2&i=&j=[&v=obs|chg]`
(LOD 0 = 0.025°, 40 px/unit; LOD 1 = 0.01°, 100 px/unit; LOD 2 = 0.1°, 10 px/unit for the
wide view, ≈ 0.6 s cold; 1 px bleed; ≤ 48 tiles per view, 160 cached client-side, 24 MB server
LRU `ne_glyph_tiles`). The button **never moves the view**: it is tappable only once the view
is zoomed ≥ 8× (`HI_MIN`), then streams tiles for exactly the viewport (LOD 2 below 110×,
LOD 0 below 300×, LOD 1 above); zooming out below 8× switches it off. Zooming is the map's
own gestures, *find* (flies to the KG) and the close button (back to the overview). Base tile: one hexagon per cell,
dominant land-use group, alpha = dominance (floor 0.62 so the light groups never read as
blank), second group as an inner hexagon when ≥ 25 %; cells held with **no declared use**
(`cov = 0`, `declared_unknown`) get one neutral colour and a legend entry — no held cell is
ever white. `X-NE-Tile: n=;ms=;lu=9 sums;unk=;pending=` feeds the **in-view legend**: the
nine groups as mixed across the tiles on screen, first five at once, the rest behind `…`
(every bar segment has its name; the same `…` rule applies to every legend in the card).

**Change at every level of detail shows candidate too**: the dots and
hex faces carry two vermilion ramps, `.c` confirmed (opacity .45 · .65 · .85 · 1 by 1 · 2–4 ·
5–9 · 10+ cells) over `.ca` candidate-only (.2 · .26 · .32 · .38, same split, `neGlyphCandBuckets`),
both under the soft halo; the full-res `v=chg` tiles already painted candidates light. The
card names both counts (`● n confirmed · ● n candidate`).

**Full screen layout** (same rev.): `screen` in the hash (`fsToggle`, native API when it can,
fixed box otherwise — a shared link enters the fixed box since there is no gesture). The
docked card is a **sheet over the map's lower edge** (`position:absolute`, aligned to the
map's box via `--fs-x/--fs-w/--fs-b` set by `fsFit`), so it never takes layout space and the
map never moves when it opens, grows or closes; the tool bar aligns to the same box, with the
full-res status (`.gt-n`, "loading… · n cells" / "n cells · s") in its own slot at the right
end so nothing to its left shifts. The view buttons' loading dots sit centred over the
separator slot (fixed 2ch), never into the next label. `floating()` reads the card's `--float`
custom property (1 floating, 0 docked / coarse) instead of `position`.

**Views at every level of detail**: dots (server paths `.o` / `.c` in the SVG), hex faces
(client paths from the JSON rows `observed` / `change_cells` / `candidate_cells`), full res (overlay tiles over
the dimmed base: `v=obs` paints the **observed land use** of each cell — `obs_lu[9]` of
the observed-layer peer's NEO1 row, located via the header's `fields[]` + `record` format (`neObsLayout`, never
hard-coded), same palette and dominance/second-group rule as the declared base so the two
read side by side; cells without an observed row stay transparent so the faint declared base
shows through; meta `obs=` painted cells, `lu=` the observed sums, `trees=Σ tree_n`,
`tmax=` tallest tree (0.5 m units), `th=` five bands of cells by tallest tree
(<5 · 5–15 · 15–25 · 25–35 · 35+ m) — trees pass the same QA the observed-layer peer applies to its own tree
index (`neObsTrees`: LiDAR flight present, ≥ 1 apex, ≤ 60 m, from 20 m up crown_frac ≥ 10 %
so masts/cranes never count, 255 = nodata); `v=chg` paints the cumulative change state from
the chunk log — candidate light, confirmed, confirmed with ≥ 10 % of the cell or a structure
count moved darkest; meta `chg=confirmed,candidate;dlu=9 signed m²`, cache 300 s). The
in-view legend reads the view: observed use mix + share observed + the tree bar (count,
tallest, wooded cells by band) · confirmed/candidate + Δ per group.

**Trees** (view-bar toggle next to *observed*, Lucide `tree-pine`, `;trees` in the link):
not a cell fill — the apices themselves, drawn as **crowns** the way the forestry peer draws
them (`srv/static/app/js/{rings,treelayer}.js` there). Source per tile, straight from the
peer (CORS, no key; `hiApex`, cached/culled like the picture tiles as `v='apex'`, LOD 0/1
only, own dot-loader on the tree button): at LOD 1 the res-12 cell layer
`GET …:8100/api/v1/cells?bbox=&layers=trees&format=columns&centres=1&fields=tree_n` — every
apex ≥ 3 m with `crown_m2`, species, vitality (~7–10 k trees, ~95 KB gz, 0.15 s per full
forest tile; the fast path {MAP_NAME} uses); at LOD 0 the sampled giant index
`GET …/trees/bbox` (h ≥ 20 m, the tallest few per ~0.1 ha). Each tree is a hollow ring of
its **true crown size** (r = √(A/π), floor 2.4 px), hairline, in a light green ramp by
height (`AP_COL`, the `g-tr*` legend swatches share it), always the same light sprites on
every ground. Density rule from the forestry peer: crowd = Σ circumference / tile area,
f = 1/(1+(crowd/0.55)²); alpha, stroke and glow follow f, so a closed stand stays a quiet
texture and a lone tree gets a halo. The mass is one `<canvas>` per tile → PNG blob →
`<image>` (`apRender`, ≤ 3 Mpx, re-rendered when the zoom drifts > 20 %, 14 px bleed so
crowns on the edge are whole); never a DOM node per tree. Only the picked tree and the
flagged ones are SVG (`#ng-tr` via `apMarks`). Hit test `apAt` = nearest crown within
max(r, 7 px) + slop (14 px touch). Hover peeks height · species · vitality · crown m²;
a tap pins the tree (card: height, band, species, crown, confidence, KG). The server's
`v=tree` cell-band tile is still served but no longer fetched by the glyph.

**Relief** (view-bar icon right after the tree toggle, `;relief` in the link, any view):
{OBS_NAME}'s hillshade XYZ tiles (`GET …/tiles/hillshade/{z}/{x}/{y}.png`, 25 m DTM,
z 10–17, 1-year immutable cache, CORS) the way {MAP_NAME} draws its ground. The
client picks the Web-Mercator zoom whose tile matches the screen (`relZ`, +½ on hi-dpi),
places each tile as an SVG `<image>` by its lon/lat corners (the glyph is plate carrée; the
warp across one tile is under a pixel; 1 px bleed against hairline seams) in a `.ov-rel`
group with `mix-blend-mode: overlay` (`isolation: isolate` on the svg so it blends with the
cells only — mid grey leaves a cell untouched, lit slopes lighten, shaded darken). Weight
(`relW`): .42 up to `HI_LOD0`, linear to 0 at `HI_LOD1` and no tiles fetched past it (the
25 m cells and the observed-layer peer's mosaic seams would dominate the res-12 hexagons); a soft blur once a
25 m cell exceeds 4 screen px. Fetched straight from the peer with ≤ 6 in flight, 429/503
`Retry-After` honoured with exponential back-off (4 tries), 204 = no data; LRU of 240 tiles,
groups of the previous zoom stay visible until every wanted tile of the new zoom answered.
Only under full res: toggled in the overview it turns full res on when the view is past
`HI_MIN`, otherwise hints to zoom in and waits (the hash carries `;relief` either way). The
button is text-like (muted ink, hairline; full ink and a thicker stroke when on), with the
same dot loader as the view words. Legend gets a `relief · hillshade, 25 m terrain model ·
{OBS_NAME}` line. Test hook: `neGlyph.state().relief / reliefZ / reliefLeft`.

**Flags on trees** (the observed-layer peer `#flags`): the card's `⚑ flag` line unfolds in place into five
claims (*looks right · not a tree · too tall · too short · misplaced* → `looks_right ·
spurious · too_tall · too_short · misplaced`), an optional public note and *send*; `POST
…/api/v1/flags {lon, lat, layer:"trees", claim, note, observed{h_m, crown_d_m,
confidence}, source:"umfeld-at/glyph", reporter:<random id in localStorage, hashed
server-side>}` — Enter sends, Esc folds. Open subjects per tile (`GET …/flags?bbox=&layer=
trees`) are matched to the nearest apex ≤ 12 m and ring it (vermilion = a complaint, teal =
`looks_right`); the peek and card name the report count and top claim. Nothing is stored
on our side.

**Full-res veil = exact KG outline**: `GET /api/v1/ne/glyph/kg?kg=` traces
the KG's res-12 cells (`kg_share ≥ ½`, plus split border cells with ≥ 3 majority neighbours)
with `h3.CellsToMultiPolygon` into an SVG path in viewBox units (holes kept, drawn evenodd;
~50 KB for a 52 k-cell KG, LRU-cached by lu digest, 1 h). The client fetches it once per
selected KG and swaps it into the veil mask; the res-7 hex faces stand in until it arrives
and remain the veil for a pinned res-7 cell.

**Card** (res-7 cell, KG, res-12 cell): three sources — declared · observed · change — each a
one-line row when collapsed and a section when expanded, **ordered by the view** (the one on
the map reads first). Change is **async** like observed: the confirmed-cell count is there
at once (glyph rows), the KG's Δ per group, candidate count, since/digests and the **QC line**
(pipeline / jitter excluded, clustered · isolated, share of digests with a tile hash) come
from `GET /api/v1/ne/stats?kg=` (debounced, cached per KG, aborted on move) and only that
part is swapped in, with the card's fade. A res-12 cell card reads **cluster first**
: `/ne/glyph/cell` adds `cluster{cells, confirmed, candidate, area_m2, d_lu_m2,
d_lu_pending_m2, d_str_n, first, last, capped}` — the run of touching res-12 cells with change
around the tapped one (ring-1 flood fill through the change logs of the cell's KGs,
`neCellCluster`, cap 6000) — and the collapsed row says what moved and when, not whether it is
confirmed: `● 11 ha alpine → forest · Oct 2026 · 452 cells` (Δ by group summed over confirmed
*and* pending deltas; the from/to groups are the ones that lost / gained land). The period
(`perOf`) is a month when the sightings fall in one, `Sep/Oct 2026` for two, quarters beyond.
Open: `11 ha redeclared · 452 touching cells 14 ha`, the cluster Δ bar, `sighted 16 Oct 2026`,
then a `this cell` line with the status dot (pale = candidate), its own m², from → to, day.
The in-view block of the change view speaks the same language (`12 ha alpine → forest · ● 469
cells`, Δ bar, period in the head from the tiles' `at=first,last` meta) so the two blocks
differ only in extent, not vocabulary. Per-cell `chg` carries `first_seen` / `last_seen`
(digest timestamps of the first / latest counted delta) next to `last_change`.

**Share link** = the whole state: `#kg=63349` | `#cell=x,y` | `#h3=<res-12 id>` |
`#q=<name>` (first KG-search hit, rewritten to `kg=` once resolved), then
`;v=obs|chg` (view), `;full` (full resolution), `;z=<zoom>` (zoom about the selection's
centre, no fly; ≥ 300 = single cells), `;at=x,y,z` (centre in viewBox units + zoom,
written once the view is zoomed — so a link reproduces the exact full-zoom picture),
`;open` (card expanded), `;below` (card docked), `;card=fx,fy` (a dragged card: its top-left as
fractions of the figure box, so the receiver sees it where the sharer put it), `;screen` (full screen), `;relief` (hillshade on). `applyHash` restores in this order: view → card state → full screen → `at` →
selection (no fly when `at` is given) → full res; the hash is rewritten (replaceState,
debounced) on every zoom end, selection, view or card change; a hashchange with no selection
token clears the selection.

**Hover = peek, click = card**: at every level of detail the pointer only
ever raises the small **peek** beside the cursor (res-7: KG · Gemeinde, digested / seen-before /
age, observed · change counts, "click for the card"; res-12: one line per source — declared bar
+ top groups, under `v=obs` the observed pixel in words (`pic_obs` of `/ne/glyph/cell`), under
`v=chg` the change state, `hiPeekHTML`; the KG name is added only while no card names it).
The **card** appears only for a pinned cell / KG / res-12 cell / tree and is **sticky**: it takes
a spot beside the selection once (`anchor`), is solid to the pointer (`pointer-events:auto` on
`.x`, the teaser's `.tour` stays pass-through), and never moves for zoom, pan, hover or content
swaps (`place(c, keep)`); a new selection re-places it only when the selection would sit under
it, a fly re-checks once at its end. When no side fits (narrow map) the card takes the roomier
side and the map pans just enough to bring the selection out beside it (the only case where a
pick moves the view). Drag by grip, title, subtitle or any section header — anywhere on the
page, not just the map; double-click on the grip snaps back. The peek keeps clear of the card
(four candidate corners, then none when the pointer is over it).

**Observed rows are shown as delivered.** We do not post-process the peer's rows — the picture
is what the peer provides; a misclassified cell is reported to {OBS_NAME} and fixed there.

**Scripted visual checks** (`window.neGlyph`, see `/api/v1/docs/llm.txt` landing inventory 7a): `state()`,
`settled(ms)`, `card()`, `legend()`, `peek()`, `hover(lon,lat)`, `pin(lon,lat)`,
`view(v)`, `lonlat`/`xy`/`client`. Recipe for "compare declared vs observed at one spot":
`/#q=<name>;full;z=400` → `settled()` → `legend()`; `view('obs')` → `settled()` →
`legend()`; `hover(lon,lat)` → `peek()`; cross-check with `/ne/glyph/cell?lon=&lat=`
(`lu.lu[]`, `pic_obs`) and, with the private token, the NEO1 row itself
(`<private>/api/v1/ne/{kg}?layers=obs`, `record`/`fields` in the header).

### 6c. Subsidy layer (`ne_glyph_sub.go`)

Fourth overlay, Lucide `tractor` after *declared* and `file-pen` after *change* (one
state, `;subsidy`, key `s`): INVEKOS claims per cell from {SUB_NAME} — ochre wash
by claimed share under **declared** (hatched = dubious outline, ring = GLÖZ), lineage vs the
previous year as **outlines of each run of cells** under **change** (bold new · dotted gone ·
hairline redrawn), nothing under observed. Server-painted `v=sub|subc` tiles from the peer's
`format=bin` bins (bounded LRU, nothing on disk), res-7 overview `/ne/glyph/sub` for dots and
hex faces, `cell?sub=1` for the card. Everything else: `docs/invekos-subsidy-layer.md`.
`docs/ne-glyph.md` is the map of the glyph code for the next session.

## 6d. DIFF — declared vs observed (`scripts/ne_diff_declared_vs_observed.py`) {#diff}

Two signals, kept apart everywhere (vocabulary, figures, glyph):
- **CHANGE** = declared over time — the chunk log, confirmed deltas (§2–5). Dates are *our* sighting dates.
- **DIFF** = declared view (2026, `layers=declared`) vs observed row (the observed-layer peer NEO1 41 B, `obs_lu[9]` in the
  same nine groups) **of the same cell, today**. Its time axis is the **lidar epoch**: the observed-layer peer's
  `als{newest, oldest, dominant_share, precision}` per KG (bulk `GET {OBS_API}/api/v1/kgs?fields=codes,als&processed_only=0`,
  ~1 MB for all 7 850 KGs, ETag-revalidated hourly (304 when unchanged); full block with histograms per KG at `/api/v1/kg/<code>`;
  per-cell `als_year_newest` exists on the observed-layer peer's `/cells` json/columns but not in the frozen row —
  precision is *chunk* (res 10) for 6 534 of 6 542 KGs, so the script maps **KG → epoch** and flags
  the 934 KGs whose newest flight is mixed (`dominant_share < 0.9`); refine per chunk via
  `POST {OBS_API}/cells {ids:[res-10]}&res=10` if ever needed).

Script (public routes only, no store files, resumable under `data/ne_diff/`, not committed):
`als` (the observed-layer peer per-KG epoch, one bulk GET, seconds) · `diff` (6 542 KGs ≈ 9 min) · `change` (KGs with
confirmed change + obs) · `report` (`data/ne_diff/report.md`).

**Vocabulary** (one kind per cell; dominant group each side must hold ≥ 50 % of the cell, else `mixed`;
the two built kinds are threshold rules and ignore the dominant test):

| kind | rule | verdict (one nationwide run) |
|---|---|---|
| `agree` | same dominant group | 61.9 % of ok cells |
| `built_undeclared` | observed building cover ≥ 20 %, declared buildings < 5 % | 0.17 % · **flat with lidar age** → not "new since the flight": only 0.9 % show an nDSM rise ≥ 2 m newest−oldest; declared dominant is grass 58 % / building land 19 % / traffic 17 %, BEV outline count 0 in 98 % |
| `declared_built_absent` | declared buildings ≥ 20 %, observed cover < 5 % | 0.24 % · **rises with lidar age** (0.09 % flown 2021 → 0.5 % flown 2009): buildings declared after the flight |
| `forest_declared_open` | declared forest, observed grass/crop/bare/construction | 9.9 % · 29 % carry a Hansen loss year, 8 % an nDSM drop ≤ −2 m (clear-cuts the register keeps as forest); the rest sparse / young stands — KG-set dependent (28 % in the 2011 cohort) |
| `forest_undeclared` | declared grass/crop/alpine/other, observed trees | 2.7 %, stable 1.7–3.9 % across epochs |
| `arable_grass` | crop vs grass either way | 18.8 % · **classifier, not land**: 1.5 % in the 2021 cohort, 48 % in 2014, p90 KG share 73 % — the observed-layer peer's crop/grass phenology split vs BEV acker/gruen |
| `building_land_green` | declared building land (bau), observed vegetation | 0.43 % (gardens, courtyards: expected) |
| `sealed_undeclared` | declared vegetation/other, observed construction/parking/road | 0.20 % |
| `traffic` | one side roads & paths | 0.88 % (canopy over roads, urban `verkehr`→`geb` in Vienna) |
| `water` | one side water | 0.69 % (riparian canopy) |
| `alpine_pasture` | declared alpine (BEV *Alpen* = alpine pasture), observed grass | 2.7 % · vocabulary gap, not a diff |
| `alpine` / `other` | rest | 0.70 % / 0.79 % |

Filter status (214.1 M joined cells, 6 542 KGs): ok 93.0 % · edge (land < 0.9 or kg_share < 0.5) 1.4 % ·
mixed 2.1 % · obs_null (the observed-layer peer verdict unknown / no height) 3.5 % · declared_unknown 0.02 %.

**Is it clean enough to show?** As a whole, **no**: 38 % of ok cells disagree and the rate does not
move with lidar age or with the edge/mixed cuts (the noise cut — ok, flown ≥ 2019, unmixed KG; 2 467
KGs, 83.7 M cells — leaves every kind within ±0.05 pp of its all-KG rate), i.e. the disagreement is
**classification vocabulary first, land second**. Three subsets are clean and showable, each with its
corroboration in the observed row itself:
1. `declared_built_absent` as *"declared since the flight (lidar YYYY)"* — monotone in age, 0.1–0.5 %.
2. `built_undeclared` **∧ nDSM rise ≥ 2 m** — 2 868 cells nationwide: structures that appeared between
   the two flights and have no declared building; the remaining 99 % of the kind are persistent
   (both flights) and stay a classifier/registration question, not a lag.
3. `forest_declared_open` **∧ (Hansen loss year ∨ nDSM drop ≤ −2 m)** — ≈ 6.4 M cells: forest the
   register still declares after a visible cut.

**Mandatory filters** before any figure: status `ok` (edge, mixed, obs_null, declared_unknown out);
drop `arable_grass`, `alpine_pasture`, `traffic`, `water`, `alpine`, `other` (24.5 % of ok cells —
vocabulary/classifier); treat acker+gruen as one class; mixed-epoch KGs (934) only per chunk; always
print the lidar year next to the figure. The epoch filter is *not* a noise filter here — it is the
label.

**DIFF kinds later resolved by a confirmed CHANGE** (145 KGs with confirmed cells and an obs layer,
4 302 confirmed cells; before = row − `d_lu_m2`/area, after = row): resolved 870 · persisted 1 471 ·
introduced 916 · agree both sides 1 045. What resolves is `forest_undeclared → agree` (607: the
register caught up with trees the lidar already saw) and `alpine → agree` (96). What the change
*introduces* is `agree → forest_declared_open` (473, mostly 73510: grass → forest reclassification
the lidar does not see) and `agree → declared_built_absent` (90: a building declared after a
pre-2020 flight — the lag in the other direction). Too few confirmed cells yet for a rate; rerun
`change` + `report` as the log fills.

## 6e. Change over time — passes, the time strip, ageing (`ne_change_passes.go`) {#time}

**Unit = pass, not month.** A confirmed change is dated by the day a peer *re-digested* that chunk, not
by when the register moved; while the second pass is partial (ETA by the
exponential tail of `chunks_nth` in the daily tracker) a calendar bar would be crawl order and spatially
biased. So the primary axis is the **k-th sighting**: per KG `passes()` ranks each chunk's sighting
dates (`neChangeKG.pass`, ≤ 12 dates/chunk), `table()` gives share seen ≥ k times, `first`, `done`
(share ≥ `neDoneShare` .995) or `eta`; a delta belongs to the pass of its `to` digest (`neCellDelta.Pass`).
`neChangeWin{K1,K2,D1,D2}` (`neParseWin`: `2`, `2..3`, `2@d1..d2`, `@d1..d2`) filters `cumulate`
(`cumWin`, ≤ 4 windows cached per KG); national table = `nePassNational` over the daily tracker
(`StatsMemo` 5 min, `the debug inflight page` line `ne_passes`). `GET /api/v1/ne/glyph/passes[?kg=]`.
Once every pass is complete the strip is a plain calendar of completed intervals — nothing to switch.

**Ageing (server + client, same curve).** Tiles `v=chg[&t=]` and the overview paths colour each cell by
age at the window head: smoothstep from `full` (45 d) to `gone` (365 d) mixing the change hue toward
ash `{8a,86,82}`, opacity `.18 + .82·age` — old change rests, never vanishes. While the history is
shorter than a year the scale is the history (`neGlyphAgeScale`: gone = span ≥ 10 d, full = 12 % of
it), `epoch` = first day with a changed digest (`neChangeEpochDay`, memo 30 min) — so the first weeks
already read as a gradient, and the scale settles by itself at one year. Confirmed cells age by
`LastAt`, candidates by `AnyAt`; the chg tile key carries the day when the window is open.

**Strip (`/* ── time` in `ne_glyph.go`, `.glyph-time`).** Level 0 passes · 1 days of a pass · 2 calendar
across passes (unit d/w/m/q by span, ≤ 400 bins); hairline track, dot = √confirmed, fill = share, dashed
= open pass + ETA; drag scrubs a range; `;t=` in the hash; keys `< > , .` step, space plays. **Play**
runs at level 2 day by day, window growing from the first bin, head marker + date label; per step only
the overview paths move (`tlPaint` additive, `tlAgeSync`), tiles/card/legend catch up on pause/stop
(`tl.play.dirty → tlCatchUp`); the teaser never runs while playing. Chg tiles are keyed `~<win>` and the
previous window's tile stays as backdrop until the new one is in (`hiCull`); `hiUnderMiss` and `hiSum`
look up the windowed key. Readout = the standard `dluBar` + legend. Soak: `check('#change;t=2')`,
`check('#kg=63330;change;full;t=@d1..d2')` — ` time`, ` time loaded`, ` time strip`, ` time tiles`.
Also fixed on the way: `;full` without `at=` now lands (`hiLand` waits for the fly), the phone's inline
card clears when the hash leaves a KG.

## 7. Not doing

No per-object anything; no change polygons (dissolved hex regions at most, later); no Go port
of `ne_cells` (deltas are arithmetic on frozen rows); no change figure from `lu_diff` (cell-set
diff); no overwrite of chunk versions once the log exists — history is the product.

## 8. Review (before implementation) — numbers from the repo/data

Verdict: **no blocker**. Two corrections to §3–4 (baseline log ≈ 1.3 GB not 0.7; step 0 saves
nothing *today* because the fleet is in its first pass) and one design detail added (the
per-chunk "last digest_ap" map must be a bounded per-KG LRU, not a nationwide map).
Epoch-0 baseline (`our_digest` = `baseline_digest`, `baseline_bbox` in the answer) is the
first-seen *peer* report per chunk under the report's own bbox layout; a chunk first seen under
an older layout is re-anchored on its first sighting under the current one.

1. **Stats compatibility.** The public `lu`/`bev` per-cell fields (`neDecodeLU`) are
   `kg_share, land, cov, flags, lu[9], bld_cover, bld_n, line_m_ha, orient_coh, orient_axis`
   — all read straight from the 30-B LU record, so a delta of two frozen rows reproduces every
   one of them (bytes 24–25 are not land use and the change product drops them by design). Additivity: `lu[i]`, `bld_cover`, `land` are 1/255 of the
   **cell** area (`algo.py`: `shares[gi] = area / cell_a`), so m² = `v/255 × cell_area_m²`
   (h3-go `AreaM2`, res 12 ≈ 307 m²) and sums are additive cell → chunk → KG → Gemeinde;
   `bld_n` is an integer count (centroid rule, each structure in exactly one cell) → additive.
   Edge cells: 63330 ∩ 63350 share 330 cells; **328 are byte-identical except `kg_share`**
   (the 2 others differ on the domain edge in `land/lu/line`), so dedupe by cell id
   (keep the KG with the larger `kg_share`) is correct and nothing is double-counted.
   Free time series: `Δlu[9]` m², `Δbld_cover` m², `Δbld_n`, `Δland` (should be 0). Not
   additive: `line_m_ha` (density; per-cell Δ is meaningful, roll-up needs land-weighting —
   served per cell only), `orient_*` (k-rule ≥ 5 structures, circular — served as "value at t",
   not as Δ), `cov`/`flags` (gates: a cell with `declared_unknown` on either side asserts no Δlu).
2. **Traceability.** Log entry = `(received_at, token, observer, source, epoch, chunk, digest
   [frozen], digest_ap, rows)`; a "seen" entry (no rows) records every later sighting of the same
   `digest_ap`. A delta row is `(cell, from_version_i, to_version_j)` → both digests, both
   `received_at`, both observers come from the log; recompute = subtract the two 30-B rows.
   Nothing in the chain reads `reports.jsonl` or SQL. Audit of one cell: `GET
   …/ne/{kg}/chunks?chunk=<res10>` lists the versions (headers + rows), `GET /ne/cell/{h3}`
   `chg` block names `from`/`to` digest_ap and dates; subtract by hand.
3. **Client cost** (63330 bev build, 52 478 rows, 1 163 res-10 chunks): `chunk_digests`
   58 ms; strip (zero bytes 24–25 of each row) + `chunk_digests_ap` 61 ms → **+0.06 s per
   KG**, no extra memory (one bytearray copy of 1.5 MB, freed), no disk. Bandwidth today:
   939 full reports in 5.0 days, 412 345 chunk digests, 16.3 MB of lines ≈ 37.5 B/chunk →
   per week ≈ 1 150 reports, 580 k chunks, 22 MB. Adding `digest_ap` = +19 B/chunk (+50 %,
   ≈ +11 MB/week) **while every report carries chunks**; the design's step 0 (header-first)
   removes the chunk list only for unchanged KGs, which today is 29 of 968 reports (3 %) because
   the fleet is still in its first pass — the ≥ 90 % figure is steady state, not now. Step 2
   rows: median chunk = 49 rows = 1 470 B raw → **276 B zlib** (0.3 KB, better than the 0.7 KB
   in §3); baseline fill ≈ 278 M cells × 5.2 B ≈ 1.45 GB one-off; weekly ≈ 60 chunks × 0.3 KB.
   Binary chunk-digest encoding (24 B/chunk) would save ≈ 60 % of 33 MB/week ≈ 20 MB/week —
   **not worth a client format change now**; revisit when the fleet is past its first pass.
4. **Server cost.** Yardstick `.nex` `bev` frames (res-9, zstd-19 + `lu-1`): 63330 = 216 911 B
   for 52 478 rows (4.1 B/cell), 19732 = 3 442 B / 504 rows. Stripped rows re-chunked at res 10,
   one zstd frame per chunk: **251 606 B for 63330** (4.8 B/cell, +16 %; no-dict 268 172) —
   nationwide baseline ≈ 278 M cells × 4.8 B ≈ **1.3 GB** (not 0.7), disk has 40 GB free, `.nex`
   is 1.7 GB. Weekly growth ≈ 60 versions × 0.3 KB + seen entries ≈ 50 KB. RAM: Austria has
   ≈ 6.2 M res-10 chunks (278 M / 45); a nationwide "last digest_ap" map would be ≈ 250 MB —
   **so it is a per-KG LRU** (`neChangeLRU`, 256 KGs × ~1 k chunks × 40 B ≈ 10 MB, size line
   `ne_change` on `/debug/inflight`). Reading the log is `os.ReadFile` of
   `{kg}.chunks.log` (63330 ≈ 0.3 MB) + zstd decode; no `search_index.db`, no `database/sql`
   (Invariants 2, 4, 5 hold; Gemeinde roll-ups resolve KG codes in RAM via `ResolveKGCodes`).
5. **Scope boundary.** `strip_register` reduces every LU record to its area+pattern bytes
   (bytes 24–25 zeroed, only LU rows kept) before hashing; `digest_ap` is sha256 of exactly
   those bytes, so a change outside land use leaves `digest_ap` unchanged (pinned by
   `test_change_register_only_does_not_move_digest_ap`). All 52 478 rows of 63330 differ
   between the two hashes, i.e. the frozen digest and `digest_ap` are distinct products.
   Public fields added: `chg{d_lu, d_str_cover, d_str_n, d_edge_m_ha, from, to, status}` and
   `change{…}` — no banned word (`TestNEPublicWordBan` greps the new files); no delta reads
   anything but the area+pattern bytes (`ne_change*.go`, `ne_cells/change.py` → only the
   strip offsets).
6. **Compatibility.** A report without `chunks_ap` and with `chunks{}` takes exactly today's
   path; the answer gains no key unless `chunks_ap` is present (`want_chunks` only then). Pinned
   by `TestNEReportResponseGolden` (key set of the baseline/compared/unchanged answers).
   `/obs` NEC1+NEO1, `.nex`, `ne-cells-2`, frozen files (`scripts/ne_cells_sync_check.sh`),
   `neBevEquivalentTags`, coverage + layout rules: untouched. Header-only body (no `chunks`)
   was a 400 before and is now `unchanged:true` / `202 need:"chunks"` — an addition, not a
   change.
