// api_license.go — Data licensing & attribution, in ONE place.
//
// Everything this API serves is derived from Open Government Data or open
// community data; each source carries an attribution (and in OSM's case a
// share-alike) obligation that our redistributors inherit. Rather than sprinkle
// license strings through handlers, this file holds the canonical texts and:
//
//   - GET /api/v1/license                machine-readable summary of every source
//   - Link: <cc-by-4.0>; rel="license"  + X-Data-Attribution on every /api/ response
//   - LicenseMeta() / AttributionMeta()  for handlers that put it in a response body
//     (Meta.License on the standard envelope, meta.license on exports)
//
// Licensing assessment (Sep 2026):
//
//	BEV Kataster (kataster.bev.gv.at tiles; parcels, footprints, address points,
//	landuse, labels) is Open Government Data under CC BY 4.0 — data.bev.gv.at
//	metadata: "Für dieses Produkt gilt die Standardlizenz CC-BY-4.0", and BEV
//	confirmed CC BY 4.0 for the tile service to the OSM community in 2023. CC BY
//	permits copying, transformation (segmentation, R-tree API, games) and
//	commercial use. Obligations: attribution, license link, indicate changes
//	(our data IS modified: re-projected to WGS84, tile-clipped and re-assembled,
//	simplified by z15/z16 quantisation, enriched), no implication of BEV
//	endorsement. BEV's copyright + sui generis database rights (§76c ff UrhG)
//	are exactly what the CC BY grant licenses (CC BY 4.0 §1(c) includes Sui
//	Generis Database Rights; §4 permits extraction/re-utilisation of substantial
//	parts and makes a derived database Adapted Material), so no separate
//	agreement is needed. HARD BOUNDARY: the grant is per product — it covers the
//	VTC tilesets at kataster.bev.gv.at/tiles/… only. The Katasterservice web app
//	and its search/info JSON API fall under the AGB + Standardentgelte (§2.3.10:
//	copies only for backup; §2.3.7: no Weitergabe), where §76c UrhG would bite
//	with no licence to answer it. Never ingest from kataster.bev.gv.at outside
//	/tiles/. Client caches: see api_cache_policy.go. Owner data is NOT in the open datasets and NOT in this API (no GDPR
//	issue); EZ numbers are public cadastre attributes.
//
//	BEV DLM – Geographische Namen (DLM 7000 NAMEN; /api/v1/toponyms/*, layers=
//	toponyms, /lookup?type=toponym) is Open Government Data under the same
//	CC BY 4.0 as the Kataster (data.bev.gv.at product page: "Standardlizenz
//	CC-BY-4.0"). It is a DIFFERENT product with its own Stichtag, so it gets its
//	own row in /api/v1/license and responses name it in meta.license.dataset.
//	Modified: re-projected EPSG:31255→WGS84, KG/Gemeinde assignment added,
//	names normalised/stemmed for search.
//
//	OSM (osm{} proximity block, /api/v1/osm/*, /search/address_osm) is ODbL, not
//	CC BY: attribute "© OpenStreetMap contributors"; a REDISTRIBUTED database
//	that merges OSM-derived fields becomes share-alike. Only API rows with an
//	osm{} block are mixed.
package main

import (
	"fmt"
	"html"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// Canonical strings — change here, everywhere follows.
const (
	BEVLicenseSPDX   = "CC-BY-4.0"
	BEVLicenseURL    = "https://creativecommons.org/licenses/by/4.0/"
	BEVAttributionDE = "Datenquelle: BEV – Bundesamt für Eich- und Vermessungswesen"
	BEVAttributionEN = "Data source: BEV – Austrian Federal Office of Metrology and Surveying"
	// BEVModifiedNote satisfies CC BY 4.0 §3(a)(1)(B): indicate modifications.
	BEVModifiedNoteDE = "bearbeitet (Daten geändert: reprojiziert nach WGS84, aus Kachel-Ausschnitten neu zusammengesetzt, vereinfacht, angereichert)"
	BEVModifiedNoteEN = "modified (re-projected to WGS84, re-assembled from tile clips, simplified, enriched)"
	BEVNoEndorsement  = "This service is not operated by, affiliated with, or endorsed by BEV."

	OSMLicenseSPDX = "ODbL-1.0"
	OSMLicenseURL  = "https://www.openstreetmap.org/copyright"
	OSMAttribution = "© OpenStreetMap contributors"

	// RecommendedAttribution is the one-liner clients should reproduce verbatim.
	RecommendedAttribution = BEVAttributionDE + ", CC BY 4.0 (" + BEVLicenseURL + "), bearbeitet"
)

// dataSourceLicense is one row of GET /api/v1/license.
type dataSourceLicense struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	License            string   `json:"license"`
	LicenseURL         string   `json:"license_url"`
	Attribution        string   `json:"attribution"`
	AttributionDE      string   `json:"attribution_de,omitempty"`
	Modified           bool     `json:"modified"`
	ModifiedNote       string   `json:"modified_note,omitempty"`
	SourceURL          string   `json:"source_url,omitempty"`
	SourceDate         string   `json:"source_date,omitempty"`
	ShareAlike         bool     `json:"share_alike"`
	RedistributionNote string   `json:"redistribution_note,omitempty"`
	Fields             []string `json:"fields,omitempty"`
	Endpoints          []string `json:"endpoints,omitempty"`
	NoEndorsement      string   `json:"no_endorsement,omitempty"`
}

// dataSourceLicenses lists every upstream dataset this API redistributes.
func dataSourceLicenses() []dataSourceLicense {
	out := dataSourceLicenses0()
	for i := range out {
		peerTextFields(&out[i])
	}
	return out
}

