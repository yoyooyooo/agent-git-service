#!/usr/bin/env python3
"""S6.01 host identity. Synthetic facts only; no production profile or DNS."""
from contextlib import ExitStack
from dataclasses import replace
import hashlib
import json
import os
from pathlib import Path
import socket
import stat
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import upgrade

HERE = Path(__file__).resolve().parent


def profile(home):
    return {'schema': 'ags.host-identity.v1', 'logical_host': 'mini',
            'host_instance': 'synthetic-mini', 'uid': 501, 'gid': 20, 'user': 'yoyo',
            'home': home, 'root': home + '/.ags', 'platform': 'Darwin', 'arch': 'arm64',
            'machine_id_kind': 'ioplatformuuid', 'machine_id_sha256': 'a' * 64,
            'aliases': ['primary-mini', 'primary-mini.local']}


def facts(home, **overrides):
    from upgrade_identity import MachineFacts
    values = dict(hostname='primary-mini.local', uid=501, euid=501, gid=20, user='yoyo',
                  home=home, platform='Darwin', arch='arm64', machine_id_kind='ioplatformuuid',
                  machine_id_sha256='a' * 64)
    values.update(overrides)
    return MachineFacts(**values)


class BaselineRegression(unittest.TestCase):
    def test_reviewed_full_hostname_is_not_truncated(self):
        """Parent API. The red is invalid_host, not a missing module."""
        ledger = Mock()
        with tempfile.TemporaryDirectory(dir=Path.home()) as directory, ExitStack() as stack:
            home = str(Path(directory).resolve())
            for name in ('getaddrinfo', 'gethostbyname', 'getfqdn'):
                stack.enter_context(patch.object(socket, name, side_effect=AssertionError('DNS forbidden')))
            stack.enter_context(patch.object(upgrade, 'private_json', ledger.private_read))
            stack.enter_context(patch.object(upgrade.subprocess, 'run', ledger.command))
            stack.enter_context(patch.object(upgrade.os, 'getuid', return_value=501))
            stack.enter_context(patch.object(upgrade.os, 'geteuid', return_value=501))
            stack.enter_context(patch('pwd.getpwuid', return_value=SimpleNamespace(pw_name='yoyo', pw_dir=home)))
            if (HERE / 'upgrade_identity.py').exists():
                import upgrade_identity as identity
                row = profile(home)
                stack.enter_context(patch.object(identity, 'read_trusted',
                                                  return_value=json.dumps(row).encode()))
                stack.enter_context(patch.object(identity, 'observed_facts',
                                                  return_value=facts(home)))
            root = upgrade.execution_identity('primary-mini.local')
            self.assertEqual(root, Path(home) / '.ags')
            self.assertEqual(ledger.mock_calls, [])
            self.assertEqual(list(Path(home).iterdir()), [])


