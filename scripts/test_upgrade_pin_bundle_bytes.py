#!/usr/bin/env python3
"""B7: bundle bytes must emit a deterministic envelope, and only True verifies.

No network, no release, no install. installable stays false.
"""
import json
import unittest

import upgrade_pin
from test_upgrade_pin_evidence import EmitterEvidenceTests


def with_bundle(good):
    raw = {}
    for target, row in good.items():
        copied = dict(row)
        copied['bundle_bytes'] = b'bundle ' + row['digest'].encode() + b' fixture-signature'
        raw[target] = copied
    return raw


class BundleBytesTests(EmitterEvidenceTests):
    def test_both_platform_bundle_bytes_emit_a_non_installable_envelope(self):
        raw = with_bundle(self.good)
        result = self.emit(raw, verifier=upgrade_pin.verify_bundle)
        self.assertEqual(result['schema'], 'ags.deploy-release-pin.v2')
        self.assertIs(result['installable'], False)
        self.assertEqual(result['verification']['both_platforms'], True)
        encoded = json.dumps(result, sort_keys=True)
        self.assertNotIn('bundle_bytes', encoded)
        for target, row in raw.items():
            digest = row['digest']
            self.assertIn(digest, encoded)
            proof = result['proof_sha256']
            self.assertEqual(len(proof), 64)
        again = self.emit(with_bundle(self.good), verifier=upgrade_pin.verify_bundle)
        self.assertEqual(again['proof_sha256'], result['proof_sha256'])

    def test_false_verifier_does_not_emit(self):
        def reject(digest_value, signature, bundle=None):
            return False

        with self.assertRaisesRegex(ValueError, 'release_attestation_unverified'):
            self.emit(self.good, verifier=reject)

    def test_non_boolean_verifier_does_not_emit(self):
        def odd(digest_value, signature, bundle=None):
            return 'accepted'

        with self.assertRaisesRegex(ValueError, 'release_attestation_unverified'):
            self.emit(self.good, verifier=odd)

    def test_internal_bundle_typeerror_refuses_without_retry(self):
        """A TypeError inside a bundle-capable verifier is refusal, not a retry."""
        calls = []
        raw = with_bundle(self.good)

        def verifier(digest_value, signature, *, bundle=None):
            calls.append(bundle is not None)
            if bundle is not None:
                raise TypeError('malformed bundle decoder')
            return True

        with self.assertRaisesRegex(ValueError, 'release_attestation_unverified'):
            self.emit(raw, verifier=verifier)
        self.assertEqual(calls, [True])


if __name__ == '__main__':
    unittest.main()
