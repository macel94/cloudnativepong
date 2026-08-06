#!/usr/bin/env bash
set -euo pipefail

if [[ "${1:-}" == "--dry-run" ]]; then
  exec node "$(dirname "$0")/synthetic-check.mjs" --dry-run
fi

if [[ -z "${SYNTHETIC_BASE_URL:-}" ]]; then
  echo 'SYNTHETIC_BASE_URL is required; refusing to report an unexecuted check as successful.' >&2
  echo 'The scheduled workflow defaults this to https://pong.belacca.com; set it explicitly for local or alternate targets.' >&2
  exit 2
fi

exec node "$(dirname "$0")/synthetic-check.mjs"
