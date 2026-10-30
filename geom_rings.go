// geom_rings.go — (Multi)Polygon helpers shared by the OSM water-area overlay:
// ring classification (outer/hole), WKB (de)serialisation, orb ↔ polyclip
// conversion and rectangle clipping.
package main

import (
	"fmt"
	"math"
	"sort"

	"github.com/ctessum/polyclip-go"
	"github.com/paulmach/orb"
	"github.com/paulmach/orb/encoding/wkb"
	"github.com/paulmach/orb/planar"
)

// coordsToPolygons parses one GeoJSON polygon coordinate array into the set of
// well-formed polygons it actually describes.
//
// WHY this is not just "ring 0 = shell, rest = holes": the BEV tile source hands
// us an UNORDERED ring bag. For a measured 163 of 361 multi-ring parcels the
// first ring is not the outer one — a small ring is emitted first and the real
// shell lands in the "holes" list. Taking ring 0 as the shell then yields a
// self-inconsistent polygon: shapely reports is_valid=False and a NEGATIVE
// signed area, point-in-polygon is inverted, and the served area is wrong
// (feedback #12: 63334-235 came out at -181 269 m² for a 179 116 m² field).
//
// classifyRings resolves the true nesting (even-odd, like shapely's
// make_valid), so a ring bag can also legitimately describe SEVERAL disjoint
// shells — hence a []orb.Polygon result.
// allRings flattens every ring of every part of a MultiPolygon into one bag, so
// classifyRings can resolve nesting ACROSS parts (see the MultiPolygon case in
// geometryToMultiPolygon for why that matters).
func allRings(mp orb.MultiPolygon) []orb.Ring {
	n := 0
	for _, p := range mp {
		n += len(p)
	}
	out := make([]orb.Ring, 0, n)
	for _, p := range mp {
		out = append(out, p...)
	}
	return out
}

// closeRingOrb closes a ring in place-ish (appends the first point when the BEV
// source left it open, which it does for ~35 % of rings). An unclosed ring makes
// every shoelace/ray-cast implementation disagree with every other one.
func closeRingOrb(r orb.Ring) orb.Ring {
	if len(r) >= 3 && r[0] != r[len(r)-1] {
		return append(r, r[0])
	}
	return r
}

// classifyRings determines which rings are shells and which are holes, purely
// from geometry, and returns correctly-oriented polygons (shell CCW, holes CW,
// so the signed area of every result is positive).
//
// Cost — this sits on the hot path of every geometry parse (cache builds,
// point-in-polygon, exports), so:
//   - 1 ring (≈97 % of parcels): returns immediately, zero extra work.
//   - n rings: O(n) bounds + shoelace areas, then parent search over rings
//     sorted by decreasing |area| with a bbox-containment pre-filter, so the
//     O(n²) ray-casts almost never run. Observed n ≤ 17 across parcels,
//     footprints and landuse, so this is a handful of microseconds.
func classifyRings(rings []orb.Ring) []orb.Polygon {
	if len(rings) == 1 {
		r := rings[0]
		if r.Orientation() == orb.CW {
			r.Reverse()
		}
		return []orb.Polygon{{r}}
	}

	type ringInfo struct {
		ring   orb.Ring
		bound  orb.Bound
		area   float64 // absolute planar area
		parent int     // index into the sorted slice, -1 = top level
		depth  int
	}
	infos := make([]ringInfo, 0, len(rings))
	for _, r := range rings {
		infos = append(infos, ringInfo{ring: r, bound: r.Bound(), area: math.Abs(ringSignedArea(r)), parent: -1})
	}
	// Largest first: a ring can only be nested inside a strictly larger one, so
	// every candidate parent is already classified when we reach a child.
	sort.Slice(infos, func(i, j int) bool { return infos[i].area > infos[j].area })

	for i := 1; i < len(infos); i++ {
		best := -1
		for j := i - 1; j >= 0; j-- { // j is larger than i by construction
			if !infos[j].bound.Contains(infos[i].bound.Min) || !infos[j].bound.Contains(infos[i].bound.Max) {
				continue
			}
			if ringContainsRing(infos[j].ring, infos[i].ring) {
				// Rings are sorted by decreasing area, so the LAST match walking
				// backwards is the smallest container = the immediate parent.
				best = j
				break
			}
		}
		if best >= 0 {
			infos[i].parent = best
			infos[i].depth = infos[best].depth + 1
		}
	}

	// Even depth = shell, odd depth = hole in its parent shell.
	polyOf := make([]int, len(infos)) // ring index -> output polygon index, -1 if hole
	out := make([]orb.Polygon, 0, 1)
	for i := range infos {
		if infos[i].depth%2 == 0 {
			r := infos[i].ring
			if r.Orientation() == orb.CW {
				r.Reverse()
			}
			polyOf[i] = len(out)
			out = append(out, orb.Polygon{r})
		} else {
			polyOf[i] = -1
		}
	}
	for i := range infos {
		if polyOf[i] != -1 || infos[i].parent < 0 {
			continue
		}
		shell := polyOf[infos[i].parent]
		if shell < 0 {
			continue // parent is itself a hole: shouldn't happen after depth calc
		}
		r := infos[i].ring
		if r.Orientation() == orb.CCW {
			r.Reverse()
		}
		out[shell] = append(out[shell], r)
	}
	return out
}

