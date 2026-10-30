// kg_universe.go — GET /api/v1/kgs: the KG universe as ONE list.
//
// Why this exists
// ───────────────
// Peers ({MAP_NAME}, {OBS_NAME}, …) need "which Katastralgemeinden
// exist" exactly once, as one download, with a stable fingerprint — not by
// looping states → districts → /search/kg pages (which is what llm.txt used
// to recommend, and which breaks on duplicate district names and page caps).
//
// The contract: kg_count = 7 850 Katastralgemeinden — the same universe
// {OBS_NAME} serves (its /api/v1/kgs?processed_only=0: kgs_total 7850).
// universe_hash = sha256(join(sorted(kg_code), "\n")) (hex) identifies the
// set of codes; it is the ETag of the unfiltered response. Any peer can
// recompute it from its own list and compare.
//
// Source of truth is a committed file, data/kg_universe.json.gz, so the list
// is served even when the index or srtm is unavailable. Build / refresh:
//
//	./umfeld-at -kg-universe-build          # from the kg table, verify vs the observed-layer peer, write file
//
// At startup the server loads the file (falls back to the kg table when the
// file is missing) and, in the background, compares the code set with
// {OBS_NAME} once; the outcome is reported as data.peer_check — a
// mismatch is logged loudly but never changes what we serve.
//
// Tier: PUBLIC. The rows are register keys (code, name, admin hierarchy,
// bbox/centroid) — the same fields srtm publishes — no parcel-level data.
// Payload: ~1.8 MB JSON; the handler keeps one pre-gzipped copy (level 9)
// of the unfiltered body and serves it directly (the gzip middleware skips
// bodies that are already encoded, like export_cache.go's .gz blobs).
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	kgUniversePath     = "data/kg_universe.json.gz"
	kgUniverseSchema   = "kg-universe-1"
	kgUniverseRecipe   = `sha256(join(sorted(kg_code), "\n")) hex — kg_code 5-digit zero-padded ("01004", never "1004")`
	kgUniverseContract = 7850 // kg_count contract — all Austrian Katastralgemeinden
)

type kgUniverseRow struct {
	KGCode       string     `json:"kg_code"`
	KGName       string     `json:"kg_name"`
	GemeindeCode string     `json:"gemeinde_code"`
	GemeindeName string     `json:"gemeinde_name"`
	DistrictCode string     `json:"district_code"`
	DistrictName string     `json:"district_name"`
	StateCode    string     `json:"state_code"`
	StateName    string     `json:"state_name"`
	BBox         [4]float64 `json:"bbox"`     // min_lon, min_lat, max_lon, max_lat
	Centroid     [2]float64 `json:"centroid"` // lon, lat (bbox centre)
}

// kgUniversePeerCheck — result of comparing our code set with a peer's.
type kgUniversePeerCheck struct {
	Service     string   `json:"service"`
	URL         string   `json:"url"`
	CheckedAt   string   `json:"checked_at"`
	Reachable   bool     `json:"reachable"`
	Error       string   `json:"error,omitempty"`
	PeerKGCount int      `json:"peer_kg_count,omitempty"`
	PeerHash    string   `json:"peer_universe_hash,omitempty"`
	Agree       bool     `json:"agree"`
	OnlyHere    []string `json:"only_here,omitempty"`
	OnlyPeer    []string `json:"only_peer,omitempty"`
}

type kgUniverse struct {
	Schema       string               `json:"schema"`
	KGCount      int                  `json:"kg_count"`
	UniverseHash string               `json:"universe_hash"`
	HashRecipe   string               `json:"universe_hash_recipe"`
	GeneratedAt  string               `json:"generated_at"`
	Source       string               `json:"source"`
	BBoxRevision int                  `json:"bbox_revision,omitempty"` // 2 = VGD envelope ∪ old (kg_universe_bbox.go); 0/1 = parcel-derived
	BBoxSource   string               `json:"bbox_source,omitempty"`
	PeerCheck    *kgUniversePeerCheck `json:"peer_check,omitempty"`
	KGs          []kgUniverseRow      `json:"kgs"`
}

var (
	kgUniverseMu    sync.RWMutex
	kgUniverseCur   *kgUniverse
	kgUniverseBody  []byte // compact JSON of the unfiltered response (data+meta)
	kgUniverseGzip  []byte // same, gzip -9
	kgUniverseIndex map[string]int
	kgUniverseLive  *kgUniversePeerCheck // live peer check, refreshed hourly (memory only)
)

