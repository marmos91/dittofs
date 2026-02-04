# Coding Conventions

**Analysis Date:** 2026-02-04

## Naming Patterns

**Files:**
- Package files: lowercase with underscores (e.g., `read.go`, `write.go`, `lock_manager.go`)
- Test files: `*_test.go` suffix (e.g., `service_test.go`, `cache_test.go`)
- E2E tests: `*_test.go` with `//go:build e2e` build tag
- No camelCase in filenames

**Functions and Methods:**
- PascalCase for exported functions/methods (e.g., `ReadAt()`, `WriteFile()`, `GetFile()`)
- camelCase for unexported functions (e.g., `parseHandle()`, `validatePath()`)
- Handlers prefixed by operation: `Handle<Operation>` (e.g., `HandleLookup()`, `HandleRead()`)
- Constructors named `New` or `New<TypeName>` (e.g., `New()`, `NewMemoryStore()`, `NewWithWal()`)
- Error checkers use `Is<ErrorType>` pattern (e.g., `IsNotFoundError()`, `IsDirectoryError()`)

**Variables:**
- camelCase for all variables (local and package-level, except constants)
- Receiver variable typically single letter (e.g., `s` for service, `c` for cache)
- Context always named `ctx` (not `context` or `c`)
- Error always named `err` (not `e` or `error`)

**Types:**
- PascalCase for type names (e.g., `MetadataService`, `FileAttr`, `Cache`)
- Interface names often end in `er` or full name (e.g., `Reader`, `MetadataStore`)
- Error types use `Error` suffix (e.g., `StoreError`)

**Constants:**
- ALL_CAPS_WITH_UNDERSCORES (e.g., `DefaultShareName`, `BlockSize`, `MaxPathLength`)
- Use `const` blocks grouped by logical purpose

**Packages:**
- Lowercase, single word preferred (e.g., `metadata`, `cache`, `blocks`, `payload`)
- No `underscore_names` in package names
- Domain-specific naming: `adapters/` contains protocol implementations, `stores/` contains backend implementations

## Code Style

**Formatting:**
- Use `gofmt` (built-in, no configuration)
- Indentation: tabs (not spaces)
- Line length: no hard limit, but keep readability in mind
- Blank lines: separate logical groups within functions

**Linting:**
- Use `go vet` (built-in static analyzer)
- Run with: `go vet ./...`
- No explicit linter config file in repo (uses Go defaults)

**Struct Field Ordering:**
- Exported fields before unexported fields
- Logically group related fields
- Use comments to document non-obvious field purposes
- Example from `internal/protocol/nfs/v3/handlers/lookup.go`:
  ```go
  type LookupRequest struct {
      // DirHandle is the file handle of the directory to search in.
      DirHandle []byte

      // Filename is the name to search for within the directory.
      Filename string
  }
  ```

## Import Organization

**Order:**
1. Standard library (e.g., `context`, `fmt`, `sync`)
2. External packages (e.g., `github.com/stretchr/testify`, `gorm.io`)
3. Internal packages (e.g., `github.com/marmos91/dittofs/pkg/...`, `github.com/marmos91/dittofs/internal/...`)

**Path Aliases:**
- Relative imports within same module never use aliases
- Long package names aliased for clarity:
  - `metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"`
  - `storemem "github.com/marmos91/dittofs/pkg/blocks/store/memory"`

