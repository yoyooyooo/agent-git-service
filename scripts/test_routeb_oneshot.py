#!/usr/bin/env python3
"""One-shot launcher authority. Suspended authority must not spawn."""
import unittest
from unittest.mock import Mock

import upgrade_recovery as recovery


class OneShotTests(unittest.TestCase):
    def test_inactive_authority_does_not_spawn(self):
        runner = Mock(return_value={'pid': 9, 'boot_id': 'boot', 'generation': 'g'})
        with self.assertRaises(recovery.Failure) as caught:
            recovery.launcher_spawn_authorized(
                {'state': 'suspended', 'epoch': 1, 'binding_sha256': 'a' * 64},
                runner, epoch=1)
        self.assertEqual(caught.exception.code, 'restart_fenced')
        self.assertEqual(runner.call_count, 0)

    def test_active_authority_spawns_once(self):
        runner = Mock(return_value={'pid': 9, 'boot_id': 'boot', 'generation': 'g'})
        authority = {'state': 'active', 'epoch': 2, 'binding_sha256': 'a' * 64}
        first = recovery.launcher_spawn_authorized(authority, runner, epoch=2)
        self.assertEqual(first['status'], 'spawned')
        self.assertEqual(runner.call_count, 1)
        with self.assertRaises(recovery.Failure) as caught:
            recovery.launcher_spawn_authorized(authority, runner, epoch=2)
        self.assertEqual(caught.exception.code, 'oneshot_consumed')
        self.assertEqual(runner.call_count, 1)

    def test_lost_start_is_not_retried(self):
        runner = Mock(side_effect=OSError('lost'))
        authority = {'state': 'active', 'epoch': 3, 'binding_sha256': 'b' * 64}
        with self.assertRaises(recovery.Failure):
            recovery.launcher_spawn_authorized(authority, runner, epoch=3)
        with self.assertRaises(recovery.Failure) as caught:
            recovery.launcher_spawn_authorized(authority, runner, epoch=3)
        self.assertEqual(caught.exception.code, 'oneshot_consumed')
        self.assertEqual(runner.call_count, 1)


if __name__ == '__main__':
    unittest.main()
