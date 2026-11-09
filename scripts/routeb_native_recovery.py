#!/usr/bin/env python3
"""Fixed native full restore and independent old start for one isolated root.

The root peer pins the real snapshot and both approvals after backup. This
program never registers authority, restores audit state, or refunds a ticket.
"""
import sys
sys.dont_write_bytecode = True
sys.pycache_prefix = None
if __name__ == '__main__':
    import os
    from pathlib import Path
    import stat
    location = Path(__file__).absolute()
    try:
        if not sys.flags.isolated or not sys.flags.no_site or os.getuid() == 0 or location.resolve() != location:
            raise ValueError()
        for path in (location, *location.parents):
            info = path.lstat()
            if info.st_uid != 0 or info.st_mode & 0o022 or stat.S_ISLNK(info.st_mode) or os.access(path, os.W_OK):
                raise ValueError()
        sys.path.insert(0, str(location.parent))
    except (OSError, ValueError):
        print('code=trusted_recovery_entry_required', file=sys.stderr)
        raise SystemExit(78)

from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import time

from upgrade import directory_fd, private_json
from upgrade_backup import durable_bytes, group_object
from upgrade_backup_contract import (ContractError, canonical, digest, fields, fingerprint,
                                     identifier, require, strict_json, timestamp)
from upgrade_identity import read_trusted
from upgrade_inventory import file_digest
from upgrade_recovery import ColdWindow, read_original_binding
from routeb_full_restore import apply_restore, old_start, plan_restore, verify_recovered_group
from routeb_host_native import FixedManager, WriterProbe
from routeb_host_package import load as load_package, read_document, NATIVE_PROFILE_SCHEMA
from routeb_native_delegation import is_delegated, require_delegation
import routeb_native_capture as capture
import routeb_stage0_client as client
import upgrade_state

REGISTRY = Path('/usr/local/libexec/ags-routeb-native')
LATE_FIELDS = ('schema registration_id registration_sha256 host_instance tool_manifest_sha256 '
               'interpreter_authority_sha256 backup_directory snapshot_id manifest_sha256 group_sha256 '
               'restore_approval old_start_approval').split()
APPROVAL_FIELDS = ('schema approval_id actor purpose decision issued_at expires_at host_instance '
                   'snapshot_id manifest_sha256 data_loss_acknowledged external_reconciliation_acknowledged').split()


def read_reference(reference):
    fields(reference, ('path', 'sha256', 'size'))
    digest(reference['sha256'])
    require(type(reference['size']) is int and 0 < reference['size'] <= 16 * 1024 * 1024,
            'recovery_reference_invalid')
    value, raw, sha = read_document(reference['path'])
    require(sha == reference['sha256'] and len(raw) == reference['size'], 'recovery_reference_changed')
    return value


def recovery_binding(root, *, operations, expected=None):
    root = Path(root)
    context = client.context(root, operations=operations)
    require(expected is None or expected == context, 'recovery_context_changed')
    raw = read_trusted(REGISTRY / context['registration_id'] / 'recovery-registration.json', maximum=1024 * 1024)
    require(hashlib.sha256(raw).hexdigest() == context['recovery_registration_sha256'], 'recovery_registration_changed')
    late = strict_json(raw)
    fields(late, LATE_FIELDS)
    require(late['schema'] == 'ags.routeb-stage0-recovery.v1'
            and all(late[key] == context[key] for key in
                    ('registration_sha256', 'tool_manifest_sha256', 'interpreter_authority_sha256'))
            and late['registration_id'] == context['registration_id']
            and late['backup_directory'] == str(root.parent / 'recovery/backups' / late['snapshot_id']),
            'recovery_registration_binding')
    identifier(late['snapshot_id']); digest(late['manifest_sha256']); digest(late['group_sha256'])
    for key in ('restore_approval', 'old_start_approval'):
        path = Path(late[key]['path'])
        require(path.parent == root.parent / 'recovery/approvals', 'recovery_approval_path')
    return context, late


def approval(late, purpose, root, *, check_time=True):
    key = 'restore_approval' if purpose == 'full-restore' else 'old_start_approval'
    value = read_reference(late[key])
    fields(value, (*APPROVAL_FIELDS, 'delegation') if is_delegated(value, purpose) else APPROVAL_FIELDS)
    require(value['schema'] in ('ags.routeb-recovery-approval.v1', 'ags.routeb-recovery-approval.v2')
            and value['decision'] == 'approve' and value['purpose'] == purpose
            and value['data_loss_acknowledged'] is True and value['external_reconciliation_acknowledged'] is True
            and all(value[key] == late[key] for key in ('host_instance', 'snapshot_id', 'manifest_sha256')),
            'recovery_approval_binding')
    identifier(value['approval_id'])
    now = datetime.now(timezone.utc)
    issued, expires = timestamp(value['issued_at']), timestamp(value['expires_at'])
    require(issued < expires and (not check_time or issued <= now < expires), 'recovery_approval_expired')
    if is_delegated(value, purpose):
        require_delegation(value, purpose, root=root, host_instance=late['host_instance'])
    else:
        require(type(value['actor']) is str and value['actor'].startswith('human:') and len(value['actor']) > 6,
                'recovery_approval_actor')
    return value


