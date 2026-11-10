// ne_mvt_qgs.go — the cell layer as something QGIS opens in one step:
//
//	GET /api/v1/ne/mvt.qgs?bbox=w,s,e,n|kg=|adm=|share=&f=&key=   a QGIS project
//	GET /api/v1/ne/mvt.qml?for=tiles|ogr                            the style alone
//
// A bare tile URL cannot carry a view: QGIS opens it on the whole world with
// one grey symbol and no legend. The project (.qgs, a few KB of XML) carries
// the view (canvas extent = the box asked for), the live tile layer with its
// zoom range and the filter, an OSM basemap underneath, and the cell style:
// one legend entry per declared group in the landing page's palette, change
// outlines on top. Double-click the file → the map the caller was looking at.
//
// The .qml is the same style as a sidecar: `for=tiles` is the vector-tile
// renderer (paste onto a live layer), `for=ogr` the categorized renderer for a
// PMTiles archive opened through GDAL (QGIS has no native PMTiles vector-tile
// source; GDAL ≥ 3.8 reads the archive as a plain vector layer, one zoom at a
// time). Saved next to `x.pmtiles` as `x.qml`, QGIS applies it on open.
//
// Everything here is XML the public tier may hand out: no prefix, no token,
// the key (if any) only because the caller put it in the request.
package main

