#!/usr/bin/env python3
"""Run the real PID1 sequence on one fresh GitHub-hosted Ubuntu VM.

The fixture supplies inputs and observes effects. It never supplies operational
journals or parent proofs. Failed attempts are retained until VM disposal.
"""
import argparse
from datetime import datetime, timezone
import os
from pathlib import Path
import re
import subprocess
import sys
import time
import urllib.request

import authorization
from approval_fault import invocation, start_paths
from common import (ID, REGISTRY, SHA, CommandFailed, Refused, absent, canonical, command, exclusive,
                    exclusive_json, file_reference, fingerprint, object_json,
                    read_bytes, read_json, reference, require, sha, utc)
from provision import provision


PROPERTIES = ('MainPID', 'ControlPID', 'ActiveState', 'SubState', 'Result', 'InvocationID',
              'ExecMainPID', 'ExecMainCode', 'ExecMainStatus', 'NRestarts')


def properties(unit):
    raw = command(['/usr/bin/systemctl', 'show', unit, '--no-pager',
                   '--property=' + ','.join(PROPERTIES)], operation='manager_properties')
    values = dict(line.split('=', 1) for line in raw.decode().splitlines() if '=' in line)
    require(set(values) == set(PROPERTIES), 'manager_readback_incomplete')
    return values


def wait_unit(row, evidence, label, timeout):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        observed = properties(row['unit'])
        if observed['MainPID'] == observed['ControlPID'] == '0' and observed['ActiveState'] in ('inactive', 'failed'):
            exclusive_json(evidence / (label + '-manager.json'), {
                'recorded_at': utc(), 'unit': row['unit'], 'properties': observed})
            return observed
        time.sleep(1)
    raise Refused('native_unit_timeout_' + label)


def start_controller(row, evidence, label, timeout=300):
    # Type=simple systemctl start success is not child completion.
    result = subprocess.run(['/usr/bin/systemctl', 'start', row['unit']], capture_output=True, timeout=150)
    exclusive_json(evidence / (label + '-request.json'), {
        'requested_at': utc(), 'unit': row['unit'], 'systemctl_returncode': result.returncode,
        'stdout_sha256': sha(result.stdout), 'stderr_sha256': sha(result.stderr)})
    observed = wait_unit(row, evidence, label, timeout)
    # Retain raw manager diagnostics privately; export only typed reason codes.
    logs = subprocess.run(['/usr/bin/journalctl', '--unit=' + row['unit'], '--no-pager',
                           '-n', '100', '-o', 'json', '--output-fields=MESSAGE,__REALTIME_TIMESTAMP'],
                          capture_output=True, timeout=20)
    exclusive(evidence / (label + '-journal.private'), logs.stdout)
    codes = []
    if logs.returncode == 0:
        for line in logs.stdout.splitlines():
            try:
                record = object_json(line)
                message = record.get('MESSAGE', '')
                if type(message) is not str:
                    continue
                if re.fullmatch(r'code=[a-z][a-z0-9_]{0,95}', message):
                    code = message[5:]
                elif message.startswith('{'):
                    item = object_json(message.encode())
                    if item.get('schema') != 'ags.routeb-stage0-result.v1' or item.get('status') != 'blocked':
                        continue
                    code = item.get('reason')
                    if type(code) is not str or not re.fullmatch(r'[a-z][a-z0-9_]{0,95}', code):
                        continue
                else:
                    continue
                codes.append({'recorded_us': record.get('__REALTIME_TIMESTAMP'), 'code': code})
            except (Refused, ValueError):
                continue
    exclusive_json(evidence / (label + '-reason-codes.json'), {
        'journal_exit_code': logs.returncode, 'raw_sha256': sha(logs.stdout), 'codes': codes})
    return observed


def finished(registry, row, observed, *, expected):
    require(observed['ExecMainCode'] == '1' and observed['ExecMainStatus'] == str(expected)
            and observed['NRestarts'] == '0'
            and observed['Result'] == ('success' if expected == 0 else 'exit-code'), 'manager_exit_unexpected')
    value, raw = read_json(registry / 'state/exits' / (row['id'] + '.json'), owner=0, mode=0o600)
    require(value.get('child_exit_code') == expected, 'child_exit_unexpected')
    return sha(raw)


