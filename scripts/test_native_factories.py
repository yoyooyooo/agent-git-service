#!/usr/bin/env python3
import hashlib
import tempfile
from pathlib import Path
import unittest

import upgrade_adapter_launchd as launchd
import upgrade_adapter_systemd as systemd


class NativeFactoryTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(dir=Path.home(), prefix='s6b07-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()

    def test_launchd_factory_requires_injected_isolated_runner_and_returns_contract(self):
        package = {'native_profile': {'domain': 'gui/fixture', 'label': 'com.example.synthetic',
                                      'plist_sha256': 'a' * 64, 'root': str(self.root)},
                   'native_runner': lambda argv: {'argv': argv}}
        capabilities = launchd.create(self.root, package)
        self.assertTrue({'manager', 'writer_probe', 'trash'} <= set(capabilities))
        self.assertEqual(capabilities['native_verified'], False)
        self.assertEqual(capabilities['unverified_reason'], 'native_probe_not_run')

    def test_factory_managers_accept_no_argument_calls(self):
        calls = []

        def runner(argv):
            calls.append(list(argv))
            return {'exit': 0, 'stdout': 'pid = 1\nboot = boot-1\n'}

        launchd_cap = launchd.create(self.root, {
            'native_profile': {'domain': 'gui/501', 'label': 'com.example.once',
                               'plist': '/Library/LaunchDaemons/com.example.once.plist'},
            'native_runner': runner,
        })
        for method in ('suppress_restart', 'stop', 'start'):
            getattr(launchd_cap['manager'], method)()
        systemd_cap = systemd.create(self.root, {
            'native_profile': {'scope': 'user', 'name': 'fixture-once.service', 'expected_uid': 501},
            'native_runner': runner,
        })
        for method in ('suppress_restart', 'stop', 'start'):
            getattr(systemd_cap['manager'], method)()
        self.assertTrue(calls)
        self.assertFalse(launchd_cap['native_verified'])
        self.assertFalse(systemd_cap['native_verified'])

    def test_systemd_factory_rejects_missing_native_runner_without_effect(self):
        with self.assertRaisesRegex(ValueError, 'native_adapter_unavailable'):
            systemd.create(self.root, {'native_profile': {'scope': 'system', 'name': 'fixture.service'}})


if __name__ == '__main__':
    unittest.main()
