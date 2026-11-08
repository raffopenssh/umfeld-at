#!/bin/sh
# Keep the Go build cache small. This repo is one big main package (~135 files),
# so EVERY edit recompiles it: ~17 MB (build) + ~21 MB (test binary) of new
# cache entries per edit, and `go` itself only evicts entries unused for 5 days,
# once a day. Dozens of agent edit cycles/day => multi-GB cache.
# The cache is content-addressed; deleting entries is always safe (they are
# rebuilt on demand). We drop anything not used in the last few hours, which
# keeps the hot entries (stdlib, the ~1 min cgo sqlite3 build, deps) and sheds
# the stale per-edit objects.
# Installed by scripts/install.sh as a systemd timer (weekly).
set -e
CACHE="${GOCACHE:-$(go env GOCACHE 2>/dev/null || echo "$HOME/.cache/go-build")}"
MAX_AGE_MIN="${GO_CACHE_MAX_AGE_MIN:-360}"
[ -d "$CACHE" ] || exit 0
before=$(du -sm "$CACHE" | cut -f1)
find "$CACHE" -type f ! -name README ! -name trim.txt -mmin +"$MAX_AGE_MIN" -delete
find "$CACHE" -mindepth 1 -type d -empty -delete
after=$(du -sm "$CACHE" | cut -f1)
echo "go-cache-trim: ${before}M -> ${after}M (entries unused > ${MAX_AGE_MIN} min removed)"
