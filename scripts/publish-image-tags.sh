#!/usr/bin/env bash
# Record a successfully published source SHA in both production overlays.
# This script hard-resets its CI checkout on every retry; it refuses to run in
# an ordinary local shell unless the integration-test guard is explicit.
set -euo pipefail

if [[ "${GITHUB_ACTIONS:-}" != "true" && "${PUBLISH_IMAGE_TAGS_TESTING:-}" != "true" ]]; then
  echo 'publish-image-tags.sh may only run in GitHub Actions or its isolated integration test' >&2
  exit 2
fi

source_sha=${GITHUB_SHA:-}
branch=${GITHUB_REF_NAME:-}
ref=${GITHUB_REF:-}
max_attempts=${PUBLISH_MAX_ATTEMPTS:-5}
retry_delay=${PUBLISH_RETRY_DELAY_SECONDS:-2}
before_push_delay=${PUBLISH_BEFORE_PUSH_DELAY_SECONDS:-0}
ready_file=${PUBLISH_TEST_READY_FILE:-}

if [[ ! "$source_sha" =~ ^[0-9a-f]{40}$ ]]; then
  echo 'GITHUB_SHA must be a full lowercase Git commit SHA' >&2
  exit 2
fi
if [[ "$branch" != main && "$branch" != feat/* ]] ||
  ! git check-ref-format --branch "$branch" >/dev/null 2>&1; then
  echo 'GITHUB_REF_NAME must be main or a valid feat/* branch' >&2
  exit 2
fi
if [[ -n "$ref" && "$ref" != "refs/heads/${branch}" ]]; then
  echo 'GITHUB_REF must identify the same branch as GITHUB_REF_NAME' >&2
  exit 2
fi
if [[ ! "$max_attempts" =~ ^[1-9]$|^10$ ]]; then
  echo 'PUBLISH_MAX_ATTEMPTS must be between 1 and 10' >&2
  exit 2
fi
if [[ ! "$retry_delay" =~ ^[0-9]+$ ]] || (( retry_delay > 30 )); then
  echo 'PUBLISH_RETRY_DELAY_SECONDS must be between 0 and 30' >&2
  exit 2
fi
if [[ ! "$before_push_delay" =~ ^[0-9]+$ ]] || (( before_push_delay > 30 )); then
  echo 'PUBLISH_BEFORE_PUSH_DELAY_SECONDS must be between 0 and 30' >&2
  exit 2
fi
if ! git diff --quiet || ! git diff --cached --quiet; then
  echo 'refusing to discard changes in the publisher checkout' >&2
  exit 2
fi

remote_ref="refs/remotes/origin/${branch}"
tag="sha-${source_sha}"

git config user.name 'github-actions[bot]'
git config user.email '41898282+github-actions[bot]@users.noreply.github.com'

path_requires_publication() {
  case "$1" in
    Dockerfile.*|.dockerignore|gateway/Caddyfile|static/Caddyfile|go.mod|go.sum|*.go|static/*|gateway/*|k8s/overlays/server/*|k8s/overlays/native-production/*|.github/workflows/publish-images.yml|scripts/publish-image-tags.sh|scripts/update-image-tag.sh|scripts/validate-release.py)
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

is_generated_publication_commit() {
  local commit=$1
  local subject changed_path generated_tag content
  local paths=0
  subject=$(git show -s --format=%s "$commit")
  [[ "$subject" =~ ^deploy:\ publish\ images\ (sha-[0-9a-f]{40})$ ]] ||
    return 1
  generated_tag=${BASH_REMATCH[1]}
  while IFS= read -r changed_path; do
    [[ -n "$changed_path" ]] || continue
    case "$changed_path" in
      k8s/overlays/server/kustomization.yaml|\
      k8s/overlays/server/api-production.yaml|\
      k8s/overlays/server/room-template.yaml|\
      k8s/overlays/native-production/kustomization.yaml|\
      k8s/overlays/native-production/api-native-production.yaml|\
      k8s/overlays/native-production/room-template.yaml)
        paths=$((paths + 1))
        ;;
      *)
        return 1
        ;;
    esac
  done < <(git diff-tree --no-commit-id --name-only -r "$commit")
  (( paths == 6 )) || return 1

  # Do not trust the subject alone: a source commit could imitate the
  # publisher's conventional message and otherwise hide a publishable change.
  # Every expected file must contain the exact tag recorded by the subject.
  for changed_path in \
    k8s/overlays/server/kustomization.yaml \
    k8s/overlays/server/api-production.yaml \
    k8s/overlays/server/room-template.yaml \
    k8s/overlays/native-production/kustomization.yaml \
    k8s/overlays/native-production/api-native-production.yaml \
    k8s/overlays/native-production/room-template.yaml; do
    content=$(git show "$commit:$changed_path") || return 1
    [[ "$content" == *"$generated_tag"* ]] || return 1
  done
  return 0
}

newer_source_requires_publication() {
  local commit parent changed_path
  while IFS= read -r commit; do
    [[ -n "$commit" ]] || continue
    if is_generated_publication_commit "$commit"; then
      continue
    fi
    parent=$(git rev-parse "${commit}^1")
    while IFS= read -r changed_path; do
      if path_requires_publication "$changed_path"; then
        return 0
      fi
    done < <(git diff --name-only "$parent" "$commit")
  done < <(git rev-list --first-parent --reverse "$source_sha..$remote_ref")
  return 1
}

for ((attempt = 1; attempt <= max_attempts; attempt++)); do
  echo "Publishing deployment reference (attempt ${attempt}/${max_attempts})"
  if git fetch --no-tags origin \
    "+refs/heads/${branch}:${remote_ref}"; then
    :
  else
    status=$?
    if (( attempt == max_attempts )); then
      exit "$status"
    fi
    sleep "$((attempt * retry_delay))"
    continue
  fi
  git reset --hard "$remote_ref"

  if ! git cat-file -e "${source_sha}^{commit}" ||
    ! git merge-base --is-ancestor "$source_sha" "$remote_ref"; then
    echo "source ${source_sha} is no longer on ${branch}; refusing a stale deployment" >&2
    exit 0
  fi

  if [[ "$source_sha" != "$(git rev-parse "$remote_ref")" ]]; then
    if newer_source_requires_publication; then
      echo "a newer publishable source supersedes ${source_sha}; leaving publication to its run"
      exit 0
    fi
    echo "newer commits contain no image-build changes; retaining ${tag}"
  fi

  ./scripts/update-image-tag.sh "$tag"
  # Verify the exact tree that will be committed. Generated deployment commits
  # are always strict even when source-push CI is in pending-publication mode.
  python3 scripts/validate-release.py
  git diff --check
  git add k8s/overlays/server k8s/overlays/native-production
  if git diff --cached --quiet; then
    echo "deployment references already point at ${tag}"
    exit 0
  fi
  git commit -m "deploy: publish images ${tag}"
  if [[ -n "$ready_file" ]]; then
    : > "$ready_file"
  fi
  if (( before_push_delay > 0 )); then
    sleep "$before_push_delay"
  fi
  if git push origin "HEAD:refs/heads/${branch}"; then
    exit 0
  fi

  echo 'branch advanced while publishing; retrying from its latest head' >&2
  if (( attempt < max_attempts )); then
    sleep "$((attempt * retry_delay))"
  fi
done

echo "could not publish deployment references after ${max_attempts} attempts" >&2
exit 1
