// ne_filter_expr.go — the glyph filter's grammar and evaluator: the field
// catalogue (one table, served as JSON so the Worker is table-driven), the
// `;f=` / `f=` expression parser, the canonical form and the evaluation of
// an expression over the columns of a filter index bin (ne_glyph_fidx.go).
// The Go twin of ne_glyph_filter.js: TestNEFilterTwin demands identical masks.
// docs/ne-filter.md §2.
//
//	f=h>=8,sl=15..30,n2k|pa=np     `,` AND between groups · `|` OR inside · `..` ranges · `+` sets · flags bare
//
// A term is {field, op, a, b} (range), {field, set} (set/flag) or {field, unk}
// (`f=unk`: the value is unknown here). Unknown values never match a range or
// a set. Unknown fields / malformed terms are dropped one by one (the
// "reserved token" posture) and listed in `ignored`.
package main

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// neFiltKind — how a column is read.
const (
	nfNum    = "num"    // raw × Scale + Off, ranges
	nfSet    = "set"    // raw = index into Members
	nfBits   = "bits"   // raw = bitmask, Members[i] = bit i
	nfFlag   = "flag"   // raw ≠ 0 (and ≠ Nodata) = set
	nfBucket = "bucket" // raw = index into Edges (distance classes)
	nfSites  = "sites"  // n2k site codes: raw = index into the bin header's n2k[] list
)

// neFiltField — one column of the fidx + how the grammar reads it.
type neFiltField struct {
	ID      string   `json:"id"`
	Label   string   `json:"label"`
	Kind    string   `json:"kind"`
	Unit    string   `json:"unit,omitempty"`
	Plane   string   `json:"plane"` // core | ext
	Dtype   string   `json:"dtype"` // u8 | i8 | u16
	Scale   float64  `json:"scale,omitempty"`
	Off     float64  `json:"off,omitempty"`
	Nodata  int      `json:"nodata"`
	Members []string `json:"members,omitempty"`
	Edges   []int    `json:"edges,omitempty"`
	Min     float64  `json:"min,omitempty"`
	Max     float64  `json:"max,omitempty"`
	Source  string   `json:"source"`
	Group   string   `json:"group"`
	Labels  []string `json:"labels,omitempty"` // member labels, parallel to Members (chips, legend)
	Halo    bool     `json:"halo,omitempty"`   // cont: bit 7 = the patch touches the halo (≥ 64 for lower bounds, unknown for upper)
}

var neFiltAspect = []string{"n", "ne", "e", "se", "s", "sw", "w", "nw", "flat"}
var neFiltAls = []string{"2020", "2021", "2022", "2023", "2024", "2025", "2026", "2027"}
var neFiltOepul = []string{"bio", "brache", "lse", "alm", "extensiv", "naturdenkmal"}
var neFiltNutz = []string{"none", "getreide", "mais", "oelsaaten", "eiweiss", "hackfruechte", "feldfutter", "gemuese", "sonderkulturen", "brache", "wiese_intensiv", "wiese_extensiv", "weide", "alm", "wein", "obst", "hopfen", "sonst"}
var neFiltDistEdges = []int{0, 25, 50, 100, 250, 500, 1000}

