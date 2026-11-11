// ne_mvt_geojson.go — the cells of a view as ONE plain vector layer:
//
//	GET /api/v1/ne/mvt/cells.geojson?bbox=w,s,e,n[&f=<expr>][&veil=1][&res=10|11|12]
//
// A vector-tile layer can be looked at and identified, but QGIS cannot run a
// geoprocessing tool on it (intersect, join by location, zonal statistics,
// the attribute table, select by expression). This is the same hexagons with
// the same attributes as the tiles (neMVTCellAttrs / neMVTAggAttrs — one
// catalogue, nothing forked) as a FeatureCollection GDAL reads from the URL.
//
//	f=      only the cells that match — what the map leaves un-veiled
//	veil=1  the complement: the cells the filter veils (needs f=)
//
// so the two layers of one filter tile the view, each cell in exactly one.
// Resolution follows the box: res 12 (the cell itself) up to neCellsJSONKm2[0],
// then the res-11 / res-10 aggregates; larger boxes → 413 (tiles / PMTiles).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/uber/h3-go/v4"
)

// neCellsJSONKm2 — the largest box (km²) served at res 12, 11, 10: about
// 50 000 features each (≈ 25 MB of GeoJSON, 3 MB on the wire).
var neCellsJSONKm2 = [3]float64{16, 110, 700}

// neCellsJSONWait — how long a request builds missing bins itself before it
// answers 503 (a project queues them when it is written, so this is rare).
const neCellsJSONWait = 45 * time.Second

// neCellsJSONRes — the finest resolution a box of that size is served at; 0 = too large.
func neCellsJSONRes(km2 float64) int {
	for i, m := range neCellsJSONKm2 {
		if km2 <= m {
			return 12 - i
		}
	}
	return 0
}

// neCellsJSONBins — the bins a box needs at res (as neMVTTileBins).
func neCellsJSONBins(box [4]float64, res int) [][2]int {
	px, py := neMVTResPadDeg(res)
	px, py = 2*px, 2*py
	if res == 12 {
		px, py = 0.0004, 0.0003
	}
	return neMVTBins(box[0]-px, box[1]-py, box[2]+px, box[3]+py)
}

// neCellsJSONPrepare — queue the bins of a view (the project writer calls it:
// by the time QGIS opens the file the layer answers at once).
func neCellsJSONPrepare(box [4]float64) {
	res := neCellsJSONRes(neMVTBoxKm2(box[0], box[1], box[2], box[3]))
	if res == 0 {
		return
	}
	neMVTBinsStatus(neCellsJSONBins(box, res), true)
}

// mvtFeatureJSON — a feature's tags as the members of a JSON object (no braces).
func mvtFeatureJSON(buf *bytes.Buffer, f *mvtFeature) {
	for i := 0; i+1 < len(f.tags); i += 2 {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(strconv.Quote(f.l.keys[f.tags[i]]))
		buf.WriteByte(':')
		switch v := f.l.vals[f.tags[i+1]]; v.kind {
		case 's':
			s, _ := json.Marshal(v.s)
			buf.Write(s)
		case 'f':
			buf.WriteString(strconv.FormatFloat(v.f, 'f', -1, 64))
		case 'i':
			buf.WriteString(strconv.FormatInt(v.i, 10))
		case 'b':
			buf.WriteString(strconv.FormatBool(v.b))
		}
	}
}

