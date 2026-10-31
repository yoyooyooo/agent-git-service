#!/usr/bin/env python3
"""Cold backup producer. No apply, service, provider or production-host entry."""
from __future__ import annotations

import hashlib
import os
from pathlib import Path
import sqlite3
import stat
import time

from upgrade_backup_contract import (ContractError, canonical, fingerprint, relative, require, validate_group)
from upgrade_inventory import (database_inventory, file_digest, inspect_group, safe_path, scan_tree, sqlite_path)

KEEP_VERIFIED = 5
SENSITIVE_ROLES = frozenset(('sqlite', 'runtime_env', 'secret', 'config', 'old_binary', 'installation',
                             'policy', 'tool'))


def checkpoint(name):
    """Named interruption point. Tests replace this; production callers leave it."""
    return name


def metadata_digest(path):
    info = path.lstat()
    return hashlib.sha256(canonical({
        'kind': 'directory' if stat.S_ISDIR(info.st_mode) else 'file',
        'mode': stat.S_IMODE(info.st_mode), 'uid': info.st_uid, 'gid': info.st_gid,
        'mtime_ns': info.st_mtime_ns, 'size': 0 if stat.S_ISDIR(info.st_mode) else info.st_size,
        'sha256': None if stat.S_ISDIR(info.st_mode) else file_digest(path),
    })).hexdigest()


def source_fingerprint(group, dsn, schema):
    """Content, refs and normalized DB digest. Physical SQLite layout is excluded."""
    inventory = inspect_group(group, dsn, schema)
    logical = database_inventory(sqlite_path(dsn), Path(group_object(group, 'git_root')['source']), schema)
    return fingerprint({'objects': inventory['objects'], 'logical_sha256': logical['logical_sha256'],
                        'store': logical['store']['files_sha256'], 'refs': logical['store']['repositories']})


def group_object(group, role):
    matches = [item for item in group['objects'] if item['role'] == role]
    require(len(matches) == 1, 'required_object_missing')
    return matches[0]


def assert_private_destination(path):
    path = safe_path(path, 'directory')
    require(path.is_absolute(), 'path_invalid')
    for component in (*reversed(path.parents), path):
        info = component.lstat()
        require(stat.S_ISDIR(info.st_mode) and not component.is_symlink(), 'symlink_forbidden')
        if component == path:
            require(stat.S_IMODE(info.st_mode) == 0o700, 'destination_permissions_invalid')
    return path


def measure_source_bytes(group):
    total = 0
    for item in group['objects']:
        path = safe_path(item['source'], item['kind'])
        if path.is_dir():
            _, size = scan_tree(path, 100000)
            total += size
        else:
            total += path.stat().st_size
    return total


def available_bytes(path):
    info = os.statvfs(path)
    return info.f_bavail * info.f_frsize


def preflight_space(group, destination):
    """Full backup, full restore-copy, scratch and declared reserve must fit now."""
    measured = measure_source_bytes(group)
    capacity = group['capacity']
    require(capacity['backup_bytes'] >= measured, 'capacity_underestimated')
    required = (capacity['backup_bytes'] + capacity['restore_copy_bytes'] + capacity['candidate_scratch_bytes']
                + capacity['migration_growth_bytes'] + capacity['runtime_reserve_bytes'])
    require(required <= capacity['available_bytes'], 'insufficient_space')
    require(available_bytes(destination) >= required, 'insufficient_space')
    return measured


def copy_file(source, destination):
    source_info = source.lstat()
    require(stat.S_ISREG(source_info.st_mode) and source_info.st_nlink == 1, 'hardlink_forbidden')
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW
    fd = os.open(destination, flags, 0o600)
    try:
        with source.open('rb') as incoming, os.fdopen(fd, 'wb') as outgoing:
            fd = None
            while True:
                block = incoming.read(1024 * 1024)
                if not block:
                    break
                outgoing.write(block)
            outgoing.flush()
            os.fsync(outgoing.fileno())
    finally:
        if fd is not None:
            os.close(fd)
    copied = destination.lstat()
    require(copied.st_ino != source_info.st_ino and copied.st_dev == source_info.st_dev or copied.st_ino != source_info.st_ino,
            'hardlink_forbidden')
    require(copied.st_nlink == 1, 'hardlink_forbidden')


