#!/usr/bin/env python3
"""Read-only, bounded inventory components; deliberately no CLI/apply integration."""
from __future__ import annotations

import base64
import math
from contextlib import contextmanager
import fcntl
import hashlib
import os
from pathlib import Path
import re
import shlex
import sqlite3
import stat
import subprocess
from urllib.parse import parse_qsl, unquote, urlsplit

from upgrade_backup_contract import (ContractError, MAX_ENTRIES, OID, absolute, canonical, digest,
                                     fingerprint, integer, relative, require, strict_json, validate_group)

# Review alongside GitMaintenanceRoots, including the cross-fork head query.
ROOT_QUERIES = (
    ('pull_requests', 'repository_id = ?', 1, ('head_sha', 'base_sha', 'merge_commit_sha', 'auto_merge_expected_head_sha')),
    ('pull_requests', 'head_repository_id = ? AND repository_id <> ?', 2, ('head_sha',)),
    ('pull_request_reviews', 'pull_request_id IN (SELECT id FROM pull_requests WHERE repository_id = ?)', 1, ('commit_sha',)),
    ('pr_review_comments', 'pull_request_id IN (SELECT id FROM pull_requests WHERE repository_id = ?)', 1, ('commit_id',)),
    ('commit_statuses', 'repository_id = ?', 1, ('commit_sha',)),
    ('pages_builds', 'repository_id = ?', 1, ('commit_sha',)),
    ('repo_flow_env_projections', 'repository_id = ?', 1, ('source_sha',)),
    ('projection_events', 'repository_id = ?', 1, ('ags_sha',)),
    ('projection_ref_states', 'repository_id = ?', 1, ('ags_sha',)),
    ('pull_request_projections', 'repository_id = ?', 1, ('last_synced_sha',)),
    ('access_grant_invocations', 'repository_id = ?', 1, ('expected_head_sha', 'expected_base_sha')),
    ('delegated_agent_sessions', 'repository_id = ?', 1, ('merge_delegation_expected_head_sha', 'merge_delegation_expected_base_sha')),
    ('workflow_runs', 'repository_id = ?', 1, ('head_sha',)),
    ('pull_request_projection_jobs', 'repository_id = ?', 1, ('head_sha', 'preflight_ags_head_sha', 'preflight_base_sha', 'desired_ags_head_sha')),
    ('pull_request_action_intents', 'repository_id = ?', 1, ('expected_head_sha', 'expected_base_sha', 'result_sha')),
)
CONSTRAINT_COLUMNS = (('access_grant_invocations', 'constraints_json'),
                      ('delegated_agent_sessions', 'operation_constraints'))
CONSTRAINT_KEYS = ('head_sha', 'base_sha', 'expected_head_sha', 'expected_base_sha', 'exact_head',
                   'commit_sha', 'sha', 'ref')
EXTERNAL_COLUMNS = {
    'projection_events': ('external_sha',), 'projection_ref_states': ('external_sha',),
    'access_grant_invocations': ('provider_merge_sha',),
    'pull_request_projection_jobs': ('expected_forgejo_old_head_sha', 'observed_forgejo_head_sha', 'remote_sha'),
    'pull_request_projection_job_attempts': ('remote_sha',),
    'outbound_deliveries': ('merge_sha',),
}
# All rows are evidence, including terminal/audit rows and future retries. S1
# intentionally does not issue a startup-execute disposition or provider approval.
RECOVERY_TABLES = frozenset(('pull_request_projection_jobs', 'pull_requests', 'pull_request_action_intents',
    'authority_boundary_receipts', 'access_grant_invocations', 'outbound_deliveries', 'repositories',
    'wiki_search_projection_tasks', 'wiki_pages', 'wiki_search_documents', 'wiki_repo_heads',
    'wiki_page_labels', 'labels'))
WIKI_TABLES = frozenset(('wiki_page_revisions', 'wiki_changesets', 'wiki_git_repair_obligations',
    'wiki_blob_refs', 'wiki_pending_blobs', 'wiki_page_index', 'wiki_index_state', 'wiki_page_history'))
