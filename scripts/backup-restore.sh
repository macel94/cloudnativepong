#!/usr/bin/env bash
# Wrapper kept intentionally dependency-light; Python's stdlib sqlite3 API is
# used instead of assuming sqlite3 exists inside the scratch application image.
set -euo pipefail
exec python3 "$(dirname "$0")/backup-restore.py" "$@"
