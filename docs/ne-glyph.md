# The landing glyph — code map

Read this before touching `ne_glyph*.go`; it replaces reading the 200 KB file. Narrative and
decisions: `docs/ne-change.md` §6a–6c, time strip + ageing §6e; subsidy layer: `docs/invekos-subsidy-layer.md`.

## Files

| File | Holds |
|---|---|
| `ne_glyph.go` | Go: res-7 frame (`neGlyphFrame`, `neGlyphLoadFrame` from `data/ne_glyph_austria_r7.json`, hex fit `neGlyphHexFit`), per-cell aggregate from the reports scan (`neGlyphAgg.add/fold`, `neGlyphBuild`), the SVG (`neGlyphSVG`: `g.dt` dot paths `.e .h .n .o .ch .ca .c` as `M x y h0` strokes, bucketed opacity), the JSON (`neGlyphJSON`: `cells[] = [x,y,chunks,passes,reports,age,obs,chg,kg,bands,cand]`, `kg_codes[]`, `proj{}`), KG search index (`neGlyphKGIndex`). Then the page: `neGlyphHTML()` (figure, tool bar, view bar), `neGlyphCSS` (one string, ~150 lines of rules), `neGlyphScript` (one IIFE, ~800 one-line functions; see map below). |
| `ne_glyph_tile.go` | Full resolution: `neGlyphLODs` `{0.025°,40px}` `{0.01°,100px}` `{0.1°,10px}` (indices 0/1/2; LOD 1 = nearest), `neGlyphTileGeomOf/poly` (hex → tile pixels via `neGlyphProj`), `neFillPoly` scanline, `neGlyphTileRender` (switch on view: base / `obs` / `tree` / `chg` / `sub` / `subc`), `neGlyphTileMeta.String` → `X-NE-Tile`, `handleNEGlyphTile` (LRU `ne_glyph_tiles` 24 MB, key `view/lod/i/j[/day]`, meta trailer joined to the PNG), `handleNEGlyphCell` (lon/lat or `cell=`, public cell doc + `pic_obs` [+ `sub` with `sub=1`]), `handleNEGlyphKG` (KG outline at res 12). |
| `ne_change_passes.go` | Passes per KG / national (`passes`, `table`, `neChangeWin`/`neParseWin`, `cumWin`), `/ne/glyph/passes`; `ne_glyph_tile.go` ages chg tiles (`neGlyphAgeScale`, `neGlyphChgIdx`). |
| `ne_glyph_sub.go` | Subsidy layer: peer fetch + LRUs (H3 bins, overview, IVP1 outline bins), `v=sub|subc` painting (bins + outlines, one cut at every LOD, `docs/invekos-subsidy-layer.md` §LOD 2), `/ne/glyph/sub` overview, `neSubCellAt`. |
| `ne_glyph_dif.go` | DIFF overlay (`v=dif`, LOD 0/1): declared row vs observed row of the same cell, today — `neDifStatus` (ok/edge/mixed…), `neDifKind` (the three corroborated kinds of ne-change.md §6d, `raw` lifts the corroboration), `neGlyphDifRender` (solid / chequer / ring, one hue `neGlyphDifRGB`), `neGlyphDifMeta` → `;obs=;dif=k1,k2,k3;cmp=;[raw=1;]als=y0,y1;mix=`, the lidar epoch table `neDifAlsOf` (the observed-layer peer's `kgs?fields=codes,als`, one conditional GET per hour — `If-None-Match` with the peer's ETag, a 304 keeps table and stamp, `neDifEpochStamp` in the tile key, `no-store` until loaded), `neGlyphPicDif` (cell doc `pic_dif`), `NEGlyphDifStats`. |
| `ne_glyph_roads.go` | ROADS overlay (`v=rd`, LOD 0/1): OSM roads + rail from `data/osm_proximity.db` (`osm_lines` cat road/rail, the same rows `/api/v1/osm/geometry` serves) drawn server-side — `neRoadClassOf` (major · road · minor=service/pedestrian/living_street/busway dashed · rail), `neRoadInk(cls, attr, lod)` the ONE place class + per-line attributes become ink (`neRoadAttr.Remote` = mean remoteness 0–100 of the chunk's vertices from `remoteGridGet()` → `neRoadRemoteGain` .5 + r/100 on alpha (clamp .92) and + ≤ .5 px width, roads only; −1 = grid not ready → class base; each line's weight rides in the coverage via the stroke's `gain`, one composite per class), `neRoadStroke` signed-distance AA stroke with max-blend coverage (joins never darken, dashes cut along the arc with soft ends), `neRoadComposite` src-over, passes casing → minor → rail (+ white ties at LOD 1) → road → major, `neRoadQuantize` → 256-colour palette (32 alpha × 8 ink/casing mixes; ~75 KB a dense LOD-1 tile vs 360 KB NRGBA), meta `;rd=;rdc=major,road,minor,rail;km=` (`clipLenM`: length inside the box so tiles add up) `;rm=;rmb=<25,25–50,50–75,≥75 km` (length-weighted mean / bands of the roads' remoteness, absent until the grid is ready), 204 `down=1` without the db. Achromatic by design (colour-blind safe); `ne_glyph_roads_test.go`. |
| `ne_glyph_subpoly.go` | Subsidy outlines at LOD 0/1: IVP1 decoder (`neSubPolyParse`), bin units → tile px (`neSubPolyMapOf`), `neFillRings` (even-odd, holes, active edges) / `neStrokeRings`, `neGlyphSubPaintPolys` (declared fills / change fills by kind + Schlag counts), `neSubSchlagAt` (cell card). |
| `ne_glyph_export.go` · `api_ne_stats_unit.go` · `ne_mvt_jobs.go` | EXPORT: the link button is a split control `.gt-kk` = `.gt-k` (copy the share link, unchanged) + `.gt-kx` ▾ (menu `.gt-xm`, a small popover — left-anchored on desktop, right-anchored ≤ 640 px — with two rows and a URL line: EXPORT JSON · CSV (download) · flat · Sheets formula `=IMPORTDATA(…csv)`; QGIS `tiles` (copies the MVT URL) · `PMTiles` (archive link, z 9–14, of the selected KG/area via `share=` or else the current view via `bbox=`; when the estimate `xArchMB` = km² × 0.17 MB (measured 0.15–0.17) exceeds `XMB`=100 the link is replaced by *view too big — zoom in*); optional key field, localStorage `ne.key`, appended as `&key=`). Every unit entry is a LINK to `/api/v1/ne/stats/unit?share=<unitHash()>&format=…&src=glyph` — `unitHash()` keeps only `kg= h3= adm= pt= f=` of `curHash()`, so the export is the share link rendered by another endpoint (`docs/stats-envelope.md`). Without a unit the first row says *pick a KG, Gemeinde, cell or point*; the QGIS row works anywhere. Open menu = `;export` in the share hash (restored by `applyHash`). `xOpen` measures: when the bar sits in the lower half (full screen, phone keyboard) the menu opens upward (`.gt-kk.up`) and is nudged inside the viewport. Nothing fetches on open; the endpoint counts (`export_gate.go`, line `export_gate` on /debug/inflight). |
| `ne_glyph_filt.go` · `ne_glyph_fidx.go` · `ne_filter_expr.go` · `ne_glyph_filter.js` · `api_ne_filter.go` | FILTER (`docs/ne-filter.md`): the funnel button `.gt-flt` after the find box + the chip row `.glyph-filt` (`neGlyphFiltBtn/HTML/CSS/JS`, spliced into `neGlyphHTML`, `neGlyphCSS`, `neGlyphScript` before `applyHash()`); the per-0.025°-bin index `fidx` (NFX1: IJ runs + planar u8/u16 columns, header polynomial IJ → viewBox, `neFidxBuild` from NE store + observed-peer cells + INVEKOS h3 + Natura 2000 + OSM distances + chunk log; LRU `ne_fidx` 48 MB → disk `data/ne_fidx/<state>/` 2 GB → build; `handleNEGlyphFidx` ETag `"<state>-<plane>"`, `neFidxWarmLoop` warms bins the tile handler saw); the grammar twin (`neFiltParse/Canonical/Words/Eval`, mirrored byte-for-byte by the Worker — `TestNEFilterJSParity` runs both over one synthetic bin); the Worker (parse · evaluate · `k` BFS · paint one PNG per view tile via OffscreenCanvas → blob URL → `<image>` in `.ov-flt`); the agent endpoint `/api/v1/ne/filter` + `/filter/fields`. |
| `ne_glyph_test.go` | Hex fit, SVG shape, tile meta, word ban is in `api_ne_public_test.go` (`TestNEPublicWordBan` scans `ne_glyph*.go`). |
| `api_docs_public_ne.go` | `llm.txt` NE section: glyph endpoints + the landing inventory §1–9 (share-link grammar §7, scripted checks §7a). Update with every behaviour change. |
| `api_license.go`, `api_docs_public.go` | Sources `ne`, `invekos`, sibling services, footer. |

