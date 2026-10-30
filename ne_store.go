// ne_store.go — the NE on-disk store (docs/ne-public-api.md §1).
//
//	data/ne_cells/{kg}.nex          NEX1 | hdr_len u32le | hdr JSON | data area
//	data/ne_cells/dict/{id}.zd      trained zstd dictionaries (committed)
//	data/ne_cells/ne_index.db       header cache so startup is one sequential read
//
// Inputs are the python build output `{kg}.nec`, the operator baseline
// `{kg}.bev.nec` and the posted `{kg}.obs`. The converter verifies each
// section digest, writes the .nex atomically, re-reads it, recomputes every
// digest from the chunks and only then deletes the input (the freeze rule
// "never overwrite" is kept by ne_all.sh testing `.nec || .nex`).
//
// Data area = per res-9 chunk one zstd frame per layer (lu / bev / obs rows of
// that chunk, cell-id sorted) + one zlib stream per NEC1 layer holding the K
// rows (kept inflated in RAM: ~29 B × 0.8 M country-wide). Chunk frames are
// served as stored (`format=zstd`), so compression happens once, at convert
// time. All offsets are relative to the data area start.
//
// Memory (AGENTS.md invariant 5): RAM index (sorted slice, 16 B/chunk ref),
// K rows, inflated-chunk LRU 64 MB, whole-container LRU 48 MB — every one
// has a size line under `ne_store` on /api/v1/debug/inflight.
// Read path: zero SQL on search_index.db, zero warming, zero disk writes.
package main

import (
	"bytes"
	"compress/zlib"

	"container/list"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	kzlib "github.com/klauspost/compress/zlib"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

const (
	nexMagic      = "NEX1"
	neLURowBytes  = 30
	neKRowBytes   = 29
	neChunkLRUMax = 64 << 20
	neWholeLRUMax = 48 << 20
)

var neLayerOrder = []string{"lu", "bev", "obs"}

// nexLayer — one stored section (NEC1 lu / NEC1 bev / NEO1 obs).
type nexLayer struct {
	Magic     string          `json:"magic"`
	Header    json.RawMessage `json:"header"` // verbatim section header bytes (private tier emits them as-is)
	Digest    string          `json:"digest"`
	Bytes     int64           `json:"bytes"`    // original section size
	BuiltAt   string          `json:"built_at"` // input file mtime, RFC 3339
	Rows      int             `json:"rows"`
	RowBytes  int             `json:"row_bytes"`
	Dict      string          `json:"dict"` // dictionary id of the chunk frames ("" = none)
	KOff      int64           `json:"k_off,omitempty"`
	KLen      int64           `json:"k_len,omitempty"`
	KRows     int             `json:"k_rows,omitempty"`
	Sorted    bool            `json:"sorted"` // rows were cell-id sorted → digest reproducible from chunks
	RawOff    int64           `json:"raw_off,omitempty"`
	RawLen    int64           `json:"raw_len,omitempty"` // !Sorted: verbatim zlib body
	srcHeader map[string]interface{}
}

// nexDirEntry — one res-9 chunk: per layer [off, len, rows].
type nexDirEntry struct {
	C9  string   `json:"c9"`
	LU  [3]int64 `json:"lu,omitempty"`
	Bev [3]int64 `json:"bev,omitempty"`
	Obs [3]int64 `json:"obs,omitempty"`
	c9  uint64
}

func (d *nexDirEntry) layer(name string) [3]int64 {
	switch name {
	case "lu":
		return d.LU
	case "bev":
		return d.Bev
	case "obs":
		return d.Obs
	}
	return [3]int64{}
}

type nexHeader struct {
	V        int                  `json:"v"`
	KG       string               `json:"kg"`
	ChunkRes int                  `json:"chunk_res"`
	Layers   map[string]*nexLayer `json:"layers"`
	Dir      []nexDirEntry        `json:"dir"`
	DataLen  int64                `json:"data_len"`
}

// neKG — one KG in RAM.
type neKG struct {
	KG      string
	Path    string
	Size    int64
	Mod     time.Time
	DataOff int64
	Hdr     nexHeader
	KRows   map[string][]byte // layer → inflated K rows (cell-id sorted)
}

func (k *neKG) layerHeader(name string) map[string]interface{} {
	l := k.Hdr.Layers[name]
	if l == nil {
		return nil
	}
	if l.srcHeader == nil {
		var m map[string]interface{}
		_ = json.Unmarshal(l.Header, &m)
		l.srcHeader = m
	}
	return l.srcHeader
}

// neChunkRef — RAM index entry: res-9 cell → (kg, dir index).
type neChunkRef struct {
	C9  uint64
	KG  uint32
	Dir uint32
}

type neStoreT struct {
	mu     sync.RWMutex
	kgs    []*neKG // index = neChunkRef.KG (append-only; replaced KGs reuse their slot)
	byKG   map[string]int
	refs   []neChunkRef // sorted by C9, then KG
	kBytes int64
	// logRefs — res-9 chunks a KG's chunk log supplies rows for that are in
	// NO section of its .nex (an index-epoch hole the peer's bevdirect build
	// filled, e.g. 04015 Klausenleopoldsdorf 2026-10-30): c9 → KG indices.
	// Fed by neDeclStateOf via setLogChunks; served by chunkHits as Dir = -1.
	// Bounded by the log's chunk set (≤ one entry per KG × log-only chunk).
	logRefs  map[uint64][]uint32
	logRefsN map[string]int // per KG: entries it holds (size line)

	chunkLRU *neLRU
	wholeLRU *neLRU
	flightMu sync.Mutex
	flight   map[string]*neFlight // whole-KG materialisations in progress (single-flight)

	dictMu sync.Mutex
	dicts  map[string][]byte
	enc    map[string]*zstd.Encoder
	dec    *zstd.Decoder

	convMu    sync.Mutex // one conversion at a time
	db        *sql.DB
	converted int
	failed    map[string]string
	ready     bool
}

var neStore = &neStoreT{byKG: map[string]int{}, dicts: map[string][]byte{}, enc: map[string]*zstd.Encoder{},
	chunkLRU: newNELRU(neChunkLRUMax), wholeLRU: newNELRU(neWholeLRUMax), failed: map[string]string{},
	flight: map[string]*neFlight{}}

// ── tiny byte LRU ───────────────────────────────────────────────────────────

type neLRU struct {
	mu    sync.Mutex
	max   int64
	size  int64
	ll    *list.List
	items map[string]*list.Element
	hits  int64
	miss  int64
}

type neLRUItem struct {
	key string
	val []byte
}

func newNELRU(max int64) *neLRU {
	return &neLRU{max: max, ll: list.New(), items: map[string]*list.Element{}}
}

func (c *neLRU) get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.ll.MoveToFront(e)
		c.hits++
		return e.Value.(*neLRUItem).val, true
	}
	c.miss++
	return nil, false
}

