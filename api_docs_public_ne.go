// api_docs_public_ne.go — public documentation of the NE read API.
//
// Two renderings: the `## NE cells` section of the public llm.txt
// (publicNELLMSection, spliced into publicLLMTemplate) and
// GET /api/v1/docs/ne.md — the stored contract docs/ne-cells.md rendered
// through the public vocabulary (nePublicDocText). Both are covered by
// TestNEPublicWordBan together with this file: the public vocabulary is
// KG, unit (land unit), register, structure, declared / observed.
package main

import (
	_ "embed"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

//go:embed docs/ne-cells.md
var neContractMD string

//go:embed docs/ne-change.md
var neChangeMD string

//go:embed docs/stats-envelope.md
var neStatsEnvelopeMD string

//go:embed docs/qgis-layer.md
var neQGISLayerMD string

// serveNEQGISLayerDoc — /api/v1/docs/qgis-layer.md (public: MVT + PMTiles setup).
func serveNEQGISLayerDoc(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write([]byte(peerText(neQGISLayerMD)))
}

// serveNEStatsEnvelopeDoc — /api/v1/docs/stats-envelope.md (public, same text on both tiers).
func serveNEStatsEnvelopeDoc(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write([]byte(peerText(neStatsEnvelopeMD)))
}

const publicNELLMSection = `
## NE cells — declared land-use statistics per H3 cell (tokenless, H3-keyed)

Complementary to {OBS_NAME} ({OBS_API}/ — the OBSERVED layer per
cell: LiDAR heights, tree apices, structures, NDVI, forest loss, terrain; same H3 res-12 ids;
GET /api/v1/cells?bbox= | ?lon=&lat=&k= | /api/v1/cells/{h3} | /api/v1/kg/{code}/cells, spec
/api/v1/docs/llm.txt#cells there). We serve the DECLARED layer (lu = BEV survey, bev = bevdirect
baseline) and the obs rows srtm posts to us opaque. Join = cell_id, client-side. Contract:
/api/v1/docs/ne.md · attribution "© BEV (CC BY 4.0), bearbeitet · observed
layer © {OBS_NAME} (CC BY 4.0)". Rate: 20 r/s per client (burst 60) → 429.

GET /api/v1/ne                              frozen{} pin, endpoints, counts, dictionary ids
GET /api/v1/ne/manifest[?fields=digests]    per-KG status/digests (ETag; digests ≈ 60 KB gz)
GET /api/v1/ne/manifest?since=<generated_at>   delta: only kgs with updated_at ≥ since + counters, delta:true; cursor = generated_at
                                            of your last answer; older than retention_s (7 d) → full body, delta:false (replace)
GET /api/v1/ne/{kg}[?layers=lu,bev,obs][&format=json]   whole-KG container (application/vnd.ne-cells,
                                            ETag "ne-<lu digest>-<obs digest|->", Range, 304; unbuilt → 404 + Retry-After)
    layers=declared                         THE CURRENT DECLARED LAYER: per cell the peers' chunk-log view (latest confirmed
                                            version, else baseline) → the peer's bevdirect build → the frozen index epoch (lu).
                                            On /{kg} one NEC1 section (header source chunks@<state>, view{…}, register bytes
                                            zero, X-NE-Declared-Source), on /cells /cell /chunk the same rows (bin|json; zstd →
                                            400), json carries declared:"view". ETag carries the view state, max-age=300.
                                            lu = the frozen index epoch (immutable); lu,declared together → 400.
                                            A logged res-10 chunk is authoritative for its cell set: a bev/lu cell the
                                            logged build no longer holds is served only from a neighbour KG whose rows
                                            cover it; if no KG's view holds it, the old row comes back with
                                            flags.superseded:true (bin: flags bit 7, serve time only) — never a hole.
                                            Superseded cells are not in the set /{kg}/declared serves (nor the manifest's
                                            declared{}); they vanish for good once a neighbour's rows or a new bev take them.
GET /api/v1/ne/{kg}/head                    section headers only
GET /api/v1/ne/dict/{id}                    zstd dictionary (16 KB, immutable) named by X-NE-Dict
GET /api/v1/ne/chunk/{h3_res9}              THE primitive: all rows of one res-9 cell (343 res-12 children)
    format=zstd (default) stored frames pass-through, X-NE-Frames: kg:layer:off:len:rows:row_bytes, X-NE-Dict
    format=bin  rows deduped across KGs, X-NE-Rows: lu=30:n;k=29:n[;obs=rb:n]   format=json columnar
    X-NE-KGs: 63330:0.80,63333:0.20 (share of the chunk per KG) · ETag immutable per digest tuple
GET /api/v1/ne/cell/{h3}                    res 12 → {cell, kgs:{kg:share}, lu:{…}, k:{…}|null, obs:{…}|null}
                                            obs ({OBS_NAME}'s observed row, CC BY 4.0): cover{geb,bau,acker,gruen,wald,wasser,
                                            verkehr,alpen,sonst} fractions (= obs_lu[9]), obs_bld_cover, obs_bld_n, h_mean/h_p90/h_max m,
                                            tree_n, tree_h_max, crown_frac, ndvi_mean/amp, dh_mean m, flight_years[], verdict, elev_mean,
                                            slope_mean/p90 °, aspect_dom, roughness, tri, tpi, dtm_change, hansen_loss_year, phenology
                                            (null = not measured) + row_hex/record_bytes/fields as posted; units: srtm /api/v1/cells/dict
                                            res 10 → K record + children[]; res 9 → 302 to /chunk
GET /api/v1/ne/cells?ids=a,b,…              ≤ 512 ids (res 9/10/12 mixed), format=bin (default) | json
GET /api/v1/ne/cells?lon=&lat=[&k=0..3]     res-12 cell of the point + k rings
GET /api/v1/ne/cells?bbox=w,s,e,n[&list=1]  res-12 cells whose CENTRE is in the box (span ≤ 0.05°, ≤ 16 384 cells
                                            else 413 {split:[4 sub-boxes]}); list=1 → {chunks:[res-9…], etags:[…]}
POST /api/v1/ne/cells {"ids":[…]}           ≤ 16 384 ids
GET /api/v1/context?lon=&lat=&include=ne    block ne = /ne/cell of the point (missing.ne when unbuilt)
GET /api/v1/ne/stats[?kg=A,B|gemeinde=|district=|state=]   CHANGE over time (gemeinde/district/state take the GKZ
                                            prefix 5/3/1 digits or a name substring; Wien Gemeindebezirke 901–923 → kg=): change{since, confirmed{cells, d_lu_m2[9],
                                            d_str_cover_m2, d_str_n, transitions_m2{"green>structure":…}}, candidate{…}, by_day{}};
                                            no filter → national. m², additive; confirmed = the new cell statistics were
                                            sighted twice (next pass or a second observer); candidate = once.
GET /api/v1/ne/glyph                        landing-page glyph data: one row per H3 res-7 cell of Austria, fields[] =
                                            [x, y, chunks, passes_sum, reports, age_h, observed, change_cells, kg] ([x, y]
                                            only when not digested yet); kgs[] names, kg_codes[]; w/h = viewBox; 5 min cache.
GET /api/v1/ne/glyph/kgs                    the glyph's search index: every KG of the universe as
                                            [code, name, gemeinde, district, x0, y0, x1, y1, gkz] (bbox in viewBox units; gkz = Statistik
                                            Austria Gemeindekennzahl, Bezirk = gkz[0:3], Land = gkz[0]); 1 h cache.
GET /api/v1/ne/glyph/admin                  outline collections the find box selects like a KG: bl[] Bundesländer (c = 1 digit),
                                            bz[] Bezirke (3), gem[] Gemeinden (5; Statistik Austria, CC BY 4.0), n2k[] Natura 2000
                                            sites (c = SITECODE, t = A birds · B habitats · C both, ha; EEA). Every row {c, n, bbox}
                                            (viewBox units); kinds{} = label + notice per collection; 1 h cache.
GET /api/v1/ne/glyph/admin/outline?c=701    one unit's outline: {c, kind, n, d (SVG path, M…Z per ring, evenodd), bbox, notice};
                                            c = any code of the index (701, 10310, AT1103112); 24 h cache.
GET /api/v1/ne/glyph/tile?lod=0|1|2&i=&j=[&v=obs|tree|chg|dif|rd|sub|subc][&raw=1][&t=] full-resolution PNG tile (lon/lat grid 0.025° / 0.01° / 0.1°): one hexagon
                                            per res-12 cell, dominant group (alpha = dominance), unknown use = neutral;
                                            v=obs → the OBSERVED land use per cell ({OBS_NAME}'s obs_lu, same palette
                                            and dominance rule; cells without an observed row stay transparent),
                                            v=tree → canopy layer over v=obs: every wooded cell in the observed teal, deeper
                                            by the band of its tallest tree (cells without a tree transparent), v=chg → candidate/confirmed
                                            change, aged: fresh = full change hue, a year old = ash (opacity floor .18; the scale
                                            shrinks to the history there is while it is < 1 year, epoch = first change day);
                                            &t= limits it to a window (grammar below; header adds ;win=); header X-NE-Tile n=;lu=9 sums (observed sums under v=obs|tree);unk=;obs=cells painted;trees=Σ tree_n;tmax=tallest
                                            tree (0.5 m);th=5 bands of cells by tallest tree (<5,5–15,15–25,25–35,35+ m);
                                            chg=confirmed,candidate;dlu=9 m².
                                            v=dif (lod 0/1 only, 400 at lod 2) → DIFF: the declared row vs the observed row of the
                                            SAME cell, today — not change. One hue (magenta), the mark is the kind: solid =
                                            structures declared, none in the LiDAR (declared ≥ 20 %, observed cover < 5 %);
                                            chequer = structures in the LiDAR, none declared (observed ≥ 20 %, declared < 5 %,
                                            nDSM rise ≥ 2 m between the two flights); ring = declared forest, currently no trees
                                            (declared forest ≥ ½, observed grass/crop/alpine/building land/other ≥ ½, a Hansen loss
                                            year or an nDSM drop ≤ −2 m, no QA'd tree apex in the cell — a cut or a young stand is
                                            forest by design, the mark says what stands there today). Only cells with status ok
                                            (land ≥ 90 %, ≥ ½ in the KG, declared use known, LiDAR verdict known) are compared;
                                            vocabulary differences (crop vs grass, alpine pasture, roads, water, 24 % of cells) are
                                            never drawn. raw=1 drops the corroboration of the chequer and ring kinds (noisier).
                                            Header adds obs=rows joined;dif=solid,chequer,ring counts;cmp=cells compared;[raw=1;]
                                            als=earliest dominant flight year,latest newest year of the KGs the tile reads;mix=KGs
                                            whose newest flight year covers < 90 % ({OBS_NAME} als{} per KG, refreshed daily).
                                            v=rd (lod 0/1 only, 400 at lod 2) → ROADS: OSM roads and rail (our osm_proximity.db,
                                            the lines /api/v1/osm/geometry serves; ODbL, X-Data-Attribution set) drawn
                                            server-side in one achromatic warm-grey ink — no hue carries meaning, colour-blind
                                            safe — width = rank (major = motorway–secondary, roads = tertiary/residential/
                                            unclassified, service ways dashed, rail with white ties at lod 1), WEIGHT =
                                            remoteness: every road samples a 250 m grid of the distance to the nearest major
                                            road, transit stop and settlement (the remoteness of /api/v1/osm/point without
                                            its road term, 0 central … 100 remote) and its ink runs from half the class
                                            weight in a city to one and a half times in the back country (+ up to .5 px) —
                                            the town mesh stays a light web, the one track far from anything is the darkest
                                            line; rail and the casing are not weighed; reads unchanged in greyscale. A pale
                                            casing under every line so it reads on a dark fill or the photo; anti-aliased,
                                            paletted PNG (32 alpha × 8 ink/casing mixes). No cells are read (n=0). Header adds
                                            rd=lines;rdc=major,roads,service,rail;km=length inside the tile (clipped, tiles add
                                            up);rm=length-weighted mean remoteness of the roads in the box;rmb=road km by band
                                            <25,25–50,50–75,≥75 (rm/rmb absent while the grid is still building, seconds after
                                            a start). 204 + down=1 while the OSM db is not open.
                                            v=sub → INVEKOS subsidy claims ({SUB_NAME}, AMA Schläge, CC BY 4.0): at every lod
                                            every Schlag of the year as drawn (their IVP1 outline bins, 0.5 m simplified), ochre fill
                                            ≈ .47 + edge, hatched = dubious hand-drawn outline, GLÖZ landscape element = edge only;
                                            v=subc → the claims' lineage vs the previous year filled like the change cells but ochre:
                                            new ≈ .64 + bold edge, redrawn (modified/split_merged/partial) ≈ .47 + hairline, gone ≈ .39
                                            hatched + dotted edge, same = nothing. Hexagon fallback when an outline bin is missing
                                            (wash by claimed share / lineage as outlines of runs of cells); lod=2 = the same from the
                                            16 bins of the 0.1° box (f=1 accepted, ignored).
                                            204 + X-NE-Tile down=1 while the peer is down. Meta adds sub=cells;cov=Σ share (1/255);
                                            cls=8 cov-weighted sums (none acker gruenland alm wein dauerkultur gloez sonst);bio=Σ,unknown;
                                            dub=cells with rel<150;chg=same,modified,split_merged,partial,new,gone;dcov=Σ Δshare;
                                            cells=res-12 cells in the box;year=;prev=;rel=release
                                            [;pn=Schläge of the year anchored (first vertex) in the box;pchg=6 counts by lineage kind;
                                            pha=Σ m²;pnew=Σ m² new;pgone=Σ prev m² gone — present iff painted from outlines].
GET /api/v1/ne/glyph/sub                    the subsidy overview: {year, prev_year, release, n, cov[], cls[], bio[], dcov[], chg[],
                                            cls2[], shr[]} in the cells[] order of /ne/glyph (1/255 of the res-7 cell; bio 255 =
                                            unknown; chg = share of the claimed area with a lineage event; cls2 second class, shr =
                                            dominant class share of the claimed area); 24 h; 503 no-store while the peer is down.
                                            Source {SUB_API}/api/v1/invekos/h3?res=7 — the raw per-cell
                                            columns are theirs; we paint and sum, keep nothing on disk.
GET /api/v1/ne/glyph/passes[?kg=]           change over time, by PASS (not by calendar: a confirmed change is dated by the day a
                                            peer re-digested that land, so the pass is the honest unit until a pass is complete):
                                            {scope: national|kg, epoch, passes[]: {k (2 = second sighting …), share of the held
                                            chunks seen k times, first, done (date the share passed 99.5 %) | eta, eta_day,
                                            rate_per_day (exponential tail), confirmed, candidate, d_lu_m2[9], d_str_n, from, to,
                                            by_day{day: {cells, d_str_n, d_lu_m2[9]}}, candidate_by_day{}}, cells[] rows
                                            [cell_index, pass, day_index, confirmed, candidate] + days[] (national only), curve}.
                                            Window grammar (t= on tile/cell, ;t= in the share link): t=2 one pass · t=2..3 passes
                                            · t=2@d1..d2 days of a pass · t=@d1..d2 calendar across passes. 5 min memo.
GET /api/v1/ne/glyph/cell?lon=&lat=|cell=[&t=] the res-12 cell under a point: public cell doc + its hexagon in viewBox units
                                            + pic_obs = what the v=obs tile shows there, in words ({dom, dom_pct (5 % steps),
                                            second?, tree?: {band 0–4, h_m, n}} | {unknown:true} | null = no observed row) — the
                                            pixel, not the row; the decoded observed row is in /ne/cell/{h3}.obs.
                                            &sub=1 adds sub{claimed, claimed_pct, class, bio (true|false|null unknown), reliable,
                                            vs_prev (same|modified|split_merged|partial|new|gone), d_claimed_pct, year, prev_year,
                                            release, schlag?} from the subsidy layer (sub:null + sub_unavailable:true while the peer
                                            is down); schlag = the outline under the cell centre {area_ha, class, bio, reliable,
                                            vs_prev, d_area_ha? (vs best-linked predecessor), prev_area_ha? (gone), clipped?}.
GET /api/v1/ne/filter?bbox=w,s,e,n|kg=&f=[&format=count|cells|h3|geojson]
                                            FILTER cells by their numbers (docs/ne-filter.md). bbox span ≤ 0.1° (16 bins) → 413
                                            above. f = ",“-separated AND groups of "|"-separated OR terms: field=member[+member]
                                            (sets), field>=n · field<=n · field=lo..hi (numbers, physical units), bare field
                                            (flags), field=unk (no value), k>=n (keep only match clusters ≥ n cells); named words
                                            (steep, forest, settlement, protected, changed, …) expand to terms; unknown terms are
                                            reported in ignored[], never an error. count: {cells, match, ha, by_d{group: n},
                                            bins, partial_bins, expr (canonical), words, link (the landing page with ;f=)};
                                            cells: + cells_h3[] (res 12, ≤ 65 536); h3: + h3[] compacted; geojson: FeatureCollection
                                            of hexagons (≤ 4 096). 5 min cache; no-store while a bin is still building.
GET /api/v1/ne/filter/fields                the field catalogue: {fields[]: {id, label, kind (num|set|bits|flag|bucket|sites),
                                            unit, plane (core|ext), dtype, scale, nodata, members[], labels[], edges[], min, max,
                                            source, group}, words{}, presets[], grammar, bin_deg}. 1 h cache.
GET /api/v1/ne/stats/unit?kg=|h3=|bbox=|adm=|pt=|share=[&f=][&format=json|flat|csv][&keys=][&peers=srtm,sub|none][&year=]
                                            ONE ROW PER UNIT (docs: /api/v1/docs/stats-envelope.md): declared/observed/diff/change/
                                            subsidy/protection aggregates of the cell set + the same unit's envelopes from {OBS_NAME}
                                            and {SUB_NAME} ("peers"). Units: kg (≤ 50, one envelope each), h3 res 5–12
                                            (exact descendants), bbox ≤ 0.1°, adm = Gemeinde (5 digits) or Natura 2000 site, pt = the
                                            res-7 cell under a point, share = the landing page's #hash verbatim (kg= h3= adm= pt= f=) —
                                            share link and export are one URL. f= filters the cells (grammar above; unit_cells_n /
                                            unit_area_ha keep the whole unit; peers are then skipped unless peers= asks). figures{} is
                                            flat and scalar: cells_n area_ha declared_ha.<group> observed_ha.<group> diff_ha.<verdict>
                                            change_n/_ha.<none|candidate|confirmed> subsidy_ha.<class> subsidy.claimed_pct natura2000_ha
                                            terrain.* height.* canopy.* kg.dominant gemeinde.* … ; flat = one object with srtm.<key> and
                                            sub.<key> merged in; csv = header + one row per unit (=IMPORTDATA / Power Query); keys= prefix
                                            filter. Same cells as /ne/filter (exact centres and areas); kg= and h3= agree with srtm cell
                                            for cell. ETag, 10 min cache; first call on a cold area builds the bins (seconds).
GET /api/v1/ne/glyph/fidx?i=&j=&p=core|ext|all
                                            the per-bin index the page filters on (bin = 0.025° cell, w = i·0.025, s = j·0.025;
                                            same grid as LOD-0 tiles): application/vnd.ne-fidx, NFX1 — "NFX1" u8 ver u16 ncol
                                            u32 n u32 hlen, JSON header {state, bbox, origin (res-12 H3 of local IJ (0,0)),
                                            a[6|12] (IJ → viewBox polynomial), hex[6] (hexagon offsets), cols[{id,dt}], n2k[]
                                            (site codes for n2ks), unk, res_px}, IJ rows as runs (u16 nrows, per row i32 j,
                                            u16 nruns, per run i32 i0 u16 len), then planar columns in cols[] order (u8 | u16 LE).
                                            ETag "<state>-<plane>"; &e=<state> (from X-NE-Fidx-State / the tile's X-NE-Tile) makes
                                            it immutable. Core ≈ 150 KB gz per urban bin; built on first request (≈ 3 s) from the
                                            NE store + peers (srtm cells, INVEKOS h3, Natura 2000, OSM) and kept on disk.
GET /api/v1/ne/mvt/{z}/{x}/{y}.pbf          the cells as VECTOR TILES for QGIS / MapLibre (docs: /api/v1/docs/qgis-layer.md):
                                            layer "cells", H3 polygons, res 7 at z 9 … res 12 at z 14–15 with every attribute
                                            (declared, observed, diff, change, subsidy, LiDAR, epochs); below res 12 aggregates
                                            (n, area_ha, dominant d/o + pct, diff_pct, chg_*, sub_pct). TileJSON: /api/v1/ne/mvt.json.
                                            z 9–11 come from a national table (instant anywhere, no peer columns); z 12+ from the
                                            area's index: 202 + Retry-After + progress JSON while its bins are being built;
                                            /api/v1/ne/mvt/status?bbox=w,s,e,n[&prepare=1] tells readiness. Gzip on the wire.
GET /api/v1/ne/pmtiles?bbox=|austria|kg=|h3=|adm=|share=[&minz=9&maxz=14]
                                            the same tiles for one area as a PMTiles v3 archive (offline QGIS); bbox=austria&maxz=11
                                            = the national overview (~30 MB). 202 + progress (HTML page for browsers, JSON for
                                            scripts) while building — the URL turns into the download; 413 above 1500 km² / 20 000 tiles at z ≥ 12,
                                            kept 48 h. /api/v1/ne/pmtiles/jobs lists recent archives. Optional ?key= as for stats/unit.
GET /api/v1/ne/glyph/kg?kg=                 the KG's outline at res 12 (union of its majority cells, holes kept) as an SVG
                                            path d in viewBox units + bbox + cells; 404 until the KG is held; 1 h cache.
Landing page GET / — the third way into land-use change, next to /ne/stats (figures) and layers=chg (cells).
  This is the complete inventory of what it shows and the endpoint behind each piece, so a client (e.g. a map
  viewer, a settlement explorer) can show ALL of it itself. HTML for humans; never scrape it — use the JSON named.
  1 Map glyph (data /ne/glyph, 5 min): one dot per H3 res-7 cell, fields [x,y,chunks,passes_sum,reports,age_h,observed,
    change_cells,kg]. Meaning: ink depth = passes_sum/chunks (how often peers digested that land, α = alpha·coverage
    raised to depth); tint = age_h < fresh_hours (6) → fresh report, fading by age; hairline = [x,y] only (never
    digested). hex{} = hexagon outline per cell for seamless faces when zoomed. Palette, one hue per meaning:
    plum = coverage depth, gold = fresh, teal = observed, vermilion = change; blue is UI only; CVD-safe.
  2 Picture = one PROFILE × any OVERLAYS (same data, one picture). Profile (what fills every cell, exactly one):
    "declared" (ink depth) or "observed" (observed > 0 → teal, same depth ramp; layer {OBS_NAME} posts) — a
    radio pair under the figure, the active word takes its colour, a dot-loader trails it while its tiles load,
    key v flips. Overlays (marks on top, each independent, any combination, on either profile): "change"
    (change_cells > 0 → vermilion, darker = more cells, soft halo at country scale; pulse icon, key c), subsidy
    (tractor, key s), trees (key t), relief (key r), photo (lucide earth, key o) — five same-sized icons after the pair, pressed = coloured + underlined, pulsing
    while their tiles load; a toggle never changes the profile and never moves the view. Every layer holds at
    every level of detail it has (dots, hex faces, full-resolution tiles with &v=obs|chg|sub|subc); z-order
    bottom → top: photo (under everything) → profile fill (dimmed to .4 under change, .8 under the subsidy claims) → change → subsidy →
    relief → trees. The card reorders its sources: change first when on, then the profile's source.
  2a Full resolution ("full" button, ;full in the link): the declared product at native res 12 as PNG tiles
    from /ne/glyph/tile — enabled only once the view is zoomed ≥ 8× (the button never moves the view);
    LOD by zoom: lod=2 (0.1° tiles) from 8×, lod=0 (0.025°) from 110×, lod=1 (0.01°) from 300×, up to 900×;
    the res-7 dots/faces step back under it and the pointer picks the res-12 cell via /ne/glyph/cell?lon=&lat=.
    Under full res the selected KG / pinned res-7 cell shows as a white veil (55 %) over everything else
    instead of an outline; for a KG the veil follows its exact res-12 outline (/ne/glyph/kg), not the res-7 hexagons.
    The observed profile at full res paints each cell's observed use (same colours as declared). The in-view legend
    is one block per layer on, in z-order: profile head + use bar, CHANGE (Δ bar, confirmed/candidate, sighting
    period), DIFF, SUBSIDY, relief note, TREES (count, tallest, trees by height band — from the apex tiles, any profile).
    Relief (view-bar icon, ;relief in the link, key r): {OBS_NAME}'s hillshade XYZ tiles
    (GET {OBS_WEB}/tiles/hillshade/{z}/{x}/{y}.png, 25 m DTM, CC BY 4.0) blended faintly over the
    cells under full res, full weight at the coarse tiles and fading out by 300× where the 25 m cells would only show
    their seams; legend adds the source line. Any view, no data of ours.
    Photo (view-bar earth icon, ;ortho[=op] in the link, key o): basemap.at's orthophoto WMTS
    (GET https://mapsneu.wien.gv.at/basemap/bmaporthofoto30cm/normal/google3857/{z}/{y}/{x}.jpeg, CC BY 4.0 'basemap.at',
    fetched by the browser) laid UNDER the base at the near tiles (≥110×, LOD 0/1); the layer's weight is the reader's
    (default .5): a click toggles it and shows the opacity popover for a moment, a hold / right-click / wheel on the icon
    adjusts without toggling; past the half the profile fills step back so the photo reads through. Legend adds the
    source line with the percentage. Any profile, any overlay, no data of ours.
    Roads (view-bar route icon, ;roads[=op] in the link, key w; near tiles only, ≥110× — the icon fades below): OSM
    roads and rail (tile?v=rd above) over the fills, subsidy and change marks, under the relief and the trees — one
    quiet ink, wider = higher rank, DARKER = MORE REMOTE (far from a town, a stop, a main road — the ink weight,
    never a hue, so it reads for every kind of colour vision and in greyscale), dashed = service ways, ties = rail;
    the whole layer weighed by the same opacity popover the photo has (default .7: click toggles + shows it, hold /
    right-click / wheel adjust). Legend adds "roads · n km of ways and rail in view · by rank · OpenStreetMap · op %"
    (km = the tiles on screen, clipped so they add up); the reading of the ink is a one-time tap hint, not a legend
    line.
    Filter (funnel icon after the find box, ;f= in the link, key f shows/hides the row, F = full screen): a chip row
    under the status line — each chip one AND group, the input adds a condition (type words, fields, members:
    suggestions list presets → words → fields → members; click a chip to edit it, × removes, Escape in the row
    clears). Cells that do NOT meet the expression are veiled (page colour .55, .7 under the photo) over the data
    overlays and under relief/trees, matching clusters get a hairline, at the near tiles only (≥110× — the readout
    says "zoom in to filter cells" below). Readout "m of n cells · ha", legend row "filter <words> · m of n · ha"
    (+ "all passes" when a time window is set and the filter uses chg: the filter counts change over all passes,
    the window only shapes the change layer beneath), card row per cell with the cell's values for the terms.
    Grammar = /api/v1/ne/filter (above); unknown terms are not errors — they stay in the input with a hint and
    ignored[]; member labels are accepted (chg=confirmed). The index behind it is /api/v1/ne/glyph/fidx, one
    blob per 0.025° bin, evaluated and painted in a Worker — a chip change repaints from memory, no request.
    Time (the strip under the view bar, on with change; ;t= in the link; keys < > step, space plays): change by
    pass — a hairline track, one dot per pass (size = confirmed cells, fill = share of Austria seen that often,
    dashed while the pass is open, with its ETA), click a pass → its days, the calendar icon → every pass by day /
    week / month / quarter; drag the track to scrub a range. The map ages the change (fresh hue → ash over the
    history there is, up to a year), play walks day by day, the window growing from the first bin with a date label
    riding on the head; pause (the button, space) catches the tiles, card and legend up; the window's figures
    (confirmed / candidate, redeclared ha, structures, lost » gained bar) read under the track. A place with no
    history yet gets the calendar from the epoch. Scope follows the selection (Austria / the KG).
    Diff (git-compare icon, ;diff in the link, key d; near tiles only, ≥110× — the icon fades below): where the
    register and the LiDAR disagree about the same cell, today; distinct from change (the register over time).
    Nothing is drawn where they agree; the profile fill steps back to .4 under the marks like under change.
    Three kinds, one hue, the mark names the kind (tile?v=dif above): solid "structures declared · none in the
    LiDAR", chequer "structures in the LiDAR · none declared", ring "declared forest · currently no trees".
    Its time axis is the LiDAR epoch, shown as an age: legend head "flown 2022 · 4 y ago" (a range "2010–2022"
    when the tiles span KGs of different years). Legend block: the kinds in view with counts, then "n of m cells
    compared · k marked · corroborated only | all cells of these kinds" (that last word toggles raw — ;diff=raw —
    the uncorroborated cells of the chequer and ring kinds) and "n KGs with mixed flight years". Card row DIFF
    (res-12 cells, with the overlay on): "LiDAR sees <group> · register says <group> · flown 2022" — or
    "agree · <group>" or "not compared · <why>"; the open block names the kind with its figures (nDSM ±m between
    the flights, forest loss year, flight years of the KG). Cell doc: /ne/glyph/cell carries pic_dif {status,
    agree, kind, kind_raw, declared{dom,dom_pct}, observed{dom,dom_pct}, dh_m, loss_year, als{kg,newest,oldest,
    dominant_share,mixed,precision}} whenever an observed row exists.
    Subsidy (tractor icon, ;subsidy in the link, key s; with the change overlay on it switches to the vs-previous-year variant):
    INVEKOS claims per cell from {SUB_NAME} (AMA Schläge, CC BY 4.0, aggregated there per H3 cell).
    Without change: ochre dots / hex faces by claimed share wide out, and in full resolution every Schlag as drawn
    (hatched = dubious outline, GLÖZ = edge only), the profile fill steps back; with change: the claims' lineage vs
    the previous year filled in ochre over the vermilion change fills (full = new · half = redrawn · hatched = gone).
    Either profile. Legend block (without change):
    "n % of the land in view claimed (year-release) · bio n % | bio unknown · dubious drawing n % · m Schläge · ha"
    + class bar; (change): "k of m Schläge moved · Δ claimed ±ha" + new (+ha) / gone (−ha) / redrawn / same counts;
    last line the attribution. Cards: one SUBSIDY row — res-12 "claimed n % · class · bio · drawn reliably |
    dubious drawing · vs <year>: same|new|gone|redrawn · Schlag x ha (±Δ)" or "not claimed", res-7 "claimed n % of
    the cell · class n % · class2 n % · bio n %", KG = sum over its res-7 cells. Hover peek = what the picture
    shows there: the Schlag "x ha · class · bio · dubious drawing [· vs <year>: kind ±ha]" when one is under
    the cell, else the cell reading.
    Peer down → "subsidy layer unavailable". Data: /ne/glyph/sub, tile?v=sub|subc, cell?sub=1.
    Hover peek under full res (with a KG or cell selected): one line per source for the cell under the cursor —
    a declared bar + its top groups + structures, under v=obs the observed pixel in words (pic_obs), with ;change
    the change state. The KG is not repeated (the selection names it).
  3 Cell card (hover/tap/arrow keys; ⋯ expands; collapsed = one line per source, sources ordered by the view):
    KG · Gemeinde (kgs[] / kg_codes[] index of /ne/glyph), chunks of 343, reports, passes per chunk as a depth bar
    (pass bands), last report age; a CHANGE section — confirmed cells at once, then async from /ne/stats?kg= the
    KG's Δ per group, candidates, since, digests and the qc line (pipeline/jitter/coverage excluded, clustered/isolated);
    every legend lists all of its bar's groups (first few, the rest behind …); and a live OBSERVED section fetched straight from {OBS_NAME}
    (GET {OBS_WEB}/api/v1/kg/{code}/cells?format=json → summary{cover_ha, cells_observed,
    structures_n, trees_n, terrain} shown at once, then per-tile cells for the hexagon) — lon/lat of a dot =
    lon = ((x−pad)/scale + minx)/kx, lat = −((y−pad)/scale + miny) with proj{} of /ne/glyph.
  Self-check for a client that claims to show land-use change: 1 depth + fresh + undigested, 2 observed and confirmed
    change as separate views, 3 per-cell card with declared AND observed side by side, 6 find a KG, 6/7 highlight a KG
    and a cell and share that exact view, 8–10 the daily digest with pass bands, QC and confirmed m². If one is
    missing, people miss it.
  4 Zoom/pan: pinch, double-tap, ctrl+scroll, drag; keys + − 0 Esc; exact cell pick at 24×. Static SVG works without
    script; prefers-reduced-motion drops the transitions.
  5 Tour (teaser): ~14 cells worth a look now — fresh report, confirmed change, deep passes, spread across the country
    — inlined as data-picks; the cursor visits six, any input stops it. Same scoring is yours from /ne/glyph fields.
  6 Find ("find", key /): one box, five sources merged into one keyboard-navigable list (↓↑ Enter, Tab/→ takes the
    ghost completion, Esc closes): KGs (name / Gemeinde / district / 5-digit code, /ne/glyph/kgs, local, instant);
    Gemeinde · Bezirk · Bundesland · Natura 2000 site (/ne/glyph/admin, local, instant; a kind tag on the row);
    addresses (BEV Adressregister via /addresses/search, ranked by what is on screen — "in view" or the distance —
    140 ms after the last keystroke) and names (DLM toponyms, /toponyms/search?near=). Address rows lead when
    the text carries a house number. Pick a KG or an admin unit → fly to its bbox, outline filled + stroked, white
    veil under full res; pick an address or a name → fly to the point, a dot that stays, the KG under it selected
    (from /ne/glyph/cell?lon&lat) with the same outline. Every landing ends in full resolution when the fit is
    deep enough. "recent" = past picks (localStorage).
  7 Share link / deep link ("link" button copies the URL of what is on screen; opening it restores the view,
    also on hashchange — so an agent explores the page by WRITING the hash, no clicks needed: set location.hash
    or open /#… and every token below is applied live). Grammar, one string, tokens joined by ";":
      selection (first):  #kg=63349   #cell=x,y (pinned at the overview unless ;at=/;z= say otherwise)
                          #h3=<res-12 id>   #q=<name> (first hit of the KG search —
                          name / Gemeinde / district / code; rewritten to #kg= once resolved)
                          #adm=<code> an admin unit or Natura 2000 site (7, 701, 70101, AT1103112)
                          #pt=lon,lat[;pn=<label | sub>] a found point (address / name): dot + the KG under it
                          A bare #kg= / #adm= / #pt= (no ;at= / ;z=) lands like the find box: full resolution
                          whenever the fit is deep enough (;nofull keeps the overview).
      ;v=obs              profile (declared when absent; legacy ;v=chg reads as declared + ;change)
      ;change             change overlay (vermilion marks, any profile)
      ;diff | ;diff=raw   diff overlay (magenta marks where register and LiDAR disagree, any profile, near tiles
                          ≥110×; =raw lifts the corroboration filter)
      ;full               full resolution (res-12 tiles)      ;nofull  no automatic full res at the deepest zoom
      ;trees              every tree as a ring of its crown (srtm apices; any profile; drawn from the near
                          tiles, ≥110×)
      ;relief             hillshade from the 25 m terrain model blended over full res (any profile; full weight
                          at the coarse tiles, gone by 300×)
      ;subsidy            INVEKOS subsidy claims (any zoom, any profile; the vs-previous-year variant with ;change)
      ;ortho[=0.1–1]      basemap.at orthophoto under the near tiles (≥110×, any profile), weight .5 unless given
      ;roads[=0.1–1]      OSM roads + rail over the near tiles (≥110×, any profile, any overlay), weight .7 unless given
      ;f=<expr>           filter: veil the cells not matching <expr> (canonical form of /api/v1/ne/filter's f=; near
                          tiles only; e.g. ;f=d=bau+geb,sl>=3 · ;f=steep,forest · ;f=chg=conf · ;f=d=wald|o=wald,ht>=15)
      ;screen             full screen
      ;z=<zoom>           zoom about the selection's centre, no fly — ≥300 reads single cells (read-only, never
                          written back; e.g. /#q=kohlschwarz;v=obs;full;trees;relief;z=400 and the same with
                          ;v= and ;trees dropped is the declared/observed pair of one spot)
      ;at=x,y,z           centre + zoom (written once zoomed → the exact picture)
      ;open               card expanded        ;below  card docked below the map (floats translucent by default;
                          drag it by its grip or header anywhere — off the map too, double-click the grip snaps
                          it back; the ⤓ button docks it)        ;card=fx,fy  dragged card, map-box fractions
    Toggles that cannot show at the current zoom (trees, diff and photo below 110×, relief past 300×, full below 8×) stay in the
    hash and come back with the zoom; the bar fades their icons meanwhile.
    x,y in viewBox units; #cell snaps
    to the nearest dot, so from lon/lat: x = round((lon·kx − minx)·scale)+pad, y = round((−lat − miny)·scale)+pad.
    Mint these yourself to hand a person the change picture of a place ("confirmed change in KG 63349" →
    /#kg=63349;open; "the trees and the ground of that hill" → /#q=<name>;full;trees;relief;z=400).
  7a Scripted checks (window.neGlyph, for a browser driver): state() {hash, view ('' | 'obs' = the profile), profile,
    change, diff, diffRaw, overlays {change, diff, subsidy, trees, relief, ortho, roads}, z, vb, full, trees, relief,
    ortho, orthoOp, orthoZ, orthoLeft, roads, roadsOp, subsidy, subsidyLoaded, reliefZ, reliefLeft, lod, left, tiles[] {k, v, on, ok, hid, n, m = X-NE-Tile meta}, sel, pinned, pin, loading,
    mem {keep = tile budget from navigator.deviceMemory (32/56/160), tiles, images, shown = <image> painted, evicted, under = other
    LODs hidden below a complete LOD, decQ/decN = off-thread decode queue}};
    settled(ms) → promise once nothing loads; go('#kg=…;full;trees') → sets the hash and resolves with settled();
    card() / legend() / peek() = the card's, the in-view legend's and the hover peek's text; hover(lon,lat),
    pin(lon,lat), profile('declared'|'observed'), view('obs'|''), change(bool), diff(bool[,{raw}]), trees(bool), relief(bool), ortho(bool[, op]), roads(bool[, op]), subsidy(bool),
    set({profile, change, diff, diffRaw, subsidy, trees, relief, ortho, orthoOp, roads, roadsOp}) → settled(), lonlat(x,y), xy(lon,lat), client(lon,lat).
    Soak: import('/api/v1/ne/glyph/soak.js').then(m=>m.run({spots:2})) on the landing page walks every profile × overlay by share
    link and returns one string (PASS|FAIL + one line per failure) — layers complete, overlays shown iff on, JSON ↔ dots ↔
    tiles ↔ cell docs consistent, share links round-trip, and fail-soft under faults (m.offline(hash): fault('offline'|'5xx'|'peer')
    via neGlyph.fault(mode), then pin/view/pan/trees/subsidy — no uncaught error, no busy button left, the card's muted
    OFFLINE row, and the next interaction after the fault heals), and the long-session path (m.stress(kg): a lod-1 walk past
    the tile budget — eviction holds mem.keep, left returns to 0, nothing paints under the LOD in use, no JS error),
    and the view-model matrix (m.matrix(kg): all 2 × 2⁴ profile × overlay combinations plus diff alone / with change /
    trees / relief by share link, each through the
    layer checks — tiles of every layer on complete and none of a layer off, groups shown iff on, bar and legend blocks
    mirror the state, hash round-trips).
    log() = JS errors + fetch counts + dbg trace (evict / under-lod lines); net() = the offline marks.
    A declared-vs-observed check
    of one spot: go('#q=<name>;full;z=400'), legend(); view('obs'), settled(), legend(); hover a
    lon/lat and read peek() — then /ne/glyph/cell?lon=&lat= gives lu[] and pic_obs for the same cell.
  8 Bar chart "Declared layer digested by peers" (figures /ne/stats change.by_day/qc_by_day + /ne coverage; sources
    per report /ne/manifest): one bar per day, fixed window, height = km² of res-10 chunks (~1.5 ha) peers re-digested
    that day, stacked by pass band (1st sight, 2nd, 3rd+ …; hatch fills); flagged days = unusual.
  9 Bar tooltip (hover floats above the bar, docks below on touch/narrow; click/Enter pins, ←/→ walk days, Esc
    releases; <title> text without script): weekday · date · rank chip · DECLARED: km² · KGs · reports · digest shape
    (whole-KG ≈ 1× KG area vs viewport burst ≪) · pass mix as a mini stacked bar · comparable share · that day's change
    with QC (confirmed / candidate / pipeline / jitter / coverage) · OBSERVED: rows srtm posted that day (a different measurement,
    never mixed into the bar) · footer: sources bevdirect@tag ×n · first-sight coverage to date.
  10 Caption line under the chart: chunks ever digested · confirmed change since … (m²) · the report endpoint.
  Nothing on the page is per land unit — all H3-keyed; attribution as above on anything you rebuild.
    layers=chg (json only, /chunk and /cells) → chg{n, cells[]: {status, since, first_seen, last_seen, last_change,
                                            d_lu_m2[9] m², d_str_cover_m2, d_str_n, d_edge_m_ha, from, to}|null per id};
    /cell/{h3} always carries chg; /ne/glyph/cell adds cluster{cells, confirmed, candidate, area_m2, d_lu_m2,
    d_lu_pending_m2, d_str_n, first, last, capped} = the touching res-12 cells with change around it (cap 6000).
    Change = difference of two frozen rows of the same cell between OUR observation dates (never a register date);
    register-derived fields (gk, n_units, K rows) are outside the change series by construction.
    fields=lu,str_cover,… projects json; layers=declared|lu[,obs][,bev] (obs public since 2026-10-26: obs_public in /ne)
    Partial answers (an unbuilt KG touches the area): pending_kgs[], X-NE-Pending-KGs, Cache-Control: no-store.
    pending ≠ "no cells".

Records (little-endian; identical bytes on every tier; shares are 1/255 of CELL area)
  LU row 30 B, res 12 (~307 m²):  cell_id u64 | kg_share u8 | land u8 | cov u8 | flags u8 (bit0 declared_unknown,
     bit1 normalised, bit7 superseded — layers=declared only, set at serve time) | lu u8[9] in group order geb bau acker gruen wald wasser verkehr alpen sonst | str_cover u8 |
     str_n u16 | gk u8 (surveyed-boundary share) | n_units u8 | edge_m_ha u16 | orient_coh u8 (0xFF none) |
     orient_axis u8 (axis/2, 0xFF none)
  K row 29 B, res 10 (~1.5 ha), only where n_units ≥ 5:  cell_id u64 | n_units u16 | unit_p10 u32 | unit_p50 u32 |
     unit_p90 u32 | reg_n u16 | units_per_reg×10 u8 (0xFF none) | str_n u16 | str_p50 u16 (0xFFFF none)
     Public tier: unit_p10/50/90 are class midpoints of 1/8-octave log buckets (≈ 9 % steps); p10/p90 = 0 (json null)
     where n_units < 10.
  OBS row: peer-defined, record_bytes + fields[] in the section header; first 8 B = cell_id.
  Border cells appear in every KG they touch (bytes equal except kg_share) — dedupe by cell_id, take the largest share.

Decode (JS):  const v = new DataView(buf); for (let o = 0; o < buf.byteLength; o += 30) {
                const id = v.getBigUint64(o, true).toString(16), land = v.getUint8(o+9),
                lu = new Uint8Array(buf, o+12, 9), strN = v.getUint16(o+22, true), edge = v.getUint16(o+26, true); }
Decode (py):  struct.iter_unpack("<QBBBB9sBHBBHBB", rows)      K: "<QHIIIHBHH"
zstd chunks:  dictionary = GET /ne/dict/{X-NE-Dict} once; python: zstd.ZstdDecompressor(dict_data=zstd.ZstdCompressionDict(d))
Playbook (map client): per viewport tile GET /ne/cells?bbox=…&list=1 → fetch the /ne/chunk/{c9} you lack (24 h,
  keyed by ETag) → colour res-12 hexes from lu[9] / str_cover / edge_m_ha; flags.declared_unknown → fog.
  Click: /context?lon&lat&include=ne → K record (unit_p50, str_n, orient_axis).
Coverage: {COVERAGE}
  Grows by submission (a KG appears the moment its cells land in the store) — GET /ne → kgs_built; GET /ne/manifest → per-KG status (built | queued + queue_position |
  stalled | failed | not_in_index). Poll with If-None-Match, honour Retry-After, never treat 404/pending as "empty".
Observers: if you assemble cells yourself from the BEV tiles (python package ne_cells, source bevdirect@<tag>, the
  reference implementation — never a port), report digests with a contributor token to
  POST /contrib/api/v1/ne/{kg}/report; the answer lists chunks that changed against the baseline. Tokens on request
  via POST /api/v1/feedback. Protocol: /api/v1/docs/ne.md § Observers / § Epochs.
  Ready-made: pip install git+https://github.com/raffopenssh/vtcseamless-py → "vtcseamless observe --kg <kg> --observer <label>"
  (runs bevdirect-serve for you, builds with the bundled ne_cells, posts the report; reads this API tokenless).
  Change protocol (optional, backwards compatible): (0) header-only report (no chunks{}) → unchanged:true or
  202 {need:"chunks"}; (1) report + chunks_ap{} (ne_cells.change.epoch_report_ap: sha256 of each chunk's rows with
  the register bytes zeroed) → want_chunks[]; (2) POST …/ne/{kg}/chunks?observer= with the NECH body
  (ne_cells.change.pack_chunk_rows) of exactly those chunks → stored, deltas[]; a report without chunks_ap
  keeps today's answer. Peers never send register bytes or K rows.
  Quality control (optional fields, docs/ne-change.md §5a): tiles{expected,fetched} per report — fetched < expected ⇒
  change_suspect:"tiles_incomplete", nothing counted; chunks_tiles{chunk: sha256[:16] over the bytes of the source
  tiles covering it, sorted z/x/y} — a digest that flips while its tile hash stays equal is status "pipeline", a Δ below
  the quantisation floor (every |Δ| < 5 % of the cell, no dominant-class flip, no count change) is "jitter", two
  versions of a chunk holding different cell sets (a truncated fetch, two observers' domains — land does not vanish)
  are "coverage"; none is candidate nor confirmed. /ne/stats.change.qc reports cells_pipeline, cells_jitter,
  cells_coverage and cluster_share; qc_by_day{day: {cells_pipeline, cells_jitter, cells_coverage}} says what was held
  back per day (same key as by_day).
`

// publicNEHTMLSection — the NE block of the public landing page (spliced into
// publicDocsHTML at {NE}; ban-checked like the llm.txt section).
const publicNEHTMLSection = `<h2 id="ne">NE cells — declared land-use statistics per H3 cell</h2>
<p>The one layer that is about the land itself, published as <em>statistics per hexagon</em> instead of objects: for every H3 res-12 cell (~307 m²) the declared land-use split in nine groups (buildings, building land, arable, grassland, forest, water, traffic, alpine, other), structure cover and count, surveyed-boundary share, unit count, boundary metres per hectare and orientation; per res-10 cell (~1.5 ha) a K record with unit-size percentiles and register counts. Tokenless, keyed by H3 cell id, lon/lat or bbox; 30-byte rows, zstd chunks, immutable ETags. The <em>observed</em> layer — LiDAR heights, tree apices, structures, NDVI, forest loss, terrain for the same cells — is served by the sibling <a href="{OBS_API}/">{OBS_NAME}</a> (<code>/api/v1/cells?bbox=</code>, <code>/api/v1/cells/{h3}</code>, <code>/api/v1/kg/{code}/cells</code>; CC BY 4.0, no key). Same H3 ids, so declared and observed zip together client-side on <code>cell_id</code>.</p>
<pre>GET /api/v1/ne
    # pin, limits, coverage (kgs_built), dictionary ids
GET /api/v1/ne/cells?lon=15.0857&amp;lat=47.0665&amp;k=1&amp;format=json
    # the cell under a point + 1 ring
GET /api/v1/ne/cells?bbox=15.08,47.06,15.10,47.08&amp;list=1
    # which res-9 chunks cover a viewport (+ their ETags)
GET /api/v1/ne/chunk/{h3_res9}
    # all rows of one chunk (zstd | bin | json); ETag immutable → cache 24 h
GET /api/v1/ne/cell/{h3}
    # one res-12 cell decoded, with its res-10 K record
GET /api/v1/ne/{kg}
    # whole-KG container (CC BY 4.0 product download)
GET /api/v1/context?lon=&amp;lat=&amp;include=ne
    # the same cell inside the context bundle
GET /api/v1/ne/stats?kg=63330
    # confirmed change over time: m² per group transition, roof area, structures</pre>
<p><strong>Coverage:</strong> {COVERAGE} It grows by submission — a KG appears the moment its cells land in the store, declared layer here, observed layer from {OBS_NAME} (<code>/api/v1/ne/manifest</code> → status per KG; an unbuilt KG answers 404 + <code>Retry-After</code>, a bbox touching one is <em>partial</em>, never "no cells"). Record layouts, decode snippets and the playbook: <a href="/api/v1/docs/llm.txt">llm.txt</a> § NE cells · contract: <a href="/api/v1/docs/ne.md">/api/v1/docs/ne.md</a>. Attribution: © BEV (CC BY 4.0), bearbeitet · observed layer © {OBS_NAME} (CC BY 4.0).</p>
{CONTRIB}`

// nePublicDocText — docs/ne-cells.md in the public vocabulary.
func nePublicDocText() string {
	s := neContractMD
	// operator-only material (build commands, env names, peer-token paths) is not part of the public contract
	if i := strings.Index(s, "\n## Build (operator)"); i >= 0 {
		j := strings.Index(s[i+1:], "\n## ")
		if j < 0 {
			s = s[:i]
		} else {
			s = s[:i] + s[i+1+j:]
		}
	}
	for _, r := range nePublicRenames {
		s = r.re.ReplaceAllString(s, r.to)
	}
	head := "# NE cells — public contract (rendered from the stored contract in public vocabulary)\n\n" +
		"Public vocabulary: **KG** (5-digit code), **unit** (land unit), **register**, **structure**, " +
		"**declared / observed**. Read API (tokenless, H3-keyed): see the `## NE cells` section of " +
		"/api/v1/docs/llm.txt. Observed layer per cell (LiDAR, trees, structures, NDVI, terrain): sibling " +
		"{OBS_API}/ (same H3 ids). Writes (`/obs`, `/report`) need a peer token. Attribution: " + NENotice() + "\n\n---\n\n"
	return peerText(head + s)
}

func serveNEPublicDoc(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	if isPrivateTier(r) {
		_, _ = w.Write([]byte(peerText(neContractMD)))
		return
	}
	_, _ = w.Write([]byte(nePublicDocText()))
}

// nePublicChangeDocText — docs/ne-change.md (the change protocol: header-first
// report, want_chunks, stripped rows, confirmed vs candidate) in the public
// vocabulary. Linked from ne.md; peers asked for it at /api/v1/docs/ne-change.md.
func nePublicChangeDocText() string {
	s := neChangeMD
	for _, r := range nePublicRenames {
		s = r.re.ReplaceAllString(s, r.to)
	}
	return "# NE change — public contract (rendered from the stored design note in public vocabulary)\n\n" +
		"Companion of /api/v1/docs/ne.md. Peers post reports under their own prefix (`/contrib/…`); " +
		"reads (`/ne/stats`, `layers=chg`, `/ne/cell/{h3}` `chg`) are tokenless. Attribution: " + NENotice() + "\n\n---\n\n" + s
}

func serveNEChangeDoc(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	if isPrivateTier(r) {
		_, _ = w.Write([]byte(peerText(neChangeMD)))
		return
	}
	_, _ = w.Write([]byte(peerText(nePublicChangeDocText())))
}

// ── coverage line for the docs ───────────────────────────────────────────────
//
// neCoverageText renders "665 of 7 850 KGs built (8.5 %) …" for llm.txt and the
// landing page. Memoised 5 min via StatsMemo: the docs are fetched often and
// must never touch the store/manifest locks per request. Inputs are the
// manifest cache (TTL 30 s, incremental ReadDir+stat, only changed headers
// parsed — milliseconds) and NEStoreStats (a loop over ~8 000 in-memory
// entries).
//
// Because the compute is cheap, a STALE entry is recomputed synchronously
// (force when age ≥ TTL) rather than stale-while-revalidate: with SWR the
// first page load after hours of idle showed the coverage from the previous
// visit ("as of 20:32" at 05:13, 1 479 KGs instead of 2 535) and only the
// second load was current. A background ticker (neCoverageKeepFresh) refreshes
// every TTL so even the first request normally finds a fresh value.
const neCoverageTTL = 5 * time.Minute

// neObsDay — observed-layer (satellite / LiDAR, {OBS_NAME}) KGs whose
// current layer was built on a UTC day, with their cells. Keyed by the
// layer's built_at, so a KG re-posted later moves to the newer day.
type neObsDay struct {
	KGs, Cells int
}

func neCoverageStats(force bool) map[string]interface{} {
	if age, ok := StatsMemoAge("ne_coverage"); ok && age >= neCoverageTTL {
		force = true
	}
	return StatsMemo("ne_coverage", neCoverageTTL, force, func() map[string]interface{} {
		neManifest.refresh(false)
		neManifest.mu.Lock()
		total, built, obs, cells := len(neManifest.kgs), 0, 0, 0
		obsKGs := make([]string, 0, 2048)
		obsDaily := map[string]neObsDay{}
		for kg, e := range neManifest.entries {
			if e.LU != nil {
				built++
				cells += e.LU.CellsN
			}
			if e.Obs != nil {
				obs++
				obsKGs = append(obsKGs, kg)
				if len(e.Obs.BuiltAt) >= 10 {
					od := obsDaily[e.Obs.BuiltAt[:10]]
					od.KGs++
					od.Cells += e.Obs.CellsN
					obsDaily[e.Obs.BuiltAt[:10]] = od
				}
			}
		}
		scanned := neManifest.scannedAt
		neManifest.mu.Unlock()
		st := NEStoreStats()
		chunks, _ := st["chunk_refs"].(int)
		// Peer observations (reports on disk, memo 60 s) → weekly tracker.
		rs := neScanReportFiles()
		daily, _ := rs["daily"].([]neDayStat)
		ever, _ := rs["chunks_ever"].(map[string]int)
		// Observed layer: "posted here" ≠ "served by the peer" (peer_ne_ready.go).
		// The public counter must say how many a reader can fetch from srtm NOW.
		served, pending, pst, peerOK := peerNEServedSplit(obsKGs)
		// Landing-page glyph (ne_glyph.go): the scan's per-cell aggregate +
		// the observed KGs of the manifest loop above, rendered once per memo.
		if ga, _ := rs["glyph"].(*neGlyphAgg); ga != nil {
			neGlyphBuild(ga, obsKGs, time.Now())
		}
		// Public state only: what is in the store here and on the observed (lidar) side.
		// Build queue, ETA and failure counts are operator information (/k manifest), not public.
		out := map[string]interface{}{
			"kgs_total": total, "built": built, "obs_posted": obs, "chunks": chunks, "cells": cells,
			"as_of": scanned.UTC().Format("2006-01-02 15:04 UTC"),
			"daily": daily, "chunks_ever": ever, "obs_daily": obsDaily,
			"obs_peer_verified": peerOK,
		}
		if peerOK {
			out["obs_served"], out["obs_pending"] = served, pending
			out["obs_peer_as_of"] = pst.CheckedAt.Format("15:04 UTC")
			out["obs_peer_served_reported"] = pst.ServedReported
			out["obs_peer_cells_generation"] = pst.CellsGen
		}
		return out
	})
}

// neCoverageKeepFresh — background refresh of the coverage memo every TTL so
// the landing page / llm.txt / license never pay the (small) recompute and
// never show a stale "as of". Started once from main (server mode only).
func neCoverageKeepFresh() {
	for {
		time.Sleep(neCoverageTTL)
		neCoverageStats(true)
	}
}

func neFmtInt(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "\u202f" + s[i:]
	}
	return s
}

