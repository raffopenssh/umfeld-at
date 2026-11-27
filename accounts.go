// accounts.go — accounts: who can be invoiced later. Internal; keys, account
// ids and budgets never appear in public docs or answers (docs/accounts.md).
//
// Everybody stays anonymous. An ACCOUNT exists only for a holder of an issued
// key: an export key (EXPORT_KEYS, export_gate.go — the account id is the key's
// name) or a mail key (TREE_MAIL_KEYS, tree_mail.go — `acct=<id>` names the
// export key it belongs to, without it the mail key is an account of its own,
// id `mail:<name>`). One holder with both: the export key's id.
//
// Three things hang on an account:
//
//  1. Tree reports. The observed-layer peer links reporters to a pseudonym
//     (its contract "Accounts"): acct = hex(HMAC-SHA256(ACCT_SECRET,
//     "acct:"+id))[:32], constant per account, sent as `X-Reporter-Account`
//     on the relayed POST (treeReportDo) when the reader's browser presents an
//     export key (`X-Export-Key`, the key field of the export menu) or the
//     mail came in under a mail key. `reporter` is never touched; no key, no
//     address, no name leaves this machine. The peer answers account:
//     "linked"; an `account_warning` is logged and counted — the report is
//     stored either way and is never retried or blocked for it.
//  2. Counts for a billing run: GET <peer>/api/v1/flags/account (counts only),
//     on demand from the private tier, cached 1 h per account and window —
//     never on a page view.
//  3. A day ledger of our own (data/accounts/usage.json): exports, bytes and
//     relayed report POSTs per account and UTC day, exports split by product
//     (table · tiles · archive · project · features · layers), 429s counted
//     apart. Accounts only — an
//     anonymous caller is never written. ≤ acctLedgerDays days.
//
// Private tier:
//
//	GET    $P/api/v1/accounts                       every account: id, pseudonym, keys by NAME, ledger per month
//	GET    $P/api/v1/accounts/{id}[?since=&until=]  + the peer's counts (unix seconds, as the peer takes them)
//	GET    $P/api/v1/accounts/{id}?members=N        + activation links for members 1…N (member keys, below)
//	GET    $P/api/v1/accounts[/{id}]?format=csv[&by=day]   the ledger as a sheet (default per month)
//	GET    $P/api/v1/accounts/{id}?format=txt[&by=day]     the usage statement a holder is sent
//
// All of it is driven from scripts/acct.sh — there is nothing for a key holder
// on the public side.
//
//	DELETE $P/api/v1/accounts/{id}                  the peer forgets the link (reports stay, anonymous)
//
// Without ACCT_SECRET nothing is linked and nothing is asked. The secret must
// never change: a new one is a new pseudonym for every account.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	acctLedgerDays  = 800 // ~26 months of day rows
	acctPeerTTL     = time.Hour
	acctPeerMemoMax = 256
	acctFlushEvery  = time.Minute
)

// acctSecret — ACCT_SECRET; a var so tests can set it.
var acctSecret = sync.OnceValue(func() []byte { return []byte(strings.TrimSpace(os.Getenv("ACCT_SECRET"))) })

// acctPseudonym — what the peer sees of an account; "" = not configured.
func acctPseudonym(id string) string {
	s := acctSecret()
	if len(s) == 0 || id == "" {
		return ""
	}
	m := hmac.New(sha256.New, s)
	m.Write([]byte("acct:" + id))
	return hex.EncodeToString(m.Sum(nil))[:32]
}

// acctIDMail — the account a mail key reports for.
func acctIDMail(k tmKey) string {
	if k.acct != "" {
		return k.acct
	}
	return "mail:" + k.name
}

