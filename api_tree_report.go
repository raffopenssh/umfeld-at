// api_tree_report.go — POST /api/v1/trees/report: the tree-report relay.
//
// The landing glyph lets a reader name a tree's species, measure it, add a
// tree the LiDAR inventory misses, flag (not a tree, a second top of the tree beside it, a line of noise), comment or withdraw. Those reports belong to
// the observed-layer peer (its POST /api/v1/flags, tree contract 2026-11-11);
// this handler only forwards them with the deployment's reporter key
// (UMFELD_PEER_OBS_REPORTER_KEY → X-Reporter-Key), so the key never reaches a
// browser and the reports are attributed to this service. Nothing is stored
// here: no tree, no report, no reporter id (docs/ne-glyph.md §Tree reports).
//
// Bounds: body ≤ 32 KB, ≤ 50 items, tree layer only, string fields ≤ 64 chars
// (species ≤ 60 is the peer's own limit; note ≤ its limits.note_chars, here ≤ 4500), one global token
// bucket at the peer's pace (1 r/s, burst 10 — every reader shares our key's
// address) on top of the public fair-use gate per client. The peer's status
// and JSON body are handed back unchanged (201 written · 200 folded · 400
// did_you_mean · 404/409 candidates · 429).
//
// Position frames (tree_frames.go): "frame" / "as_of" of an item pass through
// as sent — the client says which copy of the trees it reported from — and the
// peer's frame{} comes back in the answer.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	treeReportBodyMax = 32 << 10
	treeReportBatch   = 50
	treeReportSource  = "umfeld-at/tree"
	treeReportBurst   = 10
	treeReportWaitMax = 4 * time.Second
)

// treeReportKey — the reporter key; a var so tests can set it.
var treeReportKey = sync.OnceValue(func() string { return strings.TrimSpace(os.Getenv("UMFELD_PEER_OBS_REPORTER_KEY")) })

// treeReportURL — the peer's write endpoint; a var so tests can point it at a stub.
var treeReportURL = func() string {
	if a := peers().ObsAPI; a != "" {
		return a + "/api/v1/flags"
	}
	return ""
}

// treeReportBucket — 1 token/s, burst treeReportBurst, shared by every reader.
var treeReportBucket = struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
	sent   int64
	denied int64
	status map[int]int64
}{tokens: treeReportBurst, status: map[int]int64{}}

// treeReportTake — a token now (0), or how long until one is due.
func treeReportTake() time.Duration {
	b := &treeReportBucket
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if !b.last.IsZero() {
		b.tokens += now.Sub(b.last).Seconds()
		if b.tokens > treeReportBurst {
			b.tokens = treeReportBurst
		}
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return 0
	}
	return time.Duration((1 - b.tokens) * float64(time.Second))
}

// treeNoteCap — does the peer store a note on a tree report? It says so in its
// dictionary (limits.note_chars); asked at most once an hour, never on a
// request path that waits (the mail loop and the stats line read it).
var treeNoteState struct {
	mu   sync.Mutex
	at   time.Time
	ok   int
	busy bool
}

// treeNoteCeil — the longest note the relay lets through, whatever the peer says.
const treeNoteCeil = 4500

// treeNoteCap — the peer's note limit in characters; 0 = it stores no notes.
func treeNoteCap() int {
	s := &treeNoteState
	s.mu.Lock()
	ok, stale := s.ok, time.Since(s.at) > time.Hour && !s.busy
	if stale {
		s.busy = true
	}
	first := s.at.IsZero()
	s.mu.Unlock()
	if !stale {
		return ok
	}
	ask := func() {
		got := 0
		if a := peers().ObsAPI; a != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			if req, err := http.NewRequestWithContext(ctx, http.MethodGet, a+"/api/v1/trees/taxa/dict", nil); err == nil {
				if resp, err := http.DefaultClient.Do(req); err == nil {
					var d struct {
						Limits struct {
							NoteChars int `json:"note_chars"`
						} `json:"limits"`
					}
					if resp.StatusCode == 200 && json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&d) == nil {
						got = max(d.Limits.NoteChars, 0)
					}
					resp.Body.Close()
				}
			}
		}
		s.mu.Lock()
		s.ok, s.at, s.busy = got, time.Now(), false
		s.mu.Unlock()
	}
	if first {
		ask() // the first caller waits once (≤ 8 s, background callers only)
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.ok
	}
	go ask()
	return ok
}

// TreeReportStats — one line for debug/inflight.
func TreeReportStats() map[string]interface{} {
	b := &treeReportBucket
	b.mu.Lock()
	defer b.mu.Unlock()
	st := map[string]int64{}
	for k, v := range b.status {
		st[strconv.Itoa(k)] = v
	}
	treeNoteState.mu.Lock()
	notes := treeNoteState.ok
	treeNoteState.mu.Unlock()
	return map[string]interface{}{"keyed": treeReportKey() != "", "peer_notes": notes, "sent": b.sent, "denied_429": b.denied, "peer_status": st, "idem": treeIdemStats()}
}

// treeReportClean — one report item: tree layer only, our source label, no
// key smuggled in the body, bounded strings. Returns "" or the reason.
func treeReportClean(m map[string]interface{}) string {
	if l, ok := m["layer"]; ok {
		if s, _ := l.(string); s != "trees" {
			return "layer must be \"trees\" (or absent)"
		}
	}
	m["layer"] = "trees"
	if c, _ := m["claim"].(string); c == "" {
		// no claim = a report on the tree itself (species, measure, state since the flight ("state": "logged" | null), withdraw, undo one step =
		// retract:"last"). A flag and its withdraw always name their claim.
		m["claim"] = "tree"
	}
	delete(m, "reporter_key")
	m["source"] = treeReportSource
	for k, v := range m {
		s, ok := v.(string)
		if !ok {
			continue
		}
		max := 64
		if k == "note" {
			max = treeNoteCeil // the peer's own limit decides below that
		}
		if utf8.RuneCountInString(s) > max {
			return k + " too long (≤ " + strconv.Itoa(max) + " chars)"
		}
	}
	return ""
}

