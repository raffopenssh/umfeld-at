// ne_dist_raster.go — the NATIONAL road / water distance raster behind the
// sample store's `rd` / `wat` columns (ne_mvt_samp.go, docs/ne-filter.md §8).
//
// One 100 m grid over the OSM extent (rgBox, ≈ 5 900 × 3 100 px): every pixel
// a road (class ≥ road, as the bins' neFidxDist) or water line crosses, every
// pixel inside a water area, then one exact Euclidean distance transform each
// (edt2D, the remoteness grid's) and the distance class of the bins
// (neFiltDistEdges) packed as rd | wat<<3 in one byte. Built once from
// osm_proximity.db (seconds, not 5.6 M rtree queries), kept zstd on disk
// (data/ne_mvt/dist100.bin, ≈ 2 MB; 18 MB decoded, off-heap) and rebuilt
// when the OSM db changes (size + mtime in the header).
package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

const (
	neDistCellM = 100.0
	neDistMagic = "NDST"
	neDistVer   = 1
)

type neDistRaster struct {
	key        string // osm db size:mtime
	w, h       int
	lon0, lat0 float64
	dlon, dlat float64
	v          []byte // rd | wat<<3 (classes into neFiltDistEdges; 7 = none within range)
	ms         int64
}

var (
	neDistMu  sync.Mutex
	neDistCur *neDistRaster
)

func neDistPath() string { return filepath.Join(nePMTDir, "dist100.bin") }

// neDistKey — what the raster is keyed on ("" = no OSM db).
func neDistKey() string {
	st, err := os.Stat(filepath.Join("data", "osm_proximity.db"))
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%d", st.Size(), st.ModTime().Unix())
}

// neDistGet — the raster for the current OSM db: loaded from disk, else built
// (and saved); nil without an OSM db. Serialised; the first caller pays.
func neDistGet() *neDistRaster {
	key := neDistKey()
	if key == "" || osmDB == nil {
		return nil
	}
	neDistMu.Lock()
	defer neDistMu.Unlock()
	if neDistCur != nil && neDistCur.key == key {
		return neDistCur
	}
	if r, err := neDistLoad(neDistPath()); err == nil && r.key == key {
		neDistCur = r
		return r
	}
	t0 := time.Now()
	r, err := neDistBuild(key)
	if err != nil {
		log.Printf("distance raster: %v", err)
		return nil
	}
	r.ms = time.Since(t0).Milliseconds()
	if err := neDistSave(neDistPath(), r); err != nil {
		log.Printf("distance raster: save: %v", err)
	}
	log.Printf("🛣️  distance raster: %d×%d at %.0f m built in %.1f s", r.w, r.h, neDistCellM, float64(r.ms)/1000)
	neDistCur = r
	return r
}

func newNEDistRaster(key string) *neDistRaster {
	mLon, mLat := metresPerDeg((rgBox[1] + rgBox[3]) / 2)
	r := &neDistRaster{key: key, lon0: rgBox[0], lat0: rgBox[1], dlon: neDistCellM / mLon, dlat: neDistCellM / mLat}
	r.w = int(math.Ceil((rgBox[2] - rgBox[0]) / r.dlon))
	r.h = int(math.Ceil((rgBox[3] - rgBox[1]) / r.dlat))
	return r
}

func (r *neDistRaster) idx(lon, lat float64) (int, bool) {
	x := int((lon - r.lon0) / r.dlon)
	y := int((lat - r.lat0) / r.dlat)
	if x < 0 || y < 0 || x >= r.w || y >= r.h {
		return 0, false
	}
	return y*r.w + x, true
}

// At — (rd class, wat class) under a point; 255 outside the box.
func (r *neDistRaster) At(lon, lat float64) (rd, wat uint8) {
	if r == nil {
		return 255, 255
	}
	i, ok := r.idx(lon, lat)
	if !ok {
		return 255, 255
	}
	v := r.v[i]
	return v & 7, v >> 3
}

func (r *neDistRaster) seg(mark []bool, x0, y0, x1, y1 float64) {
	steps := int(math.Max(math.Abs(x1-x0)/r.dlon, math.Abs(y1-y0)/r.dlat)*2) + 1
	if steps > 200000 {
		return
	}
	for s := 0; s <= steps; s++ {
		t := float64(s) / float64(steps)
		if i, ok := r.idx(x0+(x1-x0)*t, y0+(y1-y0)*t); ok {
			mark[i] = true
		}
	}
}

// fill — even-odd scanline fill of one ring into mark.
func (r *neDistRaster) fill(mark []bool, ring [][2]float64) {
	if len(ring) < 3 {
		return
	}
	minY, maxY := math.Inf(1), math.Inf(-1)
	for _, p := range ring {
		minY, maxY = math.Min(minY, p[1]), math.Max(maxY, p[1])
	}
	y0, y1 := int(math.Floor((minY-r.lat0)/r.dlat)), int(math.Ceil((maxY-r.lat0)/r.dlat))
	if y0 < 0 {
		y0 = 0
	}
	if y1 >= r.h {
		y1 = r.h - 1
	}
	var xs []float64
	for y := y0; y <= y1; y++ {
		cy := r.lat0 + (float64(y)+0.5)*r.dlat
		xs = xs[:0]
		for k := 0; k < len(ring); k++ {
			p, q := ring[k], ring[(k+1)%len(ring)]
			if (p[1] > cy) == (q[1] > cy) {
				continue
			}
			xs = append(xs, p[0]+(cy-p[1])/(q[1]-p[1])*(q[0]-p[0]))
		}
		if len(xs) < 2 {
			continue
		}
		sort.Float64s(xs)
		for k := 0; k+1 < len(xs); k += 2 {
			a := int(math.Ceil((xs[k]-r.lon0)/r.dlon - 0.5))
			b := int(math.Floor((xs[k+1]-r.lon0)/r.dlon - 0.5))
			if a < 0 {
				a = 0
			}
			if b >= r.w {
				b = r.w - 1
			}
			for x := a; x <= b; x++ {
				mark[y*r.w+x] = true
			}
		}
	}
}

