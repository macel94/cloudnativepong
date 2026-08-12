#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

mkdir -p "$tmp/k8s/overlays/server" "$tmp/k8s/overlays/native-production"
for overlay in server native-production; do
  cat > "$tmp/k8s/overlays/$overlay/kustomization.yaml" <<'EOF'
images:
  - newTag: sha-0000000000000000000000000000000000000000
  - newTag: sha-0000000000000000000000000000000000000000
  - newTag: sha-0000000000000000000000000000000000000000
  - newTag: sha-0000000000000000000000000000000000000000
EOF
  printf 'image: sha-0000000000000000000000000000000000000000\n' > "$tmp/k8s/overlays/$overlay/api-production.yaml"
  printf 'image: sha-0000000000000000000000000000000000000000\n' > "$tmp/k8s/overlays/$overlay/api-native-production.yaml"
  printf 'image: sha-0000000000000000000000000000000000000000\n' > "$tmp/k8s/overlays/$overlay/room-template.yaml"
done

# Exercise the exact file set used by the production script without changing
# the checkout: its paths are relative to the application root.
cp "$root/scripts/update-image-tag.sh" "$tmp/update-image-tag.sh"
perl -0pi -e 's#k8s/overlays/#k8s/overlays/#g' "$tmp/update-image-tag.sh"
(
  cd "$tmp"
  bash ./update-image-tag.sh sha-1111111111111111111111111111111111111111
)

for file in \
  "$tmp/k8s/overlays/server/kustomization.yaml" \
  "$tmp/k8s/overlays/server/api-production.yaml" \
  "$tmp/k8s/overlays/server/room-template.yaml" \
  "$tmp/k8s/overlays/native-production/kustomization.yaml" \
  "$tmp/k8s/overlays/native-production/api-native-production.yaml" \
  "$tmp/k8s/overlays/native-production/room-template.yaml"; do
  grep -q 'sha-1111111111111111111111111111111111111111' "$file"
done

echo 'update-image-tag updates native and server overlays'
