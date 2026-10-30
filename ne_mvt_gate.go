// ne_mvt_gate.go — the gate in front of the cell tile layer (ne_mvt.go) and
// the archive jobs (ne_mvt_jobs.go). Internal — never described in public
// docs; clients only ever see standard RateLimit headers, 403 and 429.
//
// Identity = exportIdentity (export_gate.go): a token name, an EXPORT_KEYS
// name, or the client IP ("anon"). Per identity NAME the gate holds
//
//   - a bbox allow-list (lon/lat boxes, `*` = all of Austria)
//   - a daily tile budget and a daily area budget (km² of tile/archive
//     footprint), both continuous-refill buckets like neWriteBudgetGate
//     (api_ne_reports.go): over budget → 429 + Retry-After.
//
// Config (env file, 0600):
//
//	NE_MVT_ACCESS="name:w,s,e,n|w,s,e,n:tiles_per_day:km2_per_day;name:*:tiles:km2"
//	NE_MVT_GATE_ENFORCE=1   limits bite; default = count, log, advertise, allow
//
// Not enforced (default): every request passes, usage is counted per name and
// shown on the debug inflight page line `ne_mvt_gate`; a name with an entry
// still gets its RateLimit headers so clients can see what will apply.
// Enforced: a name without an entry is refused (403) — except `anon`, which
// gets the `anon` entry if configured, else 403 — and a tile outside every
// allowed box is 403; budgets 429. Everything is logged by apiMiddleware
// (status + peer=); the gate adds nothing to logs — names only, never keys.
package main

import (
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type neMVTAccess struct {
	name  string
	boxes [][4]float64 // empty = all of Austria
	tiles float64      // per day
	km2   float64      // per day
}

type neMVTUse struct {
	tiles, km2 float64 // bucket levels
	last       time.Time
	reqs       int64
	denied     int64
	tilesSum   int64
	km2Sum     float64
	first      time.Time
}

var (
	neMVTGateOnce    sync.Once
	neMVTGateEnforce bool
	neMVTAccessByNm  = map[string]*neMVTAccess{}
	neMVTUseMu       sync.Mutex
	neMVTUse_        = map[string]*neMVTUse{}
	neMVTUseMax      = 1000
	neMVTDefTiles    = 20000.0 // unconfigured name, gate off: advisory budget
	neMVTDefKm2      = 5000.0
)

// neMVTAustria — the service's extent, the `*` box.
var neMVTAustria = [4]float64{9.4, 46.3, 17.3, 49.1}

func neMVTParseAccess(s string) map[string]*neMVTAccess {
	m := map[string]*neMVTAccess{}
	for _, e := range strings.Split(s, ";") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		p := strings.Split(e, ":")
		if len(p) < 2 || p[0] == "" {
			continue
		}
		a := &neMVTAccess{name: strings.TrimSpace(p[0]), tiles: neMVTDefTiles, km2: neMVTDefKm2}
		for _, bx := range strings.Split(p[1], "|") {
			bx = strings.TrimSpace(bx)
			if bx == "*" || bx == "" {
				continue
			}
			v := strings.Split(bx, ",")
			if len(v) != 4 {
				continue
			}
			var b [4]float64
			ok := true
			for k := range v {
				x, err := strconv.ParseFloat(strings.TrimSpace(v[k]), 64)
				if err != nil {
					ok = false
				}
				b[k] = x
			}
			if ok && b[0] < b[2] && b[1] < b[3] {
				a.boxes = append(a.boxes, b)
			}
		}
		if len(p) > 2 {
			if v, err := strconv.ParseFloat(strings.TrimSpace(p[2]), 64); err == nil && v > 0 {
				a.tiles = v
			}
		}
		if len(p) > 3 {
			if v, err := strconv.ParseFloat(strings.TrimSpace(p[3]), 64); err == nil && v > 0 {
				a.km2 = v
			}
		}
		m[a.name] = a
	}
	return m
}

func neMVTGateLoad() {
	neMVTGateOnce.Do(func() {
		v := strings.ToLower(strings.TrimSpace(os.Getenv("NE_MVT_GATE_ENFORCE")))
		neMVTGateEnforce = v == "1" || v == "true" || v == "yes"
		neMVTAccessByNm = neMVTParseAccess(os.Getenv("NE_MVT_ACCESS"))
	})
}

// neMVTGateName — the config name for a request identity (export_gate.go ids).
func neMVTGateName(id string) string {
	if strings.HasPrefix(id, "ip:") {
		return "anon"
	}
	if i := strings.IndexByte(id, ':'); i >= 0 {
		return id[i+1:]
	}
	return id
}

func neMVTBoxAllowed(a *neMVTAccess, w, s, e, n float64) bool {
	if a == nil || len(a.boxes) == 0 {
		return true
	}
	for _, b := range a.boxes {
		if w < b[2] && e > b[0] && s < b[3] && n > b[1] {
			return true
		}
	}
	return false
}

