// kg_code.go — ONE canonical spelling for a Katastralgemeinde code.
//
// The wart this fixes: BEV KG codes are five digits and 740 of the 7 850 real
// codes start with a zero (`01503` Heiligenstadt, `07201` Aalfang). Everything
// that stores data uses the padded form — `kg.kg_code` in the search index, the
// `{kg}.json.gz` filenames, `parcel_id` prefixes (`01503-601/1`), footprint ids
// (`01503_fp_9`). But `data/edm_austria_lookups.json` carries an UNPADDED
// `kg_code` field for exactly those 740 KGs (its `kg_number` field keeps the
// zero), so `/api/v1/lookup?kg=01503` answered with `kg_code: "1503"` — and
// feeding that straight back into `/api/v1/query?kg=1503` matched zero parcels,
// silently, with HTTP 200. Same identifier, two formats, no error, no note in
// the docs.
//
// The fix has three parts:
//  1. the EDM loader pads on load, so /lookup only ever EMITS canonical codes
//     (see api_edm_lookup.go);
//  2. every KG code and KG-prefixed object id arriving in a query parameter is
//     canonicalised in apiMiddleware (api.go), so the un-padded form keeps
//     working instead of returning an empty result set;
//  3. handlers that parse ids out of the URL path canonicalise them here.
//
// Normalisation is deliberately narrow: only a 4-digit all-numeric string is
// padded. 1–3 digit inputs are left alone because they are prefixes, not codes
// (`/api/v1/lookup?q=63` is a legitimate typeahead query, and padding it to
// `00063` would break it).
package main

import (
	"net/http"
	"strings"
)

// kgCodeLen is the canonical width of a KG code.
const kgCodeLen = 5

// NormalizeKGCode returns the canonical 5-digit spelling of a KG code.
// A 4-digit numeric code is zero-padded ("1503" → "01503"). Anything else —
// already-canonical codes, names, prefixes, empty strings — is returned
// unchanged, so this is safe to apply to any user-supplied value.
func NormalizeKGCode(s string) string {
	if len(s) != kgCodeLen-1 || !isNumericCode(s) {
		return s
	}
	return "0" + s
}

// KGCodeWasNormalized reports whether NormalizeKGCode would change s.
func KGCodeWasNormalized(s string) bool { return NormalizeKGCode(s) != s }

// NormalizeKGCodeList canonicalises a comma/space separated list in place,
// preserving the original separators is not required — callers re-join with ",".
func NormalizeKGCodeList(s string) string {
	if s == "" {
		return s
	}
	parts := strings.Split(s, ",")
	changed := false
	for i, p := range parts {
		trimmed := strings.TrimSpace(p)
		n := NormalizeKGCode(trimmed)
		if n != p {
			parts[i] = n
			changed = true
		}
	}
	if !changed {
		return s
	}
	return strings.Join(parts, ",")
}

// NormalizeObjectID canonicalises the KG-code prefix of a cadastre object id:
//
//	parcel     "{kg}-{gnr}"   e.g. 1503-601/1   → 01503-601/1
//	footprint  "{kg}_fp_{n}"  e.g. 1503_fp_9    → 01503_fp_9
//	bare kg    "{kg}"         e.g. 1503         → 01503
//
// Ids whose prefix is not a 4-digit numeric run are returned unchanged.
func NormalizeObjectID(id string) string {
	if id == "" {
		return id
	}
	i := strings.IndexAny(id, "-_")
	if i < 0 {
		return NormalizeKGCode(id)
	}
	head := id[:i]
	n := NormalizeKGCode(head)
	if n == head {
		return id
	}
	return n + id[i:]
}

// NormalizeObjectIDs canonicalises a slice of object ids (batch endpoints).
func NormalizeObjectIDs(ids []string) []string {
	for i, id := range ids {
		ids[i] = NormalizeObjectID(strings.TrimSpace(id))
	}
	return ids
}

// ── Which query parameters carry a KG code / KG-prefixed id ─────────────────
//
// Kept as explicit sets rather than a heuristic so a new parameter can never be
// silently mangled (e.g. `plz=8580` is four numeric digits and must NOT be
// padded; `layers=kg` names a layer, not a code).

