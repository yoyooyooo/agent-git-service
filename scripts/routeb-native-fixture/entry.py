#!/usr/bin/env python3
"""Fixed isolated import entry; run.py still requires an explicit hosted VM."""
from pathlib import Path
import sys

if not sys.flags.isolated or not sys.flags.dont_write_bytecode:
    raise SystemExit(78)
sys.path.insert(0, str(Path(__file__).resolve().parent))
from run import main

raise SystemExit(main())
