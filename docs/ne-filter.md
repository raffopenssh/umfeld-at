# Glyph filter — AND/OR groups over cells at LOD 0/1

Status: **live**. Code map: `docs/ne-glyph.md` Files row *FILTER* + JS map *Filter*; caching:
`docs/payload-caching-contract.md` §Filter index; client-facing: `llm.txt` (`/api/v1/ne/filter`, `/filter/fields`,
`/glyph/fidx`). What shipped differs from the design below in three places — (1) the Worker paints one PNG per
*view tile* (the glyph's LOD-0/1 grid, at the view's pixel density) rather than per bin, so the veil sits on the
same frame box as the data tile beneath (no seam, crisp at any zoom); a LOD-1 tile straddling two bins waits for
both; (2) chips are the AND groups themselves — click a chip to edit it in the input, there is no popover; typed
input gets suggestions (presets → words → fields → members), member *labels* are accepted on input
(`chg=confirmed`, `sub=vineyard`) and canonicalised to ids; (3) `k>=n` is capped at 4 096, groups at 64, the
string at 2 048 chars; nothing is an error — unparsable terms land in `ignored[]` and stay in the input with a hint.
Tests: `ne_filter_test.go` (parse/canonical/words table, bounds quantisation, evaluator vs brute-force reference,
cluster filter, NFX1 round trip + corruption, affine/quadratic fit, **JS parity** — the Worker's parser,
canonical form, words and masks must equal Go's over 50 expressions on a synthetic bin, run through `node`
with `testdata/ne_filter_harness.js` — endpoints incl. 304/immutable/413/404, public surface + protocol
names) and `TestNEPublicWordBan` over the five files.

Changes (mobile / full screen pass): (4) the veil also paints at **LOD 2 while the view spans ≤ 36
bins** (`fltLOD()`, `FLT_BINS`) — a phone at `z=100` sits just under `HI_LOD0` yet shows every cell; at LOD 2 a
tile lists only the bins the view touches (a 0.1° tile holds 16 × 0.4 MB), the Worker updates a tile's bin list on
`want`. (5) The painter **rasterises in JS** (`fillHex` scanline per hexagon + a DDA hairline into one `ImageData`,
then `putImageData` → PNG): canvas `moveTo/lineTo` per vertex took 19 s for a 16-bin tile under software
rasterisation (300 k path ops), the JS fill takes ~150 ms. The PNG and its `<image>` are grown by `FLT_R`=0.2 units
so sheets of neighbouring tiles overlap (no dotted seam). (6) The card row re-asks the Worker while bins are
loading and `fltCardFix()` re-renders once they land; suggestion headers sit above the items they head; on a phone
the row is 14 px like the find list, the input 16 px (no iOS zoom), the list spans the row and opens upward when
the row is in the lower half; `fltAdd` drops the keyboard on coarse pointers. (7) `neFidxVer`=2: bins built before
e8d7338 lacked the log-only res-9 chunks and their state hash had not moved.

Original design (kept as the rationale):

Status at design time: **design, nothing built.** Scope: one filter component on the landing glyph that veils every
res-12 cell *not* matching a boolean expression over object height · slope · aspect · elevation ·
subsidy type · Natura 2000 site · protected area · declared/observed use, plus three derived terms
(contiguity · heterogeneity · edge). Budget: **a chip change repaints in < 100 ms on a 2019 laptop**,
< 250 ms on a phone; the first open of a region may take longer and shows the loading indicator.
Reads first: `docs/ne-glyph.md` (view model, §Area selection, §Paint budget), `docs/invekos-subsidy-layer.md`
(peer bin contract we copy), `docs/ne-public-api.md` §12 (what the obs row carries).

## 0. The one decision

**Data to the client once, predicate + veil on the client, in a Worker.** No server round trip per chip.

- A server tile per filter expression would need `tiles × expressions` cache keys, a 30–60 ms render each
  on a 2-core box, and 12–20 fetches per toggle — never < 100 ms cold, and the subsidy tiles already
  showed what a peer-dependent render chain costs.
