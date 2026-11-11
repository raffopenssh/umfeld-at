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
// renderer (paste onto a live layer), `for=ogr` the rule-based renderer for a
// PMTiles archive opened through GDAL (QGIS has no native PMTiles vector-tile
// source; GDAL ≥ 3.8 reads the archive as a plain vector layer, one zoom at a
// time). Saved next to `x.pmtiles` as `x.qml`, QGIS applies it on open.
//
// Everything here is XML the public tier may hand out: no prefix, no token,
// the key (if any) only because the caller put it in the request.
package main

import (
	"archive/zip"
	"fmt"
	"html"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
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
	orthoOp, roadsOp                                    float64
}

func neQGSViewOf(share string) neQGSView {
	v := neQGSView{change: true, diff: true, orthoOp: 0.5, roadsOp: 0.7}
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
		if strings.HasPrefix(f, "roads=") {
			if x, err := strconv.ParseFloat(f[6:], 64); err == nil && x >= 0.1 && x <= 1 {
				v.roadsOp = x
			}
		}
	}
	return v
}

// neQGSTileStyles — the basic vector-tile renderer, one legend entry per
// toggle of the landing page (enabled = what the share link showed):
//
//	the rules are neQGSRules; tree crowns, subsidy Schläge, roads and relief
//	are layers of their own (ne_mvt_layers.go)
//
// neQGSStyleMaxZ — a style's zoom ceiling: QGIS overzooms the deepest tile
// (neMVTMaxZ) when the reader zooms in further, but only paints styles whose
// range covers that zoom.
const neQGSStyleMaxZ = 24

// neQGSRule — one legend entry of the cell layer: a toggle of the landing page.
type neQGSRule struct {
	on         bool
	name, expr string
	minz, maxz int
	sym        string
}

// neQGSRules — the cell layer's legend, bottom → top. The subsidy Schläge, the
// roads and the tree crowns are layers of their own (ne_mvt_layers.go): here
// only what a cell itself says.
//
// pic: the page's own cell picture (layer "cells · as on the map", z 12+) is
// in the project and paints the cells there; the fills then cover only the
// aggregates farther out, so the two never stack.
func neQGSRules(v neQGSView, pic bool) []neQGSRule {
	fillMax, far := neMVTMaxZ, ""
	if pic {
		fillMax, far = neLayerPicMinZ-1, " (aggregates, far zooms)"
	}
	var rs []neQGSRule
	add := func(on bool, name, expr string, minz, maxz int, sym string) {
		rs = append(rs, neQGSRule{on, name, expr, minz, maxz, sym})
	}
	// fills carry a hairline outline in their own colour: without one the
	// anti-aliased edges of touching hexagons leave a light honeycomb of gaps
	for i, g := range neGlyphGroups {
		add(!v.obs, "declared · "+neQGSGroupLabel[i]+" ("+g+")"+far, `"d"='`+g+`'`, neMVTMinZ, fillMax, neQGSFill("0", neQGSRGB(i), neQGSRGB(i), "0.15"))
	}
	add(!v.obs, "no declared use"+far, `"d" IS NULL`, neMVTMinZ, fillMax, neQGSFill("0", "214,210,202,200", "214,210,202,200", "0.15"))
	for i, g := range neGlyphGroups {
		add(v.obs, "observed · "+neQGSGroupLabel[i]+" ("+g+")"+far, `"o"='`+g+`'`, neMVTMinZ, fillMax, neQGSFill("0", neQGSRGB(i), neQGSRGB(i), "0.15"))
	}
	add(v.obs, "not observed"+far, `"o" IS NULL`, neMVTMinZ, fillMax, neQGSFill("0", "214,210,202,200", "214,210,202,200", "0.15"))
	// trees at the coarse levels: the wooded hexagons tinted by their tallest tree, the page's
	// ramp and bands (neGlyphTreeRGB); from z 14 on the crowns themselves take over (layer "trees")
	for b, lo := range []int{3, 5, 15, 25, 35} {
		c := neGlyphTreeRGB[b]
		col := fmt.Sprintf("%d,%d,%d,235", c[0], c[1], c[2])
		name, expr := "", ""
		if b == 4 {
			name, expr = "trees · tallest ≥ 35 m", `"ht" >= 35`
		} else {
			hi := []int{5, 15, 25, 35}[b]
			name, expr = fmt.Sprintf("trees · tallest %d–%d m", lo, hi), fmt.Sprintf(`"ht" >= %d AND "ht" < %d`, lo, hi)
		}
		add(v.trees, name+" (hexagons, far zooms)", expr, 12, 13, neQGSFill("0", col, col, "0.15"))
	}
	add(v.change, "change · confirmed", `"chg"='conf'`, 13, neMVTMaxZ, neQGSFill("0", "194,65,12,70", "194,65,12,255", "0.5"))
	add(v.change, "change · candidate", `"chg"='cand'`, 13, neMVTMaxZ, neQGSFill("0", "0,0,0,0", "194,65,12,150", "0.3"))
	add(v.change, "change · in the area (aggregate)", `"res" < 12 AND "chg_conf" > 0`, neMVTMinZ, 12, neQGSFill("0", "0,0,0,0", "194,65,12,200", "0.35"))
	add(v.diff, "declared ≠ observed (≥ 25 % of the area)", `"res" < 12 AND "diff_pct" >= 25`, neMVTMinZ, 12, neQGSFill("0", "0,0,0,0", "168,44,143,200", "0.35"))
	add(v.diff, "declared ≠ observed (this cell)", `"res" = 12 AND "d" IS NOT NULL AND "o" IS NOT NULL AND "d" <> "o"`, 13, neMVTMaxZ, neQGSFill("0", "0,0,0,0", "168,44,143,200", "0.35"))
	subLabel := []string{"", "arable", "grassland", "alpine pasture", "vineyard", "permanent crops", "landscape elements", "other"}
	for i, c := range neSubClass {
		if i == 0 {
			continue
		}
		// off: the Schläge as drawn are the layer "subsidy claims"; these mark the claimed CELLS by class
		add(false, "subsidy cells · "+subLabel[i]+" ("+c+")", `"sub"='`+c+`'`, 13, neMVTMaxZ, neQGSFill("0", "230,159,0,60", "230,159,0,255", "0.4"))
	}
	return rs
}

func neQGSTileStyles(v neQGSView, pic bool) string {
	var b strings.Builder
	b.WriteString(`<renderer type="basic"><styles>`)
	for _, r := range neQGSRules(v, pic) {
		maxz := r.maxz
		if maxz >= neMVTMaxZ { // z15 tiles are overzoomed past the source ceiling: keep painting
			maxz = neQGSStyleMaxZ
		}
		en := "0"
		if r.on {
			en = "1"
		}
		fmt.Fprintf(&b, `<style enabled="%s" name="%s" layer="%s" geometry="2" expression="%s" min-zoom="%d" max-zoom="%d"><symbols>%s</symbols></style>`,
			en, html.EscapeString(r.name), neMVTLayerName, html.EscapeString(r.expr), r.minz, maxz, r.sym)
	}
	b.WriteString(`</styles></renderer>`)
	return b.String()
}

