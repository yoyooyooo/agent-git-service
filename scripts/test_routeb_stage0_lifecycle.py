#!/usr/bin/env python3
"""Fixture boundaries for the native parent chain; no host service is controlled."""
import copy
from contextlib import contextmanager, ExitStack, redirect_stderr, redirect_stdout
import importlib.util
import io
import os
from pathlib import Path
import re
import stat
import struct
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import MagicMock, Mock, patch

import routeb_host_native as native
import routeb_host_package as host_package
import routeb_native_bootstrap as bootstrap
import routeb_native_capture as capture
import routeb_stage0_client as peer_client
from upgrade_backup_contract import ContractError, canonical
from upgrade_identity import IdentityError


def identity(pid, parent, uid, gid, stamp, sha):
    return {'pid': pid, 'parent_pid': parent, 'uid': uid, 'gid': gid,
             'start_ticks': stamp, 'executable_sha256': sha * 64}


class ContextFailureDiagnosticsTests(unittest.TestCase):
    """Exercise the child boundary and existing CLI, without a native parent."""

    def setUp(self):
        self.root = '/fixture/account/.local/state/ags-routeb-native/example/ags'
        self.binding = {'tool_revision': 'a' * 40, 'tool_tree': 'b' * 40,
                        'tool_manifest_sha256': 'c' * 64}
        self.invocation = {'id': 'example-bootstrap', 'operation': 'bootstrap', 'phase': 'bootstrap',
                           'attempt_id': 'example-attempt', 'authority_epoch': 1,
                           'authorization_id': 'example-approval'}
        self.registration = {'id': 'example', 'binding': self.binding,
                             'native_image': {'sha256': 'a' * 64},
                             'interpreter': {'sha256': 'e' * 64}, 'invocations': [self.invocation]}
        self.host = {'root': self.root}
        self.context = {'schema': 'ags.routeb-stage0-context.v1', 'registration_id': 'example',
                        'registration_sha256': 'd' * 64, 'root': self.root,
                        **{k: v for k, v in self.invocation.items() if k != 'id'},
                        'invocation_id': self.invocation['id'],
                        'boot_id': '00000000-0000-4000-8000-000000000001',
                        'supervisor': identity(1101, 1, 0, 0, 100, 'a'),
                        'child': identity(1102, 1101, 1001, 1001, 101, 'b'),
                        'interpreter_authority_sha256': 'e' * 64, **self.binding,
                        'recovery_registration_sha256': '', 'scope_grant_sha256': ''}

    @contextmanager
    def peer(self, response=None, *, transport_error=None, local_error=None):
        if response is None:
            response = {'schema': peer_client.PROTOCOL, 'ok': True, 'context': self.context}
        raw = response if isinstance(response, bytes) else canonical(response)
        connection = MagicMock()
        connection.__enter__.return_value = connection
        connection.connect.side_effect = transport_error
        connection.getsockopt.return_value = struct.pack('3i', 1101, 0, 0)
        connection.recv.side_effect = [raw, b'']
        def metadata(path):
            socket_file = path.name.endswith('.sock')
            return SimpleNamespace(st_mode=(stat.S_IFSOCK | 0o600) if socket_file else (stat.S_IFDIR | 0o755),
                                   st_uid=1001 if socket_file else 0)
        with ExitStack() as stack:
            for name, value in (('getpid', 1102), ('getppid', 1101), ('getuid', 1001), ('getgid', 1001)):
                stack.enter_context(patch.object(peer_client.os, name, return_value=value))
            stack.enter_context(patch.object(peer_client, '_registered',
                               return_value=(self.registration, self.host, 'd' * 64), side_effect=local_error))
            stack.enter_context(patch.object(Path, 'lstat', autospec=True, side_effect=metadata))
            stack.enter_context(patch.object(peer_client.socket, 'socket', return_value=connection))
            stack.enter_context(patch.object(peer_client.socket, 'SO_PEERCRED', 17, create=True))
            stack.enter_context(patch.object(sys, 'argv', ['bootstrap', 'activate', '--root', self.root,
                                                         '--approval', '/fixture/approval.json']))
            read = stack.enter_context(patch.object(bootstrap, 'read_bindings'))
            stderr, stdout = io.StringIO(), io.StringIO()
            stack.enter_context(redirect_stderr(stderr)); stack.enter_context(redirect_stdout(stdout))
            yield stderr, stdout, read

    def refusal(self, expected, **kwargs):
        with self.peer(**kwargs) as (stderr, stdout, read):
            self.assertEqual(bootstrap.main(), 78)
            read.assert_not_called()
        self.assertEqual(stdout.getvalue(), '')
        lines = stderr.getvalue().splitlines()
        self.assertEqual(lines[-1], 'code=interpreter_full_closure_required')
        self.assertIn('code=' + expected, lines)
        self.assertNotIn('private-sentinel', stderr.getvalue())
        self.assertNotIn('/fixture/', stderr.getvalue())
        return lines

    def test_parent_refusal_survives_the_public_blocker(self):
        self.refusal('stage0_mapping_unknown', response={
            'schema': peer_client.PROTOCOL, 'ok': False, 'error': 'stage0_mapping_unknown'})

    def test_transport_failure_is_closed_and_keeps_exit_78(self):
        for error in (ConnectionRefusedError(61, 'private-sentinel /fixture/socket'),
                      TimeoutError('private-sentinel timeout')):
            with self.subTest(kind=type(error).__name__):
                self.refusal('stage0_context_transport_unavailable', transport_error=error)

    def test_other_peer_operations_keep_their_existing_os_failure(self):
        error = ConnectionRefusedError(61, 'private-sentinel')
        with self.peer(transport_error=error) as (stderr, _stdout, _read):
            with self.assertRaises(ConnectionRefusedError) as caught:
                peer_client._request('runtime_identity', root=self.root)
        self.assertIs(caught.exception, error)
        self.assertEqual(stderr.getvalue(), '')

    def test_local_identity_and_shape_failures_do_not_become_transport_failures(self):
        self.refusal('dependency_unavailable', local_error=IdentityError('dependency_unavailable', 69))
        self.refusal('stage0_context_local_unavailable', local_error=OSError('private-sentinel'))
        self.refusal('stage0_context_invalid', local_error=KeyError('private-sentinel'))

    def test_response_json_shape_and_context_binding_remain_distinct(self):
        self.refusal('json_invalid', response=b'private-sentinel not JSON')
        self.refusal('stage0_response_invalid', response={
            'schema': peer_client.PROTOCOL, 'ok': False, 'error': 'private-sentinel /fixture/path'})
        self.refusal('stage0_context_shape', response={
            'schema': peer_client.PROTOCOL, 'ok': True, 'context': {**self.context, 'extra': 'private-sentinel'}})
        self.refusal('stage0_context_binding', response={
            'schema': peer_client.PROTOCOL, 'ok': True, 'context': {**self.context, 'registration_sha256': 'f' * 64}})

    def test_go_context_fields_and_valid_child_consumer_agree(self):
        source = (Path(__file__).parents[1] / 'internal/routebstage0/types.go').read_text()
        declaration = re.search(r'type Context struct \{(.*?)\n\}', source, re.S).group(1)
        fields = set(re.findall(r'json:"([^"]+)"', declaration))
        self.assertEqual(len(fields), 19)
        self.assertEqual(fields, peer_client.CONTEXT_FIELDS)
        with self.peer() as (stderr, _stdout, read):
            self.assertEqual(host_package.require_interpreter_closure(self.root, ('bootstrap',)), self.context)
            read.assert_not_called()
        self.assertEqual(stderr.getvalue(), '')
        for field in fields:
            with self.subTest(missing=field), self.peer() as _captured:
                changed = {k: v for k, v in self.context.items() if k != field}
                with self.assertRaisesRegex(ContractError, '^stage0_context_shape$'):
                    peer_client._validate_context(changed, self.registration, self.host, 'd' * 64, 1101)

    def test_raw_or_cyclic_exception_causes_cannot_expand_public_diagnostics(self):
        error = ContractError('private-sentinel /fixture/path\nraw exception')
        error.__cause__ = error
        with patch.object(peer_client, 'context', side_effect=error), redirect_stderr(io.StringIO()) as output:
            with self.assertRaisesRegex(ContractError, '^interpreter_full_closure_required$'):
                host_package.require_interpreter_closure(self.root, ('bootstrap',))
        self.assertEqual(output.getvalue(), '')

    def test_failed_diagnostic_write_cannot_replace_the_refusal(self):
        error = ContractError('stage0_mapping_unknown')
        output = Mock()
        output.write.side_effect = OSError('private-sentinel')
        with patch.object(peer_client, 'context', side_effect=error), patch.object(sys, 'stderr', output):
            with self.assertRaisesRegex(ContractError, '^interpreter_full_closure_required$') as caught:
                host_package.require_interpreter_closure(self.root, ('bootstrap',))
        self.assertIs(caught.exception.__cause__, error)


