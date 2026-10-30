// ne_change.go — NE change: the per-KG chunk-version log and the delta engine
// (docs/ne-change.md §3–4). Public-path file: word ban applies.
//
// Log {kg}.chunks.zst = a valid zstd stream: per upload one SKIPPABLE frame
// (magic 0x184D2A5E) carrying a JSON header {kind:"batch"|"seen", token,
// observer, source, epoch, at, bbox, items:[[chunk, digest, digest_ap, rows,
// frame_len]…]} followed, for kind=batch, by one zstd frame (dict lu-1) per
// item of the stripped LU rows (bytes 24–25 of every row are 0; K records never
// arrive). kind=seen carries chunks{chunk: digest_ap} re-sighted, no frames.
// `zstd -d -D lu-1.zd` on the file yields every version's rows in order.
// Append-only; nothing is ever rewritten (§7: history is the product).
//
// RAM: neChangeLRU holds the parsed log (headers + per-chunk version index,
// deltas) of the most recently used KGs, bounded by count AND bytes; size
// line `ne_change` on /debug/inflight. Zero SQL anywhere on this path.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/uber/h3-go/v4"
)

const (
	neChangeSkipMagic    = 0x184D2A5E
	neChangeLogKind      = "chunks.zst"
	neChangeDerived      = "changes.jsonl"
	neChangeLRUMaxKGs    = 256
	neChangeLRUMaxB      = 24 << 20
	neChangeMaxBody      = 2 << 20
	neChunkRes10         = 10
	neRegOff0, neRegOff1 = 24, 25 // the two register-derived bytes — zero in every stored row
)

// neChunkVersion — one chunk version (in RAM; on disk one header per batch).
type neChunkVersion struct {
	Kind     string            `json:"kind"` // version | seen
	Chunk    string            `json:"chunk"`
	Digest   string            `json:"digest"`    // frozen full digest from the report
	AP       string            `json:"digest_ap"` // sha256[:16] of the stripped rows (the change key)
	Token    string            `json:"token,omitempty"`
	Observer string            `json:"observer"`
	Source   string            `json:"source"`
	Epoch    string            `json:"epoch,omitempty"`
	At       string            `json:"at"`
	BBox     string            `json:"bbox,omitempty"`
	Rows     int               `json:"rows"`
	FrameLen int               `json:"frame_len,omitempty"`
	TileHash string            `json:"tile_hash,omitempty"` // QC provenance: hash of the source tiles the peer built the chunk from ("" = not reported)
	Chunks   map[string]string `json:"chunks,omitempty"`    // kind=seen|tiles: chunk → digest_ap re-sighted
	Tiles    map[string]string `json:"tiles,omitempty"`     // kind=tiles (append only, never stored on a version entry): chunk → tile hash
	off      int64             // data frame offset in the file
	chunk    uint64
}

// neChangeBatchHdr — the on-disk header of one upload.
type neChangeBatchHdr struct {
	Kind     string            `json:"kind"` // batch | seen
	Token    string            `json:"token,omitempty"`
	Observer string            `json:"observer"`
	Source   string            `json:"source"`
	Epoch    string            `json:"epoch,omitempty"`
	At       string            `json:"at"`
	BBox     string            `json:"bbox,omitempty"`
	Items    [][]interface{}   `json:"items,omitempty"` // [chunk, digest, digest_ap, rows, frame_len, tile_hash?]
	Chunks   map[string]string `json:"chunks,omitempty"`
	Tiles    map[string]string `json:"tiles,omitempty"` // kind=tiles: chunk → tile hash, late provenance for the held version named in Chunks
}

// neChangeItem — one chunk of an upload.
type neChangeItem struct {
	Chunk, Digest, AP string
	TileHash          string // optional provenance (docs/ne-qc-proposal.md §1)
	Rows              []byte
}

// neChangeKG — parsed log of one KG.
type neChangeKG struct {
	kg       string
	logBytes int64
	entries  []neChunkVersion
	versions map[uint64][]int // chunk → entry indexes of kind=version, in order
	seen     map[string]int   // digest_ap → sightings (version + seen entries)
	latest   map[uint64]string
	rowsLRU  map[int][]byte // entry index → inflated rows (small, bounded by the KG's own size)
	deltas   *neChangeDeltas
	ram      int

	// A loaded KG is shared by every request goroutine that reads it (tiles of
	// one KG arrive in bursts of a dozen). entries/versions/seen/latest are
	// immutable after neChangeLoad (an append replaces the object, see
	// neChangeAppend); the two lazy caches are the only mutation and each has
	// its own lock — rowsMu for rowsLRU/ram, deltasMu for deltas (deltasOf
	// calls rows, so they must not share one). Found by the glyph soak as
	// "fatal error: concurrent map read and map write" in rows() (2026-10-29).
	rowsMu   sync.Mutex
	deltasMu sync.Mutex
	// pass table + windowed cumulations (ne_change_passes.go), own lock
	passMu sync.Mutex
	pass   *nePassKG
	winCum map[string]map[uint64]*neCellCum
}

func (c *neChangeKG) latestAP(chunk uint64) (string, bool) {
	s, ok := c.latest[chunk]
	return s, ok
}

type neChangeLRUT struct {
	mu           sync.Mutex
	kgs          map[string]*neChangeKG
	order        []string // oldest first
	bytes        int
	hits, misses int
}

var neChangeLRU = &neChangeLRUT{kgs: map[string]*neChangeKG{}}

func neChangeLogPath(kg string) string { return neFilePath(kg, neChangeLogKind) }

func neChangeLogSizeAny(kgs interface{}) int64 {
	var n int64
	if m, ok := kgs.(map[string]uint8); ok {
		for kg := range m {
			n += neChangeLogSize(kg)
		}
	}
	return n
}

func neChangeLogSize(kg string) int64 {
	st, err := os.Stat(neChangeLogPath(kg))
	if err != nil {
		return 0
	}
	return st.Size()
}

// neChangeGet — parsed log for kg (nil when there is none). Reloads when the
// file grew (append from another path) by comparing sizes.
func neChangeGet(kg string) *neChangeKG {
	st, err := os.Stat(neChangeLogPath(kg))
	if err != nil {
		return nil
	}
	l := neChangeLRU
	l.mu.Lock()
	if c, ok := l.kgs[kg]; ok && c.logBytes == st.Size() {
		l.hits++
		l.touchLocked(kg)
		l.mu.Unlock()
		return c
	}
	l.misses++
	l.mu.Unlock()
	c, err := neChangeLoad(kg)
	if err != nil || c == nil {
		return nil
	}
	l.mu.Lock()
	l.putLocked(c)
	l.mu.Unlock()
	return c
}

func (l *neChangeLRUT) touchLocked(kg string) {
	for i, k := range l.order {
		if k == kg {
			l.order = append(append(l.order[:i:i], l.order[i+1:]...), kg)
			return
		}
	}
	l.order = append(l.order, kg)
}

