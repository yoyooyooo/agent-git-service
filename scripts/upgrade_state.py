#!/usr/bin/env python3
"""Private file journal for upgrade control state. Not the business database.

The published event file is the only commit point. A staging file, a lagging
projection and an approval file never authorize an effect by themselves.
Same-UID or root replacement of the whole tree is outside this trust boundary.
"""
import sys
sys.dont_write_bytecode = True
sys.pycache_prefix = None
from datetime import datetime, timezone
import fcntl
import hashlib
import json
import os
from pathlib import Path
import stat
import uuid

from upgrade import Failure, directory_fd
from upgrade_backup_contract import canonical, digest, identifier, require

FORMAT = 'ags.upgrade-state-format.v1'
EVENT = 'ags.upgrade-event.v1'
PROJECTION = 'ags.upgrade-state.v1'
AUTHORITY = 'ags.runtime-authority.v2'
MAX_EVENT = 65536
MAX_EVENTS = 4096
MIN_FREE = 8 * 1024 * 1024
EVENT_FIELDS = (
    'schema', 'sequence', 'previous_sha256', 'event_id', 'operation_id', 'kind',
    'host_instance', 'identity_sha256', 'expected_epoch', 'epoch', 'transaction_id',
    'binding_sha256', 'before_phase', 'after_phase', 'approval_sha256',
    'evidence_sha256', 'effect', 'recorded_at')
EFFECT_FIELDS = ('effect_id', 'type', 'expected_before', 'desired_after', 'status')
NULLABLE = {'expected_epoch', 'epoch', 'transaction_id', 'approval_sha256',
            'evidence_sha256', 'effect'}


class StateError(Failure):
    def __init__(self, code, exit_code=82):
        super().__init__(code, 'state', exit_code)


def _sha(raw):
    return hashlib.sha256(raw).hexdigest()


def _private_dir(path):
    path = Path(path)
    path.mkdir(mode=0o700, exist_ok=True)
    info = path.lstat()
    if (path.is_symlink() or not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid()
            or info.st_mode & 0o077):
        raise StateError('state_path_unsafe')
    return path


def _journal_paths(root):
    state = Path(root) / 'state'
    base = state / 'ags-autodeploy/local-v2'
    control = base / 'control'
    return state, base, control, control / 'events'


def layout(root):
    """Read an existing journal. Missing directories are not a journal and are not created."""
    state, base, control, events = _journal_paths(root)
    if not control.exists() and not control.is_symlink():
        return base, control, events
    _private_dir(state)
    _private_dir(state / 'ags-autodeploy')
    _private_dir(base)
    _private_dir(control)
    if events.exists() or events.is_symlink():
        _private_dir(events)
    for name in ('approvals', 'evidence'):
        path = control / name
        if path.exists() or path.is_symlink():
            _private_dir(path)
    return base, control, events


def ensure_layout(root):
    """Create the journal directories for a commit. Readers must use layout."""
    state, base, control, events = _journal_paths(root)
    _private_dir(state)
    _private_dir(state / 'ags-autodeploy')
    _private_dir(base)
    _private_dir(control)
    _private_dir(events)
    _private_dir(control / 'approvals')
    _private_dir(control / 'evidence')
    return base, control, events


def _reject_name(name):
    if not isinstance(name, str) or name in ('.', '..') or '/' in name or name.startswith('.'):
        raise StateError('state_name_rejected')
    return name


def _fsync_dir(path):
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def _write_exclusive(directory, name, raw, mode=0o600):
    """Publish one new file. An existing name is a conflict, never an overwrite."""
    _reject_name(name)
    directory = Path(directory)
    info = os.statvfs(directory)
    if info.f_bavail * info.f_frsize < MIN_FREE + len(raw):
        raise StateError('state_space_insufficient', 83)
    staging = '.' + uuid.uuid4().hex + '.' + name + '.new'
    fd = None
    published = False
    try:
        with directory_fd(directory, private=True) as parent:
            try:
                os.lstat(name, dir_fd=parent)
            except FileNotFoundError:
                pass
            else:
                raise StateError('state_already_published')
            try:
                fd = os.open(staging, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                             mode, dir_fd=parent)
                written = os.write(fd, raw)
                if written != len(raw):
                    raise StateError('state_write_incomplete')
                os.fsync(fd)
                os.close(fd)
                fd = None
                os.link(staging, name, src_dir_fd=parent, dst_dir_fd=parent)
                published = True
                os.unlink(staging, dir_fd=parent)
                os.fsync(parent)
            except BaseException:
                if not published:
                    try:
                        os.unlink(staging, dir_fd=parent)
                    except OSError:
                        pass
                raise
            finally:
                if fd is not None:
                    os.close(fd)
    except FileExistsError as error:
        raise StateError('state_already_published') from error
    except OSError as error:
        if getattr(error, 'errno', None) in (28, 122):
            raise StateError('state_write_failed') from error
        raise StateError('state_write_failed') from error


