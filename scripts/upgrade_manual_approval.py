#!/usr/bin/env python3
"""Manual acceptance only. No install/start/stop, transport or automatic approval.

Owner-only declarations and hashes are not signatures. A same-UID/root writer can
forge the entire history. The operator must authenticate carried evidence out of
band. Completion is one atomic record containing both receipt and consumption;
claim archival is a recoverable, idempotent projection of that record.
"""
from contextlib import contextmanager
from datetime import datetime, timezone
import copy
import fcntl
import hashlib
import json
import os
from pathlib import Path
import stat
import tempfile

from upgrade import Failure, directory_fd, private_json, read_private_fd, validate_pin
from upgrade_backup_contract import (canonical, digest, fields, fingerprint, identifier,
                                     require, text, timestamp, validate_binding, strict_json, ContractError)
from upgrade_backup import durable_bytes, group_object
from upgrade_inventory import file_digest
from upgrade_recovery import ColdWindow, read_window, read_original_binding, reconcile as cold_readback
from upgrade_engine import FLAGS, authorize

CONFIRMATIONS = {'start_restores_business': True, 'rollback_mode': 'forward-only',
                 'prestart_recovery_requires_separate_approval': True,
                 'manual_provenance_only': True, 'no_zero_loss_poststart_rollback': True}
LIMITATIONS = frozenset(('native_platform_unverified', 'real_copy_start_unverified',
                         'long_tail_observation_unverified'))
APPROVAL_FIELDS = ('schema approval_id purpose decision actor recorded_at authority_reference issued_at '
                   'expires_at binding backup_binding evidence_packet_sha256 evidence_id generation '
                   'confirmations accepted_limitations independent_media unresolved_blockers').split()
PACKET_FIELDS = ('schema binding backup_binding generation journal_sha256 cold_journal_sha256 '
                 'snapshot_id manifest_sha256 restore_report_sha256 inventory_sha256 '
                 'quiescence installation_sha256 binary_sha256 running observation release_key handoff').split()
RECORD_FIELDS = ('schema status binding generation approval_id approval_sha256 evidence_packet_sha256 '
                 'completed_at receipt consumed_approval').split()


def checkpoint(name):
    """Test-only crash seam; never configurable from CLI or policy."""
    return name


def read_document(path):
    """Reuse the S1-era private reader, additionally exclude hard-linked declarations."""
    path = Path(path)
    with directory_fd(path.parent, private=True) as parent:
        fd = os.open(path.name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=parent)
        try:
            require(os.fstat(fd).st_nlink == 1, 'hardlink_forbidden')
            return read_private_fd(fd, maximum=16 * 1024 * 1024)
        finally:
            os.close(fd)


@contextmanager
def hostops(root):
    """Nonblocking S3 hostops lock, never TTL-steal or clear a pending claim."""
    parent = Path(root) / 'state/ags-autodeploy/local-v2'
    with directory_fd(parent, private=True) as directory:
        # Transactions already created this lock. Queries never create anything.
        fd = os.open('hostops.lock', os.O_RDWR | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=directory)
    try:
        info = os.fstat(fd)
        require(stat.S_ISREG(info.st_mode) and stat.S_IMODE(info.st_mode) == 0o600
                and info.st_uid == os.getuid() and info.st_nlink == 1, 'hostops_unsafe')
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise Failure('lock_held') from None
        yield parent
    finally:
        os.close(fd)


def release_key(pin):
    validate_pin(pin)
    return {'repository': pin['repository'], 'repository_id': pin['repository_id'],
            'release_id': pin['release_id'], 'release_tag': pin['version'],
            'runtime': copy.deepcopy(pin['runtime']),
            'targets': {key: {'asset_sha256': value['asset']['digest'][7:],
                             'control_revision': value['provenance']['control_revision']}
                        for key, value in pin['targets'].items()}}


