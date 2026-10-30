#!/usr/bin/env python3
"""Internal Linux namespace entry; invoked only by the pinned smoke engine."""
import fcntl
from pathlib import Path
import socket
import struct
import sys

from upgrade import Failure
from upgrade_engine import smoke


def main():
    # A host with disabled unprivileged user namespaces fails before this entry.
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as sock:
        request = struct.pack('16sh', b'lo', 1)
        fcntl.ioctl(sock, 0x8914, request)  # SIOCSIFFLAGS: IFF_UP
    smoke(Path(sys.argv[1]), sys.argv[2], Path(sys.argv[3]),
          timeout=float(sys.argv[4]), database=Path(sys.argv[5]) if len(sys.argv) > 5 else None,
          _namespace=True)


if __name__ == '__main__':
    try:
        main()
    except (Failure, OSError):
        sys.exit(69)
