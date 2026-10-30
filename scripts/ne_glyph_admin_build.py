#!/usr/bin/env python3
"""Build data/ne_glyph_admin.json.gz — every outline collection the glyph find box can select
like a KG. Consumed by ne_glyph_admin.go (one entry per collection in neGlyphAdminKinds).

  bl / bz / gem  Gemeinde polygons (Statistik Austria, CC BY 4.0, data/gemeinden_v2.geojson.gz),
                 Bezirke (GKZ[0:3]) and Bundesländer (GKZ[0]) dissolved from them
  n2k            Natura 2000 sites (EEA, data/natura2000_at.geojson.gz), keyed by SITECODE

Adding a collection: load → one entry per unit {c, n, bbox, r, …extra} under a new key here,
one row in neGlyphAdminKinds (code pattern, kind label, notice), the client picks it up from
the /admin index (docs/glyph-find-contract.md §admin). Rings in lon/lat, simplified; runs ~3 min;
needs shapely."""
import json, gzip, collections, os
from shapely.geometry import shape, Polygon
from shapely.ops import unary_union
d = json.load(gzip.open('data/gemeinden_v2.geojson.gz'))
gem = {f['properties']['id']: (f['properties']['name'], shape(f['geometry']).buffer(0)) for f in d['features']}
def rings(g, tol, minA):
    # the source topology is not clean: close hairline slivers, then drop polygons/holes below minA (deg²)
    g = g.buffer(0.0002).buffer(-0.0002).simplify(tol, preserve_topology=True)
    polys = [g] if g.geom_type == 'Polygon' else list(g.geoms)
    out = []
    for p in polys:
        if p.area < minA: continue
        for r in [p.exterior] + [i for i in p.interiors if Polygon(i).area >= minA]:
            c = [[round(x, 5), round(y, 5)] for x, y in r.coords]
            if len(c) >= 4: out.append(c)
    return out
def bbox(g): b = g.bounds; return [round(v, 5) for v in b]
out = {'bl': [], 'bz': [], 'gem': [], 'n2k': []}
bz = collections.defaultdict(list); bl = collections.defaultdict(list)
for c, (n, g) in sorted(gem.items()):
    out['gem'].append({'c': c, 'n': n, 'bz': c[:3], 'bbox': bbox(g), 'r': rings(g, 0.0004, 2e-7)})
    bz[c[:3]].append(g); bl[c[0]].append(g)
for c, gs in sorted(bz.items()):
    u = unary_union(gs); out['bz'].append({'c': c, 'bl': c[0], 'bbox': bbox(u), 'r': rings(u, 0.0008, 1e-4)})
for c, gs in sorted(bl.items()):
    u = unary_union(gs); out['bl'].append({'c': c, 'bbox': bbox(u), 'r': rings(u, 0.0012, 5e-4)})
# Natura 2000: 353 sites, 1.2 M source vertices; site type A = birds (SPA), B = habitats (SAC), C = both
n2 = json.load(gzip.open('data/natura2000_at.geojson.gz'))
for f in sorted(n2['features'], key=lambda f: f['properties']['SITECODE']):
    pr = f['properties']; g = shape(f['geometry']).buffer(0)
    out['n2k'].append({'c': pr['SITECODE'], 'n': pr['SITENAME'], 't': pr.get('SITETYPE', ''), 'ha': round(pr.get('Area_ha') or 0),
                       'bbox': bbox(g), 'r': rings(g, 0.0004, 2e-7)})
s = json.dumps(out, separators=(',', ':'), ensure_ascii=False)
gzip.open('data/ne_glyph_admin.json.gz', 'wb', 9).write(s.encode())
print(len(s) >> 10, 'KB raw', os.path.getsize('data/ne_glyph_admin.json.gz') >> 10, 'KB gz',
      sum(len(r) for x in out['gem'] for r in x['r']), 'gem verts',
      sum(len(x['r']) for x in out['bz']), 'bz rings', sum(len(x['r']) for x in out['bl']), 'bl rings',
      sum(len(r) for x in out['n2k'] for r in x['r']), 'n2k verts')
