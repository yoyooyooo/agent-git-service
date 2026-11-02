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

    def test_systemd_factory_rejects_missing_native_runner_without_effect(self):
        with self.assertRaisesRegex(ValueError, 'native_adapter_unavailable'):
            systemd.create(self.root, {'native_profile': {'scope': 'system', 'name': 'fixture.service'}})


if __name__ == '__main__':
    unittest.main()
