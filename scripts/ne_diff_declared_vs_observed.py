#!/usr/bin/env python3
"""DIFF — declared (view) vs observed (observed-layer peer NEO1, lidar epoch per KG) per cell.

Two signals, never mixed (docs/ne-change.md §diff):
  CHANGE = declared over time (chunk log, confirmed deltas)        — not computed here, only joined
  DIFF   = declared view vs observed row of the same cell, today   — this script

Reads only public routes of umfeld-at (localhost) + the observed-layer peer's public KG summary (als{} block).
No SQL, no store files. Resumable: every phase caches under --out (default data/ne_diff/,
not committed).

  python3 scripts/ne_diff_declared_vs_observed.py als            # peer als{} for all KGs → als.json (one bulk GET)
  python3 scripts/ne_diff_declared_vs_observed.py diff [--sample N] [--kgs a,b]   # per-KG rows → diff.jsonl
  python3 scripts/ne_diff_declared_vs_observed.py change        # KGs with confirmed change: before/after kinds
  python3 scripts/ne_diff_declared_vs_observed.py report        # aggregate → report.md (+ stdout)

Vocabulary (kind of one cell, dominant group each side, groups geb bau acker gruen wald wasser verkehr alpen sonst):
  agree                   same dominant group
  built_undeclared        observed building cover ≥ 20 % of the cell, declared buildings < 5 %
  declared_built_absent   declared buildings ≥ 20 %, observed building cover < 5 %
  forest_declared_open    declared forest, observed grass / crop / bare / construction
  forest_undeclared       declared grass / crop / alpine / other, observed trees
  arable_grass            crop vs grass, either direction
  building_land_green     declared building land (bau), observed vegetation
  sealed_undeclared       declared vegetation / other, observed construction, parking or road
  traffic                 one side roads & paths, the other not (not caught above)
  water                   one side water, the other not
  alpine_pasture          declared alpine (BEV "Alpen" = alpine pasture), observed grass — a vocabulary gap, not a diff
  alpine                  one side rock / bare / glacier, the other not (not caught above)
  other                   any other disagreement
Filter status of a cell (independent of kind): ok | edge | mixed | unknown_declared | obs_null | superseded.
"""
import os
import argparse, json, os, sys, time, zlib, struct, collections
import numpy as np
import requests

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))
from ne_cells.pack import unpack_sections  # noqa: E402

GROUPS = ["geb", "bau", "acker", "gruen", "wald", "wasser", "verkehr", "alpen", "sonst"]
G = {g: i for i, g in enumerate(GROUPS)}
KINDS = ["agree", "built_undeclared", "declared_built_absent", "forest_declared_open", "forest_undeclared",
         "arable_grass", "building_land_green", "sealed_undeclared", "traffic", "water", "alpine_pasture", "alpine", "other"]
KI = {k: i for i, k in enumerate(KINDS)}
STATUS = ["ok", "edge", "mixed", "unknown_declared", "obs_null", "superseded"]
SI = {s: i for i, s in enumerate(STATUS)}

LU_DT = np.dtype([("cell", "<u8"), ("kg_share", "u1"), ("land", "u1"), ("cov", "u1"), ("flags", "u1"),
                  ("lu", "u1", (9,)), ("bld_cover", "u1"), ("bld_n", "<u2"), ("gk", "u1"), ("n_parc", "u1"),
                  ("line_m_ha", "<u2"), ("orient_coh", "u1"), ("orient_axis", "u1")])
