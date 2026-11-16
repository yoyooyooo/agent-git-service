#!/usr/bin/env python3
"""Offline release/pin preparation, not a crypto verifier or publisher.

emit_verified returns schema ags.deploy-release-pin.v2 with installable=false.
That envelope is not publication. A real pin still needs a separately approved
Release and a cryptographic verifier. Only a verifier that returns True is
accepted; any other return value or any exception is a refusal.
"""
import hashlib
import inspect
import json
from types import SimpleNamespace

import release_contract as contract
from release_install_v2 import inventory
from upgrade import validate_pin, Failure
from upgrade_backup_contract import strict_json


def _evidence_fields(evidence, version):
    """Structure check only. Does not call GitHub, cosign, or a Release API."""
    digest = evidence.get('digest')
    bundle = evidence.get('cosign_bundle')
    if (evidence.get('source') != 'workflow_run'
            or evidence.get('event') != 'push'
            or evidence.get('verified') is not True
            or evidence.get('signer_workflow') != 'release-control'
            or evidence.get('runner_environment') != 'github-hosted'
            or evidence.get('build_trigger') != 'push'
            or evidence.get('source_ref') != 'refs/tags/' + version
            or type(digest) is not str or len(digest) != 71 or not digest.startswith('sha256:')
            or any(char not in '0123456789abcdef' for char in digest[7:])
            or type(bundle) is not str or not bundle.startswith('application/vnd.dev.sigstore.bundle')):
        raise ValueError('release_attestation_unverified')


def verify_bundle(digest_value, signature, *, bundle=None):
    """Source verifier. A missing or non-matching bundle is a refusal.

    This does not call cosign or GitHub. The caller supplies the bundle bytes
    and the expected digest. A production caller that has no bundle must not
    pass a substitute.
    """
    if not isinstance(bundle, (bytes, bytearray)) or not bundle:
        raise ValueError('release_attestation_unverified')
    if not isinstance(signature, (bytes, bytearray)) or signature not in bundle:
        raise ValueError('release_attestation_unverified')
    if not isinstance(digest_value, str) or digest_value.encode() not in bundle:
        raise ValueError('release_attestation_unverified')
    return True


def _accepts_bundle_keyword(signature):
    """True only when the callable can be invoked with a bundle keyword.

    Signature inspection selects the call shape before any execution. A
    TypeError raised by the verifier body is never treated as incompatibility.
    """
    parameters = signature.parameters.values()
    if any(parameter.kind is inspect.Parameter.VAR_KEYWORD for parameter in parameters):
        return True
    bundle = signature.parameters.get('bundle')
    if bundle is None:
        return False
    return bundle.kind in (inspect.Parameter.POSITIONAL_OR_KEYWORD, inspect.Parameter.KEYWORD_ONLY)


def emit_verified(candidate, source_raw, release, asset_bytes, *, attestations, verifier=None):
    """Emit only after both platform digests match and the verifier accepts.

    verifier is required. Omitting it is a refusal, not a success.
    This function does not call GitHub or cosign. Output stays installable=false.
    """
    if not callable(verifier):
        raise ValueError('release_attestation_unverified')
    targets = set(candidate.get('targets', {}))
    if targets != {'darwin_arm64', 'linux_amd64'} or not isinstance(attestations, dict):
        raise ValueError('release_attestation_unverified')
    for target in sorted(targets):
        evidence = attestations.get(target)
        if not isinstance(evidence, dict):
            raise ValueError('release_attestation_unverified')
        _evidence_fields(evidence, candidate.get('version', ''))
        expected = ((candidate.get('targets') or {}).get(target) or {}).get('asset', {}).get('digest')
        if evidence.get('digest') != expected:
            raise ValueError('release_attestation_unverified')
        if 'run_id' not in evidence or 'run_attempt' not in evidence:
            raise ValueError('release_attestation_unverified')
        digest = evidence.get('digest')
        signature = evidence.get('signature')
        bundle = evidence.get('bundle_bytes')
        try:
            signature_probe = inspect.signature(verifier)
            accepts_bundle = _accepts_bundle_keyword(signature_probe)
        except (TypeError, ValueError):
            accepts_bundle = False
        try:
            if accepts_bundle:
                accepted = verifier(digest, signature, bundle=bundle)
            else:
                accepted = verifier(digest, signature)
        except Exception:
            raise ValueError('release_attestation_unverified') from None
        if accepted is not True:
            raise ValueError('release_attestation_unverified')
    envelope = prepare(candidate, source_raw, release, asset_bytes)
    proof_input = {}
    for target, evidence in sorted(attestations.items()):
        row = {key: value for key, value in evidence.items()
               if key not in ('signature', 'bundle_bytes')}
        signature = evidence.get('signature')
        if isinstance(signature, (bytes, bytearray)):
            row['signature_sha256'] = hashlib.sha256(signature).hexdigest()
        bundle = evidence.get('bundle_bytes')
        if isinstance(bundle, (bytes, bytearray)):
            row['bundle_sha256'] = hashlib.sha256(bundle).hexdigest()
            row['bundle_length'] = len(bundle)
        proof_input[target] = row
    return {'schema': 'ags.deploy-release-pin.v2', 'installable': False,
            'candidate': candidate, 'proof_sha256': hashlib.sha256(
                json.dumps(proof_input, sort_keys=True, separators=(',', ':')).encode()).hexdigest(),
            'preparation': envelope,
            'verification': {'both_platforms': True, 'tag_push': True,
                             'install': 'not_performed', 'manager': 'not_performed'}}


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
