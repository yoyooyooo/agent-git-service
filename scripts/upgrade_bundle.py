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
out. The destination file is created exclusively and is not truncated or
followed. Member mode 0644 is fchmod on that same descriptor. A path that
no longer names it is destination_changed. This does not prove a Git
revision and does not make the bundle installable.

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

Verification first lstats the final component of the caller path and does not
follow that component. A symlink or other non-directory is
staging_type_rejected. A missing path stays manifest_missing. It then holds
one directory descriptor opened O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_NONBLOCK.
Listing, the manifest, and every required member use that descriptor and a
single basename; they do not reopen the caller path. Before success, the
descriptor and the caller path must still name that same directory object.
A replaced or removed root is staging_changed. Directory I/O or a missing
directory, no-follow, or listing capability is staging_unreadable. The
directory descriptor is closed once. This pins the directory used for lookup.
It does not freeze members, reject an ancestor symlink, or make the bundle
installable.

Each manifest or member descriptor is closed once. That close is not retried,
including after EINTR, and it is not attempted a second time. The read is
finished only when the close succeeds. A close OSError after the read itself
succeeded is manifest_unreadable or member_unreadable: exit 69, and the
message is only that code. The manifest is not parsed and later members are
not opened. A close OSError does not replace an exception this read already
raised. An exception the caller is already handling is not this read's
failure. One close attempt does not prove that every OS released the
descriptor after an unknown close error.
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

_OS_OPEN = os.open
_OS_STAT = os.stat
_OS_LISTDIR = os.listdir
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
    manifest = {'schema': SCHEMA, 'entry': ENTRY, 'files': files}
    raw = json.dumps(manifest, sort_keys=True, separators=(',', ':')).encode()
    _write_exclusive(staging / 'manifest.json', raw, 0o666, None)
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


def _open_destination(path, mode):
    """Create the final component. Never truncate, follow, or reuse an existing object."""
    nofollow = getattr(os, 'O_NOFOLLOW', None)
    nonblock = getattr(os, 'O_NONBLOCK', None)
    if nofollow is None or nonblock is None or not hasattr(os, 'fchmod'):
        raise OSError(errno.ENOTSUP, 'destination creation unavailable')
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | nofollow | nonblock
    while True:
        try:
            return os.open(path, flags, mode)
        except OSError as exc:
            if exc.errno == errno.EINTR:
                continue
            raise


def _destination_fstat(fd):
    """fstat one output descriptor. Another errno stays a destination OSError."""
    while True:
        try:
            return os.fstat(fd)
        except OSError as exc:
            if exc.errno == errno.EINTR:
                continue
            raise


def _destination_fchmod(fd, mode):
    """Set mode on the held descriptor. Do not chmod the path."""
    while True:
        try:
            os.fchmod(fd, mode)
            return
        except OSError as exc:
            if exc.errno == errno.EINTR:
                continue
            raise


def _destination_owner(info):
    """Identity that our own write and fchmod do not change."""
    return (info.st_dev, info.st_ino, stat.S_IFMT(info.st_mode), info.st_uid)


def _destination_snapshot(info):
    """Metadata after our own writes. atime is excluded because a read may change it."""
    return (info.st_dev, info.st_ino, info.st_mode, info.st_uid, info.st_size,
            info.st_mtime_ns, info.st_ctime_ns)


def _require_new_regular(info):
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
        raise BundleError('destination_changed')


def _bind_destination(fd, path, created, nbytes):
    """The final path must still name this regular descriptor. Ignore atime."""
    _require_new_regular(created)
    finished = _destination_fstat(fd)
    _require_new_regular(finished)
    if _destination_owner(finished) != _destination_owner(created) or finished.st_size != nbytes:
        raise BundleError('destination_changed')
    try:
        current = _lstat_source(path)
    except OSError as exc:
        if exc.errno in (errno.ENOENT, errno.ENOTDIR):
            raise BundleError('destination_changed') from None
        raise
    if (stat.S_ISLNK(current.st_mode) or not stat.S_ISREG(current.st_mode)
            or _destination_snapshot(current) != _destination_snapshot(finished)):
        raise BundleError('destination_changed')
    again = _destination_fstat(fd)
    if _destination_snapshot(again) != _destination_snapshot(current):
        raise BundleError('destination_changed')


def _close_output_keep(fd):
    """Close once after this output already failed. Keep the earlier error."""
    try:
        os.close(fd)
    except OSError:
        pass


def _write_exclusive(path, data, mode, chmod_mode):
    """Create, write, and close one output. chmod_mode None keeps the creation mode."""
    fd = _open_destination(path, mode)
    try:
        created = _destination_fstat(fd)
        _require_new_regular(created)
        _write_dest(fd, data)
        if chmod_mode is not None:
            _destination_fchmod(fd, chmod_mode)
        _bind_destination(fd, path, created, len(data))
    except BaseException:
        _close_output_keep(fd)
        raise
    try:
        os.close(fd)
    except OSError:
        raise


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


