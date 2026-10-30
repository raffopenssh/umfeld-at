// kg_bbox_index.go — in-memory KG bounding-box index.
//
// Why this exists
// ───────────────
// Several hot paths needed "which KGs cover this point / viewport?" and each
// answered it with its own SQL query:
//
//   - KGCodesInBBox     (api_spatial.go)          — once per bbox request
//   - KGsForPoint       (footprint_geom_cache.go) — once per POINT
//   - spatialPointsHandler                        — once per POINT (up to 1000)
//
// Combined with a 4-connection pool this was the mechanism behind the repeated
// API-wide wedges: a single POST /spatial/points with N points fired N queries,
// each grabbing a pool connection, starving every other request until it
// finished. Static handlers stayed fast, so the process looked alive while
// every DB-backed endpoint hung.
//
// There are only ~7,850 KGs and their bboxes never change while the process
// runs (new KGs only appear on index writes, which bump the generation). So we
// load them once into a slice and answer these queries from RAM in
// microseconds, with zero DB connections.
package main

import (
	"log"
	"strings"
	"sync"
	"time"
)

type kgBox struct {
	code                           string
	minLon, minLat, maxLon, maxLat float64
}

var (
	kgBoxMu       sync.RWMutex
	kgBoxes       []kgBox
	kgBoxLoaded   bool
	kgBoxLoadedAt time.Time
)

// kgBoxReloadInterval bounds staleness after a KG is (re)indexed. Cheap: one
// scan of ~7.8k rows.
const kgBoxReloadInterval = 5 * time.Minute

// LoadKGBBoxIndex (re)loads the in-memory KG bbox index from SQLite.
func LoadKGBBoxIndex() {
	if globalSearchIndex == nil {
		return
	}
	rows, err := globalSearchIndex.DB().Query(
		`SELECT kg_code, min_lon, min_lat, max_lon, max_lat FROM kg
		 WHERE min_lon IS NOT NULL AND max_lon IS NOT NULL`)
	if err != nil {
		log.Printf("🗺  KG bbox index: %v", err)
		return
	}
	defer rows.Close()

	out := make([]kgBox, 0, 8000)
	for rows.Next() {
		var b kgBox
		if rows.Scan(&b.code, &b.minLon, &b.minLat, &b.maxLon, &b.maxLat) == nil {
			out = append(out, b)
		}
	}

	kgBoxMu.Lock()
	kgBoxes = out
	kgBoxLoaded = true
	kgBoxLoadedAt = time.Now()
	kgBoxMu.Unlock()

	log.Printf("🗺  KG bbox index: %d KGs in memory", len(out))
}

// InvalidateKGBBoxIndex forces a reload on next use (call after indexing KGs).
func InvalidateKGBBoxIndex() {
	kgBoxMu.Lock()
	kgBoxLoaded = false
	kgBoxMu.Unlock()
}

// kgBoxSnapshot returns the current index, loading/refreshing it if needed.
func kgBoxSnapshot() []kgBox {
	kgBoxMu.RLock()
	loaded := kgBoxLoaded
	stale := time.Since(kgBoxLoadedAt) > kgBoxReloadInterval
	boxes := kgBoxes
	kgBoxMu.RUnlock()

	if loaded && !stale {
		return boxes
	}
	LoadKGBBoxIndex()

	kgBoxMu.RLock()
	boxes = kgBoxes
	kgBoxMu.RUnlock()
	return boxes
}

// KGCodesInBBoxMem returns KG codes whose bbox intersects the viewport.
func KGCodesInBBoxMem(west, south, east, north float64) []string {
	var out []string
	for _, b := range kgBoxSnapshot() {
		if b.minLon <= east && b.maxLon >= west && b.minLat <= north && b.maxLat >= south {
			out = append(out, b.code)
		}
	}
	return out
}

// KGCodesForPointMem returns KG codes whose bbox covers the point, padded by
// kgPointCandidatePadDeg on every side.
//
// The pad is not optional: the kg bbox is built from parcel CENTROIDS, so the
// outer half of every edge parcel (and every building on it) lies OUTSIDE its
// own KG's bbox. An unpadded lookup named only the neighbouring KG, which
// meant the true KG was never scheduled for warming and a cold point on a KG
// border answered matched:false / an empty footprint list forever.
// Exact containment is still decided against the polygons, so extra
// candidates only cost a few more R-tree rows to test.
func KGCodesForPointMem(lon, lat float64) []string {
	return KGCodesNearPointMem(lon, lat, kgPointCandidatePadDeg)
}

// kgPointCandidatePadDeg pads a point's KG candidate lookup so a KG whose
// centroid-derived bbox stops just short of the point is still a candidate
// (~0.005° ≈ 370 m E–W at 47° N, 550 m N–S — larger than any realistic
// edge-parcel half-width for a building; large alpine edge parcels are covered
// by LookupParcelContaining / FootprintsContaining also trusting rows from
// any fully-built non-candidate KG).
const kgPointCandidatePadDeg = 0.005

// KGCodesNearPointMem returns KG codes whose bbox, grown by pad degrees on
// every side, covers the point. pad=0 is the strict centroid-bbox test.
func KGCodesNearPointMem(lon, lat, pad float64) []string {
	return KGCodesInBBoxMem(lon-pad, lat-pad, lon+pad, lat+pad)
}

