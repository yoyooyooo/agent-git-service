#!/usr/bin/env python3
"""Journal chain, exclusive publish, CAS and two-process append.

Deleting the previous_sha256 comparison must turn the chain test red.
Two processes that both observe the same tail must not drop a published event.
"""
import json
import os
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path

import upgrade_state as state
from test_upgrade_journal import event


def _root():
    tmp = tempfile.TemporaryDirectory(dir=Path.home(), prefix='s6b-journal-chain-')
    root = Path(tmp.name).resolve()
    (root / 'state').mkdir(mode=0o700)
    return tmp, root


class JournalChainTests(unittest.TestCase):
    def setUp(self):
        self.tmp, self.root = _root()
        self.addCleanup(self.tmp.cleanup)

    def test_broken_previous_sha256_is_refused(self):
        state.commit_event(self.root, event())
        state.commit_event(self.root, event('second', operation='op-2'))
        published = (self.root / 'state/ags-autodeploy/local-v2/control/events'
                     / '00000000000000000002.json')
        value = json.loads(published.read_bytes())
        value['previous_sha256'] = 'f' * 64
        published.write_text(json.dumps(value))
        os.chmod(published, 0o600)
        with self.assertRaises(state.StateError) as caught:
            state.load_verified(self.root)
        self.assertEqual(caught.exception.args[0], 'state_chain_broken')

    def test_expected_epoch_is_a_cas_against_the_published_epoch(self):
        state.commit_event(self.root, event())
        later = event('second', operation='op-2')
        later['expected_epoch'] = 0
        later['epoch'] = 0
        with self.assertRaises(state.StateError) as caught:
            state.commit_event(self.root, later)
        self.assertEqual(caught.exception.args[0], 'state_epoch_conflict')
        self.assertEqual(state.load_verified(self.root)['projection']['epoch'], 1)

    def test_published_sequence_is_not_replaced(self):
        state.commit_event(self.root, event())
        events = self.root / 'state/ags-autodeploy/local-v2/control/events'
        before = (events / '00000000000000000001.json').read_bytes()
        with self.assertRaises(state.StateError):
            state._write_exclusive(events, '00000000000000000001.json', b'{"replaced":true}\n')
        self.assertEqual((events / '00000000000000000001.json').read_bytes(), before)

    def test_leftover_unique_staging_does_not_block_the_next_event(self):
        state.commit_event(self.root, event())
        events = self.root / 'state/ags-autodeploy/local-v2/control/events'
        leftover = events / '.stale-00000000000000000002.json.new'
        leftover.write_text('{}')
        os.chmod(leftover, 0o600)
        recovered = state.recover_local(self.root)
        self.assertTrue(leftover.exists())
        self.assertIn(leftover.name, recovered['ignored_staging'])
        published = state.commit_event(self.root, event('second', operation='op-2'))
        self.assertEqual(published['sequence'], 2)
        self.assertTrue(leftover.exists())

    def test_recorded_at_is_not_the_fixed_sentinel(self):
        state.commit_phase(self.root, 'op-time', 'none', 'noted', kind='note')
        recorded = state.load_verified(self.root)['events'][0]['recorded_at']
        self.assertNotEqual(recorded, '2026-11-02T00:00:00+00:00')
        self.assertIn('T', recorded)

    def test_two_processes_appending_the_same_tail_keep_both_events(self):
        state.commit_event(self.root, event())
        script = r'''
import pathlib, sys
import upgrade_state as state
from test_upgrade_journal import event
root = pathlib.Path(sys.argv[1])
name = sys.argv[2]
state.commit_event(root, event('note-' + name, operation='op-' + name))
'''
        children = {}
        for name in ('A', 'B'):
            children[name] = subprocess.Popen(
                [sys.executable, '-B', '-c', script, str(self.root), name],
                cwd=Path(__file__).resolve().parent,
                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        outcomes = {}
        for name, child in children.items():
            stdout, stderr = child.communicate(timeout=15)
            outcomes[name] = (child.returncode, stderr, stdout)
        operations = [row['operation_id'] for row in state.load_verified(self.root)['events']]
        self.assertEqual(sorted(operations), ['op-1', 'op-A', 'op-B'], outcomes)
        self.assertTrue(all(code == 0 for code, _, _ in outcomes.values()), outcomes)

    def test_commit_event_waits_out_another_process_state_lock(self):
        script = r'''
import pathlib, sys
import upgrade_state as state
from test_upgrade_journal import event
state.commit_event(pathlib.Path(sys.argv[1]), event())
'''
        with state.StateLock(self.root):
            child = subprocess.Popen(
                [sys.executable, '-B', '-c', script, str(self.root)],
                cwd=Path(__file__).resolve().parent,
                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            time.sleep(0.3)
            self.assertIsNone(child.poll())
        stdout, stderr = child.communicate(timeout=15)
        self.assertEqual(child.returncode, 0, stderr)
        self.assertEqual(len(state.load_verified(self.root)['events']), 1)


if __name__ == '__main__':
    unittest.main()
