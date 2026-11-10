package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPeerUserAgent(t *testing.T) {
	for in, comp := range map[string]string{
		"":                              "go",
		"Go-http-client/1.1":            "go",
		"umfeld-at glyph":               "glyph",
		"umfeld-at/stats-unit":          "stats-unit",
		"umfeld-at/sample (+https://x)": "sample",
		"umfeld-at glyph diff (lidar)":  "glyph",
	} {
		want := "umfeld-at/" + comp
		if p := publicURL(); p != "" {
			want += " (+" + p + ")"
		}
		if got := peerUserAgent(in); got != want {
			t.Errorf("%q → %q, want component %q", in, got, comp)
		}
	}
}

// Every client in the binary (default transport) gets the UA, and a 429 with
// Retry-After is retried — POST bodies replayed — instead of surfacing.
func TestPeerTransportRetries429(t *testing.T) {
	var hits int32
	var uas, bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		uas, bodies = append(uas, r.Header.Get("User-Agent")), append(bodies, string(b))
		w.Header().Set(peerHTTPTierHdr, peerHTTPTierWant)
		if atomic.AddInt32(&hits, 1) <= 2 {
			w.Header().Set("Retry-After", "0.05")
			w.WriteHeader(429)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	old := peerHTTPHost
	peerHTTPHost = func() string { return u.Host }
	defer func() { peerHTTPHost = old }()

	req, _ := http.NewRequest("POST", srv.URL+"/api/v1/stats/unit", bytes.NewReader([]byte("cells")))
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req) // no UA set by the caller
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "ok" || hits != 3 {
		t.Fatalf("status=%d body=%q hits=%d", resp.StatusCode, b, hits)
	}
	for i := range uas {
		if !strings.HasPrefix(uas[i], "umfeld-at/go") || bodies[i] != "cells" {
			t.Errorf("attempt %d: ua=%q body=%q", i, uas[i], bodies[i])
		}
	}
	if req.Header.Get("User-Agent") != "" {
		t.Error("caller's request was modified")
	}

	// a deadline shorter than Retry-After hands the 429 back instead of hanging
	atomic.StoreInt32(&hits, -100)
	peerHTTP.mu.Lock()
	peerHTTP.pauseUntil = time.Time{}
	peerHTTP.mu.Unlock()
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(429)
	})
	t0 := time.Now()
	resp, err = (&http.Client{Timeout: 2 * time.Second}).Get(srv.URL)
	if err != nil || resp.StatusCode != 429 || time.Since(t0) > time.Second {
		t.Fatalf("err=%v resp=%v after %v", err, resp, time.Since(t0))
	}
	resp.Body.Close()
	peerHTTP.mu.Lock()
	peerHTTP.pauseUntil = time.Time{}
	peerHTTP.mu.Unlock()
}
