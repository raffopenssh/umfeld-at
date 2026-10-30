// export_gate.go — the gate in front of row/export endpoints (today
// /api/v1/ne/stats/unit; later MVT/PMTiles exports). Internal — not described
// in public docs; the public surface only ever sees standard rate-limit
// headers and a 429.
//
// Identity: `key=` query (links, =IMPORTDATA), `X-Export-Key` or
// `Authorization: Bearer` header. A key that is one of our tokens
// (UMFELD_TOKEN / CONTRIBUTOR_TOKEN) gets its token name as identity;
// EXPORT_KEYS (`key:name:rps:burst:daily,…`, env, 0600) defines issued keys
// with their own budget; anything else is anonymous = the client IP with
// the anonymous budget. Budgets: token bucket (rps, burst) + a UTC-day
// counter. Answers carry the IETF RateLimit headers; 429 + Retry-After.
//
// Monitoring: per identity {requests, limited, bytes, last, first} (bounded
// map, ≤ 2 000 identities, idle entries swept) → the debug inflight page
// line `export_gate`; apiMiddleware's access log line already carries the
// path. Keys never appear in logs or answers, only names.
package main

import (
	"crypto/subtle"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type exportTier struct {
	name  string
	rps   float64
	burst float64
	daily int64
	key   []byte // nil for anonymous / token-derived
}

type exportIdent struct {
	tier     *exportTier
	tokens   float64
	last     time.Time
	day      string
	dayN     int64
	requests int64
	limited  int64
	bytes    int64
	first    time.Time
}

var (
	exportGateMu    sync.Mutex
	exportIdents    = map[string]*exportIdent{}
	exportTiers     []*exportTier
	exportTiersOnce sync.Once
	exportAnon      = &exportTier{name: "anon", rps: 2, burst: 10, daily: 2000}
	exportNamedTok  = exportTier{name: "token", rps: 10, burst: 60, daily: 200000}
	exportIdentMax  = 2000
	exportEnforce   = false // EXPORT_GATE_ENFORCE=1: limits bite; default = count + advertise only
	exportBySrc     = map[string]int64{}
)

// exportLoadTiers — EXPORT_KEYS from the environment, once.
func exportLoadTiers() {
	exportTiersOnce.Do(func() {
		v := strings.ToLower(strings.TrimSpace(os.Getenv("EXPORT_GATE_ENFORCE")))
		exportEnforce = v == "1" || v == "true" || v == "yes"
		for _, e := range strings.Split(os.Getenv("EXPORT_KEYS"), ",") {
			p := strings.Split(strings.TrimSpace(e), ":")
			if len(p) < 2 || p[0] == "" {
				continue
			}
			t := &exportTier{key: []byte(p[0]), name: p[1], rps: 10, burst: 60, daily: 100000}
			if len(p) > 2 {
				t.rps, _ = strconv.ParseFloat(p[2], 64)
			}
			if len(p) > 3 {
				t.burst, _ = strconv.ParseFloat(p[3], 64)
			}
			if len(p) > 4 {
				t.daily, _ = strconv.ParseInt(p[4], 10, 64)
			}
			if t.rps <= 0 {
				t.rps = 10
			}
			if t.burst <= 0 {
				t.burst = t.rps * 6
			}
			exportTiers = append(exportTiers, t)
		}
	})
}

// exportIdentity — (identity id, tier) for a request. Keys are matched in
// constant time; the id never carries the key itself.
func exportIdentity(r *http.Request) (string, *exportTier) {
	exportLoadTiers()
	loadTierConfig()
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" {
		key = strings.TrimSpace(r.Header.Get("X-Export-Key"))
	}
	if key == "" {
		if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
			key = strings.TrimSpace(a[7:])
		}
	}
	if key != "" {
		kb := []byte(key)
		for _, t := range exportTiers {
			if len(t.key) == len(kb) && subtle.ConstantTimeCompare(t.key, kb) == 1 {
				return "key:" + t.name, t
			}
		}
		if i := tokenIndex(key); i >= 0 {
			t := exportNamedTok
			t.name = tokenName(i)
			return "token:" + t.name, &t
		}
	}
	ip := neClientIP(r)
	if ip == "" {
		ip = "local"
	}
	return "ip:" + ip, exportAnon
}

