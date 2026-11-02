#!/usr/bin/env python3
"""Emitter must refuse caller evidence that is not a push workflow run with digest and cosign fields."""
import unittest

import upgrade_pin
from test_release_consumer import VERSION
from test_upgrade_pin import PinTests


def evidence(pin=None, target=None, **overrides):
    digest = 'sha256:' + 'ab' * 32
    if pin is not None and target is not None:
        digest = pin['targets'][target]['asset']['digest']
    row = {
        'source': 'workflow_run',
        'event': 'push',
        'verified': True,
        'signer_workflow': 'release-control',
        'runner_environment': 'github-hosted',
        'build_trigger': 'push',
        'source_ref': 'refs/tags/' + VERSION,
        'digest': digest,
        'cosign_bundle': 'application/vnd.dev.sigstore.bundle.v0.3+json',
        'run_id': 41406636976,
        'run_attempt': 2,
        'signature': b'fixture-signature',
    }
    row.update(overrides)
    return row


class EmitterEvidenceTests(unittest.TestCase):
    def setUp(self):
        PinTests.setUp(self)
        self.addCleanup(self.doCleanups)
        self.pin = PinTests.candidate(self)
        self.good = {
            'darwin_arm64': evidence(self.pin, 'darwin_arm64'),
            'linux_amd64': evidence(self.pin, 'linux_amd64'),
        }

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

    def test_digest_must_match_the_candidate_asset(self):
        forged = {
            'darwin_arm64': evidence(digest='sha256:' + '11' * 32),
            'linux_amd64': evidence(digest='sha256:' + '22' * 32),
        }
        with self.assertRaisesRegex(ValueError, 'release_attestation_unverified'):
            self.emit(forged)

    def test_verifier_rejection_refuses_the_envelope(self):
        def reject(material, signature):
            raise ValueError('bad signature')

        with self.assertRaisesRegex(ValueError, 'release_attestation_unverified'):
            upgrade_pin.emit_verified(
                self.pin, self.raw, self.release, self.files,
                attestations={
                    'darwin_arm64': evidence(signature=b'sig-a'),
                    'linux_amd64': evidence(signature=b'sig-b'),
                },
                verifier=reject)

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
