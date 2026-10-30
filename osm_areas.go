package main

// OSM AREA features (water polygons) — served as real (Multi)Polygons.
//
// Feedback #16 ({MAP_NAME}): /api/v1/osm/geometry?cat=water served
// riverbank / lake polygons as ≤24-vertex LineString ring CHUNKS (that is the
// right shape for the nearest-distance R-tree, and the wrong shape for a map).
// Chunks outside the request bbox are dropped by the R-tree, so a KG-bbox
// request came back as open chains that cannot be closed into the river
// polygon — the Danube rendered as a few stray strokes.
//
// This file adds a second store, osm_areas, keeping every water polygon whole
// (WKB, all rings, holes honoured) with its own R*Tree, and a serving path that
// clips each polygon to the request bbox (Sutherland–Hodgman against the
// rectangle, exact and cheap) so a viewport always receives closed, fillable
// polygons. The line chunks stay untouched: osm_parcel_metrics distances and
// every existing client keep working.
//
//   ./umfeld-at -osm-import-areas       # areas only, keeps lines/points/metrics
//   GET /api/v1/osm/geometry?kg=12105&cat=water_area          (clipped to bbox)
//   GET /api/v1/osm/geometry?bbox=...&cat=water_area&clip=0   (whole polygons)

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	"github.com/ctessum/polyclip-go"
	"github.com/paulmach/orb"
	"github.com/paulmach/orb/encoding/wkb"
	"github.com/paulmach/orb/planar"
)

// osmAreaClasses — polygon fclasses of gis_osm_water_a_free we keep.
// "wetland" is deliberately included: Au/Moor is what a settlement game and a
// flood analysis both want next to the river itself.
var osmAreaClasses = map[string]string{
	"water": "water", "riverbank": "water", "reservoir": "water",
	"river": "water", "dock": "water", "wetland": "water",
}

func createOSMAreaTables(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS osm_areas(
			id INTEGER PRIMARY KEY,
			cat TEXT NOT NULL,          -- water
			fclass TEXT, name TEXT,
			area_sqm REAL,
			wkb BLOB NOT NULL           -- Polygon or MultiPolygon, all rings
		)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS osm_areas_rtree USING rtree(
			id, min_lon, max_lon, min_lat, max_lat)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return err
		}
	}
	return nil
}

// RunOSMImportAreas (re)builds osm_areas from gis_osm_water_a_free. Independent
// of the line/point import so it can run against a live DB without wiping
// anything a request path reads.
func RunOSMImportAreas() error {
	if osmDB == nil {
		InitOSMProximity()
	}
	if osmDB == nil {
		return fmt.Errorf("osm_proximity DB unavailable")
	}
	src, err := sql.Open("sqlite3", "file:"+osmGPKGPath+"?mode=ro")
	if err != nil {
		return err
	}
	defer src.Close()
	return importOSMAreas(src)
}

func importOSMAreas(src *sql.DB) error {
	start := time.Now()
	if err := createOSMAreaTables(osmDB); err != nil {
		return err
	}
	for _, t := range []string{"osm_areas_rtree", "osm_areas"} {
		if _, err := osmDB.Exec("DELETE FROM " + t); err != nil {
			return err
		}
	}
	rows, err := src.Query(`SELECT fclass, name, geom FROM gis_osm_water_a_free`)
	if err != nil {
		return err
	}
	defer rows.Close()

	tx, err := osmDB.Begin()
	if err != nil {
		return err
	}
	prep := func(tx *sql.Tx) (*sql.Stmt, *sql.Stmt) {
		a, _ := tx.Prepare(`INSERT INTO osm_areas(id,cat,fclass,name,area_sqm,wkb) VALUES(?,?,?,?,?,?)`)
		r, _ := tx.Prepare(`INSERT INTO osm_areas_rtree(id,min_lon,max_lon,min_lat,max_lat) VALUES(?,?,?,?,?)`)
		return a, r
	}
	insA, insR := prep(tx)
	var id int64
	n, batch := 0, 0
	byClass := map[string]int{}
	for rows.Next() {
		var fclass, name sql.NullString
		var blob []byte
		if err := rows.Scan(&fclass, &name, &blob); err != nil {
			continue
		}
		cat, ok := osmAreaClasses[fclass.String]
		if !ok {
			continue
		}
		raw, gerr := gpkgToWKB(blob)
		if gerr != nil {
			continue
		}
		g, gerr := wkb.Unmarshal(raw)
		if gerr != nil {
			continue
		}
		var mp orb.MultiPolygon
		switch v := g.(type) {
		case orb.Polygon:
			mp = orb.MultiPolygon{v}
		case orb.MultiPolygon:
			mp = v
		default:
			continue
		}
		mp = orb.MultiPolygon(classifyRings(allRings(mp)))
		if len(mp) == 0 {
			continue
		}
		out, err := marshalGeomWKB(mp)
		if err != nil {
			continue
		}
		b := mp.Bound()
		area := geodesicAreaMultiPolygon(mp)
		id++
		if _, err := insA.Exec(id, cat, fclass.String, name.String, area, out); err != nil {
			continue
		}
		insR.Exec(id, b.Min[0], b.Max[0], b.Min[1], b.Max[1])
		n++
		batch++
		byClass[fclass.String]++
		if batch >= 10000 {
			insA.Close()
			insR.Close()
			if err := tx.Commit(); err != nil {
				return err
			}
			if tx, err = osmDB.Begin(); err != nil {
				return err
			}
			insA, insR = prep(tx)
			batch = 0
		}
	}
	insA.Close()
	insR.Close()
	if err := tx.Commit(); err != nil {
		return err
	}
	osmDB.Exec(`INSERT OR REPLACE INTO osm_meta(key,value) VALUES('areas_imported_at',?)`, time.Now().UTC().Format(time.RFC3339))
	log.Printf("🌊 imported gis_osm_water_a_free → osm_areas: %d polygons %v in %v", n, byClass, time.Since(start))
	return nil
}

