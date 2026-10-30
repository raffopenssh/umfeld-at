"""CLI.
  python3 -m ne_cells build --kg <kg> --epoch 2026-10 --bevdirect cell1.json [--bevdirect cell2.json …] -o today.nec
      (source = bevdirect@<bevdirect_version> read from the documents; --source bevdirect@<tag> to override)
  python3 -m ne_cells build --kg <kg> --epoch 2026-03 --source <source> \
      --parcels parcels.json --footprints footprints.json --landuse landuse.json [--input-bbox W,S,E,N] -o out.nec
  python3 -m ne_cells dump out.nec              → JSON (header + cells + kcells) on stdout
  python3 -m ne_cells compare a.nec b.nec [--tol 8]
  python3 -m ne_cells report today.nec --observer <label> [--bbox W,S,E,N]   → epoch report JSON (digests only)

Fetching the cells and posting the report is `vtcseamless observe` (package vtcseamless).
"""
import argparse
import hashlib
import json
import os
import sys
import time

from . import canon
from .algo import ALGO, H3_RES, K_RES, K, INPUT_PAD_DEG, INPUT_RULE, build_cells
from .pack import MAGIC_LU, lu_header, pack_records, pack_section, unpack_sections, epoch_report


def _load(path):
    with open(path) as f:
        return json.load(f)


def content_sha256(doc) -> str:
    """Provenance hash over CANONICAL content, so two fetches of the same rows compare equal
    regardless of fetched_at / row order / whitespace: for each layer list (parcels, footprints,
    landuse) the rows are serialised as canonical JSON (sorted keys, compact), sorted, and hashed as
    "<layer>\n" + rows joined by "\n". Document-level keys (fetched_at, bbox, …) are NOT included."""
    h = hashlib.sha256()
    for key in ("parcels", "footprints", "landuse"):
        rows = doc.get(key) if isinstance(doc, dict) else None
        if not isinstance(rows, list):
            continue
        h.update((key + "\n").encode())
        for r in sorted(json.dumps(r, sort_keys=True, separators=(",", ":"), ensure_ascii=True) for r in rows):
            h.update(r.encode())
            h.update(b"\n")
    return h.hexdigest()[:16]


def file_prov(path, rows_key=None, doc=None):
    """Provenance entry for the header's inputs[] (informative, not covered by the digest)."""
    with open(path, "rb") as f:
        b = f.read()
    if doc is None:
        doc = json.loads(b)
    d = dict(file=os.path.basename(path), sha256=content_sha256(doc), file_sha256=hashlib.sha256(b).hexdigest()[:16],
             bytes=len(b), hash_rule="content-sorted-rows-1")
    if isinstance(doc, dict):
        for k in ("ready", "truncated", "source", "bevdirect_version", "fetched_at", "bbox", "strict", "endpoint"):
            if k in doc:
                d[k] = doc[k]
        if rows_key and isinstance(doc.get(rows_key), list):
            d["rows"] = len(doc[rows_key])
        for k in ("parcels", "footprints", "landuse"):
            if rows_key is None and isinstance(doc.get(k), list):
                d[k] = len(doc[k])
    return d


def build_from_bevdirect(kg, epoch, paths, output, source=None, bbox=None, input_bbox=None):
    """Build one NEC1 file from bevdirect-serve viewport documents. Returns the summary dict
    (also what the CLI prints). Refuses documents with ready:false."""
    t0 = time.time()
    docs = [_load(x) for x in paths]
    prov = []
    for x, d in zip(paths, docs):
        if not d.get("ready", True):
            raise SystemExit(f"{x}: ready:false — refusing to build on a partial fetch")
        prov.append(file_prov(x, None, d))
    P, F, L = canon.from_bevdirect(docs)
    source = source or canon.bevdirect_source(docs)
    if not source.startswith("bevdirect@"):
        raise SystemExit("--source for bevdirect input must be bevdirect@<tag>")
    return _finish(kg, epoch, source, P, F, L, prov, output, bbox, input_bbox, t0)


def _finish(kg, epoch, source, P, F, L, prov, output, bbox, input_bbox, t0):
    res = build_cells(kg, P, F, L, coverage_bbox=bbox, input_bbox=input_bbox)
    raw = pack_records(res.cells, res.kcells)
    hdr = lu_header(kg, epoch, source, ALGO, H3_RES, K_RES, K, len(res.cells), len(res.kcells), res.stats,
                    input_bbox=res.input_bbox, coverage_bbox=bbox or res.input_bbox, input_pad_deg=INPUT_PAD_DEG,
                    input_rule=INPUT_RULE, inputs=prov)
    blob = pack_section(MAGIC_LU, hdr, raw)
    tmp = output + ".tmp"
    with open(tmp, "wb") as f:
        f.write(blob)
    os.replace(tmp, output)
    sec = unpack_sections(blob)[0]
    return dict(kg=res.kg, algo=ALGO, source=source, epoch=epoch, input_bbox=list(res.input_bbox or ()),
                cells=len(res.cells), kcells=len(res.kcells), raw_bytes=len(raw),
                file_bytes=len(blob), digest=sec.header["digest"], seconds=round(time.time() - t0, 1),
                stats=res.stats)


