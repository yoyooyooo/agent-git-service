#!/usr/bin/env python3
"""Internal host adapter, not a guaranteed entry.

Root-pinned local adapter. Host provisioning/policy activation is a separate gate.
The operator bundle is started by scripts/upgrade, which disables bytecode writes
before Python starts. Direct "python3 scripts/upgrade_host.py" is not covered.
"""
import sys
sys.dont_write_bytecode = True
sys.pycache_prefix = None
import argparse
import contextlib
import fcntl
import stat
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import socket
import sys
import tempfile
import urllib.request

from upgrade import BUNDLE, Failure, bundle_engine, execution_identity, private_json, validate_pin, directory_fd, read_private_fd, require_apply_contracts, result_exit
from upgrade_engine import Transaction, acceptance, authorize, child, dual_smoke, save
from upgrade_recovery import ColdWindow, read_original_binding, checkpoint as cold_checkpoint


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
    validate_pin(pin)
    if digest != args.pin_sha256:
        raise Failure('release_pin_invalid')
    bundle_digest = pin.get('verification_policy', {}).get('tool_bundle_manifest_sha256', '')
    bundle_engine(BUNDLE, bundle_digest)
    validate_pin(pin, source=json.loads((BUNDLE / 'release-source.json').read_bytes()))
    policy_raw = (BUNDLE / 'upgrade_policy.json').read_bytes()
    if hashlib.sha256(policy_raw).hexdigest() != pin['verification_policy']['policy_sha256']:
        raise Failure('release_pin_invalid')
    from upgrade_backup_contract import strict_json
    policy = strict_json(policy_raw)
    if policy.get('schema') != 'ags.upgrade-policy.v2':
        raise Failure('dependency_unavailable')
    entry = policy.get('deployments', {}).get(host)
    if not entry:
        if getattr(args, 'command', 'apply') == 'apply':
            require_apply_contracts(host)
        raise Failure('dependency_unavailable')
    if entry['root'] != str(root):
        raise Failure('invalid_host')
    if pin.get('repository') != 'yoyooyooo/agent-git-service' or pin.get('repository_id') != 1384519373:
        raise Failure('release_pin_invalid')
    pending = root / 'state/ags-autodeploy/local-v2/pending'
    original = read_original_binding(pending) if pending.exists() else None
    if original is None and getattr(args, 'command', 'apply') in ('status', 'reconcile', 'confirm'):
        # Completion archives pending. Locate only this exact request's original
        # claim; never reconstruct old from the now-new installation receipt.
        matches = []
        for archived in pending.parent.glob('accepted-*'):
            candidate = read_original_binding(archived)
            if all(candidate.get(k) == v for k, v in {
                    'host': host, 'deployment_id': args.deployment_id, 'pin_sha256': digest,
                    'attempt_id': args.ci_run_id + '.' + args.ci_run_attempt,
                    'authorization_id': args.authorization_id}.items()):
                matches.append(candidate)
        if len(matches) != 1:
            raise Failure('transaction_state_unknown')
        original = matches[0]
    if original is None:
        old_receipt, _ = private_json(root / 'state/installation.json')
    target = pin['runtime']
    binding = {'host': host, 'deployment_id': args.deployment_id,
               'attempt_id': args.ci_run_id + '.' + args.ci_run_attempt,
               'pin_sha256': digest, 'old': original['old'] if original else old_receipt['source'], 'new': target['revision'],
               'runtime_tree': target['tree'], 'release_id': pin['release_id'],
               'engine_sha256': bundle_digest, 'policy_sha256': hashlib.sha256((BUNDLE / 'upgrade_policy.json').read_bytes()).hexdigest(),
               'authorization_id': args.authorization_id}
    if original is not None:
        # The installed receipt may now be new. All other caller/pin fields must match.
        binding['old'] = original.get('old')
        if binding != original:
            raise Failure('claim_mismatch')
        binding = original
    return root, pin, policy, entry, binding


