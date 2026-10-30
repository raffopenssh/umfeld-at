package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ═══════════════════════════════════════════════════════════════════════
// /api/v1/ne/{kg} — Nutzungseinheit cells (declared + observed layers)
// ═══════════════════════════════════════════════════════════════════════
//
// Contract: docs/ne-cells.md. The server computes NOTHING here — the cells
// are produced by the pinned reference package `ne_cells/` (python, shapely
// 2.1.2 + h3 4.5.0) so that a peer recomputing them from a bevdirect fetch
// gets bit-identical records. Go only validates the container, stores it,
// and serves it (binary as-is, or decoded JSON for debugging).
//
//   GET  <private>/api/v1/ne                      index: built KGs
//   GET  <private>/api/v1/ne/{kg}[?layers=lu,obs][&format=json]   default binary, all stored layers
//   POST <private>/api/v1/ne/{kg}/obs             body = one NEO1 section (peer's observed layer),
//                                          optionally preceded by the peer's NEC1 (bevdirect declared build → `bev`)
//
// Gate: private tier AND first configured token ({OBS_NAME}) only, like
// the draft share — the product is not released to anyone else yet.
// Files: data/ne_cells/{kg}.nec (declared, written by the python CLI) and
// data/ne_cells/{kg}.obs (observed, written by POST). Not committed.

var neDir = "data/ne_cells"

func neDirOverride(d string) string { old := neDir; neDir = d; return old }

var (
	neKGRe     = regexp.MustCompile(`^[0-9]{5}$`)
	neWriteMu  sync.Mutex
	neMaxObsMB = 32 // 2026-10-27: KG 80110 (Sölden) NEO1 alone is 17.5 MB, NEC1+NEO1 21.5 MB; raised from 16
)

type neSection struct {
	Magic  string
	Header map[string]interface{}
	Body   []byte // deflated
	Raw    []byte // whole section bytes (magic..body)
}

// parseNESections walks the self-delimiting container (pack.py) and checks
// each digest against the inflated record block.
func parseNESections(buf []byte) ([]neSection, error) {
	var out []neSection
	off := 0
	for off < len(buf) {
		if len(buf)-off < 9 {
			return nil, fmt.Errorf("truncated section header at %d", off)
		}
		start := off
		magic := string(buf[off : off+4])
		if buf[off+4] != 1 {
			return nil, fmt.Errorf("unsupported fmt_ver %d", buf[off+4])
		}
		hl := int(binary.LittleEndian.Uint32(buf[off+5:]))
		off += 9
		if off+hl > len(buf) {
			return nil, fmt.Errorf("truncated header")
		}
		var hdr map[string]interface{}
		if err := json.Unmarshal(buf[off:off+hl], &hdr); err != nil {
			return nil, fmt.Errorf("header json: %w", err)
		}
		off += hl
		if off+4 > len(buf) {
			return nil, fmt.Errorf("truncated body length")
		}
		bl := int(binary.LittleEndian.Uint32(buf[off:]))
		off += 4
		if off+bl > len(buf) {
			return nil, fmt.Errorf("truncated body")
		}
		body := buf[off : off+bl]
		off += bl
		zr, err := zlib.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("body deflate: %w", err)
		}
		raw, err := io.ReadAll(io.LimitReader(zr, 256<<20))
		if err != nil {
			return nil, fmt.Errorf("body inflate: %w", err)
		}
		sum := sha256.Sum256(raw)
		if d, _ := hdr["digest"].(string); d != hex.EncodeToString(sum[:])[:16] {
			return nil, fmt.Errorf("digest mismatch in %s section", magic)
		}
		out = append(out, neSection{Magic: magic, Header: hdr, Body: body, Raw: buf[start:off]})
	}
	return out, nil
}

func neFilePath(kg, kind string) string { return filepath.Join(neDir, kg+"."+kind) }

func neBuiltKGs() []string {
	set := map[string]bool{}
	for _, kg := range neStore.kgCodes() {
		set[kg] = true
	}
	ents, _ := os.ReadDir(neDir)
	for _, e := range ents {
		if n := e.Name(); strings.HasSuffix(n, ".nec") && !strings.HasSuffix(n, ".bev.nec") {
			set[strings.TrimSuffix(n, ".nec")] = true
		}
	}
	kgs := make([]string, 0, len(set))
	for kg := range set {
		kgs = append(kgs, kg)
	}
	sort.Strings(kgs)
	return kgs
}

