#!/usr/bin/env python3
"""Continuation source contracts; mock root peers are not native acceptance."""
import copy
from datetime import datetime, timedelta, timezone
import hashlib
import importlib.util
import json
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import routeb_native_continuation as continuation
import routeb_host_execution as execution
import routeb_host_package as package_loader
import routeb_host_transaction as transaction
import upgrade_engine as engine
import upgrade_recovery as cold
from upgrade_backup_contract import ContractError, canonical, fingerprint
from test_routeb_cold_window import ExactSourceFixture


class AuthorityTests(unittest.TestCase):
    def test_typed_late_and_subject_match_go_and_reject_an_unrelated_nonempty_hash(self):
        subject = dict(registration_sha256='a' * 64, transaction_invocation_id='transaction-one',
                       host_instance='fixture-host', binding_sha256='b' * 64, generation='cold-one',
                       host_journal_sha256='c' * 64, cold_journal_sha256='d' * 64,
                       snapshot_id='snapshot-one', manifest_sha256='e' * 64, group_sha256='f' * 64)
        self.assertEqual(fingerprint(subject), 'b2f56fac656b3d5e1034d2270cbea49296279b13bc1fc9268c9fded5f3949e3b')
        from routeb_native_delegation import subject_sha256
        self.assertEqual(subject_sha256({'approval_id': 'continue-one', 'purpose': 'continue-installed',
                                        'subject_sha256': fingerprint(subject)}, 'continue-installed'),
                         'c920b9d371f906ce755020929ecb9bd20563125cbe95484848d603b7da24b51d')
        root = Path('/fixture/transaction/ags')
        late = dict(schema='ags.routeb-stage0-continuation.v1', registration_id='transaction', subject=subject,
                    backup_directory=str(root.parent / 'recovery/backups/snapshot-one'),
                    approval={'path': str(root.parent / 'continuation/approval.json'), 'sha256': '1' * 64, 'size': 1})
        registration = {'id': 'transaction', 'scope': 'isolated-native', 'invocations': [
            {'id': phase + '-one', 'phase': phase} for phase in
            ('transaction', 'continue-installed', 'serve-initial', 'serve-candidate')]}
        context = {'root': str(root), 'registration_id': 'transaction', 'registration_sha256': 'a' * 64,
                   'recovery_registration_sha256': fingerprint(late)}
        host = {'root': str(root), 'scope': 'isolated-native', 'host_instance': 'fixture-host'}
        with patch.object(continuation, 'read_trusted', return_value=canonical(late)):
            self.assertEqual(continuation.read_late(context, registration, host), late)
            with self.assertRaisesRegex(ContractError, 'continuation_registration_changed'):
                continuation.read_late({**context, 'recovery_registration_sha256': '2' * 64}, registration, host)
            registration['invocations'][0]['id'] = 'different-transaction'
            with self.assertRaisesRegex(ContractError, 'continuation_transaction_binding'):
                continuation.read_late(context, registration, host)
    def test_direct_entry_cannot_reach_a_loader_or_effect(self):
        with patch.object(continuation, 'load_preimage') as load, \
                patch.object(execution, 'HostExecution') as effects, self.assertRaises(ContractError):
            continuation.run('/unregistered/package', '/unregistered/approval', 'fixture')
        load.assert_not_called(); effects.assert_not_called()

    def test_continuation_does_not_borrow_restore_acknowledgements_or_original_expiry(self):
        now = datetime.now(timezone.utc)
        subject = {'fixture': 'exact failed installed state'}
        late = {'subject': subject, 'approval': {}}
        registration = {'invocations': [{'phase': 'continue-installed', 'authorization_id': 'continue-one'}]}
        context = {'root': '/fixture', 'operation': 'continue-installed'}
        value = {'schema': 'ags.routeb-continuation-approval.v1', 'approval_id': 'continue-one',
                 'actor': 'human:fixture', 'purpose': 'continue-installed', 'decision': 'approve',
                 'issued_at': (now - timedelta(minutes=2)).isoformat(),
                 'expires_at': (now - timedelta(minutes=1)).isoformat(), 'subject_sha256': fingerprint(subject)}
        with patch.object(continuation, 'read_reference', return_value=value):
            self.assertEqual(continuation.approval(late, context, registration, check_time=False), value)
            with self.assertRaisesRegex(ContractError, 'continuation_approval_expired'):
                continuation.approval(late, context, registration)
            value['expires_at'] = (now + timedelta(minutes=1)).isoformat()
            self.assertEqual(continuation.approval(late, context, registration), value)
            for mutation in ({'purpose': 'full-restore'}, {'approval_id': 'old-start-one'},
                             {'subject_sha256': 'e' * 64}, {'data_loss_acknowledged': True}):
                with self.subTest(mutation=mutation), \
                        patch.object(continuation, 'read_reference', return_value={**value, **mutation}), \
                        self.assertRaises(ContractError):
                    continuation.approval(late, context, registration)

    def test_only_original_start_progression_changes_the_frozen_window(self):
        original = {'phase': 'installed', 'start_attempted': False, 'generation': 'generation-one',
                    'installed_receipt_sha256': 'b' * 64, 'backup_binding': {'new_binary_sha256': 'a' * 64}}
        continuation.validate_progress(original, original)
        current = {**copy.deepcopy(original), 'phase': 'start_attempted', 'start_attempted': True,
                   'start_called': True, 'handoff': {'token': 'c' * 32, 'generation': 'generation-one',
                   'consumed': False, 'manager_requested': False, 'binary_sha256': 'a' * 64, 'receipt_sha256': 'b' * 64}}
        continuation.validate_progress(original, current)
        with self.assertRaisesRegex(ContractError, 'continuation_start_not_requested'):
            continuation.validate_progress(original, current, candidate=True)
        current['handoff']['manager_requested'] = True
        continuation.validate_progress(original, current, candidate=True)
        with self.assertRaisesRegex(ContractError, 'continuation_start_already_requested'):
            continuation.validate_progress(original, current)
        current['generation'] = 'another-generation'
        with self.assertRaisesRegex(ContractError, 'continuation_cold_binding_changed'):
            continuation.validate_progress(original, current, candidate=True)

    def test_running_candidate_uses_historical_authority_but_new_spawn_requires_current(self):
        path = Path(__file__).with_name('ags-runtime.py')
        spec = importlib.util.spec_from_file_location('continued_runtime', path)
        runtime = importlib.util.module_from_spec(spec); spec.loader.exec_module(runtime)
        native = {'phase': 'serve-candidate', 'recovery_registration_sha256': 'd' * 64}
        with patch.object(runtime, '_native_context', return_value=native), \
                patch.object(continuation, 'candidate_binding', side_effect=ContractError('time-window')) as gate:
            self.assertFalse(runtime.startup_permitted(Path('/fixture'), native_context=native))
            gate.assert_called_with(Path('/fixture'), native)
            self.assertFalse(runtime.running_permitted(Path('/fixture'), 'generation', 123, native_context=native))
            gate.assert_called_with(Path('/fixture'), native, check_time=False)
            with self.assertRaisesRegex(ContractError, 'time-window'):
                runtime.consume_start(Path('/fixture'), native_context=native)
            gate.assert_called_with(Path('/fixture'), native)


