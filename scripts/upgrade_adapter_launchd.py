#!/usr/bin/env python3
"""Fixed launchd actions for one reviewed service. No label search.

create() does not stop or start anything. Native proof is a separate probe and
is unverified when permission is absent. This is not a production activation.
"""
import sys
sys.dont_write_bytecode = True
sys.pycache_prefix = None

SCHEMA = 'ags.upgrade-host-adapter.v1'


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
    def __init__(self, service, runner=None, observe=None):
        self.service = service
        self.runner = runner
        self.observe = observe or runner
        self.consumed = False

    def _complete(self, result):
        if not isinstance(result, dict) or result.get('exit') != 0:
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
        printed = self._complete(self.observe(['launchctl', 'print', domain]))
        text = printed.get('stdout', '') + printed.get('stderr', '')
        if 'pid =' in text or 'state = running' in text:
            raise AdapterError('stop_not_closed')
        if 'Could not find service' not in text and text.strip():
            raise AdapterError('stop_not_closed')
        return True

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
        self._job_absent()
        self._disabled()
        return {'closed': True, 'disabled': True, 'process_group_empty': True,
                'boot_id': self.service.get('boot_id'), 'start_time': self.service.get('start_time')}

    def start(self, readback=None):
        if readback is not None:
            raise AdapterError('external_facts_rejected')
        if self.consumed:
            raise AdapterError('oneshot_consumed')
        plist = self.service.get('plist') or (self.service['label'] + '.plist')
        result = self._run(['launchctl', 'bootstrap', self.service['domain'], plist])
        if not isinstance(result, dict) or result.get('exit') != 0:
            raise AdapterError('start_not_observed')
        self.consumed = True
        return {'started': True, 'readback': result}


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
