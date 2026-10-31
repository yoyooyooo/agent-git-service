#!/usr/bin/env python3
"""Closed S1 evidence contracts. No backup, restore, deletion or apply effects."""
from __future__ import annotations

from datetime import datetime
import hashlib
import json
from pathlib import Path, PurePosixPath
import re

MAX_JSON = 16 * 1024 * 1024
MAX_ENTRIES = 100000
SHA256 = re.compile(r'[0-9a-f]{64}')
OID = re.compile(r'[0-9a-f]{40}')
IDENTIFIER = re.compile(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,95}')
ROLES = frozenset(('sqlite', 'git_root', 'runtime_env', 'secret', 'config', 'old_binary',
                   'installation', 'launcher', 'manager', 'policy', 'tool', 'external_authority'))
REQUIRED_ROLES = frozenset(('sqlite', 'git_root', 'runtime_env', 'old_binary', 'installation',
                            'launcher', 'manager', 'policy', 'tool'))
RETENTION = {'scope': 'per_host', 'keep_verified_sealed': 5,
             'eligible': 'sealed_and_restore_verified', 'restore_copy': 'trash_after_verification',
             'failed_or_incomplete': 'retain_for_manual_review', 'deletion': 'system_trash_only',
             'insufficient_space': 'refuse', 'trash_counts_as_free_space': False}
CAPACITY_FIELDS = ('backup_bytes', 'restore_copy_bytes', 'candidate_scratch_bytes',
                   'migration_growth_bytes', 'runtime_reserve_bytes', 'available_bytes')
BINDING_FIELDS = ('host_instance deployment_id transaction_id attempt_id authorization_id old new '
                  'runtime_tree control_revision control_tree release_id release_tag target asset_sha256 '
                  'pin_sha256 tool_sha256 policy_sha256 old_binary_sha256 new_binary_sha256').split()


class ContractError(ValueError):
    """Credential-free reason codes only."""


def require(condition, reason):
    if not condition:
        raise ContractError(reason)


def fields(value, names):
    require(type(value) is dict and set(value) == set(names), 'fields_invalid')


def integer(value, minimum=0, maximum=1 << 63):
    require(type(value) is int and minimum <= value <= maximum, 'integer_invalid')


def text(value):
    require(type(value) is str and 0 < len(value) <= 4096
            and not any(ord(c) < 32 for c in value), 'text_invalid')


def digest(value):
    require(type(value) is str and SHA256.fullmatch(value) is not None, 'digest_invalid')


def choice(value, allowed):
    require(type(value) is str and value in allowed, 'enum_invalid')


def identifier(value):
    require(type(value) is str and IDENTIFIER.fullmatch(value) is not None, 'identifier_invalid')


def absolute(value):
    text(value)
    path = PurePosixPath(value)
    require(path.is_absolute() and not value.startswith('//') and str(path) == value and '..' not in path.parts
            and value != '/' and '\\' not in value, 'path_invalid')
    return Path(value)


def relative(value):
    text(value)
    path = PurePosixPath(value)
    require(not path.is_absolute() and not value.startswith('//') and str(path) == value and '..' not in path.parts
            and value != '.' and '\\' not in value, 'path_invalid')
    return path


def strict_json(raw):
    require(type(raw) is bytes and len(raw) <= MAX_JSON, 'json_budget')

    def pairs(items):
        result = {}
        for key, value in items:
            require(key not in result, 'duplicate_key')
            result[key] = value
        return result

    def constant(_):
        raise ContractError('json_constant_invalid')

    try:
        return json.loads(raw.decode('utf-8'), object_pairs_hook=pairs, parse_constant=constant)
    except (UnicodeError, ValueError, RecursionError):
        raise ContractError('json_invalid') from None


def canonical(value):
    try:
        return (json.dumps(value, sort_keys=True, separators=(',', ':'), ensure_ascii=True,
                           allow_nan=False) + '\n').encode('ascii')
    except (TypeError, ValueError, RecursionError):
        raise ContractError('json_invalid') from None


def fingerprint(value):
    return hashlib.sha256(canonical(value)).hexdigest()


def validate_retention(value):
    fields(value, RETENTION)
    for key, expected in RETENTION.items():
        require(type(value[key]) is type(expected) and value[key] == expected, 'retention_invalid')


def validate_capacity(value):
    fields(value, CAPACITY_FIELDS)
    for item in value.values():
        integer(item)
    require(value['backup_bytes'] > 0 and value['restore_copy_bytes'] >= value['backup_bytes'],
            'capacity_invalid')
    required = sum(value[key] for key in CAPACITY_FIELDS if key != 'available_bytes')
    require(required <= value['available_bytes'], 'insufficient_space')
    return required