// neFiltFields — THE catalogue (docs/ne-filter.md §2a). Order = column order
// inside a plane. `u8 255 / u16 65535 / i8 −128` = unknown unless noted.
var neFiltFields = []neFiltField{
	// ── core ──
	{ID: "d", Label: "declared use", Kind: nfSet, Plane: "core", Dtype: "u8", Nodata: 255, Members: neGlyphGroups[:], Source: "declared view (log → bev → lu)", Group: "use"},
	{ID: "dsh", Label: "declared share of the dominant group", Kind: nfNum, Unit: "%", Plane: "core", Dtype: "u8", Scale: 100.0 / 255, Nodata: 255, Min: 0, Max: 100, Source: "declared view", Group: "use"},
	{ID: "o", Label: "observed use", Kind: nfSet, Plane: "core", Dtype: "u8", Nodata: 255, Members: neGlyphGroups[:], Source: "srtm obs_lu", Group: "use"},
	{ID: "osh", Label: "observed share of the dominant group", Kind: nfNum, Unit: "%", Plane: "core", Dtype: "u8", Scale: 100.0 / 255, Nodata: 255, Min: 0, Max: 100, Source: "srtm obs_lu", Group: "use"},
	{ID: "h", Label: "object height p90", Kind: nfNum, Unit: "m", Plane: "core", Dtype: "u8", Scale: 0.5, Nodata: 255, Min: 0, Max: 60, Source: "srtm h_p90", Group: "object"},
	{ID: "hmax", Label: "object height max", Kind: nfNum, Unit: "m", Plane: "core", Dtype: "u8", Scale: 0.5, Nodata: 255, Min: 0, Max: 80, Source: "srtm h_max", Group: "object"},
	{ID: "hb", Label: "tallest structure", Kind: nfNum, Unit: "m", Plane: "core", Dtype: "u8", Scale: 0.5, Nodata: 255, Min: 0, Max: 60, Source: "srtm bld_h_max", Group: "structures"},
	{ID: "ht", Label: "tallest tree", Kind: nfNum, Unit: "m", Plane: "core", Dtype: "u8", Scale: 0.5, Nodata: 255, Min: 0, Max: 60, Source: "srtm tree_h_max", Group: "trees"},
	{ID: "bn", Label: "structures in the cell", Kind: nfNum, Unit: "", Plane: "core", Dtype: "u8", Scale: 1, Nodata: 255, Min: 0, Max: 20, Source: "srtm obs_bld_n", Group: "structures"},
	{ID: "bcov", Label: "structure cover", Kind: nfNum, Unit: "%", Plane: "core", Dtype: "u8", Scale: 100.0 / 255, Nodata: 255, Min: 0, Max: 100, Source: "srtm obs_bld_cover", Group: "structures"},
	{ID: "sl", Label: "slope", Kind: nfNum, Unit: "°", Plane: "core", Dtype: "u8", Scale: 1, Nodata: 255, Min: 0, Max: 70, Source: "srtm slope_mean (obs row, else cells bin)", Group: "terrain"},
	{ID: "sl90", Label: "slope p90", Kind: nfNum, Unit: "°", Plane: "core", Dtype: "u8", Scale: 1, Nodata: 255, Min: 0, Max: 80, Source: "srtm slope_p90", Group: "terrain"},
	{ID: "as", Label: "aspect", Kind: nfSet, Plane: "core", Dtype: "u8", Nodata: 255, Members: neFiltAspect, Source: "srtm aspect_dom", Group: "terrain"},
	{ID: "el", Label: "elevation", Kind: nfNum, Unit: "m", Plane: "core", Dtype: "u16", Scale: 1, Nodata: 65535, Min: 100, Max: 3800, Source: "srtm elev_mean", Group: "terrain"},
	{ID: "sub", Label: "subsidy class", Kind: nfSet, Plane: "core", Dtype: "u8", Nodata: 255, Members: neSubClass[:], Source: "subsidy peer cls", Group: "subsidy"},
	{ID: "scov", Label: "claimed share", Kind: nfNum, Unit: "%", Plane: "core", Dtype: "u8", Scale: 100.0 / 255, Nodata: 255, Min: 0, Max: 100, Source: "subsidy peer cov", Group: "subsidy"},
	{ID: "bio", Label: "organic", Kind: nfFlag, Plane: "core", Dtype: "u8", Nodata: 255, Source: "subsidy peer bio", Group: "subsidy"},
	{ID: "n2k", Label: "Natura 2000", Kind: nfFlag, Plane: "core", Dtype: "u8", Nodata: 255, Source: "EEA Natura 2000", Group: "protection"},
	{ID: "n2kt", Label: "Natura 2000 type", Kind: nfSet, Plane: "core", Dtype: "u8", Nodata: 255, Members: []string{"none", "a", "b", "c"}, Source: "EEA Natura 2000", Group: "protection"},
	{ID: "n2ks", Label: "Natura 2000 site", Kind: nfSites, Plane: "core", Dtype: "u8", Nodata: 255, Source: "EEA Natura 2000", Group: "protection"},
	{ID: "chg", Label: "change in the register", Kind: nfSet, Plane: "core", Dtype: "u8", Nodata: 255, Members: []string{"none", "cand", "conf"}, Source: "chunk log", Group: "use"},
	{ID: "cont", Label: "patch size (same declared group)", Kind: nfNum, Unit: "cells", Plane: "core", Dtype: "u8", Scale: 1, Nodata: 255, Min: 1, Max: 64, Halo: true, Source: "derived", Group: "pattern"},
	{ID: "het", Label: "distinct declared groups around", Kind: nfNum, Unit: "", Plane: "core", Dtype: "u8", Scale: 1, Nodata: 255, Min: 0, Max: 6, Source: "derived", Group: "pattern"},
	{ID: "edge", Label: "neighbours of another declared group", Kind: nfNum, Unit: "", Plane: "core", Dtype: "u8", Scale: 1, Nodata: 255, Min: 0, Max: 6, Source: "derived", Group: "pattern"},
	// ── ext ──
	{ID: "hmean", Label: "object height mean", Kind: nfNum, Unit: "m", Plane: "ext", Dtype: "u8", Scale: 0.5, Nodata: 255, Min: 0, Max: 40, Source: "srtm h_mean", Group: "object"},
	{ID: "hbm", Label: "median structure height", Kind: nfNum, Unit: "m", Plane: "ext", Dtype: "u8", Scale: 0.5, Nodata: 255, Min: 0, Max: 40, Source: "srtm bld_h_p50", Group: "structures"},
	{ID: "stor", Label: "storeys of the tallest structure", Kind: nfNum, Unit: "", Plane: "ext", Dtype: "u8", Scale: 1, Nodata: 255, Min: 0, Max: 20, Source: "srtm stories_max", Group: "structures"},
	{ID: "roof", Label: "dominant roof form", Kind: nfSet, Plane: "ext", Dtype: "u8", Nodata: 0, Members: []string{"", "flat", "pitched"}, Source: "srtm roof_dom", Group: "structures"},
	{ID: "hc", Label: "canopy height p90", Kind: nfNum, Unit: "m", Plane: "ext", Dtype: "u8", Scale: 0.5, Nodata: 255, Min: 0, Max: 60, Source: "srtm canopy_h_p90", Group: "trees"},
	{ID: "tn", Label: "tree apices", Kind: nfNum, Unit: "", Plane: "ext", Dtype: "u8", Scale: 1, Nodata: 255, Min: 0, Max: 60, Source: "srtm tree_n", Group: "trees"},
	{ID: "canopy", Label: "crown cover", Kind: nfNum, Unit: "%", Plane: "ext", Dtype: "u8", Scale: 100.0 / 254, Nodata: 255, Min: 0, Max: 100, Source: "srtm crown_frac", Group: "trees"},
	{ID: "ndvi", Label: "NDVI mean", Kind: nfNum, Unit: "", Plane: "ext", Dtype: "u8", Scale: 2.0 / 254, Off: -1, Nodata: 255, Min: -1, Max: 1, Source: "srtm ndvi_mean", Group: "trees"},
	{ID: "ndva", Label: "NDVI amplitude", Kind: nfNum, Unit: "", Plane: "ext", Dtype: "u8", Scale: 1.0 / 254, Nodata: 255, Min: 0, Max: 1, Source: "srtm ndvi_amp", Group: "trees"},
	{ID: "phen", Label: "phenology", Kind: nfSet, Plane: "ext", Dtype: "u8", Nodata: 255, Members: []string{"unknown", "forest", "crop", "pasture", "seasonal", "bare"}, Source: "srtm phenology", Group: "trees"},
	{ID: "loss", Label: "forest loss year", Kind: nfNum, Unit: "year", Plane: "ext", Dtype: "u8", Scale: 1, Off: 2000, Nodata: 255, Min: 2001, Max: 2024, Source: "srtm hansen_loss_year (0 = none, never matches)", Group: "trees"},
	{ID: "dh", Label: "surface change between flights", Kind: nfNum, Unit: "m", Plane: "ext", Dtype: "i8", Scale: 0.25, Nodata: -128, Min: -30, Max: 30, Source: "srtm dh_mean", Group: "object"},
	{ID: "tpi", Label: "topographic position", Kind: nfNum, Unit: "m", Plane: "ext", Dtype: "i8", Scale: 0.25, Nodata: -128, Min: -30, Max: 30, Source: "srtm tpi", Group: "terrain"},
	{ID: "tri", Label: "ruggedness", Kind: nfNum, Unit: "m", Plane: "ext", Dtype: "u8", Scale: 0.1, Nodata: 255, Min: 0, Max: 25, Source: "srtm tri", Group: "terrain"},
	{ID: "rough", Label: "roughness", Kind: nfNum, Unit: "m", Plane: "ext", Dtype: "u8", Scale: 0.1, Nodata: 255, Min: 0, Max: 25, Source: "srtm roughness", Group: "terrain"},
	{ID: "dtm", Label: "ground change between flights", Kind: nfNum, Unit: "m", Plane: "ext", Dtype: "i8", Scale: 0.25, Nodata: -128, Min: -30, Max: 30, Source: "srtm dtm_change", Group: "terrain"},
	{ID: "asd", Label: "aspect (degrees)", Kind: nfNum, Unit: "°", Plane: "ext", Dtype: "u8", Scale: 2, Nodata: 255, Min: 0, Max: 358, Source: "srtm aspect_deg (254 = flat)", Group: "terrain"},
	{ID: "als", Label: "LiDAR flight year", Kind: nfBits, Plane: "ext", Dtype: "u8", Nodata: 255, Members: neFiltAls, Source: "srtm flight_years", Group: "object"},
	{ID: "ver", Label: "declared vs observed verdict", Kind: nfSet, Plane: "ext", Dtype: "u8", Nodata: 255, Members: neObsVerdict, Source: "srtm verdict", Group: "use"},
	{ID: "srel", Label: "outline reliability", Kind: nfSet, Plane: "ext", Dtype: "u8", Nodata: 255, Members: []string{"reliable", "dubious"}, Source: "subsidy peer rel < 150", Group: "subsidy"},
	{ID: "schg", Label: "claim lineage vs previous year", Kind: nfSet, Plane: "ext", Dtype: "u8", Nodata: 255, Members: neSubChg[:], Source: "subsidy peer chg", Group: "subsidy"},
	{ID: "snar", Label: "crop (Schlagnutzungsart)", Kind: nfNum, Unit: "code", Plane: "ext", Dtype: "u16", Scale: 1, Nodata: 65535, Min: 1, Max: 9999, Source: "subsidy peer snar (0 = none, never matches)", Group: "subsidy"},
	{ID: "snar2", Label: "second crop", Kind: nfNum, Unit: "code", Plane: "ext", Dtype: "u16", Scale: 1, Nodata: 65535, Min: 1, Max: 9999, Source: "subsidy peer snar2", Group: "subsidy"},
	{ID: "oepul", Label: "ÖPUL measure", Kind: nfBits, Plane: "ext", Dtype: "u8", Nodata: 255, Members: neFiltOepul, Source: "subsidy peer oepul", Group: "subsidy"},
	{ID: "nutz", Label: "AMA land-use category", Kind: nfSet, Plane: "ext", Dtype: "u8", Nodata: 255, Members: neFiltNutz, Source: "subsidy peer nutz", Group: "subsidy"},
	{ID: "rd", Label: "distance to a road", Kind: nfBucket, Unit: "m", Plane: "ext", Dtype: "u8", Nodata: 255, Edges: neFiltDistEdges, Source: "OSM roads", Group: "nearby"},
	{ID: "wat", Label: "distance to water", Kind: nfBucket, Unit: "m", Plane: "ext", Dtype: "u8", Nodata: 255, Edges: neFiltDistEdges, Source: "OSM water (lines + areas)", Group: "nearby"},
	{ID: "rem", Label: "remoteness", Kind: nfNum, Unit: "/100", Plane: "ext", Dtype: "u8", Scale: 1, Nodata: 255, Min: 0, Max: 100, Source: "OSM remoteness grid 250 m (major roads, transit, settlements; docs/osm-proximity.md)", Group: "nearby"},
}

