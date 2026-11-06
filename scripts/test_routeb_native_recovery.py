#!/usr/bin/env python3
"""Recovery orchestration contracts. Mock peers do not prove native entry."""
from contextlib import ExitStack
import copy
from datetime import datetime, timedelta, timezone
import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import routeb_native_recovery as recovery
from upgrade_backup_contract import ContractError, canonical


class RealAdmissionTests(unittest.TestCase):
    def test_direct_python_cannot_reach_restore_or_manager_effects(self):
        root = Path('/home/example/.local/state/ags-routeb-native/unregistered-test/ags')
        for operation in (recovery.restore, recovery.start_old):
            with self.subTest(operation=operation.__name__), \
                    patch.object(recovery, 'load_package') as package, \
                    patch.object(recovery, 'apply_restore') as apply, \
                    patch.object(recovery, 'FixedManager') as manager, self.assertRaises(ContractError):
                operation(root, root.parent / 'package.json', root.parent / 'approval.json')
            package.assert_not_called(); apply.assert_not_called(); manager.assert_not_called()

    def test_serialized_old_phase_does_not_replace_a_root_peer(self):
        with self.assertRaises(ContractError):
            recovery.old_start_binding(Path('/unregistered'), {'phase': 'serve-old', 'verified': True})

    def test_runtime_callback_cannot_satisfy_the_real_recovery_gate(self):
        path = Path(__file__).with_name('ags-runtime.py')
        spec = importlib.util.spec_from_file_location('native_recovery_runtime', path)
        runtime = importlib.util.module_from_spec(spec); spec.loader.exec_module(runtime)
        with patch.object(runtime, '_native_context', return_value={'phase': 'serve-old'}):
            self.assertFalse(runtime.startup_permitted(Path('/unregistered'), native_context=lambda: {}))
            with self.assertRaises(ContractError):
                runtime.consume_start(Path('/unregistered'), native_context=lambda: {})


class OrchestrationTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(dir=Path.home(), prefix='routeb-recovery-test-')
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name); self.base.chmod(0o700)
        self.root = self.base / 'ags'; self.root.mkdir(mode=0o700)
        (self.base / 'recovery').mkdir(mode=0o700)
        self.events = []
        self.context = {'registration_id': 'fixture', 'registration_sha256': 'a' * 64,
                        'recovery_registration_sha256': 'b' * 64, 'invocation_id': 'restore-one',
                        'authorization_id': 'restore-approval', 'root': str(self.root)}
        self.late = {'snapshot_id': 'snapshot-one', 'manifest_sha256': 'c' * 64,
                     'group_sha256': 'd' * 64, 'backup_directory': str(self.base / 'recovery/backups/snapshot-one')}
        self.package = SimpleNamespace(root=self.root, group={'objects': []}, dsn='sqlite:fixture',
                        profile={'schema_sha256': 'e' * 64}, host=object(),
                        cold_binding=lambda: {}, authorize=lambda: self.events.append('authorize'))
        self.manager = Mock()
        self.manager.preflight.side_effect = lambda: self.events.append('preflight')
        self.manager.suppress_restart.side_effect = lambda: self.events.append('suppress')
        self.manager.stop.side_effect = lambda: self.events.append('stop') or {'normal': True}
        self.window = SimpleNamespace(owner=False, locks=False)

        def owner(): self.window.owner = True; self.events.append('owner')
        def acquire():
            self.assertTrue(self.window.owner); self.window.locks = True; self.events.append('locks')
        def release(): self.window.locks = False; self.events.append('release')
        def release_owner(): self.window.owner = False; self.events.append('release-owner')
        self.window.acquire_owner = owner; self.window.acquire = acquire
        self.window.release = release; self.window.release_owner = release_owner
        self.restored = {'schema': 'ags.routeb-restored-group.v2', 'applied': True, 'started': False,
                         'snapshot_id': 'snapshot-one', 'approval_id': 'restore-approval'}
        self.verified = {'manifest_sha256': 'c' * 64, 'authority_evidence_sha256': 'f' * 64}
        self.stack = ExitStack(); self.addCleanup(self.stack.close)
        def patched(name, **kwargs): return self.stack.enter_context(patch.object(recovery, name, **kwargs))
        patched('packet', return_value=(self.context, self.late, self.package))
        patched('recovery_binding', return_value=(self.context, self.late))
        patched('approval', return_value={'approval_id': 'restore-approval'})
        patched('FixedManager', return_value=self.manager)
        patched('ColdWindow', return_value=self.window)
        patched('plan_restore', return_value={'manifest_sha256': 'c' * 64})
        self.apply = patched('apply_restore', side_effect=self.apply_restore)
        patched('verify_recovered_group', side_effect=self.verify_restore)
        self.writer = self.stack.enter_context(patch.object(recovery.client, 'writer_probe', side_effect=self.writers))
        self.complete = self.stack.enter_context(patch.object(recovery.client, 'complete_restore', side_effect=self.seal))

    def writers(self, root, generation):
        self.assertEqual(root, self.root); self.assertTrue(self.window.owner and self.window.locks)
        self.events.append('closed-writers')
        return {'generation': generation, 'closed': True, 'writers': [], 'unknown': []}

    def apply_restore(self, request, *, supervisor_path, journal):
        self.assertTrue(self.window.owner and self.window.locks)
        self.assertEqual(self.events[-1], 'closed-writers')
        self.events.append('apply')
        journal.append({'effect_id': 'snapshot-one:data', 'status': 'intent'})
        # The intent is already durable before the simulated switch begins.
        effects = sorted((self.base / 'recovery/native-recovery').glob('effect-*.json'))
        self.assertEqual(json.loads(effects[-1].read_bytes())['effect']['status'], 'intent')
        journal.append({'effect_id': 'snapshot-one:data', 'status': 'observed'})
        return copy.deepcopy(self.restored)

    def verify_restore(self, restored, decision):
        self.assertTrue(self.window.locks); self.events.append('verify-tq')
        self.assertEqual(restored, self.restored)
        return self.verified

    def seal(self, root, digest):
        self.assertTrue(self.window.owner and self.window.locks)
        self.events.append('seal')
        raw = (self.base / 'recovery/native-recovery/restore-result.json').read_bytes()
        self.assertEqual(hashlib.sha256(raw).hexdigest(), digest)
        return {'result': {'sha256': digest}}

    def test_restore_keeps_locks_through_tq_recheck_and_root_seal(self):
        result = recovery.restore(self.root, 'package', 'approval')
        self.assertTrue(result['restored']); self.assertFalse(result['started'])
        self.assertLess(self.events.index('stop'), self.events.index('locks'))
        self.assertLess(self.events.index('closed-writers'), self.events.index('apply'))
        self.assertLess(self.events.index('verify-tq'), self.events.index('seal'))
        self.assertEqual(self.events[-2:], ['release', 'release-owner'])
        self.manager.start.assert_not_called()
        self.assertEqual(len(list((self.base / 'recovery/native-recovery').glob('effect-*.json'))), 4)

    def test_open_or_unknown_writers_refuse_before_any_restore(self):
        self.writer.side_effect = lambda root, generation: {'generation': generation, 'closed': False,
                                                'writers': [{'kind': 'holder', 'object': 'sqlite', 'count': 1}], 'unknown': []}
        with self.assertRaisesRegex(ContractError, 'writers_not_quiescent'):
            recovery.restore(self.root, 'package', 'approval')
        self.apply.assert_not_called(); self.complete.assert_not_called(); self.manager.start.assert_not_called()
        self.assertFalse((self.base / 'recovery/native-recovery/restore-result.json').exists())

    def test_failed_tq_verification_never_seals_or_starts(self):
        with patch.object(recovery, 'verify_recovered_group', side_effect=ContractError('reprovisioned_group_unverified')):
            with self.assertRaisesRegex(ContractError, 'reprovisioned_group_unverified'):
                recovery.restore(self.root, 'package', 'approval')
        self.complete.assert_not_called(); self.manager.start.assert_not_called()
        self.assertTrue(list((self.base / 'recovery/native-recovery').glob('effect-*.json')))

    def test_root_seal_failure_preserves_result_and_never_replays_effects(self):
        self.complete.side_effect = ContractError('stage0_context_closed')
        with self.assertRaisesRegex(ContractError, 'stage0_context_closed'):
            recovery.restore(self.root, 'package', 'approval')
        self.assertTrue((self.base / 'recovery/native-recovery/restore-result.json').is_file())
        self.assertEqual(self.apply.call_count, 1); self.manager.start.assert_not_called()


