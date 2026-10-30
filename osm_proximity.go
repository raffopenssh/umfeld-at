// osm_proximity.go — OSM (Geofabrik) proximity metrics for a point or an H3 cell.
//
// Accessibility/remoteness info derived from the Geofabrik Austria extract
// (data/osm_geofabrik_data/austria.gpkg):
//   - distance to nearest drivable road + nearest MAJOR road (motorway/trunk/
//     primary/secondary), incl. class/name/ref
//   - "road on unit" heuristic (nearest road within the unit's
//     equivalent-circle radius; the unit is a cell or a caller-supplied area)
//   - distance to nearest public-transport stop (bus/tram/train/ferry) and
//     nearest train station specifically
//   - distance to nearest railway line, nearest water (river/stream/canal/
//     lake), nearest settlement (village/town/city)
//   - a composite remoteness score 0(=central)..100(=remote)
//
// ARCHITECTURE: its OWN SQLite file data/osm_proximity.db.
//
//  1. `umfeld-at -osm-import` (one-off CLI): reads the Geofabrik GPKG
//     and writes compact chunked polylines/points + R*Tree indexes into
//     data/osm_proximity.db. Run while the API keeps serving.
//  2. Server: metrics are computed live from the R-tree (few ms) for
//     /api/v1/osm/point and /api/v1/osm/cell/{h3}; nothing is persisted.
//
// Nearest-feature search: expanding-ring R*Tree bbox lookups; a result is
// final once best-distance <= ring radius. Distances in metres via a local
// equirectangular projection (adequate at Austrian latitudes for <100 km).
package main

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"os"
	"sync"
	"time"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/encoding/wkb"
)

const (
	osmProxDBPath        = "data/osm_proximity.db"
	osmGPKGPath          = "data/osm_geofabrik_data/austria.gpkg"
	osmProxSchemaVersion = "1"
	// Max chunk vertices when splitting long polylines for tight rtree boxes.
	osmChunkMaxPts = 24
	// Search cutoffs per category (metres). Beyond → distance reported as null.
	osmMaxRoadDist       = 50000.0
	osmMaxTransitDist    = 50000.0
	osmMaxRailDist       = 100000.0
	osmMaxWaterDist      = 50000.0
	osmMaxSettlementDist = 100000.0
)

// osmMajorRoad classes (the "major road" metric).
var osmMajorRoadClasses = map[string]bool{
	"motorway": true, "motorway_link": true,
	"trunk": true, "trunk_link": true,
	"primary": true, "primary_link": true,
	"secondary": true, "secondary_link": true,
}

// osmDrivableClasses — everything indexed as cat='road' (major flag separate).
var osmDrivableClasses = map[string]bool{
	"motorway": true, "motorway_link": true, "trunk": true, "trunk_link": true,
	"primary": true, "primary_link": true, "secondary": true, "secondary_link": true,
	"tertiary": true, "tertiary_link": true, "residential": true,
	"unclassified": true, "living_street": true, "pedestrian": true,
	"service": true, "busway": true,
}

var osmRailClasses = map[string]bool{
	"rail": true, "light_rail": true, "subway": true, "tram": true,
	"narrow_gauge": true, "funicular": true, "rack": true, "monorail": true,
}

var osmWaterLineClasses = map[string]bool{
	"river": true, "stream": true, "canal": true,
}

var osmWaterAreaClasses = map[string]bool{
	"water": true, "riverbank": true, "reservoir": true,
}

var osmTransitClasses = map[string]bool{
	"bus_stop": true, "bus_station": true, "tram_stop": true,
	"railway_station": true, "railway_halt": true, "ferry_terminal": true,
}

var osmTrainStationClasses = map[string]bool{
	"railway_station": true, "railway_halt": true,
}

var osmSettlementClasses = map[string]bool{
	"city": true, "town": true, "village": true, "national_capital": true,
}

// ── DB handle ────────────────────────────────────────────────────────────────

var (
	osmDB     *sql.DB
	osmDBOnce sync.Once
	osmReady  bool
	osmMu     sync.RWMutex
)

