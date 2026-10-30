package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	h3 "github.com/uber/h3-go/v4"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// ── a synthetic bin: a 24×24 IJ lattice with every core+ext column filled from a seeded PRNG,
// plus deliberate nodata / zero / halo bits so every evaluator branch is exercised.
func newNEFiltTestBin(seed int64) *neFidxBin {
	rng := rand.New(rand.NewSource(seed))
	const W = 24
	b := &neFidxBin{Cols: map[string][]byte{}}
	for j := int32(0); j < W; j++ {
		for i := int32(0); i < W; i++ {
			b.IJ = append(b.IJ, i, j)
		}
	}
	b.N = W * W
	b.Hdr = neFidxHdr{State: "test00000000", BBox: [4]float64{16.3, 48.2, 16.325, 48.225}, Origin: "8c1e15b728009ff",
		A: []float64{5900, 0.021, 0.0105, 1050, 0, 0.0182}, Hex: [][2]float64{{0.012, 0}, {0.006, 0.0105}, {-0.006, 0.0105}, {-0.012, 0}, {-0.006, -0.0105}, {0.006, -0.0105}},
		N2K: []string{"AT1304000", "AT1110137"}, ResPx: 0.2}
	for _, fd := range neFiltFields {
		sz := neFidxColSize(fd.Dtype)
		c := make([]byte, sz*b.N)
		for k := 0; k < b.N; k++ {
			var v int
			switch fd.Kind {
			case nfSet:
				v = rng.Intn(len(fd.Members))
			case nfBits:
				v = rng.Intn(64)
			case nfFlag:
				v = rng.Intn(2)
			case nfSites:
				v = rng.Intn(3) // index into Hdr.N2K, or none (nodata)
				if v == 2 {
					v = fd.Nodata
				}
			case nfBucket:
				v = rng.Intn(len(fd.Edges))
			default:
				switch fd.Dtype {
				case "u16":
					v = rng.Intn(4000)
				case "i8":
					v = rng.Intn(200) - 100
				default:
					v = rng.Intn(200)
				}
			}
			if k%37 == 0 { // nodata every 37th cell
				v = fd.Nodata
			} else if fd.Halo && k%11 == 0 {
				v = 128 | rng.Intn(64)
			}
			switch fd.Dtype {
			case "u16":
				c[2*k] = byte(v)
				c[2*k+1] = byte(v >> 8)
			case "i8":
				c[k] = byte(int8(v))
			default:
				c[k] = byte(v)
			}
		}
		b.Cols[fd.ID] = c
	}
	return b
}

