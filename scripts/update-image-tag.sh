#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 || ! "$1" =~ ^sha-[0-9a-f]{40}$ ]]; then
  echo "usage: $0 sha-<40-hex-commit>" >&2
  exit 2
fi

tag="$1"
for file in \
  k8s/overlays/server/kustomization.yaml \
  k8s/overlays/server/api-production.yaml \
  k8s/overlays/server/room-template.yaml \
  k8s/overlays/native-staging/kustomization.yaml \
  k8s/overlays/native-staging/api-native-staging.yaml \
  k8s/overlays/native-staging/room-template.yaml; do
  sed -i -E "s/(sha-)?REPLACE_ME|sha-[0-9a-f]{40}/$tag/g" "$file"
done
