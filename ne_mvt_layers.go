// ne_mvt_layers.go — the companions of the cell layer in a QGIS project
// (ne_mvt_qgs.go), all served by THIS service so that one key counts every
// layer of the map and an anonymous reader still talks to one host:
//
//	GET /api/v1/ne/mvt/relief/{z}/{x}/{y}.png        the observed-layer peer's hillshade, relayed (z 8–17)
//	GET /api/v1/ne/mvt/trees/{z}/{x}/{y}.pbf         tree apices as points, layer "trees" (z 14 only; ht=lo..hi)
//	GET /api/v1/ne/mvt/pic/{declared|observed|sub|subc|roads}/{z}/{x}/{y}.png
//	                                                  the landing page's own overlay pictures as XYZ tiles (z 12–16, 512 px)
//
// relief — the peer's tile, with the page's water key baked in (flat ground,
// tone 180, becomes transparent: the same rule as relBake in the page) so an
// overlay blend shades slopes and leaves plains alone; `raw=1` relays the
// peer's bytes untouched.
//
// trees — the peer's /trees/bbox columns for the tile box (+ a crown's margin,
// so a disc on a tile edge is whole in both tiles) encoded as MVT points with
// h (m), crown_d (m), crown_m2, species, vitality. One zoom: QGIS overzooms it.
//
// pic — the SAME renderers the page's tiles use (neGlyphSubRender: the INVEKOS
// Schläge as drawn; neGlyphRoadsRender: the roads ink), run on a pixel frame
// laid over a Web-Mercator tile. The glyph frame is plate carrée: across one
// tile of z ≥ 12 the two differ by a fraction of a pixel in x; in y the rows
// are resampled to the tile's 512. Outlines never leave as vectors.
//
// Every tile passes neMVTGate (identity, bbox allow-list, daily tile budget —
// ne_mvt_gate.go) and is accounted to the caller's identity (exportAccountFor),
// exactly like a cell tile. Caches: neLayerLRU (bounded, size line `ne_mvt.layers`).
package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	neLayerReliefMinZ = 8
	neLayerReliefMaxZ = 17
	neLayerTreesZ     = 14
	neLayerPicMinZ    = 12
	neLayerPicMaxZ    = 16
	neLayerPicPx      = 512
)

var (
	neLayerLRU     = newNELRU(64 << 20) // relayed relief tiles, tree tiles (gzip'd), rendered pictures
	neLayerClient  = &http.Client{Timeout: 25 * time.Second}
	neLayerSem     = make(chan struct{}, 2) // picture renders
	neLayerPeerSem = make(chan struct{}, 8) // relays in flight
	neLayerServed  [3]atomic.Int64          // relief, trees, pic
	neLayerPeerErr atomic.Int64
)

// NELayerStats — the inflight line `ne_mvt.layers`.
func NELayerStats() map[string]interface{} {
	m := neLayerLRU.stats()
	m["relief"], m["trees"], m["pic"] = neLayerServed[0].Load(), neLayerServed[1].Load(), neLayerServed[2].Load()
	m["peer_errors"] = neLayerPeerErr.Load()
	return m
}

