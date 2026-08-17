#!/usr/bin/env python3
"""Validate immutable application release references and supply-chain metadata."""

from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
OVERLAY_NAMES = ("server", "native-staging")
COMPONENTS = ("api", "room", "static", "gateway")
OVERLAYS = [
    ROOT / "k8s" / "overlays" / name / "kustomization.yaml"
    for name in OVERLAY_NAMES
]
ROOM_TEMPLATES = [
    ROOT / "k8s" / "overlays" / name / "room-template.yaml"
    for name in OVERLAY_NAMES
]
K8S_ROOT = ROOT / "k8s"
API_PATCHES = [
    ROOT / "k8s" / "overlays" / name / api_file
    for name, api_file in (
        ("server", "api-production.yaml"),
        ("native-staging", "api-native-staging.yaml"),
    )
]
RELEASE = ROOT / "release-metadata.json"
SHA_TAG = re.compile(r"sha-[0-9a-f]{40}\Z")
DIGEST = re.compile(r"sha256:[0-9a-f]{64}\Z")
IMAGE_PREFIX = "ghcr.io/macel94/cloudnativepong-"
EXPECTED_IMAGE_NAMES = tuple(f"cloudnativepong-{component}" for component in COMPONENTS)
EXPECTED_IMAGE_REFS = tuple(IMAGE_PREFIX + component for component in COMPONENTS)


class ReleaseError(ValueError):
    """A release-contract violation with a user-facing message."""


def fail(message: str) -> None:
    raise ReleaseError(message)


def parse_kustomization(text: str, overlay_name: str) -> tuple[str, tuple[str, ...]]:
    """Read the deliberately small `images` shape used by these overlays.

    Keeping this parser dependency-free makes the validator usable on the
    stock Python installed by GitHub-hosted runners.  The surrounding shape is
    checked strictly enough that an accidental YAML embedding or extra image
    entry cannot be silently ignored.
    """

    entries = re.findall(
        r"(?ms)^\s*-\s+name:\s*(\S+)\s*\n"
        r"\s+newName:\s*(\S+)\s*\n"
        r"\s+(newTag|digest):\s*(\S+)\s*$",
        text,
    )
    if len(entries) != len(COMPONENTS):
        fail(f"{overlay_name} overlay must define exactly four Pong images")

    by_name: dict[str, tuple[str, str]] = {}
    for name, new_name, ref_kind, value in entries:
        if name in by_name:
            fail(f"{overlay_name} overlay repeats image {name}")
        expected_new_name = IMAGE_PREFIX + name.removeprefix("cloudnativepong-")
        if name not in EXPECTED_IMAGE_NAMES or new_name != expected_new_name:
            fail(f"{overlay_name} overlay contains an unexpected Pong image name")
        if ref_kind == "newTag" and not SHA_TAG.fullmatch(value):
            fail(f"{overlay_name} overlay must use full lowercase SHA image tags")
        if ref_kind == "digest" and not DIGEST.fullmatch(value):
            fail(f"{overlay_name} overlay must use full sha256 image digests")
        by_name[name] = (ref_kind, value)

    if set(by_name) != set(EXPECTED_IMAGE_NAMES):
        fail(f"{overlay_name} overlay must cover api, room, static, and gateway")
    modes = {kind for kind, _ in by_name.values()}
    if len(modes) != 1:
        fail(f"{overlay_name} overlay must not mix SHA tags and digests")

    mode = next(iter(modes))
    refs = tuple(by_name[f"cloudnativepong-{component}"][1] for component in COMPONENTS)
    if mode == "newTag" and len(set(refs)) != 1:
        fail(f"{overlay_name} overlay must use one release tag for all four images")
    return ("tag" if mode == "newTag" else "digest", refs)


