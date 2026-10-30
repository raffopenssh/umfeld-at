-- =============================================================================
-- Austrian Cadastre Search Index — SQLite Schema
-- =============================================================================
-- Optimised for: full-text search, exact lookups, range/bbox queries,
-- combined filters, aggregation, and incremental (checksum-based) updates.
--
-- Expected scale: ~1 800 KGs, ~3M parcels, ~500K buildings, ~15M landuse rows
-- Target file size: 1–2 GB (no geometry stored; centroids only)
-- =============================================================================

PRAGMA journal_mode = WAL;          -- concurrent reads during writes
PRAGMA synchronous  = NORMAL;       -- safe with WAL
PRAGMA foreign_keys = ON;
PRAGMA page_size    = 8192;         -- better for large tables

-- =============================================================================
-- 0. SCHEMA VERSION & GLOBAL METADATA
-- =============================================================================

CREATE TABLE IF NOT EXISTS schema_info (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
INSERT OR REPLACE INTO schema_info VALUES ('schema_version', '1.0.0');
INSERT OR REPLACE INTO schema_info VALUES ('created_at', datetime('now'));

-- =============================================================================
-- 1. REFERENCE / LOOKUP TABLES  (small, rarely updated)
-- =============================================================================

-- Austrian federal states (Bundesländer) — derived from gemeinde_code prefix
CREATE TABLE IF NOT EXISTS bundesland (
    code  TEXT PRIMARY KEY,   -- '1'..'9'
    name  TEXT NOT NULL
);
INSERT OR REPLACE INTO bundesland VALUES ('1', 'Burgenland');
INSERT OR REPLACE INTO bundesland VALUES ('2', 'Kärnten');
INSERT OR REPLACE INTO bundesland VALUES ('3', 'Niederösterreich');
INSERT OR REPLACE INTO bundesland VALUES ('4', 'Oberösterreich');
INSERT OR REPLACE INTO bundesland VALUES ('5', 'Salzburg');
INSERT OR REPLACE INTO bundesland VALUES ('6', 'Steiermark');
INSERT OR REPLACE INTO bundesland VALUES ('7', 'Tirol');
INSERT OR REPLACE INTO bundesland VALUES ('8', 'Vorarlberg');
INSERT OR REPLACE INTO bundesland VALUES ('9', 'Wien');

-- Landuse code reference (codes 40–97)
CREATE TABLE IF NOT EXISTS landuse_type (
    code  TEXT PRIMARY KEY,   -- e.g. '41', '56'
    type  TEXT NOT NULL,      -- e.g. 'Baufläche (befestigt) - B(bf)'
    abbr  TEXT NOT NULL       -- e.g. 'B(bf)', 'W'
);

-- =============================================================================
-- 2. ADMINISTRATIVE HIERARCHY
-- =============================================================================

-- Gemeinde (municipality) — ~2 100 rows
CREATE TABLE IF NOT EXISTS gemeinde (
    gemeinde_code  TEXT PRIMARY KEY,  -- 5-digit, e.g. '50614'
    gemeinde_name  TEXT NOT NULL,
    bundesland     TEXT NOT NULL      -- derived: substr(gemeinde_code,1,1)
        REFERENCES bundesland(code)
);

CREATE INDEX IF NOT EXISTS idx_gemeinde_name  ON gemeinde(gemeinde_name);
CREATE INDEX IF NOT EXISTS idx_gemeinde_bland ON gemeinde(bundesland);

-- Katastralgemeinde (KG) — ~1 800 rows
CREATE TABLE IF NOT EXISTS katastralgemeinde (
    kg_code        TEXT PRIMARY KEY,  -- 5-digit, e.g. '75414'
    kg_name        TEXT,              -- may be NULL for some KGs
    gemeinde_code  TEXT NOT NULL REFERENCES gemeinde(gemeinde_code),
    -- bounding box (WGS84)
    min_lon  REAL,
    min_lat  REAL,
    max_lon  REAL,
    max_lat  REAL,
    -- summary counts (updated during indexing)
    parcel_count            INTEGER DEFAULT 0,
    building_count          INTEGER DEFAULT 0,
    building_footprint_count INTEGER DEFAULT 0,
    label_count             INTEGER DEFAULT 0,
    landuse_count           INTEGER DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_kg_gemeinde ON katastralgemeinde(gemeinde_code);
CREATE INDEX IF NOT EXISTS idx_kg_name     ON katastralgemeinde(kg_name);
-- Bbox queries: WHERE min_lon <= ? AND max_lon >= ? AND min_lat <= ? AND max_lat >= ?
CREATE INDEX IF NOT EXISTS idx_kg_bbox     ON katastralgemeinde(min_lon, max_lon, min_lat, max_lat);

-- Gemeinde ↔ PLZ mapping (M:N)
CREATE TABLE IF NOT EXISTS gemeinde_plz (
    gemeinde_code TEXT NOT NULL REFERENCES gemeinde(gemeinde_code),
    plz           TEXT NOT NULL,  -- 4-digit postal code
    PRIMARY KEY (gemeinde_code, plz)
);

CREATE INDEX IF NOT EXISTS idx_plz_gemeinde ON gemeinde_plz(plz);

-- =============================================================================
-- 3. PARCELS  (core entity — ~3M rows)
-- =============================================================================

CREATE TABLE IF NOT EXISTS parcel (
    -- Natural PK: "kg_code-gnr", e.g. "75414-1314/1"
    parcel_id     TEXT PRIMARY KEY,
    kg_code       TEXT NOT NULL REFERENCES katastralgemeinde(kg_code),
    gnr           TEXT NOT NULL,     -- Grundstücksnummer, e.g. '1314/1'
    ez            TEXT,              -- Einlagezahl (register entry number)
    status        TEXT,              -- 'E' (Eigentum) or 'G' (Grundbuch)
    area_sqm      REAL,
    lon           REAL,              -- centroid WGS84
    lat           REAL,
    building_count     INTEGER DEFAULT 0,
    footprint_count    INTEGER DEFAULT 0,
    -- Denormalised for fast filtering: top-level gemeinde info
    gemeinde_code      TEXT,         -- copied from KG for fast joins
    -- Bit flags / summary stored as JSON for flexibility
    landuse_codes_json TEXT,         -- e.g. '["41","56"]'
    landuse_summary_json TEXT        -- e.g. '{"Wald - W": 1}'
) WITHOUT ROWID;  -- clustered on parcel_id for fast PK lookups

-- Core lookup indexes
CREATE INDEX IF NOT EXISTS idx_parcel_kg       ON parcel(kg_code);
CREATE INDEX IF NOT EXISTS idx_parcel_gnr      ON parcel(gnr);                   -- search by GNR across all KGs
CREATE INDEX IF NOT EXISTS idx_parcel_ez       ON parcel(kg_code, ez);           -- EZ within a KG
CREATE INDEX IF NOT EXISTS idx_parcel_status   ON parcel(status);
CREATE INDEX IF NOT EXISTS idx_parcel_gemeinde ON parcel(gemeinde_code);

-- Range / filter indexes
CREATE INDEX IF NOT EXISTS idx_parcel_area     ON parcel(area_sqm);
CREATE INDEX IF NOT EXISTS idx_parcel_bcount   ON parcel(building_count);
-- Spatial (centroid) — simple 2D range via compound index
CREATE INDEX IF NOT EXISTS idx_parcel_lonlat   ON parcel(lon, lat);
CREATE INDEX IF NOT EXISTS idx_parcel_latlng   ON parcel(lat, lon);

-- =============================================================================
-- 4. BUILDINGS (addresses) — ~500K rows
-- =============================================================================

CREATE TABLE IF NOT EXISTS building (
    building_id    TEXT PRIMARY KEY,   -- e.g. '75414_27'
    kg_code        TEXT NOT NULL REFERENCES katastralgemeinde(kg_code),
    house_number   TEXT,
    street         TEXT,               -- from properties.strasse
    postal_code    TEXT,               -- from properties.plz
    ort            TEXT,               -- locality / settlement name
    full_address   TEXT,               -- from properties.name (the rich version)
    object_type    TEXT,               -- properties.objectType
    lon            REAL,
    lat            REAL,
    gemeinde_code  TEXT                -- denormalised
) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS idx_building_kg      ON building(kg_code);
CREATE INDEX IF NOT EXISTS idx_building_plz     ON building(postal_code);
CREATE INDEX IF NOT EXISTS idx_building_street  ON building(street);
CREATE INDEX IF NOT EXISTS idx_building_ort     ON building(ort);
CREATE INDEX IF NOT EXISTS idx_building_hnr     ON building(house_number);
CREATE INDEX IF NOT EXISTS idx_building_gemeinde ON building(gemeinde_code);

-- =============================================================================
-- 5. PARCEL ↔ BUILDING JUNCTION  (M:N)
-- =============================================================================

CREATE TABLE IF NOT EXISTS parcel_building (
    parcel_id   TEXT NOT NULL REFERENCES parcel(parcel_id),
    building_id TEXT NOT NULL REFERENCES building(building_id),
    PRIMARY KEY (parcel_id, building_id)
) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS idx_pb_building ON parcel_building(building_id);

-- =============================================================================
-- 6. PARCEL ↔ LANDUSE  (denormalised per-parcel landuse detail)
-- =============================================================================
-- For "parcels with landuse X" queries and aggregation.
-- One row per distinct landuse code per parcel.

CREATE TABLE IF NOT EXISTS parcel_landuse (
    parcel_id     TEXT NOT NULL REFERENCES parcel(parcel_id),
    landuse_code  TEXT NOT NULL REFERENCES landuse_type(code),
    count         INTEGER DEFAULT 1,   -- how many features of this type
    PRIMARY KEY (parcel_id, landuse_code)
) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS idx_plu_code   ON parcel_landuse(landuse_code);
CREATE INDEX IF NOT EXISTS idx_plu_parcel ON parcel_landuse(parcel_id);

-- =============================================================================
-- 7. LANDUSE FEATURES  (optional — full detail, ~15M rows)
-- =============================================================================
-- Only needed if per-feature queries are required.
-- If space is a concern, skip this table and rely on parcel_landuse.

CREATE TABLE IF NOT EXISTS landuse (
    id            INTEGER PRIMARY KEY,  -- auto rowid
    kg_code       TEXT NOT NULL,
    landuse_code  TEXT NOT NULL,
    source        TEXT,                 -- 'nfl' (polygon) or 'nsy' (point symbol)
    area_sqm      REAL DEFAULT 0,       -- >0 for polygons, 0 for point symbols
    lon           REAL,
    lat           REAL
);

CREATE INDEX IF NOT EXISTS idx_lu_kg   ON landuse(kg_code);
CREATE INDEX IF NOT EXISTS idx_lu_code ON landuse(landuse_code);
CREATE INDEX IF NOT EXISTS idx_lu_src  ON landuse(source);

-- =============================================================================
-- 8. BUILDING FOOTPRINTS  (optional — ~2M rows)
-- =============================================================================

CREATE TABLE IF NOT EXISTS building_footprint (
    footprint_id  TEXT PRIMARY KEY,     -- e.g. '75414_fp_0'
    kg_code       TEXT NOT NULL,
    landuse_code  TEXT,                 -- from properties.ns
    lon           REAL,
    lat           REAL
) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS idx_fp_kg ON building_footprint(kg_code);

-- =============================================================================
-- 9. FULL-TEXT SEARCH (FTS5)
-- =============================================================================

-- FTS for parcels: search by parcel number, EZ, KG name
CREATE VIRTUAL TABLE IF NOT EXISTS fts_parcel USING fts5(
    parcel_id,          -- "75414-1314/1"
    gnr,                -- "1314/1"
    ez,                 -- "67"
    kg_code,            -- "75414"
    kg_name,            -- KG name (denormalised for search)
    gemeinde_name,      -- municipality name
    landuse_text,       -- "Wald Baufläche" (concatenated landuse types)
    tokenize="unicode61 tokenchars '/.-' remove_diacritics 2"
    -- tokenchars keeps '/' and '-' as part of tokens so "1314/1" matches
    -- external-content: indexer INSERTs directly with rowid
);

-- Triggers to keep FTS in sync with the parcel table
-- NOTE: since parcel is WITHOUT ROWID, we use an external-content approach.
-- The indexer should call: INSERT INTO fts_parcel(rowid, ...) directly.
-- For simplicity, we drop and rebuild FTS after each indexing run.

-- FTS for addresses: search by street, house number, PLZ, locality
CREATE VIRTUAL TABLE IF NOT EXISTS fts_address USING fts5(
    building_id,
    full_address,       -- "9585 Finkenstein am Faaker See Goritschacher Straße 27"
    street,
    ort,                -- locality
    postal_code,
    house_number,
    kg_code,
    tokenize="unicode61 tokenchars '-' remove_diacritics 2"
);

-- FTS for administrative names (KG + Gemeinde + district)
CREATE VIRTUAL TABLE IF NOT EXISTS fts_admin USING fts5(
    kg_code,
    kg_name,
    gemeinde_code,
    gemeinde_name,
    bundesland_name,
    tokenize="unicode61 remove_diacritics 2"
);

-- =============================================================================
-- 10. INCREMENTAL UPDATE TRACKING
-- =============================================================================

CREATE TABLE IF NOT EXISTS index_tracking (
    kg_code         TEXT PRIMARY KEY,
    source_file     TEXT NOT NULL,       -- relative path to .json.gz
    file_checksum   TEXT NOT NULL,       -- SHA-256 hex of the compressed file
    file_size_bytes INTEGER,
    indexed_at      TEXT NOT NULL,       -- ISO-8601 timestamp
    indexing_ms     INTEGER,             -- how long indexing took
    parcel_count    INTEGER DEFAULT 0,
    building_count  INTEGER DEFAULT 0,
    landuse_count   INTEGER DEFAULT 0,
    status          TEXT DEFAULT 'ok'    -- 'ok', 'error', 'partial'
);

-- =============================================================================
-- 11. USEFUL VIEWS
-- =============================================================================

-- Parcel with full administrative context
CREATE VIEW IF NOT EXISTS v_parcel_full AS
SELECT
    p.parcel_id,
    p.gnr,
    p.ez,
    p.status,
    p.area_sqm,
    p.lon,
    p.lat,
    p.building_count,
    p.footprint_count,
    p.landuse_codes_json,
    p.landuse_summary_json,
    k.kg_code,
    k.kg_name,
    g.gemeinde_code,
    g.gemeinde_name,
    b.name AS bundesland_name
FROM parcel p
JOIN katastralgemeinde k ON p.kg_code = k.kg_code
JOIN gemeinde g ON k.gemeinde_code = g.gemeinde_code
JOIN bundesland b ON g.bundesland = b.code;

-- Building with parcel and admin context
CREATE VIEW IF NOT EXISTS v_building_full AS
SELECT
    bld.building_id,
    bld.house_number,
    bld.street,
    bld.postal_code,
    bld.ort,
    bld.full_address,
    bld.lon,
    bld.lat,
    pb.parcel_id,
    k.kg_code,
    k.kg_name,
    g.gemeinde_code,
    g.gemeinde_name
FROM building bld
LEFT JOIN parcel_building pb ON bld.building_id = pb.building_id
LEFT JOIN katastralgemeinde k ON bld.kg_code = k.kg_code
LEFT JOIN gemeinde g ON k.gemeinde_code = g.gemeinde_code;

-- Landuse summary per KG (for aggregation queries)
CREATE VIEW IF NOT EXISTS v_landuse_by_kg AS
SELECT
    p.kg_code,
    pl.landuse_code,
    lt.type  AS landuse_type,
    lt.abbr  AS landuse_abbr,
    COUNT(*)         AS parcel_count,
    SUM(p.area_sqm)  AS total_area_sqm
FROM parcel_landuse pl
JOIN parcel p  ON pl.parcel_id = p.parcel_id
JOIN landuse_type lt ON pl.landuse_code = lt.code
GROUP BY p.kg_code, pl.landuse_code;

-- Landuse summary per Gemeinde
CREATE VIEW IF NOT EXISTS v_landuse_by_gemeinde AS
SELECT
    p.gemeinde_code,
    g.gemeinde_name,
    pl.landuse_code,
    lt.type  AS landuse_type,
    lt.abbr  AS landuse_abbr,
    COUNT(*)         AS parcel_count,
    SUM(p.area_sqm)  AS total_area_sqm
FROM parcel_landuse pl
JOIN parcel p  ON pl.parcel_id = p.parcel_id
JOIN landuse_type lt ON pl.landuse_code = lt.code
JOIN gemeinde g ON p.gemeinde_code = g.gemeinde_code
GROUP BY p.gemeinde_code, pl.landuse_code;

-- KG statistics overview
CREATE VIEW IF NOT EXISTS v_kg_stats AS
SELECT
    k.kg_code,
    k.kg_name,
    g.gemeinde_name,
    b.name AS bundesland_name,
    k.parcel_count,
    k.building_count,
    k.landuse_count,
    ROUND(AVG(p.area_sqm), 2)  AS avg_parcel_area_sqm,
    ROUND(SUM(p.area_sqm), 2)  AS total_area_sqm,
    SUM(p.building_count)      AS total_buildings_on_parcels
FROM katastralgemeinde k
JOIN gemeinde g ON k.gemeinde_code = g.gemeinde_code
JOIN bundesland b ON g.bundesland = b.code
LEFT JOIN parcel p ON p.kg_code = k.kg_code
GROUP BY k.kg_code;

-- PLZ → parcels bridging view (via gemeinde)
CREATE VIEW IF NOT EXISTS v_plz_parcels AS
SELECT
    gp.plz,
    p.parcel_id,
    p.gnr,
    p.ez,
    p.area_sqm,
    p.kg_code,
    p.gemeinde_code
FROM gemeinde_plz gp
JOIN parcel p ON p.gemeinde_code = gp.gemeinde_code;

-- =============================================================================
-- 12. EXAMPLE QUERY PATTERNS
-- =============================================================================
-- (These are comments / documentation, not executed)
--
-- 1) Full-text search for address:
--    SELECT * FROM fts_address WHERE fts_address MATCH 'Goritschacher Straße';
--
-- 2) Exact parcel lookup:
--    SELECT * FROM v_parcel_full WHERE parcel_id = '75414-1314/1';
--
-- 3) All parcels in a KG with EZ:
--    SELECT * FROM parcel WHERE kg_code = '75414' AND ez = '67';
--
-- 4) Parcels in PLZ 8570 with landuse Wald and area > 1000:
--    SELECT p.* FROM parcel p
--    JOIN gemeinde_plz gp ON p.gemeinde_code = gp.gemeinde_code
--    JOIN parcel_landuse pl ON p.parcel_id = pl.parcel_id
--    WHERE gp.plz = '8570'
--      AND pl.landuse_code = '56'   -- Wald
--      AND p.area_sqm > 1000;
--
-- 5) Total agricultural land per KG:
--    SELECT * FROM v_landuse_by_kg
--    WHERE landuse_code IN ('52','53','54','55');
--
-- 6) Average parcel size by Bundesland:
--    SELECT b.name, ROUND(AVG(p.area_sqm),2) AS avg_area
--    FROM parcel p
--    JOIN katastralgemeinde k ON p.kg_code = k.kg_code
--    JOIN gemeinde g ON k.gemeinde_code = g.gemeinde_code
--    JOIN bundesland b ON g.bundesland = b.code
--    GROUP BY b.code;
--
-- 7) Bbox query (parcels within a map viewport):
--    SELECT * FROM parcel
--    WHERE lon BETWEEN :west AND :east
--      AND lat BETWEEN :south AND :north;
--
-- 8) Incremental update check:
--    SELECT kg_code FROM index_tracking
--    WHERE file_checksum != :new_checksum;
--
-- 9) FTS parcel search (GNR with slash — quote the term!):
--    SELECT * FROM fts_parcel WHERE fts_parcel MATCH '"1314/1"';
--
-- 10) Parcels near a point (approximate):
--     SELECT *, (  (lon - :lon)*(lon - :lon)*COS(RADIANS(:lat))*COS(RADIANS(:lat))
--                + (lat - :lat)*(lat - :lat)  ) AS dist2
--     FROM parcel
--     WHERE lon BETWEEN :lon - 0.01 AND :lon + 0.01
--       AND lat BETWEEN :lat - 0.01 AND :lat + 0.01
--     ORDER BY dist2 LIMIT 20;
