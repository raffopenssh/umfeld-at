// stats_memo.go — stale-while-revalidate memoisation for expensive "stats"
// endpoints.
//
// Why not a plain TTL memo
// ───────────────────────
// `/api/v1/footprints/link/stats` already had a 5-minute TTL memo, yet the
// 24-hour access log still showed a **3.5 s average** for it. The reason is
// that a plain TTL memo makes exactly one unlucky caller per period pay the
// full recompute — here five COUNT/COUNT(DISTINCT)/GROUP BY passes over 5.1M
// rows — and it did so while HOLDING the memo mutex, so every concurrent
// caller queued behind it. `/api/v1/osm/stats` (4.4 s avg, 14 s max) had the
// same shape with no memo at all.
//
// These numbers are informational (progress counters, readiness). Serving a
// value that is a few minutes old is strictly better than making a request
// wait seconds for a fresher one. So:
//
//	fresh   (< ttl)        → return cached, no work
//	stale   (>= ttl)       → return cached IMMEDIATELY, refresh in background
//	missing (first call)   → compute synchronously (nothing else to serve)
//
// Only one refresh per key runs at a time, so a polling dashboard cannot
// stack recomputes.
package main

import (
	"sync"
	"time"
)

type statsMemoEntry struct {
	val        map[string]interface{}
	computedAt time.Time
	refreshing bool
}

var (
	statsMemoMapMu sync.Mutex
	statsMemoMap   = map[string]*statsMemoEntry{}
)

// StatsMemo returns a memoised stats map for key.
//
// compute must be safe to call from a background goroutine and must not depend
// on the calling request's context (it may outlive it).
//
// The returned map also carries `stats_computed_at` and `stats_age_seconds` so
// callers can see how old the snapshot is, plus `stats_refreshing` while a
// background recompute is in flight. Pass force=true (e.g. ?refresh=1) to
// recompute synchronously.
func StatsMemo(key string, ttl time.Duration, force bool, compute func() map[string]interface{}) map[string]interface{} {
	statsMemoMapMu.Lock()
	e, ok := statsMemoMap[key]
	if !ok {
		e = &statsMemoEntry{}
		statsMemoMap[key] = e
	}
	haveValue := e.val != nil
	age := time.Since(e.computedAt)
	needSync := force || !haveValue
	needAsync := !needSync && age >= ttl && !e.refreshing
	if needAsync {
		e.refreshing = true
	}
	cached, computedAt, refreshing := e.val, e.computedAt, e.refreshing
	statsMemoMapMu.Unlock()

	if needSync {
		val := compute()
		statsMemoMapMu.Lock()
		e.val, e.computedAt, e.refreshing = val, time.Now(), false
		cached, computedAt, refreshing = e.val, e.computedAt, false
		statsMemoMapMu.Unlock()
	} else if needAsync {
		go func() {
			val := compute()
			statsMemoMapMu.Lock()
			e.val, e.computedAt, e.refreshing = val, time.Now(), false
			statsMemoMapMu.Unlock()
		}()
	}

	out := make(map[string]interface{}, len(cached)+3)
	for k, v := range cached {
		out[k] = v
	}
	out["stats_computed_at"] = computedAt.UTC().Format(time.RFC3339)
	out["stats_age_seconds"] = int(time.Since(computedAt).Seconds())
	if refreshing {
		out["stats_refreshing"] = true
	}
	return out
}

// StatsMemoAge reports how old the cached value for key is (0, false when
// nothing is cached yet). Lets a cheap computation opt out of
// stale-while-revalidate: `StatsMemo(key, ttl, age >= ttl, …)` serves fresh
// on the FIRST request after an idle period instead of handing the stale
// value out once and refreshing behind it.
func StatsMemoAge(key string) (time.Duration, bool) {
	statsMemoMapMu.Lock()
	defer statsMemoMapMu.Unlock()
	e, ok := statsMemoMap[key]
	if !ok || e.val == nil {
		return 0, false
	}
	return time.Since(e.computedAt), true
}
