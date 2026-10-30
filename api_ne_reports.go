// api_ne_reports.go — observer epoch reports (docs/ne-cells.md §epochs,
// §observers).
//
// Peers that assemble the cadastre themselves (bevdirect, https://github.com/raffopenssh/vtcseamless) tell us
// what they saw by POSTing DIGESTS to <private>/api/v1/ne/{kg}/report. Unlike the
// obs layer ({OBS_NAME} only, isNEWriter) a report may come from ANY
// private token: {MAP_NAME} is the first such observer (Oct 2026).
//
//   - Per-token gate: `neReportGate` — CONTRIBUTOR_TOKEN (report-only peers,
//     e.g. {MAP_NAME}) or any UMFELD_TOKEN; token bucket
//     neReportPerSec/neReportBurst per token → 429 + Retry-After. The stored
//     line carries the token's name so an observer cannot impersonate another.
//   - Baseline: a section of the same source if we hold one ({kg}.bev.nec),
//     else the FIRST report with the same (source, algo, chunk_res) for the
//     KG — so the first bevdirect report of a KG becomes epoch 0 and later
//     reports (same or another observer) are compared chunk by chunk.
//   - Dedupe: a report identical (digest) to the previous one of the same
//     token+source for that KG is stored without its chunks (`unchanged:true`).
//   - Watch: GET <private>/api/v1/ne/reports (any private token) — per token /
//     observer counters since process start plus a scan of the jsonl files
//     (memoised 60 s); `ne_reports` line on /api/v1/debug/inflight.
package main

import (
	"bufio"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ── contributor tokens ────────────────────────────────────────────────────────
//
//	CONTRIBUTOR_TOKEN=a,b            peers that only CONTRIBUTE (NE epoch reports)
//	CONTRIBUTOR_TOKEN_NAMES=x,y      parallel labels (attribution in every stored line)
//
// A contributor token opens ONLY the contributor paths under /k (see
// contributorPathAllowed) — never the cadastre surface. {MAP_NAME}
// has such a token; it has no UMFELD_TOKEN. The private-tier tokens
// (UMFELD_TOKEN: {OBS_NAME}, ops) may report too and are attributed by
// UMFELD_TOKEN_NAMES (default index 0 = {OBS_NAME}, 1 = ops, else token-N).

var (
	contribOnce   sync.Once
	contribTokens [][]byte
	contribNames  []string
)

func loadContributorConfig() {
	contribOnce.Do(func() {
		for _, t := range strings.Split(os.Getenv("CONTRIBUTOR_TOKEN"), ",") {
			if t = strings.TrimSpace(t); len(t) >= 16 {
				contribTokens = append(contribTokens, []byte(t))
			}
		}
		for _, n := range strings.Split(os.Getenv("CONTRIBUTOR_TOKEN_NAMES"), ",") {
			contribNames = append(contribNames, strings.TrimSpace(n))
		}
	})
}

func setContributorConfigForTest(tokens, names []string) {
	contribOnce.Do(func() {})
	contribTokens, contribNames = nil, names
	for _, t := range tokens {
		contribTokens = append(contribTokens, []byte(t))
	}
}

// contributorIndex — position of tok in CONTRIBUTOR_TOKEN, -1 if none.
func contributorIndex(tok string) int {
	loadContributorConfig()
	if tok == "" {
		return -1
	}
	b := []byte(tok)
	for i, t := range contribTokens {
		if len(t) == len(b) && subtle.ConstantTimeCompare(t, b) == 1 {
			return i
		}
	}
	return -1
}

// contributorPathAllowed — the whole surface a contributor token opens
// (paths as the handler sees them, /k already stripped).
func contributorPathAllowed(p string) bool {
	if p == "/api/v1/ne/reports" || p == "/api/v1/ne/manifest" || p == "/api/v1/ne" || p == "/api/v1/ne/revisit" {
		return true
	}
	rest := strings.TrimPrefix(p, "/api/v1/ne/")
	if !strings.HasPrefix(p, "/api/v1/ne/") || strings.Contains(rest, "..") {
		return false
	}
	// the cell tile layer + archives (ne_mvt.go): public with an optional key, also under /contrib
	if rest == "mvt.json" || strings.HasPrefix(rest, "mvt/") || rest == "pmtiles" || strings.HasPrefix(rest, "pmtiles/") {
		return true
	}
	// /{kg}/obs is the declared+observed layer upload; only the token named
	// neWriterName passes neWriteGate there, every other contributor → 404.
	return strings.HasSuffix(rest, "/report") || strings.HasSuffix(rest, "/chunks") || strings.HasSuffix(rest, "/obs") || strings.HasSuffix(rest, "/declared") || neKGRe.MatchString(rest)
}

// reporterID — who is reporting: name + stable key for buckets/counters.
// idx ≥ 0 private-tier token, idx < 0 none; contributor tokens get keys 1000+i.
func reporterID(r *http.Request) (key int, name string) {
	tok := requestToken(r)
	if i := tokenIndex(tok); i >= 0 {
		return i, tokenName(i)
	}
	if i := contributorIndex(tok); i >= 0 {
		n := ""
		if i < len(contribNames) {
			n = contribNames[i]
		}
		if n == "" {
			n = "contributor-" + strconv.Itoa(i)
		}
		return 1000 + i, n
	}
	return -1, ""
}

// tokenName — label of the i-th UMFELD_TOKEN (UMFELD_TOKEN_NAMES, default
// historical layout {OBS_NAME},ops) or of contributor 1000+i.
func tokenName(i int) string {
	switch {
	case i < 0:
		return ""
	case i >= 1000:
		loadContributorConfig()
		if j := i - 1000; j < len(contribNames) && contribNames[j] != "" {
			return contribNames[j]
		}
		return "contributor-" + strconv.Itoa(i-1000)
	}
	loadTierConfig()
	if i < len(tierNames) && tierNames[i] != "" {
		return tierNames[i]
	}
	return "token-" + strconv.Itoa(i)
}

// neWriterName — the ONE peer allowed to write declared/observed layers
// (POST /{kg}/obs, /{kg} whole-KG) and to read the draft share. Matched by
// token NAME, so srtm may hold a private or a contributor token (or rotate
// between them) without the gate moving to another slot. NE_WRITER overrides.
func neWriterName() string {
	if v := strings.TrimSpace(os.Getenv("NE_WRITER")); v != "" {
		return v
	}
	return peers().ObsName
}

// isNEWriter — request carries a token (private or contributor) named
// neWriterName on a private/contrib tier path.
func isNEWriter(r *http.Request) bool {
	if !isPrivateTier(r) {
		return false
	}
	_, name := reporterID(r)
	return name != "" && name == neWriterName()
}

// ── per-token rate gate ───────────────────────────────────────────────────────

var (
	neReportPerSec  = 2.0
	neReportBurst   = 30.0
	neReportRateMu  sync.Mutex
	neReportBuckets = map[int]*neBucketT{}
)

func neReportRateAllow(idx int, now time.Time) (bool, int) {
	neReportRateMu.Lock()
	defer neReportRateMu.Unlock()
	b := neReportBuckets[idx]
	if b == nil {
		b = &neBucketT{tokens: neReportBurst, last: now}
		neReportBuckets[idx] = b
	}
	b.tokens = math.Min(neReportBurst, b.tokens+now.Sub(b.last).Seconds()*neReportPerSec)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, int(math.Ceil((1 - b.tokens) / neReportPerSec))
}

// ── per-token disk-write budget ──────────────────────────────────────────────
//
// Everything a peer token may persist here (report lines, chunk-log rows, obs
// containers) is append-only and lives on the one disk the service runs from.
// A leaked or misbehaving token must not be able to fill it: each token gets a
// byte bucket refilled at neWriteBudgetBytes per day (default 4 GiB — srtm's
// whole daily feed is well under 1 GiB). Over budget → 429 with Retry-After,
// nothing stored. In-memory; a restart hands out a fresh bucket.

var (
	neWriteBudgetBytes = func() float64 {
		if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("NE_WRITE_BUDGET_MB"))); err == nil && v > 0 {
			return float64(int64(v) << 20)
		}
		return 4 << 30
	}()
	// NE_WRITE_BUDGET_MB_OVERRIDES="name:MB,name:MB" — per-token-name budgets
	// (e.g. {OBS_NAME}:5120) that beat the global default.
	neWriteBudgetOverrides = func() map[string]float64 {
		m := map[string]float64{}
		for _, kv := range strings.Split(os.Getenv("NE_WRITE_BUDGET_MB_OVERRIDES"), ",") {
			name, mb, ok := strings.Cut(strings.TrimSpace(kv), ":")
			if v, err := strconv.Atoi(strings.TrimSpace(mb)); ok && err == nil && v > 0 && name != "" {
				m[strings.TrimSpace(name)] = float64(int64(v) << 20)
			}
		}
		return m
	}()
	neWriteBudgetMu sync.Mutex
	neWriteBudgets  = map[int]*neBucketT{}
)

