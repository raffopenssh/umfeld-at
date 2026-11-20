// ne_mvt_ogc.go — the trees as an OGC API Features collection, so that a GIS
// asks for the part of the map it is looking at and keeps what it got:
//
//	GET /api/v1/ne/mvt/ogc                               landing page
//	GET /api/v1/ne/mvt/ogc/conformance · /collections · /collections/trees
//	GET /api/v1/ne/mvt/ogc/collections/trees/items?bbox=w,s,e,n[&limit=][&offset=][&ht=lo..hi]
//
// Why: trees.geojson is ONE file for ONE box. GDAL downloads and parses it
// again on every open (render, attribute table, each export — 40 MB of JSON a
// time for a wooded view) and it never follows the map. QGIS reads an OGC API
// Features source by the current extent, page by page, into a local cache, so
// "save features as…" writes what the reader sees.
//
// Bounds: a box ≤ neOGCTreesMaxKm2, pages ≤ neOGCLimitMax features, pieces of
// 0.0125° fetched from the observed-layer peer at most once a day and kept as
// 12 bytes per tree in neLayerLRU (bounded, `ne_mvt.layers`). No bbox = a
// handful of trees so a client can read the fields — never the country.
// Every first page passes neMVTGate like trees.geojson.
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
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
	neOGCTreesMaxKm2 = 40.0
	neOGCLimitDef    = 10000
	neOGCLimitMax    = 50000
	neOGCPieceStep   = 0.0125 // a quarter of the peer's tile: a closed stand stays under its 100 000-row answer
	neOGCRow         = 12     // lon, lat int32 (1e-6°) · h uint8 (¼ m) · crown uint16 (0.1 m²) · species/vitality nibbles
)

const neOGCOpenAPIType = "application/vnd.oai.openapi+json;version=3.0"

var (
	neOGCServed atomic.Int64
	// neOGCSampleBox — what an items request without bbox reads (a schema probe).
	neOGCSampleBox = [4]float64{16.1780, 48.2505, 16.1790, 48.2510}
)

// neOGCVocab — the peer's species / vitality words as nibbles (0 = unknown or
// not said). Grows as words are met, at most 15 each; process-wide like the
// LRU the packed rows live in.
type neOGCVocab struct {
	mu    sync.RWMutex
	words []string
}

var neOGCSpecies, neOGCVitality = &neOGCVocab{words: []string{""}}, &neOGCVocab{words: []string{""}}

func (v *neOGCVocab) idx(s string) byte {
	if s == "" || s == "unknown" || len(s) > 32 {
		return 0
	}
	v.mu.RLock()
	for i := 1; i < len(v.words); i++ {
		if v.words[i] == s {
			v.mu.RUnlock()
			return byte(i)
		}
	}
	v.mu.RUnlock()
	v.mu.Lock()
	defer v.mu.Unlock()
	for i := 1; i < len(v.words); i++ {
		if v.words[i] == s {
			return byte(i)
		}
	}
	if len(v.words) >= 16 {
		return 0
	}
	v.words = append(v.words, s)
	return byte(len(v.words) - 1)
}

func (v *neOGCVocab) word(i byte) string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if int(i) < len(v.words) {
		return v.words[i]
	}
	return ""
}

// neOGCPiece — the trees of piece (i,j) of the 0.025° grid, packed; whole =
// the peer said this is all of them (only then it is cached).
func neOGCPiece(i, j int) (rows []byte, whole bool, err error) {
	key := fmt.Sprintf("treep/%s/%d/%d", time.Now().UTC().Format("20060102"), i, j)
	if b, ok := neLayerLRU.get(key); ok {
		return b, true, nil
	}
	pr := peers()
	w, s := float64(i)*neOGCPieceStep, float64(j)*neOGCPieceStep
	u := fmt.Sprintf("%s/api/v1/trees/bbox?bbox=%.6f,%.6f,%.6f,%.6f&min_height=3&format=columns&fields=lon,lat,h_m,crown_m2,species,vitality&limit=100000", pr.ObsAPI, w, s, w+neOGCPieceStep, s+neOGCPieceStep)
	body, code, err := neLayerPeerGet(u, 24<<20)
	if err != nil {
		return nil, false, err
	}
	if code == 404 {
		return nil, true, nil
	}
	if code != 200 {
		return nil, false, fmt.Errorf("trees: peer %d", code)
	}
	var tc neLayerTreeCols
	if err := json.Unmarshal(body, &tc); err != nil {
		return nil, false, err
	}
	c := &tc.Columns
	n := min(len(c.Lon), len(c.Lat), len(c.H))
	rows = make([]byte, 0, n*neOGCRow)
	var rec [neOGCRow]byte
	for k := 0; k < n; k++ {
		// half-open pieces: an apex on a shared edge is in one piece only
		if c.Lon[k] < w || c.Lon[k] >= w+neOGCPieceStep || c.Lat[k] < s || c.Lat[k] >= s+neOGCPieceStep {
			continue
		}
		binary.LittleEndian.PutUint32(rec[0:], uint32(int32(math.Round(c.Lon[k]*1e6))))
		binary.LittleEndian.PutUint32(rec[4:], uint32(int32(math.Round(c.Lat[k]*1e6))))
		rec[8] = byte(math.Min(255, math.Max(0, math.Round(c.H[k]*4))))
		cr := 0.0
		if k < len(c.Crown) {
			cr = c.Crown[k]
		}
		binary.LittleEndian.PutUint16(rec[9:], uint16(math.Min(65535, math.Max(0, math.Round(cr*10)))))
		var sp, vi byte
		if k < len(c.Species) {
			sp = neOGCSpecies.idx(c.Species[k])
		}
		if k < len(c.Vitality) {
			vi = neOGCVitality.idx(c.Vitality[k])
		}
		rec[11] = sp<<4 | vi
		rows = append(rows, rec[:]...)
	}
	whole = !tc.Truncated && (tc.Ready == nil || *tc.Ready)
	if whole {
		neLayerLRU.put(key, rows)
	}
	return rows, whole, nil
}

