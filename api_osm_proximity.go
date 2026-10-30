// api_osm_proximity.go — HTTP endpoints for OSM proximity metrics.
//
//	GET /api/v1/osm/point?lon=&lat=          — ad-hoc metrics for a point
//	GET /api/v1/osm/stats                    — import/cache status
package main

import (
	"database/sql"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func RegisterOSMProximityAPI(mux *http.ServeMux) {
	mux.Handle("/api/v1/osm/point", apiMiddleware(handleV1OSMPoint))
	mux.Handle("/api/v1/osm/stats", apiMiddleware(handleV1OSMStats))
	mux.Handle("/api/v1/osm/geometry", apiMiddleware(handleV1OSMGeometry))
}

// handleV1OSMGeometry serves the raw OSM feature geometries as a GeoJSON
// FeatureCollection so clients can draw roads/rail/water/transit fast.
//
// Selection (one of):
//
//	?kg=63349                          — KG bbox (from search index)
//	?bbox=west,south,east,north        — explicit bbox (EPSG:4326)
//
// Filters:
//
//	?cat=road,rail,water,water_area,transit,place — default "road"
//	                                   water      = waterway centrelines + area-outline chunks (distance layer)
//	                                   water_area = river/lake/reservoir/wetland (Multi)Polygons, closed & fillable
//	?clip=0                            — water_area: return whole polygons instead of bbox-clipped ones
//	?major=1                           — roads: only motorway/trunk/primary/secondary
//	?limit=N                           — max features (default 20000)
func handleV1OSMGeometry(w http.ResponseWriter, r *http.Request) {
	if !OSMProximityReady() {
		apiError(w, "OSM proximity data not imported (run -osm-import)", 503)
		return
	}
	q := r.URL.Query()
	start := time.Now()
	// Public tier (api_tiers.go): scope by bbox only.
	if !isPrivateTier(r) && q.Get("kg") != "" {
		apiError(w, "public tier: scope with ?bbox=west,south,east,north (kg= is not available)", 400)
		return
	}

	// ── resolve bbox ──
	var west, south, east, north float64
	var scope string
	switch {
	case q.Get("bbox") != "":
		parts := strings.Split(q.Get("bbox"), ",")
		if len(parts) != 4 {
			apiError(w, "bbox must be west,south,east,north", 400)
			return
		}
		var errs [4]error
		west, errs[0] = strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
		south, errs[1] = strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		east, errs[2] = strconv.ParseFloat(strings.TrimSpace(parts[2]), 64)
		north, errs[3] = strconv.ParseFloat(strings.TrimSpace(parts[3]), 64)
		for _, e := range errs {
			if e != nil {
				apiError(w, "invalid bbox value", 400)
				return
			}
		}
		scope = "bbox"
	case q.Get("kg") != "":
		if globalSearchIndex == nil {
			apiError(w, "search index unavailable", 503)
			return
		}
		err := globalSearchIndex.DB().QueryRow(
			`SELECT min_lon, min_lat, max_lon, max_lat FROM kg WHERE kg_code=?`, q.Get("kg")).
			Scan(&west, &south, &east, &north)
		if err != nil {
			apiError(w, "KG not found: "+q.Get("kg"), 404)
			return
		}
		scope = "kg:" + q.Get("kg")
	default:
		apiError(w, "one of ?kg=, ?bbox= required", 400)
		return
	}

	// ── filters ──
	cats := map[string]bool{}
	for _, c := range strings.Split(q.Get("cat"), ",") {
		c = strings.TrimSpace(c)
		if c != "" {
			cats[c] = true
		}
	}
	if len(cats) == 0 {
		cats["road"] = true
	}
	majorOnly := q.Get("major") == "1" || q.Get("major") == "true"
	limit := 20000
	if l, err := strconv.Atoi(q.Get("limit")); err == nil && l > 0 && l <= 100000 {
		limit = l
	}

	features := []map[string]interface{}{}
	truncated := false

	// line categories
	lineCats := []string{}
	for _, c := range []string{"road", "rail", "water"} {
		if cats[c] {
			lineCats = append(lineCats, c)
		}
	}
	if len(lineCats) > 0 {
		ph := strings.Repeat("?,", len(lineCats))
		ph = ph[:len(ph)-1]
		extra := ""
		if majorOnly {
			extra = " AND (l.cat != 'road' OR l.major=1)"
		}
		args := []interface{}{west, east, south, north}
		for _, c := range lineCats {
			args = append(args, c)
		}
		rows, err := osmDB.Query(`
			SELECT l.cat, l.major, l.fclass, l.name, l.ref, l.coords
			FROM osm_lines_rtree rt JOIN osm_lines l ON l.id = rt.id
			WHERE rt.max_lon >= ? AND rt.min_lon <= ? AND rt.max_lat >= ? AND rt.min_lat <= ?
			  AND l.cat IN (`+ph+`)`+extra, args...)
		if err != nil {
			apiError(w, "query error: "+err.Error(), 500)
			return
		}
		for rows.Next() {
			if len(features) >= limit {
				truncated = true
				break
			}
			var cat string
			var major int
			var fclass, name, ref sql.NullString
			var coords []byte
			if err := rows.Scan(&cat, &major, &fclass, &name, &ref, &coords); err != nil {
				continue
			}
			pts := unpackCoords(coords)
			coordArr := make([][2]float64, len(pts))
			for i, p := range pts {
				coordArr[i] = [2]float64{roundCoord(p[0]), roundCoord(p[1])}
			}
			props := map[string]interface{}{"cat": cat, "fclass": fclass.String}
			if name.String != "" {
				props["name"] = name.String
			}
			if ref.String != "" {
				props["ref"] = ref.String
			}
			if cat == "road" {
				props["major"] = major == 1
			}
			features = append(features, map[string]interface{}{
				"type":       "Feature",
				"geometry":   map[string]interface{}{"type": "LineString", "coordinates": coordArr},
				"properties": props,
			})
		}
		rows.Close()
	}

	// area categories (feedback #16): whole polygons, clipped to the bbox
	areasNote := ""
	if cats["water_area"] {
		clip := q.Get("clip") != "0" && q.Get("clip") != "false"
		areas, trunc, err := OSMAreasInBBox(west, south, east, north, map[string]bool{"water": true}, clip, limit-len(features))
		if err != nil {
			areasNote = err.Error()
		}
		if trunc {
			truncated = true
		}
		for _, a := range areas {
			props := map[string]interface{}{"cat": "water_area", "fclass": a.Fclass, "area_sqm": math.Round(a.AreaSqm)}
			if a.Name != "" {
				props["name"] = a.Name
			}
			if a.Clipped {
				props["clipped"] = true
			}
			props["osm_area_id"] = a.ID
			features = append(features, map[string]interface{}{
				"type":       "Feature",
				"geometry":   multiPolygonToGeoJSON(a.Geom),
				"properties": props,
			})
		}
	}

	// point categories
	pointCats := []string{}
	for _, c := range []string{"transit", "place"} {
		if cats[c] {
			pointCats = append(pointCats, c)
		}
	}
	if len(pointCats) > 0 && !truncated {
		ph := strings.Repeat("?,", len(pointCats))
		ph = ph[:len(ph)-1]
		args := []interface{}{west, east, south, north}
		for _, c := range pointCats {
			args = append(args, c)
		}
		rows, err := osmDB.Query(`
			SELECT p.cat, p.station, p.fclass, p.name, p.pop, p.lon, p.lat
			FROM osm_points_rtree rt JOIN osm_points p ON p.id = rt.id
			WHERE rt.max_lon >= ? AND rt.min_lon <= ? AND rt.max_lat >= ? AND rt.min_lat <= ?
			  AND p.cat IN (`+ph+`)`, args...)
		if err == nil {
			for rows.Next() {
				if len(features) >= limit {
					truncated = true
					break
				}
				var cat string
				var station int
				var fclass, name sql.NullString
				var pop sql.NullInt64
				var lon, lat float64
				if err := rows.Scan(&cat, &station, &fclass, &name, &pop, &lon, &lat); err != nil {
					continue
				}
				props := map[string]interface{}{"cat": cat, "fclass": fclass.String}
				if name.String != "" {
					props["name"] = name.String
				}
				if cat == "transit" {
					props["train_station"] = station == 1
				}
				if pop.Valid && pop.Int64 > 0 {
					props["population"] = pop.Int64
				}
				features = append(features, map[string]interface{}{
					"type":       "Feature",
					"geometry":   map[string]interface{}{"type": "Point", "coordinates": [2]float64{roundCoord(lon), roundCoord(lat)}},
					"properties": props,
				})
			}
			rows.Close()
		}
	}

	w.Header().Set("Content-Type", "application/geo+json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"type":     "FeatureCollection",
		"features": features,
		"meta": map[string]interface{}{
			"scope":     scope,
			"bbox":      [4]float64{west, south, east, north},
			"count":     len(features),
			"truncated": truncated,
			"note":      "lines are served as indexed chunks (≤24 vertices); consecutive chunks of the same way share endpoints. Use cat=water_area for fillable river/lake (Multi)Polygons clipped to the bbox (clip=0 for whole polygons); cat=water is the waterway centreline + polygon outline chunks used for distances",
			"areas":     areasNote,
			"took_ms":   time.Since(start).Milliseconds(),
			"license":   OSMAttributionMap(),
		},
	})
}