def ready(root, phase, timeout=90):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            runtime, _ = read_json(root / 'state/runtime.json')
            require(runtime['stage0']['phase'] == phase and 'stopped_at' not in runtime, 'runtime_phase')
            # The URL was produced for this fixture; no external provider call.
            profile, _ = read_json(root.parent / 'package/native-profile.json')
            url = profile['capture']['health']['local']
            require(url.startswith('http://127.0.0.1:'), 'fixture_health_url')
            with urllib.request.urlopen(url, timeout=3) as response:
                health = object_json(response.read(65537))
                require(response.status == 200 and health.get('status') == 'ready', 'health_not_ready')
            return runtime
        except (Refused, OSError, ValueError, KeyError):
            time.sleep(.2)
    raise Refused('native_ready_timeout')


def failed_original(registry, registration, root, evidence):
    row = invocation(registration, 'transaction')
    host_path = root / 'state/ags-autodeploy/local-v2/pending/host-execution.json'
    cold_path = host_path.parent / 'cold-journal.json'
    host, host_raw = read_json(host_path)
    cold, cold_raw = read_json(cold_path)
    require(host.get('phase') == cold.get('phase') == 'installed' and host.get('status') == 'failed'
            and host.get('start_attempted') is False and cold.get('start_attempted') is False
            and host.get('stop_attempted') is True and 'handoff' not in cold and 'start_called' not in cold,
            'original_failure_not_installed_unstarted')
    require(all(absent(path) for path in start_paths(registry, row, invocation(registration, 'serve-candidate'),
                                                    invocation(registration, 'continue-installed'))),
            'candidate_start_already_attempted')
    exclusive(evidence / 'original-host-execution.json', host_raw)
    exclusive(evidence / 'original-cold-journal.json', cold_raw)
    backup = Path(cold['backup_dir'])
    receipt, receipt_raw = read_json(backup / 'receipt.json')
    manifest, manifest_raw = read_json(backup / 'manifest.json', 16 << 20)
    group, group_raw = read_json(backup / 'recovery-group.json', 16 << 20)
    require(backup == root.parent / 'recovery/backups' / receipt['snapshot_id']
            and sha(manifest_raw) == receipt['manifest_sha256']
            and manifest['snapshot_id'] == receipt['snapshot_id'], 'actual_snapshot_binding')
    return {'registration_sha256': fingerprint(registration), 'transaction_invocation_id': row['id'],
            'host_instance': host['host_instance'], 'binding_sha256': host['binding_sha256'],
            'generation': cold['generation'], 'host_journal_sha256': sha(host_raw),
            'cold_journal_sha256': sha(cold_raw), 'snapshot_id': receipt['snapshot_id'],
            'manifest_sha256': sha(manifest_raw), 'group_sha256': sha(group_raw)}


def continuation(registry, registration, root, subject, evidence, *, expired):
    row = invocation(registration, 'continue-installed')
    grant_ref = registration['scope_grant']
    refs = [{'kind': 'actual-installed-failure', 'sha256': subject['host_journal_sha256']},
            {'kind': 'actual-cold-window', 'sha256': subject['cold_journal_sha256']},
            {'kind': 'actual-backup-manifest', 'sha256': subject['manifest_sha256']}]
    approval = authorization.approve({'schema': 'ags.routeb-continuation-approval.v2',
        'approval_id': row['authorization_id'], 'purpose': 'continue-installed',
        'subject_sha256': fingerprint(subject)}, 'continue-installed', grant_ref, refs,
        lifetime=1 if expired else 120)
    profile = object_json(file_reference(registration['host_profile']))
    approval_ref = exclusive_json(root.parent / 'continuation/approval.json', approval,
                                 uid=profile['uid'], gid=profile['gid'])
    late = {'schema': 'ags.routeb-stage0-continuation.v1', 'registration_id': registration['id'],
            'subject': subject, 'backup_directory': str(root.parent / 'recovery/backups' / subject['snapshot_id']),
            'approval': approval_ref}
    late_ref = exclusive_json(registry / 'continuation-registration.json', late, mode=0o644)
    exclusive_json(evidence / 'continuation-review.json', {
        'recorded_at': utc(), 'late_sha256': late_ref['sha256'], 'subject_sha256': fingerprint(subject),
        'approval_sha256': approval_ref['sha256'], 'issued_at': approval['issued_at'],
        'expires_at': approval['expires_at'], 'negative_case': expired})
    if expired:
        expiry = datetime.fromisoformat(approval['expires_at'])
        while datetime.now(timezone.utc) <= expiry:
            time.sleep(.1)
    return late, approval


