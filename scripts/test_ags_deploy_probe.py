#!/usr/bin/env python3
import importlib.util
import io
from email.message import Message
from pathlib import Path
import re
import unittest
import urllib.error
import urllib.response
from unittest import mock

spec = importlib.util.spec_from_file_location('probe', Path(__file__).with_name('ags-deploy-probe.py'))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
URL = 'https://example.ts.net/api/v1/webhooks/ags-deploy'
ROOT = Path(__file__).resolve().parents[1]


def top_level_block(text, name):
    """Read a top-level workflow block for source-contract assertions."""
    match = re.search(r'^' + re.escape(name) + r':[^\n]*\n(?:(?:[ \t]+[^\n]*|)\n)*', text, re.M)
    if match is None:
        raise AssertionError('missing workflow block: ' + name)
    return match.group(0).strip()


class Opener:
    def __init__(self, code): self.code = code; self.requests = []
    def open(self, request, timeout):
        self.requests.append(request)
        raise urllib.error.HTTPError(request.full_url, self.code, 'fixture', {}, io.BytesIO(b''))


class ProbeTests(unittest.TestCase):
    def test_get_without_auth_is_the_only_effect(self):
        client = Opener(405)
        proof = module.probe(URL, client)
        self.assertEqual(proof['http_status'], 405)
        self.assertEqual(len(client.requests), 1)
        self.assertEqual(client.requests[0].method, 'GET')
        self.assertIsNone(client.requests[0].data)
        self.assertFalse(client.requests[0].has_header('Authorization'))

    def test_redirects_wrong_routes_and_non_tls_do_not_qualify(self):
        for code in (200, 302, 401, 404, 500):
            with self.assertRaises(ValueError): module.probe(URL, Opener(code))
        for url in ('http://example.ts.net/api/v1/webhooks/ags-deploy', URL+'?secret=x',
                    URL.replace('example.ts.net', 'public.example.test'), URL.replace('ags-deploy', 'other')):
            client = Opener(405)
            with self.assertRaises(ValueError): module.probe(url, client)
            self.assertEqual(client.requests, [])

    def test_default_opener_rejects_redirect_without_second_request(self):
        headers = Message()
        headers['Location'] = 'https://redirect.example.test/should-not-be-contacted'
        response = urllib.response.addinfourl(io.BytesIO(b''), headers, URL, 302)
        response.msg = 'Found'
        # Exercise urllib's real redirect/error handlers, replacing only the
        # network transport so this safety test needs no external endpoint.
        with mock.patch.object(urllib.request.HTTPSHandler, 'https_open', return_value=response) as transport:
            with self.assertRaisesRegex(ValueError, 'unexpected_probe_http_status'):
                module.probe(URL)
        self.assertEqual(transport.call_count, 1)

    def test_transport_failure_is_not_reachability_success(self):
        for failure in (TimeoutError('fixture timeout'), urllib.error.URLError('fixture TLS failure')):
            client = mock.Mock()
            client.open.side_effect = failure
            with self.assertRaises(type(failure)):
                module.probe(URL, client)


class WorkflowContractTests(unittest.TestCase):
    def setUp(self):
        self.ci = (ROOT / '.github/workflows/ci.yml').read_text()
        path = ROOT / '.github/workflows/ags-network-probe.yml'
        self.assertTrue(path.is_file(), 'probe must be a separate workflow')
        self.workflow = path.read_text()
        self.handoff = (ROOT / '.github/workflows/ags-deploy.yml').read_text()

    def test_only_manual_probe_has_its_own_concurrency_namespace(self):
        self.assertEqual(top_level_block(self.workflow, 'on'), 'on:\n  workflow_dispatch:')
        self.assertRegex(self.workflow, r'^name: AGS network probe\n')
        probe_concurrency = top_level_block(self.workflow, 'concurrency')
        self.assertEqual(probe_concurrency, 'concurrency:\n  group: ags-network-probe-${{ github.ref }}\n  cancel-in-progress: true')
        ci_concurrency = top_level_block(self.ci, 'concurrency')
        self.assertNotIn('ags-network-probe-', ci_concurrency)
        self.assertIn('fork-ci-', ci_concurrency)
        self.assertNotIn('ags_network_probe', self.ci)
        self.assertNotIn('deployment-network-probe:', self.ci)
        self.assertIn('python3 scripts/test_ags_deploy_probe.py', self.ci)

    def test_probe_admission_is_default_branch_only_before_environment_access(self):
        gate = re.search(r'^    if: >-\n((?:      [^\n]*\n)+)', self.workflow, re.M)
        self.assertIsNotNone(gate)
        self.assertEqual(' '.join(gate.group(1).split()),
                         "github.event_name == 'workflow_dispatch' && "
                         "github.ref_type == 'branch' && "
                         "github.ref_name == github.event.repository.default_branch && "
                         "github.repository == 'yoyooyooo/agent-git-service'")
        self.assertEqual(re.findall(r'^    environment: (.+)$', self.workflow, re.M), ['ags-autodeploy-probe'])
        self.assertEqual(top_level_block(self.workflow, 'permissions'), 'permissions:\n  contents: read')
        self.assertIn('ref: ${{ github.sha }}', self.workflow)
        self.assertIn('persist-credentials: false', self.workflow)
        self.assertNotIn('workflow_call:', self.workflow)

    def test_probe_has_no_deployment_credential_or_deploy_step(self):
        self.assertEqual(re.findall(r'secrets\.([A-Z_]+)', self.workflow), ['AGS_DEPLOY_TS_AUTHKEY'])
        self.assertEqual(re.findall(r'^        run: (.+)$', self.workflow, re.M), ['python3 scripts/ags-deploy-probe.py'])
        self.assertNotIn('AGS_DEPLOY_WEBHOOK_TOKEN', self.workflow)
        self.assertNotIn('GH_TOKEN', self.workflow)
        self.assertIn('AGS_DEPLOY_WEBHOOK_URL: ${{ vars.AGS_DEPLOY_WEBHOOK_URL }}', self.workflow)

    def test_production_handoff_does_not_consume_probe_success(self):
        triggers = top_level_block(self.handoff, 'on')
        self.assertIn("workflows: ['Fork CI']", triggers)
        self.assertNotIn('AGS network probe', triggers)
        self.assertIn("github.event.workflow_run.event == 'push'", self.handoff)
        self.assertIn('github.event.workflow_run.head_branch == github.event.repository.default_branch', self.handoff)

    def test_operator_contract_requires_exact_default_branch_environment_rule(self):
        doc = (ROOT / 'docs/operations/ags-network-probe.md').read_text()
        self.assertIn('custom_branch_policies: true', doc)
        self.assertIn('protected_branches: false', doc)
        self.assertIn('type: branch', doc)
        self.assertIn('exact default branch', doc)
        self.assertIn('gh workflow run ags-network-probe.yml --ref "$default_branch"', doc)


if __name__ == '__main__': unittest.main()