def result_directory(root):
    path = Path(root).parent / 'recovery/native-recovery'
    with directory_fd(path.parent, private=True) as parent:
        try:
            os.mkdir(path.name, 0o700, dir_fd=parent)
            os.fsync(parent)
        except FileExistsError:
            pass
    with directory_fd(path, private=True):
        pass
    return path


class DurableEffects:
    def __init__(self, directory, context):
        self.directory, self.context, self.entries = directory, context, []

    def append(self, effect):
        value = {'schema': 'ags.routeb-native-recovery-effect.v1', 'context_sha256': fingerprint(self.context),
                 'sequence': len(self.entries) + 1, 'effect': effect}
        durable_bytes(self.directory / ('effect-%06d.json' % value['sequence']), canonical(value))
        self.entries.append(value)


def closed(root, generation):
    result = client.writer_probe(root, generation)
    require(result == {'generation': generation, 'closed': True, 'writers': [], 'unknown': []},
            'writers_not_quiescent')
    return result


def restored_result(root, context, late):
    proof = client.restore_result(root)
    fields(proof, ('schema', 'registration_sha256', 'recovery_registration_sha256', 'restore_invocation_id',
                   'stopped_invocation_id', 'boot_id', 'context_sha256', 'result'))
    require(proof['schema'] == 'ags.routeb-stage0-restoration.v1'
            and all(proof[key] == context[key] for key in
                    ('registration_sha256', 'recovery_registration_sha256', 'boot_id'))
            and proof['result']['path'] == str(Path(root).parent / 'recovery/native-recovery/restore-result.json'),
            'recovery_completion_binding')
    value = read_reference(proof['result'])
    fields(value, ('schema', 'root', 'registration_sha256', 'recovery_registration_sha256', 'snapshot_id',
                   'manifest_sha256', 'group_sha256', 'restored', 'verification'))
    require(value['schema'] == 'ags.routeb-native-restore-result.v1' and value['root'] == str(root)
            and all(value[key] == context[key] for key in ('registration_sha256', 'recovery_registration_sha256'))
            and all(value[key] == late[key] for key in ('snapshot_id', 'manifest_sha256', 'group_sha256')),
            'recovery_result_binding')
    return value, proof


def consumed_window(root, context, late, package_path, approval_path):
    """Validate original consumption against the root-pinned sealed snapshot.

    This historical time is only for authenticating the original package. The
    live root peer and this operation's independent late approval admit effects.
    """
    pending = Path(root) / 'state/ags-autodeploy/local-v2/pending'
    binding = read_original_binding(pending)
    journal, _ = private_json(pending / 'host-execution.json')
    package, _, sha = read_document(package_path)
    _, _, approval_sha = read_document(approval_path)
    manifest, _, manifest_sha = read_document(Path(late['backup_directory']) / 'manifest.json')
    require(manifest_sha == late['manifest_sha256']
            and manifest.get('schema') == 'ags.backup-manifest.v2'
            and manifest.get('snapshot_id') == late['snapshot_id']
            and manifest.get('recovery_group_sha256') == late['group_sha256']
            and manifest.get('authority_evidence') is not None
            and manifest.get('binding') == binding, 'recovery_consumption_snapshot')
    require(journal.get('schema') == 'ags.routeb-host-journal.v1'
            and journal.get('root') == str(root)
            and journal.get('transaction_id') == binding.get('transaction_id') == context['registration_id']
            and journal.get('host_instance') == binding.get('host_instance') == late['host_instance']
            and journal.get('package_sha256') == sha
            and journal.get('approval_sha256') == approval_sha
            and journal.get('binding_sha256') == fingerprint(binding)
            and all(package.get(key) == binding.get(key) for key in
                    ('host_instance', 'transaction_id', 'attempt_id', 'authorization_id', 'tool_sha256', 'policy_sha256')),
            'recovery_consumption_binding')
    consumed = timestamp(journal['started_at'])
    require(consumed <= timestamp(manifest['started_at']) <= timestamp(manifest['ended_at'])
            <= datetime.now(timezone.utc), 'recovery_consumption_time')
    return consumed, binding


def authorize_recovery(package, context, late):
    consumed, binding = consumed_window(package.root, context, late, package.path, package.approval_path)
    require(package.cold_binding() == binding, 'recovery_consumption_binding')
    return package.authorize(consumed_at=consumed)


