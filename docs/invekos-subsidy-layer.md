# INVEKOS subsidy layer on the glyph (`ne_glyph_sub.go`, `ne_glyph_subpoly.go`)

The glyph's fourth overlay: per NE cell, the share declared as an AMA INVEKOS Schlag
(subsidy-claimed area, hand-drawn by farmers), its class, bio flag, drawing reliability and
— from the subsidy peer' lineage engine — whether the claim moved against the previous year.
Peer: `the subsidy peer` (docs `…/api/v1/invekos/docs/llm.txt` §/h3).

Decision: **aggregate there, paint here, never proxy, nothing on disk.** The polygons
(2.95 M Schläge/year) and the cross-year lineage stay on the peer; it rolls them onto H3
and we only turn the per-cell bytes into pictures, sums and one cell's reading. Since
Now the tiles paint the **outlines themselves** (peer's IVP1 bins, §outlines; all
three LODs) — a Schlag as the farmer drew it, 0.5 m simplified — and the hexagons stay the
source of the per-cell sums, the cell card and the fallback.

## Peer contract (v1.2, what we consume)

`GET …/invekos/h3?bbox=w,s,e,n&crs=4326&res=12&format=bin` with the bbox aligned to our
LOD-0 grid (`w = i·0.025`) — their stored tile, ~5 ms warm, 7–30 KB gz; unbuilt tiles of
other years 0.3–1.5 s (we only ask for the default year).
Layout: LE `u32 n`, `n × u64 cell` (sorted), then byte columns `cls cov bio rel chg dcov`
(`X-Invekos-Cols`). `cls` 0 none · 1 acker · 2 gruenland · 3 alm · 4 wein · 5 dauerkultur ·
6 gloez · 7 sonst; `cov` claimed share ×255; `bio` ×255 capped at 254 (254 = all), 255 =
unknown; `rel` ×255, < 150 = dubious hand-drawn outline; `chg` vs prev year 0 same · 1
modified · 2 split_merged · 3 partial · 4 new · 5 gone (gone cells present with `cov 0`);
`dcov` = Δcov int8. Headers `X-Invekos-Year/Prev-Year/Release/Prev-Release/N/Cols`, `ETag`,
`Cache-Control: public, max-age=86400`, `X-Data-Attribution`.
`?res=7&format=bin` = the national overview (≈ 16 k rows): same six columns plus `cls2`
(second class) and `shr` (dominant class share of the claimed area ×255); there `chg` is
**a share** (claimed area with any lineage event ×255), not a kind, and `rel` is 0.

## Outlines (IVP1, `ne_glyph_subpoly.go`)

`GET …/invekos/poly?bbox=w,s,e,n&crs=4326&format=bin`, bbox = one aligned 0.025° bin (our
LOD-0 tile). Body `"IVP1" u32 n` then per record `u8 cls bio rel chg flags`, `uvarint area_m2`
(this year; 0 for gone and for bit1 continuation parts), `svarint d_area_m2` (vs the best-linked
predecessor; gone = −prev area), `uvarint nrings`, rings of `uvarint npts` + zigzag deltas in
**bin-local units** (origin bin SW, 1 unit = 0.025°/4096 ≈ 0.46 × 0.68 m, signed, unclipped —
a Schlag touching two bins is in both; giants > 2 bins clipped ± 10 % → flags bit0). `bio` 0/1/255.
Includes the `gone` Schläge of prev_year (chg 5). Measured: farmland bins 1–40 KB gz (p95 75 KB),
Alm bins up to 183 KB, stored per release on their side (~5 ms warm, ≤ 2.3 s cold), strong ETag,
max-age 86400. No ids, no names — nothing to re-serve, and we never do.

Our side: `neSubPolyAt(i,j)` through the same fetch path as the H3 bins (single-flight, ≤ 4 in
flight, 8 s, ETag revalidation, 30 s back-off) into its own LRU `neSubPolyLRU` (24 MB raw;
`ne_glyph_sub.poly` on `the debug inflight page`); `neSubPolyParse` rejects a body that does
not decode (not cached). `neGlyphSubRender` fetches the outline bins of the padded box beside
the H3 bins; when every one is there `neGlyphSubPaintPolys` paints instead of the hexagons
(`neFillRings`: even-odd scanline with an active-edge sweep, holes kept, clipped to the
picture; `neStrokeRings` edges), else the hexagons as before — the layer never goes dark for
a missing endpoint. Rendering a warm tile: 15–70 ms.

Counts (meta `pn;pchg=6;pha;pnew;pgone`, m²): a Schlag is counted in the tile whose half-open
box holds its **first vertex**, from the bin that holds that vertex — once nationally, no
clipping, continuation parts (bit1) skipped. `pha` is therefore the area of whole Schläge
anchored in the tiles on screen, not the claimed area of the view (that is `cov/cells`).

