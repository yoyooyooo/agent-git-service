#!/usr/bin/env python3
"""Parse recorded launchctl print and systemctl show text.

This module does not execute launchctl or systemctl. A parsed readback is still
not a live-host observation; callers keep native_os_unverified.
"""
import re


class ParseError(ValueError):
    def __init__(self, code):
        super().__init__(code)
        self.code = code


_PID = re.compile(r'^\s*pid = (\d+)\s*$')
_STATE = re.compile(r'^\s*state = (.+?)\s*$')
_PATH = re.compile(r'^\s*path = (\S+)\s*$')
_JOB = re.compile(r'^(\S+) = \{$')


def parse_launchctl_print(text):
    """Read one launchctl print job block. Malformed text is refused."""
    if not isinstance(text, str) or not text.strip():
        raise ParseError('manager_output_malformed')
    job = pid = state = plist = None
    for line in text.splitlines():
        matched = _JOB.match(line)
        if matched and job is None:
            job = matched.group(1)
        matched = _PID.match(line)
        if matched:
            pid = int(matched.group(1))
        matched = _STATE.match(line)
        if matched:
            state = matched.group(1)
        matched = _PATH.match(line)
        if matched and plist is None:
            plist = matched.group(1)
    if job is None or state is None or plist is None or not plist.startswith('/'):
        raise ParseError('manager_output_malformed')
    return {'manager': 'launchd', 'job': job, 'state': state, 'pid': pid,
            'plist': plist, 'boot_id': None}


_SHOW = re.compile(r'^([A-Za-z][A-Za-z0-9]*)=(.*)$')


def parse_systemctl_show(text):
    """Read systemctl show -p lines. Missing MainPID or a relative cgroup is refused."""
    if not isinstance(text, str) or not text.strip():
        raise ParseError('manager_output_malformed')
    fields = {}
    for line in text.splitlines():
        if not line.strip():
            continue
        matched = _SHOW.match(line.strip())
        if not matched:
            raise ParseError('manager_output_malformed')
        fields[matched.group(1)] = matched.group(2)
    required = ('MainPID', 'ControlGroup', 'InvocationID')
    if any(name not in fields for name in required):
        raise ParseError('manager_output_malformed')
    try:
        pid = int(fields['MainPID'])
    except ValueError as error:
        raise ParseError('manager_output_malformed') from error
    cgroup = fields['ControlGroup']
    if not cgroup.startswith('/'):
        raise ParseError('manager_output_malformed')
    invocation = fields['InvocationID']
    if not re.fullmatch(r'[0-9a-fA-F]{32}', invocation):
        raise ParseError('manager_output_malformed')
    return {'manager': 'systemd', 'pid': pid or None, 'cgroup': cgroup,
            'boot_id': invocation.lower()}


def compare_observation(before, after, descendants):
    """Report why two readbacks are not the same generation.

    descendants is a list of still-visible child pids. A nonempty list after a
    requested stop is descendant_remaining, not closure.
    """
    if not isinstance(before, dict) or not isinstance(after, dict):
        raise ParseError('manager_output_malformed')
    if not isinstance(descendants, list) or any(type(item) is not int for item in descendants):
        raise ParseError('manager_output_malformed')
    if descendants:
        return {'same': False, 'reason': 'descendant_remaining', 'descendants': descendants}
    if before.get('boot_id') != after.get('boot_id'):
        return {'same': False, 'reason': 'boot_changed'}
    if before.get('pid') != after.get('pid'):
        return {'same': False, 'reason': 'pid_changed'}
    return {'same': True, 'reason': 'unchanged', 'pid': after.get('pid')}
