// ne_mvt_enc.go — a minimal Mapbox Vector Tile (MVT 2.1) encoder, written
// straight to protobuf wire format: no reflection, no intermediate feature
// objects, one allocation per layer. Keys and values are interned per layer
// (the spec's tag table), geometry is the command stream with zig-zag deltas.
//
// Used by ne_mvt.go (tiles) and ne_pmtiles.go (archives). Only polygons and
// the value kinds the cell layer needs (string, float, sint, bool).
package main

import (
	"math"
)

// mvtExtent — tile coordinate space (the de-facto default).
const mvtExtent = 4096

type mvtValue struct {
	kind byte // 's' string, 'f' float64, 'i' int64 (sint), 'b' bool
	s    string
	f    float64
	i    int64
	b    bool
}

type mvtLayer struct {
	name   string
	keys   []string
	keyIdx map[string]uint32
	vals   []mvtValue
	valIdx map[mvtValue]uint32
	feats  []byte // encoded Feature messages, each already wrapped with tag 2
	n      int
}

func newMVTLayer(name string) *mvtLayer {
	return &mvtLayer{name: name, keyIdx: map[string]uint32{}, valIdx: map[mvtValue]uint32{}}
}

func (l *mvtLayer) key(k string) uint32 {
	if i, ok := l.keyIdx[k]; ok {
		return i
	}
	i := uint32(len(l.keys))
	l.keys = append(l.keys, k)
	l.keyIdx[k] = i
	return i
}

func (l *mvtLayer) val(v mvtValue) uint32 {
	if i, ok := l.valIdx[v]; ok {
		return i
	}
	i := uint32(len(l.vals))
	l.vals = append(l.vals, v)
	l.valIdx[v] = i
	return i
}

// mvtFeature — a feature under construction: tags + geometry.
type mvtFeature struct {
	l    *mvtLayer
	id   uint64
	tags []uint32
	geom []uint32
	cx   int32 // cursor
	cy   int32
}

func (l *mvtLayer) feature(id uint64) *mvtFeature {
	return &mvtFeature{l: l, id: id, tags: make([]uint32, 0, 64), geom: make([]uint32, 0, 24)}
}

func (f *mvtFeature) str(k, v string) {
	f.tags = append(f.tags, f.l.key(k), f.l.val(mvtValue{kind: 's', s: v}))
}
func (f *mvtFeature) num(k string, v float64) {
	if v == math.Trunc(v) && math.Abs(v) < 1<<31 {
		f.tags = append(f.tags, f.l.key(k), f.l.val(mvtValue{kind: 'i', i: int64(v)}))
		return
	}
	f.tags = append(f.tags, f.l.key(k), f.l.val(mvtValue{kind: 'f', f: v}))
}
func (f *mvtFeature) i64(k string, v int64) {
	f.tags = append(f.tags, f.l.key(k), f.l.val(mvtValue{kind: 'i', i: v}))
}
func (f *mvtFeature) flag(k string, v bool) {
	f.tags = append(f.tags, f.l.key(k), f.l.val(mvtValue{kind: 'b', b: v}))
}

func zz32(v int32) uint32 { return uint32((v << 1) ^ (v >> 31)) }

// ring — one closed ring in tile units (x right, y down). Exterior rings must
// wind clockwise in that space (positive surveyor area the spec's way); the
// encoder reverses when needed so callers pass boundaries as they come.
func (f *mvtFeature) ring(pts [][2]int32, hole bool) {
	n := len(pts)
	if n < 3 {
		return
	}
	if pts[0] == pts[n-1] {
		pts = pts[:n-1]
		n--
		if n < 3 {
			return
		}
	}
	var a int64
	for i := 0; i < n; i++ {
		p, q := pts[i], pts[(i+1)%n]
		a += int64(p[0])*int64(q[1]) - int64(q[0])*int64(p[1])
	}
	if a == 0 {
		return
	}
	// y-down screen space: a > 0 is clockwise on screen = exterior per spec
	rev := (a < 0) != hole
	at := func(i int) [2]int32 {
		if rev {
			return pts[n-1-i]
		}
		return pts[i]
	}
	p := at(0)
	f.geom = append(f.geom, 1<<3|1, zz32(p[0]-f.cx), zz32(p[1]-f.cy)) // MoveTo ×1 (count<<3 | id)
	f.cx, f.cy = p[0], p[1]
	f.geom = append(f.geom, uint32(n-1)<<3|2) // LineTo ×(n-1)
	for i := 1; i < n; i++ {
		p = at(i)
		f.geom = append(f.geom, zz32(p[0]-f.cx), zz32(p[1]-f.cy))
		f.cx, f.cy = p[0], p[1]
	}
	f.geom = append(f.geom, 1<<3|7) // ClosePath
}

