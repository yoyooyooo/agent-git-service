#!/usr/bin/env python3
"""Read-only native observations after a Route B validated start.

The coordinator owns admission, backup, installation and the start token. This
collector cannot start a manager, accept an upgrade or manufacture M6 evidence.
It retains private observations, including an interrupted capture, for review.
Injected clocks/probes always produce fixture evidence. The CLI has no injection
options. launchctl environment/arguments are deliberately not archived.
"""
from __future__ import annotations

import sys
sys.dont_write_bytecode = True
sys.pycache_prefix = None

import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import stat
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request

import upgrade_state

SCHEMA = 'ags.routeb-native-capture.v1'
MAX_JSON = 4 * 1024 * 1024
MAX_LOG_DELTA = 64 * 1024 * 1024
PREFIX_LAUNCHD = 'com.jackie.ags-hostpkg-test.'
PREFIX_SYSTEMD = 'ags-hostpkg-test-'
STAGE0_PROFILE = 'ags.routeb-native-profile.v2'


class CaptureError(ValueError):
    def __init__(self, code):
        self.code = code
        super().__init__(code)


def require(value, code):
    if not value:
        raise CaptureError(code)


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(',', ':')).encode() + b'\n'


def object_json(raw):
    def unique(pairs):
        out = {}
        for key, value in pairs:
            require(key not in out, 'duplicate_json_key')
            out[key] = value
        return out
    try:
        value = json.loads(raw, object_pairs_hook=unique)
    except (UnicodeError, ValueError) as error:
        if isinstance(error, CaptureError):
            raise
        raise CaptureError('json_invalid') from None
    require(type(value) is dict, 'json_not_object')
    return value


def digest(value, length=64):
    require(type(value) is str and re.fullmatch('[0-9a-f]{' + str(length) + '}', value)
            and value != '0' * length, 'digest_invalid')
    return value


def absolute(value):
    require(type(value) is str and value.startswith('/')
            and not any(ord(c) < 32 for c in value), 'path_invalid')
    path = Path(value)
    require(str(path) == value and '..' not in path.parts, 'path_invalid')
    for parent in (path, *path.parents):
        if parent.exists() or parent.is_symlink():
            require(not parent.is_symlink(), 'path_symlink')
    return path


def read_regular(path, maximum=MAX_JSON, *, private=True):
    path = absolute(str(path))
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    try:
        before = os.fstat(fd)
        require(stat.S_ISREG(before.st_mode) and before.st_uid in (0, os.getuid()), 'file_unsafe')
        require(not before.st_mode & (0o077 if private else 0o022), 'file_permissions')
        require(before.st_size <= maximum, 'file_too_large')
        chunks = []
        count = 0
        while True:
            raw = os.read(fd, min(65536, maximum + 1 - count))
            if not raw:
                break
            chunks.append(raw)
            count += len(raw)
            require(count <= maximum, 'file_too_large')
        after = os.fstat(fd)
        require((before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns)
                == (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns), 'file_changed')
        return b''.join(chunks)
    finally:
        os.close(fd)


def hash_file(path):
    path = absolute(str(path))
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    try:
        before = os.fstat(fd)
        require(stat.S_ISREG(before.st_mode) and before.st_uid in (0, os.getuid())
                and not before.st_mode & 0o022, 'binary_unsafe')
        hasher = hashlib.sha256()
        while True:
            raw = os.read(fd, 1024 * 1024)
            if not raw:
                break
            hasher.update(raw)
        after = os.fstat(fd)
        require((before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns)
                == (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns), 'binary_changed')
        return hasher.hexdigest()
    finally:
        os.close(fd)


