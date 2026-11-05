#!/usr/bin/env python3
"""Synthetic time tests bookkeeping, never a native 600-second acceptance."""
import copy
import os
from pathlib import Path
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest.mock import patch

import routeb_native_capture as capture


class FakeClock:
    def __init__(self, jump=0):
        self.now, self.jump = 10_000_000_000, jump

    def monotonic_ns(self):
        return self.now

    def sleep(self, seconds):
        self.now += int((seconds + self.jump) * 1_000_000_000)


class FakeProbe:
    def __init__(self, facts, mutate=None):
        self.facts, self.mutate, self.calls, self.closed = facts, mutate, 0, False

    def sample(self):
        self.calls += 1
        result = copy.deepcopy(self.facts)
        if self.mutate and self.calls > 1:
            self.mutate(result)
        return result, {'health.json': (b'{"status":"ready"}\n', 'fixture_readyz')}

    def log_delta(self):
        return {'log-test.bin': (b'fixture line\n', 'fixture_log')}, []

    def close(self):
        self.closed = True


class CaptureTests(unittest.TestCase):
    def setUp(self):
        # Retain fixtures for debugging; no production root or service is used.
        self.folder = Path(tempfile.mkdtemp(prefix='routeb-capture-')).resolve()
        self.folder.chmod(0o700)
        self.root = self.folder / 'ags'
        (self.root / 'state').mkdir(parents=True, mode=0o700)
        self.definition = self.root / 'job.plist'
        self.put(self.definition, b'isolated fixture definition\n')
        self.profile_path, self.receipt_path = self.folder / 'profile.json', self.folder / 'execution.json'
        self.profile = {'capture': {
            'mode': 'isolated-native',
            'manager': {'kind': 'launchd', 'target': 'gui/501/com.jackie.ags-hostpkg-test.example',
                        'definition': str(self.definition), 'definition_sha256': capture.sha(self.definition.read_bytes())},
            'runtime_path': str(self.root / 'state/runtime.json'),
            'installation_path': str(self.root / 'state/installation.json'),
            'binary_path': str(self.root / 'bin/gh-server'),
            'health': {'local': 'http://127.0.0.1:19001/readyz', 'service': 'https://service.example/readyz'},
            'logs': [{'name': 'server', 'path': str(self.root / 'state/server.log')}],
            'duration_seconds': 600, 'lease_seconds': 0, 'interval_seconds': 30, 'max_gap_seconds': 60}}
        self.receipt = {
            'schema': 'ags.routeb-host-execution.v1', 'status': 'awaiting_observation',
            'artifact_kind': 'exact-source', 'host': 'test', 'host_instance': 'test-host',
            'transaction_id': 'test-transaction', 'root': str(self.root),
            'generation': 'generation-one', 'boot_id': 'test-boot',
            'candidate': {'revision': 'a' * 40, 'tree': 'b' * 40, 'binary_sha256': 'c' * 64,
                          'manifest_sha256': 'd' * 64, 'version_identity_sha256': 'e' * 64},
            'tool': {'revision': 'f' * 40, 'tree': 'a' * 40, 'bundle_sha256': 'b' * 64},
            'validated_start': {'monotonic_ns': 1_000_000_000, 'runtime_sha256': 'c' * 64,
                                'installation_sha256': 'd' * 64, 'binary_sha256': 'c' * 64,
                                'manager_sha256': 'e' * 64,
                                'log_baseline': [{'name': 'server', 'path': str(self.root / 'state/server.log'),
                                                  'device': 1, 'inode': 1, 'offset': 7}]},
            'start_once': {'attempted': True, 'token_consumed': True, 'manager_requested': True}}
        self.startup = self.folder / 'startup-evidence'
        self.startup.mkdir(mode=0o700)
        startup_log = b'before\n'
        self.put(self.startup / 'log-server.bin', startup_log)
        self.startup_manifest = {
            'schema': 'ags.routeb-startup-evidence.v1',
            'artifacts': [{'path': 'log-server.bin', 'sha256': capture.sha(startup_log),
                           'bytes': len(startup_log), 'kind': 'raw_log_delta'}],
            'startup_log_ranges': [{'name': 'server', 'device': 1, 'inode': 1, 'start': 0,
                                    'end': len(startup_log), 'sha256': capture.sha(startup_log)}],
            'trash_moves': []}
        self.save_startup()
        self.facts = {'boot_id': 'test-boot', 'generation': 'generation-one', 'revision': 'a' * 40,
                      'runtime_sha256': 'c' * 64, 'installation_sha256': 'd' * 64,
                      'binary_sha256': 'c' * 64, 'manager_sha256': 'e' * 64,
                      'child': {'pid': 100, 'started': 'time-one'},
                      'supervisor': {'pid': 99, 'started': 'time-one'}, 'manager': {'pid': 99},
                      'health': {key: {'http_status': 200, 'status': 'ready', 'revision': 'a' * 40}
                                 for key in ('local', 'service')}}
        self.save_inputs()

    def put(self, path, raw):
        path.write_bytes(raw)
        path.chmod(0o600)

    def save_inputs(self):
        raw = capture.canonical(self.profile)
        self.put(self.profile_path, raw)
        self.receipt['profile_sha256'] = capture.sha(raw)
        self.put(self.receipt_path, capture.canonical(self.receipt))

    def save_startup(self):
        raw = capture.canonical(self.startup_manifest)
        self.put(self.startup / 'manifest.json', raw)
        self.receipt['startup_evidence'] = {'path': 'startup-evidence/manifest.json',
                                           'sha256': capture.sha(raw)}

    def run_capture(self, probe=None, clock=None):
        return capture.capture(str(self.receipt_path), str(self.profile_path), self.folder / 'capture',
                               probe=probe or FakeProbe(self.facts), clock=clock or FakeClock())

    def test_synthetic_window_remains_fixture(self):
        probe = FakeProbe(self.facts)
        result = self.run_capture(probe)
        self.assertEqual((result['status'], result['kind']), ('observed', 'fixture'))
        self.assertFalse(result['accepted'])
        self.assertFalse(result['installable'])
        self.assertGreaterEqual(result['elapsed_ns'], 600_000_000_000)
        self.assertEqual(len(result['samples']), 21)
        self.assertTrue(probe.closed)
        for artifact in result['artifacts']:
            path = self.folder / 'capture' / artifact['path']
            self.assertEqual(capture.sha(path.read_bytes()), artifact['sha256'])
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)

    def test_599_seconds_refused_without_output(self):
        self.profile['capture']['duration_seconds'] = 599
        self.save_inputs()
        with self.assertRaisesRegex(capture.CaptureError, 'capture_timing_invalid'):
            self.run_capture()
        self.assertFalse((self.folder / 'capture').exists())

    def test_clock_jump_is_not_coverage(self):
        result = self.run_capture(clock=FakeClock(jump=600))
        self.assertEqual((result['status'], result['reason']), ('interrupted', 'sample_gap'))
        self.assertEqual(len(result['samples']), 1)
        self.assertTrue((self.folder / 'capture/capture.json').is_file())

    def test_delayed_first_sample_is_not_a_continuous_start_window(self):
        clock = FakeClock()
        clock.now += 600_000_000_000
        with self.assertRaisesRegex(capture.CaptureError, 'first_sample_gap'):
            self.run_capture(clock=clock)
        self.assertFalse((self.folder / 'capture').exists())

    def test_lease_extends_samples(self):
        self.profile['capture']['lease_seconds'] = 630
        self.save_inputs()
        result = self.run_capture()
        self.assertEqual((result['required_seconds'], len(result['samples'])), (630, 22))

    def test_restarted_process_retains_failed_sample(self):
        probe = FakeProbe(self.facts, lambda facts: facts['child'].update(pid=101))
        result = self.run_capture(probe)
        self.assertEqual(result['reason'], 'process_identity_changed')
        self.assertEqual(len(result['samples']), 2)
        self.assertTrue(probe.closed)

    def test_input_replacement_interrupts(self):
        def mutate(_facts):
            self.put(self.receipt_path, capture.canonical({**self.receipt, 'transaction_id': 'replacement'}))
        self.assertEqual(self.run_capture(FakeProbe(self.facts, mutate))['reason'], 'capture_input_replaced')

    def test_startup_manifest_is_required_and_original_log_bytes_are_bound(self):
        binding = self.receipt.pop('startup_evidence')
        self.save_inputs()
        with self.assertRaisesRegex(capture.CaptureError, 'startup_manifest_missing'):
            self.run_capture()
        self.receipt['startup_evidence'] = binding
        self.save_inputs()
        self.put(self.startup / 'manifest.json', capture.canonical(self.startup_manifest) + b' ')
        with self.assertRaisesRegex(capture.CaptureError, 'startup_manifest_changed'):
            self.run_capture()
        self.save_startup()
        self.put(self.startup / 'log-server.bin', b'changed')
        with self.assertRaisesRegex(capture.CaptureError, 'startup_artifact_changed'):
            self.run_capture()
        self.assertFalse((self.folder / 'capture').exists())

    def test_startup_range_must_end_at_the_continuous_log_baseline(self):
        self.startup_manifest['startup_log_ranges'][0]['end'] -= 1
        self.save_startup()
        self.save_inputs()
        with self.assertRaisesRegex(capture.CaptureError, 'startup_log_gap'):
            self.run_capture()
        self.assertFalse((self.folder / 'capture').exists())

    def test_startup_artifact_replacement_during_capture_is_not_accepted(self):
        def mutate(_facts):
            self.put(self.startup / 'log-server.bin', b'changed')
        result = self.run_capture(FakeProbe(self.facts, mutate))
        self.assertEqual((result['status'], result['reason']), ('interrupted', 'startup_artifact_changed'))
        self.assertEqual(result['startup_manifest_sha256'], self.receipt['startup_evidence']['sha256'])

    def test_boot_generation_and_health_fail_closed(self):
        for field, value, reason in [('boot_id', 'other', 'boot_changed'), ('generation', 'other', 'generation_changed')]:
            with self.assertRaisesRegex(capture.CaptureError, reason):
                capture.validate_sample({**self.facts, field: value}, self.receipt)
        facts = copy.deepcopy(self.facts)
        facts['health']['service']['http_status'] = 503
        with self.assertRaisesRegex(capture.CaptureError, 'health_not_ready'):
            capture.validate_sample(facts, self.receipt)
        facts['health'] = {}
        with self.assertRaisesRegex(capture.CaptureError, 'health_profile_invalid'):
            capture.validate_sample(facts, self.receipt)

    def test_profile_and_production_label_substitution(self):
        self.put(self.profile_path, capture.canonical({**self.profile, 'unbound': True}))
        with self.assertRaisesRegex(capture.CaptureError, 'profile_replaced'):
            self.run_capture()
        self.profile['capture']['manager']['target'] = 'gui/501/com.jackie.agent-git-service'
        self.save_inputs()
        with self.assertRaisesRegex(capture.CaptureError, 'native_label_rejected'):
            self.run_capture()

    def test_bad_head_and_integer_booleans(self):
        self.receipt['tool']['revision'] = 'z' * 40
        self.save_inputs()
        with self.assertRaisesRegex(capture.CaptureError, 'digest_invalid'):
            self.run_capture()
        self.receipt['tool']['revision'] = 'f' * 40
        self.receipt['start_once']['token_consumed'] = 1
        self.save_inputs()
        with self.assertRaisesRegex(capture.CaptureError, 'start_once_unproven'):
            self.run_capture()

    def test_symlink_and_existing_output_rejected(self):
        alias = self.folder / 'alias.json'
        alias.symlink_to(self.receipt_path)
        with self.assertRaisesRegex(capture.CaptureError, 'path_symlink'):
            capture.read_regular(alias)
        self.run_capture()
        with self.assertRaises(FileExistsError):
            self.run_capture()

    def test_exact_log_range_and_rotation_rejected(self):
        path = self.root / 'state/server.log'
        self.put(path, b'before\n')
        self.receipt['validated_start']['log_baseline'][0].update(device=path.stat().st_dev, inode=path.stat().st_ino)
        probe = capture.NativeProbe(self.receipt, self.profile['capture'])
        try:
            with path.open('ab') as stream:
                stream.write(b'new\n')
            artifacts, ranges = probe.log_delta()
            self.assertEqual(artifacts['log-server.bin'][0], b'new\n')
            self.assertEqual((ranges[0]['start'], ranges[0]['end']), (7, 11))
            path.rename(path.with_name('server.old.log'))
            self.put(path, b'rotated\n')
            with self.assertRaisesRegex(capture.CaptureError, 'log_rotated'):
                probe.log_delta()
        finally:
            probe.close()

    def test_launchctl_environment_not_archived(self):
        manager = self.profile['capture']['manager']
        text = (manager['target'] + ' = {\n\tpath = ' + manager['definition'] +
                '\n\tstate = running\n\tpid = 99\n\truns = 1\n\tenvironment = {\n'
                '\t\tSYNTHETIC_SECRET => never-archive-this\n\t}\n}\n')
        with patch.object(capture.sys, 'platform', 'darwin'), patch.object(capture, 'command', return_value=text.encode()):
            facts, raw, kind = capture.read_manager(manager)
        self.assertEqual(facts['pid'], 99)
        self.assertEqual(kind, 'filtered_launchctl_identity')
        self.assertNotIn(b'never-archive-this', raw)

    def test_health_does_not_follow_redirect(self):
        calls = []
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                calls.append(self.path)
                self.send_response(302)
                self.send_header('Location', '/unexpected')
                self.end_headers()
                self.wfile.write(b'{}')
            def log_message(self, *_args):
                pass
        server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            facts, _ = capture.health('http://127.0.0.1:' + str(server.server_port) + '/readyz')
            self.assertEqual(facts['http_status'], 302)
            self.assertEqual(calls, ['/readyz'])
        finally:
            server.shutdown()
            server.server_close()
            thread.join()


if __name__ == '__main__':
    unittest.main()
