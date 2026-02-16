# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

DittoFS is an experimental modular virtual filesystem written in Go that decouples file interfaces from storage backends.
It implements NFSv3 protocol server in pure Go (userspace, no FUSE required) with pluggable metadata and payload repositories.

**Status**: Experimental - not production ready.

## Documentation Structure

DittoFS has comprehensive documentation organized by topic:

### Core Documentation
- **[README.md](README.md)** - Quick start and project overview
- **[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)** - Deep dive into design patterns and implementation
- **[docs/CONFIGURATION.md](docs/CONFIGURATION.md)** - Complete configuration guide with examples
- **[docs/NFS.md](docs/NFS.md)** - NFSv3 protocol implementation details and client usage
- **[docs/CONTRIBUTING.md](docs/CONTRIBUTING.md)** - Development guide and contribution guidelines
- **[docs/IMPLEMENTING_STORES.md](docs/IMPLEMENTING_STORES.md)** - Guide for implementing custom metadata and payload stores

### Operational Guides
- **[docs/TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md)** - Common issues and solutions
- **[docs/SECURITY.md](docs/SECURITY.md)** - Security considerations and best practices
- **[docs/FAQ.md](docs/FAQ.md)** - Frequently asked questions
- **[docs/RELEASING.md](docs/RELEASING.md)** - Release process and versioning

### Testing and Performance
- **[test/e2e/BENCHMARKS.md](test/e2e/BENCHMARKS.md)** - Performance benchmark documentation
- **[test/e2e/COMPARISON_GUIDE.md](test/e2e/COMPARISON_GUIDE.md)** - Comparing with other NFS implementations

### Submodule Documentation
Each major subsystem has its own CLAUDE.md with non-obvious conventions and gotchas:
- **[pkg/metadata/CLAUDE.md](pkg/metadata/CLAUDE.md)** - Metadata service layer, file handles, locking
- **[pkg/blocks/CLAUDE.md](pkg/blocks/CLAUDE.md)** - Block service layer, S3 async behavior
- **[pkg/adapter/CLAUDE.md](pkg/adapter/CLAUDE.md)** - Protocol adapter lifecycle
- **[pkg/cache/CLAUDE.md](pkg/cache/CLAUDE.md)** - Block-aware cache, WAL persistence
- **[pkg/transfer/CLAUDE.md](pkg/transfer/CLAUDE.md)** - Transfer manager, background queue
- **[pkg/cache/wal/CLAUDE.md](pkg/cache/wal/CLAUDE.md)** - WAL persistence layer
- **[pkg/config/CLAUDE.md](pkg/config/CLAUDE.md)** - Named stores pattern, env overrides
- **[internal/protocol/CLAUDE.md](internal/protocol/CLAUDE.md)** - NFS/SMB wire formats, handler rules

## CLI Architecture

DittoFS provides two CLI binaries:

| Binary | Purpose | Location |
|--------|---------|----------|
| **`dfs`** | Server daemon management | `cmd/dfs/` |
| **`dfsctl`** | Remote REST API client | `cmd/dfsctl/` |

Both CLIs use the **Cobra** framework with subcommand structure and support shell completion (bash, zsh, fish, powershell).

### CLI Code Structure

```
cmd/
├── dfs/                        # Server CLI
│   ├── main.go
│   └── commands/
│       ├── root.go             # Root command, global flags
│       ├── start.go            # Start server
│       ├── stop.go             # Stop server
│       ├── status.go           # Server status
│       ├── logs.go             # Tail logs
│       ├── version.go          # Version info
│       ├── completion.go       # Shell completion
│       ├── config/             # Config subcommands (init, show, validate, edit)
│       └── backup/             # Backup subcommands
│
└── dfsctl/                     # Client CLI
    ├── main.go
    ├── cmdutil/                # Shared utilities
    │   └── util.go             # Auth client, output helpers, flags
    └── commands/
        ├── root.go             # Root command, global flags (-o, --no-color)
        ├── login.go            # Authentication
        ├── logout.go
        ├── version.go
        ├── completion.go
        ├── context/            # Multi-server context management
        ├── user/               # User CRUD
        ├── group/              # Group CRUD
        ├── share/              # Share management
        │   └── permission/     # Share permissions
        ├── store/
        │   ├── metadata/       # Metadata store management
        │   └── payload/        # Payload store management
        ├── adapter/            # Protocol adapter management
        └── settings/           # Server settings
```

### Shared CLI Packages

