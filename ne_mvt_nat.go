// ne_mvt_nat.go — the NATIONAL aggregate behind the cell tiles' low zooms.
//
// Zooms 9–11 (res 7–9) of /api/v1/ne/mvt and the "Austria" PMTiles archive
// do not read filter-index bins (a bin is ~9 s to build, Austria is ~16 000
// of them). They read one in-RAM table instead: every res-9 cell of the
// country with the counts the aggregate attributes need — declared groups,
// observed groups, both/diff, confirmed/candidate change — folded once from
// the declared view of all 7 850 KGs (+ obs rows, + chunk log), ~3 min on
// one core, kept on disk (data/ne_mvt/nat.bin, zstd, ≈ 10 MB) and reloaded
// at start. Res 8 and 7 are folded from res 9 at build time, so a z-9 tile
// of the whole country is a scan of 20 k rows.
//
// Peer-derived columns (subsidy, LiDAR, Natura 2000) are not in the
// national table: those attributes appear from z 12 (res 10) up, where the
// bins supply them. The TileJSON says so per field.
//
// Rebuild: at start when the file is missing, then whenever the store state
// (neGlyphTileState over all KGs) differs from the table's AND the table is
// older than neNatMinAge, checked hourly; one builder at a time; progress
// on /ne/mvt/status (`national`) and the inflight page (`ne_mvt.national`).
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/uber/h3-go/v4"
)

const (
	neNatRes    = 9
	neNatMinAge = 6 * time.Hour
	neNatMagic  = "NNAT"
	neNatVer    = 1
)

// neNatRow — the per-parent counts (50 B). D/O index 9 = unknown.
type neNatRow struct {
	N, Both, Diff, ChgC, ChgK uint16
	D, O                      [10]uint16
}

const neNatRowBytes = 50

type neNatLevel struct {
	Res  int
	IDs  []uint64 // sorted
	Lon  []float32
	Lat  []float32
	Rows []neNatRow
}

type neNat struct {
	State string
	Built time.Time
	KGs   int
	Cells int
	Lv    [3]*neNatLevel // res 9, 8, 7
	ms    int64
}

var (
	neNatMu       sync.RWMutex
	neNatCur      *neNat
	neNatBuilding atomic.Bool
	neNatProgress atomic.Int64 // KGs done in the running build
	neNatTotal    atomic.Int64
	neNatOnce     sync.Once
)

func neNatPath() string { return filepath.Join(nePMTDir, "nat.bin") }

func neNatGet() *neNat {
	neNatMu.RLock()
	defer neNatMu.RUnlock()
	return neNatCur
}

func (n *neNat) bytes() int64 {
	if n == nil {
		return 0
	}
	var b int64
	for _, l := range n.Lv {
		if l != nil {
			b += int64(len(l.IDs)) * (8 + 8 + neNatRowBytes)
		}
	}
	return b
}

// neNatAllKGs — every KG code the store holds, sorted.
func neNatAllKGs() []string {
	neStore.mu.RLock()
	out := make([]string, 0, len(neStore.byKG))
	for kg := range neStore.byKG {
		out = append(out, kg)
	}
	neStore.mu.RUnlock()
	sort.Strings(out)
	return out
}

func neNatState() string {
	return "nat" + string(rune('0'+neNatVer)) + ":" + neGlyphTileState(neNatAllKGs())
}

// neNatStart — load the file, then keep the table fresh. Called once from main.
func neNatStart() {
	neNatOnce.Do(func() {
		if n, err := neNatLoad(neNatPath()); err == nil {
			neNatMu.Lock()
			neNatCur = n
			neNatMu.Unlock()
			log.Printf("🗺️  national cell table loaded: %d res-9 cells, state %s, built %s", len(n.Lv[0].IDs), n.State[:12], n.Built.Format(time.RFC3339))
		} else if !errors.Is(err, os.ErrNotExist) {
			log.Printf("national cell table: %v (rebuilding)", err)
		}
		go func() {
			time.Sleep(90 * time.Second) // let the store and the glyph settle after a restart
			for {
				neNatMaybeBuild()
				time.Sleep(time.Hour)
			}
		}()
	})
}

