#!/usr/bin/env python3
"""Offline restore-copy verification. Never starts AGS or contacts a provider."""
from __future__ import annotations

import hashlib
import os
from pathlib import Path
import sqlite3
import shlex
import re
import stat
import time

from upgrade_backup_contract import (ContractError, canonical, fingerprint, require, strict_json,
                                     validate_group, validate_restore_report, validate_seal, digest)
from upgrade_backup import (assert_private_destination, copy_tree, durable_bytes,
                            publish, supported_metadata, apply_metadata, verify_payload, checkpoint, mark_partial)
from upgrade_inventory import check_repo, database_inventory, file_digest, safe_path, sqlite_path

ATTEMPT_PREFIX = 'restore-'
CHECKS = ('independent_copy', 'manifest', 'sqlite_integrity', 'sqlite_fk', 'sqlite_logical_digest',
          'git_fsck', 'db_oids', 'wiki_cas', 'dependencies')


def restore_attempt_id(snapshot_id):
    """Keep the random suffix and fit the unchanged S1 identifier limit."""
    from upgrade_backup_contract import identifier
    identifier(snapshot_id)
    label = snapshot_id
    if len(label) > 75:
        label = label[:62] + '-' + hashlib.sha256(snapshot_id.encode('ascii')).hexdigest()[:12]
    attempt = ATTEMPT_PREFIX + label + '-' + hashlib.sha256(os.urandom(16)).hexdigest()[:12]
    identifier(attempt)
    return attempt


def load_sealed(backup_dir, group):
    """A sealed backup is valid only when S1 accepts its manifest, receipt and group."""
    backup_dir = safe_path(backup_dir, 'directory')
    manifest_path = backup_dir / 'manifest.json'
    receipt_path = backup_dir / 'receipt.json'
    require(manifest_path.is_file() and receipt_path.is_file(), 'seal_missing')
    manifest_raw = safe_path(manifest_path, 'file').read_bytes()
    receipt_raw = safe_path(receipt_path, 'file').read_bytes()
    manifest = validate_seal(manifest_raw, receipt_raw, group)
    require(not (backup_dir / 'failure.json').exists(), 'backup_outcome_unknown')
    publication = strict_json(safe_path(backup_dir / 'publication.json', 'file').read_bytes())
    require(publication == {'state': 'published', 'manifest_sha256': strict_json(receipt_raw)['manifest_sha256']},
            'backup_outcome_unknown')
    require(backup_dir.name == manifest['snapshot_id'], 'snapshot_mismatch')
    return backup_dir, manifest, strict_json(receipt_raw), manifest_raw


def payload_identity(path):
    info = path.lstat()
    return info.st_dev, info.st_ino


def compare_manifest(root, manifest, sealed):
    """Compare the independent copy with the sealed payload, not with a live source path."""
    expected = {item['path']: item for item in manifest['entries']}
    observed = {}
    for path in sorted(root.rglob('*')):
        relative = path.relative_to(root).as_posix()
        info = path.lstat()
        require(not path.is_symlink(), 'symlink_forbidden')
        if stat.S_ISDIR(info.st_mode):
            observed[relative] = {'kind': 'directory', 'size': 0, 'sha256': None}
        elif stat.S_ISREG(info.st_mode):
            require(info.st_nlink == 1, 'hardlink_forbidden')
            observed[relative] = {'kind': 'file', 'size': info.st_size, 'sha256': file_digest(path)}
        else:
            raise ContractError('file_type_unsupported')
    require(set(observed) == set(expected), 'manifest_mismatch')
    for path, item in expected.items():
        got = observed[path]
        require(got['kind'] == item['kind'] and got['size'] == item['size'] and got['sha256'] == item['sha256'],
                'manifest_mismatch')
        copied = root / path
        original = sealed / path
        require(original.exists() and payload_identity(copied) != payload_identity(original), 'hardlink_forbidden')
        metadata = {key: item[key] for key in
                    ('mode', 'uid', 'gid', 'mtime_ns', 'acl_sha256', 'xattrs_sha256')}
        require(supported_metadata(original) == metadata and supported_metadata(copied) == metadata,
                'metadata_mismatch')


