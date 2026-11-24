# Idle budget — what the process does when nobody asks

Goal: with no visitor and no peer writing, the process sits at ~0 CPU and a few kB/min of
network. Measured 2026-11-18 (atop + a 5 min CPU profile on the private tier's
`debug/pprof/profile`; the server's write timeout caps one profile at < 5 min).

| Loop | Cadence | Idle cost | Rule that keeps it cheap |
|---|---|---|---|
| obs-completeness pass (`ne_obs_complete.go`) | 3 h | seconds | memo identity (`neDeclaredDigest` + obs digest) is checked BEFORE the declared view is built; only a moved KG pays the decompress + merge. First pass after a restart is the full ~4 min. |
| reports scan (`neScanReportFiles`, 1 GB of JSON, ~40 s) | on demand | 0 | memo valid until an append (`dirty`) or the UTC day turns — no age expiry. Readers never wait for a walk: a dirty memo is served stale, one background walk starts when it is > 10 min old; only the coverage keeper asks for the exact value (`neScanReportFilesFresh`). |
| coverage keeper (`neCoverageKeepFresh`) | 1 min key check | ms | recompute ≥ 5 min apart while someone reads (15 min window), ≥ 1 h apart otherwise — a peer's nightly report run does not re-walk the reports every 5 min for nobody. |
| national table + sample (`neNatMaybeBuild`) | hourly check | conditional GETs | one ~10 min pass per 6 h at most, for a moved store and for a moved peer sample alike. "Moved" survives a restart: the sample header is compared with the peer tags on disk (`neSampPeersBehind`). One exception per run: the peer sample gained apex lists and ours has none → pass at once. |
| national tree table (`neNatTreesLoop`, `ne_mvt_nat_trees.go`) | 3 h | one conditional GET (`If-None-Match` → 304) | the table itself (`/trees/hexes?res=9&format=bin`, ≈ 12 MB gzip, 2 s, folded to `data/ne_mvt/nat_trees.bin`) only when the peer's `gen` moved — at most daily; 15 min retry after a failure |
| KG universe / `ne_ready` recheck (`kgUniversePeerLoop`) | 20 min | ~1 kB | one-row probe of the peer envelope; the full list (0.8 MB gz) only when hash / total / served / cells generation moved, or every 6 h. |
| re-flown gate (`neAlsGateLoop`, `ne_als_gate.go`) | 1 h (10 min while the peer's year-range fix spreads) | two conditional GETs | the sample body (≈ 1.2 MB zstd) only when its generation moved; kept on disk as zstd, parsed once per start. |
| fidx block manifest, peer samples | 10 min / 1 h | 304s | `If-None-Match`. |
| trees by email (`treeMailLoop`, `tree_mail.go`) | 20 s | one `stat` | `Maildir/new` is listed only when its mtime moved or a mail waits in the queue; off entirely without `TREE_MAIL_KEYS`. |

Adding a loop: it must be O(changed) when nothing changed, and its row belongs here.

Triage of a CPU bump nobody caused: `atopsar -c` for the minute, then a profile over the next
occurrence (`curl -H "Authorization: Bearer …" "$P/debug/pprof/profile?seconds=280"`), read with
`go tool pprof -top -cum`. Loops log nothing when they find nothing.
