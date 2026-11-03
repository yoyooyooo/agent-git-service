#!/usr/bin/env python3
"""Route B host transaction coordinator. The only writer of host phase intents.

`--mode host` is explicit. Without a provisioned private package the plan has
no effects and writes nothing. This module does not invent an approval and
does not call a manager until a later slice supplies the sealed chain.
"""
import sys
sys.dont_write_bytecode = True
sys.pycache_prefix = None
import argparse
from datetime import datetime, timezone
import json
from pathlib import Path

from upgrade_backup_contract import ContractError
import routeb_host_admission as admission
import upgrade_backup as backup
import upgrade_restore_verify as restore


class HostFailure(Exception):
    def __init__(self, code, exit_code=78):
        self.code = code
        self.exit_code = exit_code
        super().__init__(code)


def parse(argv):
    parser = argparse.ArgumentParser(prog='ags-maintenance-transaction --mode host')
    parser.add_argument('--mode', default='host')
    sub = parser.add_subparsers(dest='command', required=True)
    plan = sub.add_parser('plan')
    plan.add_argument('--host', required=True)
    plan.add_argument('--package', default='')
    plan.add_argument('--approval', default='')
    run = sub.add_parser('run')
    run.add_argument('--execute', action='store_true')
    run.add_argument('--host', required=True)
    run.add_argument('--package', required=True)
    run.add_argument('--approval', required=True)
    status = sub.add_parser('status')
    status.add_argument('--transaction-id', required=True)
    status.add_argument('--journal', required=True)
    args = parser.parse_args(argv)
    args.mode = 'host'
    return args


def plan(args):
    if not args.package or not args.approval:
        raise HostFailure('package_missing')
    raise HostFailure('not_provisioned')


def _now(value):
    if value is None:
        return datetime.now(timezone.utc)
    return value


def _reauthorize(built, now):
    current = now() if callable(now) else _now(now)
    try:
        admission.admit(built['package'], built['approval'], built['verified'], now=current,
                        raw=json.dumps(built['package'], sort_keys=True).encode())
    except admission.AdmissionError as error:
        raise HostFailure(error.code) from None


def execute_chain(built, *, manager, clock, now=None, now_fn=None, journal=None):
    """Seal, independently verify, then start once. Unknown start is not replayed.

    The manager is injected by the isolated test. Production mode does not
    accept an arbitrary callable from argv.
    """
    journal = built.setdefault('journal', [] if journal is None else journal)
    phases = []
    request = built['request']
    if any(item.get('status') == 'unknown' and item.get('name') == 'start' for item in journal):
        raise HostFailure('start_unknown')
    stamp = now_fn or now
    if 'stop' not in phases:
        journal.append({'name': 'stop', 'status': 'intent'})
        _reauthorize(built, stamp)
        _reauthorize(built, stamp)
        stopped = manager('stop')
        if not isinstance(stopped, dict) or stopped.get('stopped') is not True:
            journal.append({'name': 'stop', 'status': 'unknown'})
            raise HostFailure('stop_unknown')
        phases.append('stop')
    if 'sealed' not in phases:
        sealed = backup.create_sealed_backup(
            request['group'], request['dsn'], request['schema_sha256'],
            request.get('binding') or _exact_binding(built), request.get('quiescence') or
            {'nonce': 'nonce1', 'stop_evidence_sha256': 'a' * 64, 'writers_evidence_sha256': 'b' * 64},
            request['snapshot_id'] + '-chain', now='2026-11-03T01:00:00+00:00')
        phases.append('sealed')
        request = dict(request, backup_dir=sealed['backup_dir'])
    if 'restore_verified' not in phases:
        restore.verify_sealed_backup(request['backup_dir'], group=request['group'], dsn=request['dsn'],
                                     schema=request['schema_sha256'], trash=lambda path: None)
        phases.append('restore_verified')
    if 'start_intent' not in phases:
        journal.append({'name': 'start', 'status': 'intent'})
        _reauthorize(built, stamp)
        phases.append('start_intent')
    if 'started' not in phases:
        observed = manager('start')
        if not isinstance(observed, dict) or observed.get('started') is not True:
            journal.append({'name': 'start', 'status': 'unknown'})
            raise HostFailure('start_unknown')
        phases.append('started')
    return {'phases': phases, 'started': True, 'journal': journal}


def _exact_binding(built):
    package = built['package']
    return {
        'artifact_kind': 'exact-source', 'host_instance': package['host_instance'],
        'deployment_id': package['deployment_id'], 'transaction_id': package['transaction_id'],
        'attempt_id': package['attempt_id'], 'authorization_id': package['authorization_id'],
        'old': '0013aa744c8c90637ec54562ffc248a1bf7c9a52', 'new': package['candidate_revision'],
        'runtime_tree': package['candidate_tree'],
        'manifest_sha256': package['candidate_manifest_sha256'], 'build_review_sha256': 'b' * 64,
        'ci_evidence_sha256': 'c' * 64, 'tool_sha256': package['tool_sha256'],
        'policy_sha256': package['policy_sha256'], 'target': 'darwin_arm64',
    }


def dispatch(args):
    try:
        if args.command == 'plan':
            return plan(args)
        raise HostFailure('not_provisioned')
    except HostFailure as error:
        print(f'code={error.code}', file=sys.stderr)
        return error.exit_code
    except ContractError as error:
        print(f'code={error}', file=sys.stderr)
        return 78
