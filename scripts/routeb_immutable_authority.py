#!/usr/bin/env python3
"""Versioned immutable host authorities, distinct from restored user-owned files.

Archive bytes are ordinary private copies. Root ownership and host metadata
belong to the live authority and its independently provisioned replacement;
this module reads and verifies them, never installs, chowns or grants privilege.
"""
import hashlib
import os
from pathlib import Path
import stat

from upgrade_backup_contract import (absolute, digest, fields, fingerprint, identifier, integer,
                                     require, strict_json, timestamp)
from upgrade_identity import read_trusted


SCHEMA = 'ags.immutable-authority.v1'
PROVISION_SCHEMA = 'ags.immutable-authority-provision.v1'
ROLES = frozenset(('tool', 'launcher', 'manager', 'policy'))
METADATA = ('mode', 'uid', 'gid', 'mtime_ns', 'acl_sha256', 'xattrs_sha256')
FIELDS = ('schema', 'host_instance', 'asset', 'asset_sha256', 'provision_receipt',
          'provision_receipt_sha256', 'provision_approval', 'provision_approval_sha256',
          'tool_manifest', 'tool_manifest_sha256', 'metadata', 'size')
PROVISION_FIELDS = ('schema', 'host_instance', 'role', 'source', 'asset_sha256', 'metadata',
                    'tool_manifest_sha256', 'installer_sha256', 'provisioned_at', 'decision',
                    'approval_sha256', 'authorization_id')


def validate(item):
    """Validate an explicit v2 dependency descriptor without trusting a claim."""
    value = item['immutable_authority']
    fields(value, FIELDS)
    require(value['schema'] == SCHEMA and item['role'] in ROLES and item['kind'] == 'file',
            'immutable_authority_role_invalid')
    identifier(value['host_instance'])
    for key in ('asset', 'provision_receipt', 'provision_approval', 'tool_manifest'):
        absolute(value[key])
    for key in ('asset_sha256', 'provision_receipt_sha256', 'provision_approval_sha256', 'tool_manifest_sha256'):
        digest(value[key]); require(value[key] != '0' * 64, 'digest_invalid')
    fields(value['metadata'], METADATA)
    for key in ('mode', 'uid', 'gid', 'mtime_ns'):
        integer(value['metadata'][key])
    for key in ('acl_sha256', 'xattrs_sha256'):
        digest(value['metadata'][key])
    mode = value['metadata']['mode']
    require(value['metadata']['uid'] == 0 and 0 <= mode <= 0o777
            and not mode & 0o022, 'immutable_authority_not_frozen')
    integer(value['size'])
    require(value['asset'] != item['source'] and value['provision_receipt'] != item['source'],
            'immutable_authority_asset_missing')
    return value


def _asset_bytes(path):
    from upgrade import directory_fd
    path = Path(path)
    with directory_fd(path.parent, private=True) as parent:
        fd = os.open(path.name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=parent)
        try:
            before = os.fstat(fd)
            require(stat.S_ISREG(before.st_mode) and before.st_uid == os.getuid()
                    and not before.st_mode & 0o022 and before.st_nlink == 1
                    and 0 <= before.st_size <= 16 * 1024 * 1024, 'immutable_asset_unsafe')
            raw = b''
            while len(raw) <= before.st_size:
                chunk = os.read(fd, 1024 * 1024)
                if not chunk:
                    break
                raw += chunk
            after = os.fstat(fd)
            signature = lambda info: (info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns, info.st_ctime_ns)
            require(len(raw) == before.st_size and signature(before) == signature(after), 'immutable_asset_changed')
            return raw
        finally:
            os.close(fd)


def _authority_bytes(path, maximum):
    path = Path(path)
    raw = read_trusted(path, maximum=maximum)
    require(os.getuid() != 0 and all(not os.access(part, os.W_OK) for part in (path, *path.parents)),
            'immutable_authority_runtime_writable')
    return raw