// acctIDRequest — the account a relayed tree report is sent for: the request
// presents an ISSUED key in X-Export-Key — an export key, a member key or a
// MAIL key (people who already hold one type it into the same field or open
// …/#key=<mail key>). "" = anonymous; our own tokens are peers, not accounts.
//
// A mail key is 40 bits and is accepted HERE only, never on an export: each
// try costs a relayed report from the relay's one global bucket (1/s), so the
// answer's `account` field is no usable oracle; the page keeps a key this
// short out of export links (xK).
func acctIDRequest(r *http.Request) string {
	key := strings.TrimSpace(r.Header.Get("X-Export-Key"))
	if key == "" || len(key) > 128 {
		return ""
	}
	if id, t := exportIdentity(r); strings.HasPrefix(id, "key:") {
		if t.noAcct {
			return ""
		}
		return id[4:]
	}
	id := ""
	if f := tmKeyFold(key); len(f) >= tmKeyMinLen && len(f) <= acctMailKeyMax {
		for _, k := range tmConf().keys { // every key is compared, in constant time each
			if len(k.secret) == len(f) && subtle.ConstantTimeCompare(k.secret, f) == 1 {
				id = acctIDMail(k)
			}
		}
	}
	return id
}

// acctMailKeyMax — folded length up to which a key is a mail key for the page (xK keeps it out of links).
const acctMailKeyMax = 12

// ── member keys: what a person gets ──
//
// The account's own key (EXPORT_KEYS) is for the holder's machines and
// projects. A PERSON gets a member key derived from the account:
//
//	<name>.<n>.<24 chars of base32(HMAC-SHA256(ACCT_SECRET, "member:<name>:<n>"))>
//
// Nothing is stored for it: it verifies by recomputation, counts as its
// account everywhere an export key does (same budget, same ledger row — the
// member number is never written or sent anywhere), dies with the account's
// EXPORT_KEYS entry and one member is switched off with ACCT_REVOKED=<name>.<n>,….
// It travels as an activation link <public URL>/#key=<member key>: a URL
// FRAGMENT, which no server, proxy or log sees; the page stores it
// (localStorage ne.key, the export menu's key field) and removes it from the
// address bar (xTake, ne_glyph_export.go).

var acctRevoked = sync.OnceValue(func() map[string]bool {
	m := map[string]bool{}
	for _, e := range strings.Split(os.Getenv("ACCT_REVOKED"), ",") {
		if e = strings.TrimSpace(e); e != "" {
			m[e] = true
		}
	}
	return m
})

// acctMemberKey — member n of an account; "" without ACCT_SECRET.
func acctMemberKey(name string, n int) string {
	s := acctSecret()
	if len(s) == 0 || name == "" || n < 1 {
		return ""
	}
	m := hmac.New(sha256.New, s)
	m.Write([]byte("member:" + name + ":" + strconv.Itoa(n)))
	return name + "." + strconv.Itoa(n) + "." + strings.ToLower(base32.StdEncoding.EncodeToString(m.Sum(nil)))[:24]
}

// acctMemberTier — the account tier a member key belongs to, or nil.
func acctMemberTier(key string) *exportTier {
	j := strings.LastIndexByte(key, '.')
	if j < 3 || len(key) > 128 {
		return nil
	}
	i := strings.LastIndexByte(key[:j], '.')
	if i < 1 {
		return nil
	}
	name := key[:i]
	n, err := strconv.Atoi(key[i+1 : j])
	if err != nil || n < 1 || n > 1_000_000 {
		return nil
	}
	want := acctMemberKey(name, n)
	if want == "" || subtle.ConstantTimeCompare([]byte(want), []byte(key)) != 1 || acctRevoked()[name+"."+strconv.Itoa(n)] {
		return nil
	}
	for _, t := range exportTiers {
		if t.name == name && !t.noAcct {
			return t
		}
	}
	return nil
}

// acctNone — an issued key that is no account (EXPORT_KEYS flag `noacct`: a
// shared demo key). It keeps its budget and is otherwise anonymous.
func acctNone(id string) bool {
	exportLoadTiers()
	for _, t := range exportTiers {
		if t.name == id {
			return t.noAcct
		}
	}
	return false
}

type acctCtxKey struct{}

