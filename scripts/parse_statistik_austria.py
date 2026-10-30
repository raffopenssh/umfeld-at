#!/usr/bin/env python3
"""Parse Statistik-Austria ODS/CSV downloads into a single JSON consumed by
the Go service (`land_prices.go`).

Input directory : data/statistik_austria/
Output          : data/statistik_austria/parsed/land_prices.json

Requires: pip install pyexcel-ods3
"""

import pyexcel_ods3, json, re, glob, csv, math, unicodedata, os, sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
SA   = ROOT / "data" / "statistik_austria"
OUT  = SA / "parsed" / "land_prices.json"
OUT.parent.mkdir(parents=True, exist_ok=True)

if not SA.exists():
    sys.exit(f"missing {SA}")

# Normalise filenames (Mac NFD → NFC) so the * globs match later
for f in SA.iterdir():
    nfc = unicodedata.normalize("NFC", f.name)
    if nfc != f.name:
        f.rename(SA / nfc)

BL_NAME_TO_CODE = {
    "Burgenland": "1", "Kärnten": "2", "Niederösterreich": "3",
    "Oberösterreich": "4", "Salzburg": "5", "Steiermark": "6",
    "Tirol": "7", "Vorarlberg": "8", "Wien": "9", "Österreich": "AT",
}

# 1) Bauland per Gemeinde (ODS, one file per release year)
bau = {}
for fp in sorted(SA.glob("Baugrundstueckspreise*.ods")):
    year = int(re.search(r"(\d{4})", fp.name).group(1))
    for sheet_rows in pyexcel_ods3.get_data(str(fp)).values():
        for r in sheet_rows:
            if len(r) < 5:
                continue
            gnr = str(r[2]).strip() if r[2] else ""
            price = r[4]
            if gnr.isdigit() and len(gnr) == 5 and isinstance(price, (int, float)):
                bau.setdefault(gnr, {})[year] = float(price)

# 2) Bezirk sheets in the Sonderauswertung
sonder = pyexcel_ods3.get_data(str(SA / "SonderauswertungImmobilienDurchschnittspreise.ods"))

def parse_bezirk(sheet):
    out, header = {}, None
    for r in sheet:
        if len(r) >= 2 and r[0] == "Bz. Nr.":
            header = r
            continue
        if not header or not r or not isinstance(r[0], (int, float)):
            continue
        bnr = str(int(r[0])).zfill(3)
        vals = {}
        for i, h in enumerate(header[2:], start=2):
            if i >= len(r):
                break
            if isinstance(r[i], (int, float)) and r[i]:
                vals[int(h)] = float(r[i])
        if vals:
            out[bnr] = vals
    return out

grund_bezirk = parse_bezirk(sonder["Grundpreise"])
haus_bezirk  = parse_bezirk(sonder["Häuserpreise"])
whg_bezirk   = parse_bezirk(sonder["Wohnungspreise"])

# Wien Bezirke (Bnr 910..923) are listed in the Bauland ODS "Wien" sheet
wien_bezirk = {}
for fp in sorted(SA.glob("Baugrundstueckspreise*.ods")):
    year = int(re.search(r"(\d{4})", fp.name).group(1))
    for r in pyexcel_ods3.get_data(str(fp)).get("Wien", []):
        if len(r) < 5:
            continue
        try:
            bnr = int(str(r[0]).strip())
        except (ValueError, TypeError):
            continue
        if 900 < bnr < 930 and isinstance(r[4], (int, float)):
            wien_bezirk.setdefault(str(bnr), {})[year] = float(r[4])
for k, v in wien_bezirk.items():
    grund_bezirk.setdefault(k, {}).update(v)

# Synthesise Wien city (90001) as geometric mean of its Bezirke
wien_by_year = {}
for yrs in wien_bezirk.values():
    for y, v in yrs.items():
        wien_by_year.setdefault(y, []).append(v)
if wien_by_year:
    bau["90001"] = {y: round(math.exp(sum(math.log(x) for x in vs)/len(vs)), 1)
                    for y, vs in wien_by_year.items()}
# Drop the bogus 91XXX gemeinde codes that some ODS years (≤ 2018) included
for k in list(bau.keys()):
    if k.startswith("9") and k != "90001":
        bau.pop(k)

