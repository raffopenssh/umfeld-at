// ne_tree_stands.go — the trees away from full resolution: the observed-layer
// peer's national stand index (one row per H3 hexagon holding ≥ 1 LiDAR tree
// apex ≥ 3 m; its /trees/summary and /trees/hexes) relayed in the shapes the
// landing glyph paints (docs/ne-glyph.md §Trees far out).
//
//	GET /api/v1/ne/glyph/trees                    national: {total{…}, trees[], tallest_m[], mean_m[]}
//	                                              one entry per cells[] entry of /ne/glyph (res 7)
//	GET /api/v1/ne/glyph/trees?res=8|9&bbox=…     binary: the hexagons of the box with their centres
//
// The national table is one bounded snapshot (≤ one row per frame cell,
// ~120 KB), refreshed in the background when older than an hour with the
// peer's ETag (a 304 costs nothing); no request waits for the peer longer
// than neStandsWait. Box answers sit in the tile LRU, keyed by the peer's
// `gen`. Size line: debug/inflight → ne_tree_stands.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/uber/h3-go/v4"
)

const (
	neStandsTTL   = time.Hour        // the peer rebuilds at most daily and caches for an hour
	neStandsRetry = 60 * time.Second // after a failed fetch
	neStandsWait  = 4 * time.Second  // a request's patience with a cold table
	// neStandsMaxDeg2 — the widest box per resolution (deg²; 1 deg² ≈ 8 300 km² here): keeps a box
	// under the peer's 20 000 hexagons. The page picks the resolution by the same numbers (TRO_A).
	neStandsMaxDeg2r8 = 1.4
	neStandsMaxDeg2r9 = 0.2
)

// neStandsTotal — the national figures (the landing page's sentence, the legend far out).
type neStandsTotal struct {
	Trees    int64     `json:"trees"`
	Hexes    int64     `json:"hexes"`
	TallestM float64   `json:"tallest_m"`
	MeanM    float64   `json:"mean_height_m"`
	Ge30     int64     `json:"ge_30m"`
	Ge40     int64     `json:"ge_40m"`
	EdgesM   []float64 `json:"edges_m,omitempty"` // lower edges of the height bands
	Bands    []int64   `json:"bands,omitempty"`   // tree tops per band (the last one is open)
}

type neStandsSnap struct {
	Gen     string
	BuiltAt string
	Total   neStandsTotal
	body    []byte // the national JSON answer
}

var neStands struct {
	mu          sync.Mutex
	snap        atomic.Pointer[neStandsSnap]
	fresh, last time.Time
	busy        bool
	done        chan struct{} // closed when the running fetch ends
	err         string
	etag        string
	n304, nBox  int
}

var neStandsBase = func() string { return peers().ObsAPI + "/api/v1/trees" } // var: tests point it at a stub

// neStandsGet — the snapshot at hand (nil until the first fetch landed); a stale one starts a refresh.
func neStandsGet() *neStandsSnap {
	s := &neStands
	s.mu.Lock()
	if time.Since(s.fresh) > neStandsTTL && time.Since(s.last) > neStandsRetry && !s.busy && peers().ObsAPI != "" {
		s.busy, s.last, s.done = true, time.Now(), make(chan struct{})
		go neStandsFetch(s.done)
	}
	s.mu.Unlock()
	return s.snap.Load()
}

// neStandsAwait — neStandsGet, waiting (bounded) for the first fetch.
func neStandsAwait(ctx context.Context) *neStandsSnap {
	if sn := neStandsGet(); sn != nil {
		return sn
	}
	neStands.mu.Lock()
	done := neStands.done
	neStands.mu.Unlock()
	if done == nil {
		return nil
	}
	t := time.NewTimer(neStandsWait)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	case <-ctx.Done():
	}
	return neStands.snap.Load()
}

