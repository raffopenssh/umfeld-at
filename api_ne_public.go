// api_ne_public.go — the NE read API, H3-keyed (docs/ne-public-api.md).
//
// Served tokenless under /api/v1/ne/… and identically under <private>/api/v1/ne/….
// Keys are H3 cell ids (or lon/lat / bbox resolved to H3 here) and the
// 5-digit KG code as a bulk key — never a single land unit.
//
//	GET /ne                       product, frozen pin, endpoints
//	GET /ne/manifest[?fields=digests]
//	GET /ne/{kg}[?layers=lu,bev,obs][&format=json]   whole container (materialised, LRU)
//	GET /ne/{kg}/head             section headers only
//	GET /ne/dict/{id}             zstd dictionary
//	GET /ne/chunk/{h3_res9}       THE primitive: stored zstd frames pass-through (bin|json too)
//	GET /ne/cell/{h3}             res 12 → decoded row + K record; res 10 → K + children; res 9 → 302 chunk
//	GET /ne/cells?ids=|lon&lat[&k]|bbox=[&list=1]   ≤ 512 ids / k ≤ 3 / span ≤ 0.05°
//	POST /ne/cells {ids:[…]}      ≤ 16 384 ids
//
// Public vocabulary: KG, unit (land unit), register, structure, declared /
// observed. Field names differ per tier through neFieldName; binary layouts
// never change. Public K percentiles are log-bucketed (1/8 octave) and
// p10/p90 withheld below 10 units (§0b). Zero SQL, zero warming, zero writes.
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ── vocabulary / attribution ─────────────────────────────────────────────────

// NENotice — attribution of source id `ne` (never the BEV default notice).
func NENotice() string {
	return "© BEV, " + strconv.Itoa(bevDataYear()) + " (CC BY 4.0), bearbeitet — statistics per H3 cell, no object geometry · observed layer © " + peers().ObsName + " (CC BY 4.0)"
}

// neObsPublic — the observed layer is served tokenless. srtm agreed to CC BY
// redistribution by posting to /obs (ne-cells.md §Licence) and serves the same
// rows tokenless itself ({OBS_NAME} /api/v1/cells); released 2026-10-26.
var neObsPublic = true

var neFieldPublicTable = map[string]string{
	"n_parc": "n_units", "parc_p10": "unit_p10", "parc_p50": "unit_p50", "parc_p90": "unit_p90",
	"line_m_ha": "edge_m_ha", "bld_cover": "str_cover", "bld_n": "str_n", "bld_p50": "str_p50",
	"ez_n": "reg_n", "parc_per_ez": "units_per_reg",
}

// neFieldName — stored name → tier name.
func neFieldName(public bool, stored string) string {
	if !public {
		return stored
	}
	if p, ok := neFieldPublicTable[stored]; ok {
		return p
	}
	return stored
}

var neLUFields = []string{"kg_share", "land", "cov", "flags", "lu", "bld_cover", "bld_n", "gk", "n_parc", "line_m_ha", "orient_coh", "orient_axis"}
var neKFields = []string{"n_parc", "parc_p10", "parc_p50", "parc_p90", "ez_n", "parc_per_ez", "bld_n", "bld_p50"}

// neRewriteHeaderPublic — the stored section header for the public tier:
// attribution/notice → NENotice, field lists in public names, provenance
// reduced to counts + hashes, stats keys renamed. Everything the peers read
// (magic fmt_ver algo epoch source kg h3_res k_res k cells_n kcells_n groups
// ns_table digest coord_decimals input_bbox coverage_bbox input_pad_deg) is
// unchanged. The header is outside the digest.
func neRewriteHeaderPublic(h map[string]interface{}) []byte {
	out := make(map[string]interface{}, len(h))
	for k, v := range h {
		switch k {
		case "attribution", "notice":
			out[k] = NENotice()
		case "input_rule":
			// dropped (names private endpoints)
		case "inputs":
			ins, _ := v.([]interface{})
			red := map[string]interface{}{"n": len(ins)}
			var shas []string
			for _, in := range ins {
				m, _ := in.(map[string]interface{})
				if s, _ := m["sha256"].(string); s != "" {
					shas = append(shas, s)
				}
				if hr, _ := m["hash_rule"].(string); hr != "" {
					red["hash_rule"] = hr
				}
			}
			if hr, _ := h["hash_rule"].(string); hr != "" {
				red["hash_rule"] = hr
			}
			red["sha256"] = shas
			out[k] = red
		case "stats":
			st, _ := v.(map[string]interface{})
			ps := make(map[string]interface{}, len(st))
			for sk, sv := range st {
				ps[neStatKeyPublic(sk)] = sv
			}
			out[k] = ps
		default:
			out[k] = v
		}
	}
	if _, ok := h["record_lu"]; ok {
		out["fields_lu"] = neNames(true, neLUFields)
		out["fields_k"] = neNames(true, neKFields)
	}
	b, _ := json.Marshal(out) // keys sorted by encoding/json
	return b
}

func neNames(public bool, in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = neFieldName(public, s)
	}
	return out
}

// ── row decoders ─────────────────────────────────────────────────────────────

// neBucket — public K percentile class midpoint (1/8-octave classes).
func neBucket(m2 uint32) uint32 {
	if m2 == 0 {
		return 0
	}
	b := math.Round(8 * math.Log2(float64(m2)))
	return uint32(math.Round(math.Pow(2, b/8)))
}

// nePublicKRow — the K row as served on the public tier (29 B, same layout):
// bucketed p50, p10/p90 bucketed or 0 where n_units < 10.
func nePublicKRow(row []byte) []byte {
	out := append([]byte(nil), row...)
	n := binary.LittleEndian.Uint16(row[8:])
	p10, p50, p90 := binary.LittleEndian.Uint32(row[10:]), binary.LittleEndian.Uint32(row[14:]), binary.LittleEndian.Uint32(row[18:])
	binary.LittleEndian.PutUint32(out[14:], neBucket(p50))
	if n >= 10 {
		binary.LittleEndian.PutUint32(out[10:], neBucket(p10))
		binary.LittleEndian.PutUint32(out[18:], neBucket(p90))
	} else {
		binary.LittleEndian.PutUint32(out[10:], 0)
		binary.LittleEndian.PutUint32(out[18:], 0)
	}
	return out
}

// neDecodeLU — one LU row → map in tier vocabulary (u8 left 0–255).
func neDecodeLU(b []byte, public bool, want map[string]bool) map[string]interface{} {
	lu := make([]int, 9)
	for j := range lu {
		lu[j] = int(b[12+j])
	}
	vals := map[string]interface{}{
		"kg_share": b[8], "land": b[9], "cov": b[10],
		"flags":     neDecodeFlags(b[11]),
		"lu":        lu,
		"bld_cover": b[21], "bld_n": binary.LittleEndian.Uint16(b[22:]), "gk": b[24], "n_parc": b[25],
		"line_m_ha": binary.LittleEndian.Uint16(b[26:]),
	}
	if b[28] != 0xFF {
		vals["orient_coh"], vals["orient_axis"] = b[28], int(b[29])*2
	} else {
		vals["orient_coh"], vals["orient_axis"] = nil, nil
	}
	out := make(map[string]interface{}, len(vals))
	for k, v := range vals {
		name := neFieldName(public, k)
		if want == nil || want[name] || want[k] {
			out[name] = v
		}
	}
	return out
}

