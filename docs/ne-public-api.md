# NE public read API — spec (H3-keyed fast path)

Status: **implemented** (`ne_h3.go`, `ne_store.go`, `api_ne_public.go`,
`api_ne_vocab.go`, `api_docs_public_ne.go`; tests `ne_h3_test.go`, `ne_store_test.go`,
`api_ne_public_test.go`). Deviations from the first design, all measured:
- `.nex` chunk = **one zstd frame per layer** (`dir[].lu|bev|obs = [off,len,rows]`), not one
  frame holding LU|OBS rows — lets `format=zstd` pass a single layer through byte-for-byte and
  gives obs its own dictionary. Unsorted input rows (never seen so far) additionally keep a
  verbatim zlib body (`raw_off/raw_len`) so the digest stays reproducible.
- Compression: **zstd CLI `-19 -D lu-1.zd --no-check`** per KG (one process, all chunks):
  221 KB vs klauspost 237 KB on 63330's 190 chunks (klauspost's encoder barely uses a
  dictionary). Fallback klauspost when the binary is missing. lu+bev 63330: 526 KB → 478 KB (0.91×).
- `/chunk` multi-KG / multi-layer zstd answers are concatenated frames described by
  `X-NE-Frames: kg:layer:off:len:rows:row_bytes`; `bin` answers carry `X-NE-Rows: lu=30:n;k=29:n[;obs=rb:n]`
  and use Content-Type `application/vnd.ne-rows` (gzip-listed).
- Word ban scope: `api_ne_public.go`, `ne_h3.go`, `ne_store.go`, `api_docs_public_ne.go`
  (the NE docs; `api_docs_public.go` keeps the live `footprint_area_sqm` /context params),
  `llm.txt` NE section, `/api/v1/docs/ne.md`, every public NE response for 63330. The rename
  table lives in `api_ne_vocab.go` (must spell the words). **Public docs never mention the private prefix.**
- `ne_index.db` = header cache (`kgs(kg,nex_bytes,nex_mtime,digests,hdr)` + `chunks`), so startup
  is one sequential read; the RAM index is a sorted `[]{c9,kg,dir}` slice (16 B/ref).
- `pending_kgs` come from the in-RAM KG bbox index (`KGCodesInBBoxMem`, no SQL) minus built KGs.
- A GET for a KG whose `.nec` just landed converts it on the spot (`getOrConvert`, ~0.3 s, no SQL);
  the background converter ticks every 60 s. Obs POST converts synchronously.
- `obs_public:true` (§11): `/ne/cell/{h3}`.obs carries the peer's row decoded (`cover{geb..sonst}`/`obs_lu[9]` as fractions, heights m, ndvi, `verdict`, terrain, `flight_years`, 255/−128 → null) when `record_bytes==41` and `fields[]` equals the observed-layer peer's v2.4 layout (`neObsFieldsV41`, its `/api/v1/cells/dict` neo1_record); otherwise only `row_hex`+`fields`. Decoding is read-side only — bytes are what the peer posted (a peer that reclassifies must re-POST).
Record/container/freeze/determinism contract: `docs/ne-cells.md` (not restated).
Tier rules: `docs/api-tiers.md`.

## 0. Scope & rules

- NE cells are the unit of the API; there is no per-object surface on any tier.
- **Read = public, no key. Write (`/obs`, `/report`) stays the private prefix, token 0.**
- **Keyed by H3 only** (cell id, or lon/lat / bbox resolved to H3 server-side);
  the KG code is allowed as a *bulk* key. Never by parcel/EZ/GNR.
- Complementary to the observed-layer peer: we serve the **declared** layer (`lu`, `bev`)
  + the posted **obs** rows opaque; the observed-layer peer serves trees/buildings/heights detail.
  Join = `cell_id`, client-side.
