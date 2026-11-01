#!/usr/bin/env python3
"""Real filesystem/lock regressions; inert bytes never execute as AGS."""
import copy
from types import SimpleNamespace
import contextlib
import fcntl
import hashlib
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

import release_contract as contract
import release_verify as verifier
import upgrade
import upgrade_engine as engine
import upgrade_host as host
from test_upgrade import authorization

C, R, T = 'c' * 40, 'a' * 40, 'b' * 40
VERSION = 'fork-20260924.9-rc1'


def pin_fixture():
    runtime = {'revision': R, 'tree': T}
    targets = {}
    for target in ('darwin_arm64', 'linux_amd64'):
        goos, goarch = target.split('_')
        binaries = {command: {'sha256': 'd' * 64, 'size': 1, 'cgo_enabled': command == 'gh-server',
                             'identity': {'schema': 'ags.build.v1', 'command': command,
                                          'version': VERSION, **runtime, 'goos': goos,
                                          'goarch': goarch, 'go_version': 'go1.26.8'}}
                    for command in contract.COMMANDS}
        asset = {'id': len(targets) + 1, 'name': 'ags-' + VERSION + '-' + target + '.tar.gz',
                 'size': 1, 'digest': 'sha256:' + 'd' * 64}
        targets[target] = {'asset': asset, 'manifest_schema': contract.MANIFEST_SCHEMA,
                           'abi': contract.abi_for(target), 'binaries': binaries, 'provenance': {
                               'mode': 'separated-control-v1', 'subject_digest': asset['digest'],
                               'repository_id': contract.REPO_ID,
                               'issuer': 'https://token.actions.githubusercontent.com',
                               'signer_workflow': verifier.WORKFLOW, 'signer_ref': 'refs/tags/' + VERSION,
                               'signer_digest': C, 'workflow_run_id': 1, 'workflow_run_attempt': 1,
                               'control_revision': C, 'control_tree': 'e' * 40,
                               'runtime_revision': R, 'runtime_tree': T,
                               'predicate_digest': 'f' * 64, 'predicate_type': verifier.PREDICATE_TYPE}}
    return {'schema': contract.PIN_SCHEMA, 'repository': contract.REPO, 'repository_id': contract.REPO_ID,
            'version': VERSION, 'release_id': 1, 'immutable': True, 'draft': False,
            'allow_prerelease': True, 'runtime': runtime,
            'toolchain': {'go_version': 'go1.26.8', 'pin_path': '.go-version'}, 'targets': targets,
            'ci': {'revision': R, 'fork_ci': {'workflow': '.github/workflows/ci.yml', 'run_id': 1, 'run_attempt': 1},
                   'secret_scan': {'workflow': '.github/workflows/secret-scan.yml', 'run_id': 2, 'run_attempt': 1}},
            'verification_policy': {'schema': 'ags.upgrade-policy.v2', 'policy_sha256': 'a' * 64,
                                    'tool_bundle_manifest_sha256': 'b' * 64}}


