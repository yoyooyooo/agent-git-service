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
    'upgrade.py', 'upgrade_host.py', 'upgrade_engine.py', 'upgrade_identity.py',
    'upgrade_host_evidence.py', 'upgrade_bootstrap.py', 'upgrade_adapter_launchd.py',
    'upgrade_adapter_systemd.py', 'upgrade_metadata.py', 'upgrade_trash.py',
    'upgrade_bundle.py', 'ags-python.sh',
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
