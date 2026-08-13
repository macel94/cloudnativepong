#!/usr/bin/env bash
# Prove the rollout gate and rollback timing on one disposable Kubernetes
# context. This script cannot select production by accident.
set -euo pipefail

usage() {
  echo "usage: $0 --context k3d-negative-rollout-<run-id> [--namespace pong] [--timeout-seconds 60] --output FILE" >&2
  exit 2
}

context=''
namespace='pong'
timeout_seconds=60
output=''
while (($#)); do
  case "$1" in
    --context) context="${2:?missing context}"; shift 2 ;;
    --namespace) namespace="${2:?missing namespace}"; shift 2 ;;
    --timeout-seconds) timeout_seconds="${2:?missing timeout}"; shift 2 ;;
    --output) output="${2:?missing output}"; shift 2 ;;
    -h|--help) usage ;;
    *) usage ;;
  esac
done

[[ "${DISPOSABLE_ROLLOUT_APPROVED:-}" == 1 ]] || {
  echo 'DISPOSABLE_ROLLOUT_APPROVED=1 is required; no cluster action was taken' >&2
  exit 2
}
[[ "$context" =~ ^k3d-negative-rollout-[0-9]+$ ]] || {
  echo 'context must be an exact k3d-negative-rollout-<numeric-run-id> context' >&2
  exit 2
}
[[ "$namespace" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || {
  echo 'namespace must be a Kubernetes DNS label' >&2
  exit 2
}
[[ "$timeout_seconds" =~ ^[1-9][0-9]*$ ]] || {
  echo 'timeout-seconds must be a positive integer' >&2
  exit 2
}
[[ -n "$output" ]] || usage

command -v kubectl >/dev/null 2>&1 || {
  echo 'kubectl is required' >&2
  exit 127
}

# Check the named context before any mutation. Refuse aliases and production
# context names even if a caller supplies a surprising kubeconfig.
kubectl config get-contexts --no-headers 2>/dev/null | awk '{print $1}' | grep -Fxq "$context" || {
  echo "disposable context $context does not exist" >&2
  exit 2
}

kubectl --context "$context" -n "$namespace" get deployment/pong-gateway >/dev/null

bad_image='cloudnativepong-gateway:negative-missing-image'
started_epoch=$(date +%s)
started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
# This is intentionally a disposable-only mutable-looking local image name;
# it never enters a production record or production manifest.
kubectl --context "$context" -n "$namespace" set image deployment/pong-gateway "gateway=$bad_image" >/dev/null

set +e
kubectl --context "$context" -n "$namespace" rollout status deployment/pong-gateway --timeout="${timeout_seconds}s" >/dev/null 2>&1
eg_status=$?
set -e
detected_epoch=$(date +%s)
detected_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
if (( eg_status == 0 )); then
  echo 'negative rollout unexpectedly succeeded; refusing to call this test green' >&2
  exit 1
fi

# Recovery is the disposable equivalent of the reviewed production action. A
# real production rollback must be a Git commit reconciled by Flux instead.
kubectl --context "$context" -n "$namespace" rollout undo deployment/pong-gateway >/dev/null
kubectl --context "$context" -n "$namespace" rollout status deployment/pong-gateway --timeout="${timeout_seconds}s" >/dev/null
recovered_epoch=$(date +%s)
recovered_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)

python3 - "$output" "$started_at" "$detected_at" "$recovered_at" "$started_epoch" "$detected_epoch" "$recovered_epoch" <<'PY'
import json
import sys
from pathlib import Path
output, started, detected, recovered, started_epoch, detected_epoch, recovered_epoch = sys.argv[1:]
data = {
    "schema_version": "belacca.negative-rollout.v1",
    "target": "disposable",
    "status": "passed",
    "fault": "missing_image",
    "rollout_started_at": started,
    "failure_detected_at": detected,
    "rollback_recovered_at": recovered,
    "detection_time_seconds": max(0, int(detected_epoch) - int(started_epoch)),
    "recovery_time_seconds": max(0, int(recovered_epoch) - int(detected_epoch)),
    "rollback_action": "disposable_kubectl_undo",
    "contains_secrets": False,
    "contains_user_data": False,
}
Path(output).write_text(json.dumps(data, indent=2) + "\n")
PY
printf 'negative disposable rollout detected and recovered; evidence: %s\n' "$output"
