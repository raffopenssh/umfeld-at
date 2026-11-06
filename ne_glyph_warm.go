// ne_glyph_warm.go — opened share links stay fast across a restart.
//
// Every in-memory cache behind the landing glyph (tile LRU, tile geometry, filter
// bins, pass tables) is empty after a restart, so the first reader of a link that
// was copied and opened directly (`/#kg=…;v=obs;full;…`) waited for every tile
// to render again. The page therefore tells us, once per DIRECT open (a usable
// hash at load, not `?trace` / `?dbg` / `?warm` — those are the operator's own
// loads), which same-origin glyph URLs it fetched in its first 25 s, together
// with the hash (search term and place name stripped) and the device class
// (`d` desktop / `m` coarse pointer). Nothing else: no IP, no user agent, no
// screen size, no time of day finer than the day (docs/ne-glyph.md §warm,
// Datenschutz 3).
//
// Store: data/ne_glyph_opened.json — ≤ neWarmMaxEntries hashes, each with the
// URL list per device class (≤ neWarmMaxURLs), the open count and the day of the
// last open; entries unopened for neWarmKeepDays fall out. Never committed.
//
// Replay: neWarmDelay (30 min, NE_WARM_DELAY) after start — a development
// restart every few minutes must not re-render the country — the newest entries
// are replayed through the in-process mux as `ua=umfeld-warm` requests, one at
// a time, yielding to real readers (neRenderYield), within neWarmBudget URLs.
// A device class that was never seen for a link is synthesised from the other
// one: same tiles when the link is not `;screen` (the frame has one aspect
// ratio everywhere), a taller / wider window around the same centre when it is.
// NE_WARM=0 turns the replay off; the beacon is always accepted (it is cheap)
// so the store is complete when the replay is on again.
package main

import (
	"encoding/json"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	neWarmPath       = "data/ne_glyph_opened.json"
	neWarmMaxEntries = 150
	neWarmMaxURLs    = 500
	neWarmKeepDays   = 30
	neWarmBodyMax    = 96 << 10
	neWarmBudget     = 6000 // URLs per replay
	neWarmEntriesMax = 60   // entries per replay (newest first)
	neWarmHashMax    = 400
	neWarmUA         = "umfeld-warm"
)

// aspect ratios (height / width) of a `;screen` view: the desktop frame and a
// phone held upright (bars taken off). Only the tile window depends on them.
const (
	neWarmARDesk  = 0.62
	neWarmARPhone = 2.0
)

type neWarmEntry struct {
	Hash string   `json:"hash"`
	N    int      `json:"n"`
	Last string   `json:"last"` // day, UTC
	D    []string `json:"d,omitempty"`
	M    []string `json:"m,omitempty"`
}

var (
	neWarmMu      sync.Mutex
	neWarmEntries map[string]*neWarmEntry
	neWarmLoaded  bool
	neWarmDirty   bool
	neWarmStat    = struct {
		beacons, rejected, replayed, failed, synth int
		startedAt, doneAt                          time.Time
		state                                      string
	}{state: "idle"}
)

// neWarmHandler — the mux the replay drives (set by main before ListenAndServe).
var neWarmHandler http.Handler

func neWarmLoad() {
	if neWarmLoaded {
		return
	}
	neWarmLoaded = true
	neWarmEntries = map[string]*neWarmEntry{}
	b, err := os.ReadFile(neWarmPath)
	if err != nil {
		return
	}
	var list []*neWarmEntry
	if json.Unmarshal(b, &list) != nil {
		return
	}
	cut := time.Now().UTC().AddDate(0, 0, -neWarmKeepDays).Format("2006-01-02")
	for _, e := range list {
		if e == nil || e.Hash == "" || e.Last < cut {
			continue
		}
		neWarmEntries[e.Hash] = e
	}
}

// neWarmSave — the store on disk (called with the lock held, debounced by the caller).
func neWarmSave() {
	list := neWarmSorted()
	b, err := json.Marshal(list)
	if err != nil {
		return
	}
	tmp := neWarmPath + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, neWarmPath)
	}
	neWarmDirty = false
}