def validate_binding(value):
    fields(value, BINDING_FIELDS)
    for key in ('host_instance', 'deployment_id', 'transaction_id', 'attempt_id', 'authorization_id'):
        identifier(value[key])
    for key in ('old', 'new', 'runtime_tree', 'control_revision', 'control_tree'):
        require(type(value[key]) is str and OID.fullmatch(value[key]) is not None
                and value[key] != '0' * 40, 'revision_invalid')
    for key in BINDING_FIELDS:
        if key.endswith('_sha256'):
            digest(value[key])
    integer(value['release_id'], 1)
    identifier(value['release_tag'])
    choice(value['target'], ('darwin_arm64', 'linux_amd64'))


def timestamp(value):
    text(value)
    try:
        parsed = datetime.fromisoformat(value.replace('Z', '+00:00'))
        require(parsed.tzinfo is not None and parsed.utcoffset() is not None, 'time_invalid')
        return parsed
    except ValueError:
        raise ContractError('time_invalid') from None


def validate_group(value):
    """An operator-reviewed dependency graph, never an inferred production inventory."""
    fields(value, ('schema', 'objects', 'dependency_review_sha256', 'storage', 'retention', 'capacity'))
    require(value['schema'] == 'ags.recovery-group.v1', 'schema_invalid')
    digest(value['dependency_review_sha256'])
    validate_retention(value['retention'])
    validate_capacity(value['capacity'])
    objects = value['objects']
    require(type(objects) is list and 0 < len(objects) <= MAX_ENTRIES, 'inventory_budget')
    ids, paths, roles = set(), [], []
    for item in objects:
        fields(item, ('id', 'role', 'source', 'restore_target', 'kind', 'depends_on'))
        identifier(item['id'])
        require(item['id'] not in ids, 'duplicate_object')
        ids.add(item['id'])
        choice(item['role'], ROLES)
        roles.append(item['role'])
        source, target = absolute(item['source']), absolute(item['restore_target'])
        require(source == target, 'relocation_unsupported')
        choice(item['kind'], ('file', 'directory'))
        require(item['kind'] == ('directory' if item['role'] == 'git_root' else 'file')
                or item['role'] in ('tool', 'external_authority'), 'kind_invalid')
        require(type(item['depends_on']) is list and len(item['depends_on']) <= MAX_ENTRIES
                and all(type(v) is str for v in item['depends_on'])
                and len(set(item['depends_on'])) == len(item['depends_on']), 'dependencies_invalid')
        for other in paths:
            require(source != other and source not in other.parents and other not in source.parents,
                    'overlapping_sources')
        paths.append(source)
    require(REQUIRED_ROLES <= set(roles), 'required_object_missing')
    for singleton in ('sqlite', 'git_root', 'runtime_env', 'old_binary', 'installation'):
        require(roles.count(singleton) == 1, 'duplicate_role')
    by_id = {item['id']: item for item in objects}
    visiting, done = set(), set()

    def visit(key):
        require(len(visiting) < 128, 'dependency_depth_budget')
        require(key in by_id and key not in visiting, 'dependency_missing_or_cycle')
        if key in done:
            return
        visiting.add(key)
        for dependency in by_id[key]['depends_on']:
            visit(dependency)
        visiting.remove(key)
        done.add(key)

    for key in by_id:
        visit(key)
    fields(value['storage'], ('backup_root', 'restore_root', 'private_policy_sha256', 'copy_method'))
    digest(value['storage']['private_policy_sha256'])
    require(value['storage']['copy_method'] == 'ordinary_copy', 'copy_method_unsupported')
    destinations = [absolute(value['storage'][key]) for key in ('backup_root', 'restore_root')]
    for index, destination in enumerate(destinations):
        require(not any(part in ('tmp', '.git') for part in destination.parts), 'backup_destination_unsafe')
        for other in paths + destinations[:index]:
            require(destination != other and destination not in other.parents and other not in destination.parents,
                    'backup_destination_overlap')
    return by_id


def validate_entry(item):
    fields(item, ('object_id', 'path', 'kind', 'size', 'sha256', 'mode', 'uid', 'gid', 'mtime_ns',
                  'acl_sha256', 'xattrs_sha256', 'restore_target'))
    identifier(item['object_id'])
    relative(item['path'])
    absolute(item['restore_target'])
    choice(item['kind'], ('file', 'directory'))
    for key in ('size', 'uid', 'gid', 'mtime_ns'):
        integer(item[key])
    integer(item['mode'], 0, 0o7777)
    for key in ('acl_sha256', 'xattrs_sha256'):
        digest(item[key])
    if item['kind'] == 'file':
        digest(item['sha256'])
    else:
        require(item['sha256'] is None and item['size'] == 0, 'directory_entry_invalid')