Routes: `api_ne.go` switch `rest == "glyph" | "glyph/kgs" | "glyph/tile" | "glyph/cell" | "glyph/kg" | "glyph/sub" | "glyph/fidx" | "glyph/filter.js"` (+ `filter`, `filter/fields` before `neParseReq`, own `format=` vocabulary);
public via the `/api/v1/ne/` prefix + `publicAllowListGolden` (`api_tiers_test.go`).

## Page anatomy

Tool bar `.glyph-tools`: find `/`, recent, link, full screen (no separators between them) · status
slot `.gt-n`. The full-res button `tD` (`.gt-d`) is still wired (busy spinner, aria state, soak)
but `hidden`: full resolution is automatic (`hiAuto`), the status slot took its room.
`change` and `diff` carry a `.gt-t` word after the icon under the same rule as find's label (phones /
coarse pointers show it, a desktop bar is icons only). View bar `.glyph-view` (`gv`) = **profile** radio pair `.gv-p` (`declared`
`.gv-b[data-v=""]` · sep · `observed` `.gv-b[data-v=obs]`) · gap · **overlay**
toggles `.gv-o` (`.gv-t.gv-chg` lucide *activity* (a pulse = change over time) · `.gv-dif` lucide *git-compare* (two sides of one cell) · `.gv-sub` tractor · `.gv-tree` · `.gv-relief` · `.gv-ortho` lucide *earth* (the photo) · `.gv-rd` lucide *route* (roads); the two opacity popovers `.gv-op.gv-op-ot` / `.gv-op.gv-op-rd` sit in `.glyph-side`, absolutely positioned, never in the flow, one `mkPop` factory drives both);
`aria-pressed`, `off` = cannot show at this zoom, `busy` = pulsing while its tiles load; the profile
words trail dot loaders in the 2ch slots). Card `.glyph-tip` (`tip`, `inner`): floats
(`--float:1`) or docks (`.dock`), `.open` shows the `.dt` detail blocks; rows `.rw > .lb + .vl`
(label / one-line value, `…` by `text-overflow`), legends `.lg` of `.it` items with `sw()`
swatches, bars `.pb` of `seg()`, notes `.nt` (`.kv` figures, `.mut` muted, `.dx` indented),
section heads `.sh` (`.lg12` = in-view legend block appended to every card).

