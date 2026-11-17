#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright Contributors to the project. See NOTICE.

# Registers and binds the whole fleet in the test layer at once (ids
# fleet-00001.., as fleetgen names them) and waits until the deployment has
# applied every registry message: the initial registry load (F4-d).
#   loadtest/fleet-bind.sh N [OP]     OP: add (default), bind, unbind, revoke
# Prints the time from the layer's write to the last device seen as bound.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
N=${1:?usage: fleet-bind.sh N [add|bind|unbind|revoke]}
OP=${2:-add}
HARNESS_DIR=${HARNESS_DIR:-$PWD/../imatra.test_harness}
H="docker compose -f $HARNESS_DIR/compose/harness.yml"
I="docker compose -p imatra-harness-server -f $PWD/docker-compose.yml -f $HARNESS_DIR/compose/imatra.override.yml --project-directory $PWD"
set -a; . ./.env; set +a
psql() { $I exec -T postgres psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -At -c "$1"; }
want=bound; [ "$OP" = unbind ] && want=unbound; [ "$OP" = revoke ] && want=revoked

t0=$(date +%s.%N)
$H run --rm --no-deps -T layer layer fleet "$OP" fleet- 1 "$N"
t1=$(date +%s.%N)
for _ in $(seq 900); do
  n=$(psql "select count(*) from devices where device_id like 'fleet-%' and bind_state = '$want'")
  [ "${n:-0}" -ge "$N" ] && break
  sleep 1
done
t2=$(date +%s.%N)
ev=$(psql "select count(*) from domain_events where type = 'device.$want' and device_id like 'fleet-%' and recorded_at >= to_timestamp($t0)")
printf 'fleet %s: %s devices; layer write %.1f s; all %s at the deployment after %.1f s; feed events device.%s: %s\n' \
  "$OP" "$N" "$(awk "BEGIN{print $t1-$t0}")" "$want" "$(awk "BEGIN{print $t2-$t0}")" "$want" "$ev"
[ "${n:-0}" -ge "$N" ] || { echo "only $n of $N $want" >&2; exit 1; }
