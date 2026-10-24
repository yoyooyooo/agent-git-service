#!/usr/bin/env python3
"""Opt-in maintenance-window adapter for an independently reviewed deployment executor.

The external executor stays private. Its default isolated mode is byte-pinned and
unchanged. This module never contains a production host, path or credential.
"""
import argparse
import contextlib
import fcntl
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import sqlite3
import stat
import sys
import time

# These are reviewed code identities, not credentials or runtime evidence.
EXECUTOR_FILES = {
    'manual-deploy.py': '2322715590a53c79089542bf07ffc7be6006a9a4d78c755df3cd363b7c4d8313',
    'original-manual.py': '68aa4973f7aaade0e8eb4aa97168d8bacf96e8d89ca52f52fd57adf76f013fe6',
    'host-reviewed.py': '9f223e8c4697c16f559ac928cfadb4e0d06116c6ffcfa8420fe22f72355ac0d2',
}


class Blocked(Exception):
    pass


def require(value, code):
    if not value:
        raise Blocked(code)


def sha(path):
    result = hashlib.sha256()
    with path.open('rb') as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b''):
            result.update(block)
    return result.hexdigest()


def fingerprint(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':'),
                                     ensure_ascii=True).encode()).hexdigest()


def private_file(path):
    info = path.lstat()
    require(stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid()
            and not info.st_mode & 0o077, 'unsafe_private_file')


def proof(item):
    require(isinstance(item, dict) and item.get('passed') is True, 'proof_not_passed')
    path = Path(item['path'])
    private_file(path)
    require(sha(path) == item.get('sha256'), 'proof_drift')


def load_executor(directory):
    for name, expected in EXECUTOR_FILES.items():
        path = directory / name
        require(path.is_file() and not path.is_symlink() and sha(path) == expected,
                'executor_digest_mismatch')
    spec = importlib.util.spec_from_file_location('reviewed_manual_deploy', directory / 'manual-deploy.py')
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


@contextlib.contextmanager
def readonly(path):
    require(path.is_absolute() and path.is_file() and not path.is_symlink(), 'unsafe_database')
    db = sqlite3.connect(path.as_uri() + '?mode=ro', uri=True, timeout=5)
    db.row_factory = sqlite3.Row
    try:
        db.execute('PRAGMA query_only=ON')
        db.execute('BEGIN')
        yield db
    finally:
        db.close()


# Scan all rows, including future retries beyond startup's first bounded batch.
# Entire rows are hashed privately; no payload, endpoint or error text is exported.
TABLE_KEYS = {
    'pull_request_projection_jobs': ('id', {'provider', 'phase', 'trigger', 'last_error_type', 'pull_request_id', 'attempt', 'action_generation', 'next_run_at'}),
    'pull_requests': ('id', {'state', 'merged'}),
    'pull_request_action_intents': ('id', {'action', 'state', 'provider_effect_status', 'provider_outcome', 'boundary_receipt_id'}),
    'authority_boundary_receipts': ('receipt_id', {'payload'}),
    'access_grant_invocations': ('id', {'state', 'provider_attempt', 'provider_outcome'}),
    'outbound_deliveries': ('id', {'status', 'lease_expires_at', 'lease_owner'}),
}
JOB_TERMINAL = {'projected', 'failed_terminal'}
INTENT_TERMINAL = {'completed', 'denied', 'conflict', 'projection_failed'}


