#!/usr/bin/env python3
"""Exact-source cold-window component tests; not native host evidence."""
import copy
import hashlib
import json
from pathlib import Path
import subprocess
import sys
import unittest
from unittest.mock import Mock, patch

import upgrade_backup_contract as contract
import upgrade_engine as engine
import upgrade_recovery as cold
from test_upgrade_quiescence import ColdFixture


class ExactSourceFixture(ColdFixture):
    def setUp(self):
        super().setUp()
        self.b = {key: self.b[key] for key in
                  ('host_instance', 'deployment_id', 'transaction_id', 'attempt_id',
                   'authorization_id', 'old', 'new', 'runtime_tree', 'tool_sha256',
                   'policy_sha256', 'target', 'old_binary_sha256', 'new_binary_sha256')}
        self.b.update(schema=contract.EXACT_COLD_SCHEMA, artifact_kind='exact-source',
                      manifest_sha256='1' * 64, build_review_sha256='2' * 64,
                      ci_evidence_sha256='3' * 64)
        self.window.backup_binding = copy.deepcopy(self.b)
        self.identity = {'revision': self.b['new'], 'tree': self.b['runtime_tree'],
                         'version': 'source-' + self.b['new']}

    def install_receipt(self, **changes):
        self.binary.write_bytes(self.new)
        value = {'schema': 'ags.installation.v2', 'artifact_kind': 'exact-source',
                 'source': self.b['new'], 'runtime_tree': self.b['runtime_tree'],
                 'version': self.identity['version'], 'tool_sha256': self.b['tool_sha256'],
                 'candidate_manifest_sha256': self.b['manifest_sha256'],
                 'binaries': {'gh-server': {'sha256': self.b['new_binary_sha256'],
                                          'identity': self.identity}}}
        value.update(changes)
        engine.save(self.root / 'state/installation.json', value)



class ExactSourceColdTests(ExactSourceFixture):
    def test_exact_source_seals_verifies_and_consumes_one_start_without_release(self):
        self.prepare()
        self.install_receipt()
        self.window.installed(self.identity, lambda _: self.identity)
        self.window.start_intent()
        self.window.release()
        self.window.start()
        self.assertEqual(self.manager.starts, 1)
        self.assertFalse(cold.startup_permitted(self.root))
        _, journal, _ = cold.read_window(self.root)
        self.assertTrue(journal['handoff']['consumed'])
        self.assertEqual(journal['backup_binding']['artifact_kind'], 'exact-source')
        self.assertNotIn('release_id', journal['backup_binding'])
        with self.assertRaises(contract.ContractError):
            self.window.start()
        self.assertEqual(self.manager.starts, 1)

    def test_large_recovery_group_uses_snapshot_budget_and_keeps_validation(self):
        assets = self.root / 'config-assets'
        assets.mkdir(mode=0o700)
        dependencies = []
        for index in range(48):
            identity = 'config-' + str(index) + '-' + 'a' * 64
            path = assets / (str(index) + '.json')
            engine.save(path, {'fixture': index})
            self.group['objects'].append({
                'id': identity, 'role': 'config', 'source': str(path),
                'restore_target': str(path), 'kind': 'file',
                'depends_on': list(dependencies)})
            dependencies.append(identity)
        self.window.group = copy.deepcopy(self.group)
        group_raw = contract.canonical(self.group)
        self.assertGreater(len(group_raw), 65536)
        self.assertLess(len(group_raw), contract.MAX_JSON)
        contract.validate_group(self.group)

        self.prepare(until='restore_verified')
        backup = Path(self.window.journal['backup_dir'])
        stored_group = backup / 'recovery-group.json'
        self.assertEqual(stored_group.read_bytes(), group_raw)
        self.assertEqual(self.window.journal['phase'], 'restore_verified')
        report = contract.strict_json((backup / 'restore-report.json').read_bytes())
        self.assertTrue(all(report['checks'].values()))
        self.assertEqual(contract.strict_json((backup / 'restore-publication.json').read_bytes()),
                         {'state': 'published', 'manifest_sha256': report['manifest_sha256']})
        self.assertEqual(self.binary.read_bytes(), self.old)
        self.assertEqual((self.root / 'state/installation.json').read_bytes(), self.raw_old_receipt)
        self.assertEqual(self.manager.starts, 0)

        changed_group = copy.deepcopy(self.group)
        changed_group['dependency_review_sha256'] = '9' * 64
        stored_group.write_bytes(contract.canonical(changed_group))
        with self.assertRaisesRegex(contract.ContractError, '^group_mismatch$'):
            self.window.verified_backup()
        stored_group.write_bytes(group_raw + b' ' * (contract.MAX_JSON - len(group_raw) + 1))
        with self.assertRaisesRegex(cold.Failure, '^invalid_request$'):
            self.window.verified_backup()

    def test_release_or_wrong_candidate_identity_cannot_be_installed_as_exact_source(self):
        self.prepare()
        for changes in ({'release_id': 1}, {'control_revision': '4' * 40},
                        {'candidate_manifest_sha256': '5' * 64}, {'tool_sha256': '6' * 64},
                        {'version': 'source-other'}, {'artifact_kind': 'release'}):
            with self.subTest(changes=changes):
                self.install_receipt(**changes)
                with self.assertRaises(contract.ContractError):
                    self.window.installed(self.identity, lambda _: self.identity)
                self.assertEqual(self.window.journal['phase'], 'install_started')
                self.assertFalse(cold.startup_permitted(self.root))

    def test_backup_only_exact_binding_is_rejected_before_stop_or_marker(self):
        self.window.backup_binding = {key: self.b[key] for key in contract.EXACT_BINDING_FIELDS}
        with self.assertRaises(contract.ContractError):
            self.window.begin()
        self.assertFalse((self.root / 'state' / cold.MARKER).exists())
        self.assertIsNone(self.manager.proc.poll())


