package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNEWhereParseShare(t *testing.T) {
	for _, raw := range []string{
		"#v=obs;full;trees;relief;f=off:ht=40..55,sl%3E=26.6,as=s;screen;at=1096,2351,33",
		"https://example.test/?dbg#v=obs;full;trees;relief;f=off:ht=40..55,sl%3E=26.6,as=s;screen;at=1096,2351,33",
		"%23v%3Dobs%3Bfull%3Btrees%3Brelief%3Bf%3Doff%3Aht%3D40..55%2Csl%253E%3D26.6%2Cas%3Ds%3Bscreen%3Bat%3D1096%2C2351%2C33",
	} {
		v := neWhereParseShare(raw)
		if v.Profile != "obs" || !v.has("full") || !v.has("trees") || !v.has("screen") || v.at == nil || v.at[2] != 33 {
			t.Errorf("%q → %+v", raw, v)
		}
		if !v.FHeld || v.F != "ht=40..55,sl>=26.6,as=s" {
			t.Errorf("%q f → held=%v %q", raw, v.FHeld, v.F)
		}
	}
	for _, raw := range []string{"#full;obs;at=4882,2441,317", "#observed;full", "#v=observed"} {
		if w := neWhereParseShare(raw); w.Profile != "obs" || len(w.dropped) != 0 {
			t.Errorf("%q → %+v", raw, w)
		}
	}
	v := neWhereParseShare("#pt=47.2,10.7;kg=1234;z=400;bogus")
	if v.pt == nil || v.pt[0] != 10.7 || v.kg != "01234" || v.z != 400 || len(v.dropped) != 1 {
		t.Errorf("%+v", v)
	}
}

func TestNEWhereEndpoint(t *testing.T) {
	if err := neGlyphLoadFrame(); err != nil {
		t.Skip("no glyph frame")
	}
	rr := httptest.NewRecorder()
	handleNEWhere(rr, httptest.NewRequest("GET", "/api/v1/ne/where?pt=10.7644,47.2072&f=sl>=3&n=0&format=txt", nil))
	if rr.Code != 200 {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	b := rr.Body.String()
	for _, want := range []string{"WHERE", "r12  ", "h3   r11", "tiles  0/", "FILTER sl>=3", "peer_cell", "where_txt"} {
		if !strings.Contains(b, want) {
			t.Errorf("txt lacks %q:\n%s", want, b)
		}
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") || rr.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("headers %v", rr.Header())
	}
	rr = httptest.NewRecorder()
	handleNEWhere(rr, httptest.NewRequest("GET", "/api/v1/ne/where", nil))
	if rr.Code != 400 {
		t.Errorf("no point → %d", rr.Code)
	}
	rr = httptest.NewRecorder()
	handleNEWhere(rr, httptest.NewRequest("GET", "/api/v1/ne/where?pt=2,2", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("outside → %d", rr.Code)
	}
}
