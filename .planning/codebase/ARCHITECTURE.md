# Architecture

**Analysis Date:** 2026-02-04

## Pattern Overview

**Overall:** Registry + Runtime Layer + Protocol Adapters

DittoFS uses a **registry pattern** where named, reusable stores are created once and shared across multiple NFS/SMB exports. The Runtime is a single entrypoint for all operations, managing both persistent configuration (via Store) and ephemeral state (metadata store instances, active mounts, running adapters).

**Key Characteristics:**
- Decoupled protocol implementations (NFS v3, SMB v2) from storage backends
- Stateless protocol handlers that route through central Runtime
- Content-addressed block storage with deduplication via ObjectStore
- Two-phase write coordination between metadata and payload layers
- Graceful shutdown with connection draining and timeout-based closure

## Layers

**Protocol Adapters (`pkg/adapter/{nfs,smb}/`):**
- Purpose: Expose DittoFS via network protocols (NFSv3, SMB2)
- Location: `pkg/adapter/`
- Contains: Wire protocol handlers, connection management, request dispatch
- Depends on: Runtime (injected via `SetRuntime()`)
- Used by: External NFS/SMB clients connecting to network ports

**Protocol Implementation (`internal/protocol/`):**
- Purpose: Low-level XDR/SMB2 encoding/decoding, RPC message framing
- Location: `internal/protocol/{nfs,smb}/`
- Contains: Handler functions, codec implementations, type definitions
- Depends on: Metadata and Payload services for business logic
- Used by: Adapters to process incoming requests

**Control Plane Runtime (`pkg/controlplane/runtime/`):**
- Purpose: Central manager for all runtime state and operations
- Location: `pkg/controlplane/runtime/runtime.go`
- Contains: Adapter lifecycle management, share registry, metadata store instances
- Depends on: Store (persistence), MetadataService, PayloadService
- Used by: Everything - protocol adapters, API handlers, internal code

**Metadata Service (`pkg/metadata/`):**
- Purpose: File system metadata operations and access control
- Location: `pkg/metadata/service.go`
- Contains: File/directory CRUD, permission checks, lock management
- Depends on: MetadataStore implementations, ObjectStore for deduplication
- Used by: Protocol handlers for all file operations

**Payload Service (`pkg/payload/`):**
- Purpose: File content operations with caching and background persistence
- Location: `pkg/payload/service.go`
- Contains: Read/write orchestration, chunk-aware operations, flush coordination
- Depends on: Cache layer, TransferManager for background uploads
- Used by: Protocol handlers for READ/WRITE/COMMIT operations

**Cache Layer (`pkg/cache/`):**
- Purpose: In-memory buffering for all content operations
- Location: `pkg/cache/cache.go`
- Contains: 4MB block buffers, coverage tracking, LRU eviction, optional WAL persistence
- Depends on: WAL persister (optional), block store via TransferManager
- Used by: PayloadService for all read/write operations

**Block Store (`pkg/payload/store/`):**
- Purpose: Persistent storage for file content blocks
- Location: `pkg/payload/store/{memory,s3,fs}/`
- Contains: S3 client, filesystem I/O, in-memory storage implementations
- Depends on: ObjectStore for deduplication metadata
- Used by: TransferManager to persist cache blocks

**Control Plane Store (`pkg/controlplane/store/`):**
- Purpose: Persistent configuration storage (users, groups, shares, adapters, settings)
- Location: `pkg/controlplane/store/gorm.go`
- Contains: GORM-based schema, SQLite/PostgreSQL implementations
- Depends on: GORM ORM
- Used by: Runtime to load and persist configuration

## Data Flow

**Write Operation (NFS WRITE):**

1. Client sends WRITE request to NFS adapter
2. Adapter extracts auth context, routes to Handler
3. Handler calls `PrepareWrite()` on MetadataService (validates, returns intent, no metadata changes yet)
4. Handler writes data to PayloadService
5. PayloadService writes to Cache (non-blocking, 4MB block buffers)
6. Cache updates coverage bitmap, marks block as Pending
7. Handler calls `CommitWrite()` on MetadataService (updates size, timestamps atomically)
8. Client receives acknowledgement while TransferManager uploads cache blocks in background
9. TransferManager flushes to block store (S3 multipart, filesystem write, etc.)
10. Block storage hashes block, checks ObjectStore for deduplication
11. If duplicate block found, increments RefCount; if new, stores block and creates Object reference

**Read Operation (NFS READ):**

