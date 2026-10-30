// api_ne_vocab.go — the public-vocabulary rename table and word ban used by
// the NE public surface. Kept out of the ban-checked files because it must
// spell the banned words (TestNEPublicWordBan reads it, never greps it).
package main

import (
	"regexp"
	"strings"
)

// nePublicWordRe — the banned vocabulary of the public NE surface (case-insensitive).
var nePublicWordRe = regexp.MustCompile(`(?i)cadastre|kataster|parcel|grundstück|gnr|einlage|\bez\b|footprint|bev_tiles`)

// nePublicRenames — stored → public vocabulary, applied to the contract text.
var nePublicRenames = []struct {
	re *regexp.Regexp
	to string
}{
	{regexp.MustCompile(`<private>/api/v1/`), "/api/v1/"},
	{regexp.MustCompile(`<private>/`), "/"},
	{regexp.MustCompile(`UMFELD_TOKEN`), "API_TOKEN"},
	{regexp.MustCompile(`n_parc`), "n_units"},
	{regexp.MustCompile(`parc_p(10|50|90)`), "unit_p$1"},
	{regexp.MustCompile(`parc_per_ez`), "units_per_reg"},
	{regexp.MustCompile(`line_m_ha`), "edge_m_ha"},
	{regexp.MustCompile(`bld_cover`), "str_cover"},
	{regexp.MustCompile(`bld_n\b`), "str_n"},
	{regexp.MustCompile(`bld_p50`), "str_p50"},
	{regexp.MustCompile(`ez_n`), "reg_n"},
	{regexp.MustCompile(`(?i)bev_tiles`), "bev tiles"},
	{regexp.MustCompile(`Grundstücksnummern?`), "unit numbers"},
	{regexp.MustCompile(`(?i)grundstücks?`), "unit"},
	{regexp.MustCompile(`(?i)einlagezahl(en)?`), "register number$1"},
	{regexp.MustCompile(`(?i)einlage`), "register"},
	{regexp.MustCompile(`Cadastre|Kataster`), "Survey"},
	{regexp.MustCompile(`cadastre|kataster`), "survey"},
	{regexp.MustCompile(`(?i)cadastral`), "surveyed"},
	{regexp.MustCompile(`Parcels`), "Units"},
	{regexp.MustCompile(`parcels`), "units"},
	{regexp.MustCompile(`Parcel`), "Unit"},
	{regexp.MustCompile(`parcel`), "unit"},
	{regexp.MustCompile(`Footprints`), "Structures"},
	{regexp.MustCompile(`footprints`), "structures"},
	{regexp.MustCompile(`Footprint`), "Structure"},
	{regexp.MustCompile(`footprint`), "structure"},
	{regexp.MustCompile(`GNR`), "unit number"},
	{regexp.MustCompile(`\bgnr\b`), "unit number"},
	{regexp.MustCompile(`\bEZ\b`), "register"},
	{regexp.MustCompile(`\bez\b`), "register"},
}

// nePublicBannedWords — every banned match in s (for tests and self-checks).
func nePublicBannedWords(s string) []string {
	m := nePublicWordRe.FindAllString(s, -1)
	seen := map[string]bool{}
	var out []string
	for _, x := range m {
		x = strings.ToLower(x)
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// neStatKeyPublic renames stats{} keys by the same vocabulary.
func neStatKeyPublic(k string) string {
	for _, r := range nePublicRenames[1:] {
		k = r.re.ReplaceAllString(k, r.to)
	}
	return neFieldName(true, k)
}