// done — serialise the feature into the layer (geometry type 3 = polygon).
func (f *mvtFeature) done() {
	if len(f.geom) == 0 {
		return
	}
	var b []byte
	b = pbVarint(b, 1<<3|0)
	b = pbVarint(b, f.id)
	b = pbPacked(b, 2, f.tags)
	b = pbVarint(b, 3<<3|0)
	b = pbVarint(b, 3)
	b = pbPacked(b, 4, f.geom)
	f.l.feats = pbBytes(f.l.feats, 2, b)
	f.l.n++
}

// encode — the Tile message: one Layer (repeated field 3).
func mvtEncode(layers ...*mvtLayer) []byte {
	var out []byte
	for _, l := range layers {
		if l == nil || l.n == 0 {
			continue
		}
		var b []byte
		b = pbVarint(b, 15<<3|0)
		b = pbVarint(b, 2)
		b = pbBytes(b, 1, []byte(l.name))
		b = append(b, l.feats...)
		for _, k := range l.keys {
			b = pbBytes(b, 3, []byte(k))
		}
		for _, v := range l.vals {
			var vb []byte
			switch v.kind {
			case 's':
				vb = pbBytes(vb, 1, []byte(v.s))
			case 'f': // double (field 3): values are interned per layer, so the 8 bytes are paid once per distinct value
				vb = pbVarint(vb, 3<<3|1)
				u := math.Float64bits(v.f)
				vb = append(vb, byte(u), byte(u>>8), byte(u>>16), byte(u>>24), byte(u>>32), byte(u>>40), byte(u>>48), byte(u>>56))
			case 'i':
				vb = pbVarint(vb, 6<<3|0)
				vb = pbVarint(vb, uint64((v.i<<1)^(v.i>>63)))
			case 'b':
				vb = pbVarint(vb, 7<<3|0)
				if v.b {
					vb = append(vb, 1)
				} else {
					vb = append(vb, 0)
				}
			}
			b = pbBytes(b, 4, vb)
		}
		b = pbVarint(b, 5<<3|0)
		b = pbVarint(b, mvtExtent)
		out = pbBytes(out, 3, b)
	}
	return out
}

// ── protobuf wire helpers ────────────────────────────────────────────────────

func pbVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func pbBytes(b []byte, field uint64, v []byte) []byte {
	b = pbVarint(b, field<<3|2)
	b = pbVarint(b, uint64(len(v)))
	return append(b, v...)
}

func pbPacked(b []byte, field uint64, vs []uint32) []byte {
	if len(vs) == 0 {
		return b
	}
	var p []byte
	for _, v := range vs {
		p = pbVarint(p, uint64(v))
	}
	return pbBytes(b, field, p)
}

// ── Web Mercator tile maths ──────────────────────────────────────────────────

// mvtTileBox — lon/lat bounds of z/x/y.
func mvtTileBox(z, x, y int) (w, s, e, n float64) {
	nn := math.Exp2(float64(z))
	lon := func(x float64) float64 { return x/nn*360 - 180 }
	lat := func(y float64) float64 {
		t := math.Pi * (1 - 2*y/nn)
		return math.Atan(math.Sinh(t)) * 180 / math.Pi
	}
	return lon(float64(x)), lat(float64(y + 1)), lon(float64(x + 1)), lat(float64(y))
}

// mvtProject — lon/lat → tile units of z/x/y (float; callers round).
func mvtProject(z, x, y int, lon, lat float64) (float64, float64) {
	nn := math.Exp2(float64(z))
	px := (lon + 180) / 360 * nn
	la := lat * math.Pi / 180
	py := (1 - math.Log(math.Tan(la)+1/math.Cos(la))/math.Pi) / 2 * nn
	return (px - float64(x)) * mvtExtent, (py - float64(y)) * mvtExtent
}

// mvtTilesInBox — the tile range covering a lon/lat box at zoom z.
func mvtTilesInBox(z int, w, s, e, n float64) (x0, y0, x1, y1 int) {
	nn := math.Exp2(float64(z))
	tx := func(lon float64) int { return int(math.Floor((lon + 180) / 360 * nn)) }
	ty := func(lat float64) int {
		la := lat * math.Pi / 180
		return int(math.Floor((1 - math.Log(math.Tan(la)+1/math.Cos(la))/math.Pi) / 2 * nn))
	}
	clamp := func(v int) int { return max(0, min(int(nn)-1, v)) }
	return clamp(tx(w)), clamp(ty(n)), clamp(tx(e)), clamp(ty(s))
}
