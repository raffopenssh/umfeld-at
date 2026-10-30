// api_search_admin.go — search over the in-memory admin datasets.
//
//   - /api/v1/search/protected_area  WDPA protected sites (name search, point-in-polygon)
//   - /api/v1/search/gadm            GADM municipalities (name search, point-in-polygon)
//
// Shared helpers: GeoJSON (Multi)Polygon parsing, centroid, point-in-polygon,
// and the generic paginated search response writers used by the admin
// search handlers (api_search_municipalities.go).
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"strings"
)

// ═══════════════════════════════════════════════════════════════════════════════
// In-memory GeoJSON datasets loaded at init time
// ═══════════════════════════════════════════════════════════════════════════════

// gadmFeature holds a parsed GADM municipality feature with pre-extracted
// properties and polygon rings for point-in-polygon queries.
type gadmFeature struct {
	GID3      string
	Name1     string // state
	Name2     string // district
	Name3     string // municipality
	Type3     string
	CenterLon float64
	CenterLat float64
	Polygons  [][][]float64 // each polygon is a ring of [lon,lat] pairs
	Raw       map[string]interface{}
}

// protectedAreaFeature holds a parsed WDPA protected area feature.
type protectedAreaFeature struct {
	Name      string
	Desig     string
	DesigEng  string
	DesigType string
	IUCNCat   string
	RepArea   float64
	GISArea   float64
	Realm     string
	CenterLon float64
	CenterLat float64
	Polygons  [][][]float64
	Raw       map[string]interface{}
}

var (
	gadmFeatures          []gadmFeature
	protectedAreaFeatures []protectedAreaFeature
)

// loadGADMData loads the GADM municipality GeoJSON into memory.
func loadGADMData() {
	data, err := os.ReadFile("data/gadm41_AUT_3.json")
	if err != nil {
		log.Printf("⚠ GADM data not loaded: %v", err)
		return
	}
	var fc struct {
		Features []struct {
			Properties map[string]interface{} `json:"properties"`
			Geometry   struct {
				Type        string          `json:"type"`
				Coordinates json.RawMessage `json:"coordinates"`
			} `json:"geometry"`
		} `json:"features"`
	}
	if err := json.Unmarshal(data, &fc); err != nil {
		log.Printf("⚠ GADM parse error: %v", err)
		return
	}
	for _, f := range fc.Features {
		polygons := parseMultiPolygonCoords(f.Geometry.Type, f.Geometry.Coordinates)
		clon, clat := polygonsCentroid(polygons)
		gf := gadmFeature{
			GID3:      strProp(f.Properties, "GID_3"),
			Name1:     strProp(f.Properties, "NAME_1"),
			Name2:     strProp(f.Properties, "NAME_2"),
			Name3:     strProp(f.Properties, "NAME_3"),
			Type3:     strProp(f.Properties, "TYPE_3"),
			CenterLon: clon,
			CenterLat: clat,
			Polygons:  polygons,
			Raw:       f.Properties,
		}
		gadmFeatures = append(gadmFeatures, gf)
	}
	log.Printf("🗺  GADM: loaded %d municipality features", len(gadmFeatures))
}

// loadProtectedAreaData loads the WDPA protected areas GeoJSON into memory.
func loadProtectedAreaData() {
	data, err := os.ReadFile("data/nationalparks_wdpa.geojson")
	if err != nil {
		log.Printf("⚠ Protected area data not loaded: %v", err)
		return
	}
	var fc struct {
		Features []struct {
			Properties map[string]interface{} `json:"properties"`
			Geometry   struct {
				Type        string          `json:"type"`
				Coordinates json.RawMessage `json:"coordinates"`
			} `json:"geometry"`
		} `json:"features"`
	}
	if err := json.Unmarshal(data, &fc); err != nil {
		log.Printf("⚠ Protected area parse error: %v", err)
		return
	}
	for _, f := range fc.Features {
		polygons := parseMultiPolygonCoords(f.Geometry.Type, f.Geometry.Coordinates)
		clon, clat := polygonsCentroid(polygons)
		pf := protectedAreaFeature{
			Name:      strProp(f.Properties, "name"),
			Desig:     strProp(f.Properties, "desig"),
			DesigEng:  strProp(f.Properties, "desig_eng"),
			DesigType: strProp(f.Properties, "desig_type"),
			IUCNCat:   strProp(f.Properties, "iucn_cat"),
			RepArea:   floatProp(f.Properties, "rep_area"),
			GISArea:   floatProp(f.Properties, "gis_area"),
			Realm:     strProp(f.Properties, "realm"),
			CenterLon: clon,
			CenterLat: clat,
			Polygons:  polygons,
			Raw:       f.Properties,
		}
		protectedAreaFeatures = append(protectedAreaFeatures, pf)
	}
	log.Printf("🛡  Protected areas: loaded %d features", len(protectedAreaFeatures))
}

