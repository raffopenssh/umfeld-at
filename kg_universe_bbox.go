// kg_universe_bbox.go — KG bbox = envelope of the BEV VGD Katastralgemeinde
// polygon (bbox revision 2, 2026-10-27) + the public sliver diff
// GET /api/v1/kgs/bbox_changes.
//
// Why
// ───
// Until revision 2 the per-KG bbox in /api/v1/kgs came from the (since
// deleted) parcel index: the envelope of the parcels we had assembled, not of
// the Katastralgemeinde. For most KGs that envelope is narrower than the
// administrative polygon (63308 Gallmannsegg: 1.2 km strip missing on the W).
// {OBS_NAME} plans its observed-layer rasterisation from this bbox, so the
// strips were never rasterised and show as "not classified yet" while the
// declared layer (NEC1 from the VGD polygon, same source bevdirect embeds)
// covers them.
//
// Fix (backward compatible, nothing shrinks)
// ──────────────────────────────────────────
//
//	bbox_new = union(bbox_old, VGD envelope padded by kgVGDBBoxPad outward)
//
// data/bev_vgd_admin.json.gz is the table bevdirect embeds (BEV
// Verwaltungsgrenzen VGD 1:50 000, CC BY 4.0; 4-decimal rounded envelopes —
// the pad undoes the nearest-rounding so the envelope contains the polygon).
// The union guarantees every already-processed bbox stays inside the new one:
// a peer only has to process the SLIVERS (bbox_new − bbox_old, ≤ 4 rectangles)
// to close its gaps. kg_count and universe_hash are untouched (the SET of
// codes did not change); generated_at moves, data.bbox_revision = 2, the ETag
// gets a "-b2" suffix so cached copies revalidate, and the kg table's
// min/max columns are updated to the same values so point/bbox lookups
// (kg_bbox_index.go) agree with the published list.
//
// `-kg-universe-build` applies the overlay, writes the new file and
// data/kg_universe_bbox_diff.json.gz (old bbox, new bbox, slivers, max delta
// per changed KG). The diff is served, tokenless, as
//
//	GET /api/v1/kgs/bbox_changes[?min_delta=0.001][&codes=…][&fields=codes]
//
// so peers can queue sliver repairs for KGs they already processed.
package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"
)

const (
	kgVGDBBoxPath          = "data/bev_vgd_admin.json.gz"
	kgUniverseBBoxDiffPath = "data/kg_universe_bbox_diff.json.gz"
	kgVGDBBoxPad           = 0.0001 // deg; VGD envelopes are rounded to 4 decimals (nearest), pad outward
	kgUniverseBBoxRevision = 2
)

// kgVGDTable — the bevdirect admin table (subset of fields we use).
type kgVGDTable struct {
	Source string `json:"source"`
	KGs    []struct {
		KGCode string  `json:"kg_code"`
		MinLon float64 `json:"min_lon"`
		MinLat float64 `json:"min_lat"`
		MaxLon float64 `json:"max_lon"`
		MaxLat float64 `json:"max_lat"`
	} `json:"kgs"`
}

// LoadKGVGDBBoxes reads data/bev_vgd_admin.json.gz → kg_code → envelope (unpadded).
func LoadKGVGDBBoxes(path string) (map[string][4]float64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, "", err
	}
	defer zr.Close()
	var t kgVGDTable
	if err := json.NewDecoder(zr).Decode(&t); err != nil {
		return nil, "", err
	}
	out := make(map[string][4]float64, len(t.KGs))
	for _, k := range t.KGs {
		c := padKGCode(k.KGCode)
		if c == "" || k.MinLon >= k.MaxLon || k.MinLat >= k.MaxLat {
			continue
		}
		out[c] = [4]float64{k.MinLon, k.MinLat, k.MaxLon, k.MaxLat}
	}
	if len(out) == 0 {
		return nil, "", fmt.Errorf("%s: no usable rows", path)
	}
	return out, t.Source, nil
}