def copy_tree(source, destination, recorder):
    """Ordinary copy. No clone, bundle, reflink or following of links."""
    source = safe_path(source)
    stack = [source]
    while stack:
        current = stack.pop()
        safe_path(current)
        info = current.lstat()
        relative_path = current.relative_to(source)
        target = destination if str(relative_path) == '.' else destination / relative_path
        require(target == destination or destination in target.parents, 'path_escape')
        if stat.S_ISDIR(info.st_mode):
            target.mkdir(mode=0o700, exist_ok=False)
            os.chmod(target, 0o700)
            recorder(current, target, info)
            stack.extend(sorted(current.iterdir(), reverse=True))
        elif stat.S_ISREG(info.st_mode):
            copy_file(current, target)
            recorder(current, target, info)
        else:
            raise ContractError('file_type_unsupported')


def sqlite_backup(source, destination):
    """Read committed WAL through the backup API. Do not repair or copy sidecars."""
    require(not Path(str(source) + '-journal').exists(), 'sqlite_journal_unsupported')
    readable = sqlite3.connect(source.as_uri() + '?mode=ro', uri=True, timeout=5)
    readable.execute('PRAGMA query_only=ON')
    copied = sqlite3.connect(destination)
    try:
        readable.backup(copied)
        copied.commit()
        check = copied.execute('PRAGMA integrity_check').fetchall()
        require(len(check) == 1 and check[0][0] == 'ok', 'sqlite_integrity_failed')
    except sqlite3.Error:
        raise ContractError('sqlite_backup_failed') from None
    finally:
        copied.close()
        readable.close()
    for suffix in ('-wal', '-shm', '-journal'):
        sidecar = Path(str(destination) + suffix)
        if sidecar.exists():
            sidecar.unlink()
    os.chmod(destination, 0o600)


def entry_for(object_id, source_root, source, target_root, info):
    relative_path = object_id if source == source_root else object_id + '/' + source.relative_to(source_root).as_posix()
    relative(relative_path)
    destination = target_root if source == source_root else target_root / source.relative_to(source_root)
    directory = stat.S_ISDIR(info.st_mode)
    return {'object_id': object_id, 'path': relative_path, 'kind': 'directory' if directory else 'file',
            'size': 0 if directory else info.st_size,
            'sha256': None if directory else file_digest(source),
            'mode': stat.S_IMODE(info.st_mode), 'uid': info.st_uid, 'gid': info.st_gid,
            'mtime_ns': info.st_mtime_ns, 'acl_sha256': hashlib.sha256(b'acl:none').hexdigest(),
            'xattrs_sha256': hashlib.sha256(b'xattr:none').hexdigest(),
            'restore_target': str(destination)}


