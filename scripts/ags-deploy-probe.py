#!/usr/bin/env python3
"""Prove TLS/network reachability using GET only, without a deployment token."""
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request


def probe(url, opener=None):
    parsed = urllib.parse.urlparse(url)
    if (parsed.scheme != 'https' or not parsed.hostname or not parsed.hostname.endswith('.ts.net')
            or parsed.username or parsed.password or parsed.query or parsed.fragment
            or parsed.path != '/api/v1/webhooks/ags-deploy'):
        raise ValueError('invalid_probe_target')
    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, *_args): return None
    client = opener or urllib.request.build_opener(NoRedirect())
    request = urllib.request.Request(url, method='GET')
    try:
        with client.open(request, timeout=20) as response:
            code = response.status
    except urllib.error.HTTPError as error:
        code = error.code
        error.close()
    # The native webhook accepts POST. GET cannot enqueue or deploy.
    if code != 405:
        raise ValueError('unexpected_probe_http_status')
    return {'tls_verified': True, 'http_status': code, 'method': 'GET',
            'deployment_token_used': False, 'deploy_requested': False}


if __name__ == '__main__':
    try:
        print(json.dumps(probe(os.environ.get('AGS_DEPLOY_WEBHOOK_URL', ''))))
    except Exception:
        print('deployment_network_probe_failed', file=sys.stderr)
        sys.exit(1)
