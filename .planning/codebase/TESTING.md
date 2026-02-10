# Testing Patterns

**Analysis Date:** 2026-02-09

## Test Framework

**Runner:**
- Go's standard `testing` package
- 107 test files across the codebase

**Assertion Library:**
- `github.com/stretchr/testify/assert` - For non-fatal assertions
- `github.com/stretchr/testify/require` - For fatal assertions (test stops on failure)
- Pattern: Use `require` for setup/preconditions, `assert` for behavior verification

**Run Commands:**
```bash
# Run all unit and integration tests
go test ./...

# Run with coverage
go test -cover ./...

# Run with race detection
go test -race ./...

# Run specific package
go test ./pkg/metadata/...

# Run with verbose output
go test -v ./...

# E2E tests (requires special build tag)
go test -tags=e2e -v ./test/e2e/...

# E2E tests with race detection
go test -tags=e2e -v -race -timeout 30m ./test/e2e/...

# E2E tests with Localstack (S3 support)
# See test/e2e/README.md for setup
sudo go test -tags=e2e -v ./test/e2e/... -run TestE2E
```

## Test File Organization

**Location:**
- Co-located with source code in same package
- Example: `pkg/cache/cache.go` alongside `pkg/cache/cache_test.go`
- Exception: E2E tests in `test/e2e/` directory with `//go:build e2e` tag

**Naming:**
- Standard: `*_test.go` suffix
- Test functions: `TestFunctionName_Scenario` pattern
- Examples:
  - `TestWrite_SimpleWrite` - Basic operation
  - `TestWrite_AtOffset` - Specific parameter
  - `TestWrite_SparseFile` - Edge case
  - `TestLookup_ExistingFile` - Success case
  - `TestLookup_NonExistentFile` - Error case

**Structure (by directory type):**

Unit/Integration Tests:
```
pkg/
├── cache/
│   ├── cache.go
│   ├── cache_test.go       # Tests for cache.go
│   ├── write.go
│   ├── write_test.go       # Tests for write.go
│   └── types.go            # No types_test.go (helper types only)
│
└── metadata/
    ├── service.go
    ├── service_test.go     # Tests for service.go
    └── store/
        ├── store.go
        ├── memory/
        │   ├── memory.go
        │   └── memory_test.go
```

E2E Tests:
```
test/
└── e2e/
    ├── main_test.go            # TestMain setup/teardown
    ├── users_test.go           # TestUserCRUD
    ├── groups_test.go          # TestGroupManagement
    ├── metadata_stores_test.go # TestMetadataStoresCRUD
    ├── framework/              # Test infrastructure
    │   ├── postgres.go         # PostgreSQL container setup
    │   └── localstack.go       # S3/Localstack setup
    └── helpers/                # Test utilities
        ├── server.go           # ServerProcess management
        ├── cli.go              # CLI runner
        └── unique.go           # Unique naming helpers
```

## Test Structure

**Test Setup Pattern:**

1. **Create fixture** (if reusable components needed):
```go
fx := handlertesting.NewHandlerFixture(t)  // t.Helper() called internally
defer fx.Close()  // Cleanup
```

2. **Setup data** (test preconditions):
```go
fileHandle := fx.CreateFile("testfile.txt", []byte("hello world"))
```

3. **Execute** (run the code being tested):
```go
req := &handlers.WriteRequest{
    Handle: fileHandle,
    Offset: 0,
    Count:  uint32(len(data)),
    Stable: 2,
    Data:   data,
}
resp, err := fx.Handler.Write(fx.ContextWithUID(0, 0), req)
```

4. **Verify** (assertions):
```go
require.NoError(t, err)                                    // Fatal assert
assert.EqualValues(t, types.NFS3OK, resp.Status)          // Non-fatal assert
assert.EqualValues(t, uint32(len(data)), resp.Count)
```

**Example from `write_test.go`:**
```go
// TestWrite_SimpleWrite tests writing to a file.
func TestWrite_SimpleWrite(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)  // Setup fixture

	// Setup: Create an empty file
	fileHandle := fx.CreateFile("testfile.txt", []byte{})

	// Execute: Write some data
	data := []byte("hello world")
	req := &handlers.WriteRequest{
		Handle: fileHandle,
		Offset: 0,
		Count:  uint32(len(data)),
		Stable: 2, // FILE_SYNC
		Data:   data,
	}
	resp, err := fx.Handler.Write(fx.ContextWithUID(0, 0), req)

	// Verify
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.EqualValues(t, uint32(len(data)), resp.Count)

	// Additional verification: read back written data
	readResp, err := fx.Handler.Read(fx.Context(), &handlers.ReadRequest{
		Handle: fileHandle,
		Offset: 0,
		Count:  100,
	})
	require.NoError(t, err)
	assert.Equal(t, data, readResp.Data)
}
```

