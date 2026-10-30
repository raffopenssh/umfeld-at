"""NE change — area+pattern digests (docs/ne-change.md §3). Lives OUTSIDE the frozen files
(algo/canon/ns_groups/pack stay byte-identical; ALGO stays ne-cells-2).

The change series is keyed by `digest_ap`: sha256[:16] of a chunk's LU records with the two
register bytes zeroed (`gk` at offset 24, `n_parc` at offset 25 — see pack.LU_REC) and K
records dropped. A chunk whose only difference is in register bytes keeps its digest_ap, so a
register-only event (split / merge / status) never produces a step-2 upload nor a delta row.

    strip_register(raw, cells_n) -> bytes        stripped LU records (cells_n × 30 B), K records gone
    chunk_digests_ap(raw, cells_n, bbox) -> {chunk_hex: digest_ap}
    chunk_rows(raw, cells_n, chunks) -> {chunk_hex: stripped rows}   what POST …/chunks sends
    epoch_report_ap(section, observer, bbox) -> report dict + "chunks_ap"
"""
from .pack import LU_REC, chunk_digests, epoch_report, h3_parent, h3hex, CHUNK_RES

REGISTER_OFFSETS = (24, 25)   # gk, n_parc — the only register-derived bytes of the LU record
AP_VERSION = "ap-1"           # bump if the stripped layout ever changes (it must not under ne-cells-2)


def strip_register(raw: bytes, cells_n: int) -> bytes:
    """LU records only, register bytes zeroed. Row order and every other byte unchanged."""
    rb = LU_REC.size
    out = bytearray(raw[:cells_n * rb])
    for i in range(cells_n):
        for o in REGISTER_OFFSETS:
            out[i * rb + o] = 0
    return bytes(out)


def chunk_digests_ap(raw: bytes, cells_n: int, bbox=None) -> dict:
    """{res-10 chunk hex: digest_ap}; same chunk set and bbox rule as pack.chunk_digests."""
    return chunk_digests(strip_register(raw, cells_n), cells_n, bbox)


def chunk_rows(raw: bytes, cells_n: int, chunks) -> dict:
    """Stripped LU rows per requested chunk (cell-id order as in the section)."""
    want = {int(c, 16) for c in chunks}
    rb = LU_REC.size
    s = strip_register(raw, cells_n)
    out = {}
    for i in range(cells_n):
        rec = s[i * rb:(i + 1) * rb]
        p = h3_parent(int.from_bytes(rec[:8], "little"), CHUNK_RES)
        if p in want:
            out.setdefault(h3hex(p), bytearray()).extend(rec)
    return {c: bytes(b) for c, b in out.items()}


def epoch_report_ap(section, observer: str, bbox=None) -> dict:
    """pack.epoch_report + `chunks_ap` for the same chunk set. Wrapper — pack.py is frozen."""
    rep = epoch_report(section, observer, bbox)
    h = section.header
    if bbox is None:
        bbox = h.get("coverage_bbox") or h.get("input_bbox")
    ap = chunk_digests_ap(section.raw, h["cells_n"], bbox)
    rep["chunks_ap"] = {c: ap[c] for c in rep["chunks"] if c in ap}
    rep["ap_version"] = AP_VERSION
    return rep


def pack_chunk_rows(rows: dict) -> bytes:
    """Body of POST …/chunks: NECH magic | u16 n | per chunk: Q chunk_id | u32 len | rows."""
    import struct
    out = bytearray(b"NECH") + struct.pack("<H", len(rows))
    for c in sorted(rows):
        b = rows[c]
        out += struct.pack("<QI", int(c, 16), len(b)) + b
    return bytes(out)
