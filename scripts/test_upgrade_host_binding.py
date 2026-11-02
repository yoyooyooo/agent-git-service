#!/usr/bin/env python3
"""B4: format host digest and first-event host must match or load is refused."""
import json
import os
from pathlib import Path
import tempfile
import unittest

import upgrade_state


def event(host='host-1'):
    return {
        'schema': 'ags.upgrade-event.v1', 'event_id': 'ev-note', 'operation_id': 'op-1',
        'kind': 'note', 'host_instance': host, 'identity_sha256': 'a' * 64,
        'expected_epoch': None, 'epoch': 1, 'transaction_id': None, 'binding_sha256': 'b' * 64,
        'before_phase': 'none', 'after_phase': 'note', 'approval_sha256': None,
        'evidence_sha256': None, 'effect': None, 'recorded_at': '2026-11-02T00:00:00+00:00'}


class HostBindingTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(dir=Path.home(), prefix='s6b-b4-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        (self.root / 'state').mkdir(mode=0o700)

    def test_format_digest_must_match_header_host(self):
        upgrade_state.commit_event(self.root, event())
        control = self.root / 'state/ags-autodeploy/local-v2/control'
        header = json.loads((control / 'format.json').read_bytes())
        header['host_instance_sha256'] = 'f' * 64
        raw = (json.dumps(header, sort_keys=True, separators=(',', ':')) + '\n').encode()
        path = control / 'format.json'
        os.chmod(path, 0o600)
        path.write_bytes(raw)
        os.chmod(path, 0o600)
        with self.assertRaises(upgrade_state.StateError) as caught:
            upgrade_state.load_verified(self.root)
        self.assertEqual(caught.exception.args[0], 'state_host_mismatch')

    def test_later_event_host_drift_is_refused(self):
        first = upgrade_state.commit_event(self.root, event('host-1'))
        drifted = event('host-2')
        drifted['event_id'] = 'ev-second'
        drifted['operation_id'] = 'op-2'
        drifted['sequence'] = 2
        drifted['previous_sha256'] = first['sha256']
        events = self.root / 'state/ags-autodeploy/local-v2/control/events'
        raw = (json.dumps(drifted, sort_keys=True, separators=(',', ':')) + '\n').encode()
        path = events / '00000000000000000002.json'
        path.write_bytes(raw)
        os.chmod(path, 0o600)
        real = upgrade_state._check_event
        def skip_writer_check(value, previous):
            saved = value['host_instance']
            value['host_instance'] = previous['host_instance'] if previous else saved
            try:
                return real(value, previous)
            finally:
                value['host_instance'] = saved
        upgrade_state._check_event = skip_writer_check
        self.addCleanup(lambda: setattr(upgrade_state, '_check_event', real))
        with self.assertRaises(upgrade_state.StateError) as caught:
            upgrade_state.load_verified(self.root)
        self.assertEqual(caught.exception.args[0], 'state_host_mismatch')

    def test_header_host_must_match_first_event_host(self):
        upgrade_state.commit_event(self.root, event('host-1'))
        control = self.root / 'state/ags-autodeploy/local-v2/control'
        header = json.loads((control / 'format.json').read_bytes())
        header['host_instance'] = 'host-2'
        header['host_instance_sha256'] = upgrade_state._sha(b'host-2')
        path = control / 'format.json'
        path.write_bytes((json.dumps(header, sort_keys=True, separators=(',', ':')) + '\n').encode())
        os.chmod(path, 0o600)
        with self.assertRaises(upgrade_state.StateError) as caught:
            upgrade_state.load_verified(self.root)
        self.assertEqual(caught.exception.args[0], 'state_host_mismatch')


if __name__ == '__main__':
    unittest.main()