**Subtests Pattern:**

```go
// Use t.Run for grouped related test cases
func TestGroupManagement(t *testing.T) {
	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)
	cli := helpers.LoginAsAdmin(t, sp.APIURL())

	t.Run("create group", func(t *testing.T) {
		testCreateGroup(t, cli)
	})

	t.Run("create group with description", func(t *testing.T) {
		testCreateGroupWithDescription(t, cli)
	})

	t.Run("list groups", func(t *testing.T) {
		testListGroups(t, cli)
	})
}
```

**Test Helper Methods:**

Fixtures use `t.Helper()` to mark helper functions:
```go
func (f *HandlerTestFixture) CreateFile(name string, content []byte) metadata.FileHandle {
	f.t.Helper()  // Mark as helper so test line numbers point to caller
	// ... implementation
}
```

## Mocking

**Framework:** No mocking framework used; real implementations preferred

**Approach:**
- Use in-memory implementations instead of mocks
- Examples:
  - `metadatamemory.NewMemoryMetadataStoreWithDefaults()` - Real in-memory store
  - `cache.New(0)` - Real in-memory cache
  - `storemem.New()` - Real in-memory block store

**Benefits:**
- Tests behavior contracts, not implementation details
- Catches integration issues between components
- More maintainable than mocked tests

**What to Mock:** Almost nothing; use real components

**What NOT to Mock:** Everything should use real implementations unless testing specific failures

**Example from test fixtures** (`internal/protocol/nfs/v3/handlers/testing/fixtures.go`):
```go
// Create real stores, not mocks
metaStore := metadatamemory.NewMemoryMetadataStoreWithDefaults()
testCache := cache.New(0) // Real in-memory cache
blockStore := storemem.New() // Real in-memory block store
transferMgr := transfer.New(testCache, blockStore, metaStore, transfer.DefaultConfig())

// Create handler with real components
handler := &handlers.Handler{
    Registry: reg,
}
```

## Fixtures and Factories

**Test Data Creation:**

1. **Handler Test Fixture** (`internal/protocol/nfs/v3/handlers/testing/fixtures.go`):
   - Creates complete test environment for NFS handler testing
   - Sets up metadata store, cache, block store, transfer manager
   - Provides helper methods: `CreateFile()`, `CreateDirectory()`, `Context()`
   - Usage:
```go
fx := handlertesting.NewHandlerFixture(t)
fileHandle := fx.CreateFile("file.txt", []byte("content"))
resp, err := fx.Handler.Read(fx.Context(), &handlers.ReadRequest{
    Handle: fileHandle,
    Offset: 0,
    Count:  100,
})
```

2. **E2E Test Environment** (`test/e2e/helpers/`):
   - `NewTestEnvironmentForMain()` - Creates shared test environment
   - `StartServerProcess(t, config)` - Starts real DittoFS server
   - `LoginAsAdmin(t, serverURL)` - Returns CLI runner with auth
   - `UniqueTestName(prefix)` - Generates unique test resource names
   - Usage:
```go
sp := helpers.StartServerProcess(t, "")
t.Cleanup(sp.ForceKill)
cli := helpers.LoginAsAdmin(t, sp.APIURL())
user, err := cli.CreateUser("testuser", "password")
```

3. **Temp Directories** (standard Go):
```go
dir := t.TempDir()  // Automatically cleaned up
walFile := filepath.Join(dir, "cache.dat")
```

## Coverage

**Requirements:** Not enforced by CI/linting, but important for critical paths

**View Coverage:**
```bash
# Generate coverage profile
go test -cover ./...

# Detailed coverage report
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out  # View in browser
```

**Test Coverage Patterns:**

Unit tests cover:
- Happy path (normal operation)
- Error cases (invalid input, not found, permission denied)
- Edge cases (zero bytes, sparse files, large files)
- Boundary conditions (min/max values)

Integration tests cover:
- Store behavior with real implementations
- Cache with WAL persistence
- Protocol handler compliance with RFC specifications

E2E tests cover:
- Complete workflows (create → read → write → delete)
- Cross-protocol scenarios (NFS and SMB)
- Permission enforcement
- Server lifecycle (start → operate → shutdown)

## Test Types

**Unit Tests:**
- Scope: Individual functions/methods in isolation
- Approach: Real in-memory components (no mocks)
- Speed: Fast (< 100ms per test)
- Examples:
  - `pkg/cache/cache_test.go` - Cache operations
  - `internal/protocol/nfs/v3/handlers/write_test.go` - WRITE handler compliance
  - `internal/protocol/nfs/v3/handlers/lookup_test.go` - LOOKUP handler compliance
