# DittoFS End-to-End Tests

Comprehensive e2e tests for DittoFS that test real file operations through mounted NFS and SMB shares.

## Overview

The e2e tests validate DittoFS functionality by:
1. Starting a DittoFS server with specific metadata/content stores
2. Mounting shares via NFS and SMB protocols (both enabled by default)
3. Performing file operations using standard Go `os` package
4. Verifying results
5. Cleaning up (unmount, shutdown, cleanup)

## Quick Start

**Important:** E2E tests require `sudo` to mount NFS/SMB shares.

```bash
# Run all tests (Docker required for PostgreSQL/S3)
sudo go test -tags=e2e -v -timeout 30m ./test/e2e/...

# Local tests only (no Docker needed)
sudo go test -tags=e2e -v ./test/e2e/... -skip "S3|postgres"

# Run specific test
sudo go test -tags=e2e -v -run TestCRUD ./test/e2e/...

# Run with short mode (skip large file tests)
sudo go test -tags=e2e -v -short ./test/e2e/...

# Full CI suite with race detection
sudo go test -tags=e2e -v -race -timeout 45m ./test/e2e/...
```

## Test Structure

```
test/e2e/
├── framework/                 # Test infrastructure
│   ├── context.go            # Unified TestContext (NFS + SMB)
│   ├── config.go             # 8 configurations
│   ├── mount.go              # NFS/SMB mount helpers
│   ├── containers.go         # Localstack/PostgreSQL helpers
│   └── helpers.go            # Utility functions
│
├── functional_test.go        # CRUD, permissions, concurrent access
├── interop_v2_test.go        # NFS<->SMB, store interoperability
├── advanced_test.go          # Symlinks, hardlinks, special files
├── scale_test.go             # Large files (up to 1GB), many files
├── cache_v2_test.go          # Cache-specific tests
│
├── main_test.go              # Test lifecycle
└── README.md                 # This file
```

## Configuration Matrix

Tests run against 8 configurations:

| Name | Metadata | Content | Cache | Docker Required |
|------|----------|---------|-------|-----------------|
| `memory-memory` | Memory | Memory | No | No |
| `memory-filesystem` | Memory | Filesystem | No | No |
| `badger-filesystem` | BadgerDB | Filesystem | No | No |
| `memory-memory-cached` | Memory | Memory | Yes | No |
| `postgres-filesystem` | PostgreSQL | Filesystem | No | Yes |
| `memory-s3` | Memory | S3 | Yes | Yes |
| `badger-s3` | BadgerDB | S3 | Yes | Yes |
| `postgres-s3` | PostgreSQL | S3 | Yes | Yes |

## Available Tests

### Functional Tests (`functional_test.go`)

Core CRUD operations and concurrent access:

```bash
# CRUD operations
sudo go test -tags=e2e -v -run TestCRUD ./test/e2e/...

# Nested folder operations (20 levels deep)
sudo go test -tags=e2e -v -run TestNestedFolders ./test/e2e/...

# Bulk operations (create/edit/delete many files)
sudo go test -tags=e2e -v -run TestBulkOperations ./test/e2e/...

# Concurrent access from multiple goroutines
sudo go test -tags=e2e -v -run TestConcurrentAccess ./test/e2e/...

# File permissions
sudo go test -tags=e2e -v -run TestFilePermissions ./test/e2e/...

# Directory listing
sudo go test -tags=e2e -v -run TestListDirectory ./test/e2e/...
```

### Interoperability Tests (`interop_v2_test.go`)

Cross-protocol and cross-store consistency:

```bash
# Protocol interop (NFS write -> SMB read, etc.)
sudo go test -tags=e2e -v -run TestProtocolInteropV2 ./test/e2e/...

# Simultaneous access from both protocols
sudo go test -tags=e2e -v -run TestSimultaneousProtocolAccessV2 ./test/e2e/...

# Large file transfer between protocols
sudo go test -tags=e2e -v -run TestLargeFileInteropV2 ./test/e2e/...

# Store integration (metadata + content stores)
sudo go test -tags=e2e -v -run TestStoreInteropV2 ./test/e2e/...
```