def recovery(registry, registration, root, subject):
    profile = object_json(file_reference(registration['host_profile']))
    late = {'schema': 'ags.routeb-stage0-recovery.v1', 'registration_id': registration['id'],
            'registration_sha256': fingerprint(registration), 'host_instance': profile['host_instance'],
            'tool_manifest_sha256': registration['binding']['tool_manifest_sha256'],
            'interpreter_authority_sha256': registration['interpreter']['sha256'],
            'backup_directory': str(root.parent / 'recovery/backups' / subject['snapshot_id']),
            **{k: subject[k] for k in ('snapshot_id', 'manifest_sha256', 'group_sha256')}}
    for purpose, phase, key in [('full-restore', 'restore', 'restore_approval'), ('old-start', 'old-start', 'old_start_approval')]:
        row = invocation(registration, phase)
        value = authorization.approve({'schema': 'ags.routeb-recovery-approval.v2',
            'approval_id': row['authorization_id'], 'purpose': purpose,
            'host_instance': profile['host_instance'], 'snapshot_id': subject['snapshot_id'],
            'manifest_sha256': subject['manifest_sha256'], 'data_loss_acknowledged': True,
            'external_reconciliation_acknowledged': True}, purpose, registration['scope_grant'],
            [{'kind': 'actual-backup-manifest', 'sha256': subject['manifest_sha256']},
             {'kind': 'actual-original-failure', 'sha256': subject['host_journal_sha256']}])
        late[key] = exclusive_json(root.parent / 'recovery/approvals' / (purpose + '.json'), value,
                                   uid=profile['uid'], gid=profile['gid'])
    exclusive_json(registry / 'recovery-registration.json', late, mode=0o644)


