#!/usr/bin/env python3
"""Native launchd StopEvidence. Exit zero is not closure."""
import os
import subprocess
import sys
import unittest

import upgrade_adapter_launchd as adapter


def service(**overrides):
    value = {'domain': 'gui/501', 'label': 'com.example.synthetic',
             'plist': '/tmp/synthetic.plist'}
    value.update(overrides)
    return value


def print_text(*, state='running', pid=4242):
    return {'exit': 0, 'stdout': f"""gui/501/com.example.synthetic = {{
\tstate = {state}
\tpid = {pid}
\tpath = /tmp/synthetic.plist
}}
""", 'stderr': ''}


class StopEvidenceTests(unittest.TestCase):
    def test_exit_zero_while_enabled_or_child_alive_is_refused(self):
        calls = []

        def runner(argv):
            calls.append(tuple(argv))
            if argv[1] == 'print-disabled':
                return {'exit': 0, 'stdout': '"com.example.synthetic" => enabled\n', 'stderr': ''}
            if argv[1] == 'print':
                return print_text()
            return {'exit': 0, 'stdout': '', 'stderr': ''}

        manager = adapter.Manager(service(), runner=runner, observe=runner)
        with self.assertRaises(adapter.AdapterError) as caught:
            manager.stop()
        self.assertIn(caught.exception.code, ('stop_not_closed', 'disabled_not_observed'))
        self.assertTrue(any(item[1] == 'print' for item in calls))

    def test_disabled_and_empty_job_is_stop_evidence(self):
        def runner(argv):
            if argv[1] == 'print-disabled':
                return {'exit': 0, 'stdout': '"com.example.synthetic" => disabled\n', 'stderr': ''}
            if argv[1] == 'print':
                return {'exit': 113, 'stdout': '', 'stderr': 'Could not find service'}
            return {'exit': 0, 'stdout': '', 'stderr': ''}

        evidence = adapter.Manager(service(), runner=runner, observe=runner).stop()
        self.assertEqual(evidence['disabled'], True)
        self.assertEqual(evidence['closed'], True)
        self.assertEqual(evidence['process_group_empty'], True)
        self.assertTrue(evidence['boot_id'])
        self.assertTrue(evidence['start_time'])
        self.assertNotEqual(evidence['boot_id'], 'boot-1')
        self.assertNotIn('native_verified', evidence)

    def test_enabled_readback_after_disable_is_refused(self):
        def runner(argv):
            if argv[1] == 'print-disabled':
                return {'exit': 0, 'stdout': '"com.example.synthetic" => enabled\n', 'stderr': ''}
            return {'exit': 0, 'stdout': '', 'stderr': ''}

        with self.assertRaises(adapter.AdapterError) as caught:
            adapter.Manager(service(), runner=runner, observe=runner).suppress_restart()
        self.assertEqual(caught.exception.code, 'disabled_not_observed')

    def test_truncated_output_is_refused(self):
        def runner(argv):
            return {'exit': 0, 'stdout': 'x' * 4096, 'stderr': '', 'truncated': True}

        with self.assertRaises(adapter.AdapterError) as caught:
            adapter.Manager(service(), runner=runner, observe=runner).suppress_restart()
        self.assertEqual(caught.exception.code, 'output_truncated')

    def test_real_absent_job_response_is_supported(self):
        """A real missing launchd job is exit 113 plus Could not find service, not exit 0."""
        calls = []

        def runner(argv):
            calls.append(tuple(argv))
            if argv[1] == 'print':
                return {'exit': 113, 'stdout': '',
                        'stderr': 'Bad request.\nCould not find service "com.example.synthetic" in domain for user gui: 501\n'}
            if argv[1] == 'print-disabled':
                return {'exit': 0, 'stdout': '"com.example.synthetic" => disabled\n', 'stderr': ''}
            return {'exit': 0, 'stdout': '', 'stderr': ''}

        evidence = adapter.Manager(service(), runner=runner, observe=runner).stop()
        self.assertTrue(any(item[1] == 'print' for item in calls))
        self.assertEqual(evidence['closed'], True)
        self.assertEqual(evidence['process_group_empty'], True)
        self.assertNotIn('native_verified', evidence)

    def test_empty_exit_zero_print_is_not_absent(self):
        def runner(argv):
            if argv[1] == 'print':
                return {'exit': 0, 'stdout': '', 'stderr': ''}
            if argv[1] == 'print-disabled':
                return {'exit': 0, 'stdout': '"com.example.synthetic" => disabled\n', 'stderr': ''}
            return {'exit': 0, 'stdout': '', 'stderr': ''}

        with self.assertRaises(adapter.AdapterError) as caught:
            adapter.Manager(service(), runner=runner, observe=runner).stop()
        self.assertIn(caught.exception.code, ('job_absence_unproven', 'command_not_observed', 'stop_not_closed'))

    def test_stop_does_not_claim_unobserved_descendants_or_identity(self):
        """A live named child and a stale boot/start are not an empty process group."""
        calls = []

        def runner(argv):
            calls.append(tuple(argv))
            if argv[1] == 'print-disabled':
                return {'exit': 0, 'stdout': '"com.example.synthetic" => disabled\n', 'stderr': ''}
            if argv[1] == 'print':
                return {'exit': 113, 'stdout': '', 'stderr': 'Could not find service'}
            return {'exit': 0, 'stdout': '', 'stderr': ''}

        child = subprocess.Popen([sys.executable, '-B', '-c', 'import sys; sys.stdin.read()'],
                                 stdin=subprocess.PIPE)
        self.addCleanup(lambda: (child.stdin.close(), child.wait(timeout=10)))
        seen = {'enumerated': 0, 'identity': 0}

        def enumerate_descendants():
            seen['enumerated'] += 1
            return [child.pid]

        def read_identity():
            seen['identity'] += 1
            return ('observed-boot', 'observed-start')

        profile = dict(service(), child_pid=child.pid, supervisor_pid=os.getpid(),
                       boot_id='stale-boot', start_time=-1)
        evidence = adapter.Manager(profile, runner=runner, observe=runner,
                                   enumerate_descendants=enumerate_descendants,
                                   read_identity=read_identity).stop()
        self.assertIsNone(child.poll())
        self.assertEqual(seen, {'enumerated': 1, 'identity': 1})
        self.assertEqual(evidence.get('descendants'), [child.pid])
        self.assertFalse(evidence.get('process_group_empty'))
        self.assertFalse(evidence.get('closed'))
        self.assertEqual(evidence.get('boot_id'), 'observed-boot')
        self.assertEqual(evidence.get('start_time'), 'observed-start')
        self.assertNotEqual(evidence.get('boot_id'), 'stale-boot')
        self.assertNotEqual(evidence.get('start_time'), -1)
        self.assertTrue(any(item[1] in ('print', 'bootout') for item in calls))


if __name__ == '__main__':
    unittest.main()
