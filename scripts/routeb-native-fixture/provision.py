"""Provision one disposable VM transaction from accepted source and real inputs.

Only initial provisioning files, approvals, and the initial serve ticket are
written here. Operational journals, claims, manager anchors, stopped proofs and
completion records belong exclusively to the native production programs.
"""
from datetime import datetime, timezone
import importlib
import json
import os
from pathlib import Path
import platform
import pwd
import secrets
import shutil
import socket
import stat
import subprocess
import sys

import authority
import authorization
from common import (ID, INTERPRETERS, REGISTRY, Refused, absent, absolute, command,
                    exclusive, exclusive_json, fingerprint, object_json,
                    protected_directory, read_bytes, read_json, reference,
                    require, sha, utc)
from source_evidence import validate as validate_source


def mkdir(path, uid=0, gid=0, mode=0o700):
    path.mkdir(mode=mode)
    os.chown(path, uid, gid)
    os.chmod(path, mode)
    return path


def parent_directories(path, uid=0, gid=0, mode=0o755):
    if not path.exists():
        parent_directories(path.parent, uid, gid, mode)
        mkdir(path, uid, gid, mode)
    require(path.resolve() == path and path.is_dir(), 'directory_alias')


def as_user(account, argv, *, timeout=120, cwd=None):
    return command(['/usr/sbin/runuser', '-u', account.pw_name, '--', '/usr/bin/env', '-i',
                    'HOME=' + account.pw_dir, 'USER=' + account.pw_name,
                    'PATH=/usr/local/bin:/usr/bin:/bin', 'LANG=C.UTF-8',
                    *map(str, argv)], timeout=timeout, cwd=cwd)


def unit(registration_id, operation, stage0):
    name = ('ags-hostpkg-test-' + registration_id if operation == 'serve' else
            'ags-routeb-control-' + registration_id + '-' + operation) + '.service'
    argv = str(stage0) + ' serve --registration ' + registration_id + ' --operation ' + operation
    complete = str(stage0) + ' manager-complete --registration ' + registration_id + ' --operation ' + operation
    content = ('[Unit]\nDescription=Disposable Route B native fixture\n[Service]\n'
               'Type=simple\nUser=root\nGroup=root\nExecStart=' + argv + '\nExecStopPost=' + complete +
               '\nRestart=no\nKillMode=mixed\nSendSIGKILL=no\nTimeoutStopSec=120\n'
               'NoNewPrivileges=yes\nStandardOutput=journal\nStandardError=journal\nUMask=0077\n')
    return name, content.encode('ascii')


def preflight_vm(uid, source):
    require(os.getuid() == os.geteuid() == 0 and platform.system() == 'Linux'
            and platform.machine() == 'x86_64' and uid > 0, 'root_linux_vm_required')
    require(Path('/proc/1/comm').read_text().strip() == 'systemd'
            and Path('/sys/fs/cgroup/cgroup.controllers').is_file(), 'pid1_systemd_cgroup2_required')
    require(socket.gethostname() not in ('imile-win', 'mini', 'pro'), 'shared_host_forbidden')
    os_release = Path('/etc/os-release').read_text()
    require('ID=ubuntu\n' in os_release and 'VERSION_ID="24.04"' in os_release, 'ubuntu_2404_required')
    account = pwd.getpwuid(uid)
    require(ID.fullmatch(account.pw_name) and account.pw_gid > 0 and absolute(account.pw_dir) != Path('/'),
            'runner_account')
    require(source.is_relative_to(Path(account.pw_dir)) and source.stat().st_uid == uid,
            'runner_checkout_required')
    require(Path('/proc/sys/kernel/yama/ptrace_scope').read_text().strip() in ('1', '2', '3'),
            'native_ptrace_policy_unavailable')
    require(Path('/usr/bin/gio').is_file() and Path('/usr/bin/systemctl').is_file(), 'vm_tools_missing')
    return account


