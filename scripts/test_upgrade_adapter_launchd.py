#!/usr/bin/env python3
"""S6.03 launchd adapter. Isolated fixture job only; no production service."""
import hashlib
import json
import os
from pathlib import Path
import platform
import plistlib
import tempfile
import unittest
from unittest.mock import Mock

HERE = Path(__file__).resolve().parent


def service(root):
    return {'schema': 'ags.launchd-service.v1', 'domain': 'gui/fixture',
            'label': 'com.example.synthetic', 'plist_sha256': 'a' * 64,
            'launcher_sha256': hashlib.sha256(b'launcher').hexdigest(),
            'root': str(root)}


class LaunchdBaseline(unittest.TestCase):
    def test_command_success_is_not_normal_stop_evidence(self):
        """Existing evidence gate. Exit zero without closure is the behavior red."""
        import upgrade_host_evidence as evidence
        ledger = Mock()
        record = evidence.observe_process(
            pid=os.getpid(), started=True, exit_code=0, forced_kill=False,
            stop_requested=False, descendants=(os.getpid(),), observable=True)
        self.assertFalse(record['closed'])
        self.assertEqual(ledger.mock_calls, [])


class LaunchdTests(unittest.TestCase):
    def setUp(self):
        import upgrade_adapter_launchd as adapter
        self.adapter = adapter
        self.tmp = tempfile.TemporaryDirectory(dir=Path.home(), prefix='launchd-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        self.effects = []
        self.addCleanup(lambda: self.assertEqual(self.effects, []))
        self.service = service(self.root)

    def test_allowlist_rejects_injected_argv(self):
        for argv in (['launchctl', 'bootout', 'gui/fixture/com.example.synthetic', ';rm'],
                     ['launchctl', 'enable', 'system/com.example.synthetic'],
                     ['bash', '-c', 'launchctl bootout']):
            with self.subTest(argv=argv):
                with self.assertRaises(self.adapter.AdapterError) as caught:
                    self.adapter.allow(argv, self.service)
                self.assertEqual(caught.exception.exit_code, 83)

    def test_disable_readback_and_consumed_oneshot(self):
        manager = self.adapter.Manager(self.service, runner=self.effects)
        disabled = manager.suppress_restart(readback={'disabled': True, 'pid': None})
        self.assertTrue(disabled['disabled'])
        with self.assertRaises(self.adapter.AdapterError):
            manager.suppress_restart(readback={'disabled': False, 'pid': 1})
        manager.start(readback={'consumed': False, 'pid': 9})
        with self.assertRaises(self.adapter.AdapterError) as caught:
            manager.start(readback={'consumed': True, 'pid': None})
        self.assertEqual(caught.exception.exit_code, 83)

    def test_bootout_timeout_and_supervisor_exit_are_not_success(self):
        manager = self.adapter.Manager(self.service, runner=self.effects)
        for facts in (
                {'command_exit': None, 'timed_out': True},
                {'command_exit': 0, 'supervisor_exit_code': 1, 'forced_kill': True},
                {'command_exit': 0, 'descendants': [4], 'boot_id': 'other'}):
            with self.subTest(facts=facts):
                record = manager.stop(facts=facts)
                self.assertFalse(record['closed'])

    def test_native_isolation_is_explicitly_unverified_off_darwin_or_without_permission(self):
        if platform.system() != 'Darwin':
            self.skipTest('launchd native sequence is Darwin-only and unverified here')
        # Do not touch the operator service. An isolated label is optional evidence.
        outcome = self.adapter.native_probe(self.service, permitted=False)
        self.assertEqual(outcome, {'verified': False, 'reason': 'permission_unavailable'})


if __name__ == '__main__':
    unittest.main()