```
internal/cli/
├── output/                     # Output formatting (table, JSON, YAML)
├── prompt/                     # Interactive prompts (confirm, password, select)
└── credentials/                # Multi-context credential storage

pkg/apiclient/                  # REST API client library
├── client.go                   # HTTP client with auth
├── users.go, groups.go, ...    # Resource-specific methods
└── errors.go                   # API error types
```

## Essential Commands

### Building
```bash
# Build both binaries
go build -o dfs cmd/dfs/main.go
go build -o dfsctl cmd/dfsctl/main.go

# Install dependencies
go mod download
```

### Server Management (dfs)
```bash
# Configuration
./dfs config init              # Create default config
./dfs config show              # Display config
./dfs config validate          # Validate config

# Server lifecycle
./dfs start                    # Start in foreground
./dfs stop                     # Graceful shutdown
./dfs status                   # Check status
./dfs logs -f                  # Follow logs

# Backup
./dfs backup controlplane --output /tmp/backup.json
```

### Remote Management (dfsctl)
```bash
# Authentication
./dfsctl login --server http://localhost:8080 --username admin
./dfsctl logout
./dfsctl context list          # Multi-server support

# User/Group management
./dfsctl user create --username alice    # Password prompted
./dfsctl user list -o json
./dfsctl group create --name editors
./dfsctl group add-user editors alice

# Share management
./dfsctl share list
./dfsctl share permission grant /export --user alice --level read-write

# Store management
./dfsctl store metadata list
./dfsctl store payload add --name s3-content --type s3 --config '{...}'

# Adapter management
./dfsctl adapter list
```

### Environment Variables
```bash
# Common environment variables:
# DITTOFS_LOGGING_LEVEL: DEBUG, INFO, WARN, ERROR
# DITTOFS_LOGGING_FORMAT: text, json (default: text)
# DITTOFS_ADAPTERS_NFS_PORT: NFS server port (default: 12049)
# DITTOFS_SERVER_SHUTDOWN_TIMEOUT: Graceful shutdown timeout (default: 30s)
# DITTOFS_SERVER_RATE_LIMITING_ENABLED: Enable rate limiting (default: false)
# DITTOFS_TELEMETRY_ENABLED: Enable OpenTelemetry tracing (default: false)
# DITTOFS_TELEMETRY_ENDPOINT: OTLP collector endpoint (default: localhost:4317)
```

**Configuration File**: See [docs/CONFIGURATION.md](docs/CONFIGURATION.md) for complete configuration guide.

**Default Location**: `~/.config/dfs/config.yaml` (or `$XDG_CONFIG_HOME/dfs/config.yaml`)

### Testing

**Unit and Integration Tests** (fast, no special permissions needed):
```bash
# Run all unit/integration tests
go test ./...

# Run with coverage
go test -cover ./...

# Run with race detection
go test -race ./...

# Run specific package
go test ./pkg/metadata/memory/
```

**E2E Tests** (requires sudo and NFS mount capabilities):
```bash
# Note: E2E tests use build tags and are excluded from `go test ./...`
# Use the provided script (recommended):
cd test/e2e
sudo ./run-e2e.sh

# Or run directly with go test:
sudo go test -tags=e2e -v ./test/e2e/...

# Run specific e2e test:
sudo go test -tags=e2e -v ./test/e2e/ -run TestCreateFile_1MB
```

### NFS Client Testing
```bash
# Mount (Linux/macOS, default port 12049)
sudo mount -t nfs -o tcp,port=12049,mountport=12049 localhost:/export /mnt/test

# Mount with custom port (if configured differently)
sudo mount -t nfs -o tcp,port=2049,mountport=2049 localhost:/export /mnt/test

# Unmount
sudo umount /mnt/test
```

### Linting and Formatting
```bash
# Format code
go fmt ./...

# Static analysis
go vet ./...
```

### End-to-End Testing
```bash
# Run all E2E tests (requires NFS client and sudo)
cd test/e2e
sudo ./run-e2e.sh

# Run with S3 tests (requires Docker for Localstack)
sudo ./run-e2e.sh --s3

# Run specific test
sudo ./run-e2e.sh --test TestCreateFile_1MB

# Run with verbose output
sudo ./run-e2e.sh --verbose

# Keep Localstack running for repeated testing
sudo ./run-e2e.sh --s3 --keep-localstack

# Run directly with go test (no script) - note the -tags=e2e flag
sudo go test -tags=e2e -v ./test/e2e/...

# Run specific configuration
sudo go test -tags=e2e -v -run "TestCreateFolder/memory-memory" ./test/e2e/

# Run with race detection
sudo go test -tags=e2e -v -race -timeout 30m ./test/e2e/...
```

