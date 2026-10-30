package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ═══════════════════════════════════════════════════════════════════════
// GET <private>/api/v1/ne/manifest — one document for the whole country
// ═══════════════════════════════════════════════════════════════════════
//
// srtm's director plans upgrades from this instead of 8 440 GETs:
//   {kgs: {kg: {lu:{digest,epoch,…}|null, bev:{…}|null, obs:{…}|null,
//               updated_at, status, queue_position?, retry_after_s?}}, queue:{…}, frozen:{…}}
// Built from the section HEADERS of data/ne_cells/{kg}.nec|.bev.nec|.obs
// (no inflate — digests were verified when the files were written/posted).
//
// Memory contract (AGENTS.md 5): one entry per file in data/ne_cells (≤ 3 per
// KG, ≤ ~24 000), rescanned at most every neManifestTTL; size line on
// /api/v1/debug/inflight ("ne_manifest"). ETag = sha256 of the body, so the
// director's If-None-Match costs one stat sweep, no JSON.
//
// Queue: scripts/migrate_local/ne_all.sh writes data/ne_cells/queue.json after
// every KG ({next_index, total, avg_seconds, failed[], finished}); the build
// order is the sorted list of local KG codes — or data/ne_cells/queue_order.txt when
// queue.json names it (priority runs) — so an unbuilt KG's position —
// and the Retry-After srtm asked for on the 404 — is index(kg) − next_index.

const neManifestTTL = 30 * time.Second

// neManifestDeltaRetention bounds `?since=`: older cursors get the full
// manifest (delta:false). updated_at comes from file mtimes / layer built_at,
// which survive restarts, but a delta cannot express a KG whose files were
// REMOVED (built → queued) and a week-old cursor would select most of a
// rebuild pass anyway — the full body is the cheaper answer then.
const neManifestDeltaRetention = 7 * 24 * time.Hour

type neLayerInfo struct {
	Digest  string `json:"digest"`
	Epoch   string `json:"epoch"`
	Algo    string `json:"algo"`
	Source  string `json:"source"`
	CellsN  int    `json:"cells_n"`
	Bytes   int64  `json:"bytes"`
	BuiltAt string `json:"built_at"`
	// NEO1 only: row layout as declared by the peer's header (record_bytes + fields[]).
	RecordBytes int      `json:"record_bytes,omitempty"`
	Fields      []string `json:"fields,omitempty"`
	// NEO1 only: the declared layer the cell set was validated against on POST
	// (lu = index epoch, or the peer's own NEC1 stored as `bev`). Compare
	// obs.declared_digest with bev.digest / lu.digest instead of cells_n.
	DeclaredSource string `json:"declared_source,omitempty"`
	DeclaredDigest string `json:"declared_digest,omitempty"`
}

// neDeclaredInfo — what the declared VIEW of a KG reads today
// (ne_change_declared.go, docs/ne-change.md §4b): source = chunks@<state> when
// the chunk log selects rows, else the bev / lu source; coverage = share of the
// KG's cells the log supplies (selected versions' rows / declared cells).
type neDeclaredInfo struct {
	Source   string  `json:"source"`
	State    string  `json:"state,omitempty"`
	NewestAt string  `json:"newest_at,omitempty"`
	Coverage float64 `json:"coverage"`
	Warming  bool    `json:"warming,omitempty"` // state not memoised yet (startup) — source says "chunks" without a state
}

type neKGEntry struct {
	LU         *neLayerInfo    `json:"lu"`
	Bev        *neLayerInfo    `json:"bev"`
	Obs        *neLayerInfo    `json:"obs"`
	Declared   *neDeclaredInfo `json:"declared,omitempty"`
	UpdatedAt  string          `json:"updated_at,omitempty"`
	Status     string          `json:"status"` // built | queued | failed | stalled | not_in_index
	QueuePos   int             `json:"queue_position,omitempty"`
	RetryAfter int             `json:"retry_after_s,omitempty"`
}

type neQueue struct {
	UpdatedAt       string   `json:"updated_at"`
	Total           int      `json:"total"`
	NextIndex       int      `json:"next_index"`
	Current         string   `json:"current"` // comma-separated when WORKERS > 1
	Workers         int      `json:"workers,omitempty"`
	Done            int      `json:"done"`
	AvgSeconds      float64  `json:"avg_seconds"` // wall seconds per finished KG (throughput, all workers)
	AvgBuildSeconds *float64 `json:"avg_build_seconds,omitempty"`
	Failed          []string `json:"failed"`
	Finished        bool     `json:"finished"`
	OrderFile       string   `json:"order_file,omitempty"` // relative to neDir; the builder's actual order when not sorted
}