assert LU_DT.itemsize == 30
# observed-layer peer v2.4 NEO1 row, dict neo1_record "<Q9sBBBBBBBBBBbBBHBBBBBbbBB" (41 B)
OBS_DT = np.dtype([("cell", "<u8"), ("lu", "u1", (9,)), ("bld_cover", "u1"), ("bld_n", "u1"), ("h_mean", "u1"),
                   ("h_p90", "u1"), ("h_max", "u1"), ("tree_n", "u1"), ("tree_h_max", "u1"), ("crown", "u1"),
                   ("ndvi_mean", "u1"), ("ndvi_amp", "u1"), ("dh", "i1"), ("flight_years", "u1"), ("verdict", "u1"),
                   ("elev", "<u2"), ("slope", "u1"), ("slope_p90", "u1"), ("aspect", "u1"), ("rough", "u1"),
                   ("tri", "u1"), ("tpi", "i1"), ("dtm", "i1"), ("hansen", "u1"), ("phen", "u1")])
assert OBS_DT.itemsize == 41
VERDICT_UNKNOWN = 6
DOM_MIN = 0.5      # dominant group must hold ≥ half of the cell
BLD_HI, BLD_LO = 0.20, 0.05
DH_NEW_M = 2.0     # nDSM rise newest−oldest that says "built since the oldest flight"


def classify(dec, obs, dec_dom, obs_dom):
    """dec/obs: (n,9) float shares of the CELL; returns kind index per row (vectorised)."""
    n = len(dec)
    kind = np.full(n, KI["other"], np.int8)
    same = dec_dom == obs_dom
    g = G
    dg, og = dec[:, g["geb"]], obs[:, g["geb"]]
    built_un = (og >= BLD_HI) & (dg < BLD_LO)
    built_ab = (dg >= BLD_HI) & (og < BLD_LO)
    isin = lambda a, names: np.isin(a, [g[x] for x in names])
    rules = [  # order = priority
        ("built_undeclared", built_un),
        ("declared_built_absent", built_ab),
        ("agree", same),
        ("forest_declared_open", (dec_dom == g["wald"]) & isin(obs_dom, ["gruen", "acker", "alpen", "bau", "sonst"])),
        ("forest_undeclared", isin(dec_dom, ["gruen", "acker", "alpen", "sonst"]) & (obs_dom == g["wald"])),
        ("arable_grass", isin(dec_dom, ["acker", "gruen"]) & isin(obs_dom, ["acker", "gruen"])),
        ("building_land_green", (dec_dom == g["bau"]) & isin(obs_dom, ["gruen", "acker", "wald"])),
        ("sealed_undeclared", isin(dec_dom, ["gruen", "acker", "wald", "sonst", "alpen"]) & isin(obs_dom, ["bau", "verkehr"])),
        ("traffic", (dec_dom == g["verkehr"]) | (obs_dom == g["verkehr"])),
        ("water", (dec_dom == g["wasser"]) | (obs_dom == g["wasser"])),
        ("alpine_pasture", (dec_dom == g["alpen"]) & (obs_dom == g["gruen"])),
        ("alpine", (dec_dom == g["alpen"]) | (obs_dom == g["alpen"])),
    ]
    done = np.zeros(n, bool)
    for name, m in rules:
        m = m & ~done
        kind[m] = KI[name]
        done |= m
    return kind


def shares_and_dom(lu):
    s = lu.astype(np.float32) / 255.0
    dom = s.argmax(1)
    top = s.max(1)
    return s, dom, top


def fetch_kg(base, kg):
    r = requests.get(f"{base}/api/v1/ne/{kg}", params={"layers": "declared,obs", "format": "bin"}, timeout=120)
    if r.status_code != 200:
        return None, None, r.status_code
    secs = unpack_sections(r.content)
    lu = obs = None
    for s in secs:
        if s.magic == b"NEC1":
            raw = s.raw
            n = s.header["cells_n"]
            lu = np.frombuffer(raw[: n * 30], LU_DT)
        elif s.magic == b"NEO1":
            rb = s.header.get("record_bytes", 41)
            if rb != 41:
                return None, None, f"record_bytes={rb}"
            obs = np.frombuffer(s.raw, OBS_DT)
    return lu, obs, 200


