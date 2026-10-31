#!/usr/bin/env python3
"""Cold backup producer. No apply, service, provider or production-host entry."""
from __future__ import annotations

import hashlib
import os
from pathlib import Path
import sqlite3
import stat
import subprocess
import sys
import time

from upgrade_backup_contract import (ContractError, canonical, fingerprint, relative, require, validate_group)
from upgrade_inventory import (database_inventory, file_digest, inspect_group, safe_path, scan_tree, sqlite_path)

KEEP_VERIFIED = 5


def checkpoint(name):
    """Named interruption point. Tests replace this; production callers leave it."""
    return name


NONE_ACL = hashlib.sha256(b'acl:none').hexdigest()
NONE_XATTR = hashlib.sha256(b'xattr:none').hexdigest()


def xattrs(path):
    """Only macOS provenance is supported; other xattrs/ACLs require a different implementation."""
    if sys.platform == 'darwin':
        result = subprocess.run(['/usr/bin/xattr', str(path)], capture_output=True, timeout=30)
        require(result.returncode == 0, 'metadata_unavailable')
        names = result.stdout.decode('utf-8').splitlines()
        require(set(names) <= {'com.apple.provenance'}, 'xattr_unsupported')
        values = {}
        for name in names:
            value = subprocess.run(['/usr/bin/xattr', '-px', name, str(path)], capture_output=True, timeout=30)
            require(value.returncode == 0, 'metadata_unavailable')
            values[name] = bytes.fromhex(value.stdout.decode('ascii')).hex()
        return values
    require(sys.platform.startswith('linux') and hasattr(os, 'listxattr'), 'metadata_platform_unsupported')
    require(not os.listxattr(path, follow_symlinks=False), 'xattr_unsupported')
    return {}


def supported_metadata(path):
    info = path.lstat()
    if sys.platform == 'darwin':
        result = subprocess.run(['/bin/ls', '-lde', str(path)], capture_output=True, timeout=30)
        require(result.returncode == 0, 'metadata_unavailable')
        require(result.stdout.count(b'\n') == 1, 'acl_unsupported')
    attributes = xattrs(path)
    return {'mode': stat.S_IMODE(info.st_mode), 'uid': info.st_uid, 'gid': info.st_gid,
            'mtime_ns': info.st_mtime_ns, 'acl_sha256': NONE_ACL,
            'xattrs_sha256': fingerprint(attributes) if attributes else NONE_XATTR}


def apply_metadata(source, destination, metadata):
    """Preserve supported metadata and fail rather than silently losing required attributes."""
    attributes = xattrs(source)
    require(fingerprint(attributes) == metadata['xattrs_sha256'] if attributes
            else metadata['xattrs_sha256'] == NONE_XATTR, 'metadata_mismatch')
    if sys.platform == 'darwin':
        current = xattrs(destination)
        # The OS may attach provenance to newly created files. Preserve the captured value exactly.
        for name in current.keys() - attributes.keys():
            result = subprocess.run(['/usr/bin/xattr', '-d', name, str(destination)], capture_output=True, timeout=30)
            require(result.returncode == 0, 'metadata_unavailable')
        for name, value in attributes.items():
            if current.get(name) == value:
                continue
            result = subprocess.run(['/usr/bin/xattr', '-wx', name, value, str(destination)],
                                    capture_output=True, timeout=30)
            require(result.returncode == 0, 'metadata_unavailable')
    info = destination.stat()
    require((info.st_uid, info.st_gid) == (metadata['uid'], metadata['gid']), 'ownership_unsupported')
    os.chmod(destination, metadata['mode'])
    os.utime(destination, ns=(metadata['mtime_ns'], metadata['mtime_ns']))
    require(supported_metadata(destination) == metadata, 'metadata_mismatch')
    fd = os.open(destination, os.O_RDONLY | os.O_NOFOLLOW)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def source_fingerprint(group, dsn, schema):
    """Content, supported metadata, refs and logical DB digest. Physical SQLite layout is excluded."""
    inventory = inspect_group(group, dsn, schema)
    logical = database_inventory(sqlite_path(dsn), Path(group_object(group, 'git_root')['source']), schema)
    metadata = {}
    for key, item in validate_group(group).items():
        path = safe_path(item['source'], item['kind'])
        metadata[key] = metadata_tree(path) if path.is_dir() else supported_metadata(path)
    objects = dict(inventory['objects'])
    objects.pop(group_object(group, 'sqlite')['id'])
    return fingerprint({'objects': objects, 'metadata': metadata,
                        'logical_sha256': logical['logical_sha256'],
                        'store': logical['store']['files_sha256'], 'refs': logical['store']['repositories']})


