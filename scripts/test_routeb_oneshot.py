#!/usr/bin/env python3
"""One-shot launcher authority. Suspended authority must not spawn."""
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import Mock

import upgrade_lifecycle
import upgrade_recovery as recovery
import upgrade_state


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

    def _root(self):
        directory = tempfile.TemporaryDirectory(dir=Path.home(), prefix='routeb-oneshot-', ignore_cleanup_errors=True)
        self.addCleanup(directory.cleanup)
        root = Path(directory.name).resolve()
        root.chmod(0o700)
        upgrade_lifecycle.activate_durable(
            root,
            {'completion_sha256': 'c' * 64, 's4_approval_id': 's4', 'epoch': 17,
             'host_instance': 'fixture-host'},
            {'purpose': 'lifecycle-activate', 'decision': 'approve',
             'approval_id': 'life-oneshot', 'completion_sha256': 'c' * 64,
             'host_instance': 'fixture-host'})
        return root

    def test_oneshot_survives_two_processes(self):
        """Two interpreters sharing one journal spawn the business process once."""
        root = self._root()
        counter = root / 'spawn.log'
        code = """import sys
from pathlib import Path
sys.path.insert(0, sys.argv[1])
import upgrade_recovery as recovery
counter = Path(sys.argv[3])
def runner(argv):
    with counter.open('a') as handle:
        handle.write('spawn\\n')
    return {'pid': 999, 'boot_id': 'synthetic', 'generation': 'g'}
try:
    print(recovery.launcher_spawn_authorized(Path(sys.argv[2]), runner, epoch=17))
except recovery.Failure as error:
    print(error.code)
"""
        for _ in range(2):
            result = subprocess.run(
                [sys.executable, '-B', '-c', code, str(Path(recovery.__file__).parent), str(root), str(counter)],
                capture_output=True, text=True, check=False)
            self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(counter.is_file())
        self.assertEqual(counter.read_text().splitlines(), ['spawn'])
        loaded = upgrade_state.load_verified(root)
        self.assertEqual([row['kind'] for row in loaded['events'] if 'spawn' in row['kind']],
                         ['spawn_intent', 'generation_spawned'])

    def test_lost_manager_start_must_not_be_retried(self):
        """A response lost after the effect is durable; a new object does not send it again."""
        root = self._root()
        calls = []

        def runner(argv):
            calls.append(list(argv))
            raise TimeoutError('synthetic lost response after effect')

        for _ in range(2):
            with self.assertRaises(recovery.Failure) as caught:
                recovery.launcher_spawn_authorized(root, runner, epoch=17)
            self.assertIn(caught.exception.code, ('oneshot_consumed', 'start_observation_unknown'))
        self.assertEqual(len(calls), 1)
        loaded = upgrade_state.load_verified(root)
        self.assertTrue(any(row['kind'] == 'spawn_intent' for row in loaded['events']))
        self.assertIn('spawn-17', loaded['projection']['pending_effects'])

    def test_lost_manager_response_is_not_sent_again(self):
        """Manager.start consumes its durable token before launchctl, including a lost response."""
        import upgrade_adapter_launchd as adapter
        root = self._root()
        calls = []

        def runner(argv):
            calls.append(list(argv))
            raise TimeoutError('synthetic lost response after effect')

        profile = {'domain': 'gui/501', 'label': 'com.example.synthetic',
                   'plist': '/tmp/synthetic.plist', 'root': str(root), 'epoch': 17}
        for _ in range(2):
            with self.assertRaises((TimeoutError, adapter.AdapterError, recovery.Failure)):
                adapter.Manager(profile, runner=runner).start()
        self.assertEqual(len(calls), 1)

    def test_crash_after_intent_is_not_retried(self):
        """A child that dies after the durable intent cannot be started by the parent."""
        root = self._root()
        counter = root / 'crash.log'
        code = """import os, sys
from pathlib import Path
sys.path.insert(0, sys.argv[1])
import upgrade_recovery as recovery
counter = Path(sys.argv[3])
def runner(argv):
    with counter.open('a') as handle:
        handle.write('spawn\\n')
        handle.flush()
        os.fsync(handle.fileno())
    os._exit(91)
try:
    recovery.launcher_spawn_authorized(Path(sys.argv[2]), runner, epoch=17)
except recovery.Failure:
    pass
"""
        crashed = subprocess.run(
            [sys.executable, '-B', '-c', code, str(Path(recovery.__file__).parent), str(root), str(counter)],
            capture_output=True, text=True, check=False)
        self.assertNotEqual(crashed.returncode, 0)
        self.assertEqual(counter.read_text().splitlines(), ['spawn'])
        calls = []
        with self.assertRaises(recovery.Failure) as caught:
            recovery.launcher_spawn_authorized(root, lambda argv: calls.append(argv), epoch=17)
        self.assertIn(caught.exception.code, ('oneshot_consumed', 'start_observation_unknown'))
        self.assertEqual(calls, [])
        self.assertTrue(any(row['kind'] == 'spawn_intent'
                            for row in upgrade_state.load_verified(root)['events']))

    def test_inactive_durable_authority_does_not_spawn(self):
        root = self._root()
        upgrade_lifecycle.suspend_durable(root, 17)
        runner = Mock(return_value={'pid': 9, 'boot_id': 'boot', 'generation': 'g'})
        with self.assertRaises(recovery.Failure) as caught:
            recovery.launcher_spawn_authorized(root, runner, epoch=17)
        self.assertEqual(caught.exception.code, 'restart_fenced')
        self.assertEqual(runner.call_count, 0)

    def test_same_version_transition_consumes_authority_before_spawn(self):
        """The reviewed same-binary transition reaches the durable fence, not a second launcher."""
        import hashlib
        import upgrade_bootstrap
        root = self._root()
        binary = b'same-old-binary'
        plan = upgrade_bootstrap.plan(root, {'schema': 'ags.runtime.v0',
                                             'binary_sha256': hashlib.sha256(binary).hexdigest()},
                                      binary=binary)
        calls = []
        result = upgrade_bootstrap.execute(plan, root, epoch=17,
                                           runner=lambda argv: calls.append(argv) or {'pid': 4, 'boot_id': 'boot', 'generation': 'g'})
        self.assertEqual(result['status'], 'spawned')
        self.assertEqual(len(calls), 1)
        with self.assertRaises((recovery.Failure, upgrade_bootstrap.BootstrapError)) as caught:
            upgrade_bootstrap.execute(plan, root, epoch=17, runner=lambda argv: calls.append(argv))
        self.assertIn(getattr(caught.exception, 'code', ''), ('oneshot_consumed', 'state_operation_conflict'))
        self.assertEqual(len(calls), 1)


if __name__ == '__main__':
    unittest.main()
