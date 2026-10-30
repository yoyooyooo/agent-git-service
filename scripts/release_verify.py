#!/usr/bin/env python3
"""Check v2 release binding evidence. Production separated-control is NOT enabled.

No installation, publication or service effects. Only gh-verified statements are
read as signed evidence. Unit fixtures are not Sigstore bundles.
"""
import sys
sys.dont_write_bytecode = True
sys.pycache_prefix = None
import argparse
import hashlib
import importlib.util
import json
from pathlib import Path
import re
import subprocess
import sys

_spec = importlib.util.spec_from_file_location('ags_installer_contract', Path(__file__).with_name('release_contract.py'))
contract = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(contract)
REPOSITORY = 'yoyooyooo/agent-git-service'
REPOSITORY_ID = 1384519373
WORKFLOW = REPOSITORY + '/.github/workflows/release-control.yml'
PREDICATE_TYPE = 'https://github.com/yoyooyooo/agent-git-service/attestations/runtime-binding/v1'
# Bundle provisioning preserves this reviewed source at the adjacent fixed path.
_source_path = Path(__file__).resolve().parents[1] / 'fork/release-source.json'
if not _source_path.is_file():
    _source_path = Path(__file__).with_name('release-source.json')
RELEASE_SOURCE = json.loads(_source_path.read_text())
RUNTIME_REVISION = RELEASE_SOURCE['runtime']['revision']
RUNTIME_TREE = RELEASE_SOURCE['runtime']['tree']
MODE = 'separated-control-v1'
SEPARATED_CONTROL_ENABLED = False
SHA = re.compile(r'[0-9a-f]{40}')


class Rejected(ValueError):
    def __init__(self, code):
        super().__init__(code)
        self.exit_code = 64 if code == 'release_pin_invalid' else 65


def binding_policy(pin, target, *, source=None):
    """Validate the pin's binding/ABI portion, not installation authorization."""
    if pin.get('schema') != contract.PIN_SCHEMA or pin.get('repository') != REPOSITORY or type(pin.get('repository_id')) is not int or pin['repository_id'] != REPOSITORY_ID:
        raise Rejected('release_pin_invalid')
    source = RELEASE_SOURCE if source is None else source
    runtime = source.get('runtime', {})
    if source.get('schema') != 'ags.release-source.v1' or set(runtime) != {'revision', 'tree'} or any(not isinstance(v, str) or not SHA.fullmatch(v) for v in runtime.values()):
        raise Rejected('release_pin_invalid')
    if pin.get('runtime') != runtime:
        raise Rejected('release_identity_mismatch')
    # A v2 pin cannot carry a retired platform declaration anywhere.
    serialized = json.dumps(pin)
    if '24.04' in serialized or '2.39' in serialized:
        raise Rejected('release_abi_incompatible')
    targets = pin.get('targets', {})
    if set(targets) != {'linux_amd64', 'darwin_arm64'} or target not in targets:
        raise Rejected('release_pin_invalid')
    for name, entry in targets.items():
        if entry.get('manifest_schema') != contract.MANIFEST_SCHEMA:
            raise Rejected('release_pin_invalid')
        try:
            contract.validate_abi(entry.get('abi'), name)
        except ValueError:
            raise Rejected('release_abi_incompatible') from None
    provenance = targets[target].get('provenance', {})
    if 'mode' in provenance:
        # No mode may be installed before real evidence and separate enablement.
        raise Rejected('release_source_binding_unverified')
    control = provenance.get('control_revision')
    if not isinstance(control, str) or not SHA.fullmatch(control) or control == runtime['revision']:
        raise Rejected('release_source_binding_unverified')
    if provenance.get('signer_workflow') != WORKFLOW or provenance.get('signer_digest') != control:
        raise Rejected('release_attestation_failed')
    return provenance


def verification_command(archive, pin, target, *, bundle=None, source=None):
    policy = binding_policy(pin, target, source=source)
    # Neither signer nor source policy can be overridden by command-line flags.
    args = ['gh', 'attestation', 'verify', str(archive), '--repo', REPOSITORY,
            '--source-digest', policy['control_revision'],
            '--signer-digest', policy['signer_digest'],
            '--signer-workflow', policy['signer_workflow'],
            '--deny-self-hosted-runners', '--predicate-type', PREDICATE_TYPE,
            '--format', 'json']
    if bundle is not None:
        args.extend(['--bundle', str(bundle)])
    return args


def check_verified_statement(rows, archive_digest, *, source=None, subject_name=None):
    """Consume only stdout from successful gh verification, never raw DSSE JSON."""
    if not isinstance(rows, list) or len(rows) != 1:
        raise Rejected('release_attestation_failed')
    statement = rows[0].get('verificationResult', {}).get('statement', {})
    if statement.get('predicateType') != PREDICATE_TYPE:
        raise Rejected('release_source_binding_unverified')
    runtime = (RELEASE_SOURCE if source is None else source)['runtime']
    if statement.get('predicate') != {'schema': 'ags.runtime-binding.v1',
                                      'runtime_revision': runtime['revision'],
                                      'runtime_tree': runtime['tree']}:
        raise Rejected('release_source_binding_unverified')
    subjects = statement.get('subject', [])
    if not isinstance(subjects, list) or len(subjects) != 1 or subjects[0].get('digest') != {'sha256': archive_digest}:
        raise Rejected('release_digest_mismatch')
    if subject_name is not None and subjects[0].get('name') != subject_name:
        raise Rejected('release_digest_mismatch')
    return statement


