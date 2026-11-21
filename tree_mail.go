// tree_mail.go — trees by email: the return path of a field trip.
//
// A reader exports the trees of a view from QGIS (mvt/ogc), walks them with a
// phone app (Locus Map), edits names, and mails the file back. This loop reads
// the mailbox the platform delivers to (Maildir), turns edited rows into tree
// reports (tree_mail_parse.go) and hands them to the observed-layer peer
// through the relay of api_tree_report.go (same key, same bucket, same
// cleaning). Public description: docs/qgis-layer.md §Trees by email.
//
// WHAT IS STORED (data/tree_mail/state.json, 0600): per subscribed address a
// keyed hash (HMAC-SHA256 with a random per-deployment salt) → the key's name
// and day; today's counters. No mail address in clear, no tree, no report, no
// attachment — a mail lives in data/tree_mail/queue/ until its receipt is out
// (seconds) and is deleted; attachments are only ever in memory.
//
// WHO MAY WRITE: an address is bound by a mail with subject "subscribe <key>"
// (keys: TREE_MAIL_KEYS=key:name[:edits per day],… — internal, never in public
// docs). After that the bound From address is enough: a phone's share sheet
// cannot be asked to carry a key, and a key in every subject line would sit in
// every mail archive. From alone can be forged, so nothing is relayed before
// the first receipt has left: the platform's gateway only lets us answer mail
// whose DKIM signature is valid and aligned with From — if it refuses, the mail
// is dropped unanswered and unrelayed. Unknown senders and wrong keys get no
// answer at all (no backscatter, no key oracle).
//
// BOUNDS: mail ≤ tmMailMax, MIME depth ≤ 3 and ≤ tmMIMEParts parts, ≤
// tmAttachments attachments, parsers as in tree_mail_parse.go; edits per key
// and per address per UTC day, one total for the path (the peer's cap for our
// reporter key is shared with the map); batches ≤ treeReportBatch through the
// relay's bucket; replies per address per day; failed subscribes per address
// and in total per day; queue ≤ tmQueueMax files, each ≤ tmQueueAge old.
//
// IDLE: one stat of Maildir/new per tmTick; nothing else when nothing arrived
// (docs/idle.md). Line `tree_mail` on debug/inflight.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/mail"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	tmTick         = 20 * time.Second
	tmMailMax      = 1536 << 10 // the platform delivers ≤ 1 MB
	tmMIMEParts    = 24
	tmAttachments  = 3
	tmQueueMax     = 100
	tmQueueAge     = 6 * time.Hour
	tmKeyDaily     = 1500 // edits per key and UTC day unless the key says otherwise
	tmAddrDaily    = 1500 // edits per address and UTC day
	tmAllDaily     = 1500 // edits by mail per UTC day, all keys
	tmRepliesDaily = 40   // mails we send one address per day
	tmSubFailAddr  = 5    // wrong keys per address per day, then silence
	tmSubFailAll   = 1000 // wrong keys per day in total, then no subscribe until tomorrow
	tmAddrsPerKey  = 5
	tmKeyMinLen    = 8
	tmReceiptLines = 30
	tmShapeLogs    = 500 // subject-shape log lines per process
	tmSource       = "umfeld-at/tree-mail"
)

type tmKey struct {
	secret []byte // folded: lower case, letters and digits only
	name   string
	daily  int
	domain string // "@x.example": only addresses there may subscribe ("" = any)
	addrs  int    // addresses the key binds at a time (a personal key: 1)
}

type tmConfig struct {
	addr  string // the address we answer for, lower case
	dir   string // Maildir
	state string // data/tree_mail
	keys  []tmKey
}

// tmKeyFold — a key as typed on a phone: case, hyphens and spaces do not matter.
func tmKeyFold(s string) []byte {
	out := make([]byte, 0, len(s))
	for _, c := range []byte(strings.ToLower(s)) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			out = append(out, c)
		}
	}
	return out
}

// tmConf — the environment, once; a var so tests can set it.
var tmConf = sync.OnceValue(tmConfRead)

func tmConfRead() *tmConfig {
	c := &tmConfig{addr: strings.ToLower(strings.TrimSpace(os.Getenv("TREE_MAIL_ADDRESS"))), dir: os.Getenv("TREE_MAIL_DIR"), state: filepath.Join("data", "tree_mail")}
	if c.addr == "" {
		if h := publicHost(); strings.Contains(h, ".") {
			c.addr = "trees@" + strings.ToLower(strings.SplitN(strings.SplitN(h, "/", 2)[0], ":", 2)[0])
		}
	}
	if c.dir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			c.dir = filepath.Join(home, "Maildir")
		}
	}
	for _, e := range strings.Split(os.Getenv("TREE_MAIL_KEYS"), ",") {
		p := strings.Split(strings.TrimSpace(e), ":")
		if len(p) < 2 || p[1] == "" || len(p[1]) > 32 {
			continue
		}
		k := tmKey{secret: tmKeyFold(p[0]), name: p[1], daily: tmKeyDaily, addrs: tmAddrsPerKey}
		if len(k.secret) < tmKeyMinLen {
			log.Printf("tree-mail: key %q ignored (shorter than %d letters/digits)", k.name, tmKeyMinLen)
			continue
		}
		for _, f := range p[2:] { // in any order: 500 (edits a day) · @x.example · addr=1
			f = strings.ToLower(strings.TrimSpace(f))
			if n, err := strconv.Atoi(f); err == nil && n > 0 {
				k.daily = n
			} else if strings.HasPrefix(f, "@") && len(f) > 3 {
				k.domain = f
			} else if n, err := strconv.Atoi(strings.TrimPrefix(f, "addr=")); err == nil && n > 0 && n <= tmAddrsPerKey {
				k.addrs = n
			} else if f != "" {
				log.Printf("tree-mail: key %q: field %d not understood, ignored", k.name, len(p))
			}
		}
		c.keys = append(c.keys, k)
	}
	return c
}

