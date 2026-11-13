#!/usr/bin/env python3
"""glyph_trace.py — digest of the glyph's ?trace / ?run records (docs/ne-glyph.md §Field diagnostics).

  scripts/glyph_trace.sh                 fetch from the local service and digest (the token is never printed)
  scripts/glyph_trace.sh --hours 3 -v    last 3 hours, with every row of the dead sessions
  python3 scripts/glyph_trace.py f.json  digest a saved feedback list

One block per session (sid; records before sid existed are grouped by arrival), newest last:
verdict (DIED / HANG / ok), env, the state it died in, the last input rows. Then the runs
(?run=…: step log + verdict) and, across all dead sessions, what they have in common and
where they differ from the sessions that lived — the first thing to read.
"""
import sys, json, argparse, collections, datetime

ap = argparse.ArgumentParser()
ap.add_argument('file', nargs='?', default='-')
ap.add_argument('--hours', type=float, default=24)
ap.add_argument('-n', type=int, default=14, help='input/mark rows shown per dead session')
ap.add_argument('-v', action='store_true', help='all rows of dead sessions')
ap.add_argument('--ua', default='', help='only sessions whose user agent contains this')
a = ap.parse_args()
raw = sys.stdin.read() if a.file == '-' else open(a.file).read()
fb = json.loads(raw).get('feedback', [])
now = datetime.datetime.now(datetime.timezone.utc)
def ts(f): return datetime.datetime.strptime(f['created_at'][:19], '%Y-%m-%dT%H:%M:%S').replace(tzinfo=datetime.timezone.utc)
fb = [f for f in fb if isinstance(f.get('context'), dict) and (now - ts(f)).total_seconds() <= a.hours * 3600
      and a.ua.lower() in str(f['context'].get('ua', '')).lower() and not str(f.get('title', '')).startswith('deep')]
fb.sort(key=lambda f: (f['created_at'], f.get('id', 0)))

# sessions: by sid; legacy records (no sid) chain by ua — a 'prev' record closes the session before it
sess, order, legacy = {}, [], {}
for f in fb:
    c = f['context']; part = c.get('part'); sid = c.get('sid') or ''
    if not sid:
        ua = c.get('ua', '')
        if part == 'prev':
            sid = 'old-%d' % f.get('id', 0)
        else:
            if part in (1, 'hang') and ua not in legacy or part == 1:
                legacy[ua] = 'old-%d' % f.get('id', 0)
            sid = legacy.setdefault(ua, 'old-%d' % f.get('id', 0))
    s = sess.get(sid)
    if not s:
        s = sess[sid] = dict(sid=sid, recs=[], rows=[], died=False, hang=False, st=None, env=None, ua=c.get('ua', ''), t=f['created_at'], hash='', run=None, prev=None)
        order.append(sid)
    s['recs'].append(f); s['hash'] = f.get('description') or s['hash']
    for r in c.get('rows') or []:
        if r not in s['rows'][-200:]: s['rows'].append(r)
    if c.get('env'): s['env'] = c['env']
    if part == 'prev': s['died'] = True; s['prev'] = c; s['st'] = c.get('st') or s['st']
    elif part == 'hang': s['hang'] = True; s['st'] = c.get('st') or s['st']
    elif c.get('st') and not s['died']: s['st'] = c['st']
    if part == 'run': s['run'] = c.get('run')

def is_event(r):
    p = r.split(' ', 2)
    return len(p) > 1 and ' | ' not in r and p[1] not in ('longtask', 'stall')
def rownum(r):
    try: return float(r.split(' ', 1)[0])
    except ValueError: return 0.0
def short(v, n=70):
    v = str(v); return v if len(v) <= n else v[:n - 1] + '…'

