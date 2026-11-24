package main

import (
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestNEDifKind — the painting rule of the diff overlay (ne-change.md §6d):
// status filter, the three corroborated kinds, raw, and the apex guard.
func TestNEDifKind(t *testing.T) {
	lay := neObsLayout(neObsFieldsV41, "<Q9sBBBBBBBBBBbBBHBBBBBbbBB")
	if lay == nil || lay.dh < 0 || lay.hansen < 0 || lay.verdict < 0 || lay.hMean < 0 {
		t.Fatalf("layout: %+v", lay)
	}
	lu := func(share, land byte, groups ...byte) []byte {
		r := make([]byte, 30)
		r[8], r[9] = share, land
		copy(r[12:21], groups)
		return r
	}
	obs := func(groups []byte, dh int8, hansen byte, treeN byte) []byte {
		r := make([]byte, 41)
		copy(r[lay.luOff:], groups)
		r[lay.dh] = byte(dh)
		r[lay.hansen] = hansen
		r[lay.hMean] = 10
		r[lay.treeN], r[lay.treeH] = treeN, 40
		r[lay.flight] = 1
		r[lay.crown] = 200
		return r
	}
	forest := []byte{0, 0, 0, 0, 255, 0, 0, 0, 0}
	grass := []byte{0, 0, 0, 255, 0, 0, 0, 0, 0}
	built := []byte{200, 0, 0, 55, 0, 0, 0, 0, 0}
	cases := []struct {
		name   string
		lu, o  []byte
		raw    bool
		status string
		kind   int
	}{
		{"agree", lu(255, 255, forest...), obs(forest, 0, 0, 0), false, "", neDifKindNone},
		{"edge", lu(100, 255, forest...), obs(grass, 0, 0, 0), false, "edge", neDifKindNone},
		{"declared built, none seen", lu(255, 255, built...), obs(grass, 0, 0, 0), false, "", neDifKindDeclared},
		{"seen built, undeclared, no rise", lu(255, 255, grass...), obs(built, 0, 0, 0), false, "", neDifKindNone},
		{"seen built, undeclared, rise", lu(255, 255, grass...), obs(built, 12, 0, 0), false, "", neDifKindAppeared},
		{"seen built, undeclared, raw", lu(255, 255, grass...), obs(built, 0, 0, 0), true, "", neDifKindAppeared},
		{"forest open, no evidence", lu(255, 255, forest...), obs(grass, 0, 0, 0), false, "", neDifKindNone},
		{"forest open, loss year", lu(255, 255, forest...), obs(grass, 0, 19, 0), false, "", neDifKindCut},
		{"forest open, drop", lu(255, 255, forest...), obs(grass, -10, 0, 0), false, "", neDifKindCut},
		{"forest open, loss year but apices", lu(255, 255, forest...), obs(grass, 0, 19, 5), false, "", neDifKindNone},
		{"forest open, raw", lu(255, 255, forest...), obs(grass, 0, 0, 0), true, "", neDifKindCut},
		{"forest open, raw, apices", lu(255, 255, forest...), obs(grass, 0, 0, 5), true, "", neDifKindNone},
		{"vocabulary only (crop vs grass)", lu(255, 255, 0, 0, 255), obs(grass, 0, 0, 0), true, "", neDifKindNone},
	}
	for _, c := range cases {
		if st := neDifStatus(c.lu, c.o, lay); st != c.status {
			t.Errorf("%s: status %q, want %q", c.name, st, c.status)
			continue
		}
		if c.status != "" {
			continue
		}
		if k := neDifKind(c.lu, c.o, lay, c.raw, true); k != c.kind {
			t.Errorf("%s: kind %d, want %d", c.name, k, c.kind)
		}
	}
	// one scan only: the height change is not a measurement — no rise, no drop; Hansen still counts
	if k := neDifKind(lu(255, 255, grass...), obs(built, 12, 0, 0), lay, false, false); k != neDifKindNone {
		t.Errorf("rise in a cell not re-flown: kind %d", k)
	}
	if k := neDifKind(lu(255, 255, forest...), obs(grass, -10, 0, 0), lay, false, false); k != neDifKindNone {
		t.Errorf("drop in a cell not re-flown: kind %d", k)
	}
	if k := neDifKind(lu(255, 255, forest...), obs(grass, 0, 19, 0), lay, false, false); k != neDifKindCut {
		t.Errorf("loss year in a cell not re-flown: kind %d", k)
	}
	o := obs(grass, 0, 0, 0)
	o[lay.verdict] = neDifVerdictUnkn
	if st := neDifStatus(lu(255, 255, forest...), o, lay); st != "obs_null" {
		t.Errorf("verdict unknown: %q", st)
	}
	var m neGlyphDifMeta
	m.raw = true
	m.kinds = [4]int{0, 1, 2, 3}
	if s := m.String(); s != ";dif=1,2,3;cmp=0;raw=1" {
		t.Errorf("meta: %q", s)
	}
}

// TestNEDifEpochETag — the epoch table revalidates with If-None-Match and keeps
// its data (and its stamp) on 304.
func TestNEDifEpochETag(t *testing.T) {
	hits, conds := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Header.Get("If-None-Match") == `"v1"` {
			conds++
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Write([]byte(`{"kgs":[{"kg_code":"63330","als":{"newest":2022,"oldest":2010,"dominant":2022,"dominant_share":0.97,"precision":"year"}}]}`))
	}))
	defer srv.Close()
	old := neDifPeerURL
	neDifPeerURL = srv.URL
	defer func() { neDifPeerURL = old }()
	e := &neDifEpoch
	e.mu.Lock()
	e.m, e.etag, e.at, e.fresh = nil, "", time.Time{}, time.Time{}
	e.mu.Unlock()
	neDifFetch()
	a, ok := neDifAlsOf("63330")
	if !ok || a.Newest != 2022 || e.etag != `"v1"` {
		t.Fatalf("first fetch: %+v %v etag=%q err=%q", a, ok, e.etag, e.err)
	}
	stamp := neDifEpochStamp()
	neDifFetch()
	if hits != 2 || conds != 1 || e.n304 != 1 || e.err != "" {
		t.Fatalf("revalidate: hits=%d conds=%d n304=%d err=%q", hits, conds, e.n304, e.err)
	}
	if _, ok := neDifAlsOf("63330"); !ok || neDifEpochStamp() != stamp {
		t.Fatalf("304 must keep the table and its stamp")
	}
}

