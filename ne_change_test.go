package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"
)

// neTestChunks — rows of 63330's lu layer grouped by res-10 chunk (first n chunks).
func neTestChunks(t *testing.T, lu []byte, n int) (order []uint64, rows map[uint64][]byte) {
	rows = map[uint64][]byte{}
	for i := 0; i+neLURowBytes <= len(lu); i += neLURowBytes {
		p := h3Parent(binary.LittleEndian.Uint64(lu[i:]), 10)
		if _, ok := rows[p]; !ok {
			if len(order) == n {
				continue
			}
			order = append(order, p)
		}
		rows[p] = append(rows[p], lu[i:i+neLURowBytes]...)
	}
	for p := range rows {
		found := false
		for _, o := range order {
			if o == p {
				found = true
			}
		}
		if !found {
			delete(rows, p)
		}
	}
	return
}

func neStrip(rows []byte) []byte {
	s := append([]byte(nil), rows...)
	for i := 0; i+neLURowBytes <= len(s); i += neLURowBytes {
		s[i+24], s[i+25] = 0, 0
	}
	return s
}

func neD16(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:])[:16] }

func neTestReport(observer string, rows map[uint64][]byte, withAP bool) map[string]interface{} {
	chunks, ap := map[string]string{}, map[string]string{}
	var all []byte
	keys := make([]uint64, 0, len(rows))
	for k := range rows {
		keys = append(keys, k)
	}
	sortU64(keys)
	for _, k := range keys {
		chunks[neCellHex(k)] = neD16(rows[k])
		ap[neCellHex(k)] = neD16(neStrip(rows[k]))
		all = append(all, rows[k]...)
	}
	rep := map[string]interface{}{"kg": "63330", "algo": "ne-cells-2", "epoch": "2026-10", "source": "bevdirect@v0.3.3", "observer": observer,
		"h3_res": 12, "chunk_res": 10, "cells_n": len(all) / neLURowBytes, "digest": neD16(all), "bbox": []float64{15.07, 47.09, 15.16, 47.16}, "chunks": chunks}
	if withAP {
		rep["chunks_ap"], rep["ap_version"] = ap, "ap-1"
	}
	return rep
}

func neNECH(rows map[uint64][]byte, only ...uint64) []byte {
	var b bytes.Buffer
	b.WriteString("NECH")
	var n [2]byte
	binary.LittleEndian.PutUint16(n[:], uint16(len(only)))
	b.Write(n[:])
	for _, c := range only {
		s := neStrip(rows[c])
		var h [12]byte
		binary.LittleEndian.PutUint64(h[:], c)
		binary.LittleEndian.PutUint32(h[8:], uint32(len(s)))
		b.Write(h[:])
		b.Write(s)
	}
	return b.Bytes()
}

