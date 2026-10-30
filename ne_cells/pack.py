"""Binary container. A file is a sequence of SECTIONS; each section is self-delimiting:

    magic[4] | fmt_ver u8 | hdr_len u32 LE | header JSON (canonical, UTF-8) | body_len u32 LE | body

body = zlib-deflated record block. `digest` in the header = sha256(RAW record block)[:16] —
the digest is what peers compare, so it must not depend on the zlib build; the deflate stream may.

Section kinds (magic):
  NEC1  declared layer (algo ne-cells-*): cells_n LU records (res 12) then kcells_n K records (res 10)
  NEO1  observed layer posted by a peer (algo obs-cells-*): records defined by the peer; header must
        carry algo, epoch, source, kg, h3_res=12, cells_n, digest, groups (== GROUP_ORDER).

LU record (30 B, little-endian):
  Q cell_id | B kg_share | B land | B cov | B flags | 9s lu[GROUP_ORDER] | B bld_cover | H bld_n |
  B gk | B n_parc | H line_m_ha | B orient_coh (0xFF none) | B orient_axis/2 (0xFF none)
  (all shares 1/255 of CELL area; cov = Σlu/land capped 1; flags bit0 declared_unknown, bit1 normalised)
K record (29 B):
  Q cell_id | H n_parc | I parc_p10 | I parc_p50 | I parc_p90 | H ez_n | B parc_per_ez×10 (0xFF none) |
  H bld_n | H bld_p50 (0xFFFF none)
"""
import hashlib
import json
import struct
import zlib

from .ns_groups import GROUP_ORDER, NS_TABLE_VERSION

FMT_VER = 1
MAGIC_LU = b"NEC1"
MAGIC_OBS = b"NEO1"
LU_REC = struct.Struct("<QBBBB9sBHBBHBB")
K_REC = struct.Struct("<QHIIIHBHH")
assert LU_REC.size == 30 and K_REC.size == 29

ATTRIBUTION = "Datenquelle: BEV – Bundesamt für Eich- und Vermessungswesen, Kataster, CC BY 4.0, bearbeitet (Nutzungseinheit-Zellen, keine Objektgeometrie)"
NOTICE = ("Statistics per H3 cell derived from the Austrian cadastre (© BEV, CC BY 4.0). No parcel, footprint, "
          "GNR or EZ is contained or recoverable. Fields below k=5 are suppressed. Not an official cadastre product.")


def canonical_json(obj) -> bytes:
    return json.dumps(obj, sort_keys=True, separators=(",", ":"), ensure_ascii=True).encode("ascii")


