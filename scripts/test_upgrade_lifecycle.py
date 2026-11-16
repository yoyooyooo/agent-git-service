#!/usr/bin/env python3
"""S6.07 lifecycle. Synthetic acceptance only; no service start."""
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock

import upgrade_recovery


class LifecycleBaseline(unittest.TestCase):
    def test_accepted_archive_without_pending_does_not_grant_restart(self):
        """Existing cold fence. No pending marker is not an accepted restart."""
        ledger = Mock()
        with tempfile.TemporaryDirectory(dir=Path.home(), prefix='life-') as directory:
            root = Path(directory).resolve()
            state = root / 'state'
            state.mkdir(mode=0o700)
            accepted = state / 'ags-autodeploy/local-v2/accepted-gen'
            accepted.mkdir(parents=True, mode=0o700)
            (accepted / 'completion.json').write_text('{}')
            # No cold marker: the old function returns true. That is the defect.
            self.assertTrue(upgrade_recovery.startup_permitted(root))
            import upgrade_lifecycle as lifecycle
            with self.assertRaises(lifecycle.LifecycleError) as caught:
                lifecycle.restart_permitted(
                    {'completion_sha256': 'a' * 64, 's4_approval_id': 's4', 'epoch': 1}, None)
            self.assertEqual(caught.exception.code, 'restart_fenced')
            self.assertEqual(ledger.mock_calls, [])


class LifecycleTests(unittest.TestCase):
    def setUp(self):
        import upgrade_lifecycle as lifecycle
        self.lifecycle = lifecycle
        self.accepted = {'completion_sha256': '', 's4_approval_id': 's4', 'epoch': 1,
                         'binary_sha256': 'b' * 64}
        digest = hashlib.sha256(json.dumps(self.accepted, sort_keys=True).encode()).hexdigest()
        self.accepted['completion_sha256'] = digest
        self.effects = []
        self.addCleanup(lambda: self.assertEqual(self.effects, []))

    def test_independent_activation_permits_a_new_generation(self):
        approval = {'purpose': 'lifecycle-activate', 'decision': 'approve',
                    'approval_id': 'life-1', 'completion_sha256': self.accepted['completion_sha256']}
        authority = self.lifecycle.activate(self.accepted, approval)
        result = self.lifecycle.restart_permitted(self.accepted, authority)
        self.assertTrue(result['permitted'])
        self.assertEqual(len(result['generation']), 32)
        again = self.lifecycle.restart_permitted(self.accepted, authority)
        self.assertNotEqual(again['generation'], result['generation'])

    def test_reused_s4_token_and_byte_drift_refuse(self):
        reused = {'purpose': 'lifecycle-activate', 'decision': 'approve', 'approval_id': 's4',
                  'completion_sha256': self.accepted['completion_sha256']}
        with self.assertRaises(self.lifecycle.LifecycleError) as caught:
            self.lifecycle.activate(self.accepted, reused)
        self.assertEqual(caught.exception.exit_code, 77)
        drifted = {**self.accepted, 'binary_sha256': 'c' * 64}
        authority = {'state': 'active', 'completion_sha256': 'd' * 64, 'approval_id': 'life-1'}
        with self.assertRaises(self.lifecycle.LifecycleError):
            self.lifecycle.restart_permitted(drifted, authority)


if __name__ == '__main__':
    unittest.main()