func TestNEFiltParseCanonical(t *testing.T) {
	cases := []struct {
		in, canon, words string
		ignored          int
		ext              bool
	}{
		{"", "", "", 0, false},
		{"d=geb+bau,sl>=3", "d=bau+geb,sl>=3", "declared use building land / buildings · slope ≥ 3°", 0, false},
		{"d=geb+bau+geb", "d=bau+geb", "declared use building land / buildings", 0, false},
		{" steep , forest ", "sl>=20,d=wald", "slope ≥ 20° · declared use forest", 0, false},
		{"hb=12..30", "hb=12..30", "tallest structure 12–30 m", 0, false},
		{"hb<=5|ht>=25", "hb<=5|ht>=25", "(tallest structure ≤ 5 m or tallest tree ≥ 25 m)", 0, false},
		{"n2k", "n2k", "Natura 2000", 0, false},
		{"n2k=unk", "n2k=unk", "Natura 2000 unknown", 0, false},
		{"n2ks=at1304000", "n2ks=AT1304000", "site AT1304000", 0, false},
		{"chg=confirmed", "chg=conf", "change in the register confirmed", 0, false},
		{"CHG=Conf", "chg=conf", "change in the register confirmed", 0, false},
		{"D=GEB", "d=geb", "declared use buildings", 0, false},
		{"as=south+north-west", "as=nw+s", "aspect north-west / south", 0, false},
		{"sub=vineyard", "sub=wein", "subsidy class vineyard", 0, false},
		{"d=geb,k>=20", "d=geb,k>=20", "declared use buildings · clusters ≥ 20 cells", 0, false},
		{"k>=3", "k>=3", "clusters ≥ 3 cells", 0, false},
		{"bogus", "", "", 1, false},
		{"d=nope", "", "", 1, false},
		{"d=geb,bogus,sl>=3", "d=geb,sl>=3", "declared use buildings · slope ≥ 3°", 1, false},
		{"sl=abc", "", "", 1, false},
		{"rd<=50", "rd<=50", "distance to a road < 100 m", 0, true},
		{"stor>=4,d=geb", "stor>=4,d=geb", "storeys of the tallest structure ≥ 4 · declared use buildings", 0, true},
		{"el=1500..2000", "el=1500..2000", "elevation 1500–2000 m", 0, false},
		{"cont>=10", "cont>=10", "patch size (same declared group) ≥ 10 cells", 0, false},
		{"d=geb;sl>=3", "", "", 1, false}, // ';' is the hash separator, never part of f
		{"sl>=3,sl>=3", "sl>=3,sl>=3", "slope ≥ 3° · slope ≥ 3°", 0, false},
		{"hb>=-1", "hb>=-1", "tallest structure ≥ -1 m", 0, false},
	}
	for _, c := range cases {
		e := neFiltParse(c.in)
		if got := e.Canonical(); got != c.canon {
			t.Errorf("%q canonical = %q, want %q", c.in, got, c.canon)
		}
		if got := e.Words(); got != c.words {
			t.Errorf("%q words = %q, want %q", c.in, got, c.words)
		}
		if len(e.Ignored) != c.ignored {
			t.Errorf("%q ignored = %v, want %d", c.in, e.Ignored, c.ignored)
		}
		if got := neFiltNeedsExt(e); got != c.ext {
			t.Errorf("%q ext = %v, want %v", c.in, got, c.ext)
		}
		// canonical is a fixed point
		if e2 := neFiltParse(e.Canonical()); e2.Canonical() != e.Canonical() {
			t.Errorf("%q canonical not idempotent: %q → %q", c.in, e.Canonical(), e2.Canonical())
		}
	}
	// every named word and preset parses cleanly
	for w, f := range neFiltWords {
		if e := neFiltParse(f); len(e.Ignored) > 0 || e.empty() {
			t.Errorf("word %q = %q does not parse", w, f)
		}
		if e := neFiltParse(w); len(e.Ignored) > 0 {
			t.Errorf("word %q itself must be accepted as a term", w)
		}
	}
	for _, p := range neFiltPresets {
		if e := neFiltParse(p[1]); len(e.Ignored) > 0 || e.empty() {
			t.Errorf("preset %q = %q does not parse", p[0], p[1])
		}
	}
	// field ids are unique, ≤ 5 chars, lower-case; every set field has labels parallel to its members
	seen := map[string]bool{}
	for _, fd := range neFiltFields {
		if seen[fd.ID] || len(fd.ID) > 6 || fd.ID != strings.ToLower(fd.ID) {
			t.Errorf("bad field id %q", fd.ID)
		}
		seen[fd.ID] = true
		if fd.Kind == nfSet && fd.Labels != nil && len(fd.Labels) != len(fd.Members) {
			t.Errorf("%s: labels %d ≠ members %d", fd.ID, len(fd.Labels), len(fd.Members))
		}
		if fd.Kind == nfNum && fd.Scale == 0 {
			t.Errorf("%s: numeric field without scale", fd.ID)
		}
		if fd.Kind == nfBucket && len(fd.Edges) < 2 {
			t.Errorf("%s: bucket field without edges", fd.ID)
		}
	}
	// a string longer than the cap is cut, not crashing, and `k` is bounded
	long := strings.Repeat("sl>=3,", 400)
	if e := neFiltParse(long); len(e.Groups) > 64 {
		t.Errorf("groups not capped: %d", len(e.Groups))
	}
	if e := neFiltParse("k>=99999999"); e.K > 4096 {
		t.Errorf("k not capped: %d", e.K)
	}
}

func TestNEFiltBoundsQuantisation(t *testing.T) {
	hb := neFiltByID["hb"] // scale 0.5 m per unit
	lo, hi := neFiltBounds(hb, neFiltParse("hb=12..30").Groups[0][0])
	if lo != 24 || hi != 60 {
		t.Errorf("hb=12..30 → raw %d..%d, want 24..60", lo, hi)
	}
	lo, hi = neFiltBounds(hb, neFiltParse("hb=12.3..30.2").Groups[0][0])
	if lo != 25 || hi != 60 { // lo = ceil, hi = floor
		t.Errorf("hb=12.3..30.2 → raw %d..%d, want 25..60", lo, hi)
	}
	dsh := neFiltByID["dsh"] // 100/255 per unit
	lo, hi = neFiltBounds(dsh, neFiltParse("dsh>=50").Groups[0][0])
	if lo != 128 || hi != math.MaxInt32 {
		t.Errorf("dsh>=50 → raw %d..%d, want 128..open", lo, hi)
	}
	rd := neFiltByID["rd"]
	for _, c := range []struct {
		v    float64
		want int
	}{{0, 0}, {25, neFiltBucketOf(rd.Edges, 25)}, {1e9, len(rd.Edges) - 1}} {
		if got := neFiltBucketOf(rd.Edges, c.v); got != c.want {
			t.Errorf("bucket(%v) = %d, want %d", c.v, got, c.want)
		}
	}
	if neFiltBucketOf(rd.Edges, float64(rd.Edges[1])) != 1 || neFiltBucketOf(rd.Edges, float64(rd.Edges[1])-0.01) != 0 {
		t.Errorf("bucket edge is inclusive on the lower side")
	}
}