- Instead the server serves, per **0.025° bin** (= LOD-0 tile = the subsidy peer' aligned bin), one
  immutable **filter index** (`fidx`, §3): every res-12 cell of the bin with ~14 quantised bytes.
  The client holds the bins in view in typed arrays and evaluates any expression over ~200 k cells in
  ≈ 3 ms; the veil is rasterised per LOD tile in an `OffscreenCanvas` (≈ 2–4 ms per tile) and shown
  as `<image>` elements in an `.ov-flt` SVG group exactly like the other overlays — nothing new in the
  scene graph, pan/zoom are free.
- Everything slow is **async and shown**: a bin index that is not in the server LRU is built on
  demand (50–300 ms, §3c) — the profile word trails dots, the filter chip pulses, tiles whose bins
  are missing stay *unveiled* (never wrongly veiled) until the index arrives.

## 1. UX — chips and one popover, no form

### 1a. Where it lives

The filter is a **selection**, not an overlay: it reuses the area-selection model of `ne-glyph.md`
§Area selection (`{idx, a, row, d, b}` + `drawSelD` + `showSel`) so the veil, the card and the
legend already know what to do with "a set of cells". A filter can coexist with a KG / admin
selection: the veil is then the intersection (cells inside the outline AND matching).

Tool bar `.glyph-tools`: a new **funnel** button (lucide *filter*, `.gt-f`, key `f`) after find.
Pressed = the **chip row** `.glyph-filt` slides in under the view bar (above the time strip), one line:

```
[ ⛉ filter ]  [ height ≥ 8 m ×]  [ slope 15–30° ×]  ( or ) [ N2K ×]   [+ add]   ⟳ 12 % of cells · 1 034 ha   [clear]
```

- **A chip = one term.** Click a chip → its popover opens anchored to the chip (edit the range in
  place); `×` removes it; drag a chip across the `( or )` divider to move it between groups.
- **Groups:** the chip row is `A ∧ B ∧ …` by default. Tapping the hairline `∧` between two chips
  turns it into `∨` and the two chips sink into a bracketed group `( A ∨ B )`. Groups are one level deep
  (conjunction of disjunctions, §2) — enough for every real question ("steep AND (N2K OR nature
  reserve)", "tall structures AND (Acker OR Grünland)") and still readable as chips.
- **`+ add`** opens *the* popover (`.gf-pop`, one `mkPop` instance like photo/roads) listing the
  fields in two columns — *measured* (height · slope · aspect · elevation) · *declared / claimed*
  (use · subsidy class · bio · Natura 2000 · protected area) · *pattern* (contiguity · heterogeneity ·
  edge). Pick one → the popover becomes that field's editor: a **two-thumb hairline range** with the
  live histogram of the cells in view behind it (so the reader sees where the mass is), or a row of
  toggle pills for categorical fields (aspect compass rose with 8 sectors + flat; use groups as the
  legend swatches; subsidy classes; N2K = *any site* or a named site from the find box).
  Committing = release of the thumb / tap outside. There is no *apply* button: **every move of a
  thumb repaints** (`input` event → Worker, result ≤ 1 frame later).
- **Readout** at the row's end: `n % of the cells in view · x ha` + the declared-use bar of the
  matching cells (same `legend()` bars). While bins load: `⟳ building index …` and the figure is
  marked *partial*.
- **Hover a chip** = that single term is previewed alone for the duration (the veil shows what the
  term alone would keep, 120 ms delay, like the find rows preview outlines). Keyboard: chips are
  focusable, `Del` removes, `Enter` opens, `Esc` closes the popover, then the row (`Esc` chain of
  `main` keydown: pop → filter row → pin → full → selection → zoom → full screen).
- **Below `HI_MIN`** (LOD 2, overview): the filter row stays, the chips stay in the link, the readout
  says *zoom in to filter cells*; the res-7 picture is not filtered (no res-7 index for these fields —
  §6 lists it as a later option). The funnel icon takes `off` like trees/diff do.
- **Hints**, once each (`setX.h`): first chip → *veiled = does not match · the un-veiled land is
  what you asked for*; first `∨` → *one of these is enough*.
- **Presets** (three rows at the top of the empty popover, each one link): *steep south-facing
  farmland* · *tall structures outside settlements* · *N2K land under subsidy claim*. They are
  ordinary expressions (§2) — nothing special in code.

### 1b. The veil

Same white veil as a KG selection (`svg.glyph .veil`, .55), but per cell: a tile-sized bitmap where
non-matching cells are white, matching cells transparent, hexagons drawn at their true size, edges
aliased on purpose (the data is cells). Compositing order: **above every data overlay and the
roads, below relief and trees** (relief shades the veil too, so the land stays legible; trees stay
visible everywhere because they are not what is being filtered — unless a tree term is active, see
§6). Under the photo (`;ortho`) the veil opacity rises to .7 so the match still reads.

Fading: the `.ov-flt` group transitions opacity .25 s on toggle; a *re-evaluation* swaps bitmaps
without transition (a thumb drag must feel instantaneous, no cross-fade smear).

### 1c. Card and legend

- The in-view legend gets a **filter block** (`.lg12` after the profile bar, before overlays):
  the expression in words (`height ≥ 8 m · slope 15–30° · (Natura 2000 or protected)`), the match
  figures, the declared-use bar of the match. Matching cells inside a selection: *of the KG* appended.
- The res-12 cell card gets one row `filter · matches | veiled` with the first failing term named
  (`veiled · slope 7° < 15°`) — the reader learns the numbers by pointing.
- `recard()` re-renders on every evaluation (same path as today).

## 2. State, grammar, and the same grammar for agents

One expression language, three surfaces: the chip row (humans), the share link `;f=` (both), and
`GET /api/v1/ne/filter?…&f=` (agents, §2c). What a human builds with chips an agent can paste (and `/api/v1/ne/stats/unit?…&f=` turns into one row of figures — the ▾ next to the link button), and
vice versa — the explorer is the API.

State: `ft = {on, groups: [[term, …], …], hov}`; `ft.groups` is a **conjunction of disjunctions**.
A term is `{f, op, a, b}` (range) or `{f, set}` (set/flag).

### 2a. Field catalogue — everything a cell knows (v1 ⊂ what we hold; ★ = peer column, §4)

| `f` | kind | meaning | source / column |
|---|---|---|---|
| **structures** | | | |
| `hb` ★ | m | tallest structure in the cell | obs peer `bld_h_max` |
| `hbm` ★ | m | median structure height | obs peer `bld_h_p50` |
| `bn` | count | structures in the cell | obs `obs_bld_n` |
| `bcov` | % | structure cover | obs `obs_bld_cover` |
| `roof` ★ | set `flat pitched` | dominant roof form | obs peer `roof_dom` |
| `stor` ★ | count | storeys of the tallest structure | obs peer `stories_max` |
| **trees / vegetation** | | | |
| `ht` | m | tallest tree | obs `tree_h_max` |
| `tn` | count | tree apices in the cell | obs `tree_n` |
| `canopy` | % | crown cover | obs `crown_frac` |
| `hc` ★ | m | canopy p90 | obs peer `canopy_h_p90` |
| `ndvi` | −1..1 | NDVI mean (`ndva` = amplitude) | obs `ndvi_mean / ndvi_amp` |
| `phen` | set `forest crop pasture seasonal bare` | phenology | obs `phenology` |
| `loss` | year | forest loss year (Hansen) | obs `hansen_loss_year` |
| **any object** | | | |
| `h` | m | object height p90 (`hmax` = max, `hmean`) | obs `h_p90 / h_max / h_mean` |
| `dh` | m | nDSM change between flights | obs `dh_mean` |
| **terrain** | | | |
| `sl` | ° | slope mean (`sl90` = p90) | obs `slope_mean / slope_p90`; ★ obs peer for cells without obs |
| `as` | set `n ne e se s sw w nw flat` | dominant aspect | obs `aspect_dom` (★ `aspect_deg` for 16 sectors) |
| `el` | m a.s.l. | elevation mean | obs `elev_mean` |
| `tpi` | m | topographic position (− hollow · + ridge) | obs `tpi` |
| `tri` / `rough` | | ruggedness / roughness | obs |
| `dtm` | m | ground change between flights | obs `dtm_change` |
| `als` | year set | LiDAR flight year | obs `flight_years` |
| **use** | | | |
| `d` | set `geb bau acker gruen wald wasser verkehr alpen sonst` | declared dominant group (`dsh` = its share %) | declared row (log → bev → lu) |
| `o` | set, same | observed dominant group (`osh`) | obs `obs_lu[9]` |
| `ver` | set `consistent forest_loss forest_gain sealed_new structure_new green_new` | the observed-layer peer's declared-vs-observed verdict | obs `verdict` |
| `chg` | set `conf cand` or flag | change seen in the register | `ne_change.go` |
| **subsidy** | | | |
| `sub` | set `acker gruenland alm wein dauerkultur gloez sonst none` | INVEKOS class | the subsidy peer `cls` |
| `scov` | % | claimed share | `cov` |
| `bio` | flag | organic | `bio` |
| `srel` | flag `dubious` | hand-drawn outline reliability < 150 | `rel` |
| `schg` | set `same modified split partial new gone` | claim lineage vs previous year | `chg` |
| `snar` ★ | set of codes or names (`mais`, `wein`) | the crop / Schlagnutzungsart | the subsidy peer `snar`, labels from `/snar_codes` |
| `oepul` ★ | set of measure bits | ÖPUL measures | the subsidy peer `oepul` |
| **protection** | | | |
| `n2k` | flag or set of `AT…` site codes; `n2kt` set `a b c` | Natura 2000 | ours |
| `pa` | flag or set `np ls ns` | protected area (national park · Landschafts- · Naturschutz) | ours (§6 licence) |
| **nearby** (we hold OSM) | | | |
| `rd` | m | distance to the nearest road (class ≥ `road`) | `osm_proximity.db` lines, bucketed 0·25·50·100·250·500·1000+ |
| `wat` | m | distance to water (line or area) | same |
| **pattern** (derived, server) | | | |
| `cont` | cells 1–64+ | patch size of the same declared group | §3b |
| `het` | 0–6 | distinct declared groups among the 6 neighbours | |
| `edge` | 0–6 | neighbours of another declared group | |
| `k` | cells | *post*-filter: keep only match clusters ≥ n cells (client BFS over the IJ lattice) | §3d |

Ranges are closed `a..b`; `a..` / `..b` open one side; `>=8` ≡ `8..`, `<=8` ≡ `..8`. Sets are
`+`-joined. Flags bare. `unk` is a member of every set and every range (`h=unk` = no LiDAR here);
otherwise an unknown value **never matches**. Units are the field's; the popover shows them.
Human-facing labels (`steep`, `south-facing`, `tall structures`, `organic`) are **named ranges** in
one table `FT_WORDS` — `steep` ≡ `sl>=20`, `tall` ≡ `hb>=12` — used by presets, the find box (§2b)
and the words in the legend; the hash always carries the numeric form.

**Hash token** (same `;`-separated hash as every other token, `curHash`/`applyHash`):

```
;f=<group>(,<group>)*          group = <term>(|<term>)*          term = <f><op><val>
;f=h>=8,sl=15..30,n2k|pa=np    →  h ≥ 8 ∧ 15 ≤ slope ≤ 30 ∧ (N2K ∨ national park)
;f=d=acker+gruen,as=s+sw+se,el=..900,cont>=7
;f=hb>=12,sub=none,k>=3        →  tall structures on unclaimed land, clusters of ≥ 3 cells
```

`,` AND between groups · `|` OR inside a group · `..` ranges · `+` sets · flags bare. All characters
hash- and query-safe, no `%` encoding. **`;f=` absent = no filter.** Unknown field names / malformed
terms are ignored one by one (`dbg: filter term ignored`) and the rest applies — the "reserved
token" posture. `curHash` writes the **canonical form** (fields sorted inside a group, groups in
creation order, numbers minimal) so equal filters give equal links and equal Worker cache keys.

