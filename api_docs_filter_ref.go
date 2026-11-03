// api_docs_filter_ref.go — the filter reference block on the landing page:
// every field of the glyph filter (neFiltFields), its values, the plain words
// that stand for a field (neFiltAlias), the named ranges (neFiltWords) and
// the presets — generated from the catalogue so it cannot drift. Collapsed
// by default (<details id="filter-fields">); the glyph's suggestion list
// links to it ("all fields ↓", fltRefGo in ne_glyph_filt.go).
package main

import (
	"html"
	"sort"
	"strconv"
	"strings"
)

// neFilterRefHTML — the block, spliced into the landing page as {FILTER_REF}.
func neFilterRefHTML() string {
	esc := html.EscapeString
	// aliases per field, words per field
	alias := map[string][]string{}
	for a, f := range neFiltAlias {
		alias[f] = append(alias[f], a)
	}
	words := map[string][]string{}
	for w, x := range neFiltWords {
		i := strings.IndexAny(x, "=<>")
		if i < 0 {
			i = len(x)
		}
		words[x[:i]] = append(words[x[:i]], w+" = "+x)
	}
	for _, m := range []map[string][]string{alias, words} {
		for k := range m {
			sort.Strings(m[k])
		}
	}
	var b strings.Builder
	b.WriteString(`<details class="fref" id="filter-fields"><summary>Filter reference — every field, its values, and the words that stand for them</summary>`)
	b.WriteString(`<p>A filter is <code>field</code> <code>operator</code> <code>value</code>, joined by <code>,</code> (and) and <code>|</code> (or): <code>d=wald,sl>=20|el>=1500</code>. ` +
		`Ranges read <code>>=a</code>, <code><=b</code>, <code>a..b</code>; sets <code>=m1+m2</code>; a flag stands bare (<code>n2k</code>); ` +
		`<code>field=unk</code> keeps the cells without a value; <code>k>=n</code> keeps only match clusters of at least n cells. ` +
		`A field named without a value (<code>ht</code>, <code>d=</code>) takes its natural term (<code>ht>=20</code>), marked on the chip so it can be changed. Type a word in place of a term; the link always carries the numeric form.</p>`)
	// one line per field, grouped, flowing in columns: `d declared use · declared · use  geb bau acker … · forest = d=wald`
	b.WriteString(`<div class="fref-cols">`)
	var groups []string
	byGroup := map[string][]*neFiltField{}
	for i := range neFiltFields {
		fd := &neFiltFields[i]
		if _, ok := byGroup[fd.Group]; !ok {
			groups = append(groups, fd.Group)
		}
		byGroup[fd.Group] = append(byGroup[fd.Group], fd)
	}
	for gi, group := range groups {
		if gi > 0 {
			b.WriteString(`</ul>`)
		}
		b.WriteString(`<h4>` + esc(group) + `</h4><ul>`)
		for _, fd := range byGroup[group] {
			line := `<code>` + esc(fd.ID) + `</code> ` + esc(fd.Label)
			if a := alias[fd.ID]; len(a) > 0 {
				line += ` <span class="a">` + esc(strings.Join(a, " ")) + `</span>`
			}
			line += ` <span class="v">` + neFiltValuesHTML(fd) + `</span>`
			if w := words[fd.ID]; len(w) > 0 {
				line += ` <span class="w">` + esc(strings.Join(w, " · ")) + `</span>`
			}
			b.WriteString(`<li>` + line + `</li>`)
		}
	}
	b.WriteString(`</ul></div>`)
	b.WriteString(`<p class="muted">Presets: `)
	for i, p := range neFiltPresets {
		if i > 0 {
			b.WriteString(" · ")
		}
		b.WriteString(esc(p[0]) + " <code>" + esc(p[1]) + "</code>")
	}
	b.WriteString(`. The same grammar serves <code>/api/v1/ne/filter?bbox=&amp;f=</code>, <code>/api/v1/ne/stats/unit?…&amp;f=</code> and the glyph link (<code>;f=</code>); the catalogue as JSON: <code>/api/v1/ne/filter/fields</code>.</p></details>`)
	return b.String()
}

// neFilterRefHTML values: the range or the member set, members carry their label as a tooltip.
func neFiltValuesHTML(fd *neFiltField) string {
	esc := html.EscapeString
	switch fd.Kind {
	case nfNum:
		if fd.Max > 0 || fd.Min != 0 {
			return esc(neFiltNum(fd.Min) + "–" + neFiltNum(fd.Max) + " " + fd.Unit)
		}
		return "number"
	case nfBucket:
		p := make([]string, len(fd.Edges))
		for i, e := range fd.Edges {
			p[i] = strconv.Itoa(e)
		}
		return esc("classes " + strings.Join(p, " ") + " " + fd.Unit)
	case nfFlag:
		return "flag"
	case nfSites:
		return "flag · =site code"
	}
	var b strings.Builder
	for i, m := range fd.Members {
		if m == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		if fd.Labels != nil && i < len(fd.Labels) && fd.Labels[i] != "" && fd.Labels[i] != m {
			b.WriteString(`<abbr title="` + esc(fd.Labels[i]) + `">` + esc(m) + `</abbr>`)
		} else {
			b.WriteString(esc(m))
		}
	}
	return b.String()
}

// neFilterRefCSS — subtle: a muted summary line; the table inherits the page's.
const neFilterRefCSS = `details.fref{margin:.6rem 0 1rem}details.fref summary{cursor:pointer;color:var(--mut);font-size:.9em;list-style:none}details.fref summary::before{content:"▸ ";font-size:.85em}details.fref[open] summary::before{content:"▾ "}details.fref summary::-webkit-details-marker{display:none}details.fref summary:hover{color:var(--fg)}
.fref-cols{columns:2;column-gap:2.5rem;font-size:.85em;line-height:1.5}.fref-cols h4{margin:.4em 0 .1em;font-size:.78em;letter-spacing:.08em;text-transform:uppercase;color:var(--mut);font-weight:700;break-after:avoid}.fref-cols ul{list-style:none;margin:0 0 .3em;padding:0}.fref-cols li{margin:0;padding:0;break-inside:avoid;overflow-wrap:anywhere}.fref-cols code{color:var(--acc)}.fref-cols .a,.fref-cols .v,.fref-cols .w{color:var(--mut)}.fref-cols .a:before{content:"= "}.fref-cols .v{color:var(--fg);opacity:.75}.fref-cols .v:before{content:"· "}.fref-cols .w:before{content:"· "}.fref-cols abbr{text-decoration:none;border-bottom:1px dotted var(--rule);cursor:help}@media(max-width:640px){.fref-cols{columns:1}}`