// TestNEAlsGate — the re-flown table from the peer's sample body: the plain
// gate (newest > oldest, both known), the until-healed pair list, the lookup
// by any descendant of the res-10 cell.
func TestNEAlsGate(t *testing.T) {
	ids := []uint64{0x8c1e15ad2c001ff, 0x8c1e15ad2c401ff, 0x8c1e15ad2c801ff, 0x8c1e15ad2cc01ff}
	yrs := [][2]uint16{{2019, 2008}, {2012, 2012}, {2012, 2008}, {65535, 2008}} // newest, oldest
	hdr := fmt.Sprintf(`{"n":4,"gen":"g1","ids":"delta_u64","columns":[{"name":"id","dtype":"u64","bytes":32},{"name":"dh_m","dtype":"i8","bytes":4},{"name":"als_year_newest","dtype":"u16","bytes":8},{"name":"als_year_oldest","dtype":"u16","bytes":8}]}`)
	body := append([]byte("NEX1"), binary.LittleEndian.AppendUint32(nil, uint32(len(hdr)))...)
	body = append(body, hdr...)
	prev := uint64(0)
	for _, id := range ids {
		body = binary.LittleEndian.AppendUint64(body, id-prev)
		prev = id
	}
	body = append(body, 0, 0, 0, 0)
	for c := 0; c < 2; c++ {
		for _, y := range yrs {
			body = binary.LittleEndian.AppendUint16(body, y[c])
		}
	}
	g, err := neAlsGateParse(body, nil)
	if err != nil || len(g.ids) != 2 || g.rows != 3 || g.gen != "g1" {
		t.Fatalf("plain gate: %+v %v", g, err)
	}
	if o, n, ok := g.reflown(ids[0]); !ok || o != 2008 || n != 2019 {
		t.Errorf("cell 0: %d %d %v", o, n, ok)
	}
	if g.is(ids[1]) || !g.is(ids[2]) || g.is(ids[3]) {
		t.Errorf("gate: same year %v, 2008→2012 %v, unknown %v", g.is(ids[1]), g.is(ids[2]), g.is(ids[3]))
	}
	h, err := neAlsGateParse(body, [][2]int{{2008, 2012}})
	if err != nil || len(h.ids) != 1 || h.pairs != 1 || h.is(ids[2]) || h.hash == g.hash {
		t.Fatalf("pair list: %+v %v", h, err)
	}
	var none *neAlsGateT
	if none.is(ids[0]) {
		t.Error("no table must answer: not re-flown")
	}
	if _, err := neAlsGateParse(body[:40], nil); err == nil {
		t.Error("truncated body accepted")
	}
}
