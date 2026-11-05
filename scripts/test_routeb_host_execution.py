#!/usr/bin/env python3
"""Real cold backup/restore and installation failures, using inert fixture binaries.

These component tests do not replace native manager or M6 acceptance evidence.
"""
import copy
import hashlib
import os
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import routeb_host_execution as execution
import upgrade_recovery as cold
import upgrade_state
from upgrade_backup_contract import ContractError, canonical
import test_routeb_cold_window as cold_fixtures


class InstallerTests(unittest.TestCase):
    def setUp(self):
        self.fixture = cold_fixtures.ExactSourceColdTests()
        self.fixture.setUp()
        self.addCleanup(self.fixture.doCleanups)
        self.fixture.prepare()
        fixture = self.fixture
        candidate = fixture.root / 'reviewed-candidate'
        candidate.write_bytes(fixture.new)
        candidate.chmod(0o700)
        self.package = SimpleNamespace(
            root=fixture.root, old_receipt=copy.deepcopy(fixture.old_receipt),
            old_receipt_sha256=hashlib.sha256(fixture.raw_old_receipt).hexdigest(),
            candidate=SimpleNamespace(binary_path=candidate, sha256=fixture.b['new_binary_sha256'],
                revision=fixture.b['new'], tree=fixture.b['runtime_tree'],
                raw_manifest_sha256=fixture.b['manifest_sha256'], value={'version': fixture.identity}),
            package={'tool_sha256': fixture.b['tool_sha256'], 'transaction_id': 'fixture-transaction'},
            authorize=Mock())
        self.operation = execution.HostExecution.__new__(execution.HostExecution)
        self.operation.package = self.package
        self.operation.root = fixture.root
        self.operation.binding = fixture.b
        self.operation.window = fixture.window

    def test_exact_source_install_keeps_full_cold_binding_and_uses_original_receipt(self):
        fixture = self.fixture
        with patch.object(execution, 'read_version', return_value=fixture.identity):
            self.operation.install()
        self.assertEqual(fixture.binary.read_bytes(), fixture.new)
        installed, _ = execution.private_json(fixture.root / 'state/installation.json')
        self.assertEqual(installed['artifact_kind'], 'exact-source')
        self.assertEqual(installed['candidate_manifest_sha256'], fixture.b['manifest_sha256'])
        self.assertEqual(installed['tool_sha256'], fixture.b['tool_sha256'])
        self.assertEqual(installed['binaries']['gh-server']['identity'], fixture.identity)
        self.assertFalse(any(key in installed for key in ('release_id', 'release_tag', 'control_revision')))
        self.assertEqual(fixture.window.journal['phase'], 'installed')
        self.assertEqual((fixture.pending / 'old-installation.json').read_bytes(), fixture.raw_old_receipt)
        self.assertEqual((fixture.pending / 'old-binary').read_bytes(), fixture.old)
        self.assertFalse((fixture.root / 'state/installing.json').exists())
        self.assertFalse(cold.startup_permitted(fixture.root))

    def test_expired_authorization_cannot_replace_the_first_byte(self):
        self.package.authorize.side_effect = ContractError('approval_expired')
        with self.assertRaisesRegex(ContractError, 'approval_expired'):
            self.operation.install()
        self.assertEqual(self.fixture.binary.read_bytes(), self.fixture.old)
        self.assertEqual((self.fixture.root / 'state/installation.json').read_bytes(), self.fixture.raw_old_receipt)
        self.assertFalse((self.fixture.root / 'state/installing.json').exists())

    def test_candidate_replacement_is_rejected_before_install_marker(self):
        self.package.candidate.binary_path.write_bytes(b'unreviewed bytes')
        with self.assertRaisesRegex(ContractError, 'candidate_replaced'):
            self.operation.install()
        self.assertEqual(self.fixture.binary.read_bytes(), self.fixture.old)
        self.assertFalse((self.fixture.root / 'state/installing.json').exists())

    def test_stop_authorization_rechecks_old_installation_and_binary(self):
        fixture = self.fixture
        self.operation.authorize_stop()
        path = fixture.root / 'state/installation.json'
        path.write_bytes(fixture.raw_old_receipt + b'\n')
        with self.assertRaisesRegex(ContractError, 'old_installation_changed'):
            self.operation.authorize_stop()
        path.write_bytes(fixture.raw_old_receipt)
        fixture.binary.write_bytes(b'changed old binary')
        with self.assertRaisesRegex(ContractError, 'old_binary_changed'):
            self.operation.authorize_stop()

    def test_interruption_between_binary_and_receipt_preserves_fence_and_raw_preimage(self):
        real_replace = os.replace

        def interrupt(source, target, **kwargs):
            if target == 'installation.json':
                raise OSError('synthetic publication interruption')
            return real_replace(source, target, **kwargs)

        with patch.object(execution.os, 'replace', side_effect=interrupt):
            with self.assertRaises(OSError):
                self.operation.install()
        self.assertEqual(self.fixture.binary.read_bytes(), self.fixture.new)
        self.assertEqual((self.fixture.root / 'state/installation.json').read_bytes(), self.fixture.raw_old_receipt)
        self.assertTrue((self.fixture.root / 'state/installing.json').is_file())
        self.assertEqual(self.fixture.window.journal['phase'], 'install_started')
        self.assertFalse(cold.startup_permitted(self.fixture.root))
        self.assertEqual(self.fixture.manager.starts, 0)


    def test_expired_start_approval_cannot_publish_a_consumable_handoff(self):
        fixture = self.fixture
        with patch.object(execution, 'read_version', return_value=fixture.identity):
            self.operation.install()
        fixture.window.start_intent()
        fixture.window.release()
        with self.assertRaisesRegex(ContractError, 'approval_expired'):
            fixture.window.start(authorize=Mock(side_effect=ContractError('approval_expired')))
        _, journal, _ = cold.read_window(fixture.root)
        self.assertTrue(journal['start_called'])
        self.assertFalse(journal['handoff']['manager_requested'])
        self.assertFalse(journal['handoff']['consumed'])
        self.assertFalse(cold.startup_permitted(fixture.root))
        self.assertEqual(fixture.manager.starts, 0)
        before = (fixture.pending / 'cold-journal.json').read_bytes()
        with self.assertRaisesRegex(ContractError, 'start_already_requested'):
            fixture.window.start(authorize=Mock())
        self.assertEqual((fixture.pending / 'cold-journal.json').read_bytes(), before)
        self.assertEqual(fixture.manager.starts, 0)


