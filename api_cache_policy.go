// api_cache_policy.go — Client-side caching that stays inside the BEV licence.
//
// Why this exists: clients (browsers, agents, GIS plugins) cache our responses
// — and that is where attribution gets lost. The data is CC BY 4.0 (BEV's own
// "Katastralmappe VTC" product sheet §2.3), so storing and re-serving copies is
// PERMITTED, but every copy must carry the notice. BEV's Standardentgelte &
// Nutzungsbedingungen §2.3.3 ask for „© BEV, JJJJ“ on *all* digital copies,
// explicitly also after processing (Folgeprodukte), and the Katasterservice
// advertises itself as "tagesaktuell" — so a cached copy must also show its
// date, otherwise it misrepresents the source.
//
// Three pieces:
//   - BEVNotice()/bevDataYear(): the canonical year-bearing notice used in the
//     X-Data-Attribution header, /license and the policy below.
//   - GET /api/v1/cache/policy: machine-readable rules a cache MUST follow.
//   - GET /api/v1/docs/cache-client.{py,js}: zero-dependency reference caches
//     (clients/umfeld_cache.*) that implement the policy: ETag revalidation,
//     bounded TTL, attribution+date stored WITH the payload, never cache
//     pending/202/error bodies, ATTRIBUTION.txt beside any persisted cache.
package main

import (
	_ "embed"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

//go:embed clients/umfeld_cache.py
var cacheClientPy []byte

//go:embed clients/umfeld_cache.js
var cacheClientJS []byte

const (
	// cachePolicyMaxTTL / cachePolicyMaxStale are RECOMMENDED freshness
	// defaults, not licence terms: CC BY 4.0 is perpetual and sets no expiry
	// on copies. What the licence requires is the notice + "modified" + a
	// visible data date on every copy, however old. The defaults exist so
	// reprocessed KGs show up and nobody presents a year-old copy as live.
	cachePolicyMaxTTL   = 24 * time.Hour
	cachePolicyMaxStale = 7 * 24 * time.Hour
)

// bevDataYear is the year to print in „© BEV, JJJJ“: the year of the BEV
// data epoch the served layers were built from, never the wall clock — a
// 2026 dataset shown in 2027 must still say 2026. Bump bevDataYearDefault
// (or set BEV_DATA_YEAR) when a new declared epoch lands.
const bevDataYearDefault = 2026

func bevDataYear() int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("BEV_DATA_YEAR"))); err == nil && v >= 2000 {
		return v
	}
	return bevDataYearDefault
}

// BEVNotice is the one-line notice every stored or displayed copy must carry.
func BEVNotice() string {
	return "© BEV, " + strconv.Itoa(bevDataYear()) + " – " + BEVAttributionDE + ", CC BY 4.0 (" + BEVLicenseURL + "), bearbeitet"
}

// BEVNoticeASCII is the same notice restricted to ASCII for HTTP header
// values (RFC 9110 §5.5: non-ASCII field content is opaque; Python's
// http.client decodes it as latin-1 and mangles „©"/„ü"). Bodies carry the
// UTF-8 form.
func BEVNoticeASCII() string {
	return "(c) BEV, " + strconv.Itoa(bevDataYear()) + " - Datenquelle: BEV - Bundesamt fuer Eich- und Vermessungswesen, CC BY 4.0 (" + BEVLicenseURL + "), bearbeitet/modified"
}