def check_pin(pin, pin_sha, binding):
    key = release_key(pin)
    require(pin_sha == binding['pin_sha256'] and pin['runtime'] == {
        'revision': binding['new'], 'tree': binding['runtime_tree']}
        and pin['release_id'] == binding['release_id'] and pin['version'] == binding['release_tag']
        and pin['targets'][binding['target']]['asset']['digest'] == 'sha256:' + binding['asset_sha256']
        and pin['targets'][binding['target']]['binaries']['gh-server']['sha256'] == binding['new_binary_sha256']
        and pin['targets'][binding['target']]['provenance']['control_revision'] == binding['control_revision']
        and pin['targets'][binding['target']]['provenance']['control_tree'] == binding['control_tree']
        and pin['verification_policy']['policy_sha256'] == binding['policy_sha256']
        and pin['verification_policy']['tool_bundle_manifest_sha256'] == binding['tool_sha256'],
        'pin_binding_mismatch')
    return key


def validate_request(request, backup):
    # Retain the existing v2 request identity; S1 supplies the additional host
    # instance and transaction ID rather than inventing a second claim schema.
    fields(request, ('host', 'deployment_id', 'attempt_id', 'pin_sha256', 'old', 'new',
                     'runtime_tree', 'release_id', 'engine_sha256', 'policy_sha256', 'authorization_id'))
    require(type(request['host']) is str and request['host'] in ('mini', 'imile-win'), 'host_invalid')
    require(type(request['release_id']) is int, 'request_binding_mismatch')
    require(all(type(request[k]) is type(backup[k]) and request[k] == backup[k]
                for k in request if k not in ('host', 'engine_sha256'))
            and request['engine_sha256'] == backup['tool_sha256']
            and backup['target'] == ('darwin_arm64' if request['host'] == 'mini' else 'linux_amd64'),
            'request_binding_mismatch')


def load_chain(root, expected):
    """Do not accept a journal/success JSON instead of actual S1/S2/S3 artifacts."""
    root = Path(root)
    marker, journal, pending = read_window(root)
    require(read_original_binding(pending, expected) == expected, 'claim_mismatch')
    require(journal['phase'] == 'start_attempted' and journal['start_attempted'] is True
            and journal['handoff']['consumed'] is True and journal['handoff']['manager_requested'] is True,
            'not_awaiting_acceptance')
    binding = journal['backup_binding']
    validate_binding(binding)
    validate_request(expected, binding)
    tx, _ = read_document(pending / 'journal.json')
    fields(tx, ('schema', 'binding', 'phase', *FLAGS))
    require(tx['schema'] == 'ags.local-upgrade-journal.v2' and tx['binding'] == expected
            and tx['phase'] == 'awaiting_observation'
            and all(tx[k] is True for k in FLAGS), 'transaction_incomplete')
    waiting, _ = read_document(pending / 'receipt.json')
    fields(waiting, ('schema', 'binding', 'status', 'rollback_mode', 'authorization_id', 'authorization_sha256'))
    digest(waiting['authorization_sha256'])
    authorization, _ = read_document(root / 'config' / ('upgrade-authorization-' + binding['authorization_id'] + '.json'))
    authorize(authorization, expected, now=timestamp(authorization['issued_at']))
    require(authorization.get('authorization_id') == binding['authorization_id']
            and authorization['actor']['type'] == 'human'
            and hashlib.sha256(json.dumps(authorization, sort_keys=True).encode()).hexdigest()
            == waiting['authorization_sha256'], 'authorization_chain_invalid')
    require(waiting['schema'] == 'ags.local-upgrade-receipt.v2' and waiting['binding'] == expected
            and waiting['status'] == 'awaiting_observation' and waiting['rollback_mode'] == 'forward-only'
            and waiting['authorization_id'] == binding['authorization_id'], 'transaction_incomplete')
    backup = Path(journal['backup_dir'])
    group, _ = private_json(backup / 'recovery-group.json', maximum=16 * 1024 * 1024)
    inventory, _ = private_json(backup / 'inventory.json', maximum=16 * 1024 * 1024)
    # Reuse S3's full S1/S2 validator, including payload, inventory, restore and publication.
    window = object.__new__(ColdWindow)
    window.journal, window.backup_binding, window.group = journal, binding, group
    window.schema = inventory['schema_sha256']
    window.dsn = 'sqlite:' + group_object(group, 'sqlite')['source']
    path, manifest = window.verified_backup()
    require(group_object(group, 'old_binary')['source'] == str(root / 'bin/gh-server')
            and group_object(group, 'installation')['source'] == str(root / 'state/installation.json'),
            'installation_mapping_mismatch')
    receipt, _ = read_document(root / 'state/installation.json')
    require(receipt['source'] == binding['new'] and receipt['runtime_tree'] == binding['runtime_tree']
            and receipt['control_revision'] == binding['control_revision']
            and receipt['version'] == binding['release_tag'] and receipt['release_id'] == binding['release_id']
            and receipt['binaries']['gh-server']['sha256'] == binding['new_binary_sha256']
            and file_digest(root / 'bin/gh-server') == binding['new_binary_sha256']
            and file_digest(root / 'state/installation.json') == journal['installed_receipt_sha256']
            and journal['handoff']['generation'] == marker['generation']
            and journal['handoff']['binary_sha256'] == binding['new_binary_sha256']
            and journal['handoff']['receipt_sha256'] == journal['installed_receipt_sha256'],
            'installed_identity_mismatch')
    return {'binding': copy.deepcopy(expected), 'backup_binding': binding,
            'generation': marker['generation'], 'journal_sha256': file_digest(pending / 'journal.json'),
            'cold_journal_sha256': file_digest(pending / 'cold-journal.json'),
            'snapshot_id': manifest['snapshot_id'], 'manifest_sha256': file_digest(path / 'manifest.json'),
            'restore_report_sha256': file_digest(path / 'restore-report.json'),
            'inventory_sha256': manifest['inventory_sha256'], 'quiescence': manifest['quiescence'],
            'installation_sha256': journal['installed_receipt_sha256'],
            'binary_sha256': binding['new_binary_sha256']}


