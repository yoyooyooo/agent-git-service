#!/usr/bin/env python3
"""Offline bundle builder and verifier. Staging only; no host provision.

The builder lstats each closed source path and does not follow that final
component. Only release-source.json may use another path, and only when that
first lstat is ENOENT; the other path is fork/release-source.json beside the
source directory. The selected file is opened once, without following and
without blocking. Its bytes and SHA256 come from that same descriptor, capped
at the first fstat size plus one probe byte. A symlink or other non-regular
source is member_missing. A change around the read is source_changed. Any
other source access failure is source_unreadable. Those codes carry no path
or body. The manifest is written only after every member copy has checked
out. This does not prove a Git revision and does not make the bundle
installable.

The manifest file itself must be a regular file owned by the current user.
Verification does not follow a symlink at that final path component. The
manifest does not contain its own digest. Verification accepts only the
fixed v2 document: at most 65536 original bytes, strict UTF-8, no duplicate
keys, and exactly schema, entry and files. Each ordinary member is then
opened once, without following its final component. That descriptor supplies
the metadata, the current-UID and executable-bit checks, and a streamed
SHA256 capped at the observed size plus one byte. The member chunk is not
the manifest budget. Success still sets installable false. A bundle missing
a required member, or containing an unexpected executable, symlink, owner,
or a member whose path changes around the read, is refused.
"""
import sys
sys.dont_write_bytecode = True
sys.pycache_prefix = None
import errno
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
_MEMBER_CHUNK = 65536
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
        path = _choose_source_path(source, name)
        dest = staging / name
        files[name] = _copy_source_member(path, dest)
        os.chmod(dest, 0o644)
    manifest = {'schema': SCHEMA, 'entry': ENTRY, 'files': files}
    raw = json.dumps(manifest, sort_keys=True, separators=(',', ':')).encode()
    (staging / 'manifest.json').write_bytes(raw)
    return {'manifest_sha256': hashlib.sha256(raw).hexdigest(), 'members': len(files)}


def _manifest_owner_uid():
    """UID compared with the manifest descriptor. Not an input, CLI, or environment override."""
    return os.getuid()


def _manifest_identity(info):
    """Stable file identity. atime is excluded because a read may update it."""
    return (info.st_dev, info.st_ino, info.st_mode, info.st_uid, info.st_size,
            info.st_mtime_ns, info.st_ctime_ns)


def _source_io_error(exc, *, started):
    """Map one source OSError. The message is only the fixed code."""
    missing = exc.errno in (errno.ENOENT, errno.ENOTDIR, errno.ELOOP, errno.EISDIR, errno.ENXIO)
    if not started and missing:
        code = 'member_missing'
    elif started and exc.errno in (errno.ENOENT, errno.ENOTDIR):
        code = 'source_changed'
    else:
        code = 'source_unreadable'
    raise BundleError(code) from None


def _lstat_source(path):
    while True:
        try:
            return os.lstat(path)
        except OSError as exc:
            if exc.errno == errno.EINTR:
                continue
            raise


def _choose_source_path(source, name):
    """Fallback only when release-source.json itself is missing. Do not choose twice."""
    primary = source / name
    try:
        _lstat_source(primary)
    except OSError as exc:
        if name == 'release-source.json' and exc.errno == errno.ENOENT:
            return source.parent / 'fork' / 'release-source.json'
        _source_io_error(exc, started=False)
    return primary


def _open_source(path):
    """Open the final component without following it. Never fall back to a following open."""
    nofollow = getattr(os, 'O_NOFOLLOW', None)
    nonblock = getattr(os, 'O_NONBLOCK', None)
    if nofollow is None or nonblock is None:
        raise BundleError('member_missing')
    flags = os.O_RDONLY | nofollow | nonblock
    while True:
        try:
            return os.open(path, flags)
        except OSError as exc:
            if exc.errno == errno.EINTR:
                continue
            _source_io_error(exc, started=False)


def _source_fstat(fd):
    while True:
        try:
            return os.fstat(fd)
        except OSError as exc:
            if exc.errno == errno.EINTR:
                continue
            raise BundleError('source_unreadable') from None


def _read_source(fd, size):
    while True:
        try:
            return os.read(fd, size)
        except OSError as exc:
            if exc.errno == errno.EINTR:
                continue
            raise BundleError('source_unreadable') from None


def _write_dest(fd, data):
    """Write staging bytes. Failures stay destination errors and do not select another source."""
    view = memoryview(data)
    while len(view):
        try:
            wrote = os.write(fd, view)
        except OSError as exc:
            if exc.errno == errno.EINTR:
                continue
            raise
        if wrote <= 0:
            raise OSError(errno.EIO, 'incomplete destination write')
        view = view[wrote:]


