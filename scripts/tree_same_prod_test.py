#!/usr/bin/env python3
"""Production round trip of the same-tree / merged-top / note contract (observed-layer peer,
llm.txt #same-tree, #tree-note) through OUR relay, with throw-away reporters.

  scripts/tree_same_prod_test.py [--relay http://localhost:8000] [--peer <peer API base>] [-v]

Flags an extra top, reads it back in `merged`, objects as a second reporter (tie = tree is back),
takes everything back, then writes and removes a note. Leaves nothing standing at the peer
(the versions stay in the tree's history, hidden). Exit 0 = every step answered as the contract says.
"""
import json, math, os, sys, time, urllib.request, urllib.error, uuid

args = sys.argv[1:]
def opt(name, default):
    return args[args.index(name) + 1] if name in args else default
RELAY = opt('--relay', 'http://localhost:8000').rstrip('/')
PEER = opt('--peer', os.environ.get('UMFELD_PEER_OBS_API', '')).rstrip('/')
VERBOSE = '-v' in args
UA = {'User-Agent': 'umfeld-at/probe (+%s)' % os.environ.get('UMFELD_PUBLIC_URL', RELAY)}
if not PEER:
    sys.exit('--peer <observed-layer peer base URL> (or UMFELD_PEER_OBS_API)')
A, B = 'probe-' + uuid.uuid4().hex[:12], 'probe-' + uuid.uuid4().hex[:12]
BBOX = opt('--bbox', '16.1500,48.2500,16.1750,48.2750')
TILE = PEER + '/api/v1/trees/bbox?bbox=' + BBOX + '&min_height=3&format=columns&fields=lon,lat,h_m,crown_m2,x,y'
fails = []

def post(body, who):
    body = dict(body, reporter=who)
    time.sleep(1.1)  # the peer's pace
    req = urllib.request.Request(RELAY + '/api/v1/trees/report', json.dumps(body).encode(),
                                 {**UA, 'Content-Type': 'application/json'})
    try:
        r = urllib.request.urlopen(req); s = r.status; j = json.load(r)
    except urllib.error.HTTPError as e:
        s = e.code; j = json.loads(e.read() or b'{}')
    if VERBOSE:
        print('   POST', json.dumps(body)[:200], '→', s, json.dumps(j)[:400])
    return s, j

etag = [None]
def tile():
    h = dict(UA, **{'Cache-Control': 'no-cache'})
    if etag[0]:
        h['If-None-Match'] = etag[0]
    try:
        r = urllib.request.urlopen(urllib.request.Request(TILE, headers=h))
    except urllib.error.HTTPError as e:
        if e.code == 304:
            return 304, None
        raise
    etag[0] = r.headers.get('ETag'); return 200, json.load(r)

def check(name, ok, got=''):
    print(('ok   ' if ok else 'FAIL ') + name + ('' if ok else '  ← ' + str(got)[:300]))
    if not ok:
        fails.append(name)

def merged_at(j, x, y):
    m = j.get('merged') or {}
    for k in range(len(m.get('x') or [])):
        if m['x'][k] == x and m['y'][k] == y:
            return {f: m[f][k] for f in m}
    return None

s, j0 = tile(); c = j0['columns']; n = len(c['lon']); cl = math.cos(math.radians(c['lat'][0])) * 111320
pair = None
for i in range(n):
    if c['h_m'][i] < 24: continue
    for k in range(max(0, i - 60), min(n, i + 60)):
        d = math.hypot((c['lon'][i] - c['lon'][k]) * cl, (c['lat'][i] - c['lat'][k]) * 111320)
        if 2 < d < 6 and 15 < c['h_m'][k] < c['h_m'][i]:
            pair = (i, k); break
    if pair: break
if not pair:
    sys.exit('no pair of touching tops in ' + BBOX)
T, X = pair
g = lambda f, i: c[f][i]
sub = {'lon': g('lon', X), 'lat': g('lat', X)}; of = {'lon': g('lon', T), 'lat': g('lat', T)}
print('tree %.6f,%.6f %.1f m · extra top %.6f,%.6f %.1f m · reporters %s / %s' % (of['lon'], of['lat'], g('h_m', T), sub['lon'], sub['lat'], g('h_m', X), A, B))
flag = dict(sub, layer='trees', claim='spurious', observed={'h_m': g('h_m', X)}, expected={'absent': True, 'hint': 'crown', 'of': of})
back = dict(sub, layer='trees', claim='spurious', retract=True)
obj = dict(sub, layer='trees', claim='looks_right')