class InstalledFixture(ExactSourceFixture):
    def setUp(self):
        super().setUp()
        self.request = copy.deepcopy(self.b)
        self.window.binding = copy.deepcopy(self.b)
        engine.save(self.pending / 'claim.json', {'schema': 'ags.local-claim.v2', 'binding': self.b})
        self.old_receipt['binaries']['gh-server']['identity']['tree'] = 'd' * 40
        engine.save(self.root / 'state/installation.json', self.old_receipt)
        self.raw_old_receipt = (self.root / 'state/installation.json').read_bytes()
        self.prepare(); self.install_receipt()
        self.window.installed(self.identity, lambda _: self.identity)
        self.cold_raw = (self.pending / 'cold-journal.json').read_bytes()
        self.old_host = {'schema': 'ags.routeb-host-journal.v1', 'root': str(self.root),
                         'transaction_id': self.b['transaction_id'], 'host_instance': self.b['host_instance'],
                         'binding_sha256': fingerprint(self.b), 'phase': 'installed', 'status': 'failed',
                         'code': 'fixture-interruption', 'stop_attempted': True, 'start_attempted': False}
        engine.save(self.pending / 'host-execution.json', self.old_host)
        self.host_raw = (self.pending / 'host-execution.json').read_bytes()
        self.late = {'registration_id': self.b['transaction_id'],
                     'backup_directory': self.window.journal['backup_dir'], 'subject': {
                         'host_instance': self.b['host_instance'], 'binding_sha256': fingerprint(self.b),
                         'generation': self.window.journal['generation'],
                         'host_journal_sha256': hashlib.sha256(self.host_raw).hexdigest(),
                         'cold_journal_sha256': hashlib.sha256(self.cold_raw).hexdigest()}}
        self.manifest_sha = hashlib.sha256((Path(self.late['backup_directory']) / 'manifest.json').read_bytes()).hexdigest()