func neKeysOf(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestNEChangeProtocol(t *testing.T) {
	env, cleanup := newNEPublicEnv(t)
	defer cleanup()
	neChangeLRU = &neChangeLRUT{kgs: map[string]*neChangeKG{}}
	lu, _ := neSrcRows(t, "63330")
	order, rows := neTestChunks(t, lu, 3)
	post := func(path string, body []byte, ctype string) (*httptest.ResponseRecorder, map[string]interface{}) {
		rr := env.do("POST", path, body, "Authorization", "Bearer "+env.tok, "Content-Type", ctype)
		var js map[string]interface{}
		json.Unmarshal(rr.Body.Bytes(), &js)
		return rr, js
	}
	report := func(rep map[string]interface{}) (*httptest.ResponseRecorder, map[string]interface{}) {
		b, _ := json.Marshal(rep)
		return post("/k/api/v1/ne/63330/report", b, "application/json")
	}
	// ── compatibility golden: a report WITHOUT chunks_ap answers exactly today's keys
	rr, js := report(neTestReport("obs-a", rows, false))
	// (the test store holds a bev section for 63330 → the "section" comparison path; keys as of 2026-10-08)
	golden := []string{"baseline", "chunks_changed", "chunks_compared", "chunks_reported", "chunks_same", "chunks_unknown_to_us", "compared", "coverage", "epoch", "identical_kg_digest", "identical_source", "kg", "observer", "our_digest", "our_epoch", "our_sources", "policy", "source", "stored", "token", "unchanged"}
	if rr.Code != 200 || !reflect.DeepEqual(neKeysOf(js), golden) {
		t.Fatalf("golden keys: %d %v", rr.Code, neKeysOf(js))
	}
	// ── step 1: same report with chunks_ap → every chunk wanted (never held)
	rr, js = report(neTestReport("obs-a", rows, true))
	want, _ := js["want_chunks"].([]interface{})
	if rr.Code != 200 || len(want) != 3 || js["unchanged"] != true {
		t.Fatalf("wants: %d %s", rr.Code, rr.Body.String())
	}
	// ── step 2: rows for the wanted chunks (+ one not wanted, + one with wrong bytes)
	bad := append([]byte(nil), rows[order[0]]...)
	bad[12] ^= 1
	body := neNECH(map[uint64][]byte{order[0]: rows[order[0]], order[1]: rows[order[1]], order[2]: bad}, order[0], order[1], order[2])
	rr, js = post("/k/api/v1/ne/63330/chunks?observer=obs-a", body, "application/octet-stream")
	if rr.Code != 200 || js["stored"].(float64) != 2 || js["rejected"].(float64) != 1 || js["chunks"].(map[string]interface{})[neCellHex(order[2])] != "digest_ap_mismatch" {
		t.Fatalf("chunks post: %d %s", rr.Code, rr.Body.String())
	}
	rr, js = post("/k/api/v1/ne/63330/chunks?observer=obs-a", neNECH(rows, order[2]), "application/octet-stream")
	if rr.Code != 200 || js["stored"].(float64) != 1 || js["want_remaining"].(float64) != 0 {
		t.Fatalf("chunks post 2: %d %s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(filepath.Join(env.dir, "63330.chunks.zst")); err != nil {
		t.Fatal(err)
	}
	// stored rows have zero register bytes and the log round-trips
	c := neChangeGet("63330")
	if c == nil || len(c.versions) != 3 {
		t.Fatalf("log: %+v", c)
	}
	for i := range c.entries {
		r, err := c.rows(i)
		if err != nil || !bytes.Equal(r, neStrip(rows[c.entries[i].chunk])) {
			t.Fatalf("rows %d: %v", i, err)
		}
	}
	// ── concurrent readers of one loaded KG (a tile burst): rows()/deltasOf() fill their
	// lazy caches under locks; this crashed with "concurrent map read and map write" before
	// 2026-10-29. Run with -race to see it. An append in between must not mutate the shared
	// object (copy-on-write: the LRU drops it, the next neChangeGet reloads).
	{
		fresh, err := neChangeLoad("63330")
		if err != nil {
			t.Fatal(err)
		}
		neChangeLRU.mu.Lock()
		neChangeLRU.putLocked(fresh)
		neChangeLRU.mu.Unlock()
		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				for i := range fresh.entries {
					if _, err := fresh.rows((i + g) % len(fresh.entries)); err != nil {
						t.Error(err)
					}
				}
				if _, err := fresh.deltasOf(); err != nil {
					t.Error(err)
				}
				_ = fresh.latestDigests()
			}(g)
		}
		wg.Wait()
		before := len(fresh.entries)
		rr, _ = report(neTestReport("obs-cow", rows, true))
		if rr.Code != 200 {
			t.Fatalf("cow report: %d %s", rr.Code, rr.Body.String())
		}
		if len(fresh.entries) != before {
			t.Fatalf("append mutated a shared object in place: %d → %d entries", before, len(fresh.entries))
		}
		if c2 := neChangeGet("63330"); c2 == fresh && c2.logBytes != fresh.logBytes {
			t.Fatal("LRU handed out a stale object after an append")
		}
	}
	// ── a register-only change (gk, n_parc) moves the frozen digest, not digest_ap → no wants
	reg := map[uint64][]byte{}
	for k, v := range rows {
		reg[k] = append([]byte(nil), v...)
	}
	reg[order[0]][24] ^= 0x7f
	reg[order[0]][25] ^= 0x01
	rr, js = report(neTestReport("obs-a", reg, true))
	want, _ = js["want_chunks"].([]interface{})
	if rr.Code != 200 || len(want) != 0 || js["chunks_held"].(float64) != 3 || js["chunks_confirmed"] != nil { // same observer, same pass: no sighting
		t.Fatalf("register-only: %d %s", rr.Code, rr.Body.String())
	}
	if !neLaterPass("2026-10-08T10:00:00Z", "2026-10-09T09:00:00Z") || neLaterPass("2026-10-08T10:00:00Z", "2026-10-08T20:00:00Z") {
		t.Fatalf("neLaterPass window")
	}
	// ── a land-use change in one cell of chunk 0: structure +10/255, green −10/255, +1 structure
	chg := map[uint64][]byte{}
	for k, v := range rows {
		chg[k] = append([]byte(nil), v...)
	}
	var cellRow []byte
	for i := 0; i+neLURowBytes <= len(chg[order[0]]); i += neLURowBytes { // a declared cell with ≥ 10/255 green
		r := chg[order[0]][i : i+neLURowBytes]
		if r[11]&1 == 0 && r[15] >= 10 && r[8] == 255 {
			cellRow = r
			break
		}
	}
	if cellRow == nil {
		t.Skip("no suitable cell in the first chunk")
	}
	cellID := binary.LittleEndian.Uint64(cellRow)
	cellRow[12] += 10
	cellRow[15] -= 10
	binary.LittleEndian.PutUint16(cellRow[22:], binary.LittleEndian.Uint16(cellRow[22:])+1)
	rr, js = report(neTestReport("obs-b", chg, true))
	want, _ = js["want_chunks"].([]interface{})
	if rr.Code != 200 || len(want) != 1 || want[0] != neCellHex(order[0]) {
		t.Fatalf("change wants: %d %s", rr.Code, rr.Body.String())
	}
	rr, js = post("/k/api/v1/ne/63330/chunks?observer=obs-b", neNECH(chg, order[0]), "application/octet-stream")
	deltas, _ := js["deltas"].([]interface{})
	if rr.Code != 200 || js["stored"].(float64) != 1 || len(deltas) != 1 {
		t.Fatalf("delta: %d %s", rr.Code, rr.Body.String())
	}
	d0 := deltas[0].(map[string]interface{})
	dlu := d0["d_lu_m2"].([]interface{})
	if d0["status"] != "candidate" || d0["cell"] != neCellHex(cellID) || dlu[0].(float64) <= 0 || dlu[3].(float64) >= 0 || d0["d_str_n"].(float64) != 1 {
		t.Fatalf("delta row: %v", d0)
	}
	// candidate only: the public cell shows status candidate, no m²
	rr = env.do("GET", "/api/v1/ne/cell/"+neCellHex(cellID), nil)
	var doc map[string]interface{}
	json.Unmarshal(rr.Body.Bytes(), &doc)
	cb, _ := doc["chg"].(map[string]interface{})
	if rr.Code != 200 || cb == nil || cb["status"] != "candidate" || cb["d_lu_m2"] != nil {
		t.Fatalf("public cell candidate: %d %s", rr.Code, rr.Body.String())
	}
	// ── second sighting (another observer, same digest_ap) → confirmed
	rr, js = report(neTestReport("obs-c", chg, true))
	if rr.Code != 200 || js["chunks_confirmed"].(float64) != 1 || len(js["want_chunks"].([]interface{})) != 0 {
		t.Fatalf("confirm: %d %s", rr.Code, rr.Body.String())
	}
	rr = env.do("GET", "/api/v1/ne/cell/"+neCellHex(cellID), nil)
	json.Unmarshal(rr.Body.Bytes(), &doc)
	cb, _ = doc["chg"].(map[string]interface{})
	if rr.Code != 200 || cb == nil || cb["status"] != "confirmed" || cb["d_lu_m2"].([]interface{})[0].(float64) <= 0 || cb["d_str_n"].(float64) != 1 {
		t.Fatalf("public cell confirmed: %d %s", rr.Code, rr.Body.String())
	}
	if bad := nePublicBannedWords(rr.Body.String()); len(bad) > 0 {
		t.Fatalf("banned words on /cell: %v", bad)
	}
	// chunk json with the chg layer
	rr = env.do("GET", fmt.Sprintf("/api/v1/ne/chunk/%s?format=json&layers=lu,chg", neCellHex(h3Parent(cellID, 9))), nil)
	json.Unmarshal(rr.Body.Bytes(), &doc)
	cm, _ := doc["chg"].(map[string]interface{})
	if rr.Code != 200 || cm == nil || cm["n"].(float64) != 1 {
		t.Fatalf("chunk chg: %d %s", rr.Code, rr.Body.String()[:200])
	}
	if bad := nePublicBannedWords(rr.Body.String()); len(bad) > 0 {
		t.Fatalf("banned words on /chunk: %v", bad)
	}
	// stats
	rr = env.do("GET", "/api/v1/ne/stats?kg=63330", nil)
	json.Unmarshal(rr.Body.Bytes(), &doc)
	ch, _ := doc["change"].(map[string]interface{})
	conf, _ := ch["confirmed"].(map[string]interface{})
	if rr.Code != 200 || conf == nil || conf["cells"].(float64) != 1 || conf["d_str_n"].(float64) != 1 || conf["transitions_m2"].(map[string]interface{})["green>structure"] == nil {
		t.Fatalf("stats: %d %s", rr.Code, rr.Body.String())
	}
	if bad := nePublicBannedWords(rr.Body.String()); len(bad) > 0 {
		t.Fatalf("banned words on /stats: %v", bad)
	}
	rr = env.do("GET", "/api/v1/ne/stats", nil)
	if rr.Code != 200 {
		t.Fatalf("national stats: %d", rr.Code)
	}
	// derived file exists and its summary matches the log
	if s, ok := neChangeSummaryOf("63330"); !ok || s.Confirmed.Cells != 1 || s.Versions != 4 {
		t.Fatalf("summary: %+v %v", s, ok)
	}
	// ── step 0: header only — equal → unchanged; different → 202 need chunks
	hdr := neTestReport("obs-c", chg, false)
	delete(hdr, "chunks")
	rr, js = report(hdr)
	if rr.Code != 200 || js["unchanged"] != true || js["header_only"] != true {
		t.Fatalf("header-only: %d %s", rr.Code, rr.Body.String())
	}
	hdr["digest"] = "0000000000000000"
	rr, js = report(hdr)
	if rr.Code != 202 || js["need"] != "chunks" {
		t.Fatalf("header-only new: %d %s", rr.Code, rr.Body.String())
	}
	// audit view lists versions and sightings
	rr = env.do("GET", "/k/api/v1/ne/63330/chunks?chunk="+neCellHex(order[0]), nil, "Authorization", "Bearer "+env.tok)
	json.Unmarshal(rr.Body.Bytes(), &doc)
	if rr.Code != 200 || doc["count"].(float64) < 3 {
		t.Fatalf("audit: %d %s", rr.Code, rr.Body.String()[:300])
	}
	// contributor path allow-list
	if !contributorPathAllowed("/api/v1/ne/63330/chunks") || isPublicPath("/api/v1/ne/63330/chunks") {
		t.Fatalf("tier split for /chunks")
	}
	// size line
	if st := NEChangeStats(); st["kgs"].(int) < 1 {
		t.Fatalf("stats line: %v", st)
	}
}