// neWriteBudgetFor — daily byte budget of token idx (override by name, else global).
func neWriteBudgetFor(idx int) float64 {
	if v, ok := neWriteBudgetOverrides[tokenName(idx)]; ok {
		return v
	}
	return neWriteBudgetBytes
}

func neWriteBudgetAllow(idx int, n int, now time.Time) (bool, int) {
	neWriteBudgetMu.Lock()
	defer neWriteBudgetMu.Unlock()
	budget := neWriteBudgetFor(idx)
	b := neWriteBudgets[idx]
	if b == nil {
		b = &neBucketT{tokens: budget, last: now}
		neWriteBudgets[idx] = b
	}
	perSec := budget / 86400
	b.tokens = math.Min(budget, b.tokens+now.Sub(b.last).Seconds()*perSec)
	b.last = now
	if b.tokens >= float64(n) {
		b.tokens -= float64(n)
		return true, 0
	}
	return false, int(math.Ceil((float64(n) - b.tokens) / perSec))
}

// neWriteBudgetGate — charge len(body) to the token; false ⇒ 429 written.
func neWriteBudgetGate(w http.ResponseWriter, idx int, n int) bool {
	ok, wait := neWriteBudgetAllow(idx, n, time.Now())
	if ok {
		return true
	}
	neReportCounters.bump(idx, "", "rate_limited")
	w.Header().Set("Retry-After", strconv.Itoa(max(wait, 1)))
	w.Header().Set("Cache-Control", "no-store")
	apiError(w, fmt.Sprintf("write budget: %.0f MB/day per token — retry after Retry-After", neWriteBudgetFor(idx)/(1<<20)), http.StatusTooManyRequests)
	return false
}

// neReportGate — private tier, any UMFELD_TOKEN or CONTRIBUTOR_TOKEN;
// returns the reporter key (tokenName(key) is the label).
func neReportGate(w http.ResponseWriter, r *http.Request) (int, bool) {
	if !isPrivateTier(r) {
		tierNotFound(w)
		return -1, false
	}
	idx, _ := reporterID(r)
	if idx < 0 {
		tierNotFound(w)
		return -1, false
	}
	if r.Method == http.MethodPost {
		if ok, wait := neReportRateAllow(idx, time.Now()); !ok {
			neReportCounters.bump(idx, "", "rate_limited")
			w.Header().Set("Retry-After", strconv.Itoa(max(wait, 1)))
			w.Header().Set("Cache-Control", "no-store")
			apiError(w, fmt.Sprintf("rate limit: %.0f reports/s per token (burst %.0f) — queue and retry after Retry-After", neReportPerSec, neReportBurst), http.StatusTooManyRequests)
			return idx, false
		}
	}
	return idx, true
}

// ── in-process counters (watch the first observer closely) ───────────────────

type neReportObsStat struct {
	Token       string          `json:"token"`
	Observer    string          `json:"observer,omitempty"`
	Reports     int             `json:"reports"`
	Rejected    int             `json:"rejected_400"`
	RateLimited int             `json:"rate_limited_429"`
	Compared    int             `json:"compared"`
	NotCompared int             `json:"not_compared"`
	Baselines   int             `json:"became_baseline"`
	Unchanged   int             `json:"unchanged"`
	ChunksSame  int             `json:"chunks_same"`
	ChunksChg   int             `json:"chunks_changed"`
	ChunksUnk   int             `json:"chunks_unknown"`
	ChunksSL    int             `json:"chunks_changed_since_last"`
	KGs         map[string]bool `json:"-"`
	KGsN        int             `json:"kgs"`
	FirstAt     string          `json:"first_at,omitempty"`
	LastAt      string          `json:"last_at,omitempty"`
	LastKG      string          `json:"last_kg,omitempty"`
	LastSource  string          `json:"last_source,omitempty"`
	LastNote    string          `json:"last_note,omitempty"`
	LastChanged []string        `json:"last_chunks_changed,omitempty"`
}

type neReportCountersT struct {
	mu    sync.Mutex
	since time.Time
	by    map[string]*neReportObsStat // key token|observer
	last  []map[string]interface{}    // ring of the last 50 outcomes
}

var neReportCounters = &neReportCountersT{since: time.Now(), by: map[string]*neReportObsStat{}}