**Import Grouping Example** from `internal/protocol/nfs/v3/handlers/testing/fixtures.go`:
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
	"github.com/marmos91/dittofs/pkg/payload"
	storemem "github.com/marmos91/dittofs/pkg/payload/store/memory"
	"github.com/marmos91/dittofs/pkg/payload/transfer"
)
```

## Error Handling

**Patterns:**
- Always check errors explicitly (no ignoring with `_`)
- Return early on error (guard clauses preferred)
- Wrap errors with context using `fmt.Errorf("operation: %w", err)` when appropriate
- Use domain error types (`metadata.StoreError`) for business logic errors
- Use standard `error` for infrastructure/system errors

**Domain Errors** (from `pkg/metadata/errors.go`):
- Errors represent business logic outcomes (file not found, permission denied)
- Each error has an `ErrorCode` for categorization
- Error factory functions for consistency:
  ```go
  NewNotFoundError(path, entityType)
  NewPermissionDeniedError(path)
  NewIsDirectoryError(path)
  NewNotDirectoryError(path)
  NewInvalidHandleError()
  NewLockedError(path, conflict)
  IsNotFoundError(err) // Checker function
  ```
- Errors must include context (path, operation) for debugging

**Logging Levels:**
- `DEBUG`: Expected/normal error conditions (file not found, permission denied)
- `INFO`: Significant state transitions (adapter started, share created)
- `WARN`: Unexpected but recoverable conditions
- `ERROR`: Unexpected errors requiring investigation (I/O errors, invariant violations)

Example from protocol handlers:
```go
// DEBUG for expected errors
logger.Debug("file not found", "path", path)

// ERROR for unexpected errors
logger.Error("metadata store returned unexpected error", "path", path, "err", err)
```

## Logging

**Framework:** `internal/logger` (custom wrapper around Go's `log/slog`)

**Usage:**
```go
logger.Debug("message", "key1", value1, "key2", value2)
logger.Info("message", "key", value)
logger.Warn("message")
logger.Error("message", "error", err)
```

**Structured Format:**
- Always log structured key-value pairs (not formatted strings)
- Keys in lowercase (e.g., `"path"`, `"handle"`, `"offset"`)
- Values left unquoted (slog handles formatting)
- Include relevant context: file paths, operation names, error details

**Configuration:**
```go
logger.Config{
    Level:  "DEBUG|INFO|WARN|ERROR",
    Format: "text|json",
    Output: "stdout|stderr|/path/to/file",
}
```

## Comments

**When to Comment:**
- Exported types/functions/methods: Always document with godoc
- Non-obvious algorithm logic: Explain the why, not the what
- Complex business rules: Document constraints and assumptions
- TODO/FIXME: Very rare (only 1 found in codebase at `pkg/payload/service.go`)

**JSDoc/Godoc Style:**
- Start with package name and purpose (for package-level)
- Start with function name and short purpose (for functions)
- Use structured sections with headers (e.g., `Purpose:`, `Thread Safety:`, `Example:`)
- Separate sections with blank lines

**Example from `pkg/metadata/service.go`:**
```go
// MetadataService provides all metadata operations for the filesystem.
//
// It manages metadata stores and routes operations to the correct store
// based on share name. All protocol handlers should interact with MetadataService
// rather than accessing stores directly.
//
// File Locking:
// MetadataService owns one LockManager per share for byte-range locking (SMB/NLM).
// Locks are ephemeral (in-memory only) and lost on server restart.
// This is separate from metadata stores which handle persistent data.
//
// Usage:
//
//	metaSvc := metadata.New()
//	metaSvc.RegisterStoreForShare("/export", memoryStore)
//
//	// High-level operations (with business logic)
//	file, err := metaSvc.CreateFile(authCtx, parentHandle, "test.txt", fileAttr)
type MetadataService struct {
	...
}
```

**RPC Procedure Documentation:**
- Request/response structs document the RFC section
- Handler functions include Purpose, Returns, and Side Effects sections
- Example from `internal/protocol/nfs/v3/handlers/lookup.go`:
```go
// LOOKUP procedure documentation includes:
// - RFC reference section
// - Fundamental purpose (path resolution building block)
// - Error conditions (NOENT, NOTDIR, ACCES)
```

## Function Design

**Size:**
- Keep functions under 100 lines (aim for 20-40)
- Break complex logic into helper functions
- Each function should have one clear responsibility

**Parameters:**
- Pass context as first parameter: `func Op(ctx context.Context, ...)`
- Group related parameters (e.g., offset+length together)
- Limit to 3-4 parameters (use structs for more)
- Always pass `*AuthContext` for operations requiring auth checks

**Return Values:**
- Single return value preferred
- Multiple returns: error always last (Go convention)
- Use named returns only for clarity in complex functions
- Return early (guard clauses) to avoid deep nesting

**Example from handlers:**
```go
// Single return with error
func (h *Handler) Lookup(ctx *AuthContext, req *LookupRequest) (*LookupResponse, error)

