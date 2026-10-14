#!/usr/bin/env python3
import importlib.util
import io
from pathlib import Path
import unittest
import urllib.error

spec = importlib.util.spec_from_file_location('probe', Path(__file__).with_name('ags-deploy-probe.py'))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
URL = 'https://example.ts.net/api/v1/webhooks/ags-deploy'


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


if __name__ == '__main__': unittest.main()