// brute-force reference evaluator over the synthetic bin
func neFiltRefEval(b *neFidxBin, e *neFiltExpr) []uint8 {
	out := make([]uint8, b.N)
	for k := 0; k < b.N; k++ {
		ok := true
		for _, g := range e.Groups {
			any := false
			for _, t := range g {
				fd := neFiltByID[t.F]
				c := b.Cols[fd.ID]
				v := neFiltRaw(fd, c, k)
				nd := v == fd.Nodata
				var hit bool
				switch {
				case t.Unk:
					hit = nd
				case nd:
					hit = false
				case t.Flag:
					hit = v != 0
				case fd.Kind == nfSites:
					for _, s := range t.Set {
						if v < len(b.Hdr.N2K) && b.Hdr.N2K[v] == s {
							hit = true
						}
					}
				case fd.Kind == nfSet:
					for _, m := range t.Set {
						if neFiltMember(fd, m) == v {
							hit = true
						}
					}
				case fd.Kind == nfBits:
					for _, m := range t.Set {
						if i := neFiltMember(fd, m); v&(1<<uint(i)) != 0 {
							hit = true
						}
					}
				default:
					lo, hi := neFiltBounds(fd, t)
					if fd.Halo && v&128 != 0 {
						hit = hi == math.MaxInt32 && lo <= 64
					} else if (fd.ID == "loss" || fd.ID == "snar" || fd.ID == "snar2") && v == 0 {
						hit = false
					} else {
						hit = v >= lo && v <= hi
					}
				}
				if hit {
					any = true
					break
				}
			}
			if !any {
				ok = false
				break
			}
		}
		if ok {
			out[k] = 1
		}
	}
	return out
}

var neFiltTestExprs = []string{
	"", "d=geb", "d=geb+bau", "d=geb+bau,sl>=3", "dsh>=50", "dsh<=10", "hb=12..30", "hb<=5|ht>=25", "n2k", "n2k=unk", "sl=unk",
	"n2ks=AT1304000", "n2ks=AT1110137+AT1304000", "chg=conf", "chg=cand+conf", "as=s+sw", "sub=wein|bio", "el=1500..2000", "el>=3000",
	"cont>=10", "cont<=5", "cont>=64", "rd<=50", "wat<=100,d=gruen", "stor>=4,d=geb", "oepul=bio", "nutz=unk", "loss>=1", "snar>=1",
	"dh>=2", "dh<=-2", "tpi>=10", "tri<=3", "ver=unk", "d=geb,k>=4", "sl>=10,k>=2", "steep,forest", "settlement", "protected,farmland",
	"d=wald|o=wald,ht>=15", "osh>=80,d=acker", "bcov>=30,bn>=2", "phen=crop", "roof=flat", "hmax>=40", "sl90>=30", "scov>=50,sub=acker",
	"h=unk,d=geb", "bogus,d=geb", "k>=3", "hb>=0", "el<=100",
}

func TestNEFiltEvalAgainstReference(t *testing.T) {
	b := newNEFiltTestBin(7)
	for _, f := range neFiltTestExprs {
		e := neFiltParse(f)
		if e.K > 0 {
			continue // cluster filter is tested separately
		}
		got, want := neFiltEval(b, e), neFiltRefEval(b, e)
		if !bytes.Equal(got, want) {
			ng, nw := 0, 0
			for k := range got {
				ng += int(got[k])
				nw += int(want[k])
			}
			t.Errorf("%q: evaluator %d matches, reference %d", f, ng, nw)
		}
	}
	// nodata never matches a range or a set, and always matches =unk
	e := neFiltParse("sl=unk")
	m := neFiltEval(b, e)
	for k := 0; k < b.N; k++ {
		if (k%37 == 0) != (m[k] == 1) {
			t.Fatalf("sl=unk at %d: %d", k, m[k])
		}
	}
}

