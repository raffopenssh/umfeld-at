// ne_mvt_jobs.go — PMTiles archives per bbox: the cell layer (ne_mvt.go) for
// offline work in QGIS. Building needs every bin under the box (≈ 9 s each
// when cold) and every tile of z minz…maxz, so archives are JOBS: one worker,
// a bounded queue, progress on every poll, the file served when done.
//
//	GET /api/v1/ne/pmtiles?bbox=w,s,e,n | kg= | h3= | adm= | share=#… [&minz=9][&maxz=14]
//	    → 200 application/vnd.pmtiles  (done; Content-Disposition attachment)
//	    → 202 {"status":"queued|building","job":…,"progress":{…},"eta_s":…}  + Refresh / Retry-After
//	    → 413 box too large (≤ nePMTMaxKm2 km², ≤ nePMTMaxTiles tiles)
//	GET /api/v1/ne/pmtiles/jobs                        the queue (ids, progress — no identities)
//
// The unit keys (kg, h3, adm, share) are the same the unit export takes
// (api_ne_stats_unit.go), so the glyph's link menu can hand the user an
// archive of exactly what they look at.
//
// Tiles are clipped to the box (neMVTTile clip): a z-9 tile in an archive
// holds only the parents whose centre lies in the box, so an archive never
// needs bins beyond the box + one parent radius.
//
// Disk: data/ne_mvt/<id>.pmtiles, rebuildable, ≤ nePMTDiskCap, oldest first.
// A job id is the hash of (box, zooms, bin states at creation) — data moved
// ⇒ new id, old files age out. Gate: neMVTGate charges the whole archive
// (tiles + km²) once, at creation.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	nePMTDir      = "data/ne_mvt"
	nePMTDiskCap  = 2 << 30
	nePMTMaxKm2   = 1500.0
	nePMTMaxTiles = 20000
	nePMTQueueMax = 16
	nePMTKeep     = 48 * time.Hour
)

type nePMTJob struct {
	ID        string     `json:"job"`
	Box       [4]float64 `json:"bbox"`
	MinZ      int        `json:"minz"`
	MaxZ      int        `json:"maxz"`
	Status    string     `json:"status"` // queued | building | done | error
	BinsTotal int        `json:"bins_total"`
	BinsDone  int        `json:"bins_done"`
	Tiles     int        `json:"tiles_total"`
	TilesDone int        `json:"tiles_done"`
	Stored    int        `json:"tiles_stored"`
	Bytes     int64      `json:"bytes"`
	Created   time.Time  `json:"created"`
	Finished  time.Time  `json:"finished,omitempty"`
	Err       string     `json:"error,omitempty"`
	ident     string
	path      string
}

var (
	nePMTMu    sync.Mutex
	nePMTJobs  = map[string]*nePMTJob{}
	nePMTQueue []string
	nePMTOnce  sync.Once
	nePMTWake  chan struct{}
)

func nePMTJobStats() map[string]interface{} {
	nePMTMu.Lock()
	defer nePMTMu.Unlock()
	var q, b, d, e int
	for _, j := range nePMTJobs {
		switch j.Status {
		case "queued":
			q++
		case "building":
			b++
		case "done":
			d++
		case "error":
			e++
		}
	}
	return map[string]interface{}{"queued": q, "building": b, "done": d, "error": e, "max_queue": nePMTQueueMax, "disk_bytes": nePMTDiskSize()}
}

func nePMTDiskSize() int64 {
	var n int64
	filepath.Walk(nePMTDir, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			n += fi.Size()
		}
		return nil
	})
	return n
}

