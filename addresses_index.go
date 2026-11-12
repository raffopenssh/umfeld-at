// addresses_index.go — the in-memory street index behind /api/v1/addresses/search.
//
// The find box is typed into on a phone: "handel-mazetti-promenade" (one z),
// "mariahilferstrasse 100", "hauptstr. 12, 4020 linz", "sankt pölten
// rathausplatz". An FTS prefix match answers none of them, so the streets
// (≈ 135 k BEV street keys, plus the named OSM roads no BEV street covers) are
// held in RAM as canonical keys → postings and matched in stages:
//
//	1 strict   every word is a prefix of a key of the street (name token, a
//	           street-type suffix "…straße", a joined form "mariahilferstr", or
//	           a place token: Gemeinde, Zustellort, Ortschaft); PLZ must fit
//	2 joined   two adjacent words glued ("maria hilfer" → "mariahilfer")
//	3 fuzzy    words within edit distance 1 (2 from 7 letters) of a key
//	4 no PLZ   the PLZ dropped
//	5 partial  one word may stay unanswered
//
// The first stage that answers wins; stages 3–5 mark their hits `approx`.
// Canonical key (adxKey): lower case, diacritics off, ß→ss, ae/oe/ue→a/o/u,
// ph→f, th→t, ck→k, y→i, ai→ei, doubled letters single, …strasse→…str,
// sankt→st, doktor→dr — so the common misspellings are *exact* matches and
// never reach the fuzzy stage.
//
// Fixed size (one entry per street, rebuilt when data/addresses.db is
// swapped); size line `addresses_index` on debug/inflight.
// House numbers stay in SQLite (adr, 2.5 M rows) and are read for the
// best-ranked streets only.
package main

import (
	"database/sql"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

type adxDoc struct {
	skz                            int64
	name, gkz, gem, ort, osch, plz string
	lon, lat                       float64
	n                              int32
	w                              float32 // importance: addresses in the Gemeinde
	osm                            bool
}

type adxIndex struct {
	docs    []adxDoc
	keys    []string // sorted, distinct
	off     []uint32 // postings of keys[i] = post[off[i]:off[i+1]]
	post    []uint32 // doc<<3 | kind
	plzGkz  map[string]map[string]struct{}
	mtime   time.Time
	bytes   int64
	osmDocs int
	buildMs int64
}

const (
	adxFirst  = 0 // the first token of the street name
	adxName   = 1 // a later token of the name
	adxSuffix = 2 // street-type suffix split off a compound ("…platz")
	adxJoin   = 3 // name tokens glued
	adxPlace  = 4 // Gemeinde / Zustellort / Ortschaft token
	adxNone   = 255
	adxMiss   = 60 // penalty of an unanswered word (stage 5)
)

var adxKindPen = [8]uint8{0, 4, 4, 1, 15} // place: a street named after the word comes before every street of that place

// adxFoldMap — diacritics the registers and OSM carry; anything else that is
// not a letter or digit separates tokens.
var adxFoldMap = map[rune]string{
	'ä': "a", 'ö': "o", 'ü': "u", 'ß': "ss", 'á': "a", 'à': "a", 'â': "a", 'ã': "a", 'å': "a", 'ā': "a", 'ą': "a",
	'é': "e", 'è': "e", 'ê': "e", 'ë': "e", 'ě': "e", 'ę': "e", 'ē': "e", 'í': "i", 'ì': "i", 'î': "i", 'ï': "i",
	'ó': "o", 'ò': "o", 'ô': "o", 'õ': "o", 'ő': "o", 'ø': "o", 'ú': "u", 'ù': "u", 'û': "u", 'ů': "u", 'ű': "u",
	'ý': "y", 'ÿ': "y", 'č': "c", 'ć': "c", 'ç': "c", 'š': "s", 'ś': "s", 'ž': "z", 'ź': "z", 'ż': "z", 'ř': "r",
	'ň': "n", 'ñ': "n", 'ń': "n", 'ď': "d", 'đ': "d", 'ť': "t", 'ľ': "l", 'ł': "l", 'æ': "ae", 'œ': "oe",
}

// adxFold — lower case, diacritics off; separators become spaces.
func adxFold(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 4)
	for _, r := range strings.ToLower(s) {
		switch {
		case r < 128 && (r >= 'a' && r <= 'z' || r >= '0' && r <= '9'):
			b.WriteByte(byte(r))
		case adxFoldMap[r] != "":
			b.WriteString(adxFoldMap[r])
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			b.WriteByte(' ')
		}
	}
	return b.String()
}

