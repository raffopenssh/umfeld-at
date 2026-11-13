#!/bin/sh
# glyph_trace.sh — fetch the glyph's ?trace / ?run records from the local service and digest them
# (scripts/glyph_trace.py; docs/ne-glyph.md §Field diagnostics). The token is read from the env file
# inside a root shell and never printed. Arguments go to the digest (--hours N, -v, --ua X, -n N).
# RAW=/path keeps the fetched JSON.
ENVF=${UMFELD_ENV:-/etc/umfeld-at.env}
OUT=${RAW:-$(mktemp)}
sudo sh -c ". $ENVF; T=\${UMFELD_TOKEN%%,*}; curl -s -H \"Authorization: Bearer \$T\" \"localhost:\${PORT:-8000}\${UMFELD_PRIVATE_PREFIX:-/k}/api/v1/feedback?agent=glyph-trace&limit=1000\"" > "$OUT" || exit 1
python3 "$(dirname "$0")/glyph_trace.py" "$OUT" "$@"
[ -n "$RAW" ] || rm -f "$OUT"