func (l *neChangeLRUT) putLocked(c *neChangeKG) {
	if old, ok := l.kgs[c.kg]; ok {
		l.bytes -= old.ram
	}
	l.kgs[c.kg] = c
	l.bytes += c.ram
	l.touchLocked(c.kg)
	for (len(l.kgs) > neChangeLRUMaxKGs || l.bytes > neChangeLRUMaxB) && len(l.order) > 1 {
		k := l.order[0]
		l.order = l.order[1:]
		if o, ok := l.kgs[k]; ok {
			l.bytes -= o.ram
			delete(l.kgs, k)
		}
	}
}

func (l *neChangeLRUT) drop(kg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if o, ok := l.kgs[kg]; ok {
		o.rowsMu.Lock()
		l.bytes -= o.ram
		o.rowsMu.Unlock()
		delete(l.kgs, kg)
		for i, k := range l.order {
			if k == kg {
				l.order = append(l.order[:i:i], l.order[i+1:]...)
				break
			}
		}
	}
}

// NEChangeStats — size line for /debug/inflight.
func NEChangeStats() map[string]interface{} {
	l := neChangeLRU
	l.mu.Lock()
	defer l.mu.Unlock()
	return map[string]interface{}{"kgs": len(l.kgs), "bytes": l.bytes, "max_kgs": neChangeLRUMaxKGs, "max_bytes": neChangeLRUMaxB,
		"hits": l.hits, "misses": l.misses, "pending_wants": neWantCount()}
}

// ── log IO ───────────────────────────────────────────────────────────────────

func neAPDigest(rows []byte) string {
	s := sha256.Sum256(rows)
	return hex.EncodeToString(s[:])[:16]
}

// neChangeAppend — one upload: items (versions, each with rows) or, with no
// items, a bulk sighting (meta.Chunks). One header per call; the loaded KG is
// updated in place (no reload).
func neChangeAppend(kg string, meta neChunkVersion, items []neChangeItem) error {
	hdr := neChangeBatchHdr{Kind: "batch", Token: meta.Token, Observer: meta.Observer, Source: meta.Source, Epoch: meta.Epoch, At: meta.At, BBox: meta.BBox}
	var frames [][]byte
	for _, it := range items {
		if len(it.Rows) == 0 || len(it.Rows)%neLURowBytes != 0 {
			return errors.New("rows not a multiple of the row size")
		}
		for i := 0; i < len(it.Rows); i += neLURowBytes {
			if it.Rows[i+neRegOff0] != 0 || it.Rows[i+neRegOff1] != 0 {
				return errors.New("register bytes must be zero")
			}
		}
		if neAPDigest(it.Rows) != it.AP {
			return errors.New("digest_ap mismatch")
		}
		fr, err := neStore.compress(neStore.latestDict("lu"), it.Rows)
		if err != nil {
			return err
		}
		frames = append(frames, fr)
		item := []interface{}{it.Chunk, it.Digest, it.AP, len(it.Rows) / neLURowBytes, len(fr)}
		if it.TileHash != "" {
			item = append(item, it.TileHash)
		}
		hdr.Items = append(hdr.Items, item)
	}
	if len(items) == 0 {
		hdr.Kind, hdr.Chunks = "seen", meta.Chunks
		if len(meta.Tiles) > 0 {
			hdr.Kind, hdr.Tiles = "tiles", meta.Tiles
		}
		if len(meta.Chunks) == 0 {
			return nil
		}
	}
	hj, _ := json.Marshal(hdr)
	var buf bytes.Buffer
	var h8 [8]byte
	binary.LittleEndian.PutUint32(h8[:], neChangeSkipMagic)
	binary.LittleEndian.PutUint32(h8[4:], uint32(len(hj)))
	buf.Write(h8[:])
	buf.Write(hj)
	for _, fr := range frames {
		buf.Write(fr)
	}
	neWriteMu.Lock()
	_ = os.MkdirAll(neDir, 0o755)
	f, err := os.OpenFile(neChangeLogPath(kg), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		neWriteMu.Unlock()
		return err
	}
	_, err = f.Write(buf.Bytes())
	_ = f.Close()
	// Copy-on-write: never mutate the loaded object in place — request
	// goroutines may be reading its maps right now. Drop it (also on a failed
	// write); the next reader reloads the log (≤ 10 MB, a few ms) and gets a
	// fresh immutable object.
	neChangeLRU.drop(kg)
	neWriteMu.Unlock()
	if err == nil {
		// Refresh the declared-view state now (one log read): chunks only the
		// log holds must reach neStore.chunkHits before anyone asks for them.
		neDeclStateOf(kg)
	}
	return err
}

// ingest — expand one on-disk header into entries; dataOff = offset of its first frame.
func (c *neChangeKG) ingest(h *neChangeBatchHdr, dataOff int64, hdrLen int) {
	if h.Kind == "tiles" {
		// late provenance (docs/ne-change.md §5a): a re-sighting of the version we
		// hold under this digest_ap, carrying the tile hash it was built from —
		// attached to the held version when it has none. Not a sighting.
		for ch, ap := range h.Chunks {
			th := h.Tiles[ch]
			id, err := strconv.ParseUint(ch, 16, 64)
			if err != nil || th == "" {
				continue
			}
			if vs := c.versions[id]; len(vs) > 0 {
				if e := &c.entries[vs[len(vs)-1]]; e.AP == ap && e.TileHash == "" {
					e.TileHash = th
				}
			}
		}
		c.entries = append(c.entries, neChunkVersion{Kind: "tiles", Token: h.Token, Observer: h.Observer, Source: h.Source, Epoch: h.Epoch, At: h.At, Chunks: h.Chunks})
		c.ram += 160 + hdrLen
		return
	}
	if h.Kind == "seen" {
		v := neChunkVersion{Kind: "seen", Token: h.Token, Observer: h.Observer, Source: h.Source, Epoch: h.Epoch, At: h.At, BBox: h.BBox, Chunks: h.Chunks}
		for _, ap := range h.Chunks {
			c.seen[ap]++
		}
		c.entries = append(c.entries, v)
		c.ram += 160 + hdrLen
		return
	}
	off := dataOff
	for _, it := range h.Items {
		if len(it) < 5 {
			continue
		}
		v := neChunkVersion{Kind: "version", Token: h.Token, Observer: h.Observer, Source: h.Source, Epoch: h.Epoch, At: h.At, BBox: h.BBox}
		v.Chunk, _ = it[0].(string)
		v.Digest, _ = it[1].(string)
		v.AP, _ = it[2].(string)
		v.Rows = asInt(it[3])
		v.FrameLen = asInt(it[4])
		if len(it) > 5 {
			v.TileHash, _ = it[5].(string)
		}
		v.off = off
		off += int64(v.FrameLen)
		v.chunk, _ = strconv.ParseUint(v.Chunk, 16, 64)
		idx := len(c.entries)
		c.entries = append(c.entries, v)
		c.seen[v.AP]++
		c.versions[v.chunk] = append(c.versions[v.chunk], idx)
		c.latest[v.chunk] = v.AP
		c.ram += 200
	}
}