// tmReporter — the reporter id a key's edits carry at the peer: stable for the
// key, short, and neither the key nor its name (HMAC keyed with the key).
func tmReporter(k tmKey) string {
	m := hmac.New(sha256.New, k.secret)
	m.Write([]byte("tree-mail reporter|" + k.name))
	return "m" + hex.EncodeToString(m.Sum(nil))[:12]
}

// ── state ──

type tmBind struct {
	Key   string `json:"key"` // the key's NAME
	Since string `json:"since"`
}

type tmStateFile struct {
	Salt     string            `json:"salt"`
	Bind     map[string]tmBind `json:"bind"` // address hash → key name
	Day      string            `json:"day"`
	KeyRows  map[string]int    `json:"key_rows"`
	AddrRows map[string]int    `json:"addr_rows"`
	Replies  map[string]int    `json:"replies"`
	SubFail  map[string]int    `json:"sub_fail"`
	AllRows  int               `json:"all_rows"`
	SubFails int               `json:"sub_fails"`
}

var tm = struct {
	mu        sync.Mutex
	loaded    bool
	st        tmStateFile
	newMod    time.Time
	tries     map[string]int
	counts    map[string]int64 // what happened to mails, by word
	last      time.Time
	queued    int
	running   bool
	shapeLogs int    // subject-shape lines since start (≤ tmShapeLogs)
	gwErr     string // the gateway's last refusal, addresses scrubbed
}{tries: map[string]int{}, counts: map[string]int64{}}

func tmCount(what string) {
	tm.mu.Lock()
	tm.counts[what]++
	tm.mu.Unlock()
}

// tmLoad — the state file, once; caller holds tm.mu.
func tmLoad() {
	if tm.loaded {
		return
	}
	tm.loaded = true
	if b, err := os.ReadFile(filepath.Join(tmConf().state, "state.json")); err == nil {
		json.Unmarshal(b, &tm.st)
	}
	s := &tm.st
	if s.Salt == "" {
		var r [32]byte
		rand.Read(r[:])
		s.Salt = hex.EncodeToString(r[:])
	}
	if s.Bind == nil {
		s.Bind = map[string]tmBind{}
	}
	tmDay()
}

// tmDay — counters belong to one UTC day; caller holds tm.mu.
func tmDay() {
	s := &tm.st
	if d := time.Now().UTC().Format("2006-01-02"); s.Day != d || s.KeyRows == nil {
		s.Day, s.KeyRows, s.AddrRows, s.Replies, s.SubFail, s.AllRows, s.SubFails = d, map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}, 0, 0
	}
}

// tmSave — atomically, 0600; caller holds tm.mu.
func tmSave() {
	dir := tmConf().state
	os.MkdirAll(dir, 0o700)
	b, _ := json.Marshal(&tm.st)
	tmp := filepath.Join(dir, "state.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		os.Rename(tmp, filepath.Join(dir, "state.json"))
	}
}

// tmAddrHash — what we keep of an address; caller holds tm.mu.
func tmAddrHash(addr string) string {
	m := hmac.New(sha256.New, []byte(tm.st.Salt))
	m.Write([]byte(strings.ToLower(addr)))
	return hex.EncodeToString(m.Sum(nil))[:24]
}

// TreeMailStats — the inflight line `tree_mail`: sizes and counters, no address, no key.
func TreeMailStats() map[string]interface{} {
	c := tmConf()
	tm.mu.Lock()
	defer tm.mu.Unlock()
	out := map[string]interface{}{"enabled": len(c.keys) > 0 && c.addr != "" && c.dir != "", "keys": len(c.keys), "running": tm.running, "queued": tm.queued}
	if !tm.loaded {
		return out
	}
	per := map[string]int{}
	for _, b := range tm.st.Bind {
		per[b.Key]++
	}
	mails := map[string]int64{}
	for k, v := range tm.counts {
		mails[k] = v
	}
	out["bound_addresses"], out["bound_per_key"] = len(tm.st.Bind), per
	out["today"] = map[string]interface{}{"day": tm.st.Day, "edits": tm.st.AllRows, "edits_per_key": tm.st.KeyRows, "addresses": len(tm.st.AddrRows), "replies": len(tm.st.Replies), "wrong_keys": tm.st.SubFails}
	out["mails"] = mails
	if tm.gwErr != "" {
		out["last_gateway_refusal"] = tm.gwErr
	}
	if !tm.last.IsZero() {
		out["last"] = tm.last.UTC().Format(time.RFC3339)
	}
	return out
}

// ── the loop ──

// treeMailLoop — started by startBackground; returns at once when the path is
// not configured.
func treeMailLoop() {
	c := tmConf()
	if len(c.keys) == 0 || c.addr == "" || c.dir == "" {
		return
	}
	tm.mu.Lock()
	tm.running = true
	tm.mu.Unlock()
	log.Printf("tree-mail: watching the mailbox (%d keys)", len(c.keys))
	for {
		tmSweep(false)
		time.Sleep(tmTick)
	}
}

