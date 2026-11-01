#!/usr/bin/env python3
"""System Trash only. No rm and no automatic purge.

Success requires the source to disappear and a trash entry to be readable.
Unknown results stay unknown. Lack of space refuses instead of emptying Trash.
"""
import sys
sys.dont_write_bytecode = True
sys.pycache_prefix = None
from pathlib import Path


class TrashError(ValueError):
    def __init__(self, reason, exit_code=83):
        super().__init__(reason)
        self.code = reason
        self.exit_code = exit_code


def allow(argv):
    if argv == ['/usr/bin/trash'] or len(argv) != 2 or argv[0] != '/usr/bin/trash':
        raise TrashError('argv_rejected')
    if argv[1].startswith('-'):
        raise TrashError('flag_rejected')
    return tuple(argv)


def move(path, *, eligible, readback, space_ok=True):
    path = Path(path)
    if not eligible or not space_ok:
        raise TrashError('not_eligible' if not eligible else 'space_insufficient')
    if path.is_symlink():
        raise TrashError('symlink_rejected')
    if readback.get('source_gone') is not True or not readback.get('trash_entry'):
        return {'status': 'unknown', 'path': str(path)}
    return {'status': 'trashed', 'entry': readback['trash_entry']}
