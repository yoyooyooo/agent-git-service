#!/usr/bin/env python3
"""Runtime authority separate from S4 completion. No implicit restart.

An accepted archive is history. Normal restart needs a new authority bound to
that completion, and a new generation. The old marker and archive stay.
"""
import sys
sys.dont_write_bytecode = True
sys.pycache_prefix = None
import uuid


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