**E2E Test Features**:
- Tests all storage backend combinations (memory, BadgerDB, filesystem, S3)
- Real NFS mount testing with actual kernel NFS client
- File size tests from 500KB to 100MB
- Shared store scenarios (multiple shares using same stores)
- Optional Localstack integration for S3 testing

**Available Test Configurations**:
- `memory/memory` - Both metadata and payload in memory
- `memory/filesystem` - Memory metadata, filesystem payload
- `badger/filesystem` - BadgerDB metadata, filesystem payload
- `memory/s3` - Memory metadata, S3 payload (requires Localstack)
- `badger/s3` - BadgerDB metadata, S3 payload (requires Localstack)

See `test/e2e/README.md` for detailed documentation.

## Production Features

### Graceful Shutdown & Connection Management

DittoFS implements comprehensive graceful shutdown with multiple layers:

1. **Automatic Drain Mode**: Listener closes immediately on shutdown signal (no new connections)
2. **Context Cancellation**: Propagates through all request handlers for clean abort
3. **Graceful Wait**: Waits up to `ShutdownTimeout` for connections to complete naturally (configurable, default 30s)
4. **Forced Closure**: After timeout, actively closes TCP connections to release resources
5. **Connection Tracking**: Uses lock-free `sync.Map` for high-performance tracking

**Shutdown Flow**:
```
SIGINT/SIGTERM → Cancel Context → Close Listener → Wait (up to timeout) → Force Close
```

**Configuration**:
```yaml
server:
  shutdown_timeout: 30s  # Customize graceful shutdown timeout
```

### Connection Pooling & Performance

- **Buffer Pooling**: Three-tier `sync.Pool` (4KB/64KB/1MB) reduces allocations by ~90%
- **Concurrent-Safe Tracking**: `sync.Map` for connection registry (optimized for high churn scenarios)
- **Goroutine-Per-Connection**: Correct model for stateful NFS protocol
- **Zero-Copy Operations**: Procedure data references pooled buffers directly
- **Optimized Accept Loop**: Minimal select overhead in hot path

### Rate Limiting

Optional request rate limiting to protect against traffic spikes:

- **Token Bucket Algorithm**: Allows burst traffic while limiting sustained rate
- **Per-Adapter Configuration**: Apply different limits to different protocols
- **Global and Adapter-Level**: Set server-wide defaults or override per adapter
- **Zero Overhead When Disabled**: No performance impact when not enabled

**Configuration**:
```yaml
server:
  # Global rate limiting (applies to all adapters)
  rate_limiting:
    enabled: true
    requests_per_second: 5000    # Sustained rate limit
    burst: 10000                  # Burst capacity (2x sustained recommended)

adapters:
  nfs:
    # Optional: override for this adapter
    rate_limiting:
      enabled: true
      requests_per_second: 10000
      burst: 20000
```

### Prometheus Metrics

Optional metrics collection with zero overhead when disabled:

- Request counters by procedure and status
- Request duration histograms
- In-flight request gauges
- Bytes transferred counters
- Active connection gauge
- Connection lifecycle counters (accepted/closed/force-closed)
- Storage operation metrics (S3, BadgerDB)

**Configuration**:
```yaml
server:
  metrics:
    enabled: true
    port: 9090
```

Metrics exposed at the `/metrics` endpoint (default port 9090).

### OpenTelemetry Tracing

Optional distributed tracing with zero overhead when disabled:

- NFS operation spans (READ, WRITE, LOOKUP, etc.)
- Storage backend spans (S3, BadgerDB, filesystem)
- Cache operation spans (hits, misses, flushes)
- Request context propagation (client IP, file handles, paths)

**Configuration**:
```yaml
telemetry:
  enabled: true
  endpoint: "localhost:4317"  # OTLP gRPC endpoint
  insecure: true              # Skip TLS for local development
  sample_rate: 1.0            # 1.0 = all traces, 0.1 = 10%
```

Traces are exported via OTLP gRPC to any compatible collector (Jaeger, Tempo, Honeycomb, etc.).

### Structured Logging

Configurable log output format:

- **text**: Human-readable format with colors (for terminal)
- **json**: Structured JSON for log aggregation (Elasticsearch, Loki, etc.)

