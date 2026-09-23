#!/usr/bin/env python3
"""Own only URL-scoped Git resolution settings. Never rewrite remotes or credentials.

Install/remove are explicit per-user machine operations, not workload commands.
State records only the owned keys and their previous values, never a whole Git
configuration or credential. Connection fallback is NOT HTTP/write replay.
"""
from __future__ import annotations
import argparse
import ipaddress
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import urllib.parse

class Failure(Exception): pass

def git(*args: str, optional: bool = False) -> str:
    result = subprocess.run(['/usr/bin/git', 'config', '--global', *args], text=True, capture_output=True, timeout=15)
    if result.returncode and not (optional and result.returncode in (1, 5)):
        raise Failure('Git configuration operation failed; existing unrelated settings are unchanged')
    return result.stdout.rstrip('\n')

def values(key: str) -> list[str]:
    result = git('--null', '--get-all', key, optional=True)
    return result.split('\0')[:-1] if result else []

def replace(key: str, entries: list[str]) -> None:
    git('--unset-all', key, optional=True)
    for value in entries: git('--add', key, value)

def state_file(name: str, create: bool = False) -> Path:
    root = Path.home() / '.local/state/ags-edge/routes'
    if root.is_symlink(): raise Failure('route state directory cannot be a symlink')
    if create:
        root.mkdir(parents=True, exist_ok=True, mode=0o700)
        root.chmod(0o700)
    return root / (name + '.json')

def read_state(path: Path) -> dict:
    if path.is_symlink() or not path.is_file() or path.stat().st_mode & 0o077:
        raise Failure('route receipt must be an owner-only regular file')
    data = json.loads(path.read_text())
    if not isinstance(data, dict) or set(data) != {'version', 'phase', 'desired', 'previous'}:
        raise Failure('invalid route receipt schema')
    if type(data.get('version')) is not int or data['version'] != 1:
        raise Failure('unsupported route receipt')
    if data['phase'] not in ('applying', 'installed', 'removed'):
        raise Failure('invalid route receipt phase')
    desired_keys, previous_keys = data['desired'], data['previous']
    if not isinstance(desired_keys, dict) or not isinstance(previous_keys, dict) or not desired_keys or set(desired_keys) != set(previous_keys):
        raise Failure('route receipt key sets do not match')
    for key in desired_keys:
        if not key.startswith('http.') or not key.endswith('/.curloptResolve'):
            raise Failure('route receipt contains an unowned Git key')
        origin = key[len('http.'):-len('.curloptResolve')]
        # Reuse the origin validator without performing Git or network I/O.
        if set(desired([origin], '192.0.2.1', '192.0.2.2')) != {key}:
            raise Failure('route receipt key is noncanonical')
        for entries in (desired_keys[key], previous_keys[key]):
            if not isinstance(entries, list) or not all(isinstance(v, str) and not any(c in v for c in '\x00\r\n') for v in entries):
                raise Failure('route receipt values are invalid')
        if len(desired_keys[key]) != 1:
            raise Failure('route receipt desired value is ambiguous')
    return data

