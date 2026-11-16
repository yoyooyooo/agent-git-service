#!/bin/sh
# Start an internal Python implementation without writing bytecode.
# CPython can write standard-library .pyc before the first line of a .py file
# runs, so the guaranteed entry must set these controls in the shell first.
# Direct "python3 script.py" is not a guaranteed entry.
set -eu
export PYTHONDONTWRITEBYTECODE=1
unset PYTHONPYCACHEPREFIX || true
exec python3 -B "$@"