// tmSweep — move new mail out of Maildir/new (ours into the queue, the rest
// to cur — it is not ours to read), then work the queue. Idle = one stat.
func tmSweep(force bool) {
	c := tmConf()
	newDir, queue := filepath.Join(c.dir, "new"), filepath.Join(c.state, "queue")
	fi, err := os.Stat(newDir)
	tm.mu.Lock()
	moved := err == nil && !fi.ModTime().Equal(tm.newMod)
	if moved {
		tm.newMod = fi.ModTime()
	}
	pending := tm.queued > 0
	tm.mu.Unlock()
	if !moved && !pending && !force {
		return
	}
	if moved || force {
		ents, _ := os.ReadDir(newDir)
		for _, e := range ents {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			src := filepath.Join(newDir, e.Name())
			if !tmIsOurs(src, c.addr) {
				os.MkdirAll(filepath.Join(c.dir, "cur"), 0o700)
				os.Rename(src, filepath.Join(c.dir, "cur", e.Name()))
				tmCount("other_address")
				continue
			}
			os.MkdirAll(queue, 0o700)
			if q, _ := os.ReadDir(queue); len(q) >= tmQueueMax {
				os.Remove(src)
				tmCount("dropped_queue_full")
				continue
			}
			dst := filepath.Join(queue, strconv.FormatInt(time.Now().UnixNano(), 36)+".eml") // our name, never the sender's
			if os.Rename(src, dst) != nil {
				if b, err := os.ReadFile(src); err == nil && len(b) <= tmMailMax {
					os.WriteFile(dst, b, 0o600)
				}
				os.Remove(src)
			}
			os.Chmod(dst, 0o600)
		}
		if fi, err := os.Stat(newDir); err == nil { // our own renames moved the mtime
			tm.mu.Lock()
			tm.newMod = fi.ModTime()
			tm.mu.Unlock()
		}
	}
	ents, _ := os.ReadDir(queue)
	left := 0
	for _, e := range ents {
		p := filepath.Join(queue, e.Name())
		info, err := e.Info()
		if err != nil || e.IsDir() {
			continue
		}
		keep := false
		if time.Since(info.ModTime()) < tmQueueAge && info.Size() <= tmMailMax {
			keep = tmProcessFile(p)
		} else {
			tmCount("dropped_old_or_large")
		}
		tm.mu.Lock()
		if keep {
			if tm.tries[p]++; tm.tries[p] >= 5 {
				keep = false
			}
		}
		if !keep {
			delete(tm.tries, p)
		}
		tm.mu.Unlock()
		if keep {
			left++
		} else {
			os.Remove(p)
		}
	}
	tm.mu.Lock()
	tm.queued = left
	tm.mu.Unlock()
}

// tmIsOurs — the injected first header names the envelope recipient.
func tmIsOurs(path, addr string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	line, _ := bufio.NewReader(io.LimitReader(f, 1024)).ReadString('\n')
	k, v, ok := strings.Cut(line, ":")
	return ok && strings.EqualFold(strings.TrimSpace(k), "Delivered-To") && strings.EqualFold(strings.Trim(strings.TrimSpace(v), "<>"), addr)
}

// tmProcessFile — one mail; true = keep it for another try (the gateway or
// the peer was busy). Never panics the service.
func tmProcessFile(path string) (retry bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("tree-mail: recovered: %v", r)
			tmCount("panic")
			retry = false
		}
	}()
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > tmMailMax {
		tmCount("dropped_unreadable")
		return false
	}
	tm.mu.Lock()
	tm.last = time.Now()
	tm.mu.Unlock()
	return tmHandle(raw)
}

// ── one mail ──

type tmAtt struct {
	name string
	data []byte
}

var tmWordDec = mime.WordDecoder{} // UTF-8, ISO-8859-1, US-ASCII only

func tmHeader(h mail.Header, k string) string {
	v := h.Get(k)
	if d, err := tmWordDec.DecodeHeader(v); err == nil {
		v = d
	}
	return strings.TrimSpace(v)
}

// tmAttachmentsOf — the .kml/.kmz/.csv parts of a message, decoded, in memory.
func tmAttachmentsOf(h mail.Header, body io.Reader) (atts []tmAtt, more bool) {
	parts := 0
	var walk func(ct, cte, disp string, r io.Reader, depth int)
	walk = func(ct, cte, disp string, r io.Reader, depth int) {
		mt, params, _ := mime.ParseMediaType(ct)
		if strings.HasPrefix(mt, "multipart/") {
			if depth >= 3 || params["boundary"] == "" {
				return
			}
			mr := multipart.NewReader(r, params["boundary"])
			for {
				if parts++; parts > tmMIMEParts {
					more = true
					return
				}
				p, err := mr.NextRawPart()
				if err != nil {
					return
				}
				walk(p.Header.Get("Content-Type"), p.Header.Get("Content-Transfer-Encoding"), p.Header.Get("Content-Disposition"), p, depth+1)
			}
		}
		name := ""
		if _, dp, err := mime.ParseMediaType(disp); err == nil {
			name = dp["filename"]
		}
		if name == "" {
			name = params["name"]
		}
		if d, err := tmWordDec.DecodeHeader(name); err == nil {
			name = d
		}
		name = filepath.Base(strings.ReplaceAll(name, "\\", "/")) // shown in the receipt, never opened
		if name == "." || name == "/" {
			name = ""
		}
		if name == "" {
			switch mt {
			case "application/vnd.google-earth.kml+xml":
				name = "attachment.kml"
			case "application/vnd.google-earth.kmz":
				name = "attachment.kmz"
			case "text/csv":
				name = "attachment.csv"
			}
		}
		l := strings.ToLower(name)
		if !strings.HasSuffix(l, ".kml") && !strings.HasSuffix(l, ".kmz") && !strings.HasSuffix(l, ".csv") {
			return
		}
		if len(atts) >= tmAttachments {
			more = true
			return
		}
		switch strings.ToLower(strings.TrimSpace(cte)) {
		case "base64":
			r = base64.NewDecoder(base64.StdEncoding, tmNoNewlines{r})
		case "quoted-printable":
			r = quotedprintable.NewReader(r)
		}
		data, err := io.ReadAll(io.LimitReader(r, tmMailMax))
		if err != nil && len(data) == 0 {
			return
		}
		atts = append(atts, tmAtt{name: tmClean(name, 48), data: data})
	}
	walk(h.Get("Content-Type"), h.Get("Content-Transfer-Encoding"), h.Get("Content-Disposition"), body, 0)
	return atts, more
}

