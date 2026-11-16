#!/usr/bin/env python3
"""Validate an archived native evidence file. This does not run launchd.

Missing evidence, a skipped run, a fixture labeled as native, or an
observation shorter than 600 seconds is not a pass. A valid file still does
not authorize install or deployment.
"""
import sys
sys.dont_write_bytecode = True
sys.pycache_prefix = None

SCHEMA = 'ags.routeb-native-evidence.v1'
FIELDS = ('schema', 'kind', 'head', 'label_prefix', 'production_label_touched',
          'cleaned', 'observation_seconds', 'metadata_roundtrip', 'restore_once', 'skipped')


class EvidenceError(ValueError):
    def __init__(self, code):
        self.code = code
        super().__init__(code)


def validate(value):
    if not isinstance(value, dict):
        raise EvidenceError('native_evidence_missing')
    if set(value) != set(FIELDS) or value.get('schema') != SCHEMA:
        raise EvidenceError('schema_invalid')
    if value.get('skipped') is True:
        raise EvidenceError('native_skipped')
    if value.get('kind') != 'native':
        raise EvidenceError('not_native')
    if value.get('production_label_touched') is not False or value.get('cleaned') is not True:
        raise EvidenceError('cleanup_unproven')
    if not str(value.get('label_prefix', '')).startswith('com.jackie.ags-hostpkg-test.'):
        raise EvidenceError('label_rejected')
    seconds = value.get('observation_seconds')
    if type(seconds) is not int or seconds < 600:
        raise EvidenceError('observation_short')
    if value.get('metadata_roundtrip') is not True or value.get('restore_once') is not True:
        raise EvidenceError('native_incomplete')
    head = value.get('head')
    if not isinstance(head, str) or len(head) != 40:
        raise EvidenceError('exact_head_missing')
    return {'passed': True, 'installable': False, 'head': head}