1. Client sends READ request to NFS adapter
2. Adapter extracts auth context, routes to Handler
3. Handler calls `PrepareRead()` on MetadataService (validates permission)
4. Handler calls `ReadAt()` on PayloadService with offset/length
5. PayloadService calls Cache.ReadAt() (checks block buffers)
6. If cache hit: returns data immediately
7. If cache miss: calls TransferManager to download block from block store
8. TransferManager hashes downloaded block, increments block RefCount via ObjectStore
9. Data returned to client

**NFS COMMIT (Non-Blocking Flush):**

1. Client sends COMMIT request after writes
2. Handler calls `Flush()` on PayloadService
3. PayloadService enqueues pending blocks to TransferManager (non-blocking)
4. Returns immediately to client (safe in mmap cache with page cache crash safety)
5. TransferManager processes queue in background, uploads blocks concurrently
6. Multiple pending blocks can be in-flight simultaneously

**Share Creation & Mount:**

1. API handler calls `CreateAdapter()` on Runtime
2. Runtime saves adapter config to persistent Store
3. Runtime instantiates adapter, calls `SetRuntime()`
4. Runtime starts adapter with `Serve()` (blocks until context cancelled)
5. Adapter listens on configured port, accepts connections
6. Client mounts export via NFS MOUNT protocol
7. Mount handler validates client access via Runtime.CheckShareAccess()
8. Mount handler returns root file handle (opaque identifier)
9. Client uses root handle for all subsequent operations
10. Runtime routes handle to correct share's metadata store

**State Management:**

- **Persistent:** Control plane database (SQLite/PostgreSQL) stores users, groups, shares, adapters, settings
- **Ephemeral:** Runtime holds metadata store instances, active shares with root handles, active mounts, in-flight locks
- **Volatile:** Cache buffers, pending uploads, in-memory metadata store data
- **Recoverable:** WAL persistence for cache blocks (crash recovery), ObjectStore reference counts ensure deduplication survives restarts

## Key Abstractions

**MetadataStore Interface (`pkg/metadata/store.go`):**
- Purpose: Abstract away file/directory storage implementation
- Examples: `pkg/metadata/store/{memory,badger,postgres}/`
- Pattern: Registry pattern - named stores created once, shared across shares
- Key methods: `GetFile()`, `CreateFile()`, `Lookup()`, `CreateDirectory()`, `Move()`, `CheckAccess()`
- Thread safety: All implementations safe for concurrent access

**BlockStore Interface (`pkg/payload/store/store.go`):**
- Purpose: Abstract away block persistence implementation
- Examples: `pkg/payload/store/{memory,s3,fs}/`
- Pattern: Content-addressed storage via block hashing
- Key methods: `ReadAt()`, `WriteAt()`, `Truncate()`, `Delete()`, `Size()`
- Content-addressed: S3 keys are path-based (`export/path/to/file`), enabling inspection and easy backup

**ObjectStore Interface (in MetadataStore):**
- Purpose: Track content-addressed blocks for deduplication
- Hierarchy: Object (file) → Chunks (64MB segments) → Blocks (4MB units)
- Pattern: SHA-256 hashing enables finding duplicate blocks before upload
- Methods: `PutObject()`, `FindBlockByHash()`, `IncrementRefCount()`, `DecrementRefCount()`
- Reference counting: Safe deduplication and garbage collection

**ProtocolAdapter Interface (`pkg/adapter/adapter.go`):**
- Purpose: Unified lifecycle for protocol servers (NFS, SMB, future protocols)
- Examples: `pkg/adapter/{nfs,smb}/`
- Pattern: Dependency injection via `SetRuntime()`, graceful shutdown via context cancellation
- Lifecycle: `SetRuntime()` → `Serve()` → `Stop()`
- Multiple adapters: Can run simultaneously, share same Runtime/stores

**Runtime Interface (implicit):**
- Purpose: Central registry for all operations
- Methods: `GetMetadataService()`, `GetPayloadService()`, `GetStoreForShare()`, `CreateAdapter()`, `StopAdapter()`
- Isolation: File handles encode share identity, Runtime routes to correct stores
- Thread safety: All methods protected with RWMutex for concurrent access

## Entry Points

