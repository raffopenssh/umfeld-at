# Peer prompt — cell lists on `/api/v1/stats/unit` (filtered units)

Paste to the observed-layer peer and to the subsidy peer. Contract text: `docs/stats-envelope.md` §6
(served at `{PUBLIC_URL}/api/v1/docs/stats-envelope.md`).

---

Add a POST form of your `GET /api/v1/stats/unit` that takes an explicit cell set instead of a unit key.
umfeld-at calls it for every *filtered* unit (its `?f=` grammar spans all three services' columns, so only
it knows which cells match); today those requests get 404/405 and the row shows `available=false`.

```
POST /api/v1/stats/unit?format=json&unit=cells[&scale=49][&year=YYYY]
Content-Type: application/octet-stream
Content-Encoding: gzip
body (after gunzip): "NEI1" · u32 LE n · n × u64 LE H3 res-12 cell ids, sorted ascending
```

Answer = your usual `stats-envelope/1` over exactly these cells:
- `unit.kind="cells"`, `unit.key` = first 16 hex of sha256 of the raw body, `unit.res=12`,
  `unit.cells_n = n × scale`, `unit.area_ha = Σ CellAreaM2(id)/1e4 × scale`
- every `_ha` / `_n` / `_eur` figure summed over the listed cells and multiplied by `scale`;
  `_pct` and means unweighted (scale cancels); `coverage.cells_known` = listed cells you hold a row for
  (× scale), `coverage.complete = cells_known == cells_n`; unknown ids are simply absent, never an error
- `scale` (default 1) is 49 when the list is umfeld-at's national 1-in-49 sample: each id is the centre
  res-12 child of a res-10 cell and stands for its 48 siblings — multiply, or if cheaper read the
  siblings' own rows (`h3.CellToChildren(parent10, 12)`) and report `coverage.note` accordingly
- border weighting of `kg=` does not apply: each listed cell counts 1
- limits: ≤ 1 000 000 ids (8 MB raw, ~1–3 MB gzip'd); 413 above; 400 on bad magic/length;
  405/415 stay for anything that is not this body; same rate limits and `keys=`/`flat`/`csv` as GET
- `year=` as on GET (subsidy peer); ignore otherwise
- optional: for lists that take > ~5 s answer `202` `{"status":"computing","retry_after_s":N}` +
  `Retry-After: N` quickly (cache by the body hash before aggregating) — umfeld-at re-POSTs the same
  body after N s for up to ~25 s; answer the status in well under a second, not after a full re-parse

Status (both peers implemented this): verified live — 7 ids of a res-10 cell ≡ `h3=` unit at both;
a 183 k-id `scale=49` list answers 200 at the subsidy peer in ~1 s and 202→200 at the observed-layer
peer after ~15 s.

Implementation hint (Python): `ids = np.frombuffer(gzip.decompress(body)[8:], '<u8')`; join on your
per-cell table by id (sorted → `searchsorted`), then the same aggregation code as a `bbox=` unit.
Cache by `unit.key` + scale + year for 10 min. Document the form in your envelope docs and add one
test: 7 ids of one res-10 cell with scale=1 vs the parent's res-10 `h3=` unit → identical `_ha` sums.

Done when `curl -X POST --data-binary @cells.nei1.gz -H 'Content-Encoding: gzip' -H 'Content-Type: application/octet-stream' '…/api/v1/stats/unit?format=flat&unit=cells'` returns a row, and umfeld-at's
`/api/v1/ne/stats/unit?kg=<kg>&f=d=wald&format=flat` shows your columns with `available=true`.
