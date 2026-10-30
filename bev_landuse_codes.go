package main

// bev_landuse_codes.go — BEV Nutzungssymbol (NS) code table.
//
// SOURCE OF TRUTH: BEV Schnittstellenbeschreibung "Katastralmappe VTC" v1.0,
// §4.2 "Nutzungssymbole (ns)" (and the identical Tabelle 8 in the SHP v2.9
// spec). Both the nfl polygon layer and the nsy symbol layer use this code set.
//
// ⚠️ THIS TABLE WAS PREVIOUSLY WRONG, and the errors were not cosmetic — they
// silently inverted land classification and therefore land VALUATION:
//
//	code  was (invented)              actually (BEV)
//	48    "Verkehrsfläche - V"        Äcker, Wiesen oder Weiden  ← FARMLAND
//	52    "Grünland (Wiese) - LN(W)"  Gärten
//	53    "Grünland (Weide)"          Weingärten
//	54    "Grünland (Hutweide)"       Alpen
//	55    "Grünland (Streuwiese)"     Krummholzflächen (Wald)
//	57    "Krummholz/Strauch - W(Kr)" Verbuschte Flächen (landwirtschaftlich)
//	58    "Alpe"                      Forststraßen (Wald)
//	59    "Ödland/Heide"              Fließende Gewässer
//	60    "Sumpf/Moor"                Stehende Gewässer
//	61    "Grünland (gemäht)"         Feuchtgebiete
//	62    "Acker (Ackerland)"         Vegetationsarme Flächen
//	63    "Weingarten - WG"           Betriebsflächen
//	64    "Gartenanlage"              Gewässerrandflächen
//	65    "Obstgarten"                Verkehrsrandflächen
//	72    "Quelle/Brunnen"            Friedhöfe
//	83    "Fels/Geröll"               Gebäudenebenflächen (BAUFLÄCHE)
//	84    "Geröll/Steinschlag"        Abbauflächen, Halden und Deponien
//	87    "Flughafen"                 Fels- und Geröllflächen
//	88    "Hafen"                     Gletscher
//	92    "Alpe (Hochalm)"            Schienenverkehrsanlagen
//	95    "Sonstige"                  Straßenverkehrsanlagen
//	96    "Gewässer - GW"             Freizeitflächen
//
// Code 48 alone accounted for the headline error in feedback #11: the single
// most common code in the country (the "Äcker, Wiesen oder Weiden" that covers
// most of rural Austria) was labelled and priced as road surface.
//
// Codes NOT listed by BEV have been removed rather than guessed. A scan of 25
// random KGs found only the 22 codes below in the wild, all of them official.
// Unknown codes now surface as "Unbekannt - Code NN" instead of being silently
// mislabelled with a plausible-looking German name.

// nsBenutzungsart is the BEV "Bezeichnung der Benützungsart" — the top-level
// legal category (Benützungsart 1–8 of the Grundstücksverzeichnis).
var nsBenutzungsart = map[string]string{
	"40": "Landwirtschaftlich genutzte Grundflächen",
	"41": "Bauflächen",
	"42": "Sonstige",
	"48": "Landwirtschaftlich genutzte Grundflächen",
	"52": "Gärten",
	"53": "Weingärten",
	"54": "Alpen",
	"55": "Wald",
	"56": "Wald",
	"57": "Landwirtschaftlich genutzte Grundflächen",
	"58": "Wald",
	"59": "Gewässer",
	"60": "Gewässer",
	"61": "Gewässer",
	"62": "Sonstige",
	"63": "Sonstige",
	"64": "Gewässer",
	"65": "Sonstige",
	"72": "Sonstige",
	"83": "Bauflächen",
	"84": "Sonstige",
	"87": "Sonstige",
	"88": "Sonstige",
	"92": "Sonstige",
	"95": "Sonstige",
	"96": "Sonstige",
}

// nsNutzung is the BEV "Bezeichnung der Nutzung" — the specific use.
var nsNutzung = map[string]string{
	"40": "Dauerkulturanlagen oder Erwerbsgärten",
	"41": "Gebäude",
	"42": "Parkplätze",
	"48": "Äcker, Wiesen oder Weiden",
	"52": "Gärten",
	"53": "Weingärten",
	"54": "Alpen",
	"55": "Krummholzflächen",
	"56": "Wälder",
	"57": "Verbuschte Flächen",
	"58": "Forststraßen",
	"59": "Fließende Gewässer",
	"60": "Stehende Gewässer",
	"61": "Feuchtgebiete",
	"62": "Vegetationsarme Flächen",
	"63": "Betriebsflächen",
	"64": "Gewässerrandflächen",
	"65": "Verkehrsrandflächen",
	"72": "Friedhöfe",
	"83": "Gebäudenebenflächen",
	"84": "Abbauflächen, Halden und Deponien",
	"87": "Fels- und Geröllflächen",
	"88": "Gletscher",
	"92": "Schienenverkehrsanlagen",
	"95": "Straßenverkehrsanlagen",
	"96": "Freizeitflächen",
}

// nutzungssymbolCodes is the display label: "<Nutzung> - <abbr>". Kept in this
// shape because it is what the search index, the /query landuse endpoint and
// the stored landuse_summary keys already use.
var nutzungssymbolCodes = func() map[string]string {
	m := make(map[string]string, len(nsNutzung))
	for c, n := range nsNutzung {
		m[c] = n + " - " + nutzungAbbreviations[c]
	}
	return m
}()

// nutzungAbbreviations are short map-style labels derived from the Benützungsart
// (BEV publishes glyphs, not letter codes, in the VTC spec).
var nutzungAbbreviations = map[string]string{
	"40": "LN(Dk)", "41": "B(Geb)", "42": "So(Pp)", "48": "LN",
	"52": "GA", "53": "WG", "54": "Alpe", "55": "W(Kr)",
	"56": "W", "57": "LN(vb)", "58": "W(Fs)", "59": "GW(f)",
	"60": "GW(s)", "61": "GW(Fg)", "62": "So(vg)", "63": "So(Bf)",
	"64": "GW(Rf)", "65": "So(Vr)", "72": "So(Fh)", "83": "B(Nf)",
	"84": "So(Ab)", "87": "So(Fe)", "88": "So(Gl)", "92": "So(Bahn)",
	"95": "So(Str)", "96": "So(Fz)",
}

// MapNutzungssymbol maps a BEV NS code to its human-readable label.
func MapNutzungssymbol(code string) string {
	if desc, ok := nutzungssymbolCodes[code]; ok {
		return desc
	}
	return "Unbekannt - Code " + code
}

// MapNutzungAbbreviation returns the short map abbreviation.
func MapNutzungAbbreviation(code string) string {
	if abbr, ok := nutzungAbbreviations[code]; ok {
		return abbr
	}
	return code
}

// MapBenutzungsart returns the BEV top-level Benützungsart for a code.
func MapBenutzungsart(code string) string {
	if b, ok := nsBenutzungsart[code]; ok {
		return b
	}
	return "Unbekannt"
}

// GetAllNutzungssymbolCodes returns all known codes for API documentation.
func GetAllNutzungssymbolCodes() map[string]string {
	return nutzungssymbolCodes
}
