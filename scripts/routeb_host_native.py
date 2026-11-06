#!/usr/bin/env python3
"""Fixed native capabilities for an already provisioned Route B host.

No shell, manager discovery, registration, sudoers changes or automatic retry.
Construction has no effects. A missing generation, unknown process, unobserved
holder or forced exit refuses the cold window rather than inventing closure.
"""
import os
from pathlib import Path
import plistlib
import pwd
import re
import shlex
import stat
import subprocess
import sys
import time
from urllib.parse import urlsplit

from upgrade_backup_contract import ContractError, require


def capture_module():
    import routeb_native_capture
    return routeb_native_capture


def run(argv, *, timeout=10, allowed=(0,)):
    env = {'PATH': '/usr/bin:/bin:/usr/sbin:/sbin', 'LC_ALL': 'C'}
    if argv[0] == '/usr/bin/systemctl' and '--user' in argv:
        env['XDG_RUNTIME_DIR'] = '/run/user/' + str(os.getuid())
        env['DBUS_SESSION_BUS_ADDRESS'] = 'unix:path=' + env['XDG_RUNTIME_DIR'] + '/bus'
    try:
        result = subprocess.run(argv, capture_output=True, check=False, timeout=timeout, env=env)
    except (OSError, subprocess.SubprocessError):
        raise ContractError('native_command_unknown') from None
    require(result.returncode in allowed and len(result.stdout) < 1024 * 1024
            and len(result.stderr) < 65536, 'native_command_failed')
    return result


def processes():
    result = run(['/bin/ps', '-axo', 'pid=,ppid='])
    require(not result.stderr, 'process_enumeration_unknown')
    rows = {}
    for line in result.stdout.decode('ascii', errors='strict').splitlines():
        parts = line.split()
        require(len(parts) == 2 and all(part.isdigit() for part in parts), 'process_enumeration_unknown')
        pid, parent = map(int, parts)
        require(pid > 0 and pid not in rows, 'process_enumeration_unknown')
        rows[pid] = parent
    require(os.getpid() in rows, 'process_enumeration_unknown')
    return rows


def descendants(rows, supervisor):
    found = {supervisor}
    changed = True
    while changed:
        added = {pid for pid, parent in rows.items() if parent in found} - found
        changed = bool(added)
        found.update(added)
    return found


def holder_writes(fields):
    """Parse lsof -Fpa records, including its mandatory descriptor field."""
    process = None
    access = None
    descriptor = None
    count = 0
    def finish():
        nonlocal count
        if descriptor is not None:
            require(access in ('r', 'w', 'u'), 'writer_probe_unknown')
            count += access in ('w', 'u')
    for line in fields:
        if re.fullmatch(r'p\d+', line):
            require(process is None or descriptor is not None, 'writer_probe_unknown')
            finish()
            process, descriptor, access = int(line[1:]), None, None
            require(process > 0, 'writer_probe_unknown')
        elif line.startswith('f') and line[1:]:
            require(process is not None, 'writer_probe_unknown')
            finish()
            descriptor, access = line[1:], None
        elif re.fullmatch(r'a[rwu]', line):
            require(descriptor is not None and access is None, 'writer_probe_unknown')
            access = line[1:]
        else:
            raise ContractError('writer_probe_unknown')
    finish()
    require(not fields or descriptor is not None, 'writer_probe_unknown')
    return count


def absent(pid):
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return True
    except OSError:
        raise ContractError('process_identity_unknown') from None
    return False


def systemd_properties(spec, names):
    scope = '--user' if spec['scope'] == 'user' else '--system'
    result = run(['/usr/bin/systemctl', scope, 'show', spec['target'], '--no-pager',
                  '--property=' + ','.join(names)])
    fields = {}
    for line in result.stdout.decode('utf-8').splitlines():
        key, separator, value = line.partition('=')
        require(separator and key in names and key not in fields, 'manager_output_malformed')
        fields[key] = value
    require(set(fields) == set(names), 'manager_output_malformed')
    return fields


