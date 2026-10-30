// ne_glyph_admin.go — outline collections for the landing glyph's find box:
// Bundesland · Bezirk · Gemeinde · Natura 2000 site. A name index and, per
// unit, an outline in viewBox units so the client can give any such unit the
// same treatment as a KG (filled selection, outer stroke, full-res veil mask).
//
//	GET /api/v1/ne/glyph/admin                   {"bl":[{c,n,bbox}],"bz":[{c,n,bl,bbox}],"gem":[{c,n,bz,bbox}],"n2k":[{c,n,t,ha,bbox}],"kinds":{key:{kind,notice}}}  (1 h)
//	GET /api/v1/ne/glyph/admin/outline?c=701     {c,kind,n,d,bbox,notice}  — d = SVG path (M…Z per ring, evenodd)
//	GET /api/v1/ne/glyph/admin/outline?c=AT1103112
//
// Geometry: data/ne_glyph_admin.json.gz, one key per collection, built once
// offline with shapely (scripts/ne_glyph_admin_build.py); committed (≈1.5 MB gz).
//
//	bl/bz/gem — Statistik Austria Gemeinde polygons (CC BY 4.0,
//	            data/gemeinden_v2.geojson.gz) dissolved to Bezirke (GKZ[0:3];
//	            Wien's 23 Gemeindebezirke are 901–923) and Bundesländer (GKZ[0]).
//	            Names: Gemeinde travels with the polygon, Bezirk = siDistrictNames,
//	            Bundesland below. Codes: 1 digit / 3 / 5.
//	n2k       — Natura 2000 sites (EEA, data/natura2000_at.geojson.gz), code =
//	            SITECODE (AT + 7), t = A birds · B habitats · C both, ha = area.
//
// Adding a collection = one key in the build script + one row in
// neGlyphAdminKinds (code pattern, kind label, notice, index extras). The
// client (ne_glyph.go admGet/admSearch/selectAdm) is generic over the index:
// every row needs c, n, bbox; everything else is display. docs/glyph-find-contract.md.
//
// Memory: the decoded rings (~10 MB) are kept for the process lifetime once a
// request touched them; projected path strings are memoised per code in a
// bounded map (neGlyphAdminLRU, ≤ 256 entries) — both on /debug/inflight.
package main

