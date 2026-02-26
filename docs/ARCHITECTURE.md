# DittoFS Architecture

This document provides a deep dive into DittoFS's architecture, design patterns, and internal implementation.

## Table of Contents

- [Core Abstraction Layers](#core-abstraction-layers)
- [Adapter Pattern](#adapter-pattern)
- [Store Registry Pattern](#store-registry-pattern)
- [Repository Interfaces](#repository-interfaces)
- [Built-In and Custom Backends](#built-in-and-custom-backends)
- [Directory Structure](#directory-structure)

## Core Abstraction Layers

DittoFS uses a **Runtime-centric architecture** where the Runtime is the single entrypoint for all operations. This design ensures that both persistent store and in-memory state stay synchronized.

```
┌─────────────────────────────────────────┐
│         Protocol Adapters               │
│            (NFS, SMB)                   │
│       pkg/adapter/{nfs,smb}/            │
└───────────────┬─────────────────────────┘
                │
                ▼
┌─────────────────────────────────────────┐
│              Runtime                    │
│   (Single entrypoint for all ops)       │
│   pkg/controlplane/runtime/             │
│                                         │
│  ┌─────────────────────────────────┐    │
│  │ Adapter Lifecycle Management    │    │
│  │ • AddAdapter, CreateAdapter     │    │
│  │ • StopAdapter, DeleteAdapter    │    │
│  │ • LoadAdaptersFromStore         │    │
│  └─────────────────────────────────┘    │
│                                         │
│  ┌────────────┐  ┌───────────────────┐  │
│  │   Store    │  │   In-Memory       │  │
│  │ (Persist)  │  │     State         │  │
│  │ users,     │  │ metadata stores,  │  │
│  │ groups,    │  │ shares, mounts,   │  │
│  │ adapters   │  │ running adapters  │  │
│  └────────────┘  └───────────────────┘  │
│                                         │
│  ┌─────────────────────────────────┐    │
│  │ Auxiliary Servers               │    │
│  │ • API Server (:8080)            │    │
│  │ • Metrics Server (:9090)        │    │
│  └─────────────────────────────────┘    │
└───────┬─────────────────────────────────┘
        │
        ▼
┌─────────────────────────────────────────┐
│            Services                     │
│   (Business logic & coordination)       │
│                                         │
│  ┌─────────────────┐ ┌────────────────┐ │
│  │ MetadataService │ │ PayloadService │ │
│  │ pkg/metadata/   │ │  pkg/payload/  │ │
│  │ service.go      │ │  service.go    │ │
│  └────────┬────────┘ └───────┬────────┘ │
│           │                  │          │
│           │    ┌─────────────┼────────┐ │
│           │    │         ┌───▼──────┐ │ │
│           │    │ Cache   │Offloader │ │ │
│           │    │ Layer   │ pkg/     │ │ │
│           │    │ pkg/    │payload/  │ │ │
│           │    │ cache/  │offloader/│ │ │
│           │    │    │    └────┬─────┘ │ │
│           │    │    │ ┌──────▼──────┐ │ │
│           │    │    │ │     WAL     │ │ │
│           │    │    │ │pkg/cache/wal│ │ │
│           │    │    └─┴─────────────┘ │ │
│           │    └─────────────────────┘ │
└───────────┼────────────────────────────┘
            │
            ▼
┌────────────────┐  ┌────────────────────┐
│   Metadata     │  │   Payload          │
│     Stores     │  │     Stores         │
│    (CRUD)      │  │    (CRUD)          │
│                │  │                    │
│  - Memory      │  │  - Memory          │
│  - BadgerDB    │  │  - S3              │
│  - PostgreSQL  │  │                    │
└────────────────┘  └────────────────────┘
```

### Key Interfaces

**1. Runtime** (`pkg/controlplane/runtime/`)
- **Single entrypoint for all operations** - both API handlers and internal code
- Updates both persistent store AND in-memory state together
- Thin composition layer delegating to 6 focused sub-services:
  - `adapters/`: Protocol adapter lifecycle management (create, start, stop, delete)
  - `stores/`: Metadata store registry
  - `shares/`: Share registration and configuration
  - `mounts/`: Unified mount tracking across protocols
  - `lifecycle/`: Server startup/shutdown orchestration
  - `identity/`: Share-level identity mapping
- Key methods:
  - `Serve(ctx)`: Starts all adapters and servers, blocks until shutdown
  - `CreateAdapter(ctx, cfg)`: Saves to store AND starts immediately
  - `DeleteAdapter(ctx, type)`: Stops adapter AND removes from store
  - `AddAdapter(adapter)`: Direct adapter injection (for testing)

**2. Control Plane Store** (`pkg/controlplane/store/`)
- Persistent configuration (users, groups, permissions, adapters)
- Decomposed into 9 sub-interfaces: `UserStore`, `GroupStore`, `ShareStore`, `PermissionStore`, `MetadataStoreConfigStore`, `PayloadStoreConfigStore`, `AdapterStore`, `SettingsStore`, `GuestStore`
- Composite `Store` interface embeds all sub-interfaces
- API handlers accept narrowest interface needed
- SQLite (single-node) or PostgreSQL (distributed)

**3. Adapter Interface** (`pkg/adapter/adapter.go`)
- Each protocol implements the `Adapter` interface
- `IdentityMappingAdapter` extends `Adapter` with `auth.IdentityMapper` for protocol-specific identity mapping
- Adapters receive a Runtime reference to access services
- `BaseAdapter` provides shared TCP lifecycle, default `MapError` and `MapIdentity` stubs
- Lifecycle: `SetRuntime() -> Serve() -> Stop()`
- Multiple adapters can share the same runtime
- Thread-safe, supports graceful shutdown

**4. Auth** (`pkg/auth/`)
- Centralized authentication abstractions shared across all protocols
- `AuthProvider` interface: `CanHandle(token)` + `Authenticate(ctx, token)`
- `Authenticator`: Chains multiple providers, tries each in order
- `Identity`: Protocol-neutral authenticated identity (Unix creds, Kerberos, NTLM, anonymous)
- `IdentityMapper` interface: Converts `AuthResult` to protocol-specific identity
- Sub-packages:
  - `kerberos/`: Kerberos `AuthProvider` with keytab management and hot-reload

**5. MetadataService** (`pkg/metadata/`)
- **Central service for all metadata operations**
- Routes operations to the correct store based on share name
- Owns LockManager per share (for SMB/NLM byte-range locking)
- Split into focused files:
  - `file_create.go`, `file_modify.go`, `file_remove.go`, `file_helpers.go`, `file_types.go`: File operations
  - `auth_identity.go`, `auth_permissions.go`: Identity resolution and permission checks
- Protocol handlers should use this instead of stores directly
- `storetest/`: Metadata store conformance test suite (all implementations must pass)

**6. PayloadService** (`pkg/payload/`)
- **Central service for all content operations**
- Routes operations to the correct store based on share name
- Coordinates between cache and offloader
- Sub-packages:
  - `io/`: Extracted read/write I/O operations
  - `offloader/`: Async cache-to-store transfer (renamed from TransferManager)
  - `gc/`: Block garbage collection (extracted from offloader)

**7. Metadata Store** (`pkg/metadata/store.go`)
- **Simple CRUD interface** for file/directory metadata
- Stores file structure, attributes, permissions
- Implementations:
  - `pkg/metadata/store/memory/`: In-memory (fast, ephemeral, full hard link support)
  - `pkg/metadata/store/badger/`: BadgerDB (persistent, embedded, path-based handles)
  - `pkg/metadata/store/postgres/`: PostgreSQL (persistent, distributed, UUID-based handles)
- File handles are opaque identifiers (implementation-specific format)

**8. Block Store** (`pkg/payload/store/store.go`)
- **Simple CRUD interface** for block data (4MB units)
- Supports put, get, delete, list operations
- Implementations:
  - `pkg/payload/store/memory/`: In-memory (fast, ephemeral)
  - `pkg/payload/store/fs/`: Filesystem storage
  - `pkg/payload/store/s3/`: S3-backed storage (range reads, multipart uploads)

**9. Cache Layer** (`pkg/cache/`)
- Slice-aware caching for the Chunk/Slice/Block storage model
- Sequential write optimization (merges 16KB-32KB NFS writes into single slices)
- Newest-wins read merging for overlapping slices
- LRU eviction with dirty data protection
- Uses `wal.Persister` interface for crash recovery
- See [Cache README](../pkg/cache/README.md) for detailed architecture

**10. WAL Persistence** (`pkg/cache/wal/`)
- Write-Ahead Log for cache crash recovery
- `Persister` interface for pluggable implementations
- `MmapPersister`: Memory-mapped file for high performance
- `NullPersister`: No-op for in-memory only deployments
- Enables cache data survival across restarts

**11. Offloader** (`pkg/payload/offloader/`)
- Async cache-to-block-store transfer orchestration (renamed from TransferManager)
- Split into focused files: `offloader.go`, `upload.go`, `download.go`, `dedup.go`, `queue.go`, `entry.go`, `types.go`, `wal_replay.go`
- **Eager upload**: Uploads complete 4MB blocks immediately
- **Download priority**: Downloads pause uploads for read latency
- **Prefetch**: Speculatively fetches upcoming blocks
- **Non-blocking flush**: COMMIT returns immediately (data safe in WAL)
- Handles crash recovery from WAL on startup

## Adapter Pattern

DittoFS uses the Adapter pattern to provide clean protocol abstractions:

```go
// ProtocolAdapter interface (defined in runtime package to avoid import cycles)
type ProtocolAdapter interface {
    Serve(ctx context.Context) error
    Stop(ctx context.Context) error
    Protocol() string
    Port() int
}

// RuntimeSetter - adapters that need runtime access implement this
type RuntimeSetter interface {
    SetRuntime(rt *Runtime)
}

// Example: NFS Adapter accesses services via runtime
type NFSAdapter struct {
    config  NFSConfig
    runtime *runtime.Runtime  // Access to MetadataService and PayloadService
}

func (a *NFSAdapter) handleRead(ctx context.Context, req *ReadRequest) {
    // Use PayloadService for reads (handles caching automatically)
    data, err := a.runtime.GetPayloadService().ReadAt(ctx, shareName, contentID, offset, size)
    // ...
}

// Multiple adapters can run concurrently, sharing the same runtime
rt := runtime.New(cpStore)
rt.SetAdapterFactory(createAdapterFactory())
rt.Serve(ctx)  // Loads adapters from store and starts them
```

## Control Plane Pattern

The Control Plane is the central management component enabling flexible, multi-share configurations.

### How It Works

1. **Named Store Creation**: Stores are created with unique names (e.g., "fast-memory", "s3-archive")
2. **Share-to-Store Mapping**: Each NFS share references a store by name
3. **Handle Identity**: File handles encode both the share ID and file-specific data
4. **Store Resolution**: When handling operations, the runtime decodes the handle to identify the share, then routes to the correct stores

### Configuration Example

Stores, shares, and adapters are managed at runtime via `dfsctl` (persisted in the control plane database):

```bash
# Create named stores (created once, shared across shares)
./dfsctl store metadata add --name fast-meta --type memory
./dfsctl store metadata add --name persistent-meta --type badger \
  --config '{"path":"/data/metadata"}'

./dfsctl store payload add --name fast-payload --type memory
./dfsctl store payload add --name s3-payload --type s3 \
  --config '{"region":"us-east-1","bucket":"my-bucket"}'

# Create shares that reference stores by name
./dfsctl share create --name /temp --metadata fast-meta --payload fast-payload
./dfsctl share create --name /archive --metadata persistent-meta --payload s3-payload
```

### Benefits

- **Resource Efficiency**: One S3 client serves multiple shares
- **Flexible Topologies**: Mix ephemeral and persistent storage per-share
- **Isolated Testing**: Each share can use different backends
- **Future Multi-Tenancy**: Foundation for per-tenant store isolation

## Service Layer

The service layer provides business logic and coordination between stores and caches.

### MetadataService

Handles all metadata operations with share-based routing:

```go
// MetadataService - central service for metadata operations
type MetadataService struct {
    stores       map[string]MetadataStore  // shareName -> store
    lockManagers map[string]*LockManager   // shareName -> lock manager
}

// Usage by protocol handlers
metaSvc := metadata.New()
metaSvc.RegisterStoreForShare("/export", memoryStore)
metaSvc.RegisterStoreForShare("/archive", badgerStore)

// High-level operations (with business logic)
file, err := metaSvc.CreateFile(authCtx, parentHandle, "test.txt", fileAttr)
entries, err := metaSvc.ReadDir(ctx, dirHandle)

// Byte-range locking (SMB/NLM)
lock, err := metaSvc.AcquireLock(ctx, shareName, handle, offset, length, exclusive)
```

### PayloadService

Handles all content operations with caching:

```go
// PayloadService - central service for content operations
type PayloadService struct {
    stores map[string]ContentStore  // shareName -> store
    caches map[string]cache.Cache   // shareName -> cache (optional)
}

// Usage by protocol handlers
payloadSvc := payload.New()
payloadSvc.RegisterStoreForShare("/export", memoryStore)
payloadSvc.RegisterCacheForShare("/export", memoryCache)

// High-level operations (cache-aware)
data, err := payloadSvc.ReadAt(ctx, shareName, contentID, offset, size)  // Checks cache first
err := payloadSvc.WriteAt(ctx, shareName, contentID, data, offset)       // Writes to cache
err := payloadSvc.Flush(ctx, shareName, contentID)                       // Flushes cache to store
```

### Store Interfaces (CRUD)

Stores are now simple CRUD interfaces, with business logic in services:

```go
// MetadataStore - simple CRUD for metadata
type MetadataStore interface {
    GetFile(ctx context.Context, handle FileHandle) (*FileAttr, error)
    CreateFile(ctx context.Context, parent FileHandle, name string, attr *FileAttr) (*FileAttr, error)
    DeleteFile(ctx context.Context, handle FileHandle) error
    UpdateFile(ctx context.Context, handle FileHandle, attr *FileAttr) error
    ListDir(ctx context.Context, handle FileHandle) ([]*DirEntry, error)
}

// ContentStore - simple CRUD for content
type ContentStore interface {
    ReadAt(ctx context.Context, id ContentID, offset int64, size int64) ([]byte, error)
    WriteAt(ctx context.Context, id ContentID, data []byte, offset int64) error
    Delete(ctx context.Context, id ContentID) error
    Truncate(ctx context.Context, id ContentID, size int64) error
    Stats(ctx context.Context, id ContentID) (*ContentStats, error)
}
```

## Built-In and Custom Backends

### Using Built-In Backends

No custom code required - configure via CLI:

```bash
# Create stores
./dfsctl store metadata add --name default-meta --type memory  # or badger, postgres
./dfsctl store payload add --name default-payload --type memory  # or filesystem, s3

# Create share referencing stores
./dfsctl share create --name /export --metadata default-meta --payload default-payload
```

Or programmatically:

```go
// Create stores
metadataStore := memory.NewMemoryMetadataStoreWithDefaults()
contentStore := fscontent.New("/data/content")

// Create services
metaSvc := metadata.New()
metaSvc.RegisterStoreForShare("/export", metadataStore)

payloadSvc := payload.New()
payloadSvc.RegisterStoreForShare("/export", contentStore)

// Create registry and wire services
registry := registry.New()
registry.SetMetadataService(metaSvc)
registry.SetPayloadService(payloadSvc)

// Start server
server := server.New(registry)
server.Serve(ctx)
```

### Implementing Custom Store Backends

Stores are simple CRUD interfaces - implement only what's needed:

```go
// 1. Implement metadata store (simple CRUD)
type PostgresStore struct {
    db *sql.DB
}

func (s *PostgresStore) GetFile(ctx context.Context, handle FileHandle) (*metadata.FileAttr, error) {
    var attr metadata.FileAttr
    err := s.db.QueryRowContext(ctx,
        "SELECT size, mtime, mode FROM files WHERE handle = $1",
        handle,
    ).Scan(&attr.Size, &attr.MTime, &attr.Mode)
    return &attr, err
}

func (s *PostgresStore) CreateFile(ctx context.Context, parent FileHandle, name string, attr *metadata.FileAttr) (*metadata.FileAttr, error) {
    // Simple INSERT - no business logic needed
}

// 2. Implement content store (simple CRUD)
type S3Store struct {
    client *s3.Client
    bucket string
}

func (s *S3Store) ReadAt(ctx context.Context, id content.ContentID, offset, size int64) ([]byte, error) {
    result, err := s.client.GetObject(ctx, &s3.GetObjectInput{
        Bucket: aws.String(s.bucket),
        Key:    aws.String(string(id)),
        Range:  aws.String(fmt.Sprintf("bytes=%d-%d", offset, offset+size-1)),
    })
    if err != nil {
        return nil, err
    }
    defer result.Body.Close()
    return io.ReadAll(result.Body)
}

// 3. Register with services (business logic is in services, not stores)
metaSvc.RegisterStoreForShare("/archive", postgresStore)
payloadSvc.RegisterStoreForShare("/archive", s3Store)
```

## Directory Structure

```
dittofs/
├── cmd/
│   ├── dfs/                      # Server CLI binary
│   │   ├── main.go               # Entry point
│   │   └── commands/             # Cobra commands (start, stop, config, logs, backup)
│   └── dfsctl/                   # Client CLI binary
│       ├── main.go               # Entry point
│       ├── cmdutil/              # Shared utilities (auth, output, flags)
│       └── commands/             # Cobra commands (user, group, share, store, adapter)
│
├── pkg/                          # Public API (stable interfaces)
│   ├── adapter/                  # Protocol adapter interface
│   │   ├── adapter.go            # Adapter + IdentityMappingAdapter interfaces
│   │   ├── auth.go               # Adapter-level Authenticator interface
│   │   ├── base.go               # BaseAdapter shared TCP lifecycle
│   │   ├── errors.go             # ProtocolError interface
│   │   ├── nfs/                  # NFS adapter implementation
│   │   └── smb/                  # SMB adapter implementation
│   │
│   ├── auth/                     # Centralized authentication abstractions
│   │   ├── auth.go               # AuthProvider, Authenticator, AuthResult
│   │   ├── identity.go           # Identity model, IdentityMapper interface
│   │   └── kerberos/             # Kerberos AuthProvider
│   │       ├── provider.go       # Provider (implements AuthProvider)
│   │       ├── keytab.go         # Keytab hot-reload manager
│   │       └── config.go         # Package doc
│   │
│   ├── metadata/                 # Metadata layer
│   │   ├── service.go            # MetadataService (business logic, routing)
│   │   ├── store.go              # MetadataStore interface (CRUD)
│   │   ├── file_create.go        # File/directory creation operations
│   │   ├── file_modify.go        # File modification operations
│   │   ├── file_remove.go        # File removal operations
│   │   ├── file_helpers.go       # Shared file operation helpers
│   │   ├── file_types.go         # File-related type definitions
│   │   ├── auth_identity.go      # Identity resolution
│   │   ├── auth_permissions.go   # Permission checking
│   │   ├── cookies.go            # CookieManager (NFS/SMB pagination)
│   │   ├── types.go              # FileAttr, DirEntry, etc.
│   │   ├── errors.go             # Metadata-specific errors
│   │   ├── locking.go            # LockManager for byte-range locks
│   │   ├── storetest/            # Conformance test suite for store implementations
│   │   └── store/                # Store implementations
│   │       ├── memory/           # In-memory (ephemeral)
│   │       ├── badger/           # BadgerDB (persistent)
│   │       └── postgres/         # PostgreSQL (distributed)
│   │
│   ├── payload/                  # Payload storage layer (Chunk/Slice/Block)
│   │   ├── service.go            # PayloadService (main entry point)
│   │   ├── types.go              # PayloadID, FlushResult, etc.
│   │   ├── errors.go             # PayloadError structured type
│   │   ├── io/                   # Extracted read/write I/O operations
│   │   │   ├── reader.go         # Cache-aware read operations
│   │   │   └── writer.go         # Cache-aware write operations
│   │   ├── offloader/            # Async cache-to-store transfer (was transfer/)
│   │   │   ├── offloader.go      # Offloader struct and orchestration
│   │   │   ├── upload.go         # Upload coordination
│   │   │   ├── download.go       # Download coordination
│   │   │   ├── dedup.go          # Deduplication handler
│   │   │   ├── queue.go          # TransferQueue (priority workers)
│   │   │   ├── entry.go          # TransferQueueEntry interface
│   │   │   ├── types.go          # Shared types
│   │   │   └── wal_replay.go     # WAL crash recovery
│   │   ├── gc/                   # Block garbage collection
│   │   │   └── gc.go             # Standalone GC function
│   │   ├── chunk/                # 64MB chunk calculations
│   │   ├── block/                # 4MB block calculations
│   │   └── store/                # Block store implementations
│   │       ├── store.go          # BlockStore interface
│   │       ├── memory/           # In-memory (ephemeral)
│   │       ├── fs/               # Filesystem
│   │       └── s3/               # S3-backed (range reads, multipart)
│   │
│   ├── cache/                    # Slice-aware cache layer
│   │   ├── cache.go              # Cache implementation (LRU, dirty tracking)
│   │   ├── read.go               # Read with newest-wins merge
│   │   ├── write.go              # Write with sequential optimization
│   │   ├── flush.go              # Flush coordination
│   │   ├── eviction.go           # LRU eviction
│   │   ├── types.go              # Slice, SliceState types
│   │   └── wal/                  # WAL persistence
│   │       ├── mmap.go           # MmapPersister implementation
│   │       └── types.go          # SliceEntry, WAL record types
│   │
│   ├── controlplane/             # Control plane (config + runtime)
│   │   ├── store/                # GORM-based persistent store
│   │   │   ├── interface.go      # 9 sub-interfaces + composite Store
│   │   │   ├── gorm.go           # GORMStore implementation
│   │   │   ├── helpers.go        # Generic GORM helpers
│   │   │   └── ...               # Per-entity implementations
│   │   ├── runtime/              # Ephemeral runtime state
│   │   │   ├── runtime.go        # Composition layer (~500 lines)
│   │   │   ├── adapters/         # Adapter lifecycle sub-service
│   │   │   ├── stores/           # Metadata store registry sub-service
│   │   │   ├── shares/           # Share management sub-service
│   │   │   ├── mounts/           # Unified mount tracking sub-service
│   │   │   ├── lifecycle/        # Serve/shutdown orchestration sub-service
│   │   │   └── identity/         # Identity mapping sub-service
│   │   ├── api/                  # REST API server
│   │   │   ├── server.go         # HTTP server with JWT
│   │   │   └── router.go         # Route definitions
│   │   └── models/               # Domain models (User, Group, Share)
│   │
│   ├── apiclient/                # REST API client library
│   │   ├── client.go             # HTTP client with token auth
│   │   ├── helpers.go            # Generic API client helpers
│   │   └── ...                   # Resource-specific methods
│   │
│   └── config/                   # Configuration parsing
│       ├── config.go             # Main config struct
│       ├── stores.go             # Store and offloader creation
│       └── runtime.go            # Runtime initialization
│
├── internal/                     # Private implementation details
│   ├── adapter/nfs/              # NFS protocol implementation
│   │   ├── dispatch.go           # RPC procedure routing
│   │   ├── rpc/                  # RPC layer (call/reply handling)
│   │   │   └── gss/              # RPCSEC_GSS framework
│   │   ├── core/                 # Generic XDR codec
│   │   ├── types/                # NFS constants and types
│   │   ├── mount/handlers/       # Mount protocol procedures
│   │   ├── v3/handlers/          # NFSv3 procedures (READ, WRITE, etc.)
│   │   └── v4/handlers/          # NFSv4.0 and v4.1 procedures
│   ├── adapter/smb/              # SMB protocol implementation
│   │   ├── auth/                 # NTLM/SPNEGO authentication
│   │   ├── framing.go            # NetBIOS framing
│   │   ├── dispatch.go           # Command dispatch
│   │   └── v2/handlers/          # SMB2 command handlers
│   ├── controlplane/api/         # API implementation
│   │   ├── handlers/             # HTTP handlers with centralized error mapping
│   │   └── middleware/           # Auth middleware
│   └── logger/                   # Logging utilities
│
├── docs/                         # Documentation
│   ├── ARCHITECTURE.md           # This file
│   ├── CONFIGURATION.md          # Configuration guide
│   └── ...
│
└── test/                         # Test suites
    ├── integration/              # Integration tests (S3, BadgerDB)
    └── e2e/                      # End-to-end tests (real NFS mounts)
```

## Cache, WAL, and Transfer Architecture

The caching subsystem provides high-performance writes with crash recovery guarantees.

### Data Flow

```
NFS WRITE Request
        │
        ▼
┌───────────────────┐
│  PayloadService   │──────────────────────────────┐
│  pkg/payload/     │                              │
└────────┬──────────┘                              │
         │                                         │
         ▼                                         │
┌───────────────────┐      ┌──────────────────┐   │
│      Cache        │─────►│       WAL        │   │
│   pkg/cache/      │      │    pkg/cache/wal/      │   │
│                   │      │                  │   │
│ • Write buffering │      │ • MmapPersister  │   │
│ • LRU eviction    │      │ • Crash recovery │   │
│ • Slice merging   │      │ • Append-only    │   │
└────────┬──────────┘      └──────────────────┘   │
         │                                         │
         │ NFS COMMIT                              │
         ▼                                         │
┌───────────────────┐                              │
│    Offloader      │                              │
│ pkg/payload/      │                              │
│   offloader/      │                              │
│ • Flush dirty     │                              │
│ • Priority queue  │                              │
│ • Background      │                              │
└────────┬──────────┘                              │
         │                                         │
         ▼                                         │
┌───────────────────┐                              │
│   PayloadStore    │◄─────────────────────────────┘
│ pkg/payload/store/│         (Direct reads bypass cache)
│                   │
│ • Memory          │
│ • S3              │
└───────────────────┘
```

### Cache Layer (`pkg/cache/`)

The cache uses a **Chunk/Slice/Block model**:

- **Chunks**: 64MB logical regions of a file
- **Slices**: Variable-size writes within a chunk (cached in memory)
- **Blocks**: 4MB units flushed to block store

**Key Features**:

```go
// Sequential write optimization - extends existing slices
// Instead of 320 slices for a 10MB file written in 32KB chunks:
// -> 1 slice (sequential writes merged automatically)
c.WriteSlice(ctx, fileHandle, chunkIdx, data, offset)

// Newest-wins read merging - overlapping slices resolved by creation time
data, found, err := c.ReadSlice(ctx, fileHandle, chunkIdx, offset, length)

// LRU eviction - only flushed slices can be evicted
evicted, err := c.EvictLRU(ctx, targetFreeBytes)
```

**Slice States**:
```
SliceStatePending → SliceStateUploading → SliceStateFlushed
     (dirty)           (flush in progress)    (safe to evict)
```

### WAL Persistence (`pkg/cache/wal/`)

The WAL ensures cache data survives crashes:

```go
// Persister interface - pluggable WAL implementations
type Persister interface {
    AppendSlice(entry *SliceEntry) error  // Log a write
    AppendRemove(fileHandle []byte) error // Log a delete
    Sync() error                          // Fsync to disk
    Recover() ([]SliceEntry, error)       // Replay on startup
    Close() error
    IsEnabled() bool
}

// MmapPersister - memory-mapped file for high performance
persister, err := wal.NewMmapPersister("/var/lib/dfs/wal")
if err != nil {
    return err
}

// NullPersister - no-op for testing/in-memory deployments
persister := wal.NewNullPersister()

// Create cache with WAL (pass persister created externally)
cache, err := cache.NewWithWal(maxSize, persister)
```

### Offloader (`pkg/payload/offloader/`)

Orchestrates async cache-to-block-store transfers (renamed from TransferManager):

```go
// TransferQueueEntry - generic transfer operation
type TransferQueueEntry interface {
    ShareName() string
    FileHandle() []byte
    ContentID() string
    Execute(ctx context.Context, offloader *Offloader) error
    Priority() int
}

// Offloader - coordinates flush operations
o := offloader.New(cache, blockStore, config)

// Flush dirty slices for a file
result, err := o.FlushFile(ctx, shareName, fileHandle, contentID)

// Background queue for async uploads
o.EnqueueTransfer(entry)

// Startup recovery from WAL
offloader.RecoverFromWAL(ctx, persister, cache, o)
```

### Garbage Collection (`pkg/payload/gc/`)

Block garbage collection extracted to standalone package:

```go
// CollectGarbage reconciles blocks against metadata and removes orphans
gc.CollectGarbage(ctx, blockStore, metadataStore)
```

## Horizontal Scaling with PostgreSQL

The PostgreSQL metadata store enables horizontal scaling for high-availability and high-throughput deployments:

### Architecture

```
┌─────────────┐  ┌─────────────┐  ┌─────────────┐
│  DittoFS #1 │  │  DittoFS #2 │  │  DittoFS #3 │
│  (Pod 1)    │  │  (Pod 2)    │  │  (Pod 3)    │
└──────┬──────┘  └──────┬──────┘  └──────┬──────┘
       │                │                │
       └────────────────┼────────────────┘
                        │
                   ┌────▼─────┐
                   │PostgreSQL│
                   │ Cluster  │
                   └──────────┘
```

### Key Features

1. **Multiple DittoFS Instances**: Run multiple instances sharing one PostgreSQL database
2. **Load Balancing**: Use Kubernetes services or external load balancers to distribute requests
3. **No Session Affinity Required**: Any instance can serve any request (stateless design)
4. **Independent Connection Pools**: Each instance maintains its own connection pool (10-15 conns typical)
5. **Statistics Caching**: 5-second TTL cache reduces database load
6. **ACID Transactions**: Ensures consistency across concurrent operations

### Deployment Example (Kubernetes)

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: dfs
spec:
  replicas: 3  # Multiple instances for HA
  selector:
    matchLabels:
      app: dfs
  template:
    metadata:
      labels:
        app: dfs
    spec:
      containers:
      - name: dfs
        image: dfs:latest
        ports:
        - containerPort: 12049
          name: nfs
        env:
        - name: DITTOFS_METADATA_POSTGRES_HOST
          value: postgres-service
        - name: DITTOFS_METADATA_POSTGRES_PASSWORD
          valueFrom:
            secretKeyRef:
              name: postgres-secret
              key: password
        resources:
          requests:
            memory: "256Mi"
            cpu: "250m"
          limits:
            memory: "512Mi"
            cpu: "500m"
---
apiVersion: v1
kind: Service
metadata:
  name: dfs-nfs
spec:
  selector:
    app: dfs
  ports:
  - port: 2049
    targetPort: 12049
    protocol: TCP
  type: LoadBalancer
```

### Connection Pool Sizing

Connection pool sizing depends on your workload:

- **Light workload** (< 10 concurrent clients): `max_conns: 10`
- **Medium workload** (10-50 concurrent clients): `max_conns: 15`
- **Heavy workload** (50+ concurrent clients): `max_conns: 20-25`

**Formula**: `max_conns ≈ 2 × expected_concurrent_operations`

**PostgreSQL Limits**: Ensure PostgreSQL `max_connections` > `(DittoFS instances × max_conns)`

Example: 3 DittoFS instances × 15 conns = 45 total connections needed from PostgreSQL

### Performance Considerations

- **Network Latency**: PostgreSQL adds ~1-2ms latency per metadata operation
- **Statistics Caching**: Reduces expensive queries (disk usage, file counts)
- **Query Optimization**: All queries use indexed fields for fast lookups
- **Transaction Overhead**: Short-lived transactions minimize lock contention

### Best Practices

1. **Use Connection Pooling**: Keep `max_conns` reasonable (10-20 per instance)
2. **Enable TLS**: Use `sslmode: require` or higher in production
3. **Monitor Connections**: Watch PostgreSQL connection count and utilization
4. **Scale Horizontally**: Add DittoFS replicas, not connection pool size
5. **Separate Read Replicas**: For read-heavy workloads, consider PostgreSQL read replicas

## Performance Characteristics

DittoFS is designed for high performance through several architectural choices:

- **Direct protocol implementation**: No FUSE overhead
- **Goroutine-per-connection model**: Leverages Go's lightweight concurrency
- **Buffer pooling**: Reduces GC pressure for large I/O operations
- **Streaming I/O**: Efficient handling of large files without full buffering
- **Pluggable caching**: Implement custom caching strategies per use case
- **Zero-copy aspirations**: Working toward minimal data copying in hot paths

## Why Pure Go?

Go provides significant advantages for a project like DittoFS:

- ✅ **Easy deployment**: Single static binary, no runtime dependencies
- ✅ **Cross-platform**: Native support for Linux, macOS, Windows
- ✅ **Easy integration**: Embed DittoFS directly into existing Go applications
- ✅ **Modern concurrency**: Goroutines and channels for natural async I/O
- ✅ **Memory safety**: No buffer overflows or use-after-free vulnerabilities
- ✅ **Strong ecosystem**: Rich standard library and third-party packages
- ✅ **Fast compilation**: Quick iteration during development
- ✅ **Built-in tooling**: Testing, profiling, and race detection included