func neNatMaybeBuild() {
	cur := neNatGet()
	if cur != nil && (time.Since(cur.Built) < neNatMinAge || cur.State == neNatState()) {
		return
	}
	neNatBuild()
}

// neNatBuild — one full pass; a second caller while one runs returns at once.
func neNatBuild() *neNat {
	if !neNatBuilding.CompareAndSwap(false, true) {
		return nil
	}
	defer neNatBuilding.Store(false)
	t0 := time.Now()
	st := neNatState()
	kgs := neNatAllKGs()
	neNatTotal.Store(int64(len(kgs)))
	neNatProgress.Store(0)
	agg := make(map[uint64]*neNatRow, 1<<20)
	cells, held := 0, 0
	var obsBuf []byte
	for _, kg := range kgs {
		neNatProgress.Add(1)
		v := neDeclViewOf(kg)
		if v == nil || len(v.Rows) == 0 {
			continue
		}
		held++
		k := neStore.get(kg)
		var obs []byte
		var lay *neObsLayoutT
		if k != nil {
			if r, _, err := neStore.layerRows(k, "obs"); err == nil && len(r) > 0 {
				rec := ""
				if h := k.layerHeader("obs"); h != nil {
					rec, _ = h["record"].(string)
				}
				lay = neObsLayout(neObsFields(k), rec)
				if lay != nil && lay.rowBytes > 0 && lay.luOff >= 0 && len(r)%lay.rowBytes == 0 {
					obs = r
				}
			}
		}
		var cum map[uint64]*neCellCum
		if c := neChangeGet(kg); c != nil {
			if d, err := c.deltasOf(); err == nil {
				cum = c.cumWin(d, neChangeWin{})
			}
		}
		rows := v.Rows
		oi, on := 0, 0
		if obs != nil {
			on = len(obs) / lay.rowBytes
		}
		for o := 0; o+neLURowBytes <= len(rows); o += neLURowBytes {
			row := rows[o : o+neLURowBytes]
			id := binary.LittleEndian.Uint64(row)
			// observed row of the same cell: both arrays are id-sorted → lockstep
			var orow []byte
			for oi < on {
				oid := binary.LittleEndian.Uint64(obs[oi*lay.rowBytes:])
				if oid < id {
					oi++
					continue
				}
				if oid == id {
					orow = obs[oi*lay.rowBytes : (oi+1)*lay.rowBytes]
				}
				break
			}
			if row[8] < 128 { // split cell counted by its majority KG only
				continue
			}
			cells++
			p := neH3ParentFast(id, neNatRes)
			a := agg[p]
			if a == nil {
				a = &neNatRow{}
				agg[p] = a
			}
			a.N++
			d := 9
			if best, _, sum := neFidxLUBest(row[12:21]); sum > 0 {
				d = best
			}
			a.D[d]++
			ob := 9
			if orow != nil {
				if best, _, sum := neFidxLUBest(orow[lay.luOff : lay.luOff+9]); sum > 0 {
					ob = best
				}
			}
			a.O[ob]++
			if d < 9 && ob < 9 {
				a.Both++
				if d != ob {
					a.Diff++
				}
			}
			if cu := cum[id]; cu != nil && cu.Share >= 128 {
				if cu.Confirmed {
					a.ChgC++
				} else if cu.Candidate {
					a.ChgK++
				}
			}
		}
		_ = obsBuf
	}
	n := &neNat{State: st, Built: time.Now(), KGs: held, Cells: cells}
	n.Lv[0] = neNatLevelOf(agg, neNatRes)
	n.Lv[1] = n.Lv[0].fold(8)
	n.Lv[2] = n.Lv[1].fold(7)
	n.ms = time.Since(t0).Milliseconds()
	if err := neNatSave(neNatPath(), n); err != nil {
		log.Printf("national cell table: save: %v", err)
	}
	neNatMu.Lock()
	neNatCur = n
	neNatMu.Unlock()
	neMVTLRU.dropPrefix("nat/")
	log.Printf("🗺️  national cell table built: %d KGs, %d cells, %d res-9 · %d res-8 · %d res-7 parents, %.1f s", held, cells, len(n.Lv[0].IDs), len(n.Lv[1].IDs), len(n.Lv[2].IDs), float64(n.ms)/1000)
	return n
}

