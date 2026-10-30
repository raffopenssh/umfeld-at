// api_search_municipalities.go — Municipality search using Statistik Austria data
//
// GET /api/v1/search/municipalities — Search Austrian municipalities with proper names
// and official Gemeindekennziffer. Source: Statistik Austria, 2114 municipalities.
//
// Unlike GADM (which has 587 names with missing spaces like "BreitenbrunnamNeusiedlerSee"),
// Statistik Austria provides correct names ("Breitenbrunn am Neusiedler See").
package main

import (
	"compress/gzip"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
)

func init() {
	loadStatAustriaMunicipalities()
	handleV1SearchMunicipalities = v1SearchMunicipalitiesHandler
}

// ═══════════════════════════════════════════════════════════════════
// Data loading
// ═══════════════════════════════════════════════════════════════════

type statAustriaMunicipality struct {
	GemeindeCode string // g_id — 5-digit Gemeindekennziffer
	Name         string // g_name — official municipality name
	CenterLon    float64
	CenterLat    float64
	Polygons     [][][]float64
	Raw          map[string]interface{}
}

var statAustriaMunicipalities []statAustriaMunicipality

func loadStatAustriaMunicipalities() {
	path := "data/statistik_austria_gem.geojson.gz"
	f, err := os.Open(path)
	if err != nil {
		log.Printf("⚠ Statistik Austria municipalities not loaded: %v", err)
		return
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		log.Printf("⚠ Statistik Austria municipalities gzip error: %v", err)
		return
	}
	defer gr.Close()

	var fc struct {
		Features []struct {
			Properties map[string]interface{} `json:"properties"`
			Geometry   struct {
				Type        string          `json:"type"`
				Coordinates json.RawMessage `json:"coordinates"`
			} `json:"geometry"`
		} `json:"features"`
	}
	if err := json.NewDecoder(gr).Decode(&fc); err != nil {
		log.Printf("⚠ Statistik Austria municipalities parse error: %v", err)
		return
	}

	for _, feat := range fc.Features {
		polygons := parseMultiPolygonCoords(feat.Geometry.Type, feat.Geometry.Coordinates)
		clon, clat := polygonsCentroid(polygons)
		statAustriaMunicipalities = append(statAustriaMunicipalities, statAustriaMunicipality{
			GemeindeCode: strProp(feat.Properties, "g_id"),
			Name:         strProp(feat.Properties, "g_name"),
			CenterLon:    clon,
			CenterLat:    clat,
			Polygons:     polygons,
			Raw:          feat.Properties,
		})
	}
	log.Printf("🏠 Statistik Austria: loaded %d municipality features", len(statAustriaMunicipalities))
}

// Derive district and state from 5-digit Gemeindekennziffer.
func gemeindeCodeParts(code string) (district, state string) {
	if len(code) < 3 {
		return "", ""
	}
	district = code[:3]
	switch code[:1] {
	case "1":
		state = "Burgenland"
	case "2":
		state = "Kärnten"
	case "3":
		state = "Niederösterreich"
	case "4":
		state = "Oberösterreich"
	case "5":
		state = "Salzburg"
	case "6":
		state = "Steiermark"
	case "7":
		state = "Tirol"
	case "8":
		state = "Vorarlberg"
	case "9":
		state = "Wien"
	}
	return
}

// District name lookup from search index.
func districtName(districtCode string) string {
	if globalSearchIndex == nil {
		return ""
	}
	db := globalSearchIndex.DB()
	var name string
	err := db.QueryRow(`SELECT DISTINCT district_name FROM kg WHERE district_code = ? LIMIT 1`, districtCode).Scan(&name)
	if err == nil {
		return name
	}
	return ""
}

// ═══════════════════════════════════════════════════════════════════
// Handler
// ═══════════════════════════════════════════════════════════════════

func v1SearchMunicipalitiesHandler(w http.ResponseWriter, r *http.Request) {
	ac := ParseAPIContext(r)
	qp := r.URL.Query()
	q := strings.ToLower(qp.Get("q"))
	gid := qp.Get("id") // Gemeindekennziffer
	state := strings.ToLower(qp.Get("state"))
	district := qp.Get("district") // 3-digit district code
	containsLon := floatParam(r, "contains_lon", 0)
	containsLat := floatParam(r, "contains_lat", 0)
	listAll := qp.Get("list") == "all"

	if len(statAustriaMunicipalities) == 0 {
		apiError(w, "Statistik Austria municipality data not loaded", 503)
		return
	}

	// Require at least one filter (unless list=all)
	if !listAll && q == "" && gid == "" && state == "" && district == "" && containsLon == 0 && containsLat == 0 {
		apiError(w, "at least one of q, id, state, district, contains_lon+contains_lat required (or list=all)", 400)
		return
	}

	var allResults []map[string]interface{}
	var allFeatures []map[string]interface{}

	for i := range statAustriaMunicipalities {
		m := &statAustriaMunicipalities[i]

		// ID exact match
		if gid != "" && m.GemeindeCode != gid {
			continue
		}

		// State filter
		if state != "" {
			_, mState := gemeindeCodeParts(m.GemeindeCode)
			if !strings.Contains(strings.ToLower(mState), state) {
				continue
			}
		}

		// District filter
		if district != "" && (len(m.GemeindeCode) < 3 || m.GemeindeCode[:3] != district) {
			continue
		}

		// Text filter
		if q != "" {
			nameL := strings.ToLower(m.Name)
			codeL := m.GemeindeCode
			if !strings.Contains(nameL, q) && !strings.Contains(codeL, q) {
				continue
			}
		}

		// Contains point filter
		if containsLon != 0 || containsLat != 0 {
			if !pointInAnyPolygon(containsLon, containsLat, m.Polygons) {
				continue
			}
		}

		distCode, stateName := gemeindeCodeParts(m.GemeindeCode)
		dName := districtName(distCode)

		result := ac.FilterFields(map[string]interface{}{
			"gemeinde_code": m.GemeindeCode,
			"name":          m.Name,
			"district_code": distCode,
			"district_name": dName,
			"state":         stateName,
			"lon":           m.CenterLon,
			"lat":           m.CenterLat,
			"source":        "statistik_austria",
		})
		allResults = append(allResults, result)

		// GeoJSON feature
		props := copyMap(result)
		delete(props, "lon")
		delete(props, "lat")
		geom := municipalityGeometry(m)
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

	respondSearchWithGeometry(w, r, ac, results, features, total, "municipalities.csv")
}

func municipalityGeometry(m *statAustriaMunicipality) map[string]interface{} {
	if len(m.Polygons) == 0 {
		return nil
	}
	// m.Polygons is [][][]float64 — each element is a ring (outer ring of one polygon part).
	// For a single-part polygon: Polygon with one ring.
	// For multi-part: MultiPolygon where each part has one ring.
	if len(m.Polygons) == 1 {
		return map[string]interface{}{
			"type":        "Polygon",
			"coordinates": [][][]float64{m.Polygons[0]},
		}
	}
	// MultiPolygon: each polygon is [ring], each ring is [][]float64
	var mp [][][][]float64
	for _, ring := range m.Polygons {
		mp = append(mp, [][][]float64{ring})
	}
	return map[string]interface{}{
		"type":        "MultiPolygon",
		"coordinates": mp,
	}
}