func neCoverageText() string {
	c := neCoverageStats(false)
	total, _ := c["kgs_total"].(int)
	built, _ := c["built"].(int)
	chunks, _ := c["chunks"].(int)
	cells, _ := c["cells"].(int)
	asOf, _ := c["as_of"].(string)
	if total == 0 {
		return "no KG built yet (store empty) — see GET /api/v1/ne."
	}
	pct := 100 * float64(built) / float64(total)
	obsTxt := neCoverageObsText(c)
	return fmt.Sprintf("as of %s: %s of %s KGs with a declared layer (%.1f %%), %s; %s res-12 cells in %s res-9 chunks.",
		asOf, neFmtInt(built), neFmtInt(total), pct, obsTxt, neFmtInt(cells), neFmtInt(chunks))
}

// neSpliceCoverage — fills {COVERAGE} and {CONTRIB} in a docs template.
func neSpliceCoverage(s string) string {
	s = strings.ReplaceAll(s, "{COVERAGE}", neCoverageText())
	s = strings.ReplaceAll(s, "{CONTRIB}", neContribChartHTML())
	return peerText(strings.ReplaceAll(s, "{GLYPH}", neGlyphHTML())) // after {CONTRIB}: its coverage memo builds the glyph
}

// ── observed-by-peers tracker (daily chart on the landing page) ──────────────
//
// Unit = res-10 chunk (~1.5 ha) a peer digested from the BEV tiles, shown as
// km²: honest for viewport-sized reports (a "KG" would be 10–100× too much)
// and additive across observers. Per UTC day: distinct chunks (bar), of which
// re-observed = already digested on an earlier day (dark part — the only area
// on which change can be seen at all). Inline SVG, no script, palette from the
// page's CSS variables (dark mode for free), one <title> per bar for hover,
// plus a text summary line. Last neContribDays days, rows from neCoverageStats.