class InstalledReopenTests(ExactSourceFixture):
    """Source component recovery. No native capability or new CLI is granted."""
    def setUp(self):
        super().setUp()
        self.prepare()
        self.install_receipt()
        self.window.installed(self.identity, lambda _: self.identity)
        self.installed_journal = (self.pending / 'cold-journal.json').read_bytes()
        self.journal_sha = hashlib.sha256(self.installed_journal).hexdigest()

    def reopen_window(self):
        self.window.release()
        self.window.release_owner()
        self.window = cold.ColdWindow(self.root, self.request, self.b, self.group,
            'sqlite:' + str(self.storage.dbpath), self.storage.admission,
            self.manager, self.writers, self.trash)
        self.addCleanup(self.window.release_owner)
        self.addCleanup(self.window.release)
        self.window.acquire_owner()
        self.window.acquire()
        return self.window

    def test_fresh_process_reopens_installed_and_uses_the_original_start_once(self):
        request = self.root / 'component-reopen.json'
        engine.save(request, {'root': str(self.root), 'binding': self.request,
            'backup_binding': self.b, 'group': self.group,
            'dsn': 'sqlite:' + str(self.storage.dbpath), 'schema': self.storage.admission,
            'identity': self.identity, 'journal_sha256': self.journal_sha})
        self.window.release()
        self.window.release_owner()
        command = [sys.executable, '-B', '-c',
            'import sys;sys.path.insert(0,sys.argv[1]);'
            'import test_routeb_cold_window as fixture;fixture.reopen_component(sys.argv[2])',
            str(Path(__file__).resolve().parent), str(request)]
        first = subprocess.run(command, capture_output=True, text=True)
        self.assertEqual(first.returncode, 0, first.stderr)
        result = json.loads(first.stdout)
        _, journal, _ = cold.read_window(self.root)
        self.assertEqual(result, {'starts': 1, 'generation': journal['generation']})
        self.assertEqual(journal['generation'], json.loads(self.installed_journal)['generation'])
        self.assertTrue(journal['start_attempted'])
        self.assertTrue(journal['handoff']['consumed'])
        before = (self.pending / 'cold-journal.json').read_bytes()
        second = subprocess.run(command, capture_output=True, text=True)
        self.assertNotEqual(second.returncode, 0)
        self.assertEqual((self.pending / 'cold-journal.json').read_bytes(), before)
        self.assertEqual((self.pending / 'old-installation.json').read_bytes(), self.raw_old_receipt)
        self.assertEqual((self.pending / 'old-binary').read_bytes(), self.old)

    def test_reopen_reads_original_evidence_without_repeating_prior_effects(self):
        window = self.reopen_window()
        authorize = Mock()
        sealed = Path(json.loads(self.installed_journal)['backup_dir'])
        proofs = {name: (sealed / name).read_bytes()
                  for name in ('manifest.json', 'receipt.json', 'restore-report.json')}
        with patch.object(window, 'begin', side_effect=AssertionError('repeat begin')), \
             patch.object(window, 'stop', side_effect=AssertionError('repeat stop')), \
             patch.object(window, 'backup', side_effect=AssertionError('repeat backup')), \
             patch.object(window, 'restore', side_effect=AssertionError('repeat restore')), \
             patch.object(window, 'install_intent', side_effect=AssertionError('repeat install')):
            window.reopen_installed(self.identity, lambda _: self.identity,
                                    journal_sha256=self.journal_sha, authorize=authorize)
        authorize.assert_called_once_with()
        self.assertEqual((self.pending / 'cold-journal.json').read_bytes(), self.installed_journal)
        self.assertEqual(window.journal, json.loads(self.installed_journal))
        self.assertEqual({name: (sealed / name).read_bytes() for name in proofs}, proofs)
        self.assertEqual(self.manager.starts, 0)

    def assert_reopen_refused(self, window, code, **kwargs):
        journal = (self.pending / 'cold-journal.json').read_bytes()
        with self.assertRaisesRegex(contract.ContractError, '^' + code + '$'):
            window.reopen_installed(self.identity, lambda _: self.identity,
                journal_sha256=kwargs.pop('journal_sha256', hashlib.sha256(journal).hexdigest()),
                authorize=kwargs.pop('authorize', lambda: None), **kwargs)
        self.assertEqual((self.pending / 'cold-journal.json').read_bytes(), journal)
        self.assertEqual(self.manager.starts, 0)
        self.assertIsNone(window.journal)

    def test_owner_and_both_cold_locks_are_required(self):
        window = self.reopen_window()
        window.release()
        self.assert_reopen_refused(window, 'cold_locks_required')
        window.acquire()
        window.release_owner()
        self.assert_reopen_refused(window, 'cold_locks_required')

    def test_original_claim_candidate_and_journal_are_bound(self):
        window = self.reopen_window()
        self.assert_reopen_refused(window, 'installed_journal_changed', journal_sha256='9' * 64)
        window.binding['authorization_id'] = 'different-authorization'
        self.assert_reopen_refused(window, 'cold_binding_mismatch')
        window.binding = copy.deepcopy(self.request)
        window.backup_binding['manifest_sha256'] = '9' * 64
        self.assert_reopen_refused(window, 'cold_binding_mismatch')

    def test_partial_install_and_every_start_intent_remain_fenced(self):
        window = self.reopen_window()
        cases = ({'phase': 'install_started'}, {'phase': 'old_restored'},
                 {'phase': 'start_attempted', 'start_attempted': True},
                 {'start_attempted': True}, {'start_called': False},
                 {'handoff': {'consumed': False, 'manager_requested': False}})
        for changes in cases:
            with self.subTest(changes=changes):
                journal = json.loads(self.installed_journal)
                journal.update(changes)
                cold.replace_json(self.pending / 'cold-journal.json', journal)
                self.assert_reopen_refused(window, 'installed_continuation_refused')

    def test_retained_old_metadata_cannot_be_replaced_by_current_metadata(self):
        window = self.reopen_window()
        (self.pending / 'old-installation.json').write_bytes(
            (self.root / 'state/installation.json').read_bytes())
        self.assert_reopen_refused(window, 'old_evidence_mismatch')

    def test_complete_derived_receipt_refuses_extra_metadata_even_with_a_matching_raw_pin(self):
        window = self.reopen_window()
        path = self.root / 'state/installation.json'
        receipt = json.loads(path.read_bytes())
        receipt['unreviewed-extra-field'] = 'fixture'
        engine.save(path, receipt)
        journal = json.loads(self.installed_journal)
        journal['installed_receipt_sha256'] = hashlib.sha256(path.read_bytes()).hexdigest()
        cold.replace_json(self.pending / 'cold-journal.json', journal)
        self.assert_reopen_refused(window, 'installed_identity_mismatch')

    def test_installed_binary_or_marker_drift_cannot_publish_start_intent(self):
        window = self.reopen_window()
        self.binary.write_bytes(b'unreviewed candidate')
        self.assert_reopen_refused(window, 'installed_identity_mismatch')
        self.binary.write_bytes(self.new)
        engine.save(self.root / 'state/installing.json', {'incomplete': True})
        self.assert_reopen_refused(window, 'installed_identity_mismatch')

    def test_unknown_writers_and_protected_data_changes_refuse(self):
        window = self.reopen_window()
        window.writer_probe = lambda: {'generation': window.journal['generation'],
            'closed': False, 'writers': [], 'unknown': ['fixture-unreadable-holder']}
        self.assert_reopen_refused(window, 'writers_not_quiescent')
        window.writer_probe = self.writers
        environment = next(row for row in self.group['objects'] if row['role'] == 'runtime_env')
        with Path(environment['source']).open('ab') as stream:
            stream.write(b'UNREVIEWED_FIXTURE_SETTING=1\n')
        self.assert_reopen_refused(window, 'source_changed')

    def test_outside_group_change_does_not_expand_the_protected_domain(self):
        window = self.reopen_window()
        (self.root / 'unrelated-fixture-event').write_bytes(b'outside the recovery group')
        window.reopen_installed(self.identity, lambda _: self.identity,
                                journal_sha256=self.journal_sha, authorize=lambda: None)
        self.assertEqual(window.journal['phase'], 'installed')
        self.assertEqual((self.pending / 'cold-journal.json').read_bytes(), self.installed_journal)

    def test_recovery_proof_and_independent_authorization_are_required(self):
        window = self.reopen_window()
        expired = Mock(side_effect=contract.ContractError('approval_expired'))
        self.assert_reopen_refused(window, 'approval_expired', authorize=expired)
        expired.assert_called_once_with()
        backup = Path(json.loads(self.installed_journal)['backup_dir'])
        report_path = backup / 'restore-report.json'
        report = json.loads(report_path.read_bytes())
        report['checks'][next(iter(report['checks']))] = False
        cold.replace_json(report_path, report)
        with self.assertRaises(contract.ContractError):
            window.reopen_installed(self.identity, lambda _: self.identity,
                                    journal_sha256=self.journal_sha, authorize=lambda: None)
        self.assertIsNone(window.journal)
        self.assertEqual((self.pending / 'cold-journal.json').read_bytes(), self.installed_journal)
        self.assertEqual(self.manager.starts, 0)

    def test_authorization_cannot_admit_a_journal_that_changed_during_rebind(self):
        window = self.reopen_window()
        def changed():
            journal = json.loads(self.installed_journal)
            journal['start_attempted'] = True
            cold.replace_json(self.pending / 'cold-journal.json', journal)
        with self.assertRaisesRegex(contract.ContractError, '^installed_journal_changed$'):
            window.reopen_installed(self.identity, lambda _: self.identity,
                                    journal_sha256=self.journal_sha, authorize=changed)
        self.assertIsNone(window.journal)
        self.assertEqual(self.manager.starts, 0)


