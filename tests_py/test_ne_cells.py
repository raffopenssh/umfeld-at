"""ne_cells determinism + contract tests.  Run: python3 -m pytest tests_py  (or python3 tests_py/test_ne_cells.py)"""
import json
import os
import random
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))
from ne_cells import canon, algo
from ne_cells.algo import build_cells, K, ALGO, H3_RES, K_RES
from ne_cells.pack import (MAGIC_LU, MAGIC_OBS, lu_header, pack_records, pack_section, unpack_sections,
                           LU_REC, K_REC)

FIX = os.path.join(os.path.dirname(__file__), "fixtures", "kohlschwarz_village.json")


def _inputs(seed=None):
    d = json.load(open(FIX))
    P, F, L = d["parcels"], d["footprints"], d["landuse"]
    if seed is not None:
        rnd = random.Random(seed)
        rnd.shuffle(P); rnd.shuffle(F); rnd.shuffle(L)
        # tile copies: duplicate some pieces, as a bevdirect fetch would deliver them
        F = F + F[: len(F) // 3]
        L = L + L[: len(L) // 2]
    return canon.parcels_from_index(P), canon.footprints_from_index(F), canon.landuse_from_index(L), tuple(d["bbox"])


def _blob(seed=None):
    P, F, L, bbox = _inputs(seed)
    r = build_cells("63330", P, F, L, coverage_bbox=bbox)
    raw = pack_records(r.cells, r.kcells)
    hdr = lu_header("63330", "2026-03", "index-2026-03", ALGO, H3_RES, K_RES, K, len(r.cells), len(r.kcells))
    return r, raw, pack_section(MAGIC_LU, hdr, raw)


def test_bit_identical_under_input_order_and_duplicates():
    r0, raw0, b0 = _blob(None)
    for seed in (1, 2):
        r1, raw1, b1 = _blob(seed)
        assert raw1 == raw0, "record block differs under shuffled/duplicated input"
        assert b1 == b0, "packed section differs"
    assert len(r0.cells) > 100


def test_roundtrip_and_header():
    r, raw, blob = _blob()
    secs = unpack_sections(blob)
    assert len(secs) == 1 and secs[0].magic == MAGIC_LU
    cells, kcells = secs[0].records()
    assert cells == r.cells and kcells == r.kcells
    h = secs[0].header
    assert h["algo"] == ALGO and h["h3_res"] == 12 and h["k_res"] == 10 and h["k"] == 5
    assert h["groups"][0] == "geb" and len(h["groups"]) == 9
    assert "CC BY 4.0" in h["attribution"] and h["notice"]
    assert len(raw) == len(cells) * LU_REC.size + len(kcells) * K_REC.size


def test_privacy_rules():
    r, _, _ = _blob()
    for c in r.cells.values():
        if c["bld_n"] < K:
            assert c["orient_coh"] == 0xFF and c["orient_axis"] == 0xFF
        assert sum(c["lu"]) <= c["land"] + 9            # ≤ land up to per-group rounding
        assert c["land"] >= int(algo.MIN_LAND * 255)
        assert c["kg_share"] >= 1
    for kc in r.kcells.values():
        assert kc["n_parc"] >= K
        if kc["bld_n"] < K:
            assert kc["bld_p50"] == 0xFFFF


def test_obs_section_passthrough():
    raw = bytes(range(256)) * 4
    hdr = dict(magic="NEO1", algo="obs-cells-0", epoch="2025-07", source="als-2025", kg="63330", h3_res=12,
               cells_n=4, groups=list(algo.GROUP_ORDER))
    blob = pack_section(MAGIC_OBS, hdr, raw)
    s = unpack_sections(blob)[0]
    assert s.magic == MAGIC_OBS and s.raw == raw and s.header["digest"] == unpack_sections(blob)[0].header["digest"]


def test_h3_parent_matches_library_and_chunks():
    import h3
    from ne_cells.pack import h3_parent, chunk_digests
    r, raw, _ = _blob()
    for c in list(r.cells)[:300]:
        assert format(h3_parent(int(c, 16), 10), "x") == h3.cell_to_parent(c, 10)
    d = chunk_digests(raw, len(r.cells))
    assert d and all(len(v) == 16 for v in d.values())


def test_bevdirect_adapter_same_result():
    """Index rows re-expressed as bevdirect-serve rows (split in two overlapping cells with
    complete:false copies) must give the identical record block."""
    d = json.load(open(FIX))
    from shapely.geometry import shape
    idx = (canon.parcels_from_index(d["parcels"]), canon.footprints_from_index(d["footprints"]),
           canon.landuse_from_index(d["landuse"]))
    half = 15.1165
    docs = [dict(parcels=[], footprints=[], landuse=[]), dict(parcels=[], footprints=[], landuse=[])]
    for r in d["parcels"]:
        row = dict(parcel_id=r["parcel_id"], kg_code=r["kg_code"], rstatus=r["status"], ez=r.get("ez"),
                   area_sqm=r["area_sqm"], geometry=r["geometry"], complete=True)
        x0, _, x1, _ = shape(r["geometry"]).bounds
        if x1 < half:
            docs[0]["parcels"].append(row)
        elif x0 >= half:
            docs[1]["parcels"].append(row)
        else:   # crosses the pad: a truncated copy in one cell, the complete one in the other
            docs[0]["parcels"].append(dict(row, complete=False, geometry=json.loads(json.dumps(r["geometry"]))))
            docs[1]["parcels"].append(row)
    for r in d["footprints"]:
        docs[0]["footprints"].append(dict(id="f", ns=41, area_sqm=r["area_sqm"], geometry=r["geometry"]))
        docs[1]["footprints"].append(dict(id="f", ns=41, area_sqm=r["area_sqm"], geometry=r["geometry"]))
    for r in d["landuse"]:
        docs[1]["landuse"].append(dict(id="l", ns=int(r["code"]), area_sqm=r["area_sqm"], geometry=r["geometry"]))
    bev = canon.from_bevdirect(docs)
    bbox = tuple(d["bbox"])
    a = build_cells("63330", *idx, coverage_bbox=bbox)
    b = build_cells("63330", *bev, coverage_bbox=bbox)
    assert pack_records(a.cells, a.kcells) == pack_records(b.cells, b.kcells)


if __name__ == "__main__":
    for n, f in list(globals().items()):
        if n.startswith("test_"):
            f(); print("ok", n)
