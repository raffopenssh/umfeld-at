package main

import (
	"image"
	"math"
	"testing"
)

// TestNERoadStroke — the signed-distance stroke: full coverage on the line,
// an AA ramp at the edge, nothing a pixel beyond, dashes cut along the arc,
// max-blend never exceeds 1 where segments overlap.
func TestNERoadStroke(t *testing.T) {
	pw, ph := 40, 20
	cov := make([]float32, pw*ph)
	neRoadStroke(cov, pw, ph, [][2]float64{{2, 10}, {38, 10}}, 1.5, 0, 0, 1)
	at := func(x, y int) float32 { return cov[y*pw+x] }
	if at(20, 9) < 0.99 || at(20, 10) < 0.99 { // centre of the 3 px line (y 8.5..11.5 → rows 9,10 full)
		t.Fatalf("centre coverage %v %v", at(20, 9), at(20, 10))
	}
	if c := at(20, 11); c <= 0.3 || c >= 0.7 { // row 11 (centre 11.5) is half inside: ramp
		t.Fatalf("edge ramp = %v, want ~.5", c)
	}
	if at(20, 13) != 0 || at(20, 6) != 0 {
		t.Fatalf("coverage beyond the stroke: %v %v", at(20, 13), at(20, 6))
	}
	// a second pass over the same line must not exceed 1
	neRoadStroke(cov, pw, ph, [][2]float64{{2, 10}, {38, 10}}, 1.5, 0, 0, 1)
	for _, c := range cov {
		if c > 1 {
			t.Fatalf("coverage > 1: %v", c)
		}
	}
	// dashes: 6 on / 4 off from x=2 → on at 2..8, off 8..12, on 12..18
	cov = make([]float32, pw*ph)
	neRoadStroke(cov, pw, ph, [][2]float64{{2, 10}, {38, 10}}, 1.5, 6, 4, 1)
	if at(4, 10) < 0.99 || at(14, 10) < 0.99 {
		t.Fatalf("dash on: %v %v", at(4, 10), at(14, 10))
	}
	if at(10, 10) != 0 {
		t.Fatalf("dash gap painted: %v", at(10, 10))
	}
}

// TestNERoadComposite — src-over keeps the ink colour where coverage is full
// and leaves untouched pixels transparent; the palette quantiser maps
// casing → index mix 0 and ink → mix 7 at the right alpha step.
func TestNERoadComposite(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 4, 1))
	cov := []float32{1, 0.5, 0, 1}
	neRoadComposite(img, cov, neRoadInkRGB, 0.5)
	if img.Pix[3] != 128 || img.Pix[0] != neRoadInkRGB[0] {
		t.Fatalf("full coverage at .5 → alpha %d rgb %d", img.Pix[3], img.Pix[0])
	}
	if img.Pix[4+3] != 64 {
		t.Fatalf("half coverage → alpha %d", img.Pix[4+3])
	}
	if img.Pix[8+3] != 0 {
		t.Fatalf("untouched pixel painted")
	}
	for _, c := range cov {
		if c != 0 {
			t.Fatalf("composite must clear the coverage buffer")
		}
	}
	// casing then ink over it: the ink wins the colour
	cov = []float32{0, 0, 0, 1}
	neRoadComposite(img, cov, neRoadCaseRGB, 0.34)
	cov = []float32{0, 0, 0, 1}
	neRoadComposite(img, cov, neRoadInkRGB, 0.66)
	p := neRoadQuantize(img)
	if p.Pix[2] != 0 {
		t.Fatalf("transparent pixel → palette 0, got %d", p.Pix[2])
	}
	if mix := int(p.Pix[3]) % 8; mix < 6 {
		t.Fatalf("ink over casing → mix near 7, got %d", mix)
	}
	a := int(p.Pix[0]) / 8
	if math.Abs(float64(a)-15.5) > 1 { // alpha 128/255 → step 15 or 16 of 31
		t.Fatalf("alpha step %d for alpha 128", a)
	}
	if len(neRoadPalette) != 256 {
		t.Fatal("palette size")
	}
}

func TestNERoadClassOf(t *testing.T) {
	cases := []struct {
		cat    string
		major  int
		fclass string
		cls    uint8
		ok     bool
	}{
		{"road", 1, "primary", neRoadMajor, true},
		{"road", 0, "residential", neRoadRoad, true},
		{"road", 0, "service", neRoadMinor, true},
		{"road", 0, "pedestrian", neRoadMinor, true},
		{"rail", 0, "rail", neRoadRail, true},
		{"water", 0, "river", 0, false},
	}
	for _, c := range cases {
		cls, ok := neRoadClassOf(c.cat, c.major, c.fclass)
		if ok != c.ok || (ok && cls != c.cls) {
			t.Fatalf("%v → %d %v", c, cls, ok)
		}
	}
	// the attribute hook: ink is a pure function of (class, attr, lod); same inputs, same style
	a := neRoadInk(neRoadMajor, neRoadAttr{Remote: -1}, 1)
	b := neRoadInk(neRoadMajor, neRoadAttr{Remote: -1}, 1)
	if a != b || a.w <= neRoadInk(neRoadRoad, neRoadAttr{Remote: -1}, 1).w {
		t.Fatalf("ink by rank: %+v", a)
	}
	if neRoadInk(neRoadMinor, neRoadAttr{}, 1).dash == 0 {
		t.Fatal("minor ways are dashed")
	}
}

func TestNERoadMetaString(t *testing.T) {
	m := neGlyphRoadsMeta{n: 5, km: 1.26}
	m.cls[neRoadMajor], m.cls[neRoadRail] = 2, 1
	if got := m.String(); got != ";rd=5;rdc=2,0,0,1;km=1.3" {
		t.Fatalf("meta %q", got)
	}
	if got := (&neGlyphRoadsMeta{down: true}).String(); got != ";rd=0;down=1" {
		t.Fatalf("down meta %q", got)
	}
	// clipped length: a 0.01° E–W segment half inside the box
	mLon, _ := metresPerDeg(47)
	if l := clipLenM(15.0, 47.005, 15.01, 47.005, 15.005, 47.0, 15.1, 47.01, mLon, 111000); math.Abs(l-0.005*mLon) > 1 {
		t.Fatalf("clipped length %v want %v", l, 0.005*mLon)
	}
}