- **Word ban on the public surface** — responses (JSON keys, strings, headers,
  rewritten container headers), `llm.txt`, served `.md`, **and Go comments /
  identifiers in every file on the public NE path** (`api_ne_public*.go`,
  `ne_h3.go`, `ne_store.go`, `api_docs_public.go`): none of
  `cadastre|kataster|parcel|grundstück|gnr|einlage|\bez\b|footprint|bev_tiles`
  (case-insensitive). Enforced by `TestNEPublicWordBan` (greps the files +
  golden responses of every public NE route for 63330). **KG and
  municipality are fine words.** Public vocabulary: **KG** (5-digit code),
  **unit** (= land unit), **register** (= EZ), **structure** (= building
  outline), **declared / observed**.
  Attribution source id `ne`: `© BEV (CC BY 4.0), bearbeitet · observed layer
  © the observed-layer peer (CC BY 4.0)` — `NENotice()`, never `BEVNotice()`.
- Read path: zero SQL on `search_index.db`, zero warming, zero disk writes.

## 0b. Data boundary — what of the Kataster is in NE

No ids, no EZ/GNR numbers, no geometry, no single-object attribute anywhere.
Fields **derived** from Kataster attributes, all as per-cell aggregates:
`gk` (share of cell land in Grenzkataster, from `status`), `ez_n` /
`parc_per_ez` (register counts), K `parc_p10/p50/p90` (from `area_sqm`,
only where `n_parc ≥ 5`). Everything else is measured from geometry
(shares, counts, metres/ha). One edge: with `n_parc = 5`, p10/p90 ≈ min/max →
the exact m² of one (unidentified) unit. **Public tier therefore emits the K
percentiles log-bucketed** (1/8-octave classes ≈ 9 % steps: `b = round(8·log2(m²))`,
served as the class midpoint) **and p10/p90 only where `n_parc ≥ 10`**
(`unit_p10/p90 = null` below); the private prefix and the binary layout stay frozen/exact.
`gk`, `ez_n`, counts and shares are published as is.

## 1. Storage — one on-disk form, smaller than today (disk efficiency)

Measured on 63330 (52 476 cells, raw rows 1.59 MB): `.nec` single zlib body
262 KB (5.0 B/cell) · res-9 chunks zlib 273 KB (+4 %) · res-10 chunks zlib
337 KB · **res-9 chunks zstd-12 + 16 KB trained dict 229 KB (−13 %)** ·
res-10 zstd+dict 258 KB.

```
data/ne_cells/{kg}.nex          THE store: NEX1 | hdr_len u32 | hdr JSON | chunks…
                                 hdr = {nec1_header, bev_header?, neo1_header?  (verbatim, private),
                                        dict_id, chunk_res:9, dir:[{c9, off, len, n12, nobs}] (sorted by c9),
                                        k_off, k_len}
                                 chunk = zstd(dict)( LU rows | OBS rows ), cell-id sorted — K rows are NOT in chunks
                                 K rows = one zlib stream per KG (~1 % of data; 643 vs 52 476 rows in 63330), kept inflated in RAM
                                          (29 B × ~0.8 M res-10 country-wide ≈ 25 MB, `ne_k_rows` size line) so the public tier
                                          buckets them (§0b) without touching the pass-through chunks
data/ne_cells/dict/{dict_id}.zd  trained zstd dictionaries (16 KB each; `lu-1`, `obs-1`; immutable, served)
data/ne_cells/ne_index.db        chunks(c9 INTEGER, kg TEXT, off, len, n12, PRIMARY KEY(c9,kg)) WITHOUT ROWID; kgs(kg PK, lu_digest, bev_digest, obs_digest, nex_bytes)
```
- `.nec` (python build output) and `.obs` (POST) are **ingest inputs**: the
  converter inflates, verifies the digest, writes `.nex` atomically, re-reads it,
  recomputes the digest from the chunks, then **deletes the input**. Disk ≈
  0.87 × today's `.nec` total (~1.2 GB country for `lu`; obs ~2.5 GB).
  Budget `NE_BUDGET_GB` (default 8), size line `ne_store` on `/debug/inflight`;
  eviction never fires in steady state (invariant 10).
