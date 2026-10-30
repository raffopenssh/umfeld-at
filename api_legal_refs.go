// api_legal_refs.go — Legal parcel reference API.
//
// Loads data/legal_parcel_refs.csv at startup into in-memory indices keyed by
// KG code and parcel ID (kg_code + "-" + gnr). Exposes four endpoints under
// /api/v1/legal/* and provides helper functions used by other search handlers
// to enrich parcel/KG/EZ results with legal reference metadata.
package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
)

// ═══════════════════════════════════════════════════════════════════════════════
// In-memory index
// ═══════════════════════════════════════════════════════════════════════════════

const legalRefsCSVPath = "data/legal_parcel_refs.csv"

// LegalParcelRef — one row of data/legal_parcel_refs.csv: a law text in RIS
// that names a cadastral parcel. Served per KG code / by search only.
type LegalParcelRef struct {
	KGCode         string
	KGName         string
	Grundstueck    string
	SourceLawName  string
	SourceDate     string
	SourceURL      string
	ParcelListType string // "listed" or "boundary_walk"
	LegalContext   string // e.g. "national_park", "nature_protection", "water_protection", "admin_boundary"
	Bundesland     string
	Gesetzesnummer string
	EZ             string
}

var (
	legalRefsByKG     map[string][]LegalParcelRef // kg_code → refs
	legalRefsByParcel map[string][]LegalParcelRef // "kg_code-gnr" → refs
	legalRefsByEZ     map[string][]LegalParcelRef // "kg_code-ez" → refs
	legalRefsAll      []LegalParcelRef
	legalRefsOnce     sync.Once
	legalRefsLoaded   bool
)

// parcelLegalID builds the lookup key for per-parcel legal refs.
func parcelLegalID(kgCode, gnr string) string {
	return kgCode + "-" + gnr
}

// ezLegalID builds the lookup key for per-EZ legal refs.
func ezLegalID(kgCode, ez string) string {
	return kgCode + "-EZ-" + ez
}

// loadLegalRefs reads the CSV once and populates the in-memory indices.
func loadLegalRefs() {
	legalRefsOnce.Do(func() {
		legalRefsByKG = make(map[string][]LegalParcelRef)
		legalRefsByParcel = make(map[string][]LegalParcelRef)
		legalRefsByEZ = make(map[string][]LegalParcelRef)

		f, err := os.Open(legalRefsCSVPath)
		if err != nil {
			log.Printf("⚖️  Legal refs CSV not found (%s) — legal API disabled", legalRefsCSVPath)
			return
		}
		defer f.Close()

		reader := csv.NewReader(f)
		reader.LazyQuotes = true
		reader.FieldsPerRecord = -1 // allow variable column count (skip corrupted rows)

		// Skip header
		header, err := reader.Read()
		if err != nil {
			log.Printf("⚖️  Legal refs CSV header error: %v", err)
			return
		}
		_ = header

		var skipped int
		for {
			record, err := reader.Read()
			if err == io.EOF {
				break
			}
			if err != nil {
				skipped++
				continue
			}
			// Accept both old 10-column and new 11-column format
			if len(record) < 10 {
				skipped++
				continue
			}

			ref := LegalParcelRef{
				KGCode:         record[0],
				KGName:         record[1],
				Grundstueck:    record[2],
				SourceLawName:  record[3],
				SourceDate:     record[4],
				SourceURL:      record[5],
				ParcelListType: record[6],
				LegalContext:   record[7],
				Bundesland:     record[8],
				Gesetzesnummer: record[9],
			}
			if len(record) > 10 {
				ref.EZ = record[10]
			}

			legalRefsAll = append(legalRefsAll, ref)
			legalRefsByKG[ref.KGCode] = append(legalRefsByKG[ref.KGCode], ref)
			pid := parcelLegalID(ref.KGCode, ref.Grundstueck)
			legalRefsByParcel[pid] = append(legalRefsByParcel[pid], ref)
			if ref.EZ != "" {
				key := ezLegalID(ref.KGCode, ref.EZ)
				legalRefsByEZ[key] = append(legalRefsByEZ[key], ref)
			}
		}

		legalRefsLoaded = true
		log.Printf("⚖️  Loaded %d legal parcel refs (%d KGs, %d parcels, %d EZ groups, %d skipped)",
			len(legalRefsAll), len(legalRefsByKG), len(legalRefsByParcel), len(legalRefsByEZ), skipped)
	})
}

