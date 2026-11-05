#!/usr/bin/env python3
"""Activate epoch 1 for one independently provisioned isolated native root.

This is not S4 acceptance or production activation. Root provisioning and its
approval already exist. This module verifies their original bytes and the
installed candidate, then commits one authority event. It never starts a
manager, initializes a business database or provisions a trust file.
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
        if not sys.flags.isolated or location.resolve() != location or os.getuid() == 0:
            raise ValueError()
        for path in (location, *location.parents):
            info = path.lstat()
            if info.st_uid != 0 or info.st_mode & 0o022 or stat.S_ISLNK(info.st_mode) or os.access(path, os.W_OK):
                raise ValueError()
        sys.path.insert(0, str(location.parent))
    except (OSError, ValueError):
        print('code=trusted_bootstrap_entry_required', file=sys.stderr)
        raise SystemExit(78)

from datetime import datetime, timezone
import hashlib
import os
from pathlib import Path
import subprocess

from upgrade import bundle_engine
from upgrade_backup_contract import (ContractError, digest, fields, fingerprint,
                                    identifier, integer, require, strict_json, timestamp)
from upgrade_inventory import file_digest
from routeb_host_identity import resolve_for_root
from routeb_host_package import (read_document, verify_launch_dependencies, oid,
                                 validate_native_paths, _runtime_environment, python_capability_blockers)
from routeb_native_capture import read_regular
from routeb_candidate import load_candidate
import upgrade_state


PROVISION = 'ags.routeb-native-provision.v1'
PROVISION_FIELDS = ('schema transaction_id host_instance host root uid gid root_device '
                    'profile_sha256 tool_sha256 tool_revision tool_tree candidate_manifest_sha256 '
                    'installation_sha256 runtime_env_sha256 launcher_sha256 interpreter provisioned_at '
                    'provision_authorization_id provision_approval_sha256').split()
APPROVAL = 'ags.routeb-native-bootstrap-approval.v1'
APPROVAL_FIELDS = ('schema authorization_id actor purpose decision issued_at expires_at root '
                   'host_instance transaction_id provision_sha256 identity_sha256 epoch').split()


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def trusted_document(path, maximum=1024 * 1024):
    from routeb_immutable_authority import _authority_bytes
    return _authority_bytes(Path(path), maximum)


def check_human(value, purpose, now, *, point=None):
    require(type(value['actor']) is str and value['actor'].startswith('human:')
            and len(value['actor']) > 6 and value['decision'] == 'approve'
            and value['purpose'] == purpose, 'bootstrap_approval_invalid')
    identifier(value['authorization_id'])
    at = now if point is None else point
    require(timestamp(value['issued_at']) <= at < timestamp(value['expires_at']), 'bootstrap_approval_expired')


def read_bindings(root, approval_path):
    require(not python_capability_blockers(), 'python_sqlite_limit_api_unavailable')
    root = Path(root)
    host = resolve_for_root(root)
    require(host.scope == 'isolated-native', 'native_bootstrap_only')
    host.recheck()
    identity = host.identity
    for path in (root / 'state/upgrade-cold.json', root / 'state/installing.json',
                 root / 'state/ags-autodeploy/local-v2/pending'):
        require(not path.exists() and not path.is_symlink(), 'bootstrap_recovery_pending')
    require(sha(trusted_document(host.profile)) == identity.profile_sha256, 'bootstrap_profile_changed')
    raw = trusted_document(host.profile.parent / 'provision.json')
    provision = strict_json(raw)
    fields(provision, PROVISION_FIELDS)
    require(provision['schema'] == PROVISION, 'bootstrap_provision_invalid')
    for key in ('transaction_id', 'host_instance', 'host', 'provision_authorization_id'):
        identifier(provision[key])
    for key in ('profile_sha256', 'tool_sha256', 'candidate_manifest_sha256', 'installation_sha256',
                'runtime_env_sha256', 'launcher_sha256', 'provision_approval_sha256'):
        digest(provision[key]); require(provision[key] != '0' * 64, 'digest_invalid')
    for key in ('tool_revision', 'tool_tree'):
        oid(provision[key])
    for key in ('uid', 'gid', 'root_device'):
        integer(provision[key])
    fields(provision['interpreter'], ('path', 'sha256'))
    digest(provision['interpreter']['sha256'])
    require(provision['root'] == str(root) and provision['uid'] == identity.uid == os.getuid()
            and provision['gid'] == identity.gid == os.getgid()
            and provision['root_device'] == root.stat().st_dev
            and provision['host'] == identity.logical_host
            and provision['host_instance'] == identity.host_instance
            and provision['transaction_id'] == root.parent.name
            and provision['profile_sha256'] == identity.profile_sha256, 'bootstrap_provision_binding_mismatch')
    bundle_engine(host.bundle, provision['tool_sha256'])
    manifest = strict_json(trusted_document(host.bundle / 'manifest.json'))
    require(all(not os.access(host.bundle / name, os.W_OK) for name in manifest['files']),
            'bootstrap_bundle_runtime_writable')
    verify_launch_dependencies(provision, host)
    require(Path(provision['interpreter']['path']) == Path(sys.executable).resolve(),
            'bootstrap_interpreter_mismatch')
    environment_raw = read_regular(root / 'config/runtime.env')
    require(sha(environment_raw) == provision['runtime_env_sha256'], 'bootstrap_environment_changed')
    validate_native_paths(host, environment=_runtime_environment(host.bundle, root))
    require(read_regular(root / 'config/runtime.env') == environment_raw, 'bootstrap_environment_changed')
    approved_raw = trusted_document(host.profile.parent / 'provision-approval.json')
    require(sha(approved_raw) == provision['provision_approval_sha256'], 'bootstrap_provision_approval_changed')
    approved = strict_json(approved_raw)
    fields(approved, ('schema', 'authorization_id', 'actor', 'purpose', 'decision',
                     'issued_at', 'expires_at', 'binding_sha256'))
    binding = {key: value for key, value in provision.items()
               if key not in ('schema', 'provisioned_at', 'provision_approval_sha256')}
    require(approved['schema'] == 'ags.routeb-native-provision-approval.v1'
            and approved['authorization_id'] == provision['provision_authorization_id']
            and approved['binding_sha256'] == fingerprint(binding), 'bootstrap_provision_approval_mismatch')
    now = datetime.now(timezone.utc)
    check_human(approved, 'native-provision', now, point=timestamp(provision['provisioned_at']))
    require(timestamp(provision['provisioned_at']) <= now, 'bootstrap_provision_time_invalid')
    manifest_path = root.parent / 'package/candidate-manifest.json'
    _, manifest_raw, manifest_sha = read_document(manifest_path)
    require(manifest_sha == provision['candidate_manifest_sha256'], 'bootstrap_candidate_manifest_changed')
    installed, installed_raw, installed_sha = read_document(root / 'state/installation.json')
    require(installed_sha == provision['installation_sha256'], 'bootstrap_installation_changed')
    candidate = load_candidate(manifest_path, {'revision': installed.get('source'),
                                              'tree': installed.get('runtime_tree')})
    require(candidate.raw_manifest_sha256 == manifest_sha and candidate.host == identity.logical_host
            and installed.get('schema') == 'ags.installation.v2'
            and installed.get('artifact_kind') == 'exact-source'
            and installed.get('candidate_manifest_sha256') == manifest_sha
            and installed.get('tool_sha256') == provision['tool_sha256']
            and installed.get('binaries', {}).get('gh-server') == {
                'sha256': candidate.sha256, 'identity': candidate.value['version']}
            and file_digest(root / 'bin/gh-server') == candidate.sha256, 'bootstrap_candidate_changed')
    result = subprocess.run([str(root / 'bin/gh-server'), '--version'], capture_output=True, timeout=15,
                            env={'PATH': '/usr/bin:/bin:/usr/sbin:/sbin', 'LC_ALL': 'C'})
    require(result.returncode == 0 and not result.stderr and len(result.stdout) <= 65536
            and strict_json(result.stdout) == candidate.value['version'], 'bootstrap_program_identity_unknown')
    approval, approval_raw, approval_sha = read_document(approval_path)
    fields(approval, APPROVAL_FIELDS)
    require(approval['schema'] == APPROVAL, 'bootstrap_approval_invalid')
    check_human(approval, 'native-bootstrap', datetime.now(timezone.utc))
    require(approval['root'] == str(root) and approval['host_instance'] == identity.host_instance
            and approval['transaction_id'] == provision['transaction_id']
            and approval['provision_sha256'] == sha(raw) and approval['identity_sha256'] == identity.sha256
            and type(approval['epoch']) is int and approval['epoch'] == 1, 'bootstrap_approval_binding_mismatch')
    # Freeze original evidence bytes, not reserialized declarations.
    return {'host': host, 'provision': provision, 'provision_raw': raw, 'provision_sha256': sha(raw),
            'provision_approval_raw': approved_raw, 'approval': approval, 'approval_raw': approval_raw,
            'approval_sha256': approval_sha, 'installation_raw': installed_raw, 'manifest_raw': manifest_raw,
            'runtime_env_raw': environment_raw,
            'binding_sha256': fingerprint({'host_identity_sha256': identity.sha256,
                 'provision_sha256': sha(raw), 'approval_sha256': approval_sha})}


def event_for(value):
    approval, host = value['approval'], value['host']
    operation = 'bootstrap-' + approval['authorization_id']
    return {'event_id': operation + '-active', 'operation_id': operation, 'kind': 'authority_activated',
            'host_instance': host.identity.host_instance, 'identity_sha256': host.identity.sha256,
            'expected_epoch': None, 'epoch': 1, 'transaction_id': approval['transaction_id'],
            'binding_sha256': value['binding_sha256'], 'before_phase': 'native_provisioned',
            'after_phase': 'active', 'approval_sha256': value['approval_sha256'],
            'evidence_sha256': value['provision_sha256'], 'effect': None,
            'recorded_at': datetime.now(timezone.utc).isoformat()}


def completed(root, loaded, value):
    rows = loaded['events']
    if not rows:
        return None
    expected = event_for(value)
    require(len(rows) == 1 and all(rows[0][key] == item for key, item in expected.items()
                                  if key != 'recorded_at'), 'bootstrap_already_bound')
    projection = loaded['projection']
    require(projection['epoch'] == 1 and projection['authority']['state'] == 'active'
            and not projection['pending_effects'], 'bootstrap_already_bound')
    control = root / 'state/ags-autodeploy/local-v2/control'
    for name, raw in archive_rows(value).items():
        require(sha(read_regular(control / name)) == sha(raw), 'bootstrap_archive_changed')
    return {'schema': 'ags.routeb-native-bootstrap.v1', 'status': 'active', 'epoch': 1,
            'host_instance': value['host'].identity.host_instance,
            'binding_sha256': value['binding_sha256'], 'event_sha256': rows[0]['sha256'],
            'effects': [], 'native_pass': False, 'installable': False}


def archive_rows(value):
    return {'approvals/native-bootstrap.json': value['approval_raw'],
            'evidence/native-provision.json': value['provision_raw'],
            'evidence/native-provision-approval.json': value['provision_approval_raw'],
            'evidence/native-installation.json': value['installation_raw'],
            'evidence/native-runtime.env': value['runtime_env_raw'],
            'evidence/native-candidate-manifest.json': value['manifest_raw']}


def activate(root, approval_path):
    root = Path(root)
    value = read_bindings(root, approval_path)
    loaded = upgrade_state.load_verified(root)
    prior = completed(root, loaded, value)
    if prior is not None:
        return prior
    control = root / 'state/ags-autodeploy/local-v2/control'
    require(not control.exists() and not control.is_symlink(), 'bootstrap_state_not_empty')
    # Check and commit under the same journal lock. Calling commit_phase here
    # would reacquire a separate flock; the locked writer is the atomic seam.
    with upgrade_state.StateLock(root):
        loaded = upgrade_state.load_verified(root)
        require(not loaded['events'] and 'format' not in loaded, 'bootstrap_state_not_empty')
        fresh = read_bindings(root, approval_path)
        require(fresh['binding_sha256'] == value['binding_sha256'], 'bootstrap_inputs_changed')
        for name, raw in archive_rows(fresh).items():
            path = control / name
            upgrade_state._write_exclusive(path.parent, path.name, raw)
        upgrade_state._commit_event_locked(root, event_for(fresh))
        result = completed(root, upgrade_state.load_verified(root), fresh)
        require(result is not None, 'bootstrap_commit_unknown')
        return result


def main():
    import argparse
    import json
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=('plan', 'activate'))
    parser.add_argument('--root', required=True)
    parser.add_argument('--approval', required=True)
    args = parser.parse_args()
    try:
        value = read_bindings(args.root, args.approval)
        require(Path(__file__).resolve().parent == value['host'].bundle, 'executing_bundle_mismatch')
        if args.command == 'activate':
            result = activate(args.root, args.approval)
        else:
            loaded = upgrade_state.load_verified(Path(args.root))
            prior = completed(Path(args.root), loaded, value)
            control = Path(args.root) / 'state/ags-autodeploy/local-v2/control'
            require(prior is not None or (not control.exists() and not control.is_symlink()),
                    'bootstrap_state_not_empty')
            result = prior or {
                'schema': 'ags.routeb-native-bootstrap-plan.v1', 'status': 'admitted',
                'binding_sha256': value['binding_sha256'], 'effects': [],
                'native_pass': False, 'installable': False}
        print(json.dumps(result, sort_keys=True))
        return 0
    except Exception as error:
        import re
        code = getattr(error, 'code', None) or (str(error) if isinstance(error, ContractError) else None)
        print('code=' + (code if isinstance(code, str) and re.fullmatch('[a-z][a-z0-9_]{0,95}', code)
                        else 'native_bootstrap_refused'), file=sys.stderr)
        return 78


if __name__ == '__main__':
    raise SystemExit(main())
