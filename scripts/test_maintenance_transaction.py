#!/usr/bin/env python3
"""Isolated checks for the Route B maintenance transaction tool."""

from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

import maintenance_transaction as txn

def sandbox_dir() -> Path:
    configured = os.environ.get("ROUTEB_SANDBOX")
    root = Path(configured) if configured else Path(tempfile.gettempdir()) / "ags-routeb-maintenance"
    root.mkdir(parents=True, exist_ok=True)
    return root


REV = "b" * 40
TREE = "c" * 40


def digest(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


class TransactionTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory(prefix="routeb-txn-", dir=sandbox_dir())
        self.addCleanup(self.tmp.cleanup)
        self.base = Path(self.tmp.name)
        self.root = self.base / "root"
        self.restore = self.base / "restore"
        self.manager = self.base / "manager"
        self.audit = self.base / "audit"
        (self.root / "data").mkdir(parents=True)
        (self.root / "bin").mkdir()
        (self.root / "data" / "row.txt").write_text("row-1\n", encoding="utf-8")
        (self.root / "bin" / "old").write_bytes(b"old-binary")
        self.candidate_bin = self.base / "candidate-bin"
        self.candidate_bin.write_bytes(b"new-binary")
        self.manifest = self.base / "candidate.json"
        body = {
            "schema": "ags.routeb-candidate.v1",
            "revision": REV,
            "tree": TREE,
            "sha256": digest(self.candidate_bin),
            "binary": str(self.candidate_bin),
        }
        self.manifest.write_text(json.dumps(body), encoding="utf-8")
        self.window = self.base / "window.json"
        self.window.write_text(json.dumps({
            "schema": "ags.routeb-window.v1",
            "decision": "approve",
            "host": "mini",
            "transaction_id": "txn-1",
            "candidate_sha256": body["sha256"],
        }), encoding="utf-8")
        self.manager.mkdir()
        (self.manager / "control.json").write_text(json.dumps({
            "suppress": "ok", "stop": "ok", "start": "ok", "writers": 0,
        }), encoding="utf-8")
        self.admit_root()

    def admit_root(self) -> None:
        self.root.chmod(0o700)
        marker = self.root / ".ags-isolated-rehearsal"
        marker.write_text("isolated\n", encoding="utf-8")
        os.chmod(marker, 0o600)

    def args(self, command: str, *extra: str) -> list[str]:
        base = [
            command, "--host", "mini", "--root", str(self.root),
            "--candidate", str(self.manifest), "--window", str(self.window),
            "--restore", str(self.restore), "--manager-dir", str(self.manager),
            "--audit", str(self.audit), "--external-contract", str(self.external_contract()),
        ]
        if command in {"run", "recover-prestart"}:
            base.append("--execute")
        return base + list(extra)

    def test_refuses_production_root_without_writing(self) -> None:
        production = Path.home() / ".ags"
        marker = production / "routeb-txn-state"
        code = txn.main([
            "plan", "--host", "mini", "--root", str(production),
            "--candidate", str(self.manifest), "--window", str(self.window),
            "--restore", str(self.restore), "--manager-dir", str(self.manager),
            "--audit", str(self.audit), "--external-contract", str(self.external_contract()),
        ])
        self.assertEqual(code, 78)
        self.assertFalse(marker.exists())

    def test_refuses_child_and_ancestor_of_production_root(self) -> None:
        for root in (Path.home() / ".ags" / "data", Path.home()):
            self.assertEqual(txn.main([
                "plan", "--host", "mini", "--root", str(root),
                "--candidate", str(self.manifest), "--window", str(self.window),
                "--restore", str(self.restore), "--manager-dir", str(self.manager),
                "--audit", str(self.audit), "--external-contract", str(self.external_contract()),
            ]), 78)

    def test_refuses_symlink_root(self) -> None:
        link = self.base / "linked-root"
        link.symlink_to(self.root, target_is_directory=True)
        self.assertEqual(txn.main(self.args("plan")[:0] or [
            "plan", "--host", "mini", "--root", str(link),
            "--candidate", str(self.manifest), "--window", str(self.window),
            "--restore", str(self.restore), "--manager-dir", str(self.manager),
            "--audit", str(self.audit), "--external-contract", str(self.external_contract()),
        ]), 78)

    def test_refuses_fault_hook_without_test_env(self) -> None:
        env = os.environ.get("AGS_TXN_TEST")
        os.environ.pop("AGS_TXN_TEST", None)
        try:
            self.assertEqual(txn.main(self.args("run", "--fault-after", "stopped")), 78)
        finally:
            if env is not None:
                os.environ["AGS_TXN_TEST"] = env

    def test_isolated_run_and_rerun(self) -> None:
        self.assertEqual(txn.main(self.args("run")), 0)
        journal = json.loads((self.root / "state" / "journal.json").read_text())
        self.assertEqual(journal["phases"][-1], "finalized")
        self.assertEqual((self.root / "bin" / "current").read_bytes(), b"new-binary")
        self.assertEqual((self.restore / "data" / "row.txt").read_text(), "row-1\n")
        calls = json.loads((self.manager / "calls.json").read_text())
        self.assertEqual(txn.main(self.args("run")), 0)
        again = json.loads((self.manager / "calls.json").read_text())
        self.assertEqual(calls, again)

    def test_interrupt_then_resume_does_not_repeat_stop(self) -> None:
        os.environ["AGS_TXN_TEST"] = "1"
        self.addCleanup(os.environ.pop, "AGS_TXN_TEST", None)
        self.assertEqual(txn.main(self.args("run", "--fault-after", "stopped")), 81)
        self.assertEqual((self.root / "state" / "fence").read_text().strip(), "stopped")
        self.assertEqual(txn.main(self.args("run")), 0)
        calls = json.loads((self.manager / "calls.json").read_text())
        self.assertEqual(calls["stop"], 1)
        self.assertEqual(calls["start"], 1)

    def test_corrupt_restore_stays_fenced(self) -> None:
        os.environ["AGS_TXN_TEST"] = "1"
        self.addCleanup(os.environ.pop, "AGS_TXN_TEST", None)
        self.assertEqual(txn.main(self.args("run", "--fault-after", "corrupt-restore")), 81)
        self.assertEqual((self.root / "state" / "fence").read_text().strip(), "stopped")
        self.assertFalse((self.root / "bin" / "current").exists())

    def test_prestart_recover_only_before_start(self) -> None:
        os.environ["AGS_TXN_TEST"] = "1"
        self.addCleanup(os.environ.pop, "AGS_TXN_TEST", None)
        self.assertEqual(txn.main(self.args("run", "--fault-after", "installed")), 81)
        self.assertEqual(txn.main([
            "recover-prestart", "--host", "mini", "--root", str(self.root),
            "--candidate", str(self.manifest), "--window", str(self.window),
            "--restore", str(self.restore), "--manager-dir", str(self.manager),
            "--audit", str(self.audit), "--external-contract", str(self.external_contract()),
            "--execute",
        ]), 0)
        self.assertEqual((self.root / "bin" / "current").read_bytes(), b"old-binary")
        shutil.copyfile(self.candidate_bin, self.root / "bin" / "current")
        journal = json.loads((self.root / "state" / "journal.json").read_text())
        journal["phases"].append("start_attempted")
        (self.root / "state" / "journal.json").write_text(json.dumps(journal), encoding="utf-8")
        self.assertEqual(txn.main([
            "recover-prestart", "--host", "mini", "--root", str(self.root),
            "--candidate", str(self.manifest), "--window", str(self.window),
            "--restore", str(self.restore), "--manager-dir", str(self.manager),
            "--audit", str(self.audit), "--external-contract", str(self.external_contract()),
            "--execute",
        ]), 78)

    def external_contract(self, contracts: list[dict] | None = None) -> Path:
        path = self.base / "external.json"
        if contracts is None:
            contracts = [{
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
            } for name in ("forgejo", "multica", "caddy", "tailscale", "service-manager")]
        path.write_text(json.dumps(contracts), encoding="utf-8")
        return path

    def test_plan_refuses_missing_external_contract(self) -> None:
        argv = [item for item in self.args("plan") if item != "--external-contract" and not item.endswith("external.json")]
        code = txn.main(argv)
        self.assertEqual(code, 2)
        self.assertFalse((self.root / "state").exists())

    def test_plan_refuses_invalid_external_contract(self) -> None:
        contracts = json.loads(self.external_contract().read_text())
        contracts[0]["owner"] = ""
        contracts[1]["content_sha256"] = "c" * 64
        contracts[0]["rolls_back_with_ags"] = True
        code = txn.main(self.args("plan")[:-1] + [str(self.external_contract(contracts))])
        self.assertEqual(code, 78)
        self.assertFalse((self.root / "state").exists())

    def test_refuses_protected_secondary_paths(self) -> None:
        self.admit_root()
        production = Path.home() / ".ags"
        sentinel = production / "routeb-b1-secondary-sentinel"
        before = sentinel.exists()
        code = txn.main([
            "plan", "--host", "mini", "--root", str(self.root),
            "--candidate", str(self.manifest), "--window", str(self.window),
            "--restore", str(production / "restore"),
            "--manager-dir", str(production / "manager"),
            "--audit", str(production / "audit"),
            "--external-contract", str(self.external_contract()),
        ])
        self.assertEqual(code, 78)
        self.assertEqual(sentinel.exists(), before)
        self.assertFalse((self.root / "state").exists())

    def test_refuses_absolute_and_parent_transaction_id(self) -> None:
        self.admit_root()
        for transaction in (str(self.base / "escaped-backup"), "../../../../escaped-backup", "txn/nested", "txn\\nested"):
            self.window.write_text(json.dumps({
                "schema": "ags.routeb-window.v1",
                "decision": "approve",
                "host": "mini",
                "transaction_id": transaction,
                "candidate_sha256": digest(self.candidate_bin),
            }), encoding="utf-8")
            code = txn.main(self.args("plan"))
            self.assertEqual(code, 78, transaction)
            self.assertFalse((self.root / "state").exists())

    def test_refuses_internal_symlink_to_external_sentinel(self) -> None:
        self.admit_root()
        sentinel = self.base / "sentinel"
        sentinel.write_bytes(b"sentinel")
        current = self.root / "bin" / "current"
        current.symlink_to(sentinel)
        code = txn.main(self.args("plan"))
        self.assertEqual(code, 78)
        self.assertEqual(sentinel.read_bytes(), b"sentinel")
        self.assertTrue(current.is_symlink())

    def test_refuses_symlink_write_targets(self) -> None:
        self.admit_root()
        sentinel = self.base / "sentinel"
        sentinel.write_bytes(b"sentinel")
        (self.root / "state").mkdir()
        (self.root / "state" / "journal.json.tmp").symlink_to(sentinel)
        (self.root / "data" / "linked.txt").symlink_to(sentinel)
        code = txn.main(self.args("run"))
        self.assertEqual(code, 78)
        self.assertEqual(sentinel.read_bytes(), b"sentinel")

    def test_refuses_root_without_isolation_admission(self) -> None:
        (self.root / ".ags-isolated-rehearsal").unlink()
        self.root.chmod(0o755)
        code = txn.main(self.args("plan"))
        self.assertEqual(code, 78)
        self.assertFalse((self.root / "state").exists())

    def test_entry_refuses_production_root(self) -> None:
        entry = Path(__file__).with_name("ags-maintenance-transaction")
        completed = subprocess.run(
            [str(entry), "plan", "--host", "mini", "--root", str(Path.home() / ".ags"),
             "--candidate", str(self.manifest), "--window", str(self.window),
             "--restore", str(self.restore), "--manager-dir", str(self.manager),
             "--audit", str(self.audit), "--external-contract", str(self.external_contract())],
            check=False, text=True, capture_output=True,
        )
        self.assertEqual(completed.returncode, 78)
        self.assertIn("production_root_refused", completed.stderr)


if __name__ == "__main__":
    unittest.main()
