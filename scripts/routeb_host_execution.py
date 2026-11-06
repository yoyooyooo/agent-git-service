#!/usr/bin/env python3
"""Execute an admitted native host package through the durable cold window.

The fixed host package owns every path and manager capability. A completed
observation is evidence, not M6 acceptance or permission for production use.
An existing claim is never resumed by run; status is the read-only entry.
"""
from contextlib import ExitStack
import copy
from datetime import datetime, timezone
import hashlib
import os
from pathlib import Path
import re
import stat
import subprocess
import time

from upgrade import Failure, directory_fd, private_json
from upgrade_backup import durable_bytes, preflight_space
from upgrade_backup_contract import ContractError, canonical, fingerprint, require
from upgrade_inventory import file_digest
from upgrade_recovery import ColdWindow, MARKER, read_window, replace_json
import routeb_native_capture as capture
from routeb_host_native import FixedManager, SystemTrash, WriterProbe
from routeb_host_package import require_interpreter_closure
import upgrade_state


JOURNAL = 'host-execution.json'
JOURNAL_SCHEMA = 'ags.routeb-host-journal.v1'
EXECUTION_SCHEMA = 'ags.routeb-host-execution.v1'


def utc():
    return datetime.now(timezone.utc).isoformat()


def failure_code(error):
    code = getattr(error, 'code', None)
    if code is None and isinstance(error, ContractError):
        code = str(error)
    return code if isinstance(code, str) and re.fullmatch('[a-z][a-z0-9_]{0,95}', code) else 'host_execution_failed'


def lifecycle_blockers(package):
    loaded = upgrade_state.load_verified(package.root)
    projection = loaded.get('projection') or {}
    authority = projection.get('authority')
    blockers = []
    if loaded.get('format', {}).get('host_instance') != package.package['host_instance']:
        blockers.append('lifecycle_host_mismatch')
    if not isinstance(authority, dict) or authority.get('state') != 'active':
        blockers.append('active_lifecycle_authority_required')
    if projection.get('pending_effects'):
        blockers.append('lifecycle_effect_unknown')
    # ColdWindow v1 writes epoch 1. Refuse a later epoch before stopping a host.
    if projection.get('epoch') != 1 or (authority or {}).get('epoch') != 1:
        blockers.append('cold_epoch_unsupported')
    return blockers


def read_version(binary):
    """Run only the reviewed executable's version command, with no service env."""
    try:
        result = subprocess.run([str(binary), '--version'], capture_output=True,
                                timeout=15, env={'PATH': '/usr/bin:/bin:/usr/sbin:/sbin', 'LC_ALL': 'C'})
    except (OSError, subprocess.SubprocessError):
        raise ContractError('program_identity_unknown') from None
    require(result.returncode == 0 and not result.stderr and len(result.stdout) <= 65536,
            'program_identity_unknown')
    return capture.object_json(result.stdout)


def log_baselines(spec):
    rows = []
    for row in spec['logs']:
        path = Path(row['path'])
        with directory_fd(path.parent) as parent:
            fd = os.open(path.name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=parent)
            try:
                info = os.fstat(fd)
                require(stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid()
                        and not info.st_mode & 0o022, 'log_unsafe')
                rows.append({**row, 'device': info.st_dev, 'inode': info.st_ino, 'offset': info.st_size})
            finally:
                os.close(fd)
    return rows


def old_installation_unchanged(package):
    current, receipt_sha = private_json(package.root / 'state/installation.json')
    require(current == package.old_receipt and receipt_sha == package.old_receipt_sha256,
            'old_installation_changed')
    require(file_digest(package.root / 'bin/gh-server') ==
            package.old_receipt['binaries']['gh-server']['sha256'], 'old_binary_changed')
    return current