class Effects:
    def __init__(self, root, pin, policy, entry, binding, scratch, *,
                 package=None):
        self.root, self.pin, self.policy = root, pin, policy
        self.entry, self.binding, self.scratch = entry, binding, scratch
        self.package = package
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
        if getattr(self, 'package', None) is None:
            require_apply_contracts(self.binding.get('host'))
        self._authorize_stage()
        self._window().backup()

    def _window(self):
        if not hasattr(self, '_cold'):
            require_apply_contracts(self.binding.get('host'), package=getattr(self, 'package', None),
                                    binding=self.binding)
            raise Failure('minimum_contract_unsatisfied', 'cold_window', 83)
        return self._cold

    def begin(self):
        package = self.package
        self._authorize_stage()
        require_apply_contracts(self.binding['host'], package=package, binding=self.binding)
        self._cold = ColdWindow(self.root, self.binding, package.backup_binding, package.group,
                                package.dsn, package.schema_sha256, package.manager,
                                package.writer_probe, package.trash)
        # Transaction owns this same inode. Borrow its ownership, never flock
        # a second descriptor (which would self-conflict).
        self._cold.owner = self.owner_fd
        self._cold.begin()
    def stop(self): self._window().stop()
    def quiescent(self): self._window().quiescent()
    def restore(self): self._window().restore()
    def _authorize_stage(self):
        package = self.package
        current, _ = private_json(self.root / 'config' / ('upgrade-authorization-' + self.binding['authorization_id'] + '.json'))
        if current != package.authorization:
            raise Failure('minimum_contract_unsatisfied', 'authorization_changed', 83)
        try:
            package.validate(self.binding['host'], self.binding)
        except Failure as error:
            if error.exit_code == 77:
                raise Failure('minimum_contract_unsatisfied', 'authorization_expired', 83) from error
            raise

    def install_intent(self, authorize):
        self._authorize_stage()
        self._window().install_intent(authorize)
    def installed(self):
        self._window().installed(self.info['binaries']['gh-server']['identity'],
                                 lambda p: json.loads(child([str(p), '--version'], 'installed')))
    def start_intent(self):
        self._authorize_stage()
        self._window().start_intent()
    def release(self):
        if hasattr(self, '_cold'):
            self._cold.release()

    def install(self):
        """Share installer/runtime locks; all writes stay relative to opened FDs."""
        cold = self._window()
        if cold.locks is None or cold.journal['phase'] != 'install_started':
            raise Failure('backup_contract_unimplemented', 'install', 83)
        if getattr(self, 'package', None) is not None:
            self._authorize_stage()
        cold.unchanged(); cold.verified_backup()
        if (self.info['source'] != cold.backup_binding['new']
                or self.info['binaries']['gh-server']['sha256'] != cold.backup_binding['new_binary_sha256']):
            raise Failure('minimum_contract_unsatisfied', 'install', 83)
        try:
            with directory_fd(self.root, private=True) as root_fd, contextlib.ExitStack() as guards:
                dirs = {}
                for name in ('state', 'bin'):
                    fd = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=root_fd)
                    guards.callback(os.close, fd)
                    mode = os.fstat(fd)
                    if mode.st_uid != os.getuid() or mode.st_mode & 0o077:
                        raise Failure('host_transaction_failed', 'install')
                    dirs[name] = fd
                state_fd, bin_fd = dirs['state'], dirs['bin']
                fd = os.open('installation.json', os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=state_fd)
                try:
                    current, _ = read_private_fd(fd)
                finally:
                    os.close(fd)
                destination = os.stat('gh-server', dir_fd=bin_fd, follow_symlinks=False)
                if not stat.S_ISREG(destination.st_mode) or destination.st_uid != os.getuid():
                    raise Failure('host_transaction_failed', 'install')
                fd = os.open(self.candidate, os.O_RDONLY | os.O_NOFOLLOW)
                with os.fdopen(fd, 'rb') as candidate:
                    data = candidate.read()
                if hashlib.sha256(data).hexdigest() != self.info['binaries']['gh-server']['sha256']:
                    raise Failure('release_digest_mismatch', 'install', 65)

                def write_json(name, value):
                    staging = name + '.new'
                    fd = os.open(staging, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=state_fd)
                    with os.fdopen(fd, 'w') as stream:
                        json.dump(value, stream, sort_keys=True)
                        stream.write('\n'); stream.flush(); os.fsync(stream.fileno())
                    cold_checkpoint(name + ':file-sync')
                    os.replace(staging, name, src_dir_fd=state_fd, dst_dir_fd=state_fd)
                    cold_checkpoint(name + ':rename')
                    os.fsync(state_fd)
                    cold_checkpoint(name + ':directory-sync')

                fd = os.open('.gh-server.upgrading', os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o700, dir_fd=bin_fd)
                with os.fdopen(fd, 'wb') as stream:
                    stream.write(data); stream.flush(); os.fsync(stream.fileno())
                cold_checkpoint('candidate-file-sync')
                write_json('installing.json', self.info)
                cold_checkpoint('installing-durable')
                os.replace('.gh-server.upgrading', 'gh-server', src_dir_fd=bin_fd, dst_dir_fd=bin_fd)
                cold_checkpoint('binary-renamed')
                os.fsync(bin_fd)
                cold_checkpoint('binary-directory-sync')
                current.update(source=self.info['source'], runtime_tree=self.info['runtime_tree'],
                               control_revision=self.info['control_revision'], version=self.info['version'],
                               release_id=self.info['release_id'], attestation=self.info['attestation'])
                current['binaries']['gh-server'] = self.info['binaries']['gh-server']
                write_json('installation.json', current)
                cold_checkpoint('receipt-durable')
                os.unlink('installing.json', dir_fd=state_fd)
                os.fsync(state_fd)
                cold_checkpoint('install-marker-cleared')
        except OSError:
            raise Failure('host_transaction_failed', 'install') from None

    def start(self):
        self._window().start()

    def verify_waiting(self):
        from upgrade_manual_approval import load_chain, check_running
        from datetime import datetime, timezone
        chain = load_chain(self.root, self.binding)
        check_running(self.root, chain, lambda: acceptance_health(self.root, self.entry, self.binding),
                      datetime.now(timezone.utc))

    def health(self, expected=None):
        if expected is None and self.package is not None:
            from upgrade_manual_approval import check_running
            from upgrade_recovery import read_window
            from datetime import datetime, timezone
            marker, journal, _ = read_window(self.root)
            if journal.get('handoff', {}).get('consumed') is not True:
                raise Failure('host_transaction_failed', 'health')
            check_running(self.root, {'backup_binding': journal['backup_binding'],
                          'generation': marker['generation']},
                          lambda: acceptance_health(self.root, self.entry, self.binding),
                          datetime.now(timezone.utc))
            return
        expected = expected or self.binding['new']
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        try:
            with opener.open(self.entry['readyz'], timeout=15) as response:
                body = json.load(response)
            if body.get('status') != 'ready' or body.get('version') != expected:
                raise Failure('host_transaction_failed', 'health')
        except (OSError, ValueError):
            raise Failure('host_transaction_failed', 'health') from None


