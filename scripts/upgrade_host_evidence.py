#!/usr/bin/env python3
"""Shared stop and writer evidence. No service control and no production path.

Adapters call these checks with facts they already observed. A command exit of
zero is not closure. This module does not invent an S3 stop record.
"""
import sys
sys.dont_write_bytecode = True
sys.pycache_prefix = None
import os


def observe_process(*, pid, started, exit_code, forced_kill, stop_requested,
                    descendants, boot_id=None, expected_boot_id=None,
                    process_start=None, expected_process_start=None, observable=True):
    """Closed only when this exact process ended normally and left no descendants."""
    unknown = []
    if not observable:
        unknown.append('unobservable')
    if boot_id != expected_boot_id or process_start != expected_process_start:
        unknown.append('identity_mismatch')
    if descendants:
        unknown.extend(descendants)
    closed = (observable and started and not unknown and forced_kill is False
              and stop_requested is True and exit_code == 0 and type(exit_code) is int)
    return {'closed': closed, 'unknown': unknown, 'pid': pid}


def admit_stop(window):
    """Refuse a manager record that still names a live writer.

    The S3 field check stays in upgrade_recovery. This gate is the missing
    writer fact: command success plus a surviving writer is not a normal stop.
    """
    record = dict(window.manager.stop())
    live = record.pop('live_writers', None)
    if live:
        from upgrade import Failure
        raise Failure('normal_stop_unproven', 'stop', 83)
    window.manager.stop.return_value = record
    return window.stop()


def writer_closed(pids):
    """True only when every named pid is gone. None means observation failed."""
    if pids is None:
        return False
    for pid in pids:
        try:
            os.kill(pid, 0)
        except ProcessLookupError:
            continue
        except OSError:
            return False
        else:
            return False
    return True