def embedded_template_image(text: str, overlay_name: str) -> str:
    """Extract and parse the JSON stored under ConfigMap `template.json`."""

    lines = text.splitlines()
    try:
        start = next(
            index
            for index, line in enumerate(lines)
            if line.strip() == "template.json: |"
        )
    except StopIteration:
        fail(f"{overlay_name} room template is missing the template.json block")

    embedded: list[str] = []
    for line in lines[start + 1 :]:
        if line.startswith("    "):
            embedded.append(line[4:])
        elif line.strip() == "":
            embedded.append("")
        else:
            break
    if not embedded:
        fail(f"{overlay_name} room template has an empty template.json block")
    try:
        pod = json.loads("\n".join(embedded))
    except json.JSONDecodeError as error:
        fail(f"{overlay_name} room template contains invalid embedded JSON: {error}")

    try:
        containers = pod["spec"]["containers"]
        if not isinstance(containers, list) or len(containers) != 1:
            raise TypeError
        image = containers[0]["image"]
    except (KeyError, IndexError, TypeError):
        fail(f"{overlay_name} room template must define one container image")
    if not isinstance(image, str):
        fail(f"{overlay_name} room template image must be a string")
    return image


def expected_image_ref(component: str, mode: str, value: str) -> str:
    name = IMAGE_PREFIX + component
    return f"{name}:{value}" if mode == "tag" else f"{name}@{value}"


def has_cpu_limit(text: str) -> bool:
    """Detect YAML or embedded room-template JSON CPU limits.

    CPU requests remain intentional: they affect scheduling and HPA
    utilization. CPU limits are not allowed because they impose a hard cgroup
    throttle even when node CPU is available. This small structural check is
    dependency-free and covers the YAML and JSON shapes used by the manifests.
    """

    lines = text.splitlines()
    for index, line in enumerate(lines):
        stripped = line.strip().lower()
        if re.search(r"limits\.cpu\s*:", stripped):
            return True
        if re.search(r"['\"]?limits['\"]?\s*:\s*\{", stripped):
            block = stripped
            for following in lines[index + 1 : index + 8]:
                block += " " + following.strip().lower()
                if "}" in following:
                    break
            if re.search(r"['\"]?cpu['\"]?\s*:", block):
                return True
        if stripped == "limits:":
            indent = len(line) - len(line.lstrip())
            for following in lines[index + 1 :]:
                if not following.strip():
                    continue
                following_indent = len(following) - len(following.lstrip())
                if following_indent <= indent:
                    break
                if re.match(r"cpu\s*:", following.strip(), re.IGNORECASE):
                    return True
    return False


def validate_cpu_limit_policy() -> None:
    """Ensure every generated Pong manifest omits CPU limits permanently."""

    try:
        paths = sorted(K8S_ROOT.rglob("*.yaml")) + sorted(K8S_ROOT.rglob("*.yml"))
    except OSError as error:
        fail(f"could not enumerate Pong Kustomize manifests: {error}")
    for path in paths:
        try:
            text = path.read_text()
        except OSError as error:
            fail(f"could not read {path}: {error}")
        if has_cpu_limit(text):
            fail(f"Pong Kustomize manifest {path.relative_to(ROOT)} sets a CPU limit")


def validate_overlay(
    overlay_name: str,
    kustomization: Path,
    room_template: Path,
    api_patch: Path,
) -> tuple[str, tuple[str, ...]]:
    try:
        kustomization_text = kustomization.read_text()
        room_text = room_template.read_text()
        api_text = api_patch.read_text()
    except OSError as error:
        fail(f"could not read {overlay_name} release files: {error}")

    mode, refs = parse_kustomization(kustomization_text, overlay_name)
    expected_room = expected_image_ref("room", mode, refs[1])

    actual_room = embedded_template_image(room_text, overlay_name)
    if actual_room != expected_room:
        fail(
            f"{overlay_name} room template image {actual_room!r} does not match "
            f"the overlay room image {expected_room!r}"
        )

    room_args = re.findall(r"--room-image=([^\s\"']+)", api_text)
    if room_args != [expected_room]:
        fail(
            f"{overlay_name} API --room-image must be exactly {expected_room!r}"
        )

    # Catch mutable references in overlay patches as well as in the two files
    # parsed above.  Base manifests intentionally use :latest for disposable
    # overlays; only these production overlays are covered here.
    overlay_dir = kustomization.parent
    for path in overlay_dir.glob("*.yaml"):
        try:
            if re.search(r"(?i)(?:image|newTag|--room-image)[^\n]*:latest\b", path.read_text()):
                fail(f"{overlay_name} production overlay contains a mutable latest reference")
        except OSError as error:
            fail(f"could not read {path}: {error}")

    return mode, refs