class SafetyTests(unittest.TestCase):
    def setUp(self):
        # The production reader intentionally rejects even sticky writable ancestors.
        self.tmp = tempfile.TemporaryDirectory(prefix='.ags-safety-', dir=Path.home())
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        for name in ('bin', 'state', 'config'):
            (self.root / name).mkdir(mode=0o700)
        self.candidate = self.root / 'candidate'
        self.candidate.write_bytes(b'inert candidate')
        self.destination = self.root / 'bin/gh-server'
        self.destination.write_bytes(b'old')
        self.info = {'source': R, 'runtime_tree': T, 'control_revision': C, 'version': VERSION,
                     'release_id': 1, 'attestation': {}, 'binaries': {
                         'gh-server': {'sha256': hashlib.sha256(self.candidate.read_bytes()).hexdigest()}}}
        engine.save(self.root / 'state/installation.json', {'source': 'e' * 40, 'binaries': {'gh-server': {}}})
        self.effects = object.__new__(host.Effects)
        self.effects.root, self.effects.candidate, self.effects.info = self.root, self.candidate, self.info
        self.effects.binding = {'host': 'mini'}
        self.effects._cold = unittest.mock.Mock()
        self.effects._cold.journal = {'phase': 'install_started'}
        self.effects._cold.backup_binding = {'new': self.info['source'],
                                            'new_binary_sha256': self.info['binaries']['gh-server']['sha256']}
        self.guards = contextlib.ExitStack()
        self.addCleanup(self.guards.close)
        def acquire():
            for name in ('install.lock', 'runtime.lock'):
                lock = self.guards.enter_context((self.root / 'state' / name).open('a+'))
                os.chmod(lock.name, 0o600)
                try: fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                except BlockingIOError: raise upgrade.Failure('lock_held')
            return self.effects._cold
        self.effects._window = acquire

    def test_direct_host_install_without_private_cold_harness_stays_83(self):
        del self.effects._cold
        del self.effects._window
        with self.assertRaises(upgrade.Failure) as caught:
            self.effects.install()
        self.assertEqual((caught.exception.code, caught.exception.exit_code),
                         ('backup_contract_unimplemented', 83))
        self.assertEqual(self.destination.read_bytes(), b'old')

    def test_each_shared_lock_blocks_without_replacement_or_receipt(self):
        for name in ('install.lock', 'runtime.lock'):
            with self.subTest(name=name), (self.root / 'state' / name).open('w') as lock:
                os.chmod(lock.name, 0o600)
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                before = (self.root / 'state/installation.json').read_bytes()
                with self.assertRaisesRegex(upgrade.Failure, 'lock_held'):
                    self.effects.install()
                self.assertEqual(self.destination.read_bytes(), b'old')
                self.assertEqual((self.root / 'state/installation.json').read_bytes(), before)
                self.assertFalse((self.root / 'state/installing.json').exists())
                self.assertFalse((self.root / 'bin/.gh-server.upgrading').exists())
                self.guards.close()

    def test_bin_ancestor_symlink_never_writes_outside(self):
        outside = self.root / 'outside'
        (self.root / 'bin').rename(outside)
        (self.root / 'bin').symlink_to(outside, target_is_directory=True)
        with self.assertRaises(upgrade.Failure):
            self.effects.install()
        self.assertEqual((outside / 'gh-server').read_bytes(), b'old')
        self.assertEqual(list(outside.iterdir()), [outside / 'gh-server'])

    def test_ancestor_swapped_after_open_cannot_redirect_writes(self):
        outside = self.root / 'outside'; outside.mkdir(mode=0o700)
        (outside / 'gh-server').write_bytes(b'outside')
        original = os.replace
        def swap(source, destination, **kwargs):
            if source == '.gh-server.upgrading':
                (self.root / 'bin').rename(self.root / 'opened-bin')
                (self.root / 'bin').symlink_to(outside, target_is_directory=True)
            return original(source, destination, **kwargs)
        with patch.object(host.os, 'replace', side_effect=swap):
            self.effects.install()
        self.assertEqual((outside / 'gh-server').read_bytes(), b'outside')
        self.assertEqual((self.root / 'opened-bin/gh-server').read_bytes(), self.candidate.read_bytes())

    def test_both_locks_held_until_receipt_is_durable(self):
        original = os.replace
        seen = []
        def inspect(source, destination, **kwargs):
            if destination == 'installation.json':
                for name in ('install.lock', 'runtime.lock'):
                    with (self.root / 'state' / name).open('r+') as lock:
                        with self.assertRaises(BlockingIOError):
                            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                        seen.append(name)
            return original(source, destination, **kwargs)
        with patch.object(host.os, 'replace', side_effect=inspect):
            self.effects.install()
        current, _ = upgrade.private_json(self.root / 'state/installation.json')
        self.assertEqual(current['source'], R)
        self.assertEqual(seen, ['install.lock', 'runtime.lock'])
        self.assertFalse((self.root / 'state/installing.json').exists())

    def test_private_reader_rejects_writable_ancestor_and_duplicate_fields(self):
        path = self.root / 'config/pin.json'; engine.save(path, pin_fixture())
        upgrade.private_json(path)
        self.root.chmod(0o777)
        try:
            with self.assertRaises(upgrade.Failure):
                upgrade.private_json(path)
        finally:
            self.root.chmod(0o700)
        path.write_text('{"schema":1,"schema":2}')
        with self.assertRaises(upgrade.Failure):
            upgrade.private_json(path)

    def test_closed_pin_schema_at_every_object_and_required_field(self):
        pin = pin_fixture()
        source = {'schema': 'ags.release-source.v1', 'version': VERSION, 'runtime': pin['runtime']}
        upgrade.validate_pin(pin, source=source)
        def objects(value, path=()):
            if isinstance(value, dict):
                yield path, value
                for key, child in value.items():
                    yield from objects(child, (*path, key))
        for path, obj in objects(pin):
            for mutation in ['unknown', *obj]:
                candidate = copy.deepcopy(pin); current = candidate
                for key in path:
                    current = current[key]
                if mutation == 'unknown':
                    current['expires_at'] = '2000-01-01T00:00:00Z'
                else:
                    del current[mutation]
                with self.subTest(path=path, mutation=mutation), self.assertRaisesRegex(upgrade.Failure, 'release_pin_invalid'):
                    upgrade.validate_pin(candidate, source=source)
        for key in ('release_id', 'repository_id'):
            candidate = copy.deepcopy(pin); candidate[key] = True
            with self.assertRaises(upgrade.Failure):
                upgrade.validate_pin(candidate)
        for field in ('revision', 'tree'):
            with self.assertRaises(upgrade.Failure):
                upgrade.validate_pin(pin, source={**source, 'runtime': {**pin['runtime'], field: '0' * 40}})
        for key, value in [('control_revision', R), ('runtime_tree', '0' * 40), ('workflow_run_id', True)]:
            candidate = copy.deepcopy(pin); candidate['targets']['linux_amd64']['provenance'][key] = value
            with self.assertRaises(upgrade.Failure):
                upgrade.validate_pin(candidate)

    def test_context_reads_original_claim_not_replaced_or_missing_installation_receipt(self):
        bundle = self.root / 'bundle'; bundle.mkdir(mode=0o700)
        policy = {'schema': 'ags.upgrade-policy.v2', 'deployments': {'mini': {'root': str(self.root)}},
                  'migration_admissions': []}
        engine.save(bundle / 'upgrade_policy.json', policy)
        pin = pin_fixture()
        pin['verification_policy']['policy_sha256'] = hashlib.sha256((bundle / 'upgrade_policy.json').read_bytes()).hexdigest()
        engine.save(self.root / 'config/ags-deploy-release-pin.json', pin)
        engine.save(bundle / 'release-source.json', {'schema': 'ags.release-source.v1',
                                                   'version': VERSION, 'runtime': pin['runtime']})
        _, digest = upgrade.private_json(self.root / 'config/ags-deploy-release-pin.json')
        args = SimpleNamespace(deployment_id='fixture', ci_run_id='1', ci_run_attempt='1',
                               authorization_id='auth', pin_sha256=digest)
        from test_upgrade_identity import facts, profile
        from upgrade_identity import parse_profile
        identity = parse_profile(json.dumps(profile(str(self.root.parent))).encode(),
                                 facts(str(self.root.parent)))
        from dataclasses import replace
        identity = replace(identity, root=str(self.root))
        (bundle / 'host-profile.json').write_bytes(json.dumps(profile(str(self.root.parent))).encode())
        args.identity_sha256 = identity.sha256
        patches = [patch.object(host, 'BUNDLE', bundle),
                   patch.object(host, 'resolve_identity', return_value=identity),
                   patch.object(host, 'require_same', return_value=identity),
                   patch.object(host, 'bundle_engine')]
        with contextlib.ExitStack() as stack:
            for guard in patches: stack.enter_context(guard)
            original = host.context(args)[-1]
            pending = self.root / 'state/ags-autodeploy/local-v2/pending'
            pending.mkdir(mode=0o700, parents=True)
            pending.parent.chmod(0o700); pending.parent.parent.chmod(0o700)
            engine.save(pending / 'claim.json', {'schema': 'ags.local-claim.v2', 'binding': original})
            # Even an interrupted receipt cannot change the immutable original old.
            (self.root / 'state/installation.json').rename(self.root / 'retained-installation')
            self.assertEqual(host.context(args)[-1], original)
            args.deployment_id = 'different'
            with self.assertRaises(upgrade.Failure): host.context(args)

    def test_apply_rejects_even_with_approved_host_before_claim_or_any_effect(self):
        binding = {'host': 'imile-win'}; auth = authorization(binding)
        effects = unittest.mock.Mock()
        state = self.root / 'absent-state'
        with self.assertRaises(upgrade.Failure) as caught:
            engine.Transaction(state, binding, auth, effects).run()
        self.assertEqual(caught.exception.exit_code, 83)
        self.assertIn('trusted_mini_handoff_bound_to_pin_release_deployment', caught.exception.missing_contracts)
        effects.assert_not_called()
        self.assertEqual(effects.mock_calls, [])
        self.assertFalse(state.exists())
        argv = ['upgrade_host.py', 'apply', '--deployment-id', 'test', '--pin-sha256', 'a' * 64,
                '--ci-run-id', '1', '--ci-run-attempt', '1', '--authorization-id', 'auth',
                '--identity-sha256', 'a' * 64]
        with patch.object(sys, 'argv', argv), patch.object(host, 'context', return_value=(self.root, {}, {}, {}, binding)), patch.object(
                host, 'private_json', return_value=(auth, 'digest')), patch.object(host, 'child') as command, patch.object(host, 'Effects') as adapter:
            with self.assertRaisesRegex(upgrade.Failure, 'backup_contract_unimplemented'):
                host.main()
            command.assert_not_called(); adapter.assert_not_called()
        with self.assertRaisesRegex(upgrade.Failure, 'backup_contract_unimplemented'):
            self.effects.backup()
        self.assertEqual(upgrade.result_exit({'status': 'awaiting_observation'}), 86)
        self.assertEqual(upgrade.result_exit({'status': 'completed'}), 0)
        self.assertNotEqual(upgrade.result_exit({'status': 'unknown'}), 0)


if __name__ == '__main__':
    unittest.main()