def _release_descriptor(fd):
    """Close once. A delayed close error stays a destination error.

    Do not close again: an error from close leaves the descriptor state unspecified.
    An earlier failure is kept; the close error must not replace it.
    """
    pending = sys.exc_info()[0]
    try:
        os.close(fd)
    except OSError:
        if pending is None:
            raise


def _release_source_descriptor(fd):
    """Close the source once. A new close error uses the source access code."""
    pending = sys.exc_info()[0]
    try:
        os.close(fd)
    except OSError as exc:
        if pending is None:
            _source_io_error(exc, started=True)


def _stream_source(fd, dest, size):
    """Copy and hash one descriptor. The extra probe byte is not written."""
    digest = hashlib.sha256()
    remaining = size
    out = os.open(dest, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o644)
    try:
        while True:
            request = 1 if remaining == 0 else min(_MEMBER_CHUNK, remaining)
            chunk = _read_source(fd, request)
            if remaining == 0:
                if chunk:
                    raise BundleError('source_changed')
                break
            if not chunk or len(chunk) > remaining:
                raise BundleError('source_changed')
            digest.update(chunk)
            _write_dest(out, chunk)
            remaining -= len(chunk)
    finally:
        _release_descriptor(out)
    return digest.hexdigest()


def _copy_source_member(path, dest):
    """Read, copy, and hash one selected source from a single no-follow descriptor."""
    try:
        info = _lstat_source(path)
    except OSError as exc:
        _source_io_error(exc, started=False)
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
        raise BundleError('member_missing')
    fd = _open_source(path)
    try:
        opened = _source_fstat(fd)
        if stat.S_ISLNK(opened.st_mode) or not stat.S_ISREG(opened.st_mode):
            raise BundleError('member_missing')
        if _manifest_identity(opened) != _manifest_identity(info):
            raise BundleError('source_changed')
        digest = _stream_source(fd, dest, opened.st_size)
        after = _source_fstat(fd)
        if _manifest_identity(after) != _manifest_identity(opened):
            raise BundleError('source_changed')
        while True:
            try:
                current = os.lstat(path)
                break
            except OSError as exc:
                if exc.errno == errno.EINTR:
                    continue
                raise BundleError('source_changed') from None
        if _manifest_identity(current) != _manifest_identity(after):
            raise BundleError('source_changed')
        return digest
    finally:
        _release_source_descriptor(fd)


def _manifest_access_error(exc):
    if exc.errno in (errno.ENOENT, errno.ENOTDIR):
        code = 'manifest_missing'
    elif exc.errno in (errno.ELOOP, errno.EISDIR, errno.ENXIO):
        code = 'manifest_type_rejected'
    else:
        code = 'manifest_unreadable'
    raise BundleError(code) from None


def _read_fd_bounded(fd, limit):
    chunks = []
    remaining = limit
    while remaining > 0:
        try:
            chunk = os.read(fd, remaining)
        except OSError as exc:
            if exc.errno == errno.EINTR:
                continue
            _manifest_access_error(exc)
        if not chunk:
            break
        chunks.append(chunk)
        remaining -= len(chunk)
    return b''.join(chunks)


def _open_manifest(path):
    """Open the final component without following it. Never fall back to a following open."""
    nofollow = getattr(os, 'O_NOFOLLOW', None)
    nonblock = getattr(os, 'O_NONBLOCK', None)
    if nofollow is None or nonblock is None:
        raise BundleError('manifest_type_rejected')
    flags = os.O_RDONLY | nofollow | nonblock
    while True:
        try:
            return os.open(path, flags)
        except OSError as exc:
            if exc.errno == errno.EINTR:
                continue
            _manifest_access_error(exc)