WIKI_COLUMNS = {
    'wiki_changesets': {'changeset_id', 'repository_id', 'synth_commit_sha', 'synth_format_ver', 'source'},
    'wiki_pages': {'page_id', 'repository_id', 'head_blob_sha', 'body_size', 'body_inline',
                   'head_revision_id', 'head_changeset_id', 'deleted_at'},
    'wiki_page_revisions': {'page_id', 'revision_id', 'changeset_id', 'blob_sha', 'body_size',
                            'body_inline', 'commit_sha', 'op'},
    'wiki_blob_refs': {'blob_sha', 'size'}, 'wiki_pending_blobs': {'blob_sha', 'size'},
    'wiki_page_index': {'repository_id', 'head_commit_sha', 'head_blob_sha'},
    'wiki_index_state': {'repository_id', 'indexed_commit_sha', 'backlinks_indexed_sha'},
    'wiki_page_history': {'repository_id', 'commit_sha', 'parent_commit_sha'},
    'wiki_git_repair_obligations': {'repository_id', 'head_sha'},
    'wiki_search_documents': {'repository_id', 'revision_sha'},
    'wiki_search_projection_tasks': {'repository_id', 'revision_sha'},
}
KNOWN_TABLES = frozenset('''users organization_members organization_invitations outside_collaborators
agent_bindings agent_invites teams team_members team_repositories repositories issue_pr_number_counters
repo_redirects tokens execution_context_snapshots access_grants access_grant_invocations
delegated_agent_sessions team_authority_epochs principal_binding_revocations user_identities device_codes
device_code_audit_logs authorization_codes labels deploy_keys milestones issues issue_comments issue_events
issue_references pages_configs pages_builds audit_log_entries pull_requests pull_request_multica_links
pull_request_projections pull_request_projection_jobs pull_request_projection_job_attempts
pull_request_action_intents authority_boundary_receipts outbound_deliveries projection_events
projection_ref_states repo_flow_env_projections linked_branches releases release_assets variables secrets
environments ssh_keys gpg_keys ssh_signing_keys projects project_fields project_items project_repo_links
workflows workflow_runs workflow_run_jobs ci_resources client_run_sessions client_run_links artifacts
action_caches rulesets review_requests pull_request_reviews pr_review_comments autolinks stars gists
commit_statuses webhooks hook_deliveries deployments deployment_statuses branch_protections dependabot_alerts
repository_invitations collaborators reactions notifications external_events repo_incidents wiki_page_labels
wiki_page_index wiki_index_state wiki_backlinks wiki_page_history wiki_search_documents
wiki_search_projection_tasks wiki_pages wiki_page_revisions wiki_changesets wiki_repo_heads
wiki_git_repair_obligations wiki_compaction_jobs wiki_page_links wiki_blob_refs wiki_pending_blobs'''.split())


def safe_path(value, kind=None):
    path = absolute(str(value))
    for component in (*reversed(path.parents), path):
        require(not component.is_symlink(), 'symlink_forbidden')
    info = path.lstat()
    require(stat.S_ISREG(info.st_mode) or stat.S_ISDIR(info.st_mode), 'file_type_unsupported')
    if kind:
        require(stat.S_ISDIR(info.st_mode) == (kind == 'directory'), 'file_type_invalid')
    if stat.S_ISREG(info.st_mode):
        require(info.st_nlink == 1, 'hardlink_forbidden')
    return path


