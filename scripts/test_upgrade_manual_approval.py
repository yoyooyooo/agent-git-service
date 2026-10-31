#!/usr/bin/env python3
"""S4 components, not E2E: S3 private harness + real SQLite/Git/files/flock."""
from datetime import datetime, timedelta, timezone
import ast
import copy
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import unittest
from unittest.mock import patch

import upgrade
import upgrade_engine as engine
import upgrade_manual_approval as manual
import upgrade_backup_contract as c
from test_upgrade_quiescence import ColdFixture
from test_upgrade_safety import pin_fixture


class ManualFixture(ColdFixture):
    host_name = 'mini'
    carried_handoff = None

    def setUp(self):
        super().setUp()
        self.pin = pin_fixture()
        self.pin['runtime'] = {'revision': self.b['new'], 'tree': self.b['runtime_tree']}
        self.pin['ci']['revision'] = self.b['new']
        self.pin['version'] = 'fork-20260924.9-rc1'
        for target, entry in self.pin['targets'].items():
            entry['asset']['name'] = 'ags-' + self.pin['version'] + '-' + target + '.tar.gz'
            p = entry['provenance']
            p.update(runtime_revision=self.b['new'], runtime_tree=self.b['runtime_tree'],
                     control_revision=self.b['control_revision'], signer_digest=self.b['control_revision'],
                     control_tree=self.b['control_tree'])
            for command, binary in entry['binaries'].items():
                binary['identity'].update(revision=self.b['new'], tree=self.b['runtime_tree'])
                if command == 'gh-server':
                    binary['sha256'] = self.b['new_binary_sha256']
        self.pin_sha = c.fingerprint(self.pin)
        self.b.update(pin_sha256=self.pin_sha, release_tag=self.pin['version'],
                      asset_sha256='d' * 64,
                      tool_sha256=self.pin['verification_policy']['tool_bundle_manifest_sha256'],
                      policy_sha256=self.pin['verification_policy']['policy_sha256'])
        self.b['target'] = 'darwin_arm64' if self.host_name == 'mini' else 'linux_amd64'
        self.window.backup_binding = copy.deepcopy(self.b)
        self.request = {key: self.b[key] for key in ('deployment_id', 'attempt_id', 'pin_sha256',
            'old', 'new', 'runtime_tree', 'release_id', 'policy_sha256', 'authorization_id')}
        self.request.update(host=self.host_name, engine_sha256=self.b['tool_sha256'])
        self.window.binding = copy.deepcopy(self.request)
        engine.save(self.pending / 'claim.json', {'schema': 'ags.local-claim.v2', 'binding': self.request})
        self.prepare(); self.install()
        self.window.start_intent(); self.window.release(); self.window.start()
        self.now = datetime.now(timezone.utc)
        self.runtime = {'schema': 'ags.runtime.v1', 'root': str(self.root),
                        'generation': self.window.journal['generation'],
                        'started_at': (self.now - timedelta(minutes=12)).isoformat(),
                        'build': {'revision': self.b['new']}, 'pid': os.getpid(), 'supervisor_pid': os.getpid()}
        engine.save(self.root / 'state/runtime.json', self.runtime)
        engine.save(self.pending / 'journal.json', {'schema': 'ags.local-upgrade-journal.v2',
                    'binding': self.request, 'phase': 'awaiting_observation', **dict.fromkeys(engine.FLAGS, True)})
        from test_upgrade import authorization
        auth = authorization(self.request)
        auth['authorization_id'] = self.request['authorization_id']
        engine.save(self.root / 'config/upgrade-authorization-window-auth.json', auth)
        engine.save(self.pending / 'receipt.json', {'schema': 'ags.local-upgrade-receipt.v2',
                    'binding': self.request, 'status': 'awaiting_observation', 'rollback_mode': 'forward-only',
                    'authorization_id': self.request['authorization_id'],
                    'authorization_sha256': hashlib.sha256(json.dumps(auth, sort_keys=True).encode()).hexdigest()})
        self.observation = {'generation': self.runtime['generation'],
            'started_at': self.runtime['started_at'], 'ended_at': (self.now - timedelta(seconds=1)).isoformat(),
            'reports': {k: {'status': 'pass', 'sha256': 'a' * 64}
                        for k in ('business', 'git', 'provider', 'recovery')}, 'unresolved_blockers': []}
        self.packet = manual.make_packet(self.root, self.request, self.pin, self.pin_sha,
                                         self.observation, self.probe, handoff=self.carried_handoff, now=self.now)
        self.packet_path = self.root / 'packet.json'
        engine.save(self.packet_path, self.packet)
        self.packet_sha = hashlib.sha256(self.packet_path.read_bytes()).hexdigest()
        self.approval_path = self.root / 'approval.json'
        self.approval = manual.write_request(self.approval_path, self.packet, now=self.now)
        self.approval.update(approval_id='human-accept', decision='approve',
            actor={'type': 'human', 'id': 'fixture-operator', 'source': 'private-record'},
            authority_reference=self.b['authorization_id'], evidence_packet_sha256=self.packet_sha,
            issued_at=(self.now - timedelta(seconds=1)).isoformat(),
            expires_at=(self.now + timedelta(minutes=5)).isoformat(),
            independent_media={'status': 'omitted', 'evidence': 'fixture-media-review', 'reason': 'synthetic only'})
        engine.save(self.approval_path, self.approval)
        self.window.release_owner()

    def probe(self):
        return {'generation': self.runtime['generation'], 'running_revision': self.b['new'],
                'local': {'status': 'ready', 'revision': self.b['new']},
                'service': {'status': 'ready', 'revision': self.b['new']}}

    def confirm(self, **kwargs):
        return manual.confirm(self.root, self.request, self.approval_path, self.packet_path,
                              self.pin, self.pin_sha, self.probe, now=self.now, **kwargs)

    def reject(self):
        with self.assertRaises(upgrade.Failure) as error:
            self.confirm()
        self.assertEqual(error.exception.exit_code, 77)
        self.assertTrue(self.pending.exists())
        self.assertFalse(manual.completion_path(self.pending.parent, self.request).exists())


