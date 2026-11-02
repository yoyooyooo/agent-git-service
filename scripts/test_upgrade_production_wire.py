#!/usr/bin/env python3
"""B2: production recovery paths must read and write the durable journal."""
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock

import upgrade_engine
import upgrade_lifecycle
import upgrade_recovery
import upgrade_recovery_cli
import upgrade_state


class ProductionWireTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(dir=Path.home(), prefix='s6b-b2-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        (self.root / 'state').mkdir(mode=0o700)
        self.ledger = Mock()
        self.addCleanup(lambda: self.assertEqual(self.ledger.mock_calls, []))

    def test_engine_checkpoint_mirrors_the_old_journal(self):
        state = self.root / 'state'
        pending = state / 'pending'
        pending.mkdir(parents=True, mode=0o700)
        binding = {'host': 'fixture', 'old': 'a' * 40, 'new': 'b' * 40}
        journal = {'schema': 'ags.local-upgrade-journal.v2', 'binding': binding,
                   'phase': 'admission', 'stop_started': False, 'install_started': False,
                   'backup_created': False, 'start_attempted': False}
        (pending / 'journal.json').write_text(json.dumps(journal))
        os.chmod(pending / 'journal.json', 0o600)
        tx = upgrade_engine.Transaction(state, binding, {'authorization_id': 'auth-1'}, object())
        tx.journal = journal
        tx.checkpoint('prepare', prepare_started=True)
        rows = upgrade_state.load_verified(self.root)['events']
        self.assertTrue(rows)
        self.assertEqual(rows[-1]['after_phase'], 'prepare')
        self.assertEqual(json.loads((pending / 'journal.json').read_text())['phase'], 'prepare')
        self.assertEqual(self.ledger.mock_calls, [])

    def test_cold_phase_mirrors_and_launcher_reads_durable_authority(self):
        accepted = {'completion_sha256': 'a' * 64, 's4_approval_id': 's4', 'epoch': 1}
        approval = {'purpose': 'lifecycle-activate', 'decision': 'approve',
                    'approval_id': 'life-1', 'completion_sha256': 'a' * 64}
        upgrade_lifecycle.activate_durable(self.root, accepted, approval)
        calls = []

        def runner(argv):
            calls.append(list(argv))
            return 0

        result = upgrade_recovery.launcher_spawn(self.root, runner=runner, epoch=1)
        self.assertEqual(result['status'], 'spawn_intent')
        self.assertEqual(calls, [])
        loaded = upgrade_state.load_verified(self.root)
        self.assertEqual(loaded['projection']['authority']['state'], 'active')
        self.assertTrue(any(row['kind'] == 'spawn_intent' for row in loaded['events']))
        self.assertEqual(self.ledger.mock_calls, [])

    def test_suspended_authority_refuses_launcher_before_runner(self):
        accepted = {'completion_sha256': 'a' * 64, 's4_approval_id': 's4', 'epoch': 1}
        approval = {'purpose': 'lifecycle-activate', 'decision': 'approve',
                    'approval_id': 'life-1', 'completion_sha256': 'a' * 64}
        upgrade_lifecycle.activate_durable(self.root, accepted, approval)
        upgrade_lifecycle.suspend_durable(self.root, 1)
        calls = []
        with self.assertRaises(upgrade_recovery.Failure) as caught:
            upgrade_recovery.launcher_spawn(self.root, runner=lambda argv: calls.append(argv), epoch=1)
        self.assertEqual(caught.exception.args[0], 'restart_fenced')
        self.assertEqual(calls, [])
        self.assertEqual(self.ledger.mock_calls, [])

    def test_old_start_production_path_uses_durable_observer(self):
        restored = {'status': 'old_restored', 'started': False, 'recovery_approval_id': 'rec',
                    'data_sha256': 'd' * 64, 'tq_sha256': 'd' * 64}
        approval = {'purpose': 'old-start', 'decision': 'approve', 'approval_id': 'start-1'}
        observed = upgrade_recovery_cli.production_old_start(
            self.root, restored, approval, observer=lambda: {'started': True, 'pid': 7, 'boot_id': 'boot'})
        self.assertEqual(observed['pid'], 7)
        kinds = [row['kind'] for row in upgrade_state.load_verified(self.root)['events']]
        self.assertEqual(kinds, ['start_intent', 'start_observed'])
        self.assertEqual(self.ledger.mock_calls, [])

    def test_s4_completion_source_calls_durable_phase(self):
        source = Path(upgrade_state.__file__).resolve().parents[0].joinpath(
            'upgrade_manual_approval.py').read_text()
        self.assertIn("kind='s4_completed'", source)
        self.assertIn('commit_phase', source)
        self.assertLess(source.index("checkpoint('completion-durable')"), source.index("kind='s4_completed'"))
        self.assertEqual(self.ledger.mock_calls, [])


if __name__ == '__main__':
    unittest.main()
