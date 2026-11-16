#!/usr/bin/env python3
"""Closed private package loader. No manager action or capability provisioning."""
from dataclasses import dataclass
from datetime import datetime, timezone
import hashlib
import importlib.util
import os
from pathlib import Path
import re
import sqlite3
import stat
from urllib.parse import urlsplit

from upgrade import bundle_engine, directory_fd
from upgrade_backup_contract import (MAX_JSON, ContractError, EXACT_COLD_SCHEMA, digest, fields, require, strict_json,
                                     validate_cold_binding, validate_group)
from upgrade_backup import group_object
from upgrade_inventory import file_digest, sqlite_path
import routeb_candidate
import routeb_host_admission
from routeb_host_identity import resolve_for_root


PROFILE_SCHEMA = 'ags.routeb-native-profile.v1'
NATIVE_PROFILE_SCHEMA = 'ags.routeb-native-profile.v2'
PROFILE_FIELDS = ('schema root tool_revision tool_tree interpreter launcher_sha256 '
                  'build_review_sha256 ci_evidence_sha256 recovery_review_sha256 '
                  'schema_sha256 trash_root capture').split()
POLICY_FIELDS = ('schema host old_revision old_tree candidate_revision candidate_tree '
                 'schema_sha256 allow_schema_change').split()
SIDE_FILES = ('native-profile.json', 'policy.json', 'recovery-group.json', 'build-review.json',
              'ci-evidence.json', 'recovery-review.json')
INTERPRETER_CLOSURE_BLOCKER = 'interpreter_full_closure_required'
PRIVATE_DOCUMENT_LIMIT = 1024 * 1024
# Original-byte cap shared with upgrade_backup_contract.MAX_JSON and Go's
# verifyRestoreArtifact. A caller selects a lower budget; document content,
# CLI, environment and reference.size cannot raise this cap.
RESTORE_RESULT_LIMIT = 16 * 1024 * 1024
if RESTORE_RESULT_LIMIT != MAX_JSON:
    raise ContractError('private_document_budget')


def require_interpreter_closure(root=None, operations=None):
    """Require the kernel-authenticated, live registered native parent.

    File-only evidence, a flag and direct Python invocation cannot satisfy it.
    The parent currently admits only its isolated Linux authority contract.
    Public callers retain one blocker; the client preserves the exact refusal
    as its cause for diagnosis. Unexpected programming errors are not masked.
    """
    from routeb_stage0_client import context
    try:
        return context(root, operations=operations)
    except ContractError as error:
        raise ContractError(INTERPRETER_CLOSURE_BLOCKER) from error


def read_document(path, *, maximum=PRIVATE_DOCUMENT_LIMIT):
    """Read one private JSON document within a source-selected byte budget.

    The default remains the ordinary package, approval and sidecar limit.
    Only the fixed restore-result caller selects RESTORE_RESULT_LIMIT.
    """
    require(type(maximum) is int and 0 < maximum <= RESTORE_RESULT_LIMIT, 'private_document_budget')
    path = Path(path)
    require(path.is_absolute(), 'private_path_invalid')
    with directory_fd(path.parent, private=True) as parent:
        fd = os.open(path.name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=parent)
        try:
            before = os.fstat(fd)
            require(stat.S_ISREG(before.st_mode) and before.st_uid == os.getuid()
                    and stat.S_IMODE(before.st_mode) == 0o600 and before.st_nlink == 1
                    and 0 < before.st_size <= maximum, 'private_document_unsafe')
            raw = os.read(fd, maximum + 1)
            after = os.fstat(fd)
            signature = lambda info: (info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns, info.st_ctime_ns)
            require(len(raw) == before.st_size and len(raw) <= maximum
                    and signature(before) == signature(after), 'private_document_changed')
        finally:
            os.close(fd)
    value = strict_json(raw)
    require(type(value) is dict, 'private_document_invalid')
    return value, raw, hashlib.sha256(raw).hexdigest()


def oid(value):
    from upgrade_backup_contract import OID
    require(type(value) is str and OID.fullmatch(value) is not None and value != '0' * 40,
            'revision_invalid')