// neQGSOGRStyles — the same legend as a rule-based renderer, for the archive
// opened as a plain vector layer (GDAL's PMTiles driver, which reads the
// deepest zoom: the cells themselves). Rules that only exist on aggregates
// are left out; every rule paints, in order, like the tile styles.
func neQGSOGRStyles(v neQGSView, maxZ int) string {
	var r, sy strings.Builder
	r.WriteString(`<renderer-v2 type="RuleRenderer" symbollevels="0" forceraster="0" enableorderby="0" referencescale="-1"><rules key="{00000000-0000-4000-8000-0000000c0000}">`)
	k := 0
	for _, x := range neQGSRules(v, false) {
		if x.minz > maxZ || x.maxz < maxZ {
			continue
		}
		chk := ""
		if !x.on {
			chk = ` checkstate="0"`
		}
		fmt.Fprintf(&r, `<rule filter="%s" symbol="%d" key="{00000000-0000-4000-8000-0000000c%04d}" label="%s"%s/>`, html.EscapeString(x.expr), k, k+1, html.EscapeString(x.name), chk)
		sy.WriteString(strings.Replace(x.sym, `name="0"`, fmt.Sprintf(`name="%d"`, k), 1))
		k++
	}
	r.WriteString(`</rules><symbols>` + sy.String() + `</symbols></renderer-v2>`)
	return r.String()
}

// neQGSTreeBands — the page's height bands (AP_COL): < 5 · 5–15 · 15–25 · 25–35 · ≥ 35 m.
var neQGSTreeBands = [6]float64{3, 5, 15, 25, 35, 80}

// neQGSTreeRing — the crown ring as a marker: its DIAMETER is the crown
// (crown_d, metres on the ground, so it grows with the zoom exactly as on the
// page), never under 1.3 mm (the page's 2.4 px minimum radius). The colour is
// the SYMBOL's colour drawn as the outline, the disc itself stays clear — so
// whatever recolours the symbol (a colour ramp of the graduated renderer, the
// colour button) recolours the ring.
//
// Web Mercator stretches the ground by 1/cos(latitude) (×1.5 over Austria) and
// QGIS does not undo that for symbol sizes, so the size expression does —
// only while the map is in EPSG:3857. corr is that factor as an expression:
// from the point's latitude ($y, the GeoJSON layer) or a constant (tiles).
func neQGSTreeRing(name string, c [3]uint8, corr string) string {
	size := html.EscapeString(`coalesce("crown_d", 6) * if(@map_crs = 'EPSG:3857', ` + corr + `, 1)`)
	return fmt.Sprintf(`<symbol type="marker" name="%s" alpha="1" clip_to_extent="1" force_rhr="0"><layer class="SimpleMarker" enabled="1" locked="0" pass="0"><Option type="Map"><Option type="QString" name="name" value="circle"/><Option type="QString" name="color" value="%d,%d,%d,255"/><Option type="QString" name="outline_style" value="solid"/><Option type="QString" name="outline_color" value="%d,%d,%d,255"/><Option type="QString" name="outline_width" value="0.4"/><Option type="QString" name="outline_width_unit" value="MM"/><Option type="QString" name="size" value="6"/><Option type="QString" name="size_unit" value="RenderMetersInMapUnits"/><Option type="QString" name="size_map_unit_scale" value="3x:0,0,1,1.3,0,0"/><Option type="QString" name="scale_method" value="diameter"/><Option type="QString" name="offset" value="0,0"/><Option type="QString" name="offset_unit" value="MM"/><Option type="QString" name="angle" value="0"/></Option><data_defined_properties><Option type="Map"><Option type="QString" name="name" value=""/><Option name="properties" type="Map"><Option name="size" type="Map"><Option type="bool" name="active" value="true"/><Option type="QString" name="expression" value="%s"/><Option type="int" name="type" value="3"/></Option><Option name="fillColor" type="Map"><Option type="bool" name="active" value="true"/><Option type="QString" name="expression" value="color_rgba(0,0,0,0)"/><Option type="int" name="type" value="3"/></Option><Option name="outlineColor" type="Map"><Option type="bool" name="active" value="true"/><Option type="QString" name="expression" value="@symbol_color"/><Option type="int" name="type" value="3"/></Option></Option><Option type="QString" name="type" value="collection"/></Option></data_defined_properties></layer></symbol>`,
		name, c[0], c[1], c[2], c[0], c[1], c[2], size)
}

const neQGSTreeCorrLat = `1 / cos(radians($y))`

func neQGSTreeLabel(i int) string {
	if i == 4 {
		return "≥ 35 m"
	}
	return fmt.Sprintf("%g–%g m", neQGSTreeBands[i], neQGSTreeBands[i+1])
}

// neQGSTreeGraduated — the trees as a GRADUATED renderer on "h" (the point
// layer trees.geojson): five classes in the page's bands and greens. In QGIS:
// Symbology → Colour ramp → any ramp (Viridis, …) recolours the rings; the
// class breaks, their count and the mode are the reader's to change.
func neQGSTreeGraduated() string {
	var b strings.Builder
	b.WriteString(`<renderer-v2 type="graduatedSymbol" attr="h" graduatedMethod="GraduatedColor" symbollevels="0" forceraster="0" enableorderby="0" referencescale="-1"><ranges>`)
	for i := 0; i < 5; i++ {
		fmt.Fprintf(&b, `<range lower="%g" upper="%g" symbol="%d" label="%s" render="true" uuid="{00000000-0000-4000-8000-00000000a%03d}"/>`, neQGSTreeBands[i], neQGSTreeBands[i+1], i, html.EscapeString(neQGSTreeLabel(i)), i)
	}
	b.WriteString(`</ranges><symbols>`)
	for i := 0; i < 5; i++ {
		b.WriteString(neQGSTreeRing(strconv.Itoa(i), neGlyphTreeRGB[i], neQGSTreeCorrLat))
	}
	b.WriteString(`</symbols><source-symbol>` + neQGSTreeRing("0", neGlyphTreeRGB[2], neQGSTreeCorrLat) + `</source-symbol>`)
	// the page's ramp, so the ramp control shows what is drawn
	b.WriteString(`<colorramp type="gradient" name="[source]"><Option type="Map"><Option type="QString" name="color1" value="` + neQGSTreeCol(0) + `"/><Option type="QString" name="color2" value="` + neQGSTreeCol(4) + `"/><Option type="QString" name="discrete" value="0"/><Option type="QString" name="rampType" value="gradient"/><Option type="QString" name="stops" value="0.25;` + neQGSTreeCol(1) + `:0.5;` + neQGSTreeCol(2) + `:0.75;` + neQGSTreeCol(3) + `"/></Option></colorramp>`)
	b.WriteString(`<classificationMethod id="Fixed"><symmetricMode enabled="0" symmetrypoint="0" astride="0"/><labelFormat format="%1–%2 m" labelprecision="0" trimtrailingzeroes="1"/><parameters><Option/></parameters><extraInformation/></classificationMethod><rotation/><sizescale/></renderer-v2>`)
	return b.String()
}