class NativeTreeTests(unittest.TestCase):
    def setUp(self):
        self.root = Path('/fixture/account/.local/state/ags-routeb-native/example/ags')
        self.registration = {'registration_id': 'example'}
        self.parent = identity(1101, 1, 0, 0, 100, 'a')
        self.python = identity(1102, 1101, os.getuid(), os.getgid(), 101, 'b')
        self.application = identity(1103, 1102, os.getuid(), os.getgid(), 102, 'c')
        self.context = {
            'schema': 'ags.routeb-stage0-context.v1', 'registration_id': 'example',
            'registration_sha256': 'd' * 64, 'root': str(self.root), 'operation': 'serve',
            'phase': 'serve-candidate', 'invocation_id': 'serve-candidate-one', 'attempt_id': 'attempt-one',
            'authority_epoch': 1, 'authorization_id': 'approval-one', 'boot_id': 'fixture-boot',
            'recovery_registration_sha256': '',
            'supervisor': self.parent, 'child': self.python,
            'interpreter_authority_sha256': 'e' * 64, 'tool_revision': 'f' * 40,
            'tool_tree': 'a' * 40, 'tool_manifest_sha256': 'b' * 64}
        self.runtime = {'root': str(self.root), 'pid': 1103, 'supervisor_pid': 1102,
                        'stage0': copy.deepcopy(self.context)}
        self.manager = {'kind': 'systemd', 'scope': 'system', 'pid': 1101,
                        'target': 'ags-hostpkg-test-example.service'}
        self.observed = {'context': self.context, 'pids': [self.parent, self.python, self.application],
                         'manager_unit': self.manager['target']}

    def read_tree(self, observed=None):
        client = SimpleNamespace(runtime_identity=Mock(return_value=observed or self.observed))
        def process(pid, binary=None, **_kwargs):
            return {'pid': pid, 'parent_pid': {1103: 1102, 1102: 1101}[pid],
                    'started': 'same-second', 'precision': 'second'}
        with patch.object(capture, 'stage0_client', return_value=client), \
                patch.object(capture, 'process_identity', side_effect=process), \
                patch.object(capture, 'hash_file', return_value='c' * 64):
            value = capture.runtime_processes(self.root, self.runtime, self.manager,
                        self.root / 'bin/gh-server', stage0=self.registration)
        client.runtime_identity.assert_called_once_with(self.root)
        return value

    def test_exact_root_python_application_chain_has_kernel_births(self):
        result = self.read_tree()
        self.assertEqual(result['child']['start_ticks'], 102)
        self.assertEqual(result['supervisor']['start_ticks'], 101)
        self.assertEqual(result['stage0']['supervisor']['start_ticks'], 100)
        self.assertEqual(result['stage0_sha256'], capture.sha(capture.canonical(self.context)))

    def test_serialized_context_cannot_replace_online_root_observation(self):
        self.runtime['stage0']['registration_sha256'] = 'f' * 64
        with self.assertRaisesRegex(capture.CaptureError, 'stage0_runtime_context_mismatch'):
            self.read_tree()

    def test_same_pid_with_changed_python_birth_is_not_the_recorded_generation(self):
        self.python['start_ticks'] += 1
        with self.assertRaisesRegex(capture.CaptureError, 'stage0_runtime_context_mismatch'):
            self.read_tree()

    def test_manager_must_be_the_direct_root_parent(self):
        self.manager['pid'] = 1110
        with self.assertRaisesRegex(capture.CaptureError, 'stage0_process_parent_mismatch'):
            self.read_tree()

    def test_malformed_or_unrelated_process_tree_refuses(self):
        for mutation in ('duplicate', 'orphan', 'wrong-binary', 'bool-birth'):
            value = copy.deepcopy(self.observed)
            if mutation == 'duplicate':
                value['pids'].append(copy.deepcopy(value['pids'][0]))
            elif mutation == 'orphan':
                value['pids'].append(identity(1200, 1201, os.getuid(), os.getgid(), 103, 'd'))
            elif mutation == 'wrong-binary':
                value['pids'][2]['executable_sha256'] = 'f' * 64
            else:
                value['pids'][2]['start_ticks'] = True
            with self.subTest(mutation=mutation), self.assertRaises(capture.CaptureError):
                self.read_tree(value)

    def test_reused_application_pid_changes_first_sample_anchor(self):
        before = self.read_tree()
        self.application['start_ticks'] += 1
        after = self.read_tree()
        self.assertEqual(before['stage0_sha256'], after['stage0_sha256'])
        self.assertNotEqual(before['stage0_tree_sha256'], after['stage0_tree_sha256'])
        anchor = {key: 'c' * 64 for key in
                  ('runtime_sha256', 'installation_sha256', 'binary_sha256', 'manager_sha256')}
        anchor.update({key: before[key] for key in ('stage0_sha256', 'stage0_tree_sha256')})
        receipt = {'boot_id': 'fixture-boot', 'generation': 'one', 'candidate': {'revision': 'a' * 40},
                   'validated_start': anchor}
        facts = {**anchor, 'boot_id': 'fixture-boot', 'generation': 'one', 'revision': 'a' * 40,
                 'stage0_tree_sha256': after['stage0_tree_sha256'], 'health': {}}
        with self.assertRaisesRegex(capture.CaptureError, 'stage0_identity_changed'):
            capture.validate_sample(facts, receipt)