func roundCoord(v float64) float64 { return math.Round(v*1e6) / 1e6 }

func handleV1OSMPoint(w http.ResponseWriter, r *http.Request) {
	if !OSMProximityReady() {
		apiError(w, "OSM proximity data not imported (run -osm-import)", 503)
		return
	}
	lon, err1 := strconv.ParseFloat(r.URL.Query().Get("lon"), 64)
	lat, err2 := strconv.ParseFloat(r.URL.Query().Get("lat"), 64)
	if err1 != nil || err2 != nil {
		apiError(w, "lon and lat query params required", 400)
		return
	}
	start := time.Now()
	// ?area_sqm= lets a client that holds its own cadastre (bevdirect) get the
	// same area-aware metrics as for a unit of that size.
	area, _ := strconv.ParseFloat(r.URL.Query().Get("area_sqm"), 64)
	m := computeOSMMetrics(lon, lat, area)
	if m == nil {
		apiError(w, "metrics computation failed", 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"lon": lon, "lat": lat, "area_sqm": area,
		"osm": osmMetricsToMap(m),
		"meta": map[string]interface{}{
			"took_ms": time.Since(start).Milliseconds(),
			"license": OSMAttributionMap(),
		},
	})
}

// osmStatsTTL — these are counters over a 1.95M-line DB. Measured 4.4 s average, 14 s max with no caching at all; served stale
// via StatsMemo (stats_memo.go) they cost nothing.
const osmStatsTTL = 5 * time.Minute