// handleNE routes /api/v1/ne/… (docs/ne-public-api.md §2, §7).
//
//	reads  — any tier (neReadGate: per-IP rate bucket); api_ne_public.go
//	writes — POST|PUT {kg}/obs, POST|GET {kg}/report, draft/* — private tier,
//	         {OBS_NAME} token only (neWriteGate / isNEWriter)
func handleNE(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/ne"), "/")
	parts := strings.Split(rest, "/")
	// ── writes / private-only ──
	if rest == "reports" {
		handleNEReportsOverview(w, r)
		return
	}
	if rest == "revisit" {
		handleNERevisit(w, r) // api_ne_revisit.go: peer revisit hints, contributor/private only
		return
	}
	if len(parts) == 2 && neKGRe.MatchString(parts[0]) && parts[1] == "report" {
		// Observer reports: any private token (api_ne_reports.go).
		tokIdx, ok := neReportGate(w, r)
		if !ok {
			return
		}
		if r.Method == http.MethodPost {
			handleNEReportPost(w, r, parts[0], tokIdx)
		} else {
			handleNEReportList(w, parts[0])
		}
		return
	}
	if len(parts) == 2 && neKGRe.MatchString(parts[0]) && parts[1] == "chunks" {
		// NE change step 2: stripped rows of wanted chunks (api_ne_change.go).
		tokIdx, ok := neReportGate(w, r)
		if !ok {
			return
		}
		if r.Method == http.MethodPost {
			handleNEChunksPost(w, r, parts[0], tokIdx)
		} else {
			handleNEChunksList(w, r, parts[0])
		}
		return
	}
	if len(parts) == 2 && neKGRe.MatchString(parts[0]) && parts[1] == "declared" {
		// the declared view cell set peers build bare NEO1 on (ne_change_declared.go)
		handleNEDeclaredGet(w, r, parts[0])
		return
	}
	if len(parts) == 2 && neKGRe.MatchString(parts[0]) && parts[1] == "obs" {
		if !neWriteGate(w, r) {
			return
		}
		kg := parts[0]
		{
			if r.Method == http.MethodPost || r.Method == http.MethodPut {
				handleNEObsPost(w, r, kg)
				return
			}
			rq, msg := neParseReq(r, "zstd")
			if msg != "" {
				apiError(w, msg, 400)
				return
			}
			rq.layers = []string{"obs"}
			handleNEWhole(w, r, rq, kg)
			return
		}
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && !(r.Method == http.MethodPost && rest == "cells") {
		apiError(w, "method not allowed", 405)
		return
	}
	// ── reads ──
	if !neReadGate(w, r) {
		return
	}
	defFormat := "bin"
	if rest == "" || strings.HasPrefix(rest, "chunk/") || neKGRe.MatchString(parts[0]) {
		defFormat = "zstd"
	}
	// the filter endpoint has its own format vocabulary (count|cells|h3|geojson)
	if rest == "mvt.json" || strings.HasPrefix(rest, "mvt/") { // cell tiles (ne_mvt.go)
		handleNEMVT(w, r, rest)
		return
	}
	if rest == "pmtiles" || strings.HasPrefix(rest, "pmtiles/") { // archives (ne_mvt_jobs.go)
		handleNEPMTiles(w, r, rest)
		return
	}
	switch rest {
	case "filter":
		handleNEFilter(w, r)
		return
	case "filter/fields":
		handleNEFilterFields(w, r)
		return
	case "stats/unit":
		if id, ok := exportGate(w, r); ok {
			handleNEStatsUnit(w, r, id)
		}
		return
	}
	rq, msg := neParseReq(r, defFormat)
	if msg != "" {
		apiError(w, msg, 400)
		return
	}
	switch {
	case rest == "":
		handleNERoot(w, r, rq)
	case rest == "manifest":
		if r.URL.Query().Get("fields") == "digests" {
			handleNEManifestDigests(w, r)
		} else {
			handleNEManifest(w, r)
		}
	case rest == "cells":
		handleNECells(w, r, rq)
	case rest == "stats":
		handleNEChangeStats(w, r, rq)
	case rest == "glyph":
		handleNEGlyph(w, r)
	case rest == "glyph/kgs":
		handleNEGlyphKGs(w, r)
	case rest == "glyph/tile":
		handleNEGlyphTile(w, r)
	case rest == "glyph/cell":
		handleNEGlyphCell(w, r)
	case rest == "glyph/kg":
		handleNEGlyphKG(w, r)
	case rest == "glyph/admin":
		handleNEGlyphAdmin(w, r)
	case rest == "glyph/admin/outline":
		handleNEGlyphAdminOutline(w, r)
	case rest == "glyph/sub":
		handleNEGlyphSub(w, r)
	case rest == "glyph/fidx":
		handleNEGlyphFidx(w, r)
	case rest == "glyph/filter.js":
		handleNEGlyphFilterJS(w, r)
	case rest == "glyph/passes":
		handleNEGlyphPasses(w, r)
	case rest == "glyph/soak.js":
		handleNEGlyphSoakJS(w, r)
	case len(parts) == 2 && parts[0] == "dict":
		handleNEDict(w, r, parts[1])
	case len(parts) == 2 && parts[0] == "chunk":
		handleNEChunk(w, r, rq, parts[1])
	case len(parts) == 2 && parts[0] == "cell":
		handleNECell(w, r, rq, parts[1])
	case len(parts) == 1 && neKGRe.MatchString(parts[0]):
		handleNEWhole(w, r, rq, parts[0])
	case len(parts) == 2 && neKGRe.MatchString(parts[0]) && parts[1] == "head":
		handleNEHead(w, r, rq, parts[0])
	default:
		apiError(w, "not found", 404)
	}
}

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// decodeNEC1 mirrors pack.py LU_REC/K_REC. Field semantics: docs/ne-cells.md.
func decodeNEC1(s neSection) (map[string]interface{}, map[string]interface{}, error) {
	zr, err := zlib.NewReader(bytes.NewReader(s.Body))
	if err != nil {
		return nil, nil, err
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		return nil, nil, err
	}
	n, _ := s.Header["cells_n"].(float64)
	kn, _ := s.Header["kcells_n"].(float64)
	if int(n)*30+int(kn)*29 != len(raw) {
		return nil, nil, fmt.Errorf("record block size %d ≠ %d×30+%d×29", len(raw), int(n), int(kn))
	}
	cells := make(map[string]interface{}, int(n))
	off := 0
	for i := 0; i < int(n); i++ {
		b := raw[off : off+30]
		off += 30
		lu := make([]int, 9)
		for j := range lu {
			lu[j] = int(b[12+j])
		}
		c := map[string]interface{}{
			"kg_share": b[8], "land": b[9], "cov": b[10], "flags": b[11], "lu": lu,
			"bld_cover": b[21], "bld_n": binary.LittleEndian.Uint16(b[22:]), "gk": b[24], "n_parc": b[25],
			"line_m_ha": binary.LittleEndian.Uint16(b[26:]),
		}
		if b[28] != 0xFF {
			c["orient_coh"], c["orient_axis_deg"] = b[28], int(b[29])*2
		}
		cells[fmt.Sprintf("%x", binary.LittleEndian.Uint64(b))] = c
	}
	kcells := make(map[string]interface{}, int(kn))
	for i := 0; i < int(kn); i++ {
		b := raw[off : off+29]
		off += 29
		c := map[string]interface{}{
			"n_parc": binary.LittleEndian.Uint16(b[8:]), "parc_p10": binary.LittleEndian.Uint32(b[10:]),
			"parc_p50": binary.LittleEndian.Uint32(b[14:]), "parc_p90": binary.LittleEndian.Uint32(b[18:]),
			"ez_n": binary.LittleEndian.Uint16(b[22:]), "bld_n": binary.LittleEndian.Uint16(b[25:]),
		}
		if b[24] != 0xFF {
			c["parc_per_ez"] = float64(b[24]) / 10
		}
		if v := binary.LittleEndian.Uint16(b[27:]); v != 0xFFFF {
			c["bld_p50"] = v
		}
		kcells[fmt.Sprintf("%x", binary.LittleEndian.Uint64(b))] = c
	}
	return cells, kcells, nil
}

// handleNEObsPost stores the peer's observed layer for a KG after validating
// the container, the digest, and the join contract (same kg, res 12, same
// 9-group order). The records themselves are the peer's format and are
// passed through untouched.
//
// Two body shapes (docs/ne-cells.md §observers):
//   - bare NEO1            → cell set must equal our lu (NEC1 index epoch); 409 otherwise.
//   - NEC1 + NEO1          → the NEC1 is the peer's own declared build (algo = frozen
//     algo, source ∈ frozen.bev_equivalent_tags); the NEO1 cell set is validated
//     against THAT NEC1, which is stored as the KG's `bev` section. lu stays
//     frozen; if lu exists the answer carries a border-cell diff (never a 409).
//
// Either way the stored NEO1 header gets declared_source/declared_digest of the
// layer it was validated against (the digest is over the record block, so the
// header rewrite is digest-neutral).
func handleNEObsPost(w http.ResponseWriter, r *http.Request, kg string) {
	// Same per-token request rate and daily byte budget as reports: each
	// accepted body is written to disk and folded into {kg}.nex (~0.3 s CPU).
	tokIdx, _ := reporterID(r)
	if ok, wait := neReportRateAllow(tokIdx, time.Now()); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(max(wait, 1)))
		w.Header().Set("Cache-Control", "no-store")
		apiError(w, fmt.Sprintf("rate limit: %.0f posts/s per token (burst %.0f)", neReportPerSec, neReportBurst), http.StatusTooManyRequests)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(neMaxObsMB)<<20+1))
	if err != nil || len(body) > neMaxObsMB<<20 {
		apiError(w, fmt.Sprintf("body too large (max %d MB)", neMaxObsMB), 413)
		return
	}
	if !neWriteBudgetGate(w, tokIdx, len(body)) {
		return
	}
	secs, err := parseNESections(body)
	if err != nil {
		apiError(w, "invalid container: "+err.Error(), 400)
		return
	}
	var obsSec, decSec *neSection
	for i := range secs {
		switch secs[i].Magic {
		case "NEO1":
			if obsSec != nil {
				apiError(w, "more than one NEO1 section", 400)
				return
			}
			obsSec = &secs[i]
		case "NEC1":
			if decSec != nil {
				apiError(w, "more than one NEC1 section", 400)
				return
			}
			decSec = &secs[i]
		default:
			apiError(w, "unexpected section "+secs[i].Magic+" (body = one NEO1, optionally with one NEC1 declared layer)", 400)
			return
		}
	}
	if obsSec == nil {
		apiError(w, "expected one NEO1 section (optionally with one NEC1 declared section)", 400)
		return
	}
	h := obsSec.Header
	if got, _ := h["kg"].(string); got != kg {
		apiError(w, "header kg ≠ path kg", 400)
		return
	}
	if res, _ := h["h3_res"].(float64); int(res) != 12 {
		apiError(w, "h3_res must be 12", 400)
		return
	}
	want := []string{"geb", "bau", "acker", "gruen", "wald", "wasser", "verkehr", "alpen", "sonst"}
	gs, _ := h["groups"].([]interface{})
	if len(gs) != len(want) {
		apiError(w, "groups must be the 9-group order "+strings.Join(want, ","), 400)
		return
	}
	for i, g := range gs {
		if g != want[i] {
			apiError(w, "groups must be the 9-group order "+strings.Join(want, ","), 400)
			return
		}
	}
	for _, k := range []string{"algo", "epoch", "source", "cells_n", "digest"} {
		if _, ok := h[k]; !ok {
			apiError(w, "header missing "+k, 400)
			return
		}
	}
	// Record block: cells_n fixed-size rows, first 8 bytes = H3 cell id; the
	// cell SET must equal the declared layer's set (join contract).
	// Row size comes from the header when the peer declares it (`record_bytes`,
	// optionally `fields[]` — srtm's obs-cells row is 41 B as of v2.4; we never
	// hard-code it). Without `record_bytes` it is derived from body/cells_n.
	cellsN, _ := h["cells_n"].(float64)
	raw, err := neInflate(obsSec.Body)
	if err != nil || cellsN < 1 {
		apiError(w, fmt.Sprintf("record block %d B unusable (cells_n=%d, inflate err=%v)", len(raw), int(cellsN), err), 400)
		return
	}
	recSize := 0
	if rb, ok := h["record_bytes"].(float64); ok {
		recSize = int(rb)
		if recSize < 9 || recSize > 64 {
			apiError(w, fmt.Sprintf("header record_bytes=%d out of range (9..64)", recSize), 400)
			return
		}
		if len(raw) != int(cellsN)*recSize {
			apiError(w, fmt.Sprintf("record block %d B ≠ cells_n=%d × record_bytes=%d", len(raw), int(cellsN), recSize), 400)
			return
		}
	} else {
		if len(raw)%int(cellsN) != 0 {
			apiError(w, fmt.Sprintf("record block %d B is not cells_n=%d × fixed row size (declare record_bytes in the header)", len(raw), int(cellsN)), 400)
			return
		}
		recSize = len(raw) / int(cellsN)
		if recSize < 9 || recSize > 64 {
			apiError(w, fmt.Sprintf("row size %d B out of range (9..64)", recSize), 400)
			return
		}
	}
	if fs, ok := h["fields"].([]interface{}); ok && len(fs) == 0 {
		apiError(w, "header fields[] present but empty", 400)
		return
	}
	// ── the declared layer to validate against ──
	// NEC1+NEO1: the posted build. Bare NEO1: the declared VIEW cell set
	// (chunk-log view → bev → lu, ne_change_declared.go — what
	// GET /{kg}/declared serves); only when the set matches neither the view
	// nor the index epoch's lu set is it a 409. A bare NEO1 that still matches
	// lu alone is stored with declared_source=index-2026-03 and shows up in
	// revisit.obs_on_index / manifest.kgs_obs_on_index until re-posted.
	var (
		ours     map[uint64]bool
		decHdr   map[string]interface{}
		decLayer = "lu"
		view     *neDeclView
	)
	luSet, luHdr, luErr := neCellIDs(kg, "nec")
	theirs := make(map[uint64]bool, int(cellsN))
	dup := 0
	for i := 0; i < int(cellsN); i++ {
		id := binary.LittleEndian.Uint64(raw[i*recSize:])
		if theirs[id] {
			dup++
		}
		theirs[id] = true
	}
	sameSet := func(a map[uint64]bool) bool {
		if len(a) != len(theirs) {
			return false
		}
		for id := range theirs {
			if !a[id] {
				return false
			}
		}
		return true
	}
	if decSec != nil {
		decLayer = "bev"
		decHdr = decSec.Header
		if msg := neValidateBevNEC1(kg, decSec); msg != "" {
			apiError(w, "NEC1: "+msg, 400)
			return
		}
		ours, err = neSectionCellIDs(decSec)
		if err != nil {
			apiError(w, "NEC1: "+err.Error(), 400)
			return
		}
	} else {
		view = neDeclViewOf(kg)
		if view == nil && luErr != nil {
			writeJSON(w, 409, map[string]interface{}{"error": "no declared cell set for this KG yet — a bare NEO1 needs our declared view to validate against; post NEC1+NEO1 (your bevdirect declared build) instead",
				"kg": kg, "manifest": tierPrefixFor(r) + "/api/v1/ne/manifest", "declared": tierPrefixFor(r) + "/api/v1/ne/" + kg + "/declared"})
			return
		}
		if view != nil && dup == 0 && sameSet(view.cellSet()) {
			decLayer = "declared"
			ours = view.cellSet()
			decHdr = map[string]interface{}{"digest": view.Digest, "source": view.Source, "epoch": view.Epoch}
		} else if luErr == nil {
			ours, decHdr = luSet, luHdr
		} else {
			ours = view.cellSet()
			decLayer = "declared"
			decHdr = map[string]interface{}{"digest": view.Digest, "source": view.Source, "epoch": view.Epoch}
		}
	}
	var missingIDs, extraIDs []string
	for id := range ours {
		if !theirs[id] && len(missingIDs) < 10 {
			missingIDs = append(missingIDs, fmt.Sprintf("%x", id))
		}
	}
	for id := range theirs {
		if !ours[id] && len(extraIDs) < 10 {
			extraIDs = append(extraIDs, fmt.Sprintf("%x", id))
		}
	}
	if dup > 0 || len(ours) != len(theirs) || len(missingIDs) > 0 || len(extraIDs) > 0 {
		sort.Strings(missingIDs)
		sort.Strings(extraIDs)
		msg := "cell set ≠ our declared view cell set for this KG (nor the index epoch's lu set) — build on GET " + tierPrefixFor(r) + "/api/v1/ne/" + kg + "/declared"
		if decSec != nil {
			msg = "NEO1 cell set ≠ the posted NEC1 cell set"
		}
		out := map[string]interface{}{"error": msg, "kg": kg, "declared_layer": decLayer,
			"ours_n": len(ours), "yours_n": len(theirs), "duplicates": dup, "declared_digest": decHdr["digest"], "declared_source": decHdr["source"],
			"our_lu_digest": luHdr["digest"], "our_lu_epoch": luHdr["epoch"],
			"missing_sample": missingIDs, "extra_sample": extraIDs}
		if view != nil {
			out["view"] = map[string]interface{}{"cells_n": view.CellsN(), "digest": view.Digest, "source": view.Source, "state": view.State, "get": tierPrefixFor(r) + "/api/v1/ne/" + kg + "/declared"}
		}
		writeJSON(w, 409, out)
		return
	}
	decDigest, _ := decHdr["digest"].(string)
	decSource, _ := decHdr["source"].(string)
	// declared_* in the posted header must agree with what we validated against
	for k, v := range map[string]string{"declared_source": decSource, "declared_digest": decDigest} {
		if got, ok := h[k].(string); ok && got != "" && got != v && decSec != nil {
			apiError(w, fmt.Sprintf("header %s=%q ≠ validated declared layer %q", k, got, v), 400)
			return
		}
	}
	neWriteMu.Lock()
	defer neWriteMu.Unlock()
	prev, prevDeclared, prevBev := "", "", ""
	if k := neStore.get(kg); k != nil {
		if oh := k.layerHeader("obs"); oh != nil {
			prev, _ = oh["digest"].(string)
			prevDeclared, _ = oh["declared_digest"].(string)
		}
		if bh := k.layerHeader("bev"); bh != nil {
			prevBev, _ = bh["digest"].(string)
		}
	} else {
		if _, ph, err := neReadHeader(neFilePath(kg, "obs")); err == nil {
			prev, _ = ph["digest"].(string)
			prevDeclared, _ = ph["declared_digest"].(string)
		}
		if _, ph, err := neReadHeader(neFilePath(kg, "bev.nec")); err == nil {
			prevBev, _ = ph["digest"].(string)
		}
	}
	resp := map[string]interface{}{
		"stored": true, "kg": kg, "bytes": len(body), "algo": h["algo"], "epoch": h["epoch"], "source": h["source"],
		"cells_n": h["cells_n"], "row_bytes": recSize, "record_bytes_declared": h["record_bytes"] != nil, "fields": h["fields"],
		"digest": h["digest"], "lu_digest": luHdr["digest"],
		"declared_layer": decLayer, "declared_source": decSource, "declared_digest": decDigest,
		"get": tierPrefixFor(r) + "/api/v1/ne/" + kg + "?layers=lu,bev,obs",
	}
	if view != nil && decLayer == "lu" {
		resp["hint"] = "validated against the index epoch's lu set, not the current declared view — build NEO1 on GET " + tierPrefixFor(r) + "/api/v1/ne/" + kg + "/declared (" + strconv.Itoa(view.CellsN()) + " cells, " + view.Source + ")"
		resp["view"] = map[string]interface{}{"cells_n": view.CellsN(), "digest": view.Digest, "source": view.Source}
	}
	if decSec != nil {
		resp["bev"] = map[string]interface{}{"digest": decDigest, "cells_n": decHdr["cells_n"], "source": decSource, "epoch": decHdr["epoch"]}
		if luErr == nil {
			// border-cell drift between the frozen index epoch and the bevdirect build — informational only
			missing, extra := 0, 0
			for id := range luSet {
				if !ours[id] {
					missing++
				}
			}
			for id := range ours {
				if !luSet[id] {
					extra++
				}
			}
			resp["lu_diff"] = map[string]interface{}{"lu_cells_n": len(luSet), "bev_cells_n": len(ours), "missing_n": missing, "extra_n": extra}
		} else {
			resp["lu_diff"] = nil
		}
	}
	bevUnchanged := decSec == nil || prevBev == decDigest
	d, _ := h["digest"].(string)
	if prev != "" && prev == d && prevDeclared == decDigest && bevUnchanged {
		resp["unchanged"] = true
		writeJSON(w, 200, resp)
		return
	}
	// stored NEO1 header carries the declared layer it was validated against
	hdrOut := make(map[string]interface{}, len(h)+2)
	for k, v := range h {
		hdrOut[k] = v
	}
	hdrOut["declared_source"], hdrOut["declared_digest"] = decSource, decDigest
	obsBytes, err := neRewriteSection(obsSec, hdrOut)
	if err == nil {
		err = os.MkdirAll(neDir, 0o755)
	}
	if err == nil && decSec != nil && !bevUnchanged {
		err = neWriteInput(kg, "bev.nec", decSec.Raw)
	}
	if err == nil {
		err = neWriteInput(kg, "obs", obsBytes)
	}
	if err != nil {
		apiError(w, "store: "+err.Error(), 500)
		return
	}
	resp["unchanged"] = false
	if prev != "" {
		resp["replaced"], resp["previous_digest"] = true, prev
	}
	if decSec != nil && prevBev != "" && !bevUnchanged {
		resp["bev_replaced"], resp["bev_previous_digest"] = true, prevBev
		log.Printf("ne: %s bev section replaced by posted NEC1 %s (was %s, source %s)", kg, decDigest, prevBev, decSource)
	}
	// fold the posted section(s) into {kg}.nex right away (ne_store.go); the
	// input files are deleted once the store re-read verifies the digests
	if err := neStore.convert(kg); err != nil {
		resp["store_warning"] = err.Error()
	}
	go neManifest.refresh(true)
	writeJSON(w, 200, resp)
}

