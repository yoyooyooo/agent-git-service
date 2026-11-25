#!/usr/bin/env python3
"""Deny one original approval open after a real durable installation.

This Linux-only fixture never edits a package, approval, journal or authority.
The registered Python child receives the real EPERM and exits through its own
fixed host dispatch. All other opens, including the root parent's final reads,
are allowed. A missing/failed injection is not a continuation test pass.
"""
import argparse
import ctypes
import errno
import os
from pathlib import Path
import select
import stat
import struct
import sys
import time

from common import (ID, REGISTRY, Refused, absent, exclusive_json, file_reference,
                    fingerprint, object_json, process_identity, protected_directory,
                    read_bytes, read_json, require, sha, utc)


FAN_CLOEXEC = 0x00000001
FAN_NONBLOCK = 0x00000002
FAN_CLASS_CONTENT = 0x00000004
FAN_MARK_ADD = 0x00000001
FAN_OPEN_PERM = 0x00010000
FAN_Q_OVERFLOW = 0x00004000
FAN_ALLOW = 0x01
FAN_DENY = 0x02
METADATA = struct.Struct('=IBBHQii')
RESPONSE = struct.Struct('=iI')


def invocation(registration, phase):
    rows = [row for row in registration['invocations'] if row['phase'] == phase]
    require(len(rows) == 1, 'invocation_missing_or_ambiguous')
    return rows[0]


def start_paths(registry, original, candidate, continuation=None):
    paths = [registry / 'state' / kind / (candidate['id'] + '.json')
             for kind in ('tickets', 'claims', 'active', 'manager-start', 'manager-completion',
                          'exits', 'failures', 'failure-exits')]
    for row in (original, continuation):
        if row is not None:
            paths.append(registry / 'state/effects' / (row['id'] + '-manager-start-intent.json'))
    return paths


def select_checkpoint(host, cold, context, registration, original, candidate, pid, identity):
    """Pure decision; actual file/process observations are supplied by observe()."""
    if context is None or pid != context.get('child', {}).get('pid'):
        return False
    if host is None or host.get('phase') != 'installed' or host.get('status') != 'running':
        return False
    require(context.get('schema') == 'ags.routeb-stage0-context.v1'
            and context.get('registration_id') == registration['id']
            and context.get('invocation_id') == original['id']
            and context.get('operation') == 'transaction' and context.get('phase') == 'transaction',
            'original_child_context')
    child = context['child']
    require(all(identity.get(key) == child.get(key)
                for key in ('pid', 'parent_pid', 'start_ticks', 'uid', 'gid')),
            'original_child_generation')
    require(host.get('schema') == 'ags.routeb-host-journal.v1'
            and host.get('transaction_id') == registration['id']
            and host.get('host_instance') == registration['binding']['host_instance']
            and host.get('start_attempted') is False and host.get('stop_attempted') is True,
            'host_checkpoint')
    require(type(cold) is dict and cold.get('schema') == 'ags.cold-journal.v1'
            and cold.get('phase') == 'installed' and cold.get('start_attempted') is False
            and 'handoff' not in cold and 'start_called' not in cold,
            'cold_checkpoint')
    require(candidate['phase'] == 'serve-candidate', 'candidate_phase')
    return True


def observe(registry, registration, registration_sha, profile, original, candidate, pid):
    active_path = registry / 'state/active' / (original['id'] + '.json')
    if absent(active_path):
        return None
    context, context_raw = read_json(active_path, owner=0, mode=0o600)
    if pid != context.get('child', {}).get('pid'):
        return None  # Never deny the root parent's authority checks.
    root = Path(profile['root'])
    pending = root / 'state/ags-autodeploy/local-v2/pending'
    if absent(pending / 'host-execution.json'):
        return None
    host, host_raw = read_json(pending / 'host-execution.json', owner=profile['uid'], mode=0o600)
    if host.get('phase') != 'installed' or host.get('status') != 'running':
        return None
    cold, cold_raw = read_json(pending / 'cold-journal.json', owner=profile['uid'], mode=0o600)
    identity = process_identity(pid)
    if not select_checkpoint(host, cold, context, registration, original, candidate, pid, identity):
        return None
    require(context.get('registration_sha256') == registration_sha
            and context.get('root') == str(root) and host.get('root') == str(root), 'checkpoint_binding')
    claim, _ = read_json(pending / 'claim.json', owner=profile['uid'], mode=0o600)
    require(claim.get('schema') == 'ags.local-claim.v2'
            and fingerprint(claim['binding']) == host['binding_sha256'] == cold['binding_sha256'],
            'checkpoint_binding')
    receipt = read_bytes(root / 'state/installation.json', owner=profile['uid'], mode=0o600)
    require(sha(receipt) == cold['installed_receipt_sha256']
            and sha(read_bytes(root / 'bin/gh-server', 1 << 30, owner=profile['uid']))
                == claim['binding']['new_binary_sha256'], 'installed_bytes_changed')
    no_start = start_paths(registry, original, candidate, invocation(registration, 'continue-installed'))
    require(all(absent(path) for path in no_start)
            and absent(root / 'state/installing.json'), 'candidate_start_domain_not_empty')
    require(process_identity(pid) == identity, 'original_child_generation')
    return {'child': identity, 'context_sha256': sha(context_raw),
            'host_journal_sha256': sha(host_raw), 'cold_journal_sha256': sha(cold_raw),
            'installed_receipt_sha256': sha(receipt),
            'absent_candidate_paths': [str(path) for path in no_start]}


