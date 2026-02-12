# Coding Conventions

**Analysis Date:** 2026-02-09

## Naming Patterns

**Files:**
- `*_test.go` - Test files, located alongside source files (co-located)
- `*.go` - All lowercase with underscores (Go standard)
- Service/handler files: `service.go`, `handler.go`, `store.go` (by responsibility)
- Interface files: No special suffix (e.g., `store.go` contains interface `Store`)
- Implementation files: Descriptive names matching store type (e.g., `memory.go`, `badger.go`, `postgres.go`)

**Packages:**
- Lowercase, no underscores: `cache`, `metadata`, `blocks`, `transfer`
- Domain-focused: `pkg/metadata/`, `pkg/blocks/`, `pkg/cache/`, `pkg/transfer/`
- Implementation-specific subdirectories: `store/memory/`, `store/badger/`, `store/s3/`

**Functions:**
- PascalCase (exported): `CreateFile()`, `GetStoreForShare()`, `WriteAt()`
- camelCase (unexported): `storeForHandle()`, `lockManagerForHandle()`, `isTerminal()`
- Package initialization functions start with `New`: `New()`, `NewWithWal()`, `NewMemoryMetadataStoreWithDefaults()`
- Getter methods: `Get*` prefix (e.g., `GetFile()`, `GetStoreForShare()`, `GetRootHandle()`)
- Setter/mutation methods: No special prefix, clear verbs (e.g., `RegisterStoreForShare()`, `WriteAt()`, `MarkBlockUploaded()`)
- Test fixture creators: `New*Fixture()` (e.g., `NewHandlerFixture()`)
- Test helper methods: Simple verb names (e.g., `CreateFile()`, `CreateDirectory()`)

**Variables:**
- Short names in tight scopes: `ctx` for context, `t` for testing.T, `err` for errors
- Loop counters: `i`, `idx`, `blockIdx`
- Meaningful names in larger scopes: `fileEntry`, `metaStore`, `blockBuffer`, `authCtx`
- Receiver names: Single letter or short (e.g., `s *MetadataService`, `c *Cache`, `f *fileEntry`)
- Interface implementations: Verify with actual code patterns

**Types:**
- Struct names: PascalCase (exported) - `FileAttr`, `MetadataService`, `BlockBuffer`, `Cache`
- Error types: Suffix with `Error` (e.g., `StoreError`)
- Error codes: ALL_CAPS constants (e.g., `ErrNotFound`, `ErrAccessDenied`, `ErrPermissionDenied`)
- Interface names: PascalCase (e.g., `MetadataStore`, `BlockStore`, `ObjectStore`)
- Constants: ALL_CAPS with underscores (e.g., `DefaultShareName`, `DefaultUID`, `BlockSize`)

**Test functions:**
- Pattern: `TestFunctionName_Scenario` or `TestFunctionName_RFC_Compliance`
- Examples from codebase:
  - `TestWrite_SimpleWrite` - Simple operation
  - `TestWrite_AtOffset` - Specific scenario
  - `TestWrite_SparseFile` - Edge case
  - `TestLookup_RFC1813` - RFC compliance
  - `TestLookup_ExistingFile` - Specific case

## Code Style

**Formatting:**
- `go fmt` (standard Go formatting)
- Line length: No strict limit, but keep reasonable (80-120 chars for readability)
- Indentation: Tabs (Go standard)

**Linting:**
- Tool: `golangci-lint` via `.golangci.yml`
- Enabled linters:
  - `govet` - Vet analysis
  - `unused` - Unused code detection
  - `errcheck` - Unchecked error handling
  - `staticcheck` - Static analysis
  - `ineffassign` - Ineffective assignments
- Disabled linters:
  - `intrange` - Range over int modernization (disabled to avoid noise)

**Configuration File:** `.golangci.yml` at repo root

## Import Organization

**Order:**
1. Standard library imports (e.g., `"context"`, `"fmt"`, `"sync"`)
2. Third-party imports (e.g., `"github.com/marmos91/dittofs/..."`)
3. Blank lines between groups

**Path Aliases:**
- Not used; imports use full GitHub paths: `"github.com/marmos91/dittofs/pkg/..."`
- All code within same module, no aliasing needed

**Example from `write_test.go`:**
```go
import (
	"testing"

	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v3/handlers"
	handlertesting "github.com/marmos91/dittofs/internal/protocol/nfs/v3/handlers/testing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)
```

## Error Handling

**Patterns:**
- Domain errors use `StoreError` struct with error codes (see `pkg/metadata/errors.go`)
- Business logic errors (permission denied, not found, etc.) return `StoreError`
- Protocol handlers convert `StoreError` codes to protocol-specific codes (NFS status, SMB error)
- Infrastructure errors (network, disk I/O) are wrapped with context

**Example from `pkg/metadata/errors.go`:**
```go
// StoreError represents a domain error from repository operations
type StoreError struct {
	Code    ErrorCode  // Business logic error category
	Message string     // Human-readable description
	Path    string     // Related filesystem path (if applicable)
}

// Error codes (constants)
const (
	ErrNotFound       // File/directory doesn't exist
	ErrAccessDenied   // Permission denied (EACCES)
	ErrPermissionDenied // Operation not permitted (EPERM)
	ErrAlreadyExists  // File already exists
	ErrNotEmpty       // Directory not empty
	ErrIsDirectory    // Expected file, got directory
	ErrNotDirectory   // Expected directory, got file
	// ... more error codes
)
```