// neWriteInput atomically writes an ingest input file data/ne_cells/{kg}.{kind}.
func neWriteInput(kg, kind string, b []byte) error {
	tmp := neFilePath(kg, kind+".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, neFilePath(kg, kind))
}

// neRewriteSection re-serialises a section with a new header (same body);
// digests are over the inflated record block, so the header is free to change.
func neRewriteSection(s *neSection, hdr map[string]interface{}) ([]byte, error) {
	hj, err := json.Marshal(hdr)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 9+len(hj)+4+len(s.Body))
	out = append(out, s.Magic...)
	out = append(out, 1)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(hj)))
	out = append(out, hj...)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(s.Body)))
	return append(out, s.Body...), nil
}

// neValidateBevNEC1 checks a peer-posted declared section for the `bev` slot:
// frozen algo, a pinned bevdirect source (frozen.bev_source or an equivalent
// tag), the right KG and a consistent record block. "" = ok.
func neValidateBevNEC1(kg string, s *neSection) string {
	h := s.Header
	if got, _ := h["kg"].(string); got != kg {
		return "header kg ≠ path kg"
	}
	if algo, _ := h["algo"].(string); algo != neFrozen["algo"] {
		return fmt.Sprintf("algo %q ≠ frozen %q", algo, neFrozen["algo"])
	}
	src, _ := h["source"].(string)
	if src == "" || neBevEquivalent(src) != neFrozen["bev_source"] {
		return fmt.Sprintf("source %q is not a pinned bevdirect tag (%s or one of %v)", src, neFrozen["bev_source"], neFrozen["bev_equivalent_tags"])
	}
	if res, ok := h["h3_res"].(float64); ok && int(res) != 12 {
		return "h3_res must be 12"
	}
	n, _ := h["cells_n"].(float64)
	if n < 1 {
		return "cells_n missing"
	}
	if _, ok := h["epoch"]; !ok {
		return "header missing epoch"
	}
	return ""
}