// bboxUnion — the envelope of both boxes; a zero box contributes nothing.
func bboxUnion(a, b [4]float64) [4]float64 {
	if a == [4]float64{} {
		return b
	}
	if b == [4]float64{} {
		return a
	}
	return [4]float64{math.Min(a[0], b[0]), math.Min(a[1], b[1]), math.Max(a[2], b[2]), math.Max(a[3], b[3])}
}

// bboxContains — a ⊇ b (closed).
func bboxContains(a, b [4]float64) bool {
	return a[0] <= b[0] && a[1] <= b[1] && a[2] >= b[2] && a[3] >= b[3]
}

// ApplyKGVGDBBoxes widens every row to union(bbox, VGD envelope + pad) and
// recomputes the centroid. Returns the number of rows that changed. Rows
// without a VGD entry keep their bbox (logged by the caller via missing).
func ApplyKGVGDBBoxes(u *kgUniverse, vgd map[string][4]float64, pad float64) (changed int, missing []string) {
	for i := range u.KGs {
		r := &u.KGs[i]
		v, ok := vgd[r.KGCode]
		if !ok {
			missing = append(missing, r.KGCode)
			continue
		}
		v = [4]float64{round7(v[0] - pad), round7(v[1] - pad), round7(v[2] + pad), round7(v[3] + pad)}
		nb := bboxUnion(r.BBox, v)
		if nb != r.BBox {
			changed++
			r.BBox = nb
		}
		r.Centroid = [2]float64{round7((nb[0] + nb[2]) / 2), round7((nb[1] + nb[3]) / 2)}
	}
	u.BBoxRevision = kgUniverseBBoxRevision
	return changed, missing
}

// kgBBoxSlivers — new − old as ≤ 4 non-overlapping rectangles
// [min_lon,min_lat,max_lon,max_lat]: W and E strips over the full new height,
// S and N strips over the old width. Requires new ⊇ old; an old zero box
// yields the whole new box.
func kgBBoxSlivers(old, nb [4]float64) [][4]float64 {
	if old == [4]float64{} || !bboxContains(nb, old) {
		return [][4]float64{nb}
	}
	var out [][4]float64
	if nb[0] < old[0] {
		out = append(out, [4]float64{nb[0], nb[1], old[0], nb[3]})
	}
	if nb[2] > old[2] {
		out = append(out, [4]float64{old[2], nb[1], nb[2], nb[3]})
	}
	if nb[1] < old[1] {
		out = append(out, [4]float64{old[0], nb[1], old[2], old[1]})
	}
	if nb[3] > old[3] {
		out = append(out, [4]float64{old[0], old[3], old[2], nb[3]})
	}
	return out
}

// kgBBoxChange — one row of the diff.
type kgBBoxChange struct {
	KGCode   string       `json:"kg_code"`
	BBoxOld  [4]float64   `json:"bbox_old"`
	BBoxNew  [4]float64   `json:"bbox_new"`
	MaxDelta float64      `json:"max_delta_deg"` // largest outward move of any edge
	Slivers  [][4]float64 `json:"slivers"`       // bbox_new − bbox_old, ≤ 4 rectangles
}

// kgBBoxDiff — data/kg_universe_bbox_diff.json.gz.
type kgBBoxDiff struct {
	Schema          string         `json:"schema"`
	BBoxRevision    int            `json:"bbox_revision"`
	BBoxSource      string         `json:"bbox_source"`
	PadDeg          float64        `json:"pad_deg"`
	UniverseHash    string         `json:"universe_hash"`
	FromGeneratedAt string         `json:"from_generated_at"`
	ToGeneratedAt   string         `json:"to_generated_at"`
	Rule            string         `json:"rule"`
	KGCount         int            `json:"kg_count"`
	ChangedCount    int            `json:"changed_count"`
	Changed         []kgBBoxChange `json:"changed"`
}

