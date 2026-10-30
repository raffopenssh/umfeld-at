// natura2000.go — Natura 2000 protected sites overlay (no index rebuild required).
//
// Loads data/natura2000_at.geojson.gz at startup into in-memory features
// covering all Austrian Natura 2000 sites (Habitats Directive SCI/SAC,
// Birds Directive SPA, and combined). Provides:
//
//   - Point-in-polygon lookup (/api/v1/context, /api/v1/natura2000/point)
//   - REST endpoints under /api/v1/natura2000/{site,search,point}
//
// Data source: EEA Natura2000Sites MapServer (layer 2 = combined Habitats+Birds).
// To refresh:
//
//	curl 'https://bio.discomap.eea.europa.eu/arcgis/rest/services/ProtectedSites/Natura2000Sites/MapServer/2/query?where=MS%3D%27AT%27&outFields=SITECODE,SITENAME,MS,SITETYPE,Area_ha&f=geojson&outSR=4326&returnGeometry=true' | gzip > data/natura2000_at.geojson.gz
package main

import (
	"compress/gzip"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
)

const natura2000GeoJSONPath = "data/natura2000_at.geojson.gz"

// Natura2000Site is a single protected-sites feature.
type Natura2000Site struct {
	SiteCode string  `json:"sitecode"`
	SiteName string  `json:"sitename"`
	SiteType string  `json:"sitetype"` // A=SPA(Birds), B=SCI/SAC(Habitats), C=both
	MS       string  `json:"ms"`
	AreaHa   float64 `json:"area_ha"`

	// Geometry: list of polygons (each polygon = outer ring + zero or more holes).
	Polygons [][][][]float64 `json:"-"`

	// Bounding box of all polygons.
	MinLon, MinLat, MaxLon, MaxLat float64
}

var (
	natura2000Sites  []Natura2000Site
	natura2000ByCode map[string]*Natura2000Site
	natura2000Once   sync.Once
	natura2000Loaded bool
)

// loadNatura2000 reads the gzipped GeoJSON once.
func loadNatura2000() {
	natura2000Once.Do(func() {
		natura2000ByCode = map[string]*Natura2000Site{}

		f, err := os.Open(natura2000GeoJSONPath)
		if err != nil {
			log.Printf("🛡  Natura2000 data not found (%s) — Natura2000 API disabled", natura2000GeoJSONPath)
			return
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			log.Printf("🛡  Natura2000 gunzip error: %v", err)
			return
		}
		defer gz.Close()

		var fc struct {
			Features []struct {
				Properties map[string]interface{} `json:"properties"`
				Geometry   struct {
					Type        string          `json:"type"`
					Coordinates json.RawMessage `json:"coordinates"`
				} `json:"geometry"`
			} `json:"features"`
		}
		if err := json.NewDecoder(gz).Decode(&fc); err != nil {
			log.Printf("🛡  Natura2000 parse error: %v", err)
			return
		}

		for _, ft := range fc.Features {
			polys := parseNatura2000Polygons(ft.Geometry.Type, ft.Geometry.Coordinates)
			if len(polys) == 0 {
				continue
			}
			s := Natura2000Site{
				SiteCode: strProp(ft.Properties, "SITECODE"),
				SiteName: strProp(ft.Properties, "SITENAME"),
				SiteType: strProp(ft.Properties, "SITETYPE"),
				MS:       strProp(ft.Properties, "MS"),
				AreaHa:   floatProp(ft.Properties, "Area_ha"),
				Polygons: polys,
			}
			s.MinLon, s.MinLat, s.MaxLon, s.MaxLat = polygonsBBox(polys)
			natura2000Sites = append(natura2000Sites, s)
		}
		for i := range natura2000Sites {
			natura2000ByCode[natura2000Sites[i].SiteCode] = &natura2000Sites[i]
		}
		natura2000Loaded = true
		log.Printf("🛡  Natura2000: loaded %d sites", len(natura2000Sites))
	})
}

// parseNatura2000Polygons decodes a Polygon or MultiPolygon into list of polygons,
// each polygon being a list of rings (outer + holes).
func parseNatura2000Polygons(geomType string, raw json.RawMessage) [][][][]float64 {
	if len(raw) == 0 {
		return nil
	}
	switch geomType {
	case "Polygon":
		var poly [][][]float64
		if err := json.Unmarshal(raw, &poly); err != nil {
			return nil
		}
		return [][][][]float64{poly}
	case "MultiPolygon":
		var mp [][][][]float64
		if err := json.Unmarshal(raw, &mp); err != nil {
			return nil
		}
		return mp
	}
	return nil
}

// polygonsBBox computes the bounding box across all rings.
func polygonsBBox(polys [][][][]float64) (minLon, minLat, maxLon, maxLat float64) {
	minLon, minLat = 180, 90
	maxLon, maxLat = -180, -90
	for _, poly := range polys {
		for _, ring := range poly {
			for _, pt := range ring {
				if len(pt) < 2 {
					continue
				}
				if pt[0] < minLon {
					minLon = pt[0]
				}
				if pt[0] > maxLon {
					maxLon = pt[0]
				}
				if pt[1] < minLat {
					minLat = pt[1]
				}
				if pt[1] > maxLat {
					maxLat = pt[1]
				}
			}
		}
	}
	return
}