Key `f` toggles the row; `;f=` present ⇒ row open on load. `neGlyph.state().filter = {expr, groups,
cells, match, ha, partial}`; hook `neGlyph.filter(expr|null)`.

### 2b. Typing instead of clicking — the find box takes expressions

The find box (`/`) already routes KGs, places, addresses, names. It learns a fourth kind, **filter**:
a query that parses as one or more terms (`sl>=20 as=s`), or whose words are in `FT_WORDS`
(`steep south-facing vineyards`, `tall buildings outside settlements`), shows a *filter* row at the
top with the chips it would create and the live match count in view; `Enter` applies. Mixed queries
work (`Gschnaidt steep forest` → fly to the KG, then filter). Agents get the same via the hash.

### 2c. Agents — the filter as an endpoint

```
GET /api/v1/ne/filter?bbox=w,s,e,n&f=<expr>[&format=count|cells|bin|geojson|h3][&lod=0|1]
GET /api/v1/ne/filter?kg=63330&f=…            KG as the bulk key (its bbox, cells of that KG only)
GET /api/v1/ne/stats/unit?kg=|h3=|bbox=|adm=|share=&f=<expr>   the same filter as ONE ROW of aggregates (docs/stats-envelope.md): figures over the matching cells, unit_* for the whole unit — the export of a filtered view
GET /api/v1/ne/filter/fields                  the catalogue above as JSON: id, kind, unit, range, members, source, coverage, histogram (national, res-7)
```

