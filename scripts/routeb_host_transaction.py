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
import json

from upgrade_backup_contract import ContractError


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