def check_running(root, chain, probe, now):
    runtime, runtime_sha = read_document(Path(root) / 'state/runtime.json')
    binding = chain['backup_binding']
    require(runtime.get('root') == str(root) and runtime.get('generation') == chain['generation']
            and 'stopped_at' not in runtime and runtime.get('build', {}).get('revision') == binding['new'],
            'running_generation_invalid')
    started = timestamp(runtime['started_at'])
    require(started <= now, 'running_generation_invalid')
    result = probe()
    require(read_document(Path(root) / 'state/runtime.json')[1] == runtime_sha, 'running_evidence_changed')
    fields(result, ('generation', 'running_revision', 'local', 'service'))
    require(result['generation'] == chain['generation'] and result['running_revision'] == binding['new'],
            'health_generation_invalid')
    for key in ('local', 'service'):
        fields(result[key], ('status', 'revision'))
        require(result[key] == {'status': 'ready', 'revision': binding['new']}, 'health_unknown')
    return {'generation': chain['generation'], 'started_at': runtime['started_at'],
            'revision': binding['new'], 'runtime_sha256': runtime_sha,
            'health_sha256': fingerprint(result)}


def check_observation(value, running, now):
    fields(value, ('generation', 'started_at', 'ended_at', 'reports', 'unresolved_blockers'))
    require(value['generation'] == running['generation'] and value['unresolved_blockers'] == [],
            'observation_invalid')
    require(timestamp(running['started_at']) <= timestamp(value['started_at'])
            <= timestamp(value['ended_at']) <= now, 'observation_invalid')
    fields(value['reports'], ('business', 'git', 'provider', 'recovery'))
    for row in value['reports'].values():
        fields(row, ('status', 'sha256'))
        require(row['status'] in ('pass', 'not_applicable'), 'observation_invalid')
        digest(row['sha256'])


def check_packet(value):
    fields(value, PACKET_FIELDS)
    require(value['schema'] == 'ags.manual-evidence.v1', 'packet_invalid')
    validate_binding(value['backup_binding'])
    validate_request(value['binding'], value['backup_binding'])
    identifier(value['generation']); identifier(value['snapshot_id'])
    for key in PACKET_FIELDS:
        if key.endswith('_sha256'):
            digest(value[key])
    fields(value['running'], ('generation', 'started_at', 'revision', 'runtime_sha256', 'health_sha256'))
    require(value['running']['generation'] == value['generation']
            and value['running']['revision'] == value['backup_binding']['new'], 'packet_invalid')
    timestamp(value['running']['started_at'])
    digest(value['running']['runtime_sha256']); digest(value['running']['health_sha256'])


