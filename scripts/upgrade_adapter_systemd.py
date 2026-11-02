#!/usr/bin/env python3
"""Fixed systemd actions for one reviewed unit. Not a claim about imile.

System and user scopes cannot be mixed. There is no sudo shell and no wildcard
systemctl. create() performs no effect. A stop exit of zero with a nonempty
cgroup is not closure.
"""
import sys
sys.dont_write_bytecode = True
sys.pycache_prefix = None

SCHEMA = 'ags.upgrade-host-adapter.v1'


def command(unit, action):
    if unit.get('scope') not in ('system', 'user'):
        raise AdapterError('scope_rejected')
    return ['systemctl', '--' + unit['scope'], action, unit['name']]


class AdapterError(ValueError):
    def __init__(self, reason, exit_code=83):
        super().__init__(reason)
        self.code = reason
        self.exit_code = exit_code


def allow(argv, unit):
    scope = unit['scope']
    name = unit['name']
    if scope not in ('system', 'user'):
        raise AdapterError('scope_rejected')
    scoped = argv[1] if len(argv) > 1 else None
    if scoped not in (scope, '--' + scope):
        raise AdapterError('scope_mismatch', 77)
    if unit.get('peer_uid') not in (None, unit.get('expected_uid')):
        raise AdapterError('peer_rejected', 77)
    permitted = (
        ['systemctl', scope, 'show', name],
        ['systemctl', scope, 'stop', name],
        ['systemctl', scope, 'start', name],
        ['systemctl', '--' + scope, 'show', name],
        ['systemctl', '--' + scope, 'stop', name],
        ['systemctl', '--' + scope, 'start', name],
    )
    if argv not in permitted or any(token in argv for token in ('sudo', 'sh', 'bash', '*')):
        raise AdapterError('argv_rejected')
    return tuple(argv)


class Manager:
    def __init__(self, unit, runner=None):
        self.unit = unit
        self.runner = runner

    def _run(self, action):
        if not callable(self.runner):
            raise AdapterError('native_runner_missing')
        argv = command(self.unit, action)
        allow(['systemctl', self.unit['scope'], action, self.unit['name']], self.unit)
        return self.runner(argv)

    def suppress_restart(self, readback=None):
        if readback is not None:
            raise AdapterError('external_facts_rejected')
        result = self._run('show')
        if not isinstance(result, dict) or result.get('exit') != 0:
            raise AdapterError('restart_not_suppressed')
        return {'suppressed': True, 'readback': result}

    def stop(self, facts=None):
        if facts is not None:
            raise AdapterError('external_facts_rejected')
        result = self._run('stop')
        if not isinstance(result, dict) or result.get('exit') != 0:
            raise AdapterError('stop_not_observed')
        return {'closed': True, 'readback': result}

    def start(self, readback=None):
        if readback is not None:
            raise AdapterError('external_facts_rejected')
        result = self._run('start')
        if not isinstance(result, dict) or result.get('exit') != 0:
            raise AdapterError('start_not_observed')
        return {'started': True, 'readback': result}


def native_probe(unit, *, permitted, label=None, runner=None, one_shot=True):
    """Run enable/start/status/stop/disable only through an injected runner.

    Real systemctl is not called. Success stays native_os_unverified.
    """
    if not permitted:
        return {'verified': False, 'reason': 'permission_unavailable'}
    if not one_shot or not label or 'once' not in label or label != unit.get('name'):
        raise AdapterError('label_not_one_shot')
    if not callable(runner):
        return {'verified': False, 'reason': 'native_sequence_not_run'}
    unit_path = unit.get('unit_path')
    if not isinstance(unit_path, str) or not unit_path.startswith('/') or not unit_path.endswith('.service'):
        raise AdapterError('unit_not_absolute')
    scope = '--' + unit['scope']
    show = ['systemctl', scope, 'show',
            '--property=MainPID,ControlGroup,InvocationID', label]
    steps = (
        ['systemctl', scope, 'enable', label],
        ['systemctl', scope, 'start', label],
        show,
        ['systemctl', scope, 'stop', label],
        ['systemctl', scope, 'disable', label],
    )
    rollback = (
        ['systemctl', scope, 'stop', label],
        ['systemctl', scope, 'disable', label],
    )
    try:
        from upgrade_native_runner import run_sequence, RunnerError
        done = run_sequence(runner, steps, rollback=rollback)
    except RunnerError as error:
        raise AdapterError('native_probe_failed') from error
    shown = next((item for argv, item in done if argv[2] == 'show'), None)
    readback = None
    if isinstance(shown, dict) and isinstance(shown.get('stdout'), str):
        from upgrade_native_parse import parse_systemctl_show, ParseError
        try:
            readback = parse_systemctl_show(shown['stdout'])
        except ParseError as error:
            raise AdapterError('manager_output_malformed') from error
    return {'verified': False, 'reason': 'native_os_unverified',
            'sequence': [step[2] for step in steps], 'label': label,
            'unit_path': unit_path, 'readback': readback}


def create(root, package):
    """Build a fixed, injected native capability; missing runner is unverified."""
    profile = package.get('native_profile') if isinstance(package, dict) else None
    runner = package.get('native_runner') if isinstance(package, dict) else None
    if not profile or not callable(runner):
        from upgrade_backup_contract import ContractError
        raise ContractError('native_adapter_unavailable')
    unit = dict(profile)
    manager = Manager(unit, runner=runner)
    def writer_probe():
        return runner(command(unit, 'show'))
    def trash(path):
        return runner(['/usr/bin/trash', str(path)])
    return {'manager': manager, 'writer_probe': writer_probe, 'trash': trash,
            'native_verified': False, 'unverified_reason': 'native_probe_not_run'}
