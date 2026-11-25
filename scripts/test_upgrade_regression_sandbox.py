#!/usr/bin/env python3
"""Synthetic filesystem/process proofs for the regression runner's resource owner."""
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import unittest

import regression_sandbox as resource

HERE = Path(__file__).resolve().parent


class SandboxTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.root = Path(tempfile.mkdtemp(prefix='ags-sandbox-tests-')).resolve()

    @classmethod
    def tearDownClass(cls):
        # Only this class's fresh synthetic tree; no permanent-delete fallback.
        outcome = resource.system_trash(cls.root)
        if outcome != 'trashed':
            print(json.dumps({'test_fixture_retained': str(cls.root), 'cleanup': outcome}))

    def owner(self):
        return resource.create(self.root, os.getpid())

    def test_success_moves_only_the_verified_owner(self):
        owner = self.owner()
        destination = owner.path.with_name(owner.path.name + '-trashed')
        def move(path):
            path.rename(destination)
            return 'trashed'
        result = resource.finish(owner, 0, move)
        self.assertEqual(result['cleanup'], 'trashed')
        self.assertFalse(owner.path.exists())
        self.assertTrue((destination / resource.RECEIPT).exists())

    def test_failure_retains_bounded_metadata_without_following_links(self):
        owner = self.owner()
        external = self.owner().path
        (external / 'private').write_text('synthetic-content-not-for-evidence')
        (owner.path / 'outside').symlink_to(external)
        for index in range(150):
            (owner.path / str(index)).write_text('fixture')
        def refused(_path):
            self.fail('failed tests must not invoke Trash')
        result = resource.finish(owner, 7, refused)
        self.assertEqual(result['cleanup'], 'retained_failed')
        text = (owner.path / resource.RECEIPT).read_text()
        evidence = json.loads(text)
        self.assertLessEqual(len(evidence['inventory']['entries']), 128)
        self.assertTrue(evidence['inventory']['truncated'])
        self.assertNotIn('synthetic-content-not-for-evidence', text)

    def test_replacement_root_and_symlink_owner_are_retained(self):
        for scenario in ('root', 'marker'):
            owner = self.owner()
            if scenario == 'root':
                owner.path.rename(owner.path.with_name(owner.path.name + '-original'))
                owner.path.mkdir()
                (owner.path / 'foreign').write_text('must remain')
            else:
                marker = owner.path / resource.OWNER
                target = owner.path / 'owner-original'
                marker.rename(target)
                marker.symlink_to(target)
            calls = []
            result = resource.finish(owner, 0, lambda path: calls.append(path))
            self.assertIn(result['cleanup'], ('owner_unverified', 'evidence_unverified'))
            self.assertEqual(calls, [])
            self.assertTrue(owner.path.exists())

    def test_claimed_success_without_a_move_remains_unknown(self):
        owner = self.owner()
        result = resource.finish(owner, 0, lambda _path: 'trashed')
        self.assertEqual(result['cleanup'], 'trash_unverified')
        self.assertTrue(owner.path.exists())

    def test_real_process_status_is_preserved(self):
        self.assertEqual(resource.run_owned([sys.executable, '-c', 'raise SystemExit(0)']), 0)
        self.assertEqual(resource.run_owned([sys.executable, '-c', 'raise SystemExit(7)']), 7)

    def test_signal_settles_real_child_and_grandchild(self):
        owner = self.owner()
        ready = owner.path / 'ready.json'
        program = (
            'import subprocess,sys,os,json,time; '
            'child=subprocess.Popen([sys.executable,"-c","import time; time.sleep(30)"]); '
            f'open({str(ready)!r},"w").write(json.dumps([os.getpid(),child.pid])); '
            'time.sleep(30)'
        )
        child = subprocess.Popen([sys.executable, '-B', str(HERE / 'regression_sandbox.py'),
                                  'run', '--', sys.executable, '-c', program])
        try:
            for _ in range(300):
                if ready.exists():
                    break
                time.sleep(0.01)
            self.assertTrue(ready.exists())
            pids = json.loads(ready.read_text())
            child.send_signal(signal.SIGTERM)
            self.assertEqual(child.wait(timeout=5), 143)
            for pid in pids:
                for _ in range(200):
                    try:
                        os.kill(pid, 0)
                    except ProcessLookupError:
                        break
                    time.sleep(0.01)
                else:
                    self.fail('owned descendant remains after cancellation')
        finally:
            if child.poll() is None:
                child.terminate()
                child.wait(timeout=5)

    def test_real_shell_runner_uses_temp_root_and_terminal_trap(self):
        for passed in (True, False):
            case = self.owner().path
            scripts = case / 'scripts'
            scripts.mkdir()
            for name in ('ags-python.sh', 'regression_sandbox.py'):
                shutil.copy2(HERE / name, scripts / name)
            runner = (HERE / 'upgrade-regression.sh').read_text()
            runner = re.sub(r'entries=\(.*?\n\)', "entries=(\n  'scripts/probe.py'\n)", runner, flags=re.S)
            (scripts / 'upgrade-regression.sh').write_text(runner)
            (scripts / 'probe.py').write_text(f'raise SystemExit({0 if passed else 7})\n')
            home, temporary = case / 'home', case / 'tmp'
            home.mkdir()
            temporary.mkdir()
            env = dict(os.environ, HOME=str(home), TMPDIR=str(temporary))
            result = subprocess.run(['bash', str(scripts / 'upgrade-regression.sh')],
                                    cwd=case, env=env, text=True, capture_output=True, timeout=20)
            self.assertEqual(result.returncode, 0 if passed else 1, result.stdout + result.stderr)
            records = [json.loads(line) for line in result.stdout.splitlines() if line.startswith('{"event": "ags_test_sandbox"')]
            self.assertEqual(len(records), 1, result.stdout)
            receipt = records[0]
            sandbox = Path(receipt['root'])
            self.assertEqual(sandbox.parent, temporary.resolve())
            self.assertEqual(list(home.iterdir()), [])
            if not passed:
                self.assertEqual(receipt['cleanup'], 'retained_failed')
                self.assertTrue((sandbox / resource.RECEIPT).exists())
            elif sys.platform == 'darwin':
                self.assertEqual(receipt['cleanup'], 'trashed')
                self.assertFalse(sandbox.exists())
            else:
                self.assertEqual(receipt['cleanup'], 'retained_trash_unavailable')


if __name__ == '__main__':
    unittest.main()