def make_packet(root, binding, pin, pin_sha, observation, probe, *, handoff=None, now=None):
    """Private executor API: fixed evidence index, no authority to accept anything."""
    now = now or datetime.now(timezone.utc)
    chain = load_chain(root, binding)
    key = check_pin(pin, pin_sha, chain['backup_binding'])
    running = check_running(root, chain, probe, now)
    check_observation(observation, running, now)
    if binding.get('host') == 'imile-win':
        validate_handoff(handoff, key, pin_sha, now)
    else:
        require(binding.get('host') == 'mini' and handoff is None, 'unexpected_handoff')
    return {'schema': 'ags.manual-evidence.v1', **chain, 'running': running,
            'observation': copy.deepcopy(observation), 'release_key': key, 'handoff': copy.deepcopy(handoff)}


def write_packet(path, packet):
    """Publish a fixed canonical index before a human records its raw digest."""
    check_packet(packet)
    with directory_fd(Path(path).parent, private=True):
        durable_bytes(Path(path), canonical(packet))
    return fingerprint(packet)


def write_request(path, packet, *, now=None):
    """The only approval-file writer in the executor always emits pending."""
    check_packet(packet)
    now = now or datetime.now(timezone.utc)
    request = {'schema': 'ags.manual-upgrade-approval.v1', 'approval_id': 'pending-' + packet['generation'],
               'purpose': 'accept', 'decision': 'pending',
               'actor': {'type': 'human', 'id': 'pending', 'source': 'pending'},
               'recorded_at': now.isoformat(), 'authority_reference': 'pending',
               'issued_at': now.isoformat(), 'expires_at': now.isoformat(),
               'binding': packet['binding'], 'backup_binding': packet['backup_binding'],
               'evidence_packet_sha256': fingerprint(packet), 'evidence_id': packet['generation'],
               'generation': packet['generation'], 'confirmations': CONFIRMATIONS,
               'accepted_limitations': [], 'independent_media': {'status': 'omitted', 'evidence': 'pending',
                                                               'reason': 'pending'},
               'unresolved_blockers': []}
    with directory_fd(Path(path).parent, private=True):
        durable_bytes(Path(path), canonical(request))
    return request


def validate_approval(value, packet, packet_sha, now, *, consumed=False):
    fields(value, APPROVAL_FIELDS)
    require(value['schema'] == 'ags.manual-upgrade-approval.v1' and value['purpose'] == 'accept'
            and value['decision'] == 'approve', 'manual_approval_invalid')
    identifier(value['approval_id']); identifier(value['evidence_id'])
    fields(value['actor'], ('type', 'id', 'source'))
    require(value['actor']['type'] == 'human', 'manual_source_invalid')
    require(value['authority_reference'] == packet['backup_binding']['authorization_id'], 'manual_source_invalid')
    for item in (*value['actor'].values(), value['authority_reference']):
        text(item); require(item != 'pending', 'manual_source_invalid')
    issued, expires, recorded = (timestamp(value[k]) for k in ('issued_at', 'expires_at', 'recorded_at'))
    require(issued <= recorded <= now and issued < expires and recorded < expires
            and (consumed or now < expires), 'manual_approval_expired')
    require(timestamp(packet['observation']['ended_at']) <= recorded, 'manual_approval_stale')
    validate_binding(value['backup_binding'])
    validate_request(value['binding'], value['backup_binding'])
    require(value['binding'] == packet['binding'] and value['backup_binding'] == packet['backup_binding']
            and value['generation'] == packet['generation']
            and value['evidence_id'] == packet['generation']
            and value['evidence_packet_sha256'] == packet_sha, 'manual_binding_mismatch')
    fields(value['confirmations'], CONFIRMATIONS)
    require(all(type(value['confirmations'][k]) is type(v) and value['confirmations'][k] == v
                for k, v in CONFIRMATIONS.items()) and value['unresolved_blockers'] == [], 'manual_boundary_invalid')
    require(type(value['accepted_limitations']) is list, 'limitations_invalid')
    seen = set()
    for row in value['accepted_limitations']:
        fields(row, ('category', 'reason'))
        require(row['category'] in LIMITATIONS and row['category'] not in seen, 'limitations_invalid')
        seen.add(row['category']); text(row['reason'])
    media = value['independent_media']
    fields(media, ('status', 'evidence', 'reason'))
    require(media['status'] in ('done', 'omitted'), 'media_invalid')
    text(media['evidence']); text(media['reason'])


