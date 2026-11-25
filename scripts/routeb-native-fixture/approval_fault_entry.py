#!/usr/bin/env python3
"""Isolated import entry for the repository-owned one-shot fault helper."""
from pathlib import Path
import sys

if not sys.flags.isolated or not sys.flags.dont_write_bytecode:
    raise SystemExit(78)
sys.path.insert(0, str(Path(__file__).resolve().parent))
from approval_fault import main

raise SystemExit(main())
