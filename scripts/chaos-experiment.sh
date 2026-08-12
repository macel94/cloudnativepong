#!/usr/bin/env bash
set -euo pipefail

# Guarded, aggregate-only disposable chaos drill runner. The workflow creates
# the cluster; this helper may mutate only its exact run-owned context.
: "${KUBE_CONTEXT:?KUBE_CONTEXT is required}"
: "${NAMESPACE:?NAMESPACE is required}"
: "${SCENARIO:?SCENARIO is required}"
: "${ARTIFACT_DIR:?ARTIFACT_DIR is required}"
: "${PONG_EXPERIMENT_MODE:?PONG_EXPERIMENT_MODE is required}"
: "${PONG_EXPERIMENT_APPROVED:?PONG_EXPERIMENT_APPROVED is required}"
: "${PONG_EXPERIMENT_TARGET:?PONG_EXPERIMENT_TARGET is required}"

RUNS="${RUNS:-3}"
RECOVERY_TARGET_MS="${RECOVERY_TARGET_MS:-360000}"
DRILL_TIMEOUT_SECONDS="${DRILL_TIMEOUT_SECONDS:-600}"
CLEANUP_TIMEOUT_SECONDS="${CLEANUP_TIMEOUT_SECONDS:-120}"
EXPERIMENT_DEADLINE_SECONDS="${EXPERIMENT_DEADLINE_SECONDS:-1200}"
ABORT_THRESHOLD="${ABORT_THRESHOLD:-2}"
mkdir -p "$ARTIFACT_DIR"

fail_config() { echo "chaos configuration rejected: $*" >&2; exit 2; }
[[ "$PONG_EXPERIMENT_MODE" == chaos ]] || fail_config 'mode must be chaos'
[[ "$PONG_EXPERIMENT_APPROVED" == 1 ]] || fail_config 'explicit approval is required'
[[ "$PONG_EXPERIMENT_TARGET" == isolated ]] || fail_config 'target must be isolated'
[[ "$KUBE_CONTEXT" =~ ^k3d-cnp-chaos-[0-9]+$ ]] || fail_config 'context is not a run-owned chaos cluster'
[[ "$NAMESPACE" =~ ^cnp-chaos-[0-9]+$ ]] || fail_config 'namespace is not run-owned'
[[ "$SCENARIO" =~ ^(api-restart|gateway-restart|room-termination|node-drain|resource-pressure)$ ]] || fail_config 'unsupported scenario'
[[ "$RUNS" =~ ^[1-3]$ ]] || fail_config 'runs must be between 1 and 3'
[[ "$RECOVERY_TARGET_MS" =~ ^[0-9]+$ && "$RECOVERY_TARGET_MS" -le 360000 ]] || fail_config 'recovery target is invalid'
[[ "$DRILL_TIMEOUT_SECONDS" =~ ^[1-9][0-9]*$ && "$DRILL_TIMEOUT_SECONDS" -le 900 ]] || fail_config 'drill timeout is invalid'
[[ "$CLEANUP_TIMEOUT_SECONDS" =~ ^[1-9][0-9]*$ && "$CLEANUP_TIMEOUT_SECONDS" -le 120 ]] || fail_config 'cleanup timeout is invalid'
[[ "$EXPERIMENT_DEADLINE_SECONDS" =~ ^[1-9][0-9]*$ && "$EXPERIMENT_DEADLINE_SECONDS" -le 1800 ]] || fail_config 'experiment deadline is invalid'
[[ "$ABORT_THRESHOLD" =~ ^[1-3]$ ]] || fail_config 'abort threshold is invalid'

DEADLINE=$(( $(date +%s) + EXPERIMENT_DEADLINE_SECONDS ))
check_deadline() { (( $(date +%s) < DEADLINE )) || fail_config 'hard experiment deadline reached'; }