// neDecodeFlags — the LU row's flags byte; `superseded` (bit 7, serve time
// only) appears only when set: the cell's logged chunk no longer holds it
// and no other KG's view does, so the index/bev row is served as a fallback.
func neDecodeFlags(f byte) map[string]bool {
	m := map[string]bool{"declared_unknown": f&1 != 0, "normalised": f&2 != 0}
	if f&neFlagSuperseded != 0 {
		m["superseded"] = true
	}
	return m
}

// neDecodeK — one K row → map (public: bucketed/withheld percentiles).
func neDecodeK(b []byte, public bool, want map[string]bool) map[string]interface{} {
	if public {
		b = nePublicKRow(b)
	}
	n := binary.LittleEndian.Uint16(b[8:])
	vals := map[string]interface{}{
		"n_parc": n, "parc_p50": binary.LittleEndian.Uint32(b[14:]),
		"ez_n": binary.LittleEndian.Uint16(b[22:]), "bld_n": binary.LittleEndian.Uint16(b[25:]),
	}
	if p := binary.LittleEndian.Uint32(b[10:]); p != 0 || !public {
		vals["parc_p10"], vals["parc_p90"] = p, binary.LittleEndian.Uint32(b[18:])
	} else {
		vals["parc_p10"], vals["parc_p90"] = nil, nil
	}
	if b[24] != 0xFF {
		vals["parc_per_ez"] = float64(b[24]) / 10
	} else {
		vals["parc_per_ez"] = nil
	}
	if v := binary.LittleEndian.Uint16(b[27:]); v != 0xFFFF {
		vals["bld_p50"] = v
	} else {
		vals["bld_p50"] = nil
	}
	out := make(map[string]interface{}, len(vals))
	for k, v := range vals {
		name := neFieldName(public, k)
		if want == nil || want[name] || want[k] {
			out[name] = v
		}
	}
	return out
}

// neObsGroups — srtm's 9 cover groups, same order as our lu[9] / ns-groups-1.
var neObsGroups = []string{"geb", "bau", "acker", "gruen", "wald", "wasser", "verkehr", "alpen", "sonst"}
var neObsAspect = []string{"N", "NE", "E", "SE", "S", "SW", "W", "NW", "flat"}
var neObsVerdict = []string{"consistent", "forest_loss", "forest_gain", "sealed_new", "structure_new", "green_new", "unknown"}
var neObsPhenology = []string{"unknown", "forest", "crop", "pasture", "seasonal_vegetation", "road_or_bare"}

// neObsRecordV41 — the layout srtm publishes at /api/v1/cells/dict
// (neo1_record "<Q9sBBBBBBBBBBbBBHBBBBBbbBB", 41 B). Rows stay opaque for
// ingest/digest; this is read-side decoding only and is applied only when the
// peer's header says record_bytes == 41 and fields[] matches this layout.
const neObsRecordV41 = 41

var neObsFieldsV41 = []string{"cell_id", "obs_lu[9]", "obs_bld_cover", "obs_bld_n", "h_mean", "h_p90", "h_max", "tree_n", "tree_h_max", "crown_frac", "ndvi_mean", "ndvi_amp", "dh_mean", "flight_years", "verdict", "elev_mean", "slope_mean", "slope_p90", "aspect_dom", "roughness", "tri", "tpi", "dtm_change", "hansen_loss_year", "phenology"}

// neDecodeObs — one observed row → map. Always carries row_hex/record_bytes/fields
// (the contractual opaque form); when the row is srtm's 41-B v2.4 record the
// fields are decoded too (obs_lu normalised to fractions like lu, units per
// srtm's dict; 255/−128 sentinels → null).
func neDecodeObs(b []byte, fields []string) map[string]interface{} {
	out := map[string]interface{}{"row_hex": hex.EncodeToString(b[8:]), "record_bytes": len(b), "fields": fields}
	if len(b) != neObsRecordV41 || !neObsLayoutV41(fields) {
		return out
	}
	u8 := func(i int) interface{} {
		if b[i] == 255 {
			return nil
		}
		return int(b[i])
	}
	scale := func(i int, div float64) interface{} {
		if b[i] == 255 {
			return nil
		}
		return math.Round(float64(b[i])/div*1000) / 1000
	}
	i8 := func(i int, mul float64) interface{} {
		v := int8(b[i])
		if v == -128 {
			return nil
		}
		return float64(v) * mul
	}
	code := func(i int, tab []string) interface{} {
		if b[i] == 255 || int(b[i]) >= len(tab) {
			return nil
		}
		return tab[b[i]]
	}
	cover := make(map[string]float64, 9)
	lu := make([]float64, 9)
	for j := range lu {
		lu[j] = math.Round(float64(b[8+j])/255*1000) / 1000
		cover[neObsGroups[j]] = lu[j]
	}
	out["obs_lu"] = lu
	out["cover"] = cover
	out["obs_bld_cover"] = scale(17, 255)
	out["obs_bld_n"] = int(b[18])
	out["h_mean"], out["h_p90"], out["h_max"] = scale(19, 2), scale(20, 2), scale(21, 2)
	out["tree_n"] = int(b[22])
	out["tree_h_max"] = scale(23, 2)
	out["crown_frac"] = scale(24, 254)
	if b[25] != 255 {
		out["ndvi_mean"] = math.Round((float64(b[25])/254*2-1)*1000) / 1000
	} else {
		out["ndvi_mean"] = nil
	}
	out["ndvi_amp"] = scale(26, 254)
	out["dh_mean"] = i8(27, 0.25)
	years := []int{}
	for i := 0; i < 8; i++ {
		if b[28]&(1<<i) != 0 {
			years = append(years, 2020+i)
		}
	}
	out["flight_years"] = years
	out["verdict"] = code(29, neObsVerdict)
	if e := binary.LittleEndian.Uint16(b[30:]); e != 65535 {
		out["elev_mean"] = int(e)
	} else {
		out["elev_mean"] = nil
	}
	out["slope_mean"], out["slope_p90"] = u8(32), u8(33)
	out["aspect_dom"] = code(34, neObsAspect)
	out["roughness"], out["tri"] = scale(35, 10), scale(36, 10)
	out["tpi"] = i8(37, 0.25)
	out["dtm_change"] = i8(38, 0.25)
	switch b[39] {
	case 0, 255:
		out["hansen_loss_year"] = nil
	default:
		out["hansen_loss_year"] = 2000 + int(b[39])
	}
	out["phenology"] = code(40, neObsPhenology)
	return out
}

func neObsLayoutV41(fields []string) bool {
	if len(fields) != len(neObsFieldsV41) {
		return false
	}
	for i, f := range fields {
		if f != neObsFieldsV41[i] {
			return false
		}
	}
	return true
}

func neObsFields(k *neKG) []string {
	h := k.layerHeader("obs")
	if h == nil {
		return nil
	}
	fs, _ := h["fields"].([]interface{})
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		if s, ok := f.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// ── per-IP token bucket (20 r/s, burst 60) ───────────────────────────────────

type neBucketT struct {
	tokens float64
	last   time.Time
}

var (
	neRateMu      sync.Mutex
	neRateBuckets = map[string]*neBucketT{}
	neRatePerSec  = 20.0
	neRateBurst   = 60.0
)

// neClientIP — the hop the exe.dev proxy saw: the LAST X-Forwarded-For entry
// (the proxy appends the real client; earlier entries are client-supplied and
// would let a caller pick its own bucket). Without XFF the TCP peer counts.
// Returns "" (= exempt) for loopback without XFF: operator curl, tests, hey —
// proxied traffic always carries XFF (exe.dev docs/proxy.md), so no public
// client can reach this carve-out.
func neClientIP(r *http.Request) string {
	if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
		if i := strings.LastIndexByte(xf, ','); i >= 0 {
			xf = xf[i+1:]
		}
		return strings.TrimSpace(xf)
	}
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
		return ""
	}
	return h
}

