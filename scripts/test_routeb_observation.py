#!/usr/bin/env python3
"""M5 observation gate. Injected clocks only; no network and no production root.

599 seconds is not 600. A longer lease must be covered. A 200 readyz does not
excuse an unknown provider result. Recovery inventory and the review gate are
the extracted maintenance-window functions, not a second implementation.
"""
import json
import sqlite3
from pathlib import Path
import tempfile
import unittest

import ags_maintenance_window as window


class ObservationTests(unittest.TestCase):
    def evidence(self, **overrides):
        value = {
            'schema': 'ags.routeb-observation.v1',
            'generation': 'gen-1',
            'boot_id': 'boot-1',
            'revision': '5154feaf84e4a3fb910be6c720c10024f35020d0',
            'started_monotonic': 0,
            'now_monotonic': 600,
            'lease_seconds': 0,
            'readyz': {'local': 200, 'service': 200, 'revision': '5154feaf84e4a3fb910be6c720c10024f35020d0',
                       'diagnostic_only': True},
            'business': {'git': 'read', 'pull_request': 'read', 'wiki': 'read'},
            'provider': {'unknown_external_results': 0, 'source': 'observed'},
            'recovery': [],
            'interrupted': False,
        }
        value.update(overrides)
        return value

    def test_599_seconds_is_refused(self):
        import routeb_observation
        with self.assertRaises(routeb_observation.ObservationError) as caught:
            routeb_observation.accept(self.evidence(now_monotonic=599))
        self.assertEqual(caught.exception.code, 'observation_short')

    def test_lease_longer_than_600_must_be_covered(self):
        import routeb_observation
        with self.assertRaises(routeb_observation.ObservationError) as caught:
            routeb_observation.accept(self.evidence(now_monotonic=600, lease_seconds=900))
        self.assertEqual(caught.exception.code, 'lease_uncovered')
        accepted = routeb_observation.accept(self.evidence(now_monotonic=900, lease_seconds=900))
        self.assertEqual(accepted['state'], 'awaiting_acceptance')

    def test_readyz_200_with_unknown_provider_is_refused(self):
        import routeb_observation
        with self.assertRaises(routeb_observation.ObservationError) as caught:
            routeb_observation.accept(self.evidence(provider={'unknown_external_results': 1, 'source': 'observed'}))
        self.assertEqual(caught.exception.code, 'provider_unknown')

    def test_elapsed_is_not_counted_from_prepare(self):
        import routeb_observation
        with self.assertRaises(routeb_observation.ObservationError) as caught:
            routeb_observation.accept(self.evidence(started_monotonic=100, now_monotonic=650,
                                                    prepare_monotonic=0))
        self.assertEqual(caught.exception.code, 'observation_short')

    def test_recovery_inventory_is_the_extracted_gate(self):
        import routeb_observation
        self.assertIs(routeb_observation.recovery_inventory, window.recovery_inventory)
        self.assertIs(routeb_observation.recovery_gate, window.recovery_gate)
        tmp = tempfile.TemporaryDirectory(prefix='routeb-obs-')
        self.addCleanup(tmp.cleanup)
        path = Path(tmp.name) / 'empty.db'
        db = sqlite3.connect(path)
        db.close()
        with self.assertRaises(Exception):
            routeb_observation.recovery_inventory(path)


if __name__ == '__main__':
    unittest.main()