// peerTextFields — peerText over every string field of a struct (pointer).
func peerTextFields(p interface{}) {
	v := reflect.ValueOf(p).Elem()
	for i := 0; i < v.NumField(); i++ {
		if f := v.Field(i); f.Kind() == reflect.String && f.CanSet() {
			f.SetString(peerText(f.String()))
		}
	}
}

func dataSourceLicenses0() []dataSourceLicense {
	osmDate := ""
	if s := osmImportedAt(); s != "" {
		osmDate = s
	}
	return []dataSourceLicense{
		{
			ID: "ne", Name: "NE cells — declared land-use statistics per H3 cell (derived from BEV Kataster) + observed layer ({OBS_NAME})",
			License: BEVLicenseSPDX, LicenseURL: BEVLicenseURL,
			Attribution: "© BEV (CC BY 4.0), bearbeitet · observed layer © {OBS_NAME} (CC BY 4.0)", AttributionDE: BEVAttributionDE + ", Kataster, CC BY 4.0, bearbeitet (Nutzungseinheit-Zellen, keine Objektgeometrie) · beobachtete Schicht © {OBS_NAME} (CC BY 4.0)",
			Modified: true, ModifiedNote: "per-H3-cell aggregates (res 12 / res 10) computed by the pinned reference package ne_cells/ (docs/ne-cells.md); no object id, number or geometry is contained or recoverable; public K percentiles log-bucketed",
			SourceURL:          "https://kataster.bev.gv.at/",
			SourceDate:         "per KG: lu.epoch in /api/v1/ne/manifest",
			RedistributionNote: "Redistribute under CC BY 4.0 with this attribution. The observed layer is posted by {OBS_NAME} under the same licence.",
			Fields:             []string{"LU rows (30 B, res 12)", "K rows (29 B, res 10)", "OBS rows (peer-defined)"},
			Endpoints:          []string{"/api/v1/ne", "/api/v1/ne/*", "/api/v1/context?include=ne"},
			NoEndorsement:      BEVNoEndorsement,
		},
		{
			ID: "kg_register", Name: "Katastralgemeinden register — codes, names, Gemeinde/Bezirk/Bundesland, bbox (derived from BEV Kataster)",
			License: BEVLicenseSPDX, LicenseURL: BEVLicenseURL,
			Attribution: BEVAttributionEN + " – Katastralgemeinden (register keys only)", AttributionDE: BEVAttributionDE + " – Katastralgemeinden (nur Schlüssel)",
			Modified: true, ModifiedNote: "KG code/name per BEV tiles; Gemeinde/Bezirk/Bundesland joined from Statistik Austria; bbox = KG extent (VGD envelope), no object data",
			SourceURL:          "https://kataster.bev.gv.at/",
			SourceDate:         "generated_at on /api/v1/kgs",
			RedistributionNote: "Redistribute under CC BY 4.0 with this attribution. kg_count=7850 and universe_hash are the contract shared with {OBS_NAME}.",
			Fields:             []string{"kg_code", "kg_name", "gemeinde_code", "gemeinde_name", "district_code", "district_name", "state_code", "state_name", "bbox", "centroid"},
			Endpoints:          []string{"/api/v1/kgs"},
			NoEndorsement:      BEVNoEndorsement,
		},
		{
			ID: "bev_dlm_namen", Name: "BEV DLM – Geographische Namen (Digitales Landschaftsmodell, DLM 7000 NAMEN)",
			License: BEVLicenseSPDX, LicenseURL: BEVLicenseURL,
			Attribution: BEVAttributionDE + " – Geographische Namen (DLM)", AttributionDE: BEVAttributionDE + " – Digitales Landschaftsmodell, Geographische Namen",
			Modified: true, ModifiedNote: "re-projected EPSG:31255 (MGI/GK M31) → WGS84; KG/Gemeinde/Bezirk assigned (Ried: BEV KG_NUMMER; others: NE declared cell set or KG bbox); names normalised and generic suffixes stemmed for search; ÖK-sheet currency year joined",
			SourceURL:          toponymSourceURL(),
			SourceDate:         "Stichtag " + toponymStichtag() + " (see /api/v1/toponyms/stats)",
			RedistributionNote: "Redistribute under CC BY 4.0 with the attribution above, the license link and a note that the data is modified. Positional accuracy per BEV ±50 m — toponyms are not parcel-precise and are never snapped to parcels.",
			Fields:             []string{"toponyms rows (id=GLOBALID, name, variant_name, name_kroat/slow/ung, f_code, f_name, objektart, elevation_m, okz, source_indikator, currency_year, lon/lat)"},
			Endpoints:          []string{"/api/v1/toponyms/*", "/api/v1/kg/{kg}/toponyms", "layers=toponyms on /api/v1/spatial/bbox, /spatial/point, /export/geojson, /export/gpkg", "/api/v1/lookup?type=toponym (and 0-row fall-through)", "/api/v1/search/text 0-row fall-through"},
			NoEndorsement:      BEVNoEndorsement,
		},
		{
			ID: "openstreetmap", Name: "OpenStreetMap (Geofabrik Austria extract; Nominatim geocoding)",
			License: OSMLicenseSPDX, LicenseURL: OSMLicenseURL,
			Attribution: OSMAttribution,
			Modified:    true, ModifiedNote: "derived: distances/remoteness computed per parcel; lines re-chunked",
			SourceURL: "https://download.geofabrik.de/europe/austria.html", SourceDate: osmDate,
			ShareAlike:         true,
			RedistributionNote: "ODbL, not CC BY. The osm{} block and /api/v1/osm/* geometries are OSM-derived; a redistributed DATABASE that merges them becomes share-alike (ODbL §4.4). Keep OSM-derived fields in separate files or license that database ODbL.",
			Fields:             []string{"osm{}", "remoteness", "dist_*_m", "major_road_*", "road_fclass"},
			Endpoints:          []string{"/api/v1/osm/*", "/api/v1/search/address_osm", "osm{} on /api/v1/context"},
		},
		{
			ID: "statistik_austria", Name: "Statistik Austria (Gemeinden; Baugrundstücks-, Häuser-, Boden-/Pachtpreise)",
			License: BEVLicenseSPDX, LicenseURL: BEVLicenseURL,
			Attribution: "Datenquelle: STATISTIK AUSTRIA",
			Modified:    true, ModifiedNote: "aggregated and calibrated into per-parcel price models (land_prices); Gemeinde polygons dissolved to Bezirke and Bundesländer and simplified for the landing glyph's find box (/ne/glyph/admin)",
			SourceURL: "https://www.statistik.at",
			Fields:    []string{"gemeinde_code", "land_prices/*", "sa_bauland_eur_per_sqm", "ne/glyph/admin outlines"},
			Endpoints: []string{"/api/v1/search/municipalities", "/api/v1/land_prices/*", "/api/v1/lookup", "/api/v1/ne/glyph/admin", "/api/v1/ne/glyph/admin/outline"},
		},
		{
			// BEV Adressregister — a different BEV product from the Kataster, its own
			// licence page and its own mandatory notice form ("© Österreichisches
			// Adressregister, Stichtagsdaten vom TT.MM.JJJJ"). Open data under
			// CC BY 4.0 (bev.gv.at → Unentgeltliche Produkte → Adressregister; data.bev.gv.at
			// metadata "Für dieses Produkt gilt die Standardlizenz CC-BY-4.0"). Checked
			// 2026-10-29 before ingesting. NOT the Katasterservice search API (inv. 14).
			ID: "bev_adressregister", Name: "BEV Adressregister – Adresse Relationale Tabellen, Stichtagsdaten",
			License: BEVLicenseSPDX, LicenseURL: BEVLicenseURL,
			Attribution: addressesAttribution(), AttributionDE: addressesAttribution(),
			Modified: true, ModifiedNote: "re-projected MGI/GK M28/M31/M34 → WGS84; streets indexed for prefix search; one street centroid per street key; house numbers as printed (HNR_ADR_ZUSAMMEN); Gebäude, Grundstücks- and Zählsprengel tables not loaded",
			SourceURL:          addressesSourceURL,
			SourceDate:         "Stichtag " + addressesStichtag(),
			RedistributionNote: "Redistribute under CC BY 4.0 with the notice above (the Stichtag is part of it). Address points are the register's geocoding, not parcel geometry — nothing here identifies a parcel.",
			Fields:             []string{"addresses/search results (street, hnr, hof, plz, ort, gemeinde, gkz, lon/lat)"},
			Endpoints:          []string{"/api/v1/addresses/search"},
			NoEndorsement:      BEVNoEndorsement,
		},
		{
			ID: "eea_natura2000", Name: "Natura 2000 sites (European Environment Agency)",
			License: "EEA standard re-use policy (attribution)", LicenseURL: "https://www.eea.europa.eu/en/legal-notice",
			Attribution: "Source: European Environment Agency (EEA), Natura 2000 data",
			Modified:    true, ModifiedNote: "parcel membership computed by centroid point-in-polygon",
			SourceURL: "https://www.eea.europa.eu/en/datahub/datahubitem-view/6fc8ad2d-195d-40f4-bdec-576e7d1268e4",
			Endpoints: []string{"/api/v1/natura2000/*", "natura2000_* fields on parcel/KG rows", "/api/v1/ne/glyph/admin (n2k rows) · /api/v1/ne/glyph/admin/outline?c=AT…"},
		},
		{
			ID: "ris", Name: "RIS — Rechtsinformationssystem des Bundes (Landesrecht)",
			License: BEVLicenseSPDX, LicenseURL: BEVLicenseURL,
			Attribution: "Datenquelle: RIS, Bundeskanzleramt Österreich",
			Modified:    true, ModifiedNote: "parcel references extracted from law texts",
			SourceURL: "https://data.bka.gv.at/ris/",
			Endpoints: []string{"/api/v1/legal/*"},
		},
		{
			ID: "wdpa", Name: "World Database on Protected Areas (UNEP-WCMC / IUCN)",
			License: "WDPA terms (non-commercial, attribution)", LicenseURL: "https://www.protectedplanet.net/en/legal",
			Attribution: "UNEP-WCMC and IUCN, Protected Planet: The World Database on Protected Areas (WDPA)",
			Modified:    false, SourceURL: "https://www.protectedplanet.net",
			Endpoints: []string{"/api/v1/search/protected_area", "/api/v1/query/protected_area"},
		},
		{
			ID: "invekos", Name: "INVEKOS Schläge — subsidy-claimed area per H3 cell (AMA / BML, aggregated by {SUB_NAME})",
			License: BEVLicenseSPDX, LicenseURL: BEVLicenseURL,
			Attribution: neSubAttribution, AttributionDE: "Datenquelle: Agrarmarkt Austria (AMA), INVEKOS Schläge, CC BY 4.0, bearbeitet · je H3-Zelle von {SUB_NAME}",
			Modified: true, ModifiedNote: "share of each H3 res-12 / res-7 cell covered by INVEKOS Schläge of one year, dominant class, bio share, outline reliability and lineage vs the previous year — computed by the peer; umfeld-at paints pictures and sums only, keeps nothing on disk and never re-serves the per-cell columns",
			SourceURL:          "{SUB_API}/api/v1/invekos/docs/llm.txt",
			SourceDate:         "release in X-Invekos-Release / sub.release of the glyph responses (2025-2 at the time of writing)",
			RedistributionNote: "Redistribute under CC BY 4.0 with the AMA attribution and the peer named; the peer's own /api/v1/invekos/license applies to what it serves.",
			Fields:             []string{"claimed share per cell (v=sub tiles)", "lineage strokes (v=subc tiles)", "res-7 cov/cls/bio/dcov (/ne/glyph/sub)", "sub{} of /ne/glyph/cell?sub=1"},
			Endpoints:          []string{"/api/v1/ne/glyph/sub", "/api/v1/ne/glyph/tile?v=sub|subc", "/api/v1/ne/glyph/cell?sub=1"},
			NoEndorsement:      "Not operated by or affiliated with AMA, BML or {SUB_NAME}.",
		},
		{
			ID: "basemap_at", Name: "basemap.at Orthofoto — aerial image WMTS (geoland.at / Stadt Wien), the photo layer of the landing map",
			License: "CC-BY-4.0", LicenseURL: "https://creativecommons.org/licenses/by/4.0/",
			Attribution: "Datenquelle: basemap.at", AttributionDE: "Datenquelle: basemap.at",
			Modified: false, ModifiedNote: "not served, not stored, not proxied: the reader's browser fetches the tiles from mapsneu.wien.gv.at (bmaporthofoto30cm, google3857) only while the photo layer is on; this server paints nothing from them",
			SourceURL:          "https://basemap.at/",
			SourceDate:         "as published by basemap.at (WMTS capabilities https://mapsneu.wien.gv.at/basemap/1.0.0/WMTSCapabilities.xml)",
			RedistributionNote: "A screenshot or export of the map with the photo layer on carries basemap.at's image: credit 'Datenquelle: basemap.at' (CC BY 4.0) next to ours.",
			Fields:             []string{"jpeg tiles, 256 px, z 12–19 (browser-side only)"},
			Endpoints:          []string{"/ (landing map, ;ortho)"},
			NoEndorsement:      "Not operated by or affiliated with basemap.at, geoland.at or the Stadt Wien.",
		},
		{
			ID: "gadm", Name: "GADM v4.1 municipality boundaries (legacy)",
			License: "GADM license (free for academic/non-commercial use)", LicenseURL: "https://gadm.org/license.html",
			Attribution: "GADM (https://gadm.org)", Modified: false,
			Endpoints: []string{"/api/v1/search/gadm"},
		},
	}
}

