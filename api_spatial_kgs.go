// api_spatial_kgs.go — Dedicated endpoint: find KG codes within a bounding box or polygon.
//
// GET  /api/v1/spatial/kgs?west=…&south=…&east=…&north=…
// POST /api/v1/spatial/kgs  (GeoJSON Polygon/Feature body)
//
// Returns a lightweight list of KG codes + names with optional admin hierarchy.
// Much faster than /spatial/bbox?layers=kg when you only need to know which KGs
// intersect a region.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
)

var handleV1SpatialKGs http.HandlerFunc = notImplemented("spatial/kgs")

func init() {
	handleV1SpatialKGs = spatialKGsHandler
}

// spatialKGsHandler returns KG codes intersecting a bbox or polygon.
func spatialKGsHandler(w http.ResponseWriter, r *http.Request) {
	if globalSearchIndex == nil {
		apiError(w, "search index not initialized", 503)
		return
	}

	ac := ParseAPIContext(r)

	var polyCoords [][]float64 // optional polygon for refinement
	var bb bboxQuery
	var hasBbox bool

	if r.Method == http.MethodPost {
		// POST: parse GeoJSON polygon from body
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			apiError(w, "invalid JSON body: "+err.Error(), 400)
			return
		}
		polyCoords = extractPolygonCoords(body)
		if len(polyCoords) < 3 {
			apiError(w, "polygon with at least 3 coordinate pairs required (GeoJSON Polygon or Feature)", 400)
			return
		}
		// Compute bbox from polygon
		west, south := 180.0, 90.0
		east, north := -180.0, -90.0
		for _, c := range polyCoords {
			if len(c) >= 2 {
				if c[0] < west {
					west = c[0]
				}
				if c[0] > east {
					east = c[0]
				}
				if c[1] < south {
					south = c[1]
				}
				if c[1] > north {
					north = c[1]
				}
			}
		}
		bb = bboxQuery{west, south, east, north}
		hasBbox = true
	} else {
		// GET: parse bbox from query params
		q := r.URL.Query()
		west := floatQ(q, "west", floatQ(q, "minlon", 0))
		south := floatQ(q, "south", floatQ(q, "minlat", 0))
		east := floatQ(q, "east", floatQ(q, "maxlon", 0))
		north := floatQ(q, "north", floatQ(q, "maxlat", 0))

		if west == 0 && east == 0 {
			apiError(w, "bbox required: west,south,east,north (or minlon,minlat,maxlon,maxlat); or POST with GeoJSON polygon", 400)
			return
		}

		// Buffer
		if buf := floatQ(q, "buffer", 0); buf > 0 {
			west, south, east, north = bufferBbox(west, south, east, north, buf)
		}

		// Optional polygon from ?geojson= param
		if gj := q.Get("geojson"); gj != "" {
			var body map[string]interface{}
			if err := json.Unmarshal([]byte(gj), &body); err == nil {
				polyCoords = extractPolygonCoords(body)
			}
		}

		bb = bboxQuery{west, south, east, north}
		hasBbox = true
	}

	if !hasBbox {
		apiError(w, "no spatial filter provided", 400)
		return
	}

	// Query KGs whose bbox intersects the request bbox
	db := globalSearchIndex.DB()
	rows, err := db.Query(`SELECT kg_code, kg_name, gemeinde_code, gemeinde_name,
		district_code, district_name, state_name,
		parcel_count, building_count, total_area_sqm,
		min_lon, min_lat, max_lon, max_lat
		FROM kg
		WHERE min_lon <= ? AND max_lon >= ? AND min_lat <= ? AND max_lat >= ?
		ORDER BY kg_code`,
		bb.East, bb.West, bb.North, bb.South)
	if err != nil {
		apiError(w, "query error: "+err.Error(), 500)
		return
	}
	defer rows.Close()

	type kgEntry struct {
		Code         string
		Name         string
		GemeindeCode string
		GemeindeName string
		DistrictCode string
		DistrictName string
		StateName    string
		ParcelCount  int
		BuildingCnt  int
		TotalArea    float64
		MinLon       float64
		MinLat       float64
		MaxLon       float64
		MaxLat       float64
	}

	var allKGs []kgEntry
	for rows.Next() {
		var e kgEntry
		rows.Scan(&e.Code, &e.Name, &e.GemeindeCode, &e.GemeindeName,
			&e.DistrictCode, &e.DistrictName, &e.StateName,
			&e.ParcelCount, &e.BuildingCnt, &e.TotalArea,
			&e.MinLon, &e.MinLat, &e.MaxLon, &e.MaxLat)
		allKGs = append(allKGs, e)
	}

	// If a polygon is provided, refine: keep only KGs whose bbox actually
	// overlaps the polygon (not just the polygon's bbox).
	if len(polyCoords) >= 3 {
		var filtered []kgEntry
		for _, kg := range allKGs {
			if bboxOverlapsPolygon(kg.MinLon, kg.MinLat, kg.MaxLon, kg.MaxLat, polyCoords) {
				filtered = append(filtered, kg)
			}
		}
		allKGs = filtered
	}

	// Apply pagination
	total := len(allKGs)
	start := ac.Offset
	if start > total {
		start = total
	}
	end := start + ac.Limit
	if end > total {
		end = total
	}
	paged := allKGs[start:end]

	// Build response
	var kgCodes []string
	var kgList []map[string]interface{}
	for _, kg := range paged {
		kgCodes = append(kgCodes, kg.Code)
		kgList = append(kgList, ac.FilterFields(map[string]interface{}{
			"kg_code":        kg.Code,
			"kg_name":        kg.Name,
			"gemeinde_code":  kg.GemeindeCode,
			"gemeinde_name":  kg.GemeindeName,
			"district_code":  kg.DistrictCode,
			"district_name":  kg.DistrictName,
			"state_name":     kg.StateName,
			"parcel_count":   kg.ParcelCount,
			"building_count": kg.BuildingCnt,
			"total_area_sqm": kg.TotalArea,
			"bbox": map[string]float64{
				"min_lon": kg.MinLon, "min_lat": kg.MinLat,
				"max_lon": kg.MaxLon, "max_lat": kg.MaxLat,
			},
		}))
	}

	sort.Strings(kgCodes)

	response := map[string]interface{}{
		"kg_codes": kgCodes,
		"kgs":      kgList,
		"bbox":     map[string]float64{"west": bb.West, "south": bb.South, "east": bb.East, "north": bb.North},
	}

	meta := NewMeta(ac, total)

	switch ac.Format {
	case "geojson":
		var features []map[string]interface{}
		for _, kg := range paged {
			cx := (kg.MinLon + kg.MaxLon) / 2
			cy := (kg.MinLat + kg.MaxLat) / 2
			features = append(features, GeoJSONPointFeature(cx, cy, map[string]interface{}{
				"kg_code":        kg.Code,
				"kg_name":        kg.Name,
				"gemeinde_code":  kg.GemeindeCode,
				"gemeinde_name":  kg.GemeindeName,
				"district_code":  kg.DistrictCode,
				"district_name":  kg.DistrictName,
				"state_name":     kg.StateName,
				"parcel_count":   kg.ParcelCount,
				"building_count": kg.BuildingCnt,
				"total_area_sqm": kg.TotalArea,
			}))
		}
		apiGeoJSONResponse(w, r, features, meta)
	case "csv":
		headers := []string{"kg_code", "kg_name", "gemeinde_code", "gemeinde_name",
			"district_code", "district_name", "state_name",
			"parcel_count", "building_count", "total_area_sqm"}
		var csvRows [][]string
		for _, kg := range paged {
			csvRows = append(csvRows, []string{
				kg.Code, kg.Name, kg.GemeindeCode, kg.GemeindeName,
				kg.DistrictCode, kg.DistrictName, kg.StateName,
				fmt.Sprintf("%d", kg.ParcelCount),
				fmt.Sprintf("%d", kg.BuildingCnt),
				fmt.Sprintf("%.1f", kg.TotalArea),
			})
		}
		apiCSVResponse(w, r, headers, csvRows, "kgs_in_region.csv")
	default:
		apiResponse(w, r, response, meta)
	}
}