SVG layers (bottom → top): `.hx` hex-face tiles (built client-side at zoom ≥ 1.7, `buildHex`,
one `<g data-b>` per 560-unit tile with paths `.h .n .e .o .ca .c` [+ `.s`]) · `.dt` dots ·
`.hi` full-res group (`hiLayer`: [`<g class="ov ov-ortho">` the photo, inserted first by `otLayer`,] `<g class="b">` base, then one `<g class="ov ov-<v>">` per overlay
view in z-order `obs chg dif sub subc rd rel tree`, each with three LOD sub-groups `data-lod=0|1|2`,
stacked 2 → 0 → 1) · selection veil / outline · cursor hexagon `.c12`. Classes on `svg.glyph`:
`v-obs` (profile) `v-chg v-dif v-sub v-rd v-tree v-rel v-ortho` (overlays, independent) `hi hi-near on` (`hi-near` =
LOD 0/1 in use, set by `hiCull`).

## View model — one profile × any overlays

The picture is **one required profile** (what fills every cell) plus **independent overlays**
(marks on top). A toggle never changes the profile and never moves the view; what cannot show at
the current zoom fades (`off`) and stays in the link. State: `view` (`'' | 'obs'`, the profile) ·
`cg.on` change · `df.on`/`df.raw` diff · `sb.on` subsidy · `hi.tree` trees · `rl.on` relief. Everything else (CSS classes,
bar, hash, legend, card order, tile wants) is derived — `neGlyph.state()` returns
`{profile, overlays:{change,diff,subsidy,trees,relief}}` and `dbg` logs every `set*`.

