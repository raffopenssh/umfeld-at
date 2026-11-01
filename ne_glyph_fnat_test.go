package main

import "testing"

// TestNEFnatEval — the overview fold over a synthetic two-row national level: member counts add within an OR
// group, AND groups take the min (upper bound, exact=false), unfoldable terms land in skipped, k>= too.
func TestNEFnatEval(t *testing.T) {
	saved := neGlyphFrame.cells
	defer func() { neGlyphFrame.cells = saved }()
	neGlyphFrame.cells = []neGlyphPos{{id: 10}, {id: 20}, {id: 30}}
	lv := &neNatLevel{Res: 7, IDs: []uint64{10, 20}, Rows: make([]neNatRow, 2)}
	lv.Rows[0] = neNatRow{N: 100, ChgC: 10, ChgK: 5}
	lv.Rows[0].D[4] = 60 // wald
	lv.Rows[0].D[7] = 20 // alpen
	lv.Rows[0].O[4] = 30
	lv.Rows[0].D[9] = 5 // unknown
	lv.Rows[1] = neNatRow{N: 50}
	lv.Rows[1].D[4] = 50

	eval := func(f string) *neFnatResult { return neFnatEval(neFiltParse(f), lv) }

	r := eval("d=wald")
	if !r.Exact || r.Cells != 150 || r.Match != 110 || r.Rows != 2 {
		t.Fatalf("d=wald: %+v", r)
	}
	if len(r.Share) != 3 || r.Share[0] != 152 || r.Share[1] != 254 || r.Share[2] != 255 {
		t.Fatalf("share %v", r.Share)
	}
	if len(r.N) != 3 || r.N[0] != 100 || r.N[1] != 50 || r.N[2] != 0 {
		t.Fatalf("n %v", r.N)
	}
	if r := eval("d=wald+alpen"); r.Match != 130 || !r.Exact {
		t.Fatalf("or within a set: %+v", r)
	}
	if r := eval("d=wald|o=wald"); r.Match != 140 || !r.Exact {
		t.Fatalf("or across fields: match=%d exact=%v", r.Match, r.Exact)
	}
	if r := eval("d=wald,o=wald"); r.Match != 30 || r.Exact {
		t.Fatalf("and = min: match=%d exact=%v", r.Match, r.Exact)
	}
	if r := eval("d=unk"); r.Match != 5 {
		t.Fatalf("unk: %d", r.Match)
	}
	if r := eval("chg=conf+cand"); r.Match != 15 || r.Share[1] != 0 {
		t.Fatalf("chg: %+v", r)
	}
	if r := eval("chg=none"); r.Match != 135 {
		t.Fatalf("chg=none: %d", r.Match)
	}
	r = eval("d=wald,sl>=15")
	if r.Exact || r.Match != 110 || len(r.Skipped) != 1 || len(r.Coarse) != 1 || r.Coarse[0] != "d" {
		t.Fatalf("and with unfoldable: %+v", r)
	}
	r = eval("d=wald|sl>=15")
	if r.Share != nil || len(r.Skipped) != 2 || len(r.Coarse) != 0 {
		t.Fatalf("or with unfoldable skips the group: %+v", r)
	}
	if r := eval("sl>=15"); r.Share != nil || r.Cells != 0 {
		t.Fatalf("nothing foldable: %+v", r)
	}
	if r := eval("d=wald,k>=3"); r.Exact || len(r.Skipped) != 1 {
		t.Fatalf("k>= skipped: %+v", r)
	}
}
