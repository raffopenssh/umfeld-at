package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestBEVNoticeCarriesYear(t *testing.T) {
	n := BEVNotice()
	if !strings.HasPrefix(n, "© BEV, "+strconv.Itoa(bevDataYear())) || !strings.Contains(n, BEVLicenseURL) || !strings.Contains(n, "bearbeitet") {
		t.Fatalf("notice must be „© BEV, JJJJ“ + licence link + modified: %q", n)
	}
	rec := httptest.NewRecorder()
	setLicenseHeaders(rec, "/api/v1/search/parcel")
	hv := rec.Header().Get("X-Data-Attribution")
	if hv != BEVNoticeASCII() || !strings.Contains(hv, strconv.Itoa(bevDataYear())) {
		t.Fatalf("header must carry the ASCII year-bearing notice: %q", hv)
	}
	for _, c := range hv {
		if c > 127 {
			t.Fatalf("header must be ASCII-safe: %q", hv)
		}
	}
}

func TestETagIgnoresQueryTime(t *testing.T) {
	a := []byte(`{"meta":{"total":1,"query_time_ms":8.02,"x":1},"data":[]}`)
	b := []byte(`{"meta":{"total":1,"query_time_ms":13.7,"x":1},"data":[]}`)
	if string(etagNormalize(a)) != string(etagNormalize(b)) {
		t.Fatalf("timing must not affect ETag: %s vs %s", etagNormalize(a), etagNormalize(b))
	}
	if string(etagNormalize(a)) != `{"meta":{"total":1,"x":1},"data":[]}` {
		t.Fatalf("unexpected normalisation: %s", etagNormalize(a))
	}
	c := []byte(`{"meta":{"total":1}}`)
	if string(etagNormalize(c)) != string(c) {
		t.Fatal("bodies without the field must pass through")
	}
}

func TestCachePolicyEndpoint(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cache/policy", nil)
	req.Header.Set("X-Umfeld-Tier", "private") // the BEV notice block is private-tier only
	handleV1CachePolicy(rec, req)
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	lic := body["license"].(map[string]interface{})
	if lic["notice"] != BEVNotice() {
		t.Fatalf("policy notice mismatch: %v", lic["notice"])
	}
	// Uncacheable prefixes in the policy must agree with cacheablePath.
	for _, p := range body["uncacheable_prefixes"].([]interface{}) {
		if cacheablePath(p.(string) + "x") {
			t.Fatalf("policy says %q is uncacheable but cacheablePath disagrees", p)
		}
	}
	if len(cacheClientPy) == 0 || len(cacheClientJS) == 0 {
		t.Fatal("embedded reference clients missing")
	}
}