# 3) Agricultural CSVs (Bundesland × year)
def parse_csv(path, delim):
    out = {}
    with open(path, encoding="utf-8-sig") as f:
        rdr = csv.reader(f, delimiter=delim)
        header = next(rdr)
        years = []
        for h in header[1:]:
            h = h.strip().strip('"')
            try:
                years.append(int(h))
            except ValueError:
                years.append(None)
        for row in rdr:
            if not row:
                continue
            name = row[0].strip().strip('"')
            code = BL_NAME_TO_CODE.get(name)
            if not code:
                continue
            for yr, val in zip(years, row[1:]):
                if not yr:
                    continue
                v = val.strip().strip('"')
                if not v:
                    continue
                if delim == ";":
                    v = v.replace(",", ".")
                try:
                    out.setdefault(code, {})[yr] = float(v)
                except ValueError:
                    pass
    return out

acker_buy  = parse_csv(SA / "Bodenpreise für Ackerland in Eurom² – Jahreswerte (Tabelle).csv", ";")
gruen_buy  = parse_csv(SA / "Bodenpreise für Dauergrünland in Eurom² – Jahreswerte (Tabelle).csv", ";")
gruen_rent = parse_csv(SA / "Pachtpreise für Dauergrünland in Euro pro Hektar – Jahreswerte (Tabelle).csv", ";")
acker_rent = parse_csv(SA / "Pachtpreise für Ackerland in Euro pro Hektar – Jahreswerte (Grafik).csv", ",")

# 2b) Per-Bezirk × Bauperiode Häuserpreise (mean of the 3×3 cells per Bauperiode
#     across size categories A/B/C). Bezirk names are matched fuzzily against
#     the Sonderauswertung canonical list (haus_bezirk keys).
import re as _re
def _norm_name(s):
    s = unicodedata.normalize("NFKC", str(s))
    s = _re.sub(r"\d\)", "", s)            # drop footnote markers like '2)'
    s = _re.sub(r"\([^)]*\)", "", s)        # drop bracketed annotations
    s = _re.sub(r"\s+", " ", s).strip().lower()
    return s

# Build name→code from Sonderauswertung Häuserpreise sheet (canonical).
bezirk_name_to_code = {}
for r in sonder["Häuserpreise"]:
    if len(r) >= 2 and isinstance(r[0], (int, float)) and r[1]:
        bezirk_name_to_code[_norm_name(r[1])] = str(int(r[0])).zfill(3)
# Wien Bezirke (91X) appear in the Bauland "Wien" sheet — recover names there.
for fp in sorted(SA.glob("Baugrundstueckspreise*.ods")):
    for r in pyexcel_ods3.get_data(str(fp)).get("Wien", []):
        if len(r) >= 2:
            try:
                bnr = int(str(r[0]).strip())
            except (ValueError, TypeError):
                continue
            if 900 < bnr < 930 and r[1]:
                bezirk_name_to_code.setdefault(_norm_name(r[1]), str(bnr))

PERIODS = ["pre1960", "y1960_1990", "post1991"]

def _is_num(x):
    return isinstance(x, (int, float)) and x > 0

def parse_haus_periods(fp):
    """Yield (bezirk_code, {period: avg_eur_per_sqm}) tuples for one yearly file.
    Averages over all size categories (A/B/C) and Wohnfläche brackets present."""
    sheets = pyexcel_ods3.get_data(str(fp))
    out = {}  # bezirk_code → {period: [values]}
    BL_SHEETS = {"Burgenland", "Kärnten", "Niederösterreich",
                 "Oberösterreich", "Salzburg", "Steiermark",
                 "Tirol", "Vorarlberg", "Wien"}
    for sn, rows in sheets.items():
        if sn not in BL_SHEETS:
            continue
        # Walk through Kategorie A/B/C blocks. Each block: title row + 'Wohnfläche
        # nach Bauperioden' + 'Bezirke,Bis 1960,…' header + size detail row + data.
        for i, r in enumerate(rows):
            if not r:
                continue
            first = str(r[0]) if r and r[0] is not None else ""
            if not first.startswith("Durchschnittspreise für Häuser der Kategorie"):
                continue
            # Look ahead: data rows start ≈ i+4 (title, '', header, size-detail).
            for j in range(i + 1, min(i + 60, len(rows))):
                rd = rows[j]
                if not rd or len(rd) < 4:
                    continue
                name = str(rd[0]) if rd[0] else ""
                if name.startswith("Durchschnittspreise") or not name:
                    if name.startswith("Durchschnittspreise"):
                        break
                    continue
                code = bezirk_name_to_code.get(_norm_name(name))
                if not code:
                    continue
                # Columns 1-9 hold (3 Wohnfläche brackets) × (3 Bauperioden).
                cells = rd[1:10] + [None] * max(0, 10 - len(rd))
                # Bauperiode 1 = cells[0:3], 2 = cells[3:6], 3 = cells[6:9]
                for pi, period in enumerate(PERIODS):
                    vals = [c for c in cells[pi*3:(pi+1)*3] if _is_num(c)]
                    if vals:
                        out.setdefault(code, {}).setdefault(period, []).extend(vals)
    # collapse list → mean
    return {c: {p: sum(v)/len(v) for p, v in pd.items()}
            for c, pd in out.items()}