var adxRepl = strings.NewReplacer("ae", "a", "oe", "o", "ue", "u", "ph", "f", "th", "t", "ck", "k", "y", "i")

// adxKey — the canonical form of one folded token (see the file comment).
func adxKey(t string) string {
	t = strings.ReplaceAll(adxRepl.Replace(t), "ai", "ei")
	b := make([]byte, 0, len(t))
	for i := 0; i < len(t); i++ {
		if i > 0 && t[i] == t[i-1] && (t[i] < '0' || t[i] > '9') {
			continue
		}
		b = append(b, t[i])
	}
	t = string(b)
	switch t {
	case "sankt":
		return "st"
	case "doktor":
		return "dr"
	case "profesor":
		return "prof"
	case "wr":
		return "wiener"
	}
	if strings.HasSuffix(t, "strase") {
		return t[:len(t)-6] + "str"
	}
	return t
}

// adxSuffixes — street-type endings split off compounds, as keys.
var adxSuffixes = func() []string {
	var out []string
	for _, s := range []string{"strasse", "gasse", "weg", "platz", "ring", "promenade", "zeile", "steig", "kai", "ufer", "siedlung", "park", "hof", "markt", "graben", "berg", "feld", "dorf", "bach", "brucke", "allee", "steg", "pfad", "stiege", "anger", "damm"} {
		out = append(out, adxKey(s))
	}
	return out
}()

func adxSuffixOf(k string) string {
	for _, s := range adxSuffixes {
		if len(k) >= len(s)+3 && strings.HasSuffix(k, s) {
			return s
		}
	}
	return ""
}

// adxTitle — tokens a name is not known by ("Dr.-Karl-Renner-Ring").
func adxTitle(k string) bool {
	return k == "dr" || k == "st" || k == "ing" || k == "prof" || k == "dipl" || k == "am" || k == "an" || k == "der" || k == "im" || k == "in" || k == "zur" || k == "zum"
}

// adxFullKey — the whole name as one key; the same for "Doktor-Karl-Renner-Ring"
// and "Dr. Karl Renner-Ring".
func adxFullKey(name string) string {
	var b strings.Builder
	for _, t := range strings.Fields(adxFold(name)) {
		b.WriteString(adxKey(t))
	}
	return adxKey(b.String())
}

// adxNameKeys — the keys a street name answers to, with their kind.
func adxNameKeys(name string, add func(key string, kind uint8)) {
	toks := strings.Fields(adxFold(name))
	first := true
	for _, t := range toks {
		k := adxKey(t)
		if first && !adxTitle(k) {
			add(k, adxFirst)
			first = false
		} else {
			add(k, adxName)
		}
		if s := adxSuffixOf(k); s != "" {
			add(s, adxSuffix)
		}
	}
	if len(toks) > 1 {
		add(adxKey(strings.Join(toks, "")), adxJoin)
		if len(toks) > 2 {
			for i := 0; i+1 < len(toks); i++ {
				add(adxKey(toks[i]+toks[i+1]), adxJoin)
			}
		}
	}
}

func adxPlaceKeys(d *adxDoc, add func(key string, kind uint8)) {
	for _, s := range [3]string{d.gem, d.ort, d.osch} {
		toks := strings.Fields(adxFold(s))
		for _, t := range toks {
			add(adxKey(t), adxPlace)
		}
		if len(toks) == 2 {
			add(adxKey(toks[0]+toks[1]), adxPlace)
		}
	}
}