def save(path: Path, value: dict) -> None:
    temp = path.with_suffix('.new')
    fd = os.open(temp, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        with os.fdopen(fd, 'w') as stream:
            json.dump(value, stream, indent=2); stream.write('\n'); stream.flush(); os.fsync(stream.fileno())
        os.replace(temp, path)
    finally:
        if temp.exists(): temp.unlink()

def desired(origins: list[str], edge: str, primary: str) -> dict[str, list[str]]:
    addresses = [str(ipaddress.ip_address(edge)), str(ipaddress.ip_address(primary))]
    if addresses[0] == addresses[1]: raise Failure('Edge and primary IP must differ')
    formatted = [f'[{a}]' if ':' in a else a for a in addresses]
    result = {}
    for value in origins:
        u = urllib.parse.urlsplit(value)
        if u.scheme not in ('http', 'https') or not u.hostname or u.username or u.password or u.query or u.fragment or u.path not in ('', '/') or not u.port:
            raise Failure('each origin must be an exact HTTP(S) host and explicit port, without credentials or path')
        if any(c in u.hostname for c in '*\\\r\n\t '): raise Failure('invalid origin hostname')
        origin = f'{u.scheme}://{u.netloc}/'
        key = 'http.' + origin + '.curloptResolve'
        if key in result: raise Failure('duplicate origin')
        result[key] = [f'{u.hostname}:{u.port}:' + ','.join(formatted)]
    if not result: raise Failure('at least one --origin is required')
    return result

def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=['plan', 'install', 'status', 'remove'])
    parser.add_argument('--name', default='primary')
    parser.add_argument('--origin', action='append', default=[])
    parser.add_argument('--edge-ip')
    parser.add_argument('--primary-ip')
    args = parser.parse_args()
    if not re.fullmatch('[a-z0-9-]{1,48}', args.name): raise Failure('invalid route name')
    if any(os.getenv(key) for key in ('MULTICA_TOKEN', 'GIT_CONFIG_GLOBAL', 'GIT_CONFIG_SYSTEM', 'GIT_CONFIG', 'GIT_CONFIG_COUNT')):
        raise Failure('machine route changes/inspection require an ordinary user environment without Git overrides or task credentials')
    target = None
    if args.command in ('plan', 'install'):
        target = desired(args.origin, args.edge_ip or '', args.primary_ip or '')
        if args.command == 'plan':
            print(json.dumps({'name': args.name, 'settings': target, 'applied': False})); return
    path = state_file(args.name, create=args.command == 'install')
    if args.command in ('status', 'remove'):
        state = read_state(path)
        matching = all(values(k) == v for k, v in state['desired'].items())
        if args.command == 'status':
            print(json.dumps({'name': args.name, 'configured': matching, 'phase': state['phase'], 'settings': {k: values(k) for k in state['desired']}, 'fallback': 'connection-only; never retry HTTP denials or uncertain writes'})); return
        if state['phase'] == 'removed':
            if not all(values(k) == v for k, v in state['previous'].items()): raise Failure('removed route has newer configuration changes')
            print(json.dumps({'name': args.name, 'removed': True, 'changed': False})); return
        if not matching: raise Failure('owned configuration drifted; refusing to overwrite newer edits')
        for key, old in state['previous'].items(): replace(key, old)
        state['phase'] = 'removed'; save(path, state)
        print(json.dumps({'name': args.name, 'removed': True, 'remote_changed': False})); return
    if path.exists():
        state = read_state(path)
        if state['phase'] == 'installed':
            if state['desired'] != target or not all(values(k) == v for k, v in target.items()): raise Failure('existing route differs; reconcile/remove before installing another route')
            print(json.dumps({'name': args.name, 'installed': True, 'changed': False})); return
        if state['phase'] != 'removed': raise Failure('interrupted installation: inspect receipt and reconcile first')
    state = {'version': 1, 'phase': 'applying', 'desired': target, 'previous': {k: values(k) for k in target}}
    save(path, state)  # Durable intent before modifying keys.
    try:
        for key, entries in target.items(): replace(key, entries)
        if not all(values(k) == v for k, v in target.items()): raise Failure('route readback mismatch')
    except Exception:
        for key, entries in state['previous'].items(): replace(key, entries)
        state['phase'] = 'removed'; save(path, state); raise
    state['phase'] = 'installed'; save(path, state)
    print(json.dumps({'name': args.name, 'installed': True, 'remote_changed': False, 'settings': target}))

if __name__ == '__main__':
    try: main()
    except (Failure, ValueError, OSError, KeyError, subprocess.TimeoutExpired) as error:
        print('edge-client-route: ' + (str(error) if isinstance(error, Failure) else type(error).__name__), file=sys.stderr)
        sys.exit(1)