func (c *neReportCountersT) stat(idx int, observer string) *neReportObsStat {
	k := tokenName(idx) + "|" + observer
	s := c.by[k]
	if s == nil {
		s = &neReportObsStat{Token: tokenName(idx), Observer: observer, KGs: map[string]bool{}}
		c.by[k] = s
	}
	return s
}

func (c *neReportCountersT) bump(idx int, observer, what string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stat(idx, observer)
	switch what {
	case "rate_limited":
		s.RateLimited++
	case "rejected":
		s.Rejected++
	}
	s.LastAt = time.Now().UTC().Format(time.RFC3339)
}

func (c *neReportCountersT) record(idx int, rep *neReport, resp map[string]interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stat(idx, rep.Observer)
	now := time.Now().UTC().Format(time.RFC3339)
	if s.FirstAt == "" {
		s.FirstAt = now
	}
	s.LastAt, s.LastKG, s.LastSource = now, rep.KG, rep.Source
	s.Reports++
	s.KGs[rep.KG] = true
	s.KGsN = len(s.KGs)
	if resp["compared"] == true {
		s.Compared++
		s.ChunksSame += asInt(resp["chunks_same"])
		if ch, _ := resp["chunks_changed"].([]string); len(ch) > 0 {
			s.ChunksChg += len(ch)
			s.LastChanged = ch
		}
		if u, _ := resp["chunks_unknown_to_us"].([]string); len(u) > 0 {
			s.ChunksUnk += len(u)
		}
	} else {
		s.NotCompared++
	}
	if resp["baseline"] == "this_report" {
		s.Baselines++
	}
	if resp["unchanged"] == true {
		s.Unchanged++
	}
	if sl, _ := resp["since_last"].(map[string]interface{}); sl != nil {
		if ch, _ := sl["changed"].([]string); len(ch) > 0 {
			s.ChunksSL += len(ch)
		}
	}
	if n, _ := resp["note"].(string); n != "" {
		s.LastNote = n
	}
	line := map[string]interface{}{"at": now, "token": tokenName(idx), "observer": rep.Observer, "kg": rep.KG, "source": rep.Source,
		"epoch": rep.Epoch, "compared": resp["compared"], "baseline": resp["baseline"], "chunks_reported": len(rep.Chunks)}
	if ch, _ := resp["chunks_changed"].([]string); len(ch) > 0 {
		line["chunks_changed"] = len(ch)
	}
	if sl, _ := resp["since_last"].(map[string]interface{}); sl != nil {
		if ch, _ := sl["changed"].([]string); len(ch) > 0 {
			line["chunks_changed_since_last"] = len(ch)
		}
	}
	c.last = append(c.last, line)
	if len(c.last) > 50 {
		c.last = c.last[len(c.last)-50:]
	}
}

func asInt(v interface{}) int {
	switch x := v.(type) {
	case int:
		return x
	case float64:
		return int(x)
	}
	return 0
}

// neReportStats — /api/v1/debug/inflight line (cheap, in-memory only).
func neReportStats() map[string]interface{} {
	neReportCounters.mu.Lock()
	defer neReportCounters.mu.Unlock()
	out := map[string]interface{}{"since": neReportCounters.since.UTC().Format(time.RFC3339), "observers": len(neReportCounters.by)}
	total, rl, rej := 0, 0, 0
	for _, s := range neReportCounters.by {
		total += s.Reports
		rl += s.RateLimited
		rej += s.Rejected
	}
	out["reports"], out["rate_limited_429"], out["rejected_400"] = total, rl, rej
	if n := len(neReportCounters.last); n > 0 {
		out["last"] = neReportCounters.last[n-1]
	}
	return out
}

// ── stored reports (jsonl) ────────────────────────────────────────────────────

type neStoredReport struct {
	ReceivedAt string                 `json:"received_at"`
	Token      string                 `json:"token,omitempty"`
	Unchanged  bool                   `json:"unchanged,omitempty"`
	Report     neReport               `json:"report"`
	Result     map[string]interface{} `json:"result"`
}

