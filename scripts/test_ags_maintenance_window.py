#!/usr/bin/env python3
"""Hermetic admission tests: synthetic SQLite, real Git, no server or provider.

All files stay under a newly created private directory. The directory is retained
for inspection; this harness never deletes or connects to any external service.
"""
import contextlib
import datetime as dt
import fcntl
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import platform
import sqlite3
import subprocess
import tempfile
import time
import types
import unittest
from unittest.mock import patch

import ags_maintenance_window as w


ROOT = Path(tempfile.mkdtemp(prefix='ags-window-tests-')).resolve()
os.umask(0o077)


def save(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value))
    path.chmod(0o600)


def strict_hashes(root):
    result = {}
    for directory, dirs, files in os.walk(root):
        for name in dirs + files:
            path = Path(directory) / name
            w.require(not path.is_symlink() and (path.is_dir() or path.is_file()), 'non_regular_file_forbidden')
            if path.is_file():
                result[str(path.relative_to(root))] = w.sha(path)
    return dict(sorted(result.items()))


def command(args, **kwargs):
    kwargs.pop('timeout', None)
    return subprocess.check_output(args, stderr=subprocess.PIPE, text=True, **kwargs)


class Fixture(unittest.TestCase):
    def setUp(self):
        self.root = Path(tempfile.mkdtemp(dir=ROOT))
        self.db = self.root / 'fixture.sqlite'
        with contextlib.closing(sqlite3.connect(self.db)) as db, db:
            db.executescript('''
                CREATE TABLE pull_requests(id INTEGER PRIMARY KEY, state TEXT, merged INTEGER,
                    forgejo_reopen_pending INTEGER, lifecycle_generation INTEGER);
                CREATE TABLE pull_request_projection_jobs(id INTEGER PRIMARY KEY, provider TEXT,
                    phase TEXT, "trigger" TEXT, last_error_type TEXT, pull_request_id INTEGER,
                    attempt INTEGER, action_generation INTEGER, next_run_at TEXT, head_sha TEXT);
                CREATE TABLE pull_request_action_intents(id TEXT PRIMARY KEY, action TEXT, state TEXT,
                    provider_effect_status TEXT, provider_outcome TEXT, boundary_receipt_id TEXT);
                CREATE TABLE authority_boundary_receipts(receipt_id TEXT PRIMARY KEY, payload TEXT);
                CREATE TABLE access_grant_invocations(id TEXT PRIMARY KEY, state TEXT, provider_attempt TEXT, provider_outcome TEXT);
                CREATE TABLE outbound_deliveries(id INTEGER PRIMARY KEY, status TEXT,
                    lease_expires_at TEXT, lease_owner TEXT, payload_json TEXT, target_type TEXT);
                CREATE TABLE repositories(id INTEGER PRIMARY KEY, full_name TEXT);
                CREATE TABLE wiki_pages(page_id INTEGER PRIMARY KEY, repository_id INTEGER, slug TEXT,
                    head_blob_sha TEXT, deleted_at TEXT, body_inline BLOB);
                CREATE TABLE wiki_search_documents(id INTEGER PRIMARY KEY, repository_id INTEGER,
                    slug TEXT, revision_sha TEXT, body TEXT);
                CREATE TABLE wiki_search_projection_tasks(id INTEGER PRIMARY KEY, repository_id INTEGER,
                    slug TEXT, kind TEXT, generation INTEGER, revision_sha TEXT, label_digest TEXT,
                    lease_token TEXT, lease_expires_at TEXT);
                CREATE TABLE wiki_repo_heads(repository_id INTEGER PRIMARY KEY,
                    reference_effects_through_changeset_id INTEGER);
                CREATE TABLE wiki_page_labels(repository_id INTEGER, slug TEXT, label_id INTEGER,
                    PRIMARY KEY(repository_id, slug, label_id));
                CREATE TABLE labels(id INTEGER PRIMARY KEY, name TEXT);
                INSERT INTO repositories VALUES(1,'sample/project');
                INSERT INTO pull_requests VALUES(1,'open',0,0,7);
            ''')

    def sql(self, query, args=()):
        with contextlib.closing(sqlite3.connect(self.db)) as db, db:
            db.execute(query, args)

    def job(self, number, phase='failed_retryable', provider='forgejo', trigger='pull_request_projection', error=''):
        self.sql('INSERT INTO pull_request_projection_jobs VALUES(?,?,?,?,?,1,50,7,NULL,?)',
                 (number, provider, phase, trigger, error, 'a' * 40))

    def evidence_proof(self):
        path = self.root / ('proof-' + str(time.time_ns()))
        save(path, {'synthetic': True})
        return {'path': str(path), 'sha256': w.sha(path), 'passed': True}

    def review(self, entries):
        return {'items': [dict(key=e['key'], facts_sha256=e['facts_sha256'], decision='allow_exact_resume',
                               reviewer='fixture-reviewer', recovery_class=e['recovery_class'], expected_effect_sha256='a' * 64,
                               unknown_external_results=0, readback=self.evidence_proof(),
                               exact_resume_test=self.evidence_proof()) for e in entries]}

    def test_zero_and_read_only(self):
        before = w.sha(self.db)
        entries = w.recovery_inventory(self.db)
        self.assertEqual(entries, [])
        w.recovery_gate(entries, {'items': []})
        self.assertEqual(w.sha(self.db), before)
        with w.readonly(self.db) as db:
            with self.assertRaises(sqlite3.OperationalError):
                db.execute('INSERT INTO pull_requests(id) VALUES(2)')

    def test_non_utf8_text_is_lossless_private_and_read_only(self):
        self.job(1, 'projected')
        self.job(2)
        self.sql('ALTER TABLE pull_request_projection_jobs ADD COLUMN last_error TEXT')
        raw = (b'synthetic-private-error\x00\xff\xc3(\xed\xa0\x80' + bytes(range(256)) * 4)[:1000]
        self.assertEqual(len(raw), 1000)
        for number in (1, 2):
            self.sql('UPDATE pull_request_projection_jobs SET last_error=CAST(? AS TEXT) WHERE id=?',
                     (raw, number))
        before = w.sha(self.db)
        # The old reader fails even on the terminal row excluded from recovery.
        with w.readonly(self.db) as db:
            row = dict(db.execute('SELECT * FROM pull_request_projection_jobs WHERE id=1').fetchone())
            self.assertEqual(row['last_error'].encode('utf-8', 'surrogateescape'), raw)
            rendered = json.dumps(row, ensure_ascii=True)
            self.assertEqual(json.loads(rendered)['last_error'].encode('utf-8', 'surrogateescape'), raw)
            rendered.encode('ascii')
            self.assertEqual(db.execute('PRAGMA query_only').fetchone()[0], 1)
            with self.assertRaises(sqlite3.OperationalError):
                db.execute("UPDATE pull_request_projection_jobs SET last_error='cleaned'")
        entries = w.recovery_inventory(self.db)
        self.assertEqual([e['key'] for e in entries], ['pull_request_projection_jobs:2'])
        self.assertNotIn('synthetic-private-error', json.dumps(entries))
        self.assertNotIn('\\udcff', json.dumps(entries))
        self.assertEqual(w.sha(self.db), before)
        review = self.review(entries)
        self.sql('UPDATE pull_request_projection_jobs SET last_error=CAST(? AS TEXT) WHERE id=2',
                 (raw[:-1] + bytes([raw[-1] ^ 1]),))
        with self.assertRaisesRegex(w.Blocked, 'recovery_facts_drift'):
            w.recovery_gate(w.recovery_inventory(self.db), review)
        with w.readonly(self.db) as db:
            self.assertEqual(bytes(db.execute('SELECT CAST(last_error AS BLOB) FROM pull_request_projection_jobs WHERE id=1').fetchone()[0]), raw)

    def test_utf8_text_fingerprint_stays_compatible(self):
        self.job(1)
        self.sql('ALTER TABLE pull_request_projection_jobs ADD COLUMN last_error TEXT')
        self.sql('UPDATE pull_request_projection_jobs SET last_error=?', ('synthetic café 中文 \\udcff',))
        with contextlib.closing(sqlite3.connect(self.db)) as db, db:
            db.row_factory = sqlite3.Row
            row = dict(db.execute('SELECT * FROM pull_request_projection_jobs').fetchone())
            pr = dict(db.execute('SELECT * FROM pull_requests').fetchone())
        self.assertEqual(w.recovery_inventory(self.db)[0]['facts_sha256'],
                         w.fingerprint({'row': row, 'pr': pr}))
        # Invalid text in linked PR/receipt facts is also read without loss.
        self.sql('ALTER TABLE pull_requests ADD COLUMN description TEXT')
        self.sql('UPDATE pull_requests SET description=CAST(? AS TEXT)', (b'private\xff',))
        self.assertEqual(len(w.recovery_inventory(self.db)), 1)
        self.sql('INSERT INTO authority_boundary_receipts VALUES(?,CAST(? AS TEXT))', ('r1', b'private\xfe'))
        self.sql('INSERT INTO pull_request_action_intents VALUES(?,?,?,?,?,?)',
                 ('i1', 'pr.rebase', 'completed', 'verified_completed', 'completed', 'r1'))
        entries = w.recovery_inventory(self.db)
        self.assertEqual(len(entries), 2)
        review = self.review(entries)
        self.sql('UPDATE authority_boundary_receipts SET payload=CAST(? AS TEXT)', (b'private\xff',))
        with self.assertRaisesRegex(w.Blocked, 'recovery_facts_drift'):
            w.recovery_gate(w.recovery_inventory(self.db), review)

    def test_sixth_terminal_requeue_requires_review(self):
        for number in (109, 1355, 1356, 1358, 1361):
            self.job(number)
        reviewed = w.recovery_inventory(self.db)
        review = self.review(reviewed)
        self.job(8384, 'failed_terminal', error='provider_admission_denied')
        entries = w.recovery_inventory(self.db)
        self.assertEqual(len(entries), 6)
        self.assertEqual(next(e['reason'] for e in entries if e['key'].endswith(':8384')), 'terminal_requeue')
        with self.assertRaisesRegex(w.Blocked, 'coverage'):
            w.recovery_gate(entries, review)
        w.recovery_gate(entries, self.review(entries))

    def test_all_future_retries_providers_and_reopen(self):
        for number in range(1, 151):
            self.job(number, 'queued', provider='github', trigger='github_shadow_ensure')
        self.sql("UPDATE pull_request_projection_jobs SET next_run_at='2099-01-01'")
        self.sql('UPDATE pull_requests SET forgejo_reopen_pending=1')
        entries = w.recovery_inventory(self.db)
        self.assertEqual(len(entries), 151)
        self.assertEqual({e['recovery_class'] for e in entries}, {'startup_execute'})
        review = self.review(entries)
        w.recovery_gate(entries, review)
        review['items'].pop()
        with self.assertRaisesRegex(w.Blocked, 'coverage'):
            w.recovery_gate(entries, review)

    def test_intents_receipts_leases_and_private_output(self):
        secret = 'SYNTHETIC_PRIVATE_PAYLOAD_DO_NOT_EXPORT'
        self.sql('INSERT INTO authority_boundary_receipts VALUES(?,?)', ('r1', secret))
        self.sql('INSERT INTO pull_request_action_intents VALUES(?,?,?,?,?,?)',
                 ('i1', 'pr.rebase', 'denied', 'outcome_unknown', '', 'r1'))
        self.sql('INSERT INTO outbound_deliveries(id,status,lease_expires_at,lease_owner,payload_json) VALUES(?,?,?,?,?)', (1, 'delivered', None, 'lease', secret))
        entries = w.recovery_inventory(self.db)
        self.assertEqual(len(entries), 2)
        self.assertNotIn(secret, json.dumps(entries))
        review = self.review(entries)
        review['items'][0]['unknown_external_results'] = 1
        with self.assertRaisesRegex(w.Blocked, 'disposition'):
            w.recovery_gate(entries, review)
        review = self.review(entries)
        self.sql("UPDATE authority_boundary_receipts SET payload='changed'")
        with self.assertRaisesRegex(w.Blocked, 'drift'):
            w.recovery_gate(w.recovery_inventory(self.db), review)

    def test_review_drift_duplicates_and_missing_proof(self):
        self.job(1)
        entries = w.recovery_inventory(self.db)
        review = self.review(entries)
        self.sql('UPDATE pull_request_projection_jobs SET attempt=51')
        with self.assertRaisesRegex(w.Blocked, 'drift'):
            w.recovery_gate(w.recovery_inventory(self.db), review)
        review = self.review(entries)
        review['items'] *= 2
        with self.assertRaisesRegex(w.Blocked, 'duplicate'):
            w.recovery_gate(entries, review)
        review = self.review(entries)
        Path(review['items'][0]['readback']['path']).write_text('changed')
        with self.assertRaisesRegex(w.Blocked, 'proof_drift'):
            w.recovery_gate(entries, review)

    def test_unknown_invocation_receipt_cannot_hide_behind_terminal_state(self):
        self.sql('INSERT INTO access_grant_invocations VALUES(?,?,?,?)',
                 ('invocation', 'completed', 'attempted', 'outcome_unknown'))
        entries = w.recovery_inventory(self.db)
        self.assertEqual(len(entries), 1)
        self.assertTrue(entries[0]['unknown_result'])
        with self.assertRaisesRegex(w.Blocked, 'coverage'):
            w.recovery_gate(entries, {'items': []})

    def test_unknown_schema_blocks_and_old_lifecycle_supported(self):
        self.sql('ALTER TABLE pull_requests DROP COLUMN forgejo_reopen_pending')
        self.assertEqual(w.recovery_inventory(self.db), [])
        self.sql('ALTER TABLE pull_request_projection_jobs DROP COLUMN last_error_type')
        with self.assertRaisesRegex(w.Blocked, 'schema_unknown'):
            w.recovery_inventory(self.db)

    def wiki_page(self, number=1, revision='current', deleted=None):
        self.sql('INSERT INTO wiki_pages VALUES(?,1,?,?,?,?)',
                 (number, 'page-' + str(number), revision, deleted, b'synthetic private body'))

    def wiki_task(self, number=1, kind='lexical', expiry=None):
        self.sql('INSERT INTO wiki_search_projection_tasks VALUES(?,1,?,?,1,?,?,?,?)',
                 (number, 'page-' + str(number), kind, 'current', 'labels', 'private-lease', expiry))

    def test_recovery_classes_match_startup_selectors_without_narrowing_audit(self):
        for number, action, state, outcome in (
                ('active', 'pr.rebase', 'planned', 'not_attempted'),
                ('terminal', 'pr.rebase', 'completed', None),
                ('other', 'pr.merge', 'planned', 'not_attempted'),
                ('unrecognized', 'pr.rebase', 'new_state', 'not_attempted')):
            self.sql('INSERT INTO pull_request_action_intents VALUES(?,?,?,?,?,NULL)',
                     (number, action, state, 'not_attempted', outcome))
        self.sql('INSERT INTO access_grant_invocations VALUES(?,?,?,NULL)',
                 ('null-outcome', 'completed', 'attempted'))
        self.job(1, 'failed_terminal', error='provider_admission_denied')
        self.job(2, 'failed_terminal')  # retired: must not be resurrected
        self.job(3, 'new_phase', provider='unknown')
        self.sql('UPDATE pull_requests SET forgejo_reopen_pending=1,state="closed"')
        self.sql('INSERT INTO outbound_deliveries VALUES(1,"delivered",NULL,"retained",NULL,NULL)')
        self.sql('INSERT INTO outbound_deliveries VALUES(2,"pending",NULL,"",NULL,NULL)')
        entries = {e['key']: e for e in w.recovery_inventory(self.db)}
        executing = {'pull_request_action_intents:active', 'pull_request_projection_jobs:1',
                     'outbound_deliveries:2'}
        self.assertNotIn('pull_request_projection_jobs:2', entries)
        for key, entry in entries.items():
            self.assertEqual(entry['recovery_class'], 'startup_execute' if key in executing else 'audit_only', key)
        self.assertTrue(entries['pull_request_action_intents:terminal']['unknown_result'])
        self.assertTrue(entries['access_grant_invocations:null-outcome']['unknown_result'])
        review = self.review(list(entries.values()))
        review['items'][0]['recovery_class'] = 'wrong'
        with self.assertRaisesRegex(w.Blocked, 'class_drift'):
            w.recovery_gate(list(entries.values()), review)

    def test_projection_phase_retirement_and_claim_classification(self):
        phases = ('queued', 'preflight', 'rebasing', 'projection_resume', 'pushing_ref',
                  'verifying_ref', 'verifying_pr', 'ensuring_pr', 'recording_projection', 'failed_retryable')
        for number, phase in enumerate(phases, 1):
            self.job(number, phase)
        self.job(11, 'new_phase')
        self.job(12, 'new_phase', provider='unknown', trigger='forgejo_action_rebase')
        self.job(13, 'projected')
        self.sql('ALTER TABLE pull_request_projection_jobs ADD COLUMN success_comment_claim_token TEXT')
        self.sql('UPDATE pull_request_projection_jobs SET success_comment_claim_token="retained" WHERE id=13')
        entries = {e['key']: e for e in w.recovery_inventory(self.db)}
        self.assertEqual(len(entries), 13)
        self.assertEqual(entries['pull_request_projection_jobs:11']['recovery_class'], 'audit_only')
        self.assertEqual(entries['pull_request_projection_jobs:13']['recovery_class'], 'audit_only')
        for number in list(range(1, 11)) + [12]:
            self.assertEqual(entries['pull_request_projection_jobs:' + str(number)]['recovery_class'], 'startup_execute')
        self.sql('UPDATE pull_requests SET state="closed"')
        entries = {e['key']: e for e in w.recovery_inventory(self.db)}
        self.assertEqual(entries['pull_request_projection_jobs:11']['recovery_class'], 'startup_execute')

    def test_outbound_recovery_classes_preserve_terminal_and_unknown_audit(self):
        for number, status, target in ((1, 'pending', 'any'), (2, 'retry_wait', 'any'),
                (3, 'delivering', 'multica_external_pr'), (4, 'delivering', 'legacy'),
                (5, 'dead_letter', 'multica_external_pr'), (6, 'new_status', 'any'),
                (7, 'delivered', 'any')):
            self.sql('INSERT INTO outbound_deliveries VALUES(?,?,?,"owner",NULL,?)',
                     (number, status, '2099-01-01', target))
        entries = w.recovery_inventory(self.db)
        self.assertEqual(len(entries), 7)
        for entry in entries:
            number = int(entry['key'].split(':')[1])
            self.assertEqual(entry['recovery_class'], 'startup_execute' if number <= 3 else 'audit_only')

    def test_wiki_leases_marker_and_repair_inputs_require_exact_coverage(self):
        self.wiki_page()
        self.wiki_task(expiry='2099-01-01')
        self.wiki_task(2, 'embedding')  # no page/document: worker still consumes it
        self.wiki_task(3, 'unknown-kind')  # no startup drain for this kind
        self.sql('INSERT INTO wiki_search_documents VALUES(1,1,"page-1","old","private")')
        self.sql('INSERT INTO wiki_search_documents VALUES(2,1,"orphan","old","private")')
        self.wiki_task(4, 'embedding', expiry='2000-01-01')
        self.sql('INSERT INTO wiki_repo_heads VALUES(1,0)')  # IS NOT NULL, not truthiness
        entries = w.recovery_inventory(self.db)
        expected = {
            'wiki_search_projection_tasks:1': 'startup_execute',
            'wiki_search_projection_tasks:2': 'startup_execute',
            'wiki_search_projection_tasks:3': 'audit_only',
            'wiki_search_projection_tasks:4': 'startup_execute',
            'wiki_pages:1': 'startup_execute',
            'wiki_search_documents:1': 'audit_only',
            'wiki_search_documents:2': 'startup_execute',
            'wiki_repo_heads:1': 'startup_execute',
        }
        self.assertEqual({e['key']: e['recovery_class'] for e in entries}, expected)
        w.recovery_gate(entries, self.review(entries))
        for entry in entries:
            with self.subTest(missing=entry['key']):
                review = self.review(entries)
                review['items'] = [i for i in review['items'] if i['key'] != entry['key']]
                with self.assertRaisesRegex(w.Blocked, 'coverage'):
                    w.recovery_gate(entries, review)
        self.assertNotIn('private', json.dumps(entries))
        before = w.sha(self.db)
        w.recovery_inventory(self.db)
        self.assertEqual(w.sha(self.db), before)
        review = self.review(entries)
        self.sql('UPDATE wiki_search_projection_tasks SET lease_token="changed" WHERE id=1')
        with self.assertRaisesRegex(w.Blocked, 'facts_drift'):
            w.recovery_gate(w.recovery_inventory(self.db), review)

    def test_wiki_repair_sql_null_deleted_and_matching_controls(self):
        for number, revision, deleted in ((1, 'same', None), (2, 'new', None),
                                          (3, 'new', '2000-01-01'), (4, 'new', None)):
            self.wiki_page(number, revision, deleted)
        for number, revision in ((1, 'same'), (2, None), (3, 'old')):
            self.sql('INSERT INTO wiki_search_documents VALUES(?,1,?,?,NULL)',
                     (number, 'page-' + str(number), revision))
        self.sql('INSERT INTO wiki_repo_heads VALUES(1,NULL)')
        entries = {e['key']: e for e in w.recovery_inventory(self.db)}
        executing = {'wiki_pages:4', 'wiki_search_documents:3'}
        self.assertEqual(len(entries), 8)
        for key, entry in entries.items():
            self.assertEqual(entry['recovery_class'], 'startup_execute' if key in executing else 'audit_only', key)

    def test_wiki_full_scan_over_100_and_linked_fact_drift(self):
        for number in range(1, 152):
            self.wiki_page(number)
            self.wiki_task(number, expiry='2099-01-01')
        entries = w.recovery_inventory(self.db)
        self.assertEqual(len(entries), 302)
        task_key = 'wiki_search_projection_tasks:151'
        original = next(e for e in entries if e['key'] == task_key)
        self.sql('UPDATE wiki_pages SET body_inline=? WHERE page_id=151', (b'changed body',))
        current = next(e for e in w.recovery_inventory(self.db) if e['key'] == task_key)
        self.assertNotEqual(original['facts_sha256'], current['facts_sha256'])
        review = self.review(w.recovery_inventory(self.db))
        self.sql('UPDATE repositories SET full_name="sample/renamed"')
        with self.assertRaisesRegex(w.Blocked, 'facts_drift'):
            w.recovery_gate(w.recovery_inventory(self.db), review)
        review = self.review(w.recovery_inventory(self.db))
        self.sql('INSERT INTO labels VALUES(1,"private label")')
        self.sql('INSERT INTO wiki_page_labels VALUES(1,"page-151",1)')
        with self.assertRaisesRegex(w.Blocked, 'facts_drift'):
            w.recovery_gate(w.recovery_inventory(self.db), review)

    def test_wiki_marker_binds_all_pages_and_marker_drift(self):
        self.wiki_page()
        self.wiki_page(2)
        self.sql('INSERT INTO wiki_repo_heads VALUES(1,42)')
        entries = w.recovery_inventory(self.db)
        marker = next(e for e in entries if e['key'] == 'wiki_repo_heads:1')
        review = self.review(entries)
        self.sql('UPDATE wiki_pages SET body_inline=? WHERE page_id=2', (b'changed',))
        changed = w.recovery_inventory(self.db)
        self.assertNotEqual(marker['facts_sha256'], next(
            e['facts_sha256'] for e in changed if e['key'] == marker['key']))
        with self.assertRaisesRegex(w.Blocked, 'facts_drift'):
            w.recovery_gate(changed, review)
        review = self.review(changed)
        self.sql('UPDATE wiki_repo_heads SET reference_effects_through_changeset_id=43')
        with self.assertRaisesRegex(w.Blocked, 'facts_drift'):
            w.recovery_gate(w.recovery_inventory(self.db), review)
        review = self.review(w.recovery_inventory(self.db))
        self.sql('UPDATE wiki_repo_heads SET reference_effects_through_changeset_id=NULL')
        with self.assertRaisesRegex(w.Blocked, 'class_drift|facts_drift'):
            w.recovery_gate(w.recovery_inventory(self.db), review)

    def test_active_intent_states_and_terminal_receipt_are_not_replayed(self):
        states = ('planned', 'dispatching', 'dispatched', 'accepted', 'running', 'recovery_needed')
        for state in states:
            self.sql('INSERT INTO pull_request_action_intents VALUES(?,?,?,?,?,NULL)',
                     (state, 'pr.rebase', state, 'not_attempted', 'not_attempted'))
        self.sql('INSERT INTO authority_boundary_receipts VALUES("r1","private")')
        self.sql('INSERT INTO pull_request_action_intents VALUES(?,?,?,?,?,?)',
                 ('terminal', 'pr.rebase', 'denied', 'verified_completed', 'completed', 'r1'))
        entries = w.recovery_inventory(self.db)
        self.assertEqual(len(entries), 7)
        for entry in entries:
            self.assertEqual(entry['recovery_class'], 'audit_only' if entry['key'].endswith(':terminal')
                             else 'startup_execute')
        review = self.review(entries)
        review['items'][-1].pop('recovery_class')
        with self.assertRaisesRegex(w.Blocked, 'class_drift'):
            w.recovery_gate(entries, review)
        review = self.review(entries)
        review['items'].append(dict(review['items'][0], key='wiki_pages:unreviewed-extra'))
        with self.assertRaisesRegex(w.Blocked, 'coverage'):
            w.recovery_gate(entries, review)

    def test_wiki_missing_repository_or_label_blocks(self):
        self.wiki_page()
        self.sql('INSERT INTO wiki_page_labels VALUES(1,"page-1",1)')
        with self.assertRaisesRegex(w.Blocked, 'label_missing'):
            w.recovery_inventory(self.db)
        self.sql('INSERT INTO labels VALUES(1,"synthetic")')
        self.sql('DELETE FROM repositories WHERE id=1')
        with self.assertRaisesRegex(w.Blocked, 'repository_missing'):
            w.recovery_inventory(self.db)

    def test_new_wiki_schema_missing_blocks_even_empty(self):
        self.sql('DROP TABLE wiki_search_projection_tasks')
        with self.assertRaisesRegex(w.Blocked, 'schema_unknown'):
            w.recovery_inventory(self.db)

    def store(self):
        store = self.root / 'store'
        repo = store / 'sample' / 'project.git'
        repo.parent.mkdir(parents=True)
        command(['git', 'init', '--bare', '--quiet', str(repo)], env={'PATH': '/usr/bin:/bin', 'HOME': str(self.root), 'GIT_CONFIG_NOSYSTEM': '1'})
        aux = store / '.ags-artifact-retirement'
        aux.mkdir(mode=0o700)
        lock = aux / 'operation.lock'
        lock.touch(mode=0o600)
        return store, aux

    def store_module(self):
        return types.SimpleNamespace(strict_hashes=strict_hashes, h=types.SimpleNamespace(command=command))

    def test_real_git_legal_lock_is_hashed_and_copied(self):
        import shutil
        store, aux = self.store()
        result = w.store_fsck(self.store_module(), store, ['sample/project.git'])
        self.assertIn('.ags-artifact-retirement/operation.lock', result['files'])
        copy = self.root / 'restore'
        shutil.copytree(store, copy)
        restored = w.store_fsck(self.store_module(), copy, ['sample/project.git'])
        self.assertEqual(result['files'], restored['files'])
        with self.assertRaisesRegex(w.Blocked, 'coverage'):
            w.store_fsck(self.store_module(), store, [])

    def test_pending_unknown_nonempty_permissions_and_busy_lock(self):
        store, aux = self.store()
        lock = aux / 'operation.lock'
        for name in ('pending.json', 'receipt.json', 'unknown', 'force-window.json'):
            path = aux / name
            path.touch(mode=0o600)
            with self.assertRaisesRegex(w.Blocked, 'pending_or_unknown'):
                w.store_fsck(self.store_module(), store)
            path.rename(self.root / name)
        lock.write_text('unexpected')
        with self.assertRaisesRegex(w.Blocked, 'not_empty'):
            w.retirement_auxiliary(aux)
        lock.write_text('')
        lock.chmod(0o644)
        with self.assertRaisesRegex(w.Blocked, 'unsafe'):
            w.retirement_auxiliary(aux)
        lock.chmod(0o600)
        with lock.open('rb') as stream:
            fcntl.flock(stream, fcntl.LOCK_EX | fcntl.LOCK_NB)
            with self.assertRaisesRegex(w.Blocked, 'busy'):
                w.retirement_auxiliary(aux)

    def test_symlink_fifo_unknown_hidden_and_incomplete_repo(self):
        for kind in ('symlink', 'fifo', 'hidden', 'missing_head'):
            with self.subTest(kind=kind):
                store, aux = self.store_in_new_directory()
                path = aux / 'unexpected'
                if kind == 'symlink':
                    path.symlink_to(self.db)
                elif kind == 'fifo':
                    os.mkfifo(path)
                elif kind == 'hidden':
                    (store / '.unknown').mkdir()
                else:
                    (store / 'sample/project.git/HEAD').rename(self.root / ('saved-head-' + str(time.time_ns())))
                with self.assertRaises(w.Blocked):
                    w.store_fsck(self.store_module(), store)

    def store_in_new_directory(self):
        original = self.root
        self.root = Path(tempfile.mkdtemp(dir=ROOT))
        result = self.store()
        self.root = original
        return result

    def test_candidate_hash_rejected_before_execution_and_overlay(self):
        module = types.SimpleNamespace(NEW='a' * 40, TREE='b' * 40, read=lambda p: json.loads(p.read_text()))
        host = types.SimpleNamespace(host='fixture', candidate=self.root / 'candidate')
        host.identity = lambda *args: self.fail('unverified bytes executed')
        binary = self.root / 'binary'
        binary.write_bytes(b'fixture candidate')
        binary.chmod(0o700)
        review = dict(schema='ags.maintenance-candidate.v1', host='fixture', reviewer='reviewer',
                      artifact_kind='exact-source', os={'Darwin': 'darwin', 'Linux': 'linux'}[platform.system()],
                      arch={'arm64': 'arm64', 'aarch64': 'arm64', 'x86_64': 'amd64'}[platform.machine()],
                      revision=module.NEW, tree=module.TREE, binary=str(binary), binary_sha256='0' * 64)
        path = self.root / 'review'
        save(path, review)
        with self.assertRaisesRegex(w.Blocked, 'identity_mismatch'):
            w.import_candidate(module, host, path)
        review.update(binary_sha256=w.sha(binary), artifact_kind='loopback-overlay')
        save(path, review)
        with self.assertRaisesRegex(w.Blocked, 'reviewed_exact'):
            w.import_candidate(module, host, path)

    def test_executor_tamper_rejected_before_import(self):
        for name in w.EXECUTOR_FILES:
            (self.root / name).write_text("raise RuntimeError('must not execute')")
        with self.assertRaisesRegex(w.Blocked, 'digest_mismatch'):
            w.load_executor(self.root)