Cell card (`cell?sub=1`): `sub.schlag {area_ha, class, bio, reliable, vs_prev, d_area_ha?
(modified/split/partial), prev_area_ha? (gone), clipped?, year, prev_year}` = the outline
under the cell centre (`neSubSchlagAt`, even-odd point test; this year's claim first, largest
wins, else a gone one). Absent when no outline holds the point or the outline bin cannot be had.

## LOD 2: one cut, like every other view

The subsidy layer behaves exactly like the declared / observed tiles at all three LODs: one
`v=sub|subc` tile per box, painted from the H3 bins + IVP1 outline bins (§outlines) at LOD 0, 1
**and 2** (the 0.1° box = 16 bins + seams, fetched side by side, ≤ 8 to the peer). No zoom
threshold of its own, no second cut, no res-7 wash under the full-res tiles. The `&f=1`
parameter of the earlier "fine cut" is accepted and ignored; cache key `…/2/i/j/day`.
Cold LOD-2 tile 2–8 s (32 peer bodies, alpine boxes the slowest), warm ≈ 0.5–1 s, then the tile
LRU — the same order as a cold declared LOD-2 tile (3–12 s).

Why the two cuts went: the res-7 wash arrived first and was swapped for the Schläge a few
seconds later (the "wash flash"), had its own threshold (`HI_SUBF = 40`), its own cull rule,
its own cache key (`~f`), its own legend text and its own 204 handling. Every one of those
was a way for the layer to behave unlike the profile under it.

**What is now identical to declared / observed** (client `ne_glyph.go`, `/* ── subsidy`):
- *Auto-on:* the tiles come with full resolution (`hiAuto`, `HI_MIN`) — `hiView` asks for
  `subView()` tiles of every wanted box at the LOD in use, nothing else gates them (the
  overview `/ne/glyph/sub` failing no longer blocks the tiles; it is re-asked on the next
  move after `NET_TTL` like every other source).
- *Under-LOD cull:* `hiUnderMiss` counts the subsidy tiles like the profile's overlay — the
  tiles of the other LODs stay as the backdrop until base **and** subsidy of the LOD in use
  are complete, then hide. In addition (the fills are translucent, the opaque views do not
  need it) a subsidy tile of another LOD steps back per tile as soon as every tile of the
  LOD in use covering it is in (`hiCull`), so fills never double during the swap.
- *Eviction:* `hiEvict`, `HI_KEEP`, off-screen first — one key shape `sub/lod/i/j`.
- *Failure:* a 204 `down=1` is a failed tile like a 5xx of any view — `hiDrop`, `hi.bad`
  backoff `NET_TTL`, the `subsidies` mark (`netFail`), hint *offline · subsidies did not
  answer — retries on your next move*; the next pan / zoom asks again. No timer, no 8 s hold.
- *The button:* never `off` (not in `avail()`); `busy` while a subsidy tile of the LOD in
  use is pending; the legend says *loading subsidy tiles…*, then the figures, or *subsidy
  layer unavailable* only while the `subsidies` mark stands and nothing is on screen.
- *Base dimming:* one rule, `.8` under the layer at every LOD (`.hi-near`/`.sub-fine` split gone).

Soak (`ne_glyph_soak.js` `run`): `sub`, `subc`, `obs+subc` at z 400 / 200 / 20 (LOD 1 / 0 / 2)
and a LOD walk 2 → 0 → 1 → 0 → 2 with the layer on; `layers()` asserts at every LOD: sub
tiles = base tiles, no `~f` key, no subsidy tile of another LOD painted once the under-LOD
cull ran, button neither `off` nor `busy`. `m.offline` covers the peer-down path.

## Our side

| Piece | Where |
|---|---|
| fetch · LRU (`ne_glyph_sub`, 32 MB, TTL = their max-age, `If-None-Match` revalidation, stale-while-down) · ≤ 8 in flight · 8 s · single-flight · 30 s back-off after a refused connection / 5xx (a timeout only fails its tile) | `ne_glyph_sub.go` `neSubFetch/neSubGet/neSubBinAt/neSubOverview` |
| `v=sub` tile (Schläge as drawn) / `v=subc` tile (lineage), one cut at every LOD (§LOD 2; `&f=1` ignored); 204 + `X-NE-Tile: …down=1` while the peer is down; cache key carries the day | `neGlyphSubRender`, wired in `ne_glyph_tile.go` (`neGlyphTileRender`, `handleNEGlyphTile`) |
| `GET /api/v1/ne/glyph/sub` overview columns in `cells[]` order (`cov cls bio dcov chg cls2 shr`, max-age 86400, 503 no-store when down) | `handleNEGlyphSub` |
| `GET /api/v1/ne/glyph/cell?…&sub=1` → `sub{claimed, claimed_pct, class, bio, reliable, vs_prev, d_claimed_pct, year, prev_year, release}` or `sub:null, sub_unavailable:true` | `neSubCellAt`, `handleNEGlyphCell` |
| licence row `invekos`, sibling service entry, footer, `X-Data-Attribution` on every response that shows the layer | `api_license.go`, `api_docs_public.go` |
| size line `ne_glyph_sub` on `the debug inflight page` | `debug_pprof.go` |

