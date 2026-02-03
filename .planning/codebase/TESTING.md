# Testing Patterns

**Analysis Date:** 2026-02-02

## Test Framework

**Runner:**
- Go's built-in `testing` package (no external test framework)
- Tests execute via `go test ./...`
- Parallel execution enabled by default: `t.Parallel()`

**Assertion Library:**
- `github.com/stretchr/testify` v1.11.1
- `assert` package for non-fatal assertions (test continues on failure)
- `require` package for fatal assertions (test stops on failure)

**Run Commands:**
```bash
# Run all unit and integration tests
go test ./...

# Run with verbose output
go test -v ./...

# Watch mode (requires external tool)
# Use standard Go test runner without special flags

# Coverage report
go test -cover ./...
go test -coverprofile=coverage.out ./...
go test -html=coverage.out ./...

# Race detection
go test -race ./...

# Run specific package
go test ./pkg/metadata/

# Run specific test
go test -run TestMetadataService_RegisterStoreForShare ./pkg/metadata/

# Run tests matching pattern
go test -run "TestMetadataService" ./pkg/metadata/

# Run with timeout
go test -timeout 10m ./...

# Run integration tests only (have //go:build integration)
go test -tags=integration ./...

# Run E2E tests only (have //go:build e2e)
go test -tags=e2e ./...
```

## Test File Organization

**Location:**
- Co-located with source: `*.go` files paired with `*_test.go` files in same directory
- Special cases:
  - Integration tests: `*_integration_test.go` (require `//go:build integration`)
  - E2E tests: All in `test/e2e/` directory (require `//go:build e2e` or e2e build tag)

**Naming:**
- Unit tests: `{source}_test.go` (e.g., `service_test.go`, `cache_test.go`)
- Integration tests: `{component}_integration_test.go` (e.g., `badger_integration_test.go`)
- E2E tests: Any `*_test.go` in `test/e2e/` with appropriate build tags

**Build Tags:**
```go
// Integration tests (requires Docker/Localstack for S3)
//go:build integration

// E2E tests (requires NFS mount capabilities and sudo)
//go:build e2e

// No tag = runs in `go test ./...`
```

**File Structure Pattern:**

```go
package metadata_test

import (
    "context"
    "testing"

    "github.com/marmos91/dittofs/pkg/metadata"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

// ============================================================================
// Test Fixtures (Section header)
// ============================================================================

// testFixture provides a configured service for testing.
type testFixture struct {
    t       *testing.T
    service *metadata.MetadataService
    store   *memory.MemoryMetadataStore
}

func newTestFixture(t *testing.T) *testFixture {
    t.Helper()  // Exclude this function from test call stack
    // ... setup code
}

// ============================================================================
// Test Suite for Function (Section header)
// ============================================================================

func TestFunction_Description(t *testing.T) {
    t.Parallel()

    t.Run("sub-case 1", func(t *testing.T) {
        t.Parallel()
        // Test code
    })

    t.Run("sub-case 2", func(t *testing.T) {
        t.Parallel()
        // Test code
    })
}
```

## Test Structure

**Suite Organization:**
Each test function typically has one or more subtests using `t.Run()`:

```go
func TestMetadataService_RegisterStoreForShare(t *testing.T) {
    t.Parallel()

    t.Run("registers store successfully", func(t *testing.T) {
        t.Parallel()
        svc := metadata.New()
        store := memory.NewMemoryMetadataStoreWithDefaults()

        err := svc.RegisterStoreForShare("/test", store)

        require.NoError(t, err)
    })

    t.Run("rejects nil store", func(t *testing.T) {
        t.Parallel()
        svc := metadata.New()

        err := svc.RegisterStoreForShare("/test", nil)

        require.Error(t, err)
        assert.Contains(t, err.Error(), "nil store")
    })
}
```

**Patterns:**

