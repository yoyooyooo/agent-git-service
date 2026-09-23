#!/usr/bin/env python3
"""Test publication checks against disposable Git repositories, without network."""
from __future__ import annotations

import importlib.util
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock

SPEC = importlib.util.spec_from_file_location("fork_audit", Path(__file__).with_name("audit.py"))
audit = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(audit)


class AuditTests(unittest.TestCase):
    def test_categories_do_not_claim_semantic_acceptance(self):
        self.assertEqual(audit.category("internal/edge/mirror.go"), "edge-and-replication")
        self.assertEqual(audit.category("internal/service/access_grant.go"), "authority-and-grants")
        self.assertEqual(audit.category("internal/service/pr_projection.go"), "provider-and-delivery")
        self.assertEqual(audit.category("unknown.data"), "unclassified")

    def test_location_rules_use_documentation_examples(self):
        rules = audit.privacy_rules(None)
        private_home = "/Users/" + "operator-private/work"
        self.assertIsNotNone(rules["operator-home-path"].search(private_home))
        self.assertIsNone(rules["operator-home-path"].search("/Users/example/work"))
        self.assertIsNone(rules["operator-home-path"].search("/home/runner/work"))

    def test_removed_content_still_blocks_history_and_empty_commits_are_counted(self):
        with tempfile.TemporaryDirectory(prefix="fork-audit-") as directory:
            root = Path(directory)
            environment = {k: v for k, v in os.environ.items() if not k.startswith("GIT_")}
            environment.update({"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.devnull,
                                "GIT_AUTHOR_NAME": "Audit Fixture", "GIT_COMMITTER_NAME": "Audit Fixture",
                                "GIT_AUTHOR_EMAIL": "fixture@example.test", "GIT_COMMITTER_EMAIL": "fixture@example.test"})

            def run(*args):
                return subprocess.run(["git", *args], cwd=root, env=environment, check=True,
                                      stdout=subprocess.PIPE, stderr=subprocess.PIPE).stdout.decode().strip()

            run("init", "-q")
            (root / "example.txt").write_text("public baseline\n")
            run("add", "example.txt")
            run("commit", "-qm", "baseline")
            base = run("rev-parse", "HEAD")
            (root / "example.txt").write_text("/Users/" + "operator-private/work\n")
            run("add", "example.txt")
            run("commit", "-qm", "historical fixture")
            (root / "example.txt").write_text("public candidate\n")
            run("add", "example.txt")
            run("commit", "-qm", "clean current file")
            run("commit", "--allow-empty", "-qm", "metadata only")
            source = run("rev-parse", "HEAD")
            with mock.patch.object(audit, "ROOT", root), mock.patch.dict(os.environ, environment, clear=True):
                report = audit.publication(base, source, None)
                self.assertGreater(report["finding_count"], 0)
                self.assertEqual(report["git_content_gate"], "blocked")
                self.assertFalse(report["public_release_approved"])
                self.assertTrue(any(f["rule"] == "operator-home-path" for f in report["findings"]))
                self.assertNotIn("operator-private", str(report))
                self.assertTrue(report["commit_metadata_scanned"])
                tree = audit.publication(base, source, None, tree_only=True)
                self.assertEqual(tree["scope"], "committed-fork-tree-only")
                self.assertEqual(tree["findings"], [])
                self.assertFalse(tree["commit_metadata_scanned"])
                self.assertFalse(tree["public_release_approved"])
                # Cleaning today's tree cannot make the removed historical
                # content safe, and the two scopes must never be conflated.
                self.assertGreater(report["finding_count"], tree["finding_count"])
                ledger = audit.inventory(base, source, True)
                self.assertEqual(ledger["commit_count"], 3)
                self.assertEqual(ledger["ledger_count"], 3)
                self.assertEqual(ledger["merge_count"], 0)
                self.assertEqual(ledger["commits"][-1]["capabilities"], ["metadata-only"])
                self.assertTrue(all(row["semantic_review"] == "pending" for row in ledger["commits"]))

    def test_synthetic_review_is_exact_and_cannot_suppress_operator_data(self):
        import json
        with tempfile.TemporaryDirectory(prefix="fixture-review-") as directory:
            root = Path(directory)
            environment = {k: v for k, v in os.environ.items() if not k.startswith("GIT_")}
            environment.update({"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.devnull,
                                "GIT_AUTHOR_NAME": "Fixture", "GIT_COMMITTER_NAME": "Fixture",
                                "GIT_AUTHOR_EMAIL": "fixture@example.test", "GIT_COMMITTER_EMAIL": "fixture@example.test"})

            def run(*args):
                return subprocess.run(["git", *args], cwd=root, env=environment, check=True,
                                      stdout=subprocess.PIPE, stderr=subprocess.PIPE).stdout.decode().strip()

            run("init", "-q")
            (root / "README.md").write_text("public baseline\n")
            run("add", ".")
            run("commit", "-qm", "baseline")
            base = run("rev-parse", "HEAD")
            fixture = root / "shape_test.go"
            fixture.write_text('package fixture\nvar value = "http://example:fixture-only@example.test"\n')
            run("add", ".")
            run("commit", "-qm", "synthetic refusal fixture")
            oid = run("rev-parse", "HEAD:shape_test.go")
            (root / "fork").mkdir()
            manifest = root / "fork/publication-fixtures.json"
            row = {"path": "shape_test.go", "object": oid, "rule": "credential-in-url",
                   "reason": "Synthetic rejected URL used only by an isolated shape fixture."}
            manifest.write_text(json.dumps({"version": 1, "examples": [row]}))
            run("add", ".")
            run("commit", "-qm", "review exact synthetic object")
            with mock.patch.object(audit, "ROOT", root), mock.patch.dict(os.environ, environment, clear=True):
                accepted = run("rev-parse", "HEAD")
                report = audit.publication(base, accepted, None)
                self.assertEqual(report["finding_count"], 0)
                self.assertEqual(report["reviewed_example_count"], 1)
                self.assertFalse(report["public_release_approved"])
                fixture.write_text(fixture.read_text() + "// changed fixture\n")
                run("add", ".")
                run("commit", "-qm", "change requires new review")
                with self.assertRaisesRegex(ValueError, "content drift"):
                    audit.publication(base, run("rev-parse", "HEAD"), None)
                row["rule"] = "operator-home-path"
                manifest.write_text(json.dumps({"version": 1, "examples": [row]}))
                run("add", ".")
                run("commit", "-qm", "invalid operator suppression")
                with self.assertRaisesRegex(ValueError, "operator data cannot be suppressed"):
                    audit.publication(base, run("rev-parse", "HEAD"), None)


if __name__ == "__main__":
    unittest.main()
