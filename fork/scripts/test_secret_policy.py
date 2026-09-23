#!/usr/bin/env python3
"""Prove the exact scanner exception cannot hide another credential."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

CONFIG = Path(__file__).resolve().parents[1] / "gitleaks.toml"


class SecretPolicyTests(unittest.TestCase):
    def test_exception_matches_only_the_validated_code_line(self):
        binary = shutil.which(os.environ.get("GITLEAKS_BIN", "gitleaks"))
        self.assertIsNotNone(binary, "gitleaks is required; do not silently skip this gate")
        with tempfile.TemporaryDirectory(prefix="fork-secret-policy-") as directory:
            root = Path(directory)
            target = root / "internal/snapshotstore/retention.go"
            target.parent.mkdir(parents=True)
            line = '\tif !item.IsDir() || len(key) != 64 || strings.Trim(key, "0123456789abcdef") != "" {\n'
            target.write_text(line)

            def scan():
                result = subprocess.run([binary, "dir", str(root), "--config", str(CONFIG),
                    "--gitleaks-ignore-path", os.devnull, "--ignore-gitleaks-allow", "--redact=100",
                    "--no-banner", "--log-level", "error", "--report-format", "json", "--report-path", "-"],
                    capture_output=True, text=True, timeout=30)
                self.assertIn(result.returncode, (0, 1), "scanner execution failed")
                return result.returncode, json.loads(result.stdout or "[]")

            exit_code, findings = scan()
            self.assertEqual(exit_code, 0)
            self.assertEqual(findings, [])
            # Construct a format-valid but deliberately synthetic token. Never
            # contact a provider; the test only exercises the local scanner.
            canary = "ghp_" + "Ab3dEf7hJk9mNp2rSt4vWx6yZa8bCd1eFg3h"
            target.write_text(line + 'const apiToken = "' + canary + '"\n')
            exit_code, findings = scan()
            self.assertEqual(exit_code, 1, "exact-line exception hid another credential")
            self.assertTrue(any(row.get("RuleID") == "github-pat" for row in findings))
            generic = hashlib.sha256(b"local scanner regression canary").hexdigest()
            target.write_text(line + 'const api_key = "' + generic + '"\n')
            exit_code, findings = scan()
            self.assertEqual(exit_code, 1, "exception hid another finding of the same rule")
            self.assertTrue(any(row.get("RuleID") == "generic-api-key" for row in findings))


if __name__ == "__main__":
    unittest.main()
