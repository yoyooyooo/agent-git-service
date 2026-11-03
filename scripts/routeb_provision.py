#!/usr/bin/env python3
"""Read-only provision plan. This module does not install or change a host.

Missing native evidence, a skipped native run, a wrong owner, or an
unreviewed interpreter closes the plan. A closed plan is not a pass.
"""
import os
import sys
sys.dont_write_bytecode = True
sys.pycache_prefix = None


class ProvisionError(ValueError):
    def __init__(self, code):
        self.code = code
        super().__init__(code)


def plan(request):
    interpreter = request.get('interpreter')
    if not isinstance(interpreter, str) or not interpreter.startswith('/'):
        raise ProvisionError('interpreter_rejected')
    if interpreter in ('/usr/bin/false', '/bin/false'):
        raise ProvisionError('interpreter_rejected')
    owner = request.get('owner')
    expected = request.get('expected_owner', os.getuid())
    if owner != expected:
        raise ProvisionError('owner_rejected')
    evidence = request.get('native_evidence')
    if not isinstance(evidence, dict) or evidence.get('passed') is not True:
        raise ProvisionError('native_evidence_missing')
    if evidence.get('skipped') is True:
        raise ProvisionError('native_skipped')
    head = request.get('exact_head')
    if not isinstance(head, str) or len(head) < 7:
        raise ProvisionError('exact_head_missing')
    if evidence.get('head') not in (None, head):
        raise ProvisionError('exact_head_mismatch')
    return {'installable': False, 'planned': True, 'effects': []}
