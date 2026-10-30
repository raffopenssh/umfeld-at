/* ne_glyph_soak.js — in-page soak test of the landing glyph (served at /api/v1/ne/glyph/soak.js).
   Runs INSIDE the landing page (needs window.neGlyph). Drives states through share links (the hash
   grammar of llm.txt §7), checks after every step that exactly the right layers are loaded and
   complete (no stuck tile, no stale overlay shown), that server JSON / tiles / cell docs agree with
   each other and with the page, and that a share link restores the state it was minted from.
   Output is ONE short string: a summary line + one line per failure (+ `verbose` lists every ok).

   Invoke from a browser driver on http://localhost:8000/ :
     (await import('/api/v1/ne/glyph/soak.js')).run({rounds:1,spots:2})
   Options: rounds (default 1), spots (KGs per round, default 2), seed (deterministic pick),
            verbose (list ok checks), settle (ms per settle, 20000), relief/subsidy/trees/ortho/roads (bool, default true),
            kgs (explicit KG codes), filter:false skips the filter step. Returns a string; also window.neGlyphSoak.last = full result object.
   Cheaper single calls (same module, docs/ne-glyph.md §soak):
     check('#change;t=2') / check('#kg=63330;change;t=@2026-10-09..2026-10-20;full')  the time window (ne-change.md §6e): strip on, window applied, chg tiles keyed by it
     check('#kg=63330;change;full;relief;z=200')  one state: go → settle → every layer check + JS errors + non-200 fetches, one string
     matrix(kg?, o?)                              every profile × overlay combination (2 × 2⁴ at lod 1, the change/subsidy combos at lod 2
                                                  and 0) by share link through the same layer checks; one line per failing combo
     tile('2/123/478/chg')                        one tile: status, meta, size, sha256 (also {lon,lat,lod,v})
     snap('a') / snap('a', specs)                 store the tiles on screen (or specs) under a name (Cache API, survives reload + restart)
     diff('a') / diff('a', specs)                 re-fetch and compare with the stored ones: status, meta keys, bytes, changed pixels
     stress(kg?, o?)                              long-session path: lod-1 walk past the tile budget (HI_KEEP), eviction, under-lod hiding,
                                                  decode queue, left=0, no JS error. run() includes it unless {stress:false}.
     offline(hash?, o?)                           fail-soft: injects faults via neGlyph.fault ('offline' = every fetch rejects, '5xx' = every
                                                  fetch answers 503, 'peer' = only cross-origin peers 503), interacts (pin, view, pan, trees,
                                                  subsidy), and checks: no uncaught error, no busy button left, the card shows the muted
                                                  offline row, left=0, then lifts the fault and checks the next interaction recovers
                                                  (tiles complete, no offline row). run() includes it unless {offline:false}. */