// parseMultiPolygonCoords parses GeoJSON Polygon or MultiPolygon coordinates
// into a flat list of polygon rings (outer rings only).
func parseMultiPolygonCoords(geomType string, raw json.RawMessage) [][][]float64 {
	var polygons [][][]float64
	switch geomType {
	case "Polygon":
		// [[[lon,lat], ...], ...] — array of rings
		var rings [][][]float64
		if err := json.Unmarshal(raw, &rings); err == nil && len(rings) > 0 {
			polygons = append(polygons, rings[0]) // outer ring only
		}
	case "MultiPolygon":
		// [[[[lon,lat], ...], ...], ...] — array of polygons, each with rings
		var multi [][][][]float64
		if err := json.Unmarshal(raw, &multi); err == nil {
			for _, poly := range multi {
				if len(poly) > 0 {
					polygons = append(polygons, poly[0]) // outer ring of each polygon
				}
			}
		}
	}
	return polygons
}

// polygonsCentroid computes a simple average centroid of all polygon vertices.
func polygonsCentroid(polygons [][][]float64) (float64, float64) {
	var sumLon, sumLat float64
	var n int
	for _, ring := range polygons {
		for _, pt := range ring {
			if len(pt) >= 2 {
				sumLon += pt[0]
				sumLat += pt[1]
				n++
			}
		}
	}
	if n == 0 {
		return 0, 0
	}
	return sumLon / float64(n), sumLat / float64(n)
}

// pointInAnyPolygon checks if a point is inside any polygon in the list.
func pointInAnyPolygon(lon, lat float64, polygons [][][]float64) bool {
	for _, ring := range polygons {
		if pointInPolygon(lon, lat, ring) {
			return true
		}
	}
	return false
}

// strProp extracts a string property from a map.
func strProp(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
		if v != nil {
			return fmt.Sprintf("%v", v)
		}
	}
	return ""
}

// floatProp extracts a float64 property from a map.
func floatProp(m map[string]interface{}, key string) float64 {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case float64:
			return n
		case int:
			return float64(n)
		}
	}
	return 0
}

// ═══════════════════════════════════════════════════════════════════════════════
// init — assign all 10 handler vars and load GeoJSON data
// ═══════════════════════════════════════════════════════════════════════════════

func init() {
	// Load in-memory GeoJSON datasets
	loadGADMData()
	loadProtectedAreaData()

	handleV1SearchProtectedArea = v1SearchProtectedAreaHandler
	handleV1SearchGADM = v1SearchGADMHandler
}

// ═══════════════════════════════════════════════════════════════════════════════
// Shared helpers for format dispatch and CSV conversion
// ═══════════════════════════════════════════════════════════════════════════════

// respondSearch dispatches results in the negotiated format.
func respondSearch(w http.ResponseWriter, r *http.Request, ac *APIContext, results []map[string]interface{}, total int, csvName string) {
	respondSearchMeta(w, r, ac, results, total, csvName, nil)
}

// respondSearchMeta is respondSearch with a hook to decorate the meta block
// (used to report background-warming gaps per the Latency & Availability
// Contract: serve what you have, queue the rest, and say so).
func respondSearchMeta(w http.ResponseWriter, r *http.Request, ac *APIContext, results []map[string]interface{}, total int, csvName string, decorate func(*Meta)) {
	if results == nil {
		results = []map[string]interface{}{}
	}
	meta := NewMeta(ac, total)
	if decorate != nil {
		decorate(&meta)
	}
	switch ac.Format {
	case "geojson":
		var features []map[string]interface{}
		for _, res := range results {
			lon, _ := res["lon"].(float64)
			lat, _ := res["lat"].(float64)
			props := copyMap(res)
			delete(props, "lon")
			delete(props, "lat")
			features = append(features, GeoJSONPointFeature(lon, lat, props))
		}
		apiGeoJSONResponse(w, r, features, meta)
	case "csv":
		headers, rows := searchResultsToCSV(results)
		apiCSVResponse(w, r, headers, rows, csvName)
	default:
		apiResponse(w, r, results, meta)
	}
}

