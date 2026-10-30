#!/usr/bin/env python3
"""Root-pinned local adapter. Host provisioning/policy activation is a separate gate."""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import socket
import sys
import tempfile
import urllib.request

from upgrade import BUNDLE, Failure, bundle_engine, execution_identity, private_json
from upgrade_engine import Transaction, authorize, child, dual_smoke, save
import sqlite3


CI_JOBS = {
    '.github/workflows/ci.yml': {
        'Source and governance', 'Root tests (service)', 'Root tests (rest)',
        'Root tests (server-graphql)', 'Root tests (db-router-git)',
        'Root tests (edge)', 'Root tests (fork-integrations)', 'Root tests (misc)',
        'Edge and authority race checks', 'Vendored client compatibility',
        'Official gh and CI backend acceptance', 'Real Forgejo protected-branch projection'},
    '.github/workflows/secret-scan.yml': {'secrets'},
}


def exact_ci(installer, revision):
    rows = installer.api('repos/' + installer.REPO + '/actions/runs?head_sha=' + revision + '&per_page=100')
    if rows.get('total_count', 101) > 100:
        raise Failure('release_artifact_unverified', 'source_verify', 65)
    evidence = []
    for path, expected in CI_JOBS.items():
        choices = [row for row in rows['workflow_runs'] if row.get('path') == path
                   and row.get('head_sha') == revision and row.get('repository', {}).get('id') == installer.REPO_ID
                   and row.get('head_repository', {}).get('id') == installer.REPO_ID
                   and row.get('event') in ('push', 'pull_request', 'workflow_dispatch')]
        if not choices:
            raise Failure('release_artifact_unverified', 'source_verify', 65)
        chosen = max(choices, key=lambda row: row['id'])
        current = installer.api('repos/' + installer.REPO + '/actions/runs/' + str(chosen['id']))
        if (current.get('head_sha') != revision or current.get('status') != 'completed'
                or current.get('conclusion') != 'success' or current.get('path') != path
                or type(current.get('run_attempt')) is not int):
            raise Failure('release_artifact_unverified', 'source_verify', 65)
        jobs = installer.api('repos/' + installer.REPO + '/actions/runs/' + str(current['id'])
                             + '/attempts/' + str(current['run_attempt']) + '/jobs?per_page=100')
        actual = jobs.get('jobs', [])
        if (jobs.get('total_count') != len(actual) or len(actual) != len(expected)
                or {j['name'] for j in actual} != expected
                or any(j.get('conclusion') != 'success' or j.get('run_attempt') != current['run_attempt'] for j in actual)):
            raise Failure('release_artifact_unverified', 'source_verify', 65)
        evidence.append({'run_id': current['id'], 'run_attempt': current['run_attempt']})
    return evidence


def load_installer():
    spec = importlib.util.spec_from_file_location('ags_pinned_installer', BUNDLE / 'install-release.py')
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


def context(args):
    host = socket.gethostname().split('.')[0]
    root = execution_identity(host)
    pin, digest = private_json(root / 'config/ags-deploy-release-pin.json')
    if digest != args.pin_sha256 or pin.get('schema') != 'ags.deploy-release-pin.v2':
        raise Failure('release_pin_invalid')
    bundle_digest = pin.get('verification_policy', {}).get('tool_bundle_manifest_sha256', '')
    bundle_engine(BUNDLE, bundle_digest)
    policy = json.loads((BUNDLE / 'upgrade_policy.json').read_bytes())
    if policy.get('schema') != 'ags.upgrade-policy.v2':
        raise Failure('dependency_unavailable')
    entry = policy.get('deployments', {}).get(host)
    if not entry:
        raise Failure('dependency_unavailable')
    if entry['root'] != str(root):
        raise Failure('invalid_host')
    if pin.get('repository') != 'yoyooyooo/agent-git-service' or pin.get('repository_id') != 1384519373:
        raise Failure('release_pin_invalid')
    old_receipt, _ = private_json(root / 'state/installation.json')
    target = pin['runtime']
    binding = {'host': host, 'deployment_id': args.deployment_id,
               'attempt_id': args.ci_run_id + '.' + args.ci_run_attempt,
               'pin_sha256': digest, 'old': old_receipt['source'], 'new': target['revision'],
               'runtime_tree': target['tree'], 'release_id': pin['release_id'],
               'engine_sha256': bundle_digest, 'policy_sha256': hashlib.sha256((BUNDLE / 'upgrade_policy.json').read_bytes()).hexdigest(),
               'authorization_id': args.authorization_id}
    return root, pin, policy, entry, binding


