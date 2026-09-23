#!/usr/bin/env python3
"""Distinguish the published shared-address allocation from operator endpoints."""
import importlib.util
from pathlib import Path
import unittest

SPEC = importlib.util.spec_from_file_location("audit", Path(__file__).with_name("audit.py"))
audit = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(audit)


class PublicRangeTests(unittest.TestCase):
    def test_standard_allocation_is_not_an_operator_endpoint(self):
        rule = audit.privacy_rules(None)["private-mesh-address"]
        self.assertIsNone(rule.search('net.ParseCIDR("100.64.0.0/10")'))
        # Construct synthetic endpoint probes locally; no network is contacted.
        for suffix in ("64.0.0", "64.0.1", "100.100.100", "127.255.254"):
            address = "100." + suffix
            self.assertIsNotNone(rule.search(address))
            self.assertIsNotNone(rule.search(address + "/32"))
        self.assertIsNone(rule.search("192.0.2.1"))


if __name__ == "__main__":
    unittest.main()