**Configuration**:
```yaml
logging:
  level: INFO      # DEBUG, INFO, WARN, ERROR
  format: json     # text or json
  output: stdout   # stdout, stderr, or file path
```

JSON logs include structured fields:
```json
{"time":"2024-01-15T10:30:45.123Z","level":"INFO","msg":"NFS READ","client":"192.168.1.100","handle":"abc123","offset":0,"count":65536}
```

## Architecture

### Core Abstraction Layers

DittoFS uses the **Registry pattern** to enable named, reusable stores that can be shared across multiple NFS exports:

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
└───────┬───────────────────┬─────────────┘
        │                   │
        ▼                   ▼
┌────────────────┐  ┌────────────────────┐
│   Metadata     │  │   Block Storage    │
│     Stores     │  │                    │
│                │  │  ┌──────────────┐  │
│  - Memory      │  │  │ Cache + WAL  │  │
│  - BadgerDB    │  │  │ pkg/cache/   │  │
│  - PostgreSQL  │  │  │ pkg/cache/wal│  │
│                │  │  └──────┬───────┘  │
│                │  │         │          │
│                │  │  ┌──────▼───────┐  │
│                │  │  │ Transfer Mgr │  │
│                │  │  │ pkg/transfer/│  │
│                │  │  └──────┬───────┘  │
│                │  │         │          │
│                │  │  ┌──────▼───────┐  │
│                │  │  │ Block Stores │  │
│                │  │  │ - Memory     │  │
│                │  │  │ - S3         │  │
│                │  │  └──────────────┘  │
└────────────────┘  └────────────────────┘
```

### Key Interfaces

**1. Runtime** (`pkg/controlplane/runtime/`)
- **Single entrypoint for all operations** - both API handlers and internal code
- Updates both persistent store AND in-memory state together
- Manages adapter lifecycle (create, start, stop, delete)
- Owns auxiliary servers (API, Metrics)
- Coordinates Services (MetadataService, PayloadService)
- Key methods:
  - `Serve(ctx)`: Starts all adapters and servers, blocks until shutdown
  - `CreateAdapter(ctx, cfg)`: Saves to store AND starts immediately
  - `DeleteAdapter(ctx, type)`: Stops adapter AND removes from store
  - `AddAdapter(adapter)`: Direct adapter injection (for testing)

**2. Control Plane Store** (`pkg/controlplane/store/`)
- GORM-based persistent storage for configuration
- Users, groups, and their permissions
- Share configurations
- Settings and metadata/payload store configs
- Supports SQLite (single-node) and PostgreSQL (HA)

**3. API Server** (`pkg/controlplane/api/`)
- REST API with JWT authentication
- User/group CRUD operations
- Share management
- Health checks
- Thin handlers that delegate to Runtime methods

**4. Adapter Interface** (`pkg/adapter/adapter.go`)
- Each protocol implements the `ProtocolAdapter` interface
- Adapters receive a runtime reference via `SetRuntime()`
- Lifecycle: `SetRuntime() → Serve() → Stop()`
- Multiple adapters can share the same runtime
- Thread-safe, supports graceful shutdown

**3. Metadata Store** (`pkg/metadata/store/`)
- Stores file/directory structure, attributes, permissions
- Handles access control and root directory creation
- Implements `ObjectStore` interface for content-addressed deduplication:
  - **Object → Chunk → Block** hierarchy (SHA-256 hashes)
  - Reference counting for garbage collection
  - `FindBlockByHash()` enables deduplication at 4MB block level
- Implementations:
  - `pkg/metadata/store/memory/`: In-memory (fast, ephemeral, full hard link support)
  - `pkg/metadata/store/badger/`: BadgerDB (persistent, embedded, path-based handles)
  - `pkg/metadata/store/postgres/`: PostgreSQL (persistent, distributed, UUID-based handles)
- File handles are opaque identifiers (format varies by implementation)
- BadgerDB handles are path-based, enabling metadata recovery from content store
- PostgreSQL handles encode shareName + UUID for multi-share, distributed deployments

**4. Block Store** (`pkg/blocks/store/`)
- Stores actual file data as blocks
- Supports read, write-at, truncate operations
- Implementations:
  - `pkg/blocks/store/memory/`: In-memory (fast, ephemeral, testing)
  - `pkg/blocks/store/s3/`: **Production-ready** S3 storage with:
    - **Range Reads**: Efficient byte-range requests (100x faster for small reads from large files)
    - **Streaming Multipart Uploads**: Automatic multipart for large files (98% memory reduction)
    - **Configurable Retry**: Exponential backoff for transient errors (network, throttling, 5xx)
    - **Stats Caching**: Intelligent caching reduces S3 ListObjects calls by 99%+
    - **Metrics Support**: Optional Prometheus instrumentation
    - **Path-Based Keys**: Objects stored as `export/path/to/file` for easy inspection

**5. Cache Layer** (`pkg/cache/`)
- Block-aware caching for the Chunk/Block storage model
- Uses **WAL persistence** (`pkg/cache/wal/`) for crash recovery
- **Key features**:
  - Block buffer model (4MB buffers with coverage bitmaps)
  - Direct writes to target position (no coalescing needed)
  - LRU eviction with dirty data protection
  - Three states: Pending (dirty) → Uploading → Uploaded (safe to evict)
- **Configuration**:
  - `cache.max_size`: Maximum cache size (LRU eviction)
  - WAL persistence via `NewWithWal()` (pass persister created externally)
- **Metrics**: Cache hits/misses/evictions exposed via Prometheus

**6. WAL Persistence** (`pkg/cache/wal/`)
- Write-Ahead Log for cache crash recovery
- `Persister` interface for pluggable implementations:
  - `MmapPersister`: Memory-mapped file for high performance
  - `NullPersister`: No-op for in-memory only deployments
- Enables cache data survival across server restarts

**7. Transfer Manager** (`pkg/transfer/`)
- Async cache-to-block-store transfer orchestration
- `TransferManager`: Coordinates flush operations on NFS COMMIT
- `TransferQueue`: Background upload queue with priority
- `TransferQueueEntry`: Generic interface for transfer operations
- Handles startup recovery from WAL

### Directory Structure

```
dittofs/
├── cmd/
│   ├── dfs/                  # Server CLI binary
│   │   ├── main.go           # Entry point
│   │   └── commands/         # Cobra commands (start, stop, config, logs, backup)
│   │
│   └── dfsctl/               # Client CLI binary
│       ├── main.go           # Entry point
│       ├── cmdutil/          # Shared utilities (auth, output, flags)
│       └── commands/         # Cobra commands (user, group, share, store, adapter)
│
├── pkg/                      # Public API (stable interfaces)
│   ├── adapter/              # Protocol adapter interface
│   │   ├── adapter.go        # Core Adapter interface
│   │   ├── nfs/              # NFS adapter implementation
│   │   └── smb/              # SMB adapter implementation
│   │
│   ├── metadata/             # Metadata layer
│   │   ├── service.go        # MetadataService (business logic, routing)
│   │   ├── store.go          # MetadataStore interface (CRUD)
│   │   └── store/            # Store implementations
│   │       ├── memory/       # In-memory (ephemeral)
│   │       ├── badger/       # BadgerDB (persistent)
│   │       └── postgres/     # PostgreSQL (distributed)
│   │
│   ├── blocks/               # Block storage layer
│   │   ├── service.go        # BlockService (caching, routing, flush)
│   │   ├── types.go          # StorageStats, FlushResult, etc.
│   │   └── store/            # Block store implementations
│   │       ├── store.go      # BlockStore interface (CRUD)
│   │       ├── memory/       # In-memory (ephemeral)
│   │       └── s3/           # S3-backed (multipart, streaming)
│   │
│   ├── cache/                # Block-aware cache layer
│   │   ├── cache.go          # Cache implementation (LRU, dirty tracking)
│   │   └── types.go          # BlockState, PendingBlock types
│   │
│   ├── wal/                  # Write-Ahead Log persistence
│   │   ├── persister.go      # Persister interface + NullPersister
│   │   ├── mmap.go           # MmapPersister implementation
│   │   └── types.go          # BlockWriteEntry, WAL record types
│   │
│   ├── transfer/             # Cache-to-store transfer orchestration
│   │   ├── manager.go        # TransferManager (flush coordination)
│   │   ├── queue.go          # TransferQueue (background uploads)
│   │   ├── entry.go          # TransferQueueEntry interface
│   │   └── recovery.go       # WAL recovery on startup
│   │
│   ├── controlplane/         # Control plane (config + runtime)
│   │   ├── store/            # GORM-based persistent store
│   │   │   ├── interface.go  # Store interface
│   │   │   ├── gorm.go       # GORMStore implementation
│   │   │   ├── users.go      # User operations
│   │   │   ├── groups.go     # Group operations
│   │   │   ├── shares.go     # Share operations
│   │   │   └── permissions.go# Permission resolution
│   │   ├── runtime/          # Ephemeral runtime state
│   │   │   ├── runtime.go    # Runtime manager
│   │   │   ├── shares.go     # Share management
│   │   │   └── mounts.go     # Mount tracking
│   │   ├── api/              # REST API server
│   │   │   ├── server.go     # HTTP server with JWT
│   │   │   ├── handlers/     # API handlers
│   │   │   └── middleware/   # Auth middleware
│   │   └── models/           # Domain models (User, Group, Share)
│   │
│   ├── config/               # Configuration parsing
│   │   ├── config.go         # Main config struct
│   │   ├── stores.go         # Store and transfer manager creation
│   │   └── runtime.go        # Runtime initialization
│   │
│   └── apiclient/            # REST API client library
│       ├── client.go         # HTTP client with token auth
│       ├── users.go          # User API methods
│       ├── groups.go         # Group API methods
│       ├── shares.go         # Share API methods
│       └── ...               # Other resource methods
│
├── internal/                 # Private implementation details
│   ├── cli/                  # CLI utilities
│   │   ├── output/           # Table, JSON, YAML formatting
│   │   ├── prompt/           # Interactive prompts
│   │   └── credentials/      # Multi-context credential storage
│   │
│   ├── protocol/nfs/         # NFS protocol implementation
│   │   ├── dispatch.go       # RPC procedure routing
│   │   ├── rpc/              # RPC layer (call/reply handling)
│   │   ├── xdr/              # XDR encoding/decoding
│   │   ├── types/            # NFS constants and types
│   │   ├── mount/handlers/   # Mount protocol procedures
│   │   └── v3/handlers/      # NFSv3 procedures (READ, WRITE, etc.)
│   └── logger/               # Logging utilities
│
└── test/                     # Test suites
    ├── integration/          # Integration tests (S3, BadgerDB)
    └── e2e/                  # End-to-end tests (real NFS mounts)