def validate_capture_profile(profile, host):
    spec = profile['capture']
    fields(spec, ('mode', 'manager', 'runtime_path', 'installation_path', 'binary_path',
                  'health', 'logs', 'interval_seconds', 'max_gap_seconds', 'duration_seconds', 'lease_seconds'))
    root = Path(host.identity.root)
    require(spec['mode'] == host.scope, 'native_scope_mismatch')
    for key, relative in (('runtime_path', 'state/runtime.json'), ('installation_path', 'state/installation.json'),
                          ('binary_path', 'bin/gh-server')):
        require(spec[key] == str(root / relative), 'capture_path_mismatch')
    manager = spec['manager']
    names = ('kind', 'target', 'definition', 'definition_sha256')
    if manager.get('kind') == 'systemd':
        names += ('scope',)
    fields(manager, names)
    digest(manager['definition_sha256'])
    target = manager['target']
    definition = Path(manager['definition'])
    require(definition.is_absolute() and definition.resolve() == definition, 'manager_definition_invalid')
    if manager['kind'] == 'launchd':
        require(host.identity.platform == 'Darwin'
                and bool(re.fullmatch(r'gui/' + str(host.identity.uid) + r'/[A-Za-z0-9._-]+', target)),
                'manager_target_invalid')
        label = target.rsplit('/', 1)[-1]
        require(label.startswith('com.jackie.ags-hostpkg-test.') if host.scope == 'isolated-native'
                else label == 'com.jackie.agent-git-service', 'manager_target_invalid')
    else:
        require(manager['kind'] == 'systemd' and host.identity.platform == 'Linux'
                and bool(re.fullmatch(r'[A-Za-z0-9._-]+\.service', target)), 'manager_target_invalid')
        native = profile.get('schema') == NATIVE_PROFILE_SCHEMA
        isolated = ((manager['scope'] == 'system' and target == 'ags-hostpkg-test-' + root.parent.name + '.service')
                    if native else (manager['scope'] == 'user' and target.startswith('ags-hostpkg-test-')))
        require(isolated
                if host.scope == 'isolated-native' else
                (manager['scope'] == 'system' and target == 'agent-git-service.service'), 'manager_target_invalid')
    if host.scope == 'isolated-native':
        if profile.get('schema') == NATIVE_PROFILE_SCHEMA and manager['kind'] == 'systemd':
            require(definition == Path('/etc/systemd/system') / target, 'native_definition_outside_root')
        else:
            require(root in definition.parents, 'native_definition_outside_root')
    fields(spec['health'], ('local', 'service'))
    for name, value in spec['health'].items():
        parsed = urlsplit(value)
        require(parsed.scheme in ('http', 'https') and parsed.path == '/readyz' and parsed.hostname
                and not parsed.username and not parsed.password and not parsed.query and not parsed.fragment,
                'health_endpoint_invalid')
        if name == 'local':
            require(parsed.hostname in ('127.0.0.1', 'localhost', '::1') and parsed.port,
                    'local_health_endpoint_invalid')
    for name in ('duration_seconds', 'lease_seconds', 'interval_seconds', 'max_gap_seconds'):
        require(type(spec[name]) is int, 'capture_timing_invalid')
    require(600 <= spec['duration_seconds'] <= 86400 and 0 <= spec['lease_seconds'] <= 86400
            and 1 <= spec['interval_seconds'] <= 30
            and spec['interval_seconds'] <= spec['max_gap_seconds'] <= 60, 'capture_timing_invalid')
    require(type(spec['logs']) is list and bool(spec['logs']), 'capture_logs_invalid')
    names = set()
    for row in spec['logs']:
        fields(row, ('name', 'path'))
        require(re.fullmatch('[A-Za-z0-9_-]+', row['name']) and row['name'] not in names,
                'capture_logs_invalid')
        names.add(row['name'])
        path = Path(row['path'])
        require(path.is_absolute() and path.resolve() == path and root in path.parents, 'capture_logs_invalid')