// tmNoNewlines — base64 in mail is wrapped; the decoder wants it bare.
type tmNoNewlines struct{ r io.Reader }

func (t tmNoNewlines) Read(p []byte) (int, error) {
	for {
		n, err := t.r.Read(p)
		k := 0
		for _, c := range p[:n] {
			if c != '\r' && c != '\n' && c != ' ' && c != '\t' {
				p[k] = c
				k++
			}
		}
		if k > 0 || err != nil {
			return k, err
		}
	}
}

// tmClean — a sender's text as we print it: one line, no control characters, clipped.
func tmClean(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 || r == 0x2028 || r == 0x2029 {
			return ' '
		}
		return r
	}, s)
	return tmClip(strings.Join(strings.Fields(s), " "), n)
}

// tmGatewayURL — the platform's send endpoint; a var so tests can stub it.
var tmGatewayURL = "http://169.254.169.254/gateway/email/send"

var tmHTTP = &http.Client{Timeout: 20 * time.Second}

var tmAddrRe = regexp.MustCompile(`[^\s<>"']+@[^\s<>"']+`)

// tmSend — a short plain-text reply in the sender's thread. 0 = no answer from the gateway.
func tmSend(to, subject, body, inReplyTo string) int {
	b, _ := json.Marshal(map[string]string{"to": to, "subject": subject, "body": body, "in_reply_to": inReplyTo, "references": inReplyTo})
	req, _ := http.NewRequest(http.MethodPost, tmGatewayURL, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := tmHTTP.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// the gateway says why (reply policy, unverified sender, rate): the only
		// place a deployment fault shows. Addresses never reach the log.
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(out, &e)
		why := tmClean(tmAddrRe.ReplaceAllString(e.Error, "<addr>"), 200)
		tm.mu.Lock()
		tm.gwErr = strconv.Itoa(resp.StatusCode) + " " + why
		tm.mu.Unlock()
		log.Printf("tree-mail: the gateway refused a reply (%d): %s", resp.StatusCode, why)
	}
	return resp.StatusCode
}

// tmSubjectShape — why a mail from an unbound address was not a subscribe,
// without its words: word count, reply/forward prefix, and the first word
// only when it is one of ours (anything else could be a key or a name).
func tmSubjectShape(subject string) string {
	f := strings.Fields(strings.ToLower(subject))
	prefix := "none"
	if len(f) > 0 {
		switch strings.TrimRight(f[0], ":") {
		case "re", "aw", "antw", "sv":
			prefix = "reply"
		case "fwd", "fw", "wg", "wtr":
			prefix = "forward"
		}
	}
	rest := f
	if prefix != "none" {
		rest = f[1:]
	}
	first := "none"
	if len(rest) > 0 {
		first = "other"
		switch w := strings.TrimRight(rest[0], ":,."); w {
		case "subscribe", "unsubscribe", "stop", "help", "undo":
			first = w
			if w != rest[0] {
				first += "+punctuation"
			}
		}
	}
	return fmt.Sprintf("shape: words=%d prefix=%s first=%s", len(f), prefix, first)
}

