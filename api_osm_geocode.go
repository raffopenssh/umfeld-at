// api_osm_geocode.go — OSM Nominatim address geocoding for the Austrian Cadastre API.
//
// Provides /api/v1/search/address_osm which resolves arbitrary Austrian
// addresses (e.g. "Innsbruck Hauptplatz") via the OpenStreetMap Nominatim
// service, returning a lon/lat point that can be fed into spatial queries.
//
// The endpoint restricts results to Austria (countrycodes=at) and adds a
// cross-reference to the nearest KG via the spatial index.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

func init() {
	handleV1SearchAddressOSM = v1SearchAddressOSMHandler
}

// ═══════════════════════════════════════════════════════════════════════════════
// Nominatim client with rate limiting and caching
// ═══════════════════════════════════════════════════════════════════════════════

const (
	nominatimBaseURL   = "https://nominatim.openstreetmap.org/search"
	nominatimUserAgent = "umfeld-at/1.0 (https://github.com/raffopenssh/umfeld-at)"
	nominatimRateLimit = 1100 * time.Millisecond // Nominatim policy: max 1 req/s
)

var (
	nominatimMu       sync.Mutex
	nominatimLastCall time.Time
	nominatimCache    sync.Map // map[string][]osmResult
)

// osmResult represents a single Nominatim search result.
type osmResult struct {
	PlaceID     int64       `json:"place_id"`
	Licence     string      `json:"licence"`
	OSMType     string      `json:"osm_type"`
	OSMID       int64       `json:"osm_id"`
	Lat         string      `json:"lat"`
	Lon         string      `json:"lon"`
	DisplayName string      `json:"display_name"`
	Class       string      `json:"class"`
	Type        string      `json:"type"`
	Importance  float64     `json:"importance"`
	BoundingBox []string    `json:"boundingbox"` // [south, north, west, east]
	Address     *osmAddress `json:"address,omitempty"`
}

type osmAddress struct {
	Road         string `json:"road,omitempty"`
	HouseNumber  string `json:"house_number,omitempty"`
	Suburb       string `json:"suburb,omitempty"`
	City         string `json:"city,omitempty"`
	Town         string `json:"town,omitempty"`
	Village      string `json:"village,omitempty"`
	Municipality string `json:"municipality,omitempty"`
	County       string `json:"county,omitempty"`
	State        string `json:"state,omitempty"`
	Postcode     string `json:"postcode,omitempty"`
	Country      string `json:"country,omitempty"`
	CountryCode  string `json:"country_code,omitempty"`
}

// queryNominatim calls the Nominatim API with rate limiting and caching.
func queryNominatim(query string, limit int) ([]osmResult, error) {
	cacheKey := fmt.Sprintf("%s|%d", strings.ToLower(strings.TrimSpace(query)), limit)

	// Check cache
	if cached, ok := nominatimCache.Load(cacheKey); ok {
		return cached.([]osmResult), nil
	}

	// Rate limit
	nominatimMu.Lock()
	since := time.Since(nominatimLastCall)
	if since < nominatimRateLimit {
		time.Sleep(nominatimRateLimit - since)
	}
	nominatimLastCall = time.Now()
	nominatimMu.Unlock()

	// Build URL
	params := url.Values{}
	params.Set("q", query)
	params.Set("format", "json")
	params.Set("addressdetails", "1")
	params.Set("limit", fmt.Sprintf("%d", limit))
	params.Set("countrycodes", "at")

	reqURL := nominatimBaseURL + "?" + params.Encode()

	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", nominatimUserAgent)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("nominatim request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("nominatim HTTP %d: %s", resp.StatusCode, string(body[:min(len(body), 200)]))
	}

	var results []osmResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	// Cache the result
	nominatimCache.Store(cacheKey, results)

	return results, nil
}

// ═══════════════════════════════════════════════════════════════════════════════
// Handler: /api/v1/search/address_osm
// ═══════════════════════════════════════════════════════════════════════════════

