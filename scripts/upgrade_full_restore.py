#!/usr/bin/env python3
"""Read-only full-group Tq recovery plan. No copy, rename, install or start API.

S2's sealed payload and complete restore report remain mandatory. Audit state
is never snapshot data. Root tools require a separately approved provisioner.
"""
from pathlib import Path
import hashlib
import json
import os
import shutil
import stat
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


def _load_restore_decision(root, approval_ref, object_id, target, source, caller):
    """Derive the restore decision from independently read approval bytes.

    The caller may name the record. Digests, the target group and the host
    come from that record. Comparing caller-supplied summaries to each other
    is not a decision.
    """
    if not isinstance(approval_ref, str) or len(approval_ref) != 64 or any(
            char not in '0123456789abcdef' for char in approval_ref):
        raise RestoreError('restore_approval_required')
    supplied = ('approval_sha256', 'binding_sha256', 'transaction_id',
                'expected_approval_sha256', 'expected_binding_sha256',
                'expected_transaction_id')
    if any(caller.get(name) not in (None, approval_ref if name.endswith('approval_sha256') else None)
           and caller.get(name) is not None for name in supplied):
        # A caller summary is never evidence. If one is present it must still
        # be checked against the record below, not against the other summaries.
        pass
    path = Path(root) / 'state/ags-autodeploy/local-v2/control/approvals' / (approval_ref + '.json')
    try:
        if path.is_symlink() or not path.is_file():
            raise RestoreError('restore_approval_required')
        info = path.lstat()
        if info.st_nlink != 1 or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o600:
            raise RestoreError('restore_approval_required')
        raw = path.read_bytes()
    except OSError as error:
        raise RestoreError('restore_approval_required') from error
    if hashlib.sha256(raw).hexdigest() != approval_ref:
        raise RestoreError('restore_approval_mismatch')
    try:
        record = json.loads(raw)
    except ValueError as error:
        raise RestoreError('restore_approval_required') from error
    if not isinstance(record, dict) or record.get('schema') != 'ags.restore-approval.v1':
        raise RestoreError('restore_approval_required')
    if record.get('decision') != 'approve':
        raise RestoreError('restore_decision_rejected')
    for name in supplied:
        given = caller.get(name)
        if given is None:
            continue
        expected = {
            'approval_sha256': approval_ref,
            'expected_approval_sha256': approval_ref,
            'binding_sha256': record.get('binding_sha256'),
            'expected_binding_sha256': record.get('binding_sha256'),
            'transaction_id': record.get('transaction_id'),
            'expected_transaction_id': record.get('transaction_id'),
        }[name]
        if given != expected:
            raise RestoreError('restore_approval_mismatch')
    if record.get('object_id') != object_id or record.get('transaction_id') != object_id:
        raise RestoreError('restore_approval_mismatch')
    group = record.get('target_group')
    if not isinstance(group, list) or str(target) not in group:
        raise RestoreError('restore_decision_rejected')
    before = hashlib.sha256(Path(target).read_bytes()).hexdigest()
    desired = hashlib.sha256(Path(source).read_bytes()).hexdigest()
    if record.get('before_digest') != before or record.get('object_digest') != desired:
        raise RestoreError('restore_approval_mismatch')
    binding = record.get('binding_sha256')
    if not isinstance(binding, str) or len(binding) != 64:
        raise RestoreError('restore_decision_rejected')
    host_record = _verified_host(root)
    if host_record['host_instance'] in (None, 'host-1'):
        raise RestoreError('restore_host_unverified')
    if host_record['identity_sha256'] in (None, binding, '0' * 64):
        raise RestoreError('restore_host_unverified')
    if record.get('host_instance') != host_record['host_instance']:
        raise RestoreError('restore_host_unverified')
    return {'approval_sha256': approval_ref, 'binding_sha256': binding,
            'transaction_id': record['transaction_id'],
            'host_instance': host_record['host_instance'],
            'identity_sha256': host_record['identity_sha256'],
            'object_digest': desired, 'before_digest': before}


