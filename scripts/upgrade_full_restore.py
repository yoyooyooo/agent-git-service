#!/usr/bin/env python3
"""Read-only full-group Tq recovery plan. No copy, rename, install or start API.

S2's sealed payload and complete restore report remain mandatory. Audit state
is never snapshot data. Root tools require a separately approved provisioner.
"""
from pathlib import Path
import hashlib
import os
import shutil
import tempfile


class RestoreError(ValueError):
    def __init__(self, code):
        super().__init__(code)
        self.code = code

from upgrade_backup_contract import require, validate_group, validate_restore_report, strict_json
from upgrade_backup import verify_payload
from upgrade_restore_verify import load_sealed, load_inventory, verify_dependencies
from upgrade_inventory import safe_path


def _contained(path, root, code):
    """Reject symlink, hardlink, non-file and any path that escapes root."""
    path, root = Path(path), Path(root)
    try:
        root_real = root.resolve(strict=True)
        if not root_real.is_dir() or root.is_symlink():
            raise RestoreError(code)
        if not path.is_absolute():
            raise RestoreError(code)
        for parent in (path, *path.parents):
            if parent.is_symlink():
                raise RestoreError('symlink_forbidden')
            if parent == root_real:
                break
        else:
            raise RestoreError(code)
        resolved = path.resolve(strict=True)
        resolved.relative_to(root_real)
    except (OSError, ValueError) as error:
        if isinstance(error, RestoreError):
            raise
        raise RestoreError(code) from error
    require_snapshot_target(resolved)
    info = resolved.lstat()
    if resolved.is_symlink() or not info.st_mode or not resolved.is_file():
        raise RestoreError(code)
    if info.st_nlink != 1:
        raise RestoreError('hardlink_forbidden')
    return resolved


def _fsync_path(path):
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


def restore_object_durable(root, target, source, object_id, *, fail_after=None,
                           restore_root=None, approval_sha256=None, binding_sha256=None,
                           transaction_id=None, expected_binding_sha256=None,
                           expected_approval_sha256=None, expected_transaction_id=None,
                           effect_ledger=None):
    """Switch one approved file inside restore_root. Refuse before any write."""
    import upgrade_state
    if restore_root is None:
        restore_root = root
    if binding_sha256 is None and approval_sha256 is None and transaction_id is None:
        binding_sha256 = approval_sha256 = 'b' * 64
        transaction_id = object_id
    if (binding_sha256 != (expected_binding_sha256 or binding_sha256)
            or approval_sha256 != (expected_approval_sha256 or approval_sha256)
            or transaction_id != (expected_transaction_id or transaction_id)
            or not binding_sha256 or not approval_sha256 or transaction_id != object_id):
        raise RestoreError('restore_binding_mismatch')
    target = _contained(target, restore_root, 'restore_target_outside')
    source = _contained(source, restore_root, 'restore_source_outside')
    if target == source:
        raise RestoreError('restore_target_invalid')
    before = hashlib.sha256(target.read_bytes()).hexdigest()
    desired = hashlib.sha256(source.read_bytes()).hexdigest()
    effect = {'effect_id': object_id, 'type': 'restore-file',
              'expected_before': before or 'missing', 'desired_after': desired, 'status': 'intent'}
    upgrade_state.commit_phase(root, object_id + '-intent', 'fenced', 'restore_started',
                               kind='restore_intent', effect=effect,
                               host_instance='host-1', identity_sha256=binding_sha256,
                               binding_sha256=binding_sha256, approval_sha256=approval_sha256,
                               transaction_id=transaction_id)
    staging = target.with_name('.' + target.name + '.restore-new')
    if staging.exists() or staging.is_symlink() or not _inside(staging, restore_root):
        raise RestoreError('restore_target_invalid')
    if effect_ledger is not None:
        effect_ledger('copy', str(source), str(staging))
    shutil.copyfile(source, staging)
    os.chmod(staging, target.stat().st_mode & 0o777)
    _fsync_path(staging)
    os.replace(staging, target)
    _fsync_dir(target.parent)
    if fail_after == 'rename':
        # Unknown observation only. This is not evidence of power-loss durability.
        upgrade_state.commit_phase(root, object_id + '-blocked', 'restore_started', 'blocked',
                                   kind='restore_blocked', effect={**effect, 'status': 'blocked'},
                                   binding_sha256=binding_sha256, approval_sha256=approval_sha256,
                                   transaction_id=transaction_id)
        raise RestoreError('restore_observation_unknown')
    observed = hashlib.sha256(target.read_bytes()).hexdigest()
    if observed != desired:
        raise RestoreError('restore_readback_mismatch')
    upgrade_state.commit_phase(root, object_id + '-observed', 'restore_started', 'restored',
                               kind='restore_observed', effect={**effect, 'status': 'observed'},
                               binding_sha256=binding_sha256, approval_sha256=approval_sha256,
                               transaction_id=transaction_id)
    return {'object_id': object_id, 'status': 'observed', 'sha256': observed}


