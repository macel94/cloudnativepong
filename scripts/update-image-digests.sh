#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 4 ]]; then
  echo 'usage: update-image-digests.sh <api-digest> <room-digest> <static-digest> <gateway-digest>' >&2
  exit 2
fi
for digest in "$@"; do
  [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || {
    echo 'each image digest must be sha256:<64 lowercase hex>' >&2
    exit 2
  }
done

python3 - "$@" <<'PY'
import re
import sys
from pathlib import Path

components = ("api", "room", "static", "gateway")
digests = dict(zip(components, sys.argv[1:], strict=True))
root = Path.cwd()
for overlay, api_name in (("server", "api-production.yaml"), ("native-staging", "api-native-staging.yaml")):
    directory = root / "k8s" / "overlays" / overlay
    kustomization = directory / "kustomization.yaml"
    text = kustomization.read_text()
    for component, digest in digests.items():
        image = f"ghcr.io/macel94/cloudnativepong-{component}"
        pattern = rf"(newName:\s*{re.escape(image)}\n\s+)(?:newTag|digest):[^\n]*"
        text, count = re.subn(pattern, rf"\1digest: {digest}", text)
        if count != 1:
            raise SystemExit(f"expected one {component} image entry in {kustomization}, found {count}")
    kustomization.write_text(text)

    room_ref = f"ghcr.io/macel94/cloudnativepong-room@{digests['room']}"
    room_path = directory / "room-template.yaml"
    text = room_path.read_text()
    text, count = re.subn(
        r"ghcr\.io/macel94/cloudnativepong-room(?:@sha256:[0-9a-f]{64}|:sha-[0-9a-f]{40})",
        room_ref,
        text,
    )
    if count != 1:
        raise SystemExit(f"expected one room image in {room_path}, found {count}")
    room_path.write_text(text)

    api_path = directory / api_name
    text = api_path.read_text()
    text, count = re.subn(
        r"--room-image=ghcr\.io/macel94/cloudnativepong-room(?:@sha256:[0-9a-f]{64}|:sha-[0-9a-f]{40})",
        f"--room-image={room_ref}",
        text,
    )
    if count != 1:
        raise SystemExit(f"expected one room image argument in {api_path}, found {count}")
    api_path.write_text(text)
PY

if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  git diff --check
fi