def validate_completion(record, binding):
    """Historical record invariants shared by confirm, handoff and read-only queries."""
    fields(record, RECORD_FIELDS)
    require(record['schema'] == 'ags.manual-completion.v1' and record['status'] == 'completed'
            and record['binding'] == binding, 'completion_mismatch')
    identifier(record['generation']); identifier(record['approval_id'])
    digest(record['approval_sha256']); digest(record['evidence_packet_sha256'])
    timestamp(record['completed_at'])
    require(record['consumed_approval'] == {'approval_id': record['approval_id'],
                                          'sha256': record['approval_sha256']}
            and record['receipt'] == {'schema': 'ags.local-upgrade-receipt.v2', 'status': 'completed',
                'binding': binding, 'generation': record['generation'], 'rollback_mode': 'forward-only',
                'approval_sha256': record['approval_sha256'],
                'evidence_packet_sha256': record['evidence_packet_sha256']}, 'completion_mismatch')


def validate_record(record, packet, approval, approval_sha, packet_sha):
    validate_completion(record, packet['binding'])
    require(record['generation'] == packet['generation']
            and record['approval_id'] == approval['approval_id'] and record['approval_sha256'] == approval_sha
            and record['evidence_packet_sha256'] == packet_sha, 'completion_mismatch')
    completed = timestamp(record['completed_at'])
    require(timestamp(approval['recorded_at']) <= completed < timestamp(approval['expires_at']),
            'completion_time_invalid')


def validate_handoff(value, key, imile_pin_sha, now):
    """Human-carried private package, never a network/source authentication claim."""
    if type(value) is dict and value.get('schema') == 'ags.manual-mini-handoff.v2':
        from upgrade_handoff import validate
        return validate(value, key, imile_pin_sha, now)
    fields(value, ('schema', 'mini_pin', 'mini_pin_sha256', 'imile_pin_sha256', 'mini_packet',
                   'mini_evidence_package_sha256', 'mini_completion', 'mini_completed_receipt_sha256',
                   'mini_approval', 'mini_approval_sha256', 'release_key', 'mini_generation',
                   'mini_health_observed_at', 'carried_by', 'source', 'received_at', 'readback', 'artifacts'))
    require(value['schema'] == 'ags.manual-mini-handoff.v1' and value['release_key'] == key
            and value['imile_pin_sha256'] == imile_pin_sha, 'handoff_release_mismatch')
    mini = value['mini_packet']
    check_packet(mini)
    require(mini['binding'].get('host') == 'mini' and mini['handoff'] is None
            and mini['backup_binding']['target'] == 'darwin_arm64'
            and mini['release_key'] == key and value['mini_generation'] == mini['generation'], 'mini_result_invalid')
    require(check_pin(value['mini_pin'], value['mini_pin_sha256'], mini['backup_binding']) == key,
            'handoff_release_mismatch')
    check_observation(mini['observation'], mini['running'], now)
    # Preserve original received bytes. Re-serializing JSON would lose the raw
    # digest used by the mini completion record and could reject valid handoffs.
    fields(value['artifacts'], ('pin', 'packet', 'approval', 'completion'))
    for name, parsed, expected_sha in (
            ('pin', value['mini_pin'], value['mini_pin_sha256']),
            ('packet', mini, value['mini_evidence_package_sha256']),
            ('approval', value['mini_approval'], value['mini_approval_sha256']),
            ('completion', value['mini_completion'], value['mini_completed_receipt_sha256'])):
        raw = value['artifacts'][name]
        require(type(raw) is str, 'handoff_artifact_invalid')
        raw = raw.encode('utf-8')
        require(fingerprint(strict_json(raw)) == fingerprint(parsed)
                and hashlib.sha256(raw).hexdigest() == expected_sha,
                'handoff_digest_mismatch')
    packet_sha = value['mini_evidence_package_sha256']
    validate_approval(value['mini_approval'], mini, packet_sha, now, consumed=True)
    validate_record(value['mini_completion'], mini, value['mini_approval'], value['mini_approval_sha256'], packet_sha)
    observed, received = timestamp(value['mini_health_observed_at']), timestamp(value['received_at'])
    require(timestamp(value['mini_completion']['completed_at']) <= observed <= received <= now
            and (now - observed).total_seconds() <= 300, 'handoff_stale')
    for item in (value['carried_by'], value['source']):
        text(item)
    require(type(value['readback']) is dict and value['readback'].get('confirmed') is True
            and value['readback'] == {'confirmed': True, 'mini_generation': mini['generation'],
                                 'mini_completed_receipt_sha256': value['mini_completed_receipt_sha256'],
                                 'mini_evidence_package_sha256': packet_sha}, 'handoff_readback_invalid')