// ringContainsRing reports whether `inner` sits inside `outer`.
//
// WHY not simply planar.RingContains(outer, inner[0]): adjacent cadastral rings
// routinely SHARE vertices, and a hole punched out by the neighbouring parcel
// very often STARTS exactly on its parent's boundary. Ray casting is undefined
// there, so a single-vertex test misclassified the hole as a second, disjoint
// shell — GEOS then reported "Nested shells" and the polygon was invalid
// (19 of 4 346 parcels in KG 84108), and the served area double-counted the hole.
//
// Sampling a few vertices fixes it without needing point-in-polygon with a
// tolerance: a ring that genuinely lies inside has at most a couple of vertices
// on the shared boundary, whereas two disjoint rings — even ones that touch —
// have NO vertex strictly inside the other. We stop at the first strict hit, so
// the common case still costs exactly one ray cast.
func ringContainsRing(outer, inner orb.Ring) bool {
	n := len(inner)
	if n == 0 {
		return false
	}
	if n > 1 && inner[0] == inner[n-1] {
		n-- // skip the duplicated closing vertex
	}
	const samples = 8
	step := 1
	if n > samples {
		step = n / samples
	}
	bound := outer.Bound()
	for k := 0; k < n; k += step {
		if bound.Contains(inner[k]) && planar.RingContains(outer, inner[k]) {
			return true
		}
	}
	return false
}

// ringSignedArea is the plain shoelace sum (positive = CCW). Only the sign and
// the relative magnitude matter here, so degrees-squared is fine.
func ringSignedArea(r orb.Ring) float64 {
	if len(r) < 3 {
		return 0
	}
	sum := 0.0
	ox, oy := r[0][0], r[0][1] // offset to the origin for round-off
	for i := 0; i < len(r)-1; i++ {
		sum += (r[i][0]-ox)*(r[i+1][1]-oy) - (r[i+1][0]-ox)*(r[i][1]-oy)
	}
	return sum / 2
}

// marshalGeomWKB encodes a multipolygon as WKB, collapsing the single-part
// case to a plain Polygon.
func marshalGeomWKB(mp orb.MultiPolygon) ([]byte, error) {
	if len(mp) == 0 {
		return nil, fmt.Errorf("empty geometry")
	}
	if len(mp) == 1 {
		return wkb.Marshal(mp[0])
	}
	return wkb.Marshal(mp)
}

// unmarshalGeomWKB decodes a WKB Polygon or MultiPolygon, always returning a
// MultiPolygon with rings re-classified (outer vs hole) so callers have a
// single code path.
func unmarshalGeomWKB(raw []byte) (orb.MultiPolygon, error) {
	g, err := wkb.Unmarshal(raw)
	if err != nil {
		return nil, err
	}
	switch t := g.(type) {
	case orb.MultiPolygon:
		if len(t) == 1 && len(t[0]) < 2 {
			return t, nil // single simple ring: nothing to classify
		}
		return orb.MultiPolygon(classifyRings(allRings(t))), nil
	case orb.Polygon:
		if len(t) < 2 {
			return orb.MultiPolygon{t}, nil
		}
		return orb.MultiPolygon(classifyRings(t)), nil
	default:
		return nil, fmt.Errorf("unexpected wkb geometry %T", g)
	}
}
func orbToPolyclip(p orb.Polygon) polyclip.Polygon {
	result := make(polyclip.Polygon, len(p))
	for i, ring := range p {
		contour := make(polyclip.Contour, len(ring))
		for j, pt := range ring {
			contour[j] = polyclip.Point{X: pt[0], Y: pt[1]}
		}
		result[i] = contour
	}
	return result
}