func neChangeLoad(kg string) (*neChangeKG, error) {
	b, err := os.ReadFile(neChangeLogPath(kg))
	if err != nil {
		return nil, err
	}
	c := &neChangeKG{kg: kg, logBytes: int64(len(b)), versions: map[uint64][]int{}, seen: map[string]int{},
		latest: map[uint64]string{}, rowsLRU: map[int][]byte{}}
	off := 0
	for off+8 <= len(b) {
		if binary.LittleEndian.Uint32(b[off:]) != neChangeSkipMagic {
			return nil, fmt.Errorf("log %s: bad frame at %d", kg, off)
		}
		n := int(binary.LittleEndian.Uint32(b[off+4:]))
		off += 8
		if off+n > len(b) {
			return nil, fmt.Errorf("log %s: truncated header", kg)
		}
		var h neChangeBatchHdr
		if err := json.Unmarshal(b[off:off+n], &h); err != nil {
			return nil, err
		}
		off += n
		c.ingest(&h, int64(off), n)
		for _, it := range h.Items {
			if len(it) >= 5 {
				off += asInt(it[4])
			}
		}
		if off > len(b) {
			return nil, fmt.Errorf("log %s: truncated frame", kg)
		}
	}
	return c, nil
}

// rows — inflated rows of a version entry (cached per loaded KG).
func (c *neChangeKG) rows(i int) ([]byte, error) {
	c.rowsMu.Lock()
	r, ok := c.rowsLRU[i]
	c.rowsMu.Unlock()
	if ok {
		return r, nil
	}
	v := c.entries[i]
	f, err := os.Open(neChangeLogPath(c.kg))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fr := make([]byte, v.FrameLen)
	if _, err := f.ReadAt(fr, v.off); err != nil {
		return nil, err
	}
	r, err = neStore.decompress(fr, v.Rows*neLURowBytes)
	if err != nil {
		return nil, err
	}
	if neAPDigest(r) != v.AP {
		return nil, errors.New("stored rows do not match digest_ap")
	}
	c.rowsMu.Lock()
	if prev, ok := c.rowsLRU[i]; ok { // lost the race: keep the first copy
		r = prev
	} else {
		c.rowsLRU[i] = r
		c.ram += len(r)
	}
	c.rowsMu.Unlock()
	return r, nil
}

// neChangeConfirm — record re-sightings: for every (chunk → digest_ap) that
// equals the latest version we hold and is not yet confirmed, one bulk seen
// entry. Returns how many chunks it confirmed. Bounded growth: at most one
// sighting entry per version (a confirmed version is never re-listed).
func neChangeConfirm(kg, token, observer, source, epoch, at string, pairs map[string]string) int {
	c := neChangeGet(kg)
	if c == nil || len(pairs) == 0 {
		return 0
	}
	hit := map[string]string{}
	for ch, ap := range pairs {
		id, err := strconv.ParseUint(ch, 16, 64)
		if err != nil {
			continue
		}
		vs := c.versions[id]
		if len(vs) == 0 {
			continue
		}
		e := c.entries[vs[len(vs)-1]]
		if e.AP != ap || c.seen[ap] >= 2 {
			continue
		}
		// the same observer confirms only on a later pass (BEV regenerates tiles
		// nightly): a re-post of the same build within hours is not a sighting
		if e.Observer == observer && !neLaterPass(e.At, at) {
			continue
		}
		hit[ch] = ap
	}
	if len(hit) == 0 {
		return 0
	}
	_ = neChangeAppend(kg, neChunkVersion{Token: token, Observer: observer, Source: source, Epoch: epoch, At: at, Chunks: hit}, nil)
	return len(hit)
}

// neChangeAttachTiles — late provenance: for held chunks whose latest version
// has the reported digest_ap but no tile hash yet, record the peer's tile hash
// with that version (kind=tiles log entry). Without this, a KG baselined before
// peers sent provenance would never get a hash, and the "same input, different
// digest" check could never fire on its next real version.
func neChangeAttachTiles(kg, token, observer, source, epoch, at string, pairs map[string]string, tiles map[string]string) int {
	c := neChangeGet(kg)
	if c == nil || len(pairs) == 0 || len(tiles) == 0 {
		return 0
	}
	aps, ths := map[string]string{}, map[string]string{}
	for ch, ap := range pairs {
		th := neTileHashNorm(tiles[ch])
		if th == "" {
			continue
		}
		id, err := strconv.ParseUint(ch, 16, 64)
		if err != nil {
			continue
		}
		vs := c.versions[id]
		if len(vs) == 0 {
			continue
		}
		if e := c.entries[vs[len(vs)-1]]; e.AP == ap && e.TileHash == "" {
			aps[ch], ths[ch] = ap, th
		}
	}
	if len(aps) == 0 {
		return 0
	}
	_ = neChangeAppend(kg, neChunkVersion{Token: token, Observer: observer, Source: source, Epoch: epoch, At: at, Chunks: aps, Tiles: ths}, nil)
	return len(aps)
}

const neSamePassWindow = 20 * time.Hour

func neLaterPass(prevAt, at string) bool {
	a, err1 := time.Parse(time.RFC3339, prevAt)
	b, err2 := time.Parse(time.RFC3339, at)
	return err1 == nil && err2 == nil && b.Sub(a) >= neSamePassWindow
}

// neChangeLatestDigests — chunk → (frozen digest, digest_ap) of the latest held version.
func (c *neChangeKG) latestDigests() map[string]neWant {
	out := make(map[string]neWant, len(c.versions))
	for ch, vs := range c.versions {
		e := c.entries[vs[len(vs)-1]]
		out[neCellHex(ch)] = neWant{Digest: e.Digest, AP: e.AP, Tile: e.TileHash}
	}
	return out
}

// ── pending wants: what we asked an observer to send (step 1 → step 2) ──────

type neWant struct {
	Digest, AP string
	Tile       string // optional tile hash the peer reported for the chunk (QC provenance)
}

// neTileHashNorm — a tile hash is any 16–64 hex digest; stored truncated to 16.
func neTileHashNorm(s string) string {
	if len(s) < 16 || len(s) > 64 {
		return ""
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return ""
		}
	}
	return s[:16]
}

var (
	neWantMu    sync.Mutex
	neWants     = map[string]map[string]neWant{} // kg|token|observer → chunk → want
	neWantOrder []string
)

const neWantMax = 4096

func neWantKey(kg, token, observer string) string { return kg + "|" + token + "|" + observer }