const neContribDays = 28

func neFmtKm2(chunks int) string {
	km2 := float64(chunks) * neChunkAreaKm2
	switch {
	case km2 == 0:
		return "0 km²"
	case km2 < 10:
		return fmt.Sprintf("%.1f km²", km2)
	default:
		return neFmtInt(int(km2+0.5)) + " km²"
	}
}

// neFmtChange — a change figure names what the reports can actually carry:
// a peer posts ONE digest per res-10 chunk (sha256 of its LU records), so a
// differing digest says "something in this ≈ 1.5 ha hexagon changed", never
// how many m² or which structures. Hence "N chunks (within ≈ X ha)" — the
// chunk count is the measurement, the hectares bound where it lies. Only
// same-layout, non-lossy diffs are counted (coverage + layout rules).
// neFmtPct — sparse shares are not "0.0 %": two decimals below 0.1 %.
func neFmtPct(p float64) string {
	if p < 0.1 {
		return fmt.Sprintf("%.2f %%", p)
	}
	return fmt.Sprintf("%.1f %%", p)
}

func neFmtChange(chunks int) string {
	ha := float64(chunks) * neChunkAreaKm2 * 100
	unit := "chunks"
	if chunks == 1 {
		unit = "chunk"
	}
	switch {
	case ha < 10:
		return fmt.Sprintf("%s %s (within ≈ %.1f ha)", neFmtInt(chunks), unit, ha)
	default:
		return fmt.Sprintf("%s %s (within ≈ %s ha)", neFmtInt(chunks), unit, neFmtInt(int(ha+0.5)))
	}
}

