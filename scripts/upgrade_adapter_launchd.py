#!/usr/bin/env python3
"""Fixed launchd actions for one reviewed service. No label search.

create() does not stop or start anything. Native proof is a separate probe and
is unverified when permission is absent. This is not a production activation.
"""
import sys
sys.dont_write_bytecode = True
sys.pycache_prefix = None
import os
import subprocess

SCHEMA = 'ags.upgrade-host-adapter.v1'
ABSENT_EXIT = 113


class AdapterError(ValueError):
    def __init__(self, reason, exit_code=83):
        super().__init__(reason)
        self.code = reason
        self.exit_code = exit_code


def allow(argv, service):
    """Only the exact domain/label forms. No shell and no extra arguments."""
    domain = service['domain'] + '/' + service['label']
    permitted = (
        ['launchctl', 'print-disabled', service['domain']],
        ['launchctl', 'disable', domain],
        ['launchctl', 'bootout', domain],
        ['launchctl', 'bootstrap', service['domain'], service['label'] + '.plist'],
        ['launchctl', 'bootstrap', service['domain'], service.get('plist', service['label'] + '.plist')],
    )
    if argv not in permitted:
        raise AdapterError('argv_rejected')
    return tuple(argv)


class Manager:
    def __init__(self, service, runner=None, observe=None, enumerate_descendants=None, read_identity=None):
        self.service = service
        self.runner = runner
        self.observe = observe or runner
        self.enumerate_descendants = enumerate_descendants
        self.read_identity = read_identity
        self.consumed = False

    def _complete(self, result, *, allow_absent=False):
        if not isinstance(result, dict):
            raise AdapterError('command_not_observed')
        exit_code = result.get('exit')
        if exit_code != 0 and not (allow_absent and exit_code == ABSENT_EXIT):
            raise AdapterError('command_not_observed')
        if result.get('truncated') is True:
            raise AdapterError('output_truncated')
        stdout = result.get('stdout', '')
        stderr = result.get('stderr', '')
        if not isinstance(stdout, str) or not isinstance(stderr, str):
            raise AdapterError('output_truncated')
        if len(stdout) >= 4096 or len(stderr) >= 4096:
            raise AdapterError('output_truncated')
        return result

    def _job_missing(self, printed):
        """Only the real launchctl absent response. Empty exit 0 is not absence."""
        if printed.get('exit') != ABSENT_EXIT:
            return False
        text = printed.get('stderr', '')
        if 'Could not find service' not in text:
            return False
        if printed.get('stdout', '').strip():
            return False
        return True

    def _enumerate(self):
        """List descendants of the named supervisor and child. Failure is unknown."""
        named = []
        for key in ('supervisor_pid', 'child_pid', 'pid'):
            value = self.service.get(key)
            if type(value) is int and value > 0:
                named.append(value)
        if not named:
            return []
        try:
            listed = subprocess.run(['ps', '-axo', 'pid=,ppid='], check=False,
                                    capture_output=True, text=True, timeout=5)
        except (OSError, subprocess.TimeoutExpired):
            raise AdapterError('descendants_unobserved') from None
        if listed.returncode != 0 or not isinstance(listed.stdout, str):
            raise AdapterError('descendants_unobserved')
        children = {}
        alive = set()
        for line in listed.stdout.splitlines():
            parts = line.split()
            if len(parts) != 2 or not parts[0].isdigit() or not parts[1].isdigit():
                continue
            pid, parent = int(parts[0]), int(parts[1])
            alive.add(pid)
            children.setdefault(parent, []).append(pid)
        found = []
        pending = [pid for pid in named if pid in alive]
        seen = set()
        while pending:
            current = pending.pop()
            if current in seen:
                continue
            seen.add(current)
            found.append(current)
            pending.extend(children.get(current, []))
        return found

    def _identity(self):
        """Read this process's boot and start time. Never copy the caller's claim."""
        try:
            boot = subprocess.run(['sysctl', '-n', 'kern.boottime'], check=False,
                                  capture_output=True, text=True, timeout=5)
            started = subprocess.run(['ps', '-o', 'lstart=', '-p', str(os.getpid())],
                                     check=False, capture_output=True, text=True, timeout=5)
        except (OSError, subprocess.TimeoutExpired):
            raise AdapterError('identity_unobserved') from None
        if boot.returncode != 0 or started.returncode != 0:
            raise AdapterError('identity_unobserved')
        boot_text = boot.stdout.strip()
        start_text = started.stdout.strip()
        if not boot_text or not start_text:
            raise AdapterError('identity_unobserved')
        return boot_text, start_text

    def _run(self, argv):
        if not callable(self.runner):
            raise AdapterError('native_runner_missing')
        allow(argv, self.service)
        return self.runner(argv)

    def _disabled(self):
        domain = self.service['domain']
        readback = self._complete(self._run(['launchctl', 'print-disabled', domain]))
        label = self.service['label']
        disabled = f'"{label}" => disabled' in readback.get('stdout', '')
        enabled = f'"{label}" => enabled' in readback.get('stdout', '')
        if not disabled or enabled:
            raise AdapterError('disabled_not_observed')
        return True

    def _job_absent(self):
        domain = self.service['domain'] + '/' + self.service['label']
        printed = self._complete(self.observe(['launchctl', 'print', domain]), allow_absent=True)
        if self._job_missing(printed):
            return True
        if printed.get('exit') != 0:
            raise AdapterError('command_not_observed')
        text = printed.get('stdout', '') + printed.get('stderr', '')
        if not text.strip():
            raise AdapterError('job_absence_unproven')
        raise AdapterError('stop_not_closed')

    def suppress_restart(self, readback=None):
        if readback is not None:
            raise AdapterError('external_facts_rejected')
        domain = self.service['domain'] + '/' + self.service['label']
        self._complete(self._run(['launchctl', 'disable', domain]))
        self._disabled()
        return {'disabled': True, 'boot_id': self.service.get('boot_id')}

    def stop(self, facts=None):
        if facts is not None:
            raise AdapterError('external_facts_rejected')
        domain = self.service['domain'] + '/' + self.service['label']
        self._complete(self._run(['launchctl', 'bootout', domain]))
        try:
            absent = self._job_absent()
        except AdapterError as error:
            if error.code == 'job_absence_unproven':
                raise
            absent = False
        self._disabled()
        descendants_known = True
        if self.enumerate_descendants is not None:
            descendants = self.enumerate_descendants()
            if not isinstance(descendants, list):
                raise AdapterError('descendants_unobserved')
        else:
            try:
                descendants = self._enumerate()
            except AdapterError:
                descendants, descendants_known = None, False
        identity_known = True
        if self.read_identity is not None:
            observed = self.read_identity()
            if not isinstance(observed, tuple) or len(observed) != 2:
                raise AdapterError('identity_unobserved')
            boot_id, start_time = observed
        else:
            try:
                boot_id, start_time = self._identity()
            except AdapterError:
                boot_id, start_time, identity_known = None, None, False
        claimed_boot = self.service.get('boot_id')
        claimed_start = self.service.get('start_time')
        identity_claimed = 'boot_id' in self.service or 'start_time' in self.service
        identity_matches = (identity_known and (not identity_claimed
                            or (claimed_boot == boot_id and claimed_start == start_time)))
        empty = absent and descendants_known and not descendants and identity_matches
        return {'closed': empty, 'disabled': True, 'process_group_empty': empty,
                'descendants': descendants, 'boot_id': boot_id, 'start_time': start_time,
                'identity_matches': identity_matches, 'identity_known': identity_known}

    def start(self, readback=None):
        if readback is not None:
            raise AdapterError('external_facts_rejected')
        if self.consumed:
            raise AdapterError('oneshot_consumed')
        root = self.service.get('root')
        epoch = self.service.get('epoch')
        if root and epoch:
            from upgrade_recovery import Failure, launcher_spawn_authorized
            def runner(_argv):
                return self._bootstrap()
            try:
                observed = launcher_spawn_authorized(root, runner, epoch=epoch)
            except Failure as error:
                self.consumed = True
                raise AdapterError(error.code) from None
            self.consumed = True
            return {'started': True, 'readback': observed}
        try:
            result = self._bootstrap()
        except Exception:
            self.consumed = True
            raise
        self.consumed = True
        return {'started': True, 'readback': result}

    def _bootstrap(self):
        plist = self.service.get('plist') or (self.service['label'] + '.plist')
        try:
            result = self._run(['launchctl', 'bootstrap', self.service['domain'], plist])
        except AdapterError:
            raise
        except Exception:
            raise AdapterError('start_not_observed') from None
        if not isinstance(result, dict) or result.get('exit') != 0:
            raise AdapterError('start_not_observed')
        return result


