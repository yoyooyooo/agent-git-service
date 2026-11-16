#!/usr/bin/env python3
"""Append-only, human-carried handoff v2. No remote reads or approval creation.

The immutable v1 base is checked at its original receipt time, not restamped.
Freshness applies independently to the last readback. Same-UID/root forgery is
outside this declaration contract. Changed mini generation is still fenced.
"""
from pathlib import Path
from upgrade_backup_contract import (fields, require, fingerprint, timestamp, identifier,
                                     digest, integer, text, canonical)


def validate(value, key, pin_sha, now):
    from upgrade_manual_approval import validate_handoff
    fields(value, ('schema', 'base', 'base_sha256', 'imile_request_sha256',
                   'transaction_id', 'accepted_epoch', 'readbacks'))
    require(value['schema'] == 'ags.manual-mini-handoff.v2', 'handoff_schema_invalid')
    base = value['base']
    require(type(base) is dict and base.get('schema') == 'ags.manual-mini-handoff.v1',
            'handoff_base_invalid')
    require(value['base_sha256'] == fingerprint(base), 'handoff_digest_mismatch')
    digest(value['imile_request_sha256']); identifier(value['transaction_id'])
    integer(value['accepted_epoch'], 1)
    original = timestamp(base['received_at'])
    require(original <= now, 'handoff_future')
    validate_handoff(base, key, pin_sha, original)
    rows = value['readbacks']
    require(type(rows) is list and 1 <= len(rows) <= 1024, 'handoff_budget')
    previous, received = None, original
    for sequence, row in enumerate(rows, 1):
        fields(row, ('schema', 'base_sha256', 'imile_request_sha256', 'transaction_id',
                     'accepted_epoch', 'sequence', 'previous_sha256', 'actor', 'observed_at',
                     'received_at', 'mini_generation', 'health', 'unresolved', 'confirmed'))
        require(row['schema'] == 'ags.manual-mini-readback.v2', 'handoff_schema_invalid')
        for name in ('base_sha256', 'imile_request_sha256', 'transaction_id', 'accepted_epoch'):
            require(type(row[name]) is type(value[name]) and row[name] == value[name],
                    'handoff_binding_mismatch')
        require(type(row['sequence']) is int and row['sequence'] == sequence
                and row['previous_sha256'] == previous, 'handoff_sequence_invalid')
        fields(row['actor'], ('type', 'id', 'source'))
        require(row['actor']['type'] == 'human' and row['confirmed'] is True,
                'handoff_readback_invalid')
        text(row['actor']['id']); text(row['actor']['source'])
        observed, new_received = timestamp(row['observed_at']), timestamp(row['received_at'])
        require(received <= observed <= new_received <= now, 'handoff_time_invalid')
        require((new_received - observed).total_seconds() <= 300, 'handoff_stale')
        # S6.07 has no durable, independently verified accepted-epoch-to-new-
        # generation chain. Do not treat a caller's claimed lifecycle as proof.
        require(row['mini_generation'] == base['mini_generation'], 'mini_lifecycle_chain_required')
        require(row['health'] == {'status': 'ready', 'release_key': key}
                and row['unresolved'] == [], 'handoff_readback_invalid')
        previous, received = fingerprint(row), new_received
    require((now - timestamp(rows[-1]['observed_at'])).total_seconds() <= 300, 'handoff_stale')
    return value


def load(reference, key, pin_sha, binding, transaction_id, now):
    """A v2 package pins the base and private append-only directory, not latest bytes."""
    from upgrade_manual_approval import read_document
    from upgrade import directory_fd
    fields(reference, ('schema', 'base', 'base_sha256', 'imile_request_sha256',
                       'transaction_id', 'accepted_epoch', 'refresh_directory'))
    require(reference['schema'] == 'ags.handoff-reference.v2', 'handoff_schema_invalid')
    require(reference['imile_request_sha256'] == fingerprint(binding)
            and reference['transaction_id'] == transaction_id, 'handoff_binding_mismatch')
    directory = Path(reference['refresh_directory'])
    require(directory.is_absolute(), 'handoff_directory_invalid')
    with directory_fd(directory, private=True):
        latest, _ = read_document(directory / 'latest.json')
        fields(latest, ('sequence', 'sha256'))
        integer(latest['sequence'], 1, 1024); digest(latest['sha256'])
        require({p.name for p in directory.iterdir()} == {'latest.json'} |
                {'readback-%04d.json' % n for n in range(1, latest['sequence'] + 1)},
                'handoff_refresh_incomplete')
        rows = []
        for number in range(1, latest['sequence'] + 1):
            row, _ = read_document(directory / ('readback-%04d.json' % number))
            rows.append(row)
        require(fingerprint(rows[-1]) == latest['sha256'], 'handoff_latest_invalid')
    value = {name: reference[name] for name in ('base', 'base_sha256',
             'imile_request_sha256', 'transaction_id', 'accepted_epoch')}
    value.update(schema='ags.manual-mini-handoff.v2', readbacks=rows)
    return validate(value, key, pin_sha, now)


def refresh(root, reference, readback_path, key, pin_sha, binding, transaction_id, now):
    """Register an existing human file under hostops. Never invent confirmed/approve."""
    from upgrade_manual_approval import hostops, read_document
    from upgrade import directory_fd
    from upgrade_backup import durable_bytes, publish
    from upgrade_engine import save
    fields(reference, ('schema', 'base', 'base_sha256', 'imile_request_sha256',
                       'transaction_id', 'accepted_epoch', 'refresh_directory'))
    require(reference['schema'] == 'ags.handoff-reference.v2', 'handoff_schema_invalid')
    directory = Path(reference['refresh_directory'])
    require(directory.is_absolute(), 'handoff_directory_invalid')
    with hostops(root):
        directory = Path(reference['refresh_directory'])
        with directory_fd(directory, private=True):
            latest_path = directory / 'latest.json'
            if latest_path.exists():
                # Older readbacks may be stale; latest new row alone restores freshness.
                latest, _ = read_document(latest_path)
                fields(latest, ('sequence', 'sha256'))
                integer(latest['sequence'], 1, 1024); digest(latest['sha256'])
                rows = [read_document(directory / ('readback-%04d.json' % n))[0]
                        for n in range(1, latest['sequence'] + 1)]
                require(fingerprint(rows[-1]) == latest['sha256'], 'handoff_latest_invalid')
            else:
                rows = []
            require(reference['imile_request_sha256'] == fingerprint(binding)
                    and reference['transaction_id'] == transaction_id, 'handoff_binding_mismatch')
            require({p.name for p in directory.iterdir()} ==
                    ({'latest.json'} if rows else set()) |
                    {'readback-%04d.json' % n for n in range(1, len(rows) + 1)},
                    'handoff_refresh_incomplete')
            candidate, _ = read_document(readback_path)
            envelope = {name: reference[name] for name in ('base', 'base_sha256',
                'imile_request_sha256', 'transaction_id', 'accepted_epoch')}
            envelope.update(schema='ags.manual-mini-handoff.v2', readbacks=rows + [candidate])
            validate(envelope, key, pin_sha, now)
            path = directory / ('readback-%04d.json' % (len(rows) + 1))
            # An orphan after crash is not overwritten or silently adopted.
            require(not path.exists() and not path.is_symlink(), 'handoff_refresh_incomplete')
            durable_bytes(path, canonical(candidate)); publish(directory)
            save(latest_path, {'sequence': len(rows) + 1, 'sha256': fingerprint(candidate)})
            return envelope
