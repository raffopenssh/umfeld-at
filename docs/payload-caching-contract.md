# Payload & Caching Contract (read before adding an endpoint)

The API was "slow" for reasons that had nothing to do with SQL: handlers finished
in ~150 ms while the client waited seconds for bytes. Rules that encode the fixes
(`api_compress.go`):

**1. Never write pretty-printed JSON by default.** `SetIndent("", "  ")` roughly
doubles coordinate-heavy payloads. Use `writeJSON` / `apiResponse` / `jsonEnc`,
which indent only when the request carried `?pretty=1`. Never call
`json.NewEncoder(w).SetIndent(...)` directly.

**2. Emit coordinates via `coord`, not `[]float64`.** Go prints float64 with 17
significant digits; 7 decimals is ~0.8 cm, finer than the BEV source. Worth
-27 % raw / -30 % gzipped on polygon payloads.

**3. Compression is global** (`gzipMiddleware` in `main.go`), level 3 not 6 — at
level 6 flate was 32 % of all CPU on a 2-core box, and level 3 + trimmed
coordinates is *smaller* than level 6 was on full-precision output. Don't set
`Content-Encoding` in a handler; the middleware skips anything already encoded
and anything under 1400 B.

**4. Idempotent `/api/` GETs get ETag + `Cache-Control: max-age=120`**
(`cacheMiddleware`). Add any endpoint whose value must be live to
`cacheablePath`'s exclusion list (`api_compress.go`; `/debug`, `/feedback` and the
NE write paths are excluded).

Measured on one Graz viewport of polygon geometry:
8 160 738 B → 508 411 B (-94 %); revalidation 165 KB → 0 B.

**What clients must do:** nothing — it is standard content negotiation. The
middleware compresses only when the request carries `Accept-Encoding: gzip` and
sets `Vary: Accept-Encoding`. Python `requests`/`httpx`, Go `net/http`, browsers
and axios advertise and decompress transparently. Two caveats worth telling
agents (and documented in `llm.txt`):
- bare `curl` does **not** advertise gzip — use `--compressed`; and if you set the
  header by hand in Go you must `gzip.NewReader` yourself.
- compressed responses are chunked, so there is **no `Content-Length`**.
Also client-visible from the same change: compact JSON by default (`?pretty=1`
opts back in) and 7-decimal coordinates, so byte-level/float-equality diffs
against older cached output will differ.

## Filter index (`/api/v1/ne/glyph/fidx`, `/api/v1/ne/filter`)

- `fidx` is one blob per 0.025° bin and plane (`core` ≈ 150 KB gz, `ext` ≈ 120 KB gz for a dense urban bin;
  `all` for agents). Content-Type `application/vnd.ne-fidx` is in `compressibleContentType` — planar u8
  columns gzip 5–6×. ETag `"<state>-<plane>"`; `If-None-Match` → 304. With `&e=<state>` (the page takes it
  from the tile's `X-NE-Tile` state / `X-NE-Fidx-State`) the answer is `immutable, max-age=1y`; without,
  `max-age=300`. The state hashes the bin's KG tile state + the INVEKOS release + the observed-layer peer epoch, so a
  peer update changes the URL, never a stale body.
- Server: LRU `ne_fidx` 48 MB → `data/ne_fidx/<state>/i_j.zst` (2 GB cap, mtime sweep) → build (≈ 3 s,
  single-flight). The warm loop builds bins the tile handler just served while the box is idle
  (`NE_FIDX_WARM=0` off). Size line: `$UMFELD_PRIVATE_PREFIX/api/v1/debug/inflight` → `ne_fidx`.
- Client: the Worker keeps ≤ 24 bins (`KEEP`) and paints one PNG per *view tile* (LOD 0/1 grid of the
  glyph), so a chip change is a repaint from memory (≈ 3 ms eval + 2–4 ms per tile), zero network.
- `/api/v1/ne/filter` answers `max-age=300`, `no-store` while any bin of the box is still building
  (`partial_bins > 0`); `/filter/fields` 1 h. bbox span ≤ 0.1° (16 bins) → 413 above; `cells`/`h3` ≤ 65 536
  matches, `geojson` ≤ 4 096 → 413 with the hint to split or use `format=count`.

## Client-side caches & the BEV licence (`api_cache_policy.go`, `clients/`)

Caching copies is **permitted without time limit**: the VTC tiles are CC BY 4.0,
and CC BY 4.0 §1(c)/§4 expressly license BEV's sui generis database right
(§§ 76c–76e UrhG) — extraction and re-utilisation of substantial parts included.
What the licence *does* require on every copy, cached or processed: the
year-bearing notice (`BEVNotice()`, „© BEV, JJJJ …", BEV Nutzungsbedingungen
§2.3.3), "bearbeitet/modified", the licence link, no implication of an
official/live extract — hence the data date must be shown. The 24 h / 7 d
figures in `/api/v1/cache/policy` and the reference clients are freshness
*recommendations*, not licence terms.

Hard boundary: the CC BY grant is per product. The Katasterservice web app and
its search/info JSON API are governed by the AGB + Standardentgelte (copies only
for backup, no Weitergabe) and are **not** licensed — ingest must only ever read
`kataster.bev.gv.at/…/tiles/{kataster|symbole}/…` (checked: those are the only
BEV URLs in the code base). `/api/v1/cache/policy` lists the rules; the
zero-dependency reference clients (`/api/v1/docs/cache-client.{py,js}`) store
notice + date with each entry, revalidate via ETag, never cache `pending`/202/
error bodies, flag `osm{}` rows (ODbL share-alike is a database-level duty) and
write `ATTRIBUTION.txt` beside persisted caches.