class IdentityTests(unittest.TestCase):
    def setUp(self):
        import upgrade_identity as identity
        self.identity = identity
        self.tmp = tempfile.TemporaryDirectory(dir=Path.home(), prefix='identity-')
        self.addCleanup(self.tmp.cleanup)
        self.home = Path(self.tmp.name).resolve()
        self.row = profile(str(self.home))
        self.observed = facts(str(self.home))
        self.ledger = Mock()
        self.stack = ExitStack()
        self.addCleanup(self.stack.close)
        for name in ('getaddrinfo', 'gethostbyname', 'getfqdn'):
            self.stack.enter_context(patch.object(socket, name, side_effect=self.ledger.dns))
        self.stack.enter_context(patch.object(upgrade, 'private_json', self.ledger.production_read))
        self.stack.enter_context(patch.object(upgrade.subprocess, 'run', self.ledger.command))
        self.stack.enter_context(patch.object(upgrade, 'bundle_engine', self.ledger.bundle))
        self.stack.enter_context(patch.object(upgrade, 'require_apply_contracts', self.ledger.capabilities))
        self.addCleanup(self.assert_inert)

    def assert_inert(self):
        self.assertEqual(self.ledger.mock_calls, [])
        self.assertFalse((self.home / '.ags').exists())

    def resolve(self, row=None, observed=None, raw=None):
        payload = raw if raw is not None else json.dumps(self.row if row is None else row).encode()
        return self.identity.parse_profile(payload, observed or self.observed)

    def refused(self, operation, code):
        with self.assertRaises(self.identity.IdentityError) as caught:
            operation()
        self.assertEqual(caught.exception.exit_code, code)
        self.assertNotIn('secret-raw', str(caught.exception))

    def test_both_explicit_aliases_and_no_environment_or_dns(self):
        for name in ('primary-mini', 'primary-mini.local', 'PRIMARY-MINI.LOCAL'):
            with patch.dict(os.environ, {'HOSTNAME': 'imile-win', 'HOME': '/forbidden',
                                          'AGS_HOST': 'imile-win', 'AGS_HOST_PROFILE': '/forbidden'}):
                result = self.resolve(observed=replace(self.observed, hostname=name))
            self.assertEqual((result.logical_host, result.root), ('mini', str(self.home / '.ags')))
        for name in ('mini', 'unknown.local', 'primary-mini.other', 'primary-mini.local.'):
            self.refused(lambda: self.resolve(observed=replace(self.observed, hostname=name)), 64)

    def test_conflicting_aliases_and_closed_schema(self):
        for aliases in ([], ['mini', 'MINI'], ['mini', 'mini'], 'mini'):
            self.refused(lambda: self.resolve({**self.row, 'aliases': aliases}), 64)
        for key, value in (('logical_host', 'other'), ('uid', 1000), ('platform', 'Linux')):
            self.refused(lambda: self.resolve({**self.row, key: value}), 64)
        self.refused(lambda: self.resolve({**self.row, 'enabled': True}), 69)
        self.refused(lambda: self.resolve(raw=b'{"schema":1,"schema":2}'), 69)
        self.refused(lambda: self.resolve(raw=b'[]'), 69)
        self.refused(lambda: self.resolve(raw=b'x' * 65537), 69)

    def test_copied_profile_and_execution_mismatch(self):
        for key, value in (('machine_id_sha256', 'b' * 64), ('platform', 'Linux'),
                           ('arch', 'x86_64'), ('machine_id_kind', 'machine-id')):
            self.refused(lambda: self.resolve(observed=replace(self.observed, **{key: value})), 64)
        for key, value in (('uid', 0), ('euid', 0), ('gid', 0), ('user', 'other'), ('home', '/other')):
            self.refused(lambda: self.resolve(observed=replace(self.observed, **{key: value})), 77)

    def test_missing_profile_does_not_measure_or_guess(self):
        with patch.object(self.identity, 'PROFILE', self.home / 'absent'), \
                patch.object(self.identity, 'observed_facts', self.ledger.measure):
            self.refused(self.identity.resolve_identity, 69)

    def test_parent_child_reparse_and_binding_mismatch(self):
        original = self.resolve()
        for row in ({**self.row, 'host_instance': 'changed'},
                    {**self.row, 'aliases': list(reversed(self.row['aliases']))}):
            changed = self.resolve(row)
            with patch.object(self.identity, 'resolve_identity', return_value=changed):
                self.refused(lambda: self.identity.require_same(original), 64)
        with patch.object(self.identity, 'resolve_identity', return_value=original):
            self.identity.require_same(original, expected_sha256=original.sha256)
            self.refused(lambda: self.identity.require_same(original, expected_sha256='b' * 64), 64)
            self.refused(lambda: self.identity.require_same(original, root='/other'), 77)
            self.refused(lambda: self.identity.require_same(original, host_instance='other'), 64)

    def test_reader_rejects_symlink_hardlink_mode_and_parent(self):
        path = self.home / 'host-profile.json'
        path.write_bytes(json.dumps(self.row).encode())
        path.chmod(0o644)
        original = os.fstat

        def owner(descriptor):
            values = list(original(descriptor))
            values[4] = os.getuid()
            return os.stat_result(values)

        with patch.object(self.identity.os, 'fstat', side_effect=owner):
            self.assertEqual(self.identity.read_trusted(path, owner=os.getuid()), path.read_bytes())
            linked = self.home / 'linked'
            linked.symlink_to(path)
            self.refused(lambda: self.identity.read_trusted(linked, owner=os.getuid()), 69)
            parent = self.home / 'parent'
            parent.symlink_to(self.home, target_is_directory=True)
            self.refused(lambda: self.identity.read_trusted(parent / path.name, owner=os.getuid()), 69)
            hard = self.home / 'hard'
            os.link(path, hard)
            self.refused(lambda: self.identity.read_trusted(path, owner=os.getuid()), 69)
            hard.rename(self.home / 'retained-hard')
            fresh = self.home / 'fresh'
            fresh.write_bytes(b'{}')
            fresh.chmod(0o666)
            self.refused(lambda: self.identity.read_trusted(fresh, owner=os.getuid()), 69)
            self.home.chmod(stat.S_IMODE(self.home.stat().st_mode) | 0o022)
            try:
                self.refused(lambda: self.identity.read_trusted(fresh, owner=os.getuid()), 69)
            finally:
                self.home.chmod(0o700)

    def test_wrapper_and_engine_recheck_before_pin(self):
        import upgrade_host as host
        original = self.resolve()
        argv = ['upgrade.py', 'apply', '--deployment-id', 'fixture', '--pin-sha256', 'a' * 64,
                '--ci-run-id', '1', '--ci-run-attempt', '1', '--authorization-id', 'auth']
        with patch.object(upgrade, 'resolve_identity', return_value=original), \
                patch.object(upgrade, 'require_same',
                             side_effect=self.identity.IdentityError('host_machine_mismatch', 64)), \
                patch.object(sys, 'argv', argv):
            with self.assertRaises(upgrade.Failure) as caught:
                upgrade.main()
            self.assertEqual(caught.exception.exit_code, 64)
        args = SimpleNamespace(identity_sha256='b' * 64)
        with patch.object(host, 'resolve_identity', return_value=original), \
                patch.object(host, 'require_same',
                             side_effect=self.identity.IdentityError('host_machine_mismatch', 64)):
            with self.assertRaises(upgrade.Failure) as caught:
                host.context(args)
            self.assertEqual(caught.exception.exit_code, 64)

    def test_cold_runtime_refuses_before_token_or_spawn(self):
        import importlib.util
        spec = importlib.util.spec_from_file_location('identity_runtime', HERE / 'ags-runtime.py')
        runtime = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(runtime)
        root = self.home / '.ags'
        for method in ('startup_permitted', 'consume_start', 'serve'):
            with self.subTest(method=method), \
                    patch.object(runtime, 'cold_pending', return_value=True), \
                    patch.object(self.identity, 'runtime_identity',
                                 side_effect=self.identity.IdentityError('host_machine_mismatch', 64)), \
                    patch.object(runtime, 'atomic_json', self.ledger.state_write), \
                    patch.object(runtime.subprocess, 'Popen', self.ledger.spawn):
                if method == 'consume_start':
                    self.refused(lambda: runtime.consume_start(root), 64)
                else:
                    expected = False if method == 'startup_permitted' else 78
                    self.assertEqual(getattr(runtime, method)(root), expected)

    def test_stage_rechecks_identity_before_installer(self):
        import upgrade_host as host
        original = self.resolve()
        changed = self.resolve({**self.row, 'host_instance': 'different'})
        package = SimpleNamespace(backup_binding={'host_instance': original.host_instance})
        with patch.object(self.identity, 'resolve_identity', return_value=changed), \
                patch.object(host, 'load_installer', self.ledger.installer):
            with self.assertRaises(upgrade.Failure) as caught:
                host.Effects(Path(original.root), {}, {}, {}, {}, self.home,
                             package=package, identity=original)
            self.assertEqual(caught.exception.exit_code, 64)

    def test_machine_digest_is_domain_separated(self):
        value = '0' * 32
        digest = self.identity.machine_digest('machine-id', value)
        self.assertEqual(len(digest), 64)
        self.assertNotEqual(digest, hashlib.sha256(value.encode()).hexdigest())
        self.refused(lambda: self.identity.machine_digest('machine-id', 'secret-raw-invalid'), 69)


if __name__ == '__main__':
    unittest.main()