var neFiltGroupLabels = []string{"buildings", "building land", "cropland", "grassland", "forest", "water", "traffic", "alpine", "other"}
var neFiltSubLabels = []string{"none", "arable", "grassland", "alpine pasture", "vineyard", "permanent crops", "landscape elements", "other"}

var neFiltByID = func() map[string]*neFiltField {
	m := make(map[string]*neFiltField, len(neFiltFields))
	for i := range neFiltFields {
		f := &neFiltFields[i]
		switch f.ID {
		case "d", "o":
			f.Labels = neFiltGroupLabels
		case "sub":
			f.Labels = neFiltSubLabels
		case "n2kt":
			f.Labels = []string{"none", "SPA (birds)", "SCI/SAC (habitats)", "both"}
		case "chg":
			f.Labels = []string{"none", "candidate", "confirmed"}
		case "as":
			f.Labels = []string{"north", "north-east", "east", "south-east", "south", "south-west", "west", "north-west", "flat"}
		}
		m[f.ID] = f
	}
	return m
}()

// neFiltWords — named ranges (FT_WORDS): presets, the find box and the legend
// speak these; the hash always carries the numeric form.
// neFiltAlias — plain words a reader types for a field (`trees>=20` = `ht>=20`);
// the canonical form always carries the field id. Mirrored in ne_glyph_filter.js.
var neFiltAlias = map[string]string{
	"tree": "ht", "trees": "ht", "treeheight": "ht", "height": "h", "slope": "sl", "elevation": "el",
	"building": "hb", "buildings": "hb", "structure": "hb", "structures": "hb", "canopycover": "canopy", "crown": "canopy",
	"use": "d", "declared": "d", "observed": "o", "subsidy": "sub", "claim": "sub", "natura": "n2k", "road": "rd", "water": "wat", "remoteness": "rem", "giant": "ht",
	"aspect": "as", "orientation": "as", "facing": "as", "exposure": "as", "exposition": "as",
}

// neFiltAspectAlias — what a reader may write for a compass sector besides the
// member id and its label (spaces and hyphens are ignored: `north east`,
// `north-east`, `northeast` all fit). Mirrored in ne_glyph_filter.js (ASPECT).
var neFiltAspectAlias = map[string]string{
	"nord": "n", "ost": "e", "o": "e", "sued": "s", "süd": "s", "nordost": "ne", "no": "ne", "suedost": "se", "südost": "se", "so": "se",
	"suedwest": "sw", "südwest": "sw", "nordwest": "nw", "eben": "flat", "flach": "flat", "level": "flat",
}

