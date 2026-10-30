// ne_change_passes.go — change over time, by PASS.
//
// Every change we hold is dated by the day a peer happened to re-digest the
// chunk, not by the day the register moved. A calendar axis over those dates
// would show the crawl order of the peers. The honest unit is the pass: the
// k-th sighting of a chunk. A delta between the chunk's (k−1)-th and k-th
// sighting "belongs to pass k" — the register moved somewhere between the two
// digests. Nationally a pass is complete when every held chunk has k sightings
// (the second pass ≈ 2 months after the first at the Oct 2026 rate); per KG it
// completes earlier, so the timeline adapts: it reads the KG's own passes while
// a KG is selected and the national ones otherwise, and the calendar (detection
// dates) is only a secondary axis inside a pass. docs/ne-change.md §6e.
//
//   GET /api/v1/ne/glyph/passes[?kg=]      pass table: share · first/done dates · ETA · confirmed/candidate · Δ per group · by detection day
//   tile / cell parameter t=       window: `2` (pass 2) · `2..3` · `2@2026-10-16..2026-10-22` (pass 2, detected in the dates) · `@2026-10-16..2026-10-22`

package main

import (
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// neChangeWin — a window on the deltas: pass ordinals K1..K2 (0 = open) and
// detection days D1..D2 ("" = open), both inclusive.
type neChangeWin struct {
	K1, K2 int
	D1, D2 string
}

func (w neChangeWin) zero() bool { return w.K1 == 0 && w.K2 == 0 && w.D1 == "" && w.D2 == "" }

func (w neChangeWin) String() string {
	if w.zero() {
		return ""
	}
	s := ""
	if w.K1 > 0 || w.K2 > 0 {
		if w.K1 == w.K2 {
			s = strconv.Itoa(w.K1)
		} else {
			s = fmt.Sprintf("%d..%d", w.K1, w.K2)
		}
	}
	if w.D1 != "" || w.D2 != "" {
		s += "@" + w.D1
		if w.D2 != w.D1 {
			s += ".." + w.D2
		}
	}
	return s
}

// has — does a delta detected at `at` on pass `k` fall inside the window?
func (w neChangeWin) has(k int, at string) bool {
	if w.K1 > 0 && k < w.K1 || w.K2 > 0 && k > w.K2 {
		return false
	}
	if len(at) >= 10 {
		at = at[:10]
	}
	if w.D1 != "" && at < w.D1 || w.D2 != "" && at > w.D2 {
		return false
	}
	return true
}

var neWinRe = regexp.MustCompile(`^(?:(\d{1,3})(?:\.\.(\d{1,3}))?)?(?:@(\d{4}-\d{2}-\d{2})(?:\.\.(\d{4}-\d{2}-\d{2}))?)?$`)

// neParseWin — "" → zero window; malformed → ok=false.
func neParseWin(s string) (neChangeWin, bool) {
	var w neChangeWin
	if s == "" {
		return w, true
	}
	m := neWinRe.FindStringSubmatch(s)
	if m == nil {
		return w, false
	}
	if m[1] != "" {
		w.K1, _ = strconv.Atoi(m[1])
		w.K2 = w.K1
		if m[2] != "" {
			w.K2, _ = strconv.Atoi(m[2])
		}
		if w.K1 < 2 || w.K2 < w.K1 {
			return w, false
		}
	}
	if m[3] != "" {
		w.D1, w.D2 = m[3], m[3]
		if m[4] != "" {
			w.D2 = m[4]
		}
		if w.D2 < w.D1 {
			return w, false
		}
	}
	return w, true
}

// ── per KG: sighting ordinals and pass table ─────────────────────────────────

// nePassKG — the pass table of one KG's chunk log.
type nePassKG struct {
	Chunks int
	ord    map[int]int // version entry index → sighting ordinal of its chunk (1 = baseline)
	dates  [][]string  // per chunk (sorted by id): sighting days, ascending, ≤ nePassKeep
}

const nePassKeep = 12

// neDoneShare — a pass counts as complete at 99.5 % of the chunks: slivers
// and lossy chunks on KG edges never close, and a random revisit's last
// fraction of a percent would hold the timeline open for months.
const neDoneShare = 0.995

// passOrdinals — entry index → ordinal, for version entries.
func (c *neChangeKG) passOrdinals() map[int]int { return c.passes().ord }

// passes — computed once per loaded KG (entries are immutable after load).
func (c *neChangeKG) passes() *nePassKG {
	c.passMu.Lock()
	defer c.passMu.Unlock()
	if c.pass != nil {
		return c.pass
	}
	p := &nePassKG{ord: map[int]int{}}
	cnt := map[uint64]int{}
	days := map[uint64][]string{}
	note := func(ch uint64, at string) {
		cnt[ch]++
		if len(at) >= 10 && len(days[ch]) < nePassKeep {
			days[ch] = append(days[ch], at[:10])
		}
	}
	for i, e := range c.entries {
		switch e.Kind {
		case "version":
			ch, err := strconv.ParseUint(e.Chunk, 16, 64)
			if err != nil {
				continue
			}
			note(ch, e.At)
			p.ord[i] = cnt[ch]
		case "seen":
			for chs := range e.Chunks {
				if ch, err := strconv.ParseUint(chs, 16, 64); err == nil && cnt[ch] > 0 {
					note(ch, e.At)
				}
			}
		}
	}
	ids := make([]uint64, 0, len(cnt))
	for ch := range cnt {
		ids = append(ids, ch)
	}
	sortU64(ids)
	p.Chunks = len(ids)
	p.dates = make([][]string, len(ids))
	for k, ch := range ids {
		d := days[ch]
		sort.Strings(d) // entries are appended in arrival order, which may differ from `at`
		p.dates[k] = d
	}
	c.pass = p
	c.rowsMu.Lock()
	c.ram += len(p.ord)*16 + p.Chunks*nePassKeep*12
	c.rowsMu.Unlock()
	return p
}

// nePassRow — one pass of a scope (KG or national), as served.
type nePassRow struct {
	K      int     `json:"k"`
	Share  float64 `json:"share"`           // chunks with ≥ k sightings / chunks
	First  string  `json:"first,omitempty"` // day the first chunk reached k
	Done   string  `json:"done,omitempty"`  // day the last chunk reached k (pass complete)
	ETA    string  `json:"eta,omitempty"`   // national: projected completion month at the current pace (neDoneShare reached)
	ETADay string  `json:"eta_day,omitempty"`
	Rate   int     `json:"rate_per_day,omitempty"` // national: chunks reaching k per day, 7-day mean
	// change detected on this pass (= deltas whose new version is the chunk's k-th sighting)
	Confirmed int                        `json:"confirmed"`
	Candidate int                        `json:"candidate"`
	DLU       [9]int                     `json:"d_lu_m2"`
	DN        int                        `json:"d_str_n"`
	From      string                     `json:"from,omitempty"` // detection days of the confirmed deltas: earliest · latest
	To        string                     `json:"to,omitempty"`
	Days      map[string]*neChangeTotals `json:"by_day,omitempty"` // confirmed, by detection day
	CandDays  map[string]int             `json:"candidate_by_day,omitempty"`
}

// table — Share / First / Done per pass k = 2.. up to the deepest sighting.
func (p *nePassKG) table() []nePassRow {
	deep := 0
	for _, d := range p.dates {
		if len(d) > deep {
			deep = len(d)
		}
	}
	var out []nePassRow
	for k := 2; k <= deep && k <= nePassKeep; k++ {
		r := nePassRow{K: k}
		n := 0
		for _, d := range p.dates {
			if len(d) >= k {
				n++
				if r.First == "" || d[k-1] < r.First {
					r.First = d[k-1]
				}
				if d[k-1] > r.Done {
					r.Done = d[k-1]
				}
			}
		}
		if p.Chunks > 0 {
			r.Share = float64(n) / float64(p.Chunks)
		}
		if float64(n) < neDoneShare*float64(p.Chunks) {
			r.Done = "" // not (nearly) every chunk has k sightings yet
		}
		out = append(out, r)
	}
	return out
}

// ── cumulation with a window ─────────────────────────────────────────────────

// cumulate — fold delta rows into per-cell cumulative state (the loop that
// lived in deltasOf), counting only rows inside the window. QC-excluded rows
// are never cumulated; absent rows mark the cell but add nothing.
func (c *neChangeKG) cumulate(rows []neCellDelta, w neChangeWin) map[uint64]*neCellCum {
	cum := map[uint64]*neCellCum{}
	for i := range rows {
		x := &rows[i]
		if x.Coverage || x.SameInput || x.Jitter {
			continue
		}
		if !w.zero() && !w.has(x.Pass, x.At) {
			continue
		}
		cu := cum[x.Cell]
		if cu == nil {
			cu = &neCellCum{FromAP: c.entries[x.From].AP}
			cum[x.Cell] = cu
		}
		if x.Absent {
			continue
		}
		at := x.At
		if cu.FirstAt == "" || at < cu.FirstAt {
			cu.FirstAt = at
		}
		if at > cu.AnyAt {
			cu.AnyAt = at
		}
		if cu.Pass == 0 || x.Pass < cu.Pass {
			cu.Pass = x.Pass
		}
		if x.Pass > cu.PassLast {
			cu.PassLast = x.Pass
		}
		if x.Confirmed {
			for j := range cu.DLU {
				cu.DLU[j] += x.DLU[j]
			}
			cu.DCover += x.DCover
			cu.DN += x.DN
			cu.DLine += x.DLine
			cu.Confirmed, cu.ToAP, cu.LastAt = true, c.entries[x.To].AP, at
		} else {
			cu.Candidate = true
			for j := range cu.CandDLU {
				cu.CandDLU[j] += x.DLU[j]
			}
		}
		cu.Share = x.Share
		cu.Unknown = cu.Unknown || x.Unknown
	}
	return cum
}

const neWinCacheMax = 4 // windows kept per loaded KG (a tile burst asks one window a dozen times)

// cumWin — d.cum for the zero window, else a windowed fold (cached per KG).
func (c *neChangeKG) cumWin(d *neChangeDeltas, w neChangeWin) map[uint64]*neCellCum {
	if w.zero() {
		return d.cum
	}
	key := w.String()
	c.passMu.Lock()
	defer c.passMu.Unlock()
	if m, ok := c.winCum[key]; ok {
		return m
	}
	m := c.cumulate(d.rows, w)
	if c.winCum == nil {
		c.winCum = map[string]map[uint64]*neCellCum{}
	}
	if len(c.winCum) >= neWinCacheMax {
		for k := range c.winCum {
			delete(c.winCum, k)
			break
		}
	}
	c.winCum[key] = m
	return m
}

// ── the pass table of a KG, with the change detected on each pass ────────────

func nePassTableKG(kg string) ([]nePassRow, int, bool) {
	c := neChangeGet(kg)
	if c == nil {
		return nil, 0, false
	}
	d, err := c.deltasOf()
	if err != nil {
		return nil, 0, false
	}
	p := c.passes()
	rows := p.table()
	for i := range rows {
		nePassFill(&rows[i], c.cumWin(d, neChangeWin{K1: rows[i].K, K2: rows[i].K}))
	}
	return rows, p.Chunks, true
}

// nePassFill — fold a windowed cumulative map into a pass row (dedupe rule of
// the summary: kg_share ≥ 128; a cell is confirmed or candidate, never both).
func nePassFill(r *nePassRow, cum map[uint64]*neCellCum) {
	for _, cu := range cum {
		if cu.Share < 128 || (!cu.Confirmed && !cu.Candidate) {
			continue
		}
		day := cu.AnyAt
		if len(day) >= 10 {
			day = day[:10]
		}
		if cu.Confirmed {
			r.Confirmed++
			r.DN += cu.DN
			if !cu.Unknown {
				for j := range r.DLU {
					r.DLU[j] += cu.DLU[j]
				}
			}
			if r.From == "" || day < r.From {
				r.From = day
			}
			if day > r.To {
				r.To = day
			}
			if r.Days == nil {
				r.Days = map[string]*neChangeTotals{}
			}
			t := r.Days[day]
			if t == nil {
				t = &neChangeTotals{}
				r.Days[day] = t
			}
			t.Cells++
			t.DN += cu.DN
			t.DCover += cu.DCover
			if !cu.Unknown {
				for j := range t.DLU {
					t.DLU[j] += cu.DLU[j]
				}
			}
		} else {
			r.Candidate++
			if r.CandDays == nil {
				r.CandDays = map[string]int{}
			}
			r.CandDays[day]++
		}
	}
}

// ── national ─────────────────────────────────────────────────────────────────

// nePassNational — the country: progress of each pass from the daily tracker
// (distinct observation days per chunk, `chunks_nth`), change per pass from the
// KGs that hold any, per res-7 cell for the overview dots. Built in the
// StatsMemo `ne_passes` (5 min, stale-while-revalidate); ≈ 150 KG logs read
// warm from the change LRU.
func nePassNational() map[string]interface{} {
	scan := neScanReportFiles()
	daily, _ := scan["daily"].([]neDayStat)
	ever, _ := scan["chunks_ever"].(map[string]int)
	g := neGlyphCurrent()
	// chunk universe: digested chunks + every frame cell nobody digested yet at the nominal 343
	universe := ever["once"]
	if g != nil {
		for i := range g.Rows {
			if g.Rows[i].Chunks == 0 {
				universe += neGlyphChunksPer
			}
		}
	}
	// cumulative curve per pass k (2..5) from the tracker's exact nth counts
	const maxK = 5
	var cum [maxK + 1]int
	var first, done [maxK + 1]string
	curve := map[int][][2]interface{}{}
	var rate [maxK + 1]float64 // chunks/day over the last 7 days
	for di, d := range daily {
		for k := 2; k <= maxK; k++ {
			n := d.Nth[k-2]
			if n == 0 {
				continue
			}
			cum[k] += n
			if first[k] == "" {
				first[k] = d.Day
			}
			curve[k] = append(curve[k], [2]interface{}{d.Day, cum[k]})
			if di >= len(daily)-7 {
				rate[k] += float64(n) / 7
			}
			if universe > 0 && float64(cum[k]) >= neDoneShare*float64(universe) && done[k] == "" {
				done[k] = d.Day
			}
		}
	}
	rows := []nePassRow{}
	for k := 2; k <= maxK; k++ {
		if cum[k] == 0 {
			break
		}
		r := nePassRow{K: k, First: first[k], Done: done[k]}
		if universe > 0 {
			r.Share = math.Min(1, float64(cum[k])/float64(universe))
		}
		if r.Done == "" && rate[k] > 0 && universe > cum[k] {
			// random revisits empty the remainder exponentially: the pace per
			// remaining chunk is rate/left, and the remainder falls to the
			// completion threshold after ln(left / (1−done)·U) of those — the
			// tail is what makes the second pass ≈ 2 months, not 2 weeks
			left := float64(universe - cum[k])
			goal := (1 - neDoneShare) * float64(universe)
			if left > goal {
				days := math.Log(left/goal) * left / rate[k]
				if days < 3650 {
					t := time.Now().UTC().AddDate(0, 0, int(days+0.5))
					r.ETA, r.ETADay = t.Format("2006-01"), t.Format("2006-01-02")
				}
			}
		}
		r.Rate = int(rate[k] + 0.5)
		rows = append(rows, r)
	}
	// change per pass + per res-7 cell per pass
	byK := map[int]*nePassRow{}
	for i := range rows {
		byK[rows[i].K] = &rows[i]
	}
	type cellK struct{ conf, cand int }
	type ckey struct {
		ix  int32
		k   int32
		day string
	}
	cells := map[ckey]*cellK{}
	cellSets := map[int]map[uint64]bool{}
	for _, kg := range neChangeKGs() {
		s, ok := neChangeSummaryOf(kg)
		if !ok || (s.Confirmed.Cells == 0 && s.Candidate.Cells == 0) {
			continue
		}
		c := neChangeGet(kg)
		if c == nil {
			continue
		}
		d, err := c.deltasOf()
		if err != nil {
			continue
		}
		ks := map[int]bool{}
		for i := range d.rows {
			if k := d.rows[i].Pass; k >= 2 {
				ks[k] = true
			}
		}
		for k := range ks {
			cum := c.cumWin(d, neChangeWin{K1: k, K2: k})
			r := byK[k]
			if r == nil {
				rows = append(rows, nePassRow{K: k})
				byK = map[int]*nePassRow{}
				for i := range rows {
					byK[rows[i].K] = &rows[i]
				}
				r = byK[k]
			}
			nePassFill(r, cum)
			if cellSets[k] == nil {
				cellSets[k] = map[uint64]bool{}
			}
			for id, cu := range cum {
				if cu.Share < 128 || (!cu.Confirmed && !cu.Candidate) || cellSets[k][id] {
					continue
				}
				cellSets[k][id] = true
				p, ok := neGlyphParent(neCellHex(id), neGlyphRes)
				if !ok {
					continue
				}
				ix, ok := neGlyphFrame.index[p]
				if !ok {
					continue
				}
				day := cu.AnyAt
				if len(day) >= 10 {
					day = day[:10]
				}
				kk := ckey{ix, int32(k), day}
				ck := cells[kk]
				if ck == nil {
					ck = &cellK{}
					cells[kk] = ck
				}
				if cu.Confirmed {
					ck.conf++
				} else {
					ck.cand++
				}
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].K < rows[j].K })
	keys := make([]ckey, 0, len(cells))
	dayIx := map[string]int{}
	for k := range cells {
		keys = append(keys, k)
		dayIx[k.day] = 0
	}
	dayList := make([]string, 0, len(dayIx))
	for d := range dayIx {
		dayList = append(dayList, d)
	}
	sort.Strings(dayList)
	for i, d := range dayList {
		dayIx[d] = i
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].ix != keys[j].ix {
			return keys[i].ix < keys[j].ix
		}
		if keys[i].k != keys[j].k {
			return keys[i].k < keys[j].k
		}
		return keys[i].day < keys[j].day
	})
	cl := make([][5]int, 0, len(keys))
	for _, k := range keys {
		c := cells[k]
		cl = append(cl, [5]int{int(k.ix), int(k.k), dayIx[k.day], c.conf, c.cand})
	}
	cv := map[string][][2]interface{}{}
	for k, c := range curve {
		cv[strconv.Itoa(k)] = c
	}
	return map[string]interface{}{"scope": "national", "as_of": time.Now().UTC().Format(time.RFC3339), "chunks": map[string]int{"universe": universe, "digested": ever["once"], "twice_or_more": ever["twice_or_more"]},
		"passes": rows, "curve": cv, "days": dayList, "cells": cl, "cells_fields": []string{"cell_index", "pass", "day_index", "confirmed", "candidate"}, "epoch": neChangeEpochDay(), "notice": NENotice()}
}

