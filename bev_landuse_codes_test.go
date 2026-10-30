package main

import "testing"

// nsSpecTable is Tabelle 8 "Nutzungssymbole (NS)" of the BEV
// Schnittstellenbeschreibung "Katastralmappe SHP" V2.9, pages 13–15,
// transcribed verbatim: {code: {Bezeichnung der Benützungsart, Bezeichnung der
// Nutzung}}.
//
// This test exists because the previous table was INVENTED — 22 of its codes
// carried plausible-looking German names that were simply wrong, and the two
// worst (48 "Verkehrsfläche" for what is actually Äcker/Wiesen/Weiden, 83
// "Fels/Geröll" for Gebäudenebenflächen) silently inverted land classification
// and therefore land valuation. Nothing in the pipeline could detect that,
// because a wrong label is still a valid string. Pinning the spec here is the
// cheapest way to make the next such drift fail loudly.
//
// The spec lists exactly these 26 codes and no others; a code absent from the
// table is not "unknown formatting", it does not exist, and MapNutzungssymbol
// must say so rather than guess.
var nsSpecTable = map[string][2]string{
	"40": {"Landwirtschaftlich genutzte Grundflächen", "Dauerkulturanlagen oder Erwerbsgärten"},
	"41": {"Bauflächen", "Gebäude"},
	"42": {"Sonstige", "Parkplätze"},
	"48": {"Landwirtschaftlich genutzte Grundflächen", "Äcker, Wiesen oder Weiden"},
	"52": {"Gärten", "Gärten"},
	"53": {"Weingärten", "Weingärten"},
	"54": {"Alpen", "Alpen"},
	"55": {"Wald", "Krummholzflächen"},
	"56": {"Wald", "Wälder"},
	"57": {"Landwirtschaftlich genutzte Grundflächen", "Verbuschte Flächen"},
	"58": {"Wald", "Forststraßen"},
	"59": {"Gewässer", "Fließende Gewässer"},
	"60": {"Gewässer", "Stehende Gewässer"},
	"61": {"Gewässer", "Feuchtgebiete"},
	"62": {"Sonstige", "Vegetationsarme Flächen"},
	"63": {"Sonstige", "Betriebsflächen"},
	"64": {"Gewässer", "Gewässerrandflächen"},
	"65": {"Sonstige", "Verkehrsrandflächen"},
	"72": {"Sonstige", "Friedhöfe"},
	"83": {"Bauflächen", "Gebäudenebenflächen"},
	"84": {"Sonstige", "Abbauflächen, Halden und Deponien"},
	"87": {"Sonstige", "Fels- und Geröllflächen"},
	"88": {"Sonstige", "Gletscher"},
	"92": {"Sonstige", "Schienenverkehrsanlagen"},
	"95": {"Sonstige", "Straßenverkehrsanlagen"},
	"96": {"Sonstige", "Freizeitflächen"},
}

func TestNSTableMatchesBEVSpec(t *testing.T) {
	for code, want := range nsSpecTable {
		if got := nsBenutzungsart[code]; got != want[0] {
			t.Errorf("code %s Benützungsart: got %q, spec %q", code, got, want[0])
		}
		if got := nsNutzung[code]; got != want[1] {
			t.Errorf("code %s Nutzung: got %q, spec %q", code, got, want[1])
		}
	}
	for code := range nsNutzung {
		if _, ok := nsSpecTable[code]; !ok {
			t.Errorf("code %s is in our table but NOT in the BEV spec — invented codes are how feedback #11 happened", code)
		}
	}
	for code := range nsBenutzungsart {
		if _, ok := nsSpecTable[code]; !ok {
			t.Errorf("code %s has a Benützungsart but is not in the BEV spec", code)
		}
	}
}

// The two codes whose old labels were most damaging: 48 is the single most
// common code in Austria (3.76M parcels carry it) and was priced as road
// surface; 83 is a Baufläche that was labelled as rock/scree.
func TestNSHeadlineCodesAreFarmlandAndBuilding(t *testing.T) {
	if nsBenutzungsart["48"] != "Landwirtschaftlich genutzte Grundflächen" {
		t.Fatal("code 48 must be farmland, not Verkehrsfläche")
	}
	if nsBenutzungsart["83"] != "Bauflächen" {
		t.Fatal("code 83 must be a Baufläche, not Fels/Geröll")
	}
}

// An unknown code must be visibly unknown, never a plausible invention.
func TestUnknownNSCodeIsMarkedUnknown(t *testing.T) {
	if got := MapNutzungssymbol("99"); got != "Unbekannt - Code 99" {
		t.Fatalf("got %q", got)
	}
	// Codes the OLD table defined but BEV does not (e.g. 51, 91, 97) must not
	// silently resolve to their invented names.
	for _, c := range []string{"49", "50", "51", "91", "97"} {
		if got := MapNutzungssymbol(c); got != "Unbekannt - Code "+c {
			t.Errorf("removed code %s still resolves to %q", c, got)
		}
	}
}

// Every code must have a display label and an abbreviation, or API rows come
// back with empty strings.
func TestNSLabelsComplete(t *testing.T) {
	for code := range nsSpecTable {
		if nutzungssymbolCodes[code] == "" {
			t.Errorf("code %s has no display label", code)
		}
		if nutzungAbbreviations[code] == "" {
			t.Errorf("code %s has no abbreviation", code)
		}
	}
}