// tmHandle — see the header comment for the order of checks. true = retry later.
func tmHandle(raw []byte) (retry bool) {
	c := tmConf()
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		tmCount("dropped_unreadable")
		return false
	}
	h := msg.Header
	// machines do not get answers: bounces, auto-replies, lists
	as, prec := strings.ToLower(strings.TrimSpace(h.Get("Auto-Submitted"))), strings.ToLower(strings.TrimSpace(h.Get("Precedence")))
	if (as != "" && as != "no") || prec == "bulk" || prec == "junk" || prec == "list" || h.Get("List-Id") != "" || h.Get("X-Autoreply") != "" {
		tmCount("dropped_automatic")
		return false
	}
	froms, err := h.AddressList("From")
	mid := strings.TrimSpace(h.Get("Message-Id"))
	if err != nil || len(froms) != 1 || mid == "" || len(mid) > 300 || len(froms[0].Address) > 254 {
		tmCount("dropped_no_sender")
		return false
	}
	from := strings.ToLower(froms[0].Address)
	if lp, _, _ := strings.Cut(from, "@"); lp == "mailer-daemon" || lp == "postmaster" || strings.HasPrefix(lp, "noreply") || strings.HasPrefix(lp, "no-reply") || from == c.addr {
		tmCount("dropped_automatic")
		return false
	}
	subject := tmClean(tmHeader(h, "Subject"), 120)
	word, arg, _ := strings.Cut(strings.ToLower(subject), " ")

	tm.mu.Lock()
	tmLoad()
	tmDay()
	ah := tmAddrHash(from)
	bind, bound := tm.st.Bind[ah]
	tm.mu.Unlock()

	reply := func(subj, body string) int {
		tm.mu.Lock()
		if tm.st.Replies[ah] >= tmRepliesDaily || len(tm.st.Replies) > 5000 {
			tm.mu.Unlock()
			tmCount("reply_capped")
			return 403
		}
		tm.st.Replies[ah]++
		tmSave()
		tm.mu.Unlock()
		st := tmSend(from, subj, body, mid)
		if st >= 200 && st < 300 {
			tmCount("replies")
		}
		return st
	}

	if word == "subscribe" {
		given := tmKeyFold(arg)
		tm.mu.Lock()
		if tm.st.SubFail[ah] >= tmSubFailAddr || tm.st.SubFails >= tmSubFailAll {
			tm.mu.Unlock()
			tmCount("subscribe_capped")
			return false
		}
		var key *tmKey
		for i := range c.keys { // every key is compared, in constant time each
			if subtle.ConstantTimeCompare(given, c.keys[i].secret) == 1 {
				key = &c.keys[i]
			}
		}
		if key == nil {
			if len(tm.st.SubFail) < 5000 {
				tm.st.SubFail[ah]++
			}
			tm.st.SubFails++
			tmSave()
			tm.mu.Unlock()
			tmCount("subscribe_wrong_key") // silence: no backscatter, no oracle
			return false
		}
		n := 0
		for a, b := range tm.st.Bind {
			if b.Key == key.name && a != ah {
				n++
			}
		}
		tm.mu.Unlock()
		// a right key that cannot bind this address is answered: the sender holds the key, silence would read as a broken service
		if key.domain != "" && !strings.HasSuffix(from, key.domain) {
			reply("Re: subscribe", tmOtherDomainText)
			tmCount("subscribe_other_domain")
			return false
		}
		if bound && bind.Key == key.name {
			reply("Re: subscribe", "This address is already subscribed with this key — nothing to do.\n\n"+tmHelpText(c, key, false))
			tmCount("subscribe_again")
			return false
		}
		if n >= key.addrs {
			reply("Re: subscribe", tmKeyInUseText(key.addrs, bound))
			tmCount("subscribe_full")
			return false
		}
		// the answer is the proof that From is real (DKIM, checked by the gateway): bind only after it left
		switch st := reply("Re: subscribe", tmHelpText(c, key, true)); {
		case st >= 200 && st < 300:
			tm.mu.Lock()
			tm.st.Bind[ah] = tmBind{Key: key.name, Since: tm.st.Day}
			tmSave()
			tm.mu.Unlock()
			tmCount("subscribed")
			log.Printf("tree-mail: an address subscribed with key %q", key.name)
		case st == 0 || st == 429 || st >= 500:
			return true
		default:
			tmCount("dropped_unverified")
		}
		return false
	}

	if !bound {
		tmCount("dropped_unknown_sender")
		tm.mu.Lock()
		tm.shapeLogs++
		quiet := tm.shapeLogs > tmShapeLogs
		tm.mu.Unlock()
		if !quiet {
			log.Printf("tree-mail: dropped, sender not subscribed; subject %s", tmSubjectShape(subject))
		}
		return false
	}
	var key *tmKey
	for i := range c.keys {
		if c.keys[i].name == bind.Key {
			key = &c.keys[i]
		}
	}
	if key == nil { // the key was withdrawn: its addresses go with it
		tm.mu.Lock()
		delete(tm.st.Bind, ah)
		tmSave()
		tm.mu.Unlock()
		tmCount("dropped_key_withdrawn")
		return false
	}
	re := "Re: " + strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(subject, "Re: "), "RE: "))
	if re == "Re: " {
		re = "Re: trees"
	}
	switch word {
	case "stop", "unsubscribe":
		tm.mu.Lock()
		delete(tm.st.Bind, ah)
		tmSave()
		tm.mu.Unlock()
		tmCount("unsubscribed")
		if word == "stop" {
			reply(re, "Done: this address is no longer subscribed and nothing about it is kept.\n")
		}
		return false
	case "help":
		reply(re, tmHelpText(c, key, false))
		return false
	}
	undo := word == "undo"

	atts, more := tmAttachmentsOf(h, msg.Body)
	if len(atts) == 0 {
		if st := reply(re, "No .kmz, .kml or .csv attachment found in your mail.\n\n"+tmHelpText(c, key, false)); st == 0 || st == 429 || st >= 500 {
			return true
		}
		tmCount("no_attachment")
		return false
	}

	// rows → edits
	now := time.Now()
	reporter := tmReporter(*key)
	var (
		items     []tmItem
		text      strings.Builder
		unchanged int
	)
	for _, a := range atts {
		rows, err := tmParseAttachment(a.name, a.data)
		if err != nil {
			fmt.Fprintf(&text, "%s: not read — %s\n", a.name, err)
			continue
		}
		n0 := len(items)
		its, same, refused := tmDiffRows(rows, reporter, now)
		items = append(items, its...)
		notes, dropped, cut := 0, 0, 0
		for _, it := range its {
			if _, ok := it.body["note"]; ok {
				notes++
			}
			dropped += it.dropped
			if it.cut {
				cut++
			}
		}
		unchanged += same
		nref := 0
		for _, v := range refused {
			nref += len(v)
		}
		fmt.Fprintf(&text, "%s: %d points · %d edits · %d unchanged · %d refused\n", a.name, len(rows), len(items)-n0, same, nref)
		if notes > 0 {
			fmt.Fprintf(&text, "  %d with a note (shown publicly)", notes)
			if cut > 0 {
				fmt.Fprintf(&text, " · %d cut at %d characters", cut, tmNoteLimit())
			}
			if dropped > 0 {
				fmt.Fprintf(&text, " · %d values left out: they look like an e-mail address or a phone number", dropped)
			}
			text.WriteString("\n")
		}
		whys := make([]string, 0, len(refused))
		for w := range refused {
			whys = append(whys, w)
		}
		sort.Slice(whys, func(i, j int) bool { return len(refused[whys[i]]) > len(refused[whys[j]]) })
		for i, w := range whys {
			if i == 6 {
				fmt.Fprintf(&text, "  … and %d more reasons\n", len(whys)-i)
				break
			}
			fmt.Fprintf(&text, "  refused %d × %s (%s)\n", len(refused[w]), w, tmRowList(refused[w]))
		}
	}
	if more {
		fmt.Fprintf(&text, "Only the first %d attachments were read.\n", tmAttachments)
	}

	// budgets: per key, per address, for the whole path
	tm.mu.Lock()
	tmDay()
	room := min(key.daily-tm.st.KeyRows[key.name], tmAddrDaily-tm.st.AddrRows[ah], tmAllDaily-tm.st.AllRows)
	if len(tm.st.AddrRows) > 5000 {
		room = 0
	}
	room = max(room, 0)
	over := 0
	if len(items) > room {
		over = len(items) - room
		items = items[:room]
	}
	tm.st.KeyRows[key.name] += len(items)
	tm.st.AddrRows[ah] += len(items)
	tm.st.AllRows += len(items)
	tmSave()
	tm.mu.Unlock()
	giveBack := func(n int) {
		tm.mu.Lock()
		tm.st.KeyRows[key.name] -= n
		tm.st.AddrRows[ah] -= n
		tm.st.AllRows -= n
		tmSave()
		tm.mu.Unlock()
	}
	if undo {
		for i := range items {
			items[i] = tmUndo(items[i])
		}
	}
	if len(items) > 0 {
		if undo {
			text.WriteString("\nTaking back now:\n")
		} else {
			text.WriteString("\nSending now:\n")
		}
		for i, it := range items {
			if i == tmReceiptLines {
				fmt.Fprintf(&text, "  … and %d more\n", len(items)-i)
				break
			}
			fmt.Fprintf(&text, "  row %d \"%s\": %s\n", it.row, tmClean(it.name, 40), tmClean(it.what, 80))
		}
	}
	if over > 0 {
		fmt.Fprintf(&text, "\n%d edits NOT sent: today's budget is used up. Send the file again tomorrow — what is already in counts as known.\n", over)
	}
	switch {
	case len(items) > 0 && undo:
		text.WriteString("\nA second mail follows only if something could not be taken back.\n")
	case len(items) > 0:
		text.WriteString("\nA second mail follows only if the tree service refuses a row.\nTo take these edits back: send the same file with the subject \"undo\".\n")
	case undo:
		text.WriteString("\nNothing to take back: attach the edited file you sent before.\n")
	default:
		text.WriteString("\nNothing to send. An edit is a changed NAME (\"Fichte\", \"25 m\", \"gefällt 2025\") — send \"help\" for the list.\n")
	}

	// the receipt first: only mail the gateway lets us answer is authentic
	switch st := reply(re, text.String()); {
	case st >= 200 && st < 300:
	case st == 0 || st == 429 || st >= 500:
		giveBack(len(items))
		return true
	default:
		giveBack(len(items))
		tmCount("dropped_unverified")
		return false
	}
	tmCount("mails_read")
	if len(items) == 0 {
		return false
	}
	res := tmRelay(context.Background(), items, undo)
	var bad strings.Builder
	written, known, failed := 0, 0, 0
	for i, r := range res {
		switch {
		case r.http == 201:
			written++
		case r.http == 200:
			known++
		default:
			if failed++; failed <= tmReceiptLines {
				fmt.Fprintf(&bad, "  row %d \"%s\" (%s): %s\n", items[i].row, tmClean(items[i].name, 40), tmClean(items[i].what, 60), tmClean(r.why, 160))
			}
		}
	}
	// position frames: rows the peer filed on the tree at its corrected position, rows it could not place
	var frameNote strings.Builder
	moved, unclear := 0, 0
	for i, r := range res {
		if r.moved {
			moved++
		}
		if r.hint != "" {
			if unclear++; unclear <= 5 {
				fmt.Fprintf(&frameNote, "  row %d \"%s\": %s\n", items[i].row, tmClean(items[i].name, 40), tmClean(r.hint, 200))
			}
		}
	}
	giveBack(failed) // a refused row costs nothing
	log.Printf("tree-mail: key=%q undo=%v edits=%d written=%d known=%d refused=%d unchanged=%d", key.name, undo, len(items), written, known, failed, unchanged)
	tm.mu.Lock()
	tm.counts["edits_written"] += int64(written)
	tm.counts["edits_known"] += int64(known)
	tm.counts["edits_refused"] += int64(failed)
	tm.mu.Unlock()
	if failed > 0 {
		out := fmt.Sprintf("%d written · %d already known · %d refused by the tree service:\n%s", written, known, failed, bad.String())
		if failed > tmReceiptLines {
			out += fmt.Sprintf("  … and %d more\n", failed-tmReceiptLines)
		}
		reply(re, out)
	}
	if moved > 0 || unclear > 0 {
		// said by mail only (a file is a copy that ages); the live map follows silently
		tm.mu.Lock()
		tm.counts["edits_moved_frame"] += int64(moved)
		tm.counts["edits_frame_unclear"] += int64(unclear)
		tm.mu.Unlock()
		log.Printf("tree-mail: key=%q position frames: %d edits filed at corrected positions, %d unclear", key.name, moved, unclear)
		out := ""
		if moved > 0 {
			out += fmt.Sprintf("%d of your edits were made on a file from before the tree positions there were corrected (by a few metres). Nothing is lost: each is filed on the same tree at its position of today. Your file keeps working, but it shows the trees at the old place — export them again when convenient.\n", moved)
		}
		if unclear > 0 {
			out += fmt.Sprintf("\n%d rows could be the tree before or after that correction and were taken as today's:\n%s", unclear, frameNote.String())
			if unclear > 5 {
				out += fmt.Sprintf("  … and %d more\n", unclear-5)
			}
			out += "If one landed on the wrong tree: export the trees again and repeat the edit on the new file.\n"
		}
		reply(re, out)
	}
	return false
}

