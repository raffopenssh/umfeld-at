package main

import (
	"encoding/binary"
	"image"
	"strings"
	"testing"
	"time"

	"github.com/uber/h3-go/v4"
)

func TestNEGlyphFrame(t *testing.T) {
	if err := neGlyphLoadFrame(); err != nil {
		t.Fatal(err)
	}
	f := &neGlyphFrame
	if len(f.cells) < 16000 || f.h <= 0 {
		t.Fatalf("frame: %d cells, h=%d", len(f.cells), f.h)
	}
	for _, c := range f.cells {
		if c.x < 0 || c.x > neGlyphW || c.y < 0 || c.y > f.h {
			t.Fatalf("cell out of frame: %+v", c)
		}
	}
}

func TestNEGlyphBuildEmpty(t *testing.T) {
	g := neGlyphBuild(newNEGlyphAgg(), nil, time.Date(2026, 10, 19, 6, 0, 0, 0, time.UTC))
	if g == nil || g.Held != 0 || len(g.Rows) != len(neGlyphFrame.cells) {
		t.Fatalf("empty build: %+v", g)
	}
	if !strings.Contains(g.SVG, `class="e"`) || strings.Contains(g.SVG, `class="h"`) {
		t.Errorf("empty glyph must be hairline only")
	}
	if !strings.Contains(string(g.JSON), `"held":0`) || !strings.Contains(string(g.JSON), `"cells":[[`) {
		t.Errorf("json: %.200s", g.JSON)
	}
	if h := neGlyphHTML(); !strings.Contains(h, `class="glyph-view"`) || strings.Contains(h, "glyph-twins") || strings.Contains(h, "/k/") {
		t.Errorf("html block: view control present, no twins, no /k/")
	}
	// a view's glyph change is an overlay in the one picture: with no data no overlay path exists
	// (change comes from the derived files on disk, not from the reports: with
	// data/ne_cells present an aggregate-less build still draws it)
	if strings.Contains(g.SVG, `class="o"`) || (g.Chg == 0 && strings.Contains(g.SVG, `class="c"`)) {
		t.Errorf("empty glyph must carry no overlay")
	}
}

func TestNEGlyphTilePalette(t *testing.T) {
	if p := neGlyphTilePalette(); len(p) != 1+9*neGlyphTileShades+1+9 || p[neGlyphIdxUnknown] == nil || p[neGlyphIdxFaded+8] == nil {
		t.Fatalf("palette: %d entries", len(p))
	}
	for _, v := range []string{"obs", "chg"} {
		if p := neGlyphOverlayPalette(v); len(p) < 2 {
			t.Errorf("%s overlay palette: %d", v, len(p))
		}
	}
	var m neGlyphTileMeta
	m.n, m.conf, m.cand, m.dlu[4] = 10, 2, 1, -300
	if s := m.String(5, "chg"); !strings.Contains(s, "chg=2,1") || !strings.Contains(s, "dlu=0,0,0,0,-300") {
		t.Errorf("meta: %s", s)
	}
}

// TestNEGlyphSubParse — the peer's bin layout (6 columns at res 12, 8 at
// res 7) and the painter's palette / masks.
func TestNEGlyphSubParse(t *testing.T) {
	mk := func(n, cols int) []byte {
		b := make([]byte, 4+n*(8+cols))
		b[0] = byte(n)
		for i := 0; i < n; i++ {
			b[4+8*i] = byte(i + 1)
		}
		o := 4 + 8*n
		for c := 0; c < cols; c++ {
			for i := 0; i < n; i++ {
				b[o+c*n+i] = byte(c*10 + i)
			}
		}
		return b
	}
	bin, err := neSubParse(mk(3, 6))
	if err != nil || bin.N != 3 || bin.Cls2 != nil || bin.Cov[2] != 12 || bin.Dcov[1] != 51 {
		t.Fatalf("res-12 bin: %+v %v", bin, err)
	}
	if bin.find(2) != 1 || bin.find(9) != -1 {
		t.Fatalf("find")
	}
	bin, err = neSubParse(mk(2, 8))
	if err != nil || bin.Cls2 == nil || bin.Shr[1] != 71 {
		t.Fatalf("res-7 bin: %+v %v", bin, err)
	}
	if _, err := neSubParse(mk(2, 7)[:4+2*8+13]); err == nil {
		t.Fatalf("ragged bin accepted")
	}
	if neSubKind(4) != 1 || neSubKind(5) != 2 || neSubKind(2) != 3 || neSubKind(0) != 0 {
		t.Fatalf("kind classes")
	}
	p := neGlyphSubPalette()
	if len(p) != 11 || neGlyphSubShade(0) != 1 || neGlyphSubShade(255) != 8 {
		t.Fatalf("palette %d shades %d %d", len(p), neGlyphSubShade(0), neGlyphSubShade(255))
	}
}

