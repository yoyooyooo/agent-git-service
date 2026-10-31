#!/usr/bin/env python3
"""Synthetic SQLite and real bare Git; never open operator data or run AGS."""
import ast
from contextlib import contextmanager
import copy
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import sqlite3
import subprocess
import tempfile
import unittest
from unittest.mock import Mock, patch

import upgrade_backup_contract as c
import upgrade_inventory as inv
import upgrade
import upgrade_engine as engine
import upgrade_host as host
import release_install_v2 as install_v2
from test_upgrade import authorization

ROOT = Path(__file__).resolve().parents[1]


@contextmanager
def fixture_db(path):
    db = sqlite3.connect(path)
    try:
        with db:
            yield db
    finally:
        db.close()


def fixture_schema():
    """Minimal admitted fixture, not a claim to reproduce GORM migrations."""
    columns = {}

    def add(table, *names):
        columns.setdefault(table, set()).update(names)

    for table in inv.RECOVERY_TABLES | inv.WIKI_TABLES | set(inv.EXTERNAL_COLUMNS) | {'deployments'}:
        add(table, 'id')
    add('repositories', 'full_name', 'git_storage_id')
    for table, where, _, names in inv.ROOT_QUERIES:
        add(table, 'id', *names)
        add(table, 'pull_request_id' if 'pull_request_id IN' in where else 'repository_id')
        if 'head_repository_id' in where:
            add(table, 'head_repository_id')
    for table, column in inv.CONSTRAINT_COLUMNS:
        add(table, column)
    for table, names in inv.EXTERNAL_COLUMNS.items():
        add(table, 'id', *names)
    add('deployments', 'repository_id', 'ref')
    add('wiki_changesets', 'changeset_id', 'repository_id', 'synth_commit_sha', 'synth_format_ver', 'source')
    add('wiki_pages', 'page_id', 'repository_id', 'head_blob_sha', 'body_size', 'body_inline', 'head_revision_id', 'head_changeset_id', 'deleted_at')
    add('wiki_page_revisions', 'page_id', 'revision_id', 'changeset_id', 'blob_sha', 'body_size', 'body_inline', 'commit_sha', 'op')
    for table in ('wiki_blob_refs', 'wiki_pending_blobs'):
        add(table, 'blob_sha', 'size')
    add('wiki_page_index', 'repository_id', 'head_commit_sha', 'head_blob_sha')
    add('wiki_index_state', 'repository_id', 'indexed_commit_sha', 'backlinks_indexed_sha')
    add('wiki_page_history', 'repository_id', 'commit_sha', 'parent_commit_sha')
    add('wiki_git_repair_obligations', 'repository_id', 'head_sha')
    for table in ('wiki_search_documents', 'wiki_search_projection_tasks'):
        add(table, 'repository_id', 'revision_sha')
    return columns


class InventoryTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='s1-', dir=Path.home())
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        self.store = self.root / 'git'; self.store.mkdir()
        self.dbpath = self.root / 'data.sqlite'
        with fixture_db(self.dbpath) as db:
            for table, columns in sorted(fixture_schema().items()):
                defs = []
                for name in sorted(columns):
                    kind = 'INTEGER' if name == 'id' or name.endswith('_id') or name in ('size', 'body_size', 'synth_format_ver') else 'BLOB' if name == 'body_inline' else 'TEXT'
                    defs.append('"' + name + '" ' + kind)
                db.execute('CREATE TABLE "' + table + '" (' + ','.join(defs) + ')')
        self.admission = inv.schema_digest(self.dbpath)

    def insert(self, table, **values):
        with fixture_db(self.dbpath) as db:
            db.execute('INSERT INTO ' + table + ' (' + ','.join(values) + ') VALUES ('
                       + ','.join('?' for _ in values) + ')', tuple(values.values()))

    def inventory(self, limit=c.MAX_ENTRIES):
        return inv.database_inventory(self.dbpath, self.store, self.admission, limit)

    def repo(self, name='sample/project', repo_id=1):
        self.insert('repositories', id=repo_id, full_name=name, git_storage_id='not-a-directory')
        path = self.store / (name + '.git')
        path.parent.mkdir(exist_ok=True)
        self.init_repo(path)
        return path

    def init_repo(self, path):
        env = {'PATH': '/usr/bin:/bin', 'HOME': str(self.root), 'GIT_CONFIG_NOSYSTEM': '1',
               'GIT_CONFIG_GLOBAL': '/dev/null'}
        subprocess.run(['git', 'init', '--bare', '--template=', str(path)], env=env, check=True,
                       capture_output=True)

    def commit(self, repo):
        tree = inv.git(repo, 'mktree', input=b'').strip().decode()
        raw = ('tree ' + tree + '\nauthor Fixture <fixture@example.test> 0 +0000\n'
               'committer Fixture <fixture@example.test> 0 +0000\n\nunreferenced\n').encode()
        return inv.git(repo, 'hash-object', '-w', '-t', 'commit', '--stdin', input=raw).strip().decode()

    def cas(self, body):
        sha = inv.git_blob(body)
        path = self.store / '.wikiblobs' / sha[:2] / sha[2:4] / sha
        path.parent.mkdir(parents=True, exist_ok=True); path.write_bytes(body)
        return sha

    def group(self):
        objects = []
        for role in sorted(c.REQUIRED_ROLES):
            path = self.dbpath if role == 'sqlite' else self.store if role == 'git_root' else self.root / role
            if role not in ('sqlite', 'git_root'):
                path.write_bytes(b'inert fixture')
            objects.append({'id': role, 'role': role, 'source': str(path), 'restore_target': str(path),
                            'kind': 'directory' if role == 'git_root' else 'file', 'depends_on': []})
        env = self.root / 'runtime_env'
        env.write_text('DB_DSN=' + str('sqlite:' + str(self.dbpath)) + '\nGIT_REPO_DIR=' + str(self.store)
                       + '\nSECRET_ENCRYPTION_KEY=synthetic-fixture\n')
        return {'schema': 'ags.recovery-group.v1', 'objects': objects,
                'dependency_review_sha256': 'a' * 64,
                'storage': {'backup_root': '/srv/ags-backups', 'restore_root': '/srv/ags-restore',
                            'private_policy_sha256': 'f' * 64, 'copy_method': 'ordinary_copy'},
                'retention': dict(c.RETENTION),
                'capacity': {'backup_bytes': 1024 * 1024, 'restore_copy_bytes': 1024 * 1024,
                             'candidate_scratch_bytes': 1024, 'migration_growth_bytes': 1024,
                             'runtime_reserve_bytes': 1024, 'available_bytes': 4 * 1024 * 1024}}

    def manifest(self, group):
        entries = []
        for obj in group['objects']:
            kind = obj['kind']
            entries.append({'object_id': obj['id'], 'path': obj['id'], 'kind': kind,
                            'size': 1 if kind == 'file' else 0,
                            'sha256': 'b' * 64 if kind == 'file' else None,
                            'mode': 0o600 if kind == 'file' else 0o700, 'uid': 501, 'gid': 20,
                            'mtime_ns': 1, 'acl_sha256': 'c' * 64, 'xattrs_sha256': 'd' * 64,
                            'restore_target': obj['restore_target']})
        binding = {key: 'e' * 64 if key.endswith('_sha256') else 'fixture' for key in c.BINDING_FIELDS}
        binding.update({key: 'f' * 40 for key in ('old', 'new', 'runtime_tree', 'control_revision', 'control_tree')})
        binding.update(release_id=1, target='darwin_arm64')
        manifest = {'schema': 'ags.backup-manifest.v1', 'snapshot_id': 'snapshot1', 'binding': binding,
                    'quiescence': {'nonce': 'nonce1', 'stop_evidence_sha256': 'a' * 64,
                                   'writers_evidence_sha256': 'b' * 64}, 'inventory_sha256': 'c' * 64,
                    'started_at': '2026-01-01T00:00:00+00:00', 'ended_at': '2026-01-01T00:01:00+00:00',
                    'recovery_group_sha256': c.fingerprint(group), 'source_before_sha256': 'e' * 64,
                    'source_after_sha256': 'e' * 64,
                    'retention': dict(c.RETENTION), 'capacity': group['capacity'], 'entries': entries}
        return manifest

    def seal(self, manifest):
        raw = c.canonical(manifest)
        receipt = {'schema': 'ags.local-backup.v2', 'state': 'sealed', 'snapshot_id': manifest['snapshot_id'],
                   'manifest_sha256': hashlib.sha256(raw).hexdigest(),
                   'inventory_sha256': manifest['inventory_sha256'],
                   'binding_sha256': c.fingerprint(manifest['binding'])}
        return raw, c.canonical(receipt)

    def test_empty_database_and_empty_repository_optional_wiki(self):
        result = self.inventory()
        self.assertEqual(result['local_oids'], [])
        self.assertEqual(result['store']['repositories'], {})
        repo = self.repo()
        result = self.inventory()
        self.assertEqual(set(result['store']['repositories']), {'sample/project.git'})
        wiki = self.store / 'sample/project.wiki.git'; self.init_repo(wiki)
        result = self.inventory()
        self.assertEqual(len(result['store']['repositories']), 2)
        self.assertTrue(repo.is_dir())

    def test_db_only_oid_fork_head_constraints_deployment_and_external_sha(self):
        repo = self.repo(); commit = self.commit(repo)
        other = self.repo('sample/fork', 2); fork_commit = self.commit(other)
        # PR head lives in both the base and head repo under the Go root contract.
        self.insert('pull_requests', id=1, repository_id=1, head_repository_id=2, head_sha=commit)
        self.insert('access_grant_invocations', id=1, repository_id=1,
                    constraints_json=json.dumps({'exact_head': commit}), provider_merge_sha='1' * 40)
        self.insert('projection_events', id=1, repository_id=1, ags_sha=commit, external_sha='2' * 40)
        self.insert('deployments', id=1, repository_id=1, ref=commit)
        result = self.inventory()
        self.assertEqual({item[0] for item in result['local_oids']}, {1, 2})
        self.assertEqual(len(result['external_oids']), 2)
        self.assertEqual(inv.git(repo, 'for-each-ref'), b'')
        with fixture_db(self.dbpath) as db:
            db.execute("UPDATE pull_requests SET head_sha = ?", ('3' * 40,))
        with self.assertRaisesRegex(c.ContractError, 'git_validation_failed'):
            self.inventory()

    def test_all_go_roots_and_constraints_are_observed(self):
        repo = self.repo(); commit = self.commit(repo)
        self.insert('pull_requests', id=1, repository_id=1)
        for index, (table, where, _, columns) in enumerate(inv.ROOT_QUERIES, 2):
            if 'head_repository_id' in where:
                continue
            values = {'id': index, 'pull_request_id' if 'pull_request_id IN' in where else 'repository_id': 1}
            values.update({key: commit for key in columns})
            self.insert(table, **values)
        for index, (table, column) in enumerate(inv.CONSTRAINT_COLUMNS, 100):
            self.insert(table, id=index, repository_id=1,
                        **{column: json.dumps({key: commit for key in inv.CONSTRAINT_KEYS})})
        self.assertEqual(self.inventory()['local_oids'], [[1, 'sample/project.git', 'commit', commit]])
        # Every direct column must independently cause a failure for an absent object.
        for table, where, _, columns in inv.ROOT_QUERIES:
            if 'head_repository_id' in where:
                continue
            for column in columns:
                with self.subTest(table=table, column=column):
                    with fixture_db(self.dbpath) as db:
                        old = db.execute('SELECT id,' + column + ' FROM ' + table + ' WHERE ' + column + ' IS NOT NULL').fetchone()
                        db.execute('UPDATE ' + table + ' SET ' + column + '=? WHERE id=?', ('9' * 40, old[0]))
                    with self.assertRaises(c.ContractError):
                        self.inventory()
                    with fixture_db(self.dbpath) as db:
                        db.execute('UPDATE ' + table + ' SET ' + column + '=? WHERE id=?', (old[1], old[0]))

    def test_historical_cas_inline_and_synthetic_wiki_are_distinct(self):
        self.repo()
        old = b'old historical body' * 300; sha = self.cas(old)
        synth = '4' * 40
        self.insert('wiki_changesets', changeset_id=1, repository_id=1, synth_commit_sha=synth,
                    synth_format_ver=0, source='rest')
        self.insert('wiki_page_revisions', page_id=1, revision_id=1, changeset_id=1, blob_sha=sha, body_size=len(old),
                    body_inline=None, commit_sha=synth, op='update')
        self.insert('wiki_blob_refs', blob_sha=sha, size=len(old))
        inline = b'new\x00\xffbody'
        self.insert('wiki_page_revisions', page_id=1, revision_id=2, changeset_id=1,
                    blob_sha=inv.git_blob(inline), body_size=len(inline), body_inline=inline,
                    commit_sha=synth, op='update')
        self.insert('wiki_pages', page_id=1, repository_id=1, head_blob_sha=inv.git_blob(inline),
                    body_size=len(inline), body_inline=inline, head_revision_id=2, head_changeset_id=1)
        result = self.inventory()
        self.assertEqual(result['synthetic_wiki_oids'], [['wiki_changesets', synth]])
        self.assertEqual(result['local_oids'], [])
        path = self.store / '.wikiblobs' / sha[:2] / sha[2:4] / sha
        path.rename(path.with_name('0' * 40))
        with self.assertRaises(c.ContractError):
            self.inventory()

    def wiki_changeset(self, changeset_id, sha):
        self.insert('wiki_changesets', changeset_id=changeset_id, repository_id=1,
                    synth_commit_sha=sha, synth_format_ver=0, source='rest')

    def content_revision(self, revision_id, changeset_id, body, op, commit):
        self.insert('wiki_page_revisions', page_id=1, revision_id=revision_id, changeset_id=changeset_id,
                    blob_sha=inv.git_blob(body), body_size=len(body), body_inline=body,
                    commit_sha=commit, op=op)

    def seed_soft_deleted_page(self):
        self.repo()
        body = b'history before deletion'
        sha = inv.git_blob(body)
        inline = body
        self.wiki_changeset(1, '1' * 40)
        self.wiki_changeset(2, '2' * 40)
        self.insert('wiki_page_revisions', page_id=1, revision_id=1, changeset_id=1,
                    blob_sha=sha, body_size=len(body), body_inline=inline, commit_sha='1' * 40, op='create')
        self.insert('wiki_page_revisions', page_id=1, revision_id=2, changeset_id=2,
                    blob_sha='', body_size=0, body_inline=None, commit_sha='2' * 40, op='delete')
        self.insert('wiki_pages', page_id=1, repository_id=1, head_blob_sha=sha,
                    body_size=len(body), body_inline=inline, head_revision_id=1,
                    head_changeset_id=1, deleted_at=None)
        self.assertEqual(self.inventory()['tables']['wiki_pages']['rows'], 1)
        # Exact page columns changed by applyDelete; retained body stays untouched.
        with fixture_db(self.dbpath) as db:
            db.execute("UPDATE wiki_pages SET head_revision_id=2, head_changeset_id=2,"
                       " deleted_at='2026-01-01T00:00:00Z'")
        self.assertEqual(self.inventory()['tables']['wiki_pages']['rows'], 1)
        return body

    def test_future_revision_cannot_prove_predelete_body(self):
        self.seed_soft_deleted_page()
        forged = b'body only written AFTER delete'
        self.wiki_changeset(3, '3' * 40)
        self.content_revision(3, 3, forged, 'restore', '3' * 40)
        with fixture_db(self.dbpath) as db:
            db.execute('UPDATE wiki_pages SET head_blob_sha=?, body_size=?, body_inline=?',
                       (inv.git_blob(forged), len(forged), forged))
        # Delete head remains revision 2. Only revision 3 carries the new body.
        with self.assertRaisesRegex(c.ContractError, 'wiki_tombstone_body_unproven'):
            self.inventory()

    def test_foreign_changeset_cannot_prove_retained_body(self):
        self.seed_soft_deleted_page()
        self.repo('sample/other', 2)
        # The delete head stays in repository 1; only its body source moves.
        with fixture_db(self.dbpath) as db:
            db.execute('UPDATE wiki_changesets SET repository_id=2 WHERE changeset_id=1')
        with self.assertRaisesRegex(c.ContractError, 'wiki_tombstone_body_unproven'):
            self.inventory()

    def test_foreign_delete_head_is_rejected(self):
        self.seed_soft_deleted_page()
        self.repo('sample/other', 2)
        with fixture_db(self.dbpath) as db:
            db.execute('UPDATE wiki_changesets SET repository_id=2 WHERE changeset_id=2')
        with self.assertRaisesRegex(c.ContractError, 'wiki_head_revision_mismatch'):
            self.inventory()

    def test_nonempty_delete_revision_is_rejected(self):
        body = self.seed_soft_deleted_page()
        with fixture_db(self.dbpath) as db:
            db.execute('UPDATE wiki_page_revisions SET blob_sha=?, body_size=?, body_inline=?'
                       ' WHERE revision_id=2', (inv.git_blob(body), len(body), body))
        with self.assertRaisesRegex(c.ContractError, 'wiki_delete_revision_invalid'):
            self.inventory()

    def test_soft_deleted_wiki_page_keeps_historical_body_and_rejects_bad_heads(self):
        # applyDelete writes op=delete with an empty blob and leaves the previous
        # body on the page. The review probe at the old head failed this state.
        self.repo()
        historical = b'historical body'
        self.wiki_changeset(1, '1' * 40)
        self.wiki_changeset(2, '2' * 40)
        self.content_revision(1, 1, historical, 'create', '1' * 40)
        self.insert('wiki_page_revisions', page_id=1, revision_id=2, changeset_id=2,
                    blob_sha='', body_size=0, body_inline=None, commit_sha='2' * 40, op='delete')
        self.insert('wiki_pages', page_id=1, repository_id=1, head_blob_sha=inv.git_blob(historical),
                    body_size=len(historical), body_inline=historical, head_revision_id=2,
                    head_changeset_id=2, deleted_at='2026-01-01T00:00:00Z')
        result = self.inventory()
        self.assertEqual(result['tables']['wiki_pages']['rows'], 1)
        self.assertEqual(result['tables']['wiki_page_revisions']['rows'], 2)
        self.assertEqual(result['local_oids'], [])

        # Same bytes, live head still the create revision: also inventoryable.
        with fixture_db(self.dbpath) as db:
            db.execute('UPDATE wiki_pages SET head_revision_id=1, head_changeset_id=1, deleted_at=NULL')
        live = self.inventory()
        self.assertEqual(live['tables']['wiki_pages']['rows'], 1)
        with fixture_db(self.dbpath) as db:
            db.execute("UPDATE wiki_pages SET head_revision_id=2, head_changeset_id=2, deleted_at='2026-01-01T00:00:00Z'")

        # deleted_at empty while head is the delete revision.
        with fixture_db(self.dbpath) as db:
            db.execute('UPDATE wiki_pages SET deleted_at=NULL')
        with self.assertRaisesRegex(c.ContractError, 'wiki_head_revision_mismatch'):
            self.inventory()
        with fixture_db(self.dbpath) as db:
            db.execute("UPDATE wiki_pages SET deleted_at='2026-01-01T00:00:00Z'")

        # deleted_at set while head is still the content revision.
        with fixture_db(self.dbpath) as db:
            db.execute("UPDATE wiki_pages SET head_revision_id=1, head_changeset_id=1")
        with self.assertRaisesRegex(c.ContractError, 'wiki_head_revision_mismatch'):
            self.inventory()
        with fixture_db(self.dbpath) as db:
            db.execute('UPDATE wiki_pages SET head_revision_id=2, head_changeset_id=2')

        # Tombstone body that no content revision of this page ever stored.
        forged = b'forged body'
        with fixture_db(self.dbpath) as db:
            db.execute('UPDATE wiki_pages SET head_blob_sha=?, body_size=?, body_inline=?',
                       (inv.git_blob(forged), len(forged), forged))
        with self.assertRaisesRegex(c.ContractError, 'wiki_tombstone_body_unproven'):
            self.inventory()

        # Restore/recreate: clear deleted_at and point head at a new content revision.
        restored = b'restored body'
        self.wiki_changeset(3, '3' * 40)
        self.content_revision(3, 3, restored, 'restore', '3' * 40)
        with fixture_db(self.dbpath) as db:
            db.execute('UPDATE wiki_pages SET head_blob_sha=?, body_size=?, body_inline=?,'
                       ' head_revision_id=3, head_changeset_id=3, deleted_at=NULL',
                       (inv.git_blob(restored), len(restored), restored))
        restored_result = self.inventory()
        self.assertEqual(restored_result['tables']['wiki_page_revisions']['rows'], 3)

        # Delete again. The retained body is the restore revision, not the original create.
        self.wiki_changeset(4, '4' * 40)
        self.insert('wiki_page_revisions', page_id=1, revision_id=4, changeset_id=4,
                    blob_sha='', body_size=0, body_inline=None, commit_sha='4' * 40, op='delete')
        with fixture_db(self.dbpath) as db:
            db.execute("UPDATE wiki_pages SET head_revision_id=4, head_changeset_id=4, deleted_at='2026-01-02T00:00:00Z'")
        again = self.inventory()
        self.assertEqual(again['tables']['wiki_page_revisions']['rows'], 4)

        # A body copied from another page does not prove this tombstone.
        other = b'other page body'
        self.wiki_changeset(5, '5' * 40)
        self.insert('wiki_page_revisions', page_id=9, revision_id=1, changeset_id=5,
                    blob_sha=inv.git_blob(other), body_size=len(other), body_inline=other,
                    commit_sha='5' * 40, op='create')
        with fixture_db(self.dbpath) as db:
            db.execute('UPDATE wiki_pages SET head_blob_sha=?, body_size=?, body_inline=?',
                       (inv.git_blob(other), len(other), other))
        with self.assertRaisesRegex(c.ContractError, 'wiki_tombstone_body_unproven'):
            self.inventory()

    def test_historical_oversized_body_survives_soft_delete_only_with_cas(self):
        self.repo()
        old = b'old historical body' * 300
        sha = self.cas(old)
        self.wiki_changeset(1, '6' * 40)
        self.wiki_changeset(2, '7' * 40)
        self.insert('wiki_page_revisions', page_id=1, revision_id=1, changeset_id=1, blob_sha=sha,
                    body_size=len(old), body_inline=None, commit_sha='6' * 40, op='create')
        self.insert('wiki_page_revisions', page_id=1, revision_id=2, changeset_id=2, blob_sha='',
                    body_size=0, body_inline=None, commit_sha='7' * 40, op='delete')
        self.insert('wiki_pages', page_id=1, repository_id=1, head_blob_sha=sha, body_size=len(old),
                    body_inline=None, head_revision_id=2, head_changeset_id=2,
                    deleted_at='2026-01-01T00:00:00Z')
        self.assertEqual(self.inventory()['tables']['wiki_pages']['rows'], 1)
        path = self.store / '.wikiblobs' / sha[:2] / sha[2:4] / sha
        held = self.root / 'held-cas'
        path.rename(held)
        with self.assertRaisesRegex(c.ContractError, 'wiki_cas_missing'):
            self.inventory()
        held.rename(path)

    def test_deleted_at_is_a_required_wiki_column(self):
        with fixture_db(self.dbpath) as db:
            db.execute('ALTER TABLE wiki_pages DROP COLUMN deleted_at')
        self.admission = inv.schema_digest(self.dbpath)
        with self.assertRaisesRegex(c.ContractError, 'required_column_missing'):
            self.inventory()

    def test_materialized_wiki_requires_real_objects_and_type(self):
        self.repo(); wiki = self.store / 'sample/project.wiki.git'; self.init_repo(wiki)
        commit = self.commit(wiki)
        self.insert('wiki_changesets', changeset_id=1, repository_id=1, synth_commit_sha=commit,
                    synth_format_ver=1, source='git')
        self.assertEqual(self.inventory()['local_oids'], [[1, 'sample/project.wiki.git', 'commit', commit]])
        with fixture_db(self.dbpath) as db:
            db.execute('UPDATE wiki_changesets SET synth_format_ver = 2')
        with self.assertRaisesRegex(c.ContractError, 'wiki_classification_unknown'):
            self.inventory()

    def test_unknown_table_schema_missing_dependency_and_budget_refuse(self):
        with fixture_db(self.dbpath) as db:
            db.execute('CREATE TABLE future_data (id INTEGER)')
        with self.assertRaisesRegex(c.ContractError, 'schema_not_admitted'):
            self.inventory()
        self.admission = inv.schema_digest(self.dbpath)
        with self.assertRaisesRegex(c.ContractError, 'unknown_table'):
            self.inventory()

    def test_missing_required_table_and_changed_schema_refuse(self):
        with fixture_db(self.dbpath) as db:
            db.execute('ALTER TABLE deployments RENAME TO lost_deployments')
        with self.assertRaises(c.ContractError):
            self.inventory()

    def test_unknown_store_paths_missing_repo_and_budget_refuse(self):
        repo = self.repo()
        with self.assertRaisesRegex(c.ContractError, 'inventory_budget'):
            self.inventory(limit=1)
        (self.store / 'unknown').mkdir()
        with self.assertRaisesRegex(c.ContractError, 'unknown_store_directory'):
            self.inventory()
        (self.store / 'unknown').rename(self.root / 'moved-unknown')
        repo.rename(self.root / 'moved-repo')
        with self.assertRaisesRegex(c.ContractError, 'repository_missing'):
            self.inventory()

    def test_git_alternates_promisor_shallow_and_wrong_type_refuse(self):
        repo = self.repo()
        for name, body in (('objects/info/alternates', b'/does-not-exist'), ('shallow', b'1' * 40),
                           ('objects/pack/test.promisor', b'')):
            path = repo / name; path.parent.mkdir(parents=True, exist_ok=True); path.write_bytes(body)
            with self.subTest(name=name), self.assertRaises(c.ContractError):
                self.inventory()
            path.rename(self.root / ('moved-' + path.name))
        blob = inv.git(repo, 'hash-object', '-w', '--stdin', input=b'not a commit').strip().decode()
        self.insert('commit_statuses', repository_id=1, commit_sha=blob)
        with self.assertRaisesRegex(c.ContractError, 'git_object_type_invalid'):
            self.inventory()

    def test_links_fifo_retirement_pending_busy_and_spool_refuse(self):
        self.repo()
        link = self.store / 'linked'; link.symlink_to(self.dbpath)
        with self.assertRaisesRegex(c.ContractError, 'symlink_forbidden'):
            self.inventory()
        link.rename(self.root / 'moved-link')
        (self.root / 'link-fixture').write_bytes(b'inert')
        os.link(self.root / 'link-fixture', self.root / 'hardlink')
        with self.assertRaisesRegex(c.ContractError, 'hardlink_forbidden'):
            inv.safe_path(self.root / 'link-fixture')
        (self.root / 'hardlink').rename(self.root / 'still-hardlink')
        retire = self.store / '.ags-artifact-retirement'; retire.mkdir(mode=0o700)
        lock = retire / 'operation.lock'; lock.write_bytes(b''); lock.chmod(0o600)
        self.inventory()
        with lock.open('rb') as stream:
            fcntl.flock(stream, fcntl.LOCK_EX | fcntl.LOCK_NB)
            with self.assertRaisesRegex(c.ContractError, 'retirement_lock_busy'):
                self.inventory()
        (retire / 'pending').write_bytes(b'inert')
        with self.assertRaisesRegex(c.ContractError, 'retirement_pending_or_unknown'):
            self.inventory()

    def test_logical_digest_preserves_null_blob_text_and_non_utf8(self):
        with fixture_db(self.dbpath) as db:
            db.execute("INSERT INTO labels (id) VALUES (NULL)")
            db.execute("INSERT INTO labels (id) VALUES (CAST(x'ff00' AS TEXT))")
            db.execute("INSERT INTO labels (id) VALUES (x'ff00')")
        first = self.inventory()
        with fixture_db(self.dbpath) as db:
            db.execute("UPDATE labels SET id = x'ff00' WHERE typeof(id) = 'text'")
        self.assertNotEqual(first['logical_sha256'], self.inventory()['logical_sha256'])

    def test_row_budget_zero_oid_and_bad_constraints(self):
        self.repo()
        self.insert('commit_statuses', repository_id=1, commit_sha='0' * 40)
        self.assertEqual(self.inventory()['local_oids'], [])
        with self.assertRaisesRegex(c.ContractError, 'inventory_budget'):
            self.inventory(limit=1)
        self.insert('access_grant_invocations', repository_id=1,
                    constraints_json='{"sha":"' + 'a' * 40 + '","sha":"' + 'b' * 40 + '"}')
        with self.assertRaises(c.ContractError):
            self.inventory()

    def test_dsn_closed_backend_options_and_absolute_paths(self):
        for dsn in ('sqlite:' + str(self.dbpath), 'sqlite://' + str(self.dbpath),
                    'file:' + str(self.dbpath) + '?mode=rw'):
            self.assertEqual(inv.sqlite_path(dsn), self.dbpath)
        for value in (':memory:', 'sqlite::memory:', 'sqlite:relative', 'postgres://host/db',
                      'root:@tcp(localhost)/db', 'file://host/db', 'file:/a?mode=memory',
                      'file:/a?immutable=1', 'sqlite:/a?mode=ro&mode=rw', 'sqlite:/a/../b',
                      'sqlite:/a?vfs=other', 'sqlite:/a%00b'):
            with self.subTest(value=value), self.assertRaises((c.ContractError, ValueError)):
                inv.sqlite_path(value)

    def test_group_dependencies_secret_and_capacity(self):
        group = self.group()
        before = hashlib.sha256(self.dbpath.read_bytes()).hexdigest()
        result = inv.inspect_group(group, 'sqlite:' + str(self.dbpath), self.admission)
        self.assertEqual(result['group_sha256'], c.fingerprint(group))
        self.assertEqual(before, hashlib.sha256(self.dbpath.read_bytes()).hexdigest())
        for change in ('missing', 'cycle', 'space', 'role', 'type'):
            bad = copy.deepcopy(group)
            if change == 'missing': bad['objects'][0]['depends_on'] = ['absent']
            if change == 'cycle': bad['objects'][0]['depends_on'] = [bad['objects'][0]['id']]
            if change == 'space': bad['capacity']['available_bytes'] = 0
            if change == 'role': bad['objects'] = [v for v in bad['objects'] if v['role'] != 'manager']
            if change == 'type': bad['retention']['keep_verified_sealed'] = True
            with self.subTest(change=change), self.assertRaises(c.ContractError): c.validate_group(bad)
        (self.root / 'runtime_env').write_text('DB_DSN=sqlite:' + str(self.dbpath)
                                             + '\nGIT_REPO_DIR=' + str(self.store) + '\n')
        with self.assertRaisesRegex(c.ContractError, 'secret_recovery_material_missing'):
            inv.inspect_group(group, 'sqlite:' + str(self.dbpath), self.admission)

    def test_declared_config_dependency_missing_and_symlink_refuse(self):
        group = self.group()
        with (self.root / 'runtime_env').open('a') as stream:
            stream.write('AGS_REPLICATION_CONFIG_FILE=/missing/config.json\n')
        with self.assertRaisesRegex(c.ContractError, 'configuration_dependency_missing'):
            inv.inspect_group(group, 'sqlite:' + str(self.dbpath), self.admission)
        group = self.group()
        (self.root / 'manager').rename(self.root / 'saved-manager')
        (self.root / 'manager').symlink_to(self.root / 'saved-manager')
        with self.assertRaisesRegex(c.ContractError, 'symlink_forbidden'):
            inv.inspect_group(group, 'sqlite:' + str(self.dbpath), self.admission)

    def test_strict_seal_digest_fields_paths_types_and_retention(self):
        group = self.group(); manifest = self.manifest(group)
        c.validate_seal(*self.seal(manifest), group)
        for change in ('field', 'bool', 'path', 'digest', 'retention', 'parent', 'mapping', 'duplicate'):
            bad = copy.deepcopy(manifest)
            if change == 'field': bad['extra'] = 1
            if change == 'bool': bad['binding']['release_id'] = True
            if change == 'path': bad['entries'][0]['path'] = '../escape'
            if change == 'digest': bad['entries'][0]['sha256'] = 'bad'
            if change == 'retention': bad['retention']['keep_verified_sealed'] = 6
            if change == 'parent': bad['entries'][0]['path'] += '/missing/file'
            if change == 'mapping': bad['entries'][0]['restore_target'] = '/elsewhere/file'
            if change == 'duplicate': bad['entries'].append(dict(bad['entries'][0]))
            with self.subTest(change=change), self.assertRaises(c.ContractError):
                c.validate_seal(*self.seal(bad), group)
        raw, receipt = self.seal(manifest)
        with self.assertRaisesRegex(c.ContractError, 'seal_digest_mismatch'):
            c.validate_seal(raw.replace(b'nonce1', b'nonce2'), receipt, group)
        with self.assertRaisesRegex(c.ContractError, 'manifest_not_canonical'):
            c.validate_seal(json.dumps(manifest).encode(), receipt, group)
        for raw in (b'{"a":1,"a":2}', b'{"a":NaN}', b'{"a":{"x":1,"x":2}}', b'\xff'):
            with self.subTest(raw=raw), self.assertRaises(c.ContractError): c.strict_json(raw)

    def test_restore_report_separate_digest_and_retention_exact(self):
        group = self.group(); manifest = self.manifest(group)
        raw, receipt_raw = self.seal(manifest); receipt = c.strict_json(receipt_raw)
        report = {'schema': 'ags.restore-verification.v1', 'snapshot_id': 'snapshot1',
                  'manifest_sha256': receipt['manifest_sha256'], 'restore_copy_id': 'copy1',
                  'verified_at': '2026-01-01T00:02:00Z', 'checks': dict.fromkeys(
                      ('independent_copy', 'manifest', 'sqlite_integrity', 'sqlite_fk',
                       'sqlite_logical_digest', 'git_fsck', 'db_oids', 'wiki_cas', 'dependencies'), True)}
        report['report_sha256'] = c.fingerprint(report)
        c.validate_restore_report(c.canonical(report), receipt)
        report['checks']['independent_copy'] = False
        with self.assertRaisesRegex(c.ContractError, 'restore_check_failed'):
            c.validate_restore_report(c.canonical(report), receipt)
        for key in c.RETENTION:
            bad = dict(c.RETENTION); bad.pop(key)
            with self.subTest(key=key), self.assertRaises(c.ContractError): c.validate_retention(bad)

    def test_wal_visible_without_immutable_and_fk_failure(self):
        connection = sqlite3.connect(self.dbpath)
        try:
            connection.execute('PRAGMA journal_mode=WAL')
            connection.execute('PRAGMA wal_autocheckpoint=0')
            connection.execute('INSERT INTO labels (id) VALUES (5)'); connection.commit()
            self.assertEqual(self.inventory()['tables']['labels']['rows'], 1)
        finally:
            connection.close()
        with fixture_db(self.dbpath) as db:
            db.execute('CREATE UNIQUE INDEX fixture_repo_id ON repositories(id)')
            db.execute('ALTER TABLE labels ADD COLUMN repository_id INTEGER REFERENCES repositories(id)')
            db.execute('UPDATE labels SET repository_id=999')
        self.admission = inv.schema_digest(self.dbpath)
        with self.assertRaisesRegex(c.ContractError, 'sqlite_fk_failed'):
            self.inventory()

    def test_empty_directories_and_legacy_inline_reference_preserved(self):
        repo = self.repo(); (repo / 'empty-metadata').mkdir()
        self.insert('wiki_blob_refs', blob_sha=inv.git_blob(b'inline old'), size=10)
        result = self.inventory()
        self.assertEqual(result['tables']['wiki_blob_refs']['rows'], 1)
        before = result['store']['files_sha256']
        (repo / 'empty-metadata').rename(self.root / 'saved-empty')
        self.assertNotEqual(self.inventory()['store']['files_sha256'], before)

    def test_storage_destination_and_unhashable_enums_refuse(self):
        group = self.group()
        for value in ('/tmp/backups', str(self.store / 'backups'), '/srv/ags-restore'):
            bad = copy.deepcopy(group); bad['storage']['backup_root'] = value
            with self.subTest(value=value), self.assertRaises(c.ContractError): c.validate_group(bad)
        bad = copy.deepcopy(group); bad['objects'][0]['role'] = []
        with self.assertRaises(c.ContractError): c.validate_group(bad)
        bad = self.manifest(group); bad['source_after_sha256'] = 'a' * 64
        with self.assertRaisesRegex(c.ContractError, 'source_changed'):
            c.validate_seal(*self.seal(bad), group)

    def test_schema_pragma_and_missing_semantic_column_refuse(self):
        with fixture_db(self.dbpath) as db:
            db.execute('PRAGMA user_version=7')
        with self.assertRaisesRegex(c.ContractError, 'schema_not_admitted'):
            self.inventory()
        self.admission = inv.schema_digest(self.dbpath)
        with fixture_db(self.dbpath) as db:
            db.execute('ALTER TABLE deployments RENAME COLUMN ref TO unknown_ref')
        self.admission = inv.schema_digest(self.dbpath)
        with self.assertRaisesRegex(c.ContractError, 'required_column_missing'):
            self.inventory()

    def test_special_files_unknown_spool_and_missing_config_refuse(self):
        self.repo()
        special = self.store / 'pipe'; os.mkfifo(special)
        with self.assertRaisesRegex(c.ContractError, 'file_type_unsupported'):
            self.inventory()
        special.rename(self.root / 'moved-pipe')
        spool = self.store / '.github-app-deliveries'; spool.mkdir()
        (spool / 'unknown').write_bytes(b'inert')
        with self.assertRaisesRegex(c.ContractError, 'delivery_spool_invalid'):
            self.inventory()
        group = self.group(); (self.root / 'manager').rename(self.root / 'moved-manager')
        with self.assertRaises(OSError):
            inv.inspect_group(group, 'sqlite:' + str(self.dbpath), self.admission)

    def test_bundle_requires_both_s1_modules_by_exact_digest(self):
        bundle = self.root / 'bundle'; bundle.mkdir(mode=0o700)
        names = ('upgrade_host.py', 'upgrade.py', 'upgrade_engine.py', 'upgrade_policy.json',
                 'release_verify.py', 'release_contract.py', 'release_install_v2.py',
                 'install-release.py', 'release-source.json', 'upgrade_smoke.py',
                 'upgrade_backup_contract.py', 'upgrade_inventory.py')
        hashes = {}
        for name in names:
            path = bundle / name; path.write_bytes(b'inert fixture'); path.chmod(0o600)
            hashes[name] = hashlib.sha256(path.read_bytes()).hexdigest()

        def check(files):
            raw = c.canonical({'schema': 'ags.upgrade-tool-bundle.v2', 'entry': 'upgrade_host.py', 'files': files})
            path = bundle / 'manifest.json'; path.write_bytes(raw); path.chmod(0o600)
            original_stat = Path.stat

            def fixture_owner(path, *args, **kwargs):
                info = original_stat(path, *args, **kwargs)
                if path in bundle.parents:
                    # Only model provisioned ancestor ownership; files/digests are real.
                    values = list(info); values[4] = os.getuid()
                    return os.stat_result(values)
                return info

            with patch.object(Path, 'stat', fixture_owner):
                return upgrade.bundle_engine(bundle, hashlib.sha256(raw).hexdigest(), owner=os.getuid())

        self.assertEqual(check(hashes), bundle / 'upgrade_host.py')
        for name in ('upgrade_backup_contract.py', 'upgrade_inventory.py'):
            with self.subTest(name=name), self.assertRaises(upgrade.Failure) as caught:
                check({k: v for k, v in hashes.items() if k != name})
            self.assertEqual(caught.exception.exit_code, 69)
        check(hashes)
        (bundle / 'upgrade_inventory.py').write_bytes(b'changed')
        with self.assertRaises(upgrade.Failure): check(hashes)

    def test_s1_never_opens_apply_backup_or_direct_install(self):
        for machine in ('mini', 'imile-win'):
            binding = {'host': machine}; effects = Mock(); state = self.root / 'absent-state'
            with self.assertRaises(upgrade.Failure) as caught:
                engine.Transaction(state, binding, authorization(binding), effects).run()
            self.assertEqual(caught.exception.code, 'backup_contract_unimplemented')
            self.assertEqual(caught.exception.exit_code, 83)
            self.assertFalse(state.exists()); self.assertEqual(effects.mock_calls, [])
            adapter = object.__new__(host.Effects); adapter.binding = binding
            with self.assertRaises(upgrade.Failure) as backup:
                adapter.backup()
            self.assertEqual(backup.exception.exit_code, 83)
            with self.assertRaises(upgrade.Failure) as wrapper:
                upgrade.require_apply_contracts(machine)
            self.assertEqual(wrapper.exception.exit_code, 83)
        with self.assertRaises(install_v2.InstallClosed) as direct:
            install_v2.require_install_contracts()
        self.assertEqual(direct.exception.exit_code, 83)
        policy = json.loads((ROOT / 'scripts/upgrade_policy.json').read_bytes())
        self.assertEqual(policy['deployments'], {}); self.assertEqual(policy['migration_admissions'], [])