// InitOSMProximity opens (or creates) the proximity DB. Safe to call always;
// enrichment is a no-op until the lines/points tables have been imported.
// osmProxMaxConns / osmProxCacheKB mirror searchIndexMaxConns /
// searchIndexCacheKB. Keep them in step: pool × cache is the real RSS budget.
const (
	osmProxMaxConns = 24
	osmProxCacheKB  = 8000
)

func InitOSMProximity() {
	osmDBOnce.Do(func() {
		registerTunedSQLite()
		db, err := sql.Open(sqliteTunedDriver, osmProxDBPath+fmt.Sprintf("?_journal_mode=WAL&_synchronous=NORMAL&_cache_size=-%d&_busy_timeout=15000", osmProxCacheKB))
		if err != nil {
			log.Printf("🛣️ osm_proximity open: %v", err)
			return
		}
		// Pool sizing: same reasoning — and the same past outage — as the search
		// index (see searchIndexMaxConns). This DB was left at
		// SetMaxOpenConns(4) when the search index was raised to 24, so it was
		// still a live instance of the wedge: WAL allows unlimited concurrent
		// readers, the Go pool was the only cap, and /api/v1/osm/* plus the
		// osm{} enrichment that /query and /search/parcel do on every row could
		// park four slots and leave every other caller blocked in
		// database/sql.(*DB).conn. _cache_size is PER CONNECTION, so the cache
		// is scaled down to keep pool × cache inside the same memory budget
		// (24 × 8 MB ≈ 192 MB, was 4 × 32 MB).
		db.SetMaxOpenConns(osmProxMaxConns)
		db.SetMaxIdleConns(osmProxMaxConns)
		db.SetConnMaxIdleTime(5 * time.Minute)
		if err := createOSMProxTables(db); err != nil {
			log.Printf("🛣️ osm_proximity create tables: %v", err)
			db.Close()
			return
		}
		osmDB = db
		var nLines int
		db.QueryRow(`SELECT count(*) FROM osm_lines`).Scan(&nLines)
		osmMu.Lock()
		osmReady = nLines > 0
		osmMu.Unlock()
		if osmReady {
			remoteGridEnsure(db) // the ROADS overlay's remoteness tint (osm_remote_grid.go)
			log.Printf("🛣️ OSM proximity ready: %d line chunks", nLines)
		} else {
			log.Printf("🛣️ OSM proximity DB empty — run `umfeld-at -osm-import` to populate")
		}
	})
}

func OSMProximityReady() bool {
	osmMu.RLock()
	defer osmMu.RUnlock()
	return osmReady
}

func createOSMProxTables(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS osm_meta(key TEXT PRIMARY KEY, value TEXT)`,
		`CREATE TABLE IF NOT EXISTS osm_lines(
			id INTEGER PRIMARY KEY,
			cat TEXT NOT NULL,        -- road | rail | water
			major INTEGER DEFAULT 0,  -- road: major-road flag
			fclass TEXT, name TEXT, ref TEXT,
			coords BLOB NOT NULL      -- packed little-endian float64 lon,lat pairs
		)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS osm_lines_rtree USING rtree(
			id, min_lon, max_lon, min_lat, max_lat)`,
		`CREATE TABLE IF NOT EXISTS osm_points(
			id INTEGER PRIMARY KEY,
			cat TEXT NOT NULL,        -- transit | place
			station INTEGER DEFAULT 0,-- transit: train station/halt flag
			fclass TEXT, name TEXT, pop INTEGER,
			lon REAL, lat REAL
		)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS osm_points_rtree USING rtree(
			id, min_lon, max_lon, min_lat, max_lat)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return err
		}
	}
	if err := createOSMAreaTables(db); err != nil {
		return err
	}
	_, err := db.Exec(`INSERT OR REPLACE INTO osm_meta(key,value) VALUES('schema_version',?)`, osmProxSchemaVersion)
	return err
}

// ── geometry helpers ─────────────────────────────────────────────────────────

