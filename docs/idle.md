# Idle budget — what the process does when nobody asks

Goal: with no visitor and no peer writing, the process sits at ~0 CPU and a few kB/min of
network. Measured 2026-11-18 (atop + a 5 min CPU profile on the private tier's
`debug/pprof/profile`; the server's write timeout caps one profile at < 5 min).

| Loop | Cadence | Idle cost | Rule that keeps it cheap |
|---|---|---|---|
| obs-completeness pass (`ne_obs_complete.go`) | 3 h | seconds | memo identity (`neDeclaredDigest` + obs digest) is checked BEFORE the declared view is built; only a moved KG pays the decompress + merge. First pass after a restart is the full ~4 min. |
| reports scan (`neScanReportFiles`, 1 GB of JSON, ~40 s) | on demand | 0 | memo valid until an append (`dirty`) or the UTC day turns — no age expiry. |
| coverage keeper (`neCoverageKeepFresh`) | 1 min key check | ms | recompute ≥ 5 min apart while someone reads (15 min window), ≥ 1 h apart otherwise — a peer's nightly report run does not re-walk the reports every 5 min for nobody. |
| national table + sample (`neNatMaybeBuild`) | hourly check | conditional GETs | one ~10 min pass per 6 h at most, for a moved store and for a moved peer sample alike. |
| KG universe / `ne_ready` recheck (`kgUniversePeerLoop`) | 20 min | ~1 kB | one-row probe of the peer envelope; the full list (0.8 MB gz) only when hash / total / served / cells generation moved, or every 6 h. |
| fidx block manifest, peer samples | 10 min / 1 h | 304s | `If-None-Match`. |

Adding a loop: it must be O(changed) when nothing changed, and its row belongs here.

Triage of a CPU bump nobody caused: `atopsar -c` for the minute, then a profile over the next
occurrence (`curl -H "Authorization: Bearer …" "$P/debug/pprof/profile?seconds=280"`), read with
`go tool pprof -top -cum`. Loops log nothing when they find nothing.