def join(lu, obs):
    """inner join on cell id; returns (lu_rows, obs_rows) aligned."""
    li = np.argsort(lu["cell"], kind="stable")
    oi = np.argsort(obs["cell"], kind="stable")
    lc, oc = lu["cell"][li], obs["cell"][oi]
    common, a, b = np.intersect1d(lc, oc, assume_unique=False, return_indices=True)
    return lu[li][a], obs[oi][b]


def cell_table(lu, obs):
    """Per joined cell: kind, status, dec_dom, obs_dom, dh, plus the shares (for the change phase)."""
    dec, ddom, dtop = shares_and_dom(lu["lu"])
    ob, odom, otop = shares_and_dom(obs["lu"])
    kind = classify(dec, ob, ddom, odom)
    status = np.full(len(lu), SI["ok"], np.int8)
    flags = lu["flags"]
    status[(flags & 0x80) != 0] = SI["superseded"]
    obs_null = (obs["verdict"] == VERDICT_UNKNOWN) | (obs["h_mean"] == 255)
    status[(status == 0) & obs_null] = SI["obs_null"]
    status[(status == 0) & ((flags & 1) != 0)] = SI["unknown_declared"]
    edge = (lu["land"] < 230) | (lu["kg_share"] < 128)
    status[(status == 0) & edge] = SI["edge"]
    mixed = (dtop < DOM_MIN) | (otop < DOM_MIN)
    # built_* kinds are threshold rules, not dominant rules: a mixed cell still counts for them
    built = (kind == KI["built_undeclared"]) | (kind == KI["declared_built_absent"])
    status[(status == 0) & mixed & ~built] = SI["mixed"]
    return kind, status, ddom, odom, dec, ob


def kg_row(kg, lu, obs, als):
    l, o = join(lu, obs)
    kind, status, ddom, odom, dec, ob = cell_table(l, o)
    row = {"kg": kg, "cells_declared": int(len(lu)), "cells_obs": int(len(obs)), "cells_joined": int(len(l)),
           "als_newest": als.get("newest"), "als_oldest": als.get("oldest"), "als_dominant_share": als.get("dominant_share"),
           "als_reflown_share": als.get("reflown_share"), "als_precision": als.get("precision")}
    # kind × status matrix
    m = np.zeros((len(KINDS), len(STATUS)), np.int64)
    np.add.at(m, (kind, status), 1)
    row["matrix"] = m.tolist()
    # corroboration: nDSM rise on built_undeclared (ok cells), nDSM drop on declared_built_absent
    dh = o["dh"].astype(np.float32) * 0.25
    dh_ok = o["dh"] != -128
    ok = status == SI["ok"]
    bu = ok & (kind == KI["built_undeclared"]) & dh_ok
    ba = ok & (kind == KI["declared_built_absent"]) & dh_ok
    fo = ok & (kind == KI["forest_declared_open"]) & dh_ok
    row["built_undeclared_dh"] = {"n": int(bu.sum()), "rise_ge2m": int((bu & (dh >= DH_NEW_M)).sum())}
    row["declared_built_absent_dh"] = {"n": int(ba.sum()), "drop_le2m": int((ba & (dh <= -DH_NEW_M)).sum())}
    row["forest_declared_open_dh"] = {"n": int(fo.sum()), "drop_le2m": int((fo & (dh <= -DH_NEW_M)).sum()),
                                      "hansen_loss": int((fo & (o["hansen"] > 0) & (o["hansen"] != 255)).sum())}
    # the observed-layer peer's own verdict vs ours (ok cells)
    v = o["verdict"]
    row["srtm_verdict_nonconsistent_ok"] = int((ok & (v != 0) & (v != VERDICT_UNKNOWN)).sum())
    row["srtm_verdict_consistent_but_diff_ok"] = int((ok & (v == 0) & (kind != KI["agree"])).sum())
    return row


# ── phases ────────────────────────────────────────────────────────────────────

