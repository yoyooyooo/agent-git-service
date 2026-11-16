#!/usr/bin/env python3
"""Route B host CLI: read-only admission/status and an explicit native run."""
import sys
sys.dont_write_bytecode = True
sys.pycache_prefix = None
import argparse
import json
from pathlib import Path
import stat

# Native execution starts this file with the registered interpreter and -I -B.
# Verify the import directory before loading any implementation from the bundle.
if __name__ == '__main__':
    try:
        bundle = Path(__file__).absolute().parent
        if not sys.flags.isolated or bundle.resolve() != bundle:
            raise ValueError('untrusted_entry')
        for parent in (bundle, *bundle.parents):
            info = parent.lstat()
            if not stat.S_ISDIR(info.st_mode) or info.st_uid != 0 or info.st_mode & 0o022:
                raise ValueError('untrusted_entry')
        sys.path = [str(bundle)] + [path for path in sys.path if path and Path(path).is_absolute()
                                    and 'site-packages' not in path]
    except (OSError, ValueError):
        print('code=frozen_host_entry_required', file=sys.stderr)
        raise SystemExit(78)

from upgrade import Failure
from upgrade_backup_contract import ContractError, require
from routeb_host_execution import HostExecution, failure_code, preflight, status
from routeb_host_package import load


class HostFailure(Exception):
    def __init__(self, code, exit_code=78):
        self.code, self.exit_code = code, exit_code
        super().__init__(code)


def parse(argv):
    parser = argparse.ArgumentParser(prog='ags-maintenance-transaction --mode host')
    sub = parser.add_subparsers(dest='command', required=True)
    for name in ('plan', 'run'):
        command = sub.add_parser(name)
        command.add_argument('--host', required=True)
        command.add_argument('--package', default='')
        command.add_argument('--approval', default='')
        if name == 'run':
            command.add_argument('--execute', action='store_true')
    inspect = sub.add_parser('status')
    inspect.add_argument('--transaction-id', required=True)
    inspect.add_argument('--journal', required=True)
    args = parser.parse_args(argv)
    args.mode = 'host'
    return args


def package_for(args):
    if not args.package or not args.approval:
        raise HostFailure('package_missing')
    package = load(args.package, args.approval, args.host)
    require(sys.flags.isolated and Path(__file__).absolute().parent == package.host.bundle,
            'frozen_host_entry_required')
    require(Path(sys.executable).resolve() == Path(package.profile['interpreter']['path']),
            'host_interpreter_mismatch')
    return package


def plan(args):
    result = preflight(package_for(args))
    print(json.dumps(result, sort_keys=True))
    return 0 if result['execution_allowed'] else 78


def dispatch(args):
    try:
        if args.command == 'plan':
            return plan(args)
        if args.command == 'status':
            print(json.dumps(status(args.journal, args.transaction_id), sort_keys=True))
            return 0
        require(args.execute is True, 'execute_required')
        package = package_for(args)
        planned = preflight(package)
        if not planned['execution_allowed']:
            print(json.dumps(planned, sort_keys=True))
            return 78
        print(json.dumps(HostExecution(package).run(), sort_keys=True))
        return 0
    except (HostFailure, Failure, ContractError) as error:
        code = error.code if isinstance(error, HostFailure) else failure_code(error)
        print('code=' + code, file=sys.stderr)
        return getattr(error, 'exit_code', 78)
    except (OSError, ValueError, KeyError, TypeError):
        print('code=host_package_unreadable', file=sys.stderr)
        return 78


if __name__ == '__main__':
    raise SystemExit(dispatch(parse(sys.argv[1:])))
