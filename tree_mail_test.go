package main

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func tmTestFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "tree_mail", name))
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(name, ".gz") {
		zr, _ := gzip.NewReader(bytes.NewReader(b))
		b, _ = io.ReadAll(zr)
	}
	return b
}

func tmDiffAll(rows []tmRow) (items []tmItem, same int, refused map[string]int) {
	its, sm, ref := tmDiffRows(rows, "mtest", time.Now())
	r2 := map[string]int{}
	for k, v := range ref {
		r2[k] = len(v)
	}
	return its, sm, r2
}

func tmDiffAllOld(rows []tmRow) (items []tmItem, same int, refused map[string]int) {
	refused = map[string]int{}
	for _, r := range rows {
		its, why := tmDiff(r, "mtest", time.Now())
		switch {
		case why != "":
			refused[why]++
		case len(its) == 0:
			same++
		}
		items = append(items, its...)
	}
	return
}

// The two files a phone sent back (Locus Map export of a QGIS view, two names
// edited by hand) and the QGIS file they came from.
func TestTreeMailSampleFiles(t *testing.T) {
	for _, f := range []struct{ name, as string }{{"Baume.kmz", "Baume.kmz"}, {"Baume.csv.gz", "Baume.csv"}} {
		rows, err := tmParseAttachment(f.as, tmTestFile(t, f.name))
		if err != nil {
			t.Fatalf("%s: %v", f.name, err)
		}
		if len(rows) != 8643 {
			t.Fatalf("%s: %d rows, want 8643", f.name, len(rows))
		}
		items, same, refused := tmDiffAll(rows)
		if len(items) != 2 || same != 8641 || len(refused) != 0 {
			t.Fatalf("%s: %d edits, %d unchanged, refused %v", f.name, len(items), same, refused)
		}
		got := map[string]string{}
		for _, it := range items {
			b, _ := json.Marshal(it.body)
			got[it.name] = string(b)
			if it.add {
				t.Errorf("%s: %q has an id, must not be an add", f.name, it.name)
			}
		}
		// "Fichte" on the 11 m tree, "12 m buche" on the 12.25 m tree: species only, the height is the export's own
		if g := got["Fichte"]; g != `{"claim":"tree","lat":48.26372,"layer":"trees","lon":16.160198,"reporter":"mtest","species":"Fichte"}` {
			t.Errorf("%s Fichte: %s", f.name, g)
		}
		if g := got["12 m buche"]; g != `{"claim":"tree","lat":48.264495,"layer":"trees","lon":16.163266,"reporter":"mtest","species":"buche"}` {
			t.Errorf("%s buche: %s", f.name, g)
		}
	}
	rows, err := tmParseAttachment("view.kml", tmTestFile(t, "qgis_view.kml"))
	if err != nil || len(rows) != 40 {
		t.Fatalf("qgis: %d rows, %v", len(rows), err)
	}
	if rows[1].attrs["id"] != "t16158249_48262484" || rows[1].attrs["species"] != "spruce" || rows[1].name != "18 m spruce" {
		t.Fatalf("qgis row: %+v", rows[1])
	}
	if items, same, refused := tmDiffAll(rows); len(items) != 0 || same != 40 || len(refused) != 0 {
		t.Fatalf("qgis untouched: %d edits, refused %v", len(items), refused)
	}
}