def _read_file(path, maximum=MAX_EVENT):
    path = Path(path)
    if path.is_symlink():
        raise StateError('state_path_unsafe')
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        info = os.fstat(fd)
        if (not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_uid != os.getuid()
                or stat.S_IMODE(info.st_mode) != 0o600 or info.st_size > maximum or info.st_size == 0):
            raise StateError('state_path_unsafe')
        raw = os.read(fd, maximum + 1)
        if len(raw) != info.st_size:
            raise StateError('state_truncated')
        return raw
    finally:
        os.close(fd)


def _object(raw):
    def unique(pairs):
        seen = {}
        for key, value in pairs:
            if key in seen:
                raise StateError('state_duplicate_key')
            seen[key] = value
        return seen
    try:
        value = json.loads(raw, object_pairs_hook=unique)
    except (ValueError, StateError) as error:
        if isinstance(error, StateError):
            raise
        raise StateError('state_json_invalid') from error
    if type(value) is not dict:
        raise StateError('state_json_invalid')
    return value


def _check_event(value, previous):
    if set(value) != set(EVENT_FIELDS) or value.get('schema') != EVENT:
        raise StateError('state_schema_invalid')
    sequence = value['sequence']
    if type(sequence) is not int or not 1 <= sequence <= MAX_EVENTS:
        raise StateError('state_sequence_invalid')
    if previous is None:
        if sequence != 1 or value['previous_sha256'] is not None:
            raise StateError('state_chain_broken')
    else:
        if sequence != previous['sequence'] + 1 or value['previous_sha256'] != previous['sha256']:
            raise StateError('state_chain_broken')
    if (previous is not None and value['expected_epoch'] is not None
            and previous['epoch'] is not None and value['expected_epoch'] != previous['epoch']):
        raise StateError('state_epoch_conflict')
    if previous is not None and value['host_instance'] != previous['host_instance']:
        raise StateError('state_host_mismatch')
    if value['epoch'] is not None and previous is not None and previous['epoch'] is not None:
        if value['epoch'] < previous['epoch']:
            raise StateError('state_epoch_conflict')
    for name in ('event_id', 'operation_id', 'kind', 'host_instance'):
        identifier(value[name])
    digest(value['identity_sha256'])
    digest(value['binding_sha256'])
    for name in ('before_phase', 'after_phase', 'recorded_at'):
        if type(value[name]) is not str or not value[name] or len(value[name]) > 128:
            raise StateError('state_field_invalid')
    for name in NULLABLE:
        if value[name] is not None and name.endswith('sha256'):
            digest(value[name])
    effect = value['effect']
    if effect is not None:
        if type(effect) is not dict or set(effect) != set(EFFECT_FIELDS):
            raise StateError('state_effect_invalid')
        identifier(effect['effect_id'])
        identifier(effect['type'])
        if effect['status'] not in ('intent', 'observed', 'committed', 'blocked'):
            raise StateError('state_effect_invalid')
    return value


def _scan(events):
    names = sorted(p.name for p in events.iterdir() if not p.name.startswith('.'))
    rows = []
    previous = None
    for index, name in enumerate(names, 1):
        expected = f'{index:020d}.json'
        if name != expected:
            raise StateError('state_chain_hole')
        raw = _read_file(events / name)
        value = _object(raw)
        _check_event(value, previous)
        value = dict(value)
        value['sha256'] = _sha(raw)
        rows.append(value)
        previous = value
    return rows


