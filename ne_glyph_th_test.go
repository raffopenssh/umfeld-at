package main

import (
	"strings"
	"testing"
)

// The height ramp: both skins monotone in luminance (bright = tall on dark, dark = tall on light),
// and a ;trees=h link turns the QGIS project to the ramp's classes.
func TestGlyphTreeHeights(t *testing.T) {
	lum := func(c [3]uint8) float64 { return .2126*float64(c[0]) + .7152*float64(c[1]) + .0722*float64(c[2]) }
	for i := 1; i < len(neGlyphThRamp); i++ {
		if lum(neGlyphThRamp[i]) <= lum(neGlyphThRamp[i-1]) {
			t.Errorf("dark-page ramp not rising at stop %d", i)
		}
		if lum(neGlyphThRampL[i]) >= lum(neGlyphThRampL[i-1]) {
			t.Errorf("light-page ramp not falling at stop %d", i)
		}
	}
	if v := neQGSViewOf("#kg=22022;full;trees=h;z=300"); !v.trees || !v.treesH {
		t.Errorf("trees=h not read: %+v", v)
	}
	if v := neQGSViewOf("#kg=22022;full;trees;z=300"); !v.trees || v.treesH {
		t.Errorf("plain trees: %+v", v)
	}
	if s := neQGSTreeStyles(true); !strings.Contains(s, "40–45 m") || strings.Contains(neQGSTreeStyles(false), "40–45 m") {
		t.Errorf("tree styles do not follow the mode")
	}
	for _, w := range []string{"thFlip", "trees=h", "shiftKey", "contextmenu"} {
		if !strings.Contains(neGlyphScript+neGlyphThJS, w) {
			t.Errorf("page script lost %q", w)
		}
	}
}
