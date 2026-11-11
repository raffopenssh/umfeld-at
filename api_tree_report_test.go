package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The relay must add the key server-side, keep reports on the tree layer,
// label them, and hand the peer's status + body back unchanged.
func TestTreeReportRelay(t *testing.T) {
	var got map[string]interface{}
	var gotKey string
	status := 201
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Reporter-Key")
		b, _ := io.ReadAll(r.Body)
		got = nil
		json.Unmarshal(b, &got)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(`{"rev":7,"tree":{"taxon":"Picea abies","dist_m":0}}`))
	}))
	defer peer.Close()
	oldURL, oldKey := treeReportURL, treeReportKey
	treeReportURL = func() string { return peer.URL }
	treeReportKey = func() string { return "trk_test" }
	defer func() { treeReportURL, treeReportKey = oldURL, oldKey }()

	post := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		handleV1TreeReport(w, httptest.NewRequest("POST", "/api/v1/trees/report", strings.NewReader(body)))
		return w
	}
	w := post(`{"lon":15.1,"lat":47.1,"species":"Fichte","reporter":"abc","source":"evil","reporter_key":"trk_other"}`)
	if w.Code != 201 || !strings.Contains(w.Body.String(), `"dist_m":0`) {
		t.Fatalf("relay: %d %s", w.Code, w.Body.String())
	}
	if gotKey != "trk_test" || got["source"] != treeReportSource || got["layer"] != "trees" || got["claim"] != "tree" || got["reporter"] != "abc" || got["reporter_key"] != nil {
		t.Fatalf("forwarded key=%q body=%v", gotKey, got)
	}
	if strings.Contains(w.Body.String(), "trk_") {
		t.Fatal("key leaked")
	}
	for _, bad := range []string{`{"cell":"8c1e","layer":"cells.cover","claim":"wrong_type"}`, `[1]`, `{"reports":[]}`, `{"species":"` + strings.Repeat("x", 80) + `"}`} {
		got = map[string]interface{}{"untouched": true}
		if w := post(bad); w.Code != 400 || got["untouched"] == nil {
			t.Fatalf("%s → %d, forwarded=%v", bad, w.Code, got)
		}
	}
	if w := post(`{"reports":[{"lon":1,"lat":2,"layer":"trees","claim":"missing","client_id":"u"}]}`); w.Code != 201 || got["reports"] == nil {
		t.Fatalf("batch: %d %v", w.Code, got)
	}
	// undo one step and a flag withdraw go through as they are (peer contract 2026-11-11)
	if w := post(`{"client_id":"u","retract":"last","reporter":"abc"}`); w.Code != 201 || got["retract"] != "last" || got["claim"] != "tree" {
		t.Fatalf("undo: %d %v", w.Code, got)
	}
	if w := post(`{"lon":15.1,"lat":47.1,"layer":"trees","claim":"spurious","retract":true,"reporter":"abc"}`); w.Code != 201 || got["retract"] != true || got["claim"] != "spurious" {
		t.Fatalf("flag withdraw: %d %v", w.Code, got)
	}
	status = 409
	if w := post(`{"lon":15.1,"lat":47.1,"dbh_cm":40}`); w.Code != 409 {
		t.Fatalf("409 must pass through, got %d", w.Code)
	}
	status = 401
	if w := post(`{"lon":15.1,"lat":47.1,"dbh_cm":40}`); w.Code != 502 || strings.Contains(w.Body.String(), "key") {
		t.Fatalf("a rejected key is our fault, not the reader's: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	handleV1TreeReport(w, httptest.NewRequest("GET", "/api/v1/trees/report", nil))
	if w.Code != 405 {
		t.Fatalf("GET → %d", w.Code)
	}
}
