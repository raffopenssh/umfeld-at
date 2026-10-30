# Memory Contract — no unbounded per-KG state (read before caching anything)

**The rule this came from.** An earlier version kept an unbounded `sync.Map` of
decoded per-KG documents (~35 MB each); a client walking KGs pinned one per
request until the heap passed `GOMEMLIMIT` into swap and a flat-traffic box
spent a day at 96 % disk busy serving 10 ms requests in 300–886 s. `GOMEMLIMIT`
is a *soft* limit and cannot free live data — only a bound on the cache can.
The fix was a 4-entry LRU with the same API; the lesson is general.

**Safety net** (`umfeld-at.service`): `MemoryMax=5G`, `MemorySwapMax=256M`,
`OOMPolicy=kill`, `Restart=always`, `GOMEMLIMIT=3GiB`. A leak costs a ~15 s
restart, not a day of swap thrash.

Rules:
1. **Every process-wide cache keyed by KG, cell, bin or tile needs a bound**
   (entries or bytes) and a size line on
   `$UMFELD_PRIVATE_PREFIX/api/v1/debug/inflight` (`debug_pprof.go`). There are
   7 850 KGs and millions of cells; "one entry per KG" is a leak with a delay.
   Current lines: `kg_universe`, `kg_bbox_diff`, `ne_manifest`, `ne_store`,
   `ne_change`, `ne_declared_view`, `ne_reports`, `ne_revisit`, `ne_glyph`,
   `ne_glyph_admin`, `ne_glyph_tiles`, `ne_glyph_sub`, `ne_glyph_dif`,
   `ne_passes`, `ne_fidx` (48 MB RAM + 2 GB disk), `ne_stats` (16 MB; unit
   envelopes + peer answers, 10 min), `ne_mvt`, `ne_mvt_gate`, `export_gate`
   (≤ 2 000 identities, idle swept), `osm_remote_grid`.
2. **Don't trust the CPU graph; read the memory numbers.** `sys ≫ user` with
   high iowait is memory pressure, not compute. Swap-used trend is the earliest
   signal; `RssAnon` in `/proc/<pid>/status` is the process-side number.
3. **Diff heap profiles, don't guess.** `curl localhost:8000/debug/pprof/heap
   -o h.pb.gz; go tool pprof -top -inuse_space ./umfeld-at h.pb.gz`, then
   `-peek='main.<Func>$'` to see who retains it. Reproduce with a replay before
   and after.