def sqlite_path(dsn):
    """Native dispatch accepts file:/sqlite:; this slice requires an absolute local DB."""
    require(type(dsn) is str and dsn == dsn.strip() and '\x00' not in dsn, 'dsn_invalid')
    if dsn.startswith('sqlite://'):
        raw = dsn[len('sqlite://'):]
    elif dsn.startswith('sqlite:'):
        raw = dsn[len('sqlite:'):]
    elif dsn.startswith('file:'):
        parsed = urlsplit(dsn)
        require(not parsed.netloc and not parsed.fragment, 'dsn_invalid')
        raw = parsed.path + ('?' + parsed.query if parsed.query else '')
    else:
        raise ContractError('backend_unsupported')
    require('#' not in raw and raw.count('?') <= 1, 'dsn_invalid')
    path, _, query = raw.partition('?')
    try:
        options = parse_qsl(query, keep_blank_values=True, strict_parsing=True) if query else []
    except ValueError:
        raise ContractError('dsn_invalid') from None
    require(len({k for k, _ in options}) == len(options), 'dsn_invalid')
    # No immutable, memory, custom VFS or options that can conceal WAL/recovery.
    require(all((k == 'mode' and v in ('ro', 'rw', 'rwc'))
                or (k == 'cache' and v == 'private')
                or (k in ('_foreign_keys', '_fk') and v in ('1', 'true', 'on'))
                for k, v in options), 'dsn_option_unsupported')
    try:
        decoded = unquote(path, errors='strict')
    except UnicodeError:
        raise ContractError('dsn_invalid') from None
    require('%' not in decoded and decoded != ':memory:', 'dsn_invalid')
    return absolute(decoded)


@contextmanager
def readonly(path):
    safe_path(path, 'file')
    require(not Path(str(path) + '-journal').exists(), 'sqlite_journal_unsupported')
    # mode=ro, not immutable: SQLite must see any committed WAL content.
    db = sqlite3.connect(path.as_uri() + '?mode=ro', uri=True, timeout=5)
    db.setlimit(sqlite3.SQLITE_LIMIT_LENGTH, 16 * 1024 * 1024)
    db.text_factory = bytes
    db.row_factory = sqlite3.Row
    try:
        db.execute('PRAGMA query_only=ON')
        db.execute('BEGIN')
        yield db
    except sqlite3.Error:
        raise ContractError('sqlite_read_failed') from None
    finally:
        db.close()


def sqlname(value):
    require(type(value) is str and re.fullmatch(r'[A-Za-z_][A-Za-z0-9_]*', value) is not None,
            'schema_identifier_invalid')
    return '"' + value + '"'


def decode(value):
    if isinstance(value, bytes):
        try:
            return value.decode('utf-8')
        except UnicodeError:
            raise ContractError('typed_text_invalid') from None
    return value


def cell(value):
    if value is None:
        return ['null']
    if type(value) is bytes:
        return ['bytes', base64.b64encode(value).decode('ascii')]
    if type(value) is float:
        require(math.isfinite(value), 'sqlite_value_unsupported')
        return ['float', value.hex()]
    require(type(value) is int, 'sqlite_value_unsupported')
    return ['int', value]


def schema_description(db):
    return {'objects': [[decode(v) for v in row] for row in db.execute(
        "SELECT type,name,tbl_name,sql FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type,name")],
            'user_version': db.execute('PRAGMA user_version').fetchone()[0],
            'application_id': db.execute('PRAGMA application_id').fetchone()[0]}


def schema_digest(path):
    """Discovery helper only; its result is not schema admission/authorization."""
    with readonly(path) as db:
        return fingerprint(schema_description(db))


def rows(db, table, limit):
    result = db.execute('SELECT * FROM ' + sqlname(table)).fetchmany(limit + 1)
    require(len(result) <= limit, 'inventory_budget')
    return result


def oid(value, *, symbolic=False):
    value = decode(value)
    if value is None or value == '' or value == '0' * 40:
        return None
    if symbolic and isinstance(value, str) and not OID.fullmatch(value):
        # Ref names have separate Git reachability, but malformed hex is not a ref.
        require(not re.fullmatch(r'[0-9a-fA-F]{40}', value), 'oid_invalid')
        return None
    require(type(value) is str and OID.fullmatch(value) is not None, 'oid_invalid')
    return value


def git(repo, *args, input=None):
    env = {'PATH': '/usr/bin:/bin:/usr/sbin:/sbin', 'HOME': '/nonexistent',
           'GIT_CONFIG_NOSYSTEM': '1', 'GIT_CONFIG_GLOBAL': '/dev/null', 'GIT_CONFIG_COUNT': '0',
           'GIT_OPTIONAL_LOCKS': '0', 'GIT_NO_REPLACE_OBJECTS': '1', 'GIT_TERMINAL_PROMPT': '0',
           'GIT_NO_LAZY_FETCH': '1', 'GIT_ALLOW_PROTOCOL': ''}
    command = ['git', '--git-dir=' + str(repo), '-c', 'core.hooksPath=/dev/null',
               '-c', 'gc.auto=0', '-c', 'maintenance.auto=false', *args]
    try:
        result = subprocess.run(command, env=env, input=input, capture_output=True, timeout=120)
        require(result.returncode == 0 and len(result.stdout) <= 16 * 1024 * 1024,
                'git_validation_failed')
        return result.stdout
    except (OSError, subprocess.TimeoutExpired):
        raise ContractError('git_unavailable_or_timeout') from None