func neReadReports(kg string) []neStoredReport {
	f, err := os.Open(neFilePath(kg, "reports.jsonl"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []neStoredReport
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20) // grows on demand; a fixed 1 MB × 7 850 files was 8 GB of churn per scan
	for sc.Scan() {
		var r neStoredReport
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.Report.KG != "" {
			out = append(out, r)
		}
	}
	return out
}

// neChunkSeen — one observation of a res-10 chunk digest.
type neChunkSeen struct {
	Digest, At, Observer, Token, Epoch string
	Layout                             string // neLayoutKey of the report that produced the digest (layout rule)
}

// neLayoutKey — the request layout a report's chunk digests depend on.
// Pre-v0.3.3 bevdirect builds (srtm's KG-bbox viewports, unaligned multi-cell
// fetches) truncated parcels at the request edge, so their digests are a
// function of the bbox: each bbox is its own layout. From v0.3.3 (aligned
// 0.02° cells, ne-cells-2 pins the input domain per KG) the bbox only selects
// which chunks are reported (chunk fully inside) and never changes a digest —
// measured 2026-10-24 over the store: aligned→aligned pairs under a different
// bbox differ on 0.04 % of shared chunks (same bbox: 0.12 %), with no edge
// concentration; legacy→aligned differ on 3.8 %, half of them within 0.01° of
// the bbox edge. So all aligned builds are ONE layout: {MAP_NAME}'s viewport
// report is a valid second sighting of srtm's cell-block candidates.
func neLayoutKey(src string, bbox []float64) string {
	if neAlignedSource(src) {
		return "aligned"
	}
	return neBBoxKey(bbox)
}

// neAlignedSource — bevdirect@vX.Y.Z with X.Y.Z ≥ 0.3.3 (aligned-cell fleet).
func neAlignedSource(src string) bool {
	v, ok := strings.CutPrefix(src, "bevdirect@v")
	if !ok {
		return false
	}
	parts := strings.Split(v, ".")
	if len(parts) < 3 {
		return false
	}
	n := [3]int{}
	for i := 0; i < 3; i++ {
		x, err := strconv.Atoi(strings.TrimRight(parts[i], "abcdefghijklmnopqrstuvwxyz-+"))
		if err != nil {
			return false
		}
		n[i] = x
	}
	return n[0] > 0 || n[1] > 3 || (n[1] == 3 && n[2] >= 3)
}

// neChunkHistory — per-chunk first-seen (epoch 0) and last-seen digests over
// every stored full report of the same (source-equivalent, algo, chunk_res) for
// this KG. The baseline is per CHUNK, not per report: viewport-driven observers
// ({MAP_NAME}) post 2–6 km bboxes, so one KG's baseline is the union of all the
// chunks ever reported for it, each pinned to the digest it was first seen with.
// `Last` carries the most recent digest per chunk so a report can also be
// answered with "changed since the previous observation" (change over time),
// not only "changed since epoch 0".
type neChunkHistory struct {
	Lossy           int // comparable reports skipped by the coverage rule
	First, Last     map[string]neChunkSeen
	Reports         int
	FirstAt, LastAt string
	FirstReport     *neStoredReport
}

func neReportHistory(stored []neStoredReport, rep *neReport) *neChunkHistory {
	want := neBevEquivalent(rep.Source)
	h := &neChunkHistory{First: map[string]neChunkSeen{}, Last: map[string]neChunkSeen{}}
	best := neBestCoverage(stored, rep)
	layout := neLayoutKey(rep.Source, rep.BBox)
	for i := range stored {
		s := &stored[i]
		if s.Unchanged || len(s.Report.Chunks) == 0 {
			continue
		}
		if neBevEquivalent(s.Report.Source) != want || s.Report.Algo != rep.Algo || s.Report.ChunkRes != rep.ChunkRes {
			continue
		}
		if neCoverageLossy(s.Report.CellsN, best[i]) {
			h.Lossy++
			continue
		}
		h.Reports++
		// Epoch 0 is anchored on the first report under the SAME layout as
		// rep (neLayoutKey: legacy builds are per bbox, aligned builds one
		// class): a chunk first seen under a legacy layout is re-anchored on
		// its first aligned sighting instead of being reported "changed vs
		// epoch 0" forever. Chunks never seen under the current layout keep
		// their old anchor (since_last then flags them layout_change).
		sLayout := neLayoutKey(s.Report.Source, s.Report.BBox)
		sameLayout := sLayout == layout
		if h.FirstReport == nil || (sameLayout && neLayoutKey(h.FirstReport.Report.Source, h.FirstReport.Report.BBox) != layout) {
			h.FirstReport, h.FirstAt = s, s.ReceivedAt
		}
		h.LastAt = s.ReceivedAt
		for c, d := range s.Report.Chunks {
			seen := neChunkSeen{Digest: d, At: s.ReceivedAt, Observer: s.Report.Observer, Token: s.Token, Epoch: s.Report.Epoch, Layout: sLayout}
			if f, ok := h.First[c]; !ok || (sameLayout && f.Layout != layout) {
				h.First[c] = seen
			}
			h.Last[c] = seen
		}
	}
	return h
}

// Coverage rule (docs/ne-cells.md §coverage). A report built from unaligned /
// multi-cell bevdirect viewports loses the land of parcels wider than
// cell+pad (compose keeps one truncated copy, ne_cells unions ALL copies from
// aligned cells) — cells_n drops by 0.6–6 % and hundreds of chunk digests
// around those parcels differ, which is not land-use change (75110/73008,
// 2026-10-08 rebuild: a v0.3.1 aligned report equals a local v0.3.2 aligned
// build bit for bit; the tag is NOT the classifier). The same holds for a
// build truncated by an incomplete fetch (a missing tile, a partial cell set).
//
// Reference (bbox-independent since 2026-10-24, in step with neLayoutKey): a
// build's cells_n is a function of its bbox (cells fully inside), monotone in
// it — a wider box holds every cell of a narrower one. So the complete build
// for rep is the one with the most cells among comparable reports whose bbox
// is CONTAINED in rep's (the identical bbox included): reports more than
// neCoverageTol below that are "lossy" — they never form a baseline nor a
// "last" digest, and their own diffs are change_suspect. Measured over the
// store 2026-10-24: 1 601 aligned pairs with a contained bbox, 0 in which the
// wider build had fewer cells; 0 of 7 551 stored verdicts flip against the
// old per-identical-bbox rule. A viewport observer ({MAP_NAME}) covering the
// cell block a revisit asks for therefore has srtm's block as its reference
// (every {MAP_NAME} revisit so far: 710 same bbox, 214 wider, 0 without). Land-use
// change does not reduce cells_n (land does not vanish). A narrower viewport
// gets only itself as reference (its cells_n is naturally smaller) — the chunk
// log catches the rest per chunk (ne_change.go deltasOf, `coverage` status).
const neCoverageTol = 0.001

func neBBoxKey(b []float64) string {
	if len(b) != 4 {
		return ""
	}
	return fmt.Sprintf("%.4f,%.4f,%.4f,%.4f", b[0], b[1], b[2], b[3])
}

// neBBoxContains — outer ⊇ inner (w,s,e,n; 1e-6° tolerance, equal boxes included).
func neBBoxContains(outer, inner []float64) bool {
	if len(outer) != 4 || len(inner) != 4 {
		return false
	}
	const eps = 1e-6
	return outer[0] <= inner[0]+eps && outer[1] <= inner[1]+eps && outer[2] >= inner[2]-eps && outer[3] >= inner[3]-eps
}

func neCoverageLossy(cells, best int) bool {
	return best > 0 && cells > 0 && float64(cells) < float64(best)*(1-neCoverageTol)
}

// neCoverageComparable — same source class, algo and chunk_res, chunk-bearing.
func neCoverageComparable(s *neStoredReport, rep *neReport) bool {
	return len(s.Report.Chunks) > 0 && neBevEquivalent(s.Report.Source) == neBevEquivalent(rep.Source) &&
		s.Report.Algo == rep.Algo && s.Report.ChunkRes == rep.ChunkRes
}

// neCoverageRef — the reference cells_n for `of`: the most cells among `of`
// itself, rep and the comparable stored reports whose bbox is contained in
// of's bbox. 0 when `of` has no bbox.
func neCoverageRef(stored []neStoredReport, rep *neReport, of *neReport) int {
	if len(of.BBox) != 4 {
		return 0
	}
	best := of.CellsN
	if rep != nil && rep != of && neBevEquivalent(rep.Source) == neBevEquivalent(of.Source) && rep.Algo == of.Algo && rep.ChunkRes == of.ChunkRes &&
		neBBoxContains(of.BBox, rep.BBox) && rep.CellsN > best {
		best = rep.CellsN
	}
	for i := range stored {
		s := &stored[i]
		if &s.Report == of || !neCoverageComparable(s, of) {
			continue
		}
		if neBBoxContains(of.BBox, s.Report.BBox) && s.Report.CellsN > best {
			best = s.Report.CellsN
		}
	}
	return best
}

// neBestCoverage — reference cells_n per stored report (by index) and for rep
// (key -1), over the comparable history AND rep.
func neBestCoverage(stored []neStoredReport, rep *neReport) map[int]int {
	best := map[int]int{-1: neCoverageRef(stored, rep, rep)}
	for i := range stored {
		if neCoverageComparable(&stored[i], rep) {
			best[i] = neCoverageRef(stored, rep, &stored[i].Report)
		}
	}
	return best
}

// neCoverageOf — the coverage verdict for rep itself against the history.
func neCoverageOf(stored []neStoredReport, rep *neReport) map[string]interface{} {
	k := neBBoxKey(rep.BBox)
	if k == "" {
		return map[string]interface{}{"status": "no_bbox"}
	}
	best := neCoverageRef(stored, rep, rep)
	out := map[string]interface{}{"cells_n": rep.CellsN, "best_cells_n": best, "bbox": k, "reference": "contained_bbox"}
	switch {
	case best == rep.CellsN:
		out["status"] = "best"
	case neCoverageLossy(rep.CellsN, best):
		out["status"] = "lossy"
		out["deficit_pct"] = 100 * (1 - float64(rep.CellsN)/float64(best))
	default:
		out["status"] = "within_tolerance"
	}
	return out
}

func (h *neChunkHistory) digests(m map[string]neChunkSeen) map[string]string {
	out := make(map[string]string, len(m))
	for c, s := range m {
		out[c] = s.Digest
	}
	return out
}

// neSinceLast — compare a report against the most recent digest we hold per
// chunk (from earlier reports of the same source). Nil when nothing overlaps.
func neSinceLast(h *neChunkHistory, rep *neReport) map[string]interface{} {
	if h == nil || len(h.Last) == 0 {
		return nil
	}
	same, changed, _ := neCompareChunks(h.digests(h.Last), rep.Chunks)
	if same+len(changed) == 0 {
		return nil
	}
	lastAt := ""
	for c := range rep.Chunks {
		if s, ok := h.Last[c]; ok && s.At > lastAt {
			lastAt = s.At
		}
	}
	out := map[string]interface{}{"compared": same + len(changed), "same": same, "changed": changed, "previous_received_at": lastAt}
	// Layout rule: a diff against a digest built under a different layout
	// (legacy KG-bbox viewport → aligned cells, neLayoutKey) is a re-baseline,
	// not change. Aligned vs aligned under any bbox is a real comparison.
	k, n := neLayoutKey(rep.Source, rep.BBox), 0
	for _, c := range changed {
		if s := h.Last[c]; s.Layout != k {
			n++
		}
	}
	if n > 0 {
		out["changed_previous_bbox_differs"] = n
	}
	return out
}

// neLayoutSuspect — the since_last diffs of rep were (at least partly) taken
// against digests from a report with a different bbox. The 75110/73008
// evidence (§coverage) shows request layout alone moves hundreds of chunk
// digests; across a layout switch nothing separates that from land-use
// change, so such diffs are change_suspect:"layout_change" and the public
// m² change figure counts only same-bbox (cell-block vs cell-block) diffs.
func neLayoutSuspect(sl map[string]interface{}) bool {
	if sl == nil {
		return false
	}
	switch v := sl["changed_previous_bbox_differs"].(type) {
	case int:
		return v > 0
	case float64:
		return v > 0
	}
	return false
}

// neMarkLossy — retroactively apply the coverage rule to a KG's stored lines
// (lines written before the rule existed carry no change_suspect): a report
// below the best cells_n seen for a bbox contained in its own (neCoverageRef)
// gets change_suspect=coverage_lossy in its (in-memory) Result. Mutates and
// returns the slice.
func neMarkLossy(reports []neStoredReport) []neStoredReport {
	ck := func(s *neStoredReport) string {
		return neBevEquivalent(s.Report.Source) + "|" + s.Report.Algo + "|" + strconv.Itoa(s.Report.ChunkRes)
	}
	best := make([]int, len(reports))
	for i := range reports {
		s := &reports[i]
		if len(s.Report.Chunks) == 0 || neBBoxKey(s.Report.BBox) == "" {
			continue
		}
		best[i] = s.Report.CellsN
		for j := range reports {
			o := &reports[j]
			if j == i || len(o.Report.Chunks) == 0 || ck(o) != ck(s) || !neBBoxContains(s.Report.BBox, o.Report.BBox) {
				continue
			}
			if o.Report.CellsN > best[i] {
				best[i] = o.Report.CellsN
			}
		}
	}
	// Lines written before the rule compared against whatever came last; if
	// that was a lossy build their since_last diffs are its artefacts.
	lastLossy := map[string]bool{}    // per (class|algo|res): the previous full report was lossy
	lastLayout := map[string]string{} // per (class|algo|res): neLayoutKey of the previous good full report
	for i := range reports {
		s := &reports[i]
		if len(s.Report.Chunks) == 0 || s.Result == nil {
			continue
		}
		k := ck(s)
		sl, _ := s.Result["since_last"].(map[string]interface{})
		slChanged := 0
		if sl != nil {
			if ch, _ := sl["changed"].([]interface{}); ch != nil {
				slChanged = len(ch)
			}
		}
		myLayout := neLayoutKey(s.Report.Source, s.Report.BBox)
		layoutSus := slChanged > 0 && lastLayout[k] != "" && lastLayout[k] != myLayout
		lossy := neCoverageLossy(s.Report.CellsN, best[i])
		switch {
		case s.Result["change_suspect"] == "layout_change" && !layoutSus && neAlignedSource(s.Report.Source) && lastLayout[k] == myLayout:
			// stored under the pre-2026-10-24 per-bbox rule ({MAP_NAME}'s viewport
			// revisits of srtm cell blocks): aligned vs aligned is not layout
			delete(s.Result, "change_suspect")
		case neStoredSuspect(s):
		case lossy:
			s.Result["change_suspect"] = "coverage_lossy"
		case lastLossy[k]:
			s.Result["change_suspect"] = "previous_lossy"
		case layoutSus, neLayoutSuspect(sl) && !neAlignedSource(s.Report.Source):
			s.Result["change_suspect"] = "layout_change"
		}
		lastLossy[k] = lossy
		if !lossy {
			lastLayout[k] = myLayout
		}
	}
	return reports
}

// neStoredSuspect — the stored result carries change_suspect (coverage rule).
func neStoredSuspect(s *neStoredReport) bool {
	v, _ := s.Result["change_suspect"].(string)
	return v != ""
}

// neReportBaseline — the first stored report with the same (source, algo,
// chunk_res) that still carries its chunks (not a dedupe stub). Kept for the
// per-report attribution fields; chunk comparison uses neReportHistory.
func neReportBaseline(stored []neStoredReport, rep *neReport) *neStoredReport {
	return neReportHistory(stored, rep).FirstReport
}

// neReportPrevious — the latest stored report of the same token+source (for dedupe).
func neReportPrevious(stored []neStoredReport, token string, rep *neReport) *neStoredReport {
	for i := len(stored) - 1; i >= 0; i-- {
		s := &stored[i]
		if s.Token == token && neBevEquivalent(s.Report.Source) == neBevEquivalent(rep.Source) && s.Report.Algo == rep.Algo {
			return s
		}
	}
	return nil
}

var (
	neHex16Re  = regexp.MustCompile(`^[0-9a-f]{16}$`)
	neH3HexRe  = regexp.MustCompile(`^[0-9a-f]{15,16}$`)
	neMaxChunk = 50000
)

// neReportValidate — shape checks beyond the required fields.
func neReportValidate(rep *neReport) string {
	if !neHex16Re.MatchString(rep.Digest) {
		return "digest must be sha256[:16] hex (16 lowercase hex chars)"
	}
	if rep.Source == "" {
		return "source required (index-YYYY-MM or bevdirect@<tag>)"
	}
	if rep.H3Res != 0 && rep.H3Res != 12 {
		return "h3_res must be 12"
	}
	if rep.ChunkRes != 0 && rep.ChunkRes != 10 {
		return "chunk_res must be 10"
	}
	if len(rep.Chunks) > neMaxChunk {
		return fmt.Sprintf("too many chunks (%d > %d)", len(rep.Chunks), neMaxChunk)
	}
	if len(rep.Observer) > 64 || strings.ContainsAny(rep.Observer, "\n\r\t") {
		return "observer: ≤ 64 chars, one line"
	}
	// Every string that ends up in a stored line is bounded: a report line
	// must stay a few hundred bytes, whatever the body (≤ 8 MB) carried.
	for k, v := range map[string]string{"algo": rep.Algo, "epoch": rep.Epoch, "source": rep.Source, "ap_version": rep.APVersion} {
		if len(v) > 64 || strings.ContainsAny(v, "\n\r\t") {
			return k + ": ≤ 64 chars, one line"
		}
	}
	if rep.Tiles != nil && len(rep.Tiles.Hash) > 64 {
		return "tiles.hash: ≤ 64 chars"
	}
	n := 0
	for c, d := range rep.Chunks {
		if !neH3HexRe.MatchString(c) || !neHex16Re.MatchString(d) {
			return fmt.Sprintf("chunks: keys are res-10 H3 ids (hex), values sha256[:16] hex — bad entry %q:%q", c, d)
		}
		if n++; n >= 2000 {
			break
		}
	}
	if len(rep.BBox) != 0 && len(rep.BBox) != 4 {
		return "bbox must be [W,S,E,N]"
	}
	return ""
}

// neCompareChunks — ours vs reported.
func neCompareChunks(ours, theirs map[string]string) (same int, changed, unknown []string) {
	for c, d := range theirs {
		switch o, ok := ours[c]; {
		case !ok:
			unknown = append(unknown, c)
		case o == d:
			same++
		default:
			changed = append(changed, c)
		}
	}
	sort.Strings(changed)
	sort.Strings(unknown)
	if changed == nil {
		changed = []string{}
	}
	if unknown == nil {
		unknown = []string{}
	}
	return
}

// neAppendReport — one jsonl line under neWriteMu.
func neAppendReport(kg string, line interface{}) {
	b, _ := json.Marshal(line)
	neWriteMu.Lock()
	defer neWriteMu.Unlock()
	_ = os.MkdirAll(neDir, 0o755)
	f, err := os.OpenFile(neFilePath(kg, "reports.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()
	neReportsScanMemo.Lock()
	neReportsScanMemo.dirty = true
	neReportsScanMemo.Unlock()
}

// ── ops view: GET <private>/api/v1/ne/reports ───────────────────────────────────────

var neReportsScanMemo = struct {
	sync.Mutex
	at    time.Time
	dir   string // neDir the memo was built for (tests swap it)
	dirty bool   // a report was appended since the last scan
	data  map[string]interface{}
	kgs   map[string]neKGReportStat // per-KG chunk-bearing report facts (api_ne_revisit.go `stale`), not in `data`
}{}

// neReportKGStats — per-KG facts from the same walk as neScanReportFiles (no
// second walk): KG → chunk-bearing, non-lossy reports, first/last, last observer.
func neReportKGStats() map[string]neKGReportStat {
	neScanReportFiles()
	neReportsScanMemo.Lock()
	defer neReportsScanMemo.Unlock()
	return neReportsScanMemo.kgs
}

// neScanReportFiles walks data/ne_cells/*.reports.jsonl — memoised 10 min, or until the next append (≥ 5 s apart).
func neScanReportFiles() map[string]interface{} {
	neReportsScanMemo.Lock()
	defer neReportsScanMemo.Unlock()
	// fresh enough: same dir, < 60 s old, and nothing appended since — or
	// appended but rescanned < 5 s ago (report bursts must not turn every
	// ops/landing read into a full walk).
	if m := &neReportsScanMemo; m.data != nil && m.dir == neDir {
		age := time.Since(m.at)
		if age < 10*time.Minute && (!m.dirty || age < 5*time.Second) {
			return m.data
		}
	}
	type agg struct {
		Reports, Compared, Changed, Unchanged, Baselines, ChangedSinceLast, ChangedSuspect int
		KGs                                                                                map[string]bool
		First, Last                                                                        string
		Sources                                                                            map[string]int
	}
	by := map[string]*agg{}
	files, _ := filepath.Glob(filepath.Join(neDir, "*.reports.jsonl"))
	kgsWithChange := map[string]int{}
	kgsWithChangeSL := map[string]int{}
	totalLines := 0
	daily := neDailyAgg{}
	glyph := newNEGlyphAgg() // ne_glyph.go: rides on this walk
	kgStats := make(map[string]neKGReportStat, len(files))
	for _, fn := range files {
		kg := strings.TrimSuffix(filepath.Base(fn), ".reports.jsonl")
		for _, s := range neMarkLossy(neReadReports(kg)) {
			totalLines++
			daily.add(kg, s)
			glyph.add(kg, &s)
			if len(s.Report.Chunks) > 0 && !neStoredSuspect(&s) {
				ks := kgStats[kg]
				ks.ChunkReports++
				if ks.FirstAt == "" || s.ReceivedAt < ks.FirstAt {
					ks.FirstAt = s.ReceivedAt
				}
				if s.ReceivedAt >= ks.LastAt {
					ks.LastAt, ks.LastObserver, ks.LastBBox = s.ReceivedAt, s.Report.Observer, neBBoxKey(s.Report.BBox)
				}
				kgStats[kg] = ks
			}
			k := s.Token
			if k == "" {
				k = "(pre-token)"
			}
			k += "|" + s.Report.Observer
			a := by[k]
			if a == nil {
				a = &agg{KGs: map[string]bool{}, Sources: map[string]int{}}
				by[k] = a
			}
			a.Reports++
			a.KGs[kg] = true
			a.Sources[s.Report.Source]++
			if a.First == "" || s.ReceivedAt < a.First {
				a.First = s.ReceivedAt
			}
			if s.ReceivedAt > a.Last {
				a.Last = s.ReceivedAt
			}
			if s.Unchanged {
				a.Unchanged++
			}
			if s.Result["compared"] == true {
				a.Compared++
				if ch, _ := s.Result["chunks_changed"].([]interface{}); len(ch) > 0 {
					a.Changed += len(ch)
					kgsWithChange[kg] += len(ch)
				}
			}
			if s.Result["baseline"] == "this_report" {
				a.Baselines++
			}
			if sl, _ := s.Result["since_last"].(map[string]interface{}); sl != nil {
				if ch, _ := sl["changed"].([]interface{}); len(ch) > 0 {
					if neStoredSuspect(&s) {
						a.ChangedSuspect += len(ch)
					} else {
						a.ChangedSinceLast += len(ch)
						kgsWithChangeSL[kg] += len(ch)
					}
				}
			}
		}
	}
	rows := make([]map[string]interface{}, 0, len(by))
	for k, a := range by {
		tok, obs, _ := strings.Cut(k, "|")
		rows = append(rows, map[string]interface{}{"token": tok, "observer": obs, "reports": a.Reports, "kgs": len(a.KGs),
			"compared": a.Compared, "chunks_changed": a.Changed, "chunks_changed_since_last": a.ChangedSinceLast, "chunks_changed_suspect": a.ChangedSuspect, "unchanged": a.Unchanged, "became_baseline": a.Baselines,
			"first_at": a.First, "last_at": a.Last, "sources": a.Sources})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i]["last_at"].(string) > rows[j]["last_at"].(string) })
	changed := make([]map[string]interface{}, 0, len(kgsWithChange))
	for kg, n := range kgsWithChange {
		changed = append(changed, map[string]interface{}{"kg": kg, "chunks_changed": n, "chunks_changed_since_last": kgsWithChangeSL[kg]})
	}
	sort.Slice(changed, func(i, j int) bool { return changed[i]["chunks_changed"].(int) > changed[j]["chunks_changed"].(int) })
	if len(changed) > 50 {
		changed = changed[:50]
	}
	glyph.fold(&daily)
	neReportsScanMemo.at, neReportsScanMemo.dir, neReportsScanMemo.dirty = time.Now(), neDir, false
	neReportsScanMemo.kgs = kgStats
	neReportsScanMemo.data = map[string]interface{}{"files": len(files), "lines": totalLines, "glyph": glyph, "observers": rows, "kgs_with_changes": changed, "daily": daily.rows(), "chunks_ever": func() map[string]int {
		o, t, d := daily.everDepth()
		return map[string]int{"once": o, "twice_or_more": t, "deepest_pass": d}
	}(), "scanned_at": neReportsScanMemo.at.UTC().Format(time.RFC3339)}
	return neReportsScanMemo.data
}

// ── daily tracker (landing page chart, /ne/reports.on_disk.daily) ────────────
//
// Unit: the res-10 chunk (~1.5 ha) a peer digested — NOT the KG (viewport
// observers post 2–6 km slices of a KG, so "KGs" would overstate by 10–100×)
// and NOT the report (dedupe stubs and bursts would double count). Per UTC day
// we count DISTINCT chunks digested, and how many of them already had a digest
// from an earlier day ("reobserved" — the only chunks on which change can be
// detected at all). Area = chunks × neChunkAreaKm2; share = /Austria.

const neChunkAreaKm2 = 0.0150475 // mean H3 res-10 hexagon
const austriaAreaKm2 = 83879.0

type neDayStat struct {
	Day        string `json:"day"` // "2026-10-06" (UTC)
	Reports    int    `json:"reports"`
	KGs        int    `json:"kgs"`
	Chunks     int    `json:"chunks"`
	Reobserved int    `json:"chunks_reobserved"`
	// Changed = chunks whose digest differed from the most recent one held
	// (sum of since_last.changed over the day's reports; a chunk reported
	// twice in one day may count twice). This is the actual change signal —
	// Reobserved is only the share that COULD carry one.
	Changed int `json:"chunks_changed_since_last"`
	// Passes[i] = chunks for which this day is their nePassBands[i]-th
	// distinct observation day (1st, 2nd, 3rd–4th, 5th–9th, 10th+), counted
	// per (chunk, assembly class): a pass under a tag that is not
	// cell-equivalent to the earlier ones is a new 1st — it cannot be compared.
	Passes [nePassBandN]int `json:"chunks_by_pass"`
	// Nth[i] = chunks for which this day is exactly their (i+2)-th distinct
	// observation day (2nd · 3rd · 4th · 5th) — the pass-completion curves of
	// ne_change_passes.go (chunks_by_pass merges 3rd–4th and 5th–9th).
	Nth [4]int `json:"chunks_nth"`
	// Sources = reports per exact source tag that day (rollout tracker).
	Sources map[string]int `json:"reports_by_source,omitempty"`
}

// Pass bands: which observation of a chunk a day represents. Band 0 is the
// first digest ever (new area), the rest are re-observations of increasing
// depth — the deeper the pass, the better the chunk's change history.
const nePassBandN = 5

var nePassBandLabel = [nePassBandN]string{"1st", "2nd", "3rd–4th", "5th–9th", "10th+"}

func nePassBand(n int) int {
	switch {
	case n <= 1:
		return 0
	case n == 2:
		return 1
	case n <= 4:
		return 2
	case n <= 9:
		return 3
	}
	return 4
}

// neDailyAgg — streaming aggregator. Memory is O(distinct chunks) with a
// 12-byte value per chunk (H3 index packed to uint64, day index + pass count),
// not O(days × chunks) string sets: whole-Austria observation (~5.6 M chunks)
// stays ≈ 150 MB transient instead of GBs. Lines must arrive chronologically
// per chunk — true because a chunk lives in exactly one KG file and files are
// append-only; a late (older-day) line still counts as a distinct day but
// cannot move the chunk's "last day".
//
// Passes are counted per assembly class (neBevEquivalent(source)): digests
// from non-equivalent tags (v0.1.0 vs v0.2.0) are never compared by /report,
// so a re-observation under a new class is a fresh 1st pass, not a 2nd. All
// tags in the field since Oct 2026 (v0.2.0 … v0.3.2) are one class, so this
// only matters on the day an assembly-changing tag rolls out — then the chart
// honestly restarts the pass depth for that land instead of promising change
// visibility it cannot deliver. A chunk digested by two classes on one day
// counts once per class (transient during such a rollout; documented).
type neDailyAgg struct {
	first  time.Time                        // day 0
	chunks map[string]map[uint64]neChunkObs // class → chunk → last day idx, distinct-day count
	days   []neDayStat                      // index = days since first
	dayKGs map[uint16]map[string]bool
	hex    map[string]uint64 // fallback for non-hex chunk keys (tests)
}

type neChunkObs struct {
	last uint16 // day index of the latest observation
	n    uint16 // distinct observation days so far
}

func neChunkKey(c string) (uint64, bool) {
	if len(c) == 0 || len(c) > 16 {
		return 0, false
	}
	v, err := strconv.ParseUint(c, 16, 64)
	return v, err == nil
}

func (a *neDailyAgg) dayIndex(day string) (uint16, bool) {
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		return 0, false
	}
	if a.chunks == nil {
		a.chunks, a.dayKGs, a.hex = map[string]map[uint64]neChunkObs{}, map[uint16]map[string]bool{}, map[string]uint64{}
		a.first = t
	}
	if t.Before(a.first) {
		// earlier than anything seen: shift the day axis
		shift := int(a.first.Sub(t).Hours()/24 + 0.5)
		if shift > 400 {
			return 0, false
		}
		a.days = append(make([]neDayStat, shift), a.days...)
		for i := range a.days[:shift] {
			a.days[i].Day = t.AddDate(0, 0, i).Format("2006-01-02")
		}
		for _, cm := range a.chunks {
			for k, o := range cm {
				o.last += uint16(shift)
				cm[k] = o
			}
		}
		nk := map[uint16]map[string]bool{}
		for d, m := range a.dayKGs {
			nk[d+uint16(shift)] = m
		}
		a.dayKGs, a.first = nk, t
	}
	idx := int(t.Sub(a.first).Hours()/24 + 0.5)
	if idx >= 400 {
		return 0, false
	}
	for len(a.days) <= idx {
		a.days = append(a.days, neDayStat{Day: a.first.AddDate(0, 0, len(a.days)).Format("2006-01-02")})
	}
	return uint16(idx), true
}