kubectl() { command kubectl --context "$KUBE_CONTEXT" "$@"; }
start_ms() { date +%s%3N; }
record_failure() { failures=$((failures + 1)); }
wait_ready() {
  timeout "${DRILL_TIMEOUT_SECONDS}s" kubectl -n "$NAMESPACE" rollout status "deployment/$1" >/dev/null
}
wait_health() {
  [[ -n "${BASE_URL:-}" ]] || return 1
  timeout "${DRILL_TIMEOUT_SECONDS}s" bash -c \
    'until curl --fail --silent --show-error --max-time 3 "$1/health" >/dev/null; do sleep 1; done' \
    _ "$BASE_URL"
}

restore() {
  local status=$?
  set +e
  if [[ ! -f "$ARTIFACT_DIR/chaos-result.json" ]]; then
    cat > "$ARTIFACT_DIR/chaos-result.json" <<EOF
{
  "mode": "chaos",
  "scenario": "${SCENARIO:-unknown}",
  "runs": 0,
  "requested_runs": ${RUNS:-0},
  "recovery_target_ms": ${RECOVERY_TARGET_MS:-360000},
  "recovery_p95_ms": 0,
  "objective": "P95 recovery under six minutes",
  "objective_passed": false,
  "failures": 1,
  "cleanup_verified": false,
  "aggregate_only": true
}
EOF
  fi
  if [[ -n "${PRESSURE_POD:-}" ]]; then
    timeout "${CLEANUP_TIMEOUT_SECONDS}s" kubectl -n "$NAMESPACE" delete pod "$PRESSURE_POD" --ignore-not-found >/dev/null 2>&1
  fi
  if [[ -n "${DRAINED_NODE:-}" ]]; then
    timeout "${CLEANUP_TIMEOUT_SECONDS}s" kubectl uncordon "$DRAINED_NODE" >/dev/null 2>&1
  fi
  if [[ -n "${ARTIFACT_DIR:-}" ]]; then
    printf '%s\n' "$([[ $status -eq 0 ]] && echo verified || echo failed)" > "$ARTIFACT_DIR/drill-cleanup"
  fi
  exit "$status"
}
trap restore EXIT INT TERM

failures=0
cleanup_failures=0
samples=()
check_deadline
# A repeatable concurrent-room baseline is mandatory before fault injection.
BASE_URL="${BASE_URL:-}"
if [[ -n "$BASE_URL" ]]; then
  timeout "$DRILL_TIMEOUT_SECONDS" env \
    PONG_EXPERIMENT_MODE=chaos PONG_EXPERIMENT_APPROVED=1 PONG_EXPERIMENT_TARGET=isolated \
    LOAD_SMOKE_BASE_URL="$BASE_URL" ./scripts/load-smoke.sh \
    --iterations=3 --concurrency=2 --timeout-ms=10000 --max-duration-ms=60000 \
    --abort-threshold="$ABORT_THRESHOLD" > "$ARTIFACT_DIR/baseline.json" 2> "$ARTIFACT_DIR/baseline.stderr" || {
      printf '%s\n' failed > "$ARTIFACT_DIR/baseline-status"
      fail_config 'baseline failed; chaos injection refused'
    }
  printf '%s\n' passed > "$ARTIFACT_DIR/baseline-status"
else
  fail_config 'BASE_URL is required for the baseline'
