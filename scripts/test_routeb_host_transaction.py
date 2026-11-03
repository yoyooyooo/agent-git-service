#!/usr/bin/env python3
"""M5 host transaction entry. Synthetic fixtures only; no production root or label.

A plan without a private package must have no effects. The default rehearsal
entry stays isolated and still refuses a real production root.
"""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent
ENTRY = ROOT / 'ags-maintenance-transaction'


class HostModeTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='routeb-host-txn-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)

    def test_host_plan_without_package_has_no_effects(self):
        completed = subprocess.run(
            [str(ENTRY), '--mode', 'host', 'plan', '--host', 'mini'],
            cwd=self.root, check=False, text=True, capture_output=True,
        )
        self.assertEqual(completed.returncode, 78, completed.stderr)
        self.assertIn('package_missing', completed.stderr)
        self.assertNotIn('effects', completed.stdout)
        self.assertEqual(list(self.root.iterdir()), [])

    def test_default_mode_still_refuses_a_production_shaped_root(self):
        home = self.root / 'home'
        home.mkdir()
        production = home / '.ags'
        production.mkdir()
        env = os.environ.copy()
        env['HOME'] = str(home)
        completed = subprocess.run(
            [str(ENTRY), 'plan', '--host', 'mini', '--root', str(production),
             '--candidate', str(self.root / 'missing'), '--window', str(self.root / 'missing'),
             '--restore', str(self.root / 'restore'), '--manager-dir', str(self.root / 'manager'),
             '--audit', str(self.root / 'audit'), '--external-contract', str(self.root / 'contract')],
            cwd=self.root, check=False, text=True, capture_output=True, env=env,
        )
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn('production_root_refused', completed.stderr)
        self.assertFalse((production / 'state').exists())

    def test_host_mode_rejects_manager_dir_and_fault_hook(self):
        completed = subprocess.run(
            [str(ENTRY), '--mode', 'host', 'run', '--execute', '--manager-dir', str(self.root),
             '--fault-after', 'stopped'],
            cwd=self.root, check=False, text=True, capture_output=True,
        )
        self.assertNotEqual(completed.returncode, 0)
        self.assertTrue('manager_dir_refused' in completed.stderr or 'fault_hook_refused' in completed.stderr)
        self.assertEqual(list(self.root.iterdir()), [])


if __name__ == '__main__':
    unittest.main()