```

## Control Plane Pattern

The Control Plane is the central innovation enabling flexible configuration and runtime management.

### Architecture

The control plane has two main components:

1. **Store** (persistent): GORM-based database (SQLite or PostgreSQL) storing:
   - Users and groups with bcrypt password hashes
   - Share configurations and permissions
   - Metadata/payload store configurations
   - Settings

2. **Runtime** (ephemeral): In-memory state for active operations:
   - Loaded metadata store instances
   - Active shares with root handles
   - Mount tracking for NFS/SMB
   - Identity resolution

### How It Works

1. **Configuration Loading**: On startup, config is read and stores are created
2. **Share Loading**: Shares are loaded from database, metadata stores initialized
3. **Handle Resolution**: File handles encode share ID, runtime routes to correct stores
4. **Permission Resolution**: Runtime queries store for user/group permissions

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

## Important Design Principles

### 1. Separation of Concerns

**Protocol handlers should ONLY handle protocol-level concerns:**
- XDR encoding/decoding
- RPC message framing
- Procedure dispatch
- Converting between wire types and internal types

**Business logic belongs in repository implementations:**
- Permission checks (`CheckAccess`)
- File creation/deletion
- Directory traversal
- Metadata updates

Example from handlers:
```go
// GOOD: Handler delegates to repository
func HandleLookup(ctx *AuthContext, dirHandle, name string) {
    // Parse XDR request
    // Call repo.Lookup(ctx, dirHandle, name)
    // Encode XDR response
}

