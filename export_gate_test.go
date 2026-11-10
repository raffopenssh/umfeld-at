package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExportGateEnforced(t *testing.T) {
	exportLoadTiers()
	was := exportEnforce
	exportEnforce = true
	defer func() { exportEnforce = was }()
	codes := map[int]int{}
	var last *httptest.ResponseRecorder
	for i := 0; i < 15; i++ { // anon burst is 10
		rr := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/v1/ne/stats/unit?kg=63330", nil)
		r.Header.Set("X-Forwarded-For", "203.0.113.200")
		if _, ok := exportGate(rr, r); !ok {
			codes[rr.Code]++
			last = rr
		}
	}
	if codes[http.StatusTooManyRequests] < 4 || last.Header().Get("Retry-After") == "" {
		t.Fatalf("anon burst not enforced: %v", codes)
	}
	for i := 0; i < 30; i++ { // loopback (operator) is counted, never refused
		r := httptest.NewRequest("GET", "/x", nil)
		r.RemoteAddr = "127.0.0.1:9"
		if _, ok := exportGate(httptest.NewRecorder(), r); !ok {
			t.Fatal("loopback refused")
		}
	}
}

func TestFeedbackPublicHidesTraces(t *testing.T) {
	if err := initFeedbackDB(); err != nil {
		t.Skip(err)
	}
	title := "trace public-hide-test"
	if _, err := feedbackDB.Exec(`INSERT INTO feedback (agent, category, severity, title, description, endpoint, context) VALUES ('glyph-trace','other','low',?,'','','{"ua":"secret-ua"}')`, title); err != nil {
		t.Skip(err)
	}
	defer feedbackDB.Exec(`DELETE FROM feedback WHERE title = ?`, title)
	get := func(private bool) string {
		r := httptest.NewRequest("GET", "/api/v1/feedback?agent=glyph-trace&limit=1000", nil)
		if private {
			r.Header.Set(tierHeader, "private")
		}
		w := httptest.NewRecorder()
		feedbackHandler(w, r)
		return w.Body.String()
	}
	if strings.Contains(get(false), title) || strings.Contains(get(false), "secret-ua") {
		t.Fatal("trace record on the public list")
	}
	if !strings.Contains(get(true), title) {
		t.Fatal("trace record missing on the private tier")
	}
	// triage writes are private
	w := httptest.NewRecorder()
	feedbackHandler(w, httptest.NewRequest("PATCH", "/api/v1/feedback/1", strings.NewReader(`{"status":"wontfix"}`)))
	if w.Code != http.StatusNotFound {
		t.Fatalf("public PATCH = %d", w.Code)
	}
}
