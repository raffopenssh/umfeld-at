package main

import (
	"strings"
	"testing"
)

func TestNEWarmCanonHash(t *testing.T) {
	got := neWarmCanonHash("#kg=63330;v=obs;q=Wien%20Mitte;pn=Foo;full;dbg;screen")
	if got != "kg=63330;v=obs;full;screen" {
		t.Fatalf("got %q", got)
	}
	if neWarmCanonHash("#") != "" || neWarmCanonHash("q=x") != "" {
		t.Fatal("empty expected")
	}
}

func TestNEWarmURLOK(t *testing.T) {
	ok := []string{"/api/v1/ne/glyph/tile?lod=1&i=1&j=2&v=obs", "/api/v1/ne/glyph/kgs", "/api/v1/ne/glyph/fidx?b=1"}
	bad := []string{"http://x/api/v1/ne/glyph/tile", "/api/v1/ne/stats/unit?kg=1", "/api/v1/ne/glyph/../x", "/api/v1/ne/glyph/tile?a=\n", "/api/v1/ne/glyph/opened"}
	for _, u := range ok {
		if !neWarmURLOK(u) {
			t.Errorf("should pass: %s", u)
		}
	}
	for _, u := range bad {
		if neWarmURLOK(u) {
			t.Errorf("should fail: %s", u)
		}
	}
}

func TestNEWarmCounterpart(t *testing.T) {
	// framed link: identical
	us := []string{"/api/v1/ne/glyph/tile?lod=1&i=10&j=20&v=obs", "/api/v1/ne/glyph/kgs"}
	if got := neWarmCounterpart("kg=1;v=obs", us, "d"); strings.Join(got, ",") != strings.Join(us, ",") {
		t.Fatalf("framed: %v", got)
	}
	// ;screen desktop 6×3 window → phone taller, narrower-ish, same centre, capped
	var d []string
	for i := 0; i < 6; i++ {
		for j := 0; j < 3; j++ {
			d = append(d, neWarmTile{1, 100 + i, 200 + j, "v=obs"}.url())
		}
	}
	m := neWarmCounterpart("v=obs;screen", d, "d")
	if len(m) == 0 || len(m) > 48 {
		t.Fatalf("phone list %d", len(m))
	}
	jmin, jmax := 1 << 30, 0
	for _, u := range m {
		tt, ok := neWarmParseTile(u)
		if !ok || tt.rest != "v=obs" || tt.lod != 1 {
			t.Fatalf("bad tile %s", u)
		}
		if tt.j < jmin {
			jmin = tt.j
		}
		if tt.j > jmax {
			jmax = tt.j
		}
	}
	if jmax-jmin+1 <= 3 {
		t.Fatalf("phone window not taller: j %d..%d", jmin, jmax)
	}
}

func TestNEWarmRecordStore(t *testing.T) {
	neWarmMu.Lock()
	neWarmLoaded, neWarmEntries = true, map[string]*neWarmEntry{}
	neWarmDirty = true // no deferred save: the store path is the real one
	neWarmMu.Unlock()
	defer func() { neWarmMu.Lock(); neWarmLoaded, neWarmEntries, neWarmDirty = false, nil, false; neWarmMu.Unlock() }()
	if neWarmRecord("#kg=1;v=obs", "x", []string{"/api/v1/ne/glyph/kgs"}) {
		t.Fatal("bad dev accepted")
	}
	if neWarmRecord("#kg=1;v=obs", "d", []string{"http://x/"}) {
		t.Fatal("no usable url accepted")
	}
	if !neWarmRecord("#kg=1;v=obs;q=a", "d", []string{"/api/v1/ne/glyph/kgs", "/api/v1/ne/glyph/kgs"}) {
		t.Fatal("rejected")
	}
	if !neWarmRecord("#kg=1;v=obs", "m", []string{"/api/v1/ne/glyph/kgs"}) {
		t.Fatal("rejected")
	}
	urls, entries, synth := neWarmPlan()
	if entries != 1 || synth != 0 || len(urls) != 1 {
		t.Fatalf("plan %d %d %v", entries, synth, urls)
	}
	e := neWarmEntries["kg=1;v=obs"]
	if e == nil || e.N != 2 || len(e.D) != 1 || len(e.M) != 1 {
		t.Fatalf("entry %+v", e)
	}
}