@dataclass
class HostPackage:
    path: Path
    approval_path: Path
    package: dict
    package_raw: bytes
    approval: dict
    approval_sha256: str
    candidate: routeb_candidate.VerifiedCandidate
    profile: dict
    policy: dict
    group: dict
    recovery_review: dict
    host: object
    documents: dict
    old_receipt: dict
    old_receipt_sha256: str
    dsn: str

    @property
    def root(self):
        return Path(self.host.identity.root)

    @property
    def pending(self):
        return self.root / 'state/ags-autodeploy/local-v2/pending'

    def authorize(self, *, consumed_at=None):
        if self.profile.get('schema') == NATIVE_PROFILE_SCHEMA:
            require_interpreter_closure(self.root, operations=('plan', 'transaction', 'restore', 'old-start'))
        self.host.recheck()
        _, raw, _ = read_document(self.path)
        require(raw == self.package_raw, 'package_replaced')
        approval, _, sha = read_document(self.approval_path)
        require(sha == self.approval_sha256, 'approval_replaced')
        for name, expected in self.documents.items():
            _, _, observed = read_document(self.path.parent / name)
            require(observed == expected, 'package_dependency_replaced')
        current = routeb_candidate.load_candidate(self.path.parent / 'candidate-manifest.json',
                    {'revision': self.package['candidate_revision'], 'tree': self.package['candidate_tree']})
        require(current == self.candidate, 'candidate_replaced')
        bundle_engine(self.host.bundle, self.package['tool_sha256'])
        verify_launch_dependencies(self.profile, self.host)
        return routeb_host_admission.admit(self.package, approval, current,
                    now=consumed_at if consumed_at is not None else datetime.now(timezone.utc), raw=raw)

    def cold_binding(self):
        value = {key: self.package[key] for key in
                 ('host_instance', 'deployment_id', 'transaction_id', 'attempt_id', 'authorization_id',
                  'tool_sha256', 'policy_sha256')}
        value.update(schema=EXACT_COLD_SCHEMA, artifact_kind='exact-source',
                     old=self.policy['old_revision'], new=self.candidate.revision,
                     runtime_tree=self.candidate.tree,
                     manifest_sha256=self.candidate.raw_manifest_sha256,
                     build_review_sha256=self.profile['build_review_sha256'],
                     ci_evidence_sha256=self.profile['ci_evidence_sha256'],
                     target=self.candidate.platform.replace('/', '_'),
                     old_binary_sha256=self.old_receipt['binaries']['gh-server']['sha256'],
                     new_binary_sha256=self.candidate.sha256)
        return validate_cold_binding(value)

    def execution_blockers(self):
        blockers = python_capability_blockers()
        try:
            require(self.profile.get('schema') == NATIVE_PROFILE_SCHEMA, INTERPRETER_CLOSURE_BLOCKER)
            value = require_interpreter_closure(self.root, operations=('plan', 'transaction', 'restore', 'old-start'))
            require(value['registration_id'] == self.profile['stage0']['registration_id']
                    and value['tool_revision'] == self.profile['tool_revision']
                    and value['tool_tree'] == self.profile['tool_tree']
                    and value['tool_manifest_sha256'] == self.package['tool_sha256'],
                    INTERPRETER_CLOSURE_BLOCKER)
        except (ContractError, OSError, KeyError, TypeError):
            blockers.append(INTERPRETER_CLOSURE_BLOCKER)
        if self.host.scope == 'production':
            blockers.append('native_full_closure_required')
        for item in self.group['objects']:
            if 'immutable_authority' in item:
                from routeb_immutable_authority import verify
                verify(item)
                continue
            source = Path(item['source'])
            paths = [source, *source.rglob('*')] if source.is_dir() else [source]
            if any(path.lstat().st_uid != os.getuid() or path.lstat().st_gid != os.getgid() for path in paths):
                blockers.append('privileged_metadata_materializer_required')
                break
        return sorted(set(blockers))


def python_capability_blockers():
    # S1 uses the native SQLite row/statement limits; do not stop first and
    # discover an older interpreter's missing API during backup.
    return ([] if all(hasattr(sqlite3.Connection, name) for name in ('setlimit', 'getlimit'))
            else ['python_sqlite_limit_api_unavailable'])


def _runtime_environment(bundle, root):
    specification = importlib.util.spec_from_file_location('routeb_environment', bundle / 'ags-runtime.py')
    module = importlib.util.module_from_spec(specification)
    specification.loader.exec_module(module)
    return module.environment(root / 'config/runtime.env')


def verify_launch_dependencies(profile, host):
    """Read these unchanged launch dependencies again before every manager call."""
    for launcher in (host.bundle / 'routeb_launcher.py', Path(host.identity.root) / 'bin/ags-runtime.py'):
        require(file_digest(launcher) == profile['launcher_sha256'], 'launcher_replaced')
    interpreter = Path(profile['interpreter']['path'])
    require(interpreter.is_absolute() and interpreter.resolve() == interpreter and interpreter.is_file()
            and interpreter.stat().st_uid in (0, os.getuid()) and not interpreter.stat().st_mode & 0o022
            and os.access(interpreter, os.X_OK)
            and file_digest(interpreter) == profile['interpreter']['sha256'], 'interpreter_unverified')


