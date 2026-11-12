// addresses.go — BEV Adressregister (CC BY 4.0) as a compact SQLite store,
// and GET /api/v1/addresses/search for the landing glyph's find box.
//
// Source: "Adresse Relationale Tabellen – Stichtagsdaten" (BEV, CC BY 4.0,
// required notice "© Österreichisches Adressregister, Stichtagsdaten vom
// TT.MM.JJJJ"); docs/glyph-find-contract.md §1. Not the Katasterservice API
// (AGENTS.md inv. 14) — a plain open-data CSV download.
//
// Store (data/addresses.db, never committed, ≈ 110 MB):
//
//	str     one row per street key (SKZ): name, Gemeinde, Zustellort, modal PLZ,
//	        centroid, address count
//	str_fts FTS5 over name/ort/gemeinde/plz — streets only (138 k rows), the
//	        2.5 M addresses are reached through their street
//	adr     (skz, hnr, hof, plz, lon·1e6, lat·1e6) with an index on (skz, hnr)
//	meta    stichtag, source_url, rows
//
// Import: `umfeld-at -addresses-import <dir with ADRESSE.csv STRASSE.csv
// GEMEINDE.csv ORTSCHAFT.csv Aktualitaetsstand.txt>` — a separate process; the
// live API picks the swapped file up on its next query (the handle is reopened
// when the mtime changes).
//
// Search: alphabetic tokens → FTS prefix terms over the streets (AND); a token
// starting with a digit is a house number (exact or prefix on adr.hnr); a bare
// 4-digit token is also tried as a PLZ. The result is ranked in Go: in the
// caller's view first, then exact house number, then distance to the view
// centre, then bm25 — so what is on screen comes first while typing.
package main

import (
	"bufio"
	"database/sql"
	"encoding/csv"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	addressesDBPath    = "data/addresses.db"
	addressesSourceURL = "https://data.bev.gv.at/download/Adressregister/Adresse_Relationale_Tabellen_Stichtagsdaten.zip"
	addressesLicURL    = "https://www.bev.gv.at/Services/Downloads/Produktbezogene-Downloads/Unentgeltliche-Produkte/Adressregister.html"
)

var addrState struct {
	mu       sync.Mutex
	db       *sql.DB
	mtime    time.Time
	checked  time.Time
	stichtag string
	rows     int64
}

// addressesDB returns the read handle, reopening when the file was swapped.
// nil when the store is absent.
func addressesDB() *sql.DB {
	a := &addrState
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	if a.db != nil && now.Sub(a.checked) < 30*time.Second {
		return a.db
	}
	a.checked = now
	st, err := os.Stat(addressesDBPath)
	if err != nil {
		if a.db != nil {
			_ = a.db.Close()
			a.db = nil
		}
		return nil
	}
	if a.db != nil && st.ModTime().Equal(a.mtime) {
		return a.db
	}
	if a.db != nil {
		_ = a.db.Close()
		a.db = nil
	}
	registerTunedSQLite()
	db, err := sql.Open(sqliteTunedDriver, fmt.Sprintf("file:%s?mode=ro&_journal_mode=DELETE&_busy_timeout=5000&_cache_size=-16000", addressesDBPath))
	if err != nil {
		log.Printf("📮 addresses: open: %v", err)
		return nil
	}
	db.SetMaxOpenConns(4)
	a.db, a.mtime = db, st.ModTime()
	a.stichtag, a.rows = "", 0
	_ = db.QueryRow(`SELECT value FROM meta WHERE key='stichtag'`).Scan(&a.stichtag)
	var rs string
	_ = db.QueryRow(`SELECT value FROM meta WHERE key='rows'`).Scan(&rs)
	a.rows, _ = strconv.ParseInt(rs, 10, 64)
	log.Printf("📮 addresses: %s (Stichtag %s, %d addresses)", addressesDBPath, a.stichtag, a.rows)
	return db
}

// addressesStichtag — "2026-10-01" or "" when the store is absent.
func addressesStichtag() string {
	if addressesDB() == nil {
		return ""
	}
	addrState.mu.Lock()
	defer addrState.mu.Unlock()
	return addrState.stichtag
}

// addressesAttribution — the notice the BEV licence requires, verbatim form.
func addressesAttribution() string {
	s := addressesStichtag()
	if len(s) == 10 {
		s = s[8:10] + "." + s[5:7] + "." + s[0:4]
	} else {
		s = "TT.MM.JJJJ"
	}
	return "© Österreichisches Adressregister, Stichtagsdaten vom " + s
}