// nePMTSweep — drop archives older than nePMTKeep, then oldest until under the cap.
func nePMTSweep() {
	ents, err := os.ReadDir(nePMTDir)
	if err != nil {
		return
	}
	type fe struct {
		p string
		t time.Time
		n int64
	}
	var fs []fe
	var tot int64
	for _, e := range ents {
		fi, err := e.Info()
		if err != nil || e.IsDir() {
			continue
		}
		p := filepath.Join(nePMTDir, e.Name())
		if time.Since(fi.ModTime()) > nePMTKeep || strings.HasSuffix(e.Name(), ".tmp") && time.Since(fi.ModTime()) > time.Hour {
			os.Remove(p)
			continue
		}
		fs = append(fs, fe{p, fi.ModTime(), fi.Size()})
		tot += fi.Size()
	}
	sort.Slice(fs, func(a, b int) bool { return fs[a].t.Before(fs[b].t) })
	for _, f := range fs {
		if tot <= nePMTDiskCap {
			break
		}
		os.Remove(f.p)
		tot -= f.n
	}
	nePMTMu.Lock()
	for id, j := range nePMTJobs {
		if j.Status == "done" || j.Status == "error" {
			if _, err := os.Stat(j.path); (err != nil && j.Status == "done") || time.Since(j.Finished) > nePMTKeep {
				delete(nePMTJobs, id)
			}
		}
	}
	nePMTMu.Unlock()
}

// nePMTTileCount — tiles of the box over the zoom range.
func nePMTTileCount(box [4]float64, minZ, maxZ int) int {
	n := 0
	for z := minZ; z <= maxZ; z++ {
		x0, y0, x1, y1 := mvtTilesInBox(z, box[0], box[1], box[2], box[3])
		n += (x1 - x0 + 1) * (y1 - y0 + 1)
	}
	return n
}

// nePMTBinZ — the lowest zoom of an archive that reads bins: with the national
// table present, res ≤ 9 zooms read the table instead (-1 = no bin zoom at all).
func nePMTBinZ(minZ, maxZ int) int {
	for z := minZ; z <= maxZ; z++ {
		if neMVTZoomRes(z) > neNatRes || neNatGet() == nil {
			return z
		}
	}
	return -1
}

// nePMTBins — the bins an archive must have.
func nePMTBins(box [4]float64, minZ, maxZ int) [][2]int {
	z := nePMTBinZ(minZ, maxZ)
	if z < 0 {
		return nil
	}
	px, py := neMVTResPadDeg(neMVTZoomRes(z))
	return neMVTBins(box[0]-px, box[1]-py, box[2]+px, box[3]+py)
}

// nePMTBinTiles — tiles of an archive that read bins (the costly ones).
func nePMTBinTiles(box [4]float64, minZ, maxZ int) int {
	z := nePMTBinZ(minZ, maxZ)
	if z < 0 {
		return 0
	}
	return nePMTTileCount(box, z, maxZ)
}

