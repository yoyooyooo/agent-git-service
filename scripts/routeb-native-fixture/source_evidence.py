#!/usr/bin/env python3
"""Retain actual accepted-source metadata using the manual job's read token."""
import argparse
import os
from pathlib import Path
import subprocess
import sys

from common import OID, Refused, command, exclusive_json, object_json, require

REPOSITORY = 'yoyooyooo/agent-git-service'


def validate(repo, commit, ci, workflow, revision):
    require(repo.get('full_name') == REPOSITORY and OID.fullmatch(revision), 'source_repository')
    require(commit.get('sha') == revision and OID.fullmatch(commit['commit']['tree']['sha']), 'source_commit')
    require(ci.get('head_sha') == revision and ci.get('head_branch') == repo['default_branch']
            and ci.get('event') == 'push' and ci.get('status') == 'completed'
            and ci.get('conclusion') == 'success' and ci.get('repository', {}).get('full_name') == REPOSITORY
            and type(ci.get('run_attempt')) is int and ci['run_attempt'] > 0, 'source_ci')
    require(workflow.get('id') == ci.get('workflow_id')
            and workflow.get('path') == '.github/workflows/ci.yml'
            and workflow.get('name') == 'Fork CI', 'source_ci_workflow')
    return commit['commit']['tree']['sha']


def collect(directory, run_id):
    require(os.getuid() != 0 and os.environ.get('GITHUB_ACTIONS') == 'true'
            and os.environ.get('RUNNER_ENVIRONMENT') == 'github-hosted'
            and os.environ.get('GITHUB_REPOSITORY') == REPOSITORY
            and os.environ.get('GITHUB_EVENT_NAME') == 'workflow_dispatch', 'hosted_manual_job_required')
    require(str(run_id).isdigit() and int(run_id) > 0, 'source_ci_id')
    revision = os.environ['GITHUB_SHA']

    def get(route):
        return object_json(command(['gh', 'api', '--hostname', 'github.com',
                                   'repos/' + REPOSITORY + route], timeout=60), 8 << 20)

    repo, commit, ci = get(''), get('/commits/' + revision), get('/actions/runs/' + str(run_id))
    workflow = get('/actions/workflows/' + str(ci['workflow_id']))
    tree = validate(repo, commit, ci, workflow, revision)
    require(os.environ['GITHUB_REF'] == 'refs/heads/' + repo['default_branch'], 'default_branch_dispatch_required')
    require(command(['git', 'rev-parse', 'HEAD']).decode().strip() == revision
            and command(['git', 'rev-parse', 'HEAD^{tree}']).decode().strip() == tree
            and not command(['git', 'status', '--porcelain']), 'source_checkout_changed')
    directory.mkdir(mode=0o700)
    for name, value in [('repository', repo), ('commit', commit), ('ci-run', ci), ('workflow', workflow)]:
        exclusive_json(directory / (name + '.json'), value)
    exclusive_json(directory / 'dispatch.json', {
        'repository': REPOSITORY, 'revision': revision, 'tree': tree,
        'event': 'workflow_dispatch', 'run_id': int(os.environ['GITHUB_RUN_ID']),
        'attempt': int(os.environ['GITHUB_RUN_ATTEMPT']),
        'actor': os.environ['GITHUB_ACTOR'], 'runner_environment': 'github-hosted',
        'native_pass': False})


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--directory', required=True, type=Path)
    parser.add_argument('--source-ci-run', required=True)
    args = parser.parse_args()
    try:
        collect(args.directory, args.source_ci_run)
    except (Refused, OSError, ValueError, KeyError, subprocess.SubprocessError) as error:
        print('source_evidence=refused code=' + (str(error) if isinstance(error, Refused) else type(error).__name__), file=sys.stderr)
        raise SystemExit(78)
