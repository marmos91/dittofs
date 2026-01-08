# Known Limitations

This document describes the known limitations of DittoFS due to protocol constraints, design decisions, or features not yet implemented.

## Table of Contents

- [NFS Protocol Limitations](#nfs-protocol-limitations)
  - [ETXTBSY (Text File Busy)](#etxtbsy-text-file-busy)
  - [Timestamps (Y2106 Limitation)](#timestamps-y2106-limitation)
  - [File Locking](#file-locking)
  - [Extended Attributes](#extended-attributes)
  - [ACLs](#acls)
  - [fallocate/posix_fallocate](#fallocateposix_fallocate)
- [Storage Backend Limitations](#storage-backend-limitations)
  - [Hard Links](#hard-links)
  - [Special Files](#special-files)
- [General Limitations](#general-limitations)
  - [Single Node Only](#single-node-only)
  - [Security](#security)
- [POSIX Compliance Summary](#posix-compliance-summary)

## NFS Protocol Limitations

These limitations affect ALL NFS implementations, not just DittoFS. They are fundamental constraints of the NFSv3 protocol.

### ETXTBSY (Text File Busy)

| Status | Reason |
|--------|--------|
| Not supported | NFS protocol limitation |

**What it is**: ETXTBSY is an error that should be returned when attempting to write to a file that is currently being executed.

**Why it doesn't work on NFS**:
- ETXTBSY requires knowing if a file is being executed
- NFS servers have **no way to know** if any client is executing a file
- NFS clients don't enforce it either (intentionally removed in SVR4)
- This was done to make local and remote filesystem behavior consistent

**Impact**: Programs can overwrite executables while they're running. This is rarely a problem in practice since:
- Most package managers remove-then-replace rather than overwrite
- Linux allows removing an executing file (just not writing to it)

**References**:
- [LWN - The shrinking role of ETXTBSY](https://lwn.net/Articles/866493/)
- [Why Text File Busy Error](https://utcc.utoronto.ca/~cks/space/blog/unix/WhyTextFileBusyError)

### Timestamps (Y2106 Limitation)

| Status | Reason |
|--------|--------|
| Max timestamp: 2106-02-07 | NFSv3 uses 32-bit unsigned seconds |

**What it is**: NFSv3's `nfstime3` structure uses a 32-bit unsigned integer for seconds since Unix epoch, limiting the maximum representable timestamp.

**Wire format** (RFC 1813 Section 2.2):
```c
struct nfstime3 {
    uint32 seconds;   // Max value: 4294967295
    uint32 nseconds;
};
```

**Timestamp range**:
- Minimum: 1970-01-01 00:00:00 UTC (0)
- Maximum: 2106-02-07 06:28:15 UTC (4294967295)

**Why timestamps beyond 2106 fail**:
1. User calls `utimensat()` with timestamp > 4294967295
2. Linux VFS checks filesystem's declared `s_time_max`
3. NFSv3 declares `s_time_max = U32_MAX` (4294967295)
4. VFS clamps timestamp to maximum per POSIX requirement
5. Clamped value is sent to server

**POSIX behavior**: Per POSIX.1, `futimens`/`utimensat` must set timestamps to "the greatest value supported by the file system that is not greater than the specified time." The Linux kernel implements this correctly.

**pjdfstest impact**: Test `utimensat/09.t` test 5 fails because it sets `mtime=4294967296` (2^32), which exceeds the NFSv3 maximum.

**NFSv4 solution**: NFSv4 uses 64-bit timestamps (`s_time_min = S64_MIN`, `s_time_max = S64_MAX`), supporting dates from year 0 to ~292 billion years in the future.

**References**:
- [RFC 1813 Section 2.2 - nfstime3](https://tools.ietf.org/html/rfc1813#section-2.2)
- [Linux kernel fs/nfs/super.c](https://github.com/torvalds/linux/blob/master/fs/nfs/super.c) - NFSv3 sets `s_time_max = U32_MAX`
- [Linux kernel VFS timestamp clamping](https://patchwork.kernel.org/patch/9581399/)
- [Linux Y2038 VFS changes](https://kernelnewbies.org/y2038/vfs)

### File Locking

| Status | Reason |
|--------|--------|
| Not implemented | NLM protocol not implemented |

DittoFS does not implement the NFS Lock Manager (NLM) protocol, which is required for:
- `flock()` - Advisory file locking
- `fcntl()` - Record (byte-range) locking
- `lockf()` - POSIX locking

**Impact**: Applications that require file locks may not work correctly. Multiple clients can write to the same file simultaneously without coordination.

**Workarounds**:
- Use application-level locking (e.g., lock files, database locks)
- Use a single-writer architecture
- Consider NFSv4 which has built-in locking (not yet implemented)

### Extended Attributes

| Status | Reason |
|--------|--------|
| Not supported | Not in NFSv3 base specification |

Extended attributes (xattrs) are not part of the NFSv3 base specification. They would require:
- NFS extensions (RFC 8276 for NFSv4.2)
- Or NFSv4 support

**Affected operations**: `getxattr`, `setxattr`, `listxattr`, `removexattr`

### ACLs

| Status | Reason |
|--------|--------|
| Not supported | Requires NFSv4 |

Access Control Lists require NFSv4 support. DittoFS only supports NFSv3 currently.

**Affected operations**: `getfacl`, `setfacl`, ACL-based permission checks

**Current permissions**: Standard POSIX mode bits (rwxrwxrwx) are fully supported.

### fallocate/posix_fallocate

| Status | Reason |
|--------|--------|
| Not supported | No ALLOCATE procedure in NFSv3 |

NFSv3 has no procedure for pre-allocating disk space. Space is allocated on actual write.

**Impact**: Applications using `fallocate()` for space reservation will fail. Writes still work normally.

## Storage Backend Limitations

### Hard Links

| Backend | Status | Notes |
|---------|--------|-------|
| Memory | Full support | Link counts tracked correctly |
| BadgerDB | Full support | NFS LINK procedure works |
| PostgreSQL | Full support | NFS LINK procedure works |

All backends now support hard links via the NFS LINK procedure. Link counts are properly tracked and exposed via `stat()`.

### Special Files

| Type | Status | Notes |
|------|--------|-------|
| Character devices | Metadata only | MKNOD creates entry, no device functionality |
| Block devices | Metadata only | MKNOD creates entry, no device functionality |
| FIFOs | Metadata only | MKNOD creates entry, no pipe functionality |
| Sockets | Metadata only | MKNOD creates entry, no socket functionality |

DittoFS can create special file entries via MKNOD, but they don't function as actual devices, pipes, or sockets. The metadata (type, mode, device numbers) is preserved but operations on the files themselves won't work as expected.

## General Limitations

### Single Node Only

DittoFS currently runs as a single server instance:
- No clustering or high availability
- No replication (except via S3 bucket replication)
- Single point of failure

For high availability, consider:
- Running behind a load balancer with sticky sessions
- Using S3 backend with cross-region replication
- Container orchestration with health checks

### Security

DittoFS is experimental and has not been security audited:

| Feature | Status |
|---------|--------|
| AUTH_UNIX | Supported (basic) |
| Kerberos | Not implemented |
| TLS/Encryption | Not built-in |
| Security Audit | Not performed |

**Recommendations**:
- Run behind VPN or use network-level encryption
- Don't expose directly to untrusted networks
- See [SECURITY.md](SECURITY.md) for detailed recommendations

## POSIX Compliance Summary

DittoFS achieves **99.99% pass rate** on [pjdfstest](https://github.com/saidsay-so/pjdfstest) POSIX compliance tests (8789 tests, 1 expected failure).

This pass rate applies to **all metadata backends**:
- **Memory store**: 99.99% (for testing/ephemeral use)
- **BadgerDB store**: 99.99% (for persistent/embedded use)
- **PostgreSQL store**: 99.99% (for distributed/production use)

### Test Results

| Metric | Value |
|--------|-------|
| Total tests | 8789 |
| Passed | 8788 |
| Failed (expected) | 1 |
| Pass rate | 99.99% |

### Expected Failures

The following tests are expected to fail due to NFSv3 protocol limitations:

| Test Pattern | Reason |
|--------------|--------|
| `utimensat/09.t:test5` | NFSv3 32-bit timestamp limit (max year 2106) |
| `open::etxtbsy` | NFS protocol limitation (not testable) |
| `flock/*` | NLM not implemented |
| `fcntl/lock*` | NLM not implemented |
| `lockf/*` | NLM not implemented |
| `xattr/*`, `*xattr/*` | Not in NFSv3 |
| `acl/*`, `*facl/*` | Requires NFSv4 |
| `fallocate/*` | No ALLOCATE in NFSv3 |
| `chflags/*` | BSD-specific |

**Note**: Only `utimensat/09.t:test5` actually fails in current pjdfstest runs. Other patterns either don't have tests in the suite or the tests are skipped.

See `test/posix/known_failures.txt` for the complete list with detailed explanations.

### PostgreSQL Store POSIX Compliance

The PostgreSQL metadata store required several specific fixes to achieve POSIX compliance parity with Memory and BadgerDB stores:

| Fix | Description |
|-----|-------------|
| EPERM vs EACCES | Return correct error code for permission failures in `SetFileAttributes` |
| SUID/SGID clearing | Clear setuid/setgid bits on write (CommitWrite) and chown (SetFileAttributes) |
| Group membership check | Validate caller belongs to target GID when changing file group ownership |
| SGID directory inheritance | Clear SGID when non-group-member writes to file in SGID directory |
| ENAMETOOLONG | Consistent 255-byte filename limit validation across all operations |
| Sticky bit restrictions | Enforce sticky bit semantics for rename, unlink, and rmdir operations |
| Cross-directory moves | Ownership check for directory moves between different parent directories |
| Parent nlink updates | Update parent directory link count via `link_counts` table (not `files.nlink`) |
| Silly rename cleanup | Use `GREATEST(link_count - 1, 0)` to handle NFS silly-renamed files with nlink=0 |

**Key implementation details:**

1. **Link count tracking**: PostgreSQL uses a separate `link_counts` table as the source of truth for nlink values. The `files.nlink` column is updated for consistency but `GetFile` reads from `link_counts` via JOIN.

2. **Silly rename handling**: When NFS clients rename files to `.nfs*` (silly rename for open-but-deleted files), the server sets `link_count=0`. Subsequent `RemoveFile` calls use `GREATEST(link_count - 1, 0)` to prevent negative values.

3. **Cross-directory directory moves**: Moving a directory to a different parent requires updating both source and destination parent link counts (the `..` entry counts as a link to the parent).

## See Also

- [FAQ](FAQ.md) - Frequently asked questions
- [Troubleshooting](TROUBLESHOOTING.md) - Common issues and solutions
- [Security](SECURITY.md) - Security considerations
- [NFS Implementation](NFS.md) - NFSv3 protocol details