def archive_claim(state, record):
    """Only finish the same record's archive projection. Never replay an effect."""
    pending = state / 'pending'
    archive = state / ('accepted-' + record['generation'])
    if pending.exists():
        require(not archive.exists(), 'archive_conflict')
        read_original_binding(pending, record['binding'])
        with directory_fd(state, private=True) as parent:
            os.rename('pending', archive.name, src_dir_fd=parent, dst_dir_fd=parent)
            os.fsync(parent)
        checkpoint('claim-archived')
    require(archive.is_dir() and not archive.is_symlink(), 'archive_missing')
    read_original_binding(archive, record['binding'])


def completion_path(state, binding):
    # Request identity, not CI attempt alone. Original binding remains immutable.
    return state / ('completed-' + fingerprint(binding) + '.json')


def _confirm(root, binding, approval_path, packet_path, pin, pin_sha, probe, clock):
    now = clock()
    with hostops(root) as state:
        if approval_path is None:
            raise Failure('awaiting_observation', 'confirm', 86)
        try:
            approval, approval_sha = read_document(approval_path)
        except FileNotFoundError:
            raise Failure('awaiting_observation', 'confirm', 86) from None
        packet, packet_sha = read_document(packet_path)
        check_packet(packet)
        require(packet['binding'] == binding, 'claim_mismatch')
        completed = completion_path(state, binding)
        if completed.exists() or completed.is_symlink():
            record, _ = read_document(completed)
            validate_approval(approval, packet, packet_sha, now, consumed=True)
            validate_record(record, packet, approval, approval_sha, packet_sha)
            # A durable completion is historical acceptance. Current health is
            # not reinterpreted and no upgrade effect is repeated after a crash.
            archive_claim(state, record)
            return record
        validate_approval(approval, packet, packet_sha, now)
        chain = load_chain(root, binding)
        require(all(packet[k] == v for k, v in chain.items()), 'evidence_chain_changed')
        key = check_pin(pin, pin_sha, chain['backup_binding'])
        require(packet['release_key'] == key, 'release_key_mismatch')
        if binding.get('host') == 'imile-win':
            require(chain['backup_binding']['target'] == 'linux_amd64', 'target_invalid')
            validate_handoff(packet['handoff'], key, pin_sha, now)
        else:
            require(binding.get('host') == 'mini' and packet['handoff'] is None, 'host_invalid')
        check_observation(packet['observation'], packet['running'], now)
        require(check_running(root, chain, probe, now) == packet['running'], 'running_evidence_changed')
        # Re-read the bound disk chain after health before committing acceptance.
        require(load_chain(root, binding) == chain, 'evidence_chain_changed')
        require(check_running(root, chain, probe, now) == packet['running'], 'running_evidence_changed')
        now = clock()
        validate_approval(approval, packet, packet_sha, now)
        if binding.get('host') == 'imile-win':
            validate_handoff(packet['handoff'], key, pin_sha, now)
        record = {'schema': 'ags.manual-completion.v1', 'status': 'completed', 'binding': binding,
                  'generation': packet['generation'], 'approval_id': approval['approval_id'],
                  'approval_sha256': approval_sha, 'evidence_packet_sha256': packet_sha,
                  'completed_at': now.isoformat(),
                  'consumed_approval': {'approval_id': approval['approval_id'], 'sha256': approval_sha},
                  'receipt': {'schema': 'ags.local-upgrade-receipt.v2', 'status': 'completed',
                              'binding': binding, 'generation': packet['generation'], 'rollback_mode': 'forward-only',
                              'approval_sha256': approval_sha, 'evidence_packet_sha256': packet_sha}}
        # Atomic publication of BOTH receipt and consumed approval in one inode.
        # A complete stage must match this acceptance. A torn JSON stage is not
        # consumption: retain its inode only after revalidating the live chain
        # and unexpired approval above. Queries never enter this recovery path.
        staging = completed.with_name('.' + completed.name + '.commit')
        candidate = None
        if staging.exists() or staging.is_symlink():
            try:
                candidate, _ = read_document(staging)
            except json.JSONDecodeError:
                retained = Path(tempfile.mkdtemp(prefix=staging.name + '.partial-', dir=state))
                with directory_fd(state, private=True) as parent, directory_fd(retained, private=True) as fragment:
                    os.rename(staging.name, staging.name, src_dir_fd=parent, dst_dir_fd=fragment)
                    os.fsync(fragment)
                    os.fsync(parent)
            else:
                validate_record(candidate, packet, approval, approval_sha, packet_sha)
                record = candidate
        if candidate is None:
            durable_bytes(staging, canonical(record))
        checkpoint('completion-staged')
        with directory_fd(state, private=True) as parent:
            os.rename(staging.name, completed.name, src_dir_fd=parent, dst_dir_fd=parent)
            os.fsync(parent)
        checkpoint('completion-durable')
        archive_claim(state, record)
        return record


