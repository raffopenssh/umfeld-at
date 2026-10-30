# OSM Proximity (roads, transit, water, remoteness)

Point-keyed accessibility metrics and map geometry from the Geofabrik Austria
extract (`data/osm_geofabrik_data/austria.gpkg`, refresh from
`https://download.geofabrik.de/europe/austria-latest-free.gpkg.zip`).
Code: `osm_proximity.go`, `osm_areas.go`, `osm_remote_grid.go`,
`api_osm_proximity.go`, `api_osm_geocode.go`.

**Own DB `data/osm_proximity.db`** (separate from `search_index.db` — zero write
contention, no index rebuild; built locally, never committed):

| Table | Contents |
|---|---|
| `osm_lines` + `osm_lines_rtree` | ~1.95M chunked polylines (≤24 vertices, packed float64 coords): cat=`road` (drivable, `major` flag for motorway–secondary), `rail`, `water` (waterway lines + lake/reservoir outer rings) |
| `osm_points` + `osm_points_rtree` | ~70k points: cat=`transit` (bus/tram/train/ferry stops, `station` flag for railway_station/halt), `place` (village/town/city) |
| `osm_areas` + rtree | water polygons (river/lake/reservoir/wetland) as whole (Multi)Polygons, `-osm-import-areas` |

**Metrics** (`GET /api/v1/osm/point?lon=&lat=[&area_sqm=]`, also the `osm` block
of `/api/v1/context`): `dist_road_m`, `road_on_parcel` (nearest road within the
equivalent-circle radius of `area_sqm` when given), `dist_major_road_m`
(+name/ref), `dist_rail_m`, `dist_transit_m`, `dist_train_station_m`,
`dist_water_m`, `dist_settlement_m`, `remoteness` 0–100 (weighted
sqrt-normalised: road/5km .30, major/20km .15, transit/10km .30, settlement/15km
.25). Nearest-feature search = expanding-ring R*Tree bbox lookups + exact
segment distance (~2 ms/point). Nothing is cached per parcel — a client that
holds its own cadastre (bevdirect) passes `area_sqm` and gets area-aware
metrics for a unit of that size.

**Remoteness grid** (`osm_remote_grid.go`): Austria at 250 m, one byte a cell = the
score above WITHOUT the road term (major/20km .15, transit/10km .30, settlement/15km .25,
renormalised to 0–100; 255 = no data). Built once after the OSM db opens — rasterise the
three feature sets, exact Euclidean distance transform (Felzenszwalb–Huttenlocher), ~0.7 s —
persisted to `data/osm_remote_grid.bin` (3.2 MB, rebuildable, gitignored; delete to rebuild),
size line `osm_remote_grid` on `$UMFELD_PRIVATE_PREFIX/api/v1/debug/inflight`. Read by the
landing glyph's ROADS overlay (`ne_glyph_roads.go`) to weigh every way's ink by how remote
it runs; sampled by `remoteGridGet().At(lon, lat)` (−1 outside / not ready). Typical values:
Graz centre 5, Vienna 1st district 14, a Wechsel village 22, Waldviertel forest 31,
Hohe Tauern 70+.

**Geometry serving:** `GET /api/v1/osm/geometry?bbox=w,s,e,n&cat=road,rail,water,water_area,transit,place[&major=1][&clip=0][&limit=]`
returns a GeoJSON FeatureCollection straight from the R-tree (few ms) for map
drawing. `cat=water` = waterway centrelines + area-outline chunks (the distance
layer); `cat=water_area` = fillable (Multi)Polygons clipped to the bbox
(`clip=0` for whole polygons). On the private tier `kg=` (KG bbox from the
`kg` table) is accepted instead of `bbox=`. There is no `parcel=` selector and
no `cat=water_parcels` on any tier — clip client-side with bevdirect if parcel
lines are wanted.

**Stats:** `GET /api/v1/osm/stats` — table counts, import date, readiness.

**Ops:**
```
unzip austria-latest-free.gpkg.zip -d data/osm_geofabrik_data/   # fresh Geofabrik download
./umfeld-at -osm-import          # GPKG → osm_proximity.db lines/points (~7 min), API keeps running
./umfeld-at -osm-import-areas    # water polygons into osm_areas (keeps lines/points)
curl localhost:8000/api/v1/osm/stats
```
Re-import wipes lines/points and rebuilds them; the gpkg/zip files can be
deleted after import. The remoteness grid rebuilds itself on the next start
(delete `data/osm_remote_grid.bin`).
