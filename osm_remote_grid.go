package main

// The remoteness grid — Austria at 250 m, one byte a cell (0 central … 100
// remote, 255 = no data), read by the ROADS overlay to tint every way by how
// far from everything it runs (ne_glyph_roads.go, neRoadInk).
//
// It is the remoteness score of docs/osm-proximity.md WITHOUT the road term
// (which is 0 on a road by definition): weighted sqrt-normalised distance to
// the nearest major road (cap 20 km, w .15), transit stop (10 km, .30) and
// settlement place (15 km, .25), renormalised to 0–100. Built once from
// data/osm_proximity.db by rasterising the three feature sets onto the grid
// and running an exact Euclidean distance transform (Felzenszwalb–Huttenlocher,
// O(n)) — a few seconds — then persisted to data/osm_remote_grid.bin
// (rebuildable, never committed) and mapped on the next start. 3.2 MB in RAM,
// fixed size; size line RemoteGridStats on /debug/inflight (Invariant 10).

import (
	"database/sql"
	"encoding/binary"
	"errors"
	"log"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	rgCellM   = 250.0 // grid pitch, metres
	rgVersion = 1
	rgNoData  = 255
)

// the box every grid covers (lon w, lat s, lon e, lat n) — the osm_lines extent with a margin
var rgBox = [4]float64{8.8, 46.3, 17.3, 49.1}

type remoteGrid struct {
	w, h       int
	lon0, lat0 float64 // south-west corner
	dlon, dlat float64 // cell size, degrees
	v          []uint8
}

var (
	rgMu      sync.RWMutex
	rgGrid    *remoteGrid
	rgOnce    sync.Once
	rgBuildMS int64
	rgSource  string // "file" | "built" | ""
	rgErr     string
)

func remoteGridPath() string { return filepath.Join("data", "osm_remote_grid.bin") }

// remoteGridGet — the grid, or nil while it is not ready (the ink stays neutral).
func remoteGridGet() *remoteGrid {
	rgMu.RLock()
	defer rgMu.RUnlock()
	return rgGrid
}

// remoteGridEnsure — load the file or build from the OSM db, once, in the background.
func remoteGridEnsure(db *sql.DB) {
	rgOnce.Do(func() {
		go func() {
			if g, err := remoteGridLoad(remoteGridPath()); err == nil {
				rgMu.Lock()
				rgGrid, rgSource = g, "file"
				rgMu.Unlock()
				log.Printf("🛣️ remoteness grid: %d×%d from %s", g.w, g.h, remoteGridPath())
				return
			}
			t0 := time.Now()
			g, err := remoteGridBuild(db)
			if err != nil {
				rgMu.Lock()
				rgErr = err.Error()
				rgMu.Unlock()
				log.Printf("🛣️ remoteness grid build failed: %v", err)
				return
			}
			ms := time.Since(t0).Milliseconds()
			rgMu.Lock()
			rgGrid, rgSource, rgBuildMS = g, "built", ms
			rgMu.Unlock()
			if err := g.save(remoteGridPath()); err != nil {
				log.Printf("🛣️ remoteness grid save: %v", err)
			}
			log.Printf("🛣️ remoteness grid: %d×%d built in %d ms", g.w, g.h, ms)
		}()
	})
}

// RemoteGridStats — size line for the debug inflight page.
func RemoteGridStats() map[string]interface{} {
	rgMu.RLock()
	defer rgMu.RUnlock()
	m := map[string]interface{}{"ready": rgGrid != nil, "source": rgSource, "build_ms": rgBuildMS, "cell_m": rgCellM}
	if rgGrid != nil {
		m["w"], m["h"], m["bytes"] = rgGrid.w, rgGrid.h, len(rgGrid.v)
	}
	if rgErr != "" {
		m["error"] = rgErr
	}
	return m
}

func newRemoteGrid() *remoteGrid {
	mLon, mLat := metresPerDeg((rgBox[1] + rgBox[3]) / 2)
	g := &remoteGrid{lon0: rgBox[0], lat0: rgBox[1], dlon: rgCellM / mLon, dlat: rgCellM / mLat}
	g.w = int(math.Ceil((rgBox[2] - rgBox[0]) / g.dlon))
	g.h = int(math.Ceil((rgBox[3] - rgBox[1]) / g.dlat))
	g.v = make([]uint8, g.w*g.h)
	return g
}

// At — remoteness 0–100 under a point, −1 outside the box / no data.
func (g *remoteGrid) At(lon, lat float64) float32 {
	if g == nil {
		return -1
	}
	x := int((lon - g.lon0) / g.dlon)
	y := int((lat - g.lat0) / g.dlat)
	if x < 0 || y < 0 || x >= g.w || y >= g.h {
		return -1
	}
	v := g.v[y*g.w+x]
	if v == rgNoData {
		return -1
	}
	return float32(v)
}