def verify(item):
    """Independently read current root authority, recoverable asset and receipt."""
    value = validate(item)
    source = Path(item['source'])
    raw = _authority_bytes(source, maximum=16 * 1024 * 1024)
    from upgrade_backup import metadata_fields, supported_metadata
    metadata = metadata_fields(supported_metadata(source))
    require(metadata == value['metadata'] and len(raw) == value['size']
            and hashlib.sha256(raw).hexdigest() == value['asset_sha256'], 'immutable_authority_changed')
    asset = _asset_bytes(value['asset'])
    require(len(asset) == value['size'] and hashlib.sha256(asset).hexdigest() == value['asset_sha256'],
            'immutable_asset_changed')
    source_info, asset_info = source.stat(), Path(value['asset']).stat()
    require((source_info.st_dev, source_info.st_ino) != (asset_info.st_dev, asset_info.st_ino),
            'immutable_asset_not_independent')
    manifest_path = Path(value['tool_manifest'])
    manifest_raw = _authority_bytes(manifest_path, maximum=1024 * 1024)
    require(hashlib.sha256(manifest_raw).hexdigest() == value['tool_manifest_sha256'],
            'immutable_tool_manifest_changed')
    from upgrade import bundle_engine
    bundle_engine(manifest_path.parent, value['tool_manifest_sha256'])
    manifest = strict_json(manifest_raw)
    if item['role'] == 'tool':
        require(source == manifest_path or (source.parent == manifest_path.parent
                and manifest['files'].get(source.name) == value['asset_sha256']),
                'immutable_tool_member_unbound')
    approval_raw = _authority_bytes(Path(value['provision_approval']), maximum=1024 * 1024)
    require(hashlib.sha256(approval_raw).hexdigest() == value['provision_approval_sha256'],
            'immutable_provision_approval_changed')
    approval = strict_json(approval_raw)
    fields(approval, ('schema', 'authorization_id', 'actor', 'decision', 'host_instance',
                      'tool_manifest_sha256', 'installer_sha256', 'issued_at', 'expires_at', 'objects'))
    require(approval['schema'] == 'ags.immutable-authority-approval.v1' and approval['decision'] == 'approve'
            and type(approval['actor']) is str and approval['actor'].startswith('human:')
            and len(approval['actor']) > len('human:')
            and approval['host_instance'] == value['host_instance']
            and approval['tool_manifest_sha256'] == value['tool_manifest_sha256']
            and type(approval['objects']) is list, 'immutable_provision_approval_invalid')
    identifier(approval['authorization_id'])
    expected_object = {'role': item['role'], 'source': item['source'], 'asset_sha256': value['asset_sha256'],
                       'metadata': value['metadata']}
    seen = set()
    for row in approval['objects']:
        fields(row, ('role', 'source', 'asset_sha256', 'metadata'))
        require(row['role'] in ROLES and row['source'] not in seen, 'immutable_provision_approval_invalid')
        absolute(row['source']); digest(row['asset_sha256']); fields(row['metadata'], METADATA)
        seen.add(row['source'])
    require(sum(row == expected_object for row in approval['objects']) == 1,
            'immutable_provision_approval_mismatch')
    receipt_raw = _authority_bytes(Path(value['provision_receipt']), maximum=1024 * 1024)
    require(hashlib.sha256(receipt_raw).hexdigest() == value['provision_receipt_sha256'],
            'immutable_provision_receipt_changed')
    receipt = strict_json(receipt_raw)
    fields(receipt, PROVISION_FIELDS)
    require(receipt['schema'] == PROVISION_SCHEMA and receipt['decision'] == 'provisioned'
            and receipt['host_instance'] == value['host_instance'] and receipt['role'] == item['role']
            and receipt['source'] == str(source) and receipt['asset_sha256'] == value['asset_sha256']
            and receipt['metadata'] == metadata and receipt['tool_manifest_sha256'] == value['tool_manifest_sha256']
            and receipt['approval_sha256'] == value['provision_approval_sha256']
            and receipt['authorization_id'] == approval['authorization_id']
            and receipt['installer_sha256'] == approval['installer_sha256'],
            'immutable_provision_receipt_mismatch')
    digest(receipt['installer_sha256']); require(receipt['installer_sha256'] != '0' * 64, 'digest_invalid')
    require(timestamp(approval['issued_at']) <= timestamp(receipt['provisioned_at'])
            < timestamp(approval['expires_at']), 'immutable_provision_approval_expired')
    require(_authority_bytes(source, maximum=16 * 1024 * 1024) == raw
            and metadata_fields(supported_metadata(source)) == metadata, 'immutable_authority_changed')
    return {'schema': 'ags.immutable-authority-evidence.v1', 'object_id': item['id'], 'role': item['role'],
            'source': str(source), 'metadata': metadata, 'size': len(raw),
            'content_sha256': value['asset_sha256'], 'asset_sha256': value['asset_sha256'],
            'provision_receipt_sha256': value['provision_receipt_sha256'],
            'provision_approval_sha256': value['provision_approval_sha256'],
            'tool_manifest_sha256': value['tool_manifest_sha256'], 'host_instance': value['host_instance']}


def verify_group(group, expected=None):
    evidence = {item['id']: verify(item) for item in group['objects'] if 'immutable_authority' in item}
    if expected is not None:
        require(evidence == expected, 'immutable_authority_evidence_mismatch')
    return evidence


def validate_evidence(item, evidence):
    value = validate(item)
    expected = {'schema': 'ags.immutable-authority-evidence.v1', 'object_id': item['id'], 'role': item['role'],
                'source': item['source'], 'metadata': value['metadata'], 'size': value['size'],
                'content_sha256': value['asset_sha256'], 'asset_sha256': value['asset_sha256'],
                'provision_receipt_sha256': value['provision_receipt_sha256'],
                'provision_approval_sha256': value['provision_approval_sha256'],
                'tool_manifest_sha256': value['tool_manifest_sha256'], 'host_instance': value['host_instance']}
    require(evidence == expected, 'immutable_authority_evidence_mismatch')
    return evidence