export async function run(o) {
  o = o || {};
  const G = window.neGlyph;
  if (!G) return 'FAIL no window.neGlyph — run on the landing page /';
  const R = { t0: Date.now(), ok: 0, fail: 0, lines: [], ms: {}, warn: [] };
  const seed = o.seed == null ? (Date.now() % 100000) : o.seed;
  const rnd = mulberry(seed);
  const SET = o.settle || 20000;
  let D = null, subUp = false;
  const want = (k, cond, detail) => { if (cond) { R.ok++; if (o.verbose) R.lines.push('ok ' + k); } else { R.fail++; R.lines.push('FAIL ' + k + (detail != null ? ': ' + str(detail) : '')); } return !!cond; };
  const lat = (k, ms) => { (R.ms[k] = R.ms[k] || []).push(ms); };
  const fetchJ = async (u, k) => { const t = performance.now(); const r = await fetch(u, { credentials: 'omit', cache: 'no-cache' }); lat(k || 'json', performance.now() - t); let j = null; try { j = await r.json(); } catch (e) { } return { s: r.status, j, h: r.headers }; };
  const fetchTile = async (lod, i, j, v) => {
    const t = performance.now(); const r = await fetch(`/api/v1/ne/glyph/tile?lod=${lod}&i=${i}&j=${j}${v ? '&v=' + v : ''}`, { credentials: 'omit' }); lat('tile', performance.now() - t);
    const m = parseMeta(r.headers.get('X-NE-Tile') || ''); let png = false, w = 0, h = 0;
    if (r.status === 200) { const b = new Uint8Array(await r.arrayBuffer()); png = b.length > 24 && b[0] === 0x89 && b[1] === 0x50 && b[2] === 0x4e && b[3] === 0x47; if (png) { w = (b[16] << 24 | b[17] << 16 | b[18] << 8 | b[19]) >>> 0; h = (b[20] << 24 | b[21] << 16 | b[22] << 8 | b[23]) >>> 0; } }
    return { s: r.status, m, png, w, h, ct: r.headers.get('Content-Type') };
  };
  const settle = async (extra) => {
    const t = performance.now(); await sleep(200); let s = await G.settled(SET);
    // settled() only waits for tiles; relief and subsidy have their own queues
    const until = Date.now() + SET; while ((s.reliefLeft > 0 || s.orthoLeft > 0 || (s.subsidy && !s.subsidyLoaded && subUp)) && Date.now() < until) { await sleep(150); s = G.state(); }
    if (extra) await sleep(extra);
    lat('settle', performance.now() - t); return G.state();
  };
  const go = async (h) => { const t = performance.now(); await G.go(h); const s = await settle(); lat('go', performance.now() - t); return s; };

  /* ── 1. server side: overview JSON, KG index, DOM dots ─────────────────────────── */
  const gj = await fetchJ('/api/v1/ne/glyph', 'glyph');
  if (!want('glyph.200', gj.s === 200 && gj.j && gj.j.cells, gj.s)) return finish();
  D = gj.j; const C = D.cells;
  want('glyph.fields', JSON.stringify(D.fields) === '["x","y","chunks","passes_sum","reports","age_h","observed","change_cells","kg","bands","candidate_cells"]', D.fields);
  /* rows: [x,y] alone = an unheld cell; 10 fields when held; an 11th (candidate_cells) only when > 0 */
  const held = C.filter(c => c[2] > 0).length, unheld = C.filter(c => !(c[2] > 0)).length, obsN = C.filter(c => c[6]).length, chgCells = C.filter(c => c[7] > 0).length, candCells = C.filter(c => c[10] > 0).length, candOnly = C.filter(c => c[10] > 0 && !(c[7] > 0)).length, freshN = C.filter(c => c[2] > 0 && c[5] >= 0 && c[5] < D.fresh_hours).length;
  want('glyph.row shapes', C.every(c => c.length === 2 || c.length === 10 || (c.length === 11 && c[10] > 0)));
  want('glyph.held=cells(chunks>0)', held === D.held, [held, D.held]);
  want('glyph.observed=cells(obs)', obsN === D.observed, [obsN, D.observed]);
  want('glyph.change=cells(change_cells>0)', chgCells === D.change, [chgCells, D.change]);
  want('glyph.candidate=cells(cand>0)', candCells === D.candidate, [candCells, D.candidate]);
  want('glyph.cells in frame', C.every(c => c[0] >= 0 && c[0] <= D.w && c[1] >= 0 && c[1] <= D.h));
  want('glyph.kg idx in range', C.every(c => c.length === 2 || c[8] === -1 || (c[8] >= 0 && c[8] < D.kg_codes.length)));
  want('glyph.kg_codes 5 digits', D.kg_codes.every(k => /^\d{5}$/.test(k)));
  const dots = sel => Array.from(document.querySelectorAll('svg.glyph .dt ' + sel)).reduce((a, p) => a + ((p.getAttribute('d') || '').match(/M/g) || []).length, 0);
  /* the SVG dots were inlined when the page loaded; the JSON is live — compare only when both come from the same build */
  const pageAsOf = (document.body.innerText.match(/as of (\d{4}-\d\d-\d\d \d\d:\d\d UTC)/) || [])[1];
  const sameBuild = !pageAsOf || pageAsOf === D.as_of; if (!sameBuild) R.warn.push(`page as_of ${pageAsOf} ≠ json ${D.as_of} — dom/json dot checks skipped (reload to compare)`);
  const wantDom = (k, c, d) => sameBuild ? want(k, c, d) : null;
  /* dot classes: .e empty · .h held (ink) · .n fresh (a second dot on a held cell) · .o observed · .c change · .ca candidate-only · .ch halo under both */
  const dHeld = dots('.h'), dFresh = dots('.n'), dEmpty = dots('.e'), dObs = dots('.o'), dChg = dots('.c'), dCand = dots('.ca'), dHalo = dots('.ch');
  wantDom('dom.dots held=json', dHeld === held, [dHeld, held]);
  wantDom('dom.dots fresh≈json', Math.abs(dFresh - freshN) <= Math.max(3, freshN * 0.05), [dFresh, freshN]); /* age_h is rounded in the JSON */
  wantDom('dom.dots halo=change∪cand', dHalo === chgCells + candOnly, [dHalo, chgCells + candOnly]);
  wantDom('dom.dots empty=json', dEmpty === unheld, [dEmpty, unheld]);
  wantDom('dom.dots observed=json', dObs === obsN, [dObs, obsN]);
  wantDom('dom.dots change cells=json', dChg === chgCells, [dChg, chgCells]);
  wantDom('dom.dots candidate-only cells=json', dCand === candOnly, [dCand, candOnly]);
  const kj = await fetchJ('/api/v1/ne/glyph/kgs', 'kgs');
  const K = kj.j && kj.j.kgs || [];
  want('kgs.7850', kj.s === 200 && K.length === 7850, [kj.s, K.length]);
  want('kgs.rows', K.every(r => /^\d{5}$/.test(r[0]) && r[4] <= r[6] && r[5] <= r[7] && r[4] >= 0 && r[7] <= D.h + 1 && r[6] <= D.w + 1));
  const kgIdx = new Map(K.map(r => [r[0], r]));
  want('kgs⊇glyph.kg_codes', K.length === 0 || D.kg_codes.every(k => kgIdx.has(k)));
  /* bad input must be rejected, never 500 */
  for (const [u, exp] of [['/api/v1/ne/glyph/tile?lod=9&i=1&j=1', 400], ['/api/v1/ne/glyph/tile?lod=1&i=1&j=1', 404], ['/api/v1/ne/glyph/cell?cell=zz', 400], ['/api/v1/ne/glyph/kg?kg=1', 400], ['/api/v1/ne/glyph/cell?lon=9&lat=9', 200]]) { const r = await fetchJ(u, 'bad'); want('input ' + u.slice(16), r.s === exp, r.s); }
  const sub = await fetchJ('/api/v1/ne/glyph/sub', 'sub'); subUp = sub.s === 200 && sub.j && !!sub.j.bio;
  want('sub.200|peer-down', sub.s === 200 || sub.s === 502 || sub.s === 503 || sub.s === 504, sub.s); if (!subUp) R.warn.push('subsidy peer down (' + sub.s + ') — sub tile checks relaxed');

  /* ── 2. pick spots: KGs that hold cells, plus one change and one observed res-7 cell ── */
  const heldCodes = Array.from(new Set(C.filter(c => c[2] > 0 && c[8] >= 0).map(c => D.kg_codes[c[8]])));
  const pick = (arr) => arr.length ? arr[Math.floor(rnd() * arr.length)] : null;
  const rounds = o.rounds || 1, nSpots = o.spots || 2;
  const chgCell = pick(C.filter(c => c[7] > 0)), obsCell = pick(C.filter(c => c[6] && c[2] > 0));

  /* ── 3. cross-layer: res-7 figures vs full-res tile meta (3×3 lod-2 block covers the res-7 cell) ── */
  const block = async (c, v, key) => { const [lon, lt] = G.lonlat(c[0], c[1]); const i0 = Math.floor(lon / 0.1), j0 = Math.floor(lt / 0.1); let sum = 0, n = 0, bad = 0; for (let di = -1; di <= 1; di++) for (let dj = -1; dj <= 1; dj++) { const t = await fetchTile(2, i0 + di, j0 + dj, v); if (t.s === 200) { n += +t.m.n || 0; sum += key(t.m); if (!t.png) bad++; } else if (t.s !== 404) bad++; } return [sum, n, bad]; };
  if (chgCell) { const [conf, n, bad] = await block(chgCell, 'chg', m => +((m.chg || '0,0').split(',')[0])); want('xlayer.chg res7→lod2 conf>0', conf > 0 && n > 0 && !bad, { cell: chgCell.slice(0, 2), conf, n, bad }); }
  if (obsCell) { const [ob, n, bad] = await block(obsCell, 'obs', m => +m.obs || 0); want('xlayer.obs res7→lod2 obs>0', ob > 0 && n > 0 && !bad, { cell: obsCell.slice(0, 2), ob, n, bad }); }
  /* every view of one tile agrees on the cell count and paints a PNG */
  if (obsCell) { const [lon, lt] = G.lonlat(obsCell[0], obsCell[1]); for (const lod of [0, 1, 2]) { const L = [0.025, 0.01, 0.1][lod], i = Math.floor(lon / L), j = Math.floor(lt / L); const ts = {}; for (const v of ['', 'obs', 'tree', 'chg', 'sub', 'subc']) ts[v] = await fetchTile(lod, i, j, v); const n0 = +ts[''].m.n; want(`tile lod${lod} views agree`, ['', 'obs', 'tree', 'chg'].every(v => ts[v].s === 200 && ts[v].png && ts[v].w === ts[''].w && ts[v].h === ts[''].h) && ['obs', 'tree'].every(v => +ts[v].m.n === n0) && ['chg', 'sub', 'subc'].every(v => !ts[v].m.n || +ts[v].m.n >= n0) /* base/obs/tree: held rows; chg/sub/subc: every H3 cell of the box */ && ['sub', 'subc'].every(v => (ts[v].s === 200 && ts[v].png) || (ts[v].s === 204 && ts[v].m.down === '1')), Object.fromEntries(Object.entries(ts).map(([v, t]) => [v || 'base', [t.s, t.m.n, t.w + 'x' + t.h, t.png]]))); } }

  /* ── 4. states per spot, through share links ───────────────────────────────────── */
  const h0 = location.hash;
  for (let r = 0; r < rounds; r++) {
    const codes = o.kgs || Array.from({ length: nSpots }, () => pick(heldCodes));
    for (const kg of codes) {
      const P = 'kg' + kg + ' ';
      const row = kgIdx.get(kg);
      /* overview + card */
      let s = await go(`#kg=${kg};open`);
      want(P + 'sel', s.sel === kg, s.sel); /* full may already be on: a small KG fits deeper than the overview's cap and hiAuto takes over */
      const card = G.card(); want(P + 'card', card.length > 20 && (!row || card.indexOf(row[1]) >= 0), card.slice(0, 60));
      want(P + 'hash kg,open', /(^|#)kg=\d{5}(;|$)/.test(s.hash) && /;open/.test(s.hash), s.hash);
      /* KG outline agrees with the index bbox */
      const kgo = await fetchJ(`/api/v1/ne/glyph/kg?kg=${kg}`, 'kgoutline');
      want(P + 'outline', kgo.s === 200 && kgo.j.d && kgo.j.cells > 0 && (!row || (kgo.j.bbox[0] >= row[4] - 40 && kgo.j.bbox[2] <= row[6] + 40 && kgo.j.bbox[1] >= row[5] - 40 && kgo.j.bbox[3] <= row[7] + 40)), kgo.s === 200 ? { bbox: kgo.j.bbox, idx: row && row.slice(4) } : kgo.s);
      /* declared, full resolution, single cells */
      s = await go(`#kg=${kg};full;z=400`);
      layers(P + 'declared/400', s, { view: '', lod: 1 });
      /* observed */
      s = await go(`#kg=${kg};v=obs;full;z=400`); layers(P + 'obs/400', s, { view: 'obs', lod: 1 });
      /* trees on either profile */
      if (o.trees !== false) { s = await go(`#kg=${kg};v=obs;full;trees;z=400`); layers(P + 'obs+trees/400', s, { view: 'obs', lod: 1, trees: true }); s = await go(`#kg=${kg};full;trees;z=400`); layers(P + 'declared+trees/400', s, { view: '', lod: 1, trees: true }); }
      /* photo (basemap.at orthophoto under the base, near tiles only): default weight, a given weight (hash round trip of ;ortho=), off at lod 2 while the token stays */
      if (o.ortho !== false) {
        s = await go(`#kg=${kg};full;ortho;z=400`); layers(P + 'ortho/400', s, { view: '', lod: 1, ortho: true }); want(P + 'ortho default weight', s.orthoOp === 0.5 && /;ortho(;|$)/.test(s.hash), { op: s.orthoOp, hash: s.hash });
        s = await go(`#kg=${kg};v=obs;change;full;ortho=0.8;z=200`); layers(P + 'obs+chg+ortho=0.8/200', s, { view: 'obs', change: true, lod: 0, ortho: true }); want(P + 'ortho weight kept', s.orthoOp === 0.8 && /;ortho=0\.8(;|$)/.test(s.hash), { op: s.orthoOp, hash: s.hash });
        const otf = getComputedStyle(document.querySelector('svg.glyph .hi .b')).filter; want(P + 'ortho fills step back past the half', /opacity\(0\.6/.test(otf), otf);
        s = await go(`#kg=${kg};full;ortho;z=20`); layers(P + 'ortho/20 (coarse: token kept, layer off)', s, { view: '', lod: 2, ortho: true });
        want(P + 'ortho button faded at lod 2', document.querySelector('.gv-ortho').classList.contains('off'), document.querySelector('.gv-ortho').className);
      }
      /* roads (OSM roads + rail, v=rd tiles, near tiles only): default weight, a given weight (hash round trip of ;roads=), off at lod 2 while the token stays, under observed + change + photo */
      if (o.roads !== false) {
        s = await go(`#kg=${kg};full;roads;z=400`); layers(P + 'roads/400', s, { view: '', lod: 1, roads: true }); want(P + 'roads default weight', s.roadsOp === 0.7 && /;roads(;|$)/.test(s.hash), { op: s.roadsOp, hash: s.hash });
        const rg = document.querySelector('svg.glyph .hi .ov-rd'); want(P + 'roads group weighed', !!rg && +rg.style.opacity === 0.7, rg && rg.style.opacity);
        s = await go(`#kg=${kg};v=obs;change;full;roads=0.4;z=200`); layers(P + 'obs+chg+roads=0.4/200', s, { view: 'obs', change: true, lod: 0, roads: true }); want(P + 'roads weight kept', s.roadsOp === 0.4 && /;roads=0\.4(;|$)/.test(s.hash) && +rg.style.opacity === 0.4, { op: s.roadsOp, hash: s.hash, g: rg.style.opacity });
        const rt = s.tiles.find(t => t.v === 'rd' && t.ok); want(P + 'roads tile meta', !!rt && 'rd' in rt.m && 'km' in rt.m && 'rdc' in rt.m, rt && rt.m); const rtk = s.tiles.find(t => t.v === 'rd' && t.ok && +t.m.km > 0.3 && (+t.m.rdc.split(',')[0] + +t.m.rdc.split(',')[1] + +t.m.rdc.split(',')[2]) > 0); if (rtk) want(P + 'roads remoteness meta (rm 0–100, rmb 4 bands)', 'rm' in rtk.m && +rtk.m.rm >= 0 && +rtk.m.rm <= 100 && (rtk.m.rmb || '').split(',').length === 4, rtk.m);
        s = await go(`#kg=${kg};full;roads;z=20`); layers(P + 'roads/20 (coarse: token kept, layer off)', s, { view: '', lod: 2, roads: true });
        want(P + 'roads button faded at lod 2', document.querySelector('.gv-rd').classList.contains('off'), document.querySelector('.gv-rd').className);
      }
      /* filter (docs/ne-filter.md): ;f= round trip, worker bins + tiles complete, veil group shown, counts sane, chips, legend row, label input canonicalised, LOD-2 readout, clear */
      if (o.filter !== false) {
        const fw = async () => { const until = Date.now() + 12000; let f = G.filter(); while ((f.pending > 0 || (f.bins === 0 && f.f)) && Date.now() < until) { await sleep(150); f = G.filter(); } return f; };
        s = await go(`#kg=${kg};full;z=200;f=d=geb+bau,sl>=3`); let F = await fw(); layers(P + 'filter/200', s, { view: '', lod: 0 });
        want(P + 'filter applied', F.f === 'd=bau+geb,sl>=3' && F.open && !F.ignored.length && /;f=d=bau\+geb,sl>=3(;|$)/.test(decodeURIComponent(location.hash)), { f: F.f, hash: location.hash });
        want(P + 'filter bins complete', F.bins > 0 && F.pending === 0 && F.cells > 0 && F.match >= 0 && F.match <= F.cells, { bins: F.bins, pending: F.pending, cells: F.cells, match: F.match });
        const fg = document.querySelector('svg.glyph .hi .ov-flt'), fi = fg ? fg.querySelectorAll('image.in[href]').length : 0, onBase = s.tiles.filter(t => t.on && t.v === '' && t.k.split('/')[1] === String(s.lod)).length;
        want(P + 'filter veil shown', !!fg && getComputedStyle(fg).display !== 'none' && fi > 0 && fi <= onBase, { fi, onBase, disp: fg && getComputedStyle(fg).display });
        want(P + 'filter chips + readout', document.querySelectorAll('.glyph-filt .gf-c').length === 2 && /of .* cells/.test(F.readout) && !!document.querySelector('.lg12 .nt.flt'), { chips: document.querySelectorAll('.glyph-filt .gf-c').length, readout: F.readout });
        const fb = F.bbox, c0 = (fb[2] - fb[0] <= 0.1 && fb[3] - fb[1] <= 0.1) ? await (await fetch(`/api/v1/ne/filter?bbox=${fb.join(',')}&f=d=geb%2Bbau,sl>=3`)).json().catch(() => null) : null;
        if (c0 && c0.cells) want(P + 'filter agent endpoint agrees', Math.abs(c0.match / c0.cells - F.match / F.cells) < 0.15, { api: [c0.match, c0.cells], page: [F.match, F.cells] });
        s = await go(`#kg=${kg};full;z=200;f=chg=confirmed|bogus`); F = await fw();
        want(P + 'filter labels + ignored', F.f === 'chg=conf' && F.ignored.length === 1 && document.querySelector('.gf-in').value.indexOf('bogus') >= 0, { f: F.f, ignored: F.ignored, input: document.querySelector('.gf-in').value });
        s = await go(`#kg=${kg};full;z=20;f=steep`); F = await fw(); layers(P + 'filter/20 (coarse: token kept, veil off)', s, { view: '', lod: 2 });
        want(P + 'filter coarse readout', F.f === 'sl>=20' && /zoom in/.test(F.readout) && F.bins === 0, { f: F.f, readout: F.readout, bins: F.bins });
        G.filter(''); await settle(); await sleep(400); F = G.filter(); want(P + 'filter cleared', !F.f && F.bins === 0 && !/;f=/.test(location.hash) && !document.querySelector('svg.glyph').classList.contains('v-flt'), { f: F.f, hash: location.hash });
      }
      /* change (+ relief) at lod 0 on either profile; legacy v=chg reads as declared + change */
      s = await go(`#kg=${kg};v=chg;full${o.relief !== false ? ';relief' : ''};z=200`); layers(P + 'chg(legacy)+relief/200', s, { view: '', change: true, lod: 0, relief: o.relief !== false });
      s = await go(`#kg=${kg};v=obs;change;full;z=200`); layers(P + 'obs+chg/200', s, { view: 'obs', change: true, lod: 0 });
      /* subsidy on either profile; the subc variant whenever change is on — at every LOD the same rules as the profile
         (one cut per LOD since 2026-10-30: sub tiles = base tiles, no `~f` key, no 204 unless the peer is down, under-LOD
         cull waits for them, a sub tile of another LOD is hidden once its cover is in) */
      if (o.subsidy !== false) {
        for (const [z, lod] of [[400, 1], [200, 0], [20, 2]]) {
          s = await go(`#kg=${kg};full;subsidy;z=${z}`); layers(P + 'sub/' + z, s, { view: '', lod, sub: true });
          s = await go(`#kg=${kg};change;full;subsidy;z=${z}`); layers(P + 'subc/' + z, s, { view: '', change: true, lod, sub: true });
          if (lod === 1) { s = await go(`#kg=${kg};v=obs;full;subsidy;z=${z}`); layers(P + 'obs+sub/' + z, s, { view: 'obs', lod, sub: true }); }
          s = await go(`#kg=${kg};v=obs;change;full;subsidy;z=${z}`); layers(P + 'obs+subc/' + z, s, { view: 'obs', change: true, lod, sub: true });
        }
        /* the LOD walk with the layer on: 2 → 0 → 1 → 0 → 2 by zoom, no tile of the LOD left behind painted once the new one is complete */
        s = await go(`#kg=${kg};full;subsidy;z=20`); for (const z of [200, 400, 200, 20]) { s = await go(`#kg=${kg};full;subsidy;z=${z}`); layers(P + 'sub walk/' + z, s, { view: '', lod: z >= 300 ? 1 : z >= 110 ? 0 : 2, sub: true }); }
      }
      /* the whole matrix at lod 1 */
      if (o.matrix !== false) { const r = await matrix(kg, { settle: SET, _want: want, P, lods: [1], trees: o.trees, relief: o.relief, subsidy: o.subsidy, ortho: o.ortho, roads: o.roads }); if (typeof r === 'string') R.lines.push(r); }
      /* coarse full res (lod 2) at z=20 */
      s = await go(`#kg=${kg};full;z=20`); layers(P + 'declared/20', s, { view: '', lod: 2 });
      /* pin a cell: client ↔ server agree on the cell under a lon/lat; card names it */
      s = await go(`#kg=${kg};full;z=400`);
      const cen = G.lonlat(s.vb[0] + s.vb[2] / 2, s.vb[1] + s.vb[3] / 2);
      G.pin(cen[0], cen[1]); s = await settle(250);
      const cd = await fetchJ(`/api/v1/ne/glyph/cell?lon=${cen[0]}&lat=${cen[1]}&sub=1`, 'cell');
      want(P + 'pin=server cell', cd.s === 200 && s.pin && cd.j.cell === s.pin, [s.pin, cd.j && cd.j.cell]);
      want(P + 'cell doc shape', cd.s === 200 && cd.j.poly && cd.j.poly.length >= 5 && cd.j.x >= 0 && cd.j.x <= D.w && (!cd.j.kgs || Object.keys(cd.j.kgs).every(k => /^\d{5}$/.test(k))) && (cd.j.held === false || ('lu' in cd.j && 'pic_obs' in cd.j && 'chg' in cd.j)), cd.j && Object.keys(cd.j));
      want(P + 'hash h3 after pin', s.pin ? new RegExp('(^|#)h3=' + s.pin).test(s.hash) : true, s.hash);
      want(P + 'card after pin', G.card().length > 20, G.card().slice(0, 80));
      /* peek: hover a held spot that is not under the card and not the pinned cell (both suppress the peek by design) */
      const tipR = document.querySelector('.glyph-tip').getBoundingClientRect(); let pk = null, pkDoc = null;
      for (const [fx, fy] of [[0.15, 0.5], [0.85, 0.5], [0.5, 0.15], [0.5, 0.85], [0.15, 0.15]]) { const ll = G.lonlat(s.vb[0] + s.vb[2] * fx, s.vb[1] + s.vb[3] * fy), cp = G.client(ll[0], ll[1]); if (cp[0] > tipR.left && cp[0] < tipR.right && cp[1] > tipR.top && cp[1] < tipR.bottom) continue; const dj = await fetchJ(`/api/v1/ne/glyph/cell?lon=${ll[0]}&lat=${ll[1]}`, 'cell'); if (dj.s === 200 && dj.j.held !== false && dj.j.cell !== s.pin) { pk = ll; pkDoc = dj.j; break; } }
      if (pk) { G.hover(pk[0], pk[1]); await sleep(700); const pt = G.peek(); want(P + 'peek', pt.length > 0 && (!pkDoc.pic_obs || true), pt.slice(0, 60)); }
      /* share link round trip: mint → leave → reopen → same state */
      const minted = G.state().hash;
      await go('#kg=' + kg); await go('#'); const s2 = await go(minted);
      const same = s2.view === s.view && !!s2.change === !!s.change && s2.full === s.full && s2.trees === s.trees && s2.relief === s.relief && s2.ortho === s.ortho && s2.orthoOp === s.orthoOp && s2.roads === s.roads && s2.roadsOp === s.roadsOp && s2.subsidy === s.subsidy && Math.abs(s2.z / s.z - 1) < 0.03 && s2.pin === s.pin;
      want(P + 'share link round trip', same, { minted, from: pickState(s), to: pickState(s2) });
      /* rapid toggles must converge, never leave a half-loaded overlay */
      G.view('obs'); G.change(true); G.view(''); G.view('obs'); G.change(false); G.change(true); G.trees(true); G.trees(false); G.trees(true); s = await settle(); layers(P + 'rapid toggles', s, { view: 'obs', change: true, lod: s.lod, trees: true });
      G.trees(false); G.change(false); if (o.subsidy !== false) { G.subsidy(true); G.change(true); G.view(''); G.change(false); G.change(true); G.change(false); s = await settle(); layers(P + 'rapid sub', s, { view: '', lod: s.lod, sub: true }); G.subsidy(false); }
      /* budget stress: a walk at lod 1 across > HI_KEEP tiles of three views — eviction must hold the budget, left must come back to 0,
         nothing may paint under the lod in use, no JS error (this is the "phone freezes after a while" path, docs/ne-glyph.md §paint budget) */
      if (o.stress !== false) { const r = await stress(kg, { settle: SET, _want: want, P }); if (typeof r === 'string') R.lines.push(r); }
    }
  }
  if (o.offline !== false) { const r = await offline(null, { settle: SET, _want: want, _warn: R.warn }); if (typeof r === 'string') R.lines.push(r); }
  await G.go(h0 || '#');
  return finish();

  function layers(P, s, e) { return layersOf({ want, G, subUp, warn: R.warn }, P, s, e); }
  function finish() {
    const q = (a, p) => { if (!a || !a.length) return 0; const b = a.slice().sort((x, y) => x - y); return Math.round(b[Math.min(b.length - 1, Math.floor(b.length * p))]); };
    const lt = Object.keys(R.ms).map(k => `${k} p50/max ${q(R.ms[k], .5)}/${q(R.ms[k], 1)}ms n${R.ms[k].length}`).join(' · ');
    const head = `glyph soak ${R.fail ? 'FAIL' : 'PASS'} ${R.ok + R.fail} checks, ${R.fail} failed · ${((Date.now() - R.t0) / 1000).toFixed(0)}s · seed ${seed} · as_of ${D ? D.as_of : '?'} · ${lt}` + (R.warn.length ? ' · warn: ' + R.warn.join('; ') : '');
    const out = [head].concat(R.lines).join('\n');
    window.neGlyphSoak = window.neGlyphSoak || {}; window.neGlyphSoak.last = R; window.neGlyphSoak.text = out;
    return out;
  }
}
/* expectations for one state: tiles wanted ⇔ present, complete, and the right overlay groups shown.
   e: view ('' | 'obs' — the profile), change, full (default true; null = do not insist, tile checks only if on), lod (null = any), trees, relief, sub, ortho, roads */
function layersOf(X, P, s, e) {
  const { want, G, subUp } = X, disp = (sel) => { const el = document.querySelector('svg.glyph .hi ' + sel); return el ? getComputedStyle(el).display : 'none'; }; /* no full-res layer yet (fresh page at the overview) counts as not displayed */
  {
    want(P + ' profile', s.view === e.view, s.view);
    want(P + ' change', !!s.change === !!e.change, s.change);
    if (e.time !== undefined) want(P + ' time', (s.time || '') === (e.time || ''), s.time); /* ;t= window (docs/ne-change.md §6e): applied, loaded, on the chg tiles */
    if (e.time) { want(P + ' time loaded', !!s.timeLoaded, s.timeLoaded); const strip = document.querySelector('.glyph-time'); want(P + ' time strip', !!strip && !strip.hidden && strip.classList.contains('on') && !!strip.querySelector('.tl-b.in'), strip && strip.className); if (s.change && s.lod != null) { const ct = s.tiles.filter(t => t.on && t.v === 'chg'); want(P + ' time tiles', ct.every(t => t.k.indexOf('~' + e.time) > 0), ct.map(t => t.k)); } }
    const fullExp = e.full === undefined ? true : e.full;
    want(P + ' full', fullExp == null || s.full === fullExp, s.full);
    want(P + ' lod', e.lod == null || s.lod === e.lod, s.lod);
    want(P + ' not stuck', !s.loading && !s.applying && s.left === 0 && s.reliefLeft === 0 && !(s.orthoLeft > 0) && !s.refits, { loading: s.loading, applying: s.applying, left: s.left, reliefLeft: s.reliefLeft, orthoLeft: s.orthoLeft, refits: s.refits, animating: s.animating });
    if (s.animating) X.warn.push(P + ' fly animation still running after settle ' + str({ fly: s.fly, tour: s.tour, z: Math.round(s.z), hash: s.hash }));
    const on = s.full ? s.tiles.filter(t => t.on && t.k.split('/')[1] === String(s.lod)) : [];
    const base = on.filter(t => t.v === ''), ov = on.filter(t => t.v === s.view && s.view), ap = on.filter(t => t.v === 'apex'), ch = on.filter(t => t.v === 'chg');
    const sv = s.change ? 'subc' : 'sub', sb = on.filter(t => t.v === sv), df = on.filter(t => t.v === 'dif'), rd = on.filter(t => t.v === 'rd'), other = on.filter(t => ['', 'obs', 'chg', 'dif', 'rd', 'sub', 'subc', 'apex'].indexOf(t.v) < 0 || (t.v === 'obs' && !s.view) || (t.v === 'chg' && !s.change) || (t.v === 'dif' && !s.diff) || (t.v === 'rd' && !s.roads));
    if (s.full) want(P + ' nothing loading for a layer that is off', other.every(t => t.ok), other.filter(t => !t.ok).map(t => t.v + '/' + t.k).slice(0, 5)); /* cached tiles of an off layer stay in view (hidden by the group), they must just not be fetched */
    if (s.full) want(P + ' base tiles', base.length > 0 && base.every(t => t.ok), { n: base.length, notOk: base.filter(t => !t.ok).map(t => t.k) });
    want(P + ' no incomplete tile on screen', on.every(t => t.ok), on.filter(t => !t.ok).map(t => t.v + '/' + t.k));
    if (s.view && s.full) want(P + ' profile tiles = base', ov.length === base.length && ov.every(t => t.ok), { ov: ov.length, base: base.length });
    if (s.change && s.full) want(P + ' change tiles = base', ch.length === base.length && ch.every(t => t.ok), { chg: ch.length, base: base.length });
    want(P + ' diff', !!s.diff === !!e.diff, s.diff);
    if (e.diff && s.full && s.lod !== 2) want(P + ' diff tiles = base', df.length === base.length && df.every(t => t.ok), { dif: df.length, base: base.length });
    if (s.full && s.lod === 2) want(P + ' no diff tile at lod 2', df.length === 0, df.length);
    want(P + ' roads', !!s.roads === !!e.roads, s.roads);
    if (e.roads && s.full && s.lod !== 2) want(P + ' roads tiles = base', rd.length === base.length && rd.every(t => t.ok), { rd: rd.length, base: base.length });
    if (s.full && s.lod === 2) want(P + ' no roads tile at lod 2', rd.length === 0, rd.length);
    want(P + ' trees', !!s.trees === !!e.trees, s.trees);
    if (e.trees && s.full && s.lod !== 2) want(P + ' apex tiles = base', ap.length === base.length && ap.every(t => t.ok), { ap: ap.length, base: base.length });
    want(P + ' relief', !!s.relief === !!e.relief, s.relief);
    want(P + ' ortho', !!s.ortho === !!e.ortho, s.ortho);
    if (e.ortho && s.full && s.lod !== 2) want(P + ' ortho tiles on screen', document.querySelectorAll('svg.glyph .hi .ov-ortho image').length > 0, s.orthoZ);
    want(P + ' subsidy', !!s.subsidy === !!e.sub, s.subsidy);
    const subVis = !!e.sub;
    if (subVis && subUp && s.full) want(P + ' sub tiles = base', s.subsidyLoaded && sb.length === base.length && sb.every(t => t.ok), { sb: sb.length, base: base.length, loaded: s.subsidyLoaded });
    if (subVis && s.full) { const fk = s.tiles.filter(t => (t.v === 'sub' || t.v === 'subc') && /~f/.test(t.k)); want(P + ' sub one cut per lod (no ~f)', fk.length === 0, fk.map(t => t.k).slice(0, 3)); }
    if (subVis && subUp && s.full && s.mem && s.mem.under) { const sh = s.tiles.filter(t => (t.v === 'sub' || t.v === 'subc') && t.on && !t.hid && t.k.split('/')[1] !== String(s.lod)); want(P + ' no sub tile of another lod painted', sh.length === 0, sh.map(t => t.k).slice(0, 3)); }
    if (subVis && subUp && s.full) want(P + ' subsidy button not off/busy', !document.querySelector('.gv-sub').classList.contains('off') && !document.querySelector('.gv-sub').classList.contains('busy'), document.querySelector('.gv-sub').className);
    /* DOM: exactly the overlay groups of the state are displayed */
    const cls = document.querySelector('svg.glyph').classList;
    want(P + ' svg classes', cls.contains('v-obs') === (s.view === 'obs') && cls.contains('v-chg') === !!s.change && cls.contains('v-dif') === !!s.diff && cls.contains('v-tree') === !!s.trees && cls.contains('v-rel') === !!s.relief && cls.contains('v-ortho') === !!s.ortho && cls.contains('v-rd') === !!s.roads && cls.contains('v-sub') === !!subVis && cls.contains('hi') === !!s.full, Array.from(cls).join(' '));
    if (document.querySelector('svg.glyph .hi .ov-chg')) { /* the overlay groups exist once full resolution was on at least once */
    want(P + ' ov-obs shown⇔obs', (disp('.ov-obs') !== 'none') === (s.view === 'obs'), disp('.ov-obs'));
    want(P + ' ov-chg shown⇔change', (disp('.ov-chg') !== 'none') === !!s.change, disp('.ov-chg'));
    want(P + ' ov-dif shown⇔diff&near', (disp('.ov-dif') !== 'none') === (!!s.diff && s.full && s.lod !== 2), disp('.ov-dif'));
    want(P + ' ov-rd shown⇔roads&near', (disp('.ov-rd') !== 'none') === (!!s.roads && s.full && s.lod !== 2), disp('.ov-rd'));
    { const kids = Array.from(document.querySelector('svg.glyph .hi').children).map(c => c.getAttribute('class')), ri = kids.indexOf('ov ov-rd'), si = kids.indexOf('ov ov-subc'), li = kids.indexOf('ov ov-rel'), ti = kids.indexOf('ov ov-tree'); want(P + ' ov-rd z-order (over subsidy, under relief + trees)', ri > si && (li < 0 || ri < li) && ri < ti, kids.join(' ')); }
    want(P + ' ov-sub shown⇔sub&!change', (disp('.ov-sub') !== 'none') === (!!subVis && !s.change), disp('.ov-sub'));
    want(P + ' ov-subc shown⇔sub&change', (disp('.ov-subc') !== 'none') === (!!subVis && !!s.change), disp('.ov-subc'));
    }
    if (s.full && document.querySelector('svg.glyph .hi .ov-tree')) want(P + ' ov-tree shown⇔trees', (disp('.ov-tree') !== 'none') === !!s.trees, disp('.ov-tree'));
    /* the bar mirrors the state */
    const pr = (sel) => { const b = document.querySelector('.glyph-view ' + sel); return b ? b.getAttribute('aria-pressed') === 'true' : null; }, ck = (sel) => { const b = document.querySelector('.glyph-view ' + sel); return b ? b.getAttribute('aria-checked') === 'true' : null; };
    want(P + ' bar = state', ck('.gv-b[data-v=""]') === !s.view && ck('.gv-b[data-v="obs"]') === (s.view === 'obs') && pr('.gv-chg') === !!s.change && pr('.gv-dif') === !!s.diff && pr('.gv-sub') === !!s.subsidy && pr('.gv-tree') === !!s.trees && pr('.gv-relief') === !!s.relief && pr('.gv-ortho') === !!s.ortho && pr('.gv-rd') === !!s.roads, { dec: ck('.gv-b[data-v=""]'), obs: ck('.gv-b[data-v="obs"]'), chg: pr('.gv-chg'), dif: pr('.gv-dif'), sub: pr('.gv-sub'), tree: pr('.gv-tree'), rel: pr('.gv-relief') });
    /* the legend carries one block per overlay on, in z-order: profile head, change, (subsidy / relief appended), trees */
    if (s.full) { const lg = G.legend().toLowerCase(), headOK = lg.indexOf(s.view === 'obs' ? 'in view · observed' : 'in view · declared') >= 0, ci = lg.indexOf('\nchange'), di = lg.indexOf('\ndiff'), ti = lg.indexOf('\ntrees'), si = lg.indexOf('\nsubsidy');
      want(P + ' legend blocks', headOK && (ci >= 0) === !!s.change && (di >= 0) === !!s.diff && (!s.change || !s.diff || ci < di) && ((ti >= 0) === !!(s.trees && s.lod !== 2) || s.lod === 2) && (!subUp || (si >= 0) === !!s.subsidy) && (!s.change || !s.trees || s.lod === 2 || ci < ti), { headOK, change: ci, diff: di, trees: ti, subsidy: si, legend: lg.slice(0, 80) }); }
    if (document.querySelector('svg.glyph .hi .ov-rel')) want(P + ' ov-rel shown⇔relief', (disp('.ov-rel') !== 'none') === !!s.relief, disp('.ov-rel'));
    if (document.querySelector('svg.glyph .hi .ov-ortho')) { want(P + ' ov-ortho shown⇔ortho&near', (disp('.ov-ortho') !== 'none') === (!!s.ortho && s.full && s.lod !== 2), disp('.ov-ortho')); want(P + ' ov-ortho under the base', document.querySelector('svg.glyph .hi').firstElementChild.classList.contains('ov-ortho'), document.querySelector('svg.glyph .hi').firstElementChild.className.baseVal); }
    if (s.full && s.lod !== 2) want(P + ' legend photo line⇔ortho', (G.legend().toLowerCase().indexOf('photo') >= 0) === !!s.ortho, G.legend().slice(-80));
    if (s.full && s.lod !== 2) want(P + ' legend roads line⇔roads', /\nroads /.test(G.legend()) === !!s.roads, G.legend().slice(-80));
    if (s.full || subVis) want(P + ' legend', G.legend().length > 10, G.legend().slice(0, 50)); /* the legend is a full-res (or subsidy) thing; the overview has none */
    /* paint budget (docs/ne-glyph.md §paint budget): once the LOD in use is complete, no tile of another LOD is painted under it;
       every <image> left in the SVG is one of the cached tiles; the decode queue is drained; the cache holds the budget (+ what is on screen) */
    if (s.mem && !s.full) { /* full off: hiOff keeps the lod-2 bitmaps in the faded-out group as the backdrop for the way back; the lod 0/1 groups are emptied */ }
    else if (s.mem) {
      const other = s.tiles.filter(t => t.on && t.ok && t.k.split('/')[1] !== String(s.lod));
      if (s.full && base.length) want(P + ' under-lod hidden', s.mem.under && other.every(t => t.hid), { under: s.mem.under, miss: s.mem.underMiss, left: s.left, painted: other.filter(t => !t.hid).map(t => t.v + '/' + t.k).slice(0, 6) });
      want(P + ' decode drained', s.mem.decQ === 0 && s.mem.decN === 0, { q: s.mem.decQ, n: s.mem.decN });
      want(P + ' no leaked <image>', s.mem.images <= s.tiles.length, { images: s.mem.images, tiles: s.tiles.length });
      want(P + ' painted = on-screen & not hidden', s.mem.shown <= s.tiles.filter(t => t.on && !t.hid && t.ok).length, { shown: s.mem.shown, painted: s.tiles.filter(t => t.on && !t.hid && t.ok).length });
      const onN = s.tiles.filter(t => t.on).length;
      want(P + ' cache ≤ budget', s.tiles.length <= Math.max(s.mem.keep, onN), { tiles: s.tiles.length, keep: s.mem.keep, on: onN });
    } else X.warn.push(P + ' state().mem missing — stale page script');
    /* hash carries the state */
    const h = ';' + s.hash.replace(/^#/, ''); want(P + ' hash tokens', (s.view ? h.indexOf(';v=' + s.view) >= 0 : !/;v=/.test(h)) && /;change/.test(h) === !!s.change && /;diff(=raw)?(;|$)/.test(h) === !!s.diff && /;full/.test(h) === s.full && /;trees/.test(h) === !!s.trees && /;relief/.test(h) === !!s.relief && /;ortho(=[\d.]+)?(;|$)/.test(h) === !!s.ortho && /;roads(=[\d.]+)?(;|$)/.test(h) === !!s.roads && /;subsidy/.test(h) === !!s.subsidy, h);
  }
}

/* ── stress(kg, o): the long-session path — pan a lod-1 window over the KG with observed + subsidy on until the tile cache has turned
   over more than its budget; after every stop: budget held, left=0, under-lod hidden, decode queue drained, no JS error; one string ── */
export async function stress(kg, o) {
  o = o || {}; kg = kg || '63330'; const G = window.neGlyph; if (!G) return 'FAIL no window.neGlyph — run on the landing page /';
  const t0 = Date.now(), R = { ok: 0, fail: 0, lines: [] }, SET = o.settle || 20000, P = (o.P || '') + 'stress ';
  const want = o._want || ((k, cond, d) => { if (cond) { R.ok++; if (o.verbose) R.lines.push('ok ' + k); } else { R.fail++; R.lines.push('FAIL ' + k + (d != null ? ': ' + str(d) : '')); } return !!cond; });
  const h0 = location.hash; G.logReset();
  await G.go('#kg=' + kg + ';v=obs;full;z=400'); let s = await G.settled(SET);
  if (!s.mem) return 'stress SKIP state().mem missing — stale page script';
  const keep = s.mem.keep, steps = o.steps || Math.ceil(keep / 6) + 4, c = s.vb.slice(), w = c[2], hh = c[3];
  let peak = 0, maxLeft = 0, ev0 = s.mem.evicted, shownMax = 0;
  for (let i = 0; i < steps; i++) {
    // spiral of lod-1 windows, alternating zoom so lod 0 and lod 1 both fill the cache; the view alternates too
    const k = i + 1, ang = k * 2.4, r = 0.9 * Math.sqrt(k), x = c[0] + c[2] / 2 + Math.cos(ang) * r * w - w / 2, y = c[1] + c[3] / 2 + Math.sin(ang) * r * hh - hh / 2, z = i % 4 === 3 ? 200 : 400, v = i % 3 === 2 ? ';change' : ';v=obs';
    await G.go('#kg=' + kg + v + ';full;at=' + Math.round(x + w / 2) + ',' + Math.round(y + hh / 2) + ',' + z);
    s = await G.settled(SET);
    const onN = s.tiles.filter(t => t.on).length; peak = Math.max(peak, s.tiles.length); maxLeft = Math.max(maxLeft, s.left); shownMax = Math.max(shownMax, s.mem.shown);
    if (s.tiles.length > Math.max(keep, onN)) want(P + 'budget held @' + i, false, { tiles: s.tiles.length, keep, on: onN });
    if (s.left !== 0) want(P + 'left=0 @' + i, false, s.left);
    const under = s.tiles.filter(t => t.on && t.ok && t.k.split('/')[1] !== String(s.lod) && !t.hid);
    if (under.length) want(P + 'under-lod hidden @' + i, false, under.map(t => t.v + '/' + t.k).slice(0, 5));
    if (s.mem.decQ || s.mem.decN) want(P + 'decode drained @' + i, false, { q: s.mem.decQ, n: s.mem.decN });
  }
  const L = G.log(), ev = G.state().mem.evicted - ev0;
  want(P + 'walked past the budget', peak >= Math.min(keep, 40) || ev > 0, { peak, keep, evicted: ev, steps });
  want(P + 'eviction ran', ev > 0 || peak < keep, { evicted: ev, peak, keep });
  want(P + 'no JS error', L.errors.length === 0, L.errors.slice(0, 3));
  want(P + 'no stuck tile', G.state().tiles.filter(t => !t.ok).length === 0, G.state().tiles.filter(t => !t.ok).map(t => t.k).slice(0, 5));
  await G.go(h0 || '#');
  if (o._want) return null;
  return `stress ${kg} ${R.fail ? 'FAIL' : 'PASS'} ${R.ok + R.fail} checks, ${R.fail} failed · ${((Date.now() - t0) / 1000).toFixed(1)}s · keep ${keep} peak ${peak} evicted ${ev} shownMax ${shownMax} maxLeft ${maxLeft} · fetch ${str(L.fetch)}` + (R.lines.length ? '\n' + R.lines.join('\n') : '');
}

/* ── matrix(kg, o): every profile × overlay combination by share link, each through layersOf — the view-model contract
   (docs/ne-glyph.md §view model). lods: [1] default (z=400); 2 (z=20) and 0 (z=200) add the change/subsidy combos only (trees need
   the near tiles, relief fades past 300×). One line per failing combo; `verbose` lists each combo as it passes. ── */
export async function matrix(kg, o) {
  o = o || {}; kg = kg || '63330'; const G = window.neGlyph; if (!G) return 'FAIL no window.neGlyph — run on the landing page /';
  const t0 = Date.now(), R = { ok: 0, fail: 0, lines: [], warn: [] }, SET = o.settle || 20000, P = (o.P || '') + 'matrix ';
  const want = o._want || ((k, cond, d) => { if (cond) { R.ok++; if (o.verbose) R.lines.push('ok ' + k); } else { R.fail++; R.lines.push('FAIL ' + k + (d != null ? ': ' + str(d) : '')); } return !!cond; });
  let subUp = false; if (o.subsidy !== false) { const r = await fetch('/api/v1/ne/glyph/sub', { credentials: 'omit' }); subUp = r.status === 200; if (!subUp) R.warn.push('subsidy peer down (' + r.status + ')'); }
  const h0 = location.hash, lods = o.lods || [1], Z = { 0: 200, 1: 400, 2: 20 }, combos = [];
  for (const lod of lods) for (const v of ['', 'obs']) for (let m = 0; m < 128; m++) {
    const c = { v, change: !!(m & 1), sub: !!(m & 2), trees: !!(m & 4), relief: !!(m & 8), diff: !!(m & 16), ortho: !!(m & 32), roads: !!(m & 64), lod };
    if (c.roads && (o.roads === false || lod !== 1 || (m & 63) > 1 && (m & 63) !== 4 && (m & 63) !== 8 && (m & 63) !== 32 && (m & 63) !== 2)) continue; /* roads: alone, with change, with subsidy, with trees, with relief, with photo */
    if (c.ortho && (o.ortho === false || lod !== 1 || (m & 31) > 1 && (m & 31) !== 4 && (m & 31) !== 8)) continue; /* photo: alone, with change, with trees, with relief */
    if (lod !== 1 && (c.trees || c.relief)) continue;
    if (c.diff && (o.diff === false || (m & 15) > 1 && (m & 15) !== 4 && (m & 15) !== 8)) continue; /* diff: alone, with change, with trees, with relief — the view-model shape, not every product */
    if ((o.trees === false && c.trees) || (o.relief === false && c.relief) || (o.subsidy === false && c.sub)) continue;
    combos.push(c);
  }
  let n = 0;
  for (const c of combos) {
    const h = '#kg=' + kg + (c.v ? ';v=' + c.v : '') + (c.change ? ';change' : '') + (c.diff ? ';diff' : '') + (c.sub ? ';subsidy' : '') + (c.trees ? ';trees' : '') + (c.relief ? ';relief' : '') + (c.ortho ? ';ortho' : '') + (c.roads ? ';roads' : '') + ';full;z=' + Z[c.lod];
    const name = P + (c.v || 'declared') + (c.change ? '+change' : '') + (c.diff ? '+diff' : '') + (c.sub ? '+subsidy' : '') + (c.trees ? '+trees' : '') + (c.relief ? '+relief' : '') + (c.ortho ? '+photo' : '') + (c.roads ? '+roads' : '') + '/' + Z[c.lod];
    if (G.logReset) G.logReset();
    await G.go(h); await sleep(150); let s = await G.settled(SET);
    const until = Date.now() + SET; while ((s.reliefLeft > 0 || s.orthoLeft > 0 || (s.subsidy && !s.subsidyLoaded && subUp)) && Date.now() < until) { await sleep(150); s = G.state(); }
    const f0 = R.fail; layersOf({ want, G, subUp, warn: R.warn }, name, s, { view: c.v, change: c.change, diff: c.diff, sub: c.sub, trees: c.trees, relief: c.relief, ortho: c.ortho, roads: c.roads, lod: c.lod });
    const L = G.log ? G.log() : { errors: [] }; want(name + ' no js errors', L.errors.length === 0, L.errors.map(x => x.m + ' @' + x.at));
    if (o.verbose && R.fail === f0) R.lines.push('ok ' + name); n++;
  }
  if (!o._want) await G.go(h0 || '#');
  if (o._want) return R.lines.length ? R.lines.join('\n') : null;
  return `matrix ${kg} ${R.fail ? 'FAIL' : 'PASS'} ${n} combos, ${R.ok + R.fail} checks, ${R.fail} failed · ${((Date.now() - t0) / 1000).toFixed(0)}s` + (R.warn.length ? ' · warn: ' + R.warn.join('; ') : '') + (R.lines.length ? '\n' + R.lines.join('\n') : '');
}

/* ── check(hash, o): one state in one call — expectations read off the hash itself ──────── */
/* fail-soft under faults: every fetch class (tiles, cell docs, kgs, obs peer, sub peer, relief) must fail without an uncaught promise
   or a stuck busy button, the card must carry the muted offline row, and the next interaction after the fault is lifted must heal. */
export async function offline(hash, o) {
  o = o || {}; const G = window.neGlyph; if (!G) return 'FAIL no window.neGlyph — run on the landing page /';
  if (!G.fault) return 'FAIL neGlyph.fault missing — stale page script, reload';
  const t0 = Date.now(), R = { ok: 0, fail: 0, lines: [], warn: o._warn || [] }, SET = o.settle || 20000;
  const want = o._want || ((k, cond, d) => { if (cond) { R.ok++; if (o.verbose) R.lines.push('ok ' + k); } else { R.fail++; R.lines.push('FAIL ' + k + (d != null ? ': ' + str(d) : '')); } return !!cond; });
  const h = hash || '#kg=63330;full;v=obs;trees;z=300', h0 = location.hash, E = expectOf(h);
  const pin = async (lon, lat) => { if (G.state().pin) { G.pin(lon, lat); await sleep(250); } G.pin(lon, lat); };
  const busy = () => Array.from(document.querySelectorAll('.glyph-view .busy, .gt-d.busy')).map(e => e.className.replace(/\s+/g, ' ').trim());
  const offRow = () => !!document.querySelector('.glyph-tip .rw.off');
  const nudge = async (f) => { const c = G.state().vb; await G.go(h.replace(/;z=\d+/, '') + ';at=' + (c[0] + c[2] * f).toFixed(0) + ',' + (c[1] + c[3] * f).toFixed(0) + ',300'); };
  const settle = async (ms) => { await sleep(150); let s = await G.settled(SET); const until = Date.now() + 4000; while ((s.reliefLeft > 0 || s.orthoLeft > 0) && Date.now() < until) { await sleep(150); s = G.state(); } if (ms) await sleep(ms); return G.state(); };
  await G.go(h); await settle(300);
  const c0 = G.state().vb, ll = G.lonlat(c0[0] + c0[2] / 2, c0[1] + c0[3] / 2);
  for (const mode of ['offline', '5xx', 'peer']) {
    const P = 'offline/' + mode + ' ';
    G.logReset(); G.fault(mode);
    // interact through every fetch class: cell doc + obs peer (pin), change stats + tiles (view), apex peer (trees), sub peer, relief, pan
    await pin(ll[0], ll[1]); await sleep(500);
    G.view(E.view === 'obs' ? '' : 'obs'); G.change(!E.change); await sleep(250); G.view(E.view); G.change(!!E.change); if (E.trees) { G.trees(false); await sleep(100); G.trees(true); }
    G.subsidy(true); await sleep(300); G.relief(true); G.ortho(true); G.roads(true); await nudge(0.3);
    let s = await settle(900);
    const L = G.log(), n = G.net();
    want(P + 'no uncaught error', L.errors.length === 0, L.errors.map(x => x.m + ' @' + x.at));
    want(P + 'no busy button', busy().length === 0, busy());
    want(P + 'left=0', s.left === 0, s.left);
    want(P + 'no tile stuck ok=false', s.tiles.filter(t => !t.ok).length === 0, s.tiles.filter(t => !t.ok).map(t => t.k));
    const failed = Object.keys(L.fetch).filter(c => L.fetch[c].err || L.fetch[c]['503']);
    if (!failed.length) R.warn.push(P + 'nothing was asked (all cached) — offline row not checked');
    else {
      want(P + 'offline noted', n.offline && Object.keys(n.what).length > 0, n);
      want(P + 'card offline row', offRow(), (G.card() || '').slice(0, 120));
      if (mode === 'peer') want(P + 'faults reached a peer', failed.indexOf('peer') >= 0 || failed.indexOf('relief') >= 0 || failed.indexOf('ortho') >= 0, L.fetch);
      else if (E.full) want(P + 'faults reached tiles', failed.indexOf('tile') >= 0, L.fetch);
    }
    if (mode === 'peer') want(P + 'own tiles unaffected', !(L.fetch.tile && (L.fetch.tile.err || L.fetch.tile['503'])), L.fetch.tile);
    // lift the fault: the next interaction heals, nothing failed stays cached
    G.fault(null); G.subsidy(false); G.relief(false); G.ortho(false); G.roads(false); await sleep(2700);
    G.logReset(); await nudge(0.5); await settle(200); G.subsidy(true); await settle(1500); G.subsidy(false); await pin(ll[0], ll[1]); s = await settle(2200);
    const L2 = G.log(), n2 = G.net();
    want(P + 'recovered: tiles complete', s.tiles.filter(t => t.on && !t.ok).length === 0 && (!E.full || s.tiles.filter(t => t.on).length > 0), s.tiles.filter(t => t.on && !t.ok).map(t => t.k));
    if (E.trees) want(P + 'recovered: apex tiles', s.tiles.filter(t => t.v === 'apex' && t.on && t.ok).length > 0, s.tiles.filter(t => t.v === 'apex').length);
    want(P + 'recovered: no offline row', !offRow() && !n2.offline, { net: n2, now: Date.now(), card: (G.card() || '').slice(0, 120) });
    want(P + 'recovered: a real card', E.full ? /res 12/.test(G.card() || '') : !/^offline/.test(G.card() || ''), (G.card() || '').slice(0, 120));
    want(P + 'recovered: no busy', busy().length === 0, busy());
    want(P + 'recovered: no js errors', L2.errors.length === 0, L2.errors.map(x => x.m + ' @' + x.at));
  }
  G.fault(null); if (!o._want) await G.go(h0 || '#');
  if (o._want) return R.lines.length ? R.lines.join('\n') : null;
  const head = `offline ${R.fail ? 'FAIL' : 'PASS'} ${R.ok + R.fail} checks, ${R.fail} failed · ${((Date.now() - t0) / 1000).toFixed(1)}s`;
  return [head].concat(R.lines).join('\n');
}

export async function check(hash, o) {
  o = o || {}; const G = window.neGlyph; if (!G) return 'FAIL no window.neGlyph — run on the landing page /';
  const t0 = Date.now(), R = { ok: 0, fail: 0, lines: [], warn: [] };
  const want = (k, cond, d) => { if (cond) { R.ok++; if (o.verbose) R.lines.push('ok ' + k); } else { R.fail++; R.lines.push('FAIL ' + k + (d != null ? ': ' + str(d) : '')); } return !!cond; };
  const e = expectOf(hash); if (o.expect) Object.assign(e, o.expect);
  let subUp = false; if (e.sub) { const r = await fetch('/api/v1/ne/glyph/sub', { credentials: 'omit' }); subUp = r.status === 200; if (!subUp) R.warn.push('subsidy peer down (' + r.status + ')'); }
  if (G.logReset) G.logReset();
  await G.go(hash); await sleep(200); let s = await G.settled(o.settle || 20000);
  const until = Date.now() + (o.settle || 20000); while ((s.reliefLeft > 0 || s.orthoLeft > 0 || (s.subsidy && !s.subsidyLoaded && subUp)) && Date.now() < until) { await sleep(150); s = G.state(); }
  layersOf({ want, G, subUp, warn: R.warn }, 'state', s, e);
  const L = G.log ? G.log() : { errors: [], fetch: {} };
  want('no js errors', L.errors.length === 0, L.errors.map(x => x.m + ' @' + x.at));
  const bad = {}; for (const c in L.fetch) for (const k in L.fetch[c]) if (k !== '200' && k !== 'abort' && !(c === 'tile' && k === '204' && !subUp) && !(c === 'relief' && k === '204') && !(c === 'ortho' && k === '404')) bad[c + ':' + k] = L.fetch[c][k];
  want('fetches ok', Object.keys(bad).length === 0, bad);
  const head = `check ${hash} ${R.fail ? 'FAIL' : 'PASS'} ${R.ok + R.fail} checks, ${R.fail} failed · ${((Date.now() - t0) / 1000).toFixed(1)}s · lod ${s.lod} z ${Math.round(s.z)} · fetch ${str(L.fetch)}` + (R.warn.length ? ' · warn: ' + R.warn.join('; ') : '');
  const out = [head].concat(R.lines).join('\n'); window.neGlyphSoak = window.neGlyphSoak || {}; window.neGlyphSoak.lastCheck = { R, state: s, log: L }; return out;
}
function expectOf(h) { const tok = String(h || '').replace(/^#/, '').split(';'); const has = t => tok.indexOf(t) >= 0; let view = (tok.find(t => /^v=/.test(t)) || '').slice(2), change = has('change'); if (view === 'chg') { view = ''; change = true; } const time = (tok.find(t => /^t=/.test(t)) || '').slice(2); if (time) change = true; return { view, change, time, diff: has('diff') || has('diff=raw'), full: has('full') || tok.some(t => /^h3=/.test(t)) ? true : null, lod: null, trees: has('trees'), relief: has('relief'), ortho: tok.some(t => /^ortho(=[\d.]+)?$/.test(t)), roads: tok.some(t => /^roads(=[\d.]+)?$/.test(t)), sub: has('subsidy') }; }

/* ── tile / snap / diff: a tile's meta + bytes + pixels, before and after a change ──────── */
const LODDEG = [0.025, 0.01, 0.1];
function specKey(sp) { /* 'lod/i/j[/v]' | 'v/lod/i/j' | {lod,i,j,v} | {lon,lat,lod,v} → 'lod/i/j/v' */
  if (typeof sp === 'string') { const p = sp.split('/'); if (p.length === 4 && !/^\d$/.test(p[0])) return `${p[1]}/${p[2]}/${p[3]}/${p[0]}`; return `${p[0]}/${p[1]}/${p[2]}/${p[3] || ''}`; }
  if (sp.lon != null) { const L = LODDEG[sp.lod]; return `${sp.lod}/${Math.floor(sp.lon / L)}/${Math.floor(sp.lat / L)}/${sp.v || ''}`; }
  return `${sp.lod}/${sp.i}/${sp.j}/${sp.v || ''}`;
}
function specURL(k) { const [lod, i, j, v] = k.split('/'); return `/api/v1/ne/glyph/tile?lod=${lod}&i=${i}&j=${j}${v ? '&v=' + v : ''}`; }
async function sha(b) { if (crypto.subtle) return Array.from(new Uint8Array(await crypto.subtle.digest('SHA-256', b))).map(x => x.toString(16).padStart(2, '0')).join('').slice(0, 16); let h = 2166136261; for (const x of new Uint8Array(b)) { h ^= x; h = Math.imul(h, 16777619); } return (h >>> 0).toString(16); }
async function tileOf(r) { const m = parseMeta(r.headers.get('X-NE-Tile') || ''); delete m.ms; const b = r.status === 200 ? await r.arrayBuffer() : new ArrayBuffer(0); const u = new Uint8Array(b); const png = u.length > 24 && u[0] === 0x89 && u[1] === 0x50; return { s: r.status, m, bytes: u.length, png, w: png ? (u[16] << 24 | u[17] << 16 | u[18] << 8 | u[19]) >>> 0 : 0, h: png ? (u[20] << 24 | u[21] << 16 | u[22] << 8 | u[23]) >>> 0 : 0, sha: u.length ? await sha(b) : '', buf: b }; }
export async function tile(spec) { const k = specKey(spec); const r = await fetch(specURL(k), { credentials: 'omit', cache: 'no-cache' }); const t = await tileOf(r); delete t.buf; t.k = k; return t; }
function onScreen() { const s = window.neGlyph.state(); return s.tiles.filter(t => t.on && t.v !== 'apex' && t.k.split('/')[1] === String(s.lod)).map(t => t.k.replace(/~f$/, '')); } /* state() keys are 'v/lod/i/j[~f]' */
export async function snap(name, specs) {
  const c = await caches.open('neglyph-snap'); const keys = (specs || onScreen()).map(specKey); const out = [];
  for (const k of keys) { const r = await fetch(specURL(k), { credentials: 'omit', cache: 'no-cache' }); await c.put('/snap/' + name + '/' + k, r.clone()); const t = await tileOf(r); out.push(`${k} ${t.s} ${t.w}x${t.h} ${t.sha} n=${t.m.n || 0}`); }
  await c.put('/snap/' + name, new Response(JSON.stringify({ keys, at: new Date().toISOString() })));
  return `snap ${name}: ${keys.length} tiles\n` + out.join('\n');
}
export async function diff(name, specs) {
  const c = await caches.open('neglyph-snap'); let keys = specs && specs.map(specKey);
  if (!keys) { const ix = await c.match('/snap/' + name); if (!ix) return `FAIL no snap '${name}'`; keys = (await ix.json()).keys; }
  const lines = []; let changed = 0, missing = 0;
  for (const k of keys) {
    const o = await c.match('/snap/' + name + '/' + k); if (!o) { missing++; lines.push(`${k} no snapshot`); continue; }
    const [a, b] = await Promise.all([tileOf(o), fetch(specURL(k), { credentials: 'omit', cache: 'no-cache' }).then(tileOf)]);
    const d = []; if (a.s !== b.s) d.push(`status ${a.s}→${b.s}`); if (a.w !== b.w || a.h !== b.h) d.push(`size ${a.w}x${a.h}→${b.w}x${b.h}`);
    for (const key of new Set([...Object.keys(a.m), ...Object.keys(b.m)])) if (a.m[key] !== b.m[key]) d.push(`${key} ${a.m[key]}→${b.m[key]}`);
    if (a.sha !== b.sha) { let px = ''; if (a.png && b.png && a.w === b.w && a.h === b.h) { const p = await pixdiff(a.buf, b.buf, a.w, a.h); px = ` px ${p.n}/${a.w * a.h} (${(100 * p.n / (a.w * a.h)).toFixed(2)}%)` + (p.n ? ` bbox ${p.bb.join(',')}` : ''); } d.push(`bytes ${a.bytes}→${b.bytes}${px}`); }
    if (d.length) { changed++; lines.push(`${k} CHANGED ${d.join(' · ')}`); } else lines.push(`${k} same`);
  }
  const out = `diff ${name}: ${keys.length} tiles, ${changed} changed, ${missing} missing\n` + lines.join('\n'); window.neGlyphSoak = window.neGlyphSoak || {}; window.neGlyphSoak.lastDiff = out; return out;
}
async function pixdiff(a, b, w, h) {
  const img = async (buf) => { const bm = await createImageBitmap(new Blob([buf], { type: 'image/png' })); const cv = typeof OffscreenCanvas !== 'undefined' ? new OffscreenCanvas(w, h) : Object.assign(document.createElement('canvas'), { width: w, height: h }); const cx = cv.getContext('2d'); cx.drawImage(bm, 0, 0); return cx.getImageData(0, 0, w, h).data; };
  const A = await img(a), B = await img(b); let n = 0, x0 = w, y0 = h, x1 = -1, y1 = -1;
  for (let i = 0; i < A.length; i += 4) if (A[i] !== B[i] || A[i + 1] !== B[i + 1] || A[i + 2] !== B[i + 2] || A[i + 3] !== B[i + 3]) { n++; const p = i >> 2, x = p % w, y = (p / w) | 0; if (x < x0) x0 = x; if (x > x1) x1 = x; if (y < y0) y0 = y; if (y > y1) y1 = y; }
  return { n, bb: n ? [x0, y0, x1, y1] : [] };
}
window.neGlyphSoak = Object.assign(window.neGlyphSoak || {}, { run, check, tile, snap, diff, matrix, stress, offline });
function parseMeta(h) { const m = {}; h.split(';').forEach(kv => { const p = kv.indexOf('='); if (p > 0) m[kv.slice(0, p)] = kv.slice(p + 1); }); return m; }
function pickState(s) { return { view: s.view, change: s.change, full: s.full, trees: s.trees, relief: s.relief, ortho: s.ortho, orthoOp: s.orthoOp, roads: s.roads, roadsOp: s.roadsOp, subsidy: s.subsidy, z: Math.round(s.z), pin: s.pin }; }
function str(x) { try { return typeof x === 'string' ? x : JSON.stringify(x); } catch (e) { return String(x); } }
function sleep(ms) { return new Promise(r => setTimeout(r, ms)); }
function mulberry(a) { return function () { a |= 0; a = a + 0x6D2B79F5 | 0; let t = Math.imul(a ^ a >>> 15, 1 | a); t = t + Math.imul(t ^ t >>> 7, 61 | t) ^ t; return ((t ^ t >>> 14) >>> 0) / 4294967296; }; }
