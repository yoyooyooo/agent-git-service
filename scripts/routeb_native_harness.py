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
    cleaned = False
    try:
        subprocess.run(['/bin/launchctl', 'bootstrap', domain, str(plist_path)],
                       check=False, capture_output=True, text=True, timeout=10)
        subprocess.run(['/bin/launchctl', 'print', target],
                       check=False, capture_output=True, text=True, timeout=10)
        cleaned = True
    finally:
        subprocess.run(['/bin/launchctl', 'bootout', target],
                       check=False, capture_output=True, text=True, timeout=10)
        if plist_path.exists():
            plist_path.unlink()
        if root.exists():
            root.rmdir()
    listed = subprocess.run(['/bin/launchctl', 'print', target], check=False,
                            capture_output=True, text=True, timeout=10)
    gone = listed.returncode != 0
    return {'label': label, 'cleaned': cleaned and gone, 'production_label_touched': False}
