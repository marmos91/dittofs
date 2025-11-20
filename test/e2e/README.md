# DittoFS End-to-End Tests

Simplified, modular e2e tests for DittoFS that test real file operations through mounted NFS shares.

## Overview

The e2e tests validate DittoFS functionality by:
1. Starting a DittoFS server with specific metadata/content stores
2. Mounting the NFS share
3. Performing file operations using standard Go `os` package
4. Verifying results
5. Cleaning up (unmount, shutdown, cleanup)

## Quick Start

**⚠️  Important:** E2E tests require `sudo` to mount NFS shares on both macOS and Linux.

### Standard Tests (No External Dependencies)

```bash
# Run all standard tests (requires sudo)
cd test/e2e
sudo ./run-e2e.sh

# Or directly with go test
sudo go test -v ./...

# Run a specific test
sudo go test -v -run TestCreateFolder
```

### S3 Tests (Requires Localstack)

```bash
# Run all tests including S3 (requires sudo + Docker)
sudo ./run-e2e.sh --s3

# The script will automatically:
# 1. Start Localstack via docker-compose
# 2. Wait for it to be ready
# 3. Run all tests
# 4. Clean up Localstack
```

## Test Structure

```
test/e2e/
├── framework.go           # Server lifecycle and mount management
├── config.go              # Configuration matrix (metadata × content stores)
├── localstack.go          # Localstack S3 integration
├── operations_test.go     # Individual operation tests
├── filesize_test.go       # Parametrized file size tests
├── docker-compose.yml     # Localstack setup
├── run-e2e.sh            # Test orchestration script
└── README.md             # This file
```

## Configuration Matrix

Tests run against all combinations of:

### Metadata Stores
- **Memory**: Fast, in-memory metadata (no persistence)
- **Badger**: Embedded key-value store (persistent)

### Content Stores
- **Memory**: Fast, in-memory content storage
- **Filesystem**: Local filesystem storage
- **S3**: S3-compatible storage (via Localstack)

### Available Configurations

**Standard (no external dependencies):**
- `memory/memory` - Both metadata and content in memory
- `memory/filesystem` - Memory metadata, filesystem content
- `badger/filesystem` - Badger metadata, filesystem content

**S3 (requires Localstack):**
- `memory/s3` - Memory metadata, S3 content
- `badger/s3` - Badger metadata, S3 content

## Available Tests

### Operation Tests (`operations_test.go`)

Individual tests that can be run separately:

```bash
# Test individual operations
go test -v -run TestCreateFolder
go test -v -run TestCreateNestedFolders
go test -v -run TestCreateEmptyFile
go test -v -run TestCreateEmptyFilesInNestedFolders
go test -v -run TestDeleteSingleFile
go test -v -run TestDeleteAllFolders
go test -v -run TestDeleteAllFiles
go test -v -run TestEditFile
go test -v -run TestEditMultipleFiles
go test -v -run TestUnmount
```

**What each test does:**

- `TestCreateFolder` - Creates a single folder
- `TestCreateNestedFolders` - Creates 20 nested folders (folder1/folder2/.../folder20)
- `TestCreateEmptyFile` - Creates a single empty file
- `TestCreateEmptyFilesInNestedFolders` - Creates 20 nested folders, each with an empty file
- `TestDeleteSingleFile` - Creates and deletes a single file
- `TestDeleteAllFolders` - Creates 10 folders and deletes them all
- `TestDeleteAllFiles` - Creates 20 files and deletes them all
- `TestEditFile` - Creates a file and overwrites it with new content
- `TestEditMultipleFiles` - Creates 20 files and edits them all
- `TestUnmount` - Tests unmounting and remounting the share

### File Size Tests (`filesize_test.go`)

Parametrized tests for different file sizes:

```bash
# Test specific file sizes
go test -v -run TestCreateFile_500KB
go test -v -run TestCreateFile_1MB
go test -v -run TestCreateFile_10MB
go test -v -run TestCreateFile_100MB

# Test all sizes
go test -v -run TestCreateFilesBySize

# Test multiple files of each size
go test -v -run TestCreateMultipleFilesBySize

# Test read operations
go test -v -run TestReadFilesBySize

# Test write-then-read
go test -v -run TestWriteThenReadBySize

# Test overwrite operations
go test -v -run TestOverwriteFilesBySize

# Test delete operations
go test -v -run TestDeleteFilesBySize
```

**Available file sizes:**
- 500KB
- 1MB
- 10MB
- 100MB

### Running Tests on Specific Configurations

```bash
# Run only on memory/memory configuration
go test -v -run "TestCreateFolder/memory-memory"

# Run only on badger/filesystem configuration
go test -v -run "TestCreateFile_1MB/badger-filesystem"

# Run all tests on S3 configurations
./run-e2e.sh --s3 --test "s3"
```

## Script Usage

The `run-e2e.sh` script provides convenient test orchestration:

```bash
# Basic usage
./run-e2e.sh                              # Standard tests only
./run-e2e.sh --s3                         # Include S3 tests
./run-e2e.sh --verbose                    # Verbose output
./run-e2e.sh --test TestCreateFile_1MB   # Run specific test
./run-e2e.sh --keep-localstack            # Keep Localstack running after tests

# Combined options
./run-e2e.sh --s3 --verbose --test TestCreateFilesBySize
```

**Script options:**
- `--s3` - Run S3 tests (starts Localstack automatically)
- `--verbose`, `-v` - Enable verbose test output
- `--test`, `-t NAME` - Run specific test by name
- `--keep-localstack` - Keep Localstack running after tests (for debugging)
- `--help`, `-h` - Show help message

## Localstack Setup

### Using the Script (Recommended)

```bash
# Script handles everything automatically
./run-e2e.sh --s3
```

### Manual Setup

```bash
# Start Localstack
docker-compose up -d

# Wait for it to be ready
curl http://localhost:4566/_localstack/health

# Set environment variable
export LOCALSTACK_ENDPOINT=http://localhost:4566

# Run tests
go test -v ./...

# Stop Localstack
docker-compose down -v
```

### Troubleshooting Localstack

```bash
# Check if Localstack is running
docker ps | grep localstack

# View Localstack logs
docker-compose logs -f

# Check health
curl http://localhost:4566/_localstack/health

# Restart Localstack
docker-compose down -v
docker-compose up -d
```

## Writing New Tests

### Adding a New Operation Test

1. Open `operations_test.go`
2. Add your test function:

```go
func TestMyNewOperation(t *testing.T) {
    runOnAllConfigs(t, func(t *testing.T, tc *TestContext) {
        // Your test code using tc.Path() for file paths
        filePath := tc.Path("myfile.txt")
        err := os.WriteFile(filePath, []byte("content"), 0644)
        if err != nil {
            t.Fatalf("Failed: %v", err)
        }
    })
}
```

### Adding a New File Size Test

1. Open `filesize_test.go`
2. Define a new size (if needed):

```go
var Size5MB = FileSize{Name: "5MB", Bytes: 5 * 1024 * 1024}
```

3. Add to `StandardFileSizes` or create a custom test:

```go
func TestCreateFile_5MB(t *testing.T) {
    runOnAllConfigs(t, func(t *testing.T, tc *TestContext) {
        testCreateFileOfSize(t, tc, Size5MB)
    })
}
```

### Running Only on Specific Configurations

```go
// Run only on S3 configurations
func TestS3SpecificFeature(t *testing.T) {
    runOnS3Configs(t, func(t *testing.T, tc *TestContext) {
        // Your S3-specific test
    })
}
```

## TestContext API

The `TestContext` provides helper methods:

```go
// Path construction
tc.Path("file.txt")                      // Absolute path in mount

// Access to configuration
tc.Config.MetadataStore                  // "memory" or "badger"
tc.Config.ContentStore                   // "memory", "filesystem", or "s3"

// Access to mount point
tc.MountPath                             // Absolute path to mount

// Access to stores (for advanced tests)
tc.MetadataStore
tc.ContentStore
```

## Common Patterns

### Create and Verify File

```go
filePath := tc.Path("test.txt")
content := []byte("test content")

// Write
err := os.WriteFile(filePath, content, 0644)
if err != nil {
    t.Fatalf("Failed to write: %v", err)
}

// Read back
actualContent, err := os.ReadFile(filePath)
if err != nil {
    t.Fatalf("Failed to read: %v", err)
}

// Verify
if !bytes.Equal(content, actualContent) {
    t.Errorf("Content mismatch")
}
```

### Create Folder Structure

```go
basePath := tc.Path("base")
err := os.MkdirAll(filepath.Join(basePath, "sub1", "sub2"), 0755)
if err != nil {
    t.Fatalf("Failed to create folders: %v", err)
}
```

### List Directory

```go
entries, err := os.ReadDir(tc.Path("folder"))
if err != nil {
    t.Fatalf("Failed to read dir: %v", err)
}

for _, entry := range entries {
    t.Logf("Found: %s (dir=%v)", entry.Name(), entry.IsDir())
}
```

## CI Integration

The tests are designed to run in CI with minimal setup:

```yaml
# .github/workflows/e2e.yml
- name: Run E2E tests (standard)
  run: |
    cd test/e2e
    ./run-e2e.sh

- name: Run E2E tests (with S3)
  run: |
    cd test/e2e
    ./run-e2e.sh --s3
```

## Prerequisites

### All Tests
- Go 1.21+
- NFS client utilities:
  - **macOS**: Built-in (no installation needed)
  - **Linux**: `sudo apt-get install nfs-common` (Debian/Ubuntu)
  - **Linux**: `sudo yum install nfs-utils` (RHEL/CentOS)

### S3 Tests
- Docker
- docker-compose

## Performance Considerations

- **Memory/Memory**: Fastest, use for rapid development
- **Memory/Filesystem**: Fast, more realistic
- **Badger/Filesystem**: More realistic, tests persistence
- **S3 Configurations**: Slower due to Localstack overhead, use for S3-specific testing

Typical test times:
- Single operation test: 100-500ms
- File size test (1MB): 200-1000ms
- File size test (100MB): 2-10s (depending on store)
- Full suite (standard): 2-5 minutes
- Full suite (with S3): 5-15 minutes

## Tips

1. **Use specific tests during development**: `./run-e2e.sh --test TestCreateFile_1MB`
2. **Keep Localstack running**: `./run-e2e.sh --s3 --keep-localstack` (then run tests repeatedly with `go test`)
3. **Verbose output for debugging**: `./run-e2e.sh --verbose`
4. **Test on specific config**: `go test -v -run "TestName/memory-filesystem"`

## Troubleshooting

### Tests fail with "permission denied" on mount
- Ensure NFS client is installed
- Check that you're not using a reserved port (tests use >1024)
- On macOS, ensure `resvport` option is used (automatically handled)

### Localstack tests are skipped
- Ensure Localstack is running: `docker ps | grep localstack`
- Check endpoint: `curl http://localhost:4566/_localstack/health`
- Use the script: `./run-e2e.sh --s3`

### Tests timeout
- Increase timeout: `go test -timeout 60m ./...`
- Large files (100MB) can be slow on some systems

### Stuck NFS mounts
```bash
# List mounts
mount | grep dittofs

# Force unmount
sudo umount -f /path/to/mount
```

## Future Enhancements

- [ ] Concurrent client tests (multiple mounts)
- [ ] Stress tests (thousands of operations)
- [ ] Network failure simulation
- [ ] Performance benchmarks (throughput, latency)
- [ ] Cross-platform NFS client testing

## License

Same as DittoFS main project.