func tmRowList(rows []int) string {
	var s []string
	for i, r := range rows {
		if i == 5 {
			s = append(s, "…")
			break
		}
		s = append(s, strconv.Itoa(r))
	}
	if len(rows) == 1 {
		return "row " + s[0]
	}
	return "rows " + strings.Join(s, ", ")
}

// tmOtherDomainText — the key is right, the sender's mail domain is not the
// one the key was issued for. The domain is not named.
const tmOtherDomainText = `Not subscribed: this key was issued for the addresses of one organisation, and this address is not one of them.

Send "subscribe <key>" again from your address there.
`

// tmKeyInUseText — the key is right and every address it may bind is taken.
func tmKeyInUseText(addrs int, bound bool) string {
	var b strings.Builder
	if addrs == 1 {
		b.WriteString("Not subscribed: this key is already in use by another address. A key is personal — one key, one address.\n\n")
	} else {
		fmt.Fprintf(&b, "Not subscribed: this key is already in use by %d addresses, as many as it allows.\n\n", addrs)
	}
	b.WriteString(`Your key, new address?   Mail "stop" from the old address, then "subscribe <key>" from this one.
Somebody else's key?     Ask the person who gave you this one for a key of your own.

Nothing was changed`)
	if bound {
		b.WriteString(": this address keeps the subscription it had")
	}
	return b.String() + ".\n"
}

