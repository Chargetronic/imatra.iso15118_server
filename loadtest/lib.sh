# SPDX-License-Identifier: Apache-2.0
# Copyright Contributors to the project. See NOTICE.

# Shared by the load-test scripts: which deployment server-up.sh started.
#   LT_MODE own    (default) this checkout alone, PKI_MODE=own: the deployment's
#                  own CA, a TEST CA here, signs the generator's devices
#   LT_MODE layer  with the test service layer of the test harness
#                  (github.com/chargetronic/imatra.test_harness), PKI_MODE=layer:
#                  admission and the registry as with a service layer
# server-up.sh records the mode in $LT/mode; the other scripts read it.
LT=${LT:-$HOME/loadtest}
if [ -z "${LT_MODE:-}" ] && [ -s "$LT/mode" ]; then LT_MODE=$(cat "$LT/mode"); fi
LT_MODE=${LT_MODE:-own}
HARNESS_DIR=${HARNESS_DIR:-$PWD/../imatra.test_harness}
case "$LT_MODE" in
  own)   P=imatra-lt
         I="docker compose -p $P -f $PWD/docker-compose.yml --project-directory $PWD" ;;
  layer) P=imatra-harness-server
         I="docker compose -p $P -f $PWD/docker-compose.yml -f $HARNESS_DIR/compose/imatra.override.yml --project-directory $PWD"
         H="docker compose -f $HARNESS_DIR/compose/harness.yml" ;;
  *)     echo "LT_MODE must be own or layer" >&2; exit 2 ;;
esac
[ -n "${LT_BIND:-}" ] && I="$I -f $PWD/loadtest/compose.remote-gen.yml"
need_layer() { [ "$LT_MODE" = layer ] || { echo "$1 needs LT_MODE=layer (the test service layer)" >&2; exit 2; }; }
