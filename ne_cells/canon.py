"""Canonical input: adapters from a per-layer row export (`*_from_index`) and from
bevdirect-serve `/viewport` documents (`from_bevdirect`) to one record schema. Everything
downstream only sees these."""
import shapely
from shapely.geometry import shape
from shapely.ops import unary_union

PRECISION = 1e-7   # coordinate grid; the index serialises 7 decimals (`coord`)


def canon_geom(gj):
    """GeoJSON → valid polygonal geometry snapped to the 1e-7 grid (kills sub-serialisation noise)."""
    if not gj:
        return None
    g = shape(gj)
    try:
        g = shapely.set_precision(g, PRECISION, mode="valid_output")
    except Exception:                       # invalid input: validate first, then snap
        g = shapely.set_precision(shapely.make_valid(g), PRECISION, mode="valid_output")
    g = shapely.make_valid(g)
    if g.geom_type == "GeometryCollection":
        parts = [p for p in g.geoms if p.geom_type in ("Polygon", "MultiPolygon")]
        g = unary_union(parts) if parts else None
    if g is None or g.is_empty or g.geom_type not in ("Polygon", "MultiPolygon"):
        return None
    return g


def parcels_from_index(rows):
    out = []
    for r in rows:
        g = canon_geom(r.get("geometry"))
        if g is None:
            continue
        out.append(dict(id=str(r["parcel_id"]), kg=str(r["kg_code"]).zfill(5),
                        status="G" if str(r.get("status", "")).upper().startswith("G") else "E",
                        ez=str(r["ez"]) if r.get("ez") not in (None, "", 0) else None,
                        area_sqm=float(r.get("area_sqm") or 0.0), complete=True, geometry=g))
    return out


def footprints_from_index(rows):
    out = []
    for r in rows:
        g = canon_geom(r.get("geometry"))
        if g is None:
            continue
        out.append(dict(geometry=g))
    return out


def landuse_from_index(rows):
    out = []
    for r in rows:
        g = canon_geom(r.get("geometry"))
        if g is None:
            continue
        out.append(dict(ns=str(r.get("code", r.get("ns", ""))).strip(), geometry=g))
    return out


def from_bevdirect(docs):
    """One or more bevdirect-serve `/viewport` documents (0.02° cells; a map client's `/api/viewport` has the
    same rows). Parcels: `parcel_id, kg_code, rstatus, ez, area_sqm, complete, geometry`; footprints:
    `id, ns, area_sqm, geometry`; landuse: `id, ns, area_sqm, geometry`. A parcel that crosses a cell's
    0.004° assembly pad appears in several cells, `complete:false` in all but one — algo.py keeps the
    complete copy (then the largest), the same rule the map client applies ("complete copy wins")."""
    if isinstance(docs, dict):
        docs = [docs]
    P, F, L = [], [], []
    for doc in docs:
        p, f, l = _one_bevdirect(doc)
        P += p; F += f; L += l
    return P, F, L


def bevdirect_source(docs) -> str:
    """`bevdirect@<version>` from the documents' `bevdirect_version` (bevdirect-serve ≥ v0.2.0 emits
    it). All documents must agree; a document without it (v0.1.0, or a client's own serialiser)
    cannot be pinned and raises — pass --source bevdirect@<tag> explicitly in that case."""
    if isinstance(docs, dict):
        docs = [docs]
    vers = {str(d.get("bevdirect_version") or "") for d in docs}
    if "" in vers:
        raise ValueError("bevdirect document without bevdirect_version — not pinnable; pass --source bevdirect@<tag>")
    if len(vers) != 1:
        raise ValueError(f"mixed bevdirect versions in one build: {sorted(vers)}")
    return "bevdirect@" + vers.pop()


def _one_bevdirect(doc):
    P = []
    for r in doc.get("parcels", []):
        g = canon_geom(r.get("geometry"))
        if g is None:
            continue
        kg = str(r.get("kg_code") or r.get("kg") or "").zfill(5)
        pid = r.get("parcel_id") or f"{kg}-{r.get('gnr', '')}"
        P.append(dict(id=str(pid), kg=kg, status="G" if str(r.get("rstatus") or r.get("status") or "").upper().startswith("G") else "E",
                      ez=str(r["ez"]) if r.get("ez") not in (None, "", 0) else None,
                      area_sqm=float(r.get("area_sqm") or r.get("area") or 0.0),
                      complete=bool(r.get("complete", True)), geometry=g))
    F = []
    for r in doc.get("footprints", []):
        g = canon_geom(r.get("geometry"))
        if g is None:
            continue
        F.append(dict(geometry=g))
    L = []
    for r in doc.get("landuse", []):
        g = canon_geom(r.get("geometry"))
        if g is None:
            continue
        L.append(dict(ns=str(r.get("ns", r.get("code", ""))).strip(), geometry=g))
    return P, F, L
