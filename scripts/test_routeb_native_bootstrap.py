#!/usr/bin/env python3
"""Native bootstrap admission with root-reader fixtures and a real private journal.

No manager or production root is accessed. Root provisioning and --version
are injected, so these are component results, never native lifecycle evidence.
"""
from datetime import datetime, timedelta, timezone
import hashlib
import os
from pathlib import Path
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import routeb_native_bootstrap as bootstrap
from upgrade_backup_contract import ContractError, canonical, fingerprint
import upgrade_state


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


class NativeBootstrapTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='native-bootstrap-', dir=Path.home())
        self.addCleanup(self.tmp.cleanup)
        base = Path(self.tmp.name).resolve()
        self.root = base / 'run-test/ags'
        for path in (self.root / 'state', self.root / 'bin', self.root / 'config', self.root.parent / 'package',
                     self.root.parent / 'registry/bundle'):
            path.mkdir(mode=0o700, parents=True)
        self.bundle = self.root.parent / 'registry/bundle'
        self.host = SimpleNamespace(scope='isolated-native', bundle=self.bundle,
                    profile=self.bundle.parent / 'host-profile.json', recheck=lambda: None,
                    identity=SimpleNamespace(root=str(self.root), host_instance='native-host-1', logical_host='imile-win',
                        uid=os.getuid(), gid=os.getgid(), profile_sha256=sha(b'root identity fixture'), sha256='2' * 64))
        wrapper = b'reviewed wrapper'
        (self.bundle / 'routeb_launcher.py').write_bytes(wrapper)
        (self.root / 'bin/ags-runtime.py').write_bytes(wrapper)
        binary = b'reviewed exact-source binary'
        (self.root / 'bin/gh-server').write_bytes(binary)
        (self.root.parent / 'package/gh-server').write_bytes(binary)
        self.version = {'schema': 'ags.build.v1', 'command': 'gh-server', 'version': 'source-' + 'a' * 40,
                        'revision': 'a' * 40, 'tree': 'b' * 40, 'goos': 'linux', 'goarch': 'amd64',
                        'go_version': 'go1.26.8'}
        manifest = {'schema': 'ags.routeb-candidate-manifest.v1', 'host': 'imile-win', 'platform': 'linux/amd64',
                    'binary': 'gh-server', 'sha256': sha(binary), 'built_revision': 'a' * 40, 'built_tree': 'b' * 40,
                    'baseline_revision': 'a' * 40, 'baseline_tree_documented': 'b' * 40, 'version': self.version,
                    'toolchain_requested': 'go1.26.8', 'host_go_version': 'go1.26.8', 'cgo_requested': '1',
                    'go_build_settings': {}, 'not_a_release': True, 'not_installed': True,
                    'started_against_ags_home': False}
        self.manifest_raw = canonical(manifest)
        self.write(self.root.parent / 'package/candidate-manifest.json', self.manifest_raw)
        installed = {'schema': 'ags.installation.v2', 'artifact_kind': 'exact-source', 'source': 'a' * 40,
                    'runtime_tree': 'b' * 40, 'candidate_manifest_sha256': sha(self.manifest_raw),
                    'tool_sha256': '3' * 64,
                    'binaries': {'gh-server': {'sha256': sha(binary), 'identity': self.version}}}
        installed_raw = canonical(installed)
        self.write(self.root / 'state/installation.json', installed_raw)
        self.environment = {'DB_DSN': 'sqlite:' + str(self.root / 'data/main.db'),
                            'GIT_REPO_DIR': str(self.root / 'data/git')}
        environment_raw = ('DB_DSN=' + self.environment['DB_DSN'] + '\nGIT_REPO_DIR=' + self.environment['GIT_REPO_DIR'] + '\n').encode()
        self.write(self.root / 'config/runtime.env', environment_raw)
        now = datetime.now(timezone.utc)
        self.window = {'issued_at': (now-timedelta(minutes=1)).isoformat(),
                       'expires_at': (now+timedelta(minutes=5)).isoformat()}
        interpreter = Path(sys.executable).resolve()
        self.provision = {'schema': bootstrap.PROVISION, 'transaction_id': 'run-test', 'host_instance': 'native-host-1',
            'host': 'imile-win', 'root': str(self.root), 'uid': os.getuid(), 'gid': os.getgid(),
            'root_device': self.root.stat().st_dev, 'profile_sha256': self.host.identity.profile_sha256, 'tool_sha256': '3' * 64,
            'tool_revision': 'c' * 40, 'tool_tree': 'd' * 40, 'candidate_manifest_sha256': sha(self.manifest_raw),
            'installation_sha256': sha(installed_raw), 'launcher_sha256': sha(wrapper),
            'runtime_env_sha256': sha(environment_raw),
            'interpreter': {'path': str(interpreter), 'sha256': sha(interpreter.read_bytes())},
            'provisioned_at': now.isoformat(), 'provision_authorization_id': 'provision-1',
            'provision_approval_sha256': '5' * 64}
        self.trusted = {'host-profile.json': b'root identity fixture',
                        'manifest.json': canonical({'files': {}})}
        self.approval_path = base / 'bootstrap-approval.json'
        self.refresh_receipts()
        for target, kwargs in (
            ('resolve_for_root', {'return_value': self.host}),
            ('trusted_document', {'side_effect': lambda path, **_kwargs: self.trusted[Path(path).name]}),
            ('_runtime_environment', {'side_effect': lambda *_args: dict(self.environment)}),
            ('bundle_engine', {'return_value': self.bundle / 'upgrade_host.py'})):
            guard = patch.object(bootstrap, target, **kwargs); guard.start(); self.addCleanup(guard.stop)
        guard = patch.object(bootstrap.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0, canonical(self.version), b''))
        self.version_runner = guard.start(); self.addCleanup(guard.stop)

    def write(self, path, raw):
        path.write_bytes(raw); path.chmod(0o600)

    def refresh_receipts(self):
        binding = {key: value for key, value in self.provision.items()
                   if key not in ('schema', 'provisioned_at', 'provision_approval_sha256')}
        provision_approval = {'schema': 'ags.routeb-native-provision-approval.v1', 'authorization_id': 'provision-1',
            'actor': 'human:component-fixture', 'purpose': 'native-provision', 'decision': 'approve',
            **self.window, 'binding_sha256': fingerprint(binding)}
        self.trusted['provision-approval.json'] = canonical(provision_approval)
        self.provision['provision_approval_sha256'] = sha(self.trusted['provision-approval.json'])
        self.trusted['provision.json'] = canonical(self.provision)
        self.approval = {'schema': bootstrap.APPROVAL, 'authorization_id': 'bootstrap-1',
            'actor': 'human:component-fixture', 'purpose': 'native-bootstrap', 'decision': 'approve', **self.window,
            'root': str(self.root), 'host_instance': 'native-host-1', 'transaction_id': 'run-test',
            'provision_sha256': sha(self.trusted['provision.json']), 'identity_sha256': '2' * 64, 'epoch': 1}
        self.write(self.approval_path, canonical(self.approval))

    def test_epoch_one_is_bound_to_original_evidence_and_retry_is_read_only(self):
        value = bootstrap.activate(self.root, self.approval_path)
        self.assertEqual(value['status'], 'active'); self.assertFalse(value['native_pass'])
        loaded = upgrade_state.load_verified(self.root)
        self.assertEqual(len(loaded['events']), 1)
        self.assertEqual(loaded['events'][0]['before_phase'], 'native_provisioned')
        self.assertEqual(loaded['events'][0]['evidence_sha256'], sha(self.trusted['provision.json']))
        before = {str(p): (p.stat().st_mtime_ns, sha(p.read_bytes())) for p in (self.root / 'state').rglob('*') if p.is_file()}
        with patch.object(upgrade_state, '_write_exclusive', side_effect=AssertionError('retry wrote')):
            self.assertEqual(bootstrap.activate(self.root, self.approval_path), value)
        after = {str(p): (p.stat().st_mtime_ns, sha(p.read_bytes())) for p in (self.root / 'state').rglob('*') if p.is_file()}
        self.assertEqual(before, after)

    def test_production_and_pending_roots_refuse_without_a_control_journal(self):
        self.host.scope = 'production'
        with self.assertRaisesRegex(ContractError, 'native_bootstrap_only'):
            bootstrap.activate(self.root, self.approval_path)
        self.host.scope = 'isolated-native'
        self.write(self.root / 'state/upgrade-cold.json', b'{}')
        with self.assertRaisesRegex(ContractError, 'bootstrap_recovery_pending'):
            bootstrap.activate(self.root, self.approval_path)
        self.assertFalse((self.root / 'state/ags-autodeploy/local-v2/control').exists())

    def test_incapable_python_refuses_before_reading_or_writing_provision(self):
        with patch.object(bootstrap, 'python_capability_blockers', return_value=['python_sqlite_limit_api_unavailable']), \
                patch.object(bootstrap, 'resolve_for_root', side_effect=AssertionError('must not read a root')):
            with self.assertRaisesRegex(ContractError, 'python_sqlite_limit_api_unavailable'):
                bootstrap.activate(self.root, self.approval_path)
        self.assertFalse((self.root / 'state/ags-autodeploy/local-v2/control').exists())

    def test_changed_approval_or_installed_binary_cannot_activate(self):
        self.approval['epoch'] = 2; self.write(self.approval_path, canonical(self.approval))
        with self.assertRaisesRegex(ContractError, 'bootstrap_approval_binding_mismatch'):
            bootstrap.activate(self.root, self.approval_path)
        self.refresh_receipts()
        (self.root / 'bin/gh-server').write_bytes(b'changed')
        with self.assertRaisesRegex(ContractError, 'bootstrap_candidate_changed'):
            bootstrap.activate(self.root, self.approval_path)
        self.assertFalse((self.root / 'state/ags-autodeploy/local-v2/control').exists())

    def test_new_approval_does_not_rebind_an_existing_authority(self):
        bootstrap.activate(self.root, self.approval_path)
        self.approval['authorization_id'] = 'bootstrap-2'; self.write(self.approval_path, canonical(self.approval))
        with self.assertRaisesRegex(ContractError, 'bootstrap_already_bound'):
            bootstrap.activate(self.root, self.approval_path)
        self.assertEqual(len(upgrade_state.load_verified(self.root)['events']), 1)

    def test_partial_commit_stays_fenced_and_never_synthesizes_acceptance(self):
        with patch.object(upgrade_state, '_commit_event_locked', side_effect=OSError('injected interruption')):
            with self.assertRaises(OSError):
                bootstrap.activate(self.root, self.approval_path)
        with self.assertRaisesRegex(ContractError, 'bootstrap_state_not_empty'):
            bootstrap.activate(self.root, self.approval_path)
        self.assertEqual(upgrade_state.load_verified(self.root)['events'], [])

    def test_changed_root_device_and_unapproved_provision_time_refuse(self):
        self.provision['root_device'] += 1; self.refresh_receipts()
        with self.assertRaisesRegex(ContractError, 'bootstrap_provision_binding_mismatch'):
            bootstrap.activate(self.root, self.approval_path)
        self.provision['root_device'] -= 1
        self.provision['provisioned_at'] = '2000-01-01T00:00:00+00:00'; self.refresh_receipts()
        with self.assertRaisesRegex(ContractError, 'bootstrap_approval_expired'):
            bootstrap.activate(self.root, self.approval_path)

    def test_an_isolated_bootstrap_cannot_select_another_roots_database(self):
        self.environment['DB_DSN'] = 'sqlite:' + str(self.root.parent.parent / 'other/main.db')
        with self.assertRaisesRegex(ContractError, 'native_path_outside_transaction'):
            bootstrap.activate(self.root, self.approval_path)
        self.assertFalse((self.root / 'state/ags-autodeploy/local-v2/control').exists())


if __name__ == '__main__':
    unittest.main()
