#!/usr/bin/env python3
"""Independent restore-copy checks against synthetic SQLite and bare Git."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import sqlite3
import unittest

import upgrade_backup as backup
import upgrade_backup_contract as c
import upgrade_inventory as inv
import upgrade_restore_verify as restore
from test_upgrade_backup import BackupTests


class RestoreTests(BackupTests):
    def seal_fixture(self):
        self.repo()
        return self.take()

    def test_restore_copy_matches_manifest_and_is_removed(self):
        sealed = self.seal_fixture()
        calls = []

        def fake_trash(path):
            calls.append(Path(path))
            shutil.rmtree(path)

        report = restore.verify_sealed_backup(sealed['backup_dir'], trash=fake_trash)
        self.assertTrue(all(report['checks'].values()))
        self.assertEqual(report['manifest_sha256'], sealed['receipt']['manifest_sha256'])
        self.assertEqual(len(calls), 1)
        self.assertFalse(Path(report['restore_copy']).exists() if 'restore_copy' in report else False)
        self.assertEqual(calls[0].name.startswith('restore-'), True)
        listed = backup.list_backups(self.backups)
        self.assertEqual([item['snapshot_id'] for item in listed['verified']], ['snapshot1'])

    def corrupt(self, path, mutator):
        """Change payload bytes and the sealed size/hash so manifest still describes them."""
        mutator(path)
        raw = (Path(path).parents[3] / 'manifest.json').read_bytes() if False else None
        backup_dir = None
        for parent in Path(path).parents:
            if (parent / 'manifest.json').is_file() and (parent / 'receipt.json').is_file():
                backup_dir = parent
                break
        manifest = json.loads((backup_dir / 'manifest.json').read_bytes())
        relative = Path(path).relative_to(backup_dir).as_posix()
        exists = Path(path).exists()
        info = Path(path).stat() if exists else None
        manifest['entries'] = [
            item for item in manifest['entries']
            if item['path'] != relative or exists
        ]
        for item in manifest['entries']:
            if item['path'] == relative and info is not None and item['kind'] == 'file':
                item['size'] = info.st_size
                item['sha256'] = hashlib.sha256(Path(path).read_bytes()).hexdigest()
        raw = c.canonical(manifest)
        receipt = json.loads((backup_dir / 'receipt.json').read_bytes())
        receipt['manifest_sha256'] = hashlib.sha256(raw).hexdigest()
        (backup_dir / 'manifest.json').write_bytes(raw)
        (backup_dir / 'receipt.json').write_bytes(c.canonical(receipt))
        return backup_dir

    def test_bad_foreign_key_fails_restore(self):
        sealed = self.seal_fixture()
        payload = Path(sealed['backup_dir']) / 'sqlite'

        def break_fk(path):
            with sqlite3.connect(path) as db:
                db.execute('PRAGMA foreign_keys=OFF')
                db.execute('CREATE TABLE parent(id INTEGER PRIMARY KEY)')
                db.execute('CREATE TABLE child(id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id))')
                db.execute('INSERT INTO child(id, parent_id) VALUES (1, 99)')

        self.corrupt(payload, break_fk)
        with self.assertRaisesRegex(c.ContractError, 'sqlite_fk_failed'):
            restore.verify_sealed_backup(sealed['backup_dir'], trash=lambda path: None,
                                         schema=self.admission)
        self.assertTrue(Path(sealed['backup_dir']).exists())

    def test_missing_pack_fails_fsck(self):
        repo = self.repo()
        commit = self.commit(repo)
        (repo / 'refs' / 'heads').mkdir(parents=True, exist_ok=True)
        (repo / 'refs' / 'heads' / 'main').write_text(commit + '\n')
        sealed = self.take()
        copied = Path(sealed['backup_dir']) / 'git_root' / 'sample' / 'project.git'
        loose = next(path for path in (copied / 'objects').rglob('*') if path.is_file())
        loose.unlink()
        with self.assertRaisesRegex(c.ContractError, 'manifest_mismatch|git_validation_failed'):
            restore.verify_sealed_backup(sealed['backup_dir'], trash=lambda path: None,
                                         schema=self.admission)

    def test_missing_db_only_oid_fails(self):
        repo = self.repo()
        commit = self.commit(repo)
        self.insert('deployments', id=1, repository_id=1, ref=commit)
        sealed = self.take()
        copied = Path(sealed['backup_dir']) / 'git_root' / 'sample' / 'project.git'
        loose = next(path for path in (copied / 'objects').rglob('*') if path.is_file())
        loose.unlink()
        with self.assertRaises(c.ContractError):
            restore.verify_sealed_backup(sealed['backup_dir'], trash=lambda path: None)

    def test_missing_cas_fails(self):
        self.repo()
        body = b'cas-body' * 400
        sha = self.cas(body)
        self.insert('wiki_blob_refs', blob_sha=sha, size=len(body))
        sealed = self.take()
        cas = Path(sealed['backup_dir']) / 'git_root' / '.wikiblobs' / sha[:2] / sha[2:4] / sha
        cas.unlink()
        with self.assertRaisesRegex(c.ContractError, 'wiki_cas_missing|manifest_mismatch'):
            restore.verify_sealed_backup(sealed['backup_dir'], trash=lambda path: None)

    def test_changed_config_or_hash_fails(self):
        sealed = self.seal_fixture()
        config = Path(sealed['backup_dir']) / 'runtime_env'
        config.write_bytes(config.read_bytes() + b'\nCHANGED=1\n')
        with self.assertRaisesRegex(c.ContractError, 'manifest_mismatch'):
            restore.verify_sealed_backup(sealed['backup_dir'], trash=lambda path: None)

    def test_alternates_shallow_and_unknown_sha_class_refuse(self):
        repo = self.repo()
        other = self.root / 'other.git'
        self.init_repo(other)
        (repo / 'objects' / 'info').mkdir(parents=True, exist_ok=True)
        (repo / 'objects' / 'info' / 'alternates').write_text(str(other / 'objects') + '\n')
        with self.assertRaisesRegex(c.ContractError, 'git_external_objects_unsupported'):
            self.take()
        (repo / 'objects' / 'info' / 'alternates').unlink()
        (repo / 'shallow').write_text('a' * 40 + '\n')
        with self.assertRaisesRegex(c.ContractError, 'git_external_objects_unsupported'):
            self.take()
        (repo / 'shallow').unlink()
        self.insert('pull_requests', id=1, repository_id=1, head_sha='not-a-classified-sha')
        with self.assertRaisesRegex(c.ContractError, 'oid_invalid'):
            self.take()

    def test_tombstone_body_without_earlier_source_fails_backup_and_restore(self):
        self.repo()
        body = b'forged retained body'
        self.insert('wiki_changesets', changeset_id=1, repository_id=1, synth_commit_sha='1' * 40,
                    synth_format_ver=0, source='rest')
        self.insert('wiki_page_revisions', page_id=1, revision_id=1, changeset_id=1, blob_sha='',
                    body_size=0, body_inline=None, commit_sha='1' * 40, op='delete')
        self.insert('wiki_pages', page_id=1, repository_id=1, head_blob_sha=inv.git_blob(body),
                    body_size=len(body), body_inline=body, head_revision_id=1, head_changeset_id=1,
                    deleted_at='2026-01-01T00:00:00Z')
        with self.assertRaisesRegex(c.ContractError, 'wiki_tombstone_body_unproven'):
            self.take()

    def test_soft_delete_restore_delete_round_trip(self):
        self.repo()
        first = b'first body'
        restored = b'restored body'
        self.insert('wiki_changesets', changeset_id=1, repository_id=1, synth_commit_sha='1' * 40,
                    synth_format_ver=0, source='rest')
        self.insert('wiki_changesets', changeset_id=2, repository_id=1, synth_commit_sha='2' * 40,
                    synth_format_ver=0, source='rest')
        self.insert('wiki_changesets', changeset_id=3, repository_id=1, synth_commit_sha='3' * 40,
                    synth_format_ver=0, source='rest')
        self.insert('wiki_changesets', changeset_id=4, repository_id=1, synth_commit_sha='4' * 40,
                    synth_format_ver=0, source='rest')
        self.insert('wiki_page_revisions', page_id=1, revision_id=1, changeset_id=1,
                    blob_sha=inv.git_blob(first), body_size=len(first), body_inline=first,
                    commit_sha='1' * 40, op='create')
        self.insert('wiki_page_revisions', page_id=1, revision_id=2, changeset_id=2,
                    blob_sha='', body_size=0, body_inline=None, commit_sha='2' * 40, op='delete')
        self.insert('wiki_page_revisions', page_id=1, revision_id=3, changeset_id=3,
                    blob_sha=inv.git_blob(restored), body_size=len(restored), body_inline=restored,
                    commit_sha='3' * 40, op='restore')
        self.insert('wiki_page_revisions', page_id=1, revision_id=4, changeset_id=4,
                    blob_sha='', body_size=0, body_inline=None, commit_sha='4' * 40, op='delete')
        self.insert('wiki_pages', page_id=1, repository_id=1, head_blob_sha=inv.git_blob(restored),
                    body_size=len(restored), body_inline=restored, head_revision_id=4,
                    head_changeset_id=4, deleted_at='2026-01-02T00:00:00Z')
        sealed = self.take()
        report = restore.verify_sealed_backup(sealed['backup_dir'], trash=lambda path: shutil.rmtree(path))
        self.assertTrue(report['checks']['wiki_cas'])
        self.assertEqual(report['manifest_sha256'], sealed['receipt']['manifest_sha256'])


if __name__ == '__main__':
    unittest.main()
