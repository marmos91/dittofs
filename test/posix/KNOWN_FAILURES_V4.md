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
| rename/23.t | bug | PostgreSQL rename-overwrite of an existing target returns EEXIST (nlink ends at 2); memory/badger pass. Not a long-path bug — normal-length names | #1160 |