func polyclipToOrb(p polyclip.Polygon) orb.Polygon {
	result := make(orb.Polygon, len(p))
	for i, contour := range p {
		ring := make(orb.Ring, len(contour))
		for j, pt := range contour {
			ring[j] = orb.Point{pt.X, pt.Y}
		}
		result[i] = ring
	}
	return result
}

// polyclipToOrbMulti turns a polyclip result into a MultiPolygon.
//
// polyclip emits a flat, UNORDERED list of contours and does NOT guarantee an
// orientation convention, so the previous "CCW starts a new polygon, CW is a
// hole of the one before it" rule mis-assembled results: a hole emitted before
// its shell became a shell of its own, two adjacent shells with the same
// winding were fused into shell+hole, and the assembled geometry could even end
// up with negative signed area. On KG 63334 this cost ~9 % of the unioned nfl
// area on top of the dedup bug below.
//
// classifyRings (geometry_multipolygon.go) decides shell-vs-hole by actual
// nesting depth, which is orientation-independent and is already the
// project-wide rule for the geometry caches.
func polyclipToOrbMulti(p polyclip.Polygon) orb.MultiPolygon {
	if len(p) == 0 {
		return nil
	}
	rings := make([]orb.Ring, 0, len(p))
	for _, contour := range p {
		if len(contour) < 3 {
			continue
		}
		ring := make(orb.Ring, 0, len(contour)+1)
		for _, pt := range contour {
			ring = append(ring, orb.Point{pt.X, pt.Y})
		}
		// polyclip contours are implicitly closed; orb rings are explicit.
		if ring[0] != ring[len(ring)-1] {
			ring = append(ring, ring[0])
		}
		rings = append(rings, ring)
	}
	if len(rings) == 0 {
		return nil
	}
	return orb.MultiPolygon(classifyRings(rings))
}

func isCounterClockwise(ring orb.Ring) bool {
	sum := 0.0
	for i := 0; i < len(ring)-1; i++ {
		sum += (ring[i+1][0] - ring[i][0]) * (ring[i+1][1] + ring[i][1])
	}
	return sum < 0
}

// rectClipPolyclip clips every contour to the rectangle b (Sutherland–Hodgman).
// Purely a cost reduction: the parcel is a subset of its own bbox, so clipping
// the landuse polygon to that bbox first cannot change the intersection, but it
// removes the long vertex tails that dominate the exact clip.
func rectClipPolyclip(pc polyclip.Polygon, b orb.Bound) polyclip.Polygon {
	out := make(polyclip.Polygon, 0, len(pc))
	for _, c := range pc {
		cur := c
		for edge := 0; edge < 4 && len(cur) >= 3; edge++ {
			next := make(polyclip.Contour, 0, len(cur)+4)
			n := len(cur)
			for i := 0; i < n; i++ {
				p1 := cur[i]
				p2 := cur[(i+1)%n]
				in1 := rectInside(p1, b, edge)
				in2 := rectInside(p2, b, edge)
				if in1 {
					next = append(next, p1)
				}
				if in1 != in2 {
					next = append(next, rectIntersect(p1, p2, b, edge))
				}
			}
			cur = next
		}
		if len(cur) >= 3 {
			out = append(out, cur)
		}
	}
	return out
}

func rectInside(p polyclip.Point, b orb.Bound, edge int) bool {
	switch edge {
	case 0:
		return p.X >= b.Min[0]
	case 1:
		return p.X <= b.Max[0]
	case 2:
		return p.Y >= b.Min[1]
	default:
		return p.Y <= b.Max[1]
	}
}

func rectIntersect(p1, p2 polyclip.Point, b orb.Bound, edge int) polyclip.Point {
	switch edge {
	case 0:
		t := (b.Min[0] - p1.X) / (p2.X - p1.X)
		return polyclip.Point{X: b.Min[0], Y: p1.Y + t*(p2.Y-p1.Y)}
	case 1:
		t := (b.Max[0] - p1.X) / (p2.X - p1.X)
		return polyclip.Point{X: b.Max[0], Y: p1.Y + t*(p2.Y-p1.Y)}
	case 2:
		t := (b.Min[1] - p1.Y) / (p2.Y - p1.Y)
		return polyclip.Point{X: p1.X + t*(p2.X-p1.X), Y: b.Min[1]}
	default:
		t := (b.Max[1] - p1.Y) / (p2.Y - p1.Y)
		return polyclip.Point{X: p1.X + t*(p2.X-p1.X), Y: b.Max[1]}
	}
}