func (c *neLRU) put(key string, val []byte) {
	if int64(len(val)) > c.max/4 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.size += int64(len(val)) - int64(len(e.Value.(*neLRUItem).val))
		e.Value.(*neLRUItem).val = val
		c.ll.MoveToFront(e)
	} else {
		c.items[key] = c.ll.PushFront(&neLRUItem{key, val})
		c.size += int64(len(val))
	}
	for c.size > c.max && c.ll.Len() > 0 {
		e := c.ll.Back()
		it := e.Value.(*neLRUItem)
		c.size -= int64(len(it.val))
		delete(c.items, it.key)
		c.ll.Remove(e)
	}
}

func (c *neLRU) dropPrefix(prefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.items {
		if strings.HasPrefix(k, prefix) {
			c.size -= int64(len(e.Value.(*neLRUItem).val))
			delete(c.items, k)
			c.ll.Remove(e)
		}
	}
}

func (c *neLRU) stats() map[string]interface{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return map[string]interface{}{"entries": c.ll.Len(), "bytes": c.size, "max_bytes": c.max, "hits": c.hits, "misses": c.miss}
}

// ── dictionaries / codecs ──────────────────────────────────────────────────

func neDictDir() string { return filepath.Join(neDir, "dict") }

func neDictPath(id string) string { return filepath.Join(neDictDir(), id+".zd") }

// neDictIDs lists the dictionaries on disk.
func neDictIDs() []string {
	ents, _ := os.ReadDir(neDictDir())
	var out []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".zd") {
			out = append(out, strings.TrimSuffix(e.Name(), ".zd"))
		}
	}
	sort.Strings(out)
	return out
}

var neDictIDRe = regexp.MustCompile(`^[a-z]+-[0-9]+$`)

// neDict returns the dictionary bytes (cached), or nil.
func (s *neStoreT) dict(id string) []byte {
	if id == "" || !neDictIDRe.MatchString(id) {
		return nil
	}
	s.dictMu.Lock()
	defer s.dictMu.Unlock()
	if d, ok := s.dicts[id]; ok {
		return d
	}
	d, err := os.ReadFile(neDictPath(id))
	if err != nil {
		d = nil
	}
	s.dicts[id] = d
	return d
}

// latestDict picks the newest `{layer}-N` dictionary present ("" when none).
func (s *neStoreT) latestDict(layer string) string {
	best, bestN := "", -1
	for _, id := range neDictIDs() {
		if p, n, ok := strings.Cut(id, "-"); ok && p == layer {
			if v, err := strconv.Atoi(n); err == nil && v > bestN {
				best, bestN = id, v
			}
		}
	}
	return best
}

func (s *neStoreT) encoder(dictID string) (*zstd.Encoder, error) {
	s.dictMu.Lock()
	defer s.dictMu.Unlock()
	if e, ok := s.enc[dictID]; ok {
		return e, nil
	}
	opts := []zstd.EOption{zstd.WithEncoderLevel(zstd.SpeedBestCompression), zstd.WithEncoderConcurrency(1), zstd.WithZeroFrames(true)}
	if dictID != "" {
		d, err := os.ReadFile(neDictPath(dictID))
		if err != nil {
			return nil, err
		}
		s.dicts[dictID] = d
		opts = append(opts, zstd.WithEncoderDict(d))
	}
	e, err := zstd.NewWriter(nil, opts...)
	if err != nil {
		return nil, err
	}
	s.enc[dictID] = e
	return e, nil
}

func (s *neStoreT) decoder() *zstd.Decoder {
	s.dictMu.Lock()
	defer s.dictMu.Unlock()
	if s.dec != nil {
		return s.dec
	}
	opts := []zstd.DOption{zstd.WithDecoderConcurrency(0), zstd.WithDecoderLowmem(true)}
	for _, id := range neDictIDs() {
		if d, err := os.ReadFile(neDictPath(id)); err == nil {
			s.dicts[id] = d
			opts = append(opts, zstd.WithDecoderDicts(d))
		}
	}
	s.dec, _ = zstd.NewReader(nil, opts...)
	return s.dec
}

func (s *neStoreT) compress(dictID string, raw []byte) ([]byte, error) {
	e, err := s.encoder(dictID)
	if err != nil {
		return nil, err
	}
	s.dictMu.Lock()
	defer s.dictMu.Unlock()
	return e.EncodeAll(raw, nil), nil
}

// neMarshalHeader — NEX1 header JSON without HTML escaping, so the embedded
// section headers (RawMessage) stay byte-identical to the input.
func neMarshalHeader(h *nexHeader) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(h); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// compressChunks — one zstd frame per blob. Uses the zstd CLI (-19, trained
// dictionary, one process per KG: measured 221 KB vs klauspost 237 KB on
// 63330's 190 chunks — klauspost's encoder barely uses a dictionary) and
// falls back to klauspost when the binary is missing.
func (s *neStoreT) compressChunks(dictID string, blobs [][]byte) ([][]byte, error) {
	out := make([][]byte, len(blobs))
	if zstdBin, err := exec.LookPath("zstd"); err == nil && len(blobs) > 0 {
		tmp, err := os.MkdirTemp("", "nex")
		if err == nil {
			defer os.RemoveAll(tmp)
			args := []string{"-19", "-q", "--no-check", "--single-thread", "--rm"}
			if dictID != "" {
				args = append(args, "-D", neDictPath(dictID))
			}
			for i, b := range blobs {
				p := filepath.Join(tmp, fmt.Sprintf("%06d", i))
				if err := os.WriteFile(p, b, 0o600); err != nil {
					return nil, err
				}
				args = append(args, p)
			}
			cmd := exec.Command(zstdBin, args...)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Run(); err == nil {
				ok := true
				for i := range blobs {
					out[i], err = os.ReadFile(filepath.Join(tmp, fmt.Sprintf("%06d.zst", i)))
					if err != nil {
						ok = false
						break
					}
				}
				if ok {
					return out, nil
				}
			} else {
				log.Printf("NE store: zstd cli failed (%v: %s) — falling back to klauspost", err, strings.TrimSpace(stderr.String()))
			}
		}
	}
	for i, b := range blobs {
		f, err := s.compress(dictID, b)
		if err != nil {
			return nil, err
		}
		out[i] = f
	}
	return out, nil
}

func (s *neStoreT) decompress(frame []byte, sizeHint int) ([]byte, error) {
	return s.decoder().DecodeAll(frame, make([]byte, 0, sizeHint))
}

// zlibDeflate — store writes (once per KG): stdlib level 9, smallest bytes on disk.
func zlibDeflate(raw []byte) []byte {
	var b bytes.Buffer
	w, _ := zlib.NewWriterLevel(&b, zlib.BestCompression)
	_, _ = w.Write(raw)
	_ = w.Close()
	return b.Bytes()
}

// zlibDeflateServe — whole-KG answers (per LRU miss): klauspost zlib level 7.
// Measured on 63330's 1.59 MB of rows: stdlib-9 262 045 B in 707 ms,
// klauspost-7 263 047 B in 44 ms (+0.4 % bytes, 16× less CPU); stdlib-6 would
// be 263 797 B / 91 ms. Standard zlib stream — any inflater reads it.
func zlibDeflateServe(raw []byte) []byte {
	var b bytes.Buffer
	b.Grow(len(raw) / 5)
	w, _ := kzlib.NewWriterLevel(&b, 7)
	_, _ = w.Write(raw)
	_ = w.Close()
	return b.Bytes()
}

func neDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:16]
}

// ── row helpers ────────────────────────────────────────────────────────────

func neRowID(rows []byte, i, rb int) uint64 { return binary.LittleEndian.Uint64(rows[i*rb:]) }

func neRowsSorted(rows []byte, rb int) bool {
	n := len(rows) / rb
	for i := 1; i < n; i++ {
		if neRowID(rows, i, rb) <= neRowID(rows, i-1, rb) {
			return false
		}
	}
	return true
}

func neSortRows(rows []byte, rb int) []byte {
	n := len(rows) / rb
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return neRowID(rows, idx[a], rb) < neRowID(rows, idx[b], rb) })
	out := make([]byte, 0, len(rows))
	for _, i := range idx {
		out = append(out, rows[i*rb:(i+1)*rb]...)
	}
	return out
}

// neFindRow — binary search in cell-id sorted fixed rows; returns the row or nil.
func neFindRow(rows []byte, rb int, id uint64) []byte {
	if rb <= 0 || len(rows) == 0 {
		return nil
	}
	n := len(rows) / rb
	i := sort.Search(n, func(i int) bool { return neRowID(rows, i, rb) >= id })
	if i < n && neRowID(rows, i, rb) == id {
		return rows[i*rb : (i+1)*rb]
	}
	return nil
}

// ── input parsing ──────────────────────────────────────────────────────────

// neInput — a verified input section: rows + K rows + verbatim header.
type neInput struct {
	layer    nexLayer
	rows     []byte
	krows    []byte
	hdrBytes []byte
}

// neParseInputSection reads one file holding exactly one section of the
// expected magic, verifies the digest and splits NEC1 into LU + K rows.
func neParseInputSection(path, layer string) (*neInput, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fi, _ := os.Stat(path)
	secs, err := parseNESections(b)
	if err != nil {
		return nil, err
	}
	wantMagic := "NEC1"
	if layer == "obs" {
		wantMagic = "NEO1"
	}
	if len(secs) != 1 || secs[0].Magic != wantMagic {
		return nil, fmt.Errorf("%s: expected one %s section", path, wantMagic)
	}
	sec := secs[0]
	hl := int(binary.LittleEndian.Uint32(sec.Raw[5:]))
	hdrBytes := append([]byte(nil), sec.Raw[9:9+hl]...)
	raw, err := neInflate(sec.Body)
	if err != nil {
		return nil, err
	}
	in := &neInput{hdrBytes: hdrBytes}
	in.layer = nexLayer{Magic: sec.Magic, Header: hdrBytes, Bytes: int64(len(sec.Raw)), srcHeader: sec.Header}
	in.layer.Digest, _ = sec.Header["digest"].(string)
	if fi != nil {
		in.layer.BuiltAt = fi.ModTime().UTC().Format(time.RFC3339)
	}
	n, _ := sec.Header["cells_n"].(float64)
	if n < 1 {
		return nil, fmt.Errorf("%s: cells_n missing", path)
	}
	if sec.Magic == "NEC1" {
		kn, _ := sec.Header["kcells_n"].(float64)
		if int(n)*neLURowBytes+int(kn)*neKRowBytes != len(raw) {
			return nil, fmt.Errorf("%s: record block %d ≠ %d×30+%d×29", path, len(raw), int(n), int(kn))
		}
		in.rows = raw[:int(n)*neLURowBytes]
		in.krows = raw[int(n)*neLURowBytes:]
		in.layer.RowBytes, in.layer.Rows, in.layer.KRows = neLURowBytes, int(n), int(kn)
	} else {
		rb := 0
		if v, ok := sec.Header["record_bytes"].(float64); ok {
			rb = int(v)
		} else if len(raw)%int(n) == 0 {
			rb = len(raw) / int(n)
		}
		if rb < 9 || rb > 64 || len(raw) != int(n)*rb {
			return nil, fmt.Errorf("%s: row size %d unusable", path, rb)
		}
		in.rows = raw
		in.layer.RowBytes, in.layer.Rows = rb, int(n)
	}
	in.layer.Sorted = neRowsSorted(in.rows, in.layer.RowBytes) && (in.krows == nil || neRowsSorted(in.krows, neKRowBytes))
	return in, nil
}

// ── writer ─────────────────────────────────────────────────────────────────

