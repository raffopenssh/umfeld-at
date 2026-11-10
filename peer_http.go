// peer_http.go — one outbound HTTP policy for the subsidy peer.
//
// The subsidy peer rate-limits per client address and exempts sibling
// services it can identify. Every request this process sends to that host
// (UMFELD_PEER_SUB_API) passes through peerTransport, which is installed as
// http.DefaultTransport — so every http.Client in the binary is covered
// without each call site having to remember:
//
//   - User-Agent is forced to  umfeld-at/<component> (+<public URL>)
//     (<component> taken from whatever UA the call site set; "go" if none);
//   - 429 → honour Retry-After (else exponential backoff), pause ALL requests
//     to the host until then, retry up to peerHTTPRetries times within the
//     caller's deadline;
//   - X-Peer-Token: $UMFELD_PEER_SUB_TOKEN when set — our own concurrency
//     bucket at the peer, independent of address and User-Agent;
//   - every response must carry X-RateLimit-Tier: peer (with the token) or
//     friendly (without); anything else is counted and logged (at most once a minute) and shows on debug/inflight
//     as peer_http.
//
// Scripts and one-off probes (curl, python) are outside this transport: they
// must send the same User-Agent themselves (AGENTS.md invariant 15).
package main

import (
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	peerHTTPRetries  = 5
	peerHTTPMaxWait  = 60 * time.Second
	peerHTTPTierHdr  = "X-RateLimit-Tier"
	peerHTTPTokenHdr = "X-Peer-Token"
)

// peerHTTPToken — UMFELD_PEER_SUB_TOKEN; a var so tests can set it.
var peerHTTPToken = sync.OnceValue(func() string { return strings.TrimSpace(os.Getenv("UMFELD_PEER_SUB_TOKEN")) })

// peerHTTPTierWant — the tier the peer must answer with.
func peerHTTPTierWant() string {
	if peerHTTPToken() != "" {
		return "peer"
	}
	return "friendly"
}

type peerTransport struct {
	next http.RoundTripper

	mu         sync.Mutex
	pauseUntil time.Time
	requests   int64
	got429     int64
	retried    int64
	gaveUp     int64
	notTier    int64
	lastTier   string
	lastWarn   time.Time
}

var peerHTTP = &peerTransport{next: http.DefaultTransport}

func init() { http.DefaultTransport = peerHTTP }

// peerHTTPHost — host[:port] of the subsidy peer, "" when not configured.
// A var so tests can point it at an httptest server.
var peerHTTPHost = sync.OnceValue(func() string {
	if u, err := url.Parse(peers().SubAPI); err == nil {
		return u.Host
	}
	return ""
})

// peerUserAgent — "umfeld-at/<component> (+<public URL>)" from whatever the
// call site set ("umfeld-at/fidx (+…)", "umfeld-at glyph", "" …).
func peerUserAgent(have string) string {
	comp := "go"
	if s, ok := strings.CutPrefix(have, "umfeld-at"); ok {
		s = strings.TrimLeft(s, "/ ")
		if i := strings.IndexAny(s, " (\t"); i >= 0 {
			s = s[:i]
		}
		if s != "" {
			comp = s
		}
	}
	ua := "umfeld-at/" + comp
	if p := publicURL(); p != "" {
		ua += " (+" + p + ")"
	}
	return ua
}

// peerRetryAfter — seconds or HTTP-date; 0 when absent/unparseable.
func peerRetryAfter(h string) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if s, err := strconv.ParseFloat(h, 64); err == nil && s >= 0 {
		return time.Duration(s * float64(time.Second))
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func (t *peerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := peerHTTPHost()
	if host == "" || req.URL.Host != host {
		return t.next.RoundTrip(req)
	}
	req = req.Clone(req.Context()) // a RoundTripper must not modify the caller's request
	req.Header.Set("User-Agent", peerUserAgent(req.Header.Get("User-Agent")))
	if tok := peerHTTPToken(); tok != "" {
		req.Header.Set(peerHTTPTokenHdr, tok)
	}
	ctx := req.Context()
	for attempt := 0; ; attempt++ {
		t.mu.Lock()
		wait := time.Until(t.pauseUntil)
		t.requests++
		t.mu.Unlock()
		if wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		resp, err := t.next.RoundTrip(req)
		if err != nil {
			return nil, err
		}
		t.noteTier(req, resp)
		if resp.StatusCode != http.StatusTooManyRequests {
			return resp, nil
		}
		d := peerRetryAfter(resp.Header.Get("Retry-After"))
		if d == 0 {
			d = time.Second << uint(attempt) // 1, 2, 4, 8, 16 s
		}
		if d > peerHTTPMaxWait {
			d = peerHTTPMaxWait
		}
		d += time.Duration(rand.Int63n(int64(d)/4 + 1)) // jitter: don't all wake at once
		t.mu.Lock()
		t.got429++
		if u := time.Now().Add(d); u.After(t.pauseUntil) {
			t.pauseUntil = u
		}
		t.mu.Unlock()
		// out of attempts, body not replayable, or the caller's deadline ends
		// before the pause does → hand the 429 back (callers treat it as
		// "peer unavailable", never as data).
		dl, hasDL := ctx.Deadline()
		if attempt >= peerHTTPRetries || (req.Body != nil && req.Body != http.NoBody && req.GetBody == nil) ||
			(hasDL && time.Until(dl) < d) {
			t.mu.Lock()
			t.gaveUp++
			t.mu.Unlock()
			return resp, nil
		}
		resp.Body.Close()
		if req.GetBody != nil {
			b, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			req.Body = b
		}
		t.mu.Lock()
		t.retried++
		t.mu.Unlock()
	}
}

func (t *peerTransport) noteTier(req *http.Request, resp *http.Response) {
	tier := resp.Header.Get(peerHTTPTierHdr)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastTier = tier
	if tier == peerHTTPTierWant() {
		return
	}
	t.notTier++
	if time.Since(t.lastWarn) > time.Minute {
		t.lastWarn = time.Now()
		log.Printf("peer_http: subsidy peer answered %s=%q (want %q) status=%d path=%s ua=%q",
			peerHTTPTierHdr, tier, peerHTTPTierWant(), resp.StatusCode, req.URL.Path, req.Header.Get("User-Agent"))
	}
}

// PeerHTTPStats — line for /debug/inflight.
func PeerHTTPStats() map[string]interface{} {
	t := peerHTTP
	t.mu.Lock()
	defer t.mu.Unlock()
	m := map[string]interface{}{
		"requests": t.requests, "got_429": t.got429, "retried": t.retried, "gave_up": t.gaveUp,
		"not_friendly": t.notTier, "last_tier": t.lastTier,
		"want_tier": peerHTTPTierWant(), "token": peerHTTPToken() != "", "user_agent": peerUserAgent("umfeld-at/<component>"),
	}
	if d := time.Until(t.pauseUntil); d > 0 {
		m["paused_s"] = int(d.Seconds()) + 1
	}
	return m
}
