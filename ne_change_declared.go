// ne_change_declared.go — the declared layer as a view on the chunk log
// (docs/ne-change.md §4b).
//
// Every peer report that carried chunk rows (srtm's one-off pass, {MAP_NAME}'s
// monthly viewport passes) left the peer's bevdirect LU rows in
// {kg}.chunks.zst — the same 30-byte layout the `lu`/`bev` sections hold,
// register bytes zero. For a KG whose `bev` section is missing or older than
// the log, those rows are the freshest declared picture we have, so the
// glyph (neReq.bevFirst) reads them first: log → bev → lu per cell.
//
// Per res-10 chunk the view picks the latest CONFIRMED version (sighted
// twice, docs/ne-change.md §5) and, when no version is confirmed yet, the
// baseline (first version). Candidates never show. A version older than the
// KG's `bev` build is skipped (bev is newer there). Lossy builds never enter
// the log, so nothing lossy can reach the view.
//
// A logged res-10 chunk is authoritative for its cell set (2026-10-27 rev. 2):
// inside a chunk the log holds (selected version, not older than bev) a cell
// absent from the log rows is NOT taken from this KG's bev/lu — BEV removed
// it (boundary moved, the cell now belongs to a neighbour, the domain
// shrank) and a report can never say "gone". The cell is served if any
// other KG's view holds it (the normal multi-KG merge by kg_share); only
// when no KG's view holds it does the old bev/lu row come back, flagged
// `superseded` (neFlagSuperseded, set at serve time only, never written) so
// the map never shows a hole where a cell used to be. Outside logged chunks
// nothing changes. The whole-KG view (neDeclView — /{kg}/declared, bare
// NEO1 validation, manifest) excludes such cells.
//
// Disk: nothing new is written — no .nec, no .nex, no derived file. RAM: the
// assembled rows of a res-9 chunk live in neStore.chunkLRU (64 MB, bounded)
// under "kg/chg/c9/state"; the per-KG state digest (which versions the view
// selects) lives in neDeclStateMemo, ≤ one small entry per KG, validated by
// the log's size (append-only). Both have size lines on /debug/inflight.
// Zero SQL. Public-path file: word ban applies.
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

// neDeclState — per KG: which log versions the declared view selects.
type neDeclState struct {
	logBytes int64
	state    string         // sha256[:16] over the selected (chunk, digest_ap) pairs; "" = the log selects nothing
	sel      map[uint64]int // chunk → selected entry index
	by9      map[uint64][]uint64
	newest   string // At of the newest selected version
	logCells int    // rows of the selected versions
}

var neDeclStateMemo = struct {
	sync.Mutex
	m      map[string]*neDeclState
	warmed bool
}{m: map[string]*neDeclState{}}

// neDeclStateOf — the view state of a KG (nil when there is no log or it
// selects nothing). Cheap when nothing was appended: one stat + a map hit.
func neDeclStateOf(kg string) *neDeclState {
	st, err := os.Stat(neChangeLogPath(kg))
	if err != nil {
		return nil
	}
	neDeclStateMemo.Lock()
	if d, ok := neDeclStateMemo.m[kg]; ok && d.logBytes == st.Size() {
		neDeclStateMemo.Unlock()
		if d.state == "" {
			return nil
		}
		return d
	}
	neDeclStateMemo.Unlock()
	c := neChangeGet(kg)
	if c == nil {
		return nil
	}
	d := c.declState()
	neDeclStateMemo.Lock()
	neDeclStateMemo.m[kg] = d
	neDeclStateMemo.Unlock()
	if d.state != "" {
		neStore.setLogChunks(kg, d.by9) // chunks only the log holds → chunkHits (Dir = -1)
	} else {
		neStore.setLogChunks(kg, nil)
	}
	if d.state == "" {
		return nil
	}
	return d
}

// neDeclStatePeek — the memoised state of a KG without loading its log
// (manifest rows); nil when not memoised yet or stale. `logCells` = rows of
// the selected versions (disjoint res-10 chunks → cells the log supplies).
func neDeclStatePeek(kg string) (d *neDeclState, fresh bool) {
	st, err := os.Stat(neChangeLogPath(kg))
	if err != nil {
		return nil, true
	}
	neDeclStateMemo.Lock()
	defer neDeclStateMemo.Unlock()
	if d, ok := neDeclStateMemo.m[kg]; ok {
		return d, d.logBytes == st.Size()
	}
	return nil, false
}