def execute(registry, scenario):
    registration, raw = read_json(registry / 'stage0-registration.json', owner=0)
    profile = object_json(file_reference(registration['host_profile']))
    root, evidence = Path(profile['root']), registry / 'fixture-evidence'
    require(fingerprint(registration) == sha(raw), 'fixture_canonical_registration')
    command(['/usr/bin/systemctl', 'daemon-reload'], operation='manager_reload')
    bootstrap = invocation(registration, 'bootstrap')
    finished(registry, bootstrap, start_controller(bootstrap, evidence, 'bootstrap'), expected=0)
    initial = invocation(registration, 'serve-initial')
    command(['/usr/bin/systemctl', 'start', initial['unit']], operation='initial_start', timeout=150)
    ready(root, 'serve-initial')
    fault_output = open(evidence / 'fault-stdout.txt', 'xb')
    fault_error = open(evidence / 'fault-stderr.txt', 'xb')
    fault = subprocess.Popen(['/usr/bin/python3', '-I', '-B', str(Path(__file__).with_name('approval_fault_entry.py')),
        '--registration', registration['id'], '--evidence', str(evidence), '--execute'],
        stdout=fault_output, stderr=fault_error)
    try:
        deadline = time.monotonic() + 15
        while absent(evidence / 'fault-ready.json'):
            require(fault.poll() is None and time.monotonic() < deadline, 'fault_not_ready')
            time.sleep(.05)
        original = invocation(registration, 'transaction')
        observed = start_controller(original, evidence, 'original', timeout=900)
        require(fault.wait(timeout=15) == 0, 'fault_not_applied')
        finished(registry, original, observed, expected=78)
    finally:
        if fault.poll() is None:
            fault.terminate()
            fault.wait(timeout=15)
        fault_output.close(); fault_error.close()
    subject = failed_original(registry, registration, root, evidence)
    _, approval = continuation(registry, registration, root, subject, evidence, expired=scenario == 'expired')
    row = invocation(registration, 'continue-installed')
    observed = start_controller(row, evidence, 'continuation', timeout=1000)
    if scenario == 'expired':
        require(observed['ExecMainStatus'] == '78' and observed['Result'] == 'exit-code'
                and absent(registry / 'state/claims' / (row['id'] + '.json'))
                and all(absent(path) for path in start_paths(registry, original, invocation(registration, 'serve-candidate'), row)),
                'expired_approval_not_refused_before_effect')
        require(sha(read_bytes(root / 'state/ags-autodeploy/local-v2/pending/cold-journal.json'))
                == subject['cold_journal_sha256'], 'negative_case_changed_cold_window')
        return {'case': scenario, 'expired_approval_refused_before_claim': True,
                'candidate_effect_absent': True, 'native_pass': True}
    finished(registry, row, observed, expected=0)
    pending = root / 'state/ags-autodeploy/local-v2/pending'
    continued, _ = read_json(pending / 'continuation/host-execution.json')
    capture, _ = read_json(pending / 'continuation/observation/capture.json', 16 << 20)
    require(continued.get('status') == 'observed' and continued.get('phase') == 'observed'
            and capture.get('status') == 'observed' and capture.get('kind') == 'native'
            and capture.get('accepted') is False and capture.get('installable') is False,
            'actual_native_capture_missing')
    require(sha(read_bytes(pending / 'host-execution.json')) == subject['host_journal_sha256'],
            'original_failure_history_changed')
    ready(root, 'serve-candidate')
    # The actual 600-second capture crosses this already issued expiry. No
    # approval is edited or backdated to exercise historical ticket checking.
    require(datetime.now(timezone.utc) > datetime.fromisoformat(approval['expires_at']), 'approval_not_yet_expired')
    recovery(registry, registration, root, subject)
    restore = invocation(registration, 'restore')
    finished(registry, restore, start_controller(restore, evidence, 'restore', timeout=600), expected=0)
    old = invocation(registration, 'old-start')
    finished(registry, old, start_controller(old, evidence, 'old-start'), expected=0)
    runtime = ready(root, 'serve-old')
    old_generation = runtime['generation']
    # A repeated start of the same finished controller must not refund its
    # original slot, issue another ticket, or disturb the running old service.
    state_before = {str(path.relative_to(registry)): sha(read_bytes(path))
                    for kind in ('claims', 'tickets', 'effects')
                    for path in (registry / 'state' / kind).glob('*.json')}
    replay = start_controller(row, evidence, 'replay', timeout=60)
    require(replay['ExecMainStatus'] == '78', 'consumed_continuation_replayed')
    state_after = {str(path.relative_to(registry)): sha(read_bytes(path))
                   for kind in ('claims', 'tickets', 'effects')
                   for path in (registry / 'state' / kind).glob('*.json')}
    require(state_before == state_after and ready(root, 'serve-old')['generation'] == old_generation,
            'replay_changed_effects')
    return {'case': scenario, 'original_installed_failure_retained': True,
            'continued_native_capture': True, 'expired_history_restore_and_old_start': True,
            'consumed_continuation_refused': True, 'native_pass': True}


