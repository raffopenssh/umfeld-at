# Natura 2000 Overlay

Natura 2000 protected sites are loaded in-memory from `data/natura2000_at.geojson.gz`
at startup (353 Austrian sites; Habitats Directive SCI/SAC, Birds Directive SPA,
and combined). No database is involved — every answer is computed live against
the in-memory polygons (`natura2000.go`).

**Data source:** EEA Natura2000Sites MapServer, layer 2 (combined).
Refresh with:
```
curl 'https://bio.discomap.eea.europa.eu/arcgis/rest/services/ProtectedSites/Natura2000Sites/MapServer/2/query?where=MS%3D%27AT%27&outFields=SITECODE,SITENAME,MS,SITETYPE,Area_ha&f=geojson&outSR=4326&returnGeometry=true' | gzip > data/natura2000_at.geojson.gz
sudo systemctl restart umfeld-at
```

**Endpoints under `/api/v1/natura2000/` (all public):**
- `search?q=&type=A|B|C&list=all` — site search by name / code
- `site/{sitecode}?geometry=1` — single site, optional MultiPolygon geometry
- `point?lon=&lat=` — point-in-polygon: the sites containing the point

The same point test fills the `natura2000` block of `/api/v1/context` and the
`/search/protected_area` route serves the WDPA layer (`data/nationalparks_wdpa.geojson`).

**Elsewhere:** site outlines are one of the glyph find box's outline
collections (`docs/glyph-find-contract.md`); the filter index (`fidx`) carries
a Natura 2000 membership bit per cell (`docs/ne-filter.md`); the unit stats
envelope reports Natura 2000 overlap per unit (`docs/stats-envelope.md`).

SITETYPE codes: `A`=Birds Directive (SPA), `B`=Habitats Directive (pSCI/SCI/SAC),
`C`=both.

**Habitat tags** (`natura2000HabitatRules`) are derived from site-name keywords
and appear as `habitats[]` on every site record: `moor`, `floodplain`, `river`,
`lake`, `wetland`, `forest`, `alpine`, `meadow`, `pasture`, `valley`, `hill`,
`steppe`, `orchard`, `park`, `cave`.

Attribution source id `eea_natura2000` (EEA standard re-use policy, attribution).