// tmHelpText — what to send back; the reply to "subscribe" and "help". Short
// on purpose: three steps and one table, read on a phone at a glance.
func tmHelpText(c *tmConfig, k *tmKey, welcome bool) string {
	var b strings.Builder
	if welcome {
		b.WriteString("Subscribed — this address may now send tree edits.\n\n")
	}
	fmt.Fprintf(&b, `1  EXPORT  QGIS "Trees as points (current view)" as KML or CSV · Locus Map as KMZ
2  RENAME  the trees you checked (table below)
3  MAIL    the whole file to %s

NAME            MEANS
Fichte          species (German, English, Latin, code)
25 m            height
bhd 40 cm       stem diameter
gefällt 2025    felled · also windwurf, tot, gebrochen, käfer, weg
kein baum       not a tree (mast, pole)
gleicher baum   a second top of the tree beside it
Eiche 15 m      on a new point: adds a missing tree

Combine: "Fichte 25 m bhd 40 cm". Untouched trees are ignored.
Comment: type below the table in the description, or a "note" column. Notes are public.

Subjects: undo (+ the same file) · help · stop
Limits: 1 MB per mail · %d files · %d points per file · %d edits per day
`, c.addr, tmAttachments, tmMaxRows, min(k.daily, tmAddrDaily))
	return b.String()
}

// ── relay ──

type tmResult struct {
	http  int
	why   string
	moved bool   // filed on the same tree at its corrected position: the file is from before a correction
	hint  string // the peer could not tell which frame the row was in
}

// tmPeerFrame — the peer's frame{} of an answer (#position-frames).
type tmPeerFrame struct {
	Moved     []float64 `json:"moved_m"`
	Ambiguous bool      `json:"ambiguous"`
	Hint      string    `json:"hint"`
}

// tmPeerItem — what we read of one batch result.
type tmPeerItem struct {
	HTTP       int             `json:"http"`
	Error      string          `json:"error"`
	Reason     string          `json:"reason"`
	DidYouMean json.RawMessage `json:"did_you_mean"`
	Frame      *tmPeerFrame    `json:"frame"` // flags
	Tree       *struct {
		Frame *tmPeerFrame `json:"frame"`
	} `json:"tree"` // tree reports
	Candidates []struct {
		Height float64 `json:"height_m"`
		Dist   float64 `json:"dist_m"`
	} `json:"candidates"`
}

func (p *tmPeerItem) why() string {
	s := p.Error
	if s == "" {
		s = "refused (" + strconv.Itoa(p.HTTP) + ")"
	}
	if p.Reason == "not_yours" {
		s = "nothing of yours on this tree"
	}
	var names []string
	var raw []interface{}
	if json.Unmarshal(p.DidYouMean, &raw) == nil {
		for _, v := range raw {
			switch t := v.(type) {
			case string:
				names = append(names, t)
			case map[string]interface{}:
				for _, k := range []string{"de", "taxon", "en", "name"} {
					if n, _ := t[k].(string); n != "" {
						names = append(names, n)
						break
					}
				}
			case []interface{}:
				if len(t) > 1 {
					if n, _ := t[1].(string); n != "" {
						names = append(names, n)
					}
				}
			}
			if len(names) == 4 {
				break
			}
		}
	}
	if len(names) > 0 {
		s += " — did you mean " + strings.Join(names, ", ") + "?"
	}
	if p.HTTP == 409 && len(p.Candidates) > 1 {
		s += fmt.Sprintf(" (%d trees in reach)", len(p.Candidates))
	}
	return s
}

// tmRelay — the items through the relay's bucket in batches. A row without a
// tree id is first a report on the tree at its point; where the peer finds no
// tree there (404) it is sent again as an added tree (undo: as the withdrawal
// of that added tree).
func tmRelay(ctx context.Context, items []tmItem, undo bool) []tmResult {
	res := make([]tmResult, len(items))
	idx := make([]int, len(items))
	bodies := make([]map[string]interface{}, len(items))
	idx, bodies = idx[:0], bodies[:0]
	for i := range items {
		// the relay's own rules: tree layer, bounded strings, no key in the body
		if why := treeReportClean(items[i].body); why != "" {
			res[i] = tmResult{http: 400, why: why}
			continue
		}
		idx, bodies = append(idx, i), append(bodies, items[i].body)
	}
	tmFrames(ctx, items)
	tmRelayPass(ctx, idx, bodies, res)
	var idx2 []int
	var bodies2 []map[string]interface{}
	for i, it := range items {
		if !it.add || res[i].http != 404 {
			continue
		}
		b := map[string]interface{}{}
		for k, v := range it.body {
			b[k] = v
		}
		b["claim"] = "missing"
		b["client_id"] = tmClientID(fmt.Sprint(b["reporter"]), b["lon"].(float64), b["lat"].(float64))
		if undo {
			b["retract"] = true
		}
		idx2, bodies2 = append(idx2, i), append(bodies2, b)
	}
	if len(idx2) > 0 {
		tmRelayPass(ctx, idx2, bodies2, res)
	}
	return res
}

