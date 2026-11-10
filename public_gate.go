// public_gate.go — the fair-use gate in front of the whole PUBLIC tier.
//
// Why: the per-endpoint buckets (neReadGate, exportGate, the MVT gate) count
// requests on NE routes only. Nothing bounded what one anonymous client could
// pull in BYTES or burn in handler TIME, and the non-NE overlays
// (osm/geometry, toponyms, context, …) had no bucket at all: a loop over
// osm/geometry?bbox=<Austria> is 0.8 MB a hit, /ne/stats/unit?bbox=<Austria>
// 12 s of work a hit.
//
// One identity = one client address as the proxy saw it (neClientIP; IPv6
// collapsed to its /64, because a single host owns the whole /64). Token
// tiers (/contrib, the private prefix) and loopback are exempt — peers have
// their own write budgets. Each identity carries three token buckets, all
// sized for a reader who opens a large shared view (hundreds of tiles in one
// burst) and keeps panning, and all far below what would hurt the host:
//
//	requests  burst 600,    refill 60/s          (the glyph's own 400/40 fits inside)
//	bytes     burst 2 GiB,  refill 256 MiB/h     (wire bytes, after gzip)
//	compute   burst 600 s,  refill 1 s/s         (CPU-seconds, upper bound — see below)
//
// plus ≤ 150 requests in flight. Compute is NOT summed wall time: one cold
// full-resolution view queues ~500 tiles behind the render pool and sums to
// 6 000 s of waiting in a minute (measured 2026-11-10) while using at most
// NumCPU × 60 CPU-seconds. Each answer is charged its wall time (minus time
// blocked in socket writes — a slow reader is not work) × min(1, NumCPU /
// the client's requests in flight), which bounds what the client can really
// have burnt. Bytes and compute are charged AFTER the
// answer (a bucket may go negative; the next request waits until it is
// positive again), so a request is never cut mid-body. A global valve covers
// the many-addresses case: once anonymous egress of the UTC day passes
// PUBLIC_GATE_GLOBAL_GB (default 60), only identities that have used less
// than 1/8 of their byte burst are served — heavy users are shed first,
// ordinary readers never notice.
//
// Refusal = 429 + Retry-After + {"error":…}; the glyph's fetchers already
// back off on that. Env (all optional): PUBLIC_GATE_ENFORCE=0 (count only),
// PUBLIC_GATE_MB, PUBLIC_GATE_MB_PER_H, PUBLIC_GATE_CPU_S,
// PUBLIC_GATE_CPU_PER_S, PUBLIC_GATE_RPS, PUBLIC_GATE_BURST,
// PUBLIC_GATE_GLOBAL_GB. State: RAM only, ≤ publicGateMax identities, idle
// ones swept every 5 min once their buckets are full again (memory-contract.md); the debug inflight page line `public_gate`
// shows budgets, refusals per reason and the top talkers of the day.
package main

import (
	"math"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type publicGateCfg struct {
	enforce       bool
	rps, burst    float64
	bytesBurst    float64 // bytes
	bytesPerSec   float64
	cpuBurst      float64 // seconds
	cpuPerSec     float64
	maxInflight   int
	globalDayByte int64
}

type publicIdent struct {
	req, bytes, cpu float64 // tokens left (bytes/cpu may be negative)
	last            time.Time
	inflight        int
	day             string
	dayBytes        int64
	dayReqs         int64
	limited         int64
}

const publicGateMax = 20000

var (
	publicGateMu      sync.Mutex
	publicGateIdents  = map[string]*publicIdent{}
	publicGateOnce    sync.Once
	publicGate        publicGateCfg
	publicGateDay     string
	publicGateDayByte int64
	publicGateLimited = map[string]int64{} // reason → count (fixed key set)
	publicGateSwept   time.Time
	publicGateWould   int64 // refusals not applied (enforce off)
)

func publicGateEnvF(name string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(name), 64); err == nil && v > 0 {
		return v
	}
	return def
}

