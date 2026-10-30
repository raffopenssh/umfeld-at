#!/usr/bin/env bash
# install.sh — build umfeld-at and install it as a systemd service.
#
#   git clone https://github.com/raffopenssh/umfeld-at.git && cd umfeld-at
#   ./scripts/install.sh
#
# 1. system dependencies (Go ≥ 1.22, gcc, sqlite3, zstd)
# 2. CGO_ENABLED=1 go build -tags fts5 -o umfeld-at .
# 3. /etc/umfeld-at.env (0600) from the template below if it does not exist
# 4. systemd unit umfeld-at.service (paths and user of this checkout)
#
# Data: the service starts with an empty NE store (data/ne_cells/) and fills
# it from peer reports; the OSM / toponym / address databases are built with
# the -osm-import, -toponyms-import and -addresses-import flags (README).

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"
RUN_USER="${RUN_USER:-$(id -un)}"
RUN_GROUP="${RUN_GROUP:-$(id -gn)}"
ENV_FILE=/etc/umfeld-at.env

echo "[1/4] dependencies"
command -v go >/dev/null || { echo "Go ≥ 1.22 is required (https://go.dev/dl)"; exit 1; }
need=()
command -v gcc >/dev/null || need+=(build-essential)
command -v sqlite3 >/dev/null || need+=(sqlite3 libsqlite3-dev)
command -v zstd >/dev/null || need+=(zstd)
if [ ${#need[@]} -gt 0 ]; then sudo apt-get update -qq && sudo apt-get install -y -qq "${need[@]}"; fi
echo "  $(go version)"

echo "[2/4] build"
CGO_ENABLED=1 go build -tags fts5 -o umfeld-at .
mkdir -p data/ne_cells data/ne_fidx

echo "[3/4] environment file"
if [ ! -f "$ENV_FILE" ]; then
  sudo install -m 0600 -o root -g root /dev/stdin "$ENV_FILE" <<'TPL'
# umfeld-at — runtime configuration (peers.go, docs/api-tiers.md). Keep 0600.
UMFELD_PUBLIC_ONLY=1
# Public base URL of this instance (share links, attribution).
UMFELD_PUBLIC_URL=https://example.org
# Private tier: path prefix + tokens (≥ 16 chars, comma-separated) and their names.
UMFELD_PRIVATE_PREFIX=/k
UMFELD_TOKEN=
UMFELD_TOKEN_NAMES=
# Report-only peer tokens (NE reports; the token named NE_WRITER may upload the observed layer).
CONTRIBUTOR_TOKEN=
CONTRIBUTOR_TOKEN_NAMES=
NE_WRITER=
# Sibling services joined per H3 cell (leave empty to run without them).
UMFELD_PEER_OBS_NAME=
UMFELD_PEER_OBS_API=
UMFELD_PEER_OBS_WEB=
UMFELD_PEER_SUB_NAME=
UMFELD_PEER_SUB_API=
UMFELD_PEER_MAP_NAME=
TPL
  echo "  wrote template $ENV_FILE — edit it, then: sudo systemctl restart umfeld-at"
else
  echo "  $ENV_FILE exists, left untouched"
fi

echo "[4/4] systemd unit"
sed "s|^User=.*|User=$RUN_USER|;s|^Group=.*|Group=$RUN_GROUP|;s|^WorkingDirectory=.*|WorkingDirectory=$ROOT_DIR|;s|^ExecStart=.*|ExecStart=$ROOT_DIR/umfeld-at|;s|^ReadWritePaths=.*|ReadWritePaths=$ROOT_DIR/data|" \
  umfeld-at.service | sudo tee /etc/systemd/system/umfeld-at.service >/dev/null
sudo systemctl daemon-reload
sudo systemctl enable umfeld-at
sudo systemctl restart umfeld-at
echo "  started on port 8000 — journalctl -u umfeld-at -f"
echo "  docs: http://localhost:8000/api/v1/docs/llm.txt"
