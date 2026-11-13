#!/usr/bin/env bash
# deploy.sh — build, swap, restart, verify, roll back. docs/restart.md
#
# The new binary is built beside the running one and renamed into place, so a
# failed build never touches the service. The restart itself refuses no
# connection (socket unit) and `systemctl restart` only returns once the new
# process reported READY; if it does not, the previous binary is put back.
# Disk cost: one previous binary (umfeld-at.prev, ~25 MB).
set -euo pipefail
cd "$(dirname "$0")/.."
UNIT=umfeld-at
PORT="${PORT:-8000}"

echo "[1/3] build"
CGO_ENABLED=1 go build -tags fts5 -o umfeld-at.new .

echo "[2/3] swap + restart"
[ -f umfeld-at ] && cp -p umfeld-at umfeld-at.prev
mv -f umfeld-at.new umfeld-at

check() { curl -fsS -o /dev/null --max-time 20 "http://localhost:$PORT/api/v1/license"; }
t0=$(date +%s.%N)
if sudo systemctl restart "$UNIT" && check; then
  printf '[3/3] ok — restart took %.1f s\n' "$(echo "$(date +%s.%N) - $t0" | bc)"
  journalctl -u "$UNIT" --no-pager -n 400 --since "@${t0%.*}" | grep -E 'startup:|ready in|drained' | sed 's/^.*\]: /  /' || true
  exit 0
fi

echo "[3/3] FAILED — rolling back to umfeld-at.prev" >&2
journalctl -u "$UNIT" --no-pager -n 30 >&2 || true
if [ -f umfeld-at.prev ]; then
  mv -f umfeld-at umfeld-at.failed
  cp -p umfeld-at.prev umfeld-at
  sudo systemctl restart "$UNIT" && check && echo "rolled back; the failed build is ./umfeld-at.failed" >&2
fi
exit 1