func neNatLevelOf(agg map[uint64]*neNatRow, res int) *neNatLevel {
	l := &neNatLevel{Res: res, IDs: make([]uint64, 0, len(agg))}
	for p := range agg {
		l.IDs = append(l.IDs, p)
	}
	sortU64(l.IDs)
	l.Rows = make([]neNatRow, len(l.IDs))
	l.Lon = make([]float32, len(l.IDs))
	l.Lat = make([]float32, len(l.IDs))
	for q, p := range l.IDs {
		l.Rows[q] = *agg[p]
		lon, lat := neCellCentre(p)
		l.Lon[q], l.Lat[q] = float32(lon), float32(lat)
	}
	return l
}

func (l *neNatLevel) fold(res int) *neNatLevel {
	agg := make(map[uint64]*neNatRow, len(l.IDs)/6+1)
	for q, id := range l.IDs {
		p := neH3ParentFast(id, res)
		a := agg[p]
		if a == nil {
			a = &neNatRow{}
			agg[p] = a
		}
		r := &l.Rows[q]
		a.N += r.N
		a.Both += r.Both
		a.Diff += r.Diff
		a.ChgC += r.ChgC
		a.ChgK += r.ChgK
		for g := 0; g < 10; g++ {
			a.D[g] += r.D[g]
			a.O[g] += r.O[g]
		}
	}
	return neNatLevelOf(agg, res)
}

// level — the table for a resolution (7, 8 or 9), nil otherwise.
func (n *neNat) level(res int) *neNatLevel {
	if n == nil || res < 7 || res > 9 {
		return nil
	}
	return n.Lv[9-res]
}

// inBox — row indices whose centre lies in the box (one linear scan; res 9 is 1 M rows ≈ 1 ms).
func (l *neNatLevel) inBox(w, s, e, n float64) []int {
	var out []int
	fw, fs, fe, fn := float32(w), float32(s), float32(e), float32(n)
	for q := range l.IDs {
		if lo, la := l.Lon[q], l.Lat[q]; lo >= fw && lo < fe && la >= fs && la < fn {
			out = append(out, q)
		}
	}
	return out
}

// agg — the row as the tile renderer's aggregate (peer columns empty).
func (r *neNatRow) agg() *neMVTAgg {
	a := &neMVTAgg{n: int(r.N), both: int(r.Both), diff: int(r.Diff)}
	for g := 0; g < 10; g++ {
		a.d[g], a.o[g] = int(r.D[g]), int(r.O[g])
	}
	a.chg[2], a.chg[1] = int(r.ChgC), int(r.ChgK)
	return a
}

// ── disk ─────────────────────────────────────────────────────────────────────