def make_profile(account, root, registry, transaction):
    host = socket.gethostname().lower()
    require(ID.fullmatch(host), 'vm_hostname')
    machine = Path('/etc/machine-id').read_text().strip()
    require(len(machine) == 32 and all(c in '0123456789abcdefABCDEF' for c in machine), 'machine_id')
    value = {'schema': 'ags.routeb-native-identity.v1', 'logical_host': host,
             'host_instance': transaction + '-host', 'uid': account.pw_uid, 'gid': account.pw_gid,
             'user': account.pw_name, 'home': account.pw_dir, 'root': str(root),
             'platform': 'Linux', 'arch': 'x86_64', 'machine_id_kind': 'machine-id',
             'machine_id_sha256': sha(('ags.host-machine.v1\0machine-id\0' + machine.lower()).encode()),
             'aliases': [host], 'scope': 'isolated-native', 'transaction_id': transaction,
             'tool_bundle': str(registry / 'bundle')}
    ref = exclusive_json(registry / 'host-profile.json', value, mode=0o644)
    return value, ref


def provision(source, build, source_evidence, authorization_path, authorization_digest, uid, transaction):
    source, build = absolute(source), absolute(build)
    require(ID.fullmatch(transaction) and transaction.startswith('gha-'), 'transaction_id')
    account = preflight_vm(uid, source)
    auth, auth_sha = authorization.source_input(authorization_path, authorization_digest)
    records = {name: read_json(source_evidence / (name + '.json'))
               for name in ('repository', 'commit', 'ci-run', 'workflow', 'dispatch')}
    dispatch = records['dispatch'][0]
    revision = dispatch['revision']
    tree = validate_source(*(records[name][0] for name in ('repository', 'commit', 'ci-run', 'workflow')), revision)
    require(dispatch['event'] == 'workflow_dispatch' and dispatch['runner_environment'] == 'github-hosted'
            and dispatch['repository'] == 'yoyooyooo/agent-git-service'
            and transaction == 'gha-%s-%s' % (dispatch['run_id'], dispatch['attempt']), 'dispatch_scope')
    require(as_user(account, ['git', '-C', source, 'rev-parse', 'HEAD']).decode().strip() == revision
            and as_user(account, ['git', '-C', source, 'rev-parse', 'HEAD^{tree}']).decode().strip() == tree
            and not as_user(account, ['git', '-C', source, 'status', '--porcelain']), 'source_changed')
    base = Path(account.pw_dir) / '.local/state/ags-routeb-native'
    parent_directories(base, account.pw_uid, account.pw_gid, 0o700)
    parent = mkdir(base / transaction, account.pw_uid, account.pw_gid)
    root = mkdir(parent / 'ags', account.pw_uid, account.pw_gid)
    for name in ('bin', 'config', 'state', 'logs', 'data'):
        mkdir(root / name, account.pw_uid, account.pw_gid)
    mkdir(root / 'data/git', account.pw_uid, account.pw_gid)
    for name in ('package', 'recovery', 'continuation'):
        mkdir(parent / name, account.pw_uid, account.pw_gid)
    for name in ('backups', 'restores', 'authority-assets', 'approvals'):
        mkdir(parent / 'recovery' / name, account.pw_uid, account.pw_gid)
    for path in (REGISTRY, INTERPRETERS):
        parent_directories(path)
        protected_directory(path)
    parent_directories(Path('/run/ags-routeb-stage0'))
    protected_directory(Path('/run/ags-routeb-stage0'))
    registry = mkdir(REGISTRY / transaction, mode=0o755)
    evidence = mkdir(registry / 'fixture-evidence')
    for name, (_, raw) in records.items():
        exclusive(evidence / ('source-' + name + '.json'), raw)
    # The private source is represented by its digest only outside the grant.
    exclusive_json(evidence / 'authorization-source.json', {
        'schema': 'ags.routeb-fixture-authorization-readback.v1', 'sha256': auth_sha,
        'captured_at': auth['captured_at'], 'delegate': auth['delegate'], 'native_pass': False})
    sys.path.insert(0, str(source / 'scripts'))
    bundle_module = importlib.import_module('upgrade_bundle')
    bundle_module.build(source / 'scripts', registry / 'bundle')
    os.chmod(registry / 'bundle', 0o755)
    for path in (registry / 'bundle').iterdir():
        os.chmod(path, 0o644)
    tool_ref = reference(registry / 'bundle/manifest.json')
    host, host_ref = make_profile(account, root, registry, transaction)
    binding = {'host_instance': host['host_instance'], 'host_profile_sha256': host_ref['sha256'],
               'tool_revision': revision, 'tool_tree': tree, 'tool_manifest_sha256': tool_ref['sha256'],
               'platform': 'linux', 'architecture': 'amd64'}
    catalog_ref = authority.produce(source / 'scripts/routeb-native-fixture/python-publisher.json',
                                    transaction, binding, build / 'native-tools')
    catalog = read_json(catalog_ref['path'], 16 << 20)[0]
    interpreter_path = Path(catalog['prefix']['root']) / catalog['executable']
    interpreter = {'path': str(interpreter_path), 'sha256': sha(read_bytes(interpreter_path, 128 << 20))}
    catalog_input = exclusive_json(evidence / 'catalog-input.json', {
        'id': transaction, 'sha256': catalog_ref['sha256'], 'binding': binding})
    catalog_result = command([str(build / 'native-tools'), 'verify-catalog', catalog_input['path']], timeout=120)
    exclusive(evidence / 'catalog-result.json', catalog_result)
    grant_ref = authorization.grant(registry / 'scope-grant.json', auth, host)
    stage = mkdir(registry / 'stage0', mode=0o755)
    mkdir(stage / 'recovery', mode=0o755)
    stage_raw = read_bytes(build / 'ags-routeb-stage0', 64 << 20)
    stage_ref = exclusive(stage / 'ags-routeb-stage0', stage_raw, mode=0o755)
    stage_recovery = exclusive(stage / 'recovery/ags-routeb-stage0', stage_raw, mode=0o755)
    unit_recovery = mkdir(registry / 'unit-recovery', mode=0o755)
    phases = ('bootstrap', 'plan', 'transaction', 'continue-installed', 'restore', 'old-start',
              'serve-initial', 'serve-candidate', 'serve-old')
    invocations, definitions = [], {}
    for phase in phases:
        operation = 'serve' if phase.startswith('serve-') else phase
        name, raw = unit(transaction, operation, stage / 'ags-routeb-stage0')
        if name not in definitions:
            definitions[name] = (exclusive(Path('/etc/systemd/system') / name, raw, mode=0o644),
                                 exclusive(unit_recovery / name, raw, mode=0o644))
        invocations.append({'id': transaction + '-' + phase, 'operation': operation, 'phase': phase,
                            'attempt_id': transaction + '-attempt', 'authority_epoch': 1,
                            'authorization_id': transaction + '-' + phase + '-auth', 'unit': name,
                            'definition': definitions[name][0], 'definition_recovery': definitions[name][1]})
    mkdir(registry / 'state')
    for name in ('tickets', 'claims', 'active', 'effects', 'exits', 'failures', 'failure-exits',
                 'manager-start', 'manager-completion'):
        mkdir(registry / 'state' / name)
    user = {'uid': account.pw_uid, 'gid': account.pw_gid}
    package_dir = parent / 'package'
    binary_raw = read_bytes(build / 'gh-server', 1 << 30)
    binary_sha = sha(binary_raw)
    exclusive(package_dir / 'candidate-bin', binary_raw, mode=0o700, **user)
    exclusive(root / 'bin/gh-server', binary_raw, mode=0o700, **user)
    version_raw = as_user(account, [root / 'bin/gh-server', '--version'], timeout=15)
    version = object_json(version_raw)
    require(version.get('schema') == 'ags.build.v1' and version.get('command') == 'gh-server'
            and version.get('revision') == revision and version.get('tree') == tree
            and version.get('goos') == 'linux' and version.get('goarch') == 'amd64', 'candidate_build_identity')
    exclusive(evidence / 'candidate-version.json', version_raw)
    candidate = {'schema': 'ags.routeb-candidate-manifest.v1', 'host': host['logical_host'],
                 'platform': 'linux/amd64', 'binary': str(package_dir / 'candidate-bin'),
                 'sha256': binary_sha, 'built_revision': revision, 'built_tree': tree,
                 'baseline_revision': revision, 'baseline_tree_documented': tree, 'version': version,
                 'toolchain_requested': version['go_version'], 'host_go_version': version['go_version'],
                 'cgo_requested': '1', 'go_build_settings': {},
                 'not_a_release': True, 'not_installed': True, 'started_against_ags_home': False}
    candidate_ref = exclusive_json(package_dir / 'candidate-manifest.json', candidate, **user)
    launcher = exclusive(root / 'bin/ags-runtime.py', read_bytes(registry / 'bundle/routeb_launcher.py'),
                         mode=0o700, **user)
    installation = {'schema': 'ags.installation.v2', 'artifact_kind': 'exact-source', 'source': revision,
                    'runtime_tree': tree, 'candidate_manifest_sha256': candidate_ref['sha256'],
                    'tool_sha256': tool_ref['sha256'],
                    'binaries': {'gh-server': {'sha256': binary_sha, 'identity': version}}}
    installation_ref = exclusive_json(root / 'state/installation.json', installation, **user)
    with socket.socket() as listener:
        listener.bind(('127.0.0.1', 0))
        port = listener.getsockname()[1]
    integrations = exclusive(root / 'config/integrations.yaml',
        b'forgejo:\n  enabled: false\ngitlab:\n  enabled: false\ngithub:\n  enabled: false\n'
        b'multica:\n  enabled: false\ngithub_app:\n  enabled: false\n', **user)
    environment = {'PORT': str(port), 'BASE_URL': 'http://127.0.0.1:' + str(port),
                   'AGS_API_BASE_URL': 'http://127.0.0.1:' + str(port),
                   'CONSOLE_BASE_URL': 'http://127.0.0.1:' + str(port),
                   'LISTEN_MODE': 'development', 'AGS_LISTEN_HOST': '127.0.0.1',
                   'DB_DSN': 'sqlite:' + str(root / 'data/main.db'), 'GIT_REPO_DIR': str(root / 'data/git'),
                   'AGS_INTEGRATIONS_CONFIG': integrations['path'], 'ENVIRONMENT': 'development',
                   'ALLOW_ANY_TOKEN': 'false', 'ADMIN_LOGIN': 'fixture-admin',
                   'ADMIN_TOKEN': secrets.token_hex(32)}
    import shlex
    env_ref = exclusive(root / 'config/runtime.env',
                        ''.join(k + '=' + shlex.quote(v) + '\n' for k, v in environment.items()).encode(), **user)
    exclusive(root / 'logs/runtime.log', b'', **user)
    seed = as_user(account, [build / 'native-tools', 'seed-db', root / 'data/main.db'])
    exclusive(evidence / 'database-seed.json', seed)
    inventory_module = importlib.import_module('upgrade_inventory')
    schema_sha = inventory_module.schema_digest(root / 'data/main.db')
    inventory_module.check_database_schema(root / 'data/main.db', schema_sha)
    review = {'schema': 'ags.routeb-build-review.v1', 'revision': revision, 'tree': tree,
              'binary_sha256': binary_sha, 'manifest_sha256': candidate_ref['sha256'],
              'decision': 'approve', 'reviewer': authorization.DELEGATE}
    review_ref = exclusive_json(package_dir / 'build-review.json', review, **user)
    ci = records['ci-run'][0]
    ci_ref = exclusive_json(package_dir / 'ci-evidence.json', {
        'schema': 'ags.routeb-ci-evidence.v1', 'repository': 'yoyooyooo/agent-git-service',
        'revision': revision, 'tree': tree, 'run_id': ci['id'], 'attempt': ci['run_attempt'],
        'conclusion': ci['conclusion'], 'evidence_sha256': sha(records['ci-run'][1])}, **user)
    refs = [{'kind': 'accepted-source-ci', 'sha256': ci_ref['sha256']},
            {'kind': 'actual-build-review', 'sha256': review_ref['sha256']},
            {'kind': 'source-authorization', 'sha256': auth_sha}]
    manager = next(row for row in invocations if row['phase'] == 'serve-initial')
    # Explicit complete fixture graph. No business data or production inventory.
    mutable = [('database', 'sqlite', root / 'data/main.db', 'file'),
               ('git', 'git_root', root / 'data/git', 'directory'),
               ('environment', 'runtime_env', root / 'config/runtime.env', 'file'),
               ('binary', 'old_binary', root / 'bin/gh-server', 'file'),
               ('installation', 'installation', root / 'state/installation.json', 'file'),
               ('launcher', 'launcher', root / 'bin/ags-runtime.py', 'file'),
               ('integrations', 'config', root / 'config/integrations.yaml', 'file')]
    immutable = [('manager', 'manager', Path(manager['definition']['path'])),
                 ('identity', 'policy', Path(host_ref['path']))]
    immutable += [('tool-' + str(index), 'tool', path)
                  for index, path in enumerate(sorted((registry / 'bundle').iterdir()))]
    backup_module = importlib.import_module('upgrade_backup')
    objects = [{'id': key, 'role': role, 'source': str(path), 'restore_target': str(path),
                'kind': kind, 'depends_on': []} for key, role, path, kind in mutable]
    metadata_rows = [{'role': role, 'source': str(path), 'asset_sha256': sha(read_bytes(path, 16 << 20)),
                      'metadata': backup_module.supported_metadata(path)} for _, role, path in immutable]
    installer_sha = sha(read_bytes(Path(__file__)))
    immutable_approval = authorization.approve({
        'schema': 'ags.immutable-authority-approval.v2', 'authorization_id': transaction + '-immutable-auth',
        'host_instance': host['host_instance'], 'tool_manifest_sha256': tool_ref['sha256'],
        'installer_sha256': installer_sha, 'objects': metadata_rows}, 'immutable-provision', grant_ref, refs)
    immutable_approval_ref = exclusive_json(registry / 'immutable-approval.json', immutable_approval, mode=0o644)
    receipt_dir = mkdir(registry / 'immutable-receipts', mode=0o755)
    for (key, role, path), row in zip(immutable, metadata_rows):
        asset = exclusive(parent / 'recovery/authority-assets' / key, read_bytes(path, 16 << 20), **user)
        receipt_ref = exclusive_json(receipt_dir / (key + '.json'), {
            'schema': 'ags.immutable-authority-provision.v1', 'host_instance': host['host_instance'],
            'role': role, 'source': str(path), 'asset_sha256': asset['sha256'], 'metadata': row['metadata'],
            'tool_manifest_sha256': tool_ref['sha256'], 'installer_sha256': installer_sha,
            'provisioned_at': utc(), 'decision': 'provisioned',
            'approval_sha256': immutable_approval_ref['sha256'],
            'authorization_id': immutable_approval['authorization_id']}, mode=0o644)
        objects.append({'id': key, 'role': role, 'source': str(path), 'restore_target': str(path),
                        'kind': 'file', 'depends_on': [], 'immutable_authority': {
            'schema': 'ags.immutable-authority.v1', 'host_instance': host['host_instance'],
            'asset': asset['path'], 'asset_sha256': asset['sha256'], 'size': asset['size'],
            'provision_receipt': receipt_ref['path'], 'provision_receipt_sha256': receipt_ref['sha256'],
            'provision_approval': immutable_approval_ref['path'],
            'provision_approval_sha256': immutable_approval_ref['sha256'],
            'tool_manifest': tool_ref['path'], 'tool_manifest_sha256': tool_ref['sha256'],
            'metadata': row['metadata']}})
    recovery_review_ref = exclusive_json(package_dir / 'recovery-review.json', {
        'schema': 'ags.routeb-fixture-dependency-review.v1', 'reviewer': authorization.DELEGATE,
        'reviewed_at': utc(), 'source_revision': revision, 'objects': objects,
        'schema_sha256': schema_sha, 'real_business_data': False, 'scope': 'isolated-native'}, **user)
    policy_ref = exclusive_json(package_dir / 'policy.json', {
        'schema': 'ags.routeb-host-policy.v1', 'host': host['logical_host'], 'old_revision': revision,
        'old_tree': tree, 'candidate_revision': revision, 'candidate_tree': tree,
        'schema_sha256': schema_sha, 'allow_schema_change': False}, **user)
    total = sum(Path(row['source']).stat().st_size for row in objects if row['kind'] == 'file')
    reserve = 64 << 20
    group = {'schema': 'ags.recovery-group.v2', 'objects': objects,
             'dependency_review_sha256': recovery_review_ref['sha256'],
             'storage': {'backup_root': str(parent / 'recovery/backups'),
                         'restore_root': str(parent / 'recovery/restores'),
                         'private_policy_sha256': policy_ref['sha256'], 'copy_method': 'ordinary_copy'},
             'retention': importlib.import_module('upgrade_backup_contract').RETENTION,
             'capacity': {'backup_bytes': total + reserve, 'restore_copy_bytes': total + reserve,
                          'candidate_scratch_bytes': len(binary_raw) + reserve, 'migration_growth_bytes': reserve,
                          'runtime_reserve_bytes': reserve, 'available_bytes': shutil.disk_usage(parent).free}}
    importlib.import_module('upgrade_backup_contract').validate_group(group)
    group_ref = exclusive_json(package_dir / 'recovery-group.json', group, **user)
    trash = Path(account.pw_dir) / '.local/share/Trash/files'
    parent_directories(trash, account.pw_uid, account.pw_gid, 0o700)
    native = {'schema': 'ags.routeb-native-profile.v2', 'root': str(root), 'tool_revision': revision,
              'tool_tree': tree, 'interpreter': interpreter, 'launcher_sha256': launcher['sha256'],
              'build_review_sha256': review_ref['sha256'], 'ci_evidence_sha256': ci_ref['sha256'],
              'recovery_review_sha256': recovery_review_ref['sha256'], 'schema_sha256': schema_sha,
              'trash_root': str(trash), 'stage0': {'registration_id': transaction}, 'capture': {
              'mode': 'isolated-native', 'manager': {'kind': 'systemd', 'scope': 'system',
                  'target': manager['unit'], 'definition': manager['definition']['path'],
                  'definition_sha256': manager['definition']['sha256']},
              'runtime_path': str(root / 'state/runtime.json'), 'installation_path': installation_ref['path'],
              'binary_path': str(root / 'bin/gh-server'),
              'health': {'local': environment['BASE_URL'] + '/readyz', 'service': environment['BASE_URL'] + '/readyz'},
              'logs': [{'name': 'app', 'path': str(root / 'logs/runtime.log')}],
              'interval_seconds': 5, 'max_gap_seconds': 30, 'duration_seconds': 600, 'lease_seconds': 0}}
    native_ref = exclusive_json(package_dir / 'native-profile.json', native, **user)
    original = next(row for row in invocations if row['phase'] == 'transaction')
    package = {'schema': 'ags.routeb-host-package.v1', 'host': host['logical_host'],
               'host_instance': host['host_instance'], 'deployment_id': transaction,
               'transaction_id': transaction, 'attempt_id': original['attempt_id'],
               'authorization_id': original['authorization_id'],
               'candidate_manifest_sha256': candidate_ref['sha256'], 'candidate_revision': revision,
               'candidate_tree': tree, 'candidate_transaction_id': transaction, 'tool_sha256': tool_ref['sha256'],
               'policy_sha256': policy_ref['sha256'], 'recovery_group_sha256': group_ref['sha256'],
               'native_profile_sha256': native_ref['sha256']}
    package_ref = exclusive_json(package_dir / 'package.json', package, **user)
    window = authorization.approve({'schema': 'ags.routeb-window.v3',
        'authorization_id': original['authorization_id'], 'package_sha256': package_ref['sha256'],
        'host': host['logical_host'], 'transaction_id': transaction, 'start_restores_business': True,
        'actions': ['stop', 'install', 'start']}, 'host-window', grant_ref, refs)
    window_ref = exclusive_json(package_dir / 'approval.json', window, **user)
    provision_binding = {'transaction_id': transaction, 'host_instance': host['host_instance'],
        'host': host['logical_host'], 'root': str(root), 'uid': account.pw_uid, 'gid': account.pw_gid,
        'root_device': root.stat().st_dev, 'profile_sha256': host_ref['sha256'],
        'tool_sha256': tool_ref['sha256'], 'tool_revision': revision, 'tool_tree': tree,
        'candidate_manifest_sha256': candidate_ref['sha256'], 'installation_sha256': installation_ref['sha256'],
        'runtime_env_sha256': env_ref['sha256'], 'launcher_sha256': launcher['sha256'],
        'interpreter': interpreter, 'provision_authorization_id': transaction + '-provision-auth'}
    provision_approval = authorization.approve({'schema': 'ags.routeb-native-provision-approval.v2',
        'authorization_id': provision_binding['provision_authorization_id'], 'purpose': 'native-provision',
        'binding_sha256': fingerprint(provision_binding)}, 'native-provision', grant_ref, refs)
    provision_approval_ref = exclusive_json(registry / 'provision-approval.json', provision_approval, mode=0o644)
    provision_ref = exclusive_json(registry / 'provision.json', {
        **provision_binding, 'schema': 'ags.routeb-native-provision.v1', 'provisioned_at': utc(),
        'provision_approval_sha256': provision_approval_ref['sha256']}, mode=0o644)
    identity = {key: value for key, value in host.items()
                if key not in ('schema', 'scope', 'transaction_id', 'tool_bundle')}
    identity['profile_sha256'] = host_ref['sha256']
    identity_sha = sha(json.dumps(identity, sort_keys=True, separators=(',', ':')).encode())
    bootstrap = next(row for row in invocations if row['phase'] == 'bootstrap')
    bootstrap_approval = authorization.approve({'schema': 'ags.routeb-native-bootstrap-approval.v2',
        'authorization_id': bootstrap['authorization_id'], 'purpose': 'native-bootstrap',
        'root': str(root), 'host_instance': host['host_instance'], 'transaction_id': transaction,
        'provision_sha256': provision_ref['sha256'], 'identity_sha256': identity_sha, 'epoch': 1},
        'native-bootstrap', grant_ref, refs)
    bootstrap_ref = exclusive_json(package_dir / 'bootstrap-approval.json', bootstrap_approval, **user)
    registration = {'schema': 'ags.routeb-stage0.v1', 'id': transaction, 'scope': 'isolated-native',
        'binding': binding, 'native_image': stage_ref, 'native_recovery_image': stage_recovery,
        'host_profile': host_ref, 'interpreter': {'id': transaction, 'sha256': catalog_ref['sha256']},
        'inputs': {'package': package_ref, 'approval': window_ref, 'bootstrap_approval': bootstrap_ref},
        'invocations': invocations, 'scope_grant': grant_ref}
    registration_ref = exclusive_json(registry / 'stage0-registration.json', registration, mode=0o644)
    initial = next(row for row in invocations if row['phase'] == 'serve-initial')
    exclusive_json(registry / 'state/tickets' / (initial['id'] + '.json'), {
        'schema': 'ags.routeb-stage0-ticket.v1', 'registration_sha256': registration_ref['sha256'],
        'invocation_id': initial['id'], 'phase': initial['phase'], 'attempt_id': initial['attempt_id'],
        'authority_epoch': initial['authority_epoch'], 'authorization_id': initial['authorization_id'],
        'issued_by_invocation_id': 'provision', 'recovery_registration_sha256': ''})
    result = {'schema': 'ags.routeb-fixture-provision.v1', 'recorded_at': utc(),
              'registration': registration_ref, 'host_profile': host_ref, 'root': str(root),
              'source_revision': revision, 'source_tree': tree, 'binary_sha256': binary_sha,
              'scope_grant_sha256': grant_ref['sha256'], 'authorization_source_sha256': auth_sha,
              'candidate_manifest_sha256': candidate_ref['sha256'], 'native_pass': False}
    exclusive_json(evidence / 'provision.json', result)
    return result