def metadata_tree(root):
    result = []
    stack = [root]
    while stack:
        current = stack.pop()
        safe_path(current)
        result.append([str(current.relative_to(root)), supported_metadata(current)])
        if current.is_dir():
            stack.extend(sorted(current.iterdir(), reverse=True))
    return result


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


def preflight_space(group, destination, restore_root=None):
    """Full backup, full restore-copy, scratch and declared reserve must fit now."""
    measured = measure_source_bytes(group)
    capacity = group['capacity']
    require(capacity['backup_bytes'] >= measured, 'capacity_underestimated')
    required = (capacity['backup_bytes'] + capacity['restore_copy_bytes'] + capacity['candidate_scratch_bytes']
                + capacity['migration_growth_bytes'] + capacity['runtime_reserve_bytes'])
    require(required <= capacity['available_bytes'], 'insufficient_space')
    require(available_bytes(destination) >= required, 'insufficient_space')
    if restore_root is not None and os.stat(destination).st_dev != os.stat(restore_root).st_dev:
        require(available_bytes(restore_root) >= capacity['restore_copy_bytes'], 'insufficient_space')
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
    require(copied.st_dev != source_info.st_dev or copied.st_ino != source_info.st_ino, 'hardlink_forbidden')
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
    copied = None
    try:
        readable.execute('PRAGMA query_only=ON')
        copied = sqlite3.connect(destination)
        readable.backup(copied)
        copied.commit()
        require(copied.execute('PRAGMA journal_mode=DELETE').fetchone()[0] == 'delete',
                'sqlite_journal_unsupported')
        check = copied.execute('PRAGMA integrity_check').fetchall()
        require(len(check) == 1 and check[0][0] == 'ok', 'sqlite_integrity_failed')
    except sqlite3.Error:
        raise ContractError('sqlite_backup_failed') from None
    finally:
        if copied is not None:
            copied.close()
        readable.close()
    os.chmod(destination, 0o600)
    fd = os.open(destination, os.O_RDONLY | os.O_NOFOLLOW)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def entry_for(object_id, source_root, source, copied, target_root, info, *, sqlite=False):
    relative_path = object_id if source == source_root else object_id + '/' + source.relative_to(source_root).as_posix()
    relative(relative_path)
    destination = target_root if source == source_root else target_root / source.relative_to(source_root)
    directory = stat.S_ISDIR(info.st_mode)
    metadata = supported_metadata(source)
    digest = None if directory else file_digest(copied)
    if not directory and not sqlite:
        require(file_digest(source) == digest and source.stat().st_size == copied.stat().st_size, 'payload_mismatch')
    return {'object_id': object_id, 'path': relative_path, 'kind': 'directory' if directory else 'file',
            'size': 0 if directory else copied.stat().st_size, 'sha256': digest, **metadata,
            'restore_target': str(destination)}


def durable_bytes(path, raw):
    """Write, fsync the file, then fsync its directory. Callers publish only after this returns."""
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW
    fd = os.open(path, flags, 0o600)
    try:
        with os.fdopen(fd, 'wb', closefd=False) as stream:
            stream.write(raw)
            stream.flush()
            os.fsync(fd)
    finally:
        os.close(fd)
    directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(directory)
    finally:
        os.close(directory)