// neWarmSorted — newest first, then most opened (lock held).
func neWarmSorted() []*neWarmEntry {
	list := make([]*neWarmEntry, 0, len(neWarmEntries))
	for _, e := range neWarmEntries {
		list = append(list, e)
	}
	sort.Slice(list, func(a, b int) bool {
		if list[a].Last != list[b].Last {
			return list[a].Last > list[b].Last
		}
		if list[a].N != list[b].N {
			return list[a].N > list[b].N
		}
		return list[a].Hash < list[b].Hash
	})
	return list
}

// neWarmCanonHash — the share hash without anything a person typed: `q=`
// (search term), `pn=` (place name), and the operator's own tokens. Tokens are
// kept in order; the result is bounded.
func neWarmCanonHash(h string) string {
	h = strings.TrimLeft(strings.TrimSpace(h), "#!/")
	if u, err := url.PathUnescape(h); err == nil && strings.Contains(h, "%3B") {
		h = u
	}
	var out []string
	for _, t := range strings.Split(h, ";") {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		k := strings.ToLower(t)
		if i := strings.IndexByte(k, '='); i >= 0 {
			k = k[:i]
		}
		switch k {
		case "q", "pn", "dbg", "trace", "export":
			continue
		}
		out = append(out, t)
	}
	s := strings.Join(out, ";")
	if len(s) > neWarmHashMax {
		s = s[:neWarmHashMax]
	}
	return s
}

// neWarmURLOK — only same-origin glyph reads are replayed: a path under
// /api/v1/ne/glyph/ with a query, no scheme, no host, no control characters.
func neWarmURLOK(u string) bool {
	if len(u) > 1024 || !strings.HasPrefix(u, "/api/v1/ne/glyph/") || strings.Contains(u, "..") {
		return false
	}
	for i := 0; i < len(u); i++ {
		if u[i] < 0x21 || u[i] > 0x7e {
			return false
		}
	}
	p := u
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	switch strings.TrimPrefix(p, "/api/v1/ne/glyph/") {
	case "tile", "cell", "kg", "kgs", "admin", "admin/outline", "sub", "fnat", "fidx", "passes":
		return true
	}
	return false
}

// handleNEGlyphOpened — POST /api/v1/ne/glyph/opened {hash, dev:"d"|"m", urls:[…]}.
// 204 always on a well-formed body (the page never reads the answer); the
// request leaves no trace beyond the store described above.
func handleNEGlyphOpened(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		apiError(w, "POST only", 405)
		return
	}
	if ip := neClientIP(r); ip != "" {
		if ok, _ := neRateAllowN(ip+"#w", time.Now(), 0.2, 6); !ok {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, neWarmBodyMax+1))
	if err != nil || len(body) > neWarmBodyMax {
		apiError(w, "body too large", 413)
		return
	}
	var in struct {
		Hash string   `json:"hash"`
		Dev  string   `json:"dev"`
		URLs []string `json:"urls"`
	}
	if json.Unmarshal(body, &in) != nil {
		apiError(w, "bad json", 400)
		return
	}
	if !neWarmRecord(in.Hash, in.Dev, in.URLs) {
		neWarmMu.Lock()
		neWarmStat.rejected++
		neWarmMu.Unlock()
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// neWarmRecord — fold one open into the store. False = nothing usable.
func neWarmRecord(hash, dev string, urls []string) bool {
	hash = neWarmCanonHash(hash)
	if hash == "" || (dev != "d" && dev != "m") {
		return false
	}
	seen := map[string]bool{}
	var keep []string
	for _, u := range urls {
		if len(keep) >= neWarmMaxURLs {
			break
		}
		if !neWarmURLOK(u) || seen[u] {
			continue
		}
		seen[u] = true
		keep = append(keep, u)
	}
	if len(keep) == 0 {
		return false
	}
	neWarmMu.Lock()
	defer neWarmMu.Unlock()
	neWarmLoad()
	neWarmStat.beacons++
	e := neWarmEntries[hash]
	if e == nil {
		if len(neWarmEntries) >= neWarmMaxEntries {
			list := neWarmSorted()
			for _, old := range list[neWarmMaxEntries-1:] {
				delete(neWarmEntries, old.Hash)
			}
		}
		e = &neWarmEntry{Hash: hash}
		neWarmEntries[hash] = e
	}
	e.N++
	e.Last = time.Now().UTC().Format("2006-01-02")
	if dev == "d" {
		e.D = keep
	} else {
		e.M = keep
	}
	if !neWarmDirty {
		neWarmDirty = true
		time.AfterFunc(10*time.Second, func() {
			neWarmMu.Lock()
			neWarmSave()
			neWarmMu.Unlock()
		})
	}
	return true
}

// ── counterpart synthesis ───────────────────────────────────────────────────

type neWarmTile struct {
	lod, i, j int
	rest      string // the query without lod/i/j, in its original order
}

func neWarmParseTile(u string) (neWarmTile, bool) {
	const p = "/api/v1/ne/glyph/tile?"
	if !strings.HasPrefix(u, p) {
		return neWarmTile{}, false
	}
	var t neWarmTile
	var rest []string
	n := 0
	for _, kv := range strings.Split(u[len(p):], "&") {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "lod", "i", "j":
			x, err := strconv.Atoi(v)
			if err != nil {
				return neWarmTile{}, false
			}
			n++
			if k == "lod" {
				t.lod = x
			} else if k == "i" {
				t.i = x
			} else {
				t.j = x
			}
		default:
			rest = append(rest, kv)
		}
	}
	if n != 3 {
		return neWarmTile{}, false
	}
	t.rest = strings.Join(rest, "&")
	return t, true
}

