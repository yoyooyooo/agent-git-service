#!/usr/bin/env python3
"""Offline bundle builder and verifier. Staging only; no host provision.

The manifest does not contain its own digest. Verification accepts only the
fixed v2 document: at most 65536 original bytes, strict UTF-8, no duplicate
keys, and exactly schema, entry and files. Success still sets installable
false. A bundle missing a required member, or containing an unexpected
executable, symlink, or owner, is refused.
"""
import sys
sys.dont_write_bytecode = True
sys.pycache_prefix = None
import hashlib
import json
import os
from pathlib import Path
import stat

REQUIRED = (
    'routeb_stage0_client.py', 'routeb_stage0_probe.py',
    'routeb_native_delegation.py', 'routeb_native_recovery.py',
    'upgrade', 'ags-python.sh', 'ags-runtime.py', 'upgrade.py', 'upgrade_host.py',
    'upgrade_engine.py', 'upgrade_identity.py', 'upgrade_host_evidence.py',
    'upgrade_bootstrap.py', 'upgrade_adapter_launchd.py', 'upgrade_adapter_systemd.py',
    'upgrade_native_runner.py', 'upgrade_native_parse.py',
    'upgrade_metadata.py', 'upgrade_metadata_darwin.py', 'upgrade_trash.py', 'upgrade_bundle.py', 'upgrade_wireup.py',
    'upgrade_wireup_safety.py', 'routeb_candidate.py', 'routeb_host_admission.py',
    'routeb_writer_probe.py', 'routeb_native_harness.py', 'routeb_native_capture.py',
    'routeb_host_identity.py', 'routeb_host_package.py', 'routeb_host_native.py',
    'routeb_launcher.py', 'routeb_host_execution.py', 'routeb_immutable_authority.py',
    'routeb_native_bootstrap.py',
    'upgrade_backup_contract.py', 'upgrade_inventory.py', 'upgrade_backup.py',
    'upgrade_restore_verify.py', 'upgrade_recovery.py', 'upgrade_manual_approval.py',
    'upgrade_lifecycle.py', 'upgrade_recovery_cli.py', 'upgrade_forward_recovery.py',
    'upgrade_full_restore.py', 'routeb_full_restore.py', 'routeb_host_transaction.py', 'routeb_observation.py',
    'ags_maintenance_window.py',
    'upgrade_state.py', 'upgrade_handoff.py', 'upgrade_pin.py', 'upgrade_smoke.py',
    'release_verify.py', 'release_contract.py', 'release_install_v2.py', 'install-release.py',
    'upgrade_policy.json', 'release-source.json',
)
SCHEMA = 'ags.upgrade-tool-bundle.v2'
ENTRY = 'upgrade_host.py'
_MANIFEST_BUDGET = 65536
_MAX_MANIFEST_DEPTH = 2
_MANIFEST_FIELDS = {'schema', 'entry', 'files'}


class BundleError(ValueError):
    def __init__(self, reason, exit_code=69):
        super().__init__(reason)
        self.code = reason
        self.exit_code = exit_code


def build(source, staging):
    """Copy only the closed member list into a new staging directory."""
    source, staging = Path(source), Path(staging)
    if staging.exists():
        raise BundleError('staging_exists')
    staging.mkdir(mode=0o700)
    files = {}
    for name in REQUIRED:
        path = source / name
        if name == 'release-source.json' and not path.exists():
            path = source.parent / 'fork/release-source.json'
        if not path.is_file() or path.is_symlink():
            raise BundleError('member_missing')
        raw = path.read_bytes()
        dest = staging / name
        dest.write_bytes(raw)
        dest.chmod(0o644)
        files[name] = hashlib.sha256(raw).hexdigest()
    manifest = {'schema': SCHEMA, 'entry': ENTRY, 'files': files}
    raw = json.dumps(manifest, sort_keys=True, separators=(',', ':')).encode()
    (staging / 'manifest.json').write_bytes(raw)
    return {'manifest_sha256': hashlib.sha256(raw).hexdigest(), 'members': len(files)}


def _read_manifest_bytes(path):
    """Read at most one byte past the budget, then refuse the oversize file."""
    with path.open('rb') as handle:
        raw = handle.read(_MANIFEST_BUDGET + 1)
    if len(raw) > _MANIFEST_BUDGET:
        raise BundleError('manifest_budget_exceeded')
    return raw


def _reject_deep(text):
    """The closed manifest is only the top object plus the files object."""
    depth = 0
    in_string = False
    escape = False
    for char in text:
        if in_string:
            if escape:
                escape = False
            elif char == '\\':
                escape = True
            elif char == '"':
                in_string = False
            continue
        if char == '"':
            in_string = True
        elif char in '{[':
            depth += 1
            if depth > _MAX_MANIFEST_DEPTH:
                raise BundleError('manifest_invalid')
        elif char in '}]' and depth:
            depth -= 1


def _unique_pairs(items):
    result = {}
    for key, value in items:
        if key in result:
            raise BundleError('manifest_invalid')
        result[key] = value
    return result


def _reject_constant(_value):
    raise BundleError('manifest_invalid')


def _load_manifest(raw):
    try:
        text = raw.decode('utf-8')
    except UnicodeError:
        raise BundleError('manifest_invalid') from None
    _reject_deep(text)
    try:
        manifest = json.loads(text, object_pairs_hook=_unique_pairs, parse_constant=_reject_constant)
    except BundleError:
        raise
    except (UnicodeError, ValueError, RecursionError):
        raise BundleError('manifest_invalid') from None
    if (type(manifest) is not dict or set(manifest) != _MANIFEST_FIELDS
            or manifest['schema'] != SCHEMA or manifest['entry'] != ENTRY
            or type(manifest['files']) is not dict):
        raise BundleError('manifest_invalid')
    files = manifest['files']
    if set(files) != set(REQUIRED):
        raise BundleError('member_set_rejected')
    if any(type(digest) is not str for digest in files.values()):
        raise BundleError('manifest_invalid')
    return files


def verify(staging, *, executable=True):
    staging = Path(staging)
    manifest_path = staging / 'manifest.json'
    if not manifest_path.is_file():
        raise BundleError('manifest_missing')
    raw = _read_manifest_bytes(manifest_path)
    files = _load_manifest(raw)
    present = {path.name for path in staging.iterdir()}
    if present != set(REQUIRED) | {'manifest.json'}:
        raise BundleError('unexpected_member')
    for name, digest in files.items():
        path = staging / name
        info = path.lstat()
        if path.is_symlink() or not stat.S_ISREG(info.st_mode):
            raise BundleError('member_type_rejected')
        if info.st_uid != os.getuid():
            raise BundleError('owner_rejected')
        if hashlib.sha256(path.read_bytes()).hexdigest() != digest:
            raise BundleError('digest_rejected')
        if executable and stat.S_IMODE(info.st_mode) & 0o111:
            raise BundleError('unexpected_executable')
    return {'verified': True, 'installable': False, 'manifest_sha256': hashlib.sha256(raw).hexdigest()}
