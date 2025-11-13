# DittoFS End-to-End Tests

This directory contains comprehensive end-to-end (e2e) tests for DittoFS that verify functionality by actually mounting the NFS server and performing real file operations.

## Overview

The e2e test suite validates the complete DittoFS stack:

```
┌─────────────────────────────┐
│    Test Client (Go)         │
│    os.ReadFile, etc.        │
└──────────┬──────────────────┘
           │
           ▼ (mounted NFS)
┌─────────────────────────────┐
│    NFS Facade               │
└──────────┬──────────────────┘
           │
           ▼
┌─────────────────────────────┐
│    Metadata Store           │
│    (Memory)                 │
└─────────────────────────────┘
           │
           ▼
┌─────────────────────────────┐
│    Content Store            │
│    (Memory or Filesystem)   │
└─────────────────────────────┘
```

## Test Matrix

Tests run against all combinations of:

- **Facades**: NFS (currently; SMB, WebDAV planned)
- **Content Stores**: Memory, Filesystem

Each test suite is executed against each combination to ensure consistent behavior across all backends.

## Test Structure

```
test/e2e/
├── framework/              # Test infrastructure
│   ├── server.go          # DittoFS server lifecycle management
│   ├── mount.go           # NFS mount/unmount helpers
│   └── helpers.go         # Test utilities and assertions
├── suites/                # Test suites
│   ├── basic.go           # Basic CRUD operations
│   ├── directory.go       # Directory operations
│   ├── symlink.go         # Symbolic link operations
│   ├── hardlink.go        # Hard link operations
│   ├── attributes.go      # File attributes and metadata
│   ├── idempotency.go     # Idempotency testing
│   └── edgecases.go       # Edge cases and boundary conditions
├── matrix_test.go         # Main test runner
└── README.md              # This file
```

## Test Suites

### Basic Operations (`basic.go`)
- File creation, reading, writing, deletion
- Empty files
- Large files (1MB+)
- Binary files
- Files with special characters in names
- Append operations
- Overwrite operations

### Directory Operations (`directory.go`)
- Directory creation (mkdir, mkdir -p)
- Directory listing (readdir)
- Nested directory structures
- Directory deletion (empty and non-empty)
- Directory renaming
- Moving files between directories
- Deep nesting

### Symlink Operations (`symlink.go`)
- Symlink creation (to files and directories)
- Absolute and relative symlinks
- Dangling symlinks
- Symlink deletion
- Symlink renaming
- Chained symlinks
- Symlink loops

### Hard Link Operations (`hardlink.go`)
- Hard link creation
- Modifications through hard links
- Hard link deletion
- Multiple hard links to same file
- Hard links across directories
- Hard link permissions

### File Attributes (`attributes.go`)
- File size
- File permissions (chmod)
- File timestamps (mtime, atime)
- File type detection
- Truncate operations
- File extension

### Idempotency (`idempotency.go`)
- Repeated operations produce consistent results
- Creating same file multiple times
- Reading same file multiple times
- Deleting non-existent files
- Rename to same name
- Recreate deleted files

### Edge Cases (`edgecases.go`)
- Empty filenames
- Very long filenames (up to 255 bytes)
- Dot files (hidden files)
- Zero-size files
- Concurrent reads
- Non-existent file operations
- Removing open files
- Deep directory nesting
- Many files in one directory

## Running Tests

### Prerequisites

**Linux:**
```bash
# Install NFS client utilities
sudo apt-get install nfs-common  # Debian/Ubuntu
sudo yum install nfs-utils        # RHEL/CentOS
```

**macOS:**
```bash
# NFS client is built-in, no installation needed
```

### Run All E2E Tests

```bash
# Run all e2e tests
go test -v ./test/e2e/...

# Run with timeout (recommended for CI)
go test -v -timeout 30m ./test/e2e/...
```

### Run Specific Test Suite

```bash
# Run only basic operations tests
go test -v ./test/e2e -run TestE2E/memory/BasicOperations

# Run only filesystem store tests
go test -v ./test/e2e -run TestE2E/filesystem

# Run only symlink tests across all stores
go test -v ./test/e2e -run TestE2E/.*/SymlinkOperations
```

### Run with Verbose Logging

```bash
# Enable server logging
go test -v ./test/e2e -run TestE2E
```

To see detailed DittoFS server logs, modify the test to set `LogLevel: "DEBUG"` in `framework/helpers.go:NewTestContext()`.

## How It Works

### 1. Server Lifecycle

Each test context:
1. Creates a temporary directory
2. Starts a DittoFS server with a free port
3. Configures the specified content store (memory or filesystem)
4. Waits for server to be ready

### 2. Mount Process

Platform-specific mount commands:

**Linux:**
```bash
mount -t nfs -o nfsvers=3,tcp,port=<PORT>,nolock localhost:/export /tmp/mount-XXXXX
```

**macOS:**
```bash
mount -t nfs -o nfsvers=3,tcp,port=<PORT> localhost:/export /tmp/mount-XXXXX
```

