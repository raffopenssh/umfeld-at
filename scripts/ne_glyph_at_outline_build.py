#!/usr/bin/env python3
"""Add the national outline ('at' collection, code AT) to data/ne_glyph_admin.json.gz.

Source: data/osm_austria_boundary.geojson.gz — OpenStreetMap relation 16239 (ODbL), which follows
the BEV KG polygons to within a few metres; the Statistik Austria Gemeinde outlines the
other collections are built from are generalised by up to ~200 m along the state border, which
left a hillshade band outside the cells (Frastanz/Liechtenstein). Used by the glyph's relief clip
(relClip) and selectable in the find box like a Land. Rewrites only the 'at' key; the 3-min
ne_glyph_admin_build.py run is not needed. Needs shapely."""
import json, gzip, os
from shapely.geometry import shape, Polygon
f = json.load(gzip.open('data/osm_austria_boundary.geojson.gz'))
g = shape(f['geometry']).buffer(0).simplify(0.00005, preserve_topology=True)
polys = [g] if g.geom_type == 'Polygon' else list(g.geoms)
rings = []
for p in polys:
    if p.area < 1e-6: continue
    for r in [p.exterior] + [i for i in p.interiors if Polygon(i).area >= 1e-6]:
        c = [[round(x, 5), round(y, 5)] for x, y in r.coords]
        if len(c) >= 4: rings.append(c)
b = g.bounds
a = json.load(gzip.open('data/ne_glyph_admin.json.gz'))
a['at'] = [{'c': 'AT', 'n': f['properties']['name'], 'bbox': [round(v, 5) for v in b], 'r': rings}]
s = json.dumps(a, separators=(',', ':'), ensure_ascii=False)
gzip.open('data/ne_glyph_admin.json.gz', 'wb', 9).write(s.encode())
print(len(rings), 'rings', sum(len(r) for r in rings), 'verts', os.path.getsize('data/ne_glyph_admin.json.gz') >> 10, 'KB gz')