def validate_native_paths(host, *, group=None, environment=None):
    """An isolated manager never authorizes paths belonging to another root."""
    if host.scope != 'isolated-native':
        return
    root = Path(host.identity.root)
    recovery = root.parent / 'recovery'

    def within(value, boundary):
        path = Path(value)
        require(path.is_absolute() and path.resolve() == path and boundary in path.parents,
                'native_path_outside_transaction')

    if group is not None:
        for item in group['objects']:
            if 'immutable_authority' in item:
                # The caller still has to verify the root-owned live authority
                # and its independent approved asset before any stop.
                within(item['immutable_authority']['asset'], recovery / 'authority-assets')
            else:
                within(item['source'], root)
                within(item['restore_target'], root)
        for key, name in (('backup_root', 'backups'), ('restore_root', 'restores')):
            path = Path(group['storage'][key])
            require(path == recovery / name and path.resolve() == path,
                    'native_storage_outside_transaction')
    if environment is not None:
        within(sqlite_path(environment.get('DB_DSN', '')), root)
        within(environment.get('GIT_REPO_DIR', ''), root)
        for key in ('AGS_INTEGRATIONS_CONFIG', 'AGS_REPLICATION_CONFIG_FILE'):
            if environment.get(key):
                within(environment[key], root)


def validate_authority_coverage(group, host, tool_sha256):
    """Every executing root-owned member has its own recoverable authority record."""
    from upgrade_bundle import REQUIRED
    from upgrade_identity import read_trusted
    manifest_path = host.bundle / 'manifest.json'
    raw = read_trusted(manifest_path, maximum=1024 * 1024)
    require(hashlib.sha256(raw).hexdigest() == tool_sha256, 'tool_manifest_changed')
    manifest = strict_json(raw)
    require(set(manifest['files']) == set(REQUIRED), 'tool_member_set_incomplete')
    expected = {str(host.bundle / name): 'tool' for name in (*REQUIRED, 'manifest.json')}
    expected[str(host.profile)] = 'policy'
    by_source = {item['source']: item for item in group['objects']}
    require(set(expected) <= set(by_source), 'tool_authority_coverage_incomplete')
    for source, role in expected.items():
        item = by_source[source]
        require(item['role'] == role and 'immutable_authority' in item,
                'tool_authority_coverage_incomplete')
        value = item['immutable_authority']
        require(value['host_instance'] == host.identity.host_instance
                and value['tool_manifest'] == str(manifest_path)
                and value['tool_manifest_sha256'] == tool_sha256,
                'tool_authority_binding_mismatch')