// geodesicAreaMultiPolygon: planar.Area on a local metre projection at the
// polygon's latitude — good to <0.1 % at this scale, same as the geom caches.
func geodesicAreaMultiPolygon(mp orb.MultiPolygon) float64 {
	if len(mp) == 0 {
		return 0
	}
	c := mp.Bound().Center()
	mLon, mLat := metresPerDeg(c[1])
	tot := 0.0
	for _, p := range mp {
		q := make(orb.Polygon, len(p))
		for i, r := range p {
			rr := make(orb.Ring, len(r))
			for j, pt := range r {
				rr[j] = orb.Point{(pt[0] - c[0]) * mLon, (pt[1] - c[1]) * mLat}
			}
			q[i] = rr
		}
		tot += planar.Area(q)
	}
	return tot
}

// OSMAreasReady reports whether osm_areas has been imported.
func OSMAreasReady() bool {
	if osmDB == nil {
		return false
	}
	var n int
	if err := osmDB.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='osm_areas'`).Scan(&n); err != nil || n == 0 {
		return false
	}
	var c int
	osmDB.QueryRow(`SELECT count(*) FROM osm_areas_rtree WHERE id < 2`).Scan(&c)
	return c > 0
}

// OSMArea is one served polygon feature.
type OSMArea struct {
	ID      int64
	Cat     string
	Fclass  string
	Name    string
	AreaSqm float64
	Geom    orb.MultiPolygon
	Clipped bool
}

// OSMAreasInBBox returns the water polygons intersecting the bbox. With clip,
// each polygon is cut to the rectangle so the client always gets closed rings
// (a river crossing the viewport edge becomes a polygon ending at the edge).
func OSMAreasInBBox(west, south, east, north float64, cats map[string]bool, clip bool, limit int) ([]OSMArea, bool, error) {
	if !OSMAreasReady() {
		return nil, false, fmt.Errorf("osm_areas not imported (run -osm-import-areas)")
	}
	rows, err := osmDB.Query(`
		SELECT a.id, a.cat, a.fclass, a.name, a.area_sqm, a.wkb
		FROM osm_areas_rtree rt JOIN osm_areas a ON a.id = rt.id
		WHERE rt.max_lon >= ? AND rt.min_lon <= ? AND rt.max_lat >= ? AND rt.min_lat <= ?
		ORDER BY a.area_sqm DESC`, west, east, south, north)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	rect := orb.Bound{Min: orb.Point{west, south}, Max: orb.Point{east, north}}
	var out []OSMArea
	truncated := false
	for rows.Next() {
		if len(out) >= limit {
			truncated = true
			break
		}
		var a OSMArea
		var fclass, name sql.NullString
		var raw []byte
		if err := rows.Scan(&a.ID, &a.Cat, &fclass, &name, &a.AreaSqm, &raw); err != nil {
			continue
		}
		if !cats[a.Cat] {
			continue
		}
		a.Fclass, a.Name = fclass.String, name.String
		mp, err := unmarshalGeomWKB(raw)
		if err != nil || len(mp) == 0 {
			continue
		}
		if clip {
			b := mp.Bound()
			if !(b.Min[0] >= west && b.Max[0] <= east && b.Min[1] >= south && b.Max[1] <= north) {
				mp = clipMultiPolygonToRect(mp, rect)
				a.Clipped = true
				if len(mp) == 0 {
					continue
				}
			}
		}
		a.Geom = mp
		out = append(out, a)
	}
	return out, truncated, nil
}

// clipMultiPolygonToRect clips every ring (shells and holes alike) to the
// rectangle and re-derives shell/hole nesting from the result. Clipping a hole
// independently of its shell is sound: the clipped hole is still inside the
// clipped shell, and classifyRings reassigns parents by containment.
func clipMultiPolygonToRect(mp orb.MultiPolygon, rect orb.Bound) orb.MultiPolygon {
	var pc polyclip.Polygon
	for _, p := range mp {
		pc = append(pc, orbToPolyclip(p)...)
	}
	return polyclipToOrbMulti(rectClipPolyclip(pc, rect))
}

// ── parcel-aligned water (feedback #16 option c) ─────────────────────────────
//
// OSM shorelines are digitised independently of the cadastre, so an OSM river
// polygon drawn over parcel polygons never coincides with a parcel edge: it
// overshoots onto the bank parcel here and leaves a dry strip of riverbed
// there. A client that colours PARCELS (a settlement game, a per-Grundstück
// analysis) needs the water expressed in parcel terms. WaterParcelsInBBox
// intersects every cached parcel polygon with the OSM water polygons and
// returns, per parcel, the water fraction plus either the intersection pieces
// (which by construction end exactly on parcel edges) or the whole parcel
// polygon once its water share passes a threshold.
