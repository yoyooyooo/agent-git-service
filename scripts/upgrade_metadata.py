#!/usr/bin/env python3
"""Narrow metadata helper client. No arbitrary chown and no live-source writes.

The unprivileged copy remains the default and still fails ownership. A helper
may be invoked only for a registered staging role whose expected owner matches.
"""
import sys
sys.dont_write_bytecode = True
sys.pycache_prefix = None
import os
from pathlib import Path


class MetadataError(ValueError):
    def __init__(self, reason, exit_code=83):
        super().__init__(reason)
        self.code = reason
        self.exit_code = exit_code


def materialize(source, destination, expected, *, role, transaction, staging, helper=None):
    source, destination, staging = Path(source), Path(destination), Path(staging)
    if helper is None:
        raise MetadataError('helper_unavailable')
    if role not in ('tool', 'launcher', 'manager') or expected.get('owner') != 0:
        raise MetadataError('role_or_owner_rejected')
    if transaction is None or source == destination:
        raise MetadataError('transaction_rejected')
    for path in (source, destination):
        if path.is_symlink() or not staging in path.parents:
            raise MetadataError('path_rejected')
        if path.stat().st_nlink != 1:
            raise MetadataError('hardlink_rejected')
    info = destination.stat()
    if (info.st_uid, info.st_gid) == (expected['uid'], expected['gid']):
        return {'applied': False, 'matched': True}
    result = helper(source, destination, expected, role, transaction)
    if not isinstance(result, dict) or result.get('applied') is not True:
        raise MetadataError('helper_readback_invalid')
    return {'applied': True, 'uid': expected['uid'], 'gid': expected['gid'],
            'role': role, 'transaction': transaction}