- `ne_all.sh` skip test becomes `{kg}.nec || {kg}.nex` (freeze rule "never
  overwrite" unchanged). Manifest/`/head` read `.nex` headers (no inflate).
- Whole-KG container (`/ne/{kg}`) is **materialised on demand**: chunks →
  rows → one zlib stream → NEC1/NEO1 sections with the stored headers
  (~30 ms CPU/KG; digest identical, deflated bytes not contractual —
  ne-cells.md: "compare digests, never deflate bytes"). Bounded LRU of
  materialised containers: 48 MB RAM (`ne_whole_lru` size line). ETag =
  `"ne-<lu digest>-<obs digest|->"` known before materialising → 304 costs a map lookup.
- RAM index `c9 → []{kgIdx, off, len}` from `ne_index.db` (Austria ≈ 0.8 M
  res-9 cells × ~24 B ≈ 20–40 MB, `ne_index` size line). Hot inflated-chunk
  LRU 64 MB (`ne_chunk_lru`). `.nex` read by `ReadAt` (page cache), never loaded whole.

## 2. Endpoints (public `/api/v1/ne/…`; same under `$UMFELD_PRIVATE_PREFIX/api/v1/ne/…`)

| Method / path | Answer | Cost |
|---|---|---|
| `GET /ne` | `frozen{}`, endpoints, counts, dict ids, `notice` | const |
| `GET /ne/manifest` | existing doc (public). `?fields=digests` → `{kgs:{kg:{lu,obs}}}` ≈ 60 KB gz | memo |
| `GET /ne/{kg}[?layers=lu,bev,obs]` | whole container, `application/vnd.ne-cells`, ETag above, `Range` honoured on the materialised bytes; 404 + `Retry-After` as today | LRU / 30 ms |
| `GET /ne/{kg}/head` | `{lu:{header}, bev, obs, dir_n, bytes}` — headers only (public-rewritten, §3) | memo |
| `GET /ne/dict/{id}` | zstd dictionary, `immutable` | sendfile |
| **`GET /ne/chunk/{h3_res9}`** | **the primitive**: this res-9 chunk (LU+OBS rows; K rows via `/cell`/`/cells`), all KGs that hold it (`X-NE-KGs: 63330:0.8,63333:0.2`), `format=zstd` (default: stored bytes pass-through, `X-NE-Dict`) \| `bin` (raw rows) \| `json`; ETag = sha of the chunk refs' digests → **immutable per digest** | 0 CPU (zstd) |
| `GET /ne/cell/{h3}` | res 12 → `{cell, kgs:{kg:share}, lu:{decoded}, k:{res-10 record\|null}, obs:{decoded via fields[]\|null}}`; res 10 → K record + `children[]`; res 9 → 302 to `/ne/chunk/`; other → 400 | 1 chunk |
| `GET /ne/cells?ids=…` | ≤ 512 ids, res 9/10/12 mixed, columnar json or bin | ≤ 512 chunks |
| `GET /ne/cells?lon&lat[&k≤3]` | res-12 cell of the point + k-ring | 1–7 chunks |
| `GET /ne/cells?bbox=w,s,e,n[&list=1]` | res-12 cells whose **centre** is in bbox (span ≤ 0.05°). `list=1` → only `{chunks:[h3_res9…], etags:[…]}` so the client fetches the chunks it lacks | ≤ ~40 chunks / 0.02° cell |
| `POST /ne/cells {ids:[…]}` | ≤ 16 384 ids | — |
| `GET /context?…&include=ne` | block `ne` = `/ne/cell/{res12 of point}`; `missing.ne` when the KG is unbuilt | 1 chunk |

Params: `layers=lu[,obs][,bev]` (default `lu,obs`), `format=zstd|bin|json`
(default **`bin`** on `/cells`, **`zstd`** on `/chunk`, json via `format=json`
or `Accept: application/json`), `fields=` (json projection, e.g.
`fields=lu,bld_cover,flags`).

Unbuilt KG inside a bbox/k-ring → **partial**: `pending_kgs:[…]`,
`X-NE-Pending-KGs`, `Cache-Control: no-store`; cells of built KGs still
returned. `pending` ≠ "no cells" (latency invariant 1 analogue).

## 3. Public header rewrite (word ban on the container)

The section header is outside the digest. On the public tier the stored
header is emitted with: `attribution`/`notice` → `NENotice()`; `record_lu`,
`record_k` → public field names (table); `input_rule`, `inputs[]` →
`"inputs": {"n": 3, "hash_rule": …, "sha256": […]}` (endpoints/file names
dropped); `stats{}` keys renamed by the same table; everything the observed-layer peer reads
(`magic fmt_ver algo epoch source kg h3_res k_res k cells_n kcells_n groups
ns_table digest coord_decimals input_bbox coverage_bbox input_pad_deg`) is
unchanged. the private prefix emits the stored header verbatim.

| stored | public | stored | public |
|---|---|---|---|
| `kg_share` | `kg_share` | `n_parc` | `n_units` |
| `gk` | `gk` (surveyed-boundary share) | `parc_p10/50/90` | `unit_p10/50/90` |
| `line_m_ha` | `edge_m_ha` | `ez_n` | `reg_n` |
| `bld_cover/bld_n` | `str_cover/str_n` | `parc_per_ez` | `units_per_reg` |
| `bld_p50` | `str_p50` | `*parcels*`/`*footprints*` stats | `*units*`/`*structures*` |
Binary layouts are untouched; JSON answers on the public tier use the public
names, the private prefix the stored ones (one `neFieldName(tier)` table, tested both ways).
Public `format=bin` K rows (from the RAM K table): the three u32 percentiles
carry the bucketed midpoint (p10/p90 = 0 where withheld, §0b); LU/OBS rows and
`format=zstd` chunks are byte-identical on both tiers.

## 4. Resolution

- `lon,lat` → res 12 via **h3-go v4** (cgo; identical `latLngToCell` to python
  h3 4.5.0 — v4 indexing is stable; fixture test: ids from
  a frozen 63330 cell-id fixture). Lookup only — the "python reference
  only" rule concerns record contents.
- Chunk = res-9 parent (`h3Parent`, pure int, exists): 343 res-12 children,
  ~1.2 KB zstd, ~10 KB raw. 0.02° map-peer viewport ≈ 3.3 km² ≈ 10 800 res-12
  cells ≈ 31–40 chunks ≈ **40–50 KB on the wire** (vs ~1 MB of polygons today).
- bbox → `polygonToCells(res 9)` + one ring → chunks → children filtered by
  centre-in-bbox (`cellToLatLng`). k-ring → `gridDisk`.
- Border cells: present in every KG they touch (bytes equal except
  `kg_share`). Dedupe by `cell_id`; `kgs:{kg:share}`; `lu` bytes taken
  from the largest share. In `format=zstd` the client gets each KG's chunk
  (dup rows possible) — `X-NE-KGs` tells it when to dedupe.
- No server-side aggregation of shares (clients sum 1/255 themselves).

## 5. Bandwidth efficiency (what makes it cheap on the wire)

1. **Pass-through zstd chunks** — stored bytes go out as-is (`format=zstd`),
   dictionary fetched once (`/ne/dict/{id}`, 16 KB, `immutable`). No
   recompression, no gzip (add `application/zstd`, `vnd.ne-cells` to the gzip
   exclusion list).
2. **Immutable chunk ETags** — digest-derived, so browser/proxy/CDN caches
   dedupe across users; `list=1` lets a client fetch only chunks it lacks.
   `Cache-Control: public, max-age=86400, immutable` while the digest tuple
   is frozen; partial answers `no-store`.
3. `bin` rows gzip ~6× by the global middleware; `json` is for debugging /
   small answers (`fields=` projection, ids as 15-hex strings, u8 left 0–255).
4. Whole-KG: `immutable` + `stale-while-revalidate=604800`, Range, 304 on
   `If-None-Match` without touching disk.
5. Manifest `?fields=digests` + ETag/304; `/head` for one KG.
6. Per-IP token bucket 20 r/s burst 60 → 429 + `Retry-After` (~0 ms, no queue).

## 6. Limits & targets

| | |
|---|---|
| cells / answer | ≤ 16 384 (bbox over → **413** `{split:[4 sub-bboxes]}`) |
| bbox span | ≤ 0.05° / axis |
| ids GET / POST | 512 / 16 384 |
| k | ≤ 3 |
| server time p50 / p99 | chunk ≤ 0.2 / 2 ms · cell ≤ 0.5 / 3 ms · 0.02° bbox ≤ 5 / 25 ms · 512 ids ≤ 8 / 40 ms · whole KG (LRU hit) ≤ 1 / 10 ms, (miss) ≤ 40 / 120 ms |
| verification | `hey -z 30s -c 16` per route under live load; goroutine dump shows no `database/sql.(*DB).conn` |

## 7. Tier wiring

- `publicPrefixPaths` + `publicAllowListGolden`: `/api/v1/ne` and `/api/v1/ne/`
  GET; **excluded** (private, token 0, `neWriteGate`): `POST|PUT …/obs`,
  `POST|GET …/report`. `neGate` → `neReadGate` (any tier) /
  `neWriteGate`.
- `sourceIDForPath("/api/v1/ne")` → `ne`; `cacheablePath` includes NE GETs,
  excludes partial answers; gzip exclusion for zstd/vnd.ne-cells.
- Public docs: new section in `api_docs_public.go` (`llm.txt`) — table of §2,
  the record layouts with **public names**, decode snippet (JS `DataView`,
  python `struct`), zstd-dict note. Serve a public copy of the contract
  (`/api/v1/docs/ne.md`, generated from the stored md through the same
  rename table + word-ban test). `/api/v1/ne` answers `spec: /api/v1/docs/ne.md`.
- Startup log: `🧩 NE store: kgs=… chunks=… nex_bytes=… dicts=…`.

## 8. Client notes

- Map peers: per viewport cell `GET /ne/cells?bbox=<cell>&list=1`, then fetch the
  missing `GET /ne/chunk/{c9}` (zstd, dict cached once), decode client-side, cache
  by ETag. Render from `lu[9]`, `str_cover`, `edge_m_ha`; `flags.declared_unknown`
  → fog. Point lookups: `/context?lon&lat&include=ne` → K record (`unit_p50`,
  `str_n`, `orient_axis`).
- Observer peers: `GET /ne/{kg}?layers=lu` is tokenless; only `POST /obs` and
  `/report` need the contributor token. Manifest gate: `/ne/manifest` (ETag) or
  `?fields=digests`; per-KG confirmation tick: `GET /ne/{kg}/head`. Header wording
  differs between the private prefix and public (§3); the keys read are the same.
- Observed rows (`verdict`, heights, trees) are joined to declared rows by cell id;
  ids come back as strings, so no H3 library is needed on the client.

## 10. Tests (must exist)
- Golden allow-list has NE GETs, not obs/report; the private prefix without token → 404.
- `TestNEPublicWordBan` over public files, `llm.txt`, `/docs/ne.md`, all public NE responses for 63330 (json + rewritten headers).
- Round-trip: `.nec` → `.nex` → materialised container has the same digest and cell set; `bin` rows byte-equal the inflated source rows for any id set.
- Border cell appears once in `/cells` with both KGs; twice in two `/chunk` answers.
- Unbuilt KG in bbox → 200 + `pending_kgs` + `no-store`.
- ETags change iff a digest changes; chunk ETag stable across restarts.
- Public K rows: percentiles bucketed, p10/p90 zero when `n_parc < 10`; the private prefix rows exact; LU rows identical on both tiers.
- 429 answered ≤ 1 ms; no SQL on `search_index.db` (sqlite trace hook in test).

## 11. Decisions
- Chunk res 9 (default, measured −13 % vs `.nec`) vs 10 (the observed-layer peer's key; +12 %).
- `obs` is public (`neObsPublic=true`): the observed-layer peer agreed to CC BY redistribution by posting (ne-cells.md §Licence) and serves the same rows tokenless itself.
- Dict versioning: a new dict is a new `dict_id`; old chunks keep theirs (header says). Retrain only when a layer format changes.
