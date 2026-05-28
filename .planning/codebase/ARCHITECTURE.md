# Architecture

**Analysis Date:** 2026-02-09

## Pattern Overview

**Overall:** Layered architecture with a Control Plane runtime managing protocol adapters and shared storage backends.

**Key Characteristics:**
- Single control plane (`pkg/controlplane/runtime/`) serves as the entrypoint for all operations
- Named store registry pattern enables resource efficiency (one S3 client for multiple shares)
- Protocol adapters (NFS, SMB) receive runtime reference and delegate all file operations to metadata/payload services
- Graceful shutdown with configurable timeouts across all layers
- Content-addressed storage with deduplication at 4MB block level

## Layers

**Protocol Adapters:**
- Purpose: Implement file sharing protocols (NFSv3, SMBv2) and translate wire operations to repository calls
- Location: `pkg/adapter/{nfs,smb}/`
- Contains: Connection management, XDR/wire protocol encoding, RPC dispatch, auth context extraction
- Depends on: Runtime (for store access), Metadata/Payload services
- Used by: External clients mounting via NFS or SMB

**Control Plane Runtime:**
- Purpose: Central orchestrator managing all server state (metadata stores, shares, mounts, adapters)
- Location: `pkg/controlplane/runtime/`
- Contains: Ephemeral state (loaded metadata stores, active shares with root handles, NFS mounts), adapter lifecycle management, API server/metrics server startup
- Depends on: Control plane store (persistent DB), adapter implementations
- Used by: API handlers, protocol adapters, CLI commands

**Metadata Service Layer:**
- Purpose: Route file operations to correct metadata store, handle permission checking, manage file handle lifecycle
- Location: `pkg/metadata/service.go`
- Contains: Business logic for CRUD operations, permission enforcement, hard link reference counting, lock manager ownership
- Depends on: MetadataStore interface (via share name routing), PayloadService (for content coordination)
- Used by: Protocol adapters, internal file operations

**Metadata Store Interface:**
- Purpose: Persistence layer for file metadata (names, attributes, directory structure)
- Location: `pkg/metadata/store.go`
- Contains: File CRUD, directory child mappings, parent tracking, link counts, content-addressed object storage (Objects, Chunks, Blocks)
- Depends on: Nothing (boundary)
- Used by: MetadataService

**Metadata Store Implementations:**
- Memory: `pkg/metadata/store/memory/` - UUID handles, RWMutex-based locking, ephemeral
- BadgerDB: `pkg/metadata/store/badger/` - Path-based handles enabling recovery, persistent, stats cache with TTL
- PostgreSQL: `pkg/metadata/store/postgres/` - UUID handles with share encoding, distributed support

**Payload Service:**
- Purpose: Coordinate cache and transfer manager for file content operations
- Location: `pkg/payload/service.go`
- Contains: Read/write operations with chunk boundary handling, COW (copy-on-write) source support, flush coordination
- Depends on: Cache (in-memory buffer), TransferManager (persistence to block store)
- Used by: Protocol adapters (READ/WRITE operations)

**Cache Layer:**
- Purpose: In-memory buffer with mmap WAL backing for crash-safe persistence
- Location: `pkg/cache/`
- Contains: Block-aware caching (4MB buffers), LRU eviction with dirty tracking, three states (Pending→Uploading→Uploaded)
- Depends on: WAL persister (for crash recovery), TransferManager (for draining on flush)
- Used by: PayloadService

**Transfer Manager:**
- Purpose: Background async persistence of cache blocks to block store with deduplication
- Location: `pkg/payload/transfer/`
- Contains: Flush coordination (NFS COMMIT), transfer queue with priorities, WAL recovery on startup
- Depends on: Block store, ObjectStore (for dedup metadata)
- Used by: PayloadService (non-blocking flush), cache eviction

**Block Store Interface:**
- Purpose: Durable storage for file content blocks (4MB units)
- Location: `pkg/payload/store/block/store.go`
- Contains: Read, WriteAt, Truncate operations; implementations support range reads and multipart uploads
- Depends on: Nothing (boundary)
- Used by: TransferManager, PayloadService (cache misses)

**Block Store Implementations:**
- Memory: `pkg/payload/store/memory/` - Ephemeral, testing only
- Filesystem: `pkg/payload/store/fs/` - Local disk storage
- S3: `pkg/payload/store/s3/` - Production-ready with range reads, streaming multipart, retry backoff