def exportable_mapping_failure(value, registration, registration_sha, invocation_id):
    """Only the fixed parent's single-row diagnostic, bound to this fixture."""
    keys = {'schema', 'registration_sha256', 'catalog_sha256', 'invocation_id',
            'subject_invocation_id', 'phase', 'boot_id', 'supervisor', 'child',
            'code', 'maps_sha256', 'maps_bytes', 'mapping'}
    if (set(value) != keys or value['schema'] != 'ags.routeb-stage0-mapping-refusal.v1'
            or value['registration_sha256'] != registration_sha
            or value['catalog_sha256'] != registration['interpreter']['sha256']
            or value['invocation_id'] != invocation_id):
        return False
    subjects = {row['id']: row['phase'] for row in registration['invocations']}
    if (type(value['subject_invocation_id']) is not str or value['subject_invocation_id'] not in subjects
            or value['phase'] != subjects[value['subject_invocation_id']]
            or type(value['boot_id']) is not str or len(value['boot_id']) > 64
            or type(value['maps_sha256']) is not str or not SHA.fullmatch(value['maps_sha256'])
            or type(value['maps_bytes']) is not int or not 0 <= value['maps_bytes'] <= 4 << 20):
        return False
    for key in ('supervisor', 'child'):
        process = value[key]
        if (type(process) is not dict
                or set(process) != {'pid', 'start_ticks', 'parent_pid', 'uid', 'gid', 'executable_sha256'}
                or any(type(process[k]) is not int or process[k] < 0
                       for k in ('pid', 'start_ticks', 'parent_pid', 'uid', 'gid'))
                or process['pid'] <= 1 or process['start_ticks'] == 0
                or type(process['executable_sha256']) is not str or not SHA.fullmatch(process['executable_sha256'])):
            return False
    detail = value['mapping']
    branches = {'field_count': 'stage0_mapping_invalid', 'inode_parse': 'stage0_mapping_invalid',
                'zero_inode_named': 'stage0_mapping_unknown', 'file_path_shape': 'stage0_mapping_unknown',
                'catalog_file_identity': 'stage0_mapping_unknown', 'device_identity': 'stage0_mapping_changed'}
    if (type(detail) is not dict
            or set(detail) != {'branch', 'line_number', 'line', 'field_count', 'catalog_match', 'expected'}
            or type(detail['branch']) is not str or branches.get(detail['branch']) != value['code']
            or type(detail['line_number']) is not int or detail['line_number'] < 1
            or type(detail['field_count']) is not int or detail['field_count'] < 0
            or type(detail['line']) is not str or len(detail['line'].encode()) > 4 << 20
            or '\n' in detail['line'] or '\r' in detail['line']
            or detail['catalog_match'] not in ('not_checked', 'absent', 'present')):
        return False
    expected = detail['expected']
    if detail['catalog_match'] == 'present':
        return (type(expected) is dict and set(expected) == {'device', 'inode'}
                and all(type(v) is int and v >= 0 for v in expected.values()))
    return expected is None