class BrokerManagerTests(unittest.TestCase):
    def setUp(self):
        self.root = Path('/fixture/account/.local/state/ags-routeb-native/example/ags')
        spec = {'kind': 'systemd', 'scope': 'system', 'target': 'ags-hostpkg-test-example.service',
                'definition': '/etc/systemd/system/ags-hostpkg-test-example.service',
                'definition_sha256': 'a' * 64}
        profile = {'schema': capture.STAGE0_PROFILE, 'stage0': {'registration_id': 'example'},
                   'capture': {'manager': spec}}
        self.manager = native.FixedManager(self.root, profile, SimpleNamespace(
            scope='isolated-native', recheck=lambda: None))
        self.client = SimpleNamespace(manager_action=Mock(), context=Mock(return_value={
            'root': str(self.root), 'registration_id': 'example'}))

    def test_only_stop_and_start_reach_registered_broker(self):
        with patch.object(capture, 'stage0_client', return_value=self.client), patch.object(native, 'run') as command:
            self.manager._systemd('stop')
            self.manager._systemd('start')
            with self.assertRaises(ContractError):
                self.manager._systemd('restart')
        self.assertEqual(self.client.manager_action.call_args_list, [
            unittest.mock.call(self.root, 'stop'), unittest.mock.call(self.root, 'start')])
        command.assert_not_called()

    def test_unknown_start_is_not_retried_in_the_same_coordinator(self):
        self.client.manager_action.side_effect = TimeoutError
        with patch.object(capture, 'stage0_client', return_value=self.client), \
                patch('routeb_host_package.require_interpreter_closure'), \
                patch.object(self.manager, 'definition'):
            with self.assertRaises(TimeoutError):
                self.manager.start()
            with self.assertRaisesRegex(ContractError, 'start_already_requested'):
                self.manager.start()
        self.assertEqual(self.client.manager_action.call_count, 1)

    def test_root_unit_requires_fixed_entry_mixed_signal_and_no_environment(self):
        entries = {
            'Type': 'simple', 'User': 'root', 'Group': 'root', 'Restart': 'no',
            'KillMode': 'mixed', 'SendSIGKILL': 'no', 'TimeoutStopSec': '120',
            'ExecStart': '/usr/local/libexec/ags-routeb-native/example/stage0/ags-routeb-stage0'
                         ' serve --registration example --operation serve',
            'ExecStopPost': '/usr/local/libexec/ags-routeb-native/example/stage0/ags-routeb-stage0'
                            ' manager-complete --registration example --operation serve'}
        def definition(*_args, **_kwargs):
            return ('[Service]\n' + ''.join(f'{key}={value}\n' for key, value in entries.items())).encode()
        properties = {'DropInPaths': '', 'Restart': 'no', 'KillMode': 'mixed', 'SendSIGKILL': 'no',
                      'TimeoutStopUSec': '2min', 'FragmentPath': self.manager.spec['definition']}
        properties['ExecStopPost'] = (
            '{ path=/usr/local/libexec/ags-routeb-native/example/stage0/ags-routeb-stage0 ; argv[]='
            + entries['ExecStopPost'] + ' ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a]'
            ' ; pid=0 ; code=(null) ; status=0/0 }')
        with patch.object(capture, 'read_regular', side_effect=definition), \
                patch.object(capture, 'sha', return_value='a' * 64), \
                patch.object(capture, 'stage0_client', return_value=self.client), \
                patch.object(native, 'systemd_properties', return_value=properties):
            self.manager.definition()
            for key, value in [('KillMode', 'control-group'), ('User', 'fixture'),
                               ('SendSIGKILL', 'yes'),
                               ('Environment', 'LD_PRELOAD=/fixture/untrusted.so'),
                               ('ExecStopPost', '/bin/true'),
                               ('ExecStart', '/bin/sh -c anything')]:
                previous = entries.get(key)
                entries[key] = value
                with self.subTest(key=key), self.assertRaises(ContractError):
                    self.manager.definition()
                if previous is None:
                    del entries[key]
                else:
                    entries[key] = previous
            del entries['SendSIGKILL']
            with self.assertRaisesRegex(ContractError, 'stage0_manager_definition_required'):
                self.manager.definition()
            entries['SendSIGKILL'] = 'no'
            for value in (None, 'yes'):
                properties['SendSIGKILL'] = value
                with self.subTest(effective=value), self.assertRaisesRegex(ContractError, 'stage0_manager_force_kill_enabled'):
                    self.manager.definition()
            properties['SendSIGKILL'] = 'no'
            original = properties['ExecStopPost']
            for value in ('', original + ' ' + original,
                          original.replace('ignore_errors=no', 'ignore_errors=yes'),
                          original.replace('--operation serve', '--operation serve --unsafe')):
                properties['ExecStopPost'] = value
                with self.subTest(hook=value), self.assertRaisesRegex(ContractError, 'stage0_manager_completion_command_changed'):
                    self.manager.definition()

    def stop_fixture(self, *, mutation=None, runtime_mutation=None, manager_result='success', manager_code='0', phase='serve-candidate'):
        tree = NativeTreeTests()
        tree.setUp()
        context = tree.context
        context.update(phase=phase, invocation_id=phase + '-one')
        tree.runtime['stage0'] = copy.deepcopy(context)
        self.manager.before = {'stage0': context, 'runtime': {**tree.runtime, 'generation': 'one'},
                               'pids': {1101, 1102, 1103}, 'boot_id': 'fixture-boot'}
        stopped = {**tree.runtime, 'generation': 'one', 'exit_code': 0, 'forced_kill': False,
                   'stop_requested': True, 'process_group_empty': True}
        if runtime_mutation is not None:
            stopped.update(runtime_mutation)
        exit_record = {'schema': 'ags.routeb-stage0-exit.v1',
                       **{key: copy.deepcopy(context[key]) for key in
                          ('registration_sha256', 'invocation_id', 'phase', 'boot_id', 'supervisor', 'child')},
                       'child_exit_code': 0, 'signal_forwarded': True, 'forced_kill': False,
                       'context_verified': True, 'authority_rechecked': True, 'normal_exit': True}
        manager_id = 'e' * 32
        exit_record['manager_completion'] = {
            'schema': 'ags.routeb-stage0-manager-completion.v1',
            **{key: context[key] for key in ('registration_sha256', 'invocation_id', 'phase', 'boot_id')},
            'unit': self.manager.spec['target'], 'manager_invocation_id': manager_id,
            'start_anchor_sha256': '1' * 64, 'context_sha256': '2' * 64, 'exit_sha256': '3' * 64,
            'hook': identity(1104, 1, 0, 0, 103, 'a'),
            'manager': {'invocation_id': manager_id, 'main_pid': 0,
                        'exec_main_pid': context['supervisor']['pid'], 'control_pid': 1104,
                        'exec_main_code': 1, 'exec_main_status': 0, 'result': 'success',
                        'service_result': 'success', 'exit_code': 'exited', 'exit_status': '0'}}
        if mutation is not None:
            mutation(exit_record)
        self.client.runtime_exit = Mock(return_value=exit_record)
        def manager_status(_spec, names):
            values = {'MainPID': '0', 'ActiveState': 'inactive', 'SubState': 'dead',
                      'ExecMainCode': manager_code, 'ExecMainStatus': '0', 'Result': manager_result}
            return {key: values[key] for key in names}
        with patch('routeb_host_package.require_interpreter_closure'), \
                patch.object(self.manager, 'definition'), patch.object(self.manager, '_systemd'), \
                patch.object(native, 'systemd_properties', side_effect=manager_status), \
                patch.object(native, 'absent', return_value=True), \
                patch.object(capture, 'stage0_client', return_value=self.client), \
                patch.object(capture, 'boot_identity', return_value=('fixture-boot', b'fixture-boot')), \
                patch.object(capture, 'read_regular', return_value=capture.canonical(stopped)):
            return self.manager.stop()

    def test_normal_stop_binds_root_exit_and_actual_manager_exit(self):
        result = self.stop_fixture()
        self.assertTrue(result['stage0_exit']['normal_exit'])
        self.assertEqual(result['stage0_exit']['manager_completion']['manager']['exec_main_code'], 1)
        self.client.runtime_exit.assert_called_once_with(self.root, 'serve-candidate-one')

    def test_new_controller_rebinds_exact_root_stop_without_repeating_a_stop(self):
        from upgrade_backup_contract import fingerprint
        result = self.stop_fixture(phase='serve-initial',
            runtime_mutation={'schema': 'ags.runtime.v1', 'stopped_at': 'fixture-time'})
        context = result['stage0']
        exited = result['stage0_exit']
        proof = {'schema': 'ags.routeb-stage0-stopped.v1',
                 'runtime': {'context': context, 'manager_unit': self.manager.spec['target'],
                             'pids': [context['supervisor'], context['child'], identity(1103, 1102, 501, 20, 102, 'c')]},
                 'exit': {k: v for k, v in exited.items() if k != 'manager_completion'},
                 'manager_completion': exited['manager_completion'],
                 'transaction_invocation_id': 'transaction-one', 'failed_controller_invocations': ['transaction-one']}
        raw = capture.canonical({k: v for k, v in result.items()
                if k not in ('supervisor_exited', 'supervisor_exit_code', 'runtime_sha256', 'stage0_exit')})
        self.assertEqual(capture.sha(raw), result['runtime_sha256'])
        client = SimpleNamespace(stopped_identity=Mock(return_value=proof), manager_action=Mock())
        cold = {'runtime_generation': 'one', 'stop_evidence_sha256': fingerprint(result)}
        with patch.object(capture, 'stage0_client', return_value=client), \
                patch.object(capture, 'boot_identity', return_value=('fixture-boot', b'')), \
                patch.object(capture, 'read_regular', return_value=raw), \
                patch.object(native, 'absent', return_value=True), patch.object(native.FixedManager, 'definition'):
            reopened = native.FixedManager(self.root, self.manager.profile, self.manager.host)
            self.assertEqual(reopened.rebind_stopped(cold), result)
            self.assertEqual(reopened.before['pids'], {1101, 1102, 1103})
            client.manager_action.assert_not_called()
            bad = native.FixedManager(self.root, self.manager.profile, self.manager.host)
            with self.assertRaisesRegex(ContractError, 'original_stop_record_changed'):
                bad.rebind_stopped({**cold, 'stop_evidence_sha256': 'f' * 64})
            self.assertIsNone(bad.before); self.assertIsNone(bad.stopped)
            client.stopped_identity.side_effect = ContractError('stage0_candidate_start_attempted_or_unknown')
            with self.assertRaisesRegex(ContractError, 'stage0_candidate_start_attempted_or_unknown'):
                bad.rebind_stopped(cold)
            client.manager_action.assert_not_called()

    def test_gc_zero_cannot_replace_the_root_manager_completion(self):
        for manager_code in ('0', '1'):
            self.manager.called.clear()
            with self.subTest(code=manager_code), self.assertRaisesRegex(ContractError, 'stage0_manager_completion_unproven'):
                self.stop_fixture(manager_code=manager_code, mutation=lambda value: value.pop('manager_completion'))

    def test_wrong_manager_incarnation_control_process_or_signal_is_rejected(self):
        mutations = [
            lambda value: value.update(invocation_id='other'),
            lambda value: value.update(unit='other.service'),
            lambda value: value.update(manager_invocation_id='f' * 32),
            lambda value: value.update(context_sha256='not-a-hash'),
            lambda value: value['manager'].update(control_pid=9999),
            lambda value: value['manager'].update(exec_main_pid=9999),
            lambda value: value['manager'].update(exec_main_code=2),
            lambda value: value['manager'].update(exec_main_status=78),
            lambda value: value['manager'].update(exit_code='killed', exit_status='TERM'),
            lambda value: value['hook'].update(uid=1000),
            lambda value: value['hook'].update(parent_pid=9999),
            lambda value: value['hook'].update(executable_sha256='f' * 64),
        ]
        for mutate in mutations:
            self.manager.called.clear()
            with self.subTest(mutation=mutate), self.assertRaisesRegex(ContractError, 'stage0_manager_completion_unproven'):
                self.stop_fixture(mutation=lambda value: mutate(value['manager_completion']))

    def test_root_receipt_cannot_predict_a_successful_manager_exit(self):
        with self.assertRaisesRegex(ContractError, 'stage0_manager_exit_unproven'):
            self.stop_fixture(manager_result='signal')

    def test_forced_or_wrong_birth_exit_is_not_normal(self):
        for mutate in (lambda value: value.update(forced_kill=True),
                       lambda value: value.update(child_exit_code=False),
                       lambda value: value['supervisor'].update(start_ticks=999)):
            self.manager.called.clear()
            with self.subTest(mutation=mutate), self.assertRaisesRegex(ContractError, 'stage0_normal_stop_unproven'):
                self.stop_fixture(mutation=mutate)

    def test_failed_native_supervision_cannot_be_normalized_to_zero(self):
        for mutation in ({'supervision_failed': True}, {'stop_timed_out': True},
                         {'supervisor_exit_code': 78}, {'supervisor_exit_code': False}):
            self.manager.called.clear()
            with self.subTest(mutation=mutation), self.assertRaisesRegex(ContractError, 'stage0_supervision_failed'):
                self.stop_fixture(runtime_mutation=mutation)


