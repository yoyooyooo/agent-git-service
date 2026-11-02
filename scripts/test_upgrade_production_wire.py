#!/usr/bin/env python3
"""B2: production recovery paths must read and write the durable journal."""
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock
from unittest.mock import Mock

import upgrade_engine
import upgrade_host
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

    def test_engine_checkpoint_uses_the_local_v2_layout(self):
        state = self.root / 'state/ags-autodeploy/local-v2'
        pending = state / 'pending'
        pending.mkdir(parents=True, mode=0o700)
        for path in (self.root, self.root / 'state', self.root / 'state/ags-autodeploy', state, pending):
            path.chmod(0o700)
        binding = {'host': 'fixture', 'host_instance': 'fixture-host', 'identity_sha256': 'c' * 64,
                   'authorization_id': 'auth-layout', 'old': 'a' * 40, 'new': 'b' * 40}
        journal = {'schema': 'ags.local-upgrade-journal.v2', 'binding': binding, 'phase': 'admission'}
        tx = upgrade_engine.Transaction(state, binding, {'authorization_id': 'auth-layout'}, object())
        tx.journal = journal
        tx.checkpoint('prepare')
        self.assertTrue((state / 'control/format.json').is_file())
        self.assertFalse((self.root.parent / 'state').exists())
        rows = upgrade_state.load_verified(self.root)['events']
        self.assertEqual(rows[-1]['transaction_id'], 'auth-layout')
        other_binding = {**binding, 'authorization_id': 'auth-other'}
        other = upgrade_engine.Transaction(state, other_binding,
                                           {'authorization_id': 'auth-other'}, object())
        other.journal = {'phase': 'admission'}
        try:
            other.checkpoint('prepare')
        except upgrade_state.StateError as error:
            self.assertEqual(error.args[0], 'state_operation_conflict')
        else:
            rows = upgrade_state.load_verified(self.root)['events']
            self.assertEqual([row['transaction_id'] for row in rows], ['auth-layout'],
                             [row['operation_id'] for row in rows])
        again = upgrade_engine.Transaction(state, binding, {'authorization_id': 'auth-layout'}, object())
        again.journal = {'phase': 'admission'}
        again.checkpoint('prepare')
        self.assertEqual(len(upgrade_state.load_verified(self.root)['events']), 1)

    def test_production_stop_suspends_authority_before_manager_stop(self):
        accepted = {'completion_sha256': 'a' * 64, 's4_approval_id': 's4', 'epoch': 1,
                    'host_instance': 'fixture-host'}
        upgrade_lifecycle.activate_durable(
            self.root, accepted,
            {'purpose': 'lifecycle-activate', 'decision': 'approve', 'approval_id': 'life-1',
             'completion_sha256': 'a' * 64, 'host_instance': 'fixture-host'})
        seen = []

        def stop(**kwargs):
            seen.append([row['kind'] for row in upgrade_state.load_verified(self.root)['events']])

        effects = upgrade_host.Effects.__new__(upgrade_host.Effects)
        effects.root = self.root
        window = Mock()
        window.stop.side_effect = stop
        effects._window = lambda: window
        effects.stop()
        self.assertIn('authority_suspended', seen[0])
        self.assertEqual(upgrade_state.load_verified(self.root)['projection']['authority']['state'], 'suspended')

    def test_engine_checkpoint_mirrors_the_old_journal(self):
        state = self.root / 'state'
        pending = state / 'pending'
        pending.mkdir(parents=True, mode=0o700)
        binding = {'host': 'fixture', 'host_instance': 'fixture-host', 'identity_sha256': 'c' * 64,
                   'old': 'a' * 40, 'new': 'b' * 40}
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
            return {'pid': 11, 'boot_id': 'boot-1', 'generation': 'gen-1'}

        result = upgrade_recovery.launcher_spawn(self.root, runner=runner, epoch=1)
        self.assertEqual(result['status'], 'spawned')
        self.assertEqual(calls, [[str(self.root / 'bin/gh-server')]])
        loaded = upgrade_state.load_verified(self.root)
        self.assertEqual(loaded['projection']['authority']['state'], 'active')
        self.assertTrue(any(row['kind'] == 'spawn_intent' for row in loaded['events']))
        self.assertEqual(loaded['projection']['pending_effects'], [])
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

    def test_production_activate_requires_host_and_historize_moves_marker(self):
        with self.assertRaises(upgrade_lifecycle.LifecycleError):
            upgrade_lifecycle.production_activate(
                self.root, {'completion_sha256': 'a' * 64, 's4_approval_id': 's4', 'epoch': 1},
                {'purpose': 'lifecycle-activate', 'decision': 'approve', 'approval_id': 'life-1',
                 'completion_sha256': 'a' * 64})
        marker = self.root / 'state/upgrade-cold.json'
        marker.write_text('{"schema":"ags.cold-window.v1"}\n')
        os.chmod(marker, 0o600)
        moved = upgrade_lifecycle.historize_marker(self.root, 1)
        self.assertFalse(marker.exists())
        self.assertTrue(Path(moved['path']).is_file())
        self.assertEqual((self.root / 'state/upgrade-cold.json').exists(), False)

    def test_supervise_records_durable_spawn_and_exit(self):
        import importlib.util
        spec = importlib.util.spec_from_file_location(
            'ags_runtime_supervise', Path(upgrade_state.__file__).resolve().parent / 'ags-runtime.py')
        runtime = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(runtime)
        upgrade_lifecycle.activate_durable(
            self.root,
            {'completion_sha256': 'a' * 64, 's4_approval_id': 's4', 'epoch': 1, 'host_instance': 'host-1'},
            {'purpose': 'lifecycle-activate', 'decision': 'approve', 'approval_id': 'life-1',
             'completion_sha256': 'a' * 64, 'host_instance': 'host-1'})
        child = Mock(pid=321, stdout=Mock())
        child.stdout.fileno.return_value = 1
        child.wait.return_value = 0
        child.poll.return_value = 0
        selector = Mock()
        selector.get_map.side_effect = [False]
        with mock.patch.object(runtime.subprocess, 'Popen', return_value=child), \
                mock.patch.object(runtime.selectors, 'DefaultSelector', return_value=selector), \
                mock.patch.object(runtime.signal, 'signal'), \
                mock.patch.object(runtime.os, 'killpg', side_effect=ProcessLookupError):
            self.assertEqual(runtime.supervise(self.root, {}, {}, Mock(), Mock(), generation='gen-supervise'), 0)
        kinds = [row['kind'] for row in upgrade_state.load_verified(self.root)['events']]
        self.assertIn('generation_spawned', kinds)
        self.assertIn('generation_exited', kinds)
        self.assertEqual(upgrade_state.load_verified(self.root)['projection']['pending_effects'], [])

    def test_startup_fence_refuses_missing_format_authority_and_pending_spawn(self):
        import importlib.util
        spec = importlib.util.spec_from_file_location(
            'ags_runtime_fence', Path(upgrade_state.__file__).resolve().parent / 'ags-runtime.py')
        runtime = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(runtime)
        upgrade_state.commit_event(self.root, {
            'event_id': 'ev-note', 'operation_id': 'op-1', 'kind': 'note', 'host_instance': 'host-1',
            'identity_sha256': 'a' * 64, 'expected_epoch': None, 'epoch': 1, 'transaction_id': None,
            'binding_sha256': 'b' * 64, 'before_phase': 'none', 'after_phase': 'note',
            'approval_sha256': None, 'evidence_sha256': None, 'effect': None,
            'recorded_at': '2026-11-02T00:00:00+00:00'})
        header = self.root / 'state/ags-autodeploy/local-v2/control/format.json'
        saved = header.with_suffix('.saved')
        header.rename(saved)
        self.assertFalse(runtime.startup_permitted(self.root))
        saved.rename(header)
        self.assertFalse(runtime.startup_permitted(self.root))
        upgrade_lifecycle.activate_durable(
            self.root,
            {'completion_sha256': 'a' * 64, 's4_approval_id': 's4', 'epoch': 1, 'host_instance': 'host-1'},
            {'purpose': 'lifecycle-activate', 'decision': 'approve', 'approval_id': 'life-1',
             'completion_sha256': 'a' * 64, 'host_instance': 'host-1'})
        upgrade_state.commit_phase(
            self.root, 'pending-spawn', 'active', 'spawn_intent', kind='spawn_intent',
            host_instance='host-1', identity_sha256='a' * 64, binding_sha256='b' * 64,
            effect={'effect_id': 'spawn-1', 'type': 'spawn', 'expected_before': 'stopped',
                    'desired_after': 'new', 'status': 'intent'})
        self.assertFalse(runtime.startup_permitted(self.root))
        with self.assertRaises(runtime.CheckFailure):
            runtime.consume_start(self.root)

    def test_s4_completion_source_calls_durable_phase(self):
        source = Path(upgrade_state.__file__).resolve().parents[0].joinpath(
            'upgrade_manual_approval.py').read_text()
        self.assertIn("kind='s4_completed'", source)
        self.assertIn('commit_phase', source)
        self.assertLess(source.index("checkpoint('completion-durable')"), source.index("kind='s4_completed'"))
        self.assertEqual(self.ledger.mock_calls, [])


if __name__ == '__main__':
    unittest.main()