func neNatSave(path string, n *neNat) error {
	var raw bytes.Buffer
	hdr, _ := json.Marshal(map[string]interface{}{"state": n.State, "built": n.Built.Format(time.RFC3339), "kgs": n.KGs, "cells": n.Cells, "ms": n.ms})
	raw.WriteString(neNatMagic)
	raw.WriteByte(neNatVer)
	var u32 [4]byte
	binary.LittleEndian.PutUint32(u32[:], uint32(len(hdr)))
	raw.Write(u32[:])
	raw.Write(hdr)
	for _, l := range n.Lv {
		binary.LittleEndian.PutUint32(u32[:], uint32(len(l.IDs)))
		raw.Write(u32[:])
		raw.WriteByte(byte(l.Res))
		for _, id := range l.IDs {
			var b [8]byte
			binary.LittleEndian.PutUint64(b[:], id)
			raw.Write(b[:])
		}
		for q := range l.IDs {
			binary.LittleEndian.PutUint32(u32[:], float32bits(l.Lon[q]))
			raw.Write(u32[:])
			binary.LittleEndian.PutUint32(u32[:], float32bits(l.Lat[q]))
			raw.Write(u32[:])
		}
		var rb [neNatRowBytes]byte
		for q := range l.Rows {
			neNatRowPut(rb[:], &l.Rows[q])
			raw.Write(rb[:])
		}
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBetterCompression))
	if err != nil {
		return err
	}
	z := enc.EncodeAll(raw.Bytes(), nil)
	enc.Close()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, z, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func neNatLoad(path string) (*neNat, error) {
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
	if len(raw) < 9 || string(raw[:4]) != neNatMagic || raw[4] != neNatVer {
		return nil, errors.New("not a national cell table")
	}
	hl := int(binary.LittleEndian.Uint32(raw[5:]))
	o := 9
	if o+hl > len(raw) {
		return nil, errors.New("truncated header")
	}
	var h struct {
		State string `json:"state"`
		Built string `json:"built"`
		KGs   int    `json:"kgs"`
		Cells int    `json:"cells"`
		Ms    int64  `json:"ms"`
	}
	if err := json.Unmarshal(raw[o:o+hl], &h); err != nil {
		return nil, err
	}
	o += hl
	n := &neNat{State: h.State, KGs: h.KGs, Cells: h.Cells, ms: h.Ms}
	n.Built, _ = time.Parse(time.RFC3339, h.Built)
	for i := range n.Lv {
		if o+5 > len(raw) {
			return nil, errors.New("truncated level")
		}
		cnt := int(binary.LittleEndian.Uint32(raw[o:]))
		res := int(raw[o+4])
		o += 5
		need := cnt*8 + cnt*8 + cnt*neNatRowBytes
		if o+need > len(raw) {
			return nil, errors.New("truncated rows")
		}
		l := &neNatLevel{Res: res, IDs: make([]uint64, cnt), Lon: make([]float32, cnt), Lat: make([]float32, cnt), Rows: make([]neNatRow, cnt)}
		for q := 0; q < cnt; q++ {
			l.IDs[q] = binary.LittleEndian.Uint64(raw[o+8*q:])
		}
		o += 8 * cnt
		for q := 0; q < cnt; q++ {
			l.Lon[q] = float32frombits(binary.LittleEndian.Uint32(raw[o+8*q:]))
			l.Lat[q] = float32frombits(binary.LittleEndian.Uint32(raw[o+8*q+4:]))
		}
		o += 8 * cnt
		for q := 0; q < cnt; q++ {
			neNatRowGet(raw[o+q*neNatRowBytes:], &l.Rows[q])
		}
		o += cnt * neNatRowBytes
		n.Lv[i] = l
	}
	return n, nil
}

func neNatRowPut(b []byte, r *neNatRow) {
	vals := [25]uint16{r.N, r.Both, r.Diff, r.ChgC, r.ChgK}
	copy(vals[5:], r.D[:])
	copy(vals[15:], r.O[:])
	for i, v := range vals {
		binary.LittleEndian.PutUint16(b[2*i:], v)
	}
}

func neNatRowGet(b []byte, r *neNatRow) {
	var vals [25]uint16
	for i := range vals {
		vals[i] = binary.LittleEndian.Uint16(b[2*i:])
	}
	r.N, r.Both, r.Diff, r.ChgC, r.ChgK = vals[0], vals[1], vals[2], vals[3], vals[4]
	copy(r.D[:], vals[5:15])
	copy(r.O[:], vals[15:25])
}

// NENatStats — inflight / status line.
func NENatStats() map[string]interface{} {
	n := neNatGet()
	m := map[string]interface{}{"loaded": n != nil, "building": neNatBuilding.Load(), "bytes": n.bytes()}
	if n != nil {
		m["state"] = n.State
		m["built"] = n.Built.Format(time.RFC3339)
		m["age_h"] = neRound(time.Since(n.Built).Hours(), 1)
		m["kgs"], m["cells"], m["build_ms"] = n.KGs, n.Cells, n.ms
		m["res9"], m["res8"], m["res7"] = len(n.Lv[0].IDs), len(n.Lv[1].IDs), len(n.Lv[2].IDs)
	}
	if neNatBuilding.Load() {
		m["progress"] = map[string]int64{"kgs_done": neNatProgress.Load(), "kgs_total": neNatTotal.Load()}
	}
	return m
}

// neNatTileState — what a tile from the table is keyed on ("" = no table yet).
func neNatTileState() string {
	if n := neNatGet(); n != nil {
		return n.State
	}
	return ""
}

var _ = h3.Cell(0)

func float32bits(f float32) uint32     { return math.Float32bits(f) }
func float32frombits(u uint32) float32 { return math.Float32frombits(u) }
