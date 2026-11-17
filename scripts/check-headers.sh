#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright Contributors to the project. See NOTICE.

# Every source and configuration file starts with the SPDX licence header
# (CONTRIBUTING.md). Run by CI.
#
#   scripts/check-headers.sh          exit 1 and the files without the header
#   scripts/check-headers.sh --fix    add the header where it is missing
set -euo pipefail
cd "$(dirname "$0")/.."
LIC='SPDX-License-Identifier: Apache-2.0'
COPY='Copyright Contributors to the project. See NOTICE.'

# files that carry the header; documentation, data and lock files do not
files() {
  git ls-files -- '*.go' '*.c' '*.h' '*.py' '*.sh' '*.sql' '*.conf' '*.txt' \
    '*Dockerfile' '*Caddyfile' '*.yml' '*.yaml' '.env.example' \
    ':!.github/ISSUE_TEMPLATE/*' ':!*/testdata/*'
}

prefix() {   # comment syntax of a file
  case "$1" in
    *.go|*.c|*.h) echo '//' ;;
    *.sql) echo '--' ;;
    *) echo '#' ;;
  esac
}

fix() {   # header after a shebang or a Dockerfile parser directive, then a blank line
  local f=$1 p tmp
  p=$(prefix "$f"); tmp=$(mktemp)
  {
    first=$(head -n 1 "$f")
    if [[ $first == '#!'* || $first == '# syntax='* ]]; then
      echo "$first"; rest_from=2
    else
      rest_from=1
    fi
    echo "$p $LIC"
    echo "$p $COPY"
    next=$(sed -n "${rest_from}p" "$f")
    [[ -n $next ]] && echo
    tail -n +"$rest_from" "$f"
  } > "$tmp"
  chmod --reference="$f" "$tmp"; mv "$tmp" "$f"
}

missing=()
while IFS= read -r f; do
  head -n 3 "$f" | grep -qF "$LIC" || missing+=("$f")
done < <(files)

if [[ ${1:-} == --fix ]]; then
  for f in "${missing[@]}"; do fix "$f"; echo "header added: $f"; done
  exit 0
fi
if (( ${#missing[@]} )); then
  echo "files without the SPDX header (scripts/check-headers.sh --fix):" >&2
  printf '  %s\n' "${missing[@]}" >&2
  exit 1
fi
echo "headers: ok"