def packet(root, package_path, approval_path, operation):
    context, late = recovery_binding(root, operations=(operation,))
    profile_raw = read_trusted(REGISTRY / context['registration_id'] / 'host-profile.json', maximum=65536)
    host = strict_json(profile_raw)
    consumed, binding = consumed_window(root, context, late, package_path, approval_path)
    package = load_package(package_path, approval_path, host['logical_host'], consumed_at=consumed)
    require(package.root == Path(root) and package.profile['schema'] == NATIVE_PROFILE_SCHEMA
            and package.host.scope == 'isolated-native' and fingerprint(package.group) == late['group_sha256']
            and not package.execution_blockers(), 'native_recovery_package_blocked')
    require(package.cold_binding() == binding, 'recovery_consumption_binding')
    authorize_recovery(package, context, late)
    return context, late, package


def restore(root, package_path, approval_path):
    context, late, package = packet(root, package_path, approval_path, 'restore')
    decision = approval(late, 'full-restore', root)
    require(decision['approval_id'] == context['authorization_id'], 'recovery_approval_operation')
    manager = FixedManager(root, package.profile, package.host)
    binding = package.cold_binding()
    window = ColdWindow(root, binding, binding, package.group, package.dsn,
                        package.profile['schema_sha256'], manager, lambda: None, None)
    directory = result_directory(root)
    effects = DurableEffects(directory, context)
    request = {'group': package.group, 'dsn': package.dsn, 'schema_sha256': package.profile['schema_sha256'],
               'backup_dir': late['backup_directory'], 'snapshot_id': late['snapshot_id'],
               'approval_id': decision['approval_id'], 'transaction_id': context['registration_id']}
    planned = plan_restore(request)
    require(planned['manifest_sha256'] == late['manifest_sha256'], 'recovery_snapshot_mismatch')
    window.acquire_owner()
    try:
        recovery_binding(root, operations=('restore',), expected=context)
        authorize_recovery(package, context, late)
        approval(late, 'full-restore', root)
        manager.preflight(); manager.suppress_restart()
        effects.append({'effect_id': 'normal-stop', 'status': 'intent'})
        stopped = manager.stop()
        effects.append({'effect_id': 'normal-stop', 'status': 'observed', 'record_sha256': fingerprint(stopped)})
        window.acquire()
        closed(root, context['invocation_id'])
        recovery_binding(root, operations=('restore',), expected=context)
        decision = approval(late, 'full-restore', root)
        restored = apply_restore(request, supervisor_path=Path(__file__), journal=effects)
        verified = verify_recovered_group(restored, decision)
        closed(root, context['invocation_id'])
        recovery_binding(root, operations=('restore',), expected=context)
        value = {'schema': 'ags.routeb-native-restore-result.v1', 'root': str(root),
                 'registration_sha256': context['registration_sha256'],
                 'recovery_registration_sha256': context['recovery_registration_sha256'],
                 **{key: late[key] for key in ('snapshot_id', 'manifest_sha256', 'group_sha256')},
                 'restored': restored, 'verification': {**verified, 'effects_sha256': fingerprint(effects.entries)}}
        raw = canonical(value)
        durable_bytes(directory / 'restore-result.json', raw)
        completion = client.complete_restore(root, hashlib.sha256(raw).hexdigest())
        require(completion['result']['sha256'] == hashlib.sha256(raw).hexdigest(), 'recovery_completion_binding')
        return {'restored': True, 'started': False, 'completion': completion}
    finally:
        window.release(); window.release_owner()


def start_old(root, package_path, approval_path):
    context, late, package = packet(root, package_path, approval_path, 'old-start')
    value, proof = restored_result(root, context, late)
    decision = approval(late, 'old-start', root)
    require(decision['approval_id'] == context['authorization_id'], 'recovery_approval_operation')
    manager = FixedManager(root, package.profile, package.host)
    binding = package.cold_binding()
    window = ColdWindow(root, binding, binding, package.group, package.dsn,
                        package.profile['schema_sha256'], manager, lambda: None, None)
    directory = result_directory(root)
    window.acquire_owner()
    try:
        window.acquire()
        closed(root, context['invocation_id'])
        verified = verify_recovered_group(value['restored'], decision)
        closed(root, context['invocation_id'])
        recovery_binding(root, operations=('old-start',), expected=context)

        def launch():
            # The independent old-start token was durably consumed before
            # this callback. Releasing locks cannot refund either root ticket.
            window.release()
            manager.start()
            deadline = time.monotonic() + 95
            while time.monotonic() < deadline:
                try:
                    before = manager.preflight()
                    require(before['stage0']['phase'] == 'serve-old', 'old_generation_unproven')
                    facts, _ = capture.health(package.profile['capture']['health']['local'])
                    require(facts['http_status'] == 200 and facts['status'] == 'ready'
                            and facts['revision'] == package.policy['old_revision'], 'health_not_ready')
                    return {'started': True, 'generation': before['runtime']['generation']}
                except (ContractError, OSError, KeyError, ValueError):
                    time.sleep(.2)
            raise ContractError('old_start_observation_unknown')

        result = old_start(value['restored'], decision, launcher=launch)
        durable_bytes(directory / 'old-start-result.json', canonical({
            'schema': 'ags.routeb-native-old-start-result.v1', 'context': context,
            'restore_proof_sha256': fingerprint(proof), 'verification': verified, 'result': result}))
        return result
    finally:
        window.release(); window.release_owner()


