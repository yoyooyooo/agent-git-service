#!/usr/bin/env python3
"""Continue one root-registered installed/unstarted transaction exactly once.

The original transaction claim and failure remain intact. A separate typed
approval admits only the existing start tail, never stop/copy/restore/install.
"""
from datetime import datetime, timezone
import hashlib
import os
from pathlib import Path
import re

from upgrade import directory_fd, private_json
from upgrade_backup import durable_bytes
from upgrade_backup_contract import (canonical, digest, fields, fingerprint,
                                     identifier, require, strict_json, timestamp)
from upgrade_identity import read_trusted
from upgrade_inventory import file_digest
from upgrade_recovery import read_window
from routeb_host_package import NATIVE_PROFILE_SCHEMA, load_preimage, read_document
from routeb_native_delegation import is_delegated, require_delegation
from routeb_native_recovery import consumed_window, read_reference
import routeb_stage0_client as client

REGISTRY = Path('/usr/local/libexec/ags-routeb-native')
SUBJECT_FIELDS = ('registration_sha256 transaction_invocation_id host_instance binding_sha256 generation '
                  'host_journal_sha256 cold_journal_sha256 snapshot_id manifest_sha256 group_sha256').split()
LATE_FIELDS = ('schema', 'registration_id', 'subject', 'backup_directory', 'approval')
APPROVAL_FIELDS = ('schema approval_id actor purpose decision issued_at expires_at subject_sha256').split()


def registered(context):
    root = REGISTRY / context['registration_id']
    raw = read_trusted(root / 'stage0-registration.json', maximum=1024 * 1024)
    require(hashlib.sha256(raw).hexdigest() == context['registration_sha256'], 'continuation_registration_changed')
    registration = strict_json(raw)
    profile = read_trusted(root / 'host-profile.json', maximum=65536)
    require(hashlib.sha256(profile).hexdigest() == registration['host_profile']['sha256'],
            'continuation_host_changed')
    return registration, strict_json(profile)


def read_late(context, registration, host):
    """Also used by the peer decoder: a nonempty hash is never generic authority."""
    root = Path(context['root'])
    raw = read_trusted(REGISTRY / context['registration_id'] / 'continuation-registration.json', maximum=1024 * 1024)
    require(hashlib.sha256(raw).hexdigest() == context['recovery_registration_sha256'],
            'continuation_registration_changed')
    value = strict_json(raw)
    fields(value, LATE_FIELDS); fields(value['subject'], SUBJECT_FIELDS)
    subject = value['subject']
    require(value['schema'] == 'ags.routeb-stage0-continuation.v1'
            and value['registration_id'] == context['registration_id'] == registration['id']
            and subject['registration_sha256'] == context['registration_sha256']
            and subject['host_instance'] == host['host_instance']
            and host['scope'] == registration['scope'] == 'isolated-native'
            and host['root'] == str(root), 'continuation_registration_binding')
    for key in SUBJECT_FIELDS:
        if key.endswith('_sha256'):
            digest(subject[key]); require(subject[key] != '0' * 64, 'continuation_subject_invalid')
    for key in ('generation', 'snapshot_id', 'transaction_invocation_id'):
        identifier(subject[key])
    for phase in ('transaction', 'continue-installed', 'serve-initial', 'serve-candidate'):
        rows = [row for row in registration['invocations'] if row['phase'] == phase]
        require(len(rows) == 1, 'continuation_unregistered')
        if phase == 'transaction':
            require(rows[0]['id'] == subject['transaction_invocation_id'], 'continuation_transaction_binding')
    require(value['backup_directory'] == str(root.parent / 'recovery/backups' / subject['snapshot_id']),
            'continuation_snapshot_path')
    fields(value['approval'], ('path', 'sha256', 'size')); digest(value['approval']['sha256'])
    require(value['approval']['path'] == str(root.parent / 'continuation/approval.json')
            and type(value['approval']['size']) is int and 0 < value['approval']['size'] <= 1024 * 1024,
            'continuation_approval_path')
    return value


def binding(root, *, operations, expected=None):
    context = client.context(root, operations=operations)
    require(expected is None or context == expected, 'continuation_context_changed')
    registration, host = registered(context)
    return context, read_late(context, registration, host), registration, host


