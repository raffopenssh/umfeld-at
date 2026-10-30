# RIS legal references per Katastralgemeinde

`data/legal_parcel_refs.csv` (committed) lists Austrian Landesrecht texts in
RIS (Rechtsinformationssystem des Bundes) that name cadastral parcels of a
Katastralgemeinde — national-park and nature/water-protection ordinances,
administrative-boundary laws. The file was produced by a one-off scan of the
RIS OGD API (`data.bka.gv.at/ris/api/v2.6/Landesrecht`, query `Katastralgemeinde X`
/ `Katastralgemeinden X` per KG, prose + fixed-width table extraction of
"Grundstück Nr." sequences). The scanner is not part of this repository;
the CSV is the artefact.

**Columns:** `kg_code, kg_name, grundstueck, source_law_name, source_date,
source_url, parcel_list_type (listed | boundary_walk), legal_context
(national_park | nature_protection | water_protection | admin_boundary | …),
bundesland, gesetzesnummer[, ez]`.

**Serving** (`api_legal_refs.go`; loaded into memory at startup, keyed by KG code):

| Route | Tier | What |
|---|---|---|
| `GET /api/v1/legal/kg/{kg}[?context=&type=&law=]` | public | all refs of one KG — the admin code is the key, the content is law text, no cadastre is read |
| `GET /api/v1/legal/search?q=[&context=&type=&bundesland=&kg=]` | public | full-text over law names |
| `GET /api/v1/legal/stats` | public | counts per context / Bundesland |

The `legal` block of `/api/v1/context` is filled only when the caller passes
`kg=` — the service never resolves a point to a KG. Per-parcel lookups are not
offered: the parcel number a law names is only meaningful against a cadastre
the client builds itself (bevdirect).

Attribution source id `ris` (`api_license.go`); KG codes are 5 digits, padded
(`docs/kg-codes.md`).
