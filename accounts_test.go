package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The three calls of the peer's "Accounts" contract against a mock: the linked
// POST (header only when a key is presented, reporter untouched, a warning
// never blocks), the counts (one fetch per hour) and the delete.
func TestAccountsPeerContract(t *testing.T) {
	var (
		mu                 sync.Mutex
		postAcct, postRep  string
		gets, dels         int
		getQ               string
		getKey, delKey     string
		warn               bool
		sawKeyInBodyOrHead bool
	)
	const exportKey = "xk_test_0123456789abcdef"
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		for _, v := range r.Header {
			if strings.Contains(strings.Join(v, " "), exportKey) {
				sawKeyInBodyOrHead = true
			}
		}
		if strings.Contains(r.URL.RawQuery, exportKey) {
			sawKeyInBodyOrHead = true
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/flags":
			var m map[string]interface{}
			json.NewDecoder(r.Body).Decode(&m)
			postRep, _ = m["reporter"].(string)
			postAcct = r.Header.Get("X-Reporter-Account")
			if _, ok := m["account"]; ok {
				sawKeyInBodyOrHead = true
			}
			w.WriteHeader(201)
			switch {
			case postAcct == "":
				w.Write([]byte(`{"rev":1}`))
			case warn:
				w.Write([]byte(`{"rev":1,"account":"not_linked","account_warning":"key not verified"}`))
			default:
				w.Write([]byte(`{"rev":1,"account":"linked"}`))
			}
		case r.Method == "GET" && r.URL.Path == "/flags/account":
			gets++
			getQ, getKey = r.URL.RawQuery, r.Header.Get("X-Reporter-Key")
			w.Write([]byte(`{"linked":true,"reporters":2,"reports":5,"tree_versions":4,"species":3,"measures":1,"added":1,"retracted":1,"trees":4,"flags":0,"first_ts":10,"last_ts":20}`))
		case r.Method == "DELETE" && r.URL.Path == "/flags/account":
			dels++
			delKey = r.Header.Get("X-Reporter-Key")
			if r.URL.Query().Get("account") == "" {
				w.WriteHeader(400)
				return
			}
			w.Write([]byte(`{"unlinked":2}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer peer.Close()

	oldURL, oldKey, oldSecret, oldConf := treeReportURL, treeReportKey, acctSecret, tmConf
	treeReportURL = func() string { return peer.URL + "/flags" }
	treeReportKey = func() string { return "trk_test" }
	acctSecret = func() []byte { return []byte("s3cret") }
	tmConf = func() *tmConfig {
		return &tmConfig{keys: []tmKey{{secret: []byte("abcd2345"), name: "org-1", acct: "org"}, {secret: []byte("efgh2345"), name: "solo"}}}
	}
	exportLoadTiers()
	oldTiers := exportTiers
	exportTiers = append([]*exportTier{}, &exportTier{key: []byte(exportKey), name: "org", rps: 10, burst: 60, daily: 1000})
	acctLedger.mu.Lock()
	oldPath := acctLedger.path
	acctLedger.path, acctLedger.loaded, acctLedger.days = filepath.Join(t.TempDir(), "a", "usage.json"), false, map[string]map[string]*acctDay{}
	acctLedger.mu.Unlock()
	defer func() {
		treeReportURL, treeReportKey, acctSecret, tmConf, exportTiers = oldURL, oldKey, oldSecret, oldConf, oldTiers
		acctLedger.mu.Lock()
		acctLedger.path, acctLedger.loaded, acctLedger.days = oldPath, false, map[string]map[string]*acctDay{}
		acctLedger.mu.Unlock()
	}()

	m := hmac.New(sha256.New, []byte("s3cret"))
	m.Write([]byte("acct:org"))
	want := hex.EncodeToString(m.Sum(nil))[:32]
	if got := acctPseudonym("org"); got != want || len(got) != 32 {
		t.Fatalf("pseudonym %q, want %q", got, want)
	}
	// one holder with both keys: the mail key reports under the export key's id; a lone mail key is its own account
	if a, b := acctIDMail(tmConf().keys[0]), acctIDMail(tmConf().keys[1]); a != "org" || b != "mail:solo" {
		t.Fatalf("mail accounts %q %q", a, b)
	}

	post := func(key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/v1/trees/report", strings.NewReader(`{"lon":15.1,"lat":47.1,"species":"Fichte","reporter":"neg-abc","account":"smuggled"}`))
		if key != "" {
			r.Header.Set("X-Export-Key", key)
		}
		w := httptest.NewRecorder()
		handleV1TreeReport(w, r)
		return w
	}
	// 1. POST — signed out: no header
	if w := post(""); w.Code != 201 || postAcct != "" || postRep != "neg-abc" {
		t.Fatalf("anonymous: %d acct=%q reporter=%q", w.Code, postAcct, postRep)
	}
	// a key nobody issued is anonymous too
	if w := post("xk_wrong_0123456789abcdef"); w.Code != 201 || postAcct != "" {
		t.Fatalf("wrong key: %d acct=%q", w.Code, postAcct)
	}
	// signed in: the pseudonym rides along, reporter unchanged
	if w := post(exportKey); w.Code != 201 || postAcct != want || postRep != "neg-abc" || !strings.Contains(w.Body.String(), `"linked"`) {
		t.Fatalf("keyed: %d acct=%q reporter=%q %s", w.Code, postAcct, postRep, w.Body.String())
	}
	// a person's member key is the same account; a forged or revoked one is nobody
	mk := acctMemberKey("org", 3)
	if w := post(mk); w.Code != 201 || postAcct != want || strings.Count(mk, ".") != 2 {
		t.Fatalf("member key %q: %d acct=%q", mk, w.Code, postAcct)
	}
	if w := post(mk[:len(mk)-1] + "x"); w.Code != 201 || postAcct != "" {
		t.Fatalf("forged member key linked: acct=%q", postAcct)
	}
	if w := post(strings.Replace(mk, "org.3.", "org.4.", 1)); w.Code != 201 || postAcct != "" {
		t.Fatalf("member key of another number linked: acct=%q", postAcct)
	}
	oldRev := acctRevoked
	acctRevoked = func() map[string]bool { return map[string]bool{"org.3": true} }
	if w := post(mk); w.Code != 201 || postAcct != "" {
		t.Fatalf("revoked member key linked: acct=%q", postAcct)
	}
	acctRevoked = oldRev
	// somebody who already holds a mail key uses it in the map as typed (hyphen, case): same account
	if w := post("ABCD-2345"); w.Code != 201 || postAcct != want {
		t.Fatalf("mail key in the browser: %d acct=%q", w.Code, postAcct)
	}
	if w := post("abcd-2346"); w.Code != 201 || postAcct != "" {
		t.Fatalf("wrong mail key linked: acct=%q", postAcct)
	}
	// …but a mail key is no export key
	if id, _ := exportIdentity(httptest.NewRequest("GET", "/api/v1/ne/stats/unit?key=abcd2345", nil)); strings.HasPrefix(id, "key:") {
		t.Fatalf("mail key opened an export identity %q", id)
	}
	// a warning is logged and counted, the report goes through
	warn = true
	before := AccountsStats()["reports_not_linked"].(int64)
	if w := post(exportKey); w.Code != 201 {
		t.Fatalf("warning must not block: %d", w.Code)
	}
	if AccountsStats()["reports_not_linked"].(int64) != before+1 {
		t.Fatal("warning not counted")
	}
	warn = false
	// the mail path links through the context
	if st, _, _, err := treeReportDo(acctCtx(context.Background(), acctIDMail(tmConf().keys[0])), []byte(`{"reporter":"m0123456789ab"}`), "tree-mail"); err != nil || st != 201 || postAcct != want || postRep != "m0123456789ab" {
		t.Fatalf("mail: %d %v acct=%q", st, err, postAcct)
	}
	if sawKeyInBodyOrHead {
		t.Fatal("the export key (or a body account field we did not set) reached the peer")
	}

	// 2. GET — counts, cached for an hour, key header, private tier only
	get := func(method, path, tier string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		if tier != "" {
			r.Header.Set(tierHeader, tier)
		}
		w := httptest.NewRecorder()
		handleV1Accounts(w, r)
		return w
	}
	for i := 0; i < 3; i++ {
		w := get("GET", "/api/v1/accounts/org?since=100&until=4000000000", "private")
		var a acctRow
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &a) != nil || a.Account != want || !strings.Contains(string(a.Peer), `"tree_versions":4`) || len(a.Mail) != 1 {
			t.Fatalf("account: %d %s", w.Code, w.Body.String())
		}
		var rep int64
		for _, mo := range a.Months {
			rep += mo.Reports
		}
		if rep != 5 { // four keyed browser posts (export, member, mail key) + one mail post
			t.Fatalf("ledger reports = %d, want 5 (%s)", rep, w.Body.String())
		}
	}
	if gets != 1 || getKey != "trk_test" || !strings.Contains(getQ, "account="+want) || !strings.Contains(getQ, "since=100") || !strings.Contains(getQ, "until=4000000000") {
		t.Fatalf("peer GET: n=%d key=%q q=%q", gets, getKey, getQ)
	}
	if w := get("GET", "/api/v1/accounts", "private"); w.Code != 200 || !strings.Contains(w.Body.String(), `"mail:solo"`) || strings.Contains(w.Body.String(), exportKey) || strings.Contains(w.Body.String(), "abcd2345") || gets != 1 {
		t.Fatalf("list: %d %s (peer GETs %d)", w.Code, w.Body.String(), gets)
	}
	if w := get("GET", "/api/v1/accounts/org?members=2", "private"); !strings.Contains(w.Body.String(), "/#key="+acctMemberKey("org", 2)) || strings.Contains(w.Body.String(), acctMemberKey("org", 3)) {
		t.Fatalf("invites: %s", w.Body.String())
	}
	if w := get("GET", "/api/v1/accounts", "contrib"); w.Code != 404 {
		t.Fatalf("contributor tier reached accounts: %d", w.Code)
	}

	// 3. DELETE — the link goes, and with it our memo of the counts
	if w := get("DELETE", "/api/v1/accounts/org", "private"); w.Code != 200 || dels != 1 || delKey != "trk_test" || !strings.Contains(w.Body.String(), `"unlinked":2`) {
		t.Fatalf("delete: %d %s n=%d", w.Code, w.Body.String(), dels)
	}
	if get("GET", "/api/v1/accounts/org?since=100&until=4000000000", "private"); gets != 3 { // 1 window + 1 all-time (invites) + 1 after the delete
		t.Fatalf("memo survived the delete (peer GETs %d)", gets)
	}

	// the ledger survives a restart
	acctLedgerFlush()
	acctLedger.mu.Lock()
	acctLedger.loaded, acctLedger.days = false, map[string]map[string]*acctDay{}
	acctLedger.mu.Unlock()
	if mo := acctLedgerMonths("org", 0, 0); len(mo) != 1 {
		t.Fatalf("ledger after reload: %v", mo)
	}

	// exports land in the ledger by product; a key in a URL never reaches a log line
	r := httptest.NewRequest("GET", "/api/v1/ne/mvt/12/1/2.pbf?f=a&key="+exportKey+"&x=1", nil)
	exportAccountFor(r, 500)
	if mo := acctLedgerMonths("org", 0, 0); len(mo) != 1 {
		t.Fatalf("ledger: %v", mo)
	} else {
		for _, v := range mo {
			if v.Exports != 1 || v.Bytes != 500 || v.Kinds["tiles"] != 1 {
				t.Fatalf("export row: %+v", v)
			}
		}
	}
	if s := acctURL(r.URL); strings.Contains(s, exportKey) || !strings.Contains(s, "f=a&key=-&x=1") {
		t.Fatalf("logged URL %q", s)
	}

	// without the secret nothing is linked
	acctSecret = func() []byte { return nil }
	if w := post(exportKey); w.Code != 201 || postAcct != "" {
		t.Fatalf("no secret: %d acct=%q", w.Code, postAcct)
	}
}
