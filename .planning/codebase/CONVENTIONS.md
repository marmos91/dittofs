# Coding Conventions

**Analysis Date:** 2026-02-02

## Naming Patterns

**Packages:**
- All lowercase, single word where possible
- Multi-word packages use full word names without abbreviation
- Examples: `metadata`, `payload`, `cache`, `transfer`, `controlplane`

**Files:**
- Lowercase with underscores for multi-word names
- Suffix pattern: `_test.go` for unit tests, `_integration_test.go` for integration tests
- Examples: `service.go`, `cache_test.go`, `flush_test.go`, `write_test.go`

**Types (Structs & Interfaces):**
- PascalCase, descriptive full names
- Service types end with `Service`: `MetadataService`, `PayloadService`
- Store types end with `Store`: `MetadataStore`, `BlockStore`, `MemoryMetadataStore`, `BadgerMetadataStore`
- Configuration types end with `Config`: `MetadataServerConfig`, `BadgerMetadataStoreConfig`
- Interface types typically don't have suffix but name the protocol/capability: `MetadataServiceInterface`, `ProtocolAdapter`
- Examples from codebase:
  - `pkg/metadata/service.go`: `MetadataService`, `MetadataStore`, `LockManager`
  - `pkg/cache/cache.go`: `Cache`, `BlockState`, `PendingBlock`, `Stats`
  - `pkg/payload/store/store.go`: `BlockStore` (interface)

**Functions:**
- PascalCase for exported functions
- Descriptive action verbs: `Get*`, `Set*`, `Create*`, `Delete*`, `Update*`
- Examples: `GetStoreForShare()`, `RegisterStoreForShare()`, `CreateFile()`, `DeleteFile()`
- Handler functions follow pattern: `Handle{Verb}` or just the verb
  - NFS v3 handlers: `Read()`, `Write()`, `Lookup()`, `Create()`, `Mkdir()`

**Variables:**
- camelCase for local variables
- snake_case for constants that are package-level configs
- Short names acceptable in tight loops: `i`, `j`, `buf`, `err`
- Longer names for package state: `currentLevel`, `currentFormat`, `globalPool`
- Examples:
  - `pkg/metadata/service.go`: `mu`, `stores`, `lockManagers`, `deferredCommit`
  - `internal/logger/logger.go`: `currentLevel`, `currentFormat`, `handler`, `slogger`

**Constants:**
- ALL_CAPS with underscores for true constants
- Example: `BlockSize = 4 << 20` (4MB blocks)
- Constants in handler testing: `DefaultShareName = "/export"`, `DefaultUID = 1000`

**Error Variables:**
- All caps prefix `Err`: `ErrNotFound`, `ErrAccessDenied`, `ErrPermissionDenied`
- Examples from `pkg/metadata/errors.go`:
  - `ErrNotFound`, `ErrAlreadyExists`, `ErrIsDirectory`, `ErrNotDirectory`
  - `ErrIOError`, `ErrNoSpace`, `ErrQuotaExceeded`, `ErrLocked`

## Code Style

