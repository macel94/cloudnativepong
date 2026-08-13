#!/usr/bin/env python3
"""Generate and validate privacy-safe Pong production promotion records.

The record deliberately accepts only immutable image references and bounded,
operator-supplied deployment evidence. It never accepts a target URL, secret,
room identifier, user data, or arbitrary metadata.
"""
from __future__ import annotations

import argparse
import datetime as dt
import json
import re
import sys
from pathlib import Path

COMPONENTS = ("api", "room", "static", "gateway")
DIGEST_REF = re.compile(
    r"^ghcr\.io/macel94/cloudnativepong-(api|room|static|gateway)@sha256:([0-9a-f]{64})$"
)
SHA = re.compile(r"^[0-9a-f]{40}$")
REVISION = re.compile(r"^[A-Za-z0-9._:/@+=,-]{1,256}$")
RECORD_ID = re.compile(r"^release-[0-9a-f]{40}-[0-9]{8}T[0-9]{6}Z$")
FORBIDDEN_KEYS = {
    "address", "body", "client", "ip", "name", "password", "room",
    "secret", "token", "url", "user", "username",
}


class RecordError(ValueError):
    pass


def fail(message: str) -> None:
    raise RecordError(message)


def parse_time(value: object, field: str) -> dt.datetime:
    if not isinstance(value, str):
        fail(f"{field} must be an RFC3339 timestamp")
    text = value
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    try:
        parsed = dt.datetime.fromisoformat(text)
    except ValueError:
        fail(f"{field} must be an RFC3339 timestamp")
    if parsed.tzinfo is None:
        fail(f"{field} must include a timezone")
    return parsed.astimezone(dt.timezone.utc)