func TestNEFidxClusterFilter(t *testing.T) {
	b := newNEFiltTestBin(1)
	// hand-made mask: a 3-cell chain, a lone cell, a 2×2 block
	m := make([]uint8, b.N)
	at := func(i, j int32) int { return int(j*24 + i) }
	for _, p := range [][2]int32{{2, 2}, {3, 2}, {4, 2}, {10, 10}, {15, 15}, {16, 15}, {15, 16}, {16, 16}} {
		m[at(p[0], p[1])] = 1
	}
	cp := append([]uint8(nil), m...)
	neFidxClusterFilter(b, cp, 2)
	if cp[at(10, 10)] != 0 || cp[at(2, 2)] != 1 || cp[at(15, 15)] != 1 {
		t.Errorf("k>=2: lone dropped, chain and block kept: %v %v %v", cp[at(10, 10)], cp[at(2, 2)], cp[at(15, 15)])
	}
	cp = append([]uint8(nil), m...)
	neFidxClusterFilter(b, cp, 4)
	if cp[at(2, 2)] != 0 || cp[at(16, 16)] != 1 { // (15,15),(16,15),(15,16),(16,16): (15,15)-(16,16) are IJ neighbours via (1,1)
		t.Errorf("k>=4: chain of 3 dropped, block of 4 kept: %v %v", cp[at(2, 2)], cp[at(16, 16)])
	}
	cp = append([]uint8(nil), m...)
	neFidxClusterFilter(b, cp, 5)
	for k, v := range cp {
		if v != 0 {
			t.Fatalf("k>=5 must drop everything, %d kept", k)
		}
	}
	// via eval: k applies after the term mask
	e := neFiltParse("d=geb,k>=3")
	full, clustered := neFiltEval(b, neFiltParse("d=geb")), b.eval(e)
	nf, nc := 0, 0
	for k := range full {
		nf += int(full[k])
		nc += int(clustered[k])
		if clustered[k] == 1 && full[k] == 0 {
			t.Fatal("cluster filter added a cell")
		}
	}
	if nc == 0 || nc >= nf {
		t.Errorf("k>=3 should drop some but not all: %d of %d", nc, nf)
	}
}

func TestNEFidxEncodeDecodeRoundtrip(t *testing.T) {
	b := newNEFiltTestBin(3)
	for _, plane := range []string{"core", "ext", "all"} {
		d := b.encode(plane)
		if string(d[:4]) != "NFX1" {
			t.Fatalf("%s: magic", plane)
		}
		got, err := neFidxDecode(d)
		if err != nil {
			t.Fatalf("%s: decode: %v", plane, err)
		}
		if got.N != b.N || len(got.IJ) != len(b.IJ) {
			t.Fatalf("%s: n %d ij %d", plane, got.N, len(got.IJ))
		}
		for k := range b.IJ {
			if got.IJ[k] != b.IJ[k] {
				t.Fatalf("%s: ij[%d] %d ≠ %d", plane, k, got.IJ[k], b.IJ[k])
			}
		}
		for _, fd := range neFiltFields {
			want := plane == "all" || fd.Plane == plane
			c, ok := got.Cols[fd.ID]
			if ok != want {
				t.Errorf("%s: column %s present=%v want %v", plane, fd.ID, ok, want)
			}
			if ok && !bytes.Equal(c, b.Cols[fd.ID]) {
				t.Errorf("%s: column %s differs", plane, fd.ID)
			}
		}
		if got.Hdr.State != b.Hdr.State || got.Hdr.Origin != b.Hdr.Origin || len(got.Hdr.A) != 6 || len(got.Hdr.Hex) != 6 || len(got.Hdr.N2K) != 2 {
			t.Errorf("%s: header lost: %+v", plane, got.Hdr)
		}
		// positions survive (header polynomial)
		x0, y0 := b.pos(100)
		x1, y1 := got.pos(100)
		if x0 != x1 || y0 != y1 {
			t.Errorf("%s: pos differs", plane)
		}
		// plane extraction from the stored "all" blob agrees with a direct encode
		if plane != "all" {
			p, err := neFidxPlane(b.encode("all"), plane)
			if err != nil || !bytes.Equal(p, d) {
				t.Errorf("%s: neFidxPlane differs from encode (err %v)", plane, err)
			}
		}
	}
	// corrupt input never panics
	for _, bad := range [][]byte{nil, []byte("NFX1"), append([]byte("NFX1\x01"), bytes.Repeat([]byte{0xff}, 20)...), b.encode("all")[:200]} {
		if _, err := neFidxDecode(bad); err == nil {
			t.Errorf("corrupt blob decoded")
		}
	}
	// an empty bin round-trips
	empty := &neFidxBin{Hdr: neFidxHdr{State: "x", A: make([]float64, 6)}, Cols: map[string][]byte{}}
	if got, err := neFidxDecode(empty.encode("core")); err != nil || got.N != 0 {
		t.Errorf("empty bin: %v", err)
	}
	// size: planar u8 columns, one blob per plane ≈ n × bytes/cell + header
	if d := b.encode("core"); len(d) > b.N*40+4096 {
		t.Errorf("core blob too large: %d B for %d cells", len(d), b.N)
	}
}

