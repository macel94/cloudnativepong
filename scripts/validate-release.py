#!/usr/bin/env python3
"""Validate immutable application release references and supply-chain metadata."""

from __future__ import annotations

import json
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
OVERLAY = ROOT / "k8s" / "overlays" / "server" / "kustomization.yaml"
ROOM_TEMPLATE = ROOT / "k8s" / "overlays" / "server" / "room-template.yaml"
RELEASE = ROOT / "release-metadata.json"
SHA_TAG = re.compile(r"sha-[0-9a-f]{40}")
DIGEST = re.compile(r"sha256:[0-9a-f]{64}")


def fail(message: str) -> None:
    raise ValueError(message)


def main() -> int:
    try:
        overlay = OVERLAY.read_text()
        room = ROOM_TEMPLATE.read_text()
        tags = re.findall(r"newTag:\s*(\S+)", overlay)
        digests = re.findall(r"digest:\s*(sha256:[0-9a-f]{64})", overlay)
        if not ((len(tags) == 4 and all(SHA_TAG.fullmatch(tag) for tag in tags)) or len(digests) == 4):
            fail("all four Pong image references must use full SHA tags or full sha256 digests")
        if "latest" in overlay or "latest" in room:
            fail("production Pong references must not use latest")
        if not (SHA_TAG.search(room) or DIGEST.search(room)):
            fail("room template must carry an immutable release reference")

        metadata = json.loads(RELEASE.read_text())
        if metadata.get("schema_version") != "belacca.release-metadata.v1":
            fail("unexpected release metadata schema")
        if metadata.get("source_commit") and not re.fullmatch(r"[0-9a-f]{40}", metadata["source_commit"]):
            fail("source_commit must be full lowercase Git SHA")
        if metadata.get("resolution_status") not in {"pending_registry_resolution", "verified"}:
            fail("resolution_status must be explicit")
        images = metadata.get("images")
        if not isinstance(images, list) or {image.get("component") for image in images} != {"api", "room", "static", "gateway"}:
            fail("release metadata must cover all Pong images")
        for image in images:
            if metadata.get("resolution_status") == "verified" and not DIGEST.fullmatch(image.get("digest", "")):
                fail(f"{image.get('component')} must record a sha256 digest when verified")
            if metadata.get("resolution_status") == "pending_registry_resolution" and image.get("digest") is not None:
                fail(f"{image.get('component')} must remain null until all registry digests are resolved")
            if metadata.get("resolution_status") == "verified" and image.get("signature") != "verified":
                fail(f"{image.get('component')} must have a verified signature when release metadata is verified")
            if image.get("provenance") not in {"required", "verified"}:
                fail("provenance must be required or verified")
            if image.get("signature") not in {"required", "verified", "not_yet_verified"}:
                fail("signature must be explicit")
    except (OSError, json.JSONDecodeError, TypeError, ValueError) as error:
        print(f"release validation failed: {error}", file=sys.stderr)
        return 1
    print(f"validated immutable Pong release references and {len(images)} metadata entries")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