class OldGenerationTests(unittest.TestCase):
    def test_old_generation_is_write_once_and_does_not_refund_the_cold_handoff(self):
        with tempfile.TemporaryDirectory(dir=Path.home(), prefix='routeb-old-generation-') as directory:
            parent = Path(directory); parent.chmod(0o700)
            root = parent / 'ags'; root.mkdir(mode=0o700); (root / 'state').mkdir(mode=0o700)
            (parent / 'recovery').mkdir(mode=0o700)
            original = canonical({'phase': 'start_attempted', 'handoff': {'consumed': True}})
            (root / 'state/cold-journal.json').write_bytes(original)
            context = {'invocation_id': 'serve-old-one', 'phase': 'serve-old'}
            with patch.object(recovery, 'old_start_binding', return_value=(context, {}, {'bound': True}, {})):
                self.assertEqual(recovery.consume_old_generation(root, context), 'old-serve-old-one')
                with self.assertRaises(FileExistsError): recovery.consume_old_generation(root, context)
            self.assertEqual((root / 'state/cold-journal.json').read_bytes(), original)

    def test_recovery_approval_purpose_and_expiry_are_not_interchangeable(self):
        now = datetime.now(timezone.utc)
        decision = {'schema': 'ags.routeb-recovery-approval.v1', 'approval_id': 'restore-one',
            'actor': 'human:fixture', 'purpose': 'full-restore', 'decision': 'approve',
            'issued_at': (now - timedelta(minutes=1)).isoformat(), 'expires_at': (now + timedelta(minutes=1)).isoformat(),
            'host_instance': 'fixture', 'snapshot_id': 'snapshot', 'manifest_sha256': 'a' * 64,
            'data_loss_acknowledged': True, 'external_reconciliation_acknowledged': True}
        late = {'restore_approval': {}, 'old_start_approval': {},
                **{key: decision[key] for key in ('host_instance', 'snapshot_id', 'manifest_sha256')}}
        with patch.object(recovery, 'read_reference', return_value=decision):
            self.assertEqual(recovery.approval(late, 'full-restore', Path('/fixture')), decision)
            with self.assertRaisesRegex(ContractError, 'recovery_approval_binding'):
                recovery.approval(late, 'old-start', Path('/fixture'))
            decision['expires_at'] = (now - timedelta(seconds=1)).isoformat()
            with self.assertRaisesRegex(ContractError, 'recovery_approval_expired'):
                recovery.approval(late, 'full-restore', Path('/fixture'))


if __name__ == '__main__':
    unittest.main()