func neQGSTreeCol(i int) string {
	c := neGlyphTreeRGB[i]
	return fmt.Sprintf("%d,%d,%d,255", c[0], c[1], c[2])
}

// neQGSTreeStyles — the same rings on the "trees" TILE layer (views too large
// for one fetch): one legend entry per band. A vector-tile layer has no
// graduated renderer; there the colour is changed per entry.
func neQGSTreeStyles() string {
	var b strings.Builder
	b.WriteString(`<renderer type="basic"><styles>`)
	for i := 0; i < 5; i++ {
		expr := fmt.Sprintf(`"h" >= %g`, neQGSTreeBands[i])
		if i < 4 {
			expr += fmt.Sprintf(` AND "h" < %g`, neQGSTreeBands[i+1])
		}
		fmt.Fprintf(&b, `<style enabled="1" name="%s" layer="trees" geometry="0" expression="%s" min-zoom="-1" max-zoom="-1"><symbols>%s</symbols></style>`, html.EscapeString("tree · "+neQGSTreeLabel(i)), html.EscapeString(expr), neQGSTreeRing("0", neGlyphTreeRGB[i], "1.48"))
	}
	b.WriteString(`</styles></renderer>`)
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

// neQGSEnc — a URL as the value of `url=` in a QGIS layer source: the source
// is itself a query string, so the URL's own `&` and `=` are written %26 and
// %3D — the two QGIS turns back (and nothing else: `?` and every other escape
// pass through as they are). Left raw, QGIS cuts the URL at the first `&` and
// a key behind it is never sent.
func neQGSEnc(u string) string {
	return strings.NewReplacer("&", "%26", "=", "%3D").Replace(u)
}

// neQGSRasterOpt — one xyz raster layer (QGIS "wms" provider, type=xyz).
type neQGSRasterOpt struct {
	id, name, url, rights string
	zmin, zmax            int
	opacity               float64
	checked               bool
	grey                  bool // desaturate (OSM under coloured cells)
	blend                 int  // QGIS blend mode: 0 normal, 8 overlay (the page's mix-blend for the relief)
	hidpi                 bool // 512-px tiles
}

// neQGSRaster — layer-tree entry + maplayer.
func neQGSRaster(o neQGSRasterOpt) (tree, layer string) {
	raw := fmt.Sprintf("type=xyz&url=%s&zmax=%d&zmin=%d", neQGSEnc(o.url), o.zmax, o.zmin)
	if o.hidpi {
		raw += "&tilePixelRatio=2"
	}
	src := html.EscapeString(raw)
	chk := "Qt::Unchecked"
	if o.checked {
		chk = "Qt::Checked"
	}
	// every attribute spelled out: a missing one reads as 0, which QGIS
	// takes as colorize / invert and paints the photo blue and magenta
	hue := `<huesaturation saturation="0" colorizeOn="0" colorizeGreen="128" colorizeBlue="128" grayscaleMode="0" colorizeRed="255" colorizeStrength="100" invertColors="0"/>`
	if o.grey {
		hue = `<huesaturation saturation="-30" colorizeOn="0" colorizeGreen="128" colorizeBlue="128" grayscaleMode="2" colorizeRed="255" colorizeStrength="100" invertColors="0"/>`
	}
	id := neQGSID(o.id)
	tree = fmt.Sprintf(`<layer-tree-layer id="%s" name="%s" source="%s" providerKey="wms" checked="%s" expanded="0" legend_exp="" patch_size="-1,-1"><customproperties><Option/></customproperties></layer-tree-layer>`, id, html.EscapeString(o.name), src, chk)
	layer = fmt.Sprintf(`<maplayer type="raster" hasScaleBasedVisibilityFlag="0" autoRefreshMode="Disabled" styleCategories="AllStyleCategories"><id>%s</id><datasource>%s</datasource><layername>%s</layername><srs>%s</srs><resourceMetadata><rights>%s</rights></resourceMetadata><provider>wms</provider><flags><Identifiable>0</Identifiable><Removable>1</Removable><Searchable>0</Searchable><Private>0</Private></flags><customproperties><Option/></customproperties><pipe><provider><resampling enabled="false" maxOversampling="2" zoomedInResamplingMethod="nearestNeighbour" zoomedOutResamplingMethod="nearestNeighbour"/></provider><rasterrenderer type="singlebandcolordata" opacity="%.2f" alphaBand="-1" band="1" nodataColor=""><rasterTransparency/><minMaxOrigin><limits>None</limits><extent>WholeRaster</extent><statAccuracy>Estimated</statAccuracy><cumulativeCutLower>0.02</cumulativeCutLower><cumulativeCutUpper>0.98</cumulativeCutUpper><stdDevFactor>2</stdDevFactor></minMaxOrigin></rasterrenderer><brightnesscontrast brightness="0" contrast="0" gamma="1"/>%s<rasterresampler maxOversampling="2"/><resamplingStage>resamplingFilter</resamplingStage></pipe><blendMode>%d</blendMode></maplayer>`,
		id, src, html.EscapeString(o.name), neQGSCRS3857, html.EscapeString(o.rights), o.opacity, hue, o.blend)
	return
}

// neQGSOrthoURL — basemap.at's orthophoto, the landing page's ;ortho
// (WMTS bmaporthofoto30cm, google3857 — note {y}/{x} order, CC BY 4.0).
const neQGSOrthoURL = "https://mapsneu.wien.gv.at/basemap/bmaporthofoto30cm/normal/google3857/{z}/{y}/{x}.jpeg"

// neQGSVector — one GeoJSON-over-HTTP vector layer (QGIS "ogr" provider reads
// the URL directly): tree apices from the observed-layer peer, OSM lines and
// water from this service. One fetch per layer on open — only offered for a
// box small enough (neLayerTreesMaxKm2), like the page's overlays
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

// neQGSHexShade — the page's shade as a data-defined fill: the group colour at
// the alpha of its dominance step (neGlyphPaintLU: four steps from a third to
// the whole cell, neGlyphTileAlphaMin … 1). share = the field holding the
// dominant group's share in percent.
func neQGSHexShade(rgb, share string) string {
	ex := fmt.Sprintf(`set_color_part('%s','alpha',255*(%.2f+%.2f*clamp(0,floor((coalesce(%s,100)/100-1/3)*6),3)/3))`, rgb, neGlyphTileAlphaMin, 1-neGlyphTileAlphaMin, share)
	prop := func(n string) string {
		return `<Option type="Map" name="` + n + `"><Option type="bool" name="active" value="true"/><Option type="QString" name="expression" value="` + html.EscapeString(ex) + `"/><Option type="int" name="type" value="3"/></Option>`
	}
	return `<data_defined_properties><Option type="Map"><Option type="QString" name="name" value=""/><Option type="Map" name="properties">` + prop("fillColor") + prop("outlineColor") + `</Option><Option type="QString" name="type" value="collection"/></Option></data_defined_properties>`
}

// neQGSHexStyles — the legend of the hexagon layer (cells.geojson at res): the
// cell layer's rules that exist at that resolution, the group fills shaded by
// share like the page's picture. Which rules start ticked is the page's state
// (declared / observed profile, change, mismatch).
// neQGSHexShaded — a rule's symbol with the shade, if the rule is a group fill.
// res 0 = tiles (cells and aggregates in one style: whichever share field is there).
func neQGSHexShaded(x neQGSRule, res int) string {
	for i, g := range neGlyphGroups {
		c := neGlyphGroupRGB[i]
		rgb := fmt.Sprintf("%d,%d,%d", c[0], c[1], c[2])
		for _, pf := range [][3]string{{"declared · ", "dsh", "d_pct"}, {"observed · ", "osh", "o_pct"}} {
			if x.name == pf[0]+neQGSGroupLabel[i]+" ("+g+")" {
				share := `"` + pf[1] + `"`
				if res == 0 {
					share += `,"` + pf[2] + `"`
				} else if res < 12 {
					share = `"` + pf[2] + `"`
				}
				return strings.Replace(x.sym, `</Option></layer>`, `</Option>`+neQGSHexShade(rgb, share)+`</layer>`, 1)
			}
		}
	}
	return x.sym
}

// neQGSVeilTileStyle — the veil tiles: one entry, the page's paper at 82 %.
func neQGSVeilTileStyle() string {
	return fmt.Sprintf(`<renderer type="basic"><styles><style enabled="1" name="veiled by the filter" layer="%s" geometry="2" expression="" min-zoom="12" max-zoom="%d"><symbols>%s</symbols></style></styles></renderer>`,
		neMVTLayerName, neQGSStyleMaxZ, neQGSFill("0", "250,250,247,209", "250,250,247,209", "0.1"))
}

func neQGSHexStyles(v neQGSView, res int) string {
	z := res + 2
	var r, sy strings.Builder
	r.WriteString(`<renderer-v2 type="RuleRenderer" symbollevels="0" forceraster="0" enableorderby="0" referencescale="-1"><rules key="{00000000-0000-4000-8000-0000000d0000}">`)
	k := 0
	for _, x := range neQGSRules(v, false) {
		if x.minz > z || x.maxz < z || strings.HasPrefix(x.name, "trees · tallest") {
			continue
		}
		chk := ""
		if !x.on {
			chk = ` checkstate="0"`
		}
		sym := strings.Replace(neQGSHexShaded(x, res), `name="0"`, fmt.Sprintf(`name="%d"`, k), 1)
		fmt.Fprintf(&r, `<rule filter="%s" symbol="%d" key="{00000000-0000-4000-8000-0000000d%04d}" label="%s"%s/>`, html.EscapeString(x.expr), k, k+1, html.EscapeString(x.name), chk)
		sy.WriteString(sym)
		k++
	}
	r.WriteString(`</rules><symbols>` + sy.String() + `</symbols></renderer-v2>`)
	return r.String()
}

// neQGSVeilStyle — the page's veil: its paper colour at 82 % over what the filter leaves out.
func neQGSVeilStyle() string {
	return `<renderer-v2 type="singleSymbol" symbollevels="0" forceraster="0" enableorderby="0" referencescale="-1"><symbols>` + neQGSFill("0", "250,250,247,209", "250,250,247,209", "0.1") + `</symbols></renderer-v2>`
}

// neQGSHexExtras — what makes the hexagon layer pleasant to read: a field
// alias per catalogue field (its label and unit, shown by identify and the
// attribute table), the cell id as the feature title, a map tip.
func neQGSHexExtras(res int) string {
	var b strings.Builder
	b.WriteString(`<aliases>`)
	al := func(f, n string) { fmt.Fprintf(&b, `<alias field="%s" name="%s"/>`, f, html.EscapeString(n)) }
	al("h3", "H3 cell id")
	al("res", "H3 resolution")
	al("match", "passes the filter")
	if res == 12 {
		for i := range neFiltFields {
			fd := &neFiltFields[i]
			if fd.Col != "" || fd.Label == "" {
				continue
			}
			n := fd.ID + " · " + fd.Label
			if fd.Unit != "" {
				n += " (" + fd.Unit + ")"
			}
			al(fd.ID, n)
			if fd.Kind == nfBucket {
				al(fd.ID+"_min", fd.ID+"_min · "+fd.Label+", lower bound of the band")
			}
		}
		al("cont_open", "patch runs on beyond what was counted (cont is a lower bound)")
		al("area_m2", "cell area (m²)")
		al("lon", "centre longitude")
		al("lat", "centre latitude")
	} else {
		for _, a := range [][2]string{{"n", "cells (res 12) inside"}, {"n_all", "cells inside, matching or not"}, {"match_pct", "share of the cells that pass the filter (%)"}, {"area_ha", "area (ha)"},
			{"d", "d · dominant declared use"}, {"d_pct", "its share of the cells (%)"}, {"d_known_pct", "cells with a declared use (%)"},
			{"o", "o · dominant observed use"}, {"o_pct", "its share of the cells (%)"}, {"o_known_pct", "cells with an observed use (%)"},
			{"diff_pct", "declared ≠ observed (% of the cells)"}, {"chg_conf", "cells with a confirmed change"}, {"chg_cand", "cells with a candidate change"}, {"chg_pct", "changed cells (%)"}, {"sub_pct", "cells under a subsidy claim (%)"}} {
			al(a[0], a[1])
		}
	}
	al("srtm_epoch", "observed layer: epoch")
	al("sub_year", "subsidy claims: year")
	al("sub_release", "subsidy claims: release")
	b.WriteString(`</aliases><previewExpression>"h3"</previewExpression>`)
	tip := `<b>[% coalesce("d",'no declared use') %]</b> declared · <b>[% coalesce("o",'not observed') %]</b> observed[% if("chg" in ('conf','cand'), '<br/>change: ' || "chg", '') %]<br/><small>[% "h3" %]</small>`
	b.WriteString(`<mapTip enabled="1">` + html.EscapeString(tip) + `</mapTip>`)
	return b.String()
}

// neQGSHexLayer — a cells.geojson layer (the hexagons, or their veil).
func neQGSHexLayer(id, name, url, abstract, rights, renderer, extras, opacity string) (tree, layer string) {
	id = neQGSID(id)
	src := html.EscapeString(url)
	tree = fmt.Sprintf(`<layer-tree-layer id="%s" name="%s" source="%s" providerKey="ogr" checked="Qt::Unchecked" expanded="0" legend_exp="" patch_size="-1,-1"><customproperties><Option/></customproperties></layer-tree-layer>`, id, html.EscapeString(name), src)
	layer = fmt.Sprintf(`<maplayer type="vector" geometry="Polygon" hasScaleBasedVisibilityFlag="0" autoRefreshMode="Disabled" styleCategories="AllStyleCategories" readOnly="1"><id>%s</id><datasource>%s</datasource><layername>%s</layername><srs>%s</srs><resourceMetadata><title>%s</title><abstract>%s</abstract><rights>%s</rights></resourceMetadata><provider encoding="UTF-8">ogr</provider><flags><Identifiable>1</Identifiable><Removable>1</Removable><Searchable>1</Searchable><Private>0</Private></flags><customproperties><Option/></customproperties>%s<blendMode>0</blendMode><layerOpacity>%s</layerOpacity><featureBlendMode>0</featureBlendMode>%s</maplayer>`,
		id, src, html.EscapeString(name), neQGSCRS4326, html.EscapeString(name), html.EscapeString(abstract), html.EscapeString(rights), renderer, opacity, extras)
	return
}

func neQGSLine(name, color, width, style string) string {
	return `<symbol type="line" name="` + name + `" alpha="1" clip_to_extent="1" force_rhr="0"><layer class="SimpleLine" enabled="1" locked="0" pass="0"><Option type="Map"><Option type="QString" name="line_color" value="` + color + `"/><Option type="QString" name="line_width" value="` + width + `"/><Option type="QString" name="line_width_unit" value="MM"/><Option type="QString" name="line_style" value="` + style + `"/><Option type="QString" name="capstyle" value="round"/><Option type="QString" name="joinstyle" value="round"/></Option></layer></symbol>`
}

func neQGSBoxKm2(b [4]float64) float64 {
	la := (b[1] + b[3]) / 2 * math.Pi / 180
	return (b[2] - b[0]) * 111.32 * math.Cos(la) * (b[3] - b[1]) * 110.57
}

// neQGSFiltHt — the tree-height window of a filter (`ht=27..34`, `ht>=20`) as
// the trees layer's `ht=lo..hi`; "" when the filter has no single ht term.
func neQGSFiltHt(canon string) string {
	if canon == "" {
		return ""
	}
	for _, g := range neFiltParse(canon).Groups {
		if len(g) != 1 || g[0].F != "ht" || g[0].Flag || g[0].Unk || (math.IsNaN(g[0].Lo) && math.IsNaN(g[0].Hi)) {
			continue
		}
		lo, hi := "", ""
		if !math.IsNaN(g[0].Lo) {
			lo = strconv.FormatFloat(g[0].Lo, 'g', -1, 64)
		}
		if !math.IsNaN(g[0].Hi) {
			hi = strconv.FormatFloat(g[0].Hi, 'g', -1, 64)
		}
		return lo + ".." + hi
	}
	return ""
}

// neQGSSpec — what a project is built from.
type neQGSSpec struct {
	base    string     // this service, as the caller reached it
	box     [4]float64 // the view
	name    string     // file-name part
	canon   string     // filter
	share   string     // the page's hash
	key     string     // the caller's key ("" = anonymous)
	archive string     // "" = live cell tiles; else the file name of a PMTiles archive next to the project
	maxZ    int        // the archive's deepest zoom
}

// neQGSProject — the project XML. Every layer of ours — cells, tree crowns,
// subsidy Schläge, roads ink, relief — is fetched from THIS service with the
// caller's key, so one identity is counted for the whole map; only the two
// third-party basemaps (basemap.at photo, OpenStreetMap) are asked directly.
func neQGSProject(sp neQGSSpec) (xml string, lname string) {
	view := neQGSViewOf(sp.share)
	base, box := sp.base, sp.box
	// q("…/x.pbf", "f=…") — our URL with the caller's key
	q := func(u string, params ...string) string {
		if sp.key != "" {
			params = append(params, "key="+url.QueryEscape(sp.key))
		}
		ps := params[:0]
		for _, p := range params {
			if p != "" {
				ps = append(ps, p)
			}
		}
		if len(ps) == 0 {
			return u
		}
		return u + "?" + strings.Join(ps, "&")
	}
	lname = "umfeld-at cells"
	fq := ""
	if sp.canon != "" {
		fq = "f=" + url.QueryEscape(sp.canon)
		lname += " · " + neFiltParse(sp.canon).Words()
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
	ext := fmt.Sprintf(`<extent><xmin>%.2f</xmin><ymin>%.2f</ymin><xmax>%.2f</xmax><ymax>%.2f</ymax></extent>`, x0, y0, x1, y1)

	// the page's stack, bottom → top: photo · cells · relief (overlay blend) ·
	// subsidy · roads · trees
	type bm struct {
		tree, layer, id string
		grp             bool // inside the "this view as vector data" group
	}
	var bms, over, vec []bm // under the cells / over them, each top → bottom; vec = the vector group
	km2 := neQGSBoxKm2(box)
	bb := fmt.Sprintf("%.5f,%.5f,%.5f,%.5f", box[0], box[1], box[2], box[3])
	if pr.ObsAPI != "" {
		ht := neQGSFiltHt(sp.canon)
		tn := "Trees · crowns by height"
		tp := "Trees as points"
		if ht != "" {
			w := ""
			switch lo, hi, _ := strings.Cut(ht, ".."); {
			case hi == "":
				w = " · ≥ " + lo + " m"
			case lo == "":
				w = " · ≤ " + hi + " m"
			default:
				w = " · " + lo + "–" + hi + " m"
			}
			tn, tp = tn+w, tp+w
			ht = "ht=" + ht
		}
		// on the map: the live tiles — the page's rings wherever the reader pans
		t, l := neQGSTileLayer(neQGSTileOpt{id: "trees", name: tn, url: q(base+"/api/v1/ne/mvt/trees/{z}/{x}/{y}.pbf", ht), zmin: neLayerTreesZ, zmax: neLayerTreesZ,
			rights: pr.ObsName + " (CC BY 4.0), BEV", renderer: neQGSTreeStyles(), checked: view.trees, opacity: "0.9", minScale: 34000,
			abstract: "Every tree found by LiDAR as a ring the size of its crown, coloured by height — as on the map. Live tiles: they load wherever the map goes. Visible below 1:34 000. Click a ring with the identify tool: h (m), crown_d (m), crown_m2, species, vitality."})
		over = append(over, bm{tree: t, layer: l, id: "trees"})
		// for analysis: the view and a margin around it in one fetch → a point
		// layer with a graduated renderer (re-rampable), in the vector group
		mx, my := (box[2]-box[0])*0.25, (box[3]-box[1])*0.25
		tb := [4]float64{box[0] - mx, box[1] - my, box[2] + mx, box[3] + my}
		if neQGSBoxKm2(tb) > neLayerTreesMaxKm2 {
			tb = box
		}
		if neQGSBoxKm2(tb) <= neLayerTreesMaxKm2 && sp.archive == "" {
			t, l := neQGSVector("treepts", tp, q(base+"/api/v1/ne/mvt/trees.geojson", fmt.Sprintf("bbox=%.5f,%.5f,%.5f,%.5f", tb[0], tb[1], tb[2], tb[3]), ht), "Point", pr.ObsName+" (CC BY 4.0), BEV", neQGSTreeGraduated(), false, "0.9", 34000)
			vec = append(vec, bm{tree: t, layer: l, id: "treepts", grp: true})
		}
	}
	t, l := neQGSRaster(neQGSRasterOpt{id: "roadsink", name: "Roads and rail", url: q(base + "/api/v1/ne/mvt/pic/roads/{z}/{x}/{y}.png"), rights: "© OpenStreetMap contributors (ODbL)",
		zmin: neLayerPicMinZ, zmax: neLayerPicMaxZ, opacity: view.roadsOp, checked: view.roads, hidpi: true})
	over = append(over, bm{tree: t, layer: l, id: "roadsink"})
	if pr.SubName != "" {
		pv, pn := "sub", "Subsidy claims (INVEKOS)"
		if view.change {
			pv, pn = "subc", "Subsidy claims · changes"
		}
		t, l := neQGSRaster(neQGSRasterOpt{id: "subsidy", name: pn, url: q(base + "/api/v1/ne/mvt/pic/" + pv + "/{z}/{x}/{y}.png"), rights: neSubAttribution,
			zmin: neLayerPicMinZ, zmax: neLayerPicMaxZ, opacity: 1, checked: view.sub, hidpi: true})
		over = append(over, bm{tree: t, layer: l, id: "subsidy"})
	}
	if pr.ObsWeb != "" {
		t, l := neQGSRaster(neQGSRasterOpt{id: "relief", name: "Terrain relief", url: q(base + "/api/v1/ne/mvt/relief/{z}/{x}/{y}.png"), rights: pr.ObsName + " (CC BY 4.0), BEV",
			zmin: neLayerReliefMinZ, zmax: neLayerReliefMaxZ, opacity: 0.45, checked: view.relief, blend: 8})
		over = append(over, bm{tree: t, layer: l, id: "relief"})
	}
	// the page's own cell picture (dominant group, shade = dominance, second
	// group as an inner hexagon): under the vector cells, which then carry the
	// outlines, the attributes and — with a filter — the matching cells
	otf := 1.0
	if view.ortho {
		otf = 1 - 0.65*view.orthoOp
	}
	dop, oop := 1.0, 1.0
	switch {
	case view.obs && (view.change || view.diff):
		dop, oop = 0.3, 0.4
	case view.obs && view.sub:
		dop, oop = 0.3, 0.8
	case view.obs:
		dop = 0.3
	case view.change || view.diff:
		dop = 0.4
	case view.sub:
		dop = 0.8
	}
	var pics []bm // above the vector cells' basemaps, below the cells
	if pr.ObsAPI != "" {
		t, l := neQGSRaster(neQGSRasterOpt{id: "picobs", name: "Land use · observed (LiDAR)", url: q(base + "/api/v1/ne/mvt/pic/observed/{z}/{x}/{y}.png"), rights: pr.ObsName + " (CC BY 4.0), BEV",
			zmin: neLayerPicMinZ, zmax: neLayerPicMaxZ, opacity: oop * otf, checked: view.obs && sp.archive == "", hidpi: true})
		pics = append(pics, bm{tree: t, layer: l, id: "picobs"})
	}
	t, l = neQGSRaster(neQGSRasterOpt{id: "picdecl", name: "Land use · declared (register)", url: q(base + "/api/v1/ne/mvt/pic/declared/{z}/{x}/{y}.png"), rights: BEVNotice(),
		zmin: neLayerPicMinZ, zmax: neLayerPicMaxZ, opacity: dop * otf, checked: sp.archive == "", hidpi: true})
	pics = append(pics, bm{tree: t, layer: l, id: "picdecl"})
	// with a filter: the page's veil — the complement of the filter as live
	// tiles, the paper colour over every cell that does not pass, right under
	// the cell layer's outlines. A veiled cell can be identified like any other.
	// And for analysis, one collapsed group: this view as plain vector layers
	// (cells.geojson / trees.geojson, one fetch each) — what the attribute table
	// and the processing tools (intersection, …) accept; tiles cannot be their input.
	if sp.archive == "" {
		rights := BEVNotice() + " · " + pr.ObsName + " · " + pr.SubName
		docs := " Field meanings: " + base + "/api/v1/docs/qgis-layer.md"
		words := ""
		if sp.canon != "" {
			words = neFiltParse(sp.canon).Words()
			t, l := neQGSTileLayer(neQGSTileOpt{id: "veilt", name: "Filter veil", url: q(base+"/api/v1/ne/mvt/{z}/{x}/{y}.pbf", fq, "veil=1"), zmin: 12, zmax: neMVTMaxZ,
				rights: rights, renderer: neQGSVeilTileStyle(), opacity: "1", minScale: 140000, checked: true,
				abstract: "The cells that do NOT pass the filter (" + words + "), whitened as on the map. Live tiles (zoom 12+), they follow the map. Click a veiled cell with the identify tool to see its values and why it fails." + docs})
			bms = append(bms, bm{tree: t, layer: l, id: "veilt"})
		}
		if hres := neCellsJSONRes(km2); hres > 0 {
			what := map[int]string{11: " · 25 m", 10: " · 66 m"}[hres] // coarser than the cells themselves
			hn := "Cells as polygons" + what
			if sp.canon != "" {
				hn = "Cells in the filter" + what
			}
			t, l := neQGSHexLayer("hex", hn, q(base+"/api/v1/ne/mvt/cells.geojson", "bbox="+bb, fq),
				"The cells of this view as a plain vector layer (one fetch, it does not grow when you pan) with every attribute: attribute table, select by expression, and the processing tools — intersection, join by location, statistics — which do not accept tiles."+docs, rights, neQGSHexStyles(view, hres), neQGSHexExtras(hres), "1")
			vec = append(vec, bm{tree: t, layer: l, id: "hex", grp: true})
			if sp.canon != "" {
				t, l := neQGSHexLayer("veil", "Cells outside the filter"+what, q(base+"/api/v1/ne/mvt/cells.geojson", "bbox="+bb, fq, "veil=1"),
					"The cells of this view that do NOT pass the filter ("+words+"). Together with the matching cells they tile the view: use this layer as the overlay of a Difference, or the matching cells as the overlay of an Intersection."+docs, rights, neQGSVeilStyle(), neQGSHexExtras(hres), "1")
				vec = append(vec, bm{tree: t, layer: l, id: "veil", grp: true})
			}
		}
		bms = append(bms, vec...)
	}
	bms = append(bms, pics...) // under the water outlines, over the photo
	t, l = neQGSRaster(neQGSRasterOpt{id: "ortho", name: "Aerial photo", url: neQGSOrthoURL, rights: "basemap.at, CC BY 4.0", zmax: 19, opacity: view.orthoOp, checked: view.ortho})
	bms = append(bms, bm{tree: t, layer: l, id: "ortho"})
	t, l = neQGSRaster(neQGSRasterOpt{id: "osm", name: "Base map (OpenStreetMap)", url: "https://tile.openstreetmap.org/{z}/{x}/{y}.png", rights: "© OpenStreetMap contributors", zmax: 19, opacity: 1, checked: !view.ortho, grey: true})
	bms = append(bms, bm{tree: t, layer: l, id: "osm"})

	// the vector cells: with the pictures underneath they paint the aggregates
	// farther out, the outlines and — filtered or offline — the cells themselves
	pic := sp.canon == "" && sp.archive == ""
	cop := otf
	if view.obs {
		cop *= oop
	} else {
		cop *= dop
	}
	abstract := "H3 cells of the declared/observed land-use product with change, subsidy and LiDAR statistics. res 7 at z 9 … res 12 (the cell itself, every attribute) at z 14+; aggregates below. Legend entries are the landing page's toggles (declared / observed profile, change, mismatch, trees) — tick them on and off. Field meanings: " + base + "/api/v1/docs/qgis-layer.md"
	meta := fmt.Sprintf(`<resourceMetadata><title>%s</title><abstract>%s&#10;&#10;%s</abstract><rights>%s</rights><links><link name="docs" type="information" url="%s/api/v1/docs/qgis-layer.md"/></links></resourceMetadata>`,
		html.EscapeString(lname), html.EscapeString(abstract), attrib, attrib, base)
	cid := neQGSID("cells")
	var ctree, clayer string
	cname := "Cells · click for details"
	if sp.canon != "" {
		cname = "Cells in the filter · click for details"
	}
	if sp.archive == "" {
		src := html.EscapeString(fmt.Sprintf("type=xyz&url=%s&zmin=%d&zmax=%d", neQGSEnc(q(base+"/api/v1/ne/mvt/{z}/{x}/{y}.pbf", fq)), neMVTMinZ, neMVTMaxZ))
		ctree = fmt.Sprintf(`<layer-tree-layer id="%s" name="%s" source="%s" providerKey="xyzvectortiles" checked="Qt::Checked" expanded="0" legend_exp="" patch_size="-1,-1"><customproperties><Option/></customproperties></layer-tree-layer>`, cid, html.EscapeString(cname), src)
		clayer = fmt.Sprintf(`<maplayer type="vector-tile" hasScaleBasedVisibilityFlag="0" autoRefreshMode="Disabled" styleCategories="AllStyleCategories"><id>%s</id><datasource>%s</datasource><layername>%s</layername><srs>%s</srs>`, cid, src, html.EscapeString(cname), neQGSCRS3857) + meta +
			`<provider>xyzvectortiles</provider><map-layer-style-manager current="default"><map-layer-style name="default"/></map-layer-style-manager><flags><Identifiable>1</Identifiable><Removable>1</Removable><Searchable>1</Searchable><Private>0</Private></flags><customproperties><Option/></customproperties>` +
			neQGSTileStyles(view, pic) + fmt.Sprintf(`<blendMode>0</blendMode><layerOpacity>%.2f</layerOpacity></maplayer>`, cop)
	} else {
		// the archive next to the project, read by GDAL's PMTiles driver (its deepest zoom)
		lname += " (offline archive)"
		// CLIP=NO: GDAL otherwise cuts every hexagon at its tile's edge, and the cut
		// shows as a seam along the tile grid under any outline
		src := html.EscapeString("./" + sp.archive + "|option:CLIP=NO|layername=" + neMVTLayerName)
		ctree = fmt.Sprintf(`<layer-tree-layer id="%s" name="%s" source="%s" providerKey="ogr" checked="Qt::Checked" expanded="1" legend_exp="" patch_size="-1,-1"><customproperties><Option/></customproperties></layer-tree-layer>`, cid, html.EscapeString(lname), src)
		clayer = fmt.Sprintf(`<maplayer type="vector" geometry="Polygon" hasScaleBasedVisibilityFlag="0" autoRefreshMode="Disabled" styleCategories="AllStyleCategories" readOnly="1"><id>%s</id><datasource>%s</datasource><layername>%s</layername><srs>%s</srs>`, cid, src, html.EscapeString(lname), neQGSCRS3857) + meta +
			`<provider encoding="UTF-8">ogr</provider><flags><Identifiable>1</Identifiable><Removable>1</Removable><Searchable>1</Searchable><Private>0</Private></flags><customproperties><Option/></customproperties>` +
			neQGSOGRStyles(view, sp.maxZ) + fmt.Sprintf(`<blendMode>0</blendMode><layerOpacity>%.2f</layerOpacity><featureBlendMode>0</featureBlendMode></maplayer>`, cop)
	}

	var b strings.Builder
	b.WriteString(`<!DOCTYPE qgis PUBLIC 'http://mrcc.com/qgis.dtd' 'SYSTEM'>` + "\n")
	fmt.Fprintf(&b, `<qgis version="3.34" projectname="%s">`, html.EscapeString(lname+" · "+sp.name))
	b.WriteString(`<title>` + html.EscapeString(lname) + `</title><projectCrs>` + neQGSCRS3857 + `</projectCrs>`)
	b.WriteString(`<mapcanvas name="theMapCanvas" annotationsVisible="1"><units>meters</units>` + ext + `<rotation>0</rotation><destinationsrs>` + neQGSCRS3857 + `</destinationsrs><rendermaptile>0</rendermaptile></mapcanvas>`)
	b.WriteString(`<layer-tree-group><customproperties><Option/></customproperties>`)
	for _, m := range over {
		b.WriteString(m.tree)
	}
	b.WriteString(ctree)
	ingrp := false
	for _, m := range bms {
		if m.grp != ingrp {
			if ingrp = m.grp; ingrp {
				b.WriteString(`<layer-tree-group name="` + neQGSVecGroup + `" checked="Qt::Checked" expanded="0"><customproperties><Option/></customproperties>`)
			} else {
				b.WriteString(`</layer-tree-group>`)
			}
		}
		b.WriteString(m.tree)
	}
	if ingrp {
		b.WriteString(`</layer-tree-group>`)
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
	b.WriteString(`<projectlayers>` + clayer)
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
	b.WriteString(`<properties><Gui><CanvasColorRedPart type="int">250</CanvasColorRedPart><CanvasColorGreenPart type="int">250</CanvasColorGreenPart><CanvasColorBluePart type="int">247</CanvasColorBluePart></Gui><Measurement><AreaUnits type="QString">ha</AreaUnits><DistanceUnits type="QString">meters</DistanceUnits></Measurement><Paths><Absolute type="bool">false</Absolute></Paths><SpatialRefSys><ProjectionsEnabled type="int">1</ProjectionsEnabled></SpatialRefSys></properties>`)
	fmt.Fprintf(&b, `<projectMetadata><title>%s</title><abstract>%s</abstract><author>umfeld-at</author></projectMetadata>`, html.EscapeString(lname), attrib)
	b.WriteString(`<ProjectViewSettings rotation="0" UseProjectScales="0"><Scales/><DefaultViewExtent xmin="` + fmt.Sprintf("%.2f", x0) + `" ymin="` + fmt.Sprintf("%.2f", y0) + `" xmax="` + fmt.Sprintf("%.2f", x1) + `" ymax="` + fmt.Sprintf("%.2f", y1) + `">` + neQGSCRS3857 + `</DefaultViewExtent></ProjectViewSettings>`)
	b.WriteString(`</qgis>` + "\n")
	return b.String(), lname
}

// neQGSVecGroup — the collapsed group of one-fetch vector layers.
const neQGSVecGroup = "Vector data of this view (analysis)"

// neQGSTileOpt / neQGSTileLayer — a vector-tile layer of ours other than the cells (the tree crowns).
type neQGSTileOpt struct {
	id, name, url, rights, renderer, abstract, opacity string
	zmin, zmax, minScale                               int
	checked                                            bool
}

func neQGSTileLayer(o neQGSTileOpt) (tree, layer string) {
	id := neQGSID(o.id)
	src := html.EscapeString(fmt.Sprintf("type=xyz&url=%s&zmin=%d&zmax=%d", neQGSEnc(o.url), o.zmin, o.zmax))
	chk := "Qt::Unchecked"
	if o.checked {
		chk = "Qt::Checked"
	}
	scale := `hasScaleBasedVisibilityFlag="0"`
	if o.minScale > 0 {
		scale = fmt.Sprintf(`hasScaleBasedVisibilityFlag="1" maxScale="1" minScale="%d"`, o.minScale)
	}
	tree = fmt.Sprintf(`<layer-tree-layer id="%s" name="%s" source="%s" providerKey="xyzvectortiles" checked="%s" expanded="0" legend_exp="" patch_size="-1,-1"><customproperties><Option/></customproperties></layer-tree-layer>`, id, html.EscapeString(o.name), src, chk)
	layer = fmt.Sprintf(`<maplayer type="vector-tile" %s autoRefreshMode="Disabled" styleCategories="AllStyleCategories"><id>%s</id><datasource>%s</datasource><layername>%s</layername><srs>%s</srs><resourceMetadata><title>%s</title><abstract>%s</abstract><rights>%s</rights></resourceMetadata><provider>xyzvectortiles</provider><map-layer-style-manager current="default"><map-layer-style name="default"/></map-layer-style-manager><flags><Identifiable>1</Identifiable><Removable>1</Removable><Searchable>0</Searchable><Private>0</Private></flags><customproperties><Option/></customproperties>%s<blendMode>0</blendMode><layerOpacity>%s</layerOpacity></maplayer>`,
		scale, id, src, html.EscapeString(o.name), neQGSCRS3857, html.EscapeString(o.name), html.EscapeString(o.abstract), html.EscapeString(o.rights), o.renderer, o.opacity)
	return
}

// neQGSSpecOf — the project a request asks for (box, name, filter, hash, key).
func neQGSSpecOf(r *http.Request) (sp neQGSSpec, msg string, code int) {
	q := neQuery(r)
	box, name, canon, msg, code := nePMTBox(r)
	if msg != "" {
		return sp, msg, code
	}
	share := q.Get("share")
	if v := q.Get("view"); v != "" { // the archive's unit is share=/bbox=; view= is the page state to style it by
		share = v
	}
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
	sp = neQGSSpec{base: publicBaseURL(r), box: box, name: name, canon: canon, share: share}
	if k := exportKeyOf(r); k != "" && len(k) <= 64 {
		sp.key = k
	}
	return sp, "", 0
}

// handleNEMVTProject — /api/v1/ne/mvt.qgs
func handleNEMVTProject(w http.ResponseWriter, r *http.Request) {
	sp, msg, code := neQGSSpecOf(r)
	if msg != "" {
		apiError(w, msg, code)
		return
	}
	xml, _ := neQGSProject(sp)
	neCellsJSONPrepare(sp.box) // the hexagon layer's bins: ready by the time the file is opened
	fn := "umfeld-at-cells-" + sp.name
	if sp.canon != "" {
		fn += "-filtered"
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/x-qgis-project; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+fn+`.qgs"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(200)
	w.Write([]byte(xml))
	exportAccountFor(r, len(xml))
}

const neQGSDoctype = `<!DOCTYPE qgis PUBLIC 'http://mrcc.com/qgis.dtd' 'SYSTEM'>` + "\n"

// neQGSStyleQML — the cell style as a sidecar: tiles = the vector-tile
// renderer, else the rule renderer for the archive opened through GDAL.
func neQGSStyleQML(tiles bool, share string, maxZ int) string {
	v := neQGSViewOf(share)
	if tiles {
		return neQGSDoctype + `<qgis version="3.34" styleCategories="Symbology">` + neQGSTileStyles(v, false) + `</qgis>` + "\n"
	}
	return neQGSDoctype + `<qgis version="3.34" styleCategories="Symbology">` + neQGSOGRStyles(v, maxZ) + `<layerGeometryType>2</layerGeometryType></qgis>` + "\n"
}

// handleNEMVTStyle — /api/v1/ne/mvt.qml?for=tiles|ogr[&share=][&maxz=]
func handleNEMVTStyle(w http.ResponseWriter, r *http.Request) {
	q := neQuery(r)
	kind := q.Get("for")
	maxZ := 14
	if v, err := strconv.Atoi(q.Get("maxz")); err == nil && v >= neMVTMinZ && v <= neMVTMaxZ {
		maxZ = v
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="umfeld-at-cells.qml"`)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(200)
	w.Write([]byte(neQGSStyleQML(!(kind == "ogr" || kind == "pmtiles"), q.Get("share"), maxZ)))
}

// neQGSBundle — the archive as something QGIS opens styled: a zip holding
//
//	<fn>.pmtiles   the archive
//	<fn>.qml       its style — QGIS applies a .qml of the same name when the archive is added or dragged in
//	<fn>.qgs       the project: the archive as the cell layer (relative path), the live companions around it
//	LIESMICH.txt   three lines
//
// Stored, not deflated: the tiles inside are gzip'd already.
func neQGSBundle(w http.ResponseWriter, r *http.Request, path, fn string, maxZ int) {
	f, err := os.Open(path)
	if err != nil {
		apiError(w, "archive gone — request again", 404)
		return
	}
	defer f.Close()
	sp, msg, code := neQGSSpecOf(r)
	if msg != "" {
		apiError(w, msg, code)
		return
	}
	sp.archive, sp.maxZ = fn+".pmtiles", maxZ
	xml, _ := neQGSProject(sp)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+fn+`.zip"`)
	w.Header().Set("Cache-Control", "no-store")
	cw := &countWriter{w: w}
	zw := zip.NewWriter(cw)
	now := time.Now()
	put := func(name string, body io.Reader) {
		h := &zip.FileHeader{Name: fn + "/" + name, Method: zip.Store, Modified: now}
		if zf, err := zw.CreateHeader(h); err == nil {
			io.Copy(zf, body)
		}
	}
	put(fn+".pmtiles", f)
	put(fn+".qml", strings.NewReader(neQGSStyleQML(false, sp.share, maxZ)))
	put(fn+".qgs", strings.NewReader(xml))
	put("LIESMICH.txt", strings.NewReader("umfeld-at cells, offline archive for QGIS (3.34 or later, GDAL 3.8 or later)\n\n"+
		"1. Unpack the folder; keep the three files together.\n"+
		"2. Double-click "+fn+".qgs: the map as on the page, the cells read from the archive.\n"+
		"   The other layers (tree crowns, subsidy claims, roads, relief, photo) are live and need a connection.\n"+
		"3. Or drag "+fn+".pmtiles into any project: the .qml of the same name styles it.\n\n"+
		"Fields: "+sp.base+"/api/v1/docs/qgis-layer.md\n"+BEVNotice()+"\n"))
	zw.Close()
	exportAccountFor(r, int(cw.n))
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