### 3. Test Execution

Tests use standard Go `os` package functions:
- `os.ReadFile()`, `os.WriteFile()`
- `os.Mkdir()`, `os.ReadDir()`
- `os.Symlink()`, `os.Link()`
- `os.Stat()`, `os.Chmod()`

This ensures we're testing real NFS client behavior.

### 4. Cleanup

After each test:
1. Unmount the NFS filesystem
2. Stop the DittoFS server
3. Remove temporary directories

## Writing New Tests

To add new tests:

1. **Create a new suite file** in `suites/`:
   ```go
   package suites

   func TestMyNewFeature(t *testing.T, storeType framework.StoreType) {
       ctx := framework.NewTestContext(t, storeType)
       defer ctx.Cleanup()

       t.Run("TestCase1", func(t *testing.T) {
           // Use ctx helper methods
           ctx.WriteFile("test.txt", []byte("data"), 0644)
           ctx.AssertFileContent("test.txt", []byte("data"))
       })
   }
   ```

2. **Register in matrix_test.go**:
   ```go
   t.Run("MyNewFeature", func(t *testing.T) {
       suites.TestMyNewFeature(t, storeType)
   })
   ```

3. **Use helper methods** from `TestContext`:
   - `ctx.Path(relativePath)` - Get absolute path
   - `ctx.WriteFile()` - Write file
   - `ctx.ReadFile()` - Read file
   - `ctx.AssertFileExists()` - Assert file exists
   - `ctx.AssertFileContent()` - Assert file content

## Helper Methods

### Assertions
- `AssertFileExists(path)` - File must exist
- `AssertFileNotExists(path)` - File must not exist
- `AssertFileContent(path, expected)` - Content must match
- `AssertDirExists(path)` - Directory must exist
- `AssertIsSymlink(path)` - Must be a symlink

### File Operations
- `WriteFile(path, content, mode)` - Write file
- `ReadFile(path)` - Read file
- `Remove(path)` - Remove file/empty dir
- `RemoveAll(path)` - Remove recursively

### Directory Operations
- `Mkdir(path, mode)` - Create directory
- `MkdirAll(path, mode)` - Create with parents
- `ReadDir(path)` - List directory

### Link Operations
- `Symlink(target, link)` - Create symlink
- `Readlink(link)` - Read symlink target
- `Link(target, link)` - Create hard link

### Metadata Operations
- `Stat(path)` - Get file info
- `Lstat(path)` - Get file info (no follow symlinks)
- `Rename(old, new)` - Rename/move file

## Continuous Integration

The e2e tests run automatically on:
- Every push to `main` or `develop`
- Every pull request
- Both Linux and macOS runners

See `.github/workflows/build.yml` for the complete CI configuration.

### CI Test Flow

```yaml
1. Build and Test (unit tests)
   └─> Ubuntu + macOS

2. E2E Tests
   ├─> Ubuntu
   │   ├─> Install nfs-common
   │   ├─> Run e2e tests
   │   └─> Upload logs on failure
   └─> macOS
       ├─> Run e2e tests (NFS built-in)
       └─> Upload logs on failure
```

## Troubleshooting

### Tests fail with "mount: permission denied"

DittoFS uses non-privileged ports (>1024) by default, so no root is needed. If you see permission errors:

1. **Check if port is free**: The test framework automatically finds a free port
2. **Check NFS client is installed**: See prerequisites above
3. **Check mount command exists**: `which mount`

### Tests timeout

E2E tests can be slow, especially on macOS. Increase timeout:

```bash
go test -v -timeout 60m ./test/e2e/...
```

### Mount fails on Linux

Ensure `nfs-common` is installed:

```bash
sudo apt-get install nfs-common
```

### Tests pass locally but fail in CI

Check CI logs for:
1. NFS utilities installation
2. Mount command output
3. Server startup logs

Logs are uploaded as artifacts on failure.

### Cleaning up stuck mounts

If a test crashes and leaves a mount:

```bash
# List mounts
mount | grep dittofs

# Force unmount
umount -f /tmp/dittofs-mount-XXXXX  # Linux
umount -f /tmp/dittofs-mount-XXXXX  # macOS
```

## Performance Considerations

E2E tests are slower than unit tests because they:
- Start a real server
- Perform actual mount operations
- Do real I/O through NFS protocol

Typical timing:
- Test suite setup: ~500ms
- Individual test: 10-100ms
- Full matrix (2 stores × 7 suites): ~5-10 minutes

## Future Enhancements

- [ ] Add SMB facade tests when implemented
- [ ] Add WebDAV facade tests when implemented
- [ ] Add concurrent client tests (multiple mounts)
- [ ] Add stress tests (large number of operations)
- [ ] Add network failure simulation
- [ ] Add quota testing
- [ ] Add permission/ACL testing with different users
- [ ] Add NFSv4 tests when implemented

## License

Same as DittoFS main project.
