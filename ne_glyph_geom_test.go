package main

import (
	"math"
	"testing"
)

// TestNEGlyphGeomExact — the cached tile geometry must reproduce H3's exact
// hexagons: every vertex within 1/1000 px of Cell.Boundary at every LOD, in
// the west, the east and the south of the country (measured: ≤ 1e-4 px).
func TestNEGlyphGeomExact(t *testing.T) {
	if err := neGlyphLoadFrame(); err != nil {
		t.Skip("no frame:", err)
	}
	for li, lod := range neGlyphLODs {
		for _, c := range [][2]float64{{9.6, 47.3}, {17.1, 47.9}, {14.0, 46.5}, {16.4, 48.2}} {
			i, j := int(c[0]/lod.deg), int(c[1]/lod.deg)
			g, _, _, _, _, ok := neGlyphTileGeomOf(lod, i, j)
			if !ok {
				t.Fatalf("lod%d %d/%d: no geom", li, i, j)
			}
			tc := neGlyphTileCellsBuild(lod, i, j)
			if tc == nil || len(tc.ids) == 0 {
				t.Fatalf("lod%d %d/%d: no cells", li, i, j)
			}
			exact := g
			g.cells = tc
			var p1, p2 [8][2]float64
			maxd := 0.0
			for k := 0; k < len(tc.ids); k += 5 {
				np1, cx1, cy1, ok1 := g.poly(tc.ids[k], &p1)
				np2, cx2, cy2, ok2 := exact.poly(tc.ids[k], &p2)
				if !ok1 || !ok2 || np1 != 6 || np2 != 6 {
					t.Fatalf("lod%d cell %x: np %d/%d ok %v/%v", li, tc.ids[k], np1, np2, ok1, ok2)
				}
				if d := math.Hypot(cx1-cx2, cy1-cy2); d > maxd {
					maxd = d
				}
				for v := 0; v < 6; v++ {
					best := math.Inf(1)
					for u := 0; u < 6; u++ {
						if d := math.Hypot(p1[v][0]-p2[u][0], p1[v][1]-p2[u][1]); d < best {
							best = d
						}
					}
					if best > maxd {
						maxd = best
					}
				}
			}
			if maxd > 1e-3 {
				t.Errorf("lod%d %d/%d: max deviation %.2e px", li, i, j, maxd)
			}
			if tc.find(tc.ids[len(tc.ids)/2]) != len(tc.ids)/2 || tc.find(0) != -1 {
				t.Errorf("find broken")
			}
			// the fast cell list is the exact polygonToCells set
			w, s, e, n := neGlyphTileBox(lod, i, j)
			ref := neBBoxCellsPoly(w-neGlyphGeomPad, s-neGlyphGeomPad, e+neGlyphGeomPad, n+neGlyphGeomPad)
			if len(ref) != len(tc.ids) {
				t.Errorf("lod%d %d/%d: fast %d cells, polygonToCells %d", li, i, j, len(tc.ids), len(ref))
			} else {
				for k := range ref {
					if ref[k] != tc.ids[k] {
						t.Errorf("lod%d %d/%d: cell %d differs", li, i, j, k)
						break
					}
				}
			}
		}
	}
}

// TestNEBBoxCellsExact — neBBoxCells (res-10 cover + children) equals polygonToCells
// at res 12 for boxes of every size the service uses, in every corner of the country.
func TestNEBBoxCellsExact(t *testing.T) {
	boxes := [][4]float64{
		{9.53, 47.27, 9.555, 47.295}, {17.15, 48.0, 17.16, 48.01}, {12.7, 46.65, 12.8, 46.75}, {16.3, 48.2, 16.3251, 48.2251},
		{9.9 - neFidxHaloDeg, 47.2 - neFidxHaloDeg, 9.925 + neFidxHaloDeg, 47.225 + neFidxHaloDeg},
		{15.0, 47.0, 15.0003, 47.0003}, {13.0, 47.5, 13.0, 47.5}, {9.0, 46.0, 9.01, 46.01},
	}
	for _, b := range boxes {
		got := neBBoxCells(b[0], b[1], b[2], b[3])
		ref := neBBoxCellsPoly(b[0], b[1], b[2], b[3])
		if len(got) != len(ref) {
			t.Errorf("%v: %d vs %d cells", b, len(got), len(ref))
			continue
		}
		for k := range ref {
			if got[k] != ref[k] {
				t.Errorf("%v: cell %d differs", b, k)
				break
			}
		}
	}
}

func TestNEFindRowFrom(t *testing.T) {
	const rb = 12
	ids := []uint64{3, 5, 5, 9, 20, 21, 50, 51, 52, 1000}
	rows := make([]byte, 0, rb*len(ids))
	for _, id := range ids {
		var b [rb]byte
		for k := 0; k < 8; k++ {
			b[k] = byte(id >> (8 * k))
		}
		rows = append(rows, b[:]...)
	}
	pos := 0
	for q := uint64(0); q < 1100; q++ {
		want := neFindRow(rows, rb, q)
		got := neFindRowFrom(rows, rb, q, &pos)
		if (want == nil) != (got == nil) || (want != nil && &want[0] != &got[0]) {
			t.Fatalf("id %d: want %v got %v", q, want != nil, got != nil)
		}
	}
	pos = 7
	if r := neFindRowFrom(rows, rb, 5, &pos); r == nil {
		t.Fatal("behind the cursor")
	}
}

// TestNEGlyphGeomDiskRoundTrip — the on-disk tile decodes bit-identical.
func TestNEGlyphGeomDiskRoundTrip(t *testing.T) {
	src := neGlyphTileCellsBuild(neGlyphLODs[1], 400, 1900)
	if src == nil || len(src.ids) == 0 {
		t.Skip("no cells")
	}
	got, ok := neGlyphGeomDecode(neGlyphGeomEncode(src))
	if !ok || len(got.ids) != len(src.ids) || got.x0 != src.x0 || got.fh != src.fh || got.off != src.off {
		t.Fatalf("round trip header/len mismatch")
	}
	for k := range src.ids {
		if got.ids[k] != src.ids[k] || got.c[2*k] != src.c[2*k] || got.c[2*k+1] != src.c[2*k+1] {
			t.Fatalf("cell %d differs", k)
		}
	}
}