// neWriteNex assembles and atomically writes {kg}.nex from the given layers.
func (s *neStoreT) neWriteNex(kg string, inputs map[string]*neInput) (string, error) {
	var data bytes.Buffer
	hdr := nexHeader{V: 1, KG: kg, ChunkRes: neChunkRes, Layers: map[string]*nexLayer{}}
	dirIdx := map[uint64]int{}
	dirEntry := func(c9 uint64) *nexDirEntry {
		if i, ok := dirIdx[c9]; ok {
			return &hdr.Dir[i]
		}
		hdr.Dir = append(hdr.Dir, nexDirEntry{C9: neCellHex(c9), c9: c9})
		dirIdx[c9] = len(hdr.Dir) - 1
		return &hdr.Dir[len(hdr.Dir)-1]
	}
	// chunk order must be the global cell order: group rows of each layer by res-9 parent
	type chunkRows struct {
		c9   uint64
		rows []byte
	}
	for _, name := range neLayerOrder {
		in := inputs[name]
		if in == nil {
			continue
		}
		l := in.layer
		rows := in.rows
		if !l.Sorted {
			l.RawOff = int64(data.Len())
			zb := zlibDeflate(append(append([]byte{}, in.rows...), in.krows...))
			data.Write(zb)
			l.RawLen = int64(len(zb))
			rows = neSortRows(in.rows, l.RowBytes)
		}
		dictID := ""
		if name == "obs" {
			dictID = s.latestDict("obs")
		} else {
			dictID = s.latestDict("lu")
		}
		l.Dict = dictID
		var chunks []chunkRows
		n := len(rows) / l.RowBytes
		for i := 0; i < n; i++ {
			p := h3Parent(neRowID(rows, i, l.RowBytes), neChunkRes)
			if len(chunks) == 0 || chunks[len(chunks)-1].c9 != p {
				chunks = append(chunks, chunkRows{c9: p})
			}
			chunks[len(chunks)-1].rows = append(chunks[len(chunks)-1].rows, rows[i*l.RowBytes:(i+1)*l.RowBytes]...)
		}
		blobs := make([][]byte, len(chunks))
		for i, ch := range chunks {
			blobs[i] = ch.rows
		}
		frames, err := s.compressChunks(dictID, blobs)
		if err != nil {
			return "", err
		}
		for i, ch := range chunks {
			frame := frames[i]
			ref := [3]int64{int64(data.Len()), int64(len(frame)), int64(len(ch.rows) / l.RowBytes)}
			data.Write(frame)
			d := dirEntry(ch.c9)
			switch name {
			case "lu":
				d.LU = ref
			case "bev":
				d.Bev = ref
			case "obs":
				d.Obs = ref
			}
		}
		if in.krows != nil {
			kr := in.krows
			if !neRowsSorted(kr, neKRowBytes) {
				kr = neSortRows(kr, neKRowBytes)
			}
			zb := zlibDeflate(kr)
			l.KOff, l.KLen = int64(data.Len()), int64(len(zb))
			data.Write(zb)
		}
		lc := l
		hdr.Layers[name] = &lc
	}
	if hdr.Layers["lu"] == nil && hdr.Layers["bev"] == nil {
		return "", errors.New("no declared layer")
	}
	sort.Slice(hdr.Dir, func(a, b int) bool { return hdr.Dir[a].c9 < hdr.Dir[b].c9 })
	hdr.DataLen = int64(data.Len())
	hj, err := neMarshalHeader(&hdr)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(neDir, 0o755); err != nil {
		return "", err
	}
	path := neFilePath(kg, "nex")
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	var pre [8]byte
	copy(pre[:4], nexMagic)
	binary.LittleEndian.PutUint32(pre[4:], uint32(len(hj)))
	if _, err = f.Write(pre[:]); err == nil {
		if _, err = f.Write(hj); err == nil {
			_, err = f.Write(data.Bytes())
		}
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return path, nil
}

// ── reader ─────────────────────────────────────────────────────────────────

// neReadNexHeader parses the NEX1 header of a file (no data read).
func neReadNexHeader(path string) (*neKG, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var pre [8]byte
	if _, err := io.ReadFull(f, pre[:]); err != nil || string(pre[:4]) != nexMagic {
		return nil, fmt.Errorf("%s: not a NEX1 file", path)
	}
	hl := binary.LittleEndian.Uint32(pre[4:])
	if hl > 64<<20 {
		return nil, fmt.Errorf("%s: header too large", path)
	}
	hj := make([]byte, hl)
	if _, err := io.ReadFull(f, hj); err != nil {
		return nil, err
	}
	return neKGFromHeader(path, fi.Size(), fi.ModTime(), hj)
}

func neKGFromHeader(path string, size int64, mod time.Time, hj []byte) (*neKG, error) {
	k := &neKG{Path: path, Size: size, Mod: mod, DataOff: 8 + int64(len(hj))}
	if err := json.Unmarshal(hj, &k.Hdr); err != nil {
		return nil, err
	}
	k.KG = k.Hdr.KG
	for i := range k.Hdr.Dir {
		v, err := strconv.ParseUint(k.Hdr.Dir[i].C9, 16, 64)
		if err != nil {
			return nil, err
		}
		k.Hdr.Dir[i].c9 = v
	}
	if k.DataOff+k.Hdr.DataLen != size {
		return nil, fmt.Errorf("%s: size %d ≠ header+data %d", path, size, k.DataOff+k.Hdr.DataLen)
	}
	return k, nil
}

// readAt reads [off, off+n) of the data area.
func (k *neKG) readAt(off, n int64) ([]byte, error) {
	if n <= 0 {
		return nil, nil
	}
	if off < 0 || off+n > k.Hdr.DataLen {
		return nil, fmt.Errorf("%s: read out of range", k.Path)
	}
	f, err := os.Open(k.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := make([]byte, n)
	_, err = f.ReadAt(b, k.DataOff+off)
	return b, err
}

// loadKRows inflates the K rows of every NEC1 layer into RAM.
func (k *neKG) loadKRows() error {
	k.KRows = map[string][]byte{}
	for name, l := range k.Hdr.Layers {
		if l.KLen == 0 {
			continue
		}
		zb, err := k.readAt(l.KOff, l.KLen)
		if err != nil {
			return err
		}
		raw, err := neInflate(zb)
		if err != nil {
			return err
		}
		if len(raw) != l.KRows*neKRowBytes {
			return fmt.Errorf("%s: K rows %d ≠ %d×29", k.Path, len(raw), l.KRows)
		}
		k.KRows[name] = raw
	}
	return nil
}

// chunkFrame — stored zstd bytes of (dir entry, layer); nil when the layer has no rows there.
func (s *neStoreT) chunkFrame(k *neKG, di int, layer string) ([]byte, error) {
	if di < 0 || di >= len(k.Hdr.Dir) {
		return nil, nil
	}
	ref := k.Hdr.Dir[di].layer(layer)
	if ref[1] == 0 {
		return nil, nil
	}
	return k.readAt(ref[0], ref[1])
}

// chunkRows — inflated rows of (dir entry, layer), via the chunk LRU.
func (s *neStoreT) chunkRows(k *neKG, di int, layer string) ([]byte, error) {
	if di < 0 || di >= len(k.Hdr.Dir) {
		return nil, nil
	}
	ref := k.Hdr.Dir[di].layer(layer)
	if ref[1] == 0 {
		return nil, nil
	}
	key := k.KG + "/" + layer + "/" + k.Hdr.Dir[di].C9
	if b, ok := s.chunkLRU.get(key); ok {
		return b, nil
	}
	frame, err := k.readAt(ref[0], ref[1])
	if err != nil {
		return nil, err
	}
	l := k.Hdr.Layers[layer]
	raw, err := s.decompress(frame, int(ref[2])*l.RowBytes)
	if err != nil {
		return nil, err
	}
	if len(raw) != int(ref[2])*l.RowBytes {
		return nil, fmt.Errorf("%s: chunk %s/%s rows %d ≠ %d", k.Path, layer, k.Hdr.Dir[di].C9, len(raw), ref[2]*int64(l.RowBytes))
	}
	s.chunkLRU.put(key, raw)
	return raw, nil
}

// layerRows — all rows of a layer in cell order (chunks concatenated), plus K rows.
func (s *neStoreT) layerRows(k *neKG, layer string) (rows, krows []byte, err error) {
	l := k.Hdr.Layers[layer]
	if l == nil {
		return nil, nil, os.ErrNotExist
	}
	if !l.Sorted {
		zb, err := k.readAt(l.RawOff, l.RawLen)
		if err != nil {
			return nil, nil, err
		}
		raw, err := neInflate(zb)
		if err != nil {
			return nil, nil, err
		}
		n := l.Rows * l.RowBytes
		return raw[:n], raw[n:], nil
	}
	// one sequential read of the whole data area is cheaper than N small reads
	data, err := k.readAt(0, k.Hdr.DataLen)
	if err != nil {
		return nil, nil, err
	}
	rows = make([]byte, 0, l.Rows*l.RowBytes)
	for di := range k.Hdr.Dir {
		ref := k.Hdr.Dir[di].layer(layer)
		if ref[1] == 0 {
			continue
		}
		raw, err := s.decompress(data[ref[0]:ref[0]+ref[1]], int(ref[2])*l.RowBytes)
		if err != nil {
			return nil, nil, err
		}
		rows = append(rows, raw...)
	}
	if l.KLen > 0 {
		krows, err = neInflate(data[l.KOff : l.KOff+l.KLen])
		if err != nil {
			return nil, nil, err
		}
	}
	if len(rows) != l.Rows*l.RowBytes {
		return nil, nil, fmt.Errorf("%s: %s rows %d ≠ %d", k.Path, layer, len(rows)/l.RowBytes, l.Rows)
	}
	return rows, krows, nil
}

// verify recomputes every layer digest from the stored chunks.
func (s *neStoreT) verify(k *neKG) error {
	for name, l := range k.Hdr.Layers {
		rows, krows, err := s.layerRows(k, name)
		if err != nil {
			return err
		}
		if d := neDigest(append(append([]byte{}, rows...), krows...)); d != l.Digest {
			return fmt.Errorf("%s: %s digest %s ≠ %s after re-read", k.Path, name, d, l.Digest)
		}
	}
	return nil
}

// ── index (RAM + SQLite header cache) ──────────────────────────────────────

func neIndexDBPath() string { return filepath.Join(neDir, "ne_index.db") }

func (s *neStoreT) openDB() {
	if s.db != nil {
		return
	}
	db, err := sql.Open("sqlite3", neIndexDBPath()+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS kgs(kg TEXT PRIMARY KEY, nex_bytes INTEGER, nex_mtime INTEGER,
		lu_digest TEXT, bev_digest TEXT, obs_digest TEXT, hdr BLOB) WITHOUT ROWID;
		CREATE TABLE IF NOT EXISTS chunks(c9 INTEGER, kg TEXT, n12 INTEGER, PRIMARY KEY(c9,kg)) WITHOUT ROWID;`)
	if err != nil {
		_ = db.Close()
		return
	}
	s.db = db
}

func (s *neStoreT) dbUpsert(k *neKG, hj []byte) {
	if s.db == nil {
		return
	}
	dig := func(n string) string {
		if l := k.Hdr.Layers[n]; l != nil {
			return l.Digest
		}
		return ""
	}
	tx, err := s.db.Begin()
	if err != nil {
		return
	}
	_, _ = tx.Exec(`INSERT OR REPLACE INTO kgs VALUES(?,?,?,?,?,?,?)`, k.KG, k.Size, k.Mod.UnixNano(), dig("lu"), dig("bev"), dig("obs"), hj)
	_, _ = tx.Exec(`DELETE FROM chunks WHERE kg=?`, k.KG)
	for _, d := range k.Hdr.Dir {
		_, _ = tx.Exec(`INSERT INTO chunks VALUES(?,?,?)`, int64(d.c9), k.KG, d.LU[2])
	}
	_ = tx.Commit()
}

func (s *neStoreT) dbDelete(kg string) {
	if s.db == nil {
		return
	}
	_, _ = s.db.Exec(`DELETE FROM kgs WHERE kg=?`, kg)
	_, _ = s.db.Exec(`DELETE FROM chunks WHERE kg=?`, kg)
}

// install puts/replaces a KG in the RAM index. Caller holds s.mu.
func (s *neStoreT) installLocked(k *neKG) {
	idx, ok := s.byKG[k.KG]
	if ok {
		if old := s.kgs[idx]; old != nil {
			for _, kr := range old.KRows {
				s.kBytes -= int64(len(kr))
			}
		}
		s.kgs[idx] = k
		// drop old refs of this KG
		out := s.refs[:0]
		for _, r := range s.refs {
			if int(r.KG) != idx {
				out = append(out, r)
			}
		}
		s.refs = out
	} else {
		idx = len(s.kgs)
		s.kgs = append(s.kgs, k)
		s.byKG[k.KG] = idx
	}
	for _, kr := range k.KRows {
		s.kBytes += int64(len(kr))
	}
	for di, d := range k.Hdr.Dir {
		s.refs = append(s.refs, neChunkRef{C9: d.c9, KG: uint32(idx), Dir: uint32(di)})
	}
	s.sortRefsLocked()
}

func (s *neStoreT) sortRefsLocked() {
	sort.Slice(s.refs, func(a, b int) bool {
		if s.refs[a].C9 != s.refs[b].C9 {
			return s.refs[a].C9 < s.refs[b].C9
		}
		return s.kgs[s.refs[a].KG].KG < s.kgs[s.refs[b].KG].KG
	})
}

func (s *neStoreT) removeLocked(kg string) {
	idx, ok := s.byKG[kg]
	if !ok {
		return
	}
	if old := s.kgs[idx]; old != nil {
		for _, kr := range old.KRows {
			s.kBytes -= int64(len(kr))
		}
	}
	s.kgs[idx] = &neKG{KG: kg} // tombstone keeps slot numbering
	delete(s.byKG, kg)
	out := s.refs[:0]
	for _, r := range s.refs {
		if int(r.KG) != idx {
			out = append(out, r)
		}
	}
	s.refs = out
	s.dropLogChunksLocked(kg, idx)
}

// dropLogChunksLocked — forget the KG's log-only chunk entries.
func (s *neStoreT) dropLogChunksLocked(kg string, idx int) {
	if s.logRefsN[kg] == 0 {
		return
	}
	for c9, l := range s.logRefs {
		out := l[:0]
		for _, i := range l {
			if int(i) != idx {
				out = append(out, i)
			}
		}
		if len(out) == 0 {
			delete(s.logRefs, c9)
		} else {
			s.logRefs[c9] = out
		}
	}
	delete(s.logRefsN, kg)
}

// get — the KG, or nil.
func (s *neStoreT) get(kg string) *neKG {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if i, ok := s.byKG[kg]; ok {
		return s.kgs[i]
	}
	return nil
}

// kgCodes — all KGs in the store, sorted.
func (s *neStoreT) kgCodes() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.byKG))
	for kg := range s.byKG {
		out = append(out, kg)
	}
	sort.Strings(out)
	return out
}

// neChunkHit — one KG holding a res-9 chunk. Dir = -1: the KG's .nex has
// no section for the chunk, only its chunk log does (chunkRows/chunkFrame
// answer nil there; the declared view supplies the rows).
type neChunkHit struct {
	KG  *neKG
	Dir int
}

// chunkHits — every KG that holds the res-9 cell, in KG order: the .nex
// directory first, then KGs whose chunk log alone holds it (Dir = -1).
func (s *neStoreT) chunkHits(c9 uint64) []neChunkHit {
	s.mu.RLock()
	defer s.mu.RUnlock()
	i := sort.Search(len(s.refs), func(i int) bool { return s.refs[i].C9 >= c9 })
	var out []neChunkHit
	for ; i < len(s.refs) && s.refs[i].C9 == c9; i++ {
		out = append(out, neChunkHit{KG: s.kgs[s.refs[i].KG], Dir: int(s.refs[i].Dir)})
	}
	for _, idx := range s.logRefs[c9] {
		dup := false
		for _, h := range out {
			if h.KG == s.kgs[idx] {
				dup = true
				break
			}
		}
		if !dup && int(idx) < len(s.kgs) && s.kgs[idx] != nil {
			out = append(out, neChunkHit{KG: s.kgs[idx], Dir: -1})
		}
	}
	if len(s.logRefs[c9]) > 0 {
		sort.Slice(out, func(a, b int) bool { return out[a].KG.KG < out[b].KG.KG })
	}
	return out
}

// setLogChunks — record the res-9 chunks the KG's chunk log supplies rows
// for that its .nex directory lacks (nil/empty = none). Replaces the KG's
// previous entries; a KG without a .nex is skipped (nothing to serve on).
func (s *neStoreT) setLogChunks(kg string, by9 map[uint64][]uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, ok := s.byKG[kg]
	if !ok {
		return
	}
	if s.logRefs == nil {
		s.logRefs = map[uint64][]uint32{}
		s.logRefsN = map[string]int{}
	}
	s.dropLogChunksLocked(kg, idx)
	k := s.kgs[idx]
	have := make(map[uint64]bool, len(k.Hdr.Dir))
	for i := range k.Hdr.Dir {
		have[k.Hdr.Dir[i].c9] = true
	}
	n := 0
	for c9 := range by9 {
		if !have[c9] {
			s.logRefs[c9] = append(s.logRefs[c9], uint32(idx))
			n++
		}
	}
	if n > 0 {
		s.logRefsN[kg] = n
	}
}

// load scans data/ne_cells for .nex files, using the SQLite header cache
// where (size, mtime) match. Returns the number of KGs.
func (s *neStoreT) load() int {
	s.openDB()
	cached := map[string]struct {
		size  int64
		mtime int64
		hdr   []byte
	}{}
	if s.db != nil {
		rows, err := s.db.Query(`SELECT kg, nex_bytes, nex_mtime, hdr FROM kgs`)
		if err == nil {
			for rows.Next() {
				var kg string
				var c struct {
					size  int64
					mtime int64
					hdr   []byte
				}
				if rows.Scan(&kg, &c.size, &c.mtime, &c.hdr) == nil {
					cached[kg] = c
				}
			}
			rows.Close()
		}
	}
	ents, _ := os.ReadDir(neDir)
	var loaded []*neKG
	seen := map[string]bool{}
	for _, e := range ents {
		name := e.Name()
		if !strings.HasSuffix(name, ".nex") || !neKGRe.MatchString(strings.TrimSuffix(name, ".nex")) {
			continue
		}
		kg := strings.TrimSuffix(name, ".nex")
		fi, err := e.Info()
		if err != nil {
			continue
		}
		seen[kg] = true
		path := filepath.Join(neDir, name)
		var k *neKG
		if c, ok := cached[kg]; ok && c.size == fi.Size() && c.mtime == fi.ModTime().UnixNano() {
			k, _ = neKGFromHeader(path, fi.Size(), fi.ModTime(), c.hdr)
		}
		if k == nil {
			k, err = neReadNexHeader(path)
			if err != nil {
				log.Printf("NE store: %s: %v", name, err)
				continue
			}
			hj, _ := neMarshalHeader(&k.Hdr)
			s.dbUpsert(k, hj)
		}
		if err := k.loadKRows(); err != nil {
			log.Printf("NE store: %s: %v", name, err)
			continue
		}
		loaded = append(loaded, k)
	}
	for kg := range cached {
		if !seen[kg] {
			s.dbDelete(kg)
		}
	}
	s.mu.Lock()
	for _, k := range loaded {
		idx, ok := s.byKG[k.KG]
		if ok {
			s.kgs[idx] = k
		} else {
			idx = len(s.kgs)
			s.kgs = append(s.kgs, k)
			s.byKG[k.KG] = idx
		}
	}
	s.refs = s.refs[:0]
	s.kBytes = 0
	for idx, k := range s.kgs {
		if _, ok := s.byKG[k.KG]; !ok {
			continue
		}
		for _, kr := range k.KRows {
			s.kBytes += int64(len(kr))
		}
		for di, d := range k.Hdr.Dir {
			s.refs = append(s.refs, neChunkRef{C9: d.c9, KG: uint32(idx), Dir: uint32(di)})
		}
	}
	s.sortRefsLocked()
	s.ready = true
	n := len(s.byKG)
	s.mu.Unlock()
	return n
}

// ── converter ──────────────────────────────────────────────────────────────

// neInputFiles — the ingest inputs present for a KG: layer → path.
func neInputFiles(kg string) map[string]string {
	out := map[string]string{}
	for layer, kind := range map[string]string{"lu": "nec", "bev": "bev.nec", "obs": "obs"} {
		if fi, err := os.Stat(neFilePath(kg, kind)); err == nil && fi.Size() > 0 {
			out[layer] = neFilePath(kg, kind)
		}
	}
	return out
}

// convert merges the inputs on disk into {kg}.nex (keeping stored layers
// that are not re-supplied), verifies, installs, deletes the inputs.
func (s *neStoreT) convert(kg string) error {
	s.convMu.Lock()
	defer s.convMu.Unlock()
	files := neInputFiles(kg)
	if len(files) == 0 {
		return nil
	}
	inputs := map[string]*neInput{}
	for layer, path := range files {
		in, err := neParseInputSection(path, layer)
		if err != nil {
			s.noteFailure(kg, err)
			return err
		}
		inputs[layer] = in
	}
	// carry over stored layers that have no new input
	if old := s.get(kg); old != nil {
		for name, l := range old.Hdr.Layers {
			if inputs[name] != nil {
				continue
			}
			rows, krows, err := s.layerRows(old, name)
			if err != nil {
				s.noteFailure(kg, err)
				return err
			}
			lc := *l
			lc.srcHeader = nil
			inputs[name] = &neInput{layer: lc, rows: rows, krows: krows, hdrBytes: l.Header}
			if !l.Sorted {
				// stored unsorted: rows came back in original order from the raw body
				inputs[name].layer.Sorted = false
			}
		}
	}
	if inputs["lu"] == nil && inputs["bev"] == nil {
		err := fmt.Errorf("%s: obs without a declared layer", kg)
		s.noteFailure(kg, err)
		return err
	}
	path, err := s.neWriteNex(kg, inputs)
	if err != nil {
		s.noteFailure(kg, err)
		return err
	}
	k, err := neReadNexHeader(path)
	if err == nil {
		err = k.loadKRows()
	}
	if err == nil {
		err = s.verify(k)
	}
	if err != nil {
		_ = os.Remove(path)
		s.noteFailure(kg, err)
		return err
	}
	hj, _ := neMarshalHeader(&k.Hdr)
	s.mu.Lock()
	s.installLocked(k)
	s.converted++
	delete(s.failed, kg)
	s.mu.Unlock()
	s.dbUpsert(k, hj)
	s.chunkLRU.dropPrefix(kg + "/")
	s.wholeLRU.dropPrefix(kg + "/")
	for _, p := range files {
		_ = os.Remove(p)
	}
	return nil
}

func (s *neStoreT) noteFailure(kg string, err error) {
	s.mu.Lock()
	s.failed[kg] = err.Error()
	s.mu.Unlock()
	log.Printf("NE store: convert %s: %v", kg, err)
}

// convertPending converts every KG that has an input file. Returns converted count.
func (s *neStoreT) convertPending() int {
	ents, _ := os.ReadDir(neDir)
	seen := map[string]bool{}
	n := 0
	for _, e := range ents {
		name := e.Name()
		kg := ""
		switch {
		case strings.HasSuffix(name, ".bev.nec"):
			kg = strings.TrimSuffix(name, ".bev.nec")
		case strings.HasSuffix(name, ".nec"):
			kg = strings.TrimSuffix(name, ".nec")
		case strings.HasSuffix(name, ".obs"):
			kg = strings.TrimSuffix(name, ".obs")
		}
		if kg == "" || !neKGRe.MatchString(kg) || seen[kg] {
			continue
		}
		seen[kg] = true
		s.mu.RLock()
		_, failedBefore := s.failed[kg]
		s.mu.RUnlock()
		if failedBefore {
			continue
		}
		if s.convert(kg) == nil {
			n++
		}
	}
	return n
}

// NEStoreStart — startup: load the index, then convert inputs in the
// background and keep watching for new ones (ne_all.sh output, POSTed obs).
func NEStoreStart() {
	n := neStore.load()
	var nexBytes int64
	neStore.mu.RLock()
	for kg, i := range neStore.byKG {
		_ = kg
		nexBytes += neStore.kgs[i].Size
	}
	chunks := len(neStore.refs)
	neStore.mu.RUnlock()
	log.Printf("🧩 NE store: kgs=%d chunks=%d nex_bytes=%d dicts=%v k_rows_bytes=%d", n, chunks, nexBytes, neDictIDs(), neStore.kBytes)
	go neCoverageKeepFresh() // landing page / llm.txt coverage line never stale
	go func() {
		for {
			if c := neStore.convertPending(); c > 0 {
				log.Printf("🧩 NE store: converted %d KG inputs → .nex (total kgs=%d)", c, len(neStore.kgCodes()))
				go neManifest.refresh(true)
			}
			time.Sleep(60 * time.Second)
		}
	}()
}

// NEStoreStats — size lines for /api/v1/debug/inflight (`ne_store`).
func NEStoreStats() map[string]interface{} {
	neStore.mu.RLock()
	var nexBytes int64
	for _, i := range neStore.byKG {
		nexBytes += neStore.kgs[i].Size
	}
	out := map[string]interface{}{
		"kgs": len(neStore.byKG), "chunk_refs": len(neStore.refs), "index_bytes": len(neStore.refs) * 16,
		"log_only_chunks": len(neStore.logRefs), "log_only_kgs": len(neStore.logRefsN),
		"nex_bytes": nexBytes, "ne_k_rows_bytes": neStore.kBytes, "converted_this_run": neStore.converted,
		"failed": neStore.failed, "ready": neStore.ready, "dicts": neDictIDs(),
		"budget_gb": neBudgetGB(),
	}
	neStore.mu.RUnlock()
	out["ne_chunk_lru"] = neStore.chunkLRU.stats()
	out["ne_whole_lru"] = neStore.wholeLRU.stats()
	return out
}

// neBudgetGB — NE_BUDGET_GB (default 8). Informative: the whole country fits
// (~1.2 GB lu + obs), so eviction never fires in steady state (invariant 10);
// we only report it.
func neBudgetGB() float64 {
	if v, err := strconv.ParseFloat(os.Getenv("NE_BUDGET_GB"), 64); err == nil && v > 0 {
		return v
	}
	return 8
}

// neResetStoreForTest — fresh store state (tests only).
func neResetStoreForTest() {
	if neStore.db != nil {
		_ = neStore.db.Close()
	}
	neStore = &neStoreT{byKG: map[string]int{}, dicts: map[string][]byte{}, enc: map[string]*zstd.Encoder{},
		chunkLRU: newNELRU(neChunkLRUMax), wholeLRU: newNELRU(neWholeLRUMax), failed: map[string]string{},
		flight: map[string]*neFlight{}}
}

// NETrainDict — operator pass: dump the raw res-9 chunk rows of `layer` from
// up to `sample` inputs/stores into a temp dir and run `zstd --train` into
// data/ne_cells/dict/{layer}-{n}.zd (16 KB). A new dictionary is a new id;
// stored chunks keep theirs (the .nex header says which). Retrain only when a
// layer format changes.
func NETrainDict(layer string, n int, sample int) error {
	tmp, err := os.MkdirTemp("", "nedict")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	files := 0
	dump := func(rows []byte, rb int, tag string) {
		cur := uint64(0)
		var buf []byte
		flush := func() {
			if len(buf) > 0 {
				_ = os.WriteFile(filepath.Join(tmp, fmt.Sprintf("%s-%x", tag, cur)), buf, 0o644)
				files++
				buf = nil
			}
		}
		for i := 0; i < len(rows)/rb; i++ {
			p := h3Parent(neRowID(rows, i, rb), neChunkRes)
			if p != cur {
				flush()
				cur = p
			}
			buf = append(buf, rows[i*rb:(i+1)*rb]...)
		}
		flush()
	}
	used := 0
	for _, kg := range neStore.kgCodes() {
		if used >= sample {
			break
		}
		k := neStore.get(kg)
		if k == nil || k.Hdr.Layers[layer] == nil {
			continue
		}
		rows, _, err := neStore.layerRows(k, layer)
		if err == nil {
			dump(rows, k.Hdr.Layers[layer].RowBytes, kg)
			used++
		}
	}
	ents, _ := os.ReadDir(neDir)
	kind := map[string]string{"lu": ".nec", "bev": ".bev.nec", "obs": ".obs"}[layer]
	for _, e := range ents {
		if used >= sample || !strings.HasSuffix(e.Name(), kind) || (kind == ".nec" && strings.HasSuffix(e.Name(), ".bev.nec")) {
			continue
		}
		in, err := neParseInputSection(filepath.Join(neDir, e.Name()), layer)
		if err == nil {
			dump(in.rows, in.layer.RowBytes, strings.TrimSuffix(e.Name(), kind))
			used++
		}
	}
	if files < 50 {
		return fmt.Errorf("only %d chunk samples from %d inputs — need more", files, used)
	}
	if err := os.MkdirAll(neDictDir(), 0o755); err != nil {
		return err
	}
	out := neDictPath(fmt.Sprintf("%s-%d", layer, n))
	args := []string{"--train", "--maxdict=16384", "-o", out, "-r", tmp, "-q"}
	cmd := exec.Command("zstd", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("zstd --train: %w", err)
	}
	log.Printf("NE dict trained: %s from %d chunks of %d inputs", out, files, used)
	return nil
}

// ── read primitives used by the API ──────────────────────────────────────────

// neCellHit — one KG's row for a res-12 cell.
type neCellHit struct {
	KG    string
	Row   []byte
	Share uint8 // kg_share (LU rows); 255 for layers without one
}

// cellRows — the rows of one res-12 cell in every KG that holds it (KG order).
func (s *neStoreT) cellRows(id uint64, layer string) ([]neCellHit, error) {
	var out []neCellHit
	for _, h := range s.chunkHits(h3Parent(id, neChunkRes)) {
		l := h.KG.Hdr.Layers[layer]
		if l == nil {
			continue
		}
		rows, err := s.chunkRows(h.KG, h.Dir, layer)
		if err != nil {
			return nil, err
		}
		if row := neFindRow(rows, l.RowBytes, id); row != nil {
			sh := uint8(255)
			if layer != "obs" {
				sh = row[8]
			}
			out = append(out, neCellHit{KG: h.KG.KG, Row: row, Share: sh})
		}
	}
	return out, nil
}

// neDeclaredDigest — the digest that identifies a KG's declared rows as a
// request reads them: `lu` (index epoch), or with bevFirst lu+bev — the
// peer's bevdirect build read first, lu where bev lacks the cell (neCollect,
// declaredRow). Both are NEC1 of the same algo and row layout.
func neDeclaredDigest(k *neKG, bevFirst bool) string {
	out := "-"
	if l := k.Hdr.Layers["lu"]; l != nil {
		out = l.Digest
	}
	if !bevFirst {
		return out
	}
	if l := k.Hdr.Layers["bev"]; l != nil {
		out += "+" + l.Digest
	}
	if st := neDeclaredLogState(k.KG); st != "" {
		out += "+chg2:" + st // the chunk-log view, logged chunks authoritative (ne_change_declared.go)
	}
	return out
}

// declaredRow — the declared row of a res-12 cell (largest kg_share wins) +
// kg→share; with bevFirst, per KG: chunk-log view → `bev` → `lu`, logged
// res-10 chunks authoritative (neDeclaredCellHits, neCollect).
func (s *neStoreT) declaredRow(id uint64, bevFirst bool) ([]byte, map[string]uint8, error) {
	if bevFirst {
		return pickDeclared(neDeclaredCellHits(id, s.chunkHits(h3Parent(id, neChunkRes))))
	}
	var hits []neCellHit
	luHits, err := s.cellRows(id, "lu")
	if err != nil {
		return nil, nil, err
	}
	return pickDeclared(neMergeHits(hits, luHits))
}

// neMergeHits — append the hits of KGs not already present.
func neMergeHits(hits, more []neCellHit) []neCellHit {
	for _, h := range more {
		dup := false
		for _, b := range hits {
			if b.KG == h.KG {
				dup = true
				break
			}
		}
		if !dup {
			hits = append(hits, h)
		}
	}
	return hits
}

func pickDeclared(hits []neCellHit) ([]byte, map[string]uint8, error) {
	if len(hits) == 0 {
		return nil, nil, nil
	}
	kgs := make(map[string]uint8, len(hits))
	best := hits[0]
	for _, h := range hits {
		kgs[h.KG] = h.Share
		if h.Share > best.Share {
			best = h
		}
	}
	return best.Row, kgs, nil
}

// kRow — the K row (res 10) of a KG layer, or nil.
func (k *neKG) kRow(layer string, id uint64) []byte {
	return neFindRow(k.KRows[layer], neKRowBytes, id)
}

// kRowAny — the K row of a res-10 cell from any KG holding its res-9 parent
// (identical bytes across KGs, so the first wins).
func (s *neStoreT) kRowAny(id uint64, layer string) ([]byte, string) {
	for _, h := range s.chunkHits(h3Parent(id, neChunkRes)) {
		if r := h.KG.kRow(layer, id); r != nil {
			return r, h.KG.KG
		}
	}
	return nil, ""
}

// materialise — the whole-KG container for the requested layers (missing
// layers skipped; caller reports them). Private tier: stored header bytes
// verbatim. Public tier: header rewritten (api_ne_public.go). Bounded LRU.
func (s *neStoreT) materialise(k *neKG, layers []string, public bool) ([]byte, error) {
	key := k.KG + "/" + strings.Join(layers, ",") + "/" + map[bool]string{false: "k", true: "pub"}[public]
	if b, ok := s.wholeLRU.get(key); ok {
		return b, nil
	}
	// single-flight: 16 concurrent first GETs of one KG must inflate+deflate
	// 1.6 MB once, not 16 times (memory as much as CPU on a 2-core box)
	s.flightMu.Lock()
	if c, ok := s.flight[key]; ok {
		s.flightMu.Unlock()
		<-c.done
		return c.out, c.err
	}
	c := &neFlight{done: make(chan struct{})}
	s.flight[key] = c
	s.flightMu.Unlock()
	c.out, c.err = s.materialiseSlow(k, layers, public, key)
	close(c.done)
	s.flightMu.Lock()
	delete(s.flight, key)
	s.flightMu.Unlock()
	return c.out, c.err
}

type neFlight struct {
	done chan struct{}
	out  []byte
	err  error
}

func (s *neStoreT) materialiseSlow(k *neKG, layers []string, public bool, key string) ([]byte, error) {
	var out []byte
	for _, name := range layers {
		l := k.Hdr.Layers[name]
		if l == nil {
			continue
		}
		rows, krows, err := s.layerRows(k, name)
		if err != nil {
			return nil, err
		}
		hdr := []byte(l.Header)
		if public {
			hdr = neRewriteHeaderPublic(k.layerHeader(name))
		}
		body := zlibDeflateServe(append(append(make([]byte, 0, len(rows)+len(krows)), rows...), krows...))
		sec := make([]byte, 0, 13+len(hdr)+len(body))
		sec = append(sec, l.Magic...)
		sec = append(sec, 1)
		sec = binary.LittleEndian.AppendUint32(sec, uint32(len(hdr)))
		sec = append(sec, hdr...)
		sec = binary.LittleEndian.AppendUint32(sec, uint32(len(body)))
		sec = append(sec, body...)
		out = append(out, sec...)
	}
	if len(out) > 0 {
		s.wholeLRU.put(key, out)
	}
	return out, nil
}

// etag — digest tuple of the layers present (known without materialising).
func (k *neKG) etag(layers []string) string {
	parts := make([]string, 0, len(layers))
	for _, name := range layers {
		if l := k.Hdr.Layers[name]; l != nil {
			parts = append(parts, l.Digest)
		} else {
			parts = append(parts, "-")
		}
	}
	return `"ne-` + strings.Join(parts, "-") + `"`
}

// getOrConvert — the KG from the store; a KG whose inputs just landed on
// disk (ne_all.sh output, before the 60 s converter tick) is converted on the
// spot (~0.3 s CPU, no SQL) so a peer's first GET after the build succeeds.
func (s *neStoreT) getOrConvert(kg string) *neKG {
	if k := s.get(kg); k != nil {
		return k
	}
	if len(neInputFiles(kg)) == 0 {
		return nil
	}
	s.mu.RLock()
	_, failed := s.failed[kg]
	s.mu.RUnlock()
	if failed {
		return nil
	}
	_ = s.convert(kg)
	return s.get(kg)
}
