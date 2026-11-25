"""Consume a private delegation source; record reviews of newly measured bytes.

The file is supplied through the manual job's single-use secret. GitHub account
type is not human review. This module never publishes the source or scope grant.
"""
from datetime import datetime, timedelta, timezone
from pathlib import Path

from common import (SHA, exclusive_json, fingerprint, read_json, require, sha, utc)


DELEGATE = 'agent:chatgpt/root/audit_release_contract'
PURPOSES = ('native-provision', 'immutable-provision', 'native-bootstrap',
            'host-window', 'continue-installed', 'full-restore', 'old-start')
SUBJECTS = {
    'immutable-provision': ('host_instance', 'tool_manifest_sha256', 'installer_sha256', 'objects'),
    'native-bootstrap': ('root', 'host_instance', 'transaction_id', 'provision_sha256',
                         'identity_sha256', 'epoch'),
    'continue-installed': ('approval_id', 'purpose', 'subject_sha256'),
    'full-restore': ('approval_id', 'purpose', 'host_instance', 'snapshot_id', 'manifest_sha256',
                     'data_loss_acknowledged', 'external_reconciliation_acknowledged'),
    'old-start': ('approval_id', 'purpose', 'host_instance', 'snapshot_id', 'manifest_sha256',
                  'data_loss_acknowledged', 'external_reconciliation_acknowledged'),
}


def source_input(path, expected_digest):
    value, raw = read_json(path, 256 << 10, mode=0o600)
    require(type(expected_digest) is str and SHA.fullmatch(expected_digest)
            and sha(raw) == expected_digest, 'authorization_source_digest')
    require(value.get('schema') == 'ags.routeb-fixture-authorization-input.v1'
            and value.get('authority') is False and value.get('native_pass') is False
            and value.get('delegate') == DELEGATE
            and value.get('grantor') == 'human:chatgpt-requesting-user', 'authorization_source_scope')
    source = value.get('source', {})
    messages = source.get('messages')
    require(source.get('kind') == 'conversation' and source.get('captured_by') == DELEGATE
            and type(messages) is list and 0 < len(messages) <= 16, 'authorization_source_scope')
    for message in messages:
        require(type(message) is dict and set(message) == {'text', 'sent_at'}
                and type(message['text']) is str and 0 < len(message['text']) <= 16384
                and message['sent_at'] is None, 'authorization_source_message')
    controller = value.get('controller_scope', {})
    require(controller.get('recorded_by') == 'agent:chatgpt/root'
            and controller.get('evidence_claim') is False
            and type(controller.get('text')) is str and bool(controller['text']), 'controller_scope')
    captured = datetime.fromisoformat(value['captured_at'].replace('Z', '+00:00'))
    require(captured.tzinfo is not None and captured <= datetime.now(timezone.utc), 'source_capture_time')
    return value, sha(raw)


def grant(path, authorization, profile):
    value = {'schema': 'ags.routeb-native-scope-grant.v1',
             'grant_id': profile['transaction_id'] + '-grant',
             'grantor': authorization['grantor'], 'delegate': authorization['delegate'],
             'source': authorization['source'], 'scope': 'isolated-native',
             'host': profile['logical_host'], 'host_instance': profile['host_instance'],
             'transaction_id': profile['transaction_id'], 'root': profile['root'],
             'purposes': list(PURPOSES), 'recorded_at': utc()}
    return exclusive_json(path, value, mode=0o644)


def subject(value, purpose):
    if purpose == 'native-provision':
        return value['binding_sha256']
    if purpose == 'host-window':
        return value['package_sha256']
    return fingerprint({key: value[key] for key in SUBJECTS[purpose]})


def approve(value, purpose, grant_ref, evidence, *, lifetime=3600):
    """Bind the actual automated review, not an invented human signature.

    Callers first verify each subject's actual files and effect records. The
    supplied evidence references are hashes of those retained observations.
    """
    require(purpose in PURPOSES and 1 <= lifetime <= 7200 and evidence, 'review_scope')
    require(all(set(item) == {'kind', 'sha256'} and SHA.fullmatch(item['sha256'])
                for item in evidence), 'review_evidence')
    reviewed = utc()
    value = {**value, 'actor': DELEGATE, 'decision': 'approve', 'issued_at': utc(),
             'expires_at': (datetime.now(timezone.utc) + timedelta(seconds=lifetime)).isoformat()}
    value['delegation'] = {'scope_grant_sha256': grant_ref['sha256'], 'review': {
        'actor': DELEGATE, 'subject_sha256': subject(value, purpose),
        'reviewed_at': reviewed, 'evidence_refs': evidence}}
    return value