class SourceMappingTests(unittest.TestCase):
    def test_git_maintenance_queries_constraints_and_deployments_match_source(self):
        source = (ROOT / 'internal/service/git_maintenance.go').read_text()
        matches = re.findall(r'\{"([^"\n]+)", "([^"\n]+)", \[\]any\{([^}]+)\}, \[\]string\{([^}]+)\}\}', source)
        parsed = [(table, where, len(args.split(',')), tuple(re.findall(r'"([^"]+)"', cols)))
                  for table, where, args, cols in matches]
        self.assertEqual(parsed, list(inv.ROOT_QUERIES))
        constraints = re.findall(r'\{"(access_grant_invocations|delegated_agent_sessions)", "([^"]+)"\}', source)
        self.assertEqual(constraints, list(inv.CONSTRAINT_COLUMNS))
        key_match = re.search(r'for _, key := range \[\]string\{([^}]+)\}', source)
        self.assertEqual(tuple(re.findall(r'"([^"]+)"', key_match[1])), inv.CONSTRAINT_KEYS)
        self.assertIn('database.Model(&db.Deployment{})', source)
        self.assertIn('Pluck("ref", &deploymentRefs)', source)
        # Pin control flow as well as extracted literals: a changed predicate needs review.
        self.assertEqual(hashlib.sha256(source.encode()).hexdigest(),
                         'd3f8395608a44f64a9448633c28108060b6fd01d431bcc334e22d69e6e5eaf4a')

    def test_recovery_set_and_storage_source_cannot_silently_drift(self):
        path = ROOT / 'scripts/ags_maintenance_window.py'
        tree = ast.parse(path.read_text())
        assignment = next(v for v in tree.body if isinstance(v, ast.Assign)
                          and any(isinstance(t, ast.Name) and t.id == 'TABLE_KEYS' for t in v.targets))
        self.assertEqual(set(ast.literal_eval(assignment.value)), inv.RECOVERY_TABLES)
        applychange = (ROOT / 'internal/wikicatalog/applychange.go').read_text()
        delete = applychange.split('func (c *Catalog) applyDelete(', 1)[1].split('func (c *Catalog) applyRename(', 1)[0]
        self.assertIn('BlobSHA:     ""', delete)
        self.assertIn('BodySize:    0', delete)
        self.assertIn('Op:          revOpDelete', delete)
        self.assertIn('"deleted_at":', delete)
        self.assertIn('"head_revision_id":', delete)
        self.assertIn('"head_changeset_id":', delete)
        self.assertNotIn('head_blob_sha', delete)
        self.assertNotIn('body_inline', delete)
        upsert = applychange.split('func pageUpsertColumns(', 1)[1]
        self.assertIn('"head_blob_sha":', upsert)
        self.assertIn('"body_inline":', upsert)
        self.assertIn('out["deleted_at"] = nil', upsert)
        # Independent AST lock of the complete recovery selectors/auxiliary semantics.
        selected = [node for node in tree.body if isinstance(node, ast.FunctionDef)
                    and node.name in ('recovery_inventory', 'retirement_auxiliary', 'store_fsck')]
        # Hash source segments, not ast.dump: Python releases add AST fields.
        segments = '\n'.join(ast.get_source_segment(path.read_text(), node) for node in selected)
        self.assertEqual(hashlib.sha256(segments.encode()).hexdigest(),
                         'e87686cab2a58b93c88d0b02428991ec56835ad6dd58ef0f42961a3fa2399fb6')
        model_sources = [(str(p.relative_to(ROOT)), hashlib.sha256(p.read_bytes()).hexdigest())
                         for p in sorted((ROOT / 'internal/db').glob('*.go'))
                         if not p.name.endswith('_test.go')]
        self.assertEqual(c.fingerprint(model_sources), 'aba5c0035242f8e952352c857d45caf978b3f5aac2bb11fa2abebeee86a9f616')
        for path, expected in SOURCE_LOCKS.items():
            self.assertEqual(hashlib.sha256((ROOT / path).read_bytes()).hexdigest(), expected, path)

    def test_known_table_inventory_matches_migrated_models(self):
        source = (ROOT / 'internal/db/db.go').read_text()
        block = source.split('database.AutoMigrate(', 1)[1].split('); err != nil', 1)[0]
        models = re.findall(r'&(\w+)\{\}', block)
        all_source = '\n'.join(p.read_text() for p in sorted((ROOT / 'internal/db').glob('*.go'))
                               if not p.name.endswith('_test.go'))
        overrides = dict(re.findall(r'func \((\w+)\) TableName\(\) string\s*\{\s*return "([^"]+)"', all_source))
        initialisms = ('SSH', 'GPG', 'PR', 'CI')

        def table_name(name):
            if name in overrides: return overrides[name]
            for word in initialisms:
                name = name.replace(word, word.title())
            value = re.sub(r'(?<!^)(?=[A-Z])', '_', name).lower()
            if value.endswith('y') and value[-2] not in 'aeiou': return value[:-1] + 'ies'
            if value.endswith(('s', 'ch', 'sh', 'x', 'z')): return value + 'es'
            return value + 's'

        self.assertEqual({table_name(name) for name in models}, inv.KNOWN_TABLES)


