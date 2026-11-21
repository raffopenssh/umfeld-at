// tree_mail_parse.go — attachments of the trees-by-mail path (tree_mail.go):
// KML / KMZ / CSV → rows → tree reports. Pure: no network, no disk, no state.
//
// Every input is hostile until proven otherwise. KMZ: a zip with ≤ tmZipEntries
// entries, of which ONE .kml is read through a LimitReader (≤ tmMaxBytes, ≤
// tmZipRatio × its packed size) — no nested archive is opened, no entry name is
// used as a path. XML: token stream, any DTD/DOCTYPE refuses the file, no
// entity table (so nothing expands), depth ≤ tmXMLDepth, ≤ tmXMLElems elements,
// ≤ tmField bytes kept per text. CSV: ≤ tmCSVCols columns. All: ≤ tmMaxRows rows.
//
// What an edit is (docs/qgis-layer.md §Trees by email): the export labels a tree
// "18 m spruce" and carries id / h / species beside it. A row whose name still
// is that label and whose fields say nothing new is unchanged. Otherwise the
// name is read as free text — a height ("25 m"), a stem diameter ("40 cm"), a
// species (anything the peer's dictionary knows), a state since the flight
// ("gefällt 2025"), "kein baum" — and compared with the row's own fields.
package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	tmMaxBytes   = 16 << 20 // one attachment, unpacked
	tmMaxRows    = 20000
	tmZipEntries = 16
	tmZipRatio   = 100 // a real export packs ~35:1
	tmXMLDepth   = 16
	tmXMLElems   = 600000
	tmField      = 12288 // bytes kept of one text (a Locus description is ~700; a note may be long)
	tmCSVCols    = 32
	tmStr        = 64 // any string that leaves for the peer
)

// tmRow — one point of an attachment.
type tmRow struct {
	n        int // 1-based position in the file
	name     string
	lon, lat float64
	hasPt    bool
	attrs    map[string]string // id h species state dbh note date height crown_d (lower-case keys)
	extra    []string          // "key: value" of every field we do not read (tmExtra) — rides in the note
	free     []string          // lines typed into a description beside the table — a note
}

// tmAttrKeys — the field names we read, with their German / app aliases.
var tmAttrKeys = map[string]string{
	"id": "id", "h": "h", "species": "species", "art": "species", "baumart": "species",
	"state": "state", "zustand": "state", "status": "state", "dbh": "dbh", "bhd": "dbh", "dbh_cm": "dbh",
	"note": "note", "notiz": "note", "date": "date", "datum": "date", "height": "height", "hoehe": "height", "höhe": "height", "h_m": "height",
	"comment": "note", "kommentar": "note", "bemerkung": "note", "anmerkung": "note", "remark": "note", "crown_d": "crown_d", "frame": "frame",
}

// tmNoteMax — a note's length in characters when the peer names no limit;
// tmNoteLimit is what applies: the peer's limits.note_chars (treeNoteCap),
// never more than treeNoteCeil. A var so tests decide.
const tmNoteMax = 280

var tmNoteLimit = func() int {
	if n := treeNoteCap(); n > 0 {
		return min(n, treeNoteCeil)
	}
	return tmNoteMax
}

// tmClipRunes — s cut to n characters.
func tmClipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// tmIgnore — fields an export or a phone app writes by itself: never a comment.
var tmIgnore = map[string]bool{"name": true, "title": true, "lat": true, "latitude": true, "lon": true, "lng": true, "long": true, "longitude": true,
	"x": true, "y": true, "z": true, "wkt": true, "geometry": true, "desc": true, "description": true, "beschreibung": true,
	"vitality": true, "alt": true, "altitude": true, "ele": true, "elevation": true, "acc": true, "accuracy": true, "time": true, "timestamp": true,
	"attachments": true, "custom_coords": true, "fid": true, "ogc_fid": true, "gid": true, "objectid": true, "icon": true, "symbol": true,
	"visibility": true, "styleurl": true, "tessellate": true, "extrude": true, "altitudemode": true, "draworder": true, "snippet": true,
	"begin": true, "end": true, "layer": true, "path": true}

// tmExtra — a field we do not read, kept as "key: value" for the note.
func (r *tmRow) tmExtra(k, v string) {
	k, v = strings.TrimSpace(k), strings.Join(strings.Fields(html.UnescapeString(v)), " ")
	if k == "" || v == "" || tmIgnore[strings.ToLower(k)] || len(r.extra) >= 32 {
		return
	}
	r.extra = append(r.extra, tmClip(k, 24)+": "+tmClip(v, 600))
}

var (
	tmMailRe  = regexp.MustCompile(`[^\s@]+@[^\s@]+\.[^\s@]+`)
	tmPhoneRe = regexp.MustCompile(`\+?\d[\d /().-]{7,}\d`)
)

// tmPersonal — a value that looks like an e-mail address or a phone number
// (≥ 9 digits in one run of digits and separators; a date has 8): notes are
// public at the peer, so such a value is left out.
func tmPersonal(v string) bool {
	if tmMailRe.MatchString(v) {
		return true
	}
	for _, m := range tmPhoneRe.FindAllString(v, 4) {
		n := 0
		for _, c := range m {
			if c >= '0' && c <= '9' {
				n++
			}
		}
		if n >= 9 && !strings.Contains(m, ".") {
			return true
		}
	}
	return false
}