def publish(directory):
    for path in sorted(directory.rglob('*'), key=lambda item: len(item.parts), reverse=True):
        if path.is_dir() and not path.is_symlink():
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
    """Copy one cold snapshot beside the source and seal it. Failure keeps the partial for review."""
    from upgrade_backup_contract import identifier
    identifier(snapshot_id)
    objects = validate_group(group)
    backup_root = assert_private_destination(group['storage']['backup_root'])
    restore_root = assert_private_destination(group['storage']['restore_root'])
    require(backup_root / snapshot_id == (backup_root / snapshot_id).resolve()
            and not (backup_root / snapshot_id).exists(), 'snapshot_exists')
    preflight_space(group, backup_root, restore_root)
    checkpoint('scan')
    before = source_fingerprint(group, dsn, schema)
    inventory = inspect_group(group, dsn, schema)
    started = now or time.strftime('%Y-%m-%dT%H:%M:%S+00:00', time.gmtime())
    partial = backup_root / ('.partial-' + snapshot_id)
    require(not partial.exists(), 'partial_exists')
    partial.mkdir(mode=0o700)
    os.chmod(partial, 0o700)
    entries = []
    final = backup_root / snapshot_id
    try:
        checkpoint('copy')
        for key, item in objects.items():
            source = safe_path(item['source'], item['kind'])
            target = partial / key
            if item['role'] == 'sqlite':
                checkpoint('sqlite')
                sqlite_backup(sqlite_path(dsn), target)
                os.chmod(target, 0o600)
                record = entry_for(key, source, source, target, Path(item['restore_target']), source.lstat(), sqlite=True)
                require(file_digest(target) == record['sha256'], 'payload_mismatch')
                entries.append(record)
                continue
            recorded = []

            def record_one(origin, copied, info, bucket=recorded, object_key=key, root=source,
                           restore_target=Path(item['restore_target'])):
                bucket.append(entry_for(object_key, root, origin, copied, restore_target, info))

            if source.is_dir():
                copy_tree(source, target, record_one)
            else:
                copy_file(source, target)
                record_one(source, target, source.lstat())
            entries.extend(recorded)
        for record in sorted(entries, key=lambda v: len(Path(v['path']).parts), reverse=True):
            apply_metadata(Path(record['restore_target']), partial / record['path'],
                           {k: record[k] for k in ('mode', 'uid', 'gid', 'mtime_ns', 'acl_sha256', 'xattrs_sha256')})
        checkpoint('payload')
        verify_payload(partial, entries)
        checkpoint('seal')
        after = source_fingerprint(group, dsn, schema)
        require(before == after, 'source_changed')
        copied_inventory = database_inventory(partial / group_object(group, 'sqlite')['id'],
                                              partial / group_object(group, 'git_root')['id'], schema)
        require(copied_inventory['logical_sha256'] == inventory['logical_sha256'], 'sqlite_logical_mismatch')
        require(copied_inventory['store'] == inventory['store'], 'store_inventory_mismatch')
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
        receipt_raw = canonical(receipt)
        validate_seal(raw, receipt_raw, group)
        checkpoint('inventory')
        durable_bytes(partial / 'inventory.json', canonical(inventory))
        durable_bytes(partial / 'recovery-group.json', canonical(group))
        checkpoint('manifest')
        durable_bytes(partial / 'manifest.json', raw)
        checkpoint('receipt')
        durable_bytes(partial / 'receipt.json', receipt_raw)
        durable_bytes(partial / 'publication.json', canonical({'state': 'publishing',
                      'manifest_sha256': receipt['manifest_sha256']}))
        checkpoint('publish')
        verify_payload(partial, entries, auxiliary={'inventory.json', 'recovery-group.json', 'manifest.json', 'receipt.json', 'publication.json'})
        publish(partial)
        final = backup_root / snapshot_id
        checkpoint('rename')
        os.rename(partial, final)
        checkpoint('root-sync')
        publish(backup_root)
        durable_bytes(final / '.publication-complete.json', canonical({'state': 'published',
                      'manifest_sha256': receipt['manifest_sha256']}))
        os.replace(final / '.publication-complete.json', final / 'publication.json')
        publish(final)
    except Exception as exc:
        mark_partial(partial if partial.exists() else final, exc)
        raise
    return {'backup_dir': str(backup_root / snapshot_id), 'manifest': manifest, 'receipt': receipt,
            'manifest_raw': raw}