// neDeclWarm — memoise the view state of every KG with a chunk log (startup,
// after the store is up): one log read per KG, throttled; afterwards every
// state lookup is a stat + map hit until the log grows.
func neDeclWarm() {
	n := 0
	for _, kg := range neChangeKGs() {
		if _, fresh := neDeclStatePeek(kg); !fresh {
			neDeclStateOf(kg)
			n++
			if n%50 == 0 {
				time.Sleep(200 * time.Millisecond)
			}
		}
	}
	neDeclStateMemo.Lock()
	neDeclStateMemo.warmed = true
	neDeclStateMemo.Unlock()
	log.Printf("🧩 NE declared view: %d KG states memoised", n)
	neManifest.mu.Lock()
	neManifest.logBytes = -1 // force the next scan to rebuild: the `declared` blocks were "warming"
	neManifest.mu.Unlock()
	go neManifest.refresh(true)
}

// NEDeclStateStats — size line for /debug/inflight.
func NEDeclStateStats() map[string]interface{} {
	neDeclStateMemo.Lock()
	defer neDeclStateMemo.Unlock()
	n := 0
	for _, d := range neDeclStateMemo.m {
		if d.state != "" {
			n++
		}
	}
	return map[string]interface{}{"kgs": len(neDeclStateMemo.m), "with_view": n, "max_kgs": kgUniverseContract, "warmed": neDeclStateMemo.warmed}
}

// declState — select per chunk the latest confirmed version, else the baseline.
func (c *neChangeKG) declState() *neDeclState {
	d := &neDeclState{logBytes: c.logBytes, sel: map[uint64]int{}, by9: map[uint64][]uint64{}}
	chunks := make([]uint64, 0, len(c.versions))
	for ch := range c.versions {
		chunks = append(chunks, ch)
	}
	sortU64(chunks)
	h := sha256.New()
	for _, ch := range chunks {
		vs := c.versions[ch]
		pick := vs[0]
		for i := len(vs) - 1; i > 0; i-- {
			if c.seen[c.entries[vs[i]].AP] >= 2 {
				pick = vs[i]
				break
			}
		}
		e := c.entries[pick]
		if e.Rows <= 0 || e.FrameLen <= 0 {
			continue
		}
		d.sel[ch] = pick
		d.logCells += e.Rows
		c9 := h3Parent(ch, neChunkRes)
		d.by9[c9] = append(d.by9[c9], ch)
		if e.At > d.newest {
			d.newest = e.At
		}
		h.Write([]byte(e.Chunk + ":" + e.AP + ";"))
	}
	if len(d.sel) > 0 {
		d.state = hex.EncodeToString(h.Sum(nil))[:16]
	}
	return d
}

// neDeclaredLogRows — the view's rows for (kg, res-9 chunk), cell-id sorted,
// 30 B each; nil when the log holds nothing for the chunk or every selected
// version is older than `after` (the KG's bev build, "" = no bev). Cached in
// neStore.chunkLRU.
func neDeclaredLogRows(kg string, c9 uint64, after string) []byte {
	d := neDeclStateOf(kg)
	if d == nil {
		return nil
	}
	chs := d.by9[c9]
	if len(chs) == 0 {
		return nil
	}
	key := kg + "/chg/" + neCellHex(c9) + "/" + d.state + "/" + after
	if b, ok := neStore.chunkLRU.get(key); ok {
		if len(b) == 0 {
			return nil
		}
		return b
	}
	c := neChangeGet(kg)
	if c == nil {
		return nil
	}
	var rows []byte
	var f *os.File
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	for _, ch := range chs {
		i, ok := d.sel[ch]
		if !ok || i >= len(c.entries) {
			continue
		}
		v := c.entries[i]
		if after != "" && v.At < after {
			continue
		}
		if f == nil {
			var err error
			if f, err = os.Open(neChangeLogPath(kg)); err != nil {
				return nil
			}
		}
		fr := make([]byte, v.FrameLen)
		if _, err := f.ReadAt(fr, v.off); err != nil {
			return nil
		}
		r, err := neStore.decompress(fr, v.Rows*neLURowBytes)
		if err != nil || len(r) != v.Rows*neLURowBytes || neAPDigest(r) != v.AP {
			return nil
		}
		rows = append(rows, r...)
	}
	if !neRowsSorted(rows, neLURowBytes) {
		rows = neSortRows(rows, neLURowBytes)
	}
	neStore.chunkLRU.put(key, rows) // an empty value caches "nothing newer than bev"
	if len(rows) == 0 {
		return nil
	}
	return rows
}