class AdmissionTests(unittest.TestCase):
    def test_status_cannot_be_used_to_read_an_arbitrary_private_json_file(self):
        with self.assertRaisesRegex(ContractError, 'status_journal_path_invalid'):
            execution.status('/private/operator/credentials.json', 'txn')

    def test_exception_text_is_not_copied_into_public_diagnostics(self):
        self.assertEqual(execution.failure_code(OSError('token=synthetic-secret')), 'host_execution_failed')

    def test_lifecycle_requires_host_active_epoch_and_resolved_effects(self):
        package = SimpleNamespace(root=Path('/unused'), package={'host_instance': 'fixture-host'})
        healthy = {'format': {'host_instance': 'fixture-host'},
                   'projection': {'epoch': 1, 'authority': {'state': 'active', 'epoch': 1},
                                  'pending_effects': []}}
        with patch.object(upgrade_state, 'load_verified', return_value=healthy):
            self.assertEqual(execution.lifecycle_blockers(package), [])
        cases = [(None, 'active_lifecycle_authority_required'),
                 ({'state': 'suspended', 'epoch': 1}, 'active_lifecycle_authority_required'),
                 ({'state': 'active', 'epoch': 2}, 'cold_epoch_unsupported')]
        for authority, expected in cases:
            value = copy.deepcopy(healthy)
            value['projection']['authority'] = authority
            with patch.object(upgrade_state, 'load_verified', return_value=value):
                self.assertIn(expected, execution.lifecycle_blockers(package))
        value = copy.deepcopy(healthy)
        value['projection']['pending_effects'] = ['unknown-start']
        with patch.object(upgrade_state, 'load_verified', return_value=value):
            self.assertIn('lifecycle_effect_unknown', execution.lifecycle_blockers(package))


