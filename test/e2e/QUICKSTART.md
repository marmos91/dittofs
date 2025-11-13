# E2E Tests Quick Start

## TL;DR

```bash
# Run all e2e tests
go test -v ./test/e2e/...

# Run with timeout (recommended)
go test -v -timeout 30m ./test/e2e/...
```

## What Gets Tested

The e2e tests verify DittoFS by:
1. Starting a real DittoFS server
2. Mounting it via NFS (no root needed - uses non-privileged ports)
3. Running file operations using standard Go `os` package
4. Testing against both **memory** and **filesystem** content stores

## Test Coverage

- ✅ Basic file operations (create, read, write, delete, append)
- ✅ Directory operations (mkdir, readdir, rmdir, deep nesting)
- ✅ Symbolic links (create, read, chains, dangling links)
- ✅ Hard links (create, modify through links, delete)
- ✅ File attributes (permissions, timestamps, size, truncate)
- ✅ Idempotency (repeated operations are safe)
- ✅ Edge cases (empty files, large files, long names, many files)

## Prerequisites

**Linux:**
```bash
sudo apt-get install nfs-common
```

**macOS:**
```bash
# NFS client is built-in, nothing to install
```

## Run Specific Tests

```bash
# Test only memory store
go test -v ./test/e2e -run TestE2E/memory

# Test only filesystem store
go test -v ./test/e2e -run TestE2E/filesystem

# Test only basic operations
go test -v ./test/e2e -run TestE2E/.*/BasicOperations

# Test only symlinks
go test -v ./test/e2e -run TestE2E/.*/SymlinkOperations
```

## CI/CD

Tests automatically run on:
- ✅ Linux (ubuntu-latest)
- ✅ macOS (macos-latest)

On every push and pull request.

## Troubleshooting

**Mount fails?**
- Check NFS client is installed (see prerequisites)
- Tests use non-privileged ports, no root needed
- Check `mount` command exists: `which mount`

**Tests timeout?**
- Increase timeout: `go test -v -timeout 60m ./test/e2e/...`
- macOS can be slower than Linux

**Stuck mounts?**
- Force unmount: `umount -f /tmp/dittofs-mount-*`

## Architecture

```
Test (Go os package)
    ↓ (real NFS mount)
NFS Facade
    ↓
Metadata Store (memory)
    ↓
Content Store (memory OR filesystem)
```

Each test runs against both content store types to ensure consistency.

See [README.md](README.md) for complete documentation.