def events(raw):
    offset = 0
    while offset < len(raw):
        require(len(raw) - offset >= METADATA.size, 'fanotify_short_event')
        length, version, _, metadata_length, mask, fd, pid = METADATA.unpack_from(raw, offset)
        require(version == 3 and metadata_length == METADATA.size
                and METADATA.size <= length <= len(raw) - offset, 'fanotify_event_shape')
        require(not mask & FAN_Q_OVERFLOW and fd >= 0 and mask & FAN_OPEN_PERM,
                'fanotify_event_unavailable')
        yield fd, pid
        offset += length


def watch(registration_id, evidence, timeout):
    require(sys.platform == 'linux' and os.geteuid() == 0 and ID.fullmatch(registration_id),
            'root_linux_required')
    require(1 <= timeout <= 1800, 'fault_timeout')
    registry = protected_directory(REGISTRY / registration_id)
    evidence = protected_directory(evidence)
    registration, raw = read_json(registry / 'stage0-registration.json', owner=0)
    require(registration.get('id') == registration_id and registration.get('scope') == 'isolated-native',
            'isolated_registration_required')
    profile = object_json(file_reference(registration['host_profile']))
    original, candidate = invocation(registration, 'transaction'), invocation(registration, 'serve-candidate')
    approval = registration['inputs']['approval']
    approval_raw = file_reference(approval, owner=profile['uid'])
    approval_path = Path(approval['path'])
    require(approval_path.parent == Path(profile['root']).parent / 'package', 'approval_path')
    before = approval_path.stat()
    require(stat.S_ISREG(before.st_mode), 'approval_type')
    libc = ctypes.CDLL(None, use_errno=True)
    init, mark = libc.fanotify_init, libc.fanotify_mark
    init.argtypes = (ctypes.c_uint, ctypes.c_uint)
    init.restype = ctypes.c_int
    mark.argtypes = (ctypes.c_int, ctypes.c_uint, ctypes.c_uint64, ctypes.c_int, ctypes.c_char_p)
    mark.restype = ctypes.c_int
    fd = init(FAN_CLASS_CONTENT | FAN_CLOEXEC | FAN_NONBLOCK, os.O_RDONLY | os.O_CLOEXEC)
    require(fd >= 0, 'fanotify_permission_unavailable')
    try:
        require(mark(fd, FAN_MARK_ADD, FAN_OPEN_PERM, -100, os.fsencode(approval_path)) == 0,
                'fanotify_inode_mark_failed')
        exclusive_json(evidence / 'fault-ready.json', {
            'schema': 'ags.routeb-fixture-fault-ready.v1', 'registration_sha256': sha(raw),
            'approval_sha256': sha(approval_raw), 'approval_device': before.st_dev,
            'approval_inode': before.st_ino, 'recorded_at': utc()})
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if not select.select([fd], [], [], min(1, max(0, deadline - time.monotonic())))[0]:
                continue
            raw_events = os.read(fd, 65536)
            for event_fd, pid in events(raw_events):
                try:
                    info = os.fstat(event_fd)
                    require((info.st_dev, info.st_ino, info.st_size) ==
                            (before.st_dev, before.st_ino, before.st_size), 'approval_inode_changed')
                    checkpoint = observe(registry, registration, sha(raw), profile, original, candidate, pid)
                    decision = FAN_DENY if checkpoint is not None else FAN_ALLOW
                    if checkpoint is not None:
                        exclusive_json(evidence / 'fault-selected.json', {
                            'schema': 'ags.routeb-fixture-fault-selected.v1', 'recorded_at': utc(),
                            'registration_sha256': sha(raw), 'approval_sha256': sha(approval_raw),
                            'checkpoint': checkpoint, 'response_not_yet_written': True})
                    response = RESPONSE.pack(event_fd, decision)
                    require(os.write(fd, response) == len(response), 'fanotify_response_unknown')
                    if checkpoint is not None:
                        exclusive_json(evidence / 'fault-applied.json', {
                            'schema': 'ags.routeb-fixture-fault-applied.v1', 'recorded_at': utc(),
                            'response': 'FAN_DENY', 'expected_errno': errno.EPERM,
                            'registration_sha256': sha(raw), 'checkpoint': checkpoint,
                            'native_pass': False})
                        return
                finally:
                    os.close(event_fd)
        raise Refused('fault_checkpoint_not_observed')
    finally:
        # Closing removes this single-file mark. Undecided operations are
        # allowed by the kernel; failure here must never be reported as a pass.
        os.close(fd)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--registration', required=True)
    parser.add_argument('--evidence', type=Path, required=True)
    parser.add_argument('--timeout-seconds', type=int, default=900)
    parser.add_argument('--execute', action='store_true')
    args = parser.parse_args()
    try:
        require(args.execute, 'execute_required')
        watch(args.registration, args.evidence, args.timeout_seconds)
        print('fault=applied native_pass=false')
        return 0
    except (Refused, OSError, ValueError, KeyError):
        print('fault=unverified native_pass=false', file=sys.stderr)
        return 78


if __name__ == '__main__':
    raise SystemExit(main())