func TestTreeMailNames(t *testing.T) {
	base := map[string]string{"id": "t16158249_48262484", "h": "18.25", "species": "spruce"}
	for name, want := range map[string]string{
		"18 m spruce":            "",
		"18 M Spruce":            "",
		"Fichte":                 "species Fichte",
		"18 m Fichte":            "species Fichte",
		"25 m":                   "height 25 m",
		"25m spruce":             "height 25 m",
		"18,5 m":                 "height 18.5 m",
		"Fichte 25 m bhd 40 cm":  "species Fichte · height 25 m · diameter 40 cm",
		"bhd 40":                 "diameter 40 cm",
		"gefällt 2025":           "state logged 2025",
		"18 m spruce gefällt":    "state logged",
		"Windwurf 8.2025":        "state windthrow 2025-08",
		"tot":                    "state dead",
		"Käfer 14.8.2025":        "state infested 2025-08-14",
		"kein Baum":              "not a tree",
		"not a tree":             "not a tree",
		"passt":                  "looks right",
		"Picea abies":            "species Picea abies",
		"Rot-Buche 20 m gefällt": "species Rot-Buche · height 20 m · state logged",
	} {
		its, why := tmDiff(tmRow{n: 1, name: name, lon: 16.158249, lat: 48.262484, hasPt: true, attrs: base}, "m1", time.Now())
		got := why
		if why == "" {
			var w []string
			for _, it := range its {
				w = append(w, it.what)
			}
			got = strings.Join(w, " + ")
		}
		if got != want {
			t.Errorf("%q: got %q, want %q", name, got, want)
		}
	}
	for name, want := range map[string]string{"95 m": "height must be 1–70 m", "gefällt 2099": "", "bhd 900 cm": "stem diameter must be 1–600 cm",
		strings.Repeat("x", 70): "species name too long (≤ 60 characters)"} {
		its, why := tmDiff(tmRow{n: 1, name: name, attrs: base}, "m1", time.Now())
		if name == "gefällt 2099" { // a year in the future is no date and no height either
			if why == "" && len(its) > 0 && its[0].body["date"] != nil {
				t.Errorf("future date accepted: %v", its[0].body)
			}
			continue
		}
		if why != want {
			t.Errorf("%q: %q, want %q", name, why, want)
		}
	}
	// a table edit: the name still is the export's label, the species field was typed over
	its, _ := tmDiff(tmRow{n: 1, name: "18 m spruce", attrs: map[string]string{"id": "t16158249_48262484", "h": "18.25", "species": "Tanne"}}, "m1", time.Now())
	if len(its) != 1 || its[0].body["species"] != "Tanne" {
		t.Errorf("table edit: %+v", its)
	}
	// a point of one's own: an add candidate; without anything said, refused
	its, why := tmDiff(tmRow{n: 1, name: "Eiche 15 m", lon: 16.2, lat: 48.2, hasPt: true, attrs: map[string]string{}}, "m1", time.Now())
	if why != "" || len(its) != 1 || !its[0].add || its[0].body["species"] != "Eiche" || its[0].body["h_m"] != 15.0 {
		t.Errorf("add: %v %+v", why, its)
	}
	if _, why := tmDiff(tmRow{n: 1, name: "", lon: 16.2, lat: 48.2, hasPt: true, attrs: map[string]string{}}, "m1", time.Now()); why == "" {
		t.Error("empty own point must be refused")
	}
	if _, why := tmDiff(tmRow{n: 1, name: "Eiche", lon: 2.35, lat: 48.85, hasPt: true, attrs: map[string]string{}}, "m1", time.Now()); why != "outside Austria" {
		t.Errorf("Paris: %q", why)
	}
	if _, why := tmDiff(tmRow{n: 1, name: "Eiche", attrs: map[string]string{"id": "../../etc/passwd"}}, "m1", time.Now()); why != "id is not a tree id" {
		t.Errorf("bad id: %q", why)
	}
}

func tmZip(t *testing.T, files map[string][]byte) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for n, b := range files {
		w, _ := zw.Create(n)
		w.Write(b)
	}
	zw.Close()
	return buf.Bytes()
}