def recovery_inventory(path):
    entries = []
    with readonly(path) as db:
        tables = {}
        for table, (key, fields) in TABLE_KEYS.items():
            columns = {row[1] for row in db.execute('PRAGMA table_info(' + table + ')')}
            require(fields | {key} <= columns, 'recovery_schema_unknown')
            tables[table] = [dict(row) for row in db.execute('SELECT * FROM ' + table + ' ORDER BY ' + key)]

        def add(table, row, reason, unknown=False):
            key = TABLE_KEYS[table][0]
            # Bind linked PR facts and immutable receipt lineage as well as job facts.
            bound = {'row': row}
            if row.get('pull_request_id') is not None:
                linked = [pr for pr in tables['pull_requests'] if pr['id'] == row['pull_request_id']]
                require(len(linked) == 1, 'recovery_pr_missing')
                bound['pr'] = linked[0]
            receipt = row.get('boundary_receipt_id')
            if receipt:
                linked = [r for r in tables['authority_boundary_receipts'] if r['receipt_id'] == receipt]
                require(len(linked) == 1, 'boundary_receipt_missing')
                bound['receipt'] = linked[0]
            entries.append({'key': table + ':' + str(row[key]), 'reason': reason,
                            'facts_sha256': fingerprint(bound), 'unknown_result': unknown})

        for row in tables['pull_request_projection_jobs']:
            heal = (row['provider'] == 'forgejo' and row['trigger'] == 'pull_request_projection'
                    and row['phase'] == 'failed_terminal' and row['last_error_type'] == 'provider_admission_denied')
            # All nonterminal providers/triggers are conservative admission candidates;
            # disabled integrations and future retry times cannot hide queued work.
            if row['phase'] not in JOB_TERMINAL or heal or row.get('success_comment_claim_token') or (row.get('success_comment_claimed_at') and not row.get('success_commented_at')):
                add('pull_request_projection_jobs', row, 'terminal_requeue' if heal else 'projection_resume')
        for row in tables['pull_requests']:
            # Old schema legitimately lacks these three additive lifecycle columns.
            if row.get('forgejo_reopen_pending'):
                add('pull_requests', row, 'reopen_pending')
        for row in tables['pull_request_action_intents']:
            unknown = (row['provider_effect_status'] not in ('', 'not_attempted', 'verified_completed')
                       or row['provider_outcome'] not in ('', 'not_attempted', 'verified_completed', 'completed', 'confirmed', 'reconciled_after_error', 'source_success', 'not_applicable'))
            if row['state'] not in INTENT_TERMINAL or unknown or row.get('boundary_receipt_id'):
                add('pull_request_action_intents', row, 'intent_recovery', unknown)
        for row in tables['access_grant_invocations']:
            known = row['provider_outcome'] in ('not_attempted', 'not_applicable', 'confirmed',
                                               'reconciled_after_error', 'source_success')
            if row['state'] not in INTENT_TERMINAL or not known:
                add('access_grant_invocations', row, 'invocation_outcome', not known)
        for row in tables['outbound_deliveries']:
            leased = bool(row['lease_owner']) or row['lease_expires_at'] is not None
            if row['status'] != 'delivered' or leased:
                add('outbound_deliveries', row, 'delivery_or_lease', leased or row['status'] == 'delivering')
    return sorted(entries, key=lambda entry: entry['key'])


def recovery_gate(entries, review):
    require(isinstance(review, dict) and isinstance(review.get('items'), list), 'recovery_review_missing')
    items = review['items']
    require(len(items) == len({item['key'] for item in items}), 'duplicate_recovery_review')
    require({item['key'] for item in items} == {entry['key'] for entry in entries}, 'recovery_review_coverage')
    by_key = {item['key']: item for item in items}
    for entry in entries:
        item = by_key[entry['key']]
        require(item.get('facts_sha256') == entry['facts_sha256'], 'recovery_facts_drift')
        require(item.get('decision') == 'allow_exact_resume' and item.get('reviewer')
                and re.fullmatch(r'[0-9a-f]{64}', item.get('expected_effect_sha256', ''))
                and type(item.get('unknown_external_results')) is int and item['unknown_external_results'] == 0,
                'recovery_disposition_missing')
        # Unknown DB flags require explicit provider readback, never a boolean waiver.
        proof(item['readback'])
        proof(item['exact_resume_test'])
    return fingerprint(entries)