func handleV1TreeReport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		apiError(w, "POST a tree report (species, measures, add, withdraw, undo) — relayed to "+peers().ObsName, http.StatusMethodNotAllowed)
		return
	}
	target := treeReportURL()
	if target == "" {
		apiError(w, "tree reports are not configured on this deployment", http.StatusServiceUnavailable)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, treeReportBodyMax+1))
	if err != nil || len(raw) > treeReportBodyMax {
		apiError(w, "body too large (≤ 32 KB)", http.StatusRequestEntityTooLarge)
		return
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		apiError(w, "body must be a JSON object", http.StatusBadRequest)
		return
	}
	if reps, ok := m["reports"]; ok {
		list, _ := reps.([]interface{})
		if len(list) == 0 || len(list) > treeReportBatch {
			apiError(w, "reports: 1–50 items", http.StatusBadRequest)
			return
		}
		for i, it := range list {
			im, _ := it.(map[string]interface{})
			if im == nil {
				apiError(w, "reports["+strconv.Itoa(i)+"] must be an object", http.StatusBadRequest)
				return
			}
			if why := treeReportClean(im); why != "" {
				apiError(w, "reports["+strconv.Itoa(i)+"]: "+why, http.StatusBadRequest)
				return
			}
		}
		delete(m, "reporter_key")
	} else if why := treeReportClean(m); why != "" {
		apiError(w, why, http.StatusBadRequest)
		return
	}
	body, _ := json.Marshal(m)

	// exactly-once (api_tree_report_idem.go): a retry of a request whose answer was
	// lost on the way gets that answer again, the peer is not written twice
	idem := treeIdemKey(r.Header.Get("X-Idem"), body)
	ctx := r.Context()
	if idem != "" {
		e, done := treeIdemBegin(idem, treeReportWaitMax+21*time.Second)
		if e != nil {
			if e.ra != "" {
				w.Header().Set("Retry-After", e.ra)
			}
			w.Header().Set("X-Idem-Replayed", "1")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(e.status)
			w.Write(e.body)
			return
		}
		defer done()
		ctx = context.WithoutCancel(ctx) // a keyed write finishes whether or not the reader is still there
	}

	// the peer's pace: wait a little for a token, then say when
	deadline := time.Now().Add(treeReportWaitMax)
	for {
		d := treeReportTake()
		if d == 0 {
			break
		}
		if time.Now().Add(d).After(deadline) {
			treeReportBucket.mu.Lock()
			treeReportBucket.denied++
			treeReportBucket.mu.Unlock()
			w.Header().Set("Retry-After", strconv.Itoa(int(d/time.Second)+1))
			apiError(w, "too many tree reports right now — try again shortly", http.StatusTooManyRequests)
			return
		}
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return
		}
	}

	status, out, ra, err := treeReportDo(ctx, body, "tree-report")
	if idem != "" && r.Context().Err() != nil {
		treeIdem.mu.Lock()
		treeIdem.detached++
		treeIdem.mu.Unlock()
	}
	if err != nil {
		apiError(w, peers().ObsName+" did not answer", http.StatusBadGateway)
		return
	}
	if status == http.StatusUnauthorized || (status == http.StatusForbidden && !bytes.Contains(out, []byte(`"not_yours"`))) {
		// our key, not the reader's fault — and never a hint about it. A 403 with
		// reason "not_yours" is the peer's answer to a withdraw by somebody who never
		// reported on that tree: that one goes back to the reader.
		apiError(w, "tree reports are not accepted right now", http.StatusBadGateway)
		return
	}
	if ra != "" {
		w.Header().Set("Retry-After", ra)
	}
	if !json.Valid(out) {
		apiError(w, peers().ObsName+" answered "+strconv.Itoa(status), http.StatusBadGateway)
		return
	}
	if idem != "" {
		treeIdemPut(idem, status, out, ra)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(out)
}

// treeReportDo — one POST to the peer's write endpoint with the deployment's
// reporter key; the caller holds a bucket token. Shared by the HTTP relay and
// the mail path (tree_mail.go).
func treeReportDo(ctx context.Context, body []byte, component string) (status int, out []byte, retryAfter string, err error) {
	target := treeReportURL()
	if target == "" {
		return 0, nil, "", errors.New("tree reports are not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "umfeld-at/"+component+" (+"+publicURL()+")")
	if k := treeReportKey(); k != "" {
		req.Header.Set("X-Reporter-Key", k)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer resp.Body.Close()
	out, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	treeReportBucket.mu.Lock()
	treeReportBucket.sent++
	treeReportBucket.status[resp.StatusCode]++
	treeReportBucket.mu.Unlock()
	treeFrameAnswer(out) // moved_m: the reporter's copy (maybe ours too) is from before a position correction
	return resp.StatusCode, out, resp.Header.Get("Retry-After"), nil
}

// treeReportWait — a bucket token, waiting as long as ctx allows (the mail
// path is in no hurry; a reader's request waits treeReportWaitMax at most).
func treeReportWait(ctx context.Context) error {
	for {
		d := treeReportTake()
		if d == 0 {
			return nil
		}
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