def kgs_with_obs(base):
    r = requests.get(f"{base}/api/v1/ne/manifest", timeout=120)
    r.raise_for_status()
    man = r.json()
    kgs = man.get("kgs", {})
    return sorted(kg for kg, k in kgs.items() if isinstance(k, dict) and k.get("obs") and k.get("lu"))


def phase_als(a):
    """Observed-layer peer als{} per KG. One bulk GET (/api/v1/kgs?fields=codes,als&processed_only=0, ~1 MB);
    per-KG /api/v1/kg/<code> at 5 r/s only for KGs the bulk answer lacks."""
    path = os.path.join(a.out, "als.json")
    als = json.load(open(path)) if os.path.exists(path) else {}
    s = requests.Session()
    try:
        r = s.get(f"{a.srtm}/api/v1/kgs", params={"fields": "codes,als", "processed_only": "0"}, timeout=120)
        r.raise_for_status()
        n = 0
        for row in r.json().get("kgs", []):
            if isinstance(row, dict) and row.get("kg_code") and isinstance(row.get("als"), dict):
                als[str(row["kg_code"]).zfill(5)] = row["als"]; n += 1
        print(f"als: bulk {n} KGs", flush=True)
        json.dump(als, open(path, "w"))
    except Exception as e:  # noqa: BLE001
        print(f"als: bulk fetch failed ({str(e)[:80]}), falling back to per-KG", flush=True)
    kgs = [k for k in kgs_with_obs(a.base) if k not in als or "error" in als[k]]
    print(f"als: {len(als)} held, {len(kgs)} to fetch per KG", flush=True)
    for i, kg in enumerate(kgs):
        try:
            r = s.get(f"{a.srtm}/api/v1/kg/{kg}", timeout=30)
            als[kg] = r.json().get("als") or {"error": r.status_code} if r.status_code == 200 else {"error": r.status_code}
        except Exception as e:  # noqa: BLE001
            als[kg] = {"error": str(e)[:80]}
        time.sleep(0.22)
        if i % 200 == 199:
            json.dump(als, open(path, "w"))
            print(f"  {i+1}/{len(kgs)}", flush=True)
    json.dump(als, open(path, "w"))
    print(f"als: {len(als)} KGs")