// neContribChartSVG — stacked daily bars, one segment per pass band
// (1st · 2nd · 3rd–4th · 5th–9th · 10th+ observation of a chunk), deepest pass
// at the bottom. Bands differ by tint and by an overlaid hatch pattern so they
// stay apart in greyscale print and in dark mode. cw = column width (viewBox).
func neContribChartSVG(daily []neDayStat) string {
	const W, H, top, bottom, left = 840.0, 176.0, 18.0, 50.0, 0.0
	n := len(daily)
	cw := (W - left) / float64(n)
	maxC := 1
	for _, d := range daily {
		if d.Chunks > maxC {
			maxC = d.Chunks
		}
	}
	plotH := H - top - bottom
	y := func(chunks int) float64 { return plotH * float64(chunks) / float64(maxC) }
	// segment: solid tint + optional hatch overlay for the deeper bands
	seg := func(b *strings.Builder, band int, x, yy, w, h float64) {
		fmt.Fprintf(b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" class="p%d"/>`, x, yy, w, h, band)
		if band >= 2 && h >= 2 { // a hatch under 2 units is invisible: save the bytes
			fmt.Fprintf(b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="url(#ne-h%d)"/>`, x, yy, w, h, band)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="contrib" viewBox="0 0 %.0f %.0f" role="img" aria-label="declared-layer area digested by peers per day, last %d days, by digest pass" xmlns="http://www.w3.org/2000/svg">`, W, H, n)
	// hatch patterns: 3rd–4th diagonal, 5th–9th cross, 10th+ dots — strokes in
	// the page background so they read as texture, not as a second colour.
	b.WriteString(`<defs>` +
		`<pattern id="ne-h2" patternUnits="userSpaceOnUse" width="5" height="5" patternTransform="rotate(45)"><line x1="0" y1="0" x2="0" y2="5" class="hatch"/></pattern>` +
		`<pattern id="ne-h3" patternUnits="userSpaceOnUse" width="5" height="5" patternTransform="rotate(45)"><line x1="0" y1="0" x2="0" y2="5" class="hatch"/><line x1="0" y1="0" x2="5" y2="0" class="hatch"/></pattern>` +
		`<pattern id="ne-h4" patternUnits="userSpaceOnUse" width="4" height="4"><circle cx="2" cy="2" r=".9" class="dot"/></pattern>` +
		`</defs>`)
	// y guide: max value
	fmt.Fprintf(&b, `<line x1="0" y1="%.1f" x2="%.0f" y2="%.1f" class="grid"/><text x="0" y="%.1f" class="lbl">%s / day</text>`,
		top, W, top, top-3, neFmtKm2(maxC))
	fmt.Fprintf(&b, `<line x1="0" y1="%.1f" x2="%.0f" y2="%.1f" class="axis"/>`, H-bottom, W, H-bottom)
	for i, d := range daily {
		x := left + float64(i)*cw
		bw := cw * 0.56
		bx := x + (cw-bw)/2
		title := fmt.Sprintf("%s · %s in %s KG · %d reports", d.Day, neFmtKm2(d.Chunks), neFmtInt(d.KGs), d.Reports)
		for band := 0; band < nePassBandN; band++ {
			if d.Passes[band] > 0 {
				title += fmt.Sprintf(" · %s %s", nePassBandLabel[band], neFmtKm2(d.Passes[band]))
			}
		}
		if d.Chunks == 0 {
			title = d.Day + " · no report"
		} else if mix := neSourceMix(d.Sources); mix != "" {
			title += " · " + mix
		}
		title += neChangeCaption(d.Day)
		sig := neChangeSignal(daily, i)
		if sig.Unusual {
			title += " · land-use change " + neFmtArea(sig.MovedM2)
			if math.IsInf(sig.Ratio, 1) {
				title += " (first in this window)"
			} else {
				title += fmt.Sprintf(" (×%.0f the usual rate)", sig.Ratio)
			}
			title += " — open for details"
		}
		// one focusable group per day; data-day joins it to its detail card
		fmt.Fprintf(&b, `<g class="day%s" data-day="%s" tabindex="0" role="button" aria-label="%s">`, map[bool]string{true: " flag"}[sig.Unusual], d.Day, title)
		fmt.Fprintf(&b, `<title>%s</title>`, title)
		// hover target spans the full column height
		fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" class="hit"/>`, x, top, cw, plotH)
		// selection marker under the axis (visible only on g.sel)
		fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="2.5" class="mark"/>`, bx, H-bottom+1, bw)
		switch {
		case d.Chunks == 0:
			fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="1" class="zero"/>`, bx, H-bottom-1, bw)
		case y(d.Chunks) < 1.5:
			// too thin to split: one sliver in the band of the majority pass
			best := 0
			for band := 1; band < nePassBandN; band++ {
				if d.Passes[band] > d.Passes[best] {
					best = band
				}
			}
			seg(&b, best, bx, H-bottom-1.5, bw, 1.5)
		default:
			// stack deepest pass first (bottom), first observations on top
			yy := H - bottom
			for band := nePassBandN - 1; band >= 0; band-- {
				if d.Passes[band] == 0 {
					continue
				}
				h := y(d.Passes[band])
				yy -= h
				seg(&b, band, bx, yy, bw, h)
			}
		}
		// unusual change: an amber cap on the bar and a chevron under the
		// axis — the cue to open the day; the figures live in the card.
		if sig.Unusual {
			capY := H - bottom - math.Max(y(d.Chunks), 1.5)
			fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="2.5" class="cap"/>`, bx, capY, bw)
			fmt.Fprintf(&b, `<path d="M%.1f %.1fl3.5 -4.5h-7z" class="chev"/>`, x+cw/2, H-bottom+8)
		}
		b.WriteString(`</g>`)
		// value label on the tallest bar
		if d.Chunks == maxC {
			fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" class="lbl val" text-anchor="middle">%s</text>`, x+cw/2, H-bottom-y(d.Chunks)-4, neFmtKm2(d.Chunks))
		}
		// x labels: Mondays, plus the last day when it is not itself a Monday-label neighbour
		if t, err := time.Parse("2006-01-02", d.Day); err == nil {
			wd := int(t.Weekday()+6) % 7 // Monday = 0
			if wd == 0 || (i == n-1 && wd >= 3) {
				anchor := "middle"
				if i == n-1 {
					anchor = "end"
				} else if i == 0 {
					anchor = "start"
				}
				fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="tick"/><text x="%.1f" y="%.1f" class="lbl" text-anchor="%s">%s</text>`,
					x+cw/2, H-bottom, x+cw/2, H-bottom+4, x+cw/2, H-bottom+17, anchor, d.Day[5:])
			}
		}
	}
	// legend: one swatch per pass band, right-aligned under the x labels
	{
		const sw, gap, ly = 11.0, 12.0, H - 11
		// measure from the right: label ≈ 6.4 px per char at 11px mono
		x := W - 4
		for band := nePassBandN - 1; band >= 0; band-- {
			lbl := nePassBandLabel[band]
			lw := float64(len([]rune(lbl)))*6.9 + 2
			x -= lw
			fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" class="lbl">%s</text>`, x, ly+9, lbl)
			x -= sw + 4
			seg(&b, band, x, ly, sw, sw)
			fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" class="swatch"/>`, x, ly, sw, sw)
			x -= gap
		}
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" class="lbl" text-anchor="end">digest of a chunk:</text>`, x, ly+9)
		// left: the flag legend
		fmt.Fprintf(&b, `<rect x="2" y="%.1f" width="%.1f" height="2.5" class="cap"/><path d="M%.1f %.1fl3.5 -4.5h-7z" class="chev"/><text x="%.1f" y="%.1f" class="lbl">unusual land-use change — open the day</text>`,
			ly+4, sw, 2+sw+9, ly+sw-1, 2+sw+18, ly+9)
	}
	b.WriteString(`</svg>`)
	return b.String()
}