def verify_payload(root, entries, *, auxiliary=frozenset()):
    """The bytes about to be sealed must match the manifest, not only the source."""
    expected = {item['path']: item for item in entries}
    observed = set()
    for path in root.rglob('*'):
        relative_path = path.relative_to(root).as_posix()
        if relative_path in auxiliary:
            safe_path(path, 'file')
            continue
        require(relative_path in expected and not path.is_symlink(), 'payload_mismatch')
        item = expected[relative_path]
        info = path.lstat()
        if item['kind'] == 'directory':
            require(stat.S_ISDIR(info.st_mode), 'payload_mismatch')
        else:
            require(stat.S_ISREG(info.st_mode) and info.st_size == item['size']
                    and file_digest(path) == item['sha256'], 'payload_mismatch')
        require(supported_metadata(path) == {k: item[k] for k in
                ('mode', 'uid', 'gid', 'mtime_ns', 'acl_sha256', 'xattrs_sha256')}, 'metadata_mismatch')
        require(path.is_dir() or info.st_nlink == 1, 'hardlink_forbidden')
        observed.add(relative_path)
    require(observed == set(expected), 'payload_mismatch')


def mark_partial(path, exc):
    """Keep a failed attempt. A marker names the failure; its absence after copy means unknown."""
    if not path.exists():
        return
    reason = str(exc) if isinstance(exc, ContractError) else 'unknown'
    marker = path / 'failure.json'
    if marker.exists():
        return
    raw = canonical({'schema': 'ags.backup-failure.v1', 'state': 'failed', 'reason': reason})
    try:
        durable_bytes(marker, raw)
    except OSError:
        return


def list_backups(root, *, group, host_instance):
    """Classify only direct children. Eligibility is decided by reading seal evidence."""
    validate_group(group)
    require(Path(group['storage']['backup_root']) == Path(root), 'retention_binding_mismatch')
    from upgrade_backup_contract import identifier
    identifier(host_instance)
    root = assert_private_destination(root)
    verified, sealed, failed = [], [], []
    for child in sorted(root.iterdir()):
        if child.is_symlink() or not child.is_dir() or child.name.startswith('.'):
            failed.append({'path': str(child), 'state': 'incomplete'})
            continue
        item = read_verified(root, child, group, host_instance)
        if item is None:
            failed.append({'path': str(child), 'snapshot_id': child.name, 'state': 'incomplete'})
        elif item['state'] == 'restore_verified':
            verified.append(item)
        else:
            sealed.append(item)
    return {'verified': verified, 'sealed_unverified': sealed, 'failed': failed}


