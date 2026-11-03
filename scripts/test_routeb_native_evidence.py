#!/usr/bin/env python3
"""M6 native evidence schema. Missing or skipped evidence is not a pass.

Ordinary CI does not run the launchd experiment. A fixture result must not be
labeled native. The 600 second field is a minimum, not a default success.
"""
import json
import unittest

import routeb_native_evidence as evidence


class NativeEvidenceTests(unittest.TestCase):
    def sample(self, **overrides):
        value = {
            'schema': 'ags.routeb-native-evidence.v1',
            'kind': 'native',
            'head': 'b0b6602f24d62eaa6597622b87ea06178c4d0607',
            'label_prefix': 'com.jackie.ags-hostpkg-test.',
            'production_label_touched': False,
            'cleaned': True,
            'observation_seconds': 600,
            'metadata_roundtrip': True,
            'restore_once': True,
            'skipped': False,
        }
        value.update(overrides)
        return value

    def test_missing_evidence_is_not_a_pass(self):
        with self.assertRaises(evidence.EvidenceError) as caught:
            evidence.validate(None)
        self.assertEqual(caught.exception.code, 'native_evidence_missing')

    def test_skipped_is_not_a_pass(self):
        with self.assertRaises(evidence.EvidenceError) as caught:
            evidence.validate(self.sample(skipped=True))
        self.assertEqual(caught.exception.code, 'native_skipped')

    def test_fixture_cannot_be_labeled_native(self):
        with self.assertRaises(evidence.EvidenceError) as caught:
            evidence.validate(self.sample(kind='fixture'))
        self.assertEqual(caught.exception.code, 'not_native')

    def test_599_seconds_is_not_enough(self):
        with self.assertRaises(evidence.EvidenceError) as caught:
            evidence.validate(self.sample(observation_seconds=599))
        self.assertEqual(caught.exception.code, 'observation_short')
        accepted = evidence.validate(self.sample())
        self.assertEqual(accepted['passed'], True)
        self.assertNotEqual(accepted.get('installable'), True)


if __name__ == '__main__':
    unittest.main()
