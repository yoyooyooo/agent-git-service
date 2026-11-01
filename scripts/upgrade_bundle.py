#!/usr/bin/env python3
"""Offline bundle builder and verifier. Staging only; no host provision.

The manifest does not contain its own digest. A bundle missing a required
member, or containing an unexpected executable, symlink, or owner, is refused.
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
    'upgrade', 'ags-python.sh', 'ags-runtime.py', 'upgrade.py', 'upgrade_host.py',
    'upgrade_engine.py', 'upgrade_identity.py', 'upgrade_host_evidence.py',
    'upgrade_bootstrap.py', 'upgrade_adapter_launchd.py', 'upgrade_adapter_systemd.py',
    'upgrade_metadata.py', 'upgrade_trash.py', 'upgrade_bundle.py', 'upgrade_wireup.py',
    'upgrade_backup_contract.py', 'upgrade_inventory.py', 'upgrade_backup.py',
    'upgrade_restore_verify.py', 'upgrade_recovery.py', 'upgrade_manual_approval.py',
    'upgrade_lifecycle.py', 'upgrade_recovery_cli.py', 'upgrade_forward_recovery.py',
    'upgrade_full_restore.py', 'upgrade_handoff.py', 'upgrade_pin.py', 'upgrade_smoke.py',
    'release_verify.py', 'release_contract.py', 'release_install_v2.py', 'install-release.py',
    'upgrade_policy.json', 'release-source.json',
)
SCHEMA = 'ags.upgrade-tool-bundle.v2'


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
    manifest = {'schema': SCHEMA, 'entry': 'upgrade_host.py', 'files': files}
    raw = json.dumps(manifest, sort_keys=True, separators=(',', ':')).encode()
    (staging / 'manifest.json').write_bytes(raw)
    return {'manifest_sha256': hashlib.sha256(raw).hexdigest(), 'members': len(files)}


def verify(staging, *, executable=True):
    staging = Path(staging)
    manifest_path = staging / 'manifest.json'
    if not manifest_path.is_file():
        raise BundleError('manifest_missing')
    raw = manifest_path.read_bytes()
    manifest = json.loads(raw)
    files = manifest.get('files', {})
    if set(files) != set(REQUIRED):
        raise BundleError('member_set_rejected')
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