fi
for run in $(seq 1 "$RUNS"); do
  check_deadline
  PRESSURE_POD=''
  DRAINED_NODE=''
  started=$(start_ms)
  scenario_status=0
  case "$SCENARIO" in
    api-restart)
      kubectl -n "$NAMESPACE" rollout restart deployment/pong-api || scenario_status=1
      [[ "$scenario_status" -ne 0 ]] || wait_ready pong-api || scenario_status=1
      ;;
    gateway-restart)
      kubectl -n "$NAMESPACE" rollout restart deployment/pong-gateway || scenario_status=1
      [[ "$scenario_status" -ne 0 ]] || wait_ready pong-gateway || scenario_status=1
      ;;
    room-termination)
      if [[ -n "${BASE_URL:-}" ]]; then
        curl --fail --silent --show-error --max-time 10 -X POST "$BASE_URL/api/rooms/create" \
          -H 'content-type: application/json' -d '{"name":"chaos-drill"}' >/dev/null || scenario_status=1
        sleep 2
      else
        scenario_status=1
      fi
      room=$(kubectl -n "$NAMESPACE" get pod -l role=room -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
      if [[ -z "$room" ]]; then
        scenario_status=1
      else
        kubectl -n "$NAMESPACE" delete pod "$room" --wait=false || scenario_status=1
        [[ "$scenario_status" -ne 0 ]] || timeout "${DRILL_TIMEOUT_SECONDS}s" kubectl -n "$NAMESPACE" wait --for=delete "pod/$room" >/dev/null || scenario_status=1
        [[ "$scenario_status" -ne 0 ]] || wait_health || scenario_status=1
      fi
      ;;
    node-drain)
      DRAINED_NODE=$(kubectl get nodes -l k3d.io/nodefilter='agent:0' -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
      if [[ -z "$DRAINED_NODE" ]]; then scenario_status=1; else
        kubectl drain "$DRAINED_NODE" --ignore-daemonsets --delete-emptydir-data --force --timeout="${DRILL_TIMEOUT_SECONDS}s" || scenario_status=1
        kubectl uncordon "$DRAINED_NODE" || scenario_status=1
        DRAINED_NODE=''
        [[ "$scenario_status" -ne 0 ]] || wait_health || scenario_status=1
      fi
      ;;
    resource-pressure)
      PRESSURE_POD="chaos-pressure-$run"
      kubectl -n "$NAMESPACE" run "$PRESSURE_POD" --image=busybox:1.36 --restart=Never \
        --limits='cpu=100m,memory=32Mi' --requests='cpu=50m,memory=16Mi' \
        -- sh -c 'i=0; while [ "$i" -lt 600 ]; do i=$((i+1)); :; done; sleep 20' || scenario_status=1
      if [[ "$scenario_status" -eq 0 ]]; then
        sleep 2
        kubectl -n "$NAMESPACE" delete pod "$PRESSURE_POD" --wait=false || scenario_status=1
        PRESSURE_POD=''
        [[ "$scenario_status" -ne 0 ]] || wait_health || scenario_status=1
      fi
      ;;
  esac
  if [[ "$scenario_status" -eq 0 && "$SCENARIO" != room-termination ]]; then
    wait_health || scenario_status=1
  fi
  ended=$(start_ms)
  recovery=$((ended - started))
  samples+=("$recovery")
  printf '%s\n' "run=$run recovery_ms=$recovery status=$scenario_status" >> "$ARTIFACT_DIR/drill-samples.txt"
  if [[ "$scenario_status" -ne 0 ]]; then record_failure; fi
  if [[ "$recovery" -gt "$RECOVERY_TARGET_MS" ]]; then record_failure; fi
  if [[ "$scenario_status" -ne 0 ]]; then cleanup_failures=$((cleanup_failures + 1)); fi
  if [[ "$failures" -ge "$ABORT_THRESHOLD" ]]; then
    printf '%s\n' aborted > "$ARTIFACT_DIR/abort-status"
    break
  fi
  [[ "$scenario_status" -eq 0 ]] || true
  sleep 1
done

sorted=$(printf '%s\n' "${samples[@]}" | sort -n)
p95=$(printf '%s\n' "$sorted" | awk -v n="$RUNS" 'NR==int(n*0.95+0.999999) {print; found=1} END {if (!found) print 0}')
passed=false
[[ "$failures" -eq 0 && "$cleanup_failures" -eq 0 && "$p95" -lt "$RECOVERY_TARGET_MS" ]] && passed=true
cat > "$ARTIFACT_DIR/chaos-result.json" <<EOF
{
  "mode": "chaos",
  "scenario": "$SCENARIO",
  "runs": ${#samples[@]},
  "requested_runs": $RUNS,
  "abort_threshold": $ABORT_THRESHOLD,
  "hard_deadline_seconds": $EXPERIMENT_DEADLINE_SECONDS,
  "recovery_target_ms": $RECOVERY_TARGET_MS,
  "recovery_p95_ms": $p95,
  "objective": "P95 recovery under six minutes",
  "objective_passed": $passed,
  "failures": $failures,
  "cleanup_verified": true,
  "aggregate_only": true
}
EOF
[[ "$passed" == true ]]
