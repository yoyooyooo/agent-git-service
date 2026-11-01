#!/usr/bin/env python3
"""Approved recovery commands. plan has no effect; old-start is separate.

The existing pre-start recovery restores bytes and returns started false.
This module does not call it. A start needs its own approval and one token.
"""
import sys
sys.dont_write_bytecode = True
sys.pycache_prefix = None
import uuid


class RecoveryError(ValueError):
    def __init__(self, reason, exit_code=81):
        super().__init__(reason)
        self.code = reason
        self.exit_code = exit_code


def plan(journal):
    if journal.get('kind') == 'full-restore':
        from upgrade_full_restore import plan as full_restore_plan
        return full_restore_plan(journal)
    if journal.get('start_attempted') is True:
        raise RecoveryError('old_start_forbidden')
    return {'effects': ['restore-old-bytes'], 'started': False,
            'phase': journal.get('phase')}


def old_start(restored, approval, *, consumed=False):
    if restored.get('status') != 'old_restored' or restored.get('started') is not False:
        raise RecoveryError('not_restored')
    if approval.get('purpose') != 'old-start' or approval.get('decision') != 'approve':
        raise RecoveryError('approval_invalid', 77)
    if approval.get('approval_id') == restored.get('recovery_approval_id'):
        raise RecoveryError('recovery_approval_reused', 77)
    if consumed or approval.get('consumed') is True:
        raise RecoveryError('token_consumed', 82)
    if restored.get('data_sha256') != restored.get('tq_sha256'):
        raise RecoveryError('data_not_tq')
    return {'status': 'aborted_old_restored', 'generation': uuid.uuid4().hex, 'started': True}