// nePMTJobID — hash of box + zooms + the states of the bins under the box (+ the national table's).
func nePMTJobID(box [4]float64, minZ, maxZ int) string {
	h := sha256.New()
	fmt.Fprintf(h, "pmt1;%.5f,%.5f,%.5f,%.5f;%d-%d;%s;", box[0], box[1], box[2], box[3], minZ, maxZ, neNatTileState())
	for _, k := range nePMTBins(box, minZ, maxZ) {
		h.Write([]byte(neMVTBinState(k[0], k[1])))
		h.Write([]byte{';'})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// nePMTSubmit — find or create the job for (box, zooms). Returns job, created.
func nePMTSubmit(box [4]float64, minZ, maxZ int, ident string) (*nePMTJob, bool, string) {
	nePMTOnce.Do(func() {
		nePMTWake = make(chan struct{}, 1)
		os.MkdirAll(nePMTDir, 0o755)
		go nePMTWorker()
	})
	id := nePMTJobID(box, minZ, maxZ)
	path := filepath.Join(nePMTDir, id+".pmtiles")
	nePMTMu.Lock()
	defer nePMTMu.Unlock()
	if j := nePMTJobs[id]; j != nil {
		return j, false, ""
	}
	j := &nePMTJob{ID: id, Box: box, MinZ: minZ, MaxZ: maxZ, Created: time.Now(), ident: ident, path: path,
		Tiles: nePMTTileCount(box, minZ, maxZ)}
	if fi, err := os.Stat(path); err == nil { // built earlier (restart): serve it
		j.Status, j.Bytes, j.Finished = "done", fi.Size(), fi.ModTime()
		nePMTJobs[id] = j
		return j, false, ""
	}
	if len(nePMTQueue) >= nePMTQueueMax {
		return nil, false, "archive queue full — try again later"
	}
	j.Status = "queued"
	j.BinsTotal = len(nePMTBins(box, minZ, maxZ))
	nePMTJobs[id] = j
	nePMTQueue = append(nePMTQueue, id)
	select {
	case nePMTWake <- struct{}{}:
	default:
	}
	return j, true, ""
}

func nePMTWorker() {
	for {
		nePMTMu.Lock()
		if len(nePMTQueue) == 0 {
			nePMTMu.Unlock()
			<-nePMTWake
			continue
		}
		id := nePMTQueue[0]
		nePMTQueue = nePMTQueue[1:]
		j := nePMTJobs[id]
		if j != nil {
			j.Status = "building"
		}
		nePMTMu.Unlock()
		if j == nil {
			continue
		}
		err := nePMTBuild(j)
		nePMTMu.Lock()
		j.Finished = time.Now()
		if err != nil {
			j.Status, j.Err = "error", err.Error()
			log.Printf("pmtiles job %s: %v", j.ID, err)
		} else {
			j.Status = "done"
		}
		nePMTMu.Unlock()
		nePMTSweep()
	}
}

// nePMTBuild — bins first (progress), then every tile through the same
// renderer + LRU the live endpoint uses, then one write.
func nePMTBuild(j *nePMTJob) error {
	bins := nePMTBins(j.Box, j.MinZ, j.MaxZ)
	for q, k := range bins {
		if _, _, ok := neFidxReady(k[0], k[1]); !ok {
			if _, _, err := neFidxGet(k[0], k[1]); err != nil {
				log.Printf("pmtiles job %s: bin %d/%d: %v", j.ID, k[0], k[1], err)
			}
			neMVTStateForget(k[0], k[1])
		}
		nePMTMu.Lock()
		j.BinsDone = q + 1
		nePMTMu.Unlock()
	}
	pw := newPMTWriter(j.MinZ, j.MaxZ, j.Box, neMVTVectorLayersMeta(j.Box, j.MinZ, j.MaxZ))
	done := 0
	for z := j.MinZ; z <= j.MaxZ; z++ {
		x0, y0, x1, y1 := mvtTilesInBox(z, j.Box[0], j.Box[1], j.Box[2], j.Box[3])
		for x := x0; x <= x1; x++ {
			for y := y0; y <= y1; y++ {
				gz, _, prog, complete, err := neMVTTileRetry(z, x, y, &j.Box)
				if err == nil && complete {
					pw.add(z, x, y, gz)
				} else {
					log.Printf("pmtiles job %s: tile %d/%d/%d skipped: complete=%v err=%v progress=%+v", j.ID, z, x, y, complete, err, prog)
				}
				done++
				if done%50 == 0 {
					nePMTMu.Lock()
					j.TilesDone = done
					nePMTMu.Unlock()
				}
			}
		}
	}
	nePMTMu.Lock()
	j.TilesDone, j.Stored = done, len(pw.entries)
	nePMTMu.Unlock()
	tmp := j.path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	n, err := pw.write(f)
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, j.path); err != nil {
		return err
	}
	nePMTMu.Lock()
	j.Bytes = n
	nePMTMu.Unlock()
	return nil
}

// neMVTTileRetry — a tile for the archive: bins are built by now; a busy
// semaphore is waited out rather than skipped.
func neMVTTileRetry(z, x, y int, clip *[4]float64) ([]byte, string, neMVTProgress, bool, error) {
	for a := 0; ; a++ {
		gz, key, p, complete, err := neMVTTile(z, x, y, -1, clip)
		if err != errBusy || a >= 30 {
			return gz, key, p, complete, err
		}
		time.Sleep(time.Second)
	}
}

// nePMTBox — the box of the request: bbox= or the unit keys of the stats export.
func nePMTBox(r *http.Request) ([4]float64, string, string, int) {
	q := r.URL.Query()
	if s := q.Get("bbox"); s != "" && q.Get("share") == "" {
		if s == "austria" || s == "at" {
			return neMVTAustria, "austria", "", 0
		}
		b, msg := neMVTParseBox(s)
		return b, "bbox", msg, 400
	}
	units, msg, code := neStatsUnits(q)
	if msg != "" {
		return [4]float64{}, "", msg, code
	}
	u := units[0]
	b := u.bbox
	for _, v := range units[1:] {
		b[0], b[1] = min(b[0], v.bbox[0]), min(b[1], v.bbox[1])
		b[2], b[3] = max(b[2], v.bbox[2]), max(b[3], v.bbox[3])
	}
	name := u.kind + "-" + u.key
	if len(units) > 1 {
		name += fmt.Sprintf("+%d", len(units)-1)
	}
	return b, name, "", 0
}

// handleNEPMTiles — see file header.
func handleNEPMTiles(w http.ResponseWriter, r *http.Request, rest string) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if rest == "pmtiles/jobs" {
		nePMTMu.Lock()
		jobs := make([]*nePMTJob, 0, len(nePMTJobs))
		for _, j := range nePMTJobs {
			c := *j
			jobs = append(jobs, &c)
		}
		nePMTMu.Unlock()
		sort.Slice(jobs, func(a, b int) bool { return jobs[a].Created.After(jobs[b].Created) })
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, map[string]interface{}{"jobs": jobs, "queue": nePMTJobStats()})
		return
	}
	q := r.URL.Query()
	box, name, msg, code := nePMTBox(r)
	if msg != "" {
		apiError(w, msg, code)
		return
	}
	minZ, maxZ := neMVTMinZ, 14
	if v, err := strconv.Atoi(q.Get("minz")); err == nil {
		minZ = v
	}
	if v, err := strconv.Atoi(q.Get("maxz")); err == nil {
		maxZ = v
	}
	if minZ < neMVTMinZ || maxZ > neMVTMaxZ || minZ > maxZ {
		apiError(w, fmt.Sprintf("minz/maxz within %d…%d", neMVTMinZ, neMVTMaxZ), 400)
		return
	}
	km2 := neMVTBoxKm2(box[0], box[1], box[2], box[3])
	tiles := nePMTTileCount(box, minZ, maxZ)
	// the limits bound the bin-backed zooms; an overview archive (z ≤ 11 from
	// the national table) may span the whole country
	if bt := nePMTBinTiles(box, minZ, maxZ); bt > 0 && (km2 > nePMTMaxKm2 || bt > nePMTMaxTiles) {
		apiError(w, fmt.Sprintf("archive too large for the fine zooms: %.0f km² / %d tiles at z ≥ %d (≤ %.0f km² and ≤ %d tiles) — split the box, or maxz=11 for the overview of any area up to all of Austria", km2, bt, nePMTBinZ(minZ, maxZ), nePMTMaxKm2, nePMTMaxTiles), 413)
		return
	}
	if tiles > 40000 {
		apiError(w, fmt.Sprintf("archive too large: %d tiles — lower maxz or split the box", tiles), 413)
		return
	}
	// an existing archive costs nothing; a new job is charged once
	id := nePMTJobID(box, minZ, maxZ)
	nePMTMu.Lock()
	j := nePMTJobs[id]
	if j == nil { // the same box already queued/building under an earlier state: that one will do
		for _, o := range nePMTJobs {
			if o.Box == box && o.MinZ == minZ && o.MaxZ == maxZ && (o.Status == "queued" || o.Status == "building") {
				j, id = o, o.ID
				break
			}
		}
	}
	nePMTMu.Unlock()
	gateID := ""
	if j == nil {
		var ok bool
		if gateID, ok = neMVTGate(w, r, tiles, box[0], box[1], box[2], box[3]); !ok {
			return
		}
	}
	if j == nil {
		j, _, msg = nePMTSubmit(box, minZ, maxZ, gateID)
	}
	if msg != "" {
		w.Header().Set("Retry-After", "60")
		apiError(w, msg, http.StatusServiceUnavailable)
		return
	}
	nePMTMu.Lock()
	c := *j
	nePMTMu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	switch c.Status {
	case "done":
		f, err := os.Open(c.path)
		if err != nil {
			apiError(w, "archive gone — request again", 404)
			nePMTMu.Lock()
			delete(nePMTJobs, c.ID)
			nePMTMu.Unlock()
			return
		}
		defer f.Close()
		fi, _ := f.Stat()
		w.Header().Set("Content-Type", "application/vnd.pmtiles")
		w.Header().Set("Content-Disposition", `attachment; filename="umfeld-at-cells-`+name+`-z`+strconv.Itoa(c.MinZ)+`-`+strconv.Itoa(c.MaxZ)+`.pmtiles"`)
		w.Header().Set("Cache-Control", "private, max-age=3600")
		w.Header().Set("ETag", `"`+c.ID+`"`)
		http.ServeContent(w, r, c.ID+".pmtiles", fi.ModTime(), f)
		if gateID == "" {
			gateID, _ = exportIdentity(r)
		}
		exportAccount(gateID, int(fi.Size()))
	case "error":
		apiError(w, "archive failed: "+c.Err+" — request again later", 500)
		nePMTMu.Lock()
		delete(nePMTJobs, c.ID)
		nePMTMu.Unlock()
	default:
		pos := 0
		nePMTMu.Lock()
		for i, qid := range nePMTQueue {
			if qid == c.ID {
				pos = i + 1
			}
		}
		nePMTMu.Unlock()
		per := 9.0
		if b, ms := neFidxBuilt.Load(), neFidxBuildMs.Load(); b > 3 {
			per = float64(ms) / float64(b) / 1000
		}
		eta := int(float64(c.BinsTotal-c.BinsDone)*per) + (c.Tiles-c.TilesDone)/20 + 1
		wait := max(10, min(eta, 120))
		w.Header().Set("Retry-After", strconv.Itoa(wait))
		w.Header().Set("Refresh", strconv.Itoa(wait)) // browsers re-request on their own
		if strings.Contains(r.Header.Get("Accept"), "text/html") && q.Get("format") != "json" {
			hw := max(3, min(wait, 8)) // a page can afford to look more often than a script
			w.Header().Set("Refresh", strconv.Itoa(hw))
			nePMTProgressPage(w, &c, pos, eta, hw)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]interface{}{"status": c.Status, "job": c.ID, "queue_position": pos, "bbox": c.Box, "minz": c.MinZ, "maxz": c.MaxZ,
			"progress": map[string]int{"bins_done": c.BinsDone, "bins_total": c.BinsTotal, "tiles_done": c.TilesDone, "tiles_total": c.Tiles}, "eta_s": eta,
			"hint": "the archive is being prepared — this page reloads itself; keep the URL, it turns into the download when done"})
	}
}

