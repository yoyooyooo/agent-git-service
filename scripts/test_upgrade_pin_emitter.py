#!/usr/bin/env python3
import copy
import tempfile
from pathlib import Path
import unittest
from unittest.mock import patch

import upgrade_pin
from test_upgrade_pin import PinTests
from test_upgrade_wireup import snapshot

class EmitterTests(unittest.TestCase):
    def setUp(self):
        PinTests.setUp(self)
        self.addCleanup(self.doCleanups)
    def candidate(self):
        return PinTests.candidate(self)
    def test_verified_emitter_requires_two_real_attestations_and_never_dispatch(self):
        pin=self.candidate(); before=snapshot(self.root)
        with self.assertRaises(ValueError) as caught:
            upgrade_pin.emit_verified(pin, self.raw, self.release, self.files,
                                     attestations={'darwin_arm64': {'verified': True, 'signer_workflow':'dispatch'},
                                                   'linux_amd64': {'verified': True, 'signer_workflow':'release'}},
                                     verifier=lambda *args, **kwargs: True)
        self.assertIn('release_attestation_unverified', str(caught.exception))
        self.assertEqual(snapshot(self.root), before)
    def test_verified_emitter_requires_all_targets(self):
        with self.assertRaisesRegex(ValueError,'release_attestation_unverified'):
            upgrade_pin.emit_verified(self.candidate(), self.raw, self.release, self.files,
                                      attestations={'darwin_arm64': {'verified': True}},
                                      verifier=lambda *args, **kwargs: True)

if __name__=='__main__': unittest.main()
