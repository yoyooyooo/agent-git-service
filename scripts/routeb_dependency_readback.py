#!/usr/bin/env python3
"""Check a caller-supplied external dependency contract.

The input is a JSON file. This command does not discover or open live host
configuration.
"""

from __future__ import annotations

import json
from pathlib import Path
import sys

REQUIRED = ("forgejo", "multica", "caddy", "tailscale", "service-manager")


def is_hex64(value: object) -> bool:
    if not isinstance(value, str) or len(value) != 64:
        return False
    return all(character in "0123456789abcdef" for character in value)


def evaluate(contracts: object) -> list[str]:
    if not isinstance(contracts, list):
        return ["contract_rejected"]
    errors: set[str] = set()
    seen: set[str] = set()
    for raw in contracts:
        if not isinstance(raw, dict) or not isinstance(raw.get("name"), str) or not raw.get("name"):
            errors.add("contract_rejected")
            continue
        name = raw["name"]
        if name in seen:
            errors.add(f"{name}.duplicate")
        seen.add(name)
        if name not in REQUIRED:
            errors.add(f"{name}.unknown_dependency")
        if not isinstance(raw.get("owner"), str) or not raw["owner"].strip():
            errors.add(f"{name}.owner_missing")
        objects = raw.get("objects")
        if not isinstance(objects, list) or not objects or not all(isinstance(item, str) and item for item in objects):
            errors.add(f"{name}.objects_missing")
        if not isinstance(raw.get("version"), str) or not raw["version"].strip():
            errors.add(f"{name}.version_missing")
        if not is_hex64(raw.get("acl_digest")):
            errors.add(f"{name}.acl_missing")
        if not isinstance(raw.get("mtime_ns"), int) or isinstance(raw.get("mtime_ns"), bool) or raw["mtime_ns"] < 0:
            errors.add(f"{name}.mtime_missing")
        content = raw.get("content_sha256")
        expected = raw.get("expected_sha256")
        if not is_hex64(content) or not is_hex64(expected) or content != expected:
            errors.add(f"{name}.hash_mismatch")
        for field in ("stop_before", "readback"):
            value = raw.get(field)
            if not isinstance(value, list) or not value or not all(isinstance(item, str) and item for item in value):
                errors.add(f"{name}.{field}_missing")
        if not isinstance(raw.get("restore_order"), int) or isinstance(raw.get("restore_order"), bool):
            errors.add(f"{name}.restore_order_missing")
        if raw.get("unknown") != "stop":
            errors.add(f"{name}.unknown_not_stop")
        if raw.get("rolls_back_with_ags") is not False:
            errors.add(f"{name}.shared_rollback_refused")
    for name in REQUIRED:
        if name not in seen:
            errors.add(f"{name}.missing_dependency")
    return sorted(errors)


def main(argv: list[str]) -> int:
    if len(argv) != 1:
        print("code=input_required", file=sys.stderr)
        return 78
    try:
        payload = json.loads(Path(argv[0]).read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        print("code=unreadable_input", file=sys.stderr)
        return 78
    errors = evaluate(payload)
    if errors:
        print("\n".join(errors), file=sys.stderr)
        return 78
    print("accepted")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
