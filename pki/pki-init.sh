#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
# Copyright Contributors to the project. See NOTICE.

# PKI of one deployment: the CA the broker trusts for device certificates, and the
# broker's own TLS certificate (13.11.2026, OSS scope, tasks A3, A7).
#
# Two modes (PKI_MODE in .env):
#
#   own    (default) the deployment keeps its own CA — a community installation
#          without the Chargetronic service layer. The CA is created once;
#          device certificates are issued with "device" below.
#   layer  the service layer keeps a root for this cloud (cloud_id) and
#          issues the broker certificate and the devices' 7-day client
#          certificates (renewed daily by the devices). The deployment holds no CA
#          key: put the operator's files from the layer in with "import".
#
# Commands (docker compose run --rm pki-init <command>):
#   init                     (default, on every "up") check / create, see the modes
#   import                   PKI_MODE=layer: copy ca.crt and broker.pem (or broker.crt + broker.key) from /in
#                            (docker compose run --rm -v "$PWD/from-layer:/in:ro" pki-init import)
#   ca                       print the CA certificate (devices pin it)
#   device <id> [days]       PKI_MODE=own: new device key and certificate, PEM on stdout
#   sign <id> [days] < csr   PKI_MODE=own: certificate for a device CSR (key stays on the device)
#
# Device ids: lowercase letters, digits and '-', 3..63 characters; "server"
# is the services' account and is refused. The certificate's CN is the id;
# the broker takes it as the user name and allows dev/<id>/# only.
#
# Idempotent: an existing CA is never replaced (that would cut off every
# device). In "own" mode the broker certificate is reissued only when its
# names change or it is within 30 days of expiry.
set -eu
cd /pki
MODE=${PKI_MODE:-own}
CMD=${1:-init}
DEVICE_DAYS_DEFAULT=${DEVICE_CERT_DAYS:-365}

die() { echo "pki: $*" >&2; exit 1; }

check_id() {
  case "$1" in
    server|layer) die "'$1' is reserved ('server': the deployment's services, 'layer': the service layer)" ;;
    *[!a-z0-9-]*|-*) die "device id '$1': lowercase letters, digits and '-' only" ;;
  esac
  n=$(printf '%s' "$1" | wc -c)
  [ "$n" -ge 3 ] && [ "$n" -le 63 ] || die "device id '$1': 3..63 characters"
}

perms() {
  # Mosquitto (uid 1883) must read the broker key; the CA key stays root-only.
  [ -f ca.key ] && chmod 0600 ca.key
  chown 1883:1883 broker.key && chmod 0400 broker.key
  chmod 0644 ca.crt broker.crt
}

own_ca() {
  [ "$MODE" = own ] || die "PKI_MODE=$MODE: device certificates come from the service layer"
  [ -f ca.key ] || die "no CA key: run init first"
}

client_ext() {
  printf 'basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=clientAuth\n' > "$1"
}

case "$CMD" in
init)
  if [ "$MODE" = layer ]; then
    for f in ca.crt broker.crt broker.key; do
      [ -f "$f" ] || die "PKI_MODE=layer: $f missing — import the files from the service layer (pki-init import)"
    done
    [ ! -f ca.key ] || echo "pki: warning: ca.key present in PKI_MODE=layer (an own-mode CA left over?); the broker trusts ca.crt only" >&2
    openssl verify -CAfile ca.crt broker.crt >/dev/null || die "broker.crt is not issued by ca.crt"
    openssl x509 -in broker.crt -noout -checkend $((7*24*3600)) >/dev/null ||
      echo "pki: warning: broker certificate expires within 7 days — get a new one from the service layer" >&2
    perms
    echo "pki ready (layer): CA $(openssl x509 -in ca.crt -noout -subject), broker $(openssl x509 -in broker.crt -noout -enddate)"
    exit 0
  fi
  [ "$MODE" = own ] || die "PKI_MODE must be own or layer"
  : "${BROKER_DNS:?set BROKER_DNS in .env}"
  BROKER_IPS=${BROKER_IPS:-}
  if [ ! -f ca.key ]; then
    [ ! -f ca.crt ] || die "ca.crt without ca.key (layer files?) — set PKI_MODE=layer or remove them"
    echo "creating the deployment CA"
    openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
      -keyout ca.key -out ca.crt -days 3650 \
      -subj "/O=${CA_ORG:-ISO 15118 deployment}/CN=${CA_NAME:-Deployment device CA}" \
      -addext "basicConstraints=critical,CA:TRUE,pathlen:0" \
      -addext "keyUsage=critical,keyCertSign,cRLSign"
  fi
  san="DNS:${BROKER_DNS}"
  for ip in $BROKER_IPS; do san="${san},IP:${ip}"; done
  need=no
  if [ ! -f broker.crt ] || [ ! -f broker.san ] || [ "$(cat broker.san)" != "$san" ]; then
    need=yes
  elif ! openssl x509 -in broker.crt -noout -checkend $((30*24*3600)) >/dev/null; then
    need=yes
  fi
  if [ "$need" = yes ]; then
    echo "issuing broker certificate for ${san}"
    openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
      -keyout broker.key -out broker.csr -subj "/CN=${BROKER_DNS}"
    printf 'subjectAltName=%s\nbasicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=serverAuth\n' "$san" > broker.ext
    openssl x509 -req -in broker.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
      -out broker.crt -days 825 -sha256 -extfile broker.ext
    rm -f broker.csr broker.ext
    echo "$san" > broker.san
  fi
  perms
  echo "pki ready (own): $(openssl x509 -in broker.crt -noout -enddate)"
  ;;