func neWantSet(kg, token, observer string, m map[string]neWant) {
	k := neWantKey(kg, token, observer)
	neWantMu.Lock()
	defer neWantMu.Unlock()
	if _, ok := neWants[k]; !ok {
		neWantOrder = append(neWantOrder, k)
	}
	neWants[k] = m
	for len(neWantOrder) > neWantMax {
		delete(neWants, neWantOrder[0])
		neWantOrder = neWantOrder[1:]
	}
}

func neWantGet(kg, token, observer string) map[string]neWant {
	neWantMu.Lock()
	m := neWants[neWantKey(kg, token, observer)]
	neWantMu.Unlock()
	if m != nil {
		return m
	}
	// after a restart: the latest stored report of this observer carries want_ap
	st := neReadReports(kg)
	for i := len(st) - 1; i >= 0; i-- {
		s := &st[i]
		if s.Token != token || s.Report.Observer != observer {
			continue
		}
		wa, _ := s.Result["want_ap"].(map[string]interface{})
		if wa == nil {
			return nil
		}
		wt, _ := s.Result["want_tiles"].(map[string]interface{})
		m = map[string]neWant{}
		for c, v := range wa {
			if ap, _ := v.(string); ap != "" {
				th, _ := wt[c].(string)
				m[c] = neWant{Digest: s.Report.Chunks[c], AP: ap, Tile: th}
			}
		}
		neWantSet(kg, token, observer, m)
		return m
	}
	return nil
}

func neWantCount() int {
	neWantMu.Lock()
	defer neWantMu.Unlock()
	return len(neWants)
}

// ── delta engine ─────────────────────────────────────────────────────────────

// neCellDelta — one cell between two consecutive versions of its chunk.
type neCellDelta struct {
	Cell      uint64
	Chunk     uint64
	From, To  int // entry indexes
	DLU       [9]int
	DCover    int // m²
	DN        int
	DLine     int  // m/ha
	Unknown   bool // declared_unknown on either side → DLU not asserted
	Confirmed bool
	Share     uint8  // kg_share of the `to` row (dedupe rule: counts where ≥ 128)
	Absent    bool   // cell missing on one side (layout / sliver) — not counted
	At        string // `at` of the digest that brought the new version (detection time)
	Pass      int    // sighting ordinal of the new version within its chunk (2 = the chunk's second pass; ne_change_passes.go)
	// QC (docs/ne-qc-proposal.md): a delta failing either check is neither
	// candidate nor confirmed — it is listed, counted in summary.qc, never summed.
	SameInput bool // both versions carry a tile hash and it is equal: the input did not change, the pipeline did
	Jitter    bool // below the quantisation floor: no dominant-class flip, no structure count change, every Δ < neQCFloorShare of the cell
	Coverage  bool // the two versions hold different cell SETS (neVersionsCoverage): a fetch/domain difference, not land
}

// neVersionsCoverage — per-chunk coverage rule (docs/ne-cells.md §coverage,
// 2026-10-24). A complete build emits a fixed cell set per res-10 chunk: land
// does not vanish or appear, and since bevdirect v0.3.3 the request bbox only
// selects chunks (neLayoutKey). Two versions of one chunk with DIFFERENT cell
// sets therefore differ in what was fetched or in the observer's domain — a
// build truncated by a missing tile or a partial cell set, or two observers
// that disagree on the KG's edge cells ({MAP_NAME} emits kg_share 255 over the
// neighbours' land where srtm has partial shares; 31 of 1 296 version
// pairs in the log on 2026-10-24, every one at a KG boundary, mixed
// directions) — never land-use change. Such a pair is listed with status
// "coverage" and never cumulated, confirmed or pending; the next version
// compares against the (possibly truncated) latest one by the same rule, so a
// complete build after a truncated one re-baselines instead of "gaining land".
// No reference bbox, no peer field: it needs only the rows the log already holds.
func neVersionsCoverage(ma, mb map[uint64][]byte) bool {
	if len(ma) != len(mb) {
		return true
	}
	for id := range ma {
		if _, ok := mb[id]; !ok {
			return true
		}
	}
	return false
}

// neQCFloorShare — quantisation floor. z16 tiles have a 4096 extent (≈ 0.6 m);
// a 1/255 share step on a 307 m² res-12 cell is 1.2 m². Boundary vertex jitter
// moves a few m² per cell without any class winning or losing the cell; 5 %
// of the cell (≈ 15 m²) is well above that noise and well below the smallest
// object BEV maps.
const neQCFloorShare = 0.05

// neQCStatus — the status string of a delta, QC included.
func neQCStatus(x *neCellDelta) string {
	switch {
	case x.Coverage:
		return "coverage"
	case x.SameInput:
		return "pipeline"
	case x.Jitter:
		return "jitter"
	}
	return neStatus(x.Confirmed)
}

// neDeltaJitter — the §4 quantisation-floor rule on a (from, to) row pair.
func neDeltaJitter(a, b []byte, x *neCellDelta, area float64) bool {
	if x.DN != 0 {
		return false
	}
	floor := int(neQCFloorShare*area + 0.5)
	if x.DCover >= floor || -x.DCover >= floor {
		return false
	}
	for _, v := range x.DLU {
		if v >= floor || -v >= floor {
			return false
		}
	}
	return neDominantClass(a) == neDominantClass(b)
}

func neDominantClass(row []byte) int {
	best, bi := -1, -1
	for j := 0; j < 9; j++ {
		if int(row[12+j]) > best {
			best, bi = int(row[12+j]), j
		}
	}
	return bi
}

type neChangeDeltas struct {
	rows  []neCellDelta
	since string // earliest version date
	cum   map[uint64]*neCellCum
}

// neCellCum — cumulative state per cell (sum of consecutive deltas).
type neCellCum struct {
	DLU          [9]int
	DCover, DN   int
	DLine        int
	Confirmed    bool
	LastAt       string // `at` of the digest that confirmed the latest counted delta
	FirstAt      string // `at` of the digest in which the change was first sighted (candidate or confirmed)
	AnyAt        string // `at` of the latest counted delta, confirmed or not
	FromAP, ToAP string
	Share        uint8
	Unknown      bool
	Candidate    bool
	CandDLU      [9]int // Δ of the deltas still awaiting a second sighting (not asserted, shown as pending)
	Pass         int    // sighting ordinal of the earliest counted delta's new version (2 = detected on the chunk's second pass)
	PassLast     int    // … of the latest counted delta
}

func neCellAreaM2(id uint64) float64 {
	a, err := h3.CellAreaM2(h3.Cell(id))
	if err != nil || a <= 0 {
		return 307.0
	}
	return a
}