// neOGCLink — one link object; q = the caller's own parameters (key, ht) carried along.
func neOGCLink(rel, typ, href string) map[string]string {
	return map[string]string{"rel": rel, "type": typ, "href": href}
}

func neOGCTreesCollection(base, qs string) map[string]interface{} {
	pr := peers()
	return map[string]interface{}{
		"id": "trees", "title": "Trees (LiDAR apices)", "itemType": "feature",
		"description": "Every tree found by LiDAR (h ≥ 3 m) as a point: name (label), h (m), crown_d (m), species, vitality. Ask with bbox= (≤ " + strconv.Itoa(int(neOGCTreesMaxKm2)) + " km²); without it a few trees come back as a sample of the fields. ht=lo..hi limits the height. Source: " + pr.ObsName + " (CC BY 4.0), BEV.",
		"extent":      map[string]interface{}{"spatial": map[string]interface{}{"bbox": [][4]float64{neMVTAustria}, "crs": "http://www.opengis.net/def/crs/OGC/1.3/CRS84"}},
		"crs":         []string{"http://www.opengis.net/def/crs/OGC/1.3/CRS84"},
		"links": []map[string]string{
			neOGCLink("self", "application/json", base+"/collections/trees"+qs),
			neOGCLink("items", "application/geo+json", base+"/collections/trees/items"+qs),
			neOGCLink("license", "text/html", "https://creativecommons.org/licenses/by/4.0/"),
		},
	}
}

