#!/usr/bin/env python3
"""One disposable launchd label. Never the production label or ~/.ags.

The caller must set AGS_ROUTEB_NATIVE=1. The label is bootstrapped only inside
a 0700 temporary root and is always booted out afterwards.
"""
import os
from pathlib import Path
import plistlib
import secrets
import subprocess
import tempfile


PRODUCTION_LABEL = 'com.jackie.agent-git-service'
PREFIX = 'com.jackie.ags-hostpkg-test.'


class HarnessError(RuntimeError):
    def __init__(self, code):
        super().__init__(code)
        self.code = code


def _run(argv):
    return subprocess.run(argv, check=False, capture_output=True, text=True, timeout=10)


def exercise():
    if os.environ.get('AGS_ROUTEB_NATIVE') != '1':
        raise RuntimeError('native_not_requested')
    label = PREFIX + secrets.token_hex(6)
    if label == PRODUCTION_LABEL or not label.startswith(PREFIX):
        raise RuntimeError('label_rejected')
    root = Path(tempfile.mkdtemp(prefix='ags-hostpkg-native-'))
    os.chmod(root, 0o700)
    plist_path = root / 'job.plist'
    domain = 'gui/' + str(os.getuid())
    target = domain + '/' + label
    plist_path.write_bytes(plistlib.dumps({
        'Label': label,
        'ProgramArguments': ['/usr/bin/true'],
        'RunAtLoad': False,
        'KeepAlive': False,
    }))
    os.chmod(plist_path, 0o600)
    created = False
    observed = False
    try:
        bootstrap = _run(['/bin/launchctl', 'bootstrap', domain, str(plist_path)])
        created = bootstrap.returncode == 0
        printed = _run(['/bin/launchctl', 'print', target])
        observed = (printed.returncode == 0 and label in printed.stdout
                    and 'path = ' + str(plist_path) in printed.stdout)
    finally:
        bootout = subprocess.run(['/bin/launchctl', 'bootout', target], check=False,
                                 capture_output=True, text=True, timeout=10)
        if plist_path.exists():
            plist_path.unlink()
        if root.exists():
            root.rmdir()
    listed = subprocess.run(['/bin/launchctl', 'print', target], check=False,
                            capture_output=True, text=True, timeout=10)
    gone = listed.returncode == 113 and 'Could not find service' in (listed.stderr or '')
    if not created:
        raise HarnessError('bootstrap_failed')
    if not observed:
        raise HarnessError('identity_unobserved')
    if bootout.returncode != 0 or not gone:
        raise HarnessError('cleanup_unproven')
    return {'label': label, 'cleaned': True, 'created': created, 'observed': observed,
            'production_label_touched': label == PRODUCTION_LABEL}