// neRowDelta — subtract two stripped rows (b − a) in m² / counts.
func neRowDelta(a, b []byte, area float64) (d neCellDelta) {
	for j := 0; j < 9; j++ {
		d.DLU[j] = int(float64(int(b[12+j])-int(a[12+j]))/255*area + 0.5*sign(int(b[12+j])-int(a[12+j])))
	}
	dc := int(b[21]) - int(a[21])
	d.DCover = int(float64(dc)/255*area + 0.5*sign(dc))
	d.DN = int(binary.LittleEndian.Uint16(b[22:])) - int(binary.LittleEndian.Uint16(a[22:]))
	d.DLine = int(binary.LittleEndian.Uint16(b[26:])) - int(binary.LittleEndian.Uint16(a[26:]))
	d.Unknown = a[11]&1 != 0 || b[11]&1 != 0
	d.Share = b[8]
	return d
}

func sign(v int) float64 {
	if v < 0 {
		return -1
	}
	return 1
}

var neZeroRow = make([]byte, neLURowBytes)

// deltasOf — all consecutive-version deltas of the KG (computed once per load).
func (c *neChangeKG) deltasOf() (*neChangeDeltas, error) {
	c.deltasMu.Lock() // single flight per KG: the second tile of a burst waits for the first instead of recomputing
	defer c.deltasMu.Unlock()
	if c.deltas != nil {
		return c.deltas, nil
	}
	d := &neChangeDeltas{}
	for _, e := range c.entries {
		if e.Kind == "version" && (d.since == "" || e.At < d.since) {
			d.since = e.At
		}
	}
	ord := c.passOrdinals()
	chunks := make([]uint64, 0, len(c.versions))
	for ch := range c.versions {
		chunks = append(chunks, ch)
	}
	sortU64(chunks)
	for _, ch := range chunks {
		vs := c.versions[ch]
		for i := 0; i+1 < len(vs); i++ {
			ra, err := c.rows(vs[i])
			if err != nil {
				return nil, err
			}
			rb, err := c.rows(vs[i+1])
			if err != nil {
				return nil, err
			}
			confirmed := c.seen[c.entries[vs[i+1]].AP] >= 2
			ma, mb := neRowMap(ra), neRowMap(rb)
			coverage := neVersionsCoverage(ma, mb)
			ids := make([]uint64, 0, len(mb))
			for id := range mb {
				ids = append(ids, id)
			}
			for id := range ma {
				if _, ok := mb[id]; !ok {
					ids = append(ids, id)
				}
			}
			sortU64(ids)
			for _, id := range ids {
				a, okA := ma[id]
				b, okB := mb[id]
				if !okA {
					a = neZeroRow
				}
				if !okB {
					b = neZeroRow
				}
				if bytes.Equal(a[9:], b[9:]) { // kg_share alone is not change
					continue
				}
				area := neCellAreaM2(id)
				x := neRowDelta(a, b, area)
				x.Cell, x.Chunk, x.From, x.To, x.Confirmed, x.Absent, x.Coverage = id, ch, vs[i], vs[i+1], confirmed, !okA || !okB, coverage
				x.At, x.Pass = c.entries[vs[i+1]].At, ord[vs[i+1]]
				if !okB {
					x.Share = a[8]
				}
				if th := c.entries[vs[i+1]].TileHash; th != "" && th == c.entries[vs[i]].TileHash {
					x.SameInput = true
				} else if okA && okB {
					x.Jitter = neDeltaJitter(a, b, &x, area)
				}
				d.rows = append(d.rows, x)
			}
		}
	}
	d.cum = c.cumulate(d.rows, neChangeWin{})
	c.deltas = d
	c.rowsMu.Lock()
	c.ram += len(d.rows)*96 + len(d.cum)*120
	c.rowsMu.Unlock()
	neChangeWriteDerived(c, d)
	return d, nil
}

func neRowMap(rows []byte) map[uint64][]byte {
	m := make(map[uint64][]byte, len(rows)/neLURowBytes)
	for i := 0; i+neLURowBytes <= len(rows); i += neLURowBytes {
		m[binary.LittleEndian.Uint64(rows[i:])] = rows[i : i+neLURowBytes]
	}
	return m
}

// ── derived file + summaries ────────────────────────────────────────────────

// neChangeSummary — the roll-up of one KG (edge cells counted where kg_share ≥ 128).
// neChangeSummarySchema — bump when the summary gains fields; a derived file
// with an older schema is recomputed from the log.
const neChangeSummarySchema = 5

type neChangeSummary struct {
	Schema    int                        `json:"schema"`
	KG        string                     `json:"kg"`
	Since     string                     `json:"since"`
	LogBytes  int64                      `json:"log_bytes"`
	Versions  int                        `json:"versions"`
	Chunks    int                        `json:"chunks"`
	Confirmed neChangeTotals             `json:"confirmed"`
	Candidate neChangeTotals             `json:"candidate"`
	Cells     int                        `json:"cells_changed"`
	ByDay     map[string]*neChangeTotals `json:"by_day,omitempty"`           // confirmed, by the day the new version arrived
	CandByDay map[string]*neChangeTotals `json:"candidate_by_day,omitempty"` // candidate (one sighting), same key
	QC        neChangeQC                 `json:"qc"`
	QCByDay   map[string]*neQCDay        `json:"qc_by_day,omitempty"` // pipeline/jitter/coverage exclusions by the day the new version arrived (counts only)
}

// neQCDay — the two QC exclusions by day (counts only; clustering is a
// whole-log property and has no day).
type neQCDay struct {
	Pipeline int `json:"cells_pipeline"`
	Jitter   int `json:"cells_jitter"`
	Coverage int `json:"cells_coverage,omitempty"`
}

// neChangeQC — quality-control roll-up (docs/ne-qc-proposal.md §1, §4, §5).
type neChangeQC struct {
	Pipeline      int     `json:"cells_pipeline"`  // same tile hash on both sides: pipeline, not land
	Jitter        int     `json:"cells_jitter"`    // below the quantisation floor
	Coverage      int     `json:"cells_coverage"`  // the chunk's cell set differs between the two versions (neVersionsCoverage): fetch/domain, not land
	Clustered     int     `json:"cells_clustered"` // counted cells with ≥ 1 counted neighbour (res-12 ring 1)
	Isolated      int     `json:"cells_isolated"`  // counted cells with none
	ClusterShare  float64 `json:"cluster_share"`   // clustered / (clustered + isolated); real change clusters, noise scatters
	WithTileHash  int     `json:"versions_with_tile_hash"`
	VersionsTotal int     `json:"versions"`
}

type neChangeTotals struct {
	Cells       int            `json:"cells"`
	DLU         [9]int         `json:"d_lu_m2"`
	DCover      int            `json:"d_str_cover_m2"`
	DN          int            `json:"d_str_n"`
	Transitions map[string]int `json:"transitions_m2,omitempty"` // "from>to" by group index
}

var neGroupNames = []string{"structure", "built", "arable", "green", "forest", "water", "road", "alpine", "other"}