// neFiltUnits — unit suffixes a value may carry (`20 m`, `15°`, `15 %`), mapped to the catalogue's unit strings.
var neFiltUnits = map[string]string{"m": "m", "meter": "m", "meters": "m", "metres": "m", "°": "°", "deg": "°", "degree": "°", "degrees": "°", "%": "%", "percent": "%", "pct": "%", "cell": "cells", "cells": "cells", "year": "year", "years": "year", "yr": "year"}

const neFiltValRe = `(-?\d+(?:\.\d+)?)\s*((?:m|meters?|metres?|deg|degrees?|percent|pct|cells?|years?|yr)\b|°|%)?`

var (
	neFiltReHead    = regexp.MustCompile(`^([a-z][a-z0-9]*)(.*)$`)
	neFiltReBetween = regexp.MustCompile(`^(?:=\s*|is\s+)?(?:between\s+|from\s+)?` + neFiltValRe + `\s*(?:and|to|\.\.|-)\s*` + neFiltValRe + `$`)
	neFiltReLower   = regexp.MustCompile(`^(?:>=|>|≥|at least|min(?:imum)?|over|above|more than|from|not less than)\s*` + neFiltValRe + `$`)
	neFiltReUpper   = regexp.MustCompile(`^(?:<=|<|≤|at most|max(?:imum)?|under|below|up to|less than|not more than|to)\s*` + neFiltValRe + `$`)
	neFiltReExact   = regexp.MustCompile(`^(?:=|==|:|is|of)?\s*` + neFiltValRe + `$`)
	neFiltReSetTo   = regexp.MustCompile(`^(?:=|:|is)?\s*(?:between\s+|from\s+)?([a-zäöü][a-zäöü -]*?|\d+)\s+to\s+([a-zäöü][a-zäöü -]*|\d+)$`)
)

// neFiltNorm — human phrasing → the core grammar, one term at a time
// (mirrored by norm() in ne_glyph_filter.js). Runs after the word table, before
// neFiltParseTerm, so the hash and the API still carry only the core form:
//
//	ht between 10 and 20 · trees 10 to 20 m · ht 10-20 · ht ≥ 20 m · slope at least 15 % · slope under 5° · el over 1500
//	as northwest to northeast · as=nw..ne · aspect north east · as north and south
//
// A value may carry the field's unit (`20 m`, `15°`, `50 %`); slope in percent
// is converted to degrees (atan, one decimal). Anything else is left as typed and
// fails in neFiltParseTerm like before (→ ignored / incomplete).
func neFiltNorm(t string) string {
	t = strings.Join(strings.Fields(t), " ")
	m := neFiltReHead.FindStringSubmatch(t)
	if m == nil {
		return t
	}
	f, rest := m[1], strings.TrimSpace(m[2])
	if a, ok := neFiltAlias[f]; ok {
		f = a
	}
	fd := neFiltByID[f]
	if fd == nil || f == "k" || rest == "" {
		return t
	}
	if rest == "unk" || rest == "=unk" || rest == "unknown" || rest == "is unknown" || rest == "= unknown" {
		return f + "=unk"
	}
	if neFiltRangeOK(fd) {
		val := func(num, unit string) (string, bool) {
			if unit == "" {
				return num, true
			}
			u := neFiltUnits[unit]
			if u == "%" && fd.Unit == "°" {
				p, err := strconv.ParseFloat(num, 64)
				if err != nil {
					return "", false
				}
				return neFiltNum(math.Round(math.Atan(p/100)*180/math.Pi*10) / 10), true
			}
			return num, u != "" && u == fd.Unit
		}
		if x := neFiltReBetween.FindStringSubmatch(rest); x != nil {
			a, oka := val(x[1], x[2])
			b, okb := val(x[3], x[4])
			if oka && okb {
				return f + "=" + a + ".." + b
			}
			return t
		}
		for _, r := range []struct {
			re *regexp.Regexp
			op string
		}{{neFiltReLower, ">="}, {neFiltReUpper, "<="}, {neFiltReExact, "="}} {
			if x := r.re.FindStringSubmatch(rest); x != nil {
				if a, ok := val(x[1], x[2]); ok {
					return f + r.op + a
				}
				return t
			}
		}
		return t
	}
	// sets, flags, sites: `as north east`, `as north to east`, `d wald or wasser`, `as=nw..ne`
	if f == "as" {
		if x := neFiltReSetTo.FindStringSubmatch(rest); x != nil {
			return f + "=" + strings.TrimSpace(x[1]) + ".." + strings.TrimSpace(x[2])
		}
	}
	v := ""
	switch {
	case strings.HasPrefix(rest, "="), strings.HasPrefix(rest, ":"):
		v = strings.TrimSpace(rest[1:])
	case strings.HasPrefix(rest, "is "):
		v = strings.TrimSpace(rest[3:])
	case strings.HasPrefix(m[2], " "): // `as north east`, `d wald or wasser`: a space after the field
		v = rest
	default:
		return t
	}
	if v == "" {
		return f + "="
	}
	if fd.Kind == nfFlag && (v == "yes" || v == "1" || v == "true") {
		return f
	}
	v = strings.NewReplacer(" and ", "+", " or ", "+", " + ", "+", ", ", "+", "/", "+").Replace(v)
	return f + "=" + v
}

var neFiltWords = map[string]string{
	"tall-trees": "ht>=20", "giants": "ht>=40", "treeless": "ht=unk", "remote": "rem>=60", "central": "rem<=20", "sparse": "canopy<=25", "dense": "canopy>=75",
	"steep": "sl>=20", "flat": "sl<=3", "high": "el>=1500", "low": "el<=400",
	"south-facing": "as=s+sw+se", "north-facing": "as=n+nw+ne",
	"tall": "hb>=12", "built": "bn>=1", "unbuilt": "bn=0", "wooded": "canopy>=50", "open": "canopy<=10",
	"organic": "bio", "claimed": "sub=acker+gruenland+alm+wein+dauerkultur+gloez+sonst", "unclaimed": "sub=none",
	"protected": "n2k", "changed": "chg=conf", "farmland": "d=acker+gruen", "forest": "d=wald", "settlement": "d=geb+bau",
	"vineyards": "sub=wein", "meadows": "sub=gruenland", "fields": "sub=acker", "alpine": "d=alpen",
}

