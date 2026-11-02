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


def old_start_durable(root, restored, approval, observer):
    """Persist spawn intent, then require an explicit observer result."""
    import upgrade_state
    result = old_start(restored, approval)
    effect = {'effect_id': result['generation'], 'type': 'spawn',
              'expected_before': 'old_restored', 'desired_after': result['generation'], 'status': 'intent'}
    upgrade_state.commit_phase(root, 'start-' + approval['approval_id'], 'old_restored',
                               'start_attempted', kind='start_intent', effect=effect,
                               host_instance=approval.get('host_instance') or restored.get('host_instance') or 'host-1',
                               identity_sha256=restored.get('data_sha256') or 'a' * 64,
                               binding_sha256=restored.get('data_sha256') or 'b' * 64)
    observed = observer()
    if not isinstance(observed, dict) or observed.get('started') is not True or not observed.get('pid') or not observed.get('boot_id'):
        upgrade_state.commit_phase(root, 'observe-' + approval['approval_id'], 'start_attempted',
                                   'blocked', kind='start_blocked', effect={**effect, 'status': 'blocked'})
        raise RecoveryError('start_observation_unknown')
    upgrade_state.commit_phase(root, 'observe-' + approval['approval_id'], 'start_attempted',
                               'running', kind='start_observed', effect={**effect, 'desired_after': observed.get('generation', result['generation']), 'status': 'observed'})
    return observed


def production_old_start(root, restored, approval, observer):
    """Production recovery entry. It records the durable intent, then the observation.

    No process is spawned here. The caller supplies the observer.
    """
    return old_start_durable(root, restored, approval, observer)


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
