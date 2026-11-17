#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright Contributors to the project. See NOTICE.

# Names in the contracts (device protocol, registry, business rules, schema,
# API, events, settings, metrics) and in the code are functional only: no
# product, no brand, no server edition. Edition and organisation names belong
# to packaging and documentation only (README and the other community files at
# the root, NOTICE, THIRD_PARTY, the API
# title and EDITION in api/app/main.py, image names). Run by CI.
#
#   scripts/check-names.sh        exit 1 and the lines found when a name slipped in
set -euo pipefail
cd "$(dirname "$0")/.."
# products and brands; editions of the server line
WORDS='smart ?plug|wallbox|chargetronic cloud|imatra|mikkeli|tampere|helsinki'
# allowed: packaging and documentation; this script; the load-test kit (it names
# the test harness repository and the compose project the harness starts)
ALLOW='^(loadtest/|README\.md|docs/deployment\.md|NOTICE|THIRD_PARTY\.md|LICENSE|GOVERNANCE\.md|CONTRIBUTING\.md|SECURITY\.md|SUPPORT\.md|ROADMAP\.md|CHANGELOG\.md|RELEASE\.md|CODE_OF_CONDUCT\.md|COMMITTERS\.csv|scripts/check-names\.sh|\.github/)'
found=$(git grep -niE "$WORDS" -- . ':!*.sum' \
  | grep -vE "$ALLOW" \
  | grep -vE 'github\.com/chargetronic/imatra\.iso15118_server|ghcr\.io/chargetronic/imatra\.iso15118_server|EDITION = "imatra"|title="Imatra|COMPOSE_PROJECT_NAME:-imatra|name: \$\{COMPOSE_PROJECT_NAME|POSTGRES_(DB|USER)=imatra|Europe/Helsinki|imatra-(engine|api|gateway|mqtt|pki)' \
  || true)
if [[ -n "$found" ]]; then
  echo "names that do not belong in the code or the contracts:" >&2
  echo "$found" >&2
  exit 1
fi
echo "names: ok"
