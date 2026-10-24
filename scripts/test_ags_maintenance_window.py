#!/usr/bin/env python3
"""Hermetic admission tests: synthetic SQLite, real Git, no server or provider.

All files stay under a newly created private directory. The directory is retained
for inspection; this harness never deletes or connects to any external service.
"""
import contextlib
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
        with sqlite3.connect(self.db) as db:
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
                    lease_expires_at TEXT, lease_owner TEXT, payload_json TEXT);
                INSERT INTO pull_requests VALUES(1,'open',0,0,7);
            ''')

    def sql(self, query, args=()):
        with sqlite3.connect(self.db) as db:
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
                               reviewer='fixture-reviewer', expected_effect_sha256='a' * 64,
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
            self.job(number, 'queued', provider='github', trigger='github_ensure')
        self.sql("UPDATE pull_request_projection_jobs SET next_run_at='2099-01-01'")
        self.sql('UPDATE pull_requests SET forgejo_reopen_pending=1')
        self.assertEqual(len(w.recovery_inventory(self.db)), 151)

    def test_intents_receipts_leases_and_private_output(self):
        secret = 'SYNTHETIC_PRIVATE_PAYLOAD_DO_NOT_EXPORT'
        self.sql('INSERT INTO authority_boundary_receipts VALUES(?,?)', ('r1', secret))
        self.sql('INSERT INTO pull_request_action_intents VALUES(?,?,?,?,?,?)',
                 ('i1', 'pr.rebase', 'denied', 'outcome_unknown', '', 'r1'))
        self.sql('INSERT INTO outbound_deliveries VALUES(?,?,?,?,?)', (1, 'delivered', None, 'lease', secret))
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
        self.sql('CREATE TABLE repositories(id INTEGER PRIMARY KEY, full_name TEXT)')
        self.sql("INSERT INTO repositories VALUES(1,'sample/project')")
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
                save(self.root / 'state/runtime.json', {'exit_code': 0, 'stopped_at': 'fixture'})
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
                with sqlite3.connect(outer.db) as src, sqlite3.connect(destination) as dst:
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
                save(args.evidence, e)
            engine.run()
        self.assertEqual(engine.state()['phase'], 'new-ready')
        self.assertEqual(host.start_count, 1)
        self.assertEqual(w.sha(host.backup / 'startup-dependencies/0'), w.sha(dependency))
        self.assertTrue((host.backup / 'gitrepos/.ags-artifact-retirement/operation.lock').is_file())
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