// neSectionCellIDs returns the LU cell-id set of an NEC1 section (first cells_n×30 B).
func neSectionCellIDs(s *neSection) (map[uint64]bool, error) {
	raw, err := neInflate(s.Body)
	if err != nil {
		return nil, err
	}
	n, _ := s.Header["cells_n"].(float64)
	kn, _ := s.Header["kcells_n"].(float64)
	if int(n)*neLURowBytes+int(kn)*neKRowBytes != len(raw) {
		return nil, fmt.Errorf("record block %d B ≠ %d×30+%d×29", len(raw), int(n), int(kn))
	}
	ids := make(map[uint64]bool, int(n))
	dup := 0
	for i := 0; i < int(n); i++ {
		id := binary.LittleEndian.Uint64(raw[i*neLURowBytes:])
		if ids[id] {
			dup++
		}
		ids[id] = true
	}
	if dup > 0 {
		return nil, fmt.Errorf("%d duplicate cell ids", dup)
	}
	return ids, nil
}

func neInflate(body []byte) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(zr, 256<<20))
}

// neCellIDs returns the LU cell-id set of our stored NEC1 section of `kind`.
func neCellIDs(kg, kind string) (map[uint64]bool, map[string]interface{}, error) {
	if k := neStore.get(kg); k != nil {
		layer := map[string]string{"nec": "lu", "bev.nec": "bev"}[kind]
		if l := k.Hdr.Layers[layer]; l != nil {
			rows, _, err := neStore.layerRows(k, layer)
			if err != nil {
				return nil, nil, err
			}
			ids := make(map[uint64]bool, l.Rows)
			for i := 0; i < l.Rows; i++ {
				ids[neRowID(rows, i, neLURowBytes)] = true
			}
			return ids, k.layerHeader(layer), nil
		}
	}
	b, err := os.ReadFile(neFilePath(kg, kind))
	if err != nil {
		return nil, nil, err
	}
	secs, err := parseNESections(b)
	if err != nil || len(secs) == 0 || secs[0].Magic != "NEC1" {
		return nil, nil, fmt.Errorf("stored NEC1 unreadable")
	}
	raw, err := neInflate(secs[0].Body)
	if err != nil {
		return nil, nil, err
	}
	n, _ := secs[0].Header["cells_n"].(float64)
	ids := make(map[uint64]bool, int(n))
	for i := 0; i < int(n) && (i+1)*30 <= len(raw); i++ {
		ids[binary.LittleEndian.Uint64(raw[i*30:])] = true
	}
	return ids, secs[0].Header, nil
}

