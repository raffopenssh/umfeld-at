"""ne_cells.change — digest_ap contract (docs/ne-change.md §3, §5). Run: python3 -m pytest tests_py"""
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))
from ne_cells.pack import LU_REC, chunk_digests, unpack_sections, Section, MAGIC_LU
from ne_cells.change import strip_register, chunk_digests_ap, chunk_rows, epoch_report_ap, pack_chunk_rows
from test_ne_cells import _blob


def test_strip_zeroes_only_register_bytes_and_drops_k():
    r, raw, blob = _blob(None)
    n = len(r.cells)
    s = strip_register(raw, n)
    assert len(s) == n * LU_REC.size
    for i in range(n):
        a, b = raw[i * 30:(i + 1) * 30], s[i * 30:(i + 1) * 30]
        assert b[24] == 0 and b[25] == 0
        assert a[:24] == b[:24] and a[26:] == b[26:]
    assert any(raw[i * 30 + 25] for i in range(n)), "fixture must have register bytes set"


def test_change_register_only_does_not_move_digest_ap():
    r, raw, blob = _blob(None)
    n = len(r.cells)
    d0, ap0 = chunk_digests(raw, n), chunk_digests_ap(raw, n)
    assert set(d0) == set(ap0) and all(d0[c] != ap0[c] for c in d0)  # distinct products
    m = bytearray(raw)
    m[24] ^= 0x5A          # gk of the first cell
    m[30 + 25] ^= 0x01     # n_parc of the second cell
    d1, ap1 = chunk_digests(bytes(m), n), chunk_digests_ap(bytes(m), n)
    assert d1 != d0 and ap1 == ap0
    m[12] ^= 0x01          # lu[0] (structure share) of the first cell → digest_ap moves
    assert chunk_digests_ap(bytes(m), n) != ap0


def test_epoch_report_ap_is_a_superset_and_rows_verify():
    r, raw, blob = _blob(None)
    sec = unpack_sections(blob)[0]
    rep = epoch_report_ap(sec, "t")
    assert set(rep["chunks_ap"]) == set(rep["chunks"]) and rep["ap_version"] == "ap-1"
    rows = chunk_rows(sec.raw, sec.header["cells_n"], list(rep["chunks"])[:2])
    from ne_cells.pack import digest_of
    for c, b in rows.items():
        assert digest_of(b) == rep["chunks_ap"][c] and len(b) % 30 == 0
    body = pack_chunk_rows(rows)
    assert body[:4] == b"NECH" and len(body) == 6 + sum(12 + len(b) for b in rows.values())