func packCoords(ls orb.LineString) []byte {
	b := make([]byte, 16*len(ls))
	for i, p := range ls {
		binary.LittleEndian.PutUint64(b[i*16:], math.Float64bits(p[0]))
		binary.LittleEndian.PutUint64(b[i*16+8:], math.Float64bits(p[1]))
	}
	return b
}

func unpackCoords(b []byte) []orb.Point {
	n := len(b) / 16
	pts := make([]orb.Point, n)
	for i := 0; i < n; i++ {
		pts[i][0] = math.Float64frombits(binary.LittleEndian.Uint64(b[i*16:]))
		pts[i][1] = math.Float64frombits(binary.LittleEndian.Uint64(b[i*16+8:]))
	}
	return pts
}

// gpkgToWKB strips the GeoPackage binary header, returning raw WKB.
func gpkgToWKB(blob []byte) ([]byte, error) {
	if len(blob) < 8 || blob[0] != 'G' || blob[1] != 'P' {
		return nil, fmt.Errorf("not a GPKG geometry blob")
	}
	flags := blob[3]
	envInd := (flags >> 1) & 7
	var envSize int
	switch envInd {
	case 0:
		envSize = 0
	case 1:
		envSize = 32
	case 2, 3:
		envSize = 48
	case 4:
		envSize = 64
	default:
		return nil, fmt.Errorf("invalid envelope indicator %d", envInd)
	}
	if len(blob) < 8+envSize {
		return nil, fmt.Errorf("truncated GPKG blob")
	}
	return blob[8+envSize:], nil
}

// metres per degree at a latitude (equirectangular local projection)
func metresPerDeg(lat float64) (mLon, mLat float64) {
	return 111320.0 * math.Cos(lat*math.Pi/180), 110574.0
}

// pointSegDistM: distance point→segment in metres (local equirect).
func pointSegDistM(lon, lat, x1, y1, x2, y2, mLon, mLat float64) float64 {
	ax := (x1 - lon) * mLon
	ay := (y1 - lat) * mLat
	bx := (x2 - lon) * mLon
	by := (y2 - lat) * mLat
	dx, dy := bx-ax, by-ay
	l2 := dx*dx + dy*dy
	t := 0.0
	if l2 > 0 {
		t = -(ax*dx + ay*dy) / l2
		if t < 0 {
			t = 0
		} else if t > 1 {
			t = 1
		}
	}
	px, py := ax+t*dx, ay+t*dy
	return math.Sqrt(px*px + py*py)
}

// ── nearest-feature search ───────────────────────────────────────────────────

type osmNearestHit struct {
	Dist   float64
	Fclass string
	Name   string
	Ref    string
}

var osmSearchRings = []float64{250, 1000, 4000, 16000, 64000, 128000}

// osmNearestLine finds the nearest line chunk of a category.
// extraWhere is appended to the SQL (e.g. "AND major=1").
func osmNearestLine(lon, lat float64, cat, extraWhere string, maxDist float64) *osmNearestHit {
	if osmDB == nil {
		return nil
	}
	mLon, mLat := metresPerDeg(lat)
	best := math.Inf(1)
	var hit osmNearestHit
	for _, r := range osmSearchRings {
		dLon, dLat := r/mLon, r/mLat
		rows, err := osmDB.Query(`
			SELECT l.fclass, l.name, l.ref, l.coords FROM osm_lines_rtree rt
			JOIN osm_lines l ON l.id = rt.id
			WHERE rt.max_lon >= ? AND rt.min_lon <= ? AND rt.max_lat >= ? AND rt.min_lat <= ?
			  AND l.cat = ? `+extraWhere,
			lon-dLon, lon+dLon, lat-dLat, lat+dLat, cat)
		if err != nil {
			return nil
		}
		for rows.Next() {
			var fclass, name, ref sql.NullString
			var coords []byte
			if err := rows.Scan(&fclass, &name, &ref, &coords); err != nil {
				continue
			}
			pts := unpackCoords(coords)
			for i := 0; i+1 < len(pts); i++ {
				d := pointSegDistM(lon, lat, pts[i][0], pts[i][1], pts[i+1][0], pts[i+1][1], mLon, mLat)
				if d < best {
					best = d
					hit = osmNearestHit{Dist: d, Fclass: fclass.String, Name: name.String, Ref: ref.String}
				}
			}
		}
		rows.Close()
		if best <= r || r >= maxDist {
			break
		}
	}
	if math.IsInf(best, 1) || best > maxDist {
		return nil
	}
	hit.Dist = math.Round(hit.Dist*10) / 10
	return &hit
}

