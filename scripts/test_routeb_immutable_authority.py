#!/usr/bin/env python3
"""Authority contracts and archive semantics. Provision receipts are fixtures.

The filesystem cases read one existing root-owned executable, never modify it.
Only the receipt/manifest reader is injected; these are component tests, not a
native host-package provision or M6 acceptance result.
"""
import copy
import hashlib
import os
from pathlib import Path
import unittest
from unittest.mock import patch

import routeb_immutable_authority as authority
import upgrade
import upgrade_backup as backup
import upgrade_backup_contract as contract
import upgrade_restore_verify as restore
import test_upgrade_backup as storage_fixtures


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def descriptor():
    return {'id': 'tool', 'role': 'tool', 'kind': 'file', 'source': '/usr/local/libexec/fixture/tool.py',
            'restore_target': '/usr/local/libexec/fixture/tool.py', 'depends_on': [],
            'immutable_authority': {'schema': authority.SCHEMA, 'host_instance': 'mini-1',
                'asset': '/fixture/assets/tool.py', 'asset_sha256': '1' * 64,
                'provision_receipt': '/usr/local/libexec/fixture/receipt.json', 'provision_receipt_sha256': '2' * 64,
                'provision_approval': '/usr/local/libexec/fixture/approval.json', 'provision_approval_sha256': '3' * 64,
                'tool_manifest': '/usr/local/libexec/fixture/manifest.json', 'tool_manifest_sha256': '4' * 64,
                'metadata': {'mode': 0o644, 'uid': 0, 'gid': 0, 'mtime_ns': 1,
                             'acl_sha256': '5' * 64, 'xattrs_sha256': '6' * 64}, 'size': 10}}


class AuthorityContractTests(unittest.TestCase):
    def test_root_authority_is_explicit_and_preserves_the_existing_role(self):
        item = descriptor()
        authority.validate(item)
        for role in authority.ROLES:
            authority.validate({**item, 'role': role})
        for role in ('sqlite', 'runtime_env', 'old_binary', 'installation', 'external_authority'):
            with self.subTest(role=role), self.assertRaises(contract.ContractError):
                authority.validate({**item, 'role': role})

    def test_user_owned_writable_or_directory_cannot_be_frozen_by_a_flag(self):
        for change in ({'uid': 501}, {'mode': 0o666}, {'mode': 0o4755}):
            item = descriptor()
            item['immutable_authority']['metadata'].update(change)
            with self.subTest(change=change), self.assertRaises(contract.ContractError):
                authority.validate(item)
        with self.assertRaises(contract.ContractError):
            authority.validate({**descriptor(), 'kind': 'directory'})

    def test_asset_cannot_point_back_at_the_live_object(self):
        item = descriptor(); item['immutable_authority']['asset'] = item['source']
        with self.assertRaises(contract.ContractError):
            authority.validate(item)

    def test_runtime_write_access_to_an_ancestor_refuses_even_root_owned_files(self):
        with patch.object(authority, 'read_trusted', return_value=b'bytes'), \
                patch.object(authority.os, 'getuid', return_value=501), \
                patch.object(authority.os, 'access', side_effect=lambda path, _mode: str(path) == '/usr/local/libexec/fixture'):
            with self.assertRaisesRegex(contract.ContractError, 'runtime_writable'):
                authority._authority_bytes(Path(descriptor()['source']), 100)


