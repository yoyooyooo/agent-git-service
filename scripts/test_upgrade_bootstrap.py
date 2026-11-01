#!/usr/bin/env python3
"""S6.02 launcher bootstrap. Same old binary only; no business upgrade."""
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch

HERE = Path(__file__).resolve().parent


def old_receipt(binary):
    return {'schema': 'ags.runtime.v0', 'pid': 1, 'started_at': '2000-01-01T00:00:00+00:00',
            'exit_code': 0, 'binary_sha256': hashlib.sha256(binary).hexdigest()}


class BootstrapBaseline(unittest.TestCase):
    def test_old_launcher_receipt_cannot_enter_s3(self):
        """Existing begin() seam. A pid/time receipt has no S3 generation."""
        import upgrade_recovery as recovery
        ledger = Mock()
        with tempfile.TemporaryDirectory(dir=Path.home(), prefix='bootstrap-') as directory:
            root = Path(directory).resolve()
            window = recovery.ColdWindow.__new__(recovery.ColdWindow)
            window.owner = object()
            window.root = root
            window.state = root
            window.pending = root / 'pending'
            window.binding = {}
            window.backup_binding = {}
            window.group = {}
            window.trash = ledger.trash
            window.journal = None
            legacy = old_receipt(b'old')
            if (HERE / 'upgrade_bootstrap.py').exists():
                import upgrade_bootstrap as bootstrap
                with self.assertRaises(recovery.Failure) as caught:
                    bootstrap.admit_legacy(legacy, ledger=ledger)
                self.assertEqual(caught.exception.code, 'stop_generation_unknown')
            else:
                with patch.object(recovery, 'validate_binding'), \
                        patch.object(recovery, 'validate_group'), \
                        patch.object(recovery, 'read_original_binding'), \
                        patch.object(recovery, 'private_json', return_value=(legacy, 'digest')), \
                        patch.object(recovery, 'durable_bytes', ledger.write), \
                        patch.object(recovery, 'replace_json', ledger.write):
                    with self.assertRaises(recovery.ContractError) as caught:
                        window.begin()
                    self.assertEqual(str(caught.exception), 'identifier_invalid')
            self.assertEqual(ledger.mock_calls, [])


class BootstrapTests(unittest.TestCase):
    def setUp(self):
        import upgrade_bootstrap as bootstrap
        self.bootstrap = bootstrap
        self.tmp = tempfile.TemporaryDirectory(dir=Path.home(), prefix='bootstrap-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        self.ledger = Mock()
        self.binary = b'old-binary-fixture'
        self.addCleanup(lambda: self.assertEqual(self.ledger.mock_calls, []))

    def test_transition_same_binary_then_real_receipt_is_admitted(self):
        legacy = old_receipt(self.binary)
        plan = self.bootstrap.plan(self.root, legacy, binary=self.binary)
        self.assertEqual(plan['binary_sha256'], hashlib.sha256(self.binary).hexdigest())
        self.assertFalse(plan['business_upgrade'])
        evidence = self.bootstrap.complete(
            plan, binary=self.binary,
            receipt={'schema': 'ags.runtime.v1', 'generation': 'a' * 32,
                     'exit_code': 0, 'forced_kill': False, 'stop_requested': True,
                     'supervisor_exited': True, 'process_group_empty': True,
                     'binary_sha256': hashlib.sha256(self.binary).hexdigest()})
        self.assertEqual(evidence['generation'], 'a' * 32)
        self.assertFalse(evidence['business_upgrade'])

    def test_each_crash_fence_refuses_and_changed_binary_is_not_bootstrap(self):
        legacy = old_receipt(self.binary)
        for phase in ('observe', 'retain', 'restart', 'receipt'):
            with self.subTest(phase=phase):
                with self.assertRaises(self.bootstrap.BootstrapError) as caught:
                    self.bootstrap.complete_partial(self.root, legacy, phase=phase, ledger=self.ledger)
                self.assertEqual(caught.exception.exit_code, 82)
        with self.assertRaises(self.bootstrap.BootstrapError) as caught:
            self.bootstrap.plan(self.root, legacy, binary=b'different-binary')
        self.assertEqual(caught.exception.exit_code, 83)


if __name__ == '__main__':
    unittest.main()