// ═══════════════════════════════════════════════════════════════════════════════
// JSON serialisation helper
// ═══════════════════════════════════════════════════════════════════════════════

func legalRefToMap(ref LegalParcelRef) map[string]interface{} {
	m := map[string]interface{}{
		"kg_code":          ref.KGCode,
		"kg_name":          ref.KGName,
		"grundstueck":      ref.Grundstueck,
		"source_law_name":  ref.SourceLawName,
		"source_date":      ref.SourceDate,
		"source_url":       ref.SourceURL,
		"parcel_list_type": ref.ParcelListType,
		"legal_context":    ref.LegalContext,
		"bundesland":       ref.Bundesland,
		"gesetzesnummer":   ref.Gesetzesnummer,
	}
	if ref.EZ != "" {
		m["ez"] = ref.EZ
	}
	return m
}

func legalRefsToMaps(refs []LegalParcelRef) []map[string]interface{} {
	results := make([]map[string]interface{}, 0, len(refs))
	for _, ref := range refs {
		results = append(results, legalRefToMap(ref))
	}
	return results
}

// ═══════════════════════════════════════════════════════════════════════════════
// Filtering
// ═══════════════════════════════════════════════════════════════════════════════

func filterLegalRefs(refs []LegalParcelRef, context, listType, lawQ string) []LegalParcelRef {
	if context == "" && listType == "" && lawQ == "" {
		return refs
	}
	lawQLower := strings.ToLower(lawQ)
	var filtered []LegalParcelRef
	for _, ref := range refs {
		if context != "" && ref.LegalContext != context {
			continue
		}
		if listType != "" && ref.ParcelListType != listType {
			continue
		}
		if lawQ != "" && !strings.Contains(strings.ToLower(ref.SourceLawName), lawQLower) {
			continue
		}
		filtered = append(filtered, ref)
	}
	return filtered
}

// ═══════════════════════════════════════════════════════════════════════════════
// Public enrichment helpers (used by search handlers)
// ═══════════════════════════════════════════════════════════════════════════════

// LegalRefsForParcel returns legal refs for a parcel identified by kg_code and gnr.
func LegalRefsForParcel(kgCode, gnr string) []LegalParcelRef {
	loadLegalRefs()
	return legalRefsByParcel[parcelLegalID(kgCode, gnr)]
}

// LegalRefsForKG returns legal refs for an entire KG.
func LegalRefsForKG(kgCode string) []LegalParcelRef {
	loadLegalRefs()
	return legalRefsByKG[kgCode]
}

// LegalRefCountForKG returns the count and distinct contexts for a KG.
func LegalRefCountForKG(kgCode string) (int, []string) {
	refs := LegalRefsForKG(kgCode)
	if len(refs) == 0 {
		return 0, nil
	}
	contextSet := make(map[string]bool)
	for _, ref := range refs {
		if ref.LegalContext != "" {
			contextSet[ref.LegalContext] = true
		}
	}
	contexts := make([]string, 0, len(contextSet))
	for c := range contextSet {
		contexts = append(contexts, c)
	}
	sort.Strings(contexts)
	return len(refs), contexts
}

// LegalRefsForEZ returns legal refs for all parcels in a given EZ.
func LegalRefsForEZ(kgCode, ez string) []LegalParcelRef {
	loadLegalRefs()
	return legalRefsByEZ[ezLegalID(kgCode, ez)]
}

// LegalRefCountForEZ returns the count and distinct contexts for an EZ.
func LegalRefCountForEZ(kgCode, ez string) (int, []string) {
	refs := LegalRefsForEZ(kgCode, ez)
	if len(refs) == 0 {
		return 0, nil
	}
	contextSet := make(map[string]bool)
	for _, ref := range refs {
		if ref.LegalContext != "" {
			contextSet[ref.LegalContext] = true
		}
	}
	contexts := make([]string, 0, len(contextSet))
	for c := range contextSet {
		contexts = append(contexts, c)
	}
	sort.Strings(contexts)
	return len(refs), contexts
}