def load_verified(root):
    """Accept only a complete continuous chain. Never pick the newest-looking file."""
    base, control, events = layout(root)
    format_path = control / 'format.json'
    if not format_path.exists():
        if events.exists() and any(events.iterdir()):
            raise StateError('state_format_missing')
        return {'schema': FORMAT, 'events': [], 'projection': None, 'root': str(base)}
    header = _object(_read_file(format_path))
    if header.get('schema') != FORMAT or header.get('min_reader') != 1:
        raise StateError('state_schema_invalid')
    host = header.get('host_instance')
    if type(host) is not str or not host:
        raise StateError('state_schema_invalid')
    if header.get('host_instance_sha256') != _sha(host.encode()):
        raise StateError('state_host_mismatch')
    rows = _scan(events)
    for row in rows:
        if row['host_instance'] != host or _sha(row['host_instance'].encode()) != header.get('host_instance_sha256'):
            raise StateError('state_host_mismatch')
    return {'schema': FORMAT, 'format': header, 'events': rows,
            'projection': _projection(rows), 'root': str(base)}


def _projection(rows):
    current = {'schema': PROJECTION, 'sequence': 0, 'event_sha256': None, 'epoch': None,
               'authority': None, 'pending_effects': [], 'generation': None, 'phase': None}
    for row in rows:
        current['sequence'] = row['sequence']
        current['event_sha256'] = row['sha256']
        current['epoch'] = row['epoch']
        current['phase'] = row['after_phase']
        effect = row['effect']
        if effect is not None and effect['status'] == 'intent':
            current['pending_effects'].append(effect['effect_id'])
        if effect is not None and effect['status'] in ('observed', 'committed', 'blocked'):
            current['pending_effects'] = [item for item in current['pending_effects']
                                           if item != effect['effect_id']]
        if row['kind'] == 'authority_activated':
            current['authority'] = {'schema': AUTHORITY, 'state': 'active', 'epoch': row['epoch'],
                                    'approval_sha256': row['approval_sha256'],
                                    'binding_sha256': row['binding_sha256']}
        if row['kind'] == 'authority_suspended':
            if current['authority'] is not None:
                current['authority'] = {**current['authority'], 'state': 'suspended'}
        if row['kind'] in ('generation_spawned', 'start_observed') and effect and effect['status'] == 'observed':
            current['generation'] = effect['desired_after']
    return current


def commit_event(root, event):
    """Publish one event under StateLock. No effect runs here."""
    with StateLock(root):
        return _commit_event_locked(root, event)


def _commit_event_locked(root, event):
    loaded = load_verified(root)
    rows = loaded['events']
    previous = rows[-1] if rows else None
    value = dict(event)
    value['schema'] = EVENT
    value['sequence'] = (previous['sequence'] + 1) if previous else 1
    value['previous_sha256'] = previous['sha256'] if previous else None
    _check_event(value, previous)
    for row in rows:
        if row['operation_id'] == value['operation_id']:
            comparable = {key: row[key] for key in EVENT_FIELDS if key not in ('sequence', 'previous_sha256', 'recorded_at')}
            requested = {key: value[key] for key in EVENT_FIELDS if key not in ('sequence', 'previous_sha256', 'recorded_at')}
            if comparable == requested:
                return {'sequence': row['sequence'], 'sha256': row['sha256'],
                        'projection': loaded['projection']}
            raise StateError('state_operation_conflict')
        if row['event_id'] == value['event_id']:
            raise StateError('state_duplicate_event')
    raw = canonical(value)
    if len(raw) > MAX_EVENT:
        raise StateError('state_event_too_large')
    base, control, events = ensure_layout(root)
    if not (control / 'format.json').exists():
        host = value['host_instance']
        header = canonical({'schema': FORMAT, 'min_reader': 1, 'host_instance': host,
                            'host_instance_sha256': _sha(host.encode())})
        _write_exclusive(control, 'format.json', header)
    _write_exclusive(events, f'{value["sequence"]:020d}.json', raw)
    published = _scan(events)
    projected = canonical(_projection(published))
    current = control / 'current.json'
    if current.exists() or current.is_symlink():
        _replace(control, 'current.json', projected)
    else:
        _write_exclusive(control, 'current.json', projected)
    return {'sequence': value['sequence'], 'sha256': _sha(raw), 'projection': _object(projected)}


