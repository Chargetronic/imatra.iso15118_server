#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright Contributors to the project. See NOTICE.

# Runs one load-test scenario on this host against the deployment started by
# server-up.sh and writes results/<label>/: report.md, fleetgen.json,
# fleetgen.log, samples.jsonl, server.json, events.txt.
#   loadtest/gen-run.sh <scenario> [label]
# Scenarios (N devices, default 5000; ~65 % get an EV):
#   f0      short check of the kit (use N=500)
#   steady  steady state (sessions end on their own, spread over 5 min), a stage of
#           2 000 from the server's pool at 12 min and its cancel
#   reconnect  every connection dropped, back over mTLS within 60 s
#   stage   a short run with one stage of STAGE_N from the pool at 7 min: the time to
#           deliver a stage of a given size (run with growing STAGE_N)
#   stopwave  the charging fleet plugs in within 20 s and stops 6 min later, within
#           the same 20 s (a SessionStop wave; not a target, a limit to know)
#   faults  steady state with failures one at a time (F3): an engine killed (6 min),
#           Postgres stopped for 2 min (10 min), Valkey killed, restarts with its
#           AOF (15 min), relay stopped for 2 min (19 min)
#   flush   steady state, Valkey FLUSHALL at 8 min (working state lost: running
#           sessions end with FAILED_UnknownSession, new ones go on)
#   smoke   a minute of the kit end to end (use N=20; CI)
#   layerdown  steady state, the test layer stopped for 5 min at 6 min (F4-g; LT_MODE=layer)
# Env: N, CONN_RATE (200), SKEW (device clock offset, -30s), LT (~/loadtest),
#      SERVER: deploy@<private IP> of the deployment when this host is a separate
#      generator (the collector runs there over ssh; the files of $LT copied from it once),
#      STAGE_N (2000), STAGE_SPREAD (s, the server paces the stage over it; 0), PREFIX (fleet-).
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
SC=${1:?usage: gen-run.sh <f0|steady|reconnect> [label]}
N=${N:-5000}
CONN_RATE=${CONN_RATE:-200}
SKEW=${SKEW:--30s}
LT=${LT:-$HOME/loadtest}
STAGE_N=${STAGE_N:-2000}
if [ -n "${SERVER:-}" ] && [ ! -s "$LT/secrets" ]; then   # a separate generator: the files once
  mkdir -p "$LT" && chmod 700 "$LT"
  for f in secrets ca.crt root.crt root.key mode; do scp -q "$SERVER:loadtest/$f" "$LT/"; done
  chmod 600 "$LT"/*
fi
. loadtest/lib.sh
[ "$SC" = layerdown ] && need_layer layerdown
declare -A FLAGS=(
  [smoke]="-plug-spread 15s -charge 40s -duration 75s"
  [f0]="-plug-spread 1m -charge 4m -duration 5m"
  [steady]="-plug-spread 5m -charge 18m -duration 24m -activate-at 12m -activate-n $STAGE_N -activate-pool -activate-groups 1"
  [stopwave]="-plug-spread 20s -charge 6m -duration 8m"
  [stage]="-plug-spread 3m -charge 10m -duration 14m -activate-at 7m -activate-n $STAGE_N -activate-pool -activate-groups 1 -activate-spread ${STAGE_SPREAD:-0}"
  [reconnect]="-plug-spread 3m -charge 11m -duration 15m -reconnect-at 8m -reconnect-jitter 60s"
  [faults]="-plug-spread 3m -charge 30m -duration 25m"
  [flush]="-plug-spread 3m -charge 20m -duration 14m"
  [layerdown]="-plug-spread 3m -charge 20m -duration 15m"
)
[ -n "${FLAGS[$SC]:-}" ] || { echo "unknown scenario $SC" >&2; exit 1; }
LABEL=${2:-$(date +%m%d-%H%M)-$SC-$N}
OUT=$PWD/results/$LABEL
mkdir -p "$OUT"
[ -s "$LT/secrets" ] || { echo "run loadtest/server-up.sh first" >&2; exit 1; }
docker image inspect fleetgen >/dev/null 2>&1 || docker build -q -f engine/cmd/fleetgen/Dockerfile -t fleetgen engine >/dev/null

note() { echo "$(date -u +%FT%TZ) $*" | tee -a "$OUT/events.txt"; }
: > "$OUT/events.txt"
# the run's parameters, for loadtest/summary.py (results/summary.csv)
printf '%s\n' "SCENARIO=$SC" "N=$N" "STAGE_N=${STAGE_N:-}" "STAGE_SPREAD=${STAGE_SPREAD:-}" "SETTLE=${SETTLE:-}" \
  "CONN_RATE=$CONN_RATE" "GEN_ARGS=${GEN_ARGS:-}" "ENGINE_BUILD=${ENGINE_BUILD:-}" > "$OUT/run.env"
if [ -n "${SERVER:-}" ]; then   # the collector on the deployment's host
  RREPO=${SERVER_REPO:-imatra.iso15118_server}
  CPID=$(ssh -n "$SERVER" "cd $RREPO && mkdir -p results/$LABEL && { LT_PROJECT=$P nohup python3 loadtest/collect.py --out results/$LABEL > results/$LABEL/collect.log 2>&1 < /dev/null & echo \$!; }")
  stop_collector() { ssh -n "$SERVER" "kill -TERM $CPID; for i in \$(seq 60); do [ -s $RREPO/results/$LABEL/server.json ] && break; sleep 2; done"
                     for f in samples.jsonl server.json collect.log; do scp -q "$SERVER:$RREPO/results/$LABEL/$f" "$OUT/"; done; }
else
  LT_PROJECT=$P python3 loadtest/collect.py --out "$OUT" > "$OUT/collect.log" 2>&1 &
  COL=$!
  stop_collector() { kill -TERM $COL; wait $COL || true; }
fi
trap 'stop_collector 2>/dev/null || true' EXIT
note "$LABEL: $N devices, scenario $SC"

# failures on the deployment's host (loadtest/inject.sh), logged in events.txt
inj() { if [ -n "${SERVER:-}" ]; then ssh -n "$SERVER" "cd ${SERVER_REPO:-imatra.iso15118_server} && loadtest/inject.sh $1"; else loadtest/inject.sh "$1"; fi >> "$OUT/events.txt"; }
case "$SC" in
  faults)    ( sleep 360; inj engine-kill; sleep 240; inj pg-stop; sleep 120; inj pg-start
               sleep 180; inj valkey-kill; sleep 240; inj relay-stop; sleep 120; inj relay-start ) & ;;
  flush)     ( sleep 480; inj valkey-flush ) & ;;
  layerdown) ( sleep 360; inj layer-stop; sleep 300; inj layer-start ) & ;;
esac

# the generator on the deployment's own network (broker mqtt:8883, mTLS; API api:8000),
# or, on a separate host, against the deployment's private address
if [ -n "${SERVER:-}" ]; then
  IP=${SERVER#*@}; NET=host; BROKER=ssl://$IP:${PUBLIC_TCP_PORT:-8883}; API=http://$IP:8100
else
  NET=${P}_default; BROKER=ssl://mqtt:8883; API=http://api:8000
fi
# shellcheck disable=SC2086
docker run --rm --network "$NET" --user "$(id -u):$(id -g)" --ulimit nofile=1048576:1048576 \
  --env-file "$LT/secrets" -v "$LT:/lt:ro" -v "$OUT:/out" fleetgen \
  -broker "$BROKER" -ca /lt/ca.crt -client-ca /lt/root.crt -client-ca-key /lt/root.key \
  -prefix "${PREFIX:-fleet-}" -n "$N" -conn-rate "$CONN_RATE" -clock-skew "$SKEW" -api "$API" \
  ${FLAGS[$SC]} ${SETTLE:+-settle $SETTLE} ${GEN_ARGS:-} -report /out/fleetgen.json 2>&1 | tee "$OUT/fleetgen.log" \
  | grep -E --line-buffered "connecting|connected|settling|sessions|activation|reconnect|run time|report|interrupted|panic" || true
note "generator done"
echo "projector catching up (30 s) ..."
sleep 30
trap - EXIT
stop_collector
python3 loadtest/report.py "$OUT" > "$OUT/report.md"
echo "report: $OUT/report.md"
sed -n '1,30p' "$OUT/report.md"
python3 loadtest/summary.py results || true
