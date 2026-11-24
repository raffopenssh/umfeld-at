// api_docs_method.go — "How the data was made": one short method note per
// family of things this service serves, written once and rendered twice —
// collapsed on the landing page ({METHOD} in publicDocsHTMLTemplate) and as a
// section of llm.txt ({METHOD} in publicLLMTemplate) — so the two never drift.
//
// Keep it plain: what went in, what was done to it, how far to trust it. No
// endpoint lists (the tables below it carry those), no private paths, peer
// names only as {OBS_NAME}/{SUB_NAME}/{MAP_NAME} (served through peerText).
// When a rule here changes (thresholds, cadence, sample rate), change the
// sentence with it.
package main

import (
	"html"
	"strings"
)

type methodPara struct{ Head, Text string }

var publicMethodParas = []methodPara{
	{"Declared use", `The official cadastre is published by BEV as vector tiles (CC BY 4.0). Peers stitch those tiles into ` +
		`seamless geometry on their own machines (vtcseamless) and lay it over H3 hexagons of about 300 m² (res 12). For ` +
		`each cell an open, frozen reference implementation (ne_cells) measures the area share of nine land-use groups — ` +
		`buildings, building land, arable, grassland, forest, water, traffic, alpine, other — plus roof cover, the number ` +
		`of building outlines and the density of boundary lines. Only these per-cell figures and a digest of each block ` +
		`of cells reach this server; no parcel does. Shares are stored in steps of 1/255 of a cell, about 1.2 m².`},
	{"Change over time", `All 7 850 Katastralgemeinden are digested again about once a month. When the digest of a block ` +
		`differs from the last one, its cells are compared and the difference is kept in m² per group — first as a ` +
		`candidate. It becomes confirmed when the same new state is sighted again on a later pass or by a second ` +
		`observer; public figures count confirmed change only. Builds that came back incomplete (a missing tile) are set ` +
		`aside as coverage, never counted. Dates are the days we saw the register differ, not the day something happened ` +
		`on the ground.`},
	{"Observed use, trees, diff, subsidy", `These layers are made by peers on the same hexagons; we store, join and ` +
		`paint them unaltered. {OBS_NAME} segments LiDAR, orthophoto and satellite series into land cover, heights and ` +
		`single tree tops (its own method note: {OBS_METHOD}), and supplies the relief. Diff sets declared against ` +
		`observed for the same cell today and marks only three robust cases: structures declared but under 5 percent observed ` +
		`cover, structures observed with a height gain of 2 m or more but none declared (under a quarter or more crown cover only ` +
		`where a building was counted, since trees grow that much), and declared forest that was cut ` +
		`and holds no tree top. A height change exists only where a place was laser-scanned twice — about a quarter of ` +
		`the cells, a median of 12 years apart; elsewhere it is empty rather than zero, in the filter too, and a cut is ` +
		`then taken from the satellite loss year alone. Flights are years old (each cell knows its flight years) and a ` +
		`clear-cut is forest in the register by design, so a mark says where to look, not who is wrong. Subsidy shows INVEKOS Schläge (AMA) as ` +
		`farmers drew them for their claims; {SUB_NAME} rolls them onto the cells and compares each year with the one ` +
		`before (new, redrawn, gone).`},
	{"Filter and counts", `Every filter field is a per-cell value from the layers above, or a distance computed from ` +
		`OpenStreetMap. Zoomed in, the filter and its counts are exact cell by cell; farther out they come from a ` +
		`national sample of one cell in 49, so those counts are estimates — responses say so ` +
		`(exact:false, approx:true), as do exports of large units.`},
	{"Around a point", `Municipality, Natura 2000 site and protected area are point-in-polygon tests against the ` +
		`published outlines (Statistik Austria, EEA, WDPA). Distances to road, rail, transit stop, water and settlement ` +
		`are exact distances to the nearest OpenStreetMap feature; remoteness (0–100) is a weighted mix of them. Place ` +
		`names are BEV's official name register (DLM-Namen), reprojected to WGS84. Law references come from one scan of ` +
		`the RIS Landesrecht for texts that name a Katastralgemeinde, keyed by its code. The land price is modelled: the ` +
		`average Statistik Austria publishes for the Gemeinde (farmland: for the Bundesland), scaled by the size, shape ` +
		`and setting of the plot you describe — an estimate, not a valuation.`},
}

// publicMethodHTML — the note collapsed under the landing page's intro, in the
// look of the filter reference (details.fref) so the page gains one line.
func publicMethodHTML() string {
	var b strings.Builder
	b.WriteString(`<details class="fref" id="how-made"><summary>How the data was made — sources, method and limits of each layer</summary>`)
	for _, p := range publicMethodParas {
		t := html.EscapeString(p.Text)
		t = strings.Replace(t, "{OBS_NAME}", `<a href="{OBS_WEB}/">{OBS_NAME}</a>`, 1)
		t = strings.Replace(t, "{OBS_METHOD}", `<a href="{OBS_WEB}/#how-made">how it was made</a>`, 1)
		t = strings.Replace(t, "{SUB_NAME}", `<a href="{SUB_API}/">{SUB_NAME}</a>`, 1)
		t = strings.Replace(t, "vtcseamless", `<a href="https://github.com/raffopenssh/vtcseamless">vtcseamless</a>`, 1)
		t = strings.Replace(t, "ne_cells", `<a href="/api/v1/docs/ne.md">ne_cells</a>`, 1)
		b.WriteString(`<p><strong>` + html.EscapeString(p.Head) + `.</strong> ` + t + `</p>`)
	}
	b.WriteString(`</details>`)
	return b.String()
}

// publicMethodText — the same note for llm.txt, wrapped at 108 columns.
func publicMethodText() string {
	var b strings.Builder
	for i, p := range publicMethodParas {
		if i > 0 {
			b.WriteByte('\n')
		}
		col := 0
		for _, w := range strings.Fields(p.Head + ". " + strings.Replace(p.Text, "{OBS_METHOD}", "{OBS_WEB}/api/v1/docs/method", 1)) {
			n := len([]rune(w))
			if col > 0 && col+1+n > 108 {
				b.WriteByte('\n')
				col = 0
			} else if col > 0 {
				b.WriteByte(' ')
				col++
			}
			b.WriteString(w)
			col += n
		}
		b.WriteByte('\n')
	}
	return b.String()
}