// LicenseMeta is the compact block embedded in response bodies.
type LicenseMeta struct {
	License     string `json:"license"`
	LicenseURL  string `json:"license_url"`
	Attribution string `json:"attribution"`
	Modified    bool   `json:"modified"`
	OSMNote     string `json:"osm_note,omitempty"`
	// Dataset names the specific BEV product when a response is not the
	// Kataster (e.g. DLM-Namen toponyms) so re-users can cite it precisely.
	Dataset string `json:"dataset,omitempty"`
	Details string `json:"details"`
}

// AttributionMeta returns the block for a response of BEV-derived data.
// withOSM adds the ODbL notice when rows carry an osm{} block.
func AttributionMeta(withOSM bool) *LicenseMeta {
	m := &LicenseMeta{
		License:     BEVLicenseSPDX,
		LicenseURL:  BEVLicenseURL,
		Attribution: RecommendedAttribution,
		Modified:    true,
		Details:     "/api/v1/license",
	}
	if withOSM {
		m.OSMNote = "osm{} fields: " + OSMAttribution + ", " + OSMLicenseSPDX + " (share-alike if redistributed as a database)"
	}
	return m
}

// AttributionMap is AttributionMeta for handlers that build map bodies.
func AttributionMap(withOSM bool) map[string]interface{} {
	m := AttributionMeta(withOSM)
	out := map[string]interface{}{
		"license":     m.License,
		"license_url": m.LicenseURL,
		"attribution": m.Attribution,
		"modified":    m.Modified,
		"details":     m.Details,
	}
	if m.OSMNote != "" {
		out["osm_note"] = m.OSMNote
	}
	return out
}

