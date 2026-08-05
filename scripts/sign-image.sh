#!/usr/bin/env bash
# Optional Sigstore keyless signing hook. The image must already be published
# and addressed by digest; tags are mutable and are intentionally rejected.
set -euo pipefail

usage() {
  echo "usage: $0 IMAGE@sha256:<64-hex-digest> [--dry-run]" >&2
  exit 2
}

image="${1:-}"
dry_run=0
if [[ "${2:-}" == --dry-run ]]; then dry_run=1; elif [[ $# -gt 1 ]]; then usage; fi
[[ "$image" =~ @sha256:[0-9a-fA-F]{64}$ ]] || {
  echo 'sign-image requires an immutable IMAGE@sha256:<digest> reference' >&2
  exit 2
}
if ((dry_run)); then
  echo "would run: cosign sign --yes $image"
  echo 'requires an OIDC identity (GitHub Actions id-token: write or local browser login)'
  exit 0
fi
command -v cosign >/dev/null 2>&1 || { echo 'cosign is required' >&2; exit 127; }
exec cosign sign --yes "$image"
