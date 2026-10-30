package main

import (
	"encoding/json"
	"math"
	"net/http/httptest"
	"testing"
)

func TestKGBBoxSlivers(t *testing.T) {
	old := [4]float64{1, 1, 2, 2}
	nb := [4]float64{0.5, 0.8, 2.5, 2.1}
	sl := kgBBoxSlivers(old, nb)
	if len(sl) != 4 {
		t.Fatalf("want 4 slivers, got %d: %v", len(sl), sl)
	}
	area := func(b [4]float64) float64 { return (b[2] - b[0]) * (b[3] - b[1]) }
	sum := 0.0
	for _, s := range sl {
		if !bboxContains(nb, s) || area(s) <= 0 {
			t.Fatalf("sliver %v outside new box or empty", s)
		}
		// never overlaps the old box
		if s[0] < old[2] && s[2] > old[0] && s[1] < old[3] && s[3] > old[1] {
			t.Fatalf("sliver %v overlaps old %v", s, old)
		}
		sum += area(s)
	}
	if math.Abs(sum-(area(nb)-area(old))) > 1e-12 {
		t.Fatalf("slivers cover %.6f, want %.6f", sum, area(nb)-area(old))
	}
	if got := kgBBoxSlivers(old, old); len(got) != 0 {
		t.Fatalf("identical boxes → no slivers, got %v", got)
	}
	if got := kgBBoxSlivers([4]float64{}, nb); len(got) != 1 || got[0] != nb {
		t.Fatalf("zero old → whole new box, got %v", got)
	}
	if got := kgBBoxSlivers(old, [4]float64{1.5, 1, 2, 2}); len(got) != 1 {
		t.Fatalf("new not containing old → whole new box, got %v", got)
	}
}

// TestKGUniverseBBoxRevision2 — the committed list carries bbox revision 2:
// every bbox contains the VGD envelope (+pad) AND the previous bbox from the
// committed diff; the diff's bbox_new is what the list serves.
func TestKGUniverseBBoxRevision2(t *testing.T) {
	u, err := LoadKGUniverseFile(kgUniversePath)
	if err != nil {
		t.Fatal(err)
	}
	if u.BBoxRevision != kgUniverseBBoxRevision || u.BBoxSource == "" {
		t.Fatalf("bbox_revision=%d source=%q", u.BBoxRevision, u.BBoxSource)
	}
	vgd, src, err := LoadKGVGDBBoxes(kgVGDBBoxPath)
	if err != nil || len(vgd) != kgUniverseContract || src == "" {
		t.Fatalf("vgd: %v rows=%d src=%q", err, len(vgd), src)
	}
	d, err := LoadKGBBoxDiffFile(kgUniverseBBoxDiffPath)
	if err != nil {
		t.Fatal(err)
	}
	if d.BBoxRevision != u.BBoxRevision || d.UniverseHash != u.UniverseHash || d.KGCount != kgUniverseContract {
		t.Fatalf("diff header %+v", map[string]interface{}{"rev": d.BBoxRevision, "hash": d.UniverseHash, "n": d.KGCount})
	}
	diff := make(map[string]kgBBoxChange, len(d.Changed))
	for _, c := range d.Changed {
		diff[c.KGCode] = c
	}
	for _, r := range u.KGs {
		v, ok := vgd[r.KGCode]
		if !ok {
			t.Fatalf("%s: no VGD row", r.KGCode)
		}
		v = [4]float64{v[0] - kgVGDBBoxPad + 1e-9, v[1] - kgVGDBBoxPad + 1e-9, v[2] + kgVGDBBoxPad - 1e-9, v[3] + kgVGDBBoxPad - 1e-9}
		if !bboxContains(r.BBox, v) {
			t.Fatalf("%s: bbox %v does not contain VGD envelope %v", r.KGCode, r.BBox, v)
		}
		if c, ok := diff[r.KGCode]; ok {
			if c.BBoxNew != r.BBox || !bboxContains(r.BBox, c.BBoxOld) {
				t.Fatalf("%s: diff new=%v old=%v list=%v", r.KGCode, c.BBoxNew, c.BBoxOld, r.BBox)
			}
			if len(c.Slivers) == 0 || c.MaxDelta <= 0 {
				t.Fatalf("%s: empty slivers / delta", r.KGCode)
			}
		}
	}
	// The motivating case: 63308 Gallmannsegg gained its western strip.
	c := diff["63308"]
	if c.BBoxNew[0] > 15.0226 || c.MaxDelta < 0.015 {
		t.Fatalf("63308: %+v", c)
	}
}

func TestKGBBoxChangesHandler(t *testing.T) {
	u, err := LoadKGUniverseFile(kgUniversePath)
	if err != nil {
		t.Fatal(err)
	}
	setKGUniverse(u, nil)
	InitKGBBoxDiff()
	get := func(path string) map[string]interface{} {
		rr := httptest.NewRecorder()
		handleV1KGsBBoxChanges(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != 200 {
			t.Fatalf("%s → %d %s", path, rr.Code, rr.Body.String())
		}
		if rr.Header().Get("X-KG-BBox-Revision") != "2" {
			t.Fatalf("missing revision header")
		}
		var doc struct {
			Data map[string]interface{} `json:"data"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		return doc.Data
	}
	all := get("/api/v1/kgs/bbox_changes?fields=codes")
	if all["bbox_revision"].(float64) != 2 || len(all["codes"].([]interface{})) != int(all["changed_total"].(float64)) {
		t.Fatalf("all: %v", all["count"])
	}
	one := get("/api/v1/kgs/bbox_changes?codes=63308,1004&min_delta=0.01")
	rows := one["changed"].([]interface{})
	if len(rows) < 1 || rows[0].(map[string]interface{})["kg_code"] != "63308" {
		t.Fatalf("filtered: %v", rows)
	}
	big := get("/api/v1/kgs/bbox_changes?min_delta=0.005&fields=codes")
	if n := len(big["codes"].([]interface{})); n == 0 || n >= int(all["changed_total"].(float64)) {
		t.Fatalf("min_delta filter: %d", n)
	}
	rr := httptest.NewRecorder()
	handleV1KGsBBoxChanges(rr, httptest.NewRequest("GET", "/api/v1/kgs/bbox_changes?min_delta=x", nil))
	if rr.Code != 400 {
		t.Fatalf("bad min_delta → %d", rr.Code)
	}
}
