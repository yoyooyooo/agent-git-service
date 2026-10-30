"""Internal transaction implementation, not a guaranteed entry.

Durable local transaction state. The fixed host adapter supplies authorized effects.
Callers reach this module through scripts/upgrade, which starts Python with
bytecode writes disabled. Direct "python3 scripts/upgrade_engine.py" is not covered.
"""
import sys
sys.dont_write_bytecode = True
sys.pycache_prefix = None
from datetime import datetime, timezone
import fcntl
import hashlib
import json
import os
from pathlib import Path
import signal
import sys
import socket
import sqlite3
import subprocess
import tempfile
import time
import re
import urllib.request

from upgrade import Failure, private_json, require_apply_contracts

PRE_EFFECT = {'admission', 'source_verify', 'release_resolve', 'artifact_download',
              'artifact_verify', 'candidate_identity', 'candidate_smoke', 'old_smoke',
              'upgrade_smoke', 'schema_compare', 'legacy_scan'}
FLAGS = ('prepare_started', 'stop_started', 'install_started', 'backup_created')


def save(path, value):
    temporary = path.with_name(path.name + '.new')
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, 'w') as stream:
        json.dump(value, stream, sort_keys=True)
        stream.write('\n'); stream.flush(); os.fsync(stream.fileno())
    os.replace(temporary, path)
    fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def controlled_directory(path):
    if path.resolve() != path or path.is_symlink():
        raise Failure('journal_invalid')
    path.mkdir(mode=0o700, exist_ok=True)
    st = path.stat()
    if st.st_uid != os.getuid() or st.st_mode & 0o077:
        raise Failure('journal_invalid')


def authorize(record, binding, now=None):
    now = now or datetime.now(timezone.utc)
    try:
        if (record['schema'] != 'ags.local-upgrade-authorization.v2'
                or record['decision'] != 'approve' or record['binding'] != binding
                or set(record['actor']) != {'type', 'id', 'source'}
                or not all(isinstance(v, str) and 0 < len(v) <= 256 for v in record['actor'].values())
                or record['rollback_mode'] != 'forward-only'
                or not isinstance(record['evidence_sha256'], str) or not re.fullmatch(r'[0-9a-f]{64}', record['evidence_sha256'])):
            raise Failure('authorization_invalid')
        issued = datetime.fromisoformat(record['issued_at'])
        expires = datetime.fromisoformat(record['expires_at'])
        if issued.tzinfo is None or expires.tzinfo is None:
            raise Failure('authorization_invalid')
        if issued > now or expires <= now or expires <= issued:
            raise Failure('authorization_expired')
    except (KeyError, TypeError, ValueError):
        raise Failure('authorization_invalid') from None


def migration_admission(old, new, migrations, registry):
    """Three exact columns; no old/new-only or subset migration permission."""
    if (not isinstance(migrations, list) or len(set(migrations)) != len(migrations)
            or migrations != sorted(migrations)):
        raise Failure('migration_admission_rejected', 'schema_compare')
    matches = [row for row in registry if row == {'old': old, 'new': new, 'migrations': migrations}]
    if len(matches) != 1:
        raise Failure('migration_admission_rejected', 'schema_compare')
    return matches[0]


def child(argv, phase, *, env=None, cwd=None, timeout=120, effects=False):
    env = os.environ.copy() if env is None else dict(env)
    env['PYTHONDONTWRITEBYTECODE'] = '1'
    env.pop('PYTHONPYCACHEPREFIX', None)
    command = list(argv)
    if command and (command[0] == sys.executable
                    or re.fullmatch(r'python(?:3(?:\.\d+)?)?', Path(command[0]).name)):
        command.insert(1, '-B')
    try:
        result = subprocess.run(command, env=env, cwd=cwd, capture_output=True, timeout=timeout)
    except FileNotFoundError:
        raise Failure('dependency_unavailable', phase) from None
    except subprocess.TimeoutExpired:
        # subprocess.run kills and waits for its direct child. Host effect commands
        # must not daemonize. No claim is released based on this code alone.
        raise Failure('child_timeout', phase, 82 if effects else 69) from None
    except OSError:
        raise Failure('child_failed', phase, 82 if effects else 69) from None
    if result.returncode:
        raise Failure('child_failed', phase, 82 if effects else 69)
    return result.stdout