class Archive:
    """Exclusive private output. A failed run is retained, never overwritten."""
    def __init__(self, root):
        self.root = absolute(str(root))
        parent = self.root.parent.stat()
        require(parent.st_uid == os.getuid() and not parent.st_mode & 0o022, 'output_parent_unsafe')
        self.root.mkdir(mode=0o700)
        self.artifacts = []

    def write(self, name, raw, kind):
        require(re.fullmatch(r'[a-zA-Z0-9_.-]+', name) and not name.startswith('.'), 'artifact_name')
        fd = os.open(self.root / name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, 'wb') as handle:
            handle.write(raw)
            handle.flush()
            os.fsync(handle.fileno())
        directory = os.open(self.root, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
        row = {'path': name, 'sha256': sha(raw), 'bytes': len(raw), 'kind': kind}
        self.artifacts.append(row)
        return row


def command(argv):
    # Callers pass only the fixed read commands below. No shell or inherited
    # loader/proxy/Task environment enters the operating-system probe.
    environment = {'PATH': '/usr/bin:/bin:/usr/sbin:/sbin', 'LC_ALL': 'C'}
    if argv[0] == '/usr/bin/systemctl' and '--user' in argv:
        environment['XDG_RUNTIME_DIR'] = '/run/user/' + str(os.getuid())
        environment['DBUS_SESSION_BUS_ADDRESS'] = 'unix:path=' + environment['XDG_RUNTIME_DIR'] + '/bus'
    result = subprocess.run(argv, capture_output=True, check=False, timeout=15, env=environment)
    require(result.returncode == 0, 'native_probe_failed')
    require(len(result.stdout) <= MAX_JSON, 'native_probe_too_large')
    return result.stdout


def boot_identity():
    if sys.platform == 'darwin':
        raw = command(['/usr/sbin/sysctl', '-n', 'kern.bootsessionuuid'])
    elif sys.platform.startswith('linux'):
        raw = Path('/proc/sys/kernel/random/boot_id').read_bytes()
    else:
        raise CaptureError('native_platform_unsupported')
    value = raw.decode().strip()
    require(bool(re.fullmatch(r'[0-9a-fA-F-]{36}', value)), 'boot_identity_invalid')
    return value, raw


def read_manager(spec):
    """Stable manager facts for both the start anchor and later samples.

    launchctl output includes environment values. Only top-level identity
    lines are retained, explicitly as a filtered artifact, never as raw text.
    systemctl is asked for a bounded set of non-secret properties.
    """
    target = spec['target']
    if spec['kind'] == 'launchd':
        require(sys.platform == 'darwin', 'manager_platform_mismatch')
        raw = command(['/bin/launchctl', 'print', target]).decode()
        lines = raw.splitlines()
        require(lines and lines[0].strip() == target + ' = {', 'manager_target_mismatch')
        found = {}
        depth = 0
        for line in lines:
            if depth == 1:
                match = re.fullmatch(r'\s*(pid|state|path|runs) = (.*?)\s*', line)
                if match:
                    key, value = match.groups()
                    require(key not in found, 'manager_duplicate_field')
                    found[key] = value
            if line.strip().endswith(' = {'):
                depth += 1
            elif line.strip() == '}':
                depth -= 1
        require(set(('pid', 'state', 'path')).issubset(found), 'manager_identity_missing')
        require(found['pid'].isdigit() and int(found['pid']) > 0, 'manager_pid_invalid')
        require(found['state'] == 'running' and found['path'] == spec['definition'], 'manager_identity_mismatch')
        facts = {'kind': 'launchd', 'target': target, 'pid': int(found['pid']),
                 'state': found['state'], 'definition': found['path'], 'runs': found.get('runs')}
        return facts, canonical(facts), 'filtered_launchctl_identity'
    require(spec['kind'] == 'systemd' and sys.platform.startswith('linux'), 'manager_platform_mismatch')
    names = ('Id', 'MainPID', 'ActiveState', 'SubState', 'InvocationID', 'NRestarts', 'FragmentPath')
    scope = ['--user'] if spec['scope'] == 'user' else ['--system']
    raw = command(['/usr/bin/systemctl', *scope, 'show', target, '--no-pager', '--property=' + ','.join(names)])
    fields = {}
    for line in raw.decode().splitlines():
        key, separator, value = line.partition('=')
        require(separator and key in names and key not in fields, 'manager_output_malformed')
        fields[key] = value
    require(set(fields) == set(names), 'manager_identity_missing')
    require(fields['Id'] == target and fields['FragmentPath'] == spec['definition'], 'manager_target_mismatch')
    require(fields['MainPID'].isdigit() and int(fields['MainPID']) > 0
            and fields['NRestarts'].isdigit(), 'manager_pid_invalid')
    require(fields['ActiveState'] == 'active' and fields['SubState'] == 'running', 'manager_not_running')
    require(bool(re.fullmatch('[0-9a-fA-F]{32}', fields['InvocationID'])), 'manager_invocation_invalid')
    facts = {'kind': 'systemd', 'scope': spec['scope'], 'target': target, 'pid': int(fields['MainPID']),
             'state': 'running', 'definition': fields['FragmentPath'],
             'invocation': fields['InvocationID'], 'restarts': int(fields['NRestarts'])}
    return facts, raw, 'raw_systemctl_selected_properties'


def process_identity(pid, binary=None, *, artifacts=None, name='process'):
    require(type(pid) is int and pid > 0, 'process_pid_invalid')
    raw = command(['/bin/ps', '-p', str(pid), '-o', 'pid=,ppid=,lstart='])
    if artifacts is not None:
        artifacts[name + '-ps.txt'] = (raw, 'raw_ps_identity_without_arguments')
    match = re.fullmatch(r'\s*(\d+)\s+(\d+)\s+(.+?)\s*', raw.decode())
    require(match is not None and int(match[1]) == pid, 'process_unknown')
    facts = {'pid': pid, 'parent_pid': int(match[2]), 'started': match[3], 'precision': 'second'}
    if binary is None:
        return facts
    path = absolute(binary)
    if sys.platform.startswith('linux'):
        executable = Path('/proc') / str(pid) / 'exe'
        link = os.readlink(executable)
        if artifacts is not None:
            artifacts[name + '-exe-link.txt'] = (os.fsencode(link), 'raw_proc_exe_link')
        require(link == str(path), 'process_binary_mismatch')
        live = executable.stat()
    else:
        raw_executable = command(['/usr/sbin/lsof', '-a', '-p', str(pid), '-d', 'txt', '-Ffin'])
        if artifacts is not None:
            artifacts[name + '-lsof.txt'] = (raw_executable, 'raw_lsof_executable_identity')
        output = raw_executable.decode()
        inode = None
        matches = []
        for line in output.splitlines():
            if line.startswith('f'):
                inode = None
            elif line.startswith('i'):
                inode = line[1:]
            elif line == 'n' + str(path):
                matches.append(inode)
        require(len(matches) == 1 and matches[0] == str(path.stat().st_ino), 'process_binary_mismatch')
        live = path.stat()
    disk = path.stat()
    require((live.st_dev, live.st_ino) == (disk.st_dev, disk.st_ino), 'process_binary_changed')
    return {**facts, 'executable': str(path), 'device': live.st_dev, 'inode': live.st_ino}


def stage0_client():
    # The native entry imports this from the verified, root-owned bundle.
    # Importing it, or presenting runtime JSON, never supplies peer authority.
    import routeb_stage0_client
    return routeb_stage0_client


def stage0_registration(profile, root):
    if profile.get('schema') != STAGE0_PROFILE:
        return None
    value = profile.get('stage0')
    require(type(value) is dict and set(value) == {'registration_id'}, 'stage0_profile_invalid')
    identifier = value['registration_id']
    require(type(identifier) is str and re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,47}', identifier)
            and identifier not in ('.', '..') and Path(root).name == 'ags'
            and Path(root).parent.name == identifier, 'stage0_profile_mismatch')
    return value


