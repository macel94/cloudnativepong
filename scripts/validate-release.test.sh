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

# The checked-in release is a synchronized immutable release. Support both
# the historical SHA-tag form and the current digest-pinned form so this test
# remains valid across the publication transition.
run_validator

# CPU limits are forbidden in every generated Pong manifest. This catches a
# future Kustomize/base/room-template regression even when memory limits remain
# present and the release references are otherwise valid.
cat > "$tmp/k8s/overlays/test/cpu-limit-regression.yaml" <<'EOF'
apiVersion: v1
kind: ConfigMap
metadata:
  name: cpu-limit-regression
  namespace: pong
data:
  pod.json: |-
    {"spec":{"containers":[{"resources":{"limits":{"cpu":"200m"}}}]}}
EOF
expect_failure run_validator --allow-pending-publication
rm "$tmp/k8s/overlays/test/cpu-limit-regression.yaml"

# Pending publication allows two complete immutable overlays to differ, but it
# does not allow one overlay to be internally split or malformed.
reference_kind=$(sed -n -E 's/.*(newTag|digest): .*/\1/p' "$tmp/k8s/overlays/server/kustomization.yaml" | head -n 1)
case "$reference_kind" in
  newTag)
    old_reference=$(sed -n 's/.*newTag: //p' "$tmp/k8s/overlays/server/kustomization.yaml" | head -n 1)
    [[ "$old_reference" =~ ^sha-[0-9a-f]{40}$ ]] || {
      echo "could not find checked-in immutable tag: $old_reference" >&2
      exit 1
    }
    new_reference='sha-1111111111111111111111111111111111111111'
    ;;
  digest)
    old_api=$(sed -n 's/.*newName: ghcr.io\/macel94\/cloudnativepong-api$/x/p; s/.*digest: //p' "$tmp/k8s/overlays/server/kustomization.yaml" | tail -n 4 | head -n 1)
    old_room=$(sed -n 's/.*digest: //p' "$tmp/k8s/overlays/server/kustomization.yaml" | tail -n 3 | head -n 1)
    old_static=$(sed -n 's/.*digest: //p' "$tmp/k8s/overlays/server/kustomization.yaml" | tail -n 2 | head -n 1)
    old_gateway=$(sed -n 's/.*digest: //p' "$tmp/k8s/overlays/server/kustomization.yaml" | tail -n 1)
    for old_digest in "$old_api" "$old_room" "$old_static" "$old_gateway"; do
      [[ "$old_digest" =~ ^sha256:[0-9a-f]{64}$ ]] || {
        echo "could not find checked-in immutable digest: $old_digest" >&2
        exit 1
      }
    done
    new_api='sha256:1111111111111111111111111111111111111111111111111111111111111111'
    new_room='sha256:2222222222222222222222222222222222222222222222222222222222222222'
    new_static='sha256:3333333333333333333333333333333333333333333333333333333333333333'
    new_gateway='sha256:4444444444444444444444444444444444444444444444444444444444444444'
    old_reference="$old_api"
    new_reference="$new_api"
    ;;
  *)
    echo "unknown checked-in reference kind: $reference_kind" >&2
    exit 1
    ;;
esac
if [[ "$reference_kind" == newTag ]]; then
  (cd "$tmp" && OLD="$old_reference" NEW="$new_reference" perl -0pi -e 's/\Q$ENV{OLD}\E/$ENV{NEW}/g' \
    k8s/overlays/native-staging/kustomization.yaml \
    k8s/overlays/native-staging/api-native-staging.yaml \
    k8s/overlays/native-staging/room-template.yaml)
else
  (cd "$tmp" && sed -i \
    -e "s/$old_api/$new_api/g" \
    -e "s/$old_room/$new_room/g" \
    -e "s/$old_static/$new_static/g" \
    -e "s/$old_gateway/$new_gateway/g" \
    k8s/overlays/native-staging/kustomization.yaml && \
    sed -i "s/$old_room/$new_room/g" \
    k8s/overlays/native-staging/api-native-staging.yaml \
    k8s/overlays/native-staging/room-template.yaml)
fi
expect_failure run_validator
run_validator --allow-pending-publication

# A mismatch inside one overlay is never a valid pending state. For digest
# references, alter the room entry because the validator cross-checks it
# against the embedded room template and API argument.
if [[ "$reference_kind" == newTag ]]; then
  mismatch_old="$old_reference"
  mismatch_new="$new_reference"
else
  mismatch_old="$old_room"
  mismatch_new="$new_room"
fi
(cd "$tmp" && OLD="$mismatch_old" NEW="$mismatch_new" perl -0pi -e 's/\Q$ENV{NEW}\E/$ENV{OLD}/' \
  k8s/overlays/native-staging/kustomization.yaml)
expect_failure run_validator --allow-pending-publication
(cd "$tmp" && sed -i "s/$mismatch_old/$mismatch_new/" \
  k8s/overlays/native-staging/kustomization.yaml)

# Mutable references are rejected even with the narrow pending exception.
(cd "$tmp" && OLD="$old_reference" NEW="$new_reference" perl -0pi -e 's/\Q$ENV{NEW}\E/$ENV{OLD}/' \
  k8s/overlays/native-staging/kustomization.yaml)
if [[ "$reference_kind" == newTag ]]; then
  (cd "$tmp" && sed -i "s/newTag: ${old_reference}/newTag: latest/" \
    k8s/overlays/native-staging/kustomization.yaml)
else
  (cd "$tmp" && sed -i "s/digest: ${old_reference}/newTag: latest/" \
    k8s/overlays/native-staging/kustomization.yaml)
fi
expect_failure run_validator --allow-pending-publication
if [[ "$reference_kind" == newTag ]]; then
  (cd "$tmp" && sed -i "s/newTag: latest/newTag: $new_reference/" \
    k8s/overlays/native-staging/kustomization.yaml)
else
  (cd "$tmp" && sed -i "s/newTag: latest/digest: $new_reference/" \
    k8s/overlays/native-staging/kustomization.yaml)
fi

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