// ── import ───────────────────────────────────────────────────────────────────

func openBOMCSV(path string) (*csv.Reader, *os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	br := bufio.NewReaderSize(f, 1<<20)
	if b, _ := br.Peek(3); len(b) == 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		_, _ = br.Discard(3)
	}
	r := csv.NewReader(br)
	r.Comma = ';'
	r.LazyQuotes = true
	r.ReuseRecord = true
	r.FieldsPerRecord = -1
	return r, f, nil
}

// RunAddressesImport builds data/addresses.db from an unzipped Adressregister
// directory, atomically (tmp file → rename).
func RunAddressesImport(dir string) error {
	t0 := time.Now()
	stichtag := ""
	if b, err := os.ReadFile(filepath.Join(dir, "Aktualitaetsstand.txt")); err == nil {
		for _, ln := range strings.Split(string(b), "\n") {
			if i := strings.Index(ln, ": "); i > 0 && strings.Contains(ln, "Stichtag") {
				d := strings.TrimSpace(ln[i+2:])
				if len(d) == 10 && d[2] == '.' && d[5] == '.' {
					stichtag = d[6:10] + "-" + d[3:5] + "-" + d[0:2]
				}
			}
		}
	}
	if stichtag == "" {
		return fmt.Errorf("Aktualitaetsstand.txt: no Stichtag found")
	}
	// Gemeinden, Ortschaften
	gem := map[string]string{}
	if err := eachCSV(filepath.Join(dir, "GEMEINDE.csv"), func(rec []string) {
		if len(rec) >= 2 {
			gem[rec[0]] = rec[1]
		}
	}); err != nil {
		return err
	}
	ort := map[string]string{}
	if err := eachCSV(filepath.Join(dir, "ORTSCHAFT.csv"), func(rec []string) {
		if len(rec) >= 3 {
			ort[rec[0]+"/"+rec[1]] = rec[2]
		}
	}); err != nil {
		return err
	}
	// Streets (one SKZ can recur with a different GKZ/Zustellort — keyed by SKZ, first wins)
	type street struct {
		name, gkz, ort string
		sx, sy         float64
		n              int
		plz            map[string]int
		orts           map[string]bool
	}
	streets := map[string]*street{}
	if err := eachCSV(filepath.Join(dir, "STRASSE.csv"), func(rec []string) {
		if len(rec) < 6 || rec[0] == "SKZ" {
			return
		}
		if _, ok := streets[rec[0]]; ok {
			return
		}
		streets[rec[0]] = &street{name: strings.TrimSpace(rec[1]), gkz: rec[4], ort: strings.TrimSpace(rec[5]), plz: map[string]int{}, orts: map[string]bool{}}
	}); err != nil {
		return err
	}
	tmp := addressesDBPath + ".tmp"
	_ = os.Remove(tmp)
	db, err := sql.Open("sqlite3", "file:"+tmp+"?_journal_mode=OFF&_synchronous=OFF")
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA page_size=4096; PRAGMA cache_size=-200000;
		CREATE TABLE meta(key TEXT PRIMARY KEY, value TEXT);
		CREATE TABLE adr(skz INTEGER NOT NULL, hnr TEXT NOT NULL, hof TEXT, plz INTEGER, lon INTEGER NOT NULL, lat INTEGER NOT NULL);
		CREATE TABLE str(skz INTEGER PRIMARY KEY, name TEXT NOT NULL, gkz TEXT, gemeinde TEXT, ort TEXT, ortschaft TEXT, plz TEXT, lon REAL, lat REAL, n INTEGER);
		CREATE VIRTUAL TABLE str_fts USING fts5(name, ort, gemeinde, plz, ortschaft, content='str', content_rowid='skz', tokenize="unicode61 remove_diacritics 2", prefix='2 3 4');`); err != nil {
		return err
	}
	// Addresses — streamed, one transaction per 50 k rows
	r, f, err := openBOMCSV(filepath.Join(dir, "ADRESSE.csv"))
	if err != nil {
		return err
	}
	defer f.Close()
	hdr, err := r.Read()
	if err != nil {
		return err
	}
	col := map[string]int{}
	for i, h := range hdr {
		col[strings.TrimSpace(h)] = i
	}
	need := []string{"GKZ", "OKZ", "PLZ", "SKZ", "HAUSNRTEXT", "HNR_ADR_ZUSAMMEN", "HOFNAME", "RW", "HW", "EPSG"}
	for _, k := range need {
		if _, ok := col[k]; !ok {
			return fmt.Errorf("ADRESSE.csv: column %s missing", k)
		}
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	ins, err := tx.Prepare(`INSERT INTO adr(skz,hnr,hof,plz,lon,lat) VALUES(?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	var n, bad int64
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if len(rec) < len(hdr) {
			bad++
			continue
		}
		skzS := rec[col["SKZ"]]
		st := streets[skzS]
		if st == nil {
			bad++
			continue
		}
		epsg, _ := strconv.Atoi(rec[col["EPSG"]])
		rw, e1 := strconv.ParseFloat(rec[col["RW"]], 64)
		hw, e2 := strconv.ParseFloat(rec[col["HW"]], 64)
		if e1 != nil || e2 != nil {
			bad++
			continue
		}
		lon, lat, ok := GKToWGS84(epsg, rw, hw)
		if !ok {
			bad++
			continue
		}
		hnr := strings.TrimSpace(rec[col["HNR_ADR_ZUSAMMEN"]])
		if hnr == "" {
			hnr = strings.TrimSpace(rec[col["HAUSNRTEXT"]])
		}
		hof := strings.TrimSpace(rec[col["HOFNAME"]])
		plz, _ := strconv.Atoi(rec[col["PLZ"]])
		skz, _ := strconv.Atoi(skzS)
		var hofV interface{}
		if hof != "" {
			hofV = hof
		}
		if _, err := ins.Exec(skz, hnr, hofV, plz, int64(math.Round(lon*1e6)), int64(math.Round(lat*1e6))); err != nil {
			return err
		}
		st.sx += lon
		st.sy += lat
		st.n++
		st.plz[rec[col["PLZ"]]]++
		if o := ort[rec[col["GKZ"]]+"/"+rec[col["OKZ"]]]; o != "" {
			if st.ort == "" {
				st.ort = o
			}
			if !strings.EqualFold(o, st.ort) && !strings.EqualFold(o, gem[st.gkz]) {
				st.orts[o] = true
			}
		}
		n++
		if n%50000 == 0 {
			ins.Close()
			if err := tx.Commit(); err != nil {
				return err
			}
			if tx, err = db.Begin(); err != nil {
				return err
			}
			if ins, err = tx.Prepare(`INSERT INTO adr(skz,hnr,hof,plz,lon,lat) VALUES(?,?,?,?,?,?)`); err != nil {
				return err
			}
			if n%500000 == 0 {
				log.Printf("📮 addresses: %d rows …", n)
			}
		}
	}
	ins.Close()
	si, err := tx.Prepare(`INSERT INTO str(skz,name,gkz,gemeinde,ort,ortschaft,plz,lon,lat,n) VALUES(?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	ns := 0
	for skzS, st := range streets {
		if st.n == 0 {
			continue
		}
		best, bc := "", 0
		for p, c := range st.plz {
			if c > bc || (c == bc && p < best) {
				best, bc = p, c
			}
		}
		skz, _ := strconv.Atoi(skzS)
		var orts []string
		for o := range st.orts {
			orts = append(orts, o)
		}
		sort.Strings(orts)
		if _, err := si.Exec(skz, st.name, st.gkz, gem[st.gkz], st.ort, strings.Join(orts, " · "), best, math.Round(st.sx/float64(st.n)*1e6)/1e6, math.Round(st.sy/float64(st.n)*1e6)/1e6, st.n); err != nil {
			return err
		}
		ns++
	}
	si.Close()
	for k, v := range map[string]string{"stichtag": stichtag, "source_url": addressesSourceURL, "rows": strconv.FormatInt(n, 10), "streets": strconv.Itoa(ns), "built": time.Now().UTC().Format(time.RFC3339)} {
		if _, err := tx.Exec(`INSERT INTO meta VALUES(?,?)`, k, v); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	log.Printf("📮 addresses: %d rows, %d streets, %d skipped — indexing", n, ns, bad)
	if _, err := db.Exec(`CREATE INDEX adr_skz ON adr(skz, hnr); INSERT INTO str_fts(str_fts) VALUES('rebuild'); INSERT INTO str_fts(str_fts) VALUES('optimize'); VACUUM;`); err != nil {
		return err
	}
	if err := db.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, addressesDBPath); err != nil {
		return err
	}
	st, _ := os.Stat(addressesDBPath)
	log.Printf("📮 addresses: %s ready (%d MB, Stichtag %s) in %s", addressesDBPath, st.Size()>>20, stichtag, time.Since(t0).Round(time.Second))
	return nil
}

func eachCSV(path string, fn func([]string)) error {
	r, f, err := openBOMCSV(path)
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		rec, err := r.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		fn(rec)
	}
}

// ── search ───────────────────────────────────────────────────────────────────

type addrHit struct {
	Kind      string  `json:"kind"`
	Label     string  `json:"label"`
	Sub       string  `json:"sub"`
	Street    string  `json:"street"`
	Hnr       string  `json:"hnr,omitempty"`
	Hof       string  `json:"hof,omitempty"`
	PLZ       string  `json:"plz"`
	Ort       string  `json:"ort"`
	Ortschaft string  `json:"ortschaft,omitempty"`
	Gemeinde  string  `json:"gemeinde"`
	GKZ       string  `json:"gkz"`
	N         int     `json:"n,omitempty"`
	Lon       float64 `json:"lon"`
	Lat       float64 `json:"lat"`
	InView    bool    `json:"in_view"`
	DistKm    float64 `json:"dist_km,omitempty"`
	Approx    bool    `json:"approx,omitempty"` // a correction of what was typed (typo, dropped PLZ or word)
	Full      bool    `json:"full,omitempty"`   // the text names the whole street
	Src       string  `json:"src,omitempty"`    // "osm": a named OSM road no BEV street covers (ODbL)
	rank      float64
	exact     bool
	skz       int64
}

// addrTokens splits q into words and number tokens ("12", "12a", "12/3",
// "4020"); after[i] says whether nums[i] follows a word.
func addrTokens(q string) (words []string, nums []string, after []bool) {
	// split on anything but letters and digits, except inside a house number
	// ("12/3", "12-14", "12a" stay one token)
	q = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '/' || r == '-' {
			return r
		}
		return ' '
	}, q)
	for _, t := range strings.Fields(q) {
		if t[0] >= '0' && t[0] <= '9' {
			if len(t) > 12 {
				t = t[:12]
			}
			nums = append(nums, strings.Trim(t, "-/"))
			after = append(after, len(words) > 0)
			continue
		}
		for _, w := range strings.FieldsFunc(t, func(r rune) bool { return r == '/' || r == '-' }) {
			if len(w) > 64 {
				w = w[:64]
			}
			words = append(words, w)
		}
	}
	return
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// addrReading — which number is the PLZ and which the house number. The PLZ
// is the last 4-digit token that is a PLZ of the register; the house number
// the first other number behind a word ("1. Mai-Straße 5" → 5), else the
// first. A lone PLZ-shaped number behind a word may be a house number after
// all: alt carries that second reading.
func addrReading(ix *adxIndex, words, nums []string, after []bool) (plz, hnr, altHnr string, extra []string) {
	pi, hi := -1, -1
	for i := len(nums) - 1; i >= 0; i-- {
		if _, ok := ix.plzGkz[nums[i]]; ok && len(nums[i]) == 4 {
			pi = i
			break
		}
	}
	for i := range nums {
		if i != pi && after[i] {
			hi = i
			break
		}
	}
	if hi < 0 {
		for i := range nums {
			if i != pi {
				hi = i
				break
			}
		}
	}
	if pi >= 0 {
		plz = nums[pi]
		if hi < 0 && after[pi] {
			altHnr = plz
		}
	}
	if hi >= 0 {
		hnr = nums[hi]
	}
	// numbers that are neither may be part of the name ("1. Mai-Straße")
	for i, n := range nums {
		if i != pi && i != hi && isDigits(n) {
			extra = append(extra, n)
		}
	}
	return
}

// hnrLess — natural order of house numbers: 9 < 10 < 10a < 11.
func hnrLess(a, b string) bool {
	na, nb := 0, 0
	i, j := 0, 0
	for ; i < len(a) && a[i] >= '0' && a[i] <= '9'; i++ {
		na = na*10 + int(a[i]-'0')
	}
	for ; j < len(b) && b[j] >= '0' && b[j] <= '9'; j++ {
		nb = nb*10 + int(b[j]-'0')
	}
	if na != nb {
		return na < nb
	}
	return a[i:] < b[j:]
}

// addressesSearch — the query behind /api/v1/addresses/search: streets from
// the in-memory index (addresses_index.go: typo-tolerant, staged), house
// numbers from adr for the best-ranked streets. approx = the answer is a
// correction of what was typed.
func addressesSearch(db *sql.DB, q string, bbox *[4]float64, c *[2]float64, limit int) (out []addrHit, approx bool, err error) {
	ix := addressesIndex(db)
	if ix == nil {
		return nil, false, nil
	}
	words, nums, after := addrTokens(q)
	plz, hnr, altHnr, extra := addrReading(ix, words, nums, after)
	if len(words) == 0 && plz == "" {
		return nil, false, nil // a bare house number finds nothing on its own
	}
	cands, approx := ix.search(words, plz, bbox, c)
	if len(extra) > 0 { // "1 mai strasse 5": the 1 belongs to the name
		if c2, a2 := ix.search(append(append([]string{}, words...), extra...), plz, bbox, c); len(c2) > 0 && (!a2 || approx || len(cands) == 0) {
			cands, approx = c2, a2
		}
	}
	if altHnr != "" && (len(cands) == 0 || approx) { // "hauptstraße 1010": a house number after all
		if c2, a2 := ix.search(words, "", bbox, c); len(c2) > 0 && !a2 {
			cands, approx, plz, hnr = c2, a2, "", altHnr
		}
	}
	if len(cands) == 0 {
		return []addrHit{}, false, nil
	}
	var kx float64
	if c != nil {
		kx = math.Cos(c[1] * math.Pi / 180)
	}
	hitOf := func(cd *adxCand, i int) addrHit {
		d := &ix.docs[cd.doc]
		h := addrHit{Kind: "street", Label: d.name, Street: d.name, GKZ: d.gkz, Gemeinde: d.gem, Ort: d.ort, PLZ: d.plz, Ortschaft: d.osch,
			N: int(d.n), Lon: d.lon, Lat: d.lat, InView: cd.inView, Approx: approx, Full: cd.full && !approx, skz: d.skz, rank: float64(i)}
		if d.osm {
			h.Src = "osm"
		}
		return h
	}
	if hnr != "" {
		const maxStreets = 120
		bySkz := map[int64]*addrHit{}
		skzs := make([]interface{}, 0, maxStreets)
		streets := make([]addrHit, 0, maxStreets)
		for i := range cands {
			if ix.docs[cands[i].doc].osm {
				continue
			}
			streets = append(streets, hitOf(&cands[i], i))
			if len(streets) == maxStreets {
				break
			}
		}
		for i := range streets {
			bySkz[streets[i].skz] = &streets[i]
			skzs = append(skzs, streets[i].skz)
		}
		ph := strings.TrimSuffix(strings.Repeat("?,", len(skzs)), ",")
		scan := func(where string, extra interface{}, cap int, exact bool) error {
			if len(skzs) == 0 {
				return nil
			}
			args := append(append([]interface{}{}, skzs...), extra)
			ar, err := db.Query(`SELECT skz, hnr, hof, plz, lon, lat FROM adr WHERE skz IN (`+ph+`) AND `+where+` LIMIT `+strconv.Itoa(cap), args...)
			if err != nil {
				return err
			}
			defer ar.Close()
			for ar.Next() {
				var skz, lon, lat int64
				var hn string
				var hof sql.NullString
				var pz sql.NullInt64
				if err := ar.Scan(&skz, &hn, &hof, &pz, &lon, &lat); err != nil {
					return err
				}
				s := bySkz[skz]
				if s == nil || (!exact && strings.EqualFold(hn, hnr)) {
					continue
				}
				h := *s
				h.Kind, h.Hnr, h.Hof, h.N = "address", hn, hof.String, 0
				if pz.Valid && pz.Int64 > 0 {
					h.PLZ = strconv.FormatInt(pz.Int64, 10)
				}
				h.Lon, h.Lat = float64(lon)/1e6, float64(lat)/1e6
				h.Label = strings.TrimSpace(h.Street + " " + hn)
				if h.Hof != "" {
					h.Label += " · " + h.Hof
				}
				h.exact = exact
				if bbox != nil {
					h.InView = h.Lon >= bbox[0] && h.Lon <= bbox[2] && h.Lat >= bbox[1] && h.Lat <= bbox[3]
				}
				out = append(out, h)
			}
			return ar.Err()
		}
		if err := scan("hnr = ? COLLATE NOCASE", hnr, 300, true); err != nil {
			return nil, false, err
		}
		if len(out) < limit*3 {
			like := strings.NewReplacer("%", "", "_", "").Replace(hnr) + "%"
			if err := scan("hnr LIKE ?", like, 300, false); err != nil {
				return nil, false, err
			}
		}
		sort.SliceStable(out, func(i, j int) bool {
			a, b := &out[i], &out[j]
			if a.InView != b.InView {
				return a.InView
			}
			if a.exact != b.exact {
				return a.exact
			}
			if plz != "" && (a.PLZ == plz) != (b.PLZ == plz) {
				return a.PLZ == plz
			}
			if a.rank != b.rank {
				return a.rank < b.rank
			}
			return hnrLess(a.Hnr, b.Hnr)
		})
	}
	if len(out) == 0 { // no house number asked for, or the street has none like it: the streets
		n := limit
		if n > len(cands) {
			n = len(cands)
		}
		for i := 0; i < n; i++ {
			out = append(out, hitOf(&cands[i], i))
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	for i := range out {
		h := &out[i]
		if c != nil {
			h.DistKm = math.Round(math.Hypot((h.Lon-c[0])*kx, h.Lat-c[1])*111.32*10) / 10
		}
		sub := strings.TrimSpace(h.PLZ + " " + h.Ort)
		if h.Gemeinde != "" && !strings.EqualFold(h.Gemeinde, h.Ort) && !sameHead(h.Gemeinde, h.Ort) {
			sub += " · " + h.Gemeinde
		}
		if h.Kind == "street" && h.N > 0 {
			sub += fmt.Sprintf(" · %d addresses", h.N)
		}
		if h.Src == "osm" {
			sub = strings.TrimPrefix(sub+" · OpenStreetMap", " · ")
		}
		h.Sub = sub
	}
	return out, approx, nil
}

// handleV1AddressesSearch — GET /api/v1/addresses/search?q=&bbox=&c=&limit=
func handleV1AddressesSearch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) > 160 {
		q = q[:160]
	}
	limit := 8
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = v
		if limit > 20 {
			limit = 20
		}
	}
	var bbox *[4]float64
	if s := r.URL.Query().Get("bbox"); s != "" {
		p := strings.Split(s, ",")
		if len(p) == 4 {
			var b [4]float64
			ok := true
			for i := range p {
				v, err := strconv.ParseFloat(p[i], 64)
				if err != nil {
					ok = false
				}
				b[i] = v
			}
			if ok && b[0] < b[2] && b[1] < b[3] {
				bbox = &b
			}
		}
	}
	var c *[2]float64
	if s := r.URL.Query().Get("c"); s != "" {
		p := strings.Split(s, ",")
		if len(p) == 2 {
			lon, e1 := strconv.ParseFloat(p[0], 64)
			lat, e2 := strconv.ParseFloat(p[1], 64)
			if e1 == nil && e2 == nil {
				c = &[2]float64{lon, lat}
			}
		}
	} else if bbox != nil {
		c = &[2]float64{(bbox[0] + bbox[2]) / 2, (bbox[1] + bbox[3]) / 2}
	}
	resp := map[string]interface{}{
		"q": q, "results": []addrHit{},
		"license": "CC-BY-4.0", "license_url": BEVLicenseURL, "source": addressesLicURL,
	}
	db := addressesDB()
	if db == nil {
		resp["unavailable"] = true
		resp["attribution"] = addressesAttribution()
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, resp)
		return
	}
	resp["stichtag"] = addressesStichtag()
	resp["attribution"] = addressesAttribution()
	if len([]rune(q)) >= 2 {
		hits, approx, err := addressesSearch(db, q, bbox, c, limit)
		if err != nil {
			log.Printf("📮 addresses: %q: %v", q, err)
			apiError(w, "search failed", 500)
			return
		}
		if hits == nil {
			hits = []addrHit{}
		}
		resp["results"] = hits
		if approx {
			resp["approx"] = true
		}
		for i := range hits {
			if hits[i].Src == "osm" {
				resp["attribution_osm"] = OSMAttribution + " (" + OSMLicenseSPDX + ") — rows with src:\"osm\""
				break
			}
		}
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, 200, resp)
}

// sameHead — "Schönberg/Stubaital" vs "Schönberg im Stubaital": the postal
// spelling and the Gemeinde name of one place; showing both is noise.
func sameHead(a, b string) bool {
	f := func(s string) string {
		s = strings.ToLower(s)
		i := strings.IndexAny(s, " /-,.")
		if i > 0 {
			s = s[:i]
		}
		return s
	}
	return len(f(a)) >= 4 && f(a) == f(b)
}
