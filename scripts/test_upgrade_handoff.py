#!/usr/bin/env python3
"""Human-carried refresh; S4 base is real synthetic completion, no network."""
import copy
from datetime import timedelta
import unittest
from unittest.mock import Mock

import upgrade_backup_contract as c
import upgrade_manual_approval as manual
from test_upgrade_manual_approval import ManualFixture, ManualTests
from test_upgrade_wireup import snapshot


class HandoffTests(ManualFixture):
    make_handoff = ManualTests.make_handoff

    def prepare_handoff(self):
        self.base = self.make_handoff()
        self.later = self.now + timedelta(minutes=11)
        self.readback = {'schema': 'ags.manual-mini-readback.v2',
            'base_sha256': c.fingerprint(self.base), 'imile_request_sha256': '2' * 64,
            'transaction_id': 'imile-transaction', 'accepted_epoch': 1,
            'sequence': 1, 'previous_sha256': None, 'actor': {'type': 'human', 'id': 'fixture-human',
            'source': 'private-carried'}, 'observed_at': self.later.isoformat(),
            'received_at': self.later.isoformat(), 'mini_generation': self.base['mini_generation'],
            'health': {'status': 'ready', 'release_key': self.base['release_key']},
            'unresolved': [], 'confirmed': True}
        self.envelope = {'schema': 'ags.manual-mini-handoff.v2', 'base': self.base,
            'base_sha256': c.fingerprint(self.base), 'imile_request_sha256': '2' * 64,
            'transaction_id': 'imile-transaction', 'accepted_epoch': 1, 'readbacks': [self.readback]}
        self.ledger = Mock()
        before = snapshot(self.root)
        self.check_unchanged = True
        self.addCleanup(lambda: self.assertEqual(snapshot(self.root), before) if self.check_unchanged else None)
        self.addCleanup(lambda: self.assertEqual(self.ledger.mock_calls, []))

    def test_fresh_readback_after_eleven_minutes_keeps_base_immutable(self):
        self.prepare_handoff()
        before = c.canonical(self.base)
        with self.assertRaisesRegex(c.ContractError, 'handoff_stale'):
            manual.validate_handoff(self.base, self.base['release_key'], '1' * 64, self.later)
        manual.validate_handoff(self.envelope, self.base['release_key'], '1' * 64, self.later)
        self.assertEqual(c.canonical(self.base), before)

    def test_expiry_future_replay_and_changed_bindings_refuse(self):
        self.prepare_handoff()
        for change in ({'sequence': 0}, {'actor': {'type': 'agent', 'id': 'x', 'source': 'x'}},
                       {'base_sha256': '0' * 64}, {'accepted_epoch': 2},
                       {'mini_generation': 'another'}, {'transaction_id': 'another'},
                       {'observed_at': (self.later + timedelta(seconds=1)).isoformat()}):
            bad = copy.deepcopy(self.envelope)
            bad['readbacks'][0].update(change)
            with self.subTest(change=change), self.assertRaises(c.ContractError):
                manual.validate_handoff(bad, self.base['release_key'], '1' * 64, self.later)
        with self.assertRaisesRegex(c.ContractError, 'handoff_stale'):
            manual.validate_handoff(self.envelope, self.base['release_key'], '1' * 64,
                                    self.later + timedelta(seconds=301))
        bad = copy.deepcopy(self.envelope)
        bad['readbacks'].append(copy.deepcopy(self.readback))
        with self.assertRaises(c.ContractError):
            manual.validate_handoff(bad, self.base['release_key'], '1' * 64, self.later)

    def test_private_refresh_registration_preserves_package_and_history(self):
        from pathlib import Path
        import upgrade_handoff as handoff
        import upgrade_engine as engine
        self.prepare_handoff()
        # Registration is the only allowed effect: synthetic evidence directory.
        # The snapshot cleanup is specific to refusal/read-only cases above.
        self.check_unchanged = False
        binding = {'host': 'imile-win', 'synthetic_request': True}
        ref = {key: self.envelope[key] for key in ('base', 'base_sha256',
            'imile_request_sha256', 'transaction_id', 'accepted_epoch')}
        ref.update(schema='ags.handoff-reference.v2', refresh_directory=str(self.root / 'refresh'))
        ref['imile_request_sha256'] = c.fingerprint(binding)
        Path(ref['refresh_directory']).mkdir(mode=0o700)
        package = c.canonical(ref)
        self.readback['imile_request_sha256'] = c.fingerprint(binding)
        path = self.root / 'carried-readback.json'
        engine.save(path, self.readback)
        result = handoff.refresh(self.root, ref, path, self.base['release_key'], '1' * 64,
                                 binding, ref['transaction_id'], self.later)
        self.assertEqual(c.canonical(ref), package)
        self.assertEqual(handoff.load(ref, self.base['release_key'], '1' * 64, binding,
                         ref['transaction_id'], self.later), result)
        before = snapshot(self.root)
        with self.assertRaises(c.ContractError):
            handoff.refresh(self.root, ref, path, self.base['release_key'], '1' * 64,
                            binding, ref['transaction_id'], self.later)
        self.assertEqual(snapshot(self.root), before)
        later = self.later + timedelta(minutes=12)
        second = copy.deepcopy(self.readback)
        second.update(sequence=2, previous_sha256=c.fingerprint(self.readback),
                      observed_at=later.isoformat(), received_at=later.isoformat())
        engine.save(path, second)
        previous_bytes = (Path(ref['refresh_directory']) / 'readback-0001.json').read_bytes()
        handoff.refresh(self.root, ref, path, self.base['release_key'], '1' * 64,
                        binding, ref['transaction_id'], later)
        self.assertEqual((Path(ref['refresh_directory']) / 'readback-0001.json').read_bytes(), previous_bytes)
        self.assertEqual(c.canonical(ref), package)
        self.assertEqual(handoff.load(ref, self.base['release_key'], '1' * 64, binding,
                         ref['transaction_id'], later)['readbacks'], [self.readback, second])
        # Rolling latest back while the immutable second row remains is rejected.
        engine.save(Path(ref['refresh_directory']) / 'latest.json',
                    {'sequence': 1, 'sha256': c.fingerprint(self.readback)})
        before = snapshot(self.root)
        with self.assertRaisesRegex(c.ContractError, 'handoff_refresh_incomplete'):
            handoff.load(ref, self.base['release_key'], '1' * 64, binding, ref['transaction_id'], later)
        self.assertEqual(snapshot(self.root), before)


if __name__ == '__main__':
    unittest.main()
