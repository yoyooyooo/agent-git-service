#!/usr/bin/env python3
"""Behavior regressions for cold backup evidence, retention and offline recovery."""
import hashlib
import os
from pathlib import Path
import sqlite3
import subprocess
import sys
import unittest
from unittest.mock import patch

import upgrade_backup as backup
import upgrade_backup_contract as c
import upgrade_inventory as inv
import upgrade_restore_verify as restore
import test_upgrade_restore as fixtures
from test_upgrade_inventory import fixture_db


class BackupRegressionTests(unittest.TestCase):
    def setUp(self):
        self.fixture = fixtures.RestoreTests()
        self.fixture.setUp()
        self.addCleanup(self.fixture.doCleanups)
        self.t = self.fixture

    def seal(self, *, group=None, snapshot='snapshot1', now=None):
        self.t.repo()
        return self.t.take(group, snapshot_id=snapshot, now=now)

    def verify(self, sealed, **kwargs):
        kwargs.setdefault('group', self.t.last_group)
        kwargs.setdefault('schema', self.t.admission)
        kwargs.setdefault('dsn', 'sqlite:' + str(self.t.dbpath))
        kwargs.setdefault('trash', lambda path: None)
        return restore.verify_sealed_backup(sealed['backup_dir'], **kwargs)

    def reseal_payload(self, sealed):
        """Describe modified bytes accurately so semantic checks must reject independently."""
        root = Path(sealed['backup_dir'])
        manifest = c.strict_json((root / 'manifest.json').read_bytes())
        entries = []
        for item in manifest['entries']:
            path = root / item['path']
            if not path.exists():
                continue
            item.update(backup.supported_metadata(path))
            if path.is_file():
                item.update(size=path.stat().st_size, sha256=inv.file_digest(path))
            entries.append(item)
        manifest['entries'] = entries
        raw = c.canonical(manifest)
        receipt = c.strict_json((root / 'receipt.json').read_bytes())
        receipt['manifest_sha256'] = hashlib.sha256(raw).hexdigest()
        (root / 'manifest.json').write_bytes(raw)
        (root / 'receipt.json').write_bytes(c.canonical(receipt))
        (root / 'publication.json').write_bytes(c.canonical({'state': 'published',
                                                           'manifest_sha256': receipt['manifest_sha256']}))
        sealed.update(manifest=manifest, receipt=receipt)

    def test_b1_missing_inputs_and_invalid_seal_refuse_without_report(self):
        sealed = self.seal()
        for key in ('schema', 'dsn', 'group'):
            with self.subTest(key=key), self.assertRaisesRegex(c.ContractError, 'restore_input_missing'):
                self.verify(sealed, **{key: None})
        receipt = Path(sealed['backup_dir']) / 'receipt.json'
        value = c.strict_json(receipt.read_bytes()); value['state'] = 'incomplete'
        receipt.write_bytes(c.canonical(value))
        with self.assertRaises(c.ContractError):
            self.verify(sealed)
        self.assertFalse((receipt.parent / 'restore-report.json').exists())

    def test_b1_missing_or_changed_inventory_refuses(self):
        sealed = self.seal()
        path = Path(sealed['backup_dir']) / 'inventory.json'
        value = c.strict_json(path.read_bytes()); value['logical_sha256'] = '0' * 64
        path.write_bytes(c.canonical(value))
        with self.assertRaisesRegex(c.ContractError, 'inventory_digest_mismatch'):
            self.verify(sealed)
        path.rename(path.with_name('missing-inventory'))
        with self.assertRaises((c.ContractError, FileNotFoundError)):
            self.verify(sealed)

    def test_b1_bad_cas_rejected_even_when_manifest_matches(self):
        self.t.repo()
        body = b'body' * 1200
        sha = self.t.cas(body)
        self.t.insert('wiki_blob_refs', blob_sha=sha, size=len(body))
        sealed = self.t.take()
        path = Path(sealed['backup_dir']) / 'git_root/.wikiblobs' / sha[:2] / sha[2:4] / sha
        path.write_bytes(b'BAD!' * 1200)
        self.reseal_payload(sealed)
        with self.assertRaisesRegex(c.ContractError, 'cas_digest_invalid'):
            self.verify(sealed)
        self.assertFalse((Path(sealed['backup_dir']) / 'restore-report.json').exists())

    def test_b1_db_only_oid_checked_after_matching_manifest(self):
        repo = self.t.repo()
        commit = self.t.commit(repo)
        self.t.insert('deployments', id=1, repository_id=1, ref=commit)
        sealed = self.t.take()
        copied = Path(sealed['backup_dir']) / 'git_root/sample/project.git'
        obj = copied / 'objects' / commit[:2] / commit[2:]
        obj.rename(self.t.root / 'removed-object')
        self.reseal_payload(sealed)
        with self.assertRaisesRegex(c.ContractError, 'git_validation_failed'):
            self.verify(sealed)

    def test_b1_logical_digest_and_dependencies_are_not_optional(self):
        sealed = self.seal()
        root = Path(sealed['backup_dir'])
        with fixture_db(root / 'sqlite') as db:
            db.execute("UPDATE repositories SET git_storage_id='changed'")
        self.reseal_payload(sealed)
        with self.assertRaisesRegex(c.ContractError, 'sqlite_logical_mismatch'):
            self.verify(sealed)
        # Restore the logical fact, but introduce an undeclared environment dependency.
        with fixture_db(root / 'sqlite') as db:
            db.execute("UPDATE repositories SET git_storage_id='not-a-directory'")
        env = root / 'runtime_env'
        env.write_bytes(env.read_bytes() + b'CERT_FILE=/srv/undeclared.pem\n')
        self.reseal_payload(sealed)
        with self.assertRaisesRegex(c.ContractError, 'configuration_dependency_missing'):
            self.verify(sealed)

    def test_b2_invalid_reports_and_symlinks_do_not_count(self):
        group = self.t.prepared_group()
        for index in range(6):
            path = self.t.backups / ('snap' + str(index)); path.mkdir()
            (path / 'receipt.json').write_bytes(b'{}')
            (path / 'restore-report.json').write_bytes(b'{"checks":false}')
        outside = self.t.root / 'outside'; outside.mkdir()
        (self.t.backups / 'link').symlink_to(outside, target_is_directory=True)
        calls = []
        listed = backup.list_backups(self.t.backups, group=group, host_instance='fixture')
        self.assertEqual(listed['verified'], [])
        backup.apply_retention(self.t.backups, group=group, host_instance='fixture', trash=calls.append)
        self.assertEqual(calls, [])

    def test_b2_root_outside_traversal_and_symlink_never_reach_trash(self):
        group = self.t.prepared_group()
        outside = self.t.root / 'outside'; outside.mkdir()
        link = self.t.backups / 'link'; link.symlink_to(outside, target_is_directory=True)
        for path in (self.t.backups, outside, self.t.backups / '..' / 'outside', link):
            calls = []
            with self.subTest(path=path), self.assertRaisesRegex(c.ContractError, 'retention_path_invalid'):
                backup.apply_retention(self.t.backups, [{'path': path, 'state': 'restore_verified'}],
                                       group=group, host_instance='fixture', trash=calls.append)
            self.assertEqual(calls, [])

    def test_b2_verified_time_not_mtime_and_host_report_binding(self):
        self.t.repo(); group = self.t.prepared_group(); sealed = []
        for index in range(6):
            item = self.t.take(group, snapshot_id='snap' + str(index),
                               now='2026-01-0' + str(index + 1) + 'T00:00:00+00:00')
            self.verify(item)
            os.utime(item['backup_dir'], (10000 - index, 10000 - index))
            sealed.append(item)
        calls = []
        backup.apply_retention(self.t.backups, group=group, host_instance='another-host', trash=calls.append)
        self.assertEqual(calls, [])
        backup.apply_retention(self.t.backups, group=group, host_instance='fixture', trash=calls.append)
        self.assertEqual([p.name for p in calls], ['snap0'])
        report = Path(sealed[0]['backup_dir']) / 'restore-report.json'
        value = c.strict_json(report.read_bytes()); value['manifest_sha256'] = '0' * 64
        report.write_bytes(c.canonical(value))
        self.assertEqual(len(backup.list_backups(self.t.backups, group=group,
                                               host_instance='fixture')['verified']), 5)
        calls.clear()
        backup.apply_retention(self.t.backups, group=group, host_instance='fixture', trash=calls.append)
        self.assertEqual(calls, [])

    def test_b3_failed_restore_copy_is_retained_and_not_trashed(self):
        sealed = self.seal()
        calls = []
        with patch.object(restore, 'database_inventory', side_effect=c.ContractError('injected')):
            with self.assertRaisesRegex(c.ContractError, 'injected'):
                self.verify(sealed, trash=calls.append)
        self.assertEqual(calls, [])
        copies = list(self.t.restores.iterdir())
        self.assertEqual(len(copies), 1)
        self.assertTrue((copies[0] / 'failure.json').is_file())
        self.assertFalse((Path(sealed['backup_dir']) / 'restore-report.json').exists())

    def test_b4_corrupted_copy_never_publishes_seal(self):
        original = backup.copy_file
        def corrupt(source, destination):
            original(source, destination)
            if source.name == 'old_binary':
                destination.write_bytes(b'corrupt copied binary')
        with patch.object(backup, 'copy_file', corrupt), self.assertRaisesRegex(c.ContractError, 'payload_mismatch'):
            self.seal()
        self.assertFalse((self.t.backups / 'snapshot1').exists())
        self.assertTrue((self.t.backups / '.partial-snapshot1/failure.json').exists())

    def test_b4_manifest_receipt_report_are_file_fsynced_before_publish(self):
        fsynced = []
        original = os.fsync
        def record(fd):
            fsynced.append((os.fstat(fd).st_dev, os.fstat(fd).st_ino))
            return original(fd)
        with patch.object(os, 'fsync', record):
            sealed = self.seal()
            self.verify(sealed)
        root = Path(sealed['backup_dir'])
        for name in ('inventory.json', 'manifest.json', 'receipt.json', 'restore-report.json'):
            info = (root / name).stat()
            self.assertIn((info.st_dev, info.st_ino), fsynced, name)

    def test_b4_report_publish_fault_retains_copy_and_marks_unknown(self):
        self.t.repo()
        group = self.t.prepared_group()
        for stage in ('restore-report-write', 'restore-report-publish', 'restore-report-sync'):
            with self.subTest(stage=stage):
                sealed = self.t.take(group, snapshot_id=stage)
                calls = []
                def fail(name):
                    if name == stage:
                        raise OSError('injected')
                with patch.object(restore, 'checkpoint', fail), self.assertRaises(OSError):
                    self.verify(sealed, trash=calls.append)
                self.assertEqual(calls, [])
                self.assertTrue((Path(sealed['backup_dir']) / 'failure.json').is_file())
                self.assertEqual(backup.list_backups(self.t.backups, group=self.t.last_group,
                                                   host_instance='fixture')['verified'], [])

    def test_db_source_mutation_and_live_space_refuse(self):
        self.t.repo()
        group = self.t.prepared_group()
        original = backup.sqlite_backup
        def mutate(source, target):
            original(source, target)
            with fixture_db(source) as db:
                db.execute("UPDATE repositories SET git_storage_id='changed'")
        with patch.object(backup, 'sqlite_backup', mutate), self.assertRaisesRegex(c.ContractError, 'source_changed'):
            self.t.take(group)
        with patch.object(backup, 'available_bytes', return_value=0):
            with self.assertRaisesRegex(c.ContractError, 'insufficient_space'):
                self.t.take(group, snapshot_id='no-space')
        self.assertFalse((self.t.backups / '.partial-no-space').exists())

    def test_b5_committed_wal_round_trip_and_real_extra_payload_refused(self):
        self.t.repo(); group = self.t.prepared_group()
        with fixture_db(self.t.dbpath) as db:
            db.execute('PRAGMA journal_mode=WAL'); db.execute('PRAGMA wal_autocheckpoint=0')
            db.execute("UPDATE repositories SET git_storage_id='wal-committed'"); db.commit()
            self.assertGreater(Path(str(self.t.dbpath) + '-wal').stat().st_size, 0)
            sealed = self.t.take(group)
            with fixture_db(Path(sealed['backup_dir']) / 'sqlite') as copied:
                self.assertEqual(copied.execute("SELECT git_storage_id FROM repositories").fetchone()[0],
                                 'wal-committed')
            self.assertTrue(all(self.verify(sealed)['checks'].values()))
        (Path(sealed['backup_dir']) / 'unexpected-wal').write_bytes(b'not a sqlite sidecar')
        with self.assertRaisesRegex(c.ContractError, 'manifest_mismatch'):
            self.verify(sealed)

    def test_b6_role_ids_and_missing_original_sources_restore(self):
        self.t.repo(); group = self.t.prepared_group()
        for item in group['objects']:
            item['id'] = {'sqlite': 'database', 'git_root': 'repositories'}.get(item['id'], item['id'])
        sealed = self.t.take(group)
        for item in group['objects']:
            path = Path(item['source']); path.rename(path.with_name(path.name + '-gone'))
        self.assertTrue(all(self.verify(sealed)['checks'].values()))

    def test_b6_independence_uses_device_and_inode_pair(self):
        sealed = self.seal()
        calls = []
        self.verify(sealed, trash=calls.append)
        root = Path(sealed['backup_dir'])
        def cross_device(path):
            return (1 if root in path.parents else 2, 10)
        with patch.object(restore, 'payload_identity', cross_device):
            restore.compare_manifest(calls[0], sealed['manifest'], root)
        with patch.object(restore, 'payload_identity', return_value=(1, 10)):
            with self.assertRaisesRegex(c.ContractError, 'hardlink_forbidden'):
                self.verify(sealed)

    def test_b7_mode_change_changes_source_fingerprint_and_round_trips(self):
        self.t.repo(); group = self.t.prepared_group(); binary = self.t.root / 'old_binary'
        binary.chmod(0o700)
        before = backup.source_fingerprint(group, 'sqlite:' + str(self.t.dbpath), self.t.admission)
        binary.chmod(0o600)
        after = backup.source_fingerprint(group, 'sqlite:' + str(self.t.dbpath), self.t.admission)
        self.assertNotEqual(before, after)
        binary.chmod(0o700)
        sealed = self.t.take(group)
        calls = []
        self.verify(sealed, trash=calls.append)
        restored = calls[0] / 'old_binary'
        self.assertEqual(restored.stat().st_mode & 0o777, 0o700)
        entry = next(v for v in sealed['manifest']['entries'] if v['path'] == 'old_binary')
        self.assertEqual(backup.supported_metadata(restored), {k: entry[k] for k in
                         ('mode', 'uid', 'gid', 'mtime_ns', 'acl_sha256', 'xattrs_sha256')})

    def test_b7_required_xattr_and_acl_refused(self):
        self.t.repo(); group = self.t.prepared_group(); path = self.t.root / 'old_binary'
        if sys.platform == 'darwin':
            subprocess.run(['/usr/bin/xattr', '-w', 'com.example.ags-review', 'synthetic', str(path)], check=True)
        else:
            os.setxattr(path, 'user.ags-review', b'synthetic')
        with self.assertRaisesRegex(c.ContractError, 'xattr_unsupported'):
            self.t.take(group)
        if sys.platform == 'darwin':
            subprocess.run(['/usr/bin/xattr', '-d', 'com.example.ags-review', str(path)], check=True)
            subprocess.run(['/bin/chmod', '+a', 'everyone deny delete', str(path)], check=True)
            with self.assertRaisesRegex(c.ContractError, 'acl_unsupported'):
                self.t.take(group)
        else:
            os.removexattr(path, 'user.ags-review')
            os.setxattr(path, 'user.required-acl', b'unsupported')
            with self.assertRaisesRegex(c.ContractError, 'xattr_unsupported'):
                self.t.take(group)

    def test_restore_sqlite_connections_close_on_success_and_failure(self):
        sealed = self.seal(); handles = []; connect = sqlite3.connect
        class Tracked(sqlite3.Connection):
            was_closed = False
            def close(self):
                self.was_closed = True
                return super().close()
        def record(*args, **kwargs):
            kwargs['factory'] = Tracked
            connection = connect(*args, **kwargs); handles.append(connection)
            return connection
        with patch.object(sqlite3, 'connect', record):
            self.verify(sealed)
        self.assertTrue(handles)
        self.assertTrue(all(handle.was_closed for handle in handles))
        handles.clear()
        with patch.object(sqlite3, 'connect', record), patch.object(restore, 'check_repo',
                side_effect=c.ContractError('injected')):
            with self.assertRaises(c.ContractError):
                self.verify(sealed)
        self.assertTrue(all(handle.was_closed for handle in handles))

    def test_snapshot_identifier_boundaries_restore_and_retry(self):
        self.t.repo()
        for length in (75, 76, 96):
            with self.subTest(length=length):
                snapshot = ('S0_.-' * 20)[:length]
                c.identifier(snapshot)
                sealed = self.t.take(snapshot_id=snapshot)
                root = Path(sealed['backup_dir'])
                receipt = c.strict_json((root / 'receipt.json').read_bytes())
                c.validate_seal((root / 'manifest.json').read_bytes(), c.canonical(receipt), self.t.last_group)
                attempts = []
                for _ in range(2):
                    calls = []

                    def fake_trash(path):
                        calls.append(Path(path))
                        Path(path).rename(self.t.root / ('trashed-' + Path(path).name))

                    report = self.verify(sealed, trash=fake_trash)
                    published = c.validate_restore_report((root / 'restore-report.json').read_bytes(), receipt)
                    self.assertEqual(published, report)
                    self.assertEqual(report['snapshot_id'], snapshot)
                    self.assertEqual(report['manifest_sha256'], receipt['manifest_sha256'])
                    self.assertTrue(all(report['checks'].values()))
                    self.assertLessEqual(len(report['restore_copy_id']), 96)
                    self.assertRegex(report['restore_copy_id'], r'^restore-[A-Za-z0-9_.-]+-[0-9a-f]{12}$')
                    self.assertEqual(len(calls), 1)
                    self.assertEqual(calls[0].name, report['restore_copy_id'])
                    self.assertFalse(calls[0].exists())
                    self.assertFalse(any(self.t.restores.glob('*/failure.json')))
                    attempts.append(report['restore_copy_id'])
                self.assertNotEqual(attempts[0], attempts[1])

    def test_cross_filesystem_restore_root_space_is_checked(self):
        self.t.repo(); group = self.t.prepared_group(); original = os.stat
        def devices(path, *args, **kwargs):
            info = original(path, *args, **kwargs)
            if Path(path) == self.t.restores:
                values = list(info); values[2] += 1
                return os.stat_result(values)
            return info
        def free(path):
            return 0 if Path(path) == self.t.restores else 10**12
        with patch.object(os, 'stat', devices), patch.object(backup, 'available_bytes', free):
            with self.assertRaisesRegex(c.ContractError, 'insufficient_space'):
                self.t.take(group)
        self.assertEqual(list(self.t.backups.iterdir()), [])


if __name__ == '__main__':
    unittest.main()