class BaseEngine:
    """Persistence seam only. Real fix2 compatibility is checked separately."""
    def __init__(self, host, args):
        self.host, self.a = host, args
        self.file = host.work / 'phase-v2.json'

    def state(self):
        return json.loads(self.file.read_text())

    def mark(self, phase, **extra):
        state = self.state() if self.file.exists() else {'run': self.a.run, 'host': self.a.host,
                                                       'started_new': False}
        state.update(phase=phase, updated_ns=time.time_ns(), **extra)
        save(self.file, state)
        return state

    def offline(self):
        return contextlib.nullcontext()

    def run(self):
        assert self.a.action == "validate"
        self.host.health("a" * 40)
        self.mark("new-ready")


class Stages(unittest.TestCase):
    sql = Fixture.sql
    job = Fixture.job
    evidence_proof = Fixture.evidence_proof
    review = Fixture.review
    def setUp(self):
        Fixture.setUp(self)
        self.work = self.root / 'work'
        self.work.mkdir()
        self.start_calls = 0
        self.health_failure = False
        self.host = types.SimpleNamespace(work=self.work, root=self.root, backup=self.root / 'backup',
                                          build_receipt=self.root / 'candidate-info', production_db=lambda: self.db,
                                          quiescent=lambda: None, verify_backup=lambda: {}, doctor=lambda: None,
                                          start=self.start, health=self.health)
        binary = self.root / 'bin/gh-server'
        binary.parent.mkdir()
        binary.write_bytes(b'exact fixture')
        identity = {'revision': 'a' * 40, 'tree': 'b' * 40}
        self.host.identity = lambda *args: identity
        save(self.host.build_receipt, {'identity': identity, 'binary_sha256': w.sha(binary)})
        self.host.backup.mkdir()
        save(self.host.backup / 'manifest.json', {'synthetic': True})
        self.module = types.SimpleNamespace(Host=object, Engine=BaseEngine, NEW=identity['revision'], digest=w.sha,
                                            read=lambda p: json.loads(p.read_text()), save=save)
        self.args = types.SimpleNamespace(run='ags-fixture', host='fixture', evidence=self.root / 'evidence',
                                          service_url='https://example.ts.net', action='start-new')
        _, Engine = w.adapter_types(self.module)
        self.engine = Engine(self.host, self.args)
        self.engine.mark('installed-new', manifest_sha256=w.sha(self.host.backup / 'manifest.json'))
        self.install_calls = 0
        self.host.install = self.install
        self.module.fsck = lambda *args: None
        self.module.repository_inventory = lambda *args: []
        self.module.c = types.SimpleNamespace(verify_sqlite=lambda *args: None)

    def install(self):
        self.install_calls += 1

    def stopped_evidence(self, kind='resume', exit_code=0):
        runtime = self.root / 'state/runtime.json'
        save(runtime, {'exit_code': exit_code, 'started_at': 'synthetic',
                       'stopped_at': dt.datetime.now(dt.timezone.utc).isoformat()})
        e = self.evidence(kind)
        e.update(normal_exit_no_timeout_no_sigkill=True, no_direct_file_writers=True,
                 callers_paused=True, autostart_suppressed=True)
        e['offline_record'] = self.bound_offline_record(e)
        save(self.args.evidence, e)
        return e

    def bound_offline_record(self, e):
        path = self.root / ('offline-' + str(time.time_ns()))
        record = {key: e[key] for key in ('run', 'host', 'mode', 'phase', 'nonce', 'kind', 'observed_ns')}
        record['runtime_sha256'] = w.sha(self.root / 'state/runtime.json')
        save(path, record)
        return {'path': str(path), 'sha256': w.sha(path), 'passed': True}

    def assert_resume_blocked(self, pattern):
        before = self.engine.state()
        with self.assertRaisesRegex((w.Blocked, KeyError), pattern):
            self.engine.run()
        self.assertEqual(self.engine.state(), before)
        self.assertEqual(self.install_calls, 0)
        self.assertEqual(self.start_calls, 0)

    def test_resume_requires_clean_exit_and_fresh_bound_offline_proof(self):
        self.engine.mark('start-attempted', started_new=True, start_attempted_ns=time.time_ns())
        self.args.action = 'resume-new'
        for code in (137, -9, 1, None, False):
            with self.subTest(exit_code=code):
                self.stopped_evidence(exit_code=code)
                self.assert_resume_blocked('runtime_exit_not_clean')
        for field in ('normal_exit_no_timeout_no_sigkill', 'no_direct_file_writers',
                      'autostart_suppressed', 'offline_record', 'callers_paused'):
            with self.subTest(missing=field):
                e = self.stopped_evidence()
                e.pop(field)
                save(self.args.evidence, e)
                self.assert_resume_blocked('stop_not_proven|offline_record|writers_not_reviewed')
        for field in ('normal_exit_no_timeout_no_sigkill', 'autostart_suppressed'):
            with self.subTest(false=field):
                e = self.stopped_evidence()
                e[field] = False
                save(self.args.evidence, e)
                self.assert_resume_blocked('stop_not_proven')
        e = self.stopped_evidence()
        Path(e['offline_record']['path']).write_text('changed')
        self.assert_resume_blocked('proof_drift')
        e = self.stopped_evidence()
        e['observed_ns'] -= 301 * 10**9
        save(self.args.evidence, e)
        self.assert_resume_blocked('evidence_time_invalid')
        self.stopped_evidence()
        with patch.object(self.host, 'quiescent', side_effect=w.Blocked('holder_present')):
            self.assert_resume_blocked('holder_present')
        e = self.stopped_evidence()
        runtime = self.root / 'state/runtime.json'
        for field in ('exit_code', 'stopped_at'):
            with self.subTest(runtime_missing=field):
                self.stopped_evidence()
                value = json.loads(runtime.read_text())
                value.pop(field)
                save(runtime, value)
                self.assert_resume_blocked('runtime_exit_not_clean|offline_runtime_drift')
        for field, value in (('nonce', 'old'), ('phase', 'new-ready'), ('run', 'ags-other'),
                             ('runtime_sha256', '0' * 64), ('observed_ns', 0)):
            with self.subTest(offline_drift=field):
                e = self.stopped_evidence()
                path = Path(e['offline_record']['path'])
                record = json.loads(path.read_text())
                record[field] = value
                save(path, record)
                e['offline_record']['sha256'] = w.sha(path)
                save(self.args.evidence, e)
                self.assert_resume_blocked('offline_record_binding|offline_runtime_drift')
        e = self.stopped_evidence()
        value = json.loads(runtime.read_text())
        value['stopped_at'] = '2000-01-01T00:00:00+00:00'
        save(runtime, value)
        e['offline_record'] = self.bound_offline_record(e)
        save(self.args.evidence, e)
        self.assert_resume_blocked('runtime_stop_predates_start')
        self.stopped_evidence()
        self.engine.run()
        self.assertEqual(self.engine.state()['phase'], 'installed-new')
        self.assertEqual(self.install_calls, 1)
        self.assertEqual(self.start_calls, 0)
        self.args.action = 'start-new'
        self.evidence('start')
        self.engine.run()
        self.assertEqual(self.start_calls, 1)

    def test_resume_stop_gate_applies_to_every_admitted_phase(self):
        for phase in ('install-allowed', 'installing-new', 'installed-new',
                      'start-attempted', 'new-ready', 'observing'):
            with self.subTest(phase=phase):
                self.start_calls = self.install_calls = 0
                self.engine.mark(phase, started_new=True, start_attempted_ns=time.time_ns())
                self.args.action = 'resume-new'
                self.stopped_evidence(exit_code=137)
                self.assert_resume_blocked('runtime_exit_not_clean')
                self.stopped_evidence()
                self.engine.run()
                self.assertEqual(self.engine.state()['phase'], 'installed-new')
                self.assertEqual(self.install_calls, 1)
                self.assertEqual(self.start_calls, 0)

    def test_observing_recovery_requires_stop_readback_and_restarts_observation(self):
        for failure in ('crash', 'health', 'provider'):
            with self.subTest(failure=failure):
                self.start_calls = self.install_calls = 0
                self.engine.mark('observing', started_new=True, start_attempted_ns=time.time_ns(),
                                 observation_started_ns=time.time_ns() - 700 * 10**9)
                old_observation = self.engine.state()['observation_started_ns']
                self.args.action = 'observe'
                e = self.evidence('observe')
                e.update(required_observation_seconds=600, all_actual_leases_covered=True,
                         unknown_external_results=0, observation_record=self.evidence_proof(),
                         provider_readback=self.evidence_proof())
                self.health_failure = failure in ('crash', 'health')
                if failure == 'provider':
                    Path(e['provider_readback']['path']).write_text('changed')
                save(self.args.evidence, e)
                with self.assertRaises(w.Blocked):
                    self.engine.run()
                self.assertEqual(self.engine.state()['phase'], 'observing')
                self.args.action = 'resume-new'
                self.stopped_evidence(exit_code=137 if failure == 'crash' else 0)
                if failure == 'crash':
                    self.assert_resume_blocked('runtime_exit_not_clean')
                    # A crash has no normal-stop positive path in this protocol.
                    # Do not rewrite its runtime result to manufacture recovery.
                    self.assertEqual(self.engine.state()['phase'], 'observing')
                    continue
                e = json.loads(self.args.evidence.read_text())
                self.job(8384, 'failed_terminal', error='provider_admission_denied')
                self.assert_resume_blocked('coverage')
                self.stopped_evidence()
                self.engine.run()
                self.assertEqual(self.start_calls, 0)
                self.assertEqual(self.engine.state()['phase'], 'installed-new')
                self.assertIsNone(self.engine.state()['observation_started_ns'])
                self.health_failure = False
                self.args.action = 'start-new'
                self.evidence('start')
                self.engine.run()
                self.args.action = 'validate'
                e = self.evidence('validate')
                e.update(business_reads=self.evidence_proof(), provider_readback=self.evidence_proof())
                save(self.args.evidence, e)
                self.engine.run()
                self.assertGreater(self.engine.state()['observation_started_ns'], old_observation)
                self.args.action = 'observe'
                e = self.evidence('observe')
                e.update(required_observation_seconds=600, all_actual_leases_covered=True,
                         unknown_external_results=0, observation_record=self.evidence_proof(),
                         provider_readback=self.evidence_proof())
                save(self.args.evidence, e)
                with self.assertRaisesRegex(w.Blocked, 'observation_incomplete'):
                    self.engine.run()
                self.assertEqual(self.start_calls, 1)
                self.sql('DELETE FROM pull_request_projection_jobs')

    def test_candidate_install_drift_rejected_before_identity(self):
        candidate = self.root / 'staged-candidate'
        candidate.write_bytes(b'exact fixture')
        candidate.chmod(0o700)
        self.host.candidate = candidate
        self.host.identity = lambda *args: self.fail('candidate identity executed before hash check')
        Host, _ = w.adapter_types(self.module)
        for kind in ('hash', 'permissions', 'symlink'):
            with self.subTest(kind=kind):
                if kind == 'hash':
                    candidate.write_bytes(b'changed')
                elif kind == 'permissions':
                    candidate.write_bytes(b'exact fixture')
                    candidate.chmod(0o755)
                else:
                    candidate.rename(self.root / 'saved-candidate')
                    candidate.symlink_to(self.root / 'saved-candidate')
                with self.assertRaisesRegex(w.Blocked, 'candidate_drift|unsafe_private_file'):
                    Host.install(self.host)


    def start(self):
        self.start_calls += 1
        self.assertEqual(self.engine.state()['phase'], 'start-attempted')
        self.assertTrue(self.engine.state()['started_new'])

    def health(self, expected):
        if self.health_failure:
            raise w.Blocked('synthetic_health_timeout')

    def evidence(self, kind):
        state = self.engine.state()
        proof = self.evidence_proof()
        e = dict(run=self.args.run, host=self.args.host, nonce=state['nonce'], phase=state['phase'],
                 mode='maintenance-window', kind=kind, observed_ns=time.time_ns(), operator='fixture',
                 passed=True, service_binding_verified=True, service_url=self.args.service_url,
                 route_record=proof, start_restores_business=True, default_forward=True,
                 recovery_review=self.review(w.recovery_inventory(self.db)))
        save(self.args.evidence, e)
        return e

    def test_start_without_fences_and_duplicate_start_blocked(self):
        self.evidence('start')
        self.engine.run()
        self.assertEqual(self.start_calls, 1)
        self.assertEqual(self.engine.state()['phase'], 'new-ready')
        with self.assertRaisesRegex(w.Blocked, 'wrong_phase'):
            self.engine.run()
        self.assertEqual(self.start_calls, 1)

    def test_timeout_preserves_marker_readback_never_starts(self):
        self.evidence('start')
        self.health_failure = True
        with self.assertRaisesRegex(w.Blocked, 'timeout'):
            self.engine.run()
        self.assertEqual(self.engine.state()['phase'], 'start-attempted')
        self.args.action = 'read-new-start'
        self.evidence('new-readback')
        with self.assertRaisesRegex(w.Blocked, 'timeout'):
            self.engine.run()
        self.health_failure = False
        self.engine.run()
        self.assertEqual(self.start_calls, 1)
        self.assertEqual(self.engine.state()['phase'], 'new-ready')

    def test_prestop_required_before_stop_and_offline_before_start(self):
        self.engine.mark('prepared')
        self.args.action = 'stop'
        with self.assertRaisesRegex(w.Blocked, 'wrong_phase'):
            self.engine.run()
        self.assertEqual(self.start_calls, 0)
        self.engine.mark('installed-new')
        self.args.action = 'start-new'
        self.evidence('start')
        self.host.quiescent = lambda: w.require(False, 'fixture_holder_present')
        with self.assertRaisesRegex(w.Blocked, 'holder_present'):
            self.engine.run()
        self.assertEqual(self.engine.state()['phase'], 'installed-new')
        self.assertEqual(self.start_calls, 0)

    def test_start_rejects_changed_approved_manifest(self):
        self.evidence('start')
        save(self.host.backup / 'manifest.json', {'changed': True})
        with self.assertRaisesRegex(w.Blocked, 'approved_backup_drift'):
            self.engine.run()
        self.assertEqual(self.start_calls, 0)

    def test_changed_recovery_blocks_before_start(self):
        self.evidence('start')
        self.job(8384, 'failed_terminal', error='provider_admission_denied')
        with self.assertRaisesRegex(w.Blocked, 'coverage'):
            self.engine.run()
        self.assertEqual(self.start_calls, 0)

    def test_reviewed_nonzero_recovery_can_start(self):
        self.job(8384, 'failed_terminal', error='provider_admission_denied')
        self.evidence('start')
        self.engine.run()
        self.assertEqual(self.start_calls, 1)

    def test_stale_nonce_wrong_mode_and_restore_actions_fail(self):
        self.evidence('start')
        self.engine.mark('installed-new')
        with self.assertRaisesRegex(w.Blocked, 'stale'):
            self.engine.run()
        state = self.engine.state()
        state['mode'] = 'isolated'
        save(self.engine.file, state)
        with self.assertRaisesRegex(w.Blocked, 'mode_mismatch'):
            self.engine.run()
        state['mode'] = 'maintenance-window'
        save(self.engine.file, state)
        for action in ('restore-old', 'start-old-isolated', 'abort-before-install', 'stage-successor'):
            self.args.action = action
            with self.assertRaisesRegex(w.Blocked, 'separate_isolated_recovery'):
                self.engine.run()
        self.assertEqual(self.start_calls, 0)

    def test_observation_requires_time_readback_and_recovery(self):
        self.engine.mark('observing', observation_started_ns=time.time_ns())
        self.args.action = 'observe'
        e = self.evidence('observe')
        e.update(required_observation_seconds=600, all_actual_leases_covered=True,
                 unknown_external_results=0, observation_record=self.evidence_proof(),
                 provider_readback=self.evidence_proof())
        save(self.args.evidence, e)
        with self.assertRaisesRegex(w.Blocked, 'observation_incomplete'):
            self.engine.run()
        state = self.engine.state()
        state['observation_started_ns'] -= 601 * 10**9
        save(self.engine.file, state)
        self.engine.run()
        self.assertEqual(self.engine.state()['phase'], 'complete')