// handleNEMVTLayer — rest = "mvt/relief/…", "mvt/trees/…", "mvt/pic/…"; false = not ours.
func handleNEMVTLayer(w http.ResponseWriter, r *http.Request, rest string) bool {
	if rest == "mvt/trees.geojson" {
		handleNELayerTreesGeoJSON(w, r)
		return true
	}
	p := strings.Split(strings.TrimPrefix(rest, "mvt/"), "/")
	kind, view := p[0], ""
	switch {
	case kind == "relief" && len(p) == 4, kind == "trees" && len(p) == 4:
		p = p[1:]
	case kind == "pic" && len(p) == 5:
		view, p = p[1], p[2:]
		if view != "sub" && view != "subc" && view != "roads" && view != "declared" && view != "observed" {
			apiError(w, "mvt/pic/{declared|observed|sub|subc|roads}/{z}/{x}/{y}.png", 404)
			return true
		}
	default:
		return false
	}
	if i := strings.IndexByte(p[2], '.'); i > 0 {
		p[2] = p[2][:i]
	}
	z, e1 := strconv.Atoi(p[0])
	x, e2 := strconv.Atoi(p[1])
	y, e3 := strconv.Atoi(p[2])
	if e1 != nil || e2 != nil || e3 != nil || z < 0 || z > 22 || x < 0 || y < 0 || x >= 1<<uint(z) || y >= 1<<uint(z) {
		apiError(w, "mvt/"+kind+"/…/{z}/{x}/{y}", 400)
		return true
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	bw, bs, be, bn := mvtTileBox(z, x, y)
	lo, hi := neLayerReliefMinZ, neLayerReliefMaxZ
	switch kind {
	case "trees":
		lo, hi = neLayerTreesZ, neLayerTreesZ
	case "pic":
		lo, hi = neLayerPicMinZ, neLayerPicMaxZ
	}
	if z < lo || z > hi || be < neMVTAustria[0] || bw > neMVTAustria[2] || bn < neMVTAustria[1] || bs > neMVTAustria[3] {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	if _, ok := neMVTGate(w, r, 1, bw, bs, be, bn); !ok {
		return true
	}
	var body []byte
	var ctype, cache string
	var gz bool
	var err error
	switch kind {
	case "relief":
		body, err = neLayerRelief(z, x, y, r.URL.Query().Get("raw") == "1")
		ctype, cache = "image/png", "public, max-age=604800"
		neLayerServed[0].Add(1)
	case "trees":
		var whole bool
		body, whole, err = neLayerTrees(z, x, y, r.URL.Query().Get("ht"))
		ctype, cache, gz = "application/vnd.mapbox-vector-tile", "public, max-age=3600", true
		if !whole {
			cache = "no-store"
		}
		neLayerServed[1].Add(1)
	case "pic":
		body, err = neLayerPic(view, z, x, y)
		ctype, cache = "image/png", "public, max-age=3600"
		neLayerServed[2].Add(1)
	}
	if err != nil {
		if err != errBusy {
			neLayerPeerErr.Add(1)
		}
		w.Header().Set("Retry-After", "5")
		w.Header().Set("Cache-Control", "no-store")
		apiError(w, "layer tile unavailable — retry in a few seconds", http.StatusServiceUnavailable)
		return true
	}
	w.Header().Set("Cache-Control", cache)
	if len(body) == 0 {
		w.WriteHeader(http.StatusNoContent)
		exportAccountFor(r, 0)
		return true
	}
	w.Header().Set("Content-Type", ctype)
	if gz {
		w.Header().Add("Vary", "Accept-Encoding")
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
		} else if body, err = neGunzip(body); err != nil {
			apiError(w, "layer tile unavailable", 503)
			return true
		}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Write(body)
	exportAccountFor(r, len(body))
	return true
}

// neLayerPeerGet — one bounded GET at the observed-layer peer.
func neLayerPeerGet(url string, max int64) ([]byte, int, error) {
	select {
	case neLayerPeerSem <- struct{}{}:
		defer func() { <-neLayerPeerSem }()
	case <-time.After(10 * time.Second):
		return nil, 0, errBusy
	}
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "umfeld-at/qgis-layer (+"+publicURL()+")")
	resp, err := neLayerClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, max))
	return b, resp.StatusCode, err
}

