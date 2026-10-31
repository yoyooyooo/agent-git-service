#!/usr/bin/env python3
"""Private cold-window protocol; no CLI, manager discovery or production apply entry.

Locks are advisory: the fixed adapter must prove closure of all other writers.
A durable marker is never expired or removed by this module. Recovery restores
only old program/receipt bytes, never business data and never starts a process.
"""
from contextlib import ExitStack
from datetime import datetime, timezone
import copy
import fcntl
import hashlib
import os
from pathlib import Path
import stat
import re
import uuid

from upgrade import Failure, directory_fd, private_json
from upgrade_backup_contract import (ContractError, MAX_ENTRIES, canonical, digest, fields, fingerprint,
                                     identifier, require, text, timestamp, validate_binding,
                                     validate_group, validate_restore_report)
from upgrade_backup import (create_sealed_backup, durable_bytes, group_object, source_fingerprint,
                            supported_metadata, verify_payload)
from upgrade_inventory import database_inventory, file_digest, safe_path, scan_tree, sqlite_path
from upgrade_restore_verify import load_sealed, load_inventory, verify_sealed_backup

MARKER = 'upgrade-cold.json'
PHASES = ('stop', 'quiescent', 'backup', 'restore_verified', 'install_started',
          'installed', 'start_attempted', 'recovery_authorized', 'old_restored')
TRANSITIONS = {'stop': {'quiescent'}, 'quiescent': {'backup', 'recovery_authorized'},
               'backup': {'restore_verified', 'recovery_authorized'},
               'restore_verified': {'install_started', 'recovery_authorized'},
               'install_started': {'installed', 'recovery_authorized'},
               'installed': {'start_attempted', 'recovery_authorized'},
               'recovery_authorized': {'old_restored'}, 'start_attempted': set(), 'old_restored': set()}
AUXILIARY = frozenset(('manifest.json', 'receipt.json', 'inventory.json', 'recovery-group.json',
                       'restore-report.json', 'publication.json', 'restore-publication.json'))


def checkpoint(name):
    """Fault-injection seam, not a configurable command or bypass."""
    return name


def replace_bytes(path, raw, mode=0o600):
    """Durable replacement; an interrupted staging file is retained, never overwritten."""
    path = Path(path)
    with directory_fd(path.parent, private=True) as parent:
        staging = '.' + path.name + '.cold-new'
        fd = os.open(staging, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                     mode, dir_fd=parent)
        with os.fdopen(fd, 'wb') as output:
            output.write(raw); output.flush(); os.fsync(output.fileno())
        checkpoint(path.name + ':file-sync')
        os.replace(staging, path.name, src_dir_fd=parent, dst_dir_fd=parent)
        checkpoint(path.name + ':rename')
        os.fsync(parent)
        checkpoint(path.name + ':directory-sync')


def replace_json(path, value):
    replace_bytes(path, canonical(value))


def read_original_binding(pending, expected=None):
    """Read original request, never derive old from the replaced installation receipt."""
    claim, _ = private_json(Path(pending) / 'claim.json')
    fields(claim, ('schema', 'binding'))
    require(claim.get('schema') == 'ags.local-claim.v2' and type(claim.get('binding')) is dict,
            'claim_invalid')
    binding = claim['binding']
    if expected is not None:
        require(binding == expected, 'claim_mismatch')
    return copy.deepcopy(binding)


def read_window(root):
    state = Path(root) / 'state'
    marker, _ = private_json(state / MARKER)
    fields(marker, ('schema', 'generation', 'binding_sha256'))
    require(marker['schema'] == 'ags.cold-window.v1', 'cold_marker_invalid')
    identifier(marker['generation']); digest(marker['binding_sha256'])
    pending = state / 'ags-autodeploy/local-v2/pending'
    binding = read_original_binding(pending)
    require(fingerprint(binding) == marker['binding_sha256'], 'cold_binding_mismatch')
    journal, _ = private_json(pending / 'cold-journal.json')
    require(type(journal) is dict, 'cold_journal_invalid')
    require(journal.get('schema') == 'ags.cold-journal.v1'
            and journal.get('generation') == marker['generation']
            and journal.get('binding_sha256') == marker['binding_sha256']
            and type(journal.get('phase')) is str and journal.get('phase') in PHASES
            and type(journal.get('start_attempted')) is bool, 'cold_journal_invalid')
    validate_binding(journal['backup_binding'])
    require(all(journal['backup_binding'][k] == v for k, v in binding.items()
                if k in journal['backup_binding']), 'cold_binding_mismatch')
    return marker, journal, pending