// TestNEChangeQC — docs/ne-qc-proposal.md: tile-hash provenance (same input ⇒
// "pipeline"), the quantisation floor ("jitter"), incomplete tile fetches
// (change_suspect) and the qc block of the summary.
func TestNEChangeQC(t *testing.T) {
	env, cleanup := newNEPublicEnv(t)
	defer cleanup()
	neChangeLRU = &neChangeLRUT{kgs: map[string]*neChangeKG{}}
	lu, _ := neSrcRows(t, "63330")
	order, rows := neTestChunks(t, lu, 2)
	post := func(path string, body []byte, ctype string) (*httptest.ResponseRecorder, map[string]interface{}) {
		rr := env.do("POST", path, body, "Authorization", "Bearer "+env.tok, "Content-Type", ctype)
		var js map[string]interface{}
		json.Unmarshal(rr.Body.Bytes(), &js)
		return rr, js
	}
	tiles := func(rep map[string]interface{}, tag string) map[string]interface{} {
		m := map[string]string{}
		for c := range rep["chunks"].(map[string]string) {
			m[c] = neD16([]byte(tag + c))
		}
		rep["chunks_tiles"] = m
		return rep
	}
	report := func(rep map[string]interface{}) (*httptest.ResponseRecorder, map[string]interface{}) {
		b, _ := json.Marshal(rep)
		return post("/k/api/v1/ne/63330/report", b, "application/json")
	}
	// incomplete fetch → change_suspect, nothing wanted
	rep := neTestReport("obs-a", rows, true)
	rep["tiles"] = map[string]int{"expected": 10, "fetched": 9}
	rr, js := report(rep)
	if rr.Code != 200 || js["change_suspect"] != "tiles_incomplete" || len(js["want_chunks"].([]interface{})) != 0 {
		t.Fatalf("tiles_incomplete: %d %s", rr.Code, rr.Body.String())
	}
	// baseline with tile hashes T1
	rr, js = report(tiles(neTestReport("obs-a", rows, true), "T1"))
	if rr.Code != 200 || len(js["want_chunks"].([]interface{})) != 2 || len(js["want_tiles"].(map[string]interface{})) != 2 {
		t.Fatalf("baseline wants: %d %s", rr.Code, rr.Body.String())
	}
	rr, js = post("/k/api/v1/ne/63330/chunks?observer=obs-a", neNECH(rows, order[0], order[1]), "application/octet-stream")
	if rr.Code != 200 || js["stored"].(float64) != 2 {
		t.Fatalf("baseline rows: %d %s", rr.Code, rr.Body.String())
	}
	c := neChangeGet("63330")
	if c == nil || c.entries[0].TileHash == "" || c.entries[0].TileHash != neD16([]byte("T1"+c.entries[0].Chunk)) {
		t.Fatalf("tile hash not in log: %+v", c.entries[0])
	}
	// pick a declared, interior cell of chunk 0 with some green
	find := func(m map[uint64][]byte) []byte {
		for i := 0; i+neLURowBytes <= len(m[order[0]]); i += neLURowBytes {
			r := m[order[0]][i : i+neLURowBytes]
			if r[11]&1 == 0 && r[15] >= 10 && r[8] == 255 {
				return r
			}
		}
		return nil
	}
	clone := func() map[uint64][]byte {
		m := map[uint64][]byte{}
		for k, v := range rows {
			m[k] = append([]byte(nil), v...)
		}
		return m
	}
	if find(rows) == nil {
		t.Skip("no suitable cell")
	}
	// ── real change (+10/255 structure, +1 structure) but the SAME tile hashes → pipeline
	same := clone()
	r := find(same)
	cell := binary.LittleEndian.Uint64(r)
	r[12] += 10
	r[15] -= 10
	binary.LittleEndian.PutUint16(r[22:], binary.LittleEndian.Uint16(r[22:])+1)
	rr, js = report(tiles(neTestReport("obs-b", same, true), "T1"))
	if rr.Code != 200 || js["qc_same_input"].(float64) != 1 || len(js["want_chunks"].([]interface{})) != 1 {
		t.Fatalf("same input report: %d %s", rr.Code, rr.Body.String())
	}
	rr, js = post("/k/api/v1/ne/63330/chunks?observer=obs-b", neNECH(same, order[0]), "application/octet-stream")
	deltas, _ := js["deltas"].([]interface{})
	if rr.Code != 200 || len(deltas) != 1 || deltas[0].(map[string]interface{})["status"] != "pipeline" {
		t.Fatalf("pipeline delta: %d %s", rr.Code, rr.Body.String())
	}
	sum := js["summary"].(map[string]interface{})
	qc := sum["qc"].(map[string]interface{})
	if qc["cells_pipeline"].(float64) != 1 || sum["candidate"].(map[string]interface{})["cells"].(float64) != 0 || qc["versions_with_tile_hash"].(float64) != 3 {
		t.Fatalf("qc after pipeline: %v", sum)
	}
	if rr = env.do("GET", "/api/v1/ne/cell/"+neCellHex(cell), nil); bytes.Contains(rr.Body.Bytes(), []byte(`"chg"`)) {
		t.Fatalf("pipeline delta must not reach the public cell: %s", rr.Body.String())
	}
	// ── jitter: new tiles T2, but only ±2/255 (≈ 2.4 m²) and no count change → jitter
	jit := clone()
	r = find(jit)
	r[12] += 10
	r[15] -= 10
	binary.LittleEndian.PutUint16(r[22:], binary.LittleEndian.Uint16(r[22:])+1) // = the "same" version…
	r[13] += 2                                                                  // …plus a 2/255 wobble
	r[15] -= 2
	rr, js = report(tiles(neTestReport("obs-c", jit, true), "T2"))
	if rr.Code != 200 || js["qc_same_input"] != nil || len(js["want_chunks"].([]interface{})) != 1 {
		t.Fatalf("jitter report: %d %s", rr.Code, rr.Body.String())
	}
	rr, js = post("/k/api/v1/ne/63330/chunks?observer=obs-c", neNECH(jit, order[0]), "application/octet-stream")
	deltas, _ = js["deltas"].([]interface{})
	if rr.Code != 200 || len(deltas) != 1 || deltas[0].(map[string]interface{})["status"] != "jitter" {
		t.Fatalf("jitter delta: %d %s", rr.Code, rr.Body.String())
	}
	qc = js["summary"].(map[string]interface{})["qc"].(map[string]interface{})
	if qc["cells_jitter"].(float64) != 1 || qc["cells_pipeline"].(float64) != 1 {
		t.Fatalf("qc after jitter: %v", qc)
	}
	// ── a real change on new tiles T3 → candidate, isolated (one cell)
	real := clone()
	r = find(real)
	r[12] += 10
	r[15] -= 10
	binary.LittleEndian.PutUint16(r[22:], binary.LittleEndian.Uint16(r[22:])+1)
	r[13] += 2
	r[15] -= 2
	r[14] += 20
	r[15] -= 20
	binary.LittleEndian.PutUint16(r[22:], binary.LittleEndian.Uint16(r[22:])+1)
	rr, js = report(tiles(neTestReport("obs-d", real, true), "T3"))
	if rr.Code != 200 || len(js["want_chunks"].([]interface{})) != 1 {
		t.Fatalf("real report: %d %s", rr.Code, rr.Body.String())
	}
	rr, js = post("/k/api/v1/ne/63330/chunks?observer=obs-d", neNECH(real, order[0]), "application/octet-stream")
	deltas, _ = js["deltas"].([]interface{})
	if rr.Code != 200 || len(deltas) != 1 || deltas[0].(map[string]interface{})["status"] != "candidate" {
		t.Fatalf("candidate delta: %d %s", rr.Code, rr.Body.String())
	}
	sum = js["summary"].(map[string]interface{})
	qc = sum["qc"].(map[string]interface{})
	if sum["candidate"].(map[string]interface{})["cells"].(float64) != 1 || qc["cells_isolated"].(float64) != 1 || qc["cluster_share"].(float64) != 0 {
		t.Fatalf("qc after real: %v", sum)
	}
	// stats roll-up carries qc; wants survive a restart with their tile hash
	rr = env.do("GET", "/api/v1/ne/stats?kg=63330", nil)
	if rr.Code != 200 || !bytes.Contains(rr.Body.Bytes(), []byte(`"cells_pipeline":1`)) || !bytes.Contains(rr.Body.Bytes(), []byte(`"cells_jitter":1`)) {
		t.Fatalf("stats qc: %d %s", rr.Code, rr.Body.String())
	}
	if bad := nePublicBannedWords(rr.Body.String()); len(bad) > 0 {
		t.Fatalf("banned words on /stats: %v", bad)
	}
	rr, js = report(tiles(neTestReport("obs-e", same, true), "T9"))
	neWantMu.Lock()
	neWants, neWantOrder = map[string]map[string]neWant{}, nil
	neWantMu.Unlock()
	if w := neWantGet("63330", "ops", "obs-e"); len(w) != 1 || w[neCellHex(order[0])].Tile != neD16([]byte("T9"+neCellHex(order[0]))) {
		t.Fatalf("want restore: %+v", w)
	}
}