func tmRelayPass(ctx context.Context, idx []int, bodies []map[string]interface{}, res []tmResult) {
	for len(idx) > 0 {
		n := 0
		var batch []byte
		for n < len(idx) && n < treeReportBatch {
			bodies[n]["source"] = tmSource
			b, _ := json.Marshal(map[string]interface{}{"reports": bodies[:n+1]})
			if len(b) > treeReportBodyMax-1024 && n > 0 {
				break
			}
			batch, n = b, n+1
		}
		out, status, err := tmPost(ctx, batch)
		var ans struct {
			Results []tmPeerItem `json:"results"`
		}
		if err == nil {
			json.Unmarshal(out, &ans)
		}
		for k := 0; k < n; k++ {
			r := &res[idx[k]]
			switch {
			case err != nil || len(ans.Results) != n:
				*r = tmResult{http: 503, why: "the tree service did not take the batch (" + strconv.Itoa(status) + ") — send the file again later"}
			case ans.Results[k].HTTP == 200 || ans.Results[k].HTTP == 201:
				*r = tmResult{http: ans.Results[k].HTTP}
				fr := ans.Results[k].Frame
				if t := ans.Results[k].Tree; t != nil && t.Frame != nil {
					fr = t.Frame
				}
				if fr != nil {
					r.moved = len(fr.Moved) == 2 && (fr.Moved[0] != 0 || fr.Moved[1] != 0)
					if fr.Ambiguous {
						if r.hint = fr.Hint; r.hint == "" {
							r.hint = "the position fits the tree before and after a correction — taken as today's"
						}
					}
				}
			default:
				*r = tmResult{http: ans.Results[k].HTTP, why: ans.Results[k].why()}
			}
		}
		idx, bodies = idx[n:], bodies[n:]
	}
}

// tmPost — one batch: a bucket token, the POST, a few patient retries when
// the peer is busy.
func tmPost(ctx context.Context, body []byte) (out []byte, status int, err error) {
	for try := 0; ; try++ {
		if err = treeReportWait(ctx); err != nil {
			return nil, 0, err
		}
		var ra string
		status, out, ra, err = treeReportDo(ctx, body, "tree-mail")
		if err == nil && status != 429 && status < 500 {
			if status != 200 && status != 201 {
				return out, status, errors.New("refused")
			}
			return out, status, nil
		}
		if try == 2 {
			if err == nil {
				err = errors.New("busy")
			}
			return out, status, err
		}
		wait := time.Duration(5*(try+1)) * time.Second
		if s, e := strconv.Atoi(ra); e == nil && s > 0 && s <= 60 {
			wait = time.Duration(s) * time.Second
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		}
	}
}

// tmFramePieces — the most pieces of today's trees read to place a file's rows by height.
const tmFramePieces = 16

// tmFramePiece — today's trees of a piece; a var so tests decide.
var tmFramePiece = func(i, j int) ([]byte, bool) {
	if peers().ObsAPI == "" {
		return nil, false
	}
	rows, _, _, err := neOGCPiece(i, j)
	return rows, err == nil
}

// tmFrames — the position frame of every row that names a tree of an export
// and does not carry one (an export from before frames, a KML that lost the
// field). #position-frames, bulk import:
//
//  1. ask the peer which frame the file's points are in, per KG. No KG listed
//     = none of them ever moved: nothing to send.
//  2. the LiDAR height decides where it is given: a row whose position AND
//     height are an apex of today's copy is in today's frame — the frame that
//     apex carries, whatever KG it is in.
//  3. else the peer's answer for the row's KG, when the KG boxes around the
//     point agree (treeFrameOf); else nothing — the peer infers it.
//
// A row that carries a frame keeps it. Added trees and drawn points are
// real-world positions and never get one.
func tmFrames(ctx context.Context, items []tmItem) {
	var need []int
	var pts [][2]float64
	for i := range items {
		b := items[i].body
		if !items[i].apex || b == nil || b["frame"] != nil {
			continue
		}
		lon, ok1 := b["lon"].(float64)
		lat, ok2 := b["lat"].(float64)
		if !ok1 || !ok2 {
			continue
		}
		need, pts = append(need, i), append(pts, [2]float64{lon, lat})
	}
	if len(need) == 0 {
		return
	}
	kgs, err := treeFrameAsk(ctx, pts)
	if err != nil || len(kgs) == 0 {
		return
	}
	// 2. by height, where the file is small enough to read today's trees for it
	type ij struct{ i, j int }
	type ll struct{ lon, lat int32 }
	type hf struct{ h, f byte }
	pieces := map[ij]bool{}
	for _, p := range pts {
		pieces[ij{int(math.Floor(p[0] / neOGCPieceStep)), int(math.Floor(p[1] / neOGCPieceStep))}] = true
	}
	today := map[ll]hf{}
	if len(pieces) <= tmFramePieces {
		for p := range pieces {
			rows, ok := tmFramePiece(p.i, p.j)
			if !ok {
				continue
			}
			for o := 0; o+neOGCRow <= len(rows); o += neOGCRow {
				today[ll{int32(binary.LittleEndian.Uint32(rows[o:])), int32(binary.LittleEndian.Uint32(rows[o+4:]))}] = hf{rows[o+8], rows[o+12]}
			}
		}
	}
	byHeight, byKG := 0, 0
	for n, i := range need {
		it := &items[i]
		if a, ok := today[ll{int32(math.Round(pts[n][0] * 1e6)), int32(math.Round(pts[n][1] * 1e6))}]; ok && it.h0 > 0 && math.Abs(it.h0-float64(a.h)/4) <= 0.13 {
			it.body["frame"] = neOGCFrame(a.f)
			byHeight++
			continue
		}
		if f := treeFrameOf(kgs, pts[n][0], pts[n][1]); f != "" {
			it.body["frame"] = f
			byKG++
		}
	}
	tm.mu.Lock()
	tm.counts["frame_by_height"] += int64(byHeight)
	tm.counts["frame_by_kg"] += int64(byKG)
	tm.mu.Unlock()
}
