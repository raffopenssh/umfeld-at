package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTreeFrameToken(t *testing.T) {
	for s, ok := range map[string]bool{"p0": true, "p-5+5": true, "p+10-3": true, "": false, "x": false, "p": false, "p0 ": false, "p<script>": false} {
		if (treeFrameToken(s) != "") != ok {
			t.Errorf("token %q: ok=%v", s, !ok)
		}
	}
}

// a row of an export carries its frame back; a drawn point never does
func TestTreeMailFrameField(t *testing.T) {
	r := tmTree(1, "Buche", "t16158400_48262400", "26", "6")
	r.attrs["frame"] = "p-5+5"
	its, why := tmDiff(r, "m", time.Now())
	if why != "" || len(its) != 1 || its[0].body["frame"] != "p-5+5" || !its[0].apex || its[0].h0 != 26 {
		t.Fatalf("export row: %v %+v", why, its)
	}
	r.attrs["frame"] = "nonsense"
	if its, _ = tmDiff(r, "m", time.Now()); len(its) != 1 || its[0].body["frame"] != nil {
		t.Fatalf("a bad token must not travel: %+v", its)
	}
	its, _ = tmDiff(tmRow{n: 1, name: "Buche", lon: 16.2, lat: 48.2, hasPt: true, attrs: map[string]string{"frame": "p-5+5"}}, "m", time.Now())
	if len(its) != 1 || its[0].body["frame"] != nil || its[0].apex {
		t.Fatalf("a drawn point has no frame: %+v", its)
	}
	// the field is ours, never a note
	rows, err := tmParseAttachment("a.csv", []byte("id,name,h,frame\nt16158400_48262400,Buche,26,p-5+5\n"))
	if err != nil || len(rows) != 1 || rows[0].attrs["frame"] != "p-5+5" || len(rows[0].extra) != 0 {
		t.Fatalf("csv: %v %+v", err, rows)
	}
}

// a file without frames: the peer is asked; the LiDAR height places a row in
// today's frame when position and height are an apex of today's copy.
func TestTreeMailFramesByHeight(t *testing.T) {
	asked := 0
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var q struct {
			Points [][2]float64 `json:"points"`
		}
		json.Unmarshal(b, &q)
		asked += len(q.Points)
		io.WriteString(w, `{"points":2,"kgs":{"01202":{"frame":"p0","current":"p-5+5","stale_hits":1,"current_hits":1,"confident":false}}}`)
	}))
	defer peer.Close()
	oldU, oldP := treeFrameURL, tmFramePiece
	defer func() { treeFrameURL, tmFramePiece = oldU, oldP }()
	treeFrameURL = func() string { return peer.URL }
	fi := neOGCFrames.idx("p-5+5")
	tmFramePiece = func(i, j int) ([]byte, bool) {
		var rec [neOGCRow]byte
		binary.LittleEndian.PutUint32(rec[0:], uint32(16158400))
		binary.LittleEndian.PutUint32(rec[4:], uint32(48262400))
		rec[8], rec[12] = 26*4, fi
		return rec[:], true
	}
	mk := func(lon, lat, h float64) tmItem {
		return tmItem{apex: true, h0: h, body: map[string]interface{}{"lon": lon, "lat": lat}}
	}
	items := []tmItem{
		mk(16.1584, 48.2624, 26),   // today's apex, same height → today's frame
		mk(16.1584, 48.2624, 19),   // same place, another height → not that tree: undecided
		mk(16.15846, 48.26236, 26), // 7 m off: the copy before the correction; the peer is unsure → nothing
		{apex: true, body: map[string]interface{}{"lon": 16.1, "lat": 48.2, "frame": "p0"}}, // carries its own
		{body: map[string]interface{}{"lon": 16.1584, "lat": 48.2624}},                      // a drawn point
	}
	tmFrames(context.Background(), items)
	if asked != 3 {
		t.Fatalf("asked the peer about %d points, want 3", asked)
	}
	want := []interface{}{"p-5+5", nil, nil, "p0", nil}
	for i, w := range want {
		if items[i].body["frame"] != w {
			t.Errorf("item %d: frame %v, want %v", i, items[i].body["frame"], w)
		}
	}
	// no KG listed = never moved: nothing is added
	peer2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"points":1,"kgs":{}}`) }))
	defer peer2.Close()
	treeFrameURL = func() string { return peer2.URL }
	one := []tmItem{mk(16.1584, 48.2624, 26)}
	if tmFrames(context.Background(), one); one[0].body["frame"] != nil {
		t.Fatalf("never moved: %v", one[0].body)
	}
}

// the peer's frame{} of a batch answer reaches the result
func TestTreeMailFrameAnswer(t *testing.T) {
	var ans struct {
		Results []tmPeerItem `json:"results"`
	}
	json.Unmarshal([]byte(`{"results":[{"http":201,"tree":{"frame":{"frame":"p-5+5","sent":"p0","by":"frame","moved_m":[-5,5]}}},{"http":201,"frame":{"ambiguous":true,"hint":"send frame"}},{"http":200}]}`), &ans)
	if f := ans.Results[0].Tree.Frame; f == nil || len(f.Moved) != 2 || ans.Results[1].Frame == nil || !ans.Results[1].Frame.Ambiguous || ans.Results[2].Frame != nil {
		t.Fatalf("%+v", ans)
	}
}

// the glyph sends the frame of its copy and reads the answer
func TestGlyphFrameJS(t *testing.T) {
	js := neGlyphTreeRepJS + neGlyphTreeMergedJS
	for _, s := range []string{"trFrameOf(a,body)", "trFrameAns(a,j)", "trFrameRev(j)", "e.fr=j.frames||null", ":'p0'"} {
		if !strings.Contains(js, s) {
			t.Errorf("glyph JS lost %q", s)
		}
	}
}
