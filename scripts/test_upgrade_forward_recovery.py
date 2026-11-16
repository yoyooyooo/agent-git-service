#!/usr/bin/env python3
"""S6.09 same-version recovery. No process is spawned."""
import unittest
from unittest.mock import Mock

import upgrade_recovery


class ForwardBaseline(unittest.TestCase):
    def test_missing_pid_is_not_a_restart(self):
        """Existing reconcile contract: no PID does not authorize another start."""
        ledger = Mock()
        self.assertIn('missing PID', upgrade_recovery.reconcile.__doc__)
        import upgrade_forward_recovery as forward
        with self.assertRaises(forward.ForwardError) as caught:
            forward.same_version({'outcome': 'unknown', 'pid_missing': True,
                                  'closed': True, 'writers': [], 'binary_sha256': 'a' * 64,
                                  'start_token': 'token-1'},
                                 {'purpose': 'same-version-restart', 'decision': 'approve',
                                  'approval_id': 'new', 'binary_sha256': 'a' * 64})
        self.assertEqual(caught.exception.exit_code, 82)
        self.assertEqual(ledger.mock_calls, [])


class ForwardTests(unittest.TestCase):
    def setUp(self):
        import upgrade_forward_recovery as forward
        self.forward = forward
        self.incident = {'outcome': 'exited', 'pid_missing': False, 'closed': True,
                         'writers': [], 'binary_sha256': 'a' * 64, 'start_token': 'token-1'}
        self.approval = {'purpose': 'same-version-restart', 'decision': 'approve',
                         'approval_id': 'restart-1', 'binary_sha256': 'a' * 64}
        self.effects = []
        self.addCleanup(lambda: self.assertEqual(self.effects, []))

    def test_closed_incident_mints_a_new_generation_without_spawn(self):
        result = self.forward.same_version(self.incident, self.approval)
        self.assertFalse(result['started'])
        self.assertEqual(len(result['generation']), 32)
        self.assertEqual(result['binary_sha256'], 'a' * 64)

    def test_open_writer_reused_token_and_new_candidate_refuse(self):
        with self.assertRaises(self.forward.ForwardError) as caught:
            self.forward.same_version({**self.incident, 'writers': [1]}, self.approval)
        self.assertEqual(caught.exception.exit_code, 83)
        with self.assertRaises(self.forward.ForwardError) as caught:
            self.forward.same_version(self.incident, {**self.approval, 'approval_id': 'token-1'})
        self.assertEqual(caught.exception.exit_code, 77)
        with self.assertRaises(self.forward.ForwardError):
            self.forward.same_version(self.incident, {**self.approval, 'binary_sha256': 'b' * 64})


if __name__ == '__main__':
    unittest.main()
