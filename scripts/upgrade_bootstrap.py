#!/usr/bin/env python3
"""One-time launcher transition. Same old binary only; no business upgrade.

A legacy pid/time receipt cannot enter S3. After the same binary restarts under
the new launcher, a real generation receipt can. Each incomplete phase stays
fenced. This module does not stop a service or rewrite a live runtime file.
"""
import sys
sys.dont_write_bytecode = True
sys.pycache_prefix = None
import hashlib


class BootstrapError(ValueError):
    def __init__(self, reason, exit_code):
        super().__init__(reason)
        self.code = reason
        self.exit_code = exit_code


PHASES = ('observe', 'retain', 'restart', 'receipt')


def admit_legacy(receipt, *, ledger=None):
    """Legacy receipts have no generation. S3 begin refuses them the same way."""
    from upgrade import Failure
    from upgrade_backup_contract import ContractError, identifier
    try:
        identifier(receipt.get('generation') if isinstance(receipt, dict) else None)
    except ContractError as error:
        raise Failure('stop_generation_unknown', 'bootstrap', 83) from error


def plan(root, receipt, *, binary):
    digest = hashlib.sha256(binary).hexdigest()
    if not isinstance(receipt, dict) or receipt.get('binary_sha256') != digest:
        raise BootstrapError('binary_mismatch', 83)
    if receipt.get('schema') == 'ags.runtime.v1':
        raise BootstrapError('already_current', 83)
    return {'root': str(root), 'binary_sha256': digest, 'business_upgrade': False,
            'phase': 'planned'}


def complete(plan_row, *, binary, receipt):
    if hashlib.sha256(binary).hexdigest() != plan_row['binary_sha256']:
        raise BootstrapError('binary_mismatch', 83)
    required = ('schema', 'generation', 'exit_code', 'forced_kill', 'stop_requested',
                'supervisor_exited', 'process_group_empty', 'binary_sha256')
    if (not isinstance(receipt, dict) or tuple(receipt) != required
            or receipt['schema'] != 'ags.runtime.v1'
            or receipt['binary_sha256'] != plan_row['binary_sha256']
            or receipt['exit_code'] != 0 or receipt['forced_kill'] is not False
            or receipt['stop_requested'] is not True or receipt['supervisor_exited'] is not True
            or receipt['process_group_empty'] is not True
            or not isinstance(receipt['generation'], str) or len(receipt['generation']) != 32):
        raise BootstrapError('receipt_not_s3', 82)
    return {'generation': receipt['generation'], 'business_upgrade': False,
            'binary_sha256': plan_row['binary_sha256']}


def complete_partial(root, receipt, *, phase, ledger=None):
    """A crash at any transition phase produces no S3 admission and no effect."""
    if phase not in PHASES:
        raise BootstrapError('phase_unknown', 82)
    raise BootstrapError('bootstrap_fenced', 82)
