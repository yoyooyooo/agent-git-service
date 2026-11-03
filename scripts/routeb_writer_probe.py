#!/usr/bin/env python3
"""Closed writer observation for one generation. No process is stopped here."""


def observe(*, generation, listeners, holders, descendants):
    writers = []
    unknown = []
    if listeners:
        writers.append({'kind': 'listener', 'count': len(listeners)})
    if holders:
        writers.append({'kind': 'holder', 'count': len(holders)})
    if descendants:
        writers.append({'kind': 'descendant', 'count': len(descendants)})
    return {'generation': generation, 'closed': not writers and not unknown,
            'writers': writers, 'unknown': unknown}