// legalKGCodesForContext returns all KG codes that have legal refs,
// optionally filtered by a specific legal_context. Used as a SQL pre-filter.
func legalKGCodesForContext(context string) []string {
	loadLegalRefs()
	if context == "" {
		// All KGs with any legal refs
		codes := make([]string, 0, len(legalRefsByKG))
		for kg := range legalRefsByKG {
			codes = append(codes, kg)
		}
		return codes
	}
	// Only KGs with refs matching the given context
	kgSet := make(map[string]bool)
	for _, ref := range legalRefsAll {
		if ref.LegalContext == context {
			kgSet[ref.KGCode] = true
		}
	}
	codes := make([]string, 0, len(kgSet))
	for kg := range kgSet {
		codes = append(codes, kg)
	}
	return codes
}

// EnrichParcelResult adds legal_refs to a parcel result map (if any exist).
func EnrichParcelResult(result map[string]interface{}) {
	kgCode, _ := result["kg_code"].(string)
	gnr, _ := result["gnr"].(string)
	if kgCode == "" || gnr == "" {
		return
	}
	refs := LegalRefsForParcel(kgCode, gnr)
	if len(refs) > 0 {
		result["legal_refs"] = legalRefsToMaps(refs)
	}
}

// EnrichEZResult adds legal_ref_count and legal_contexts to an EZ result map.
func EnrichEZResult(result map[string]interface{}) {
	kgCode, _ := result["kg_code"].(string)
	ez, _ := result["ez"].(string)
	if kgCode == "" || ez == "" {
		return
	}
	count, contexts := LegalRefCountForEZ(kgCode, ez)
	if count > 0 {
		result["legal_ref_count"] = count
		result["legal_contexts"] = contexts
	}
}

// EnrichKGResult adds legal_ref_count and legal_contexts to a KG result map.
func EnrichKGResult(result map[string]interface{}) {
	kgCode, _ := result["kg_code"].(string)
	if kgCode == "" {
		return
	}
	count, contexts := LegalRefCountForKG(kgCode)
	if count > 0 {
		result["legal_ref_count"] = count
		result["legal_contexts"] = contexts
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// Handler stubs (assigned in init)
// ═══════════════════════════════════════════════════════════════════════════════

var (
	handleV1LegalKG     http.HandlerFunc = notImplemented("legal/kg")
	handleV1LegalParcel http.HandlerFunc = notImplemented("legal/parcel")
	handleV1LegalStats  http.HandlerFunc = notImplemented("legal/stats")
	handleV1LegalSearch http.HandlerFunc = notImplemented("legal/search")
)

func init() {
	handleV1LegalKG = legalKGHandler
	handleV1LegalParcel = legalParcelHandler
	handleV1LegalStats = legalStatsHandler
	handleV1LegalSearch = legalSearchHandler
}

// RegisterLegalRefsAPI registers the /api/v1/legal/* routes.
func RegisterLegalRefsAPI(mux *http.ServeMux) {
	loadLegalRefs()

	v1 := func(pattern string, handler http.HandlerFunc) {
		mux.Handle(pattern, apiMiddleware(handler))
	}

	v1("/api/v1/legal/kg/", handleV1LegalKG)
	v1("/api/v1/legal/parcel/", handleV1LegalParcel)
	v1("/api/v1/legal/stats", handleV1LegalStats)
	v1("/api/v1/legal/search", handleV1LegalSearch)

	log.Println("⚖️  Legal refs API routes registered under /api/v1/legal/")
}

// ═══════════════════════════════════════════════════════════════════════════════
// GET /api/v1/legal/kg/{kg_code}
// All legal refs for a KG. Optional: ?context=, ?type=, ?law=
// ═══════════════════════════════════════════════════════════════════════════════

func legalKGHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		apiError(w, "method not allowed", 405)
		return
	}
	loadLegalRefs()
	if !legalRefsLoaded {
		apiError(w, "legal refs data not available", 503)
		return
	}

	ac := ParseAPIContext(r)

	// Extract kg_code from path: /api/v1/legal/kg/{kg_code}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/legal/kg/")
	kgCode := NormalizeKGCode(strings.TrimSuffix(strings.Split(path, "/")[0], "/"))
	if kgCode == "" {
		apiError(w, "kg_code required in path", 400)
		return
	}

	qp := r.URL.Query()
	contextF := qp.Get("context")
	typeF := qp.Get("type")
	lawF := qp.Get("law")

	refs := legalRefsByKG[kgCode]
	filtered := filterLegalRefs(refs, contextF, typeF, lawF)

	total := len(filtered)

	// Apply pagination
	start := ac.Offset
	if start > len(filtered) {
		start = len(filtered)
	}
	end := start + ac.Limit
	if end > len(filtered) {
		end = len(filtered)
	}
	paged := filtered[start:end]

	// Compute distinct contexts and laws for this KG
	contextSet := make(map[string]bool)
	lawSet := make(map[string]bool)
	for _, ref := range filtered {
		if ref.LegalContext != "" {
			contextSet[ref.LegalContext] = true
		}
		if ref.Gesetzesnummer != "" {
			lawSet[ref.Gesetzesnummer] = true
		}
	}
	contexts := sortedKeys(contextSet)

	results := legalRefsToMaps(paged)

	meta := NewMeta(ac, total)
	apiResponse(w, r, map[string]interface{}{
		"kg_code":        kgCode,
		"total_refs":     total,
		"unique_laws":    len(lawSet),
		"legal_contexts": contexts,
		"refs":           results,
	}, meta)
}