def verify_evidence(archive, pin, target, *, bundle=None, source=None, invocation_uri=None, source_ref=None):
    args = verification_command(archive, pin, target, bundle=bundle, source=source)
    try:
        result = subprocess.run(args, capture_output=True, timeout=180)
    except (subprocess.TimeoutExpired, OSError):
        raise Rejected('release_attestation_failed') from None
    if result.returncode:
        # Provider diagnostics can contain private paths; do not relay them.
        raise Rejected('release_attestation_failed')
    try:
        rows = json.loads(result.stdout)
        if invocation_uri is not None:
            extensions = rows[0]['verificationResult']['signature']['certificate']
            if (extensions.get('runInvocationURI') != invocation_uri
                    or extensions.get('sourceRepositoryRef') != source_ref
                    or extensions.get('buildTrigger') != 'push'):
                raise Rejected('release_attestation_failed')
        digest = hashlib.sha256(Path(archive).read_bytes()).hexdigest()
        return check_verified_statement(rows, digest, source=source)
    except (KeyError, IndexError, TypeError, AttributeError, json.JSONDecodeError):
        raise Rejected('release_attestation_failed') from None


def verify_release(archive, pin, target, *, bundle, source, version):
    """Installer-only v2 path. The legacy production pin gate stays closed."""
    args = verification_command(archive, pin, target, bundle=bundle, source=source)
    args.extend(['--source-ref', 'refs/tags/' + version,
                 '--cert-oidc-issuer', 'https://token.actions.githubusercontent.com'])
    try:
        result = subprocess.run(args, capture_output=True, timeout=180)
    except FileNotFoundError:
        error = Rejected('dependency_unavailable'); error.exit_code = 69
        raise error from None
    except (subprocess.TimeoutExpired, OSError):
        raise Rejected('release_attestation_failed') from None
    if result.returncode:
        raise Rejected('release_attestation_failed')
    try:
        rows = json.loads(result.stdout)
        check_verified_statement(rows, hashlib.sha256(Path(archive).read_bytes()).hexdigest(),
                                 source=source, subject_name=Path(archive).name)
        cert = rows[0]['verificationResult']['signature']['certificate']
        control = pin['targets'][target]['provenance']['control_revision']
        ref = 'refs/tags/' + version
        expected = {'sourceRepositoryDigest': control, 'buildSignerDigest': control,
                    'sourceRepositoryIdentifier': str(REPOSITORY_ID),
                    'sourceRepositoryURI': 'https://github.com/' + REPOSITORY,
                    'sourceRepositoryRef': ref, 'buildTrigger': 'push',
                    'runnerEnvironment': 'github-hosted',
                    'issuer': 'https://token.actions.githubusercontent.com',
                    'buildSignerURI': 'https://github.com/' + WORKFLOW + '@' + ref}
        if any(cert.get(k) != v for k, v in expected.items()):
            raise Rejected('release_attestation_failed')
        match = re.fullmatch('https://github.com/' + re.escape(REPOSITORY)
                             + r'/actions/runs/([1-9][0-9]*)/attempts/([1-9][0-9]*)',
                             cert.get('runInvocationURI', ''))
        if not match:
            raise Rejected('release_attestation_failed')
        return {'run_id': match[1], 'run_attempt': match[2], 'control_revision': control,
                'signer_workflow': WORKFLOW, 'predicate_type': PREDICATE_TYPE,
                'bundle_sha256': hashlib.sha256(Path(bundle).read_bytes()).hexdigest(),
                'verified': True}
    except (KeyError, IndexError, TypeError, AttributeError, json.JSONDecodeError):
        raise Rejected('release_attestation_failed') from None


def verify_production(archive, pin, target, *, bundle=None):
    if not SEPARATED_CONTROL_ENABLED:
        raise Rejected('release_source_binding_unverified')
    # This branch is intentionally unreachable until separately reviewed enablement.
    return verify_evidence(archive, pin, target, bundle=bundle)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('archive', type=Path)
    parser.add_argument('--pin', required=True, type=Path)
    parser.add_argument('--target', required=True, choices=['linux_amd64', 'darwin_arm64'])
    parser.add_argument('--bundle', type=Path)
    args = parser.parse_args()
    # No installed pin is generated or modified here. The production gate stays closed.
    verify_production(args.archive, json.loads(args.pin.read_text()), args.target, bundle=args.bundle)


if __name__ == '__main__':
    try:
        main()
    except Rejected as error:
        print(str(error), file=sys.stderr)
        sys.exit(error.exit_code)
