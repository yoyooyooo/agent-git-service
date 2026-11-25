"""Small, closed-file primitives for the disposable Linux fixture only."""
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess


REGISTRY = Path('/usr/local/libexec/ags-routeb-native')
INTERPRETERS = Path('/usr/local/libexec/ags-routeb-interpreters')
ID = re.compile(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,47}')
SHA = re.compile(r'[0-9a-f]{64}')
OID = re.compile(r'[0-9a-f]{40}')


class Refused(RuntimeError):
    pass


def require(condition, code):
    if not condition:
        raise Refused(code)


def utc():
    return datetime.now(timezone.utc).isoformat()


def canonical(value):
    return (json.dumps(value, sort_keys=True, separators=(',', ':'),
                       ensure_ascii=True, allow_nan=False) + '\n').encode('ascii')


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def fingerprint(value):
    return sha(canonical(value))


def object_json(raw, maximum=1 << 20):
    require(0 < len(raw) <= maximum, 'json_size')

    def pairs(items):
        result = {}
        for key, value in items:
            require(key not in result, 'duplicate_json_key')
            result[key] = value
        return result

    def constant(_):
        raise Refused('nonfinite_json')

    try:
        result = json.loads(raw, object_pairs_hook=pairs, parse_constant=constant)
    except (ValueError, UnicodeError, RecursionError):
        raise Refused('invalid_json') from None
    require(type(result) is dict, 'json_object_required')
    return result


def absolute(path):
    path = Path(path)
    require(path.is_absolute() and str(path) == os.path.normpath(str(path))
            and path.resolve() == path, 'noncanonical_path')
    return path


def read_bytes(path, maximum=1 << 20, *, owner=None, mode=None):
    path = absolute(path)
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC)
    with os.fdopen(fd, 'rb') as stream:
        before = os.fstat(stream.fileno())
        require(stat.S_ISREG(before.st_mode) and before.st_nlink == 1
                and 0 <= before.st_size <= maximum, 'file_shape')
        require(owner is None or before.st_uid == owner, 'file_owner')
        require(mode is None or stat.S_IMODE(before.st_mode) == mode, 'file_mode')
        raw = stream.read(maximum + 1)
        after = os.fstat(stream.fileno())
        signature = lambda s: (s.st_dev, s.st_ino, s.st_size, s.st_mtime_ns, s.st_ctime_ns)
        require(len(raw) == before.st_size and signature(before) == signature(after), 'file_changed')
        return raw


def read_json(path, maximum=1 << 20, **kwargs):
    raw = read_bytes(path, maximum, **kwargs)
    return object_json(raw, maximum), raw


def fsync_dir(path):
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def exclusive(path, raw, *, mode=0o600, uid=None, gid=None):
    path = absolute(path)
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC, mode)
    with os.fdopen(fd, 'wb') as stream:
        stream.write(raw)
        os.fchmod(stream.fileno(), mode)
        if uid is not None:
            os.fchown(stream.fileno(), uid, gid)
        stream.flush()
        os.fsync(stream.fileno())
    fsync_dir(path.parent)
    return {'path': str(path), 'sha256': sha(raw), 'size': len(raw)}


def exclusive_json(path, value, **kwargs):
    return exclusive(path, canonical(value), **kwargs)


def reference(path, maximum=1 << 20):
    raw = read_bytes(path, maximum)
    return {'path': str(path), 'sha256': sha(raw), 'size': len(raw)}


def absent(path):
    try:
        Path(path).lstat()
    except FileNotFoundError:
        return True
    return False


def protected_directory(path):
    path = absolute(path)
    for current in (path, *path.parents):
        info = current.lstat()
        require(stat.S_ISDIR(info.st_mode) and info.st_uid == 0
                and not stat.S_IMODE(info.st_mode) & 0o022, 'unprotected_directory')
    return path


def file_reference(value, *, owner=0, maximum=1 << 20):
    require(type(value) is dict and set(value) == {'path', 'sha256', 'size'}
            and type(value['sha256']) is str and SHA.fullmatch(value['sha256'])
            and type(value['size']) is int and 0 < value['size'] <= maximum, 'file_reference')
    raw = read_bytes(value['path'], maximum, owner=owner)
    require(len(raw) == value['size'] and sha(raw) == value['sha256'], 'file_reference_changed')
    return raw


def process_identity(pid):
    require(type(pid) is int and pid > 1, 'pid_invalid')
    proc = Path('/proc') / str(pid)
    raw = (proc / 'stat').read_text()
    close = raw.rfind(')')
    require(close > 0 and raw[:raw.find(' ')] == str(pid), 'process_stat')
    fields = raw[close + 2:].split()
    status = (proc / 'status').read_text().splitlines()
    identities = {}
    for line in status:
        key, separator, value = line.partition(':')
        if separator and key in ('Uid', 'Gid'):
            values = value.split()
            require(len(values) == 4 and len(set(values)) == 1, 'process_credentials')
            identities[key] = int(values[0])
    require(set(identities) == {'Uid', 'Gid'}, 'process_credentials')
    return {'pid': pid, 'parent_pid': int(fields[1]), 'start_ticks': int(fields[19]),
            'uid': identities['Uid'], 'gid': identities['Gid']}


def command(argv, *, timeout=60, cwd=None, env=None):
    result = subprocess.run(argv, cwd=cwd, env=env, capture_output=True, timeout=timeout)
    require(result.returncode == 0, 'command_failed')
    return result.stdout