// neCellsJSONRender — the FeatureCollection from ready bins.
func neCellsJSONRender(box [4]float64, res int, bins [][2]int, blobs [][]byte, states []string, expr *neFiltExpr, veil bool) ([]byte, int) {
	filt := expr != nil && !expr.empty()
	kw, ks, ke, kn := neMVTKeepBox(box[0], box[1], box[2], box[3], res)
	in := func(lon, lat float64) bool { return lon >= kw && lon < ke && lat >= ks && lat < kn }
	layer := newMVTLayer(neMVTLayerName) // scratch: the attribute writers speak mvtFeature
	var buf bytes.Buffer
	buf.WriteString(`{"type":"FeatureCollection","name":"cells","features":[`)
	n := 0
	emit := func(id uint64, f *mvtFeature) {
		bd, err := h3.Cell(id).Boundary()
		if err != nil || len(bd) < 3 {
			return
		}
		if n > 0 {
			buf.WriteByte(',')
		}
		n++
		buf.WriteString(`{"type":"Feature","geometry":{"type":"Polygon","coordinates":[[`)
		for i := 0; i <= len(bd); i++ {
			p := bd[i%len(bd)]
			if i > 0 {
				buf.WriteByte(',')
			}
			fmt.Fprintf(&buf, "[%.7f,%.7f]", p.Lng, p.Lat)
		}
		buf.WriteString(`]]},"properties":{`)
		mvtFeatureJSON(&buf, f)
		buf.WriteString(`}}`)
	}
	if res == 12 {
		seen := map[uint64]bool{}
		for q := range bins {
			if blobs[q] == nil {
				continue
			}
			b, err := neFidxDecode(blobs[q])
			if err != nil {
				continue
			}
			b.Hdr.I, b.Hdr.J = bins[q][0], bins[q][1]
			ids := neMVTBinIDs(b, states[q])
			var mask []uint8
			if filt {
				mask = b.eval(expr)
			}
			for k := 0; k < b.N; k++ {
				if mask != nil && (mask[k] == 0) != veil {
					continue
				}
				lon, lat := neMVTBinLonLat(b, k)
				if !in(lon, lat) || ids[k] == 0 || seen[ids[k]] {
					continue
				}
				seen[ids[k]] = true
				f := layer.feature(ids[k])
				f.str("h3", neCellHex(ids[k]))
				f.i64("res", 12)
				if filt {
					f.flag("match", !veil)
				}
				neMVTCellAttrs(f, b, k)
				neMVTEpochs(f, b)
				if a, err := h3.CellAreaM2(h3.Cell(ids[k])); err == nil {
					f.num("area_m2", neRound(a, 1))
				}
				f.num("lon", neRound(lon, 6))
				f.num("lat", neRound(lat, 6))
				emit(ids[k], f)
			}
		}
	} else {
		aggs := map[uint64]*neMVTAgg{}
		keep := map[uint64]bool{}
		for q := range bins {
			if blobs[q] == nil {
				continue
			}
			b, err := neFidxDecode(blobs[q])
			if err != nil {
				continue
			}
			b.Hdr.I, b.Hdr.J = bins[q][0], bins[q][1]
			ids := neMVTBinIDs(b, states[q])
			var mask []uint8
			if filt {
				mask = b.eval(expr)
			}
			for k := 0; k < b.N; k++ {
				if ids[k] == 0 {
					continue
				}
				p := neH3ParentFast(ids[k], res)
				ok, seen := keep[p]
				if !seen {
					lon, lat := neCellCentre(p)
					ok = in(lon, lat)
					keep[p] = ok
				}
				if !ok {
					continue
				}
				a := aggs[p]
				if a == nil {
					a = &neMVTAgg{}
					aggs[p] = a
				}
				if filt {
					a.nAll++
					if (mask[k] == 0) != veil {
						continue
					}
				}
				neMVTAggAdd(a, b, k)
			}
		}
		parents := make([]uint64, 0, len(aggs))
		for p, a := range aggs {
			if a.n > 0 {
				parents = append(parents, p)
			}
		}
		sortU64(parents)
		for _, p := range parents {
			f := layer.feature(p)
			f.str("h3", neCellHex(p))
			f.i64("res", int64(res))
			if filt {
				f.flag("match", !veil)
			}
			area, _ := h3.CellAreaM2(h3.Cell(p))
			neMVTAggAttrs(f, aggs[p], area/1e4)
			emit(p, f)
		}
	}
	buf.WriteString("]}")
	return buf.Bytes(), n
}