def database_snapshot(path):
    with sqlite3.connect(path.as_uri() + '?mode=ro', uri=True) as database:
        if database.execute('PRAGMA integrity_check').fetchall() != [('ok',)]:
            raise Failure('schema_not_additive', 'schema_compare')
        tables = {}
        for (table,) in database.execute("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'"):
            quoted = '"' + table.replace('"', '""') + '"'
            columns = database.execute('PRAGMA table_info(' + quoted + ')').fetchall()
            rows = database.execute('SELECT * FROM ' + quoted).fetchall()
            indexes = database.execute('PRAGMA index_list(' + quoted + ')').fetchall()
            index_shapes = []
            for index in indexes:
                index_name = '"' + index[1].replace('"', '""') + '"'
                index_shapes.append((index[1:], database.execute('PRAGMA index_xinfo(' + index_name + ')').fetchall()))
            foreign_keys = database.execute('PRAGMA foreign_key_list(' + quoted + ')').fetchall()
            triggers = database.execute("SELECT name,sql FROM sqlite_master WHERE type='trigger' AND tbl_name=? ORDER BY name", (table,)).fetchall()
            tables[table] = {'columns': columns, 'rows': rows, 'indexes': index_shapes,
                             'foreign_keys': foreign_keys, 'triggers': triggers}
        return tables


def schema_changes(old, new):
    migrations = []
    for table, before in old.items():
        after = new.get(table)
        if after is None:
            raise Failure('schema_not_additive', 'schema_compare')
        for attribute in ('indexes', 'foreign_keys', 'triggers'):
            if before.get(attribute) != after.get(attribute):
                raise Failure('schema_not_additive', 'schema_compare')
        columns = {c[1]: c[2:] for c in after['columns']}
        for col in before['columns']:
            if columns.get(col[1]) != col[2:]:
                raise Failure('schema_not_additive', 'schema_compare')
        old_names = [c[1] for c in before['columns']]
        positions = [[c[1] for c in after['columns']].index(name) for name in old_names]
        projected = [tuple(row[i] for i in positions) for row in after['rows']]
        if sorted(projected, key=repr) != sorted(before['rows'], key=repr):
            raise Failure('schema_not_additive', 'schema_compare')
        for col in after['columns']:
            if col[1] not in old_names:
                migrations.append('column:' + table + '.' + col[1] + ':' + hashlib.sha256(repr(col[2:]).encode()).hexdigest())
    for table in set(new) - set(old):
        migrations.append('table:' + table + ':' + hashlib.sha256(repr(new[table]['columns']).encode()).hexdigest())
    return sorted(migrations)


def smoke(binary, revision, directory, *, database=None, timeout=60, _namespace=False):
    """Only isolated scratch DB/HOME/Git; never inherit runtime.env or .env."""
    if sys.platform.startswith('linux') and not _namespace:
        # The fixed helper brings up only loopback in a new user/network namespace.
        helper = Path(__file__).with_name('upgrade_smoke.py')
        command = ['/usr/bin/unshare', '--user', '--map-root-user', '--net', sys.executable, '-B',
                   str(helper), str(binary), revision, str(directory), str(timeout)]
        if database is not None:
            command.append(str(database))
        child(command, 'candidate_smoke', env={'PATH': '/usr/bin:/bin', 'PYTHONDONTWRITEBYTECODE': '1'},
              timeout=timeout + 15)
        return directory / 'smoke.sqlite'
    directory.mkdir(mode=0o700)
    db = directory / 'smoke.sqlite'
    if database is not None:
        with sqlite3.connect(database.as_uri() + '?mode=ro', uri=True) as source, sqlite3.connect(db) as target:
            source.backup(target)
    for name in ('home', 'tmp', 'git'):
        (directory / name).mkdir(mode=0o700)
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0)); port = sock.getsockname()[1]
    env = {'PATH': '/usr/bin:/bin', 'HOME': str(directory / 'home'), 'TMPDIR': str(directory / 'tmp'),
           'DB_DSN': 'file:' + str(db), 'GIT_REPO_DIR': str(directory / 'git'),
           'PORT': str(port), 'BASE_URL': 'http://127.0.0.1:' + str(port),
           'LISTEN_MODE': 'production', 'ENVIRONMENT': 'production', 'ENABLE_WORKFLOW_EXEC': '0'}
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with (directory / 'output.log').open('wb') as output:
        proc = subprocess.Popen([str(binary)], cwd=directory, env=env, stdout=output, stderr=output,
                                start_new_session=True)
        try:
            deadline = time.monotonic() + timeout
            while time.monotonic() < deadline:
                if proc.poll() is not None:
                    raise Failure('child_failed', 'candidate_smoke', 69)
                try:
                    with opener.open(env['BASE_URL'] + '/readyz', timeout=1) as response:
                        body = json.load(response)
                    if body.get('status') == 'ready' and body.get('version') == revision:
                        break
                except (OSError, ValueError):
                    pass
                time.sleep(.05)
            else:
                raise Failure('child_timeout', 'candidate_smoke', 69)
        finally:
            if proc.poll() is None:
                os.killpg(proc.pid, signal.SIGTERM)
            try:
                proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                os.killpg(proc.pid, signal.SIGKILL); proc.wait()
                raise Failure('child_timeout', 'candidate_smoke', 69) from None
        if proc.returncode != 0:
            raise Failure('child_failed', 'candidate_smoke', 69)
    database_snapshot(db)
    return db


