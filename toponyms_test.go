package main

import "testing"

func TestNormalizeToponym(t *testing.T) {
	cases := map[string]string{
		"Grünalm":               "gruenalm",
		"Hintere Lignitz-Almen": "hintere lignitz almen",
		"AGRARGEM. LIEGNITZALM": "agrargem liegnitzalm",
		"Großglockner":          "grossglockner",
		"Čepić":                 "cepic",
	}
	for in, want := range cases {
		if got := normalizeToponym(in); got != want {
			t.Errorf("normalizeToponym(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStemToponym(t *testing.T) {
	cases := map[string]string{
		"lignitzalpe":          "lignitz",
		"lignitzer alm":        "lignitz",
		"hintere lignitzalmen": "lignitz",
		"liegnitzalm":          "liegnitz",
		"pflegalm":             "pfleg",
		"grossau":              "", // "gross" is generic → no stem
		"liebach":              "", // "lie" too short
		"alm":                  "", // nothing specific left
		"lignitz winkl":        "", // nothing generic to strip
		"ennslehenhuette":      "ennslehen",
	}
	for in, want := range cases {
		if got := stemToponym(in); got != want {
			t.Errorf("stemToponym(%q) = %q, want %q", in, got, want)
		}
	}
}

// Build a tiny in-memory index and check the fuzzy/stem ranking contract.
func testToponymIndex() *toponymIndex {
	idx := &toponymIndex{tri: map[[3]byte][]uint32{}, kgByCode: map[string]uint16{}, byKG: map[string]int{}}
	idx.kgs = []toponymKG{{}, {code: "58039", gemeindeCode: "50506"}, {code: "60349", gemeindeCode: "61111"}}
	add := func(name, variant string, layer uint8, kg uint16, lon, lat float32) {
		ei := uint32(len(idx.entries))
		idx.entries = append(idx.entries, toponymEntry{rowid: uint32(ei + 1), layer: layer, kg: kg, lon: lon, lat: lat})
		nn := normalizeToponym(name)
		idx.addName(nn, ei, 0)
		if st := stemToponym(nn); st != "" {
			idx.addName(st, ei, 3)
		}
		if variant != "" {
			idx.addName(normalizeToponym(variant), ei, 1)
		}
	}
	add("Lignitzalpe", "", 6, 1, 13.70, 47.245)                   // 0 ried
	add("Hintere Lignitzalmen", "", 0, 1, 13.709, 47.224)         // 1 siedlung
	add("Lignitz", "", 6, 1, 13.716, 47.203)                      // 2 ried
	add("Pflegalm", "", 0, 2, 15.0, 47.52)                        // 3
	add("Liebach", "", 6, 2, 15.1, 47.9)                          // 4 — must NOT match Liegnitz*
	add("Grünalm", "", 0, 2, 12.6, 47.2)                          // 5
	add("Grünberg", "", 2, 2, 12.7, 47.3)                         // 6
	add("Ennslehenhütte", "Hintere Marbachalm", 0, 1, 13.4, 47.3) // 7 variant
	return idx
}

func TestToponymSearchFuzzyAndStem(t *testing.T) {
	idx := testToponymIndex()
	q := &ToponymQuery{Q: "Liegnitzalm", Stem: true, MinScore: toponymsDefaultMinScore}
	hits := idx.search(q)
	if len(hits) == 0 {
		t.Fatalf("misspelt 'Liegnitzalm' must still find the Lignitz names")
	}
	for _, h := range hits {
		if h.entry.rowid == 5 {
			t.Errorf("'Liebach' must not match 'Liegnitzalm' (short stem leak), score %.3f", h.score)
		}
	}
	names := map[uint32]bool{}
	for _, h := range hits {
		names[h.entry.rowid] = true
	}
	for _, want := range []uint32{1, 2, 3} {
		if !names[want] {
			t.Errorf("expected rowid %d among hits: %+v", want, hits)
		}
	}
	// Exact literal outranks stem-only
	q2 := &ToponymQuery{Q: "Grünalm", Stem: true, MinScore: toponymsDefaultMinScore}
	h2 := idx.search(q2)
	if len(h2) == 0 || h2[0].entry.rowid != 6 || h2[0].score != 1.0 {
		t.Fatalf("exact 'Grünalm' must rank first with score 1.0: %+v", h2)
	}
	for _, h := range h2[1:] {
		if h.score >= 0.9 {
			t.Errorf("stem-only hit (Grünberg) must score < 0.9, got %.3f", h.score)
		}
	}
	// Variant names are searchable
	h3 := idx.search(&ToponymQuery{Q: "Hintere Marbachalm", Stem: true, MinScore: 0.5})
	if len(h3) == 0 || h3[0].entry.rowid != 8 || h3[0].matched != "variant_name" {
		t.Fatalf("variant name must resolve: %+v", h3)
	}
	// Filters apply
	h4 := idx.search(&ToponymQuery{Q: "Lignitz", Stem: true, MinScore: 0.5, KG: map[string]bool{"60349": true}})
	if len(h4) != 0 {
		t.Errorf("kg filter must exclude the Mariapfarr rows, got %d", len(h4))
	}
	h5 := idx.search(&ToponymQuery{Q: "Lignitz", Stem: true, MinScore: 0.5, Layers: map[string]bool{"ried": true}})
	for _, h := range h5 {
		if h.entry.layer != 6 {
			t.Errorf("layer filter leaked layer %d", h.entry.layer)
		}
	}
}

func TestToponymFallbackQueries(t *testing.T) {
	alts := toponymFallbackQueries("AGRARGEM. LIEGNITZALM")
	if len(alts) == 0 || alts[0].q != "liegnitzalm" {
		t.Fatalf("org stopword must be stripped: %+v", alts)
	}
	if alts := toponymFallbackQueries("Lignitz"); alts != nil {
		t.Errorf("single word has no fallback: %+v", alts)
	}
}

func TestToponymNearFilter(t *testing.T) {
	idx := testToponymIndex()
	q := &ToponymQuery{HasNear: true, NearLon: 13.70, NearLat: 47.245, RadiusM: 3000}
	hits := idx.filter(q, 0)
	if len(hits) != 2 { // Lignitzalpe (0 m) + Hintere Lignitzalmen (~2.4 km)
		t.Fatalf("expected 2 hits within 3 km, got %d", len(hits))
	}
	if hits[0].entry.rowid != 1 || hits[0].distance > 1 {
		t.Errorf("nearest first: %+v", hits)
	}
}