func (t neWarmTile) url() string {
	s := "/api/v1/ne/glyph/tile?lod=" + strconv.Itoa(t.lod) + "&i=" + strconv.Itoa(t.i) + "&j=" + strconv.Itoa(t.j)
	if t.rest != "" {
		s += "&" + t.rest
	}
	return s
}

// neWarmCounterpart — the other device class's list from a recorded one. Tiles
// of a `;screen` link get a window of the other aspect ratio around the same
// centre (the client's hiWant: centre-first, capped at 48 × ds tiles); every
// other URL (and every tile of a framed link) is the same on both.
func neWarmCounterpart(hash string, urls []string, fromDev string) []string {
	screen := false
	for _, t := range strings.Split(hash, ";") {
		if strings.EqualFold(t, "screen") {
			screen = true
		}
	}
	if !screen {
		return urls
	}
	ratio := neWarmARPhone / neWarmARDesk
	if fromDev == "m" {
		ratio = 1 / ratio
	}
	groups := map[string][]neWarmTile{}
	var order []string
	var out []string
	for _, u := range urls {
		t, ok := neWarmParseTile(u)
		if !ok {
			out = append(out, u)
			continue
		}
		k := strconv.Itoa(t.lod) + "|" + t.rest
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], t)
	}
	for _, k := range order {
		g := groups[k]
		i0, i1, j0, j1 := g[0].i, g[0].i, g[0].j, g[0].j
		for _, t := range g {
			i0, i1 = min(i0, t.i), max(i1, t.i)
			j0, j1 = min(j0, t.j), max(j1, t.j)
		}
		cap := 48
		if ds := neWarmQueryInt(g[0].rest, "ds"); ds >= 4 {
			cap = 48 * 4
		} else if ds >= 2 {
			cap = 48 * 2
		}
		h := float64(j1-j0+1) * ratio
		if h < 1 {
			h = 1
		}
		cj := float64(j0+j1) / 2
		nj0, nj1 := int(math.Floor(cj-h/2+0.5)), int(math.Floor(cj+h/2-0.5))
		ci := float64(i0+i1) / 2
		type cand struct {
			t neWarmTile
			d float64
		}
		var cs []cand
		for i := i0; i <= i1; i++ {
			for j := nj0; j <= nj1; j++ {
				cs = append(cs, cand{neWarmTile{g[0].lod, i, j, g[0].rest}, math.Hypot(float64(i)-ci, float64(j)-cj)})
			}
		}
		sort.SliceStable(cs, func(a, b int) bool { return cs[a].d < cs[b].d })
		if len(cs) > cap {
			cs = cs[:cap]
		}
		for _, c := range cs {
			out = append(out, c.t.url())
		}
	}
	if len(out) > neWarmMaxURLs {
		out = out[:neWarmMaxURLs]
	}
	return out
}

func neWarmQueryInt(rest, key string) int {
	for _, kv := range strings.Split(rest, "&") {
		if k, v, _ := strings.Cut(kv, "="); k == key {
			n, _ := strconv.Atoi(v)
			return n
		}
	}
	return 0
}

