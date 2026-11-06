#!/usr/bin/env python3
"""Read-only package validation and fixed native target boundaries."""
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

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

    def test_executable_and_sqlite_capability_cannot_admit_interpreter_closure(self):
        value = package.HostPackage.__new__(package.HostPackage)
        value.group = {'objects': []}
        value.profile = {'schema': package.NATIVE_PROFILE_SCHEMA}
        for scope in ('isolated-native', 'production'):
            value.host = SimpleNamespace(scope=scope, identity=SimpleNamespace(root=str(self.root)))
            with self.subTest(scope=scope), patch.object(package.sqlite3, 'Connection',
                    SimpleNamespace(setlimit=True, getlimit=True)):
                blockers = value.execution_blockers()
                self.assertIn('interpreter_full_closure_required', blockers)
                if scope == 'production':
                    self.assertIn('native_full_closure_required', blockers)

    def test_linux_admission_refusals_keep_the_public_blocker_and_private_cause(self):
        """Real client checks run on every source-CI platform; no peer is mocked."""
        import routeb_stage0_client as client
        for isolated, reason in ((0, 'stage0_isolated_interpreter_required'),
                                 (1, 'stage0_fixed_bundle_required')):
            flags = SimpleNamespace(isolated=isolated, ignore_environment=1,
                                    no_site=1, no_user_site=1)
            with self.subTest(isolated=isolated), \
                    patch.object(client.sys, 'platform', 'linux'), \
                    patch.object(client.sys, 'flags', flags), \
                    patch.object(client.sys, 'dont_write_bytecode', True), \
                    patch.multiple(client.os, getuid=lambda: 1000, geteuid=lambda: 1000,
                                   getgid=lambda: 1000, getegid=lambda: 1000):
                with self.assertRaisesRegex(ContractError, '^' + reason + '$'):
                    client.context(self.root, operations=('serve',))
                with self.assertRaisesRegex(ContractError, '^interpreter_full_closure_required$') as raised:
                    package.require_interpreter_closure(self.root, operations=('serve',))
                self.assertIsInstance(raised.exception.__cause__, ContractError)
                self.assertEqual(str(raised.exception.__cause__), reason)

    def test_public_gate_does_not_relabel_programming_errors_as_missing_authority(self):
        import routeb_stage0_client as client
        error = AttributeError('invalid fixture shape')
        with patch.object(client, 'context', side_effect=error):
            with self.assertRaises(AttributeError) as raised:
                package.require_interpreter_closure(self.root, operations=('serve',))
        self.assertIs(raised.exception, error)

    def test_native_bootstrap_refuses_before_provision_reads_or_journal_lock(self):
        import routeb_native_bootstrap as bootstrap
        with patch.object(bootstrap, 'read_bindings', side_effect=AssertionError('provision read')) as read, \
                patch.object(bootstrap.upgrade_state, 'StateLock') as lock:
            with self.assertRaisesRegex(ContractError, '^interpreter_full_closure_required$'):
                bootstrap.activate(self.root, self.root / 'approval.json')
        read.assert_not_called()
        lock.assert_not_called()

    def test_host_run_refuses_before_preflight_claim_or_native_window(self):
        import routeb_host_execution as execution
        operation = execution.HostExecution.__new__(execution.HostExecution)
        operation.root = self.root
        operation.package = Mock()
        operation.window = Mock()
        operation.claim = Mock()
        with patch.object(execution, 'preflight', side_effect=AssertionError('preflight')) as preflight:
            with self.assertRaisesRegex(ContractError, '^interpreter_full_closure_required$'):
                operation.run()
        preflight.assert_not_called()
        operation.claim.assert_not_called()
        self.assertEqual(operation.window.mock_calls, [])

    def test_native_manager_effects_refuse_without_consuming_an_action(self):
        import routeb_host_native as native
        profile = {'capture': {'manager': {'kind': 'systemd', 'scope': 'user',
                   'target': 'ags-hostpkg-test-example.service'}}}
        for action in ('stop', 'start'):
            host = SimpleNamespace(scope='isolated-native', recheck=Mock())
            manager = native.FixedManager(self.root, profile, host)
            manager.before = {'pids': set()}
            manager.definition = Mock()
            with self.subTest(action=action), patch.object(native, 'run',
                    side_effect=AssertionError('manager command')) as command:
                with self.assertRaisesRegex(ContractError, '^interpreter_full_closure_required$'):
                    getattr(manager, action)()
            self.assertEqual(manager.called, set())
            host.recheck.assert_not_called()
            manager.definition.assert_not_called()
            command.assert_not_called()

    def test_bootstrap_cli_plan_and_activate_are_closed_before_any_binding_read(self):
        from contextlib import redirect_stderr, redirect_stdout
        import io
        import json
        import sys
        import routeb_native_bootstrap as bootstrap
        for command in ('plan', 'activate'):
            stdout, stderr = io.StringIO(), io.StringIO()
            argv = ['routeb-native-bootstrap', command, '--root', str(self.root),
                    '--approval', str(self.root / 'approval.json')]
            with self.subTest(command=command), patch.object(sys, 'argv', argv), \
                    patch.object(bootstrap, 'read_bindings', side_effect=AssertionError('binding read')) as read, \
                    redirect_stdout(stdout), redirect_stderr(stderr):
                self.assertEqual(bootstrap.main(), 78)
            read.assert_not_called()
            if command == 'plan':
                value = json.loads(stdout.getvalue())
                self.assertEqual(value['status'], 'blocked')
                self.assertFalse(value['native_execution_allowed'])
                self.assertFalse(value['native_pass'])
                self.assertFalse(value['installable'])
                self.assertEqual(value['blockers'], ['interpreter_full_closure_required'])
                self.assertEqual(value['effects'], [])
            else:
                self.assertEqual(stderr.getvalue(), 'code=interpreter_full_closure_required\n')

    def test_verified_bundle_launcher_still_refuses_runtime_execution(self):
        """Synthetic root trust exercises the gate, never a native acceptance."""
        from contextlib import ExitStack, redirect_stderr, redirect_stdout
        import io
        import json
        import stat
        import sys
        import routeb_launcher as launcher
        import routeb_host_identity
        import upgrade
        import upgrade_identity
        bundle = self.root / 'bundle'
        bundle.mkdir(mode=0o700)
        launcher_raw = Path(launcher.__file__).read_bytes()
        trusted = lambda path, **_kwargs: b'{}' if path.name == 'manifest.json' else launcher_raw
        for command in ('doctor', 'serve'):
            stdout, stderr = io.StringIO(), io.StringIO()
            with self.subTest(command=command), ExitStack() as stack:
                stack.enter_context(patch.object(sys, 'path', list(sys.path)))
                stack.enter_context(patch.object(sys, 'argv', list(sys.argv)))
                stack.enter_context(patch.object(sys, 'flags', SimpleNamespace(
                    isolated=1, ignore_environment=1, no_site=1, no_user_site=1)))
                stack.enter_context(patch.object(launcher, 'bundle_location', return_value=bundle))
                stack.enter_context(patch.object(Path, 'lstat', return_value=SimpleNamespace(
                    st_uid=0, st_mode=stat.S_IFDIR | 0o755)))
                verifier = stack.enter_context(patch.object(upgrade, 'bundle_engine'))
                stack.enter_context(patch.object(upgrade_identity, 'read_trusted', side_effect=trusted))
                stack.enter_context(patch.object(routeb_host_identity, 'resolve_for_root',
                                                 return_value=SimpleNamespace(bundle=bundle)))
                runtime = stack.enter_context(patch.object(launcher.runpy, 'run_path',
                    side_effect=AssertionError('runtime execution')))
                stack.enter_context(redirect_stdout(stdout))
                stack.enter_context(redirect_stderr(stderr))
                self.assertEqual(launcher.main(['--root', str(self.root), command]), 78)
                verifier.assert_called_once()
                runtime.assert_not_called()
            if command == 'doctor':
                value = json.loads(stdout.getvalue())
                self.assertEqual(value['status'], 'blocked')
                self.assertFalse(value['native_execution_allowed'])
                self.assertFalse(value['installable'])
                self.assertEqual(value['blockers'], ['interpreter_full_closure_required'])
                self.assertEqual(value['effects'], [])
            else:
                self.assertEqual(stderr.getvalue(), 'code=interpreter_full_closure_required\n')

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