var nePassKGMemo = struct {
	sync.Mutex
	m map[string]struct {
		v  map[string]interface{}
		at time.Time
		lb int64
	}
}{m: map[string]struct {
	v  map[string]interface{}
	at time.Time
	lb int64
}{}}

// handleNEGlyphPasses — GET /api/v1/ne/glyph/passes[?kg=]
func handleNEGlyphPasses(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if kg := q.Get("kg"); kg != "" {
		kg = NormalizeKGCode(kg)
		lb := neChangeLogSize(kg)
		m := &nePassKGMemo
		m.Lock()
		if e, ok := m.m[kg]; ok && e.lb == lb && time.Since(e.at) < 5*time.Minute {
			m.Unlock()
			w.Header().Set("Cache-Control", "public, max-age=120")
			writeJSON(w, 200, e.v)
			return
		}
		m.Unlock()
		rows, chunks, ok := nePassTableKG(kg)
		out := map[string]interface{}{"scope": "kg", "kg": kg, "chunks": chunks, "passes": rows, "epoch": neChangeEpochDay(), "notice": NENotice()}
		if !ok {
			out["passes"] = []nePassRow{}
			out["no_log"] = true
		}
		if rows == nil {
			out["passes"] = []nePassRow{}
		}
		m.Lock()
		if len(m.m) > 512 {
			for k := range m.m {
				delete(m.m, k)
				if len(m.m) <= 256 {
					break
				}
			}
		}
		m.m[kg] = struct {
			v  map[string]interface{}
			at time.Time
			lb int64
		}{out, time.Now(), lb}
		m.Unlock()
		w.Header().Set("Cache-Control", "public, max-age=120")
		writeJSON(w, 200, out)
		return
	}
	if err := neGlyphLoadFrame(); err != nil {
		apiError(w, "glyph frame unavailable", 503)
		return
	}
	out := StatsMemo("ne_passes", 5*time.Minute, q.Get("refresh") == "1" && isPrivateTier(r), nePassNational)
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, 200, out)
}

// nePassMemoSize — for /debug/inflight (docs/memory-contract.md).
func nePassMemoSize() map[string]interface{} {
	nePassKGMemo.Lock()
	n := len(nePassKGMemo.m)
	nePassKGMemo.Unlock()
	out := map[string]interface{}{"kg_tables": n, "kg_tables_max": 512}
	if age, ok := StatsMemoAge("ne_passes"); ok {
		out["national_age_s"] = int(age.Seconds())
	}
	return out
}

var _ = strings.TrimSpace