def _stream_source(fd, dest, size):
    """Copy and hash one descriptor. The extra probe byte is not written."""
    digest = hashlib.sha256()
    remaining = size
    out = _open_destination(dest, 0o644)
    try:
        created = _destination_fstat(out)
        _require_new_regular(created)
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
        _destination_fchmod(out, 0o644)
        _bind_destination(out, dest, created, size)
    except BaseException:
        # This copy already failed. Close once and keep that error.
        # Do not close again: close leaves the descriptor state unspecified.
        _close_output_keep(out)
        raise
    # A caller's handled exception is not this copy's failure.
    # Close once, including after EINTR. Do not retry or close again.
    try:
        os.close(out)
    except OSError:
        raise
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
        result = digest
    except BaseException:
        # This copy already failed. Close the source once and keep that error.
        try:
            os.close(fd)
        except OSError:
            pass
        raise
    try:
        os.close(fd)
    except OSError as exc:
        _source_io_error(exc, started=True)
    return result


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


def _directory_entry_name(name):
    """One basename. A manifest name cannot escape the held directory."""
    if (type(name) is not str or not name or name in ('.', '..')
            or '/' in name or '\\' in name or name != Path(name).name):
        raise BundleError('unexpected_member')
    return name


def _stat_directory_entry(name, dir_fd):
    """lstat one basename in the held directory. Do not follow and do not use a path."""
    _directory_entry_name(name)
    while True:
        try:
            return os.stat(name, dir_fd=dir_fd, follow_symlinks=False)
        except NotImplementedError:
            raise BundleError('staging_unreadable') from None
        except OSError as exc:
            if exc.errno == errno.EINTR:
                continue
            raise


def _open_manifest(name, dir_fd):
    """Open the final component without following it. Never fall back to a following open."""
    _directory_entry_name(name)
    nofollow = getattr(os, 'O_NOFOLLOW', None)
    nonblock = getattr(os, 'O_NONBLOCK', None)
    if nofollow is None or nonblock is None:
        raise BundleError('manifest_type_rejected')
    flags = os.O_RDONLY | nofollow | nonblock
    while True:
        try:
            return os.open(name, flags, dir_fd=dir_fd)
        except OSError as exc:
            if exc.errno == errno.EINTR:
                continue
            _manifest_access_error(exc)


def _read_manifest_bytes(name, *, executable, dir_fd):
    """Read one no-follow descriptor, capped at one byte past the budget.

    Close that descriptor once. A close OSError after this read succeeded is
    manifest_unreadable and the bytes are not parsed. A close OSError after
    this read already failed is discarded. Close is not retried.
    """
    try:
        info = _stat_directory_entry(name, dir_fd)
    except OSError as exc:
        _manifest_access_error(exc)
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
        raise BundleError('manifest_type_rejected')
    fd = _open_manifest(name, dir_fd)
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
                current = _stat_directory_entry(name, dir_fd)
            except OSError:
                raise BundleError('manifest_changed') from None
            if _manifest_identity(current) != _manifest_identity(after):
                raise BundleError('manifest_changed')
            if len(raw) > _MANIFEST_BUDGET:
                raise BundleError('manifest_budget_exceeded')
            result = raw
        except BundleError:
            raise
        except OSError as exc:
            _manifest_access_error(exc)
    except BaseException:
        # This read already failed. Close once and keep that error.
        try:
            os.close(fd)
        except OSError:
            pass
        raise
    # A caller's handled exception is not this read's failure.
    # Close once, including after EINTR. Do not retry or close again.
    try:
        os.close(fd)
    except OSError:
        raise BundleError('manifest_unreadable') from None
    return result


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


def _open_member(name, dir_fd):
    """Open the final component without following it. Never fall back to a following open."""
    _directory_entry_name(name)
    nofollow = getattr(os, 'O_NOFOLLOW', None)
    nonblock = getattr(os, 'O_NONBLOCK', None)
    if nofollow is None or nonblock is None:
        raise BundleError('member_type_rejected')
    flags = os.O_RDONLY | nofollow | nonblock
    while True:
        try:
            return os.open(name, flags, dir_fd=dir_fd)
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


def _verify_member(name, digest, *, executable, dir_fd):
    """Check one member and hash it from the same no-follow descriptor.

    Close that descriptor once. A close OSError after the checks succeeded is
    member_unreadable and stops later members. A close OSError after this
    member already failed is discarded. Close is not retried.
    """
    try:
        info = _stat_directory_entry(name, dir_fd)
    except OSError as exc:
        _member_access_error(exc)
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
        raise BundleError('member_type_rejected')
    fd = _open_member(name, dir_fd)
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
            current = _stat_directory_entry(name, dir_fd)
        except OSError:
            raise BundleError('member_changed') from None
        if _manifest_identity(current) != _manifest_identity(after):
            raise BundleError('member_changed')
        if actual != digest:
            raise BundleError('digest_rejected')
    except BaseException:
        # This member already failed. Close once and keep that error.
        try:
            os.close(fd)
        except OSError:
            pass
        raise
    # A caller's handled exception is not this member's failure.
    # Close once, including after EINTR. Do not retry or close again.
    try:
        os.close(fd)
    except OSError:
        raise BundleError('member_unreadable') from None


