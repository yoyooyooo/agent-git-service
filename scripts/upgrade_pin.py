#!/usr/bin/env python3
"""Offline release/pin preparation, not a crypto verifier or publisher.

Always emits a non-installable envelope, never ags.deploy-release-pin.v2. Actual
pin publication needs a separately approved real Release and crypto verifier.
"""
import hashlib
import json
from types import SimpleNamespace

import release_contract as contract
from release_install_v2 import inventory
from upgrade import validate_pin, Failure
from upgrade_backup_contract import strict_json


def prepare(candidate, source_raw, release, asset_bytes):
    """Validate exact nine-asset bytes and candidate binding without effects."""
    try:
        source = strict_json(source_raw)
        validate_pin(candidate, source=source)
        runtime = source['runtime']
        if (set(source) != {'schema', 'version', 'runtime', 'acceptance'}
                or set(source['acceptance']) != {'runtime_pull_request', 'disclosure'}
                or type(source['acceptance']['runtime_pull_request']) is not int
                or source['acceptance']['runtime_pull_request'] <= 0
                or source['acceptance']['disclosure'] != 'fork/PUBLICATION.md'
                or release.get('id') != candidate['release_id']
                or type(release.get('id')) is not int
                or release.get('tag_name') != candidate['version']
                or release.get('draft') is not False or release.get('immutable') is not True
                or release.get('prerelease') is not ('-rc' in candidate['version'])):
            raise ValueError('release_identity_mismatch')
        limits = SimpleNamespace(MAX_ARCHIVE=contract.MAX_ARCHIVE)
        assets = inventory(limits, release, candidate['version'])
        if set(asset_bytes) != set(assets):
            raise ValueError('release_artifact_unverified')
        for name, row in assets.items():
            raw = asset_bytes[name]
            if (type(raw) is not bytes or len(raw) != row['size']
                    or 'sha256:' + hashlib.sha256(raw).hexdigest() != row['digest']):
                raise ValueError('release_digest_mismatch')
        if asset_bytes['release-source.json'] != source_raw:
            raise ValueError('release_identity_mismatch')
        sums = ''.join(hashlib.sha256(asset_bytes[name]).hexdigest() + '  ' + name + '\n'
                       for name in sorted(set(assets) - {'SHA256SUMS'})).encode()
        if asset_bytes['SHA256SUMS'] != sums:
            raise ValueError('release_digest_mismatch')
        for target, row in candidate['targets'].items():
            asset = row['asset']
            if asset != {key: assets[asset['name']][key] for key in ('id', 'name', 'size', 'digest')}:
                raise ValueError('release_identity_mismatch')
            info = json.loads(asset_bytes['build-info-' + target + '.json'])
            contract.validate_manifest_contract(info, target)
            if (info.get('repository') != contract.REPO or info.get('repository_id') != contract.REPO_ID
                    or info.get('revision') != runtime['revision'] or info.get('tree') != runtime['tree']
                    or info.get('version') != candidate['version'] or info.get('target') != target
                    or info.get('binaries') != row['binaries']):
                raise ValueError('release_identity_mismatch')
    except Failure:
        raise ValueError('release_pin_invalid') from None
    return {'schema': 'ags.release-pin-preparation.v1', 'installable': False,
            'source_sha256': hashlib.sha256(source_raw).hexdigest(),
            'candidate_sha256': hashlib.sha256(json.dumps(candidate, sort_keys=True).encode()).hexdigest(),
            'release_id': release['id'], 'asset_ids': sorted(row['id'] for row in assets.values()),
            'verification': {'binding': 'checked', 'asset_bytes': 'checked',
                'crypto': 'unverified', 'tag_push': 'unverified', 'archive_execution': 'unverified',
                'immutable_api_readback': 'unverified', 'human_control_approval': 'unverified'},
            'required': ['real-tag-push-provenance', 'both-platform-crypto-verification',
                'immutable-release-api-readback', 'exact-C-R-T-CI-and-control-human-approval',
                'host-policy-and-bundle-digests', 'separately-approved-pin-generation']}
