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
from unittest import mock

from approval_fault import (FAN_OPEN_PERM, FAN_Q_OVERFLOW, METADATA,
                            events, select_checkpoint, start_paths)
import authority
import authorization
import common
from common import Refused, canonical, exclusive, fingerprint, object_json, read_bytes, sha
from provision import unit
from source_evidence import validate
import run as fixture_run


class CommandDiagnosticsTests(unittest.TestCase):
    def failed(self, operation='candidate_version', stderr=b'private-stderr-sentinel'):
        stdout = b'private-stdout-sentinel'
        code = ('import sys;sys.stdout.buffer.write(%r);sys.stderr.buffer.write(%r);sys.exit(23)'
                % (stdout, stderr))
        with self.assertRaises(Refused) as caught:
            common.command([sys.executable, '-c', code, 'private-argv-sentinel'],
                           operation=operation, env={**os.environ, 'PRIVATE_SENTINEL': 'private-env-sentinel'})
        self.assertEqual(str(caught.exception), 'command_failed')
        return caught.exception, stdout

    def test_real_failure_retains_operation_returncode_and_only_output_digests(self):
        error, stdout = self.failed()
        value = error.diagnostic
        self.assertEqual(value['operation'], 'candidate_version')
        self.assertEqual(value['returncode'], 23)
        self.assertEqual(value['stdout'], {'bytes': len(stdout), 'sha256': sha(stdout)})
        self.assertEqual(value['stderr'], {'bytes': 23, 'sha256': sha(b'private-stderr-sentinel')})
        self.assertNotIn('private-', json.dumps(value))
        self.assertNotIn('argv', value)
        self.assertNotIn('env', value)

    def test_native_failure_accepts_only_matching_stage_and_known_reason(self):
        payload = {'schema': 'ags.routeb-fixture-command-error.v1', 'stage': 'verify-catalog',
                   'reason': 'authority_linux_needed_unbound'}
        error, _ = self.failed('verify_catalog', canonical(payload))
        self.assertEqual(error.diagnostic['native_tools'], payload)
        for changed in ({**payload, 'stage': 'seed-db'},
                        {**payload, 'reason': 'authority_private_sentinel'},
                        {**payload, 'private': 'private-extra-sentinel'}):
            with self.subTest(changed=changed):
                error, _ = self.failed('verify_catalog', canonical(changed))
                self.assertNotIn('native_tools', error.diagnostic)
                self.assertNotIn('private-', json.dumps(error.diagnostic))

    def test_non_native_and_unparseable_stderr_stay_opaque(self):
        valid = canonical({'schema': 'ags.routeb-fixture-command-error.v1',
                           'stage': 'verify-catalog', 'reason': 'authority_binding_mismatch'})
        error, _ = self.failed('source_head', valid)
        self.assertNotIn('native_tools', error.diagnostic)
        for raw in (b'private-stderr-sentinel', b'private-' * 1024,
                    b'{"schema":"private","schema":"ags.routeb-fixture-command-error.v1"}'):
            with self.subTest(size=len(raw)):
                error, _ = self.failed('verify_catalog', raw)
                self.assertNotIn('native_tools', error.diagnostic)
                self.assertEqual(error.diagnostic['stderr']['sha256'], sha(raw))
                self.assertNotIn('private-', json.dumps(error.diagnostic))

    def test_success_preserves_bytes_and_rejects_unknown_operation_before_launch(self):
        self.assertEqual(common.command([sys.executable, '-c', 'import sys;sys.stdout.buffer.write(b"ok\\x00")'],
                                        operation='candidate_version'), b'ok\x00')
        with mock.patch.object(common.subprocess, 'run') as launch:
            with self.assertRaisesRegex(Refused, '^command_operation_invalid$'):
                common.command(['unused'], operation='private-operation-sentinel')
            launch.assert_not_called()

    def test_failed_provision_exports_diagnostics_without_manager_effects(self):
        error, _ = self.failed('verify_catalog')
        with tempfile.TemporaryDirectory() as temporary:
            base = Path(temporary).resolve()
            output = base / 'output'
            argv = ['fixture', '--source', str(base), '--build', str(base), '--source-evidence', str(base),
                    '--authorization-file', str(base / 'private-input'), '--authorization-sha256', '1' * 64,
                    '--uid', str(os.getuid()), '--transaction', 'gha-13-1', '--scenario', 'continue',
                    '--export-directory', str(output), '--execute']
            with mock.patch.object(sys, 'argv', argv), mock.patch.object(fixture_run, 'REGISTRY', base / 'registry'), \
                 mock.patch.object(fixture_run, 'provision', side_effect=error), \
                 mock.patch.object(fixture_run.subprocess, 'run') as manager, \
                 mock.patch.object(fixture_run.os, 'chown'), mock.patch.object(fixture_run.os, 'fchown'), \
                 mock.patch('builtins.print'):
                self.assertEqual(fixture_run.main(), 78)
                manager.assert_not_called()
            value = json.loads((output / 'result.json').read_bytes())
            self.assertFalse(value['native_pass'])
            self.assertEqual(value['code'], 'command_failed')
            self.assertEqual(value['command_failure'], error.diagnostic)
            self.assertNotIn('private-', json.dumps(value))

    def test_existing_journal_export_preserves_closed_causes_without_raw_messages(self):
        messages = ['code=stage0_mapping_unknown', 'code=interpreter_full_closure_required',
                    'private-sentinel raw exception /fixture/socket',
                    'code=private-sentinel extra fields',
                    json.dumps({'schema': 'ags.routeb-stage0-result.v1', 'status': 'blocked',
                                'reason': 'stage0_child_or_authority_failed'})]
        raw = b''.join(canonical({'MESSAGE': text, '__REALTIME_TIMESTAMP': str(index)})
                       for index, text in enumerate(messages))
        requests = [mock.Mock(returncode=0, stdout=b'', stderr=b''),
                    mock.Mock(returncode=0, stdout=raw, stderr=b'')]
        with tempfile.TemporaryDirectory() as temporary:
            evidence = Path(temporary).resolve()
            with mock.patch.object(fixture_run.subprocess, 'run', side_effect=requests), \
                    mock.patch.object(fixture_run, 'wait_unit', return_value={'ExecMainStatus': '78'}):
                observed = fixture_run.start_controller({'unit': 'fixture-bootstrap.service'}, evidence, 'bootstrap')
            value = json.loads((evidence / 'bootstrap-reason-codes.json').read_bytes())
            self.assertEqual((evidence / 'bootstrap-journal.private').read_bytes(), raw)
        self.assertEqual(observed, {'ExecMainStatus': '78'})
        self.assertEqual([item['code'] for item in value['codes']],
                         ['stage0_mapping_unknown', 'interpreter_full_closure_required',
                          'stage0_child_or_authority_failed'])
        self.assertEqual(value['raw_sha256'], sha(raw))
        self.assertNotIn('private-sentinel', json.dumps(value))
        self.assertNotIn('/fixture/', json.dumps(value))