func publicGateLoad() {
	publicGateOnce.Do(func() {
		publicGate = publicGateCfg{
			enforce:       os.Getenv("PUBLIC_GATE_ENFORCE") != "0",
			rps:           publicGateEnvF("PUBLIC_GATE_RPS", 60),
			burst:         publicGateEnvF("PUBLIC_GATE_BURST", 600),
			bytesBurst:    publicGateEnvF("PUBLIC_GATE_MB", 2048) * (1 << 20),
			bytesPerSec:   publicGateEnvF("PUBLIC_GATE_MB_PER_H", 256) * (1 << 20) / 3600,
			cpuBurst:      publicGateEnvF("PUBLIC_GATE_CPU_S", 600),
			cpuPerSec:     publicGateEnvF("PUBLIC_GATE_CPU_PER_S", 1),
			maxInflight:   150,
			globalDayByte: int64(publicGateEnvF("PUBLIC_GATE_GLOBAL_GB", 60) * (1 << 30)),
		}
	})
}

// publicGateExempt — pages a refused reader must still be able to open.
func publicGateExempt(p string) bool {
	return p == "/impressum" || p == "/datenschutz" || p == "/lizenzen" || p == "/robots.txt"
}

// publicGateAdmit — (identity or nil when exempt, refusal reason, wait s).
func publicGateAdmit(ip string, now time.Time) (*publicIdent, string, int) {
	publicGateLoad()
	c := &publicGate
	day := now.UTC().Format("2006-01-02")
	publicGateMu.Lock()
	defer publicGateMu.Unlock()
	if day != publicGateDay {
		publicGateDay, publicGateDayByte = day, 0
	}
	if now.Sub(publicGateSwept) > 5*time.Minute { // addresses do not linger in RAM (/datenschutz §2.2)
		publicGateSwept = now
		publicGateSweep(now)
		go neRateSweep(now)
	}
	e := publicGateIdents[ip]
	if e == nil {
		if len(publicGateIdents) >= publicGateMax {
			publicGateSweep(now)
		}
		e = &publicIdent{req: c.burst, bytes: c.bytesBurst, cpu: c.cpuBurst, last: now, day: day}
		publicGateIdents[ip] = e
	}
	dt := now.Sub(e.last).Seconds()
	e.last = now
	e.req = math.Min(c.burst, e.req+dt*c.rps)
	e.bytes = math.Min(c.bytesBurst, e.bytes+dt*c.bytesPerSec)
	e.cpu = math.Min(c.cpuBurst, e.cpu+dt*c.cpuPerSec)
	if e.day != day {
		e.day, e.dayBytes, e.dayReqs = day, 0, 0
	}
	e.dayReqs++
	reason, wait := "", 0
	switch {
	case e.inflight >= c.maxInflight:
		reason, wait = "inflight", 1
	case e.req < 1:
		reason, wait = "requests", int(math.Ceil((1-e.req)/c.rps))
	case e.bytes <= 0:
		reason, wait = "bytes", int(math.Ceil((1-e.bytes)/c.bytesPerSec))
	case e.cpu <= 0:
		reason, wait = "compute", int(math.Ceil((1-e.cpu)/c.cpuPerSec))
	case publicGateDayByte > c.globalDayByte && e.bytes < c.bytesBurst*7/8:
		reason, wait = "global", int(math.Ceil((c.bytesBurst*7/8-e.bytes)/c.bytesPerSec))
	}
	if reason != "" {
		e.limited++
		publicGateLimited[reason]++
		if !c.enforce {
			publicGateWould++
			reason = ""
		} else {
			return e, reason, min(max(wait, 1), 3600)
		}
	}
	e.req = math.Max(e.req-1, 0)
	e.inflight++
	return e, "", 0
}

// publicGateDone — charge what the answer cost.
func publicGateDone(e *publicIdent, wire int64, took time.Duration) {
	publicGateMu.Lock()
	if n := float64(runtime.NumCPU()); float64(e.inflight) > n {
		took = time.Duration(float64(took) * n / float64(e.inflight))
	}
	e.inflight--
	e.bytes -= float64(wire)
	e.cpu -= took.Seconds()
	e.dayBytes += wire
	publicGateDayByte += wire
	publicGateMu.Unlock()
}