// BAD: Handler implements permission checks
func HandleLookup(ctx *AuthContext, dirHandle, name string) {
    attr := getFile(dirHandle)
    if attr.UID != ctx.UID { /* check permissions */ }  // ❌ Wrong layer
}
```

### 2. Authentication Context Threading

All operations require an `*metadata.AuthContext` containing:
- Client address
- Auth flavor (AUTH_UNIX, AUTH_NULL)
- Unix credentials (UID, GID, GIDs)

The context is created in `dispatch.go:ExtractAuthContext()` and passed through:
```
RPC Call → ExtractAuthContext() → Handler → Repository Method
```

Export-level access control (AllSquash, RootSquash) is applied during mount in `CheckExportAccess()`, creating an `AuthContext` with effective credentials for the mount session.

### 3. File Handle Management

File handles are **opaque 64-bit identifiers**:
- Generated by metadata store
- Encode share identity for runtime routing
- Never parsed or interpreted by protocol handlers
- Must remain stable across server restarts for production

Handles are obtained via:
- Root export handle: `GetRootHandle(exportPath)`
- File creation: `CreateFile()`, `CreateDirectory()`, etc.
- Lookup: `GetChild(parentHandle, name)`

### 4. Error Handling

Return proper NFS error codes via `metadata.ExportError`:
```go
// Examples from metadata/errors.go
ErrNotDirectory      // NFS3ERR_NOTDIR
ErrNoEntity          // NFS3ERR_NOENT
ErrAccess            // NFS3ERR_ACCES
ErrExist             // NFS3ERR_EXIST
ErrNotEmpty          // NFS3ERR_NOTEMPTY
```

Log appropriately:
- `logger.Debug()`: Expected/normal errors (permission denied, file not found)
- `logger.Error()`: Unexpected errors (I/O errors, invariant violations)

## NFSv3 Implementation Details

### RPC Flow
1. TCP connection accepted
2. RPC message parsed (`rpc/message.go`)
3. Program/version/procedure validated
4. Auth context extracted (`dispatch.go:ExtractAuthContext`)
5. Procedure handler dispatched
6. Handler calls repository methods
7. Response encoded and sent

### Critical Procedures

**Mount Protocol** (`internal/protocol/nfs/mount/handlers/`)
- `MNT`: Validates export access, records mount, returns root handle
- `UMNT`: Removes mount record
- `EXPORT`: Lists available exports
- `DUMP`: Lists active mounts (can be restricted)

**NFSv3 Core** (`internal/protocol/nfs/v3/handlers/`)
- `LOOKUP`: Resolve name in directory → file handle
- `GETATTR`: Get file attributes
- `SETATTR`: Update attributes (size, mode, times)
- `READ`: Read file content (uses payload store)
- `WRITE`: Write file content (coordinates metadata + payload stores)
- `CREATE`: Create file
- `MKDIR`: Create directory
- `REMOVE`: Delete file
- `RMDIR`: Delete empty directory
- `RENAME`: Move/rename file
- `READDIR` / `READDIRPLUS`: List directory entries

### Write Coordination Pattern

WRITE operations require coordination between metadata and payload stores:

```go
// 1. Update metadata (validates permissions, updates size/timestamps)
attr, preSize, preMtime, preCtime, err := metadataStore.WriteFile(handle, newSize, authCtx)