def dual_smoke(old_binary, new_binary, old, new, registry, *, runner=smoke):
    with tempfile.TemporaryDirectory(prefix='ags-upgrade-smoke-') as scratch:
        base = Path(scratch).resolve()
        old_db = runner(old_binary, old, base / 'old')
        before = database_snapshot(old_db)
        fresh_db = runner(new_binary, new, base / 'new')
        upgrade_db = runner(new_binary, new, base / 'populated', database=old_db)
        upgraded = database_snapshot(upgrade_db)
        migrations = schema_changes(before, upgraded)
        # A fresh empty affected table cannot establish data-preserving upgrade.
        for migration in migrations:
            if migration.startswith('column:'):
                table = migration.split(':')[1].split('.')[0]
                if not before[table]['rows']:
                    raise Failure('requires_manual_data_or_runtime_review', 'upgrade_smoke', 78)
        fresh = database_snapshot(fresh_db)
        if {t: {k: x for k, x in v.items() if k != 'rows'} for t, v in fresh.items()} != {t: {k: x for k, x in v.items() if k != 'rows'} for t, v in upgraded.items()}:
            raise Failure('schema_not_additive', 'schema_compare')
        migration_admission(old, new, migrations, registry)
        return migrations


class Transaction:
    def __init__(self, state, binding, authorization, effects):
        self.state, self.binding, self.authorization, self.effects = state, binding, authorization, effects
        self.pending = state / 'pending'
        self.journal = None

    def checkpoint(self, phase, **fields):
        self.journal.update(phase=phase, **fields)
        save(self.pending / 'journal.json', self.journal)

    def run(self):
        # Authorization is rechecked here, before claim, backup or service calls.
        authorize(self.authorization, self.binding)
        require_apply_contracts(self.binding.get('host'))
        controlled_directory(self.state)
        fd = os.open(self.state / 'hostops.lock', os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, 'w') as lock:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                raise Failure('lock_held') from None
            if self.pending.exists():
                try:
                    claim, _ = private_json(self.pending / 'claim.json')
                except Failure:
                    raise Failure('transaction_state_unknown') from None
                if claim != {'schema': 'ags.local-claim.v2', 'binding': self.binding}:
                    raise Failure('claim_mismatch')
                try:
                    self.journal, _ = private_json(self.pending / 'journal.json')
                except Failure:
                    raise Failure('journal_invalid') from None
                if self.journal.get('binding') != self.binding or any(type(self.journal.get(k)) is not bool for k in FLAGS):
                    raise Failure('journal_invalid')
                if self.journal.get('phase') == 'awaiting_observation':
                    self.effects.health()
                    result, _ = private_json(self.pending / 'receipt.json')
                    return result
                # Crashes and pre-effect failures preserve the claim. No inferred
                # safety or automatic replay from a timeout/code/name.
                raise Failure('forward_only_recovery_required' if self.journal['stop_started']
                              or self.journal['install_started'] else 'transaction_state_unknown',
                              self.journal.get('phase', 'unknown'))
            self.pending.mkdir(mode=0o700)
            # fsync mkdir before claim. A crash between mkdir and claim is unknown.
            save(self.pending / 'claim.json', {'schema': 'ags.local-claim.v2', 'binding': self.binding})
            self.journal = {'schema': 'ags.local-upgrade-journal.v2', 'binding': self.binding,
                            'phase': 'admission', **dict.fromkeys(FLAGS, False)}
            self.checkpoint('admission')
            try:
                self.effects.preflight()
                authorize(self.authorization, self.binding)
                self.checkpoint('prepare', prepare_started=True)
                self.effects.backup()
                self.checkpoint('prepare', backup_created=True)
                self.checkpoint('stop', stop_started=True)
                self.effects.stop()
                self.checkpoint('install', install_started=True)
                self.effects.install()
                self.checkpoint('start')
                self.effects.start()
                self.checkpoint('health')
                self.effects.health()
                result = {'schema': 'ags.local-upgrade-receipt.v2', 'binding': self.binding,
                          'status': 'awaiting_observation', 'rollback_mode': 'forward-only',
                          'authorization_id': self.authorization['authorization_id'],
                          'authorization_sha256': hashlib.sha256(json.dumps(self.authorization, sort_keys=True).encode()).hexdigest()}
                save(self.pending / 'receipt.json', result)
                self.checkpoint('awaiting_observation')
                return result
            except Exception as error:
                self.checkpoint(self.journal['phase'], failed=True)
                if self.journal['stop_started'] or self.journal['install_started']:
                    raise Failure('forward_only_recovery_required', self.journal['phase']) from error
                raise