// KGUniverseHash — sha256 over the sorted, newline-joined, CANONICAL codes.
//
// Every code is zero-padded to 5 digits and de-duplicated before hashing, so
// a caller holding the unpadded spelling ("1004" instead of "01004" — 740 of
// the 7 850 codes start with a zero) still computes the same universe_hash as
// a peer holding the padded one. The hash is a statement about the SET of
// Katastralgemeinden, not about how a particular client spelled them.
func KGUniverseHash(codes []string) string {
	return hex.EncodeToString(kgUniverseHashSum(canonicalKGCodeSet(codes)))
}

func kgUniverseHashSum(sorted []string) []byte {
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return sum[:]
}

// canonicalKGCodeSet pads, trims, de-duplicates and sorts a code list.
// Empty entries are dropped.
func canonicalKGCodeSet(codes []string) []string {
	seen := make(map[string]struct{}, len(codes))
	out := make([]string, 0, len(codes))
	for _, c := range codes {
		c = padKGCode(c)
		if c == "" {
			continue
		}
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// BuildKGUniverseFromIndex reads the kg table into a universe document.
func BuildKGUniverseFromIndex() (*kgUniverse, error) {
	if globalSearchIndex == nil {
		return nil, fmt.Errorf("search index not initialized")
	}
	rows, err := globalSearchIndex.DB().Query(`SELECT kg_code, COALESCE(kg_name,''), COALESCE(gemeinde_code,''), COALESCE(gemeinde_name,''),
		COALESCE(district_code,''), COALESCE(district_name,''), COALESCE(state_code,''), COALESCE(state_name,''),
		COALESCE(min_lon,0), COALESCE(min_lat,0), COALESCE(max_lon,0), COALESCE(max_lat,0) FROM kg ORDER BY kg_code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	u := &kgUniverse{Schema: kgUniverseSchema, HashRecipe: kgUniverseRecipe, Source: "search_index.db kg table (BEV Kataster tiles, CC BY 4.0)",
		GeneratedAt: time.Now().UTC().Format(time.RFC3339)}
	for rows.Next() {
		var r kgUniverseRow
		if err := rows.Scan(&r.KGCode, &r.KGName, &r.GemeindeCode, &r.GemeindeName, &r.DistrictCode, &r.DistrictName,
			&r.StateCode, &r.StateName, &r.BBox[0], &r.BBox[1], &r.BBox[2], &r.BBox[3]); err != nil {
			return nil, err
		}
		r.Centroid = [2]float64{(r.BBox[0] + r.BBox[2]) / 2, (r.BBox[1] + r.BBox[3]) / 2}
		u.KGs = append(u.KGs, r)
	}
	u.finish()
	return u, nil
}

func (u *kgUniverse) codes() []string {
	c := make([]string, len(u.KGs))
	for i, r := range u.KGs {
		c[i] = r.KGCode
	}
	return c
}

// finish canonicalises the rows (5-digit codes, no duplicates, sorted) and
// derives kg_count + universe_hash. Every constructor funnels through here, so
// an unpadded code in the kg table, in an old data/kg_universe.json.gz or in a
// hand-edited file can never leak into the served list or shift the hash.
func (u *kgUniverse) finish() {
	rows := make([]kgUniverseRow, 0, len(u.KGs))
	seen := make(map[string]int, len(u.KGs))
	for _, r := range u.KGs {
		r.KGCode = padKGCode(r.KGCode)
		if r.KGCode == "" {
			continue
		}
		if i, dup := seen[r.KGCode]; dup {
			// Same KG spelled twice (e.g. "1004" and "01004"): keep the richer row.
			if rows[i].KGName == "" && r.KGName != "" {
				rows[i] = r
			}
			continue
		}
		seen[r.KGCode] = len(rows)
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].KGCode < rows[j].KGCode })
	u.KGs = rows
	u.KGCount = len(u.KGs)
	u.UniverseHash = KGUniverseHash(u.codes())
	if u.Schema == "" {
		u.Schema = kgUniverseSchema
	}
	u.HashRecipe = kgUniverseRecipe
}

// LoadKGUniverseFile reads data/kg_universe.json.gz.
func LoadKGUniverseFile(path string) (*kgUniverse, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var u kgUniverse
	if err := json.NewDecoder(zr).Decode(&u); err != nil {
		return nil, err
	}
	if len(u.KGs) == 0 {
		return nil, fmt.Errorf("%s: empty kgs[]", path)
	}
	want := KGUniverseHash(u.codes())
	if u.UniverseHash != "" && u.UniverseHash != want {
		return nil, fmt.Errorf("%s: universe_hash %s does not match codes (%s)", path, u.UniverseHash, want)
	}
	u.finish()
	return &u, nil
}

// WriteKGUniverseFile writes the document atomically (gzip -9).
func WriteKGUniverseFile(path string, u *kgUniverse) error {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	enc := json.NewEncoder(zw)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(u); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// CheckKGUniversePeer fetches the peer's full KG list and compares code sets.
func CheckKGUniversePeer(ctx context.Context, u *kgUniverse) *kgUniversePeerCheck {
	pc := &kgUniversePeerCheck{Service: peers().ObsName, URL: (peers().ObsAPI + "/api/v1/kgs?processed_only=0&limit=5000&offset=") + "0", CheckedAt: time.Now().UTC().Format(time.RFC3339)}
	client := &http.Client{Timeout: 60 * time.Second}
	peer := map[string]bool{}
	ne := &peerNEState{CheckedAt: time.Now().UTC(), Ready: map[string]bool{}}
	defer func() { ne.Reachable = pc.Reachable; setPeerNEState(ne) }()
	for offset := 0; offset < 50000; offset += 5000 {
		req, _ := http.NewRequestWithContext(ctx, "GET", (peers().ObsAPI+"/api/v1/kgs?processed_only=0&limit=5000&offset=")+strconv.Itoa(offset), nil)
		req.Header.Set("User-Agent", "umfeld-at/kg-universe-check")
		resp, err := client.Do(req)
		if err != nil {
			pc.Error = err.Error()
			return pc
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if err != nil || resp.StatusCode != 200 {
			pc.Error = fmt.Sprintf("HTTP %d: %.120s", resp.StatusCode, string(body))
			return pc
		}
		var doc struct {
			KGs []struct {
				KGCode  string `json:"kg_code"`
				NEReady bool   `json:"ne_ready"` // authoritative "its /api/v1/cells serves this KG" (peer_ne_ready.go)
			} `json:"kgs"`
			Total       int    `json:"total"`
			Count       int    `json:"count"`
			NEServed    int    `json:"kgs_ne_served"`
			CellsGen    string `json:"cells_generation"`
			ManifestGen string `json:"manifest_generation"`
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			pc.Error = "decode: " + err.Error()
			return pc
		}
		ne.ServedReported, ne.CellsGen, ne.ManifestGen = doc.NEServed, doc.CellsGen, doc.ManifestGen
		for _, r := range doc.KGs {
			c := padKGCode(r.KGCode)
			peer[c] = true
			if r.NEReady && len(ne.Ready) < 2*kgUniverseContract { // bound: a peer cannot grow this past the universe
				ne.Ready[c] = true
			}
		}
		if doc.Count == 0 || len(peer) >= doc.Total {
			break
		}
	}
	pc.Reachable = true
	pc.PeerKGCount = len(peer)
	pcodes := make([]string, 0, len(peer))
	for c := range peer {
		pcodes = append(pcodes, c)
	}
	pc.PeerHash = KGUniverseHash(pcodes)
	here := map[string]bool{}
	for _, c := range u.codes() {
		here[c] = true
		if !peer[c] {
			pc.OnlyHere = append(pc.OnlyHere, c)
		}
	}
	for c := range peer {
		if !here[c] {
			pc.OnlyPeer = append(pc.OnlyPeer, c)
		}
	}
	sort.Strings(pc.OnlyHere)
	sort.Strings(pc.OnlyPeer)
	pc.Agree = pc.PeerHash == u.UniverseHash
	return pc
}

// padKGCode — full 5-digit zero padding for a COMPLETE KG code ("1004" → "01004",
// also "4" → "00004"). Unlike NormalizeKGCode (request parameters, where 1–3 digit
// strings are typeahead prefixes) every code in a universe document, a peer list
// or a ?codes= filter is a whole code, so any short numeric run is padded.
// Non-numeric input is only trimmed.
func padKGCode(c string) string {
	c = strings.TrimSpace(c)
	for len(c) < 5 && c != "" && isNumericCode(c) {
		c = "0" + c
	}
	return c
}

// InitKGUniverse loads the committed file (preferred) or builds from the index,
// prepares the cached bodies, and kicks off one background peer check.
func InitKGUniverse() {
	u, err := LoadKGUniverseFile(kgUniversePath)
	if err != nil {
		log.Printf("⚠️  kg_universe: %v — building from the kg table", err)
		u, err = BuildKGUniverseFromIndex()
		if err != nil {
			log.Printf("⚠️  kg_universe: unavailable (%v); /api/v1/kgs answers 503", err)
			return
		}
	}
	setKGUniverse(u, nil)
	InitKGBBoxDiff()
	if u.KGCount != kgUniverseContract {
		log.Printf("⚠️  kg_universe: kg_count=%d ≠ contract %d", u.KGCount, kgUniverseContract)
	}
	log.Printf("🗺️  kg_universe: %d KGs, hash %.16s… (%s)", u.KGCount, u.UniverseHash, kgUniversePath)
	go kgUniversePeerLoop(u)
}

// kgUniversePeerRecheck — how often the srtm agreement is re-verified. Since
// 2026-10-04 both peers ({OBS_NAME}, {MAP_NAME}) key their per-KG
// state on this list and compare universe_hash, so `peer_check.agree` must be
// live, not a startup snapshot. The same fetch harvests srtm's per-KG
// `ne_ready` for the landing-page "observed layer live" counter
// (peer_ne_ready.go); pending KGs self-promote there as its ingest drains, so
// 20 min keeps that counter honest at ~6 MB/h.
const kgUniversePeerRecheck = 20 * time.Minute

func kgUniversePeerLoop(u *kgUniverse) {
	time.Sleep(45 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		pc := CheckKGUniversePeer(ctx, u)
		cancel()
		kgUniverseMu.RLock()
		prev := kgUniverseLive
		kgUniverseMu.RUnlock()
		// Re-encode the precomputed body with the live peer_check so the
		// unfiltered path stays a byte copy (no per-request encode) and the
		// ETag stays the universe hash — hourly rechecks must not break 304s.
		setKGUniverse(u, pc)
		switch {
		case !pc.Reachable:
			log.Printf("kg_universe: peer %s unreachable (%s) — serving the local file", pc.Service, pc.Error)
		case pc.Agree && (prev == nil || !prev.Agree):
			log.Printf("kg_universe: peer %s agrees (%d KGs, hash %.16s…)", pc.Service, pc.PeerKGCount, pc.PeerHash)
		case !pc.Agree:
			log.Printf("🚨 kg_universe: peer %s DISAGREES: peer %d KGs, only_here=%d only_peer=%d — run -kg-universe-build and reconcile",
				pc.Service, pc.PeerKGCount, len(pc.OnlyHere), len(pc.OnlyPeer))
		}
		time.Sleep(kgUniversePeerRecheck)
	}
}

func setKGUniverse(u *kgUniverse, live *kgUniversePeerCheck) {
	idx := make(map[string]int, len(u.KGs))
	for i, r := range u.KGs {
		idx[r.KGCode] = i
	}
	body := kgUniverseEncode(u, u.KGs, live, false)
	var gz bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	zw.Write(body)
	zw.Close()
	kgUniverseMu.Lock()
	kgUniverseCur, kgUniverseIndex, kgUniverseBody, kgUniverseGzip, kgUniverseLive = u, idx, body, gz.Bytes(), live
	kgUniverseMu.Unlock()
}

// kgUniverseEncode renders {data:{…}, meta:{…}} compactly.
func kgUniverseEncode(u *kgUniverse, rows []kgUniverseRow, peer *kgUniversePeerCheck, codesOnly bool) []byte {
	if peer == nil {
		peer = u.PeerCheck
	}
	data := map[string]interface{}{
		"kg_count":             u.KGCount,
		"kg_count_contract":    kgUniverseContract,
		"universe_hash":        u.UniverseHash,
		"universe_hash_recipe": u.HashRecipe,
		"generated_at":         u.GeneratedAt,
		"source":               u.Source,
		"bbox_revision":        u.BBoxRevision,
		"bbox_source":          u.BBoxSource,
		"peer_check":           peer,
		"count":                len(rows),
	}
	if codesOnly {
		codes := make([]string, len(rows))
		for i, r := range rows {
			codes[i] = r.KGCode
		}
		data["codes"] = codes
	} else {
		if rows == nil {
			rows = []kgUniverseRow{}
		}
		data["kgs"] = rows
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(map[string]interface{}{
		"data": data,
		"meta": map[string]interface{}{
			"total":   len(rows),
			"license": AttributionMetaFor("/api/v1/kgs", false),
		},
	})
	return buf.Bytes()
}

// KGUniverseStats — size line for /api/v1/debug/inflight.
func KGUniverseStats() map[string]interface{} {
	kgUniverseMu.RLock()
	defer kgUniverseMu.RUnlock()
	if kgUniverseCur == nil {
		return map[string]interface{}{"loaded": false}
	}
	out := map[string]interface{}{"loaded": true, "kg_count": kgUniverseCur.KGCount, "universe_hash": kgUniverseCur.UniverseHash,
		"body_bytes": len(kgUniverseBody), "gzip_bytes": len(kgUniverseGzip)}
	if kgUniverseLive != nil {
		out["peer_agree"] = kgUniverseLive.Agree
		out["peer_reachable"] = kgUniverseLive.Reachable
	}
	if st := PeerNEState(); st != nil {
		out["peer_ne_ready"] = map[string]interface{}{"kgs": len(st.Ready), "served_reported": st.ServedReported,
			"cells_generation": st.CellsGen, "checked_at": st.CheckedAt.Format(time.RFC3339), "reachable": st.Reachable}
	}
	return out
}

// handleV1KGs — GET /api/v1/kgs
//
//	(no params)                 the whole universe, ETag "<universe_hash>", pre-gzipped
//	?fields=codes               {codes:[…]} only (≈ 60 KB)
//	?codes=63349,01004          subset by code (unknown codes are omitted; count tells)
//	?district_code=616          subset by Bezirk code (3 digits)
//	?gemeinde_code=61630        subset by Gemeinde code
//	?state=6 | ?state=Steiermark
//	?district=Voitsberg         exact Bezirk name (case-insensitive)
func handleV1KGs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		apiError(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	kgUniverseMu.RLock()
	u, idx, body, gz, live := kgUniverseCur, kgUniverseIndex, kgUniverseBody, kgUniverseGzip, kgUniverseLive
	kgUniverseMu.RUnlock()
	if u == nil {
		w.Header().Set("Retry-After", "30")
		apiError(w, "kg universe not loaded yet", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	codesOnly := q.Get("fields") == "codes"
	filtered := codesOnly || q.Get("codes") != "" || q.Get("district_code") != "" || q.Get("gemeinde_code") != "" ||
		q.Get("state") != "" || q.Get("district") != "" || q.Get("pretty") != ""

	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-KG-Universe-Hash", u.UniverseHash)
	h.Set("X-KG-Count", strconv.Itoa(u.KGCount))
	h.Set("Cache-Control", "public, max-age=3600, stale-while-revalidate=86400")
	h.Set("Access-Control-Allow-Origin", "*")

	// ETag = universe hash, i.e. the SET of codes — peer_check.checked_at
	// moving every hour is not a new representation. A peer DISAGREEMENT is
	// (clients must notice), so it flips the tag.
	// A bbox revision (kg_universe_bbox.go) is a new representation too —
	// the codes and universe_hash stay, "-b<rev>" makes cached copies refetch.
	etag := `"` + u.UniverseHash + bboxRevTag(u) + livePeerTag(live) + `"`
	if filtered {
		qs := sha256.Sum256([]byte(r.URL.RawQuery))
		etag = `"` + u.UniverseHash[:32] + "-" + hex.EncodeToString(qs[:6]) + bboxRevTag(u) + livePeerTag(live) + `"`
	}
	if u.BBoxRevision > 0 {
		h.Set("X-KG-BBox-Revision", strconv.Itoa(u.BBoxRevision))
	}
	if live != nil {
		h.Set("X-KG-Universe-Peer-Agree", strconv.FormatBool(live.Agree))
	}
	h.Set("ETag", etag)
	if m := r.Header.Get("If-None-Match"); m != "" && etagMatches(m, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	if !filtered {
		// Unfiltered: serve the precomputed body; gzip copy when accepted.
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			h.Set("Content-Encoding", "gzip")
			h.Add("Vary", "Accept-Encoding")
			h.Set("Content-Length", strconv.Itoa(len(gz)))
			w.WriteHeader(http.StatusOK)
			if r.Method != http.MethodHead {
				_, _ = w.Write(gz)
			}
			return
		}
		h.Add("Vary", "Accept-Encoding")
		h.Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
		return
	}

	rows := u.KGs
	if v := q.Get("codes"); v != "" {
		// Middleware already padded 4-digit codes (kgCodeParams); padKGCode
		// also covers whitespace and shorter numeric spellings.
		rows = nil
		seen := map[string]bool{}
		for _, c := range ParseKGList(v) {
			c = padKGCode(c)
			if i, ok := idx[c]; ok && !seen[c] {
				seen[c] = true
				rows = append(rows, u.KGs[i])
			}
		}
	}
	match := func(keep func(kgUniverseRow) bool) {
		out := rows[:0:0]
		for _, row := range rows {
			if keep(row) {
				out = append(out, row)
			}
		}
		rows = out
	}
	if v := q.Get("district_code"); v != "" {
		match(func(r kgUniverseRow) bool { return r.DistrictCode == v })
	}
	if v := q.Get("district"); v != "" {
		if isNumericCode(v) {
			match(func(r kgUniverseRow) bool { return r.DistrictCode == v })
		} else {
			match(func(r kgUniverseRow) bool { return strings.EqualFold(r.DistrictName, v) })
		}
	}
	if v := q.Get("gemeinde_code"); v != "" {
		match(func(r kgUniverseRow) bool { return r.GemeindeCode == v })
	}
	if v := q.Get("state"); v != "" {
		match(func(r kgUniverseRow) bool { return r.StateCode == v || strings.EqualFold(r.StateName, v) })
	}
	peer := u.PeerCheck
	if live != nil {
		peer = live
	}
	out := kgUniverseEncode(u, rows, peer, codesOnly)
	if q.Get("pretty") != "" {
		var pretty bytes.Buffer
		if json.Indent(&pretty, out, "", "  ") == nil {
			out = pretty.Bytes()
		}
	}
	h.Set("Content-Length", strconv.Itoa(len(out)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(out)
	}
}

// bboxRevTag — ETag suffix "-b2" from bbox revision 2 on (revision 1 = the
// original parcel-derived envelopes, no suffix, so old tags stay valid).
func bboxRevTag(u *kgUniverse) string {
	if u.BBoxRevision <= 1 {
		return ""
	}
	return "-b" + strconv.Itoa(u.BBoxRevision)
}

// livePeerTag — ETag suffix: empty while the peer agrees or is unknown,
// "-peer-disagree" when the live check found a different universe.
func livePeerTag(pc *kgUniversePeerCheck) string {
	if pc == nil || !pc.Reachable || pc.Agree {
		return ""
	}
	return "-peer-disagree"
}

// RunKGUniverseBuild — CLI pass: build from the index, verify against the
// peer, write data/kg_universe.json.gz, print a summary. Exit code 1 on
// disagreement so a cron/CI step notices.
func RunKGUniverseBuild() int {
	u, err := BuildKGUniverseFromIndex()
	if err != nil {
		fmt.Fprintln(os.Stderr, "kg_universe build:", err)
		return 2
	}
	prevFile, prevErr := LoadKGUniverseFile(kgUniversePath)
	// bbox revision 2: widen to the VGD KG polygon envelope (kg_universe_bbox.go).
	vgd, vgdSrc, err := LoadKGVGDBBoxes(kgVGDBBoxPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kg_universe build: VGD bboxes:", err)
		return 2
	}
	u.BBoxSource = "union(parcel-derived envelope, " + vgdSrc + " envelope + " + strconv.FormatFloat(kgVGDBBoxPad, 'g', -1, 64) + "°)"
	widened, missing := ApplyKGVGDBBoxes(u, vgd, kgVGDBBoxPad)
	fmt.Printf("bbox revision %d: %d rows widened to the VGD envelope, %d without VGD row %v\n", u.BBoxRevision, widened, len(missing), missing)
	if prevErr == nil {
		for i, r := range u.KGs { // never shrink what peers already planned on
			if j, ok := prevIndex(prevFile, r.KGCode); ok {
				u.KGs[i].BBox = bboxUnion(prevFile.KGs[j].BBox, r.BBox)
				u.KGs[i].Centroid = [2]float64{round7((u.KGs[i].BBox[0] + u.KGs[i].BBox[2]) / 2), round7((u.KGs[i].BBox[1] + u.KGs[i].BBox[3]) / 2)}
			}
		}
	}
	if n, err := updateKGTableBBoxes(u); err != nil {
		fmt.Fprintln(os.Stderr, "kg_universe build: kg table bbox update:", err)
		return 2
	} else {
		fmt.Printf("kg table: %d bbox rows updated\n", n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	u.PeerCheck = CheckKGUniversePeer(ctx, u)
	if prev, err := LoadKGUniverseFile(kgUniversePath); err == nil && prev.UniverseHash == u.UniverseHash && !u.PeerCheck.Reachable && prev.PeerCheck != nil {
		u.PeerCheck = prev.PeerCheck // keep the last successful agreement when the peer is down
		fmt.Println("peer unreachable — keeping previous peer_check from file")
	}
	if err := WriteKGUniverseFile(kgUniversePath, u); err != nil {
		fmt.Fprintln(os.Stderr, "kg_universe write:", err)
		return 2
	}
	fmt.Printf("kg_universe: %d KGs (contract %d), hash %s → %s\n", u.KGCount, kgUniverseContract, u.UniverseHash, kgUniversePath)
	if prevErr == nil {
		// Diff vs the file that was committed before this pass; when the previous
		// file already carried a diff of the same revision, keep its older baseline
		// so re-running the build does not erase the sliver list peers rely on.
		base := prevFile
		if old, err := LoadKGBBoxDiffFile(kgUniverseBBoxDiffPath); err == nil && old.BBoxRevision == u.BBoxRevision && prevFile.BBoxRevision == u.BBoxRevision {
			base = kgUniverseFromDiffBaseline(prevFile, old)
		}
		d := BuildKGBBoxDiff(base, u)
		if err := WriteKGBBoxDiffFile(kgUniverseBBoxDiffPath, d); err != nil {
			fmt.Fprintln(os.Stderr, "kg_universe bbox diff write:", err)
			return 2
		}
		fmt.Printf("bbox diff: %d of %d KGs changed vs %s → %s\n", d.ChangedCount, d.KGCount, d.FromGeneratedAt, kgUniverseBBoxDiffPath)
	}
	pc := u.PeerCheck
	fmt.Printf("peer %s: reachable=%v agree=%v peer_kgs=%d only_here=%d only_peer=%d %s\n",
		pc.Service, pc.Reachable, pc.Agree, pc.PeerKGCount, len(pc.OnlyHere), len(pc.OnlyPeer), pc.Error)
	if u.KGCount != kgUniverseContract || (pc.Reachable && !pc.Agree) {
		return 1
	}
	return 0
}

func prevIndex(u *kgUniverse, code string) (int, bool) {
	i := sort.Search(len(u.KGs), func(i int) bool { return u.KGs[i].KGCode >= code })
	if i < len(u.KGs) && u.KGs[i].KGCode == code {
		return i, true
	}
	return 0, false
}

// kgUniverseFromDiffBaseline — the universe as it was BEFORE an existing
// diff: current rows, but bbox_old from the diff for every KG it lists.
func kgUniverseFromDiffBaseline(cur *kgUniverse, d *kgBBoxDiff) *kgUniverse {
	b := *cur
	b.KGs = append([]kgUniverseRow(nil), cur.KGs...)
	b.GeneratedAt = d.FromGeneratedAt
	for _, c := range d.Changed {
		if i, ok := prevIndex(&b, c.KGCode); ok {
			b.KGs[i].BBox = c.BBoxOld
		}
	}
	return &b
}

// KGUniverseCodes — the 7 850 KG codes of the peer contract (sorted, padded),
// nil before InitKGUniverse. The NE manifest keys its "built/total" on this.
func KGUniverseCodes() []string {
	kgUniverseMu.RLock()
	u := kgUniverseCur
	kgUniverseMu.RUnlock()
	if u == nil {
		return nil
	}
	return u.codes()
}
