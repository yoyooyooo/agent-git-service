#!/usr/bin/env python3
"""S5 synthetic host.main apply. Real gates, SQLite/Git, locks and S1-S4 validators.

Only identity/root provisioning, Release transport/smoke and health/manager are
fixture boundaries. No real AGS, launchd, provider, credential or production path.
"""
from contextlib import ExitStack, redirect_stdout
from datetime import datetime, timedelta, timezone
import copy
import fcntl
import hashlib
import io
import json
import os
from pathlib import Path
import signal
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import upgrade
import upgrade_backup_contract as c
import upgrade_engine as engine
import upgrade_host as host
import upgrade_manual_approval as manual
import upgrade_recovery as cold
import release_install_v2 as v2
from test_upgrade import authorization
from test_upgrade_quiescence import ColdFixture, FixtureManager
from test_upgrade_safety import pin_fixture

HERE = Path(__file__).resolve().parent
CAPABILITIES = {}


def snapshot(root):
    return [(str(p.relative_to(root)), p.stat().st_mode, p.stat().st_mtime_ns,
             p.read_bytes() if p.is_file() else None) for p in [root, *sorted(root.rglob('*'))]]


class Manager(FixtureManager):
    def start(self):
        self.starts += 1
        with (self.root / 'state/runtime.lock').open('r+') as runtime, \
                (self.root / 'state/install.lock').open('r+') as install:
            fcntl.flock(runtime, fcntl.LOCK_EX | fcntl.LOCK_NB)
            fcntl.flock(install, fcntl.LOCK_SH | fcntl.LOCK_NB)
            self.generation = cold.consume_start(self.root)
        self.cleanup()
        self.launch()
        record = {**self.record, 'build': {'revision': self.new_revision}}
        engine.save(self.root / 'state/runtime.json', record)


