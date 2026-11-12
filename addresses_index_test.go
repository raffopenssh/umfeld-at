package main

import "testing"

func adxTestIndex() *adxIndex {
	docs := []adxDoc{
		{skz: 1, name: "Handel-Mazzetti-Promenade", gkz: "40201", gem: "Steyr", ort: "Steyr", plz: "4400", lon: 14.42, lat: 48.04, n: 17},
		{skz: 2, name: "Handel-Mazzetti-Straße", gkz: "40101", gem: "Linz", ort: "Linz", plz: "4020", lon: 14.29, lat: 48.30, n: 9},
		{skz: 3, name: "Mariahilfer Straße", gkz: "90001", gem: "Wien", ort: "Wien", osch: "Wien,Neubau", plz: "1070", lon: 16.35, lat: 48.20, n: 211},
		{skz: 4, name: "Hauptstraße", gkz: "40101", gem: "Linz", ort: "Linz", plz: "4040", lon: 14.28, lat: 48.31, n: 80},
		{skz: 5, name: "Am Hof", gkz: "90001", gem: "Wien", ort: "Wien", plz: "1010", lon: 16.37, lat: 48.21, n: 12},
		{skz: 6, name: "Am Hofacker", gkz: "31601", gem: "Staatz", ort: "Ameis", plz: "2141", lon: 16.5, lat: 48.67, n: 9},
		{skz: 7, name: "Rathausplatz", gkz: "30201", gem: "St. Pölten", ort: "St.Pölten", plz: "3100", lon: 15.62, lat: 48.20, n: 21},
		{skz: 8, name: "Stephansplatz", gkz: "90001", gem: "Wien", ort: "Wien", plz: "1010", lon: 16.37, lat: 48.208, n: 10},
		{skz: 9, name: "Dr.-Karl-Renner-Ring", gkz: "90001", gem: "Wien", ort: "Wien", plz: "1010", lon: 16.36, lat: 48.208, n: 3},
		{skz: 10, name: "Wolfgang-Hauser-Straße", gkz: "40201", gem: "Steyr", ort: "Steyr", plz: "4400", lon: 14.43, lat: 48.05, n: 3},
		{skz: 11, name: "Kärntner Straße", gkz: "90001", gem: "Wien", ort: "Wien", plz: "1010", lon: 16.37, lat: 48.205, n: 53},
		{skz: 12, name: "Linzer Straße", gkz: "40101", gem: "Linz", ort: "Linz", plz: "4020", lon: 14.30, lat: 48.29, n: 4},
	}
	plz := map[string]map[string]struct{}{}
	for _, d := range docs {
		if plz[d.plz] == nil {
			plz[d.plz] = map[string]struct{}{}
		}
		plz[d.plz][d.gkz] = struct{}{}
	}
	plz["4020"]["40101"] = struct{}{}
	return adxBuild(docs, plz)
}