// tmNote — the note of a row: what the reader wrote (note field, lines typed
// into the description), then — with=true — every field we do not read.
// dropped = values left out as personal, cut = clipped at tmNoteMax.
func tmNote(r tmRow, with bool) (note string, dropped int, cut bool) {
	parts := []string{}
	add := func(v string) {
		if v = strings.Join(strings.Fields(v), " "); v == "" {
			return
		}
		if tmPersonal(v) {
			dropped++
			return
		}
		parts = append(parts, v)
	}
	add(r.attrs["note"])
	for _, f := range r.free {
		add(f)
	}
	if with {
		for _, x := range r.extra {
			add(x)
		}
	}
	note = strings.Join(parts, " · ")
	if lim := tmNoteLimit(); utf8.RuneCountInString(note) > lim {
		note, cut = tmClipRunes(note, lim-1)+"…", true
	}
	return
}

func tmClip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// tmParseAttachment — rows of one attachment by its file name.
func tmParseAttachment(name string, data []byte) ([]tmRow, error) {
	l := strings.ToLower(name)
	switch {
	case strings.HasSuffix(l, ".kmz"):
		kml, err := tmUnzipKML(data)
		if err != nil {
			return nil, err
		}
		return tmParseKML(kml)
	case strings.HasSuffix(l, ".kml"):
		return tmParseKML(data)
	case strings.HasSuffix(l, ".csv"):
		return tmParseCSV(data)
	}
	return nil, errors.New("not a .kml, .kmz or .csv file")
}

// tmUnzipKML — the one KML of a KMZ.
func tmUnzipKML(data []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, errors.New("not a readable KMZ (zip)")
	}
	if len(zr.File) > tmZipEntries {
		return nil, fmt.Errorf("KMZ holds %d files (≤ %d)", len(zr.File), tmZipEntries)
	}
	var pick *zip.File
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || !strings.HasSuffix(strings.ToLower(f.Name), ".kml") {
			continue // pictures, nested archives: never opened
		}
		if pick == nil || strings.EqualFold(f.Name, "doc.kml") {
			pick = f
		}
	}
	if pick == nil {
		return nil, errors.New("KMZ without a .kml inside")
	}
	if pick.UncompressedSize64 > tmMaxBytes {
		return nil, fmt.Errorf("KML inside the KMZ is too large (≤ %d MB unpacked)", tmMaxBytes>>20)
	}
	if pick.CompressedSize64 == 0 || pick.UncompressedSize64/pick.CompressedSize64 > tmZipRatio {
		return nil, errors.New("KMZ refused (compression ratio)")
	}
	rc, err := pick.Open()
	if err != nil {
		return nil, errors.New("not a readable KMZ (zip)")
	}
	defer rc.Close()
	// the header is a claim; the reader is the bound
	max := int64(tmMaxBytes)
	if r := int64(pick.CompressedSize64)*tmZipRatio + 1024; r < max {
		max = r
	}
	out, err := io.ReadAll(io.LimitReader(rc, max+1))
	if int64(len(out)) > max {
		return nil, errors.New("KMZ refused (unpacks larger than declared)")
	}
	if err != nil {
		return nil, errors.New("not a readable KMZ (zip)")
	}
	return out, nil
}

var (
	// a key cell and its value cell, whatever inline tags an app wraps them in
	// (Locus: <td><small><b>h</b></small></td><td>20</td>; others: <th>h</th><td><i>20</i></td>)
	tmHTMLRow = regexp.MustCompile(`(?is)<t[dh]\b[^>]{0,200}>\s*(?:<(?:b|i|u|em|strong|small|span|font)\b[^>]{0,200}>\s*){0,4}([\w.]{1,24})\s*:?\s*(?:</(?:b|i|u|em|strong|small|span|font)>\s*){0,4}</t[dh]>\s*<td\b[^>]{0,200}>\s*(?:<(?:b|i|u|em|strong|small|span|font)\b[^>]{0,200}>\s*){0,4}([^<]{0,200})(?:</(?:b|i|u|em|strong|small|span|font)>\s*){0,4}</td>`)
	tmTextRow = regexp.MustCompile(`(?m)^\s*([\p{L}_]{1,24})\s*[:=]\s*(.{1,200}?)\s*$`)
	tmTags    = regexp.MustCompile(`<[^>]{0,400}>`)
	tmIDRe    = regexp.MustCompile(`^t(-?\d{1,9})_(-?\d{1,9})$`)
)

