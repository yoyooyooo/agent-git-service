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
        patched('authorize_recovery', side_effect=lambda *_args: self.events.append('authorize'))
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


class PacketWindowTests(unittest.TestCase):
    """Real disk packet/load/admit/authorize; synthetic native/host boundaries.

    These fixtures neither provision a root peer nor execute a native operation.
    The package, window, candidate, original claim and snapshot are real bytes.
    """
    def setUp(self):
        import routeb_host_package as package
        from test_routeb_host_admission import AdmissionTests, package as make_package, approval as make_approval
        self.module = package
        self.fixture = AdmissionTests()
        self.fixture.setUp()
        self.addCleanup(self.fixture.doCleanups)
        self.base = self.fixture.root.resolve()
        self.root = self.base / 'txn-1/ags'
        self.root.mkdir(parents=True, mode=0o700)
        self.pending = self.root / 'state/ags-autodeploy/local-v2/pending'
        self.pending.mkdir(parents=True, mode=0o700)
        self.snapshot = self.root.parent / 'recovery/backups/snapshot-one'
        self.snapshot.mkdir(parents=True, mode=0o700)
        for directory in self.base.rglob('*'):
            if directory.is_dir():
                directory.chmod(0o700)
        self.now = datetime.now(timezone.utc)
        self.consumed = self.now - timedelta(hours=3)
        self.group = {'objects': []}
        self.profile = {key: 'b' * 64 for key in package.PROFILE_FIELDS}
        self.profile.update(schema=package.NATIVE_PROFILE_SCHEMA, root=str(self.root),
            tool_revision='c' * 40, tool_tree='d' * 40,
            interpreter={'path': '/synthetic/python', 'sha256': 'e' * 64},
            stage0={'registration_id': 'txn-1'}, capture={'manager': {'definition': str(self.base / 'unit')}})
        c = self.fixture.verified
        self.policy = dict(schema='ags.routeb-host-policy.v1', host='mini',
            old_revision=c.revision, old_tree=c.tree, candidate_revision=c.revision,
            candidate_tree=c.tree, schema_sha256=self.profile['schema_sha256'], allow_schema_change=False)
        build = dict(schema='ags.routeb-build-review.v1', revision=c.revision, tree=c.tree,
            binary_sha256=c.sha256, manifest_sha256=c.raw_manifest_sha256, decision='approve', reviewer='fixture')
        ci = dict(schema='ags.routeb-ci-evidence.v1', repository='yoyooyooo/agent-git-service',
            revision=c.revision, tree=c.tree, run_id=1, attempt=1, conclusion='success', evidence_sha256='f' * 64)
        for name, value, key in [('build-review.json', build, 'build_review_sha256'),
                ('ci-evidence.json', ci, 'ci_evidence_sha256'), ('recovery-review.json', {}, 'recovery_review_sha256')]:
            self.profile[key] = self.write(self.base / name, value)
        hashes = {name: self.write(self.base / name, value) for name, value in
                  [('native-profile.json', self.profile), ('policy.json', self.policy), ('recovery-group.json', self.group)]}
        self.pkg = make_package(c.raw_manifest_sha256, native_profile_sha256=hashes['native-profile.json'],
            policy_sha256=hashes['policy.json'], recovery_group_sha256=hashes['recovery-group.json'])
        self.path = self.base / 'package.json'
        self.approval_path = self.base / 'approval.json'
        self.package_sha = self.write(self.path, self.pkg)
        self.window = make_approval(self.package_sha, issued_at=(self.consumed-timedelta(hours=1)).isoformat(),
                                   expires_at=(self.consumed+timedelta(hours=1)).isoformat())
        self.approval_sha = self.write(self.approval_path, self.window)
        # Candidate loader verifies real binary and manifest bytes on every pass.
        (self.base / 'candidate-manifest.json').write_bytes((self.base / 'manifest.json').read_bytes())
        self.old = dict(schema='ags.installation.v2', source=c.revision,
                        binaries={'gh-server': {'sha256': 'a'*64, 'identity': {'tree': c.tree}}})
        self.write(self.root / 'state/installation.json', self.old)
        self.context = {'registration_id': 'txn-1', 'tool_revision': self.profile['tool_revision'],
            'tool_tree': self.profile['tool_tree'], 'tool_manifest_sha256': self.pkg['tool_sha256']}
        self.host = SimpleNamespace(scope='isolated-native', bundle=self.base,
            identity=SimpleNamespace(root=str(self.root), logical_host='mini', host_instance=self.pkg['host_instance']),
            recheck=Mock())
        self.stack = ExitStack(); self.addCleanup(self.stack.close)
        self.stack.enter_context(patch.object(recovery, 'recovery_binding', side_effect=lambda *a, **kw: (self.context, self.late)))
        self.stack.enter_context(patch.object(recovery, 'read_trusted', return_value=canonical({'logical_host':'mini'})))
        self.stack.enter_context(patch.object(package, 'require_interpreter_closure', return_value=self.context))
        self.stack.enter_context(patch.object(package, 'resolve_for_root', return_value=self.host))
        for name in ('validate_capture_profile', 'bundle_engine', 'verify_launch_dependencies',
                     'validate_native_paths', 'validate_authority_coverage', 'validate_group'):
            self.stack.enter_context(patch.object(package, name))
        self.stack.enter_context(patch.object(package, 'file_digest', return_value='a'*64))
        paths = dict(old_binary=self.root/'bin/gh-server', installation=self.root/'state/installation.json',
                     runtime_env=self.root/'config/runtime.env', launcher=self.root/'bin/ags-runtime.py',
                     manager=self.base/'unit', sqlite=self.root/'main.db', git_root=self.root/'git')
        self.stack.enter_context(patch.object(package, 'group_object', side_effect=lambda group, role: {'source':str(paths[role])}))
        self.stack.enter_context(patch.object(package, '_runtime_environment',
            return_value={'DB_DSN':'sqlite:'+str(paths['sqlite']), 'GIT_REPO_DIR':str(paths['git_root'])}))
        loaded = package.load(self.path, self.approval_path, 'mini', consumed_at=self.consumed)
        self.binding = loaded.cold_binding()
        self.write(self.pending/'claim.json', {'schema':'ags.local-claim.v2', 'binding':self.binding})
        self.journal = dict(schema='ags.routeb-host-journal.v1', root=str(self.root), transaction_id='txn-1',
            host_instance=self.pkg['host_instance'], package_sha256=self.package_sha, approval_sha256=self.approval_sha,
            binding_sha256=recovery.fingerprint(self.binding), started_at=self.consumed.isoformat())
        self.write(self.pending/'host-execution.json', self.journal)
        self.manifest = dict(schema='ags.backup-manifest.v2', snapshot_id='snapshot-one',
            recovery_group_sha256=recovery.fingerprint(self.group), authority_evidence={}, binding=self.binding,
            started_at=(self.consumed+timedelta(minutes=1)).isoformat(),
            ended_at=(self.consumed+timedelta(minutes=2)).isoformat())
        self.late = dict(snapshot_id='snapshot-one', group_sha256=recovery.fingerprint(self.group),
            host_instance=self.pkg['host_instance'], backup_directory=str(self.snapshot),
            manifest_sha256=self.write(self.snapshot/'manifest.json', self.manifest))
        for purpose, key in [('full-restore','restore_approval'), ('old-start','old_start_approval')]:
            decision = dict(schema='ags.routeb-recovery-approval.v1', approval_id=purpose+'-one',
                actor='human:fixture', purpose=purpose, decision='approve',
                issued_at=(self.now-timedelta(minutes=1)).isoformat(), expires_at=(self.now+timedelta(hours=1)).isoformat(),
                **{k:self.late[k] for k in ('host_instance','snapshot_id','manifest_sha256')},
                data_loss_acknowledged=True, external_reconciliation_acknowledged=True)
            path = self.base/(key+'.json'); sha = self.write(path, decision)
            self.late[key] = {'path':str(path), 'sha256':sha, 'size':path.stat().st_size}

    def write(self, path, value):
        raw = canonical(value); path.write_bytes(raw); path.chmod(0o600)
        return hashlib.sha256(raw).hexdigest()

    def test_packet_accepts_consumed_expired_window_for_both_independent_operations(self):
        for operation, purpose in [('restore','full-restore'), ('old-start','old-start')]:
            with self.subTest(operation=operation):
                _, late, loaded = recovery.packet(self.root, self.path, self.approval_path, operation)
                self.assertEqual(recovery.approval(late, purpose, self.root)['purpose'], purpose)
                recovery.authorize_recovery(loaded, self.context, late)
                # The same expired window still cannot admit a transaction.
                with self.assertRaisesRegex(ContractError, '^approval_expired$'):
                    loaded.authorize()

    def test_recovery_rejects_changed_history_and_invalid_consumption_time(self):
        _, _, loaded = recovery.packet(self.root, self.path, self.approval_path, 'restore')
        for change in ({'approval_sha256':'f'*64}, {'package_sha256':'f'*64},
                       {'started_at':(self.consumed-timedelta(hours=2)).isoformat()},
                       {'started_at':self.now.isoformat()}):
            with self.subTest(change=change):
                self.write(self.pending/'host-execution.json', dict(self.journal, **change))
                with self.assertRaises(ContractError):
                    recovery.authorize_recovery(loaded, self.context, self.late)
        self.write(self.pending/'host-execution.json', self.journal)
        (self.snapshot/'manifest.json').write_bytes(canonical(self.manifest)+b' ')
        with self.assertRaisesRegex(ContractError, '^recovery_consumption_snapshot$'):
            recovery.packet(self.root, self.path, self.approval_path, 'old-start')

    def test_expired_late_approval_still_refuses_a_new_operation(self):
        for operation, purpose, key in [('restore','full-restore','restore_approval'), ('old-start','old-start','old_start_approval')]:
            with self.subTest(operation=operation):
                _, late, _ = recovery.packet(self.root, self.path, self.approval_path, operation)
                path = Path(late[key]['path']); decision = json.loads(path.read_bytes())
                decision['issued_at'] = (self.now-timedelta(hours=2)).isoformat()
                decision['expires_at'] = (self.now-timedelta(hours=1)).isoformat()
                late[key]['sha256'] = self.write(path, decision); late[key]['size'] = path.stat().st_size
                with self.assertRaisesRegex(ContractError, '^recovery_approval_expired$'):
                    recovery.approval(late, purpose, self.root)


if __name__ == '__main__':
    unittest.main()