// TestNEChangeTilesAttachLate — a KG baselined before peers sent provenance
// gets the tile hash from the next re-sighting that carries one (kind=tiles
// log entry, append-only); the next real version with the same hash is then
// a pipeline defect. The attachment survives a reload of the log.
func TestNEChangeTilesAttachLate(t *testing.T) {
	env, cleanup := newNEPublicEnv(t)
	defer cleanup()
	neChangeLRU = &neChangeLRUT{kgs: map[string]*neChangeKG{}}
	lu, _ := neSrcRows(t, "63330")
	order, rows := neTestChunks(t, lu, 2)
	post := func(path string, body []byte, ctype string) (*httptest.ResponseRecorder, map[string]interface{}) {
		rr := env.do("POST", path, body, "Authorization", "Bearer "+env.tok, "Content-Type", ctype)
		var js map[string]interface{}
		json.Unmarshal(rr.Body.Bytes(), &js)
		return rr, js
	}
	tiles := func(rep map[string]interface{}, tag string) map[string]interface{} {
		m := map[string]string{}
		for c := range rep["chunks"].(map[string]string) {
			m[c] = neD16([]byte(tag + c))
		}
		rep["chunks_tiles"] = m
		return rep
	}
	report := func(rep map[string]interface{}) (*httptest.ResponseRecorder, map[string]interface{}) {
		b, _ := json.Marshal(rep)
		return post("/k/api/v1/ne/63330/report", b, "application/json")
	}
	// baseline WITHOUT provenance (the fleet before v0.3.4)
	rr, js := report(neTestReport("obs-old", rows, true))
	if rr.Code != 200 || len(js["want_chunks"].([]interface{})) != 2 || js["want_tiles"] != nil {
		t.Fatalf("baseline: %d %s", rr.Code, rr.Body.String())
	}
	if rr, js = post("/k/api/v1/ne/63330/chunks?observer=obs-old", neNECH(rows, order[0], order[1]), "application/octet-stream"); js["stored"].(float64) != 2 {
		t.Fatalf("baseline rows: %s", rr.Body.String())
	}
	// identical build from a provenance-capable peer: nothing wanted, hashes attached
	rr, js = report(tiles(neTestReport("obs-new", rows, true), "T1"))
	if rr.Code != 200 || len(js["want_chunks"].([]interface{})) != 0 || js["tiles_attached"].(float64) != 2 {
		t.Fatalf("attach: %d %s", rr.Code, rr.Body.String())
	}
	neChangeLRU = &neChangeLRUT{kgs: map[string]*neChangeKG{}} // reload from disk
	c := neChangeGet("63330")
	for _, ch := range order {
		vs := c.versions[ch]
		if e := c.entries[vs[len(vs)-1]]; e.TileHash != neD16([]byte("T1"+neCellHex(ch))) {
			t.Fatalf("hash not attached after reload: %+v", e)
		}
	}
	if sum := c.summary(mustDeltas(t, c)); sum.QC.WithTileHash != 2 || sum.Versions != 2 {
		t.Fatalf("summary: %+v", sum.QC)
	}
	// a second report with hashes attaches nothing more (already held)
	if _, js = report(tiles(neTestReport("obs-new2", rows, true), "T9")); js["tiles_attached"] != nil {
		t.Fatalf("re-attach: %v", js["tiles_attached"])
	}
	// now a different digest_ap under the SAME hash T1 → pipeline
	same := map[uint64][]byte{}
	for k, v := range rows {
		same[k] = append([]byte(nil), v...)
	}
	r := same[order[0]][:neLURowBytes]
	r[12] += 10
	r[15] -= 10
	rr, js = report(tiles(neTestReport("obs-b", same, true), "T1"))
	if rr.Code != 200 || js["qc_same_input"].(float64) != 1 {
		t.Fatalf("same input: %d %s", rr.Code, rr.Body.String())
	}
}