// neFiltPresets — three rows at the top of the empty popover.
var neFiltPresets = [][2]string{
	{"steep south-facing farmland", "d=acker+gruen,sl>=15,as=s+sw+se"},
	{"tall structures outside settlements", "hb>=12,d=acker+gruen+wald+sonst"},
	{"Natura 2000 land under subsidy claim", "n2k,scov>=50"},
	{"tree giants (over 40 m)", "ht>=40"},
	{"remote forest, no road within 500 m", "d=wald,remote,rd>=500"},
}

// ── terms ────────────────────────────────────────────────────────────────────

type neFiltTerm struct {
	F    string
	Lo   float64 // range bounds in physical units; NaN = open
	Hi   float64
	Set  []string // set / bits / sites members, flag: empty
	Unk  bool     // f=unk
	Flag bool     // bare field
}

type neFiltExpr struct {
	Groups     [][]neFiltTerm // conjunction of disjunctions
	K          int            // post-filter: keep match clusters ≥ K cells (0 = off)
	Ignored    []string
	Incomplete []neFiltIncomplete // the subset of Ignored that names a real field without a usable value
}

// neFiltIncomplete — an ignored term whose field exists (`ht`, `trees`, `d=`,
// `sl>=`, `d=nope`): not a new filter, a nudge. The page puts Start in the input and
// asks for a value; the API answers it next to `ignored`.
type neFiltIncomplete struct {
	Term    string `json:"term"`
	Field   string `json:"field"`
	Label   string `json:"label"`
	Start   string `json:"start"`   // what to type first: `ht>=` · `d=`
	Example string `json:"example"` // a complete term: `ht>=20` · `d=wald`
	Values  string `json:"values"`  // what fits after the operator
}

// neFiltIncompleteOf — classifies an ignored term; nil when its field is unknown.
func neFiltIncompleteOf(t string) *neFiltIncomplete {
	i := 0
	for i < len(t) && (t[i] >= 'a' && t[i] <= 'z' || t[i] >= '0' && t[i] <= '9' && i > 0) {
		i++
	}
	f, rest := t[:i], t[i:]
	if a, ok := neFiltAlias[f]; ok {
		f = a
	}
	fd := neFiltByID[f]
	if fd == nil || f == "k" {
		return nil
	}
	op := ">="
	if !neFiltRangeOK(fd) {
		op = "="
	}
	start := f + op
	if rest == ">=" || rest == "<=" || rest == "=" {
		start = f + rest
	}
	return &neFiltIncomplete{Term: t, Field: f, Label: fd.Label, Start: start, Example: neFiltExample(fd), Values: neFiltValues(fd)}
}

// neFiltExample — one complete term for a field: a named word if one covers
// it (`tall-trees` → `ht>=20`), else a third of the range, the first member.
func neFiltExample(fd *neFiltField) string {
	best := ""
	for _, w := range neFiltWords {
		if strings.HasPrefix(w, fd.ID+">=") || strings.HasPrefix(w, fd.ID+"<=") || (strings.HasPrefix(w, fd.ID+"=") && !strings.HasSuffix(w, "=unk")) {
			if best == "" || w < best {
				best = w
			}
		}
	}
	if best != "" {
		return best
	}
	switch fd.Kind {
	case nfNum:
		v := fd.Max / 3
		if fd.Min < 0 || fd.Max <= 1 {
			v = fd.Max / 2
		}
		return fd.ID + ">=" + neFiltNum(math.Round(v))
	case nfBucket:
		return fd.ID + "<=" + neFiltNum(float64(fd.Edges[2]))
	case nfSites:
		return fd.ID
	default:
		for _, m := range fd.Members {
			if m != "" && m != "none" && m != "unknown" {
				return fd.ID + "=" + m
			}
		}
	}
	return fd.ID
}

// neFiltValues — the value space of a field in one line (the nudge, the reference table).
func neFiltValues(fd *neFiltField) string {
	switch fd.Kind {
	case nfNum:
		r := ""
		if fd.Max > 0 || fd.Min != 0 {
			r = " · " + neFiltNum(fd.Min) + "–" + neFiltNum(fd.Max)
			if fd.Unit != "" {
				r += " " + fd.Unit
			}
		}
		return "≥a · ≤b · a..b" + r
	case nfBucket:
		p := make([]string, len(fd.Edges))
		for i, e := range fd.Edges {
			p[i] = strconv.Itoa(e)
		}
		return "≥a · ≤b · classes " + strings.Join(p, " · ") + " " + fd.Unit
	case nfFlag:
		return "bare (" + fd.ID + ")"
	case nfSites:
		return "bare = any site · =<site code>"
	default:
		p := make([]string, 0, len(fd.Members))
		for i, m := range fd.Members {
			if m == "" {
				continue
			}
			if fd.Labels != nil && i < len(fd.Labels) && fd.Labels[i] != "" && fd.Labels[i] != m {
				p = append(p, m+" ("+fd.Labels[i]+")")
			} else {
				p = append(p, m)
			}
		}
		return "=m1+m2 · " + strings.Join(p, " · ")
	}
}

func (e *neFiltExpr) empty() bool { return len(e.Groups) == 0 && e.K == 0 }

// neFiltNeedsExt — does the expression touch an ext-plane column (the Worker fetches p=ext then)?
func neFiltNeedsExt(e *neFiltExpr) bool {
	for _, g := range e.Groups {
		for _, t := range g {
			if fd := neFiltByID[t.F]; fd != nil && fd.Plane == "ext" {
				return true
			}
		}
	}
	return false
}

// neFiltParse — one expression; never fails, bad pieces land in Ignored.
func neFiltParse(s string) *neFiltExpr {
	e := &neFiltExpr{}
	s = strings.ToLower(strings.TrimSpace(s)) // field ids, members and words are case-insensitive; site codes are re-uppercased
	if s == "" {
		return e
	}
	if len(s) > 2048 {
		s = s[:2048]
	}
	for _, g := range strings.Split(s, ",") {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		if len(e.Groups) >= 64 {
			e.Ignored = append(e.Ignored, g)
			continue
		}
		var terms []neFiltTerm
		for _, t := range strings.Split(g, "|") {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			if w, ok := neFiltWords[strings.ToLower(t)]; ok {
				t = w
			} else {
				t = neFiltNorm(t)
			}
			if term, ok := neFiltParseTerm(t); ok {
				if term.F == "k" {
					if !math.IsNaN(term.Lo) && term.Lo >= 2 {
						e.K = int(math.Min(term.Lo, 4096))
					} else {
						e.Ignored = append(e.Ignored, t)
					}
					continue
				}
				terms = append(terms, term)
			} else {
				e.Ignored = append(e.Ignored, t)
				if inc := neFiltIncompleteOf(t); inc != nil {
					e.Incomplete = append(e.Incomplete, *inc)
				}
			}
		}
		if len(terms) > 0 {
			e.Groups = append(e.Groups, terms)
		}
	}
	return e
}

