#!/usr/bin/env python3
"""Exact-source cold-window component tests; not native host evidence."""
import copy
from pathlib import Path
import unittest

import upgrade_backup_contract as contract
import upgrade_engine as engine
import upgrade_recovery as cold
from test_upgrade_quiescence import ColdFixture


class ExactSourceColdTests(ColdFixture):
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


if __name__ == '__main__':
    unittest.main()