// neFlagSuperseded — bit 7 of the LU row's flags byte, set at serve time on
// a bev/lu row whose res-10 chunk a peer's logged build no longer contains
// and that no other KG's view holds (public json `flags.superseded`).
const neFlagSuperseded = 0x80

// neDeclaredLoggedChunks — the res-10 chunks of (kg, res-9 chunk) whose
// selected log version is in the view (not older than `after`): inside them
// the log's cell set is authoritative. nil when the log supplies no rows
// for the chunk (neDeclaredLogRows == nil), so a failed read never hides.
func neDeclaredLoggedChunks(kg string, c9 uint64, after string) []uint64 {
	d := neDeclStateOf(kg)
	if d == nil {
		return nil
	}
	chs := d.by9[c9]
	if len(chs) == 0 {
		return nil
	}
	c := neChangeGet(kg)
	if c == nil {
		return nil
	}
	var out []uint64
	for _, ch := range chs {
		i, ok := d.sel[ch]
		if !ok || i >= len(c.entries) {
			continue
		}
		if after != "" && c.entries[i].At < after {
			continue
		}
		out = append(out, ch)
	}
	return out
}

// neLoggedHas — is the cell's res-10 chunk in the logged list (≤ 7 entries).
func neLoggedHas(logged []uint64, id uint64) bool {
	if len(logged) == 0 {
		return false
	}
	c10 := h3Parent(id, neKRes)
	for _, ch := range logged {
		if ch == c10 {
			return true
		}
	}
	return false
}

// neRowSuperseded — a copy of a bev/lu row with the superseded flag set.
func neRowSuperseded(row []byte) []byte {
	r := append([]byte(nil), row...)
	r[11] |= neFlagSuperseded
	return r
}

// neDeclaredAfter — the bev build time a log version must reach to win over
// the KG's bev row ("" when the KG has no bev section).
func neDeclaredAfter(k *neKG) string {
	if l := k.Hdr.Layers["bev"]; l != nil {
		return l.BuiltAt
	}
	return ""
}

// neDeclaredLogState — the view's state digest for ETags ("" = no view).
func neDeclaredLogState(kg string) string {
	if d := neDeclStateOf(kg); d != nil {
		return d.state
	}
	return ""
}

// neDeclaredCellHits — the declared view's hits of one res-12 cell across
// the KGs holding its res-9 chunk (neCellDoc path): per KG log row (register
// bytes from the KG's stored bev/lu row) → bev → lu, except inside a logged
// res-10 chunk where only the log row counts. When no KG's view holds the
// cell, the suppressed bev/lu rows come back flagged superseded.
func neDeclaredCellHits(id uint64, hits []neChunkHit) []neCellHit {
	var out, sup []neCellHit
	c9 := h3Parent(id, neChunkRes)
	for _, h := range hits {
		after := neDeclaredAfter(h.KG)
		rows := neDeclaredLogRows(h.KG.KG, c9, after)
		row := neFindRow(rows, neLURowBytes, id)
		var old []byte
		for _, layer := range []string{"bev", "lu"} {
			if l := h.KG.Hdr.Layers[layer]; l != nil {
				if r, _ := neStore.chunkRows(h.KG, h.Dir, layer); r != nil {
					if old = neFindRow(r, l.RowBytes, id); old != nil {
						break
					}
				}
			}
		}
		switch {
		case row != nil:
			out = append(out, neCellHit{KG: h.KG.KG, Row: neRowWithRegister(row, old), Share: row[8]})
		case old == nil:
		case rows != nil && neLoggedHas(neDeclaredLoggedChunks(h.KG.KG, c9, after), id):
			sup = append(sup, neCellHit{KG: h.KG.KG, Row: neRowSuperseded(old), Share: old[8]})
		default:
			out = append(out, neCellHit{KG: h.KG.KG, Row: old, Share: old[8]})
		}
	}
	if len(out) == 0 {
		return sup
	}
	return out
}

