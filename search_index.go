// search_index.go — the KG-level register: SQLite with the `kg` table (codes,
// names, admin hierarchy, bbox), plz_kg, kg_landuse aggregates, reference
// tables and the fts_admin name index. Seeded from data/kg_meta_dump.sql.gz;
// it feeds the in-memory KG bbox index (kg_bbox_index.go), /api/v1/kgs
// (kg_universe.go), land prices, Natura 2000 and toponym KG assignment.
//
// Tables: kg, plz_kg, kg_landuse, landuse_types, bundesland, index_meta
// FTS5:   fts_admin (kg_code, kg_name, gemeinde_name, district_name, state_name)
package main

import (
	"database/sql"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const (
	SearchIndexPath    = "data/search_index.db"
	SearchIndexVersion = 2
)

// SearchIndex holds the SQLite connection for the search index.
type SearchIndex struct {
	db *sql.DB
	mu sync.RWMutex
}

var globalSearchIndex *SearchIndex

// searchIndexMaxConns bounds the database/sql pool for the search index.
//
// WAL gives us unlimited concurrent readers at the SQLite level, so the only
// real cost per connection is an fd and a private page cache. The previous
// value of 4 was far too low and turned any burst of concurrent requests into
// an apparent server-wide hang (see OpenSearchIndex).
//
// The ceiling is memory, not SQLite: `_cache_size` is PER CONNECTION, so pool
// size × cache size is the real budget. 24 × 8 MB = ~192 MB, which is plenty of
// read concurrency for viewport bursts without the multi-GB RSS a large pool
// combined with the old 64 MB-per-connection cache would have produced.
const (
	searchIndexMaxConns = 24
	// searchIndexCacheKB is the per-connection page cache (negative = KiB).
	searchIndexCacheKB = 8000
)

// dbWriteMu serializes the few background writers on this DB (kg bbox
// updates) so their transactions never overlap and trip SQLite's
// single-writer lock. Reads do NOT take this mutex.
var dbWriteMu sync.Mutex

// InitSearchIndex opens (or creates) the search index database.
func InitSearchIndex() error {
	idx, err := OpenSearchIndex(SearchIndexPath)
	if err != nil {
		return err
	}
	globalSearchIndex = idx

	// Log stats
	stats := idx.Stats()
	log.Printf("🔍 Search index ready: %v KGs, 0 parcels", stats["kg_count"])

	// Load the in-memory KG bbox index up-front so the first spatial request
	// doesn't pay for it (and never needs a DB connection for KG lookup).
	LoadKGBBoxIndex()
	return nil
}

func OpenSearchIndex(path string) (*SearchIndex, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	// busy_timeout makes concurrent writers wait for the lock instead of
	// failing with "database is locked".
	dsn := fmt.Sprintf("%s?_journal_mode=WAL&_synchronous=NORMAL&_cache_size=-%d&_foreign_keys=ON&_busy_timeout=15000",
		path, searchIndexCacheKB)
	// sqlite3_tuned adds mmap_size + temp_store=MEMORY per connection
	// (see sqlite_driver.go) — DSN params can't express those.
	registerTunedSQLite()
	db, err := sql.Open(sqliteTunedDriver, dsn)
	if err != nil {
		return nil, fmt.Errorf("open search index: %w", err)
	}
	// Connection pool sizing: WAL allows unlimited concurrent readers; the Go
	// pool is the only cap, so keep it generous (see searchIndexMaxConns).
	db.SetMaxOpenConns(searchIndexMaxConns)
	db.SetMaxIdleConns(searchIndexMaxConns)
	db.SetConnMaxIdleTime(5 * time.Minute)
	idx := &SearchIndex{db: db}
	if err := idx.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate search index: %w", err)
	}
	return idx, nil
}

func (idx *SearchIndex) Close() error {
	if idx.db != nil {
		return idx.db.Close()
	}
	return nil
}

