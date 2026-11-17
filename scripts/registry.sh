#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright Contributors to the project. See NOTICE.

# Device registry without the service layer (PKI_MODE=own): publishes the same
# retained messages the layer would, through the broker's internal listener.
# The registry is optional: a device with a valid certificate is admitted
# without an entry (its row appears at the first connection).
#
#   scripts/registry.sh bound   DEVICE [TYPE] [LAT LON] [TZ] [RULES]   RULES: a JSON object (docs/rules.md)
#   scripts/registry.sh unbound DEVICE
#   scripts/registry.sh revoked DEVICE
#   scripts/registry.sh clear   DEVICE        remove the retained message (the stored state stays)
#
# binding_ver: the current time in milliseconds (always greater than the last one).
set -euo pipefail
cd "$(dirname "$0")/.."
[[ -f .env ]] && set -a && . ./.env && set +a
STATE=${1:?usage: registry.sh bound|unbound|revoked|clear DEVICE ...}
DEV=${2:?device id}
[[ "$DEV" =~ ^[a-z0-9][a-z0-9-]{2,62}$ ]] || { echo "bad device id '$DEV'" >&2; exit 1; }
pub() { docker compose exec -T mqtt mosquitto_pub -h 127.0.0.1 -p 1883 -u server -P "$MQTT_SERVER_PASSWORD" -q 1 -r "$@"; }
if [[ "$STATE" == clear ]]; then pub -t "registry/dev/$DEV" -n; echo "cleared $DEV"; exit 0; fi
[[ "$STATE" =~ ^(bound|unbound|revoked)$ ]] || { echo "state: bound, unbound, revoked or clear" >&2; exit 1; }
TYPE=${3:-}; LAT=${4:-}; LON=${5:-}; TZ_=${6:-}; RULES=${7:-}
[[ -z "$RULES" || "$RULES" == \{*\} ]] || { echo "RULES must be a JSON object, e.g. '{\"rated_current_a\":16}'" >&2; exit 1; }
VER=$(date +%s%3N)
AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
LOC=""
[[ -n "$LAT" && -n "$LON" ]] && LOC=$(printf ',"lat":%s,"lon":%s' "$LAT" "$LON")
[[ -n "$RULES" ]] && LOC="$LOC,\"rules\":$RULES"
MSG=$(printf '{"v":1,"device_id":"%s","state":"%s","type":"%s","binding_ver":%s,"at":"%s","tz":"%s"%s}' \
  "$DEV" "$STATE" "$TYPE" "$VER" "$AT" "$TZ_" "$LOC")
pub -t "registry/dev/$DEV" -m "$MSG"
echo "$MSG"