// neContribSummaryText — the one text line under the chart (also what a
// text-only reader gets).
func neContribSummaryText(daily []neDayStat, ever map[string]int) string {
	var last7, last7KGs, reports, changed7 int
	kgs := map[bool]int{}
	_ = kgs
	for i := max(0, len(daily)-7); i < len(daily); i++ {
		last7 += daily[i].Chunks
		last7KGs += daily[i].KGs
		reports += daily[i].Reports
		changed7 += daily[i].Changed
	}
	once, twice, deepest := ever["once"], ever["twice_or_more"], ever["deepest_pass"]
	s := fmt.Sprintf("last 7 days: %s in %s reports · ever: %s digested (%.2f %% of Austria), %s on two or more days",
		neFmtKm2(last7), neFmtInt(reports), neFmtKm2(once), 100*float64(once)*neChunkAreaKm2/austriaAreaKm2, neFmtKm2(twice))
	if deepest >= 3 {
		s += fmt.Sprintf(", deepest pass %d", deepest)
	}
	if changed7 > 0 {
		s += " · change detected (7 d): " + neFmtChange(changed7)
	} else if twice > 0 {
		s += " · change detected (7 d): none"
	}
	// rollout tracker: which bevdirect tags the fleet ran in the last 7 days,
	// and whether they form one assembly class (passes comparable across tags)
	week := map[string]int{}
	for i := max(0, len(daily)-7); i < len(daily); i++ {
		for src, n := range daily[i].Sources {
			if strings.Contains(src, "@") { // tagged peer assemblies only — selftests/local builds are not a rollout
				week[src] += n
			}
		}
	}
	if mix := neSourceMix(week); mix != "" {
		s += " · sources (7 d): " + mix
		classes := map[string]bool{}
		for src := range week {
			classes[neBevEquivalent(src)] = true
		}
		if len(week) > 1 {
			if len(classes) == 1 {
				s += " — one assembly class, passes comparable across tags"
			} else {
				s += fmt.Sprintf(" — %d assembly classes, passes counted per class", len(classes))
			}
		}
	}
	return s
}

