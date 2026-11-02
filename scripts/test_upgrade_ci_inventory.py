#!/usr/bin/env python3
"""Release and host CI inventories must accept a real jobs response shape.

Native adapter isolation is not release-required evidence. A recorded
Fork CI response that includes that job as skipped must still satisfy
the exact required inventory, and a missing required job must not.
"""
import importlib.util
import json
import sys
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / 'scripts'))
sys.path.insert(0, str(ROOT / 'fork' / 'scripts'))

import upgrade_host  # noqa: E402


def _load_release():
    spec = importlib.util.spec_from_file_location(
        'ags_release_inventory', ROOT / 'fork' / 'scripts' / 'release.py')
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


RECORDED = {
    'schema': 'ags.ci-jobs-response.v1',
    'note': 'Shape recorded from Fork CI run 41406636976 attempt 2. Names and conclusions only.',
    'path': '.github/workflows/ci.yml',
    'run_attempt': 2,
    'jobs': [
        {'name': 'Edge and authority race checks', 'status': 'completed', 'conclusion': 'success', 'run_attempt': 2},
        {'name': 'Native adapter isolation', 'status': 'completed', 'conclusion': 'skipped', 'run_attempt': 2},
        {'name': 'Root tests (rest)', 'status': 'completed', 'conclusion': 'success', 'run_attempt': 2},
        {'name': 'Root tests (misc)', 'status': 'completed', 'conclusion': 'success', 'run_attempt': 2},
        {'name': 'Root tests (service)', 'status': 'completed', 'conclusion': 'success', 'run_attempt': 2},
        {'name': 'Root tests (edge)', 'status': 'completed', 'conclusion': 'success', 'run_attempt': 2},
        {'name': 'Root tests (server-graphql)', 'status': 'completed', 'conclusion': 'success', 'run_attempt': 2},
        {'name': 'Source and governance', 'status': 'completed', 'conclusion': 'success', 'run_attempt': 2},
        {'name': 'Vendored client compatibility', 'status': 'completed', 'conclusion': 'success', 'run_attempt': 2},
        {'name': 'Root tests (db-router-git)', 'status': 'completed', 'conclusion': 'success', 'run_attempt': 2},
        {'name': 'Official gh and CI backend acceptance', 'status': 'completed', 'conclusion': 'success', 'run_attempt': 2},
        {'name': 'Root tests (fork-integrations)', 'status': 'completed', 'conclusion': 'success', 'run_attempt': 2},
        {'name': 'Real Forgejo protected-branch projection', 'status': 'completed', 'conclusion': 'success', 'run_attempt': 2},
    ],
}


class CIInventoryTests(unittest.TestCase):
    def test_recorded_jobs_response_matches_release_and_host_inventories(self):
        release = _load_release()
        expected = release.CI_JOBS[RECORDED['path']]
        self.assertEqual(upgrade_host.CI_JOBS[RECORDED['path']], expected)
        self.assertNotIn('Native adapter isolation', expected)
        self.assertFalse(release.CI_JOBS.get('.github/workflows/native-adapter.yml'))
        release.validate_jobs(RECORDED['jobs'], expected, RECORDED['run_attempt'])
        self.assertEqual(
            upgrade_host.required_jobs(RECORDED['jobs'], expected, RECORDED['run_attempt']),
            sorted(expected))
        missing = [row for row in RECORDED['jobs'] if row['name'] != 'Source and governance']
        with self.assertRaises(ValueError):
            release.validate_jobs(missing, expected, RECORDED['run_attempt'])
        with self.assertRaises(upgrade_host.Failure):
            upgrade_host.required_jobs(missing, expected, RECORDED['run_attempt'])
        skipped_required = [dict(row) for row in RECORDED['jobs']
                            if row['name'] != 'Native adapter isolation']
        skipped_required[0] = {**skipped_required[0], 'conclusion': 'skipped'}
        with self.assertRaises(ValueError):
            release.validate_jobs(skipped_required, expected, RECORDED['run_attempt'])

    def test_native_adapter_workflow_is_separate_and_not_release_evidence(self):
        ci = (ROOT / '.github/workflows/ci.yml').read_text()
        native = (ROOT / '.github/workflows/native-adapter.yml').read_text()
        self.assertNotIn('native-isolated:', ci)
        self.assertNotIn('native_probe:', ci)
        self.assertIn('name: Native adapter isolation', native)
        self.assertIn("github.event_name == 'workflow_dispatch'", native)
        self.assertIn('inputs.native_probe', native)
        self.assertIn('UNVERIFIED', native)
        docs = (ROOT / 'docs/ci.md').read_text()
        self.assertIn('not release-required evidence', docs)
        self.assertIn('native-adapter.yml', docs)


if __name__ == '__main__':
    json.dumps(RECORDED)
    unittest.main()