// tmDescAttrs — fields out of a description: the HTML table Locus Map makes of
// a KML's ExtendedData, or plain "key: value" lines.
func tmDescAttrs(desc string, row *tmRow) {
	if desc == "" {
		return
	}
	into := row.attrs
	set := func(k, v string) bool {
		c, ok := tmAttrKeys[strings.ToLower(k)]
		if !ok {
			return false
		}
		if v = strings.TrimSpace(html.UnescapeString(v)); v != "" {
			if _, have := into[c]; !have {
				into[c] = tmClip(v, 200)
			}
		}
		return true
	}
	for _, m := range tmHTMLRow.FindAllStringSubmatch(desc, 32) {
		if !set(m[1], m[2]) {
			row.tmExtra(m[1], m[2])
		}
	}
	// what is left beside the table: "key: value" lines we read, and anything
	// else the reader typed there — a note
	rest := tmHTMLRow.ReplaceAllString(desc, " ")
	plain := tmTags.ReplaceAllString(strings.NewReplacer("<br>", "\n", "<br />", "\n", "<br/>", "\n", "</tr>", "\n", "</p>", "\n", "</div>", "\n").Replace(rest), " ")
	for _, ln := range strings.Split(plain, "\n") {
		if ln = strings.Join(strings.Fields(html.UnescapeString(ln)), " "); ln == "" {
			continue
		}
		if m := tmTextRow.FindStringSubmatch(ln); m != nil && (set(m[1], m[2]) || tmIgnore[strings.ToLower(m[1])]) {
			continue
		}
		if len(row.free) < 40 {
			row.free = append(row.free, tmClip(ln, tmField))
		}
	}
}

// tmParseKML — Placemarks with a Point. QGIS writes the fields as
// ExtendedData/SchemaData/SimpleData, Locus Map as an HTML table in
// <description>; both are read.
func tmParseKML(data []byte) ([]tmRow, error) {
	if len(data) > tmMaxBytes {
		return nil, fmt.Errorf("file too large (≤ %d MB)", tmMaxBytes>>20)
	}
	d := xml.NewDecoder(bytes.NewReader(data))
	d.Strict = true
	d.Entity = nil // only the five XML entities exist; anything else is an error
	var (
		rows   []tmRow
		stack  []string
		elems  int
		in     bool // inside a Placemark
		cur    tmRow
		desc   string
		coords string
		dname  string // SimpleData / Data name
		text   strings.Builder
	)
	for {
		tok, err := d.RawToken()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.New("not well-formed KML")
		}
		switch t := tok.(type) {
		case xml.Directive:
			return nil, errors.New("KML with a DOCTYPE/DTD is refused")
		case xml.StartElement:
			if elems++; elems > tmXMLElems {
				return nil, errors.New("KML has too many elements")
			}
			if len(stack) >= tmXMLDepth {
				return nil, errors.New("KML nested too deep")
			}
			stack = append(stack, t.Name.Local)
			text.Reset()
			switch t.Name.Local {
			case "Placemark":
				if in {
					return nil, errors.New("not well-formed KML")
				}
				in, cur, desc, coords = true, tmRow{attrs: map[string]string{}}, "", ""
			case "SimpleData", "Data":
				dname = ""
				for _, a := range t.Attr {
					if a.Name.Local == "name" {
						dname = tmClip(a.Value, 24)
					}
				}
			}
		case xml.CharData:
			if in && text.Len() < tmField {
				text.Write(t[:min(len(t), tmField-text.Len())])
			}
		case xml.EndElement:
			if len(stack) == 0 || stack[len(stack)-1] != t.Name.Local {
				return nil, errors.New("not well-formed KML")
			}
			stack = stack[:len(stack)-1]
			parent := ""
			if len(stack) > 0 {
				parent = stack[len(stack)-1]
			}
			if !in {
				continue
			}
			v := strings.TrimSpace(text.String())
			text.Reset()
			switch t.Name.Local {
			case "name":
				if parent == "Placemark" {
					cur.name = v
				}
			case "description":
				if parent == "Placemark" {
					desc = v
				}
			case "coordinates":
				if parent == "Point" {
					coords = v
				}
			case "SimpleData", "value":
				if c, ok := tmAttrKeys[strings.ToLower(dname)]; ok {
					if v != "" {
						cur.attrs[c] = tmClip(v, 200)
					}
				} else {
					cur.tmExtra(dname, v)
				}
			case "Placemark":
				in = false
				if coords == "" {
					continue // a track, an area: not a tree
				}
				if len(rows) >= tmMaxRows {
					return nil, fmt.Errorf("more than %d points in one file — send a smaller area", tmMaxRows)
				}
				if p := strings.Split(coords, ","); len(p) >= 2 {
					lon, e1 := strconv.ParseFloat(strings.TrimSpace(p[0]), 64)
					lat, e2 := strconv.ParseFloat(strings.TrimSpace(p[1]), 64)
					if e1 == nil && e2 == nil {
						cur.lon, cur.lat, cur.hasPt = lon, lat, true
					}
				}
				tmDescAttrs(desc, &cur)
				cur.n = len(rows) + 1
				rows = append(rows, cur)
			}
		}
	}
	if len(stack) != 0 {
		return nil, errors.New("not well-formed KML")
	}
	return rows, nil
}

var tmWKT = regexp.MustCompile(`(?i)^\s*POINT\s*Z?\s*\(\s*(-?[\d.]+)\s+(-?[\d.]+)`)

