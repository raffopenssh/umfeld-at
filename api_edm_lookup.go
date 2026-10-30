// api_edm_lookup.go — EDM (Austrian federal register) lookup & autocomplete endpoint.
//
// Provides /api/v1/lookup — a fast, fuzzy-tolerant search across PLZ, municipality,
// Katastralgemeinde, and Ortschaft names. Designed for typeahead/autocomplete UIs
// and programmatic resolution of Austrian place names to structured codes.
//
// Data source: data/edm_austria_lookups.json (BEV EDM register)
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// ═══════════════════════════════════════════════════════════════════════════════
// Data structures
// ═══════════════════════════════════════════════════════════════════════════════

type edmEntry struct {
	Type         string   `json:"type"` // "plz", "gemeinde", "kg", "ortschaft"
	Code         string   `json:"code"` // primary code (canonical spelling)
	Name         string   `json:"name"` // display name
	PLZ          []string `json:"plz,omitempty"`
	GemeindeCode string   `json:"gemeinde_code,omitempty"`
	GemeindeName string   `json:"gemeinde_name,omitempty"`
	KGCode       string   `json:"kg_code,omitempty"`
	// KGNumber is the BEV Katastralgemeindenummer. It is always 5 digits and now
	// equals kg_code; it is exposed because the source register's own `kg_code`
	// field is the one that drops the leading zero, so `kg_number` is the field
	// that was always trustworthy.
	KGNumber   string `json:"kg_number,omitempty"`
	Location   string `json:"location,omitempty"`
	searchText string // lowercased, normalized — not exported
}

var edmEntries []edmEntry

// ═══════════════════════════════════════════════════════════════════════════════
// Loading
// ═══════════════════════════════════════════════════════════════════════════════

