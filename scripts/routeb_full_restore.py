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

from upgrade_backup_contract import ContractError, require
from upgrade_backup import apply_metadata, copy_file, copy_tree, metadata_fields, supported_metadata
from upgrade_full_restore import plan as offline_plan, require_snapshot_target


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
    return {'applied': True, 'started': False, 'members': planned['members'],
            'switched': switched, 'snapshot_id': planned['snapshot_id'],
            'approval_id': request.get('approval_id')}


def _consumed_path(restored, approval):
    root = Path(restored.get('journal_root') or '.')
    return root / ('.old-start-' + approval['approval_id'])


def old_start(restored, approval, *, launcher):
    """One approved old start. The restore approval cannot authorize it."""
    require(restored.get('applied') is True and restored.get('started') is False, 'not_restored')
    require(approval.get('schema') == 'ags.routeb-recovery-approval.v1', 'approval_invalid')
    require(approval.get('decision') == 'approve' and approval.get('purpose') == 'old-start',
            'approval_invalid')
    require(approval.get('data_loss_acknowledged') is True, 'data_loss_unacknowledged')
    require(approval.get('snapshot_id') == restored.get('snapshot_id'), 'snapshot_mismatch')
    require(approval.get('approval_id') and approval.get('approval_id') != restored.get('approval_id'),
            'recovery_approval_reused')
    marker = _consumed_path(restored, approval)
    require(not marker.exists(), 'old_start_already_consumed')
    marker.parent.mkdir(parents=True, exist_ok=True)
    fd = os.open(marker, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    os.close(fd)
    observed = launcher()
    require(isinstance(observed, dict) and observed.get('started') is True, 'start_observation_unknown')
    return {'started': True, 'approval_id': approval['approval_id'], 'snapshot_id': restored['snapshot_id']}
