#!/usr/bin/env python3
"""Read-only package validation and fixed native target boundaries."""
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import routeb_host_package as package
from routeb_launcher import bundle_location
from upgrade_backup_contract import ContractError
from upgrade_backup_contract import canonical


class HostPackageBoundaryTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='routeb-package-test-', dir=Path.home())
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()

    def test_private_original_bytes_and_duplicate_keys(self):
        path = self.root / 'input.json'
        raw = b'{"a": 1}\n'
        path.write_bytes(raw); path.chmod(0o600)
        self.assertEqual(package.read_document(path)[1], raw)
        path.write_bytes(b'{"a":1,"a":2}')
        with self.assertRaises(ContractError):
            package.read_document(path)
        path.write_bytes(raw); path.chmod(0o644)
        with self.assertRaises(ContractError):
            package.read_document(path)

    def test_sqlite_limit_capability_is_required_before_a_cold_window(self):
        with patch.object(package.sqlite3, 'Connection', SimpleNamespace()):
            self.assertEqual(package.python_capability_blockers(), ['python_sqlite_limit_api_unavailable'])
        with patch.object(package.sqlite3, 'Connection', SimpleNamespace(setlimit=True, getlimit=True)):
            self.assertEqual(package.python_capability_blockers(), [])

    def profile(self):
        identity = SimpleNamespace(root=str(self.root), platform='Linux', uid=1000)
        host = SimpleNamespace(scope='isolated-native', identity=identity)
        spec = {'mode': 'isolated-native', 'manager': {'kind': 'systemd', 'scope': 'user',
                'target': 'ags-hostpkg-test-example.service', 'definition': str(self.root / 'test.service'),
                'definition_sha256': 'a' * 64},
                'runtime_path': str(self.root / 'state/runtime.json'),
                'installation_path': str(self.root / 'state/installation.json'),
                'binary_path': str(self.root / 'bin/gh-server'),
                'health': {'local': 'http://127.0.0.1:19666/readyz', 'service': 'http://127.0.0.1:19666/readyz'},
                'logs': [{'name': 'app', 'path': str(self.root / 'logs/primary.log')}],
                'duration_seconds': 600, 'lease_seconds': 0, 'interval_seconds': 20, 'max_gap_seconds': 30}
        return {'capture': spec}, host

    def test_native_profile_cannot_select_production_manager_or_other_paths(self):
        profile, host = self.profile()
        package.validate_capture_profile(profile, host)
        for key, value in (('target', 'agent-git-service.service'), ('scope', 'system'),
                           ('target', 'ags-hostpkg-test.example.service'), ('definition', '/etc/systemd/system/native.service')):
            original = profile['capture']['manager'][key]
            profile['capture']['manager'][key] = value
            with self.subTest(key=key, value=value), self.assertRaises(ContractError):
                package.validate_capture_profile(profile, host)
            profile['capture']['manager'][key] = original
        profile['capture']['runtime_path'] = '/fixture/other-root/state/runtime.json'
        with self.assertRaises(ContractError):
            package.validate_capture_profile(profile, host)

    def test_launcher_has_no_environment_selected_trust_path(self):
        home = Path('/fixture/home')
        self.assertEqual(bundle_location(home / '.ags', home), Path('/usr/local/libexec/ags-autodeploy'))
        self.assertEqual(bundle_location(home / '.local/state/ags-routeb-native/run1/ags', home),
                         Path('/usr/local/libexec/ags-routeb-native/run1/bundle'))
        with self.assertRaises(ValueError):
            bundle_location(Path('/tmp/ags'), home)

    def test_launch_bytes_are_rechecked_after_initial_admission(self):
        import hashlib
        bundle = self.root / 'bundle'; bundle.mkdir(mode=0o700)
        runtime = self.root / 'bin'; runtime.mkdir(mode=0o700)
        for path in (bundle / 'routeb_launcher.py', runtime / 'ags-runtime.py'):
            path.write_bytes(b'reviewed wrapper')
        interpreter = self.root / 'python'; interpreter.write_bytes(b'reviewed interpreter'); interpreter.chmod(0o700)
        host = SimpleNamespace(bundle=bundle, identity=SimpleNamespace(root=str(self.root)))
        profile = {'launcher_sha256': hashlib.sha256(b'reviewed wrapper').hexdigest(),
                   'interpreter': {'path': str(interpreter), 'sha256': hashlib.sha256(interpreter.read_bytes()).hexdigest()}}
        package.verify_launch_dependencies(profile, host)
        (runtime / 'ags-runtime.py').write_bytes(b'changed after admission')
        with self.assertRaisesRegex(ContractError, 'launcher_replaced'):
            package.verify_launch_dependencies(profile, host)
        (runtime / 'ags-runtime.py').write_bytes(b'reviewed wrapper')
        interpreter.write_bytes(b'changed after admission')
        with self.assertRaisesRegex(ContractError, 'interpreter_unverified'):
            package.verify_launch_dependencies(profile, host)

    def test_isolated_mutable_paths_cannot_escape_into_another_private_root(self):
        import copy
        native = self.root / 'native/ags'; native.mkdir(parents=True, mode=0o700)
        other = self.root / 'other/ags'; other.mkdir(parents=True, mode=0o700)
        host = SimpleNamespace(scope='isolated-native', identity=SimpleNamespace(root=str(native)))
        group = {'objects': [{'source': str(native / 'data/main.db'), 'restore_target': str(native / 'data/main.db')}],
                 'storage': {'backup_root': str(native.parent / 'recovery/backups'),
                             'restore_root': str(native.parent / 'recovery/restores')}}
        env = {'DB_DSN': 'sqlite:' + str(native / 'data/main.db'), 'GIT_REPO_DIR': str(native / 'data/git')}
        package.validate_native_paths(host, group=group, environment=env)
        for key in ('source', 'restore_target'):
            bad = copy.deepcopy(group); bad['objects'][0][key] = str(other / 'data/main.db')
            with self.subTest(key=key), self.assertRaisesRegex(ContractError, 'native_path_outside_transaction'):
                package.validate_native_paths(host, group=bad)
        for key in ('backup_root', 'restore_root'):
            bad = copy.deepcopy(group); bad['storage'][key] = str(other / key)
            with self.subTest(key=key), self.assertRaisesRegex(ContractError, 'native_storage_outside_transaction'):
                package.validate_native_paths(host, group=bad)
        for key, value in (('DB_DSN', 'sqlite:' + str(other / 'data/main.db')),
                           ('GIT_REPO_DIR', str(other / 'data/git')),
                           ('AGS_INTEGRATIONS_CONFIG', str(other / 'config/integrations.yaml'))):
            with self.subTest(key=key), self.assertRaisesRegex(ContractError, 'native_path_outside_transaction'):
                package.validate_native_paths(host, environment={**env, key: value})
        link = native / 'linked'; link.symlink_to(other, target_is_directory=True)
        with self.assertRaisesRegex(ContractError, 'native_path_outside_transaction'):
            package.validate_native_paths(host, environment={**env, 'GIT_REPO_DIR': str(link / 'data/git')})

    def test_external_authority_assets_stay_in_the_native_recovery_directory(self):
        native = self.root / 'native/ags'; native.mkdir(parents=True, mode=0o700)
        host = SimpleNamespace(scope='isolated-native', identity=SimpleNamespace(root=str(native)))
        item = {'source': '/usr/local/libexec/frozen/tool.py', 'restore_target': '/usr/local/libexec/frozen/tool.py',
                'immutable_authority': {'asset': str(native.parent / 'recovery/authority-assets/tool.py')}}
        group = {'objects': [item], 'storage': {'backup_root': str(native.parent / 'recovery/backups'),
                                               'restore_root': str(native.parent / 'recovery/restores')}}
        package.validate_native_paths(host, group=group)
        item['immutable_authority']['asset'] = str(self.root / 'other/tool.py')
        with self.assertRaisesRegex(ContractError, 'native_path_outside_transaction'):
            package.validate_native_paths(host, group=group)

    def test_every_bundle_member_and_fixed_identity_need_a_recoverable_authority(self):
        import hashlib
        import upgrade_bundle
        host = SimpleNamespace(bundle=Path('/usr/local/libexec/ags-routeb-native/run1/bundle'),
                               profile=Path('/usr/local/libexec/ags-routeb-native/run1/host-profile.json'),
                               identity=SimpleNamespace(host_instance='host-1'))
        raw = canonical({'schema': upgrade_bundle.SCHEMA, 'entry': 'upgrade_host.py',
                         'files': {name: 'a' * 64 for name in upgrade_bundle.REQUIRED}})
        sha = hashlib.sha256(raw).hexdigest()
        paths = {str(host.bundle / name): 'tool' for name in (*upgrade_bundle.REQUIRED, 'manifest.json')}
        paths[str(host.profile)] = 'policy'
        objects = [{'source': path, 'role': role, 'immutable_authority': {
                        'host_instance': 'host-1', 'tool_manifest': str(host.bundle / 'manifest.json'),
                        'tool_manifest_sha256': sha}} for path, role in paths.items()]
        with patch('upgrade_identity.read_trusted', return_value=raw):
            package.validate_authority_coverage({'objects': objects}, host, sha)
            with self.assertRaisesRegex(ContractError, 'coverage_incomplete'):
                package.validate_authority_coverage({'objects': objects[:-1]}, host, sha)
            changed = [{**item} for item in objects]
            changed[0].pop('immutable_authority')
            with self.assertRaisesRegex(ContractError, 'coverage_incomplete'):
                package.validate_authority_coverage({'objects': changed}, host, sha)


if __name__ == '__main__':
    unittest.main()
