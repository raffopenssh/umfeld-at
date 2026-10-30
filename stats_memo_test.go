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
