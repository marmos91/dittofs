# POSIX Compliance Test Results

**Test Suite**: [pjdfstest](https://github.com/saidsay-so/pjdfstest) (8,789 tests)
**Last Updated**: January 8, 2026

## Summary

| Metadata Store | Passed | Failed | Pass Rate |
|----------------|--------|--------|-----------|
| Memory         | 8,788  | 1      | **99.99%** |
| BadgerDB       | 8,788  | 1      | **99.99%** |
| PostgreSQL     | 8,788  | 1      | **99.99%** |

All three metadata stores achieve parity with a single expected failure.

## Expected Failure

**utimensat/09.t test #5** - NFSv3 timestamp limitation

NFSv3's `nfstime3` structure uses a 32-bit unsigned integer for seconds, limiting timestamps to year 2106. This is a protocol limitation, not a DittoFS bug.

See `test/posix/known_failures.txt` for the complete list of known limitations.

## Test Environment

- **OS**: Linux 6.17.0-8-generic
- **NFS Port**: 12049
- **Mount Options**: NFSv3, tcp, nolock
- **Content Store**: Memory (for POSIX testing)

## Running Tests

```bash
# Enter development shell
nix develop

# Start DittoFS with desired metadata store
./dfs start --config test/posix/configs/config-memory.yaml
./dfs start --config test/posix/configs/config-badger.yaml
./dfs start --config test/posix/configs/config-postgres.yaml

# Additional content store configurations
./dfs start --config test/posix/configs/config-memory-content.yaml  # Memory content store
./dfs start --config test/posix/configs/config-cache-s3.yaml        # Cache + S3 (requires Localstack)

# Mount and run tests
dittofs-mount /tmp/dittofs-test
dittofs-posix
dittofs-umount /tmp/dittofs-test
```

## Key Fixes Applied

All metadata stores implement the following POSIX fixes:

| Fix | Description |
|-----|-------------|
| Sticky bit | Enforce sticky bit restrictions for rename/unlink/rmdir |
| Cross-dir moves | Only owner/root can move directories to different parents |
| Silly rename | Set nlink=0 for `.nfs*` renamed files (NFS open-but-deleted) |
| SUID/SGID | Clear setuid/setgid bits on write and non-root chown |
| Permission checks | Return EPERM vs EACCES correctly per POSIX |
| Group membership | Validate caller belongs to target GID for chown |

### PostgreSQL-Specific Fixes

| Fix | Description |
|-----|-------------|
| Partial unique constraint | Allow nlink=0 files while enforcing uniqueness for active files |
| Link count source of truth | Use `link_counts` table instead of `files.nlink` for GetFile |
| Parent nlink updates | Update parent link counts via `link_counts` table for directory operations |
| Silly rename cleanup | Use `GREATEST(link_count - 1, 0)` to handle files with nlink already at 0 |

## Raw Test Results

Raw test output is stored in `test/posix/results/` with timestamps indicating the test run date and configuration.