// handleNELayerCellsGeoJSON — GET /api/v1/ne/mvt/cells.geojson
func handleNELayerCellsGeoJSON(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	box, msg := neMVTParseBox(q.Get("bbox"))
	if msg != "" {
		apiError(w, msg, 400)
		return
	}
	var expr *neFiltExpr
	canon := ""
	if f := q.Get("f"); f != "" {
		expr = neFiltParse(f)
		if !expr.empty() {
			canon = expr.Canonical()
		}
	}
	veil := q.Get("veil") == "1"
	if veil && canon == "" {
		apiError(w, "veil=1 is the complement of a filter: give f=", 400)
		return
	}
	km2 := neMVTBoxKm2(box[0], box[1], box[2], box[3])
	res := neCellsJSONRes(km2)
	if rq, err := strconv.Atoi(q.Get("res")); err == nil && rq >= 10 && rq <= 12 && (res == 0 || rq < res) {
		if km2 <= neCellsJSONKm2[2] {
			res = rq // coarser than the box needs is always fine
		}
	}
	if res == 0 {
		apiError(w, fmt.Sprintf("box too large (%.0f km², ≤ %.0f): use the tiles mvt/{z}/{x}/{y}.pbf or a PMTiles archive", km2, neCellsJSONKm2[2]), 413)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	x0, y0, x1, y1 := mvtTilesInBox(res+2, box[0], box[1], box[2], box[3])
	if _, ok := neMVTGate(w, r, (x1-x0+1)*(y1-y0+1), box[0], box[1], box[2], box[3]); !ok {
		return
	}
	bins := neCellsJSONBins(box, res)
	blobs, states, missing, prog := neMVTBinsStatus(bins, false)
	if len(missing) > 0 {
		// build what is missing here, a few at a time, within the wait
		deadline := time.Now().Add(neCellsJSONWait)
		sem := make(chan struct{}, 3)
		var wg sync.WaitGroup
		for _, k := range missing {
			if time.Now().After(deadline) {
				break
			}
			sem <- struct{}{}
			wg.Add(1)
			go func(k [2]int) {
				defer func() { <-sem; wg.Done() }()
				if time.Now().After(deadline) {
					return
				}
				neFidxGet(k[0], k[1])
				neMVTStateForget(k[0], k[1])
			}(k)
		}
		wg.Wait()
		blobs, states, missing, prog = neMVTBinsStatus(bins, true)
	}
	if len(missing) > 0 {
		neMVTPending.Add(1)
		w.Header().Set("Retry-After", strconv.Itoa(max(5, min(prog.EtaS, 120))))
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{"status": "building", "progress": prog,
			"hint": "the area is being prepared — reload the layer in a moment; /api/v1/ne/mvt/status?bbox= shows progress for a box"})
		return
	}
	key := "cellj/" + neMVTTileKey(res, 0, 0, append(append([]string{}, states...), fmt.Sprintf("%.5f,%.5f,%.5f,%.5f", box[0], box[1], box[2], box[3]), "f="+canon, fmt.Sprint(veil)))
	gz, hit := neLayerLRU.get(key)
	if !hit {
		select {
		case neMVTSem <- struct{}{}:
		case <-time.After(neMVTSemWait):
			w.Header().Set("Retry-After", "3")
			w.Header().Set("Cache-Control", "no-store")
			apiError(w, "busy — retry in a few seconds", http.StatusServiceUnavailable)
			return
		}
		err := neGlyphLoadFrame()
		var raw []byte
		if err == nil {
			raw, _ = neCellsJSONRender(box, res, bins, blobs, states, expr, veil)
		}
		<-neMVTSem
		if err != nil {
			apiError(w, "cells unavailable", 503)
			return
		}
		gz = pmtGzip(raw)
		if len(gz) <= 6<<20 {
			neLayerLRU.put(key, gz)
		}
	}
	neLayerServed[3].Add(1)
	etag := `"` + strings.TrimPrefix(key, "cellj/") + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Add("Vary", "Accept-Encoding")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/geo+json")
	body := gz
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
	} else {
		var err error
		if body, err = neGunzip(gz); err != nil {
			apiError(w, "cells unavailable", 503)
			return
		}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Write(body)
	exportAccountFor(r, len(body))
}
