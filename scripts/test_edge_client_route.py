import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).with_name('edge-client-route.py')
ORIGIN = 'http://primary.example.test:6666'
ALIAS = 'http://primary-alias.example.test:6666'
EDGE_IP = '192.0.2.20'
PRIMARY_IP = '192.0.2.10'
KEY = 'http.' + ORIGIN + '/.curloptResolve'


class ClientRouteTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.env = {'HOME': self.temp.name, 'PATH': os.environ['PATH'], 'GIT_CONFIG_NOSYSTEM': '1', 'PYTHONDONTWRITEBYTECODE': '1'}

    def git(self, *args):
        return subprocess.run(['/usr/bin/git', 'config', '--global', *args], env=self.env, capture_output=True, text=True, timeout=15)

    def command(self, *args, success=True):
        p = subprocess.run([sys.executable, str(SCRIPT), *args], env=self.env, capture_output=True, text=True, timeout=20)
        self.assertEqual(p.returncode == 0, success, p.stderr)
        return json.loads(p.stdout) if success else p

    def route_arguments(self):
        return ['--name', 'test-primary', '--origin', ORIGIN, '--origin', ALIAS, '--edge-ip', EDGE_IP, '--primary-ip', PRIMARY_IP]

    def install(self):
        return self.command('install', *self.route_arguments())

    def test_idempotent_install_matching_and_exact_rollback(self):
        previous = 'primary.example.test:6666:192.0.2.30'
        self.git('--add', KEY, previous)
        self.git('--add', 'user.name', 'unrelated')
        self.install()
        self.assertFalse(self.install()['changed'])
        self.assertTrue(self.command('status', '--name', 'test-primary')['configured'])
        self.assertIn(EDGE_IP + ',' + PRIMARY_IP, self.git('--get-urlmatch', 'http.curloptResolve', ORIGIN + '/any/new-repo.git').stdout)
        self.assertEqual(self.git('--get-urlmatch', 'http.curloptResolve', 'http://other.example.test:6666/any/repo.git').returncode, 1)
        self.command('remove', '--name', 'test-primary')
        self.assertEqual(self.git('--get', KEY).stdout.strip(), previous)
        self.assertEqual(self.git('--get', 'user.name').stdout.strip(), 'unrelated')

    def test_drift_is_not_overwritten(self):
        self.install()
        newer = 'primary.example.test:6666:192.0.2.90'
        self.git('--replace-all', KEY, newer)
        self.assertFalse(self.command('status', '--name', 'test-primary')['configured'])
        self.command('remove', '--name', 'test-primary', success=False)
        self.assertEqual(self.git('--get', KEY).stdout.strip(), newer)

    def test_reject_credentials_paths_and_workload(self):
        for origin in ('http://token:secret@primary.example.test:6666', ORIGIN + '/repo', ORIGIN + '?token=x'):
            self.command('install', '--origin', origin, '--edge-ip', EDGE_IP, '--primary-ip', PRIMARY_IP, success=False)
        self.env['MULTICA_TOKEN'] = 'fixture-only'
        self.command('status', '--name', 'test-primary', success=False)

    def test_plan_and_missing_status_do_not_write_state(self):
        before = sorted(str(p.relative_to(self.temp.name)) for p in Path(self.temp.name).rglob('*'))
        self.assertFalse(self.command('plan', *self.route_arguments())['applied'])
        after = sorted(str(p.relative_to(self.temp.name)) for p in Path(self.temp.name).rglob('*'))
        self.assertEqual(after, before, 'plan created machine state')
        self.command('status', '--name', 'missing', success=False)
        self.assertEqual(sorted(str(p.relative_to(self.temp.name)) for p in Path(self.temp.name).rglob('*')), before)

    def test_malformed_receipt_cannot_modify_unowned_git_keys(self):
        self.git('--add', 'user.name', 'unrelated')
        self.install()
        receipt = Path(self.temp.name) / '.local/state/ags-edge/routes/test-primary.json'
        data = json.loads(receipt.read_text())
        data['desired']['user.name'] = ['unrelated']
        data['previous']['user.name'] = ['wrong-name']
        receipt.write_text(json.dumps(data))
        receipt.chmod(0o600)
        self.command('remove', '--name', 'test-primary', success=False)
        self.assertEqual(self.git('--get', 'user.name').stdout.strip(), 'unrelated')
        self.assertIn(EDGE_IP, self.git('--get', KEY).stdout)


if __name__ == '__main__':
    unittest.main()
