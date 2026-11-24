package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A retry with the same X-Idem key gets the first answer back and the peer is written once —
// also when the first reader hung up before the answer, and when two copies race.
func TestTreeReportIdem(t *testing.T) {
	var n atomic.Int64
	status := atomic.Int64{}
	status.Store(201)
	gate := make(chan struct{})
	close(gate)
	var gmu sync.Mutex
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := n.Add(1)
		gmu.Lock()
		g := gate
		gmu.Unlock()
		<-g
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(status.Load()))
		w.Write([]byte(`{"rev":` + string(rune('0'+c)) + `}`))
	}))
	defer peer.Close()
	oldURL, oldKey := treeReportURL, treeReportKey
	treeReportURL = func() string { return peer.URL }
	treeReportKey = func() string { return "trk_test" }
	defer func() { treeReportURL, treeReportKey = oldURL, oldKey }()
	post := func(ctx context.Context, key, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/api/v1/trees/report", strings.NewReader(body)).WithContext(ctx)
		if key != "" {
			r.Header.Set("X-Idem", key)
		}
		handleV1TreeReport(w, r)
		return w
	}
	bg := context.Background()
	undo := `{"client_id":"u","retract":"last"}`
	a := post(bg, "key-00000001", undo)
	b := post(bg, "key-00000001", undo)
	if a.Code != 201 || b.Code != 201 || a.Body.String() != b.Body.String() || b.Header().Get("X-Idem-Replayed") != "1" || n.Load() != 1 {
		t.Fatalf("replay: %d %q / %d %q, peer calls %d", a.Code, a.Body.String(), b.Code, b.Body.String(), n.Load())
	}
	// same key, another body = another request; no key = never remembered; a bad key is no key
	if post(bg, "key-00000001", `{"client_id":"v","retract":"last"}`); n.Load() != 2 {
		t.Fatalf("other body under the same key must be sent, peer calls %d", n.Load())
	}
	post(bg, "", undo)
	post(bg, "", undo)
	post(bg, "bad key!", undo)
	if n.Load() != 5 {
		t.Fatalf("unkeyed requests are always sent, peer calls %d", n.Load())
	}
	// a 5xx / 429 is not an answer to keep: the retry goes to the peer again
	status.Store(503)
	post(bg, "key-00000002", undo)
	status.Store(201)
	if w := post(bg, "key-00000002", undo); w.Code != 201 || n.Load() != 7 || w.Header().Get("X-Idem-Replayed") != "" {
		t.Fatalf("retry after 503: %d, peer calls %d", w.Code, n.Load())
	}
	// the reader hangs up while the peer is slow: the write finishes, the retry (racing it) gets its answer
	gmu.Lock()
	gate = make(chan struct{})
	g := gate
	gmu.Unlock()
	ctx, cancel := context.WithCancel(bg)
	first := make(chan int, 1)
	go func() { first <- post(ctx, "key-00000003", undo).Code }()
	for n.Load() < 8 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	second := make(chan *httptest.ResponseRecorder, 1)
	go func() { second <- post(bg, "key-00000003", undo) }()
	time.Sleep(30 * time.Millisecond)
	close(g)
	w := <-second
	<-first
	if w.Code != 201 || n.Load() != 8 || w.Header().Get("X-Idem-Replayed") != "1" {
		t.Fatalf("hang-up + retry: %d replayed=%q, peer calls %d (want 8)", w.Code, w.Header().Get("X-Idem-Replayed"), n.Load())
	}
	if st := treeIdemStats(); st["entries"].(int) < 3 || st["bytes"].(int) <= 0 {
		t.Fatalf("stats: %v", st)
	}
}