def cmd_build(a):
    bbox = tuple(map(float, a.bbox.split(","))) if a.bbox else None
    ibox = tuple(map(float, a.input_bbox.split(","))) if a.input_bbox else None
    if a.bevdirect:
        out = build_from_bevdirect(a.kg, a.epoch, a.bevdirect, a.output, a.source, bbox, ibox)
    else:
        if not a.source:
            raise SystemExit("--source required for row-export input")
        t0 = time.time()
        docs, prov = {}, []
        for key, path in (("parcels", a.parcels), ("footprints", a.footprints), ("landuse", a.landuse)):
            if not path:
                raise SystemExit("--parcels, --footprints and --landuse are all required (or use --bevdirect)")
            docs[key] = _load(path)
            if not docs[key].get("ready", True) or docs[key].get("truncated"):
                raise SystemExit(f"{path}: ready:false or truncated — refusing to build on a partial input")
            prov.append(file_prov(path, key, docs[key]))
        P = canon.parcels_from_index(docs["parcels"]["parcels"])
        F = canon.footprints_from_index(docs["footprints"]["footprints"])
        L = canon.landuse_from_index(docs["landuse"]["landuse"])
        out = _finish(a.kg, a.epoch, a.source, P, F, L, prov, a.output, bbox, ibox, t0)
    print(json.dumps(out), file=sys.stderr)


def cmd_dump(a):
    with open(a.file, "rb") as f:
        secs = unpack_sections(f.read())
    out = []
    for s in secs:
        d = dict(header=s.header)
        if s.magic == MAGIC_LU:
            cells, kcells = s.records()
            d["cells"], d["kcells"] = cells, kcells
        else:
            d["raw_bytes"] = len(s.raw)
        out.append(d)
    json.dump(out if len(out) > 1 else out[0], sys.stdout, separators=(",", ":"))


def report_of(path, observer, bbox=None):
    """Epoch report (digests only) for the NEC1 section of a file."""
    with open(path, "rb") as f:
        secs = [s for s in unpack_sections(f.read()) if s.magic == MAGIC_LU]
    if not secs:
        raise SystemExit(f"{path}: no NEC1 section")
    return epoch_report(secs[0], observer, bbox)


def cmd_report(a):
    bbox = tuple(map(float, a.bbox.split(","))) if a.bbox else None
    json.dump(report_of(a.file, a.observer, bbox), sys.stdout, separators=(",", ":"))


def cmd_compare(a):
    def lu_of(path):
        with open(path, "rb") as f:
            for s in unpack_sections(f.read()):
                if s.magic == MAGIC_LU:
                    return s
        raise SystemExit(f"{path}: no NEC1 section")
    A, B = lu_of(a.a), lu_of(a.b)
    ca, _ = A.records()
    cb, _ = B.records()
    same = exact = 0
    diffs = []
    for c in sorted(set(ca) | set(cb)):
        x, y = ca.get(c), cb.get(c)
        if x is None or y is None:
            diffs.append((c, "missing"))
            continue
        if x == y:
            exact += 1
            same += 1
            continue
        worst = max(abs(p - q) for p, q in zip(x["lu"], y["lu"]))
        if worst <= a.tol and abs(x["bld_cover"] - y["bld_cover"]) <= a.tol:
            same += 1
        else:
            diffs.append((c, worst))
    print(json.dumps(dict(digest_a=A.header["digest"], digest_b=B.header["digest"], identical_digest=A.header["digest"] == B.header["digest"],
                          cells_a=len(ca), cells_b=len(cb), exact=exact, within_tol=same, differing=len(diffs),
                          sample=diffs[:20])))


def main(argv=None):
    ap = argparse.ArgumentParser(prog="ne_cells", description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sp = ap.add_subparsers(dest="cmd", required=True)
    b = sp.add_parser("build")
    b.add_argument("--kg", required=True)
    b.add_argument("--epoch", required=True, help="ISO month, e.g. 2026-10")
    b.add_argument("--source", help="bevdirect@<tag> (default for bevdirect input: from the documents)")
    b.add_argument("--parcels"); b.add_argument("--footprints"); b.add_argument("--landuse")
    b.add_argument("--bevdirect", action="append", help="bevdirect-serve /viewport document (repeatable)")
    b.add_argument("--bbox", help="coverage bbox for K cells (default = input bbox)")
    b.add_argument("--input-bbox", help="input domain W,S,E,N (default: bbox(own parcels)+0.004°, 4 dp outward; header input_bbox)")
    b.add_argument("-o", "--output", required=True)
    b.set_defaults(fn=cmd_build)
    d = sp.add_parser("dump"); d.add_argument("file"); d.set_defaults(fn=cmd_dump)
    r = sp.add_parser("report"); r.add_argument("file"); r.add_argument("--observer", required=True)
    r.add_argument("--bbox"); r.set_defaults(fn=cmd_report)
    c = sp.add_parser("compare"); c.add_argument("a"); c.add_argument("b"); c.add_argument("--tol", type=int, default=8)
    c.set_defaults(fn=cmd_compare)
    a = ap.parse_args(argv)
    a.fn(a)


if __name__ == "__main__":
    main()
