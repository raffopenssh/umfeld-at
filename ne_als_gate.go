// ne_als_gate.go — which cells were laser-scanned twice. The observed row's
// height change (dh_mean, dtm_change) is a difference between the newest and
// the oldest scan; where a place was not re-flown both editions hold the same
// scan and the value is 0 = "not measured again", not "unchanged". The row is
// never flagged for it (frozen layout), so every reader of those two bytes
// asks here first:
//
//	neDifKind       (diff overlay)  dh corroborates a kind only in a re-flown cell
//	fidx bin, national sample       dh / dtm are nodata elsewhere (filter, exports, tiles)
//	neGlyphPicDif   (cell card)     dh_m only where measured; the cell's own two flight years
//
// Source: the observed-layer peer's national res-10 sample with the two
// flight years per cell (one body per generation, ≈ 1.2 MB zstd, conditional
// GET) and its rule document /api/v1/cells/als. Gate: newest > oldest, both
// known. While the document says the year-range fix has not reached the
// sample we hold, the (oldest, newest) pairs it lists count as not re-flown.
//
// Held: the re-flown res-10 cells only (≈ 1.4 M ids + 2 year bytes ≈ 14 MB),
// one snapshot. A cell whose res-10 parent is not in it — not re-flown, no
// flight year, or no table yet — has no measured height change. The values
// are baked into the fidx bins and the national sample when those are built;
// a change of flight years at the peer arrives with a new block generation,
// which rebuilds the bins anyway, so no cache is keyed on this table except
// the diff tiles (hash of the id set — they are painted locally).
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type neAlsGateT struct {
	ids   []uint64 // re-flown res-10 cells, ascending
	yrs   []uint8  // 2 per id: oldest − 1900, newest − 1900
	rows  int      // sampled cells with both years known
	pairs int      // of them set aside by the until-healed pair list
	gen   string   // the sample's generation
	hash  string
	at    time.Time
}

type neAlsRule struct {
	Gate      string `json:"gate"`
	RangeRule struct {
		ID     string `json:"id"`
		Healed bool   `json:"healed"`
		Bins   struct {
			Pending int `json:"bins_pending"`
		} `json:"bins"`
		Sample struct {
			Gen     string `json:"gen"`
			Current bool   `json:"current"`
		} `json:"sample_r10"`
		Until struct {
			Pairs [][2]int `json:"treat_as_not_reflown_pairs_oldest_newest"`
		} `json:"until_healed"`
	} `json:"range_rule"`
}

const (
	neAlsFields    = "dh_m,als_year_newest,als_year_oldest"
	neAlsPollHeal  = 10 * time.Minute // the peer's bound while healed=false
	neAlsPoll      = time.Hour
	neAlsWaitFirst = 15 * time.Second
)

var (
	neAlsGate     atomic.Pointer[neAlsGateT]
	neAlsStarted  atomic.Bool
	neAlsReady    = make(chan struct{})
	neAlsReadyOne sync.Once
	neAlsMu       sync.Mutex // stats below
	neAlsLast     string
	neAlsRuleTag  string
	neAlsRuleKey  string
	neAlsClient   = &http.Client{Timeout: 30 * time.Second}
)

func neAlsRulePath() string { return filepath.Join(nePMTDir, "als_rule.json") }

// neAlsGateGet — the table, waiting for the first load after a start (the
// loop reads it from disk within a second); nil when there is none.
func neAlsGateGet() *neAlsGateT {
	if g := neAlsGate.Load(); g != nil || !neAlsStarted.Load() {
		return g
	}
	select {
	case <-neAlsReady:
	case <-time.After(neAlsWaitFirst):
	}
	return neAlsGate.Load()
}

// neAlsHash — the id-set hash ("" without a table): part of the diff tile key
// and of the national sample's header.
func neAlsHash() string {
	if g := neAlsGateGet(); g != nil {
		return g.hash
	}
	return ""
}

// reflown — the two flight years of the cell's res-10 parent when it was
// scanned twice. id: any cell of res ≥ 10. A nil table answers false.
func (g *neAlsGateT) reflown(id uint64) (oldest, newest int, ok bool) {
	if g == nil {
		return 0, 0, false
	}
	p := neH3ParentFast(id, 10)
	k := sort.Search(len(g.ids), func(i int) bool { return g.ids[i] >= p })
	if k == len(g.ids) || g.ids[k] != p {
		return 0, 0, false
	}
	return 1900 + int(g.yrs[2*k]), 1900 + int(g.yrs[2*k+1]), true
}

func (g *neAlsGateT) is(id uint64) bool { _, _, ok := g.reflown(id); return ok }

