#!/usr/bin/env python3
"""Operator privacy rules must catch case variants without exposing matches."""
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

SPEC = importlib.util.spec_from_file_location("fork_audit", Path(__file__).with_name("audit.py"))
audit = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(audit)


class CasefoldPolicyTests(unittest.TestCase):
    def test_operator_literals_match_host_and_account_case_variants(self):
        with tempfile.TemporaryDirectory(prefix="privacy-policy-") as directory:
            policy = Path(directory) / "policy.json"
            policy.write_text(json.dumps({"literal_patterns": {
                "fixture-operator": ["private-operator.example.invalid", "fixture.person"]
            }}))
            rule = audit.privacy_rules(str(policy))["fixture-operator"]
            self.assertIsNotNone(rule.search("PRIVATE-OPERATOR.EXAMPLE.INVALID"))
            self.assertIsNotNone(rule.search("FiXtUrE.PeRsOn"))
            self.assertIsNone(rule.search("unrelated public example"))

    def test_operator_policy_cannot_replace_a_builtin_check(self):
        with tempfile.TemporaryDirectory(prefix="privacy-policy-") as directory:
            policy = Path(directory) / "policy.json"
            policy.write_text(json.dumps({"literal_patterns": {"operator-home-path": ["never-match"]}}))
            with self.assertRaises(ValueError):
                audit.privacy_rules(str(policy))


if __name__ == "__main__":
    unittest.main()