def startup_permitted(root):
    """Read-only inspection. Consumption must still happen under runtime EX lock."""
    state = Path(root) / 'state'
    marker_path = state / MARKER
    pending = state / 'ags-autodeploy/local-v2/pending'
    if not marker_path.exists() and not marker_path.is_symlink():
        return not pending.exists() and not pending.is_symlink()
    try:
        marker, journal, _ = read_window(root)
        handoff = journal.get('handoff', {})
        require(type(handoff) is dict, 'cold_handoff_invalid')
        return (journal['phase'] == 'start_attempted' and journal['start_attempted'] is True
                and handoff.get('generation') == marker['generation']
                and handoff.get('consumed') is False
                and handoff.get('manager_requested') is True
                and type(handoff.get('token')) is str
                and re.fullmatch(r'[0-9a-f]{32}', handoff['token']) is not None
                and handoff.get('binary_sha256') == journal['backup_binding']['new_binary_sha256']
                and handoff.get('receipt_sha256') == journal.get('installed_receipt_sha256')
                and handoff.get('binary_sha256') == file_digest(Path(root) / 'bin/gh-server')
                and handoff.get('receipt_sha256') == file_digest(state / 'installation.json'))
    except (Failure, ContractError, OSError, KeyError, TypeError, ValueError):
        return False


def consume_start(root):
    """Caller holds runtime EX and install SH. Consume before spawn, never refund."""
    state = Path(root) / 'state'
    if not (state / MARKER).exists() and not (state / MARKER).is_symlink():
        require(not (state / 'ags-autodeploy/local-v2/pending').exists(), 'cold_pending_without_marker')
        return uuid.uuid4().hex
    require(startup_permitted(root), 'cold_start_refused')
    _, journal, pending = read_window(root)
    journal['handoff']['consumed'] = True
    replace_json(pending / 'cold-journal.json', journal)
    checkpoint('start-token-consumed')
    return journal['generation']


def reconcile(root):
    """Read current facts only. Never infer that a missing PID means safe retry."""
    marker, journal, pending = read_window(root)
    runtime_path = Path(root) / 'state/runtime.json'
    runtime, _ = private_json(runtime_path)
    return {'schema': 'ags.cold-readback.v1', 'generation': marker['generation'],
            'phase': journal['phase'], 'start_attempted': journal['start_attempted'],
            'manager_requested': journal.get('handoff', {}).get('manager_requested'),
            'token_consumed': journal.get('handoff', {}).get('consumed'),
            'runtime_generation': runtime.get('generation'),
            'runtime_stopped': 'stopped_at' in runtime,
            'binary_sha256': file_digest(Path(root) / 'bin/gh-server'),
            'receipt_sha256': file_digest(Path(root) / 'state/installation.json'),
            'journal_sha256': file_digest(pending / 'cold-journal.json'),
            'action': 'read_only_no_replay'}


def stable_data(group, dsn, schema):
    """Tq data/config fingerprint, excluding the two intentionally replaced objects.

    S2's full source fingerprint still guards backup and installation intent.
    Only old_binary and installation are excluded here so interrupted replacement
    can be recovered without ignoring changes to DB/Git/CAS/config/tool/manager.
    """
    objects = validate_group(group)
    logical = database_inventory(sqlite_path(dsn),
                                 Path(group_object(group, 'git_root')['source']), schema)
    summaries = {}
    for key, item in objects.items():
        if item['role'] in ('old_binary', 'installation'):
            continue
        path = safe_path(item['source'], item['kind'])
        if item['role'] == 'sqlite':
            summaries[key] = {'logical_sha256': logical['logical_sha256'],
                              'metadata': supported_metadata(path)}
        elif path.is_dir():
            summaries[key] = {'tree': scan_tree(path, MAX_ENTRIES), 'metadata': [
                (str(p.relative_to(path)), supported_metadata(p))
                for p in [path, *sorted(path.rglob('*'))]]}
        else:
            summaries[key] = {'sha256': file_digest(path), 'metadata': supported_metadata(path)}
    return fingerprint(summaries)