func mustDeltas(t *testing.T, c *neChangeKG) *neChangeDeltas {
	t.Helper()
	d, err := c.deltasOf()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestNEChangeCoveragePerChunk — per-chunk coverage rule (docs/ne-cells.md
// §coverage, 2026-10-24): a chunk version whose cell SET differs from the
// previous one (a build truncated by a missing tile, or two observers'
// domains) is status "coverage" — listed, never cumulated, never a candidate,
// so a viewport observer's own next pass cannot confirm it as change. The
// complete build that follows re-baselines by the same rule.
func TestNEChangeCoveragePerChunk(t *testing.T) {
	env, cleanup := newNEPublicEnv(t)
	defer cleanup()
	neChangeLRU = &neChangeLRUT{kgs: map[string]*neChangeKG{}}
	lu, _ := neSrcRows(t, "63330")
	order, rows := neTestChunks(t, lu, 2)
	tc, oc := order[1], order[0] // tc: the chunk we truncate (63330's second chunk has interior cells); oc: untouched
	if len(rows[tc]) < 4*neLURowBytes {
		t.Skip("chunk too small")
	}
	post := func(path string, body []byte) (*httptest.ResponseRecorder, map[string]interface{}) {
		rr := env.do("POST", path, body, "Authorization", "Bearer "+env.tok, "Content-Type", "application/octet-stream")
		var js map[string]interface{}
		json.Unmarshal(rr.Body.Bytes(), &js)
		return rr, js
	}
	report := func(rep map[string]interface{}) (*httptest.ResponseRecorder, map[string]interface{}) {
		b, _ := json.Marshal(rep)
		return post("/k/api/v1/ne/63330/report", b)
	}
	// srtm baseline (cell block)
	rr, js := report(neTestReport("peer-vm-1", rows, true))
	if rr.Code != 200 || len(js["want_chunks"].([]interface{})) != 2 {
		t.Fatalf("baseline: %d %s", rr.Code, rr.Body.String())
	}
	if rr, js = post("/k/api/v1/ne/63330/chunks?observer=peer-vm-1", neNECH(rows, order[0], order[1])); rr.Code != 200 || js["stored"].(float64) != 2 {
		t.Fatalf("baseline rows: %d %s", rr.Code, rr.Body.String())
	}
	// mapTok, wider viewport, chunk 0 truncated: the last two cells missing,
	// one remaining cell with less green (the parcel clipped at the tile edge)
	trunc := map[uint64][]byte{oc: rows[oc]}
	c0 := append([]byte(nil), rows[tc][:len(rows[tc])-2*neLURowBytes]...)
	clipped := false
	for i := 0; i+neLURowBytes <= len(c0); i += neLURowBytes {
		if c0[i+8] < 255 {
			continue
		}
		for j := 12; j < 21 && !clipped; j++ {
			if c0[i+j] >= 20 {
				c0[i+j] -= 20
				c0[i+12+(j-12+1)%9] += 20
				clipped = true
			}
		}
		if clipped {
			break
		}
	}
	if !clipped {
		t.Skip("no interior cell to clip")
	}
	trunc[tc] = c0
	rep := neTestReport("map-peer", trunc, true)
	rep["bbox"] = []float64{15.06, 47.08, 15.18, 47.18}
	rep["cells_n"] = rep["cells_n"].(int) + 500 // the wider box holds more cells overall — not lossy by cells_n
	rr, js = report(rep)
	if rr.Code != 200 || js["change_suspect"] != nil || len(js["want_chunks"].([]interface{})) != 1 || js["coverage"].(map[string]interface{})["status"] != "best" {
		t.Fatalf("truncated viewport report: %d %s", rr.Code, rr.Body.String())
	}
	rr, js = post("/k/api/v1/ne/63330/chunks?observer=map-peer", neNECH(trunc, tc))
	deltas, _ := js["deltas"].([]interface{})
	if rr.Code != 200 || js["stored"].(float64) != 1 || len(deltas) < 1 {
		t.Fatalf("truncated rows: %d %s", rr.Code, rr.Body.String())
	}
	for _, d := range deltas {
		if d.(map[string]interface{})["status"] != "coverage" {
			t.Fatalf("truncated chunk delta must be status coverage: %v", d)
		}
	}
	sum := js["summary"].(map[string]interface{})
	qc := sum["qc"].(map[string]interface{})
	if sum["candidate"].(map[string]interface{})["cells"].(float64) != 0 || qc["cells_coverage"].(float64) < 1 {
		t.Fatalf("coverage pair must not be a candidate: %v", sum)
	}
	// map peer's own next pass (> 20 h later) with the same truncated build: a
	// sighting of the version, but no candidate exists to confirm
	neChangeGet("63330").entries[2].At = "2026-10-01T00:00:00Z"
	rr, js = report(rep)
	if rr.Code != 200 || js["chunks_held"].(float64) != 2 {
		t.Fatalf("second pass: %d %s", rr.Code, rr.Body.String())
	}
	s, _ := neChangeSummaryOf("63330")
	if s.Confirmed.Cells != 0 || s.Candidate.Cells != 0 || s.QC.Coverage < 1 {
		t.Fatalf("confirmed change from a truncated chunk: %+v", s)
	}
	// revisit hints: nothing pending for this KG
	rr = env.do("GET", "/k/api/v1/ne/revisit?refresh=1", nil, "Authorization", "Bearer "+env.tok)
	if rr.Code != 200 || bytes.Contains(rr.Body.Bytes(), []byte(`"pending_versions":1`)) {
		t.Fatalf("revisit must not list a coverage version as pending: %d %s", rr.Code, rr.Body.String())
	}
	// the complete build again (srtm): a new version against the truncated one → coverage, not "land gained"
	rr, js = report(neTestReport("peer-vm-2", rows, true))
	if rr.Code != 200 || len(js["want_chunks"].([]interface{})) != 1 {
		t.Fatalf("complete again: %d %s", rr.Code, rr.Body.String())
	}
	rr, js = post("/k/api/v1/ne/63330/chunks?observer=peer-vm-2", neNECH(rows, tc))
	deltas, _ = js["deltas"].([]interface{})
	if rr.Code != 200 || len(deltas) < 1 {
		t.Fatalf("complete rows: %d %s", rr.Code, rr.Body.String())
	}
	for _, d := range deltas {
		if d.(map[string]interface{})["status"] != "coverage" {
			t.Fatalf("re-baseline delta must be status coverage: %v", d)
		}
	}
	sum = js["summary"].(map[string]interface{})
	if sum["candidate"].(map[string]interface{})["cells"].(float64) != 0 || sum["confirmed"].(map[string]interface{})["cells"].(float64) != 0 {
		t.Fatalf("no change may come out of truncation: %v", sum)
	}
	// a real change on the same cell set afterwards is a plain candidate
	real := map[uint64][]byte{tc: append([]byte(nil), rows[tc]...), oc: rows[oc]}
	r := real[tc]
	for i := 0; i+neLURowBytes <= len(r); i += neLURowBytes {
		if r[i+11]&1 == 0 && r[i+15] >= 30 && r[i+8] == 255 {
			r[i+12] += 30
			r[i+15] -= 30
			binary.LittleEndian.PutUint16(r[i+22:], binary.LittleEndian.Uint16(r[i+22:])+1)
			break
		}
	}
	rr, js = report(neTestReport("peer-vm-3", real, true))
	if rr.Code != 200 || len(js["want_chunks"].([]interface{})) != 1 {
		t.Skipf("no suitable cell for a real change: %s", rr.Body.String())
	}
	rr, js = post("/k/api/v1/ne/63330/chunks?observer=peer-vm-3", neNECH(real, tc))
	deltas, _ = js["deltas"].([]interface{})
	if rr.Code != 200 || len(deltas) != 1 || deltas[0].(map[string]interface{})["status"] != "candidate" {
		t.Fatalf("real change after re-baseline: %d %s", rr.Code, rr.Body.String())
	}
	// stats roll-up carries the new qc counter, word ban holds
	rr = env.do("GET", "/api/v1/ne/stats?kg=63330", nil)
	if rr.Code != 200 || !bytes.Contains(rr.Body.Bytes(), []byte(`"cells_coverage":`)) {
		t.Fatalf("stats qc: %d %s", rr.Code, rr.Body.String())
	}
	if bad := nePublicBannedWords(rr.Body.String()); len(bad) > 0 {
		t.Fatalf("banned words on /stats: %v", bad)
	}
}