def _kernel_identity(value):
    names = {'pid', 'start_ticks', 'parent_pid', 'uid', 'gid', 'executable_sha256'}
    require(type(value) is dict and set(value) == names, 'stage0_process_identity_invalid')
    require(all(type(value[key]) is int and value[key] >= 0 for key in
                ('pid', 'start_ticks', 'parent_pid', 'uid', 'gid'))
            and value['pid'] > 1 and value['start_ticks'] > 0, 'stage0_process_identity_invalid')
    digest(value['executable_sha256'])
    return value


def runtime_processes(root, runtime, manager, binary, *, stage0=None, artifacts=None):
    """Bind the fixed root parent, its Python child and that child's AGS process.

    The online root broker reads kernel birth identities. Runtime JSON only
    names the claimed context; it must equal that independent observation.
    The old two-process form remains available for legacy component callers.
    """
    child = process_identity(runtime['pid'], binary, artifacts=artifacts, name='child')
    supervisor = process_identity(runtime['supervisor_pid'], artifacts=artifacts, name='supervisor')
    require(child['parent_pid'] == runtime['supervisor_pid'], 'process_parent_mismatch')
    if stage0 is None:
        require(manager['pid'] == runtime['supervisor_pid'], 'process_parent_mismatch')
        return {'child': child, 'supervisor': supervisor}
    observed = stage0_client().runtime_identity(root)
    require(type(observed) is dict and set(observed) == {'context', 'pids', 'manager_unit'},
            'stage0_runtime_identity_invalid')
    context = observed['context']
    require(type(context) is dict and context == runtime.get('stage0')
            and context.get('schema') == 'ags.routeb-stage0-context.v1'
            and context.get('root') == str(root)
            and context.get('registration_id') == stage0['registration_id']
            and context.get('operation') == 'serve'
            and context.get('phase') in ('serve-initial', 'serve-candidate', 'serve-old')
            and observed['manager_unit'] == manager['target'], 'stage0_runtime_context_mismatch')
    root_parent = _kernel_identity(context.get('supervisor'))
    python = _kernel_identity(context.get('child'))
    require(root_parent['uid'] == root_parent['gid'] == 0 and root_parent['parent_pid'] == 1
            and root_parent['pid'] == manager['pid']
            and python['pid'] == runtime['supervisor_pid']
            and python['parent_pid'] == root_parent['pid']
            and supervisor['parent_pid'] == root_parent['pid']
            and python['uid'] == os.getuid() and python['gid'] == os.getgid(),
            'stage0_process_parent_mismatch')
    require(type(observed['pids']) is list and 3 <= len(observed['pids']) <= 4096,
            'stage0_process_tree_invalid')
    members = {}
    for row in observed['pids']:
        _kernel_identity(row)
        require(row['pid'] not in members, 'stage0_process_tree_invalid')
        members[row['pid']] = row
    require(members.get(root_parent['pid']) == root_parent and members.get(python['pid']) == python,
            'stage0_process_tree_invalid')
    application = members.get(runtime['pid'])
    require(application is not None and application['parent_pid'] == python['pid']
            and application['uid'] == os.getuid() and application['gid'] == os.getgid()
            and application['executable_sha256'] == hash_file(binary), 'stage0_application_mismatch')
    for pid in members:
        seen = set()
        while pid != root_parent['pid']:
            require(pid in members and pid not in seen, 'stage0_process_tree_invalid')
            seen.add(pid)
            pid = members[pid]['parent_pid']
    child.update(start_ticks=application['start_ticks'], uid=application['uid'], gid=application['gid'],
                 executable_sha256=application['executable_sha256'])
    supervisor.update(start_ticks=python['start_ticks'], uid=python['uid'], gid=python['gid'],
                      executable_sha256=python['executable_sha256'])
    if artifacts is not None:
        artifacts['stage0-runtime.json'] = (canonical(observed), 'online_root_broker_runtime_identity')
    stable_tree = {'child': child, 'supervisor': supervisor, 'stage0': context}
    return {**stable_tree, 'stage0_sha256': sha(canonical(context)),
            'stage0_tree_sha256': sha(canonical(stable_tree)), 'pids': observed['pids']}


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