// ── epoch reports: POST <private>/api/v1/ne/{kg}/report ────────────────────────────
//
// A peer that recomputed the cells of a KG (or part of it) from a bevdirect
// fetch posts DIGESTS only (`python3 -m ne_cells report`): the whole-KG digest
// and one sha256[:16] per res-10 chunk. We compare against our stored epoch
// and answer which chunks differ — that list is the change-detection signal
// (docs/ne-cells.md §epochs). Reports are appended to data/ne_cells/{kg}.reports.jsonl.

type neReport struct {
	KG       string            `json:"kg"`
	Algo     string            `json:"algo"`
	Epoch    string            `json:"epoch"`
	Source   string            `json:"source"`
	Observer string            `json:"observer"`
	H3Res    int               `json:"h3_res"`
	ChunkRes int               `json:"chunk_res"`
	CellsN   int               `json:"cells_n"`
	Digest   string            `json:"digest"`
	BBox     []float64         `json:"bbox,omitempty"`
	Chunks   map[string]string `json:"chunks"`
	// NE change (docs/ne-change.md §3): optional digest_ap per chunk (sha256 of
	// the rows with the register bytes zeroed). Never stored in full — the
	// stored line keeps only `want_ap` for the chunks we asked for.
	ChunksAP  map[string]string `json:"chunks_ap,omitempty"`
	APVersion string            `json:"ap_version,omitempty"`
	// QC provenance (docs/ne-qc-proposal.md §1), optional. `tiles` = the fetch
	// the build was made from; fetched < expected ⇒ lossy, not change.
	// `chunks_tiles` = per chunk sha256 (any 16–64 hex, kept as 16) over the
	// bytes of the source tiles covering it, sorted by z/x/y. Never stored in
	// full — the stored line keeps `want_tiles` for the chunks we asked for;
	// the chunk log keeps the hash with each version.
	Tiles       *neReportTiles    `json:"tiles,omitempty"`
	ChunksTiles map[string]string `json:"chunks_tiles,omitempty"`
}

