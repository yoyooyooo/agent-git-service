#!/usr/bin/env python3
"""Full-group restore execution. Reuses the offline plan and does not start a service.

The running recovery program is not a member of the group it restores. Audit
files, tokens and journals are excluded. A mixed or incomplete switch refuses
to start anything.
"""
from __future__ import annotations

import os
from pathlib import Path
import shutil
import stat
from datetime import datetime, timezone

from upgrade_backup_contract import ContractError, canonical, fingerprint, identifier, require, strict_json, timestamp
from upgrade_backup import apply_metadata, copy_file, copy_tree, metadata_fields, supported_metadata
from upgrade_full_restore import plan as offline_plan, require_snapshot_target
from routeb_native_delegation import is_delegated, require_delegation, SCHEMAS as DELEGATED_SCHEMAS


def _fsync(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def _fsync_dir(path):
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def plan_restore(request):
    planned = offline_plan(request)
    require(request.get('snapshot_id') in (None, planned['snapshot_id']), 'snapshot_mismatch')
    require(request.get('include_audit') is not True, 'audit_restore_forbidden')
    return planned


def recovery_journal_root(group):
    """A fixed audit sibling, bound by the sealed recovery group's storage path."""
    backup_root = Path(group['storage']['backup_root'])
    return backup_root.with_name('.' + backup_root.name + '-old-start-consumption')


def apply_restore(request, *, supervisor_path=None, journal=None):
    """Replace every planned member from the sealed payload. No service start."""
    planned = plan_restore(request)
    journal = [] if journal is None else journal
    if supervisor_path is not None:
        require(Path(supervisor_path).resolve() not in
                {Path(item['target']).resolve() for item in planned['members']},
                'supervisor_self_overwrite')
    sealed = Path(request['backup_dir'])
    switched = []
    for member in planned['members']:
        if member['provision_separately']:
            continue
        source = sealed / member['id']
        target = Path(member['target'])
        require_snapshot_target(target)
        require(source.exists(), 'payload_missing')
        effect_id = planned['snapshot_id'] + ':' + member['id']
        require(effect_id != request.get('transaction_id'), 'effect_identity_invalid')
        journal.append({'effect_id': effect_id, 'member': member['id'], 'status': 'intent'})
        staging = target.with_name('.' + target.name + '.routeb-restore')
        if staging.exists():
            raise ContractError('restore_target_invalid')
        if source.is_dir():
            copy_tree(source, staging, lambda *_args: None)
            # Children first: copying a child must not change a restored parent mtime.
            for origin in sorted([source, *source.rglob('*')], key=lambda p: len(p.parts), reverse=True):
                target_copy = staging if origin == source else staging / origin.relative_to(source)
                apply_metadata(origin, target_copy, metadata_fields(supported_metadata(origin)))
        else:
            copy_file(source, staging)
        sidecar = sealed / (member['id'] + '.metadata.json')
        if source.is_file():
            metadata = metadata_fields(supported_metadata(source))
            apply_metadata(source, staging, metadata)
            wal = Path(str(target) + '-wal')
            shm = Path(str(target) + '-shm')
            for sidecar_path in (wal, shm):
                if sidecar_path.exists() and not sidecar_path.is_symlink():
                    sidecar_path.rename(sidecar_path.with_name(sidecar_path.name + '.incident'))
        if staging.is_file():
            _fsync(staging)
        if target.exists():
            incident = target.with_name(target.name + '.incident')
            require(not incident.exists(), 'incident_material_exists')
            os.replace(target, incident)
            _fsync_dir(target.parent)
        os.replace(staging, target)
        _fsync_dir(target.parent)
        journal.append({'effect_id': effect_id, 'member': member['id'], 'status': 'observed'})
        switched.append(member['id'])
    require(switched, 'coverage_incomplete')
    result = {'applied': True, 'started': False, 'members': planned['members'],
            'switched': switched, 'snapshot_id': planned['snapshot_id'],
            'approval_id': request.get('approval_id')}
    from upgrade_restore_verify import load_sealed
    _, manifest, receipt, _ = load_sealed(sealed, request['group'])
    result.update(schema='ags.routeb-restored-group.v2', recovery_context={
        'backup_dir': str(sealed), 'group_sha256': fingerprint(request['group']),
        'manifest_sha256': receipt['manifest_sha256'], 'dsn': request['dsn'],
        'schema_sha256': request['schema_sha256'], 'host_instance': manifest['binding']['host_instance']},
        journal_root=str(recovery_journal_root(request['group'])))
    return result


def verify_recovered_group(restored, approval):
    """V2 old-start proof comes from the sealed group and current Tq bytes."""
    from upgrade_backup import source_fingerprint, verify_payload
    from upgrade_restore_verify import load_inventory, load_sealed
    from upgrade_recovery import AUXILIARY
    from upgrade_backup_contract import validate_restore_report
    from routeb_immutable_authority import verify_group
    context = restored.get('recovery_context')
    require(type(context) is dict, 'recovery_context_missing')
    backup_dir = Path(context['backup_dir'])
    from upgrade_inventory import safe_path
    group = strict_json(safe_path(backup_dir / 'recovery-group.json', 'file').read_bytes())
    require(fingerprint(group) == context['group_sha256'], 'recovery_group_mismatch')
    _, manifest, receipt, _ = load_sealed(backup_dir, group)
    require(receipt['manifest_sha256'] == context['manifest_sha256'] == approval.get('manifest_sha256')
            and manifest['snapshot_id'] == restored['snapshot_id']
            and approval.get('host_instance') == manifest['binding']['host_instance'], 'recovery_binding_mismatch')
    purpose = approval.get('purpose')
    require(purpose in ('full-restore', 'old-start'), 'recovery_purpose_invalid')
    if is_delegated(approval, purpose):
        require_delegation(approval, purpose, host_instance=manifest['binding']['host_instance'])
    else:
        require(type(approval.get('actor')) is str and approval['actor'].startswith('human:'),
                'recovery_approval_expired')
    require(timestamp(approval['issued_at']) <= datetime.now(timezone.utc) < timestamp(approval['expires_at']),
            'recovery_approval_expired')
    verify_payload(backup_dir, manifest['entries'], auxiliary=AUXILIARY)
    load_inventory(backup_dir, manifest, group, context['schema_sha256'], context['dsn'])
    report = validate_restore_report((backup_dir / 'restore-report.json').read_bytes(), receipt)
    if 'authority_evidence' in manifest:
        require(report.get('authority_evidence_sha256') == fingerprint(manifest['authority_evidence']),
                'immutable_authority_evidence_mismatch')
        verify_group(group, manifest['authority_evidence'])
    # This also covers the user-owned launcher/manager/policy that the live
    # switch deliberately leaves to its separate provisioner.
    require(source_fingerprint(group, context['dsn'], context['schema_sha256'])
            == manifest['source_before_sha256'], 'reprovisioned_group_unverified')
    return {'manifest_sha256': receipt['manifest_sha256'],
            'authority_evidence_sha256': report.get('authority_evidence_sha256'),
            'journal_root': str(recovery_journal_root(group))}


def _consumed_path(restored, approval, verified):
    require(type(restored.get('journal_root')) is str, 'recovery_journal_required')
    root = Path(restored['journal_root'])
    require(root.is_absolute() and root.resolve() == root, 'recovery_journal_invalid')
    require(str(root) == verified['journal_root'], 'recovery_journal_binding_mismatch')
    identifier(approval['approval_id'])
    return root / ('.old-start-' + approval['approval_id'])


def old_start(restored, approval, *, launcher):
    """One approved old start. The restore approval cannot authorize it."""
    require(restored.get('applied') is True and restored.get('started') is False, 'not_restored')
    require(approval.get('schema') in ('ags.routeb-recovery-approval.v1', DELEGATED_SCHEMAS['old-start']),
            'approval_invalid')
    require(approval.get('decision') == 'approve' and approval.get('purpose') == 'old-start',
            'approval_invalid')
    require(approval.get('data_loss_acknowledged') is True, 'data_loss_unacknowledged')
    require(approval.get('snapshot_id') == restored.get('snapshot_id'), 'snapshot_mismatch')
    require(approval.get('approval_id') and approval.get('approval_id') != restored.get('approval_id'),
            'recovery_approval_reused')
    require(restored.get('schema') == 'ags.routeb-restored-group.v2', 'recovery_context_missing')
    verified = verify_recovered_group(restored, approval)
    marker = _consumed_path(restored, approval, verified)
    from upgrade import directory_fd
    if not marker.parent.exists():
        with directory_fd(marker.parent.parent, private=True) as parent:
            os.mkdir(marker.parent.name, 0o700, dir_fd=parent)
            os.fsync(parent)
    with directory_fd(marker.parent, private=True) as parent:
        try:
            fd = os.open(marker.name, os.O_CREAT | os.O_EXCL | os.O_WRONLY | os.O_NOFOLLOW,
                         0o600, dir_fd=parent)
        except FileExistsError:
            raise ContractError('old_start_already_consumed') from None
        try:
            with os.fdopen(fd, 'wb', closefd=False) as stream:
                stream.write(canonical({'schema': 'ags.old-start-consumed.v1',
                    'snapshot_id': restored['snapshot_id'], 'approval_id': approval['approval_id'],
                    'manifest_sha256': verified['manifest_sha256']}))
                stream.flush()
                os.fsync(fd)
        finally:
            os.close(fd)
        os.fsync(parent)
    observed = launcher()
    require(isinstance(observed, dict) and observed.get('started') is True, 'start_observation_unknown')
    return {'started': True, 'approval_id': approval['approval_id'], 'snapshot_id': restored['snapshot_id']}