def approval(late, context, registration, *, check_time=True):
    value = read_reference(late['approval'])
    delegated = is_delegated(value, 'continue-installed')
    fields(value, (*APPROVAL_FIELDS, 'delegation') if delegated else APPROVAL_FIELDS)
    invocation = [row for row in registration['invocations'] if row['phase'] == 'continue-installed']
    require(len(invocation) == 1 and value['approval_id'] == invocation[0]['authorization_id']
            and value['schema'] in ('ags.routeb-continuation-approval.v1', 'ags.routeb-continuation-approval.v2')
            and value['purpose'] == 'continue-installed' and value['decision'] == 'approve'
            and value['subject_sha256'] == fingerprint(late['subject']), 'continuation_approval_binding')
    identifier(value['approval_id'])
    issued, expires = timestamp(value['issued_at']), timestamp(value['expires_at'])
    require(issued < expires and (not check_time or issued <= datetime.now(timezone.utc) < expires),
            'continuation_approval_expired')
    if delegated:
        require_delegation(value, 'continue-installed', root=context['root'],
                           host_instance=late['subject']['host_instance'])
    else:
        require(type(value['actor']) is str and value['actor'].startswith('human:') and len(value['actor']) > 6,
                'continuation_approval_actor')
    return value


def snapshot(late):
    return {**late['subject'], 'backup_directory': late['backup_directory']}


def frozen_window(root, late, *, archived=False):
    pending = Path(root) / 'state/ags-autodeploy/local-v2/pending'
    directory = pending / 'continuation' if archived else pending
    prefix = 'original-' if archived else ''
    host, host_raw, host_sha = read_document(directory / (prefix + 'host-execution.json'))
    cold, cold_raw, cold_sha = read_document(directory / (prefix + 'cold-journal.json'))
    subject = late['subject']
    require(host_sha == subject['host_journal_sha256'] and cold_sha == subject['cold_journal_sha256'],
            'continuation_failed_window_changed')
    require(host.get('schema') == 'ags.routeb-host-journal.v1' and host.get('root') == str(root)
            and host.get('transaction_id') == late['registration_id']
            and host.get('host_instance') == subject['host_instance']
            and host.get('binding_sha256') == subject['binding_sha256']
            and host.get('phase') == 'installed' and host.get('status') == 'failed'
            and host.get('stop_attempted') is True and host.get('start_attempted') is False,
            'continuation_failed_window_binding')
    require(cold.get('schema') == 'ags.cold-journal.v1' and cold.get('phase') == 'installed'
            and cold.get('start_attempted') is False and 'start_called' not in cold and 'handoff' not in cold
            and cold.get('generation') == subject['generation']
            and cold.get('binding_sha256') == subject['binding_sha256']
            and cold.get('backup_dir') == late['backup_directory'], 'continuation_start_unknown')
    return host, cold, host_raw, cold_raw


def validate_progress(original, current, *, candidate=False):
    """Only the original ColdWindow start transition may change these fields."""
    mutable = {'phase', 'start_attempted', 'start_called', 'handoff'}
    require({k: v for k, v in current.items() if k not in mutable} ==
            {k: v for k, v in original.items() if k not in mutable}, 'continuation_cold_binding_changed')
    if not candidate and current == original:
        return
    require(current.get('phase') == 'start_attempted' and current.get('start_attempted') is True
            and type(current.get('start_called')) is bool, 'continuation_cold_phase_invalid')
    handoff = current.get('handoff')
    fields(handoff, ('token', 'generation', 'consumed', 'binary_sha256', 'receipt_sha256', 'manager_requested'))
    require(type(handoff['token']) is str and re.fullmatch('[0-9a-f]{32}', handoff['token']) is not None
            and handoff['generation'] == original['generation']
            and handoff['binary_sha256'] == original['backup_binding']['new_binary_sha256']
            and handoff['receipt_sha256'] == original['installed_receipt_sha256']
            and type(handoff['consumed']) is bool and type(handoff['manager_requested']) is bool,
            'continuation_handoff_binding')
    if candidate:
        require(current['start_called'] is True and handoff['manager_requested'] is True,
                'continuation_start_not_requested')
    else:
        require(handoff['consumed'] is False and handoff['manager_requested'] is False,
                'continuation_start_already_requested')