**Painting rules** (`neGlyphSubRender`): one hue, the page's `--gsub` (Okabe-Ito orange
`#E69F00`, dark `#F0B429`), colour-blind safe against the teal observed and the vermilion
change. Palette 0 transparent · 1–8 wash alpha .3 → .9 · 9 full stroke · 10 light stroke.
It is an overlay: no outline fill goes past ≈ .64, the ground must read through.
- *Declared view* (`v=sub`), every LOD, with outlines: every Schlag of the year filled at shade 3
  (≈ .47) + 1 px edge; pixel-hatched (`neFillRings` mask 1) when `rel < 150`; `gloez` (hedges,
  verges) edge only, never a fill; gone ones not painted. Hexagon fallback: wash per cell,
  alpha = `cov`, hatched when dubious, gloez as a thin ring. The page lifts the base to .8
  under the layer.
- *Change view* (`v=subc`), every LOD, with outlines: the analogue of the vermilion change cells in
  the layer's ochre — **new** = shade 5 (≈ .64) + 2 px edge, **redrawn** (modified / split_merged /
  partial) = shade 3 (≈ .47) + light hairline, **gone** = shade 2 (≈ .39) hatched + 1 px dotted
  edge, same = nothing. Hexagon fallback: lineage as the outline of each run of same-kind cells
  (`neSubOutline`; new 2 px, gone dotted, redrawn hairline, never a fill).
- *Observed profile*: painted the same way over `ov-obs` (view model in
  `docs/ne-glyph.md`).
- Seams: the client clips each sub tile image to its box (`clip-path: inset(bleed)`) so the
  translucent wash never doubles at tile edges.

`X-NE-Tile` adds `sub=n;cov=Σ;cls=8 cov-weighted sums;bio=Σ,unknown;dub=n;chg=6 counts;
dcov=Σ;cells=res-12 cells in the box;year=;prev=;rel=[;pn=;pchg=6;pha=;pnew=;pgone=]`;
sums count each cell once (centre in the tile's own half-open box); the `p*` fields are the
outline counts (§outlines) and mark a tile painted from outlines.

**Client** (`ne_glyph.go` JS, block `/* ── subsidy`): state `sb {on, d (overview), err}`;
`subVis()` = on and not observed; `setSub` (hash `;subsidy`, key `s`), `subSync` (two
buttons `.gv-sub` (Lucide `tractor`) after *declared* and `.gv-sub2` (Lucide `file-pen`, the form) after *change*, same state, the one
not attached to the active view steps back), `subLoad/subBuild/subHex` (overview `.s`
dots/faces by four cov buckets under the ink; `svg.v-sub` dims the ink to .14), `subSum`
(tiles on screen) / `subSumOv` (res-7 cells in the view box, class shares honest via
`shr`/`cls2`), `subLegendHTML` (the block appended to the in-view legend), `subCellLine`
(res-12 card; fetches `?cell=&sub=1` lazily when the doc predates the toggle),
`subDotLine` (res-7 card), `subKGLine` (KG card, sum over its res-7 cells).
`neGlyph.state().subsidy`, `neGlyph.subsidy(bool)` for scripted checks.

Legend text (every figure named): declared view "n % of the land in view claimed (year-release)
· bio n % | bio unknown · dubious drawing n % [· m Schläge · x ha]" + class bar; change view
with outlines "k of m Schläge moved · Δ claimed ±x ha" + items new n (+ha) / gone n (−ha) /
redrawn n / same n with filled swatches (`subSw(k, fill)`), hexagon fallback "n % of the claimed
cells moved" with stroke swatches (ha = dcov/255 · 307 m² per res-12 cell, 516 ha per res-7
cell — Δ claimed is always the net H3 `dcov`, never Σ d_area, which is partial for splits).
Hover peek (collapsed = what the picture shows): with an outline under the cell "x ha · class ·
bio · dubious drawing [· vs prev: kind ±ha]", else the cell reading "claimed n % · class …";
the card row keeps the cell reading and appends "Schlag x ha (±Δ)".

## What we asked the peer for

IVP1 outline bins (`/invekos/poly`): shipped, verified (ETag/304, sizes, decoder
round trip, 19 giants clipped + flagged, bit1 continuation parts). Nothing open. Note for
readers of the picture: thin spikes along a field edge are in the data (farmer-drawn slivers,
`rel` < 150 — hatched in the declared view), not a rasterising artefact.

Everything else shipped in the peer's v1.2:

ETag, `X-Invekos-Prev-Year`, res-7 `cls2`/`shr` and a lineage share, the 254-cap note,
ASCII attribution header. Open: cold tiles of non-default years (we don't request them).
