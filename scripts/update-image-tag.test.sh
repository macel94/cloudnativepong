#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

mkdir -p "$tmp/k8s/overlays/server" "$tmp/k8s/overlays/native-staging"
for overlay in server native-staging; do
  cat > "$tmp/k8s/overlays/$overlay/kustomization.yaml" <<'EOF'
images:
  - newTag: sha-0000000000000000000000000000000000000000
  - newTag: sha-0000000000000000000000000000000000000000
  - newTag: sha-0000000000000000000000000000000000000000
  - newTag: sha-0000000000000000000000000000000000000000
EOF
  printf 'image: sha-0000000000000000000000000000000000000000\n' > "$tmp/k8s/overlays/$overlay/api-production.yaml"
  printf 'image: sha-0000000000000000000000000000000000000000\n' > "$tmp/k8s/overlays/$overlay/api-native-staging.yaml"
  printf 'image: sha-0000000000000000000000000000000000000000\n' > "$tmp/k8s/overlays/$overlay/room-template.yaml"
done

# Exercise the exact file set used by the production script without changing
# the checkout: its paths are relative to the application root.
cp "$root/scripts/update-image-tag.sh" "$tmp/update-image-tag.sh"
perl -0pi -e 's#k8s/overlays/#k8s/overlays/#g' "$tmp/update-image-tag.sh"

# Also cover the current digest-pinned release form: source publication first
# restores the SHA tag, after which the publisher may resolve registry digests.
for overlay in server native-staging; do
  sed -i -E \
    -e 's/newTag: sha-[0-9a-f]{40}/digest: sha256:0000000000000000000000000000000000000000000000000000000000000000/g' \
    -e 's/image: sha-[0-9a-f]{40}/image: ghcr.io\/macel94\/cloudnativepong-room@sha256:0000000000000000000000000000000000000000000000000000000000000000/g' \
    "$tmp/k8s/overlays/$overlay/kustomization.yaml" \
    "$tmp/k8s/overlays/$overlay/api-production.yaml" \
    "$tmp/k8s/overlays/$overlay/api-native-staging.yaml" \
    "$tmp/k8s/overlays/$overlay/room-template.yaml"
done
(
  cd "$tmp"
  bash ./update-image-tag.sh sha-2222222222222222222222222222222222222222
)
for file in \
  "$tmp/k8s/overlays/server/kustomization.yaml" \
  "$tmp/k8s/overlays/server/api-production.yaml" \
  "$tmp/k8s/overlays/server/room-template.yaml" \
  "$tmp/k8s/overlays/native-staging/kustomization.yaml" \
  "$tmp/k8s/overlays/native-staging/api-native-staging.yaml" \
  "$tmp/k8s/overlays/native-staging/room-template.yaml"; do
  ! grep -q 'sha256:' "$file"
  grep -q 'sha-2222222222222222222222222222222222222222' "$file"
done

(
  cd "$tmp"
  bash ./update-image-tag.sh sha-1111111111111111111111111111111111111111
)

for file in \
  "$tmp/k8s/overlays/server/kustomization.yaml" \
  "$tmp/k8s/overlays/server/api-production.yaml" \
  "$tmp/k8s/overlays/server/room-template.yaml" \
  "$tmp/k8s/overlays/native-staging/kustomization.yaml" \
  "$tmp/k8s/overlays/native-staging/api-native-staging.yaml" \
  "$tmp/k8s/overlays/native-staging/room-template.yaml"; do
  grep -q 'sha-1111111111111111111111111111111111111111' "$file"
done

echo 'update-image-tag updates native and server overlays'
