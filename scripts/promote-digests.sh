#!/usr/bin/env bash
# Promote already-published, verified image digests into the production overlay.
# This is intentionally explicit and never resolves tags or signs artifacts.
set -euo pipefail

usage() {
  echo "usage: $0 --api IMAGE@sha256:DIGEST --room IMAGE@sha256:DIGEST --static IMAGE@sha256:DIGEST --gateway IMAGE@sha256:DIGEST" >&2
  exit 2
}

api=''; room=''; static=''; gateway=''
while (($#)); do
  case "$1" in
    --api) api="${2:?missing api digest}"; shift 2 ;;
    --room) room="${2:?missing room digest}"; shift 2 ;;
    --static) static="${2:?missing static digest}"; shift 2 ;;
    --gateway) gateway="${2:?missing gateway digest}"; shift 2 ;;
    -h|--help) usage ;;
    *) usage ;;
  esac
done
for value in "$api" "$room" "$static" "$gateway"; do
  [[ "$value" =~ ^ghcr\.io/macel94/cloudnativepong-(api|room|static|gateway)@sha256:[0-9a-f]{64}$ ]] || {
    echo 'all inputs must be exact GHCR IMAGE@sha256:<64-hex-digest> references' >&2
    exit 2
  }
done

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
export API_DIGEST="$api" ROOM_DIGEST="$room" STATIC_DIGEST="$static" GATEWAY_DIGEST="$gateway"
python3 - "$root" <<'PY'
import os
import re
import sys
from pathlib import Path

root = Path(sys.argv[1])
refs = {
    "api": os.environ["API_DIGEST"],
    "room": os.environ["ROOM_DIGEST"],
    "static": os.environ["STATIC_DIGEST"],
    "gateway": os.environ["GATEWAY_DIGEST"],
}
for component, ref in refs.items():
    digest = ref.split("@", 1)[1]
    if component == "room":
        path = root / "k8s/overlays/server/room-template.yaml"
        text = path.read_text()
        text = re.sub(r'ghcr\.io/macel94/cloudnativepong-room:[^"\s]+', ref, text)
        path.write_text(text)
    elif component == "api":
        path = root / "k8s/overlays/server/api-production.yaml"
        text = path.read_text()
        text = re.sub(r'--room-image=ghcr\.io/macel94/cloudnativepong-room:[^\s]+', '--room-image=' + refs["room"], text)
        path.write_text(text)
    else:
        pass

kustomization = root / "k8s/overlays/server/kustomization.yaml"
text = kustomization.read_text()
for component, ref in refs.items():
    name = f"ghcr.io/macel94/cloudnativepong-{component}"
    digest = ref.split("@", 1)[1]
    text = re.sub(rf'(newName: {re.escape(name)}\n\s+)(newTag: ).*', rf'\1digest: {digest}', text)
kustomization.write_text(text)
metadata = root / "release-metadata.json"
data = __import__('json').loads(metadata.read_text())
data["resolution_status"] = "verified"
for image in data["images"]:
    image["digest"] = refs[image["component"]].split("@", 1)[1]
    image["signature"] = "not_yet_verified"
metadata.write_text(__import__('json').dumps(data, indent=2) + "\n")
print('promoted immutable image digests into the production overlay; signature remains not_yet_verified')
PY