def acceptance_health(root, entry, binding):
    """Read-only runtime/process + local and service route health, no manager calls.

    The admitted private policy supplies two fixed URLs. Missing health is
    unknown, never completion; this does not discover or provision a host.
    """
    runtime, _ = private_json(root / 'state/runtime.json')
    startup, _ = private_json(root / 'state/startup-health.json')
    from upgrade_backup_contract import timestamp
    if (startup.get('schema') != 'ags.startup-health.v1' or startup.get('ready_to_start') is not True
            or not startup.get('checks') or any(value != 'ok' for value in startup['checks'].values())
            or timestamp(startup['checked_at']) > timestamp(runtime['started_at'])):
        raise Failure('host_transaction_failed', 'health')
    for key in ('pid', 'supervisor_pid'):
        pid = runtime.get(key)
        if type(pid) is not int or pid <= 0:
            raise Failure('host_transaction_failed', 'health')
        os.kill(pid, 0)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    result = {'generation': runtime.get('generation'), 'running_revision': runtime.get('build', {}).get('revision')}
    for key, config in (('local', 'readyz'), ('service', 'service_readyz')):
        url = entry.get(config)
        if not isinstance(url, str) or not url.startswith(('http://', 'https://')):
            raise Failure('host_transaction_failed', 'health')
        with opener.open(url, timeout=15) as response:
            body = json.load(response)
        result[key] = {'status': body.get('status'), 'revision': body.get('version')}
    after, _ = private_json(root / 'state/runtime.json')
    if after != runtime:
        raise Failure('host_transaction_failed', 'health')
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=['plan', 'apply', 'status', 'reconcile', 'confirm'])
    for name in ('deployment-id', 'pin-sha256', 'ci-run-id', 'ci-run-attempt', 'authorization-id'):
        parser.add_argument('--' + name, required=True)
    parser.add_argument('--approval-file', type=Path)
    parser.add_argument('--evidence-packet', type=Path)
    args = parser.parse_args()
    if args.command != 'confirm' and (args.approval_file or args.evidence_packet):
        raise Failure('invalid_request')
    root, pin, policy, entry, binding = context(args)
    state = root / 'state/ags-autodeploy/local-v2'
    if args.command == 'plan':
        print(json.dumps({'schema': 'ags.local-upgrade-request.v2', 'binding': binding,
                          'read_only': True, 'verification': 'not_run', 'authorized': False}))
        return 0
    if args.command in ('status', 'reconcile', 'confirm'):
        result = acceptance(args.command, root, binding, approval_path=args.approval_file,
                            packet_path=args.evidence_packet, pin=pin, pin_sha=args.pin_sha256,
                            probe=lambda: acceptance_health(root, entry, binding))
        print(json.dumps(result))
        return result_exit(result) if args.command == 'confirm' else 0
    authorization, _ = private_json(root / 'config' / ('upgrade-authorization-' + args.authorization_id + '.json'))
    authorize(authorization, binding)
    package = None
    if entry.get('host_package'):
        from upgrade_wireup import load_admission
        package = load_admission(root, pin, policy, entry, binding, authorization, BUNDLE)
    require_apply_contracts(binding['host'], package=package, binding=binding)
    with tempfile.TemporaryDirectory(prefix='ags-upgrade-apply-') as directory:
        effects = Effects(root, pin, policy, entry, binding, Path(directory), package=package)
        result = Transaction(state, binding, authorization, effects).run()
        print(json.dumps(result))
    return result_exit(result)


if __name__ == '__main__':
    try:
        sys.exit(main())
    except Failure as error:
        print(json.dumps({'schema': 'ags.upgrade-diagnostic.v2', 'status': 'failed',
                          'code': error.code, 'phase': error.phase, 'host': socket.gethostname().split('.')[0],
                          'deployment_id': None, 'attempt_id': None, 'journal_sha256': None,
                          'missing_contracts': getattr(error, 'missing_contracts', [])}))
        sys.exit(error.exit_code)