// adxBuild — keys and postings over docs.
func adxBuild(docs []adxDoc, plzGkz map[string]map[string]struct{}) *adxIndex {
	t0 := time.Now()
	ix := &adxIndex{docs: docs, plzGkz: plzGkz}
	gemN := map[string]int{}
	for i := range docs {
		gemN[docs[i].gkz] += int(docs[i].n)
	}
	type kp struct {
		key string
		p   uint32
	}
	all := make([]kp, 0, len(docs)*8)
	seen := map[string]uint8{}
	for i := range docs {
		d := &docs[i]
		d.w = float32(gemN[d.gkz])
		for k := range seen {
			delete(seen, k)
		}
		add := func(k string, kind uint8) {
			if k == "" {
				return
			}
			if o, ok := seen[k]; !ok || adxKindPen[kind] < adxKindPen[o] {
				seen[k] = kind
			}
		}
		adxNameKeys(d.name, add)
		adxPlaceKeys(d, add)
		for k, kind := range seen {
			all = append(all, kp{k, uint32(i)<<3 | uint32(kind)})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].key != all[j].key {
			return all[i].key < all[j].key
		}
		return all[i].p < all[j].p
	})
	ix.post = make([]uint32, len(all))
	for i := range all {
		if i == 0 || all[i].key != all[i-1].key {
			ix.keys = append(ix.keys, all[i].key)
			ix.off = append(ix.off, uint32(i))
			ix.bytes += int64(len(all[i].key)) + 16 + 4
		}
		ix.post[i] = all[i].p
	}
	ix.off = append(ix.off, uint32(len(all)))
	ix.bytes += int64(len(all)) * 4
	for i := range docs {
		d := &docs[i]
		ix.bytes += int64(len(d.name)+len(d.gkz)+len(d.gem)+len(d.ort)+len(d.osch)+len(d.plz)) + 150
		if d.osm {
			ix.osmDocs++
		}
	}
	ix.buildMs = time.Since(t0).Milliseconds()
	return ix
}

// ── query ────────────────────────────────────────────────────────────────────

type adxWord struct {
	k, alt string // canonical key; alt = "…str" when the word ends in a typed-out part of "straße"
}

func adxWordOf(tok string) adxWord {
	w := adxWord{k: adxKey(tok)}
	for _, p := range [...]string{"strase", "stras", "stra"} {
		if strings.HasSuffix(w.k, p) {
			w.alt = w.k[:len(w.k)-len(p)] + "str"
			break
		}
	}
	return w
}

var adxPenPool = sync.Pool{New: func() interface{} { return []uint8(nil) }}

func (ix *adxIndex) penBuf() []uint8 {
	b := adxPenPool.Get().([]uint8)
	if cap(b) < len(ix.docs) {
		b = make([]uint8, len(ix.docs))
	}
	b = b[:len(ix.docs)]
	for i := range b {
		b[i] = adxNone
	}
	return b
}

func (ix *adxIndex) addKey(pen []uint8, ki int, base uint8, nameOnly bool) {
	for _, p := range ix.post[ix.off[ki]:ix.off[ki+1]] {
		v := base + adxKindPen[p&7]
		if base > 0 && p&7 == adxJoin {
			v += 6 // a glued form answers the glued spelling, not a first letter
		}
		if d := p >> 3; v < pen[d] {
			if nameOnly && p&7 == adxPlace {
				continue
			}
			pen[d] = v
		}
	}
}

// prefix — every key starting with k: exact 0, longer 10 + ½ per extra letter.
func (ix *adxIndex) prefix(pen []uint8, k string, nameOnly bool) {
	if k == "" {
		return
	}
	lo := sort.SearchStrings(ix.keys, k)
	for i := lo; i < len(ix.keys) && strings.HasPrefix(ix.keys[i], k); i++ {
		base := uint8(0)
		if x := len(ix.keys[i]) - len(k); x > 0 {
			if x > 8 {
				x = 8
			}
			base = uint8(10 + x/2)
		}
		ix.addKey(pen, i, base, nameOnly)
	}
}

// fuzzy — keys with the same first letter within edit distance 1 (2 from 7
// letters) of k: 30 per edit. Only the last word (the one being typed) may
// be the beginning of a longer key.
func (ix *adxIndex) fuzzy(pen []uint8, k string, open, nameOnly bool) {
	if len(k) < 4 {
		return
	}
	maxd := 1
	if len(k) >= 7 {
		maxd = 2
	}
	lo := sort.SearchStrings(ix.keys, k[:1])
	for i := lo; i < len(ix.keys) && ix.keys[i][0] == k[0]; i++ {
		key := ix.keys[i]
		if len(key) < len(k)-maxd || (!open && len(key) > len(k)+maxd) {
			continue
		}
		d := adxEditPrefix(k, key, maxd)
		if d == 0 || d > maxd {
			continue
		}
		base := 30 * d
		if x := len(key) - len(k); x > 0 {
			if x > 8 {
				x = 8
			}
			base += 10 + x/2
		}
		ix.addKey(pen, i, uint8(base), nameOnly)
	}
}