func neFiltParseTerm(t string) (neFiltTerm, bool) {
	nan := math.NaN()
	term := neFiltTerm{Lo: nan, Hi: nan}
	i := 0
	for i < len(t) && (t[i] >= 'a' && t[i] <= 'z' || t[i] >= '0' && t[i] <= '9' && i > 0) {
		i++
	}
	f, rest := t[:i], t[i:]
	if f == "" {
		return term, false
	}
	if a, ok := neFiltAlias[f]; ok {
		f = a
	}
	term.F = f
	if f == "k" {
		if strings.HasPrefix(rest, ">=") {
			rest = rest[2:]
		} else if strings.HasPrefix(rest, "=") {
			rest = rest[1:]
		} else {
			return term, false
		}
		v, err := strconv.ParseFloat(rest, 64)
		if err != nil {
			return term, false
		}
		term.Lo = v
		return term, true
	}
	fd := neFiltByID[f]
	if fd == nil {
		return term, false
	}
	if rest == "" {
		if fd.Kind == nfFlag || fd.Kind == nfSites {
			term.Flag = true
			return term, true
		}
		if fd.ID == "chg" {
			term.Set = []string{"cand", "conf"}
			return term, true
		}
		return term, false // a bare numeric field is incomplete (neFiltIncomplete): the reader is asked for a value
	}
	num := func(s string) (float64, bool) {
		v, err := strconv.ParseFloat(s, 64)
		return v, err == nil && !math.IsNaN(v) && !math.IsInf(v, 0)
	}
	switch {
	case strings.HasPrefix(rest, ">="):
		v, ok := num(rest[2:])
		if !ok || !neFiltRangeOK(fd) {
			return term, false
		}
		term.Lo = v
	case strings.HasPrefix(rest, "<="):
		v, ok := num(rest[2:])
		if !ok || !neFiltRangeOK(fd) {
			return term, false
		}
		term.Hi = v
	case strings.HasPrefix(rest, "="):
		v := rest[1:]
		if v == "unk" {
			term.Unk = true
			return term, true
		}
		if neFiltRangeOK(fd) {
			if p := strings.Index(v, ".."); p >= 0 {
				a, b := v[:p], v[p+2:]
				if a == "" && b == "" {
					return term, false
				}
				if a != "" {
					x, ok := num(a)
					if !ok {
						return term, false
					}
					term.Lo = x
				}
				if b != "" {
					x, ok := num(b)
					if !ok {
						return term, false
					}
					term.Hi = x
				}
				if !math.IsNaN(term.Lo) && !math.IsNaN(term.Hi) && term.Lo > term.Hi {
					term.Lo, term.Hi = term.Hi, term.Lo
				}
				return term, true
			}
			x, ok := num(v)
			if !ok {
				return term, false
			}
			term.Lo, term.Hi = x, x
			return term, true
		}
		// set / bits / sites / flag
		if fd.ID == "as" {
			v = neFiltAspectExpand(v)
		}
		seen := map[string]bool{}
		for _, m := range strings.Split(v, "+") {
			m = strings.ToLower(strings.TrimSpace(m))
			if m == "" || seen[m] {
				continue
			}
			switch fd.Kind {
			case nfSet, nfBits:
				i := neFiltMember(fd, m)
				if i < 0 {
					return term, false
				}
				m = fd.Members[i]
			case nfSites:
				m = strings.ToUpper(m)
				if len(m) != 9 || !strings.HasPrefix(m, "AT") {
					return term, false
				}
			case nfFlag:
				if m == "1" || m == "true" || m == "yes" {
					term.Flag = true
					return term, true
				}
				return term, false
			}
			seen[m] = true
			term.Set = append(term.Set, m)
		}
		if len(term.Set) == 0 {
			return term, false
		}
		sort.Strings(term.Set)
	default:
		return term, false
	}
	return term, true
}

func neFiltRangeOK(fd *neFiltField) bool { return fd.Kind == nfNum || fd.Kind == nfBucket }

func neFiltMember(fd *neFiltField, m string) int {
	m = strings.ToLower(strings.TrimSpace(m))
	if fd.ID == "as" {
		m = strings.NewReplacer(" ", "", "-", "").Replace(m)
		if a, ok := neFiltAspectAlias[m]; ok {
			m = a
		}
	}
	for i, x := range fd.Members {
		if x != "" && strings.EqualFold(x, m) {
			return i
		}
	}
	// labels are accepted too (chg=confirmed, sub=vineyard, as=south): canonical form keeps the member id
	for i, x := range fd.Labels {
		if x != "" && i < len(fd.Members) && strings.EqualFold(strings.ReplaceAll(x, " ", "-"), strings.ReplaceAll(m, " ", "-")) {
			return i
		}
		if fd.ID == "as" && x != "" && i < len(fd.Members) && strings.NewReplacer(" ", "", "-", "").Replace(strings.ToLower(x)) == m {
			return i
		}
	}
	return -1
}

// neFiltAspectExpand — `nw..ne` (or `300..60` in degrees) → the clockwise arc
// of sectors `nw+n+ne`; other members pass through. Mirrored in the Worker.
func neFiltAspectExpand(v string) string {
	fd := neFiltByID["as"]
	var out []string
	for _, p := range strings.Split(v, "+") {
		q := strings.Index(p, "..")
		if q < 0 {
			out = append(out, p)
			continue
		}
		a, b := neFiltAspectSector(fd, p[:q]), neFiltAspectSector(fd, p[q+2:])
		if a < 0 || b < 0 {
			out = append(out, p) // fails in the member loop → ignored
			continue
		}
		for i := a; ; i = (i + 1) % 8 {
			out = append(out, fd.Members[i])
			if i == b {
				break
			}
		}
	}
	return strings.Join(out, "+")
}

