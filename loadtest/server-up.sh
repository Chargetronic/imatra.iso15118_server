#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright Contributors to the project. See NOTICE.

# Starts the load-test deployment on this host: the test service layer of the
# test harness (github.com/chargetronic/imatra.test_harness) and this checkout of
# Imatra in PKI_MODE=layer, tuned for a fleet of ten thousand devices; then
# hands the generator the files it needs (in $LT, mode 700):
#   ca.crt, root.crt, root.key   the deployment root of the TEST layer: fleetgen
#                                checks the broker with it and signs its devices
#   secrets                      FLEET_API_KEY (operator API)
# Run from the repo root:  loadtest/server-up.sh
# Env: HARNESS_DIR (../imatra.test_harness), ENGINES (2), LT (~/loadtest),
#      LT_BIND: this host's private address when the generator runs on another
#      host (the broker certificate names it; the API listens on it, port 8100).
# Nothing here is a production secret: every root is created by the test layer.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
IMATRA=$PWD
HARNESS_DIR=${HARNESS_DIR:-$IMATRA/../imatra.test_harness}
ENGINES=${ENGINES:-2}
LT=${LT:-$HOME/loadtest}
[ -x "$HARNESS_DIR/compose/up.sh" ] || { echo "test harness not found in $HARNESS_DIR" >&2; exit 1; }
umask 077
mkdir -p "$LT"

if [ ! -f .env ]; then
  r() { openssl rand -hex "$1"; }
  KEY=$(r 24)
  sed -e "s|^SITE_ADDRESS=.*|SITE_ADDRESS=localhost|" \
      -e "s|^BROKER_DNS=.*|BROKER_DNS=mqtt|" \
      -e "s|^PKI_MODE=.*|PKI_MODE=layer|" \
      -e "s|^POSTGRES_PASSWORD=.*|POSTGRES_PASSWORD=$(r 16)|" \
      -e "s|^MQTT_SERVER_PASSWORD=.*|MQTT_SERVER_PASSWORD=$(r 16)|" \
      -e "s|^OPERATOR_API_KEYS=.*|OPERATOR_API_KEYS=\"loadtest:$KEY\"|" \
      .env.example > .env
  # a machine with 8 GB for ten thousand devices; the generator runs beside it
  cat >> .env <<'ENV'

# --- load test (loadtest/server-up.sh) ---
PG_SHARED_BUFFERS=1GB
PG_EFFECTIVE_CACHE_SIZE=3GB
PG_WORK_MEM=8MB
PG_MAINTENANCE_WORK_MEM=256MB
PG_MAX_WAL_SIZE=4GB
PG_SHM_SIZE=256m
PG_MEM_LIMIT=2g
VALKEY_MAXMEMORY=768mb
VALKEY_MEM_LIMIT=1g
MQTT_MEM_LIMIT=768m
ENGINE_MEM_LIMIT=512m
ENV
  printf 'FLEET_API_KEY=%s\n' "$KEY" > "$LT/secrets"
  echo ".env written; operator key in $LT/secrets"
fi

# the test layer and Imatra from this checkout, no test station
export DEPLOYMENT_DNS="mqtt${LT_BIND:+,$LT_BIND}"
IMATRA_DIR=$IMATRA IMATRA_REF=local NO_STATION=1 "$HARNESS_DIR/compose/up.sh"
I="docker compose -p imatra-harness-server -f $IMATRA/docker-compose.yml -f $HARNESS_DIR/compose/imatra.override.yml --project-directory $IMATRA"
[ -n "${LT_BIND:-}" ] && I="$I -f $IMATRA/loadtest/compose.remote-gen.yml"
$I up -d --scale engine="$ENGINES" >/dev/null
echo "waiting for the API ..."
for _ in $(seq 60); do
  $I exec -T api python -c "import urllib.request;urllib.request.urlopen('http://127.0.0.1:8000/health')" >/dev/null 2>&1 && break
  sleep 2
done

# the deployment root of the test layer, for the generator
H="docker compose -f $HARNESS_DIR/compose/harness.yml"
$H run --rm --no-deps -T layer-init sh -c 'cat /data/pki/deployments/test-deployment/ca.crt' > "$LT/ca.crt"
$H run --rm --no-deps -T layer-init sh -c 'cat /data/pki/deployments/test-deployment/ca.key' > "$LT/root.key"
cp "$LT/ca.crt" "$LT/root.crt"
chmod 600 "$LT"/*
$I ps --format 'table {{.Name}}\t{{.Status}}'
echo "ready: $ENGINES engine(s); generator files in $LT"