// neRateAllow — O(1), no queue; sweeps idle buckets when the map grows.
func neRateAllow(ip string, now time.Time) (bool, int) {
	neRateMu.Lock()
	defer neRateMu.Unlock()
	b := neRateBuckets[ip]
	if b == nil {
		if len(neRateBuckets) > 10000 {
			for k, v := range neRateBuckets {
				if now.Sub(v.last) > time.Minute {
					delete(neRateBuckets, k)
				}
			}
		}
		b = &neBucketT{tokens: neRateBurst, last: now}
		neRateBuckets[ip] = b
	}
	b.tokens = math.Min(neRateBurst, b.tokens+now.Sub(b.last).Seconds()*neRatePerSec)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, int(math.Ceil((1 - b.tokens) / neRatePerSec))
}

// neReadGate — any tier; rate-limited per IP.
func neReadGate(w http.ResponseWriter, r *http.Request) bool {
	ip := neClientIP(r)
	if ip == "" {
		return true
	}
	ok, wait := neRateAllow(ip, time.Now())
	if !ok {
		w.Header().Set("Retry-After", strconv.Itoa(max(wait, 1)))
		w.Header().Set("Cache-Control", "no-store")
		apiError(w, "rate limit: 20 requests/s per client (burst 60)", http.StatusTooManyRequests)
		return false
	}
	return true
}

// neWriteGate — private/contrib tier AND the token named neWriterName ({OBS_NAME}).
func neWriteGate(w http.ResponseWriter, r *http.Request) bool {
	if !isNEWriter(r) {
		tierNotFound(w)
		return false
	}
	return true
}

// ── request helpers ──────────────────────────────────────────────────────────

type neReq struct {
	public bool
	layers []string
	format string
	fields map[string]bool
	base   string // "" or the private prefix for links in JSON bodies
	// bevFirst: the declared row of a cell comes from the KG's `bev` section
	// (the peer's current bevdirect build, the cell set its obs rows were
	// validated against) and from `lu` only where bev lacks the cell. Set by
	// the landing glyph (tiles, cell card); the layered API keeps `lu` pure.
	bevFirst bool
}

func neParseReq(r *http.Request, defFormat string) (*neReq, string) {
	q := r.URL.Query()
	rq := &neReq{public: !isPrivateTier(r), format: defFormat}
	if !rq.public {
		rq.base = privateTierPrefix
	}
	rq.layers = []string{"lu", "obs"}
	if l := q.Get("layers"); l != "" {
		rq.layers = rq.layers[:0]
		for _, x := range splitCSV(l) {
			switch x {
			case "lu":
			case "declared":
				// the declared VIEW: chunk-log view → bev → lu per cell
				// (ne_change_declared.go, docs/ne-change.md §4b); `lu` stays the index epoch
				x, rq.bevFirst = "lu", true
			case "obs", "observed":
				x = "obs"
			case "bev", "lu_bevdirect":
				x = "bev"
			case "chg", "change":
				x = "chg" // cumulative confirmed change per cell (ne_change.go); json only
			default:
				return nil, "unknown layer " + x + " (lu|bev|obs|chg)"
			}
			rq.layers = append(rq.layers, x)
		}
		if rq.bevFirst && strings.Contains(l, "lu") {
			return nil, "layers lu and declared are exclusive (lu = index epoch, declared = the current view)"
		}
	}
	if f := q.Get("format"); f != "" {
		rq.format = f
	} else if strings.Contains(r.Header.Get("Accept"), "application/json") {
		rq.format = "json"
	}
	switch rq.format {
	case "zstd", "bin", "json":
	default:
		return nil, "format must be zstd|bin|json"
	}
	if f := q.Get("fields"); f != "" {
		rq.fields = map[string]bool{}
		for _, x := range splitCSV(f) {
			rq.fields[x] = true
		}
	}
	return rq, ""
}

// layerVisible — obs is private until released.
func (rq *neReq) layerVisible(name string) bool { return !(rq.public && name == "obs" && !neObsPublic) }

func neSetCache(w http.ResponseWriter, rq *neReq, partial bool) {
	switch {
	case partial:
		w.Header().Set("Cache-Control", "no-store")
	case rq.wantsLayer("chg"), rq.bevFirst: // moves with the chunk log (peer reports), not immutable
		w.Header().Set("Cache-Control", "public, max-age=300")
	case rq.public:
		w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	default:
		w.Header().Set("Cache-Control", "private, max-age=3600")
	}
}

func neNotModified(w http.ResponseWriter, r *http.Request, etag string) bool {
	w.Header().Set("ETag", etag)
	if strings.Contains(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	return false
}

// ── GET /ne (root) ───────────────────────────────────────────────────────────

func handleNERoot(w http.ResponseWriter, r *http.Request, rq *neReq) {
	b := rq.base + "/api/v1/ne"
	st := NEStoreStats()
	doc := map[string]interface{}{
		"product":   "NE cells — declared land-use statistics per H3 cell (res 12, K records res 10), observed layer by " + peers().ObsName,
		"spec":      rq.base + "/api/v1/docs/ne.md",
		"frozen":    neFrozen,
		"kgs_built": st["kgs"], "chunks": st["chunk_refs"], "dicts": st["dicts"],
		"obs_public": neObsPublic,
		"endpoints": []string{
			"GET " + b + "/manifest[?fields=digests]", "GET " + b + "/{kg}[?layers=lu,bev,obs][&format=json]", "GET " + b + "/{kg}/head",
			"GET " + b + "/dict/{id}", "GET " + b + "/chunk/{h3_res9}[?format=zstd|bin|json]", "GET " + b + "/cell/{h3}",
			"GET " + b + "/cells?ids=…|lon=&lat=[&k=]|bbox=w,s,e,n[&list=1]", "POST " + b + "/cells {ids:[…]}",
			"GET " + rq.base + "/api/v1/context?lon=&lat=&include=ne",
		},
		"limits": map[string]interface{}{"ids_get": 512, "ids_post": 16384, "cells_per_answer": 16384, "bbox_span_deg": 0.05, "k_max": 3, "rate": "20 r/s, burst 60"},
		"notice": NENotice(),
	}
	if !rq.public {
		doc["endpoints"] = append(doc["endpoints"].([]string), "POST "+b+"/{kg}/obs", "POST|GET "+b+"/{kg}/report", "GET "+b+"/draft/ne_cells.tar.gz")
		doc["reference"] = "ne_cells/ python package (pinned shapely 2.1.2 + h3 4.5.0) — " + b + "/draft/ne_cells.tar.gz"
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, doc)
}

// ── GET /ne/dict/{id} ────────────────────────────────────────────────────────

func handleNEDict(w http.ResponseWriter, r *http.Request, id string) {
	if !neDictIDRe.MatchString(id) {
		apiError(w, "dictionary id must look like lu-1", 400)
		return
	}
	f, err := os.Open(neDictPath(id))
	if err != nil {
		apiError(w, "no such dictionary", 404)
		return
	}
	defer f.Close()
	fi, _ := f.Stat()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("ETag", `"ned-`+id+`"`)
	http.ServeContent(w, r, id+".zd", fi.ModTime(), f)
}

// ── GET /ne/{kg} · /ne/{kg}/head ─────────────────────────────────────────────

func neUnbuilt(w http.ResponseWriter, rq *neReq, kg string, missing []string) {
	e := neEntry(kg)
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", fmt.Sprint(e.RetryAfter))
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 404, map[string]interface{}{
		"error": "ne cells not built for this KG", "kg": kg, "layers_missing": missing,
		"status": e.Status, "queue_position": e.QueuePos, "retry_after_s": e.RetryAfter,
		"manifest": rq.base + "/api/v1/ne/manifest", "notice": NENotice(),
	})
}