def _read_manifest_bytes(path, *, executable):
    """Read one no-follow descriptor, capped at one byte past the budget."""
    try:
        info = path.lstat()
    except OSError as exc:
        _manifest_access_error(exc)
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
        raise BundleError('manifest_type_rejected')
    fd = _open_manifest(path)
    try:
        try:
            opened = os.fstat(fd)
            if stat.S_ISLNK(opened.st_mode) or not stat.S_ISREG(opened.st_mode):
                raise BundleError('manifest_type_rejected')
            if _manifest_identity(opened) != _manifest_identity(info):
                raise BundleError('manifest_changed')
            if opened.st_uid != _manifest_owner_uid():
                raise BundleError('owner_rejected')
            if executable and stat.S_IMODE(opened.st_mode) & 0o111:
                raise BundleError('unexpected_executable')
            raw = _read_fd_bounded(fd, _MANIFEST_BUDGET + 1)
            after = os.fstat(fd)
            if _manifest_identity(after) != _manifest_identity(opened):
                raise BundleError('manifest_changed')
            try:
                current = path.lstat()
            except OSError:
                raise BundleError('manifest_changed') from None
            if _manifest_identity(current) != _manifest_identity(after):
                raise BundleError('manifest_changed')
            if len(raw) > _MANIFEST_BUDGET:
                raise BundleError('manifest_budget_exceeded')
            return raw
        except BundleError:
            raise
        except OSError as exc:
            _manifest_access_error(exc)
    finally:
        try:
            os.close(fd)
        except OSError:
            pass


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


def _member_owner_uid():
    """UID compared with a member descriptor. Not an input, CLI, or environment override."""
    return os.getuid()


def _member_access_error(exc):
    if exc.errno in (errno.ENOENT, errno.ENOTDIR):
        code = 'member_missing'
    elif exc.errno in (errno.ELOOP, errno.EISDIR, errno.ENXIO):
        code = 'member_type_rejected'
    else:
        code = 'member_unreadable'
    raise BundleError(code) from None


def _member_fstat(fd):
    while True:
        try:
            return os.fstat(fd)
        except OSError as exc:
            if exc.errno == errno.EINTR:
                continue
            _member_access_error(exc)


def _open_member(path):
    """Open the final component without following it. Never fall back to a following open."""
    nofollow = getattr(os, 'O_NOFOLLOW', None)
    nonblock = getattr(os, 'O_NONBLOCK', None)
    if nofollow is None or nonblock is None:
        raise BundleError('member_type_rejected')
    flags = os.O_RDONLY | nofollow | nonblock
    while True:
        try:
            return os.open(path, flags)
        except OSError as exc:
            if exc.errno == errno.EINTR:
                continue
            _member_access_error(exc)


def _hash_member_fd(fd, size):
    """Stream SHA256 for the observed size, plus one byte so growth stops the read."""
    digest = hashlib.sha256()
    remaining = size
    extra = True
    while remaining > 0 or extra:
        request = 1 if remaining == 0 else min(_MEMBER_CHUNK, remaining)
        while True:
            try:
                chunk = os.read(fd, request)
                break
            except OSError as exc:
                if exc.errno == errno.EINTR:
                    continue
                _member_access_error(exc)
        if remaining == 0:
            if chunk:
                raise BundleError('member_changed')
            break
        if not chunk or len(chunk) > remaining:
            raise BundleError('member_changed')
        digest.update(chunk)
        remaining -= len(chunk)
    return digest.hexdigest()


def _verify_member(path, digest, *, executable):
    """Check one member and hash it from the same no-follow descriptor."""
    try:
        info = path.lstat()
    except OSError as exc:
        _member_access_error(exc)
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
        raise BundleError('member_type_rejected')
    fd = _open_member(path)
    try:
        opened = _member_fstat(fd)
        if stat.S_ISLNK(opened.st_mode) or not stat.S_ISREG(opened.st_mode):
            raise BundleError('member_type_rejected')
        if _manifest_identity(opened) != _manifest_identity(info):
            raise BundleError('member_changed')
        if opened.st_uid != _member_owner_uid():
            raise BundleError('owner_rejected')
        if executable and stat.S_IMODE(opened.st_mode) & 0o111:
            raise BundleError('unexpected_executable')
        actual = _hash_member_fd(fd, opened.st_size)
        after = _member_fstat(fd)
        if _manifest_identity(after) != _manifest_identity(opened):
            raise BundleError('member_changed')
        try:
            current = path.lstat()
        except OSError:
            raise BundleError('member_changed') from None
        if _manifest_identity(current) != _manifest_identity(after):
            raise BundleError('member_changed')
        if actual != digest:
            raise BundleError('digest_rejected')
    finally:
        try:
            os.close(fd)
        except OSError:
            pass


def verify(staging, *, executable=True):
    staging = Path(staging)
    raw = _read_manifest_bytes(staging / 'manifest.json', executable=executable)
    files = _load_manifest(raw)
    present = {path.name for path in staging.iterdir()}
    if present != set(REQUIRED) | {'manifest.json'}:
        raise BundleError('unexpected_member')
    for name, digest in files.items():
        _verify_member(staging / name, digest, executable=executable)
    return {'verified': True, 'installable': False, 'manifest_sha256': hashlib.sha256(raw).hexdigest()}