// ── replay ──────────────────────────────────────────────────────────────────

func neWarmDelay() time.Duration {
	if v := os.Getenv("NE_WARM_DELAY"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return 30 * time.Minute
}

// neWarmLoop — started from main; sleeps the delay, then replays once.
func neWarmLoop() {
	if os.Getenv("NE_WARM") == "0" {
		neWarmMu.Lock()
		neWarmStat.state = "off"
		neWarmMu.Unlock()
		return
	}
	neWarmMu.Lock()
	neWarmStat.state = "waiting"
	neWarmMu.Unlock()
	time.Sleep(neWarmDelay())
	neWarmReplay()
}

// neWarmPlan — the URL list of one replay: newest entries first, both device
// classes (the missing one synthesised), deduplicated, within the budget.
func neWarmPlan() (urls []string, entries, synth int) {
	neWarmMu.Lock()
	neWarmLoad()
	list := neWarmSorted()
	neWarmMu.Unlock()
	seen := map[string]bool{}
	add := func(us []string) {
		for _, u := range us {
			if !seen[u] && len(urls) < neWarmBudget {
				seen[u] = true
				urls = append(urls, u)
			}
		}
	}
	for _, e := range list {
		if entries >= neWarmEntriesMax || len(urls) >= neWarmBudget {
			break
		}
		entries++
		d, m := e.D, e.M
		if d == nil && m != nil {
			d = neWarmCounterpart(e.Hash, m, "m")
			synth++
		} else if m == nil && d != nil {
			m = neWarmCounterpart(e.Hash, d, "d")
			synth++
		}
		add(d)
		add(m)
	}
	return
}

func neWarmReplay() {
	if neWarmHandler == nil {
		return
	}
	urls, entries, synth := neWarmPlan()
	neWarmMu.Lock()
	neWarmStat.state = "running"
	neWarmStat.startedAt = time.Now()
	neWarmStat.synth = synth
	neWarmMu.Unlock()
	log.Printf("🔥 warm: replaying %d opened links, %d URLs (%d device classes synthesised)", entries, len(urls), synth)
	t0 := time.Now()
	ok, bad := 0, 0
	for _, u := range urls {
		neRenderYield() // readers first
		req := httptest.NewRequest(http.MethodGet, u, nil)
		req.Header.Set("User-Agent", neWarmUA)
		req.RemoteAddr = "127.0.0.1:0"
		rec := httptest.NewRecorder()
		rec.Body = nil // the bytes are the cache's business, not ours
		neWarmHandler.ServeHTTP(rec, req)
		if rec.Code < 400 {
			ok++
		} else {
			bad++
		}
		time.Sleep(40 * time.Millisecond)
	}
	neWarmMu.Lock()
	neWarmStat.state = "done"
	neWarmStat.doneAt = time.Now()
	neWarmStat.replayed += ok
	neWarmStat.failed += bad
	neWarmMu.Unlock()
	log.Printf("🔥 warm: done, %d ok / %d failed in %s", ok, bad, time.Since(t0).Round(time.Second))
}

// NEWarmStats — the `ne_warm` line of /debug/inflight (memory contract).
func NEWarmStats() map[string]interface{} {
	neWarmMu.Lock()
	defer neWarmMu.Unlock()
	neWarmLoad()
	urls := 0
	for _, e := range neWarmEntries {
		urls += len(e.D) + len(e.M)
	}
	m := map[string]interface{}{
		"entries": len(neWarmEntries), "max_entries": neWarmMaxEntries, "urls": urls, "max_urls": neWarmMaxEntries * neWarmMaxURLs * 2,
		"beacons": neWarmStat.beacons, "rejected": neWarmStat.rejected, "state": neWarmStat.state,
		"replayed": neWarmStat.replayed, "failed": neWarmStat.failed, "synth": neWarmStat.synth,
		"delay": neWarmDelay().String(),
	}
	if !neWarmStat.startedAt.IsZero() {
		m["started_at"] = neWarmStat.startedAt.UTC().Format(time.RFC3339)
	}
	if !neWarmStat.doneAt.IsZero() {
		m["done_at"] = neWarmStat.doneAt.UTC().Format(time.RFC3339)
	}
	return m
}