// osmNearestPoint finds the nearest point feature of a category.
func osmNearestPoint(lon, lat float64, cat, extraWhere string, maxDist float64) *osmNearestHit {
	if osmDB == nil {
		return nil
	}
	mLon, mLat := metresPerDeg(lat)
	best := math.Inf(1)
	var hit osmNearestHit
	for _, r := range osmSearchRings {
		dLon, dLat := r/mLon, r/mLat
		rows, err := osmDB.Query(`
			SELECT p.fclass, p.name, p.lon, p.lat FROM osm_points_rtree rt
			JOIN osm_points p ON p.id = rt.id
			WHERE rt.max_lon >= ? AND rt.min_lon <= ? AND rt.max_lat >= ? AND rt.min_lat <= ?
			  AND p.cat = ? `+extraWhere,
			lon-dLon, lon+dLon, lat-dLat, lat+dLat, cat)
		if err != nil {
			return nil
		}
		for rows.Next() {
			var fclass, name sql.NullString
			var plon, plat float64
			if err := rows.Scan(&fclass, &name, &plon, &plat); err != nil {
				continue
			}
			dx := (plon - lon) * mLon
			dy := (plat - lat) * mLat
			d := math.Sqrt(dx*dx + dy*dy)
			if d < best {
				best = d
				hit = osmNearestHit{Dist: d, Fclass: fclass.String, Name: name.String}
			}
		}
		rows.Close()
		if best <= r || r >= maxDist {
			break
		}
	}
	if math.IsInf(best, 1) || best > maxDist {
		return nil
	}
	hit.Dist = math.Round(hit.Dist*10) / 10
	return &hit
}

// ── metrics computation ──────────────────────────────────────────────────────

// OSMMetrics is the full metrics record for one parcel/point.
type OSMMetrics struct {
	DistRoadM        *float64 `json:"dist_road_m,omitempty"`
	RoadFclass       string   `json:"road_fclass,omitempty"`
	RoadName         string   `json:"road_name,omitempty"`
	RoadOnUnit       bool     `json:"road_on_parcel"`
	DistMajorRoadM   *float64 `json:"dist_major_road_m,omitempty"`
	MajorRoadFclass  string   `json:"major_road_fclass,omitempty"`
	MajorRoadName    string   `json:"major_road_name,omitempty"`
	MajorRoadRef     string   `json:"major_road_ref,omitempty"`
	DistRailM        *float64 `json:"dist_rail_m,omitempty"`
	DistTransitM     *float64 `json:"dist_transit_m,omitempty"`
	TransitFclass    string   `json:"transit_fclass,omitempty"`
	TransitName      string   `json:"transit_name,omitempty"`
	DistTrainStatM   *float64 `json:"dist_train_station_m,omitempty"`
	TrainStatName    string   `json:"train_station_name,omitempty"`
	DistWaterM       *float64 `json:"dist_water_m,omitempty"`
	WaterFclass      string   `json:"water_fclass,omitempty"`
	WaterName        string   `json:"water_name,omitempty"`
	DistSettlementM  *float64 `json:"dist_settlement_m,omitempty"`
	SettlementFclass string   `json:"settlement_fclass,omitempty"`
	SettlementName   string   `json:"settlement_name,omitempty"`
	Remoteness       float64  `json:"remoteness"`
}