Same grammar, same `fidx` bins, same evaluator (the Go twin of the worker; `TestNEFilterTwin`
runs both on a fixture bin and demands identical masks). `count` → `{cells, match, ha, by_d[9],
partial_bins}` in ~5 ms a bin; `cells` → H3 ids (≤ 65 536, else 413 → split); `bin` → the matching
rows of the fidx (columns requested via `cols=`); `geojson` → cell hexagons ≤ 4 096; `h3` → ids
compacted (`h3.compact`) for the smallest answer. bbox span ≤ 0.1° (16 bins) per call; the tokenless
20 r/s bucket applies. The answer carries the share link (`"link":"https://example.org/#…;f=…"`)
so an agent can hand its finding to a human, and the page's legend offers *copy as API call* so a
human can hand the question to an agent. Public tier rules (invariant 1): keyed by bbox/KG/H3, word
ban, `NENotice()` attribution + peer attributions when their columns were read. `llm.txt` §7 gets the
grammar and three worked examples.

## 3. Data — the filter index (`fidx`) per 0.025° bin

### 3a. What we already hold (nothing to ask for)

| Field | Where | Coverage |
|---|---|---|
| declared dominant group + share | `neStore.declaredRow` via the view (log → bev → lu) | every built KG (7 850) |
| observed group shares, `obs_bld_cover`, `h_mean/p90/max` (0.5 m), `tree_n`, `tree_h_max`, `crown_frac` | posted NEO1 rows, 41-B v2.4 (`neObsFieldsV41`) | 6 854 of 7 850 KGs have obs; `kgs_obs_on_bev` 3 347 are the ones joined to the bev cell set |
| `elev_mean` (u16 m), `slope_mean`, `slope_p90` (u8 °), `aspect_dom` (8 + flat), `tpi`, `tri` | same rows | same |
| change state of a cell (confirmed / candidate, day) | `ne_change.go` deltas per KG | every KG with ≥ 2 sightings |
| subsidy `cls cov bio rel chg dcov` | the subsidy peer H3 bins already in `neSubLRU` | national, per release |
| Natura 2000 site polygons + codes/types | `data/natura2000_at.geojson.gz`, `ne_glyph_admin.json.gz` | national |
| national parks (WDPA) | `data/nationalparks_wdpa.geojson` | national; **non-commercial licence — §6** |