// neAlsGateParse — the peer's NEX1 sample (ids delta_u64, als_year_newest /
// als_year_oldest u16, 65535 = unknown) → the table. skip: (oldest, newest)
// pairs that count as not re-flown (nil once the peer's fix has reached the body).
func neAlsGateParse(body []byte, skip [][2]int) (*neAlsGateT, error) {
	if len(body) < 8 || string(body[:4]) != "NEX1" {
		return nil, fmt.Errorf("not NEX1")
	}
	hl := int(binary.LittleEndian.Uint32(body[4:]))
	if hl < 0 || 8+hl > len(body) {
		return nil, fmt.Errorf("short header")
	}
	var hdr struct {
		N       int    `json:"n"`
		Gen     string `json:"gen"`
		IDs     string `json:"ids"`
		Columns []struct {
			Name  string `json:"name"`
			Dtype string `json:"dtype"`
			Bytes int    `json:"bytes"`
		} `json:"columns"`
	}
	if err := json.Unmarshal(body[8:8+hl], &hdr); err != nil {
		return nil, err
	}
	var idc, nw, od []byte
	o := 8 + hl
	for _, c := range hdr.Columns {
		if c.Bytes < 0 || o+c.Bytes > len(body) {
			return nil, fmt.Errorf("column %s overruns the body", c.Name)
		}
		seg := body[o : o+c.Bytes]
		o += c.Bytes
		switch {
		case c.Name == "id" && c.Bytes == 8*hdr.N:
			idc = seg
		case c.Name == "als_year_newest" && c.Dtype == "u16" && c.Bytes == 2*hdr.N:
			nw = seg
		case c.Name == "als_year_oldest" && c.Dtype == "u16" && c.Bytes == 2*hdr.N:
			od = seg
		}
	}
	if idc == nil || nw == nil || od == nil || hdr.N == 0 {
		return nil, fmt.Errorf("no id / als_year_newest / als_year_oldest column")
	}
	skipM := map[[2]int]bool{}
	for _, p := range skip {
		skipM[p] = true
	}
	g := &neAlsGateT{gen: hdr.Gen, at: time.Now()}
	var acc uint64
	for k := 0; k < hdr.N; k++ {
		v := binary.LittleEndian.Uint64(idc[8*k:])
		if k == 0 || hdr.IDs != "delta_u64" {
			acc = v
		} else {
			acc += v
		}
		n, ol := int(binary.LittleEndian.Uint16(nw[2*k:])), int(binary.LittleEndian.Uint16(od[2*k:]))
		if n == 65535 || ol == 65535 {
			continue
		}
		g.rows++
		if n <= ol || ol < 1900 || n > 2155 {
			continue
		}
		if skipM[[2]int{ol, n}] {
			g.pairs++
			continue
		}
		g.ids = append(g.ids, neH3ParentFast(acc, 10))
		g.yrs = append(g.yrs, uint8(ol-1900), uint8(n-1900))
	}
	if !sort.SliceIsSorted(g.ids, func(a, b int) bool { return g.ids[a] < g.ids[b] }) {
		return nil, fmt.Errorf("ids not ascending")
	}
	h := sha256.New()
	var b8 [8]byte
	for _, id := range g.ids {
		binary.LittleEndian.PutUint64(b8[:], id)
		h.Write(b8[:])
	}
	g.hash = hex.EncodeToString(h.Sum(nil))[:12]
	return g, nil
}

func neAlsRuleRead() *neAlsRule {
	b, err := os.ReadFile(neAlsRulePath())
	if err != nil {
		return nil
	}
	var r neAlsRule
	if json.Unmarshal(b, &r) != nil || r.RangeRule.ID == "" {
		return nil
	}
	return &r
}

// key — the part of the rule document a table depends on.
func (r *neAlsRule) key() string {
	return fmt.Sprint(r.RangeRule.ID, r.RangeRule.Healed, r.RangeRule.Sample, r.RangeRule.Until.Pairs)
}

// neAlsGateBuild — rule document + sample on disk → the live table. Without
// the document there is no table: we cannot tell a fixed sample from an old one.
func neAlsGateBuild() error {
	r := neAlsRuleRead()
	if r == nil {
		return fmt.Errorf("no rule document yet")
	}
	body := neSampPeerRead("als")
	if body == nil {
		return fmt.Errorf("no sample yet")
	}
	gen := neSampPeerMeta("als")["X-Cells-Gen"]
	var skip [][2]int
	if !r.RangeRule.Healed && !(r.RangeRule.Sample.Current && r.RangeRule.Sample.Gen == gen) {
		skip = r.RangeRule.Until.Pairs
	}
	g, err := neAlsGateParse(body, skip)
	if err != nil {
		return err
	}
	if g.gen == "" {
		g.gen = gen
	}
	old := ""
	if o := neAlsGate.Load(); o != nil {
		old = o.hash
	}
	neAlsGate.Store(g)
	if g.hash != old {
		log.Printf("als gate: %d of %d sampled res-10 cells re-flown (%d set aside by the pair list), gen %s, hash %s (was %q)", len(g.ids), g.rows, g.pairs, g.gen, g.hash, old)
	}
	neAlsMu.Lock()
	neAlsRuleKey = r.key() + "|" + gen
	neAlsMu.Unlock()
	return nil
}