func TestNEFidxFit(t *testing.T) {
	// a planar lattice is recovered exactly as affine; a curved one falls back to quadratic within the pixel budget
	b := &neFidxBin{Hdr: neFidxHdr{}}
	var cells []neFidxCell
	var ord []int
	k := 0
	for j := int32(0); j < 20; j++ {
		for i := int32(0); i < 20; i++ {
			fi, fj := float64(i), float64(j)
			cells = append(cells, neFidxCell{i: i, j: j, x: 100 + 0.5*fi + 0.25*fj, y: 200 - 0.433*fj})
			ord = append(ord, k)
			k++
		}
	}
	b.N = len(cells)
	for _, c := range cells {
		b.IJ = append(b.IJ, c.i, c.j)
	}
	if err := neFidxFitXY(b, cells, ord); err != nil {
		t.Fatal(err)
	}
	if len(b.Hdr.A) != 6 {
		t.Errorf("planar lattice should fit affine, got %d coefficients", len(b.Hdr.A))
	}
	for q := range cells {
		x, y := b.pos(q)
		if math.Abs(x-cells[q].x) > 1e-6 || math.Abs(y-cells[q].y) > 1e-6 {
			t.Fatalf("affine residual at %d: %v %v", q, x-cells[q].x, y-cells[q].y)
		}
	}
	for q := range cells {
		cells[q].x += 0.002 * cells[q].x * cells[q].x / 1e3 * float64(cells[q].j) // mild curvature
	}
	if err := neFidxFitXY(b, cells, ord); err != nil {
		t.Fatal(err)
	}
	for q := range cells {
		x, y := b.pos(q)
		if math.Abs(x-cells[q].x) > 0.3 || math.Abs(y-cells[q].y) > 0.3 {
			t.Fatalf("fit residual > 0.3 px at %d: %v %v (%d coeffs)", q, x-cells[q].x, y-cells[q].y, len(b.Hdr.A))
		}
	}
}

