package main

import "testing"

// A reported unit: 179 116 m² of which the register says 155 722 m² is
// Landwirtschaft, 12 636 Wald, 910 Baufläche. It carries a footprint, so the
// old rule ("any building ⇒ bauland_built") priced ALL 17.9 ha at the Bauland
// rate: 3 421 329 EUR instead of ~340 000.
func TestAreaClassBeatsStraySymbols(t *testing.T) {
	split := &LanduseAreaSplit{
		Areas: map[string]float64{
			"48": 144842, // Äcker, Wiesen oder Weiden  (was mislabelled "Verkehrsfläche")
			"56": 12889,  // Wälder
			"83": 910,    // Gebäudenebenflächen (Baufläche)
		},
		CoveredSqm: 158641,
	}
	cls, src, share := classifyParcelByArea(0, 1, split)
	if cls != ClassAckerland {
		t.Fatalf("class: got %s want %s", cls, ClassAckerland)
	}
	if src != "area" {
		t.Fatalf("class_source: got %q want area", src)
	}
	if share < 0.85 {
		t.Fatalf("dominant share too low: %f", share)
	}
}

// A real building plot must still be bauland_built.
func TestBuiltPlotStaysBauland(t *testing.T) {
	split := &LanduseAreaSplit{
		Areas:      map[string]float64{"83": 400, "52": 200},
		CoveredSqm: 600,
	}
	if cls, _, _ := classifyParcelByArea(1, 1, split); cls != ClassBaulandBuilt {
		t.Fatalf("got %s want bauland_built", cls)
	}
}

// Forest that is 96 % Wald must not be gruenland.
func TestForestClassifiesAsWald(t *testing.T) {
	split := &LanduseAreaSplit{
		Areas:      map[string]float64{"56": 192887, "48": 8200},
		CoveredSqm: 201087,
	}
	if cls, _, _ := classifyParcelByArea(0, 0, split); cls != ClassWald {
		t.Fatalf("got %s want wald", cls)
	}
}

// Farmland that is 97 % Landwirtschaft must not be wald.
func TestFarmlandNotForest(t *testing.T) {
	split := &LanduseAreaSplit{
		Areas:      map[string]float64{"48": 42125, "56": 1178},
		CoveredSqm: 43303,
	}
	if cls, _, _ := classifyParcelByArea(0, 0, split); cls != ClassAckerland {
		t.Fatalf("got %s want ackerland", cls)
	}
}

func TestNilSplitFallsBackToSymbols(t *testing.T) {
	if cls, _, _ := classifyParcelByArea(0, 0, nil); cls != "" {
		t.Fatalf("nil split should yield empty class, got %s", cls)
	}
}

// The corrected BEV code table is the whole basis of the fix — guard it.
func TestBEVCodeTableMatchesSpec(t *testing.T) {
	want := map[string]string{
		"48": "Äcker, Wiesen oder Weiden",
		"52": "Gärten",
		"53": "Weingärten",
		"54": "Alpen",
		"55": "Krummholzflächen",
		"56": "Wälder",
		"57": "Verbuschte Flächen",
		"58": "Forststraßen",
		"83": "Gebäudenebenflächen",
		"95": "Straßenverkehrsanlagen",
	}
	for code, nutzung := range want {
		if got := nsNutzung[code]; got != nutzung {
			t.Errorf("NS %s: got %q want %q", code, got, nutzung)
		}
	}
	if MapBenutzungsart("48") != "Landwirtschaftlich genutzte Grundflächen" {
		t.Errorf("NS 48 Benützungsart: %q", MapBenutzungsart("48"))
	}
	if MapBenutzungsart("83") != "Bauflächen" {
		t.Errorf("NS 83 Benützungsart: %q", MapBenutzungsart("83"))
	}
	// Codes we invented must be gone, not silently mislabelled.
	for _, gone := range []string{"43", "44", "45", "46", "47", "49", "50", "51", "66", "67", "70", "71", "91", "97"} {
		if _, ok := nsNutzung[gone]; ok {
			t.Errorf("invented code %s still present", gone)
		}
	}
}

func TestBreakdownIsSortedAndShares(t *testing.T) {
	s := &LanduseAreaSplit{
		Areas:      map[string]float64{"56": 100, "48": 300},
		CoveredSqm: 400,
	}
	b := s.Breakdown()
	if len(b) != 2 || b[0]["code"] != "48" {
		t.Fatalf("not sorted by area: %+v", b)
	}
	if b[0]["share"].(float64) != 0.75 {
		t.Fatalf("share: %v", b[0]["share"])
	}
}
