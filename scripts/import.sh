#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright Contributors to the project. See NOTICE.

# Restores an archive made by export.sh on a NEW host and starts the server.
#
#   git clone https://github.com/chargetronic/imatra.iso15118_server.git && cd imatra.iso15118_server
#   git checkout <commit from the archive>          # optional, to match exactly
#   scripts/import.sh /path/deployment-export-*.tar [--behind-proxy]
#
# --behind-proxy: the host has a reverse proxy of its own that does TLS; the
#               gateway serves plain HTTP on 127.0.0.1:PUBLIC_PORT (compose.behind-proxy.yml).
#               Default: the gateway serves HTTPS on 80/443 itself.
#
# Refuses to overwrite existing volumes or .env: import goes onto a clean host.
set -euo pipefail
cd "$(dirname "$0")/.."

ARCHIVE=${1:?usage: scripts/import.sh <archive.tar> [--behind-proxy]}
MODE=${2:-}
PROJECT=${COMPOSE_PROJECT_NAME:-imatra}   # compose project (docker-compose.yml "name"): volumes <project>_pki, ...
COMPOSE=(docker compose -f docker-compose.yml)
[[ "$MODE" == "--behind-proxy" ]] && COMPOSE+=(-f compose.behind-proxy.yml)

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
tar xf "$ARCHIVE" -C "$WORK"
[[ -f "$WORK/db.dump" && -f "$WORK/pki.tgz" && -f "$WORK/env" ]] \
  || { echo "archive incomplete (needs db.dump, pki.tgz, env)" >&2; exit 1; }

echo "archive made from commit $(cat "$WORK/commit"); this checkout is $(git rev-parse HEAD 2>/dev/null || echo unknown)"

if [[ -e .env ]]; then echo ".env already exists — refusing to overwrite" >&2; exit 1; fi
for vol in pki mqtt-data caddy-data pg-data; do
  if docker volume inspect "${PROJECT}_${vol}" >/dev/null 2>&1; then
    echo "volume ${PROJECT}_${vol} already exists — this is not a clean host" >&2; exit 1
  fi
done

install -m 600 "$WORK/env" .env
set -a; . ./.env; set +a

for vol in pki mqtt-data caddy-data; do
  [[ -f "$WORK/$vol.tgz" ]] || continue
  echo "restoring volume ${PROJECT}_${vol}"
  docker volume create "${PROJECT}_${vol}" >/dev/null
  docker run --rm -v "${PROJECT}_${vol}:/v" -v "$WORK:/in:ro" alpine:3.20 \
    tar xzf "/in/$vol.tgz" -C /v
done

echo "starting database"
"${COMPOSE[@]}" up -d postgres
for i in $(seq 1 60); do
  "${COMPOSE[@]}" exec -T postgres pg_isready -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" >/dev/null 2>&1 && break
  sleep 2
  [[ $i == 60 ]] && { echo "database did not come up" >&2; exit 1; }
done

echo "restoring database"
"${COMPOSE[@]}" exec -T postgres pg_restore -U "$POSTGRES_USER" -d "$POSTGRES_DB" --no-owner < "$WORK/db.dump"

echo "starting everything"
"${COMPOSE[@]}" up -d --build

cat <<EOF

Done. Still to do on this host / in DNS:
  1. A record ${BROKER_DNS} -> this host's IP (devices find the server by name).
  2. Firewall: allow ${PUBLIC_TCP_PORT}/tcp$( [[ "$MODE" != "--behind-proxy" ]] && echo ", 80/tcp, 443/tcp" ).
  3. Check: $( [[ "$MODE" == "--behind-proxy" ]] && echo "curl -sS http://127.0.0.1:${PUBLIC_PORT}/health" || echo "curl -sS https://${SITE_ADDRESS}/health" )
  4. Delete the archive.
EOF
