# pjdfstest Known Failures — NFSv3

Tests listed here are expected to fail and will NOT cause CI to report failure.
Only NEW failures (not in this list) will cause CI to fail. This is the same
blacklist model the SMB conformance harness uses, and both share the table
format and the `test/common/known-failures.sh` parser.

The `Test Name` column is a pjdfstest test-file path relative to the `tests/`
directory (e.g. `chmod/03.t`). Shell-glob wildcards are supported
(`flock/*`, `utimensat/0?.t`). `parse-results.sh` keys off this column.

Categories:
- **proto** — fundamental NFS protocol limitation (cannot be implemented).
- **feature** — a feature DittoFS does not implement (NFSv3 scope).
- **env** — test-environment / client-side kernel limitation, not a server bug.
- **bug** — a real DittoFS defect, tracked by the linked issue (walk back once fixed).

## Expected Failures

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| open/etxtbsy | proto | ETXTBSY cannot be enforced over NFS — server has no execution visibility | - |
| open::etxtbsy | proto | ETXTBSY cannot be enforced over NFS — server has no execution visibility | - |
| flock/* | feature | NLM protocol not implemented — no file locking over NFSv3 | - |
| fcntl/lock* | feature | NLM protocol not implemented — no record locking | - |
| lockf/* | feature | NLM protocol not implemented | - |
| xattr/* | feature | Extended attributes not supported in NFSv3 | - |
| getxattr/* | feature | Extended attributes not supported | - |
| setxattr/* | feature | Extended attributes not supported | - |
| listxattr/* | feature | Extended attributes not supported | - |
| removexattr/* | feature | Extended attributes not supported | - |
| chflags/* | feature | BSD-specific file flags, not in POSIX | - |
| lchflags/* | feature | BSD-specific file flags, not in POSIX | - |
| acl/* | feature | ACLs require NFSv4; NFSv3 mount has no ACL support | - |
| getfacl/* | feature | ACLs require NFSv4 | - |
| setfacl/* | feature | ACLs require NFSv4 | - |
| fallocate/* | feature | No ALLOCATE procedure in NFSv3 | - |
| posix_fallocate/* | feature | No ALLOCATE procedure in NFSv3 | - |
| utimensat/09.t | proto | NFSv3 nfstime3 uses uint32 seconds — cannot represent values >= 2^32 (year 2106) | - |
| open/03.t | env | PATH_MAX test fails: mount-point prefix pushes the absolute path over PATH_MAX in the Linux VFS before NFS sees it — client-side, affects any non-root mount | - |