print('%d records, %d sessions in the last %g h' % (len(fb), len(order), a.hours))
dead, lived = [], []
for sid in order:
    s = sess[sid]
    s['rows'].sort(key=rownum)
    verdict = 'DIED' if s['died'] else 'HANG' if s['hang'] else 'ok'
    (dead if s['died'] or s['hang'] else lived).append(s)
    e = s['env'] or {}
    print('\n== %s  %s  %s  %d rec  %s' % (s['t'][11:19], sid, verdict, len(s['recs']), short(s['hash'], 90)))
    print('   ua   ' + short(s['ua'], 110))
    if e: print('   env  ' + ' '.join('%s=%s' % (k, short(e[k], 40)) for k in ('lite', 'wv', 'nolive', 'kx', 'brands', 'gpu', 'dpr', 'inner', 'scr', 'mem', 'tp', 'ovf', 'q') if k in e and e[k] != ''))
    if s['prev']:
        p = s['prev']; print('   died alive_s=%s last_raf_s=%s reopened %ss later%s' % (p.get('alive_s'), p.get('last_raf_s'), p.get('died_s_ago'),
              '  (main thread ran on %.1f s after the last frame: compositor/GPU)' % (p.get('alive_s', 0) - p.get('last_raf_s', 0)) if p.get('alive_s', 0) - p.get('last_raf_s', 0) > 1.5 else ''))
    if s['st']: print('   %s ' % ('at death' if s['died'] or s['hang'] else 'state   ') + ' '.join('%s=%s' % (k, short(v, 40)) for k, v in s['st'].items() if v not in ('', None)))
    errs = [x for f in s['recs'] for x in (f['context'].get('err') or [])]
    if errs: print('   ERR  ' + short(json.dumps(errs[:3]), 300))
    if s['died'] or s['hang']:
        rows = s['rows'] if a.v else [r for r in s['rows'] if is_event(r)][-a.n:]
        for r in rows: print('     ' + short(r, 200))
    else:
        ev = [r for r in s['rows'] if is_event(r)]
        print('   %d rows, %d events; last: %s' % (len(s['rows']), len(ev), short(ev[-1], 120) if ev else '-'))

runs = [s for s in (sess[x] for x in order) if s['run']]
if runs:
    print('\n#### runs')
    byrid = collections.OrderedDict()
    for s in runs: byrid[s['run'].get('rid')] = s['run']
    for rid, r in byrid.items():
        print('run %s  phase=%s  %s' % (rid, r.get('ph'), r.get('verdict') or ('open: ' + str(r.get('cur')) + '/' + str(r.get('sub')))))
        for st in r.get('log') or []: print('   %-22s %-8s on=%s' % (st[0], st[2], st[1] or '-'))

if dead:
    print('\n#### %d dead session(s) — what they share (and how many of the %d that lived share it)' % (len(dead), len(lived)))
    keys = collections.OrderedDict()
    def facts(s):
        o = {}
        for k, v in (s['st'] or {}).items():
            if k in ('fs_age', 'in_age', 'z', 'left', 'map_downs'): continue
            o['st.' + k] = v
        st = s['st'] or {}
        if 'z' in st: o['st.z'] = 'street (≥ 300)' if st['z'] >= 300 else 'near (≥ 24)' if st['z'] >= 24 else 'far'
        if 'in_age' in st: o['death within 2 s of the last input'] = st['in_age'] >= 0 and st['in_age'] <= 2
        if 'fs_age' in st and st.get('fs'): o['first 10 s of full screen'] = st['fs_age'] <= 10
        for k in ('lite', 'wv', 'nolive', 'kx', 'brands', 'gpu', 'ovf'):
            if k in (s['env'] or {}): o['env.' + k] = s['env'][k]
        return o
    df, lf = [facts(s) for s in dead], [facts(s) for s in lived]
    for k in df[0]:
        vals = [d.get(k) for d in df]
        if all(v == vals[0] for v in vals):
            same = sum(1 for l in lf if l.get(k) == vals[0]); have = sum(1 for l in lf if k in l)
            print('   %-36s = %-28s lived with the same: %d/%d%s' % (k, short(vals[0], 28), same, have, '   ← only the dead' if have and not same else ''))
    if not any(s['st'] for s in dead): print('   (no state snapshots — sessions recorded before the recorder carried them; read the rows above)')