// cachePolicy is the body of GET /api/v1/cache/policy. Keep it boring and
// stable — clients bake it into config.
func cachePolicy() map[string]interface{} {
	return map[string]interface{}{
		"version": 1,
		"summary": "Caching copies of this API's responses is permitted without time limit (CC BY 4.0 is perpetual). Legal obligations: keep the notice below, say 'modified', show the data date, don't imply an official/live BEV extract. TTLs below are freshness RECOMMENDATIONS, not licence terms. Never cache pending/warming or error responses.",
		"license": map[string]interface{}{
			"spdx":           BEVLicenseSPDX,
			"url":            BEVLicenseURL,
			"notice":         BEVNotice(),
			"notice_header":  BEVNoticeASCII(),
			"notice_short":   "© BEV, " + strconv.Itoa(bevDataYear()) + ", CC BY 4.0, bearbeitet",
			"data_year":      bevDataYear(),
			"modified":       true,
			"no_endorsement": BEVNoEndorsement,
			"basis": []string{
				"BEV Katastralmappe VTC product sheet §2.3: Standardlizenz CC BY 4.0 for the tilesets at kataster.bev.gv.at/tiles — the ONLY BEV cadastre source this API ingests",
				"Sui generis database right (§§ 76c–76e UrhG, Directive 96/9/EC): asserted by BEV and LICENSED by CC BY 4.0 §1(c)/§4 (extraction and re-utilisation of substantial parts permitted; a derived database must carry the §3(a) notice)",
				"Katasterservice web app / search & info JSON API are NOT CC BY (AGB + Standardentgelte, copies only for backup) — nothing here is derived from them",
				"Directive (EU) 2019/1024 Art. 1(6) / IWG 2022: a public body may not use the database right to block permitted re-use",
				"BEV Standardentgelte und Nutzungsbedingungen §2.3.3: notice „© BEV, JJJJ“ on every digital copy, also after processing",
				"CC BY 4.0 §3(a): attribution, licence link, indicate modifications",
			},
		},
		"ttl": map[string]interface{}{
			"legal_expiry":            nil,
			"legal_note":              "No expiry. The licence does not limit how long a copy may be kept; it requires attribution, 'modified' and that the copy is not presented as live/official.",
			"server_max_age_s":        apiCacheMaxAge,
			"recommended_ttl_s":       int(cachePolicyMaxTTL.Seconds()),
			"recommended_max_stale_s": int(cachePolicyMaxStale.Seconds()),
			"revalidate":              "send If-None-Match with the stored ETag; a 304 renews the TTL at zero bytes",
			"data_date":               "the authoritative 'Stand' is processing_metadata.processed_at / assembly_version.reprocessed_at (per KG, see /api/v1/search/kg), not the HTTP fetch time; the fetch time is only a proxy",
		},
		"must": []string{
			"Store the X-Data-Attribution header (or license.notice) and the fetch timestamp WITH the payload; render both wherever the data is shown or exported",
			"Store the ETag and revalidate with If-None-Match after recommended_ttl_s",
			"Show the data date with every copy (e.g. 'Stand: 2026-03-14'), especially when older than server_max_age_s; discarding after recommended_max_stale_s is good practice, not a licence term",
			"Write an ATTRIBUTION.txt (license.notice + licence URL + 'modified') beside any cache persisted to disk or a database",
			"Keep rows containing an osm{} block out of any redistributed database, or license that database ODbL (" + OSMLicenseURL + ") — a database-level (sui generis) obligation",
			"If you build your own database from substantial parts of this API, that database is 'Adapted Material' under CC BY 4.0 §4(b): carry the notice on the database itself, not only on rows",
		},
		"must_not": []string{
			"Cache HTTP 202 or any body with pending:true, ready:false or a warming{} block — these mean UNKNOWN, not 'no data'",
			"Cache non-200 responses, /api/v1/debug/*, /api/v1/feedback*",
			"Strip meta.license / X-Data-Attribution when re-serving to your own users",
			"Present cached data as live/tagesaktuell or as an official BEV extract",
		},
		"cacheable_prefixes":   []string{"/api/v1/"},
		"uncacheable_prefixes": []string{"/api/v1/debug/", "/api/v1/feedback"},
		"pending_markers":      []string{"pending", "ready", "warming"},
		"reference_clients": map[string]string{
			"python":              "/api/v1/docs/cache-client.py",
			"javascript":          "/api/v1/docs/cache-client.js",
			"bev_direct_fallback": "https://github.com/raffopenssh/vtcseamless",
		},
		"details": "/api/v1/license",
	}
}

func handleV1CachePolicy(w http.ResponseWriter, r *http.Request) {
	if !isPrivateTier(r) {
		writeJSON(w, http.StatusOK, publicCachePolicy())
		return
	}
	writeJSON(w, http.StatusOK, cachePolicy())
}