// BuildKGBBoxDiff — per-KG diff prev → next (same code set assumed; codes
// only in next are reported with a zero old bbox).
func BuildKGBBoxDiff(prev, next *kgUniverse) *kgBBoxDiff {
	old := make(map[string][4]float64, len(prev.KGs))
	for _, r := range prev.KGs {
		old[r.KGCode] = r.BBox
	}
	d := &kgBBoxDiff{Schema: "kg-universe-bbox-diff-1", BBoxRevision: next.BBoxRevision, BBoxSource: next.BBoxSource,
		PadDeg: kgVGDBBoxPad, UniverseHash: next.UniverseHash, FromGeneratedAt: prev.GeneratedAt, ToGeneratedAt: next.GeneratedAt,
		Rule:    "bbox_new = union(bbox_old, VGD KG polygon envelope + pad_deg); bbox_new ⊇ bbox_old for every KG; process `slivers` to close the gap",
		KGCount: next.KGCount, Changed: []kgBBoxChange{}}
	for _, r := range next.KGs {
		ob := old[r.KGCode]
		if ob == r.BBox {
			continue
		}
		md := math.Max(math.Max(ob[0]-r.BBox[0], ob[1]-r.BBox[1]), math.Max(r.BBox[2]-ob[2], r.BBox[3]-ob[3]))
		if ob == [4]float64{} {
			md = math.Max(r.BBox[2]-r.BBox[0], r.BBox[3]-r.BBox[1])
		}
		d.Changed = append(d.Changed, kgBBoxChange{KGCode: r.KGCode, BBoxOld: ob, BBoxNew: r.BBox,
			MaxDelta: round7(md), Slivers: kgBBoxSlivers(ob, r.BBox)})
	}
	d.ChangedCount = len(d.Changed)
	return d
}