class Effects:
    def __init__(self, root, pin, policy, entry, binding, scratch):
        self.root, self.pin, self.policy = root, pin, policy
        self.entry, self.binding, self.scratch = entry, binding, scratch
        self.installer = load_installer()

    def preflight(self):
        if (self.root / 'state/ags-autodeploy/pending').exists() or (self.root / 'state/installing.json').exists():
            raise Failure('claim_held')
        # Latest applicable full CI, never an older success over a newer failure.
        for revision in (self.pin['runtime']['revision'], self.pin['targets'][self.installer.target()]['provenance']['control_revision']):
            evidence = exact_ci(self.installer, revision)
            if revision == self.binding['new'] and evidence[0] != {
                    'run_id': int(self.binding['attempt_id'].split('.')[0]),
                    'run_attempt': int(self.binding['attempt_id'].split('.')[1])}:
                raise Failure('release_artifact_unverified', 'source_verify', 65)
        version = self.pin['version']
        output = child([sys.executable, str(BUNDLE / 'install-release.py'), '--version', version,
                        '--allow-prerelease', '--verify-only'], 'artifact_verify')
        verified = json.loads(output)
        target = self.installer.target()
        expected = self.pin['targets'][target]
        if (verified['source'] != self.binding['new'] or verified['runtime_tree'] != self.pin['runtime']['tree']
                or verified['release_id'] != self.pin['release_id']
                or 'sha256:' + verified['asset_sha256'] != expected['asset']['digest']
                or verified['control_revision'] != expected['provenance']['control_revision']
                or verified['binaries'] != expected['binaries']):
            raise Failure('release_pin_invalid', 'artifact_verify')
        self.info = verified
        archive_name = 'ags-' + version + '-' + target + '.tar.gz'
        self.installer.execute(['gh', 'release', 'download', version, '--repo', self.installer.REPO,
                                '--pattern', archive_name, '--dir', str(self.scratch)])
        archive = self.scratch / archive_name
        if self.installer.digest(archive) != verified['asset_sha256']:
            raise Failure('release_digest_mismatch', 'artifact_verify', 65)
        if not self.installer.adjacent_module('release_install_v2').host_abi(target):
            raise Failure('release_abi_incompatible', 'candidate_identity', 65)
        info = self.installer.inspect_bundle(archive, version, self.binding['new'], target)
        if info['binaries'] != verified['binaries']:
            raise Failure('release_identity_mismatch', 'artifact_verify', 65)
        staged = self.scratch / 'verified'; staged.mkdir(mode=0o700)
        self.installer.extract_bundle(archive, staged)
        self.candidate = staged / 'bin/gh-server'
        identity = json.loads(child([str(self.candidate), '--version'], 'candidate_identity'))
        if identity != verified['binaries']['gh-server']['identity']:
            raise Failure('release_identity_mismatch', 'candidate_identity', 65)
        old_binary = self.root / 'bin/gh-server'
        previous, _ = private_json(self.root / 'state/installation.json')
        if (old_binary.is_symlink() or self.installer.digest(old_binary)
                != previous['binaries']['gh-server']['sha256']):
            raise Failure('release_identity_mismatch', 'old_smoke', 65)
        old = json.loads(child([str(old_binary), '--version'], 'old_smoke'))
        if old.get('revision') != self.binding['old']:
            raise Failure('release_identity_mismatch', 'old_smoke', 65)
        # Linux production-mode listeners require a namespace wrapper provisioned
        # in reviewed policy. The default policy admits no host.
        self.migrations = dual_smoke(old_binary, self.candidate, self.binding['old'],
                                     self.binding['new'], self.policy['migration_admissions'])
        child(self.entry['legacy_check'], 'legacy_scan')
        self.health(expected=self.binding['old'])

    def backup(self):
        child(self.entry['backup'], 'prepare', effects=True)
        receipt_path = Path(self.entry['backup_receipt'])
        receipt, _ = private_json(receipt_path)
        if (receipt.get('schema') != 'ags.local-backup.v2' or receipt.get('binding') != self.binding
                or receipt.get('consistent') is not True or not receipt.get('files')):
            raise Failure('host_transaction_failed', 'prepare')
        backup_root = receipt_path.parent
        seen = set()
        for item in receipt['files']:
            relative = Path(item['path'])
            if relative.is_absolute() or '..' in relative.parts or str(relative) in seen:
                raise Failure('host_transaction_failed', 'prepare')
            seen.add(str(relative))
            path = backup_root / relative
            if (path.resolve() != path or path.is_symlink() or not path.is_file()
                    or path.stat().st_uid != os.getuid() or path.stat().st_mode & 0o077
                    or path.stat().st_size != item['size']
                    or self.installer.digest(path) != item['sha256']):
                raise Failure('host_transaction_failed', 'prepare')
        database = backup_root / receipt['database']
        if receipt['database'] not in seen:
            raise Failure('host_transaction_failed', 'prepare')
        with sqlite3.connect(database.as_uri() + '?mode=ro', uri=True) as db:
            if (db.execute('PRAGMA integrity_check').fetchall() != [('ok',)]
                    or db.execute('PRAGMA foreign_key_check').fetchall()):
                raise Failure('host_transaction_failed', 'prepare')

    def stop(self):
        child(self.entry['stop'], 'stop', effects=True)

    def install(self):
        # Only the primary binary is in the deployment scope.
        destination = self.root / 'bin/gh-server'
        if destination.is_symlink():
            raise Failure('host_transaction_failed', 'install')
        with self.candidate.open('rb') as stream:
            os.fsync(stream.fileno())
        # Stage on the destination filesystem; scratch may be another mount.
        temporary = destination.with_name('.gh-server.upgrading')
        fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o700)
        with os.fdopen(fd, 'wb') as stream:
            stream.write(self.candidate.read_bytes()); stream.flush(); os.fsync(stream.fileno())
        save(self.root / 'state/installing.json', self.info)
        os.replace(temporary, destination)
        fd = os.open(destination.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)
        current, _ = private_json(self.root / 'state/installation.json')
        current.update(source=self.info['source'], runtime_tree=self.info['runtime_tree'],
                       control_revision=self.info['control_revision'], version=self.info['version'],
                       release_id=self.info['release_id'], attestation=self.info['attestation'])
        current['binaries']['gh-server'] = self.info['binaries']['gh-server']
        save(self.root / 'state/installation.json', current)
        (self.root / 'state/installing.json').unlink()
        fd = os.open(self.root / 'state', os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)

    def start(self):
        child(self.entry['start'], 'start', effects=True)

    def health(self, expected=None):
        expected = expected or self.binding['new']
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        try:
            with opener.open(self.entry['readyz'], timeout=15) as response:
                body = json.load(response)
            if body.get('status') != 'ready' or body.get('version') != expected:
                raise Failure('host_transaction_failed', 'health')
        except (OSError, ValueError):
            raise Failure('host_transaction_failed', 'health') from None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=['plan', 'apply', 'status'])
    for name in ('deployment-id', 'pin-sha256', 'ci-run-id', 'ci-run-attempt', 'authorization-id'):
        parser.add_argument('--' + name, required=True)
    args = parser.parse_args()
    root, pin, policy, entry, binding = context(args)
    state = root / 'state/ags-autodeploy/local-v2'
    if args.command == 'plan':
        print(json.dumps({'schema': 'ags.local-upgrade-request.v2', 'binding': binding,
                          'read_only': True, 'verification': 'not_run', 'authorized': False}))
        return 0
    if args.command == 'status':
        if (state / 'pending/receipt.json').exists():
            result, _ = private_json(state / 'pending/receipt.json')
            print(json.dumps(result))
        else:
            print(json.dumps({'status': 'unknown', 'binding': binding}))
        return 0
    authorization, _ = private_json(root / 'config' / ('upgrade-authorization-' + args.authorization_id + '.json'))
    authorize(authorization, binding)
    # imile additionally requires a locally provisioned trusted handoff verifier;
    # a digest of arbitrary JSON does not establish mini origin.
    if binding['host'] == 'imile-win':
        child(entry['mini_handoff_verify'], 'admission')
    with tempfile.TemporaryDirectory(prefix='ags-upgrade-apply-') as directory:
        effects = Effects(root, pin, policy, entry, binding, Path(directory))
        result = Transaction(state, binding, authorization, effects).run()
        print(json.dumps(result))
    return 0


if __name__ == '__main__':
    try:
        sys.exit(main())
    except Failure as error:
        print(json.dumps({'schema': 'ags.upgrade-diagnostic.v2', 'status': 'failed',
                          'code': error.code, 'phase': error.phase, 'host': socket.gethostname().split('.')[0],
                          'deployment_id': None, 'attempt_id': None, 'journal_sha256': None}))
        sys.exit(error.exit_code)