def check_repo(repo):
    for marker in ('objects/info/alternates', 'objects/info/http-alternates', 'shallow'):
        require(not (repo / marker).exists(), 'git_external_objects_unsupported')
    require(not list((repo / 'objects').rglob('*.promisor')), 'git_promisor_unsupported')
    require(git(repo, 'rev-parse', '--is-bare-repository').strip() == b'true', 'git_not_bare')
    config = git(repo, 'config', '--no-includes', '--local', '--list').lower().splitlines()
    require(all(not line.startswith((b'include.', b'includeif.', b'extensions.', b'core.worktree=',
                                    b'core.repositoryformatversion=1', b'core.alternate', b'fsck.'))
                and b'.promisor=' not in line and b'.partialclonefilter=' not in line
                and not line.startswith((b'core.fsmonitor=', b'core.sshcommand=', b'core.attributesfile='))
                for line in config), 'git_config_unsupported')
    git(repo, 'fsck', '--full', '--strict')
    return {'refs_sha256': hashlib.sha256(git(repo, 'for-each-ref', '--format=%(refname) %(objectname)')).hexdigest(),
            'head_sha256': hashlib.sha256((repo / 'HEAD').read_bytes()).hexdigest()}


def check_oid(repo, value, kind):
    require(repo is not None, 'wiki_repository_missing')
    require(git(repo, 'cat-file', '-t', value).strip() == kind.encode(), 'git_object_type_invalid')


def git_blob(body):
    return hashlib.sha1(b'blob ' + str(len(body)).encode() + b'\0' + body).hexdigest()


def require_wiki_body(blob_sha, size, body, store):
    """Inline bodies and CAS objects share one check; delete revisions do not."""
    value = oid(blob_sha)
    require(value is not None, 'wiki_blob_missing')
    integer(size, 0, 64 * 1024 * 1024)
    if size <= 4096:
        require(type(body) is bytes and len(body) == size and git_blob(body) == value, 'wiki_inline_invalid')
    else:
        require(store['cas'].get(value) == size, 'wiki_cas_missing')


def file_digest(path):
    h = hashlib.sha256()
    with path.open('rb') as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b''):
            h.update(block)
    return h.hexdigest()


def scan_tree(root, limit):
    """All paths including empty directories; no following links or executing hooks."""
    result = []
    total = 0
    stack = [root]
    while stack:
        path = stack.pop()
        safe_path(path)
        info = path.lstat()
        require(len(result) < limit, 'inventory_budget')
        name = str(path.relative_to(root))
        if path.is_dir():
            result.append([name, 'directory', stat.S_IMODE(info.st_mode)])
            stack.extend(sorted(path.iterdir(), reverse=True))
        else:
            require(info.st_size <= 1 << 40, 'file_budget')
            total += info.st_size
            result.append([name, 'file', info.st_size, file_digest(path), stat.S_IMODE(info.st_mode)])
    return result, total


