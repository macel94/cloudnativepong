#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"
cat > "$tmp/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
state=${FAKE_KUBECTL_STATE:?}
args="$*"
if [[ "$args" == 'config get-contexts --no-headers' ]]; then
  printf '%s\n' "$FAKE_CONTEXT"
  exit 0
fi
if [[ "$args" == *' get deployment/pong-gateway' ]]; then
  exit 0
fi
if [[ "$args" == *' rollout status deployment/pong-gateway'* ]]; then
  if ! grep -q '^failure-detected$' "$state" 2>/dev/null; then
    printf 'failure-detected\n' >> "$state"
    exit 1
  fi
  exit 0
fi
if [[ "$args" == *' rollout undo deployment/pong-gateway' ]]; then
  printf 'undo\n' >> "$state"
  exit 0
fi
if [[ "$args" == *' set image deployment/pong-gateway '* ]]; then
  printf 'set-image\n' >> "$state"
  exit 0
fi
echo "unexpected fake kubectl call: $args" >&2
exit 1
EOF
chmod +x "$tmp/bin/kubectl"

expect_failure() {
  if "$@" >/dev/null 2>&1; then
    echo "unexpected success: $*" >&2
    exit 1
  fi
}

# No approval and no production-shaped context means no mutation.
expect_failure env PATH="$tmp/bin:$PATH" FAKE_CONTEXT=k3d-negative-rollout-123 \
  FAKE_KUBECTL_STATE="$tmp/state" "$root/scripts/negative-rollout.sh" \
  --context k3d-negative-rollout-123 --output "$tmp/no.json"
expect_failure env PATH="$tmp/bin:$PATH" DISPOSABLE_ROLLOUT_APPROVED=1 \
  FAKE_CONTEXT=k3d-negative-rollout-123 FAKE_KUBECTL_STATE="$tmp/state" \
  "$root/scripts/negative-rollout.sh" --context belacca-production --output "$tmp/no.json"

DISPOSABLE_ROLLOUT_APPROVED=1 PATH="$tmp/bin:$PATH" \
  FAKE_CONTEXT=k3d-negative-rollout-123 FAKE_KUBECTL_STATE="$tmp/state" \
  "$root/scripts/negative-rollout.sh" --context k3d-negative-rollout-123 \
  --output "$tmp/evidence.json"
python3 - "$tmp/evidence.json" "$tmp/state" <<'PY'
import json, sys
record = json.load(open(sys.argv[1]))
assert record['status'] == 'passed'
assert record['fault'] == 'missing_image'
assert record['detection_time_seconds'] >= 0
assert record['recovery_time_seconds'] >= 0
assert open(sys.argv[2]).read().count('undo') == 1
assert open(sys.argv[2]).read().count('set-image') == 1
PY

echo 'negative rollout guards context/approval and records failed rollout recovery'
