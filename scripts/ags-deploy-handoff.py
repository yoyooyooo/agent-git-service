#!/usr/bin/env python3
"""Opt-in handoff only; no build, deployment, service control or credential output."""
import json
import os
import re
import subprocess
import sys
from urllib.parse import urlparse
import urllib.request

REPO = 'yoyooyooo/agent-git-service'
WORKFLOW_ID = 365692966


def payload(event):
    run = event['workflow_run']
    sha = run['head_sha']
    if (event['repository']['full_name'] != REPO or run['repository']['full_name'] != REPO
            or run['head_branch'] != 'main' or run['event'] != 'push'
            or run['status'] != 'completed' or run['conclusion'] != 'success'
            or run['workflow_id'] != WORKFLOW_ID or not re.fullmatch('[0-9a-f]{40}', sha)
            or type(run['id']) is not int or run['id'] <= 0
            or type(run['run_attempt']) is not int or run['run_attempt'] <= 0):
        raise ValueError('invalid_ci_event')
    return {'dagRunId': f"ags-{run['id']}-{run['run_attempt']}-{sha[:12]}", 'payload': {
        'repository': REPO, 'branch': 'main', 'sha': sha,
        'run_id': run['id'], 'run_attempt': run['run_attempt']}}


def main():
    with open(os.environ['GITHUB_EVENT_PATH']) as stream:
        body = payload(json.load(stream))
    tip = subprocess.run(['gh', 'api', '--hostname', 'github.com', f'repos/{REPO}/branches/main', '--jq', '.commit.sha'],
                         capture_output=True, text=True, timeout=30)
    if tip.returncode != 0 or tip.stdout.strip() != body['payload']['sha']:
        raise ValueError('stale_or_unverified_main')
    url = os.environ['AGS_DEPLOY_WEBHOOK_URL']
    parsed = urlparse(url)
    if (parsed.scheme != 'https' or parsed.username or parsed.password or parsed.query or parsed.fragment
            or not parsed.hostname or not parsed.hostname.endswith('.ts.net')
            or parsed.path != '/api/v1/webhooks/ags-deploy'):
        raise ValueError('invalid_private_webhook_url')
    token = os.environ['AGS_DEPLOY_WEBHOOK_TOKEN']
    if not token or '\n' in token or '\r' in token:
        raise ValueError('invalid_token')
    request = urllib.request.Request(url, data=json.dumps(body).encode(), method='POST', headers={
        'Content-Type': 'application/json', 'Authorization': 'Bearer ' + token})
    # Do not follow redirects with a bearer. No automatic retry of an unknown outcome.
    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, *_args): return None
    opener = urllib.request.build_opener(NoRedirect())
    with opener.open(request, timeout=30) as response:
        receipt = json.load(response)
        if response.status != 200 or receipt.get('dagRunId') != body['dagRunId'] or receipt.get('dagName') != 'ags-deploy':
            raise ValueError('handoff_receipt_mismatch')
    print(json.dumps({'status': 'enqueued', 'dagRunId': body['dagRunId'], 'sha': body['payload']['sha']}))


if __name__ == '__main__':
    try:
        main()
    except Exception:
        print('ags_deploy_handoff_failed: reconcile the deterministic run ID before retrying', file=sys.stderr)
        sys.exit(1)