// computeOSMMetrics runs all nearest searches for one point.
// areaSqm (may be 0) drives the road_on_parcel heuristic.
func computeOSMMetrics(lon, lat, areaSqm float64) *OSMMetrics {
	if !OSMProximityReady() {
		return nil
	}
	m := &OSMMetrics{}
	if h := osmNearestLine(lon, lat, "road", "", osmMaxRoadDist); h != nil {
		m.DistRoadM, m.RoadFclass, m.RoadName = &h.Dist, h.Fclass, h.Name
		if areaSqm > 0 {
			rEff := math.Sqrt(areaSqm / math.Pi)
			m.RoadOnUnit = h.Dist <= rEff
		}
	}
	if h := osmNearestLine(lon, lat, "road", "AND l.major=1", osmMaxRoadDist); h != nil {
		m.DistMajorRoadM, m.MajorRoadFclass, m.MajorRoadName, m.MajorRoadRef = &h.Dist, h.Fclass, h.Name, h.Ref
	}
	if h := osmNearestLine(lon, lat, "rail", "", osmMaxRailDist); h != nil {
		m.DistRailM = &h.Dist
	}
	if h := osmNearestPoint(lon, lat, "transit", "", osmMaxTransitDist); h != nil {
		m.DistTransitM, m.TransitFclass, m.TransitName = &h.Dist, h.Fclass, h.Name
	}
	if h := osmNearestPoint(lon, lat, "transit", "AND p.station=1", osmMaxRailDist); h != nil {
		m.DistTrainStatM, m.TrainStatName = &h.Dist, h.Name
	}
	if h := osmNearestLine(lon, lat, "water", "", osmMaxWaterDist); h != nil {
		m.DistWaterM, m.WaterFclass, m.WaterName = &h.Dist, h.Fclass, h.Name
	}
	if h := osmNearestPoint(lon, lat, "place", "", osmMaxSettlementDist); h != nil {
		m.DistSettlementM, m.SettlementFclass, m.SettlementName = &h.Dist, h.Fclass, h.Name
	}
	m.Remoteness = remotenessScore(m)
	return m
}

// remotenessScore: 0 (central) .. 100 (remote). Weighted sqrt-normalised
// distances with per-component caps:
//
//	road 5 km (w=0.30), major road 20 km (w=0.15),
//	transit stop 10 km (w=0.30), settlement 15 km (w=0.25)
func remotenessScore(m *OSMMetrics) float64 {
	comp := func(d *float64, cap float64) float64 {
		if d == nil {
			return 1.0
		}
		v := *d / cap
		if v > 1 {
			v = 1
		}
		return math.Sqrt(v)
	}
	score := 0.30*comp(m.DistRoadM, 5000) +
		0.15*comp(m.DistMajorRoadM, 20000) +
		0.30*comp(m.DistTransitM, 10000) +
		0.25*comp(m.DistSettlementM, 15000)
	return math.Round(score*1000) / 10
}

// ── persistence ──────────────────────────────────────────────────────────────

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// OSMMetricsForParcel returns cached metrics (fast path: PK lookup).
// Returns nil when not cached.
func osmMetricsToMap(m *OSMMetrics) map[string]interface{} {
	out := map[string]interface{}{
		"remoteness":     m.Remoteness,
		"road_on_parcel": m.RoadOnUnit,
	}
	set := func(k string, d *float64, extra map[string]string) {
		if d == nil {
			return
		}
		out[k] = *d
		for ek, ev := range extra {
			if ev != "" {
				out[ek] = ev
			}
		}
	}
	set("dist_road_m", m.DistRoadM, map[string]string{"road_fclass": m.RoadFclass, "road_name": m.RoadName})
	set("dist_major_road_m", m.DistMajorRoadM, map[string]string{"major_road_fclass": m.MajorRoadFclass, "major_road_name": m.MajorRoadName, "major_road_ref": m.MajorRoadRef})
	set("dist_rail_m", m.DistRailM, nil)
	set("dist_transit_m", m.DistTransitM, map[string]string{"transit_fclass": m.TransitFclass, "transit_name": m.TransitName})
	set("dist_train_station_m", m.DistTrainStatM, map[string]string{"train_station_name": m.TrainStatName})
	set("dist_water_m", m.DistWaterM, map[string]string{"water_fclass": m.WaterFclass, "water_name": m.WaterName})
	set("dist_settlement_m", m.DistSettlementM, map[string]string{"settlement_fclass": m.SettlementFclass, "settlement_name": m.SettlementName})
	return out
}