// OSMAttributionMap is the block for responses that are ENTIRELY OSM-derived.
func OSMAttributionMap() map[string]interface{} {
	return map[string]interface{}{
		"license":     OSMLicenseSPDX,
		"license_url": OSMLicenseURL,
		"attribution": OSMAttribution,
		"share_alike": true,
		"details":     "/api/v1/license",
	}
}

// setLicenseHeaders adds the RFC 8288 license link and a short attribution
// header to every /api/ response. Headers survive gzip and are cheap, so a
// client that only looks at the wire still sees the obligation.
func setLicenseHeaders(w http.ResponseWriter, path string) {
	if !strings.HasPrefix(path, "/api/") {
		return
	}
	h := w.Header()
	switch id := sourceIDForPath(path); id {
	case "openstreetmap":
		h.Add("Link", "<"+OSMLicenseURL+">; rel=\"license\"; title=\""+OSMLicenseSPDX+"\"")
		h.Set("X-Data-Attribution", "(c) OpenStreetMap contributors ("+OSMLicenseSPDX+")")
	case "bev_dlm_namen":
		h.Add("Link", "<"+BEVLicenseURL+">; rel=\"license\"; title=\""+BEVLicenseSPDX+"\"")
		h.Set("X-Data-Attribution", "(c) BEV, "+strconv.Itoa(bevDataYear())+" - Datenquelle: BEV - Bundesamt fuer Eich- und Vermessungswesen - Geographische Namen (DLM), CC BY 4.0, bearbeitet/modified")
	case "bev_adressregister":
		h.Add("Link", "<"+BEVLicenseURL+">; rel=\"license\"; title=\""+BEVLicenseSPDX+"\"")
		h.Set("X-Data-Attribution", asciiFold(addressesAttribution())+", CC BY 4.0")
	case "kg_register":
		h.Add("Link", "<"+BEVLicenseURL+">; rel=\"license\"; title=\""+BEVLicenseSPDX+"\"")
		h.Set("X-Data-Attribution", BEVNoticeASCII())
	case "mixed":
		h.Add("Link", "</api/v1/license>; rel=\"license\"")
		h.Set("X-Data-Attribution", "per block - Statistik Austria / EEA Natura 2000 / WDPA / BEV DLM-Namen CC BY 4.0 / RIS; osm{} (c) OpenStreetMap contributors ODbL - see /api/v1/license")
	default:
		if s := sourceByID(id); s != nil {
			h.Add("Link", "<"+s.LicenseURL+">; rel=\"license\"")
			h.Set("X-Data-Attribution", asciiFold(s.Attribution)+" ("+asciiFold(s.License)+")")
		}
	}
}

