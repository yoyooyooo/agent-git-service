#!/usr/bin/env python3
import tempfile
from pathlib import Path
import unittest

import upgrade_recovery_cli
import upgrade_state


class ResumeTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(dir=Path.home(), prefix='s6b05-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve(); (self.root / 'state').mkdir(mode=0o700)
        self.restored = {'status': 'old_restored', 'started': False,
                         'recovery_approval_id': 'recover-1', 'data_sha256': 'a' * 64,
                         'tq_sha256': 'a' * 64}
        self.approval = {'purpose': 'old-start', 'decision': 'approve', 'approval_id': 'start-1'}

    def test_old_start_persists_intent_and_observation(self):
        observed = {'generation': 'g-1', 'pid': 42, 'boot_id': 'boot-1', 'started': True}
        result = upgrade_recovery_cli.old_start_durable(self.root, self.restored, self.approval,
                                                        lambda: observed)
        self.assertEqual(result, observed)
        rows = upgrade_state.load_verified(self.root)['events']
        self.assertEqual([row['kind'] for row in rows], ['start_intent', 'start_observed'])
        self.assertEqual(upgrade_state.load_verified(self.root)['projection']['pending_effects'], [])

    def test_retry_reads_back_the_same_generation(self):
        calls = {'n': 0}

        def observer():
            calls['n'] += 1
            if calls['n'] == 1:
                raise RuntimeError('crash-before-observe')
            return {'generation': 'g-retry', 'pid': 9, 'boot_id': 'boot', 'started': True}

        with self.assertRaises(RuntimeError):
            upgrade_recovery_cli.old_start_durable(self.root, self.restored, self.approval, observer)
        first = upgrade_state.load_verified(self.root)['events'][0]['effect']['desired_after']
        result = upgrade_recovery_cli.old_start_durable(self.root, self.restored, self.approval, observer)
        self.assertEqual(result['generation'], 'g-retry')
        rows = upgrade_state.load_verified(self.root)['events']
        self.assertEqual(rows[0]['effect']['desired_after'], first)
        self.assertEqual([row['kind'] for row in rows], ['start_intent', 'start_observed'])

    def test_unknown_observation_is_fenced_and_not_replayed(self):
        with self.assertRaises(upgrade_recovery_cli.RecoveryError) as caught:
            upgrade_recovery_cli.old_start_durable(self.root, self.restored, self.approval,
                                                   lambda: None)
        self.assertEqual(caught.exception.code, 'start_observation_unknown')
        rows = upgrade_state.load_verified(self.root)['events']
        self.assertEqual([row['kind'] for row in rows], ['start_intent', 'start_blocked'])


if __name__ == '__main__':
    unittest.main()
