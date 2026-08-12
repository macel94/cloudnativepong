#!/usr/bin/env bash
# Promote already-published immutable image digests into the production overlay.
# By default this records digest resolution only. Use --verify-attestations to
# require GitHub Artifact Attestation verification for every image before the
# release metadata is marked fully verified.
set -euo pipefail

usage() {
  echo "usage: $0 [--verify-attestations] --api IMAGE@sha256:DIGEST --room IMAGE@sha256:DIGEST --static IMAGE@sha256:DIGEST --gateway IMAGE@sha256:DIGEST" >&2
  exit 2
}

api=''
room=''
static=''
gateway=''
verify_attestations=0
while (($#)); do
  case "$1" in
    --api) api="${2:?missing api digest}"; shift 2 ;;
    --room) room="${2:?missing room digest}"; shift 2 ;;
    --static) static="${2:?missing static digest}"; shift 2 ;;
    --gateway) gateway="${2:?missing gateway digest}"; shift 2 ;;
    --verify-attestations) verify_attestations=1; shift ;;
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
if ((verify_attestations)); then
  for value in "$api" "$room" "$static" "$gateway"; do
    "$root/scripts/verify-attestation.sh" "$value"
  done
fi

export API_DIGEST="$api" ROOM_DIGEST="$room" STATIC_DIGEST="$static" GATEWAY_DIGEST="$gateway" VERIFY_ATTESTATIONS="$verify_attestations"
python3 - "$root" <<'PY'
import json
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
verified = os.environ.get("VERIFY_ATTESTATIONS") == "1"

for overlay, api_file in [
    ("server", "api-production.yaml"),
    ("native-production", "api-native-production.yaml"),
]:
    room_template = root / f"k8s/overlays/{overlay}/room-template.yaml"
    text = room_template.read_text()
    text = re.sub(r'ghcr\.io/macel94/cloudnativepong-room:[^"\s]+', refs["room"], text)
    room_template.write_text(text)

    api = root / f"k8s/overlays/{overlay}/{api_file}"
    text = api.read_text()
    text = re.sub(
        r'--room-image=ghcr\.io/macel94/cloudnativepong-room:[^\s]+',
        '--room-image=' + refs["room"],
        text,
    )
    api.write_text(text)

    kustomization = root / f"k8s/overlays/{overlay}/kustomization.yaml"
    text = kustomization.read_text()
    for component, ref in refs.items():
        name = f"ghcr.io/macel94/cloudnativepong-{component}"
        digest = ref.split("@", 1)[1]
        text = re.sub(
            rf'(newName: {re.escape(name)}\n\s+)(?:newTag|digest): .*',
            rf'\1digest: {digest}',
            text,
        )
    kustomization.write_text(text)

metadata = root / "release-metadata.json"
data = json.loads(metadata.read_text())
data["resolution_status"] = "verified" if verified else "digests_resolved"
for image in data["images"]:
    image["digest"] = refs[image["component"]].split("@", 1)[1]
    image["provenance"] = "verified" if verified else "required"
    image["attestation"] = "verified" if verified else "not_yet_verified"
metadata.write_text(json.dumps(data, indent=2) + "\n")

if verified:
    print("promoted immutable image digests after GitHub Artifact Attestation verification")
else:
    print("promoted immutable image digests; GitHub Artifact Attestation verification remains required")
PY