def preflight(package):
    """Inspect native capabilities and prerequisites without creating a claim."""
    package.authorize()
    blockers = list(package.execution_blockers())
    if package.pending.exists() or package.pending.is_symlink():
        blockers.append('transaction_already_claimed')
    for name in (MARKER, 'installing.json'):
        path = package.root / 'state' / name
        if path.exists() or path.is_symlink():
            blockers.append('installation_recovery_required')
    blockers.extend(lifecycle_blockers(package))
    manager = FixedManager(package.root, package.profile, package.host)
    observed = None
    try:
        old_installation_unchanged(package)
        observed = manager.preflight()
        WriterProbe(manager, package.group, lambda: 'preflight')
        SystemTrash(package.group['storage']['restore_root'], package.profile['trash_root'])
        preflight_space(package.group, Path(package.group['storage']['backup_root']),
                        Path(package.group['storage']['restore_root']))
        log_baselines(package.profile['capture'])
    except (ContractError, Failure, OSError) as error:
        blockers.append(failure_code(error))
    return {'schema': 'ags.routeb-host-plan.v1', 'host': package.package['host'],
            'host_instance': package.package['host_instance'], 'scope': package.host.scope,
            'transaction_id': package.package['transaction_id'],
            'package_sha256': hashlib.sha256(package.package_raw).hexdigest(),
            'candidate_revision': package.candidate.revision, 'candidate_tree': package.candidate.tree,
            'tool_sha256': package.package['tool_sha256'], 'effects': [],
            'execution_allowed': not blockers, 'installable': False,
            'blockers': sorted(set(blockers)),
            'running_generation': None if observed is None else observed['runtime']['generation']}


