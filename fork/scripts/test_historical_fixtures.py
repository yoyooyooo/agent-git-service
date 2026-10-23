#!/usr/bin/env python3
"""Exact historical fixture review never suppresses an unreviewed new blob."""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock

SPEC = importlib.util.spec_from_file_location("fork_audit", Path(__file__).with_name("audit.py"))
audit = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(audit)

class HistoricalFixtureTests(unittest.TestCase):
    def test_exact_ancestor_review_cannot_hide_new_content(self):
        with tempfile.TemporaryDirectory(prefix="history-fixture-") as directory:
            root = Path(directory)
            env = {k: v for k, v in os.environ.items() if not k.startswith("GIT_")}
            env.update(GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull,
                       GIT_AUTHOR_NAME="Fixture", GIT_COMMITTER_NAME="Fixture",
                       GIT_AUTHOR_EMAIL="fixture@example.test", GIT_COMMITTER_EMAIL="fixture@example.test")
            def run(*args):
                return subprocess.check_output(["git", *args], cwd=root, env=env, stderr=subprocess.PIPE).decode().strip()
            run("init", "-q")
            (root / "README.md").write_text("baseline\n")
            run("add", "."); run("commit", "-qm", "baseline")
            base = run("rev-parse", "HEAD")
            fixture = root / "negative_test.go"
            fixture.write_text('package fixture\nvar bad = "https://' + 'user:pass@example.test"\n')
            run("add", "."); run("commit", "-qm", "old synthetic fixture")
            ancestor, old = run("rev-parse", "HEAD"), run("rev-parse", "HEAD:negative_test.go")
            fixture.write_text(fixture.read_text() + "// additional assertion\n")
            run("add", "."); run("commit", "-qm", "new synthetic fixture")
            new = run("rev-parse", "HEAD:negative_test.go")
            row = dict(path="negative_test.go", object=new, rule="credential-in-url", reason="Synthetic rejected URL only; no network or genuine credentials.")
            historical = dict(row, object=old, commit=ancestor)
            (root / "fork").mkdir()
            manifest = root / "fork/publication-fixtures.json"
            def commit_manifest(data):
                manifest.write_text(json.dumps(data)); run("add", "."); run("commit", "-qm", "exact fixture review")
                return run("rev-parse", "HEAD")
            data = dict(version=1, examples=[row], historical_examples=[historical])
            source = commit_manifest(data)
            with mock.patch.object(audit, "ROOT", root), mock.patch.dict(os.environ, env, clear=True):
                result = audit.publication(base, source, None)
                self.assertEqual(result["finding_count"], 0)
                self.assertEqual(result["reviewed_example_count"], 2)
                fixture.write_text(fixture.read_text() + "// unreviewed new bytes\n")
                run("add", "."); run("commit", "-qm", "unreviewed change")
                with self.assertRaisesRegex(ValueError, "content drift"):
                    audit.publication(base, run("rev-parse", "HEAD"), None)
                row["object"] = run("rev-parse", "HEAD:negative_test.go")
                historical["rule"] = "operator-home-path"
                with self.assertRaisesRegex(ValueError, "operator data cannot be suppressed"):
                    audit.publication(base, commit_manifest(data), None)
                historical["rule"] = "credential-in-url"
                historical["commit"] = base
                with self.assertRaisesRegex(ValueError, "content drift"):
                    audit.publication(base, commit_manifest(data), None)

if __name__ == "__main__":
    unittest.main()
