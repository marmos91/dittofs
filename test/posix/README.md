# POSIX Compliance Testing for DittoFS

This directory contains POSIX compliance testing for DittoFS using [pjdfstest](https://github.com/saidsay-so/pjdfstest) (Rust rewrite).

## Overview

The test suite validates DittoFS's NFSv3 implementation against POSIX filesystem semantics using the Rust rewrite of pjdfstest, which provides better performance and maintainability than the original Perl version.

## Quick Start

### macOS

On macOS, run DittoFS and mount NFS natively, then run pjdfstest in Docker:

```bash
# Terminal 1: Build and run DittoFS
go build -o dittofs cmd/dittofs/main.go
./dittofs init
./dittofs start

# Terminal 2: Mount NFS share
sudo mkdir -p /tmp/dittofs-test
sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049,resvport,nolock \
  localhost:/export /tmp/dittofs-test

# Terminal 2: Build and run pjdfstest container
docker build -t dittofs-pjdfstest -f test/posix/Dockerfile.pjdfstest .

# Run all tests
docker run --rm -v /tmp/dittofs-test:/mnt/test dittofs-pjdfstest

# Run specific test category
docker run --rm -v /tmp/dittofs-test:/mnt/test dittofs-pjdfstest chmod

# Cleanup
sudo umount /tmp/dittofs-test
```

### Linux with Nix

On Linux, pjdfstest is available directly in the Nix development environment.

**Option 1: Using helper commands (recommended)**

```bash
# Terminal 1: Build and run DittoFS (as regular user)
nix develop
go build -o dittofs cmd/dittofs/main.go
./dittofs init
./dittofs start

# Terminal 2: Enter nix shell and use helpers
nix develop

# Use helper commands
dittofs-mount                          # Mount NFS to /tmp/dittofs-test
dittofs-posix                          # Run all tests
dittofs-posix chmod                    # Run chmod tests only
dittofs-posix chmod chown              # Run multiple test categories
dittofs-umount                         # Unmount
```

**Option 2: Root nix shell (manual)**

```bash
# Terminal 2: Enter nix shell as root for testing
sudo nix develop

# Mount and run tests
mkdir -p /tmp/dittofs-test
mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049,nolock \
  localhost:/export /tmp/dittofs-test

cd /tmp/dittofs-test

# Run all tests using pjdfstest binary directly
pjdfstest -c /path/to/pjdfstest.toml -p .

# Run specific test category
pjdfstest -c /path/to/pjdfstest.toml -p . chmod

# Cleanup
umount /tmp/dittofs-test
```

### Linux with Docker

Alternatively, use Docker on Linux (same as macOS):

```bash
docker build -t dittofs-pjdfstest -f test/posix/Dockerfile.pjdfstest .
docker run --rm -v /tmp/dittofs-test:/mnt/test dittofs-pjdfstest
```

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

See `known_failures.txt` for expected test failures with reasons.

## Files

```
test/posix/
├── README.md              # This file
├── Dockerfile.pjdfstest   # pjdfstest container (for macOS/Docker)
├── known_failures.txt     # Expected failures with reasons
└── results/               # Test results (not committed)
```

## CI Integration

Tests run automatically via GitHub Actions on Linux using the Nix environment.

## References

- [pjdfstest (Rust)](https://github.com/saidsay-so/pjdfstest) - Rust rewrite of POSIX test tool (used by DittoFS)
- [pjdfstest (original)](https://github.com/pjd/pjdfstest) - Original Perl POSIX test tool
- [RFC 1813 - NFSv3](https://tools.ietf.org/html/rfc1813) - Protocol specification