// neGroupLabels — human label and tooltip per group for rendered text (HTML,
// summary lines). The JSON keys in neGroupNames stay as they are; only what a
// reader sees changes. The labels match the nine groups named in the public
// NE docs; the tooltips say which declared-use class (Benützungsart) is behind
// each, so "buildings → building land" reads as what it is: land whose declared
// use moved from building to building land, not an outline count.
var neGroupLabels = map[string][2]string{
	"structure": {"buildings", "declared use: building (Benützungsart 41) — the land under a building"},
	"built":     {"building land", "declared use: building land (Benützungsart 42–47, 63, 83, 91) — developed land without a building on it"},
	"arable":    {"arable", "declared use: arable land, cropland"},
	"green":     {"grassland", "declared use: grassland, meadows, gardens"},
	"forest":    {"forest", "declared use: forest"},
	"water":     {"water", "declared use: water bodies"},
	"road":      {"traffic", "declared use: roads, rail, traffic areas"},
	"alpine":    {"alpine", "declared use: alpine land"},
	"other":     {"other", "declared use: other, or several minor classes"},
}

// neGroupLabel — display label for a group key (unknown keys pass through).
func neGroupLabel(k string) string {
	if l, ok := neGroupLabels[k]; ok {
		return l[0]
	}
	return k
}

// neGroupTitle — tooltip for a group key ("" when unknown).
func neGroupTitle(k string) string {
	return neGroupLabels[k][1]
}

// neTransitionLabel — "from>to" key → "buildings → building land"; a bare key
// such as "other" (the roll-up of small transitions) → its label.
func neTransitionLabel(k string) string {
	if from, to, ok := strings.Cut(k, ">"); ok {
		return neGroupLabel(from) + " → " + neGroupLabel(to)
	}
	if k == "other" {
		return "other transitions"
	}
	return neGroupLabel(k)
}

// neTransitionTitle — tooltip for a "from>to" key: both class descriptions.
func neTransitionTitle(k string) string {
	if from, to, ok := strings.Cut(k, ">"); ok {
		return "from " + neGroupTitle(from) + " · to " + neGroupTitle(to)
	}
	if k == "other" {
		return "all smaller transitions together"
	}
	return neGroupTitle(k)
}

// neOutlineTitle — tooltip of the building-outline line, which is a different
// input from the declared-use transitions above it.
const neOutlineTitle = "building outlines, a separate layer from the declared use: count = outlines whose centre lies in the cell, roof = their area"

func (t *neChangeTotals) add(x *neCellDelta) {
	t.Cells++
	if x.Unknown {
		return
	}
	for j := range t.DLU {
		t.DLU[j] += x.DLU[j]
	}
	t.DCover += x.DCover
	t.DN += x.DN
	// greedy pairing of losses with gains inside the cell → "from>to" m²
	losses, gains := []int{}, []int{}
	for j, v := range x.DLU {
		if v < 0 {
			losses = append(losses, j)
		} else if v > 0 {
			gains = append(gains, j)
		}
	}
	sort.Slice(losses, func(a, b int) bool { return x.DLU[losses[a]] < x.DLU[losses[b]] })
	sort.Slice(gains, func(a, b int) bool { return x.DLU[gains[a]] > x.DLU[gains[b]] })
	rem := make([]int, 9)
	copy(rem, x.DLU[:])
	for _, l := range losses {
		for _, g := range gains {
			if rem[l] >= 0 {
				break
			}
			if rem[g] <= 0 {
				continue
			}
			m := min(-rem[l], rem[g])
			rem[l] += m
			rem[g] -= m
			if t.Transitions == nil {
				t.Transitions = map[string]int{}
			}
			t.Transitions[neGroupNames[l]+">"+neGroupNames[g]] += m
		}
	}
}

func (c *neChangeKG) summary(d *neChangeDeltas) neChangeSummary {
	s := neChangeSummary{Schema: neChangeSummarySchema, KG: c.kg, Since: d.since, LogBytes: c.logBytes, Chunks: len(c.versions)}
	for _, e := range c.entries {
		if e.Kind == "version" {
			s.Versions++
		}
	}
	cells := map[uint64]bool{}
	for i := range d.rows {
		x := &d.rows[i]
		if x.Absent || x.Share < 128 {
			continue
		}
		day := c.entries[x.To].At
		if len(day) >= 10 {
			day = day[:10]
		}
		if x.Coverage || x.SameInput || x.Jitter {
			if s.QCByDay == nil {
				s.QCByDay = map[string]*neQCDay{}
			}
			if s.QCByDay[day] == nil {
				s.QCByDay[day] = &neQCDay{}
			}
			if x.Coverage {
				s.QC.Coverage++
				s.QCByDay[day].Coverage++
			} else if x.SameInput {
				s.QC.Pipeline++
				s.QCByDay[day].Pipeline++
			} else {
				s.QC.Jitter++
				s.QCByDay[day].Jitter++
			}
			continue
		}
		cells[x.Cell] = true
		byDay := &s.ByDay
		if x.Confirmed {
			s.Confirmed.add(x)
		} else {
			s.Candidate.add(x)
			byDay = &s.CandByDay
		}
		if *byDay == nil {
			*byDay = map[string]*neChangeTotals{}
		}
		if (*byDay)[day] == nil {
			(*byDay)[day] = &neChangeTotals{}
		}
		(*byDay)[day].add(x)
	}
	s.Cells = len(cells)
	for _, e := range c.entries {
		if e.Kind == "version" {
			s.QC.VersionsTotal++
			if e.TileHash != "" {
				s.QC.WithTileHash++
			}
		}
	}
	for id := range cells {
		hit := false
		for _, n := range neGridDisk(id, 1) {
			if n != id && cells[n] {
				hit = true
				break
			}
		}
		if hit {
			s.QC.Clustered++
		} else {
			s.QC.Isolated++
		}
	}
	if n := s.QC.Clustered + s.QC.Isolated; n > 0 {
		s.QC.ClusterShare = float64(s.QC.Clustered) / float64(n)
	}
	return s
}

func neQCAdd(t, s *neChangeQC) {
	t.Pipeline += s.Pipeline
	t.Jitter += s.Jitter
	t.Coverage += s.Coverage
	t.Clustered += s.Clustered
	t.Isolated += s.Isolated
	t.WithTileHash += s.WithTileHash
	t.VersionsTotal += s.VersionsTotal
	if n := t.Clustered + t.Isolated; n > 0 {
		t.ClusterShare = float64(t.Clustered) / float64(n)
	}
}

