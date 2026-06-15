# pjdfstest Known Failures — NFSv4.0 / NFSv4.1

Tests listed here are expected to fail and will NOT cause CI to report failure.
Only NEW failures (not in this list) will cause CI to fail. This is the same
blacklist model the SMB conformance harness uses, and both share the table
format and the `test/common/known-failures.sh` parser.

The `Test Name` column is a pjdfstest test-file path relative to the `tests/`
directory (e.g. `unlink/14.t`). Shell-glob wildcards are supported. This list
applies to both NFSv4.0 and NFSv4.1 mounts.

Categories:
- **proto** — fundamental NFS / NFSv4-client protocol behavior (not a server bug).
- **feature** — a feature DittoFS does not implement (NFSv4.0 scope).
- **env** — test-environment / client-side kernel limitation, not a server bug.
- **bug** — a real DittoFS defect, tracked by the linked issue (walk back once fixed).

## Expected Failures

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| open/etxtbsy | proto | ETXTBSY cannot be enforced over NFS — server has no execution visibility | - |
| open::etxtbsy | proto | ETXTBSY cannot be enforced over NFS — server has no execution visibility | - |
| chflags/* | feature | BSD-specific file flags, not in POSIX | - |
| lchflags/* | feature | BSD-specific file flags, not in POSIX | - |
| fallocate/* | feature | No ALLOCATE operation in NFSv4.0 (requires NFSv4.2) | - |
| posix_fallocate/* | feature | No ALLOCATE operation in NFSv4.0 (requires NFSv4.2) | - |
| xattr/* | feature | NFSv4 named attributes (OPENATTR) not implemented | - |
| getxattr/* | feature | NFSv4 named attributes not implemented | - |
| setxattr/* | feature | NFSv4 named attributes not implemented | - |
| listxattr/* | feature | NFSv4 named attributes not implemented | - |
| removexattr/* | feature | NFSv4 named attributes not implemented | - |
| open/03.t | env | PATH_MAX test fails: mount-point prefix pushes the absolute path over PATH_MAX in the Linux VFS before NFS sees it — client-side, affects any non-root mount | - |
| unlink/14.t | proto | NFSv4 silly-rename: open file + unlink triggers rename instead of remove, nlink stays 1 (same as Linux knfsd) | - |
| chmod/03.t | bug | Deep/long composite path (~4000 chars) fails on the PostgreSQL backend (memory/badger pass) | #1152 |
| chown/03.t | bug | Deep/long composite path fails on the PostgreSQL backend | #1152 |
| ftruncate/03.t | bug | Deep/long composite path fails on the PostgreSQL backend | #1152 |
| link/03.t | bug | Deep/long composite path fails on the PostgreSQL backend | #1152 |
| truncate/03.t | bug | Deep/long composite path fails on the PostgreSQL backend | #1152 |
| unlink/03.t | bug | Deep/long composite path fails on the PostgreSQL backend | #1152 |
| rename/23.t | bug | Long-path rename fails on the PostgreSQL backend | #1152 |
| chown/07.t | bug | Intermittent EEXIST/EIO on the PostgreSQL backend — collateral of the deep-path bug leaving orphaned rows | #1152 |
| link/04.t | bug | Intermittent EEXIST/EIO on the PostgreSQL backend — collateral of the deep-path bug | #1152 |
| open/22.t | bug | Intermittent EEXIST / nlink mismatch on the PostgreSQL backend — collateral of the deep-path bug | #1152 |
| truncate/06.t | bug | Intermittent EEXIST on the PostgreSQL backend — collateral of the deep-path bug | #1152 |
| ftruncate/06.t | bug | Intermittent EEXIST on the PostgreSQL backend — collateral of the deep-path bug | #1152 |
| unlink/04.t | bug | Intermittent EEXIST on the PostgreSQL backend — collateral of the deep-path bug | #1152 |
| chmod/11.t | bug | Intermittent EEXIST on the PostgreSQL backend — collateral of the deep-path bug | #1152 |
