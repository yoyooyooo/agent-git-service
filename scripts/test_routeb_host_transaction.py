#!/usr/bin/env python3
"""Host CLI admission boundaries. No production manager or filesystem effects."""
import contextlib
import io
import os
from pathlib import Path
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import routeb_host_transaction as host

ROOT = Path(__file__).resolve().parent
ENTRY = ROOT / 'ags-maintenance-transaction'


class HostModeTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='routeb-host-txn-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)

    def test_host_plan_without_package_has_no_effects(self):
        completed = subprocess.run([str(ENTRY), '--mode', 'host', 'plan', '--host', 'mini'],
                                   cwd=self.root, check=False, text=True, capture_output=True)
        self.assertEqual(completed.returncode, 78, completed.stderr)
        self.assertIn('package_missing', completed.stderr)
        self.assertNotIn('effects', completed.stdout)
        self.assertEqual(list(self.root.iterdir()), [])

    def test_default_mode_still_refuses_a_production_shaped_root(self):
        home = self.root / 'home'
        home.mkdir()
        production = home / '.ags'
        production.mkdir()
        env = dict(os.environ, HOME=str(home))
        completed = subprocess.run(
            [str(ENTRY), 'plan', '--host', 'mini', '--root', str(production),
             '--candidate', str(self.root / 'missing'), '--window', str(self.root / 'missing'),
             '--restore', str(self.root / 'restore'), '--manager-dir', str(self.root / 'manager'),
             '--audit', str(self.root / 'audit'), '--external-contract', str(self.root / 'contract')],
            cwd=self.root, check=False, text=True, capture_output=True, env=env)
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn('production_root_refused', completed.stderr)
        self.assertFalse((production / 'state').exists())

    def test_host_mode_rejects_manager_dir_and_fault_hook(self):
        completed = subprocess.run(
            [str(ENTRY), '--mode', 'host', 'run', '--execute', '--manager-dir', str(self.root),
             '--fault-after', 'stopped'], cwd=self.root, check=False, text=True, capture_output=True)
        self.assertNotEqual(completed.returncode, 0)
        self.assertTrue('manager_dir_refused' in completed.stderr or 'fault_hook_refused' in completed.stderr)
        self.assertEqual(list(self.root.iterdir()), [])

    def test_run_without_execute_never_loads_or_calls_a_capability(self):
        args = host.parse(['run', '--host', 'mini', '--package', '/missing', '--approval', '/missing'])
        with patch.object(host, 'load') as loader, patch.object(host, 'HostExecution') as effects:
            with contextlib.redirect_stderr(io.StringIO()) as error:
                self.assertEqual(host.dispatch(args), 78)
            self.assertIn('execute_required', error.getvalue())
            loader.assert_not_called()
            effects.assert_not_called()

    def test_a_blocked_native_plan_cannot_enter_execution(self):
        args = host.parse(['run', '--execute', '--host', 'mini', '--package', '/private/package',
                           '--approval', '/private/approval'])
        result = {'execution_allowed': False, 'blockers': ['native_full_closure_required'], 'effects': []}
        with patch.object(host, 'package_for', return_value=SimpleNamespace()), \
                patch.object(host, 'preflight', return_value=result), \
                patch.object(host, 'HostExecution') as effects, contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(host.dispatch(args), 78)
            effects.assert_not_called()

    def test_native_entry_refuses_an_untrusted_import_directory_before_import(self):
        entry = self.root / 'routeb_host_transaction.py'
        entry.write_bytes((ROOT / entry.name).read_bytes())
        (self.root / 'upgrade.py').write_text('raise RuntimeError("untrusted import")\n')
        completed = subprocess.run([sys.executable, '-I', '-B', str(entry), 'plan', '--host', 'mini'],
                                   cwd=self.root, text=True, capture_output=True)
        self.assertEqual(completed.returncode, 78, completed.stderr)
        self.assertIn('frozen_host_entry_required', completed.stderr)
        self.assertNotIn('untrusted import', completed.stderr)

    def test_host_entry_binds_the_actual_transaction_interpreter(self):
        args = SimpleNamespace(package='/private/package', approval='/private/approval', host='mini')
        package = SimpleNamespace(host=SimpleNamespace(bundle=ROOT),
                                  profile={'interpreter': {'path': str(self.root / 'another-python')}})
        with patch.object(host, 'load', return_value=package), \
                patch.object(host.sys, 'flags', SimpleNamespace(isolated=True)):
            with self.assertRaisesRegex(host.ContractError, 'host_interpreter_mismatch'):
                host.package_for(args)
            package.profile['interpreter']['path'] = str(Path(sys.executable).resolve())
            self.assertIs(host.package_for(args), package)


if __name__ == '__main__':
    unittest.main()
