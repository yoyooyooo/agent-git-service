#!/usr/bin/env python3
"""Source regressions only. These tests never claim native admission."""
import copy
import importlib.util
import io
import json
import os
from pathlib import Path
import struct
import sys
import tarfile
import tempfile
import unittest

from approval_fault import (FAN_OPEN_PERM, FAN_Q_OVERFLOW, METADATA,
                            events, select_checkpoint, start_paths)
import authority
import authorization
from common import Refused, canonical, exclusive, fingerprint, object_json, read_bytes, sha
from provision import unit
from source_evidence import validate


class CheckpointTests(unittest.TestCase):
    def setUp(self):
        self.identity = {'pid': 221, 'parent_pid': 220, 'start_ticks': 1234, 'uid': 1001, 'gid': 1001}
        self.registration = {'id': 'fixture', 'binding': {'host_instance': 'fixture-host'}}
        self.original = {'id': 'fixture-transaction', 'phase': 'transaction'}
        self.candidate = {'id': 'fixture-candidate', 'phase': 'serve-candidate'}
        self.host = {'schema': 'ags.routeb-host-journal.v1', 'phase': 'installed', 'status': 'running',
                     'transaction_id': 'fixture', 'host_instance': 'fixture-host',
                     'stop_attempted': True, 'start_attempted': False}
        self.cold = {'schema': 'ags.cold-journal.v1', 'phase': 'installed', 'start_attempted': False}
        self.context = {'schema': 'ags.routeb-stage0-context.v1', 'registration_id': 'fixture',
                        'invocation_id': 'fixture-transaction', 'operation': 'transaction',
                        'phase': 'transaction', 'child': self.identity}

    def selected(self, pid=221, identity=None):
        return select_checkpoint(self.host, self.cold, self.context, self.registration,
                                 self.original, self.candidate, pid, identity or self.identity)

    def test_exact_installed_child_is_selected(self):
        self.assertTrue(self.selected())

    def test_root_parent_and_unrelated_child_are_allowed(self):
        self.assertFalse(self.selected(220))
        self.assertFalse(self.selected(222))

    def test_before_installed_is_allowed(self):
        self.host['phase'] = 'install_started'
        self.assertFalse(self.selected())

    def test_already_failed_is_not_injected_again(self):
        self.host['status'] = 'failed'
        self.assertFalse(self.selected())

    def test_reused_pid_refuses(self):
        with self.assertRaises(Refused):
            self.selected(identity={**self.identity, 'start_ticks': 9999})

    def test_each_start_marker_refuses(self):
        for key, value in [('start_attempted', True), ('start_called', False), ('handoff', {})]:
            with self.subTest(key=key):
                self.cold = {'schema': 'ags.cold-journal.v1', 'phase': 'installed', 'start_attempted': False, key: value}
                with self.assertRaises(Refused):
                    self.selected()

    def test_original_identity_mismatch_refuses(self):
        for key, value in [('operation', 'continue-installed'), ('phase', 'serve-candidate'),
                           ('registration_id', 'another'), ('invocation_id', 'another')]:
            with self.subTest(key=key):
                original = self.context[key]
                self.context[key] = value
                with self.assertRaises(Refused):
                    self.selected()
                self.context[key] = original

    def test_candidate_domain_contains_intent_and_claim(self):
        paths = start_paths(Path('/registry/id'), self.original, self.candidate)
        self.assertIn(Path('/registry/id/state/effects/fixture-transaction-manager-start-intent.json'), paths)
        self.assertIn(Path('/registry/id/state/claims/fixture-candidate.json'), paths)

    def test_permission_event_requires_expected_version_and_type(self):
        raw = METADATA.pack(24, 3, 0, 24, FAN_OPEN_PERM, 5, 221)
        self.assertEqual(list(events(raw)), [(5, 221)])
        for value in (raw[:-1], METADATA.pack(24, 2, 0, 24, FAN_OPEN_PERM, 5, 221),
                      METADATA.pack(24, 3, 0, 24, FAN_Q_OVERFLOW, -1, 0)):
            with self.subTest(raw=value):
                with self.assertRaises(Refused):
                    list(events(value))