// neLayerRelief — the peer's hillshade tile; baked: flat ground transparent.
func neLayerRelief(z, x, y int, raw bool) ([]byte, error) {
	pr := peers()
	if pr.ObsWeb == "" {
		return nil, nil
	}
	key := fmt.Sprintf("rel/%d/%d/%d", z, x, y)
	if raw {
		key = "relraw" + key[3:]
	}
	if b, ok := neLayerLRU.get(key); ok {
		return b, nil
	}
	b, code, err := neLayerPeerGet(fmt.Sprintf("%s/tiles/hillshade/%d/%d/%d.png", pr.ObsWeb, z, x, y), 4<<20)
	if err != nil {
		return nil, err
	}
	if code == 404 || code == 204 {
		neLayerLRU.put(key, []byte{})
		return nil, nil
	}
	if code != 200 {
		return nil, fmt.Errorf("relief: peer %d", code)
	}
	if !raw {
		if b, err = neLayerReliefBake(b); err != nil {
			return nil, err
		}
	}
	neLayerLRU.put(key, b)
	return b, nil
}

// neLayerReliefBake — the page's water key (relBake): alpha = |R − 180| × 127,
// clamped, so the model's flat tone (lakes, plains, no-data beyond the border)
// vanishes and every slope keeps its ink; 179/181 keep half.
func neLayerReliefBake(pngb []byte) ([]byte, error) {
	src, err := png.Decode(bytes.NewReader(pngb))
	if err != nil {
		return nil, err
	}
	bd := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, bd.Dx(), bd.Dy()))
	draw.Draw(dst, dst.Rect, src, bd.Min, draw.Src)
	any := false
	for i := 0; i < len(dst.Pix); i += 4 {
		d := int(dst.Pix[i]) - 180
		if d < 0 {
			d = -d
		}
		a := d * 127
		if a > 255 {
			a = 255
		}
		a = a * int(dst.Pix[i+3]) / 255
		dst.Pix[i+3] = uint8(a)
		if a > 0 {
			any = true
		}
	}
	if !any {
		return []byte{}, nil
	}
	var out bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&out, dst); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// neLayerTreeCols — the peer's /trees/bbox?format=columns answer.
type neLayerTreeCols struct {
	Truncated bool  `json:"truncated"`
	Ready     *bool `json:"ready"`
	Columns   struct {
		Lon      []float64 `json:"lon"`
		Lat      []float64 `json:"lat"`
		H        []float64 `json:"h_m"`
		Crown    []float64 `json:"crown_m2"`
		Species  []string  `json:"species"`
		Vitality []string  `json:"vitality"`
	} `json:"columns"`
}

// neLayerHtRange — "lo..hi" / "lo.." / "..hi" in metres; ok=false when absent or malformed.
func neLayerHtRange(s string) (lo, hi float64, ok bool) {
	i := strings.Index(s, "..")
	if s == "" || len(s) > 24 || i < 0 {
		return 0, 0, false
	}
	lo, hi = 0, 200
	var e1, e2 error
	if i > 0 {
		lo, e1 = strconv.ParseFloat(s[:i], 64)
	}
	if i+2 < len(s) {
		hi, e2 = strconv.ParseFloat(s[i+2:], 64)
	}
	if e1 != nil || e2 != nil || math.IsNaN(lo) || math.IsNaN(hi) || lo > hi {
		return 0, 0, false
	}
	return lo, hi, true
}

