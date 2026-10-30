// ne_h3.go — H3 resolution helpers for the NE read path (docs/ne-public-api.md §4).
//
// Lookup only: lon/lat → res-12 cell, cell → centre, k-ring, bbox → res-9
// chunk keys. Record CONTENTS always come from the python reference package;
// this file never computes a cell value. h3-go v4 and python h3 4.5.0 share
// the v4 indexing, so latLngToCell agrees bit for bit (TestNEH3Fixture).
package main

import (
	"fmt"
	"slices"
	"strconv"

	"github.com/uber/h3-go/v4"
)

const (
	neCellRes  = 12 // LU / OBS rows
	neKRes     = 10 // K rows
	neChunkRes = 9  // store chunk key (343 res-12 children)
)

// neParseCell parses a 15-hex H3 id, returning the cell and its resolution.
func neParseCell(s string) (uint64, int, error) {
	if len(s) < 8 || len(s) > 16 {
		return 0, 0, fmt.Errorf("invalid H3 id %q", s)
	}
	v, err := strconv.ParseUint(s, 16, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid H3 id %q", s)
	}
	c := h3.Cell(v)
	if !c.IsValid() {
		return 0, 0, fmt.Errorf("invalid H3 id %q", s)
	}
	return v, c.Resolution(), nil
}

func neCellHex(id uint64) string { return strconv.FormatUint(id, 16) }

func neCellRes0(id uint64) int { return int((id >> 52) & 0xF) }

// neLatLngToCell — res-12 (or given res) cell containing the point.
func neLatLngToCell(lon, lat float64, res int) (uint64, error) {
	c, err := h3.LatLngToCell(h3.NewLatLng(lat, lon), res)
	if err != nil {
		return 0, err
	}
	return uint64(c), nil
}

// neCellCentre — (lon, lat) of the cell centre.
func neCellCentre(id uint64) (lon, lat float64) {
	ll, _ := h3.Cell(id).LatLng()
	return ll.Lng, ll.Lat
}

// neGridDisk — the cell and its k rings.
func neGridDisk(id uint64, k int) []uint64 {
	cs, _ := h3.Cell(id).GridDisk(k)
	out := make([]uint64, 0, len(cs))
	for _, c := range cs {
		out = append(out, uint64(c))
	}
	return out
}

// neChildren — children at a finer resolution.
func neChildren(id uint64, res int) []uint64 {
	cs, _ := h3.Cell(id).Children(res)
	out := make([]uint64, 0, len(cs))
	for _, c := range cs {
		out = append(out, uint64(c))
	}
	return out
}

// neBBoxChunks — res-9 cells whose polygon-fill covers the bbox, plus one
// ring so children poking over the fill edge are not lost.
func neBBoxChunks(w, s, e, n float64) []uint64 {
	poly := h3.GeoPolygon{GeoLoop: h3.GeoLoop{
		h3.NewLatLng(s, w), h3.NewLatLng(n, w), h3.NewLatLng(n, e), h3.NewLatLng(s, e),
	}}
	cs, _ := h3.PolygonToCells(poly, neChunkRes)
	if len(cs) == 0 {
		if c, err := h3.LatLngToCell(h3.NewLatLng((s+n)/2, (w+e)/2), neChunkRes); err == nil {
			cs = []h3.Cell{c}
		}
	}
	set := make(map[uint64]struct{}, len(cs)*2)
	for _, c := range cs {
		for _, d := range neGridDisk(uint64(c), 1) {
			set[d] = struct{}{}
		}
	}
	out := make([]uint64, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sortU64(out)
	return out
}

// neBBoxCells — every res-12 cell whose centre lies in the box, from ONE
// polygon fill (H3 v4 default containment = centre). Replaces "children of
// every chunk + one cgo centre call per cell": a 0.02° box is ~11 k cells,
// so that was 11 k cgo round-trips (≈30 % of the request CPU) per answer.
func neBBoxCells(w, s, e, n float64) []uint64 {
	poly := h3.GeoPolygon{GeoLoop: h3.GeoLoop{
		h3.NewLatLng(s, w), h3.NewLatLng(n, w), h3.NewLatLng(n, e), h3.NewLatLng(s, e),
	}}
	cs, _ := h3.PolygonToCells(poly, neCellRes)
	out := make([]uint64, len(cs))
	for i, c := range cs {
		out[i] = uint64(c)
	}
	sortU64(out)
	return out
}

// neCentreInBBox — is the cell centre inside the closed box?
func neCentreInBBox(id uint64, w, s, e, n float64) bool {
	lon, lat := neCellCentre(id)
	return lon >= w && lon <= e && lat >= s && lat <= n
}

func sortU64(a []uint64) { slices.Sort(a) }