def load_inventory(backup_dir, manifest, group, schema, dsn):
    digest(schema)
    require(sqlite_path(dsn) == Path(next(item['source'] for item in group['objects']
                                         if item['role'] == 'sqlite')), 'database_mapping_mismatch')
    raw = safe_path(backup_dir / 'inventory.json', 'file').read_bytes()
    value = strict_json(raw)
    require(canonical(value) == raw and value['inventory_sha256'] == manifest['inventory_sha256']
            and fingerprint({k: v for k, v in value.items() if k != 'inventory_sha256'})
            == manifest['inventory_sha256'], 'inventory_digest_mismatch')
    require(value['group_sha256'] == fingerprint(group) and value['schema_sha256'] == schema,
            'inventory_binding_mismatch')
    digest(value['logical_sha256'])
    return value


def verify_dependencies(destination, group, dsn):
    """Validate the declared dependency graph and env references without opening original paths."""
    objects = validate_group(group)
    env_item = next(item for item in objects.values() if item['role'] == 'runtime_env')
    path = destination / env_item['id']
    require(path.stat().st_size <= 65536, 'environment_budget')
    env = {}
    try:
        for line in path.read_text(encoding='utf-8').splitlines():
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
    sources = {item['source'] for item in objects.values()}
    git_source = next(item['source'] for item in objects.values() if item['role'] == 'git_root')
    require(env.get('DB_DSN') == dsn and env.get('GIT_REPO_DIR') == git_source,
            'environment_mapping_mismatch')
    require(bool(env.get('SECRET_ENCRYPTION_KEY')), 'secret_recovery_material_missing')
    for key, value in env.items():
        if key.endswith(('_FILE', '_DIR')) or key in ('AGS_INTEGRATIONS_CONFIG', 'FORGEJO_INTEGRATION_REPO_MAP'):
            if value and key != 'GIT_REPO_DIR':
                require(value in sources, 'configuration_dependency_missing')
    require(all((destination / key).exists() for key in objects), 'coverage_incomplete')


def role_paths(group):
    objects = validate_group(group)
    by_role = {}
    for key, item in objects.items():
        require(item['role'] not in by_role or item['role'] not in ('sqlite', 'git_root'), 'duplicate_role')
        by_role.setdefault(item['role'], key)
    require('sqlite' in by_role and 'git_root' in by_role, 'required_object_missing')
    return by_role


def open_readonly(path):
    db = sqlite3.connect(path.as_uri() + '?mode=ro', uri=True, timeout=5)
    try:
        db.execute('PRAGMA query_only=ON')
        return db
    except Exception:
        db.close()
        raise


