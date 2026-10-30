// toponyms.go — BEV DLM-Namen: Austria's official geographic-names register
// as a queryable layer (feedback #17).
//
// Source: "Digitales Landschaftsmodell – Geographische Namen" (DLM 7000 NAMEN),
// 233k named POINTS in 7 layers — settlements down to single farmsteads and
// Alm huts (Einzelhäuser), areas, mountains/passes/valleys, glaciers, waters,
// other named objects (huts, chapels, castles) and 104k Riednamen (cadastral
// field names) that BEV already keys by KG_NUMMER. CC BY 4.0, same producer
// and attribution family as the parcels.
//
// Why here: the integration contract tells siblings "resolve every name through
// ONE canonical lookup, never fuzzy-match locally". /lookup knew Gemeinden, KGs,
// Ortschaften and addresses — not Almen, farms, Rieden, peaks. A CAP-subsidy
// consumer needs "AGRARGEM. LIEGNITZALM" → a point → parcels, and every other
// sibling would otherwise download the 58 MB GPKG and geocode itself.
//
// Design (mirrors osm_proximity.db / landuse_truth.db):
//
//   - OWN database `data/toponyms.db`, rebuilt whole by `-toponyms-import`,
//     swapped in atomically. Zero contention with search_index.db's single
//     writer; deleting it just disables the feature.
//   - Native EPSG:31255 → WGS84 reprojection (mgi_transform.go), no GDAL.
//   - KG assignment: RIED rows carry BEV's KG_NUMMER (authoritative,
//     kg_code_source="bev"). Every other layer is assigned by exact
//     point-in-parcel against the parcel_geom cache where that KG is warm
//     ("pip"), else by the majority KG of the nearest parcel centroids
//     ("nearest_parcel"). Both are disclosed per row.
//   - The FAST PATH is an in-memory trigram index over ~250k normalised
//     name strings (name, variant, minority-language names and the
//     generic-suffix-stripped stem). A search touches no SQLite connection
//     until it fetches the final page by rowid, so it cannot compete with the
//     parcel pool (Latency Contract rule 3/5). Fuzzy by construction:
//     "Liegnitzalm" (as written on a subsidy recipient) finds "Lignitzalpe" —
//     the register has no "Liegnitz" at all.
//   - R*Tree on lon/lat in SQLite for near / bbox / point queries.
//
// Coordinates are BEV's: ±50 m positional accuracy, NOT parcel-precise. A
// toponym is never snapped to a parcel; /toponyms/{id} returns the parcel as a
// separate object with the same pending/matched semantics as /spatial/points.
package main

import (
	"database/sql"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/encoding/wkb"
	"github.com/paulmach/orb/planar"
	"golang.org/x/text/unicode/norm"
)

const (
	toponymsDBPath        = "data/toponyms.db"
	toponymsSchemaVersion = "1"
	// toponymsDefaultSource is the Stichtag the spec was verified against. A
	// newer file can be passed with -toponyms-gpkg / -toponyms-url.
	toponymsDefaultURL      = "https://data.bev.gv.at/download/DLM/DLM_20250325/DLM_7000_NAMEN_20250325.gpkg"
	toponymsDefaultGPKG     = "data/bev_namen/DLM_7000_NAMEN_20250325.gpkg"
	toponymsReloadInterval  = 10 * time.Minute
	toponymsPositionalAccM  = 50 // BEV-stated positional accuracy
	toponymsDefaultMinScore = 0.55
	toponymsNearDefaultM    = 500.0
	toponymsNearMaxM        = 10000.0
	toponymsMaxCandidates   = 20000
)

// toponymLayers maps GPKG table → API layer name.
var toponymLayers = []struct {
	table, layer string
}{
	{"NAM_7100_SIEDLUNG_P", "siedlung"},
	{"NAM_7200_GEBIET_P", "gebiet"},
	{"NAM_7300_GELAENDEFORM_P", "gelaende"},
	{"NAM_7400_GLETSCHER_P", "gletscher"},
	{"NAM_7500_GEWAESSER_P", "gewaesser"},
	{"NAM_7600_SONSTIGE_P", "sonstige"},
	{"NAM_7700_RIED_P", "ried"},
}

var toponymLayerIndex = map[string]uint8{
	"siedlung": 0, "gebiet": 1, "gelaende": 2, "gletscher": 3, "gewaesser": 4, "sonstige": 5, "ried": 6,
}
var toponymLayerNames = []string{"siedlung", "gebiet", "gelaende", "gletscher", "gewaesser", "sonstige", "ried"}

// toponymLayerAliases accepts the German/English spellings agents send.
func canonicalToponymLayer(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "siedlung", "siedlungen", "settlement", "settlements", "place", "places":
		return "siedlung"
	case "gebiet", "gebiete", "area", "areas", "region":
		return "gebiet"
	case "gelaende", "gelände", "gelaendeform", "geländeform", "terrain", "relief", "berg", "mountain", "mountains":
		return "gelaende"
	case "gletscher", "glacier", "glaciers":
		return "gletscher"
	case "gewaesser", "gewässer", "water", "waters", "hydro":
		return "gewaesser"
	case "sonstige", "sonstiges", "other", "objects", "objekt":
		return "sonstige"
	case "ried", "ried_p", "riede", "rieden", "riedname", "riednamen", "field", "fields", "flurname", "flurnamen":
		return "ried"
	}
	return strings.ToLower(strings.TrimSpace(s))
}

// ─── state ──────────────────────────────────────────────────────────────────

var (
	topoMu       sync.RWMutex
	topoDB       *sql.DB
	topoReady    bool
	topoMeta     map[string]string
	topoIdx      *toponymIndex
	topoOpenedAt time.Time
	topoFileMod  time.Time
	topoOnce     sync.Once
)

// toponymEntry is the in-memory projection of one row: enough to filter and
// rank without a DB round-trip. ~40 B/row → ~10 MB for the country.
type toponymEntry struct {
	rowid     uint32
	layer     uint8
	fcode     uint16
	abandoned bool
	kg        uint16 // index into idx.kgs (0 = none)
	lon, lat  float32
}

type toponymKG struct {
	code, gemeindeCode, districtCode, stateCode, stateName string
}

// toponymName is one searchable string (name / variant / minority / stem)
// pointing back at its entry.
type toponymName struct {
	norm  string
	entry uint32
	kind  uint8 // 0 name, 1 variant, 2 minority-language, 3 stem
	ntri  uint8
}

type toponymIndex struct {
	entries  []toponymEntry
	names    []toponymName
	tri      map[[3]byte][]uint32 // trigram → indexes into names
	kgs      []toponymKG
	kgByCode map[string]uint16
	byLayer  [7]int
	byKG     map[string]int
}

// EnsureToponyms opens data/toponyms.db if present and builds the in-memory
// index in the background. Safe to call from any startup path; the API answers
// 503 with a hint until ready.
func EnsureToponyms() {
	topoOnce.Do(func() {
		go func() {
			loadToponyms()
			t := time.NewTicker(toponymsReloadInterval)
			for range t.C {
				// A -toponyms-import in ANOTHER process swaps the file; pick it up.
				if st, err := os.Stat(toponymsDBPath); err == nil {
					topoMu.RLock()
					changed := !st.ModTime().Equal(topoFileMod)
					topoMu.RUnlock()
					if changed {
						log.Printf("🏔  toponyms.db changed on disk — reloading")
						loadToponyms()
					}
				}
			}
		}()
	})
}

// ToponymsReady reports whether the layer can answer.
func ToponymsReady() bool {
	topoMu.RLock()
	defer topoMu.RUnlock()
	return topoReady
}

