#!/usr/bin/env python3
"""Validate immutable application release references and supply-chain metadata."""

from __future__ import annotations

import json
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
OVERLAYS = [
    ROOT / "k8s" / "overlays" / "server" / "kustomization.yaml",
    ROOT / "k8s" / "overlays" / "native-staging" / "kustomization.yaml",
]
ROOM_TEMPLATES = [
    ROOT / "k8s" / "overlays" / "server" / "room-template.yaml",
    ROOT / "k8s" / "overlays" / "native-staging" / "room-template.yaml",
]
RELEASE = ROOT / "release-metadata.json"
SHA_TAG = re.compile(r"sha-[0-9a-f]{40}")
DIGEST = re.compile(r"sha256:[0-9a-f]{64}")


def fail(message: str) -> None:
    raise ValueError(message)


def main() -> int:
    try:
        overlays = [path.read_text() for path in OVERLAYS]
        rooms = [path.read_text() for path in ROOM_TEMPLATES]
        release_refs = []
        for overlay, room in zip(overlays, rooms):
            tags = re.findall(r"newTag:\s*(\S+)", overlay)
            digests = re.findall(r"digest:\s*(sha256:[0-9a-f]{64})", overlay)
            if len(tags) == 4 and all(SHA_TAG.fullmatch(tag) for tag in tags):
                release_refs.append(tuple(tags))
            elif len(digests) == 4:
                release_refs.append(tuple(digests))
            else:
                fail("all four Pong image references must use full SHA tags or full sha256 digests")
            if "latest" in overlay or "latest" in room:
                fail("production Pong references must not use latest")
            room_refs = SHA_TAG.findall(room) + DIGEST.findall(room)
            if not room_refs:
                fail("room template must carry an immutable release reference")
        if len(set(release_refs)) != 1:
            fail("native and compatibility Pong overlays must reference the same release")

        metadata = json.loads(RELEASE.read_text())
        if metadata.get("schema_version") != "belacca.release-metadata.v2":
            fail("unexpected release metadata schema")
        if metadata.get("source_commit") and not re.fullmatch(r"[0-9a-f]{40}", metadata["source_commit"]):
            fail("source_commit must be full lowercase Git SHA")
        if metadata.get("resolution_status") not in {"pending_registry_resolution", "digests_resolved", "verified"}:
            fail("resolution_status must be explicit")
        images = metadata.get("images")
        if not isinstance(images, list) or {image.get("component") for image in images} != {"api", "room", "static", "gateway"}:
            fail("release metadata must cover all Pong images")
        for image in images:
            status = metadata.get("resolution_status")
            if status in {"digests_resolved", "verified"} and not DIGEST.fullmatch(image.get("digest", "")):
                fail(f"{image.get('component')} must record a sha256 digest when digests are resolved")
            if status == "pending_registry_resolution" and image.get("digest") is not None:
                fail(f"{image.get('component')} must remain null until all registry digests are resolved")
            if status == "verified":
                if image.get("provenance") != "verified":
                    fail(f"{image.get('component')} must have verified GitHub provenance")
                if image.get("attestation") != "verified":
                    fail(f"{image.get('component')} must have a verified GitHub artifact attestation")
            if image.get("provenance") not in {"required", "verified"}:
                fail("provenance must be required or verified")
            if image.get("attestation") not in {"required", "verified", "not_yet_verified"}:
                fail("attestation must be explicit")
            if "signature" in image:
                fail("use GitHub artifact attestation instead of the legacy signature field")
    except (OSError, json.JSONDecodeError, TypeError, ValueError) as error:
        print(f"release validation failed: {error}", file=sys.stderr)
        return 1
    print(f"validated immutable Pong release references and {len(images)} metadata entries")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
