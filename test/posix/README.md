# POSIX Compliance Testing for DittoFS

This directory contains POSIX compliance testing for DittoFS using [pjdfstest](https://github.com/saidsay-so/pjdfstest) (Rust rewrite).

## Overview

The test suite validates DittoFS's NFSv3 implementation against POSIX filesystem semantics using the Rust rewrite of pjdfstest, which provides better performance and maintainability than the original Perl version.

## Quick Start

### Through dt (Recommended)

pjdfstest is a suite of the test registry, `test/suites.yaml`, and runs through `dt`, as CI
runs it in `.github/workflows/tests.yml`. `dt` starts the server on the `badger-s3` profile,
mounts it with `noac,sync,lookupcache=none` (and `nolock` on NFSv3), runs each test file with
`prove`, and grades the results against `known.yaml`:

```bash
test/harness/bin/dt services up localstack          # badger-s3 keeps its blocks in S3
test/harness/bin/dt run pjdfstest/badger-s3/3       # NFSv3
test/harness/bin/dt run pjdfstest/badger-s3/4.1     # NFSv4.1
test/harness/bin/dt run pjdfstest/badger-s3/4 chmod/00.t   # one test file
```

It needs Linux, the kernel NFS client and passwordless sudo; on macOS, run it in the Linux
container, `test/harness/bin/dtc run pjdfstest/badger-s3/3`.

### Manual Setup

If you prefer manual setup:

```bash
# Terminal 1: Start DittoFS server
./dfs start --config test/posix/configs/config.yaml

# Terminal 2: Configure via API (server generates admin password on first start)
# Check server output for the generated password
ADMIN_PASSWORD="<from-server-output>"

# Login
./dfsctl login --server http://localhost:8080 --username admin --password "$ADMIN_PASSWORD"

# Create stores
./dfsctl store metadata add --name default --in-memory
./dfsctl store block add --name default --type memory

# Create share
./dfsctl share create --name /export --metadata default --block-store default

# Enable NFS adapter
./dfsctl adapter update nfs --enabled true --port 12049

# Mount NFS share
sudo mkdir -p /tmp/dittofs-test
sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049,nolock \
  localhost:/export /tmp/dittofs-test
```

## NFSv4 Testing

`dt` runs the same pjdfstest suite against NFSv3, NFSv4.0 and NFSv4.1 mounts, one cell each:
`pjdfstest/badger-s3/3`, `pjdfstest/badger-s3/4` and `pjdfstest/badger-s3/4.1`. By hand, mount
with the NFSv4 options below.

### NFSv4 Mount Differences

NFSv4 uses different mount options than NFSv3:

| Option | NFSv3 | NFSv4 |
|--------|-------|-------|
| Version | `nfsvers=3` | `vers=4.0` |
| Mount port | `mountport=PORT` | Not used (no separate mount protocol) |
| Locking | `nolock` (NLM disabled) | Not needed (integrated locking) |
| Caching | `noac,sync,lookupcache=none` | `noac,sync,lookupcache=none` |

### Known Failures

The known failures are in `known.yaml`; each entry's `where` gives the NFS versions it applies to. Key differences from NFSv3:

- **Locking**: NFSv4 has integrated locking (no NLM), so locking tests may behave differently
- **Extended attributes**: NFSv4 named attributes (OPENATTR) not implemented
- **ACLs**: NFSv4 ACLs are supported, but pjdfstest uses POSIX ACLs which are different
- **Timestamps**: NFSv4 supports 64-bit timestamps (no year-2106 limitation unlike NFSv3)
- **Error codes**: Some tests may get different NFS4ERR_* codes vs NFS3ERR_*

## Test Categories

The test suite includes these categories:
- `chmod` - Permission changes
- `chown` - Ownership changes
- `chflags` - File flags (FreeBSD-specific, most skipped on Linux)
- `ftruncate` - File truncation via file descriptor
- `granular` - Granular permission tests
- `link` - Hard links
- `mkdir` - Directory creation
- `mkfifo` - Named pipe creation
- `mknod` - Special file creation (block/char devices)
- `open` - File creation/opening
- `posix_fallocate` - Space allocation (skipped on NFS)
- `rename` - File/directory renaming
- `rmdir` - Directory removal
- `symlink` - Symbolic links
- `truncate` - File truncation
- `unlink` - File removal
- `utimensat` - Timestamp modification

## DittoFS Limitations

Some tests will fail or be skipped due to NFSv3/DittoFS limitations:

| Feature | Status | Notes |
|---------|--------|-------|
| ETXTBSY | Skip | NFS protocol limitation - server can't detect executing files |
| File locking | Skip | NLM protocol not implemented |
| Extended attributes | Skip | Not in NFSv3 base spec |
| ACLs | Skip | Requires NFSv4 |
| posix_fallocate | Skip | No ALLOCATE in NFSv3 |
| chflags | Skip | FreeBSD-specific |
| Special files | Pass | Metadata only (no device functionality) |

See [docs/KNOWN_LIMITATIONS.md](../../docs/KNOWN_LIMITATIONS.md) for detailed explanations of each limitation.

See `known.yaml` for the expected failures, each with its reason and the NFS versions it applies to.

## Files

```
test/posix/
├── README.md                # This file
├── known.yaml               # Expected failures with reasons, graded by dt
├── Dockerfile.pjdfstest     # pjdfstest container, for a mount made by hand
├── configs/                 # Configuration files
│   └── config.yaml          # Single config (paths set via env vars)
└── results/                 # Test results (not committed)
```

## CI Integration

`.github/workflows/tests.yml` runs every pjdfstest cell through `dt` on each pull request, after
each merge and nightly, with pjdfstest built by Nix at the `flake.lock` revision.

## References

- [pjdfstest (Rust)](https://github.com/saidsay-so/pjdfstest) - Rust rewrite of POSIX test tool (used by DittoFS)
- [pjdfstest (original)](https://github.com/pjd/pjdfstest) - Original Perl POSIX test tool
- [RFC 1813 - NFSv3](https://tools.ietf.org/html/rfc1813) - Protocol specification
