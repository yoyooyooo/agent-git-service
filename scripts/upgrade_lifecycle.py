#!/usr/bin/env python3
"""Runtime authority separate from S4 completion. No implicit restart.

An accepted archive is history. Normal restart needs a new authority bound to
that completion, and a new generation. The old marker and archive stay.
"""
import sys
sys.dont_write_bytecode = True
sys.pycache_prefix = None
import uuid
from pathlib import Path


def _approval_sha(approval):
    import hashlib
    import json
    return hashlib.sha256(json.dumps(approval, sort_keys=True, separators=(',', ':')).encode()).hexdigest()


class LifecycleError(ValueError):
    def __init__(self, reason, exit_code=81):
        super().__init__(reason)
        self.code = reason
        self.exit_code = exit_code


def restart_permitted(accepted, authority):
    """Accepted history alone is not permission. Authority must match exactly."""
    if not accepted or authority is None:
        raise LifecycleError('restart_fenced')
    if authority.get('state') != 'active':
        raise LifecycleError('restart_fenced')
    if authority.get('completion_sha256') != accepted.get('completion_sha256'):
        raise LifecycleError('completion_mismatch')
    if authority.get('approval_id') == accepted.get('s4_approval_id'):
        raise LifecycleError('s4_approval_reused', 77)
    return {'permitted': True, 'generation': uuid.uuid4().hex}


def activate_durable(root, accepted, approval):
    """Commit activation authority before any launcher effect; retries read it back."""
    if approval.get('purpose') != 'lifecycle-activate' or approval.get('decision') != 'approve':
        raise LifecycleError('approval_invalid', 77)
    if approval.get('approval_id') == accepted.get('s4_approval_id'):
        raise LifecycleError('s4_approval_reused', 77)
    if approval.get('completion_sha256') != accepted.get('completion_sha256'):
        raise LifecycleError('completion_mismatch')
    if not isinstance(accepted.get('completion_sha256'), str) or len(accepted['completion_sha256']) != 64:
        raise LifecycleError('completion_mismatch')
    import upgrade_state
    existing = upgrade_state.load_verified(root)
    for row in existing['events']:
        if row['kind'] == 'authority_activated' and row['approval_sha256'] == _approval_sha(approval) and row['epoch'] == accepted['epoch']:
            return existing['projection']['authority']
    current = (existing['projection'] or {}).get('authority')
    if current is not None and (current.get('state') != 'suspended' or accepted['epoch'] <= current.get('epoch', 0)):
        raise LifecycleError('authority_already_bound')
    result = upgrade_state.commit_phase(
        root, 'activation-' + approval['approval_id'], 'accepted', 'active',
        kind='authority_activated', epoch=accepted['epoch'],
        host_instance=accepted.get('host_instance') or approval.get('host_instance') or 'host-1',
        identity_sha256=accepted['completion_sha256'],
        approval_sha256=_approval_sha(approval),
        binding_sha256=accepted['completion_sha256'])
    return result['projection']['authority']


def production_activate(root, accepted, approval):
    """Production activation. The caller must already hold a verified host identity.

    Missing host or identity is a refusal, not a host-1 default.
    """
    if not accepted.get('host_instance') or not accepted.get('completion_sha256'):
        raise LifecycleError('host_identity_required', 77)
    return activate_durable(root, accepted, approval)


def historize_marker(root, epoch):
    """Move an accepted cold marker into the epoch history directory. Never delete it."""
    import os
    from pathlib import Path
    root = Path(root)
    marker = root / 'state/upgrade-cold.json'
    if not marker.is_file() or marker.is_symlink():
        raise LifecycleError('marker_missing')
    history = root / 'state/ags-autodeploy/local-v2/history' / ('epoch-' + str(epoch))
    history.mkdir(mode=0o700, parents=True, exist_ok=True)
    destination = history / 'upgrade-cold.json'
    if destination.exists() or destination.is_symlink():
        raise LifecycleError('marker_already_historized')
    os.replace(marker, destination)
    for parent in (marker.parent, destination.parent):
        fd = os.open(parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)
    return {'status': 'historized', 'path': str(destination), 'epoch': epoch}


def suspend_durable(root, epoch):
    import upgrade_state
    loaded = upgrade_state.load_verified(root)
    authority = loaded['projection'].get('authority')
    if authority is None or authority.get('epoch') != epoch or authority.get('state') != 'active':
        raise LifecycleError('authority_not_active')
    result = upgrade_state.commit_phase(root, 'suspend-' + str(epoch), 'active', 'suspended',
                                        kind='authority_suspended', epoch=epoch,
                                        identity_sha256=authority['binding_sha256'],
                                        binding_sha256=authority['binding_sha256'])
    return result['projection']['authority']


def activate(accepted, approval):
    if approval.get('purpose') != 'lifecycle-activate' or approval.get('decision') != 'approve':
        raise LifecycleError('approval_invalid', 77)
    if approval.get('approval_id') == accepted.get('s4_approval_id'):
        raise LifecycleError('s4_approval_reused', 77)
    if approval.get('completion_sha256') != accepted.get('completion_sha256'):
        raise LifecycleError('completion_mismatch')
    if not isinstance(accepted.get('completion_sha256'), str) or len(accepted['completion_sha256']) != 64:
        raise LifecycleError('completion_mismatch')
    return {'schema': 'ags.runtime-authority.v1', 'state': 'active',
            'completion_sha256': accepted['completion_sha256'],
            'approval_id': approval['approval_id'], 'epoch': accepted['epoch']}
