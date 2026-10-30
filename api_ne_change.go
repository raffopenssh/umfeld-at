// api_ne_change.go — NE change protocol handlers (docs/ne-change.md §3):
//
//	step 0  POST …/ne/{kg}/report   header only → unchanged | 202 need:"chunks"   (neReportHeaderOnly)
//	step 1  POST …/ne/{kg}/report   + chunks_ap{} → want_chunks[]                   (neReportWants)
//	step 2  POST …/ne/{kg}/chunks   NECH body: stripped rows of want_chunks        (handleNEChunksPost)
//	audit   GET  …/ne/{kg}/chunks?chunk=<res10>[&rows=1]                          (handleNEChunksList)
//
// Contributor-allowed under /contrib, mirrored under /k. Public-path file: word ban applies.
package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"time"
)

// neReportHeaderOnly — step 0. "Last held" = the most recent report of the
// same token + source class + algo for this KG (neReportPrevious — the SAME
// key the full path uses), whether it carried chunks or was itself stored
// stripped as `unchanged:true`; equal digest and epoch ⇒ unchanged. Observer
// and bbox are NOT part of the key: the dedupe is fleet-wide per token, and a
// bbox change that leaves the digest equal is a no-op (23308 after v0.3.3:
// the old rule required a chunk-bearing report with the SAME bbox, which no
// longer existed, so every header probe answered 202 forever).
func neReportHeaderOnly(w http.ResponseWriter, kg string, tokIdx int, tokName string, rep *neReport) {
	prior := neReadReports(kg)
	last := neReportPrevious(prior, tokName, rep)
	// the chunk table for the re-sighting: the last report with chunks under
	// this digest (an unchanged record is stored without its chunks)
	var withChunks *neStoredReport
	if last != nil && last.Report.Digest == rep.Digest {
		for i := len(prior) - 1; i >= 0; i-- {
			s := &prior[i]
			if s.Token == tokName && neBevEquivalent(s.Report.Source) == neBevEquivalent(rep.Source) && s.Report.Algo == rep.Algo &&
				s.Report.Digest == rep.Digest && len(s.Report.Chunks) > 0 {
				withChunks = s
				break
			}
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if last == nil || last.Report.Digest != rep.Digest || last.Report.Epoch != rep.Epoch {
		resp := map[string]interface{}{"kg": kg, "stored": false, "unchanged": false, "need": "chunks", "observer": rep.Observer, "token": tokName}
		if last != nil {
			resp["last_digest"], resp["last_received_at"] = last.Report.Digest, last.ReceivedAt
		}
		neReportCounters.bump(tokIdx, rep.Observer, "need_chunks")
		writeJSON(w, 202, resp)
		return
	}
	nChunks := 0
	if withChunks != nil {
		nChunks = len(withChunks.Report.Chunks)
	}
	resp := map[string]interface{}{"kg": kg, "stored": true, "unchanged": true, "unchanged_since": last.ReceivedAt, "observer": rep.Observer,
		"token": tokName, "epoch": rep.Epoch, "source": rep.Source, "header_only": true,
		"since_last": map[string]interface{}{"compared": nChunks, "same": nChunks, "changed": []string{}, "previous_received_at": last.ReceivedAt}}
	// every chunk of the equal report is a re-sighting of the version we hold
	// under that frozen digest (confirmation, docs/ne-change.md §5): same
	// observer only on a later pass (≥ 20 h), another observer at once
	if c := neChangeGet(kg); c != nil && withChunks != nil {
		pairs := map[string]string{}
		held := c.latestDigests()
		for ch, d := range withChunks.Report.Chunks {
			if l, ok := held[ch]; ok && l.Digest == d {
				pairs[ch] = l.AP
			}
		}
		if n := neChangeConfirm(kg, tokName, rep.Observer, rep.Source, rep.Epoch, now, pairs); n > 0 {
			resp["chunks_confirmed"] = n
		}
	}
	line := neStoredReport{ReceivedAt: now, Token: tokName, Unchanged: true, Report: *rep, Result: resp}
	neAppendReport(kg, line)
	neReportCounters.record(tokIdx, rep, resp)
	log.Printf("🧾 NE report kg=%s tok=%s obs=%q src=%s header-only unchanged since %s", kg, tokName, rep.Observer, rep.Source, last.ReceivedAt)
	writeJSON(w, 200, resp)
}

// neReportWants — step 1: given the report's digest_ap per chunk, decide which
// chunks we want rows for (new digest_ap or never held), record re-sightings
// of versions we hold, and remember the wants for step 2. Lossy/layout-suspect
// reports get no wants: their diffs are not change (docs/ne-cells.md §coverage).
func neReportWants(kg, tokName, now string, rep *neReport, ap map[string]string, resp map[string]interface{}) {
	resp["ap_version"] = rep.APVersion
	if sus, _ := resp["change_suspect"].(string); sus != "" {
		resp["want_chunks"] = []string{}
		resp["want_note"] = "report is change_suspect:" + sus + " — no chunk rows requested"
		return
	}
	held := map[string]neWant{}
	c := neChangeGet(kg)
	if c != nil {
		held = c.latestDigests()
	}
	want := map[string]neWant{}
	wantAP := map[string]string{}
	wantTiles := map[string]string{}
	pairs := map[string]string{}
	sameInput := 0
	for ch, d := range rep.Chunks {
		a, ok := ap[ch]
		if !ok || !neHex16Re.MatchString(a) {
			continue
		}
		if h, ok := held[ch]; ok && h.AP == a {
			pairs[ch] = a
			continue
		}
		th := neTileHashNorm(rep.ChunksTiles[ch])
		if th != "" {
			wantTiles[ch] = th
			if h, ok := held[ch]; ok && h.Tile == th {
				sameInput++ // digest flipped, tiles did not: a pipeline defect — stored, flagged, never counted
			}
		}
		want[ch] = neWant{Digest: d, AP: a, Tile: th}
		wantAP[ch] = a
	}
	if len(wantTiles) > 0 {
		resp["want_tiles"] = wantTiles
	}
	if sameInput > 0 {
		resp["qc_same_input"] = sameInput
	}
	list := make([]string, 0, len(want))
	for ch := range want {
		list = append(list, ch)
	}
	sort.Strings(list)
	resp["want_chunks"] = list
	resp["chunks_held"] = len(pairs)
	if len(want) > 0 {
		resp["want_ap"] = wantAP
		neWantSet(kg, tokName, rep.Observer, want)
	}
	if n := neChangeConfirm(kg, tokName, rep.Observer, rep.Source, rep.Epoch, now, pairs); n > 0 {
		resp["chunks_confirmed"] = n
	}
	if n := neChangeAttachTiles(kg, tokName, rep.Observer, rep.Source, rep.Epoch, now, pairs, rep.ChunksTiles); n > 0 {
		resp["tiles_attached"] = n // held versions that now carry this report's tile hash (late provenance)
	}
}

// handleNEChunksPost — step 2. Body: NECH | u16 n | n × (u64 chunk, u32 len, rows).
func handleNEChunksPost(w http.ResponseWriter, r *http.Request, kg string, tokIdx int) {
	body, err := io.ReadAll(io.LimitReader(r.Body, neChangeMaxBody+1))
	if err != nil || len(body) > neChangeMaxBody {
		apiError(w, fmt.Sprintf("body ≤ %d bytes", neChangeMaxBody), 413)
		return
	}
	if !neWriteBudgetGate(w, tokIdx, len(body)) {
		return
	}
	observer := r.URL.Query().Get("observer")
	if len(body) < 6 || string(body[:4]) != "NECH" || observer == "" {
		apiError(w, "body must be NECH (ne_cells.change.pack_chunk_rows) and ?observer= the reporting observer", 400)
		return
	}
	tokName := tokenName(tokIdx)
	wants := neWantGet(kg, tokName, observer)
	if wants == nil {
		apiError(w, "no pending want_chunks for this observer — post the report with chunks_ap first", 409)
		return
	}
	n := int(binary.LittleEndian.Uint16(body[4:]))
	off := 6
	now := time.Now().UTC().Format(time.RFC3339)
	src, epoch, bboxKey := neLastReportMeta(kg, tokName, observer)
	results := map[string]interface{}{}
	stored, seen, rejected := 0, 0, 0
	var items []neChangeItem
	sightings := map[string]string{}
	held := neChangeGet(kg)
	for i := 0; i < n; i++ {
		if off+12 > len(body) {
			apiError(w, "truncated body", 400)
			return
		}
		cid := binary.LittleEndian.Uint64(body[off:])
		ln := int(binary.LittleEndian.Uint32(body[off+8:]))
		off += 12
		if off+ln > len(body) || ln%neLURowBytes != 0 {
			apiError(w, "truncated chunk rows", 400)
			return
		}
		rows := body[off : off+ln]
		off += ln
		ch := neCellHex(cid)
		wnt, ok := wants[ch]
		switch {
		case !ok:
			results[ch] = "not_wanted"
			rejected++
			continue
		case neAPDigest(rows) != wnt.AP:
			results[ch] = "digest_ap_mismatch"
			rejected++
			continue
		case !neRowsBelong(rows, cid):
			results[ch] = "rows_outside_chunk"
			rejected++
			continue
		}
		if held != nil {
			if l, ok := held.latest[cid]; ok && l == wnt.AP { // another observer landed it first → sighting
				sightings[ch] = wnt.AP
				results[ch] = "seen"
				seen++
				delete(wants, ch)
				continue
			}
		}
		items = append(items, neChangeItem{Chunk: ch, Digest: wnt.Digest, AP: wnt.AP, TileHash: wnt.Tile, Rows: rows})
		results[ch] = "stored"
		stored++
		delete(wants, ch)
	}
	if len(items) > 0 {
		meta := neChunkVersion{Token: tokName, Observer: observer, Source: src, Epoch: epoch, At: now, BBox: bboxKey}
		if err := neChangeAppend(kg, meta, items); err != nil {
			apiError(w, "store: "+err.Error(), 500)
			return
		}
	}
	if len(sightings) > 0 {
		neChangeConfirm(kg, tokName, observer, src, epoch, now, sightings)
	}
	if len(wants) == 0 {
		neWantSet(kg, tokName, observer, map[string]neWant{})
	}
	resp := map[string]interface{}{"kg": kg, "observer": observer, "token": tokName, "chunks": results, "stored": stored, "seen": seen, "rejected": rejected,
		"want_remaining": len(wants)}
	if c := neChangeGet(kg); c != nil && stored > 0 {
		if d, err := c.deltasOf(); err == nil {
			resp["deltas"] = neDeltasFor(c, d, results)
			resp["summary"] = c.summary(d)
		}
	}
	log.Printf("🧾 NE chunks kg=%s tok=%s obs=%q stored=%d seen=%d rejected=%d", kg, tokName, observer, stored, seen, rejected)
	writeJSON(w, 200, resp)
}

func neRowsBelong(rows []byte, chunk uint64) bool {
	for i := 0; i+neLURowBytes <= len(rows); i += neLURowBytes {
		if h3Parent(binary.LittleEndian.Uint64(rows[i:]), neChunkRes10) != chunk {
			return false
		}
	}
	return true
}

func neLastReportMeta(kg, token, observer string) (source, epoch, bbox string) {
	st := neReadReports(kg)
	for i := len(st) - 1; i >= 0; i-- {
		if st[i].Token == token && st[i].Report.Observer == observer {
			return st[i].Report.Source, st[i].Report.Epoch, neBBoxKey(st[i].Report.BBox)
		}
	}
	return "", "", ""
}

// neDeltasFor — the delta rows of the chunks just stored (what the peer gets back).
func neDeltasFor(c *neChangeKG, d *neChangeDeltas, results map[string]interface{}) []map[string]interface{} {
	out := []map[string]interface{}{}
	for i := range d.rows {
		x := &d.rows[i]
		if results[neCellHex(x.Chunk)] != "stored" || x.To != c.versions[x.Chunk][len(c.versions[x.Chunk])-1] {
			continue
		}
		out = append(out, map[string]interface{}{"cell": neCellHex(x.Cell), "chunk": neCellHex(x.Chunk), "d_lu_m2": x.DLU, "d_str_cover_m2": x.DCover,
			"d_str_n": x.DN, "d_edge_m_ha": x.DLine, "status": neQCStatus(x), "unknown": x.Unknown, "absent": x.Absent,
			"from": neVersionRef(c, x.From), "to": neVersionRef(c, x.To)})
		if len(out) >= 5000 {
			break
		}
	}
	return out
}

// handleNEChunksList — audit view of the log: versions per chunk (all, or one
// ?chunk=), optionally with the rows as hex (?rows=1).
func handleNEChunksList(w http.ResponseWriter, r *http.Request, kg string) {
	c := neChangeGet(kg)
	if c == nil {
		writeJSON(w, 200, map[string]interface{}{"kg": kg, "versions": []interface{}{}, "count": 0})
		return
	}
	only := r.URL.Query().Get("chunk")
	withRows := r.URL.Query().Get("rows") == "1"
	var out []map[string]interface{}
	for i, e := range c.entries {
		if only != "" && e.Chunk != only && e.Chunks[only] == "" {
			continue
		}
		m := map[string]interface{}{"i": i, "kind": e.Kind, "chunk": e.Chunk, "digest": e.Digest, "digest_ap": e.AP, "observer": e.Observer,
			"token": e.Token, "source": e.Source, "epoch": e.Epoch, "at": e.At, "rows": e.Rows, "sightings": c.seen[e.AP]}
		if e.TileHash != "" {
			m["tile_hash"] = e.TileHash
		}
		if e.Kind == "seen" || e.Kind == "tiles" {
			m["chunks"] = e.Chunks
		}
		if withRows && e.Kind == "version" {
			if rows, err := c.rows(i); err == nil {
				m["rows_hex"] = fmt.Sprintf("%x", rows)
			}
		}
		out = append(out, m)
	}
	resp := map[string]interface{}{"kg": kg, "versions": out, "count": len(out), "log_bytes": c.logBytes, "chunks": len(c.versions)}
	if s, ok := neChangeSummaryOf(kg); ok {
		resp["summary"] = s
	}
	writeJSON(w, 200, resp)
}

// handleNEChangeStats — GET /ne/stats[?kg=A,B|gemeinde=…]: the change{} block
// (public, tokenless; roll-ups only). Without a filter: national.
func handleNEChangeStats(w http.ResponseWriter, r *http.Request, rq *neReq) {
	q := r.URL.Query()
	var kgs []string
	switch {
	case q.Get("kg") != "":
		for _, k := range splitCSV(q.Get("kg")) {
			if neKGRe.MatchString(k) {
				kgs = append(kgs, k)
			}
		}
	case q.Get("gemeinde") != "" || q.Get("district") != "" || q.Get("state") != "":
		codes, _ := ResolveKGCodes(KGCodeFilter{Gemeinde: q.Get("gemeinde"), District: q.Get("district"), State: q.Get("state")})
		kgs = codes
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	if kgs == nil {
		nat := neChangeNational(false)
		writeJSON(w, 200, map[string]interface{}{"scope": "national", "change": nat, "notice": NENotice(), "unit": "m²; shares are 1/255 of the cell area; confirmed = digest seen twice; qc = cells excluded as pipeline (same tile hash), jitter (below the quantisation floor) or coverage (the chunk's cell set differs between the two digests), and how clustered the counted cells are"})
		return
	}
	if len(kgs) > 8000 { // a Bundesland is ≤ 3 040 KGs; the summaries are cached files, ~µs each
		apiError(w, "filter resolves to too many KGs", 400)
		return
	}
	tot := neChangeSummary{Schema: neChangeSummarySchema, KG: "*"}
	per := map[string]neChangeSummary{}
	for _, kg := range kgs {
		s, ok := neChangeSummaryOf(kg)
		if !ok {
			continue
		}
		per[kg] = s
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
	}
	out := map[string]interface{}{"scope": "kgs", "kgs": len(kgs), "kgs_with_log": len(per), "change": tot, "notice": NENotice()}
	if len(per) <= 50 {
		out["per_kg"] = per
	}
	writeJSON(w, 200, out)
}