// neSourceMix — "bevdirect@v0.3.1 ×289, v0.3.2 ×1" (count desc, then tag
// desc so the newest tag wins ties; the common prefix is written once).
// Bar tooltips show every source; the figcaption passes tagged ones only.
func neSourceMix(m map[string]int) string {
	if len(m) == 0 {
		return ""
	}
	type kv struct {
		s string
		n int
	}
	var kvs []kv
	for s, n := range m {
		kvs = append(kvs, kv{s, n})
	}
	sort.Slice(kvs, func(i, j int) bool {
		if kvs[i].n != kvs[j].n {
			return kvs[i].n > kvs[j].n
		}
		return kvs[i].s > kvs[j].s
	})
	var parts []string
	prefix := ""
	for _, e := range kvs {
		name := e.s
		if i := strings.LastIndex(e.s, "@"); i > 0 {
			if p := e.s[:i+1]; p == prefix {
				name = e.s[i+1:]
			} else {
				prefix = p
			}
		} else {
			prefix = ""
		}
		parts = append(parts, fmt.Sprintf("%s ×%d", name, e.n))
	}
	return strings.Join(parts, ", ")
}

// neContribChartHTML — the block spliced into the landing page below Coverage.
func neContribChartHTML() string {
	c := neCoverageStats(false)
	daily, _ := c["daily"].([]neDayStat)
	ever, _ := c["chunks_ever"].(map[string]int)
	obsDaily, _ := c["obs_daily"].(map[string]neObsDay)
	if len(daily) == 0 {
		return `<p><strong>Observed by peers:</strong> no observer report yet — the chart appears with the first <code>POST /contrib/api/v1/ne/{kg}/report</code>.</p>` + "\n"
	}
	// running first-sight total over the FULL history (before trimming), so the
	// card can say how far coverage had grown by that day
	cum := make([]int, len(daily))
	for i, d := range daily {
		if i > 0 {
			cum[i] = cum[i-1]
		}
		cum[i] += d.Passes[0]
	}
	// pad to a fixed window so the chart keeps its shape from day one
	if len(daily) > neContribDays {
		daily = daily[len(daily)-neContribDays:]
		cum = cum[len(cum)-neContribDays:]
	} else if len(daily) < neContribDays {
		t, _ := time.Parse("2006-01-02", daily[0].Day)
		pad := make([]neDayStat, 0, neContribDays-len(daily))
		for i := neContribDays - len(daily); i > 0; i-- {
			pad = append(pad, neDayStat{Day: t.AddDate(0, 0, -i).Format("2006-01-02")})
		}
		daily = append(pad, daily...)
		cum = append(make([]int, len(pad)), cum...)
	}
	return `<p><strong>Declared layer digested by peers</strong> — land area whose <em>declared</em> (survey) cells a peer re-digested from the BEV tiles, per day (res-10 chunks, ~1.5 ha each). Each bar is split by how often that land had been digested up to then — first digest on top, 2nd, 3rd–4th, 5th–9th, 10th+ pass below; only land seen before can show change, and the deeper the pass, the longer its history. The <em>observed</em> layer (satellite &amp; LiDAR, {OBS_NAME}) is a separate measurement: it is not in the bars, each day's card lists it on its own line.</p>
<figure class="contrib">` + neContribChartSVG(daily) + `
` + neContribDetailHTML(daily, cum, obsDaily) + `
<figcaption>` + neContribSummaryText(daily, ever) + neChangeCaption("") + ` · <code>POST /contrib/api/v1/ne/{kg}/report</code></figcaption></figure>
` + neContribScript + "\n"
}

// neChangeCaption — " · confirmed change since …" (ne_change.go) after the
// chunk-count line, which stays: the chunk count is the measurement, the m²
// line is the subset of it that two sightings confirmed (docs/ne-change.md §5).
func neChangeCaption(day string) string {
	if t := neChangeText(day); t != "" {
		return " · " + t
	}
	return ""
}

// neMeanKGAreaKm2 — Austria over its 7 850 KGs; the yardstick that tells a
// whole-KG digest (≈ 1 × per KG) from a viewport burst (≪ 1 ×).
const neMeanKGAreaKm2 = austriaAreaKm2 / 7850