// ── JS parity: the Worker's parser, canonical form, words and evaluator must agree bit-for-bit with Go.
func TestNEFilterJSParity(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	fields := map[string]interface{}{"fields": neFiltFields, "words": neFiltWords}
	b := newNEFiltTestBin(11)
	job := map[string]interface{}{"fields": fields, "exprs": neFiltTestExprs, "bin": base64.StdEncoding.EncodeToString(b.encode("all"))}
	jb, _ := json.Marshal(job)
	cmd := exec.Command(node, "testdata/ne_filter_harness.js", "ne_glyph_filter.js")
	cmd.Stdin = bytes.NewReader(jb)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("harness: %v\n%s", err, stderr.String())
	}
	var res struct {
		N     int    `json:"n"`
		State string `json:"state"`
		Exprs []struct {
			Canon   string   `json:"canon"`
			Words   string   `json:"words"`
			Ignored []string `json:"ignored"`
			Ext     bool     `json:"ext"`
		} `json:"exprs"`
		Masks []string `json:"masks"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("harness output: %v\n%s", err, out)
	}
	if res.N != b.N || res.State != b.Hdr.State {
		t.Fatalf("JS decode: n=%d state=%q", res.N, res.State)
	}
	for i, f := range neFiltTestExprs {
		e := neFiltParse(f)
		js := res.Exprs[i]
		if js.Canon != e.Canonical() {
			t.Errorf("%q: JS canonical %q ≠ Go %q", f, js.Canon, e.Canonical())
		}
		if strings.ReplaceAll(js.Words, "\u202f", " ") != e.Words() {
			t.Errorf("%q: JS words %q ≠ Go %q", f, js.Words, e.Words())
		}
		if len(js.Ignored) != len(e.Ignored) {
			t.Errorf("%q: JS ignored %v ≠ Go %v", f, js.Ignored, e.Ignored)
		}
		if js.Ext != neFiltNeedsExt(e) {
			t.Errorf("%q: JS ext %v ≠ Go %v", f, js.Ext, neFiltNeedsExt(e))
		}
		want := b.eval(e)
		got, _ := base64.StdEncoding.DecodeString(res.Masks[i])
		if !bytes.Equal(got, want) {
			ng, nw, first := 0, 0, -1
			for k := range want {
				if k < len(got) {
					ng += int(got[k])
				}
				nw += int(want[k])
				if first < 0 && (k >= len(got) || got[k] != want[k]) {
					first = k
				}
			}
			t.Errorf("%q: JS mask %d matches ≠ Go %d (first diff at %d)", f, ng, nw, first)
		}
	}
}

// ── HTTP: the agent endpoint and the fields catalogue
func TestNEFilterEndpoints(t *testing.T) {
	h := publicNEHandlerForTest(t)
	get := func(p string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", p, nil))
		return rr
	}
	rr := get("/api/v1/ne/filter/fields")
	if rr.Code != 200 {
		t.Fatalf("fields: %d %s", rr.Code, rr.Body.String())
	}
	var cat struct {
		Fields  []neFiltField     `json:"fields"`
		Words   map[string]string `json:"words"`
		Presets []map[string]string
		Grammar string
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &cat); err != nil || len(cat.Fields) != len(neFiltFields) || len(cat.Words) == 0 || len(cat.Presets) == 0 || cat.Grammar == "" {
		t.Fatalf("fields payload: %v %d", err, len(cat.Fields))
	}
	if rr.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("fields: CORS")
	}
	for _, c := range []struct {
		p    string
		code int
	}{
		{"/api/v1/ne/filter", 400},
		{"/api/v1/ne/filter?bbox=1,2,3", 400},
		{"/api/v1/ne/filter?bbox=a,b,c,d", 400},
		{"/api/v1/ne/filter?bbox=16.3,48.2,16.2,48.3", 400},
		{"/api/v1/ne/filter?bbox=1,1,2,2", 400},
		{"/api/v1/ne/filter?bbox=16.0,48.0,16.5,48.5&f=d=geb", 413},
		{"/api/v1/ne/filter?bbox=16.30,48.20,16.31,48.21&f=d=geb&format=xml", 400},
		{"/api/v1/ne/filter?kg=00000&f=d=geb", 404},
	} {
		if rr := get(c.p); rr.Code != c.code {
			t.Errorf("%s → %d, want %d: %s", c.p, rr.Code, c.code, rr.Body.String())
		}
	}
	// a synthetic bin planted in the LRU under the live state answers count / cells / h3 / geojson
	if err := neGlyphLoadFrame(); err != nil {
		t.Skip("frame unavailable:", err)
	}
	i, j := 652, 1928 // 16.300, 48.200
	st := neFidxState(i, j)
	b := newNEFiltTestBin(5)
	b.Hdr.State = st
	b.Hdr.BBox = [4]float64{float64(i) * neFidxBinDeg, float64(j) * neFidxBinDeg, float64(i+1) * neFidxBinDeg, float64(j+1) * neFidxBinDeg}
	// place the 24×24 lattice inside the bin: pos(i,j) = proj(lon0 + i·dl, lat0 + j·dl), affine in the frame
	dl := neFidxBinDeg / 26
	x0, y0 := neGlyphProj(b.Hdr.BBox[0]+dl, b.Hdr.BBox[1]+dl)
	xi, yi := neGlyphProj(b.Hdr.BBox[0]+2*dl, b.Hdr.BBox[1]+dl)
	xj, yj := neGlyphProj(b.Hdr.BBox[0]+dl, b.Hdr.BBox[1]+2*dl)
	b.Hdr.A = []float64{x0, xi - x0, xj - x0, y0, yi - y0, yj - y0}
	// membership is decided on the exact cell centre: anchor the IJ lattice on a real res-12 cell inside the bin
	oc, _ := h3.LatLngToCell(h3.LatLng{Lat: b.Hdr.BBox[1] + 8*dl, Lng: b.Hdr.BBox[0] + 8*dl}, 12)
	b.Hdr.Origin = neCellHex(uint64(oc))
	oij, _ := h3.CellToLocalIJ(oc, oc) // local IJ is anchored on the base cell, not the origin: offset the lattice
	for k := range b.IJ {
		if k%2 == 0 {
			b.IJ[k] += int32(oij.I)
		} else {
			b.IJ[k] += int32(oij.J)
		}
	}
	oi, oj := float64(oij.I), float64(oij.J)
	b.Hdr.A = []float64{x0 - oi*(xi-x0) - oj*(xj-x0), xi - x0, xj - x0, y0 - oi*(yi-y0) - oj*(yj-y0), yi - y0, yj - y0}
	neFidxLRU.put(fmt.Sprintf("%d/%d/%s", i, j, st), b.encode("all"))
	defer neFidxLRU.dropPrefix(fmt.Sprintf("%d/%d/", i, j))
	rr = get("/api/v1/ne/filter?bbox=16.300,48.200,16.325,48.225&f=d=geb,bogus")
	if rr.Code != 200 {
		t.Fatalf("count: %d %s", rr.Code, rr.Body.String())
	}
	var cnt struct {
		Cells, Match, Bins int
		Partial            int `json:"partial_bins"`
		Expr, Words, Link  string
		Ignored            []string
		ByD                map[string]int `json:"by_d"`
		Ha                 float64
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &cnt); err != nil {
		t.Fatal(err)
	}
	wantM := 0
	for _, v := range neFiltEval(b, neFiltParse("d=geb")) {
		wantM += int(v)
	}
	if cnt.Cells != b.N || cnt.Match != wantM || cnt.Bins != 1 || cnt.Partial != 0 || cnt.Expr != "d=geb" || len(cnt.Ignored) != 1 || cnt.ByD["geb"] != wantM || !strings.Contains(cnt.Link, ";f=d=geb") || cnt.Ha <= 0 {
		t.Errorf("count payload: %+v", cnt)
	}
	for _, f := range []string{"cells", "h3", "geojson"} {
		rr = get("/api/v1/ne/filter?bbox=16.300,48.200,16.325,48.225&f=d=geb&format=" + f)
		if rr.Code != 200 {
			t.Errorf("%s: %d %s", f, rr.Code, rr.Body.String())
			continue
		}
		body := rr.Body.String()
		if bad := nePublicBannedWords(body); len(bad) > 0 {
			t.Errorf("%s: banned words %v", f, bad)
		}
		switch f {
		case "geojson":
			var fc struct {
				Type     string
				Features []struct {
					Geometry struct{ Coordinates [][][]float64 }
					Props    map[string]interface{} `json:"properties"`
				}
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &fc); err != nil || fc.Type != "FeatureCollection" || len(fc.Features) != wantM || len(fc.Features[0].Geometry.Coordinates[0]) != 7 {
				t.Errorf("geojson: %v type=%q n=%d", err, fc.Type, len(fc.Features))
			}
		case "h3":
			var v struct{ H3 []string }
			if err := json.Unmarshal(rr.Body.Bytes(), &v); err != nil || len(v.H3) != wantM || len(v.H3[0]) != 15 {
				t.Errorf("h3: %v n=%d", err, len(v.H3))
			}
		case "cells":
			var v struct {
				Cells []string `json:"cells_h3"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &v); err != nil || len(v.Cells) != wantM || len(v.Cells[0]) != 15 {
				t.Errorf("cells: %v n=%d", err, len(v.Cells))
			}
		}
	}
	// the cells cap answers 413 rather than a huge body
	rr = get("/api/v1/ne/filter?bbox=16.300,48.200,16.325,48.225&f=&format=geojson")
	if rr.Code != 200 && rr.Code != 413 {
		t.Errorf("geojson of everything: %d", rr.Code)
	}
	// fidx: the planted blob is served with the state ETag, plane extraction, 304 and immutability on e=
	rr = get(fmt.Sprintf("/api/v1/ne/glyph/fidx?i=%d&j=%d&p=core", i, j))
	if rr.Code != 200 || rr.Header().Get("Content-Type") != "application/vnd.ne-fidx" || rr.Header().Get("ETag") != `"`+st+`-core"` || rr.Header().Get("X-NE-Fidx-State") != st {
		t.Fatalf("fidx: %d %q %q", rr.Code, rr.Header().Get("ETag"), rr.Header().Get("Content-Type"))
	}
	if got, err := neFidxDecode(rr.Body.Bytes()); err != nil || got.N != b.N || got.Cols["rd"] != nil || got.Cols["d"] == nil {
		t.Errorf("fidx core plane: %v", err)
	}
	req := httptest.NewRequest("GET", fmt.Sprintf("/api/v1/ne/glyph/fidx?i=%d&j=%d&p=ext", i, j), nil)
	req.Header.Set("If-None-Match", `"`+st+`-ext"`)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 304 {
		t.Errorf("fidx If-None-Match: %d", rr.Code)
	}
	rr = get(fmt.Sprintf("/api/v1/ne/glyph/fidx?i=%d&j=%d&p=all&e=%s", i, j, st))
	if rr.Code != 200 || !strings.Contains(rr.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("fidx e= should be immutable: %d %q", rr.Code, rr.Header().Get("Cache-Control"))
	}
	rr = get(fmt.Sprintf("/api/v1/ne/glyph/fidx?i=%d&j=%d&p=all&e=stale", i, j))
	if rr.Code != 200 || strings.Contains(rr.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("fidx stale e= must not be immutable: %d %q", rr.Code, rr.Header().Get("Cache-Control"))
	}
	for _, p := range []string{"/api/v1/ne/glyph/fidx", "/api/v1/ne/glyph/fidx?i=x&j=1", "/api/v1/ne/glyph/fidx?i=1&j=1&p=bogus", "/api/v1/ne/glyph/fidx?i=99999&j=1"} {
		if rr := get(p); rr.Code != 400 && rr.Code != 404 {
			t.Errorf("%s → %d", p, rr.Code)
		}
	}
	// the Worker source is served as JS with the fields it needs
	rr = get("/api/v1/ne/glyph/filter.js")
	if rr.Code != 200 || !strings.HasPrefix(rr.Header().Get("Content-Type"), "text/javascript") && !strings.HasPrefix(rr.Header().Get("Content-Type"), "application/javascript") || !strings.Contains(rr.Body.String(), "self.onmessage") {
		t.Errorf("filter.js: %d %q", rr.Code, rr.Header().Get("Content-Type"))
	}
}

