#!/usr/bin/env python3
"""B3: native probe runs a fixed sequence only through an injected runner."""
import unittest

import upgrade_adapter_launchd as launchd
import upgrade_adapter_systemd as systemd


class NativeProbeSequenceTests(unittest.TestCase):
    def test_default_probe_does_not_call_a_runner(self):
        calls = []
        service = {'domain': 'gui/501', 'label': 'com.example.once'}
        result = launchd.native_probe(service, permitted=False, runner=lambda argv: calls.append(argv))
        self.assertEqual(result['reason'], 'permission_unavailable')
        self.assertEqual(calls, [])
        unit = {'scope': 'system', 'name': 'fixture.service', 'expected_uid': 0}
        result = systemd.native_probe(unit, permitted=False, runner=lambda argv: calls.append(argv))
        self.assertEqual(result['reason'], 'permission_unavailable')
        self.assertEqual(calls, [])

    def test_launchd_sequence_order_and_cleanup(self):
        calls = []

        def runner(argv):
            calls.append(list(argv))
            return {'exit': 0, 'disabled': True, 'pid': None, 'state': 'running'}

        service = {'domain': 'gui/501', 'label': 'com.example.once-abc',
                   'plist': '/Library/LaunchDaemons/com.example.once-abc.plist'}
        result = launchd.native_probe(service, permitted=True, label='com.example.once-abc', runner=runner)
        self.assertEqual(result['verified'], False)
        self.assertEqual(result['reason'], 'native_os_unverified')
        self.assertEqual([item[1] for item in calls],
                         ['bootstrap', 'print', 'bootout'])
        self.assertTrue(all(item[0] == 'launchctl' for item in calls))
        self.assertEqual(calls[-1][1], 'bootout')

    def test_launchd_failure_rolls_back_with_bootout(self):
        calls = []

        def runner(argv):
            calls.append(list(argv))
            if argv[1] == 'print':
                raise RuntimeError('print failed')
            return {'exit': 0}

        service = {'domain': 'gui/501', 'label': 'com.example.once-abc',
                   'plist': '/Library/LaunchDaemons/com.example.once-abc.plist'}
        with self.assertRaises(launchd.AdapterError) as caught:
            launchd.native_probe(service, permitted=True, label='com.example.once-abc', runner=runner)
        self.assertEqual(caught.exception.code, 'native_probe_failed')
        self.assertEqual(calls[-1][1], 'bootout')

    def test_systemd_sequence_order_and_cleanup(self):
        calls = []

        def runner(argv):
            calls.append(list(argv))
            return {'exit': 0, 'ActiveState': 'active', 'Result': 'success'}

        unit = {'scope': 'user', 'name': 'fixture-once.service', 'expected_uid': 501,
                'unit_path': '/etc/systemd/system/fixture-once.service'}
        result = systemd.native_probe(unit, permitted=True, label='fixture-once.service', runner=runner)
        self.assertEqual(result['reason'], 'native_os_unverified')
        self.assertEqual([item[2] for item in calls],
                         ['enable', 'start', 'show', 'stop', 'disable'])
        self.assertTrue(all(item[:2] == ['systemctl', '--user'] for item in calls))

    def test_systemd_failure_disables_after_stop(self):
        calls = []

        def runner(argv):
            calls.append(list(argv))
            if argv[2] == 'show':
                return {'exit': 1, 'ActiveState': 'failed'}
            return {'exit': 0}

        unit = {'scope': 'system', 'name': 'fixture-once.service', 'expected_uid': 0,
                'unit_path': '/etc/systemd/system/fixture-once.service'}
        with self.assertRaises(systemd.AdapterError):
            systemd.native_probe(unit, permitted=True, label='fixture-once.service', runner=runner)
        self.assertEqual([item[2] for item in calls[-2:]], ['stop', 'disable'])

    def test_relative_plist_is_refused_and_recorded_print_is_parsed(self):
        calls = []
        relative = {'domain': 'gui/501', 'label': 'com.example.once-abc', 'plist': 'job.plist'}
        with self.assertRaises(launchd.AdapterError) as caught:
            launchd.native_probe(relative, permitted=True, label='com.example.once-abc',
                                 runner=lambda argv: calls.append(argv))
        self.assertEqual(caught.exception.code, 'plist_not_absolute')
        self.assertEqual(calls, [])
        recorded = (
            'gui/501/com.example.once-abc = {\n'
            '\tpath = /Library/LaunchDaemons/com.example.once-abc.plist\n'
            '\tstate = running\n'
            '\tpid = 77\n}\n')

        def runner(argv):
            calls.append(list(argv))
            if argv[1] == 'print':
                return {'exit': 0, 'stdout': recorded}
            return {'exit': 0}

        service = {'domain': 'gui/501', 'label': 'com.example.once-abc',
                   'plist': '/Library/LaunchDaemons/com.example.once-abc.plist'}
        result = launchd.native_probe(service, permitted=True, label='com.example.once-abc', runner=runner)
        self.assertEqual(result['verified'], False)
        self.assertEqual(result['readback']['pid'], 77)
        self.assertEqual(result['plist'], service['plist'])
        self.assertEqual(calls[0][3], service['plist'])

    def test_reused_label_is_refused_before_runner(self):
        calls = []
        service = {'domain': 'gui/501', 'label': 'com.example.stable'}
        with self.assertRaises(launchd.AdapterError) as caught:
            launchd.native_probe(service, permitted=True, label='com.example.stable',
                                 runner=lambda argv: calls.append(argv), one_shot=False)
        self.assertEqual(caught.exception.code, 'label_not_one_shot')
        self.assertEqual(calls, [])


if __name__ == '__main__':
    unittest.main()
