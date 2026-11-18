#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright Contributors to the project. See NOTICE.

# Starts the load-test deployment on this host, tuned for a fleet in the
# thousands, and hands the generator the files it needs (in $LT, mode 700):
#   ca.crt               the CA the broker's certificate is checked with
#   root.crt, root.key   the CA fleetgen signs its devices' certificates with
#   secrets              FLEET_API_KEY (operator API)
#   mode                 LT_MODE, for the other scripts
# Two modes (loadtest/lib.sh):
#   LT_MODE=own (default)  this checkout alone, PKI_MODE=own; the deployment's
#                          own CA signs the devices
#   LT_MODE=layer          with the test service layer of the test harness
#                          (HARNESS_DIR, ../imatra.test_harness), PKI_MODE=layer;
#                          then loadtest/fleet-bind.sh binds the fleet
# Run from the repo root:  loadtest/server-up.sh
# Env: LT_MODE, ENGINES (2), LT (~/loadtest), LT_BIND: this host's private
#      address when the generator runs on another host (the broker certificate
#      names it; the API listens on it, port 8100).
# Nothing here is a production secret: the CA of a load-test deployment is a
# TEST CA; do not run this on a deployment that serves real devices.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
IMATRA=$PWD
ENGINES=${ENGINES:-2}
LT=${LT:-$HOME/loadtest}
rm -f "$LT/mode"         # the mode of this start, not of an earlier one
. loadtest/lib.sh
if [ "$LT_MODE" = layer ]; then
  [ -x "$HARNESS_DIR/compose/up.sh" ] || { echo "test harness not found in $HARNESS_DIR" >&2; exit 1; }
fi
umask 077
mkdir -p "$LT"

if [ ! -f .env ]; then
  r() { openssl rand -hex "$1"; }
  KEY=$(r 24)
  sed -e "s|^SITE_ADDRESS=.*|SITE_ADDRESS=localhost|" \
      -e "s|^BROKER_DNS=.*|BROKER_DNS=mqtt|" \
      -e "s|^PKI_MODE=.*|PKI_MODE=$LT_MODE|" \
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
  # own mode: the broker certificate also names the private address
  [ "$LT_MODE" = own ] && [ -n "${LT_BIND:-}" ] && printf 'BROKER_IPS=%s\n' "$LT_BIND" >> .env
  printf 'FLEET_API_KEY=%s\n' "$KEY" > "$LT/secrets"
  echo ".env written; operator key in $LT/secrets"
fi

if [ "$LT_MODE" = layer ]; then
  # the test layer and Imatra from this checkout, no test station
  export DEPLOYMENT_DNS="mqtt${LT_BIND:+,$LT_BIND}"
  IMATRA_DIR=$IMATRA IMATRA_REF=local NO_STATION=1 "$HARNESS_DIR/compose/up.sh"
  $I up -d --scale engine="$ENGINES" >/dev/null
else
  $I up -d --build --scale engine="$ENGINES" >/dev/null
fi
echo "waiting for the API ..."
for _ in $(seq 90); do
  $I exec -T api python -c "import urllib.request;urllib.request.urlopen('http://127.0.0.1:8000/health')" >/dev/null 2>&1 && break
  sleep 2
done

if [ "$LT_MODE" = layer ]; then
  # the deployment root of the test layer, for the generator
  $H run --rm --no-deps -T layer-init sh -c 'cat /data/pki/deployments/test-deployment/ca.crt' > "$LT/ca.crt"
  $H run --rm --no-deps -T layer-init sh -c 'cat /data/pki/deployments/test-deployment/ca.key' > "$LT/root.key"
else
  # the deployment's own CA (a test CA: this is a load-test deployment)
  $I run --rm --no-deps -T pki-init ca > "$LT/ca.crt"
  $I run --rm --no-deps -T --entrypoint cat pki-init /pki/ca.key > "$LT/root.key"
fi
cp "$LT/ca.crt" "$LT/root.crt"
echo "$LT_MODE" > "$LT/mode"
chmod 600 "$LT"/*
$I ps --format 'table {{.Name}}\t{{.Status}}'
echo "ready: $ENGINES engine(s); generator files in $LT"
