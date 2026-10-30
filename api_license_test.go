package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLicenseHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	setLicenseHeaders(rec, "/api/v1/search/parcel")
	if !strings.Contains(rec.Header().Get("Link"), BEVLicenseURL) {
		t.Fatalf("BEV license link missing: %q", rec.Header().Get("Link"))
	}
	if !strings.Contains(rec.Header().Get("X-Data-Attribution"), "BEV") {
		t.Fatalf("attribution header missing")
	}
	rec = httptest.NewRecorder()
	setLicenseHeaders(rec, "/api/v1/osm/geometry")
	if !strings.Contains(rec.Header().Get("Link"), OSMLicenseURL) {
		t.Fatalf("OSM endpoints must link ODbL: %q", rec.Header().Get("Link"))
	}
	rec = httptest.NewRecorder()
	setLicenseHeaders(rec, "/index.html")
	if rec.Header().Get("Link") != "" {
		t.Fatalf("non-API paths must not get license headers")
	}
}

func TestAttributionMetaOSMNote(t *testing.T) {
	if AttributionMeta(false).OSMNote != "" {
		t.Fatal("no OSM note expected without osm rows")
	}
	if m := AttributionMeta(true); !strings.Contains(m.OSMNote, OSMAttribution) {
		t.Fatalf("OSM note missing: %+v", m)
	}
	rows := []map[string]interface{}{{"parcel_id": "x"}, {"parcel_id": "y", "osm": map[string]interface{}{}}}
	if !dataHasOSM(rows) {
		t.Fatal("dataHasOSM should detect osm block")
	}
	if !strings.Contains(RecommendedAttribution, "bearbeitet") || !strings.Contains(RecommendedAttribution, BEVLicenseURL) {
		t.Fatalf("recommended attribution must carry modified note + license link: %s", RecommendedAttribution)
	}
	_ = http.StatusOK
}