func loadEDMLookupData() {
	data, err := os.ReadFile("data/edm_austria_lookups.json")
	if err != nil {
		log.Printf("Warning: could not load EDM lookups: %v", err)
		return
	}

	var raw struct {
		Gemeinden          map[string]json.RawMessage `json:"gemeinden"`
		Katastralgemeinden map[string]json.RawMessage `json:"katastralgemeinden"`
		Ortschaften        map[string]json.RawMessage `json:"ortschaften"`
		PLZ                map[string]json.RawMessage `json:"plz"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		log.Printf("Warning: could not parse EDM lookups: %v", err)
		return
	}

	// Gemeinden
	for _, v := range raw.Gemeinden {
		var g struct {
			GemeindeCode string `json:"gemeinde_code"`
			GemeindeName string `json:"gemeinde_name"`
		}
		json.Unmarshal(v, &g)
		e := edmEntry{
			Type:         "gemeinde",
			Code:         g.GemeindeCode,
			Name:         g.GemeindeName,
			GemeindeCode: g.GemeindeCode,
			GemeindeName: g.GemeindeName,
		}
		e.searchText = normalizeSearch(g.GemeindeName + " " + g.GemeindeCode)
		edmEntries = append(edmEntries, e)
	}

	// Katastralgemeinden
	//
	// NOTE the padding. The register's `kg_code` field drops the leading zero for
	// 740 of the 7 850 KGs (`07201` is stored as `7201` — the sibling `kg_number`
	// field keeps the zero), while EVERY other part of this API — the search
	// index's `kg.kg_code`, `{kg}.json.gz` filenames, `parcel_id`/`footprint_id`
	// prefixes — uses the padded 5-digit form. Emitting the register's spelling
	// made /lookup hand out a code that /query?kg= then matched zero parcels with.
	// We canonicalise on load (kg_code.go) so /lookup only ever emits ids that work
	// verbatim elsewhere; the unpadded spelling stays in searchText so searching
	// "7201" still finds it.
	for _, v := range raw.Katastralgemeinden {
		var kg struct {
			KGNumber     string `json:"kg_number"`
			KGName       string `json:"kg_name"`
			KGCode       string `json:"kg_code"`
			GemeindeCode string `json:"gemeinde_code"`
			GemeindeName string `json:"gemeinde_name"`
		}
		json.Unmarshal(v, &kg)
		canonical := NormalizeKGCode(kg.KGCode)
		if canonical == "" {
			canonical = NormalizeKGCode(kg.KGNumber)
		}
		e := edmEntry{
			Type:         "kg",
			Code:         canonical,
			Name:         kg.KGName,
			KGCode:       canonical,
			KGNumber:     kg.KGNumber,
			GemeindeCode: kg.GemeindeCode,
			GemeindeName: kg.GemeindeName,
		}
		e.searchText = normalizeSearch(kg.KGName + " " + canonical + " " + kg.KGCode + " " + kg.KGNumber + " " + kg.GemeindeName + " " + kg.GemeindeCode)
		edmEntries = append(edmEntries, e)
	}

	// Ortschaften
	for _, v := range raw.Ortschaften {
		var o struct {
			OrtschaftCode string   `json:"ortschaft_code"`
			OrtschaftName string   `json:"ortschaft_name"`
			PLZ           []string `json:"plz"`
			GemeindeCode  string   `json:"gemeinde_code"`
			GemeindeName  string   `json:"gemeinde_name"`
		}
		json.Unmarshal(v, &o)
		e := edmEntry{
			Type:         "ortschaft",
			Code:         o.OrtschaftCode,
			Name:         o.OrtschaftName,
			PLZ:          o.PLZ,
			GemeindeCode: o.GemeindeCode,
			GemeindeName: o.GemeindeName,
		}
		e.searchText = normalizeSearch(o.OrtschaftName + " " + strings.Join(o.PLZ, " ") + " " + o.GemeindeCode + " " + o.GemeindeName)
		edmEntries = append(edmEntries, e)
	}

	// PLZ
	for plzCode, v := range raw.PLZ {
		var entries []struct {
			PLZ           string `json:"plz"`
			OrtschaftCode string `json:"ortschaft_code"`
			Location      string `json:"location"`
		}
		json.Unmarshal(v, &entries)
		// Deduplicate locations per PLZ
		locations := make([]string, 0, len(entries))
		seen := map[string]bool{}
		for _, ent := range entries {
			if !seen[ent.Location] {
				locations = append(locations, ent.Location)
				seen[ent.Location] = true
			}
		}
		locationStr := strings.Join(locations, "; ")
		name := plzCode
		if len(locations) > 0 {
			name = plzCode + " " + locations[0]
		}
		e := edmEntry{
			Type:     "plz",
			Code:     plzCode,
			Name:     name,
			PLZ:      []string{plzCode},
			Location: locationStr,
		}
		e.searchText = normalizeSearch(plzCode + " " + locationStr)
		edmEntries = append(edmEntries, e)
	}

	log.Printf("\U0001f4d6 EDM lookup loaded: %d entries (%d gemeinden, %d KGs, %d ortschaften, %d PLZ)",
		len(edmEntries),
		len(raw.Gemeinden), len(raw.Katastralgemeinden),
		len(raw.Ortschaften), len(raw.PLZ))
}

// ═══════════════════════════════════════════════════════════════════════════════
// Search helpers
// ═══════════════════════════════════════════════════════════════════════════════

// normalizeSearch strips diacritics and lowercases for fuzzy matching.
func normalizeSearch(s string) string {
	// NFD decompose, strip combining marks, then lowercase
	var b strings.Builder
	for _, r := range norm.NFD.String(s) {
		if !unicode.Is(unicode.Mn, r) { // Mn = nonspacing mark (combining diacritics)
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

type scoredEdm struct {
	entry *edmEntry
	score int // lower = better
}

func searchEDM(query string, filterType string, limit int) []edmEntry {
	q := normalizeSearch(query)
	if q == "" {
		return nil
	}

	tokens := strings.Fields(q)
	var results []scoredEdm

	for i := range edmEntries {
		e := &edmEntries[i]
		if filterType != "" && e.Type != filterType {
			continue
		}

		// All tokens must match
		allMatch := true
		totalPos := 0
		for _, tok := range tokens {
			pos := strings.Index(e.searchText, tok)
			if pos < 0 {
				allMatch = false
				break
			}
			totalPos += pos
		}
		if !allMatch {
			continue
		}

		// Score: prefer prefix matches, shorter names, and exact code matches
		score := totalPos * 2
		score += len(e.searchText) // prefer shorter/more specific results

		// Bonus for exact code match. Compared in canonical form so the
		// zero-stripped register spelling ("7201") and the padded API spelling
		// ("07201") both score as an exact hit.
		if e.Code == query || NormalizeKGCode(e.Code) == NormalizeKGCode(query) ||
			strings.TrimLeft(e.Code, "0") == strings.TrimLeft(query, "0") && isNumericCode(query) {
			score = -1000
		}
		// Bonus for name starting with query
		if strings.HasPrefix(e.searchText, q) {
			score -= 500
		}
		// Bonus for exact name match
		normName := normalizeSearch(e.Name)
		if normName == q {
			score -= 800
		}

		results = append(results, scoredEdm{entry: e, score: score})
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].score < results[j].score
	})

	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}

	out := make([]edmEntry, len(results))
	for i, r := range results {
		out[i] = *r.entry
	}
	return out
}

// ═══════════════════════════════════════════════════════════════════════════════
// HTTP handler
// ═══════════════════════════════════════════════════════════════════════════════

var handleV1Lookup http.HandlerFunc = notImplemented("lookup")

func init() {
	loadEDMLookupData()

	handleV1Lookup = func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		typ := r.URL.Query().Get("type") // optional: plz, gemeinde, kg, ortschaft
		limitStr := r.URL.Query().Get("limit")

		// Shorthand forms: ?kg=63349 == ?q=63349&type=kg. The natural-looking
		// shape used to 400 with "q parameter required", which is a whole class of
		// failed first calls for agents guessing the interface.
		if q == "" {
			for _, name := range []string{"kg", "plz", "gemeinde", "ortschaft"} {
				if v := r.URL.Query().Get(name); v != "" {
					q = v
					if typ == "" {
						typ = name
					}
					break
				}
			}
		}

		limit := 20
		if limitStr != "" {
			if n, err := strconv.Atoi(limitStr); err == nil && n > 0 && n <= 200 {
				limit = n
			}
		}

		if q == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error": "a search term is required",
				"usage": "/api/v1/lookup?q=<search>&type=plz|gemeinde|kg|ortschaft|toponym&limit=20",
				"shorthand": []string{
					"/api/v1/lookup?kg=63349",
					"/api/v1/lookup?plz=8580",
					"/api/v1/lookup?gemeinde=K\u00f6flach",
					"/api/v1/lookup?ortschaft=Piberegg",
				},
				"note": "kg=/plz=/gemeinde=/ortschaft= are shorthand for q=<value>&type=<param>",
			})
			return
		}

		// type=toponym → BEV DLM-Namen (farmsteads, Almen, Rieden, peaks…);
		// and when the register knows nothing under the name, fall through to it
		// so a caller never gets a silent 0 for a place that exists (feedback #17).
		if typ == "toponym" || typ == "toponyms" {
			ac := ParseAPIContext(r)
			ac.Limit = limit
			if !ToponymsReady() {
				toponymsUnavailable(w)
				return
			}
			tq, perr := parseToponymQuery(r)
			if perr != "" {
				apiError(w, perr, 400)
				return
			}
			tq.Q = q
			rows, total, err := SearchToponyms(tq, limit, 0)
			if err != nil {
				apiError(w, err.Error(), 500)
				return
			}
			for _, row := range rows {
				row["type"] = "toponym"
			}
			respondSearchMeta(w, r, ac, rows, total, "lookup_toponyms.csv", func(m *Meta) {
				m.License = toponymLicenseMeta()
				m.ToponymNote = toponymSearchNote(tq, total)
				m.QueryUsed = tq.QueryUsed
			})
			return
		}

		results := searchEDM(q, typ, limit)

		meta := map[string]interface{}{
			"total":  len(results),
			"limit":  limit,
			"query":  q,
			"filter": typ,
		}
		if len(results) == 0 && typ == "" && !isNumericCode(q) {
			if topo := toponymFallbackRows(q, limit); len(topo) > 0 {
				meta["total"] = len(topo)
				meta["source"] = "toponym"
				meta["note"] = "no PLZ/Gemeinde/KG/Ortschaft matched; these are BEV DLM-Namen toponyms (farmsteads, Almen, Rieden, terrain, waters). Use /api/v1/toponyms/search for filters (kg, gemeinde, near=lon,lat) and score semantics."
				meta["license"] = toponymLicenseMeta()
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Access-Control-Allow-Origin", "*")
				writeJSON(w, 200, map[string]interface{}{"data": topo, "meta": meta})
				return
			}
		}
		// Say it out loud: the `kg_code` we return is the 5-digit zero-padded form
		// and can be pasted verbatim into every other endpoint. It used to be the
		// register's unpadded spelling for 740 KGs, which then matched nothing in
		// /api/v1/query?kg=.
		for _, e := range results {
			if e.Type == "kg" {
				meta["kg_code_format"] = "5-digit zero-padded (canonical); use verbatim in ?kg=, parcel_id and footprint_id. Both spellings are accepted on input."
				break
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"data": results,
			"meta": meta,
		})
	}
}