def validate_seal(manifest_raw, receipt_raw, group):
    """Validate evidence shape/digests; S2 must prove copy, stability and restore."""
    objects = validate_group(group)
    manifest, receipt = strict_json(manifest_raw), strict_json(receipt_raw)
    fields(manifest, ('schema', 'snapshot_id', 'binding', 'quiescence', 'inventory_sha256',
                      'started_at', 'ended_at', 'recovery_group_sha256', 'source_before_sha256',
                      'source_after_sha256', 'retention', 'capacity', 'entries'))
    require(manifest['schema'] == 'ags.backup-manifest.v1', 'schema_invalid')
    require(canonical(manifest) == manifest_raw, 'manifest_not_canonical')
    identifier(manifest['snapshot_id'])
    validate_binding(manifest['binding'])
    fields(manifest['quiescence'], ('nonce', 'stop_evidence_sha256', 'writers_evidence_sha256'))
    identifier(manifest['quiescence']['nonce'])
    for key in ('stop_evidence_sha256', 'writers_evidence_sha256'):
        digest(manifest['quiescence'][key])
    digest(manifest['inventory_sha256'])
    for key in ('recovery_group_sha256', 'source_before_sha256', 'source_after_sha256'):
        digest(manifest[key])
    require(manifest['recovery_group_sha256'] == fingerprint(group), 'group_mismatch')
    require(manifest['source_before_sha256'] == manifest['source_after_sha256'], 'source_changed')
    require(timestamp(manifest['started_at']) <= timestamp(manifest['ended_at']), 'time_order_invalid')
    validate_retention(manifest['retention'])
    validate_capacity(manifest['capacity'])
    require(manifest['retention'] == group['retention'] and manifest['capacity'] == group['capacity'],
            'group_mismatch')
    entries = manifest['entries']
    require(type(entries) is list and 0 < len(entries) <= MAX_ENTRIES, 'inventory_budget')
    paths, targets, covered = set(), set(), set()
    by_path = {}
    for item in entries:
        validate_entry(item)
        key, path, target = item['object_id'], item['path'], Path(item['restore_target'])
        require(key in objects, 'unknown_object')
        obj = objects[key]
        root = Path(obj['restore_target'])
        suffix = path.split('/', 1)
        require(suffix[0] == key, 'payload_mapping_invalid')
        expected = root if len(suffix) == 1 else root / suffix[1]
        require(target == expected and (obj['kind'] == 'directory' or len(suffix) == 1),
                'restore_mapping_invalid')
        if len(suffix) == 1:
            require(item['kind'] == obj['kind'], 'kind_invalid')
        require(path not in paths and str(target) not in targets, 'duplicate_entry')
        paths.add(path); targets.add(str(target)); covered.add(key); by_path[path] = item
    require(covered == set(objects), 'coverage_incomplete')
    for key in objects:
        require(key in by_path, 'root_entry_missing')
    for path in paths:
        parent = str(PurePosixPath(path).parent)
        if parent != '.':
            require(parent in by_path and by_path[parent]['kind'] == 'directory', 'parent_missing')
    fields(receipt, ('schema', 'state', 'snapshot_id', 'manifest_sha256', 'inventory_sha256',
                     'binding_sha256'))
    require(receipt['schema'] == 'ags.local-backup.v2' and receipt['state'] == 'sealed', 'schema_invalid')
    for key in ('manifest_sha256', 'inventory_sha256', 'binding_sha256'):
        digest(receipt[key])
    require(receipt['snapshot_id'] == manifest['snapshot_id']
            and receipt['manifest_sha256'] == hashlib.sha256(manifest_raw).hexdigest()
            and receipt['inventory_sha256'] == manifest['inventory_sha256']
            and receipt['binding_sha256'] == fingerprint(manifest['binding']), 'seal_digest_mismatch')
    return manifest


def validate_restore_report(raw, receipt):
    value = strict_json(raw)
    fields(value, ('schema', 'snapshot_id', 'manifest_sha256', 'restore_copy_id', 'verified_at',
                   'checks', 'report_sha256'))
    require(value['schema'] == 'ags.restore-verification.v1', 'schema_invalid')
    identifier(value['snapshot_id']); identifier(value['restore_copy_id'])
    timestamp(value['verified_at']); digest(value['manifest_sha256']); digest(value['report_sha256'])
    require(value['snapshot_id'] == receipt['snapshot_id']
            and value['manifest_sha256'] == receipt['manifest_sha256'], 'restore_binding_mismatch')
    fields(value['checks'], ('independent_copy', 'manifest', 'sqlite_integrity', 'sqlite_fk',
                            'sqlite_logical_digest', 'git_fsck', 'db_oids', 'wiki_cas', 'dependencies'))
    require(all(type(v) is bool and v for v in value['checks'].values()), 'restore_check_failed')
    require(value['report_sha256'] == fingerprint({k: v for k, v in value.items()
                                                 if k != 'report_sha256'}), 'restore_digest_mismatch')
    return value
