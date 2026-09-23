#!/usr/bin/env python3
"""Regression tests for object-only public generation construction."""
from __future__ import annotations

import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock

SPEC = importlib.util.spec_from_file_location("generation", Path(__file__).with_name("generation.py"))
generation = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(generation)


class GenerationTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="generation-fixture-")
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name) / "checkout"
        self.root.mkdir()
        self.environment = {k: v for k, v in os.environ.items() if not k.startswith("GIT_")}
        self.environment.update({"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.devnull,
                                 "GIT_AUTHOR_NAME": "Private Fixture", "GIT_COMMITTER_NAME": "Private Fixture",
                                 "GIT_AUTHOR_EMAIL": "private-fixture@example.test", "GIT_COMMITTER_EMAIL": "private-fixture@example.test"})
        self.patchenv = mock.patch.dict(os.environ, self.environment, clear=True)
        self.patchenv.start()
        self.addCleanup(self.patchenv.stop)
        self.patchroot = mock.patch.object(generation, "ROOT", self.root)
        self.patchroot.start()
        self.addCleanup(self.patchroot.stop)
        self.run_git("init", "-q")
        # The fixture asserts the planner itself performs no writes. Disable
        # automatic maintenance in this disposable repo so detached Git GC
        # cannot mutate object storage or race TemporaryDirectory cleanup.
        self.run_git("config", "gc.auto", "0")
        self.run_git("config", "gc.autoDetach", "false")
        self.run_git("config", "maintenance.auto", "false")
        (self.root / "source.go").write_text("package sample\nfunc Value() int { return 42 }\n")
        self.run_git("add", ".")
        self.run_git("commit", "-qm", "public upstream")
        self.base = self.run_git("rev-parse", "HEAD")
        (self.root / "fork").mkdir()
        (self.root / "fork/UPSTREAM_BASELINE").write_text(self.base + "\n")
        (self.root / "source_test.go").write_text('package sample\nvar name = "operator-private-fixture"\n')
        (self.root / "private-notes.md").write_text("operator-private-fixture\n")
        self.run_git("add", ".")
        self.run_git("commit", "-qm", "private intermediate narrative")
        self.source = self.run_git("rev-parse", "HEAD")
        self.policy = {"version": 1, "source": self.source, "baseline": self.base,
                       "author": {"name": "Public Maintainer", "email": "fixture@users.noreply.github.com"},
                       "externalize": ["private-notes.md"],
                       "substitutions": [{"from": "operator-private-fixture", "to": "example-fixture"}]}

    def run_git(self, *args):
        return subprocess.run(["git", *args], cwd=self.root, env=self.environment,
                              check=True, capture_output=True).stdout.decode().strip()

    def test_plan_changes_no_objects_refs_or_checkout(self):
        before = self.run_git("show-ref")
        count = self.run_git("count-objects", "-v")
        summary, target, transformed = generation.plan(self.source, self.base, self.policy)
        self.assertEqual(before, self.run_git("show-ref"))
        self.assertEqual(count, self.run_git("count-objects", "-v"))
        self.assertEqual(self.run_git("status", "--porcelain"), "")
        self.assertFalse(summary["public_release_approved"])
        self.assertEqual(summary["production_go_files_preserved"], 1)
        self.assertEqual(target["source.go"], generation.entries(self.source)["source.go"])
        self.assertNotIn("private-notes.md", target)
        self.assertTrue(transformed)

    def test_create_strips_private_ancestry_preserves_code_and_never_moves_ref(self):
        summary, target, transformed = generation.plan(self.source, self.base, self.policy)
        result = generation.create(summary, target, transformed, self.policy, "refs/heads/fork/main.20260101")
        head = result["generation"]
        self.assertEqual(self.run_git("rev-parse", "HEAD"), self.source)
        self.assertEqual(self.run_git("status", "--porcelain"), "")
        self.assertEqual(self.run_git("merge-base", self.source, head), self.base)
        self.assertEqual(generation.entries(head), target)
        self.assertEqual(self.run_git("show", head + ":source.go"), self.run_git("show", self.source + ":source.go"))
        self.assertIn("example-fixture", self.run_git("show", head + ":source_test.go"))
        history = self.run_git("log", "--format=fuller", self.base + ".." + head)
        self.assertNotIn("private-fixture@example.test", history)
        self.assertNotIn("private intermediate narrative", history)
        self.assertIn("fixture@users.noreply.github.com", history)
        with self.assertRaises(generation.Rejected):
            generation.create(summary, target, transformed, self.policy, "refs/heads/fork/main.20260101")
        self.assertEqual(self.run_git("rev-parse", "fork/main.20260101"), head)

    def test_closed_policy_disallows_production_deletion_and_drift(self):
        policy_file = Path(self.tmp.name) / "policy.json"
        policy_file.write_text(json.dumps(self.policy))
        policy_file.chmod(0o600)
        generation.load_policy(policy_file, self.source, self.base)
        for changes in ({"externalize": ["source.go"]}, {"source": self.base}, {"unexpected": True}):
            policy_file.write_text(json.dumps({**self.policy, **changes}))
            with self.assertRaises(generation.Rejected):
                generation.load_policy(policy_file, self.source, self.base)
        policy_file.write_text(json.dumps(self.policy))
        policy_file.chmod(0o644)
        with self.assertRaises(generation.Rejected):
            generation.load_policy(policy_file, self.source, self.base)

    def test_credentials_and_arbitrary_refs_cannot_enter_generation_metadata(self):
        summary, target, transformed = generation.plan(self.source, self.base, self.policy)
        for ref in ("refs/heads/main", "refs/tags/release", "refs/heads/fork/../bad"):
            with self.assertRaises(generation.Rejected):
                generation.create(summary, target, transformed, self.policy, ref)
        policy_file = Path(self.tmp.name) / "policy.json"
        policy_file.write_text(json.dumps({**self.policy, "author": {"name": "Maintainer", "email": "private@example.test"}}))
        policy_file.chmod(0o600)
        with self.assertRaises(generation.Rejected):
            generation.load_policy(policy_file, self.source, self.base)


if __name__ == "__main__":
    unittest.main()
