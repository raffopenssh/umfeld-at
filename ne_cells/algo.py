"""ne-cells-1 — the reference algorithm. Deterministic by construction:

* no latitude constant: area SHARES are ratios of areas in the same (degree) space, which any
  affine map leaves invariant; metres (line_m_ha) use a cell-local affine from the cell's own centre;
* no set/dict iteration order reaches the output: everything is sorted (cell ids, parcel ids, WKB);
* no float statistics: percentiles are integer index picks on sorted integer m² attributes;
* quantisation is round-half-up on exact rationals of the same floats, never banker's rounding.

Same canonical input ⇒ bit-identical records (pack.py) ⇒ identical digest.
"""
import collections
import math
import h3
import shapely
from shapely import affinity
from shapely.geometry import Polygon
from shapely.ops import unary_union
from shapely.strtree import STRtree

from .ns_groups import GROUP_ORDER, NS_TABLE_VERSION, group_of

ALGO = "ne-cells-2"      # bump on ANY change to this file or ns_groups.py
INPUT_PAD_DEG = 0.004    # input domain = bbox(own KG parcels) + this pad, rounded outward to 4 dp
INPUT_RULE = "geom-intersects-input_bbox"   # an object is INPUT iff its canonical geometry intersects input_bbox
H3_RES = 12              # lu / bld_cover / fabric layer (~307 m², ~20 m across)
K_RES = 10               # k≥K record-derived distributions (~15 000 m²)
K = 5
MIN_LAND = 0.02          # omit slivers: cells with < 2 % cadastral land
COV_UNKNOWN = 0.90       # Σlu/land below this ⇒ flag DECLARED_UNKNOWN (no verdict)
FLAG_DECLARED_UNKNOWN = 0x01
FLAG_NORMALISED = 0x02   # Σlu exceeded land (tile-overlap double count) and was scaled down
NONE8 = 0xFF
NONE16 = 0xFFFF
CELL_PAD_DEG = 0.0005    # ≥ one res-12 cell diameter; enumerating cells that touch the KG


def q255(v: float) -> int:
    """round-half-up of v·255 into 0..255."""
    if v <= 0:
        return 0
    if v >= 1:
        return 255
    return min(255, int(v * 255 + 0.5))


def _poly_parts(g):
    if g is None or g.is_empty:
        return []
    if g.geom_type == "Polygon":
        return [g]
    if g.geom_type == "MultiPolygon":
        return list(g.geoms)
    if g.geom_type == "GeometryCollection":
        out = []
        for p in g.geoms:
            out.extend(_poly_parts(p))
        return out
    return []


def _area(g) -> float:
    return sum(p.area for p in _poly_parts(g))


def _union_sorted(geoms):
    """unary_union over a WKB-sorted list → order-independent result."""
    gs = [g for g in geoms if g is not None and not g.is_empty]
    if not gs:
        return None
    if len(gs) == 1:
        return gs[0]
    gs.sort(key=lambda g: g.wkb)
    return unary_union(gs)


def cell_polygon(cell: str) -> Polygon:
    return Polygon([(lng, lat) for lat, lng in h3.cell_to_boundary(cell)])


def cell_affine(cell: str):
    """cell-local degrees→metres affine (equirectangular at the cell centre)."""
    lat, _ = h3.cell_to_latlng(cell)
    mx = 111320.0 * math.cos(math.radians(lat))
    return [mx, 0, 0, 110574.0, 0, 0]


def footprint_axis_deg(gm) -> float:
    """Long-side axis of the minimum rotated rectangle, 0..180 (0 = east-west). Computed here from the
    geometry — never taken from an attribute — so both sources yield the same value."""
    mrr = gm.minimum_rotated_rectangle
    if mrr.geom_type != "Polygon":
        return 0.0
    xs, ys = mrr.exterior.coords.xy
    best, blen = 0.0, -1.0
    for i in range(4):
        dx, dy = xs[i + 1] - xs[i], ys[i + 1] - ys[i]
        l = math.hypot(dx, dy)
        if l > blen:
            blen, best = l, math.degrees(math.atan2(dy, dx)) % 180.0
    return best


