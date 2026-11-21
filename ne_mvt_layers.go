// ne_mvt_layers.go — the companions of the cell layer in a QGIS project
// (ne_mvt_qgs.go), all served by THIS service so that one key counts every
// layer of the map and an anonymous reader still talks to one host:
//
//	GET /api/v1/ne/mvt/relief/{z}/{x}/{y}.png        the observed-layer peer's hillshade, relayed (z 10–17)
//	GET /api/v1/ne/mvt/trees/{z}/{x}/{y}.pbf         tree apices as points, layer "trees" (z 14 only; ht=lo..hi)
//	GET /api/v1/ne/mvt/pic/{declared|observed|sub|subc|roads}/{z}/{x}/{y}.png
//	                                                  the landing page's own overlay pictures as XYZ tiles (z 12–16, 512 px)
//
// relief — the peer's tile, with the page's tone curve baked in (flat ground,
// tone 180, becomes the overlay blend's neutral 128: the same rule as relBake
// in the page) so an overlay blend shades slopes and leaves plains alone; `raw=1` relays the
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
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	neLayerReliefMinZ = 10 // the tileset's minzoom (TileJSON)
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
	neLayerServed  [4]atomic.Int64          // relief, trees, pic, cells.geojson
	neLayerPeerErr atomic.Int64
)

// NELayerStats — the inflight line `ne_mvt.layers`.
func NELayerStats() map[string]interface{} {
	m := neLayerLRU.stats()
	m["relief"], m["trees"], m["pic"], m["cells_geojson"] = neLayerServed[0].Load(), neLayerServed[1].Load(), neLayerServed[2].Load(), neLayerServed[3].Load()
	m["peer_errors"] = neLayerPeerErr.Load()
	m["trees_ogc"] = neOGCServed.Load()
	neReliefVer.Lock()
	m["relief_version"] = neReliefVer.v
	m["relief_alpha_binary"] = strings.HasPrefix(neReliefVer.alpha, "binary")
	neReliefVer.Unlock()
	return m
}