func neDistBuild(key string) (*neDistRaster, error) {
	if osmDB == nil {
		return nil, errors.New("osm db not open")
	}
	r := newNEDistRaster(key)
	n := r.w * r.h
	road, water := make([]bool, n), make([]bool, n)
	rows, err := osmDB.Query(`SELECT cat, major, fclass, coords FROM osm_lines WHERE cat IN ('road','water')`)
	if err != nil {
		return nil, err
	}
	nl := 0
	for rows.Next() {
		var cat, fclass string
		var major int
		var coords []byte
		if rows.Scan(&cat, &major, &fclass, &coords) != nil {
			continue
		}
		m := water
		if cat == "road" {
			if cls, ok := neRoadClassOf(cat, major, fclass); !ok || cls == neRoadMinor {
				continue
			}
			m = road
		}
		pts := unpackCoords(coords)
		for k := 1; k < len(pts); k++ {
			r.seg(m, pts[k-1][0], pts[k-1][1], pts[k][0], pts[k][1])
		}
		nl++
	}
	rows.Close()
	if nl == 0 {
		return nil, errors.New("no lines in osm_lines")
	}
	if OSMAreasReady() {
		if rows, err := osmDB.Query(`SELECT wkb FROM osm_areas WHERE cat='water'`); err == nil {
			for rows.Next() {
				var raw []byte
				if rows.Scan(&raw) != nil {
					continue
				}
				mp, err := unmarshalGeomWKB(raw)
				if err != nil {
					continue
				}
				for _, poly := range mp {
					for _, ring := range poly {
						rg := make([][2]float64, len(ring))
						for k, p := range ring {
							rg[k] = [2]float64{p[0], p[1]}
						}
						r.fill(water, rg)
					}
				}
			}
			rows.Close()
		}
	}
	r.v = make([]byte, n)
	classOf := func(d2 float32) uint8 {
		m := math.Sqrt(float64(d2)) * neDistCellM
		if d2 >= 1e12 {
			m = 1e9
		}
		return uint8(neFiltBucketOf(neFiltDistEdges, m))
	}
	dr := edt2D(road, r.w, r.h)
	for i := range r.v {
		r.v[i] = classOf(dr[i])
	}
	dr = nil
	dw := edt2D(water, r.w, r.h)
	for i := range r.v {
		r.v[i] |= classOf(dw[i]) << 3
	}
	return r, nil
}

func neDistSave(path string, r *neDistRaster) error {
	hdr, _ := json.Marshal(map[string]interface{}{"key": r.key, "w": r.w, "h": r.h, "cell_m": neDistCellM, "ms": r.ms, "built": time.Now().UTC().Format(time.RFC3339)})
	raw := make([]byte, 0, 9+len(hdr)+len(r.v))
	raw = append(raw, neDistMagic...)
	raw = append(raw, neDistVer)
	raw = binary.LittleEndian.AppendUint32(raw, uint32(len(hdr)))
	raw = append(raw, hdr...)
	raw = append(raw, r.v...)
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBetterCompression))
	if err != nil {
		return err
	}
	z := enc.EncodeAll(raw, nil)
	enc.Close()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path+".tmp", z, 0o644); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func neDistLoad(path string) (*neDistRaster, error) {
	z, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	raw, err := dec.DecodeAll(z, nil)
	if err != nil {
		return nil, err
	}
	if len(raw) < 9 || string(raw[:4]) != neDistMagic || raw[4] != neDistVer {
		return nil, errors.New("not a distance raster")
	}
	hl := int(binary.LittleEndian.Uint32(raw[5:]))
	if 9+hl > len(raw) {
		return nil, errors.New("truncated")
	}
	var h struct {
		Key  string  `json:"key"`
		W    int     `json:"w"`
		H    int     `json:"h"`
		Ms   int64   `json:"ms"`
		Cell float64 `json:"cell_m"`
	}
	if err := json.Unmarshal(raw[9:9+hl], &h); err != nil {
		return nil, err
	}
	r := newNEDistRaster(h.Key)
	if h.Cell != neDistCellM || r.w != h.W || r.h != h.H || len(raw)-9-hl != r.w*r.h {
		return nil, errors.New("raster shape differs")
	}
	r.v = raw[9+hl:]
	r.ms = h.Ms
	return r, nil
}

// NEDistStats — inflight line (under ne_mvt.sample.dist).
func NEDistStats() map[string]interface{} {
	neDistMu.Lock()
	r := neDistCur
	neDistMu.Unlock()
	if r == nil {
		return map[string]interface{}{"loaded": false}
	}
	return map[string]interface{}{"loaded": true, "w": r.w, "h": r.h, "bytes": len(r.v), "build_ms": r.ms}
}