def timestamp(value: dt.datetime) -> str:
    return value.astimezone(dt.timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")


def reject_private_keys(value: object, path: str = "record") -> None:
    if isinstance(value, dict):
        for key, child in value.items():
            if not isinstance(key, str):
                fail(f"{path} contains a non-string key")
            if key.lower() in FORBIDDEN_KEYS:
                fail(f"{path}.{key} is not permitted in release evidence")
            reject_private_keys(child, f"{path}.{key}")
    elif isinstance(value, list):
        for index, child in enumerate(value):
            reject_private_keys(child, f"{path}[{index}]")


def require_keys(value: dict[str, object], required: set[str], allowed: set[str], path: str) -> None:
    missing = required - set(value)
    extra = set(value) - allowed
    if missing:
        fail(f"{path} is missing {', '.join(sorted(missing))}")
    if extra:
        fail(f"{path} contains unsupported fields: {', '.join(sorted(extra))}")


def nonnegative_int(value: object, field: str) -> int:
    if not isinstance(value, int) or isinstance(value, bool) or value < 0:
        fail(f"{field} must be a non-negative integer")
    return value


def validate_images(images: object) -> list[dict[str, object]]:
    if not isinstance(images, list) or len(images) != len(COMPONENTS):
        fail("images must contain exactly four entries")
    seen: set[str] = set()
    normalized: list[dict[str, object]] = []
    for index, item in enumerate(images):
        path = f"images[{index}]"
        if not isinstance(item, dict):
            fail(f"{path} must be an object")
        if "reference" in item:
            require_keys(item, {"reference", "attestation"}, {"reference", "attestation"}, path)
            reference = item["reference"]
            if not isinstance(reference, str):
                fail(f"{path}.reference must be a digest reference")
            match = DIGEST_REF.fullmatch(reference)
            if not match:
                fail(f"{path}.reference must be an exact GHCR IMAGE@sha256 digest")
            component = match.group(1)
            image = reference.split("@", 1)[0]
            digest = "sha256:" + match.group(2)
        else:
            require_keys(item, {"component", "image", "digest", "attestation"},
                         {"component", "image", "digest", "attestation"}, path)
            component = item["component"]
            image = item["image"]
            digest = item["digest"]
            if component not in COMPONENTS or image != f"ghcr.io/macel94/cloudnativepong-{component}" or not isinstance(digest, str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
                fail(f"{path} contains an invalid canonical image digest")
            match = DIGEST_REF.fullmatch(f"{image}@{digest}")
        if component in seen:
            fail(f"images repeats component {component}")
        seen.add(component)
        attestation = item["attestation"]
        if attestation not in {"verified", "failed", "not_run"}:
            fail(f"{path}.attestation must be verified, failed, or not_run")
        normalized.append({
            "component": component,
            "image": image,
            "digest": digest,
            "attestation": attestation,
        })
    if seen != set(COMPONENTS):
        fail("images must cover api, room, static, and gateway")
    return sorted(normalized, key=lambda item: COMPONENTS.index(item["component"]))


def validate_synthetic(value: object) -> dict[str, object]:
    if not isinstance(value, dict):
        fail("promotion.synthetic must be an object")
    require_keys(value, {"status", "started_at", "completed_at", "duration_ms"},
                 {"status", "started_at", "completed_at", "duration_ms"}, "promotion.synthetic")
    status = value["status"]
    if status not in {"passed", "failed", "not_run"}:
        fail("promotion.synthetic.status is invalid")
    started = value["started_at"]
    completed = value["completed_at"]
    duration = value["duration_ms"]
    if status == "not_run":
        if started is not None or completed is not None or duration is not None:
            fail("a not_run synthetic result cannot contain timing")
        return {"status": status, "started_at": None, "completed_at": None, "duration_ms": None}
    start = parse_time(started, "promotion.synthetic.started_at")
    end = parse_time(completed, "promotion.synthetic.completed_at")
    if end < start:
        fail("promotion.synthetic.completed_at precedes started_at")
    duration_int = nonnegative_int(duration, "promotion.synthetic.duration_ms")
    actual_ms = int((end - start).total_seconds() * 1000)
    if abs(actual_ms - duration_int) > 1000:
        fail("promotion.synthetic.duration_ms disagrees with timestamps")
    return {"status": status, "started_at": timestamp(start), "completed_at": timestamp(end), "duration_ms": duration_int}


def validate_rollback(value: object) -> dict[str, object]:
    if not isinstance(value, dict):
        fail("rollback must be an object")
    require_keys(value, {"status", "action", "detected_at", "recovered_at", "detection_time_seconds", "recovery_time_seconds"},
                 {"status", "action", "detected_at", "recovered_at", "detection_time_seconds", "recovery_time_seconds"}, "rollback")
    status = value["status"]
    if status not in {"not_required", "pending_review", "completed"}:
        fail("rollback.status is invalid")
    action = value["action"]
    allowed_actions = {None, "reviewed_git_flux_revert", "disposable_kubectl_undo"}
    if action not in allowed_actions:
        fail("rollback.action is invalid")
    detected = value["detected_at"]
    recovered = value["recovered_at"]
    detection = value["detection_time_seconds"]
    recovery = value["recovery_time_seconds"]
    if status == "not_required":
        if action is not None or detected is not None or recovered is not None or detection is not None or recovery is not None:
            fail("not_required rollback must not contain timing or an action")
        return {"status": status, "action": None, "detected_at": None, "recovered_at": None, "detection_time_seconds": None, "recovery_time_seconds": None}
    if action is None or detected is None:
        fail("a rollback requiring review must include an action and detection time")
    detected_time = parse_time(detected, "rollback.detected_at")
    detection_int = nonnegative_int(detection, "rollback.detection_time_seconds")
    if status == "pending_review":
        if recovered is not None or recovery is not None:
            fail("pending_review rollback cannot claim recovery")
        return {"status": status, "action": action, "detected_at": timestamp(detected_time), "recovered_at": None, "detection_time_seconds": detection_int, "recovery_time_seconds": None}
    if recovered is None:
        fail("completed rollback must include recovered_at")
    recovered_time = parse_time(recovered, "rollback.recovered_at")
    if recovered_time < detected_time:
        fail("rollback.recovered_at precedes detected_at")
    recovery_int = nonnegative_int(recovery, "rollback.recovery_time_seconds")
    actual_seconds = int((recovered_time - detected_time).total_seconds())
    if abs(actual_seconds - recovery_int) > 1:
        fail("rollback.recovery_time_seconds disagrees with timestamps")
    return {"status": status, "action": action, "detected_at": timestamp(detected_time),
            "recovered_at": timestamp(recovered_time), "detection_time_seconds": detection_int,
            "recovery_time_seconds": recovery_int}


def validate_record(raw: object) -> dict[str, object]:
    if not isinstance(raw, dict):
        fail("record must be a JSON object")
    reject_private_keys(raw)
    required = {"schema_version", "record_id", "source_commit", "release_tag", "images", "promotion", "rollback", "dora", "privacy"}
    allowed = required
    require_keys(raw, required, allowed, "record")
    if raw["schema_version"] != "belacca.release-record.v1":
        fail("unexpected release record schema")
    source = raw["source_commit"]
    if not isinstance(source, str) or not SHA.fullmatch(source):
        fail("source_commit must be a full lowercase Git SHA")
    if raw["release_tag"] != "sha-" + source:
        fail("release_tag must match source_commit")
    record_id = raw["record_id"]
    if not isinstance(record_id, str) or not RECORD_ID.fullmatch(record_id) or not record_id.startswith("release-" + source + "-"):
        fail("record_id must identify the source commit and promotion timestamp")
    images = validate_images(raw["images"])

    promotion = raw["promotion"]
    if not isinstance(promotion, dict):
        fail("promotion must be an object")
    require_keys(promotion, {"environment", "status", "source_commit_at", "started_at", "completed_at", "deployment_revision", "flux_source_revision", "flux_kustomization_revision", "synthetic"},
                 {"environment", "status", "source_commit_at", "started_at", "completed_at", "deployment_revision", "flux_source_revision", "flux_kustomization_revision", "synthetic"}, "promotion")
    if promotion["environment"] != "production":
        fail("promotion.environment must be production")
    status = promotion["status"]
    if status not in {"succeeded", "halted", "rolled_back"}:
        fail("promotion.status is invalid")
    source_at = parse_time(promotion["source_commit_at"], "promotion.source_commit_at")
    started_at = parse_time(promotion["started_at"], "promotion.started_at")
    completed_at = parse_time(promotion["completed_at"], "promotion.completed_at")
    if started_at < source_at:
        fail("promotion.started_at precedes source_commit_at")
    if completed_at < started_at:
        fail("promotion.completed_at precedes started_at")
    for field in ("deployment_revision", "flux_source_revision", "flux_kustomization_revision"):
        if not isinstance(promotion[field], str) or not REVISION.fullmatch(promotion[field]):
            fail(f"promotion.{field} must be a bounded revision string")
    synthetic = validate_synthetic(promotion["synthetic"])
    if status == "succeeded" and synthetic["status"] != "passed":
        fail("a succeeded promotion requires a passed synthetic result")
    if status == "succeeded" and any(image["attestation"] != "verified" for image in images):
        fail("a succeeded promotion requires verified attestations for every image")
    if status in {"halted", "rolled_back"} and synthetic["status"] == "passed" and status == "halted":
        fail("a halted promotion cannot claim a passed post-deploy synthetic")

    rollback = validate_rollback(raw["rollback"])
    if status == "rolled_back" and rollback["status"] != "completed":
        fail("a rolled_back promotion requires completed rollback evidence")
    if status == "succeeded" and rollback["status"] != "not_required":
        fail("a succeeded promotion cannot contain rollback evidence")

    dora = raw["dora"]
    if not isinstance(dora, dict):
        fail("dora must be an object")
    require_keys(dora, {"window_start", "window_end", "deployment_count", "failed_deployment_count", "deployment_frequency_per_day", "change_failure_rate", "lead_time_seconds"},
                 {"window_start", "window_end", "deployment_count", "failed_deployment_count", "deployment_frequency_per_day", "change_failure_rate", "lead_time_seconds"}, "dora")
    window_start = parse_time(dora["window_start"], "dora.window_start")
    window_end = parse_time(dora["window_end"], "dora.window_end")
    if window_end <= window_start:
        fail("dora.window_end must be after window_start")
    count = nonnegative_int(dora["deployment_count"], "dora.deployment_count")
    failed_count = nonnegative_int(dora["failed_deployment_count"], "dora.failed_deployment_count")
    if failed_count > count:
        fail("dora.failed_deployment_count cannot exceed deployment_count")
    for field in ("deployment_frequency_per_day", "change_failure_rate"):
        if not isinstance(dora[field], (int, float)) or isinstance(dora[field], bool) or dora[field] < 0:
            fail(f"dora.{field} must be non-negative")
    if dora["change_failure_rate"] > 1:
        fail("dora.change_failure_rate must be between zero and one")
    lead_time = nonnegative_int(dora["lead_time_seconds"], "dora.lead_time_seconds")
    expected_lead = int((started_at - source_at).total_seconds())
    if abs(expected_lead - lead_time) > 1:
        fail("dora.lead_time_seconds disagrees with source and promotion timestamps")
    days = (window_end - window_start).total_seconds() / 86400
    expected_frequency = count / days
    expected_failure_rate = failed_count / count if count else 0
    if abs(float(dora["deployment_frequency_per_day"]) - expected_frequency) > 0.0001:
        fail("dora.deployment_frequency_per_day disagrees with the window and count")
    if abs(float(dora["change_failure_rate"]) - expected_failure_rate) > 0.0001:
        fail("dora.change_failure_rate disagrees with the window and count")

    privacy = raw["privacy"]
    if privacy != {"contains_secrets": False, "contains_user_data": False}:
        fail("privacy declaration must explicitly be false for secrets and user data")
    return {
        "schema_version": "belacca.release-record.v1",
        "record_id": record_id,
        "source_commit": source,
        "release_tag": "sha-" + source,
        "images": images,
        "promotion": {**promotion, "source_commit_at": timestamp(source_at), "started_at": timestamp(started_at), "completed_at": timestamp(completed_at), "synthetic": synthetic},
        "rollback": rollback,
        "dora": {**dora, "window_start": timestamp(window_start), "window_end": timestamp(window_end), "lead_time_seconds": lead_time},
        "privacy": {"contains_secrets": False, "contains_user_data": False},
    }


def generate(raw: object) -> dict[str, object]:
    if not isinstance(raw, dict):
        fail("input must be a JSON object")
    # Derive the record ID and DORA calculated values so callers cannot inject
    # inconsistent rates or a mutable release reference.
    source = raw.get("source_commit")
    promotion = raw.get("promotion")
    dora = raw.get("dora")
    if not isinstance(source, str) or not isinstance(promotion, dict) or not isinstance(dora, dict):
        fail("input must include source_commit, promotion, and dora")
    started = parse_time(promotion.get("started_at"), "promotion.started_at")
    start_window = parse_time(dora.get("window_start"), "dora.window_start")
    end_window = parse_time(dora.get("window_end"), "dora.window_end")
    count = nonnegative_int(dora.get("deployment_count"), "dora.deployment_count")
    failed = nonnegative_int(dora.get("failed_deployment_count"), "dora.failed_deployment_count")
    days = (end_window - start_window).total_seconds() / 86400
    if days <= 0:
        fail("dora window must be positive")
    generated = json.loads(json.dumps(raw))
    generated["schema_version"] = "belacca.release-record.v1"
    generated["release_tag"] = "sha-" + source
    generated["record_id"] = f"release-{source}-{started.strftime('%Y%m%dT%H%M%SZ')}"
    generated.setdefault("privacy", {"contains_secrets": False, "contains_user_data": False})
    generated["dora"]["deployment_frequency_per_day"] = count / days
    generated["dora"]["change_failure_rate"] = failed / count if count else 0
    source_at = parse_time(promotion.get("source_commit_at"), "promotion.source_commit_at")
    generated["dora"]["lead_time_seconds"] = int((started - source_at).total_seconds())
    return validate_record(generated)


def load(path: Path) -> object:
    try:
        return json.loads(path.read_text())
    except (OSError, json.JSONDecodeError) as error:
        fail(f"could not read JSON input: {error}")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    for command in ("generate", "validate"):
        item = sub.add_parser(command)
        item.add_argument("--input", required=True, type=Path)
        item.add_argument("--output", type=Path)
    args = parser.parse_args()
    try:
        if args.command == "generate":
            result = generate(load(args.input))
        else:
            result = validate_record(load(args.input))
        if args.output:
            args.output.write_text(json.dumps(result, indent=2) + "\n")
        else:
            print(json.dumps(result, indent=2))
        return 0
    except (RecordError, OSError, TypeError, ValueError) as error:
        print(f"release record validation failed: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