def verify_sealed_backup(backup_dir, *, trash, group=None, dsn=None, schema=None):
    """Copy the sealed payload aside, run every required check, then Trash only a successful copy."""
    require(group is not None and schema is not None and dsn is not None, 'restore_input_missing')
    backup_dir, manifest, receipt, _raw = load_sealed(backup_dir, group)
    basis = load_inventory(backup_dir, manifest, group, schema, dsn)
    try:
        verify_payload(backup_dir, manifest['entries'], auxiliary={'manifest.json', 'receipt.json',
                       'inventory.json', 'recovery-group.json', 'restore-report.json', 'publication.json',
                       'restore-publication.json'})
    except ContractError as exc:
        if str(exc) in ('payload_mismatch', 'metadata_mismatch'):
            raise ContractError('manifest_mismatch') from None
        raise
    restore_root = assert_private_destination(group['storage']['restore_root'])
    require(os.statvfs(restore_root).f_bavail * os.statvfs(restore_root).f_frsize
            >= group['capacity']['restore_copy_bytes'], 'insufficient_space')
    attempt = restore_attempt_id(manifest['snapshot_id'])
    destination = restore_root / attempt
    require(not destination.exists() and destination.parent == restore_root, 'restore_attempt_invalid')
    destination.mkdir(mode=0o700)
    roles = role_paths(group)
    checks = {}
    try:
        for key in validate_group(group):
            source = backup_dir / key
            require(source.exists() and not source.is_symlink(), 'payload_missing')
            target = destination / key
            if source.is_dir():
                copy_tree(source, target, lambda *_args: None)
            else:
                from upgrade_backup import copy_file
                copy_file(source, target)
        for item in sorted(manifest['entries'], key=lambda v: len(Path(v['path']).parts), reverse=True):
            apply_metadata(backup_dir / item['path'], destination / item['path'],
                           {key: item[key] for key in ('mode', 'uid', 'gid', 'mtime_ns', 'acl_sha256', 'xattrs_sha256')})
        compare_manifest(destination, manifest, backup_dir)
        checks['independent_copy'] = True
        database = destination / roles['sqlite']
        git_root = destination / roles['git_root']
        db = open_readonly(database)
        try:
            db.execute('PRAGMA foreign_keys=ON')
            integrity = db.execute('PRAGMA integrity_check').fetchall()
            require(len(integrity) == 1 and integrity[0][0] == 'ok', 'sqlite_integrity_failed')
            checks['sqlite_integrity'] = True
            foreign = db.execute('PRAGMA foreign_key_check').fetchall()
            require(not foreign, 'sqlite_fk_failed')
            checks['sqlite_fk'] = True
        finally:
            db.close()
        for repo in git_root.rglob('*.git'):
            if (repo / 'HEAD').is_file():
                check_repo(repo)
        checks['git_fsck'] = True
        inventory = database_inventory(database, git_root, schema)
        checks['db_oids'] = True
        checks['wiki_cas'] = True
        sealed_inventory = database_inventory(backup_dir / roles['sqlite'], backup_dir / roles['git_root'], schema)
        require(basis['logical_sha256'] == sealed_inventory['logical_sha256'] == inventory['logical_sha256'],
                'sqlite_logical_mismatch')
        require(basis['store'] == sealed_inventory['store'] == inventory['store'], 'store_inventory_mismatch')
        verify_dependencies(destination, group, dsn)
        checks['dependencies'] = True
        checks['sqlite_logical_digest'] = True
        compare_manifest(destination, manifest, backup_dir)
        checks['manifest'] = True
        require(set(checks) == set(CHECKS) and all(checks.values()), 'restore_check_failed')
        report_body = {'schema': 'ags.restore-verification.v1', 'snapshot_id': manifest['snapshot_id'],
                       'manifest_sha256': receipt['manifest_sha256'], 'restore_copy_id': attempt,
                       'verified_at': time.strftime('%Y-%m-%dT%H:%M:%S+00:00', time.gmtime()),
                       'checks': {key: checks[key] for key in CHECKS}}
        report_body['report_sha256'] = fingerprint({key: value for key, value in report_body.items()
                                                    if key != 'report_sha256'})
        raw = canonical(report_body)
        validate_restore_report(raw, receipt)
        report_path = backup_dir / 'restore-report.json'
        pending = backup_dir / ('.' + attempt + '-report.json')
        publication_pending = backup_dir / ('.' + attempt + '-publication.json')
        try:
            durable_bytes(publication_pending, canonical({'state': 'publishing',
                          'manifest_sha256': receipt['manifest_sha256']}))
            os.replace(publication_pending, backup_dir / 'restore-publication.json')
            publish(backup_dir)
            checkpoint('restore-report-write')
            durable_bytes(pending, raw)
            checkpoint('restore-report-publish')
            publish(backup_dir)
            os.replace(pending, report_path)
            checkpoint('restore-report-sync')
            publish(backup_dir)
            durable_bytes(publication_pending, canonical({'state': 'published',
                          'manifest_sha256': receipt['manifest_sha256']}))
            os.replace(publication_pending, backup_dir / 'restore-publication.json')
            publish(backup_dir)
        except Exception as exc:
            mark_partial(backup_dir, exc)
            raise
        require(destination.name.startswith(ATTEMPT_PREFIX) and destination.parent == restore_root,
                'restore_attempt_invalid')
        trash(destination)
    except Exception:
        mark_failed_copy(destination)
        raise
    return report_body


def mark_failed_copy(path):
    """A failed restore copy stays for review. Success is the only path that calls trash."""
    if not path.exists():
        return
    marker = path / 'failure.json'
    if marker.exists():
        return
    try:
        durable_bytes(marker, canonical({'schema': 'ags.restore-failure.v1', 'state': 'failed'}))
    except OSError:
        return