var kgCodeParams = map[string]bool{
	"kg":          true,
	"kg_code":     true,
	"kgs":         true,
	"kg_codes":    true,
	"kg_list":     true,
	"gemeinde_kg": true,
	"codes":       true, // /api/v1/kgs?codes= and /llm/kgs?codes= — KG code lists
}

var kgObjectIDParams = map[string]bool{
	"parcel":        true,
	"parcel_id":     true,
	"parcel_ids":    true,
	"footprint":     true,
	"footprint_id":  true,
	"footprint_ids": true,
	"ids":           true,
	"id":            true,
}

// ambiguousIDParams are parameters named generically enough that the SAME name
// carries a non-KG identifier on some endpoints — `?id=` is a parcel_id on
// /search/parcel but a Gemeindekennziffer on /search/municipalities. For these
// we only rewrite composite ids (those carrying a `-`/`_` separator, i.e.
// unambiguously `{kg}-{gnr}` / `{kg}_fp_{n}`) and never a bare numeric code.
var ambiguousIDParams = map[string]bool{"id": true, "ids": true} // NormalizeRequestParam canonicalises one query-parameter value if that
// parameter is known to carry KG codes or KG-prefixed ids. Returns the value
// unchanged for every other parameter.
func NormalizeRequestParam(name, value string) string {
	if value == "" {
		return value
	}
	switch {
	case kgCodeParams[name]:
		return NormalizeKGCodeList(value)
	case kgObjectIDParams[name]:
		norm := NormalizeObjectID
		if ambiguousIDParams[name] {
			norm = normalizeCompositeObjectID
		}
		if !strings.Contains(value, ",") {
			return norm(value)
		}
		parts := strings.Split(value, ",")
		for i, p := range parts {
			parts[i] = norm(strings.TrimSpace(p))
		}
		return strings.Join(parts, ",")
	}
	return value
}

// normalizeCompositeObjectID canonicalises only ids that carry a KG prefix plus
// a separator, leaving bare numeric codes untouched (see ambiguousIDParams).
func normalizeCompositeObjectID(id string) string {
	if !strings.ContainsAny(id, "-_") {
		return id
	}
	return NormalizeObjectID(id)
}

// ═══════════════════════════════════════════════════════════════════════════════
// Request rewriting
// ═══════════════════════════════════════════════════════════════════════════════

// kgNormalizedHeader is set on any response whose request carried a non-canonical
// KG code, so a client can notice it is sending the short form.
const kgNormalizedHeader = "X-KG-Code-Normalized"

// normalizeKGParams rewrites r.URL.RawQuery so every KG code / KG-prefixed id in
// the query string is in canonical 5-digit form before any handler sees it.
//
// Only touches parameters listed in kgCodeParams / kgObjectIDParams (kg_code.go),
// and only rewrites RawQuery when something actually changed — so the common case
// costs one Query() parse that handlers were going to do anyway.
//
// Path-embedded ids are NOT handled here: a bare 4-digit path segment is
// ambiguous (`/natura2000/parcel/12358/1234` — the second segment is a
// Grundstücksnummer, not a KG code), so those handlers call NormalizeObjectID /
// NormalizeKGCode on the specific segment that is a KG code.
func normalizeKGParams(r *http.Request, w http.ResponseWriter) {
	if r.URL.RawQuery == "" {
		return
	}
	q := r.URL.Query()
	changed := false
	for name, vals := range q {
		if !kgCodeParams[name] && !kgObjectIDParams[name] {
			continue
		}
		for i, v := range vals {
			n := NormalizeRequestParam(name, v)
			if n != v {
				vals[i] = n
				changed = true
			}
		}
	}
	if !changed {
		return
	}
	r.URL.RawQuery = q.Encode()
	if w != nil {
		w.Header().Set(kgNormalizedHeader, "true")
	}
}

// normalizeKGCodes canonicalises a slice of KG codes in place (CLI flag lists).
func normalizeKGCodes(codes []string) []string {
	for i, c := range codes {
		codes[i] = NormalizeKGCode(strings.TrimSpace(c))
	}
	return codes
}