func (idx *SearchIndex) DB() *sql.DB {
	return idx.db
}

// ══════════════════════════════════════════════════════════════════════
// Schema
// ══════════════════════════════════════════════════════════════════════
func (idx *SearchIndex) migrate() error {
	statements := []string{
		// ── Per-KG bookkeeping (epoch of the seed) ──
		`CREATE TABLE IF NOT EXISTS index_meta (
			kg_code       TEXT PRIMARY KEY,
			checksum      TEXT NOT NULL,
			file_size     INTEGER,
			indexed_at    TEXT NOT NULL,
			indexing_ms   INTEGER,
			parcel_count  INTEGER DEFAULT 0,
			building_count INTEGER DEFAULT 0,
			landuse_count  INTEGER DEFAULT 0,
			status        TEXT DEFAULT 'ok',
			version       INTEGER NOT NULL DEFAULT 1
		)`,

		// ── Reference tables ──
		`CREATE TABLE IF NOT EXISTS bundesland (
			code TEXT PRIMARY KEY,
			name TEXT NOT NULL
		)`,

		`CREATE TABLE IF NOT EXISTS landuse_types (
			code TEXT PRIMARY KEY,
			type_name TEXT NOT NULL,
			abbr TEXT NOT NULL
		)`,
		// ── KG (Katastralgemeinde) ──
		`CREATE TABLE IF NOT EXISTS kg (
			kg_code         TEXT PRIMARY KEY,
			kg_name         TEXT,
			gemeinde_code   TEXT,
			gemeinde_name   TEXT,
			district_code   TEXT,
			district_name   TEXT,
			state_code      TEXT,
			state_name      TEXT,
			min_lon         REAL,
			min_lat         REAL,
			max_lon         REAL,
			max_lat         REAL,
			parcel_count    INTEGER DEFAULT 0,
			building_count  INTEGER DEFAULT 0,
			footprint_count INTEGER DEFAULT 0,
			label_count     INTEGER DEFAULT 0,
			landuse_count   INTEGER DEFAULT 0,
			total_area_sqm  REAL DEFAULT 0,
			updated_at      TEXT
		)`,

		// ── PLZ ↔ KG ──
		`CREATE TABLE IF NOT EXISTS plz_kg (
			plz             TEXT NOT NULL,
			kg_code         TEXT NOT NULL,
			PRIMARY KEY (plz, kg_code)
		)`,
		// ── Landuse aggregates per KG ──
		`CREATE TABLE IF NOT EXISTS kg_landuse (
			kg_code         TEXT NOT NULL,
			landuse_code    TEXT NOT NULL,
			landuse_type    TEXT,
			landuse_abbr    TEXT,
			polygon_count   INTEGER DEFAULT 0,
			point_count     INTEGER DEFAULT 0,
			total_area_sqm  REAL DEFAULT 0,
			PRIMARY KEY (kg_code, landuse_code)
		)`,
		// ── Indexes ──
		`CREATE INDEX IF NOT EXISTS idx_plz_kg_plz ON plz_kg(plz)`,
		`CREATE INDEX IF NOT EXISTS idx_plz_kg_kg ON plz_kg(kg_code)`,

		`CREATE INDEX IF NOT EXISTS idx_kg_landuse_code ON kg_landuse(landuse_code)`,
		`CREATE INDEX IF NOT EXISTS idx_kg_landuse_abbr ON kg_landuse(landuse_abbr)`,

		`CREATE INDEX IF NOT EXISTS idx_kg_gemeinde ON kg(gemeinde_code)`,
		`CREATE INDEX IF NOT EXISTS idx_kg_district ON kg(district_code)`,
		`CREATE INDEX IF NOT EXISTS idx_kg_state ON kg(state_code)`,
		`CREATE INDEX IF NOT EXISTS idx_kg_bbox ON kg(min_lon, max_lon, min_lat, max_lat)`,
		// ── FTS5 over admin names ──
		`CREATE VIRTUAL TABLE IF NOT EXISTS fts_admin USING fts5(
			kg_code, kg_name, gemeinde_name, district_name, state_name,
			tokenize="unicode61 remove_diacritics 2"
		)`}

	for _, stmt := range statements {
		if _, err := idx.db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate: %w\n%s", err, stmt)
		}
	}

	idx.seedReferenceData()
	return nil
}