So height/slope/aspect/elevation **do not need a peer call at all** for ~87 % of KGs — they come out
of the obs rows we already read for `v=obs` tiles. The peers are asked only for the fields the
41-byte row cannot carry (§4).

### 3b. Layout — `GET /api/v1/ne/glyph/fidx?i=&j=[&e=<state>]` (public, tokenless, allow-listed)

One bin (0.025° × 0.025°, `w = i·0.025`, `s = j·0.025`, same index as the LOD-0 tile). Binary,
`application/vnd.ne-fidx`, gzip-listed, **immutable per state** (`Cache-Control: public, max-age=31536000, immutable`
when `e=` is present; `max-age=300` without), strong `ETag` = the state hash, `X-NE-Fidx: n=;unk=;srtm=;sub=;built_ms=`.

```
"NFX1" u8 ver  u16 ncol  u32 n            n = cells in the bin
header JSON (u16 len): {"state":…, "ij":{"origin":h3, "a":[6 or 12 floats]}, "cols":[…], "unk":255,
                        "sub":{"year":2025,"release":…}|null, "srtm":{"epoch":…}|null, "n2k":["AT…",…], "pa":[…]}
rows:  nrow u16, then per row: j i16, nruns u8, (i0 i16, len u8)*      ← cell order = row-major in H3 local IJ
cols:  ncol × n bytes, one column after the other (byte-planar, so gzip sees runs)
```

Cells are addressed by **H3 local IJ coordinates** (`cellToLocalIj` from a bin-centre origin; a bin is
~3 km, far below where the local frame breaks). Row-major runs replace per-cell positions entirely
(~1 KB instead of 70 KB per bin) **and** give the client adjacency for free: the 6 neighbours are
`(i±1,j)`, `(i,j±1)`, `(i+1,j−1)`, `(i−1,j+1)` — a `Map(i<<16|j → ordinal)` built once per bin.
The pixel centre of a cell is `P(i,j)` from the header's polynomial `a` (affine; the server verifies
the residual against the true H3 centres and upgrades to quadratic when any cell is off by > 0.3 px
at LOD 1 — never seen in a 3 km bin, but checked). Hexagon vertices are the 6 fixed offsets scaled by
the LOD, like `neGlyphTileGeomOf/poly` does on the server.

Columns (u8 unless noted; `255` = unknown/no row, `254` = saturated). **Two planes**: the *core*
plane every client needs (≈ 20 B/cell) and the *ext* plane (`&cols=ext`, fetched lazily the first
time a chip from it is added) — a reader who only ever filters slopes never downloads NDVI:

| plane | cols |
|---|---|
| core | `d dsh o osh` · `h90 hmax hb ht` (0.5 m) · `bn bcov` · `sl sl90 as` · `el` (u16) · `sub scov sbio` · `n2k pa` (bitfields, indices into header lists) · `chg` · `cont het edge` |
| ext | `hmean hbm hc tn canopy ndvi ndva phen loss dh tpi tri rough dtm als ver srel schg snar(u16) snar2(u16) oepul roof stor rd wat` |

Coding: heights 0.5 m; shares ×255; slope °; aspect 0–7 N…NW, 8 flat; `n2k` bit 0 inside, bits 1–2
type, bits 3–7 index into `n2k[]` (≤ 31 sites a bin); `pa` bits 0–3 kind, 4–7 index; `chg` 0/1/2
none/candidate/confirmed; `cont` 6 bits capped 64 + bit 7 *touches halo*; `rd`/`wat` distance
buckets 0·25·50·100·250·500·1000+ m (3 bits each, one byte); peer bytes verbatim.

Size: byte-planar columns + IJ runs under the wire gzip → **core ≈ 5–7 B/cell = 80–120 KB a full
farmland bin, 25–50 KB alpine; ext ≈ +60 %**. A desktop view at LOD 1 touches 4–6 bins, LOD 0
12–20 → first open ≈ 0.5–2 MB core, then everything is local and immutable-cached by the browser
(`e=` in the URL, one year). The same view already moves 3–5 MB of PNG tiles. Phones (`deviceMemory
≤ 4`) fetch the view's bins only, no ring. Brotli beats gzip ~25 % on planar bytes: the fidx handler
pre-encodes `br` per LRU entry when the client accepts it (the only route that does — the global gzip
middleware is bypassed for `application/vnd.ne-fidx`, documented in `payload-caching-contract.md`).

`cont/het/edge` are computed server-side over the bin **plus a one-cell-row halo** (the neighbours
across the bin edge are read, so `het`/`edge` are exact everywhere; `cont` is exact up to 64 or marks
*touches halo* when the flood fill left the bin — the client treats bit 7 as "≥ 64" for `cont≥` terms and
as unknown for `cont≤`). Same declared row the tiles read, so a filter never disagrees with the picture.

