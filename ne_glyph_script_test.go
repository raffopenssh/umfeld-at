package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The landing glyph is one <script> inside a Go raw string. Two traps have each taken the whole glyph down
// once (docs/ne-glyph.md §Phone load triage): a syntax error (window.neGlyph undefined, blank map) and a
// helper declared one line too early so it nested inside the previous function (ReferenceError at the
// first tile). Both are invisible to `go build`; this test catches them before a deploy.
//
// Convention: every `function name(` at column 0 of the const is a script-scope helper and must sit at
// brace depth 1 (inside the IIFE). The lexer below skips strings, comments and regex literals.
func glyphScriptBody(t *testing.T) string {
	s := neGlyphScript
	s = strings.TrimPrefix(s, "<script>")
	s = strings.TrimSuffix(strings.TrimSpace(s), "</script>")
	if !strings.HasPrefix(s, "(function(){") || !strings.HasSuffix(strings.TrimSpace(s), "})();") {
		t.Fatalf("glyph script is not one IIFE: starts %q ends %q", s[:20], s[len(s)-20:])
	}
	return s
}

// glyphScopeDepths returns, for each `function name(` that starts a line, its brace depth; and the final depth.
func glyphScopeDepths(src string) (map[string]int, int) {
	out := map[string]int{}
	depth, i, n := 0, 0, len(src)
	lineStart := true
	var prev byte // last significant (non-space, non-comment) byte, for the regex heuristic
	regexBefore := func(b byte) bool { return b == 0 || strings.IndexByte("(,=:[!&|?{};+-*%<>~^", b) >= 0 }
	for i < n {
		c := src[i]
		switch {
		case c == '\n':
			lineStart = true
			i++
			continue
		case c == ' ' || c == '\t' || c == '\r':
			lineStart = false // column 0 only: indented `function` is an inner helper by convention
			i++
			continue
		case c == '/' && i+1 < n && src[i+1] == '/':
			for i < n && src[i] != '\n' {
				i++
			}
			continue
		case c == '/' && i+1 < n && src[i+1] == '*':
			j := strings.Index(src[i+2:], "*/")
			if j < 0 {
				return out, depth
			}
			i += j + 4
			continue
		case c == '\'' || c == '"' || c == '`':
			q := c
			i++
			for i < n && src[i] != q {
				if src[i] == '\\' {
					i++
				}
				i++
			}
			i++
			prev = q
			lineStart = false
			continue
		case c == '/' && (regexBefore(prev) || strings.HasSuffix(src[:i], "return")):
			i++
			cls := false
			for i < n && (cls || src[i] != '/') && src[i] != '\n' {
				if src[i] == '\\' {
					i++
				} else if src[i] == '[' {
					cls = true
				} else if src[i] == ']' {
					cls = false
				}
				i++
			}
			i++
			for i < n && src[i] >= 'a' && src[i] <= 'z' {
				i++
			}
			prev = ')'
			lineStart = false
			continue
		}
		if lineStart && strings.HasPrefix(src[i:], "function ") {
			j := i + len("function ")
			k := j
			for k < n && (src[k] == '_' || src[k] == '$' || src[k] >= 'a' && src[k] <= 'z' || src[k] >= 'A' && src[k] <= 'Z' || src[k] >= '0' && src[k] <= '9') {
				k++
			}
			if k > j {
				out[src[j:k]] = depth
			}
		}
		lineStart = false
		switch c {
		case '{':
			depth++
		case '}':
			depth--
		}
		prev = c
		i++
	}
	return out, depth
}

func TestGlyphScriptScope(t *testing.T) {
	src := glyphScriptBody(t)
	depths, end := glyphScopeDepths(src)
	if end != 0 {
		t.Fatalf("lexer ends at depth %d (unbalanced braces or lexer confused)", end)
	}
	must := []string{"hiCommit", "hiFlush", "hiHref", "hiTile", "relFetch", "relBake", "hiCull", "hiEvict", "hiDecode"}
	for _, m := range must {
		if _, ok := depths[m]; !ok {
			t.Errorf("helper %s not found at a line start — renamed? update the list", m)
		}
	}
	if len(depths) < 300 {
		t.Fatalf("only %d line-start functions found, expected hundreds", len(depths))
	}
	for name, d := range depths {
		if d != 1 {
			t.Errorf("function %s is at brace depth %d, not script scope (nested inside the previous function — move it)", name, d)
		}
	}
}

func TestGlyphScriptSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	src := glyphScriptBody(t)
	p := filepath.Join(t.TempDir(), "glyph.js")
	if err := os.WriteFile(p, []byte(peerText(src)), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, "--check", p).CombinedOutput(); err != nil {
		t.Fatalf("node --check: %v\n%s", err, out)
	}
}

// TestGlyphPressTracker — the hold state machine (neGlyphPressJS) under node: time + movement
// decide, never the target.
func TestGlyphPressTracker(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	p := filepath.Join(t.TempDir(), "press.js")
	if err := os.WriteFile(p, []byte(neGlyphPressJS+"\n"+glyphPressHarness), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, p).CombinedOutput(); err != nil {
		t.Fatalf("press tracker: %v\n%s", err, out)
	}
}

const glyphPressHarness = `
var assert=require('assert');
function mk(){var L={f:[],c:[]};L.t=pressTracker({fire:function(p){L.f.push(p)},cancel:function(w){L.c.push(w)}});return L}
var L;
L=mk();L.t.down(1,5,7,0);L.t.tick(499);assert.strictEqual(L.f.length,0,'no fire before ms');assert(L.t.active());
L.t.tick(500);assert.deepStrictEqual(L.f,[{x:5,y:7}],'fires after ms when still');assert(!L.t.active());
L.t.tick(900);L.t.tick(2000);assert.strictEqual(L.f.length,1,'fires only once');assert.strictEqual(L.t.up(1),true,'up reports fired');assert.deepStrictEqual(L.c,[]);
L=mk();L.t.down(1,0,0,0);L.t.move(1,11,0);assert.deepStrictEqual(L.c,['moved']);L.t.tick(600);assert.strictEqual(L.f.length,0,'move > slop cancels');
L=mk();L.t.down(1,100,100,0);L.t.move(1,106,108);L.t.move(1,110,100);L.t.tick(600);assert.deepStrictEqual(L.f,[{x:100,y:100}],'move <= slop keeps, start point reported');
L=mk();L.t.down(1,0,0,0);L.t.move(2,500,500);assert(L.t.active(),'another pointer id moving is not this press');
assert.strictEqual(L.t.down(2,50,50,100),false);assert.deepStrictEqual(L.c,['second']);L.t.tick(700);assert.strictEqual(L.f.length,0,'second pointer cancels');
L=mk();L.t.down(1,0,0,0);assert.strictEqual(L.t.up(1),false);assert.deepStrictEqual(L.c,['up']);L.t.tick(600);assert.strictEqual(L.f.length,0,'up before ms cancels');
L=mk();L.t.tick(600);L.t.move(1,9,9);assert.strictEqual(L.t.up(1),false);assert.deepStrictEqual(L.f,[]);assert.deepStrictEqual(L.c,[],'idle is inert');
L=mk();L.t.down(1,0,0,0);L.t.tick(500);L.t.up(1);L.t.down(1,3,3,1000);L.t.tick(1500);assert.strictEqual(L.f.length,2,'a new press after release fires again');
`