def native_probe(service, *, permitted, label=None, runner=None, one_shot=True):
    """Run the fixed bootstrap/print/bootout sequence only when explicitly permitted.

    The runner is injected. This function does not call launchctl itself. A
    successful injected sequence is still native_os_unverified.
    """
    if not permitted:
        return {'verified': False, 'reason': 'permission_unavailable'}
    if not one_shot or not label or 'once' not in label or label != service.get('label'):
        raise AdapterError('label_not_one_shot')
    if not callable(runner):
        return {'verified': False, 'reason': 'native_sequence_not_run'}
    domain = service['domain'] + '/' + label
    plist = service.get('plist')
    if not isinstance(plist, str) or not plist.startswith('/') or not plist.endswith('.plist'):
        raise AdapterError('plist_not_absolute')
    steps = (
        ['launchctl', 'bootstrap', service['domain'], plist],
        ['launchctl', 'print', domain],
        ['launchctl', 'bootout', domain],
    )
    rollback = (['launchctl', 'bootout', domain],)
    try:
        from upgrade_native_runner import run_sequence, RunnerError
        done = run_sequence(runner, steps, rollback=rollback)
    except RunnerError as error:
        raise AdapterError('native_probe_failed') from error
    printed = next((item for argv, item in done if argv[1] == 'print'), None)
    readback = None
    if isinstance(printed, dict) and isinstance(printed.get('stdout'), str):
        from upgrade_native_parse import parse_launchctl_print, ParseError
        try:
            readback = parse_launchctl_print(printed['stdout'])
        except ParseError as error:
            raise AdapterError('manager_output_malformed') from error
        if readback['plist'] != plist:
            raise AdapterError('plist_mismatch')
    return {'verified': False, 'reason': 'native_os_unverified',
            'sequence': [step[1] for step in steps], 'label': label,
            'plist': plist, 'readback': readback}


def create(root, package):
    """Build a fixed, injected native capability; no implicit launchctl call."""
    profile = package.get('native_profile') if isinstance(package, dict) else None
    runner = package.get('native_runner') if isinstance(package, dict) else None
    if not profile or not callable(runner):
        from upgrade_backup_contract import ContractError
        raise ContractError('native_adapter_unavailable')
    service = dict(profile)
    service.setdefault('root', str(root))
    manager = Manager(service, runner=runner)
    def writer_probe():
        return runner(['launchctl', 'print', service['domain'] + '/' + service['label']])
    def trash(path):
        return runner(['/usr/bin/trash', str(path)])
    return {'manager': manager, 'writer_probe': writer_probe, 'trash': trash,
            'native_verified': False, 'unverified_reason': 'native_probe_not_run'}
