// ne_mvt_nat_trees.go — the trees of the cell tiles' low zooms (z 9–11, res
// 7–9, no filter): apices per 5 m height band and the tallest apex per
// hexagon, from the observed-layer peer's national table
// (/api/v1/cells/national?res=9&fields=tree_n,canopy_h_top,tree_hh — the same
// apices as its stand index and the bins' apex lists, sums equal apex for
// apex). Res 8 and 7 are folded here.
//
// One fetch per peer generation (≈ 10 MB gzip, the peer rebuilds at most
// daily): a ~1 KB meta probe every neNatTreesEvery says whether the gen
// moved. The folded table is a file (data/ne_mvt/nat_trees.bin, ≈ 50 MB,
// rebuildable — delete freely) mapped read-only: no heap, pages come and go
// with the tiles that read them. Size line: debug/inflight → ne_mvt_nat_trees.
//
//	"NNT1" | u32 header_len | header JSON | pad to 8 |
//	per level (res 9, 8, 7): n × u64 ids ascending | n × 52 B rows (12 × u32 bands, u32 tallest raw 0.5 m)
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	neNatTreesMagic = "NNT1"
	neNatTreesRow   = 4*neTreeBands + 4
	neNatTreesEvery = 3 * time.Hour
	neNatTreesMax   = 96 << 20 // the peer's body, inflated
)

type neNatTreesHdr struct {
	Gen   string `json:"gen"`
	Epoch string `json:"epoch"`
	Built string `json:"built_at"`
	N     [3]int `json:"n"` // rows at res 9, 8, 7
	Trees int64  `json:"trees"`
}

type neNatTrees struct {
	neNatTreesHdr
	data []byte // the mapped file
	ids  [3]int // offsets
	rows [3]int
}

var neNatTreesState struct {
	cur             atomic.Pointer[neNatTrees]
	mu              sync.Mutex
	checked, loaded time.Time
	err             string
	fetches         int
}

var neNatTreesBase = func() string { return peers().ObsAPI + "/api/v1/cells/national" } // var: tests point it at a stub

func neNatTreesPath() string { return filepath.Join(nePMTDir, "nat_trees.bin") }

func neNatTreesGet() *neNatTrees { return neNatTreesState.cur.Load() }

// state — the tile-key part ("" without a table).
func (t *neNatTrees) state() string {
	if t == nil {
		return ""
	}
	return "+t" + t.Gen[:min(len(t.Gen), 10)]
}

// fill — the trees of hexagon id (res 7–9). With a table every hexagon has a
// figure: no row = no tree.
func (t *neNatTrees) fill(a *neTreeAgg, res int, id uint64) {
	if t == nil || res < 7 || res > 9 {
		return
	}
	a.est = true
	lv := 9 - res
	n, at := t.N[lv], t.ids[lv]
	q := sort.Search(n, func(i int) bool { return binary.LittleEndian.Uint64(t.data[at+8*i:]) >= id })
	if q == n || binary.LittleEndian.Uint64(t.data[at+8*q:]) != id {
		return
	}
	r := t.data[t.rows[lv]+q*neNatTreesRow:]
	for b := 0; b < neTreeBands; b++ {
		c := int64(binary.LittleEndian.Uint32(r[4*b:]))
		a.bands[b] = c
		a.n += c
	}
	a.top = int(binary.LittleEndian.Uint32(r[4*neTreeBands:]))
}