**1. Arrange-Act-Assert (Setup-Execute-Verify):**
```go
func TestWrite_SimpleWrite(t *testing.T) {
    fx := handlertesting.NewHandlerFixture(t)

    // Arrange: Setup test state
    fileHandle := fx.CreateFile("testfile.txt", []byte{})

    // Act: Execute the code being tested
    data := []byte("hello world")
    resp, err := fx.Handler.Write(fx.ContextWithUID(0, 0), &handlers.WriteRequest{
        Handle: fileHandle,
        Offset: 0,
        Count:  uint32(len(data)),
        Data:   data,
    })

    // Assert: Verify results
    require.NoError(t, err)
    assert.EqualValues(t, types.NFS3OK, resp.Status)
}
```

**2. Fixture Pattern:**
Create reusable test fixtures for complex setups:

```go
// testFixture provides a configured MetadataService with a memory store for testing.
type testFixture struct {
    t          *testing.T
    service    *metadata.MetadataService
    store      *metadata.MemoryMetadataStore
    shareName  string
    rootHandle metadata.FileHandle
}

func newTestFixture(t *testing.T) *testFixture {
    t.Helper()
    // ... setup
    return &testFixture{...}
}

// Helper methods on fixture
func (f *testFixture) authContext(uid, gid uint32) *metadata.AuthContext {
    return &metadata.AuthContext{
        Context:    context.Background(),
        AuthMethod: "unix",
        Identity:   &metadata.Identity{UID: uid, GID: gid},
        ClientAddr: "127.0.0.1",
    }
}
```

**3. Table-Driven Tests:**
For testing multiple input combinations:

```go
func TestNewNotFoundError(t *testing.T) {
    t.Parallel()

    tests := []struct {
        name       string
        path       string
        entityType string
        wantMsg    string
    }{
        {"file not found", "/path/to/file.txt", "file", "file not found: /path/to/file.txt"},
        {"directory not found", "/path/to/dir", "directory", "directory not found: /path/to/dir"},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            t.Parallel()
            err := NewNotFoundError(tt.path, tt.entityType)

            assert.Equal(t, tt.wantMsg, err.Error())
        })
    }
}
```

## Mocking

**Framework:**
- No mocking library used in core tests
- Uses real memory stores instead of mocks
- Reason: Memory stores are deterministic, testable, and prevent mock brittleness

**Approach:**
```go
// PREFERRED: Use real in-memory implementations
store := memory.NewMemoryMetadataStoreWithDefaults()
svc := metadata.New()
svc.RegisterStoreForShare("/test", store)

// NOT: No gomock or similar - too coupled to implementation
```

**When to NOT Mock:**
- Metadata stores: Use real `memory.MemoryMetadataStore` instead
- Block stores: Use real `storemem.New()` for most tests
- Services: Inject real implementations, not mocks

**Integration Test Setup Pattern:**
When testing with Docker/Localstack:

```go
type localstackHelper struct {
    container testcontainers.Container
    endpoint  string
    client    *s3.Client
}

func newLocalstackHelper(t *testing.T) *localstackHelper {
    t.Helper()
    ctx := context.Background()

    // Check for external Localstack
    if endpoint := os.Getenv("LOCALSTACK_ENDPOINT"); endpoint != "" {
        return &localstackHelper{endpoint: endpoint}
    }

    // Start container
    req := testcontainers.ContainerRequest{
        Image:        "localstack/localstack:3.0",
        ExposedPorts: []string{"4566/tcp"},
        Env: map[string]string{
            "SERVICES": "s3",
        },
        WaitingFor: wait.ForAll(
            wait.ForListeningPort("4566/tcp"),
            wait.ForHTTP("/_localstack/health").WithPort("4566/tcp"),
        ),
    }

    container, err := testcontainers.GenericContainer(ctx,
        testcontainers.GenericContainerRequest{
            ContainerRequest: req,
            Started:          true,
        })
    if err != nil {
        t.Fatalf("failed to start localstack: %v", err)
    }

    // Extract host:port and create client...
    return &localstackHelper{container: container, endpoint: fmt.Sprintf("http://%s:%s", host, port)}
}
```

## Fixtures and Factories

**Test Data Pattern:**
The `testFixture` pattern provides factory methods for creating test data:

```go
// HandlerTestFixture in internal/protocol/nfs/v3/handlers/testing/fixtures.go
type HandlerTestFixture struct {
    t                *testing.T
    Handler          *handlers.Handler
    Registry         *runtime.Runtime
    MetadataService  *metadata.MetadataService
    ContentService   *payload.PayloadService
    ShareName        string
    RootHandle       metadata.FileHandle
}

// Factory method: NewHandlerFixture(t)
func NewHandlerFixture(t *testing.T) *HandlerTestFixture {
    t.Helper()
    // ... full setup with real stores
    return &HandlerTestFixture{...}
}

// Helper methods for creating test data
func (f *HandlerTestFixture) CreateFile(name string, data []byte) metadata.FileHandle {
    // Creates a file and returns handle
}

func (f *HandlerTestFixture) CreateDirectory(name string) metadata.FileHandle {
    // Creates a directory and returns handle
}

func (f *HandlerTestFixture) Context() context.Context {
    // Returns request context
}

func (f *HandlerTestFixture) ContextWithUID(uid, gid uint32) *metadata.AuthContext {
    // Returns auth context with specific user
}
```

**Location:**
- Fixtures in `testdata/` subdirectories or test file itself
- Shared test helpers in `testing/` subdirectories (e.g., `pkg/metadata/store/memory/testing/`)
- Handler test fixtures in `internal/protocol/nfs/v3/handlers/testing/fixtures.go`

## Coverage

**Requirements:**
- No hard coverage threshold enforced
- Recommendations:
  - Business logic: 80%+ coverage
  - Protocol handlers: 70%+ coverage (some error paths hard to trigger)
  - Internal utilities: 60%+ coverage

**View Coverage:**
```bash
# Generate coverage report
go test -coverprofile=coverage.out ./...

# View in terminal
go tool cover -func=coverage.out

# View in browser (HTML)
go tool cover -html=coverage.out -o coverage.html
open coverage.html
```

**Coverage Gaps to Prioritize:**
- Error paths (disk full, I/O errors, etc.)
- Concurrent access patterns
- State transitions (pending → uploading → uploaded)
- Protocol compliance (RFC 1813 edge cases)

## Test Types

**Unit Tests:**
- Location: Co-located `*_test.go` files
- Scope: Single function or method
- Dependencies: Use real implementations (no mocks)
- Speed: Milliseconds
- Example: `pkg/metadata/service_test.go` tests `MetadataService` methods
- Run with: `go test ./...`

**Integration Tests:**
- Location: `*_integration_test.go` with `//go:build integration`
- Scope: Multiple components working together (store + service + handler)
- Dependencies: Real S3 (via Localstack), BadgerDB, filesystem
- Speed: Seconds
- Examples:
  - `pkg/metadata/store/badger/badger_integration_test.go` - BadgerDB store integration
  - `pkg/payload/store/blockstore_integration_test.go` - Block store integrations
- Run with: `go test -tags=integration ./...`

**E2E Tests:**
- Location: `test/e2e/` with `//go:build e2e`
- Scope: Full NFS server with real kernel NFS client
- Dependencies: Real NFS mount, sudo privileges
- Speed: Minutes
- Examples:
  - `test/e2e/functional_test.go` - File operations (create, read, write, delete)
  - `test/e2e/advanced_test.go` - Advanced scenarios (permissions, hard links)
  - `test/e2e/scale_test.go` - Large file handling
- Run with:
  ```bash
  sudo go test -tags=e2e -v ./test/e2e/ -timeout 30m
  # Or using provided script:
  cd test/e2e && sudo ./run-e2e.sh
  ```

## Common Patterns

**Async Testing (Contexts & Timeouts):**
```go
func TestFlushWithTimeout(t *testing.T) {
    t.Parallel()

    // Create context with timeout
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    // Use context in async operation
    result, err := payloadSvc.Flush(ctx, payloadID)

    require.NoError(t, err)
    assert.True(t, result.Finalized)
}
```

**Error Testing Pattern:**
```go
func TestNotFoundError(t *testing.T) {
    t.Parallel()

    t.Run("returns error for missing file", func(t *testing.T) {
        t.Parallel()
        store := memory.NewMemoryMetadataStoreWithDefaults()

        _, err := store.GetFile(context.Background(), invalidHandle)

        require.Error(t, err)
        assert.True(t, metadata.IsNotFoundError(err))
    })

    t.Run("error message includes path", func(t *testing.T) {
        t.Parallel()
        err := metadata.NewNotFoundError("/path/to/file", "file")

        assert.Contains(t, err.Error(), "/path/to/file")
    })
}
```