// tmParseCSV — a header line, then one point per line. Locus Map: name, lat,
// lon, alt, acc, time, desc, …; QGIS / a spreadsheet: X, Y or WKT plus the
// fields as columns.
func tmParseCSV(data []byte) ([]tmRow, error) {
	if len(data) > tmMaxBytes {
		return nil, fmt.Errorf("file too large (≤ %d MB)", tmMaxBytes>>20)
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(data) {
		return nil, errors.New("CSV must be UTF-8")
	}
	first := data
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		first = data[:i]
	}
	delim, best := ',', bytes.Count(first, []byte(","))
	for _, c := range []rune{';', '\t'} {
		if n := bytes.Count(first, []byte(string(c))); n > best {
			delim, best = c, n
		}
	}
	r := csv.NewReader(bytes.NewReader(data))
	r.Comma, r.LazyQuotes, r.FieldsPerRecord, r.ReuseRecord = delim, true, -1, true
	head, err := r.Read()
	if err != nil {
		return nil, errors.New("CSV without a header line")
	}
	if len(head) > tmCSVCols {
		return nil, fmt.Errorf("CSV has %d columns (≤ %d)", len(head), tmCSVCols)
	}
	col := map[string]int{}
	attrCol := map[int]string{}
	extraCol := map[int]string{}
	for i, h := range head {
		h = strings.ToLower(strings.TrimSpace(h))
		switch h {
		case "name", "title":
			h = "name"
		case "lat", "latitude", "y":
			h = "lat"
		case "lon", "lng", "long", "longitude", "x":
			h = "lon"
		case "desc", "description", "beschreibung":
			h = "desc"
		case "wkt", "geometry":
			h = "wkt"
		default:
			if c, ok := tmAttrKeys[h]; ok {
				attrCol[i] = c
			} else if h != "" && !tmIgnore[h] {
				extraCol[i] = tmClip(strings.TrimSpace(head[i]), 24)
			}
			continue
		}
		if _, dup := col[h]; !dup {
			col[h] = i
		}
	}
	_, hasLL := col["lat"]
	_, hasWKT := col["wkt"]
	_, hasID := tmFind(attrCol, "id")
	if !(hasLL && col["lon"] >= 0) && !hasWKT && !hasID {
		return nil, errors.New("CSV needs lat + lon columns (or WKT, or id)")
	}
	get := func(rec []string, k string) string {
		if i, ok := col[k]; ok && i < len(rec) {
			return tmClip(strings.TrimSpace(rec[i]), tmField)
		}
		return ""
	}
	num := func(s string) (float64, bool) {
		if delim != ',' {
			s = strings.Replace(s, ",", ".", 1)
		}
		v, err := strconv.ParseFloat(s, 64)
		return v, err == nil && s != ""
	}
	var rows []tmRow
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("CSV not readable near line %d", len(rows)+2)
		}
		if len(rec) > tmCSVCols {
			return nil, fmt.Errorf("CSV has a line with %d columns (≤ %d)", len(rec), tmCSVCols)
		}
		if len(rec) == 1 && strings.TrimSpace(rec[0]) == "" {
			continue
		}
		if len(rows) >= tmMaxRows {
			return nil, fmt.Errorf("more than %d points in one file — send a smaller area", tmMaxRows)
		}
		row := tmRow{n: len(rows) + 1, name: get(rec, "name"), attrs: map[string]string{}}
		for i, c := range attrCol {
			if i < len(rec) {
				if v := strings.TrimSpace(rec[i]); v != "" {
					row.attrs[c] = tmClip(v, 200)
				}
			}
		}
		for i := range rec {
			if k, ok := extraCol[i]; ok {
				row.tmExtra(k, rec[i])
			}
		}
		lat, ok1 := num(get(rec, "lat"))
		lon, ok2 := num(get(rec, "lon"))
		if ok1 && ok2 {
			row.lon, row.lat, row.hasPt = lon, lat, true
		} else if m := tmWKT.FindStringSubmatch(get(rec, "wkt")); m != nil {
			row.lon, _ = strconv.ParseFloat(m[1], 64)
			row.lat, _ = strconv.ParseFloat(m[2], 64)
			row.hasPt = true
		}
		tmDescAttrs(get(rec, "desc"), &row)
		rows = append(rows, row)
	}
	return rows, nil
}

func tmFind(m map[int]string, v string) (int, bool) {
	for i, c := range m {
		if c == v {
			return i, true
		}
	}
	return 0, false
}

// ── the name as free text ──

// tmClasses — the model classes an export names a species by (the peer's
// dictionary `classes`): a species word from this list is what we wrote, not
// what somebody typed.
var tmClasses = map[string]bool{"alder": true, "ash": true, "beech": true, "birch": true, "broadleaf": true, "conifer": true, "fir": true, "fruit": true,
	"larch": true, "maple": true, "oak": true, "pine": true, "poplar": true, "spruce": true, "willow": true}