def store_inventory(root, repositories, limit):
    safe_path(root, 'directory')
    files, size = scan_tree(root, limit)
    expected, optional = set(), set()
    for row in repositories:
        name = decode(row['full_name'])
        require(type(name) is str and len(name.split('/')) == 2, 'repository_name_invalid')
        relative(name)
        require(all(re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]*', p) for p in name.split('/')),
                'repository_name_invalid')
        require(name + '.git' not in expected, 'duplicate_repository')
        expected.add(name + '.git'); optional.add(name + '.wiki.git')
    require(not expected & optional, 'repository_wiki_name_collision')
    actual, auxiliary, cas = set(), [], {}
    allowed_owners = {name.split('/')[0] for name in expected}
    for child in sorted(root.iterdir()):
        if child.name == '.wikiblobs':
            require(child.is_dir(), 'cas_layout_invalid')
            for path in child.rglob('*'):
                rel = path.relative_to(child).parts
                if path.is_dir():
                    require(len(rel) <= 2 and all(re.fullmatch('[0-9a-f]{2}', p) for p in rel),
                            'cas_layout_invalid')
                else:
                    require(len(rel) == 3 and OID.fullmatch(rel[2]) is not None
                            and rel[:2] == (rel[2][:2], rel[2][2:4]), 'cas_layout_invalid')
                    require(path.stat().st_size <= 64 * 1024 * 1024, 'cas_budget')
                    body = path.read_bytes()
                    require(len(body) <= 64 * 1024 * 1024 and git_blob(body) == rel[2], 'cas_digest_invalid')
                    cas[rel[2]] = len(body)
            auxiliary.append(child.name)
        elif child.name == '.github-app-deliveries':
            require(child.is_dir() and all(p.is_file() and re.fullmatch(r'[0-9a-f]{64}\.json', p.name)
                                         for p in child.iterdir()), 'delivery_spool_invalid')
            auxiliary.append(child.name)
        elif child.name == '.ags-artifact-retirement':
            require(child.is_dir() and child.stat().st_uid == os.getuid()
                    and child.stat().st_mode & 0o077 == 0, 'retirement_directory_invalid')
            require({p.name for p in child.iterdir()} == {'operation.lock'}, 'retirement_pending_or_unknown')
            lock = child / 'operation.lock'
            require(lock.is_file() and lock.stat().st_size == 0 and lock.stat().st_uid == os.getuid()
                    and lock.stat().st_mode & 0o077 == 0, 'retirement_lock_invalid')
            with lock.open('rb') as stream:
                try:
                    fcntl.flock(stream, fcntl.LOCK_EX | fcntl.LOCK_NB)
                except BlockingIOError:
                    raise ContractError('retirement_lock_busy') from None
            auxiliary.append(child.name)
        else:
            require(child.is_dir() and child.name in allowed_owners, 'unknown_store_directory')
            for repo in child.iterdir():
                name = str(repo.relative_to(root))
                require(repo.is_dir() and name in expected | optional, 'unknown_repository')
                actual.add(name)
    require(expected <= actual, 'repository_missing')
    identities = {name: check_repo(root / name) for name in sorted(actual)}
    return {'files_sha256': fingerprint(files), 'bytes': size, 'repositories': identities,
            'auxiliary': auxiliary, 'cas': cas}, actual