class LifecycleOwnershipTests(unittest.TestCase):
    def attempt(self, *, execute, provision_error=None, operation_error=None):
        with tempfile.TemporaryDirectory() as temporary:
            base = Path(temporary).resolve()
            registry = base / 'registry'
            transaction = registry / 'gha-12-1'
            transaction.mkdir(parents=True)
            (transaction / 'fixture-evidence').mkdir()
            before = exclusive(transaction / 'stage0-registration.json', canonical({
                'invocations': [{'unit': 'existing-fixture.service'}], 'host_profile': {}}))
            output = base / 'output'
            output.mkdir()
            argv = ['fixture', '--source', str(base), '--build', str(base),
                    '--source-evidence', str(base), '--authorization-file', str(base / 'authorization.json'),
                    '--authorization-sha256', '1' * 64, '--uid', str(os.getuid()),
                    '--transaction', 'gha-12-1', '--scenario', 'continue', '--export-directory', str(output)]
            if execute:
                argv.append('--execute')
            with mock.patch.object(sys, 'argv', argv), mock.patch.object(fixture_run, 'REGISTRY', registry), \
                 mock.patch.object(fixture_run, 'provision', side_effect=provision_error) as prepare, \
                 mock.patch.object(fixture_run, 'execute', side_effect=operation_error) as operation, \
                 mock.patch.object(fixture_run.subprocess, 'run', return_value=mock.Mock(returncode=0)) as manager, \
                 mock.patch.object(fixture_run, 'properties', return_value={'MainPID': '0', 'ControlPID': '0'}), \
                 mock.patch.object(fixture_run, 'file_reference', return_value=canonical({'uid': os.getuid(), 'gid': os.getgid()})), \
                 mock.patch.object(fixture_run, 'export'), mock.patch('builtins.print'):
                self.assertEqual(fixture_run.main(), 78)
                self.assertEqual(sha((transaction / 'stage0-registration.json').read_bytes()), before['sha256'])
                if not execute or provision_error is not None:
                    manager.assert_not_called()
                    operation.assert_not_called()
                    self.assertEqual(list((transaction / 'fixture-evidence').iterdir()), [])
                else:
                    manager.assert_called_once_with(['/usr/bin/systemctl', 'stop', 'existing-fixture.service'],
                                                    capture_output=True, timeout=150)
                if not execute:
                    prepare.assert_not_called()

    def test_missing_execute_does_not_stop_existing_instance(self):
        self.attempt(execute=False)

    def test_rejected_provision_does_not_stop_existing_instance(self):
        self.attempt(execute=True, provision_error=Refused('vm_preflight_refused'))

    def test_existing_transaction_does_not_stop_existing_instance(self):
        self.attempt(execute=True, provision_error=FileExistsError('transaction exists'))

    def test_owned_provision_keeps_cleanup_after_execution_failure(self):
        self.attempt(execute=True, operation_error=Refused('native_execution_failed'))

    def test_invalid_transaction_is_rejected_before_registry_derivation(self):
        class RegistryGuard:
            def __truediv__(self, other):
                raise AssertionError('registry derived from unvalidated transaction')
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary)
            argv = ['fixture', '--source', '/source', '--build', '/build', '--source-evidence', '/evidence',
                    '--authorization-file', '/input', '--authorization-sha256', '1' * 64, '--uid', str(os.getuid()),
                    '--transaction', '../existing', '--scenario', 'continue', '--export-directory', str(output), '--execute']
            with mock.patch.object(sys, 'argv', argv), mock.patch.object(fixture_run, 'REGISTRY', RegistryGuard()), \
                 mock.patch.object(fixture_run, 'provision') as prepare, \
                 mock.patch.object(fixture_run.subprocess, 'run') as manager, mock.patch('builtins.print'):
                self.assertEqual(fixture_run.main(), 78)
                prepare.assert_not_called()
                manager.assert_not_called()


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