def read_verified(root, child, group, host_instance):
    from upgrade_backup_contract import strict_json, validate_restore_report
    try:
        from upgrade_backup_contract import validate_seal, timestamp
        child = backup_child(root, child)
        require(not (child / 'failure.json').exists(), 'backup_outcome_unknown')
        receipt_raw = safe_path(child / 'receipt.json', 'file').read_bytes()
        manifest_raw = safe_path(child / 'manifest.json', 'file').read_bytes()
        receipt = strict_json(receipt_raw)
        stored_group = strict_json(safe_path(child / 'recovery-group.json', 'file').read_bytes())
        require(Path(stored_group['storage']['backup_root']) == Path(root), 'retention_binding_mismatch')
        manifest = validate_seal(manifest_raw, receipt_raw, stored_group)
        inventory = strict_json(safe_path(child / 'inventory.json', 'file').read_bytes())
        require(inventory['inventory_sha256'] == manifest['inventory_sha256']
                and fingerprint({k: v for k, v in inventory.items() if k != 'inventory_sha256'})
                == manifest['inventory_sha256'], 'inventory_digest_mismatch')
        verify_payload(child, manifest['entries'], auxiliary={'inventory.json', 'recovery-group.json',
                       'manifest.json', 'receipt.json', 'restore-report.json', 'publication.json',
                       'restore-publication.json'})
        require(receipt['snapshot_id'] == child.name and manifest['binding']['host_instance'] == host_instance,
                'retention_binding_mismatch')
        publication = strict_json(safe_path(child / 'publication.json', 'file').read_bytes())
        require(publication == {'state': 'published', 'manifest_sha256': receipt['manifest_sha256']},
                'backup_outcome_unknown')
        sealed_at = timestamp(manifest['ended_at'])
        if not (child / 'restore-report.json').is_file():
            return {'path': str(child), 'snapshot_id': child.name, 'state': 'sealed',
                    'sealed_at': sealed_at, 'host_instance': manifest['binding']['host_instance']}
        restore_publication = strict_json(safe_path(child / 'restore-publication.json', 'file').read_bytes())
        require(restore_publication == {'state': 'published', 'manifest_sha256': receipt['manifest_sha256']},
                'restore_outcome_unknown')
        report = validate_restore_report(safe_path(child / 'restore-report.json', 'file').read_bytes(), receipt)
        require(timestamp(report['verified_at']) >= sealed_at, 'time_order_invalid')
        require(report['snapshot_id'] == child.name, 'restore_binding_mismatch')
        return {'path': str(child), 'snapshot_id': child.name, 'state': 'restore_verified',
                'sealed_at': sealed_at, 'host_instance': manifest['binding']['host_instance'],
                'verified_at': report['verified_at']}
    except (OSError, ContractError, KeyError, TypeError):
        return None


def backup_child(root, path):
    root = assert_private_destination(root).resolve()
    candidate = Path(path)
    require(candidate.is_absolute() and '..' not in candidate.parts and not candidate.is_symlink(),
            'retention_path_invalid')
    require(candidate.parent == root and candidate != root, 'retention_path_invalid')
    require(candidate.name not in ('', '.', '..') and '/' not in candidate.name and candidate.name == Path(candidate.name).name,
            'retention_path_invalid')
    resolved = candidate.resolve()
    require(resolved.parent == root and resolved.is_dir() and not resolved.is_symlink(), 'retention_path_invalid')
    return resolved


def apply_retention(root, verified=None, *, trash, group, host_instance):
    """Trash only older verified backups of this host. Paths and state come from the directories."""
    require(callable(trash), 'trash_required')
    if verified is not None:
        for item in verified:
            child = backup_child(root, item['path'])
            confirmed = read_verified(root, child, group, host_instance)
            require(confirmed and confirmed['state'] == 'restore_verified', 'retention_candidate_invalid')
    listed = list_backups(root, group=group, host_instance=host_instance)
    eligible = [item for item in listed['verified'] if item['host_instance'] == host_instance]
    ordered = sorted(eligible, key=lambda item: (item['sealed_at'], item['snapshot_id']))
    for item in ordered[:-KEEP_VERIFIED]:
        child = backup_child(root, item['path'])
        confirmed = read_verified(assert_private_destination(root), child, group, host_instance)
        require(confirmed and confirmed['state'] == 'restore_verified'
                and confirmed['host_instance'] == host_instance
                and confirmed == item, 'retention_candidate_invalid')
        trash(child)
