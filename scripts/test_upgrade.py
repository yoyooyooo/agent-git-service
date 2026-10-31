#!/usr/bin/env python3
"""Real isolated transactions, executable smoke servers and crash injection."""
from datetime import datetime, timedelta, timezone
import contextlib
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import upgrade
import upgrade_engine as engine
import upgrade_host as host

OLD, NEW = 'a' * 40, 'b' * 40


def authorization(binding):
    now = datetime.now(timezone.utc)
    return {'schema': 'ags.local-upgrade-authorization.v2', 'authorization_id': 'auth1',
            'binding': binding, 'decision': 'approve', 'rollback_mode': 'forward-only',
            'actor': {'type': 'human', 'id': 'operator', 'source': 'tty'},
            'issued_at': (now - timedelta(minutes=1)).isoformat(),
            'expires_at': (now + timedelta(minutes=5)).isoformat(), 'evidence_sha256': 'c' * 64}


class Effects:
    def __init__(self, root, fail=None):
        self.root, self.fail = root, fail
        self.released = False

    def event(self, event):
        with (self.root / 'effects').open('a') as stream:
            stream.write(event + '\n')
        if self.fail == event:
            raise upgrade.Failure('child_timeout', event, 82)

    def begin(self): self.event('begin')
    def quiescent(self): self.event('quiescent')
    def restore(self): self.event('restore')
    def install_intent(self, authorize): authorize(); self.event('install_intent')
    def installed(self): self.event('installed')
    def start_intent(self): self.event('start_intent')
    def release(self):
        if not self.released:
            self.released = True
            # No resource was acquired on an admission/preflight failure.
            if (self.root / 'effects').exists() and 'begin' in (self.root / 'effects').read_text().splitlines():
                self.event('release')
    def preflight(self): self.event('preflight')
    def backup(self): self.event('backup')
    def stop(self): self.event('stop')
    def install(self): self.event('install')
    def start(self): self.event('start')
    def health(self): self.event('health')


