#!/usr/bin/env bash
set -euo pipefail

if [[ "${1:-}" == "--dry-run" ]]; then
  exec node "$(dirname "$0")/synthetic-check.mjs" --dry-run
fi

if [[ -z "${SYNTHETIC_BASE_URL:-}" ]]; then
  if [[ "${REQUIRE_SYNTHETIC:-0}" == 1 ]]; then
    echo 'SYNTHETIC_BASE_URL is required when REQUIRE_SYNTHETIC=1' >&2
    exit 2
  fi
  echo 'SYNTHETIC_BASE_URL is unset; public check is intentionally skipped.'
  echo 'Configure it as an out-of-band repository/org variable or local environment value.'
  exit 0
fi

exec node "$(dirname "$0")/synthetic-check.mjs"