### 3c. Server build, cache, disk

`neFidxBuild(i,j)`: `neBBoxCells` of the bin + a one-cell halo → `neCollect(lu, obs, bevFirst)`
(the `v=obs` path, 20–60 ms a bin warm) → subsidy bin from `neSubLRU` (or one peer fetch) → peer
ext columns (§4, one fetch per peer per bin per epoch) → N2K / PA membership by point-in-polygon
of the cell centre against polygons whose bbox touches the bin (rings pre-indexed per bin lazily,
~2 ms) → road/water distance from `osm_proximity.db` rtree (one query per bin, distance transform
on a 25 m grid, ~10 ms) → derived terms (one flood fill over ~17 k cells, < 5 ms) →
`cellToLocalIj` + affine fit. **Target: 60–200 ms a bin cold, 2–8 ms from the LRU.**

Caches, all bounded and on `the debug inflight page` (invariant 10):
- RAM LRU `ne_fidx` 48 MB (≈ 400 bins, a working session), single-flight per bin.
- **Disk** `data/ne_fidx/{e}/{i}_{j}.zst` — derived, rebuildable, never committed, **capped at 2 GB**
  (oldest state dirs removed first, then LRU by atime); a cold start serves a bin from disk in ~3 ms
  instead of rebuilding. All of Austria at core+ext ≈ 12 k bins × ~150 KB ≈ 1.8 GB *if* every bin
  were ever asked — it will not be; the cap holds either way. Peer bytes live inside the fidx, so a
  peer is asked for a bin **once per epoch**, not once per reader.
- The agent endpoint (§2c) reads the same LRU/disk, so humans warm agents and agents warm humans.

**State hash `e`** = `neGlyphTileState` of every KG the bin touches (log/bev/obs generation) ⊕
subsidy release ⊕ observed-layer columns epoch ⊕ N2K/PA/OSM file hashes ⊕ `NFX` version. The landing JSON
(`/ne/glyph`) carries `fidx_state` so the client mints immutable URLs from the first byte; a report
or obs POST changes the state → new URL, old entries age out of both caches.

Warm path: when the filter row opens, the client asks the bins **of the view first, then one ring**
(the `hiWant` shape), ≤ 4 in flight, aborted on view change like `hiView`. Server side a low-priority
goroutine prebuilds bins of the last 24 h of LOD 0/1 *tile* requests (off by `NE_FIDX_WARM=0`,
bounded by the caches, idle-only: it yields while any request is in flight).

### 3d. Client evaluation and paint

One `Worker` (`ne_glyph_filter.js`, served next to `soak.js`) owns the bins:

1. `load(bin)` → parse → `{ij→ordinal map, cols: Uint8Array per column, pos: Float32Array centres
   in frame units}`; posted back as a transferable so the main thread never parses.
2. `eval(expr)` → `Uint8Array match` per bin; a term compiles to a closure over one column
   (`col[k] >= a && col[k] <= b`, `mask & (1<<col[k])`); groups short-circuit. 200 k cells ≈ 3 ms.
   Hover-preview of one chip = `eval` of that term alone, same path.
3. `paint(tileSpec, lod)` → the veil bitmap of one LOD tile: `OffscreenCanvas` of the tile's pixel
   size (as `neGlyphLODs`), white fill, then `clearRect`-free: draw the **matching** cells as
   `destination-out` hexagon paths (`Path2D` per bin reused, `translate` per cell) — the veil is the
   complement. ~2.7 k hexes at LOD 1 ≈ 2 ms, ~17 k at LOD 0 ≈ 6 ms; `transferToImageBitmap()`, posted
   back, set on `<image>` via a blob URL (the `hiDecode` queue, so a burst lands over frames).
   A tile whose bins are not all loaded is **not painted** (no veil) and counted in `partial`.
4. The main thread keeps `ft.tiles[key] = {bitmap, expr}`; a new `expr` invalidates all, a pan paints
   only new tiles. `HI_KEEP` applies (same eviction list, veil images count ¼ — 1-bit alpha).

Budget per chip change at LOD 0, 16 tiles on screen: eval 3 ms + paint 16 × 6 ms = ~100 ms in the
Worker *total*, but tiles come back one by one and the first appears after ~10 ms; the main thread
does nothing but swap hrefs. On a thumb drag we coalesce to one `eval` per frame and drop stale results
(`seq` counter) — the same `whl`/`wheelFlush` posture.

Readout = `Σ match` over cells whose centre lies in the view box (the worker returns per-bin counts
and the declared-group histogram of the match; ha = cells × 307 m²), `partial` = any bin missing.

## 4. Peer requests — exact columns, pull-on-demand, nothing re-posted

