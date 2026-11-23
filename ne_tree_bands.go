// ne_tree_bands.go — ONE tree figure set for every export: the unit stats
// (json / flat / csv), the cell tiles and archives at every zoom, the
// cells.geojson hexagons and the QGIS project's legend.
//
//	count · per ha · tallest · mean height · apices per 5 m height band
//
// The bands are the observed-layer peer's (its stand index and national
// table): 3–5 m, then 5 m steps, 55 m and up. Sources, all the same apices
// (LiDAR tree tops ≥ 3 m):
//
//   - the apex height lists of the filter-index bins and of the national
//     sample (ne_tree_h.go) — heights in 0.5 m steps: unit stats, tiles
//     z 12+, any filtered tile. The peer's heights are 0.25 m steps rounded
//     half-even, so a whole-metre value holds x−0.25, x and x+0.25: of the
//     apices sitting exactly on a band edge one third belongs to the band
//     below (bandsOut) — without it every band would lose its top quarter
//     metre to the next one (3–5 m: −13 % against the peer's own bands);
//   - the peer's national table per res-9 hexagon (ne_mvt_nat_trees.go) —
//     band counts and the tallest apex: tiles z 9–11 without a filter. The
//     mean is then Σ band × the band's national mean height (neTreeBandCentM,
//     fitted 2026-11-23 against the stand index: |error| p95 0.3 m per res-9
//     hexagon, 0.1 m per res-7).
package main

import (
	"strconv"
)

const neTreeBands = 12

// neTreeBandEdgesM — lower edges in metres (the last band is open).
var neTreeBandEdgesM = [neTreeBands]int{3, 5, 10, 15, 20, 25, 30, 35, 40, 45, 50, 55}

// neTreeBandCentM — mean apex height inside each band, nationally.
var neTreeBandCentM = [neTreeBands]float64{3.41, 7.11, 12.51, 17.24, 22.43, 27.31, 32.48, 37.26, 42.56, 47.3, 52.4, 57}

// neTreeBandKeys — "3_5" … "50_55", "55_up" (figure key suffixes).
var neTreeBandKeys = func() (k [neTreeBands]string) {
	for b := range k {
		if b == neTreeBands-1 {
			k[b] = strconv.Itoa(neTreeBandEdgesM[b]) + "_up"
		} else {
			k[b] = strconv.Itoa(neTreeBandEdgesM[b]) + "_" + strconv.Itoa(neTreeBandEdgesM[b+1])
		}
	}
	return
}()

// neTreeBandOf — the band of a height in raw 0.5 m units.
func neTreeBandOf(raw int) int {
	if raw < 10 {
		return 0
	}
	return min(raw/10, neTreeBands-1)
}

// neTreeAgg — the tree figures of a set of cells.
type neTreeAgg struct {
	n      int64 // apices (inside the height window when one is set)
	sum    int64 // Σ height, raw 0.5 m — list-counted apices only
	top    int   // tallest counted apex, raw
	bands  [neTreeBands]int64
	edge   [neTreeBands]int64 // list apices exactly on the band's lower edge (band ≥ 1)
	rng    bool               // counted inside a height window
	cells  int                // cells added
	listed int                // … of them counted from an apex list
	est    bool               // bands from the national table: the mean comes from band centroids
}

// addList — the apices of cell k (lo..hi raw when rng).
func (t *neTreeAgg) addList(th *neTreeH, k, lo, hi int, rng bool) {
	t.cells++
	t.listed++
	t.rng = t.rng || rng
	l := th.H[th.Off[k]:th.Off[k+1]]
	for _, h := range l {
		v := int(h)
		if rng && (v < lo || v > hi) {
			continue
		}
		t.n++
		t.sum += int64(v)
		b := neTreeBandOf(v)
		t.bands[b]++
		if b > 0 && v == 2*neTreeBandEdgesM[b] {
			t.edge[b]++
		}
		if v > t.top {
			t.top = v
		}
	}
}

// addCount — a cell without a list: its whole-cell count (the bands stay unknown).
func (t *neTreeAgg) addCount(n int) {
	t.cells++
	t.n += int64(n)
}

func (t *neTreeAgg) scale(f int64) {
	t.n, t.sum = t.n*f, t.sum*f
	t.cells, t.listed = t.cells*int(f), t.listed*int(f)
	for b := range t.bands {
		t.bands[b] *= f
		t.edge[b] *= f
	}
}

// bandsOut — the bands as published: a third of the apices on a band's lower
// edge go to the band below (see the header). Not under a height window: its
// bounds are the same 0.5 m values, the count inside it stays what it is.
func (t *neTreeAgg) bandsOut() [neTreeBands]int64 {
	o := t.bands
	if t.est || t.rng {
		return o
	}
	for b := 1; b < neTreeBands; b++ {
		m := (t.edge[b] + 1) / 3
		o[b] -= m
		o[b-1] += m
	}
	return o
}

// detailed — heights are known for every counted apex.
func (t *neTreeAgg) detailed() bool { return t.est || (t.cells > 0 && t.listed == t.cells) }

// meanM — mean apex height in metres; false without apices or heights.
func (t *neTreeAgg) meanM() (float64, bool) {
	if t.n <= 0 || !t.detailed() {
		return 0, false
	}
	if t.est {
		var s float64
		for b, c := range t.bands {
			s += float64(c) * neTreeBandCentM[b]
		}
		return s / float64(t.n), true
	}
	return float64(t.sum) * 0.5 / float64(t.n), true
}

// mvt — the tile attributes of an aggregate: tn, tn_ha, ht_mean, tn_<edge>
// per non-empty band. rng: a height window is set — a count that is not
// list-exact would be the wrong population and is left out.
func (t *neTreeAgg) mvt(f *mvtFeature, areaHa float64, rng bool) {
	if t.cells == 0 && !t.est || rng && !t.detailed() {
		return
	}
	f.i64("tn", t.n)
	if t.n <= 0 {
		return
	}
	if areaHa > 0 {
		f.num("tn_ha", neRound(float64(t.n)/areaHa, 1))
	}
	if m, ok := t.meanM(); ok {
		f.num("ht_mean", neRound(m, 1))
		for b, c := range t.bandsOut() {
			if c > 0 {
				f.i64("tn_"+strconv.Itoa(neTreeBandEdgesM[b]), c)
			}
		}
	}
}

// figures — the unit-stats keys beside trees_n: trees.per_ha,
// trees.height_mean_m, trees.tallest_m (exact counts only: the tallest of a
// sample is not the tallest of the unit), trees_height_n.<band>.
func (t *neTreeAgg) figures(f map[string]interface{}, areaHa float64, approx bool) {
	if !t.detailed() {
		return
	}
	if areaHa > 0 {
		f["trees.per_ha"] = neRound(float64(t.n)/areaHa, 1)
	}
	if m, ok := t.meanM(); ok {
		f["trees.height_mean_m"] = neRound(m, 1)
		if !approx {
			f["trees.tallest_m"] = float64(t.top) * 0.5
		}
	}
	for b, c := range t.bandsOut() {
		f["trees_height_n."+neTreeBandKeys[b]] = c
	}
}

// neMVTTreeFields — the attribute names above, for TileJSON / archive metadata.
func neMVTTreeFields(fields map[string]string) {
	fields["tn_ha"], fields["ht_mean"] = "Number", "Number"
	for _, e := range neTreeBandEdgesM {
		fields["tn_"+strconv.Itoa(e)] = "Number"
	}
}