class UnknownStartTests(unittest.TestCase):
    def test_unknown_start_retains_durable_claim_and_never_replays_manager(self):
        fixture = cold_fixtures.ExactSourceColdTests()
        fixture.setUp()
        self.addCleanup(fixture.doCleanups)
        fixture.window.release_owner()
        fixture.pending.rename(fixture.pending.with_name('fixture-initial-claim-retained'))
        candidate = fixture.root / 'reviewed-candidate'
        candidate.write_bytes(fixture.new)
        candidate.chmod(0o700)
        log = fixture.root / 'logs/runtime.log'
        log.write_bytes(b'fixture startup log\n')
        log.chmod(0o600)
        package = SimpleNamespace(
            root=fixture.root, pending=fixture.pending, group=fixture.group,
            dsn='sqlite:' + str(fixture.storage.dbpath), old_receipt=copy.deepcopy(fixture.old_receipt),
            old_receipt_sha256=hashlib.sha256(fixture.raw_old_receipt).hexdigest(),
            approval_sha256='5' * 64, package_raw=b'fixture admitted package',
            package={'transaction_id': 'fixture-transaction', 'host_instance': fixture.b['host_instance'],
                     'tool_sha256': fixture.b['tool_sha256']},
            profile={'schema_sha256': fixture.storage.admission, 'trash_root': '/fixture-unused-trash',
                     'capture': {'logs': [{'name': 'runtime', 'path': str(log)}]}},
            candidate=SimpleNamespace(binary_path=candidate, sha256=fixture.b['new_binary_sha256'],
                revision=fixture.b['new'], tree=fixture.b['runtime_tree'],
                raw_manifest_sha256=fixture.b['manifest_sha256'], value={'version': fixture.identity}),
            host=SimpleNamespace(scope='fixture'), cold_binding=lambda: copy.deepcopy(fixture.b), authorize=Mock())
        fixture.manager.preflight = Mock(return_value={'runtime': fixture.manager.record})

        class FixtureTrash:
            receipts = []

            def __call__(self, path):
                fixture.trash(path)

        def writer_factory(_manager, _group, generation):
            return lambda: {'generation': generation(), 'closed': True, 'writers': [], 'unknown': []}

        def version(path):
            return fixture.identity if Path(path).read_bytes() == fixture.new else fixture.old_receipt['binaries']['gh-server']['identity']

        def boundary(pkg):
            return {'blockers': ['transaction_already_claimed'] if pkg.pending.exists() else []}

        with patch.object(execution, 'FixedManager', return_value=fixture.manager), \
                patch.object(execution, 'SystemTrash', return_value=FixtureTrash()), \
                patch.object(execution, 'WriterProbe', side_effect=writer_factory), \
                patch.object(execution, 'lifecycle_blockers', return_value=[]), \
                patch.object(execution, 'preflight', side_effect=boundary), \
                patch.object(execution, 'read_version', side_effect=version):
            operation = execution.HostExecution(package)
            with patch.object(operation, 'observe_start', side_effect=ContractError('start_observation_unknown')):
                with self.assertRaisesRegex(ContractError, 'start_observation_unknown'):
                    operation.run()
            journal, _ = execution.private_json(fixture.pending / execution.JOURNAL)
            self.assertEqual(journal['status'], 'failed')
            self.assertTrue(journal['recovery_required'])
            self.assertTrue(journal['start_attempted'])
            self.assertEqual(fixture.manager.starts, 1)
            self.assertTrue((fixture.pending / 'old-installation.json').is_file())
            self.assertTrue((Path(operation.window.journal['backup_dir']) / 'restore-report.json').is_file())
            saved = (fixture.pending / execution.JOURNAL).read_bytes()
            with self.assertRaisesRegex(ContractError, 'execution_preflight_blocked'):
                execution.HostExecution(package).run()
            self.assertEqual(fixture.manager.starts, 1)
            self.assertEqual((fixture.pending / execution.JOURNAL).read_bytes(), saved)


if __name__ == '__main__':
    unittest.main()