// neLayerTrees — gzip'd MVT of the apices in a z-14 tile; whole=false when the
// peer answered a partial list (not cached).
func neLayerTrees(z, x, y int, ht string) ([]byte, bool, error) {
	pr := peers()
	if pr.ObsAPI == "" {
		return nil, true, nil
	}
	hlo, hhi, hasHt := neLayerHtRange(ht)
	key := fmt.Sprintf("tree/%s/%d/%d/%d", time.Now().UTC().Format("20060102"), z, x, y)
	if hasHt {
		key += fmt.Sprintf("/%g..%g", hlo, hhi)
	}
	if b, ok := neLayerLRU.get(key); ok {
		return b, true, nil
	}
	w, s, e, n := mvtTileBox(z, x, y)
	// a crown's margin (≈ 16 m): a disc centred just outside is drawn in this tile too
	mx, my := (e-w)*0.01, (n-s)*0.01
	url := fmt.Sprintf("%s/api/v1/trees/bbox?bbox=%.6f,%.6f,%.6f,%.6f&min_height=3&format=columns&fields=lon,lat,h_m,crown_m2,species,vitality&limit=100000", pr.ObsAPI, w-mx, s-my, e+mx, n+my)
	b, code, err := neLayerPeerGet(url, 24<<20)
	if err != nil {
		return nil, false, err
	}
	if code == 404 {
		return nil, false, nil
	}
	if code != 200 {
		return nil, false, fmt.Errorf("trees: peer %d", code)
	}
	var tc neLayerTreeCols
	if err := json.Unmarshal(b, &tc); err != nil {
		return nil, false, err
	}
	c := &tc.Columns
	l := newMVTLayer("trees")
	for i := range c.Lon {
		if i >= len(c.Lat) || i >= len(c.H) {
			break
		}
		h := c.H[i]
		// 0.5 m steps, the quantisation of the cell filter (neFiltBounds): a 34.8 m tree is a 35 m apex
		if q := math.Round(h * 2); hasHt && (q < math.Ceil(hlo*2) || q > math.Floor(hhi*2)) {
			continue
		}
		px, py := mvtProject(z, x, y, c.Lon[i], c.Lat[i])
		f := l.feature(uint64(i + 1))
		f.geom = append(f.geom, 1<<3|1, zz32(int32(math.Round(px))), zz32(int32(math.Round(py))))
		f.num("h", math.Round(h*10)/10)
		if i < len(c.Crown) && c.Crown[i] > 0 {
			f.num("crown_m2", math.Round(c.Crown[i]*10)/10)
			f.num("crown_d", math.Round(2*math.Sqrt(c.Crown[i]/math.Pi)*10)/10)
		}
		if i < len(c.Species) && c.Species[i] != "" && c.Species[i] != "unknown" {
			f.str("species", c.Species[i])
		}
		if i < len(c.Vitality) && c.Vitality[i] != "" && c.Vitality[i] != "unknown" {
			f.str("vitality", c.Vitality[i])
		}
		f.donePoint()
	}
	whole := !tc.Truncated && (tc.Ready == nil || *tc.Ready)
	if l.n == 0 {
		if whole {
			neLayerLRU.put(key, []byte{})
		}
		return nil, whole, nil
	}
	gz := pmtGzip(mvtEncode(l))
	if whole {
		neLayerLRU.put(key, gz)
	}
	return gz, whole, nil
}

// neLayerTreesMaxKm2 — the largest box trees.geojson answers in one fetch
// (a closed stand holds ~6 000 apices per km²); beyond it the tiles serve.
const neLayerTreesMaxKm2 = 40.0

