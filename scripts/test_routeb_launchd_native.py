#!/usr/bin/env python3
"""Route B native launchd harness. Skipped unless AGS_ROUTEB_NATIVE=1.

The skipped result is not a pass. A run uses one disposable label and a
private temporary root, then bootouts that label.
"""
import os
import unittest


NATIVE = os.environ.get('AGS_ROUTEB_NATIVE') == '1'


class WriterProbeTests(unittest.TestCase):
    def test_probe_reports_closed_writers_for_one_generation(self):
        import routeb_writer_probe as probe
        evidence = probe.observe(generation='abc', listeners=[], holders=[], descendants=[])
        self.assertEqual(evidence['generation'], 'abc')
        self.assertEqual(evidence['closed'], True)
        self.assertEqual(evidence['writers'], [])
        self.assertEqual(evidence['unknown'], [])

    def test_live_listener_is_not_closed(self):
        import routeb_writer_probe as probe
        evidence = probe.observe(generation='abc', listeners=[('127.0.0.1', 9)], holders=[], descendants=[])
        self.assertEqual(evidence['closed'], False)
        self.assertTrue(evidence['writers'])

    def test_unknown_writer_observations_must_not_close(self):
        """None is not an empty observation. Enumeration failure stays open."""
        import routeb_writer_probe as probe
        evidence = probe.observe(generation='g', listeners=None, holders=None, descendants=None)
        self.assertFalse(evidence['closed'])
        self.assertTrue(evidence['unknown'])
        self.assertEqual(evidence['generation'], 'g')
        failed = probe.observe(generation='g', listeners=[], holders=[], descendants=[],
                               sources={'listeners': 'permission_denied'})
        self.assertFalse(failed['closed'])
        self.assertTrue(failed['unknown'])


@unittest.skipUnless(NATIVE, 'native launchd evidence not requested')
class NativeLaunchdTests(unittest.TestCase):
    def test_disposable_label_bootstraps_and_clears(self):
        import routeb_native_harness as harness
        result = harness.exercise()
        self.assertTrue(result['label'].startswith('com.jackie.ags-hostpkg-test.'))
        self.assertNotEqual(result['label'], 'com.jackie.agent-git-service')
        self.assertEqual(result['cleaned'], True)
        self.assertEqual(result['production_label_touched'], False)


if __name__ == '__main__':
    unittest.main()