// publicCachePolicy — the public tier serves no BEV Kataster data, so the
// policy is per source rather than one BEV notice.
func publicCachePolicy() map[string]interface{} {
	return map[string]interface{}{
		"version": 2,
		"tier":    "public",
		"summary": "Caching this API's responses is permitted. Keep each response's own attribution (meta.license / X-Data-Attribution) and fetch time with the payload; obligations differ per source. TTLs are freshness recommendations, not licence terms. Never cache error or 503/Retry-After responses.",
		"per_source": map[string]interface{}{
			"statistik_austria": map[string]interface{}{"license": "CC-BY-4.0", "notice": "Datenquelle: STATISTIK AUSTRIA; Preisschätzungen modelliert", "ttl_s": int(cachePolicyMaxTTL.Seconds()), "note": "series change once a year (new Statistik Austria release) — a 24 h TTL is generous"},
			"eea_natura2000":    map[string]interface{}{"license": "EEA re-use (attribution)", "notice": "Source: European Environment Agency (EEA), Natura 2000", "ttl_s": int(cachePolicyMaxTTL.Seconds())},
			"wdpa":              map[string]interface{}{"license": "WDPA terms (non-commercial, attribution)", "notice": "UNEP-WCMC and IUCN, Protected Planet (WDPA)", "ttl_s": int(cachePolicyMaxTTL.Seconds()), "note": "non-commercial use only — do not redistribute in a commercial product"},
			"openstreetmap":     map[string]interface{}{"license": "ODbL-1.0", "notice": OSMAttribution, "ttl_s": int(cachePolicyMaxTTL.Seconds()), "share_alike": true, "note": "a database you redistribute that merges osm{} fields must be ODbL — keep OSM columns separable"},
			"bev_dlm_namen":     map[string]interface{}{"license": "CC-BY-4.0", "notice": BEVAttributionDE + " – Geographische Namen (DLM), CC BY 4.0, bearbeitet", "ttl_s": int(cachePolicyMaxTTL.Seconds()), "note": "stichtag from /api/v1/toponyms/stats"},
			"ris":               map[string]interface{}{"license": "CC-BY-4.0", "notice": "Datenquelle: RIS, Bundeskanzleramt Österreich", "ttl_s": int(cachePolicyMaxTTL.Seconds())},
		},
		"ttl": map[string]interface{}{
			"server_max_age_s":        apiCacheMaxAge,
			"recommended_ttl_s":       int(cachePolicyMaxTTL.Seconds()),
			"recommended_max_stale_s": int(cachePolicyMaxStale.Seconds()),
			"revalidate":              "send If-None-Match with the stored ETag; a 304 renews the TTL at zero bytes",
		},
		"must": []string{
			"Store meta.license (or the X-Data-Attribution header) and the fetch timestamp WITH each payload; render them wherever the data is shown",
			"Store the ETag and revalidate with If-None-Match after recommended_ttl_s",
			"Write an ATTRIBUTION.txt listing every source you cached beside any cache persisted to disk",
		},
		"must_not": []string{
			"Cache non-200 responses or any body with a 503/Retry-After",
			"Strip meta.license / X-Data-Attribution when re-serving",
			"Present land-price estimates as official valuations or Statistik Austria figures — they are model outputs",
			"Use WDPA- or GADM-derived responses in a commercial product without checking their terms",
		},
		"cacheable_prefixes":   []string{"/api/v1/"},
		"uncacheable_prefixes": []string{"/api/v1/feedback"},
		"reference_clients": map[string]string{
			"python":              "/api/v1/docs/cache-client.py",
			"javascript":          "/api/v1/docs/cache-client.js",
			"bev_direct_cadastre": "https://github.com/raffopenssh/vtcseamless",
		},
		"details": "/api/v1/license",
	}
}

func handleV1CacheClientPy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-python; charset=utf-8")
	_, _ = w.Write(cacheClientPy)
}

func handleV1CacheClientJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, _ = w.Write(cacheClientJS)
}