// EnrichParcelResultOSM attaches an "osm" block to a parcel result map.
// Fast path: single PK lookup against the (separate) osm_proximity.db.
// Miss in bulk contexts: queue whole-KG warming (continuous request warming)
// but do NOT compute live — large result sets would multiply latency.
// Callers that need a guaranteed answer for ONE parcel use
// /api/v1/osm/parcel/{id} which computes live on miss.
func RunOSMImport() error {
	if _, err := os.Stat(osmGPKGPath); err != nil {
		return fmt.Errorf("GPKG not found at %s — unzip data/osm_geofabrik_data/geofabrik_ssd_roads.zip first", osmGPKGPath)
	}
	InitOSMProximity()
	if osmDB == nil {
		return fmt.Errorf("osm_proximity DB unavailable")
	}
	src, err := sql.Open("sqlite3", "file:"+osmGPKGPath+"?mode=ro")
	if err != nil {
		return err
	}
	defer src.Close()

	start := time.Now()
	// wipe previous import
	for _, t := range []string{"osm_lines_rtree", "osm_lines", "osm_points_rtree", "osm_points"} {
		if _, err := osmDB.Exec("DELETE FROM " + t); err != nil {
			return err
		}
	}

	nextID := int64(1)
	importLines := func(table, cat string, keep map[string]bool, majorSet map[string]bool, hasRef bool) error {
		cols := "fclass, name, geom"
		if hasRef {
			cols = "fclass, name, ref, geom"
		}
		rows, err := src.Query(fmt.Sprintf("SELECT %s FROM %s", cols, table))
		if err != nil {
			return err
		}
		defer rows.Close()
		tx, err := osmDB.Begin()
		if err != nil {
			return err
		}
		insL, _ := tx.Prepare(`INSERT INTO osm_lines(id,cat,major,fclass,name,ref,coords) VALUES(?,?,?,?,?,?,?)`)
		insR, _ := tx.Prepare(`INSERT INTO osm_lines_rtree(id,min_lon,max_lon,min_lat,max_lat) VALUES(?,?,?,?,?)`)
		n, batch := 0, 0
		flush := func() error {
			insL.Close()
			insR.Close()
			if err := tx.Commit(); err != nil {
				return err
			}
			tx, err = osmDB.Begin()
			if err != nil {
				return err
			}
			insL, _ = tx.Prepare(`INSERT INTO osm_lines(id,cat,major,fclass,name,ref,coords) VALUES(?,?,?,?,?,?,?)`)
			insR, _ = tx.Prepare(`INSERT INTO osm_lines_rtree(id,min_lon,max_lon,min_lat,max_lat) VALUES(?,?,?,?,?)`)
			batch = 0
			return nil
		}
		for rows.Next() {
			var fclass, name, ref sql.NullString
			var blob []byte
			if hasRef {
				if err := rows.Scan(&fclass, &name, &ref, &blob); err != nil {
					continue
				}
			} else {
				if err := rows.Scan(&fclass, &name, &blob); err != nil {
					continue
				}
			}
			if !keep[fclass.String] {
				continue
			}
			raw, err := gpkgToWKB(blob)
			if err != nil {
				continue
			}
			geom, err := wkb.Unmarshal(raw)
			if err != nil {
				continue
			}
			major := 0
			if majorSet != nil && majorSet[fclass.String] {
				major = 1
			}
			for _, ls := range geomToLineStrings(geom) {
				for _, chunk := range chunkLine(ls, osmChunkMaxPts) {
					if len(chunk) < 2 {
						continue
					}
					b := chunk.Bound()
					if _, err := insL.Exec(nextID, cat, major, fclass.String, name.String, ref.String, packCoords(chunk)); err != nil {
						continue
					}
					insR.Exec(nextID, b.Min[0], b.Max[0], b.Min[1], b.Max[1])
					nextID++
					n++
					batch++
				}
			}
			if batch >= 20000 {
				if err := flush(); err != nil {
					return err
				}
			}
		}
		insL.Close()
		insR.Close()
		if err := tx.Commit(); err != nil {
			return err
		}
		log.Printf("🛣️ imported %s → cat=%s: %d chunks", table, cat, n)
		return nil
	}

	importPoints := func(table, cat string, keep map[string]bool, stationSet map[string]bool, hasPop bool) error {
		cols := "fclass, name, geom"
		if hasPop {
			cols = "fclass, name, population, geom"
		}
		rows, err := src.Query(fmt.Sprintf("SELECT %s FROM %s", cols, table))
		if err != nil {
			return err
		}
		defer rows.Close()
		tx, err := osmDB.Begin()
		if err != nil {
			return err
		}
		insP, _ := tx.Prepare(`INSERT INTO osm_points(id,cat,station,fclass,name,pop,lon,lat) VALUES(?,?,?,?,?,?,?,?)`)
		insR, _ := tx.Prepare(`INSERT INTO osm_points_rtree(id,min_lon,max_lon,min_lat,max_lat) VALUES(?,?,?,?,?)`)
		n := 0
		for rows.Next() {
			var fclass, name sql.NullString
			var pop sql.NullInt64
			var blob []byte
			var err error
			if hasPop {
				err = rows.Scan(&fclass, &name, &pop, &blob)
			} else {
				err = rows.Scan(&fclass, &name, &blob)
			}
			if err != nil || !keep[fclass.String] {
				continue
			}
			raw, gerr := gpkgToWKB(blob)
			if gerr != nil {
				continue
			}
			geom, gerr := wkb.Unmarshal(raw)
			if gerr != nil {
				continue
			}
			pt, ok := geom.(orb.Point)
			if !ok {
				continue
			}
			station := 0
			if stationSet != nil && stationSet[fclass.String] {
				station = 1
			}
			if _, err := insP.Exec(nextID, cat, station, fclass.String, name.String, pop.Int64, pt[0], pt[1]); err != nil {
				continue
			}
			insR.Exec(nextID, pt[0], pt[0], pt[1], pt[1])
			nextID++
			n++
		}
		insP.Close()
		insR.Close()
		if err := tx.Commit(); err != nil {
			return err
		}
		log.Printf("🛣️ imported %s → cat=%s: %d points", table, cat, n)
		return nil
	}

	if err := importLines("gis_osm_roads_free", "road", osmDrivableClasses, osmMajorRoadClasses, true); err != nil {
		return err
	}
	if err := importLines("gis_osm_railways_free", "rail", osmRailClasses, nil, false); err != nil {
		return err
	}
	if err := importLines("gis_osm_waterways_free", "water", osmWaterLineClasses, nil, false); err != nil {
		return err
	}
	if err := importWaterAreas(src); err != nil {
		return err
	}
	if err := importOSMAreas(src); err != nil {
		return err
	}
	if err := importPoints("gis_osm_transport_free", "transit", osmTransitClasses, osmTrainStationClasses, false); err != nil {
		return err
	}
	if err := importPoints("gis_osm_places_free", "place", osmSettlementClasses, nil, true); err != nil {
		return err
	}

	osmDB.Exec(`INSERT OR REPLACE INTO osm_meta(key,value) VALUES('imported_at',?)`, time.Now().UTC().Format(time.RFC3339))
	osmDB.Exec(`INSERT OR REPLACE INTO osm_meta(key,value) VALUES('source','geofabrik austria.gpkg')`)
	log.Printf("🛣️ OSM import complete in %v", time.Since(start))
	osmMu.Lock()
	osmReady = true
	osmMu.Unlock()
	return nil
}

