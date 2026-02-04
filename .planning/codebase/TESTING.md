# Testing Patterns

**Analysis Date:** 2026-02-04

## Test Framework

**Runner:**
- Go 1.25.0's built-in `testing` package
- No external test framework (not using testify/suite or others)
- Config: No special test configuration file

**Assertion Library:**
- `github.com/stretchr/testify` (assert, require packages)
- `require` for assertions that should fail the test
- `assert` for soft assertions (continues testing)

**Run Commands:**
```bash
# Run all unit and integration tests
go test ./...

# Run with coverage
go test -cover ./...

# Run with race detection
go test -race ./...

# Run E2E tests (requires sudo and NFS client)
sudo go test -tags=e2e -v ./test/e2e/...

# Run specific E2E test
sudo go test -tags=e2e -v -run TestNFSFileOperations ./test/e2e/

# Run specific configuration (all combinations in E2E)
sudo go test -tags=e2e -v -run "TestCreateFile/memory-filesystem" ./test/e2e/
```

## Test File Organization

**Location:**
- Co-located pattern: `*_test.go` in same package as code
- Unit tests test individual components
- Integration tests in subdirectories (`test/integration/`, `test/e2e/`)

**Naming:**
- Test files: `{module}_test.go` (e.g., `service_test.go`, `cache_test.go`)
- Test functions: `Test{FunctionName}` (e.g., `TestWrite_SimpleWrite`, `TestCache_WalPersistence`)
- Subtests use `t.Run()` with descriptive names

**Structure:**
```
pkg/
├── metadata/
│   ├── service.go
│   ├── service_test.go          # Tests for MetadataService
│   ├── store.go
│   ├── errors.go
│   └── errors_test.go           # Tests for error types

test/
├── integration/                 # Tests requiring backend setup
│   └── *_test.go
└── e2e/                        # Tests requiring NFS mount
    ├── main_test.go            # TestMain setup
    └── *_test.go
```

## Test Structure

**Suite Organization:**
Tests in DittoFS use table-driven tests and subtests, not test suites.

**Example Pattern** from `test/e2e/metadata_stores_test.go`:
```go
//go:build e2e

package e2e

import (
	"testing"
	"github.com/stretchr/testify/require"
)

// Comprehensive test with sequential and parallel subtests
func TestMetadataStoresCRUD(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping in short mode")
	}

	// Start server once, shared by all subtests
	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	cli := helpers.LoginAsAdmin(t, sp.APIURL())

	// Parallel subtests for independent operations
	t.Run("create memory store", func(t *testing.T) {
		t.Parallel()

		storeName := helpers.UniqueTestName("meta_mem")
		t.Cleanup(func() {
			_ = cli.DeleteMetadataStore(storeName)
		})

		store, err := cli.CreateMetadataStore(storeName, "memory")
		require.NoError(t, err, "Should create memory metadata store")
		assert.Equal(t, storeName, store.Name)
	})

	t.Run("create badger store", func(t *testing.T) {
		t.Parallel()
		// ...
	})
}
```

**Pattern Details:**
- One function per test scenario
- `t.Run()` for logical grouping of related tests
- `t.Parallel()` for independent subtests (must not share mutable state)
- `t.Cleanup()` for resource cleanup (deferred automatically)
- `testing.Short()` for quick vs. comprehensive test modes

## Fixtures and Factories

**Test Data:**

Handler tests use `HandlerTestFixture` from `internal/protocol/nfs/v3/handlers/testing/fixtures.go`:

```go
type HandlerTestFixture struct {
	t *testing.T

	// Handler is the NFS v3 handler under test.
	Handler *handlers.Handler

	// Registry manages stores and shares.
	Registry *runtime.Runtime

	// MetadataService provides high-level metadata operations.
	MetadataService *metadata.MetadataService

	// ContentService provides high-level content operations.
	ContentService *payload.PayloadService

	// ShareName is the name of the test share.
	ShareName string

	// RootHandle is the file handle for the share's root directory.
	RootHandle metadata.FileHandle
}

// Create fixture with real memory stores
fx := handlertesting.NewHandlerFixture(t)

// Use in tests
fileHandle := fx.CreateFile("testfile.txt", []byte{})
readResp, err := fx.Handler.Read(fx.Context(), &handlers.ReadRequest{
	Handle: fileHandle,
	Offset: 0,
	Count:  100,
})
```

**Fixture Usage Example** from `internal/protocol/nfs/v3/handlers/write_test.go`:
```go
func TestWrite_SimpleWrite(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create an empty file
	fileHandle := fx.CreateFile("testfile.txt", []byte{})

	// Write some data
	data := []byte("hello world")
	req := &handlers.WriteRequest{
		Handle: fileHandle,
		Offset: 0,
		Count:  uint32(len(data)),
		Stable: 2, // FILE_SYNC
		Data:   data,
	}
	resp, err := fx.Handler.Write(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.EqualValues(t, uint32(len(data)), resp.Count)
}
```

**Location:**
- Handler test fixtures: `internal/protocol/nfs/v3/handlers/testing/fixtures.go`
- E2E test helpers: `test/e2e/helpers/`
- Framework utilities: `test/e2e/framework/`