type neReportTiles struct {
	Expected int    `json:"expected"`
	Fetched  int    `json:"fetched"`
	Hash     string `json:"hash,omitempty"` // over all tiles of the build
}

// h3Parent mirrors ne_cells.pack.h3_parent (pure bit arithmetic, h3 v4 layout).
func h3Parent(cid uint64, res uint) uint64 {
	cur := uint((cid >> 52) & 0xF)
	if res >= cur {
		return cid
	}
	out := (cid &^ (0xF << 52)) | (uint64(res) << 52)
	for d := res + 1; d < 16; d++ {
		out |= 0x7 << ((15 - d) * 3)
	}
	return out
}

// neStoredSources lists the declared sections we hold for a KG: {kg}.nec is the
// index epoch (source index-YYYY-MM), {kg}.bev.nec the bevdirect baseline
// (source bevdirect@<tag>, built by the operator from a local bevdirect-serve).
// A report is only compared against the section with the SAME source string:
// index vs bevdirect differ by source noise (lu ±8/255, declared_unknown 1 953
// vs 0 in 63330), not by change — FIX 4 of srtm's check.
// neBevEquivalent maps bevdirect tags whose cell output is byte-identical onto
// the baseline tag we hold (docs/ne-cells.md §FREEZE); v0.2.1 = v0.2.0 + tile
// cache sweeper, no assembly change.
func neBevEquivalent(src string) string {
	if eq, ok := neBevEquivalentTags[src]; ok {
		return eq
	}
	return src
}

var neBevEquivalentTags = map[string]string{
	"bevdirect@v0.2.1": "bevdirect@v0.2.0", // + tile-cache sweeper only
	"bevdirect@v0.3.0": "bevdirect@v0.2.0", // first public tag of github.com/raffopenssh/vtcseamless; byte-identical on five viewports
	"bevdirect@v0.3.1": "bevdirect@v0.2.0", // RAM-only tile cache (no .pbf on disk); download/cache layer only, no assembly change
	"bevdirect@v0.3.2": "bevdirect@v0.2.0", // multi-cell /viewport dedupe fix (Piece.complete/members); aligned 0.02° cell docs byte-identical apart from the 2 new fields, which ne_cells/canon.py ignores
	"bevdirect@v0.3.3": "bevdirect@v0.2.0", // empty layers [] instead of null; no geometry change (aligned cells verified identical v0.3.1 = v0.3.2 on 97 cells, 2026-10-08)
	"bevdirect@v0.3.4": "bevdirect@v0.2.0", // tile hashes (tiles_expected/fetched, per-tile sha256) in stats + deterministic-assembly CI; release notes promise no geometry/assembly change (docs/ne-qc-peer-prompts.md A.3); first rows 2026-10-15: 63330 qc re-report unchanged=true against its own v0.3.4 baseline
	"bevdirect@v0.3.5": "bevdirect@v0.2.0", // 2026-10-23: TLS trust only (GEANT TLS RSA 1 intermediate BEV stopped sending, AIA fallback, -ca-bundle) + /health upstream{}; no assembly change, same tile bytes → same digests
	"bevdirect@v0.3.6": "bevdirect@v0.2.0", // 2026-10-24: cellOf epsilon (15.54/0.02 floored to 776 → aligned request folded the W/S neighbour in, compose kept its truncated copy of parcels wider than cell+pad). Affected cells (≈1 214/18 000 columns and rows) change ONCE; the per-chunk rule absorbs that as a candidate that the next v0.3.6 pass confirms — it was the cause of the {MAP_NAME}/srtm border asymmetry, not two build deviations
}

// NOTE (2026-10-08 rebuild, docs/ne-cells.md §coverage): the tag is NOT what
// separates comparable from non-comparable reports — the request layout is.
// Lossy builds (unaligned/multi-cell viewports) are caught per report by the
// coverage rule in api_ne_reports.go (neBestCoverage), on every tag.

