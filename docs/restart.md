# Restarts — no refused connection, ready in seconds

We deploy many times a day (build in place + restart). A restart must not cost a
visitor or a peer a failed request, and must cost no extra disk or bandwidth.

## How a restart goes

| Step | What happens | Where |
|---|---|---|
| socket | `umfeld-at.socket` owns `:8000` for the life of the machine. While the service is down the kernel still completes handshakes into the backlog (4096); requests wait, none is refused, no client retries (= no re-sent bytes). | `umfeld-at.socket`, `startupListen` |
| drain | SIGTERM → `STOPPING=1`, stop accepting, in-flight requests get `startupDrain` (10 s) to finish, then exit. Unit `TimeoutStopSec=20`. | `startupServe` |
| init | search index, then the three heavy loads in parallel (national sample + cell table · Natura 2000 · NE store, itself fanned out per KG over the cores), then route registration. | `main.go`, `neStoreT.load` |
| ready | `READY=1` (unit `Type=notify`) when the accept loop runs; the queued connections are answered. `systemctl restart` returns here — or fails after `TimeoutStartSec=120`. | `startupServe` |
| after | warmers and periodic loops (`startBackground`) start only now, so they do not compete with init for the cores. | `main.go` |
| watchdog | every 30 s the process sends itself a request through the accept loop (per-process nonce header, answered before tiers/gate/log) and pings systemd; 90 s without → restart. | `startupWatchdog`, `WatchdogSec` |

Outside systemd (tests, a dev run with `PORT=`) there is no socket unit: the process binds
before init, drains the same way, and the notify/watchdog calls are no-ops.

## Deploy

```
scripts/deploy.sh        # build → umfeld-at.new, keep umfeld-at.prev, swap, restart, verify, roll back
```

Prints the restart time and the `⏱ startup:` lines. A build error never touches the running
service; a binary that does not come up ready is replaced by `umfeld-at.prev` (the failed one
stays as `umfeld-at.failed`). `sudo systemctl restart umfeld-at` after a plain build is
equally safe for clients — it just has no rollback.

## Budget and triage

```
journalctl -u umfeld-at -n 300 | grep -E 'startup:|ready in|drained|watchdog'
```

- `✅ ready in N s` — exec → accepting. Budget: **≤ 5 s** on the 2-core machine with a warm
  page cache (measured in the table below). Anything above is a regression: the `⏱ startup:`
  lines (stages ≥ 100 ms) name the stage.
- `🛑 terminated: drained in …` — how long the old process took to finish its requests.
- Client-visible stall of a restart = drain + ready. Nothing is refused in between.

Rules for new code:

- Nothing slow in `main` before `startupServe`. A new load that requests need → a step in
  `startupParallel` (must be once-guarded and independent of the other steps). Everything
  else (warmers, loops, peer calls) → `startBackground`.
- No network I/O before ready — a slow peer must never hold a restart.
- No start-up caches on disk to buy speed: the loads are parallel, not persisted twice.
- Do not bind a second listener or call `http.ListenAndServe`; the socket is `startupListen`'s.

## Measurements