def load(package_path, approval_path, expected_host, *, consumed_at=None):
    path, approval_path = Path(package_path), Path(approval_path)
    package, raw, _ = read_document(path)
    approval, _, approval_sha = read_document(approval_path)
    sidecars, documents = {}, {}
    for name in SIDE_FILES:
        value, _, sha = read_document(path.parent / name)
        sidecars[name], documents[name] = value, sha
    profile = sidecars['native-profile.json']
    native = profile.get('schema') == NATIVE_PROFILE_SCHEMA
    fields(profile, (*PROFILE_FIELDS, 'stage0') if native else PROFILE_FIELDS)
    require(profile['schema'] in (PROFILE_SCHEMA, NATIVE_PROFILE_SCHEMA), 'native_profile_invalid')
    if native:
        fields(profile['stage0'], ('registration_id',))
        from upgrade_backup_contract import identifier
        identifier(profile['stage0']['registration_id'])
        require(profile['stage0']['registration_id'] == Path(profile['root']).parent.name,
                'native_profile_invalid')
    for key in ('tool_revision', 'tool_tree'):
        oid(profile[key])
    for key in ('launcher_sha256', 'build_review_sha256', 'ci_evidence_sha256',
                'recovery_review_sha256', 'schema_sha256'):
        digest(profile[key])
    fields(profile['interpreter'], ('path', 'sha256'))
    digest(profile['interpreter']['sha256'])
    candidate = routeb_candidate.load_candidate(path.parent / 'candidate-manifest.json',
                 {'revision': package.get('candidate_revision'), 'tree': package.get('candidate_tree')})
    routeb_host_admission.admit(package, approval, candidate, now=consumed_at if consumed_at is not None else datetime.now(timezone.utc), raw=raw)
    require(package['host'] == expected_host and documents['native-profile.json'] == package['native_profile_sha256']
            and documents['policy.json'] == package['policy_sha256']
            and documents['recovery-group.json'] == package['recovery_group_sha256'], 'package_dependency_mismatch')
    for name, key in (('build-review.json', 'build_review_sha256'), ('ci-evidence.json', 'ci_evidence_sha256'),
                      ('recovery-review.json', 'recovery_review_sha256')):
        require(documents[name] == profile[key], 'package_dependency_mismatch')
    host = resolve_for_root(profile['root'])
    require(host.identity.logical_host == expected_host and host.identity.host_instance == package['host_instance'],
            'host_identity_mismatch')
    validate_capture_profile(profile, host)
    bundle_engine(host.bundle, package['tool_sha256'])
    verify_launch_dependencies(profile, host)
    root = Path(host.identity.root)
    policy, group = sidecars['policy.json'], sidecars['recovery-group.json']
    fields(policy, POLICY_FIELDS)
    require(policy['schema'] == 'ags.routeb-host-policy.v1' and policy['host'] == expected_host
            and policy['allow_schema_change'] is False
            and policy['candidate_revision'] == candidate.revision and policy['candidate_tree'] == candidate.tree
            and policy['schema_sha256'] == profile['schema_sha256'], 'host_policy_invalid')
    for key in ('old_revision', 'old_tree'):
        oid(policy[key])
    require(policy['old_revision'] == candidate.revision and policy['old_tree'] == candidate.tree,
            'application_source_transition_not_admitted')
    validate_group(group)
    validate_native_paths(host, group=group)
    validate_authority_coverage(group, host, package['tool_sha256'])
    require(Path(group_object(group, 'old_binary')['source']) == root / 'bin/gh-server'
            and Path(group_object(group, 'installation')['source']) == root / 'state/installation.json'
            and Path(group_object(group, 'runtime_env')['source']) == root / 'config/runtime.env'
            and Path(group_object(group, 'launcher')['source']) == root / 'bin/ags-runtime.py'
            and Path(group_object(group, 'manager')['source']) == Path(profile['capture']['manager']['definition']),
            'recovery_mapping_mismatch')
    environment = _runtime_environment(host.bundle, root)
    validate_native_paths(host, environment=environment)
    dsn = environment.get('DB_DSN', '')
    require(sqlite_path(dsn) == Path(group_object(group, 'sqlite')['source'])
            and environment.get('GIT_REPO_DIR') == group_object(group, 'git_root')['source'],
            'runtime_data_mapping_mismatch')
    old, _, old_sha = read_document(root / 'state/installation.json')
    require(old.get('schema') == 'ags.installation.v2' and old.get('source') == policy['old_revision']
            and old.get('binaries', {}).get('gh-server', {}).get('identity', {}).get('tree') == policy['old_tree']
            and old.get('binaries', {}).get('gh-server', {}).get('sha256') == file_digest(root / 'bin/gh-server'),
            'old_installation_mismatch')
    build = sidecars['build-review.json']
    fields(build, ('schema', 'revision', 'tree', 'binary_sha256', 'manifest_sha256', 'decision', 'reviewer'))
    require(build['schema'] == 'ags.routeb-build-review.v1' and build['decision'] == 'approve'
            and type(build['reviewer']) is str and bool(build['reviewer'])
            and (build['revision'], build['tree'], build['binary_sha256'], build['manifest_sha256']) ==
                (candidate.revision, candidate.tree, candidate.sha256, candidate.raw_manifest_sha256),
            'build_review_mismatch')
    ci = sidecars['ci-evidence.json']
    fields(ci, ('schema', 'repository', 'revision', 'tree', 'run_id', 'attempt', 'conclusion', 'evidence_sha256'))
    require(ci['schema'] == 'ags.routeb-ci-evidence.v1' and ci['repository'] == 'yoyooyooo/agent-git-service'
            and ci['revision'] == candidate.revision and ci['tree'] == candidate.tree
            and type(ci['run_id']) is int and ci['run_id'] > 0 and type(ci['attempt']) is int and ci['attempt'] > 0
            and ci['conclusion'] == 'success', 'ci_evidence_mismatch')
    digest(ci['evidence_sha256'])
    return HostPackage(path, approval_path, package, raw, approval, approval_sha, candidate, profile, policy,
                       group, sidecars['recovery-review.json'], host, documents, old, old_sha, dsn)
