#!/usr/bin/env python3
"""B3: parse recorded launchctl/systemctl text. No live manager call."""
import unittest

import upgrade_native_parse as parse


LAUNCHCTL_RUNNING = """\
gui/501/com.example.once = {
	active count = 1
	path = /Library/LaunchDaemons/com.example.once.plist
	state = running
	pid = 4321
	program = /usr/local/libexec/ags/gh-server
}
"""

LAUNCHCTL_STOPPED = """\
gui/501/com.example.once = {
	path = /Library/LaunchDaemons/com.example.once.plist
	state = not running
}
"""

SYSTEMCTL_SHOW = """\
MainPID=720
ControlGroup=/system.slice/example.service
InvocationID=f7e172a7b99d4c918ca0abcd664ea47f
"""


class NativeParseTests(unittest.TestCase):
    def test_launchctl_print_reads_pid_state_and_absolute_plist(self):
        row = parse.parse_launchctl_print(LAUNCHCTL_RUNNING)
        self.assertEqual(row['pid'], 4321)
        self.assertEqual(row['state'], 'running')
        self.assertEqual(row['plist'], '/Library/LaunchDaemons/com.example.once.plist')
        self.assertTrue(row['plist'].startswith('/'))

    def test_launchctl_stopped_has_no_pid(self):
        row = parse.parse_launchctl_print(LAUNCHCTL_STOPPED)
        self.assertIsNone(row['pid'])
        self.assertEqual(row['state'], 'not running')

    def test_launchctl_malformed_text_is_refused(self):
        with self.assertRaises(parse.ParseError) as caught:
            parse.parse_launchctl_print('not a job\n')
        self.assertEqual(caught.exception.code, 'manager_output_malformed')

    def test_systemctl_show_reads_pid_cgroup_and_boot(self):
        row = parse.parse_systemctl_show(SYSTEMCTL_SHOW)
        self.assertEqual(row['pid'], 720)
        self.assertEqual(row['cgroup'], '/system.slice/example.service')
        self.assertEqual(row['boot_id'], 'f7e172a7b99d4c918ca0abcd664ea47f')

    def test_systemctl_malformed_property_is_refused(self):
        with self.assertRaises(parse.ParseError):
            parse.parse_systemctl_show('MainPID=720\nControlGroup=relative\nInvocationID=abc\n')

    def test_pid_change_boot_change_and_descendant_are_distinct(self):
        before = parse.parse_launchctl_print(LAUNCHCTL_RUNNING)
        after = dict(before, pid=9999)
        self.assertEqual(parse.compare_observation(before, after, [])['reason'], 'pid_changed')
        booted = dict(before, boot_id='other-boot')
        before = dict(before, boot_id='boot-1')
        self.assertEqual(parse.compare_observation(before, booted, [])['reason'], 'boot_changed')
        self.assertEqual(parse.compare_observation(before, before, [55])['reason'], 'descendant_remaining')
        self.assertEqual(parse.compare_observation(before, before, [])['reason'], 'unchanged')


if __name__ == '__main__':
    unittest.main()
