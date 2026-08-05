#!/usr/bin/env bash
# Local supply-chain helpers. CI may call these with --dry-run so missing
# registries/credentials never turn an ordinary test run into a release.
set -euo pipefail

usage() {
  cat >&2 <<'EOF'
usage:
  scripts/supply-chain.sh sbom [--target PATH] [--output DIR] [--dry-run]
  scripts/supply-chain.sh scan-fs [--target PATH] [--output DIR] [--strict] [--dry-run]
  scripts/supply-chain.sh scan-image IMAGE [--output DIR] [--strict] [--dry-run]
  scripts/supply-chain.sh digest IMAGE [--dry-run]

Environment:
  SUPPLY_CHAIN_STRICT=1  fail scans on HIGH/CRITICAL findings
  TRIVY_SEVERITY=HIGH,CRITICAL (default)
  TRIVY_IGNORE_UNFIXED=true (default)
EOF
  exit 2
}

command_name="${1:-}"
shift || true
output_dir="artifacts/supply-chain"
target="."
dry_run=0
strict="${SUPPLY_CHAIN_STRICT:-0}"
image=""

while (($#)); do
  case "$1" in
    --target) target="${2:?missing value for --target}"; shift 2 ;;
    --output) output_dir="${2:?missing value for --output}"; shift 2 ;;
    --strict) strict=1; shift ;;
    --dry-run) dry_run=1; shift ;;
    --help|-h) usage ;;
    --*) echo "unknown option: $1" >&2; usage ;;
    *)
      if [[ -z "$image" ]]; then image="$1"; shift
      else echo "unexpected argument: $1" >&2; usage; fi
      ;;
  esac
done

need_tool() {
  if ((dry_run)); then
    return 0
  fi
  command -v "$1" >/dev/null 2>&1 || {
    echo "required tool not found: $1 (install it or use --dry-run to inspect the planned command)" >&2
    exit 127
  }
}

run_or_print() {
  if ((dry_run)); then
    printf '+ '
    printf '%q ' "$@"
    printf '\n'
  else
    "$@"
  fi
}

mkdir_output() {
  if ((dry_run)); then
    echo "would create output directory: $output_dir"
  else
    mkdir -p "$output_dir"
  fi
}

case "$command_name" in
  sbom)
    need_tool syft
    mkdir_output
    run_or_print syft "dir:$target" --output "spdx-json=$output_dir/source.sbom.spdx.json"
    ;;
  scan-fs)
    need_tool trivy
    mkdir_output
    exit_code=0
    [[ "$strict" == 1 ]] && exit_code=1
    run_or_print trivy fs --scanners vuln,secret,misconfig \
      --severity "${TRIVY_SEVERITY:-HIGH,CRITICAL}" \
      --ignore-unfixed="${TRIVY_IGNORE_UNFIXED:-true}" \
      --format json --output "$output_dir/filesystem.trivy.json" \
      --exit-code "$exit_code" "$target"
    ;;
  scan-image)
    [[ -n "$image" ]] || { echo "scan-image requires IMAGE" >&2; usage; }
    need_tool trivy
    mkdir_output
    exit_code=0
    [[ "$strict" == 1 ]] && exit_code=1
    safe_name="${image//[^a-zA-Z0-9_.-]/_}"
    run_or_print trivy image \
      --severity "${TRIVY_SEVERITY:-HIGH,CRITICAL}" \
      --ignore-unfixed="${TRIVY_IGNORE_UNFIXED:-true}" \
      --format json --output "$output_dir/$safe_name.trivy.json" \
      --exit-code "$exit_code" "$image"
    ;;
  digest)
    [[ -n "$image" ]] || { echo "digest requires IMAGE" >&2; usage; }
    need_tool podman
    if ((dry_run)); then
      echo "would inspect immutable digest for: $image"
    else
      podman image inspect --format '{{.Id}}' "$image"
      if command -v skopeo >/dev/null 2>&1; then
        skopeo inspect --format '{{.Digest}}' "docker://$image"
      fi
    fi
    ;;
  *) usage ;;
esac