type neFileEntry struct {
	mod   time.Time
	size  int64
	infos map[string]*neLayerInfo // layer (lu|bev|obs) → header summary
}

type neManifestCache struct {
	mu        sync.Mutex
	scannedAt time.Time
	files     map[string]neFileEntry // file name → header summary
	kgs       []string               // sorted local KG codes (build order)
	kgIndex   map[string]int
	queue     neQueue
	body      []byte
	etag      string
	entries   map[string]*neKGEntry
	// snapshot of the last rebuild for `?since=` deltas (handleNEManifest)
	built       int
	obs         int
	obsOnBev    int
	obsOnIndex  int
	logBytes    int64 // Σ chunk-log sizes at the last scan (append-only → grows when any log did)
	generatedAt string
}

var neManifest = &neManifestCache{files: map[string]neFileEntry{}}

// neReadHeader reads magic + header of the FIRST section of a container file
// without inflating the body.
func neReadHeader(path string) (string, map[string]interface{}, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	var pre [9]byte
	if _, err := f.Read(pre[:]); err != nil {
		return "", nil, err
	}
	if pre[4] != 1 {
		return "", nil, fmt.Errorf("fmt_ver %d", pre[4])
	}
	hl := binary.LittleEndian.Uint32(pre[5:])
	if hl > 4<<20 {
		return "", nil, fmt.Errorf("header too large")
	}
	hj := make([]byte, hl)
	if _, err := f.Read(hj); err != nil {
		return "", nil, err
	}
	var hdr map[string]interface{}
	if err := json.Unmarshal(hj, &hdr); err != nil {
		return "", nil, err
	}
	return string(pre[:4]), hdr, nil
}

func neLayerInfoFromHeader(h map[string]interface{}, size int64, mod time.Time) *neLayerInfo {
	s := func(k string) string { v, _ := h[k].(string); return v }
	n, _ := h["cells_n"].(float64)
	li := &neLayerInfo{Digest: s("digest"), Epoch: s("epoch"), Algo: s("algo"), Source: s("source"),
		CellsN: int(n), Bytes: size, BuiltAt: mod.UTC().Format(time.RFC3339)}
	if rb, ok := h["record_bytes"].(float64); ok {
		li.RecordBytes = int(rb)
	}
	li.DeclaredSource, li.DeclaredDigest = s("declared_source"), s("declared_digest")
	if fs, ok := h["fields"].([]interface{}); ok {
		for _, f := range fs {
			if str, ok := f.(string); ok {
				li.Fields = append(li.Fields, str)
			}
		}
	}
	return li
}

func neQueuePath() string { return filepath.Join(neDir, "queue.json") }