func (a *neDailyAgg) add(kg string, s neStoredReport) {
	if len(s.ReceivedAt) < 10 {
		return
	}
	d, ok := a.dayIndex(s.ReceivedAt[:10])
	if !ok {
		return
	}
	st := &a.days[d]
	st.Reports++
	if a.dayKGs[d] == nil {
		a.dayKGs[d] = map[string]bool{}
	}
	a.dayKGs[d][kg] = true
	if src := s.Report.Source; src != "" {
		if st.Sources == nil {
			st.Sources = map[string]int{}
		}
		st.Sources[src]++
	}
	if sl, _ := s.Result["since_last"].(map[string]interface{}); sl != nil && !neStoredSuspect(&s) {
		if ch, _ := sl["changed"].([]interface{}); len(ch) > 0 {
			st.Changed += len(ch)
		}
	}
	class := neBevEquivalent(s.Report.Source)
	chunks := a.chunks[class]
	if chunks == nil {
		chunks = map[uint64]neChunkObs{}
		a.chunks[class] = chunks
	}
	for c := range s.Report.Chunks {
		k, ok := neChunkKey(c)
		if !ok {
			k, ok = a.hex[c]
			if !ok {
				k = uint64(len(a.hex)) | 1<<63
				a.hex[c] = k
			}
		}
		o, seen := chunks[k]
		switch {
		case !seen:
			o = neChunkObs{last: d, n: 1}
		case o.last == d:
			continue // same day, same chunk (burst / second observer): one distinct chunk
		case d > o.last:
			o.last, o.n = d, o.n+1
		default:
			o.n++ // out-of-order older line: still a distinct day
		}
		chunks[k] = o
		st.Chunks++
		st.Passes[nePassBand(int(o.n))]++
		if o.n >= 2 && o.n <= 5 {
			st.Nth[o.n-2]++
		}
		if o.n > 1 {
			st.Reobserved++
		}
	}
}