// neAlsRuleFetch — conditional GET of the rule document; true when a new body was stored.
func neAlsRuleFetch() bool {
	api := peers().ObsAPI
	if api == "" {
		return false
	}
	req, err := http.NewRequest("GET", api+"/api/v1/cells/als", nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "umfeld-at/als (+"+publicURL()+")")
	neAlsMu.Lock()
	tag := neAlsRuleTag
	neAlsMu.Unlock()
	if tag != "" {
		req.Header.Set("If-None-Match", tag)
	}
	note := func(s string) {
		neAlsMu.Lock()
		neAlsLast = time.Now().UTC().Format(time.RFC3339) + " " + s
		neAlsMu.Unlock()
	}
	resp, err := neAlsClient.Do(req)
	if err != nil {
		note("rule: " + err.Error())
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		note(fmt.Sprintf("rule: %d", resp.StatusCode))
		return false
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var r neAlsRule
	if err != nil || json.Unmarshal(b, &r) != nil || r.RangeRule.ID == "" {
		note("rule: malformed")
		return false
	}
	os.MkdirAll(nePMTDir, 0o755)
	if os.WriteFile(neAlsRulePath()+".tmp", b, 0o644) != nil || os.Rename(neAlsRulePath()+".tmp", neAlsRulePath()) != nil {
		return false
	}
	neAlsMu.Lock()
	neAlsRuleTag = resp.Header.Get("ETag")
	neAlsMu.Unlock()
	note(fmt.Sprintf("rule: 200 healed=%v pending=%d sample_current=%v", r.RangeRule.Healed, r.RangeRule.Bins.Pending, r.RangeRule.Sample.Current))
	return true
}

// neAlsRefresh — one round: rule document, sample (a 304 while its generation
// stands), a new table when either says something else than the one we hold.
func neAlsRefresh() {
	neAlsRuleFetch()
	if api := peers().ObsAPI; api != "" {
		neSampPeerFetch("als", api+"/api/v1/cells/sample?res=10&format=bin&fields="+neAlsFields)
	}
	r := neAlsRuleRead()
	if r == nil {
		return
	}
	neAlsMu.Lock()
	have := neAlsRuleKey
	neAlsMu.Unlock()
	if want := r.key() + "|" + neSampPeerMeta("als")["X-Cells-Gen"]; want != have || neAlsGate.Load() == nil {
		if err := neAlsGateBuild(); err != nil {
			log.Printf("als gate: %v", err)
		}
	}
}

// neAlsGateLoop — disk first (readers wait for this), then the peer: every
// 10 min while its fix is still spreading, hourly afterwards.
func neAlsGateLoop() {
	neAlsStarted.Store(true)
	if err := neAlsGateBuild(); err != nil {
		log.Printf("als gate: nothing on disk (%v) — asking the peer", err)
		neAlsRefresh()
	}
	neAlsReadyOne.Do(func() { close(neAlsReady) })
	for {
		d := neAlsPoll
		if r := neAlsRuleRead(); r == nil || !r.RangeRule.Healed {
			d = neAlsPollHeal
		}
		time.Sleep(d)
		neAlsRefresh()
	}
}

// NEAlsGateStats — size line for /api/v1/debug/inflight (invariant 10).
func NEAlsGateStats() map[string]interface{} {
	m := map[string]interface{}{"bound": "one snapshot: re-flown res-10 cells × 10 B"}
	if g := neAlsGate.Load(); g != nil {
		m["reflown_r10"], m["sampled_r10"], m["pair_list_set_aside"] = len(g.ids), g.rows, g.pairs
		m["bytes"], m["gen"], m["age_s"], m["hash"] = 10*len(g.ids), g.gen, int(time.Since(g.at).Seconds()), g.hash
	}
	if r := neAlsRuleRead(); r != nil {
		m["rule"] = map[string]interface{}{"id": r.RangeRule.ID, "healed": r.RangeRule.Healed, "bins_pending": r.RangeRule.Bins.Pending, "sample_current": r.RangeRule.Sample.Current, "sample_gen": r.RangeRule.Sample.Gen}
	}
	neAlsMu.Lock()
	m["last"] = neAlsLast
	neAlsMu.Unlock()
	return m
}