// ivp1 — build a small IVP1 body (one square + a gone triangle) for the decoder test.
func ivp1Body(t *testing.T) []byte {
	uv := func(b []byte, v uint64) []byte { return binary.AppendUvarint(b, v) }
	sv := func(b []byte, v int64) []byte { return binary.AppendUvarint(b, uint64((v<<1)^(v>>63))) }
	b := []byte("IVP1")
	b = binary.LittleEndian.AppendUint32(b, 2)
	// square 100..200 units, acker, bio, rel 200, chg 1 (modified), area 4000 m², d -300
	b = append(b, 1, 1, 200, 1, 0)
	b = uv(b, 4000)
	b = sv(b, -300)
	b = uv(b, 1)
	b = uv(b, 4)
	for _, d := range [][2]int64{{100, 100}, {100, 0}, {0, 100}, {-100, 0}} {
		b = sv(b, d[0])
		b = sv(b, d[1])
	}
	// gone triangle, outside the bin on the west (neged x), prev area 900
	b = append(b, 2, 255, 90, 5, 0)
	b = uv(b, 0)
	b = sv(b, -900)
	b = uv(b, 1)
	b = uv(b, 3)
	for _, d := range [][2]int64{{-50, 10}, {-40, 0}, {20, 30}} {
		b = sv(b, d[0])
		b = sv(b, d[1])
	}
	return b
}

func TestNESubPolyParse(t *testing.T) {
	p, err := neSubPolyParse(&neSubRaw{Year: 2025, Prev: 2024, Body: ivp1Body(t)})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Recs) != 2 || p.Year != 2025 {
		t.Fatalf("recs %d", len(p.Recs))
	}
	r := p.Recs[0]
	if r.Cls != 1 || r.Bio != 1 || r.Rel != 200 || r.Chg != 1 || r.Area != 4000 || r.DArea != -300 || len(r.Rings) != 1 || len(r.Rings[0]) != 4 {
		t.Fatalf("rec0 %+v", r)
	}
	if got := r.Rings[0][2]; got != [2]int32{200, 200} {
		t.Fatalf("ring vertex %v", got)
	}
	if !neSubPolyContains(&r, 150, 150) || neSubPolyContains(&r, 50, 150) {
		t.Fatal("point in polygon")
	}
	g := p.Recs[1]
	if g.Chg != 5 || g.DArea != -900 || g.Rings[0][0] != [2]int32{-50, 10} || g.Rings[0][2] != [2]int32{-70, 40} {
		t.Fatalf("rec1 %+v", g)
	}
	// truncated / wrong magic are errors, not panics
	if _, err := neSubPolyParse(&neSubRaw{Body: ivp1Body(t)[:20]}); err == nil {
		t.Fatal("truncated body accepted")
	}
	if _, err := neSubPolyParse(&neSubRaw{Body: []byte("NOPE\x00\x00\x00\x00")}); err == nil {
		t.Fatal("bad magic accepted")
	}
}