func handleV1OSMStats(w http.ResponseWriter, r *http.Request) {
	out := StatsMemo("osm_stats", osmStatsTTL,
		r.URL.Query().Get("refresh") == "1", computeOSMStats)
	// Readiness is cheap and must never be stale.
	out["ready"] = OSMProximityReady()
	writeJSON(w, http.StatusOK, out)
}

func computeOSMStats() map[string]interface{} {
	out := map[string]interface{}{}
	if osmDB == nil {
		return out
	}
	var lines, points int64
	osmDB.QueryRow(`SELECT count(*) FROM osm_lines`).Scan(&lines)
	osmDB.QueryRow(`SELECT count(*) FROM osm_points`).Scan(&points)
	out["line_chunks"] = lines
	out["points"] = points
	meta := map[string]string{}
	rows, err := osmDB.Query(`SELECT key, value FROM osm_meta`)
	if err == nil {
		for rows.Next() {
			var k, v string
			if rows.Scan(&k, &v) == nil {
				meta[k] = v
			}
		}
		rows.Close()
	}
	out["meta"] = meta
	byCat := map[string]int64{}
	crows, err := osmDB.Query(`SELECT cat, count(*) FROM osm_lines GROUP BY cat`)
	if err == nil {
		for crows.Next() {
			var c string
			var n int64
			if crows.Scan(&c, &n) == nil {
				byCat["lines_"+c] = n
			}
		}
		crows.Close()
	}
	prows, err := osmDB.Query(`SELECT cat, count(*) FROM osm_points GROUP BY cat`)
	if err == nil {
		for prows.Next() {
			var c string
			var n int64
			if prows.Scan(&c, &n) == nil {
				byCat["points_"+c] = n
			}
		}
		prows.Close()
	}
	out["by_category"] = byCat
	return out
}
