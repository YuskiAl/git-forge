#!/usr/bin/env python3
"""Launcher for the static Go forge binary.

The platform runs `python3 /bot/main.py`; this process replaces itself
in-place via os.execv, so PID 1 is 100% native Go with zero Python runtime
overhead. If /bot is read-only and the execute bit was lost in transit,
the binary is copied to /tmp and executed from there.
"""
import os
import shutil
import stat
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
NATIVE = os.path.join(HERE, "forge-linux-amd64")


def main():
    native = NATIVE
    if not os.path.exists(native):
        sys.stderr.write("forge binary missing: %s\n" % native)
        return 1
    if not os.access(native, os.X_OK):
        tmp = "/tmp/forge-linux-amd64"
        try:
            shutil.copyfile(native, tmp)
            os.chmod(tmp, 0o755)
            native = tmp
        except OSError as exc:
            sys.stderr.write("cannot stage forge binary: %s\n" % exc)
            return 1
    os.execv(native, [native])
    return 1


if __name__ == "__main__":
    sys.exit(main())