class ManualTests(ManualFixture):
    def test_accept_atomic_record_and_idempotence_without_health_replay(self):
        before = self.binary.read_bytes()
        result = self.confirm()
        self.assertEqual(result['status'], 'completed')
        self.assertEqual(self.binary.read_bytes(), before)
        self.assertFalse(self.pending.exists())
        self.assertTrue((self.pending.parent / ('accepted-' + self.packet['generation']) / 'claim.json').is_file())
        with patch.object(self, 'probe', side_effect=AssertionError('no health or effects on replay')):
            self.assertEqual(self.confirm(), result)
        self.approval['approval_id'] = 'different-human-accept'
        engine.save(self.approval_path, self.approval)
        with self.assertRaises(upgrade.Failure) as error:
            self.confirm()
        self.assertEqual(error.exception.exit_code, 77)
        self.assertEqual(manual.status(self.root, self.request)['completion'], result)

    def test_missing_approval_is_86(self):
        with self.assertRaises(upgrade.Failure) as error:
            manual.confirm(self.root, self.request, None, self.packet_path,
                           self.pin, self.pin_sha, self.probe)
        self.assertEqual(error.exception.exit_code, 86)

    def test_every_approval_field_missing_unknown_and_wrong_value(self):
        for key in self.approval:
            for mutation in ('missing', 'wrong'):
                value = copy.deepcopy(self.approval)
                if mutation == 'missing': del value[key]
                else: value[key] = None
                engine.save(self.approval_path, value)
                with self.subTest(key=key, mutation=mutation): self.reject()
        for path in ((), ('actor',), ('confirmations',), ('independent_media',)):
            value = copy.deepcopy(self.approval); target = value
            for part in path: target = target[part]
            target['unknown'] = True
            engine.save(self.approval_path, value)
            with self.subTest(path=path): self.reject()

    def test_actor_phase_time_generation_binding_digest_and_limitations(self):
        changes = [ ('actor', {'type': 'agent', 'id': 'x', 'source': 'x'}),
            ('purpose', 'window'), ('decision', 'reject'), ('decision', 'pending'),
            ('issued_at', (self.now + timedelta(seconds=1)).isoformat()),
            ('expires_at', self.now.isoformat()), ('recorded_at', '2020-01-01T00:00:00'),
            ('generation', 'another-generation'), ('binding', {**self.request, 'old': 'c' * 40}),
            ('backup_binding', {**self.b, 'transaction_id': 'another'}),
            ('evidence_packet_sha256', '0' * 64), ('evidence_id', 'another'),
            ('unresolved_blockers', ['unknown-provider']),
            ('accepted_limitations', [{'category': 'bad_fk', 'reason': 'not allowed'}])]
        for key, val in changes:
            value = copy.deepcopy(self.approval); value[key] = val
            engine.save(self.approval_path, value)
            with self.subTest(key=key, val=val): self.reject()

    def test_real_permissions_symlink_hardlink_writable_ancestors(self):
        for mode in (0o644, 0o400, 0o660, 0o700):
            self.approval_path.chmod(mode)
            with self.subTest(mode=mode): self.reject()
        self.approval_path.chmod(0o600)
        original = self.root / 'approval-original.json'; self.approval_path.rename(original)
        self.approval_path.symlink_to(original)
        self.reject()
        self.approval_path.rename(self.root / 'retained-symlink')
        os.link(original, self.approval_path); self.reject()
        self.approval_path.rename(self.root / 'retained-hardlink')
        engine.save(self.approval_path, self.approval)
        for mode in (0o777, 0o720):
            self.root.chmod(mode)
            try: self.reject()
            finally: self.root.chmod(0o700)
        # Owner mismatch uses real fstat and a different required owner, no chown privilege.
        with self.assertRaises(upgrade.Failure):
            upgrade.private_json(self.approval_path, owner=os.getuid() + 1)

    def test_duplicate_json_rejected(self):
        self.approval_path.write_bytes(b'{"decision":"approve","decision":"approve"}')
        self.reject()

    def test_generation_and_health_failures(self):
        for change in ({'generation': 'restarted'}, {'stopped_at': self.now.isoformat()},
                       {'build': {'revision': 'c' * 40}}):
            engine.save(self.root / 'state/runtime.json', {**self.runtime, **change})
            with self.subTest(change=change): self.reject()
        engine.save(self.root / 'state/runtime.json', self.runtime)
        for result in ({}, {**self.probe(), 'local': {'status': 'unknown', 'revision': self.b['new']}},
                       {**self.probe(), 'generation': 'old-generation'}):
            with patch.object(self, 'probe', return_value=result): self.reject()

    def test_full_chain_not_a_success_json(self):
        targets = [self.pending / 'journal.json', self.pending / 'cold-journal.json',
                   self.root / 'state/installation.json', self.root / 'config/upgrade-authorization-window-auth.json',
                   Path(self.window.journal['backup_dir']) / 'manifest.json',
                   Path(self.window.journal['backup_dir']) / 'inventory.json',
                   Path(self.window.journal['backup_dir']) / 'restore-report.json',
                   Path(self.window.journal['backup_dir']) / 'restore-publication.json']
        for path in targets:
            raw = path.read_bytes()
            engine.save(path, {'status': 'success'})
            with self.subTest(path=path.name): self.reject()
            path.write_bytes(raw)
        data = Path(self.window.journal['backup_dir']) / 'old_binary'
        raw = data.read_bytes(); data.write_bytes(b'changed')
        self.reject(); data.write_bytes(raw)

    def test_host_and_engine_dispatch_no_install_fallback(self):
        import upgrade_host as host
        from types import SimpleNamespace
        for command in ('status', 'reconcile'):
            with patch.object(engine.Transaction, 'run', side_effect=AssertionError('not an install route')):
                self.assertEqual(engine.acceptance(command, self.root, self.request)['action'], 'read_only_no_replay')
            args = SimpleNamespace(command=command, approval_file=None, evidence_packet=None, pin_sha256=self.pin_sha)
            with patch.object(host.argparse.ArgumentParser, 'parse_args', return_value=args), \
                    patch.object(host, 'context', return_value=(self.root, self.pin, {}, {}, self.request)), \
                    patch.object(host, 'acceptance', return_value={'status': 'read_only'}) as dispatch:
                self.assertEqual(host.main(), 0)
                self.assertEqual(dispatch.call_args.args[0], command)
        args = SimpleNamespace(command='confirm', approval_file=None, evidence_packet=self.packet_path,
                               pin_sha256=self.pin_sha)
        with patch.object(host.argparse.ArgumentParser, 'parse_args', return_value=args), \
                patch.object(host, 'context', return_value=(self.root, self.pin, {}, {}, self.request)):
            with self.assertRaises(upgrade.Failure) as error: host.main()
            self.assertEqual(error.exception.exit_code, 86)
        with self.assertRaises(upgrade.Failure):
            engine.acceptance('install', self.root, self.request)

    def test_packet_fields_and_bound_observation_cannot_be_replaced(self):
        for key in self.packet:
            value = copy.deepcopy(self.packet); value[key] = 'wrong' if value[key] is None else None
            engine.save(self.packet_path, value)
            with self.subTest(key=key): self.reject()
        engine.save(self.packet_path, self.packet)
        changed = {**self.observation, 'unresolved_blockers': ['unknown provider result']}
        with self.assertRaises(c.ContractError):
            manual.make_packet(self.root, self.request, self.pin, self.pin_sha,
                               changed, self.probe, now=self.now)

    def test_status_reconcile_are_readonly(self):
        before = {str(p): (p.read_bytes(), p.stat().st_mtime_ns) for p in self.root.rglob('*') if p.is_file()}
        result = manual.status(self.root, self.request)
        self.assertEqual(result['action'], 'read_only_no_replay')
        after = {str(p): (p.read_bytes(), p.stat().st_mtime_ns) for p in self.root.rglob('*') if p.is_file()}
        self.assertEqual(before, after)

    def test_crash_retry_only_same_durable_result(self):
        with patch.object(manual, 'checkpoint', side_effect=lambda name: (_ for _ in ()).throw(
                RuntimeError('crash')) if name == 'completion-staged' else None):
            with self.assertRaisesRegex(RuntimeError, 'crash'): self.confirm()
        staged = next(self.pending.parent.glob('.*.commit'))
        expected = json.loads(staged.read_bytes())
        self.now += timedelta(seconds=1)
        self.assertEqual(self.confirm(), expected)

    def test_crash_after_durable_completion_never_rechecks_or_runs_effect(self):
        with patch.object(manual, 'checkpoint', side_effect=lambda name: (_ for _ in ()).throw(
                RuntimeError('crash')) if name == 'completion-durable' else None):
            with self.assertRaises(RuntimeError): self.confirm()
        expected, _ = manual.read_document(manual.completion_path(self.pending.parent, self.request))
        self.assertTrue(self.pending.exists())
        with patch.object(self, 'probe', side_effect=AssertionError('no replay')):
            self.assertEqual(self.confirm(), expected)

    def snapshot(self):
        return {str(p.relative_to(self.root)): (p.stat().st_mode, p.stat().st_mtime_ns,
                p.read_bytes() if p.is_file() else None) for p in self.root.rglob('*')}

    def crash_partial_stage(self, *, empty=False):
        pid = os.fork()
        if pid == 0:
            original = manual.durable_bytes
            def interrupted(path, raw):
                if path.name.endswith('.commit'):
                    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
                    if not empty:
                        os.write(fd, raw[:len(raw) // 2])
                    os.fsync(fd)
                    os._exit(91)
                original(path, raw)
            manual.durable_bytes = interrupted
            self.confirm()
            os._exit(92)
        _, status = os.waitpid(pid, 0)
        self.assertEqual(os.waitstatus_to_exitcode(status), 91)
        staging = next(self.pending.parent.glob('.*.commit'))
        fragment = staging.read_bytes()
        self.assertEqual(bool(fragment), not empty)
        with self.assertRaises(json.JSONDecodeError):
            json.loads(fragment)
        before = self.snapshot()
        for command in ('status', 'reconcile'):
            result = engine.acceptance(command, self.root, self.request)
            self.assertEqual(result['status'], 'awaiting_observation')
            self.assertNotIn('completion', result)
        self.assertEqual(before, self.snapshot())
        self.assertTrue(self.pending.exists())
        self.assertFalse(manual.completion_path(self.pending.parent, self.request).exists())
        return staging, fragment

    def assert_retained_fragment(self, fragment):
        retained = [p for p in self.pending.parent.rglob('*') if p.is_file()
                    and p.name != 'hostops.lock' and p.read_bytes() == fragment]
        self.assertEqual(len(retained), 1, retained)
        return retained[0]

    def test_partial_stage_process_crash_readonly_retry_and_idempotence(self):
        staging, fragment = self.crash_partial_stage()
        # A fragment cannot bypass renewed approval or full-chain validation.
        original = self.approval_path.read_bytes()
        engine.save(self.approval_path, {**self.approval, 'decision': 'pending'})
        self.reject()
        self.assertEqual(staging.read_bytes(), fragment)
        self.approval_path.write_bytes(original)
        payload = Path(self.window.journal['backup_dir']) / 'old_binary'
        original = payload.read_bytes()
        info = payload.stat()
        payload.write_bytes(b'corrupt')
        self.reject()
        self.assertEqual(staging.read_bytes(), fragment)
        payload.write_bytes(original)
        os.utime(payload, ns=(info.st_atime_ns, info.st_mtime_ns))
        result = self.confirm()
        self.assertEqual(result['status'], 'completed')
        self.assertFalse(staging.exists())
        retained = self.assert_retained_fragment(fragment)
        before = self.snapshot()
        with patch.object(self, 'probe', side_effect=AssertionError('no replay')):
            self.assertEqual(self.confirm(), result)
            for command in ('status', 'reconcile'):
                self.assertEqual(engine.acceptance(command, self.root, self.request)['completion'], result)
        self.assertEqual(before, self.snapshot())
        self.assertEqual(retained.read_bytes(), fragment)

    def test_empty_stage_process_crash_retry(self):
        staging, fragment = self.crash_partial_stage(empty=True)
        self.assertEqual(self.confirm()['status'], 'completed')
        self.assertFalse(staging.exists())
        self.assert_retained_fragment(fragment)

    def test_partial_stage_recovery_crash_after_retaining_fragment(self):
        staging, fragment = self.crash_partial_stage()
        inode = staging.stat().st_ino
        pid = os.fork()
        if pid == 0:
            original = manual.durable_bytes
            def interrupted(path, raw):
                if path.name.endswith('.commit'):
                    os._exit(96)
                original(path, raw)
            manual.durable_bytes = interrupted
            self.confirm()
            os._exit(97)
        _, status = os.waitpid(pid, 0)
        self.assertEqual(os.waitstatus_to_exitcode(status), 96)
        self.assertFalse(staging.exists())
        self.assertTrue(self.pending.exists())
        retained = self.assert_retained_fragment(fragment)
        self.assertEqual(retained.stat().st_ino, inode)
        before = self.snapshot()
        self.assertEqual(engine.acceptance('reconcile', self.root, self.request)['status'], 'awaiting_observation')
        self.assertEqual(before, self.snapshot())
        self.assertEqual(self.confirm()['status'], 'completed')
        self.assertEqual(retained.read_bytes(), fragment)

    def test_complete_but_mismatched_stage_is_not_discarded(self):
        with patch.object(manual, 'checkpoint', side_effect=lambda name: (_ for _ in ()).throw(
                RuntimeError('crash')) if name == 'completion-staged' else None):
            with self.assertRaises(RuntimeError):
                self.confirm()
        staging = next(self.pending.parent.glob('.*.commit'))
        candidate, _ = manual.read_document(staging)
        candidate['consumed_approval']['approval_id'] = 'unrelated-approval'
        engine.save(staging, candidate)
        before = self.snapshot()
        self.reject()
        self.assertEqual(before, self.snapshot())
        self.assertTrue(staging.exists())

    def test_partial_stage_expired_approval_cannot_recover(self):
        staging, fragment = self.crash_partial_stage()
        self.now += timedelta(minutes=6)
        before = self.snapshot()
        self.reject()
        self.assertEqual(before, self.snapshot())
        self.assertEqual(staging.read_bytes(), fragment)

    def assert_readonly_completion_rejected(self):
        before = self.snapshot()
        with patch.object(manual, 'check_running', side_effect=AssertionError('no health')), \
                patch.object(manual, 'load_chain', side_effect=AssertionError('no live chain')), \
                patch.object(manual, 'validate_approval', side_effect=AssertionError('no approval reuse')), \
                patch.object(engine.Transaction, 'run', side_effect=AssertionError('no upgrade')):
            for command in ('status', 'reconcile'):
                with self.subTest(command=command), self.assertRaises(upgrade.Failure):
                    engine.acceptance(command, self.root, self.request)
        self.assertEqual(before, self.snapshot())

    def test_completed_readback_rejects_internal_inconsistency(self):
        record = self.confirm()
        path = manual.completion_path(self.pending.parent, self.request)
        mutations = [('receipt', 'generation', 'different-generation'),
                     ('consumed_approval', 'approval_id', 'unrelated-approval'),
                     ('receipt', 'approval_sha256', '0' * 64),
                     ('consumed_approval', 'sha256', '0' * 64),
                     ('receipt', 'evidence_packet_sha256', '0' * 64),
                     ('receipt', 'rollback_mode', 'automatic'),
                     ('receipt', 'unknown', True),
                     (None, 'approval_sha256', 'not-a-digest'),
                     (None, 'generation', '../invalid'),
                     (None, 'approval_id', ''),
                     (None, 'completed_at', 'not-a-time')]
        for section, key, value in mutations:
            candidate = copy.deepcopy(record)
            target = candidate if section is None else candidate[section]
            target[key] = value
            engine.save(path, candidate)
            with self.subTest(section=section, key=key):
                self.assert_readonly_completion_rejected()
        engine.save(path, record)
        self.assertEqual(manual.status(self.root, self.request)['completion'], record)

    def test_completed_readback_checks_claim_before_and_after_archive(self):
        with patch.object(manual, 'checkpoint', side_effect=lambda name: (_ for _ in ()).throw(
                RuntimeError('crash')) if name == 'completion-durable' else None):
            with self.assertRaises(RuntimeError):
                self.confirm()
        record, _ = manual.read_document(manual.completion_path(self.pending.parent, self.request))
        for directory in (self.pending, self.pending.parent / ('accepted-' + record['generation'])):
            claim_path = directory / 'claim.json'
            before = self.snapshot()
            with patch.object(self, 'probe', side_effect=AssertionError('no replay')):
                for command in ('status', 'reconcile'):
                    self.assertEqual(engine.acceptance(command, self.root, self.request)['completion'], record)
            self.assertEqual(before, self.snapshot())
            original = claim_path.read_bytes()
            for key, value in (('authorization_id', 'different-window'), ('old', 'c' * 40)):
                engine.save(claim_path, {'schema': 'ags.local-claim.v2', 'binding': {**self.request, key: value}})
                with self.subTest(directory=directory.name, field=key):
                    self.assert_readonly_completion_rejected()
                claim_path.write_bytes(original)
            with patch.object(self, 'probe', side_effect=AssertionError('no replay')):
                self.assertEqual(self.confirm(), record)

    def test_real_flock_concurrent_confirm(self):
        with manual.hostops(self.root):
            command = '''import json,sys
from pathlib import Path
sys.path.insert(0,sys.argv[1])
import upgrade_manual_approval as m
from upgrade import Failure
try:
 m.confirm(Path(sys.argv[2]),json.loads(sys.argv[3]),Path(sys.argv[4]),Path(sys.argv[5]),{},'x',lambda: {})
except Failure as e:
 print(e.code);sys.exit(e.exit_code)
'''
            result = subprocess.run([sys.executable, '-B', '-c', command, str(Path(__file__).parent),
                str(self.root), json.dumps(self.request), str(self.approval_path), str(self.packet_path)],
                capture_output=True)
            self.assertEqual(result.returncode, 73, result.stderr)
            self.assertEqual(result.stdout.strip(), b'lock_held')
        self.assertEqual(self.confirm()['status'], 'completed')

    def test_real_process_crash_after_commit_or_archive_only_completes_same_record(self):
        pid = os.fork()
        if pid == 0:
            manual.checkpoint = lambda name: os._exit(91) if name == 'completion-durable' else None
            self.confirm()
            os._exit(92)
        _, status = os.waitpid(pid, 0)
        self.assertEqual(os.waitstatus_to_exitcode(status), 91)
        expected, _ = manual.read_document(manual.completion_path(self.pending.parent, self.request))
        self.assertTrue(self.pending.exists())
        pid = os.fork()
        if pid == 0:
            manual.checkpoint = lambda name: os._exit(93) if name == 'claim-archived' else None
            self.confirm()
            os._exit(94)
        _, status = os.waitpid(pid, 0)
        self.assertEqual(os.waitstatus_to_exitcode(status), 93)
        with patch.object(self, 'probe', side_effect=AssertionError('durable readback only')):
            self.assertEqual(self.confirm(), expected)

    def test_two_real_confirm_processes_cannot_commit_concurrently(self):
        ready_r, ready_w = os.pipe()
        release_r, release_w = os.pipe()
        pid = os.fork()
        if pid == 0:
            os.close(ready_r); os.close(release_w)
            original = self.probe
            first = True
            def probe():
                nonlocal first
                if first:
                    os.write(ready_w, b'1'); os.read(release_r, 1); first = False
                return original()
            self.probe = probe
            try:
                self.confirm()
                os._exit(0)
            except BaseException:
                os._exit(95)
        os.close(ready_w); os.close(release_r)
        try:
            self.assertEqual(os.read(ready_r, 1), b'1')
            with self.assertRaises(upgrade.Failure) as error: self.confirm()
            self.assertEqual(error.exception.exit_code, 73)
        finally:
            os.write(release_w, b'1')
            os.close(ready_r); os.close(release_w)
            _, status = os.waitpid(pid, 0)
        self.assertEqual(os.waitstatus_to_exitcode(status), 0)
        self.assertEqual(self.confirm()['status'], 'completed')

    def test_executor_only_writes_pending(self):
        request_path = self.root / 'generated-pending.json'
        request = manual.write_request(request_path, self.packet, now=self.now)
        self.assertEqual(manual.read_document(request_path)[0]['decision'], 'pending')
        self.assertEqual(request['decision'], 'pending')
        self.approval_path.write_bytes(c.canonical(request)); self.reject()
        tree = ast.parse(Path(manual.__file__).read_text())
        for node in ast.walk(tree):
            if isinstance(node, ast.Dict):
                for key, value in zip(node.keys, node.values):
                    if isinstance(key, ast.Constant) and key.value == 'decision':
                        self.assertIsInstance(value, ast.Constant)
                        self.assertEqual(value.value, 'pending')
        self.assertNotIn('subprocess', Path(manual.__file__).read_text())
        # Across production upgrade modules, no literal decision=approve writer.
        for source in Path(__file__).parent.glob('upgrade*.py'):
            tree = ast.parse(source.read_text())
            for node in ast.walk(tree):
                if isinstance(node, ast.Dict):
                    for key, value in zip(node.keys, node.values):
                        if isinstance(key, ast.Constant) and key.value == 'decision':
                            self.assertFalse(isinstance(value, ast.Constant) and value.value == 'approve', source)
                if isinstance(node, ast.keyword) and node.arg == 'decision':
                    self.assertFalse(isinstance(node.value, ast.Constant) and node.value.value == 'approve', source)

    def make_handoff(self):
        # Carried files use canonical bytes, digest each received artifact exactly.
        self.packet_path.write_bytes(c.canonical(self.packet))
        self.packet_sha = c.fingerprint(self.packet)
        self.approval['evidence_packet_sha256'] = self.packet_sha
        self.approval_path.write_bytes(c.canonical(self.approval))
        record = self.confirm()
        return {'schema': 'ags.manual-mini-handoff.v1', 'mini_pin': self.pin,
            'mini_pin_sha256': self.pin_sha, 'imile_pin_sha256': '1' * 64,
            'mini_packet': self.packet, 'mini_evidence_package_sha256': self.packet_sha,
            'mini_completion': record, 'mini_completed_receipt_sha256': c.fingerprint(record),
            'mini_approval': self.approval, 'mini_approval_sha256': c.fingerprint(self.approval),
            'release_key': manual.release_key(self.pin), 'mini_generation': self.packet['generation'],
            'mini_health_observed_at': self.now.isoformat(), 'carried_by': 'fixture-human',
            'source': 'private-human-channel', 'received_at': self.now.isoformat(),
            'artifacts': {'pin': c.canonical(self.pin).decode(), 'packet': self.packet_path.read_text(),
                'approval': self.approval_path.read_text(), 'completion': c.canonical(record).decode()},
            'readback': {'confirmed': True, 'mini_generation': self.packet['generation'],
                'mini_completed_receipt_sha256': c.fingerprint(record),
                'mini_evidence_package_sha256': self.packet_sha}}

    def test_imile_requires_own_acceptance_and_matching_mini_package(self):
        handoff = self.make_handoff()
        handoff['imile_pin_sha256'] = self.pin_sha
        fixture = ManualFixture()
        fixture.host_name, fixture.carried_handoff = 'imile-win', handoff
        try:
            fixture.setUp()
            with self.assertRaises(upgrade.Failure) as error:
                manual.confirm(fixture.root, fixture.request, None, fixture.packet_path,
                               fixture.pin, fixture.pin_sha, fixture.probe)
            self.assertEqual(error.exception.exit_code, 86)
            own = fixture.approval
            engine.save(fixture.approval_path, self.approval)
            fixture.reject()
            engine.save(fixture.approval_path, own)
            self.assertEqual(fixture.confirm()['status'], 'completed')
        finally:
            fixture.doCleanups()

    def test_handoff_positive_and_each_field_negative(self):
        value = self.make_handoff(); key = manual.release_key(self.pin)
        manual.validate_handoff(value, key, '1' * 64, self.now)
        for field in value:
            for mutation in ('missing', 'wrong'):
                candidate = copy.deepcopy(value)
                if mutation == 'missing': del candidate[field]
                else: candidate[field] = None
                with self.subTest(field=field, mutation=mutation), self.assertRaises(
                        (c.ContractError, KeyError, TypeError, upgrade.Failure)):
                    manual.validate_handoff(candidate, key, '1' * 64, self.now)
        for field, bad in [('mini_generation', 'old'),
            ('imile_pin_sha256', '0' * 64), ('mini_completed_receipt_sha256', '0' * 64),
            ('mini_health_observed_at', (self.now - timedelta(minutes=6)).isoformat()),
            ('readback', {**value['readback'], 'confirmed': False}),
            ('mini_completion', {**value['mini_completion'], 'status': 'awaiting_observation'}),
            ('release_key', {**key, 'release_id': 999})]:
            with self.subTest(field=field), self.assertRaises(c.ContractError):
                manual.validate_handoff({**value, field: bad}, key, '1' * 64, self.now)


if __name__ == '__main__':
    unittest.main()