def digest_of(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()[:16]


class Section:
    def __init__(self, magic: bytes, header: dict, raw: bytes):
        self.magic, self.header, self.raw = magic, header, raw

    def records(self):
        """NEC1 only: (cells{dict}, kcells{dict})."""
        if self.magic != MAGIC_LU:
            raise ValueError("not a declared-layer section")
        n, kn = self.header["cells_n"], self.header["kcells_n"]
        cells, kcells = {}, {}
        off = 0
        for _ in range(n):
            (cid, kgs, land, cov, flags, lu, bc, bn, gk, npc, line, coh, axis) = LU_REC.unpack_from(self.raw, off)
            off += LU_REC.size
            cells[h3hex(cid)] = dict(kg_share=kgs, land=land, cov=cov, flags=flags, lu=list(lu), bld_cover=bc,
                                     bld_n=bn, gk=gk, n_parc=npc, line_m_ha=line, orient_coh=coh, orient_axis=axis)
        for _ in range(kn):
            (cid, npc, p10, p50, p90, ezn, ppe, bn, bp50) = K_REC.unpack_from(self.raw, off)
            off += K_REC.size
            kcells[h3hex(cid)] = dict(n_parc=npc, parc_p10=p10, parc_p50=p50, parc_p90=p90, ez_n=ezn,
                                      parc_per_ez=ppe, bld_n=bn, bld_p50=bp50)
        if off != len(self.raw):
            raise ValueError("trailing bytes in section body")
        return cells, kcells


def h3hex(cid: int) -> str:
    return format(cid, "x")


def pack_records(cells: dict, kcells: dict) -> bytes:
    out = bytearray()
    for c in sorted(cells):
        r = cells[c]
        out += LU_REC.pack(int(c, 16), r["kg_share"], r["land"], r["cov"], r["flags"], bytes(r["lu"]),
                           r["bld_cover"], r["bld_n"], r["gk"], r["n_parc"], r["line_m_ha"],
                           r["orient_coh"], r["orient_axis"])
    for c in sorted(kcells):
        r = kcells[c]
        out += K_REC.pack(int(c, 16), r["n_parc"], r["parc_p10"], r["parc_p50"], r["parc_p90"], r["ez_n"],
                          r["parc_per_ez"], r["bld_n"], r["bld_p50"])
    return bytes(out)


def pack_section(magic: bytes, header: dict, raw: bytes, level: int = 9) -> bytes:
    hdr = dict(header)
    hdr["digest"] = digest_of(raw)
    hdr["fmt_ver"] = FMT_VER
    hj = canonical_json(hdr)
    body = zlib.compress(raw, level)
    return magic + bytes([FMT_VER]) + struct.pack("<I", len(hj)) + hj + struct.pack("<I", len(body)) + body


def lu_header(kg: str, epoch: str, source: str, algo: str, h3_res: int, k_res: int, k: int,
              cells_n: int, kcells_n: int, stats: dict | None = None, input_bbox=None, coverage_bbox=None,
              input_pad_deg=None, input_rule=None, inputs=None, coord_decimals=7) -> dict:
    """NEC1 header. `input_bbox`/`input_rule`/`input_pad_deg` pin the input domain (ne-cells-2);
    `inputs` is build provenance (files, sha256, endpoints, ready state) — NOT part of the digest,
    which covers the record block only."""
    h = dict(magic=MAGIC_LU.decode(), algo=algo, epoch=epoch, source=source, h3_res=h3_res, k_res=k_res, k=k,
             kg=str(kg).zfill(5), cells_n=cells_n, kcells_n=kcells_n, groups=list(GROUP_ORDER),
             ns_table=NS_TABLE_VERSION, attribution=ATTRIBUTION, notice=NOTICE,
             record_lu=LU_REC.format, record_k=K_REC.format, coord_decimals=coord_decimals)
    if input_bbox is not None:
        h["input_bbox"] = [float(v) for v in input_bbox]
    if coverage_bbox is not None:
        h["coverage_bbox"] = [float(v) for v in coverage_bbox]
    if input_pad_deg is not None:
        h["input_pad_deg"] = input_pad_deg
    if input_rule:
        h["input_rule"] = input_rule
    if inputs:
        h["inputs"] = inputs
    if stats:
        h["stats"] = {k_: int(v) for k_, v in sorted(stats.items())}
    return h


def unpack_sections(buf: bytes) -> list:
    out = []
    off = 0
    while off < len(buf):
        if len(buf) - off < 9:
            raise ValueError("truncated section header")
        magic = buf[off:off + 4]
        ver = buf[off + 4]
        if ver != FMT_VER:
            raise ValueError(f"unsupported fmt_ver {ver}")
        (hl,) = struct.unpack_from("<I", buf, off + 5)
        off += 9
        header = json.loads(buf[off:off + hl].decode("utf-8"))
        off += hl
        (bl,) = struct.unpack_from("<I", buf, off)
        off += 4
        raw = zlib.decompress(buf[off:off + bl])
        off += bl
        if header.get("digest") != digest_of(raw):
            raise ValueError("digest mismatch")
        out.append(Section(magic, header, raw))
    return out


# ── chunk digests: what a peer posts back as an epoch report ────────────────────────────
CHUNK_RES = 10


def h3_parent(cid: int, res: int) -> int:
    """Pure-int H3 parent (h3 v4 bit layout) — mirrored in Go (api_ne.go)."""
    cur = (cid >> 52) & 0xF
    if res >= cur:
        return cid
    out = (cid & ~(0xF << 52)) | (res << 52)
    for d in range(res + 1, 16):
        out |= 0x7 << ((15 - d) * 3)
    return out


def chunk_digests(raw: bytes, cells_n: int, bbox=None) -> dict:
    """{res-10 chunk id hex: sha256(its LU records in cell-id order)[:16]}. With `bbox` (W,S,E,N)
    only chunks whose res-10 hexagon lies FULLY inside it are kept — a chunk cut by the edge of the
    observer's complete extent has a truncated record set and must not be compared."""
    import collections
    groups = collections.defaultdict(bytearray)
    for i in range(cells_n):
        rec = raw[i * LU_REC.size:(i + 1) * LU_REC.size]
        groups[h3_parent(int.from_bytes(rec[:8], "little"), CHUNK_RES)] += rec
    if bbox is not None:
        import h3
        W, S, E, N = bbox
        def inside(cell):
            lats, lngs = zip(*h3.cell_to_boundary(cell))
            return min(lngs) >= W and max(lngs) <= E and min(lats) >= S and max(lats) <= N
        # H3 children are NOT contained in their parent: test every res-12 child of the chunk, not the
        # res-10 hexagon (a child poking out of the bbox is withheld by the build → chunk differs).
        groups = {p: b for p, b in groups.items() if all(inside(ch) for ch in h3.cell_to_children(h3hex(p), 12))}
    return {h3hex(p): digest_of(bytes(b)) for p, b in sorted(groups.items())}


def epoch_report(section: "Section", observer: str, bbox=None) -> dict:
    """Body for POST /contrib/api/v1/ne/{kg}/report — digests only, no cell content. `bbox` defaults to
    the section's coverage_bbox/input_bbox; chunks not fully inside it are dropped."""
    h = section.header
    if bbox is None:
        bbox = h.get("coverage_bbox") or h.get("input_bbox")
    chunks = chunk_digests(section.raw, h["cells_n"], bbox)
    return dict(kg=h["kg"], algo=h["algo"], epoch=h["epoch"], source=h["source"], observer=observer,
                h3_res=h["h3_res"], chunk_res=CHUNK_RES, cells_n=h["cells_n"], digest=h["digest"],
                input_bbox=h.get("input_bbox"), bbox=list(bbox) if bbox else None,
                chunks_total=len(chunk_digests(section.raw, h["cells_n"])), chunks=chunks)