// adxEditPrefix — optimal-string-alignment distance between a and the best
// prefix of b, cut off above maxd.
func adxEditPrefix(a, b string, maxd int) int {
	if len(b) > len(a)+maxd {
		b = b[:len(a)+maxd]
	}
	var r0, r1, r2 [72]int
	if len(a) > 64 || len(b) > 70 {
		return maxd + 1
	}
	prev2, prev, cur := r0[:len(b)+1], r1[:len(b)+1], r2[:len(b)+1]
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		min := i
		for j := 1; j <= len(b); j++ {
			c := 1
			if a[i-1] == b[j-1] {
				c = 0
			}
			v := prev[j-1] + c
			if x := prev[j] + 1; x < v {
				v = x
			}
			if x := cur[j-1] + 1; x < v {
				v = x
			}
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				if x := prev2[j-2] + 1; x < v {
					v = x
				}
			}
			cur[j] = v
			if v < min {
				min = v
			}
		}
		if min > maxd {
			return maxd + 1
		}
		prev2, prev, cur = prev, cur, prev2
	}
	best := maxd + 1
	for j := 0; j <= len(b); j++ {
		if prev[j] < best {
			best = prev[j]
		}
	}
	return best
}

type adxCand struct {
	doc    int32
	pen    int32   // text penalty, 10 = one "prefix instead of exact"
	geo    float32 // smaller = nearer / bigger place
	inView bool
	full   bool // the query covers the whole street name
}

type adxQuery struct {
	words  []adxWord
	plz    string
	bbox   *[4]float64
	c      *[2]float64
	fuzzy  bool
	miss   int
	approx bool
}

const adxKeep = 400 // candidates per class (in view / elsewhere) that reach the fine ranking

// match — one stage. Candidates come back ranked.
func (ix *adxIndex) match(q *adxQuery) []adxCand {
	pens := make([][]uint8, len(q.words), len(q.words)+1)
	fill := func(p []uint8, i int, nameOnly bool) {
		w := q.words[i]
		ix.prefix(p, w.k, nameOnly)
		if w.alt != "" {
			ix.prefix(p, w.alt, nameOnly)
		}
		if q.fuzzy {
			ix.fuzzy(p, w.k, i == len(q.words)-1, nameOnly)
		}
	}
	for i := range q.words {
		pens[i] = ix.penBuf()
		fill(pens[i], i, false)
	}
	// a street found with a word unanswered must be found by its name:
	// "hauptstraße innsbruck" is not every street of Innsbruck
	var named []uint8
	if q.miss > 0 {
		named = ix.penBuf()
		for i := range q.words {
			if len(q.words[i].k) >= 4 {
				fill(named, i, true)
			}
		}
		pens = append(pens, named)
		pens = pens[:len(q.words)]
		defer adxPenPool.Put(named) //nolint:staticcheck
	}
	defer func() {
		for _, p := range pens {
			adxPenPool.Put(p) //nolint:staticcheck
		}
	}()
	var plzG map[string]struct{}
	if q.plz != "" {
		plzG = ix.plzGkz[q.plz]
	}
	var kx float64
	if q.c != nil {
		kx = math.Cos(q.c[1] * math.Pi / 180)
	}
	var cls [2][]adxCand
	for d := range ix.docs {
		s, miss := int32(0), 0
		for _, p := range pens {
			if v := p[d]; v == adxNone {
				miss++
				s += adxMiss
			} else {
				s += int32(v)
			}
		}
		if miss > q.miss || (miss > 0 && named[d] == adxNone) {
			continue
		}
		doc := &ix.docs[d]
		if q.plz != "" {
			if doc.plz != q.plz {
				if _, ok := plzG[doc.gkz]; !ok {
					continue
				}
				s += 3
			}
		}
		if doc.osm {
			s += 2
		}
		c := adxCand{doc: int32(d), pen: s}
		if q.bbox != nil {
			c.inView = doc.lon >= q.bbox[0] && doc.lon <= q.bbox[2] && doc.lat >= q.bbox[1] && doc.lat <= q.bbox[3]
		}
		wgt := float64(doc.w)
		if q.c != nil {
			km := math.Hypot((doc.lon-q.c[0])*kx, doc.lat-q.c[1]) * 111.32
			c.geo = float32(km / math.Sqrt(math.Min(math.Max(wgt/2000, 1), 50)))
		} else {
			c.geo = float32(1000 / math.Sqrt(wgt+1))
		}
		i := 1
		if c.inView {
			i = 0
		}
		cls[i] = append(cls[i], c)
	}
	var out []adxCand
	for _, cs := range cls {
		if len(cs) > adxKeep {
			sort.Slice(cs, func(i, j int) bool {
				if cs[i].pen/5 != cs[j].pen/5 {
					return cs[i].pen < cs[j].pen
				}
				return cs[i].geo < cs[j].geo
			})
			cs = cs[:adxKeep]
		}
		out = append(out, cs...)
	}
	// coverage: how much of the street name the words answer — "Am Hof"
	// before "Am Hofacker", "Ring" before "Ringweg".
	qk := map[string]struct{}{}
	for _, w := range q.words {
		qk[w.k] = struct{}{}
		if w.alt != "" {
			qk[w.alt] = struct{}{}
		}
	}
	for i := range out {
		c := &out[i]
		total, hit := 0, 0
		name := ix.docs[c.doc].name
		for _, t := range strings.Fields(adxFold(name)) {
			k := adxKey(t)
			_, ok := qk[k]
			if !ok && adxTitle(k) {
				continue
			}
			total++
			if ok {
				hit++
			}
		}
		if total > 0 {
			if _, ok := qk[adxFullKey(name)]; ok {
				hit = total
			}
			c.pen -= int32(15 * hit / total)
			c.full = hit == total
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return adxLess(&out[i], &out[j], ix) })
	return out
}