class UpgradeTests(unittest.TestCase):
    def setUp(self):
        # directory_fd rejects group/other-writable ancestors. Runner /tmp is 1777.
        self.tmp = tempfile.TemporaryDirectory(prefix='.ags-upgrade-tests-', dir=Path.home())
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        self.state = self.root / 'local-v2'
        self.binding = {'deployment_id': 'fixture', 'attempt_id': '1.1', 'old': OLD, 'new': NEW}
        self.auth = authorization(self.binding)
        # Dormant journal mechanics are separate from the unconditional apply gate.
        gate = patch.object(engine, 'require_apply_contracts')
        gate.start(); self.addCleanup(gate.stop)

    def test_authorization_precedes_even_claim_and_backup(self):
        self.auth['decision'] = 'deny'
        with self.assertRaisesRegex(upgrade.Failure, 'authorization_invalid'):
            engine.Transaction(self.state, self.binding, self.auth, Effects(self.root)).run()
        self.assertFalse(self.state.exists())
        self.assertFalse((self.root / 'effects').exists())

    def test_host_plan_does_not_claim_backup_or_invoke_effects(self):
        with patch.object(sys, 'argv', ['upgrade_host.py', 'plan', '--deployment-id', 'fixture',
                                      '--pin-sha256', 'c' * 64, '--ci-run-id', '1',
                                      '--ci-run-attempt', '1', '--authorization-id', 'auth1']), patch.object(
                host, 'context', return_value=(self.root, {}, {}, {}, self.binding)), patch.object(
                host, 'Transaction', side_effect=AssertionError('plan cannot transact')), contextlib.redirect_stdout(io.StringIO()) as output:
            self.assertEqual(host.main(), 0)
        self.assertTrue(json.loads(output.getvalue())['read_only'])
        self.assertEqual(list(self.root.iterdir()), [])

    def test_exact_three_column_admission(self):
        registry = [{'old': OLD, 'new': NEW, 'migrations': ['one', 'two']}]
        engine.migration_admission(OLD, NEW, ['one', 'two'], registry)
        for old, new, migrations in ((NEW, NEW, ['one', 'two']), (OLD, OLD, ['one', 'two']),
                                      (OLD, NEW, ['one']), (OLD, NEW, ['one', 'three']),
                                      (OLD, NEW, ['two', 'one']), (OLD, NEW, [])):
            with self.subTest(migrations=migrations), self.assertRaises(upgrade.Failure):
                engine.migration_admission(old, new, migrations, registry)

    def test_wrong_uid_is_refused_before_business(self):
        with patch.object(upgrade.os, 'getuid', return_value=0):
            with self.assertRaisesRegex(upgrade.Failure, 'wrong_execution_identity'):
                upgrade.execution_identity('mini')
        self.assertFalse(self.state.exists())

    def test_missing_bundle_is_dependency_69(self):
        with self.assertRaises(upgrade.Failure) as caught:
            upgrade.bundle_engine(self.root / 'absent', '0' * 64)
        self.assertEqual((caught.exception.code, caught.exception.exit_code), ('dependency_unavailable', 69))

    def test_completed_attempt_is_observed_not_replayed_and_has_no_self_approval(self):
        first = engine.Transaction(self.state, self.binding, self.auth, Effects(self.root)).run()
        second = engine.Transaction(self.state, self.binding, self.auth, Effects(self.root)).run()
        self.assertEqual(first, second)
        events = (self.root / 'effects').read_text().splitlines()
        self.assertEqual(events, ['preflight', 'begin', 'stop', 'quiescent', 'backup', 'restore', 'install_intent', 'install', 'installed', 'start_intent', 'release', 'start', 'health', 'health'])
        self.assertNotIn('approved', json.dumps(first))
        self.assertEqual(first['status'], 'awaiting_observation')

    def test_post_stop_failure_is_forward_only_no_rollback(self):
        with self.assertRaisesRegex(upgrade.Failure, 'forward_only_recovery_required'):
            engine.Transaction(self.state, self.binding, self.auth, Effects(self.root, 'install')).run()
        journal, _ = upgrade.private_json(self.state / 'pending/journal.json')
        self.assertTrue(journal['stop_started'])
        self.assertTrue(journal['install_started'])
        with self.assertRaisesRegex(upgrade.Failure, 'forward_only_recovery_required'):
            engine.Transaction(self.state, self.binding, self.auth, Effects(self.root)).run()
        self.assertEqual((self.root / 'effects').read_text().splitlines(), ['preflight', 'begin', 'stop', 'quiescent', 'backup', 'restore', 'install_intent', 'install', 'release'])

    def test_real_process_crash_retains_claim_and_refuses_replay(self):
        script = '''import os,sys,json
from pathlib import Path
import upgrade_engine
upgrade_engine.require_apply_contracts=lambda host, **kwargs: None
from upgrade_engine import Transaction
class Effects:
 def preflight(self): os._exit(91)
 def release(self): pass
b=json.loads(sys.argv[2]);a=json.loads(sys.argv[3])
Transaction(Path(sys.argv[1]),b,a,Effects()).run()
'''
        result = subprocess.run([sys.executable, '-c', script, str(self.state), json.dumps(self.binding), json.dumps(self.auth)],
                                cwd=Path(__file__).parent, capture_output=True)
        self.assertEqual(result.returncode, 91, result.stderr)
        self.assertTrue((self.state / 'pending/claim.json').exists())
        with self.assertRaisesRegex(upgrade.Failure, 'transaction_state_unknown'):
            engine.Transaction(self.state, self.binding, self.auth, Effects(self.root)).run()
        self.assertFalse((self.root / 'effects').exists())

    def test_partial_claim_crash_is_explicit_failure(self):
        self.state.mkdir(mode=0o700); (self.state / 'pending').mkdir(mode=0o700)
        with self.assertRaises(upgrade.Failure):
            engine.Transaction(self.state, self.binding, self.auth, Effects(self.root)).run()
        self.assertFalse((self.root / 'effects').exists())

    def test_contract_failure_after_stop_preserves_83_and_stopped_recovery_state(self):
        effects = Effects(self.root)
        from upgrade_backup_contract import ContractError
        with patch.object(effects, 'backup', side_effect=ContractError('source_changed')):
            with self.assertRaises(upgrade.Failure) as caught:
                engine.Transaction(self.state, self.binding, self.auth, effects).run()
        self.assertEqual(caught.exception.exit_code, 83)
        journal, _ = upgrade.private_json(self.state / 'pending/journal.json')
        self.assertEqual(journal['service_state'], 'stopped_or_unknown')
        self.assertTrue(journal['recovery_required'])
        self.assertNotIn('install', (self.root / 'effects').read_text().splitlines())

    def test_child_timeout_classifies_phase_and_missing_command(self):
        for phase, effects, code in [('old_smoke', False, 69), ('prepare', True, 82), ('stop', True, 82)]:
            with self.subTest(phase=phase), self.assertRaises(upgrade.Failure) as caught:
                engine.child([sys.executable, '-c', 'import time;time.sleep(2)'], phase, effects=effects, timeout=.03)
            self.assertEqual((caught.exception.code, caught.exception.phase, caught.exception.exit_code), ('child_timeout', phase, code))
        with self.assertRaises(upgrade.Failure) as caught:
            engine.child([str(self.root / 'absent')], 'admission')
        self.assertEqual(caught.exception.exit_code, 69)

    def test_python_child_disables_bytecode_before_startup(self):
        prefix = self.root / 'installation'
        (prefix / 'state').mkdir(parents=True)
        (prefix / 'state/receipt').write_bytes(b'existing receipt')
        def snapshot():
            return [(str(path.relative_to(prefix)), path.stat().st_mode, path.stat().st_mtime_ns,
                     None if path.is_dir() else path.read_bytes())
                    for path in [prefix, *sorted(prefix.rglob('*'))]]
        before = snapshot()
        for explicit in (False, True):
            with self.subTest(explicit_environment=explicit):
                env = {key: value for key, value in os.environ.items()
                       if key not in ('PYTHONDONTWRITEBYTECODE', 'PYTHONPYCACHEPREFIX',
                                      'PYTHONPATH', 'BASH_ENV')}
                cache = prefix / ('state/cache-explicit' if explicit else 'cache-inherited')
                env['PYTHONPYCACHEPREFIX'] = str(cache)
                command = [sys.executable, '-X', 'pycache_prefix=' + str(cache), '-c',
                           'import sys; print(sys.flags.dont_write_bytecode)']
                with patch.dict(os.environ, env, clear=True):
                    output = engine.child(command, 'admission', env=env if explicit else None)
                self.assertEqual(output, b'1\n')
                self.assertEqual(snapshot(), before)
                self.assertEqual(list(prefix.rglob('*.pyc')), [])
                self.assertNotIn('PYTHONDONTWRITEBYTECODE', env)
                self.assertEqual(env['PYTHONPYCACHEPREFIX'], str(cache))
                self.assertNotIn('-B', command)

    def binary(self, name, revision, new):
        path = self.root / name
        path.write_text('#!' + sys.executable + '\n' + '''import json,os,signal,sqlite3
from http.server import BaseHTTPRequestHandler,HTTPServer
from pathlib import Path
assert 'PRIVATE_PRODUCTION_TOKEN' not in os.environ
assert Path(os.environ['HOME']).parent == Path.cwd()
db=sqlite3.connect(os.environ['DB_DSN'][5:])
db.execute('CREATE TABLE IF NOT EXISTS users(id INTEGER PRIMARY KEY,name TEXT)')
''' + ("db.execute('ALTER TABLE users ADD COLUMN active INTEGER DEFAULT 1') if len(db.execute('PRAGMA table_info(users)').fetchall())==2 else None\n" if new else '') + '''db.execute("INSERT OR IGNORE INTO users(id,name) VALUES(1,'preserved')")
db.commit();db.close()
class Handler(BaseHTTPRequestHandler):
 def do_GET(self):
  self.send_response(200);self.end_headers();self.wfile.write(json.dumps({'status':'ready','version':''' + repr(revision) + '''}).encode())
 def log_message(self,*args): pass
signal.signal(signal.SIGTERM,lambda *args:exit(0))
HTTPServer(('127.0.0.1',int(os.environ['PORT'])),Handler).serve_forever()
''')
        path.chmod(0o700)
        return path

    def test_real_old_new_and_populated_smokes_admit_only_exact_observed_set(self):
        old = self.binary('old', OLD, False); new = self.binary('new', NEW, True)
        observed = []
        def runner(binary, revision, directory, **kwargs):
            result = engine.smoke(binary, revision, directory, _namespace=True, **kwargs)
            if directory.name == 'populated':
                observed.extend(engine.schema_changes(engine.database_snapshot(directory.parent / 'old/smoke.sqlite'),
                                                       engine.database_snapshot(result)))
            return result
        with patch.dict(os.environ, {'PRIVATE_PRODUCTION_TOKEN': 'must-not-leak'}):
            with self.assertRaisesRegex(upgrade.Failure, 'migration_admission_rejected'):
                engine.dual_smoke(old, new, OLD, NEW, [], runner=runner)
            self.assertEqual(len(observed), 1)
            result = engine.dual_smoke(old, new, OLD, NEW,
                                      [{'old': OLD, 'new': NEW, 'migrations': observed}],
                                      runner=lambda *args, **kwargs: engine.smoke(*args, _namespace=True, **kwargs))
            self.assertEqual(result, observed)
        self.assertFalse((self.root / 'smoke.sqlite').exists())

    def test_schema_compare_rejects_changed_populated_rows(self):
        before = {'t': {'columns': [(0, 'id', 'INTEGER', 0, None, 1)], 'rows': [(1,)]}}
        after = {'t': {'columns': [(0, 'id', 'INTEGER', 0, None, 1)], 'rows': [(2,)]}}
        with self.assertRaisesRegex(upgrade.Failure, 'schema_not_additive'):
            engine.schema_changes(before, after)


if __name__ == '__main__':
    unittest.main()
