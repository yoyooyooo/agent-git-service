#!/usr/bin/env python3
"""Internal local upgrade implementation, not a guaranteed entry.

plan is read-only; apply requires external authorization.
Deployment bundles are provisioned separately. This checkout never installs one.

Guaranteed invocation is scripts/upgrade. That launcher sets
PYTHONDONTWRITEBYTECODE and clears PYTHONPYCACHEPREFIX before the first Python
process, then execs python3 -B. Direct "python3 scripts/upgrade.py" is not
covered by the read-only guarantee.
"""
import sys
sys.dont_write_bytecode = True
sys.pycache_prefix = None
import argparse
from contextlib import contextmanager
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys

from upgrade_identity import IdentityError, failure_from, resolve_identity, require_same

BUNDLE = Path('/usr/local/libexec/ags-autodeploy')
SHA256 = re.compile(r'[0-9a-f]{64}')
ID = re.compile(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,95}')
EXIT_CODES = {'invalid_request': 64, 'invalid_host': 64, 'release_pin_invalid': 64,
              'dependency_unavailable': 69, 'wrong_execution_identity': 77,
              'not_approved': 77, 'authorization_invalid': 77, 'authorization_expired': 77,
              'migration_admission_rejected': 78, 'schema_not_additive': 78,
              'claim_held': 73, 'claim_mismatch': 73, 'lock_held': 73,
              'forward_only_recovery_required': 81, 'transaction_state_unknown': 82,
              'journal_invalid': 82, 'host_transaction_failed': 82,
              'backup_contract_unimplemented': 83, 'minimum_contract_unsatisfied': 83,
              'mini_handoff_contract_unimplemented': 84,
              'observation_finalize_unimplemented': 85, 'awaiting_observation': 86}


class Failure(Exception):
    def __init__(self, code, phase='admission', exit_code=None):
        super().__init__(code)
        self.code, self.phase = code, phase
        self.exit_code = exit_code or EXIT_CODES.get(code, 82)