// handleNEOGC — rest = "mvt/ogc[/…]"; false = not ours.
func handleNEOGC(w http.ResponseWriter, r *http.Request, rest string) bool {
	if rest != "mvt/ogc" && !strings.HasPrefix(rest, "mvt/ogc/") {
		return false
	}
	sub := strings.Trim(strings.TrimPrefix(rest, "mvt/ogc"), "/")
	if peers().ObsAPI == "" {
		apiError(w, "no tree source configured", 404)
		return true
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	// links point back at the prefix the caller used and keep its key / ht
	path := r.URL.Path
	if i := strings.Index(path, "/mvt/ogc"); i >= 0 {
		path = path[:i+len("/mvt/ogc")]
	}
	base := publicBaseURL(r) + path
	q := r.URL.Query()
	keep := url.Values{}
	for _, k := range []string{"key", "ht"} {
		if v := q.Get(k); v != "" && len(v) <= 64 {
			keep.Set(k, v)
		}
	}
	qs := ""
	if len(keep) > 0 {
		qs = "?" + keep.Encode()
	}
	meta := func(v interface{}) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		writeJSON(w, 200, v)
	}
	switch sub {
	case "":
		meta(map[string]interface{}{"title": "umfeld-at · trees", "description": "OGC API Features: the trees of the map as points, by extent.",
			"links": []map[string]string{
				neOGCLink("self", "application/json", base+qs),
				neOGCLink("conformance", "application/json", base+"/conformance"+qs),
				neOGCLink("data", "application/json", base+"/collections"+qs),
				neOGCLink("service-desc", neOGCOpenAPIType, base+"/api"+qs), // QGIS takes no source without one
				neOGCLink("service-doc", "text/markdown", publicBaseURL(r)+"/api/v1/docs/qgis-layer.md"),
			}})
	case "api":
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Header().Set("Content-Type", neOGCOpenAPIType)
		b, _ := json.Marshal(map[string]interface{}{"openapi": "3.0.2",
			"info":    map[string]string{"title": "umfeld-at · trees", "version": "1", "description": "Trees as points by extent. License CC BY 4.0."},
			"servers": []map[string]string{{"url": base}},
			"paths": map[string]interface{}{"/collections/trees/items": map[string]interface{}{"get": map[string]interface{}{
				"parameters": []map[string]string{{"$ref": "#/components/parameters/bbox"}, {"$ref": "#/components/parameters/limit"}, {"$ref": "#/components/parameters/offset"}, {"$ref": "#/components/parameters/ht"}},
				"responses":  map[string]interface{}{"200": map[string]string{"description": "GeoJSON FeatureCollection"}}}}},
			"components": map[string]interface{}{"parameters": map[string]interface{}{
				"bbox":   map[string]interface{}{"name": "bbox", "in": "query", "description": "w,s,e,n in degrees, ≤ " + strconv.Itoa(int(neOGCTreesMaxKm2)) + " km²", "schema": map[string]interface{}{"type": "array", "items": map[string]string{"type": "number"}, "minItems": 4, "maxItems": 6}, "style": "form", "explode": false},
				"limit":  map[string]interface{}{"name": "limit", "in": "query", "schema": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": neOGCLimitMax, "default": neOGCLimitDef}},
				"offset": map[string]interface{}{"name": "offset", "in": "query", "schema": map[string]interface{}{"type": "integer", "minimum": 0, "default": 0}},
				"ht":     map[string]interface{}{"name": "ht", "in": "query", "description": "height range in metres, lo..hi", "schema": map[string]string{"type": "string"}},
			}}})
		w.Write(b)
	case "conformance":
		meta(map[string]interface{}{"conformsTo": []string{
			"http://www.opengis.net/spec/ogcapi-features-1/1.0/conf/core",
			"http://www.opengis.net/spec/ogcapi-features-1/1.0/conf/geojson"}})
	case "collections":
		meta(map[string]interface{}{"links": []map[string]string{neOGCLink("self", "application/json", base+"/collections"+qs)},
			"collections": []interface{}{neOGCTreesCollection(base, qs)}})
	case "collections/trees":
		meta(neOGCTreesCollection(base, qs))
	case "collections/trees/items":
		handleNEOGCTreeItems(w, r, base+"/collections/trees/items", keep)
	default:
		apiError(w, "mvt/ogc · /conformance · /collections · /collections/trees · /collections/trees/items?bbox=", 404)
	}
	return true
}

func handleNEOGCTreeItems(w http.ResponseWriter, r *http.Request, self string, keep url.Values) {
	q := r.URL.Query()
	limit, offset := neOGCLimitDef, 0
	if s := q.Get("limit"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 1 {
			apiError(w, "limit: 1–"+strconv.Itoa(neOGCLimitMax), 400)
			return
		}
		limit = min(v, neOGCLimitMax)
	}
	if s := q.Get("offset"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 0 {
			apiError(w, "offset ≥ 0", 400)
			return
		}
		offset = v
	}
	bq := q.Get("bbox")
	sample := bq == ""
	var box [4]float64
	empty := func() {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Header().Set("Content-Type", "application/geo+json")
		w.Write([]byte(`{"type":"FeatureCollection","features":[],"numberReturned":0,"links":[]}`))
	}
	if sample {
		// A client reading the fields asks for a handful: a few real trees, one
		// page. Anything larger without a box is a download of "everything" —
		// answered empty, so the sample never lands in somebody's export.
		if q.Get("limit") == "" || limit > 10 {
			empty()
			return
		}
		box, offset = neOGCSampleBox, 0
	} else {
		if p := strings.Split(bq, ","); len(p) == 6 { // w,s,zmin,e,n,zmax
			bq = p[0] + "," + p[1] + "," + p[3] + "," + p[4]
		}
		var msg string
		if box, msg = neMVTParseBox(bq); msg != "" {
			if msg == "box outside Austria or empty" { // a valid question with an empty answer
				empty()
				return
			}
			apiError(w, msg, 400)
			return
		}
		for k := 0; k < 4; k++ { // clip to the country: a GIS asks for its whole canvas
			if k < 2 {
				box[k] = math.Max(box[k], neMVTAustria[k])
			} else {
				box[k] = math.Min(box[k], neMVTAustria[k])
			}
		}
		if km2 := neMVTBoxKm2(box[0], box[1], box[2], box[3]); km2 > neOGCTreesMaxKm2 {
			w.Header().Set("Cache-Control", "no-store")
			apiError(w, fmt.Sprintf("view too large for single trees (%.0f km², ≤ %.0f) — zoom in", km2, neOGCTreesMaxKm2), 413)
			return
		}
	}
	i0, i1 := int(math.Floor(box[0]/neOGCPieceStep)), int(math.Floor(box[2]/neOGCPieceStep))
	j0, j1 := int(math.Floor(box[1]/neOGCPieceStep)), int(math.Floor(box[3]/neOGCPieceStep))
	charge := 0
	if offset == 0 && !sample {
		charge = ((i1-i0+1)*(j1-j0+1) + 3) / 4 // counted in the peer's 0.025° tiles, like trees.geojson
	}
	if _, ok := neMVTGate(w, r, charge, box[0], box[1], box[2], box[3]); !ok {
		return
	}
	hlo, hhi, hasHt := neLayerHtRange(q.Get("ht"))
	qlo, qhi := byte(0), byte(255)
	if hasHt && !sample { // the sample shows the fields whatever the filter
		qlo, qhi = byte(math.Min(255, math.Max(0, math.Ceil(hlo*4)))), byte(math.Min(255, math.Max(0, math.Floor(hhi*4))))
	}
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
	whole := true
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
	bw, bs := int32(math.Floor(box[0]*1e6)), int32(math.Floor(box[1]*1e6))
	be, bn := int32(math.Ceil(box[2]*1e6)), int32(math.Ceil(box[3]*1e6))
	var buf bytes.Buffer
	buf.Grow(min(limit, 20000) * 190)
	buf.WriteString(`{"type":"FeatureCollection","features":[`)
	seen, n, more := 0, 0, false
scan:
	for _, rows := range pieces {
		for o := 0; o+neOGCRow <= len(rows); o += neOGCRow {
			lon := int32(binary.LittleEndian.Uint32(rows[o:]))
			lat := int32(binary.LittleEndian.Uint32(rows[o+4:]))
			h := rows[o+8]
			if lon < bw || lon > be || lat < bs || lat > bn || h < qlo || h > qhi {
				continue
			}
			if sv := rows[o+11]; sample && (sv>>4 == 0 || sv&15 == 0 || rows[o+9]|rows[o+10] == 0) {
				continue // the sample: trees with every field said, so each gets its type
			}
			if seen++; seen <= offset {
				continue
			}
			if n == limit {
				more = true
				break scan
			}
			if n > 0 {
				buf.WriteByte(',')
			}
			n++
			neOGCTreeFeature(&buf, lon, lat, h, binary.LittleEndian.Uint16(rows[o+9:]), rows[o+11])
		}
	}
	buf.WriteString(`],"numberReturned":`)
	buf.WriteString(strconv.Itoa(n))
	buf.WriteString(`,"links":[`)
	if more && !sample {
		nx := url.Values{}
		for k, v := range keep {
			nx[k] = v
		}
		nx.Set("bbox", fmt.Sprintf("%.6f,%.6f,%.6f,%.6f", box[0], box[1], box[2], box[3]))
		nx.Set("limit", strconv.Itoa(limit))
		nx.Set("offset", strconv.Itoa(offset+n))
		l, _ := json.Marshal(neOGCLink("next", "application/geo+json", self+"?"+nx.Encode()))
		buf.Write(l)
	}
	buf.WriteString("]}")
	neOGCServed.Add(1)
	w.Header().Set("Content-Type", "application/geo+json")
	w.Header().Set("Cache-Control", "no-store")
	if whole {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	w.Write(buf.Bytes())
	exportAccountFor(r, buf.Len())
}

// neOGCTreeFeature — one tree. The id is its position (1e-6°): stable between
// fetches, so a client's cache keeps one copy, and it is what a report names.
// name = the label a field app shows ("26 m" / "26 m spruce").
func neOGCTreeFeature(buf *bytes.Buffer, lon, lat int32, h byte, crown uint16, sv byte) {
	hm := float64(h) / 4
	sp, vi := neOGCSpecies.word(sv>>4), neOGCVitality.word(sv&15)
	name := strconv.FormatFloat(math.Round(hm), 'f', 0, 64) + " m"
	if sp != "" {
		name += " " + sp
	}
	fmt.Fprintf(buf, `{"type":"Feature","id":"t%d_%d","geometry":{"type":"Point","coordinates":[%.6f,%.6f]},"properties":{"name":%q,"h":%.2f`, lon, lat, float64(lon)/1e6, float64(lat)/1e6, name, hm)
	if crown > 0 {
		fmt.Fprintf(buf, `,"crown_d":%.1f`, 2*math.Sqrt(float64(crown)/10/math.Pi))
	} else {
		buf.WriteString(`,"crown_d":null`)
	}
	if sp != "" {
		fmt.Fprintf(buf, `,"species":%q`, sp)
	} else {
		buf.WriteString(`,"species":null`)
	}
	if vi != "" {
		fmt.Fprintf(buf, `,"vitality":%q`, vi)
	} else {
		buf.WriteString(`,"vitality":null`)
	}
	buf.WriteString("}}")
}