func WriteKGBBoxDiffFile(path string, d *kgBBoxDiff) error {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	enc := json.NewEncoder(zw)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(d); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func LoadKGBBoxDiffFile(path string) (*kgBBoxDiff, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var d kgBBoxDiff
	if err := json.NewDecoder(zr).Decode(&d); err != nil {
		return nil, err
	}
	if d.Changed == nil {
		d.Changed = []kgBBoxChange{}
	}
	return &d, nil
}

// updateKGTableBBoxes writes the universe's bboxes back to the kg table in
// one transaction (one-off CLI pass; the prepared statement runs inside a
// single transaction, not per-request) so kg_bbox_index.go and every
// point/bbox lookup agree with the published list.
func updateKGTableBBoxes(u *kgUniverse) (int64, error) {
	if globalSearchIndex == nil {
		return 0, fmt.Errorf("search index not initialized")
	}
	tx, err := globalSearchIndex.DB().Begin()
	if err != nil {
		return 0, err
	}
	st, err := tx.Prepare(`UPDATE kg SET min_lon=?, min_lat=?, max_lon=?, max_lat=?
		WHERE kg_code=? AND (min_lon IS NOT ? OR min_lat IS NOT ? OR max_lon IS NOT ? OR max_lat IS NOT ?)`)
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	var n int64
	for _, r := range u.KGs {
		b := r.BBox
		res, err := st.Exec(b[0], b[1], b[2], b[3], r.KGCode, b[0], b[1], b[2], b[3])
		if err != nil {
			st.Close()
			tx.Rollback()
			return 0, err
		}
		k, _ := res.RowsAffected()
		n += k
	}
	st.Close()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	InvalidateKGBBoxIndex()
	return n, nil
}

// ── served diff ──────────────────────────────────────────────────────────

var (
	kgBBoxDiffMu  sync.RWMutex
	kgBBoxDiffCur *kgBBoxDiff
)

// InitKGBBoxDiff loads the committed diff (absent → endpoint answers an empty list).
func InitKGBBoxDiff() {
	d, err := LoadKGBBoxDiffFile(kgUniverseBBoxDiffPath)
	if err != nil {
		d = &kgBBoxDiff{Schema: "kg-universe-bbox-diff-1", Changed: []kgBBoxChange{}}
	}
	kgBBoxDiffMu.Lock()
	kgBBoxDiffCur = d
	kgBBoxDiffMu.Unlock()
}

// KGBBoxDiffStats — size line for the debug inflight page.
func KGBBoxDiffStats() map[string]interface{} {
	kgBBoxDiffMu.RLock()
	defer kgBBoxDiffMu.RUnlock()
	if kgBBoxDiffCur == nil {
		return map[string]interface{}{"loaded": false}
	}
	return map[string]interface{}{"loaded": true, "bbox_revision": kgBBoxDiffCur.BBoxRevision, "changed": len(kgBBoxDiffCur.Changed)}
}

// handleV1KGsBBoxChanges — GET /api/v1/kgs/bbox_changes
//
//	(no params)         every KG whose bbox widened in the current bbox revision
//	?min_delta=0.001    only KGs where some edge moved outward by ≥ min_delta degrees
//	?codes=63308,63330  subset by code
//	?fields=codes       {codes:[…]} only
//	?state=6 | ?district_code=616 | ?gemeinde_code=61630   subset via the universe rows
func handleV1KGsBBoxChanges(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		apiError(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	kgBBoxDiffMu.RLock()
	d := kgBBoxDiffCur
	kgBBoxDiffMu.RUnlock()
	kgUniverseMu.RLock()
	u, idx := kgUniverseCur, kgUniverseIndex
	kgUniverseMu.RUnlock()
	if d == nil || u == nil {
		w.Header().Set("Retry-After", "30")
		apiError(w, "kg universe not loaded yet", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	minDelta := 0.0
	if v := q.Get("min_delta"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 || f > 10 {
			apiError(w, "min_delta: degrees, 0 ≤ x ≤ 10", http.StatusBadRequest)
			return
		}
		minDelta = f
	}
	var want map[string]bool
	if v := q.Get("codes"); v != "" {
		want = map[string]bool{}
		for _, c := range ParseKGList(v) {
			want[padKGCode(c)] = true
		}
	}
	state, dist, gem := q.Get("state"), q.Get("district_code"), q.Get("gemeinde_code")
	rows := make([]kgBBoxChange, 0, len(d.Changed))
	for _, c := range d.Changed {
		if c.MaxDelta < minDelta || (want != nil && !want[c.KGCode]) {
			continue
		}
		if state != "" || dist != "" || gem != "" {
			i, ok := idx[c.KGCode]
			if !ok {
				continue
			}
			ur := u.KGs[i]
			if (state != "" && ur.StateCode != state && ur.StateName != state) || (dist != "" && ur.DistrictCode != dist) || (gem != "" && ur.GemeindeCode != gem) {
				continue
			}
		}
		rows = append(rows, c)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].KGCode < rows[j].KGCode })
	data := map[string]interface{}{
		"bbox_revision":     d.BBoxRevision,
		"bbox_source":       d.BBoxSource,
		"pad_deg":           d.PadDeg,
		"rule":              d.Rule,
		"universe_hash":     u.UniverseHash,
		"from_generated_at": d.FromGeneratedAt,
		"to_generated_at":   d.ToGeneratedAt,
		"changed_total":     d.ChangedCount,
		"min_delta":         minDelta,
		"count":             len(rows),
	}
	if q.Get("fields") == "codes" {
		codes := make([]string, len(rows))
		for i, c := range rows {
			codes[i] = c.KGCode
		}
		data["codes"] = codes
	} else {
		data["changed"] = rows
	}
	h := w.Header()
	h.Set("Cache-Control", "public, max-age=3600, stale-while-revalidate=86400")
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("X-KG-Universe-Hash", u.UniverseHash)
	h.Set("X-KG-BBox-Revision", strconv.Itoa(d.BBoxRevision))
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": data, "meta": map[string]interface{}{
		"total": len(rows), "generated_at": time.Now().UTC().Format(time.RFC3339), "license": AttributionMetaFor("/api/v1/kgs", false)}})
}