def export(registry, destination, uid, gid):
    """Closed artifact allowlist. No source/grant/approval/env/database payload."""
    destination.mkdir(mode=0o700)
    os.chown(destination, uid, gid)
    registration, registration_raw = read_json(registry / 'stage0-registration.json')
    profile = object_json(file_reference(registration['host_profile']))
    root = Path(profile['root'])
    candidates = list((registry / 'fixture-evidence').glob('*.json'))
    # Source API objects can contain public commit messages; only their hashes
    # are needed. The private authorization readback already has no source text.
    candidates = [p for p in candidates if not p.name.startswith('source-') and p.name != 'catalog-input.json']
    for kind in ('claims', 'active', 'effects', 'exits', 'manager-start', 'manager-completion'):
        candidates += list((registry / 'state' / kind).glob('*.json'))
    # Do not export arbitrary private failures or journal text. A mapping row
    # names its actual object (including spaces/deleted suffix); a path digest
    # alone cannot distinguish the existing refusal branches in an ended VM.
    mapping_failures = {}
    for row in registration['invocations']:
        path = registry / 'state/failures' / (row['id'] + '.json')
        if path.is_file():
            value, raw = read_json(path, 8 << 20)
            if exportable_mapping_failure(value, registration, sha(registration_raw), row['id']):
                candidates.append(path)
                mapping_failures[path] = raw
    for suffix in ('continuation/host-execution.json', 'continuation/observation/capture.json'):
        path = root / 'state/ags-autodeploy/local-v2/pending' / suffix
        if path.is_file():
            candidates.append(path)
    index = []
    for number, path in enumerate(sorted(set(candidates))):
        raw = mapping_failures[path] if path in mapping_failures else read_bytes(path, 16 << 20)
        name = '%03d-%s' % (number, path.name)
        exclusive(destination / name, raw, uid=uid, gid=gid)
        index.append({'name': name, 'original_path': str(path), 'sha256': sha(raw), 'size': len(raw)})
    exclusive_json(destination / 'index.json', {'schema': 'ags.routeb-fixture-export.v1', 'files': index}, uid=uid, gid=gid)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for key in ('source', 'build', 'source-evidence', 'authorization-file', 'export-directory'):
        parser.add_argument('--' + key, required=True, type=Path)
    parser.add_argument('--authorization-sha256', required=True)
    parser.add_argument('--uid', required=True, type=int)
    parser.add_argument('--transaction', required=True)
    parser.add_argument('--scenario', choices=('continue', 'expired'), required=True)
    parser.add_argument('--execute', action='store_true')
    args = parser.parse_args()
    registry, provisioned = None, False
    result, code = {'native_pass': False, 'case': args.scenario}, 78
    try:
        require(args.execute, 'execute_required')
        require(ID.fullmatch(args.transaction) and args.transaction.startswith('gha-'), 'transaction_id')
        registry = REGISTRY / args.transaction
        provision(args.source, args.build, args.source_evidence, args.authorization_file,
                  args.authorization_sha256, args.uid, args.transaction)
        provisioned = True
        result, code = execute(registry, args.scenario), 0
    except (Refused, OSError, ValueError, KeyError, subprocess.SubprocessError) as error:
        result['code'] = str(error) if isinstance(error, Refused) else type(error).__name__
        if isinstance(error, CommandFailed):
            result['command_failure'] = error.diagnostic
    finally:
        # provision() starts no service. Only its successful return gives this
        # invocation ownership; a pre-existing registry is never that proof.
        if provisioned and (registry / 'stage0-registration.json').is_file():
            registration, _ = read_json(registry / 'stage0-registration.json')
            # Stop only this transaction's fixed units. No removal/reset-failed,
            # restart, forced kill, or other service operation is available.
            cleanup = []
            for name in sorted({row['unit'] for row in registration['invocations']}):
                try:
                    stopped = subprocess.run(['/usr/bin/systemctl', 'stop', name], capture_output=True, timeout=150)
                    actual = properties(name)
                    cleanup.append({'unit': name, 'returncode': stopped.returncode, 'properties': actual})
                    if stopped.returncode or actual['MainPID'] != '0' or actual['ControlPID'] != '0':
                        result.update(native_pass=False, cleanup='unverified')
                        code = 78
                except (Refused, OSError, subprocess.SubprocessError) as error:
                    cleanup.append({'unit': name, 'readback': 'unavailable', 'error_type': type(error).__name__})
                    result.update(native_pass=False, cleanup='unverified')
                    code = 78
            exclusive_json(registry / 'fixture-evidence/cleanup.json', {'recorded_at': utc(), 'units': cleanup})
            exclusive_json(registry / 'fixture-evidence/result.json', {**result, 'recorded_at': utc(), 'exit_code': code})
            profile = object_json(file_reference(registration['host_profile']))
            export(registry, args.export_directory, profile['uid'], profile['gid'])
        elif registry is not None and not args.export_directory.exists():
            args.export_directory.mkdir(mode=0o700)
            import pwd
            account = pwd.getpwuid(args.uid)
            os.chown(args.export_directory, account.pw_uid, account.pw_gid)
            exclusive_json(args.export_directory / 'result.json', {**result, 'recorded_at': utc(), 'exit_code': code},
                           uid=account.pw_uid, gid=account.pw_gid)
    print('native_fixture=' + ('passed' if code == 0 else 'unverified') + ' case=' + args.scenario
          + (' code=' + result['code'] if 'code' in result else ''))
    return code


if __name__ == '__main__':
    raise SystemExit(main())