// 2. Write actual data via payload store
err = payloadStore.WriteAt(attr.PayloadID, data, offset)

// 3. Return updated attributes to client for cache consistency
```

The metadata store:
- Validates write permission
- Returns pre-operation attributes (for WCC data)
- Updates file size if extended
- Updates mtime/ctime timestamps
- Ensures PayloadID exists

### Buffer Pooling

Large I/O operations use buffer pools (`internal/protocol/nfs/bufpool.go`):
- Reduces GC pressure
- Reuses buffers for READ/WRITE
- Automatically sizes based on request

## Common Development Tasks

### Adding a New NFS Procedure

1. Add handler in `internal/protocol/nfs/v3/handlers/` or `internal/protocol/nfs/mount/handlers/`
2. Implement XDR request/response parsing
3. Extract auth context from call
4. Delegate business logic to repository methods
5. Update dispatch table in `dispatch.go`
6. Add test coverage

### Adding a New Store Backend

**Metadata Store:**
1. Implement `pkg/metadata/Store` interface
2. Handle file handle generation (must be unique and stable)
3. Implement root directory creation (`CreateRootDirectory`)
4. Implement permission checking in `CheckAccess()`
5. Ensure thread safety (concurrent access across shares)
6. Consider persistence strategy for handles

**Block Store:**
1. Implement `pkg/blocks/store.BlockStore` interface
2. Support random-access reads/writes (`ReadAt`/`WriteAt`)
3. Handle sparse files and truncation
4. Consider implementing `ReadAtBlockStore` for efficient partial reads
5. Test with the integration test suite in `test/integration/`

### Adding a New Protocol Adapter

1. Create new package in `pkg/adapter/`
2. Implement `Adapter` interface:
   - `Serve(ctx)`: Start protocol server
   - `Stop(ctx)`: Graceful shutdown
   - `SetRuntime()`: Receive runtime reference for store access
   - `Protocol()`: Return name
   - `Port()`: Return listen port
3. Register in `cmd/dfs/main.go`
4. Update README with usage instructions

## Testing Approach

### Unit Tests
- Test repository implementations in isolation
- Mock dependencies where needed
- Focus on business logic correctness
- Run with: `go test ./...`

### Integration Tests
- Test complete request/response cycles with real backends
- Verify S3, BadgerDB, and filesystem integration
- Test with in-memory stores for speed
- Verify protocol compliance
- Located in: `test/integration/`

### End-to-End (E2E) Tests
- Real NFS mount testing with actual kernel NFS client
- Tests all storage backend combinations
- Verifies complete workflows (create, read, write, delete, etc.)
- Tests shared store scenarios (multiple shares using same stores)
- Includes performance benchmarks (separate from regular tests)
- Located in: `test/e2e/`

**Running E2E Tests**:
```bash
# Run all E2E tests (requires NFS client and sudo)
go test -v -timeout 30m ./test/e2e/...