def database_inventory(path, root, reviewed_schema_sha256, limit=MAX_ENTRIES):
    integer(limit, 1, MAX_ENTRIES); digest(reviewed_schema_sha256)
    with readonly(path) as db:
        schema = schema_description(db)
        require(fingerprint(schema) == reviewed_schema_sha256, 'schema_not_admitted')
        tables = {decode(row[0]) for row in db.execute("SELECT name FROM sqlite_master WHERE type='table'")}
        require(tables <= KNOWN_TABLES | {'sqlite_sequence'}, 'unknown_table')
        required = RECOVERY_TABLES | WIKI_TABLES | {q[0] for q in ROOT_QUERIES} | set(EXTERNAL_COLUMNS) | {'deployments'}
        require(required <= tables, 'required_table_missing')
        integrity = db.execute('PRAGMA integrity_check').fetchmany(2)
        require(len(integrity) == 1 and integrity[0][0] == b'ok', 'sqlite_integrity_failed')
        require(not db.execute('PRAGMA foreign_key_check').fetchone(), 'sqlite_fk_failed')
        all_rows, row_digests, count, byte_count = {}, {}, 0, 0
        for table in sorted(tables):
            data = rows(db, table, limit)
            count += len(data)
            require(count <= limit, 'inventory_budget')
            types = [decode(r[1]) for r in db.execute('PRAGMA table_info(' + sqlname(table) + ')')]
            typed = db.execute('SELECT ' + ','.join('typeof(' + sqlname(col) + '),' + sqlname(col)
                                                      for col in types)
                               + ' FROM ' + sqlname(table)).fetchmany(limit + 1)
            require(len(typed) == len(data), 'sqlite_rows_changed')
            encoded = sorted(canonical([[decode(row[i]), cell(row[i + 1])]
                                        for i in range(0, len(row), 2)]) for row in typed)
            byte_count += sum(len(v) for v in encoded)
            require(byte_count <= 64 * 1024 * 1024, 'inventory_budget')
            row_digests[table] = {'rows': len(data), 'sha256': hashlib.sha256(b''.join(encoded)).hexdigest()}
            all_rows[table] = data
        # These columns define the accepted semantic mapping even for empty tables.
        required_columns = {'repositories': {'id', 'full_name'}, 'deployments': {'repository_id', 'ref'}}
        for table, where, _, columns in ROOT_QUERIES:
            required_columns.setdefault(table, set()).update(columns)
            required_columns[table].add('pull_request_id' if 'pull_request_id IN' in where else 'repository_id')
            if 'head_repository_id' in where:
                required_columns[table].add('head_repository_id')
        for table, column in CONSTRAINT_COLUMNS:
            required_columns.setdefault(table, set()).add(column)
        for table, columns in EXTERNAL_COLUMNS.items():
            required_columns.setdefault(table, set()).update(columns)
        for table, columns in WIKI_COLUMNS.items():
            required_columns.setdefault(table, set()).update(columns)
        for table, columns in required_columns.items():
            observed = {decode(row[1]) for row in db.execute('PRAGMA table_info(' + sqlname(table) + ')')}
            require(columns <= observed, 'required_column_missing')
        store, actual = store_inventory(root, all_rows['repositories'], limit)
        repos = {}
        for row in all_rows['repositories']:
            require(type(row['id']) is int and row['id'] > 0 and row['id'] not in repos, 'repository_id_invalid')
            repos[row['id']] = decode(row['full_name'])
        local, external, synthetic = set(), set(), set()
        checked = set()

        def local_oid(repo_id, value, kind='commit', wiki=False):
            value = oid(value)
            if value is None:
                return
            require(repo_id in repos, 'oid_repository_missing')
            name = repos[repo_id] + ('.wiki.git' if wiki else '.git')
            require(name in actual, 'oid_repository_missing')
            if (name, value, kind) not in checked:
                check_oid(root / name, value, kind)
                checked.add((name, value, kind))
            local.add((repo_id, name, kind, value))
            require(len(local) <= limit, 'inventory_budget')

        for repo_id in repos:
            for table, where, arity, columns in ROOT_QUERIES:
                for column in columns:
                    query = ('SELECT DISTINCT ' + sqlname(column) + ' FROM ' + sqlname(table)
                             + ' WHERE ' + where + ' LIMIT ?')
                    values = db.execute(query, (*([repo_id] * arity), limit + 1)).fetchall()
                    require(len(values) <= limit, 'inventory_budget')
                    for row in values:
                        local_oid(repo_id, row[0])
            for table, column in CONSTRAINT_COLUMNS:
                for row in all_rows[table]:
                    if row['repository_id'] != repo_id or row[column] in (None, b'', ''):
                        continue
                    raw = row[column]
                    require(type(raw) is bytes and len(raw) <= 16 * 1024 * 1024, 'constraints_invalid')
                    values = strict_json(raw)
                    require(type(values) is dict, 'constraints_invalid')
                    for key in CONSTRAINT_KEYS:
                        value = values.get(key)
                        if value in (None, ''):
                            continue
                        require(type(value) is str, 'constraint_oid_unknown')
                        if key == 'ref' and not OID.fullmatch(value):
                            continue
                        local_oid(repo_id, value)
        for row in all_rows['deployments']:
            value = oid(row['ref'], symbolic=True)
            if value:
                local_oid(row['repository_id'], value)
        for table, columns in EXTERNAL_COLUMNS.items():
            for row in all_rows[table]:
                for column in columns:
                    value = oid(row[column])
                    if value:
                        external.add((table, column, value))
        changesets, revisions = {}, {}
        for row in all_rows['wiki_changesets']:
            key, value, version = row['changeset_id'], oid(row['synth_commit_sha']), row['synth_format_ver']
            source = decode(row['source'])
            require(key not in changesets and version in (0, 1)
                    and source in ('rest', 'admin', 'batch', 'compact', 'push', 'git'), 'wiki_classification_unknown')
            require(value is not None and row['repository_id'] in repos, 'wiki_identity_invalid')
            changesets[key] = row
            if version == 1 or source == 'git':
                local_oid(row['repository_id'], value, wiki=True)
            else:
                synthetic.add(('wiki_changesets', value))
        for row in all_rows['wiki_pages']:
            require(row['repository_id'] in repos, 'wiki_repository_missing')
            require_wiki_body(row['head_blob_sha'], row['body_size'], row['body_inline'], store)
        for row in all_rows['wiki_page_revisions']:
            require(row['changeset_id'] in changesets, 'wiki_changeset_missing')
            cs = changesets[row['changeset_id']]
            require(oid(row['commit_sha']) == oid(cs['synth_commit_sha']), 'wiki_commit_mismatch')
            revision_key = (row['page_id'], row['revision_id'])
            require(revision_key not in revisions, 'wiki_duplicate_revision')
            revisions[revision_key] = row
            op = decode(row['op'])
            if op == 'delete':
                require(oid(row['blob_sha']) is None and row['body_size'] == 0 and row['body_inline'] is None,
                        'wiki_delete_revision_invalid')
            else:
                require(op in ('create', 'update', 'rename', 'restore', 'compact'), 'wiki_revision_op_unknown')
                require_wiki_body(row['blob_sha'], row['body_size'], row['body_inline'], store)
        # applyDelete keeps the pre-delete body on the page and points head at an
        # op=delete revision whose blob is empty. deleted_at selects that class.
        # A live head must still match the page body exactly. A tombstone must
        # prove its retained body came from an earlier content revision of the
        # same page; skipping every deleted row would hide a forged body.
        for page in all_rows['wiki_pages']:
            revision = revisions.get((page['page_id'], page['head_revision_id']))
            require(revision is not None and revision['changeset_id'] == page['head_changeset_id'],
                    'wiki_head_revision_missing')
            cs = changesets[revision['changeset_id']]
            require(cs['repository_id'] == page['repository_id'], 'wiki_head_revision_mismatch')
            deleted = page['deleted_at'] not in (None, b'', '')
            if decode(revision['op']) == 'delete':
                require(deleted, 'wiki_head_revision_mismatch')
                require(any(item['page_id'] == page['page_id']
                            and decode(item['op']) in ('create', 'update', 'rename', 'restore', 'compact')
                            and item['blob_sha'] == page['head_blob_sha']
                            and item['body_size'] == page['body_size']
                            and item['body_inline'] == page['body_inline']
                            for item in revisions.values()), 'wiki_tombstone_body_unproven')
            else:
                require(not deleted
                        and revision['blob_sha'] == page['head_blob_sha']
                        and revision['body_size'] == page['body_size']
                        and revision['body_inline'] == page['body_inline'], 'wiki_head_revision_mismatch')
        for table in ('wiki_blob_refs', 'wiki_pending_blobs'):
            for row in all_rows[table]:
                value = oid(row['blob_sha'])
                integer(row['size'], 0, 64 * 1024 * 1024)
                require(value is not None, 'wiki_blob_invalid')
                # Legacy inline reference metadata never required a CAS file.
                if table == 'wiki_blob_refs' and row['size'] <= 4096:
                    continue
                require(store['cas'].get(value) == row['size'], 'wiki_cas_missing')
        for row in all_rows['wiki_page_index']:
            local_oid(row['repository_id'], row['head_commit_sha'], wiki=True)
            local_oid(row['repository_id'], row['head_blob_sha'], kind='blob', wiki=True)
        for row in all_rows['wiki_index_state']:
            for key in ('indexed_commit_sha', 'backlinks_indexed_sha'):
                local_oid(row['repository_id'], row[key], wiki=True)
        for row in all_rows['wiki_page_history']:
            for key in ('commit_sha', 'parent_commit_sha'):
                local_oid(row['repository_id'], row[key], wiki=True)
        for row in all_rows['wiki_git_repair_obligations']:
            local_oid(row['repository_id'], row['head_sha'], wiki=True)
        # Search revision SHA is a blob digest in the catalog, not a commit.
        for table in ('wiki_search_documents', 'wiki_search_projection_tasks'):
            for row in all_rows[table]:
                value = oid(row['revision_sha'])
                require(value is not None and row['repository_id'] in repos, 'wiki_search_identity_invalid')
        return {'schema': 'ags.upgrade-inventory.v1', 'schema_sha256': reviewed_schema_sha256,
                'tables': row_digests, 'logical_sha256': fingerprint(row_digests), 'store': store,
                'local_oids': [list(v) for v in sorted(local)],
                'external_oids': [list(v) for v in sorted(external)],
                'synthetic_wiki_oids': [list(v) for v in sorted(synthetic)],
                'recovery': {table: row_digests[table] for table in sorted(RECOVERY_TABLES | WIKI_TABLES)},
                'recovery_disposition': 'requires_separate_review',
                'unverified': ['quiescence', 'nested_configuration_completeness', 'backup_copy',
                               'seal_durability', 'restore_copy', 'key_decryption', 'host_metadata']}