// rows — one entry per UTC day from the first report to TODAY (gaps as zero
// rows), oldest first, at most 400.
func (a *neDailyAgg) rows() []neDayStat {
	if len(a.days) == 0 {
		return nil
	}
	today := time.Now().UTC().Format("2006-01-02")
	out := make([]neDayStat, 0, len(a.days))
	for i := range a.days {
		if a.days[i].Day > today {
			break
		}
		st := a.days[i]
		st.KGs = len(a.dayKGs[uint16(i)])
		out = append(out, st)
	}
	for d := a.first.AddDate(0, 0, len(out)); len(out) < 400; d = d.AddDate(0, 0, 1) {
		day := d.Format("2006-01-02")
		if day > today {
			break
		}
		out = append(out, neDayStat{Day: day})
	}
	return out
}

// ever — distinct chunks digested at least once / on ≥ 2 different days.
func (a *neDailyAgg) ever() (once, twice int) {
	once, twice, _ = a.everDepth()
	return
}

// everDepth — like ever, plus the deepest pass any chunk has reached.
// once = distinct land (union over classes); twice/deepest are per class,
// because only same-class passes are comparable.
func (a *neDailyAgg) everDepth() (once, twice, deepest int) {
	var union map[uint64]struct{}
	if len(a.chunks) > 1 {
		union = map[uint64]struct{}{}
	}
	for _, cm := range a.chunks {
		for k, o := range cm {
			if union != nil {
				union[k] = struct{}{}
			} else {
				once++
			}
			if o.n >= 2 {
				twice++
			}
			if int(o.n) > deepest {
				deepest = int(o.n)
			}
		}
	}
	if union != nil {
		once = len(union)
	}
	return
}

func handleNEReportsOverview(w http.ResponseWriter, r *http.Request) {
	if _, ok := neReportGate(w, r); !ok {
		return
	}
	neReportCounters.mu.Lock()
	live := make([]*neReportObsStat, 0, len(neReportCounters.by))
	for _, s := range neReportCounters.by {
		live = append(live, s)
	}
	last := append([]map[string]interface{}{}, neReportCounters.last...)
	neReportCounters.mu.Unlock()
	sort.Slice(live, func(i, j int) bool { return live[i].LastAt > live[j].LastAt })
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]interface{}{
		"since_process_start": neReportCounters.since.UTC().Format(time.RFC3339),
		"live":                live,
		"last_50":             last,
		"on_disk":             neScanReportFiles(),
		"policy": map[string]interface{}{"reports_per_s_per_token": neReportPerSec, "burst": neReportBurst, "max_body_mb": 8,
			"max_chunks": neMaxChunk, "baseline_rule": "stored section of same source, else per chunk: the digest first reported for it by any report with same (source, algo, chunk_res); since_last = most recent digest per chunk"},
	})
}
