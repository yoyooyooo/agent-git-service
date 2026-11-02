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
    )
    if argv not in permitted:
        raise AdapterError('argv_rejected')
    return tuple(argv)


class Manager:
    def __init__(self, service, runner=None):
        self.service = service
        self.runner = runner
        self.consumed = False

    def suppress_restart(self, readback):
        if readback.get('disabled') is not True or readback.get('pid') is not None:
            raise AdapterError('disable_not_read_back')
        return {'disabled': True}

    def stop(self, facts):
        closed = (facts.get('command_exit') == 0 and not facts.get('timed_out')
                  and facts.get('forced_kill') is False and not facts.get('descendants')
                  and facts.get('supervisor_exit_code') == 0 and facts.get('boot_id') == self.service.get('boot_id'))
        return {'closed': bool(closed), 'facts': {key: facts.get(key) for key in
                ('command_exit', 'timed_out', 'forced_kill', 'supervisor_exit_code')}}

    def start(self, readback):
        if self.consumed or readback.get('consumed') is True:
            raise AdapterError('oneshot_consumed')
        if readback.get('pid') is None:
            raise AdapterError('start_not_observed')
        self.consumed = True
        return {'started': True}


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
    plist = label + '.plist'
    steps = (
        ['launchctl', 'bootstrap', service['domain'], plist],
        ['launchctl', 'print', domain],
        ['launchctl', 'bootout', domain],
    )
    rollback = (['launchctl', 'bootout', domain],)
    try:
        from upgrade_native_runner import run_sequence, RunnerError
        run_sequence(runner, steps, rollback=rollback)
    except RunnerError as error:
        raise AdapterError('native_probe_failed') from error
    return {'verified': False, 'reason': 'native_os_unverified',
            'sequence': [step[1] for step in steps], 'label': label}


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
