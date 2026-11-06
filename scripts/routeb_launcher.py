#!/usr/bin/env python3
"""Small user-owned launcher for a separately provisioned root-owned bundle.

The wrapper is part of the recovery group. The frozen Python modules remain
independent authority assets. Neither HOME nor an environment import path can
select a bundle, and this launcher does not register or install one.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import pwd
import re
import runpy
import stat
import sys


def bundle_location(root, home):
    root, home = Path(root), Path(home)
    if root == home / '.ags':
        return Path('/usr/local/libexec/ags-autodeploy')
    base = home / '.local/state/ags-routeb-native'
    if (root.parent.parent != base or root.name != 'ags'
            or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,95}', root.parent.name)):
        raise ValueError('native_root_invalid')
    return Path('/usr/local/libexec/ags-routeb-native') / root.parent.name / 'bundle'


def main(argv=None):
    if not sys.flags.isolated:
        raise ValueError('isolated_interpreter_required')
    parser = argparse.ArgumentParser()
    parser.add_argument('--root', required=True, type=Path)
    parser.add_argument('command', choices=('doctor', 'serve'))
    args = parser.parse_args(argv)
    root = args.root
    if not root.is_absolute() or root.resolve() != root:
        raise ValueError('host_root_invalid')
    bundle = bundle_location(root, pwd.getpwuid(os.getuid()).pw_dir)
    for path in (bundle, *bundle.parents):
        info = path.lstat()
        if not stat.S_ISDIR(info.st_mode) or info.st_uid != 0 or info.st_mode & 0o022:
            raise ValueError('bundle_untrusted')
    sys.dont_write_bytecode = True
    sys.pycache_prefix = None
    # Fresh CLI process: remove user-selected import locations before loading
    # only the fixed root-owned implementation. Standard-library paths remain.
    sys.path = [str(bundle)] + [path for path in sys.path if path and Path(path).is_absolute()
                                and Path(path).resolve() != Path(__file__).resolve().parent
                                and 'site-packages' not in path]
    from upgrade import bundle_engine
    from upgrade_identity import read_trusted
    from routeb_host_identity import resolve_for_root
    host = resolve_for_root(root)
    if host.bundle != bundle:
        raise ValueError('bundle_identity_mismatch')
    raw = read_trusted(bundle / 'manifest.json', maximum=1024 * 1024)
    bundle_engine(bundle, hashlib.sha256(raw).hexdigest())
    if read_trusted(bundle / 'routeb_launcher.py', maximum=1024 * 1024) != Path(__file__).read_bytes():
        raise ValueError('launcher_identity_mismatch')
    from upgrade_backup_contract import ContractError
    from routeb_host_package import INTERPRETER_CLOSURE_BLOCKER, require_interpreter_closure
    try:
        require_interpreter_closure()
    except ContractError as error:
        if str(error) != INTERPRETER_CLOSURE_BLOCKER:
            raise
        if args.command == 'doctor':
            print(json.dumps({'status': 'blocked', 'native_execution_allowed': False,
                              'installable': False, 'blockers': [INTERPRETER_CLOSURE_BLOCKER],
                              'effects': []}, sort_keys=True))
        else:
            print('code=' + INTERPRETER_CLOSURE_BLOCKER, file=sys.stderr)
        return 78
    sys.argv = [str(bundle / 'ags-runtime.py'), '--root', str(root), args.command]
    runpy.run_path(str(bundle / 'ags-runtime.py'), run_name='__main__')


if __name__ == '__main__':
    try:
        raise SystemExit(main())
    except Exception:
        print('code=routeb_launcher_unavailable', file=sys.stderr)
        raise SystemExit(78)
