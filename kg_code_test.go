package main

import (
	"net/http/httptest"
	"testing"
)

func TestNormalizeKGCode(t *testing.T) {
	cases := map[string]string{
		"1503":  "01503", // the wart: EDM register spelling → canonical
		"01503": "01503", // already canonical
		"7201":  "07201",
		"63349": "63349",
		"":      "",
		"63":    "63",   // typeahead prefix — must NOT become 00063
		"635":   "635",  // ditto
		"Wien":  "Wien", // a name, not a code
		"850a":  "850a", // not numeric
		"85801": "85801",
	}
	for in, want := range cases {
		if got := NormalizeKGCode(in); got != want {
			t.Errorf("NormalizeKGCode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeObjectID(t *testing.T) {
	cases := map[string]string{
		"1503-601/1":  "01503-601/1",
		"01503-601/1": "01503-601/1",
		"1503_fp_9":   "01503_fp_9",
		"63349-348/6": "63349-348/6",
		"1503":        "01503",
		"":            "",
		"abc-1":       "abc-1",
	}
	for in, want := range cases {
		if got := NormalizeObjectID(in); got != want {
			t.Errorf("NormalizeObjectID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeKGCodeList(t *testing.T) {
	if got := NormalizeKGCodeList("1503,63349, 7201"); got != "01503,63349,07201" {
		t.Errorf("got %q", got)
	}
	if got := NormalizeKGCodeList("63349"); got != "63349" {
		t.Errorf("unchanged input should be returned verbatim, got %q", got)
	}
}

func TestNormalizeRequestParam(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"kg", "1503", "01503"},
		{"kg", "1503,7201", "01503,07201"},
		{"plz", "8580", "8580"},      // a PLZ is 4 digits and must never be padded
		{"gemeinde", "1503", "1503"}, // gemeinde codes are their own namespace
		{"ez", "1234", "1234"},
		{"gnr", "1234", "1234"},
		{"parcel_id", "1503-601/1", "01503-601/1"},
		{"ids", "1503-601/1,7201-5", "01503-601/1,07201-5"},
		{"id", "1503", "1503"}, // ambiguous: bare code on /search/municipalities
		{"id", "1503-601/1", "01503-601/1"},
		{"limit", "1503", "1503"},
	}
	for _, c := range cases {
		if got := NormalizeRequestParam(c.name, c.in); got != c.want {
			t.Errorf("NormalizeRequestParam(%q,%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestNormalizeKGParamsRewritesRequest(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/v1/query?kg=1503&limit=5&plz=8580", nil)
	w := httptest.NewRecorder()
	normalizeKGParams(r, w)
	if got := r.URL.Query().Get("kg"); got != "01503" {
		t.Errorf("kg = %q, want 01503", got)
	}
	if got := r.URL.Query().Get("plz"); got != "8580" {
		t.Errorf("plz must be untouched, got %q", got)
	}
	if got := r.URL.Query().Get("limit"); got != "5" {
		t.Errorf("limit = %q", got)
	}
	if w.Header().Get(kgNormalizedHeader) != "true" {
		t.Error("expected the advisory header on a rewritten request")
	}

	// A canonical request must not be rewritten at all (no header, no reorder).
	r2 := httptest.NewRequest("GET", "/api/v1/query?kg=63349&limit=5", nil)
	w2 := httptest.NewRecorder()
	normalizeKGParams(r2, w2)
	if r2.URL.RawQuery != "kg=63349&limit=5" {
		t.Errorf("RawQuery should be untouched, got %q", r2.URL.RawQuery)
	}
	if w2.Header().Get(kgNormalizedHeader) != "" {
		t.Error("no header expected for a canonical request")
	}
}