// neRowWithRegister — a copy of a log row with the two register bytes of the
// stored row `old` (nil → the row as is, zeros).
func neRowWithRegister(row, old []byte) []byte {
	if old == nil || len(old) < neLURowBytes || (old[neRegOff0] == 0 && old[neRegOff1] == 0) {
		return row
	}
	r := append([]byte(nil), row...)
	r[neRegOff0], r[neRegOff1] = old[neRegOff0], old[neRegOff1]
	return r
}

// ── the whole-KG declared view (obs validation, /{kg}/declared, layers=declared) ──

// neDeclView — the assembled declared cell set of one KG: log view → bev → lu
// per cell (exactly what the glyph and layers=declared read), register bytes
// zero, cell-id sorted, 30 B rows. Identity = neDeclaredDigest(k, true).
type neDeclView struct {
	KG       string
	Rows     []byte // sorted, register bytes zero
	Digest   string // neDigest(Rows)
	Source   string // "chunks@<state>" when the log won ≥ 1 row, else bev/lu source
	Epoch    string
	State    string // log view state ("" = no log rows in the view)
	NewestAt string // newest selected log version (RFC 3339), "" = none
	ChunksN  int    // res-10 chunks whose rows come from the log
	FromLog  int
	FromBev  int
	FromLU   int
	Hidden   int    // bev/lu cells inside logged chunks the log no longer holds (not in the set)
	Identity string // neDeclaredDigest(k, true) — ETag source
}

func (v *neDeclView) CellsN() int { return len(v.Rows) / neLURowBytes }

// neDeclViewOf — the view of a KG, nil when the KG holds neither lu, bev nor
// a chunk log that selects anything. Cached in neStore.chunkLRU (bounded,
// `ne_store` line on /debug/inflight) under kg/view/<identity>.
func neDeclViewOf(kg string) *neDeclView {
	k := neStore.getOrConvert(kg)
	if k == nil {
		return nil
	}
	id := neDeclaredDigest(k, true)
	key := kg + "/view/" + id
	if b, ok := neStore.chunkLRU.get(key); ok && len(b) > 0 {
		v := neDeclViewDecode(kg, b)
		if v != nil {
			return v
		}
	}
	v := neDeclViewBuild(k)
	if v == nil {
		return nil
	}
	neStore.chunkLRU.put(key, v.encode())
	return v
}

// encode/decode — the LRU stores one byte blob: a small JSON meta line + rows.
func (v *neDeclView) encode() []byte {
	meta, _ := json.Marshal(map[string]interface{}{"d": v.Digest, "s": v.Source, "e": v.Epoch, "st": v.State, "n": v.NewestAt,
		"c": v.ChunksN, "fl": v.FromLog, "fb": v.FromBev, "fu": v.FromLU, "h": v.Hidden, "id": v.Identity})
	out := make([]byte, 0, 4+len(meta)+len(v.Rows))
	out = binary.LittleEndian.AppendUint32(out, uint32(len(meta)))
	out = append(out, meta...)
	return append(out, v.Rows...)
}

func neDeclViewDecode(kg string, b []byte) *neDeclView {
	if len(b) < 4 {
		return nil
	}
	n := int(binary.LittleEndian.Uint32(b))
	if 4+n > len(b) {
		return nil
	}
	var m struct {
		D, S, E, St, N, ID string
		C, FL, FB, FU, H   int
	}
	if json.Unmarshal(b[4:4+n], &m) != nil {
		return nil
	}
	return &neDeclView{KG: kg, Rows: b[4+n:], Digest: m.D, Source: m.S, Epoch: m.E, State: m.St, NewestAt: m.N,
		ChunksN: m.C, FromLog: m.FL, FromBev: m.FB, FromLU: m.FU, Hidden: m.H, Identity: m.ID}
}