// neMVTBoxKm2 — area of a lon/lat box in km² (spherical, good to 0.1 %).
func neMVTBoxKm2(w, s, e, n float64) float64 {
	const R = 6371.0088
	dl := (e - w) * math.Pi / 180
	return math.Abs(R * R * dl * (math.Sin(n*math.Pi/180) - math.Sin(s*math.Pi/180)))
}

// neMVTGate — admit a request for `tiles` tiles covering box (w,s,e,n) of
// `km2`. Returns the identity id (for accounting) and whether to proceed;
// on refusal the answer is written.
func neMVTGate(w http.ResponseWriter, r *http.Request, tiles int, bw, bs, be, bn float64) (string, bool) {
	neMVTGateLoad()
	id, _ := exportIdentity(r)
	name := neMVTGateName(id)
	a := neMVTAccessByNm[name]
	km2 := neMVTBoxKm2(bw, bs, be, bn)
	now := time.Now()
	neMVTUseMu.Lock()
	defer neMVTUseMu.Unlock()
	u := neMVTUse_[id]
	if u == nil {
		if len(neMVTUse_) >= neMVTUseMax {
			for k, v := range neMVTUse_ {
				if now.Sub(v.last) > time.Hour {
					delete(neMVTUse_, k)
				}
			}
			if len(neMVTUse_) >= neMVTUseMax {
				var oldK string
				var oldT time.Time
				for k, v := range neMVTUse_ {
					if oldK == "" || v.last.Before(oldT) {
						oldK, oldT = k, v.last
					}
				}
				delete(neMVTUse_, oldK)
			}
		}
		tb, kb := neMVTDefTiles, neMVTDefKm2
		if a != nil {
			tb, kb = a.tiles, a.km2
		}
		u = &neMVTUse{tiles: tb, km2: kb, last: now, first: now}
		neMVTUse_[id] = u
	}
	tb, kb := neMVTDefTiles, neMVTDefKm2
	if a != nil {
		tb, kb = a.tiles, a.km2
	}
	dt := now.Sub(u.last).Seconds()
	u.tiles = math.Min(tb, u.tiles+dt*tb/86400)
	u.km2 = math.Min(kb, u.km2+dt*kb/86400)
	u.last = now
	u.reqs++
	w.Header().Set("RateLimit-Policy", strconv.Itoa(int(tb))+";w=86400")
	w.Header().Set("RateLimit-Limit", strconv.Itoa(int(tb)))
	w.Header().Set("RateLimit-Remaining", strconv.Itoa(int(math.Max(u.tiles-float64(tiles), 0))))
	refuse := func(code int, msg string, wait int) (string, bool) {
		u.denied++
		if !neMVTGateEnforce {
			return id, true
		}
		w.Header().Set("Cache-Control", "no-store")
		if wait > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(wait))
			w.Header().Set("RateLimit-Reset", strconv.Itoa(wait))
		}
		apiError(w, msg, code)
		return id, false
	}
	if a == nil && neMVTGateEnforce {
		return refuse(http.StatusForbidden, "this layer is available to selected accounts — pass your key (key= / X-Export-Key) or ask for access", 0)
	}
	if !neMVTBoxAllowed(a, bw, bs, be, bn) {
		return refuse(http.StatusForbidden, "outside the area enabled for this account", 0)
	}
	if u.tiles < float64(tiles) || u.km2 < km2 {
		wait := 1
		if u.tiles < float64(tiles) {
			wait = int(math.Ceil((float64(tiles) - u.tiles) / (tb / 86400)))
		}
		if u.km2 < km2 {
			wait = max(wait, int(math.Ceil((km2-u.km2)/(kb/86400))))
		}
		return refuse(http.StatusTooManyRequests, "daily budget for this layer reached — retry after Retry-After", wait)
	}
	u.tiles -= float64(tiles)
	u.km2 -= km2
	u.tilesSum += int64(tiles)
	u.km2Sum += km2
	return id, true
}

// NEMVTGateStats — the debug inflight page line.
func NEMVTGateStats() map[string]interface{} {
	neMVTGateLoad()
	neMVTUseMu.Lock()
	defer neMVTUseMu.Unlock()
	type row struct {
		ID     string  `json:"id"`
		Reqs   int64   `json:"requests"`
		Tiles  int64   `json:"tiles"`
		Km2    float64 `json:"km2"`
		Denied int64   `json:"denied"`
		LeftT  int     `json:"tiles_left"`
		LeftK  int     `json:"km2_left"`
		Last   string  `json:"last"`
	}
	var rows []row
	for id, u := range neMVTUse_ {
		rows = append(rows, row{id, u.reqs, u.tilesSum, math.Round(u.km2Sum), u.denied, int(u.tiles), int(u.km2), u.last.UTC().Format(time.RFC3339)})
	}
	sort.Slice(rows, func(a, b int) bool { return rows[a].Reqs > rows[b].Reqs })
	if len(rows) > 20 {
		rows = rows[:20]
	}
	names := make([]string, 0, len(neMVTAccessByNm))
	for n := range neMVTAccessByNm {
		names = append(names, n)
	}
	sort.Strings(names)
	return map[string]interface{}{"enforce": neMVTGateEnforce, "configured": names, "identities": len(neMVTUse_), "max": neMVTUseMax, "top": rows}
}
