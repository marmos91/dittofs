# POSIX Compliance Testing for DittoFS

This directory contains POSIX compliance testing for DittoFS using [pjdfstest](https://github.com/saidsay-so/pjdfstest) (Rust rewrite).

## Overview

The test suite validates DittoFS's NFSv3 implementation against POSIX filesystem semantics using the Rust rewrite of pjdfstest, which provides better performance and maintainability than the original Perl version.

## Quick Start

### Automated Setup (Recommended)

The setup script handles starting the server, configuring stores/shares via API, and mounting:

```bash
# Build binaries
go build -o dfs ./cmd/dfs
go build -o dfsctl ./cmd/dfsctl

# Setup with memory metadata store (default)
sudo ./test/posix/setup-posix.sh

# Or use a different store type
sudo ./test/posix/setup-posix.sh badger
sudo ./test/posix/setup-posix.sh postgres  # requires running postgres

# Run POSIX tests
cd /tmp/dittofs-test
sudo env PATH="$PATH" ./test/posix/run-posix.sh

# Run specific test category
sudo env PATH="$PATH" ./test/posix/run-posix.sh chmod

# Teardown when done
sudo ./test/posix/teardown-posix.sh
```

### Store Types

The setup script supports different storage backend combinations:

| Store Type | Metadata Store | Payload Store | Requirements |
|------------|----------------|---------------|--------------|
| `memory` | In-memory | Filesystem | None (default) |
| `badger` | BadgerDB | Filesystem | None |
| `postgres` | PostgreSQL | Filesystem | Running PostgreSQL |
| `memory-content` | In-memory | In-memory | None |
| `cache-s3` | In-memory | S3 | Running Localstack |

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
./dfsctl store metadata add --name default --type memory
./dfsctl store payload add --name default --type filesystem \
  --config '{"path":"/tmp/dfs-content"}'

# Create share
./dfsctl share create --name /export --metadata default --payload default

# Enable NFS adapter
./dfsctl adapter update nfs --enabled true --port 12049

# Mount NFS share
sudo mkdir -p /tmp/dittofs-test
sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049,nolock \
  localhost:/export /tmp/dittofs-test

# Run tests
cd /tmp/dittofs-test
sudo env PATH="$PATH" ../test/posix/run-posix.sh
```

### macOS with Docker

On macOS, run pjdfstest in Docker:

```bash
# Setup DittoFS and mount
sudo ./test/posix/setup-posix.sh

# Build pjdfstest container
docker build -t dittofs-pjdfstest -f test/posix/Dockerfile.pjdfstest .

# Run all tests
docker run --rm -v /tmp/dittofs-test:/mnt/test dittofs-pjdfstest

# Run specific test category
docker run --rm -v /tmp/dittofs-test:/mnt/test dittofs-pjdfstest chmod

# Cleanup
sudo ./test/posix/teardown-posix.sh
```

### Linux with Nix

On Linux, pjdfstest is available directly in the Nix development environment:

```bash
# Enter nix shell
nix develop

# Setup DittoFS
sudo ./test/posix/setup-posix.sh

# Use helper commands
dittofs-posix                          # Run all tests
dittofs-posix chmod                    # Run chmod tests only
dittofs-posix chmod chown              # Run multiple test categories

# Cleanup
sudo ./test/posix/teardown-posix.sh
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
├── setup-posix.sh         # Automated setup script
├── teardown-posix.sh      # Cleanup script
├── run-posix.sh           # Test runner
├── Dockerfile.pjdfstest   # pjdfstest container (for macOS/Docker)
├── known_failures.txt     # Expected failures with reasons
├── configs/               # Configuration files
│   └── config.yaml        # Single config (paths set via env vars)
└── results/               # Test results (not committed)
```

## CI Integration

Tests run automatically via GitHub Actions on Linux using the Nix environment.

## References

- [pjdfstest (Rust)](https://github.com/saidsay-so/pjdfstest) - Rust rewrite of POSIX test tool (used by DittoFS)
- [pjdfstest (original)](https://github.com/pjd/pjdfstest) - Original Perl POSIX test tool
- [RFC 1813 - NFSv3](https://tools.ietf.org/html/rfc1813) - Protocol specification
