#!/usr/bin/env python3
import copy
import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location('handoff', Path(__file__).with_name('ags-deploy-handoff.py'))
handoff = importlib.util.module_from_spec(spec)
spec.loader.exec_module(handoff)
EVENT = {'repository': {'full_name': handoff.REPO}, 'workflow_run': {
    'repository': {'full_name': handoff.REPO}, 'id': 42, 'run_attempt': 1,
    'workflow_id': handoff.WORKFLOW_ID, 'head_sha': 'a' * 40, 'head_branch': 'main',
    'event': 'push', 'status': 'completed', 'conclusion': 'success'}}


class HandoffTests(unittest.TestCase):
    def test_exact_success_payload_and_idempotency(self):
        value = handoff.payload(EVENT)
        self.assertEqual(value['dagRunId'], 'ags-42-1-aaaaaaaaaaaa')
        self.assertEqual(value['payload']['sha'], 'a' * 40)
        self.assertEqual(handoff.payload(EVENT), value)

    def test_rejects_untrusted_and_non_success_events(self):
        for patch in [{'conclusion': 'failure'}, {'status': 'in_progress'}, {'event': 'pull_request'},
                      {'head_branch': 'fork/main.20260924'}, {'head_sha': '$(unsafe)'},
                      {'workflow_id': 1}, {'repository': {'full_name': 'attacker/fork'}},
                      {'id': True}, {'run_attempt': 0}]:
            event = copy.deepcopy(EVENT)
            event['workflow_run'].update(patch)
            with self.assertRaises(ValueError): handoff.payload(event)


if __name__ == '__main__': unittest.main()