def health(url):
    parsed = urllib.parse.urlsplit(url)
    require(parsed.scheme in ('http', 'https') and parsed.hostname and not parsed.username
            and not parsed.password and parsed.path == '/readyz' and not parsed.query
            and not parsed.fragment, 'health_url_invalid')
    require(parsed.scheme == 'https' or parsed.hostname in ('127.0.0.1', '::1', 'localhost'), 'health_http_not_loopback')
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    try:
        response = opener.open(url, timeout=15)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        raw = response.read(MAX_JSON + 1)
        require(len(raw) <= MAX_JSON, 'health_response_too_large')
        body = object_json(raw)
        return {'http_status': response.code, 'status': body.get('status'), 'revision': body.get('version')}, raw


def validate_startup_archive(receipt_path, receipt):
    """Bind the preserved startup bytes to the first continuous log offset."""
    binding = receipt.get('startup_evidence')
    require(type(binding) is dict and set(binding) == {'path', 'sha256'}
            and binding['path'] == 'startup-evidence/manifest.json', 'startup_manifest_missing')
    path = absolute(str(receipt_path)).parent / binding['path']
    raw = read_regular(path)
    require(sha(raw) == digest(binding['sha256']), 'startup_manifest_changed')
    manifest = object_json(raw)
    require(set(manifest) == {'schema', 'artifacts', 'startup_log_ranges', 'trash_moves'}
            and manifest['schema'] == 'ags.routeb-startup-evidence.v1'
            and type(manifest['artifacts']) is list and bool(manifest['artifacts'])
            and type(manifest['trash_moves']) is list, 'startup_manifest_invalid')
    artifacts = {}
    for row in manifest['artifacts']:
        require(type(row) is dict and set(row) == {'path', 'sha256', 'bytes', 'kind'},
                'startup_artifact_invalid')
        name = row['path']
        require(type(name) is str and re.fullmatch(r'[a-zA-Z0-9_.-]+', name)
                and not name.startswith('.') and name != 'manifest.json' and name not in artifacts
                and type(row['kind']) is str, 'startup_artifact_invalid')
        require(type(row['bytes']) is int and 0 <= row['bytes'] <= MAX_LOG_DELTA,
                'startup_artifact_invalid')
        content = read_regular(path.parent / name, maximum=MAX_LOG_DELTA)
        require(len(content) == row['bytes'] and sha(content) == digest(row['sha256']),
                'startup_artifact_changed')
        artifacts[name] = row
    baselines = {row['name']: row for row in receipt['validated_start']['log_baseline']}
    ranges = manifest['startup_log_ranges']
    require(type(ranges) is list and len(ranges) == len(baselines), 'startup_log_boundary_invalid')
    seen = set()
    for row in ranges:
        require(type(row) is dict and set(row) == {'name', 'device', 'inode', 'start', 'end', 'sha256'},
                'startup_log_boundary_invalid')
        name = row['name']
        require(type(name) is str and name in baselines and name not in seen,
                'startup_log_boundary_invalid')
        seen.add(name)
        require(all(type(row[key]) is int and row[key] >= 0 for key in ('device', 'inode', 'start', 'end'))
                and row['start'] <= row['end'], 'startup_log_boundary_invalid')
        baseline = baselines[name]
        require((row['device'], row['inode'], row['end']) ==
                (baseline['device'], baseline['inode'], baseline['offset']), 'startup_log_gap')
        artifact = artifacts.get('log-' + name + '.bin')
        require(artifact is not None and artifact['kind'] == 'raw_log_delta'
                and artifact['sha256'] == digest(row['sha256'])
                and artifact['bytes'] == row['end'] - row['start'], 'startup_log_artifact_mismatch')
    return raw


