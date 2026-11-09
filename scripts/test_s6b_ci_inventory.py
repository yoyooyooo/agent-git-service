#!/usr/bin/env python3
import unittest
from pathlib import Path

ROOT=Path(__file__).resolve().parents[1]
class S6bCIInventoryTests(unittest.TestCase):
    def test_portable_entries_and_unverified_native_evidence_are_explicit(self):
        runner=(ROOT/'scripts/upgrade-regression.sh').read_text()
        for name in ('test_upgrade_journal.py','test_upgrade_durable_wiring.py','test_upgrade_activation.py','test_upgrade_resume.py','test_upgrade_restore_intent.py','test_native_factories.py','test_native_systemd_contract.py','test_upgrade_helpers.py','test_upgrade_epoch_chain.py','test_buildinfo_binding.py','test_upgrade_pin_emitter.py','test_routeb_cold_window.py'):
            self.assertIn(name, runner)
        docs=(ROOT/'docs/operations/local-upgrade.md').read_text()
        self.assertIn('UNVERIFIED', docs)
        self.assertIn('launchctl', docs)
        self.assertIn('systemctl', docs)
    def test_ci_runs_sequential_runner_without_dispatch(self):
        workflow=(ROOT/'.github/workflows/ci.yml').read_text()
        self.assertIn('bash scripts/upgrade-regression.sh',workflow)
        self.assertNotIn('dispatch candidate',workflow.lower())
if __name__=='__main__': unittest.main()