def _replace(directory, name, raw):
    """Replace a projection only. Event sequence names never use this path."""
    _reject_name(name)
    if name[:1].isdigit():
        raise StateError('state_already_published')
    with directory_fd(directory, private=True) as parent:
        staging = '.' + uuid.uuid4().hex + '.' + name + '.replace'
        fd = os.open(staging, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=parent)
        try:
            if os.write(fd, raw) != len(raw):
                raise StateError('state_write_incomplete')
            os.fsync(fd)
        finally:
            os.close(fd)
        os.replace(staging, name, src_dir_fd=parent, dst_dir_fd=parent)
        os.fsync(parent)


def project(root):
    """Rebuild current.json from the chain. A lagging projection grants nothing."""
    loaded = load_verified(root)
    if not loaded['events']:
        raise StateError('state_empty')
    raw = canonical(loaded['projection'])
    _, control, _ = layout(root)
    current = control / 'current.json'
    if current.exists() and not current.is_symlink():
        try:
            if _read_file(current) == raw:
                return loaded['projection']
        except StateError:
            pass
        _replace(control, 'current.json', raw)
    else:
        _write_exclusive(control, 'current.json', raw)
    return loaded['projection']


def recover_local(root):
    """Local projection repair only. No service, network or business effect."""
    loaded = load_verified(root)
    projected = project(root) if loaded['events'] else None
    staging = [p.name for p in (Path(root) / 'state/ags-autodeploy/local-v2/control').rglob('.*')
               if p.is_file() and p.name.endswith('.new')]
    return {'schema': 'ags.upgrade-state-recovery.v1', 'effects': [],
            'sequence': 0 if projected is None else projected['sequence'],
            'ignored_staging': sorted(staging), 'projection': projected}


def commit_phase(root, operation_id, before_phase, after_phase, *, kind=None,
                 host_instance=None, identity_sha256=None,
                 epoch=1, expected_epoch=None, transaction_id=None, binding_sha256=None,
                 effect=None, approval_sha256=None, evidence_sha256=None):
    """Record a phase or an effect observation without performing the effect.

    The first event must carry a real host and identity. Later events inherit
    them from the verified chain. host-1 and a zero digest are not defaults.
    """
    if kind is None:
        kind = operation_id
    if host_instance is None or identity_sha256 is None or binding_sha256 is None:
        loaded = load_verified(root)
        if not loaded['events']:
            raise StateError('state_host_mismatch')
        previous = loaded['events'][-1]
        host_instance = host_instance or previous['host_instance']
        identity_sha256 = identity_sha256 or previous['identity_sha256']
        binding_sha256 = binding_sha256 or previous['binding_sha256']
    if not host_instance or identity_sha256 in (None, '0' * 64) or binding_sha256 in (None, '0' * 64):
        raise StateError('state_host_mismatch')
    event_id = f'{operation_id}-{after_phase}'
    return commit_event(root, {
        'event_id': event_id, 'operation_id': operation_id, 'kind': kind,
        'host_instance': host_instance, 'identity_sha256': identity_sha256,
        'expected_epoch': expected_epoch, 'epoch': epoch, 'transaction_id': transaction_id,
        'binding_sha256': binding_sha256, 'before_phase': before_phase,
        'after_phase': after_phase, 'approval_sha256': approval_sha256,
        'evidence_sha256': evidence_sha256, 'effect': effect,
        'recorded_at': datetime.now(timezone.utc).strftime('%Y-%m-%dT%H:%M:%S+00:00')})


class StateLock:
    """Short commit serialization. Never call a manager or take another lock while held."""

    def __init__(self, root):
        self.root = Path(root)
        self.fd = None

    def __enter__(self):
        base, _, _ = ensure_layout(self.root)
        fd = os.open(base / 'state.lock', os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_uid != os.getuid() or info.st_mode & 0o077:
            os.close(fd)
            raise StateError('state_lock_unsafe')
        fcntl.flock(fd, fcntl.LOCK_EX)
        self.fd = fd
        return self

    def __exit__(self, *_):
        if self.fd is not None:
            os.close(self.fd)
            self.fd = None
