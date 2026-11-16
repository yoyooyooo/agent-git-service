#!/usr/bin/env python3
"""Offline release preparation: synthetic nine assets never become a deploy pin."""
import copy
import hashlib
import unittest
from unittest.mock import Mock

import release_install_v2 as consumer
from test_release_consumer import ConsumerChainTests, installer, VERSION
from test_upgrade_safety import pin_fixture
from test_upgrade_wireup import snapshot


class PinTests(ConsumerChainTests):
    def test_duplicate_asset_identity_refuses_before_any_io(self):
        ledger = Mock()
        before = snapshot(self.root)
        release = copy.deepcopy(self.release)
        release['assets'][1]['id'] = release['assets'][0]['id']
        with self.assertRaisesRegex(ValueError, 'release_digest_mismatch'):
            consumer.inventory(installer, release, VERSION)
        self.assertEqual(ledger.mock_calls, [])
        self.assertEqual(snapshot(self.root), before)

    def candidate(self):
        pin = pin_fixture()
        pin['release_id'] = self.release['id']
        for target, row in pin['targets'].items():
            row['asset'] = next({k: a[k] for k in ('id', 'name', 'size', 'digest')}
                                for a in self.assets if a['name'] == 'ags-' + VERSION + '-' + target + '.tar.gz')
            row['binaries'] = self.infos[target]['binaries']
            row['provenance']['subject_digest'] = row['asset']['digest']
        return pin

    def test_complete_synthetic_nine_assets_only_make_noninstallable_template(self):
        import upgrade_pin
        before = snapshot(self.root)
        result = upgrade_pin.prepare(self.candidate(), self.raw, self.release, self.files)
        self.assertEqual(result['schema'], 'ags.release-pin-preparation.v1')
        self.assertFalse(result['installable'])
        self.assertEqual(result['source_sha256'], hashlib.sha256(self.raw).hexdigest())
        self.assertNotEqual(result['schema'], 'ags.deploy-release-pin.v2')
        self.assertEqual(snapshot(self.root), before)

    def test_changed_source_dispatch_wrong_runtime_immutable_and_member_refuse(self):
        import upgrade_pin
        before = snapshot(self.root)
        ledger = Mock()
        for mutation in ('source', 'dispatch', 'runtime', 'immutable', 'missing', 'extra', 'release_id'):
            pin, raw, release, files = self.candidate(), self.raw, copy.deepcopy(self.release), dict(self.files)
            if mutation == 'source': raw += b' '
            elif mutation == 'dispatch': pin['targets']['darwin_arm64']['provenance']['signer_ref'] = 'refs/heads/main'
            elif mutation == 'runtime': pin['runtime']['tree'] = '0' * 40
            elif mutation == 'immutable': release['immutable'] = False
            elif mutation == 'missing': release['assets'].pop()
            elif mutation == 'extra': files['extra'] = b'not allowed'
            elif mutation == 'release_id': release['id'] = None
            with self.subTest(mutation=mutation), self.assertRaises(ValueError):
                upgrade_pin.prepare(pin, raw, release, files)
            self.assertEqual(ledger.mock_calls, [])
            self.assertEqual(snapshot(self.root), before)


if __name__ == '__main__':
    unittest.main()