// ─── Name → KG-code resolution ───────────────────────────────────────────
//
// Why
// ───
// `/api/v1/query?state=Steiermark` took **9 s regardless of ?limit**. The
// filter was `k.state_name LIKE '%Steiermark%'`, which no index can serve, so
// SQLite scanned all 10.1M `parcels` rows joined to `kg` just to find the first
// 100 — the ORDER BY area could not use idx_parcels_area either, because the
// LIKE had to be evaluated per row.
//
// The `kg` table is only ~7.8k rows and is already fully resident in the bbox
// index. Resolving the name filter there first turns the predicate into
// `p.kg_code IN (…)`, which is an index seek per KG. Same result, no scan.
//
// Same treatment for gemeinde and district names.

type kgNameRow struct {
	code         string
	kgName       string
	gemeindeCode string
	gemeindeName string
	districtCode string
	districtName string
	stateName    string
	stateCode    string
}

var (
	kgNameMu       sync.RWMutex
	kgNameRows     []kgNameRow
	kgNameLoaded   bool
	kgNameLoadedAt time.Time
)

// LoadKGNameIndex (re)loads the in-memory KG name index.
func LoadKGNameIndex() {
	if globalSearchIndex == nil {
		return
	}
	rows, err := globalSearchIndex.DB().Query(
		`SELECT kg_code, kg_name, gemeinde_code, gemeinde_name,
		        district_code, district_name, state_name, COALESCE(state_code,'') FROM kg`)
	if err != nil {
		log.Printf("🗺  KG name index: %v", err)
		return
	}
	defer rows.Close()
	out := make([]kgNameRow, 0, 8000)
	for rows.Next() {
		var r kgNameRow
		if rows.Scan(&r.code, &r.kgName, &r.gemeindeCode, &r.gemeindeName,
			&r.districtCode, &r.districtName, &r.stateName, &r.stateCode) == nil {
			out = append(out, r)
		}
	}
	kgNameMu.Lock()
	kgNameRows = out
	kgNameLoaded = true
	kgNameLoadedAt = time.Now()
	kgNameMu.Unlock()
	log.Printf("🗺  KG name index: %d KGs in memory", len(out))
}

func kgNameSnapshot() []kgNameRow {
	kgNameMu.RLock()
	loaded := kgNameLoaded
	stale := time.Since(kgNameLoadedAt) > kgBoxReloadInterval
	rows := kgNameRows
	kgNameMu.RUnlock()
	if loaded && !stale {
		return rows
	}
	LoadKGNameIndex()
	kgNameMu.RLock()
	rows = kgNameRows
	kgNameMu.RUnlock()
	return rows
}

// KGCodeFilter describes an admin-name/code filter to resolve.
type KGCodeFilter struct {
	Gemeinde string // numeric gemeinde_code, or a name substring
	District string // district name substring (or numeric district_code)
	State    string // state name substring (or numeric state_code, 1 digit)
}

// Empty reports whether nothing needs resolving.
func (f KGCodeFilter) Empty() bool {
	return f.Gemeinde == "" && f.District == "" && f.State == ""
}

// ResolveKGCodes returns the KG codes matching every non-empty part of f
// (AND semantics, case-insensitive substring match on names, exact on codes).
//
// A nil slice with ok=false means the index isn't loaded and the caller should
// fall back to SQL. A non-nil EMPTY slice means "no KG matches" — the caller
// should short-circuit to zero results.
func ResolveKGCodes(f KGCodeFilter) (codes []string, ok bool) {
	if f.Empty() {
		return nil, false
	}
	rows := kgNameSnapshot()
	if len(rows) == 0 {
		return nil, false
	}
	gem := strings.ToLower(f.Gemeinde)
	dis := strings.ToLower(f.District)
	sta := strings.ToLower(f.State)
	gemIsCode := f.Gemeinde != "" && isNumericCode(f.Gemeinde)
	disIsCode := f.District != "" && isNumericCode(f.District)
	staIsCode := f.State != "" && isNumericCode(f.State)

	codes = make([]string, 0, 64)
	for _, r := range rows {
		if f.Gemeinde != "" {
			if gemIsCode {
				if r.gemeindeCode != f.Gemeinde {
					continue
				}
			} else if !strings.Contains(strings.ToLower(r.gemeindeName), gem) {
				continue
			}
		}
		if f.District != "" {
			if disIsCode {
				if r.districtCode != f.District {
					continue
				}
			} else if !strings.Contains(strings.ToLower(r.districtName), dis) {
				continue
			}
		}
		if f.State != "" {
			if staIsCode {
				if r.stateCode != f.State {
					continue
				}
			} else if !strings.Contains(strings.ToLower(r.stateName), sta) {
				continue
			}
		}
		codes = append(codes, r.code)
	}
	return codes, true
}

// maxInlineKGCodes bounds how many codes we're willing to inline as bound
// parameters. SQLite's default SQLITE_MAX_VARIABLE_NUMBER is 32766 and every
// KG in the country is ~7.8k, so this is generous; above it we fall back to
// the LIKE predicate rather than risk a "too many variables" error.
const maxInlineKGCodes = 20000

// KGBBoxMem returns the stored bbox of one KG from the in-memory index.
func KGBBoxMem(code string) (minLon, minLat, maxLon, maxLat float64, ok bool) {
	kgBoxMu.RLock()
	defer kgBoxMu.RUnlock()
	for i := range kgBoxes {
		if kgBoxes[i].code == code {
			b := kgBoxes[i]
			return b.minLon, b.minLat, b.maxLon, b.maxLat, true
		}
	}
	return 0, 0, 0, 0, false
}