func handleNEWhole(w http.ResponseWriter, r *http.Request, rq *neReq, kg string) {
	k := neStore.getOrConvert(kg)
	var present, missing []string
	var view *neDeclView // layers=declared: the assembled view replaces the stored lu section
	for _, l := range rq.layers {
		if l == "lu" && rq.bevFirst && k != nil {
			if view = neDeclViewOf(kg); view != nil {
				present = append(present, l)
				continue
			}
		}
		if k != nil && k.Hdr.Layers[l] != nil && rq.layerVisible(l) && !(l == "lu" && rq.bevFirst) {
			present = append(present, l)
		} else {
			missing = append(missing, l)
		}
	}
	if len(present) == 0 {
		neUnbuilt(w, rq, kg, missing)
		return
	}
	etag := k.etag(present)
	if view != nil {
		etag = strings.TrimSuffix(etag, `"`) + "+" + strings.Trim(view.etag(false), `"`) + `"`
	}
	if rq.public {
		etag = strings.TrimSuffix(etag, `"`) + `-p"`
	}
	if len(missing) > 0 {
		w.Header().Set("X-NE-Layers-Missing", strings.Join(missing, ","))
	}
	switch {
	case view != nil:
		w.Header().Set("Cache-Control", "public, max-age=300") // the view moves with peer reports
	case rq.public:
		w.Header().Set("Cache-Control", "public, max-age=86400, immutable, stale-while-revalidate=604800")
	default:
		w.Header().Set("Cache-Control", "private, max-age=3600")
	}
	if neNotModified(w, r, etag) {
		return
	}
	var blob []byte
	var err error
	if view != nil {
		blob = view.section(k, rq.public)
		w.Header().Set("X-NE-Declared-Source", view.Source)
		rest := make([]string, 0, len(present))
		for _, l := range present {
			if l != "lu" {
				rest = append(rest, l)
			}
		}
		present = append([]string{"lu"}, rest...) // section order: the view first, then the stored sections
		if len(rest) > 0 {
			var more []byte
			more, err = neStore.materialise(k, rest, rq.public)
			blob = append(blob, more...)
		}
	} else {
		blob, err = neStore.materialise(k, present, rq.public)
	}
	if err != nil {
		apiError(w, "store: "+err.Error(), 500)
		return
	}
	if rq.format != "json" {
		w.Header().Set("Content-Type", "application/vnd.ne-cells")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="%s.nec"`, kg))
		http.ServeContent(w, r, "", k.Mod, bytesReader(blob))
		return
	}
	secs, err := parseNESections(blob)
	if err != nil {
		apiError(w, "stored container corrupt: "+err.Error(), 500)
		return
	}
	out := map[string]interface{}{"kg": kg, "layers_missing": missing, "notice": NENotice()}
	var ss []interface{}
	for i, s := range secs {
		m := map[string]interface{}{"header": s.Header, "layer": present[i]}
		if s.Magic == "NEC1" {
			cells, kcells, err := neDecodeNEC1Tier(s, rq.public, rq.fields)
			if err != nil {
				apiError(w, "decode: "+err.Error(), 500)
				return
			}
			m["cells"], m["kcells"] = cells, kcells
		} else {
			m["body_deflated_bytes"] = len(s.Body)
		}
		ss = append(ss, m)
	}
	out["sections"] = ss
	writeJSON(w, 200, out)
}

func neDecodeNEC1Tier(s neSection, public bool, want map[string]bool) (map[string]interface{}, map[string]interface{}, error) {
	raw, err := neInflate(s.Body)
	if err != nil {
		return nil, nil, err
	}
	n, _ := s.Header["cells_n"].(float64)
	kn, _ := s.Header["kcells_n"].(float64)
	if int(n)*neLURowBytes+int(kn)*neKRowBytes != len(raw) {
		return nil, nil, fmt.Errorf("record block size")
	}
	cells := make(map[string]interface{}, int(n))
	for i := 0; i < int(n); i++ {
		b := raw[i*neLURowBytes : (i+1)*neLURowBytes]
		cells[neCellHex(binary.LittleEndian.Uint64(b))] = neDecodeLU(b, public, want)
	}
	kcells := make(map[string]interface{}, int(kn))
	off := int(n) * neLURowBytes
	for i := 0; i < int(kn); i++ {
		b := raw[off+i*neKRowBytes : off+(i+1)*neKRowBytes]
		kcells[neCellHex(binary.LittleEndian.Uint64(b))] = neDecodeK(b, public, want)
	}
	return cells, kcells, nil
}

func handleNEHead(w http.ResponseWriter, r *http.Request, rq *neReq, kg string) {
	k := neStore.getOrConvert(kg)
	if k == nil {
		neUnbuilt(w, rq, kg, rq.layers)
		return
	}
	etag := strings.TrimSuffix(k.etag([]string{"lu", "bev", "obs"}), `"`) + `-h"`
	neSetCache(w, rq, false)
	w.Header().Set("Cache-Control", "public, max-age=60")
	if neNotModified(w, r, etag) {
		return
	}
	out := map[string]interface{}{"kg": kg, "dir_n": len(k.Hdr.Dir), "bytes": k.Size, "chunk_res": k.Hdr.ChunkRes, "notice": NENotice()}
	for _, name := range neLayerOrder {
		l := k.Hdr.Layers[name]
		if l == nil || !rq.layerVisible(name) {
			out[name] = nil
			continue
		}
		var hdr interface{}
		if rq.public {
			var m map[string]interface{}
			_ = json.Unmarshal(neRewriteHeaderPublic(k.layerHeader(name)), &m)
			hdr = m
		} else {
			hdr = k.layerHeader(name)
		}
		out[name] = map[string]interface{}{"header": hdr, "digest": l.Digest, "rows": l.Rows, "row_bytes": l.RowBytes, "k_rows": l.KRows,
			"dict": l.Dict, "built_at": l.BuiltAt, "bytes": l.Bytes}
	}
	writeJSON(w, 200, out)
}

// ── GET /ne/chunk/{h3_res9} ──────────────────────────────────────────────────

// neChunkETag — sha of the chunk refs' digests → immutable per digest tuple.
func neChunkETag(hits []neChunkHit, layers []string, public bool, bevFirst ...bool) string {
	h := sha256.New()
	view := len(bevFirst) > 0 && bevFirst[0]
	for _, hit := range hits {
		for _, l := range layers {
			if l == "lu" && view { // the declared view: lu+bev digests + chunk-log state
				fmt.Fprintf(h, "%s:declared:%s;", hit.KG.KG, neDeclaredDigest(hit.KG, true))
				continue
			}
			if x := hit.KG.Hdr.Layers[l]; x != nil {
				fmt.Fprintf(h, "%s:%s:%s;", hit.KG.KG, l, x.Digest)
			}
		}
	}
	if public {
		h.Write([]byte("p"))
	}
	for _, l := range layers {
		if l == "chg" { // change state moves with the log, not with the frozen layers
			for _, hit := range hits {
				fmt.Fprintf(h, "chg:%d;", neChangeLogSize(hit.KG.KG))
			}
		}
	}
	return `"nec-` + hex.EncodeToString(h.Sum(nil))[:16] + `"`
}

func nePendingKGs(cands []string) []string {
	var out []string
	for _, kg := range cands {
		if neStore.get(kg) == nil {
			out = append(out, kg)
		}
	}
	sort.Strings(out)
	return out
}

func handleNEChunk(w http.ResponseWriter, r *http.Request, rq *neReq, hexID string) {
	c9, res, err := neParseCell(hexID)
	if err != nil || res != neChunkRes {
		apiError(w, "chunk key must be a res-9 H3 id", 400)
		return
	}
	hits := neStore.chunkHits(c9)
	lon, lat := neCellCentre(c9)
	pending := nePendingKGs(KGCodesNearPointMem(lon, lat, 0.01))
	var layers []string
	var missing []string
	for _, l := range rq.layers {
		if rq.layerVisible(l) {
			layers = append(layers, l)
		} else {
			missing = append(missing, l)
		}
	}
	if len(hits) == 0 {
		w.Header().Set("Cache-Control", "no-store")
		if len(pending) > 0 {
			w.Header().Set("X-NE-Pending-KGs", strings.Join(pending, ","))
			writeJSON(w, 404, map[string]interface{}{"error": "no cells for this chunk yet", "chunk": hexID, "pending_kgs": pending, "notice": NENotice()})
			return
		}
		writeJSON(w, 404, map[string]interface{}{"error": "no cells for this chunk", "chunk": hexID, "notice": NENotice()})
		return
	}
	// KG shares of the chunk = rows per KG / total rows (lu)
	var total int64
	for _, h := range hits {
		if h.Dir >= 0 {
			total += h.KG.Hdr.Dir[h.Dir].LU[2]
		}
	}
	kgShares := make([]string, 0, len(hits))
	for _, h := range hits {
		var n int64
		if h.Dir >= 0 {
			n = h.KG.Hdr.Dir[h.Dir].LU[2]
		}
		kgShares = append(kgShares, fmt.Sprintf("%s:%.2f", h.KG.KG, float64(n)/float64(max(total, 1))))
	}
	w.Header().Set("X-NE-KGs", strings.Join(kgShares, ","))
	if len(missing) > 0 {
		w.Header().Set("X-NE-Layers-Missing", strings.Join(missing, ","))
	}
	if len(pending) > 0 {
		w.Header().Set("X-NE-Pending-KGs", strings.Join(pending, ","))
	}
	neSetCache(w, rq, len(pending) > 0)
	if neNotModified(w, r, neChunkETag(hits, layers, rq.public, rq.bevFirst)) {
		return
	}
	if rq.format == "zstd" && rq.bevFirst {
		apiError(w, "layers=declared is assembled per cell (not a stored section) — use format=bin or format=json, or layers=lu for the stored index-epoch frames", 400)
		return
	}
	switch rq.format {
	case "zstd":
		var body []byte
		var frames []string
		dicts := map[string]bool{}
		for _, h := range hits {
			for _, l := range layers {
				fr, err := neStore.chunkFrame(h.KG, h.Dir, l)
				if err != nil {
					apiError(w, "store: "+err.Error(), 500)
					return
				}
				if fr == nil {
					continue
				}
				lay := h.KG.Hdr.Layers[l]
				frames = append(frames, fmt.Sprintf("%s:%s:%d:%d:%d:%d", h.KG.KG, l, len(body), len(fr), h.KG.Hdr.Dir[h.Dir].layer(l)[2], lay.RowBytes))
				if lay.Dict != "" {
					dicts[lay.Dict] = true
				}
				body = append(body, fr...)
			}
		}
		dl := make([]string, 0, len(dicts))
		for d := range dicts {
			dl = append(dl, d)
		}
		sort.Strings(dl)
		w.Header().Set("X-NE-Dict", strings.Join(dl, ","))
		w.Header().Set("X-NE-Frames", strings.Join(frames, ",")) // kg:layer:off:len:rows:row_bytes
		w.Header().Set("Content-Type", "application/zstd")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	default:
		res := neCollect(nil, neChildren(c9, neCellRes), layers, rq)
		res.pending = pending
		neWriteCells(w, rq, res)
	}
}

// ── collection (cell / cells / bbox) ─────────────────────────────────────────

type neResult struct {
	ids     []uint64           // res-12 cells found, sorted
	lu      [][]byte           // row per id
	kgs     []map[string]uint8 // kg→share per id
	obs     [][]byte           // row per id or nil
	obsFlds []string
	obsRec  string   // the peer's struct format of the obs row (header "record")
	kIDs    []uint64 // res-10 cells with a K record, sorted
	kRows   [][]byte
	pending []string
	etagSrc map[string]string // kg → lu digest, for the ETag
}

// neCollect gathers rows for the res-12 ids (and K records of their parents
// plus the explicit res-10 ids). Work is grouped per res-9 chunk: the sorted
// ids are walked run by run, each run = one chunkHits + one chunk-LRU fetch
// per (KG, layer), then a binary search per id inside the inflated rows. The
// earlier per-id form did the index lookup, the LRU lock and a key string
// concat 11 k times per 0.02° box. kg→share maps are built only for the
// JSON encoder (bin never reads them).
func neCollect(kIDs []uint64, ids []uint64, layers []string, rq *neReq) *neResult {
	res := &neResult{etagSrc: map[string]string{}}
	wantLU, wantObs := false, false
	for _, l := range layers {
		switch l {
		case "lu":
			wantLU = true
		case "obs":
			wantObs = true
		}
	}
	wantKGs := rq == nil || rq.format != "bin"
	sortU64(ids)
	kSet := make(map[uint64]bool, len(kIDs)+len(ids)/49+1)
	for _, k := range kIDs {
		kSet[k] = true
	}
	res.ids = make([]uint64, 0, len(ids))
	if wantLU {
		res.lu = make([][]byte, 0, len(ids))
	}
	if wantKGs {
		res.kgs = make([]map[string]uint8, 0, len(ids))
	}
	type hitRows struct {
		kg     *neKG
		lu     []byte // nil when the KG has no lu rows in this chunk
		luRB   int
		bev    []byte // the peer's current bevdirect build of the same KG (neDeclaredDigest)
		bevRB  int
		chg    []byte   // the declared view on the KG's chunk log, newer than bev (ne_change_declared.go)
		logged []uint64 // res-10 chunks the log is authoritative for (cells absent there are not read from bev/lu)
		obs    []byte
		obsRB  int
	}
	var hrs []hitRows
	for i := 0; i < len(ids); {
		c9 := h3Parent(ids[i], neChunkRes)
		j := i + 1
		for j < len(ids) && h3Parent(ids[j], neChunkRes) == c9 {
			j++
		}
		run := ids[i:j]
		i = j
		hits := neStore.chunkHits(c9)
		if len(hits) == 0 {
			continue
		}
		hrs = hrs[:0]
		for _, h := range hits {
			hr := hitRows{kg: h.KG}
			if l := h.KG.Hdr.Layers["lu"]; l != nil {
				hr.lu, _ = neStore.chunkRows(h.KG, h.Dir, "lu")
				hr.luRB = l.RowBytes
			}
			if rq != nil && rq.bevFirst {
				if l := h.KG.Hdr.Layers["bev"]; l != nil {
					hr.bev, _ = neStore.chunkRows(h.KG, h.Dir, "bev")
					hr.bevRB = l.RowBytes
				}
				hr.chg = neDeclaredLogRows(h.KG.KG, c9, neDeclaredAfter(h.KG))
				if hr.chg != nil {
					hr.logged = neDeclaredLoggedChunks(h.KG.KG, c9, neDeclaredAfter(h.KG))
				}
			}
			if wantObs {
				if l := h.KG.Hdr.Layers["obs"]; l != nil {
					hr.obs, _ = neStore.chunkRows(h.KG, h.Dir, "obs")
					hr.obsRB = l.RowBytes
				}
			}
			if hr.lu != nil || hr.bev != nil || hr.chg != nil {
				res.etagSrc[h.KG.KG] = neDeclaredDigest(h.KG, rq != nil && rq.bevFirst)
				hrs = append(hrs, hr)
			}
		}
		if len(hrs) == 0 {
			continue
		}
		var last uint64 = math.MaxUint64
		for _, id := range run {
			if id == last {
				continue
			}
			last = id
			var best, sup []byte
			var bestShare, supShare uint8
			var supKG string
			var kgs map[string]uint8
			for _, hr := range hrs {
				// declared row: with bevFirst the newest confirmed chunk-log
				// version (peers' reports), then the peer's posted bevdirect build
				// (the cell set the obs rows were validated against), the index
				// epoch only where both lack the cell (nil slices otherwise).
				// Inside a logged res-10 chunk the log's cell set is authoritative:
				// a bev/lu row there is only a fallback flagged superseded, used
				// when no KG's view holds the cell (ne_change_declared.go).
				row := neFindRow(hr.chg, neLURowBytes, id)
				if row != nil {
					// log rows carry zero register bytes: take them from the stored row
					if old := neFindRow(hr.bev, hr.bevRB, id); old == nil {
						row = neRowWithRegister(row, neFindRow(hr.lu, hr.luRB, id))
					} else {
						row = neRowWithRegister(row, old)
					}
				}
				if row == nil {
					row = neFindRow(hr.bev, hr.bevRB, id)
				}
				if row == nil {
					row = neFindRow(hr.lu, hr.luRB, id)
				}
				if row == nil {
					continue
				}
				if hr.chg != nil && neFindRow(hr.chg, neLURowBytes, id) == nil && neLoggedHas(hr.logged, id) {
					if sup == nil || row[8] > supShare {
						sup, supShare, supKG = row, row[8], hr.kg.KG
					}
					continue
				}
				if wantKGs {
					if kgs == nil {
						kgs = make(map[string]uint8, 1)
					}
					kgs[hr.kg.KG] = row[8]
				}
				if best == nil || row[8] > bestShare {
					best, bestShare = row, row[8]
				}
			}
			if best == nil && sup != nil {
				best = neRowSuperseded(sup)
				if wantKGs {
					kgs = map[string]uint8{supKG: supShare}
				}
			}
			if best == nil {
				continue
			}
			res.ids = append(res.ids, id)
			if wantKGs {
				res.kgs = append(res.kgs, kgs)
			}
			if wantLU {
				res.lu = append(res.lu, best)
			}
			if wantObs {
				var orow []byte
				for _, hr := range hrs {
					if hr.obs == nil {
						continue
					}
					if r := neFindRow(hr.obs, hr.obsRB, id); r != nil {
						orow = r
						if res.obsFlds == nil {
							res.obsFlds = neObsFields(hr.kg)
							if h := hr.kg.layerHeader("obs"); h != nil {
								res.obsRec, _ = h["record"].(string)
							}
						}
						break
					}
				}
				res.obs = append(res.obs, orow)
			}
			kSet[h3Parent(id, neKRes)] = true
		}
	}
	res.kIDs = make([]uint64, 0, len(kSet))
	for k := range kSet {
		res.kIDs = append(res.kIDs, k)
	}
	sortU64(res.kIDs)
	out := res.kIDs[:0]
	for _, k := range res.kIDs {
		if row, kg := neStore.kRowAny(k, "lu"); row != nil {
			out = append(out, k)
			res.kRows = append(res.kRows, row)
			if x := neStore.get(kg); x != nil {
				res.etagSrc[kg] = neDeclaredDigest(x, rq != nil && rq.bevFirst)
			}
		}
	}
	res.kIDs = out
	return res
}

func (res *neResult) etag(rq *neReq) string {
	kgs := make([]string, 0, len(res.etagSrc))
	for kg := range res.etagSrc {
		kgs = append(kgs, kg)
	}
	sort.Strings(kgs)
	h := sha256.New()
	for _, kg := range kgs {
		fmt.Fprintf(h, "%s:%s;", kg, res.etagSrc[kg])
	}
	fmt.Fprintf(h, "n=%d,k=%d,f=%s,p=%v", len(res.ids), len(res.kIDs), rq.format, rq.public)
	return `"nes-` + hex.EncodeToString(h.Sum(nil))[:16] + `"`
}

// neWriteCells — bin (rows, X-NE-Rows) or columnar json.
func neWriteCells(w http.ResponseWriter, rq *neReq, res *neResult) {
	if len(res.pending) > 0 {
		w.Header().Set("X-NE-Pending-KGs", strings.Join(res.pending, ","))
	}
	neSetCache(w, rq, len(res.pending) > 0)
	w.Header().Set("ETag", res.etag(rq))
	if rq.format == "bin" {
		var parts []string
		body := make([]byte, 0, len(res.lu)*neLURowBytes+len(res.kRows)*neKRowBytes+len(res.obs)*48)
		if res.lu != nil {
			for _, r := range res.lu {
				body = append(body, r...)
			}
			parts = append(parts, fmt.Sprintf("lu=%d:%d", neLURowBytes, len(res.lu)))
		}
		for _, r := range res.kRows {
			if rq.public {
				r = nePublicKRow(r)
			}
			body = append(body, r...)
		}
		parts = append(parts, fmt.Sprintf("k=%d:%d", neKRowBytes, len(res.kRows)))
		if res.obs != nil {
			n, rb := 0, 0
			for _, r := range res.obs {
				if r != nil {
					body = append(body, r...)
					n++
					rb = len(r)
				}
			}
			parts = append(parts, fmt.Sprintf("obs=%d:%d", rb, n))
		}
		w.Header().Set("X-NE-Rows", strings.Join(parts, ";"))
		w.Header().Set("X-NE-N", strconv.Itoa(len(res.ids)))
		w.Header().Set("Content-Type", "application/vnd.ne-rows")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
		return
	}
	writeJSON(w, 200, res.columnar(rq))
}

// columnar — ids as hex strings, one array per field.
func (res *neResult) columnar(rq *neReq) map[string]interface{} {
	ids := make([]string, len(res.ids))
	kgs := make([]map[string]uint8, len(res.ids))
	for i, id := range res.ids {
		ids[i] = neCellHex(id)
		if i < len(res.kgs) { // absent when collected for format=bin
			kgs[i] = res.kgs[i]
		}
	}
	out := map[string]interface{}{"n": len(res.ids), "ids": ids, "kgs": kgs, "notice": NENotice()}
	if rq.bevFirst {
		out["declared"] = "view" // `lu` columns are the declared view (chunk-log view → bev → lu per cell), not the index epoch
	}
	if res.lu != nil {
		cols := map[string][]interface{}{}
		for _, row := range res.lu {
			for k, v := range neDecodeLU(row, rq.public, rq.fields) {
				cols[k] = append(cols[k], v)
			}
		}
		out["lu"] = cols
	}
	if len(res.kIDs) > 0 || rq.fields == nil {
		kids := make([]string, len(res.kIDs))
		cols := map[string][]interface{}{}
		for i, id := range res.kIDs {
			kids[i] = neCellHex(id)
			for k, v := range neDecodeK(res.kRows[i], rq.public, rq.fields) {
				cols[k] = append(cols[k], v)
			}
		}
		out["k"] = map[string]interface{}{"n": len(kids), "ids": kids, "cols": cols}
	}
	if res.obs != nil {
		var oids, rows []string
		for i, r := range res.obs {
			if r != nil {
				oids = append(oids, ids[i])
				rows = append(rows, hex.EncodeToString(r[8:]))
			}
		}
		out["obs"] = map[string]interface{}{"n": len(oids), "ids": oids, "rows_hex": rows, "fields": res.obsFlds}
	}
	if len(res.pending) > 0 {
		out["pending_kgs"] = res.pending
		out["pending"] = true
	}
	if rq != nil && rq.wantsLayer("chg") && res.kgs != nil {
		chg := make([]interface{}, len(res.ids))
		n := 0
		for i, id := range res.ids {
			if c := neCellChange(id, res.kgs[i]); c != nil {
				chg[i] = c
				n++
			}
		}
		out["chg"] = map[string]interface{}{"n": n, "cells": chg, "since": neChangeNational(false)["since"]}
	}
	return out
}

func (rq *neReq) wantsLayer(name string) bool {
	for _, l := range rq.layers {
		if l == name {
			return true
		}
	}
	return false
}

// ── GET /ne/cell/{h3} ────────────────────────────────────────────────────────

func neCellDoc(id uint64, rq *neReq) (map[string]interface{}, bool) {
	lu, kgs, _ := neStore.declaredRow(id, rq.bevFirst)
	if lu == nil {
		return nil, false
	}
	out := map[string]interface{}{"cell": neCellHex(id), "kgs": kgs, "lu": neDecodeLU(lu, rq.public, rq.fields)}
	if lu[11]&neFlagSuperseded != 0 {
		out["superseded"] = true // the index/bev row is the only source left: the logged build of its chunk no longer holds the cell
	}
	if krow, _ := neStore.kRowAny(h3Parent(id, neKRes), "lu"); krow != nil {
		out["k"] = neDecodeK(krow, rq.public, rq.fields)
		out["k_cell"] = neCellHex(h3Parent(id, neKRes))
	} else {
		out["k"] = nil
	}
	out["chg"] = neCellChange(id, kgs)
	out["obs"] = nil
	if rq.layerVisible("obs") {
		if oh, _ := neStore.cellRows(id, "obs"); len(oh) > 0 {
			out["obs"] = neDecodeObs(oh[0].Row, neObsFields(neStore.get(oh[0].KG)))
		}
	}
	return out, true
}

func handleNECell(w http.ResponseWriter, r *http.Request, rq *neReq, hexID string) {
	id, res, err := neParseCell(hexID)
	if err != nil {
		apiError(w, err.Error(), 400)
		return
	}
	lon, lat := neCellCentre(id)
	pending := nePendingKGs(KGCodesNearPointMem(lon, lat, 0.005))
	switch res {
	case neChunkRes:
		http.Redirect(w, r, rq.base+"/api/v1/ne/chunk/"+hexID+"?"+r.URL.RawQuery, http.StatusFound)
		return
	case neCellRes:
		doc, ok := neCellDoc(id, rq)
		if !ok {
			w.Header().Set("Cache-Control", "no-store")
			if len(pending) > 0 {
				w.Header().Set("X-NE-Pending-KGs", strings.Join(pending, ","))
			}
			writeJSON(w, 404, map[string]interface{}{"error": "no cell", "cell": hexID, "pending_kgs": pending, "pending": len(pending) > 0, "notice": NENotice()})
			return
		}
		res := neCollect(nil, []uint64{id}, []string{"lu"}, rq)
		res.pending = pending
		doc["notice"] = NENotice()
		if len(pending) > 0 {
			doc["pending_kgs"] = pending
			w.Header().Set("X-NE-Pending-KGs", strings.Join(pending, ","))
		}
		neSetCache(w, rq, len(pending) > 0)
		etag := res.etag(rq)
		if doc["chg"] != nil { // a cell with change history: the block moves with the log
			w.Header().Set("Cache-Control", "public, max-age=300")
			etag = etag[:len(etag)-1] + "-chg" + strconv.FormatInt(neChangeLogSizeAny(doc["kgs"]), 10) + `"`
		}
		if neNotModified(w, r, etag) {
			return
		}
		writeJSON(w, 200, doc)
	case neKRes:
		krow, _ := neStore.kRowAny(id, "lu")
		rq.format = "json" // this route is always columnar JSON (needs kg→share per cell)
		res := neCollect([]uint64{id}, neChildren(id, neCellRes), rq.layers, rq)
		res.pending = pending
		if krow == nil && len(res.ids) == 0 {
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, 404, map[string]interface{}{"error": "no cell", "cell": hexID, "pending_kgs": pending, "pending": len(pending) > 0, "notice": NENotice()})
			return
		}
		neSetCache(w, rq, len(pending) > 0)
		if neNotModified(w, r, res.etag(rq)) {
			return
		}
		doc := res.columnar(rq)
		doc["cell"] = hexID
		if krow != nil {
			doc["k_record"] = neDecodeK(krow, rq.public, rq.fields)
		} else {
			doc["k_record"] = nil
		}
		doc["children"] = doc["ids"]
		writeJSON(w, 200, doc)
	default:
		apiError(w, "cell must be res 12 (row), 10 (K record) or 9 (chunk)", 400)
	}
}