// tmStates — words for what happened to a tree since the flight → the peer's
// state values. Keys are folded (tmFold).
var tmStates = map[string]string{
	"gefaellt": "logged", "gefallt": "logged", "felled": "logged", "logged": "logged", "geschlaegert": "logged", "geschlagert": "logged", "umgeschnitten": "logged", "cut": "logged", "geerntet": "logged", "stock": "logged", "stumpf": "logged",
	"windwurf": "windthrow", "windthrow": "windthrow", "umgefallen": "windthrow", "sturmschaden": "windthrow", "geworfen": "windthrow",
	"verbrannt": "burnt", "burnt": "burnt", "burned": "burnt",
	"tot": "dead", "dead": "dead", "abgestorben": "dead", "duerr": "dead", "durr": "dead", "totholz": "dead",
	"gebrochen": "broken", "broken": "broken", "bruch": "broken", "wipfelbruch": "broken",
	"kaefer": "infested", "kafer": "infested", "borkenkaefer": "infested", "borkenkafer": "infested", "infested": "infested", "befall": "infested", "befallen": "infested", "krank": "infested",
	"weg": "gone", "gone": "gone", "fehlt": "gone", "verschwunden": "gone",
	"steht": "standing", "standing": "standing",
}

// tmPhrases — two/three-word sayings, folded, replaced by one token first.
var tmPhrases = strings.NewReplacer(
	"kein baum", "\x01spurious", "not a tree", "\x01spurious", "no tree", "\x01spurious", "nicht baum", "\x01spurious", "keinbaum", "\x01spurious",
	"same tree", "\x01crown", "gleicher baum", "\x01crown", "selber baum", "\x01crown", "derselbe baum", "\x01crown", "ein baum", "\x01crown", "one tree", "\x01crown",
	"looks right", "\x01looks_right", "still standing", "standing", "steht noch", "standing",
)

var tmFolder = strings.NewReplacer("ä", "ae", "ö", "oe", "ü", "ue", "ß", "ss", "Ä", "ae", "Ö", "oe", "Ü", "ue")

func tmFold(s string) string { return strings.ToLower(tmFolder.Replace(s)) }

var (
	tmNumRe  = regexp.MustCompile(`^(\d{1,4}(?:[.,]\d{1,2})?)(m|cm|meter|metres|meters)?$`)
	tmDateRe = regexp.MustCompile(`^(?:(\d{4})-(\d{1,2})(?:-(\d{1,2}))?|(?:(\d{1,2})\.)?(\d{1,2})\.(\d{4}))$`)
)

// tmSaid — what a name (or one field) says.
type tmSaid struct {
	h, dbh  float64 // 0 = not said
	species string
	state   string
	date    string
	flag    string // spurious | looks_right | crown (same tree: a second top of the tree beside it)
	bad     string // why the text cannot be used
}

// tmDate — "2025" · "8.2025" · "14.8.2025" · "2025-08[-14]" → YYYY[-MM[-DD]];
// 1990 … today, as precise as written.
func tmDate(s string, now time.Time) (string, bool) {
	var y, m, d int
	if v, err := strconv.Atoi(s); err == nil && len(s) == 4 {
		y = v
	} else if g := tmDateRe.FindStringSubmatch(s); g != nil {
		if g[1] != "" {
			y, _ = strconv.Atoi(g[1])
			m, _ = strconv.Atoi(g[2])
			d, _ = strconv.Atoi(g[3])
		} else {
			d, _ = strconv.Atoi(g[4])
			m, _ = strconv.Atoi(g[5])
			y, _ = strconv.Atoi(g[6])
		}
		if m < 1 || m > 12 || d > 31 {
			return "", false
		}
	} else {
		return "", false
	}
	if y < 1990 || y > now.Year() {
		return "", false
	}
	out := fmt.Sprintf("%04d", y)
	if m > 0 {
		out += fmt.Sprintf("-%02d", m)
		if d > 0 {
			out += fmt.Sprintf("-%02d", d)
		}
	}
	if out > now.Format("2006-01-02")[:len(out)] {
		return "", false
	}
	return out, true
}