## Mocking

**Framework:** No dedicated mocking library

**Pattern:** Use real implementations instead of mocks
- Tests use real memory stores (fast, in-memory)
- Tests use real cache implementations
- Avoids testing implementation details

**Example** from `internal/protocol/nfs/v3/handlers/testing/fixtures.go`:
```go
// Create real memory stores, not mocks
metaStore := metadatamemory.NewMemoryMetadataStoreWithDefaults()
blockStore := storemem.New()
testCache := cache.New(0) // 0 = unlimited size

// Use real services
metadataSvc := metadata.New()
metadataSvc.RegisterStoreForShare(DefaultShareName, metaStore)

payloadSvc, err := payload.New(testCache, transferMgr)
require.NoError(t, err)
```

**What to Mock:**
- External network services (S3 via Localstack in E2E tests)
- Time-dependent operations (use `time.Now()` wrapper if needed)

**What NOT to Mock:**
- Core services (MetadataService, PayloadService)
- Store implementations (use memory stores instead)
- Protocol handlers (test with real fixtures)

## Coverage

**Requirements:** No explicit coverage targets enforced

**View Coverage:**
```bash
# Generate coverage report
go test -coverprofile=coverage.out ./...

# Display coverage
go tool cover -html=coverage.out

# Show coverage summary by package
go tool cover -func=coverage.out
```

## Test Types

**Unit Tests:**
- Scope: Individual functions, services, stores
- Location: Alongside code in `*_test.go` files
- Speed: Fast (< 100ms typically)
- Dependencies: Real memory stores, no external services
- Example: `pkg/cache/cache_test.go` tests cache operations with real data

**Integration Tests:**
- Scope: Multiple components working together
- Location: `test/integration/`
- Speed: Medium (seconds)
- Dependencies: Real backends (S3 via Localstack, BadgerDB, PostgreSQL via testcontainers)
- Focus: Store backend integration, not protocol behavior

**E2E Tests:**
- Scope: Complete workflows via actual protocol
- Location: `test/e2e/`
- Speed: Slow (minutes for full suite)
- Setup: Real NFS/SMB mounts, server process, protocol clients
- Coverage: All backend combinations (memory, badger, filesystem, S3)
- Build tag: `//go:build e2e` - excluded from normal test runs

## Common Patterns

**Async Testing:**
Not common in this codebase. Tests typically use synchronous operations.
Where async occurs:
- Use `ctx := context.Background()` for blocking operations
- Use `time.After()` for timeouts in race conditions
- Wait for completion before assertions

**Error Testing:**

Example from `internal/protocol/nfs/v3/handlers/write_test.go`:
```go
func TestWrite_AccessDenied(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup with no-owner file
	fileHandle := fx.CreateFile("readonly.txt", []byte{})
	// ... make it read-only

	// Test that write fails with permission error
	req := &handlers.WriteRequest{
		Handle: fileHandle,
		// ... write request
	}
	resp, err := fx.Handler.Write(fx.ContextWithUID(1000, 1000), req)

	// For expected errors, use require.Error instead of require.NoError
	require.Error(t, err)
	// Or check specific error code
	assert.EqualValues(t, types.NFS3ErrAccess, resp.Status)
}
```

**Table-Driven Tests:**

Example from cache tests:
```go
func TestWriteAtVariousSizes(t *testing.T) {
	testCases := []struct {
		name        string
		dataSize    int
		offset      uint64
		expectErr   bool
	}{
		{"small write", 10, 0, false},
		{"large write", 1024 * 1024, 0, false},
		{"write at offset", 100, 4096, false},
		{"write beyond block", 100, 4*1024*1024 + 100, false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			c := cache.New(0)
			data := make([]byte, tc.dataSize)

			err := c.WriteAt(ctx, payloadID, tc.offset, data)

			if tc.expectErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
```

**File Operations Testing:**

Protocol handler tests verify RFC 1813 compliance:

Example from `internal/protocol/nfs/v3/handlers/read_test.go`:
```go
// TestRead_RFC1813 tests READ handler behaviors per RFC 1813 Section 3.3.6.
//
// READ is used to read data from a regular file. It supports:
// - Reading at any offset
// - Reading beyond EOF (returns partial data with eof=true)
// - Efficient range queries for sparse files
// - WCC data for cache consistency

func TestRead_SimpleRead(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create file with known content
	content := []byte("hello world")
	fileHandle := fx.CreateFile("testfile.txt", content)

	// Read entire file
	req := &handlers.ReadRequest{
		Handle: fileHandle,
		Offset: 0,
		Count:  100,
	}
	resp, err := fx.Handler.Read(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.Equal(t, content, resp.Data)
	assert.False(t, resp.Eof)
}
```

**E2E Server Process Testing:**