Principle: **we pull one aligned bin from a peer once per epoch and keep the bytes in the fidx**;
a peer never ships Austria, never re-POSTs, never sees a per-reader request. Both peers already have
the shape (aligned 0.025° bins, `format=bin`, sorted H3 ids, strong ETag, immutable per release) —
we ask each for **extra columns on the existing endpoint**. A missing column is `255/65535` (*unknown*,
never matches), so the filter ships before they do and degrades to "no data" chips, never to errors.

What it costs them, worst case (every bin of Austria asked once): 12 k bins × one request × ~20–60 KB
≈ 0.3–0.7 GB egress **per epoch**, stored per release like their H3 tiles (obs peer: ≈ 8 B/cell × 273 M
cells ≈ 2 GB raw, ~0.5 GB zstd on disk — or zero if computed from the H3B products they already keep,
~40 ms a bin). Realistically a few hundred bins a week. Compare: a NEO1 v2.5 re-POST would move
~3 GB each way and rewrite every `.nex` — **not asked for**.

### 4a. the observed-layer peer — what the 41-byte obs row cannot carry

`GET /api/v1/cells?bbox=<aligned bin>&format=bin&fields=…` exists. Add these per res-12 cell:

| field | type | meaning |
|---|---|---|
| `bld_h_max` | u8, 0.5 m, 0 none, 255 unknown | tallest QA'd structure (`h_robust_m`) |
| `bld_h_p50` | u8, 0.5 m | median structure height |
| `stories_max` | u8 | storeys of the tallest structure |
| `roof_dom` | u8 `0 unknown 1 flat 2 pitched` | dominant roof form |
| `canopy_h_p90` | u8, 0.5 m | canopy p90 — splits *object height* into structure vs tree |
| `aspect_deg` | u8, 2° steps, 255 flat/unknown | aspect in degrees (16-sector rose) |
| `slope_mean slope_p90 elev_mean aspect_deg` | as NEO1 | **for every cell with DTM**, not only cells with an obs row (996 KGs have no NEO1; terrain exists everywhere) |

Shape: one call per aligned bin (`w=i·0.025`, `s=j·0.025`, half-open centres), rows sorted by cell id,
`fields=` subset honoured, ≤ 60 KB gz, ≤ 50 ms warm, built from the stored H3B products, stored per
epoch, strong ETag + `X-Cells-Epoch` (we key the fidx state on it), `If-None-Match` → 304. Listed in
`/cells/dict`. Rate: we hold ≤ 4 in flight and ask a bin once per epoch.

### 4b. the subsidy peer — the crop, per cell

`GET /api/v1/invekos/h3?bbox=<bin>&res=12&format=bin` has `cls cov bio rel chg dcov`. Append:

| col | type | meaning |
|---|---|---|
| `snar` | u16 LE | dominant `snar_code` by area (0 none) |
| `snar2` | u16 | second code (0 none) — a half-maize/half-grass cell matches either |
| `oepul` | u8 bitmask | ÖPUL measures on the dominant Schlag; bit table in `/h3/meta`, bit 0 = bio |
| `nutz` | u8 | AMA land-use category of the dominant Schlag where finer than `cls` |

After `dcov`, named in `X-Invekos-Cols`, `X-Invekos-Version` bumped; the six-column prefix unchanged
(their res-7 `cls2/shr` precedent). `res=7` gains `snar` so the overview can name the crop. Cost to
them: +6 B/cell on tiles they already store (~+30 % of ~160 MB) and one rebuild per release.

### 4c. Prompts to paste (one per peer)

**the observed-layer peer**

> umfeld-at is adding a per-cell AND/OR filter to the landing glyph and the same grammar as
> `GET /api/v1/ne/filter` for agents (design: umfeld-at `docs/ne-filter.md` §4a). We read heights,
> slope, aspect and elevation from the NEO1 v2.4 rows you posted, but the 41-byte row cannot tell a
> tall structure from a tall tree and has no terrain for cells you never posted. Please add, on
> `GET /api/v1/cells?bbox=&format=bin&fields=…` for a 0.025°-aligned bin (`w=i·0.025`, `s=j·0.025`,
> half-open, rows sorted by cell id): `bld_h_max` (u8, 0.5 m, 0 none, 255 unknown), `bld_h_p50` (u8),
> `stories_max` (u8), `roof_dom` (u8: 0 unknown 1 flat 2 pitched), `canopy_h_p90` (u8, 0.5 m),
> `aspect_deg` (u8, 2° steps, 255 flat) — and serve `slope_mean slope_p90 elev_mean aspect_deg` for
> **every** cell with DTM, not only cells with an obs row. Built from your stored H3B products, stored
> per epoch, ≤ 60 KB gz, ≤ 50 ms warm, strong ETag + `X-Cells-Epoch`, `If-None-Match` → 304, listed in
> `/cells/dict`. We pull each bin **once per epoch** (≤ 4 in flight) and keep the bytes in our own
> derived index — no re-POST of NEO1, no per-reader traffic to you. Tell us which header to key on.