class AuthorityTests(unittest.TestCase):
    def archive(self, base, names):
        path = base / 'publisher.tar.gz'
        with tarfile.open(path, 'w:gz') as stream:
            for name, kind, payload in names:
                info = tarfile.TarInfo(name)
                info.uid = info.gid = 0
                if kind == 'file':
                    info.size, info.mode = len(payload), 0o644
                    stream.addfile(info, io.BytesIO(payload))
                else:
                    info.type, info.linkname = tarfile.SYMTYPE, payload
                    stream.addfile(info)
        return path

    def test_regular_archive_creates_fresh_independent_bytes(self):
        with tempfile.TemporaryDirectory() as temporary:
            base = Path(temporary).resolve()
            archive = self.archive(base, [('python/bin/python3.13', 'file', b'elf')])
            prefix = base / 'prefix'
            authority.extract_publisher(archive, prefix)
            self.assertEqual((prefix / 'bin/python3.13').read_bytes(), b'elf')
            with self.assertRaises(FileExistsError):
                authority.extract_publisher(archive, prefix)

    def test_archive_cannot_escape_or_replace_parent(self):
        rows = [[('../outside', 'file', b'x')],
                [('python/../../outside', 'file', b'x')],
                [('python/bin/link', 'link', '../../../outside')],
                [('python/bin', 'file', b'x'), ('python/bin/python', 'file', b'x')],
                [('python/bin/python', 'file', b'a'), ('python/bin/python', 'file', b'b')]]
        for content in rows:
            with self.subTest(content=content), tempfile.TemporaryDirectory() as temporary:
                base = Path(temporary).resolve()
                with self.assertRaises((Refused, ValueError)):
                    authority.extract_publisher(self.archive(base, content), base / 'prefix')
                self.assertFalse((base / 'outside').exists())

    def test_no_replace_and_no_symlink_reads(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            exclusive(root / 'a', b'retained')
            with self.assertRaises(FileExistsError):
                exclusive(root / 'a', b'overwrite')
            (root / 'link').symlink_to(root / 'a')
            with self.assertRaises((Refused, OSError)):
                read_bytes(root / 'link')
            self.assertEqual((root / 'a').read_bytes(), b'retained')

    def test_duplicate_and_nonfinite_json_refuse(self):
        for raw in (b'{"a":1,"a":2}', b'{"a":NaN}', b'[]'):
            with self.subTest(raw=raw), self.assertRaises(Refused):
                object_json(raw)


class DelegationTests(unittest.TestCase):
    def test_subject_uses_production_contract(self):
        scripts = Path(__file__).resolve().parent.parent
        sys.path.insert(0, str(scripts))
        try:
            import routeb_native_delegation as production
            values = {
                'native-provision': {'binding_sha256': '1' * 64},
                'host-window': {'package_sha256': '2' * 64},
                'continue-installed': {'approval_id': 'a', 'purpose': 'continue-installed', 'subject_sha256': '3' * 64},
                'native-bootstrap': {'root': '/a', 'host_instance': 'h', 'transaction_id': 't',
                                     'provision_sha256': '4' * 64, 'identity_sha256': '5' * 64, 'epoch': 1},
                'immutable-provision': {'host_instance': 'h', 'tool_manifest_sha256': '6' * 64,
                                       'installer_sha256': '7' * 64, 'objects': []}}
            for purpose in ('full-restore', 'old-start'):
                values[purpose] = {'approval_id': 'a', 'purpose': purpose, 'host_instance': 'h',
                                   'snapshot_id': 's', 'manifest_sha256': '8' * 64,
                                   'data_loss_acknowledged': True, 'external_reconciliation_acknowledged': True}
            for purpose, value in values.items():
                self.assertEqual(authorization.subject(value, purpose), production.subject_sha256(value, purpose))
            self.assertNotEqual(authorization.subject(values['continue-installed'], 'continue-installed'), '3' * 64)
        finally:
            sys.path.remove(str(scripts))

    def test_review_has_real_time_and_exact_evidence_binding(self):
        value = authorization.approve({'binding_sha256': '1' * 64}, 'native-provision',
                                       {'sha256': '2' * 64}, [{'kind': 'test-observation', 'sha256': '3' * 64}])
        self.assertEqual(value['actor'], authorization.DELEGATE)
        self.assertEqual(value['delegation']['review']['subject_sha256'], '1' * 64)
        self.assertLessEqual(value['delegation']['review']['reviewed_at'], value['issued_at'])
        self.assertLess(value['issued_at'], value['expires_at'])

    def test_empty_or_wrong_digest_authorization_refuses(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary).resolve() / 'source.json'
            exclusive(path, b'{}')
            with self.assertRaises(Refused):
                authorization.source_input(path, '0' * 64)
            with self.assertRaises(Refused):
                authorization.source_input(path, sha(b'{}'))


class SourceAndEntryTests(unittest.TestCase):
    def setUp(self):
        self.sha = '1' * 40
        self.repo = {'full_name': 'yoyooyooo/agent-git-service', 'default_branch': 'fork/main.20260924'}
        self.commit = {'sha': self.sha, 'commit': {'tree': {'sha': '2' * 40}}}
        self.ci = {'head_sha': self.sha, 'head_branch': self.repo['default_branch'], 'event': 'push',
                   'status': 'completed', 'conclusion': 'success', 'repository': self.repo,
                   'run_attempt': 1, 'workflow_id': 17}
        self.workflow = {'id': 17, 'path': '.github/workflows/ci.yml', 'name': 'Fork CI'}

    def test_exact_accepted_ci_required(self):
        self.assertEqual(validate(self.repo, self.commit, self.ci, self.workflow, self.sha), '2' * 40)
        for key, value in [('head_sha', '9' * 40), ('conclusion', 'failure'), ('status', 'in_progress'),
                           ('event', 'pull_request'), ('head_branch', 'unaccepted'), ('run_attempt', 0)]:
            with self.subTest(key=key), self.assertRaises(Refused):
                validate(self.repo, self.commit, {**self.ci, key: value}, self.workflow, self.sha)

    def test_another_successful_workflow_is_not_fork_ci(self):
        with self.assertRaises(Refused):
            validate(self.repo, self.commit, self.ci, {**self.workflow, 'path': '.github/workflows/native-adapter.yml'}, self.sha)

    def test_units_keep_native_manager_permissions_and_completion(self):
        name, raw = unit('gha-12-1', 'continue-installed', Path('/usr/local/libexec/ags-routeb-native/gha-12-1/stage0/ags-routeb-stage0'))
        self.assertEqual(name, 'ags-routeb-control-gha-12-1-continue-installed.service')
        text = raw.decode()
        for directive in ('User=root\n', 'Group=root\n', 'Restart=no\n', 'SendSIGKILL=no\n',
                           'NoNewPrivileges=yes\n', ' manager-complete --registration gha-12-1 --operation continue-installed'):
            self.assertIn(directive, text)
        self.assertNotIn('Environment=', text)
        self.assertNotIn('/bin/sh', text)


if __name__ == '__main__':
    unittest.main()