def old_start_binding(root, expected=None, *, verify_data=True):
    """A serve-old generation requires a live peer, never a callback flag."""
    context, late = recovery_binding(root, operations=('serve',), expected=expected)
    require(context['phase'] == 'serve-old', 'old_start_native_phase_required')
    value, proof = restored_result(root, context, late)
    # Expiry fences a new spawn. A living, already consumed generation keeps
    # its exact root-bound authorization; it cannot use this path to respawn.
    decision = approval(late, 'old-start', root, check_time=verify_data)
    require(decision['approval_id'] == context['authorization_id'], 'old_start_authorization_changed')
    if verify_data:
        verified = verify_recovered_group(value['restored'], decision)
        marker = Path(verified['journal_root']) / ('.old-start-' + decision['approval_id'])
        consumed, _ = private_json(marker)
        require(consumed == {'schema': 'ags.old-start-consumed.v1', 'snapshot_id': late['snapshot_id'],
                             'approval_id': decision['approval_id'], 'manifest_sha256': late['manifest_sha256']},
                'old_start_not_consumed')
    loaded = upgrade_state.load_verified(root)
    projection = loaded.get('projection') or {}
    authority = projection.get('authority') or {}
    require(authority.get('state') == 'active' and authority.get('epoch') == 1
            and not projection.get('pending_effects'), 'old_start_authority_inactive')
    return context, value, proof, loaded


def consume_old_generation(root, expected):
    context, _, proof, _ = old_start_binding(root, expected)
    generation = 'old-' + context['invocation_id']
    identifier(generation)
    durable_bytes(result_directory(root) / ('generation-' + context['invocation_id'] + '.json'), canonical({
        'schema': 'ags.routeb-native-old-generation.v1', 'generation': generation,
        'context_sha256': fingerprint(context), 'restore_proof_sha256': fingerprint(proof)}))
    return generation


def old_generation_running(root, generation, child_pid, expected):
    context, value, _, loaded = old_start_binding(root, expected, verify_data=False)
    require(generation == 'old-' + context['invocation_id'] and type(child_pid) is int and child_pid > 1,
            'old_generation_binding')
    runtime, _ = private_json(Path(root) / 'state/runtime.json')
    require(runtime.get('stage0') == context and runtime.get('generation') == generation
            and runtime.get('supervisor_pid') == os.getpid() and runtime.get('pid') == child_pid
            and 'stopped_at' not in runtime and loaded['projection']['generation'] == generation,
            'old_generation_binding')
    group = strict_json((Path(value['restored']['recovery_context']['backup_dir']) / 'recovery-group.json').read_bytes())
    require(fingerprint(group) == value['group_sha256'], 'recovery_group_mismatch')
    for role in ('old_binary', 'installation'):
        item = group_object(group, role)
        require(file_digest(item['source']) == file_digest(Path(value['restored']['recovery_context']['backup_dir']) / item['id']),
                'old_generation_identity_changed')
    facts = capture.process_identity(child_pid, Path(root) / 'bin/gh-server')
    require(facts['parent_pid'] == os.getpid(), 'old_generation_parent_changed')
    return True


def main():
    import argparse
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=('restore', 'old-start'))
    parser.add_argument('--root', required=True, type=Path)
    parser.add_argument('--package', required=True, type=Path)
    parser.add_argument('--approval', required=True, type=Path)
    args = parser.parse_args()
    try:
        result = (restore if args.command == 'restore' else start_old)(args.root, args.package, args.approval)
        print(json.dumps(result, sort_keys=True))
        return 0
    except Exception as error:
        code = str(error) if isinstance(error, ContractError) else 'native_recovery_failed'
        if not code.isascii() or not code.replace('_', '').isalnum() or len(code) > 96:
            code = 'native_recovery_failed'
        print('code=' + code, file=sys.stderr)
        return 78


if __name__ == '__main__':
    raise SystemExit(main())