func neStandsPeerGet(ctx context.Context, path, etag string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", neStandsBase()+path, nil)
	if err != nil {
		return nil, err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	return http.DefaultClient.Do(req)
}

type neStandsHexes struct {
	Columns struct {
		H3      []string  `json:"h3"`
		Trees   []int64   `json:"trees"`
		Tallest []float64 `json:"tallest_m"`
		Mean    []float64 `json:"mean_height_m"`
	} `json:"columns"`
	Gen     string `json:"gen"`
	BuiltAt string `json:"built_at"`
}

func neStandsFetch(done chan struct{}) {
	s := &neStands
	fail := func(msg string) {
		s.mu.Lock()
		s.err, s.busy = msg, false
		s.mu.Unlock()
		close(done)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := neGlyphLoadFrame(); err != nil {
		fail("frame: " + err.Error())
		return
	}
	s.mu.Lock()
	etag := s.etag
	s.mu.Unlock()
	resp, err := neStandsPeerGet(ctx, "/hexes?res=7", etag)
	if err != nil {
		fail(err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		s.mu.Lock()
		s.fresh, s.busy, s.err = time.Now(), false, ""
		s.n304++
		s.mu.Unlock()
		close(done)
		return
	}
	if resp.StatusCode != 200 {
		fail("hexes: HTTP " + strconv.Itoa(resp.StatusCode))
		return
	}
	var hx neStandsHexes
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&hx); err != nil {
		fail("hexes: " + err.Error())
		return
	}
	c := hx.Columns
	if len(c.H3) == 0 || len(c.Trees) != len(c.H3) || len(c.Tallest) != len(c.H3) || len(c.Mean) != len(c.H3) {
		fail("hexes: bad columns")
		return
	}
	var sum struct {
		Areas []struct {
			Hexes   int64   `json:"hexes"`
			Trees   int64   `json:"trees"`
			AtLeast []int64 `json:"at_least"`
			Bands   []int64 `json:"bands"`
			Mean    float64 `json:"mean_height_m"`
			Tallest struct {
				M float64 `json:"tallest_m"`
			} `json:"tallest"`
		} `json:"areas"`
		Edges []float64 `json:"edges_m"`
		Gen   string    `json:"gen"`
	}
	r2, err := neStandsPeerGet(ctx, "/summary", "")
	if err != nil {
		fail(err.Error())
		return
	}
	defer r2.Body.Close()
	if r2.StatusCode != 200 || json.NewDecoder(io.LimitReader(r2.Body, 1<<20)).Decode(&sum) != nil || len(sum.Areas) != 1 || sum.Areas[0].Trees <= 0 {
		fail("summary: HTTP " + strconv.Itoa(r2.StatusCode))
		return
	}
	a := sum.Areas[0]
	if sum.Gen != hx.Gen { // rebuilt between the two calls: next round reads one generation
		fail("generation moved between calls")
		return
	}
	f := &neGlyphFrame
	n := len(f.cells)
	doc := struct {
		Gen     string        `json:"gen"`
		BuiltAt string        `json:"built_at"`
		Source  string        `json:"source"`
		License string        `json:"license"`
		Res     int           `json:"res"`
		CellHa  float64       `json:"cell_ha"`
		Total   neStandsTotal `json:"total"`
		Trees   []int64       `json:"trees"`
		Tallest []int         `json:"tallest_m"`
		Mean    []float64     `json:"mean_m"`
	}{Gen: hx.Gen, BuiltAt: hx.BuiltAt, Source: peers().ObsName, License: "CC-BY-4.0", Res: neGlyphRes,
		Trees: make([]int64, n), Tallest: make([]int, n), Mean: make([]float64, n)}
	doc.Total = neStandsTotal{Trees: a.Trees, Hexes: a.Hexes, TallestM: a.Tallest.M, MeanM: a.Mean}
	if len(a.Bands) > 1 && len(a.Bands) == len(sum.Edges) {
		doc.Total.EdgesM, doc.Total.Bands = sum.Edges, a.Bands
	}
	if len(a.AtLeast) >= 9 {
		doc.Total.Ge30, doc.Total.Ge40 = a.AtLeast[6], a.AtLeast[8]
	}
	var area float64
	for i, hs := range c.H3 {
		id, err := strconv.ParseUint(hs, 16, 64)
		if err != nil {
			continue
		}
		j, ok := f.index[id]
		if !ok {
			continue // a border hexagon outside the frame
		}
		doc.Trees[j], doc.Tallest[j], doc.Mean[j] = c.Trees[i], int(math.Round(c.Tallest[i])), c.Mean[i]
	}
	for _, p := range f.cells {
		area += neStandsHexHa(p.id)
	}
	doc.CellHa = math.Round(area/float64(n)*10) / 10
	body, _ := json.Marshal(doc)
	s.snap.Store(&neStandsSnap{Gen: hx.Gen, BuiltAt: hx.BuiltAt, Total: doc.Total, body: body})
	s.mu.Lock()
	s.fresh, s.busy, s.err, s.etag = time.Now(), false, "", resp.Header.Get("ETag")
	s.mu.Unlock()
	close(done)
}

func neStandsHexHa(id uint64) float64 {
	m2, err := h3.CellAreaM2(h3.Cell(id))
	if err != nil {
		return 0
	}
	return m2 / 1e4
}

// neStandsBox — the hexagons of a box at res 8 or 9 as one blob (little endian):
//
//	"NTH1" · n u32 · res u32 · cell_ha f32 · trees u32[n] · lon u16[n] · lat u16[n] · tallest_m u8[n] · mean u8[n] (0.25 m)
//
// lon = 9 + u/65535·9, lat = 46 + u/65535·4 (the frame of /ne/glyph/fnat?res=9).
func neStandsBox(ctx context.Context, res int, w, s, e, n float64) ([]byte, int, error) {
	resp, err := neStandsPeerGet(ctx, fmt.Sprintf("/hexes?res=%d&bbox=%.3f,%.3f,%.3f,%.3f", res, w, s, e, n), "")
	if err != nil {
		return nil, 502, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		code := 502
		if resp.StatusCode == 503 {
			code = 503
		}
		return nil, code, fmt.Errorf("peer HTTP %d", resp.StatusCode)
	}
	var hx neStandsHexes
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&hx); err != nil {
		return nil, 502, err
	}
	c := hx.Columns
	if len(c.Trees) != len(c.H3) || len(c.Tallest) != len(c.H3) || len(c.Mean) != len(c.H3) {
		return nil, 502, fmt.Errorf("bad columns")
	}
	type row struct {
		lon, lat   uint16
		trees      uint32
		tall, mean uint8
	}
	rows := make([]row, 0, len(c.H3))
	var ha float64
	q := func(v, lo, span float64) uint16 {
		return uint16(math.Round(math.Max(0, math.Min(1, (v-lo)/span)) * 65535))
	}
	for i, hs := range c.H3 {
		id, err := strconv.ParseUint(hs, 16, 64)
		if err != nil || !h3.Cell(id).IsValid() {
			continue
		}
		ll, _ := h3.Cell(id).LatLng()
		if ha == 0 {
			ha = neStandsHexHa(id)
		}
		rows = append(rows, row{q(ll.Lng, 9, 9), q(ll.Lat, 46, 4), uint32(c.Trees[i]),
			uint8(math.Min(255, math.Round(c.Tallest[i]))), uint8(math.Min(255, math.Round(c.Mean[i]*4)))})
	}
	k := len(rows)
	out := make([]byte, 16+10*k)
	copy(out, "NTH1")
	le := binary.LittleEndian
	le.PutUint32(out[4:], uint32(k))
	le.PutUint32(out[8:], uint32(res))
	le.PutUint32(out[12:], math.Float32bits(float32(ha)))
	for i, r := range rows {
		le.PutUint32(out[16+4*i:], r.trees)
		le.PutUint16(out[16+4*k+2*i:], r.lon)
		le.PutUint16(out[16+6*k+2*i:], r.lat)
		out[16+8*k+i] = r.tall
		out[16+9*k+i] = r.mean
	}
	return out, 200, nil
}

func handleNEGlyphTrees(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	q := r.URL.Query()
	sn := neStandsAwait(r.Context())
	if u := q.Get("u"); u != "" && sn != nil {
		neStandsUnit(w, r, sn, u)
		return
	}
	if sn == nil {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Retry-After", "30")
		apiError(w, "tree index not at hand yet", 503)
		return
	}
	w.Header().Set("X-Data-Attribution", peers().ObsName+" (CC BY 4.0)")
	if q.Get("res") == "" {
		etag := `"` + sn.Gen + `"`
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "public, max-age=3600")
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(sn.body)
		return
	}
	res, _ := strconv.Atoi(q.Get("res"))
	var b [4]float64
	p := strings.Split(q.Get("bbox"), ",")
	ok := len(p) == 4 && (res == 8 || res == 9)
	for i := 0; ok && i < 4; i++ {
		v, err := strconv.ParseFloat(strings.TrimSpace(p[i]), 64)
		ok = err == nil && !math.IsNaN(v) && !math.IsInf(v, 0)
		b[i] = math.Round(v*1000) / 1000
	}
	if !ok || b[0] >= b[2] || b[1] >= b[3] {
		apiError(w, "res=8|9 and bbox=w,s,e,n are required together (no parameter = the national res-7 table)", 400)
		return
	}
	max := neStandsMaxDeg2r8
	if res == 9 {
		max = neStandsMaxDeg2r9
	}
	if (b[2]-b[0])*(b[3]-b[1]) > max+1e-9 {
		apiError(w, fmt.Sprintf("bbox too large for res=%d (≤ %.1f deg²); use a lower res", res, max), 413)
		return
	}
	key := fmt.Sprintf("trh/%s/%d/%.3f,%.3f,%.3f,%.3f", sn.Gen, res, b[0], b[1], b[2], b[3])
	out, hit := neGlyphTileLRU.get(key)
	if !hit {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		var code int
		var err error
		if out, code, err = neStandsBox(ctx, res, b[0], b[1], b[2], b[3]); err != nil {
			w.Header().Set("Cache-Control", "no-store")
			apiError(w, "tree index unavailable", code)
			return
		}
		neGlyphTileLRU.put(key, out)
		neStands.mu.Lock()
		neStands.nBox++
		neStands.mu.Unlock()
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(out)
}

// NEStandsStats — debug/inflight line.
func NEStandsStats() map[string]interface{} {
	s := &neStands
	s.mu.Lock()
	defer s.mu.Unlock()
	o := map[string]interface{}{"busy": s.busy, "n304": s.n304, "box_fetches": s.nBox, "bound": "1 national snapshot; boxes in ne_glyph_tiles"}
	if s.err != "" {
		o["err"] = s.err
	}
	if !s.fresh.IsZero() {
		o["age_s"] = int(time.Since(s.fresh).Seconds())
	}
	if sn := s.snap.Load(); sn != nil {
		o["gen"], o["bytes"], o["trees"] = sn.Gen, len(sn.body), sn.Total.Trees
	}
	return o
}

// ── a selection's trees: GET /api/v1/ne/glyph/trees?u=<unit token of the share link> ──
//
// The unit is the one /ne/glyph/area and the export answer for (neStatsUnits):
// KG-based units (KG, Gemeinde, Bezirk, a combination of them) are the sum of
// their KGs at the peer (one batch call, KGs are disjoint), a Land is the
// peer's state=, a Natura 2000 site is its outline. Answer: {trees, hexes,
// tallest_m, mean_height_m, edges_m[], bands[]} — the height distribution the
// card draws. 404 = a unit without one (h3, box, a combination holding a site).
type neStandsArea struct {
	Hexes   int64   `json:"hexes"`
	Trees   int64   `json:"trees"`
	Bands   []int64 `json:"bands"`
	Mean    float64 `json:"mean_height_m"`
	Tallest struct {
		M float64 `json:"tallest_m"`
	} `json:"tallest"`
}

func neStandsSummary(ctx context.Context, query string, body interface{}) ([]neStandsArea, []float64, int, error) {
	method, rd := "GET", io.Reader(nil)
	if body != nil {
		b, _ := json.Marshal(body)
		method, rd = "POST", bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, neStandsBase()+"/summary"+query, rd)
	if err != nil {
		return nil, nil, 502, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, 502, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		code := 502
		if resp.StatusCode == 503 {
			code = 503
		}
		return nil, nil, code, fmt.Errorf("peer HTTP %d", resp.StatusCode)
	}
	var doc struct {
		Edges []float64      `json:"edges_m"`
		Areas []neStandsArea `json:"areas"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&doc); err != nil {
		return nil, nil, 502, err
	}
	return doc.Areas, doc.Edges, 200, nil
}

// neStandsPolygons — even-odd rings → GeoJSON MultiPolygon coordinates: a ring
// inside an odd number of others is a hole of the smallest ring around it.
func neStandsPolygons(rings [][][2]float64) [][][][2]float64 {
	type rg struct {
		pts   [][2]float64
		depth int
		in    int // index of the innermost ring around it, -1
		area  float64
	}
	rs := make([]rg, 0, len(rings))
	for _, r := range rings {
		if len(r) < 3 {
			continue
		}
		p := r
		if p[0] != p[len(p)-1] {
			p = append(append([][2]float64{}, p...), p[0])
		}
		var a float64
		for i := 0; i+1 < len(p); i++ {
			a += p[i][0]*p[i+1][1] - p[i+1][0]*p[i][1]
		}
		rs = append(rs, rg{pts: p, in: -1, area: math.Abs(a)})
	}
	for i := range rs {
		for j := range rs {
			if i == j || rs[j].area <= rs[i].area || !neStatsInRings([][][2]float64{rs[j].pts}, rs[i].pts[0][0], rs[i].pts[0][1]) {
				continue
			}
			rs[i].depth++
			if rs[i].in < 0 || rs[j].area < rs[rs[i].in].area {
				rs[i].in = j
			}
		}
	}
	at := map[int]int{}
	var out [][][][2]float64
	for i, r := range rs {
		if r.depth%2 == 0 {
			at[i] = len(out)
			out = append(out, [][][2]float64{r.pts})
		}
	}
	for _, r := range rs {
		if r.depth%2 == 1 {
			if k, ok := at[r.in]; ok {
				out[k] = append(out[k], r.pts)
			}
		}
	}
	return out
}

func neStandsUnit(w http.ResponseWriter, r *http.Request, sn *neStandsSnap, u string) {
	if len(u) > 4096 {
		apiError(w, "u= too long", 400)
		return
	}
	key := "tru/" + sn.Gen + "/" + u
	out, hit := neGlyphTileLRU.get(key)
	if !hit {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		var areas []neStandsArea
		var edges []float64
		var code int
		var err error
		if len(u) == 5 && strings.HasPrefix(u, "adm=") && u[4] >= '1' && u[4] <= '9' {
			areas, edges, code, err = neStandsSummary(ctx, "?state="+u[4:], nil)
		} else {
			units, msg, c := neStatsUnits(url.Values{"share": {u}})
			if msg != "" || len(units) == 0 {
				if c == 0 {
					c = 400
				}
				apiError(w, msg, c)
				return
			}
			un := units[0]
			switch {
			case len(un.kgs) == 1:
				areas, edges, code, err = neStandsSummary(ctx, "?kg="+un.kgs[0], nil)
			case len(un.kgs) > 1 && len(un.kgs) <= 500:
				as := make([]map[string]string, len(un.kgs))
				for i, kg := range un.kgs {
					as[i] = map[string]string{"id": kg, "kg": kg}
				}
				areas, edges, code, err = neStandsSummary(ctx, "", map[string]interface{}{"areas": as})
			case un.rings != nil:
				n := 0
				for _, rg := range un.rings {
					n += len(rg)
				}
				if n > 49000 {
					apiError(w, "outline too detailed for the tree index", 404)
					return
				}
				areas, edges, code, err = neStandsSummary(ctx, "", map[string]interface{}{"geometry": map[string]interface{}{"type": "MultiPolygon", "coordinates": neStandsPolygons(un.rings)}})
			default:
				apiError(w, "no tree figures for this unit", 404)
				return
			}
		}
		if err != nil || len(areas) == 0 {
			w.Header().Set("Cache-Control", "no-store")
			if code == 0 || code == 200 {
				code = 502
			}
			apiError(w, "tree index unavailable", code)
			return
		}
		var o struct {
			Gen     string    `json:"gen"`
			Trees   int64     `json:"trees"`
			Hexes   int64     `json:"hexes"`
			Tallest float64   `json:"tallest_m"`
			Mean    float64   `json:"mean_height_m"`
			Edges   []float64 `json:"edges_m"`
			Bands   []int64   `json:"bands"`
		}
		o.Gen, o.Edges, o.Bands = sn.Gen, edges, make([]int64, len(edges))
		var ms float64
		for _, a := range areas {
			o.Trees += a.Trees
			o.Hexes += a.Hexes
			ms += a.Mean * float64(a.Trees)
			o.Tallest = math.Max(o.Tallest, a.Tallest.M)
			for i := 0; i < len(a.Bands) && i < len(o.Bands); i++ {
				o.Bands[i] += a.Bands[i]
			}
		}
		if o.Trees > 0 {
			o.Mean = math.Round(ms/float64(o.Trees)*10) / 10
		}
		out, _ = json.Marshal(o)
		neGlyphTileLRU.put(key, out)
		neStands.mu.Lock()
		neStands.nBox++
		neStands.mu.Unlock()
	}
	w.Header().Set("X-Data-Attribution", peers().ObsName+" (CC BY 4.0)")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(out)
}
