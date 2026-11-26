package main

import (
	"fmt"
	"strings"

	h3 "github.com/uber/h3-go/v4"
)

// Trees under a cell filter (`f=` on mvt/trees tiles, mvt/trees.geojson and
// mvt/ogc items) — the page's rule (apFilt): a tree is shown when the res-12
// cell its top stands in passes the filter; a single `ht` term narrows by the
// tree's own height on top of that (`ht=`, neLayerHtRange). Without this a
// filtered QGIS project drew every crown over the veil.
//
// The verdict is the filter index bin's own mask (b.eval) — the same cells the
// veil tiles and `/ne/filter` count. Bins come through neFidxGet (LRU → disk →
// build); a bin that cannot be had fails the request (503, retried), never an
// unfiltered answer.

type neTreePassBin struct {
	mask   []uint8
	lat    map[uint32]int32
	origin h3.Cell
	oi, oj int
}

type neTreePass struct {
	expr  *neFiltExpr
	canon string
	bins  map[[2]int]*neTreePassBin
	memo  map[h3.Cell]bool
	key   string // the bins' states: part of a cache key
}

// neTreePassFor — nil, nil when f is no filter; the matcher for trees in the box.
func neTreePassFor(f string, w, s, e, n float64) (*neTreePass, error) {
	if strings.TrimSpace(f) == "" || len(f) > 512 {
		return nil, nil
	}
	expr := neFiltParse(f)
	if expr == nil || expr.empty() {
		return nil, nil
	}
	p := &neTreePass{expr: expr, canon: expr.Canonical(), bins: map[[2]int]*neTreePassBin{}, memo: map[h3.Cell]bool{}}
	// a cell is filed by its centre: pad the box by a cell (res 12 ≈ 10 m edge)
	const pad = 0.0003
	i0, j0 := neWhereTileKey(0, w-pad, s-pad)
	i1, j1 := neWhereTileKey(0, e+pad, n+pad)
	if (i1-i0+1)*(j1-j0+1) > 64 {
		return nil, fmt.Errorf("tree filter: box too large")
	}
	var sb strings.Builder
	for i := i0; i <= i1; i++ {
		for j := j0; j <= j1; j++ {
			blob, st, err := neFidxGet(i, j)
			if err != nil {
				return nil, err
			}
			fmt.Fprintf(&sb, "%d.%d.%s,", i, j, st)
			b, err := neFidxDecode(blob)
			if err != nil {
				return nil, err
			}
			if pb := neTreePassBinOf(b, expr); pb != nil {
				p.bins[[2]int{i, j}] = pb
			}
		}
	}
	p.key = sb.String()
	return p, nil
}

// neTreePassBinOf — a decoded bin's verdicts, addressed by local IJ; nil for an empty bin.
func neTreePassBinOf(b *neFidxBin, expr *neFiltExpr) *neTreePassBin {
	if b.N == 0 {
		return nil
	}
	oid, _, err := neParseCell(b.Hdr.Origin)
	if err != nil {
		return nil
	}
	o := h3.Cell(oid)
	oij, err := h3.CellToLocalIJ(o, o)
	if err != nil {
		return nil
	}
	return &neTreePassBin{mask: b.eval(expr), lat: b.lattice(), origin: o, oi: oij.I, oj: oij.J}
}

func (p *neTreePass) pass(lon, lat float64) bool {
	c, err := h3.LatLngToCell(h3.LatLng{Lat: lat, Lng: lon}, 12)
	if err != nil {
		return false
	}
	if v, ok := p.memo[c]; ok {
		return v
	}
	v := false
	clon, clat := neCellCentre(uint64(c))
	i, j := neWhereTileKey(0, clon, clat)
	if b := p.bins[[2]int{i, j}]; b != nil {
		if ij, err := h3.CellToLocalIJ(b.origin, c); err == nil {
			if k, ok := b.lat[ijKey(int32(ij.I-b.oi), int32(ij.J-b.oj))]; ok && int(k) < len(b.mask) {
				v = b.mask[k] != 0
			}
		}
	}
	p.memo[c] = v
	return v
}

// neTreeHtOf — the height window of a tree request: `ht=` when given, else the
// filter's own lone height term (the page's fltTreeRange), so `f=` alone
// answers what the page draws.
func neTreeHtOf(ht, f string) string {
	if ht != "" || strings.TrimSpace(f) == "" || len(f) > 512 {
		return ht
	}
	return neQGSFiltHt(neFiltParse(f).Canonical())
}
