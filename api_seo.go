package main

// SEO surface of the public tier: robots.txt, sitemap.xml, the Open Graph
// image, and the <head> meta block of the landing page. Everything is static
// text with {PUBLIC_URL} placeholders resolved by peerText at serve time
// (absolute URLs are mandatory for og:image / canonical / sitemap <loc>).
//
// The OG image (data/og_image.jpg, 1200×630) is a capture of the landing glyph:
// KG 63330 Kohlschwarz in the observed profile with trees and relief
// (/#kg=63330;v=obs;full;trees;relief;z=60). Regenerate with the same link when
// the glyph's look changes; keep it ≤ 300 KB.

import (
	_ "embed"
	"net/http"
	"strings"
)

//go:embed data/og_image.jpg
var ogImageJPG []byte

// seoDescription — one sentence for <meta name=description> and og:description.
const seoDescription = "Austria as H3 cells: declared land use from the BEV cadastre, the use LiDAR and satellite observe, " +
	"and the change between digests, cell by cell. Plus the context of any point — municipality, land price, Natura 2000, " +
	"OpenStreetMap, place names, law references. Open JSON API, CORS, no key."

// seoTitle — shared by <title>, og:title and the sitemap comment.
const seoTitle = "umfeld-at — context layers around a place in Austria"

// seoHeadMeta — spliced into the landing page <head> after <title>.
const seoHeadMeta = `<meta name="description" content="` + seoDescription + `">
<link rel="canonical" href="{PUBLIC_URL}/">
<link rel="icon" href="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'%3E%3Cpath fill='%232e7d32' d='M16 2l12 7v14l-12 7L4 23V9z'/%3E%3C/svg%3E">
<meta name="theme-color" content="#2e7d32">
<meta property="og:type" content="website">
<meta property="og:site_name" content="umfeld-at">
<meta property="og:locale" content="en_GB">
<meta property="og:locale:alternate" content="de_AT">
<meta property="og:title" content="` + seoTitle + `">
<meta property="og:description" content="` + seoDescription + `">
<meta property="og:url" content="{PUBLIC_URL}/">
<meta property="og:image" content="{PUBLIC_URL}/og.jpg">
<meta property="og:image:type" content="image/jpeg">
<meta property="og:image:width" content="1200">
<meta property="og:image:height" content="630">
<meta property="og:image:alt" content="Land use of the cadastral municipality Kohlschwarz (Styria) as H3 cells, observed by LiDAR: forest, grassland and cropland over a hillshade">
<meta name="twitter:card" content="summary_large_image">
<meta name="twitter:title" content="` + seoTitle + `">
<meta name="twitter:description" content="` + seoDescription + `">
<meta name="twitter:image" content="{PUBLIC_URL}/og.jpg">
<script type="application/ld+json">{"@context":"https://schema.org","@graph":[
{"@type":"WebSite","@id":"{PUBLIC_URL}/#website","url":"{PUBLIC_URL}/","name":"umfeld-at","description":"` + seoDescription + `","inLanguage":["en","de"]},
{"@type":"WebAPI","@id":"{PUBLIC_URL}/#api","name":"umfeld-at API","url":"{PUBLIC_URL}/api/v1","documentation":"{PUBLIC_URL}/api/v1/docs/llm.txt","termsOfService":"{PUBLIC_URL}/lizenzen","provider":{"@id":"{PUBLIC_URL}/#website"}},
{"@type":"Dataset","@id":"{PUBLIC_URL}/#ne-cells","name":"NE cells — declared and observed land use of Austria, H3 res 12","description":"Per-cell declared land use (digested by peers from BEV cadastre vector tiles, CC BY 4.0), observed use from LiDAR and satellite, and change between digests.","url":"{PUBLIC_URL}/api/v1/docs/ne.md","spatialCoverage":{"@type":"Place","name":"Austria"},"license":"https://creativecommons.org/licenses/by/4.0/","isAccessibleForFree":true,"includedInDataCatalog":{"@id":"{PUBLIC_URL}/#website"},"distribution":[{"@type":"DataDownload","encodingFormat":"application/json","contentUrl":"{PUBLIC_URL}/api/v1/ne/cells"},{"@type":"DataDownload","encodingFormat":"application/vnd.mapbox-vector-tile","contentUrl":"{PUBLIC_URL}/api/v1/ne/mvt.json"}]}
]}</script>
`

// seoRobotsTxt — the API itself is not for crawlers (every call renders a
// different JSON body); the landing page, the docs and the licence page are.
const seoRobotsTxt = `User-agent: *
Allow: /$
Allow: /og.jpg
Allow: /llm.txt
Allow: /api/v1/docs/
Allow: /api/v1/license
Allow: /impressum
Allow: /datenschutz
Allow: /lizenzen
Disallow: /api/
Disallow: /contrib/

Sitemap: {PUBLIC_URL}/sitemap.xml
`

// seoSitemapPaths — indexable pages. Legal pages carry noindex and stay out.
var seoSitemapPaths = []struct {
	path, freq, prio string
}{
	{"/", "daily", "1.0"},
	{"/api/v1/docs/llm.txt", "weekly", "0.8"},
	{"/api/v1/docs/ne.md", "weekly", "0.7"},
	{"/api/v1/docs/ne-change.md", "weekly", "0.6"},
	{"/api/v1/docs/stats-envelope.md", "monthly", "0.5"},
	{"/api/v1/docs/qgis-layer.md", "monthly", "0.5"},
	{"/api/v1/license", "monthly", "0.4"},
}

func seoSitemapXML() string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` + "\n")
	for _, p := range seoSitemapPaths {
		b.WriteString("  <url><loc>{PUBLIC_URL}" + p.path + "</loc><changefreq>" + p.freq + "</changefreq><priority>" + p.prio + "</priority></url>\n")
	}
	b.WriteString("</urlset>\n")
	return b.String()
}

// seoPublicURL — {PUBLIC_URL} from the env, else the request's own origin so
// the absolute links stay valid on a machine without UMFELD_PUBLIC_URL.
func seoPublicURL(r *http.Request) string {
	if u := peers().PublicURL; u != "" {
		return u
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// seoText — peerText plus the origin fallback for {PUBLIC_URL}.
func seoText(r *http.Request, s string) string {
	return peerText(strings.ReplaceAll(s, "{PUBLIC_URL}", seoPublicURL(r)))
}

func handleSEO(mux *http.ServeMux) {
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write([]byte(seoText(r, seoRobotsTxt)))
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write([]byte(seoText(r, seoSitemapXML())))
	})
	mux.HandleFunc("/og.jpg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "public, max-age=604800")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		_, _ = w.Write(ogImageJPG)
	})
}
