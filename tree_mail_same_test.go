package main

import (
	"strings"
	"testing"
	"time"
)

func init() {
	tmNoteAlone = func() bool { return true }
	tmNoteLimit = func() int { return tmNoteMax }
}

func tmTree(n int, name, id, h, crown string) tmRow {
	return tmRow{n: n, name: name, attrs: map[string]string{"id": id, "h": h, "crown_d": crown}}
}

// "same tree" by mail: the tallest unmarked neighbour whose crown touches is
// the tree; the taller top stays whichever one the reader named.
func TestTreeMailSameTree(t *testing.T) {
	// three tops of one beech (≈ 4–5 m apart), a spruce 40 m away
	rows := []tmRow{
		tmTree(1, "26 m", "t16158400_48262400", "26", "6"),
		tmTree(2, "gleicher Baum", "t16158450_48262420", "24.5", "5"),
		tmTree(3, "Zwiesel", "t16158360_48262430", "23", "5"),
		tmTree(4, "18 m spruce", "t16158900_48262600", "18", "4"),
	}
	rows[3].attrs["species"] = "spruce"
	items, same, refused := tmDiffRows(rows, "m", time.Now())
	if len(items) != 2 || same != 2 || len(refused) != 0 {
		t.Fatalf("items %d same %d refused %v", len(items), same, refused)
	}
	for i, it := range items {
		b := it.body
		ex, _ := b["expected"].(map[string]interface{})
		of, _ := ex["of"].(map[string]interface{})
		if b["claim"] != "spurious" || ex["hint"] != "crown" || of["lon"] != 16.1584 || of["lat"] != 48.2624 {
			t.Fatalf("item %d: %v", i, b)
		}
		if b["lon"] != rowsLon(rows[i+1]) || !strings.HasPrefix(it.what, "same tree as the 26 m top") {
			t.Fatalf("item %d: subject %v, what %q", i, b["lon"], it.what)
		}
		if u := tmUndo(it).body; u["retract"] != true || u["claim"] != "spurious" || u["lon"] != b["lon"] {
			t.Fatalf("undo %v", u)
		}
	}
	// the reader named the TALLER top: the lower one is flagged, of = the named one
	rows = []tmRow{tmTree(1, "same tree", "t16158400_48262400", "26", "6"), tmTree(2, "25 m", "t16158450_48262420", "24.5", "5")}
	items, _, _ = tmDiffRows(rows, "m", time.Now())
	if len(items) != 1 || items[0].body["lon"] != 16.15845 || !strings.Contains(items[0].what, "taller top stays") {
		t.Fatalf("swap: %+v", items)
	}
	if of := items[0].body["expected"].(map[string]interface{})["of"].(map[string]interface{}); of["lon"] != 16.1584 {
		t.Fatalf("swap of %v", of)
	}
	// nobody beside it · not a tree of the export
	_, _, refused = tmDiffRows([]tmRow{tmTree(1, "same tree", "t16158400_48262400", "26", "6"), tmTree(2, "18 m", "t16158900_48262600", "18", "4")}, "m", time.Now())
	if len(refused[tmCrownNone]) != 1 {
		t.Fatalf("alone: %v", refused)
	}
	if _, why := tmDiff(tmRow{n: 1, name: "same tree", lon: 16.2, lat: 48.2, hasPt: true, attrs: map[string]string{}}, "m", time.Now()); !strings.Contains(why, "needs a tree of the export") {
		t.Fatalf("no id: %q", why)
	}
	// the species of the tree rides beside the flag of its second top
	rows = []tmRow{tmTree(1, "Buche 30 m bhd 80 cm", "t16158400_48262400", "26", "6"), tmTree(2, "doppelt", "t16158450_48262420", "24.5", "5")}
	if items, _, _ = tmDiffRows(rows, "m", time.Now()); len(items) != 2 || items[0].body["species"] != "Buche" || !items[1].crown {
		t.Fatalf("combined: %+v", items)
	}
}

func rowsLon(r tmRow) float64 { lon, _, _, _ := tmPos(r); return lon }

// Notes: what the reader wrote is a report; unread fields ride along with an
// edit only; app fields never; personal values are left out; 280 at most.
func TestTreeMailNote(t *testing.T) {
	now := time.Now()
	desc := `<br /><tr><td><table width="100%"><tr><td align="left"><small><b>id</b></small></td><td align="center">t16158405_48262430</td></tr>
<tr><td align="left"><small><b>h</b></small></td><td align="center">20</td></tr>
<tr><td align="left"><small><b>crown_d</b></small></td><td align="center">7.9</td></tr>
<tr><td align="left"><small><b>vitality</b></small></td><td align="center">vital</td></tr>
<tr><td align="left"><small><b>plot</b></small></td><td align="center">A7</td></tr>
</table></td></tr>`
	mk := func(name, extra string) tmRow {
		r := tmRow{n: 1, name: name, attrs: map[string]string{}}
		tmDescAttrs(desc+extra, &r)
		return r
	}
	// untouched: an unread field alone is no edit
	if its, why := tmDiff(mk("20 m", ""), "m", now); len(its) != 0 || why != "" {
		t.Fatalf("untouched: %v %q", its, why)
	}
	// typed below the table: a note of its own, the unread field with it
	its, why := tmDiff(mk("20 m", "<br>Wespennest am Stammfuss<br>call me 0664 123 45 67"), "m", now)
	if why != "" || len(its) != 1 || its[0].body["note"] != "Wespennest am Stammfuss · plot: A7" || its[0].dropped != 1 || its[0].body["claim"] != "tree" {
		t.Fatalf("typed: %+v %q", its, why)
	}
	// an edit carries the unread fields
	its, _ = tmDiff(mk("Fichte", ""), "m", now)
	if len(its) != 1 || its[0].body["note"] != "plot: A7" || its[0].body["species"] != "Fichte" {
		t.Fatalf("edit: %+v", its)
	}
	// long → cut at 280 bytes, on a rune
	its, _ = tmDiff(mk("Fichte", "<br>"+strings.Repeat("Währ ", 80)), "m", now)
	if n := its[0].body["note"].(string); len([]rune(n)) != tmNoteMax || !strings.HasSuffix(n, "…") || !its[0].cut {
		t.Fatalf("cut: %d %v", len(n), its[0].cut)
	}
	// before the peer stores notes: a note alone is refused, with an edit it travels
	tmNoteAlone = func() bool { return false }
	defer func() { tmNoteAlone = func() bool { return true } }()
	if _, why := tmDiff(mk("20 m", "<br>Wespennest"), "m", now); !strings.Contains(why, "note alone") {
		t.Fatalf("gate: %q", why)
	}
	// CSV: unread columns, app columns ignored, a date is not a phone number
	rows, err := tmParseCSV([]byte("name,lat,lon,alt,acc,time,id,h,Baumnummer,Pflege,owner\nEiche,48.26243,16.158405,365,3,2026-11-20T16:59:12Z,t16158405_48262430,20,17,Kronenschnitt 2025-08-14,a@b.at\n"))
	if err != nil {
		t.Fatal(err)
	}
	its, _ = tmDiff(rows[0], "m", now)
	if len(its) != 1 || its[0].body["note"] != "Baumnummer: 17 · Pflege: Kronenschnitt 2025-08-14" || its[0].dropped != 1 {
		t.Fatalf("csv: %+v", its)
	}
}