// tmReadName — a height, a diameter, a state (+ date), a flag; the rest is the species.
func tmReadName(name string, now time.Time) tmSaid {
	var out tmSaid
	name = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 || r == ';' {
			return ' '
		}
		return r
	}, name)
	raw := strings.Fields(name)
	if len(raw) > 12 {
		out.bad = "name too long to read"
		return out
	}
	// phrases on the folded text, token for token aligned with raw where possible
	folded := strings.Fields(tmPhrases.Replace(tmFold(strings.Join(raw, " "))))
	aligned := len(folded) == len(raw)
	var rest []string
	next := "" // what a unit-less number after "bhd" / "h" means
	for i := 0; i < len(folded); i++ {
		t := strings.Trim(folded[i], ".:()")
		orig := t
		if aligned {
			orig = strings.Trim(raw[i], ":()")
		}
		switch {
		case t == "":
		case t == "\x01spurious" || t == "\x01looks_right":
			out.flag = t[1:]
		case t == "\x01crown" || t == "zwiesel" || t == "doppelt" || t == "doppelwipfel" || t == "duplicate":
			out.flag = "crown"
		case t == "ok" || t == "passt" || t == "stimmt":
			out.flag = "looks_right"
		case t == "bhd" || t == "dbh" || t == "ø" || t == "durchmesser":
			next = "cm"
		case t == "h" || t == "hoehe" || t == "height":
			next = "m"
		case t == "m" || t == "cm" || t == "baum" || t == "tree" || t == "neu" || t == "new" || t == "am" || t == "im" || t == "seit" || t == "since":
			// filler
		case tmStates[t] != "":
			out.state = tmStates[t]
		default:
			if g := tmNumRe.FindStringSubmatch(t); g != nil {
				v, _ := strconv.ParseFloat(strings.Replace(g[1], ",", ".", 1), 64)
				unit := g[2]
				if unit == "" && i+1 < len(folded) {
					switch strings.Trim(folded[i+1], ".") {
					case "m", "meter", "metres", "meters":
						unit = "m"
						i++
					case "cm":
						unit = "cm"
						i++
					}
				}
				if unit == "" {
					if d, ok := tmDate(t, now); ok && next == "" {
						out.date = d
						continue
					}
					if unit = next; unit == "" {
						unit = "m" // a bare number on a tree is its height
					}
				}
				next = ""
				if unit == "cm" {
					out.dbh = v
				} else {
					out.h = v
				}
				continue
			}
			if d, ok := tmDate(t, now); ok {
				out.date = d
				continue
			}
			rest = append(rest, orig)
		}
	}
	out.species = strings.Join(rest, " ")
	if out.h != 0 && (out.h < 1 || out.h > 70) {
		out.bad = "height must be 1–70 m"
	}
	if out.dbh != 0 && (out.dbh < 1 || out.dbh > 600) {
		out.bad = "stem diameter must be 1–600 cm"
	}
	if len(out.species) > 60 {
		out.bad = "species name too long (≤ 60 characters)"
	}
	return out
}

var tmCanonRe = regexp.MustCompile(`^(\d{1,2}) m(?: ([a-z]+))?$`)

// tmLabel — the label the export gives a tree (neOGCTreeFeature).
func tmLabel(h float64, species string) string {
	s := strconv.FormatFloat(math.Round(h), 'f', 0, 64) + " m"
	if species != "" {
		s += " " + species
	}
	return s
}

// tmItem — one report for the peer and how the receipt names it.
type tmItem struct {
	row  int
	name string // the row's name, clipped — for the receipt
	what string // "species Fichte · state logged 2025"
	body map[string]interface{}
	add  bool // no id: a tree report on the point first, an added tree if nothing is there

	crown   bool    // "same tree": the tree it belongs to is found among the file's rows (tmCrowns)
	h       float64 // the export's height of this row, 0 unknown
	dropped int     // values left out of the note as personal
	apex    bool    // a tree of the export (it has an id): its position lives in a position frame (tmFrames)
	h0      float64 // the export's LiDAR height of that tree, 0 unknown — the check that a position is today's
	cut     bool    // note clipped
}

// tmNoteAlone — may a note be a report of its own? Yes since the peer stores
// notes (its #tree-note contract, 2026-11-20); a var so a deployment against
// an older peer, and tests, can say no.
var tmNoteAlone = func() bool { return true }

// tmPos — where a row's tree stands: the id is the apex (a point dragged in
// an app does not move the tree), else the point.
func tmPos(r tmRow) (lon, lat float64, ok bool, bad string) {
	lon, lat, ok = r.lon, r.lat, r.hasPt
	if id := r.attrs["id"]; id != "" {
		m := tmIDRe.FindStringSubmatch(id)
		if m == nil {
			return 0, 0, false, "id is not a tree id"
		}
		a, _ := strconv.ParseFloat(m[1], 64)
		b, _ := strconv.ParseFloat(m[2], 64)
		lon, lat, ok = a/1e6, b/1e6, true
	}
	return
}

func tmNum(s string) float64 {
	v, _ := strconv.ParseFloat(strings.Replace(strings.TrimSpace(s), ",", ".", 1), 64)
	return v
}

// tmClientID — the idempotency key of a tree added by mail: the same reporter
// adding at the same spot is the same tree, and a later undo finds it. Nothing
// is stored for that.
func tmClientID(reporter string, lon, lat float64) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("tree-mail|%s|%.6f|%.6f", reporter, lon, lat)))
	x := hex.EncodeToString(h[:16])
	return x[:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:32]
}

