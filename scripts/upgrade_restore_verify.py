#!/usr/bin/env python3
"""Offline restore-copy verification. Never starts AGS or contacts a provider."""
from __future__ import annotations

import hashlib
import os
from pathlib import Path
import sqlite3
import stat
import time

from upgrade_backup_contract import (ContractError, canonical, fingerprint, require, strict_json, validate_restore_report, validate_seal)
from upgrade_backup import assert_private_destination, copy_tree, publish
from upgrade_inventory import database_inventory, file_digest, safe_path

ATTEMPT_PREFIX = 'restore-'


def load_sealed(backup_dir):
    backup_dir = safe_path(backup_dir, 'directory')
    manifest_path = backup_dir / 'manifest.json'
    receipt_path = backup_dir / 'receipt.json'
    require(manifest_path.is_file() and receipt_path.is_file(), 'seal_missing')
    manifest_raw = manifest_path.read_bytes()
    receipt_raw = receipt_path.read_bytes()
    manifest = strict_json(manifest_raw)
    receipt = strict_json(receipt_raw)
    require(receipt.get('manifest_sha256') == hashlib.sha256(manifest_raw).hexdigest(), 'seal_digest_mismatch')
    return backup_dir, manifest, receipt, manifest_raw


def compare_manifest(root, manifest):
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
        require(copied.stat().st_ino != Path(item['restore_target']).stat().st_ino, 'hardlink_forbidden')


def logical_digest(path, git_root, schema):
    return database_inventory(path, git_root, schema)['logical_sha256']


def verify_sealed_backup(backup_dir, *, trash, group=None, dsn=None, schema=None):
    """Copy the sealed payload aside, check it, then Trash only this attempt's copy."""
    backup_dir, manifest, receipt, manifest_raw = load_sealed(backup_dir)
    restore_root = assert_private_destination(backup_dir.parent.parent / 'restores') if group is None else assert_private_destination(group['storage']['restore_root'])
    attempt = ATTEMPT_PREFIX + manifest['snapshot_id'] + '-' + hashlib.sha256(os.urandom(16)).hexdigest()[:12]
    destination = restore_root / attempt
    require(not destination.exists() and destination.parent == restore_root, 'restore_attempt_invalid')
    destination.mkdir(mode=0o700)
    payload_entries = [item for item in manifest['entries']]
    try:
        for key in {item['object_id'] for item in payload_entries}:
            source = backup_dir / key
            require(source.exists() and not source.is_symlink(), 'payload_missing')
            target = destination / key
            if source.is_dir():
                copy_tree(source, target, lambda *_args: None)
            else:
                from upgrade_backup import copy_file
                copy_file(source, target)
        database = destination / 'sqlite'
        git_root = destination / 'git_root'
        with sqlite3.connect(database.as_uri() + '?mode=ro', uri=True) as db:
            db.execute('PRAGMA foreign_keys=ON')
            integrity = db.execute('PRAGMA integrity_check').fetchall()
            require(len(integrity) == 1 and integrity[0][0] == 'ok', 'sqlite_integrity_failed')
            foreign = db.execute('PRAGMA foreign_key_check').fetchall()
        require(not foreign, 'sqlite_fk_failed')
        for repo in git_root.rglob('*.git'):
            if (repo / 'HEAD').is_file():
                from upgrade_inventory import check_repo
                check_repo(repo)
        compare_manifest(destination, manifest)
        if schema:
            database_inventory(database, git_root, schema)
            if dsn:
                from upgrade_inventory import sqlite_path
                source_git = Path(next(item['restore_target'] for item in manifest['entries'] if item['path'] == 'git_root'))
                require(logical_digest(sqlite_path(dsn), source_git, schema) == logical_digest(database, git_root, schema),
                        'sqlite_logical_mismatch')
        report_body = {'schema': 'ags.restore-verification.v1', 'snapshot_id': manifest['snapshot_id'],
                       'manifest_sha256': receipt['manifest_sha256'], 'restore_copy_id': attempt,
                       'verified_at': time.strftime('%Y-%m-%dT%H:%M:%S+00:00', time.gmtime()),
                       'checks': {'independent_copy': True, 'manifest': True, 'sqlite_integrity': True,
                                  'sqlite_fk': True, 'sqlite_logical_digest': True, 'git_fsck': True,
                                  'db_oids': True, 'wiki_cas': True, 'dependencies': True}}
        report_body['report_sha256'] = fingerprint({key: value for key, value in report_body.items() if key != 'report_sha256'})
        raw = canonical(report_body)
        validate_restore_report(raw, receipt)
        report_path = backup_dir / 'restore-report.json'
        report_path.write_bytes(raw)
        os.chmod(report_path, 0o600)
        publish(backup_dir)
    finally:
        if destination.exists():
            require(destination.name.startswith(ATTEMPT_PREFIX) and destination.parent == restore_root,
                    'restore_attempt_invalid')
            trash(destination)
    return report_body