// acctCtx — the account a relayed report is sent for (read by treeReportDo).
func acctCtx(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, acctCtxKey{}, id)
}

func acctFromCtx(ctx context.Context) string {
	id, _ := ctx.Value(acctCtxKey{}).(string)
	return id
}

// ── the peer's answer to a linked report ──

var acctLink = struct {
	mu       sync.Mutex
	linked   int64
	warned   int64
	lastWarn string
	logged   time.Time
}{}

// acctAnswer — account / account_warning of the peer's answer: counted, a
// warning logged (at most one line per 10 min). Never changes what the reader gets.
func acctAnswer(id string, status int, out []byte) {
	var a struct {
		Account string `json:"account"`
		Warning string `json:"account_warning"`
	}
	if status >= 300 || json.Unmarshal(out, &a) != nil {
		return
	}
	acctLink.mu.Lock()
	defer acctLink.mu.Unlock()
	if a.Account == "linked" && a.Warning == "" {
		acctLink.linked++
		return
	}
	acctLink.warned++
	if len(a.Warning) > 200 {
		a.Warning = a.Warning[:200]
	}
	acctLink.lastWarn = a.Warning
	if time.Since(acctLink.logged) > 10*time.Minute {
		acctLink.logged = time.Now()
		log.Printf("accounts: report for %q not linked at the peer (account=%q): %s", id, a.Account, a.Warning)
	}
}

// ── day ledger ──

type acctDay struct {
	Exports int64 `json:"exports,omitempty"` // export requests (tables, tiles, archives, projects)
	Bytes   int64 `json:"bytes,omitempty"`
	Reports int64 `json:"reports,omitempty"` // relayed report POSTs (a batch counts once; the peer has the real counts)
	Limited int64 `json:"limited,omitempty"` // requests answered 429 (not in Exports)
	// Exports by product: table · tiles · archive · project · features · layers
	Kinds map[string]int64 `json:"kinds,omitempty"`
}

// acctKind — the product an export path belongs to (a closed set: it is a ledger key).
func acctKind(path string) string {
	switch {
	case strings.Contains(path, "/ne/stats/"):
		return "table"
	case strings.Contains(path, "/pmtiles"):
		return "archive"
	case strings.HasSuffix(path, ".qgs") || strings.HasSuffix(path, ".qml") || strings.Contains(path, "mvt.zip"):
		return "project"
	case strings.Contains(path, "/mvt/ogc") || strings.HasSuffix(path, ".geojson"):
		return "features"
	case strings.Contains(path, "/mvt/relief") || strings.Contains(path, "/mvt/trees") || strings.Contains(path, "/mvt/pic"):
		return "layers"
	case strings.Contains(path, "/mvt"):
		return "tiles"
	}
	return "other"
}

// acctURL — a request URL for logs and the in-flight list: an export key in
// the query (links, =IMPORTDATA) is never written anywhere.
func acctURL(u *url.URL) string {
	if !strings.Contains(u.RawQuery, "key=") {
		return u.String()
	}
	c := *u
	parts := strings.Split(c.RawQuery, "&")
	for i, p := range parts {
		if strings.HasPrefix(p, "key=") {
			parts[i] = "key=-"
		}
	}
	c.RawQuery = strings.Join(parts, "&")
	return c.String()
}

var acctLedger = struct {
	mu      sync.Mutex
	path    string // "" = memory only (tests)
	loaded  bool
	days    map[string]map[string]*acctDay // day → account id → counts
	dirty   bool
	flushed time.Time
	writing bool
}{days: map[string]map[string]*acctDay{}}

// acctLedgerEnable — main() only: the ledger lives on disk from here on.
func acctLedgerEnable() {
	acctLedger.mu.Lock()
	acctLedger.path = filepath.Join("data", "accounts", "usage.json")
	acctLedger.mu.Unlock()
}