**Cleanup Pattern:**
```go
func TestWithCleanup(t *testing.T) {
    t.Parallel()

    dir := t.TempDir()  // Automatically cleaned up after test
    persister, err := wal.NewMmapPersister(dir)
    require.NoError(t, err)

    c := cache.NewWithWal(1<<30, persister)
    defer func() {
        _ = c.Close()  // Graceful cleanup
    }()

    // Test code
}
```

**Concurrent Access Testing:**
```go
func TestConcurrentAccess(t *testing.T) {
    t.Parallel()

    store := memory.NewMemoryMetadataStoreWithDefaults()
    var wg sync.WaitGroup

    // Multiple goroutines accessing store
    for i := 0; i < 10; i++ {
        wg.Add(1)
        go func(id int) {
            defer wg.Done()
            file, err := store.CreateFile(context.Background(), rootHandle,
                fmt.Sprintf("file-%d", id), fileAttr)
            require.NoError(t, err)
            assert.NotNil(t, file)
        }(i)
    }

    wg.Wait()
}
```

## Test Helpers

**t.Helper() Usage:**
Always include `t.Helper()` at the start of helper functions:

```go
func (f *testFixture) authContext(uid, gid uint32) *metadata.AuthContext {
    return &metadata.AuthContext{  // Missing t.Helper() - test call stack will show this line
        ...
    }
}

// BETTER:
func (f *testFixture) authContext(uid, gid uint32) *metadata.AuthContext {
    f.t.Helper()  // Excludes this function from test call stack
    return &metadata.AuthContext{  // Call stack now shows caller of authContext()
        ...
    }
}
```

**Assertion Helper Patterns:**

```go
// Use require for fatal assertions (stops test immediately)
require.NoError(t, err)          // Stop test on error
require.NotNil(t, result)        // Stop test if nil
require.Equal(t, expected, got)  // Stop test if not equal

// Use assert for non-fatal assertions (test continues)
assert.EqualValues(t, types.NFS3OK, resp.Status)  // Soft fail
assert.Contains(t, err.Error(), "expected text")   // Soft fail
```

## Build Tags

**Integration Tests:**
```go
//go:build integration

package store_test

import "testing"

// This test only runs with: go test -tags=integration ./...
func TestS3Integration(t *testing.T) {
    // Uses Localstack container
}
```

**E2E Tests:**
```go
//go:build e2e

package e2e

import "testing"

// This test only runs with: go test -tags=e2e ./... or sudo ./run-e2e.sh
// Requires: NFS client, mount capabilities, sudo
func TestCreateFile_1MB(t *testing.T) {
    // Real NFS mount testing
}
```

## Performance Testing

**Benchmarking Pattern:**
Located in `pkg/cache/benchmark_test.go`:

```go
func BenchmarkCache_SequentialWrite(b *testing.B) {
    c := cache.New(0)

    buf := make([]byte, 32<<10)  // 32KB buffer
    for _, v := range buf {
        v = byte(rand.Intn(256))
    }

    b.ReportAllocs()
    b.ResetTimer()

    for i := 0; i < b.N; i++ {
        _ = c.WriteAt(context.Background(), "payload", 0, buf, 0)
    }
}
```

**Run Benchmarks:**
```bash
# Run all benchmarks
go test -bench=. -benchmem ./pkg/cache/

# Run specific benchmark with CPU count
go test -bench=BenchmarkCache_SequentialWrite -benchtime=10s ./pkg/cache/

# Save results
go test -bench=. -benchmem -benchstat ./pkg/cache/
```

## Special Directives

**Skipping Tests:**
```go
func TestExpensiveOperation(t *testing.T) {
    if testing.Short() {
        t.Skip("Skipping expensive test in short mode")
    }
    // Long-running test
}

// Run with: go test -short ./...
```

**Timeout Control:**
```go
// Global timeout for all tests
go test -timeout 30m ./...

// Per-test timeout handled via context
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()
```

---

*Testing analysis: 2026-02-02*
