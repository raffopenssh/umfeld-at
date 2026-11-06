package main

import (
	"testing"
	"time"
)

// A memo whose owner opts out of stale-while-revalidate via StatsMemoAge must
// serve the FRESH value on the first call after the TTL elapsed (landing page
// coverage line: "load twice to see the current state" regression).
func TestStatsMemoAgeForcesSyncRefresh(t *testing.T) {
	key := "test_age_sync"
	n := 0
	get := func() int {
		force := false
		if age, ok := StatsMemoAge(key); ok && age >= time.Minute {
			force = true
		}
		n++
		v := StatsMemo(key, time.Minute, force, func() map[string]interface{} { return map[string]interface{}{"n": n} })
		return v["n"].(int)
	}
	if got := get(); got != 1 {
		t.Fatalf("first: %d", got)
	}
	if got := get(); got != 1 {
		t.Fatalf("fresh cached: %d", got)
	}
	statsMemoMapMu.Lock()
	statsMemoMap[key].computedAt = time.Now().Add(-2 * time.Minute) // idle for a while
	statsMemoMapMu.Unlock()
	if got := get(); got != 3 {
		t.Fatalf("stale must recompute synchronously on the first call, got %d", got)
	}
}

// While the first (synchronous) compute of a key runs, other callers must not
// start a second one: they get an empty value flagged stats_refreshing. And a
// force while a refresh runs is likewise skipped. (2026-11-06: the landing
// page blocked 30 s on the coverage walk; a second walk in parallel doubled it.)
func TestStatsMemoNeverTwoComputes(t *testing.T) {
	key := "test_single_compute"
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan map[string]interface{}, 1)
	go func() {
		done <- StatsMemo(key, time.Minute, false, func() map[string]interface{} {
			close(started)
			<-release
			return map[string]interface{}{"n": 1}
		})
	}()
	<-started
	calls := 0
	second := StatsMemo(key, time.Minute, true, func() map[string]interface{} { calls++; return map[string]interface{}{"n": 2} })
	if calls != 0 || second["stats_refreshing"] != true || second["n"] != nil {
		t.Fatalf("concurrent caller must get an empty refreshing value, got %v calls=%d", second, calls)
	}
	close(release)
	if v := <-done; v["n"] != 1 {
		t.Fatalf("first compute: %v", v)
	}
	if v := StatsMemo(key, time.Minute, false, nil); v["n"] != 1 || v["stats_refreshing"] == true {
		t.Fatalf("after compute: %v", v)
	}
	StatsMemoTouch(key)
	if age, _ := StatsMemoAge(key); age > time.Second {
		t.Fatalf("touch must reset age, got %s", age)
	}
}
