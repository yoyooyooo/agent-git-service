#!/usr/bin/env python3
"""Emitter must refuse caller evidence that is not a push workflow run with digest and cosign fields."""
import unittest

import upgrade_pin
from test_release_consumer import VERSION
from test_upgrade_pin import PinTests


def evidence(**overrides):
    row = {
        'source': 'workflow_run',
        'event': 'push',
        'verified': True,
        'signer_workflow': 'release-control',
        'runner_environment': 'github-hosted',
        'build_trigger': 'push',
        'source_ref': 'refs/tags/' + VERSION,
        'digest': 'sha256:' + 'ab' * 32,
        'cosign_bundle': 'application/vnd.dev.sigstore.bundle.v0.3+json',
    }
    row.update(overrides)
    return row


class EmitterEvidenceTests(unittest.TestCase):
    def setUp(self):
        PinTests.setUp(self)
        self.addCleanup(self.doCleanups)
        self.pin = PinTests.candidate(self)
        self.good = {'darwin_arm64': evidence(), 'linux_amd64': evidence()}

    def emit(self, attestations):
        return upgrade_pin.emit_verified(
            self.pin, self.raw, self.release, self.files, attestations=attestations)

    def test_complete_push_workflow_evidence_stays_not_installable(self):
        result = self.emit(self.good)
        self.assertFalse(result['installable'])
        self.assertEqual(result['verification']['install'], 'not_performed')
        self.assertEqual(result['schema'], 'ags.deploy-release-pin.v2')

    def test_missing_digest_or_cosign_material_is_refused(self):
        for bad in (
            evidence(digest=None),
            evidence(cosign_bundle=None),
            {key: value for key, value in evidence().items() if key != 'digest'},
            {key: value for key, value in evidence().items() if key != 'cosign_bundle'},
            evidence(digest='not-a-digest'),
            evidence(cosign_bundle=''),
        ):
            with self.subTest(bad=sorted(bad)), self.assertRaisesRegex(ValueError, 'release_attestation_unverified'):
                self.emit({'darwin_arm64': bad, 'linux_amd64': evidence()})

    def test_source_must_be_a_push_triggered_workflow_run(self):
        for bad in (
            evidence(source='caller'),
            evidence(source='workflow_dispatch'),
            evidence(event='workflow_dispatch'),
            evidence(event='pull_request'),
            evidence(build_trigger='workflow_dispatch'),
            {key: value for key, value in evidence().items() if key != 'source'},
            {key: value for key, value in evidence().items() if key != 'event'},
        ):
            with self.subTest(bad=sorted(bad)), self.assertRaisesRegex(ValueError, 'release_attestation_unverified'):
                self.emit({'darwin_arm64': evidence(), 'linux_amd64': bad})


if __name__ == '__main__':
    unittest.main()