# Run specific test suite
go test -v ./test/e2e/ -run TestE2E/memory
go test -v ./test/e2e/ -run TestE2E/badger
go test -v ./test/e2e/ -run TestE2E/s3

# Run with race detection
go test -v -race -timeout 30m ./test/e2e/...
```

**File Size Testing**:
```bash
# Test specific file sizes
sudo go test -v -run TestCreateFile_500KB ./test/e2e/
sudo go test -v -run TestCreateFile_1MB ./test/e2e/
sudo go test -v -run TestCreateFile_10MB ./test/e2e/
sudo go test -v -run TestCreateFile_100MB ./test/e2e/

# Test read/write operations
sudo go test -v -run TestReadFilesBySize ./test/e2e/
sudo go test -v -run TestWriteThenReadBySize ./test/e2e/
```

### Manual NFS Testing
```bash
# Start server
./dfs start

# Or with debug logging
DITTOFS_LOGGING_LEVEL=DEBUG ./dfs start

# Mount and test operations
sudo mount -t nfs -o tcp,port=12049,mountport=12049 localhost:/export /mnt/test
cd /mnt/test
ls -la              # READDIR / READDIRPLUS
cat readme.txt      # READ
echo "test" > new   # CREATE + WRITE
mkdir foo           # MKDIR
rm new              # REMOVE
rmdir foo           # RMDIR
mv file1 file2      # RENAME
ln file1 file2      # LINK (hard link)
```

## Known Limitations

1. **Memory metadata is ephemeral**: In-memory metadata store loses all data on restart
   - Use BadgerDB backend for persistence
   - BadgerDB provides full persistence with path-based handles

2. **ETXTBSY not enforced**: Writing to executing files is allowed (NFS protocol limitation)
   - NFS servers cannot know if clients are executing files
   - This affects ALL NFS implementations, not a DittoFS bug
   - See [docs/KNOWN_LIMITATIONS.md](docs/KNOWN_LIMITATIONS.md) for details

3. **No file locking**: NLM (Network Lock Manager) protocol not implemented
   - Applications requiring file locks may not work correctly
   - No protection against concurrent writes from multiple clients

4. **No NFSv4**: Only NFSv3 is supported
   - No ACLs, no named attributes, no delegations
   - Use NFSv3-compatible clients only

5. **Limited security**: Basic AUTH_UNIX only
   - No Kerberos authentication
   - No built-in encryption (use VPN or network-level encryption)
   - See [docs/SECURITY.md](docs/SECURITY.md) for recommendations

6. **Single-node only**: No distributed/HA support
   - No clustering or high availability
   - No replication (except via S3 bucket replication)
   - Single point of failure

See [docs/KNOWN_LIMITATIONS.md](docs/KNOWN_LIMITATIONS.md) for comprehensive documentation of all limitations, including POSIX compliance details.

## References

- [RFC 1813 - NFS Version 3](https://tools.ietf.org/html/rfc1813)
- [RFC 5531 - RPC Protocol](https://tools.ietf.org/html/rfc5531)
- [RFC 4506 - XDR](https://tools.ietf.org/html/rfc4506)
- See README.md for detailed architecture documentation
- See CONTRIBUTING for contribution guidelines
- Keep in mind the official NFS implementation here: https://github.com/torvalds/linux/tree/master/fs/nfs. Always compare our implementation with the official one to make sure it's correct
- Keep in mind the official SMB implementation here: https://github.com/samba-team/samba. Always compare our implementation with the official source code, to make sure it's correct