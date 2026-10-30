"""ne_cells — Nutzungseinheit cells: declared land use + fabric statistics per H3 cell,
computed deterministically from the objects of a bevdirect-serve ``/viewport`` document
(or any export with the same row schema).

Pinned: shapely==2.1.2 (bundled GEOS), h3==4.5.0. Same package + same canonical input
⇒ bit-identical records and digest. This is the reference implementation of algo
``ne-cells-2``; it is never ported — peers run this package.
"""
from .algo import ALGO, H3_RES, K_RES, K, build_cells, BuildResult
from .pack import pack_section, unpack_sections, Section, FMT_VER, MAGIC_LU, MAGIC_OBS
from .ns_groups import GROUP_ORDER, NS_TABLE_VERSION, group_of

__version__ = "2.1.0"
__all__ = ["ALGO", "H3_RES", "K_RES", "K", "build_cells", "BuildResult", "pack_section",
           "unpack_sections", "Section", "FMT_VER", "MAGIC_LU", "MAGIC_OBS", "GROUP_ORDER",
           "NS_TABLE_VERSION", "group_of"]