// neDeclViewBuild — assemble per res-9 chunk (the KG's dir ∪ the log's chunks).
func neDeclViewBuild(k *neKG) *neDeclView {
	kg := k.KG
	after := neDeclaredAfter(k)
	d := neDeclStateOf(kg)
	c9s := make(map[uint64]int, len(k.Hdr.Dir)+8)
	for i := range k.Hdr.Dir {
		c9s[k.Hdr.Dir[i].c9] = i
	}
	if d != nil {
		for c9 := range d.by9 {
			if _, ok := c9s[c9]; !ok {
				c9s[c9] = -1
			}
		}
	}
	order := make([]uint64, 0, len(c9s))
	for c9 := range c9s {
		order = append(order, c9)
	}
	sortU64(order)
	v := &neDeclView{KG: kg, Identity: neDeclaredDigest(k, true)}
	var rows []byte
	logChunks := map[uint64]bool{}
	luL, bevL := k.Hdr.Layers["lu"], k.Hdr.Layers["bev"]
	for _, c9 := range order {
		di := c9s[c9]
		var lu, bev []byte
		if di >= 0 {
			if luL != nil {
				lu, _ = neStore.chunkRows(k, di, "lu")
			}
			if bevL != nil {
				bev, _ = neStore.chunkRows(k, di, "bev")
			}
		}
		chg := neDeclaredLogRows(kg, c9, after)
		var logged []uint64
		if chg != nil {
			logged = neDeclaredLoggedChunks(kg, c9, after)
		}
		seen := make(map[uint64]bool, len(chg)/neLURowBytes+len(bev)/neLURowBytes)
		add := func(src []byte, rb int, from *int, fromLog bool) {
			for i := 0; i+rb <= len(src); i += rb {
				id := neRowID(src, i/rb, rb)
				if seen[id] {
					continue
				}
				if !fromLog && neLoggedHas(logged, id) { // the logged chunk's cell set is authoritative
					seen[id] = true
					v.Hidden++
					continue
				}
				seen[id] = true
				row := append([]byte(nil), src[i:i+neLURowBytes]...)
				row[neRegOff0], row[neRegOff1] = 0, 0
				rows = append(rows, row...)
				*from++
				if fromLog {
					logChunks[h3Parent(id, neKRes)] = true
				}
			}
		}
		add(chg, neLURowBytes, &v.FromLog, true)
		if bevL != nil {
			add(bev, bevL.RowBytes, &v.FromBev, false)
		}
		if luL != nil {
			add(lu, luL.RowBytes, &v.FromLU, false)
		}
	}
	if len(rows) == 0 {
		return nil
	}
	if !neRowsSorted(rows, neLURowBytes) {
		rows = neSortRows(rows, neLURowBytes)
	}
	v.Rows = rows
	v.Digest = neDigest(rows)
	v.ChunksN = len(logChunks)
	switch {
	case v.FromLog > 0 && d != nil:
		v.State, v.NewestAt = d.state, d.newest
		v.Source = "chunks@" + d.state
		v.Epoch = d.newest
		if len(v.Epoch) > 7 {
			v.Epoch = v.Epoch[:7]
		}
	case v.FromBev > 0:
		h := k.layerHeader("bev")
		v.Source, _ = h["source"].(string)
		v.Epoch, _ = h["epoch"].(string)
	default:
		h := k.layerHeader("lu")
		v.Source, _ = h["source"].(string)
		v.Epoch, _ = h["epoch"].(string)
	}
	return v
}

// cellSet — the view's cell ids.
func (v *neDeclView) cellSet() map[uint64]bool {
	n := v.CellsN()
	m := make(map[uint64]bool, n)
	for i := 0; i < n; i++ {
		m[neRowID(v.Rows, i, neLURowBytes)] = true
	}
	return m
}