func adxLess(a, b *adxCand, ix *adxIndex) bool {
	if a.inView != b.inView {
		return a.inView
	}
	if a.pen/5 != b.pen/5 {
		return a.pen < b.pen
	}
	if a.geo != b.geo {
		return a.geo < b.geo
	}
	if a.pen != b.pen {
		return a.pen < b.pen
	}
	da, db := &ix.docs[a.doc], &ix.docs[b.doc]
	if da.n != db.n {
		return da.n > db.n
	}
	return da.name < db.name
}

// adxTrim — a correction is a guess: only the guesses about as good as the
// best one ("kärtnerstraße wien" is Kärntner Straße, not also Körner-Straße).
func adxTrim(cs []adxCand) []adxCand {
	best := cs[0].pen
	for _, c := range cs {
		if c.pen < best {
			best = c.pen
		}
	}
	out := cs[:0]
	for _, c := range cs {
		if c.pen <= best+20 {
			out = append(out, c)
		}
	}
	return out
}

// search — the staged ladder (file comment). approx reports that the answer
// is not what was typed, letter for letter.
func (ix *adxIndex) search(words []string, plz string, bbox *[4]float64, c *[2]float64) (out []adxCand, approx bool) {
	if len(words) > 8 {
		words = words[:8]
	}
	q := &adxQuery{plz: plz, bbox: bbox, c: c}
	for _, w := range words {
		for _, t := range strings.Fields(adxFold(w)) {
			if aw := adxWordOf(t); aw.k != "" {
				q.words = append(q.words, aw)
			}
		}
	}
	if len(q.words) == 0 && plz == "" {
		return nil, false
	}
	if out = ix.match(q); len(out) > 0 {
		return out, false
	}
	if len(q.words) > 1 { // 2 joined
		base := q.words
		for i := 0; i+1 < len(base); i++ {
			j := append(append([]adxWord{}, base[:i]...), adxWordOf(base[i].k+base[i+1].k))
			q.words = append(j, base[i+2:]...)
			if out = ix.match(q); len(out) > 0 {
				return out, false
			}
		}
		q.words = base
	}
	q.fuzzy = true
	if out = ix.match(q); len(out) > 0 {
		return adxTrim(out), true
	}
	if q.plz != "" && len(q.words) > 0 {
		p := q.plz
		q.plz = ""
		if out = ix.match(q); len(out) > 0 {
			return adxTrim(out), true
		}
		q.plz = p
	}
	if len(q.words) > 1 {
		q.miss = 1
		if out = ix.match(q); len(out) > 0 {
			return adxTrim(out), true
		}
		if q.plz != "" {
			q.plz = ""
			if out = ix.match(q); len(out) > 0 {
				return adxTrim(out), true
			}
		}
	}
	return nil, false
}

