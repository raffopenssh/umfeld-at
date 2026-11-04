package main

// Tile geometry on disk. H3 never changes, so a built neGlyphTileCells is
// valid forever: writing it once (data/ne_geom/<lod>/<i>_<j>.bin, raw ids as
// delta-varints + the float32 centres bit-for-bit as in memory) turns the
// 200 ms cgo build of a LOD-2 tile into a ~3 ms read — across restarts and
// beyond the in-memory LRU. Bounded by neGlyphGeomDiskCap (oldest files go
// first), rebuildable: delete the directory freely. Stats on
// /debug/inflight ne_glyph_geom.disk.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	neGlyphGeomDiskDir = "data/ne_geom"
	neGlyphGeomDiskCap = int64(1 << 30) // 1 GB ≈ 450 LOD-2 tiles
	neGlyphGeomDiskVer = "NGG1"
)

var (
	neGlyphGeomDiskMu     sync.Mutex
	neGlyphGeomDiskSize   int64 = -1 // -1 = not walked yet
	neGlyphGeomDiskHits   int64
	neGlyphGeomDiskWrites int64
	neGlyphGeomDiskBad    int64
)

func neGlyphGeomDiskPath(lod neGlyphLOD, i, j int) string {
	return filepath.Join(neGlyphGeomDiskDir, fmt.Sprint(lodIndex(lod)), fmt.Sprintf("%d_%d.bin", i, j))
}

// neGlyphGeomDiskRead — the tile from disk, nil when absent or damaged
// (a damaged file is removed and rebuilt).
func neGlyphGeomDiskRead(lod neGlyphLOD, i, j int) *neGlyphTileCells {
	p := neGlyphGeomDiskPath(lod, i, j)
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	t, ok := neGlyphGeomDecode(b)
	if !ok {
		atomic.AddInt64(&neGlyphGeomDiskBad, 1)
		_ = os.Remove(p)
		return nil
	}
	w, s, e, n := neGlyphTileBox(lod, i, j)
	x0, y0 := neGlyphProj(w, n)
	x1, y1 := neGlyphProj(e, s)
	if math.Abs(x0-t.x0) > 1e-6 || math.Abs(y0-t.y0) > 1e-6 || math.Abs((x1-x0)-t.fw) > 1e-6 || math.Abs((y1-y0)-t.fh) > 1e-6 {
		atomic.AddInt64(&neGlyphGeomDiskBad, 1) // the frame changed under the file
		_ = os.Remove(p)
		return nil
	}
	atomic.AddInt64(&neGlyphGeomDiskHits, 1)
	now := time.Now()
	_ = os.Chtimes(p, now, now) // mtime = last use, for the sweep
	return t
}

// neGlyphGeomDiskWrite — persist a built tile (best effort, never blocks the
// render: called from a goroutine).
func neGlyphGeomDiskWrite(lod neGlyphLOD, i, j int, t *neGlyphTileCells) {
	p := neGlyphGeomDiskPath(lod, i, j)
	if _, err := os.Stat(p); err == nil {
		return
	}
	b := neGlyphGeomEncode(t)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		_ = os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return
	}
	atomic.AddInt64(&neGlyphGeomDiskWrites, 1)
	neGlyphGeomDiskMu.Lock()
	defer neGlyphGeomDiskMu.Unlock()
	if neGlyphGeomDiskSize < 0 {
		neGlyphGeomDiskSize = neGlyphGeomDiskWalk()
	} else {
		neGlyphGeomDiskSize += int64(len(b))
	}
	if neGlyphGeomDiskSize > neGlyphGeomDiskCap {
		neGlyphGeomDiskSweep()
	}
}

func neGlyphGeomDiskWalk() int64 {
	var sz int64
	_ = filepath.Walk(neGlyphGeomDiskDir, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			sz += fi.Size()
		}
		return nil
	})
	return sz
}

// neGlyphGeomDiskSweep — under the lock: drop the least recently used files
// until 90 % of the cap.
func neGlyphGeomDiskSweep() {
	type f struct {
		p  string
		sz int64
		t  time.Time
	}
	var fs []f
	_ = filepath.Walk(neGlyphGeomDiskDir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			fs = append(fs, f{p, fi.Size(), fi.ModTime()})
		}
		return nil
	})
	sort.Slice(fs, func(a, b int) bool { return fs[a].t.Before(fs[b].t) })
	sz := int64(0)
	for _, x := range fs {
		sz += x.sz
	}
	for _, x := range fs {
		if sz <= neGlyphGeomDiskCap*9/10 {
			break
		}
		if os.Remove(x.p) == nil {
			sz -= x.sz
		}
	}
	neGlyphGeomDiskSize = sz
}

func neGlyphGeomDiskStats() map[string]interface{} {
	neGlyphGeomDiskMu.Lock()
	sz := neGlyphGeomDiskSize
	neGlyphGeomDiskMu.Unlock()
	return map[string]interface{}{
		"bytes": sz, "cap": neGlyphGeomDiskCap, "hits": atomic.LoadInt64(&neGlyphGeomDiskHits),
		"writes": atomic.LoadInt64(&neGlyphGeomDiskWrites), "bad": atomic.LoadInt64(&neGlyphGeomDiskBad),
	}
}

// Layout: "NGG1" · x0 y0 fw fh float64 · off 4×6×2 float32 · n uint32 ·
// n delta-varint ids · 2n float32 centres (little endian).
func neGlyphGeomEncode(t *neGlyphTileCells) []byte {
	var b bytes.Buffer
	b.Grow(4 + 32 + 192 + 4 + 5*len(t.ids) + 8*len(t.ids))
	b.WriteString(neGlyphGeomDiskVer)
	for _, v := range []float64{t.x0, t.y0, t.fw, t.fh} {
		_ = binary.Write(&b, binary.LittleEndian, v)
	}
	_ = binary.Write(&b, binary.LittleEndian, t.off)
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(t.ids)))
	var prev uint64
	var tmp [binary.MaxVarintLen64]byte
	for _, id := range t.ids {
		n := binary.PutUvarint(tmp[:], id-prev)
		b.Write(tmp[:n])
		prev = id
	}
	_ = binary.Write(&b, binary.LittleEndian, t.c)
	return b.Bytes()
}

func neGlyphGeomDecode(b []byte) (*neGlyphTileCells, bool) {
	if len(b) < 4+32+192+4 || string(b[:4]) != neGlyphGeomDiskVer {
		return nil, false
	}
	r := bytes.NewReader(b[4:])
	t := &neGlyphTileCells{}
	for _, p := range []*float64{&t.x0, &t.y0, &t.fw, &t.fh} {
		if binary.Read(r, binary.LittleEndian, p) != nil {
			return nil, false
		}
	}
	if binary.Read(r, binary.LittleEndian, &t.off) != nil {
		return nil, false
	}
	var n uint32
	if binary.Read(r, binary.LittleEndian, &n) != nil || n > 4<<20 {
		return nil, false
	}
	t.ids = make([]uint64, n)
	var prev uint64
	for k := range t.ids {
		d, err := binary.ReadUvarint(r)
		if err != nil || (k > 0 && d == 0) {
			return nil, false
		}
		prev += d
		t.ids[k] = prev
	}
	t.c = make([]float32, 2*n)
	if binary.Read(r, binary.LittleEndian, t.c) != nil || r.Len() != 0 {
		return nil, false
	}
	t.size = int64(16*len(t.ids)) + 512
	return t, true
}