def launchd_fields(raw):
    """Only top-level lifecycle facts; never return launchctl environment text."""
    found, depth = {}, 0
    for line in raw.decode('utf-8').splitlines():
        if depth == 1:
            match = re.fullmatch(r'\s*(pid|state|last exit code|last terminating signal) = (.*?)\s*', line)
            if match:
                key, value = match.groups()
                require(key not in found, 'manager_output_malformed')
                found[key] = value
        if line.strip().endswith(' = {'):
            depth += 1
        elif line.strip() == '}':
            depth -= 1
    return found


class FixedManager:
    def __init__(self, root, profile, host):
        self.root = Path(root)
        self.profile, self.host = profile, host
        self.spec = profile['capture']['manager']
        self.before = None
        self.called = set()

    def definition(self):
        cap = capture_module()
        raw = cap.read_regular(self.spec['definition'], private=False)
        require(cap.sha(raw) == self.spec['definition_sha256'], 'manager_definition_changed')
        expected = [self.profile['interpreter']['path'], '-I', '-B', str(self.root / 'bin/ags-runtime.py'),
                    '--root', str(self.root), 'serve']
        if self.spec['kind'] == 'launchd':
            value = plistlib.loads(raw)
            require(value.get('Label') == self.spec['target'].rsplit('/', 1)[-1]
                    and value.get('ProgramArguments') == expected
                    and value.get('KeepAlive') is False and value.get('RunAtLoad') is True
                    and type(value.get('ExitTimeOut')) is int and value['ExitTimeOut'] >= 90,
                    'oneshot_manager_definition_required')
        else:
            # Unit syntax and effective manager properties must agree. Drop-ins
            # or ExecStart hooks are not admitted by this initial fixed profile.
            entries = {}
            section = None
            for line in raw.decode('utf-8').splitlines():
                line = line.strip()
                if not line or line.startswith(('#', ';')):
                    continue
                if line.startswith('[') and line.endswith(']'):
                    section = line[1:-1]
                    continue
                key, separator, value = line.partition('=')
                require(separator and '\\' not in line and section is not None,
                        'manager_definition_unsupported')
                if section == 'Service':
                    require(key not in entries, 'manager_definition_unsupported')
                    entries[key] = value
            require(shlex.split(entries.get('ExecStart', '')) == expected
                    and entries.get('Restart') == 'no' and entries.get('KillMode') == 'control-group'
                    and entries.get('Type', 'simple') == 'simple'
                    and entries.get('TimeoutStopSec') in ('90', '90s', '95', '95s', '120', '120s')
                    and not any(key.startswith('Exec') and key != 'ExecStart' for key in entries),
                    'oneshot_manager_definition_required')
            facts = systemd_properties(self.spec, ('DropInPaths', 'Restart', 'KillMode', 'TimeoutStopUSec', 'FragmentPath'))
            require(facts['DropInPaths'] == '' and facts['Restart'] == 'no'
                    and facts['KillMode'] == 'control-group'
                    and facts['FragmentPath'] == self.spec['definition'], 'manager_definition_changed')
        return raw

    def preflight(self):
        cap = capture_module()
        self.host.recheck()
        self.definition()
        runtime = cap.object_json(cap.read_regular(self.root / 'state/runtime.json'))
        manager, _, _ = cap.read_manager(self.spec)
        require(runtime.get('root') == str(self.root) and runtime.get('generation')
                and 'stopped_at' not in runtime and manager['pid'] == runtime.get('supervisor_pid'),
                'generation_launcher_required')
        child = cap.process_identity(runtime['pid'], self.root / 'bin/gh-server')
        require(child['parent_pid'] == runtime['supervisor_pid'], 'process_parent_mismatch')
        self.before = {'runtime': runtime, 'manager': manager, 'boot_id': cap.boot_identity()[0],
                       'pids': descendants(processes(), runtime['supervisor_pid'])}
        return self.before

    def suppress_restart(self):
        # Restart=no / KeepAlive=false is verified before the stop. This method
        # cannot alter a manager definition or obtain new privileges.
        require(self.before is not None, 'native_preflight_required')
        self.definition()
        return {'suppressed': True}

    def _once(self, action):
        require(action not in self.called, action + '_already_requested')
        self.called.add(action)
        self.host.recheck()
        self.definition()

    def _systemd(self, action):
        require(action in ('stop', 'start'), 'manager_action_invalid')
        if self.host.scope == 'production':
            require(self.spec['kind'] == 'systemd' and self.spec['scope'] == 'system'
                    and self.spec['target'] == 'agent-git-service.service', 'manager_target_invalid')
            # The installed maintenance helper already admits exactly these
            # two commands. No mask, timer or generic root command is requested.
            argv = ['sudo', '-n', 'systemctl', action, 'agent-git-service.service']
        else:
            require(self.spec['scope'] == 'user', 'manager_scope_invalid')
            argv = ['/usr/bin/systemctl', '--user', action, self.spec['target']]
        return run(argv, timeout=125)

    def stop(self):
        from routeb_host_package import require_interpreter_closure
        require_interpreter_closure()
        require(self.before is not None, 'native_preflight_required')
        self._once('stop')
        if self.spec['kind'] == 'systemd':
            self._systemd('stop')
        else:
            run(['/bin/launchctl', 'kill', 'SIGTERM', self.spec['target']])
        cap = capture_module()
        deadline = time.monotonic() + 95
        exit_observed = False
        while time.monotonic() < deadline:
            if self.spec['kind'] == 'systemd':
                row = systemd_properties(self.spec, ('MainPID', 'ActiveState', 'SubState', 'ExecMainCode', 'ExecMainStatus'))
                exit_observed = (row['MainPID'] == '0' and row['ActiveState'] == 'inactive'
                                 and row['SubState'] == 'dead' and row['ExecMainCode'] == '1'
                                 and row['ExecMainStatus'] == '0')
            else:
                row = launchd_fields(run(['/bin/launchctl', 'print', self.spec['target']]).stdout)
                exit_observed = (row.get('state') in ('not running', 'exited') and 'pid' not in row
                                 and row.get('last exit code') == '0'
                                 and row.get('last terminating signal') in (None, '0'))
            if exit_observed and all(absent(pid) for pid in self.before['pids']):
                break
            time.sleep(.2)
        require(exit_observed and all(absent(pid) for pid in self.before['pids']), 'normal_stop_unproven')
        require(cap.boot_identity()[0] == self.before['boot_id'], 'boot_changed')
        raw = cap.read_regular(self.root / 'state/runtime.json')
        stopped = cap.object_json(raw)
        require(stopped.get('generation') == self.before['runtime']['generation']
                and stopped.get('root') == str(self.root)
                and stopped.get('exit_code') == 0 and type(stopped.get('exit_code')) is int
                and stopped.get('forced_kill') is False and stopped.get('stop_requested') is True
                and stopped.get('process_group_empty') is True, 'normal_stop_unproven')
        if self.spec['kind'] == 'launchd':
            run(['/bin/launchctl', 'bootout', self.spec['target']])
            gone = run(['/bin/launchctl', 'print', self.spec['target']], allowed=(113,))
            require(not gone.stdout and b'Could not find service' in gone.stderr, 'manager_stop_unknown')
        return {**stopped, 'supervisor_exited': True, 'supervisor_exit_code': 0,
                'runtime_sha256': cap.sha(raw)}

    def start(self):
        from routeb_host_package import require_interpreter_closure
        require_interpreter_closure()
        self._once('start')
        if self.spec['kind'] == 'systemd':
            self._systemd('start')
        else:
            domain = self.spec['target'].rsplit('/', 1)[0]
            run(['/bin/launchctl', 'bootstrap', domain, self.spec['definition']])
        return {'requested': True}