// neChangeWriteDerived — {kg}.changes.jsonl: line 1 summary, then one line per
// delta row. A cache of the log, rebuilt whenever the log grew.
func neChangeWriteDerived(c *neChangeKG, d *neChangeDeltas) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	_ = enc.Encode(c.summary(d))
	for i := range d.rows {
		x := &d.rows[i]
		_ = enc.Encode(map[string]interface{}{"cell": neCellHex(x.Cell), "chunk": neCellHex(x.Chunk),
			"from": neVersionRef(c, x.From), "to": neVersionRef(c, x.To), "d_lu_m2": x.DLU, "d_str_cover_m2": x.DCover,
			"d_str_n": x.DN, "d_edge_m_ha": x.DLine, "status": neQCStatus(x), "unknown": x.Unknown, "absent": x.Absent, "kg_share": x.Share, "pass": x.Pass})
	}
	neWriteMu.Lock()
	defer neWriteMu.Unlock()
	tmp := neFilePath(c.kg, neChangeDerived+".tmp")
	if os.WriteFile(tmp, b.Bytes(), 0o644) == nil {
		_ = os.Rename(tmp, neFilePath(c.kg, neChangeDerived))
	}
}

func neVersionRef(c *neChangeKG, i int) map[string]interface{} {
	e := c.entries[i]
	m := map[string]interface{}{"digest_ap": e.AP, "digest": e.Digest, "at": e.At, "observer": e.Observer, "source": e.Source}
	if e.TileHash != "" {
		m["tile_hash"] = e.TileHash
	}
	return m
}

func neStatus(confirmed bool) string {
	if confirmed {
		return "confirmed"
	}
	return "candidate"
}

// neChangeSummaryOf — summary of a KG: from the derived file when it matches
// the log size (no decode), else recomputed.
func neChangeSummaryOf(kg string) (neChangeSummary, bool) {
	st, err := os.Stat(neChangeLogPath(kg))
	if err != nil {
		return neChangeSummary{}, false
	}
	if f, err := os.Open(neFilePath(kg, neChangeDerived)); err == nil {
		var s neChangeSummary
		dec := json.NewDecoder(f)
		err := dec.Decode(&s)
		f.Close()
		if err == nil && s.LogBytes == st.Size() && s.Schema == neChangeSummarySchema {
			return s, true
		}
	}
	c := neChangeGet(kg)
	if c == nil {
		return neChangeSummary{}, false
	}
	d, err := c.deltasOf()
	if err != nil {
		return neChangeSummary{}, false
	}
	return c.summary(d), true
}

// neChangeKGs — KGs with a chunk log.
func neChangeKGs() []string {
	ents, _ := os.ReadDir(neDir)
	var out []string
	for _, e := range ents {
		n := e.Name()
		if len(n) == 5+1+len(neChangeLogKind) && n[5:] == "."+neChangeLogKind {
			out = append(out, n[:5])
		}
	}
	return out
}

// neChangeNational — roll-up over every KG with a log (StatsMemo, 5 min).
func neChangeNational(force bool) map[string]interface{} {
	return StatsMemo("ne_change_national", 5*time.Minute, force, func() map[string]interface{} {
		tot := neChangeSummary{}
		kgsConfirmed, kgs := 0, 0
		for _, kg := range neChangeKGs() {
			s, ok := neChangeSummaryOf(kg)
			if !ok {
				continue
			}
			kgs++
			if tot.Since == "" || (s.Since != "" && s.Since < tot.Since) {
				tot.Since = s.Since
			}
			tot.Versions += s.Versions
			tot.Chunks += s.Chunks
			tot.Cells += s.Cells
			tot.LogBytes += s.LogBytes
			neTotalsAdd(&tot.Confirmed, &s.Confirmed)
			neTotalsAdd(&tot.Candidate, &s.Candidate)
			neQCAdd(&tot.QC, &s.QC)
			if s.Confirmed.Cells > 0 {
				kgsConfirmed++
			}
			for day, t := range s.ByDay {
				if tot.ByDay == nil {
					tot.ByDay = map[string]*neChangeTotals{}
				}
				if tot.ByDay[day] == nil {
					tot.ByDay[day] = &neChangeTotals{}
				}
				neTotalsAdd(tot.ByDay[day], t)
			}
			for day, t := range s.CandByDay {
				if tot.CandByDay == nil {
					tot.CandByDay = map[string]*neChangeTotals{}
				}
				if tot.CandByDay[day] == nil {
					tot.CandByDay[day] = &neChangeTotals{}
				}
				neTotalsAdd(tot.CandByDay[day], t)
			}
			for day, q := range s.QCByDay {
				if tot.QCByDay == nil {
					tot.QCByDay = map[string]*neQCDay{}
				}
				if tot.QCByDay[day] == nil {
					tot.QCByDay[day] = &neQCDay{}
				}
				tot.QCByDay[day].Pipeline += q.Pipeline
				tot.QCByDay[day].Jitter += q.Jitter
				tot.QCByDay[day].Coverage += q.Coverage
			}
		}
		return map[string]interface{}{"since": tot.Since, "kgs_with_log": kgs, "kgs_with_confirmed_change": kgsConfirmed,
			"versions": tot.Versions, "chunks": tot.Chunks, "log_bytes": tot.LogBytes, "cells_changed": tot.Cells,
			"confirmed": tot.Confirmed, "candidate": tot.Candidate, "qc": tot.QC, "by_day": tot.ByDay, "candidate_by_day": tot.CandByDay,
			"qc_by_day": tot.QCByDay}
	})
}

// neChangeText — the landing-page line: "confirmed change since <date>: green → structure 3.4 ha ·
// roof +2.9 ha · structures +212 · in 48 KGs". Empty when no log exists yet; day != "" → that day only.
func neChangeText(day string) string {
	nat := neChangeNational(false)
	kgs, _ := nat["kgs_with_log"].(int)
	if kgs == 0 {
		return ""
	}
	since, _ := nat["since"].(string)
	if len(since) >= 10 {
		since = since[:10]
	}
	var t *neChangeTotals
	if day == "" {
		if c, ok := nat["confirmed"].(neChangeTotals); ok {
			t = &c
		}
	} else if bd, ok := nat["by_day"].(map[string]*neChangeTotals); ok {
		t = bd[day]
	}
	if t == nil || t.Cells == 0 {
		if day != "" {
			return ""
		}
		cand, _ := nat["candidate"].(neChangeTotals)
		if cand.Cells > 0 {
			return fmt.Sprintf("confirmed change since %s: none yet (%s cells candidate, awaiting a second sighting)", since, neFmtInt(cand.Cells))
		}
		return fmt.Sprintf("confirmed change since %s: none yet", since)
	}
	parts := []string{}
	type kv struct {
		k string
		v int
	}
	tr := make([]kv, 0, len(t.Transitions))
	for k, v := range t.Transitions {
		tr = append(tr, kv{k, v})
	}
	sort.Slice(tr, func(a, b int) bool { return tr[a].v > tr[b].v || (tr[a].v == tr[b].v && tr[a].k < tr[b].k) })
	for i, x := range tr {
		if i == 3 {
			break
		}
		parts = append(parts, fmt.Sprintf("%s %s", neTransitionLabel(x.k), neFmtArea(x.v)))
	}
	parts = append(parts, "roof "+neFmtAreaSigned(t.DCover), fmt.Sprintf("building outlines %+d", t.DN))
	head := "confirmed change since " + since
	if day != "" {
		head = "confirmed change"
	} else if n, _ := nat["kgs_with_confirmed_change"].(int); n > 0 {
		parts = append(parts, fmt.Sprintf("in %d KGs", n))
	}
	return head + ": " + strings.Join(parts, " · ")
}