haus_bezirk_period = {}  # bezirk_code → {year → {period → avg EUR/m²}}
for fp in sorted(SA.glob("Haeuserpreise*.ods")):
    year = int(_re.search(r"(\d{4})", fp.name).group(1))
    for code, pd in parse_haus_periods(fp).items():
        haus_bezirk_period.setdefault(code, {})[year] = pd

# Synthesise Wien city aggregate (district_code '900' in our cadastre index)
# as the geometric mean of all Wien Bezirke (910…923) per year + period.
wien_codes = [c for c in haus_bezirk_period if c.startswith("9") and c != "900" and 900 < int(c) < 930]
if wien_codes:
    agg = {}  # year → period → [values]
    for code in wien_codes:
        for year, pd in haus_bezirk_period[code].items():
            for p, v in pd.items():
                agg.setdefault(year, {}).setdefault(p, []).append(v)
    haus_bezirk_period["900"] = {
        year: {p: round(math.exp(sum(math.log(x) for x in vs) / len(vs)), 1)
               for p, vs in pds.items()}
        for year, pds in agg.items()
    }
    # Mirror the same aggregate into the plain haus_by_bezirk table so the
    # bezirk-only fallback path also resolves "900".
    if "900" not in haus_bezirk:
        agg2 = {}
        for code in wien_codes:
            for year, v in haus_bezirk.get(code, {}).items():
                agg2.setdefault(year, []).append(v)
        haus_bezirk["900"] = {y: round(math.exp(sum(math.log(x) for x in vs) / len(vs)), 1)
                              for y, vs in agg2.items() if vs}

result = {
    "bauland_eur_per_sqm_by_gemeinde": bau,
    "bauland_eur_per_sqm_by_bezirk":   grund_bezirk,
    "haus_eur_per_sqm_by_bezirk":      haus_bezirk,
    "haus_eur_per_sqm_by_bezirk_period": haus_bezirk_period,
    "wohnung_eur_per_sqm_by_bezirk":   whg_bezirk,
    "agri_eur_per_sqm_by_state": {
        "ackerland": acker_buy,
        "gruenland": gruen_buy,
    },
    "agri_rent_eur_per_ha_per_year_by_state": {
        "ackerland": acker_rent,
        "gruenland": gruen_rent,
    },
    "_meta": {
        "source": "Statistik Austria (statistik.at), 2015-2024",
        "bauland_basis": "Datenbasis 2020-2024, geometrische Mittel, normiert auf Preisniveau",
        "bauland_ns_codes": ["40","41","42","43","44","45","46","47","91"],
        "ackerland_ns_codes": ["51","62"],
        "gruenland_ns_codes": ["50","52","53","54","55","58","61"],
        "wald_ns_codes": ["56","57"],
        "bezirk_code_format": "3 digits, e.g. 101 = Eisenstadt-Stadt",
        "gemeinde_code_format": "5 digits",
        "bundesland_code_format": "1 digit (1=Bgld..9=Wien)",
        "price_unit_bauland": "EUR per m²",
        "price_unit_agri_buy": "EUR per m²",
        "price_unit_agri_rent": "EUR per hectare per year",
        "haus_periods": ["pre1960", "y1960_1990", "post1991"],
        "haus_period_basis": "mean over size-category A/B/C and Wohnflächenbrackets",
    },
}

OUT.write_text(json.dumps(result, indent=2, ensure_ascii=False))
print(f"✅ wrote {OUT}")
print(f"   {len(bau)} gemeinden, {len(grund_bezirk)} bezirke, agri {len(acker_buy)} states")
print(f"   haus_periods: {len(haus_bezirk_period)} bezirke with period detail")
