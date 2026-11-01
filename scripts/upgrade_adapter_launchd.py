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


def native_probe(service, *, permitted):
    """Never calls launchctl unless an isolated permission was explicitly granted."""
    if not permitted:
        return {'verified': False, 'reason': 'permission_unavailable'}
    return {'verified': False, 'reason': 'native_sequence_not_run'}


def create(root, package):
    """Parse only. The returned callables perform no effect until invoked."""
    return {'manager': Manager(package), 'writer_probe': lambda: None, 'trash': lambda path: None}
