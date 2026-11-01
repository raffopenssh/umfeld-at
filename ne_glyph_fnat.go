// ne_glyph_fnat.go — the glyph filter's OVERVIEW preview (docs/ne-filter.md §1a
// "below HI_MIN"): GET /api/v1/ne/glyph/fnat?f=<expr> folds a filter
// expression over the national res-7 table (ne_mvt_nat.go) and answers, per
// res-7 cell of the glyph frame, the share of its res-12 cells that match —
// so a filter is not dead at the Austria view: the overview dots veil by
// share while the cell-exact veil waits for full resolution.
//
// Only what the national table holds can be folded: declared use `d`,
// observed use `o`, change `chg` (incl. `=unk`). Every other term is listed
// in `skipped` and applies at full resolution only. Within an OR group the
// member counts add (exact for members of one set field); across AND groups
// the share is the minimum (an upper bound) — `exact` says which.
// Zero SQL, public, bounded: one row scan of ~16 k cells, answers cached
// in neGlyphTileLRU under the table state + canonical expression.
package main

import (
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strings"
)

// neFnatFields — the fields the national table can fold.
var neFnatFields = map[string]bool{"d": true, "o": true, "chg": true}

// neFnatTermCount — matching res-12 cells of a row for one coarse term.
func neFnatTermCount(r *neNatRow, t neFiltTerm) int {
	n := 0
	switch t.F {
	case "d", "o":
		col := &r.D
		if t.F == "o" {
			col = &r.O
		}
		if t.Unk {
			return int(col[9])
		}
		fd := neFiltByID[t.F]
		seen := map[int]bool{}
		for _, m := range t.Set {
			k := neFiltMember(fd, m)
			if k >= 0 && k < 9 && !seen[k] {
				seen[k] = true
				n += int(col[k])
			}
		}
	case "chg":
		if t.Unk {
			return 0
		}
		seen := map[string]bool{}
		for _, m := range t.Set {
			k := neFiltMember(neFiltByID["chg"], m)
			id := ""
			if k >= 0 {
				id = neFiltByID["chg"].Members[k]
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			switch id {
			case "conf":
				n += int(r.ChgC)
			case "cand":
				n += int(r.ChgK)
			case "none":
				if v := int(r.N) - int(r.ChgC) - int(r.ChgK); v > 0 {
					n += v
				}
			}
		}
	}
	if n > int(r.N) {
		n = int(r.N)
	}
	return n
}

// neFnatResult — the answer; Share is 0..254 per frame cell, 255 = no row.
type neFnatResult struct {
	F       string   `json:"f"`
	Canon   string   `json:"canon"`
	Words   string   `json:"words"`
	Res     int      `json:"res"`
	Exact   bool     `json:"exact"`
	Coarse  []string `json:"coarse"`
	Skipped []string `json:"skipped"`
	Ignored []string `json:"ignored"`
	Cells   int      `json:"cells"`
	Match   int      `json:"match"`
	Rows    int      `json:"rows"`
	State   string   `json:"state"`
	Share   []int    `json:"share"`
	N       []int    `json:"n"` // res-12 cells with a digest per frame cell (0 = no row); Σ share/254·n over any subset = its match
	Units   string   `json:"units"`
}

// neFnatEval — fold e over the res-7 level into the frame order.
func neFnatEval(e *neFiltExpr, lv *neNatLevel) *neFnatResult {
	fr := &neGlyphFrame
	res := &neFnatResult{Res: 7, Canon: e.Canonical(), Words: e.Words(), Ignored: e.Ignored, Coarse: []string{}, Skipped: []string{}, Units: "share = matching res-12 cells of the hexagon in 1/254 (255 = no digest); order = cells[] of /api/v1/ne/glyph; n = digested cells per hexagon, share/254·n = its matching cells; exact only when one AND group remains, else an upper bound"}
	if res.Ignored == nil {
		res.Ignored = []string{}
	}
	var groups [][]neFiltTerm
	coarse := map[string]bool{}
	for _, g := range e.Groups {
		var keep []neFiltTerm
		for _, t := range g {
			if neFnatFields[t.F] {
				keep = append(keep, t)
			} else {
				res.Skipped = append(res.Skipped, t.Words())
			}
		}
		if len(keep) == len(g) && len(keep) > 0 {
			groups = append(groups, keep)
			for _, t := range keep {
				coarse[t.F] = true
			}
		} else if len(keep) > 0 {
			// an OR group with an unfoldable member: dropping the member would under-count — the group is skipped as a whole
			for _, t := range keep {
				res.Skipped = append(res.Skipped, t.Words())
			}
		}
	}
	for f := range coarse {
		res.Coarse = append(res.Coarse, f)
	}
	sort.Strings(res.Coarse)
	if e.K > 0 {
		res.Skipped = append(res.Skipped, "k>="+neFiltNum(float64(e.K)))
	}
	if len(groups) == 0 {
		return res
	}
	res.Exact = len(groups) == 1 && len(res.Skipped) == 0
	share := make([]int, len(fr.cells))
	ncell := make([]int, len(fr.cells))
	for i, c := range fr.cells {
		q := sort.Search(len(lv.IDs), func(k int) bool { return lv.IDs[k] >= c.id })
		if q >= len(lv.IDs) || lv.IDs[q] != c.id || lv.Rows[q].N == 0 {
			share[i] = 255
			continue
		}
		r := &lv.Rows[q]
		res.Rows++
		res.Cells += int(r.N)
		ncell[i] = int(r.N)
		best := int(r.N)
		for _, g := range groups {
			n := 0
			for _, t := range g {
				n += neFnatTermCount(r, t)
			}
			if n < best {
				best = n
			}
		}
		res.Match += best
		share[i] = int(math.Round(254 * float64(best) / float64(r.N)))
	}
	res.Share = share
	res.N = ncell
	return res
}

func handleNEGlyphFnat(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	f := strings.TrimSpace(r.URL.Query().Get("f"))
	if f == "" {
		apiError(w, "f= is required (a filter expression, see /api/v1/ne/filter/fields)", 400)
		return
	}
	if len(f) > 2048 {
		apiError(w, "f too long", 413)
		return
	}
	if err := neGlyphLoadFrame(); err != nil {
		apiError(w, "glyph frame unavailable", 503)
		return
	}
	nat := neNatGet()
	if nat == nil || nat.level(7) == nil {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Retry-After", "120")
		apiError(w, "national table not built yet", 503)
		return
	}
	e := neFiltParse(f)
	key := "fnat/" + nat.State + "/" + e.Canonical()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	if b, ok := neGlyphTileLRU.get(key); ok {
		_, _ = w.Write(b)
		return
	}
	res := neFnatEval(e, nat.level(7))
	res.F = f
	res.State = nat.State
	b, _ := json.Marshal(res)
	neGlyphTileLRU.put(key, b)
	_, _ = w.Write(b)
}