**Logging:**
- Expected/normal errors: `logger.Debug()` (permission denied, file not found)
- Unexpected errors: `logger.Error()` (I/O errors, invariant violations)
- Use structured logging via `internal/logger` package

## Comments

**When to Comment:**
- Public API documentation: Always document exported types and functions
- Complex algorithms: Document non-obvious logic (especially thread-safety, locking)
- RFC compliance: Reference specific RFC sections (e.g., "RFC 1813 Section 3.3.6")
- Non-obvious behavior: Lock ordering, transaction isolation, buffer pooling
- Business rules: Why a particular approach was chosen

**JSDoc/GoDoc Style:**
- Package-level documentation: Describe package purpose and usage
- Type documentation: Describe the type and provide usage examples
- Method documentation: Describe parameters, return values, and behavior
- Example format from `pkg/cache/cache.go`:

```go
// Cache is the mandatory cache layer for all content operations.
//
// It uses 4MB block buffers as first-class citizens, storing data directly
// at the correct position. Optional WAL persistence can be enabled via MmapPersister.
//
// Thread Safety:
// Uses two-level locking for efficiency:
//   - globalMu: Protects the files map
//   - per-file mutexes: Protect individual file operations
//
// This allows concurrent operations on different files.
type Cache struct {
	// ... fields
}

// New creates a new in-memory cache with no persistence.
//
// Parameters:
//   - maxSize: Maximum total cache size in bytes. Use 0 for unlimited.
func New(maxSize uint64) *Cache {
	// ...
}
```

**Multi-line Test Comments:**
- Document what RFC section is being tested
- Describe the test scenario
- Example from `write_test.go`:

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

## Function Design

**Size:** Keep functions focused on single responsibility; 50-100 lines is reasonable, can go longer for complex logic

**Parameters:**
- Pass context as first parameter: `func (s *Service) GetFile(ctx context.Context, ...)`
- Use pointer receivers for methods on large structs: `func (s *MetadataService) RegisterStore(...)`
- Group related parameters together (e.g., all handle-related params together)
- Example from `pkg/metadata/service.go`:

```go
func (s *MetadataService) RegisterStoreForShare(shareName string, store MetadataStore) error {
	// First param: receiver
	// Second param: primary parameter (share name)
	// Third param: dependency (store)
}
```

**Return Values:**
- Error as last return value (Go convention)
- Use multiple named returns for clarity (optional, only when beneficial)
- Exported functions should document all return values
- Example: `(handle FileHandle, err error)` or `(file *File, preSize uint64, err error)`

## Module Design

**Exports:**
- Interfaces define public contract: `MetadataStore`, `BlockStore`, `ObjectStore`
- Types/structs are exported when they're part of public API
- Private helper types use lowercase: `fileEntry`, `blockBuffer`, `chunkEntry`

**Barrel Files:**
- Not used; each file has specific responsibility
- Import directly from specific files: `pkg/metadata/store/memory` not `pkg/metadata/store`

**Package Boundaries:**
- `pkg/` - Public API (stable interfaces)
- `internal/` - Private implementation details
- `cmd/` - Command-line applications
- Each package owns its interfaces and concrete implementations

## Concurrency Patterns

**Locking:**
- Use `sync.RWMutex` for read-heavy workloads: `mu sync.RWMutex`
- Protect critical sections with defer unlock: `defer s.mu.Unlock()`
- Document lock ownership in struct comments
- Example from `pkg/metadata/service.go`:

```go
type MetadataService struct {
	mu    sync.RWMutex              // Protects stores map
	stores map[string]MetadataStore // shareName -> store
}

func (s *MetadataService) RegisterStoreForShare(name string, store MetadataStore) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stores[name] = store
	return nil
}

func (s *MetadataService) GetStoreForShare(name string) (MetadataStore, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if store, ok := s.stores[name]; ok {
		return store, nil
	}
	return nil, fmt.Errorf("no store configured for share %q", name)
}
```

**Atomics:**
- Use `sync/atomic` for simple counters: `atomic.Uint64`
- Example from `pkg/cache/cache.go`: `totalSize atomic.Uint64`

**Channels:**
- Used for signal passing (shutdown, done)
- Not used for data transfer in this codebase (prefers mutexes for shared state)

## Protocol Layer Conventions

**Separation of Concerns:**
- Protocol handlers (NFS/SMB) handle ONLY protocol concerns:
  - XDR/SMB2 encoding/decoding
  - RPC message framing
  - Procedure dispatch
- Business logic belongs in service/repository layer (`pkg/metadata`, `pkg/blocks`)
- Never implement permission checks in handlers - delegate to service

**Authentication Context:**
- Thread through all operations: `ExtractAuthContext() → Handler → Service → Store`
- Type: `*metadata.AuthContext` containing UID, GID, client address
- Example: Protocol dispatcher extracts context, passes to handler, handler passes to service

**File Handle Management:**
- Handles are opaque 64-bit identifiers
- NEVER parse handle contents (except for share name extraction in runtime)
- Use `DecodeFileHandle()` only for routing, not interpretation
- Always pass handles through as-is to metadata stores

---

*Convention analysis: 2026-02-09*