// ── /ne/cells ────────────────────────────────────────────────────────────────

const (
	neMaxIDsGet   = 512
	neMaxIDsPost  = 16384
	neMaxCells    = 16384
	neMaxBBoxSpan = 0.05
	neMaxK        = 3
)

func handleNECells(w http.ResponseWriter, r *http.Request, rq *neReq) {
	q := r.URL.Query()
	var idStrs []string
	maxIDs := neMaxIDsGet
	if r.Method == http.MethodPost {
		body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
		if err != nil {
			apiError(w, "read", 400)
			return
		}
		var in struct {
			IDs []string `json:"ids"`
		}
		if err := json.Unmarshal(body, &in); err != nil {
			apiError(w, "body must be {\"ids\":[…]}", 400)
			return
		}
		idStrs, maxIDs = in.IDs, neMaxIDsPost
	} else if s := q.Get("ids"); s != "" {
		idStrs = splitCSV(s)
	}
	layers := make([]string, 0, len(rq.layers))
	for _, l := range rq.layers {
		if rq.layerVisible(l) {
			layers = append(layers, l)
		}
	}
	switch {
	case len(idStrs) > 0:
		if len(idStrs) > maxIDs {
			apiError(w, fmt.Sprintf("too many ids (max %d)", maxIDs), 413)
			return
		}
		var ids, kIDs []uint64
		pendSet := map[string]bool{}
		seenC9 := map[uint64]bool{}
		for _, s := range idStrs {
			id, res, err := neParseCell(strings.TrimSpace(s))
			if err != nil {
				apiError(w, err.Error(), 400)
				return
			}
			switch res {
			case neCellRes:
				ids = append(ids, id)
			case neKRes:
				kIDs = append(kIDs, id)
				ids = append(ids, neChildren(id, neCellRes)...)
			case neChunkRes:
				ids = append(ids, neChildren(id, neCellRes)...)
			default:
				apiError(w, "ids must be res 9, 10 or 12: "+s, 400)
				return
			}
			if len(ids) > neMaxCells {
				apiError(w, fmt.Sprintf("more than %d cells requested", neMaxCells), 413)
				return
			}
			// pending KGs once per res-9 chunk (≈0.1 km²), not per id: ids of a
			// batch cluster, so this is a handful of centre calls + bbox scans
			if c9 := h3Parent(id, neChunkRes); !seenC9[c9] {
				seenC9[c9] = true
				lon, lat := neCellCentre(c9)
				for _, kg := range nePendingKGs(KGCodesNearPointMem(lon, lat, 0.005)) {
					pendSet[kg] = true
				}
			}
		}
		res := neCollect(kIDs, ids, layers, rq)
		for kg := range pendSet {
			res.pending = append(res.pending, kg)
		}
		sort.Strings(res.pending)
		if neNotModified(w, r, res.etag(rq)) {
			return
		}
		neWriteCells(w, rq, res)
	case q.Get("bbox") != "":
		p := splitCSV(q.Get("bbox"))
		var bb [4]float64
		if len(p) != 4 {
			apiError(w, "bbox=w,s,e,n", 400)
			return
		}
		for i := range p {
			v, err := strconv.ParseFloat(p[i], 64)
			if err != nil {
				apiError(w, "bbox=w,s,e,n", 400)
				return
			}
			bb[i] = v
		}
		if bb[2] <= bb[0] || bb[3] <= bb[1] {
			apiError(w, "bbox must be w<e and s<n", 400)
			return
		}
		if bb[2]-bb[0] > neMaxBBoxSpan+1e-9 || bb[3]-bb[1] > neMaxBBoxSpan+1e-9 {
			neSplit(w, bb, fmt.Sprintf("bbox span > %.2f°", neMaxBBoxSpan))
			return
		}
		pending := nePendingKGs(KGCodesInBBoxMem(bb[0], bb[1], bb[2], bb[3]))
		if q.Get("list") == "1" {
			chunks := neBBoxChunks(bb[0], bb[1], bb[2], bb[3])
			var have []string
			var etags []string
			for _, c9 := range chunks {
				if hits := neStore.chunkHits(c9); len(hits) > 0 {
					have = append(have, neCellHex(c9))
					etags = append(etags, neChunkETag(hits, layers, rq.public, rq.bevFirst))
				}
			}
			if have == nil {
				have, etags = []string{}, []string{}
			}
			out := map[string]interface{}{"chunks": have, "etags": etags, "chunk_res": neChunkRes, "dict": neStore.latestDict("lu"), "notice": NENotice()}
			if len(pending) > 0 {
				out["pending_kgs"], out["pending"] = pending, true
				w.Header().Set("X-NE-Pending-KGs", strings.Join(pending, ","))
			}
			neSetCache(w, rq, len(pending) > 0)
			if len(pending) == 0 {
				w.Header().Set("Cache-Control", "public, max-age=3600")
			}
			writeJSON(w, 200, out)
			return
		}
		ids := neBBoxCells(bb[0], bb[1], bb[2], bb[3])
		if len(ids) > neMaxCells {
			neSplit(w, bb, fmt.Sprintf("more than %d cells", neMaxCells))
			return
		}
		res := neCollect(nil, ids, layers, rq)
		res.pending = pending
		if neNotModified(w, r, res.etag(rq)) {
			return
		}
		neWriteCells(w, rq, res)
	case q.Get("lon") != "" || q.Get("lat") != "":
		lon, e1 := strconv.ParseFloat(q.Get("lon"), 64)
		lat, e2 := strconv.ParseFloat(q.Get("lat"), 64)
		if e1 != nil || e2 != nil {
			apiError(w, "lon and lat required", 400)
			return
		}
		k, _ := strconv.Atoi(q.Get("k"))
		if k < 0 || k > neMaxK {
			apiError(w, fmt.Sprintf("k must be 0..%d", neMaxK), 400)
			return
		}
		id, err := neLatLngToCell(lon, lat, neCellRes)
		if err != nil {
			apiError(w, "bad point", 400)
			return
		}
		res := neCollect(nil, neGridDisk(id, k), layers, rq)
		res.pending = nePendingKGs(KGCodesNearPointMem(lon, lat, 0.002))
		if neNotModified(w, r, res.etag(rq)) {
			return
		}
		if rq.format == "bin" {
			// bin: neCollect skipped res.kgs — never call columnar here.
			w.Header().Set("X-NE-Centre-Cell", neCellHex(id))
			neWriteCells(w, rq, res)
			return
		}
		doc := res.columnar(rq)
		doc["centre_cell"] = neCellHex(id)
		if len(res.pending) > 0 {
			w.Header().Set("X-NE-Pending-KGs", strings.Join(res.pending, ","))
		}
		neSetCache(w, rq, len(res.pending) > 0)
		writeJSON(w, 200, doc)
	default:
		apiError(w, "give ids=…, lon=&lat=[&k=] or bbox=w,s,e,n", 400)
	}
}

