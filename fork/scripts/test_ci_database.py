#!/usr/bin/env python3
"""Exercise CI ownership guards using process doubles; never contact Docker."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).with_name("ci-database.sh")


class CIDatabaseTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="ci-db-guard-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.trace = self.root / "calls.jsonl"
        self.env = {
            "PATH": self.temp.name + os.pathsep + os.environ["PATH"],
            "HOME": self.temp.name, "GITHUB_ACTIONS": "true", "CI": "true",
            "GITHUB_RUN_ID": "100", "GITHUB_RUN_ATTEMPT": "2", "GITHUB_JOB": "shard",
            "FAKE_TRACE": str(self.trace), "FAKE_OWNER": "100:2:shard", "FAKE_EXISTS": "1",
        }
        # A process-boundary double records every attempted action. It cannot
        # forward calls to the real Docker executable.
        docker = self.root / "docker"
        docker.write_text("#!" + sys.executable + "\n" +
            "import json,os,sys\n" +
            "with open(os.environ['FAKE_TRACE'],'a') as f: f.write(json.dumps(sys.argv[1:])+'\\n')\n" +
            "args=sys.argv[1:]\n" +
            "if args[:2]==['container','inspect']: sys.exit(0 if os.environ['FAKE_EXISTS']=='1' else 1)\n" +
            "if args[:1]==['inspect']: print(os.environ['FAKE_OWNER'])\n" +
            "elif args[:1] not in (['run'],['stop'],['logs']): sys.exit(99)\n")
        docker.chmod(0o700)
        mysql = self.root / "mysql"
        mysql.write_text("#!/bin/sh\nexit 0\n")
        mysql.chmod(0o700)

    def run_script(self, command):
        result = subprocess.run(["bash", str(SCRIPT), command], cwd=self.root,
                                env=self.env, capture_output=True, text=True, timeout=10)
        calls = [json.loads(line) for line in self.trace.read_text().splitlines()] if self.trace.exists() else []
        return result, calls

    def test_non_ci_and_invalid_identity_refuse_before_docker(self):
        for key, value in (("CI", "false"), ("GITHUB_ACTIONS", "false"), ("GITHUB_RUN_ID", "../unsafe")):
            with self.subTest(key=key):
                previous = self.env[key]
                self.env[key] = value
                result, calls = self.run_script("stop")
                self.assertEqual(result.returncode, 2)
                self.assertEqual(calls, [])
                self.env[key] = previous

    def test_foreign_container_is_not_stopped(self):
        self.env["FAKE_OWNER"] = "another:job:owner"
        result, calls = self.run_script("stop")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("owner mismatch", result.stderr)
        self.assertFalse(any(call[0] == "stop" for call in calls))

    def test_owned_container_stops_only_exact_name(self):
        result, calls = self.run_script("stop")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual([call for call in calls if call[0] == "stop"],
                         [["stop", "--timeout", "15", "ags-test-100-2-shard"]])

    def test_existing_database_is_not_reused(self):
        result, calls = self.run_script("start")
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any(call[0] == "run" for call in calls))

    def test_start_has_loopback_and_ownership_without_checkout_mount(self):
        self.env["FAKE_EXISTS"] = "0"
        result, calls = self.run_script("start")
        self.assertEqual(result.returncode, 0, result.stderr)
        runs = [call for call in calls if call[0] == "run"]
        self.assertEqual(len(runs), 1)
        args = runs[0]
        self.assertIn("127.0.0.1:45400:4000", args)
        self.assertIn("ags.ci.owner=100:2:shard", args)
        self.assertIn("--path=/tmp/ags-test", args)
        for option in ("--mount", "--volume", "-v"):
            self.assertNotIn(option, args)
        self.assertNotIn(str(self.root), " ".join(args))


if __name__ == "__main__":
    unittest.main()