// respondSearchWithGeometry dispatches results that already have full GeoJSON
// geometry (not just point coordinates).
func respondSearchWithGeometry(w http.ResponseWriter, r *http.Request, ac *APIContext, results []map[string]interface{}, features []map[string]interface{}, total int, csvName string) {
	meta := NewMeta(ac, total)
	switch ac.Format {
	case "geojson":
		apiGeoJSONResponse(w, r, features, meta)
	case "csv":
		headers, rows := searchResultsToCSV(results)
		apiCSVResponse(w, r, headers, rows, csvName)
	default:
		apiResponse(w, r, results, meta)
	}
}

// searchResultsToCSV converts generic result maps to CSV headers+rows by
// collecting all unique keys from the first result.
func searchResultsToCSV(results []map[string]interface{}) ([]string, [][]string) {
	if len(results) == 0 {
		return nil, nil
	}
	// Collect headers from first result in stable order
	var headers []string
	seen := map[string]bool{}
	for _, res := range results {
		for k := range res {
			if !seen[k] {
				headers = append(headers, k)
				seen[k] = true
			}
		}
	}
	var rows [][]string
	for _, res := range results {
		row := make([]string, len(headers))
		for i, h := range headers {
			row[i] = str(res[h])
		}
		rows = append(rows, row)
	}
	return headers, rows
}

// paginateResults applies limit/offset to an in-memory slice.
func paginateResults(results []map[string]interface{}, ac *APIContext) []map[string]interface{} {
	if ac.Offset >= len(results) {
		return nil
	}
	end := ac.Offset + ac.Limit
	if end > len(results) {
		end = len(results)
	}
	return results[ac.Offset:end]
}

// ═══════════════════════════════════════════════════════════════════════════════
// Handler: /api/v1/search/protected_area
// Search WDPA protected areas by name, point proximity, or point-in-polygon.
// ═══════════════════════════════════════════════════════════════════════════════

func v1SearchProtectedAreaHandler(w http.ResponseWriter, r *http.Request) {
	ac := ParseAPIContext(r)
	qp := r.URL.Query()
	q := strings.ToLower(qp.Get("q"))
	nearLon := floatParam(r, "near_lon", 0)
	nearLat := floatParam(r, "near_lat", 0)
	containsLon := floatParam(r, "contains_lon", 0)
	containsLat := floatParam(r, "contains_lat", 0)

	var allResults []map[string]interface{}
	var allFeatures []map[string]interface{} // GeoJSON features with full polygon geometry

	for i := range protectedAreaFeatures {
		pf := &protectedAreaFeatures[i]

		// Text filter
		if q != "" {
			nameLower := strings.ToLower(pf.Name)
			desigLower := strings.ToLower(pf.Desig)
			desigEngLower := strings.ToLower(pf.DesigEng)
			if !strings.Contains(nameLower, q) && !strings.Contains(desigLower, q) && !strings.Contains(desigEngLower, q) {
				continue
			}
		}

		// Contains point filter
		if containsLon != 0 || containsLat != 0 {
			if !pointInAnyPolygon(containsLon, containsLat, pf.Polygons) {
				continue
			}
		}

		result := ac.FilterFields(map[string]interface{}{
			"name":       pf.Name,
			"desig":      pf.Desig,
			"desig_eng":  pf.DesigEng,
			"desig_type": pf.DesigType,
			"iucn_cat":   pf.IUCNCat,
			"rep_area":   pf.RepArea,
			"gis_area":   pf.GISArea,
			"realm":      pf.Realm,
			"lon":        pf.CenterLon,
			"lat":        pf.CenterLat,
		})

		// Add distance if near query
		if nearLon != 0 || nearLat != 0 {
			dist := haversine(nearLat, nearLon, pf.CenterLat, pf.CenterLon)
			result["distance_m"] = math.Round(dist*100) / 100
		}

		allResults = append(allResults, result)

		// Build GeoJSON feature with full polygon geometry
		props := copyMap(result)
		delete(props, "lon")
		delete(props, "lat")
		geom := protectedAreaGeometry(pf)
		allFeatures = append(allFeatures, map[string]interface{}{
			"type":       "Feature",
			"geometry":   geom,
			"properties": props,
		})
	}

	// Sort by distance if near query
	if nearLon != 0 || nearLat != 0 {
		sortByDistance(allResults, allFeatures)
	}

	total := len(allResults)
	results := paginateResults(allResults, ac)

	// Paginate features in parallel
	var features []map[string]interface{}
	if ac.Offset < len(allFeatures) {
		end := ac.Offset + ac.Limit
		if end > len(allFeatures) {
			end = len(allFeatures)
		}
		features = allFeatures[ac.Offset:end]
	}

	respondSearchWithGeometry(w, r, ac, results, features, total, "search_protected_area.csv")
}