### Advanced Tests (`advanced_test.go`)

Special file operations:

```bash
# Hard links (create, modify, delete original)
sudo go test -tags=e2e -v -run TestHardlinks ./test/e2e/...

# Symlinks (to files, directories, readlink)
sudo go test -tags=e2e -v -run TestSymlinks ./test/e2e/...

# Rename/move operations
sudo go test -tags=e2e -v -run TestRename ./test/e2e/...

# Special files (FIFOs, sockets, devices)
sudo go test -tags=e2e -v -run TestSpecialFiles ./test/e2e/...
```

### Scale Tests (`scale_test.go`)

Large file and many-file tests:

```bash
# Large files (1MB, 10MB, 100MB, 1GB)
sudo go test -tags=e2e -v -run TestLargeFiles ./test/e2e/...

# 1GB file test only (explicit)
sudo go test -tags=e2e -v -timeout 1h -run "TestLargeFiles/1GB" ./test/e2e/...

# Many files (100, 1000, 10000)
sudo go test -tags=e2e -v -run TestManyFiles ./test/e2e/...

# Many directories
sudo go test -tags=e2e -v -run TestManyDirectories ./test/e2e/...

# Deep nesting (50 levels)
sudo go test -tags=e2e -v -run TestDeepNesting ./test/e2e/...

# Mixed content (files + subdirectories)
sudo go test -tags=e2e -v -run TestMixedContent ./test/e2e/...
```

### Cache Tests (`cache_v2_test.go`)

Cache-specific behavior (runs only on cached configurations):

```bash
# Basic cache operations
sudo go test -tags=e2e -v -run TestCacheBasicOperations ./test/e2e/...

# Cache read hits
sudo go test -tags=e2e -v -run TestCacheReadHits ./test/e2e/...

# Cache coherence (invalidation on writes)
sudo go test -tags=e2e -v -run TestCacheCoherence ./test/e2e/...

# Cache with many files
sudo go test -tags=e2e -v -run TestCacheWithManyFiles ./test/e2e/...

# Cache flush
sudo go test -tags=e2e -v -run TestCacheFlush ./test/e2e/...

# Cache append
sudo go test -tags=e2e -v -run TestCacheAppend ./test/e2e/...
```

## Running Tests on Specific Configurations

```bash
# Run only on memory-memory configuration
sudo go test -tags=e2e -v -run "/memory-memory" ./test/e2e/...

# Run only on postgres configurations
sudo go test -tags=e2e -v -run "/postgres" ./test/e2e/...

# Run only on S3 configurations
sudo go test -tags=e2e -v -run "/s3" ./test/e2e/...

# Run a specific test on a specific configuration
sudo go test -tags=e2e -v -run "TestCRUD/badger-filesystem" ./test/e2e/...
```

## Writing New Tests

### Adding a Test to an Existing File

```go
func TestMyNewOperation(t *testing.T) {
    framework.RunOnAllConfigs(t, func(t *testing.T, tc *framework.TestContext) {
        // Your test code using tc.Path() for file paths
        filePath := tc.Path("myfile.txt")
        framework.WriteFile(t, filePath, []byte("content"))

        // Verify
        content := framework.ReadFile(t, filePath)
        if !bytes.Equal(content, []byte("content")) {
            t.Error("Content mismatch")
        }
    })
}
```

### Running Only on Local Configurations (No Docker)

```go
func TestLocalOnly(t *testing.T) {
    framework.RunOnLocalConfigs(t, func(t *testing.T, tc *framework.TestContext) {
        // Your test - runs on memory/filesystem/badger configs only
    })
}
```

### Running Only on Cached Configurations

```go
func TestCacheSpecific(t *testing.T) {
    framework.RunOnCachedConfigs(t, func(t *testing.T, tc *framework.TestContext) {
        if !tc.HasCache() {
            t.Skip("Cache not enabled")
        }
        // Your cache-specific test
    })
}
```

### Testing Cross-Protocol Operations

