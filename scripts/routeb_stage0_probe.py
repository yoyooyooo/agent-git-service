#!/usr/bin/env python3
"""Fixed, read-only native child probe. No service or production data access."""
import os
from pathlib import Path
import stat
import sys

_bundle = Path(__file__).absolute().parent
if (sys.flags.isolated != 1 or sys.flags.no_site != 1 or os.getuid() == 0
        or _bundle.name != 'bundle' or _bundle.parent.parent != Path('/usr/local/libexec/ags-routeb-native')):
    raise SystemExit(78)
for _path in (_bundle, *_bundle.parents):
    _info = _path.lstat()
    if not stat.S_ISDIR(_info.st_mode) or _info.st_uid != 0 or _info.st_mode & 0o022:
        raise SystemExit(78)
sys.path.insert(0, str(_bundle))

import argparse
import json
import sqlite3
from routeb_stage0_client import context


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', required=True)
    args = parser.parse_args()
    before = context(args.root, operations=('probe',))
    with sqlite3.connect(':memory:') as connection:
        old = connection.getlimit(sqlite3.SQLITE_LIMIT_LENGTH)
        connection.setlimit(sqlite3.SQLITE_LIMIT_LENGTH, 4096)
        assert connection.getlimit(sqlite3.SQLITE_LIMIT_LENGTH) == 4096
        connection.execute('create table probe (value text)')
        connection.execute('insert into probe values (?)', ('stage0',))
        assert connection.execute('select value from probe').fetchone() == ('stage0',)
        connection.setlimit(sqlite3.SQLITE_LIMIT_LENGTH, old)
    after = context(args.root, operations=('probe',))
    if before != after:
        raise SystemExit(78)
    print(json.dumps({'schema': 'ags.routeb-stage0-probe.v1', 'context': after,
                      'sqlite_limit_api': True, 'memory_database': True,
                      'production_accepted': False}, sort_keys=True))


if __name__ == '__main__':
    main()