// Multiple returns (common pattern)
attr, preSize, preMtime, preCtime, err := store.WriteFile(handle, newSize, authCtx)
```

## Module Design

**Exports:**
- Exported identifiers (capital letters) are public API
- Unexported identifiers (lowercase) are internal implementation
- Interface types exported, but implementations often private

**Barrel Files:**
- `store.go` defines interfaces and helpers (not implementations)
- Implementations in subdirectories (e.g., `store/memory/`, `store/badger/`)
- No circular imports

**Package Structure Example** (`pkg/metadata/`):
```
interface.go      # MetadataStore interface, high-level types
store.go          # Files, Transaction interfaces
service.go        # MetadataService (routes by share name)
service_test.go   # Service tests
errors.go         # StoreError, error factory functions
types.go          # Domain types (File, FileAttr, etc.)
store/
  ├── memory/     # In-memory store implementation
  ├── badger/     # BadgerDB store implementation
  └── postgres/   # PostgreSQL store implementation
```

## Thread Safety

**Conventions:**
- Document thread safety in godoc comments
- Use `sync.RWMutex` for read-heavy operations
- Use `sync.Mutex` for critical sections
- Use `sync.Map` for concurrent maps (high churn, lock-free reads)
- Never hold locks across function calls (deadlock risk)

**Transaction Safety:**
- All stores support `WithTransaction(ctx, fn)` for atomic operations
- **Nested transactions NOT supported** - don't call `WithTransaction` inside a transaction
- Each file handle encoded with share name for routing

**Example from `pkg/metadata/service.go`:**
```go
// MetadataService struct with sync protection
type MetadataService struct {
	mu             sync.RWMutex
	stores         map[string]MetadataStore // shareName -> store
	lockManagers   map[string]*LockManager  // shareName -> lock manager
	deferredCommit bool
	cookies        *CookieManager
}

// Read operations use RLock
func (s *MetadataService) GetFile(ctx context.Context, handle FileHandle) (*File, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// ...
}

// Write operations use Lock
func (s *MetadataService) RegisterStoreForShare(shareName string, store MetadataStore) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// ...
}
```

## Buffer Pooling

**Pattern:**
- Large I/O operations use buffer pools from `internal/bufpool/`
- Three-tier pools: 4KB, 64KB, 1MB
- Reduces GC pressure by ~90%
- Automatically sized based on request

**Usage:**
- Get buffer: `pool.Get()`
- Return buffer: defer `pool.Put(buf)` immediately after allocation
- No manual management (done by protocol handlers)

## File Handle Management

**Conventions:**
- File handles are opaque 64-bit identifiers
- NEVER parse handle contents in protocol handlers
- Handles generated by metadata store (format varies by implementation)
- Handles must remain stable across server restarts (for BadgerDB, PostgreSQL)
- Memory store handles unstable (testing only)

**Routing Example:**
```go
// Handlers pass handles to services/stores
// Services/stores decode share name from handle
// Never parse in handlers
handle, err := metaSvc.GetChild(ctx, dirHandle, filename)
```

## Content Hashing

**Pattern:**
- SHA-256 hashing for content deduplication
- Block-level deduplication at 4MB boundaries
- Reference counting for garbage collection
- Always check `FindBlockByHash()` before uploading new block

**Example from payload/transfer layer:**
```go
// Hash block data
blockHash := sha256.Sum256(blockData)

// Check for existing block with same hash
existing, err := objectStore.FindBlockByHash(blockHash)
if existing != nil {
    // Block already exists, increment refcount
    objectStore.IncrementBlockRefCount(blockHash)
} else {
    // Block is new, upload to store
    blockStore.PutBlock(ctx, blockHash, blockData)
    objectStore.PutBlock(ctx, blockHash, metadata)
}
```

---

*Convention analysis: 2026-02-04*
