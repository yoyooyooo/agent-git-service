#!/usr/bin/env python3
"""Fixed-path host identity. No DNS, HOME discovery, or environment override.

The root profile is a separate provisioned trust root. This module never writes
one, never guesses a missing profile, and never grants service capability.
"""
import sys
sys.dont_write_bytecode = True
sys.pycache_prefix = None
from dataclasses import asdict, dataclass
import hashlib
import json
import os
from pathlib import Path
import platform
import plistlib
import pwd
import re
import socket
import stat
import subprocess

PROFILE = Path('/usr/local/libexec/ags-autodeploy/host-profile.json')
HOSTS = {'mini': (501, 'Darwin', 'arm64', 'ioplatformuuid'),
         'imile-win': (1000, 'Linux', 'x86_64', 'machine-id')}
MAX_PROFILE = 65536


class IdentityError(ValueError):
    def __init__(self, reason, exit_code):
        super().__init__(reason)
        self.code = reason
        self.phase = 'identity'
        self.exit_code = exit_code


def reject(reason='dependency_unavailable', code=69):
    raise IdentityError(reason, code)


def machine_digest(kind, value):
    """Domain-separated hash. The raw OS identifier never leaves this function."""
    if kind == 'ioplatformuuid':
        if type(value) is not str or not re.fullmatch(
                r'[0-9A-Fa-f]{8}(-[0-9A-Fa-f]{4}){3}-[0-9A-Fa-f]{12}', value):
            reject()
    elif kind == 'machine-id':
        if type(value) is not str or not re.fullmatch(r'[0-9A-Fa-f]{32}', value):
            reject()
    else:
        reject()
    material = 'ags.host-machine.v1\0' + kind + '\0' + value.lower()
    return hashlib.sha256(material.encode('ascii')).hexdigest()


@dataclass(frozen=True)
class MachineFacts:
    hostname: str
    uid: int
    euid: int
    gid: int
    user: str
    home: str
    platform: str
    arch: str
    machine_id_kind: str
    machine_id_sha256: str


@dataclass(frozen=True)
class HostIdentity:
    logical_host: str
    host_instance: str
    uid: int
    gid: int
    user: str
    home: str
    root: str
    platform: str
    arch: str
    machine_id_kind: str
    machine_id_sha256: str
    aliases: tuple
    profile_sha256: str

    @property
    def sha256(self):
        payload = json.dumps(asdict(self), sort_keys=True, separators=(',', ':')).encode()
        return hashlib.sha256(payload).hexdigest()


def observed_facts():
    """Local OS facts only. Hostname is not resolved and environment is ignored."""
    try:
        uid = os.getuid()
        account = pwd.getpwuid(uid)
        system = platform.system()
        if system == 'Darwin':
            output = subprocess.run(
                ['/usr/sbin/ioreg', '-a', '-d', '2', '-c', 'IOPlatformExpertDevice'],
                capture_output=True, check=True, timeout=10,
                env={'PATH': '/usr/bin:/bin', 'LC_ALL': 'C'})
            if len(output.stdout) > 1048576:
                reject()
            rows = plistlib.loads(output.stdout)
            values = [row['IOPlatformUUID'] for row in rows if 'IOPlatformUUID' in row]
            if len(values) != 1:
                reject()
            kind, value = 'ioplatformuuid', values[0]
        elif system == 'Linux':
            value = read_trusted(Path('/etc/machine-id'), maximum=128).decode('ascii').strip()
            kind = 'machine-id'
        else:
            reject('host_machine_mismatch', 64)
        return MachineFacts(socket.gethostname(), uid, os.geteuid(), os.getgid(),
                            account.pw_name, account.pw_dir, system, platform.machine(),
                            kind, machine_digest(kind, value))
    except (OSError, KeyError, TypeError, UnicodeError, plistlib.InvalidFileException,
            subprocess.SubprocessError):
        reject()


def read_trusted(path, *, owner=0, maximum=MAX_PROFILE):
    """No-follow walk. Every ancestor and the regular file belong to owner.

    owner is a unit-test seam, never a CLI, config, or environment parameter.
    """
    path = Path(path)
    if not path.is_absolute() or '..' in path.parts or str(path) != os.path.normpath(str(path)):
        reject()
    fd = None
    try:
        fd = os.open('/', os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)

        def directory_ok(descriptor):
            info = os.fstat(descriptor)
            if info.st_uid != owner or info.st_mode & 0o022:
                reject()

        directory_ok(fd)
        for part in path.parts[1:-1]:
            child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
            os.close(fd)
            fd = child
            directory_ok(fd)
        child = os.open(path.name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=fd)
        os.close(fd)
        fd = child
        before = os.fstat(fd)
        if (not stat.S_ISREG(before.st_mode) or before.st_nlink != 1 or before.st_uid != owner
                or before.st_mode & 0o022 or before.st_size > maximum):
            reject()
        raw = os.read(fd, maximum + 1)
        after = os.fstat(fd)
        signature = lambda info: (info.st_dev, info.st_ino, info.st_mode, info.st_uid,
                                  info.st_gid, info.st_nlink, info.st_size,
                                  info.st_mtime_ns, info.st_ctime_ns)
        if len(raw) != before.st_size or signature(before) != signature(after):
            reject()
        return raw
    except OSError:
        reject()
    finally:
        if fd is not None:
            os.close(fd)