func TestTreeMailHostile(t *testing.T) {
	pm := `<Placemark><name>x</name><Point><coordinates>16.2,48.2</coordinates></Point></Placemark>`
	kml := func(inner string) []byte {
		return []byte(`<?xml version="1.0"?><kml xmlns="http://www.opengis.net/kml/2.2"><Document>` + inner + `</Document></kml>`)
	}
	laughs := `<?xml version="1.0"?><!DOCTYPE lolz [<!ENTITY lol "lol"><!ENTITY lol2 "&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;"><!ENTITY lol3 "&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;">]><kml><Document><Placemark><name>&lol3;</name><Point><coordinates>16,48</coordinates></Point></Placemark></Document></kml>`
	xxe := `<?xml version="1.0"?><!DOCTYPE k [<!ENTITY x SYSTEM "file:///etc/passwd">]><kml><Document><Placemark><name>&x;</name><Point><coordinates>16,48</coordinates></Point></Placemark></Document></kml>`
	manyZip := map[string][]byte{}
	for i := 0; i < tmZipEntries+1; i++ {
		manyZip[fmt.Sprintf("f%d.kml", i)] = kml(pm)
	}
	bigCSV := "name,lat,lon\n" + strings.Repeat("x,48.2,16.2\n", tmMaxRows+1)
	cases := []struct {
		name, file string
		data       []byte
		want       string
	}{
		{"billion laughs", "a.kml", []byte(laughs), "DOCTYPE"},
		{"external entity", "a.kml", []byte(xxe), "DOCTYPE"},
		{"unknown entity", "a.kml", kml(`<Placemark><name>&bomb;</name><Point><coordinates>16,48</coordinates></Point></Placemark>`), "well-formed"},
		{"deep", "a.kml", kml(strings.Repeat("<Folder>", 40) + pm + strings.Repeat("</Folder>", 40)), "deep"},
		{"elements", "a.kml", kml(strings.Repeat("<a/>", tmXMLElems+1)), "too many elements"},
		{"rows", "a.kml", kml(strings.Repeat(pm, tmMaxRows+1)), "more than"},
		{"truncated", "a.kml", kml(pm)[:90], "well-formed"},
		{"zip bomb", "a.kmz", tmZip(t, map[string][]byte{"doc.kml": make([]byte, 64<<20)}), "too large"},
		{"zip ratio", "a.kmz", tmZip(t, map[string][]byte{"doc.kml": bytes.Repeat([]byte(" "), 8<<20)}), "ratio"},
		{"zip entries", "a.kmz", tmZip(t, manyZip), "holds"},
		{"nested archive", "a.kmz", tmZip(t, map[string][]byte{"inner.kmz": tmZip(t, map[string][]byte{"doc.kml": kml(pm)}), "x.zip": []byte("PK")}), "without a .kml"},
		{"not a zip", "a.kmz", []byte("hello"), "not a readable"},
		{"huge csv", "a.csv", []byte(bigCSV), "more than"},
		{"wide csv", "a.csv", []byte(strings.Repeat("c,", 40) + "lat,lon\n"), "columns"},
		{"wide csv line", "a.csv", []byte("name,lat,lon\n" + strings.Repeat("x,", 40) + "\n"), "columns"},
		{"csv no position", "a.csv", []byte("a,b\n1,2\n"), "lat + lon"},
		{"binary csv", "a.csv", []byte("name,lat,lon\n\xff\xfe,1,2\n"), "UTF-8"},
		{"other type", "a.exe", []byte("MZ"), "not a .kml"},
	}
	for _, c := range cases {
		start := time.Now()
		rows, err := tmParseAttachment(c.file, c.data)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %d rows, err %v — want %q", c.name, len(rows), err, c.want)
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("%s took %v", c.name, d)
		}
	}
	// a path in a zip entry name is only a name; a text longer than tmField is cut to it; long names do not travel
	rows, err := tmParseAttachment("a.kmz", tmZip(t, map[string][]byte{"../../../../tmp/evil.kml": kml(`<Placemark><name>` + strings.Repeat("y", tmField+900) + `</name><Point><coordinates>16.2,48.2</coordinates></Point></Placemark>`)}))
	if err != nil || len(rows) != 1 || len(rows[0].name) != tmField {
		t.Fatalf("path entry: %v %d", err, len(rows))
	}
	if _, err := os.Stat("/tmp/evil.kml"); err == nil {
		t.Fatal("zip entry was written to disk")
	}
	if _, why := tmDiff(rows[0], "m", time.Now()); why == "" {
		t.Error("a 4 kB name must be refused")
	}
}

// ── the mailbox, end to end, against a stub gateway and a stub peer ──

type tmStub struct {
	mu      sync.Mutex
	sent    []map[string]string      // gateway
	posts   []map[string]interface{} // peer batches
	gateway int
	peer    func(item map[string]interface{}) map[string]interface{}
}

