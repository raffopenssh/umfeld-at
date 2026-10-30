#!/usr/bin/env python3
"""Fill interior holes in data/ne_glyph_austria_r7.json.

The frame was built as "H3 res-7 cells whose centre lies in a GADM level-3
polygon"; slivers between municipality polygons left cells with all six
neighbours in the frame but the cell itself missing — white, untappable
hexagons inside Austria (2026-10-29). Idempotent; prints what it added.
The bounding box (→ projection, share links) is untouched: holes are interior.
"""
import json, sys
import h3

p = 'data/ne_glyph_austria_r7.json'
d = json.load(open(p))
fr = set(d['cells'])
added = set()
while True:
    new = set()
    for c in fr:
        for n in h3.grid_disk(c, 1):
            if n not in fr and n not in new and all(m in fr for m in h3.grid_ring(n, 1)):
                new.add(n)
    if not new:
        break
    fr |= new
    added |= new
d['cells'] = sorted(fr)
if added:
    d['source'] = d['source'].split(';')[0] + '; interior holes (all six neighbours inside) filled by scripts/ne_glyph_frame_fill_holes.py'
    json.dump(d, open(p, 'w'), separators=(',', ':'))
print(f'{len(added)} added, {len(fr)} cells', file=sys.stderr)
for h in sorted(added):
    print(h, h3.cell_to_latlng(h))
