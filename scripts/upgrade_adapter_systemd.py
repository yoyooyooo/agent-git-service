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
    if argv and argv[1] != scope:
        raise AdapterError('scope_mismatch', 77)
    if unit.get('peer_uid') != unit.get('expected_uid'):
        raise AdapterError('peer_rejected', 77)
    permitted = (
        ['systemctl', scope, 'show', name],
        ['systemctl', scope, 'stop', name],
        ['systemctl', scope, 'start', name],
    )
    if argv not in permitted or any(token in argv for token in ('sudo', 'sh', 'bash', '*')):
        raise AdapterError('argv_rejected')
    return tuple(argv)


class Manager:
    def __init__(self, unit):
        self.unit = unit

    def suppress_restart(self, readback):
        if readback.get('restart') != 'no' or readback.get('dropin_sha256') != self.unit['dropin_sha256']:
            raise AdapterError('restart_not_suppressed')
        if readback.get('triggers'):
            raise AdapterError('trigger_not_closed')
        return {'suppressed': True}

    def stop(self, facts):
        closed = (facts.get('command_exit') == 0 and not facts.get('timed_out')
                  and facts.get('result') == 'success' and not facts.get('cgroup')
                  and facts.get('main_pid') in (0, None)
                  and facts.get('peer_uid') == self.unit['expected_uid'])
        return {'closed': bool(closed)}

    def start(self, readback):
        if readback.get('exec_start') != self.unit['exec_start']:
            raise AdapterError('exec_start_drift')
        return {'started': readback.get('active') is True}


def create(root, package):
    """Refuse before claim: native effects/observer have not been implemented."""
    from upgrade_backup_contract import ContractError
    raise ContractError('native_adapter_unavailable')