def load_inputs(receipt_path, profile_path):
    receipt_raw = read_regular(receipt_path)
    receipt = object_json(receipt_raw)
    require(receipt.get('schema') == 'ags.routeb-host-execution.v1'
            and receipt.get('status') == 'awaiting_observation'
            and receipt.get('artifact_kind') == 'exact-source', 'execution_receipt_invalid')
    profile_raw = read_regular(profile_path, private=False)
    require(sha(profile_raw) == digest(receipt.get('profile_sha256')), 'profile_replaced')
    profile = object_json(profile_raw)
    spec = profile.get('capture')
    require(type(spec) is dict, 'capture_profile_missing')
    root = absolute(receipt['root'])
    native_registration = stage0_registration(profile, root)
    require('stage0_registration' not in spec, 'capture_profile_invalid')
    if native_registration is not None:
        spec = {**spec, 'stage0_registration': native_registration}
    require(root.is_dir(), 'root_missing')
    for key in ('runtime_path', 'installation_path', 'binary_path'):
        path = absolute(spec[key])
        require(root in path.parents, 'capture_path_outside_root')
    require(spec['runtime_path'] == str(root / 'state/runtime.json')
            and spec['installation_path'] == str(root / 'state/installation.json'), 'capture_path_mismatch')
    require(spec['mode'] in ('isolated-native', 'production'), 'capture_mode_invalid')
    manager = spec['manager']
    require(type(manager) is dict and manager.get('kind') in ('launchd', 'systemd'), 'manager_invalid')
    target = manager.get('target')
    require(type(target) is str, 'manager_target_invalid')
    if manager['kind'] == 'launchd':
        require(bool(re.fullmatch(r'(gui|user)/\d+/[a-zA-Z0-9._-]+', target)), 'manager_target_invalid')
        if spec['mode'] == 'isolated-native':
            require(target.rsplit('/', 1)[-1].startswith(PREFIX_LAUNCHD), 'native_label_rejected')
    else:
        require(bool(re.fullmatch(r'[a-zA-Z0-9._-]+\.service', target)), 'manager_target_invalid')
        require(manager.get('scope') in ('user', 'system'), 'manager_scope_invalid')
        if spec['mode'] == 'isolated-native':
            if native_registration is None:
                require(target.startswith(PREFIX_SYSTEMD) and manager['scope'] == 'user', 'native_label_rejected')
            else:
                require(target == PREFIX_SYSTEMD + native_registration['registration_id'] + '.service'
                        and manager['scope'] == 'system', 'native_label_rejected')
    definition = absolute(manager['definition'])
    if spec['mode'] == 'isolated-native':
        if native_registration is None:
            require(root in definition.parents, 'native_definition_outside_root')
        else:
            require(manager['kind'] == 'systemd'
                    and definition == Path('/etc/systemd/system') / target, 'native_definition_outside_registry')
    require(sha(read_regular(definition, private=False)) == digest(manager['definition_sha256']), 'manager_definition_changed')
    for key in ('duration_seconds', 'lease_seconds', 'interval_seconds', 'max_gap_seconds'):
        require(type(spec.get(key)) is int, 'capture_timing_invalid')
    require(600 <= spec['duration_seconds'] <= 86400 and 0 <= spec['lease_seconds'] <= 86400
            and 1 <= spec['interval_seconds'] <= 30
            and spec['interval_seconds'] <= spec['max_gap_seconds'] <= 60, 'capture_timing_invalid')
    require(set(spec['health']) == {'local', 'service'}, 'health_profile_invalid')
    for key in ('revision', 'tree'):
        digest(receipt['candidate'][key], 40)
        digest(receipt['tool'][key], 40)
    for key in ('binary_sha256', 'manifest_sha256', 'version_identity_sha256'):
        digest(receipt['candidate'][key])
    digest(receipt['tool']['bundle_sha256'])
    for key in ('runtime_sha256', 'manager_sha256', 'binary_sha256', 'installation_sha256'):
        digest(receipt['validated_start'][key])
    if native_registration is not None:
        digest(receipt['validated_start'].get('stage0_sha256'))
        digest(receipt['validated_start'].get('stage0_tree_sha256'))
    else:
        require(not any(key in receipt['validated_start'] for key in ('stage0_sha256', 'stage0_tree_sha256')),
                'stage0_profile_missing')
    require(receipt['validated_start']['binary_sha256'] == receipt['candidate']['binary_sha256'], 'candidate_binary_mismatch')
    require(type(receipt['validated_start']['monotonic_ns']) is int and receipt['validated_start']['monotonic_ns'] > 0, 'start_clock_invalid')
    require(receipt['start_once'] == {'attempted': True, 'token_consumed': True, 'manager_requested': True}
            and all(type(value) is bool for value in receipt['start_once'].values()), 'start_once_unproven')
    require(type(spec.get('logs')) is list and bool(spec['logs']), 'logs_missing')
    baselines = receipt['validated_start'].get('log_baseline')
    require(type(baselines) is list and len(baselines) == len(spec['logs']), 'log_baseline_missing')
    baseline_names = set()
    for row in baselines:
        require(type(row) is dict and set(row) == {'name', 'path', 'device', 'inode', 'offset'}, 'log_baseline_invalid')
        require(type(row['name']) is str and row['name'] not in baseline_names, 'log_baseline_invalid')
        baseline_names.add(row['name'])
        require(all(type(row[key]) is int and row[key] >= 0 for key in ('device', 'inode', 'offset')), 'log_baseline_invalid')
    names = set()
    for row in spec['logs']:
        require(type(row) is dict and set(row) == {'name', 'path'}, 'log_profile_invalid')
        name = row['name']
        require(type(name) is str and re.fullmatch(r'[a-zA-Z0-9_-]+', name) and name not in names, 'log_name_invalid')
        names.add(name)
        require(root in absolute(row['path']).parents, 'log_outside_root')
        require(any(item['name'] == name and item['path'] == row['path'] for item in baselines), 'log_baseline_mismatch')
    startup_raw = validate_startup_archive(receipt_path, receipt)
    return receipt, spec, receipt_raw, profile_raw, startup_raw