**the subsidy peer**

> umfeld-at is adding a per-cell AND/OR filter to the glyph + `GET /api/v1/ne/filter` for agents
> (design: `docs/ne-filter.md` §4b); "subsidy type" needs the crop, not only the class. On
> `GET /api/v1/invekos/h3?bbox=&res=12&format=bin` please append after `dcov`: `snar` (u16 LE,
> dominant `snar_code` by area, 0 none), `snar2` (u16, second, 0 none), `oepul` (u8 bitmask of ÖPUL
> measures on the dominant Schlag, bit 0 = bio, table in `/h3/meta`), `nutz` (u8 AMA land-use
> category where finer than `cls`). Name them in `X-Invekos-Cols`, bump `X-Invekos-Version`, keep the
> six-column prefix as is; `res=7` gets `snar`. We fetch `/invekos/snar_codes?year=` once a day for
> labels and pull each bin once per release as today — same bins, same ETag/immutable rules as v1.2.

## 5. Why this stays < 100 ms (and what we measure)

| step | where | cost | bound |
|---|---|---|---|
| chip edit → expr | main | < 1 ms | canonical string, no DOM rebuild (chips are keyed) |
| eval | worker | ~3 ms / 200 k cells | typed arrays, one closure per term |
| paint one tile | worker | 2–6 ms | `Path2D` hexes, `destination-out`, OffscreenCanvas |
| swap | main | < 1 ms / tile | `href` on an existing `<image>`, no layout |
| first paint after a chip | — | **≈ 10–20 ms** | first tile back |
| full screen settled | — | **≈ 60–110 ms** at LOD 0 (16 tiles), 30 ms at LOD 1 | drop stale `seq` |
| bin index cold | server | 50–150 ms | LRU 48 MB, warm-behind-tiles, immutable URL |
| first open of a region | network | 0.5–2 MB | parallel 4, view first, ring after, loader shown |

Measured by the soak (`m.filter(hash)`): `eval` and `paint` timings from the worker's
`performance.now()` ride back in every message and land in `neGlyph.log().filter{evalMs, paintMs p50/max}`;
the check fails above 8 ms eval / 12 ms per tile on the CI box. Server: `X-NE-Fidx: built_ms=` and
the `ne_fidx` LRU hit rate on `the debug inflight page`.

## 6. Open points and later

- **Protected areas licence:** WDPA is non-commercial (`glyph-find-contract.md` §2 rule 1) — fine for
  a membership *bit* we derive? No: the bit is derived data under the same terms. v1 ships `pa` from
  national park outlines of the Länder open-data portals (CC BY) where available, WDPA bit otherwise
  **off** until replaced; `n2k` (EEA, free) is the v1 "protected" chip. Landschafts-/Naturschutzgebiete
  = one `ne_glyph_admin_build.py` block each once a CC BY source per Land is collected.
- **LOD 2:** a res-7 filter index (`fidx7`, one national file ~16 k rows, histograms per field) would
  let the overview *dim* res-7 cells where no child could match. Cheap, later.
- **Tree terms** (`ht`) veil the trees layer too? v1: the veil goes under trees always; revisit if
  readers ask for "show only the tall trees".
- **Fields we could add from what we hold**, cheap, when asked: distance to the nearest structure
  (declared `geb` cell), distance to a Natura 2000 edge, land price band of the Gemeinde
  (`land_prices`), toponym within n m. One column each; the catalogue endpoint announces them.
- **Server-side mask tiles** stay the fallback if a client cannot run the worker (no `OffscreenCanvas`
  — Safari < 16.4): `GET /ne/glyph/tile?v=flt&lod&i&j&f=<expr>` renders the same veil from the
  same `fidx` (≤ 8 expressions cached per tile). Not built unless needed.

## 7. Build order

1. `ne_filter_expr.go` (grammar: parse → canonical → eval over columns; the Go twin of the worker) +
   `ne_glyph_fidx.go` (builder, RAM/disk caches, handler, `fidx_state`); tests: layout round trip, IJ
   runs cover every cell, affine residual, halo-exact `het/edge`, twin masks identical, word ban.
2. `GET /api/v1/ne/filter` + `/filter/fields` (§2c) on the same evaluator — agents first: it is the
   smallest surface and validates the index before any pixel is drawn.
3. `ne_glyph_filter.js` worker (parse, eval, paint, `k` BFS); `/* ── filter` JS block in `ne_glyph.go`
   (state, `setFilter`, chips, popover via `mkPop`, hash token, key `f`, find-box kind, legend block,
   card row, hook).
4. Soak: `m.filter()`; `layers()` learns `.ov-flt`; `expectOf` reads `f=`.
5. Docs: `llm.txt` §7 grammar + examples, `ne-glyph.md` row + view-model line, `payload-caching-contract.md`
   (br on fidx), this file → status *live*.
6. Peers ship §4 → `hb`/`snar`/… chips leave *unknown*.