// protectedAreaGeometry builds a GeoJSON MultiPolygon geometry from the feature's polygons.
func protectedAreaGeometry(pf *protectedAreaFeature) map[string]interface{} {
	if len(pf.Polygons) == 0 {
		return nil
	}
	if len(pf.Polygons) == 1 {
		return map[string]interface{}{
			"type":        "Polygon",
			"coordinates": []interface{}{pf.Polygons[0]},
		}
	}
	var coords []interface{}
	for _, ring := range pf.Polygons {
		coords = append(coords, []interface{}{ring})
	}
	return map[string]interface{}{
		"type":        "MultiPolygon",
		"coordinates": coords,
	}
}

// gadmGeometry builds a GeoJSON MultiPolygon geometry from the feature's polygons.
func gadmGeometry(gf *gadmFeature) map[string]interface{} {
	if len(gf.Polygons) == 0 {
		return nil
	}
	if len(gf.Polygons) == 1 {
		return map[string]interface{}{
			"type":        "Polygon",
			"coordinates": []interface{}{gf.Polygons[0]},
		}
	}
	var coords []interface{}
	for _, ring := range gf.Polygons {
		coords = append(coords, []interface{}{ring})
	}
	return map[string]interface{}{
		"type":        "MultiPolygon",
		"coordinates": coords,
	}
}

// sortByDistance sorts results and features in-place by distance_m ascending.
func sortByDistance(results []map[string]interface{}, features []map[string]interface{}) {
	// Simple insertion sort (datasets are small)
	n := len(results)
	for i := 1; i < n; i++ {
		for j := i; j > 0; j-- {
			di, _ := results[j]["distance_m"].(float64)
			dj, _ := results[j-1]["distance_m"].(float64)
			if di < dj {
				results[j], results[j-1] = results[j-1], results[j]
				if j < len(features) && j-1 < len(features) {
					features[j], features[j-1] = features[j-1], features[j]
				}
			} else {
				break
			}
		}
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// Handler: /api/v1/search/gadm
// Search GADM municipality features by name, GID, or point-in-polygon.
// ═══════════════════════════════════════════════════════════════════════════════

func v1SearchGADMHandler(w http.ResponseWriter, r *http.Request) {
	ac := ParseAPIContext(r)
	qp := r.URL.Query()
	q := strings.ToLower(qp.Get("q"))
	gid := qp.Get("gid")
	containsLon := floatParam(r, "contains_lon", 0)
	containsLat := floatParam(r, "contains_lat", 0)

	if q == "" && gid == "" && containsLon == 0 && containsLat == 0 {
		apiError(w, "at least one of q, gid, contains_lon+contains_lat required", 400)
		return
	}

	var allResults []map[string]interface{}
	var allFeatures []map[string]interface{}

	for i := range gadmFeatures {
		gf := &gadmFeatures[i]

		// GID exact match
		if gid != "" && gf.GID3 != gid {
			continue
		}

		// Text filter
		if q != "" {
			name3Lower := strings.ToLower(gf.Name3)
			name2Lower := strings.ToLower(gf.Name2)
			name1Lower := strings.ToLower(gf.Name1)
			if !strings.Contains(name3Lower, q) && !strings.Contains(name2Lower, q) && !strings.Contains(name1Lower, q) {
				continue
			}
		}

		// Contains point filter
		if containsLon != 0 || containsLat != 0 {
			if !pointInAnyPolygon(containsLon, containsLat, gf.Polygons) {
				continue
			}
		}

		result := ac.FilterFields(map[string]interface{}{
			"gid_3":  gf.GID3,
			"name_1": gf.Name1,
			"name_2": gf.Name2,
			"name_3": gf.Name3,
			"type_3": gf.Type3,
			"lon":    gf.CenterLon,
			"lat":    gf.CenterLat,
		})
		allResults = append(allResults, result)

		// GeoJSON feature with full polygon geometry
		props := copyMap(result)
		delete(props, "lon")
		delete(props, "lat")
		geom := gadmGeometry(gf)
		allFeatures = append(allFeatures, map[string]interface{}{
			"type":       "Feature",
			"geometry":   geom,
			"properties": props,
		})
	}

	total := len(allResults)
	results := paginateResults(allResults, ac)

	// Paginate features in parallel
	var features []map[string]interface{}
	if ac.Offset < len(allFeatures) {
		end := ac.Offset + ac.Limit
		if end > len(allFeatures) {
			end = len(allFeatures)
		}
		features = allFeatures[ac.Offset:end]
	}

	respondSearchWithGeometry(w, r, ac, results, features, total, "search_gadm.csv")
}
