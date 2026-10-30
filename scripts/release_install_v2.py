"""V2 consumer for a complete reviewed checkout; never used by v1 bootstrap."""
import base64
import hashlib
import importlib
import json
from pathlib import Path
import re
import platform
import tempfile

TARGETS = ('darwin_arm64', 'linux_amd64')
verifier = importlib.import_module('release_verify')


def source_at(installer, control, version):
    path = 'fork/release-source.json'
    row = installer.api('repos/' + installer.REPO + '/contents/' + path + '?ref=' + control)
    if row.get('type') != 'file' or row.get('path') != path or row.get('encoding') != 'base64':
        raise ValueError('release_manifest_invalid')
    raw = base64.b64decode(row['content'], validate=False)
    if len(raw) > 65536 or hashlib.sha1(b'blob ' + str(len(raw)).encode() + b'\0' + raw).hexdigest() != row['sha']:
        raise ValueError('release_digest_mismatch')
    source = json.loads(raw)
    runtime = source.get('runtime', {})
    if (set(source) != {'schema', 'version', 'runtime', 'acceptance'}
            or source.get('schema') != 'ags.release-source.v1' or source.get('version') != version
            or set(runtime) != {'revision', 'tree'}
            or any(not isinstance(v, str) or not installer.SHA.fullmatch(v) for v in runtime.values())
            or runtime['revision'] == control
            or set(source.get('acceptance', {})) != {'runtime_pull_request', 'disclosure'}
            or type(source['acceptance'].get('runtime_pull_request')) is not int
            or source['acceptance']['runtime_pull_request'] <= 0
            or source['acceptance'].get('disclosure') != 'fork/PUBLICATION.md'):
        raise ValueError('release_identity_mismatch')
    commit = installer.api('repos/' + installer.REPO + '/git/commits/' + runtime['revision'])
    comparison = installer.api('repos/' + installer.REPO + '/compare/' + runtime['revision'] + '...' + control)
    if (commit['tree']['sha'] != runtime['tree'] or comparison.get('status') != 'ahead'
            or comparison.get('merge_base_commit', {}).get('sha') != runtime['revision']):
        raise ValueError('release_identity_mismatch')
    return raw, source


def inventory(installer, release, version):
    names = {'release-source.json', 'install.sh', 'SHA256SUMS'}
    for target in TARGETS:
        names.update({'ags-' + version + '-' + target + '.tar.gz',
                      'build-info-' + target + '.json', 'attestation-' + target + '.sigstore.json'})
    rows = release.get('assets', [])
    if len(rows) != 9 or {r['name'] for r in rows} != names:
        raise ValueError('release_artifact_unverified')
    for row in rows:
        maximum = installer.MAX_ARCHIVE if row['name'].endswith('.tar.gz') else 1024 * 1024
        if (type(row.get('id')) is not int or row['id'] <= 0 or type(row.get('size')) is not int
                or not 0 < row['size'] <= maximum or not re.fullmatch(r'sha256:[0-9a-f]{64}', row.get('digest', ''))):
            raise ValueError('release_digest_mismatch')
    return {r['name']: r for r in rows}


def check_sums(cache, assets, installer):
    expected = ''.join(installer.digest(cache / name) + '  ' + name + '\n'
                       for name in sorted(set(assets) - {'SHA256SUMS'}))
    if (cache / 'SHA256SUMS').read_text() != expected:
        raise ValueError('release_digest_mismatch')


def host_abi(target):
    if target == 'darwin_arm64':
        try:
            return tuple(int(v) for v in platform.mac_ver()[0].split('.')[:2]) >= (15, 0)
        except ValueError:
            return False
    name, version = platform.libc_ver()
    try:
        return name == 'glibc' and tuple(int(v) for v in version.split('.')[:2]) >= (2, 35)
    except ValueError:
        return False


class InstallClosed(ValueError):
    """V2 replacement stays closed until backup, handoff and observation exist."""
    exit_code = 83


def require_install_contracts():
    # Same missing contracts as upgrade apply. A host entry or a command that
    # exits zero is not an implementation, and this gate does not read either.
    missing = ['backup_git_config_coverage_and_git_db_consistency',
               'independent_observation_approval_and_finalize',
               'trusted_mini_handoff_bound_to_pin_release_deployment']
    error = InstallClosed('install_contracts_unimplemented')
    error.missing_contracts = missing
    raise error