// neLUMoved — m² of land use that changed group inside the cells (the sum of
// the paired from→to transitions); the "land-use change" figure of a day.
func (t *neChangeTotals) neLUMoved() int {
	n := 0
	for _, v := range t.Transitions {
		n += v
	}
	return n
}

func neTotalsAdd(t, s *neChangeTotals) {
	t.Cells += s.Cells
	for j := range t.DLU {
		t.DLU[j] += s.DLU[j]
	}
	t.DCover += s.DCover
	t.DN += s.DN
	for k, v := range s.Transitions {
		if t.Transitions == nil {
			t.Transitions = map[string]int{}
		}
		t.Transitions[k] += v
	}
}

// neCellChange — the public `chg` block of one res-12 cell (nil = unchanged).
func neCellChange(id uint64, kgs map[string]uint8) map[string]interface{} {
	return neCellChangeWin(id, kgs, neChangeWin{})
}

// neCellChangeWin — the same, restricted to the deltas inside a pass / date window (ne_change_passes.go).
func neCellChangeWin(id uint64, kgs map[string]uint8, win neChangeWin) map[string]interface{} {
	bestKG, bestShare := "", -1
	for kg, sh := range kgs {
		if int(sh) > bestShare || (int(sh) == bestShare && kg < bestKG) {
			bestKG, bestShare = kg, int(sh)
		}
	}
	for _, kg := range neOrderedKGs(kgs, bestKG) {
		c := neChangeGet(kg)
		if c == nil {
			continue
		}
		d, err := c.deltasOf()
		if err != nil {
			continue
		}
		cu := c.cumWin(d, win)[id]
		if cu == nil {
			return nil
		}
		out := map[string]interface{}{"since": d.since, "status": "candidate", "from": cu.FromAP, "first_seen": cu.FirstAt, "last_seen": cu.AnyAt}
		if cu.Confirmed {
			out["status"] = "confirmed"
			out["to"], out["last_change"] = cu.ToAP, cu.LastAt
			if !cu.Unknown {
				out["d_lu_m2"], out["d_str_cover_m2"] = cu.DLU, cu.DCover
			}
			out["d_str_n"], out["d_edge_m_ha"] = cu.DN, cu.DLine
			if cu.Candidate {
				out["candidate_pending"] = true
			}
		}
		out["unknown"] = cu.Unknown
		if cu.Pass > 0 {
			out["pass"] = cu.Pass
			if cu.PassLast > cu.Pass {
				out["pass_last"] = cu.PassLast
			}
		}
		return out
	}
	return nil
}

// neCellCluster — the run of touching res-12 cells with change (candidate or
// confirmed) around id: the reader tapped one hexagon, the land that moved is
// usually a field or a stand of trees spanning dozens. Flood fill over ring-1
// neighbours through the change logs of the cell's KGs, capped at
// neClusterCap cells (a cap hit is flagged `capped`). Figures: cells,
// confirmed / candidate counts, area (m²), Δ declared use by group summed over
// confirmed deltas (`d_lu_m2`) and over the still-pending ones (`d_lu_pending_m2`),
// structures Δ, and the sighting window `first` … `last` (digest timestamps),
// so a card can say "4.3 ha alpine → forest · Oct 2026". nil when the cell has
// no change itself.
const neClusterCap = 6000

func neCellCluster(id uint64, kgs map[string]uint8) map[string]interface{} {
	return neCellClusterWin(id, kgs, neChangeWin{})
}

func neCellClusterWin(id uint64, kgs map[string]uint8, win neChangeWin) map[string]interface{} {
	var cums []map[uint64]*neCellCum
	for _, kg := range neOrderedKGs(kgs, "") {
		c := neChangeGet(kg)
		if c == nil {
			continue
		}
		if d, err := c.deltasOf(); err == nil && d != nil {
			cums = append(cums, c.cumWin(d, win))
		}
	}
	look := func(n uint64) *neCellCum {
		for _, m := range cums {
			if cu := m[n]; cu != nil && (cu.Confirmed || cu.Candidate) {
				return cu
			}
		}
		return nil
	}
	if look(id) == nil {
		return nil
	}
	seen := map[uint64]bool{id: true}
	queue := []uint64{id}
	var dlu, pend [9]int
	cells, conf, cand, dn := 0, 0, 0, 0
	area := 0.0
	first, last := "", ""
	capped := false
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		cu := look(n)
		cells++
		area += neCellAreaM2(n)
		if cu.Confirmed {
			conf++
			dn += cu.DN
			if !cu.Unknown {
				for j := range dlu {
					dlu[j] += cu.DLU[j]
				}
			}
		} else {
			cand++
		}
		if cu.Candidate && !cu.Unknown {
			for j := range pend {
				pend[j] += cu.CandDLU[j]
			}
		}
		if cu.FirstAt != "" && (first == "" || cu.FirstAt < first) {
			first = cu.FirstAt
		}
		if cu.AnyAt > last {
			last = cu.AnyAt
		}
		if cells >= neClusterCap {
			capped = true
			break
		}
		for _, nb := range neGridDisk(n, 1) {
			if !seen[nb] && look(nb) != nil {
				seen[nb] = true
				queue = append(queue, nb)
			}
		}
	}
	return map[string]interface{}{"cells": cells, "confirmed": conf, "candidate": cand, "area_m2": int(area + 0.5),
		"d_lu_m2": dlu, "d_lu_pending_m2": pend, "d_str_n": dn, "first": first, "last": last, "capped": capped}
}

func neOrderedKGs(kgs map[string]uint8, first string) []string {
	out := []string{}
	if first != "" {
		out = append(out, first)
	}
	rest := []string{}
	for kg := range kgs {
		if kg != first {
			rest = append(rest, kg)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// neFmtArea — human area with an auto-picked unit: m² below 1 ha (so small
// confirmed changes never read "0.0 ha"), ha up to 100 ha, km² above.
func neFmtArea(m2 int) string {
	a := neAbs(m2)
	switch {
	case a == 0:
		return "0 m²"
	case a < 1e4:
		return strconv.Itoa(m2) + " m²"
	case a < 1e6:
		return strconv.FormatFloat(float64(m2)/1e4, 'f', 1, 64) + " ha"
	default:
		return strconv.FormatFloat(float64(m2)/1e6, 'f', 2, 64) + " km²"
	}
}

func neFmtAreaSigned(m2 int) string {
	if m2 > 0 {
		return "+" + neFmtArea(m2)
	}
	return neFmtArea(m2)
}