class ColdWindow:
    """Private harness supplies a reviewed fixed manager and writer probe.

    manager.stop() must return a durable runtime stop record; writer_probe() must
    return exactly the generation-bound closed-writer evidence below. Unknown or
    merely successful stop commands cannot establish quiescence.
    """
    def __init__(self, root, binding, backup_binding, group, dsn, schema, manager,
                 writer_probe, trash):
        self.root = Path(root)
        self.state = self.root / 'state'
        self.pending = self.state / 'ags-autodeploy/local-v2/pending'
        self.binding = copy.deepcopy(binding)
        self.backup_binding = copy.deepcopy(backup_binding)
        self.group, self.dsn, self.schema = copy.deepcopy(group), dsn, schema
        self.manager, self.writer_probe, self.trash = manager, writer_probe, trash
        self.locks = None
        self.journal = None
        self.owner = None

    def acquire_owner(self):
        require(self.owner is None, 'hostops_already_held')
        with directory_fd(self.pending.parent, private=True) as parent:
            fd = os.open('hostops.lock', os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW | os.O_NONBLOCK,
                         0o600, dir_fd=parent)
        try:
            info = os.fstat(fd)
            require(stat.S_ISREG(info.st_mode) and info.st_nlink == 1
                    and info.st_uid == os.getuid() and not info.st_mode & 0o077, 'cold_lock_unsafe')
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BaseException:
            os.close(fd)
            raise
        self.owner = fd

    def release_owner(self):
        if self.owner is not None:
            os.close(self.owner)
            self.owner = None

    def begin(self):
        require(self.owner is not None, 'hostops_required')
        validate_binding(self.backup_binding); validate_group(self.group)
        require(all(self.backup_binding[k] == v for k, v in self.binding.items()
                    if k in self.backup_binding), 'cold_binding_mismatch')
        require(callable(self.trash), 'trash_unavailable')
        read_original_binding(self.pending, self.binding)
        require(not (self.state / MARKER).exists() and not (self.state / MARKER).is_symlink(),
                'cold_claim_held')
        before, _ = private_json(self.state / 'runtime.json')
        identifier(before.get('generation'))
        require('stopped_at' not in before and before.get('root') == str(self.root), 'stop_generation_unknown')
        generation = uuid.uuid4().hex
        self.journal = {'schema': 'ags.cold-journal.v1', 'generation': generation,
                        'binding_sha256': fingerprint(self.binding), 'backup_binding': self.backup_binding,
                        'phase': 'stop', 'start_attempted': False, 'runtime_generation': before['generation'],
                        'stop_requested_at': datetime.now(timezone.utc).isoformat()}
        # Marker first: any crash, including before journal, denies all spawning.
        durable_bytes(self.state / MARKER, canonical({'schema': 'ags.cold-window.v1',
                      'generation': generation, 'binding_sha256': fingerprint(self.binding)}))
        checkpoint('cold-marker')
        replace_json(self.pending / 'cold-journal.json', self.journal)
        checkpoint('stop-intent')

    def phase(self, name, **values):
        require(name in TRANSITIONS.get(self.journal['phase'], set()), 'cold_phase_invalid')
        if self.journal['start_attempted']:
            require(name == 'start_attempted', 'prestart_recovery_refused')
        self.journal.update(phase=name, **values)
        replace_json(self.pending / 'cold-journal.json', self.journal)
        checkpoint(name)

    def stop(self):
        require(self.journal['phase'] == 'stop' and not self.journal.get('stop_called'), 'cold_phase_invalid')
        self.journal['stop_called'] = True
        replace_json(self.pending / 'cold-journal.json', self.journal)
        self.manager.suppress_restart()
        record = self.manager.stop()
        require(type(record) is dict and record.get('generation') == self.journal['runtime_generation']
                and record.get('root') == str(self.root)
                and record.get('exit_code') == 0 and type(record.get('exit_code')) is int
                and record.get('forced_kill') is False and record.get('stop_requested') is True
                and record.get('supervisor_exit_code') == 0 and type(record.get('supervisor_exit_code')) is int
                and record.get('supervisor_exited') is True and record.get('process_group_empty') is True
                and timestamp(record['stopped_at']) >= timestamp(self.journal['stop_requested_at'])
                and timestamp(record['stopped_at']) >= timestamp(record['started_at']), 'normal_stop_unproven')
        persisted, _ = private_json(self.state / 'runtime.json')
        require(all(persisted.get(k) == record[k] for k in
                    ('generation', 'exit_code', 'forced_kill', 'stop_requested', 'started_at', 'stopped_at')),
                'normal_stop_unproven')
        self.journal['stop_evidence_sha256'] = fingerprint(record)
        replace_json(self.pending / 'cold-journal.json', self.journal)

    def acquire(self):
        require(self.owner is not None, 'hostops_required')
        require(self.locks is None, 'cold_lock_already_held')
        resources = ExitStack()
        try:
            with directory_fd(self.state, private=True) as parent:
                # All locks are nonblocking. Runtime takes runtime EX then install SH;
                # installers take install EX then runtime EX, so no cycle can wait.
                for name in ('install.lock', 'runtime.lock'):
                    fd = os.open(name, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW | os.O_NONBLOCK,
                                 0o600, dir_fd=parent)
                    resources.callback(os.close, fd)
                    info = os.fstat(fd)
                    require(stat.S_ISREG(info.st_mode) and info.st_nlink == 1
                            and info.st_uid == os.getuid() and not info.st_mode & 0o077, 'cold_lock_unsafe')
                    try:
                        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    except BlockingIOError:
                        raise Failure('lock_held', 'quiescent') from None
            self.locks = resources
        except BaseException:
            resources.close()
            raise

    def release(self):
        if self.locks is not None:
            self.locks.close()
            self.locks = None

    def writers(self):
        require(self.locks is not None and self.owner is not None, 'cold_locks_required')
        evidence = self.writer_probe()
        require(evidence == {'generation': self.journal['generation'], 'closed': True,
                             'writers': [], 'unknown': []}, 'writers_not_quiescent')
        return fingerprint(evidence)

    def quiescent(self):
        require(self.journal['phase'] == 'stop' and 'stop_evidence_sha256' in self.journal,
                'normal_stop_unproven')
        self.acquire()
        evidence = self.writers()
        before = source_fingerprint(self.group, self.dsn, self.schema)
        data = stable_data(self.group, self.dsn, self.schema)
        require(before == source_fingerprint(self.group, self.dsn, self.schema), 'source_changed')
        self.phase('quiescent', tq_sha256=before, data_sha256=data,
                   quiescence={'nonce': uuid.uuid4().hex,
                               'stop_evidence_sha256': self.journal['stop_evidence_sha256'],
                               'writers_evidence_sha256': evidence})

    def unchanged(self, *, full=True):
        self.writers()
        require(stable_data(self.group, self.dsn, self.schema) == self.journal['data_sha256'], 'source_changed')
        if full:
            require(source_fingerprint(self.group, self.dsn, self.schema) == self.journal['tq_sha256'],
                    'source_changed')

    def backup(self):
        require(self.journal['phase'] == 'quiescent', 'cold_phase_invalid')
        self.unchanged()
        sealed = create_sealed_backup(self.group, self.dsn, self.schema, self.backup_binding,
                                      self.journal['quiescence'], self.journal['generation'])
        self.unchanged()
        self.phase('backup', backup_dir=sealed['backup_dir'])

    def restore(self):
        require(self.journal['phase'] == 'backup', 'cold_phase_invalid')
        self.unchanged()
        verify_sealed_backup(self.journal['backup_dir'], group=self.group, dsn=self.dsn,
                             schema=self.schema, trash=self.trash)
        self.verified_backup()
        self.unchanged()
        self.phase('restore_verified')

    def verified_backup(self):
        path, manifest, receipt, _ = load_sealed(self.journal['backup_dir'], self.group)
        require(manifest['binding'] == self.backup_binding
                and manifest['quiescence'] == self.journal['quiescence']
                and manifest['source_before_sha256'] == self.journal['tq_sha256'], 'backup_binding_mismatch')
        load_inventory(path, manifest, self.group, self.schema, self.dsn)
        stored_group, _ = private_json(path / 'recovery-group.json')
        require(stored_group == self.group, 'group_mismatch')
        verify_payload(path, manifest['entries'], auxiliary=AUXILIARY)
        report, _ = private_json(path / 'restore-report.json')
        validate_restore_report(canonical(report), receipt)
        require(timestamp(report['verified_at']) >= timestamp(manifest['ended_at']), 'time_order_invalid')
        publication, _ = private_json(path / 'restore-publication.json')
        require(publication == {'state': 'published', 'manifest_sha256': receipt['manifest_sha256']},
                'restore_outcome_unknown')
        return path, manifest

    def install_intent(self, authorize):
        require(self.journal['phase'] == 'restore_verified', 'cold_phase_invalid')
        self.unchanged(); self.verified_backup(); authorize()
        old_binary = Path(group_object(self.group, 'old_binary')['source'])
        old_receipt = Path(group_object(self.group, 'installation')['source'])
        require(old_binary == self.root / 'bin/gh-server'
                and old_receipt == self.state / 'installation.json', 'installation_mapping_mismatch')
        require(file_digest(old_binary) == self.backup_binding['old_binary_sha256'], 'old_binary_mismatch')
        old, _ = private_json(old_receipt)
        require(old['source'] == self.binding['old']
                and old['binaries']['gh-server']['sha256'] == file_digest(old_binary), 'old_receipt_mismatch')
        # These are independent, immutable raw copies. No reconstruction from a later receipt.
        durable_bytes(self.pending / 'old-binary', old_binary.read_bytes())
        durable_bytes(self.pending / 'old-installation.json', old_receipt.read_bytes())
        self.phase('install_started', old_receipt_sha256=file_digest(old_receipt),
                   old_mode=stat.S_IMODE(old_binary.stat().st_mode))

    def installed(self, expected_identity, identity_reader):
        require(self.journal['phase'] == 'install_started', 'cold_phase_invalid')
        self.unchanged(full=False)
        binary = self.root / 'bin/gh-server'
        receipt, _ = private_json(self.state / 'installation.json')
        require(not (self.state / 'installing.json').exists()
                and not (self.state / 'installing.json').is_symlink()
                and file_digest(binary) == self.backup_binding['new_binary_sha256']
                and receipt['source'] == self.binding['new']
                and receipt.get('runtime_tree') == self.backup_binding['runtime_tree']
                and receipt.get('control_revision') == self.backup_binding['control_revision']
                and receipt.get('release_id') == self.backup_binding['release_id']
                and receipt.get('version') == self.backup_binding['release_tag']
                and expected_identity.get('revision') == self.binding['new']
                and receipt['binaries']['gh-server']['sha256'] == file_digest(binary)
                and receipt['binaries']['gh-server']['identity'] == expected_identity
                and identity_reader(binary) == expected_identity, 'installed_identity_mismatch')
        self.phase('installed', installed_receipt_sha256=file_digest(self.state / 'installation.json'))

    def start_intent(self):
        require(self.journal['phase'] == 'installed' and not self.journal['start_attempted'], 'cold_phase_invalid')
        self.unchanged(full=False); self.verified_backup()
        require(file_digest(self.root / 'bin/gh-server') == self.backup_binding['new_binary_sha256']
                and file_digest(self.state / 'installation.json') == self.journal['installed_receipt_sha256'],
                'installed_identity_mismatch')
        self.phase('start_attempted', start_attempted=True,
                   handoff={'token': uuid.uuid4().hex, 'generation': self.journal['generation'],
                            'consumed': False, 'binary_sha256': self.backup_binding['new_binary_sha256'],
                            'receipt_sha256': self.journal['installed_receipt_sha256'],
                            'manager_requested': False})

    def start(self):
        require(self.owner is not None and self.locks is None
                and self.journal['phase'] == 'start_attempted', 'cold_start_refused')
        # No dynamic argv/token environment: fixed manager invokes the fixed launcher;
        # the launcher reads and consumes the durable handoff under runtime EX.
        _, current, _ = read_window(self.root)
        require(current['phase'] == 'start_attempted' and current['start_attempted'] is True
                and current['generation'] == self.journal['generation']
                and current.get('handoff', {}).get('consumed') is False
                and current.get('handoff', {}).get('manager_requested') is False, 'start_already_requested')
        current['handoff']['manager_requested'] = True
        replace_json(self.pending / 'cold-journal.json', current)
        checkpoint('manager-start-intent')
        self.journal = current
        self.manager.start()