// neContribDetailHTML — one pre-rendered card per day, all hidden, inside a
// floating tip the script positions over the hovered bar (pinned on click).
// Server-rendered so the card uses the same formatting as the chart and
// nothing is re-derived in the browser.
//
// Layout (top → bottom), two sources kept visibly apart:
//
//	header   weekday · date · rank chip
//	DECLARED survey land use the peers digested from the BEV tiles — the
//	         bar itself: km² · KG · reports · digest shape; pass mix as a
//	         mini stacked bar; comparable share; the change block with QC
//	OBSERVED satellite / LiDAR layer posted by {OBS_NAME} that day — a
//	         different measurement, never mixed into the bar or the change
//	footer   sources · first-sight coverage to date
func neContribDetailHTML(daily []neDayStat, cum []int, obsDaily map[string]neObsDay) string {
	var b strings.Builder
	b.WriteString(`<div class="contrib-tip" role="status" aria-live="polite" hidden>`)
	for i, d := range daily {
		t, _ := time.Parse("2006-01-02", d.Day)
		od := obsDaily[d.Day]
		fmt.Fprintf(&b, `<div class="dd" data-day="%s" hidden><div class="dh"><span class="wd">%s</span> <strong>%s</strong>`, d.Day, t.Format("Mon"), d.Day)
		if d.Chunks == 0 && od.KGs == 0 {
			b.WriteString(` <span class="mut">· no report</span></div></div>`)
			continue
		}
		if d.Chunks > 0 {
			if rk := neContribRank(daily, i); rk != "" {
				fmt.Fprintf(&b, ` <span class="rk">%s</span>`, rk)
			}
		}
		b.WriteString(`</div>`)
		// ── declared (survey) ──
		b.WriteString(`<div class="sec dec"><div class="lab"><i class="src"></i>declared <span class="mut">· survey land use · BEV tiles via bevdirect</span></div>`)
		if d.Chunks == 0 {
			b.WriteString(`<div class="kv mut">no digest this day</div></div>`)
		} else {
			fmt.Fprintf(&b, `<div class="kv"><strong>%s</strong> digested <span class="mut">· %s KG · %s report%s%s</span></div>`,
				neFmtKm2(d.Chunks), neFmtInt(d.KGs), neFmtInt(d.Reports), map[bool]string{true: "s"}[d.Reports != 1], neContribShape(d))
			// pass mix: mini stacked bar + legend (percent; km² in the title)
			b.WriteString(`<div class="pb">`)
			for band := 0; band < nePassBandN; band++ {
				if d.Passes[band] == 0 {
					continue
				}
				fmt.Fprintf(&b, `<svg style="width:%.1f%%" aria-hidden="true"><rect width="100%%" height="100%%" class="p%d"/>`, 100*float64(d.Passes[band])/float64(d.Chunks), band)
				if band >= 2 {
					fmt.Fprintf(&b, `<rect width="100%%" height="100%%" fill="url(#ne-h%d)"/>`, band)
				}
				b.WriteString(`</svg>`)
			}
			b.WriteString(`</div><div class="pl">`)
			first := true
			for band := 0; band < nePassBandN; band++ {
				if d.Passes[band] == 0 {
					continue
				}
				if !first {
					b.WriteString(`<span class="mut"> · </span>`)
				}
				first = false
				fmt.Fprintf(&b, `<span class="band" title="%s"><svg class="sw" viewBox="0 0 10 10" aria-hidden="true"><rect width="10" height="10" class="p%d"/>`, neFmtKm2(d.Passes[band]), band)
				if band >= 2 {
					fmt.Fprintf(&b, `<rect width="10" height="10" fill="url(#ne-h%d)"/>`, band)
				}
				fmt.Fprintf(&b, `</svg>%s <span class="mut">%s</span></span>`, nePassBandLabel[band], neFmtPctShort(100*float64(d.Passes[band])/float64(d.Chunks)))
			}
			b.WriteString(`</div>`)
			if re := d.Chunks - d.Passes[0]; re > 0 {
				fmt.Fprintf(&b, `<div class="cmp">seen before <strong>%s</strong> <span class="mut">(%.0f %%) → comparable for change</span></div>`, neFmtKm2(re), 100*float64(re)/float64(d.Chunks))
			} else {
				b.WriteString(`<div class="cmp mut">all first sight — nothing comparable yet</div>`)
			}
			b.WriteString(neContribChangeHTML(daily, i))
			b.WriteString(`</div>`)
		}
		// ── observed ──
		b.WriteString(`<div class="sec obs"><div class="lab"><i class="src"></i>observed <span class="mut">· satellite &amp; LiDAR · {OBS_NAME}</span></div>`)
		if od.KGs == 0 {
			b.WriteString(`<div class="kv mut">no observed layer posted this day</div>`)
		} else {
			fmt.Fprintf(&b, `<div class="kv"><strong>%s</strong> observed layer posted <span class="mut">· %s KG</span></div>`,
				neFmtKm2Cells(od.Cells), neFmtInt(od.KGs))
		}
		b.WriteString(`</div>`)
		// ── footer ──
		var foot []string
		if mix := neSourceMix(d.Sources); mix != "" {
			foot = append(foot, mix)
		}
		if cum[i] > 0 {
			foot = append(foot, fmt.Sprintf("first-sight to date %s · %.1f %% of Austria", neFmtKm2(cum[i]), 100*float64(cum[i])*neChunkAreaKm2/austriaAreaKm2))
		}
		if len(foot) > 0 {
			fmt.Fprintf(&b, `<div class="ds mut">%s</div>`, strings.Join(foot, " · "))
		}
		b.WriteString(`</div>`)
	}
	b.WriteString(`</div>`)
	return b.String()
}

// neFmtPctShort — "62 %", never "0 %" for a present band: "<1 %".
func neFmtPctShort(p float64) string {
	if p > 0 && p < 0.5 {
		return "<1 %"
	}
	return fmt.Sprintf("%.0f %%", p)
}

// neFmtKm2Cells — res-12 cells (~307 m²) as km².
func neFmtKm2Cells(cells int) string {
	km2 := float64(cells) * 307 / 1e6
	switch {
	case km2 == 0:
		return "0 km²"
	case km2 < 10:
		return fmt.Sprintf("%.1f km²", km2)
	default:
		return neFmtInt(int(km2+0.5)) + " km²"
	}
}

// neContribRank — "#6 of 14 days · ≈ 7-day median": the day's place in the
// window and its size against the trailing median of reporting days.
func neContribRank(daily []neDayStat, i int) string {
	d := daily[i]
	var parts []string
	rank, active := 1, 0
	for _, o := range daily {
		if o.Chunks > 0 {
			active++
			if o.Chunks > d.Chunks {
				rank++
			}
		}
	}
	if active > 1 {
		if rank == 1 {
			parts = append(parts, fmt.Sprintf("busiest of %d days", active))
		} else {
			parts = append(parts, fmt.Sprintf("#%d of %d days", rank, active))
		}
	}
	var prev []int
	for j := max(0, i-7); j < i; j++ {
		if daily[j].Chunks > 0 {
			prev = append(prev, daily[j].Chunks)
		}
	}
	if len(prev) >= 2 {
		sort.Ints(prev)
		med := float64(prev[len(prev)/2])
		if len(prev)%2 == 0 {
			med = float64(prev[len(prev)/2-1]+prev[len(prev)/2]) / 2
		}
		r := float64(d.Chunks) / med
		switch {
		case r >= 1.15:
			parts = append(parts, fmt.Sprintf("×%.1f median", r))
		case r <= 0.87:
			parts = append(parts, fmt.Sprintf("%.0f %% of median", 100*r))
		default:
			parts = append(parts, "≈ median")
		}
	}
	return strings.Join(parts, " · ")
}

// neContribShape — " · whole-KG digests" from the area per KG against the
// mean KG size (tells a whole-KG digest from a viewport burst).
func neContribShape(d neDayStat) string {
	if d.KGs == 0 {
		return ""
	}
	perKG := float64(d.Chunks) * neChunkAreaKm2 / float64(d.KGs)
	q := perKG / neMeanKGAreaKm2
	switch {
	case q >= 0.7:
		return " · whole-KG digests"
	case q >= 0.25:
		return " · partial-KG digests"
	}
	return " · viewport bursts"
}

// ── change in the card ───────────────────────────────────────────────────────

// neChangeSig — the day's land-use change rate against the window's usual
// rate. Rate = m² of land use that changed group (paired from→to transitions
// of the cell deltas whose new version arrived that day, confirmed + candidate)
// per km² the day could compare (re-observed chunks), so a big first-sight day
// does not dilute it. Usual = median over the other days of the window that
// compared anything. Unusual = ≥ 3× the usual with at least 1 ha moved, or
// any confirmed delta on the day. Candidate figures are shown but labelled —
// the public totals in the caption stay confirmed-only (docs/ne-change.md §5).
type neChangeSig struct {
	Rate, Usual, Ratio   float64 // ha moved per km² compared
	Compared             int     // chunks
	MovedM2              int     // land use that changed group, m² (conf + cand)
	Unusual              bool
	Confirmed, Candidate *neChangeTotals
}

// neChangeNationalFn — indirection so tests can inject a day's deltas.
var neChangeNationalFn = neChangeNational

func neChangeDayTotals(day string) (conf, cand *neChangeTotals) {
	nat := neChangeNationalFn(false)
	if bd, ok := nat["by_day"].(map[string]*neChangeTotals); ok {
		if t := bd[day]; t != nil && t.Cells > 0 {
			conf = t
		}
	}
	if bd, ok := nat["candidate_by_day"].(map[string]*neChangeTotals); ok {
		if t := bd[day]; t != nil && t.Cells > 0 {
			cand = t
		}
	}
	return
}

func neChangeDayRate(d neDayStat) (rate float64, compared, moved int, conf, cand *neChangeTotals) {
	compared = d.Chunks - d.Passes[0]
	conf, cand = neChangeDayTotals(d.Day)
	if conf != nil {
		moved += conf.neLUMoved()
	}
	if cand != nil {
		moved += cand.neLUMoved()
	}
	if compared > 0 {
		rate = float64(moved) / 1e4 / (float64(compared) * neChunkAreaKm2)
	}
	return
}

func neChangeSignal(daily []neDayStat, i int) neChangeSig {
	var sig neChangeSig
	sig.Rate, sig.Compared, sig.MovedM2, sig.Confirmed, sig.Candidate = neChangeDayRate(daily[i])
	var rates []float64
	for j, o := range daily {
		if j == i {
			continue
		}
		if r, re, _, _, _ := neChangeDayRate(o); re > 0 {
			rates = append(rates, r)
		}
	}
	if len(rates) > 0 {
		sort.Float64s(rates)
		sig.Usual = rates[len(rates)/2]
		if len(rates)%2 == 0 {
			sig.Usual = (rates[len(rates)/2-1] + rates[len(rates)/2]) / 2
		}
	}
	switch {
	case sig.Rate == 0:
	case sig.Usual == 0:
		sig.Ratio = math.Inf(1)
	default:
		sig.Ratio = sig.Rate / sig.Usual
	}
	sig.Unusual = sig.Confirmed != nil || (sig.MovedM2 >= 10000 && len(rates) >= 2 && sig.Ratio >= 3)
	return sig
}

// neFmtRate — change per km² compared, unit auto-picked: m²/km² below
// 0.1 ha/km² (so a tiny but real rate never reads "0.000"), ha/km² above.
func neFmtRate(haPerKm2 float64) string {
	switch {
	case haPerKm2 == 0:
		return "0"
	case haPerKm2 < 0.1:
		return fmt.Sprintf("%.0f m² per km²", haPerKm2*1e4)
	case haPerKm2 < 10:
		return fmt.Sprintf("%.2f ha per km²", haPerKm2)
	}
	return fmt.Sprintf("%.0f ha per km²", haPerKm2)
}

