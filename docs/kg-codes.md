# KG Codes

- **KG codes** (5 digits): `63303`, `60102` — Katastralgemeinde register key
- **PLZ** (4 digits): `8570`, `1010` — postal only, NOT KG codes
- Gemeinde ↔ KG: `data/kg_to_gemeinde.json`, `GET /api/v1/lookup`, `GET /api/v1/kgs`

## Zero padding — one canonical spelling (`kg_code.go`)

740 of the 7 850 KG codes start with a zero (`01503` Heiligenstadt, `07201`
Aalfang) and **every** place the system STORES one uses the padded form:
`kg.kg_code`, the KG universe, NE containers (`data/ne_cells/{kg}.*`), legal
refs. But `data/edm_austria_lookups.json` has an **unpadded `kg_code`** field
for exactly those 740 (its `kg_number` keeps the zero), so `/api/v1/lookup?kg=01503`
used to answer `kg_code: "1503"` — and feeding that back into a KG-keyed route
matched **nothing, silently, with HTTP 200**. Same identifier, two formats, no
error.

Fixed in three places:
1. **Output** — the EDM loader canonicalises on load, so `/lookup` only ever
   emits ids that work verbatim elsewhere (`kg_number` is exposed alongside).
2. **Input** — `normalizeKGParams` in `apiMiddleware` zero-pads any 4-digit
   numeric value of a KG-carrying query parameter. Handlers that parse a code out
   of the **path** call `NormalizeKGCode` on that segment; the KG universe pads in
   `finish()` and before hashing (`docs/api-tiers.md` §KG universe).
3. **Disclosure** — a rewritten request gets `X-KG-Code-Normalized: true`, and
   `llm.txt` carries a "KG code format" section.

**Adding a filter?** If it carries a KG code, add its parameter name to
`kgCodeParams` in `kg_code.go`. This is an explicit set, not a heuristic,
precisely so a same-shaped value in a DIFFERENT namespace is never mangled —
deliberately NOT padded:

- 1–3 digit values (a typeahead **prefix**: `?q=63` must not become `00063`)
- `plz=` — also 4 digits (`8580`), different namespace
- `gemeinde=` / `gemeinde_code=` / `district=`
- `?id=` — a Gemeindekennziffer on `/search/municipalities`

Covered by `kg_code_test.go`.