// publicGateSweep — drop identities with nothing in flight whose buckets have
// refilled (idle ≥ 10 min and no debt); if the map is still full, the oldest
// half goes. Caller holds the lock.
func publicGateSweep(now time.Time) {
	c := &publicGate
	for k, v := range publicGateIdents {
		idle := now.Sub(v.last).Seconds()
		if v.inflight == 0 && idle > 600 && v.bytes+idle*c.bytesPerSec >= c.bytesBurst && v.cpu+idle*c.cpuPerSec >= c.cpuBurst {
			delete(publicGateIdents, k)
		}
	}
	if len(publicGateIdents) < publicGateMax {
		return
	}
	type kv struct {
		k string
		t time.Time
	}
	all := make([]kv, 0, len(publicGateIdents))
	for k, v := range publicGateIdents {
		if v.inflight == 0 {
			all = append(all, kv{k, v.last})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].t.Before(all[j].t) })
	for _, x := range all[:len(all)/2] {
		delete(publicGateIdents, x.k)
	}
}

var publicGateMsg = map[string]string{
	"inflight": "too many requests in flight from this client",
	"requests": "rate limit: too many requests per second from this client",
	"bytes":    "fair use: this client's transfer budget is used up for now",
	"compute":  "fair use: this client's compute budget is used up for now",
	"global":   "service is under heavy load; heavy clients are paused first",
}

// publicGateMiddleware sits inside egressMiddleware (so the wire counter is
// in the context and refusals show in the egress stats) and outside gzip.
func publicGateMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		ip := ""
		if t := r.Header.Get(tierHeader); t != "private" && t != "contrib" && !publicGateExempt(r.URL.Path) {
			ip = neClientIP(r)
		}
		if ip == "" {
			next.ServeHTTP(w, r)
			return
		}
		t0 := time.Now()
		e, reason, wait := publicGateAdmit(ip, t0)
		if reason != "" {
			h.Set("Retry-After", strconv.Itoa(wait))
			h.Set("Cache-Control", "no-store")
			h.Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"` + publicGateMsg[reason] + `","status":429,"retry_after_s":` + strconv.Itoa(wait) + "}\n"))
			return
		}
		defer func() {
			var wire int64
			took := time.Since(t0)
			if c := egressFromContext(r.Context()); c != nil {
				wire = atomic.LoadInt64(&c.wire)
				took -= time.Duration(atomic.LoadInt64(&c.wnanos)) // a slow reader is not compute
			}
			publicGateDone(e, wire, max(took, 0))
		}()
		next.ServeHTTP(w, r)
	})
}

// PublicGateStats — the `public_gate` line of the debug inflight page.
func PublicGateStats() map[string]interface{} {
	publicGateLoad()
	c := publicGate
	type top struct {
		ID       string  `json:"id"`
		MB       float64 `json:"mb_today"`
		Reqs     int64   `json:"requests_today"`
		Limited  int64   `json:"limited"`
		BytesPct float64 `json:"bytes_left_pct"`
		CPUPct   float64 `json:"compute_left_pct"`
	}
	publicGateMu.Lock()
	rows := make([]top, 0, len(publicGateIdents))
	for k, v := range publicGateIdents {
		if v.day != publicGateDay {
			continue
		}
		rows = append(rows, top{k, math.Round(float64(v.dayBytes)/1e4) / 100, v.dayReqs, v.limited,
			math.Round(100 * v.bytes / c.bytesBurst), math.Round(100 * v.cpu / c.cpuBurst)})
	}
	lim := map[string]int64{}
	for k, v := range publicGateLimited {
		lim[k] = v
	}
	n, dayB, would := len(publicGateIdents), publicGateDayByte, publicGateWould
	publicGateMu.Unlock()
	sort.Slice(rows, func(i, j int) bool { return rows[i].MB > rows[j].MB })
	if len(rows) > 10 {
		rows = rows[:10]
	}
	return map[string]interface{}{
		"enforce": c.enforce, "identities": n, "max": publicGateMax,
		"per_client": map[string]interface{}{
			"rps": c.rps, "burst": c.burst, "mb_burst": c.bytesBurst / (1 << 20), "mb_per_h": math.Round(c.bytesPerSec * 3600 / (1 << 20)),
			"compute_s_burst": c.cpuBurst, "compute_s_per_s": c.cpuPerSec, "max_inflight": c.maxInflight,
		},
		"global_gb_day": float64(c.globalDayByte) / (1 << 30), "anon_mb_today": math.Round(float64(dayB) / (1 << 20)),
		"limited": lim, "would_limit_unenforced": would, "top": rows,
	}
}