// ═══════════════════════════════════════════════════════════════════
// Geometry: bbox-polygon overlap test
// ═══════════════════════════════════════════════════════════════════

// bboxOverlapsPolygon returns true if the axis-aligned bounding box
// [minX,minY]–[maxX,maxY] overlaps the given polygon.
//
// Three conditions (any → true):
//  1. Any polygon vertex is inside the bbox
//  2. Any bbox corner is inside the polygon
//  3. Any polygon edge intersects any bbox edge
func bboxOverlapsPolygon(minX, minY, maxX, maxY float64, poly [][]float64) bool {
	// 1. Any polygon vertex inside the bbox?
	for _, c := range poly {
		if len(c) >= 2 && c[0] >= minX && c[0] <= maxX && c[1] >= minY && c[1] <= maxY {
			return true
		}
	}

	// 2. Any bbox corner inside the polygon?
	corners := [4][2]float64{
		{minX, minY}, {maxX, minY}, {maxX, maxY}, {minX, maxY},
	}
	for _, c := range corners {
		if pointInPolygon(c[0], c[1], poly) {
			return true
		}
	}

	// 3. Any polygon edge crosses any bbox edge?
	// The four bbox edges:
	bboxEdges := [4][4]float64{
		{minX, minY, maxX, minY}, // bottom
		{maxX, minY, maxX, maxY}, // right
		{maxX, maxY, minX, maxY}, // top
		{minX, maxY, minX, minY}, // left
	}
	n := len(poly)
	for i := 0; i < n; i++ {
		j := (i + 1) % n
		if len(poly[i]) < 2 || len(poly[j]) < 2 {
			continue
		}
		px1, py1 := poly[i][0], poly[i][1]
		px2, py2 := poly[j][0], poly[j][1]
		for _, be := range bboxEdges {
			if segmentsIntersect(px1, py1, px2, py2, be[0], be[1], be[2], be[3]) {
				return true
			}
		}
	}

	return false
}