def recover_prestart(window, approval_path, *, now=None):
    """Separate fresh approval, original binding and data=Tq; no startup or data restore."""
    require(window.locks is not None and window.owner is not None, 'cold_locks_required')
    marker, journal, pending = read_window(window.root)
    require(not journal['start_attempted'] and journal['phase'] in
            ('quiescent', 'backup', 'restore_verified', 'install_started', 'installed'),
            'prestart_recovery_refused')
    require(window.binding == read_original_binding(pending)
            and window.backup_binding == journal['backup_binding'], 'cold_binding_mismatch')
    approval, approval_sha = private_json(Path(approval_path))
    fields(approval, ('schema', 'approval_id', 'purpose', 'decision', 'binding', 'generation',
                      'journal_sha256', 'issued_at', 'expires_at', 'actor'))
    require(approval['schema'] == 'ags.prestart-recovery-approval.v1'
            and approval['purpose'] == 'prestart-recover' and approval['decision'] == 'approve'
            and approval['binding'] == window.binding and approval['generation'] == marker['generation'],
            'recovery_approval_invalid')
    identifier(approval['approval_id'])
    require(approval['approval_id'] != window.binding['authorization_id'], 'recovery_approval_not_fresh')
    fields(approval['actor'], ('type', 'id', 'source'))
    require(approval['actor']['type'] == 'human' and all(type(v) is str and v for v in approval['actor'].values()),
            'recovery_approval_invalid')
    for value in approval['actor'].values():
        text(value)
    digest(approval['journal_sha256'])
    now = now or datetime.now(timezone.utc)
    require(timestamp(approval['issued_at']) <= now < timestamp(approval['expires_at']), 'recovery_approval_expired')
    require(approval['journal_sha256'] == file_digest(pending / 'cold-journal.json'), 'recovery_approval_stale')
    window.journal = journal
    window.unchanged(full=False)
    if journal['phase'] in ('quiescent', 'backup', 'restore_verified'):
        # No installation intent exists: require the complete original source,
        # including program/receipt, to remain equal to Tq. No replacement needed.
        window.unchanged()
        binary = safe_path(window.root / 'bin/gh-server', 'file')
        receipt, _ = private_json(window.state / 'installation.json')
        require(file_digest(binary) == window.backup_binding['old_binary_sha256']
                and receipt['source'] == window.binding['old']
                and receipt['binaries']['gh-server']['sha256'] == file_digest(binary), 'old_evidence_mismatch')
        durable_bytes(pending / ('recovery-' + approval['approval_id'] + '.json'), canonical(approval))
        window.phase('recovery_authorized', recovery_approval_sha256=approval_sha)
        window.phase('old_restored')
        return {'status': 'old_restored', 'generation': marker['generation'], 'started': False}
    backup, _ = window.verified_backup()
    old_bytes = safe_path(pending / 'old-binary', 'file').read_bytes()
    old_receipt = safe_path(pending / 'old-installation.json', 'file').read_bytes()
    require(hashlib.sha256(old_bytes).hexdigest() == window.backup_binding['old_binary_sha256']
            and hashlib.sha256(old_receipt).hexdigest() == journal['old_receipt_sha256'], 'old_evidence_mismatch')
    require(old_bytes == (backup / group_object(window.group, 'old_binary')['id']).read_bytes()
            and old_receipt == (backup / group_object(window.group, 'installation')['id']).read_bytes(),
            'old_evidence_mismatch')
    # Persist consumption before the first replacement. Interrupted recovery is
    # readback-only; no automatic replay, even with the same approval.
    durable_bytes(pending / ('recovery-' + approval['approval_id'] + '.json'), canonical(approval))
    window.phase('recovery_authorized', recovery_approval_sha256=approval_sha)
    replace_bytes(window.root / 'bin/gh-server', old_bytes, journal['old_mode'])
    checkpoint('recovery-binary')
    replace_bytes(window.state / 'installation.json', old_receipt)
    checkpoint('recovery-receipt')
    window.unchanged(full=False)
    require(file_digest(window.root / 'bin/gh-server') == window.backup_binding['old_binary_sha256']
            and file_digest(window.state / 'installation.json') == journal['old_receipt_sha256'],
            'old_evidence_mismatch')
    window.phase('old_restored')
    # Marker deliberately retained. Restarting old needs a later, separately
    # admitted one-shot protocol; S3 does not expose any recovery CLI.
    return {'status': 'old_restored', 'generation': marker['generation'], 'started': False}