// ── build from the stores ────────────────────────────────────────────────────

var adxState struct {
	mu       sync.Mutex
	ix       *adxIndex
	building bool
	done     chan struct{}
}

// addressesIndex — the index for db, built on first use and again (in the
// background, the old one answering meanwhile) when the store was swapped.
func addressesIndex(db *sql.DB) *adxIndex {
	addrState.mu.Lock()
	mt := addrState.mtime
	addrState.mu.Unlock()
	s := &adxState
	s.mu.Lock()
	ix := s.ix
	if ix != nil && ix.mtime.Equal(mt) {
		s.mu.Unlock()
		return ix
	}
	if !s.building {
		s.building, s.done = true, make(chan struct{})
		go func(done chan struct{}) {
			n, err := adxLoad(db)
			s.mu.Lock()
			if err != nil {
				log.Printf("📮 addresses index: %v", err)
			} else {
				n.mtime = mt
				s.ix = n
				log.Printf("📮 addresses index: %d streets (%d OSM-only), %d keys, %d MB, built in %d ms", len(n.docs), n.osmDocs, len(n.keys), n.bytes>>20, n.buildMs)
			}
			s.building = false
			s.mu.Unlock()
			close(done)
		}(s.done)
	}
	done := s.done
	s.mu.Unlock()
	if ix != nil {
		return ix
	}
	<-done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ix
}

// AddressesIndexWarm — build at startup so the first keystroke does not wait.
func AddressesIndexWarm() {
	if db := addressesDB(); db != nil {
		addressesIndex(db)
	}
}

func AddressesIndexStats() map[string]interface{} {
	s := &adxState
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ix == nil {
		return map[string]interface{}{"streets": 0, "building": s.building}
	}
	return map[string]interface{}{"streets": len(s.ix.docs), "osm_only": s.ix.osmDocs, "keys": len(s.ix.keys), "postings": len(s.ix.post), "bytes": s.ix.bytes, "build_ms": s.ix.buildMs, "building": s.building, "bound": "one entry per street (fixed)"}
}

