#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
# Copyright Contributors to the project. See NOTICE.

# Refuses to start a deployment with empty, example or short secrets.
# Runs once on every "docker compose up" before the PKI, the broker and the
# database (service "preflight" in docker-compose.yml); the rest of the
# deployment waits for it to succeed.
#
# Checked (from .env):
#   POSTGRES_PASSWORD, MQTT_SERVER_PASSWORD   set, not an example value, 16+ characters
#   OPERATOR_API_KEYS                         may be empty; each "name:key" pair has a key
#                                             of 16+ characters that is not an example value
# Generate a value with: openssl rand -hex 24
set -eu

fail=0
bad() { echo "preflight: $*" >&2; fail=1; }

# an example value from .env.example or an obvious placeholder
example() {
  case "$1" in
    change-me|changeme|replace-with*|example*|password|secret|test|admin) return 0 ;;
  esac
  return 1
}

secret() { # name value
  if [ -z "$2" ]; then
    bad "$1 is empty in .env"
  elif example "$2"; then
    bad "$1 still has the example value from .env.example"
  elif [ "${#2}" -lt 16 ]; then
    bad "$1 is shorter than 16 characters"
  fi
}

secret POSTGRES_PASSWORD "${POSTGRES_PASSWORD:-}"
secret MQTT_SERVER_PASSWORD "${MQTT_SERVER_PASSWORD:-}"

for pair in ${OPERATOR_API_KEYS:-}; do
  name=${pair%%:*}
  key=${pair#*:}
  if [ "$name" = "$pair" ] || [ -z "$name" ]; then
    bad "OPERATOR_API_KEYS: \"$name\" is not a name:key pair"
  else
    secret "OPERATOR_API_KEYS ($name)" "$key"
  fi
done

if [ "$fail" -ne 0 ]; then
  echo "preflight: the deployment does not start with these values. Set them in .env" >&2
  echo "preflight: (e.g. openssl rand -hex 24) and run docker compose up again." >&2
  exit 1
fi
echo "preflight: ok"