def inspect_group(group, dsn, reviewed_schema_sha256, limit=MAX_ENTRIES):
    """Validate declared dependencies and read synthetic/offline inputs only when called."""
    objects = validate_group(group)
    by_role = {}
    summaries = {}
    total_entries = 0
    for key, item in objects.items():
        path = safe_path(item['source'], item['kind'])
        by_role.setdefault(item['role'], []).append(path)
        files, size = scan_tree(path, limit) if path.is_dir() else (None, path.stat().st_size)
        total_entries += len(files) if files is not None else 1
        require(total_entries <= limit, 'inventory_budget')
        summaries[key] = {'bytes': size, 'sha256': fingerprint(files) if files is not None
                          else file_digest(path)}
    path = sqlite_path(dsn)
    require(path == by_role['sqlite'][0], 'database_mapping_mismatch')
    # Environment parsing follows ags-runtime's shlex assignment rules; never source/eval it.
    env = {}
    require(by_role['runtime_env'][0].stat().st_size <= 65536, 'environment_budget')
    raw = by_role['runtime_env'][0].read_bytes()
    require(len(raw) <= 65536, 'environment_budget')
    try:
        for line in raw.decode('utf-8').splitlines():
            line = line.strip()
            if not line or line.startswith('#'):
                continue
            if line.startswith('export '):
                line = line[7:]
            key, separator, value = line.partition('=')
            require(separator and re.fullmatch(r'[A-Za-z_][A-Za-z0-9_]*', key)
                    and key not in env, 'environment_invalid')
            words = shlex.split(value, comments=True, posix=True)
            require(len(words) <= 1, 'environment_invalid')
            env[key] = words[0] if words else ''
    except (ValueError, UnicodeError):
        raise ContractError('environment_invalid') from None
    require(bool(env.get('SECRET_ENCRYPTION_KEY')), 'secret_recovery_material_missing')
    require(env.get('DB_DSN') == dsn and env.get('GIT_REPO_DIR') == str(by_role['git_root'][0]),
            'environment_mapping_mismatch')
    sources = {item['source'] for item in objects.values()}
    for key, value in env.items():
        if key.endswith(('_FILE', '_DIR')) or key in ('AGS_INTEGRATIONS_CONFIG', 'FORGEJO_INTEGRATION_REPO_MAP'):
            if value and key != 'GIT_REPO_DIR':
                require(str(absolute(value)) in sources, 'configuration_dependency_missing')
    # Opaque YAML/JSON/service dependencies require a reviewed exact dependency graph;
    # its review digest is mandatory, but is not a proof of completeness or approval.
    result = database_inventory(path, by_role['git_root'][0], reviewed_schema_sha256, limit)
    result['group_sha256'] = fingerprint(group)
    result['objects'] = summaries
    require(group['capacity']['backup_bytes'] >= sum(v['bytes'] for v in summaries.values()),
            'capacity_underestimated')
    result['inventory_sha256'] = fingerprint(result)
    return result