@contextmanager
def directory_fd(path, *, private=False):
    """Walk every ancestor without following links; retain the opened directory."""
    path = Path(path)
    if not path.is_absolute() or '..' in path.parts:
        raise Failure('invalid_request')
    fd = os.open('/', os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        for part in path.parts[1:]:
            next_fd = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
            os.close(fd)
            fd = next_fd
            mode = os.fstat(fd)
            if mode.st_uid not in (0, os.getuid()) or mode.st_mode & 0o022:
                raise Failure('invalid_request')
        mode = os.fstat(fd)
        if private and (mode.st_uid != os.getuid() or mode.st_mode & 0o077):
            raise Failure('invalid_request')
        yield fd
    finally:
        os.close(fd)


def read_private_fd(fd, maximum=65536, *, owner=None):
    before = os.fstat(fd)
    if (not stat.S_ISREG(before.st_mode)
            or before.st_uid != (os.getuid() if owner is None else owner)
            or stat.S_IMODE(before.st_mode) != 0o600 or before.st_size > maximum):
        raise Failure('invalid_request')
    raw = os.read(fd, maximum + 1)
    after = os.fstat(fd)
    if len(raw) > maximum or (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns) != (
            after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns):
        raise Failure('invalid_request')
    def unique(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError('duplicate JSON field')
            result[key] = value
        return result
    return json.loads(raw, object_pairs_hook=unique), hashlib.sha256(raw).hexdigest()


def private_json(path, maximum=65536, *, owner=None):
    """Validate every directory and read the pinned file through its directory FD."""
    try:
        with directory_fd(path.parent) as parent:
            mode = os.fstat(parent)
            if mode.st_uid != (os.getuid() if owner is None else owner) or mode.st_mode & 0o077:
                raise Failure('invalid_request')
            fd = os.open(path.name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=parent)
            try:
                return read_private_fd(fd, maximum, owner=owner)
            finally:
                os.close(fd)
    except (OSError, ValueError, TypeError):
        raise Failure('invalid_request') from None


def validate_pin(pin, *, source=None):
    """Closed preparation schema. Validation does not enable a production mode."""
    import release_contract as contract
    from release_verify import WORKFLOW, PREDICATE_TYPE

    def fields(value, names):
        if type(value) is not dict or set(value) != set(names.split()):
            raise ValueError('fields')

    def positive(value):
        if type(value) is not int or value <= 0:
            raise ValueError('integer')

    def sha(value, width=40):
        if type(value) is not str or not re.fullmatch('[0-9a-f]{' + str(width) + '}', value):
            raise ValueError('digest')

    try:
        fields(pin, 'schema repository repository_id version release_id immutable draft allow_prerelease runtime toolchain targets ci verification_policy')
        if (pin['schema'] != contract.PIN_SCHEMA or pin['repository'] != contract.REPO
                or type(pin['repository_id']) is not int or pin['repository_id'] != contract.REPO_ID
                or type(pin['version']) is not str or not contract.VERSION.fullmatch(pin['version'])
                or pin['immutable'] is not True or pin['draft'] is not False
                or type(pin['allow_prerelease']) is not bool
                or ('-rc' in pin['version'] and not pin['allow_prerelease'])):
            raise ValueError('identity')
        positive(pin['release_id'])
        fields(pin['runtime'], 'revision tree')
        for value in pin['runtime'].values():
            sha(value)
        if source is not None and (source.get('schema') != 'ags.release-source.v1'
                or source.get('version') != pin['version'] or source.get('runtime') != pin['runtime']):
            raise ValueError('reviewed runtime')
        fields(pin['toolchain'], 'go_version pin_path')
        if pin['toolchain'] != {'go_version': 'go1.26.8', 'pin_path': '.go-version'}:
            raise ValueError('toolchain')
        fields(pin['targets'], 'darwin_arm64 linux_amd64')
        controls = set()
        for target, entry in pin['targets'].items():
            fields(entry, 'asset manifest_schema abi binaries provenance')
            if entry['manifest_schema'] != contract.MANIFEST_SCHEMA:
                raise ValueError('manifest')
            contract.validate_abi(entry['abi'], target)
            asset = entry['asset']
            fields(asset, 'id name size digest')
            positive(asset['id']); positive(asset['size'])
            if (asset['size'] > contract.MAX_ARCHIVE
                    or asset['name'] != 'ags-' + pin['version'] + '-' + target + '.tar.gz'
                    or type(asset['digest']) is not str or not re.fullmatch(r'sha256:[0-9a-f]{64}', asset['digest'])):
                raise ValueError('asset')
            fields(entry['binaries'], ' '.join(contract.COMMANDS))
            for command, binary in entry['binaries'].items():
                fields(binary, 'sha256 size cgo_enabled identity')
                sha(binary['sha256'], 64); positive(binary['size'])
                if binary['size'] > contract.MAX_MEMBER or binary['cgo_enabled'] is not (command == 'gh-server'):
                    raise ValueError('binary')
                identity = binary['identity']
                fields(identity, 'schema command version revision tree goos goarch go_version')
                os_name, arch = target.split('_')
                if identity != {'schema': 'ags.build.v1', 'command': command, 'version': pin['version'],
                                **pin['runtime'], 'goos': os_name, 'goarch': arch, 'go_version': 'go1.26.8'}:
                    raise ValueError('binary identity')
            provenance = entry['provenance']
            fields(provenance, 'mode subject_digest repository_id issuer signer_workflow signer_ref signer_digest workflow_run_id workflow_run_attempt control_revision control_tree runtime_revision runtime_tree predicate_digest predicate_type')
            for key in ('control_revision', 'control_tree', 'signer_digest', 'runtime_revision', 'runtime_tree'):
                sha(provenance[key])
            sha(provenance['predicate_digest'], 64)
            positive(provenance['workflow_run_id']); positive(provenance['workflow_run_attempt'])
            if (provenance['mode'] != 'separated-control-v1'
                    or provenance['subject_digest'] != asset['digest']
                    or type(provenance['repository_id']) is not int or provenance['repository_id'] != contract.REPO_ID
                    or provenance['issuer'] != 'https://token.actions.githubusercontent.com'
                    or provenance['signer_workflow'] != WORKFLOW
                    or provenance['signer_ref'] != 'refs/tags/' + pin['version']
                    or provenance['signer_digest'] != provenance['control_revision']
                    or provenance['control_revision'] == pin['runtime']['revision']
                    or provenance['runtime_revision'] != pin['runtime']['revision']
                    or provenance['runtime_tree'] != pin['runtime']['tree']
                    or provenance['predicate_type'] != PREDICATE_TYPE):
                raise ValueError('provenance')
            controls.add((provenance['control_revision'], provenance['control_tree']))
        if len(controls) != 1:
            raise ValueError('control set')
        fields(pin['ci'], 'revision fork_ci secret_scan')
        if pin['ci']['revision'] != pin['runtime']['revision']:
            raise ValueError('ci runtime')
        for key, workflow in [('fork_ci', '.github/workflows/ci.yml'), ('secret_scan', '.github/workflows/secret-scan.yml')]:
            row = pin['ci'][key]
            fields(row, 'workflow run_id run_attempt')
            positive(row['run_id']); positive(row['run_attempt'])
            if row['workflow'] != workflow:
                raise ValueError('ci workflow')
        policy = pin['verification_policy']
        fields(policy, 'schema policy_sha256 tool_bundle_manifest_sha256')
        if policy['schema'] != 'ags.upgrade-policy.v2':
            raise ValueError('policy')
        sha(policy['policy_sha256'], 64); sha(policy['tool_bundle_manifest_sha256'], 64)
    except (ValueError, TypeError, KeyError, AttributeError):
        raise Failure('release_pin_invalid') from None


def require_apply_contracts(host, *, policy=None, package=None, binding=None):
    """Capability gate only; future stage evidence is never demanded in advance."""
    from upgrade_wireup import Admission
    if type(package) is not Admission:
        missing = ['private_host_package', 'backup_git_config_coverage_and_git_db_consistency',
                   'independent_observation_approval_and_finalize']
        if host == 'imile-win':
            missing.append('trusted_mini_handoff_bound_to_pin_release_deployment')
        error = Failure('backup_contract_unimplemented')
        error.missing_contracts = missing
        raise error
    try:
        from upgrade_backup_contract import ContractError
        package.validate(host, binding)
    except ContractError as error:
        raise Failure('minimum_contract_unsatisfied', 'admission', 83) from error


def result_exit(result):
    if result.get('status') == 'completed':
        return 0
    if result.get('status') == 'awaiting_observation':
        return EXIT_CODES['awaiting_observation']
    return EXIT_CODES['transaction_state_unknown']


def execution_identity(host):
    """Compatibility API. The observed alias must match the trusted profile."""
    try:
        identity = resolve_identity()
        from upgrade_identity import alias
        if alias(host) not in identity.aliases:
            raise IdentityError('host_alias_unknown', 64)
        require_same(identity)
        return Path(identity.root)
    except IdentityError as error:
        raise failure_from(error) from None


def bundle_engine(bundle, expected_digest, *, owner=0):
    """No PATH/source fallback. Every bundle file is bound to the reviewed manifest."""
    try:
        if bundle.resolve() != bundle:
            raise Failure('dependency_unavailable')
        for path in (bundle, *bundle.parents):
            mode = path.stat()
            if mode.st_uid != owner or mode.st_mode & 0o022:
                raise Failure('dependency_unavailable')
        manifest_path = bundle / 'manifest.json'
        if manifest_path.is_symlink() or not manifest_path.is_file():
            raise Failure('dependency_unavailable')
        raw = manifest_path.read_bytes()
        if len(raw) > 65536 or hashlib.sha256(raw).hexdigest() != expected_digest:
            raise Failure('dependency_unavailable')
        manifest = json.loads(raw)
        if (set(manifest) != {'schema', 'entry', 'files'} or manifest['schema'] != 'ags.upgrade-tool-bundle.v2'
                or manifest['entry'] != 'upgrade_host.py' or not isinstance(manifest['files'], dict)
                or not {'upgrade_host.py', 'upgrade.py', 'upgrade_engine.py', 'upgrade_policy.json',
                        'release_verify.py', 'release_contract.py', 'release_install_v2.py',
                        'install-release.py', 'release-source.json', 'upgrade_smoke.py',
                        'upgrade_backup_contract.py', 'upgrade_inventory.py',
                        'upgrade_backup.py', 'upgrade_restore_verify.py', 'upgrade_recovery.py',
                        'upgrade_manual_approval.py', 'upgrade_wireup.py',
                        'upgrade_identity.py'} <= set(manifest['files'])):
            raise Failure('dependency_unavailable')
        for name, digest in manifest['files'].items():
            if not re.fullmatch(r'[A-Za-z0-9_.-]+', name) or not SHA256.fullmatch(digest):
                raise Failure('dependency_unavailable')
            path = bundle / name
            mode = path.lstat()
            if (not stat.S_ISREG(mode.st_mode) or mode.st_uid != owner or mode.st_mode & 0o022
                    or hashlib.sha256(path.read_bytes()).hexdigest() != digest):
                raise Failure('dependency_unavailable')
        return bundle / manifest['entry']
    except (OSError, ValueError, KeyError, TypeError):
        raise Failure('dependency_unavailable') from None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=['plan', 'apply', 'status', 'reconcile', 'confirm'])
    parser.add_argument('--deployment-id', required=True)
    parser.add_argument('--pin-sha256', required=True)
    parser.add_argument('--ci-run-id', required=True)
    parser.add_argument('--ci-run-attempt', required=True)
    parser.add_argument('--authorization-id', required=True)
    parser.add_argument('--approval-file', type=Path)
    parser.add_argument('--evidence-packet', type=Path)
    args = parser.parse_args()
    if args.command != 'confirm' and (args.approval_file or args.evidence_packet):
        raise Failure('invalid_request')
    if (not ID.fullmatch(args.deployment_id) or not ID.fullmatch(args.authorization_id)
            or not SHA256.fullmatch(args.pin_sha256)
            or not re.fullmatch(r'[1-9][0-9]*', args.ci_run_id)
            or not re.fullmatch(r'[1-9][0-9]*', args.ci_run_attempt)):
        raise Failure('invalid_request')
    try:
        identity = resolve_identity()
        require_same(identity)
    except IdentityError as error:
        raise failure_from(error) from None
    root = Path(identity.root)
    pin, digest = private_json(root / 'config/ags-deploy-release-pin.json')
    validate_pin(pin)
    if digest != args.pin_sha256:
        raise Failure('release_pin_invalid')
    expected = pin.get('verification_policy', {}).get('tool_bundle_manifest_sha256', '')
    if not SHA256.fullmatch(expected):
        raise Failure('release_pin_invalid')
    engine = bundle_engine(BUNDLE, expected)
    validate_pin(pin, source=json.loads((BUNDLE / 'release-source.json').read_bytes()))
    if hashlib.sha256((BUNDLE / 'host-profile.json').read_bytes()).hexdigest() != identity.profile_sha256:
        raise Failure('dependency_unavailable')
    try:
        require_same(identity)
    except IdentityError as error:
        raise failure_from(error) from None
    # The wrapper owns no lock/claim. The pinned engine is their only writer.
    command = [sys.executable, '-B', str(engine), *sys.argv[1:],
               '--identity-sha256', identity.sha256]
    try:
        result = subprocess.run(command, cwd=root, env={
            'HOME': str(root.parent), 'USER': 'yoyo', 'LOGNAME': 'yoyo',
            'PATH': '/usr/bin:/bin', 'PYTHONDONTWRITEBYTECODE': '1'})
    except OSError:
        raise Failure('dependency_unavailable') from None
    return result.returncode


if __name__ == '__main__':
    try:
        sys.exit(main())
    except Failure as error:
        print(json.dumps({'schema': 'ags.upgrade-diagnostic.v2', 'status': 'failed',
                          'code': error.code, 'phase': error.phase,
                          'host': None,
                          'deployment_id': None, 'attempt_id': None, 'journal_sha256': None}))
        sys.exit(error.exit_code)
