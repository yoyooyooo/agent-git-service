#!/usr/bin/env python3
"""S6b.02 journal. Red against the old fixed staging writer, green on the journal."""
import hashlib
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock

import upgrade_backup
import upgrade_recovery


def event(kind='note', effect=None, operation='op-1'):
    return {
        'schema': 'ags.upgrade-event.v1', 'event_id': 'ev-' + kind, 'operation_id': operation,
        'kind': kind, 'host_instance': 'host-1', 'identity_sha256': 'a' * 64,
        'expected_epoch': None, 'epoch': 1, 'transaction_id': None, 'binding_sha256': 'b' * 64,
        'before_phase': 'none', 'after_phase': kind, 'approval_sha256': None,
        'evidence_sha256': None, 'effect': effect, 'recorded_at': '2026-11-02T00:00:00+00:00'}


class JournalBaseline(unittest.TestCase):
    """The old writer keeps one fixed staging name and has no chain reader."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(dir=Path.home(), prefix='s6b02-red-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        state = self.root / 'state'
        state.mkdir(mode=0o700)
        self.ledger = Mock()
        self.addCleanup(lambda: self.assertEqual(self.ledger.mock_calls, []))

    def test_old_writer_keeps_one_staging_name_and_has_no_journal(self):
        target = self.root / 'state/fact.json'
        upgrade_backup.durable_bytes(target, b'{"n":1}\n')
        self.assertTrue(target.is_file())
        # The published protocol retains this exact name and refuses a second create.
        with self.assertRaises(FileExistsError):
            upgrade_backup.durable_bytes(target, b'{"n":2}\n')
        self.assertFalse((self.root / 'state/ags-autodeploy/local-v2/control/events').exists())
        self.assertFalse(hasattr(upgrade_recovery, 'load_verified'))
        self.assertEqual(self.ledger.mock_calls, [])


class JournalTests(unittest.TestCase):
    def setUp(self):
        import upgrade_state as state
        self.state = state
        self.tmp = tempfile.TemporaryDirectory(dir=Path.home(), prefix='s6b02-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        (self.root / 'state').mkdir(mode=0o700)
        self.ledger = Mock()
        self.addCleanup(lambda: self.assertEqual(self.ledger.mock_calls, []))

    def test_publish_then_rebuild_and_reject_a_hole(self):
        first = self.state.commit_event(self.root, event())
        self.assertEqual(first['sequence'], 1)
        loaded = self.state.load_verified(self.root)
        self.assertEqual(loaded['projection']['sequence'], 1)
        self.assertEqual(loaded['projection']['event_sha256'], first['sha256'])
        events = self.root / 'state/ags-autodeploy/local-v2/control/events'
        published = events / '00000000000000000001.json'
        published.rename(events / '00000000000000000002.json')
        with self.assertRaises(self.state.StateError) as caught:
            self.state.load_verified(self.root)
        self.assertEqual(caught.exception.args[0], 'state_chain_hole')
        self.assertEqual(self.ledger.mock_calls, [])

    def test_duplicate_key_and_duplicate_event_refuse(self):
        raw = (b'{"schema":"ags.upgrade-event.v1","schema":"ags.upgrade-event.v1"}\n')
        with self.assertRaises(self.state.StateError) as caught:
            self.state._object(raw)
        self.assertEqual(caught.exception.args[0], 'state_duplicate_key')
        self.state.commit_event(self.root, event())
        conflicting = event(operation='op-2')
        with self.assertRaises(self.state.StateError) as again:
            self.state.commit_event(self.root, conflicting)
        self.assertEqual(again.exception.args[0], 'state_duplicate_event')

    def test_same_operation_does_not_publish_a_second_event(self):
        effect = {'effect_id': 'fx-1', 'type': 'local-file', 'expected_before': 'old',
                  'desired_after': 'new', 'status': 'intent'}
        first = self.state.commit_event(self.root, event('install_intent', effect, 'op-same'))
        again = self.state.commit_event(self.root, event('install_intent', effect, 'op-same'))
        self.assertEqual(again['sequence'], first['sequence'])
        self.assertEqual(again['sha256'], first['sha256'])
        names = list((self.root / 'state/ags-autodeploy/local-v2/control/events').glob('[0-9]*.json'))
        self.assertEqual(len(names), 1)

    def test_staging_is_not_a_commit_and_recovery_has_no_effect(self):
        self.state.commit_event(self.root, event())
        control = self.root / 'state/ags-autodeploy/local-v2/control'
        staging = control / 'events/.00000000000000000002.json.new'
        staging.write_bytes(b'{"schema":"not-published"}\n')
        os.chmod(staging, 0o600)
        recovered = self.state.recover_local(self.root)
        self.assertEqual(recovered['effects'], [])
        self.assertEqual(recovered['sequence'], 1)
        self.assertIn(staging.name, recovered['ignored_staging'])
        self.assertEqual(self.state.load_verified(self.root)['projection']['sequence'], 1)

    def test_projection_lag_is_rebuilt_and_does_not_authorize(self):
        self.state.commit_event(self.root, event())
        current = self.root / 'state/ags-autodeploy/local-v2/control/current.json'
        current.write_bytes(b'{"schema":"stale"}\n')
        os.chmod(current, 0o600)
        rebuilt = self.state.project(self.root)
        self.assertEqual(rebuilt['sequence'], 1)
        self.assertNotIn('active', json.dumps(rebuilt))
        self.assertIsNone(rebuilt['authority'])

    def test_symlink_event_refuses(self):
        self.state.commit_event(self.root, event())
        events = self.root / 'state/ags-autodeploy/local-v2/control/events'
        outside = self.root / 'outside.json'
        outside.write_bytes(b'{}\n')
        (events / '00000000000000000002.json').symlink_to(outside)
        with self.assertRaises(self.state.StateError):
            self.state.load_verified(self.root)

    def test_two_processes_cannot_both_hold_the_state_lock(self):
        import subprocess
        import sys
        held = self.state.StateLock(self.root)
        held.__enter__()
        self.addCleanup(held.__exit__, None, None, None)
        probe = subprocess.run(
            [sys.executable, '-B', '-c',
             'import sys; from upgrade_state import StateLock, StateError\n'
             'try:\n'
             '    StateLock(sys.argv[1]).__enter__()\n'
             'except StateError as error:\n'
             '    print(error.args[0]); raise SystemExit(0)\n'
             'raise SystemExit(1)', str(self.root)],
            cwd=Path(__file__).resolve().parent, capture_output=True, text=True)
        self.assertEqual(probe.returncode, 0, probe.stderr)
        self.assertEqual(probe.stdout.strip(), 'state_lock_held')


if __name__ == '__main__':
    unittest.main()