func (idx *SearchIndex) seedReferenceData() {
	// Bundesländer
	for code, name := range map[string]string{
		"1": "Burgenland", "2": "Kärnten", "3": "Niederösterreich",
		"4": "Oberösterreich", "5": "Salzburg", "6": "Steiermark",
		"7": "Tirol", "8": "Vorarlberg", "9": "Wien",
	} {
		idx.db.Exec("INSERT OR IGNORE INTO bundesland (code, name) VALUES (?, ?)", code, name)
	}

	// Landuse types
	for code, desc := range nutzungssymbolCodes {
		abbr := ""
		if a, ok := nutzungAbbreviations[code]; ok {
			abbr = a
		}
		idx.db.Exec("INSERT OR REPLACE INTO landuse_types (code, type_name, abbr) VALUES (?, ?, ?)",
			code, desc, abbr)
	}
}

// ══════════════════════════════════════════════════════════════════════
// Stats
// ══════════════════════════════════════════════════════════════════════

var (
	statsMemoMu    sync.Mutex
	statsMemoVal   map[string]interface{}
	statsMemoStamp time.Time
)

const statsMemoTTL = 5 * time.Minute

// InvalidateStatsMemo drops the cached Stats() snapshot.
func InvalidateStatsMemo() {
	statsMemoMu.Lock()
	statsMemoVal = nil
	statsMemoMu.Unlock()
}

func (idx *SearchIndex) Stats() map[string]interface{} {
	statsMemoMu.Lock()
	if statsMemoVal != nil && time.Since(statsMemoStamp) < statsMemoTTL {
		out := make(map[string]interface{}, len(statsMemoVal))
		for k, v := range statsMemoVal {
			out[k] = v
		}
		statsMemoMu.Unlock()
		return out
	}
	statsMemoMu.Unlock()

	stats := idx.computeStats()

	statsMemoMu.Lock()
	statsMemoVal, statsMemoStamp = stats, time.Now()
	statsMemoMu.Unlock()

	out := make(map[string]interface{}, len(stats))
	for k, v := range stats {
		out[k] = v
	}
	return out
}
func (idx *SearchIndex) computeStats() map[string]interface{} {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	stats := map[string]interface{}{}
	for _, q := range []struct {
		name, query string
	}{
		{"kg_count", "SELECT COUNT(*) FROM kg"},
		{"plz_count", "SELECT COUNT(DISTINCT plz) FROM plz_kg"},
		{"landuse_types", "SELECT COUNT(DISTINCT landuse_code) FROM kg_landuse"},
		{"total_area_sqm", "SELECT COALESCE(SUM(total_area_sqm),0) FROM kg"},
		{"indexed_kgs", "SELECT COUNT(*) FROM index_meta"},
		{"states_covered", "SELECT COUNT(DISTINCT state_code) FROM kg WHERE state_code != ''"},
		{"districts_covered", "SELECT COUNT(DISTINCT district_code) FROM kg WHERE district_code != ''"},
		{"gemeinden_covered", "SELECT COUNT(DISTINCT gemeinde_code) FROM kg WHERE gemeinde_code != ''"},
	} {
		var val float64
		idx.db.QueryRow(q.query).Scan(&val)
		if q.name == "total_area_sqm" {
			stats[q.name] = val
			stats["total_area_km2"] = math.Round(val/1e6*100) / 100
		} else {
			stats[q.name] = int(val)
		}
	}
	return stats
}