// ═══════════════════════════════════════════════════════════════════════════════
// GET /api/v1/legal/parcel/{kg_code}/{gnr}
// Legal refs for a specific parcel.
// ═══════════════════════════════════════════════════════════════════════════════

func legalParcelHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		apiError(w, "method not allowed", 405)
		return
	}
	loadLegalRefs()
	if !legalRefsLoaded {
		apiError(w, "legal refs data not available", 503)
		return
	}

	ac := ParseAPIContext(r)

	// Extract from path: /api/v1/legal/parcel/{kg_code}/{gnr}
	// gnr can contain "/" (e.g. "213/1"), so we split carefully
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/legal/parcel/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		apiError(w, "path must be /api/v1/legal/parcel/{kg_code}/{gnr}", 400)
		return
	}
	kgCode := NormalizeKGCode(parts[0])
	gnr := strings.TrimSuffix(parts[1], "/")

	refs := legalRefsByParcel[parcelLegalID(kgCode, gnr)]

	results := legalRefsToMaps(refs)
	meta := NewMeta(ac, len(refs))
	apiResponse(w, r, map[string]interface{}{
		"kg_code":     kgCode,
		"grundstueck": gnr,
		"parcel_id":   parcelLegalID(kgCode, gnr),
		"total_refs":  len(refs),
		"refs":        results,
	}, meta)
}

// ═══════════════════════════════════════════════════════════════════════════════
// GET /api/v1/legal/stats
// Summary statistics across all legal refs.
// ═══════════════════════════════════════════════════════════════════════════════

func legalStatsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		apiError(w, "method not allowed", 405)
		return
	}
	loadLegalRefs()
	if !legalRefsLoaded {
		apiError(w, "legal refs data not available", 503)
		return
	}

	ac := ParseAPIContext(r)

	// By legal_context
	contextCounts := make(map[string]int)
	// By parcel_list_type
	typeCounts := make(map[string]int)
	// Unique KGs
	kgSet := make(map[string]bool)
	// Unique laws
	lawSet := make(map[string]bool)
	// By bundesland
	bundeslandCounts := make(map[string]int)

	for _, ref := range legalRefsAll {
		ctx := ref.LegalContext
		if ctx == "" {
			ctx = "unclassified"
		}
		contextCounts[ctx]++
		typeCounts[ref.ParcelListType]++
		kgSet[ref.KGCode] = true
		if ref.Gesetzesnummer != "" {
			lawSet[ref.Gesetzesnummer] = true
		}
		bl := ref.Bundesland
		if bl == "" {
			bl = "unknown"
		}
		bundeslandCounts[bl]++
	}

	meta := NewMeta(ac, len(legalRefsAll))
	apiResponse(w, r, map[string]interface{}{
		"total_refs":          len(legalRefsAll),
		"unique_kgs":          len(kgSet),
		"unique_parcels":      len(legalRefsByParcel),
		"unique_laws":         len(lawSet),
		"by_legal_context":    contextCounts,
		"by_parcel_list_type": typeCounts,
		"by_bundesland":       bundeslandCounts,
	}, meta)
}