// handleNEMVTLayer — rest = "mvt/relief/…", "mvt/trees/…", "mvt/pic/…"; false = not ours.
func handleNEMVTLayer(w http.ResponseWriter, r *http.Request, rest string) bool {
	if rest == "mvt/trees.geojson" {
		handleNELayerTreesGeoJSON(w, r)
		return true
	}
	if rest == "mvt/cells.geojson" {
		handleNELayerCellsGeoJSON(w, r)
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
		// a day, then a 304: the ETag is the tileset version (+ the tile is in
		// the URL), so a revalidation costs no body and no peer request
		ctype, cache = "image/png", "public, max-age=86400"
		if v := neReliefVersion(); v != "" {
			w.Header().Set("X-Tileset-Version", v)
			et := `"rel2-` + v + `"`
			w.Header().Set("ETag", et)
			if strings.Contains(r.Header.Get("If-None-Match"), et) {
				w.Header().Set("Cache-Control", cache)
				w.WriteHeader(http.StatusNotModified)
				exportAccountFor(r, 0)
				return true
			}
		}
		body, err = neLayerRelief(z, x, y, r.URL.Query().Get("raw") == "1")
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

// neLayerRelief — the peer's hillshade tile; baked: flat ground neutral grey.
func neLayerRelief(z, x, y int, raw bool) ([]byte, error) {
	pr := peers()
	if pr.ObsWeb == "" {
		return nil, nil
	}
	// the cache is keyed on the tileset version: a rebuild at the peer purges
	// every relayed tile (neReliefVersion), and ?v= busts whatever sits between
	ver := neReliefVersion()
	key := fmt.Sprintf("rel/%s/%d/%d/%d", ver, z, x, y)
	if raw {
		key = "relraw" + key[3:]
	}
	if b, ok := neLayerLRU.get(key); ok {
		return b, nil
	}
	b, code, err := neLayerPeerGet(fmt.Sprintf("%s/tiles/hillshade/%d/%d/%d.png?v=%s", pr.ObsWeb, z, x, y, url.QueryEscape(ver)), 4<<20)
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

// neReliefVersion — the `version` of the peer's hillshade tileset
// (GET /tiles/hillshade.json, TileJSON; the whole string is the key), asked
// at most once an hour (the peer's contract) and
// only while relief tiles are being served (no loop: an idle service asks
// nothing). A change drops every relayed relief tile. While the peer does not
// answer, the last known version stands (the floor: the 2026-11-21 border-cut
// rebuild). The same answer carries `attribution` (used verbatim:
// neReliefAttribution) and `alpha`: "binary…" = the tile's alpha IS the border
// (inside Austria AND terrain available), nothing is clipped here; anything
// else is logged and shown on the inflight line (`relief_alpha`).
const (
	neReliefVersionFloor = "5.1795274706.1-0818165f"
	neReliefAttrFloor    = "BEV ALS DTM (CC BY 4.0); Staatsgrenze © OpenStreetMap contributors (ODbL)"
	neReliefPoll         = time.Hour
)

var neReliefVer struct {
	sync.Mutex
	v     string
	attr  string
	alpha string
	asked time.Time
}

// neReliefAttribution — the TileJSON `attribution`, verbatim.
func neReliefAttribution() string {
	neReliefVersion()
	neReliefVer.Lock()
	defer neReliefVer.Unlock()
	if neReliefVer.attr == "" {
		return neReliefAttrFloor
	}
	return neReliefVer.attr
}

func neReliefVersion() string {
	pr := peers()
	if pr.ObsWeb == "" {
		return ""
	}
	neReliefVer.Lock()
	if neReliefVer.v == "" {
		neReliefVer.v = neReliefVersionFloor
	}
	v := neReliefVer.v
	if time.Since(neReliefVer.asked) < neReliefPoll {
		neReliefVer.Unlock()
		return v
	}
	neReliefVer.asked = time.Now() // one asker per window; the others ride on the known version
	neReliefVer.Unlock()
	b, code, err := neLayerPeerGet(pr.ObsWeb+"/tiles/hillshade.json", 64<<10)
	var tj struct {
		Version     string `json:"version"`
		Attribution string `json:"attribution"`
		Alpha       string `json:"alpha"`
	}
	if err != nil || code != 200 || json.Unmarshal(b, &tj) != nil || tj.Version == "" || len(tj.Version) > 64 {
		return v
	}
	neReliefVer.Lock()
	defer neReliefVer.Unlock()
	if tj.Attribution != "" && len(tj.Attribution) <= 300 {
		neReliefVer.attr = tj.Attribution
	}
	if a := tj.Alpha; a != neReliefVer.alpha {
		if !strings.HasPrefix(a, "binary") {
			log.Printf("relief: TileJSON alpha is no longer binary (%.80q) — relayed tiles are NOT border-clipped", a)
		}
		neReliefVer.alpha = a
	}
	if tj.Version != neReliefVer.v {
		log.Printf("relief: tileset version %s → %s, relayed tiles dropped", neReliefVer.v, tj.Version)
		neReliefVer.v = tj.Version
		neLayerLRU.dropPrefix("rel/")
		neLayerLRU.dropPrefix("relraw/")
	}
	return neReliefVer.v
}

// neReliefPalette — index = grey for 0..254, 255 = transparent.
var neReliefPalette = func() color.Palette {
	p := make(color.Palette, 256)
	for i := 0; i < 255; i++ {
		p[i] = color.NRGBA{uint8(i), uint8(i), uint8(i), 255}
	}
	p[255] = color.NRGBA{}
	return p
}()

// neLayerReliefBake — the page's tone curve (relBake): the model's flat tone
// 180 (lakes, plains) is re-centred on 128, the
// neutral grey of an overlay blend (0..180 → 0..128, 180..255 → 128..255), so
// flat ground leaves the map alone and slopes shade continuously. It was an
// alpha key (|R − 180| × 127) until 2026-11-21: every slope whose shade passes
// through 180 got a hole — triangles at the deep zooms. Alpha is the peer's
// (strictly 0 = no data or 255) and passes through untouched. A tile that is
// flat everywhere stays 204.
func neLayerReliefBake(pngb []byte) ([]byte, error) {
	src, err := png.Decode(bytes.NewReader(pngb))
	if err != nil {
		return nil, err
	}
	bd := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, bd.Dx(), bd.Dy()))
	draw.Draw(dst, dst.Rect, src, bd.Min, draw.Src)
	// one byte per pixel on the wire: the curve leaves ≤ 205 greys and alpha
	// is binary, so a tile with holes (border, missing DTM) goes out as a paletted PNG (index = grey, 255 =
	// transparent; 255 as a grey folds into 254) — a fully opaque one as plain 8-bit
	// grey; both well under the peer's RGBA bytes, in the LRU and to every client. A tile with any other alpha (the
	// contract broken) keeps RGBA.
	pal := image.NewPaletted(dst.Rect, neReliefPalette)
	gray := image.NewGray(dst.Rect)
	any, binary, opaque := false, true, true
	for i, j := 0, 0; i < len(dst.Pix); i, j = i+4, j+1 {
		v, a := int(dst.Pix[i]), dst.Pix[i+3]
		if v != 180 && a > 0 {
			any = true
		}
		if v <= 180 {
			v = (v*128 + 90) / 180
		} else {
			v = 128 + ((v-180)*127+37)/75
		}
		dst.Pix[i], dst.Pix[i+1], dst.Pix[i+2] = uint8(v), uint8(v), uint8(v)
		gray.Pix[j] = uint8(v)
		switch {
		case a == 0:
			pal.Pix[j], opaque = 255, false
		case a == 255:
			if v > 254 {
				v = 254
			}
			pal.Pix[j] = uint8(v)
		default:
			binary, opaque = false, false
		}
	}
	if !any {
		return []byte{}, nil
	}
	var out bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	var img image.Image = pal
	if opaque {
		img = gray // the inland majority: 8-bit grey, row filters work — ⅔ of the peer's bytes
	} else if !binary {
		img = dst
	}
	if err := enc.Encode(&out, img); err != nil {
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
// the observed-layer peer in 0.0125° pieces (neOGCPiece, shared with the
// trees-by-extent layer; `"truncated":true` + X-Trees-Truncated when a piece is
// not whole), relayed through here so the caller's identity is counted.
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
	// The pieces of the trees-by-extent layer (ne_mvt_ogc.go): a quarter of the
	// peer's 0.025° tile each. The peer answers one box with ≤ 100 000 rows,
	// tallest first, so a whole tile of closed stand lost its shorter trees
	// without a word; a quarter tile stays under that.
	const step = neOGCPieceStep
	i0, i1 := int(math.Floor(box[0]/step)), int(math.Floor(box[2]/step))
	j0, j1 := int(math.Floor(box[1]/step)), int(math.Floor(box[3]/step))
	if _, ok := neMVTGate(w, r, ((i1-i0+1)*(j1-j0+1)+3)/4, box[0], box[1], box[2], box[3]); !ok {
		return
	}
	hlo, hhi, hasHt := neLayerHtRange(q.Get("ht"))
	key := fmt.Sprintf("treej/%s/%.5f,%.5f,%.5f,%.5f/%v/%g..%g", time.Now().UTC().Format("20060102"), box[0], box[1], box[2], box[3], hasHt, hlo, hhi)
	gz, hit := neLayerLRU.get(key)
	whole := true
	if !hit {
		type ij struct{ i, j int }
		var ids []ij
		for i := i0; i <= i1; i++ {
			for j := j0; j <= j1; j++ {
				ids = append(ids, ij{i, j})
			}
		}
		pieces := make([][]byte, len(ids))
		errs := make([]error, len(ids))
		wholes := make([]bool, len(ids))
		done := make(chan struct{}, len(ids))
		for k := range ids {
			go func(k int) {
				defer func() { done <- struct{}{} }()
				pieces[k], wholes[k], errs[k] = neOGCPiece(ids[k].i, ids[k].j)
			}(k)
		}
		for range ids {
			<-done
		}
		for k := range ids {
			if errs[k] != nil {
				neLayerPeerErr.Add(1)
				w.Header().Set("Retry-After", "5")
				w.Header().Set("Cache-Control", "no-store")
				apiError(w, "trees unavailable — retry in a few seconds", http.StatusServiceUnavailable)
				return
			}
			whole = whole && wholes[k]
		}
		// the box is half-open like the pieces: two boxes side by side list an apex once
		bw, bs := int32(math.Ceil(box[0]*1e6)), int32(math.Ceil(box[1]*1e6))
		be, bn := int32(math.Ceil(box[2]*1e6)), int32(math.Ceil(box[3]*1e6))
		qlo, qhi := byte(0), byte(255)
		if hasHt {
			qlo, qhi = byte(math.Min(255, math.Max(0, math.Ceil(hlo*4)))), byte(math.Min(255, math.Max(0, math.Floor(hhi*4))))
		}
		var buf bytes.Buffer
		buf.WriteString(`{"type":"FeatureCollection","name":"trees",`)
		if !whole {
			// never silently short: a piece the peer cut or is still building
			buf.WriteString(`"truncated":true,`)
		}
		buf.WriteString(`"features":[`)
		n := 0
		for _, rows := range pieces {
			for o := 0; o+neOGCRow <= len(rows); o += neOGCRow {
				lon := int32(binary.LittleEndian.Uint32(rows[o:]))
				lat := int32(binary.LittleEndian.Uint32(rows[o+4:]))
				h := rows[o+8]
				if lon < bw || lon >= be || lat < bs || lat >= bn || h < qlo || h > qhi {
					continue
				}
				if n > 0 {
					buf.WriteByte(',')
				}
				n++
				fmt.Fprintf(&buf, `{"type":"Feature","geometry":{"type":"Point","coordinates":[%.6f,%.6f]},"properties":{"h":%.2f`, float64(lon)/1e6, float64(lat)/1e6, float64(h)/4)
				if cr := float64(binary.LittleEndian.Uint16(rows[o+9:])) / 10; cr > 0 {
					fmt.Fprintf(&buf, `,"crown_d":%.1f,"crown_m2":%.1f`, 2*math.Sqrt(cr/math.Pi), cr)
				}
				if sp := neOGCSpecies.word(rows[o+11] >> 4); sp != "" {
					fmt.Fprintf(&buf, `,"species":%q`, sp)
				}
				if vi := neOGCVitality.word(rows[o+11] & 15); vi != "" {
					fmt.Fprintf(&buf, `,"vitality":%q`, vi)
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
	} else {
		w.Header().Set("X-Trees-Truncated", "1")
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