def retirement_auxiliary(directory):
    """Source contract: startup_gate.go + state_safety.go, no pending work admitted."""
    info = directory.lstat()
    require(stat.S_ISDIR(info.st_mode) and info.st_uid == os.getuid()
            and not info.st_mode & 0o077, 'unsafe_retirement_directory')
    require({p.name for p in directory.iterdir()} == {'operation.lock'}, 'retirement_pending_or_unknown')
    lock = directory / 'operation.lock'
    private_file(lock)
    require(lock.stat().st_size == 0, 'retirement_lock_not_empty')
    # Opening an existing lock read-only neither truncates nor creates it.
    with lock.open('rb') as stream:
        try:
            fcntl.flock(stream, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise Blocked('retirement_lock_busy') from None


def store_fsck(module, store, expected=None):
    """R1 full namespace/DB coverage, with one explicit auxiliary directory."""
    files = module.strict_hashes(store)
    repos, auxiliary, directories = [], [], []

    def visit(directory):
        relative = str(directory.relative_to(store))
        if directory != store:
            directories.append(relative)
        names = {p.name for p in directory.iterdir()}
        if directory.name.endswith('.git') or names & {'HEAD', 'objects', 'refs', 'config'}:
            require(directory != store, 'repository_at_store_root_forbidden')
            for name in ('HEAD', 'config'):
                require((directory / name).is_file(), 'incomplete_repository_' + name)
            for name in ('objects', 'refs'):
                require((directory / name).is_dir(), 'incomplete_repository_' + name)
            repos.append(directory)
            return
        if directory == store / '.ags-artifact-retirement':
            retirement_auxiliary(directory)
            auxiliary.append(relative)
            return
        if directory == store / '.github-app-deliveries':
            require(all(p.is_file() and re.fullmatch(r'[0-9a-f]{64}\.json', p.name)
                        for p in directory.iterdir()), 'unknown_delivery_spool_entry')
            auxiliary.append(relative)
            return
        children = sorted(directory.iterdir())
        require(directory == store or children, 'unrecognized_empty_store_directory')
        require(all(p.is_dir() for p in children), 'unrecognized_store_file')
        for child in children:
            visit(child)

    visit(store)
    paths = sorted(str(repo.relative_to(store)) for repo in repos)
    if expected is not None:
        require(paths == sorted(expected), 'repository_inventory_coverage_mismatch')
    env = {'PATH': '/usr/bin:/bin:/usr/sbin:/sbin', 'HOME': '/nonexistent',
           'GIT_CONFIG_NOSYSTEM': '1', 'GIT_CONFIG_GLOBAL': '/dev/null',
           'GIT_OPTIONAL_LOCKS': '0', 'GIT_NO_REPLACE_OBJECTS': '1',
           'GIT_CONFIG_COUNT': '0', 'GIT_TERMINAL_PROMPT': '0', 'GIT_ALLOW_PROTOCOL': 'file'}
    for repo in repos:
        require(not (repo / 'objects/info/alternates').exists(), 'git_alternates_forbidden')
        require(not (repo / 'shallow').exists(), 'shallow_repository_forbidden')
        command = ['git', '--git-dir=' + str(repo), '-c', 'core.hooksPath=/dev/null']
        require(module.h.command(command + ['rev-parse', '--is-bare-repository'], env=env).strip() == 'true', 'not_bare')
        module.h.command(command + ['fsck', '--full', '--strict'], env=env, timeout=600)
    return {'repositories': len(repos), 'repository_paths': paths, 'namespace_directories': directories,
            'auxiliary_paths': auxiliary, 'files': files}


def import_candidate(module, host, review_path):
    private_file(review_path)
    review = module.read(review_path)
    binary = Path(review['binary'])
    private_file(binary)
    require(review.get('schema') == 'ags.maintenance-candidate.v1'
            and review.get('host') == host.host and review.get('reviewer')
            and review.get('artifact_kind') == 'exact-source', 'candidate_not_reviewed_exact')
    import platform
    expected_platform = {'Darwin': 'darwin', 'Linux': 'linux'}[platform.system()]
    expected_arch = {'arm64': 'arm64', 'aarch64': 'arm64', 'x86_64': 'amd64', 'AMD64': 'amd64'}.get(platform.machine())
    require(review.get('os') == expected_platform and review.get('arch') == expected_arch, 'candidate_platform_mismatch')
    require(review.get('revision') == module.NEW and review.get('tree') == module.TREE
            and sha(binary) == review.get('binary_sha256'), 'candidate_identity_mismatch')
    for name in ('build', 'smoke', 'startup_configuration'):
        proof(review[name])
    # Verify the trusted hash before invoking even --version on supplied bytes.
    module.c.durable_copy(binary, host.candidate)
    require(sha(host.candidate) == review['binary_sha256'], 'candidate_copy_drift')
    info = host.identity(host.candidate)
    require(info['revision'] == module.NEW and info['tree'] == module.TREE, 'candidate_version_drift')
    module.save(host.build_receipt, {'identity': info, 'binary_sha256': review['binary_sha256'],
                                  'review_sha256': sha(review_path)})
    host.info = info


def adapter_types(module):
    class WindowHost(module.Host):
        def ready(self, expected, timeout=15):
            module.h.Host.ready(self, expected, timeout)
            return super().ready(expected, timeout)

        def quiescent(self):
            super().quiescent()
            require(self.old_start_observation()['status'] == 'definitely_absent', 'manager_or_pid_ambiguous')
            result = module.subprocess.run(['lsof', '-nP', '-t', '-iTCP:7443', '-sTCP:LISTEN'],
                                           capture_output=True, timeout=15)
            require(result.returncode == 1 and not result.stdout and not result.stderr, 'replication_listener_or_inspection_failed')

        def verify_backup(self):
            manifest = super().verify_backup()
            for number, item in enumerate(manifest.get('startup_dependencies', [])):
                source, backup = Path(item['path']), self.backup / 'startup-dependencies' / str(number)
                private_file(source)
                private_file(backup)
                require(sha(source) == sha(backup) == item['sha256'], 'startup_dependency_backup_drift')
            return manifest

        def build(self):
            require(self.args.candidate_review is not None, 'candidate_review_required')
            import_candidate(module, self, self.args.candidate_review)

    class WindowEngine(module.Engine):
        def state(self):
            state = super().state()
            require(state.get('mode') == 'maintenance-window', 'run_mode_mismatch')
            return state

        def mark(self, phase, **extra):
            import uuid
            return super().mark(phase, mode='maintenance-window', nonce=uuid.uuid4().hex, **extra)

        def evidence(self, kind):
            state = self.state()
            require(self.a.evidence is not None, 'evidence_required')
            private_file(self.a.evidence)
            e = module.read(self.a.evidence)
            for key, value in {'run': self.a.run, 'host': self.a.host, 'nonce': state['nonce'],
                               'phase': state['phase'], 'kind': kind, 'mode': 'maintenance-window'}.items():
                require(e.get(key) == value, 'stale_or_wrong_evidence')
            require(type(e.get('observed_ns')) is int and state['updated_ns'] <= e['observed_ns'] <= time.time_ns()
                    and time.time_ns() - e['observed_ns'] <= 300 * 10**9, 'evidence_time_invalid')
            require(e.get('operator') and e.get('passed') is True, 'evidence_not_passed')
            require(e.get('service_binding_verified') is True and e.get('service_url') == self.a.service_url,
                    'service_binding_missing')
            proof(e['route_record'])
            require(e.get('start_restores_business') is True and e.get('default_forward') is True,
                    'maintenance_policy_missing')
            for number, item in enumerate(state.get('dependency_files', [])):
                path = Path(item['path'])
                private_file(path)
                require(sha(path) == item['sha256'], 'startup_dependency_drift')
                if state['phase'] not in ('prepared', 'prestop-passed', 'backup-preparing'):
                    backup = self.host.backup / 'startup-dependencies' / str(number)
                    require(sha(backup) == item['sha256'], 'startup_dependency_backup_drift')
            # No fence booleans are read or synthesized in this mode.
            module.save(self.host.work / ('accepted-' + kind + '-' + str(time.time_ns()) + '.json'), e)
            return e

        def convergence(self, evidence):
            entries = recovery_inventory(self.host.production_db())
            result = recovery_gate(entries, evidence.get('recovery_review'))
            module.save(self.host.work / ('recovery-' + str(time.time_ns()) + '.json'),
                        {'entries': entries, 'sha256': result})
            return result

        def run(self):
            action, host = self.a.action, self.host
            if action == 'prepare':
                return super().run()
            state = self.state()
            phase = state['phase']
            if action == 'gate-prestop':
                require(phase in ('prepared', 'prestop-passed'), 'wrong_phase')
                e = self.evidence('prestop')
                require(e.get('callers_paused') is True and e.get('no_direct_file_writers') is True,
                        'writers_not_reviewed')
                proof(e['startup_dependencies'])
                require(isinstance(e.get('dependency_files'), list), 'dependency_inventory_missing')
                dependencies = []
                for item in e['dependency_files']:
                    path = Path(item['path'])
                    private_file(path)
                    require(sha(path) == item['sha256'], 'startup_dependency_drift')
                    dependencies.append({'path': str(path), 'sha256': item['sha256']})
                digest = self.convergence(e)
                self.mark('prestop-passed', recovery_sha256=digest, dependency_files=dependencies)
                return
            if action == 'stop':
                require(phase in ('prestop-passed', 'backup-preparing', 'stop-requested'), 'wrong_phase')
                e = self.evidence('stop')
                require(e.get('callers_paused') is True and e.get('no_direct_file_writers') is True,
                        'writers_not_reviewed')
                self.convergence(e)  # Immediately before asking the manager to stop.
                if phase != 'stop-requested':
                    if phase == 'backup-preparing' and host.backup.exists():
                        os.rename(host.backup, host.backup.with_name(host.backup.name + '-partial-' + str(time.time_ns())))
                        module.c.syncdir(host.backup.parent)
                    self.mark('backup-preparing')
                    host.prepare()
                    dependency_dir = host.backup / 'startup-dependencies'
                    dependency_dir.mkdir(mode=0o700)
                    for number, item in enumerate(state['dependency_files']):
                        path = Path(item['path'])
                        private_file(path)
                        require(sha(path) == item['sha256'], 'startup_dependency_drift')
                        module.c.durable_copy(path, dependency_dir / str(number))
                    module.save(dependency_dir / 'inventory.json', state['dependency_files'])
                    manifest = module.read(host.backup / 'manifest.json')
                    manifest['startup_dependencies'] = state['dependency_files']
                    module.save(host.backup / 'manifest.json', manifest)
                    host.verify_backup()
                    host.idle()
                    self.convergence(e)
                require(time.time_ns() - e['observed_ns'] <= 300 * 10**9, 'stop_evidence_expired')
                self.mark('stop-requested')
                host.stop()
                self.mark('await-drain')
                return
            if action == 'gate-drain':
                require(phase in ('await-drain', 'stop-requested'), 'wrong_phase')
                e = self.evidence('drain')
                require(e.get('normal_exit_no_timeout_no_sigkill') is True
                        and e.get('no_direct_file_writers') is True and e.get('autostart_suppressed') is True,
                        'stop_not_proven')
                proof(e['offline_record'])  # Includes both listener ports and store holders.
                with self.offline():
                    host.quiescent()
                    runtime = module.read(host.root / 'state/runtime.json')
                    require(runtime.get('exit_code') == 0 and runtime.get('stopped_at'), 'runtime_exit_not_clean')
                    self.mark('drain-passed', recovery_sha256=self.convergence(e))
                return
            if action in ('snapshot', 'gate-backup', 'gate-git'):
                # Keep fix2's final snapshot, independent restore probe and R1 checks.
                return super().run()
            if action in ('install', 'resume-new'):
                allowed = ('install-allowed',) if action == 'install' else (
                    'install-allowed', 'installing-new', 'installed-new', 'start-attempted', 'new-ready')
                require(phase in allowed, 'wrong_phase')
                e = self.evidence('install' if action == 'install' else 'resume')
                with self.offline():
                    host.quiescent()
                    manifest = host.verify_backup()
                    require(module.digest(host.backup / 'manifest.json') == state['manifest_sha256'], 'approved_backup_drift')
                    module.fsck(host.backup / 'gitrepos', module.repository_inventory(host.backup / 'db/ags.sqlite'))
                    module.c.verify_sqlite(host.production_db())
                    if not state['started_new']:
                        require(module.h.hashes(host.root / 'data/gitrepos') == manifest['git_store'], 'live_store_drift')
                    self.convergence(e)
                    self.mark('installing-new')
                    host.install()
                    self.mark('installed-new')
                return
            if action == 'start-new':
                require(phase == 'installed-new', 'wrong_phase')
                e = self.evidence('start')
                require(e.get('start_restores_business') is True, 'start_policy_missing')
                with self.offline():
                    host.quiescent()
                    host.verify_backup()
                    require(module.digest(host.backup / 'manifest.json') == state['manifest_sha256'], 'approved_backup_drift')
                    host.doctor()
                    receipt = module.read(host.build_receipt)
                    require(sha(host.root / 'bin/gh-server') == receipt['binary_sha256']
                            and host.identity() == receipt['identity'], 'installed_candidate_drift')
                    self.convergence(e)
                    self.mark('start-attempted', started_new=True, start_attempted_ns=time.time_ns())
                # Migration, seed and recovery may write before any listener exists.
                host.start()
                host.health(module.NEW)
                self.mark('new-ready', ready_ns=time.time_ns())
                return
            if action == 'read-new-start':
                require(phase in ('start-attempted', 'new-ready'), 'wrong_phase')
                self.evidence('new-readback')
                # Never start here. A timeout leaves the durable attempted marker.
                host.health(module.NEW)
                self.mark('new-ready', ready_ns=state.get('ready_ns', time.time_ns()))
                return
            if action == 'validate':
                require(phase == 'new-ready', 'wrong_phase')
                e = self.evidence('validate')
                proof(e['business_reads'])
                proof(e['provider_readback'])
                super().run()
                self.mark('observing', observation_started_ns=time.time_ns())
                return
            if action == 'observe':
                require(phase == 'observing', 'wrong_phase')
                e = self.evidence('observe')
                duration = e.get('required_observation_seconds')
                require(type(duration) is int and duration >= 600, 'observation_budget_invalid')
                require(time.time_ns() - state['observation_started_ns'] >= duration * 10**9,
                        'observation_incomplete')
                require(e.get('all_actual_leases_covered') is True and type(e.get('unknown_external_results')) is int and e['unknown_external_results'] == 0,
                        'observation_recovery_incomplete')
                proof(e['observation_record'])
                proof(e['provider_readback'])
                host.health(module.NEW)
                self.convergence(e)
                self.mark('complete')
                return
            # Old recovery stays behind the original executor's isolated-mode R2.
            raise Blocked('action_requires_separate_isolated_recovery')

    return WindowHost, WindowEngine


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--executor-dir', required=True, type=Path)
    parser.add_argument('--mode', choices=('isolated', 'maintenance-window'), default='isolated')
    known, rest = parser.parse_known_args(argv)
    module = load_executor(known.executor_dir)
    # The strict auxiliary contract applies to both modes; all other isolated
    # evidence and recovery behavior remains the original executor's implementation.
    module.fsck = lambda store, expected=None: store_fsck(module, store, expected)
    if known.mode == 'isolated':
        original_engine = module.Engine

        class IsolatedEngine(original_engine):
            def state(self):
                state = super().state()
                require(state.get('mode', 'isolated') == 'isolated', 'run_mode_mismatch')
                return state

        module.Engine = IsolatedEngine
        return module.main(rest)
    p = module.parser()
    for argument in p._actions:
        if argument.dest == 'action':
            argument.choices = ['prepare', 'gate-prestop', 'stop', 'gate-drain', 'snapshot', 'gate-backup',
                                'gate-git', 'install', 'start-new', 'read-new-start', 'validate', 'observe', 'resume-new']
    p.add_argument('--candidate-review', type=Path)
    a = p.parse_args(rest)
    # Reuse all original argument/path/Service admission before constructing Host.
    if not a.execute:
        print(json.dumps({'mode': known.mode, 'action': a.action, 'automatic_downgrade': False}))
        return
    require(a.run.startswith('ags-') and all(c.isalnum() or c == '-' for c in a.run), 'invalid_run')
    from urllib.parse import urlsplit
    url = urlsplit(a.service_url)
    require(url.scheme == 'https' and url.hostname and url.hostname.endswith('.ts.net')
            and url.username is None and url.password is None and url.path in ('', '/')
            and not url.query and not url.fragment, 'service_url_required')
    os.umask(0o077)
    Host, Engine = adapter_types(module)
    module.fsck = lambda store, expected=None: store_fsck(module, store, expected)
    host = Host(a)
    require(a.backup == host.backup, 'backup_binding_failed')
    with (host.root / 'state/hostops.lock').open('a+') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        engine = Engine(host, a)
        try:
            engine.run()
        except BaseException:
            if host.work.exists():
                module.save(host.work / ('failure-' + str(time.time_ns()) + '.json'),
                            {'action': a.action, 'preserve_scene': True, 'default_forward': True,
                             'no_automatic_start_stop_restore': True})
            raise
    print(json.dumps({'status': 'stage-complete', 'phase': engine.state()['phase']}))


if __name__ == '__main__':
    try:
        main()
    except Exception:
        # External exceptions can contain private paths, payloads or command output.
        print(json.dumps({'status': 'blocked-preserve-scene', 'detail': 'inspect private run evidence'}), file=sys.stderr)
        sys.exit(1)