class NativeProbe:
    def __init__(self, receipt, spec):
        self.receipt, self.spec = receipt, spec
        self.root = Path(receipt['root'])
        self.logs = {}
        try:
            for row in receipt['validated_start']['log_baseline']:
                path = absolute(row['path'])
                fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
                self.logs[row['name']] = {'fd': fd, 'path': path, 'device': row['device'],
                                          'inode': row['inode'], 'start': row['offset'], 'offset': row['offset']}
                info = os.fstat(fd)
                require(stat.S_ISREG(info.st_mode) and info.st_uid in (0, os.getuid())
                        and not info.st_mode & 0o022, 'log_unsafe')
                require((info.st_dev, info.st_ino) == (row['device'], row['inode'])
                        and info.st_size >= row['offset'], 'log_baseline_changed')
        except BaseException:
            self.close()
            raise

    def close(self):
        for row in self.logs.values():
            os.close(row['fd'])
        self.logs = {}

    def sample(self):
        spec, receipt = self.spec, self.receipt
        artifacts = {}
        raw_runtime = read_regular(spec['runtime_path'])
        runtime = object_json(raw_runtime)
        installation = read_regular(spec['installation_path'])
        manager, raw_manager, manager_kind = read_manager(spec['manager'])
        journal = upgrade_state.load_verified(self.root)
        events = journal['events']
        require(bool(events) and journal['projection'] is not None, 'journal_missing')
        require(journal['projection']['sequence'] == receipt['journal']['sequence']
                and journal['projection']['event_sha256'] == receipt['journal']['event_head_sha256'], 'journal_changed')
        require(journal['format']['host_instance'] == receipt['host_instance'], 'journal_host_mismatch')
        require(not journal['projection']['pending_effects'], 'journal_effect_unknown')
        require(runtime.get('schema') == 'ags.runtime.v1' and runtime.get('root') == str(self.root)
                and 'stopped_at' not in runtime, 'runtime_invalid')
        process_tree = runtime_processes(self.root, runtime, manager, spec['binary_path'],
                                         stage0=spec.get('stage0_registration'), artifacts=artifacts)
        observed_boot, raw_boot = boot_identity()
        artifacts['boot.txt'] = (raw_boot, 'raw_os_boot_identity')
        facts = {'boot_id': observed_boot, 'generation': runtime.get('generation'),
                 'revision': runtime.get('build', {}).get('revision'),
                 'runtime_sha256': sha(raw_runtime), 'installation_sha256': sha(installation),
                 'binary_sha256': hash_file(spec['binary_path']), 'manager_sha256': sha(canonical(manager)),
                 'manager': manager, 'child': process_tree['child'], 'supervisor': process_tree['supervisor'],
                 'journal_head_sha256': journal['projection']['event_sha256'], 'health': {}}
        if 'stage0' in process_tree:
            require(process_tree['stage0']['boot_id'] == observed_boot, 'stage0_boot_changed')
            facts.update({key: process_tree[key] for key in ('stage0', 'stage0_sha256', 'stage0_tree_sha256')})
        artifacts.update({'runtime.json': (raw_runtime, 'raw_runtime'),
                     'installation.json': (installation, 'raw_installation'),
                     'manager.txt': (raw_manager, manager_kind),
                     'journal.json': (canonical(journal), 'verified_journal_projection')})
        for key, url in spec['health'].items():
            facts['health'][key], raw = health(url)
            artifacts['health-' + key + '.json'] = (raw, 'raw_readyz')
        require(read_regular(spec['runtime_path']) == raw_runtime, 'runtime_changed_during_probe')
        require(sha(read_regular(spec['manager']['definition'], private=False))
                == spec['manager']['definition_sha256'], 'manager_definition_changed')
        return facts, artifacts

    def log_delta(self):
        artifacts, ranges = {}, []
        for name, row in self.logs.items():
            current = row['path'].lstat()
            info = os.fstat(row['fd'])
            require((current.st_dev, current.st_ino) == (row['device'], row['inode'])
                    and stat.S_ISREG(current.st_mode), 'log_rotated')
            require(info.st_size >= row['offset'], 'log_truncated')
            size = info.st_size - row['offset']
            require(size <= MAX_LOG_DELTA, 'log_delta_too_large')
            os.lseek(row['fd'], row['offset'], os.SEEK_SET)
            chunks, remaining = [], size
            while remaining:
                chunk = os.read(row['fd'], min(65536, remaining))
                require(bool(chunk), 'log_short_read')
                chunks.append(chunk)
                remaining -= len(chunk)
            raw = b''.join(chunks)
            after = row['path'].lstat()
            require((after.st_dev, after.st_ino) == (row['device'], row['inode'])
                    and after.st_size >= info.st_size, 'log_changed_during_read')
            ranges.append({'name': name, 'device': row['device'], 'inode': row['inode'],
                           'start': row['offset'], 'end': info.st_size, 'sha256': sha(raw)})
            artifacts['log-' + name + '.bin'] = (raw, 'raw_log_delta')
            row['offset'] = info.st_size
        return artifacts, ranges


