#!/usr/bin/env bash
# Verify a Cosign signature on an immutable image digest. Certificate identity
# is required so a valid signature from an unrelated workflow is not accepted.
set -euo pipefail

usage() {
  echo "usage: $0 IMAGE@sha256:<64-hex-digest> --certificate-identity-regexp REGEXP [--dry-run]" >&2
  exit 2
}

image="${1:-}"
shift || true
identity=''
dry_run=0
while (($#)); do
  case "$1" in
    --certificate-identity-regexp) identity="${2:?missing value}"; shift 2 ;;
    --dry-run) dry_run=1; shift ;;
    *) usage ;;
  esac
done
[[ "$image" =~ @sha256:[0-9a-fA-F]{64}$ ]] || { echo 'an IMAGE@sha256 digest is required' >&2; exit 2; }
[[ -n "$identity" ]] || { echo '--certificate-identity-regexp is required' >&2; exit 2; }
issuer="${COSIGN_OIDC_ISSUER:-https://token.actions.githubusercontent.com}"
if ((dry_run)); then
  echo "would verify signature for: $image"
  echo "would require certificate identity regexp: $identity"
  echo "would require OIDC issuer: $issuer"
  exit 0
fi
command -v cosign >/dev/null 2>&1 || { echo 'cosign is required' >&2; exit 127; }
exec cosign verify --certificate-identity-regexp "$identity" \
  --certificate-oidc-issuer "$issuer" "$image"
