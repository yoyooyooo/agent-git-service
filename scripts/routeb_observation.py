#!/usr/bin/env python3
"""Production observation after a validated start. No network calls.

Elapsed time starts at validation, not prepare. 599 seconds is short. A lease
longer than the elapsed window is uncovered. readyz 200 does not close an
unknown provider result. Recovery inventory and gate are the maintenance-window
functions; this module does not reimplement them.
"""
import sys
sys.dont_write_bytecode = True
sys.pycache_prefix = None

from ags_maintenance_window import recovery_gate, recovery_inventory


MINIMUM_SECONDS = 600


class ObservationError(ValueError):
    def __init__(self, code):
        self.code = code
        super().__init__(code)


def accept(evidence):
    if evidence.get('schema') != 'ags.routeb-observation.v1':
        raise ObservationError('schema_invalid')
    if evidence.get('interrupted') or not evidence.get('generation') or not evidence.get('boot_id'):
        raise ObservationError('observation_interrupted')
    started = evidence.get('started_monotonic')
    now = evidence.get('now_monotonic')
    if type(started) is not int or type(now) is not int or now < started:
        raise ObservationError('clock_invalid')
    elapsed = now - started
    if elapsed < MINIMUM_SECONDS:
        raise ObservationError('observation_short')
    lease = evidence.get('lease_seconds', 0)
    if type(lease) is not int or lease < 0:
        raise ObservationError('lease_invalid')
    if elapsed < max(MINIMUM_SECONDS, lease):
        raise ObservationError('lease_uncovered')
    readyz = evidence.get('readyz') or {}
    if readyz.get('local') != 200 or readyz.get('service') != 200:
        raise ObservationError('readyz_not_ready')
    if readyz.get('revision') != evidence.get('revision'):
        raise ObservationError('revision_mismatch')
    if readyz.get('diagnostic_only') is not True:
        raise ObservationError('readyz_not_diagnostic')
    provider = evidence.get('provider') or {}
    if provider.get('source') != 'observed' or provider.get('unknown_external_results') != 0:
        raise ObservationError('provider_unknown')
    business = evidence.get('business') or {}
    for key in ('git', 'pull_request', 'wiki'):
        if business.get(key) != 'read':
            raise ObservationError('business_unread')
    if evidence.get('recovery') not in ([], ()):
        raise ObservationError('recovery_not_fresh')
    return {'state': 'awaiting_acceptance', 'elapsed': elapsed, 'accepted': False}
