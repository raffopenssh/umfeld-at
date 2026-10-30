# Toponyms — BEV DLM Geographische Namen

`toponyms.go` + `api_toponyms.go` + `mgi_transform.go`, own DB `data/toponyms.db`.

**What.** BEV's official toponym register (DLM 7000 NAMEN): 232 979 named points
in 7 layers — `siedlung` 58k (incl. 33k Einzelhäuser = farmsteads/Alm huts),
`gebiet`, `gelaende` (peaks/passes/valleys), `gletscher`, `gewaesser`, `sonstige`
(huts, chapels, castles) and `ried` 104k Riednamen that BEV keys by KG_NUMMER.
CC BY 4.0, same producer as the Kataster but a **different product with its own
Stichtag** — it has its own row in `/api/v1/license` (`bev_dlm_namen`) and
toponym responses name it in `meta.license.dataset`.

**Why here.** Peers are told to "resolve every name through ONE canonical
lookup, never fuzzy-match locally", yet `/lookup?q=Lignitzalpe` returned 0. A
subsidy-data consumer needs `AGRARGEM. LIEGNITZALM` → a point; the Alm name is
the only non-heuristic key for ~1 900 Agrargemeinschaft recipients. Now
`/lookup` falls through to toponyms on 0 rows.

**Ingest (`-toponyms-import`, ~3 min, separate process).**
- Reads the GPKG (SQLite) directly; **native EPSG:31255 → WGS84** in
  `mgi_transform.go` (Bessel TM inverse + EPSG:1618 Helmert), pinned against
  PROJ to <0.5 m by `mgi_transform_test.go`. No GDAL dependency for this path.
- KG assignment, disclosed per row as `kg_code_source`: `bev` (Ried KG_NUMMER,
  104 100 rows) or the import-time spatial assignment for the other layers
  (the KG bbox index narrows the candidates; the value names the method used).
  0 rows unassigned.
- `currency_year` from the 1 993 ÖK map-sheet polygons (DLM_AKTUALITAETSSTAND).
- Writes `toponyms.db.tmp`, then atomic rename. The live server stats the file
  every 10 min and reloads — no restart. Gitignored; rebuildable in 3 min.

**The fast path is in RAM, not FTS5.** ~357k normalised strings (name, variant,
minority-language names and a generic-suffix-stripped **stem**) in a trigram
posting index (~8.8k trigrams). Because each string is indexed by its DISTINCT
trigram set, the posting-walk count *is* the number of shared trigrams, so
Dice = 2c/(|q|+|name|) needs no per-candidate work; only exact/prefix/whole-word
boosts touch the string. A national fuzzy search is **5–20 ms** and takes no
SQLite connection until the page fetch (`WHERE rowid IN (…)`), so it cannot
starve the connection pool (Latency Contract rule 3/5). First version used a map per
query + recomputed Dice per candidate: 100–200 ms. Filters (layer/f_code/kg/
gemeinde/district/state/near/bbox) are evaluated on the in-memory entry
(40 B/row); gemeinde/district names go through `ResolveKGCodes`, never LIKE.

**Ranking contract** (`score` + `matched_on` on every row): 1.0 exact ·
≥0.92 prefix · 0.85 whole word · 0.75 substring · else Dice. Evidence via the
**stem** is capped at 0.90, stem-vs-stem at 0.80, so a literal "Grünalm" (score
1.0) outranks "Grünberg" (shares stem "gruen"). Stems must be ≥4 chars and not
themselves generic ("Großau" → "gross" is rejected; "Liebach" → "lie" is
rejected) — the first version leaked "Liebach" into a "Liegnitzalm" query at
0.85 via a 3-char stem. Pinned by `toponyms_test.go`. `min_score` default 0.55.

**Fallback rewrites** when a multi-word query scores <0.75: organisational
stopwords stripped (`AGRARGEM.`, `Weidegemeinschaft`, …) and a token that is
exactly a Gemeinde name becomes a **filter** (`Agrargemeinschaft Hochalm
Kramsach` → q=hochalm, gemeinde=Kramsach → the one Hochalm in Kramsach at 1.0).
Reported in `meta.query_used`. The Gemeinde-name map is pre-built at load (the
first request used to pay 900 ms for it).

**Honesty about the coordinate.** BEV states ±50 m. A toponym is **never snapped
to a parcel** — the service holds none; a client that wants the parcel under a
toponym clips it from BEV tiles with bevdirect.

**Routes** (`api_toponyms.go`, all public): `/api/v1/toponyms?q=` =
`/toponyms/search?q=&layer=&f_code=&kg=&gemeinde=&district=&state=&near=lon,lat&bbox=&min_score=`,
`/toponyms/near?lon=&lat=&radius_m=`, `/toponyms/{id}`, `/toponyms/stats`,
`/kg/{kg}/toponyms`. Also the `toponyms` block of `/api/v1/context` and the
glyph find box (`docs/glyph-find-contract.md`). Attribution source id `bev_dlm_namen`.

**Data facts worth knowing.** The register has NO "Liegnitz": the Lungau valley
is *Lignitz* (Ried "Lignitzalpe", "Hintere Lignitzalmen"), which is exactly why
fuzzy + stem matching is a requirement, not a nicety. "Hochalm" occurs 1 099
times — always constrain by kg/gemeinde/district or `near=`. `VARIANTNAME` is
populated for only ~30 settlement rows but carries the alias cases
("Ennslehenhütte" ↔ "Hintere Marbachalm") and is indexed. Elevation 0 = missing
(omitted). `HasPrefix` boosts use the normalised (umlaut-folded) string, so
"Gruenalm" and "Grünalm" are the same query.
