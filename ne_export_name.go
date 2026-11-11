package main

import (
	"fmt"
	"strings"
	"time"
)

// Export file names. Every file the page hands out (CSV/JSON table, .qgs
// project, .qml style, .pmtiles archive, .zip bundle) is named the way the
// page's "recent" list labels the same thing: WHAT · WHERE · the state words
// of the view · the filter · the day —
//
//	umfeld-at-cells-view-Gschnitz-observed-trees-relief-f-sl-ge-11.3-2026-11-11.qgs
//	umfeld-at-stats-KG-81113-Gschnitz-2026-11-11.csv
//
// so two downloads of different places or states never differ only by the
// browser's "-3". ASCII only (asciiFold), no characters a file system minds.

// neExportSlug — a name as a file-name part: folded to ASCII, runs of anything
// but letters, digits and '.' become one '-', capped at max bytes.
func neExportSlug(s string, max int) string {
	s = asciiFold(s)
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '+':
			b = append(b, c)
		default:
			if n := len(b); n > 0 && b[n-1] != '-' {
				b = append(b, '-')
			}
		}
	}
	if len(b) > max {
		b = b[:max]
	}
	return strings.Trim(string(b), "-.")
}

// neExportBoxPlace — where a box is: the Gemeinde under its centre, else the
// centre itself.
func neExportBoxPlace(b [4]float64) string {
	lon, lat := (b[0]+b[2])/2, (b[1]+b[3])/2
	if b[2]-b[0] > 3 { // most of the country: no single Gemeinde tells it
		return "Austria"
	}
	if m := municipalityAtPoint(lon, lat); m != nil {
		gem, _ := m["name"].(string)
		gc, _ := m["gemeinde_code"].(string)
		// the KG under the centre, as the recent list labels a view: of the
		// Gemeinde's KGs whose box holds the point, the one centred nearest
		kg, best := "", 0.0
		for _, c := range KGCodesNearPointMem(lon, lat, 0) {
			if r := neStatsKGRow(c); r != nil && r.GemeindeCode == gc {
				dx, dy := (r.Centroid[0]-lon)*0.68, r.Centroid[1]-lat
				if d := dx*dx + dy*dy; kg == "" || d < best {
					kg, best = r.KGName, d
				}
			}
		}
		p := neExportSlug(kg, 32)
		if g := neExportSlug(gem, 32); g != "" && !strings.EqualFold(g, p) {
			if p != "" {
				p += "-"
			}
			p += g
		}
		if p != "" {
			return p
		}
	}
	return fmt.Sprintf("%.3fN-%.3fE", lat, lon)
}

var neExportKinds = map[string]string{"kg": "KG", "gemeinde": "Gemeinde", "bezirk": "Bezirk", "land": "Land", "natura2000": "Natura2000"}

// neExportUnitPlace — the unit(s) of an export: kind, code, name.
func neExportUnitPlace(units []*neStatsUnit) string {
	if len(units) == 0 {
		return "export"
	}
	u := units[0]
	kind := neExportKinds[u.kind]
	if kind == "" {
		kind = u.kind
	}
	p := []string{kind}
	if len(u.key) <= 16 && u.kind != "combined" && u.kind != "bbox" {
		p = append(p, neExportSlug(u.key, 16))
	}
	if u.name != "" {
		p = append(p, neExportSlug(u.name, 40))
	} else {
		p = append(p, neExportBoxPlace(u.bbox))
	}
	s := strings.Join(p, "-")
	if len(units) > 1 {
		s += fmt.Sprintf("+%d", len(units)-1)
	}
	return s
}

// neExportState — the state words of a share hash, as the recent list shows them.
func neExportState(share string) string {
	if share == "" {
		return ""
	}
	sv := neWhereParseShare(share)
	var st []string
	if sv.Profile == "obs" {
		st = append(st, "observed")
	}
	for _, f := range [][2]string{{"change", "change"}, {"diff", "diff"}, {"trees", "trees"}, {"relief", "relief"}, {"ortho", "photo"}, {"roads", "roads"}, {"subsidy", "subsidy"}} {
		if sv.has(f[0]) {
			st = append(st, f[1])
		}
	}
	return strings.Join(st, "-")
}

// neExportFilter — a canonical filter as a file-name part (f-sl-ge-11.3).
func neExportFilter(canon string) string {
	if canon == "" {
		return ""
	}
	s := strings.NewReplacer(">=", "-ge-", "<=", "-le-", "!=", "-ne-", ">", "-gt-", "<", "-lt-", "..", "-to-").Replace(canon)
	if s = neExportSlug(s, 32); s == "" {
		return "filtered"
	}
	return "f-" + s
}

// neExportStem — umfeld-at-<what>-<place>[-<state>][-<filter>][-<tail>]-<day>.
func neExportStem(what, place, share, canon, tail string) string {
	p := []string{"umfeld-at", what, place}
	for _, s := range []string{neExportState(share), neExportFilter(canon), tail} {
		if s != "" {
			p = append(p, s)
		}
	}
	return strings.Join(append(p, time.Now().Format("2006-01-02")), "-")
}
