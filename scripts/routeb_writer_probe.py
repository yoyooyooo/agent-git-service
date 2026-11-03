#!/usr/bin/env python3
"""Closed writer observation for one generation. No process is stopped here.

An empty list is an observation that found nothing. None, a failed enumeration,
or a permission error is unknown and cannot close the generation.
"""


def _domain(name, value, source):
    if value is None or source not in (None, 'observed'):
        return [], [{'domain': name, 'source': source or 'unobserved'}]
    if not isinstance(value, (list, tuple)):
        return [], [{'domain': name, 'source': 'malformed'}]
    return [{'kind': name, 'count': len(value)}], []


def observe(*, generation, listeners, holders, descendants, sources=None):
    sources = sources or {}
    writers = []
    unknown = []
    for name, value in (('listener', listeners), ('holder', holders), ('descendant', descendants)):
        found, missing = _domain(name, value, sources.get(name + 's'))
        if value:
            writers.extend(found)
        unknown.extend(missing)
    return {'generation': generation, 'source': 'routeb-writer-probe',
            'closed': not writers and not unknown,
            'writers': writers, 'unknown': unknown}