// tmDiff — what one row reports. items empty and why empty = unchanged;
// why set = the row is refused.
func tmDiff(r tmRow, reporter string, now time.Time) (items []tmItem, why string) {
	id := r.attrs["id"]
	lon, lat, has, bad := tmPos(r)
	if bad != "" {
		return nil, bad
	}
	if !has {
		return nil, "no position"
	}
	if lon < neMVTAustria[0] || lon > neMVTAustria[2] || lat < neMVTAustria[1] || lat > neMVTAustria[3] {
		return nil, "outside Austria"
	}
	baseH, hasH := 0.0, false
	if v, err := strconv.ParseFloat(strings.Replace(r.attrs["h"], ",", ".", 1), 64); err == nil && v > 0 && v < 100 {
		baseH, hasH = v, true
	}
	baseSp := r.attrs["species"]
	name := strings.Join(strings.Fields(r.name), " ")
	var said tmSaid
	untouched := name == "" || (hasH && strings.EqualFold(name, tmLabel(baseH, baseSp)))
	if !untouched {
		if said = tmReadName(name, now); said.bad != "" {
			return nil, said.bad
		}
	}
	// species: what the name says — unless the name only repeats the export's
	// class word while the field beside it was typed over (a table edit)
	sp := ""
	ns, baseClass := said.species, tmClasses[strings.ToLower(baseSp)]
	switch {
	case ns != "" && !strings.EqualFold(ns, baseSp):
		if sp = ns; tmClasses[strings.ToLower(ns)] && baseSp != "" && !baseClass {
			sp = baseSp
		}
	case ns == "" && baseSp != "" && !baseClass:
		sp = baseSp
	}
	// height: the name's, when it is not the export's own rounding
	h := 0.0
	if said.h != 0 {
		switch {
		case !hasH:
			h = said.h
		case said.h == math.Trunc(said.h) && math.Round(baseH) == said.h:
		case math.Abs(said.h-baseH) < 0.25:
		default:
			h = said.h
		}
	}
	if v := r.attrs["height"]; v != "" && h == 0 {
		f, err := strconv.ParseFloat(strings.Replace(strings.TrimSuffix(strings.TrimSpace(strings.TrimSuffix(v, "m")), " "), ",", ".", 1), 64)
		if err != nil || f < 1 || f > 70 {
			return nil, "height must be 1–70 m"
		}
		h = f
	}
	dbh := said.dbh
	if v := r.attrs["dbh"]; v != "" && dbh == 0 {
		f, err := strconv.ParseFloat(strings.Replace(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "cm")), ",", ".", 1), 64)
		if err != nil || f < 1 || f > 600 {
			return nil, "stem diameter must be 1–600 cm"
		}
		dbh = f
	}
	state, date, flag := said.state, said.date, said.flag
	if v := r.attrs["state"]; v != "" && state == "" && flag == "" {
		s := tmReadName(v, now)
		if s.state == "" && s.flag == "" {
			return nil, "state not understood: " + tmClip(v, 24)
		}
		state, flag = s.state, s.flag
		if date == "" {
			date = s.date
		}
	}
	if v := r.attrs["date"]; v != "" && date == "" {
		d, ok := tmDate(strings.TrimSpace(v), now)
		if !ok {
			return nil, "date not understood (2025 · 8.2025 · 14.8.2025, not in the future)"
		}
		date = d
	}
	if len(sp) > 60 {
		return nil, "species name too long (≤ 60 characters)"
	}
	if date != "" && state == "" {
		date = "" // a date belongs to a state
	}

	frame := "" // the position frame the export wrote on the row (tree_frames.go); a drawn point has none
	if id != "" {
		frame = treeFrameToken(strings.TrimSpace(r.attrs["frame"]))
	}
	pos := func(m map[string]interface{}) map[string]interface{} {
		m["lon"], m["lat"] = math.Round(lon*1e7)/1e7, math.Round(lat*1e7)/1e7
		if frame != "" {
			m["frame"] = frame
		}
		m["layer"] = "trees"
		m["reporter"] = reporter
		return m
	}
	show := tmClip(name, 40)
	var what []string
	b := map[string]interface{}{}
	if sp != "" {
		b["species"] = sp
		what = append(what, "species "+sp)
	}
	if h != 0 {
		b["h_m"], b["h_method"] = h, "estimate"
		what = append(what, "height "+strconv.FormatFloat(h, 'f', -1, 64)+" m")
	}
	if dbh != 0 {
		b["dbh_cm"] = dbh
		what = append(what, "diameter "+strconv.FormatFloat(dbh, 'f', -1, 64)+" cm")
	}
	if state != "" {
		b["state"] = state
		w := "state " + state
		if date != "" {
			b["date"] = date
			w += " " + date
		}
		what = append(what, w)
	}
	// the note: what the reader wrote is a report of its own (a tree of the
	// export, once the peer stores notes); the fields we do not read only ride
	// along with something said
	own, _, _ := tmNote(r, false)
	if len(b) == 0 && own != "" && flag == "" && id != "" && !tmNoteAlone() {
		return nil, "a note alone cannot be stored yet — it travels with a species, a height or a state"
	}
	if len(b) > 0 || (own != "" && id != "") {
		note, dropped, cut := tmNote(r, true)
		if note != "" {
			b["note"] = note
			what = append(what, "note “"+tmClip(note, 40)+"”")
		}
		if len(b) > 0 {
			b["claim"] = "tree"
			items = append(items, tmItem{row: r.n, name: show, what: strings.Join(what, " · "), body: pos(b), add: id == "", dropped: dropped, cut: cut, apex: id != "", h0: baseH})
		}
	}
	if flag != "" {
		w := map[string]string{"spurious": "not a tree", "looks_right": "looks right", "crown": "same tree"}[flag]
		if id == "" {
			return nil, "“" + w + "” needs a tree of the export (a point with its id)"
		}
		claim := flag
		if flag == "crown" {
			claim = "spurious"
		}
		f := pos(map[string]interface{}{"claim": claim})
		if hasH {
			f["observed"] = map[string]interface{}{"h_m": baseH}
		}
		items = append(items, tmItem{row: r.n, name: show, what: w, body: f, crown: flag == "crown", h: baseH, apex: true, h0: baseH})
	}
	if len(items) == 0 && id == "" {
		return nil, "not a tree of the export and nothing to add (name a species or a height)"
	}
	return items, ""
}

