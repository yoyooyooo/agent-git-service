#!/usr/bin/env python3
"""B3: systemd probe readback must request systemctl show, not status text.

The runner is injected. No test calls a real systemctl.
"""
import unittest

import upgrade_adapter_systemd as systemd
from test_native_manager_parse import SYSTEMCTL_SHOW


UNIT = {'scope': 'system', 'name': 'fixture-once.service',
        'unit_path': '/etc/systemd/system/fixture-once.service'}
STATUS_TEXT = (
    '● fixture-once.service - Fixture\n'
    '     Loaded: loaded (/etc/systemd/system/fixture-once.service; enabled)\n'
    '     Active: active (running)\n'
    '   Main PID: 720 (fixture)\n')


def _runner(calls, readback_action, stdout):
    def runner(argv):
        calls.append(list(argv))
        if argv[2] == readback_action:
            return {'exit': 0, 'stdout': stdout}
        return {'exit': 0}
    return runner


class SystemdShowReadbackTests(unittest.TestCase):
    def test_show_text_is_parsed_and_requested_with_properties(self):
        calls = []
        result = systemd.native_probe(
            UNIT, permitted=True, label=UNIT['name'],
            runner=_runner(calls, 'show', SYSTEMCTL_SHOW))
        self.assertEqual(result['verified'], False)
        self.assertEqual(result['reason'], 'native_os_unverified')
        show = [argv for argv in calls if argv[2] == 'show']
        self.assertEqual(len(show), 1)
        self.assertEqual(show[0][:3], ['systemctl', '--system', 'show'])
        self.assertIn(UNIT['name'], show[0])
        self.assertIn('--property=MainPID,ControlGroup,InvocationID', show[0])
        self.assertNotIn('status', [argv[2] for argv in calls])
        self.assertEqual(result['readback']['pid'], 720)
        self.assertEqual(result['readback']['cgroup'], '/system.slice/example.service')
        self.assertEqual(result['readback']['boot_id'], 'f7e172a7b99d4c918ca0abcd664ea47f')

    def test_status_shaped_text_is_refused(self):
        calls = []
        with self.assertRaises(systemd.AdapterError) as caught:
            systemd.native_probe(
                UNIT, permitted=True, label=UNIT['name'],
                runner=_runner(calls, 'show', STATUS_TEXT))
        self.assertEqual(caught.exception.code, 'manager_output_malformed')
        self.assertTrue(any(argv[2] == 'show' for argv in calls))

    def test_malformed_show_is_refused(self):
        calls = []
        malformed = 'MainPID=720\nControlGroup=relative\nInvocationID=abc\n'
        with self.assertRaises(systemd.AdapterError) as caught:
            systemd.native_probe(
                UNIT, permitted=True, label=UNIT['name'],
                runner=_runner(calls, 'show', malformed))
        self.assertEqual(caught.exception.code, 'manager_output_malformed')


if __name__ == '__main__':
    unittest.main()
