// ne_glyph_diag.go — the landing glyph's field diagnostics, spliced into
// neGlyphScript (ne_glyph.go). Three plain JS files, so node can check and test
// them (ne_glyph_diag_test.go); docs/ne-glyph.md §Field diagnostics.
//
//	ne_glyph_kx.js     KX: the one switch registry (?kx=a,b · the ?deep panel · ?run),
//	                   the run scenarios (KXRUN) and their pure state machine (kxRunStep)
//	ne_glyph_trace.js  ?trace field recorder (env, state, input rows, heartbeat,
//	                   watchdog Worker, carry-over) and the ?run driver
//	ne_glyph_deep.js   ?deep: the switch panel + DOM / animation dumps
package main

import _ "embed"

//go:embed ne_glyph_kx.js
var neGlyphKxJS string

//go:embed ne_glyph_trace.js
var neGlyphTraceJS string

//go:embed ne_glyph_deep.js
var neGlyphDeepJS string
