#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

cat > "$tmp/input.json" <<'EOF'
{
  "source_commit": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "images": [
    {"reference": "ghcr.io/macel94/cloudnativepong-api@sha256:1111111111111111111111111111111111111111111111111111111111111111", "attestation": "verified"},
    {"reference": "ghcr.io/macel94/cloudnativepong-room@sha256:2222222222222222222222222222222222222222222222222222222222222222", "attestation": "verified"},
    {"reference": "ghcr.io/macel94/cloudnativepong-static@sha256:3333333333333333333333333333333333333333333333333333333333333333", "attestation": "verified"},
    {"reference": "ghcr.io/macel94/cloudnativepong-gateway@sha256:4444444444444444444444444444444444444444444444444444444444444444", "attestation": "verified"}
  ],
  "promotion": {
    "environment": "production",
    "status": "succeeded",
    "source_commit_at": "2026-01-01T00:00:00Z",
    "started_at": "2026-01-01T00:01:00Z",
    "completed_at": "2026-01-01T00:02:00Z",
    "deployment_revision": "deployment/pong-api:42",
    "flux_source_revision": "sha1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "flux_kustomization_revision": "sha1:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
    "synthetic": {"status": "passed", "started_at": "2026-01-01T00:01:10Z", "completed_at": "2026-01-01T00:01:20Z", "duration_ms": 10000}
  },
  "rollback": {"status": "not_required", "action": null, "detected_at": null, "recovered_at": null, "detection_time_seconds": null, "recovery_time_seconds": null},
  "dora": {"window_start": "2025-12-01T00:00:00Z", "window_end": "2026-01-01T00:00:00Z", "deployment_count": 10, "failed_deployment_count": 1},
  "privacy": {"contains_secrets": false, "contains_user_data": false}
}
EOF

python3 "$root/scripts/release-record.py" generate --input "$tmp/input.json" --output "$tmp/record.json"
python3 "$root/scripts/release-record.py" validate --input "$tmp/record.json"

expect_failure() {
  if "$@" >/dev/null 2>&1; then
    echo "unexpected success: $*" >&2
    exit 1
  fi
}

# Mutable tags can never enter a promotion record.
cp "$tmp/input.json" "$tmp/mutable.json"
 sed -i 's#@sha256:1111111111111111111111111111111111111111111111111111111111111111#:sha-main#' "$tmp/mutable.json"
expect_failure python3 "$root/scripts/release-record.py" generate --input "$tmp/mutable.json"

# Secret/user-data-shaped fields are rejected even when the value is harmless.
cp "$tmp/input.json" "$tmp/private.json"
python3 - "$tmp/private.json" <<'PY'
import json, sys
path = sys.argv[1]
data = json.load(open(path))
data['promotion']['token'] = 'not-a-real-secret'
json.dump(data, open(path, 'w'))
PY
expect_failure python3 "$root/scripts/release-record.py" generate --input "$tmp/private.json"

# Timing must agree with the stated evidence.
cp "$tmp/input.json" "$tmp/timing.json"
sed -i 's/"duration_ms": 10000/"duration_ms": 1/' "$tmp/timing.json"
expect_failure python3 "$root/scripts/release-record.py" generate --input "$tmp/timing.json"

# A record with no attestation is not accepted as a successful promotion.
cp "$tmp/input.json" "$tmp/attestation.json"
sed -i '0,/"attestation": "verified"/s//"attestation": "not_run"/' "$tmp/attestation.json"
expect_failure python3 "$root/scripts/release-record.py" generate --input "$tmp/attestation.json"

# Completed rollback records must carry both detection and recovery timing.
cp "$tmp/input.json" "$tmp/rollback.json"
python3 - "$tmp/rollback.json" <<'PY'
import json, sys
path = sys.argv[1]
data = json.load(open(path))
data['promotion']['status'] = 'rolled_back'
data['promotion']['synthetic']['status'] = 'failed'
data['rollback'] = {
    'status': 'completed',
    'action': 'reviewed_git_flux_revert',
    'detected_at': '2026-01-01T00:01:20Z',
    'recovered_at': '2026-01-01T00:02:20Z',
    'detection_time_seconds': 10,
    'recovery_time_seconds': 60,
}
json.dump(data, open(path, 'w'))
PY
python3 "$root/scripts/release-record.py" generate --input "$tmp/rollback.json" >/dev/null

echo 'release record validates privacy, immutable digests, timing, rollback, and DORA arithmetic'