**Control Plane Store:**
- Purpose: Persistent database for configuration (users, groups, shares, adapters, settings)
- Location: `pkg/controlplane/store/`
- Contains: GORM-based storage supporting SQLite (single-node) and PostgreSQL (HA), user auth with bcrypt + NT hash
- Depends on: Nothing (boundary)
- Used by: Runtime initialization, API handlers

**API Server:**
- Purpose: REST API for remote management (user/group CRUD, share configuration, adapter lifecycle)
- Location: `pkg/controlplane/api/` and `internal/controlplane/api/`
- Contains: HTTP handlers, JWT authentication, request validation
- Depends on: Runtime (for mutations), Control plane store (for reads)
- Used by: `dittofsctl` CLI client, external tools

## Data Flow

**File Operation (e.g., NFSv3 READ):**

1. NFS client sends READ RPC → TCP connection handler (`pkg/adapter/nfs/nfs_connection.go`)
2. Handler parses XDR request, extracts auth context (UID, GID, client IP)
3. Handler calls `v3.Handler.HandleRead()` (`internal/protocol/nfs/v3/handlers/read.go`)
4. Handler delegates to `runtime.Metadata(ctx)` → `MetadataService.GetFile()` (permission check)
5. MetadataService routes to `MetadataStore.GetFile(handle)` via share name in handle
6. MetadataService then calls `PayloadService.ReadAt(payloadID, offset, length)`
7. PayloadService checks cache first, falls back to block store on miss
8. Data returned through all layers, XDR-encoded, sent back to client
9. All context cancellation signals propagate on shutdown for graceful termination

**File Write (e.g., NFSv3 WRITE + COMMIT):**

1. Client sends WRITE RPC with data
2. Handler calls `v3.Handler.HandleWrite()`
3. Handler updates metadata (size, mtime) via `MetadataService.WriteFile()`
4. Handler writes data via `PayloadService.WriteAt()` → cache (non-blocking)
5. Handler returns immediately (data is now in mmap cache)
6. Later, client sends COMMIT RPC
7. Handler calls `PayloadService.Flush()` which:
   - Enqueues cache blocks to TransferManager (non-blocking)
   - Returns immediately; upload happens asynchronously
8. TransferManager background worker:
   - Computes block hashes for deduplication
   - Queries ObjectStore for existing blocks
   - Uploads new blocks to block store (S3, filesystem)
   - Marks cache blocks as Uploaded (safe for LRU eviction)

**Server Startup:**

1. `cmd/dittofs start` entry point
2. Load config from YAML + env overrides
3. Initialize logger, telemetry, profiling
4. Create control plane store (SQLite/PostgreSQL)
5. Ensure admin user, default groups, default adapters exist
6. `runtime.InitializeFromStore()`:
   - Load metadata store configs from DB
   - Create metadata store instances (memory, badger, postgres)
   - Load share configs from DB
   - For each share, create root directory if needed
   - Create PayloadService once (single global cache + transfer manager)
7. Register adapter factories with runtime
8. For each adapter in config:
   - Create adapter instance
   - Call `SetRuntime()` to inject dependencies
   - Launch in goroutine calling `Serve(ctx)`
9. Start API server, metrics server
10. Block on context until SIGINT/SIGTERM

**Graceful Shutdown:**

1. SIGINT/SIGTERM signal received
2. Context cancelled
3. All adapters detect context cancellation in their `Serve()` loop
4. NFS adapter:
   - Listener closed (stops accepting new connections)
   - Shutdown context cancelled (signals in-flight requests)
   - Waits up to ShutdownTimeout for active connections to complete
   - Force-closes remaining connections after timeout
5. SMB adapter: Similar flow
6. API server: Returns with context cancellation
7. Metrics server: Gracefully shuts down
8. Runtime cleanup
9. Control plane store connection closed
10. Process exits

**State Management:**

**Control Plane Store (Persistent):**
- Users, groups, group memberships
- Share configurations (name, metadata store ref, payload store ref)
- Adapter configurations and runtime state
- Settings

**Runtime (Ephemeral):**
- Loaded metadata store instances (keyed by store name)
- Active shares (keyed by share name, contains root handle)
- NFS mounts (keyed by client address, contains mount point)
- Single PayloadService instance (cache + transfer manager)
- Active protocol adapters and their error channels

**Cache (Persistent via WAL):**
- 4MB block buffers for each chunk
- Dirty flags and coverage bitmaps
- Flush state (Pending/Uploading/Uploaded)
- WAL persister backs cache to mmap file for crash recovery