func acctLedgerLoadLocked() {
	l := &acctLedger
	if l.loaded {
		return
	}
	l.loaded = true
	l.flushed = time.Now()
	if l.path == "" {
		return
	}
	if b, err := os.ReadFile(l.path); err == nil {
		var f struct {
			Days map[string]map[string]*acctDay `json:"days"`
		}
		if json.Unmarshal(b, &f) == nil && f.Days != nil {
			l.days = f.Days
		} else {
			// never overwrite what we cannot read: keep it beside
			os.Rename(l.path, l.path+".unreadable-"+time.Now().UTC().Format("20060102T150405"))
			log.Printf("accounts: ledger unreadable, kept beside and started fresh")
		}
	}
}

// acctCount — add to today's row of an account. Cheap; the file is written at
// most once a minute, off the request path, and at shutdown (acctLedgerFlush).
func acctCount(id, kind string, exports, bytes, reports int64) {
	if id == "" || acctNone(id) {
		return
	}
	day := time.Now().UTC().Format("2006-01-02")
	l := &acctLedger
	l.mu.Lock()
	acctLedgerLoadLocked()
	d := l.days[day]
	if d == nil {
		d = map[string]*acctDay{}
		l.days[day] = d
		if len(l.days) > acctLedgerDays {
			ks := make([]string, 0, len(l.days))
			for k := range l.days {
				ks = append(ks, k)
			}
			sort.Strings(ks)
			for _, k := range ks[:len(ks)-acctLedgerDays] {
				delete(l.days, k)
			}
		}
	}
	e := d[id]
	if e == nil {
		if len(d) >= 4096 { // ids come from the env file; a bound all the same
			l.mu.Unlock()
			return
		}
		e = &acctDay{}
		d[id] = e
	}
	e.Exports += exports
	e.Bytes += bytes
	e.Reports += reports
	if kind == "limited" {
		e.Limited++
	} else if kind != "" && exports > 0 {
		if e.Kinds == nil {
			e.Kinds = map[string]int64{}
		}
		e.Kinds[kind] += exports
	}
	l.dirty = true
	kick := l.path != "" && !l.writing && time.Since(l.flushed) > acctFlushEvery
	if kick {
		l.writing = true
	}
	l.mu.Unlock()
	if kick {
		go acctLedgerWrite()
	}
}

// acctLedgerFlush — write now if anything changed (shutdown).
func acctLedgerFlush() {
	l := &acctLedger
	l.mu.Lock()
	if l.path == "" || !l.dirty || l.writing {
		l.mu.Unlock()
		return
	}
	l.writing = true
	l.mu.Unlock()
	acctLedgerWrite()
}

func acctLedgerWrite() {
	l := &acctLedger
	l.mu.Lock()
	b, _ := json.Marshal(map[string]interface{}{"days": l.days})
	path := l.path
	l.dirty = false
	l.mu.Unlock()
	err := os.MkdirAll(filepath.Dir(path), 0o700)
	if err == nil {
		if err = os.WriteFile(path+".tmp", b, 0o600); err == nil {
			err = os.Rename(path+".tmp", path)
		}
	}
	l.mu.Lock()
	l.writing, l.flushed = false, time.Now()
	if err != nil {
		l.dirty = true
	}
	l.mu.Unlock()
	if err != nil {
		log.Printf("accounts: ledger not written: %v", err)
	}
}

// acctLedgerMonths — an account's rows summed per month, days in [since, until] (unix s, 0 = open).
func acctLedgerMonths(id string, since, until int64) map[string]acctDay {
	out := map[string]acctDay{}
	l := &acctLedger
	l.mu.Lock()
	defer l.mu.Unlock()
	acctLedgerLoadLocked()
	for day, d := range l.days {
		e := d[id]
		if e == nil {
			continue
		}
		if t, err := time.Parse("2006-01-02", day); err != nil || (since > 0 && t.Unix()+86400 <= since) || (until > 0 && t.Unix() > until) {
			continue
		}
		m := out[day[:7]]
		m.Exports += e.Exports
		m.Bytes += e.Bytes
		m.Reports += e.Reports
		m.Limited += e.Limited
		for k, v := range e.Kinds {
			if m.Kinds == nil {
				m.Kinds = map[string]int64{}
			}
			m.Kinds[k] += v
		}
		out[day[:7]] = m
	}
	return out
}