@unittest.skipUnless(os.environ.get('AGS_REVIEWED_EXECUTOR_DIR'), 'external reviewed executor not supplied')
class ReviewedExecutor(unittest.TestCase):
    setUp = Fixture.setUp
    sql = Fixture.sql
    job = Fixture.job
    evidence_proof = Fixture.evidence_proof
    review = Fixture.review
    store = Fixture.store
    def test_real_executor_stages_and_isolated_fences(self):
        import shutil
        m = w.load_executor(Path(os.environ['AGS_REVIEWED_EXECUTOR_DIR']))
        original_fsck = m.fsck
        store, aux = self.store()
        # Reproduce the reported fix2 error before installing the adapter.
        with self.assertRaisesRegex(m.h.Failure, 'unrecognized_store_file'):
            original_fsck(store, ['sample/project.git'])
        m.fsck = lambda store, expected=None: w.store_fsck(m, store, expected)
        m.fsck(store, ['sample/project.git'])
        # Exact schema validation is exercised by the upstream executor tests;
        # this fixture uses the already-migrated synthetic schema.
        WindowHost, Engine = w.adapter_types(m)
        outer = self

        class Host(WindowHost):
            def __init__(self, args):
                self.args, self.host, self.run = args, 'fixture', args.run
                self.root = outer.root
                self.work = outer.root / 'run'
                self.backup = outer.root / 'backups/run'
                self.service = outer.root / 'service'
                self.old, self.sha = m.OLD, m.NEW
                self.candidate = self.work / 'gh-server'
                self.build_receipt = self.work / 'candidate-build-info.json'
                self.start_count, self.stopped = 0, False
                self.fault = None

            def identity(self, binary=None):
                return json.loads((binary or self.root / 'bin/gh-server').read_text())

            def doctor(self):
                pass

            def health(self, revision):
                w.require(self.identity()['revision'] == revision, 'fixture_health_failed')

            def idle(self):
                pass

            def production_db(self):
                return outer.db

            def build(self):
                w.import_candidate(m, self, self.args.candidate_review)

            def stop(self):
                self.stopped = True
                save(self.root / 'state/runtime.json', {'exit_code': 0,
                     'stopped_at': dt.datetime.now(dt.timezone.utc).isoformat()})
                if self.fault == 'stop':
                    raise w.Blocked('fixture_stop_reply_lost')

            def install(self):
                super().install()
                if self.fault == 'install':
                    raise w.Blocked('fixture_install_reply_lost')

            def start(self):
                self.start_count += 1
                self.stopped = False

            def quiescent(self):
                w.require(self.stopped, 'fixture_not_stopped')

            def drained(self):
                return contextlib.nullcontext()

            def snapshot_db(self):
                (self.backup / 'db').mkdir()
                destination = self.backup / 'db/ags.sqlite'
                with contextlib.closing(sqlite3.connect(outer.db)) as src, contextlib.closing(sqlite3.connect(destination)) as dst:
                    src.backup(dst)
                return {'ags.sqlite': {'sha256': w.sha(destination), 'size': destination.stat().st_size}}

        for name in ('bin', 'state', 'config', 'backups', 'data'):
            (self.root / name).mkdir(mode=0o700, exist_ok=True)
        shutil.copytree(store, self.root / 'data/gitrepos')
        identity = {'revision': m.OLD, 'tree': 'c' * 40, 'version': 'fixture-old'}
        save(self.root / 'bin/gh-server', identity)
        for name in ('ags-edge', 'ags-replication'):
            (self.root / 'bin' / name).write_text('fixture')
        save(self.root / 'service', {'synthetic': True})
        for name in ('installation.json', 'build-info.json', 'runtime.json', 'startup-health.json'):
            save(self.root / 'state' / name, {'binaries': {}})
        binary = self.root / 'exact-candidate'
        save(binary, dict(revision=m.NEW, tree=m.TREE, version='fixture-new'))
        review_path = self.root / 'candidate-review'
        save(review_path, dict(schema='ags.maintenance-candidate.v1', host='fixture', reviewer='reviewer',
                              artifact_kind='exact-source', os={'Darwin': 'darwin', 'Linux': 'linux'}[platform.system()],
                              arch={'arm64': 'arm64', 'aarch64': 'arm64', 'x86_64': 'amd64'}[platform.machine()],
                              binary=str(binary), binary_sha256=w.sha(binary), revision=m.NEW, tree=m.TREE,
                              build=self.evidence_proof(), smoke=self.evidence_proof(), startup_configuration=self.evidence_proof()))
        args = types.SimpleNamespace(host='fixture', run='ags-fixture', action='prepare',
                                     service_url='https://example.ts.net', candidate_review=review_path,
                                     evidence=self.root / 'stage-evidence')
        host = Host(args)
        engine = Engine(host, args)
        engine.run()
        dependency = self.root / 'completed-retirement-receipt'
        save(dependency, {'status': 'completed', 'synthetic': True})
        kinds = [('gate-prestop', 'prestop'), ('stop', 'stop'), ('gate-drain', 'drain'),
                 ('snapshot', 'snapshot-fences'), ('gate-backup', 'backup'), ('gate-git', 'git'),
                 ('install', 'install'), ('start-new', 'start')]
        for action, kind in kinds:
            args.action = action
            state = engine.state()
            e = dict(run=args.run, host=args.host, nonce=state['nonce'], phase=state['phase'],
                     kind=kind, mode='maintenance-window', observed_ns=time.time_ns(), operator='fixture',
                     passed=True, service_binding_verified=True, service_url=args.service_url,
                     route_record=self.evidence_proof(), start_restores_business=True, default_forward=True,
                     callers_paused=True, no_direct_file_writers=True, startup_dependencies=self.evidence_proof(),
                     dependency_files=[{'path': str(dependency), 'sha256': w.sha(dependency)}],
                     normal_exit_no_timeout_no_sigkill=True, autostart_suppressed=True,
                     offline_record=self.evidence_proof(), recovery_review=self.review(w.recovery_inventory(self.db)))
            def bind_offline():
                record = {key: e[key] for key in ('run', 'host', 'mode', 'phase', 'nonce', 'kind', 'observed_ns')}
                record['runtime_sha256'] = w.sha(host.root / 'state/runtime.json')
                path = self.root / ('offline-' + str(time.time_ns()))
                save(path, record)
                e['offline_record'] = {'path': str(path), 'sha256': w.sha(path), 'passed': True}

            if action == 'gate-drain':
                bind_offline()
            save(args.evidence, e)
            if action == 'install':
                original = host.candidate.read_bytes()
                host.candidate.write_bytes(original + b' ')
                with patch.object(host, 'identity', side_effect=AssertionError('unverified candidate executed')):
                    with self.assertRaisesRegex(w.Blocked, 'candidate_drift'):
                        engine.run()
                host.candidate.write_bytes(original)
                current = engine.state()
                e.update(nonce=current['nonce'], phase=current['phase'], observed_ns=time.time_ns())
                save(args.evidence, e)
                # A pre-install rejection is preserved at installing-new; recover
                # through the same reviewed offline path, not a phase edit.
                args.action, e['kind'] = 'resume-new', 'resume'
                bind_offline()
                save(args.evidence, e)
            if action in ('stop', 'install'):
                host.fault = action
                with self.assertRaisesRegex(w.Blocked, 'reply_lost'):
                    engine.run()
                self.assertEqual(engine.state()['phase'], 'stop-requested' if action == 'stop' else 'installing-new')
                host.fault = None
                if action == 'install':
                    args.action, e['kind'] = 'resume-new', 'resume'
                current = engine.state()
                e.update(nonce=current['nonce'], phase=current['phase'], observed_ns=time.time_ns())
                if action == 'install':
                    bind_offline()
                save(args.evidence, e)
            engine.run()
        self.assertEqual(engine.state()['phase'], 'new-ready')
        self.assertEqual(host.start_count, 1)
        self.assertEqual(w.sha(host.backup / 'startup-dependencies/0'), w.sha(dependency))
        self.assertTrue((host.backup / 'gitrepos/.ags-artifact-retirement/operation.lock').is_file())
        # Resume checks run against the actual pinned Engine, Host installation,
        # SQLite verifier and backup/Git path, not only the persistence seam.
        args.action = 'resume-new'
        for code in (137, None, 0):
            host.stopped = True
            save(host.root / 'state/runtime.json', {'exit_code': code,
                 'stopped_at': dt.datetime.now(dt.timezone.utc).isoformat()})
            state = engine.state()
            e.update(kind='resume', phase=state['phase'], nonce=state['nonce'], observed_ns=time.time_ns())
            bind_offline()
            save(args.evidence, e)
            before = host.start_count
            if code != 0:
                with self.assertRaisesRegex(w.Blocked, 'runtime_exit_not_clean'):
                    engine.run()
                self.assertEqual(engine.state()['phase'], 'new-ready')
            else:
                engine.run()
                self.assertEqual(engine.state()['phase'], 'installed-new')
            self.assertEqual(host.start_count, before)
        # Original isolated evidence still rejects missing fences; R2 implementation
        # is inherited unchanged and is never invoked by maintenance recovery.
        isolated = m.Engine(host, args)
        state = engine.state()
        e.update(phase=state['phase'], nonce=state['nonce'], kind='old-resume', observed_ns=time.time_ns(),
                 route_record_sha256='a' * 64)
        save(args.evidence, e)
        with self.assertRaisesRegex(m.h.Failure, 'fence_evidence_missing'):
            isolated.evidence('old-resume')


if __name__ == '__main__':
    unittest.main(verbosity=2)