class Clock:
    monotonic_ns = staticmethod(time.monotonic_ns)
    sleep = staticmethod(time.sleep)


def validate_sample(facts, receipt, previous=None):
    require(facts['boot_id'] == receipt['boot_id'], 'boot_changed')
    require(facts['generation'] == receipt['generation'], 'generation_changed')
    require(facts['revision'] == receipt['candidate']['revision'], 'revision_changed')
    for key in ('runtime_sha256', 'installation_sha256', 'binary_sha256', 'manager_sha256'):
        require(facts[key] == receipt['validated_start'][key], key + '_changed')
    for key in ('stage0_sha256', 'stage0_tree_sha256'):
        require(facts.get(key) == receipt['validated_start'].get(key), 'stage0_identity_changed')
    require(set(facts['health']) == {'local', 'service'}, 'health_profile_invalid')
    for value in facts['health'].values():
        require(value['http_status'] == 200 and value['status'] == 'ready'
                and value['revision'] == receipt['candidate']['revision'], 'health_not_ready')
    if previous is not None:
        for key in ('child', 'supervisor', 'manager', 'stage0'):
            require(facts.get(key) == previous.get(key), 'process_identity_changed')


def capture(receipt_path, profile_path, output, *, probe=None, clock=None):
    receipt, spec, receipt_raw, profile_raw, startup_raw = load_inputs(receipt_path, profile_path)
    injected = probe is not None or clock is not None
    clock = clock or Clock()
    now = clock.monotonic_ns()
    initial_gap = now - receipt['validated_start']['monotonic_ns']
    require(initial_gap >= 0, 'start_clock_in_future')
    require(initial_gap <= spec['max_gap_seconds'] * 1_000_000_000, 'first_sample_gap')
    archive = Archive(output)
    archive.write('execution.json', receipt_raw, 'input_execution')
    archive.write('profile.json', profile_raw, 'input_profile')
    archive.write('startup-manifest.json', startup_raw, 'input_startup_manifest')
    report = {'schema': SCHEMA, 'kind': 'fixture' if injected else 'native', 'mode': spec['mode'],
              'status': 'interrupted', 'accepted': False, 'installable': False,
              'host': receipt['host'], 'host_instance': receipt['host_instance'],
              'transaction_id': receipt['transaction_id'], 'root': receipt['root'],
              'generation': receipt['generation'], 'boot_id': receipt['boot_id'],
              'candidate': receipt['candidate'], 'tool': receipt['tool'],
              'execution_sha256': sha(receipt_raw), 'profile_sha256': sha(profile_raw),
              'startup_manifest_sha256': sha(startup_raw),
              'collector_sha256': sha(Path(__file__).read_bytes()),
              'platform': {'system': platform.system(), 'machine': platform.machine()},
              'validated_start_monotonic_ns': receipt['validated_start']['monotonic_ns'],
              'initial_gap_ns': initial_gap,
              'started_at': datetime.now(timezone.utc).isoformat(), 'samples': [],
              'required_seconds': max(600, spec['duration_seconds'], spec['lease_seconds']),
              'max_gap_seconds': spec['max_gap_seconds']}
    live = None
    try:
        live = probe if probe is not None else NativeProbe(receipt, spec)
        previous = None
        first_finished = None
        previous_started = None
        while True:
            started = clock.monotonic_ns()
            if previous_started is None:
                require(0 <= started - receipt['validated_start']['monotonic_ns']
                        <= spec['max_gap_seconds'] * 1_000_000_000, 'first_sample_gap')
            else:
                require(0 <= started - previous_started <= spec['max_gap_seconds'] * 1_000_000_000, 'sample_gap')
            facts, artifacts = live.sample()
            logs, ranges = live.log_delta()
            artifacts.update(logs)
            index = len(report['samples'])
            saved = {name: archive.write(f'{index:05d}-' + name, raw, kind)
                     for name, (raw, kind) in artifacts.items()}
            finished = clock.monotonic_ns()
            require(finished >= started, 'clock_reversed')
            require(finished - started <= spec['max_gap_seconds'] * 1_000_000_000, 'probe_too_slow')
            sample = {'started_monotonic_ns': started, 'finished_monotonic_ns': finished,
                      'facts': facts, 'artifacts': saved, 'log_ranges': ranges}
            report['samples'].append(sample)
            archive.write(f'{index:05d}-sample.json', canonical(sample), 'sample')
            validate_sample(facts, receipt, previous)
            require(read_regular(receipt_path) == receipt_raw and read_regular(profile_path, private=False) == profile_raw,
                    'capture_input_replaced')
            if first_finished is None:
                first_finished = finished
                report['started_monotonic_ns'] = first_finished
            if started - first_finished >= report['required_seconds'] * 1_000_000_000:
                require(validate_startup_archive(receipt_path, receipt) == startup_raw,
                        'startup_manifest_changed')
                report['finished_monotonic_ns'] = started
                report['elapsed_ns'] = started - first_finished
                report['status'] = 'observed'
                break
            previous, previous_started = facts, started
            clock.sleep(spec['interval_seconds'])
    except Exception as error:
        report['reason'] = error.code if isinstance(error, CaptureError) else type(error).__name__
    finally:
        if live is not None:
            live.close()
        report['finished_at'] = datetime.now(timezone.utc).isoformat()
        report['artifacts'] = list(archive.artifacts)
        archive.write('capture.json', canonical(report), 'capture')
    return report


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--receipt', required=True)
    parser.add_argument('--profile', required=True)
    parser.add_argument('--output', required=True)
    args = parser.parse_args(argv)
    try:
        report = capture(args.receipt, args.profile, args.output)
        print(json.dumps({'status': report['status'], 'kind': report['kind'],
                          'samples': len(report['samples']), 'reason': report.get('reason'),
                          'accepted': False, 'installable': False, 'output': args.output}))
        return 0 if report['status'] == 'observed' else 78
    except (CaptureError, OSError, KeyError, TypeError) as error:
        print('code=' + (error.code if isinstance(error, CaptureError) else type(error).__name__), file=sys.stderr)
        return 78


if __name__ == '__main__':
    sys.exit(main())
