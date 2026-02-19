---
status: resolved
trigger: "Fix remaining NFSv4 POSIX compliance test failures (20 test files)"
created: 2026-02-19T00:00:00Z
updated: 2026-02-19T16:30:00Z
---

## Current Focus

RESOLVED - All fixes verified in CI run 22189302629
All 6 jobs passed (3 NFSv3 + 3 NFSv4, each with memory/badger/postgres backends)
NFSv4: 2 known failures (open/03.t, unlink/14.t) - all others pass

## Symptoms

expected: All POSIX compliance tests pass (as they do for NFSv3)
actual: 20 test files failing across categories
errors: See detailed breakdown below
reproduction: CI run 22185316253 on branch fix/140-nfsv4-posix-ci
started: After NFSv4 adapter was implemented

## Eliminated

- hypothesis: unlink/14.t is a server bug (nlink not set to 0)
  evidence: RemoveFile correctly sets nlink=0. The issue is NFSv4 client-side
    silly-rename: stateful OPEN holds dentry reference (d_count > 1), causing
    kernel to rename instead of remove. File keeps nlink=1 because it was
    renamed, not removed. NFSv3 passes because stateless protocol means
    d_count == 1, so kernel sends REMOVE directly. This is inherent to the
    Linux NFS client's NFSv4 implementation.
  timestamp: 2026-02-19

## Evidence

- timestamp: 2026-02-19
  checked: NFSv4 OPEN handler (open.go lines 111-132, 241-272)
  found: skipFattr4(reader) discards createattrs instead of parsing mode; hardcodes Mode 0o644
  implication: ROOT CAUSE for open/00.t and open/02.t - mode not applied during OPEN CREATE

- timestamp: 2026-02-19
  checked: NFSv4 OPEN handler (open.go lines 230-240 NOCREATE path)
  found: OPEN4_NOCREATE only does Lookup but never checks file access permissions (read/write)
  implication: ROOT CAUSE for open/06.t - EACCES not returned for unauthorized open

- timestamp: 2026-02-19
  checked: NFSv4 OPEN handler vs open/07.t failures
  found: O_RDONLY|O_TRUNC expected EACCES got EPERM - truncate permission check uses wrong error
  implication: ROOT CAUSE for open/07.t - wrong error mapping for unauthorized truncate

- timestamp: 2026-02-19
  checked: NFSv4 attrs/encode.go for RAWDEV support
  found: FATTR4_RAWDEV bit number was 34 instead of 41 (RFC 7530 specifies bit 41)
  implication: ROOT CAUSE for mknod/11.t - device major/minor encoded at wrong position

- timestamp: 2026-02-19
  checked: NFSv4 attrs/encode.go for ctime support
  found: FATTR4_TIME_METADATA (bit 52) completely missing from attribute encoding
  implication: ROOT CAUSE for majority of "bare not ok" ctime failures across ~14 test files

- timestamp: 2026-02-19
  checked: SetFileAttributes for truncate error code
  found: writePermSufficient path returns ErrPermissionDenied (EPERM) instead of ErrAccessDenied (EACCES)
  implication: ROOT CAUSE for open/07.t - truncate without write permission returns wrong errno

- timestamp: 2026-02-19
  checked: SetFileAttributes for truncate mtime update
  found: Size change via SETATTR does not update mtime server-side
  implication: ROOT CAUSE for open/00.t tests 43-44 - O_TRUNC mtime not updated

- timestamp: 2026-02-19
  checked: deferredCommitWrite for SUID/SGID clearing (chmod/12.t)
  found: Mode clearing only recorded in pending state, not persisted to store.
    NFSv4 client COMPOUND WRITE+GETATTR uses cache_consistency_bitmask which
    excludes MODE bit. Standalone GETATTR with noac should request MODE, but
    the pending-state-only approach was insufficient for the client to see
    the cleared SUID bits.
  implication: ROOT CAUSE for chmod/12.t - SUID not cleared after non-owner write

- timestamp: 2026-02-19
  checked: Linux kernel NFS client unlink behavior (v3 vs v4)
  found: NFSv4 stateful OPEN increments dentry refcount, causing kernel
    silly-rename (d_count > 1) when unlink is called on open file. NFSv3
    stateless protocol doesn't increment d_count, so REMOVE is sent directly.
  implication: unlink/14.t is inherent NFSv4 client behavior, not a server bug

## Resolution

root_cause: Multiple NFSv4 handler deficiencies (8 server issues + 1 client-side):
1. OPEN skips createattrs instead of parsing/applying mode
2. OPEN doesn't verify file access permissions (read/write)
3. FATTR4_RAWDEV bit number wrong (34 instead of 41)
4. Missing FATTR4_TIME_METADATA (ctime) encoding
5. Wrong error code (EPERM instead of EACCES) for write-denied truncate/utimensat
6. Truncate via SETATTR does not update mtime server-side
7. SUID/SGID clearing not persisted to store in deferred write path
8. unlink/14.t: NFSv4 client silly-rename (inherent, documented as known failure)

fix: Applied 8 fixes in 3 commits:
  - c7e58a9: Fix OPEN createattrs, access check, RAWDEV, TIME_METADATA, truncate error code
  - 50c6a22: Fix mtime update on truncate
  - c988366: Fix RAWDEV bit 34->41, SUID clearing persistence, unlink/14.t known failure

verification: CI run 22189302629 - ALL 6 JOBS PASSED
  - NFS v3 / Memory: 1 known failure (open/03.t) - PASS
  - NFS v3 / BadgerDB: 1 known failure (open/03.t) - PASS
  - NFS v3 / PostgreSQL: 1 known failure (open/03.t) - PASS
  - NFS v4 / Memory: 2 known failures (open/03.t, unlink/14.t) - PASS
  - NFS v4 / BadgerDB: 2 known failures (open/03.t, unlink/14.t) - PASS
  - NFS v4 / PostgreSQL: 2 known failures (open/03.t, unlink/14.t) - PASS
  All 8789 tests per job, 237 test files. Zero unexpected failures.

files_changed:
  - internal/protocol/nfs/v4/attrs/encode.go
  - internal/protocol/nfs/v4/handlers/open.go
  - pkg/metadata/file.go
  - pkg/metadata/io.go
  - test/posix/known_failures_v4.txt
  - .github/workflows/posix-tests.yml
