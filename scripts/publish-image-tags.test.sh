#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

setup_repo() {
  local name=$1
  local dir="$tmp/$name"
  mkdir -p "$dir/seed"
  cp -R "$root/k8s" "$dir/seed/k8s"
  cp -R "$root/scripts" "$dir/seed/scripts"
  cp "$root/release-metadata.json" "$dir/seed/release-metadata.json"
  cp "$root/Dockerfile.api" "$dir/seed/Dockerfile.api"

  git init --bare "$dir/origin.git" >/dev/null
  git -C "$dir/seed" init -b main >/dev/null
  git -C "$dir/seed" config user.name test
  git -C "$dir/seed" config user.email test@example.invalid
  git -C "$dir/seed" add .
  git -C "$dir/seed" commit -m 'source A' >/dev/null
  git -C "$dir/seed" remote add origin "$dir/origin.git"
  git -C "$dir/seed" push -u origin main >/dev/null
  git clone -q -b main "$dir/origin.git" "$dir/publisher"
  git clone -q -b main "$dir/origin.git" "$dir/updater"
  git -C "$dir/updater" config user.name test
  git -C "$dir/updater" config user.email test@example.invalid
  printf '%s\n' "$dir"
}

run_publisher() {
  local dir=$1 sha=$2 log=$3 ready=$4 delay=$5
  (
    cd "$dir/publisher"
    GITHUB_ACTIONS=true PUBLISH_IMAGE_TAGS_TESTING=true \
      GITHUB_SHA="$sha" GITHUB_REF_NAME=main GITHUB_REF=refs/heads/main \
      PUBLISH_MAX_ATTEMPTS=5 PUBLISH_RETRY_DELAY_SECONDS=0 \
      PUBLISH_BEFORE_PUSH_DELAY_SECONDS="$delay" \
      PUBLISH_TEST_READY_FILE="$ready" \
      ./scripts/publish-image-tags.sh >"$log" 2>&1
  ) &
  publisher_pid=$!
}

# Basic isolated publication proves the script can create the synchronized
# deployment commit without a GitHub runner or network access.
basic=$(setup_repo basic)
basic_sha=$(git -C "$basic/seed" rev-parse HEAD)
(
  cd "$basic/publisher"
  GITHUB_ACTIONS=true PUBLISH_IMAGE_TAGS_TESTING=true \
    GITHUB_SHA="$basic_sha" GITHUB_REF_NAME=main GITHUB_REF=refs/heads/main \
    PUBLISH_RETRY_DELAY_SECONDS=0 ./scripts/publish-image-tags.sh >/dev/null
)
[[ "$(git --git-dir "$basic/origin.git" log main -1 --format=%s)" == \
deploy:\ publish\ images\ sha-"$basic_sha" ]] || {
  echo 'basic publisher test did not record the source SHA' >&2
  exit 1
}
for overlay in server native-production; do
  git --git-dir "$basic/origin.git" show "main:k8s/overlays/$overlay/kustomization.yaml" \
    | grep -q "sha-$basic_sha"
done

# A non-image source commit racing with publication must be retained and the
# generated deployment commit must be retried on top of it after the NFF push.
race=$(setup_repo race)
race_sha=$(git -C "$race/seed" rev-parse HEAD)
race_ready="$race/ready"
run_publisher "$race" "$race_sha" "$race/publisher.log" "$race_ready" 1
for _ in {1..100}; do [[ -e "$race_ready" ]] && break; sleep 0.05; done
[[ -e "$race_ready" ]] || { echo 'publisher did not reach forced race point' >&2; exit 1; }
printf 'non-image source B\n' > "$race/updater/README.race"
git -C "$race/updater" add README.race
git -C "$race/updater" commit -m 'source B docs' >/dev/null
git -C "$race/updater" push origin HEAD:main >/dev/null
wait "$publisher_pid"
grep -q 'branch advanced while publishing; retrying' "$race/publisher.log"
race_tip=$(git --git-dir "$race/origin.git" rev-parse main)
race_parent=$(git --git-dir "$race/origin.git" rev-parse "$race_tip^1")
git --git-dir "$race/origin.git" show "$race_parent:README.race" >/dev/null
[[ "$(git --git-dir "$race/origin.git" show -s --format=%s "$race_tip")" == \
deploy:\ publish\ images\ sha-"$race_sha" ]]

# A newer image-affecting source commit wins the race; an older publisher must
# not create a deployment commit over it. The newer run owns publication.
newer=$(setup_repo newer)
newer_sha=$(git -C "$newer/seed" rev-parse HEAD)
newer_ready="$newer/ready"
run_publisher "$newer" "$newer_sha" "$newer/publisher.log" "$newer_ready" 1
for _ in {1..100}; do [[ -e "$newer_ready" ]] && break; sleep 0.05; done
[[ -e "$newer_ready" ]] || { echo 'publisher did not reach second race point' >&2; exit 1; }
printf '\n# source B changes the image build\n' >> "$newer/updater/Dockerfile.api"
git -C "$newer/updater" add Dockerfile.api
git -C "$newer/updater" commit -m 'source B image change' >/dev/null
git -C "$newer/updater" push origin HEAD:main >/dev/null
wait "$publisher_pid"
grep -q 'a newer publishable source supersedes' "$newer/publisher.log"
newer_subject=$(git --git-dir "$newer/origin.git" log main -1 --format=%s)
[[ "$newer_subject" == 'source B image change' ]] || {
  echo 'stale publisher overwrote a newer image source' >&2
  exit 1
}

echo 'publish-image-tags handles isolated publication and remote races safely'