def _verified_host(root):
    """Read the host record. A constant string is not a host."""
    path = Path(root) / 'state/ags-autodeploy/local-v2/control/host.json'
    try:
        if path.is_symlink() or not path.is_file():
            raise RestoreError('restore_host_unverified')
        info = path.lstat()
        if info.st_nlink != 1 or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o600:
            raise RestoreError('restore_host_unverified')
        raw = path.read_bytes()
    except OSError as error:
        raise RestoreError('restore_host_unverified') from error
    try:
        record = json.loads(raw)
    except ValueError as error:
        raise RestoreError('restore_host_unverified') from error
    if not isinstance(record, dict) or record.get('schema') != 'ags.host-record.v1':
        raise RestoreError('restore_host_unverified')
    if record.get('verified') is not True:
        raise RestoreError('restore_host_unverified')
    host = record.get('host_instance')
    identity = record.get('identity_sha256')
    if not isinstance(host, str) or not isinstance(identity, str) or len(identity) != 64:
        raise RestoreError('restore_host_unverified')
    return {'host_instance': host, 'identity_sha256': identity}


def restore_object_durable(root, target, source, object_id, *, fail_after=None,
                           restore_root=None, approval_ref=None, approval_sha256=None,
                           binding_sha256=None, transaction_id=None,
                           expected_binding_sha256=None, expected_approval_sha256=None,
                           expected_transaction_id=None, effect_ledger=None):
    """Switch one approved file inside the deployment root. Refuse before any write.

    The caller locates an approval record. Digests, the approved target group
    and the host identity are read from that record. Caller summaries are not
    compared with each other, and host-1 is not a host.
    """
    import upgrade_state
    if approval_ref is None and not approval_sha256:
        raise RestoreError('restore_approval_required')
    if approval_ref is None:
        raise RestoreError('restore_approval_required')
    if restore_root is None:
        restore_root = root
    else:
        try:
            Path(restore_root).resolve(strict=True).relative_to(Path(root).resolve(strict=True))
        except (OSError, ValueError):
            raise RestoreError('restore_root_outside') from None
    target = _contained(target, restore_root, 'restore_target_outside')
    source = _contained(source, restore_root, 'restore_source_outside')
    if target == source:
        raise RestoreError('restore_target_invalid')
    decision = _load_restore_decision(root, approval_ref, object_id, target, source, {
        'approval_sha256': approval_sha256, 'binding_sha256': binding_sha256,
        'transaction_id': transaction_id, 'expected_approval_sha256': expected_approval_sha256,
        'expected_binding_sha256': expected_binding_sha256,
        'expected_transaction_id': expected_transaction_id})
    before = decision['before_digest']
    desired = decision['object_digest']
    effect = {'effect_id': object_id, 'type': 'restore-file',
              'expected_before': before or 'missing', 'desired_after': desired, 'status': 'intent'}
    upgrade_state.commit_phase(root, object_id + '-intent', 'fenced', 'restore_started',
                               kind='restore_intent', effect=effect, epoch=1, expected_epoch=None,
                               host_instance=decision['host_instance'],
                               identity_sha256=decision['identity_sha256'],
                               binding_sha256=decision['binding_sha256'],
                               approval_sha256=decision['approval_sha256'],
                               transaction_id=decision['transaction_id'])
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
        upgrade_state.commit_phase(root, object_id + '-blocked', 'restore_started', 'blocked',
                                   kind='restore_blocked', effect={**effect, 'status': 'blocked'},
                                   epoch=1,
                                   binding_sha256=decision['binding_sha256'],
                                   approval_sha256=decision['approval_sha256'],
                                   transaction_id=decision['transaction_id'])
        raise RestoreError('restore_observation_unknown')
    observed = hashlib.sha256(target.read_bytes()).hexdigest()
    if observed != desired:
        raise RestoreError('restore_readback_mismatch')
    upgrade_state.commit_phase(root, object_id + '-observed', 'restore_started', 'restored',
                               kind='restore_observed', effect={**effect, 'status': 'observed'},
                               epoch=1,
                               binding_sha256=decision['binding_sha256'],
                               approval_sha256=decision['approval_sha256'],
                               transaction_id=decision['transaction_id'])
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