// exportGate — admit or 429. Sets RateLimit-* on every answer. Returns the
// identity id so the handler can account bytes (exportAccount).
func exportGate(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, tier := exportIdentity(r)
	now := time.Now()
	day := now.UTC().Format("2006-01-02")
	exportGateMu.Lock()
	defer exportGateMu.Unlock()
	e := exportIdents[id]
	if e == nil {
		if len(exportIdents) >= exportIdentMax {
			for k, v := range exportIdents {
				if now.Sub(v.last) > time.Hour {
					delete(exportIdents, k)
				}
			}
			if len(exportIdents) >= exportIdentMax { // still full: drop the oldest
				var oldK string
				var oldT time.Time
				for k, v := range exportIdents {
					if oldK == "" || v.last.Before(oldT) {
						oldK, oldT = k, v.last
					}
				}
				delete(exportIdents, oldK)
			}
		}
		e = &exportIdent{tier: tier, tokens: tier.burst, last: now, day: day, first: now}
		exportIdents[id] = e
	}
	e.tier = tier
	if e.day != day {
		e.day, e.dayN = day, 0
	}
	e.tokens = math.Min(tier.burst, e.tokens+now.Sub(e.last).Seconds()*tier.rps)
	e.last = now
	e.requests++
	if src := r.URL.Query().Get("src"); src != "" && len(src) <= 16 && len(exportBySrc) < 64 {
		exportBySrc[src]++
	} else if src == "" {
		exportBySrc["api"]++
	}
	remainDay := tier.daily - e.dayN
	w.Header().Set("RateLimit-Policy", strconv.Itoa(int(tier.burst))+";w=1, "+strconv.FormatInt(tier.daily, 10)+";w=86400")
	if e.tokens < 1 || remainDay <= 0 {
		e.limited++
		if !exportEnforce { // counted, advertised, not enforced (yet)
			e.tokens = math.Max(e.tokens, 0)
			e.dayN++
			w.Header().Set("RateLimit-Limit", strconv.Itoa(int(tier.burst)))
			w.Header().Set("RateLimit-Remaining", "0")
			return id, true
		}
		wait := int(math.Ceil((1 - e.tokens) / tier.rps))
		if remainDay <= 0 {
			wait = int(time.Until(now.UTC().Truncate(24*time.Hour).Add(24*time.Hour)).Seconds()) + 1
		}
		w.Header().Set("RateLimit-Limit", strconv.Itoa(int(tier.burst)))
		w.Header().Set("RateLimit-Remaining", "0")
		w.Header().Set("RateLimit-Reset", strconv.Itoa(max(wait, 1)))
		w.Header().Set("Retry-After", strconv.Itoa(max(wait, 1)))
		w.Header().Set("Cache-Control", "no-store")
		apiError(w, "rate limit — slow down or pass your key (key= / X-Export-Key)", http.StatusTooManyRequests)
		return id, false
	}
	e.tokens--
	e.dayN++
	w.Header().Set("RateLimit-Limit", strconv.Itoa(int(tier.burst)))
	w.Header().Set("RateLimit-Remaining", strconv.Itoa(int(e.tokens)))
	w.Header().Set("RateLimit-Reset", strconv.Itoa(int(math.Ceil((tier.burst-e.tokens)/tier.rps))))
	return id, true
}

// exportAccount — bytes served to an identity.
func exportAccount(id string, n int) {
	exportGateMu.Lock()
	if e := exportIdents[id]; e != nil {
		e.bytes += int64(n)
	}
	exportGateMu.Unlock()
}

// ExportGateStats — the debug inflight page line: totals + top identities
// by requests today (names only, never keys).
func ExportGateStats() map[string]interface{} {
	exportLoadTiers()
	exportGateMu.Lock()
	defer exportGateMu.Unlock()
	type row struct {
		ID       string `json:"id"`
		Tier     string `json:"tier"`
		Requests int64  `json:"requests"`
		Today    int64  `json:"today"`
		Limited  int64  `json:"limited"`
		Bytes    int64  `json:"bytes"`
		Last     string `json:"last"`
	}
	var rows []row
	var req, lim, by int64
	for id, e := range exportIdents {
		req += e.requests
		lim += e.limited
		by += e.bytes
		rows = append(rows, row{id, e.tier.name, e.requests, e.dayN, e.limited, e.bytes, e.last.UTC().Format(time.RFC3339)})
	}
	sort.Slice(rows, func(a, b int) bool { return rows[a].Requests > rows[b].Requests })
	if len(rows) > 20 {
		rows = rows[:20]
	}
	return map[string]interface{}{"identities": len(exportIdents), "max": exportIdentMax, "keys_configured": len(exportTiers),
		"requests": req, "limited": lim, "bytes": by, "top": rows, "enforce": exportEnforce, "by_src": exportBySrc,
		"anon": map[string]interface{}{"rps": exportAnon.rps, "burst": exportAnon.burst, "daily": exportAnon.daily}}
}