func neStoredSources(kg string) map[string]string {
	out := map[string]string{}
	if k := neStore.get(kg); k != nil {
		for layer, kind := range map[string]string{"lu": "nec", "bev": "bev.nec"} {
			if h := k.layerHeader(layer); h != nil {
				if src, _ := h["source"].(string); src != "" {
					out[src] = kind
				}
			}
		}
	}
	for _, kind := range []string{"nec", "bev.nec"} {
		b, err := os.ReadFile(neFilePath(kg, kind))
		if err != nil {
			continue
		}
		if secs, err := parseNESections(b); err == nil && len(secs) > 0 && secs[0].Magic == "NEC1" {
			if src, _ := secs[0].Header["source"].(string); src != "" {
				out[src] = kind
			}
		}
	}
	return out
}

// neChunkDigests recomputes the per-res-10 chunk digests of our stored NEC1 section of `kind`.
func neChunkDigests(kg, kind string) (map[string]string, map[string]interface{}, error) {
	var raw []byte
	var hdr map[string]interface{}
	layer := map[string]string{"nec": "lu", "bev.nec": "bev"}[kind]
	if k := neStore.get(kg); k != nil && k.Hdr.Layers[layer] != nil {
		rows, _, err := neStore.layerRows(k, layer)
		if err != nil {
			return nil, nil, err
		}
		raw, hdr = rows, k.layerHeader(layer)
	} else {
		b, err := os.ReadFile(neFilePath(kg, kind))
		if err != nil {
			return nil, nil, err
		}
		secs, err := parseNESections(b)
		if err != nil || len(secs) == 0 || secs[0].Magic != "NEC1" {
			return nil, nil, fmt.Errorf("stored NEC1 unreadable")
		}
		zr, _ := zlib.NewReader(bytes.NewReader(secs[0].Body))
		raw, _ = io.ReadAll(zr)
		hdr = secs[0].Header
	}
	n, _ := hdr["cells_n"].(float64)
	groups := map[uint64][]byte{}
	var order []uint64
	for i := 0; i < int(n); i++ {
		rec := raw[i*30 : (i+1)*30]
		p := h3Parent(binary.LittleEndian.Uint64(rec), 10)
		if _, ok := groups[p]; !ok {
			order = append(order, p)
		}
		groups[p] = append(groups[p], rec...)
	}
	out := make(map[string]string, len(groups))
	for _, p := range order {
		s := sha256.Sum256(groups[p])
		out[fmt.Sprintf("%x", p)] = hex.EncodeToString(s[:])[:16]
	}
	return out, hdr, nil
}