func TestAddressesIndexMatch(t *testing.T) {
	ix := adxTestIndex()
	steyr := &[4]float64{14.4, 48.0, 14.45, 48.06}
	for _, tc := range []struct {
		q      string
		bbox   *[4]float64
		want   string // first street
		approx bool
	}{
		{"handel-mazetti-promenade", nil, "Handel-Mazzetti-Promenade", false}, // one z
		{"handel mazetti prom steyr", nil, "Handel-Mazzetti-Promenade", false},
		{"HANDEL MAZZETTI PROMENADE", nil, "Handel-Mazzetti-Promenade", false},
		{"mariahilferstrasse wien", nil, "Mariahilfer Straße", false}, // glued, ss
		{"maria hilfer strasse", nil, "Mariahilfer Straße", false},    // split
		{"mariahilfer str.", nil, "Mariahilfer Straße", false},
		{"mariahilferstra", nil, "Mariahilfer Straße", false}, // being typed
		{"hauptstr 4020 linz", nil, "Hauptstraße", false},     // PLZ of the Gemeinde, not of the street
		{"haupt strasse linz", nil, "Hauptstraße", false},
		{"am hof", nil, "Am Hof", false}, // whole name before the longer one
		{"sankt pölten rathausplatz", nil, "Rathausplatz", false},
		{"st poelten rathauspl", nil, "Rathausplatz", false},
		{"stefansplatz", nil, "Stephansplatz", false},
		{"doktor karl renner ring", nil, "Dr.-Karl-Renner-Ring", false},
		{"karl renner ring", nil, "Dr.-Karl-Renner-Ring", false},
		{"kaerntner strasse 1010", nil, "Kärntner Straße", false},
		{"ha", steyr, "Handel-Mazzetti-Promenade", false}, // first token before "…-Hauser-…"
		{"kärtnerstraße", nil, "Kärntner Straße", true},   // a letter missing
		{"hadnel mazzetti steyr", nil, "Handel-Mazzetti-Promenade", true},
		{"hauptstraße 9999 linz", nil, "Hauptstraße", false},
		{"handel mazzetti promenade linz", nil, "Handel-Mazzetti-Promenade", true}, // one word unanswered
		{"mazzetti 4400", nil, "Handel-Mazzetti-Promenade", false},
		{"promenade 1010", nil, "Handel-Mazzetti-Promenade", true}, // wrong PLZ dropped
	} {
		words, nums, after := addrTokens(tc.q)
		plz, _, _, _ := addrReading(ix, words, nums, after)
		var c *[2]float64
		if tc.bbox != nil {
			c = &[2]float64{(tc.bbox[0] + tc.bbox[2]) / 2, (tc.bbox[1] + tc.bbox[3]) / 2}
		}
		got, approx := ix.search(words, plz, tc.bbox, c)
		if len(got) == 0 {
			t.Errorf("%q: nothing, want %q", tc.q, tc.want)
			continue
		}
		if n := ix.docs[got[0].doc].name; n != tc.want || approx != tc.approx {
			t.Errorf("%q: %q approx=%v, want %q approx=%v", tc.q, n, approx, tc.want, tc.approx)
		}
	}
	for _, q := range []string{"xyzzyq", "qqqq wwww", "linz"} {
		words, _, _ := addrTokens(q)
		got, _ := ix.search(words, "", nil, nil)
		if q == "linz" {
			if len(got) != 3 { // place word: the streets of Linz
				t.Errorf("linz: %d streets, want 3", len(got))
			}
			if ix.docs[got[0].doc].name != "Linzer Straße" { // named after it first
				t.Errorf("linz: first %q", ix.docs[got[0].doc].name)
			}
			continue
		}
		if len(got) != 0 {
			t.Errorf("%q: %d hits, want none", q, len(got))
		}
	}
	// "hauptstraße innsbruck" must not be every street of a place
	words, _, _ := addrTokens("rathausplatz wien")
	got, approx := ix.search(words, "", nil, nil)
	if len(got) != 1 || !approx || ix.docs[got[0].doc].name != "Rathausplatz" {
		t.Errorf("rathausplatz wien: %d hits approx=%v", len(got), approx)
	}
}

func TestAddressesReading(t *testing.T) {
	ix := adxTestIndex()
	for _, tc := range []struct{ q, plz, hnr, alt string }{
		{"hauptstr 12 linz", "", "12", ""},
		{"hauptstr. 12, 4020 linz", "4020", "12", ""},
		{"4020 linz hauptstr 12", "4020", "12", ""},
		{"1010 wien am hof 1", "1010", "1", ""},
		{"stadtplatz 4400", "4400", "", "4400"},
		{"4400 stadtplatz", "4400", "", ""},
		{"1 mai strasse 5", "", "5", ""},
		{"hauptstr 12a/3", "", "12a/3", ""},
	} {
		words, nums, after := addrTokens(tc.q)
		plz, hnr, alt, _ := addrReading(ix, words, nums, after)
		if plz != tc.plz || hnr != tc.hnr || alt != tc.alt {
			t.Errorf("%q: plz=%q hnr=%q alt=%q, want %q %q %q", tc.q, plz, hnr, alt, tc.plz, tc.hnr, tc.alt)
		}
	}
	if !hnrLess("9", "10") || !hnrLess("10", "10a") || hnrLess("11", "10a") {
		t.Error("hnrLess: natural order broken")
	}
}

func TestAdxKey(t *testing.T) {
	for a, b := range map[string]string{
		"Mazzetti": "Mazetti", "Straße": "strasse", "Schloßstraße": "schlossstrasse", "Stephan": "stefan",
		"Mühlgasse": "muehlgase", "Mayer": "maier", "Sankt": "st", "Thalgau": "talgau", "Brücke": "bruke",
	} {
		if x, y := adxKey(adxFold(a)), adxKey(adxFold(b)); x != y {
			t.Errorf("%q → %q, %q → %q", a, x, b, y)
		}
	}
	if d := adxEditPrefix("getriedegase", "getreidegase", 2); d != 1 {
		t.Errorf("transposition: %d", d)
	}
	if d := adxEditPrefix("mazet", "mazetipromenade", 1); d != 0 {
		t.Errorf("prefix: %d", d)
	}
}