class HostExecution:
    def __init__(self, package):
        self.package = package
        self.root = package.root
        self.pending = package.pending
        self.manager = FixedManager(self.root, package.profile, package.host)
        self.trash = SystemTrash(package.group['storage']['restore_root'], package.profile['trash_root'])
        self.binding = package.cold_binding()
        self.window = ColdWindow(self.root, self.binding, self.binding, package.group,
                                 package.dsn, package.profile['schema_sha256'], self.manager,
                                 lambda: None, self.trash)
        self.window.writer_probe = WriterProbe(self.manager, package.group,
                                               lambda: self.window.journal['generation'])
        self.journal = None

    def authorize_stop(self):
        self.package.authorize()
        old_installation_unchanged(self.package)

    def stage(self, phase, **values):
        self.journal.update(phase=phase, updated_at=utc(), **values)
        replace_json(self.pending / JOURNAL, self.journal)

    def claim(self):
        require(not self.pending.exists() and not self.pending.is_symlink(), 'transaction_already_claimed')
        with directory_fd(self.pending.parent, private=True) as parent:
            os.mkdir(self.pending.name, 0o700, dir_fd=parent)
            os.fsync(parent)
        durable_bytes(self.pending / 'claim.json', canonical({'schema': 'ags.local-claim.v2',
                                                             'binding': self.binding}))
        self.journal = {'schema': JOURNAL_SCHEMA, 'transaction_id': self.package.package['transaction_id'],
                        'host_instance': self.package.package['host_instance'], 'root': str(self.root),
                        'package_sha256': hashlib.sha256(self.package.package_raw).hexdigest(),
                        'approval_sha256': self.package.approval_sha256,
                        'binding_sha256': fingerprint(self.binding), 'started_at': utc(),
                        'phase': 'admitted', 'stop_attempted': False, 'start_attempted': False,
                        'status': 'running', 'accepted': False, 'installable': False}
        durable_bytes(self.pending / JOURNAL, canonical(self.journal))

    def install(self):
        """Replace only the binary and installation receipt under both cold locks."""
        window, package = self.window, self.package
        require(window.locks is not None and window.journal['phase'] == 'install_started',
                'cold_locks_required')
        package.authorize()
        window.unchanged()
        window.verified_backup()
        expected = package.candidate.value['version']
        with directory_fd(self.root, private=True) as root_fd, ExitStack() as guards:
            dirs = {}
            for name in ('state', 'bin'):
                fd = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=root_fd)
                guards.callback(os.close, fd)
                info = os.fstat(fd)
                require(info.st_uid == os.getuid() and not info.st_mode & 0o077, 'install_directory_unsafe')
                dirs[name] = fd
            state_fd, bin_fd = dirs['state'], dirs['bin']
            current = old_installation_unchanged(package)
            fd = os.open(package.candidate.binary_path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
            with os.fdopen(fd, 'rb') as source:
                before = os.fstat(source.fileno())
                require(stat.S_ISREG(before.st_mode) and before.st_uid == os.getuid()
                        and before.st_nlink == 1 and 0 < before.st_size <= 1024 ** 3, 'candidate_unsafe')
                data = source.read(before.st_size + 1)
                after = os.fstat(source.fileno())
                require((before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns, before.st_ctime_ns) ==
                        (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns, after.st_ctime_ns)
                        and len(data) == before.st_size and capture.sha(data) == package.candidate.sha256,
                        'candidate_replaced')
            installed = copy.deepcopy(current)
            for key in ('release_id', 'release_tag', 'control_revision', 'attestation'):
                installed.pop(key, None)
            installed.update(artifact_kind='exact-source', source=package.candidate.revision,
                             runtime_tree=package.candidate.tree, version=expected['version'],
                             tool_sha256=package.package['tool_sha256'],
                             candidate_manifest_sha256=package.candidate.raw_manifest_sha256)
            installed['binaries']['gh-server'] = {'sha256': package.candidate.sha256, 'identity': expected}

            def write_json(name, value):
                stage = name + '.routeb-new'
                fd = os.open(stage, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                             0o600, dir_fd=state_fd)
                with os.fdopen(fd, 'wb') as stream:
                    stream.write(canonical(value)); stream.flush(); os.fsync(stream.fileno())
                os.replace(stage, name, src_dir_fd=state_fd, dst_dir_fd=state_fd)
                os.fsync(state_fd)

            fd = os.open('.gh-server.routeb-new', os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                         0o700, dir_fd=bin_fd)
            with os.fdopen(fd, 'wb') as stream:
                stream.write(data); stream.flush(); os.fsync(stream.fileno())
            write_json('installing.json', {'schema': 'ags.routeb-install-intent.v1',
                                         'transaction_id': package.package['transaction_id'],
                                         'binary_sha256': package.candidate.sha256,
                                         'installation_sha256': fingerprint(installed)})
            os.replace('.gh-server.routeb-new', 'gh-server', src_dir_fd=bin_fd, dst_dir_fd=bin_fd)
            os.fsync(bin_fd)
            write_json('installation.json', installed)
            os.unlink('installing.json', dir_fd=state_fd)
            os.fsync(state_fd)
        window.installed(expected, read_version)

    def ready_snapshot(self):
        spec = self.package.profile['capture']
        runtime_raw = capture.read_regular(spec['runtime_path'])
        runtime = capture.object_json(runtime_raw)
        require(runtime.get('schema') == 'ags.runtime.v1' and runtime.get('root') == str(self.root)
                and runtime.get('generation') == self.window.journal['generation']
                and 'stopped_at' not in runtime and runtime.get('build') == self.package.candidate.value['version'],
                'started_runtime_unverified')
        manager, manager_raw, manager_kind = capture.read_manager(spec['manager'])
        artifacts = {}
        tree = capture.runtime_processes(self.root, runtime, manager, spec['binary_path'],
                    stage0=capture.stage0_registration(self.package.profile, self.root), artifacts=artifacts)
        boot_id, boot_raw = capture.boot_identity()
        require(boot_id == self.manager.before['boot_id'], 'boot_changed')
        if 'stage0' in tree:
            require(tree['stage0']['boot_id'] == boot_id, 'stage0_boot_changed')
        installation_raw = capture.read_regular(spec['installation_path'])
        require(capture.sha(installation_raw) == self.window.journal['installed_receipt_sha256']
                and capture.hash_file(spec['binary_path']) == self.package.candidate.sha256,
                'installed_identity_mismatch')
        loaded = upgrade_state.load_verified(self.root)
        projection = loaded.get('projection') or {}
        require(loaded.get('format', {}).get('host_instance') == self.package.package['host_instance']
                and projection.get('generation') == runtime['generation']
                and not projection.get('pending_effects') and not lifecycle_blockers(self.package),
                'started_journal_unverified')
        _, cold, _ = read_window(self.root)
        require(cold['phase'] == 'start_attempted' and cold['start_attempted'] is True
                and cold['generation'] == runtime['generation']
                and cold.get('handoff', {}).get('consumed') is True
                and cold.get('handoff', {}).get('manager_requested') is True, 'start_once_unproven')
        artifacts.update({'runtime.json': (runtime_raw, 'raw_runtime'),
                     'installation.json': (installation_raw, 'raw_installation'),
                     'manager.txt': (manager_raw, manager_kind), 'boot.txt': (boot_raw, 'raw_os_boot_identity'),
                     'journal.json': (capture.canonical(loaded), 'verified_journal_projection')})
        for name, url in spec['health'].items():
            facts, raw = capture.health(url)
            require(facts['http_status'] == 200 and facts['status'] == 'ready'
                    and facts['revision'] == self.package.candidate.revision, 'health_not_ready')
            artifacts['health-' + name + '.json'] = (raw, 'raw_readyz')
        require(capture.read_regular(spec['runtime_path']) == runtime_raw, 'runtime_changed_during_probe')
        self.manager.definition()
        return {'runtime': runtime, 'boot_id': boot_id, 'manager': manager,
                'child': tree['child'], 'supervisor': tree['supervisor'], 'projection': projection,
                'runtime_sha256': capture.sha(runtime_raw),
                'installation_sha256': capture.sha(installation_raw), 'artifacts': artifacts,
                **({key: tree[key] for key in ('stage0', 'stage0_sha256', 'stage0_tree_sha256')}
                   if 'stage0' in tree else {})}

    def observe_start(self):
        deadline = time.monotonic() + 90
        while True:
            try:
                return self.ready_snapshot()
            except (ContractError, capture.CaptureError, Failure, OSError, KeyError):
                if time.monotonic() >= deadline:
                    raise ContractError('start_observation_unknown') from None
                time.sleep(.2)

    def publish_execution(self, snapshot, startup_reader):
        package, window = self.package, self.window
        backup_dir = Path(window.journal['backup_dir'])
        sealed, _ = private_json(backup_dir / 'receipt.json')
        record = {key: package.package[key] for key in
                  ('host', 'host_instance', 'deployment_id', 'transaction_id', 'attempt_id')}
        record.update(schema=EXECUTION_SCHEMA, status='awaiting_observation', artifact_kind='exact-source',
                      root=str(self.root), generation=window.journal['generation'], boot_id=snapshot['boot_id'],
                      candidate={'revision': package.candidate.revision, 'tree': package.candidate.tree,
                                 'binary_sha256': package.candidate.sha256,
                                 'manifest_sha256': package.candidate.raw_manifest_sha256,
                                 'version_identity_sha256': capture.sha(capture.canonical(package.candidate.value['version']))},
                      tool={'revision': package.profile['tool_revision'], 'tree': package.profile['tool_tree'],
                            'bundle_sha256': package.package['tool_sha256']},
                      package_sha256=capture.sha(package.package_raw), approval_sha256=package.approval_sha256,
                      profile_sha256=package.package['native_profile_sha256'],
                      recovery_group_sha256=package.package['recovery_group_sha256'],
                      backup={'snapshot_id': sealed['snapshot_id'], 'manifest_sha256': sealed['manifest_sha256'],
                              'inventory_sha256': file_digest(backup_dir / 'inventory.json'),
                              'restore_report_sha256': file_digest(backup_dir / 'restore-report.json')},
                      journal={'path': str(self.root / 'state/ags-autodeploy/local-v2/control'),
                               'event_head_sha256': snapshot['projection']['event_sha256'],
                               'sequence': snapshot['projection']['sequence']},
                      start_once={'attempted': True, 'token_consumed': True, 'manager_requested': True},
                      accepted=False, installable=False)
        archive = capture.Archive(self.pending / 'startup-evidence')
        for name, (raw, kind) in snapshot['artifacts'].items():
            archive.write(name, raw, kind)
        artifacts, ranges = startup_reader.log_delta()
        for name, (raw, kind) in artifacts.items():
            archive.write(name, raw, kind)
        startup_manifest = archive.write('manifest.json', capture.canonical({
                                                        'schema': 'ags.routeb-startup-evidence.v1',
                                                        'artifacts': list(archive.artifacts),
                                                        'startup_log_ranges': ranges,
                                                        'trash_moves': self.trash.receipts}), 'startup_manifest')
        record['startup_evidence'] = {'path': 'startup-evidence/manifest.json',
                                      'sha256': startup_manifest['sha256']}
        # Freeze the log boundary and clock only after startup artifacts are durable.
        # A final runtime/manager read prevents validating a stale generation.
        fresh = self.ready_snapshot()
        require(fresh['runtime_sha256'] == snapshot['runtime_sha256']
                and fresh['manager'] == snapshot['manager']
                and fresh['projection'] == snapshot['projection']
                and fresh['child'] == snapshot['child'] and fresh['supervisor'] == snapshot['supervisor']
                and fresh.get('stage0') == snapshot.get('stage0'), 'start_changed_before_capture')
        record['validated_start'] = {'at_utc': utc(), 'monotonic_ns': time.monotonic_ns(),
                                     'runtime_sha256': fresh['runtime_sha256'],
                                     'manager_sha256': capture.sha(capture.canonical(fresh['manager'])),
                                     'binary_sha256': package.candidate.sha256,
                                     'installation_sha256': fresh['installation_sha256'],
                                     'log_baseline': [
                                         {'name': row['name'],
                                          'path': next(item['path'] for item in package.profile['capture']['logs']
                                                       if item['name'] == row['name']),
                                          'device': row['device'], 'inode': row['inode'], 'offset': row['end']}
                                         for row in ranges]}
        if 'stage0_sha256' in fresh:
            record['validated_start']['stage0_sha256'] = fresh['stage0_sha256']
            record['validated_start']['stage0_tree_sha256'] = fresh['stage0_tree_sha256']
        require(capture.sha(capture.read_regular(package.profile['capture']['runtime_path'])) ==
                record['validated_start']['runtime_sha256'], 'runtime_changed_during_probe')
        target = self.pending / 'execution.json'
        durable_bytes(target, capture.canonical(record))
        return target

    def run(self):
        require_interpreter_closure(self.root, operations=('transaction',))
        package, window = self.package, self.window
        require(not preflight(package)['blockers'], 'execution_preflight_blocked')
        require(read_version(package.candidate.binary_path) == package.candidate.value['version']
                and read_version(self.root / 'bin/gh-server') == package.old_receipt['binaries']['gh-server']['identity'],
                'program_identity_mismatch')
        startup_reader = None
        window.acquire_owner()
        try:
            package.authorize()
            require(not lifecycle_blockers(package), 'lifecycle_changed')
            self.manager.preflight()
            self.authorize_stop()
            self.claim()
            window.begin()
            self.stage('stop', stop_attempted=True)
            window.stop(authorize=self.authorize_stop)
            window.quiescent()
            self.stage('quiescent')
            package.authorize()
            window.backup()
            self.stage('backup', backup_dir=window.journal['backup_dir'])
            package.authorize()
            window.restore()
            self.stage('restore_verified')
            window.install_intent(package.authorize)
            self.stage('install_started')
            self.install()
            self.stage('installed')
            package.authorize()
            window.start_intent()
            self.stage('start_attempted', start_attempted=True)
            # NativeProbe's log reader is also used for the startup range. It
            # performs no process/health sample until a complete receipt exists.
            startup_reader = capture.NativeProbe(
                {'root': str(self.root),
                 'validated_start': {'log_baseline': log_baselines(package.profile['capture'])}},
                package.profile['capture'])
            window.release()
            window.start(authorize=package.authorize)
            snapshot = self.observe_start()
            execution_path = self.publish_execution(snapshot, startup_reader)
            startup_reader.close()
            startup_reader = None
            self.stage('awaiting_observation', execution_receipt=str(execution_path))
            observation = capture.capture(execution_path, package.path.parent / 'native-profile.json',
                                          self.pending / 'observation')
            require(observation['status'] == 'observed' and observation['accepted'] is False,
                    'native_observation_interrupted')
            require(observation['kind'] == 'native' and observation['installable'] is False
                    and observation['mode'] == package.profile['capture']['mode'],
                    'native_observation_unverified')
            self.stage('observed', status='observed', observation_receipt=str(self.pending / 'observation/capture.json'))
            return dict(self.journal)
        except BaseException as error:
            if self.journal is not None:
                self.stage(self.journal['phase'], status='failed', code=failure_code(error),
                           recovery_required=self.journal['stop_attempted'],
                           service_state='stopped_or_unknown' if self.journal['stop_attempted'] else 'unchanged')
            raise
        finally:
            if startup_reader is not None:
                startup_reader.close()
            window.release()
            window.release_owner()


def status(journal_path, transaction_id):
    path = Path(journal_path)
    require(path.is_absolute()
            and path.parts[-5:] == ('state', 'ags-autodeploy', 'local-v2', 'pending', JOURNAL),
            'status_journal_path_invalid')
    # Exact suffix, rather than a caller-selected arbitrary JSON file.
    root = path.parents[4]
    require(path == root / 'state/ags-autodeploy/local-v2/pending' / JOURNAL, 'status_journal_path_invalid')
    from routeb_host_identity import resolve_for_root
    host = resolve_for_root(root)
    value, sha = private_json(path)
    require(value.get('schema') == JOURNAL_SCHEMA and value.get('transaction_id') == transaction_id
            and value.get('host_instance') == host.identity.host_instance
            and value.get('root') == str(root), 'status_binding_mismatch')
    loaded = upgrade_state.load_verified(root)
    return {'schema': 'ags.routeb-host-status.v1', 'transaction_id': transaction_id,
            'phase': value['phase'], 'status': value['status'], 'code': value.get('code'),
            'stop_attempted': value['stop_attempted'], 'start_attempted': value['start_attempted'],
            'journal_sha256': sha, 'journal_projection': loaded.get('projection'),
            'accepted': False, 'installable': False, 'effects': []}