// ═══════════════════════════════════════════════════════════════════════════════
// Spatial queries
// ═══════════════════════════════════════════════════════════════════════════════

// natura2000SitesForPoint returns all sites whose outer ring contains the point.
// Holes are honoured (point inside hole → not contained).
func natura2000SitesForPoint(lon, lat float64) []*Natura2000Site {
	loadNatura2000()
	if !natura2000Loaded {
		return nil
	}
	var matches []*Natura2000Site
	for i := range natura2000Sites {
		s := &natura2000Sites[i]
		if lon < s.MinLon || lon > s.MaxLon || lat < s.MinLat || lat > s.MaxLat {
			continue
		}
		if sitePointInside(s, lon, lat) {
			matches = append(matches, s)
		}
	}
	return matches
}

func sitePointInside(s *Natura2000Site, lon, lat float64) bool {
	for _, poly := range s.Polygons {
		if len(poly) == 0 {
			continue
		}
		if !pointInPolygon(lon, lat, poly[0]) {
			continue
		}
		// Check holes.
		inHole := false
		for h := 1; h < len(poly); h++ {
			if pointInPolygon(lon, lat, poly[h]) {
				inHole = true
				break
			}
		}
		if !inHole {
			return true
		}
	}
	return false
}

// buildKGOverlapCache precomputes (lazily) a KG → [sitecode] map by bbox
// intersection. This is fast and good enough for the KG-level "has overlap"
// indicator. Per-parcel results still use exact point-in-polygon.// ═══════════════════════════════════════════════════════════════════════════════
// Enrichment helpers (called from api_search.go alongside legal_refs enrichers)
// ═══════════════════════════════════════════════════════════════════════════════

func natura2000SiteToMap(s *Natura2000Site) map[string]interface{} {
	return map[string]interface{}{
		"sitecode":        s.SiteCode,
		"sitename":        s.SiteName,
		"sitetype":        s.SiteType,
		"site_type_label": natura2000TypeLabel(s.SiteType),
		"area_ha":         s.AreaHa,
		"habitats":        natura2000HabitatsForName(s.SiteName),
	}
}

// natura2000HabitatRules maps a habitat tag to keyword patterns found in the
// SITENAME. Names are German (mostly), so keywords are lowercased substrings.
// Order does not matter — a site can belong to multiple habitat classes
// (e.g. "Donau-Auen" → floodplain + river).
var natura2000HabitatRules = []struct {
	tag      string
	keywords []string
}{
	{"moor", []string{"moor", "moos"}},
	{"floodplain", []string{"au­", "auen", "-au ", "-au-", " au ", " au-"}}, // "Donau-Auen", "March-Auen"
	{"river", []string{"fluss", "bach", "strom", "-tal", "fluß", "unterlauf", "oberlauf"}},
	{"lake", []string{"see", "teich", "lacke", "weiher"}},
	{"wetland", []string{"sumpf", "ried", "feuchtwiese", "feuchtgebiet"}},
	{"forest", []string{"wald", "forst", "-au-wald", "föhren", "buchen"}},
	{"alpine", []string{"alpen", "hochgebirge", "gebirge", "berg", "-spitze", "glet", "karst"}},
	{"meadow", []string{"wiese", "trockenrasen", "halbtrocken", "magerrasen", "rasen"}},
	{"pasture", []string{"alm", "weide", "hutweide"}},
	{"valley", []string{"tal", "klamm", "schlucht"}},
	{"hill", []string{"hügel", "berg", "kogel", "rücken"}},
	{"steppe", []string{"steppe", "heide"}},
	{"orchard", []string{"obst", "weingarten", "weinberg"}},
	{"park", []string{"nationalpark", "naturpark"}},
	{"cave", []string{"höhle"}},
}

// natura2000HabitatsForName classifies a site name into 0..N habitat tags.
func natura2000HabitatsForName(name string) []string {
	if name == "" {
		return nil
	}
	lc := strings.ToLower(name)
	var tags []string
	seen := map[string]bool{}
	for _, rule := range natura2000HabitatRules {
		for _, kw := range rule.keywords {
			if strings.Contains(lc, kw) {
				if !seen[rule.tag] {
					tags = append(tags, rule.tag)
					seen[rule.tag] = true
				}
				break
			}
		}
	}
	return tags
}

func natura2000TypeLabel(t string) string {
	switch t {
	case "A":
		return "Birds Directive (SPA)"
	case "B":
		return "Habitats Directive (pSCI/SCI/SAC)"
	case "C":
		return "Habitats + Birds Directive"
	}
	return t
}

// EnrichParcelResultNatura2000 adds natura2000 site annotations to a parcel result
// when the parcel centroid falls inside one or more sites. Uses the precomputed
// parcel cache when ready (O(1)); falls back to live point-in-polygon otherwise.// ═══════════════════════════════════════════════════════════════════════════════
// HTTP handlers
// ═══════════════════════════════════════════════════════════════════════════════