func handleNEReportPost(w http.ResponseWriter, r *http.Request, kg string, tokIdx int) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		apiError(w, "read", 400)
		return
	}
	if !neWriteBudgetGate(w, tokIdx, len(body)) {
		return
	}
	var rep neReport
	if err := json.Unmarshal(body, &rep); err != nil || rep.KG != kg || rep.Algo == "" || rep.Epoch == "" ||
		rep.Observer == "" || rep.Digest == "" {
		neReportCounters.bump(tokIdx, rep.Observer, "rejected")
		apiError(w, "report needs kg (= path), algo, epoch, source, observer, digest, chunks{}", 400)
		return
	}
	if msg := neReportValidate(&rep); msg != "" {
		neReportCounters.bump(tokIdx, rep.Observer, "rejected")
		apiError(w, msg, 400)
		return
	}
	tokName := tokenName(tokIdx)
	if len(rep.Chunks) == 0 {
		// Step 0 (docs/ne-change.md §3): header only. Equal to the last full
		// report of this token/class/bbox → unchanged (and every chunk of that
		// report is a re-sighting of its digest_ap); else ask for the chunks.
		neReportHeaderOnly(w, kg, tokIdx, tokName, &rep)
		return
	}
	chunksAP := rep.ChunksAP
	rep.ChunksAP = nil // never stored in full
	// rep.ChunksTiles is read by neReportWants and dropped before the line is stored
	stored := neStoredSources(kg)
	kind, sameSource := stored[neBevEquivalent(rep.Source)]
	var ours map[string]string
	var hdr map[string]interface{}
	if sameSource {
		ours, hdr, err = neChunkDigests(kg, kind)
	}
	resp := map[string]interface{}{"kg": kg, "stored": true, "observer": rep.Observer, "token": tokName, "epoch": rep.Epoch,
		"source": rep.Source, "chunks_reported": len(rep.Chunks), "identical_source": sameSource, "baseline": "none"}
	srcs := make([]string, 0, len(stored))
	for s := range stored {
		srcs = append(srcs, s)
	}
	sort.Strings(srcs)
	resp["our_sources"] = srcs
	prior := neReadReports(kg)
	if prev := neReportPrevious(prior, tokName, &rep); prev != nil && prev.Report.Digest == rep.Digest && prev.Report.Epoch == rep.Epoch {
		resp["unchanged"] = true
		resp["unchanged_since"] = prev.ReceivedAt
	} else {
		resp["unchanged"] = false
	}
	switch {
	case sameSource && err != nil:
		resp["compared"] = false
		resp["note"] = "stored section unreadable: " + err.Error()
	case sameSource && hdr["algo"] != rep.Algo:
		resp["compared"] = false
		resp["note"] = fmt.Sprintf("algo mismatch: ours %v, yours %s — digests are not comparable", hdr["algo"], rep.Algo)
	case sameSource:
		same, changed, unknown := neCompareChunks(ours, rep.Chunks)
		resp["compared"] = true
		resp["baseline"] = "section:" + kind
		resp["chunks_compared"] = same + len(changed)
		resp["our_epoch"] = hdr["epoch"]
		resp["our_digest"] = hdr["digest"]
		resp["identical_kg_digest"] = hdr["digest"] == rep.Digest
		resp["chunks_same"] = same
		resp["chunks_changed"] = changed
		resp["chunks_unknown_to_us"] = unknown
	default:
		// No section of this source: the baseline is per CHUNK — the digest
		// each res-10 chunk was first reported with, over every earlier full
		// report of the same (source, algo, chunk_res) for this KG. Chunks
		// never seen before become baseline now (viewport observers cover a
		// KG piecewise), overlapping chunks are compared.
		hist := neReportHistory(prior, &rep)
		if hist.Reports > 0 {
			same, changed, unknown := neCompareChunks(hist.digests(hist.First), rep.Chunks)
			base := hist.FirstReport
			resp["baseline_reports"] = hist.Reports
			resp["baseline_chunks"] = len(hist.First)
			resp["baseline_observer"] = base.Report.Observer
			resp["baseline_token"] = base.Token
			resp["baseline_epoch"] = base.Report.Epoch
			resp["baseline_received_at"] = base.ReceivedAt
			resp["baseline_last_received_at"] = hist.LastAt
			resp["our_digest"] = base.Report.Digest // = baseline_digest: the first-seen peer report we anchor epoch 0 on, not a build of ours
			resp["baseline_digest"] = base.Report.Digest
			resp["baseline_bbox"] = base.Report.BBox
			resp["identical_kg_digest"] = base.Report.Digest == rep.Digest
			resp["chunks_unknown_to_us"] = unknown
			resp["chunks_baselined_now"] = len(unknown)
			if same+len(changed) > 0 {
				resp["compared"] = true
				resp["baseline"] = "report"
				resp["chunks_compared"] = same + len(changed)
				resp["chunks_same"] = same
				resp["chunks_changed"] = changed
				note := fmt.Sprintf("compared against the first-seen digest of each chunk (%d earlier reports of this source)", hist.Reports)
				if len(unknown) > 0 {
					note += fmt.Sprintf("; %d chunks not reported before are now part of the baseline", len(unknown))
				}
				if len(stored) > 0 {
					note += " — we hold no section with this source; index vs bevdirect differ by source noise, not change"
				}
				resp["note"] = note
			} else {
				resp["compared"] = false
				resp["baseline"] = "this_report"
				resp["chunks_changed"] = []string{}
				resp["note"] = fmt.Sprintf("no overlap with the %d earlier reports of this source (%d chunks) — these %d chunks are now part of the baseline", hist.Reports, len(hist.First), len(unknown))
			}
		} else {
			resp["compared"] = false
			resp["baseline"] = "this_report"
			resp["chunks_baselined_now"] = len(rep.Chunks)
			if len(stored) == 0 {
				resp["note"] = "first report for this KG — kept as the baseline for later reports of the same source"
			} else {
				resp["note"] = "no stored epoch with this source — this report is now the baseline for " + rep.Source + " on this KG"
			}
		}
	}
	// Change over time: besides "vs epoch 0" every answer carries "vs the most
	// recent digest we hold per chunk" from earlier reports of this source, so
	// a chunk that changed once and stayed stable is not re-reported forever.
	if sl := neSinceLast(neReportHistory(prior, &rep), &rep); sl != nil {
		resp["since_last"] = sl
	}
	// Coverage rule (neBestCoverage): a lossy build's diffs are not change.
	cov := neCoverageOf(prior, &rep)
	resp["coverage"] = cov
	if t := rep.Tiles; t != nil && t.Expected > 0 && t.Fetched < t.Expected {
		resp["change_suspect"] = "tiles_incomplete"
		resp["note"] = fmt.Sprintf("%v the build fetched %d of %d source tiles: chunk diffs are not counted as change — retry the missing tiles and rebuild",
			strings.TrimSpace(fmt.Sprint(resp["note"])+";"), t.Fetched, t.Expected)
	} else if cov["status"] == "lossy" {
		resp["change_suspect"] = "coverage_lossy"
		resp["note"] = fmt.Sprintf("%v cells_n %d is %.1f %% below the best build seen for a bbox inside this one (%d): incomplete fetch (missing tile, partial cell set) or unaligned/multi-cell viewports? chunk diffs are not counted as change — rebuild from aligned 0.02° cells with every tile fetched",
			strings.TrimSpace(fmt.Sprint(resp["note"])+";"), rep.CellsN, cov["deficit_pct"], cov["best_cells_n"])
	} else if sl, _ := resp["since_last"].(map[string]interface{}); neLayoutSuspect(sl) {
		resp["change_suspect"] = "layout_change"
		resp["note"] = fmt.Sprintf("%v since_last diffs were taken against digests built under a different bbox (%v chunks): request layout alone moves chunk digests, so they are not counted as change — this report re-baselines the cell block",
			strings.TrimSpace(fmt.Sprint(resp["note"])+";"), sl["changed_previous_bbox_differs"])
	}
	if resp["unchanged"] == true {
		resp["since_last"] = map[string]interface{}{"compared": len(rep.Chunks), "same": len(rep.Chunks), "changed": []string{}, "previous_received_at": resp["unchanged_since"]}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if chunksAP != nil {
		neReportWants(kg, tokName, now, &rep, chunksAP, resp)
	}
	rep.ChunksTiles = nil
	line := neStoredReport{ReceivedAt: now, Token: tokName, Report: rep, Result: resp}
	if resp["unchanged"] == true {
		line.Unchanged = true
		line.Report.Chunks = nil // digest equal ⇒ chunks equal; keep the file small
	}
	neAppendReport(kg, line)
	neReportCounters.record(tokIdx, &rep, resp)
	nChg, nUnk := 0, 0
	if ch, _ := resp["chunks_changed"].([]string); ch != nil {
		nChg = len(ch)
	}
	if u, _ := resp["chunks_unknown_to_us"].([]string); u != nil {
		nUnk = len(u)
	}
	slChg := -1
	if sl, _ := resp["since_last"].(map[string]interface{}); sl != nil {
		slChg = len(sl["changed"].([]string))
	}
	log.Printf("🧾 NE report kg=%s tok=%s obs=%q src=%s epoch=%s chunks=%d → baseline=%v compared=%v changed=%d new=%d since_last_changed=%d unchanged=%v",
		kg, tokName, rep.Observer, rep.Source, rep.Epoch, len(rep.Chunks), resp["baseline"], resp["compared"], nChg, nUnk, slChg, resp["unchanged"])
	resp["policy"] = map[string]interface{}{"reports_per_s_per_token": neReportPerSec, "burst": neReportBurst}
	writeJSON(w, 200, resp)
}

// handleNEReportList — the stored lines with the effective verdict: the
// coverage/layout rules are applied on read (neMarkLossy), so a line stored
// under an older rule shows what counts today, not what was written.
func handleNEReportList(w http.ResponseWriter, kg string) {
	rows := neMarkLossy(neReadReports(kg))
	writeJSON(w, 200, map[string]interface{}{"kg": kg, "reports": rows, "count": len(rows)})
}