func tmSetup(t *testing.T) (*tmStub, string) {
	t.Helper()
	st := &tmStub{gateway: 200}
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]string
		json.NewDecoder(r.Body).Decode(&m)
		st.mu.Lock()
		defer st.mu.Unlock()
		if st.gateway != 200 {
			w.WriteHeader(st.gateway)
			return
		}
		st.sent = append(st.sent, m)
		w.Write([]byte(`{}`))
	}))
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]interface{}
		json.NewDecoder(r.Body).Decode(&m)
		st.mu.Lock()
		defer st.mu.Unlock()
		m["_key"] = r.Header.Get("X-Reporter-Key")
		st.posts = append(st.posts, m)
		var res []interface{}
		for _, it := range m["reports"].([]interface{}) {
			out := map[string]interface{}{"http": 201}
			if st.peer != nil {
				out = st.peer(it.(map[string]interface{}))
			}
			res = append(res, out)
		}
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]interface{}{"results": res})
	}))
	t.Cleanup(gw.Close)
	t.Cleanup(peer.Close)
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "Maildir", "new"), 0o700)
	oldConf, oldGW, oldURL, oldKey := tmConf, tmGatewayURL, treeReportURL, treeReportKey
	cfg := &tmConfig{addr: "trees@vm.example", dir: filepath.Join(dir, "Maildir"), state: filepath.Join(dir, "state"),
		keys: []tmKey{{secret: tmKeyFold("K7M3-QX9D"), name: "anna", daily: 5}}}
	tmConf = func() *tmConfig { return cfg }
	tmGatewayURL = gw.URL
	treeReportURL = func() string { return peer.URL }
	treeReportKey = func() string { return "trk_test" }
	tmReset := func() {
		tm.mu.Lock()
		tm.loaded, tm.st, tm.newMod, tm.queued = false, tmStateFile{}, time.Time{}, 0
		tm.tries, tm.counts = map[string]int{}, map[string]int64{}
		tm.mu.Unlock()
	}
	tmReset()
	t.Cleanup(func() { tmConf, tmGatewayURL, treeReportURL, treeReportKey = oldConf, oldGW, oldURL, oldKey; tmReset() })
	return st, dir
}

var tmSeq int