// neNatTreesOpen — map a file written by neNatTreesWrite.
func neNatTreesOpen(path string) (*neNatTrees, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() < 16 {
		return nil, errors.New("short file")
	}
	data, err := syscall.Mmap(int(f.Fd()), 0, int(st.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, err
	}
	t := &neNatTrees{data: data}
	bad := func(m string) (*neNatTrees, error) { syscall.Munmap(data); return nil, errors.New(m) }
	if string(data[:4]) != neNatTreesMagic {
		return bad("magic")
	}
	hl := int(binary.LittleEndian.Uint32(data[4:]))
	if 8+hl > len(data) || json.Unmarshal(data[8:8+hl], &t.neNatTreesHdr) != nil {
		return bad("header")
	}
	o := (8 + hl + 7) &^ 7
	for lv := 0; lv < 3; lv++ {
		if t.N[lv] < 0 {
			return bad("rows")
		}
		t.ids[lv] = o
		o += 8 * t.N[lv]
		t.rows[lv] = o
		o += neNatTreesRow * t.N[lv]
	}
	if o != len(data) {
		return bad("length")
	}
	return t, nil
}

func neNatTreesFold(src map[uint64]*[neTreeBands + 1]uint32, res int) map[uint64]*[neTreeBands + 1]uint32 {
	out := make(map[uint64]*[neTreeBands + 1]uint32, len(src)/6+1)
	for id, r := range src {
		p := neH3ParentFast(id, res)
		a := out[p]
		if a == nil {
			a = &[neTreeBands + 1]uint32{}
			out[p] = a
		}
		for b := 0; b < neTreeBands; b++ {
			a[b] += r[b]
		}
		if r[neTreeBands] > a[neTreeBands] {
			a[neTreeBands] = r[neTreeBands]
		}
	}
	return out
}

// neNatTreesParse — the peer's NEX1 body (res 9) → rows of hexagons holding a tree.
func neNatTreesParse(body []byte) (map[uint64]*[neTreeBands + 1]uint32, *neNatTreesHdr, error) {
	if len(body) < 8 || string(body[:4]) != "NEX1" {
		return nil, nil, errors.New("not NEX1")
	}
	hl := int(binary.LittleEndian.Uint32(body[4:]))
	if hl <= 0 || 8+hl > len(body) {
		return nil, nil, errors.New("header length")
	}
	var hdr struct {
		N       int      `json:"n"`
		Res     int      `json:"res"`
		Gen     string   `json:"gen"`
		Epoch   string   `json:"epoch"`
		Built   string   `json:"built_at"`
		IDs     string   `json:"ids"`
		Bands   []string `json:"tree_hh_bands"`
		Columns []struct {
			Name  string `json:"name"`
			Dtype string `json:"dtype"`
			Bytes int    `json:"bytes"`
		} `json:"columns"`
	}
	if err := json.Unmarshal(body[8:8+hl], &hdr); err != nil {
		return nil, nil, err
	}
	if hdr.Res != 9 || hdr.N <= 0 || len(hdr.Bands) != neTreeBands || hdr.Bands[0] != "3-5" || hdr.Bands[neTreeBands-1] != "55+" {
		return nil, nil, fmt.Errorf("unexpected table: res %d, n %d, bands %v", hdr.Res, hdr.N, hdr.Bands)
	}
	var ids, top, hh []byte
	o := 8 + hl
	for _, c := range hdr.Columns {
		if c.Bytes < 0 || o+c.Bytes > len(body) {
			return nil, nil, errors.New("column beyond body")
		}
		seg := body[o : o+c.Bytes]
		o += c.Bytes
		switch {
		case c.Name == "id" && c.Dtype == "u64" && c.Bytes == 8*hdr.N:
			ids = seg
		case c.Name == "canopy_h_top" && c.Dtype == "u8" && c.Bytes == hdr.N:
			top = seg
		case c.Name == "tree_hh" && c.Dtype == "u16" && c.Bytes == 2*neTreeBands*hdr.N:
			hh = seg
		}
	}
	if ids == nil || top == nil || hh == nil {
		return nil, nil, errors.New("id / canopy_h_top / tree_hh missing or of another type")
	}
	rows := make(map[uint64]*[neTreeBands + 1]uint32, hdr.N)
	out := &neNatTreesHdr{Gen: hdr.Gen, Epoch: hdr.Epoch, Built: hdr.Built}
	var id uint64
	delta := hdr.IDs != "u64"
	for k := 0; k < hdr.N; k++ {
		v := binary.LittleEndian.Uint64(ids[8*k:])
		if delta {
			id += v
		} else {
			id = v
		}
		var r [neTreeBands + 1]uint32
		var n uint32
		for b := 0; b < neTreeBands; b++ {
			r[b] = uint32(binary.LittleEndian.Uint16(hh[2*(k*neTreeBands+b):]))
			n += r[b]
		}
		if n == 0 {
			continue
		}
		if top[k] != 255 {
			r[neTreeBands] = uint32(top[k])
		}
		rows[id] = &r
		out.Trees += int64(n)
	}
	return rows, out, nil
}

// neNatTreesWrite — fold res 8 and 7 and write the file (tmp + rename).
func neNatTreesWrite(path string, r9 map[uint64]*[neTreeBands + 1]uint32, hdr *neNatTreesHdr) error {
	r8 := neNatTreesFold(r9, 8)
	levels := []map[uint64]*[neTreeBands + 1]uint32{r9, r8, neNatTreesFold(r8, 7)}
	for lv, m := range levels {
		hdr.N[lv] = len(m)
	}
	hj, _ := json.Marshal(hdr)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "nat_trees-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	head := make([]byte, (8+len(hj)+7)&^7)
	copy(head, neNatTreesMagic)
	binary.LittleEndian.PutUint32(head[4:], uint32(len(hj)))
	copy(head[8:], hj)
	if _, err := f.Write(head); err != nil {
		f.Close()
		return err
	}
	for _, m := range levels {
		ids := make([]uint64, 0, len(m))
		for id := range m {
			ids = append(ids, id)
		}
		sortU64(ids)
		buf := make([]byte, 8*len(ids)+neNatTreesRow*len(ids))
		rows := buf[8*len(ids):]
		for q, id := range ids {
			binary.LittleEndian.PutUint64(buf[8*q:], id)
			for b, v := range m[id] {
				binary.LittleEndian.PutUint32(rows[q*neNatTreesRow+4*b:], v)
			}
		}
		if _, err := f.Write(buf); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// neNatTreesSwap — a new table: tiles are keyed on it; the old mapping goes once its readers are done.
func neNatTreesSwap(t *neNatTrees) {
	old := neNatTreesState.cur.Swap(t)
	neNatTreesState.mu.Lock()
	neNatTreesState.loaded = time.Now()
	neNatTreesState.mu.Unlock()
	neMVTLRU.dropPrefix("nat2/")
	if old != nil {
		go func(b []byte) { time.Sleep(2 * time.Minute); syscall.Munmap(b) }(old.data)
	}
}

func neNatTreesHTTP(ctx context.Context, query string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", neNatTreesBase()+query, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("peer HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("answer too large")
	}
	return b, nil
}

// neNatTreesRefresh — probe the peer's generation; fetch and fold when it moved.
func neNatTreesRefresh() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	meta, err := neNatTreesHTTP(ctx, "?res=9&format=json", 1<<20)
	if err != nil {
		return err
	}
	var m struct {
		Gen string `json:"gen"`
	}
	if json.Unmarshal(meta, &m) != nil || m.Gen == "" {
		return errors.New("meta without gen")
	}
	if cur := neNatTreesGet(); cur != nil && cur.Gen == m.Gen {
		return nil
	}
	t0 := time.Now()
	body, err := neNatTreesHTTP(ctx, "?res=9&fields=tree_n,canopy_h_top,tree_hh", neNatTreesMax)
	if err != nil {
		return err
	}
	rows, hdr, err := neNatTreesParse(body)
	if err != nil {
		return err
	}
	if len(rows) < 1000 && neNatTreesGet() != nil { // a table that lost its trees is the peer's accident, not news
		return fmt.Errorf("only %d hexagons with trees", len(rows))
	}
	if err := neNatTreesWrite(neNatTreesPath(), rows, hdr); err != nil {
		return err
	}
	t, err := neNatTreesOpen(neNatTreesPath())
	if err != nil {
		return err
	}
	neNatTreesSwap(t)
	neNatTreesState.mu.Lock()
	neNatTreesState.fetches++
	neNatTreesState.mu.Unlock()
	log.Printf("🌲 national tree table: gen %s, %d trees in %d res-9 · %d res-8 · %d res-7 hexagons, %.1f s", t.Gen, t.Trees, t.N[0], t.N[1], t.N[2], time.Since(t0).Seconds())
	return nil
}

// neNatTreesLoop — background: the file at hand first, then the peer.
func neNatTreesLoop() {
	if t, err := neNatTreesOpen(neNatTreesPath()); err == nil {
		neNatTreesSwap(t)
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Printf("national tree table: %v (fetching again)", err)
	}
	if peers().ObsAPI == "" {
		return
	}
	time.Sleep(20 * time.Second)
	for {
		err := neNatTreesRefresh()
		neNatTreesState.mu.Lock()
		neNatTreesState.checked, neNatTreesState.err = time.Now(), ""
		if err != nil {
			neNatTreesState.err = err.Error()
		}
		neNatTreesState.mu.Unlock()
		if err != nil {
			log.Printf("national tree table: %v", err)
			time.Sleep(15 * time.Minute)
			continue
		}
		time.Sleep(neNatTreesEvery)
	}
}

// NENatTreesStats — debug/inflight line.
func NENatTreesStats() map[string]interface{} {
	s := &neNatTreesState
	o := map[string]interface{}{"bound": "1 file, mapped read-only (no heap)"}
	if t := s.cur.Load(); t != nil {
		o["gen"], o["epoch"], o["built_at"], o["trees"], o["bytes_mmap"] = t.Gen, t.Epoch, t.Built, t.Trees, len(t.data)
		o["res9"], o["res8"], o["res7"] = t.N[0], t.N[1], t.N[2]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o["fetches"] = s.fetches
	if !s.checked.IsZero() {
		o["checked_s"] = int(time.Since(s.checked).Seconds())
	}
	if s.err != "" {
		o["err"] = s.err
	}
	return o
}
