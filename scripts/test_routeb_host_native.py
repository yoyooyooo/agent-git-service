#!/usr/bin/env python3
"""Native capability boundaries with recorded inputs; no manager is controlled."""
from pathlib import Path
import plistlib
import subprocess
import sys
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import routeb_host_native as native
from upgrade_backup_contract import ContractError


class NativeCapabilityTests(unittest.TestCase):
    def test_native_commands_do_not_inherit_loader_or_manager_overrides(self):
        with patch.dict(native.os.environ, {'LD_LIBRARY_PATH': '/untrusted', 'SYSTEMD_BUS_ADDRESS': 'untrusted'}), \
                patch.object(native.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0, b'', b'')) as runner:
            native.run(['/usr/bin/systemctl', '--user', 'show', 'ags-hostpkg-test-test.service'])
        env = runner.call_args.kwargs['env']
        self.assertEqual(set(env), {'PATH', 'LC_ALL', 'XDG_RUNTIME_DIR', 'DBUS_SESSION_BUS_ADDRESS'})
        self.assertEqual(env['DBUS_SESSION_BUS_ADDRESS'], 'unix:path=/run/user/' + str(native.os.getuid()) + '/bus')

    def test_lsof_mandatory_descriptors_and_access_modes(self):
        self.assertEqual(native.holder_writes([]), 0)
        self.assertEqual(native.holder_writes(['p12', 'f3', 'ar']), 0)
        self.assertEqual(native.holder_writes(['p12', 'f3', 'ar', 'f4', 'au', 'p14', 'f5', 'aw']), 2)

    def test_lsof_partial_unknown_and_duplicate_access_refuse(self):
        for rows in (['p12'], ['p12', 'f3'], ['p12', 'f3', 'a '], ['f3', 'aw'],
                     ['p12', 'f3', 'ar', 'aw'], ['p12', 'p13', 'f3', 'ar'], ['unknown']):
            with self.subTest(rows=rows), self.assertRaises(ContractError):
                native.holder_writes(rows)

    def test_process_descendants_cover_the_original_generation_tree(self):
        self.assertEqual(native.descendants({10: 1, 11: 10, 12: 11, 20: 1}, 10), {10, 11, 12})

    def manager(self, kind='launchd', scope='isolated-native'):
        root = Path('/fixture/account/.local/state/ags-routeb-native/test/ags')
        spec = {'kind': kind, 'target': 'gui/501/com.jackie.ags-hostpkg-test.test',
                'definition': str(root / 'config/test.plist'), 'definition_sha256': 'a' * 64}
        if kind == 'systemd':
            spec.update(scope='system' if scope == 'production' else 'user',
                        target='agent-git-service.service' if scope == 'production' else 'ags-hostpkg-test-test.service')
        host = SimpleNamespace(scope=scope, bundle=Path('/usr/local/libexec/frozen'), recheck=lambda: None)
        profile = {'capture': {'manager': spec}, 'interpreter': {'path': '/usr/bin/python3'}}
        return native.FixedManager(root, profile, host)

    def test_launchd_definition_requires_no_restart_and_exact_isolated_interpreter(self):
        manager = self.manager()
        value = {'Label': manager.spec['target'].rsplit('/', 1)[-1],
                 'ProgramArguments': ['/usr/bin/python3', '-I', '-B', str(manager.root / 'bin/ags-runtime.py'),
                                      '--root', str(manager.root), 'serve'],
                 'RunAtLoad': True, 'KeepAlive': False, 'ExitTimeOut': 90}
        cap = SimpleNamespace(read_regular=lambda *_a, **_k: plistlib.dumps(value), sha=lambda _: 'a' * 64)
        with patch.object(native, 'capture_module', return_value=cap):
            manager.definition()
            for key, changed in (('KeepAlive', True), ('ExitTimeOut', 20), ('ProgramArguments', ['/bin/sh'])):
                original = value[key]
                value[key] = changed
                with self.subTest(key=key), self.assertRaises(ContractError):
                    manager.definition()
                value[key] = original

    def test_systemd_production_has_only_the_two_existing_fixed_commands(self):
        manager = self.manager('systemd', 'production')
        with patch.object(native, 'run', return_value=subprocess.CompletedProcess([], 0, b'', b'')) as runner:
            manager._systemd('stop')
            self.assertEqual(runner.call_args.args[0], ['sudo', '-n', 'systemctl', 'stop', 'agent-git-service.service'])
            with self.assertRaises(ContractError):
                manager._systemd('mask')
            manager.spec['target'] = 'unrelated.service'
            with self.assertRaises(ContractError):
                manager._systemd('start')
            self.assertEqual(runner.call_count, 1)

    def test_same_start_action_is_never_replayed_after_unknown(self):
        manager = self.manager('systemd')
        # Isolate the once-only component from the closed native entry.
        with patch('routeb_host_package.require_interpreter_closure', return_value=None), \
                patch.object(manager, 'definition'), patch.object(manager, '_systemd', side_effect=TimeoutError) as runner:
            with self.assertRaises(TimeoutError):
                manager.start()
            with self.assertRaises(ContractError):
                manager.start()
            self.assertEqual(runner.call_count, 1)

    def test_legacy_systemd_still_rejects_gc_zero_exit_status(self):
        manager = self.manager('systemd')
        manager.before = {'pids': {123}}
        properties = {'MainPID': '0', 'ActiveState': 'inactive', 'SubState': 'dead',
                      'ExecMainCode': '0', 'ExecMainStatus': '0'}
        with patch('routeb_host_package.require_interpreter_closure'), \
                patch.object(manager, 'definition'), patch.object(manager, '_systemd'), \
                patch.object(native, 'systemd_properties', return_value=properties), \
                patch.object(native, 'absent', return_value=True), \
                patch.object(native.time, 'monotonic', side_effect=[0, 0, 96]), \
                patch.object(native.time, 'sleep'), \
                self.assertRaisesRegex(ContractError, 'normal_stop_unproven'):
            manager.stop()


if __name__ == '__main__':
    unittest.main()
