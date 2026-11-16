#!/usr/bin/env python3
"""Shared closed-document checks for Release and Route B admission.

Extracted so both callers refuse duplicate keys, a replaced package and an
expired approval the same way. This module does not load a package or call a
manager.
"""
import hashlib
import json
from datetime import datetime

from upgrade_backup_contract import ContractError, require


def object_with_unique_keys(raw):
    require(type(raw) is bytes and 0 < len(raw) <= 1024 * 1024, 'json_budget')

    def pairs(items):
        result = {}
        for key, value in items:
            if key in result:
                raise ContractError('duplicate_key')
            result[key] = value
        return result

    def constant(_):
        raise ContractError('json_constant_invalid')

    try:
        value = json.loads(raw.decode('utf-8'), object_pairs_hook=pairs, parse_constant=constant)
    except ContractError:
        raise
    except (UnicodeError, ValueError, RecursionError):
        raise ContractError('json_invalid') from None
    require(type(value) is dict, 'json_invalid')
    return value


def package_digest(raw):
    require(type(raw) is bytes, 'package_invalid')
    return hashlib.sha256(raw).hexdigest()


def require_unexpired(issued_at, expires_at, now):
    require(isinstance(now, datetime) and now.tzinfo is not None, 'time_invalid')
    issued = datetime.fromisoformat(issued_at)
    expires = datetime.fromisoformat(expires_at)
    require(issued.tzinfo is not None and expires.tzinfo is not None, 'time_invalid')
    if now >= expires or now < issued:
        raise ContractError('approval_expired')