// the public NE prefix is allow-listed for the new routes and the landing page carries the filter UI
func TestNEFilterPublicSurface(t *testing.T) {
	for _, p := range []string{"/api/v1/ne/filter", "/api/v1/ne/filter/fields", "/api/v1/ne/glyph/fidx", "/api/v1/ne/glyph/filter.js"} {
		if !isPublicPath(p) {
			t.Errorf("%s must be public", p)
		}
	}
	// the page pieces: the row + button HTML are spliced into the figure, the JS block into the script, the CSS into the sheet
	src, err := os.ReadFile("ne_glyph.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"neGlyphFiltBtn", "neGlyphFiltHTML", "neGlyphFiltCSS", "neGlyphFiltJS", "fltView(", "fltClear()", "'f='+", "fltLegend()", "fltCardRow("} {
		if !strings.Contains(string(src), s) {
			t.Errorf("ne_glyph.go does not wire %q", s)
		}
	}
	for _, s := range []string{`class="glyph-filt"`, "gf-in", "gf-chips", "gf-n"} {
		if !strings.Contains(neGlyphFiltHTML, s) {
			t.Errorf("filter row lacks %q", s)
		}
	}
	if !strings.Contains(neGlyphFiltBtn, "gt-flt") || !strings.Contains(neGlyphScript, "/api/v1/ne/glyph/filter.js") || !strings.Contains(neGlyphScript, "fltView") || !strings.Contains(neGlyphCSS, ".glyph-filt") || !strings.Contains(neGlyphCSS, ".ov-flt") {
		t.Errorf("filter UI pieces missing from the page constants")
	}
	// the Worker file and the page agree on the message protocol
	js, _ := os.ReadFile("ne_glyph_filter.js")
	for _, m := range []string{"m.t==='init'", "m.t==='expr'", "m.t==='want'", "m.t==='at'", "m.t==='theme'", "t:'tile'", "t:'bin'", "t:'cat'"} {
		if !strings.Contains(string(js), m) {
			t.Errorf("worker lacks %q", m)
		}
	}
	for _, m := range []string{"m.t==='cat'", "m.t==='expr'", "m.t==='bin'", "m.t==='tile'", "m.t==='at'", "t:'want'", "t:'init'", "t:'expr'"} {
		if !strings.Contains(neGlyphFiltJS, m) {
			t.Errorf("page lacks %q", m)
		}
	}
	for _, f := range []string{"llm.txt", "docs/ne-filter.md", "docs/ne-glyph.md", "docs/payload-caching-contract.md"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if !strings.Contains(string(b), "/api/v1/ne/filter") {
			t.Errorf("%s does not mention /api/v1/ne/filter", f)
		}
	}
	if !strings.Contains(publicNELLMSection, "/api/v1/ne/filter") {
		t.Errorf("llm.txt NE section must document the filter endpoint")
	}
}

var _ = http.StatusOK

// publicNEHandlerForTest — the public tier over the NE routes only (no store fixtures needed).
func publicNEHandlerForTest(t *testing.T) http.Handler {
	t.Helper()
	setTierConfigForTest(true, "0123456789abcdef0123")
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/ne", handleNE)
	mux.HandleFunc("/api/v1/ne/", handleNE)
	return tierMiddleware(mux)
}