func tmDeliver(t *testing.T, dir, to, from, subject, attName string, att []byte, extra string) {
	t.Helper()
	tmSeq++
	var b strings.Builder
	fmt.Fprintf(&b, "Delivered-To: %s\r\n%sFrom: Anna <%s>\r\nTo: %s\r\nSubject: %s\r\nMessage-ID: <m%d@phone.example>\r\nMIME-Version: 1.0\r\n", to, extra, from, to, subject, tmSeq)
	if att == nil {
		b.WriteString("Content-Type: text/plain\r\n\r\nhello\r\n")
	} else {
		b.WriteString("Content-Type: multipart/mixed; boundary=\"BB\"\r\n\r\n--BB\r\nContent-Type: text/plain\r\n\r\nsent from my phone\r\n--BB\r\n")
		fmt.Fprintf(&b, "Content-Type: application/octet-stream; name=\"%s\"\r\nContent-Disposition: attachment; filename=\"%s\"\r\nContent-Transfer-Encoding: base64\r\n\r\n", attName, attName)
		enc := base64.StdEncoding.EncodeToString(att)
		for len(enc) > 76 {
			b.WriteString(enc[:76] + "\r\n")
			enc = enc[76:]
		}
		b.WriteString(enc + "\r\n--BB--\r\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "Maildir", "new", fmt.Sprintf("%d.eml", tmSeq)), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func tmFiles(dir string) (n int) {
	filepath.Walk(dir, func(p string, i os.FileInfo, err error) error {
		if err == nil && !i.IsDir() && filepath.Base(p) != "state.json" {
			n++
		}
		return nil
	})
	return
}

func TestTreeMailFlow(t *testing.T) {
	st, dir := tmSetup(t)
	const me, addr = "anna@phone.example", "trees@vm.example"
	edited := []byte(`<?xml version="1.0" encoding="utf-8"?><kml xmlns="http://www.opengis.net/kml/2.2"><Document>
<Placemark><name>Fichte 25 m</name><description><![CDATA[<table><tr><td><small><b>id</b></small></td><td align="center">t16160198_48263720</td></tr><tr><td><small><b>h</b></small></td><td>11</td></tr></table>]]></description><Point><coordinates>16.1601980,48.2637200,341.00</coordinates></Point></Placemark>
<Placemark><name>17 m spruce</name><description><![CDATA[<table><tr><td><small><b>id</b></small></td><td>t16158180_48262470</td></tr><tr><td><small><b>h</b></small></td><td>17</td></tr><tr><td><small><b>species</b></small></td><td>spruce</td></tr></table>]]></description><Point><coordinates>16.15818,48.26247</coordinates></Point></Placemark>
<Placemark><name>Eiche 15 m</name><Point><coordinates>16.1600000,48.2630000</coordinates></Point></Placemark>
<Placemark><name>Quatsch</name><description><![CDATA[<b>id</b></small></td><td>t16158532_48262477</td></tr><tr><td><b>h</b></td><td>18</td>]]></description><Point><coordinates>16.158532,48.262477</coordinates></Point></Placemark>
<Placemark><name>Paris</name><Point><coordinates>2.35,48.85</coordinates></Point></Placemark>
</Document></kml>`)

	// nobody we know: no answer, nothing relayed, mail gone
	tmDeliver(t, dir, addr, "eve@else.example", "trees", "a.kml", edited, "")
	tmDeliver(t, dir, addr, me, "subscribe wrongkey1", "", nil, "")
	tmDeliver(t, dir, addr, "MAILER-DAEMON@x.example", "subscribe K7M3-QX9D", "", nil, "")
	tmDeliver(t, dir, addr, me, "subscribe K7M3-QX9D", "", nil, "Auto-Submitted: auto-replied\r\n")
	tmDeliver(t, dir, "someone@vm.example", me, "private", "", nil, "") // not our address: left alone in cur
	tmSweep(true)
	if len(st.sent) != 0 || len(st.posts) != 0 {
		t.Fatalf("answered or relayed for strangers: %v %v", st.sent, st.posts)
	}
	if n := tmFiles(filepath.Join(dir, "Maildir", "new")) + tmFiles(filepath.Join(dir, "state")); n != 0 {
		t.Fatalf("%d mails left behind", n)
	}
	if n := tmFiles(filepath.Join(dir, "Maildir", "cur")); n != 1 {
		t.Fatalf("foreign mail: %d in cur", n)
	}

	// a forged subscribe: the gateway will not answer it → not bound
	st.gateway = 403
	tmDeliver(t, dir, addr, me, "subscribe K7M3-QX9D", "", nil, "")
	tmSweep(true)
	st.gateway = 200
	tmDeliver(t, dir, addr, me, "trees", "a.kml", edited, "")
	tmSweep(true)
	if len(st.sent) != 0 || len(st.posts) != 0 {
		t.Fatal("a forged subscribe bound the address")
	}

	// the real one — typed on a phone: other case, no hyphen
	tmDeliver(t, dir, addr, me, "Subscribe k7m3qx9d", "", nil, "")
	tmSweep(true)
	if len(st.sent) != 1 || st.sent[0]["to"] != me || st.sent[0]["in_reply_to"] == "" || !strings.Contains(st.sent[0]["body"], "Subscribed") {
		t.Fatalf("subscribe reply: %v", st.sent)
	}
	if strings.Contains(strings.ToLower(st.sent[0]["body"]+st.sent[0]["subject"]), "k7m3") {
		t.Fatal("the key is echoed in the reply")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "state", "state.json"))
	if strings.Contains(string(raw), "anna@") || strings.Contains(string(raw), "phone.example") || strings.Contains(strings.ToLower(string(raw)), "k7m3") || !strings.Contains(string(raw), `"anna"`) {
		t.Fatalf("state holds an address or a key: %s", raw)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "state", "state.json")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("state mode %v", fi.Mode())
	}

	// edits: Fichte 25 m on a tree, an untouched tree, an own point (404 → added), an unknown species, a point abroad
	st.peer = func(it map[string]interface{}) map[string]interface{} {
		switch {
		case it["species"] == "Quatsch":
			return map[string]interface{}{"http": 400, "error": "unknown species", "did_you_mean": []string{"Quitte"}}
		case it["species"] == "Eiche" && it["claim"] == "tree":
			return map[string]interface{}{"http": 404, "error": "no tree here"}
		}
		return map[string]interface{}{"http": 201}
	}
	st.sent = nil
	tmDeliver(t, dir, addr, me, "Bäume vom Wald", "Baume.kml", edited, "")
	tmSweep(true)
	if len(st.sent) != 2 {
		t.Fatalf("want receipt + refusal mail, got %d: %v", len(st.sent), st.sent)
	}
	r1, r2 := st.sent[0]["body"], st.sent[1]["body"]
	for _, want := range []string{"Baume.kml: 5 points · 3 edits · 1 unchanged · 1 refused", "outside Austria (row 5)", `row 1 "Fichte 25 m": species Fichte · height 25 m`, `subject "undo"`} {
		if !strings.Contains(r1, want) {
			t.Errorf("receipt lacks %q:\n%s", want, r1)
		}
	}
	if !strings.Contains(r2, "2 written · 0 already known · 1 refused") || !strings.Contains(r2, "did you mean Quitte?") {
		t.Errorf("second mail:\n%s", r2)
	}
	if st.sent[0]["subject"] != "Re: Bäume vom Wald" {
		t.Errorf("subject %q", st.sent[0]["subject"])
	}
	if len(st.posts) != 2 {
		t.Fatalf("want 2 batches (reports, then the add), got %d", len(st.posts))
	}
	first := st.posts[0]["reports"].([]interface{})
	it0 := first[0].(map[string]interface{})
	rep := tmReporter(tmConf().keys[0])
	if len(first) != 3 || it0["reporter"] != rep || it0["source"] != tmSource || it0["layer"] != "trees" || it0["h_m"] != 25.0 || it0["h_method"] != "estimate" || st.posts[0]["_key"] != "trk_test" {
		t.Fatalf("first batch: %v", st.posts[0])
	}
	if len(rep) != 13 || strings.Contains(rep, "anna") {
		t.Fatalf("reporter %q", rep)
	}
	add := st.posts[1]["reports"].([]interface{})[0].(map[string]interface{})
	if add["claim"] != "missing" || add["client_id"] != tmClientID(rep, 16.16, 48.263) || add["species"] != "Eiche" {
		t.Fatalf("add: %v", add)
	}
	if n := tmFiles(filepath.Join(dir, "Maildir", "new")) + tmFiles(filepath.Join(dir, "state")); n != 0 {
		t.Fatalf("%d files left after the receipt", n)
	}

	// undo: the same file takes it back; the added tree by its client_id
	st.sent, st.posts = nil, nil
	st.peer = func(it map[string]interface{}) map[string]interface{} {
		if it["claim"] == "tree" && it["lon"] == 16.16 {
			return map[string]interface{}{"http": 404}
		}
		return map[string]interface{}{"http": 201}
	}
	tmDeliver(t, dir, addr, me, "undo", "Baume.kml", edited, "")
	tmSweep(true)
	if len(st.posts) != 2 || len(st.sent) != 1 {
		t.Fatalf("undo: %d batches, %d mails", len(st.posts), len(st.sent))
	}
	u0 := st.posts[0]["reports"].([]interface{})[0].(map[string]interface{})
	u1 := st.posts[1]["reports"].([]interface{})[0].(map[string]interface{})
	if u0["retract"] != "last" || u0["species"] != nil || u1["retract"] != true || u1["claim"] != "missing" || u1["client_id"] == nil {
		t.Fatalf("undo bodies: %v · %v", u0, u1)
	}

	// the day's budget (5 for this key; 2+1 written, then 3 undone = 6 asked, some given back)
	st.sent, st.posts, st.peer = nil, nil, nil
	var many strings.Builder
	many.WriteString("name,lat,lon\n")
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&many, "Eiche,48.26%02d,16.16\n", i)
	}
	tmDeliver(t, dir, addr, me, "more", "more.csv", []byte(many.String()), "")
	tmSweep(true)
	sentN := 0
	for _, p := range st.posts {
		sentN += len(p["reports"].([]interface{}))
	}
	if sentN > 5 || len(st.sent) == 0 || !strings.Contains(st.sent[0]["body"], "NOT sent") {
		t.Fatalf("budget: %d relayed; %v", sentN, st.sent)
	}

	// a forged report from the bound address: the gateway refuses the receipt → nothing relayed
	tm.mu.Lock()
	tm.st.KeyRows, tm.st.AddrRows, tm.st.AllRows = map[string]int{}, map[string]int{}, 0
	tm.mu.Unlock()
	st.sent, st.posts, st.gateway = nil, nil, 403
	tmDeliver(t, dir, addr, me, "trees", "a.kml", edited, "")
	tmSweep(true)
	if len(st.posts) != 0 {
		t.Fatal("relayed a mail the gateway would not answer")
	}
	tm.mu.Lock()
	used := tm.st.AllRows
	tm.mu.Unlock()
	if used != 0 {
		t.Fatalf("a dropped mail cost %d edits", used)
	}
	// gateway down: the mail waits, then goes through
	st.gateway = 503
	tmDeliver(t, dir, addr, me, "trees", "a.kml", edited, "")
	tmSweep(true)
	if n := tmFiles(filepath.Join(dir, "state", "queue")); n != 1 || len(st.posts) != 0 {
		t.Fatalf("gateway down: %d queued, %d relayed", n, len(st.posts))
	}
	st.gateway = 200
	tmSweep(false)
	if n := tmFiles(filepath.Join(dir, "state", "queue")); n != 0 || len(st.posts) == 0 {
		t.Fatalf("after recovery: %d queued, %d relayed", n, len(st.posts))
	}

	// stop: unbound, then silence again
	st.sent, st.posts = nil, nil
	tmDeliver(t, dir, addr, me, "stop", "", nil, "")
	tmDeliver(t, dir, addr, me, "trees", "a.kml", edited, "")
	tmSweep(true)
	if len(st.sent) != 1 || len(st.posts) != 0 {
		t.Fatalf("stop: %d mails, %d batches", len(st.sent), len(st.posts))
	}
	raw, _ = os.ReadFile(filepath.Join(dir, "state", "state.json"))
	if !strings.Contains(string(raw), `"bind":{}`) {
		t.Fatalf("binding kept after stop: %s", raw)
	}
	// wrong keys: after tmSubFailAddr the address is ignored even with the right key
	for i := 0; i < tmSubFailAddr; i++ {
		tmDeliver(t, dir, addr, "bob@x.example", fmt.Sprintf("subscribe aaaaaaa%d", i), "", nil, "")
	}
	tmDeliver(t, dir, addr, "bob@x.example", "subscribe K7M3-QX9D", "", nil, "")
	st.sent = nil
	tmSweep(true)
	if len(st.sent) != 0 {
		t.Fatal("key guessing is not capped")
	}
	if s := fmt.Sprint(TreeMailStats()); strings.Contains(s, "example") || strings.Contains(strings.ToLower(s), "k7m3") {
		t.Fatalf("stats leak: %s", s)
	}
}

func TestTmSubjectShape(t *testing.T) {
	for in, want := range map[string]string{
		"subscribe abcd-efgh":     "shape: words=2 prefix=none first=subscribe",
		"Re: subscribe abcd-efgh": "shape: words=3 prefix=reply first=subscribe",
		"Fwd: Subscribe: abcd":    "shape: words=3 prefix=forward first=subscribe+punctuation",
		"abcd-efgh subscribe":     "shape: words=2 prefix=none first=other",
		"":                        "shape: words=0 prefix=none first=none",
		"AW:":                     "shape: words=1 prefix=reply first=none",
	} {
		if got := tmSubjectShape(in); got != want {
			t.Errorf("%q: %s, want %s", in, got, want)
		}
		if strings.Contains(tmSubjectShape(in), "abcd") {
			t.Errorf("%q: the shape leaks subject text", in)
		}
	}
}