// remoteGridBuild — rasterise major roads, transit stops and places, distance-transform each, combine.
func remoteGridBuild(db *sql.DB) (*remoteGrid, error) {
	if db == nil {
		return nil, errors.New("osm db not open")
	}
	g := newRemoteGrid()
	n := g.w * g.h
	score := make([]float32, n)
	mark := make([]bool, n)
	add := func(cap, weight float64) {
		d := edt2D(mark, g.w, g.h)
		for i := range score {
			v := math.Sqrt(float64(d[i])) * rgCellM / cap
			if v > 1 {
				v = 1
			}
			score[i] += float32(weight * math.Sqrt(v))
			mark[i] = false
		}
	}
	// 1 · major roads (motorway–secondary): every cell a segment crosses
	rows, err := db.Query(`SELECT coords FROM osm_lines WHERE cat='road' AND major=1`)
	if err != nil {
		return nil, err
	}
	nl := 0
	for rows.Next() {
		var b []byte
		if rows.Scan(&b) != nil {
			continue
		}
		pts := unpackCoords(b)
		for i := 1; i < len(pts); i++ {
			g.rasterSeg(mark, pts[i-1][0], pts[i-1][1], pts[i][0], pts[i][1])
		}
		nl++
	}
	rows.Close()
	if nl == 0 {
		return nil, errors.New("no major roads in osm_lines")
	}
	add(20000, 0.15)
	// 2 · transit stops, 3 · settlement places
	for _, p := range []struct {
		cat         string
		cap, weight float64
	}{{"transit", 10000, 0.30}, {"place", 15000, 0.25}} {
		rows, err := db.Query(`SELECT lon, lat FROM osm_points WHERE cat=?`, p.cat)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var lon, lat float64
			if rows.Scan(&lon, &lat) != nil {
				continue
			}
			if i, ok := g.idx(lon, lat); ok {
				mark[i] = true
			}
		}
		rows.Close()
		add(p.cap, p.weight)
	}
	for i, s := range score {
		v := math.Round(float64(s) / 0.70 * 100)
		if v > 100 {
			v = 100
		}
		g.v[i] = uint8(v)
	}
	return g, nil
}

func (g *remoteGrid) idx(lon, lat float64) (int, bool) {
	x := int((lon - g.lon0) / g.dlon)
	y := int((lat - g.lat0) / g.dlat)
	if x < 0 || y < 0 || x >= g.w || y >= g.h {
		return 0, false
	}
	return y*g.w + x, true
}

// rasterSeg — mark every cell a segment passes (half-cell steps).
func (g *remoteGrid) rasterSeg(mark []bool, x0, y0, x1, y1 float64) {
	steps := int(math.Max(math.Abs(x1-x0)/g.dlon, math.Abs(y1-y0)/g.dlat)*2) + 1
	for s := 0; s <= steps; s++ {
		t := float64(s) / float64(steps)
		if i, ok := g.idx(x0+(x1-x0)*t, y0+(y1-y0)*t); ok {
			mark[i] = true
		}
	}
}

// edt2D — squared Euclidean distance (in cells) from every cell to the nearest
// marked one; Felzenszwalb & Huttenlocher's separable lower-envelope transform.
func edt2D(mark []bool, w, h int) []float32 {
	const inf = float32(1e20)
	d := make([]float32, w*h)
	for i, m := range mark {
		if m {
			d[i] = 0
		} else {
			d[i] = inf
		}
	}
	mx := w
	if h > mx {
		mx = h
	}
	f := make([]float32, mx)
	out := make([]float32, mx)
	v := make([]int, mx)
	z := make([]float64, mx+1)
	// columns
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			f[y] = d[y*w+x]
		}
		edt1D(f[:h], out[:h], v, z)
		for y := 0; y < h; y++ {
			d[y*w+x] = out[y]
		}
	}
	// rows
	for y := 0; y < h; y++ {
		row := d[y*w : (y+1)*w]
		copy(f[:w], row)
		edt1D(f[:w], out[:w], v, z)
		copy(row, out[:w])
	}
	return d
}

// edt1D — 1-D squared distance transform of a sampled function f (inf where no feature).
func edt1D(f, out []float32, v []int, z []float64) {
	n := len(f)
	k := 0
	v[0] = 0
	z[0] = -1e30
	z[1] = 1e30
	for q := 1; q < n; q++ {
		var s float64
		for {
			p := v[k]
			s = ((float64(f[q]) + float64(q*q)) - (float64(f[p]) + float64(p*p))) / float64(2*(q-p))
			if s <= z[k] {
				k--
				continue
			}
			break
		}
		k++
		v[k] = q
		z[k] = s
		z[k+1] = 1e30
	}
	k = 0
	for q := 0; q < n; q++ {
		for z[k+1] < float64(q) {
			k++
		}
		dq := float32(q - v[k])
		out[q] = dq*dq + f[v[k]]
	}
}

// ── persistence: 32-byte header (magic, version, w, h, box) + the bytes ──

func (g *remoteGrid) save(path string) error {
	hdr := make([]byte, 32)
	copy(hdr, "RGRD")
	binary.LittleEndian.PutUint32(hdr[4:], rgVersion)
	binary.LittleEndian.PutUint32(hdr[8:], uint32(g.w))
	binary.LittleEndian.PutUint32(hdr[12:], uint32(g.h))
	binary.LittleEndian.PutUint32(hdr[16:], uint32(rgCellM))
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err = f.Write(hdr); err == nil {
		_, err = f.Write(g.v)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func remoteGridLoad(path string) (*remoteGrid, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) < 32 || string(b[:4]) != "RGRD" || binary.LittleEndian.Uint32(b[4:]) != rgVersion || binary.LittleEndian.Uint32(b[16:]) != uint32(rgCellM) {
		return nil, errors.New("remote grid: bad header")
	}
	g := newRemoteGrid()
	if int(binary.LittleEndian.Uint32(b[8:])) != g.w || int(binary.LittleEndian.Uint32(b[12:])) != g.h || len(b)-32 != g.w*g.h {
		return nil, errors.New("remote grid: size mismatch")
	}
	copy(g.v, b[32:])
	return g, nil
}
