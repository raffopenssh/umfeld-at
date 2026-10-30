// geo_util.go — small shared helpers: coordinate math at Austrian latitudes,
// query-parameter parsing, map/string coercion.
package main

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/paulmach/orb"
)

const (
	metersPerDegreeLat = 111320.0
	metersPerDegreeLon = 75500.0 // cos(47°) * 111320
	earth              = 6371000.0
)

func deg2rad(d float64) float64 { return d * math.Pi / 180 }

// haversine returns distance in meters between two WGS84 points.
func haversine(lat1, lon1, lat2, lon2 float64) float64 {
	dLat := deg2rad(lat2 - lat1)
	dLon := deg2rad(lon2 - lon1)
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(deg2rad(lat1))*math.Cos(deg2rad(lat2))*
			math.Sin(dLon/2)*math.Sin(dLon/2)
	return earth * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

// floatParam reads a float query parameter with a default.
func floatParam(r *http.Request, name string, defaultVal float64) float64 {
	s := r.URL.Query().Get(name)
	if s == "" {
		return defaultVal
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return defaultVal
	}
	return v
}

// isNumericCode returns true if s consists entirely of digits (e.g., "61630").
func isNumericCode(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func str(v interface{}) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

func copyMap(m map[string]interface{}) map[string]interface{} {
	c := make(map[string]interface{}, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

// bboxQuery is a WGS84 bounding box (west, south, east, north).
type bboxQuery struct {
	West, South, East, North float64
}

func floatQ(q map[string][]string, name string, def float64) float64 {
	vs, ok := q[name]
	if !ok || len(vs) == 0 || vs[0] == "" {
		return def
	}
	v, err := strconv.ParseFloat(vs[0], 64)
	if err != nil {
		return def
	}
	return v
}

// extractPolygonCoords returns the outer ring of a GeoJSON Polygon (or a
// Feature wrapping one) as [lon,lat] pairs.
func extractPolygonCoords(body map[string]interface{}) [][]float64 {
	if t, _ := body["type"].(string); t == "Feature" {
		if geom, ok := body["geometry"].(map[string]interface{}); ok {
			return extractPolygonCoords(geom)
		}
	}
	geomType, _ := body["type"].(string)
	coords, _ := body["coordinates"].([]interface{})
	if geomType == "Polygon" && len(coords) > 0 {
		ring, _ := coords[0].([]interface{})
		return parseCoordArray(ring)
	}
	return nil
}

func parseCoordArray(arr []interface{}) [][]float64 {
	var coords [][]float64
	for _, c := range arr {
		pair, ok := c.([]interface{})
		if !ok || len(pair) < 2 {
			continue
		}
		lon, _ := pair[0].(float64)
		lat, _ := pair[1].(float64)
		coords = append(coords, []float64{lon, lat})
	}
	return coords
}

// pointInPolygon — ray casting over one ring of [lon,lat] pairs.
func pointInPolygon(x, y float64, polygon [][]float64) bool {
	n := len(polygon)
	inside := false
	j := n - 1
	for i := 0; i < n; i++ {
		xi, yi := polygon[i][0], polygon[i][1]
		xj, yj := polygon[j][0], polygon[j][1]
		if ((yi > y) != (yj > y)) && (x < (xj-xi)*(y-yi)/(yj-yi)+xi) {
			inside = !inside
		}
		j = i
	}
	return inside
}

// polygonToGeoJSON / multiPolygonToGeoJSON — orb → GeoJSON geometry objects
// with 7-decimal coordinates (coord, api_compress.go).
func polygonToGeoJSON(p orb.Polygon) map[string]interface{} {
	rings := make([]interface{}, 0, len(p))
	for _, r := range p {
		coords := make([]coord, 0, len(r))
		for _, pt := range r {
			coords = append(coords, coord{pt[0], pt[1]})
		}
		rings = append(rings, coords)
	}
	return map[string]interface{}{"type": "Polygon", "coordinates": rings}
}

func multiPolygonToGeoJSON(mp orb.MultiPolygon) map[string]interface{} {
	if len(mp) == 0 {
		return nil
	}
	if len(mp) == 1 {
		return polygonToGeoJSON(mp[0])
	}
	parts := make([]interface{}, 0, len(mp))
	for _, p := range mp {
		rings := make([]interface{}, 0, len(p))
		for _, r := range p {
			coords := make([]coord, 0, len(r))
			for _, pt := range r {
				coords = append(coords, coord{pt[0], pt[1]})
			}
			rings = append(rings, coords)
		}
		parts = append(parts, rings)
	}
	return map[string]interface{}{"type": "MultiPolygon", "coordinates": parts}
}

// bufferBbox expands a bbox by meters.
func bufferBbox(west, south, east, north, meters float64) (float64, float64, float64, float64) {
	dLat := meters / metersPerDegreeLat
	dLon := meters / metersPerDegreeLon
	return west - dLon, south - dLat, east + dLon, north + dLat
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round4(v float64) float64 { return math.Round(v*10000) / 10000 }

// ParseKGList splits a comma/space separated list of KG codes.
func ParseKGList(s string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\t'
	}) {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

var siDistrictNames = map[string]string{
	"101": "Eisenstadt (Stadt)", "102": "Rust (Stadt)", "103": "Eisenstadt-Umgebung",
	"104": "Güssing", "105": "Jennersdorf", "106": "Mattersburg",
	"107": "Neusiedl am See", "108": "Oberpullendorf", "109": "Oberwart",
	"201": "Klagenfurt (Stadt)", "202": "Villach (Stadt)", "203": "Hermagor",
	"204": "Klagenfurt-Land", "205": "Sankt Veit an der Glan",
	"206": "Spittal an der Drau", "207": "Villach-Land", "208": "Völkermarkt",
	"209": "Wolfsberg", "210": "Feldkirchen",
	"301": "Krems an der Donau (Stadt)", "302": "Sankt Pölten (Stadt)",
	"303": "Waidhofen an der Ybbs (Stadt)", "304": "Wiener Neustadt (Stadt)",
	"305": "Amstetten", "306": "Baden", "307": "Bruck an der Leitha",
	"308": "Gänserndorf", "309": "Gmünd", "310": "Hollabrunn",
	"311": "Horn", "312": "Korneuburg", "313": "Krems (Land)",
	"314": "Lilienfeld", "315": "Melk", "316": "Mistelbach",
	"317": "Mödling", "318": "Neunkirchen", "319": "Sankt Pölten (Land)",
	"320": "Scheibbs", "321": "Tulln", "322": "Waidhofen an der Thaya",
	"323": "Wiener Neustadt (Land)", "324": "Zwettl",
	"325": "Waidhofen an der Thaya", // Historical district, merged into 322 in 2017, gemeinde codes 325xx still in use
	"401": "Linz (Stadt)", "402": "Steyr (Stadt)", "403": "Wels (Stadt)",
	"404": "Braunau am Inn", "405": "Eferding", "406": "Freistadt",
	"407": "Gmunden", "408": "Grieskirchen", "409": "Kirchdorf an der Krems",
	"410": "Linz-Land", "411": "Perg", "412": "Ried im Innkreis",
	"413": "Rohrbach", "414": "Schärding", "415": "Steyr-Land",
	"416": "Urfahr-Umgebung", "417": "Vöcklabruck", "418": "Wels-Land",
	"501": "Salzburg (Stadt)", "502": "Hallein", "503": "Salzburg-Umgebung",
	"504": "Sankt Johann im Pongau", "505": "Tamsweg", "506": "Zell am See",
	"601": "Graz (Stadt)", "603": "Deutschlandsberg", "606": "Graz-Umgebung",
	"610": "Leibnitz", "611": "Leoben", "612": "Liezen",
	"614": "Murau", "616": "Voitsberg", "617": "Weiz",
	"620": "Murtal", "621": "Bruck-Mürzzuschlag", "622": "Hartberg-Fürstenfeld",
	"623": "Südoststeiermark",
	"701": "Innsbruck (Stadt)", "702": "Imst", "703": "Innsbruck-Land",
	"704": "Kitzbühel", "705": "Kufstein", "706": "Landeck",
	"707": "Lienz", "708": "Reutte", "709": "Schwaz",
	"801": "Bludenz", "802": "Bregenz", "803": "Dornbirn", "804": "Feldkirch",
	"900": "Wien", "901": "Wien",
}

func siLookupDistrictName(districtCode string) string {
	if name, ok := siDistrictNames[districtCode]; ok {
		return name
	}
	return ""
}