class WriterProbe:
    def __init__(self, manager, group, generation):
        self.manager, self.group, self.generation = manager, group, generation
        self.lsof = '/usr/sbin/lsof' if sys.platform == 'darwin' else '/usr/bin/lsof'
        require(Path(self.lsof).is_file(), 'writer_probe_unavailable')
        parsed = urlsplit(manager.profile['capture']['health']['local'])
        require(parsed.hostname in ('127.0.0.1', 'localhost', '::1') and parsed.port,
                'local_health_endpoint_invalid')
        self.port = parsed.port

    def _query(self, arguments):
        result = run([self.lsof, '-nP', *arguments], allowed=(0, 1))
        require(not result.stderr and (result.returncode == 0 or not result.stdout), 'writer_probe_unknown')
        return result.stdout.decode('utf-8', errors='strict').splitlines()

    def __call__(self):
        cap = capture_module()
        require(self.manager.before is not None and cap.boot_identity()[0] == self.manager.before['boot_id'], 'boot_changed')
        current = processes()
        writers = []
        if self.manager.before['pids'] & current.keys():
            writers.append({'kind': 'descendant', 'count': len(self.manager.before['pids'] & current.keys())})
        listeners = self._query(['-iTCP:' + str(self.port), '-sTCP:LISTEN', '-Fp'])
        require(all(re.fullmatch(r'(p[1-9]\d*|f\d+)', line) for line in listeners), 'writer_probe_unknown')
        if listeners:
            count = sum(line.startswith('f') for line in listeners)
            require(listeners[0].startswith('p') and count > 0, 'writer_probe_unknown')
            writers.append({'kind': 'listener', 'count': count})
        for item in self.group['objects']:
            source = Path(item['source'])
            arguments = ['-Fpa', '+D', str(source)] if source.is_dir() else ['-Fpa', '--', str(source)]
            fields = self._query(arguments)
            count = holder_writes(fields)
            if count:
                writers.append({'kind': 'holder', 'object': item['id'], 'count': count})
        return {'generation': self.generation(), 'closed': not writers, 'writers': writers, 'unknown': []}


