#!/usr/bin/env python3
"""Post-start recovery. Unknown stays unknown; same version needs a new approval.

No automatic retry when the previous spawn outcome is missing. A different
binary is not a same-version restart and is refused here.
"""
import sys
sys.dont_write_bytecode = True
sys.pycache_prefix = None
import uuid


class ForwardError(ValueError):
    def __init__(self, reason, exit_code=82):
        super().__init__(reason)
        self.code = reason
        self.exit_code = exit_code


def same_version(incident, approval):
    if incident.get('outcome') == 'unknown' or incident.get('pid_missing') is True:
        raise ForwardError('spawn_outcome_unknown', 82)
    if incident.get('closed') is not True or incident.get('writers'):
        raise ForwardError('closure_unproven', 83)
    if approval.get('purpose') != 'same-version-restart' or approval.get('decision') != 'approve':
        raise ForwardError('approval_invalid', 77)
    if approval.get('approval_id') == incident.get('start_token'):
        raise ForwardError('token_reused', 77)
    if incident.get('binary_sha256') != approval.get('binary_sha256'):
        raise ForwardError('candidate_changed', 83)
    return {'generation': uuid.uuid4().hex, 'binary_sha256': incident['binary_sha256'],
            'started': False}
