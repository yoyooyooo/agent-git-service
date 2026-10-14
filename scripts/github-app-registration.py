#!/usr/bin/env python3
"""Create an owner-private registration form; never print bearer, state or manifest code."""
import argparse
import html
import json
import os
from pathlib import Path
import stat
import sys
from urllib.parse import urlparse
import urllib.request


def create_form(api_url, token_file, name, output):
    parsed = urlparse(api_url)
    if parsed.scheme != 'https' or not parsed.hostname or not parsed.hostname.endswith('.ts.net') or parsed.username or parsed.password or parsed.query or parsed.fragment or parsed.path not in ('', '/'):
        raise ValueError('private HTTPS service URL required')
    info = token_file.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o600:
        raise ValueError('owner-private token file required')
    token = token_file.read_text().strip()
    if not token or '\r' in token or '\n' in token:
        raise ValueError('invalid credential')
    request = urllib.request.Request(api_url.rstrip('/') + '/api/ext/v1/integrations/github/app-manifest/start',
                                     data=json.dumps({'name': name}).encode(), method='POST',
                                     headers={'Authorization': 'Bearer ' + token, 'Content-Type': 'application/json'})
    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, *_args): return None
    with urllib.request.build_opener(NoRedirect()).open(request, timeout=30) as response:
        if response.status != 201: raise ValueError('registration not started')
        body = json.load(response)
    target = urlparse(body['registration_url'])
    if target.scheme != 'https' or target.netloc != 'github.com' or target.path != '/settings/apps/new' or target.fragment:
        raise ValueError('unexpected GitHub registration target')
    document = ('<!doctype html><meta charset="utf-8"><meta name="referrer" content="no-referrer">'
                '<title>Create GitHub App</title><h1>Create GitHub App</h1>'
                '<p>Continue to GitHub, review the manifest and click Create GitHub App. '
                'This form expires after one hour. Webhook delivery remains inactive.</p>'
                '<form method="post" action="' + html.escape(body['registration_url'], quote=True) + '">'
                '<input type="hidden" name="manifest" value="' + html.escape(json.dumps(body['manifest']), quote=True) + '">'
                '<button type="submit">Review and create on GitHub</button></form>')
    fd = os.open(output, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, 'w') as stream: stream.write(document)
    print(json.dumps({'status': 'form_saved', 'form_path': str(output)}))


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--api-url', required=True)
    parser.add_argument('--token-file', type=Path, required=True)
    parser.add_argument('--name', required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    try:
        create_form(args.api_url, args.token_file, args.name, args.output)
    except Exception:
        print('registration_form_failed: inspect private registration state before retrying', file=sys.stderr)
        sys.exit(1)
