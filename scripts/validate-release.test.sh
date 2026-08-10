#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

cp -R "$root/k8s" "$tmp/k8s"
cp "$root/release-metadata.json" "$tmp/release-metadata.json"
mkdir -p "$tmp/scripts"
cp "$root/scripts/validate-release.py" "$tmp/scripts/validate-release.py"
cp "$root/scripts/promote-digests.sh" "$tmp/scripts/promote-digests.sh"

run_validator() {
  (cd "$tmp" && python3 scripts/validate-release.py "$@")
}
expect_failure() {
  if "$@"; then
    echo "unexpected validation success: $*" >&2
    exit 1
  fi
}

# The checked-in release is a synchronized, tag-based release.
run_validator

# Pending publication allows two complete immutable overlays to differ, but it
# does not allow one overlay to be internally split or malformed.
old_tag=$(sed -n 's/.*newTag: //p' "$tmp/k8s/overlays/server/kustomization.yaml" | head -n 1)
[[ "$old_tag" =~ ^sha-[0-9a-f]{40}$ ]] || {
  echo "could not find checked-in immutable tag: $old_tag" >&2
  exit 1
}
new_tag='sha-1111111111111111111111111111111111111111'
(cd "$tmp" && sed -i "s/$old_tag/$new_tag/g" \
  k8s/overlays/native-staging/kustomization.yaml \
  k8s/overlays/native-staging/api-native-staging.yaml \
  k8s/overlays/native-staging/room-template.yaml)
expect_failure run_validator
run_validator --allow-pending-publication

# A tag mismatch inside one overlay is never a valid pending state.
(cd "$tmp" && OLD="$old_tag" NEW="$new_tag" perl -0pi -e 's/\Q$ENV{NEW}\E/$ENV{OLD}/' \
  k8s/overlays/native-staging/kustomization.yaml)
expect_failure run_validator --allow-pending-publication
(cd "$tmp" && sed -i "s/$old_tag/$new_tag/" \
  k8s/overlays/native-staging/kustomization.yaml)

# Mutable references are rejected even with the narrow pending exception.
(cd "$tmp" && OLD="$old_tag" NEW="$new_tag" perl -0pi -e 's/\Q$ENV{NEW}\E/$ENV{OLD}/' \
  k8s/overlays/native-staging/kustomization.yaml && \
  sed -i "s/newTag: ${old_tag}/newTag: latest/" \
  k8s/overlays/native-staging/kustomization.yaml)
expect_failure run_validator --allow-pending-publication
(cd "$tmp" && sed -i 's/newTag: latest/newTag: sha-1111111111111111111111111111111111111111/' \
  k8s/overlays/native-staging/kustomization.yaml)

# Digest promotion updates both overlays, API room arguments, embedded JSON,
# and release metadata as one consistent immutable state.
digest() { printf 'sha256:%064d' "$1"; }
(cd "$tmp" && bash scripts/promote-digests.sh \
  --api "ghcr.io/macel94/cloudnativepong-api@$(digest 1)" \
  --room "ghcr.io/macel94/cloudnativepong-room@$(digest 2)" \
  --static "ghcr.io/macel94/cloudnativepong-static@$(digest 3)" \
  --gateway "ghcr.io/macel94/cloudnativepong-gateway@$(digest 4)")
run_validator

# A digest mismatch between the overlays is still strict-fail, while the
# pending flag only covers that cross-overlay publication window.
(cd "$tmp" && OLD='sha256:0000000000000000000000000000000000000000000000000000000000000002' \
  NEW='sha256:9999999999999999999999999999999999999999999999999999999999999999' \
  perl -0pi -e 's/\Q$ENV{OLD}\E/$ENV{NEW}/g' \
  k8s/overlays/native-staging/kustomization.yaml \
  k8s/overlays/native-staging/api-native-staging.yaml \
  k8s/overlays/native-staging/room-template.yaml)
expect_failure run_validator
run_validator --allow-pending-publication

# The validator also rejects malformed/mutable embedded room JSON, not merely
# the Kustomize image list.
(cd "$tmp" && sed -i 's#ghcr.io/macel94/cloudnativepong-room@sha256:[0-9a-f]*#ghcr.io/macel94/cloudnativepong-room:latest#' \
  k8s/overlays/server/room-template.yaml)
expect_failure run_validator --allow-pending-publication

echo 'release validation covers synchronized tags, pending publication, digests, and embedded references'