// neFiltAspectSector — a sector name or a bearing in degrees → 0..7 (N…NW), −1 if neither (flat is not on the ring).
func neFiltAspectSector(fd *neFiltField, s string) int {
	s = strings.TrimSpace(s)
	if d, err := strconv.ParseFloat(s, 64); err == nil {
		if math.IsNaN(d) || math.IsInf(d, 0) || d < 0 || d > 360 { // ParseFloat accepts "nan"/"inf" without error
			return -1
		}
		return int(math.Round(d/45)) % 8
	}
	if i := neFiltMember(fd, s); i >= 0 && i < 8 {
		return i
	}
	return -1
}

// neFiltAspectWords — a set of sectors in compass order; a clockwise arc of
// three or more reads `north-west to north-east`. Mirrored in the Worker.
func neFiltAspectWords(fd *neFiltField, set []string) string {
	in := [9]bool{}
	for _, m := range set {
		if i := neFiltMember(fd, m); i >= 0 {
			in[i] = true
		}
	}
	n, start := 0, -1
	for i := 0; i < 8; i++ {
		if in[i] {
			n++
			if !in[(i+7)%8] {
				if start >= 0 {
					start = -2 // more than one arc
				} else {
					start = i
				}
			}
		}
	}
	var ws []string
	if n >= 3 && n < 8 && start >= 0 {
		ws = append(ws, fd.Labels[start]+" to "+fd.Labels[(start+n-1)%8])
	} else {
		for i := 0; i < 8; i++ {
			if in[i] {
				ws = append(ws, fd.Labels[i])
			}
		}
	}
	if in[8] {
		ws = append(ws, fd.Labels[8])
	}
	return strings.Join(ws, " / ")
}

// neFiltPct — a slope in degrees as percent (`15° (27 %)` in words).
func neFiltPct(deg float64) string {
	return neFiltNum(math.Round(math.Tan(deg*math.Pi/180) * 100))
}

