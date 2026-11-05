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
