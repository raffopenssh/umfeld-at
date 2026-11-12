# The landing glyph — code map

Read this before touching `ne_glyph*.go`; it replaces reading the 200 KB file. Narrative and
decisions: `docs/ne-change.md` §6a–6c, time strip + ageing §6e; subsidy layer: `docs/invekos-subsidy-layer.md`.

## Files

| File | Holds |
|---|---|
| `ne_glyph.go` | Go: res-7 frame (`neGlyphFrame`, `neGlyphLoadFrame` from `data/ne_glyph_austria_r7.json`, hex fit `neGlyphHexFit`), per-cell aggregate from the reports scan (`neGlyphAgg.add/fold`, `neGlyphBuild`), the SVG (`neGlyphSVG`: `g.dt` dot paths `.e .h .n .o .ch .ca .c` as `M x y h0` strokes, bucketed opacity), the JSON (`neGlyphJSON`: `cells[] = [x,y,chunks,passes,reports,age,obs,chg,kg,bands,cand]`, `kg_codes[]`, `proj{}`), KG search index (`neGlyphKGIndex`). Then the page: `neGlyphHTML()` (figure, tool bar, view bar), `neGlyphCSS` (one string, ~150 lines of rules), `neGlyphScript` (one IIFE, ~800 one-line functions; see map below). |
| `ne_glyph_tile.go` | Full resolution: `neGlyphLODs` `{0.025°,40px}` `{0.01°,100px}` `{0.1°,10px}` (indices 0/1/2; LOD 1 = nearest), `neGlyphTileGeomOf/poly` (hex → tile pixels via `neGlyphProj`), `neFillPoly` scanline, `neGlyphTileRender` (switch on view: base / `obs` / `tree` / `chg` / `sub` / `subc`), `neGlyphTileMeta.String` → `X-NE-Tile`, `handleNEGlyphTile` (LRU `ne_glyph_tiles` 64 MB, key `view/lod/i/j[/day]`, meta trailer joined to the PNG), `handleNEGlyphCell` (lon/lat or `cell=`, public cell doc + `pic_obs` [+ `sub` with `sub=1`]), `handleNEGlyphKG` (KG outline at res 12). |
| `ne_change_passes.go` | Passes per KG / national (`passes`, `table`, `neChangeWin`/`neParseWin`, `cumWin`), `/ne/glyph/passes`; `ne_glyph_tile.go` ages chg tiles (`neGlyphAgeScale`, `neGlyphChgIdx`). |
| `ne_glyph_sub.go` | Subsidy layer: peer fetch + LRUs (H3 bins, overview, IVP1 outline bins), `v=sub|subc` painting (bins + outlines, one cut at every LOD, `docs/invekos-subsidy-layer.md` §LOD 2), `/ne/glyph/sub` overview, `neSubCellAt`. |
| `ne_glyph_dif.go` | DIFF overlay (`v=dif`, LOD 0/1): declared row vs observed row of the same cell, today — `neDifStatus` (ok/edge/mixed…), `neDifKind` (the three corroborated kinds of ne-change.md §6d, `raw` lifts the corroboration), `neGlyphDifRender` (solid / chequer / ring, one hue `neGlyphDifRGB`), `neGlyphDifMeta` → `;obs=;dif=k1,k2,k3;cmp=;[raw=1;]als=y0,y1;mix=`, the lidar epoch table `neDifAlsOf` (the observed-layer peer's `kgs?fields=codes,als`, one conditional GET per hour — `If-None-Match` with the peer's ETag, a 304 keeps table and stamp, `neDifEpochStamp` in the tile key, `no-store` until loaded), `neGlyphPicDif` (cell doc `pic_dif`), `NEGlyphDifStats`. |
| `ne_glyph_roads.go` | ROADS overlay (`v=rd`, LOD 0/1): OSM roads + rail from `data/osm_proximity.db` (`osm_lines` cat road/rail, the same rows `/api/v1/osm/geometry` serves) drawn server-side — `neRoadClassOf` (major · road · minor=service/pedestrian/living_street/busway dashed · rail), `neRoadInk(cls, attr, lod)` the ONE place class + per-line attributes become ink (`neRoadAttr.Remote` = mean remoteness 0–100 of the chunk's vertices from `remoteGridGet()` → `neRoadRemoteGain` .5 + r/100 on alpha (clamp .92) and + ≤ .5 px width, roads only; −1 = grid not ready → class base; each line's weight rides in the coverage via the stroke's `gain`, one composite per class), `neRoadStroke` signed-distance AA stroke with max-blend coverage (joins never darken, dashes cut along the arc with soft ends), `neRoadComposite` src-over, passes casing → minor → rail (+ white ties at LOD 1) → road → major, `neRoadQuantize` → 256-colour palette (32 alpha × 8 ink/casing mixes; ~75 KB a dense LOD-1 tile vs 360 KB NRGBA), meta `;rd=;rdc=major,road,minor,rail;km=` (`clipLenM`: length inside the box so tiles add up) `;rm=;rmb=<25,25–50,50–75,≥75 km` (length-weighted mean / bands of the roads' remoteness, absent until the grid is ready), 204 `down=1` without the db. Achromatic by design (colour-blind safe); `ne_glyph_roads_test.go`. |
| `ne_glyph_subpoly.go` | Subsidy outlines at LOD 0/1: IVP1 decoder (`neSubPolyParse`), bin units → tile px (`neSubPolyMapOf`), `neFillRings` (even-odd, holes, active edges) / `neStrokeRings`, `neGlyphSubPaintPolys` (declared fills / change fills by kind + Schlag counts), `neSubSchlagAt` (cell card). |
| `ne_glyph.go` `linkSafe` · `copyLink` | The COPIED link (and every `?f=` the export menu mints, and the envelope's `link`, `neStatsLink`) percent-encodes `< > \| " '` and spaces — mail clients and messengers stop linkifying a URL at `>` (Gmail cut `…f=as=s,sl>=11.3…` after `sl`). The hash in the address bar stays readable (`fltHashEnc`), both forms open: `applyHash` decodes `f=`, the server unescapes `share=`/`f=`. |
| `ne_glyph.go` `hashTokens` · `applyHash` | SHARE LINK PARSING is lenient by design: tokens are read one by one, every token the page understands applies, the rest is dropped and named once (`showHint` *link: ignored …*, `dbg: hash dropped`) — never the whole link. `hashTokens` normalises `#`, `#!`, `#/` prefixes, a fully percent-encoded hash (`%3B`, `%3D` — messengers do that), `&` as a fallback separator, blank and duplicate tokens (the last wins). Per token: keys case-insensitive, `kg=` pads to 5 digits, `pt=` takes integers and `lat,lon` the wrong way round (swapped when that lands in Austria; outside Austria dropped), `pn=`/`q=` decode `+` and survive bad `%`, `h3=` any case, `adm=` any case, `z=`/`at=` zoom clamped to `HI_ZMAX`, `ortho=`/`roads=` clamped 0.1–1, `t=` malformed drops only `t=`, `v=`/`v=d`/`v=lu` = declared, `f=` incomplete terms are completed, `f=off:<expr>` lands as a held (switched-off) filter (ne-filter.md §1d). `pn` without `pt` is dropped. Only a link with nothing usable returns false. |
| `ne_glyph_export.go` · `api_ne_stats_unit.go` · `ne_mvt_jobs.go` | EXPORT: the link button is a split control `.gt-kk` = `.gt-k` (copy the share link, unchanged) + `.gt-kx` ▾ (menu `.gt-xm`, a small popover — left-anchored on desktop, right-anchored ≤ 640 px — with two rows and a URL line: EXPORT JSON · CSV (download) · flat · Sheets formula `=IMPORTDATA(…csv)`; QGIS `tiles` (copies the MVT URL) · `PMTiles` (archive link, z 9–14, of the selected KG/area via `share=` or else the current view via `bbox=`; `xArchMB` = km² × 0.17 MB (measured 0.15–0.17) is the size shown in the title); optional key field, localStorage `ne.key`, appended as `&key=`). Every unit entry is a LINK to `/api/v1/ne/stats/unit?share=<unitHash()>&format=…&src=glyph` — `unitHash()` keeps only `kg= h3= adm= pt= f=` of `curHash()`, so the export is the share link rendered by another endpoint (`docs/stats-envelope.md`). **Without a unit the table covers the current view** (`xUnit()` → `bbox=<xViewBox()>[&f=]`, label *the view*; beyond `XBINS`=48 bins of 0.025° the server answers from the national sample and the label reads *≈ the view*). The PMTiles link offers every cell (z 9–14) up to `XKM2`=1 500 km² (the server's `nePMTMaxKm2`), farther out the z ≤ 11 *overview* archive of the view (`maxz=11`, any area); the QGIS row works anywhere, and with a filter on, `xF()` puts `?f=` on the tile URL and `&f=` on a `bbox=` archive (a `share=` archive carries `;f=` already) — QGIS and Excel see the veil (`docs/qgis-layer.md` §Filtered layers, `docs/stats-envelope.md` §6). Open menu = `;export` in the share hash (restored by `applyHash`). `xOpen` measures: when the bar sits in the lower half (full screen, phone keyboard) the menu opens upward (`.gt-kk.up`) and is nudged inside the viewport. **File names** (`ne_export_name.go`, `neExportStem`): every file is named like the recent list labels the view — `umfeld-at-<cells|stats>-<view-KG-Gemeinde | KG-code-name | Gemeinde-… >[-observed-change-diff-trees-relief-photo-roads-subsidy][-f-<filter>][-z9-14]-<day>.<ext>` (ASCII-folded; JSON/flat carry it as `inline` so *save as* gets it too; the zip's inner files share the stem). **Every export click also lands in the recent list** (`vpKeep(ex)`: a `view` entry for the current hash without `;export`, whatever the zoom or selection; `it.ex` = the formats, shown as *exported .qgs CSV* and kept when a later dwell refreshes the entry). Nothing fetches on open; the endpoint counts (`export_gate.go`, line `export_gate` on /debug/inflight). |
| `ne_glyph_treerep.go` · `ne_glyph_treematch.go` · `api_tree_report.go` | TREE REPORTS (observed-layer peer contract 2026-11-11): the tree card is one flow — `species` (one-tap chips: recents, one species of the model class, common Austrian trees, `other…` = dict search — **species search** `trSearch` (`ne_glyph_treematch.go`, pure, `TestGlyphTreeMatch` over `testdata/tree_taxa_rev2.json`): the peer's dict rev 2 rows `[taxon,de,en,class,rank,codes,alt]` in rank order; German / English / scientific names, other names, forestry + EPPO codes, Latin abbreviations `P. abies` · `PiAb` · `PicAbi`, the peer's normalisation (ß, umlauts also as ae/oe/ue, × , hyphens/dots/spaces), one or two typos as a last resort; order = match step, then `trPri`: named on trees in view → the reader's recents → named ≥ 3× nationally (`/trees/taxa/top`) → rank; the box opens with six suggestions before a key is pressed, the marked chip is what Enter takes (↑/↓ move it), a chip names the matched code/other name in grey when the shown name did not match), `height` / `dbh` (one row each in the card's label column, `.rw.mr`, equal steppers under each other at every width; typing one hides the other, `set` beside the input, methods on the line below; steppers − / + in 1 m / 1 cm, the number opens the keyboard prefilled with what is known; step taps rest 1.3 s then ONE report, with `too_tall`/`too_short` batched when ≥ 2 m and ≥ 10 % off the LiDAR height), the **`today` row** (`trToday`, every tree card, collapsed too — built like the species row: muted one-tap chips `looks right · felled · not a tree · other…`, `other…` = the full list in two lines, once said the word itself in bold and a tap reopens the list with it marked = tap again takes it back). Two kinds of answer in it: a **state** since the flight on a real tree (`state`: felled=`logged` · windthrow · burnt · dead, standing · broken · infested · gone · still standing — the list is the peer's `taxa/dict` `states.values`, `standing` only offered against a reported state; `{identity,state}` → `tree{state,state_date}` onto the row without a refetch, `state:null` takes the reader's back; `when?` beside it = an optional date kept as precise as entered, `2025` · `8.2025` · `14.8.2025` or ISO → `YYYY[-MM[-DD]]`, `trWhenParse`, never future; a changed tree's ring is struck through once in `trDraw`, still selectable; the delta poll keeps state-only trees) and a **wrong apex** (flags: `looks right`, `not a tree` = this apex alone, `{lon,lat,claim:spurious,observed{h_m}}`, no `expected`; `linear` = a straight chain of false trees, mostly a power line the LiDAR picked up — ONE flag near its middle with `expected{absent:true,hint:"linear"}`, the term stays *linear* because it trains the peer's detector; `misplaced` in the expanded card). **Hidden rows**: a chain the peer already hides (`/flags` subject with `expected.spurious.hint=linear`, no apex within 3 m) is a row of its own (`kd` 4, `trLinRow` from `apFlags`), drawn as three faint points; its card (`trLinCard`) has one chip `real trees` → `{lon,lat,claim:looks_right,note:"auto-linear <id> wrong"}` (id read once with `group=report`), toggle + undo like any flag; it never enters a share link. Expanded card only (`.xo`): `misplaced`, versions/history (method, date, state, `by`) and withdraw. **Undo**: after every send the footer carries a muted dotted `undo` + lucide undo-2 icon (`button.u`, `data-a=un`) directly behind the words, collapsed card too; `trUndo` is ONE step back at the peer. In full screen the card folds by itself (`MN_AUTO`): a send re-arms the clock at the answer and the undo carries it as a draining hairline (`button.u .cd`, `mnCd`); a folded card shows the chevron only (no ×: it would deselect the tree). **Slow loads**: a late cell answer never replaces a later pick (`hi.pkN`), a cell pick cancels a link's pending tree (`hi.want`), a partial apex tile only pins the exact tree (≤ 0.5 m), and a pinned tree whose tile reloads is re-bound to the new rows (`hi.re`, `apRe`). Tree sends (`reported — thank you`, `already reported`, `tree added`, `already added`, and a `withdrawn` that wrote a version) → `{identity, retract:"last"}`: the peer hides the reader's latest version, the one before is current again (latest was the add → the tree goes, `trGone`; latest was a withdraw → it is taken back, the tree is the reader's again); a `too_tall`/`too_short` flag that went out with the measure is withdrawn in the same batch. LiDAR flags (`flagged · …`) → the same `{lon,lat,layer,claim,reporter}` with `retract:true` (`trFlagBack`; always with layer + claim — a bare `{lon,lat,retract}` is a tree withdraw); the pressed chip in the `lidar` row is a toggle (tap again = withdraw). `trTally` applies the peer's `tally` (state AFTER the write; n 0 = mark gone). Answers: `✓ undone` / `✓ flag withdrawn`, no second undo (one step per send); 403 `not_yours` → `not your report/flag — nothing withdrawn`. History shows `undo of vN` and dims the hidden version. `withdraw` (expanded card) stays `retract:true` = everything the reader said. Hold the map (`pressTracker`, `neGlyphPressJS`: 500 ms still within 10 px, one pointer, fires on the timer with the START point — time + movement decide, never the hit target; `TestGlyphPressTracker`) or right-click = the tree is ADDED at once at the pressed spot (`claim:missing`, `client_id` UUID, nearby median height as `estimate`, DBH empty) with `undo`; a hold ON an existing tree still drafts there but does not send — the card offers `this one` (open that tree) / `second stem here`; a short tap is untouched; 409 near an apex → `this one` / `second stem here`; 409/404 candidates → chooser. Every POST goes through the relay `POST /api/v1/trees/report` (adds `X-Reporter-Key` from `UMFELD_PEER_OBS_REPORTER_KEY`, forces `layer=trees`, default `claim=tree`, 1 r/s burst 10, ≤ 50 items; counters on `debug/inflight` → `tree_report`). Reads stay direct at the peer: `trees/bbox` blocks `user`/`added`/`reported` (`trIngest`), delta `trees/taxa?since=` every 45 s (`trSync`), `taxa/dict`, `taxa/history`. **Marks on the map** (`trDraw` → `trInk`, one vocabulary, always at the tree's own crown radius — the ring `apDraw` gives it, at every zoom — paper white over a soft paper halo, never a dark rim, no red): solid ring = named, measured or confirmed · dotted ring (`trDots`) = in doubt (no LiDAR apex, or the apex reported wrong) · **centre dot = yours** (`TR.mine`: tree ids, `client_id`s, positions, and `f<lon,lat>` for flags, written by `trTally`) — others' marks are the bare ring · one stroke = changed since the flight. `trKey()` adds the signs on screen with counts to the legend's trees block (`.trk`). `neGlyph.treeDraft(lon,lat)` = the hold without the send. localStorage: `neg-tr` (my trees), `neg-sp`, `neg-tm`, `neg-trh`. |
| `ne_glyph_filt.go` · `ne_glyph_fidx.go` · `ne_filter_expr.go` · `ne_glyph_filter.js` · `api_ne_filter.go` | FILTER (`docs/ne-filter.md`): the funnel button `.gt-flt` after the find box + the chip row `.glyph-filt` (`neGlyphFiltBtn/HTML/CSS/JS`, spliced into `neGlyphHTML`, `neGlyphCSS`, `neGlyphScript` before `applyHash()`); the per-0.025°-bin index `fidx` (NFX1: IJ runs + planar u8/u16 columns, header polynomial IJ → viewBox, `neFidxBuild` from NE store + observed-peer cells + INVEKOS h3 + Natura 2000 + OSM distances + chunk log; LRU `ne_fidx` 48 MB → disk `data/ne_fidx/<state>/` 2 GB → build; `handleNEGlyphFidx` ETag `"<state>-<plane>"`, `neFidxWarmLoop` warms bins the tile handler saw); the grammar twin (`neFiltParse/Canonical/Words/Eval`, mirrored byte-for-byte by the Worker — `TestNEFilterJSParity` runs both over one synthetic bin); the Worker (parse · evaluate · `k` BFS; holds bins for tree points and the card/peek `at` — since 2026-11-03 the veil itself is the **server tile** `v=flt&f=` (`ne_glyph_flt_tile.go`, ring `neGlyphFltBleed` painted with the neighbours' cells, `FLT_BLEED` on the client) loaded by `hiTile` like any layer at every LOD, hover peek line `fltPeekLine`); near full zoom the veil is a **vector** tile `vec=1&sp=` and base/obs tiles come supersampled `ss=2` (`hiSS`/`hiResample`, ring twins `.cur.g`/`.hov.g` — `docs/ne-filter.md` §12); the agent endpoint `/api/v1/ne/filter` + `/filter/fields`. **Overview preview** `ne_glyph_fnat.go`: `/api/v1/ne/glyph/fnat?f=` folds `d`/`o`/`chg` terms over the national res-7 table (`ne_mvt_nat.go`) → `share[]`/`n[]` per frame cell; the page veils the res-7 dots and hex faces by share while full resolution is off (`fv*` in `neGlyphFiltJS`), other terms are named in the readout as "at full resolution". · **§8 national sample** (`ne_mvt_samp.go`, `ne_glyph_fnat.go` `neFnatSample`/`neFnat9`, page `fv9*` + `fvLegend`): every term folds at every zoom from the 1-in-49 sample, res-9 veil in the mid band, only a bare `k>=` says zoom in · **§9 tree figure** (`ne_tree_h.go`): apex height lists from the peer's bins (`tree_h_n`+`tree_h`, tree mode gated by an hourly probe), `v=flt` meta `trees= tx=`, readout exact inside the height term when every tile has lists, else ≈ from the sample · **§9a `ht` = a tree of that height** (any apex in the window, `htmax` = the tallest-tree rule; Worker plane `p=th`, `apKeep` quantised to 0.5 m so the overlay draws exactly the counted apices) |
| `ne_glyph_where.go` · `ne_glyph_dbg.go` · `ne_where.go` | **WHERE panel (`?dbg`) = `GET /api/v1/ne/where`** — top-right `pre.glyph-dbgw` naming the spot under the pointer for peer triage (an observed-layer seam, a wrong class). The panel's body is the **text of `/api/v1/ne/where?pt=&share=<curHash()>&ar=<vb.h/vb.w>&format=txt`** (`neWhereText`), verbatim — what a person reads an agent curls; the page prefixes only its client facts (viewBox xy, live z/lod/full, the doc it holds under the pointer) and the curl line. The endpoint (`handleNEWhere`, public, no-store): point from `pt= lon=&lat= cell= share=` (share = the hash, a full URL or a percent-encoded one; `neWhereParseShare`: pt > at= centre > h3 > cell=x,y > kg), the view from `at=`/`z=` (`neGlyphUnproj`, LOD thresholds mirrored in `neWhere*` consts — keep in step with `HI_LOD0/HI_LOD1/ZMAX0/HI_MIN`), the cell block `neGlyphCellDbg` + `neCellDoc` + `pic_obs/pic_dif`, the fidx row (`neWhereBinRow`: lattice lookup, one cgo call; `neWhereVals`: every catalogue field as value/raw/source, `ht_list`), the peer's cells block from `neFidxSrtmBlk` (LRU only, never a fetch), the filter verdict per term (`neWhereTerms` = `neFiltTermFn` on the row) and the view's matches nearest first (`neWhereMatches`, box clipped to 0.1°, `n` ≤ 200), peer + own URLs. Hover follows (220 ms), click pins, Esc unpins, click on the panel copies; `window.neGlyphWhere(lon,lat)` pins from the console and returns the curl URL. Documented in llm.txt (§ glyph endpoints, §7a'). |
| `ne_glyph_warm.go` · `opn` in `ne_glyph.go` | WARM (§warm below): shared links (link icon clicked) replayed after a restart so copied links stay fast — the link icon beacons the hash + the last two minutes of glyph GETs; a direct open adds its first 25 s of glyph GETs for its device class. Old text: the page beacons its first 25 s of glyph GETs on a direct open (`POST /api/v1/ne/glyph/opened`, no `?trace`/`?dbg`/`?warm`), the server keeps ≤ 150 hashes × 2 device classes × ≤ 500 URLs 30 days in `data/ne_glyph_opened.json` and replays them 30 min after start as `ua=umfeld-warm`. |
| `ne_glyph_test.go` | Hex fit, SVG shape, tile meta, word ban is in `api_ne_public_test.go` (`TestNEPublicWordBan` scans `ne_glyph*.go`). |
| `api_docs_public_ne.go` | `llm.txt` NE section: glyph endpoints + the landing inventory §1–9 (share-link grammar §7, scripted checks §7a). Update with every behaviour change. |
| `api_license.go`, `api_docs_public.go` | Sources `ne`, `invekos`, sibling services, footer. |

Routes: `api_ne.go` switch `rest == "glyph" | "glyph/kgs" | "glyph/tile" | "glyph/cell" | "glyph/kg" | "glyph/sub" | "glyph/fidx" | "glyph/filter.js" | "glyph/opened"` (+ `filter`, `filter/fields` before `neParseReq`, own `format=` vocabulary);
public via the `/api/v1/ne/` prefix + `publicAllowListGolden` (`api_tiers_test.go`).

## Page anatomy

Tool bar `.glyph-tools`: find `/`, recent (places · cells · filters · dwell views: `vpKeep`, 15 s after the last state change while zoomed in, whole hash stored, restored through `applyHash`), link, full screen (no separators between them) · status
slot `.gt-n`. Recent (`neg-hist`, localStorage, 10 rows) keeps places (KG · admin · point · cell · res 12)
AND filters: every expression the Worker accepted lands as `{k:'f',f:canon,n:words}` (the build-up steps
collapse into the finished one, prefix rule in `pushHist`); a tap on a filter row re-applies it (`fltSet`). The full-res button `tD` (`.gt-d`) is still wired (busy spinner, aria state, soak)
but `hidden`: full resolution is automatic (`hiAuto`), the status slot took its room.
**In-view note / card** (`iv`, `ivNote/ivShow/ivPlace/ivClose/ivToggle`): with a **floating** card
(desktop) nothing picked meant no legend at all. Once the tiles are in, the loading note in `.gt-n`
settles into `ⓘ in view · observed · 9 706 k cells` (`.gt-n.iv`, lucide info, a button); tapping it
opens the in-view legend as the floating card in that corner (`.glyph-tip.iv`, no grip, dock + × kept)
and the note steps away while the card is up; ×, Esc, dismiss close it and the note returns. Nothing
shows before the picture is complete (`hi.left || hiLeftBy()`); once up it stays through pans. Hover
cards take the spot and the in-view card returns in `hide()`. Dock ⇒ the sheet (carries the legend
itself), float back ⇒ in-view card again. Share token `;info`. Phones / `;below` (docked) never see
the note. Hook: `state().info`, `state().infoNote`, `info(true|false|undefined=toggle)`.
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
| **change** `;change` · key `c` | ✓ O·2·0·1 — dots `.c .ca` over faded `.h`, `.ch` = one soft halo path under the **confirmed** dots only (opacity `CHH` .1 on the path, so overlaps never add up; width `--chw` set by `chw()` in `blend()`: 72 viewBox units at the overview, z^-0.6 down to 48, gone with the dots above `HEX1`); tiles `ov-chg` over the base at **.4** | ✓ O·2·0·1 — dots `.c` over `.o`; tiles `ov-chg` over `ov-obs` at **.4** (base stays .3) |
| ↳ change **under a filter** | with a veil on (`v-flt` / res-9 sheet) `chgZ()` (`ne_glyph_filt.go`) lifts `ov-chg`+`ov-dif` above `ov-flt`, like the tree rings, under a drop-shadow halo; only when the expression asks about change itself (`fltUsesChg`, `chg=…`) is the tile asked with `f=<expr>` and paints only the matching cells (server: `ne_glyph_flt_match.go`, the fidx bin's own mask; a pending bin shows its cells and the tile is `no-store`) — a tree/slope filter leaves every mark. Marks are grown to ~5 screen px with a dark 1-px rim like a tree ring (`neChgGrow`, `neGlyphIdxChgHalo`, nearest-mark colour): always at LOD 2 (a res-12 cell is 2 tile px there), at LOD 0/1 only while downscaled (`hl=` = the veil's `fltHL`, tile px per screen px > 1). Tile key `chg/<k>[~win][~f<expr>][~h<hl>]` (`chgKey`). Hillshade `ov-relo` sits right over `ov-obs`, under every overlay. **No window = full tone** — ageing (`tlAge`, `neGlyphAge`) applies only inside a window / while the strip plays. | same |
| ↳ loading hint | `hiStatus` → `hiLeftBy()`: `loading · declared 12 · change 27 · relief 4 · photo 3 …` — every tile view in flight (`hi.tiles` with `!ok`) plus the relief/ortho queues; `relStatus`/`otStatus` refresh it. | same |
| **subsidy** `;subsidy` · `s` | ✓ O (res-7 dots/faces) · 2·0·1 Schläge as drawn (`sub`, fill dimmed .8), the same tile rules as the profile at every LOD (auto-on, under-LOD cull, eviction, fail-soft; never `off`). **With change: variant `subc`** (new / redrawn / gone vs 2024) | ✓ same rules; `ov-obs` dims like the base does (.8) |
| **trees** `;trees` · `t` | ✓ 0·1 only (`off` below 110×) — crown rings topmost · *was: forced the observed view* | ✓ 0·1 only |
| **relief** `;relief` · `r` | ✓ 2·0·1 (full res only; weight eases to ½ past 300×) — hillshade blended over every fill, under the trees | ✓ same |
| **diff** `;diff` · `;diff=raw` · key `d` | ✓ 0·1 only (`off` below 110×) — magenta marks where the register and the LiDAR disagree about the same cell (ne-change.md §6d, the three corroborated kinds; `=raw` lifts the corroboration): solid *structures declared · none in the LiDAR*, chequer *structures in the LiDAR · none declared*, ring *declared forest · currently no trees*; `ov-dif` over the base at **.4** (like change); legend head = lidar epoch as an age | ✓ same over `ov-obs` at .4 |
| **time** `;t=` · keys `< >` space | ✓ with change at every LOD — the strip under the view bar (passes · days of a pass · calendar), chg tiles/paths windowed and **aged** (fresh hue → ash, floor .18, scale = min(history, 1 y)); play = day-by-day at level 2, paths only, tiles/card/legend catch up on pause (ne-change.md §6e) | ✓ same over `ov-obs` |
| **photo** `;ortho[=0.1–1]` · `o` | ✓ 0·1 only (`off` below 110×) — basemap.at orthophoto **under the base** (`ov-ortho`, first child of `.hi`), weight = the reader's (default .5, `OT_OP`): ≤ ½ the photo just fades in beneath the cells, past ½ the profile fills step back (`--otf` on the svg → `filter:opacity()` on `.b`/`.ov-obs`, .35 at 1) so it reads through; `saturate(.8)` keeps the data colours in front. Popover `.gv-op` (hairline range, bar head): click = toggle + show for a moment, hold 380 ms / right-click / wheel = adjust without toggling, hover on a pressed icon shows it; anchored above the icon's own row (the bar wraps on phones) | ✓ same under `ov-obs` |
| **roads** `;roads[=0.1–1]` · `w` | ✓ 0·1 only (`off` below 110×) — OSM roads + rail (`v=rd`, `ne_glyph_roads.go`) over the fills, change, diff and subsidy, under relief and trees: one quiet warm-grey ink, wider = higher rank, **darker = more remote** (ink weight from the 250 m remoteness grid, `osm_remote_grid.go`: ×.5 in a city centre … ×1.5 in the back country, + ≤ .5 px; rail and casing untouched; no hue → colour-blind and greyscale safe), dashed = service ways, white ties = rail, a pale casing under every line so it reads on a dark fill or the photo; whole-layer weight = the reader's (default .7, same popover as the photo: click toggles + shows, hold / right-click / wheel adjust) | ✓ same |
| **my position** (no token) · `l` | all LODs — the browser's Geolocation (`watchPosition`) as `g.lc` (`.lch` halo = max(2.4 r, reported accuracy in map units via `proj.scale`), `.lcp` pulse ring `lcpulse`, `.lcd` dot) in the found point's ink, sized like `ptSync`. Button `.gv-loc` (lucide locate / locate-fixed, `.gv-l` after roads) shown only under `(any-pointer:coarse),(pointer:coarse),(max-width:1024px)` — phones and tablets; a mouse desktop keeps the bar lean, the key still works. `lc` state machine (`lcTap`): stage 1 watch + `fitBox` to the dot **at full resolution** (ZMAX lifted like selectKG, box = finest LOD `W/HI_LOD1` around the dot, `landFull` switches once landed), **nothing written** (no hash token, no `pushHist`, nothing sent); stage 2 `lcPin` → `goPoint({t:'loc'})` = the found point (`pt=` in the link like an address, `pushHist` skips `t:'loc'`) + full-res fit (ZMAX lifted like selectKG), `g.lc.pin` hides dot/halo so the point's own dot shows; stage 3 `lcOff` clears the watch and, if the point is ours, `ptClear` + selection + card. Fix outside the frame / error → off + a one-line hint that says what to do (`lcMsg`): code 1 with the site permission `granted` = device location off → “turn on GPS”; code 1 within `LC_FAST` (no prompt was answered: off or blocked, WebViews answer 1 for both) → “turn on GPS · allow location for this site”; a slow code 1 without a grant = the reader declined → “tap again to ask”; 2/3 → GPS nudge. All but the declined case arm a 3-min quiet retry (`lcBack`: back from the settings shade/app, or permission `onchange` → granted) so switching GPS on resumes without a tap; `locate({err,perm,dt})` returns the hint text; hidden tab → off unless pinned. `neGlyph.locate()` state (`{on,stage,fix,center}`), `locate(true|false)` steps, `locate({lon,lat,acc})` feeds a fix without the sensor (soak step `locate`). Datenschutz 2.6 / Impressum describe it — keep in step. |

Reserved tokens parse and are ignored today (`dbg: reserved token`), so a link minted later still opens.
Overlays combine freely; **z-order** bottom → top: profile fill → change → diff → subsidy → roads → relief → trees.
**Legend merge** (`hiLegendHTML0`): one `.lg12` = head `in view · <profile>` + profile bar, then one
block per overlay in z-order, every head prefixed `in view ·` so it never reads like the picked unit's own `change`/`declared` rows above: `change` (Δ bar, confirmed/candidate, sighting period) · `diff` (`difBlock`: kinds in view, `n of m cells compared`, the *corroborated only / all cells* toggle, mixed-epoch KGs) · `subsidy`
(claims; "vs the year before" when change is on) · `roads` line (`rdLine`: km in view by rank, source, weight) · `relief` note · `trees` (count, tallest, band bar).
**Card source order** (`srcs`): change first when on, then the profile's source, then the rest.
**Hash grammar**: `;tree=lon,lat` the picked tree (6 decimals; implies `;trees;full`, without `at=`/`z=` the view goes to it at 900×; `apWant` pins the apex within 3 m once its tile lands, with the jump-in pulse; a whole tile without one → hint *that tree is not on the map any more* and the token goes; never written for an unsent draft; `/ne/where?share=` reads it as the point) · `;v=obs` profile · `;change ;diff[=raw] ;subsidy ;trees ;relief ;ortho[=op] ;roads[=op]` overlays · `;t=2|2..3|2@d1..d2|@d1..d2` time window (implies change; `at=x,y,z` stays the viewport); legacy `;v=chg` reads
as declared + change, and `;trees` no longer implies `;v=obs`. **Keys**: `v` flips the profile, `c d s t r o w`
flip the overlays. **Busy**: the profile word trails dots while its tiles load; an overlay toggle pulses.
**Soak**: `m.matrix(kg)` walks all 2 × 2⁴ combinations (+ diff and photo each alone / with change / trees / relief) by share link at LOD 1 (and the change/subsidy
combos at LOD 2 and 0) through `layers()`, whose expectations are read off the hash (`expectOf`).

## JS map (`neGlyphScript`, search the function name)

- **State**: `vb` viewBox, `W H`, `cells[]`, `kgs`, `view` (`'' | obs | chg`, `VIEWS`), `hi`
  (full res: `on tiles order l[] g pin hov apx tree left`), `rl` (relief), `sb` (subsidy),
  `sel` (KG), `pinned` / `shown` (res-7 index), `picked` + `pk {t: cell|kg|h3|tree|pick}` (what
  the card shows), `auto` (auto full-res), `tour`.
- **Filter readout × selection** (`fltIv`/`fltSelS` in `ne_glyph_filt.go`): with a KG / area / combined selection
  (`ivTok()`) the readout and the legend's filter line count the cells of THAT unit — "m of n cells selected · ha / ha
  · trees" — from `/api/v1/ne/glyph/area?u=<unit>&f=<expr>` (the whole unit, = the export's row; figure `trees_n`;
  with `f=` the handler runs the light count `neStatsComputeOpt(…, light)` — cell-exact up to `neStatsReadoutBins`=400 bins from the bins at hand (`neFidxPeek`; missing bins = `pending`, the page asks again after 3 s), since a rare match is invisible to the sample; beyond that the 1-in-49 sample, ≈). The veil and the tile sums are untouched; without a
  selection, or without an answer, the tile sums stand. Debounced, cached per unit+expr, last answer stands.
- **Filter overview preview** (`ne_glyph_filt.go` `fv*`, state `fv {f lf d ld err h}`): `fvOn()` = filter active
  AND `fltFar()` (full res off or the view spans > 36 bins) · `fvSync()` (from `setVB`, `fltView`, `fltSet`)
  fetches `/ne/glyph/fnat?f=` once per expression, toggles svg class `v-fv`, debounces the readout on pan ·
  `fvPaint()` appends `<path class="fv">` per opacity bucket (`FV_B`=8 buckets × `FV_A`=.62 max) to the
  dots group, `fvHex()` the same per hex tile `<g data-b>` (called from `buildHex`), `fvTop()` re-appends
  them after `tlPaint` · `fvReadout()` = `fvSum()` over the hexagons whose centre is in `vb` ("Austria" when
  all are) · `fvDotLine(ix)` = the res-7 card row "filter n % of the cells" · the funnel button gets
  `.off` when nothing folds (readout says which terms wait for full resolution). Debug: `?dbgveil` paints
  the preview magenta too and exposes `window.__fv(lon,lat)` → `{i, share, bucket, on, far, active, row,
  veil, d:{exact,skipped,coarse,match,cells}}` for the nearest hexagon.
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
- **Trees**: `hiApex/apRender/apCard/apUnpin/setTree`.
  Picked tree = its own crown ring in another state, never a fill (`g.tp`, built by `apMarks`): the ring
  in paper white, 2.2 px growing to 4.4 px with the crown (`--w`), over a dark seam, plus a quiet outer
  ring 6 px out; `apDraw` skips its canvas ring; `.cur.cap` draws nothing. Motion: a tap = ONE wave
  outwards (`.nu`, `AP_TAP` 640 ms — the finger covered the crown); a jump (`apPin(h,true)`:
  `neGlyph.tree()`, *this one* on the card, a fresh draft) = four 1.6 s beats, each a wave of
  `AP_WAVE` 22 px (the wave animates the circle's `r` from `--r0` to `--r1`, never a CSS `transform` — Safari strokes a scaled circle at these coordinates with a stray white bar) while the outer ring breathes (`.pu`, `AP_PULSE`); negative `--d` delays survive
  redraws; none under reduced motion. Hit test `apAt`: pointer inside a crown (≥ 7 px) → the smallest
  such crown; else the nearest ring within the slop (14 px touch · 5 px mouse).
  Reader marks (`trDraw`): solid ring = named/measured/confirmed, dotted ring = in doubt,
  centre dot = yours, all at the crown's own radius (row *TREE REPORTS*). Hovered tree = `hovRing(d,true)`
  (`.hov.tr`). Deepest zoom `HI_ZMAX` = 2400.
  **Tree sprites at LOD 0 and LOD 1 — `/trees/bbox?format=columns` contract** ({OBS_NAME}, CC BY,
  CORS, no key; `APEX_Q`, `apexKey`, `apObj` next to `hiApex`): ONE call per tile at both LODs,
  `bbox=<tile>&min_height=3&format=columns&fields=lon,lat,h_m,crown_m2,species,vitality,kg_code`
  — every apex ≥ 3 m tallest first as parallel arrays (lon/lat 1e-6°, h_m 0.25 m steps, crown_m2
  0.1 m²; ~6–8 B/apex gz; `limit` default = max = 100 000, so a dense alpine 0.025° tile — ~31 k
  apices — is one ~250–280 KB response, ~70 ms server / ~110 ms wall warm; a 0.01° LOD-1 tile
  ~5 k apices, ~30 KB, ~60 ms). `truncated:true` → `e.trunc` + `apex truncated <key>` in the `?dbg`
  log; bare `ready:false` (+ `retry_after_s`) → thrown as 503 → `hiRetry`; with apices → drawn, refreshed (below). The same endpoint, QA
  (`realistic=1`) and fields at both LODs, so zooming between them never changes a tree (LOD 1
  used to read the res-12 `/cells` tree layer — slower, larger, and a different set).
  **Caching is the browser's HTTP cache**: the server sends a strong ETag (per-KG NE file state +
  query) and `Cache-Control: public, max-age=86400` when ready; a
  re-fetch within max-age costs no request, after it the browser revalidates with `If-None-Match`
  (304 in ~2 ms). While a KG of the tile is still **pending** the peer answers `ready:false` +
  `pending_kgs` + `retry_after_s` **with the other KGs' apices** and `Cache-Control: public,
  no-cache` (since its `trees-c2`; the ETag hashes the pending list, so it flips the moment the KG
  lands): `hiApex` draws the partial tile (`e.part`), re-asks after ≤ `APEX_REFRESH` (60 s) — the
  browser revalidates, a 304 is the same body (still pending), a 200 the whole tile; `hiRetry`
  caps a long `Retry-After` the same way for a bare `ready:false`. A bbox inside the peer's
  envelope but touching no KG is `ready:true`, empty columns, cacheable for a day; `no-store` only
  on a transient index error (`error` + `retry_after_s`, retried like a failed tile). Keep the query string byte-identical per tile (it is part of the ETag) and never
  add a `cache:` option or a custom header to the fetch (a custom header forces a CORS preflight).
  **No height bands any more**: the filter window (`fltTreeRange`, 0.5 m units) is applied
  client-side by `apKeep` to the cached tile, so changing `ht`/`htmax` never refetches; the key is
  plain `apex/<k>`. **Columnar in memory**: `e.c = {n, lon, lat, h, cr, rm, x, y, r, sp, vt, kg}`
  (x/y glyph units, r crown radius in glyph units); `apDraw`, `apSum`, `apFilt`, `apAt`,
  `apFlags`, `apMarks` walk the arrays (89 k apices in view drawn in ~11 ms); a tree object exists
  only once touched — `apObj(e,i)` builds and memoises it in `e.ob` (flag state `fg`/`fl` sticks
  to it; `e.flI[i]` holds the matched open flag per index). `hi.apx = {e, i, a}`; identity is
  `(e, a.i)`. `apWhat` accepts the words the columns carry (`spruce`, `vital`) and the codes of the
  cell layer alike. Check: `#pt=10.0875,47.1375;v=obs;full;trees;z=260` and `;f=ht=20..25` →
  trees drawn, `left=0`, `neGlyph.apN()` non-zero, no `error` events; `z=900` for LOD 1; a tap on
  a ring opens the card with species · vitality · crown · KG. **Picked tree** = `g.tp` (above; never `.sel` — that class is the KG outline and is hidden at full res). **Relief**: `relView/relStatus/setRelief` (`mtWant(z,n)` = the Web-Mercator squares touching the view, shared).
  **Photo** (`ot`, `/* ── ortho`): `otZ` (one step finer than `relZ`) · `otView/otTile/otPump/otFetch/otSettle/otClear` (relief's shape; URL `ORTHO` = mapsneu.wien.gv.at WMTS `{z}/{y}/{x}.jpeg`, 404 = outside coverage, ≤ 6 in flight, `OT_KEEP` 240) · `setOrtho(on,quiet)` · `setOrthoOp(v,quiet)` + `otApply` (group opacity + `--otf`) · popover `otPop(on,ms)/otPopSync` (`otHeld` swallows the click after a hold) · fetch class `ortho`, mark `the photo`, group `basemap.at`.
  **Roads** (`rd`, `/* ── roads`): `RD_OP` .7 · `setRoads(on,quiet)` · `setRoadsOp(v,quiet)` + `rdApply` (group opacity on `.ov-rd`) · `rdPop = mkPop({…})` · `rdLine()` legend line from `hiSum('rd')` (`km rd rdc`; `rm` = Σ rm·Σrmb / Σ Σrmb is aggregated for scripts, not shown — the remoteness reading is the one-time hint on enable, never legend clutter); tiles ride the ordinary `hiTile(t,'rd')` path (key `rd/<lod>/<i>/<j>`, no `~` suffix — the weight is CSS, never a refetch).
  **Popover factory** `mkPop({btn, panel, on, turnOn, get, set, commit})` → callable `pop(on, ms)` with `.sync()` and `.held()` (swallows the click after a hold); `otPop` and `rdPop` are its two instances.
  **Subsidy**: `setSub/subSync/subVis/subLoad/subBuild/subHex/subSum/subSumOv/subLegendHTML/
  subCellLine/subDotLine/subKGLine/subSw`.
- **Time** (`tl`: `win lvl k bins d ld map pt pg play`): `tlLoad/tlData` (passes JSON, scope = selection) · `tlBins` (level 0/1/2) · `tlRender` (track, dots, head marker + label, readout) · `setTime(w,quiet)` · `tlParse/tlStr/tlLabel` · `tlMap` (cell → [conf, cand, day]) · `tlPaint(force)` (additive overview paths `.tl`, per tone × day in `tl.pg`) · `tlAge/tlScale/tlAgeSet/tlAgeSync` (ash) · `tlHead` · `tlStep/tlPlay/tlPause/tlResume/tlStop/tlTick` · `tlWait` (play holds while full-res tiles are in flight: dots on the date, `.tl-play.wait`, `state().timeWaiting`) · `tlCatchUp` (tiles/card/legend after a play) · `tlQ()` (`&t=` for cell docs) · `tlRepin` · `tlBusy`; head date → `.gt-dt` (left-bound on the tools row, `tlDateSet`); drag on `.tl-s` (`tlGrip` picks the mode): finger on the head hairline (within ~half a bin) = drag the window's END, head follows the finger continuously and settles on the bin edge on release; start inside the window = slide it (width kept, clamped to the strip); start outside or shift-drag = range from the anchor bin. While the finger is down (`.glyph-time.dg`) there are no transitions and `setTime` skips `tlCatchUp` (tiles/card/legend catch up on release); `hiEvict` does not protect change tiles of another window, so a scrub cannot grow the cache past `HI_KEEP`. Below 640 px / coarse pointer the card's inner div is `width:auto` (fills the figure).
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
  The veil is on whenever an area was asked for by name (KG · Gemeinde · Bezirk · Land · site), in the
  overview too (`svg.glyph.veiled`: fill and stroke of the selection go, the un-veiled land is the selection).
  Browsing never veils: a pinned res-7 cell (zoom, arrow keys, click) and a KG inferred from a point
  (address · toponym · own position, `pt` set) show no veil — the only whitening left in those states is
  the observed view's own "not observed yet" picture.
  **Combined selection** (several outlines as one area): an `asel` with `.set` = members
  `{t:'kg'|'adm', c, n, s, d, eo, b, idx, kgs, ex}` (`selectSet`, built by `setMember`). Cells and KGs are the
  union (nothing counted twice), `veilSync` cuts the veil with one mask path per member (`.veilmg`; one evenodd
  path would punch overlaps back out), the card is `admCard` over the union with the sub-line
  `combined · <area> · 4 KGs + 1 Gemeinde · 43 KGs in all` (`cmbSub`). **The area is the server's**
  (`setAreaGet` → `/api/v1/ne/glyph/area?u=`, `handleNEGlyphArea`): the combined unit of `/ne/stats/unit`
  (`neStatsUnitSet`: union of cells, every cell once whatever the overlap; same memo as the export, so card and
  sheet cannot disagree). Until it arrives the outline estimate (`setArea`) stands with `≈`; a selection beyond
  48 bins shows the sample figure with `≈` and is re-asked until the background cell-exact pass is in
  (`neStatsExactAsk`). Hash `#kg=a,b;adm=c,d` = the `share=` of the export (one combined row).
  Bounds `SETMAX` 50 members / `SETKG` 300 KGs (= `neStatsMaxKGs`) → refused with a hint, selection kept.
  UI, all in the find box: a quiet **+** at the end of the settled box (`.gt-a`, shown while the selection is a
  shape — `.cmb`) opens add mode (`openAdd`, `.add`): shapes only, a ＋/− mark per row, the empty box lists the
  members with a − each; under a mouse the box stays open across picks (`boxHold`), on touch it settles. Tags:
  two members = two names, more = first + `+N` (tap → the member list); under a mouse a tag's glyph turns into
  × on hover (every tag kind); Backspace drops the last; Shift/Ctrl/⌘ on a pick adds from the plain box; a box
  holding a set opens in add mode, and its × there only closes the list. `selParts/selGo/selToggle/selDrop` are
  the whole API; `collapse()` is the reader's close (ends a hold), `settle()` the code's own.
  **Selection figures** (every selection: KG, area, combination): `setAreaGet` asks `/api/v1/ne/glyph/area?u=<kg=…;adm=…>`
  (`handleNEGlyphArea` = the `/ne/stats/unit` unit, same memo as the export) → `s.fig`, copied onto the row by
  `areaRow` (`row.cmb`, `row.fig`). The card's declared area (`decLine`), observed line (`figObsLine`) and the
  subsidy share read it; `undefined` = asked (the line says *measuring…*), `null` = none (a Bundesland: > 300
  KGs) → the page's own res-7 figures stand. The change row stays on the chunk-log counts (`chgHTML`).
  **In view × selection** (`ivGet`, `ivKey`): the in-view legend's profile block counts only the cells inside
  the selection — the unit clipped to the view (`&clip=w,s,e,n`, `neStatsClip`; no clip when all of it is in
  view), debounced, last answer kept while the next loads; header `N cells selected`. The overlay blocks
  (change, diff, subsidy, trees) are tile sums and are headed `whole view · …` while a selection stands. The
  filter is not applied to it (its own block sits on top).
  **Cut lines open in place**: a card line ending in … (`.rw>.vl`, `.sub`, `.hd>b`) wraps on tap / click and
  folds back on the next (`wrEl`, classes `w-dec|w-ob|w-chg|w-sb|w-sub|w-hd` on the card, reset per subject).
  A future filter component = build `{idx, a, row, d, b}` the same way and call `drawSelD` + `showSel`.
  Wheel zoom is coalesced per frame (`whl`/`wheelFlush`): deltas of one frame are summed, one
  `zoomAt` per rAF, events older than 250 ms dropped — slow office PCs otherwise replay a stale
  backlog after the hand stopped or reversed, which looked like inverted zoom.
- **Card buttons** (`.glyph-tip`, right of the header): dock toggle `.glyph-dk` (desktop; `;below`), minimiser
  `.glyph-mn` (phones/touch, docked desktop cards and every full-screen card; `;min`; folds to the header line, header or chevron unfolds,
  undocking unfolds), × `.glyph-x` (`dismiss` — **always a close, in full screen too**; the way back is the ⓘ in-view note in the
  lower right corner. Folding and closing are two controls, never one button doing either). A docked sheet (phones, `;below`) **rests on the in-view legend**:
  `sheetRest()` = hint line + `hiLegendHTML()` at full res — `hide()` folds whatever is on it back to that (never
  to the bare hint, which had no way back), the chevron folds it to the legend's header line (`IN VIEW · OBSERVED ·
  n cells` = the phone's ⓘ note). The × is `visibility:hidden` while the card is not `.x` — faded out it still
  hit-tested and, sharing the corner with the chevron on phones, swallowed the tap (dismiss instead of fold).
  The × must always be visible feedback: floating → the card slides
  away; docked → `hide()` folds whatever is on the sheet to the one-line placeholder `PH` with the `setHTML`
  height animation, even when the caller (`hiUnpin`, `apUnpin`) nulled `picked` before `hide()`.
- **Share links**: `curHash()` writes `kg= cell= h3= q= tree= v= change diff[=raw] full nofull trees relief ortho[=op] roads[=op] subsidy screen info
  at= open min below card= export`; `applyHash()` parses the same (regex per token, unknown token → ignore
  all) and applies in order `autoSet → setView → setTree → setRelief → setOrthoOp/setOrtho → setRoadsOp/setRoads → setSub → fs → open → dock`.
  **Auto full res on a link** (`hiLand` in `applyHash`): a bare `#kg=`/`#adm=`/`#pt=` switches once the fly
  has landed (`W/vb.w ≥ HI_MIN`, `auto.by`), `;z=`/`;at=` beyond `ZMAX0` lift `ZMAX` and switch (`deep`, same
  rule as `hiAuto` crossing 24×), `;full` always. The only opt-out is `auto.opt`: `;nofull` sets it for THAT
  link only (`autoSet(nofull || autoPref())` — the next hash re-reads the saved preference, so a `;nofull`
  link never makes a later bare `#kg=` land coarse), the saved preference comes from `hiLeave` (esc out of
  auto full res) alone. `mem.coarse` / `DEVMEM` size the tile budget (`HI_KEEP`), they never gate the switch.
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
a 5xx, relief forgets a failed tile. **The interaction is the retry** — and for the stack's own tiles
also a timer: `hiRetry(key, err)` re-runs `hiView(true)` after `Retry-After` (429) or a 3 s + 1.5 s·n
back-off, ≤ `HI_RETRY` 6 times per key (`hi.badN`, cleared when the key lands or on `hiOff`). Before
that, 7 of 120 tiles that got 429 from the API bucket stayed blank behind `… +7 tiles` until the
reader panned (2026-11-04); the glyph now has its own bucket (`neGlyphRateBurst` 400). `netTouch()` (`hiView`, `pinAt`)
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

## Tile cache — RAM, disk, ETag

`handleNEGlyphTile` answers in this order: (1) `If-None-Match` equals the key's ETag (`"t"+sha256(key)[:16]`) → 304,
no cache read, no render (only complete tiles are ever sent with the ETag; the handler's own ETag also keeps its
`max-age=600` — without it the cache middleware rewrites to 120 s); (2) `neGlyphTileLRU` (64 MB); (3) disk
(`ne_glyph_tile_disk.go`, `data/ne_tiles/`, ≤ 4 GB and ≤ 400 k files, least recently used first, `NE_TILE_DISK_MB=0`
= off, off under `go test`); (4) render behind the gate (`NumCPU` slots). The key carries every state the picture
reads, so a disk tile never needs invalidation — a moved state is a new key and the old file ages out. Survives
restarts; shared by all readers. Numbers: `debug/inflight` → `ne_glyph_tiles.disk{hits,misses,writes,bytes,files}`
and `etag_304`. Why (2026-11-10): reader tiles at LOD 2 had p50 1 s / p90 8–17 s — queue time behind two render
slots — while the same renders unqueued took ~150 ms.

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
- **Load fold** (`lf`, `LF_N=12`, `lfSync` from `gvBusy`): from 12 pending pieces (tiles + relief + photo) the card
  folds to its header — not a fold the reader made, not while the reader is in the card — and once nothing is pending
  for 450 ms it unfolds in the reduced mode (`setOpen(false)` first); the chevron ends the automatic part; `;min`
  is not written to the hash while the fold is automatic. The res-9 veil sheet stays until **every** exact veil tile
  of the view is in (`fv9Hold`), one swap at the end instead of lifting tile by tile.
- **Downscaled tiles far out** (`ds=2|4|8`, `hiDS(lod)` from `fltDP`: a tile px covering ≤ ½ / ¼ / ⅛ device px;
  the server box-filters the finished tile with `neGlyphTileDownsample`, LRU key `/ds4`, tile key `~s4`): at the
  lowest full-res zoom a LOD-2 tile was drawn at a twentieth of its size, and a tall phone (412×915 @ 3×) needs
  ~100 of them per screen — the old flat cap of 48 nearest the centre left stepped holes in the corners (relief
  and veil included). Now the cap grows with the downscale (`hiCap` = 48 · min(ds, 4)), the budget counts a
  tile as 1/ds² (`hiCost`, `hiLoad`), a tile of another `ds` steps out once its twin at the current one is in
  (`hiCull`), and `hiEvict` drops stale-`ds` variants first. Relief / photo squares: `mtFit` steps the Web-Mercator
  zoom coarser until the squares that touch the view fit `RL_TILES` (48) / `OT_TILES` (64) instead of cutting
  nearest-first.
- **One raster per batch** (`hiCommit`/`hiFlush`, `hi.cq` + `hi.cq2`): a landed, decoded tile waits and the whole
  batch joins the SVG on one timer (400 ms while `lf.on`, 120 ms else); readout/legend/`fltCardFix` once per batch;
  `.hi.ld` turns the per-image fade off while heavy. Relief squares go through the same batch (`cq2`, not counted in
  `hi.left`). Every insert re-rasters the blended stack, and on a phone that raster is slower than a frame.
  `hiCommit` must sit at **script scope** (it was once declared inside `hiTile` — tiles saw it, `relFetch` got
  `ReferenceError`, the hillshade never appeared; `?trace` column `rel` shows it).
- **Relief without an SVG filter** (`relBake`): the water key (alpha = |R−180|·127, formerly `filter=url(#relf)`
  per `<image>`) is baked into the pixels on a 256-px canvas at decode time (`createImageBitmap` → `toBlob`);
  the filter stays only as the fallback. A dozen filtered images re-ran the filter at every repaint of the
  stack — on an emulated 384×721 @ 2.8 phone that was ~1 s of blank hillshade per tile batch (the "flashing").
- **Filter row height settles early** (`fltSet` provisional chips from the canon groups, `.glyph-filt.act .gf-n`
  min-height 2 lines, `fltChips`/`fltRow` call `fsFit()` synchronously): the map is fitted when the row appears,
  not again when the Worker's words / the readout land, and never with a delay (the ResizeObserver callback
  waited behind the Worker reply → the svg overflowed its box by 26 px for ~0.7 s).
- **Status note over the sheet** (`.gt-n` bottom = `calc(100% + var(--tip-h))`): an observer IIFE after
  `main.classList.add('js')` sets `--tip-h` to the card's height while it is a bottom sheet over the map
  (`fs` + `--sheet:b`); verified on the phone emulation, folded and unfolded.
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

**Picture tile seams**: base and `v=obs` tiles carry a 3-px ring but are translucent (alpha = dominance), so the ring two
neighbours share composited twice — a band of other tone along every seam, 14 px at 2400×. Each picture is clipped to exactly its
tile (`clip-path: inset(ring − var(--pe,0px))`; `--pe` is only the `?deep` panel's knob). Safari 26, checked with the panel: any
overlap, even one screen px, is a visible hairline; exact is not. Blink (Android, 2026-11-12) anti-aliases that clip: two
neighbours covering a seam pixel c and 1−c composite source-over to 1−c(1−c), so up to a quarter of the page shows through — a pale
hairline at fractional scales (DPR 2.625). There the pictures of one LOD group **add up** (`mix-blend-mode: plus-lighter`, the
`[data-lod]` group under `.b` / `.ov-obs` isolated): c + (1−c) = 1. Behind `@supports … and (not (font:-apple-system-body))`, i.e.
not WebKit, where exact was checked; `?deep` switches `add` / `noadd` force either. Repro without a phone: emulate 412×800 @ 2.625,
`#full;at=4882,2441,317`, look for a 1-px paler column in the forest.

**Flag marks are canvas** (`trDraw`/`trInk`): reported apexes used to be SVG `<use href="#ng-tr">` clones (dashed
non-scaling strokes, scaled). Safari 26 left a strip of the map unpainted (page colour, map top down to the tapped tree, ~6 px
wide) whenever `apMarks` rewrote them on a tap — found with the `?deep` switch *flag marks off*. `apMarks` now writes the picked
ring `g.tp` only.

## Phone load triage — `?trace` field recorder + emulated screencast

A load that "flashes" on a phone we cannot hold: add `?trace` (hard reload with a fresh `?r=N` after every
deploy — the page is cached) to the URL and keep the tab in front 45 s. Every 120 ms the page samples
`t | svg top:bottom:w | card rect | card classes | tools rect | filter-row rect | chips/<row children with px>
| visible imgs | relief imgs | all imgs | vb w/h | hi.left | relief pending | fs | innerWxH | vv height | dpr
| visibility | card head`, keeps the rows that changed and POSTs them with `dbg` and the error list to
`/api/v1/feedback` (agent `glyph-trace`). Trace records are triage material, not feedback: `feedbackPurgeLoop` (api_feedback.go) deletes
them 24 h after arrival (at start, then hourly; Datenschutz 2.3). They are served on the private tier only
(the public list and its counts skip them). Read the newest record (`$P` = private prefix, `$T` = a private token):

```
curl -s -H "Authorization: Bearer $T" "localhost:8000$P/api/v1/feedback?agent=glyph-trace" | python3 -c "import sys,json;fb=sorted(json.load(sys.stdin)['feedback'],key=lambda f:f['created_at']);c=fb[-1]['context'];print(fb[-1]['created_at'],c['ua']);print('\n'.join(c['rows']));print('\n'.join(c['dbg']));print(c['err'])"
```

The trace also carries `stall Nms` rows (the 120 ms sampler fired late = main thread busy), `longtask Nms`
(PerformanceObserver), and every `pointerdown`/`click` with the element hit (`button.glyph-mn`, `svg` = the icon
inside a button, `button.glyph-x`…), coordinates and the card's classes; `dbg` has `chevron: fold|unfold` when the
chevron handler ran. A tap that lands on an unexpected element is the first thing to look for.

Reading it: svg bottom stepping = the frame refit (filter row / card changed height); `card classes` `on min`
= the load fold; `hi.left` falling and `visible imgs` rising = tiles landing (monotone is good); `rel` = 0 while
`relief pending` is 0 = the hillshade never made it (see `hiCommit` above). `err` must be `[]`.

Emulate: `browser emulate_custom 384×721 @2.8 mobile touch`, `screencast_start` (max 384×721), navigate to
`http://localhost:8000/?trace&r=N#<hash>`, wait ~40 s, `screencast_stop`. Blank frames between relief frames
show up without looking at pictures: crop the map area and read `signalstats` YAVG per frame — an out-and-back
pair of jumps (e.g. 227→225→227) is a blank; a contact sheet (`select='not(mod(n\,10))'`, `tile=16x10`) confirms.

```
ffmpeg -i /tmp/shelley-screencasts/<id>.mp4 -vf "crop=iw:ih*0.6:0:ih*0.05,signalstats,metadata=print:key=lavfi.signalstats.YAVG:file=/tmp/y.txt" -f null -
```

Before every deploy: `node --check` the first `<script>` of `curl localhost:8000/` (a syntax error silently
kills the glyph — `window.neGlyph` undefined); and a brace-depth check that `function hiCommit`/`hiFlush` are at
depth 1 of the IIFE (`/tmp/depth.js` style crude lexer, or just `typeof hiCommit` from `relFetch`).
The same trap bit `hiHref`: a `function` placed right after `hiTile`'s `.catch(...)` line lands inside `hiTile`.

**WebKit and the batch commit (Safari desktop + every iPhone browser).** WebKit drops the `load` event of an
SVG `<image>` whose `href` was set while detached and does not resend it on insertion; with `hiCommit`
deferring the insert, the whole screen sat decoded in the DOM at opacity 0 — no hillshade, half the tiles
(trace #38: 132 imgs, `hi.left` 0, `err []`, nothing visible). `hiHref(im,url)` therefore sets the href
**inside** the commit callback, after `insertBefore`/`appendChild`, for profile/overlay tiles and relief
squares (ortho inserts immediately and is unaffected); a 700 ms fallback adds `.in` anyway and logs
`image load event missed <key>` to `dbg` — zero of those in a trace is the healthy state. Verify with a real
WebKit, not Chrome (Chrome fires the event either way): `npm i playwright@1.47` (node 18) +
`npx playwright install webkit`, load the share link, count `svg.glyph .hi image:not(.in)` at 8/15/25 s → 0.

## Warm — shared links stay fast across a restart (`ne_glyph_warm.go`)

Every cache behind the glyph (tile LRU, tile geometry, filter bins, pass tables) is in memory and empty after a restart,
so the first reader of a copied link waited for every tile to render again. Now:

- **Share = the trigger.** A link enters the store only when somebody clicks the link icon of the glyph ("link copied",
  `copyLink` → `opnShare`): `{hash, dev: 'd'|'m' (coarse pointer), shared: true, urls}` → `POST /api/v1/ne/glyph/opened`,
  `urls` = the same-origin `/api/v1/ne/glyph/*` GETs of the last two minutes (`opn.all`, ≤ 600 kept, ≤ 500 sent — the
  tiles of the view being shared). Nothing is sent under `?warm`.
- **Open = the refinement.** On a DIRECT open (a usable hash at load; `applyHash()` returned something; not `?trace`,
  `?dbg`, `?warm` — the operator's own loads, always add `?trace` when you open the page yourself) the page notes the glyph
  GETs of its first 25 s (≤ 500) and sends ONE beacon at 25 s or on hide, without `shared` (204, 0.2/s per IP, body ≤ 96 KB).
  The server folds it into an existing shared entry (device-class URL list, open count); an open of a hash nobody shared
  is dropped unstored. No IP, UA, screen size or time finer than the day is kept; `q=`, `pn=`, `dbg`, `trace`, `export`
  tokens are stripped from the hash (`neWarmCanonHash`), foreign and non-glyph URLs dropped (`neWarmURLOK`).
- **Store.** `data/ne_glyph_opened.json` (never committed): ≤ 150 hashes, per hash `s` (copies), `n` (opens), `last` (day,
  UTC), `d[]`, `m[]`; entries untouched for 30 days fall out at load; the newest-then-most-opened survive when full. Entries
  from before the share trigger (`s` absent) stay until they age out but are never replayed. Size line: `ne_warm` on
  `<private>/api/v1/debug/inflight` (entries, shared_entries, urls, beacons, shared_beacons, rejected, state, replayed,
  failed, synth, delay).
- **Replay.** `neWarmLoop`: 30 min after start (`NE_WARM_DELAY`, Go duration; `NE_WARM=0` = off, beacons still stored)
  the newest ≤ 60 shared entries with a URL list (≤ 6000 URLs) go through the in-process mux one at a time,
  `neRenderYield()` before each, 40 ms apart, as `ua=umfeld-warm` (grep the journal for it). The 30 min keep development
  restarts from re-rendering the country. A device class never seen for a link is synthesised from the other
  (`neWarmCounterpart`): the same URLs when the link is not `;screen` (the frame has one aspect ratio everywhere); for
  `;screen` the tile window of each LOD/variant group is re-shaped around the same centre by AR 0.62 (desktop) ↔ 2.0
  (phone), centre-first, capped like the client's `hiCap`.
- **Seed by hand** (a link that is out there but was copied before the trigger existed): the share beacon with an empty
  list, `curl -X POST localhost:8000/api/v1/ne/glyph/opened -d '{"hash":"#kg=63330;v=obs","dev":"d","shared":true,"urls":[]}'`,
  then open the link once on a desktop and once on a phone (or emulate one; no `?trace`) so both tile lists fill —
  or send the lists in the seed. The file follows 10 s later.
- **Check.** A systemd drop-in `Environment=NE_WARM_DELAY=20s` + restart → `🔥 warm: replaying … / done` in the journal
  (remove the drop-in afterwards).

## Soak test (`ne_glyph_soak.js` → `/api/v1/ne/glyph/soak.js`)

Roads step (`run`, `{roads:false}` skips): `;roads` at LOD 1 (default weight written bare, `.ov-rd` opacity .7), `;roads=0.4` with observed + change at LOD 0 (weight kept in hash and group, tile meta carries `rd km rdc`), `;roads` at LOD 2 (token kept, group hidden, icon `off`); `layers()` asserts `rd` tiles = base at LOD 0/1 and none at LOD 2, `ov-rd` shown ⇔ on & near, z-order over `ov-subc` and under `ov-rel`/`ov-tree`, `v-rd`, bar, legend *roads* line, hash token; matrix adds roads alone / with change / subsidy / trees / relief / photo; the share-link round trip compares `roads`/`roadsOp`; `offline()` switches the roads on under each fault.

Photo step (`run`, `{ortho:false}` skips): `;ortho` at LOD 1 (default weight written as the bare token), `;ortho=0.8` with observed + change at LOD 0 (weight kept in the hash, `.b` computes `opacity` .48 (= `--bop` × `--otf`; plain opacity, never `filter:` — WebKit ignores CSS filter functions on SVG groups)), `;ortho` at LOD 2 (token kept, group hidden, icon `off`); `layers()` asserts `ov-ortho` shown ⇔ on & near, first child of `.hi`, `v-ortho`, bar, legend *photo* line, hash token; the share-link round trip compares `ortho`/`orthoOp`; `offline()` switches the photo on under each fault and counts the `ortho` class; a 404 from basemap.at (outside coverage) is not a bad fetch.

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

**Faster runs.** The server's tile rendering is the wall clock, not the checks: drive one page per
KG in the same browser (`run({kgs:[kg], spots:1, seed})` in each) — two WebKit pages finish in
~6 min against ~11 min for one page doing both. `stress` lowers the tile budget to 24 cost units
with `neGlyph.keep(n)` for its walk (a `ds=2` tile costs ¼, `state().mem.load` is the figure the
budget is held against) and restores it after: eviction is exercised in eight stops instead of thirty.
`settled()` also waits for the load fold (`state().mem.fold`, the card unfolds 450 ms after the last
piece), the decode queue and a pending `pin` lands when its cell doc answers — poll `state().pin`
/ the hash for `h3=` rather than sleeping. The legend's *trees* block follows `state().avail.trees`
(trees toggled on below `HI_TREE` have no block yet).

### Agent toolkit — verify a glyph change without a screenshot

All from one `const m = await import('/api/v1/ne/glyph/soak.js?'+Date.now())` on `http://localhost:8000/`
(headless browser eval). Each returns **one string**; nothing needs the page to be looked at.

| Tool | One call | Gives |
|---|---|---|
| `m.check(hash)` | `await m.check('#kg=63330;v=chg;full;relief;z=200')` | goes there, settles, runs every `layers` check (view/full/lod/tiles complete/overlay=base/apex/sub/`.ov-*` display/svg classes/legend/hash tokens; on a docked sheet — phone emulation or `;below` — also the sheet round trip: rests on the in-view legend with the chevron, pin → card, same cell again → legend back, chevron folds to the header (`;min`) and unfolds; `{expect:{sheetTap:false}}` skips) with expectations read off the hash itself, plus **no JS errors** and **no non-200 fetch** (204 relief / 204 sub-when-peer-down tolerated) → `check … PASS 20 checks` or one `FAIL` line per miss. ~1–3 s warm. `{expect:{…}, verbose, settle}` override. |
| `m.tile(spec)` | `await m.tile('1/1511/4712/chg')` · `m.tile({lon,lat,lod,v})` | `{s, m (X-NE-Tile minus ms), bytes, png, w, h, sha}` of one tile — JSON, no bytes. |
| `m.snap(name[, specs])` | `await m.snap('before')` | stores the tiles on screen (cached, in view, of the current LOD, every view) — or the given specs — in the Cache API under `name`; survives reload **and** server restart; one line per tile `key status WxH sha n=`. |
| `m.diff(name[, specs])` | `await m.diff('before')` | re-fetches and compares each with the stored one: status, every meta key (`n 3309→3308 · lu …`), bytes, and **changed pixels** (`px 493992/1126624 (43.85%) bbox x0,y0,x1,y1`) → `same` or `CHANGED …` per tile. |
| `m.stress(kg?)` | `await m.stress('63330')` | long-session path: lod-1 spiral walk (views alternate) past the tile budget (lowered to 24 cost units for the walk, `{keep:n}` / `{steps:n}` override) → budget held (`mem.load`), `left=0`, under-LOD hidden, decode drained, eviction ran, no JS error; one line `keep/peak/evicted/shownMax/maxLeft`. ~20 s. |
| `m.offline(hash?)` | `await m.offline('#kg=63330;full;v=obs;trees;z=300')` | fail-soft under faults (§offline): for `offline`, `5xx`, `peer` — inject, then pin / view / trees / subsidy / relief / pan, settle → **no uncaught error, no `.busy` button, `left=0`, no tile `ok=false`, offline noted, card `.rw.off`, faults reached tiles/peer**; lift → pan + re-ask subsidies + pin → tiles complete, apex back, no offline row, a real card, no busy, no errors. ~30 s. Checks that no fetch happened under a mode (all cached) become `warn:`. |
| `neGlyph.log()` | `JSON.stringify(window.neGlyph.log())` | `{errors:[{t,m,at}], fetch:{tile:{200:n,204:n,404:n,abort:n,err:n}, relief:{…}, peer:{…}, api:{…}}, dbg:[…]}` — the page's own counters since load / `logReset()`; `dbg` is the 40-line gesture/nav trace (`?dbg` only puts it on screen). |
| trees | `neGlyph.apices()` → `[{x,y,h,lon,lat,k,i}]` apices on screen (client px, filter applied) · `neGlyph.tree(n)` / `tree(lon,lat)` pins the n-th / nearest one through `apPin` (the tap path) · `tree()` = the pinned apex with `dist` card ↔ tree and `covered` · `tree(null)` unpins · `neGlyph.tap(x,y[,touch])` a real pointerdown/up at client px · `neGlyph.cardBox()` = card rect, classes, `anchor`, `moved`, `min`, `fs`, the ⓘ note text, head line, and `tree{dist,covered,onScreen}` | tree card behaviour without a screenshot: a tree tap must give `dist` ≈ 20 px, `covered:false`; × → `on:false` and `note` set; chevron → `min:true`. |
| filter | `?dbgveil` in the URL, then `window.__fv(lon,lat)` (overview preview, above) · `svg.glyph` classes `v-fv` (preview) / `v-flt` (cell-exact veil) · `.gf-n` innerText = the readout · `neGlyph.log().dbg` lines `filter overview share rows=… exact=… skipped=…` | the filter's state at any zoom without a screenshot; magenta = veiled in both modes. |
| server side | `journalctl -u umfeld-at --since -10m \| grep glyph/tile \| grep -oE '→ [0-9]{3}' \| sort \| uniq -c` | tile status counts without a browser. |

Recipe for a tile/paint change: `m.check(hash)` → `m.snap('a')` → edit, build, restart → reload page →
`m.check(hash)` → `m.diff('a')`. Only `CHANGED` lines land in context; `bbox` tells where to look if a
screenshot is wanted after all. Spec grammar: `lod/i/j[/v]`, `v/lod/i/j` (= `state().tiles[].k`),
`{lod,i,j,v}`, `{lon,lat,lod,v}`; `v` ∈ `'' obs tree chg dif rd sub subc`.

Hook additions for drivers: `state()` carries `animating`, `refits`, `applying`, `fly`, `tour`,
`animMs`, `viewPending` (the 110 ms `hiView` debounce is armed — tiles of the new window are not
requested yet); `settled()` waits for `applying`, tiles, a running fly (≤ 2.5 s), pending KG-outline
refits, relief tiles and `viewPending`. Without the last one a driver polling at `left=0` could land
in the debounce gap and read the under-LOD backdrop as "not hidden" (the soak `stress` flake).

Bugs it found on day one (all fixed): a pending KG-outline refit zoomed back after a same-KG
`;full;z=` link; `#kg=` links kept a stale pinned res-12 cell; `hi.left` / `rl.inflight` went
negative after an abort; `zoomTo` ran its first frame inline and re-entered via `place()`; the
end of a fly let `place()` start a perpetual 320 ms pan loop; the res-7 change count ignored the
`kg_share ≥ 128` / `absent` dedupe rule the tiles and the summary use.

### Tree sheet opacity (2026-11-11)

The `v=tree` bitmap is **opaque**; its tiles sit in one `g.sh` per LOD group under `.ov-tree` and that group carries the
sheet's opacity (`.843` = `neGlyphTreeSheetAlpha`). With the alpha in the pixels the 1-px bleed ring two neighbours share
composited twice — a pale stripe along every tile seam, 3+ screen px at LOD 2. Clipping the ring instead leaves an
anti-aliased dark hairline. Crown marks (`.ap`) are siblings of `g.sh`. Tile key `/o`, URL `&o=1` (cache bust).