class ExactSourceReceiptTests(unittest.TestCase):
    def test_derivation_preserves_nonprimary_metadata_without_mutating_its_inputs(self):
        old = {'schema': 'ags.installation.v2', 'source': 'a' * 40,
               'release_id': 7, 'release_tag': 'fixture-release', 'control_revision': 'c' * 40,
               'attestation': {'fixture': True}, 'owner_metadata': {'purpose': 'fixture'},
               'binaries': {'gh-server': {'sha256': '4' * 64}, 'ags-edge': {'sha256': '5' * 64}}}
        binding = {'new': 'b' * 40, 'runtime_tree': 'd' * 40, 'new_binary_sha256': '6' * 64,
                   'tool_sha256': '7' * 64, 'manifest_sha256': '8' * 64}
        identity = {'revision': binding['new'], 'tree': binding['runtime_tree'], 'version': 'fixture-source'}
        original = copy.deepcopy((old, binding, identity))
        receipt = cold.exact_source_installation(old, binding, identity)
        self.assertEqual(receipt['owner_metadata'], old['owner_metadata'])
        self.assertEqual(receipt['binaries']['ags-edge'], old['binaries']['ags-edge'])
        self.assertEqual(receipt['binaries']['gh-server']['identity'], identity)
        self.assertFalse(any(key in receipt for key in ('release_id', 'release_tag', 'control_revision', 'attestation')))
        self.assertEqual((old, binding, identity), original)
        receipt['binaries']['gh-server']['identity']['revision'] = 'e' * 40
        self.assertEqual((old, binding, identity), original)


def reopen_component(path):
    """A separate Python process with fixture-only manager and writer callbacks."""
    value = json.loads(Path(path).read_bytes())
    root = Path(value['root'])
    class Manager:
        starts = 0
        def start(self):
            self.starts += 1
            return cold.consume_start(root)
    manager = Manager()
    window = cold.ColdWindow(root, value['binding'], value['backup_binding'], value['group'],
                            value['dsn'], value['schema'], manager, None, None)
    window.writer_probe = lambda: {'generation': window.journal['generation'],
                                  'closed': True, 'writers': [], 'unknown': []}
    window.acquire_owner()
    try:
        window.acquire()
        window.reopen_installed(value['identity'], lambda _: value['identity'],
                                journal_sha256=value['journal_sha256'], authorize=lambda: None)
        window.start_intent()
        window.release()
        window.start(authorize=lambda: None)
        print(json.dumps({'starts': manager.starts, 'generation': window.journal['generation']}))
    finally:
        window.release()
        window.release_owner()


if __name__ == '__main__':
    unittest.main()
