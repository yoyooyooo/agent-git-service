#!/usr/bin/env python3
"""Fixed local upgrade entry. plan is read-only; apply requires external authorization.

Deployment bundles are provisioned separately. This checkout never installs one.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import pwd
import re
import socket
import stat
import subprocess
import sys

BUNDLE = Path('/usr/local/libexec/ags-autodeploy')
SHA256 = re.compile(r'[0-9a-f]{64}')
ID = re.compile(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,95}')
HOSTS = {'mini': 501, 'imile-win': 1000}
EXIT_CODES = {'invalid_request': 64, 'invalid_host': 64, 'release_pin_invalid': 64,
              'dependency_unavailable': 69, 'wrong_execution_identity': 77,
              'not_approved': 77, 'authorization_invalid': 77, 'authorization_expired': 77,
              'migration_admission_rejected': 78, 'schema_not_additive': 78,
              'claim_held': 73, 'claim_mismatch': 73, 'lock_held': 73,
              'forward_only_recovery_required': 81, 'transaction_state_unknown': 82,
              'journal_invalid': 82, 'host_transaction_failed': 82}


class Failure(Exception):
    def __init__(self, code, phase='admission', exit_code=None):
        super().__init__(code)
        self.code, self.phase = code, phase
        self.exit_code = exit_code or EXIT_CODES.get(code, 82)


def private_json(path, maximum=65536, *, owner=None):
    """Read through O_NOFOLLOW and validate the FD, not a prior path lookup."""
    try:
        if path.resolve() != path:
            raise Failure('invalid_request')
        parent = path.parent.stat()
        if parent.st_uid != (os.getuid() if owner is None else owner) or parent.st_mode & 0o077:
            raise Failure('invalid_request')
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
        with os.fdopen(fd, 'rb') as stream:
            before = os.fstat(stream.fileno())
            if (not stat.S_ISREG(before.st_mode) or before.st_uid != (os.getuid() if owner is None else owner)
                    or stat.S_IMODE(before.st_mode) != 0o600 or before.st_size > maximum):
                raise Failure('invalid_request')
            raw = stream.read(maximum + 1)
            after = os.fstat(stream.fileno())
            if len(raw) > maximum or (before.st_ino, before.st_size, before.st_mtime_ns) != (after.st_ino, after.st_size, after.st_mtime_ns):
                raise Failure('invalid_request')
        return json.loads(raw), hashlib.sha256(raw).hexdigest()
    except (OSError, ValueError, TypeError):
        raise Failure('invalid_request') from None


def execution_identity(host):
    if host not in HOSTS:
        raise Failure('invalid_host')
    uid = HOSTS[host]
    if os.getuid() != uid or os.geteuid() != uid or pwd.getpwuid(uid).pw_name != 'yoyo':
        raise Failure('wrong_execution_identity')
    return Path(pwd.getpwuid(uid).pw_dir) / '.ags'


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
                        'install-release.py', 'release-source.json', 'upgrade_smoke.py'} <= set(manifest['files'])):
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
    parser.add_argument('command', choices=['plan', 'apply', 'status'])
    parser.add_argument('--deployment-id', required=True)
    parser.add_argument('--pin-sha256', required=True)
    parser.add_argument('--ci-run-id', required=True)
    parser.add_argument('--ci-run-attempt', required=True)
    parser.add_argument('--authorization-id', required=True)
    args = parser.parse_args()
    if (not ID.fullmatch(args.deployment_id) or not ID.fullmatch(args.authorization_id)
            or not SHA256.fullmatch(args.pin_sha256)
            or not re.fullmatch(r'[1-9][0-9]*', args.ci_run_id)
            or not re.fullmatch(r'[1-9][0-9]*', args.ci_run_attempt)):
        raise Failure('invalid_request')
    host = socket.gethostname().split('.')[0]
    root = execution_identity(host)
    pin, digest = private_json(root / 'config/ags-deploy-release-pin.json')
    if digest != args.pin_sha256 or pin.get('schema') != 'ags.deploy-release-pin.v2':
        raise Failure('release_pin_invalid')
    expected = pin.get('verification_policy', {}).get('tool_bundle_manifest_sha256', '')
    if not SHA256.fullmatch(expected):
        raise Failure('release_pin_invalid')
    engine = bundle_engine(BUNDLE, expected)
    # The wrapper owns no lock/claim. The pinned engine is their only writer.
    command = [sys.executable, str(engine), *sys.argv[1:]]
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
                          'host': socket.gethostname().split('.')[0],
                          'deployment_id': None, 'attempt_id': None, 'journal_sha256': None}))
        sys.exit(error.exit_code)