// tmUndo — the item that takes a report back (the same file, subject "undo").
func tmUndo(it tmItem) tmItem {
	b := map[string]interface{}{"lon": it.body["lon"], "lat": it.body["lat"], "layer": "trees", "reporter": it.body["reporter"], "claim": it.body["claim"]}
	if it.body["claim"] == "tree" {
		b["retract"] = "last"
	} else {
		b["retract"] = true
	}
	return tmItem{row: it.row, name: it.name, what: "take back: " + it.what, body: b, add: it.add}
}

// ── same tree ──

const (
	tmCrownMax  = 25.0 // metres: the peer refuses a farther "of"
	tmCrownNone = "same tree: no tree beside it in the file (send the whole folder)"
)

// tmReach — how far a tree's crown goes: half the export's crown_d, else a
// fifth of its height, at least 2 m.
func tmReach(r tmRow) float64 {
	if d := tmNum(r.attrs["crown_d"]); d > 0 && d < 60 {
		return math.Max(2, d/2)
	}
	return math.Max(2, tmNum(r.attrs["h"])*0.2)
}

// tmDiffRows — every row of a file through tmDiff, then the rows named "same
// tree" bound to the tree they belong to: the tallest tree of the file whose
// crown touches theirs and that is not named so itself. ONE rule, the map's
// too: the taller top stays — when the reader named the taller one, the flag
// goes to the lower and the receipt says so.
func tmDiffRows(rows []tmRow, reporter string, now time.Time) (items []tmItem, same int, refused map[string][]int) {
	refused = map[string][]int{}
	marked := map[int]bool{}
	for _, r := range rows {
		its, why := tmDiff(r, reporter, now)
		switch {
		case why != "":
			refused[why] = append(refused[why], r.n)
		case len(its) == 0:
			same++
		default:
			for _, it := range its {
				if it.crown {
					marked[r.n] = true
				}
			}
			items = append(items, its...)
		}
	}
	if len(marked) == 0 {
		return
	}
	type pt struct {
		lon, lat, h, reach float64
		n                  int
	}
	const cell = 0.0004 // ≥ 30 m in both axes
	key := func(lon, lat float64) [2]int32 {
		return [2]int32{int32(math.Floor(lon / cell)), int32(math.Floor(lat / cell))}
	}
	grid := map[[2]int32][]pt{}
	at := map[int]pt{}
	for _, r := range rows {
		lon, lat, ok, bad := tmPos(r)
		if !ok || bad != "" || r.attrs["id"] == "" {
			continue
		}
		p := pt{lon, lat, tmNum(r.attrs["h"]), tmReach(r), r.n}
		at[r.n] = p
		if !marked[r.n] {
			grid[key(lon, lat)] = append(grid[key(lon, lat)], p)
		}
	}
	flagged := map[[2]float64]bool{}
	out := items[:0]
	for _, it := range items {
		if !it.crown {
			out = append(out, it)
			continue
		}
		m := at[it.row]
		k, best, bd := key(m.lon, m.lat), pt{}, 0.0
		cl := math.Cos(m.lat*math.Pi/180) * 111320
		for dx := int32(-1); dx <= 1; dx++ {
			for dy := int32(-1); dy <= 1; dy++ {
				for _, c := range grid[[2]int32{k[0] + dx, k[1] + dy}] {
					d := math.Hypot((c.lon-m.lon)*cl, (c.lat-m.lat)*111320)
					if d > tmCrownMax || d > (m.reach+c.reach)*1.25+1 {
						continue
					}
					if best.n == 0 || c.h > best.h || (c.h == best.h && d < bd) {
						best, bd = c, d
					}
				}
			}
		}
		if best.n == 0 {
			refused[tmCrownNone] = append(refused[tmCrownNone], it.row)
			continue
		}
		lo, hi := m, best // the flag sits on the lower top
		it.what = fmt.Sprintf("same tree as the %s top %.0f m away", strconv.FormatFloat(hi.h, 'f', -1, 64)+" m", bd)
		if best.h < m.h {
			lo, hi = best, m
			it.what = fmt.Sprintf("same tree — the %s top beside it joins this one (the taller top stays)", strconv.FormatFloat(lo.h, 'f', -1, 64)+" m")
		}
		sub := [2]float64{lo.lon, lo.lat}
		if flagged[sub] {
			continue // two taller tops named for one lower top: one flag
		}
		flagged[sub] = true
		it.body["lon"], it.body["lat"] = math.Round(lo.lon*1e7)/1e7, math.Round(lo.lat*1e7)/1e7
		if lo.h > 0 {
			it.body["observed"] = map[string]interface{}{"h_m": lo.h}
		}
		it.body["expected"] = map[string]interface{}{"absent": true, "hint": "crown",
			"of": map[string]interface{}{"lon": math.Round(hi.lon*1e7) / 1e7, "lat": math.Round(hi.lat*1e7) / 1e7}}
		out = append(out, it)
	}
	items = out
	return
}