// handleNELayerTreesGeoJSON — GET /api/v1/ne/mvt/trees.geojson?bbox=w,s,e,n[&ht=lo..hi]
//
// The apices of a box as plain points (h, crown_d, crown_m2, species,
// vitality): a normal vector layer in QGIS, so the height classes are a
// graduated renderer the reader can re-ramp, relabel or re-break in the
// Symbology panel — a vector-tile layer has no such renderer. Fetched from
// the observed-layer peer in 0.025° pieces (its own tile size, each whole in
// one answer), relayed through here so the caller's identity is counted.
func handleNELayerTreesGeoJSON(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	box, msg := neMVTParseBox(q.Get("bbox"))
	if msg != "" {
		apiError(w, msg, 400)
		return
	}
	pr := peers()
	if pr.ObsAPI == "" {
		apiError(w, "no tree source configured", 404)
		return
	}
	km2 := neMVTBoxKm2(box[0], box[1], box[2], box[3])
	if km2 > neLayerTreesMaxKm2 {
		apiError(w, fmt.Sprintf("box too large (%.0f km², ≤ %.0f): use the tiles mvt/trees/{z}/{x}/{y}.pbf", km2, neLayerTreesMaxKm2), 413)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	const step = 0.025
	i0, i1 := int(math.Floor(box[0]/step)), int(math.Floor(box[2]/step))
	j0, j1 := int(math.Floor(box[1]/step)), int(math.Floor(box[3]/step))
	if _, ok := neMVTGate(w, r, (i1-i0+1)*(j1-j0+1), box[0], box[1], box[2], box[3]); !ok {
		return
	}
	hlo, hhi, hasHt := neLayerHtRange(q.Get("ht"))
	key := fmt.Sprintf("treej/%s/%.5f,%.5f,%.5f,%.5f/%v/%g..%g", time.Now().UTC().Format("20060102"), box[0], box[1], box[2], box[3], hasHt, hlo, hhi)
	gz, hit := neLayerLRU.get(key)
	whole := true
	if !hit {
		type piece struct {
			tc  neLayerTreeCols
			err error
		}
		var jobs [][4]float64
		for i := i0; i <= i1; i++ {
			for j := j0; j <= j1; j++ {
				jobs = append(jobs, [4]float64{math.Max(box[0], float64(i)*step), math.Max(box[1], float64(j)*step), math.Min(box[2], float64(i+1)*step), math.Min(box[3], float64(j+1)*step)})
			}
		}
		out := make([]piece, len(jobs))
		done := make(chan int, len(jobs))
		for k := range jobs {
			go func(k int) {
				defer func() { done <- k }()
				b := jobs[k]
				u := fmt.Sprintf("%s/api/v1/trees/bbox?bbox=%.6f,%.6f,%.6f,%.6f&min_height=3&format=columns&fields=lon,lat,h_m,crown_m2,species,vitality&limit=100000", pr.ObsAPI, b[0], b[1], b[2], b[3])
				body, code, err := neLayerPeerGet(u, 24<<20)
				if err == nil && code != 200 && code != 404 {
					err = fmt.Errorf("trees: peer %d", code)
				}
				if err == nil && code == 200 {
					err = json.Unmarshal(body, &out[k].tc)
				}
				out[k].err = err
			}(k)
		}
		for range jobs {
			<-done
		}
		var buf bytes.Buffer
		buf.WriteString(`{"type":"FeatureCollection","name":"trees","features":[`)
		n := 0
		for k := range out {
			if out[k].err != nil {
				neLayerPeerErr.Add(1)
				w.Header().Set("Retry-After", "5")
				w.Header().Set("Cache-Control", "no-store")
				apiError(w, "trees unavailable — retry in a few seconds", http.StatusServiceUnavailable)
				return
			}
			tc := &out[k].tc
			if tc.Truncated || (tc.Ready != nil && !*tc.Ready) {
				whole = false
			}
			c, b := &tc.Columns, jobs[k]
			for i := range c.Lon {
				if i >= len(c.Lat) || i >= len(c.H) {
					break
				}
				// half-open pieces: an apex on a shared edge is listed once
				if c.Lon[i] < b[0] || c.Lon[i] >= b[2] || c.Lat[i] < b[1] || c.Lat[i] >= b[3] {
					continue
				}
				h := c.H[i]
				if q := math.Round(h * 2); hasHt && (q < math.Ceil(hlo*2) || q > math.Floor(hhi*2)) {
					continue
				}
				if n > 0 {
					buf.WriteByte(',')
				}
				n++
				fmt.Fprintf(&buf, `{"type":"Feature","geometry":{"type":"Point","coordinates":[%.6f,%.6f]},"properties":{"h":%.1f`, c.Lon[i], c.Lat[i], h)
				if i < len(c.Crown) && c.Crown[i] > 0 {
					fmt.Fprintf(&buf, `,"crown_d":%.1f,"crown_m2":%.1f`, 2*math.Sqrt(c.Crown[i]/math.Pi), c.Crown[i])
				}
				if i < len(c.Species) && c.Species[i] != "" && c.Species[i] != "unknown" {
					fmt.Fprintf(&buf, `,"species":%q`, c.Species[i])
				}
				if i < len(c.Vitality) && c.Vitality[i] != "" && c.Vitality[i] != "unknown" {
					fmt.Fprintf(&buf, `,"vitality":%q`, c.Vitality[i])
				}
				buf.WriteString("}}")
			}
		}
		buf.WriteString("]}")
		gz = pmtGzip(buf.Bytes())
		if whole && len(gz) <= 6<<20 {
			neLayerLRU.put(key, gz)
		}
	}
	neLayerServed[1].Add(1)
	w.Header().Set("Content-Type", "application/geo+json")
	w.Header().Set("Cache-Control", "no-store")
	if whole {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	w.Header().Add("Vary", "Accept-Encoding")
	body := gz
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
	} else {
		var err error
		if body, err = neGunzip(gz); err != nil {
			apiError(w, "trees unavailable", 503)
			return
		}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Write(body)
	exportAccountFor(r, len(body))
}

// neLayerPic — one of the page's overlay pictures on a Web-Mercator tile.
func neLayerPic(view string, z, x, y int) ([]byte, error) {
	if err := neGlyphLoadFrame(); err != nil {
		return nil, err
	}
	key := fmt.Sprintf("pic/%s/%s/%d/%d/%d", view, time.Now().UTC().Format("20060102"), z, x, y)
	if b, ok := neLayerLRU.get(key); ok {
		return b, nil
	}
	w, s, e, n := mvtTileBox(z, x, y)
	x0, y0 := neGlyphProj(w, n)
	x1, y1 := neGlyphProj(e, s)
	px := float64(neLayerPicPx) / (x1 - x0)
	ph := int(math.Round((y1 - y0) * px))
	if ph < 8 || ph > 4*neLayerPicPx {
		return nil, nil
	}
	// the style switch of the renderers is the LOD: 0.87 m/px ≈ the page's LOD 1, ~2 m/px LOD 0, coarser LOD 2
	lod := neGlyphLODs[2]
	if z >= 16 {
		lod = neGlyphLODs[1]
	} else if z >= 14 {
		lod = neGlyphLODs[0]
	}
	lod.px = px
	g := neGlyphTileGeom{lod: lod, x0: x0, y0: y0, pw: neLayerPicPx, ph: ph}
	if view == "declared" || view == "observed" {
		b, err := neLayerPicCells(view == "observed", g, w, s, e, n)
		if err == nil {
			if b == nil {
				b = []byte{}
			}
			neLayerLRU.put(key, b)
		}
		return b, err
	}
	if view != "roads" { // the wire before the render gate, like handleNEGlyphTile
		defer neSubPrefetch(w, s, e, n)()
	}
	select {
	case neLayerSem <- struct{}{}:
		defer func() { <-neLayerSem }()
	case <-time.After(neMVTSemWait):
		return nil, errBusy
	}
	var img image.Image
	if view == "roads" {
		var m neGlyphRoadsMeta
		img = neGlyphRoadsRender(&g, w, s, e, n, &m)
		if m.down {
			return nil, fmt.Errorf("roads: source down")
		}
	} else {
		var m neGlyphSubMeta
		pimg, ok := neGlyphSubRender(&g, lodIndex(lod), w, s, e, n, view == "subc", &m)
		if !ok {
			return nil, fmt.Errorf("subsidy: peer down")
		}
		img = pimg
	}
	src := image.NewNRGBA(image.Rect(0, 0, g.pw, g.ph))
	draw.Draw(src, src.Rect, img, img.Bounds().Min, draw.Src)
	dst := neLayerRows(src, neLayerPicPx)
	any := false
	for i := 3; i < len(dst.Pix); i += 4 {
		if dst.Pix[i] != 0 {
			any = true
			break
		}
	}
	if !any {
		neLayerLRU.put(key, []byte{})
		return nil, nil
	}
	var out bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&out, dst); err != nil {
		return nil, err
	}
	neLayerLRU.put(key, out.Bytes())
	return out.Bytes(), nil
}

// neLayerPicCells — the cell picture of the page (declared base / observed
// overlay) for one XYZ tile, painted by the page's own painter
// (neGlyphPaintLU: dominant group, alpha = dominance, second group as an inner
// hexagon when ≥ 25 %; superseded cells faded) at 2× and box-filtered, like
// the page's supersampled tiles. Cells without an observed row stay clear.
func neLayerPicCells(observed bool, g neGlyphTileGeom, w, s, e, n float64) ([]byte, error) {
	select {
	case neLayerSem <- struct{}{}:
		defer func() { <-neLayerSem }()
	case <-time.After(neMVTSemWait):
		return nil, errBusy
	}
	const ss = 2
	g.lod.px *= ss
	g.pw *= ss
	g.ph *= ss
	ids := neBBoxCells(w-neGlyphGeomPad, s-neGlyphGeomPad, e+neGlyphGeomPad, n+neGlyphGeomPad)
	if len(ids) == 0 {
		return nil, nil
	}
	pimg := image.NewPaletted(image.Rect(0, 0, g.pw, g.ph), neGlyphTilePalette())
	var m neGlyphTileMeta
	var poly [8][2]float64
	painted := 0
	if observed {
		res := neCollect(nil, ids, []string{"lu", "obs"}, &neReq{public: false, format: "bin", bevFirst: true})
		lay := neObsLayout(res.obsFlds, res.obsRec)
		for k, id := range res.ids {
			row := res.obs[k]
			if row == nil || lay == nil || len(row) != lay.rowBytes {
				continue
			}
			if np, cx, cy, ok := g.poly(id, &poly); ok {
				neGlyphPaintLU(pimg, row[lay.luOff:lay.luOff+9], poly[:np], np, cx, cy, &m)
				painted++
			}
		}
	} else {
		res := neCollect(nil, ids, []string{"lu"}, &neReq{public: true, format: "bin", bevFirst: true})
		for k, id := range res.ids {
			np, cx, cy, ok := g.poly(id, &poly)
			if !ok {
				continue
			}
			painted++
			if res.lu[k][11]&neFlagSuperseded != 0 {
				neGlyphPaintLUFaded(pimg, res.lu[k][12:21], poly[:np], &m)
				continue
			}
			neGlyphPaintLU(pimg, res.lu[k][12:21], poly[:np], np, cx, cy, &m)
		}
	}
	if painted == 0 {
		return nil, nil
	}
	dst := neLayerRows(neGlyphDownsampleN(pimg, ss), neLayerPicPx)
	var out bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&out, dst); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// neLayerRows — resample the rows of src to h (linear, premultiplied so a
// transparent row never bleeds its colour); the width stays.
func neLayerRows(src *image.NRGBA, h int) *image.NRGBA {
	sw, sh := src.Rect.Dx(), src.Rect.Dy()
	if sh == h {
		return src
	}
	dst := image.NewNRGBA(image.Rect(0, 0, sw, h))
	for yy := 0; yy < h; yy++ {
		fy := (float64(yy)+0.5)*float64(sh)/float64(h) - 0.5
		y0 := int(math.Floor(fy))
		t := fy - float64(y0)
		y1 := y0 + 1
		if y0 < 0 {
			y0 = 0
		}
		if y1 >= sh {
			y1 = sh - 1
		}
		a, b := src.Pix[y0*src.Stride:], src.Pix[y1*src.Stride:]
		d := dst.Pix[yy*dst.Stride:]
		for xx := 0; xx < sw*4; xx += 4 {
			a0, a1 := float64(a[xx+3])*(1-t), float64(b[xx+3])*t
			al := a0 + a1
			if al < 0.5 {
				continue
			}
			for c := 0; c < 3; c++ {
				d[xx+c] = uint8((float64(a[xx+c])*a0+float64(b[xx+c])*a1)/al + 0.5)
			}
			d[xx+3] = uint8(al + 0.5)
		}
	}
	return dst
}

func neGunzip(gz []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(zr)
}