def _lower_median(sorted_vals):
    return sorted_vals[(len(sorted_vals) - 1) // 2]


def _pct(sorted_vals, p):
    return sorted_vals[int(p * (len(sorted_vals) - 1))]


class BuildResult:
    def __init__(self, kg, cells, kcells, stats, input_bbox=None):
        self.kg, self.cells, self.kcells, self.stats, self.input_bbox = kg, cells, kcells, stats, input_bbox


def _round_out(v, places, up):
    f = 10 ** places
    return (math.ceil(v * f) if up else math.floor(v * f)) / f


def default_input_bbox(own_geoms, pad=INPUT_PAD_DEG):
    """bbox(own KG parcels) padded by `pad`, rounded OUTWARD to 4 decimals — the extent a peer
    must supply complete objects for. Written into the header so a rebuild can pass it back."""
    xs0, ys0, xs1, ys1 = zip(*(g.bounds for g in own_geoms))
    return (_round_out(min(xs0) - pad, 4, False), _round_out(min(ys0) - pad, 4, False),
            _round_out(max(xs1) + pad, 4, True), _round_out(max(ys1) + pad, 4, True))


def _in_domain(objs, bbox):
    """ne-cells-2 input rule: keep objects whose canonical geometry intersects the input bbox (as a
    closed box). Makes the record block independent of how much MORE a fetch returned."""
    W, S, E, N = bbox
    box = shapely.box(W, S, E, N)
    prep = shapely.prepared.prep(box)
    return [o for o in objs if prep.intersects(o["geometry"])]


def build_cells(kg: str, parcels, footprints, landuse, coverage_bbox=None, input_bbox=None) -> BuildResult:
    """Compute the KG product for `kg` from the objects supplied (neighbouring KGs included so
    that border cells are KG-independent). Inputs are canonical records (canon.py).

    input_bbox (W,S,E,N): the INPUT DOMAIN. Default = bbox(own parcels) + INPUT_PAD_DEG rounded
    outward to 4 dp (`default_input_bbox`). Only objects whose geometry intersects it are used
    (`INPUT_RULE`), so a fetch that returned more than the domain yields the same bytes. A peer
    must supply a COMPLETE copy of every object intersecting it (fetch every bevdirect cell that
    intersects input_bbox). The value is written to the header; pass it back to reproduce.
    coverage_bbox (W,S,E,N): extent inside which the input is complete; res-10 k-cells not fully
    inside it are withheld (their parcel population would be truncated). Default = input_bbox."""
    kg = str(kg).zfill(5)
    # ── dedupe (tile copies) and sort ─────────────────────────────────────────────────
    copies = collections.defaultdict(list)  # several copies of one parcel (bevdirect cell pads)
    for p in parcels:
        copies[p["id"]].append(p)
    byid = {}
    for pid, cs in copies.items():
        complete = [p for p in cs if p.get("complete", True)]
        if complete:                        # a complete copy wins (largest, then WKB, if several)
            byid[pid] = max(complete, key=lambda p: (p["geometry"].area, p["geometry"].wkb))
        else:                               # only truncated copies: their UNION is the most complete
            u = _union_sorted([p["geometry"] for p in cs])   # geometry we can know (a parcel wider
            q = dict(max(cs, key=lambda p: (p["geometry"].area, p["geometry"].wkb)))  # than the pad has
            q["geometry"] = u                                 # a different truncation in every cell)
            byid[pid] = q
    P = sorted(byid.values(), key=lambda p: p["id"])
    F = sorted({f["geometry"].wkb: f for f in footprints}.values(), key=lambda f: f["geometry"].wkb)
    L = sorted({(l["ns"], l["geometry"].wkb): l for l in landuse}.values(), key=lambda l: (l["ns"], l["geometry"].wkb))
    own = [p for p in P if p["kg"] == kg]
    if not own:
        return BuildResult(kg, {}, {}, dict(parcels=0, footprints=len(F), landuse=len(L), cells=0, kcells=0), input_bbox)
    if input_bbox is None:
        input_bbox = default_input_bbox([p["geometry"] for p in own])
    input_bbox = tuple(float(v) for v in input_bbox)
    if coverage_bbox is None:
        coverage_bbox = input_bbox
    n_in = (len(P), len(F), len(L))
    P, F, L = _in_domain(P, input_bbox), _in_domain(F, input_bbox), _in_domain(L, input_bbox)
    own = [p for p in P if p["kg"] == kg]
    own_union = _union_sorted([p["geometry"] for p in own])
    # ── cells touching the KG ─────────────────────────────────────────────────────────
    cand = set(h3.geo_to_cells(shapely.buffer(own_union, CELL_PAD_DEG), H3_RES))
    cellids = sorted(cand)
    cellpoly = {c: cell_polygon(c) for c in cellids}
    tree = STRtree([cellpoly[c] for c in cellids])
    own_prepared = shapely.prepared.prep(own_union)
    keep = [c for c in cellids if own_prepared.intersects(cellpoly[c])]
    # a cell is a PRODUCT cell only if it lies fully inside the input domain: for a whole-KG build
    # (domain = KG + pad) this drops nothing; for a partial-area build it is what makes the output
    # a function of the fetched area alone.
    W, S, E, N = input_bbox
    n_keep = len(keep)
    keep = [c for c in keep if (lambda b: b[0] >= W and b[1] >= S and b[2] <= E and b[3] <= N)(cellpoly[c].bounds)]
    cells_outside = n_keep - len(keep)
    keepset = set(keep)
    cellpoly = {c: cellpoly[c] for c in keep}
    tree = STRtree([cellpoly[c] for c in keep])

    acc = {c: dict(land=[], own=[], g=[], ids=set(), line=0.0, lu=collections.defaultdict(list), fp=[],
                   bld_n=0, bld_or=[]) for c in keep}
    kacc = collections.defaultdict(lambda: dict(ids={}, ez=set(), bld=[]))

    def hits(g):
        return sorted(int(i) for i in tree.query(g))

    for p in P:
        g = p["geometry"]
        for i in hits(g):
            c = keep[i]
            cp = cellpoly[c]
            x = g.intersection(cp)
            if x.is_empty or _area(x) <= 0:
                continue
            d = acc[c]
            d["land"].append(x)
            d["ids"].add(p["id"])
            if p["kg"] == kg:
                d["own"].append(x)
            if p["status"] == "G":
                d["g"].append(x)
            b = g.boundary.intersection(cp)
            if not b.is_empty:
                d["line"] += affinity.affine_transform(b, cell_affine(c)).length / 2.0
            kp = h3.cell_to_parent(c, K_RES)
            kd = kacc[kp]
            kd["ids"][p["id"]] = int(p["area_sqm"] + 0.5)
            if p["ez"]:
                kd["ez"].add(p["ez"])
    for f in F:
        g = f["geometry"]
        for i in hits(g):
            c = keep[i]
            x = g.intersection(cellpoly[c])
            if not x.is_empty and _area(x) > 0:
                acc[c]["fp"].append(x)
        ctr = g.centroid
        c0 = h3.latlng_to_cell(ctr.y, ctr.x, H3_RES)
        if c0 in keepset:
            gm = affinity.affine_transform(g, cell_affine(c0))   # metres, cell-local
            acc[c0]["bld_n"] += 1
            acc[c0]["bld_or"].append(footprint_axis_deg(gm))
            kacc[h3.cell_to_parent(c0, K_RES)]["bld"].append(int(gm.area + 0.5))
    for l in L:
        grp = group_of(l["ns"])
        g = l["geometry"]
        for i in hits(g):
            c = keep[i]
            x = g.intersection(cellpoly[c])
            if not x.is_empty and _area(x) > 0:
                acc[c]["lu"][grp].append(x)

    # ── per-cell rows ─────────────────────────────────────────────────────────────────
    rows = {}
    st = collections.Counter()
    for c in keep:
        d = acc[c]
        cp = cellpoly[c]
        cell_a = cp.area
        land_g = _union_sorted(d["land"])
        land_a = _area(land_g)
        land = land_a / cell_a
        if land < MIN_LAND:
            st["slivers_omitted"] += 1
            continue
        own_a = _area(_union_sorted(d["own"]))
        kg_share = own_a / cell_a
        if q255(kg_share) == 0:
            st["no_own_share"] += 1
            continue
        geb_g = _union_sorted(d["fp"] + d["lu"].get("geb", []))
        geb_a = _area(geb_g)
        shares = [0.0] * 9
        shares[0] = geb_a / cell_a
        for gi, grp in enumerate(GROUP_ORDER):
            if gi == 0:
                continue
            u = _union_sorted(d["lu"].get(grp, []))
            if u is None:
                continue
            if geb_g is not None:            # buildings win: March ns=48 is not cut around them
                u = u.difference(geb_g)
            shares[gi] = _area(u) / cell_a
        tot = sum(shares)
        flags = 0
        cov = tot / land if land > 0 else 0.0
        if tot > land and tot > 0:
            shares = [s * land / tot for s in shares]
            if tot - land >= 1.0 / 255:        # flag only a visible double count, not float noise
                flags |= FLAG_NORMALISED
                st["normalised"] += 1
        if cov < COV_UNKNOWN:
            flags |= FLAG_DECLARED_UNKNOWN
            st["declared_unknown"] += 1
        lu = [q255(s) for s in shares]
        bld_cover = q255(_area(_union_sorted(d["fp"])) / cell_a)
        coh, axis = NONE8, NONE8
        if d["bld_n"] >= K:
            xs = sum(math.cos(math.radians(2 * a)) for a in d["bld_or"])
            ys = sum(math.sin(math.radians(2 * a)) for a in d["bld_or"])
            coh = q255(math.hypot(xs, ys) / len(d["bld_or"]))
            axis = int(((math.degrees(math.atan2(ys, xs)) / 2.0) % 180.0) / 2.0) % 90
        land_m2 = affinity.affine_transform(land_g, cell_affine(c)).area if land_g is not None else 0.0
        line = int(d["line"] / (land_m2 / 1e4) + 0.5) if land_m2 > 50 else 0
        rows[c] = dict(kg_share=q255(kg_share), land=q255(land), cov=q255(min(cov, 1.0)), flags=flags,
                       lu=lu, bld_cover=bld_cover, bld_n=min(NONE16 - 1, d["bld_n"]),
                       gk=q255(_area(_union_sorted(d["g"])) / land_a) if land_a > 0 else 0,
                       n_parc=min(255, len(d["ids"])), line_m_ha=min(NONE16 - 1, line),
                       orient_coh=coh, orient_axis=axis)
    # ── res-10 k-layer ────────────────────────────────────────────────────────────────
    kcells = {}
    parents = sorted({h3.cell_to_parent(c, K_RES) for c in rows})
    for kp in parents:
        kd = kacc.get(kp)
        if not kd or len(kd["ids"]) < K:
            continue
        if coverage_bbox is not None:
            W, S, E, N = coverage_bbox
            x0, y0, x1, y1 = cell_polygon(kp).bounds
            if x0 < W or y0 < S or x1 > E or y1 > N:
                st["kcells_withheld_coverage"] += 1
                continue
        areas = sorted(kd["ids"].values())
        n = len(areas)
        ezn = len(kd["ez"])
        blds = sorted(kd["bld"])
        kcells[kp] = dict(n_parc=min(NONE16 - 1, n), parc_p10=_pct(areas, 0.1), parc_p50=_lower_median(areas),
                          parc_p90=_pct(areas, 0.9), ez_n=min(NONE16 - 1, ezn),
                          parc_per_ez=min(254, (n * 10) // ezn) if ezn else NONE8,
                          bld_n=min(NONE16 - 1, len(blds)),
                          bld_p50=min(NONE16 - 1, _lower_median(blds)) if len(blds) >= K else NONE16)
    st.update(dict(parcels=len(P), own_parcels=len(own), footprints=len(F), landuse=len(L),
                   parcels_outside_domain=n_in[0] - len(P), footprints_outside_domain=n_in[1] - len(F),
                   landuse_outside_domain=n_in[2] - len(L), cells_outside_domain=cells_outside,
                   cells=len(rows), kcells=len(kcells)))
    return BuildResult(kg, rows, kcells, dict(st), input_bbox)
