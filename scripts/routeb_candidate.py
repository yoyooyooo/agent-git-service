#!/usr/bin/env python3
"""Closed Route B candidate identity. Reads a reviewed manifest; starts nothing.

A relative binary is resolved only against the manifest parent. The rehearsal
schema and history flags are not authority. This module does not install.
"""
import sys
sys.dont_write_bytecode = True
sys.pycache_prefix = None
import hashlib
import json
import os
import stat
from dataclasses import dataclass
from pathlib import Path

from upgrade_backup_contract import (ContractError, absolute, digest, fields, identifier,
                                    relative, require, strict_json, text)


SCHEMA = 'ags.routeb-candidate-manifest.v1'
FIELDS = ('schema', 'host', 'platform', 'binary', 'sha256', 'built_revision', 'built_tree',
          'baseline_revision', 'baseline_tree_documented', 'version', 'toolchain_requested',
          'host_go_version', 'cgo_requested', 'go_build_settings', 'not_a_release',
          'not_installed', 'started_against_ags_home')
VERSION_FIELDS = ('schema', 'command', 'version', 'revision', 'tree', 'goos', 'goarch', 'go_version')
OID = __import__('re').compile(r'[0-9a-f]{40}')


class CandidateError(ContractError):
    def __init__(self, reason):
        super().__init__(reason)
        self.code = reason


def _object(raw):
    require(type(raw) is bytes and 0 < len(raw) <= 1024 * 1024, 'json_budget')

    def pairs(items):
        result = {}
        for key, item in items:
            if key in result:
                raise CandidateError('duplicate_key')
            result[key] = item
        return result

    def constant(_):
        raise CandidateError('json_constant_invalid')

    try:
        return json.loads(raw.decode('utf-8'), object_pairs_hook=pairs, parse_constant=constant)
    except CandidateError:
        raise
    except (UnicodeError, ValueError, RecursionError):
        raise CandidateError('json_invalid') from None


def _oid(value):
    require(type(value) is str and OID.fullmatch(value) is not None and value != '0' * 40,
            'revision_invalid')


def _contained(parent, candidate):
    try:
        resolved = candidate.resolve(strict=True)
        resolved.relative_to(parent.resolve(strict=True))
    except (OSError, ValueError):
        raise CandidateError('path_escape') from None
    return resolved


def load_candidate(manifest_path, expected_review):
    """Return a verified candidate bound to the raw manifest bytes and parent."""
    try:
        path = Path(manifest_path)
        raw = path.read_bytes()
        value = _object(raw)
        fields(value, FIELDS)
        require(value['schema'] == SCHEMA, 'schema_invalid')
        identifier(value['host'])
        choice_platform = value['platform']
        require(choice_platform in ('darwin/arm64', 'linux/amd64'), 'platform_invalid')
        digest(value['sha256'])
        for key in ('built_revision', 'built_tree', 'baseline_revision', 'baseline_tree_documented'):
            _oid(value[key])
        fields(value['version'], VERSION_FIELDS)
        require(value['version']['schema'] == 'ags.build.v1'
                and value['version']['command'] == 'gh-server', 'version_invalid')
        text(value['version']['version']); text(value['version']['go_version'])
        text(value['toolchain_requested']); text(value['host_go_version']); text(value['cgo_requested'])
        require(value['version']['goos'] in ('darwin', 'linux')
                and value['version']['goarch'] in ('arm64', 'amd64'), 'version_invalid')
        require(value['platform'] == value['version']['goos'] + '/' + value['version']['goarch'],
                'platform_mismatch')
        require(type(value['go_build_settings']) is dict and not value['go_build_settings'],
                'build_settings_unreviewed')
        require(value['not_a_release'] is True and value['not_installed'] is True
                and value['started_against_ags_home'] is False, 'not_a_release_required')
        fields(expected_review, ('revision', 'tree'))
        _oid(expected_review['revision']); _oid(expected_review['tree'])
        require(value['built_revision'] == expected_review['revision']
                and value['built_tree'] == expected_review['tree'], 'review_mismatch')
        require(value['version']['revision'] == value['built_revision']
                and value['version']['tree'] == value['built_tree']
                and value['version']['version'] == 'source-' + value['built_revision'],
                'identity_mismatch')
        binary = value['binary']
        text(binary)
        if binary.startswith('/'):
            absolute(binary)
            candidate = Path(binary)
        else:
            relative(binary)
            candidate = path.parent / binary
        info = os.lstat(candidate)
        require(stat.S_ISREG(info.st_mode) and not stat.S_ISLNK(info.st_mode) and info.st_nlink == 1,
                'binary_type_rejected')
        resolved = _contained(path.parent, candidate)
        require(info.st_uid == os.getuid(), 'binary_owner_rejected')
        body = resolved.read_bytes()
        require(hashlib.sha256(body).hexdigest() == value['sha256'], 'binary_digest_mismatch')
        after = resolved.lstat()
        require((after.st_ino, after.st_dev, after.st_size, after.st_mtime_ns) ==
                (info.st_ino, info.st_dev, info.st_size, info.st_mtime_ns), 'binary_changed')
        return VerifiedCandidate(raw_manifest_sha256=hashlib.sha256(raw).hexdigest(),
                                 manifest_path=path.resolve(), binary_path=resolved,
                                 sha256=value['sha256'], revision=value['built_revision'],
                                 tree=value['built_tree'], host=value['host'],
                                 platform=value['platform'], value=value)
    except CandidateError:
        raise
    except ContractError as error:
        raise CandidateError(str(error)) from None
    except OSError:
        raise CandidateError('candidate_unreadable') from None


@dataclass(frozen=True)
class VerifiedCandidate:
    raw_manifest_sha256: str
    manifest_path: Path
    binary_path: Path
    sha256: str
    revision: str
    tree: str
    host: str
    platform: str
    value: dict
