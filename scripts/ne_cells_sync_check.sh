#!/usr/bin/env bash
# The public python package (https://github.com/raffopenssh/vtcseamless-py) ships ne_cells; its
# frozen files MUST be byte-identical to ours (the digest contract depends on it). Compares
# against a local clone (default ~/vtcseamless-py) or, with REMOTE=1, against GitHub main.
#   scripts/ne_cells_sync_check.sh            # local clone
#   REMOTE=1 scripts/ne_cells_sync_check.sh   # raw.githubusercontent.com
set -euo pipefail
HERE=$(cd "$(dirname "$0")/.." && pwd)
FILES="algo.py canon.py ns_groups.py pack.py __init__.py"
rc=0
for f in $FILES; do
  if [ "${REMOTE:-0}" = 1 ]; then
    theirs=$(mktemp); curl -fsSL "https://raw.githubusercontent.com/raffopenssh/vtcseamless-py/main/ne_cells/$f" -o "$theirs"
  else
    theirs="${CLONE:-$HOME/vtcseamless-py}/ne_cells/$f"
  fi
  if cmp -s "$HERE/ne_cells/$f" "$theirs"; then echo "ok    ne_cells/$f"; else echo "DIFF  ne_cells/$f"; rc=1; fi
done
# __main__.py differs by design: ours keeps the operator-only `fetch` subcommand.
# change.py is NOT frozen but mirrored by copy (docs/ne-change.md) — warn, do not fail.
if [ "${REMOTE:-0}" != 1 ] && ! cmp -s "$HERE/ne_cells/change.py" "${CLONE:-$HOME/vtcseamless-py}/ne_cells/change.py"; then echo "WARN  ne_cells/change.py differs (mirror by copy)"; fi
exit $rc