def alias(value):
    if type(value) is not str or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9.-]{0,252}', value):
        reject('host_alias_unknown', 64)
    return value.lower()


def parse_profile(raw, facts):
    """Closed identity profile. Synthetic facts are an explicit Python seam."""
    def unique(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                reject()
            result[key] = value
        return result

    try:
        if len(raw) > MAX_PROFILE:
            reject()
        row = json.loads(raw, object_pairs_hook=unique)
        names = ('schema logical_host host_instance uid gid user home root platform arch '
                 'machine_id_kind machine_id_sha256 aliases').split()
        if type(row) is not dict or set(row) != set(names) or row['schema'] != 'ags.host-identity.v1':
            reject()
        if row['logical_host'] not in HOSTS:
            reject('host_mapping_conflict', 64)
        uid, system, arch, kind = HOSTS[row['logical_host']]
        if (type(row['uid']) is not int or row['uid'] != uid
                or type(row['gid']) is not int or row['gid'] < 0 or row['user'] != 'yoyo'
                or (row['platform'], row['arch'], row['machine_id_kind']) != (system, arch, kind)):
            reject('host_mapping_conflict', 64)
        if (type(row['host_instance']) is not str
                or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,95}', row['host_instance'])
                or type(row['machine_id_sha256']) is not str
                or not re.fullmatch(r'[0-9a-f]{64}', row['machine_id_sha256'])):
            reject()
        home = row['home']
        if (type(home) is not str or not home.startswith('/') or '..' in Path(home).parts
                or os.path.normpath(home) != home or home == '/'
                or row['root'] != str(Path(home) / '.ags')):
            reject()
        if type(row['aliases']) is not list or not 1 <= len(row['aliases']) <= 32:
            reject('host_mapping_conflict', 64)
        aliases = tuple(alias(item) for item in row['aliases'])
        if len(set(aliases)) != len(aliases):
            reject('host_mapping_conflict', 64)
        if alias(facts.hostname) not in aliases:
            reject('host_alias_unknown', 64)
        if (facts.uid != uid or facts.euid != uid or facts.gid != row['gid']
                or facts.user != row['user'] or facts.home != home):
            reject('wrong_execution_identity', 77)
        if ((facts.platform, facts.arch, facts.machine_id_kind, facts.machine_id_sha256)
                != (system, arch, kind, row['machine_id_sha256'])):
            reject('host_machine_mismatch', 64)
        selected = {key: row[key] for key in names if key not in ('schema', 'aliases')}
        return HostIdentity(**selected, aliases=aliases,
                            profile_sha256=hashlib.sha256(raw).hexdigest())
    except (ValueError, TypeError, KeyError, UnicodeError) as error:
        if isinstance(error, IdentityError):
            raise
        reject()


def resolve_identity():
    # Trust root first. Measuring the machine cannot select a profile.
    return parse_profile(read_trusted(PROFILE), observed_facts())


def require_same(identity, *, expected_sha256=None, root=None, host_instance=None):
    """Re-resolve before an effect. A parent string is not authority."""
    current = resolve_identity()
    if current != identity or (expected_sha256 is not None and current.sha256 != expected_sha256):
        reject('host_machine_mismatch', 64)
    if root is not None and current.root != str(root):
        reject('wrong_execution_identity', 77)
    if host_instance is not None and current.host_instance != host_instance:
        reject('host_machine_mismatch', 64)
    return current


def runtime_identity(root):
    """Cold-window admission only. Ordinary single-root startup stays independent."""
    identity = resolve_identity()
    require_same(identity, root=root)
    from upgrade import BUNDLE, bundle_engine
    from upgrade_recovery import read_window
    _, journal, _ = read_window(root)
    binding = journal['backup_binding']
    if identity.host_instance != binding['host_instance']:
        reject('host_machine_mismatch', 64)
    bundle_engine(BUNDLE, binding['tool_sha256'])
    if hashlib.sha256(read_trusted(PROFILE)).hexdigest() != identity.profile_sha256:
        reject()
    return identity


def failure_from(error):
    from upgrade import Failure
    return Failure(error.code, error.phase, error.exit_code)