import (
	"fmt"
	"html"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// neQGSGroupLabel — legend text per declared group (ns_groups order).
var neQGSGroupLabel = [9]string{"buildings", "building land", "arable", "grassland", "forest", "water", "traffic", "alpine", "other"}

func neQGSRGB(i int) string {
	c := neGlyphGroupRGB[i]
	return fmt.Sprintf("%d,%d,%d,255", c[0], c[1], c[2])
}

// neQGSFill — one SimpleFill symbol (QGIS symbol XML); outline "" = none.
func neQGSFill(name, color, outline, width string) string {
	os := "solid"
	if outline == "" {
		os, outline = "no", "0,0,0,255"
	}
	return `<symbol type="fill" name="` + name + `" alpha="1" clip_to_extent="1" force_rhr="0"><layer class="SimpleFill" enabled="1" locked="0" pass="0"><Option type="Map">` +
		`<Option type="QString" name="color" value="` + color + `"/><Option type="QString" name="joinstyle" value="miter"/><Option type="QString" name="offset" value="0,0"/><Option type="QString" name="offset_unit" value="MM"/>` +
		`<Option type="QString" name="outline_color" value="` + outline + `"/><Option type="QString" name="outline_style" value="` + os + `"/><Option type="QString" name="outline_width" value="` + width + `"/><Option type="QString" name="outline_width_unit" value="MM"/><Option type="QString" name="style" value="solid"/>` +
		`</Option></layer></symbol>`
}

// neQGSView — what the landing page showed, read from the share hash
// (neWhereParseShare): the project turns each of its toggles into a legend
// entry or basemap that starts in the same state. Without a hash: declared
// profile, change + mismatch on, the rest off.
type neQGSView struct {
	obs, change, diff, trees, sub, roads, relief, ortho bool
	orthoOp                                             float64
}

func neQGSViewOf(share string) neQGSView {
	v := neQGSView{change: true, diff: true, orthoOp: 0.5}
	if share == "" {
		return v
	}
	sv := neWhereParseShare(share)
	v.obs = sv.Profile == "obs"
	v.change, v.diff = sv.has("change"), sv.has("diff")
	v.trees, v.sub, v.roads, v.relief = sv.has("trees"), sv.has("subsidy"), sv.has("roads"), sv.has("relief")
	v.ortho = sv.has("ortho")
	for _, f := range sv.Flags {
		if strings.HasPrefix(f, "ortho=") {
			if x, err := strconv.ParseFloat(f[6:], 64); err == nil && x >= 0.1 && x <= 1 {
				v.orthoOp = x
			}
		}
	}
	return v
}

// neQGSTileStyles — the basic vector-tile renderer, one legend entry per
// toggle of the landing page (enabled = what the share link showed):
//
//	declared · <group>     fill by "d" (every zoom: aggregates carry the dominant group)
//	observed · <group>     fill by "o" (the ;v=obs profile)
//	no declared use        "d" IS NULL
//	change · confirmed/candidate   red outlines, z 13+ (;change)
//	declared ≠ observed    violet outline on aggregates ≥ 25 % mismatch, z 9–12 (;diff)
//	trees · <height>       green hatch by "ht" (;trees), z 13+
//	subsidy · <class>      orange outline by "sub" (;subsidy), z 13+
//	roads · ≤ 50 m         grey outline, "rd_min" = 0 (;roads), z 13+
//
// neQGSStyleMaxZ — a style's zoom ceiling: QGIS overzooms the deepest tile
// (neMVTMaxZ) when the reader zooms in further, but only paints styles whose
// range covers that zoom.
const neQGSStyleMaxZ = 24

func neQGSTileStyles(v neQGSView) string {
	var b strings.Builder
	b.WriteString(`<renderer type="basic"><styles>`)
	style := func(on bool, name, expr string, minz, maxz int, sym string) {
		if maxz >= neMVTMaxZ { // z15 tiles are overzoomed past the source ceiling: keep painting
			maxz = neQGSStyleMaxZ
		}
		en := "0"
		if on {
			en = "1"
		}
		fmt.Fprintf(&b, `<style enabled="%s" name="%s" layer="%s" geometry="2" expression="%s" min-zoom="%d" max-zoom="%d"><symbols>%s</symbols></style>`,
			en, html.EscapeString(name), neMVTLayerName, html.EscapeString(expr), minz, maxz, sym)
	}
	// fills carry a hairline outline in their own colour: without one the
	// anti-aliased edges of touching hexagons leave a light honeycomb of gaps
	for i, g := range neGlyphGroups {
		style(!v.obs, "declared · "+neQGSGroupLabel[i]+" ("+g+")", `"d"='`+g+`'`, neMVTMinZ, neMVTMaxZ, neQGSFill("0", neQGSRGB(i), neQGSRGB(i), "0.15"))
	}
	style(!v.obs, "no declared use", `"d" IS NULL`, neMVTMinZ, neMVTMaxZ, neQGSFill("0", "235,235,235,160", "235,235,235,160", "0.15"))
	for i, g := range neGlyphGroups {
		style(v.obs, "observed · "+neQGSGroupLabel[i]+" ("+g+")", `"o"='`+g+`'`, neMVTMinZ, neMVTMaxZ, neQGSFill("0", neQGSRGB(i), neQGSRGB(i), "0.15"))
	}
	style(v.obs, "not observed", `"o" IS NULL`, neMVTMinZ, neMVTMaxZ, neQGSFill("0", "235,235,235,160", "235,235,235,160", "0.15"))
	style(v.change, "change · confirmed", `"chg"='conf'`, 13, neMVTMaxZ, neQGSFill("0", "0,0,0,0", "200,30,30,255", "0.5"))
	style(v.change, "change · candidate", `"chg"='cand'`, 13, neMVTMaxZ, neQGSFill("0", "0,0,0,0", "200,30,30,140", "0.3"))
	style(v.change, "change · in the area (aggregate)", `"res" < 12 AND "chg_conf" > 0`, neMVTMinZ, 12, neQGSFill("0", "0,0,0,0", "200,30,30,200", "0.35"))
	style(v.diff, "declared ≠ observed (≥ 25 % of the area)", `"res" < 12 AND "diff_pct" >= 25`, neMVTMinZ, 12, neQGSFill("0", "0,0,0,0", "120,60,180,200", "0.35"))
	style(v.diff, "declared ≠ observed (this cell)", `"res" = 12 AND "d" IS NOT NULL AND "o" IS NOT NULL AND "d" <> "o"`, 13, neMVTMaxZ, neQGSFill("0", "0,0,0,0", "120,60,180,200", "0.35"))
	for _, t := range []struct {
		lo, hi int
		a      string
	}{{3, 10, "70"}, {10, 20, "120"}, {20, 30, "170"}, {30, 99, "220"}} {
		name := fmt.Sprintf("trees · %d–%d m", t.lo, t.hi)
		expr := fmt.Sprintf(`"ht" >= %d AND "ht" < %d`, t.lo, t.hi)
		if t.hi == 99 {
			name, expr = fmt.Sprintf("trees · ≥ %d m", t.lo), fmt.Sprintf(`"ht" >= %d`, t.lo)
		}
		style(v.trees, name, expr, 13, neMVTMaxZ, neQGSFill("0", "20,110,60,"+t.a, "20,110,60,255", "0.2"))
	}
	subLabel := []string{"", "arable", "grassland", "alpine pasture", "vineyard", "permanent crops", "landscape elements", "other"}
	for i, c := range neSubClass {
		if i == 0 {
			continue
		}
		style(v.sub, "subsidy · "+subLabel[i]+" ("+c+")", `"sub"='`+c+`'`, 13, neMVTMaxZ, neQGSFill("0", "230,159,0,60", "230,159,0,255", "0.4"))
	}
	style(v.roads, "roads · within 50 m", `"rd_min" = 0`, 13, neMVTMaxZ, neQGSFill("0", "0,0,0,0", "90,90,90,220", "0.3"))
	b.WriteString(`</styles></renderer>`)
	return b.String()
}

// neQGSOGRStyles — categorized renderer on "d" for the archive opened as a
// vector layer (GDAL PMTiles driver).
func neQGSOGRStyles() string {
	var b strings.Builder
	b.WriteString(`<renderer-v2 type="categorizedSymbol" attr="d" forceraster="0" enableorderby="0" symbollevels="0"><categories>`)
	for i, g := range neGlyphGroups {
		fmt.Fprintf(&b, `<category type="string" value="%s" label="%s" symbol="%d" render="true"/>`, g, html.EscapeString(neQGSGroupLabel[i]+" ("+g+")"), i)
	}
	b.WriteString(`<category value="" label="no declared use" symbol="9" render="true"/></categories><symbols>`)
	for i := range neGlyphGroups {
		b.WriteString(neQGSFill(fmt.Sprint(i), neQGSRGB(i), neQGSRGB(i), "0.15"))
	}
	b.WriteString(neQGSFill("9", "235,235,235,160", "235,235,235,160", "0.15"))
	b.WriteString(`</symbols><source-symbol>` + neQGSFill("0", "200,200,200,255", "", "0") + `</source-symbol></renderer-v2>`)
	return b.String()
}

const neQGSCRS3857 = `<spatialrefsys nativeFormat="Wkt"><wkt>PROJCRS["WGS 84 / Pseudo-Mercator",BASEGEOGCRS["WGS 84",DATUM["World Geodetic System 1984",ELLIPSOID["WGS 84",6378137,298.257223563,LENGTHUNIT["metre",1]]],PRIMEM["Greenwich",0,ANGLEUNIT["degree",0.0174532925199433]],ID["EPSG",4326]],CONVERSION["Popular Visualisation Pseudo-Mercator",METHOD["Popular Visualisation Pseudo Mercator",ID["EPSG",1024]],PARAMETER["Latitude of natural origin",0,ANGLEUNIT["degree",0.0174532925199433],ID["EPSG",8801]],PARAMETER["Longitude of natural origin",0,ANGLEUNIT["degree",0.0174532925199433],ID["EPSG",8802]],PARAMETER["False easting",0,LENGTHUNIT["metre",1],ID["EPSG",8806]],PARAMETER["False northing",0,LENGTHUNIT["metre",1],ID["EPSG",8807]]],CS[Cartesian,2],AXIS["easting (X)",east,ORDER[1],LENGTHUNIT["metre",1]],AXIS["northing (Y)",north,ORDER[2],LENGTHUNIT["metre",1]],ID["EPSG",3857]]</wkt><proj4>+proj=merc +a=6378137 +b=6378137 +lat_ts=0 +lon_0=0 +x_0=0 +y_0=0 +k=1 +units=m +nadgrids=@null +wktext +no_defs</proj4><srsid>3857</srsid><srid>3857</srid><authid>EPSG:3857</authid><description>WGS 84 / Pseudo-Mercator</description><projectionacronym>merc</projectionacronym><ellipsoidacronym>EPSG:7030</ellipsoidacronym><geographicflag>false</geographicflag></spatialrefsys>`

func neQGSMerc(lon, lat float64) (float64, float64) {
	const r = 6378137.0
	lat = max(-85.05, min(85.05, lat))
	return lon * math.Pi / 180 * r, math.Log(math.Tan((90+lat)*math.Pi/360)) * r
}

// neQGSID — a QGIS layer id. QGIS only binds layer-tree entries to layers
// whose id has the `<name>_<uuid>` shape it generates itself; a plain word
// loads the layer but leaves the tree entry empty. Deterministic per name.
func neQGSID(name string) string {
	h := uint64(1469598103934665603)
	for i := 0; i < len(name); i++ {
		h = (h ^ uint64(name[i])) * 1099511628211
	}
	x := fmt.Sprintf("%016x", h)
	return fmt.Sprintf("%s_%s_%s_4%s_8%s_%s%s", name, x[:8], x[8:12], x[12:15], x[15:16]+x[:2], x[2:12], "00")
}

// neQGSRaster — one xyz raster basemap (QGIS "wms" provider, type=xyz):
// layer-tree entry + maplayer. grey: desaturate (OSM under coloured cells).
func neQGSRaster(id, name, url, rights string, zmax int, opacity float64, checked, grey bool) (tree, layer string) {
	src := html.EscapeString(fmt.Sprintf("type=xyz&url=%s&zmax=%d&zmin=0", url, zmax))
	chk := "Qt::Unchecked"
	if checked {
		chk = "Qt::Checked"
	}
	// every attribute spelled out: a missing one reads as 0, which QGIS
	// takes as colorize / invert and paints the photo blue and magenta
	hue := `<huesaturation saturation="0" colorizeOn="0" colorizeGreen="128" colorizeBlue="128" grayscaleMode="0" colorizeRed="255" colorizeStrength="100" invertColors="0"/>`
	if grey {
		hue = `<huesaturation saturation="-30" colorizeOn="0" colorizeGreen="128" colorizeBlue="128" grayscaleMode="2" colorizeRed="255" colorizeStrength="100" invertColors="0"/>`
	}
	id = neQGSID(id)
	tree = fmt.Sprintf(`<layer-tree-layer id="%s" name="%s" source="%s" providerKey="wms" checked="%s" expanded="0" legend_exp="" patch_size="-1,-1"><customproperties><Option/></customproperties></layer-tree-layer>`, id, html.EscapeString(name), src, chk)
	layer = fmt.Sprintf(`<maplayer type="raster" hasScaleBasedVisibilityFlag="0" autoRefreshMode="Disabled" styleCategories="AllStyleCategories"><id>%s</id><datasource>%s</datasource><layername>%s</layername><srs>%s</srs><resourceMetadata><rights>%s</rights></resourceMetadata><provider>wms</provider><flags><Identifiable>0</Identifiable><Removable>1</Removable><Searchable>0</Searchable><Private>0</Private></flags><customproperties><Option/></customproperties><pipe><provider><resampling enabled="false" maxOversampling="2" zoomedInResamplingMethod="nearestNeighbour" zoomedOutResamplingMethod="nearestNeighbour"/></provider><rasterrenderer type="singlebandcolordata" opacity="%.2f" alphaBand="-1" band="1" nodataColor=""><rasterTransparency/><minMaxOrigin><limits>None</limits><extent>WholeRaster</extent><statAccuracy>Estimated</statAccuracy><cumulativeCutLower>0.02</cumulativeCutLower><cumulativeCutUpper>0.98</cumulativeCutUpper><stdDevFactor>2</stdDevFactor></minMaxOrigin></rasterrenderer><brightnesscontrast brightness="0" contrast="0" gamma="1"/>%s<rasterresampler maxOversampling="2"/><resamplingStage>resamplingFilter</resamplingStage></pipe><blendMode>0</blendMode></maplayer>`,
		id, src, html.EscapeString(name), neQGSCRS3857, html.EscapeString(rights), opacity, hue)
	return
}

// neQGSOrthoURL — basemap.at's orthophoto, the landing page's ;ortho
// (WMTS bmaporthofoto30cm, google3857 — note {y}/{x} order, CC BY 4.0).
const neQGSOrthoURL = "https://mapsneu.wien.gv.at/basemap/bmaporthofoto30cm/normal/google3857/{z}/{y}/{x}.jpeg"

// neQGSVector — one GeoJSON-over-HTTP vector layer (QGIS "ogr" provider reads
// the URL directly): tree apices from the observed-layer peer, OSM lines and
// water from this service. One fetch per layer on open — only offered for a
// box small enough (neQGSTreesKm2 / neQGSLinesKm2), like the page's overlays
// that exist at the near zooms only.
func neQGSVector(id, name, url, geom, rights, renderer string, checked bool, opacity string, minScale int) (tree, layer string) {
	id = neQGSID(id)
	chk := "Qt::Unchecked"
	if checked {
		chk = "Qt::Checked"
	}
	src := html.EscapeString(url)
	tree = fmt.Sprintf(`<layer-tree-layer id="%s" name="%s" source="%s" providerKey="ogr" checked="%s" expanded="0" legend_exp="" patch_size="-1,-1"><customproperties><Option/></customproperties></layer-tree-layer>`, id, html.EscapeString(name), src, chk)
	scale := `hasScaleBasedVisibilityFlag="0"`
	if minScale > 0 { // like the page: trees appear at the near zooms only
		scale = fmt.Sprintf(`hasScaleBasedVisibilityFlag="1" maxScale="1" minScale="%d"`, minScale)
	}
	layer = fmt.Sprintf(`<maplayer type="vector" geometry="%s" %s autoRefreshMode="Disabled" styleCategories="AllStyleCategories" readOnly="1"><id>%s</id><datasource>%s</datasource><layername>%s</layername><srs>%s</srs><resourceMetadata><rights>%s</rights></resourceMetadata><provider encoding="UTF-8">ogr</provider><flags><Identifiable>1</Identifiable><Removable>1</Removable><Searchable>0</Searchable><Private>0</Private></flags><customproperties><Option/></customproperties>%s<blendMode>0</blendMode><layerOpacity>%s</layerOpacity><featureBlendMode>0</featureBlendMode></maplayer>`,
		geom, scale, id, src, html.EscapeString(name), neQGSCRS4326, html.EscapeString(rights), renderer, opacity)
	return
}

const neQGSCRS4326 = `<spatialrefsys nativeFormat="Wkt"><wkt>GEOGCRS["WGS 84",DATUM["World Geodetic System 1984",ELLIPSOID["WGS 84",6378137,298.257223563,LENGTHUNIT["metre",1]]],PRIMEM["Greenwich",0,ANGLEUNIT["degree",0.0174532925199433]],CS[ellipsoidal,2],AXIS["geodetic latitude (Lat)",north,ORDER[1],ANGLEUNIT["degree",0.0174532925199433]],AXIS["geodetic longitude (Lon)",east,ORDER[2],ANGLEUNIT["degree",0.0174532925199433]],ID["EPSG",4326]]</wkt><proj4>+proj=longlat +datum=WGS84 +no_defs</proj4><srsid>3452</srsid><srid>4326</srid><authid>EPSG:4326</authid><description>WGS 84</description><projectionacronym>longlat</projectionacronym><ellipsoidacronym>EPSG:7030</ellipsoidacronym><geographicflag>true</geographicflag></spatialrefsys>`

// neQGSTreeRenderer — the page's tree marks: a disc per apex, its diameter the
// crown (crown_d_m, metres on the map), colour = the page's five height bands
// (AP_COL, apBand: <5, 5–15, 15–25, 25–35, ≥35 m), a subtle rim, translucent so
// overlapping crowns and the ground read through — the legend is the ramp.
func neQGSTreeRenderer() string {
	cls := []struct {
		lo, hi   float64
		label    string
		col, rim string // translucent disc + a subtle rim of the same hue, darker
	}{{3, 5, "3–5 m", "244,252,234,120", "190,215,170,110"}, {5, 15, "5–15 m", "220,242,196,120", "160,200,130,120"}, {15, 25, "15–25 m", "191,231,163,125", "130,185,100,130"}, {25, 35, "25–35 m", "159,218,134,135", "100,170,80,140"}, {35, 70, "≥ 35 m", "124,203,110,150", "70,150,60,150"}}
	var r, sy strings.Builder
	r.WriteString(`<renderer-v2 type="graduatedSymbol" attr="height_m" graduatedMethod="GraduatedColor" symbollevels="0" forceraster="0" enableorderby="0"><ranges>`)
	for i, c := range cls {
		fmt.Fprintf(&r, `<range lower="%g" upper="%g" symbol="%d" label="%s" render="true"/>`, c.lo, c.hi, i, html.EscapeString(c.label))
		fmt.Fprintf(&sy, `<symbol type="marker" name="%d" alpha="1" clip_to_extent="1" force_rhr="0"><layer class="SimpleMarker" enabled="1" locked="0" pass="0"><Option type="Map"><Option type="QString" name="name" value="circle"/><Option type="QString" name="color" value="%s"/><Option type="QString" name="outline_style" value="solid"/><Option type="QString" name="outline_color" value="%s"/><Option type="QString" name="outline_width" value="0.12"/><Option type="QString" name="outline_width_unit" value="MM"/><Option type="QString" name="size" value="6"/><Option type="QString" name="size_unit" value="MetersInMapUnits"/><Option type="QString" name="scale_method" value="diameter"/><Option type="QString" name="offset" value="0,0"/><Option type="QString" name="offset_unit" value="MM"/><Option type="QString" name="angle" value="0"/></Option><data_defined_properties><Option type="Map"><Option type="QString" name="name" value=""/><Option name="properties" type="Map"><Option name="size" type="Map"><Option type="bool" name="active" value="true"/><Option type="QString" name="expression" value="coalesce(&quot;crown_d_m&quot;, 6)"/><Option type="int" name="type" value="3"/></Option></Option><Option type="QString" name="type" value="collection"/></Option></data_defined_properties></layer></symbol>`, i, c.col, c.rim)
	}
	r.WriteString(`</ranges><symbols>` + sy.String() + `</symbols><source-symbol>` + neQGSLine("0", "124,203,110,255", "0.3", "solid") + `</source-symbol></renderer-v2>`)
	return r.String()
}

func neQGSLine(name, color, width, style string) string {
	return `<symbol type="line" name="` + name + `" alpha="1" clip_to_extent="1" force_rhr="0"><layer class="SimpleLine" enabled="1" locked="0" pass="0"><Option type="Map"><Option type="QString" name="line_color" value="` + color + `"/><Option type="QString" name="line_width" value="` + width + `"/><Option type="QString" name="line_width_unit" value="MM"/><Option type="QString" name="line_style" value="` + style + `"/><Option type="QString" name="capstyle" value="round"/><Option type="QString" name="joinstyle" value="round"/></Option></layer></symbol>`
}

// neQGSRoadRenderer — the page's roads ink: major roads darker and wider,
// rail dashed, the rest thin grey.
func neQGSRoadRenderer() string {
	return `<renderer-v2 type="RuleRenderer" symbollevels="0" forceraster="0" enableorderby="0"><rules key="{00000000-0000-4000-8000-000000000rd0}">` +
		`<rule filter="&quot;cat&quot; = 'rail'" symbol="0" key="{00000000-0000-4000-8000-000000000rd1}" label="rail"/>` +
		`<rule filter="&quot;cat&quot; = 'road' AND &quot;major&quot;" symbol="1" key="{00000000-0000-4000-8000-000000000rd2}" label="major road"/>` +
		`<rule filter="&quot;cat&quot; = 'road' AND NOT &quot;major&quot;" symbol="2" key="{00000000-0000-4000-8000-000000000rd3}" label="road, track, path"/>` +
		`</rules><symbols>` + neQGSLine("0", "70,70,70,255", "0.5", "dash") + neQGSLine("1", "60,60,60,230", "0.6", "solid") + neQGSLine("2", "110,110,110,200", "0.3", "solid") + `</symbols></renderer-v2>`
}

func neQGSWaterRenderer() string {
	return `<renderer-v2 type="singleSymbol" symbollevels="0" forceraster="0" enableorderby="0"><symbols>` + neQGSFill("0", "79,147,214,120", "79,147,214,255", "0.2") + `</symbols></renderer-v2>`
}

// box sizes (km²) up to which a project offers the one-fetch GeoJSON layers:
// trees ≈ 7 000 apices/km² (the peer caps a call at 20 000), lines ≈ 15/km².
const (
	neQGSTreesKm2 = 3.0
	neQGSLinesKm2 = 400.0
)

func neQGSBoxKm2(b [4]float64) float64 {
	la := (b[1] + b[3]) / 2 * math.Pi / 180
	return (b[2] - b[0]) * 111.32 * math.Cos(la) * (b[3] - b[1]) * 110.57
}

// handleNEMVTProject — /api/v1/ne/mvt.qgs
func handleNEMVTProject(w http.ResponseWriter, r *http.Request) {
	q := neQuery(r)
	box, name, canon, msg, code := nePMTBox(r)
	if msg != "" {
		apiError(w, msg, code)
		return
	}
	share := q.Get("share")
	view := neQGSViewOf(share)
	// a share link with a view (;at=) opens on THAT view, even when it also
	// names a unit — the project mirrors the page, the archive covers the unit
	if share != "" {
		if vb := neShareViewBox(share); vb != nil {
			box, name = *vb, "view"
			if k, v, _ := neStatsParseShare(share); k != "" {
				name = k + "-" + v + "-view"
			}
		}
	}
	base := publicBaseURL(r)
	tileURL := base + "/api/v1/ne/mvt/{z}/{x}/{y}.pbf"
	lname := "umfeld-at cells"
	var sep string
	if canon != "" {
		tileURL += "?f=" + url.QueryEscape(canon)
		lname += " · " + neFiltParse(canon).Words()
		sep = "&"
	} else {
		sep = "?"
	}
	if k := q.Get("key"); k != "" && len(k) <= 64 {
		tileURL += sep + "key=" + url.QueryEscape(k)
	}
	// a view box smaller than one res-12 cell (h3=) still needs an extent
	if box[2]-box[0] < 0.002 {
		box[0], box[2] = box[0]-0.001, box[2]+0.001
	}
	if box[3]-box[1] < 0.0015 {
		box[1], box[3] = box[1]-0.00075, box[3]+0.00075
	}
	x0, y0 := neQGSMerc(box[0], box[1])
	x1, y1 := neQGSMerc(box[2], box[3])
	pr := peers()
	attrib := html.EscapeString(BEVNotice() + " · " + pr.ObsName + " · " + pr.SubName + " · © OpenStreetMap contributors · basemap.at (CC BY 4.0) · EEA Natura 2000")
	src := html.EscapeString(fmt.Sprintf("type=xyz&url=%s&zmin=%d&zmax=%d", tileURL, neMVTMinZ, neMVTMaxZ))
	ext := fmt.Sprintf(`<extent><xmin>%.2f</xmin><ymin>%.2f</ymin><xmax>%.2f</xmax><ymax>%.2f</ymax></extent>`, x0, y0, x1, y1)

	// basemaps, top → bottom under the cells: relief (observed-layer peer's
	// hillshade, ;relief), orthophoto (;ortho), OpenStreetMap (on unless the
	// photo is)
	type bm struct{ tree, layer, id string }
	var bms, over []bm // under the cells / over them (the page's roads + trees overlays)
	km2 := neQGSBoxKm2(box)
	bb := fmt.Sprintf("%.5f,%.5f,%.5f,%.5f", box[0], box[1], box[2], box[3])
	if pr.ObsAPI != "" && km2 <= neQGSTreesKm2 {
		t, l := neQGSVector("trees", "trees · "+pr.ObsName+" (crown rings, this view)", pr.ObsAPI+"/api/v1/trees/bbox?bbox="+bb+"&min_height=3&format=geojson&limit=20000", "Point", pr.ObsName+" (CC BY 4.0), BEV", neQGSTreeRenderer(), view.trees, "1", 6000)
		over = append(over, bm{t, l, "trees"})
	}
	if km2 <= neQGSLinesKm2 {
		t, l := neQGSVector("roads", "roads · OpenStreetMap (this view)", base+"/api/v1/osm/geometry?bbox="+bb+"&cat=road,rail", "Line", "© OpenStreetMap contributors (ODbL)", neQGSRoadRenderer(), view.roads, "0.8", 0)
		over = append(over, bm{t, l, "roads"})
		t, l = neQGSVector("water", "water · OpenStreetMap (this view)", base+"/api/v1/osm/geometry?bbox="+bb+"&cat=water_area", "Polygon", "© OpenStreetMap contributors (ODbL)", neQGSWaterRenderer(), false, "0.8", 0)
		bms = append(bms, bm{t, l, "water"})
	}
	if pr.ObsWeb != "" {
		t, l := neQGSRaster("relief", "relief · "+pr.ObsName, pr.ObsWeb+"/tiles/hillshade/{z}/{x}/{y}.png", pr.ObsName, 15, 0.55, view.relief, false)
		bms = append(bms, bm{t, l, "relief"})
	}
	t, l := neQGSRaster("ortho", "orthophoto · basemap.at", neQGSOrthoURL, "basemap.at, CC BY 4.0", 19, math.Min(1, view.orthoOp*1.6), view.ortho, false)
	bms = append(bms, bm{t, l, "ortho"})
	t, l = neQGSRaster("osm", "OpenStreetMap", "https://tile.openstreetmap.org/{z}/{x}/{y}.png", "© OpenStreetMap contributors", 19, 1, !view.ortho, true)
	bms = append(bms, bm{t, l, "osm"})

	var b strings.Builder
	b.WriteString(`<!DOCTYPE qgis PUBLIC 'http://mrcc.com/qgis.dtd' 'SYSTEM'>` + "\n")
	fmt.Fprintf(&b, `<qgis version="3.34" projectname="%s">`, html.EscapeString(lname+" · "+name))
	b.WriteString(`<title>` + html.EscapeString(lname) + `</title><projectCrs>` + neQGSCRS3857 + `</projectCrs>`)
	b.WriteString(`<mapcanvas name="theMapCanvas" annotationsVisible="1"><units>meters</units>` + ext + `<rotation>0</rotation><destinationsrs>` + neQGSCRS3857 + `</destinationsrs><rendermaptile>0</rendermaptile></mapcanvas>`)
	b.WriteString(`<layer-tree-group><customproperties><Option/></customproperties>`)
	cid := neQGSID("cells")
	for _, m := range over {
		b.WriteString(m.tree)
	}
	fmt.Fprintf(&b, `<layer-tree-layer id="%s" name="%s" source="%s" providerKey="xyzvectortiles" checked="Qt::Checked" expanded="1" legend_exp="" patch_size="-1,-1"><customproperties><Option/></customproperties></layer-tree-layer>`, cid, html.EscapeString(lname), src)
	for _, m := range bms {
		b.WriteString(m.tree)
	}
	b.WriteString(`<custom-order enabled="0">`)
	for _, m := range over {
		b.WriteString(`<item>` + neQGSID(m.id) + `</item>`)
	}
	b.WriteString(`<item>` + cid + `</item>`)
	for _, m := range bms {
		b.WriteString(`<item>` + neQGSID(m.id) + `</item>`)
	}
	b.WriteString(`</custom-order></layer-tree-group>`)
	b.WriteString(`<projectlayers>`)
	fmt.Fprintf(&b, `<maplayer type="vector-tile" hasScaleBasedVisibilityFlag="0" autoRefreshMode="Disabled" styleCategories="AllStyleCategories"><id>%s</id><datasource>%s</datasource><layername>%s</layername><srs>%s</srs>`, cid, src, html.EscapeString(lname), neQGSCRS3857)
	fmt.Fprintf(&b, `<resourceMetadata><title>%s</title><abstract>%s&#10;&#10;%s</abstract><rights>%s</rights><links><link name="docs" type="information" url="%s/api/v1/docs/qgis-layer.md"/></links></resourceMetadata>`,
		html.EscapeString(lname), html.EscapeString("H3 cells of the declared/observed land-use product with change, subsidy and LiDAR statistics. res 7 at z 9 … res 12 (the cell itself, every attribute) at z 14+; aggregates below. Legend entries are the landing page's toggles (declared / observed profile, change, mismatch, trees, subsidy, roads) — tick them on and off. Field meanings: "+base+"/api/v1/docs/qgis-layer.md"), attrib, attrib, base)
	b.WriteString(`<provider>xyzvectortiles</provider><map-layer-style-manager current="default"><map-layer-style name="default"/></map-layer-style-manager><flags><Identifiable>1</Identifiable><Removable>1</Removable><Searchable>1</Searchable><Private>0</Private></flags><customproperties><Option/></customproperties>`)
	b.WriteString(neQGSTileStyles(view))
	op := "0.85"
	if view.ortho {
		op = "0.7"
	}
	b.WriteString(`<blendMode>0</blendMode><layerOpacity>` + op + `</layerOpacity></maplayer>`)
	for _, m := range append(over, bms...) {
		b.WriteString(m.layer)
	}
	b.WriteString(`</projectlayers><layerorder>`)
	for _, m := range over {
		b.WriteString(`<layer id="` + neQGSID(m.id) + `"/>`)
	}
	b.WriteString(`<layer id="` + cid + `"/>`)
	for _, m := range bms {
		b.WriteString(`<layer id="` + neQGSID(m.id) + `"/>`)
	}
	b.WriteString(`</layerorder>`)
	b.WriteString(`<properties><Gui><CanvasColorRedPart type="int">250</CanvasColorRedPart><CanvasColorGreenPart type="int">250</CanvasColorGreenPart><CanvasColorBluePart type="int">247</CanvasColorBluePart></Gui><Measurement><AreaUnits type="QString">ha</AreaUnits><DistanceUnits type="QString">meters</DistanceUnits></Measurement><SpatialRefSys><ProjectionsEnabled type="int">1</ProjectionsEnabled></SpatialRefSys></properties>`)
	fmt.Fprintf(&b, `<projectMetadata><title>%s</title><abstract>%s</abstract><author>umfeld-at</author></projectMetadata>`, html.EscapeString(lname), attrib)
	b.WriteString(`<ProjectViewSettings rotation="0" UseProjectScales="0"><Scales/><DefaultViewExtent xmin="` + fmt.Sprintf("%.2f", x0) + `" ymin="` + fmt.Sprintf("%.2f", y0) + `" xmax="` + fmt.Sprintf("%.2f", x1) + `" ymax="` + fmt.Sprintf("%.2f", y1) + `">` + neQGSCRS3857 + `</DefaultViewExtent></ProjectViewSettings>`)
	b.WriteString(`</qgis>` + "\n")
	fn := "umfeld-at-cells-" + name
	if canon != "" {
		fn += "-filtered"
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/x-qgis-project; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+fn+`.qgs"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(200)
	w.Write([]byte(b.String()))
	exportAccountFor(r, b.Len())
}

// handleNEMVTStyle — /api/v1/ne/mvt.qml?for=tiles|ogr
func handleNEMVTStyle(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("for")
	var body string
	if kind == "ogr" || kind == "pmtiles" {
		body = `<qgis version="3.34" styleCategories="Symbology">` + neQGSOGRStyles() + `<layerGeometryType>2</layerGeometryType></qgis>`
	} else {
		body = `<qgis version="3.34" styleCategories="Symbology">` + neQGSTileStyles(neQGSViewOf(r.URL.Query().Get("share"))) + `</qgis>`
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="umfeld-at-cells.qml"`)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(200)
	w.Write([]byte(`<!DOCTYPE qgis PUBLIC 'http://mrcc.com/qgis.dtd' 'SYSTEM'>` + "\n" + body + "\n"))
}