class SystemTrash:
    """Move only this transaction's verified restore copy to the OS Trash.

    Same-volume inode readback proves the move. Unsupported Trash locations or
    command availability refuse before stop; an unknown move is never retried.
    """
    def __init__(self, restore_root, trash_root):
        self.restore_root, self.trash_root = Path(restore_root), Path(trash_root)
        if sys.platform == 'darwin':
            self.argv = ['/usr/bin/trash']
            expected = Path(pwd.getpwuid(os.getuid()).pw_dir) / '.Trash'
        else:
            self.argv = ['/usr/bin/gio', 'trash', '--']
            expected = Path(pwd.getpwuid(os.getuid()).pw_dir) / '.local/share/Trash/files'
        require(self.trash_root == expected and self.trash_root.resolve() == self.trash_root
                and self.trash_root.is_dir() and self.trash_root.stat().st_uid == os.getuid()
                and not self.trash_root.stat().st_mode & 0o077
                and self.trash_root.stat().st_dev == self.restore_root.stat().st_dev
                and Path(self.argv[0]).is_file(), 'trash_unavailable')
        self.receipts = []

    def __call__(self, path):
        path = Path(path)
        require(path.parent == self.restore_root and path.is_dir() and not path.is_symlink(),
                'restore_attempt_invalid')
        info = path.stat()
        require(info.st_uid == os.getuid() and info.st_dev == self.trash_root.stat().st_dev,
                'trash_unavailable')
        before = {item.name for item in self.trash_root.iterdir()}
        run([*self.argv, str(path)], timeout=30)
        matches = []
        for item in self.trash_root.iterdir():
            if item.name in before or item.is_symlink():
                continue
            observed = item.stat()
            if (observed.st_dev, observed.st_ino) == (info.st_dev, info.st_ino):
                matches.append(item)
        require(not path.exists() and not path.is_symlink() and len(matches) == 1,
                'trash_readback_unknown')
        result = {'status': 'trashed', 'entry': str(matches[0]), 'device': info.st_dev, 'inode': info.st_ino}
        self.receipts.append(result)
        return result