class WireFixture(ColdFixture):
    host_name = 'mini'

    def setUp(self):
        super().setUp()
        self.window.release_owner()
        # ColdFixture did not start a cold window. Move its setup-only claim
        # aside; the actual Transaction must create and fsync its own claim.
        self.pending.rename(self.root / 'fixture-unused-claim')
        self.manager.cleanup()
        self.manager = Manager(self.root)
        self.manager.new_revision = self.b['new']
        self.manager.launch()
        self.addCleanup(self.manager.cleanup)
        self.bundle = self.root / 'bundle'; self.bundle.mkdir(mode=0o700)
        self.pin = pin_fixture()
        self.pin['runtime'] = {'revision': self.b['new'], 'tree': self.b['runtime_tree']}
        self.pin['ci']['revision'] = self.b['new']
        for target, row in self.pin['targets'].items():
            row['provenance'].update(runtime_revision=self.b['new'], runtime_tree=self.b['runtime_tree'],
                                     control_revision=self.b['control_revision'], signer_digest=self.b['control_revision'],
                                     control_tree=self.b['control_tree'])
            for command, binary in row['binaries'].items():
                binary['identity'].update(revision=self.b['new'], tree=self.b['runtime_tree'])
                if command == 'gh-server':
                    binary['sha256'] = self.b['new_binary_sha256']
        self.package_path = self.root / 'config/host-package.json'
        self.entry = {'root': str(self.root), 'readyz': 'http://127.0.0.1:9/readyz',
                      'service_readyz': 'http://127.0.0.1:9/service/readyz',
                      'legacy_check': ['/usr/bin/false'], 'host_package': str(self.package_path)}
        self.policy = {'schema': 'ags.upgrade-policy.v2', 'deployments': {self.host_name: self.entry},
                       'migration_admissions': [{'old': self.b['old'], 'new': self.b['new'], 'migrations': []}]}
        engine.save(self.bundle / 'upgrade_policy.json', self.policy)
        engine.save(self.bundle / 'release-source.json', {'schema': 'ags.release-source.v1',
                    'version': self.pin['version'], 'runtime': self.pin['runtime']})
        # Exact-digest bundle contains all actual implementation modules. The
        # sole private adapter is a fixed fixture factory, never a success argv.
        for name in ('upgrade.py', 'upgrade_host.py', 'upgrade_engine.py', 'upgrade_wireup.py',
                     'upgrade_backup_contract.py', 'upgrade_inventory.py', 'upgrade_backup.py',
                     'upgrade_restore_verify.py', 'upgrade_recovery.py', 'upgrade_manual_approval.py',
                     'release_verify.py', 'release_contract.py', 'release_install_v2.py',
                     'install-release.py', 'upgrade_smoke.py'):
            (self.bundle / name).write_bytes((HERE / name).read_bytes())
        self.adapter = self.bundle / 'upgrade_adapter_fixture.py'
        self.adapter.write_text("SCHEMA = 'ags.upgrade-host-adapter.v1'\ndef create(root, package):\n"
                                " import sys\n return sys.modules['__main__'].CAPABILITIES[str(root)]\n")
        for p in self.bundle.iterdir(): p.chmod(0o600)
        manifest = {'schema': 'ags.upgrade-tool-bundle.v2', 'entry': 'upgrade_host.py',
                    'files': {p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in self.bundle.iterdir()}}
        engine.save(self.bundle / 'manifest.json', manifest)
        self.pin['verification_policy'].update(
            policy_sha256=hashlib.sha256((self.bundle / 'upgrade_policy.json').read_bytes()).hexdigest(),
            tool_bundle_manifest_sha256=hashlib.sha256((self.bundle / 'manifest.json').read_bytes()).hexdigest())
        engine.save(self.root / 'config/ags-deploy-release-pin.json', self.pin)
        self.pin_sha = hashlib.sha256((self.root / 'config/ags-deploy-release-pin.json').read_bytes()).hexdigest()
        self.request = {'host': self.host_name, 'deployment_id': self.b['deployment_id'], 'attempt_id': '1.1',
                        'authorization_id': self.b['authorization_id'], 'old': self.b['old'], 'new': self.b['new'],
                        'runtime_tree': self.b['runtime_tree'], 'release_id': self.pin['release_id'],
                        'engine_sha256': self.pin['verification_policy']['tool_bundle_manifest_sha256'],
                        'policy_sha256': self.pin['verification_policy']['policy_sha256'], 'pin_sha256': self.pin_sha}
        self.auth = authorization(self.request); self.auth['authorization_id'] = self.request['authorization_id']
        engine.save(self.root / 'config/upgrade-authorization-window-auth.json', self.auth)
        self.group['objects'].append({'id': 'private_adapter', 'role': 'tool', 'source': str(self.adapter),
                                     'restore_target': str(self.adapter), 'kind': 'file', 'depends_on': []})
        self.package = {'schema': 'ags.private-host-package.v1', 'host': self.host_name,
                        'host_instance': 'synthetic-mini', 'deployment_id': self.request['deployment_id'],
                        'transaction_id': 'transaction-window-auth-1', 'authorization_id': 'window-auth',
                        'pin_sha256': self.pin_sha, 'dsn': 'sqlite:' + str(self.storage.dbpath),
                        'schema_sha256': self.storage.admission, 'recovery_group': self.group,
                        'adapter_module': self.adapter.name, 'mini_handoff': None}
        engine.save(self.package_path, self.package)
        self.auth['evidence_sha256'] = hashlib.sha256(self.package_path.read_bytes()).hexdigest()
        engine.save(self.root / 'config/upgrade-authorization-window-auth.json', self.auth)
        CAPABILITIES[str(self.root)] = {'manager': self.manager, 'writer_probe': self.writer_probe, 'trash': self.trash}
        self.addCleanup(lambda: CAPABILITIES.pop(str(self.root), None))
        self.target = 'darwin_arm64' if self.host_name == 'mini' else 'linux_amd64'
        self.identity = self.pin['targets'][self.target]['binaries']['gh-server']['identity']
        self.effects_seen = []

    def writer_probe(self):
        return {'generation': cold.read_window(self.root)[0]['generation'], 'closed': True,
                'writers': [], 'unknown': []}

    def probe(self, *args):
        runtime, _ = upgrade.private_json(self.root / 'state/runtime.json')
        return {'generation': runtime['generation'], 'running_revision': self.request['new'],
                'local': {'status': 'ready', 'revision': self.request['new']},
                'service': {'status': 'ready', 'revision': self.request['new']}}

    def preflight(self, effects):
        # Release/CI/ABI/smoke are separately exercised by the existing owning
        # suites. Inert bytes never run as a binary in this synthetic E2E.
        effects.candidate = self.root / 'fixture-candidate'
        effects.candidate.write_bytes(self.new); effects.candidate.chmod(0o700)
        effects.info = {'source': self.request['new'], 'runtime_tree': self.request['runtime_tree'],
                        'control_revision': self.b['control_revision'], 'version': self.pin['version'],
                        'release_id': self.pin['release_id'], 'attestation': {},
                        'binaries': self.pin['targets'][self.target]['binaries']}

    def invoke(self, command='apply', **kwargs):
        args = SimpleNamespace(command=command, deployment_id=self.request['deployment_id'],
                               pin_sha256=self.pin_sha, ci_run_id='1', ci_run_attempt='1',
                               authorization_id=self.request['authorization_id'],
                               approval_file=None, evidence_packet=None)
        for key, value in kwargs.items(): setattr(args, key, value)
        original_bundle = upgrade.bundle_engine
        original_stat = Path.stat
        def fixture_stat(path, *a, **kw):
            info = original_stat(path, *a, **kw)
            if path in self.bundle.parents:
                values = list(info); values[4] = os.getuid(); return os.stat_result(values)
            return info
        def check_bundle(bundle, digest):
            with patch.object(Path, 'stat', fixture_stat):
                return original_bundle(bundle, digest, owner=os.getuid())
        with ExitStack() as stack:
            stack.enter_context(patch.object(host, 'BUNDLE', self.bundle))
            stack.enter_context(patch.object(host, 'execution_identity', return_value=self.root))
            stack.enter_context(patch.object(host.socket, 'gethostname', return_value=self.request['host']))
            stack.enter_context(patch.object(host, 'bundle_engine', side_effect=check_bundle))
            stack.enter_context(patch.object(host.argparse.ArgumentParser, 'parse_args', return_value=args))
            stack.enter_context(patch.object(host.Effects, 'preflight', lambda effects: self.preflight(effects)))
            stack.enter_context(patch.object(host, 'acceptance_health', self.probe))
            original_child = host.child
            stack.enter_context(patch.object(host, 'child', side_effect=lambda argv, phase, **kw:
                json.dumps(self.identity).encode() if phase == 'installed' else original_child(argv, phase, **kw)))
            output = stack.enter_context(redirect_stdout(io.StringIO()))
            code = host.main()
            return code, json.loads(output.getvalue())

    def accept(self):
        now = datetime.now(timezone.utc)
        runtime, _ = upgrade.private_json(self.root / 'state/runtime.json')
        observation = {'generation': runtime['generation'], 'started_at': runtime['started_at'],
                       'ended_at': now.isoformat(), 'reports': {key: {'status': 'pass', 'sha256': 'a' * 64}
                         for key in ('business', 'git', 'provider', 'recovery')}, 'unresolved_blockers': []}
        packet = manual.make_packet(self.root, self.request, self.pin, self.pin_sha, observation, self.probe, handoff=self.package['mini_handoff'])
        packet_path = self.root / 'packet-s5.json'; manual.write_packet(packet_path, packet)
        approval_path = self.root / 'approval-s5.json'
        approval = manual.write_request(approval_path, packet)
        approval.update(approval_id='human-s5-accept', decision='approve',
                        actor={'type': 'human', 'id': 'fixture-operator', 'source': 'fixture-private-record'},
                        authority_reference=self.request['authorization_id'],
                        expires_at=(now + timedelta(minutes=5)).isoformat(),
                        independent_media={'status': 'omitted', 'evidence': 'fixture-only', 'reason': 'synthetic'})
        engine.save(approval_path, approval)
        return self.invoke('confirm', approval_file=approval_path, evidence_packet=packet_path)