// header — the NEC1-shaped header of the view (what /{kg}/declared and
// layers=declared on /{kg} serve). Shape follows the stored lu header so
// peers' NEC1 readers parse it unchanged; `view{}` says where rows came from.
func (v *neDeclView) header(k *neKG, public bool) map[string]interface{} {
	base := k.layerHeader("lu")
	if base == nil {
		base = k.layerHeader("bev")
	}
	h := make(map[string]interface{}, len(base)+8)
	for key, val := range base {
		switch key {
		case "stats", "inputs", "input_rule", "coverage", "kcells_n", "fields_k", "record_k", "k", "k_res":
			continue
		}
		h[key] = val
	}
	h["magic"], h["fmt_ver"], h["kg"] = "NEC1", 1, v.KG
	h["cells_n"], h["kcells_n"] = v.CellsN(), 0
	h["digest"], h["source"], h["epoch"] = v.Digest, v.Source, v.Epoch
	h["register_bytes"] = "zero"
	h["view"] = map[string]interface{}{"state": v.State, "newest_at": v.NewestAt, "chunks_n": v.ChunksN,
		"from_log": v.FromLog, "from_bev": v.FromBev, "from_lu": v.FromLU, "hidden": v.Hidden,
		"rule": "per cell: chunk-log view (latest confirmed version, else baseline) → bev → lu; inside a logged res-10 chunk the log's cell set is authoritative (bev/lu cells it lacks are not in the set)"}
	h["attribution"], h["notice"] = NENotice(), NENotice()
	if public {
		var m map[string]interface{}
		_ = json.Unmarshal(neRewriteHeaderPublic(h), &m)
		return m
	}
	return h
}

// section — the view as one NEC1 section (zlib body), for /{kg}/declared.
func (v *neDeclView) section(k *neKG, public bool) []byte {
	hj, _ := json.Marshal(v.header(k, public))
	body := zlibDeflateServe(v.Rows)
	out := make([]byte, 0, 9+len(hj)+4+len(body))
	out = append(out, "NEC1"...)
	out = append(out, 1)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(hj)))
	out = append(out, hj...)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(body)))
	return append(out, body...)
}

// etag — carries the view identity (lu+bev digests + log state).
func (v *neDeclView) etag(public bool) string {
	s := sha256.Sum256([]byte(v.Identity + "|" + v.Digest))
	t := `"nev-` + hex.EncodeToString(s[:])[:16]
	if public {
		t += "-p"
	}
	return t + `"`
}

// handleNEDeclaredGet — GET /contrib/api/v1/ne/{kg}/declared (contributor or
// private token; never public): the view as one NEC1 section (default) or
// ?format=json {header, cells_n, …} without rows. Peers build their bare NEO1
// on this cell set (api_ne.go handleNEObsPost).
func handleNEDeclaredGet(w http.ResponseWriter, r *http.Request, kg string) {
	if _, ok := neReportGate(w, r); !ok {
		return
	}
	v := neDeclViewOf(kg)
	k := neStore.get(kg)
	if v == nil || k == nil {
		writeJSON(w, 404, map[string]interface{}{"error": "no declared rows for this KG yet (no lu, no bev, no chunk log) — post NEC1+NEO1 or a report with chunk rows", "kg": kg})
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=300")
	if neNotModified(w, r, v.etag(false)) {
		return
	}
	w.Header().Set("X-NE-Declared-Source", v.Source)
	w.Header().Set("X-NE-Declared-Digest", v.Digest)
	if r.URL.Query().Get("format") == "json" {
		writeJSON(w, 200, map[string]interface{}{"kg": kg, "header": v.header(k, false), "cells_n": v.CellsN(), "source": v.Source, "digest": v.Digest,
			"state": v.State, "newest_at": v.NewestAt, "chunks_n": v.ChunksN, "from": map[string]int{"log": v.FromLog, "bev": v.FromBev, "lu": v.FromLU}, "hidden": v.Hidden,
			"nec1": tierPrefixFor(r) + "/api/v1/ne/" + kg + "/declared"})
		return
	}
	w.Header().Set("Content-Type", "application/vnd.ne-cells")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="%s.declared.nec"`, kg))
	http.ServeContent(w, r, "", k.Mod, bytesReader(v.section(k, false)))
}