def run(installer, args, release, control, target):
    if getattr(args, 'install', False):
        # Closed before downloads, scratch, installation root, locks or candidate execution.
        require_install_contracts()
    if (release.get('tag_name') != args.version or type(release.get('id')) is not int
            or release.get('prerelease') is not ('-rc' in args.version)):
        raise ValueError('release_identity_mismatch')
    assets = inventory(installer, release, args.version)
    source_raw, source = source_at(installer, control, args.version)
    prefix = args.prefix.expanduser().absolute()
    if prefix.resolve() != prefix or prefix.is_symlink():
        raise ValueError('release_pin_invalid')
    receipt = {'schema': installer.INSTALLATION_SCHEMA, 'repository': installer.REPO,
               'version': args.version, 'source': source['runtime']['revision'],
               'runtime_tree': source['runtime']['tree'], 'control_revision': control,
               'release_id': release['id'], 'tag': args.version, 'target': target,
               'prefix': str(prefix), 'install': args.install, 'service_or_data_changed': False,
               'verification': {'metadata': 'verified', 'asset_bytes': 'unverified',
                                'immutable_release': 'unverified', 'attestation': 'unverified',
                                'archive': 'unverified', 'executable_identity': 'unverified'}}
    if not args.install and not args.verify_only:
        return receipt
    contract = installer.adjacent_module('release_contract')
    pin = {'schema': contract.PIN_SCHEMA, 'repository': installer.REPO,
           'repository_id': installer.REPO_ID, 'runtime': source['runtime'],
           'targets': {name: {'manifest_schema': contract.MANIFEST_SCHEMA, 'abi': contract.abi_for(name),
                     'provenance': {'control_revision': control, 'signer_digest': control,
                                    'signer_workflow': verifier.WORKFLOW}} for name in TARGETS}}
    # Verification has no installation-root writes, including on failure.
    with tempfile.TemporaryDirectory(prefix='ags-release-verify-') as directory:
        cache = Path(directory)
        for name, row in assets.items():
            installer.execute(['gh', 'release', 'download', args.version, '--repo', installer.REPO,
                               '--pattern', name, '--dir', str(cache)])
            path = cache / name
            if path.is_symlink() or path.stat().st_size != row['size'] or 'sha256:' + installer.digest(path) != row['digest']:
                raise ValueError('release_digest_mismatch')
            installer.execute(['gh', 'release', 'verify-asset', args.version, str(path), '--repo', installer.REPO])
        check_sums(cache, assets, installer)
        if (cache / 'release-source.json').read_bytes() != source_raw:
            raise ValueError('release_identity_mismatch')
        runs = set()
        attestations = {}
        for name in TARGETS:
            archive = cache / ('ags-' + args.version + '-' + name + '.tar.gz')
            bundle = cache / ('attestation-' + name + '.sigstore.json')
            # verify_release checks the SUCCESSFUL gh output and its certificate;
            # raw DSSE payloads never select expected runtime, signer or run.
            proof = verifier.verify_release(archive, pin, name, bundle=bundle, source=source,
                                            version=args.version)
            runs.add((proof['run_id'], proof['run_attempt']))
            info = contract.inspect_bundle(archive, args.version, receipt['source'], name)
            if (info.get('schema') != contract.MANIFEST_SCHEMA or info['tree'] != receipt['runtime_tree']
                    or info != json.loads((cache / ('build-info-' + name + '.json')).read_bytes())):
                raise ValueError('release_manifest_invalid')
            attestations[name] = proof
            if name == target:
                selected_info, selected_archive = info, archive
        if len(runs) != 1:
            raise ValueError('release_attestation_failed')
        receipt.update(run_id=next(iter(runs))[0], run_attempt=next(iter(runs))[1],
                       attestation=attestations, asset_sha256=installer.digest(selected_archive),
                       binaries=selected_info['binaries'], immutable_release_verified=True,
                       build_provenance_verified=True)
        receipt['verification'].update(asset_bytes='verified', immutable_release='verified',
                                       attestation='verified', archive='verified')
        return receipt