var (
	handleV1Natura2000Site   http.HandlerFunc = notImplemented("natura2000/site")
	handleV1Natura2000Search http.HandlerFunc = notImplemented("natura2000/search")
	handleV1Natura2000Point  http.HandlerFunc = notImplemented("natura2000/point")
)

func init() {
	handleV1Natura2000Site = natura2000SiteHandler
	handleV1Natura2000Search = natura2000SearchHandler
	handleV1Natura2000Point = natura2000PointHandler
}

// RegisterNatura2000API registers /api/v1/natura2000/* routes.
func RegisterNatura2000API(mux *http.ServeMux) {
	loadNatura2000()
	v1 := func(pattern string, handler http.HandlerFunc) {
		mux.Handle(pattern, apiMiddleware(handler))
	}
	v1("/api/v1/natura2000/site/", handleV1Natura2000Site)
	v1("/api/v1/natura2000/search", handleV1Natura2000Search)
	v1("/api/v1/natura2000/point", handleV1Natura2000Point)
	log.Println("🛡  Natura2000 API routes registered under /api/v1/natura2000/")
}

// GET /api/v1/natura2000/site/{sitecode}
func natura2000SiteHandler(w http.ResponseWriter, r *http.Request) {
	loadNatura2000()
	if !natura2000Loaded {
		apiError(w, "natura2000 data not available", 503)
		return
	}
	code := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/natura2000/site/"), "/")
	if code == "" {
		apiError(w, "sitecode required", 400)
		return
	}
	s, ok := natura2000ByCode[code]
	if !ok {
		apiError(w, "sitecode not found: "+code, 404)
		return
	}
	ac := ParseAPIContext(r)
	includeGeom := r.URL.Query().Get("geometry") == "1"
	out := natura2000SiteToMap(s)
	out["bbox"] = map[string]float64{
		"min_lon": s.MinLon, "min_lat": s.MinLat,
		"max_lon": s.MaxLon, "max_lat": s.MaxLat,
	}
	if includeGeom {
		out["geometry"] = map[string]interface{}{
			"type":        "MultiPolygon",
			"coordinates": s.Polygons,
		}
	}
	apiResponse(w, r, out, NewMeta(ac, 1))
}

// GET /api/v1/natura2000/search?q=&type=&list=all
func natura2000SearchHandler(w http.ResponseWriter, r *http.Request) {
	loadNatura2000()
	if !natura2000Loaded {
		apiError(w, "natura2000 data not available", 503)
		return
	}
	ac := ParseAPIContext(r)
	qp := r.URL.Query()
	q := strings.ToLower(qp.Get("q"))
	typeF := qp.Get("type")
	listAll := qp.Get("list") == "all"

	var matches []*Natura2000Site
	for i := range natura2000Sites {
		s := &natura2000Sites[i]
		if typeF != "" && s.SiteType != typeF {
			continue
		}
		if !listAll && q != "" {
			if !strings.Contains(strings.ToLower(s.SiteName), q) &&
				!strings.Contains(strings.ToLower(s.SiteCode), q) {
				continue
			}
		}
		matches = append(matches, s)
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].SiteName < matches[j].SiteName })
	total := len(matches)
	start := ac.Offset
	if start > total {
		start = total
	}
	end := start + ac.Limit
	if end > total {
		end = total
	}
	results := make([]map[string]interface{}, 0, end-start)
	for _, s := range matches[start:end] {
		m := natura2000SiteToMap(s)
		m["bbox"] = map[string]float64{
			"min_lon": s.MinLon, "min_lat": s.MinLat,
			"max_lon": s.MaxLon, "max_lat": s.MaxLat,
		}
		results = append(results, m)
	}
	apiResponse(w, r, map[string]interface{}{
		"total":   total,
		"results": results,
	}, NewMeta(ac, total))
}

// GET /api/v1/natura2000/stats// GET /api/v1/natura2000/point?lon=&lat=
func natura2000PointHandler(w http.ResponseWriter, r *http.Request) {
	loadNatura2000()
	if !natura2000Loaded {
		apiError(w, "natura2000 data not available", 503)
		return
	}
	ac := ParseAPIContext(r)
	lon := floatParam(r, "lon", 0)
	lat := floatParam(r, "lat", 0)
	if lon == 0 && lat == 0 {
		apiError(w, "lon and lat required", 400)
		return
	}
	matches := natura2000SitesForPoint(lon, lat)
	results := make([]map[string]interface{}, 0, len(matches))
	for _, s := range matches {
		results = append(results, natura2000SiteToMap(s))
	}
	apiResponse(w, r, map[string]interface{}{
		"lon":           lon,
		"lat":           lat,
		"in_natura2000": len(results) > 0,
		"sites":         results,
	}, NewMeta(ac, len(results)))
}

// GET /api/v1/natura2000/parcel/{kg_code}/{gnr}