// importWaterAreas: lake/reservoir polygons — outer rings as 'water' lines.
func importWaterAreas(src *sql.DB) error {
	rows, err := src.Query(`SELECT fclass, name, geom FROM gis_osm_water_a_free`)
	if err != nil {
		return err
	}
	defer rows.Close()
	tx, err := osmDB.Begin()
	if err != nil {
		return err
	}
	var maxID int64
	osmDB.QueryRow(`SELECT COALESCE(MAX(id),0) FROM osm_lines`).Scan(&maxID)
	var maxPID int64
	osmDB.QueryRow(`SELECT COALESCE(MAX(id),0) FROM osm_points`).Scan(&maxPID)
	if maxPID > maxID {
		maxID = maxPID
	}
	nextID := maxID + 1
	insL, _ := tx.Prepare(`INSERT INTO osm_lines(id,cat,major,fclass,name,ref,coords) VALUES(?,?,0,?,?,'',?)`)
	insR, _ := tx.Prepare(`INSERT INTO osm_lines_rtree(id,min_lon,max_lon,min_lat,max_lat) VALUES(?,?,?,?,?)`)
	n, batch := 0, 0
	for rows.Next() {
		var fclass, name sql.NullString
		var blob []byte
		if err := rows.Scan(&fclass, &name, &blob); err != nil || !osmWaterAreaClasses[fclass.String] {
			continue
		}
		raw, gerr := gpkgToWKB(blob)
		if gerr != nil {
			continue
		}
		geom, gerr := wkb.Unmarshal(raw)
		if gerr != nil {
			continue
		}
		for _, ring := range geomToOuterRings(geom) {
			for _, chunk := range chunkLine(orb.LineString(ring), osmChunkMaxPts) {
				if len(chunk) < 2 {
					continue
				}
				b := chunk.Bound()
				if _, err := insL.Exec(nextID, "water", fclass.String, name.String, packCoords(chunk)); err != nil {
					continue
				}
				insR.Exec(nextID, b.Min[0], b.Max[0], b.Min[1], b.Max[1])
				nextID++
				n++
				batch++
			}
		}
		if batch >= 20000 {
			insL.Close()
			insR.Close()
			if err := tx.Commit(); err != nil {
				return err
			}
			tx, err = osmDB.Begin()
			if err != nil {
				return err
			}
			insL, _ = tx.Prepare(`INSERT INTO osm_lines(id,cat,major,fclass,name,ref,coords) VALUES(?,?,0,?,?,'',?)`)
			insR, _ = tx.Prepare(`INSERT INTO osm_lines_rtree(id,min_lon,max_lon,min_lat,max_lat) VALUES(?,?,?,?,?)`)
			batch = 0
		}
	}
	insL.Close()
	insR.Close()
	if err := tx.Commit(); err != nil {
		return err
	}
	log.Printf("🛣️ imported gis_osm_water_a_free → cat=water: %d ring chunks", n)
	return nil
}

func geomToLineStrings(g orb.Geometry) []orb.LineString {
	switch v := g.(type) {
	case orb.LineString:
		return []orb.LineString{v}
	case orb.MultiLineString:
		return []orb.LineString(v)
	}
	return nil
}

func geomToOuterRings(g orb.Geometry) []orb.Ring {
	switch v := g.(type) {
	case orb.Polygon:
		if len(v) > 0 {
			return []orb.Ring{v[0]}
		}
	case orb.MultiPolygon:
		var out []orb.Ring
		for _, p := range v {
			if len(p) > 0 {
				out = append(out, p[0])
			}
		}
		return out
	}
	return nil
}

func chunkLine(ls orb.LineString, maxPts int) []orb.LineString {
	if len(ls) <= maxPts {
		return []orb.LineString{ls}
	}
	var out []orb.LineString
	for start := 0; start < len(ls)-1; start += maxPts - 1 {
		end := start + maxPts
		if end > len(ls) {
			end = len(ls)
		}
		out = append(out, ls[start:end])
		if end == len(ls) {
			break
		}
	}
	return out
}