// neSplit — 413 with four sub-bboxes the client can fetch instead.
func neSplit(w http.ResponseWriter, bb [4]float64, why string) {
	mx, my := (bb[0]+bb[2])/2, (bb[1]+bb[3])/2
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 413, map[string]interface{}{
		"error": why,
		"split": [][4]float64{{bb[0], bb[1], mx, my}, {mx, bb[1], bb[2], my}, {bb[0], my, mx, bb[3]}, {mx, my, bb[2], bb[3]}},
	})
}

// ── /context include=ne ──────────────────────────────────────────────────────

// neContextBlock — the res-12 cell of a point for /api/v1/context.
func neContextBlock(lon, lat float64, public bool) (map[string]interface{}, string) {
	id, err := neLatLngToCell(lon, lat, neCellRes)
	if err != nil {
		return nil, "bad point"
	}
	rq := &neReq{public: public}
	doc, ok := neCellDoc(id, rq)
	if !ok {
		if p := nePendingKGs(KGCodesNearPointMem(lon, lat, 0.002)); len(p) > 0 {
			return nil, "ne cells not built yet for KG " + strings.Join(p, ",")
		}
		return nil, "no cell at this point"
	}
	return doc, ""
}

// ── manifest ?fields=digests ─────────────────────────────────────────────────

func handleNEManifestDigests(w http.ResponseWriter, r *http.Request) {
	neManifest.refresh(false)
	neManifest.mu.Lock()
	etag := strings.TrimSuffix(neManifest.etag, `"`) + `-d"`
	kgs := make(map[string]map[string]interface{}, len(neManifest.entries))
	for kg, e := range neManifest.entries {
		if e.LU == nil && e.Obs == nil && e.Bev == nil {
			continue
		}
		m := map[string]interface{}{}
		if e.LU != nil {
			m["lu"] = e.LU.Digest
		}
		if e.Bev != nil {
			m["bev"] = e.Bev.Digest
		}
		if e.Obs != nil {
			m["obs"] = e.Obs.Digest
		}
		kgs[kg] = m
	}
	neManifest.mu.Unlock()
	w.Header().Set("Cache-Control", "public, max-age=60")
	if neNotModified(w, r, etag) {
		return
	}
	writeJSON(w, 200, map[string]interface{}{"kgs": kgs, "built": len(kgs), "frozen": neFrozen, "obs_public": neObsPublic, "notice": NENotice()})
}