**Formatting:**
- Use `gofmt` (Go's built-in formatter)
- Standard Go 80-character soft line limit (Go community convention)
- Indentation: tabs (Go standard)

**Linting:**
- Use golangci-lint with configured linters: `govet`, `unused`, `errcheck`, `staticcheck`, `ineffassign`
- Config file: `.golangci.yml`
- Disabled rules: `intrange` (range over int modernization)

**File Header Comments:**
- Package documentation comment immediately above package declaration
- Multi-line comments use `//` not `/* */`
- Example from `internal/protocol/nfs/v3/handlers/testing/fixtures.go`:
  ```go
  // Package testing provides test fixtures for NFS v3 handler behavioral tests.
  //
  // This package uses real memory stores (not mocks) to test handlers against
  // RFC 1813 behavioral requirements without testing implementation details.
  package testing
  ```

**Type Documentation:**
- Full documentation comment for every exported type
- Include purpose, usage examples, thread-safety notes when relevant
- Example from `pkg/metadata/service.go`:
  ```go
  // MetadataService provides all metadata operations for the filesystem.
  //
  // It manages metadata stores and routes operations to the correct store
  // based on share name. All protocol handlers should interact with MetadataService
  // rather than accessing stores directly.
  ```

**Method Documentation:**
- Document all exported methods with full sentences starting with the method name
- Example from `pkg/metadata/service.go`:
  ```go
  // RegisterStoreForShare associates a metadata store with a share.
  // Each share must have exactly one store. Calling this again for the same
  // share will replace the previous store.
  ```

**Function Documentation:**
- All exported functions documented
- When complex: include usage examples in documentation
- Example from `pkg/metadata/service.go`:
  ```go
  // New creates a new empty MetadataService instance.
  // Use RegisterStoreForShare to configure stores for each share.
  // By default, deferred commits are enabled for better write performance.
  func New() *MetadataService
  ```

## Import Organization

**Order:**
1. Standard library imports (`fmt`, `sync`, `context`, etc.)
2. External imports (`github.com/*`, `go.opentelemetry.io/*`, etc.)
3. Internal imports (`github.com/marmos91/dittofs/pkg/*`, `github.com/marmos91/dittofs/internal/*`)

Each group separated by blank line.

**Example from `pkg/metadata/service.go`:**
```go
import (
    "context"
    "fmt"
    "sync"
)
```

**Example with externals from `internal/protocol/nfs/v3/handlers/testing/fixtures.go`:**
```go
import (
    "context"
    "path/filepath"
    "testing"

    "github.com/marmos91/dittofs/internal/protocol/nfs/v3/handlers"
    "github.com/marmos91/dittofs/pkg/cache"
    "github.com/marmos91/dittofs/pkg/controlplane/runtime"
    "github.com/marmos91/dittofs/pkg/metadata"
    metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)
```

**Import Aliases:**
- Used for disambiguation when multiple imports have same name
- Pattern: `shortname` for store imports: `metadatamemory`, `storemem`, `blocks3`
- Example: `metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"`

## Error Handling

**Error Return Pattern:**
- Functions return `(T, error)` or just `error`
- Multiple return values standard: no custom error types wrapping unless necessary
- Example: `func GetStoreForShare(shareName string) (MetadataStore, error)`

**Error Creation:**
- Use `fmt.Errorf()` for formatted errors with context
- Use custom error types (e.g., `*StoreError`) for structured errors requiring additional fields
- Example from `pkg/metadata/service.go`:
  ```go
  if store == nil {
      return fmt.Errorf("cannot register nil store for share %q", shareName)
  }
  ```

**Custom Error Type Pattern:**
- Defined in `errors.go` files per package
- Include error code (enum), message, and optional path
- Implement `error` interface via `Error()` method
- Example from `pkg/metadata/errors.go`:
  ```go
  type StoreError struct {
      Code    ErrorCode
      Message string
      Path    string
  }

  func (e *StoreError) Error() string {
      if e.Path != "" {
          return e.Message + ": " + e.Path
      }
      return e.Message
  }
  ```

**Error Factory Functions:**
- Lowercase factory functions for creating specific errors
- Pattern: `New{ErrorName}Error(...)`
- Examples: `NewNotFoundError()`, `NewPermissionDeniedError()`, `NewIsDirectoryError()`
- Example from `pkg/metadata/errors.go`:
  ```go
  func NewNotFoundError(path, entityType string) *StoreError {
      return &StoreError{
          Code:    ErrNotFound,
          Message: fmt.Sprintf("%s not found", entityType),
          Path:    path,
      }
  }
  ```

**Error Checking Functions:**
- Lowercase helper functions: `Is{ErrorName}Error()`
- Example: `IsNotFoundError(err)`
- Often use `errors.As()` internally for type checking

**Log Level for Errors:**
- `logger.Debug()`: Expected operational errors (file not found, permission denied, stale handle)
- `logger.Error()`: Unexpected errors (I/O failures, invariant violations, resource exhaustion)
- No logging from handlers themselves - let service layer log; handlers just return errors

## Logging

**Framework:**
- Standard library `log/slog` via internal wrapper `pkg/logger`
- Configured globally with levels: DEBUG, INFO, WARN, ERROR

**Usage Pattern:**
```go
logger.Debug("operation", "details")
logger.Info("significant event", "field", value)
logger.Warn("unexpected condition", "field", value)
logger.Error("operation failed", "field", value)
```

**Log Levels:**
- DEBUG: Detailed operation traces, protocol handler entry/exit, lock acquisitions
- INFO: Service startup/shutdown, significant state changes, successful operations summary
- WARN: Recoverable issues, deprecated usage, performance degradation warnings
- ERROR: Unrecoverable failures, invariant violations, resource exhaustion

**When to Log:**
- Service layer: Log before/after operations, especially at boundaries
- Handler layer: Minimal logging, defer to service errors
- Store layer: Log only resource contention or unexpected issues

## Comments

**Block Comments:**
- Used for test organization: `// ============================================================================`
- Used to separate logical sections within functions
- Never use `/* */` style (use `//` instead)

**Inline Comments:**
- Explain "why" not "what" - code shows what, comments explain intent
- Example from `pkg/metadata/service.go`:
  ```go
  // Create a lock manager for this share if it doesn't exist
  if _, exists := s.lockManagers[shareName]; !exists {
      s.lockManagers[shareName] = NewLockManager()
  }
  ```

**Documentation Comments (JSDoc-style):**
- Not heavily used in Go (relies on exported/unexported distinction)
- Can include RFC references for protocol implementations
- Example from `internal/protocol/nfs/v3/handlers/read.go`:
  ```go
  // RFC 1813 Section 3.3.6 specifies the READ procedure as:
  //
  //    READ3res NFSPROC3_READ(READ3args) = 6;
  ```

**TODO/FIXME Comments:**
- Golangci.yml disabled `intrange` linting - add comment if decision changes
- Otherwise minimal - issues tracked in GitHub

## Function Design

**Receiver Types:**
- Value receivers for immutable types: most value types, small structs
- Pointer receivers for mutable types: stores, services, large structs
- Consistent within type: if one method uses `*T`, all methods use `*T`
- Example: `MetadataService` always uses `(s *MetadataService)` receiver

**Parameter Order:**
- Context first: `ctx context.Context`
- Then data parameters
- Finally options/callbacks
- Example: `func (s *MetadataService) CreateFile(authCtx *AuthContext, parentHandle FileHandle, name string, attr *FileAttr) (*File, error)`

**Return Values:**
- Successful result first, error second: `(T, error)`
- Multiple return values: `(T1, T2, error)` where T1, T2 are results
- Never return error in other positions

**Parameter Documentation:**
- Include in comment above function, especially for complex types
- Example from `pkg/metadata/service.go`:
  ```go
  // RegisterStoreForShare associates a metadata store with a share.
  // Each share must have exactly one store. Calling this again for the same
  // share will replace the previous store.
  //
  // This also creates a LockManager for the share if one doesn't exist.
  func (s *MetadataService) RegisterStoreForShare(shareName string, store MetadataStore) error
  ```

## Concurrency Patterns

**Mutex Naming:**
- `mu` for single lock in type: common pattern
- `mu sync.RWMutex` for read-write locks
- Always defer unlock immediately: `defer mu.Unlock()`
- Example from `pkg/metadata/service.go`:
  ```go
  type MetadataService struct {
      mu sync.RWMutex
      stores map[string]MetadataStore
  }
  ```

**Lock Acquisition:**
- Read locks for data retrieval: `s.mu.RLock()` then `defer s.mu.RUnlock()`
- Write locks for mutations: `s.mu.Lock()` then `defer s.mu.Unlock()`

**Atomic Operations:**
- Use `sync/atomic` for simple counters/flags
- Example from `internal/logger/logger.go`:
  ```go
  var currentLevel atomic.Int32
  currentLevel.Store(int32(LevelInfo))
  ```

**Test Helpers:**
- Include `t.Helper()` at start to exclude from test call stack
- Example from `pkg/metadata/service_test.go`:
  ```go
  func newTestFixture(t *testing.T) *testFixture {
      t.Helper()
      // ... setup code
  }
  ```

## Module & Package Structure

**Directory Organization:**
- `cmd/` - Entry points (CLI binaries)
- `pkg/` - Public APIs (stable interfaces)
- `internal/` - Private implementation (protocol, logging, utilities)
- `test/` - Test suites (integration, e2e)

**Backward Compatibility:**
- `pkg/*` imports must maintain backward compatibility
- `internal/*` can change freely
- Use major.minor.patch versions for public API changes

---

*Convention analysis: 2026-02-02*