// asciiFold keeps header values ASCII-safe (RFC 7230 field-value).
func asciiFold(s string) string {
	r := strings.NewReplacer("ä", "ae", "ö", "oe", "ü", "ue", "Ä", "Ae", "Ö", "Oe", "Ü", "Ue", "ß", "ss", "©", "(c)", "–", "-", "—", "-")
	s = r.Replace(s)
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] < 0x80 && s[i] >= 0x20 {
			out = append(out, s[i])
		}
	}
	return string(out)
}

// toponymStichtag / toponymSourceURL read the loaded DLM-Namen metadata for
// the license row ("" / default URL when the layer is not loaded).
func toponymStichtag() string {
	_, _, meta, ready := toponymsSnapshot()
	if !ready {
		return "not loaded"
	}
	return meta["stichtag"]
}

func toponymSourceURL() string {
	_, _, meta, ready := toponymsSnapshot()
	if ready && meta["source_url"] != "" {
		return meta["source_url"]
	}
	return toponymsDefaultURL
}

// handleV1License serves GET /api/v1/license.
func handleV1License(w http.ResponseWriter, r *http.Request) {
	doc := licenseDoc(r)
	if wantsLicenseHTML(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=600")
		_, _ = w.Write([]byte(peerText(licenseHTML(doc))))
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// wantsLicenseHTML — browsers (Accept lists text/html before json) and
// ?format=html get the page; everything else keeps the JSON contract.
func wantsLicenseHTML(r *http.Request) bool {
	switch strings.ToLower(r.URL.Query().Get("format")) {
	case "html":
		return true
	case "json":
		return false
	}
	a := r.Header.Get("Accept")
	h, j := strings.Index(a, "text/html"), strings.Index(a, "application/json")
	return h >= 0 && (j < 0 || h < j)
}

func licenseDoc(r *http.Request) map[string]interface{} {
	return peerTextDoc(licenseDoc0(r)).(map[string]interface{})
}

func licenseDoc0(r *http.Request) map[string]interface{} {
	if !isPrivateTier(r) {
		return publicLicense()
	}
	sources := dataSourceLicenses()
	return map[string]interface{}{
		"primary": map[string]interface{}{
			"license":          BEVLicenseSPDX,
			"license_url":      BEVLicenseURL,
			"attribution":      RecommendedAttribution,
			"notice":           BEVNotice(),
			"data_year":        bevDataYear(),
			"attribution_de":   BEVAttributionDE + " – " + BEVModifiedNoteDE,
			"attribution_en":   BEVAttributionEN + ", CC BY 4.0, " + BEVModifiedNoteEN,
			"attribution_html": `<a href="https://www.bev.gv.at">Datenquelle: BEV – Bundesamt für Eich- und Vermessungswesen</a>, <a href="` + BEVLicenseURL + `">CC BY 4.0</a>, bearbeitet`,
			"modified":         true,
			"no_endorsement":   BEVNoEndorsement,
		},
		"obligations": []string{
			"Attribution with year on every copy, also cached/processed ones (BEV Nutzungsbedingungen §2.3.3): " + BEVNotice(),
			"License link: " + BEVLicenseURL,
			"Indicate changes: state 'bearbeitet' / 'modified' — this data is re-projected, tile-clipped, re-assembled, simplified and enriched",
			"Do not imply BEV endorsement",
			"Toponyms (/api/v1/toponyms/*, layers=toponyms) are BEV DLM – Geographische Namen, also CC BY 4.0: attribute '" + BEVAttributionDE + " – Geographische Namen (DLM)' with the Stichtag from /api/v1/toponyms/stats",
			"If you redistribute rows containing the osm{} block: attribute '" + OSMAttribution + "' and observe ODbL share-alike for that database",
			"NE cells (/api/v1/ne/*): '" + NENotice() + "' — per-cell statistics; the observed rows are {OBS_NAME}'s, credit them too",
		},
		"stage":            serviceStage + " — " + serviceStageNote,
		"siblings":         siblingServices(),
		"personal_data":    "Owner (Eigentümer) data is not part of the BEV open datasets and is not served by this API. EZ numbers are public cadastre attributes.",
		"sources":          sources,
		"response_headers": []string{"Link: <...>; rel=\"license\"", "X-Data-Attribution"},
		"caching":          "/api/v1/cache/policy (rules for client-side caches + reference clients)",
		"code_license":     "MIT",
		"code_repository":  "private (source not published); public components: https://github.com/raffopenssh/vtcseamless (bevdirect), https://github.com/raffopenssh/vtcseamless-py (ne_cells reference)",
		"operator":         "umfeld-at — Impressum: /impressum · Datenschutz: /datenschutz",
		"generated_at":     time.Now().UTC().Format(time.RFC3339),
	}
}

// licenseHTML — the licence document rendered in the mono docs style
// (monoDocsCSS): one table row per source, the obligations, siblings.
func licenseHTML(doc map[string]interface{}) string {
	esc := html.EscapeString
	var b strings.Builder
	b.WriteString(`<!doctype html><html lang="en"><head><meta charset="utf-8"><title>umfeld-at — licences &amp; attribution</title>` +
		`<meta name="viewport" content="width=device-width,initial-scale=1"><style>` + monoDocsCSS + `</style></head><body>`)
	b.WriteString(`<h1>umfeld-at — licences &amp; attribution<span class="stage" title="` + esc(serviceStage+" — "+serviceStageNote) + `">` + esc(serviceStage) + `</span></h1>`)
	if sum, _ := doc["summary"].(string); sum != "" {
		b.WriteString("<p>" + esc(sum) + "</p>")
	}
	if pr, ok := doc["primary"].(map[string]interface{}); ok {
		b.WriteString("<h2>Primary source</h2><pre>" + esc(fmt.Sprint(pr["notice"])) + "\n" + esc(fmt.Sprint(pr["attribution_en"])) + "</pre>")
		b.WriteString("<p>" + esc(fmt.Sprint(pr["no_endorsement"])) + "</p>")
	}
	b.WriteString("<h2>Sources</h2><table><tr><th>Source</th><th>Licence · attribution · where</th></tr>")
	if srcs, ok := doc["sources"].([]dataSourceLicense); ok {
		for _, s := range srcs {
			b.WriteString("<tr><td>" + esc(s.Name) + "</td><td>")
			if s.LicenseURL != "" {
				b.WriteString(`<a href="` + esc(s.LicenseURL) + `">` + esc(s.License) + "</a>")
			} else {
				b.WriteString(esc(s.License))
			}
			if s.ShareAlike {
				b.WriteString(" (share-alike)")
			}
			b.WriteString("<br><code>" + esc(s.Attribution) + "</code>")
			if s.Modified && s.ModifiedNote != "" {
				b.WriteString("<br>modified: " + esc(s.ModifiedNote))
			}
			if s.SourceDate != "" {
				b.WriteString("<br>date: " + esc(s.SourceDate))
			}
			if s.RedistributionNote != "" {
				b.WriteString("<br>" + esc(s.RedistributionNote))
			}
			if len(s.Endpoints) > 0 {
				eps := make([]string, len(s.Endpoints))
				for i, e := range s.Endpoints {
					eps[i] = "<code>" + esc(e) + "</code>"
				}
				b.WriteString("<br>where: " + strings.Join(eps, " · "))
			}
			if s.SourceURL != "" {
				b.WriteString(`<br><a href="` + esc(s.SourceURL) + `">` + esc(s.SourceURL) + "</a>")
			}
			b.WriteString("</td></tr>")
		}
	}
	b.WriteString("</table>")
	for _, key := range []string{"obligations", "per_source_obligations"} {
		if obl, ok := doc[key].([]string); ok && len(obl) > 0 {
			b.WriteString("<h2>Obligations</h2><pre>")
			for _, o := range obl {
				b.WriteString("- " + esc(o) + "\n")
			}
			b.WriteString("</pre>")
		}
	}
	if sib, ok := doc["siblings"].([]map[string]string); ok && len(sib) > 0 {
		b.WriteString("<h2>Sibling services</h2><table><tr><th>Service</th><th>Serves · licence</th></tr>")
		for _, x := range sib {
			b.WriteString(`<tr><td><a href="` + esc(x["url"]) + `">` + esc(x["id"]) + "</a></td><td>" + esc(x["serves"]) +
				`<br><code>` + esc(x["attribution"]) + `</code><br><a href="` + esc(x["license"]) + `">` + esc(x["license"]) + "</a></td></tr>")
		}
		b.WriteString("</table>")
	}
	for _, kv := range [][2]string{{"not_served", "Not served"}, {"personal_data", "Personal data"}, {"caching", "Caching"}} {
		if v, _ := doc[kv[0]].(string); v != "" {
			b.WriteString("<p><strong>" + kv[1] + ".</strong> " + esc(v) + "</p>")
		}
	}
	b.WriteString(`<p class="muted">Every <code>/api/</code> response carries <code>Link: &lt;…&gt;; rel="license"</code> and <code>X-Data-Attribution</code> headers and a <code>meta.license</code> block naming its own source. ` +
		`Machine-readable: <a href="/api/v1/license?format=json">/api/v1/license?format=json</a> · caching rules: <a href="/api/v1/cache/policy">/api/v1/cache/policy</a> · back to <a href="/">overview</a>. Code: MIT. Stage: ` + esc(serviceStage) + ` — ` + esc(serviceStageNote) + `.</p></body></html>`)
	return b.String()
}

// osmImportedAt returns the Geofabrik import stamp for /license, or "".
func osmImportedAt() string {
	if !OSMProximityReady() {
		return ""
	}
	var s string
	if osmDB == nil {
		return ""
	}
	_ = osmDB.QueryRow("SELECT value FROM osm_meta WHERE key='imported_at'").Scan(&s)
	return s
}

// ── Per-source attribution (public tier) ─────────────────────────────────────
//
// The public tier (api_tiers.go) serves nothing from the BEV Kataster, so the
// default "© BEV … Kataster" attribution would be WRONG on most public
// responses. sourceIDForPath maps a request path to the dataset that answers
// it; AttributionMetaFor / setLicenseHeaders / handleV1License use it so each
// response names its own source. Unknown paths fall back to the Kataster
// (private tier only).

func sourceIDForPath(p string) string {
	switch {
	case strings.HasPrefix(p, "/api/v1/osm/"), strings.HasPrefix(p, "/api/v1/search/address_osm"):
		return "openstreetmap"
	case strings.HasPrefix(p, "/api/v1/toponyms"), strings.HasPrefix(p, "/api/v1/kg/") && strings.HasSuffix(p, "/toponyms"):
		return "bev_dlm_namen"
	case strings.HasPrefix(p, "/api/v1/addresses/"):
		return "bev_adressregister"
	case strings.HasPrefix(p, "/api/v1/ne/glyph/admin"):
		return "statistik_austria"
	case strings.HasPrefix(p, "/api/v1/natura2000/"):
		return "eea_natura2000"
	case strings.HasPrefix(p, "/api/v1/legal/"):
		return "ris"
	case strings.HasPrefix(p, "/api/v1/land_prices/"), p == "/api/v1/search/municipalities", p == "/api/v1/lookup":
		return "statistik_austria"
	case p == "/api/v1/kgs":
		return "kg_register"
	case p == "/api/v1/search/protected_area", p == "/api/v1/query/protected_area":
		return "wdpa"
	case p == "/api/v1/search/gadm":
		return "gadm"
	case p == "/api/v1/context":
		return "mixed"
	case p == "/api/v1/ne/glyph/sub":
		return "invekos"
	case p == "/api/v1/ne", strings.HasPrefix(p, "/api/v1/ne/"), p == "/api/v1/docs/ne.md", p == "/api/v1/docs/ne-change.md":
		return "ne"
	}
	return "kg_register"
}

func sourceByID(id string) *dataSourceLicense {
	for _, s := range dataSourceLicenses() {
		if s.ID == id {
			s := s
			return &s
		}
	}
	return nil
}

// AttributionMetaFor is AttributionMeta keyed by the answering dataset.
func AttributionMetaFor(path string, withOSM bool) *LicenseMeta {
	id := sourceIDForPath(path)
	switch id {
	case "kg_register":
		return AttributionMeta(withOSM)
	case "mixed":
		m := &LicenseMeta{
			License:     "per block — see /api/v1/license",
			LicenseURL:  "/api/v1/license",
			Attribution: "Statistik Austria · EEA Natura 2000 · UNEP-WCMC/IUCN WDPA · BEV DLM-Namen (CC BY 4.0) · RIS; osm{} © OpenStreetMap contributors (ODbL)",
			Modified:    true,
			Dataset:     "context bundle",
			Details:     "/api/v1/license",
		}
		if withOSM {
			m.OSMNote = "osm{} block: " + OSMAttribution + ", " + OSMLicenseSPDX + " (share-alike if redistributed as a database)"
		}
		return m
	}
	s := sourceByID(id)
	if s == nil {
		return AttributionMeta(withOSM)
	}
	m := &LicenseMeta{
		License:     s.License,
		LicenseURL:  s.LicenseURL,
		Attribution: s.Attribution,
		Modified:    s.Modified,
		Dataset:     s.Name,
		Details:     "/api/v1/license",
	}
	if s.ShareAlike {
		m.OSMNote = OSMAttribution + ", " + OSMLicenseSPDX + " — share-alike if redistributed as a database"
	}
	return m
}

// AttributionMapFor — map form of AttributionMetaFor.
func AttributionMapFor(path string, withOSM bool) map[string]interface{} {
	m := AttributionMetaFor(path, withOSM)
	out := map[string]interface{}{
		"license":     m.License,
		"license_url": m.LicenseURL,
		"attribution": m.Attribution,
		"modified":    m.Modified,
		"details":     m.Details,
	}
	if m.Dataset != "" {
		out["dataset"] = m.Dataset
	}
	if m.OSMNote != "" {
		out["osm_note"] = m.OSMNote
	}
	return out
}

// publicSourceIDs — datasets the public tier redistributes.
var publicSourceIDs = []string{"ne", "kg_register", "invekos", "basemap_at", "statistik_austria", "bev_adressregister", "eea_natura2000", "wdpa", "openstreetmap", "bev_dlm_namen", "ris", "gadm"}

// siblingServices — services that share ids/vocabulary with this one; their
// own /api/v1/license applies to what THEY serve. Listed so re-users joining
// the two layers see both notices in one place.
var siblingServicesTpl = []map[string]string{{
	"id": "{OBS_NAME}", "url": "{OBS_API}/",
	"serves":      "observed layer per H3 cell (LiDAR heights, tree apices, structures, NDVI, forest loss, terrain), same cell ids as /api/v1/ne",
	"license":     "{OBS_API}/api/v1/license",
	"attribution": "Datenquelle: BEV – ALS DTM/DSM & DOP (CC BY 4.0, bearbeitet) · Copernicus Sentinel data (ESA) · ESA WorldCover · Hansen GFC · segmentation © {OBS_NAME} (CC BY 4.0)",
}, {
	"id": "{SUB_NAME}", "url": "{SUB_API}/",
	"serves":      "INVEKOS Schläge (AMA subsidy claims) per H3 cell — claimed share, class, bio, outline reliability, lineage vs the previous year; the glyph's subsidy layer (/api/v1/ne/glyph/sub, tile?v=sub)",
	"license":     "{SUB_API}/api/v1/invekos/license",
	"attribution": "Datenquelle: Agrarmarkt Austria (AMA), INVEKOS Schläge <release>, CC BY 4.0, bearbeitet · per H3 cell by {SUB_NAME}",
}}

// siblingServices — siblingServicesTpl with the configured peer names/URLs.
func siblingServices() []map[string]string {
	out := make([]map[string]string, 0, len(siblingServicesTpl))
	for _, m := range siblingServicesTpl {
		c := make(map[string]string, len(m))
		for k, v := range m {
			c[k] = peerText(v)
		}
		out = append(out, c)
	}
	return out
}

// publicLicense — GET /api/v1/license on the public tier.
// peerTextDoc — peerText over every string in a nested JSON-ish document.
func peerTextDoc(v interface{}) interface{} {
	switch t := v.(type) {
	case string:
		return peerText(t)
	case []string:
		out := make([]string, len(t))
		for i, x := range t {
			out[i] = peerText(x)
		}
		return out
	case []interface{}:
		for i := range t {
			t[i] = peerTextDoc(t[i])
		}
		return t
	case map[string]interface{}:
		for k := range t {
			t[k] = peerTextDoc(t[k])
		}
		return t
	case map[string]string:
		for k := range t {
			t[k] = peerText(t[k])
		}
		return t
	}
	return v
}

func publicLicense() map[string]interface{} {
	all := dataSourceLicenses()
	sources := make([]dataSourceLicense, 0, len(publicSourceIDs))
	for _, id := range publicSourceIDs {
		for _, s := range all {
			if s.ID != id {
				continue
			}
			// Keep only endpoints that exist on the public tier.
			var eps []string
			for _, e := range s.Endpoints {
				base := strings.TrimSuffix(e, "*")
				if isPublicPath(strings.TrimSuffix(base, "/")) || isPublicPath(base) || isPublicPath(base+"x") {
					eps = append(eps, e)
				}
			}
			if id == "statistik_austria" {
				eps = []string{"/api/v1/search/municipalities", "/api/v1/land_prices/{gemeinde,bezirk,bundesland}/*", "/api/v1/land_prices/predict", "/api/v1/land_prices/point", "/api/v1/lookup", "/api/v1/context (municipality, land_price)"}
				s.ModifiedNote = "aggregated and calibrated into a price model; the estimate uses caller-supplied parcel features"
			}
			if id == "eea_natura2000" {
				eps = []string{"/api/v1/natura2000/point", "/api/v1/natura2000/site/*", "/api/v1/natura2000/search", "/api/v1/context (natura2000)"}
				s.ModifiedNote = "re-projected; point-in-polygon served per request"
			}
			if id == "openstreetmap" {
				eps = []string{"/api/v1/osm/point", "/api/v1/osm/geometry", "/api/v1/search/address_osm", "/api/v1/context (osm)"}
				s.Fields = []string{"osm{}", "remoteness", "dist_*_m", "major_road_*", "road_fclass"}
				s.ModifiedNote = "derived: distances/remoteness computed for the requested point or geometry"
			}
			if id == "ris" {
				eps = []string{"/api/v1/legal/kg/{kg}", "/api/v1/legal/search", "/api/v1/legal/stats", "/api/v1/context?kg= (legal)"}
			}
			if id == "wdpa" {
				eps = []string{"/api/v1/search/protected_area", "/api/v1/context (protected_area)"}
			}
			if id == "bev_dlm_namen" {
				eps = []string{"/api/v1/toponyms/*", "/api/v1/kg/{kg}/toponyms", "/api/v1/lookup?type=toponym", "/api/v1/context (toponyms)"}
			}
			if id == "ne" {
				eps = []string{"/api/v1/ne", "/api/v1/ne/manifest", "/api/v1/ne/{kg}", "/api/v1/ne/chunk/{h3_res9}", "/api/v1/ne/cell/{h3}", "/api/v1/ne/cells", "/api/v1/context (ne)"}
				s.Name = "NE cells — declared land-use statistics per H3 cell (derived from BEV survey data) + observed layer ({OBS_NAME})"
				s.SourceDate = "per KG: lu.epoch in /api/v1/ne/manifest; coverage " + neCoverageText()
			}
			s.Endpoints = eps
			sources = append(sources, s)
		}
	}
	return map[string]interface{}{
		"tier":    "public",
		"stage":   serviceStage + " — " + serviceStageNote,
		"summary": "This API serves context layers AROUND a place in Austria — land prices, Natura 2000 / protected areas, OpenStreetMap proximity, geographic names, legal references, municipalities — and NE cells: declared land-use STATISTICS per H3 cell (no object ids, numbers or geometry). It serves no parcel-level data. Every response names its own source in meta.license and the X-Data-Attribution header; obligations differ per source.",
		"per_source_obligations": []string{
			"NE cells (/api/v1/ne/*): CC BY 4.0 — '" + NENotice() + "'; statistics per cell, nothing to re-identify; the observed rows inside are {OBS_NAME}'s and carry its credit",
			"Statistik Austria (Gemeinden, Preise): CC BY 4.0 — 'Datenquelle: STATISTIK AUSTRIA', licence link, 'bearbeitet/modelliert' for price estimates (they are model outputs, not official valuations)",
			"EEA Natura 2000: attribution 'Source: European Environment Agency (EEA)'",
			"WDPA (UNEP-WCMC/IUCN): attribution, NON-COMMERCIAL use only under the WDPA terms — check https://www.protectedplanet.net/en/legal before commercial use",
			"OpenStreetMap: '© OpenStreetMap contributors', ODbL — a redistributed DATABASE that merges osm{} fields becomes share-alike",
			"BEV DLM – Geographische Namen: '" + BEVAttributionDE + " – Geographische Namen (DLM), CC BY 4.0, bearbeitet' with the Stichtag from /api/v1/toponyms/stats",
			"RIS: 'Datenquelle: RIS, Bundeskanzleramt Österreich' (CC BY 4.0)",
			"GADM: academic / non-commercial only",
		},
		"not_served":       "Cadastre (parcels, building footprints, Benützungsart polygons, EZ, GNR). Clients that need it assemble it themselves from the BEV vector tiles (CC BY 4.0) — see bevdirect, https://github.com/raffopenssh/vtcseamless (MIT) — and pass us only a point (lon/lat) plus their own features.",
		"personal_data":    "None. No owner data, no parcel identifiers are stored or returned.",
		"sources":          sources,
		"siblings":         siblingServices(),
		"response_headers": []string{"Link: <...>; rel=\"license\" (per source)", "X-Data-Attribution (per source)"},
		"caching":          "/api/v1/cache/policy",
		"code_license":     "MIT",
		"code_repository":  "private (source not published); public components: https://github.com/raffopenssh/vtcseamless (bevdirect), https://github.com/raffopenssh/vtcseamless-py (ne_cells reference)",
		"operator":         "umfeld-at — Impressum: /impressum · Datenschutz: /datenschutz",
		"generated_at":     time.Now().UTC().Format(time.RFC3339),
	}
}