class CatalogFailureFactsTests(unittest.TestCase):
    def catalog(self):
        content = {'sha256': 'a' * 64, 'size': 21}
        external = {'path': '/private-path-sentinel/dependency', 'kind': 'file',
                    'metadata': {'mode': 0o644}, 'file': content.copy()}
        reference = {'path': '/private-path-sentinel/asset', **content}
        return {'publisher': {'archive': reference.copy(), 'checksums': reference.copy()},
                'prefix': {'entries': []}, 'external': [{'entry': external, 'asset': reference.copy()}]}

    def test_non_owner_write_bits_are_facts_not_permission_to_change_the_file(self):
        value = self.catalog()
        before = copy.deepcopy(value)
        self.assertEqual(authority.catalog_file_conditions(value, 'b' * 64)['matching_conditions'], 0)
        for mode in (0o664, 0o666):
            with self.subTest(mode=mode):
                value['external'][0]['entry']['metadata']['mode'] = mode
                facts = authority.catalog_file_conditions(value, 'b' * 64)
                self.assertEqual(facts['authority_sha256'], 'b' * 64)
                self.assertEqual(facts['matching_conditions'], 1)
                row = facts['conditions'][0]
                self.assertEqual((row['scope'], row['index'], row['field'], row['mode']),
                                 ('external_entry', 0, 'mode', mode))
                self.assertEqual(row['non_owner_write_bits'], mode & 0o022)
                self.assertEqual(row['path_sha256'], sha(b'/private-path-sentinel/dependency'))
                self.assertNotIn('private-path-sentinel', json.dumps(facts))
                self.assertNotIn('native_pass', facts)
        value['external'][0]['entry']['metadata']['mode'] = 0o644
        self.assertEqual(value, before)

    def test_all_file_invalid_fields_are_projected_without_input_values(self):
        changes = [
            ('publisher_archive', 'path', lambda v: v['publisher']['archive'].update(path='private-relative-sentinel')),
            ('publisher_checksums', 'sha256', lambda v: v['publisher']['checksums'].update(sha256='private-digest-sentinel')),
            ('publisher_archive', 'size', lambda v: v['publisher']['archive'].update(size=(256 << 20) + 1)),
            ('external_asset', 'path', lambda v: v['external'][0]['asset'].update(path='/private-path-sentinel/../asset')),
            ('prefix_entry', 'mode', lambda v: v['prefix']['entries'].append({**v['external'][0]['entry'],
                'path': 'lib/private-prefix-sentinel', 'metadata': {'mode': 0o664}})),
            ('external_entry', 'file', lambda v: v['external'][0]['entry'].pop('file')),
            ('external_entry', 'link', lambda v: v['external'][0]['entry'].update(link='private-link-sentinel')),
            ('external_entry', 'sha256', lambda v: v['external'][0]['entry']['file'].update(sha256='0' * 64)),
            ('external_entry', 'size', lambda v: v['external'][0]['entry']['file'].update(size=-1)),
        ]
        for scope, field, change in changes:
            with self.subTest(scope=scope, field=field):
                value = self.catalog()
                change(value)
                facts = authority.catalog_file_conditions(value, 'b' * 64)
                self.assertEqual(facts['matching_conditions'], 1)
                self.assertEqual((facts['conditions'][0]['scope'], facts['conditions'][0]['field']), (scope, field))
                self.assertNotIn('private-', json.dumps(facts))

    def test_projection_is_bounded_and_keeps_the_original_command_failure(self):
        import provision
        from subprocess import CompletedProcess
        value = self.catalog()
        value['external'][0]['entry']['metadata']['mode'] = 0o666
        value['external'] *= 20
        facts = authority.catalog_file_conditions(value, 'b' * 64)
        self.assertEqual(facts['matching_conditions'], 20)
        self.assertEqual(len(facts['conditions']), 8)
        self.assertTrue(facts['truncated'])
        for reason in ('authority_file_invalid', 'authority_path_untrusted'):
            payload = {'schema': 'ags.routeb-fixture-command-error.v1', 'stage': 'verify-catalog', 'reason': reason}
            error = common.CommandFailed('verify_catalog', CompletedProcess([], 78, b'', canonical(payload)))
            with mock.patch.object(provision, 'command', side_effect=error):
                with self.assertRaises(common.CommandFailed) as caught:
                    provision.verify_catalog(Path('/unused-tool'), Path('/unused-input'), value, 'b' * 64)
            self.assertIs(caught.exception, error)
            self.assertEqual(error.diagnostic['returncode'], 78)
            self.assertEqual('catalog_file_conditions' in error.diagnostic, reason == 'authority_file_invalid')
            self.assertNotIn('private-', json.dumps(error.diagnostic))


    def test_diagnostic_problem_cannot_replace_the_refusal_or_run_on_success(self):
        import provision
        from subprocess import CompletedProcess
        payload = {'schema': 'ags.routeb-fixture-command-error.v1', 'stage': 'verify-catalog',
                   'reason': 'authority_file_invalid'}
        error = common.CommandFailed('verify_catalog', CompletedProcess([], 78, b'', canonical(payload)))
        with mock.patch.object(provision, 'command', side_effect=error), \
             mock.patch.object(authority, 'catalog_file_conditions', side_effect=ValueError('private-detail-sentinel')):
            with self.assertRaises(common.CommandFailed) as caught:
                provision.verify_catalog(Path('/unused-tool'), Path('/unused-input'), self.catalog(), 'b' * 64)
        self.assertIs(caught.exception, error)
        self.assertEqual(error.diagnostic['catalog_file_conditions'], {'authority_sha256': 'b' * 64, 'unavailable': True})
        self.assertNotIn('private-', json.dumps(error.diagnostic))
        with mock.patch.object(provision, 'command', return_value=b'original-success-bytes'), \
             mock.patch.object(authority, 'catalog_file_conditions') as diagnose:
            self.assertEqual(provision.verify_catalog(Path('/unused-tool'), Path('/unused-input'), self.catalog(), 'b' * 64),
                             b'original-success-bytes')
            diagnose.assert_not_called()


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