class WireupTests(WireFixture):
    def test_real_gate_apply_86_then_separate_human_confirm_0(self):
        phases = []
        with patch.object(cold, 'checkpoint', side_effect=phases.append):
            code, waiting = self.invoke()
        expected = ['cold-marker', 'stop-intent', 'quiescent', 'backup', 'restore_verified',
                    'install_started', 'installed', 'start_attempted', 'manager-start-intent',
                    'start-token-consumed']
        self.assertEqual([phase for phase in phases if phase in expected], expected)
        self.assertEqual((code, waiting['status']), (86, 'awaiting_observation'))
        self.assertEqual(self.binary.read_bytes(), self.new)
        self.assertEqual(self.manager.starts, 1)
        code, replay = self.invoke()
        self.assertEqual((code, replay), (86, waiting))
        self.assertEqual(self.manager.starts, 1)
        with self.assertRaises(upgrade.Failure) as missing: self.invoke('confirm')
        self.assertEqual(missing.exception.exit_code, 86)
        code, accepted = self.accept()
        self.assertEqual((code, accepted['status']), (0, 'completed'))
        self.assertFalse(self.pending.exists())
        self.assertEqual(self.manager.starts, 1)

    def test_valid_pin_bundle_with_public_policy_host_dispatch_is_83(self):
        policy = {'schema': 'ags.upgrade-policy.v2', 'deployments': {}, 'migration_admissions': []}
        engine.save(self.bundle / 'upgrade_policy.json', policy)
        self.pin['verification_policy']['policy_sha256'] = hashlib.sha256(
            (self.bundle / 'upgrade_policy.json').read_bytes()).hexdigest()
        manifest = json.loads((self.bundle / 'manifest.json').read_bytes())
        manifest['files']['upgrade_policy.json'] = self.pin['verification_policy']['policy_sha256']
        engine.save(self.bundle / 'manifest.json', manifest)
        self.pin['verification_policy']['tool_bundle_manifest_sha256'] = hashlib.sha256(
            (self.bundle / 'manifest.json').read_bytes()).hexdigest()
        engine.save(self.root / 'config/ags-deploy-release-pin.json', self.pin)
        self.pin_sha = hashlib.sha256((self.root / 'config/ags-deploy-release-pin.json').read_bytes()).hexdigest()
        before = snapshot(self.root)
        with self.assertRaises(upgrade.Failure) as error: self.invoke()
        self.assertEqual((error.exception.code, error.exception.exit_code), ('backup_contract_unimplemented', 83))
        self.assertEqual(snapshot(self.root), before)
        self.assertFalse(self.pending.exists())

    def test_each_package_field_missing_is_83_no_claim_or_service_effect(self):
        raw = self.package_path.read_bytes()
        for key in self.package:
            bad = copy.deepcopy(self.package); del bad[key]
            engine.save(self.package_path, bad)
            before = snapshot(self.root)
            with self.subTest(key=key), self.assertRaises(upgrade.Failure) as caught: self.invoke()
            self.assertEqual(caught.exception.exit_code, 83)
            self.assertEqual(snapshot(self.root), before)
            self.assertFalse(self.pending.exists())
            self.assertEqual(self.manager.starts, 0)
        self.package_path.write_bytes(raw)

    def test_default_public_apply_backup_transaction_and_install_stay_83(self):
        before = snapshot(self.root)
        with self.assertRaises(upgrade.Failure) as caught:
            engine.Transaction(self.pending.parent, self.request, self.auth, object()).run()
        self.assertEqual((caught.exception.code, caught.exception.exit_code), ('backup_contract_unimplemented', 83))
        effects = object.__new__(host.Effects); effects.binding = self.request
        with self.assertRaises(upgrade.Failure) as backup: effects.backup()
        self.assertEqual(backup.exception.exit_code, 83)
        with self.assertRaises(v2.InstallClosed) as direct: v2.require_install_contracts()
        self.assertEqual((str(direct.exception), direct.exception.exit_code), ('install_contracts_unimplemented', 83))
        self.assertEqual(snapshot(self.root), before)

    def test_unsafe_package_and_wrong_schema_fail_before_stop(self):
        for mutate in (lambda p: p.update(schema_sha256='0' * 64),
                       lambda p: p.update(pin_sha256='0' * 64),
                       lambda p: p.update(adapter_module='true'),
                       lambda p: p['recovery_group']['objects'].clear()):
            bad = copy.deepcopy(self.package); mutate(bad); engine.save(self.package_path, bad)
            changed_auth = {**self.auth, 'evidence_sha256': hashlib.sha256(self.package_path.read_bytes()).hexdigest()}
            engine.save(self.root / 'config/upgrade-authorization-window-auth.json', changed_auth)
            with self.assertRaises(upgrade.Failure) as caught: self.invoke()
            self.assertEqual(caught.exception.exit_code, 83)
            self.assertFalse(self.pending.exists())
        engine.save(self.package_path, self.package)
        engine.save(self.root / 'config/upgrade-authorization-window-auth.json', self.auth)
        self.package_path.chmod(0o644)
        with self.assertRaises(upgrade.Failure): self.invoke()
        self.assertFalse(self.pending.exists())

    def test_private_policy_closed_fields_and_required_recovery_roles(self):
        from upgrade_wireup import validate_policy
        validate_policy(self.policy)
        for key in self.policy:
            bad = copy.deepcopy(self.policy); del bad[key]
            with self.subTest(policy=key), self.assertRaises(c.ContractError): validate_policy(bad)
        for key in self.entry:
            bad = copy.deepcopy(self.policy); del bad['deployments']['mini'][key]
            with self.subTest(entry=key), self.assertRaises(c.ContractError): validate_policy(bad)
        for key in ('enabled', 'skip_backup', 'force'):
            bad = copy.deepcopy(self.policy); bad['deployments']['mini'][key] = True
            with self.subTest(flag=key), self.assertRaises(c.ContractError): validate_policy(bad)
        for role in sorted(c.REQUIRED_ROLES):
            bad = copy.deepcopy(self.package)
            bad['recovery_group']['objects'] = [v for v in bad['recovery_group']['objects'] if v['role'] != role]
            engine.save(self.package_path, bad)
            engine.save(self.root / 'config/upgrade-authorization-window-auth.json', {
                **self.auth, 'evidence_sha256': hashlib.sha256(self.package_path.read_bytes()).hexdigest()})
            before = snapshot(self.root)
            with self.subTest(role=role), self.assertRaises(upgrade.Failure) as denied: self.invoke()
            self.assertEqual(denied.exception.exit_code, 83)
            self.assertEqual(snapshot(self.root), before)
            self.assertFalse(self.pending.exists())

    def test_live_health_accepts_consumed_restart_fence_but_no_other_failed_check(self):
        self.assertEqual(self.invoke()[0], 86)
        now = datetime.now(timezone.utc).isoformat()
        checks = {'upgrade_cold_window': 'blocked', 'binary': 'ok', 'installation': 'ok'}
        path = self.root / 'state/startup-health.json'
        engine.save(path, {'schema': 'ags.startup-health.v1', 'checked_at': now,
                           'ready_to_start': False, 'checks': checks, 'check_details': {}})
        class Opener:
            def open(inner, url, timeout):
                return io.StringIO(json.dumps({'status': 'ready', 'version': self.request['new']}))
        with patch.object(host.urllib.request, 'build_opener', return_value=Opener()):
            result = host.acceptance_health(self.root, self.entry, self.request)
            self.assertEqual(result['running_revision'], self.request['new'])
            for bad in ({**checks, 'binary': 'identity_drift'},
                        {**checks, 'upgrade_cold_window': 'unknown'}, {}):
                engine.save(path, {'schema': 'ags.startup-health.v1', 'checked_at': now,
                                   'ready_to_start': False, 'checks': bad})
                with self.subTest(checks=bad), self.assertRaises(upgrade.Failure):
                    host.acceptance_health(self.root, self.entry, self.request)

    def test_two_logical_hosts_require_completed_mini_and_own_imile_approval(self):
        imile = WireFixture()
        imile.host_name = 'imile-win'
        imile.setUp()
        try:
            before = snapshot(imile.root)
            with self.assertRaises(upgrade.Failure) as incomplete: imile.invoke()
            self.assertEqual(incomplete.exception.exit_code, 83)
            self.assertEqual(snapshot(imile.root), before)
            self.assertFalse(imile.pending.exists())
            self.assertEqual(self.invoke()[0], 86)
            self.assertEqual(self.accept()[0], 0)
            packet, _ = manual.read_document(self.root / 'packet-s5.json')
            approval, _ = manual.read_document(self.root / 'approval-s5.json')
            record, _ = manual.read_document(manual.completion_path(self.pending.parent, self.request))
            now = datetime.now(timezone.utc).isoformat()
            handoff = {'schema': 'ags.manual-mini-handoff.v1', 'mini_pin': self.pin,
                'mini_pin_sha256': self.pin_sha, 'imile_pin_sha256': imile.pin_sha,
                'mini_packet': packet, 'mini_evidence_package_sha256': c.fingerprint(packet),
                'mini_completion': record, 'mini_completed_receipt_sha256': c.fingerprint(record),
                'mini_approval': approval, 'mini_approval_sha256': c.fingerprint(approval),
                'release_key': manual.release_key(self.pin), 'mini_generation': packet['generation'],
                'mini_health_observed_at': now, 'carried_by': 'fixture-human', 'source': 'private-fixture',
                'received_at': now, 'artifacts': {
                    'pin': (self.root / 'config/ags-deploy-release-pin.json').read_text(),
                    'packet': c.canonical(packet).decode(),
                    'approval': (self.root / 'approval-s5.json').read_text(),
                    'completion': c.canonical(record).decode()},
                'readback': {'confirmed': True, 'mini_generation': packet['generation'],
                    'mini_completed_receipt_sha256': c.fingerprint(record),
                    'mini_evidence_package_sha256': c.fingerprint(packet)}}
            # Raw approval was written by save, not canonical; retain its digest.
            handoff['mini_approval_sha256'] = hashlib.sha256(handoff['artifacts']['approval'].encode()).hexdigest()
            self.assertNotEqual(self.pin_sha, imile.pin_sha)
            for change in (
                    {'mini_completion': {**record, 'status': 'awaiting_observation'}},
                    {'imile_pin_sha256': self.pin_sha},
                    {'release_key': {**handoff['release_key'], 'release_id': 999}},
                    {'mini_generation': 'old-generation'},
                    {'mini_health_observed_at': '2000-01-01T00:00:00+00:00'}):
                imile.package['mini_handoff'] = {**handoff, **change}
                engine.save(imile.package_path, imile.package)
                imile.auth['evidence_sha256'] = hashlib.sha256(imile.package_path.read_bytes()).hexdigest()
                engine.save(imile.root / 'config/upgrade-authorization-window-auth.json', imile.auth)
                before = snapshot(imile.root)
                with self.subTest(change=next(iter(change))), self.assertRaises(upgrade.Failure) as denied:
                    imile.invoke()
                self.assertEqual(denied.exception.exit_code, 83)
                self.assertEqual(snapshot(imile.root), before)
                self.assertFalse(imile.pending.exists())
            imile.package['mini_handoff'] = handoff
            engine.save(imile.package_path, imile.package)
            imile.auth['evidence_sha256'] = hashlib.sha256(imile.package_path.read_bytes()).hexdigest()
            engine.save(imile.root / 'config/upgrade-authorization-window-auth.json', imile.auth)
            self.assertEqual(imile.invoke()[0], 86)
            with self.assertRaises(upgrade.Failure) as missing: imile.invoke('confirm')
            self.assertEqual(missing.exception.exit_code, 86)
            # A completed mini approval is not an imile approval.
            with self.assertRaises(upgrade.Failure) as wrong:
                imile.invoke('confirm', approval_file=self.root / 'approval-s5.json',
                             evidence_packet=self.root / 'packet-s5.json')
            self.assertEqual(wrong.exception.exit_code, 77)
            self.assertEqual(imile.accept()[0], 0)
            self.assertEqual(imile.manager.starts, 1)
        finally:
            imile.doCleanups()

    def test_seal_restore_journal_or_consumption_cannot_be_replaced_by_success(self):
        self.assertEqual(self.invoke()[0], 86)
        _, journal, _ = cold.read_window(self.root)
        paths = (Path(journal['backup_dir']) / 'manifest.json',
                 Path(journal['backup_dir']) / 'restore-report.json',
                 self.pending / 'cold-journal.json')
        for path in paths:
            raw = path.read_bytes()
            info = path.stat()
            engine.save(path, {'status': 'success'})
            with self.subTest(path=path.name):
                with self.assertRaises((upgrade.Failure, c.ContractError, KeyError)):
                    self.invoke()
                self.assertEqual(self.binary.read_bytes(), self.new)
                self.assertEqual(self.manager.starts, 1)
            path.write_bytes(raw); os.utime(path, ns=(info.st_atime_ns, info.st_mtime_ns))
        raw = (self.pending / 'cold-journal.json').read_bytes()
        bad = copy.deepcopy(journal); bad['handoff']['consumed'] = False
        engine.save(self.pending / 'cold-journal.json', bad)
        with self.assertRaises((upgrade.Failure, c.ContractError)):
            self.invoke()
        self.assertEqual(self.manager.starts, 1)
        (self.pending / 'cold-journal.json').write_bytes(raw)

    def test_real_apply_crash_each_phase_is_readback_only_on_reentry(self):
        points = ('admission', 'prepare', 'cold-marker', 'stop-intent', 'quiescent', 'backup',
                  'restore_verified', 'install_started', 'candidate-file-sync', 'binary-renamed',
                  'receipt-durable', 'installed', 'start_attempted', 'manager-start-intent',
                  'start-token-consumed', 'health', 'awaiting_observation')
        for point in points:
            with self.subTest(point=point):
                if point != points[0]: self.doCleanups(); self.setUp()
                self.manager.cleanup()
                pid = os.fork()
                if pid == 0:
                    try:
                        self.manager = Manager(self.root)
                        self.manager.new_revision = self.request['new']
                        self.manager.launch()
                        CAPABILITIES[str(self.root)]['manager'] = self.manager
                        def crash(name):
                            if name == point: os._exit(91)
                        cold.checkpoint = crash
                        host.cold_checkpoint = crash
                        original = engine.Transaction.checkpoint
                        def transaction_checkpoint(tx, phase, **kw):
                            original(tx, phase, **kw); crash(phase)
                        engine.Transaction.checkpoint = transaction_checkpoint
                        self.invoke()
                        os._exit(92)
                    except BaseException:
                        import traceback
                        traceback.print_exc()
                        os._exit(93)
                _, status = os.waitpid(pid, 0)
                self.assertEqual(os.waitstatus_to_exitcode(status), 91)
                before = snapshot(self.root)
                try:
                    result = self.invoke()
                except (upgrade.Failure, c.ContractError) as error:
                    self.assertIn(getattr(error, 'exit_code', 83), (73, 81, 82, 83))
                else:
                    self.assertEqual((point, result[0]), ('awaiting_observation', 86))
                self.assertEqual(snapshot(self.root), before)
                self.assertEqual(self.manager.starts, 0)
                runtime, _ = upgrade.private_json(self.root / 'state/runtime.json')
                # This PID was launched solely by this test's child, never a
                # discovered host service. Reap its isolated process group.
                if 'stopped_at' not in runtime:
                    try: os.killpg(runtime['pid'], signal.SIGTERM)
                    except ProcessLookupError: pass

    def test_real_apply_stage_contract_failures_keep_83_and_never_install(self):
        cases = ('stop', 'writers', 'manifest', 'restore', 'source', 'authorization')
        for case in cases:
            with self.subTest(case=case):
                if case != cases[0]: self.doCleanups(); self.setUp()
                if case == 'stop':
                    original_stop = self.manager.stop
                    def invalid_stop():
                        record = original_stop(); record['forced_kill'] = True; return record
                    self.manager.stop = invalid_stop
                if case == 'writers':
                    CAPABILITIES[str(self.root)]['writer_probe'] = lambda: {'unknown': ['writer']}
                original = cold.checkpoint
                def tamper(name):
                    if case == 'manifest' and name == 'backup':
                        _, journal, _ = cold.read_window(self.root)
                        path = Path(journal['backup_dir']) / 'manifest.json'
                        path.write_bytes(b'{}')
                    if case == 'restore' and name == 'restore_verified':
                        _, journal, _ = cold.read_window(self.root)
                        path = Path(journal['backup_dir']) / 'restore-report.json'
                        path.write_bytes(b'{}')
                    if case == 'source' and name == 'restore_verified':
                        (self.root / 'runtime_env').write_bytes(b'changed')
                    if case == 'authorization' and name == 'restore_verified':
                        path = self.root / 'config/upgrade-authorization-window-auth.json'
                        engine.save(path, {**self.auth, 'expires_at': '2000-01-01T00:00:00+00:00'})
                    return original(name)
                with patch.object(cold, 'checkpoint', tamper):
                    with self.assertRaises(upgrade.Failure) as error: self.invoke()
                self.assertEqual(error.exception.exit_code, 83)
                journal, _ = upgrade.private_json(self.pending / 'journal.json')
                self.assertEqual(journal['service_state'], 'stopped_or_unknown')
                self.assertTrue(journal['recovery_required'])
                self.assertEqual(self.binary.read_bytes(), self.old)
                self.assertEqual(self.manager.starts, 0)

    def assert_stage_authorization_refused(self, *, starting):
        journal, _ = upgrade.private_json(self.pending / 'journal.json')
        self.assertEqual(journal['phase'], 'start_attempted' if starting else 'stop')
        self.assertEqual(journal['service_state'], 'stopped_or_unknown')
        self.assertTrue(journal['recovery_required'])
        self.assertTrue(journal['stop_started'])
        self.assertEqual(journal['start_attempted'], starting)
        marker, window, _ = cold.read_window(self.root)
        self.assertEqual(window['start_attempted'], starting)
        self.assertEqual(marker['generation'], window['generation'])
        if starting:
            self.assertFalse(window['handoff']['consumed'])
        self.assertEqual(self.manager.starts, 0)
        self.assertEqual(self.binary.read_bytes(), self.new if starting else self.old)
        before = snapshot(self.root)
        with self.assertRaises(upgrade.Failure) as replay: self.invoke()
        self.assertIn(replay.exception.exit_code, (77, 81))
        self.assertEqual(snapshot(self.root), before)
        self.assertEqual(self.manager.starts, 0)

    def test_authorization_revoked_after_begin_before_stop(self):
        def revoke(phase):
            if phase == 'stop-intent':
                engine.save(self.root / 'config/upgrade-authorization-window-auth.json',
                            {**self.auth, 'decision': 'reject'})
        with patch.object(self.manager, 'stop', wraps=self.manager.stop) as stop, \
                patch.object(cold, 'checkpoint', revoke):
            with self.assertRaises(upgrade.Failure) as error: self.invoke()
        self.assertEqual(error.exception.exit_code, 83)
        self.assertEqual(stop.call_count, 0)
        self.assert_stage_authorization_refused(starting=False)

    def test_authorization_revoked_after_start_intent_before_start(self):
        original = engine.Transaction.checkpoint
        def revoke(tx, phase, **kw):
            original(tx, phase, **kw)
            if phase == 'start_attempted':
                engine.save(self.root / 'config/upgrade-authorization-window-auth.json',
                            {**self.auth, 'decision': 'reject'})
        with patch.object(self.manager, 'start', wraps=self.manager.start) as start, \
                patch.object(engine.Transaction, 'checkpoint', revoke):
            with self.assertRaises(upgrade.Failure) as error: self.invoke()
        self.assertEqual(error.exception.exit_code, 83)
        self.assertEqual(start.call_count, 0)
        self.assert_stage_authorization_refused(starting=True)

    def test_window_expires_after_start_intent_before_manager_start(self):
        original = engine.Transaction.checkpoint
        expired = datetime.fromisoformat(self.auth['expires_at']) + timedelta(seconds=1)
        class LateClock(datetime):
            @classmethod
            def now(cls, tz=None): return expired
        with ExitStack() as stack:
            def delay(tx, phase, **kw):
                original(tx, phase, **kw)
                if phase == 'start_attempted':
                    stack.enter_context(patch.object(engine, 'datetime', LateClock))
            start = stack.enter_context(patch.object(self.manager, 'start', wraps=self.manager.start))
            stack.enter_context(patch.object(engine.Transaction, 'checkpoint', delay))
            with self.assertRaises(upgrade.Failure) as error: self.invoke()
        self.assertEqual(error.exception.exit_code, 83)
        self.assertEqual(start.call_count, 0)
        self.assert_stage_authorization_refused(starting=True)

    def test_authorization_revoked_by_restart_suppression_before_stop(self):
        original = self.manager.suppress_restart
        def revoke():
            original()
            engine.save(self.root / 'config/upgrade-authorization-window-auth.json',
                        {**self.auth, 'decision': 'reject'})
        with patch.object(self.manager, 'suppress_restart', revoke), \
                patch.object(self.manager, 'stop', wraps=self.manager.stop) as stop:
            with self.assertRaises(upgrade.Failure) as error: self.invoke()
        self.assertEqual(error.exception.exit_code, 83)
        self.assertEqual(stop.call_count, 0)
        self.assert_stage_authorization_refused(starting=False)

    def test_authorization_revoked_after_manager_start_intent(self):
        def revoke(phase):
            if phase == 'manager-start-intent':
                engine.save(self.root / 'config/upgrade-authorization-window-auth.json',
                            {**self.auth, 'decision': 'reject'})
        with patch.object(cold, 'checkpoint', revoke), \
                patch.object(self.manager, 'start', wraps=self.manager.start) as start:
            with self.assertRaises(upgrade.Failure) as error: self.invoke()
        self.assertEqual(error.exception.exit_code, 83)
        self.assertEqual(start.call_count, 0)
        self.assert_stage_authorization_refused(starting=True)

    def stage_authorization_read_failure(self, invalidate):
        def tamper(phase):
            if phase == 'quiescent':
                invalidate(self.root / 'config/upgrade-authorization-window-auth.json')
        with patch.object(cold, 'checkpoint', tamper):
            with self.assertRaises(upgrade.Failure) as error: self.invoke()
        self.assertEqual((error.exception.code, error.exception.exit_code),
                         ('minimum_contract_unsatisfied', 83))
        journal, _ = upgrade.private_json(self.pending / 'journal.json')
        self.assertEqual(journal['phase'], 'quiescent')
        self.assertEqual(journal['service_state'], 'stopped_or_unknown')
        self.assertTrue(journal['recovery_required'])
        self.assertFalse(journal['install_started'])
        self.assertFalse(journal['start_attempted'])
        self.assertEqual(self.binary.read_bytes(), self.old)
        self.assertEqual(self.manager.starts, 0)

    def test_stage_authorization_unsafe_permissions_keep_83(self):
        self.stage_authorization_read_failure(lambda path: path.chmod(0o644))

    def test_stage_authorization_unreadable_keeps_83(self):
        self.stage_authorization_read_failure(lambda path: path.chmod(0o000))

    def test_stage_authorization_missing_keeps_83(self):
        self.stage_authorization_read_failure(lambda path: path.rename(self.root / 'retained-authorization.json'))

    def test_stage_authorization_malformed_keeps_83(self):
        self.stage_authorization_read_failure(lambda path: path.write_text('{broken'))

    def stage_package_read_failure(self, point, *, unreadable=False):
        reached = []
        retained = self.root / 'retained-host-package.json'
        def tamper(phase):
            if phase == point:
                reached.append(phase)
                if unreadable:
                    self.package_path.chmod(0o000)
                else:
                    self.package_path.rename(retained)
        with patch.object(cold, 'checkpoint', tamper):
            with self.assertRaises(upgrade.Failure) as error: self.invoke()
        self.assertEqual(reached, [point])
        journal, _ = upgrade.private_json(self.pending / 'journal.json')
        starting = point == 'manager-start-intent'
        self.assertEqual(journal['phase'], 'start_attempted' if starting else 'quiescent')
        self.assertEqual(journal['service_state'], 'stopped_or_unknown')
        self.assertTrue(journal['recovery_required'])
        self.assertTrue(journal['stop_started'])
        self.assertEqual(journal['install_started'], starting)
        self.assertEqual(journal['start_attempted'], starting)
        marker, window, _ = cold.read_window(self.root)
        self.assertEqual(marker['generation'], window['generation'])
        self.assertEqual(window['phase'], 'start_attempted' if starting else 'quiescent')
        self.assertEqual(window['start_attempted'], starting)
        if starting:
            self.assertTrue(window['handoff']['manager_requested'])
            self.assertFalse(window['handoff']['consumed'])
            self.assertRegex(window['handoff']['token'], r'^[0-9a-f]{32}$')
        self.assertEqual(self.manager.starts, 0)
        self.assertEqual(self.binary.read_bytes(), self.new if starting else self.old)
        # Snapshot the package's unreadable state without trying to read it.
        if unreadable:
            self.assertEqual(self.package_path.stat().st_mode & 0o777, 0)
            self.package_path.chmod(0o600)
            before = snapshot(self.root)
            self.package_path.chmod(0o000)
        else:
            before = snapshot(self.root)
        with self.assertRaises(upgrade.Failure) as replay: self.invoke()
        self.assertEqual(replay.exception.exit_code, 83)
        if unreadable:
            self.assertEqual(self.package_path.stat().st_mode & 0o777, 0)
            self.package_path.chmod(0o600)
        self.assertEqual(snapshot(self.root), before)
        self.assertEqual(self.manager.starts, 0)
        # Restoring the contract must not authorize replay of the failed attempt.
        if not unreadable:
            retained.rename(self.package_path)
        before = snapshot(self.root)
        with self.assertRaises(upgrade.Failure) as repaired_replay: self.invoke()
        self.assertEqual(repaired_replay.exception.exit_code, 81)
        self.assertEqual(snapshot(self.root), before)
        self.assertEqual(self.manager.starts, 0)
        # Classification is checked after the durable fences and no-replay.
        self.assertEqual((error.exception.code, error.exception.exit_code),
                         ('minimum_contract_unsatisfied', 83))

    def test_package_missing_after_quiescent_keeps_83(self):
        self.stage_package_read_failure('quiescent')

    def test_package_missing_after_manager_start_intent_keeps_83(self):
        self.stage_package_read_failure('manager-start-intent')

    def test_package_unreadable_after_quiescent_keeps_83(self):
        self.stage_package_read_failure('quiescent', unreadable=True)

    def test_package_unreadable_after_manager_start_intent_keeps_83(self):
        self.stage_package_read_failure('manager-start-intent', unreadable=True)

    def test_real_apply_prestart_recovery_and_poststart_refusal(self):
        def interrupted(name):
            if name == 'installed': raise RuntimeError('fixture lost response')
        with patch.object(cold, 'checkpoint', interrupted):
            with self.assertRaises(upgrade.Failure): self.invoke()
        marker, journal, _ = cold.read_window(self.root)
        window = cold.ColdWindow(self.root, self.request, journal['backup_binding'], self.group,
                                 self.package['dsn'], self.package['schema_sha256'], self.manager,
                                 self.writer_probe, self.trash)
        window.acquire_owner(); self.addCleanup(window.release_owner)
        window.acquire(); self.addCleanup(window.release)
        now = datetime.now(timezone.utc)
        approval = {'schema': 'ags.prestart-recovery-approval.v1', 'approval_id': 'fresh-recovery',
                    'purpose': 'prestart-recover', 'decision': 'approve', 'binding': self.request,
                    'generation': marker['generation'], 'journal_sha256': hashlib.sha256(
                        (self.pending / 'cold-journal.json').read_bytes()).hexdigest(),
                    'issued_at': (now - timedelta(seconds=1)).isoformat(),
                    'expires_at': (now + timedelta(minutes=5)).isoformat(),
                    'actor': {'type': 'human', 'id': 'fixture-operator', 'source': 'private-record'}}
        path = self.root / 'recovery-approval.json'; engine.save(path, approval)
        restored = cold.recover_prestart(window, path)
        self.assertEqual(restored['status'], 'old_restored')
        self.assertEqual(self.binary.read_bytes(), self.old)
        self.assertEqual((self.root / 'state/installation.json').read_bytes(), self.raw_old_receipt)
        self.assertEqual(self.manager.starts, 0)
        self.doCleanups(); self.setUp()
        self.invoke()
        _, journal, _ = cold.read_window(self.root)
        window = cold.ColdWindow(self.root, self.request, journal['backup_binding'], self.group,
                                 self.package['dsn'], self.package['schema_sha256'], self.manager,
                                 self.writer_probe, self.trash)
        window.acquire_owner(); self.addCleanup(window.release_owner)
        # A running fixture holds no runtime lock; actual runtime would refuse
        # lock acquisition. Even with both locks the durable flag forbids restore.
        window.acquire(); self.addCleanup(window.release)
        with self.assertRaisesRegex(c.ContractError, 'prestart_recovery_refused'):
            cold.recover_prestart(window, self.root / 'absent-approval')
        self.assertEqual(self.binary.read_bytes(), self.new)

    def test_start_response_loss_never_replays_manager_or_old_restore(self):
        original = self.manager.start
        def lost():
            original(); raise TimeoutError('fixture lost response')
        self.manager.start = lost
        with self.assertRaises(upgrade.Failure) as error: self.invoke()
        self.assertEqual(error.exception.exit_code, 81)
        self.assertEqual(self.manager.starts, 1)
        before = snapshot(self.root)
        with self.assertRaises(upgrade.Failure): self.invoke()
        self.assertEqual(self.manager.starts, 1)
        self.assertEqual(snapshot(self.root), before)


if __name__ == '__main__':
    unittest.main()