import)
  [ "$MODE" = layer ] || die "import is for PKI_MODE=layer"
  t=$(mktemp -d)
  cp /in/* "$t"/ 2>/dev/null || true
  # the layer hands out broker.pem (key, then certificate): split it
  if [ -f "$t/broker.pem" ] && [ ! -f "$t/broker.crt" ]; then
    awk -v d="$t" '/BEGIN .*PRIVATE KEY/{f=d"/broker.key"} /BEGIN CERTIFICATE/{f=d"/broker.crt"} f{print > f}' "$t/broker.pem"
  fi
  set -- "$t"
  for f in ca.crt broker.crt broker.key; do [ -f "$1/$f" ] || die "/in/$f (or broker.pem) missing"; done
  openssl verify -CAfile "$1/ca.crt" "$1/broker.crt" >/dev/null || die "broker.crt is not issued by ca.crt"
  [ "$(openssl x509 -in "$1/broker.crt" -noout -pubkey | openssl sha256)" = \
    "$(openssl pkey -in "$1/broker.key" -pubout | openssl sha256)" ] || die "broker.key does not match broker.crt"
  cp "$1/ca.crt" ca.crt && cp "$1/broker.crt" broker.crt && cp "$1/broker.key" broker.key
  rm -rf "$1" broker.san
  perms
  echo "imported: CA $(openssl x509 -in ca.crt -noout -subject); restart the broker: docker compose restart mqtt"
  ;;
ca)
  cat ca.crt
  ;;
device)
  own_ca
  id=${2:?usage: device <id> [days]}; days=${3:-$DEVICE_DAYS_DEFAULT}
  check_id "$id"
  t=$(mktemp -d)
  openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -keyout "$t/k" -out "$t/csr" -subj "/CN=$id" 2>/dev/null
  client_ext "$t/ext"
  openssl x509 -req -in "$t/csr" -CA ca.crt -CAkey ca.key -CAcreateserial -out "$t/c" -days "$days" -sha256 -extfile "$t/ext" 2>/dev/null
  cat "$t/k" "$t/c"
  rm -rf "$t"
  echo "pki: device $id, $days days" >&2
  ;;
sign)
  own_ca
  id=${2:?usage: sign <id> [days] < device.csr}; days=${3:-$DEVICE_DAYS_DEFAULT}
  check_id "$id"
  t=$(mktemp -d)
  cat > "$t/csr"
  openssl req -in "$t/csr" -noout -verify 2>/dev/null || die "not a valid CSR"
  client_ext "$t/ext"
  # the broker takes the CN as the user name: it must be the id asked for
  cn=$(openssl req -in "$t/csr" -noout -subject -nameopt multiline | sed -n 's/^ *commonName *= *//p')
  [ "$cn" = "$id" ] || die "the CSR's CN '$cn' is not '$id'"
  openssl x509 -req -in "$t/csr" -CA ca.crt -CAkey ca.key -CAcreateserial -out "$t/c" -days "$days" -sha256 -extfile "$t/ext" 2>/dev/null
  cat "$t/c"
  rm -rf "$t"
  echo "pki: device $id, $days days" >&2
  ;;
*)
  die "unknown command '$CMD' (init, import, ca, device, sign)"
  ;;
esac