func TestNEFillRingsHole(t *testing.T) {
	img := image.NewPaletted(image.Rect(0, 0, 20, 20), neGlyphSubPalette())
	outer := [][2]float64{{2, 2}, {18, 2}, {18, 18}, {2, 18}}
	hole := [][2]float64{{8, 8}, {12, 8}, {12, 12}, {8, 12}}
	neFillRings(img, [][][2]float64{outer, hole}, 3, 0)
	if img.Pix[5*20+5] != 3 || img.Pix[10*20+10] != 0 || img.Pix[0] != 0 {
		t.Fatalf("fill: %d %d %d", img.Pix[5*20+5], img.Pix[10*20+10], img.Pix[0])
	}
	// a polygon far outside the picture paints nothing and does not panic
	neFillRings(img, [][][2]float64{{{-900, -900}, {-800, -900}, {-800, -800}}}, 3, 1)
}

// TestNEGlyphFrameNoInteriorHoles — a res-7 cell with all six neighbours in
// the frame must be in the frame itself (GADM slivers left 25 white,
// untappable hexagons inside Austria on 2026-10-29;
// scripts/ne_glyph_frame_fill_holes.py repairs the file).
func TestNEGlyphFrameNoInteriorHoles(t *testing.T) {
	if err := neGlyphLoadFrame(); err != nil {
		t.Skip(err)
	}
	in := make(map[h3.Cell]bool, len(neGlyphFrame.cells))
	for _, p := range neGlyphFrame.cells {
		in[h3.Cell(p.id)] = true
	}
	var holes []string
	for c := range in {
		ring, _ := c.GridRing(1)
		for _, n := range ring {
			if in[n] {
				continue
			}
			nr, _ := n.GridRing(1)
			full := true
			for _, m := range nr {
				if !in[m] {
					full = false
					break
				}
			}
			if full {
				holes = append(holes, n.String())
			}
		}
	}
	if len(holes) > 0 {
		t.Fatalf("%d interior holes in the glyph frame, run scripts/ne_glyph_frame_fill_holes.py: %v", len(holes), holes)
	}
	if n := len(neGlyphFrame.cells); n != 16424 {
		t.Errorf("frame has %d cells, want 16424 (update the count in ne_glyph.go / docs if the frame changed on purpose)", n)
	}
}

// A cell no report ever touched still paints observed when the bbox of a KG
// that posted an observed layer covers its centre (Gablitz 2026-10-29).
func TestNEGlyphObsWithoutReports(t *testing.T) {
	if err := neGlyphLoadFrame(); err != nil {
		t.Skip(err)
	}
	c := neGlyphFrame.cells[100]
	boxes := neGlyphObsBoxes(map[string]bool{"01902": true, "99999": true},
		[]kgBox{{"01902", c.lon - .01, c.lat - .01, c.lon + .01, c.lat + .01}, {"00001", c.lon - .01, c.lat - .01, c.lon + .01, c.lat + .01}})
	if len(boxes) != 1 || neGlyphObsAt(boxes, c.lon, c.lat) != "01902" || neGlyphObsAt(boxes, c.lon+1, c.lat) != "" {
		t.Fatalf("obs boxes: %+v", boxes)
	}
	r := neGlyphRow{Chunks: 0, Obs: true, AgeH: -1, KG: 3}
	g := &neGlyph{Rows: []neGlyphRow{r, {Chunks: 0, AgeH: -1, KG: -1}}, KGs: []string{"a", "b", "c", "Gablitz"}, KGCodes: []string{"1", "2", "3", "01902"}}
	g.SVG = neGlyphSVG(g)
	if !strings.Contains(g.SVG, `class="o"`) || !strings.Contains(g.SVG, `class="e"`) {
		t.Errorf("svg must carry the observed dot and the hairline: %.300s", g.SVG)
	}
	j := string(neGlyphJSON(g))
	if !strings.Contains(j, `"cells":[[0,0,0,0,0,-1,1,0,3,[`) || !strings.HasSuffix(j, `,[0,0]]}`) {
		t.Errorf("json: %.300s … %s", j, j[len(j)-20:])
	}
}