class SealedPreimageTests(InstalledFixture):
    def load_old(self):
        return package_loader._old_installation(self.root, self.group,
            {'old_revision': self.b['old'], 'old_tree': 'd' * 40},
            (hashlib.sha256(self.cold_raw).hexdigest(), self.manifest_sha))

    def test_loader_uses_real_sealed_old_receipt_after_current_receipt_is_new(self):
        old, digest = self.load_old()
        self.assertEqual(old, self.old_receipt)
        self.assertEqual(digest, hashlib.sha256(self.raw_old_receipt).hexdigest())
        self.assertNotEqual(old, json.loads((self.root / 'state/installation.json').read_bytes()))

    def test_current_receipt_cannot_replace_fixed_old_preimage(self):
        (self.pending / 'old-installation.json').write_bytes((self.root / 'state/installation.json').read_bytes())
        with self.assertRaisesRegex(ContractError, 'old_evidence_mismatch'):
            self.load_old()

    def test_changed_snapshot_or_current_window_refuses_preimage(self):
        self.manifest_sha = 'f' * 64
        with self.assertRaisesRegex(ContractError, 'preimage_snapshot_binding'):
            self.load_old()
        (self.pending / 'cold-journal.json').write_bytes(self.cold_raw + b'\n')
        with self.assertRaisesRegex(ContractError, 'installed_journal_changed'):
            self.load_old()


class ContinuationTailTests(InstalledFixture):
    def test_original_failure_is_preserved_and_only_original_start_tail_runs_once(self):
        self.window.release(); self.window.release_owner()
        manager = Mock()
        manager.start.side_effect = self.manager.start
        manager.rebind_stopped.return_value = {'root_proof': 'mock-only'}
        package = SimpleNamespace(root=self.root, pending=self.pending, group=self.group,
            dsn='sqlite:' + str(self.storage.dbpath), cold_binding=lambda: copy.deepcopy(self.b), host=object(),
            profile={'schema_sha256': self.storage.admission, 'trash_root': str(self.root), 'capture': {'logs': []}},
            candidate=SimpleNamespace(value={'version': self.identity}), package={}, path=self.root / 'package.json')
        context = {'recovery_registration_sha256': 'd' * 64}
        def writers(_manager, _group, generation):
            return lambda: {'generation': generation(), 'closed': True, 'writers': [], 'unknown': []}
        with patch.object(continuation, 'packet', return_value=(context, self.late, package)), \
                patch.object(continuation, 'binding', return_value=(context, self.late, {}, {})), \
                patch.object(continuation, 'authorize'), \
                patch.object(execution, 'FixedManager', return_value=manager), \
                patch.object(execution, 'SystemTrash', return_value=self.trash), \
                patch.object(execution, 'WriterProbe', side_effect=writers), \
                patch.object(execution, 'lifecycle_blockers', return_value=[]), \
                patch.object(execution, 'read_version', return_value=self.identity), \
                patch.object(execution.HostExecution, 'observe_start', side_effect=ContractError('fixture_ready_unknown')), \
                patch.object(execution.HostExecution, 'install', side_effect=AssertionError('reinstalled')):
            with self.assertRaisesRegex(ContractError, 'fixture_ready_unknown'):
                continuation.run('package', 'approval', 'fixture')
            self.assertEqual(self.manager.starts, 1)
            self.assertEqual((self.pending / 'host-execution.json').read_bytes(), self.host_raw)
            directory = self.pending / 'continuation'
            self.assertEqual((directory / 'original-host-execution.json').read_bytes(), self.host_raw)
            self.assertEqual((directory / 'original-cold-journal.json').read_bytes(), self.cold_raw)
            result = json.loads((directory / 'host-execution.json').read_bytes())
            self.assertEqual((result['phase'], result['status']), ('start_attempted', 'failed'))
            self.assertFalse(result['stop_attempted']); self.assertTrue(result['previous_stop_observed'])
            before = (self.pending / 'cold-journal.json').read_bytes()
            with self.assertRaisesRegex(ContractError, 'continuation_failed_window_changed'):
                continuation.run('package', 'approval', 'fixture')
            self.assertEqual((self.pending / 'cold-journal.json').read_bytes(), before)
            self.assertEqual(self.manager.starts, 1)
        manager.stop.assert_not_called()


if __name__ == '__main__':
    unittest.main()
