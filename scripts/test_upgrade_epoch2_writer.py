#!/usr/bin/env python3
"""B2: production writers must use the verified authority epoch after epoch=2.

The two-chain fixture stops at activation. This suite continues from that
active authority and records a runtime spawn/exit plus a later engine phase.
It does not start a service or call a manager.
"""
import importlib.util
import json
import os
from pathlib import Path
import sys
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parent))
import upgrade_state
from test_upgrade_two_chain import TwoChainTests


def _runtime():
    path = Path(__file__).resolve().parent / 'ags-runtime.py'
    spec = importlib.util.spec_from_file_location('ags_runtime_under_test', path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class Epoch2WriterTests(unittest.TestCase):
    def setUp(self):
        self.fixture = TwoChainTests()
        self.fixture.setUp()
        self.addCleanup(self.fixture.doCleanups)
        self.root = self.fixture.root

    def test_runtime_and_engine_record_against_authority_epoch_2(self):
        self.fixture.test_two_chains_and_a_second_process_replay_the_same_phase()
        loaded = upgrade_state.load_verified(self.root)
        self.assertEqual(loaded['projection']['authority']['epoch'], 2)
        self.assertEqual(loaded['projection']['authority']['state'], 'active')
        before = loaded['projection']['sequence']

        runtime = _runtime()
        runtime._journal_effect(
            self.root, 'supervise-spawn-epoch2', 'active', 'running', 'generation_spawned',
            {'effect_id': 'gen-epoch2', 'type': 'spawn', 'expected_before': 'stopped',
             'desired_after': 'gen-epoch2', 'status': 'observed'})
        runtime._journal_effect(
            self.root, 'supervise-exit-epoch2', 'running', 'exited', 'generation_exited',
            {'effect_id': 'gen-epoch2', 'type': 'exit', 'expected_before': 'running',
             'desired_after': 'exited', 'status': 'observed'})

        chain = self.fixture._transaction('auth-C')
        chain.journal['phase'] = 'none'
        chain.checkpoint('admission')

        loaded = upgrade_state.load_verified(self.root)
        self.assertEqual(loaded['projection']['authority']['epoch'], 2)
        self.assertEqual(loaded['projection']['authority']['state'], 'active')
        added = loaded['events'][before:]
        self.assertEqual([row['kind'] for row in added],
                         ['generation_spawned', 'generation_exited', 'engine_admission'])
        self.assertTrue(all(row['epoch'] == 2 and row['expected_epoch'] == 2 for row in added))
        self.assertEqual(added[-1]['transaction_id'], 'auth-C')
        self.assertEqual(added[-1]['after_phase'], 'admission')
        self.assertFalse(loaded['projection']['pending_effects'])
        self.assertGreater(loaded['projection']['sequence'], before)
        self.assertFalse(os.environ.get('AGS_RUNTIME_STARTED'))


if __name__ == '__main__':
    unittest.main()