**Lock Manager (Per-Share, Ephemeral):**
- Byte-range locks for SMB/NLM
- Lock manager created per share in MetadataService
- Locks are advisory only (don't survive restart)

## Key Abstractions

**Adapter Interface:**
- Purpose: Unified lifecycle for protocol-specific servers
- Examples: `pkg/adapter/nfs/`, `pkg/adapter/smb/`
- Pattern: `SetRuntime()` → `Serve()` → `Stop()` with context-based shutdown

**MetadataService:**
- Purpose: Central routing and permission enforcement for file operations
- Examples: File CRUD, directory ops, permission checking, hard link management
- Pattern: Service methods take `*AuthContext` and route via share name extracted from handle

**MetadataStore:**
- Purpose: Pluggable persistence for file metadata
- Examples: Memory, BadgerDB, PostgreSQL implementations
- Pattern: Handle-based CRUD with parent/child relationships and transaction support

**PayloadService:**
- Purpose: Coordinate cache and transfer for file content
- Examples: Read/write operations with chunk awareness, flush coordination
- Pattern: Non-blocking writes to cache, async persistence via TransferManager

**BlockStore:**
- Purpose: Pluggable durable storage for content blocks
- Examples: Memory, Filesystem, S3 implementations
- Pattern: `ReadAt`, `WriteAt`, `Truncate` with optional deduplication support

**ObjectStore (within MetadataStore):**
- Purpose: Content-addressed metadata enabling deduplication
- Examples: Object (file), Chunk (64MB), Block (4MB) hierarchy with RefCounting
- Pattern: Find blocks by hash, increment/decrement reference counts atomically

## Entry Points

**Server CLI:**
- Location: `cmd/dittofs/main.go`
- Triggers: `dittofs start [--foreground] [--config /path]`
- Responsibilities: Parse flags, load config, initialize runtime and adapters, handle graceful shutdown

**Remote CLI:**
- Location: `cmd/dittofsctl/main.go`
- Triggers: `dittofsctl [command] [args]`
- Responsibilities: Authenticate to API, execute management commands, output results

**Protocol Handlers (NFS):**
- Location: `internal/protocol/nfs/v3/handlers/` and `internal/protocol/nfs/mount/handlers/`
- Triggers: Incoming RPC calls from NFS clients
- Responsibilities: Parse XDR, validate auth, delegate to metadata/payload services, encode response

**Protocol Handlers (SMB):**
- Location: `internal/protocol/smb/v2/handlers/`
- Triggers: Incoming SMB packets from SMB clients
- Responsibilities: Parse wire format, manage sessions, delegate to metadata/payload services

**API Handlers:**
- Location: `internal/controlplane/api/handlers/`
- Triggers: HTTP requests from dittofsctl or external tools
- Responsibilities: Validate auth, parse JSON, call runtime methods, return JSON responses

## Error Handling

**Strategy:** Layered error types with semantic mapping to protocol responses

**Patterns:**
- `metadata.ExportError`: Semantic errors that map to NFS status codes (ErrNotDirectory, ErrNoEntity, ErrAccess, etc.)
- Context propagation: `*AuthContext` required for all operations; permission checks happen at service layer
- Logging: Debug-level for expected errors (permission denied, file not found), Error-level for unexpected errors
- NFS: Return proper NFS3ERR_* codes via `metadata.ExportError`
- SMB: Return NTSTATUS codes via error types in `internal/protocol/smb/types/`
- Graceful degradation: If share initialization fails, log warning but continue with other shares

## Cross-Cutting Concerns

**Logging:** Structured logging via `internal/logger/` with configurable level (DEBUG/INFO/WARN/ERROR) and format (text/json)

**Validation:**
- GORM struct tags for control plane store (`validate:"required,gt=0"`)
- Protocol-specific validation in handlers (e.g., `CheckExportAccess()` for NFS mount access)
- Configuration validation in `pkg/config/` with environment variable override support

**Authentication:**
- NFS: AUTH_UNIX with optional AllSquash/RootSquash export-level access control
- SMB: NTLM/SPNEGO with session state management
- API: JWT tokens with claims containing user ID and group memberships

**Metrics:** Optional Prometheus collection via `pkg/metrics/prometheus/` with zero overhead when disabled

**Tracing:** Optional OpenTelemetry via `internal/telemetry/` with configurable sample rate and OTLP endpoint

**Rate Limiting:** Optional per-adapter request rate limiting using token bucket algorithm

---

*Architecture analysis: 2026-02-09*
