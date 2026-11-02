#!/usr/bin/env python3
"""Synthetic readback for the Route B external dependency contract."""

from __future__ import annotations

import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

import routeb_dependency_readback as readback

REQUIRED = ("forgejo", "multica", "caddy", "tailscale", "service-manager")


def item(name: str, **overrides: object) -> dict:
    body = {
        "name": name,
        "owner": f"{name}-owner",
        "objects": ["synthetic-object"],
        "version": "synthetic-1",
        "acl_digest": "a" * 64,
        "mtime_ns": 1,
        "content_sha256": "b" * 64,
        "expected_sha256": "b" * 64,
        "stop_before": ["writers"],
        "restore_order": 1,
        "readback": ["exact-effect"],
        "unknown": "stop",
        "rolls_back_with_ags": False,
    }
    body.update(overrides)
    return body


def complete() -> list[dict]:
    return [item(name) for name in REQUIRED]


class ReadbackTest(unittest.TestCase):
    def test_complete_contract_is_accepted(self) -> None:
        self.assertEqual(readback.evaluate(complete()), [])

    def test_missing_owner_is_refused(self) -> None:
        contracts = complete()
        contracts[0]["owner"] = ""
        self.assertIn("forgejo.owner_missing", readback.evaluate(contracts))

    def test_hash_mismatch_is_refused(self) -> None:
        contracts = complete()
        contracts[1]["content_sha256"] = "c" * 64
        self.assertIn("multica.hash_mismatch", readback.evaluate(contracts))

    def test_shared_forgejo_cannot_roll_back_with_ags(self) -> None:
        contracts = complete()
        contracts[0]["rolls_back_with_ags"] = True
        self.assertIn("forgejo.shared_rollback_refused", readback.evaluate(contracts))

    def test_module_does_not_touch_production_home(self) -> None:
        source = Path(readback.__file__).read_text(encoding="utf-8")
        self.assertNotIn("Path.home", source)
        self.assertNotIn(".ags", source)

    def test_cli_accepts_synthetic_file_only(self) -> None:
        script = Path(__file__).with_name("routeb_dependency_readback.py")
        configured = os.environ.get("ROUTEB_SANDBOX")
        parent = Path(configured) if configured else Path(tempfile.gettempdir()) / "ags-routeb-readback"
        parent.mkdir(parents=True, exist_ok=True)
        with tempfile.TemporaryDirectory(prefix="routeb-readback-", dir=parent) as directory:
            path = Path(directory) / "contract.json"
            path.write_text(json.dumps(complete()), encoding="utf-8")
            completed = subprocess.run([script, str(path)], check=False, text=True, capture_output=True)
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertNotIn("/.ags", completed.stdout + completed.stderr)


if __name__ == "__main__":
    unittest.main()
