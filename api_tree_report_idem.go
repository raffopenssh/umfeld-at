// api_tree_report_idem.go — exactly-once for the tree-report relay.
//
// The glyph keeps every tree report in an outbox on the phone and sends it
// until an answer arrives (ne_glyph_outbox.go). On a weak signal the request
// may reach us while the answer is lost; the retry then carries the same
// `X-Idem` key and gets the FIRST answer back instead of a second write at the
// peer (an undo sent twice would take two steps back).
//
// Keyed by X-Idem + the body's hash (a key reused with another body is a new
// request). Only final answers are kept — never a 429, a 5xx or a transport
// error: those are retried for real. A keyed write is detached from the
// reader's connection: once started it finishes at the peer and is remembered,
// whether or not the reader is still there. Bounds: treeIdemMax entries,
// treeIdemBytes of bodies, treeIdemTTL; in memory only (after a restart the
// peer's own folding of repeats is the fallback). Sizes on debug/inflight →
// tree_report.idem.
package main

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

const (
	treeIdemMax   = 4000
	treeIdemBytes = 8 << 20
	treeIdemTTL   = 24 * time.Hour
	treeIdemBody  = 64 << 10 // an answer larger than this is not kept
)

type treeIdemEntry struct {
	key    string
	status int
	body   []byte
	ra     string
	at     time.Time
	el     *list.Element
}

var treeIdem = struct {
	mu       sync.Mutex
	m        map[string]*treeIdemEntry
	lru      *list.List // front = newest
	bytes    int
	flight   map[string]chan struct{}
	hits     int64
	joined   int64
	detached int64
}{m: map[string]*treeIdemEntry{}, lru: list.New(), flight: map[string]chan struct{}{}}

// treeIdemKey — "" when the header is absent or not a plain token (8–64 of [A-Za-z0-9-]).
func treeIdemKey(h string, body []byte) string {
	if len(h) < 8 || len(h) > 64 {
		return ""
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-') {
			return ""
		}
	}
	s := sha256.Sum256(body)
	return h + ":" + hex.EncodeToString(s[:8])
}

// treeIdemBegin — the remembered answer (hit), or the right to make the request:
// done() must be called when it is over. While another request with the same key
// is in flight the caller waits for it (≤ wait) and then sees its answer.
func treeIdemBegin(key string, wait time.Duration) (e *treeIdemEntry, done func()) {
	deadline := time.Now().Add(wait)
	for {
		treeIdem.mu.Lock()
		if x := treeIdem.m[key]; x != nil {
			if time.Since(x.at) < treeIdemTTL {
				treeIdem.hits++
				treeIdem.lru.MoveToFront(x.el)
				treeIdem.mu.Unlock()
				return x, nil
			}
			treeIdemDropLocked(x)
		}
		ch := treeIdem.flight[key]
		if ch == nil {
			ch = make(chan struct{})
			treeIdem.flight[key] = ch
			treeIdem.mu.Unlock()
			return nil, func() {
				treeIdem.mu.Lock()
				delete(treeIdem.flight, key)
				treeIdem.mu.Unlock()
				close(ch)
			}
		}
		treeIdem.joined++
		treeIdem.mu.Unlock()
		d := time.Until(deadline)
		if d <= 0 {
			return nil, func() {} // the first is still out: this one goes too (the peer folds a repeat)
		}
		select {
		case <-ch:
		case <-time.After(d):
		}
	}
}

func treeIdemDropLocked(x *treeIdemEntry) {
	delete(treeIdem.m, x.key)
	treeIdem.lru.Remove(x.el)
	treeIdem.bytes -= len(x.body)
}

// treeIdemFinal — is this an answer a retry must get again (and not a reason to retry)?
func treeIdemFinal(status int) bool {
	return status >= 200 && status < 500 && status != 429 && status != 408
}

func treeIdemPut(key string, status int, body []byte, ra string) {
	if !treeIdemFinal(status) || len(body) > treeIdemBody {
		return
	}
	treeIdem.mu.Lock()
	defer treeIdem.mu.Unlock()
	if x := treeIdem.m[key]; x != nil {
		treeIdemDropLocked(x)
	}
	e := &treeIdemEntry{key: key, status: status, body: append([]byte(nil), body...), ra: ra, at: time.Now()}
	e.el = treeIdem.lru.PushFront(e)
	treeIdem.m[key] = e
	treeIdem.bytes += len(e.body)
	for treeIdem.lru.Len() > treeIdemMax || treeIdem.bytes > treeIdemBytes {
		treeIdemDropLocked(treeIdem.lru.Back().Value.(*treeIdemEntry))
	}
}

func treeIdemStats() map[string]interface{} {
	treeIdem.mu.Lock()
	defer treeIdem.mu.Unlock()
	return map[string]interface{}{"entries": len(treeIdem.m), "max_entries": treeIdemMax, "bytes": treeIdem.bytes, "max_bytes": treeIdemBytes,
		"in_flight": len(treeIdem.flight), "replayed": treeIdem.hits, "joined": treeIdem.joined, "finished_without_reader": treeIdem.detached}
}