| | **declared** (fill = declared use, `bev`-first) | **observed** (`;v=obs`, fill = the observed-layer peer's use over the declared fill at .3) |
|---|---|---|
| *(none)* | all LODs · legend: `in view · declared` + use bar | all LODs · `in view · observed` + use bar + "n % of the cells observed" |
| **change** `;change` · key `c` | ✓ O·2·0·1 — dots `.c .ca .ch` over faded `.h`; tiles `ov-chg` over the base at **.4** | ✓ O·2·0·1 — dots `.c` over `.o`; tiles `ov-chg` over `ov-obs` at **.4** (base stays .3) |
| **subsidy** `;subsidy` · `s` | ✓ O (res-7 dots/faces) · 2·0·1 Schläge as drawn (`sub`, fill dimmed .8), the same tile rules as the profile at every LOD (auto-on, under-LOD cull, eviction, fail-soft; never `off`). **With change: variant `subc`** (new / redrawn / gone vs 2024) | ✓ same rules; `ov-obs` dims like the base does (.8) |
| **trees** `;trees` · `t` | ✓ 0·1 only (`off` below 110×) — crown rings topmost · *was: forced the observed view* | ✓ 0·1 only |
| **relief** `;relief` · `r` | ✓ 2·0·1 (full res only; weight eases to ½ past 300×) — hillshade blended over every fill, under the trees | ✓ same |
| **diff** `;diff` · `;diff=raw` · key `d` | ✓ 0·1 only (`off` below 110×) — magenta marks where the register and the LiDAR disagree about the same cell (ne-change.md §6d, the three corroborated kinds; `=raw` lifts the corroboration): solid *structures declared · none in the LiDAR*, chequer *structures in the LiDAR · none declared*, ring *declared forest · currently no trees*; `ov-dif` over the base at **.4** (like change); legend head = lidar epoch as an age | ✓ same over `ov-obs` at .4 |
| **time** `;t=` · keys `< >` space | ✓ with change at every LOD — the strip under the view bar (passes · days of a pass · calendar), chg tiles/paths windowed and **aged** (fresh hue → ash, floor .18, scale = min(history, 1 y)); play = day-by-day at level 2, paths only, tiles/card/legend catch up on pause (ne-change.md §6e) | ✓ same over `ov-obs` |
| **photo** `;ortho[=0.1–1]` · `o` | ✓ 0·1 only (`off` below 110×) — basemap.at orthophoto **under the base** (`ov-ortho`, first child of `.hi`), weight = the reader's (default .5, `OT_OP`): ≤ ½ the photo just fades in beneath the cells, past ½ the profile fills step back (`--otf` on the svg → `filter:opacity()` on `.b`/`.ov-obs`, .35 at 1) so it reads through; `saturate(.8)` keeps the data colours in front. Popover `.gv-op` (hairline range, bar head): click = toggle + show for a moment, hold 380 ms / right-click / wheel = adjust without toggling, hover on a pressed icon shows it; anchored above the icon's own row (the bar wraps on phones) | ✓ same under `ov-obs` |
| **roads** `;roads[=0.1–1]` · `w` | ✓ 0·1 only (`off` below 110×) — OSM roads + rail (`v=rd`, `ne_glyph_roads.go`) over the fills, change, diff and subsidy, under relief and trees: one quiet warm-grey ink, wider = higher rank, **darker = more remote** (ink weight from the 250 m remoteness grid, `osm_remote_grid.go`: ×.5 in a city centre … ×1.5 in the back country, + ≤ .5 px; rail and casing untouched; no hue → colour-blind and greyscale safe), dashed = service ways, white ties = rail, a pale casing under every line so it reads on a dark fill or the photo; whole-layer weight = the reader's (default .7, same popover as the photo: click toggles + shows, hold / right-click / wheel adjust) | ✓ same |

Reserved tokens parse and are ignored today (`dbg: reserved token`), so a link minted later still opens.
Overlays combine freely; **z-order** bottom → top: profile fill → change → diff → subsidy → roads → relief → trees.
**Legend merge** (`hiLegendHTML0`): one `.lg12` = head `in view · <profile>` + profile bar, then one
block per overlay in z-order: `change` (Δ bar, confirmed/candidate, sighting period) · `diff` (`difBlock`: kinds in view, `n of m cells compared`, the *corroborated only / all cells* toggle, mixed-epoch KGs) · `subsidy`
(claims; "vs the year before" when change is on) · `roads` line (`rdLine`: km in view by rank, source, weight) · `relief` note · `trees` (count, tallest, band bar).
**Card source order** (`srcs`): change first when on, then the profile's source, then the rest.
**Hash grammar**: `;v=obs` profile · `;change ;diff[=raw] ;subsidy ;trees ;relief ;ortho[=op] ;roads[=op]` overlays · `;t=2|2..3|2@d1..d2|@d1..d2` time window (implies change; `at=x,y,z` stays the viewport); legacy `;v=chg` reads
as declared + change, and `;trees` no longer implies `;v=obs`. **Keys**: `v` flips the profile, `c d s t r o w`
flip the overlays. **Busy**: the profile word trails dots while its tiles load; an overlay toggle pulses.
**Soak**: `m.matrix(kg)` walks all 2 × 2⁴ combinations (+ diff and photo each alone / with change / trees / relief) by share link at LOD 1 (and the change/subsidy
combos at LOD 2 and 0) through `layers()`, whose expectations are read off the hash (`expectOf`).

## JS map (`neGlyphScript`, search the function name)

- **State**: `vb` viewBox, `W H`, `cells[]`, `kgs`, `view` (`'' | obs | chg`, `VIEWS`), `hi`
  (full res: `on tiles order l[] g pin hov apx tree left`), `rl` (relief), `sb` (subsidy),
  `sel` (KG), `pinned` / `shown` (res-7 index), `picked` + `pk {t: cell|kg|h3|tree|pick}` (what
  the card shows), `auto` (auto full-res), `tour`.
- **Filter** (`ne_glyph_filt.go`, state `flt {f open w cat bins tiles groups words ext ready px ed}`):
  `fltWorker()` starts `/api/v1/ne/glyph/filter.js` (theme from `--bg`/`--acc`) · `fltMsg` (`cat` →
  suggestions ready, `expr` → chips/words/ignored (`.bad` + hint, input keeps the ignored text), `bin`
  → counts, `tile` → `<image href=blob>` + class `in`, `at` → card row) · `fltView(now)` the ONE
  placer: `hiWant(hiLOD())` tiles → `{k, bins[] (0.025° bins a LOD-1 tile straddles), x,y,w,h}` +
  `px` (screen px per frame unit, 6–64) → Worker `want`; drops tiles/bins out of view, revokes blobs ·
  `fltSet/fltAdd/fltRemove/fltEdit` (chips are groups; click = edit in the input) · `fltSugg`
  (presets → words → fields → members) · `fltReadout` (`.gf-n`: `m of n cells · ha`, `zoom in to
  filter cells` at LOD 2) · `fltLegend` (`.nt.flt` block in `.lg12`, `· all passes` when a time window
  is set and `chg` is used) · `fltCardRow` (async, per pinned cell) · hash `;f=<canonical>` ·
  `svg.v-flt` toggles `.ov-flt` (opacity .55, .7 under ortho) · keys `f` row / `F` full screen · the
  find box offers an expression row for typed filters · hook `neGlyph.filter(f?)` → `{f, words,
  open, bins, pending, cells, match, ext, ignored, readout}`.
- **Overview**: `faceD(i)` hex path · `buildHex` · `cull` · `blend` (dots ↔ faces by zoom) ·
  `card(c)` res-7 card · `kgCard()` · `kgAgg()` · `cellsInBox` · `near` · `hexAt`.
- **Full res**: `hiLOD()` (z ≥ 300 → 1, ≥ 110 → 0, else 2) · `hiWant(lod)` tiles for the view
  · `hiTile(t,v)` fetch + `<image>` (meta from `X-NE-Tile` into `e.m`; 204 → `e.ok` with no
  image, except `down=1` (subsidy peer) = a failed tile; `HI_KEEP` eviction) · `hiCull` (also hides a sub tile of another LOD once covered) · `hiView(now)` (debounced: base + view + overlays) ·
  `hiSum(v)` legend sums · `hiLegendHTML0/hiLegendHTML/hiLegend` (`.lg12`; relief and subsidy
  blocks appended by slicing the closing `</div>`) · `hiOn/hiOff/hiLeave` · `hiAuto` ·
  `avail/hiBtn/availLazy/availHint` · `hiAt/hiFind/hiFetch` (cell doc cache `hi.docs`) ·
  `hiRow` (cell doc → row shape) · `hiCard(d)` res-12 card · `hiChgHTML` (cluster-first change
  row; helpers `perOf` month/quarter period, `dayOf`, `areaU`, `chgDir` from → to, `chgMoved`,
  `addV`; `docs/ne-change.md` §6a) · `hiPeekHTML` ·
  `hiShow/hiPick/hiUnpin/hiGoto(cell)`.
- **Diff**: `df`, `setDif(on,quiet,raw)`, `difBlock` (legend), `difCellLine` (card row + open block), `difSw` (swatches: solid · `ng-h4` dots · hollow hexagon), `difAge` (`flown 2022 · 4 y ago`), `DIF_L/DIF_T/DIF_K`; `hiSum('dif')` reads `dif cmp als mix`; `hiCull` hides a dif tile whose `raw` does not match `df.raw`; the legend's `a.raw` link toggles raw (delegated click on `inner`).
- **Trees**: `hiApex/apRender/apCard/apUnpin/setTree`. **Relief**: `relView/relStatus/setRelief` (`mtWant(z,n)` = the Web-Mercator squares touching the view, shared).
  **Photo** (`ot`, `/* ── ortho`): `otZ` (one step finer than `relZ`) · `otView/otTile/otPump/otFetch/otSettle/otClear` (relief's shape; URL `ORTHO` = mapsneu.wien.gv.at WMTS `{z}/{y}/{x}.jpeg`, 404 = outside coverage, ≤ 6 in flight, `OT_KEEP` 240) · `setOrtho(on,quiet)` · `setOrthoOp(v,quiet)` + `otApply` (group opacity + `--otf`) · popover `otPop(on,ms)/otPopSync` (`otHeld` swallows the click after a hold) · fetch class `ortho`, mark `the photo`, group `basemap.at`.
  **Roads** (`rd`, `/* ── roads`): `RD_OP` .7 · `setRoads(on,quiet)` · `setRoadsOp(v,quiet)` + `rdApply` (group opacity on `.ov-rd`) · `rdPop = mkPop({…})` · `rdLine()` legend line from `hiSum('rd')` (`km rd rdc`; `rm` = Σ rm·Σrmb / Σ Σrmb is aggregated for scripts, not shown — the remoteness reading is the one-time hint on enable, never legend clutter); tiles ride the ordinary `hiTile(t,'rd')` path (key `rd/<lod>/<i>/<j>`, no `~` suffix — the weight is CSS, never a refetch).
  **Popover factory** `mkPop({btn, panel, on, turnOn, get, set, commit})` → callable `pop(on, ms)` with `.sync()` and `.held()` (swallows the click after a hold); `otPop` and `rdPop` are its two instances.
  **Subsidy**: `setSub/subSync/subVis/subLoad/subBuild/subHex/subSum/subSumOv/subLegendHTML/
  subCellLine/subDotLine/subKGLine/subSw`.
- **Time** (`tl`: `win lvl k bins d ld map pt pg play`): `tlLoad/tlData` (passes JSON, scope = selection) · `tlBins` (level 0/1/2) · `tlRender` (track, dots, head marker + label, readout) · `setTime(w,quiet)` · `tlParse/tlStr/tlLabel` · `tlMap` (cell → [conf, cand, day]) · `tlPaint(force)` (additive overview paths `.tl`, per tone × day in `tl.pg`) · `tlAge/tlScale/tlAgeSet/tlAgeSync` (ash) · `tlHead` · `tlStep/tlPlay/tlPause/tlResume/tlStop/tlTick` · `tlCatchUp` (tiles/card/legend after a play) · `tlQ()` (`&t=` for cell docs) · `tlRepin` · `tlBusy`; head date → `.gt-dt` (left-bound on the tools row, `tlDateSet`); drag on `.tl-s` (`tlGrip` picks the mode): finger on the head hairline (within ~half a bin) = drag the window's END, head follows the finger continuously and settles on the bin edge on release; start inside the window = slide it (width kept, clamped to the strip); start outside or shift-drag = range from the anchor bin. While the finger is down (`.glyph-time.dg`) there are no transitions and `setTime` skips `tlCatchUp` (tiles/card/legend catch up on release); `hiEvict` does not protect change tiles of another window, so a scrub cannot grow the cache past `HI_KEEP`. Below 640 px / coarse pointer the card's inner div is `width:auto` (fills the figure).
- **Card plumbing**: `setHTML` (height-animated swap) · `reveal(c,html)` · `recard()` (re-render
  whatever is picked — use it, do not re-derive) · `place` · `hide/dismiss` · `srcs(dec,ob,ch)`
  (source order follows the view) · `obsHTML/obsFetch` (observed-peer cells) · `chgHTML/chgFetch` ·
  helpers `sw seg item legend(items,max,lead) obsBar dluBar pct fmt kfmt km2 ldots esc`.
- **Navigation**: `setVB` (every view change: `blend hiView availLazy hiAuto setHash`) · `zoomTo`
  · `zoomCenter` · `reset` · `fsToggle/fsFit/fsVV` (full screen follows `visualViewport` while a soft keyboard shortens it — iOS; the landing viewport meta carries `interactive-widget=resizes-content` so Chrome resizes the layout viewport itself) · find (`tIn/tList`; `search` KGs from `/ne/glyph/kgs`,
    `admSearch` Gemeinde/Bezirk/Land/Natura 2000 from `/ne/glyph/admin`, `remote` addresses + names,
    `assemble` merges, `pick` → `selectKG` / `selectAdm` / `goPoint`; `landFull` ends every landing in
    full res when deep enough — `docs/glyph-find-contract.md`) · `tour`.
- **Area selection** (one model for every outline, built to take a cell/area filter next): `sel` (KG) and
  `asel` (admin unit / Natura 2000 site) both carry `idx` (res-7 cells inside), `a` (`aggOf(idx)` —
  `aggNew/aggAdd` is the one accumulator, `kgAgg` uses it too), `row` (`areaRow(s)`: the glyph row shape
  over the cells, `.k` cache key, `.kg` for one KG or `.filt`/`.kgs`/`.kgn` for a KG set), `b`/`c2`/`r`
  (bbox, centre, radius). `areaCard()` → `kgCard()` | `admCard()`; `showSel` / `hide` / `recard` serve
  both. The cell set of an admin unit comes from the KG index's GKZ (`admKGs`: Gemeinde 5 · Bezirk 3 ·
  Land 1 digits, union of `kgAgg().idx`); Wien Gemeindebezirke and Natura 2000 sites fall back to
  `cellsInPath` (centre inside the outline, evenodd on `ringsOf(d)`) and `kgsOf(idx)` for the KG list.
  Observed for a set = `obsSet` → the observed-layer peer's `/api/v1/kgs/summary?<filt>` (400 → `codes=`, ≤ 500); change for
  a set = `/ne/stats?<filt>` or `kg=` list, cached under `row.k` (`chgKey`). Subsidy: `subSumOv(idx)`.
  The veil is on whenever an area is selected, in the overview too (`svg.glyph.veiled`: fill and stroke
  of the selection go, the un-veiled land is the selection); the pinned res-7 cell is veiled in full res only.
  A future filter component = build `{idx, a, row, d, b}` the same way and call `drawSelD` + `showSel`.
  Wheel zoom is coalesced per frame (`whl`/`wheelFlush`): deltas of one frame are summed, one
  `zoomAt` per rAF, events older than 250 ms dropped — slow office PCs otherwise replay a stale
  backlog after the hand stopped or reversed, which looked like inverted zoom.
- **Share links**: `curHash()` writes `kg= cell= h3= q= v= change diff[=raw] full nofull trees relief ortho[=op] roads[=op] subsidy screen
  at= open below card= export`; `applyHash()` parses the same (regex per token, unknown token → ignore
  all) and applies in order `autoSet → setView → setTree → setRelief → setOrthoOp/setOrtho → setRoadsOp/setRoads → setSub → fs → open → dock`.
- **Keys** (`main` keydown): arrows pan/step, `v` cycles views, `c` change, `d` diff, `s` subsidy, `t` trees, `r` relief, `o` photo, `w` roads, `+ - 0`, `Esc`
  unwinds (pin → full → selection → zoom → full screen).
- **Hook** `window.neGlyph`: `state() card() legend() peek() hover(lon,lat) pin(lon,lat) view(v)
  trees(b) relief(b) ortho(b[,op]) roads(b[,op]) subsidy(b) go(hash) settled(ms) lonlat xy client log() logReset()` — scripted
  browser checks (`log()` = JS errors + fetch counts + dbg trace, §soak).

## Conventions

- Every figure in a legend or card is named (`item()` with label + value); long bars collapse
  behind `…` (`legend(items,max)`, `lgMax()` = 9 open / 5 collapsed).
- Overlays never move the bar; a toggle that cannot show now fades (`off`) and stays in the hash.
- One hint per toggle, once (`setX.h`), phrased as what the colour means.
- **Diff ≠ change.** Change is the register over time (our sighting dates); diff is register vs LiDAR of one cell today (the LiDAR epoch is its date, always shown). A diff mark is not a finding against the register — kind 3 is worded *currently no trees*, never "cut": a cut stand is forest by design.
- New overlay = Go painting in its own file + a JS block in the same shape as `/* ── relief` and
  `/* ── subsidy` (state object, `setX`, status, sums, legend block, card lines, hash token,
  key, hook) + CSS appended to the overlay selector lists + llm.txt §2a/§7/§7a + this file.

- **Declared row = `bev` first.** Tiles (`v=`, `v=obs`, `v=tree`) and the cell card
  read a cell's declared row from the KG's `bev` section (the peer's bevdirect build, the cell set
  its obs rows were validated against) and from the index-epoch `lu` only where bev lacks the
  cell (`neReq.bevFirst`, `neCollect`, `neStore.declaredRow`). Before, both were `lu`-keyed and
  obs was joined onto lu ids, so a cell the March index never held (63235 parcel 27, 24 ha forest,
  `8c1e10ba01361ff`) was a hole in the declared AND the observed picture although the peer's NEC1+NEO1
  carried it — "undeclared gap replicating in observed" = our join, not the peer. The layered API
  (`/ne/cells`, `/ne/chunk`, `/ne/cell`) keeps `lu` pure; ask `layers=bev` there.
- **… and the chunk log before that (`ne_change_declared.go`).** Per KG the declared
  row is log view → `bev` → `lu`: the latest confirmed (else baseline) chunk version any peer
  reported — so KGs the observed-layer peer never posted NEC1 for (63225 Gschnaidt, the grey 31 % + white hole) are
  right as soon as a report with rows is in, and refresh with the map peer's monthly passes. Tile cache
  keys carry the state of every KG the tile reads (`neGlyphTileState`). `docs/ne-change.md` §4b.
  **White hexagons inside a KG** (04015 Klausenleopoldsdorf) = res-9 chunks the March
  index never held for the KG, which `chunkHits` (built from `.nex` sections) never asked the log
  for although the peer's build had them — fixed by `neStore.setLogChunks` (`Dir = -1` hits),
  ne-change.md §4b last paragraph.

## Offline / fail-soft

Every fetch of the page goes through one wrapper (`fetch` in `neGlyphScript`, top). A failure — rejected
promise or a ≥ 500 answer — is a **mark** per source (`net.what[w]`: `the picture` tiles · `this cell` cell
doc · `change figures` · `KG search` · `{OBS_NAME}` (the observed-layer peer) obs/apices · `relief` · `subsidies` · `a peer` ·
`umfeld-at`), the hint (`showHint`, the zoom-hint layout) says once per 20 s *offline · X did not answer —
retries on your next move*, `figure.glyph.offline` is set, and every card gets one muted row
`.rw.off` (`offRow()`, appended by `srcs()`) naming the sources; a source row itself just says `offline`
(`offTxt`). A pick whose cell doc never came shows `offCard()` (`pk.t='off'`, coordinates + the row).
Nothing failed is cached for good: `hiTile`/`hiApex` drop a failed tile (`hiDrop`, `hi.bad[key]` backoff
`NET_TTL` 2.5 s, ≤ 400 keys), `obsC`/`chgC` null entries are re-asked after `NET_TTL`, `kgOut` forgets
a 5xx, relief forgets a failed tile. **The interaction is the retry**: `netTouch()` (`hiView`, `pinAt`)
drops marks older than `NET_TTL` — whatever the move needs is asked again and re-marks itself if it
fails; a success clears its origin group (`netGroup`: own origin / observed peer / subsidies / other peer); a mark
nobody re-asked fades after 12 s (`netPrune`). `jsonOf(r)` turns a non-OK answer into a rejection so a
`text/plain` 503 never reaches `r.json()` unhandled. Hooks: `neGlyph.fault(mode)` injects faults
(`'offline'` every fetch rejects like a lost network, `'5xx'` every fetch answers 503, `'peer'` only
cross-origin peers 503, `null` off; the fake answer honours `AbortController`), `neGlyph.net()` returns
the marks. Soak step: `m.offline(hash?)` below; `run()` includes it (`{offline:false}` skips).

Side findings fixed with it: a share link without `;relief` left the relief queue counting
(`setRelief(false, quiet)` skipped `relClear`); relief responses landing after a clear re-queued dead
entries and drove `inflight` negative; sub tiles did not return after the subsidy overview recovered
(`subLoad` success now calls `hiView(true)`).

## Paint budget — phones

Symptom: a phone browser (DuckDuckGo = Android WebView, which rasters on the app's UI thread) froze
with the ANR dialog after the page had been open a while and the user pinched into the full level,
status still "loading…". Three things made the paint heavy and unbounded; each has a knob and a line in
`neGlyph.state().mem`:

- **Under-LOD cull** (`hiUnder`, `hiCull`): the three LOD groups are stacked, and before, every tile of
  LOD 2 and LOD 0 under a LOD-1 view stayed painted — three 872×1292 bitmaps per screen area, times
  the overlay. Now, once every wanted tile of the LOD in use is `ok` (base **and** the view's overlay),
  tiles of the other LODs that touch the view get `display:none` (`e.hid`, `mem.under`). Subsidy tiles count like the profile's overlay here (`hiUnderMiss`). While the
  new LOD is still loading they stay as the backdrop. `dbg` logs `under-lod hidden|shown lod=`.
- **Device-sized cache** (`HI_KEEP` from `navigator.deviceMemory`: ≤ 2 GB → 32, ≤ 4 GB or touch → 56,
  else 160 tiles ≈ 1.1 MP each; `mem.keep`): `hiEvict` evicts **off-screen tiles only**, oldest first,
  so a tile on screen is never dropped and re-fetched; `mem.evicted` counts, `dbg` logs `evict n`.
  A tile evicted while its fetch is in flight still releases `hi.left` (was a leak → busy forever).
- **Off-thread decode queue** (`hiDecode`, `dec`): a tile's PNG is decoded via `HTMLImageElement.decode()`
  on the same blob URL (Blink shares the decoded frame by URL) before its `<image>` joins the SVG, at
  most 2 (touch) / 4 at a time — a burst of 30 tiles lands over frames, not in one paint. `mem.decQ/decN`.
- **No per-frame SVG animation on touch** (`@media(hover:none)`): the shimmer `.ph` keyframes and the
  image fade-in repaint the whole SVG every frame while anything loads; phones get a static placeholder.

Verify: `m.check(hash)` now asserts `under-lod hidden`, `decode drained`, `no leaked <image>`, `cache ≤ budget`;
`m.stress(kg)` walks a LOD-1 window past the budget (`run()` includes it; `{stress:false}` skips) and
returns `keep/peak/evicted/shownMax/maxLeft`. Emulate a phone first (`coarse` → keep 56) to test the
mobile path. Found with it the same day: a server data race — `neChangeKG.rows/deltasOf` filled their
lazy caches unlocked while a tile burst of one KG read them (`fatal error: concurrent map read and map
write`, process restart mid-session). Now `rowsMu`/`deltasMu`, and `neChangeAppend` is copy-on-write
(drops the loaded KG, next reader reloads) — `TestNEChangeProtocol` runs 8 readers under `-race`.

## Soak test (`ne_glyph_soak.js` → `/api/v1/ne/glyph/soak.js`)

Roads step (`run`, `{roads:false}` skips): `;roads` at LOD 1 (default weight written bare, `.ov-rd` opacity .7), `;roads=0.4` with observed + change at LOD 0 (weight kept in hash and group, tile meta carries `rd km rdc`), `;roads` at LOD 2 (token kept, group hidden, icon `off`); `layers()` asserts `rd` tiles = base at LOD 0/1 and none at LOD 2, `ov-rd` shown ⇔ on & near, z-order over `ov-subc` and under `ov-rel`/`ov-tree`, `v-rd`, bar, legend *roads* line, hash token; matrix adds roads alone / with change / subsidy / trees / relief / photo; the share-link round trip compares `roads`/`roadsOp`; `offline()` switches the roads on under each fault.

Photo step (`run`, `{ortho:false}` skips): `;ortho` at LOD 1 (default weight written as the bare token), `;ortho=0.8` with observed + change at LOD 0 (weight kept in the hash, `.b` carries `filter:opacity(0.61)`), `;ortho` at LOD 2 (token kept, group hidden, icon `off`); `layers()` asserts `ov-ortho` shown ⇔ on & near, first child of `.hi`, `v-ortho`, bar, legend *photo* line, hash token; the share-link round trip compares `ortho`/`orthoOp`; `offline()` switches the photo on under each fault and counts the `ortho` class; a 404 from basemap.at (outside coverage) is not a bad fetch.

One in-page module that drives the glyph through its states by **share link** and checks, after
every step, that exactly the right layers are loaded and complete — no tile left `ok=false` on
screen, overlay tiles = base tiles, apex tiles under `;trees`, sub tiles = base under `;subsidy` at every LOD (no `~f`, none of another LOD painted, button not `off`/`busy`; `run` walks z 400/200/20 and a LOD walk with the layer on), dif tiles = base under `;diff` at LOD 0/1 and none at LOD 2, the `.ov-*` groups displayed iff the state says so, `svg.glyph` classes, legend
present, hash tokens — plus server-side consistency: `/ne/glyph` JSON ↔ inlined dots, KG index,
KG outline ↔ index bbox, `/ne/glyph/cell` ↔ the pinned cell, res-7 change/observed ↔ lod-2 tile
meta, tile views agree, bad input → 400/404, and a share-link round trip (mint → leave → reopen →
same view/full/trees/relief/subsidy/zoom/pin). Rapid toggles must converge. Output is **one string**:
a summary line (`PASS|FAIL n checks, k failed · seconds · seed · latencies p50/max`) + one line per
failure (+ `warn:`), so a driver can read it without the page.

Run it from a browser driver on `http://localhost:8000/` (the page must be loaded — it uses
`window.neGlyph`); the module is `no-store`, add a cache buster anyway:

```
import('/api/v1/ne/glyph/soak.js?'+Date.now()).then(m=>m.run({spots:2, seed:7}))
```

Options: `rounds` (1), `spots` KGs per round (2), `kgs:[…]` explicit codes, `seed`, `verbose`
(list every ok), `settle` ms (20000), `trees|relief|subsidy|ortho:false` to skip an overlay, `offline:false`
to skip the fail-soft step (~30 s per round, three fault modes). Full result
object in `window.neGlyphSoak.last`. ~30 s per KG warm, ~60 s cold (lod-2 tiles render in 3–12 s).

### Agent toolkit — verify a glyph change without a screenshot

All from one `const m = await import('/api/v1/ne/glyph/soak.js?'+Date.now())` on `http://localhost:8000/`
(headless browser eval). Each returns **one string**; nothing needs the page to be looked at.

| Tool | One call | Gives |
|---|---|---|
| `m.check(hash)` | `await m.check('#kg=63330;v=chg;full;relief;z=200')` | goes there, settles, runs every `layers` check (view/full/lod/tiles complete/overlay=base/apex/sub/`.ov-*` display/svg classes/legend/hash tokens) with expectations read off the hash itself, plus **no JS errors** and **no non-200 fetch** (204 relief / 204 sub-when-peer-down tolerated) → `check … PASS 20 checks` or one `FAIL` line per miss. ~1–3 s warm. `{expect:{…}, verbose, settle}` override. |
| `m.tile(spec)` | `await m.tile('1/1511/4712/chg')` · `m.tile({lon,lat,lod,v})` | `{s, m (X-NE-Tile minus ms), bytes, png, w, h, sha}` of one tile — JSON, no bytes. |
| `m.snap(name[, specs])` | `await m.snap('before')` | stores the tiles on screen (cached, in view, of the current LOD, every view) — or the given specs — in the Cache API under `name`; survives reload **and** server restart; one line per tile `key status WxH sha n=`. |
| `m.diff(name[, specs])` | `await m.diff('before')` | re-fetches and compares each with the stored one: status, every meta key (`n 3309→3308 · lu …`), bytes, and **changed pixels** (`px 493992/1126624 (43.85%) bbox x0,y0,x1,y1`) → `same` or `CHANGED …` per tile. |
| `m.stress(kg?)` | `await m.stress('63330')` | long-session path: lod-1 spiral walk (views alternate) past the tile budget → budget held, `left=0`, under-LOD hidden, decode drained, eviction ran, no JS error; one line `keep/peak/evicted/shownMax/maxLeft`. ~10 s. |
| `m.offline(hash?)` | `await m.offline('#kg=63330;full;v=obs;trees;z=300')` | fail-soft under faults (§offline): for `offline`, `5xx`, `peer` — inject, then pin / view / trees / subsidy / relief / pan, settle → **no uncaught error, no `.busy` button, `left=0`, no tile `ok=false`, offline noted, card `.rw.off`, faults reached tiles/peer**; lift → pan + re-ask subsidies + pin → tiles complete, apex back, no offline row, a real card, no busy, no errors. ~30 s. Checks that no fetch happened under a mode (all cached) become `warn:`. |
| `neGlyph.log()` | `JSON.stringify(window.neGlyph.log())` | `{errors:[{t,m,at}], fetch:{tile:{200:n,204:n,404:n,abort:n,err:n}, relief:{…}, peer:{…}, api:{…}}, dbg:[…]}` — the page's own counters since load / `logReset()`; `dbg` is the 40-line gesture/nav trace (`?dbg` only puts it on screen). |
| server side | `journalctl -u umfeld-at --since -10m \| grep glyph/tile \| grep -oE '→ [0-9]{3}' \| sort \| uniq -c` | tile status counts without a browser. |

Recipe for a tile/paint change: `m.check(hash)` → `m.snap('a')` → edit, build, restart → reload page →
`m.check(hash)` → `m.diff('a')`. Only `CHANGED` lines land in context; `bbox` tells where to look if a
screenshot is wanted after all. Spec grammar: `lod/i/j[/v]`, `v/lod/i/j` (= `state().tiles[].k`),
`{lod,i,j,v}`, `{lon,lat,lod,v}`; `v` ∈ `'' obs tree chg dif rd sub subc`.

Hook additions for drivers: `state()` carries `animating`, `refits`, `applying`, `fly`, `tour`,
`animMs`; `settled()` waits for `applying`, tiles, a running fly (≤ 2.5 s), pending KG-outline
refits and relief tiles.

Bugs it found on day one (all fixed): a pending KG-outline refit zoomed back after a same-KG
`;full;z=` link; `#kg=` links kept a stale pinned res-12 cell; `hi.left` / `rl.inflight` went
negative after an abort; `zoomTo` ran its first frame inline and re-entered via `place()`; the
end of a fly let `place()` start a perpetual 320 ms pan loop; the res-7 change count ignored the
`kg_share ≥ 128` / `absent` dedupe rule the tiles and the summary use.