try:
    s, j = post(dict(flag, expected={'absent': True, 'hint': 'crown', 'of': sub}), A)
    check('of = subject → 400 bad_of', s == 400 and j.get('reason') == 'bad_of', (s, j))
    s, j = post(dict(flag, lon=sub['lon'] + 0.0004, lat=sub['lat'] + 0.00037, expected={'absent': True, 'hint': 'crown', 'of': of}), A)
    check('subject off every apex → 404 no_apex (or 400 bad_of: too far)', (s == 404 and j.get('reason') == 'no_apex') or (s == 400 and j.get('reason') == 'bad_of'), (s, j))
    s, j = post(dict(of, layer='trees', claim='spurious', observed={'h_m': g('h_m', T)}, expected={'absent': True, 'hint': 'crown', 'of': sub}), B)
    check('taller top flagged → 201 + warnings[]', s == 201 and j.get('warnings'), (s, j))
    s, j = post(dict(of, layer='trees', claim='spurious', retract=True), B)
    check('… taken back', s in (200, 201), (s, j))

    s, j = post(flag, A); cr = j.get('crown') or {}
    check('A: same tree → 201 crown{of{x,y},dist_m}', s == 201 and (cr.get('of') or {}).get('x') == g('x', T) and cr['of'].get('y') == g('y', T) and cr.get('dist_m') is not None, (s, j))
    s, j = post(flag, A)
    check('A: again → folded', s in (200, 201) and j.get('folded'), (s, j))
    s, j1 = tile(); m = merged_at(j1 or {}, g('x', X), g('y', X)) if j1 else None
    check('tile (no-cache) → 200 with the top in merged, of_i = its tree', s == 200 and m and m['of_x'] == g('x', T) and m['of_y'] == g('y', T) and m['n'] == 1 and m['against'] == 0
          and m['of_i'] is not None and j1['columns']['x'][m['of_i']] == g('x', T), (s, m))
    if j1 and m:
        check('count − 1, crown_m2 of the tree + the top\'s', j1['count'] == j0['count'] - 1 and abs(j1['columns']['crown_m2'][m['of_i']] - g('crown_m2', T) - m['crown_m2']) < 0.15,
              (j1['count'], j0['count'], j1['columns']['crown_m2'][m['of_i']], g('crown_m2', T), m['crown_m2']))
    s, j = tile(); check('tile again → 304', s == 304, s)

    s, j = post(back, B); check('B: retract → 403 not_yours', s == 403 and j.get('reason') == 'not_yours', (s, j))
    s, j = post(obj, B); check('B: "its own tree" → 201 crown{against}', s == 201 and (j.get('crown') or {}).get('against') is True, (s, j))
    s, j2 = tile(); check('tie → the tree is back (not in merged, count as before)', s == 200 and not merged_at(j2, g('x', X), g('y', X)) and j2['count'] == j0['count'], (s, j2 and j2.get('merged')))
    s, j = post(dict(obj, retract=True), B); check('B: objection taken back', s in (200, 201), (s, j))
    s, j3 = tile(); m = merged_at(j3 or {}, g('x', X), g('y', X)) if j3 else None
    check('merged again', s == 200 and m and m['against'] == 0, (s, m))

    lim = json.load(urllib.request.urlopen(urllib.request.Request(PEER + '/api/v1/trees/taxa/dict', headers=UA)))['limits']['note_chars']
    check('limits.note_chars advertised', lim >= 280, lim)
    s, j = post(dict(of, layer='trees', claim='tree', note='n' * (lim + 1)), A)
    check('note over the limit → refused by the relay (400), nothing stored', s == 400, (s, j))
    long = 'probe line one\nline two ' + 'n' * (lim - 24)
    s, j = post(dict(of, layer='trees', claim='tree', note=long), A); t = j.get('tree') or {}
    check('note of exactly the limit, with a line break → 201 op note, kept whole', s == 201 and t.get('op') == 'note' and t.get('note') == long, (s, str(j)[:300]))
    s, j = post(dict(of, layer='trees', claim='tree', note='probe note'), A); t = j.get('tree') or {}
    check('note replaced → 201 op note', s == 201 and t.get('note') == 'probe note', (s, j))
    s, j = post(dict(of, layer='trees', claim='tree', note=None), B); check('B: unnote → 403 not_yours', s == 403, (s, j))
finally:
    s, j = post(dict(of, layer='trees', claim='tree', note=None), A); check('A: unnote', s in (200, 201) or s == 403, (s, j))
    s, j = post(dict(of, layer='trees', claim='tree', retract=True), A); check('A: tree report withdrawn', s in (200, 201, 403, 404), (s, j))
    post(dict(obj, retract=True), B)
    s, j = post(back, A); check('A: same tree taken back', s in (200, 201), (s, j))
    s, j4 = tile(); check('tile as at the start (no merged top, same count)', s == 200 and not merged_at(j4, g('x', X), g('y', X)) and j4['count'] == j0['count'], (s, j4 and j4.get('merged')))
print('FAILED: ' + '; '.join(fails) if fails else 'all steps as the contract says')
sys.exit(1 if fails else 0)