@unittest.skipIf(os.getuid() == 0, 'archive ownership case requires the non-root runtime account')
class AuthorityArchiveTests(unittest.TestCase):
    def setUp(self):
        self.storage = storage_fixtures.BackupTests(); self.storage.setUp()
        self.addCleanup(self.storage.doCleanups)
        self.storage.repo()
        self.group = self.storage.prepared_group()
        self.group['schema'] = 'ags.recovery-group.v2'
        self.source = Path('/usr/bin/true')
        if not self.source.is_file() or self.source.is_symlink() or self.source.stat().st_uid != 0:
            self.skipTest('root-owned ordinary read-only fixture unavailable')
        self.asset = self.storage.root / 'tool-asset'
        self.asset.write_bytes(self.source.read_bytes()); self.asset.chmod(0o600)
        metadata = backup.metadata_fields(backup.supported_metadata(self.source))
        self.manifest_path = Path('/usr/bin/routeb-fixture-manifest.json')
        manifest_raw = contract.canonical({'schema': 'ags.upgrade-tool-bundle.v2', 'entry': 'upgrade_host.py',
                                           'files': {'true': digest(self.asset.read_bytes())}})
        self.binding = storage_fixtures.binding(); self.binding['tool_sha256'] = digest(manifest_raw)
        obj = next(item for item in self.group['objects'] if item['role'] == 'tool')
        obj.update(source=str(self.source), restore_target=str(self.source), kind='file')
        approved_object = {'role': 'tool', 'source': str(self.source),
                           'asset_sha256': digest(self.asset.read_bytes()), 'metadata': metadata}
        approval = {'schema': 'ags.immutable-authority-approval.v1', 'authorization_id': 'native-provision-1',
                    'actor': 'human:component-fixture', 'decision': 'approve',
                    'host_instance': self.binding['host_instance'], 'tool_manifest_sha256': digest(manifest_raw),
                    'installer_sha256': '7' * 64, 'issued_at': '2026-11-05T00:00:00+00:00',
                    'expires_at': '2026-11-05T01:00:00+00:00', 'objects': [approved_object]}
        approval_raw = contract.canonical(approval)
        receipt = {'schema': authority.PROVISION_SCHEMA, **approved_object,
                   'host_instance': self.binding['host_instance'], 'tool_manifest_sha256': digest(manifest_raw),
                   'installer_sha256': '7' * 64, 'provisioned_at': '2026-11-05T00:01:00+00:00',
                   'decision': 'provisioned', 'approval_sha256': digest(approval_raw),
                   'authorization_id': 'native-provision-1'}
        receipt_raw = contract.canonical(receipt)
        self.receipt_path = Path('/usr/bin/routeb-fixture-provision.json')
        self.approval_path = Path('/usr/bin/routeb-fixture-approval.json')
        obj['immutable_authority'] = {'schema': authority.SCHEMA, 'host_instance': self.binding['host_instance'],
                    'asset': str(self.asset), 'asset_sha256': digest(self.asset.read_bytes()),
                    'provision_receipt': str(self.receipt_path), 'provision_receipt_sha256': digest(receipt_raw),
                    'provision_approval': str(self.approval_path), 'provision_approval_sha256': digest(approval_raw),
                    'tool_manifest': str(self.manifest_path), 'tool_manifest_sha256': digest(manifest_raw),
                    'metadata': metadata, 'size': self.source.stat().st_size}
        self.object = obj
        self.raw_fixtures = {self.manifest_path: manifest_raw, self.receipt_path: receipt_raw, self.approval_path: approval_raw}
        real_read = authority.read_trusted
        def reader(path, **kwargs):
            return self.raw_fixtures[Path(path)] if Path(path) in self.raw_fixtures else real_read(Path(path), **kwargs)
        reader_patch = patch.object(authority, 'read_trusted', side_effect=reader)
        reader_patch.start(); self.addCleanup(reader_patch.stop)
        bundle_patch = patch.object(upgrade, 'bundle_engine', return_value=Path('/usr/bin/upgrade_host.py'))
        bundle_patch.start(); self.addCleanup(bundle_patch.stop)

    def take(self):
        return backup.create_sealed_backup(self.group, 'sqlite:' + str(self.storage.dbpath), self.storage.admission,
                    self.binding, {'nonce': 'nonce1', 'stop_evidence_sha256': 'a' * 64, 'writers_evidence_sha256': 'b' * 64},
                    'authority-snapshot', now='2026-11-05T00:02:00+00:00')

    def test_archive_has_runtime_owner_and_separate_original_root_metadata(self):
        sealed = self.take()
        manifest = sealed['manifest']
        row = next(item for item in manifest['entries'] if item['object_id'] == self.object['id'])
        self.assertEqual(row['uid'], os.getuid())
        self.assertEqual(row['mode'], 0o600)
        self.assertEqual(manifest['authority_evidence'][self.object['id']]['metadata']['uid'], 0)
        self.assertEqual(sealed['receipt']['schema'], 'ags.local-backup.v3')
        report = restore.verify_sealed_backup(sealed['backup_dir'], trash=lambda path: Path(path).rename(
                    self.storage.root / ('retained-' + Path(path).name)), group=self.group,
                    dsn='sqlite:' + str(self.storage.dbpath), schema=self.storage.admission)
        self.assertEqual(report['schema'], 'ags.restore-verification.v2')
        self.assertTrue(report['checks']['immutable_authorities'])
        self.assertEqual(report['authority_evidence_sha256'], contract.fingerprint(manifest['authority_evidence']))
        bad = copy.deepcopy(manifest); bad['schema'] = 'ags.backup-manifest.v1'
        raw = contract.canonical(bad)
        receipt = {**sealed['receipt'], 'manifest_sha256': digest(raw)}
        with self.assertRaises(contract.ContractError):
            contract.validate_seal(raw, contract.canonical(receipt), self.group)

    def test_missing_receipt_and_changed_asset_refuse_before_snapshot(self):
        self.asset.write_bytes(b'changed')
        with self.assertRaises(contract.ContractError):
            self.take()
        self.assertEqual(list(self.storage.backups.iterdir()), [])
        self.asset.write_bytes(self.source.read_bytes())
        self.raw_fixtures[self.receipt_path] = b'{}'
        with self.assertRaises(contract.ContractError):
            self.take()
        self.assertEqual(list(self.storage.backups.iterdir()), [])

    def test_old_group_schema_rejects_the_new_dependency_marker(self):
        group = copy.deepcopy(self.group); group['schema'] = 'ags.recovery-group.v1'
        with self.assertRaises(contract.ContractError):
            contract.validate_group(group)


if __name__ == '__main__':
    unittest.main()
