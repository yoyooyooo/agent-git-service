#!/usr/bin/env python3
"""S6.04 systemd adapter. Synthetic units only; no production systemctl."""
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock

HERE = Path(__file__).resolve().parent


def unit():
    return {'scope': 'system', 'name': 'example.service', 'expected_uid': 1000,
            'peer_uid': 1000, 'dropin_sha256': 'a' * 64,
            'exec_start': '/usr/local/libexec/ags-autodeploy/launcher'}


class SystemdBaseline(unittest.TestCase):
    def test_zero_exit_with_cgroup_is_not_closed(self):
        """Existing evidence shape. A nonempty cgroup is the behavior red."""
        import upgrade_host_evidence as evidence
        ledger = Mock()
        record = evidence.observe_process(
            pid=1, started=True, exit_code=0, forced_kill=False, stop_requested=True,
            descendants=(os.getpid(),))
        self.assertFalse(record['closed'])
        self.assertEqual(ledger.mock_calls, [])


class SystemdTests(unittest.TestCase):
    def setUp(self):
        import upgrade_adapter_systemd as adapter
        self.adapter = adapter
        self.tmp = tempfile.TemporaryDirectory(dir=Path.home(), prefix='systemd-')
        self.addCleanup(self.tmp.cleanup)
        self.effects = []
        self.addCleanup(lambda: self.assertEqual(self.effects, []))
        self.unit = unit()

    def test_scope_mix_peer_and_argv_injection_refuse(self):
        other = {**self.unit, 'scope': 'user'}
        with self.assertRaises(self.adapter.AdapterError) as caught:
            self.adapter.allow(['systemctl', 'system', 'stop', self.unit['name']], other)
        self.assertEqual(caught.exception.exit_code, 77)
        for argv in (['systemctl', 'system', 'stop', self.unit['name'], '--', 'bash'],
                     ['sudo', 'systemctl', 'stop', self.unit['name']],
                     ['systemctl', 'system', 'stop', '*']):
            with self.subTest(argv=argv), self.assertRaises(self.adapter.AdapterError):
                self.adapter.allow(argv, self.unit)

    def test_triggers_dropin_and_cgroup_must_be_closed(self):
        manager = self.adapter.Manager(self.unit)
        self.assertTrue(manager.suppress_restart(
            {'restart': 'no', 'dropin_sha256': 'a' * 64, 'triggers': []})['suppressed'])
        with self.assertRaises(self.adapter.AdapterError):
            manager.suppress_restart({'restart': 'always', 'dropin_sha256': 'a' * 64, 'triggers': []})
        with self.assertRaises(self.adapter.AdapterError):
            manager.suppress_restart({'restart': 'no', 'dropin_sha256': 'b' * 64, 'triggers': []})
        with self.assertRaises(self.adapter.AdapterError):
            manager.suppress_restart({'restart': 'no', 'dropin_sha256': 'a' * 64, 'triggers': ['timer']})
        self.assertFalse(manager.stop({'command_exit': 0, 'result': 'timeout', 'cgroup': ['pid'],
                                       'main_pid': 9, 'peer_uid': 1000, 'timed_out': True})['closed'])
        self.assertTrue(manager.stop({'command_exit': 0, 'result': 'success', 'cgroup': [],
                                      'main_pid': 0, 'peer_uid': 1000, 'timed_out': False})['closed'])

    def test_wrong_peer_and_exec_drift_refuse(self):
        with self.assertRaises(self.adapter.AdapterError):
            self.adapter.allow(['systemctl', 'system', 'show', self.unit['name']],
                               {**self.unit, 'peer_uid': 0})
        manager = self.adapter.Manager(self.unit)
        with self.assertRaises(self.adapter.AdapterError):
            manager.start({'exec_start': '/bin/sh', 'active': True})


if __name__ == '__main__':
    unittest.main()