func adxLoad(db *sql.DB) (*adxIndex, error) {
	t0 := time.Now()
	rows, err := db.Query(`SELECT skz, name, gkz, gemeinde, ort, ortschaft, plz, lon, lat, n FROM str`)
	if err != nil {
		return nil, err
	}
	docs := make([]adxDoc, 0, 170000)
	intern := map[string]string{}
	in := func(s sql.NullString) string {
		if v, ok := intern[s.String]; ok {
			return v
		}
		intern[s.String] = s.String
		return s.String
	}
	for rows.Next() {
		var d adxDoc
		var gkz, gem, ort, osch, plz sql.NullString
		if err := rows.Scan(&d.skz, &d.name, &gkz, &gem, &ort, &osch, &plz, &d.lon, &d.lat, &d.n); err != nil {
			rows.Close()
			return nil, err
		}
		d.gkz, d.gem, d.ort, d.osch, d.plz = in(gkz), in(gem), in(ort), in(osch), in(plz)
		docs = append(docs, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// PLZ → Gemeinden it serves: "Hauptstraße 12, 4020 Linz" must find the
	// street whose most common PLZ is 4040.
	plzGkz := map[string]map[string]struct{}{}
	for i := range docs {
		d := &docs[i]
		if d.plz == "" {
			continue
		}
		m := plzGkz[d.plz]
		if m == nil {
			m = map[string]struct{}{}
			plzGkz[d.plz] = m
		}
		m[d.gkz] = struct{}{}
	}
	nBEV := len(docs)
	docs = adxAddOSM(docs)
	ix := adxBuild(docs, plzGkz)
	ix.buildMs = time.Since(t0).Milliseconds()
	_ = nBEV
	return ix, nil
}

// adxAddOSM — named OSM roads (data/osm_proximity.db, ODbL) that no BEV
// street of the same name lies near: roads without a single address
// (promenades, pass roads, squares). One entry per name and ~4 km cluster,
// placed on the segment nearest the cluster's middle, labelled with the place
// of the nearest BEV street.
func adxAddOSM(docs []adxDoc) []adxDoc {
	InitOSMProximity()
	if osmDB == nil {
		return docs
	}
	rows, err := osmDB.Query(`SELECT l.name, (r.min_lon+r.max_lon)/2, (r.min_lat+r.max_lat)/2 FROM osm_lines l JOIN osm_lines_rtree r ON r.id = l.id
		WHERE l.cat = 'road' AND l.name != '' AND l.fclass NOT IN ('motorway','motorway_link','trunk','trunk_link','busway','service')`)
	if err != nil {
		return docs // table not imported: BEV only
	}
	defer rows.Close()
	type pt struct{ x, y float64 }
	type cl struct {
		sx, sy float64
		pts    []pt
	}
	byName := map[string][]*cl{}
	for rows.Next() {
		var name string
		var p pt
		if rows.Scan(&name, &p.x, &p.y) != nil {
			continue
		}
		var c *cl
		for _, k := range byName[name] {
			n := float64(len(k.pts))
			if math.Abs(k.sx/n-p.x) < 0.05 && math.Abs(k.sy/n-p.y) < 0.035 {
				c = k
				break
			}
		}
		if c == nil {
			c = &cl{}
			byName[name] = append(byName[name], c)
		}
		c.sx += p.x
		c.sy += p.y
		c.pts = append(c.pts, p)
	}
	nameKey := adxFullKey
	type placed struct {
		k, plz, ort string
	}
	have := map[placed]int{} // one entry per name and place: the cluster with most segments
	segs := map[int]int{}
	type cell struct{ x, y int32 }
	cellOf := func(x, y float64) cell { return cell{int32(math.Floor(x / 0.03)), int32(math.Floor(y / 0.02))} }
	bevName := map[string][]int32{}
	grid := map[cell][]int32{}
	nBEV := len(docs)
	for i := 0; i < nBEV; i++ {
		k := nameKey(docs[i].name)
		bevName[k] = append(bevName[k], int32(i))
		c := cellOf(docs[i].lon, docs[i].lat)
		grid[c] = append(grid[c], int32(i))
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		k := nameKey(name)
		if len(k) < 3 {
			continue
		}
	next:
		for _, c := range byName[name] {
			n := float64(len(c.pts))
			mx, my := c.sx/n, c.sy/n
			for _, bi := range bevName[k] {
				if math.Abs(docs[bi].lon-mx) < 0.08 && math.Abs(docs[bi].lat-my) < 0.055 {
					continue next
				}
			}
			best, bd := c.pts[0], math.MaxFloat64
			for _, p := range c.pts {
				if d := math.Hypot(p.x-mx, p.y-my); d < bd {
					best, bd = p, d
				}
			}
			d := adxDoc{name: name, lon: math.Round(best.x*1e6) / 1e6, lat: math.Round(best.y*1e6) / 1e6, osm: true}
			// place: nearest BEV street within the 3 × 3 (then 7 × 7) cells
			cc := cellOf(best.x, best.y)
			near, nd := int32(-1), math.MaxFloat64
			for _, rad := range [2]int32{1, 3} {
				for dx := -rad; dx <= rad; dx++ {
					for dy := -rad; dy <= rad; dy++ {
						for _, bi := range grid[cell{cc.x + dx, cc.y + dy}] {
							if dd := math.Hypot((docs[bi].lon-best.x)*0.67, docs[bi].lat-best.y); dd < nd {
								near, nd = bi, dd
							}
						}
					}
				}
				if near >= 0 {
					break
				}
			}
			if near >= 0 {
				b := &docs[near]
				d.gkz, d.gem, d.ort, d.plz = b.gkz, b.gem, b.ort, b.plz
			}
			pk := placed{k, d.plz, d.ort}
			if j, ok := have[pk]; ok {
				if len(c.pts) > segs[j] {
					docs[j], segs[j] = d, len(c.pts)
				}
				continue
			}
			have[pk], segs[len(docs)] = len(docs), len(c.pts)
			docs = append(docs, d)
		}
	}
	return docs
}