// nePMTProgressPage — what a browser sees while its archive is being built:
// a small page with a bar, reloading itself (Refresh) until the same URL
// answers the file. Download links from the glyph land here.
func nePMTProgressPage(w http.ResponseWriter, c *nePMTJob, pos, eta, wait int) {
	tot := max(c.BinsTotal*8+c.Tiles, 1)
	done := c.BinsDone*8 + c.TilesDone
	pct := done * 100 / tot
	st := "preparing"
	if c.Status == "queued" {
		st = fmt.Sprintf("queued · position %d", pos)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>preparing your archive · umfeld-at</title>
<style>body{font:16px/1.5 ui-monospace,SFMono-Regular,Menlo,monospace;color:#222;background:#fbfbf8;margin:0;display:flex;min-height:100vh;align-items:center;justify-content:center}
main{max-width:30em;padding:2em}h1{font-size:1.1em;font-weight:500;margin:0 0 .6em}.bar{height:8px;border-radius:4px;background:#e4e4dd;overflow:hidden;margin:1em 0}.bar i{display:block;height:100%%;width:%d%%;background:#4b7a3f;transition:width .4s}
p{margin:.4em 0;color:#555}b{color:#222;font-weight:500}small{color:#888}@media(prefers-color-scheme:dark){body{background:#151515;color:#ddd}p{color:#aaa}b{color:#eee}.bar{background:#333}}</style>
<main><h1>preparing your archive</h1><p><b>%s</b> · %d %% · about %d s left</p><div class="bar"><i></i></div>
<p>box %.3f, %.3f – %.3f, %.3f · zoom %d–%d · %d of %d tiles</p>
<p><small>This page reloads itself every %d s and becomes the download when the file is ready. You can close it and open the same link later — the archive stays 48 h.</small></p></main>`,
		pct, st, pct, eta, c.Box[0], c.Box[1], c.Box[2], c.Box[3], c.MinZ, c.MaxZ, c.TilesDone, c.Tiles, wait)
}
