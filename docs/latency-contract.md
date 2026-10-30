# Latency & Availability Contract (read before adding an endpoint)

The rules below were learnt the hard way on an earlier, much larger version of
this service (every DB-backed endpoint timing out while static ones answered in
120 µs). Every cause found with `/debug/pprof/goroutine?debug=2` was
**self-inflicted blocking**, not slow SQLite. Violating them reintroduces the
wedge.

**1. Never block a request on cold data.**
Anything that must be fetched, parsed or built before it can be served
(NE container materialisation, fidx bins, national MVT table, peer answers)
is served from what is there, scheduled in the background and reported as
`ready:false` / `pending:true` / `status` — single-object reads may wait a
few seconds at most, viewport / batch / bulk paths wait **0**.

**2. Serve what you have, queue the rest, and say so.**
A partial answer is `complete:false` with the gap described (`missing{}`,
`pending`, `kgs_warming`); clients retry and converge. `{"wait":true}` (where
offered) opts back into blocking. Any new endpoint touching lazily-built data
must follow this shape. `StatsMemo` (`stats_memo.go`) is the stale-while-revalidate
primitive for expensive aggregates.

**3. No per-row or per-point SQL in a loop.**
One query per item with a small pool starves the whole process. KG lookups by
point/bbox go through the in-memory KG bbox index (`kg_bbox_index.go`, ~7.8k
boxes, zero connections); sets of codes go through `ResolveKGCodes` →
`kg_code IN (…)`; whole-table facts are read once and memoised.

**4. Isolate a bbox predicate from `GROUP BY` in the same SELECT.**
Grouping alongside LEFT JOINs makes SQLite pick the PK index and full-scan;
select ids in a CTE first, then join.

**5. Size the pool for readers.** WAL allows unlimited concurrent readers, but
a Go pool capped at 4 blocks everything in `database/sql.(*DB).conn`.
`searchIndexMaxConns = 24` (`search_index.go`). Note `_cache_size` is **per
connection** — pool size × cache size is the real memory budget
(24 × 8 MB ≈ 192 MB).

## Memory

`GOMEMLIMIT` in the unit file caps transient growth; it is a *soft* limit and
cannot free live data, so every cache needs its own bound
(`docs/memory-contract.md`).

## Triage a suspected wedge

```
curl -H "Authorization: Bearer $TOK" localhost:8000$UMFELD_PRIVATE_PREFIX/api/v1/debug/inflight   # stuck URLs + ages, pool stats, cache sizes
curl 'localhost:8000/debug/pprof/goroutine?debug=2'                                               # where they're blocked
```
Many goroutines in `database/sql.(*DB).conn` ⇒ pool starvation (rule 3/5).
Many in a `time.Sleep` loop inside a build/ensure call ⇒ rule 1.

# Background work

Heavy builds (NE conversion, fidx bins, national MVT table, PMTiles archives)
run on single-writer loops / job workers with bounded concurrency so a core
stays free for request handlers (`ne_mvt_jobs.go`, `neFidxWarmLoop`). Long
CLI passes (`-osm-import`, `-toponyms-import`, `-addresses-import`,
`-kg-universe-build`, `-ne-train-dict`) run as a **separate process**; the
live server picks the result up on reload / restart. In-memory ready-sets are
per process — a cross-process "is it built?" check must read the artefact
(file, table), never a process variable.

# SQLite Connection Tuning

`sqlite_driver.go` registers `sqlite3_tuned` (used by the search index and
`osm_proximity.db`) because go-sqlite3's DSN cannot express `mmap_size`:
`mmap_size=4 GiB` + `temp_store=MEMORY`. Without it `sqlite3_step` spent most of
its time copying pages out of the OS page cache into 24 private per-connection
caches; the mmap is shared and kernel-reclaimable. `_cache_size` remains
**per connection**.