def validate_metadata() -> int:
    try:
        metadata = json.loads(RELEASE.read_text())
    except (OSError, json.JSONDecodeError) as error:
        fail(f"could not read release metadata: {error}")

    if metadata.get("schema_version") != "belacca.release-metadata.v2":
        fail("unexpected release metadata schema")
    source_commit = metadata.get("source_commit")
    if source_commit is not None and not re.fullmatch(r"[0-9a-f]{40}", source_commit):
        fail("source_commit must be full lowercase Git SHA or null")
    if source_commit is not None and metadata.get("release_tag") != f"sha-{source_commit}":
        fail("release_tag must match source_commit")
    if metadata.get("resolution_status") not in {
        "pending_registry_resolution",
        "digests_resolved",
        "verified",
    }:
        fail("resolution_status must be explicit")

    images = metadata.get("images")
    if not isinstance(images, list) or len(images) != len(COMPONENTS):
        fail("release metadata must contain exactly four image entries")
    by_component: dict[str, dict[str, object]] = {}
    for image in images:
        if not isinstance(image, dict):
            fail("release metadata image entries must be objects")
        component = image.get("component")
        if component in by_component:
            fail(f"release metadata repeats component {component}")
        if component not in COMPONENTS:
            fail(f"release metadata contains unexpected component {component}")
        by_component[component] = image
        status = metadata["resolution_status"]
        digest = image.get("digest")
        if status in {"digests_resolved", "verified"} and not isinstance(digest, str):
            fail(f"{component} must record a sha256 digest when digests are resolved")
        if status in {"digests_resolved", "verified"} and not DIGEST.fullmatch(digest or ""):
            fail(f"{component} must record a full sha256 digest when digests are resolved")
        if status == "pending_registry_resolution" and digest is not None:
            fail(f"{component} must remain null until all registry digests are resolved")
        if status == "verified":
            if image.get("provenance") != "verified":
                fail(f"{component} must have verified GitHub provenance")
            if image.get("attestation") != "verified":
                fail(f"{component} must have a verified GitHub artifact attestation")
        if image.get("provenance") not in {"required", "verified"}:
            fail("provenance must be required or verified")
        if image.get("attestation") not in {"required", "verified", "not_yet_verified"}:
            fail("attestation must be explicit")
        if "signature" in image:
            fail("use GitHub artifact attestation instead of the legacy signature field")

    if set(by_component) != set(COMPONENTS):
        fail("release metadata must cover api, room, static, and gateway")
    return len(images)


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Validate immutable application release references and metadata."
    )
    parser.add_argument(
        "--allow-pending-publication",
        action="store_true",
        help=(
            "allow the two deployment overlays to carry different immutable "
            "releases while a source-push publication is pending; each overlay "
            "is still validated completely"
        ),
    )
    args = parser.parse_args()

    try:
        validate_cpu_limit_policy()
        release_refs = [
            validate_overlay(name, overlay, room, api)
            for name, overlay, room, api in zip(
                OVERLAY_NAMES, OVERLAYS, ROOM_TEMPLATES, API_PATCHES
            )
        ]
        modes = {mode for mode, _ in release_refs}
        if len(modes) != 1:
            fail("native and compatibility overlays must use the same reference mode")
        if len(set(release_refs)) != 1 and not args.allow_pending_publication:
            fail("native and compatibility Pong overlays must reference the same release")
        image_count = validate_metadata()
    except (OSError, TypeError, ValueError, ReleaseError) as error:
        print(f"release validation failed: {error}", file=sys.stderr)
        return 1
    print(f"validated immutable Pong release references and {image_count} metadata entries")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