// ═══════════════════════════════════════════════════════════════════════════════
// GET /api/v1/legal/search?q=Kalkalpen
// Full-text search across all CSV fields.
// Optional filters: ?context=, ?type=, ?bundesland=, ?kg=
// ═══════════════════════════════════════════════════════════════════════════════

func legalSearchHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		apiError(w, "method not allowed", 405)
		return
	}
	loadLegalRefs()
	if !legalRefsLoaded {
		apiError(w, "legal refs data not available", 503)
		return
	}

	ac := ParseAPIContext(r)
	qp := r.URL.Query()
	q := qp.Get("q")
	contextF := qp.Get("context")
	typeF := qp.Get("type")
	bundeslandF := qp.Get("bundesland")
	kgF := qp.Get("kg")

	if q == "" && contextF == "" && typeF == "" && bundeslandF == "" && kgF == "" {
		apiError(w, "at least one of q, context, type, bundesland, kg required", 400)
		return
	}

	qLower := strings.ToLower(q)
	blLower := strings.ToLower(bundeslandF)

	var matched []LegalParcelRef
	for _, ref := range legalRefsAll {
		// Apply structured filters first (cheap)
		if contextF != "" && ref.LegalContext != contextF {
			continue
		}
		if typeF != "" && ref.ParcelListType != typeF {
			continue
		}
		if kgF != "" && ref.KGCode != kgF {
			continue
		}
		if bundeslandF != "" && !strings.Contains(strings.ToLower(ref.Bundesland), blLower) {
			continue
		}

		// Free-text search across all fields
		if q != "" {
			haystack := strings.ToLower(
				ref.KGCode + " " + ref.KGName + " " + ref.Grundstueck + " " +
					ref.SourceLawName + " " + ref.SourceDate + " " +
					ref.LegalContext + " " + ref.Bundesland + " " + ref.Gesetzesnummer)
			if !strings.Contains(haystack, qLower) {
				continue
			}
		}

		matched = append(matched, ref)
	}

	total := len(matched)

	// Pagination
	start := ac.Offset
	if start > len(matched) {
		start = len(matched)
	}
	end := start + ac.Limit
	if end > len(matched) {
		end = len(matched)
	}
	paged := matched[start:end]

	results := legalRefsToMaps(paged)

	// Summary of matched results
	contextSet := make(map[string]int)
	kgSet := make(map[string]bool)
	lawSet := make(map[string]bool)
	for _, ref := range matched {
		ctx := ref.LegalContext
		if ctx == "" {
			ctx = "unclassified"
		}
		contextSet[ctx]++
		kgSet[ref.KGCode] = true
		if ref.Gesetzesnummer != "" {
			lawSet[ref.Gesetzesnummer] = true
		}
	}

	meta := NewMeta(ac, total)
	apiResponse(w, r, map[string]interface{}{
		"query":            q,
		"total_refs":       total,
		"unique_kgs":       len(kgSet),
		"unique_laws":      len(lawSet),
		"by_legal_context": contextSet,
		"refs":             results,
	}, meta)
}

// ═══════════════════════════════════════════════════════════════════════════════
// Helpers
// ═══════════════════════════════════════════════════════════════════════════════

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// init check: make sure fmt is used (it's used in parcelLegalID indirectly but let's be safe)
var _ = fmt.Sprintf
