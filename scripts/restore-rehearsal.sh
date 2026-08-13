#!/usr/bin/env bash
# Non-destructive, opt-in SQLite restore rehearsal for an isolated disposable cluster.
#
# This script intentionally refuses native production and never
# deletes a cluster or PVC except the exact disposable cluster it created.
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -- "$SCRIPT_DIR/.." && pwd)
BACKUP_HELPER="$SCRIPT_DIR/backup-restore.sh"

usage() {
  cat <<'USAGE'
Usage:
  restore-rehearsal.sh self-test
  restore-rehearsal.sh --backup FILE [options]

Required acknowledgement:
  --i-understand-this-creates-an-isolated-cluster

Options:
  --backup FILE       Verified SQLite backup artifact to copy into the new PVC
  --cluster NAME      Disposable k3d name (must start with pong-restore-)
  --namespace NAME    Namespace inside the disposable cluster (default: pong)
  --host-port PORT    Bind the gateway NodePort to localhost:PORT (default: auto)
  --helper-image IMG  Image with sleep, tar, and sha256sum (default: busybox:1.36.1)
  --build-images      Build the four local images before importing them
  --keep-cluster      Leave the rehearsal cluster for inspection after success
  --dry-run           Print guarded commands; do not create a cluster or copy data
  --i-understand-this-creates-an-isolated-cluster
                      Explicit opt-in; required for a real run
  -h, --help          Show this help

The source backup is never modified. The copied database is seeded into a new
PVC in the disposable cluster, and the API is checked through its gateway.
This script does not upload to object storage and never restores /data/pong.db
in native production.
USAGE
}

fail() {
  echo "restore rehearsal: $*" >&2
  exit 1
}

log() {
  printf '[restore-rehearsal] %s\n' "$*"
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "required command not found: $1"
}

is_forbidden_cluster() {
  local name=$1
  [[ "$name" == "pong" || "$name" == "native-production" || "$name" == "belacca-native" ]]
}

quote_cmd() {
  printf '%q ' "$@"
  printf '\n'
}

self_test() {
  local status=0
  [[ -x "$BACKUP_HELPER" ]] || { echo "backup helper is not executable: $BACKUP_HELPER" >&2; status=1; }
  grep -q -- '--i-understand-this-creates-an-isolated-cluster' "$0" || { echo 'missing explicit opt-in guard' >&2; status=1; }
  grep -q -- 'pong-restore-' "$0" || { echo 'missing dedicated cluster prefix guard' >&2; status=1; }
  grep -q -- 'belacca-native' "$0" || { echo 'missing native production guard' >&2; status=1; }
  is_forbidden_cluster belacca-native || { echo 'native production predicate is not refusing belacca-native' >&2; status=1; }
  is_forbidden_cluster pong || { echo 'production cluster predicate is not refusing pong' >&2; status=1; }
  is_forbidden_cluster pong-restore-self-test && { echo 'restore cluster predicate rejects its own dedicated prefix' >&2; status=1; }
  grep -q -- '../../base' "$REPO_ROOT/k8s/overlays/test/kustomization.yaml" || { echo 'test overlay no longer references the application base' >&2; status=1; }
  grep -q -- 'pong-api-data' "$REPO_ROOT/k8s/base/all.yaml" || { echo 'application base no longer names the expected PVC' >&2; status=1; }
  if grep -nE 'k3d cluster delete (pong|native-production|belacca-native)|kubectl .*delete (pvc|namespace)' "$0"; then
    echo 'destructive production/PVC command found in rehearsal script' >&2
    status=1
  fi
  if (( status == 0 )); then
    echo 'restore rehearsal self-test passed: opt-in, prefix, production, and PVC guards present'
  fi
  return "$status"
}

backup_file=''
cluster_name="pong-restore-$(date -u +%Y%m%d%H%M%S)-$$"
namespace='pong'
host_port=''
helper_image='busybox:1.36.1'
build_images=0
keep_cluster=0
dry_run=0
acknowledged=0

if [[ "${1:-}" == self-test ]]; then
  self_test
  exit $?
fi