func v1SearchAddressOSMHandler(w http.ResponseWriter, r *http.Request) {
	ac := ParseAPIContext(r)
	q := r.URL.Query()

	query := q.Get("q")
	if query == "" {
		apiError(w, "parameter 'q' required (address or place name, e.g. 'Innsbruck Hauptplatz')", 400)
		return
	}

	limit := ac.Limit
	if limit > 50 {
		limit = 50 // Nominatim max
	}
	if limit < 1 {
		limit = 5
	}

	osmResults, err := queryNominatim(query, limit)
	if err != nil {
		log.Printf("⚠️ Nominatim error for %q: %v", query, err)
		apiError(w, "OSM geocoding failed: "+err.Error(), 502)
		return
	}

	// Convert to our response format with KG cross-reference
	var results []map[string]interface{}
	var features []map[string]interface{}

	for _, r := range osmResults {
		lat := parseFloat64(r.Lat)
		lon := parseFloat64(r.Lon)

		result := map[string]interface{}{
			"type":         "osm_address",
			"display_name": r.DisplayName,
			"lon":          lon,
			"lat":          lat,
			"osm_type":     r.OSMType,
			"osm_id":       r.OSMID,
			"class":        r.Class,
			"place_type":   r.Type,
			"importance":   r.Importance,
		}

		// Add address details
		if r.Address != nil {
			addr := map[string]interface{}{}
			if r.Address.Road != "" {
				addr["road"] = r.Address.Road
			}
			if r.Address.HouseNumber != "" {
				addr["house_number"] = r.Address.HouseNumber
			}
			if r.Address.City != "" {
				addr["city"] = r.Address.City
			} else if r.Address.Town != "" {
				addr["city"] = r.Address.Town
			} else if r.Address.Village != "" {
				addr["city"] = r.Address.Village
			}
			if r.Address.Municipality != "" {
				addr["municipality"] = r.Address.Municipality
			}
			if r.Address.County != "" {
				addr["county"] = r.Address.County
			}
			if r.Address.State != "" {
				addr["state"] = r.Address.State
			}
			if r.Address.Postcode != "" {
				addr["postcode"] = r.Address.Postcode
			}
			if r.Address.Suburb != "" {
				addr["suburb"] = r.Address.Suburb
			}
			result["address"] = addr
		}

		// Add bounding box
		if len(r.BoundingBox) == 4 {
			result["bbox"] = map[string]float64{
				"south": parseFloat64(r.BoundingBox[0]),
				"north": parseFloat64(r.BoundingBox[1]),
				"west":  parseFloat64(r.BoundingBox[2]),
				"east":  parseFloat64(r.BoundingBox[3]),
			}
		}

		// Cross-reference: find nearest KG from spatial index
		if globalSearchIndex != nil && (lon != 0 || lat != 0) {
			kg := findNearestKG(lon, lat)
			if kg != nil {
				result["nearest_kg"] = kg
			}
		}

		result = ac.FilterFields(result)
		results = append(results, result)

		// GeoJSON feature
		props := copyMap(result)
		delete(props, "lon")
		delete(props, "lat")
		features = append(features, GeoJSONPointFeature(lon, lat, props))
	}

	total := len(results)
	meta := NewMeta(ac, total)

	switch ac.Format {
	case "geojson":
		apiGeoJSONResponse(w, r, features, meta)
	case "csv":
		headers, rows := osmResultsToCSV(results)
		apiCSVResponse(w, r, headers, rows, "address_osm.csv")
	default:
		apiResponse(w, r, results, meta)
	}
}

// findNearestKG looks up the nearest KG to a given point using the search index.
func findNearestKG(lon, lat float64) map[string]interface{} {
	db := globalSearchIndex.DB()
	if db == nil {
		return nil
	}

	// First try: find KGs whose bbox contains this point
	row := db.QueryRow(`
		SELECT kg_code, kg_name, gemeinde_name, district_name, state_name,
		       (min_lon+max_lon)/2.0 as clon, (min_lat+max_lat)/2.0 as clat
		FROM kg
		WHERE min_lon <= ? AND max_lon >= ? AND min_lat <= ? AND max_lat >= ?
		ORDER BY (
			(((min_lon+max_lon)/2.0) - ?) * (((min_lon+max_lon)/2.0) - ?) +
			(((min_lat+max_lat)/2.0) - ?) * (((min_lat+max_lat)/2.0) - ?)
		) ASC
		LIMIT 1`,
		lon, lon, lat, lat,
		lon, lon, lat, lat)

	var kgCode, kgName, gemName, distName, stateName string
	var clon, clat float64
	if err := row.Scan(&kgCode, &kgName, &gemName, &distName, &stateName, &clon, &clat); err == nil {
		dist := haversine(lat, lon, clat, clon)
		return map[string]interface{}{
			"kg_code":       kgCode,
			"kg_name":       kgName,
			"gemeinde_name": gemName,
			"district_name": distName,
			"state_name":    stateName,
			"distance_m":    math.Round(dist*100) / 100,
		}
	}

	// Fallback: find nearest KG by centroid distance (wider search)
	row = db.QueryRow(`
		SELECT kg_code, kg_name, gemeinde_name, district_name, state_name,
		       (min_lon+max_lon)/2.0 as clon, (min_lat+max_lat)/2.0 as clat
		FROM kg
		ORDER BY (
			(((min_lon+max_lon)/2.0) - ?) * (((min_lon+max_lon)/2.0) - ?) +
			(((min_lat+max_lat)/2.0) - ?) * (((min_lat+max_lat)/2.0) - ?)
		) ASC
		LIMIT 1`,
		lon, lon, lat, lat)

	if err := row.Scan(&kgCode, &kgName, &gemName, &distName, &stateName, &clon, &clat); err == nil {
		dist := haversine(lat, lon, clat, clon)
		return map[string]interface{}{
			"kg_code":       kgCode,
			"kg_name":       kgName,
			"gemeinde_name": gemName,
			"district_name": distName,
			"state_name":    stateName,
			"distance_m":    math.Round(dist*100) / 100,
		}
	}

	return nil
}

func parseFloat64(s string) float64 {
	var v float64
	fmt.Sscanf(s, "%f", &v)
	return v
}

func osmResultsToCSV(results []map[string]interface{}) ([]string, [][]string) {
	headers := []string{"display_name", "lon", "lat", "class", "place_type",
		"osm_type", "osm_id", "importance", "kg_code", "kg_name", "gemeinde_name"}
	var rows [][]string
	for _, r := range results {
		kgCode, kgName, gemName := "", "", ""
		if kg, ok := r["nearest_kg"].(map[string]interface{}); ok {
			kgCode = str(kg["kg_code"])
			kgName = str(kg["kg_name"])
			gemName = str(kg["gemeinde_name"])
		}
		rows = append(rows, []string{
			str(r["display_name"]), str(r["lon"]), str(r["lat"]),
			str(r["class"]), str(r["place_type"]),
			str(r["osm_type"]), str(r["osm_id"]), str(r["importance"]),
			kgCode, kgName, gemName,
		})
	}
	return headers, rows
}
