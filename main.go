// main.go — Austrian Cadastre Processing & API Server
//
// Processes all ~8,000 Katastralgemeinden (cadastral municipalities) from
// BEV vector tiles and serves them via a comprehensive search/query API.
//
// Modes:
//
//	server (default)  — start HTTP API on :8000 (PORT); import flags below run once and exit.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	// Parse flags
	addressesImport := flag.String("addresses-import", "", "Build data/addresses.db from an unzipped BEV Adressregister directory (ADRESSE.csv, STRASSE.csv, GEMEINDE.csv, ORTSCHAFT.csv, Aktualitaetsstand.txt), then exit")
	toponymsImport := flag.Bool("toponyms-import", false, "Build data/toponyms.db from the BEV DLM-Namen GPKG (downloads it when missing), then exit. Live API reloads the file automatically")
	toponymsGPKG := flag.String("toponyms-gpkg", "", "Local DLM_7000_NAMEN_<YYYYMMDD>.gpkg to import (default data/bev_namen/DLM_7000_NAMEN_20250325.gpkg)")
	toponymsURL := flag.String("toponyms-url", "", "Download URL for the DLM-Namen GPKG when the local file is missing (default: the 2025-03-25 Stichtag)")
	osmImport := flag.Bool("osm-import", false, "Import roads/rail/transit/water/places from data/osm_geofabrik_data/austria.gpkg into data/osm_proximity.db, then exit")
	osmImportAreas := flag.Bool("osm-import-areas", false, "Import water polygons (gis_osm_water_a_free) into osm_areas as whole (Multi)Polygons, then exit (keeps lines/points)")
	kgUniverseBuild := flag.Bool("kg-universe-build", false, "Build data/kg_universe.json.gz (all KGs, universe_hash) from the kg table ∪ VGD envelopes (data/bev_vgd_admin.json.gz), update kg table bboxes, write data/kg_universe_bbox_diff.json.gz, verify the code set against the KG-universe peer, then exit (kg_universe.go, kg_universe_bbox.go)")
	neTrainDict := flag.String("ne-train-dict", "", "Train the NE zstd dictionary for a layer (lu|obs) from the local inputs/store into data/ne_cells/dict/{layer}-{ne-dict-n}.zd, then exit (ne_store.go)")
	neDictN := flag.Int("ne-dict-n", 1, "Dictionary number for -ne-train-dict")
	flag.Parse()

	// One-off passes (separate process; the live API is unaffected).
	if *neTrainDict != "" {
		neStore.load()
		if err := NETrainDict(*neTrainDict, *neDictN, 60); err != nil {
			log.Fatal(err)
		}
		return
	}
	if *kgUniverseBuild {
		if err := InitSearchIndex(); err != nil {
			log.Fatalf("Search index init failed: %v", err)
		}
		os.Exit(RunKGUniverseBuild())
	}

	// BEV DLM-Namen toponym import (separate process; live API reloads the
	// swapped DB within toponymsReloadInterval). Needs the KG register.
	if *addressesImport != "" {
		if err := RunAddressesImport(*addressesImport); err != nil {
			log.Fatalf("addresses-import failed: %v", err)
		}
		return
	}
	if *toponymsImport {
		if err := InitSearchIndex(); err != nil {
			log.Fatalf("Search index init failed: %v", err)
		}
		if err := RunToponymImport(ToponymImportOptions{GPKGPath: *toponymsGPKG, URL: *toponymsURL}); err != nil {
			log.Fatalf("toponyms-import failed: %v", err)
		}
		return
	}

	// OSM proximity import / backfill (separate process; live API unaffected)
	if *osmImport {
		if err := RunOSMImport(); err != nil {
			log.Fatalf("osm-import failed: %v", err)
		}
		return
	}
	if *osmImportAreas {
		if err := RunOSMImportAreas(); err != nil {
			log.Fatalf("osm-import-areas failed: %v", err)
		}
		return
	}
	// Initialize the KG register
	if err := InitSearchIndex(); err != nil {
		log.Printf("Warning: search index init failed: %v", err)
	}

	// Revisit-hint memo (api_ne_revisit.go): the first compute walks the
	// derived change files + reports cold (~10 s); warm it off the request path.
	neNatStart()        // national cell table behind the tiles' low zooms (ne_mvt_nat.go)
	go neFidxWarmLoop() // filter index: prebuild the bins behind recent near tiles (ne_glyph_fidx.go)
	go func() {
		time.Sleep(45 * time.Second)
		neDeclWarm() // declared-view states for the manifest (ne_change_declared.go)
		StatsMemo(neRevisitMemoKey, neRevisitTTL, false, neRevisitCompute)
	}()

	// ── Register API routes ─────────────────────────────────────────
	mux := http.NewServeMux()

	// API v1 endpoints (/api/v1/*)
	RegisterAPIv1(mux)

	// KG universe — GET /api/v1/kgs (kg_universe.go): committed file first,
	// kg table fallback, background agreement check with the KG-universe peer.
	InitKGUniverse()

	// Legal refs API (/api/v1/legal/*)
	RegisterLegalRefsAPI(mux)

	// Natura 2000 protected sites API (/api/v1/natura2000/*)
	RegisterNatura2000API(mux)

	// NE routes go through apiMiddleware like everything else so every peer
	// write (/contrib, /k) shows up in the 📨 request log with its peer= tag.
	NEStoreStart()                                     // .nex store + converter (ne_store.go)
	mux.Handle("/api/v1/ne", apiMiddleware(handleNE))  // NE cells: reads public (api_ne_public.go), writes srtm only (api_ne.go)
	mux.Handle("/api/v1/ne/", apiMiddleware(handleNE)) // draft prefix above wins for /ne/draft/*
	// OSM proximity metrics (roads / transit / rail / water / remoteness) for
	// a point or an H3 cell, plus the OSM geometry overlay.
	InitOSMProximity()
	RegisterOSMProximityAPI(mux)

	// BEV DLM-Namen toponyms: own DB, in-memory trigram index.
	EnsureToponyms()
	RegisterToponymAPI(mux)

	// Land prices API (/api/v1/land_prices/*) — Statistik Austria-calibrated
	if _, err := LoadLandPrices(); err != nil {
		log.Printf("Warning: land_prices init: %v", err)
	}
	RegisterLandPriceAPI(mux)
	// Point-keyed overlay bundle for clients with their own cadastre (api_context.go).
	RegisterContextAPI(mux)

	// Impressum / Datenschutz / Lizenzen (api_legal_pages.go).
	handleLegalPages(mux)

	// Static files (if present); otherwise "/" is the public landing page.
	if _, err := os.Stat("static"); err == nil {
		mux.Handle("/", http.FileServer(http.Dir("static")))
	} else {
		mux.HandleFunc("/", handlePublicRoot)
	}

	port := "8000"
	if p := os.Getenv("PORT"); p != "" {
		port = p
	}

	// pprof + /api/v1/debug/inflight (private tier only)
	RegisterDebugAPI(mux)

	log.Printf("\U0001f1e6\U0001f1f9 umfeld-at starting on :%s", port)

	// Wrap with request logger + in-flight tracking, then transparent gzip.
	//
	// gzip is the single biggest client-visible win: a Graz-sized viewport
	// response is ~8.2 MB pretty-printed / ~1.2 MB compact / ~190 KB gzipped.
	// Handlers finish in ~150 ms but the browser used to spend seconds just
	// receiving bytes.
	// Order: gzip (outermost, so a 304 skips compression entirely) → ETag /
	// Cache-Control → in-flight tracking → mux.
	// Outermost: API tiers (api_tiers.go) — public allow-list on /api/v1,
	// token-gated private prefix for the ops surface. Strips the prefix before
	// the cache/egress layers so they key on the handler's path.
	handler := tierMiddleware(egressMiddleware(gzipMiddleware(cacheMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		done := TrackRequest(r.Method, r.URL.String())
		defer done()
		mux.ServeHTTP(w, r)
	})))))
	log.Printf("🔐 API tiers: public_only=%v private_tier=%v", publicOnly(), privateTierOpen())

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}
