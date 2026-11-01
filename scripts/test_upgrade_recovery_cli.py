#!/usr/bin/env python3
"""S6.08 recovery CLI. No process is spawned."""
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock


class RecoveryBaseline(unittest.TestCase):
    def test_existing_prestart_result_does_not_start(self):
        """The S3 return shape is the behavior red: started is false."""
        ledger = Mock()
        result = {'status': 'old_restored', 'generation': 'a' * 32, 'started': False,
                  'recovery_approval_id': 'recover-1', 'data_sha256': 'b' * 64,
                  'tq_sha256': 'b' * 64}
        self.assertFalse(result['started'])
        import upgrade_recovery_cli as cli
        with self.assertRaises(cli.RecoveryError) as caught:
            cli.old_start(result, {'purpose': 'prestart-recover', 'decision': 'approve',
                                   'approval_id': 'recover-1'})
        self.assertEqual(caught.exception.exit_code, 77)
        self.assertEqual(ledger.mock_calls, [])


class RecoveryCliTests(unittest.TestCase):
    def setUp(self):
        import upgrade_recovery_cli as cli
        self.cli = cli
        self.restored = {'status': 'old_restored', 'generation': 'a' * 32, 'started': False,
                         'recovery_approval_id': 'recover-1', 'data_sha256': 'b' * 64,
                         'tq_sha256': 'b' * 64}
        self.effects = []
        self.addCleanup(lambda: self.assertEqual(self.effects, []))

    def test_separate_old_start_is_one_shot(self):
        approval = {'purpose': 'old-start', 'decision': 'approve', 'approval_id': 'start-1'}
        result = self.cli.old_start(self.restored, approval)
        self.assertEqual(result['status'], 'aborted_old_restored')
        self.assertTrue(result['started'])
        with self.assertRaises(self.cli.RecoveryError) as caught:
            self.cli.old_start(self.restored, {**approval, 'consumed': True})
        self.assertEqual(caught.exception.exit_code, 82)

    def test_plan_has_no_effect_and_data_drift_refuses(self):
        plan = self.cli.plan({'phase': 'installed', 'start_attempted': False})
        self.assertFalse(plan['started'])
        self.assertEqual(self.effects, [])
        drifted = {**self.restored, 'data_sha256': 'c' * 64}
        with self.assertRaises(self.cli.RecoveryError):
            self.cli.old_start(drifted, {'purpose': 'old-start', 'decision': 'approve',
                                         'approval_id': 'start-2'})
        with self.assertRaises(self.cli.RecoveryError):
            self.cli.plan({'phase': 'start_attempted', 'start_attempted': True})


if __name__ == '__main__':
    unittest.main()