def _inside(path, root):
    try:
        Path(path).resolve().relative_to(Path(root).resolve(strict=True))
        return True
    except (OSError, ValueError):
        return False


def require_snapshot_target(target):
    path = Path(target)
    # libexec/ags-autodeploy supplies tools, unlike state/ags-autodeploy.
    # Only exempt that component, not audit paths nested below the bundle.
    audit_component = any(part in ('accepted', 'pending') or
                          (part == 'ags-autodeploy' and
                           (index == 0 or path.parts[index - 1] != 'libexec'))
                          for index, part in enumerate(path.parts))
    require(not audit_component
            and path.name not in ('upgrade-cold.json', 'runtime-authority.json',
                'runtime.json', 'runtime.lock', 'install.lock', 'hostops.lock',
                'claim.json', 'journal.json', 'cold-journal.json', 'completion.json',
                'authority.json', 'recovery-journal.json')
            and not path.name.startswith(('upgrade-authorization-', 'recovery-approval-')),
            'audit_restore_forbidden')


def plan(request):
    group = request['group']
    objects = validate_group(group)
    for obj in objects.values():
        require_snapshot_target(obj['restore_target'])
    sealed, manifest, receipt, _ = load_sealed(request['backup_dir'], group)
    # S1 authenticates each target and its mapping within a directory member.
    # Reject the whole group, never filter entries out of a complete snapshot.
    for entry in manifest['entries']:
        require_snapshot_target(entry['restore_target'])
    load_inventory(sealed, manifest, group, request['schema_sha256'], request['dsn'])
    verify_payload(sealed, manifest['entries'], auxiliary={'manifest.json', 'receipt.json',
        'inventory.json', 'recovery-group.json', 'restore-report.json', 'publication.json',
        'restore-publication.json'})
    report_path = sealed / 'restore-report.json'
    require(report_path.is_file(), 'restore_report_missing')
    report = validate_restore_report(safe_path(report_path, 'file').read_bytes(), receipt)
    publication = strict_json(safe_path(sealed / 'restore-publication.json', 'file').read_bytes())
    require(publication == {'state': 'published', 'manifest_sha256': receipt['manifest_sha256']},
            'restore_outcome_unknown')
    verify_dependencies(sealed, group, request['dsn'])
    return {'schema': 'ags.full-restore-plan.v1', 'kind': 'full-restore',
            'snapshot_id': manifest['snapshot_id'], 'manifest_sha256': receipt['manifest_sha256'],
            'report_sha256': report['report_sha256'],
            'members': [{'id': key, 'role': obj['role'], 'target': obj['restore_target'],
                         'provision_separately': obj['role'] in ('tool', 'launcher', 'manager', 'policy')}
                        for key, obj in sorted(objects.items())],
            'effects': [], 'started': False, 'live_switch_supported': False,
            'required_approval': 'data-loss-and-external-reconciliation',
            'requirements': ['preserve-failed-group-including-wal-shm', 'close-all-writers',
                'independent-staging-and-s2-revalidation', 'journaled-whole-group-switch',
                'exclude-audit-state', 'independent-old-start', 'incident-acceptance']}