def _directory_object(info):
    """Directory identity. Size and timestamps are not part of this contract."""
    return (info.st_dev, info.st_ino, stat.S_IFMT(info.st_mode))


def _lstat_call(path):
    while True:
        try:
            return os.lstat(path)
        except OSError as exc:
            if exc.errno == errno.EINTR:
                continue
            raise


def _staging_directory_flags():
    """Flags for one no-follow directory open. None means the platform cannot do it.

    Capability is the import-time os functions. A later test seam may replace
    os.open without making a capable platform look incapable.
    """
    nofollow = getattr(os, 'O_NOFOLLOW', None)
    nonblock = getattr(os, 'O_NONBLOCK', None)
    directory = getattr(os, 'O_DIRECTORY', None)
    if nofollow is None or nonblock is None or directory is None:
        return None
    dir_fds = getattr(os, 'supports_dir_fd', ())
    fd_paths = getattr(os, 'supports_fd', ())
    followers = getattr(os, 'supports_follow_symlinks', ())
    if (_OS_STAT not in dir_fds or _OS_OPEN not in dir_fds or _OS_LISTDIR not in fd_paths
            or _OS_STAT not in followers):
        return None
    return os.O_RDONLY | directory | nofollow | nonblock


def _close_directory_keep(fd):
    """Close a directory descriptor once after this verify already failed."""
    try:
        os.close(fd)
    except OSError:
        pass


def _close_directory_success(fd):
    """Close once. EINTR is not retried and does not become success."""
    try:
        os.close(fd)
    except OSError:
        raise BundleError('staging_unreadable') from None


def _fstat_directory(fd):
    while True:
        try:
            return os.fstat(fd)
        except OSError as exc:
            if exc.errno == errno.EINTR:
                continue
            raise BundleError('staging_unreadable') from None


def _require_held_directory(info, identity):
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
        raise BundleError('staging_changed')
    if _directory_object(info) != identity:
        raise BundleError('staging_changed')


def _recheck_staging(fd, staging, identity):
    """The held descriptor and the caller path must still be the initial directory."""
    _require_held_directory(_fstat_directory(fd), identity)
    try:
        current = _lstat_call(staging)
    except OSError as exc:
        if exc.errno in (errno.ENOENT, errno.ENOTDIR, errno.ELOOP, errno.ENXIO):
            raise BundleError('staging_changed') from None
        raise BundleError('staging_unreadable') from None
    _require_held_directory(current, identity)


def _list_staging(dir_fd):
    while True:
        try:
            names = os.listdir(dir_fd)
            break
        except TypeError:
            raise BundleError('staging_unreadable') from None
        except OSError as exc:
            if exc.errno == errno.EINTR:
                continue
            raise BundleError('staging_unreadable') from None
    if set(names) != set(REQUIRED) | {'manifest.json'}:
        raise BundleError('unexpected_member')


def _open_staging_directory(staging):
    """Bind the final caller component. Do not resolve a symlink away first."""
    try:
        info = _lstat_call(staging)
    except OSError as exc:
        if exc.errno in (errno.ENOENT, errno.ENOTDIR):
            raise BundleError('manifest_missing') from None
        raise BundleError('staging_unreadable') from None
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
        raise BundleError('staging_type_rejected')
    flags = _staging_directory_flags()
    if flags is None:
        raise BundleError('staging_unreadable')
    while True:
        try:
            fd = os.open(os.fspath(staging), flags)
            break
        except OSError as exc:
            if exc.errno == errno.EINTR:
                continue
            if exc.errno in (errno.ENOENT, errno.ENOTDIR, errno.ELOOP, errno.ENXIO, errno.EISDIR):
                raise BundleError('staging_changed') from None
            raise BundleError('staging_unreadable') from None
    try:
        _recheck_staging(fd, staging, _directory_object(info))
    except BaseException:
        _close_directory_keep(fd)
        raise
    return fd, _directory_object(info)


def verify(staging, *, executable=True):
    """Verify one held staging directory. Success is still not installable."""
    staging = Path(staging)
    fd, identity = _open_staging_directory(staging)
    try:
        raw = _read_manifest_bytes('manifest.json', executable=executable, dir_fd=fd)
        _recheck_staging(fd, staging, identity)
        files = _load_manifest(raw)
        _recheck_staging(fd, staging, identity)
        _list_staging(fd)
        _recheck_staging(fd, staging, identity)
        for name, digest in files.items():
            _recheck_staging(fd, staging, identity)
            _verify_member(name, digest, executable=executable, dir_fd=fd)
        _recheck_staging(fd, staging, identity)
        result = {
            'verified': True, 'installable': False,
            'manifest_sha256': hashlib.sha256(raw).hexdigest(),
        }
    except BaseException:
        _close_directory_keep(fd)
        raise
    _close_directory_success(fd)
    return result
