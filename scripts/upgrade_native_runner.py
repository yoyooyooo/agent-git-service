#!/usr/bin/env python3
"""Fixed native command runner. Tests inject a runner; this module does not call one.

A permitted probe still returns native_os_unverified. Real launchctl/systemctl
execution is not performed by the default path.
"""
import os
import subprocess

MAX_OUTPUT = 4096
TIMEOUT = 5
DROPPED = ('LD_PRELOAD', 'DYLD_INSERT_LIBRARIES', 'BASH_ENV', 'PYTHONPATH',
           'PYTHONPYCACHEPREFIX')


class RunnerError(ValueError):
    def __init__(self, code):
        super().__init__(code)
        self.code = code


def clean_env():
    env = {key: value for key, value in os.environ.items() if key not in DROPPED}
    env['LC_ALL'] = 'C'
    return env


def bounded_runner(argv):
    """Run one fixed argv. Not used by tests or by the default probe."""
    if not isinstance(argv, (list, tuple)) or not argv or any(not isinstance(part, str) for part in argv):
        raise RunnerError('argv_rejected')
    if argv[0] not in ('launchctl', 'systemctl', '/usr/bin/trash'):
        raise RunnerError('argv_rejected')
    completed = subprocess.run(list(argv), check=False, capture_output=True, text=True,
                               timeout=TIMEOUT, env=clean_env())
    return {'exit': completed.returncode,
            'stdout': completed.stdout[:MAX_OUTPUT],
            'stderr': completed.stderr[:MAX_OUTPUT]}


def run_sequence(runner, steps, *, rollback):
    """Run steps in order. On failure run rollback, then raise."""
    done = []
    try:
        for argv in steps:
            observed = runner(list(argv))
            done.append((argv, observed))
            failed = (isinstance(observed, dict) and observed.get('exit') not in (0, None)
                      or (isinstance(observed, dict) and observed.get('ActiveState') == 'failed'))
            if failed:
                raise RunnerError('native_probe_failed')
    except Exception as error:
        for argv in rollback:
            try:
                runner(list(argv))
            except Exception:
                pass
        if isinstance(error, RunnerError):
            raise
        raise RunnerError('native_probe_failed') from error
    return done
