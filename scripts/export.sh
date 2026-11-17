#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright Contributors to the project. See NOTICE.

# Packs everything needed to move the server into ONE archive:
#   db.dump        PostgreSQL (pg_dump -Fc)
#   pki.tgz        private CA + broker certificate   <- devices trust this CA
#   mqtt-data.tgz  broker persistence (retained messages, sessions)
#   caddy-data.tgz the gateway's TLS certificates
#   env            the filled-in .env                <- secrets
#   commit         git commit of the running code
#
#   scripts/export.sh [output-dir]      (from the repository root)
#
# The archive holds the CA key and all passwords: keep it chmod 600, move it
# with scp, delete it after import.
set -euo pipefail
cd "$(dirname "$0")/.."

PROJECT=${COMPOSE_PROJECT_NAME:-imatra}   # compose project (docker-compose.yml "name"): volumes <project>_pki, ...
COMPOSE=(docker compose -f docker-compose.yml)
OUT_DIR=${1:-..}
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

[[ -f .env ]] || { echo "no .env in the repo root — is this a deployed checkout?" >&2; exit 1; }
set -a; . ./.env; set +a

echo "dumping database"
"${COMPOSE[@]}" exec -T postgres pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Fc > "$WORK/db.dump"
[[ -s "$WORK/db.dump" ]] || { echo "empty database dump" >&2; exit 1; }

for vol in pki mqtt-data caddy-data; do
  name="${PROJECT}_${vol}"
  if docker volume inspect "$name" >/dev/null 2>&1; then
    echo "packing volume $name"
    docker run --rm -v "$name:/v:ro" -v "$WORK:/out" alpine:3.20 \
      tar czf "/out/$vol.tgz" -C /v .
  fi
done
[[ -f "$WORK/pki.tgz" ]] || { echo "volume ${PROJECT}_pki missing — nothing to move?" >&2; exit 1; }

cp .env "$WORK/env"
git rev-parse HEAD > "$WORK/commit" 2>/dev/null || echo unknown > "$WORK/commit"

ARCHIVE="$OUT_DIR/deployment-export-$STAMP.tar"
umask 077
tar cf "$ARCHIVE" -C "$WORK" .
chmod 600 "$ARCHIVE"
echo
echo "written: $ARCHIVE ($(du -h "$ARCHIVE" | cut -f1))"
echo "contains the CA key and all passwords — move it with scp and delete it afterwards"