**Server Startup (`cmd/dittofs/commands/start.go`):**
- Location: `cmd/dittofs/commands/start.go`
- Triggers: `./dittofs start` or `./dittofs start --daemon`
- Responsibilities:
  1. Load configuration from YAML file
  2. Initialize control plane store (SQLite/PostgreSQL connection)
  3. Create admin user if not exists
  4. Load metadata stores from config
  5. Create PayloadService with cache and transfer manager
  6. Call Runtime.Serve() (blocks until shutdown)
  7. Serve NFS/SMB adapters concurrently with API server

**NFS Protocol Entry (`internal/protocol/nfs/dispatch.go`):**
- Location: `internal/protocol/nfs/dispatch.go`
- Triggers: Incoming NFS RPC call on port 12049 (default)
- Responsibilities:
  1. Parse RPC message from wire format
  2. Extract auth context (UID, GID, client address)
  3. Route to appropriate procedure handler (LOOKUP, READ, WRITE, etc.)
  4. Handler processes request via MetadataService/PayloadService
  5. Encode response back to XDR wire format

**SMB Protocol Entry (`internal/protocol/smb/v2/handlers/`):**
- Location: `internal/protocol/smb/v2/handlers/`
- Triggers: Incoming SMB2 request on port 445 (default)
- Responsibilities: Similar to NFS but with SMB2-specific session/security handling

**API Server Entry (`pkg/controlplane/api/`):**
- Location: `pkg/controlplane/api/server.go`
- Triggers: HTTP requests on port 8080 (default)
- Responsibilities:
  1. Parse HTTP request body (JSON)
  2. Extract JWT token from Authorization header
  3. Route to handler (users, groups, shares, adapters, stores)
  4. Handler calls Runtime methods to modify state
  5. Return JSON response

**Metadata Store Initialization (`pkg/controlplane/runtime/runtime.go`):**
- Location: `pkg/controlplane/runtime/runtime.go:New()`
- Triggers: Runtime construction during server startup
- Responsibilities:
  1. Create named metadata store instances from config
  2. Call `CreateRootDirectory()` on each store for each share
  3. Register stores with MetadataService
  4. Cache root file handles for fast mount response

## Error Handling

**Strategy:** Return appropriate domain error types, convert to protocol-specific codes in handlers

**Patterns:**

- `metadata.ExportError`: Base type for all metadata errors; converts to NFS3ERR_* codes via `Status()` method
  - `ErrAccess`: NFS3ERR_ACCES (permission denied)
  - `ErrNoEntity`: NFS3ERR_NOENT (file not found)
  - `ErrNotDirectory`: NFS3ERR_NOTDIR (operation on non-directory)
  - `ErrExist`: NFS3ERR_EXIST (file already exists)
  - `ErrNotEmpty`: NFS3ERR_NOTEMPTY (directory not empty)
  - `ErrStale`: NFS3ERR_STALE (handle became invalid)
  - `ErrIO`: NFS3ERR_IO (I/O error, catch-all for unexpected errors)

- Handler responsibility: Log context-specific error details, return protocol error code
- Service responsibility: Return domain-specific error with minimal context
- Store responsibility: Return system errors (file not found, disk full, permission denied)

**Example Error Path:**
```
BlockStore.WriteAt() returns os.ErrPermission
→ TransferManager logs "S3 write failed: permission denied"
→ Cache.Flush() returns ErrIO
→ Handler logs and returns NFS3ERR_IO to client
```

## Cross-Cutting Concerns

**Logging:** Structured logging via `internal/logger/` (slog-based)
- All operations log at DEBUG level with context-specific fields
- Errors log at ERROR level with error details
- Use `logger.DebugCtx(ctx, msg, fields...)` for context-aware logging

**Validation:**
- Input validation happens at protocol handler level (length checks, type checks)
- Business logic validation in MetadataService (permission, consistency checks)
- Store validation in implementations (invariant checks)

**Authentication:**
- Export-level: NFS mount protocol validates client access via CheckShareAccess()
- File-level: Operations check Unix credentials via CheckPermissions()
- API-level: JWT token validation in middleware before handler execution

**Locking:**
- Byte-range locks (SMB/NLM): Managed by LockManager in MetadataService (per-share)
- Internal synchronization: Two-level locking (global RWMutex + per-file mutexes) for cache
- Transaction isolation: Store-specific (memory=mutex, BadgerDB=serializable, PostgreSQL=configurable)

**Metrics & Observability:**
- Prometheus metrics optional (zero overhead when disabled)
- OpenTelemetry tracing with OTLP export (configurable sample rate)
- Structured JSON logging for aggregation (Elasticsearch, Loki, etc.)

---

*Architecture analysis: 2026-02-04*