- Location: Co-located with source files

**Integration Tests:**
- Scope: Multiple components working together
- Approach: Real stores (BadgerDB, S3, PostgreSQL) with test containers
- Speed: Medium (1-10s per test)
- Examples:
  - Cache + WAL persistence recovery
  - Metadata store + block store coordination
  - Handler + service layer integration
- Location: `test/integration/` (if separated) or alongside source

**E2E Tests:**
- Scope: Complete system workflows with real NFS/SMB mounts
- Approach: Real server process, actual protocol clients, Docker containers for external services
- Speed: Slow (10-60s per test)
- Examples:
  - User CRUD via CLI
  - File operations via NFS mount
  - Permission enforcement via SMB
  - Server lifecycle management
- Location: `test/e2e/` with `//go:build e2e` build tag
- Requirements:
  - NFS client installed (`mount`, `umount`)
  - macOS or Linux (tests mount real NFS)
  - Docker (for Localstack S3, PostgreSQL)
  - `sudo` access (for NFS mounts)

## Common Patterns

**Async/Context Testing:**

```go
// Test context cancellation
ctx, cancel := context.WithCancel(context.Background())
cancel()  // Cancel before operation
resp, err := fx.Handler.Write(ctx, req)
require.Error(t, err)  // Should fail due to cancelled context
```

**Error Testing:**

Use subtests to test different error conditions:
```go
t.Run("not found", func(t *testing.T) {
    req := &handlers.ReadRequest{
        Handle: []byte{}, // Invalid handle
        Offset: 0,
        Count:  100,
    }
    resp, err := fx.Handler.Read(fx.Context(), req)
    require.NoError(t, err)
    assert.EqualValues(t, types.NFS3ErrBadHandle, resp.Status)
})

t.Run("permission denied", func(t *testing.T) {
    // Create file as one user
    fx := handlertesting.NewHandlerFixture(t)
    fileHandle := fx.CreateFile("file.txt", []byte("content"))

    // Try to read as different user without permission
    resp, err := fx.Handler.Read(fx.ContextWithUID(1001, 1001), &handlers.ReadRequest{
        Handle: fileHandle,
        Offset: 0,
        Count:  100,
    })
    require.NoError(t, err)
    assert.EqualValues(t, types.NFS3ErrAccess, resp.Status)
})
```

**RFC Compliance Testing:**

Tests reference specific RFC sections with clear test names:
```go
// TestWrite_RFC1813 tests WRITE handler behaviors per RFC 1813 Section 3.3.7.
//
// WRITE is used to write data to a regular file. It supports:
// - Writing at any offset
// - Extending files beyond their current size
// - Different stability levels (UNSTABLE, DATA_SYNC, FILE_SYNC)
// - WCC data for cache consistency

// TestWrite_SimpleWrite tests writing to a file.
func TestWrite_SimpleWrite(t *testing.T) {
    // ...
}
```

**Cleanup Patterns:**

Use `t.Cleanup()` for test-specific cleanup:
```go
func TestUserManagement(t *testing.T) {
    sp := helpers.StartServerProcess(t, "")
    t.Cleanup(sp.ForceKill)  // Ensures server is killed even if test panics

    cli := helpers.LoginAsAdmin(t, sp.APIURL())

    username := "testuser"
    t.Cleanup(func() {
        _ = cli.DeleteUser(username)  // Clean up test data
    })

    user, err := cli.CreateUser(username, "password")
    require.NoError(t, err)
    // Test continues...
}
```

**Build Tags for Test Categories:**

E2E tests use build tag to separate from regular tests:
```go
//go:build e2e

package e2e

import (
	"testing"
	"github.com/marmos91/dittofs/test/e2e/helpers"
)

func TestUserCRUD(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping user CRUD tests in short mode")
	}
	// ... test implementation
}
```

Run regular tests:
```bash
go test ./...  # Excludes E2E tests
```

Run E2E tests:
```bash
go test -tags=e2e -v ./test/e2e/...
```

## Key Testing Packages

**testify:** `github.com/stretchr/testify`
- `assert` - Non-fatal assertions
- `require` - Fatal assertions (stops test on failure)

**testcontainers:** `github.com/testcontainers/testcontainers-go`
- Manages Docker containers for PostgreSQL, Localstack (S3)
- Shared containers across test suite for performance

**Internal test helpers:** `test/e2e/{framework,helpers}/`
- Server process management
- CLI runner with authentication
- Mount management and cleanup
- Unique test resource naming

---

*Testing analysis: 2026-02-09*
