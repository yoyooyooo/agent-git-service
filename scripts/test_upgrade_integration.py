#!/usr/bin/env python3
"""S6 integration: dependency closure and fail-closed effect boundaries."""
import ast
import hashlib
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock

import upgrade_bundle
from test_upgrade_wireup import snapshot

HERE = Path(__file__).resolve().parent


class IntegrationTests(unittest.TestCase):
    def test_real_bundle_contains_all_local_imports_and_fixed_entries(self):
        with tempfile.TemporaryDirectory(dir=Path.home(), prefix='s6-integration-') as directory:
            root = Path(directory)
            staging = root / 'bundle'
            upgrade_bundle.build(HERE, staging)
            upgrade_bundle.verify(staging)
            required = {'upgrade', 'ags-python.sh', 'ags-runtime.py', 'upgrade_wireup.py',
                        'upgrade_full_restore.py', 'upgrade_handoff.py', 'upgrade_pin.py',
                        'upgrade_policy.json', 'release-source.json'}
            missing = required - {p.name for p in staging.iterdir()}
            for path in staging.glob('*.py'):
                tree = ast.parse(path.read_text())
                for node in ast.walk(tree):
                    names = [a.name for a in node.names] if isinstance(node, ast.Import) else (
                        [node.module] if isinstance(node, ast.ImportFrom) and node.module else [])
                    for name in names:
                        local = name.split('.')[0] + '.py'
                        if (HERE / local).exists() and not (staging / local).exists():
                            missing.add(local)
            self.assertEqual(missing, set(), 'staged bundle must be dependency complete')
            ledger = Mock()
            before = snapshot(staging)
            self.assertFalse(upgrade_bundle.verify(staging)['installable'])
            self.assertEqual(snapshot(staging), before)
            self.assertEqual(ledger.mock_calls, [])

    def test_unprovisioned_native_factories_refuse_instead_of_returning_noop_effects(self):
        import upgrade_adapter_launchd
        import upgrade_adapter_systemd
        import upgrade_backup_contract as c
        ledger = Mock()
        with tempfile.TemporaryDirectory(dir=Path.home(), prefix='s6-factory-') as directory:
            root = Path(directory)
            before = snapshot(root)
            for module in (upgrade_adapter_launchd, upgrade_adapter_systemd):
                with self.subTest(module=module.__name__), self.assertRaisesRegex(
                        c.ContractError, 'native_adapter_unavailable'):
                    module.create(root, {})
                self.assertEqual(snapshot(root), before)
                self.assertEqual(ledger.mock_calls, [])

    def test_shared_runner_covers_every_upgrade_suite(self):
        runner = (HERE / 'upgrade-regression.sh').read_text()
        for test in HERE.glob('test_upgrade*.py'):
            self.assertIn('scripts/' + test.name, runner)
        manifest = (HERE / 'regression_gate.list').read_text()
        workflow = (HERE.parent / '.github/workflows/ci.yml').read_text()
        self.assertIn('scripts/upgrade-regression.sh', manifest)
        self.assertIn('bash scripts/upgrade-regression.sh', workflow)

    def test_public_policy_and_direct_install_remain_closed(self):
        import json
        import upgrade
        import release_install_v2
        self.assertEqual(json.loads((HERE / 'upgrade_policy.json').read_text()),
            {'schema': 'ags.upgrade-policy.v2', 'deployments': {}, 'migration_admissions': []})
        ledger = Mock()
        for host in ('mini', 'imile-win'):
            with self.assertRaises(upgrade.Failure) as caught:
                upgrade.require_apply_contracts(host)
            self.assertEqual(caught.exception.exit_code, 83)
        with self.assertRaises(release_install_v2.InstallClosed) as caught:
            release_install_v2.require_install_contracts()
        self.assertEqual(caught.exception.exit_code, 83)
        self.assertEqual(ledger.mock_calls, [])


if __name__ == '__main__':
    unittest.main()
