package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/paulmach/orb/encoding/mvt"
	"github.com/uber/h3-go/v4"
)

func mvtDecodeGz(t *testing.T, gz []byte) *mvt.Layer {
	t.Helper()
	if len(gz) == 0 {
		return &mvt.Layer{}
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(zr)
	layers, err := mvt.Unmarshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) == 0 {
		return &mvt.Layer{}
	}
	return layers[0]
}

// TestNEMVTFilter — `f=` on a tile: at res 12 exactly the matching cells
// remain; below, a hexagon keeps n = matching children, n_all = every child,
// and hexagons without a match are dropped. Same mask as /ne/filter (bin.eval).
func TestNEMVTFilter(t *testing.T) {
	if err := neGlyphLoadFrame(); err != nil {
		t.Skip("frame:", err)
	}
	b := newNEFiltTestBin(5)
	origin, _ := neParseCellHex(b.Hdr.Origin)
	ids := neMVTBinIDs(b, "t")
	// the synthetic bin's polynomial is a stand-in: refit it to the true cell positions so the res-12 path (fitted centres) is exact too
	cells := make([]neFidxCell, b.N)
	ord := make([]int, b.N)
	for k := range cells {
		lon, lat := neCellCentre(ids[k])
		cells[k] = neFidxCell{i: b.IJ[2*k], j: b.IJ[2*k+1], lon: lon, lat: lat}
		ord[k] = k
	}
	if err := neFidxFit(b, cells, ord, h3.Cell(origin)); err != nil {
		t.Fatal(err)
	}
	w, s, e, n := 999.0, 999.0, -999.0, -999.0
	for k := range ids {
		lon, lat := neCellCentre(ids[k])
		w, s, e, n = min(w, lon), min(s, lat), max(e, lon), max(n, lat)
	}
	b.Hdr.BBox = [4]float64{w, s, e, n}
	blob := b.encode("all")
	ll, _ := h3.Cell(neH3ParentFast(ids[b.N/2], 10)).LatLng()
	expr := neFiltParse("d=wald,sl>=10")
	mask := b.eval(expr)
	for _, z := range []int{12, 13} {
		res := neMVTZoomRes(z)
		x0, y0, _, _ := mvtTilesInBox(z, ll.Lng, ll.Lat, ll.Lng, ll.Lat)
		bins := [][2]int{{0, 0}}
		full, _ := neMVTRenderFromBlobs(z, x0, y0, bins, [][]byte{blob}, []string{"t"}, nil, nil)
		filt, _ := neMVTRenderFromBlobs(z, x0, y0, bins, [][]byte{blob}, []string{"t"}, nil, expr)
		lf, lt := mvtDecodeGz(t, full), mvtDecodeGz(t, filt)
		if len(lf.Features) == 0 {
			t.Fatalf("z%d: no features in the unfiltered tile", z)
		}
		nFull := map[string]int{}
		for _, f := range lf.Features {
			nFull[fmt.Sprint(f.Properties["h3"])] = int(f.Properties["n"].(float64))
			if _, ok := f.Properties["n_all"]; ok {
				t.Errorf("z%d: n_all without a filter", z)
			}
		}
		want := map[uint64]int{}
		for k := range ids {
			if ids[k] != 0 && mask[k] == 1 {
				want[neH3ParentFast(ids[k], res)]++
			}
		}
		seen := 0
		for _, f := range lt.Features {
			h := fmt.Sprint(f.Properties["h3"])
			n, nAll := int(f.Properties["n"].(float64)), int(f.Properties["n_all"].(float64))
			if nAll != nFull[h] {
				t.Errorf("z%d %s: n_all %d ≠ unfiltered n %d", z, h, nAll, nFull[h])
			}
			id, _ := neParseCellHex(h)
			if n != want[id] || n == 0 {
				t.Errorf("z%d %s: n %d ≠ matching children %d", z, h, n, want[id])
			}
			if mp := f.Properties["match_pct"].(float64); mp <= 0 || mp > 100 {
				t.Errorf("z%d %s: match_pct %v", z, h, mp)
			}
			seen++
		}
		for id, n := range want {
			if _, ok := nFull[neCellHex(id)]; ok && n > 0 {
				seen--
			}
		}
		if seen != 0 {
			t.Errorf("z%d: filtered feature set ≠ hexagons with a match (%d off)", z, seen)
		}
	}
	// res 12: the cell set is the mask
	z := 14
	x0, y0, x1, y1 := mvtTilesInBox(z, b.Hdr.BBox[0], b.Hdr.BBox[1], b.Hdr.BBox[2], b.Hdr.BBox[3])
	var gotFull, gotFilt int
	for x := x0; x <= x1; x++ {
		for y := y0; y <= y1; y++ {
			full, _ := neMVTRenderFromBlobs(z, x, y, [][2]int{{0, 0}}, [][]byte{blob}, []string{"t"}, nil, nil)
			filt, _ := neMVTRenderFromBlobs(z, x, y, [][2]int{{0, 0}}, [][]byte{blob}, []string{"t"}, nil, expr)
			lf, lt := mvtDecodeGz(t, full), mvtDecodeGz(t, filt)
			gotFull += len(lf.Features)
			for _, f := range lt.Features {
				id, _ := neParseCellHex(fmt.Sprint(f.Properties["h3"]))
				k := -1
				for q := range ids {
					if ids[q] == id {
						k = q
					}
				}
				if k < 0 || mask[k] != 1 {
					t.Errorf("z14: cell %x in the filtered tile does not match", id)
				}
				gotFilt++
			}
		}
	}
	wantMatch := 0
	for k := range mask {
		wantMatch += int(mask[k])
	}
	if gotFull == 0 || gotFilt == 0 || gotFilt >= gotFull || gotFilt > wantMatch {
		t.Errorf("z14: full %d filtered %d matching %d", gotFull, gotFilt, wantMatch)
	}
	// TileJSON carries the filter in the tile URL and the extra fields
	rr := httptest.NewRecorder()
	handleNEMVT(rr, httptest.NewRequest("GET", "/api/v1/ne/mvt.json?f=d%3Dwald%2Csl%3E%3D10", nil), "mvt.json")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "{z}/{x}/{y}.pbf?f=") || !strings.Contains(rr.Body.String(), `"n_all"`) || !strings.Contains(rr.Body.String(), `"match_pct"`) {
		t.Errorf("mvt.json?f=: %d %s", rr.Code, rr.Body.String())
	}
	// NEI1 cell list body for the peers
	body := neStatsCellsBody([]uint64{3, 1, 2})
	zr, _ := gzip.NewReader(bytes.NewReader(body))
	raw, _ := io.ReadAll(zr)
	if string(raw[:4]) != "NEI1" || len(raw) != 8+24 || raw[4] != 3 || raw[8] != 1 || raw[16] != 2 || raw[24] != 3 {
		t.Errorf("NEI1 body: %x", raw)
	}
}
