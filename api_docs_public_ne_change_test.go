package main

import (
	"os"
	"strings"
	"testing"
)

// TestNEContribChangeCard — the change section of the day card and the chart
// flag: rate = land use moved per km² compared, flagged at ≥ 3× the usual (or
// any confirmed delta); confirmed rows in colour, candidate rows labelled.
func TestNEContribChangeCard(t *testing.T) {
	old := neChangeNationalFn
	defer func() { neChangeNationalFn = old }()
	conf := &neChangeTotals{Cells: 7, DCover: 28_900, DN: 12, Transitions: map[string]int{"green>structure": 34_000, "arable>built": 12_500, "forest>green": 4_100}}
	cand := &neChangeTotals{Cells: 3, DN: 2, DCover: 1_200, Transitions: map[string]int{"green>built": 9_000}}
	neChangeNationalFn = func(bool) map[string]interface{} {
		return map[string]interface{}{
			"by_day":           map[string]*neChangeTotals{"2026-10-20": conf},
			"candidate_by_day": map[string]*neChangeTotals{"2026-10-20": cand, "2026-10-15": {Cells: 1, Transitions: map[string]int{"green>built": 600}}},
		}
	}
	daily := make([]neDayStat, 28)
	for i := range daily {
		daily[i].Day = "2026-10-" + string(rune('0'+(i+1)/10)) + string(rune('0'+(i+1)%10))
		if i < 2 {
			continue
		}
		daily[i].Chunks, daily[i].KGs, daily[i].Reports = 400+37*i, 3, 4
		daily[i].Passes = [nePassBandN]int{200, 150, 50 + 37*i, 0, 0}
		if i%3 == 0 {
			daily[i].Changed = 2
		}
	}
	daily[19].Changed = 11 // 2026-10-20
	daily[14].Changed = 1  // 2026-10-15: 600 m² — below the 1 ha floor
	daily[4].Chunks, daily[4].Passes = 300, [nePassBandN]int{300}

	sig := neChangeSignal(daily, 19)
	if !sig.Unusual || sig.Confirmed != conf || sig.Candidate != cand || sig.MovedM2 != 59_600 {
		t.Fatalf("signal: %+v", sig)
	}
	if s := neChangeSignal(daily, 14); s.Unusual {
		t.Fatalf("600 m² must not flag: %+v", s)
	}
	if s := neChangeSignal(daily, 4); s.Compared != 0 || s.Unusual {
		t.Fatalf("first-sight day: %+v", s)
	}
	svg := neContribChartSVG(daily)
	if strings.Count(svg, `class="day flag"`) != 1 || !strings.Contains(svg, `class="cap"`) || !strings.Contains(svg, "land-use change 6.0 ha (first in this window)") {
		t.Fatalf("flag: %s", svg)
	}
	card := neContribChangeHTML(daily, 19)
	for _, want := range []string{`class="dc conf"`, "land use changed</strong> on 6.0 ha", `class="tag">`, "ha per km² compared", `<div class="sb"><i class="s0" style="width:67.2%"`, "grassland → buildings", "3.4 ha", "building outlines</span> <strong>+12</strong> new", "roof +2.9 ha", "confirmed ✓ 2 sightings</span> <span class=\"mut\">· 7 cells", "candidate · 1 sighting</span> <span class=\"mut\">· 3 cells", `class="stk c"`} {
		if !strings.Contains(card, want) {
			t.Fatalf("card lacks %q: %s", want, card)
		}
	}
	if c := neContribChangeHTML(daily, 4); !strings.Contains(c, "all first sight") {
		t.Fatalf("first sight: %s", c)
	}
	if c := neContribChangeHTML(daily, 3); !strings.Contains(c, "not derived yet") {
		t.Fatalf("differed, nothing moved: %s", c)
	}
	if c := neContribChangeHTML(daily, 5); !strings.Contains(c, "no change detected in") {
		t.Fatalf("unchanged: %s", c)
	}
	for _, h := range []string{svg, card} {
		if bad := nePublicBannedWords(h); len(bad) > 0 {
			t.Fatalf("banned: %v", bad)
		}
	}
	if p := os.Getenv("NE_PREVIEW"); p != "" {
		cum := make([]int, len(daily))
		page := "<!doctype html><meta name=viewport content='width=device-width'><style>" + monoDocsCSS + "</style><body><main><h1>preview</h1><figure class=\"contrib\">" + svg + neContribDetailHTML(daily, cum, nil) + "<figcaption>x</figcaption></figure>" + neContribScript + "</main>"
		os.WriteFile(p, []byte(page), 0o644)
	}
}