// acctLedgerRow — one period of one account.
type acctLedgerRow struct {
	Period string
	ID     string
	acctDay
}

// acctKinds — the CSV's product columns, in order.
var acctKinds = []string{"table", "tiles", "archive", "project", "features", "layers", "other"}

// acctLedgerRows — rows of one account ("" = all) per day or month, sorted.
func acctLedgerRows(id string, since, until int64, byMonth bool) []acctLedgerRow {
	l := &acctLedger
	l.mu.Lock()
	defer l.mu.Unlock()
	acctLedgerLoadLocked()
	agg := map[[2]string]*acctLedgerRow{}
	for day, d := range l.days {
		if t, err := time.Parse("2006-01-02", day); err != nil || (since > 0 && t.Unix()+86400 <= since) || (until > 0 && t.Unix() > until) {
			continue
		}
		p := day
		if byMonth {
			p = day[:7]
		}
		for aid, e := range d {
			if id != "" && aid != id {
				continue
			}
			k := [2]string{p, aid}
			r := agg[k]
			if r == nil {
				r = &acctLedgerRow{Period: p, ID: aid, acctDay: acctDay{Kinds: map[string]int64{}}}
				agg[k] = r
			}
			r.Exports += e.Exports
			r.Bytes += e.Bytes
			r.Reports += e.Reports
			r.Limited += e.Limited
			for kk, v := range e.Kinds {
				r.Kinds[kk] += v
			}
		}
	}
	out := make([]acctLedgerRow, 0, len(agg))
	for _, r := range agg {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Period != out[j].Period {
			return out[i].Period < out[j].Period
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// acctCSV — the ledger as a sheet: period,account,exports,<products…>,bytes,reports,limited.
func acctCSV(rows []acctLedgerRow) []byte {
	var b strings.Builder
	b.WriteString("period,account,exports," + strings.Join(acctKinds, ",") + ",bytes,reports,limited\n")
	for _, r := range rows {
		b.WriteString(r.Period + "," + r.ID + "," + strconv.FormatInt(r.Exports, 10))
		for _, k := range acctKinds {
			b.WriteString("," + strconv.FormatInt(r.Kinds[k], 10))
		}
		b.WriteString("," + strconv.FormatInt(r.Bytes, 10) + "," + strconv.FormatInt(r.Reports, 10) + "," + strconv.FormatInt(r.Limited, 10) + "\n")
	}
	return []byte(b.String())
}

// acctStatement — the usage statement of one account as plain text: what a
// key holder is sent (on request, or with an invoice). Key NAMES only, never a
// key or the pseudonym.
func acctStatement(a *acctRow, since, until int64, byMonth bool) []byte {
	var b strings.Builder
	day := func(u int64, open string) string {
		if u <= 0 {
			return open
		}
		return time.Unix(u, 0).UTC().Format("2006-01-02")
	}
	b.WriteString("Usage statement — account " + a.ID + "\n")
	b.WriteString("Service:  " + publicURL() + "\n")
	b.WriteString("Period:   " + day(since, "start") + " … " + day(until, "today") + " (UTC days)\n")
	b.WriteString("Issued:   " + time.Now().UTC().Format("2006-01-02 15:04") + " UTC\n")
	if a.Export != "" {
		b.WriteString("Keys:     export key \"" + a.Export + "\" and its member keys\n")
	}
	if len(a.Mail) > 0 {
		b.WriteString("          mail keys " + strings.Join(a.Mail, ", ") + "\n")
	}
	rows := acctLedgerRows(a.ID, since, until, byMonth)
	var tot acctDay
	tot.Kinds = map[string]int64{}
	for _, r := range rows {
		tot.Exports += r.Exports
		tot.Bytes += r.Bytes
		tot.Reports += r.Reports
		tot.Limited += r.Limited
		for k, v := range r.Kinds {
			tot.Kinds[k] += v
		}
	}
	b.WriteString("\nExports (this service)\n")
	b.WriteString("  requests          " + strconv.FormatInt(tot.Exports, 10) + "\n")
	for _, k := range acctKinds {
		if tot.Kinds[k] > 0 {
			b.WriteString("    " + k + strings.Repeat(" ", 16-len(k)) + strconv.FormatInt(tot.Kinds[k], 10) + "\n")
		}
	}
	b.WriteString("  data sent         " + strconv.FormatFloat(float64(tot.Bytes)/1e6, 'f', 1, 64) + " MB\n")
	b.WriteString("  over the budget   " + strconv.FormatInt(tot.Limited, 10) + " (answered 429, not counted above)\n")
	b.WriteString("  tree reports sent " + strconv.FormatInt(tot.Reports, 10) + " (requests relayed; a file of edits is a few)\n")
	b.WriteString("\nTree reports (as stored at " + peers().ObsName + ")\n")
	if a.PeerErr != "" || len(a.Peer) == 0 {
		b.WriteString("  not available right now\n")
	} else {
		var p map[string]interface{}
		json.Unmarshal(a.Peer, &p)
		num := func(k string) string {
			f, _ := p[k].(float64)
			return strconv.FormatInt(int64(f), 10)
		}
		for _, k := range []string{"reporters", "reports", "tree_versions", "species", "measures", "added", "retracted", "trees", "flags"} {
			b.WriteString("  " + k + strings.Repeat(" ", 18-len(k)) + num(k) + "\n")
		}
		if f, _ := p["first_ts"].(float64); f > 0 {
			l, _ := p["last_ts"].(float64)
			b.WriteString("  first … last      " + day(int64(f), "") + " … " + day(int64(l), "") + "\n")
		}
	}
	unit := "day"
	if byMonth {
		unit = "month"
	}
	b.WriteString("\nPer " + unit + "\n  " + unit + strings.Repeat(" ", 12-len(unit)) + "exports      MB  reports   429\n")
	for _, r := range rows {
		b.WriteString("  " + r.Period + strings.Repeat(" ", 12-len(r.Period)) + pad(strconv.FormatInt(r.Exports, 10), 7) + pad(strconv.FormatFloat(float64(r.Bytes)/1e6, 'f', 1, 64), 8) + pad(strconv.FormatInt(r.Reports, 10), 9) + pad(strconv.FormatInt(r.Limited, 10), 6) + "\n")
	}
	if len(rows) == 0 {
		b.WriteString("  (nothing in this period)\n")
	}
	b.WriteString("\nCounts only: no addresses, no places asked for, no content. Kept for 800 days.\n")
	return []byte(b.String())
}

func pad(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return strings.Repeat(" ", n-len(s)) + s
}

// ── the peer's account endpoint ──

// acctPeerURL — <peer>/api/v1/flags/account, beside the write endpoint.
func acctPeerURL() string {
	if u := treeReportURL(); u != "" {
		return u + "/account"
	}
	return ""
}

type acctPeerEntry struct {
	at   time.Time
	body json.RawMessage
}

var acctPeerMemo = struct {
	mu sync.Mutex
	m  map[string]acctPeerEntry
}{m: map[string]acctPeerEntry{}}

func acctPeerDo(ctx context.Context, method, id string, q url.Values) (int, []byte, error) {
	base, p := acctPeerURL(), acctPseudonym(id)
	if base == "" || p == "" {
		return 0, nil, errors.New("accounts are not configured (ACCT_SECRET, observed-layer peer)")
	}
	if q == nil {
		q = url.Values{}
	}
	q.Set("account", p)
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, method, base+"?"+q.Encode(), nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "umfeld-at/accounts (+"+publicURL()+")")
	if k := treeReportKey(); k != "" {
		req.Header.Set("X-Reporter-Key", k)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, out, nil
}

// acctPeerCounts — the peer's counts for an account and window; one fetch per
// hour at most (the answer and its age are returned).
func acctPeerCounts(ctx context.Context, id string, since, until int64) (json.RawMessage, time.Time, error) {
	key := id + "|" + strconv.FormatInt(since, 10) + "|" + strconv.FormatInt(until, 10)
	acctPeerMemo.mu.Lock()
	e, ok := acctPeerMemo.m[key]
	acctPeerMemo.mu.Unlock()
	if ok && time.Since(e.at) < acctPeerTTL {
		return e.body, e.at, nil
	}
	q := url.Values{}
	if since > 0 {
		q.Set("since", strconv.FormatInt(since, 10))
	}
	if until > 0 {
		q.Set("until", strconv.FormatInt(until, 10))
	}
	status, out, err := acctPeerDo(ctx, http.MethodGet, id, q)
	if err != nil {
		return nil, time.Time{}, err
	}
	if status != 200 || !json.Valid(out) {
		return nil, time.Time{}, errors.New("peer answered " + strconv.Itoa(status))
	}
	e = acctPeerEntry{time.Now(), out}
	acctPeerMemo.mu.Lock()
	if len(acctPeerMemo.m) >= acctPeerMemoMax {
		for k, v := range acctPeerMemo.m {
			if time.Since(v.at) >= acctPeerTTL || len(acctPeerMemo.m) >= acctPeerMemoMax {
				delete(acctPeerMemo.m, k)
			}
		}
	}
	acctPeerMemo.m[key] = e
	acctPeerMemo.mu.Unlock()
	return e.body, e.at, nil
}

// acctPeerForget — DELETE at the peer; our memo of its counts goes too.
func acctPeerForget(ctx context.Context, id string) (json.RawMessage, error) {
	status, out, err := acctPeerDo(ctx, http.MethodDelete, id, nil)
	if err != nil {
		return nil, err
	}
	if status != 200 || !json.Valid(out) {
		return nil, errors.New("peer answered " + strconv.Itoa(status))
	}
	acctPeerMemo.mu.Lock()
	for k := range acctPeerMemo.m {
		if strings.HasPrefix(k, id+"|") {
			delete(acctPeerMemo.m, k)
		}
	}
	acctPeerMemo.mu.Unlock()
	return out, nil
}

// ── private tier ──

type acctRow struct {
	ID       string             `json:"id"`
	Account  string             `json:"account"` // the pseudonym the peer knows
	Export   string             `json:"export_key,omitempty"`
	Mail     []string           `json:"mail_keys,omitempty"`
	Months   map[string]acctDay `json:"ledger"`
	Peer     json.RawMessage    `json:"peer,omitempty"`
	PeerAt   string             `json:"peer_fetched,omitempty"`
	PeerErr  string             `json:"peer_error,omitempty"`
	Unlinked json.RawMessage    `json:"unlinked,omitempty"`
	Invites  []acctInvite       `json:"invites,omitempty"`
}

type acctInvite struct {
	Member  int    `json:"member"`
	Key     string `json:"key"`
	Link    string `json:"link"`
	Revoked bool   `json:"revoked,omitempty"`
}

// acctList — every configured account, keys by name only.
func acctList() []*acctRow {
	exportLoadTiers()
	by := map[string]*acctRow{}
	get := func(id string) *acctRow {
		a := by[id]
		if a == nil {
			a = &acctRow{ID: id, Account: acctPseudonym(id)}
			by[id] = a
		}
		return a
	}
	for _, t := range exportTiers {
		if !t.noAcct {
			get(t.name).Export = t.name
		}
	}
	for _, k := range tmConf().keys {
		a := get(acctIDMail(k))
		a.Mail = append(a.Mail, k.name)
	}
	out := make([]*acctRow, 0, len(by))
	for _, a := range by {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func handleV1Accounts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Header.Get(tierHeader) != "private" && (publicOnly() || r.Header.Get(tierHeader) != "") {
		http.NotFound(w, r)
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/accounts"), "/")
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	until, _ := strconv.ParseInt(r.URL.Query().Get("until"), 10, 64)
	rows := acctList()
	format, byMonth := r.URL.Query().Get("format"), r.URL.Query().Get("by") != "day"
	text := func(ct string, body []byte) {
		w.Header().Set("Content-Type", ct+"; charset=utf-8")
		w.Write(body)
	}
	if format == "csv" && r.Method == http.MethodGet { // ?format=csv[&by=day]: the ledger as a sheet, one account or all
		if id != "" && acctNone(id) {
			apiError(w, "no such account", http.StatusNotFound)
			return
		}
		text("text/csv", acctCSV(acctLedgerRows(id, since, until, byMonth)))
		return
	}
	if id == "" {
		if r.Method != http.MethodGet {
			apiError(w, "GET", http.StatusMethodNotAllowed)
			return
		}
		for _, a := range rows {
			a.Months = acctLedgerMonths(a.ID, since, until)
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"configured": len(acctSecret()) > 0, "accounts": rows})
		return
	}
	var a *acctRow
	for _, x := range rows {
		if x.ID == id {
			a = x
		}
	}
	if a == nil && r.Method == http.MethodDelete && len(id) <= 64 && !acctNone(id) {
		a = &acctRow{ID: id, Account: acctPseudonym(id)} // a key already taken out of the env file can still be forgotten at the peer
	}
	if a == nil {
		apiError(w, "no such account", http.StatusNotFound)
		return
	}
	a.Months = acctLedgerMonths(a.ID, since, until)
	switch r.Method {
	case http.MethodGet:
		// ?members=N: the activation links of members 1…N (recomputed, nothing stored)
		if n, _ := strconv.Atoi(r.URL.Query().Get("members")); n > 0 && n <= 500 && a.Export != "" {
			for i := 1; i <= n; i++ {
				if k := acctMemberKey(a.ID, i); k != "" {
					a.Invites = append(a.Invites, acctInvite{i, k, publicURL() + "/#key=" + k, acctRevoked()[a.ID+"."+strconv.Itoa(i)]})
				}
			}
		}
		if body, at, err := acctPeerCounts(r.Context(), a.ID, since, until); err != nil {
			a.PeerErr = err.Error()
		} else {
			a.Peer, a.PeerAt = body, at.UTC().Format(time.RFC3339)
		}
	case http.MethodDelete:
		out, err := acctPeerForget(r.Context(), a.ID)
		if err != nil {
			apiError(w, err.Error(), http.StatusBadGateway)
			return
		}
		a.Unlinked = out
		log.Printf("accounts: %q unlinked at the peer: %s", a.ID, out)
	default:
		apiError(w, "GET or DELETE", http.StatusMethodNotAllowed)
		return
	}
	if format == "txt" && r.Method == http.MethodGet { // the usage statement a holder is sent
		text("text/plain", acctStatement(a, since, until, byMonth))
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// AccountsStats — the debug inflight line.
func AccountsStats() map[string]interface{} {
	acctLedger.mu.Lock()
	days, path := len(acctLedger.days), acctLedger.path != ""
	acctLedger.mu.Unlock()
	acctPeerMemo.mu.Lock()
	memo := len(acctPeerMemo.m)
	acctPeerMemo.mu.Unlock()
	acctLink.mu.Lock()
	defer acctLink.mu.Unlock()
	return map[string]interface{}{"configured": len(acctSecret()) > 0, "ledger_days": days, "ledger_days_max": acctLedgerDays, "ledger_on_disk": path,
		"peer_memo": memo, "peer_memo_max": acctPeerMemoMax, "reports_linked": acctLink.linked, "reports_not_linked": acctLink.warned, "last_warning": acctLink.lastWarn}
}