while (($#)); do
  case "$1" in
    --backup)
      (($# >= 2)) || fail '--backup requires a path'
      backup_file=$2
      shift 2
      ;;
    --cluster)
      (($# >= 2)) || fail '--cluster requires a name'
      cluster_name=$2
      shift 2
      ;;
    --namespace)
      (($# >= 2)) || fail '--namespace requires a name'
      namespace=$2
      shift 2
      ;;
    --host-port)
      (($# >= 2)) || fail '--host-port requires a port'
      host_port=$2
      shift 2
      ;;
    --helper-image)
      (($# >= 2)) || fail '--helper-image requires an image'
      helper_image=$2
      shift 2
      ;;
    --build-images)
      build_images=1
      shift
      ;;
    --keep-cluster)
      keep_cluster=1
      shift
      ;;
    --dry-run)
      dry_run=1
      shift
      ;;
    --i-understand-this-creates-an-isolated-cluster)
      acknowledged=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      usage >&2
      fail "unknown argument: $1"
      ;;
  esac
done

[[ -n "$backup_file" ]] || { usage >&2; fail '--backup is required'; }
[[ "$namespace" == pong ]] || fail 'namespace must remain pong; the checked-in manifest and PVC contract are fixed to pong'
[[ "$cluster_name" =~ ^pong-restore-[a-z0-9][a-z0-9-]*$ ]] || fail 'cluster must match pong-restore-<unique-name>'
[[ "$helper_image" != *[[:space:]]* ]] || fail 'helper image must not contain whitespace'
! is_forbidden_cluster "$cluster_name" || fail "refusing cluster name that could target production: $cluster_name"
[[ "$backup_file" != /data/pong.db ]] || fail 'refusing the live database path /data/pong.db'
[[ "$backup_file" != *belacca-native* ]] || fail 'refusing a backup path that names native production; copy it to a neutral protected path first'

if (( dry_run == 0 && acknowledged == 0 )); then
  fail 'real runs require --i-understand-this-creates-an-isolated-cluster'
fi

if (( dry_run == 0 )); then
  require_command k3d
  require_command kubectl
  require_command curl
  require_command python3
  require_command sha256sum
  [[ -x "$BACKUP_HELPER" ]] || fail "backup helper is not executable: $BACKUP_HELPER"
  [[ -f "$backup_file" ]] || fail "backup file does not exist: $backup_file"
  [[ "$backup_file" != "$REPO_ROOT"/* ]] || log 'backup is inside the repository; do not commit it and ensure it is gitignored'
fi

if [[ -z "$host_port" ]]; then
  if (( dry_run )); then
    host_port=18080
  else
    host_port=$(python3 - <<'PY'
import socket
with socket.socket() as sock:
    sock.bind(('127.0.0.1', 0))
    print(sock.getsockname()[1])
PY
)
  fi
fi
[[ "$host_port" =~ ^[0-9]+$ && "$host_port" -ge 1024 && "$host_port" -le 65535 ]] || fail "invalid host port: $host_port"

if (( dry_run )); then
  log 'dry-run: no k3d, kubectl, filesystem, or backup operations will run'
  quote_cmd "$BACKUP_HELPER" verify "$backup_file"
  quote_cmd k3d cluster create "$cluster_name" --agents 1 --wait --port "127.0.0.1:${host_port}:30080@agent:0"
  quote_cmd kubectl --context "k3d-${cluster_name}" apply -k "$REPO_ROOT/k8s/overlays/test"
  quote_cmd kubectl --context "k3d-${cluster_name}" -n "$namespace" scale deployment/pong-api --replicas=0
  quote_cmd kubectl --context "k3d-${cluster_name}" -n "$namespace" cp "$backup_file" pong-restore-seed:/data/pong.db
  quote_cmd kubectl --context "k3d-${cluster_name}" -n "$namespace" scale deployment/pong-api --replicas=1
  log "dry-run complete for disposable cluster $cluster_name; native production is not addressed"
  exit 0
fi

if k3d cluster list --no-headers 2>/dev/null | awk '{print $1}' | grep -Fxq "$cluster_name"; then
  fail "cluster already exists; refusing to attach or delete it: $cluster_name"
fi

# Verify before any cluster operation. The helper only reads the source backup.
"$BACKUP_HELPER" verify "$backup_file"
source_sha=$(sha256sum "$backup_file" | awk '{print $1}')
created=0
context="k3d-${cluster_name}"
cleanup() {
  local exit_code=$?
  if (( keep_cluster == 0 && created == 1 )); then
    log "cleaning only the newly created disposable cluster: $cluster_name"
    k3d cluster delete "$cluster_name" >/dev/null 2>&1 || log "cleanup failed; inspect $context manually"
  elif (( created == 1 )); then
    log "keeping disposable cluster $cluster_name for inspection; delete only with: k3d cluster delete $cluster_name"
  fi
  exit "$exit_code"
}
trap cleanup EXIT INT TERM

log "creating isolated cluster $cluster_name on localhost:$host_port"
# Claim ownership only after k3d reports success. If creation is interrupted,
# fail closed and leave the exact name for an operator to inspect rather than
# risking deletion of a cluster created by another process.
k3d cluster create "$cluster_name" --agents 1 --wait --port "127.0.0.1:${host_port}:30080@agent:0"
created=1
kubectl --context "$context" wait --for=condition=Ready node --all --timeout=120s

if (( build_images == 1 )); then
  require_command docker
  log 'building local images explicitly requested by operator'
  docker build -t cloudnativepong-api:latest -f "$REPO_ROOT/Dockerfile.api" "$REPO_ROOT"
  docker build -t cloudnativepong-room:latest -f "$REPO_ROOT/Dockerfile.room" "$REPO_ROOT"
  docker build -t cloudnativepong-static:latest -f "$REPO_ROOT/Dockerfile.static" "$REPO_ROOT"
  docker build -t cloudnativepong-gateway:latest -f "$REPO_ROOT/Dockerfile.gateway" "$REPO_ROOT"
fi

log 'importing application images; use --build-images if they are not already local'
require_command docker
for image in cloudnativepong-api:latest cloudnativepong-room:latest cloudnativepong-static:latest cloudnativepong-gateway:latest; do
  docker image inspect "$image" >/dev/null 2>&1 || fail "local image missing: $image (build them or use --build-images)"
done
docker images --format '{{.Repository}}:{{.Tag}}' | grep -E '^cloudnativepong-(api|room|static|gateway):latest$' >/dev/null
k3d image import cloudnativepong-api:latest cloudnativepong-room:latest cloudnativepong-static:latest cloudnativepong-gateway:latest --cluster "$cluster_name"

log 'applying the checked-in application manifest only to the isolated context'
kubectl --context "$context" apply -k "$REPO_ROOT/k8s/overlays/test"
kubectl --context "$context" -n "$namespace" wait --for=condition=available deployment/pong-api --timeout=180s
kubectl --context "$context" -n "$namespace" scale deployment/pong-api --replicas=0
kubectl --context "$context" -n "$namespace" wait --for=delete pod -l 'app=cloudnativepong,component=api' --timeout=120s

cat <<POD | kubectl --context "$context" apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: pong-restore-seed
  namespace: $namespace
  labels:
    app: cloudnativepong-restore
    component: seed
spec:
  restartPolicy: Never
  containers:
    - name: seed
      image: $helper_image
      imagePullPolicy: IfNotPresent
      command: ["sh", "-c", "sleep 3600"]
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop: ["ALL"]
        seccompProfile:
          type: RuntimeDefault
        readOnlyRootFilesystem: false
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: pong-api-data
POD
kubectl --context "$context" -n "$namespace" wait --for=jsonpath='{.status.phase}'=Running pod/pong-restore-seed --timeout=180s
kubectl --context "$context" -n "$namespace" cp "$backup_file" pong-restore-seed:/data/pong.db
copied_sha=$(kubectl --context "$context" -n "$namespace" exec pong-restore-seed -- sha256sum /data/pong.db | awk '{print $1}')
[[ "$copied_sha" == "$source_sha" ]] || fail "PVC copy hash mismatch: source=$source_sha copied=$copied_sha"
kubectl --context "$context" -n "$namespace" delete pod/pong-restore-seed --wait=true --timeout=120s

log 'starting the restored API and waiting for the application contract'
kubectl --context "$context" -n "$namespace" scale deployment/pong-api --replicas=1
kubectl --context "$context" -n "$namespace" rollout status deployment/pong-api --timeout=180s
kubectl --context "$context" -n "$namespace" rollout status deployment/pong-gateway --timeout=180s
kubectl --context "$context" -n "$namespace" rollout status deployment/pong-static --timeout=180s
curl --fail --silent --show-error --max-time 10 "http://127.0.0.1:${host_port}/health" >/dev/null
curl --fail --silent --show-error --max-time 10 "http://127.0.0.1:${host_port}/api/rooms" >/tmp/pong-restore-rooms.json
log "restore rehearsal passed: copied $source_sha into $context and verified gateway/API"
log "rooms response saved at /tmp/pong-restore-rooms.json"