import (
	"compress/gzip"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

var neGlyphBundeslandNames = map[string]string{
	"1": "Burgenland", "2": "Kärnten", "3": "Niederösterreich", "4": "Oberösterreich", "5": "Salzburg",
	"6": "Steiermark", "7": "Tirol", "8": "Vorarlberg", "9": "Wien",
}

// Wien's Gemeindebezirke (GKZ 9xx01 → Bezirk 9xx) — not in siDistrictNames.
var neGlyphWienBezirke = []string{"", "Innere Stadt", "Leopoldstadt", "Landstraße", "Wieden", "Margareten", "Mariahilf", "Neubau", "Josefstadt", "Alsergrund", "Favoriten", "Simmering", "Meidling", "Hietzing", "Penzing", "Rudolfsheim-Fünfhaus", "Ottakring", "Hernals", "Währing", "Döbling", "Brigittenau", "Floridsdorf", "Donaustadt", "Liesing"}

func neGlyphBezirkName(c string) string {
	if len(c) == 3 && c[0] == '9' {
		if n, err := strconv.Atoi(c[1:]); err == nil && n >= 1 && n < len(neGlyphWienBezirke) {
			return "Wien " + strconv.Itoa(n) + "., " + neGlyphWienBezirke[n]
		}
		return "Wien"
	}
	if n := siDistrictNames[c]; n != "" {
		return n
	}
	return "Bezirk " + c
}

type neGlyphAdminUnit struct {
	C    string         `json:"c"`
	N    string         `json:"n,omitempty"`
	BL   string         `json:"bl,omitempty"`
	BZ   string         `json:"bz,omitempty"`
	T    string         `json:"t,omitempty"`  // n2k: site type A/B/C
	Ha   int            `json:"ha,omitempty"` // n2k: area in ha
	BBox [4]float64     `json:"bbox"`
	R    [][][2]float64 `json:"r"`
	kind *neGlyphAdminKind
}

// neGlyphAdminKind — one outline collection. Order = order in the /admin index.
type neGlyphAdminKind struct {
	Key    string         // key in the file and the index
	Kind   string         // label in outline answers and the client's tag
	Code   *regexp.Regexp // what c= looks like
	Notice string
	Name   func(u *neGlyphAdminUnit) string // nil = the file's n
}

var neGlyphAdminKinds = []neGlyphAdminKind{
	{Key: "bl", Kind: "bundesland", Code: regexp.MustCompile(`^\d$`), Notice: "© Statistik Austria, CC BY 4.0", Name: func(u *neGlyphAdminUnit) string { return neGlyphBundeslandNames[u.C] }},
	{Key: "bz", Kind: "bezirk", Code: regexp.MustCompile(`^\d{3}$`), Notice: "© Statistik Austria, CC BY 4.0", Name: func(u *neGlyphAdminUnit) string { return neGlyphBezirkName(u.C) }},
	{Key: "gem", Kind: "gemeinde", Code: regexp.MustCompile(`^\d{5}$`), Notice: "© Statistik Austria, CC BY 4.0"},
	{Key: "n2k", Kind: "natura2000", Code: regexp.MustCompile(`^AT[0-9A-Z]{7}$`), Notice: "Source: European Environment Agency (EEA), Natura 2000 data"},
}

type neGlyphAdminFile map[string][]neGlyphAdminUnit

var neGlyphAdmin struct {
	once  sync.Once
	err   error
	f     neGlyphAdminFile
	by    map[string]*neGlyphAdminUnit
	index []byte // the /admin JSON
	mu    sync.Mutex
	paths map[string][]byte // code → outline JSON (bounded)
	bytes int
}

const neGlyphAdminLRUMax = 256

func neGlyphAdminLoad() error {
	a := &neGlyphAdmin
	a.once.Do(func() {
		if a.err = neGlyphLoadFrame(); a.err != nil {
			return
		}
		fh, err := os.Open(filepath.Join("data", "ne_glyph_admin.json.gz"))
		if err != nil {
			a.err = err
			return
		}
		defer fh.Close()
		zr, err := gzip.NewReader(fh)
		if err != nil {
			a.err = err
			return
		}
		f := neGlyphAdminFile{}
		if a.err = json.NewDecoder(zr).Decode(&f); a.err != nil {
			return
		}
		a.f = f
		a.by = map[string]*neGlyphAdminUnit{}
		a.paths = map[string][]byte{}
		fr := &neGlyphFrame
		px := func(lon, lat float64) (float64, float64) {
			return (lon*fr.kx-fr.minx)*fr.scale + neGlyphPad, (-lat-fr.miny)*fr.scale + neGlyphPad
		}
		fbox := func(b [4]float64) []float64 {
			x0, y0 := px(b[0], b[3])
			x1, y1 := px(b[2], b[1])
			return []float64{math.Round(x0*10) / 10, math.Round(y0*10) / 10, math.Round(x1*10) / 10, math.Round(y1*10) / 10}
		}
		type row struct {
			C    string    `json:"c"`
			N    string    `json:"n"`
			BL   string    `json:"bl,omitempty"`
			BZ   string    `json:"bz,omitempty"`
			T    string    `json:"t,omitempty"`
			Ha   int       `json:"ha,omitempty"`
			BBox []float64 `json:"bbox"`
		}
		idx := map[string]interface{}{}
		kinds := map[string]interface{}{}
		for ki := range neGlyphAdminKinds {
			k := &neGlyphAdminKinds[ki]
			units := f[k.Key]
			rows := make([]row, 0, len(units))
			for i := range units {
				u := &units[i]
				u.kind = k
				if k.Name != nil {
					u.N = k.Name(u)
				}
				a.by[u.C] = u
				rows = append(rows, row{C: u.C, N: u.N, BL: u.BL, BZ: u.BZ, T: u.T, Ha: u.Ha, BBox: fbox(u.BBox)})
			}
			idx[k.Key] = rows
			kinds[k.Key] = map[string]string{"kind": k.Kind, "notice": k.Notice}
		}
		idx["kinds"] = kinds
		idx["notice"] = "Gemeindegrenzen © Statistik Austria, CC BY 4.0 (Bezirke/Bundesländer dissolved from them); Natura 2000: European Environment Agency (EEA)"
		a.index, _ = json.Marshal(idx)
	})
	return a.err
}

func neGlyphAdminCodeOK(c string) bool {
	for i := range neGlyphAdminKinds {
		if neGlyphAdminKinds[i].Code.MatchString(c) {
			return true
		}
	}
	return false
}

// neGlyphAdminOutline — the outline JSON for one code (memoised).
func neGlyphAdminOutline(code string) []byte {
	a := &neGlyphAdmin
	u := a.by[code]
	if u == nil {
		return nil
	}
	a.mu.Lock()
	if b, ok := a.paths[code]; ok {
		a.mu.Unlock()
		return b
	}
	a.mu.Unlock()
	fr := &neGlyphFrame
	var sb strings.Builder
	for _, ring := range u.R {
		for i, p := range ring {
			x := (p[0]*fr.kx-fr.minx)*fr.scale + neGlyphPad
			y := (-p[1]-fr.miny)*fr.scale + neGlyphPad
			if i == 0 {
				sb.WriteByte('M')
			} else {
				sb.WriteByte('L')
			}
			sb.WriteString(strconv.FormatFloat(x, 'f', 1, 64))
			sb.WriteByte(' ')
			sb.WriteString(strconv.FormatFloat(y, 'f', 1, 64))
		}
		sb.WriteByte('Z')
	}
	x0, y0 := (u.BBox[0]*fr.kx-fr.minx)*fr.scale+neGlyphPad, (-u.BBox[3]-fr.miny)*fr.scale+neGlyphPad
	x1, y1 := (u.BBox[2]*fr.kx-fr.minx)*fr.scale+neGlyphPad, (-u.BBox[1]-fr.miny)*fr.scale+neGlyphPad
	b, _ := json.Marshal(map[string]interface{}{
		"c": code, "kind": u.kind.Kind, "n": u.N, "d": sb.String(),
		"bbox":   []float64{math.Round(x0*10) / 10, math.Round(y0*10) / 10, math.Round(x1*10) / 10, math.Round(y1*10) / 10},
		"notice": u.kind.Notice,
	})
	a.mu.Lock()
	if len(a.paths) >= neGlyphAdminLRUMax {
		for k, v := range a.paths { // bounded, not LRU-precise: drop any one
			delete(a.paths, k)
			a.bytes -= len(v)
			break
		}
	}
	a.paths[code] = b
	a.bytes += len(b)
	a.mu.Unlock()
	return b
}

// neGlyphAdminStats — for the debug inflight page.
func neGlyphAdminStats() map[string]interface{} {
	a := &neGlyphAdmin
	a.mu.Lock()
	defer a.mu.Unlock()
	n := len(a.by)
	return map[string]interface{}{"units": n, "outlines_memo": len(a.paths), "outlines_bytes": a.bytes, "max": neGlyphAdminLRUMax}
}

func handleNEGlyphAdmin(w http.ResponseWriter, r *http.Request) {
	if err := neGlyphAdminLoad(); err != nil {
		apiError(w, "admin outlines unavailable", 503)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(neGlyphAdmin.index)
}

func handleNEGlyphAdminOutline(w http.ResponseWriter, r *http.Request) {
	if err := neGlyphAdminLoad(); err != nil {
		apiError(w, "admin outlines unavailable", 503)
		return
	}
	c := r.URL.Query().Get("c")
	if !neGlyphAdminCodeOK(c) {
		apiError(w, "c= required: 1, 3 or 5 digits (Bundesland, Bezirk, Gemeinde) or a Natura 2000 SITECODE", 400)
		return
	}
	b := neGlyphAdminOutline(c)
	if b == nil {
		apiError(w, "unknown code", 404)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(b)
}