// refresh rescans data/ne_cells if the TTL elapsed. Caller holds no lock.
func (m *neManifestCache) refresh(force bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !force && time.Since(m.scannedAt) < neManifestTTL {
		return
	}
	m.scannedAt = time.Now()
	ents, _ := os.ReadDir(neDir)
	seen := make(map[string]bool, len(ents))
	changed := false
	var logBytes int64
	for _, e := range ents {
		name := e.Name()
		kind := ""
		switch {
		case strings.HasSuffix(name, ".bev.nec"), strings.HasSuffix(name, ".nec"), strings.HasSuffix(name, ".obs"), strings.HasSuffix(name, ".nex"):
			kind = name
		case strings.HasSuffix(name, "."+neChangeLogKind):
			kind = "log"
		}
		if kind == "" {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if kind == "log" { // chunk logs move the per-KG `declared` block: their total size is part of the change check
			logBytes += fi.Size()
			continue
		}
		seen[name] = true
		if old, ok := m.files[name]; ok && old.mod.Equal(fi.ModTime()) && old.size == fi.Size() {
			continue
		}
		if strings.HasSuffix(name, ".nex") {
			// the store: one file, up to three layers (ne_store.go)
			k := neStore.get(strings.TrimSuffix(name, ".nex"))
			if k == nil || !k.Mod.Equal(fi.ModTime()) {
				if k, err = neReadNexHeader(filepath.Join(neDir, name)); err != nil {
					continue
				}
			}
			infos := map[string]*neLayerInfo{}
			for layer, l := range k.Hdr.Layers {
				t, _ := time.Parse(time.RFC3339, l.BuiltAt)
				infos[layer] = neLayerInfoFromHeader(k.layerHeader(layer), l.Bytes, t)
			}
			m.files[name] = neFileEntry{mod: fi.ModTime(), size: fi.Size(), infos: infos}
			changed = true
			continue
		}
		_, hdr, err := neReadHeader(filepath.Join(neDir, name))
		if err != nil {
			continue
		}
		layer := "lu"
		switch {
		case strings.HasSuffix(name, ".bev.nec"):
			layer = "bev"
		case strings.HasSuffix(name, ".obs"):
			layer = "obs"
		}
		m.files[name] = neFileEntry{mod: fi.ModTime(), size: fi.Size(), infos: map[string]*neLayerInfo{layer: neLayerInfoFromHeader(hdr, fi.Size(), fi.ModTime())}}
		changed = true
	}
	for name := range m.files {
		if !seen[name] {
			delete(m.files, name)
			changed = true
		}
	}
	if logBytes != m.logBytes {
		m.logBytes = logBytes
		changed = true
	}
	// build order = sorted local KG codes (or queue.order_file, see below)
	kgs := KGUniverseCodes()
	sort.Strings(kgs)
	if len(kgs) != len(m.kgs) {
		changed = true
	}
	m.kgs = kgs
	m.kgIndex = make(map[string]int, len(kgs))
	for i, k := range kgs {
		m.kgIndex[k] = i
	}
	var q neQueue
	if b, err := os.ReadFile(neQueuePath()); err == nil {
		_ = json.Unmarshal(b, &q)
	}
	if q.UpdatedAt != m.queue.UpdatedAt {
		changed = true
	}
	m.queue = q
	// The builder may run a priority order (srtm's v2+ KGs first): queue positions must follow
	// the published order, not the sorted list. Only codes present in the index count.
	if q.OrderFile != "" && !strings.Contains(q.OrderFile, "/") {
		if b, err := os.ReadFile(filepath.Join(neDir, q.OrderFile)); err == nil {
			idx := make(map[string]int, len(kgs))
			i := 0
			for _, line := range strings.Split(string(b), "\n") {
				k := strings.TrimSpace(line)
				if _, ok := m.kgIndex[k]; !ok {
					continue
				}
				if _, dup := idx[k]; dup {
					continue
				}
				idx[k] = i
				i++
			}
			if i == len(kgs) {
				m.kgIndex = idx
			}
		}
	}
	if changed || m.body == nil {
		m.rebuildLocked()
	}
}

// queueStatusLocked classifies an unbuilt KG against the running build.
func (m *neManifestCache) queueStatusLocked(kg string) (status string, pos, retry int) {
	idx, inIndex := m.kgIndex[kg]
	if !inIndex {
		return "not_in_index", 0, 0
	}
	for _, f := range m.queue.Failed {
		if f == kg {
			return "failed", 0, 0
		}
	}
	q := m.queue
	if q.UpdatedAt == "" || q.Finished {
		return "queued", 0, 3600
	}
	avg := q.AvgSeconds
	if avg <= 0 {
		avg = 50
	}
	pos = idx - q.NextIndex
	if pos < 0 {
		pos = 0 // the builder passed it (failed and not yet recorded, or being written now)
	}
	retry = int(float64(pos+1) * avg)
	if retry < 30 {
		retry = 30
	}
	if t, err := time.Parse(time.RFC3339, q.UpdatedAt); err == nil && time.Since(t) > time.Hour {
		return "stalled", pos, 3600
	}
	return "queued", pos, retry
}

func (m *neManifestCache) rebuildLocked() {
	entries := make(map[string]*neKGEntry, len(m.kgs)+64)
	get := func(kg string) *neKGEntry {
		e := entries[kg]
		if e == nil {
			e = &neKGEntry{}
			entries[kg] = e
		}
		return e
	}
	for name, f := range m.files {
		kg := name[:strings.IndexByte(name, '.')]
		e := get(kg)
		for layer, info := range f.infos {
			switch layer {
			case "bev":
				e.Bev = info
			case "lu":
				e.LU = info
			case "obs":
				e.Obs = info
			}
		}
	}
	for _, kg := range m.kgs {
		get(kg)
	}
	built, obs, obsOnBev, obsOnIndex := 0, 0, 0, 0
	luSource, _ := neFrozen["lu_source"].(string)
	for kg, e := range entries {
		e.Declared = neDeclaredInfoOf(kg, e)
		latest := ""
		for _, l := range []*neLayerInfo{e.LU, e.Bev, e.Obs} {
			if l != nil && l.BuiltAt > latest {
				latest = l.BuiltAt
			}
		}
		e.UpdatedAt = latest
		if e.Obs != nil {
			obs++
			if e.Bev != nil && e.Obs.DeclaredDigest != "" && e.Obs.DeclaredDigest == e.Bev.Digest {
				obsOnBev++
			}
			if e.Obs.DeclaredSource == luSource || e.Obs.DeclaredSource == "" {
				obsOnIndex++ // bare NEO1 validated against the index epoch's cell set (api_ne.go handleNEObsPost)
			}
		}
		if e.LU != nil {
			e.Status = "built"
			built++
			continue
		}
		e.Status, e.QueuePos, e.RetryAfter = m.queueStatusLocked(kg)
	}
	m.entries = entries
	m.built, m.obs, m.obsOnBev, m.obsOnIndex = built, obs, obsOnBev, obsOnIndex
	m.generatedAt = time.Now().UTC().Format(time.RFC3339)
	doc := map[string]interface{}{
		"product":          "NE cells manifest — one entry per KG (lu = index epoch, bev = bevdirect baseline, obs = posted NEO1)",
		"frozen":           neFrozen,
		"kgs_total":        len(m.kgs),
		"built":            built,
		"obs_posted":       obs,
		"kgs_obs_on_bev":   obsOnBev,   // obs validated against the peer's own NEC1 (`bev`), not lu
		"kgs_obs_on_index": obsOnIndex, // obs still validated against the index epoch's cell set — re-post on GET /{kg}/declared (revisit.obs_on_index)
		"declared_rule":    "per KG `declared` = what the declared view reads: chunk-log view (latest confirmed version per chunk, else baseline) → bev → lu; coverage = share of cells the log supplies",
		"queue":            m.queue,
		"kgs":              entries,
		"generated_at":     m.generatedAt,
		"delta":            false, // ?since= answers delta:true with only the changed kgs (handleNEManifest)
		"obs_public":       neObsPublic,
		"notice":           NENotice(),
	}
	b, _ := json.Marshal(doc)
	m.body = b
	// ETag over the content that matters (not generated_at): the entries + queue
	sum := sha256.New()
	eb, _ := json.Marshal(entries)
	sum.Write(eb)
	qb, _ := json.Marshal(m.queue)
	sum.Write(qb)
	m.etag = `"nem-` + hex.EncodeToString(sum.Sum(nil)[:8]) + `"`
}

// neFrozen is the contract pin for srtm's v2.4 run (docs/ne-cells.md §freeze).
var neFrozen = map[string]interface{}{
	"algo":                "ne-cells-2",
	"ns_table":            "ns-groups-1",
	"lu_source":           "index-2026-03",
	"lu_epoch":            "2026-03",
	"from":                "2026-10-04",
	"until":               "2027-04-30",
	"rule":                "a KG's lu digest never changes under the same (algo, source, epoch); a rebuild gets a new epoch and shows up here",
	"bev_source":          "bevdirect@v0.2.0",
	"bev_equivalent_tags": []string{"bevdirect@v0.2.1", "bevdirect@v0.3.0", "bevdirect@v0.3.1", "bevdirect@v0.3.2", "bevdirect@v0.3.3", "bevdirect@v0.3.4", "bevdirect@v0.3.5", "bevdirect@v0.3.6"},
}

// neDeclaredInfoOf — the declared block of a manifest entry from the memoised
// view state (no log read on the manifest path; neDeclWarm fills the memo).
func neDeclaredInfoOf(kg string, e *neKGEntry) *neDeclaredInfo {
	cells := 0
	src := ""
	if e.LU != nil {
		cells, src = e.LU.CellsN, e.LU.Source
	}
	if e.Bev != nil {
		cells, src = e.Bev.CellsN, e.Bev.Source
	}
	d, fresh := neDeclStatePeek(kg)
	if d != nil && !fresh { // memoised but the log grew since: one log read
		d, fresh = neDeclStateOf(kg), true
	}
	if d == nil && fresh { // no chunk log
		if src == "" {
			return nil
		}
		return &neDeclaredInfo{Source: src}
	}
	if d == nil || !fresh {
		return &neDeclaredInfo{Source: "chunks", Warming: true}
	}
	if d.state == "" {
		if src == "" {
			return nil
		}
		return &neDeclaredInfo{Source: src}
	}
	cov := 1.0
	if cells > 0 {
		cov = min(1, float64(d.logCells)/float64(cells))
	}
	return &neDeclaredInfo{Source: "chunks@" + d.state, State: d.state, NewestAt: d.newest, Coverage: float64(int(cov*1000)) / 1000}
}

// neEntry returns the manifest entry of one KG (refreshing if stale).
func neEntry(kg string) neKGEntry {
	neManifest.refresh(false)
	neManifest.mu.Lock()
	defer neManifest.mu.Unlock()
	if e := neManifest.entries[kg]; e != nil {
		return *e
	}
	st, pos, retry := neManifest.queueStatusLocked(kg)
	return neKGEntry{Status: st, QueuePos: pos, RetryAfter: retry}
}

func neManifestStats() map[string]interface{} {
	neManifest.mu.Lock()
	defer neManifest.mu.Unlock()
	return map[string]interface{}{
		"files": len(neManifest.files), "kgs": len(neManifest.entries), "body_bytes": len(neManifest.body),
		"scanned_at": neManifest.scannedAt.UTC().Format(time.RFC3339), "ttl_s": int(neManifestTTL.Seconds()),
		"queue": neManifest.queue,
	}
}

func handleNEManifest(w http.ResponseWriter, r *http.Request) {
	neManifest.refresh(r.URL.Query().Get("refresh") == "1")
	if since := strings.TrimSpace(r.URL.Query().Get("since")); since != "" {
		t, err := time.Parse(time.RFC3339Nano, since)
		if err != nil {
			apiError(w, "since: RFC 3339 timestamp expected (use generated_at of your last manifest), got "+strconv.Quote(since), 400)
			return
		}
		if body, etag, ok := neManifest.delta(t); ok {
			w.Header().Set("Cache-Control", "public, max-age=60")
			if neNotModified(w, r, etag) {
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = w.Write(body)
			return
		}
		// cursor too old / unknown → full manifest below (delta:false in the body)
	}
	neManifest.mu.Lock()
	body, etag := neManifest.body, neManifest.etag
	neManifest.mu.Unlock()
	w.Header().Set("Cache-Control", "public, max-age=60")
	if strings.Contains(r.Header.Get("If-None-Match"), etag) {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", etag) // handler-managed: cacheMiddleware streams, gzipMiddleware compresses
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, _ = w.Write(body)
}

// delta renders `?since=` — the kgs whose updated_at ≥ since (second
// resolution, INCLUSIVE: a file written in the same second as the previous
// generated_at but after that ReadDir would otherwise be lost; the client may
// re-see a boundary entry, merging is idempotent) plus the counters. ok=false
// when the cursor is older than neManifestDeltaRetention or later than the
// current generated_at (a cursor from another server / clock) → caller sends
// the full body. Entries whose files vanished (updated_at "") never appear in
// a delta; the retention bound keeps that window short.
//
// Per request: one pass over ≤ 7 850 entries + a small marshal (srtm polls
// every 10 min). entries/queue are replaced wholesale by rebuildLocked, never
// mutated in place, so the pass runs outside the lock.
func (m *neManifestCache) delta(since time.Time) (body []byte, etag string, ok bool) {
	m.mu.Lock()
	entries, built, obs, obsOnBev, total, generatedAt := m.entries, m.built, m.obs, m.obsOnBev, len(m.kgs), m.generatedAt
	m.mu.Unlock()
	if generatedAt == "" || time.Since(since) > neManifestDeltaRetention {
		return nil, "", false
	}
	sinceStr := since.UTC().Truncate(time.Second).Format(time.RFC3339)
	if sinceStr > generatedAt {
		return nil, "", false
	}
	changed := make(map[string]*neKGEntry, 64)
	for kg, e := range entries {
		if e.UpdatedAt != "" && e.UpdatedAt >= sinceStr {
			changed[kg] = e
		}
	}
	doc := map[string]interface{}{
		"delta":          true,
		"since":          sinceStr,
		"generated_at":   generatedAt,
		"kgs_total":      total,
		"built":          built,
		"obs_posted":     obs,
		"kgs_obs_on_bev": obsOnBev,
		"kgs_changed":    len(changed),
		"kgs":            changed,
		"retention_s":    int(neManifestDeltaRetention.Seconds()),
	}
	body, _ = json.Marshal(doc)
	// ETag over what the client merges (changed entries + counters + cursor), not generated_at
	sum := sha256.New()
	eb, _ := json.Marshal(changed)
	sum.Write(eb)
	fmt.Fprintf(sum, "|%s|%d|%d|%d", sinceStr, total, built, obs)
	return body, `"nem-` + hex.EncodeToString(sum.Sum(nil)[:8]) + `-s"`, true
}
