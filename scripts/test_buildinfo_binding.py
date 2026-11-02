#!/usr/bin/env python3
from pathlib import Path
import unittest

ROOT=Path(__file__).resolve().parents[1]
class BuildInfoBindingTests(unittest.TestCase):
    def test_exact_source_builder_stamps_all_four_identity_values(self):
        text=(ROOT/'scripts/build-exact-source.sh').read_text()
        for value in ('server.gitSHA','internal/buildinfo.Version','internal/buildinfo.Revision','internal/buildinfo.Tree'):
            self.assertIn(value,text)
    def test_docker_builder_stamps_all_four_identity_values(self):
        text=(ROOT/'Dockerfile').read_text()
        for value in ('server.gitSHA','internal/buildinfo.Version','internal/buildinfo.Revision','internal/buildinfo.Tree'):
            self.assertIn(value,text)
if __name__=='__main__': unittest.main()