class PeerCompletionTests(unittest.TestCase):
    def test_runtime_exit_requires_separate_peer_completion_without_reencoding_exit(self):
        original = {'schema': 'ags.routeb-stage0-exit.v1'}
        completion = {'schema': 'ags.routeb-stage0-manager-completion.v1', 'exit_sha256': 'a' * 64}
        with patch.object(peer_client, '_request', return_value={'exit': original, 'manager_completion': completion}):
            value = peer_client.runtime_exit('/fixture/root', 'serve-one')
        self.assertEqual(value, {**original, 'manager_completion': completion})
        self.assertNotIn('manager_completion', original)
        for response in ({'exit': original}, {'exit': original, 'manager_completion': None},
                         {'exit': {**original, 'manager_completion': completion}, 'manager_completion': completion}):
            with self.subTest(response=response), patch.object(peer_client, '_request', return_value=response), \
                    self.assertRaisesRegex(ContractError, 'stage0_manager_completion_unproven'):
                peer_client.runtime_exit('/fixture/root', 'serve-one')


class RootWriterProbeTests(unittest.TestCase):
    def setUp(self):
        self.root = Path('/fixture/account/.local/state/ags-routeb-native/example/ags')
        self.manager = SimpleNamespace(
            root=self.root, native_registration=lambda: {'registration_id': 'example'},
            before={'boot_id': 'fixture-boot', 'pids': {1101, 1102, 1103}},
            stopped={'stage0_exit': {'normal_exit': True}})
        self.group = {'objects': [{'id': 'database', 'source': '/fixture/database'}]}
        self.client = SimpleNamespace(writer_probe=Mock())

    def probe(self, result, current=None):
        self.client.writer_probe.return_value = result
        with patch.object(capture, 'stage0_client', return_value=self.client), \
                patch.object(capture, 'boot_identity', return_value=('fixture-boot', b'')), \
                patch.object(native, 'processes', return_value=current or {}), \
                patch.object(native, 'run') as command:
            value = native.WriterProbe(self.manager, self.group, lambda: 'generation-one')()
        command.assert_not_called()
        return value

    def test_root_owned_writers_are_observed_by_registered_peer(self):
        observed = {'generation': 'generation-one', 'closed': False, 'unknown': [],
                    'writers': [{'kind': 'holder', 'object': 'database', 'count': 1}]}
        self.assertEqual(self.probe(observed), observed)
        self.client.writer_probe.assert_called_once_with(self.root, 'generation-one')

    def test_only_a_complete_root_observation_can_report_closed(self):
        result = {'generation': 'generation-one', 'closed': True, 'writers': [], 'unknown': []}
        self.assertEqual(self.probe(result), result)
        for mutation in ({'generation': 'other'}, {'unknown': ['denied']}, {'closed': 1},
                         {'writers': [{'kind': 'holder', 'object': 'database', 'count': 1}]},
                         {'closed': False, 'writers': [{'kind': 'holder', 'object': 'other', 'count': 1}]},
                         {'closed': False, 'writers': [{'kind': 'listener', 'count': True}]},
                         {'closed': False, 'writers': [{'kind': 'listener', 'count': 1}] * 2}):
            with self.subTest(mutation=mutation), self.assertRaisesRegex(ContractError, 'writer_probe_unknown'):
                self.probe({**result, **mutation})

    def test_stop_proof_and_process_absence_precede_writer_observation(self):
        result = {'generation': 'generation-one', 'closed': True, 'writers': [], 'unknown': []}
        with self.assertRaisesRegex(ContractError, 'normal_stop_unproven'):
            self.probe(result, current={1103: {'pid': 1103}})
        self.manager.stopped = None
        with self.assertRaisesRegex(ContractError, 'normal_stop_unproven'):
            self.probe(result)
        self.client.writer_probe.assert_not_called()


