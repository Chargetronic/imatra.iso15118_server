#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright Contributors to the project. See NOTICE.

# One failure (or its end) on the deployment's host, for gen-run.sh:
#   loadtest/inject.sh engine-kill|pg-stop|pg-start|valkey-kill|valkey-flush|relay-stop|relay-start|layer-stop|layer-start
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
HARNESS_DIR=${HARNESS_DIR:-$PWD/../imatra.test_harness}
P=imatra-harness-server
I="docker compose -p $P -f $PWD/docker-compose.yml -f $HARNESS_DIR/compose/imatra.override.yml --project-directory $PWD"
H="docker compose -f $HARNESS_DIR/compose/harness.yml"
case "${1:?what}" in
  engine-kill)  docker kill -s KILL $P-engine-1 >/dev/null; m="engine-1 killed (kill -9), Docker restarts it" ;;
  pg-stop)      $I stop postgres >/dev/null 2>&1; m="postgres stopped" ;;
  pg-start)     $I start postgres >/dev/null 2>&1; m="postgres started" ;;
  valkey-kill)  docker kill -s KILL $P-valkey-1 >/dev/null; m="valkey killed (kill -9), restarts with its AOF" ;;
  valkey-flush) docker exec $P-valkey-1 valkey-cli FLUSHALL >/dev/null; m="valkey FLUSHALL" ;;
  relay-stop)   $I stop relay >/dev/null 2>&1; m="relay stopped" ;;
  relay-start)  $I start relay >/dev/null 2>&1; m="relay started" ;;
  layer-stop)   $H stop layer layer-mqtt >/dev/null 2>&1; m="test layer stopped" ;;
  layer-start)  $H start layer-mqtt layer >/dev/null 2>&1; m="test layer started" ;;
  *) echo "unknown: $1" >&2; exit 2 ;;
esac
echo "$(date -u +%FT%TZ) $m"