func openToponymsDB(path string, rw bool) (*sql.DB, error) {
	mode := "ro"
	if rw {
		mode = "rwc"
	}
	dsn := fmt.Sprintf("file:%s?mode=%s&_journal_mode=DELETE&_synchronous=NORMAL&_busy_timeout=15000&_cache_size=-32000", path, mode)
	db, err := sql.Open(sqliteTunedDriver, dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	return db, nil
}

func loadToponyms() {
	st, err := os.Stat(toponymsDBPath)
	if err != nil {
		log.Printf("🏔  toponyms: %s not present — run `./umfeld-at -toponyms-import` (feedback #17)", toponymsDBPath)
		return
	}
	db, err := openToponymsDB(toponymsDBPath, false)
	if err != nil {
		log.Printf("🏔  toponyms: open: %v", err)
		return
	}
	meta := map[string]string{}
	rows, err := db.Query(`SELECT key, value FROM toponyms_meta`)
	if err != nil {
		log.Printf("🏔  toponyms: meta: %v (rebuild with -toponyms-import)", err)
		db.Close()
		return
	}
	for rows.Next() {
		var k, v string
		if rows.Scan(&k, &v) == nil {
			meta[k] = v
		}
	}
	rows.Close()
	if meta["schema_version"] != toponymsSchemaVersion {
		log.Printf("🏔  toponyms: schema %q ≠ %q — rebuild with -toponyms-import", meta["schema_version"], toponymsSchemaVersion)
		db.Close()
		return
	}
	t0 := time.Now()
	idx, err := buildToponymIndex(db)
	if err != nil {
		log.Printf("🏔  toponyms: index: %v", err)
		db.Close()
		return
	}
	topoMu.Lock()
	old := topoDB
	topoDB = db
	topoMeta = meta
	topoIdx = idx
	topoReady = true
	topoOpenedAt = time.Now()
	topoFileMod = st.ModTime()
	topoMu.Unlock()
	if old != nil {
		// Give in-flight statements a moment before closing the old handle.
		go func() { time.Sleep(30 * time.Second); old.Close() }()
	}
	gemeindeKGSetByExactName("") // pre-build the Gemeinde-name map off the request path
	log.Printf("🏔  toponyms: %d names (%d search strings, %d trigrams) from Stichtag %s in %s",
		len(idx.entries), len(idx.names), len(idx.tri), meta["stichtag"], time.Since(t0).Round(time.Millisecond))
}

func buildToponymIndex(db *sql.DB) (*toponymIndex, error) {
	idx := &toponymIndex{
		tri:      make(map[[3]byte][]uint32, 40000),
		kgByCode: map[string]uint16{},
		byKG:     map[string]int{},
	}
	idx.kgs = append(idx.kgs, toponymKG{}) // 0 = none
	rows, err := db.Query(`SELECT rowid, layer, f_code, abandoned, COALESCE(kg_code,''), COALESCE(gemeinde_code,''),
		COALESCE(district_code,''), COALESCE(state_code,''), COALESCE(state_name,''), lon, lat,
		name_norm, COALESCE(name_stem,''), COALESCE(variant_name,''), COALESCE(name_kroat,''), COALESCE(name_slow,''), COALESCE(name_ung,'')
		FROM toponyms ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	idx.entries = make([]toponymEntry, 0, 240000)
	idx.names = make([]toponymName, 0, 500000)
	for rows.Next() {
		var rowid int64
		var layer, kg, gem, dist, stc, stn, nn, stem, variant, kro, slo, ung string
		var fcode, abandoned int
		var lon, lat float64
		if err := rows.Scan(&rowid, &layer, &fcode, &abandoned, &kg, &gem, &dist, &stc, &stn, &lon, &lat,
			&nn, &stem, &variant, &kro, &slo, &ung); err != nil {
			return nil, err
		}
		li, ok := toponymLayerIndex[layer]
		if !ok {
			continue
		}
		var kgi uint16
		if kg != "" {
			if i, ok := idx.kgByCode[kg]; ok {
				kgi = i
			} else {
				kgi = uint16(len(idx.kgs))
				idx.kgs = append(idx.kgs, toponymKG{kg, gem, dist, stc, stn})
				idx.kgByCode[kg] = kgi
			}
			idx.byKG[kg]++
		}
		ei := uint32(len(idx.entries))
		idx.entries = append(idx.entries, toponymEntry{
			rowid: uint32(rowid), layer: li, fcode: uint16(fcode), abandoned: abandoned != 0,
			kg: kgi, lon: float32(lon), lat: float32(lat),
		})
		idx.byLayer[li]++
		idx.addName(nn, ei, 0)
		if stem != "" && stem != nn {
			idx.addName(stem, ei, 3)
		}
		if variant != "" {
			idx.addName(normalizeToponym(variant), ei, 1)
		}
		for _, m := range []string{kro, slo, ung} {
			if m != "" {
				idx.addName(normalizeToponym(m), ei, 2)
			}
		}
	}
	return idx, rows.Err()
}

func (idx *toponymIndex) addName(norm string, entry uint32, kind uint8) {
	if norm == "" {
		return
	}
	tris := trigramsOf(norm)
	ni := uint32(len(idx.names))
	n := len(tris)
	if n > 255 {
		n = 255
	}
	idx.names = append(idx.names, toponymName{norm: norm, entry: entry, kind: kind, ntri: uint8(n)})
	for t := range tris {
		idx.tri[t] = append(idx.tri[t], ni)
	}
}

// ─── name normalisation ─────────────────────────────────────────────────────

var topoNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// normalizeToponym lower-cases, folds German umlauts to their digraphs
// (ä→ae, ö→oe, ü→ue, ß→ss — the spelling used on subsidy registers and old
// maps), strips remaining diacritics (Slovene č/š/ž), and collapses every
// run of punctuation/whitespace into one space.
func normalizeToponym(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 4)
	for _, r := range strings.ToLower(s) {
		switch r {
		case 'ä':
			b.WriteString("ae")
		case 'ö':
			b.WriteString("oe")
		case 'ü':
			b.WriteString("ue")
		case 'ß':
			b.WriteString("ss")
		default:
			b.WriteRune(r)
		}
	}
	// strip remaining diacritics
	var c strings.Builder
	for _, r := range norm.NFD.String(b.String()) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		c.WriteRune(r)
	}
	out := topoNonAlnum.ReplaceAllString(c.String(), " ")
	return strings.TrimSpace(out)
}

// Generic toponym suffixes, longest first so "almen" wins over "alm".
var topoGenericSuffixes = []string{
	"huetten", "huette", "haeuser", "alpen", "almen", "alpe", "alpl", "alm", "alp",
	"hoefe", "hof", "gut", "muehle", "muehl", "wiesen", "wiese", "aecker", "acker",
	"felder", "feld", "wald", "waelder", "berg", "kogel", "kogl", "spitze", "spitz",
	"kopf", "graben", "bach", "see", "tal", "boden", "leiten", "leite", "moos",
	"bichl", "buehel", "eck", "egg", "au", "hals", "sattel", "joch", "scharte",
	"kar", "hoehe", "hoeh", "wand", "stein", "riegel", "bauer", "gasse", "weg",
}

// Standalone generic words dropped when they are the last token.
var topoGenericWords = map[string]bool{
	"alm": true, "alpe": true, "almen": true, "alpen": true, "huette": true, "hof": true,
	"gut": true, "hoehe": true, "berg": true, "kogel": true, "spitze": true, "see": true, "bach": true,
	"tal": true, "wald": true, "wiese": true, "feld": true, "moos": true, "au": true, "kar": true,
	"joch": true, "sattel": true, "scharte": true, "graben": true, "leiten": true, "boden": true,
	"hoch": true, "ober": true, "unter": true, "vorder": true, "hinter": true, "vordere": true, "hintere": true,
	"obere": true, "untere": true, "gross": true, "klein": true, "grosse": true, "kleine": true,
}

// stemToponym removes the generic suffix so "Lignitzalpe", "Lignitz Alm" and
// "Lignitzer Alm" all stem to "lignitz" and a misspelt "Liegnitzalm" still
// lands. Returns "" when nothing specific is left (a name that IS just "Alm").
func stemToponym(norm string) string {
	if norm == "" {
		return ""
	}
	words := strings.Fields(norm)
	// drop generic leading qualifiers (hintere, obere …) and trailing generic word
	for len(words) > 1 && topoGenericWords[words[0]] {
		words = words[1:]
	}
	for len(words) > 1 && topoGenericWords[words[len(words)-1]] {
		words = words[:len(words)-1]
	}
	last := words[len(words)-1]
	for _, suf := range topoGenericSuffixes {
		if len(last) > len(suf)+2 && strings.HasSuffix(last, suf) {
			last = strings.TrimSuffix(last, suf)
			break
		}
	}
	// adjectival "-er" (lignitzer → lignitz, grazer → graz)
	if len(last) > 6 && strings.HasSuffix(last, "er") {
		last = strings.TrimSuffix(last, "er")
	}
	words[len(words)-1] = last
	out := strings.Join(words, " ")
	// A stem must still be specific: ≥4 chars and not itself a generic word
	// ("Großau" → "gross" would match every "Groß…" in the country).
	if out == norm || len(out) < 4 || topoGenericWords[out] {
		return ""
	}
	return out
}

// trigramsOf returns the padded trigram set of a normalised string.
func trigramsOf(s string) map[[3]byte]struct{} {
	padded := "  " + s + " "
	out := make(map[[3]byte]struct{}, len(padded))
	for i := 0; i+3 <= len(padded); i++ {
		var t [3]byte
		copy(t[:], padded[i:i+3])
		out[t] = struct{}{}
	}
	return out
}

// diceTrigram is the string similarity used for scoring (0..1).
func diceTrigram(a, b string) float64 {
	ta, tb := trigramsOf(a), trigramsOf(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	common := 0
	for t := range ta {
		if _, ok := tb[t]; ok {
			common++
		}
	}
	return 2 * float64(common) / float64(len(ta)+len(tb))
}

// ─── search (in-memory fast path) ───────────────────────────────────────────

// ToponymQuery is the parsed filter set shared by search / near / bbox / kg.
type ToponymQuery struct {
	Q         string
	Layers    map[string]bool
	FCodes    map[int]bool
	KG        map[string]bool
	Gemeinde  map[string]bool
	District  map[string]bool
	State     string // code or name, normalised lower
	NearLon   float64
	NearLat   float64
	RadiusM   float64 // >0 enables distance filter
	HasNear   bool
	Stem      bool
	MinScore  float64
	Abandoned *bool
	BBox      *[4]float64 // west,south,east,north
	// QueryUsed is set when a fallback rewrite of Q produced the hits.
	QueryUsed string
}

// toponymFallback is one rewrite of the query: a shorter q and, when a token
// named a Gemeinde ("Hochalm Kramsach"), that Gemeinde's KG set as a filter.
type toponymFallback struct {
	q    string
	kgs  map[string]bool
	note string
}

// toponymFallbackQueries returns progressively looser rewrites of q:
//  1. organisational stopwords removed; a token that is exactly a Gemeinde
//     name becomes a gemeinde filter instead of a search term
//  2. the single longest (most specific) remaining token
func toponymFallbackQueries(q string) []toponymFallback {
	words := strings.Fields(normalizeToponym(q))
	if len(words) < 2 {
		return nil
	}
	var kept []string
	var kgs map[string]bool
	var gemNote string
	for _, w := range words {
		if topoOrgStopwords[w] || len(w) < 3 {
			continue
		}
		if len(w) >= 4 && kgs == nil {
			if set := gemeindeKGSetByExactName(w); set != nil {
				kgs = set
				gemNote = w
				continue
			}
		}
		kept = append(kept, w)
	}
	var out []toponymFallback
	if len(kept) > 0 && (len(kept) < len(words) || kgs != nil) {
		out = append(out, toponymFallback{q: strings.Join(kept, " "), kgs: kgs, note: gemNote})
	}
	if len(kept) > 1 {
		longest := kept[0]
		for _, w := range kept[1:] {
			if len(w) > len(longest) {
				longest = w
			}
		}
		out = append(out, toponymFallback{q: longest, kgs: kgs, note: gemNote})
	}
	return out
}

// gemeindeKGSetByExactName resolves a normalised single-word Gemeinde name
// ("kramsach") to its KG codes; nil when no Gemeinde has exactly that name.
func gemeindeKGSetByExactName(w string) map[string]bool {
	topoGemOnce.Do(func() {
		m := map[string]map[string]bool{}
		for _, r := range kgNameSnapshot() {
			n := normalizeToponym(r.gemeindeName)
			if n == "" {
				continue
			}
			if m[n] == nil {
				m[n] = map[string]bool{}
			}
			m[n][r.code] = true
		}
		topoGemByNorm = m
	})
	return topoGemByNorm[w]
}

var (
	topoGemOnce   sync.Once
	topoGemByNorm map[string]map[string]bool
)

type toponymHit struct {
	entry    *toponymEntry
	score    float64
	distance float64
	matched  string // which string matched: name|variant|minority|stem
}

func (idx *toponymIndex) passes(e *toponymEntry, q *ToponymQuery) (dist float64, ok bool) {
	if len(q.Layers) > 0 && !q.Layers[toponymLayerNames[e.layer]] {
		return 0, false
	}
	if len(q.FCodes) > 0 && !q.FCodes[int(e.fcode)] {
		return 0, false
	}
	if q.Abandoned != nil && e.abandoned != *q.Abandoned {
		return 0, false
	}
	if len(q.KG) > 0 || len(q.Gemeinde) > 0 || len(q.District) > 0 || q.State != "" {
		k := idx.kgs[e.kg]
		if len(q.KG) > 0 && !q.KG[k.code] {
			return 0, false
		}
		if len(q.Gemeinde) > 0 && !q.Gemeinde[k.gemeindeCode] {
			return 0, false
		}
		if len(q.District) > 0 && !q.District[k.districtCode] {
			return 0, false
		}
		if q.State != "" && strings.ToLower(k.stateCode) != q.State && normalizeToponym(k.stateName) != q.State {
			return 0, false
		}
	}
	if q.BBox != nil {
		b := q.BBox
		lon, lat := float64(e.lon), float64(e.lat)
		if lon < b[0] || lon > b[2] || lat < b[1] || lat > b[3] {
			return 0, false
		}
	}
	if q.HasNear {
		dist = haversine(q.NearLat, q.NearLon, float64(e.lat), float64(e.lon))
		if q.RadiusM > 0 && dist > q.RadiusM {
			return 0, false
		}
	}
	return dist, true
}

// search ranks names against q.Q. Candidate generation is the trigram
// posting-list walk (no SQL, no per-candidate allocation): because addName
// indexes each name's DISTINCT trigram set, the posting count c IS the
// number of shared trigrams, so Dice = 2c/(|q|+|name|) falls out of the walk
// for free. Only the string boosts (exact/prefix/whole-word) touch the name.
//
// Two passes when stemming is on: the literal query and its generic-suffix-
// stripped stem ("liegnitzalm" → "liegnitz"). Stem-side evidence is capped
// below literal evidence so a real "Grünalm" always outranks a "Grünberg"
// that merely shares the stem.
func (idx *toponymIndex) search(q *ToponymQuery) []toponymHit {
	qn := normalizeToponym(q.Q)
	if qn == "" {
		return nil
	}
	qStem := ""
	if q.Stem {
		qStem = stemToponym(qn)
		if qStem == qn {
			qStem = ""
		}
	}

	nNames := len(idx.names)
	counts := make([]uint8, nNames)
	touched := make([]uint32, 0, 8192)
	walk := func(tris map[[3]byte]struct{}, counts []uint8) {
		for t := range tris {
			for _, ni := range idx.tri[t] {
				if counts[ni] == 0 {
					touched = append(touched, ni)
				}
				if counts[ni] < 255 {
					counts[ni]++
				}
			}
		}
	}
	qTris := trigramsOf(qn)
	nq := len(qTris)
	walk(qTris, counts)

	var counts2 []uint8
	nqs := 0
	if qStem != "" {
		qsTris := trigramsOf(qStem)
		nqs = len(qsTris)
		counts2 = make([]uint8, nNames)
		walk(qsTris, counts2)
	}

	// Short queries (<3 chars): trigram sets are just padding; prefix scan.
	if len(qn) < 3 {
		touched = touched[:0]
		for i := range idx.names {
			if strings.HasPrefix(idx.names[i].norm, qn) {
				touched = append(touched, uint32(i))
				counts[i] = 1
			}
		}
	}

	best := make(map[uint32]*toponymHit, 1024)
	seen := make(map[uint32]struct{}, len(touched))
	for _, ni := range touched {
		if _, dup := seen[ni]; dup {
			continue
		}
		seen[ni] = struct{}{}
		n := &idx.names[ni]
		c := int(counts[ni])
		c2 := 0
		if counts2 != nil {
			c2 = int(counts2[ni])
		}
		// Upper bound: even with every boost a stem-only hit cannot exceed 0.9,
		// and Dice on the literal query is 2c/(nq+ntri). Skip hopeless names.
		lit := 2 * float64(c) / float64(nq+int(n.ntri))
		stm := 0.0
		if c2 > 0 {
			stm = 2 * float64(c2) / float64(nqs+int(n.ntri))
		}
		if lit < q.MinScore*0.7 && stm < q.MinScore*0.7 && len(qn) >= 3 {
			continue
		}
		score := scoreToponym(qn, qStem, n, lit, stm)
		if score < q.MinScore {
			continue
		}
		e := &idx.entries[n.entry]
		dist, ok := idx.passes(e, q)
		if !ok {
			continue
		}
		if h, ok := best[n.entry]; ok {
			if score > h.score {
				h.score = score
				h.matched = toponymKindName(n.kind)
			}
			continue
		}
		best[n.entry] = &toponymHit{entry: e, score: score, distance: dist, matched: toponymKindName(n.kind)}
		if len(best) > toponymsMaxCandidates {
			break
		}
	}
	hits := make([]toponymHit, 0, len(best))
	for _, h := range best {
		hits = append(hits, *h)
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		if q.HasNear && hits[i].distance != hits[j].distance {
			return hits[i].distance < hits[j].distance
		}
		if hits[i].entry.layer != hits[j].entry.layer {
			return hits[i].entry.layer < hits[j].entry.layer
		}
		return hits[i].entry.rowid < hits[j].entry.rowid
	})
	return hits
}

func toponymKindName(k uint8) string {
	switch k {
	case 1:
		return "variant_name"
	case 2:
		return "minority_name"
	case 3:
		return "name_stem"
	}
	return "name"
}

// scoreToponym combines the trigram Dice values with string boosts.
//
//	literal query vs literal name : exact 1.0, prefix ≤0.92, whole word 0.85, substring 0.75, else Dice
//	literal query vs name STEM    : exact 0.90, prefix 0.85, else Dice×0.95   (kind 3)
//	query STEM  vs literal name   : exact 0.90, prefix 0.85, else Dice×0.95
//	query STEM  vs name STEM      : exact 0.80,              else Dice×0.90   (weakest evidence)
func scoreToponym(qn, qStem string, n *toponymName, lit, stm float64) float64 {
	norm := n.norm
	isStem := n.kind == 3
	var s float64
	if isStem {
		s = lit * 0.95
		if norm == qn {
			s = 0.90
		} else if strings.HasPrefix(norm, qn) {
			s = math.Max(s, 0.85)
		} else if len(norm) >= 5 && len(norm)*10 >= len(qn)*6 && strings.HasPrefix(qn, norm) {
			// the query is "<stem><suffix>" — only when the stem carries most of it
			s = math.Max(s, 0.85)
		}
		if qStem != "" {
			t := stm * 0.90
			if norm == qStem {
				t = 0.80
			}
			s = math.Max(s, t)
		}
	} else {
		s = lit
		if norm == qn {
			s = 1.0
		} else if strings.HasPrefix(norm, qn) {
			s = math.Max(s, 0.92-0.02*math.Min(5, float64(len(norm)-len(qn))/4))
		} else if strings.Contains(" "+norm+" ", " "+qn+" ") {
			s = math.Max(s, 0.85)
		} else if len(qn) >= 4 && strings.Contains(norm, qn) {
			s = math.Max(s, 0.75)
		}
		if qStem != "" {
			t := stm * 0.95
			if norm == qStem {
				t = 0.90
			} else if strings.HasPrefix(norm, qStem) {
				t = math.Max(t, 0.85)
			}
			s = math.Max(s, t)
		}
	}
	return math.Round(s*1000) / 1000
}

// filter returns every entry passing q's filters (no name ranking), sorted by
// distance when near is set, else by layer/rowid. Used by near / bbox / kg.
func (idx *toponymIndex) filter(q *ToponymQuery, cap int) []toponymHit {
	var hits []toponymHit
	// Bbox pre-pass keeps the scan cheap: compare 2 floats before anything else.
	for i := range idx.entries {
		e := &idx.entries[i]
		if q.BBox != nil {
			b := q.BBox
			if float64(e.lon) < b[0] || float64(e.lon) > b[2] || float64(e.lat) < b[1] || float64(e.lat) > b[3] {
				continue
			}
		}
		dist, ok := idx.passes(e, q)
		if !ok {
			continue
		}
		hits = append(hits, toponymHit{entry: e, distance: dist, score: 1})
		if cap > 0 && len(hits) >= cap && !q.HasNear {
			break
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if q.HasNear && hits[i].distance != hits[j].distance {
			return hits[i].distance < hits[j].distance
		}
		if hits[i].entry.layer != hits[j].entry.layer {
			return hits[i].entry.layer < hits[j].entry.layer
		}
		return hits[i].entry.rowid < hits[j].entry.rowid
	})
	return hits
}

// ─── row fetch ──────────────────────────────────────────────────────────────

const toponymRowCols = `rowid, id, layer, f_code, COALESCE(f_name,''), name, COALESCE(variant_name,''),
	COALESCE(name_kroat,''), COALESCE(name_slow,''), COALESCE(name_ung,''), COALESCE(objektart,''), COALESCE(art,''),
	COALESCE(bedeutung,''), elevation_m, okz, COALESCE(einwohner_klasse,''), abandoned,
	COALESCE(kg_code,''), COALESCE(kg_code_source,''), COALESCE(kg_name,''), COALESCE(gemeinde_code,''), COALESCE(gemeinde_name,''),
	COALESCE(district_code,''), COALESCE(district_name,''), COALESCE(state_code,''), COALESCE(state_name,''),
	lon, lat, COALESCE(source_indikator,''), lfd_nummer, COALESCE(erstell_dat,''), COALESCE(bearb_dat,''),
	COALESCE(stichtag,''), currency_year, COALESCE(name_norm,''), COALESCE(name_stem,'')`

func scanToponymRow(rows interface{ Scan(...interface{}) error }) (map[string]interface{}, uint32, error) {
	var rowid int64
	var id, layer, fname, name, variant, kro, slo, ung, objektart, art, bedeutung, ek string
	var kg, kgSrc, kgName, gem, gemName, dist, distName, stc, stn, indik, erstell, bearb, stichtag, nn, stem string
	var fcode, abandoned int
	var elev, okz, lfd, curYear sql.NullInt64
	var lon, lat float64
	if err := rows.Scan(&rowid, &id, &layer, &fcode, &fname, &name, &variant, &kro, &slo, &ung, &objektart, &art,
		&bedeutung, &elev, &okz, &ek, &abandoned, &kg, &kgSrc, &kgName, &gem, &gemName, &dist, &distName, &stc, &stn,
		&lon, &lat, &indik, &lfd, &erstell, &bearb, &stichtag, &curYear, &nn, &stem); err != nil {
		return nil, 0, err
	}
	m := map[string]interface{}{
		"id":     id,
		"layer":  layer,
		"f_code": fcode,
		"f_name": fname,
		"name":   name,
		"lon":    round7(lon),
		"lat":    round7(lat),
	}
	if variant != "" {
		m["variant_name"] = variant
	}
	if kro != "" {
		m["name_kroat"] = kro
	}
	if slo != "" {
		m["name_slow"] = slo
	}
	if ung != "" {
		m["name_ung"] = ung
	}
	if objektart != "" {
		m["objektart"] = objektart
	}
	if art != "" {
		m["art"] = art
	}
	if bedeutung != "" {
		m["bedeutung"] = bedeutung
	}
	if elev.Valid {
		m["elevation_m"] = elev.Int64
	}
	if okz.Valid {
		m["okz"] = okz.Int64
	}
	if ek != "" {
		m["einwohner_klasse"] = ek
	}
	if abandoned != 0 {
		m["abandoned"] = true
	}
	if kg != "" {
		m["kg_code"] = kg
		m["kg_code_source"] = kgSrc
		m["kg_name"] = kgName
		m["gemeinde_code"] = gem
		m["gemeinde_name"] = gemName
		m["district_code"] = dist
		m["district_name"] = distName
		m["state_code"] = stc
		m["state_name"] = stn
	} else {
		m["kg_code"] = nil
		m["kg_code_source"] = kgSrc
	}
	if indik != "" {
		m["source_indikator"] = indik
	}
	if lfd.Valid {
		m["lfd_nummer"] = lfd.Int64
	}
	m["erstell_dat"] = erstell
	m["bearb_dat"] = bearb
	m["stichtag"] = stichtag
	if curYear.Valid {
		m["currency_year"] = curYear.Int64
	}
	m["name_norm"] = nn
	if stem != "" {
		m["name_stem"] = stem
	}
	return m, uint32(rowid), nil
}

// fetchToponymRows loads full rows for the given hits, preserving order and
// attaching score / distance_m.
func fetchToponymRows(db *sql.DB, hits []toponymHit, withScore bool, withDist bool) ([]map[string]interface{}, error) {
	if len(hits) == 0 {
		return []map[string]interface{}{}, nil
	}
	ids := make([]string, len(hits))
	pos := make(map[uint32]int, len(hits))
	for i, h := range hits {
		ids[i] = strconv.Itoa(int(h.entry.rowid))
		pos[h.entry.rowid] = i
	}
	out := make([]map[string]interface{}, len(hits))
	// chunk to stay under SQLite's variable/expression limits
	for start := 0; start < len(ids); start += 500 {
		end := start + 500
		if end > len(ids) {
			end = len(ids)
		}
		rows, err := db.Query(`SELECT ` + toponymRowCols + ` FROM toponyms WHERE rowid IN (` + strings.Join(ids[start:end], ",") + `)`)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			m, rid, err := scanToponymRow(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			i := pos[rid]
			h := hits[i]
			if withScore {
				m["score"] = h.score
				m["matched_on"] = h.matched
			}
			if withDist {
				m["distance_m"] = math.Round(h.distance*10) / 10
			}
			out[i] = m
		}
		rows.Close()
	}
	// drop any nil (shouldn't happen)
	compact := out[:0]
	for _, m := range out {
		if m != nil {
			compact = append(compact, m)
		}
	}
	return compact, nil
}

// ToponymByID returns one full row by GLOBALID (or numeric rowid).
func ToponymByID(id string) (map[string]interface{}, error) {
	topoMu.RLock()
	db := topoDB
	ready := topoReady
	topoMu.RUnlock()
	if !ready {
		return nil, fmt.Errorf("toponyms not loaded")
	}
	// GLOBALIDs are hex UUIDs in braces; a bare integer means the rowid
	// (CAST would have turned "7F3A…" into rowid 7 — checked in Go instead).
	id = strings.TrimSpace(id)
	var row *sql.Row
	if n, err := strconv.Atoi(id); err == nil && n > 0 {
		row = db.QueryRow(`SELECT `+toponymRowCols+` FROM toponyms WHERE rowid = ?`, n)
	} else {
		if !strings.HasPrefix(id, "{") {
			id = "{" + strings.ToUpper(id) + "}"
		}
		row = db.QueryRow(`SELECT `+toponymRowCols+` FROM toponyms WHERE id = ?`, id)
	}
	m, _, err := scanToponymRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return m, err
}

// ToponymsSnapshot returns the current index + db for handlers.
func toponymsSnapshot() (*toponymIndex, *sql.DB, map[string]string, bool) {
	topoMu.RLock()
	defer topoMu.RUnlock()
	return topoIdx, topoDB, topoMeta, topoReady
}

// ToponymsForKG returns every toponym stored under a KG, Rieden first.
func ToponymsForKG(kg string, layers map[string]bool, limit, offset int) ([]map[string]interface{}, int, error) {
	idx, db, _, ready := toponymsSnapshot()
	if !ready {
		return nil, 0, fmt.Errorf("toponyms not loaded")
	}
	q := &ToponymQuery{KG: map[string]bool{kg: true}, Layers: layers}
	hits := idx.filter(q, 0)
	// Rieden are the cadastral names — list them first, then settlements.
	order := map[uint8]int{6: 0, 0: 1, 5: 2, 1: 3, 2: 4, 4: 5, 3: 6}
	sort.SliceStable(hits, func(i, j int) bool {
		return order[hits[i].entry.layer] < order[hits[j].entry.layer]
	})
	total := len(hits)
	if offset > len(hits) {
		offset = len(hits)
	}
	end := offset + limit
	if limit <= 0 || end > len(hits) {
		end = len(hits)
	}
	rows, err := fetchToponymRows(db, hits[offset:end], false, false)
	return rows, total, err
}

// ToponymsInBBox is the layers=toponyms provider for /spatial/bbox.
func ToponymsInBBox(west, south, east, north float64, layers map[string]bool, limit int) ([]map[string]interface{}, int, error) {
	idx, db, _, ready := toponymsSnapshot()
	if !ready {
		return nil, 0, fmt.Errorf("toponyms not loaded")
	}
	q := &ToponymQuery{BBox: &[4]float64{west, south, east, north}, Layers: layers}
	hits := idx.filter(q, 0)
	total := len(hits)
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	rows, err := fetchToponymRows(db, hits, false, false)
	return rows, total, err
}

// ToponymsNear is the reverse lookup: names around a coordinate, by distance.
func ToponymsNear(lon, lat, radiusM float64, layers map[string]bool, fcodes map[int]bool, limit, offset int) ([]map[string]interface{}, int, error) {
	idx, db, _, ready := toponymsSnapshot()
	if !ready {
		return nil, 0, fmt.Errorf("toponyms not loaded")
	}
	dLat := radiusM / 111320.0
	dLon := radiusM / (111320.0 * math.Cos(lat*math.Pi/180))
	q := &ToponymQuery{
		BBox:   &[4]float64{lon - dLon, lat - dLat, lon + dLon, lat + dLat},
		Layers: layers, FCodes: fcodes, HasNear: true, NearLon: lon, NearLat: lat, RadiusM: radiusM,
	}
	hits := idx.filter(q, 0)
	total := len(hits)
	if offset > len(hits) {
		offset = len(hits)
	}
	end := offset + limit
	if limit <= 0 || end > len(hits) {
		end = len(hits)
	}
	rows, err := fetchToponymRows(db, hits[offset:end], false, true)
	return rows, total, err
}

// topoOrgStopwords are the organisational words on subsidy/land registers
// ("AGRARGEM. LIEGNITZALM", "WEIDEGEMEINSCHAFT PFLEGALM") that carry no
// toponymic information. Stripped only in the fallback pass.
var topoOrgStopwords = map[string]bool{
	"agrargem": true, "agrargemeinschaft": true, "agrargemeinsch": true, "ag": true, "agr": true,
	"weidegemeinschaft": true, "weidegem": true, "almgemeinschaft": true, "almgem": true,
	"alminteressentschaft": true, "interessentschaft": true, "interessentsch": true, "nachbarschaft": true,
	"gemeinschaft": true, "genossenschaft": true, "gen": true, "verein": true, "gmbh": true, "og": true, "kg": true,
	"forstverwaltung": true, "forstbetrieb": true, "gutsverwaltung": true, "gut": true, "stift": true,
	"gemeinde": true, "stadtgemeinde": true, "marktgemeinde": true, "ortsgemeinde": true,
	"und": true, "u": true, "der": true, "die": true, "das": true, "des": true, "von": true, "vom": true, "am": true, "im": true, "an": true, "in": true,
	"bei": true, "zu": true, "zum": true, "zur": true,
}

// SearchToponyms is the public search entry point. When the literal query
// yields nothing and it has several words, it retries with organisational
// stopwords removed and finally with the single most specific token, so the
// name as written on a register ("AGRARGEM. LIEGNITZALM") still resolves.
// The query actually used is reported via QueryUsed.
func SearchToponyms(q *ToponymQuery, limit, offset int) ([]map[string]interface{}, int, error) {
	idx, db, _, ready := toponymsSnapshot()
	if !ready {
		return nil, 0, fmt.Errorf("toponyms not loaded")
	}
	if q.MinScore <= 0 {
		q.MinScore = toponymsDefaultMinScore
	}
	var hits []toponymHit
	if strings.TrimSpace(q.Q) == "" {
		hits = idx.filter(q, 0)
	} else {
		hits = idx.search(q)
		// Weak or empty literal result on a multi-word query: try the rewrites
		// and keep whichever answers best.
		if len(hits) == 0 || hits[0].score < 0.75 {
			bestScore := 0.0
			if len(hits) > 0 {
				bestScore = hits[0].score
			}
			for _, alt := range toponymFallbackQueries(q.Q) {
				q2 := *q
				q2.Q = alt.q
				if alt.kgs != nil && q2.KG == nil {
					q2.KG = alt.kgs
				}
				h2 := idx.search(&q2)
				if len(h2) > 0 && h2[0].score > bestScore {
					hits, bestScore = h2, h2[0].score
					q.QueryUsed = alt.q
					if alt.note != "" {
						q.QueryUsed += " (gemeinde=" + alt.note + ")"
					}
					if bestScore >= 0.9 {
						break
					}
				}
			}
		}
	}
	total := len(hits)
	if offset > len(hits) {
		offset = len(hits)
	}
	end := offset + limit
	if limit <= 0 || end > len(hits) {
		end = len(hits)
	}
	rows, err := fetchToponymRows(db, hits[offset:end], strings.TrimSpace(q.Q) != "", q.HasNear)
	return rows, total, err
}

// ToponymStats is the block for /api/v1/stats and /toponyms/stats.
func ToponymStats() map[string]interface{} {
	idx, _, meta, ready := toponymsSnapshot()
	if !ready {
		return map[string]interface{}{
			"ready": false,
			"hint":  "run ./umfeld-at -toponyms-import (BEV DLM-Namen, feedback #17)",
		}
	}
	byLayer := map[string]int{}
	for i, n := range idx.byLayer {
		byLayer[toponymLayerNames[i]] = n
	}
	out := map[string]interface{}{
		"ready":                 true,
		"rows":                  len(idx.entries),
		"search_strings":        len(idx.names),
		"trigrams":              len(idx.tri),
		"by_layer":              byLayer,
		"kgs_with_names":        len(idx.byKG),
		"stichtag":              meta["stichtag"],
		"source_url":            meta["source_url"],
		"imported_at":           meta["imported_at"],
		"positional_accuracy_m": toponymsPositionalAccM,
		"license":               "CC-BY-4.0 (BEV), see /api/v1/license id=bev_dlm_namen",
	}
	for _, k := range []string{"kg_source_bev", "kg_source_pip", "kg_source_nearest_parcel", "kg_source_none", "kg_source_bev_unverified", "source_gpkg", "import_duration"} {
		if v, ok := meta[k]; ok {
			out[k] = v
		}
	}
	return out
}

// ─── import ─────────────────────────────────────────────────────────────────

// ToponymImportOptions controls -toponyms-import.
type ToponymImportOptions struct {
	GPKGPath string // local file; downloaded from URL when missing
	URL      string
	Stichtag string // YYYY-MM-DD; derived from the filename when empty
}

type topoCurrencySheet struct {
	poly orb.Polygon
	bbox orb.Bound
	year int
}

var topoStichtagRe = regexp.MustCompile(`(\d{4})(\d{2})(\d{2})\.gpkg$`)

// RunToponymImport builds data/toponyms.db from the BEV GPKG. Runs in its own
// process; the live API reloads the swapped file within toponymsReloadInterval.
func RunToponymImport(opt ToponymImportOptions) error {
	t0 := time.Now()
	if opt.GPKGPath == "" {
		opt.GPKGPath = toponymsDefaultGPKG
	}
	if opt.URL == "" {
		opt.URL = toponymsDefaultURL
	}
	if _, err := os.Stat(opt.GPKGPath); err != nil {
		log.Printf("🏔  downloading %s → %s", opt.URL, opt.GPKGPath)
		if err := downloadToFile(opt.URL, opt.GPKGPath); err != nil {
			return fmt.Errorf("download: %w", err)
		}
	}
	if opt.Stichtag == "" {
		if m := topoStichtagRe.FindStringSubmatch(opt.GPKGPath); m != nil {
			opt.Stichtag = m[1] + "-" + m[2] + "-" + m[3]
		} else if m := topoStichtagRe.FindStringSubmatch(opt.URL); m != nil {
			opt.Stichtag = m[1] + "-" + m[2] + "-" + m[3]
		}
	}

	src, err := sql.Open("sqlite3", "file:"+opt.GPKGPath+"?mode=ro")
	if err != nil {
		return err
	}
	defer src.Close()

	// KG register for admin enrichment (kg table of the search index).
	kgReg, err := loadKGRegisterForToponyms()
	if err != nil {
		return fmt.Errorf("kg register: %w", err)
	}
	log.Printf("🏔  KG register: %d KGs", len(kgReg))

	sheets, err := loadCurrencySheets(src)
	if err != nil {
		log.Printf("🏔  currency sheets: %v (currency_year will be empty)", err)
	} else {
		log.Printf("🏔  currency sheets: %d ÖK map sheets", len(sheets))
	}

	tmp := toponymsDBPath + ".tmp"
	os.Remove(tmp)
	dst, err := openToponymsDB(tmp, true)
	if err != nil {
		return err
	}
	dst.SetMaxOpenConns(1)
	if err := createToponymSchema(dst); err != nil {
		dst.Close()
		return err
	}

	tx, err := dst.Begin()
	if err != nil {
		dst.Close()
		return err
	}
	ins, err := tx.Prepare(`INSERT INTO toponyms(id, layer, f_code, f_name, name, variant_name, name_kroat, name_slow, name_ung,
		name_norm, name_stem, objektart, art, bedeutung, elevation_m, okz, einwohner_klasse, abandoned,
		kg_code, kg_code_source, kg_name, gemeinde_code, gemeinde_name, district_code, district_name, state_code, state_name,
		lon, lat, source_indikator, lfd_nummer, erstell_dat, bearb_dat, stichtag, currency_year)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		tx.Rollback()
		dst.Close()
		return err
	}
	insR, err := tx.Prepare(`INSERT INTO toponyms_rtree(id, min_lon, max_lon, min_lat, max_lat) VALUES (?,?,?,?,?)`)
	if err != nil {
		tx.Rollback()
		dst.Close()
		return err
	}

	assigner := newToponymKGAssigner(kgReg)
	total := 0
	srcCounts := map[string]int{}
	for _, L := range toponymLayers {
		n, err := importToponymLayer(src, L.table, L.layer, opt.Stichtag, kgReg, sheets, assigner, ins, insR, srcCounts)
		if err != nil {
			tx.Rollback()
			dst.Close()
			return fmt.Errorf("%s: %w", L.table, err)
		}
		log.Printf("🏔  %-24s %7d rows", L.table, n)
		total += n
	}
	metaKV := map[string]string{
		"schema_version":  toponymsSchemaVersion,
		"stichtag":        opt.Stichtag,
		"source_url":      opt.URL,
		"source_gpkg":     opt.GPKGPath,
		"imported_at":     time.Now().UTC().Format(time.RFC3339),
		"rows":            strconv.Itoa(total),
		"import_duration": time.Since(t0).Round(time.Second).String(),
	}
	for k, v := range srcCounts {
		metaKV["kg_source_"+k] = strconv.Itoa(v)
	}
	for k, v := range metaKV {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO toponyms_meta(key,value) VALUES(?,?)`, k, v); err != nil {
			tx.Rollback()
			dst.Close()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		dst.Close()
		return err
	}
	if _, err := dst.Exec(`ANALYZE`); err != nil {
		log.Printf("🏔  analyze: %v", err)
	}
	dst.Close()
	if err := os.Rename(tmp, toponymsDBPath); err != nil {
		return err
	}
	log.Printf("🏔  toponyms import complete: %d rows, kg sources %v, %s → %s (%s)",
		total, srcCounts, opt.GPKGPath, toponymsDBPath, time.Since(t0).Round(time.Second))
	return nil
}

func createToponymSchema(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE toponyms (
			rowid INTEGER PRIMARY KEY,
			id TEXT NOT NULL UNIQUE,
			layer TEXT NOT NULL, f_code INTEGER, f_name TEXT,
			name TEXT NOT NULL, variant_name TEXT, name_kroat TEXT, name_slow TEXT, name_ung TEXT,
			name_norm TEXT NOT NULL, name_stem TEXT,
			objektart TEXT, art TEXT, bedeutung TEXT,
			elevation_m INTEGER, okz INTEGER, einwohner_klasse TEXT, abandoned INTEGER NOT NULL DEFAULT 0,
			kg_code TEXT, kg_code_source TEXT, kg_name TEXT,
			gemeinde_code TEXT, gemeinde_name TEXT, district_code TEXT, district_name TEXT,
			state_code TEXT, state_name TEXT,
			lon REAL NOT NULL, lat REAL NOT NULL,
			source_indikator TEXT, lfd_nummer INTEGER,
			erstell_dat TEXT, bearb_dat TEXT, stichtag TEXT, currency_year INTEGER
		)`,
		`CREATE VIRTUAL TABLE toponyms_rtree USING rtree(id, min_lon, max_lon, min_lat, max_lat)`,
		`CREATE INDEX idx_topo_kg ON toponyms(kg_code)`,
		`CREATE INDEX idx_topo_gem ON toponyms(gemeinde_code)`,
		`CREATE INDEX idx_topo_layer ON toponyms(layer, f_code)`,
		`CREATE INDEX idx_topo_norm ON toponyms(name_norm)`,
		`CREATE TABLE toponyms_meta(key TEXT PRIMARY KEY, value TEXT)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("%s: %w", s[:40], err)
		}
	}
	return nil
}

type topoKGRow struct {
	code, name, gemCode, gemName, distCode, distName, stateCode, stateName string
}

func loadKGRegisterForToponyms() (map[string]topoKGRow, error) {
	if globalSearchIndex == nil {
		return nil, fmt.Errorf("search index not initialised")
	}
	rows, err := globalSearchIndex.DB().Query(`SELECT kg_code, COALESCE(kg_name,''), COALESCE(gemeinde_code,''), COALESCE(gemeinde_name,''),
		COALESCE(district_code,''), COALESCE(district_name,''), COALESCE(state_code,''), COALESCE(state_name,'') FROM kg`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]topoKGRow{}
	for rows.Next() {
		var r topoKGRow
		if rows.Scan(&r.code, &r.name, &r.gemCode, &r.gemName, &r.distCode, &r.distName, &r.stateCode, &r.stateName) == nil {
			out[r.code] = r
		}
	}
	return out, nil
}

func loadCurrencySheets(src *sql.DB) ([]topoCurrencySheet, error) {
	rows, err := src.Query(`SELECT geom, AKTUALITAETSSTAND FROM DLM_AKTUALITAETSSTAND WHERE geom IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []topoCurrencySheet
	for rows.Next() {
		var blob []byte
		var year sql.NullInt64
		if rows.Scan(&blob, &year) != nil || !year.Valid {
			continue
		}
		raw, err := gpkgToWKB(blob)
		if err != nil {
			continue
		}
		g, err := wkb.Unmarshal(raw)
		if err != nil {
			continue
		}
		var polys []orb.Polygon
		switch t := g.(type) {
		case orb.Polygon:
			polys = []orb.Polygon{t}
		case orb.MultiPolygon:
			polys = t
		}
		for _, p := range polys {
			wp := make(orb.Polygon, 0, len(p))
			for _, ring := range p {
				wr := make(orb.Ring, 0, len(ring))
				for _, pt := range ring {
					lon, lat, ok := GKCentralToWGS84(pt[0], pt[1])
					if !ok {
						continue
					}
					wr = append(wr, orb.Point{lon, lat})
				}
				if len(wr) >= 4 {
					wp = append(wp, wr)
				}
			}
			if len(wp) > 0 {
				out = append(out, topoCurrencySheet{poly: wp, bbox: wp.Bound(), year: int(year.Int64)})
			}
		}
	}
	return out, nil
}

func currencyYearFor(sheets []topoCurrencySheet, lon, lat float64) (int, bool) {
	pt := orb.Point{lon, lat}
	for i := range sheets {
		if !sheets[i].bbox.Contains(pt) {
			continue
		}
		if planar.PolygonContains(sheets[i].poly, pt) {
			return sheets[i].year, true
		}
	}
	return 0, false
}

// toponymKGAssigner resolves the KG for a WGS84 point.
type toponymKGAssigner struct {
	kgReg map[string]topoKGRow
}

func newToponymKGAssigner(kgReg map[string]topoKGRow) *toponymKGAssigner {
	return &toponymKGAssigner{kgReg: kgReg}
}

// assign returns (kg_code, source). Sources: "ne" (the res-12 cell of the
// point is in exactly one candidate KG's declared cell set), "bbox" (one
// bbox candidate, or the smallest candidate bbox), "" when nothing is near.
func (a *toponymKGAssigner) assign(lon, lat float64) (string, string) {
	cands := KGCodesForPointMem(lon, lat)
	if len(cands) == 0 {
		return "", ""
	}
	if len(cands) == 1 {
		return cands[0], "bbox"
	}
	if id, err := neLatLngToCell(lon, lat, 12); err == nil {
		var hits []string
		for _, kg := range cands {
			if v := neDeclViewOf(kg); v != nil && neFindRow(v.Rows, neLURowBytes, id) != nil {
				hits = append(hits, kg)
			}
		}
		if len(hits) == 1 {
			return hits[0], "ne"
		}
		if len(hits) > 1 {
			cands = hits
		}
	}
	best, bestArea := "", math.Inf(1)
	for _, kg := range cands {
		if w, s, e, n, ok := KGBBoxMem(kg); ok {
			if ar := (e - w) * (n - s); ar < bestArea {
				best, bestArea = kg, ar
			}
		}
	}
	if best == "" {
		best = cands[0]
	}
	return best, "bbox"
}

func importToponymLayer(src *sql.DB, table, layer, stichtag string, kgReg map[string]topoKGRow, sheets []topoCurrencySheet,
	assigner *toponymKGAssigner, ins, insR *sql.Stmt, srcCounts map[string]int) (int, error) {
	// Discover columns so one SELECT works for every layer.
	cols := map[string]bool{}
	crows, err := src.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return 0, err
	}
	for crows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if crows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk) == nil {
			cols[strings.ToUpper(name)] = true
		}
	}
	crows.Close()
	sel := func(c string) string {
		if cols[c] {
			return c
		}
		return "NULL AS " + c
	}
	q := fmt.Sprintf(`SELECT GLOBALID, geom, F_CODE, F_NAME, NAME, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, ERSTELL_DAT, BEARB_DAT FROM %s WHERE geom IS NOT NULL AND NAME IS NOT NULL`,
		sel("VARIANTNAME"), sel("NAME_KROAT"), sel("NAME_SLOW"), sel("NAME_UNG"), sel("OBJEKTART"), sel("ART"), sel("BEDEUTUNG"),
		sel("HOEHE_BODEN"), sel("OKZ"), sel("EINWOHNER_KLASSE"), sel("KG_NUMMER"), sel("LFD_NUMMER"), sel("INDIKATOR"), table)
	rows, err := src.Query(q)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var gid string
		var blob []byte
		var fcode sql.NullInt64
		var fname, name, variant, kro, slo, ung, objektart, art, bedeutung, ek, indik, erstell, bearb sql.NullString
		var hoehe, okz, kgNum, lfd sql.NullInt64
		if err := rows.Scan(&gid, &blob, &fcode, &fname, &name, &variant, &kro, &slo, &ung, &objektart, &art, &bedeutung,
			&hoehe, &okz, &ek, &kgNum, &lfd, &indik, &erstell, &bearb); err != nil {
			return n, err
		}
		raw, err := gpkgToWKB(blob)
		if err != nil {
			continue
		}
		g, err := wkb.Unmarshal(raw)
		if err != nil {
			continue
		}
		pt, ok := g.(orb.Point)
		if !ok {
			continue
		}
		lon, lat, ok := GKCentralToWGS84(pt[0], pt[1])
		if !ok {
			continue
		}
		nm := strings.TrimSpace(name.String)
		if nm == "" {
			continue
		}
		nn := normalizeToponym(nm)
		stem := stemToponym(nn)

		// KG assignment
		var kg, kgSrc string
		if layer == "ried" && kgNum.Valid && kgNum.Int64 > 0 {
			kg = NormalizeKGCode(strconv.FormatInt(kgNum.Int64, 10))
			if _, ok := kgReg[kg]; ok {
				kgSrc = "bev"
			} else {
				kgSrc = "bev_unverified"
			}
		} else {
			kg, kgSrc = assigner.assign(lon, lat)
			if kg == "" {
				kgSrc = "none"
			}
		}
		srcCounts[kgSrc]++
		reg := kgReg[kg]

		abandoned := 0
		switch fcode.Int64 {
		case 7123, 7124, 7131:
			abandoned = 1
		}
		var elev interface{}
		if hoehe.Valid && hoehe.Int64 > 0 {
			elev = hoehe.Int64
		}
		var okzV, lfdV interface{}
		if okz.Valid && okz.Int64 > 0 {
			okzV = okz.Int64
		}
		if lfd.Valid {
			lfdV = lfd.Int64
		}
		var curYear interface{}
		if y, ok := currencyYearFor(sheets, lon, lat); ok {
			curYear = y
		}
		var kgV, kgNameV, gemV, gemNameV, distV, distNameV, stcV, stnV interface{}
		if kg != "" {
			kgV = kg
			if reg.code != "" {
				kgNameV, gemV, gemNameV, distV, distNameV, stcV, stnV = reg.name, reg.gemCode, reg.gemName, reg.distCode, reg.distName, reg.stateCode, reg.stateName
			}
		}
		res, err := ins.Exec(gid, layer, fcode.Int64, nullIfEmpty(fname.String), nm, nullIfEmpty(variant.String),
			nullIfEmpty(kro.String), nullIfEmpty(slo.String), nullIfEmpty(ung.String), nn, nullIfEmpty(stem),
			nullIfEmpty(objektart.String), nullIfEmpty(art.String), nullIfEmpty(bedeutung.String), elev, okzV,
			nullIfEmpty(ek.String), abandoned, kgV, kgSrc, kgNameV, gemV, gemNameV, distV, distNameV, stcV, stnV,
			lon, lat, nullIfEmpty(indik.String), lfdV, topoDate(erstell.String), topoDate(bearb.String), stichtag, curYear)
		if err != nil {
			return n, err
		}
		rid, _ := res.LastInsertId()
		if _, err := insR.Exec(rid, lon, lon, lat, lat); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

func nullIfEmpty(s string) interface{} {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return s
}

func topoDate(s string) string {
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}

// downloadToFile streams url to path (atomic via .part).
func downloadToFile(url, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(path + ".part")
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	f.Close()
	return os.Rename(path+".part", path)
}

// marshal helper used by the handlers for GeoJSON features.
func toponymFeature(m map[string]interface{}) map[string]interface{} {
	props := copyMap(m)
	lon, _ := props["lon"].(float64)
	lat, _ := props["lat"].(float64)
	delete(props, "lon")
	delete(props, "lat")
	props["_layer"] = "toponym"
	return GeoJSONPointFeature(lon, lat, props)
}

func round7(v float64) float64 { return math.Round(v*1e7) / 1e7 }