class RuntimePeerTests(unittest.TestCase):
    def setUp(self):
        spec = importlib.util.spec_from_file_location('stage0_fixture_runtime',
                                                     Path(__file__).with_name('ags-runtime.py'))
        self.runtime = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(self.runtime)
        self.root = Path(tempfile.mkdtemp(prefix='routeb-stage0-runtime-')).resolve() / 'ags'
        self.root.mkdir(mode=0o700)
        (self.root / 'state').mkdir(mode=0o700)
        self.context = {
            'schema': 'ags.routeb-stage0-context.v1', 'root': str(self.root), 'operation': 'serve',
            'supervisor': {'pid': os.getppid(), 'uid': 0, 'gid': 0},
            'child': {'pid': os.getpid(), 'parent_pid': os.getppid(), 'uid': os.getuid(), 'gid': os.getgid()},
            'invocation_id': 'fixture-serve'}

    def test_missing_live_context_refuses_before_program_spawn(self):
        callback = Mock(side_effect=ContractError('stage0_peer_unavailable'))
        with patch.object(self.runtime.subprocess, 'Popen') as spawn, self.assertRaises(ContractError):
            self.runtime.supervise(self.root, {}, {}, Mock(), Mock(), native_context=callback)
        spawn.assert_not_called()

    def test_context_snapshot_does_not_share_mutable_callback_state(self):
        frozen = self.runtime._native_context(self.root, lambda: self.context)
        self.context['invocation_id'] = 'replacement'
        self.assertEqual(frozen['invocation_id'], 'fixture-serve')
        with self.assertRaisesRegex(ValueError, 'native runtime context changed'):
            self.runtime._native_context(self.root, lambda: self.context, frozen)

    def test_peer_is_rechecked_after_spawn_and_before_normal_exit(self):
        callback = Mock(side_effect=lambda: copy.deepcopy(self.context))
        child = Mock(pid=123, stdout=io.BytesIO())
        child.wait.return_value = child.poll.return_value = 0
        selector = Mock()
        selector.get_map.return_value = False
        with patch.object(self.runtime.subprocess, 'Popen', return_value=child), \
                patch.object(self.runtime.selectors, 'DefaultSelector', return_value=selector), \
                patch.object(self.runtime.signal, 'signal'), \
                patch.object(self.runtime.os, 'killpg', side_effect=ProcessLookupError):
            self.assertEqual(self.runtime.supervise(self.root, {}, {}, Mock(), Mock(),
                                                    native_context=callback), 0)
        self.assertEqual(callback.call_count, 3)
        saved = capture.object_json((self.root / 'state/runtime.json').read_bytes())
        self.assertEqual(saved['stage0'], self.context)
        self.assertEqual(saved['exit_code'], 0)

    def test_lost_peer_after_spawn_retains_only_the_owned_child_until_exit(self):
        callback = Mock(side_effect=[self.context, ContractError('stage0_peer_unavailable')])
        reader, writer = os.pipe()
        os.close(writer)
        child = Mock(pid=123, stdout=os.fdopen(reader, 'rb', buffering=0))
        polls = iter([None])
        child.poll.side_effect = lambda: next(polls, 0)
        child.wait.return_value = 0
        def signal_group(_pid, signum):
            if signum == 0:
                raise ProcessLookupError
        with patch.object(self.runtime.subprocess, 'Popen', return_value=child), \
                patch.object(self.runtime.signal, 'signal'), \
                patch.object(self.runtime.os, 'killpg', side_effect=signal_group) as signals, \
                self.assertRaisesRegex(self.runtime.NativeSupervisionFailure, 'native_supervision_failed'):
            self.runtime.supervise(self.root, {}, {}, Mock(), Mock(), native_context=callback)
        self.assertEqual(signals.call_args_list, [unittest.mock.call(123, self.runtime.signal.SIGTERM),
                                                unittest.mock.call(123, 0)])
        child.wait.assert_called_once_with()
        saved = capture.object_json((self.root / 'state/runtime.json').read_bytes())
        self.assertTrue(saved['supervision_failed'])
        self.assertEqual(saved['supervisor_exit_code'], 78)


if __name__ == '__main__':
    unittest.main()