def confirm(root, binding, approval_path, packet_path, pin, pin_sha, probe, *, now=None):
    try:
        clock = (lambda: now) if now is not None else (lambda: datetime.now(timezone.utc))
        return _confirm(Path(root), binding, approval_path, packet_path, pin, pin_sha, probe, clock)
    except Failure as error:
        if error.code in ('lock_held', 'awaiting_observation'):
            raise
        raise Failure('manual_approval_invalid', 'confirm', 77) from error
    except (ContractError, OSError, KeyError, ValueError, TypeError):
        raise Failure('manual_approval_invalid', 'confirm', 77) from None


def status(root, binding):
    """Queries return facts, not authorization. No file/lock creation or healing."""
    with hostops(root) as state:
        path = completion_path(state, binding)
        if path.exists() or path.is_symlink():
            record, _ = read_document(path)
            validate_completion(record, binding)
            archive = state / ('accepted-' + record['generation'])
            history = archive if archive.exists() or archive.is_symlink() else state / 'pending'
            claim, _ = read_document(history / 'claim.json')
            fields(claim, ('schema', 'binding'))
            require(claim['schema'] == 'ags.local-claim.v2' and claim['binding'] == binding, 'claim_mismatch')
            cold, _ = read_document(history / 'cold-journal.json')
            validate_binding(cold['backup_binding'])
            validate_request(binding, cold['backup_binding'])
            require(cold['schema'] == 'ags.cold-journal.v1' and cold['phase'] == 'start_attempted'
                    and cold['start_attempted'] is True and cold['generation'] == record['generation']
                    and cold['binding_sha256'] == fingerprint(binding), 'completion_mismatch')
            return {'query': 'read_only_no_replay', 'completion': record}
        read_original_binding(state / 'pending', binding)
        journal, _ = read_document(state / 'pending/journal.json')
        require(journal.get('binding') == binding and type(journal.get('phase')) is str, 'journal_invalid')
        return {'status': journal['phase'], **cold_readback(root)}
