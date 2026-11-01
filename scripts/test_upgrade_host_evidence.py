#!/usr/bin/env python3
"""S6.02 stop/writer evidence. Real child processes, synthetic roots only."""
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import Mock

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))


class EvidenceBaseline(unittest.TestCase):
    def test_zero_exit_with_a_live_writer_is_not_closed(self):
        """Existing S3 API. A zero exit plus a live writer is the behavior red."""
        import upgrade_recovery as recovery
        ledger = Mock()
        with tempfile.TemporaryDirectory(dir=Path.home(), prefix='evidence-') as directory:
            root = Path(directory).resolve()
            writer = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(30)'])
            self.addCleanup(writer.kill)
            self.addCleanup(writer.wait)
            state = root / 'state'
            state.mkdir(mode=0o700)
            pending = state / 'ags-autodeploy/local-v2/pending'
            pending.mkdir(mode=0o700, parents=True)
            for path in (pending, *pending.parents):
                if root == path or root in path.parents:
                    path.chmod(0o700)
            record = {
                'schema': 'ags.runtime.v1', 'generation': 'a' * 32, 'root': str(root),
                'exit_code': 0, 'forced_kill': False, 'stop_requested': True,
                'supervisor_exit_code': 0, 'supervisor_exited': True,
                'process_group_empty': True, 'started_at': '2000-01-01T00:00:01+00:00',
                'stopped_at': '2000-01-01T00:00:02+00:00', 'command_exit': 0}
            (state / 'runtime.json').write_text(json.dumps(record))
            (state / 'runtime.json').chmod(0o600)
            window = recovery.ColdWindow.__new__(recovery.ColdWindow)
            window.journal = {'phase': 'stop', 'runtime_generation': 'a' * 32,
                              'stop_requested_at': '2000-01-01T00:00:00+00:00'}
            window.state = state
            window.pending = pending
            window.root = root
            window.manager = Mock()
            window.manager.suppress_restart.return_value = None
            window.manager.stop.return_value = {**record, 'live_writers': [writer.pid]}
            window.writer_probe = ledger.writer_probe
            if (HERE / 'upgrade_host_evidence.py').exists():
                import upgrade_host_evidence as evidence
                with self.assertRaises(recovery.Failure) as caught:
                    evidence.admit_stop(window)
                self.assertEqual(caught.exception.code, 'normal_stop_unproven')
            else:
                window.stop()
                self.fail('stop exit 0 with a live writer was accepted')
            self.assertEqual(ledger.mock_calls, [])


class EvidenceTests(unittest.TestCase):
    def setUp(self):
        import upgrade_host_evidence as evidence
        self.evidence = evidence
        self.tmp = tempfile.TemporaryDirectory(dir=Path.home(), prefix='evidence-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        self.ledger = Mock()
        self.addCleanup(lambda: self.assertEqual(self.ledger.mock_calls, []))

    def test_normal_child_exit_closes_and_forced_kill_does_not(self):
        child = subprocess.Popen([sys.executable, '-c', 'raise SystemExit(0)'])
        child.wait(timeout=10)
        record = self.evidence.observe_process(
            pid=child.pid, started=True, exit_code=0, forced_kill=False,
            stop_requested=True, descendants=())
        self.assertTrue(record['closed'])
        killed = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(30)'])
        killed.send_signal(signal.SIGKILL)
        killed.wait(timeout=10)
        refused = self.evidence.observe_process(
            pid=killed.pid, started=True, exit_code=-9, forced_kill=True,
            stop_requested=True, descendants=())
        self.assertFalse(refused['closed'])

    def test_surviving_grandchild_and_pid_reuse_are_open(self):
        grandchild = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(30)'])
        self.addCleanup(grandchild.kill)
        self.addCleanup(grandchild.wait)
        record = self.evidence.observe_process(
            pid=os.getpid(), started=True, exit_code=0, forced_kill=False,
            stop_requested=True, descendants=(grandchild.pid,))
        self.assertFalse(record['closed'])
        self.assertIn(grandchild.pid, record['unknown'])
        reused = self.evidence.observe_process(
            pid=os.getpid(), started=True, exit_code=0, forced_kill=False,
            stop_requested=True, descendants=(), boot_id='now', expected_boot_id='then',
            process_start='2', expected_process_start='1')
        self.assertFalse(reused['closed'])

    def test_missing_observation_permission_is_unknown(self):
        record = self.evidence.observe_process(
            pid=1, started=True, exit_code=0, forced_kill=False, stop_requested=True,
            descendants=None, observable=False)
        self.assertFalse(record['closed'])
        self.assertEqual(record['unknown'], ['unobservable'])


if __name__ == '__main__':
    unittest.main()