SOURCE_LOCKS = {
    'scripts/ags_maintenance_window.py': '81553239d9d8a06eda87ac21b4ebf5ac12732b1efc58cd8a8d0a7a81a1d340b5',
    'internal/db/db.go': '2bb26abd84aee743b14491a40f5b6b817b19e8bf85573665b7ab5a7f63fe3d26',
    'internal/gitstore/store.go': '4f8a58cdb16bbf94a0a96aa7e6cfac290c8a2b2f91366b39f1a9b52791c684e0',
    'internal/wikicatalog/blobstore.go': 'f366a2a76475f1200c1bbe05c06252a77ffc364f6c9a08fbcf9ec3057659e75b',
    'internal/wikicatalog/gc.go': 'c3904aceb0366be0f00e2d7740b7ec9eb4bab92758dd9226b93984d5d0cf6dc9',
    'internal/wikicatalog/apply.go': 'f9eb2996be5e80054dd600e88fd98d98279a0a9cc44c79b4112df65e4feb40b5',
    'internal/wikicatalog/applychange.go': 'acffc58e78d73a4ef8712ce3bd34fcc223a7b241acb6c7046329b6fa2909af99',
    'internal/db/models_wiki_catalog.go': '30f5b16b21185acca0021854a522f3d1ea5bb13b28833d3f404f155922820197',
    'internal/db/models_wiki_v2.go': '75a8d514af3bc91bef8f1a4441e3c18a7cc74f34e5845da1ba7de904080b1a52',
}

if __name__ == '__main__':
    unittest.main()