def publish(directory):
    for path in sorted(directory.rglob('*'), key=lambda item: len(item.parts), reverse=True):
        if path.is_dir():
            fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY)
            try:
                os.fsync(fd)
            finally:
                os.close(fd)
    fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def create_sealed_backup(group, dsn, schema, binding, quiescence, snapshot_id, *, now=None):
    """Copy one cold snapshot beside the source and seal it. Failure leaves nothing."""
    objects = validate_group(group)
    backup_root = assert_private_destination(group['storage']['backup_root'])
    assert_private_destination(group['storage']['restore_root'])
    require(not (backup_root / snapshot_id).exists(), 'snapshot_exists')
    preflight_space(group, backup_root)
    checkpoint('scan')
    before = source_fingerprint(group, dsn, schema)
    inventory = inspect_group(group, dsn, schema)
    started = now or time.strftime('%Y-%m-%dT%H:%M:%S+00:00', time.gmtime())
    partial = backup_root / ('.partial-' + snapshot_id)
    require(not partial.exists(), 'partial_exists')
    partial.mkdir(mode=0o700)
    os.chmod(partial, 0o700)
    entries = []
    try:
        checkpoint('copy')
        for key, item in objects.items():
            source = safe_path(item['source'], item['kind'])
            target = partial / key
            if item['role'] == 'sqlite':
                checkpoint('sqlite')
                sqlite_backup(sqlite_path(dsn), target)
                os.chmod(target, 0o600)
                info = target.lstat()
                record = entry_for(key, source, source, Path(item['restore_target']), source.lstat())
                record['size'] = info.st_size
                record['sha256'] = file_digest(target)
                record['mode'] = 0o600
                entries.append(record)
                continue
            recorded = []

            def record_one(origin, _copied, info, bucket=recorded, object_key=key, root=source,
                           restore_root=Path(item['restore_target'])):
                bucket.append(entry_for(object_key, root, origin, restore_root, info))

            if source.is_dir():
                copy_tree(source, target, record_one)
            else:
                copy_file(source, target)
                record_one(source, target, source.lstat())
                os.chmod(target, 0o600 if item['role'] in SENSITIVE_ROLES else 0o600)
            entries.extend(recorded)
        checkpoint('seal')
        after = source_fingerprint(group, dsn, schema)
        require(before == after, 'source_changed')
        ended = now or time.strftime('%Y-%m-%dT%H:%M:%S+00:00', time.gmtime())
        manifest = {'schema': 'ags.backup-manifest.v1', 'snapshot_id': snapshot_id, 'binding': binding,
                    'quiescence': quiescence, 'inventory_sha256': inventory['inventory_sha256'],
                    'started_at': started, 'ended_at': ended, 'recovery_group_sha256': fingerprint(group),
                    'source_before_sha256': before, 'source_after_sha256': after,
                    'retention': group['retention'], 'capacity': group['capacity'],
                    'entries': sorted(entries, key=lambda item: item['path'])}
        from upgrade_backup_contract import validate_seal
        raw = canonical(manifest)
        receipt = {'schema': 'ags.local-backup.v2', 'state': 'sealed', 'snapshot_id': snapshot_id,
                   'manifest_sha256': hashlib.sha256(raw).hexdigest(),
                   'inventory_sha256': manifest['inventory_sha256'],
                   'binding_sha256': fingerprint(binding)}
        validate_seal(raw, canonical(receipt), group)
        manifest_path = partial / 'manifest.json'
        receipt_path = partial / 'receipt.json'
        manifest_path.write_bytes(raw)
        receipt_path.write_bytes(canonical(receipt))
        os.chmod(manifest_path, 0o600)
        os.chmod(receipt_path, 0o600)
        publish(partial)
        final = backup_root / snapshot_id
        os.rename(partial, final)
        publish(backup_root)
    except Exception:
        if partial.exists():
            remove_partial(partial)
        raise
    return {'backup_dir': str(backup_root / snapshot_id), 'manifest': manifest, 'receipt': receipt,
            'manifest_raw': raw}


def remove_partial(path):
    """A failed attempt is not a backup. Remove only the unsealed partial this call created."""
    for child in sorted(path.rglob('*'), key=lambda item: len(item.parts), reverse=True):
        if child.is_symlink() or child.is_file():
            child.unlink()
        else:
            child.rmdir()
    path.rmdir()


def list_backups(root):
    root = assert_private_destination(root)
    verified, sealed, failed = [], [], []
    for child in sorted(root.iterdir()):
        if child.name.startswith('.partial-') or not child.is_dir():
            failed.append({'path': child, 'state': 'incomplete'})
            continue
        receipt_path = child / 'receipt.json'
        report_path = child / 'restore-report.json'
        if not receipt_path.is_file():
            failed.append({'path': child, 'snapshot_id': child.name, 'state': 'incomplete'})
        elif report_path.is_file():
            verified.append({'path': child, 'snapshot_id': child.name, 'state': 'restore_verified',
                             'sealed_at': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime(child.stat().st_mtime))})
        else:
            sealed.append({'path': child, 'snapshot_id': child.name, 'state': 'sealed'})
    return {'verified': verified, 'sealed_unverified': sealed, 'failed': failed}


def apply_retention(root, verified, *, trash):
    """Move only older sealed-and-verified backups to Trash. Failures stay for review."""
    ordered = sorted(verified, key=lambda item: (item.get('sealed_at', ''), item['snapshot_id']))
    for item in ordered[:-KEEP_VERIFIED]:
        require(item.get('state') == 'restore_verified', 'retention_candidate_invalid')
        trash(item['path'])