def packet(package_path, approval_path, expected_host):
    context = client.context(operations=('continue-installed',))
    root = Path(context['root'])
    context, late, registration, host = binding(root, operations=('continue-installed',), expected=context)
    require(str(Path(package_path)) == registration['inputs']['package']['path']
            and str(Path(approval_path)) == registration['inputs']['approval']['path']
            and host['logical_host'] == expected_host, 'continuation_package_binding')
    approval(late, context, registration)
    frozen_window(root, late)
    consumed, original = consumed_window(root, context, snapshot(late), package_path, approval_path)
    package = load_preimage(package_path, approval_path, expected_host, consumed_at=consumed,
                           journal_sha256=late['subject']['cold_journal_sha256'],
                           manifest_sha256=late['subject']['manifest_sha256'])
    require(package.root == root and package.host.scope == 'isolated-native'
            and package.profile['schema'] == NATIVE_PROFILE_SCHEMA and not package.execution_blockers()
            and package.cold_binding() == original and fingerprint(original) == late['subject']['binding_sha256']
            and fingerprint(package.group) == late['subject']['group_sha256'], 'continuation_package_binding')
    return context, late, package


def authorize(package, expected, late):
    context, current_late, registration, _ = binding(package.root, operations=('continue-installed',), expected=expected)
    require(current_late == late, 'continuation_registration_changed')
    approval(late, context, registration)
    _, original, _, _ = frozen_window(package.root, late, archived=True)
    require(file_digest(package.pending / 'host-execution.json') == late['subject']['host_journal_sha256'],
            'continuation_original_failure_changed')
    consumed, original_binding = consumed_window(package.root, context, snapshot(late), package.path, package.approval_path)
    require(package.cold_binding() == original_binding, 'continuation_package_binding')
    package.authorize(consumed_at=consumed)
    _, current, _ = read_window(package.root)
    validate_progress(original, current)


def candidate_binding(root, expected, *, check_time=True):
    context, late, registration, _ = binding(root, operations=('serve',), expected=expected)
    require(context['phase'] == 'serve-candidate', 'continuation_candidate_required')
    approval(late, context, registration, check_time=check_time)
    _, original, _, _ = frozen_window(root, late, archived=True)
    _, current, pending = read_window(root)
    require(file_digest(pending / 'host-execution.json') == late['subject']['host_journal_sha256'],
            'continuation_original_failure_changed')
    validate_progress(original, current, candidate=True)
    return context


def run(package_path, approval_path, expected_host):
    from routeb_host_execution import HostExecution, failure_code, lifecycle_blockers, read_version, utc
    context, late, package = packet(package_path, approval_path, expected_host)
    operation = HostExecution(package)
    window = operation.window
    window.acquire_owner()
    try:
        _, fresh, _, _ = binding(package.root, operations=('continue-installed',), expected=context)
        require(fresh == late and not lifecycle_blockers(package), 'continuation_binding_changed')
        old_host, old_cold, host_raw, cold_raw = frozen_window(package.root, late)
        window.acquire()
        operation.manager.rebind_stopped(old_cold)
        directory = package.pending / 'continuation'
        with directory_fd(package.pending, private=True) as parent:
            os.mkdir(directory.name, 0o700, dir_fd=parent)
            os.fsync(parent)
        durable_bytes(directory / 'original-host-execution.json', host_raw)
        durable_bytes(directory / 'original-cold-journal.json', cold_raw)
        operation.journal_path = directory / 'host-execution.json'
        operation.evidence_directory = directory
        history_fields = {'code', 'recovery_required', 'service_state'}
        operation.journal = {**{k: v for k, v in old_host.items() if k not in history_fields},
                             'status': 'running', 'started_at': utc(), 'stop_attempted': False,
                             'previous_stop_observed': True,
                             'operation': 'continue-installed', 'continuation': {
                                 'context_sha256': fingerprint(context),
                                 'registration_sha256': context['recovery_registration_sha256'],
                                 'original_host_journal_sha256': late['subject']['host_journal_sha256'],
                                 'original_cold_journal_sha256': late['subject']['cold_journal_sha256']}}
        durable_bytes(operation.journal_path, canonical(operation.journal))
        decision = lambda: authorize(package, context, late)
        window.reopen_installed(package.candidate.value['version'], read_version,
                                journal_sha256=late['subject']['cold_journal_sha256'], authorize=decision)
        return operation.start_and_observe(decision)
    except BaseException as error:
        if operation.journal is not None:
            operation.stage(operation.journal['phase'], status='failed', code=failure_code(error),
                            recovery_required=True, service_state='stopped_or_unknown')
        raise
    finally:
        window.release(); window.release_owner()