Example from `test/e2e/file_operations_nfs_test.go`:
```go
func TestNFSFileOperations(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping E2E tests in short mode")
	}

	// Start server (automatically killed on cleanup)
	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	// Create shares via CLI
	runner := helpers.LoginAsAdmin(t, sp.APIURL())
	metaStoreName := helpers.UniqueTestName("meta")
	payloadStoreName := helpers.UniqueTestName("payload")

	_, err := runner.CreateMetadataStore(metaStoreName, "memory")
	require.NoError(t, err)
	t.Cleanup(func() { _ = runner.DeleteMetadataStore(metaStoreName) })

	// Mount NFS and test operations
	mount := framework.MountNFS(t, nfsPort)
	t.Cleanup(mount.Cleanup)

	// Test file operations via actual filesystem
	testFile := filepath.Join(mount.MountPoint, "test.txt")
	err = os.WriteFile(testFile, []byte("test"), 0644)
	require.NoError(t, err)

	data, err := os.ReadFile(testFile)
	require.NoError(t, err)
	assert.Equal(t, []byte("test"), data)
}
```

**Cache Persistence Testing:**

Example from `pkg/cache/cache_test.go`:
```go
func TestCache_WalPersistence(t *testing.T) {
	dir := t.TempDir()

	// Create cache with WAL persistence
	c := newTestCacheWithWal(t, dir, 0)
	ctx := context.Background()

	// Write data
	data := []byte("persistent data")
	if err := c.WriteAt(ctx, payloadID, 0, data, 0); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// Verify WAL file exists
	walFile := filepath.Join(dir, "cache.dat")
	if _, err := os.Stat(walFile); os.IsNotExist(err) {
		t.Fatal("WAL file should exist")
	}

	// Close and recover
	if err := c.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Reopen cache and verify data recovered
	c2 := newTestCacheWithWal(t, dir, 0)
	defer func() { _ = c2.Close() }()

	result := make([]byte, len(data))
	found, err := c2.ReadAt(ctx, payloadID, 0, 0, uint32(len(data)), result)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, data, result)
}
```

## E2E Test Features

**Build Tag:**
```go
//go:build e2e

package e2e
```
- Excluded from `go test ./...`
- Included only when explicitly requested with `-tags=e2e`

**Global Test Environment:**
- `TestMain()` handles setup and cleanup in `test/e2e/main_test.go`
- Lazy container initialization on first test that needs them
- Signal handlers for graceful shutdown on CTRL+C
- Stale mount cleanup (important on macOS)

**Available Test Configurations:**
- `memory/memory` - Both metadata and content in memory
- `memory/filesystem` - Memory metadata, filesystem content
- `badger/filesystem` - BadgerDB metadata, filesystem content
- `memory/s3` - Memory metadata, S3 content (requires Localstack)
- `badger/s3` - BadgerDB metadata, S3 content (requires Localstack)

**Test Isolation:**
- `env.NewScope(t)` for per-test isolation
- Unique PostgreSQL schema per test
- Unique S3 prefix per test
- Unique server port per test

**Running Specific Configurations:**
```bash
# Run all memory backend tests
sudo go test -tags=e2e -v -run "TestE2E/memory" ./test/e2e/

# Run all BadgerDB tests
sudo go test -tags=e2e -v -run "TestE2E/badger" ./test/e2e/

# Run all S3 tests (requires Localstack)
sudo go test -tags=e2e -v -run "TestE2E/s3" ./test/e2e/

# Run specific file size test
sudo go test -tags=e2e -v -run "TestCreateFile_10MB" ./test/e2e/

# Run with race detection
sudo go test -tags=e2e -v -race -timeout 30m ./test/e2e/...
```

## Helper Patterns

**Server Process Management** (`test/e2e/helpers/`):
```go
// Start server in background
sp := helpers.StartServerProcess(t, "")
t.Cleanup(sp.ForceKill)

// Get API URL for CLI/HTTP calls
apiURL := sp.APIURL()

// CLI runner with authentication
runner := helpers.LoginAsAdmin(t, apiURL)

// Create stores via CLI
runner.CreateMetadataStore(name, storetype)
runner.CreatePayloadStore(name, storetype)
runner.CreateShare(sharePath, metastoreName, payloadstoreName)

// Enable adapters
runner.EnableAdapter("nfs", helpers.WithAdapterPort(port))

// Disable adapters
runner.DisableAdapter("nfs")
```

**Framework Utilities** (`test/e2e/framework/`):
```go
// Mount NFS
mount := framework.MountNFS(t, port)
t.Cleanup(mount.Cleanup)
mount.MountPoint // Use this path for file operations

// Wait for server
framework.WaitForServer(t, port, timeout)

// Cleanup stale mounts
framework.CleanupStaleMounts()
```

## Common Test Mistakes

1. **Ignoring resource cleanup** - Always use `t.Cleanup()` for mounts, processes, databases
2. **Not using `require` for setup** - Setup errors should fail fast with `require.NoError()`
3. **Testing implementation details** - Test behavior (RFC compliance), not internal structure
4. **Not using table-driven tests** - Use for multiple similar test cases
5. **Blocking on COMMIT in tests** - E2E tests properly use non-blocking flush patterns
6. **Not isolating parallel tests** - Use `t.Parallel()` only if subtest state is isolated
7. **Assuming deterministic timing** - Don't rely on sleep durations, use `WaitFor()` helpers

---

*Testing analysis: 2026-02-04*