// neFiltNum — minimal decimal form.
func neFiltNum(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e9 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func (t neFiltTerm) String() string {
	switch {
	case t.Unk:
		return t.F + "=unk"
	case t.Flag:
		return t.F
	case len(t.Set) > 0:
		return t.F + "=" + strings.Join(t.Set, "+")
	case !math.IsNaN(t.Lo) && !math.IsNaN(t.Hi):
		if t.Lo == t.Hi {
			return t.F + "=" + neFiltNum(t.Lo)
		}
		return t.F + "=" + neFiltNum(t.Lo) + ".." + neFiltNum(t.Hi)
	case !math.IsNaN(t.Lo):
		return t.F + ">=" + neFiltNum(t.Lo)
	case !math.IsNaN(t.Hi):
		return t.F + "<=" + neFiltNum(t.Hi)
	}
	return t.F
}

// Canonical — fields sorted inside a group (then by text), groups in
// creation order, numbers minimal, `k` last.
func (e *neFiltExpr) Canonical() string {
	var gs []string
	for _, g := range e.Groups {
		ts := make([]string, len(g))
		for i, t := range g {
			ts[i] = t.String()
		}
		sort.Strings(ts)
		gs = append(gs, strings.Join(ts, "|"))
	}
	if e.K > 0 {
		gs = append(gs, "k>="+strconv.Itoa(e.K))
	}
	return strings.Join(gs, ",")
}

// Words — the expression in words for the legend.
func (e *neFiltExpr) Words() string {
	var gs []string
	for _, g := range e.Groups {
		var ts []string
		for _, t := range g {
			ts = append(ts, t.Words())
		}
		if len(ts) > 1 {
			gs = append(gs, "("+strings.Join(ts, " or ")+")")
		} else {
			gs = append(gs, ts...)
		}
	}
	if e.K > 0 {
		gs = append(gs, "clusters ≥ "+strconv.Itoa(e.K)+" cells")
	}
	return strings.Join(gs, " · ")
}

func (t neFiltTerm) Words() string {
	fd := neFiltByID[t.F]
	if fd == nil {
		return t.String()
	}
	u := fd.Unit
	if u != "" && u != "%" && u != "°" {
		u = " " + u
	}
	switch {
	case t.Unk:
		return fd.Label + " unknown"
	case t.Flag:
		return fd.Label
	case len(t.Set) > 0:
		if fd.Kind == nfSites {
			return "site " + strings.Join(t.Set, " / ")
		}
		if fd.ID == "as" {
			return fd.Label + " " + neFiltAspectWords(fd, t.Set)
		}
		ws := make([]string, 0, len(t.Set))
		for _, m := range t.Set {
			if i := neFiltMember(fd, m); i >= 0 && i < len(fd.Labels) && fd.Labels[i] != "" {
				ws = append(ws, fd.Labels[i])
			} else if m == "" {
				ws = append(ws, "none")
			} else {
				ws = append(ws, m)
			}
		}
		return fd.Label + " " + strings.Join(ws, " / ")
	case fd.Kind == nfBucket && !math.IsNaN(t.Hi) && math.IsNaN(t.Lo):
		// distance classes: say the bucket's upper edge, which is what the index can tell
		k := neFiltBucketOf(fd.Edges, t.Hi) + 1
		if k > len(fd.Edges)-1 {
			k = len(fd.Edges) - 1
		}
		return fd.Label + " < " + strconv.Itoa(fd.Edges[k]) + u
	case fd.Kind == nfBucket && !math.IsNaN(t.Lo) && math.IsNaN(t.Hi):
		return fd.Label + " ≥ " + strconv.Itoa(fd.Edges[neFiltBucketOf(fd.Edges, t.Lo)]) + u
	case !math.IsNaN(t.Lo) && !math.IsNaN(t.Hi):
		if t.Lo == t.Hi {
			return fd.Label + " = " + neFiltNum(t.Lo) + u + neFiltSlopePct(fd, t.Lo, math.NaN())
		}
		return fd.Label + " " + neFiltNum(t.Lo) + "–" + neFiltNum(t.Hi) + u + neFiltSlopePct(fd, t.Lo, t.Hi)
	case !math.IsNaN(t.Lo):
		return fd.Label + " ≥ " + neFiltNum(t.Lo) + u + neFiltSlopePct(fd, t.Lo, math.NaN())
	case !math.IsNaN(t.Hi):
		return fd.Label + " ≤ " + neFiltNum(t.Hi) + u + neFiltSlopePct(fd, t.Hi, math.NaN())
	}
	return fd.Label
}

// neFiltSlopePct — slopes read in both units: ` (27 %)` / ` (9–27 %)` after the degrees.
func neFiltSlopePct(fd *neFiltField, a, b float64) string {
	if fd.ID != "sl" && fd.ID != "sl90" {
		return ""
	}
	if math.IsNaN(b) {
		return " (" + neFiltPct(a) + " %)"
	}
	return " (" + neFiltPct(a) + "–" + neFiltPct(b) + " %)"
}

// ── evaluation over a bin ────────────────────────────────────────────────────

// neFiltCols — what the evaluator reads: raw columns by field id (u16 as LE
// pairs, i8 as the byte) + the bin header's site list.
type neFiltCols interface {
	col(id string) []byte
	n() int
	sites() []string
}

// rawAt — the raw integer of cell k in a column (dtype-aware).
func neFiltRaw(fd *neFiltField, c []byte, k int) int {
	switch fd.Dtype {
	case "u16":
		return int(c[2*k]) | int(c[2*k+1])<<8
	case "i8":
		return int(int8(c[k]))
	}
	return int(c[k])
}

// neFiltBounds — the physical range of a term as closed raw bounds (the
// quantisation rule the Worker mirrors: lo = ceil, hi = floor).
func neFiltBounds(fd *neFiltField, t neFiltTerm) (lo, hi int) {
	lo, hi = math.MinInt32, math.MaxInt32
	if fd.Kind == nfBucket {
		if !math.IsNaN(t.Lo) {
			lo = neFiltBucketOf(fd.Edges, t.Lo)
		}
		if !math.IsNaN(t.Hi) {
			hi = neFiltBucketOf(fd.Edges, t.Hi)
		}
		return
	}
	sc := fd.Scale
	if sc == 0 {
		sc = 1
	}
	if !math.IsNaN(t.Lo) {
		lo = int(math.Ceil(math.Round((t.Lo-fd.Off)/sc*1e6) / 1e6))
	}
	if !math.IsNaN(t.Hi) {
		hi = int(math.Floor(math.Round((t.Hi-fd.Off)/sc*1e6) / 1e6))
	}
	return
}

// neFiltBucketOf — the class a distance falls in: edges[k] ≤ v < edges[k+1]; the last class is open.
func neFiltBucketOf(edges []int, v float64) int {
	k := 0
	for k+1 < len(edges) && v >= float64(edges[k+1]) {
		k++
	}
	return k
}

// neFiltTermFn — compiles a term to a closure over one column.
func neFiltTermFn(cols neFiltCols, t neFiltTerm) func(k int) bool {
	fd := neFiltByID[t.F]
	if fd == nil {
		return func(int) bool { return false }
	}
	c := cols.col(fd.ID)
	if c == nil {
		// column not in this bin (plane not loaded / older index): unknown everywhere
		if t.Unk {
			return func(int) bool { return true }
		}
		return func(int) bool { return false }
	}
	nd := fd.Nodata
	if t.Unk {
		return func(k int) bool { return neFiltRaw(fd, c, k) == nd }
	}
	switch fd.Kind {
	case nfFlag:
		return func(k int) bool { v := neFiltRaw(fd, c, k); return v != nd && v != 0 }
	case nfSet:
		var mask uint64
		for _, m := range t.Set {
			if i := neFiltMember(fd, m); i >= 0 && i < 64 {
				mask |= 1 << uint(i)
			}
		}
		return func(k int) bool {
			v := neFiltRaw(fd, c, k)
			return v != nd && v >= 0 && v < 64 && mask&(1<<uint(v)) != 0
		}
	case nfBits:
		var mask int
		for _, m := range t.Set {
			if i := neFiltMember(fd, m); i >= 0 {
				mask |= 1 << uint(i)
			}
		}
		return func(k int) bool { v := neFiltRaw(fd, c, k); return v != nd && v&mask != 0 }
	case nfSites:
		if t.Flag {
			return func(k int) bool { v := neFiltRaw(fd, c, k); return v != nd }
		}
		var mask uint64
		for i, code := range cols.sites() {
			for _, m := range t.Set {
				if m == code && i < 64 {
					mask |= 1 << uint(i)
				}
			}
		}
		if mask == 0 {
			return func(int) bool { return false }
		}
		return func(k int) bool { v := neFiltRaw(fd, c, k); return v != nd && v < 64 && mask&(1<<uint(v)) != 0 }
	}
	lo, hi := neFiltBounds(fd, t)
	if fd.Halo {
		// cont: bit 7 = the patch leaves the bin: ≥ 64 for a lower bound, unknown for an upper bound
		return func(k int) bool {
			v := neFiltRaw(fd, c, k)
			if v == nd {
				return false
			}
			if v&128 != 0 {
				return hi == math.MaxInt32 && lo <= 64
			}
			return v >= lo && v <= hi
		}
	}
	zeroNone := fd.ID == "loss" || fd.ID == "snar" || fd.ID == "snar2" // 0 = none: never a value
	return func(k int) bool {
		v := neFiltRaw(fd, c, k)
		if v == nd || (zeroNone && v == 0) {
			return false
		}
		return v >= lo && v <= hi
	}
}

// neFiltEval — the match mask of a bin (1 = matches). `k` is applied by the
// caller (needs the lattice): neFidxBin.eval.
func neFiltEval(cols neFiltCols, e *neFiltExpr) []uint8 {
	n := cols.n()
	out := make([]uint8, n)
	if len(e.Groups) == 0 {
		for k := range out {
			out[k] = 1
		}
		return out
	}
	fns := make([][]func(int) bool, len(e.Groups))
	for gi, g := range e.Groups {
		for _, t := range g {
			fns[gi] = append(fns[gi], neFiltTermFn(cols, t))
		}
	}
	for k := 0; k < n; k++ {
		ok := true
		for _, g := range fns {
			any := false
			for _, f := range g {
				if f(k) {
					any = true
					break
				}
			}
			if !any {
				ok = false
				break
			}
		}
		if ok {
			out[k] = 1
		}
	}
	return out
}
