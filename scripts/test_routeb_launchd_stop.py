#!/usr/bin/env python3
"""Native launchd StopEvidence. Exit zero is not closure."""
import unittest

import upgrade_adapter_launchd as adapter


def service():
    return {'domain': 'gui/501', 'label': 'com.example.synthetic',
            'plist': '/tmp/synthetic.plist', 'boot_id': 'boot-1', 'start_time': 10}


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
                return {'exit': 0, 'stdout': '', 'stderr': 'Could not find service'}
            return {'exit': 0, 'stdout': '', 'stderr': ''}

        evidence = adapter.Manager(service(), runner=runner, observe=runner).stop()
        self.assertEqual(evidence['disabled'], True)
        self.assertEqual(evidence['process_group_empty'], True)
        self.assertEqual(evidence['boot_id'], 'boot-1')
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


if __name__ == '__main__':
    unittest.main()
