#!/usr/bin/env python3
# Byte-range lock helper for the NLM cross-protocol interop e2e (issue #1503).
#
# Uses fcntl.lockf() (non-blocking POSIX byte-range locks) so a conflict returns
# immediately and we avoid any arch-specific `struct flock` layout.
#
# Two modes:
#
#   hold  PATH TYPE START LEN ACQFILE RELFILE
#       Acquire the lock, write "ok" to ACQFILE (or "err: ..." on failure), then
#       block until RELFILE appears (or a 30s safety timeout), then release.
#
#   try   PATH TYPE START LEN
#       Attempt the lock once. Print "ACQUIRED" (and release) if granted, or
#       "BLOCKED" if it conflicts (EACCES/EAGAIN). Any other error prints
#       "ERROR: ..." and exits 2.
#
# TYPE is "w" (exclusive) or "r" (shared).
import errno
import fcntl
import os
import sys
import time

# Errnos that mean "another holder owns a conflicting lock" — the only outcome
# that counts as a real, server-detected conflict. Anything else (ENOLCK from a
# not-ready lock manager, ESTALE, EIO, ...) is a harness error, NOT a conflict,
# and must not be reported as BLOCKED.
_CONFLICT_ERRNOS = (errno.EACCES, errno.EAGAIN)


def _open(path):
    return os.open(path, os.O_RDWR | os.O_CREAT, 0o644)


def _cmd(ltype):
    base = fcntl.LOCK_EX if ltype == "w" else fcntl.LOCK_SH
    return base | fcntl.LOCK_NB


def main():
    mode = sys.argv[1]
    path = sys.argv[2]
    cmd = _cmd(sys.argv[3])
    start = int(sys.argv[4])
    length = int(sys.argv[5])

    if mode == "try":
        fd = _open(path)
        try:
            fcntl.lockf(fd, cmd, length, start, os.SEEK_SET)
        except OSError as e:
            if e.errno in _CONFLICT_ERRNOS:
                print("BLOCKED")
                return 0
            print("ERROR: %s" % e)
            return 2
        else:
            print("ACQUIRED")
            fcntl.lockf(fd, fcntl.LOCK_UN, length, start, os.SEEK_SET)
            return 0
        finally:
            os.close(fd)

    if mode == "hold":
        acqfile, relfile = sys.argv[6], sys.argv[7]
        fd = _open(path)
        try:
            fcntl.lockf(fd, cmd, length, start, os.SEEK_SET)
        except OSError as e:
            with open(acqfile, "w") as f:
                f.write("err: %s" % e)
            os.close(fd)
            return 3
        with open(acqfile, "w") as f:
            f.write("ok pid=%d" % os.getpid())
        for _ in range(300):  # 30s safety cap
            if os.path.exists(relfile):
                break
            time.sleep(0.1)
        os.close(fd)
        return 0

    print("ERROR: unknown mode %r" % mode)
    return 2


if __name__ == "__main__":
    sys.exit(main())