def phase_diff(a):
    path = os.path.join(a.out, "diff.jsonl")
    done = set()
    if os.path.exists(path) and not a.fresh:
        for line in open(path):
            done.add(json.loads(line)["kg"])
    als = json.load(open(os.path.join(a.out, "als.json"))) if os.path.exists(os.path.join(a.out, "als.json")) else {}
    kgs = a.kgs.split(",") if a.kgs else kgs_with_obs(a.base)
    if a.sample:
        kgs = kgs[:: max(1, len(kgs) // a.sample)][: a.sample]
    kgs = [k for k in kgs if k not in done]
    print(f"diff: {len(done)} done, {len(kgs)} to do", flush=True)
    t0 = time.time()
    with open(path, "a" if not a.fresh else "w") as f:
        for i, kg in enumerate(kgs):
            lu, obs, st = fetch_kg(a.base, kg)
            if lu is None or obs is None:
                f.write(json.dumps({"kg": kg, "error": st}) + "\n")
                continue
            row = kg_row(kg, lu, obs, als.get(kg) or {})
            f.write(json.dumps(row) + "\n")
            if i % 50 == 49:
                f.flush()
                print(f"  {i+1}/{len(kgs)}  {time.time()-t0:.0f}s", flush=True)


def phase_change(a):
    """KGs with confirmed CHANGE cells: classify each changed cell against the declared row BEFORE
    (= now − d_lu) and AFTER (= now) the confirmed delta → resolved / persisted / introduced."""
    import h3
    path = os.path.join(a.out, "change.jsonl")
    als = json.load(open(os.path.join(a.out, "als.json"))) if os.path.exists(os.path.join(a.out, "als.json")) else {}
    obs_kgs = set(kgs_with_obs(a.base))
    kgs = []
    for kg in sorted(obs_kgs):  # /ne/stats?kg= is public and cheap; the change block says who has confirmed cells
        s = requests.get(f"{a.base}/api/v1/ne/stats", params={"kg": kg}, timeout=60).json()
        if ((s.get("change") or {}).get("confirmed") or {}).get("cells", 0) > 0:
            kgs.append(kg)
    if a.kgs:
        kgs = a.kgs.split(",")
    print(f"change: {len(kgs)} KGs with confirmed change + obs", flush=True)
    out = open(path, "w")
    for kg in kgs:
        lu, obs, st = fetch_kg(a.base, kg)
        if lu is None or obs is None:
            continue
        l, o = join(lu, obs)
        ids = [h3.int_to_str(int(c)) for c in l["cell"]]
        chg = {}
        for i in range(0, len(ids), 16384):
            rr = requests.post(f"{a.base}/api/v1/ne/cells?layers=chg&format=json", json={"ids": ids[i:i + 16384]}, timeout=300)
            if rr.status_code != 200:
                break
            j = rr.json()
            for cid, c in zip(j["ids"], j["chg"]["cells"]):
                if c and c.get("status") == "confirmed" and c.get("d_lu_m2") is not None:
                    chg[cid] = c
        if not chg:
            continue
        idx = {cid: i for i, cid in enumerate(ids)}
        sel = np.array([idx[c] for c in chg], np.int64)
        ls, os_ = l[sel], o[sel]
        area = np.array([h3.cell_area(c, unit="m^2") for c in chg], np.float32)
        dlu = np.array([chg[c]["d_lu_m2"] for c in chg], np.float32)
        dec_after = ls["lu"].astype(np.float32) / 255.0
        dec_before = np.clip(dec_after - dlu / area[:, None], 0, 1)
        ob = os_["lu"].astype(np.float32) / 255.0
        ka = classify(dec_after, ob, dec_after.argmax(1), ob.argmax(1))
        kb = classify(dec_before, ob, dec_before.argmax(1), ob.argmax(1))
        agree = KI["agree"]
        trans = collections.Counter()
        for x, y in zip(kb, ka):
            trans[f"{KINDS[x]}>{KINDS[y]}"] += 1
        row = {"kg": kg, "confirmed_cells": len(chg), "als_newest": (als.get(kg) or {}).get("newest"),
               "resolved": int(((kb != agree) & (ka == agree)).sum()), "persisted": int(((kb != agree) & (ka != agree)).sum()),
               "introduced": int(((kb == agree) & (ka != agree)).sum()), "agree_both": int(((kb == agree) & (ka == agree)).sum()),
               "transitions": dict(trans.most_common(12))}
        out.write(json.dumps(row) + "\n")
        out.flush()
        print(f"  {kg}: {row['confirmed_cells']} confirmed, resolved {row['resolved']} persisted {row['persisted']} introduced {row['introduced']}", flush=True)
    out.close()


def phase_report(a):
    rows = [json.loads(x) for x in open(os.path.join(a.out, "diff.jsonl"))]
    err = [r for r in rows if "error" in r]
    rows = [r for r in rows if "error" not in r]
    ap_ = os.path.join(a.out, "als.json")
    if os.path.exists(ap_):
        als = json.load(open(ap_))
        for r in rows:
            x = als.get(r["kg"]) or {}
            for k in ("newest", "oldest", "dominant_share", "reflown_share", "precision"):
                if k in x:
                    r["als_" + k] = x[k]
    M = np.array([r["matrix"] for r in rows], np.int64)  # (kg, kind, status)
    tot = M.sum(0)
    cells = int(tot.sum())
    L = []
    p = lambda s="": L.append(s)
    p(f"DIFF declared vs observed — {len(rows)} KGs ({len(err)} errors), {cells:,} joined cells")
    p()
    p("Filter status (all cells):")
    for s in STATUS:
        n = int(tot[:, SI[s]].sum())
        p(f"  {s:18s} {n:>12,}  {100*n/cells:5.1f} %")
    ok = tot[:, SI["ok"]]
    nok = int(ok.sum())
    p()
    p(f"Kind on cells with status ok ({nok:,} = {100*nok/cells:.1f} % of joined):")
    p(f"  {'kind':22s} {'ok':>12s} {'% ok':>6s}   {'all':>12s} {'% all':>6s}")
    for k in KINDS:
        n, na = int(ok[KI[k]]), int(tot[KI[k]].sum())
        p(f"  {k:22s} {n:>12,} {100*n/nok:6.2f}   {na:>12,} {100*na/cells:6.2f}")
    # epoch dependence (KG-level ALS newest year)
    p()
    p("Diff rate (ok cells, kind ≠ agree) by ALS newest year (KG-level, peer als.newest):")
    by = collections.defaultdict(lambda: np.zeros((len(KINDS),), np.int64))
    nk = collections.Counter()
    for r, m in zip(rows, M):
        y = r.get("als_newest") or "?"
        by[y] += m[:, SI["ok"]]
        nk[y] += 1
    p(f"  {'year':>5s} {'KGs':>5s} {'ok cells':>12s} {'diff %':>7s}  built_undecl %  decl_built_abs %  forest_open %  forest_undecl %  arable_grass %  bauland_green %")
    for y in sorted(by, key=lambda v: (str(v) == "?", str(v))):
        v = by[y]
        n = int(v.sum())
        if n == 0:
            continue
        d = n - int(v[KI["agree"]])
        f = lambda k: 100 * v[KI[k]] / n
        p(f"  {str(y):>5s} {nk[y]:>5d} {n:>12,} {100*d/n:7.2f}  {f('built_undeclared'):13.2f}  {f('declared_built_absent'):15.2f}  {f('forest_declared_open'):12.2f}  {f('forest_undeclared'):14.2f}  {f('arable_grass'):13.2f}  {f('building_land_green'):13.2f}")
    # mixed-epoch KGs
    mixed = [r for r in rows if (r.get("als_dominant_share") or 1) < 0.9]
    p()
    p(f"KGs whose newest flight is mixed (dominant_share < 0.9): {len(mixed)} of {len(rows)}; precision "
      + str(collections.Counter(r.get('als_precision') for r in rows).most_common()))
    # corroboration
    bu = sum(r["built_undeclared_dh"]["n"] for r in rows); bur = sum(r["built_undeclared_dh"]["rise_ge2m"] for r in rows)
    ba = sum(r["declared_built_absent_dh"]["n"] for r in rows); bad = sum(r["declared_built_absent_dh"]["drop_le2m"] for r in rows)
    fo = sum(r["forest_declared_open_dh"]["n"] for r in rows); fod = sum(r["forest_declared_open_dh"]["drop_le2m"] for r in rows)
    foh = sum(r["forest_declared_open_dh"]["hansen_loss"] for r in rows)
    p()
    p("Corroboration by the observed layer's own time axis (ok cells with dh_m ≠ null):")
    p(f"  built_undeclared:      {bu:,} cells, nDSM rise ≥ 2 m newest−oldest flight: {bur:,} ({100*bur/max(bu,1):.1f} %) → built since the older flight")
    p(f"  declared_built_absent: {ba:,} cells, nDSM drop ≤ −2 m: {bad:,} ({100*bad/max(ba,1):.1f} %) → demolished since the older flight")
    p(f"  forest_declared_open:  {fo:,} cells, nDSM drop ≤ −2 m: {fod:,} ({100*fod/max(fo,1):.1f} %), Hansen loss year set: {foh:,} ({100*foh/max(fo,1):.1f} %)")
    sv = sum(r["srtm_verdict_nonconsistent_ok"] for r in rows); sc = sum(r["srtm_verdict_consistent_but_diff_ok"] for r in rows)
    p(f"  the observed-layer peer's own consistency ≠ consistent on ok cells: {sv:,}; peer consistent but our kind ≠ agree: {sc:,}")
    # per-KG spread: share of KGs where a kind exceeds 5 % of ok cells
    p()
    p("Per-KG concentration (ok cells): KGs where the kind is ≥ 5 % of the KG's ok cells / median KG share:")
    okm = M[:, :, SI["ok"]].astype(np.float64)
    okn = okm.sum(1)
    for k in KINDS[1:]:
        sh = np.where(okn > 0, okm[:, KI[k]] / np.maximum(okn, 1), 0)
        p(f"  {k:22s} {int((sh >= 0.05).sum()):>5d} KGs   median {100*np.median(sh):5.2f} %   p90 {100*np.percentile(sh, 90):5.2f} %")
    # the noise cut: what would be shown
    p()
    p("Noise cut (what survives): status ok, kind in the shown set, KG flown ≥ 2019 and unmixed (dominant_share ≥ 0.9):")
    shown = ["built_undeclared", "declared_built_absent", "forest_declared_open", "forest_undeclared", "building_land_green", "sealed_undeclared"]
    keep = [i for i, r in enumerate(rows) if isinstance(r.get("als_newest"), int) and r["als_newest"] >= 2019 and (r.get("als_dominant_share") or 0) >= 0.9]
    okk = M[keep][:, :, SI["ok"]].sum(0) if keep else np.zeros(len(KINDS), np.int64)
    nk_ = int(okk.sum())
    p(f"  {len(keep)} KGs, {nk_:,} ok cells")
    for k in shown:
        p(f"  {k:22s} {int(okk[KI[k]]):>12,}  {100*okk[KI[k]]/max(nk_,1):6.2f} %  (all KGs: {100*ok[KI[k]]/nok:6.2f} %)")
    p(f"  dropped from the shown set as vocabulary/classifier noise: arable_grass, alpine_pasture, traffic, water, alpine, other"
      f" = {100*sum(ok[KI[k]] for k in ['arable_grass','alpine_pasture','traffic','water','alpine','other'])/nok:.1f} % of ok cells")
    cp = os.path.join(a.out, "change.jsonl")
    if os.path.exists(cp):
        ch = [json.loads(x) for x in open(cp)]
        p()
        p(f"CHANGE join — {len(ch)} KGs with confirmed change cells and an observed layer:")
        tr = collections.Counter()
        for r in ch:
            tr.update(r["transitions"])
        R = sum(r["resolved"] for r in ch); P = sum(r["persisted"] for r in ch); I = sum(r["introduced"] for r in ch); A = sum(r["agree_both"] for r in ch)
        p(f"  confirmed cells {R+P+I+A:,}: diff resolved by the change {R:,} · persisted {P:,} · introduced {I:,} · agree before and after {A:,}")
        p("  top before>after transitions: " + ", ".join(f"{k} {v}" for k, v in tr.most_common(15)))
    txt = "\n".join(L)
    open(os.path.join(a.out, "report.md"), "w").write(txt + "\n")
    print(txt)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("phase", choices=["als", "diff", "change", "report"])
    ap.add_argument("--base", default="http://localhost:8000")
    ap.add_argument("--srtm", default=os.environ.get("UMFELD_PEER_OBS_API", ""))
    ap.add_argument("--out", default=os.path.join(os.path.dirname(__file__), "..", "data", "ne_diff"))
    ap.add_argument("--sample", type=int, default=0, help="evenly spaced sample of N KGs")
    ap.add_argument("--kgs", default="", help="comma list of KG codes")
    ap.add_argument("--fresh", action="store_true", help="diff: discard diff.jsonl")
    a = ap.parse_args()
    os.makedirs(a.out, exist_ok=True)
    {"als": phase_als, "diff": phase_diff, "change": phase_change, "report": phase_report}[a.phase](a)


if __name__ == "__main__":
    main()