// segmentsIntersect returns true if line segment (ax1,ay1)-(ax2,ay2)
// intersects segment (bx1,by1)-(bx2,by2).
func segmentsIntersect(ax1, ay1, ax2, ay2, bx1, by1, bx2, by2 float64) bool {
	d1 := cross(bx1, by1, bx2, by2, ax1, ay1)
	d2 := cross(bx1, by1, bx2, by2, ax2, ay2)
	d3 := cross(ax1, ay1, ax2, ay2, bx1, by1)
	d4 := cross(ax1, ay1, ax2, ay2, bx2, by2)

	if ((d1 > 0 && d2 < 0) || (d1 < 0 && d2 > 0)) &&
		((d3 > 0 && d4 < 0) || (d3 < 0 && d4 > 0)) {
		return true
	}

	// Collinear cases
	if d1 == 0 && onSegment(bx1, by1, bx2, by2, ax1, ay1) {
		return true
	}
	if d2 == 0 && onSegment(bx1, by1, bx2, by2, ax2, ay2) {
		return true
	}
	if d3 == 0 && onSegment(ax1, ay1, ax2, ay2, bx1, by1) {
		return true
	}
	if d4 == 0 && onSegment(ax1, ay1, ax2, ay2, bx2, by2) {
		return true
	}
	return false
}

// cross returns the cross product of vectors (x2-x1,y2-y1) and (px-x1,py-y1).
func cross(x1, y1, x2, y2, px, py float64) float64 {
	return (x2-x1)*(py-y1) - (y2-y1)*(px-x1)
}

// onSegment checks if point (px,py) lies on segment (x1,y1)-(x2,y2),
// assuming they are collinear.
func onSegment(x1, y1, x2, y2, px, py float64) bool {
	return min64(x1, x2) <= px && px <= max64(x1, x2) &&
		min64(y1, y2) <= py && py <= max64(y1, y2)
}

func min64(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
