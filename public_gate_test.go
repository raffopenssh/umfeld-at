package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func publicGateReset(c publicGateCfg) {
	publicGateOnce.Do(func() {})
	publicGateMu.Lock()
	publicGate = c
	publicGateIdents = map[string]*publicIdent{}
	publicGateLimited = map[string]int64{}
	publicGateDayByte, publicGateWould = 0, 0
	publicGateMu.Unlock()
}

func TestPublicGateBuckets(t *testing.T) {
	cfg := publicGateCfg{enforce: true, rps: 10, burst: 5, bytesBurst: 1000, bytesPerSec: 10, cpuBurst: 2, cpuPerSec: 1, maxInflight: 3, globalDayByte: 5000}
	publicGateReset(cfg)
	defer publicGateReset(publicGateCfg{enforce: false, rps: 1e9, burst: 1e9, bytesBurst: 1e18, bytesPerSec: 1, cpuBurst: 1e9, cpuPerSec: 1, maxInflight: 1 << 30, globalDayByte: 1 << 60})
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

	// in flight cap
	var es []*publicIdent
	for i := 0; i < 3; i++ {
		e, why, _ := publicGateAdmit("a", now)
		if why != "" {
			t.Fatalf("admit %d: %s", i, why)
		}
		es = append(es, e)
	}
	if _, why, _ := publicGateAdmit("a", now); why != "inflight" {
		t.Fatalf("want inflight, got %q", why)
	}
	for _, e := range es {
		publicGateDone(e, 100, 0)
	}
	// requests: burst 5, 3 used (+ the refused one is not charged)
	for i := 0; i < 2; i++ {
		e, why, _ := publicGateAdmit("a", now)
		if why != "" {
			t.Fatalf("burst %d: %s", i, why)
		}
		publicGateDone(e, 0, 0)
	}
	if _, why, wait := publicGateAdmit("a", now); why != "requests" || wait < 1 {
		t.Fatalf("want requests, got %q %d", why, wait)
	}
	// bytes: debt allowed once, then refused until refilled
	now = now.Add(time.Second)
	e, why, _ := publicGateAdmit("b", now)
	if why != "" {
		t.Fatal(why)
	}
	publicGateDone(e, 1500, 0) // 500 in debt → 50 s at 10 B/s
	if _, why, wait := publicGateAdmit("b", now); why != "bytes" || wait < 50 || wait > 52 {
		t.Fatalf("want bytes ~50s, got %q %d", why, wait)
	}
	if e, why, _ = publicGateAdmit("b", now.Add(52*time.Second)); why != "" {
		t.Fatalf("after refill: %s", why)
	}
	publicGateDone(e, 0, 0)
	// compute
	e, _, _ = publicGateAdmit("c", now)
	publicGateDone(e, 0, 5*time.Second)
	if _, why, _ := publicGateAdmit("c", now); why != "compute" {
		t.Fatalf("want compute, got %q", why)
	}
	// global valve: day total over the cap sheds identities below 7/8 only
	publicGateMu.Lock()
	publicGateDayByte = 6000
	publicGateMu.Unlock()
	e, why, _ = publicGateAdmit("fresh", now)
	if why != "" {
		t.Fatalf("fresh client under global pressure: %s", why)
	}
	publicGateDone(e, 300, 0)
	if _, why, _ := publicGateAdmit("fresh", now); why != "global" {
		t.Fatalf("want global, got %q", why)
	}
	// count-only mode admits everything
	cfg.enforce = false
	publicGateReset(cfg)
	for i := 0; i < 20; i++ {
		e, why, _ := publicGateAdmit("d", now)
		if why != "" {
			t.Fatalf("unenforced refused: %s", why)
		}
		publicGateDone(e, 1e6, time.Minute)
	}
	if publicGateWould == 0 {
		t.Fatal("unenforced refusals not counted")
	}
}

func TestPublicGateMiddleware(t *testing.T) {
	publicGateReset(publicGateCfg{enforce: true, rps: 1, burst: 2, bytesBurst: 1e9, bytesPerSec: 1, cpuBurst: 1e9, cpuPerSec: 1, maxInflight: 10, globalDayByte: 1 << 60})
	defer publicGateReset(publicGateCfg{enforce: false, rps: 1e9, burst: 1e9, bytesBurst: 1e18, bytesPerSec: 1, cpuBurst: 1e9, cpuPerSec: 1, maxInflight: 1 << 30, globalDayByte: 1 << 60})
	h := egressMiddleware(publicGateMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })))
	do := func(path string, hdr map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.RemoteAddr = "127.0.0.1:1"
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	x := map[string]string{"X-Forwarded-For": "6.6.6.6, 198.51.100.7"}
	for i := 0; i < 2; i++ {
		if w := do("/api/v1/kgs", x); w.Code != 200 {
			t.Fatalf("hit %d: %d", i, w.Code)
		}
	}
	w := do("/api/v1/kgs", x)
	if w.Code != 429 || w.Header().Get("Retry-After") == "" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("want 429 + Retry-After, got %d %v", w.Code, w.Header())
	}
	// a spoofed leading XFF entry does not buy a new bucket
	if w := do("/api/v1/kgs", map[string]string{"X-Forwarded-For": "9.9.9.9, 198.51.100.7"}); w.Code != 429 {
		t.Fatalf("spoofed XFF got %d", w.Code)
	}
	// exempt: legal pages, token tiers, loopback without XFF
	if w := do("/datenschutz", x); w.Code != 200 {
		t.Fatalf("legal page gated: %d", w.Code)
	}
	if w := do("/api/v1/kgs", map[string]string{"X-Forwarded-For": "198.51.100.7", tierHeader: "private"}); w.Code != 200 {
		t.Fatalf("private tier gated: %d", w.Code)
	}
	if w := do("/api/v1/kgs", nil); w.Code != 200 {
		t.Fatalf("loopback gated: %d", w.Code)
	}
	// IPv6: one /64 = one bucket
	a := map[string]string{"X-Forwarded-For": "2001:db8:1:2::1"}
	b := map[string]string{"X-Forwarded-For": "2001:db8:1:2:ffff::9"}
	do("/", a)
	do("/", b)
	if w := do("/", a); w.Code != 429 {
		t.Fatalf("IPv6 /64 not shared: %d", w.Code)
	}
	if got := neClientNet("2001:db8:1:2:ffff::9"); got != "2001:db8:1:2::/64" {
		t.Fatalf("neClientNet = %q", got)
	}
}