// neChangeStackHTML — one status block: a chip (confirmed / candidate), the
// cell count and the area that changed group, a stacked bar with one segment
// per from→to transition (largest first, ≤ 5 then "other"), its legend, and
// the building-outline line (count, roof m²) — labelled apart from the
// transitions because it comes from a different layer.
func neChangeStackHTML(t *neChangeTotals, cand bool) string {
	type kv struct {
		k string
		v int
	}
	tr := make([]kv, 0, len(t.Transitions))
	for k, v := range t.Transitions {
		tr = append(tr, kv{k, v})
	}
	sort.Slice(tr, func(a, b int) bool { return tr[a].v > tr[b].v || (tr[a].v == tr[b].v && tr[a].k < tr[b].k) })
	if len(tr) > 5 {
		other := 0
		for _, x := range tr[5:] {
			other += x.v
		}
		tr = append(tr[:5], kv{"other", other})
	}
	total := t.neLUMoved()
	var b strings.Builder
	cls := "stk"
	if cand {
		cls += " c"
	}
	fmt.Fprintf(&b, `<div class="%s"><div class="sh">%s <span class="mut">· %s cell%s</span>`, cls, neChangeChip(cand), neFmtInt(t.Cells), map[bool]string{true: "s"}[t.Cells != 1])
	if total > 0 {
		fmt.Fprintf(&b, ` · <strong>%s</strong> <span class="mut" title="area whose declared land use moved from one group to another">changed declared use</span>`, neFmtArea(total))
	}
	b.WriteString(`</div>`)
	if total > 0 {
		b.WriteString(`<div class="sb">`)
		for i, x := range tr {
			fmt.Fprintf(&b, `<i class="s%d" style="width:%.1f%%" title="%s"></i>`, i, 100*float64(x.v)/float64(total), neTransitionLabel(x.k)+" "+neFmtArea(x.v))
		}
		b.WriteString(`</div><div class="sl">`)
		for i, x := range tr {
			if i > 0 {
				b.WriteString(`<span class="mut"> · </span>`)
			}
			fmt.Fprintf(&b, `<span class="band" title="%s"><i class="sw s%d"></i>%s <span class="mut">%s</span></span>`, neTransitionTitle(x.k), i, neTransitionLabel(x.k), neFmtArea(x.v))
		}
		b.WriteString(`</div>`)
	}
	if t.DCover != 0 || t.DN != 0 {
		fmt.Fprintf(&b, `<div class="ss"><span class="mut" title="%s">building outlines</span> `, neOutlineTitle)
		parts := []string{}
		switch {
		case t.DN > 0:
			parts = append(parts, fmt.Sprintf("<strong>+%d</strong> new", t.DN))
		case t.DN < 0:
			parts = append(parts, fmt.Sprintf("<strong>%d</strong> removed", t.DN))
		}
		if t.DCover != 0 {
			parts = append(parts, "roof "+neFmtAreaSigned(t.DCover))
		}
		b.WriteString(strings.Join(parts, " · ") + `</div>`)
	}
	b.WriteString(`</div>`)
	return b.String()
}

// neChangeChip — the status chip. Confirmed = the same delta sighted twice;
// candidate = one sighting so far (listed, never in the public totals).
func neChangeChip(cand bool) string {
	if cand {
		return `<span class="chip cand" title="one sighting so far — awaiting a second sighting before it counts">candidate · 1 sighting</span>`
	}
	return `<span class="chip conf" title="the same delta sighted twice">confirmed ✓ 2 sightings</span>`
}

// neChangeQCDay — pipeline / jitter / coverage exclusions whose new version arrived that day.
func neChangeQCDay(day string) (pipeline, jitter, coverage int) {
	nat := neChangeNationalFn(false)
	if bd, ok := nat["qc_by_day"].(map[string]*neQCDay); ok {
		if q := bd[day]; q != nil {
			return q.Pipeline, q.Jitter, q.Coverage
		}
	}
	return 0, 0, 0
}

// neChangeQCHTML — "QC held back · 12 cells jitter · 3 cells pipeline": what
// the quality gate excluded that day before anything was counted. Empty when
// nothing was excluded.
func neChangeQCHTML(day string) string {
	p, j, cv := neChangeQCDay(day)
	if p == 0 && j == 0 && cv == 0 {
		return ""
	}
	parts := []string{}
	if j > 0 {
		parts = append(parts, fmt.Sprintf(`<span title="below the quantisation floor: no class flip, no outline count change, every Δ under %.0f %% of the cell">%s cell%s jitter</span>`, 100*neQCFloorShare, neFmtInt(j), map[bool]string{true: "s"}[j != 1]))
	}
	if p > 0 {
		parts = append(parts, fmt.Sprintf(`<span title="same source tiles on both sides: the build changed, not the land">%s cell%s pipeline</span>`, neFmtInt(p), map[bool]string{true: "s"}[p != 1]))
	}
	if cv > 0 {
		parts = append(parts, fmt.Sprintf(`<span title="the two digests hold different cell sets: a truncated fetch or two observers' domains, not land">%s cell%s coverage</span>`, neFmtInt(cv), map[bool]string{true: "s"}[cv != 1]))
	}
	return `<div class="qc"><span class="chip qc">QC</span> held back <span class="mut">· </span>` + strings.Join(parts, `<span class="mut"> · </span>`) + `</div>`
}

// neContribChangeHTML — the change section of a day's declared-layer block. States:
// nothing comparable · compared, nothing differed · digests differed but no
// land use moved (register-only) · land use changed. The last is drawn as
// bars: the headline (area moved, status chip, chunk count, unusual tag),
// the rate against the usual rate when there is one, one stacked bar per
// status (confirmed in colour, candidate muted) of the area that changed
// group, split by from→to transition, the structure line, and the QC line.
func neContribChangeHTML(daily []neDayStat, i int) string {
	d := daily[i]
	if d.Chunks == 0 {
		return ""
	}
	sig := neChangeSignal(daily, i)
	qc := neChangeQCHTML(d.Day)
	switch {
	case sig.Compared == 0:
		return `<div class="dc none">no change possible — all first sight</div>`
	case d.Changed == 0:
		return fmt.Sprintf(`<div class="dc none">no change detected in %s compared</div>`, neFmtKm2(sig.Compared))
	case sig.Confirmed == nil && sig.Candidate == nil:
		if qc != "" {
			return fmt.Sprintf(`<div class="dc none">%s differed — nothing passed QC%s</div>`, neFmtChange(d.Changed), qc)
		}
		return fmt.Sprintf(`<div class="dc none">%s differed — cell deltas not derived yet (log pending)</div>`, neFmtChange(d.Changed))
	case sig.MovedM2 == 0 && (sig.Confirmed == nil || sig.Confirmed.DN == 0 && sig.Confirmed.DCover == 0) && (sig.Candidate == nil || sig.Candidate.DN == 0 && sig.Candidate.DCover == 0):
		return fmt.Sprintf(`<div class="dc none">%s differed — register rows only, no land use moved%s</div>`, neFmtChange(d.Changed), qc)
	}
	var b strings.Builder
	cls := "cand"
	if sig.Confirmed != nil {
		cls = "conf"
	}
	fmt.Fprintf(&b, `<div class="dc %s"><div class="dt"><strong>land use changed</strong> on %s <span class="mut" title="%s">· in %s chunks</span>`, cls, neFmtArea(sig.MovedM2), neFmtChange(d.Changed), neFmtInt(d.Changed))
	switch {
	case !sig.Unusual:
	case math.IsInf(sig.Ratio, 1):
		b.WriteString(` <span class="tag">first in window</span>`)
	case sig.Ratio >= 100:
		b.WriteString(` <span class="tag">≫ usual</span>`)
	default:
		fmt.Fprintf(&b, ` <span class="tag">×%.0f usual</span>`, sig.Ratio)
	}
	b.WriteString(`</div>`)
	if sig.Rate > 0 || sig.Usual > 0 {
		fmt.Fprintf(&b, `<div class="st">rate %s compared · usual %s</div>`, neFmtRate(sig.Rate), neFmtRate(sig.Usual))
	}
	if sig.Confirmed != nil {
		b.WriteString(neChangeStackHTML(sig.Confirmed, false))
	}
	if sig.Candidate != nil {
		b.WriteString(neChangeStackHTML(sig.Candidate, true))
	}
	b.WriteString(qc)
	b.WriteString(`</div>`)
	return b.String()
}

func neAbs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// neContribScript — progressive enhancement of the chart: hover floats the
// day's card above its bar when it fits, else docks it below the chart (never
// over the bars; always docked on touch/narrow screens), click/Enter pins it (click again or Esc releases),
// arrow keys walk the days while pinned. Without script the <title> tooltips
// remain; with it they are dropped (the card replaces them). Nothing is shown
// until the pointer or focus reaches a bar.
const neContribScript = `<script>(function(){
var fig=document.querySelector('figure.contrib');if(!fig)return;
var svg=fig.querySelector('svg.contrib'),tip=fig.querySelector('.contrib-tip');if(!svg||!tip)return;
var days=[].slice.call(svg.querySelectorAll('g.day')),cards={},byDay={};
[].forEach.call(tip.querySelectorAll('.dd'),function(c){cards[c.dataset.day]=c});
days.forEach(function(g){byDay[g.dataset.day]=g;var t=g.querySelector('title');if(t)g.removeChild(t)});
var pinned=null,shown=null,hideT=0;
function place(g){tip.classList.remove('dock');if(getComputedStyle(tip).position!=='absolute'){tip.style.transform='';return}
 var fr=fig.getBoundingClientRect(),hit=g.querySelector('.hit').getBoundingClientRect(),top=hit.bottom;
 [].forEach.call(g.querySelectorAll('rect'),function(r){if(r.classList.contains('hit')||r.classList.contains('mark'))return;var b=r.getBoundingClientRect();if(b.top<top)top=b.top});
 var w=tip.offsetWidth,h=tip.offsetHeight,x=hit.left+hit.width/2-fr.left-w/2,y=top-fr.top-h-8;
 if(y<0){tip.classList.add('dock');tip.style.transform='';return}
 x=Math.max(0,Math.min(x,fr.width-w));
 tip.style.transform='translate('+Math.round(x)+'px,'+Math.round(y)+'px)'}
function show(day){clearTimeout(hideT);var g=byDay[day];if(!g)return;
 if(shown&&cards[shown])cards[shown].hidden=true;shown=day;if(cards[day])cards[day].hidden=false;
 var fresh=tip.hidden;tip.hidden=false;if(fresh)tip.classList.remove('on');place(g);
 if(fresh){tip.style.transition='none';void tip.offsetWidth;tip.style.transition=''}
 tip.classList.add('on');svg.classList.add('focus');days.forEach(function(x){x.classList.toggle('hov',x===g)})}
function hide(){if(pinned){show(pinned);return}days.forEach(function(x){x.classList.remove('hov')});tip.classList.remove('on');svg.classList.remove('focus');
 hideT=setTimeout(function(){tip.hidden=true;shown=null},180)}
function pin(day){pinned=day;days.forEach(function(g){var on=g.dataset.day===day;g.classList.toggle('sel',on);g.setAttribute('aria-pressed',on?'true':'false')});if(day)show(day);else hide()}
svg.classList.add('js');
days.forEach(function(g,i){
 g.addEventListener('mouseenter',function(){show(g.dataset.day)});
 g.addEventListener('focus',function(){show(g.dataset.day)});
 g.addEventListener('blur',function(){if(!pinned)hide()});
 g.addEventListener('click',function(e){e.preventDefault();pin(pinned===g.dataset.day?null:g.dataset.day);g.focus({preventScroll:true})});
 g.addEventListener('keydown',function(e){var k=e.key;
  if(k==='Enter'||k===' '){e.preventDefault();pin(pinned===g.dataset.day?null:g.dataset.day)}
  else if(k==='ArrowLeft'&&i>0){e.preventDefault();days[i-1].focus();if(pinned)pin(days[i-1].dataset.day)}
  else if(k==='ArrowRight'&&i<days.length-1){e.preventDefault();days[i+1].focus();if(pinned)pin(days[i+1].dataset.day)}
  else if(k==='Escape'){e.preventDefault();pin(null)}});
});
svg.addEventListener('mouseleave',hide);
document.addEventListener('keydown',function(e){if(e.key==='Escape'&&pinned)pin(null)});
document.addEventListener('click',function(e){if(pinned&&!fig.contains(e.target))pin(null)});
window.addEventListener('resize',function(){if(shown)place(byDay[shown])});
})();</script>`

// neCoverageObsText — the observed-layer clause of the coverage line (see peer_ne_ready.go).
func neCoverageObsText(c map[string]interface{}) string { return peerText(neCoverageObsText0(c)) }

func neCoverageObsText0(c map[string]interface{}) string {
	// Observed layer wording: the number a reader can rely on is what the peer
	// SERVES (its ne_ready), not what it has posted here. Pending = poll later.
	obs, _ := c["obs_posted"].(int)
	obsTxt := fmt.Sprintf("%s with an observed layer posted by {OBS_NAME} (publication state there not verified yet)", neFmtInt(obs))
	if ok, _ := c["obs_peer_verified"].(bool); ok {
		served, _ := c["obs_served"].(int)
		pending, _ := c["obs_pending"].(int)
		peerAsOf, _ := c["obs_peer_as_of"].(string)
		obsTxt = fmt.Sprintf("%s with an observed layer live at {OBS_NAME}", neFmtInt(served))
		if pending > 0 {
			obsTxt += fmt.Sprintf(" (%s more already shared with us, awaiting publication there — poll, not a gap)", neFmtInt(pending))
		}
		obsTxt += fmt.Sprintf(" [peer state %s]", peerAsOf)
	}
	return obsTxt
}