```go
func TestCrossProtocol(t *testing.T) {
    framework.RunOnAllConfigs(t, func(t *testing.T, tc *framework.TestContext) {
        if !tc.HasNFS() || !tc.HasSMB() {
            t.Skip("Both NFS and SMB required")
        }

        // Write via NFS
        framework.WriteFile(t, tc.NFSPath("file.txt"), []byte("content"))

        // Read via SMB
        content := framework.ReadFile(t, tc.SMBPath("file.txt"))
    })
}
```

## TestContext API

```go
// Path construction
tc.Path("file.txt")      // Default path (NFS by default)
tc.NFSPath("file.txt")   // Explicit NFS path
tc.SMBPath("file.txt")   // Explicit SMB path

// Protocol checks
tc.HasNFS()              // Always true (NFS always enabled)
tc.HasSMB()              // True when SMB is enabled (default: true)
tc.HasCache()            // True when cache is enabled

// Configuration access
tc.Config.Name           // Configuration name (e.g., "memory-memory")
tc.Config.MetadataStore  // MetadataStoreType
tc.Config.ContentStore   // ContentStoreType
```

## Framework Helpers

```go
// File operations
framework.WriteFile(t, path, content)
framework.ReadFile(t, path) []byte
framework.WriteRandomFile(t, path, size) string  // Returns checksum
framework.VerifyFileChecksum(t, path, checksum)

// Directory operations
framework.CreateDir(t, path)
framework.ListDir(t, path) []os.DirEntry
framework.CountFiles(t, path) int
framework.CountDirs(t, path) int

// Existence checks
framework.FileExists(path) bool
framework.DirExists(path) bool

// Cleanup
framework.RemoveAll(t, path)

// File info
framework.GetFileInfo(t, path) FileInfo

// Test skipping
framework.SkipIfShort(t, reason)
```

## CI Integration

```yaml
# .github/workflows/e2e.yml
jobs:
  e2e:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - name: Setup Go
        uses: actions/setup-go@v5
        with:
          go-version: '1.21'

      - name: Install NFS client
        run: sudo apt-get install -y nfs-common

      - name: Run E2E tests
        run: sudo go test -tags=e2e -v -timeout 30m ./test/e2e/...
```

## Prerequisites

### All Tests
- Go 1.21+
- NFS client utilities:
  - **macOS**: Built-in (no installation needed)
  - **Linux**: `sudo apt-get install nfs-common` (Debian/Ubuntu)
  - **Linux**: `sudo yum install nfs-utils` (RHEL/CentOS)

### Docker Tests (PostgreSQL/S3)
- Docker (testcontainers automatically manages containers)
- No docker-compose needed

## Performance Considerations

| Configuration | Speed | Use Case |
|---------------|-------|----------|
| memory-memory | Fastest | Rapid development |
| memory-filesystem | Fast | More realistic |
| badger-filesystem | Medium | Tests persistence |
| memory-memory-cached | Fast | Cache behavior |
| postgres-filesystem | Slower | Distributed metadata |
| *-s3 | Slowest | S3-specific testing |

Typical test times:
- Single operation test: 100-500ms
- File size test (1MB): 200-1000ms
- File size test (100MB): 2-10s
- Full suite (local): 2-5 minutes
- Full suite (all configs): 5-15 minutes

## Troubleshooting

### Tests fail with "permission denied" on mount
- E2E tests require `sudo`
- Ensure NFS client is installed
- On macOS, ensure `resvport` option is used (automatically handled)

### Docker tests fail or are skipped
- Ensure Docker is running
- testcontainers will auto-start PostgreSQL/Localstack
- Check Docker has sufficient resources

### Tests timeout
- Increase timeout: `sudo go test -tags=e2e -v -timeout 60m ./test/e2e/...`
- Large files (100MB+) can be slow on some systems
- Use `-short` flag to skip large file tests

### Stuck NFS/SMB mounts
```bash
# List mounts
mount | grep -E "(nfs|smb|cifs)"

# Force unmount NFS
sudo umount -f /path/to/mount

# Force unmount SMB (macOS)
sudo umount -f /path/to/mount
```

### Run specific configuration for debugging
```bash
# Just the fast memory-memory config
sudo go test -tags=e2e -v -run "/memory-memory" ./test/e2e/...
```

## License

Same as DittoFS main project.
