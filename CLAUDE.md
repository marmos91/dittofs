# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

DittoFS is an experimental modular virtual filesystem written in Go that decouples file interfaces from storage backends.
It implements NFSv3, NFSv4.0, and NFSv4.1 protocol servers in pure Go (userspace, no FUSE required) with pluggable metadata and block storage backends.

**Status**: Experimental - not production ready.

## Documentation Structure

DittoFS has comprehensive documentation organized by topic:

### Core Documentation
- **[README.md](README.md)** - Quick start and project overview
- **[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)** - Deep dive into design patterns and implementation
- **[docs/CONFIGURATION.md](docs/CONFIGURATION.md)** - Complete configuration guide with examples
- **[docs/NFS.md](docs/NFS.md)** - NFS protocol implementation details (v3, v4.0, v4.1) and client usage
- **[docs/CONTRIBUTING.md](docs/CONTRIBUTING.md)** - Development guide and contribution guidelines
- **[docs/IMPLEMENTING_STORES.md](docs/IMPLEMENTING_STORES.md)** - Guide for implementing custom metadata and block stores

### Operational Guides
- **[docs/TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md)** - Common issues and solutions
- **[docs/SECURITY.md](docs/SECURITY.md)** - Security considerations and best practices
- **[docs/FAQ.md](docs/FAQ.md)** - Frequently asked questions
- **[docs/RELEASING.md](docs/RELEASING.md)** - Release process and versioning

### Testing and Performance
- **[test/e2e/BENCHMARKS.md](test/e2e/BENCHMARKS.md)** - Performance benchmark documentation
- **[test/e2e/COMPARISON_GUIDE.md](test/e2e/COMPARISON_GUIDE.md)** - Comparing with other NFS implementations

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
        │   └── block/          # Block store management (local + remote)
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
./dfsctl store block add --kind remote --name s3-content --type s3 --config '{...}'

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
- `memory/memory` - Both metadata and block store in memory
- `memory/filesystem` - Memory metadata, filesystem block store
- `badger/filesystem` - BadgerDB metadata, filesystem block store
- `memory/s3` - Memory metadata, S3 block store (requires Localstack)
- `badger/s3` - BadgerDB metadata, S3 block store (requires Localstack)

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
- Block store operation spans (hits, misses, flushes)
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

DittoFS uses a **Runtime-centric architecture** where the Runtime is the single entrypoint for all operations:

```
┌─────────────────────────────────────────┐
│         Protocol Adapters               │
│            (NFS, SMB)                   │
│       pkg/adapter/{nfs,smb}/            │
└───────────────┬─────────────────────────┘
                │ GetBlockStoreForHandle(handle)
                ▼
┌─────────────────────────────────────────┐
│              Runtime                    │
│   (Composition layer + sub-services)    │
│   pkg/controlplane/runtime/             │
│                                         │
│  ┌──────────┐ ┌────────┐ ┌──────────┐  │
│  │ adapters │ │ stores │ │  shares  │  │
│  │lifecycle │ │registry│ │per-share │  │
│  └──────────┘ └────────┘ │BlockStore│  │
│  ┌──────────┐ ┌────────┐ └──────────┘  │
│  │  mounts  │ │lifecycl│ ┌──────────┐  │
│  │ tracking │ │  serve  │ │ identity │  │
│  └──────────┘ └────────┘ │ mapping  │  │
│                           └──────────┘  │
│  ┌────────────┐  ┌───────────────────┐  │
│  │   Store    │  │   Auth Layer      │  │
│  │ (Persist)  │  │   pkg/auth/       │  │
│  │ 9 sub-ifs  │  │ AuthProvider,     │  │
│  │            │  │ IdentityMapper    │  │
│  └────────────┘  └───────────────────┘  │
└───────┬───────────────────┬─────────────┘
        │                   │
        ▼                   ▼
┌────────────────┐  ┌──────────────────────┐
│   Metadata     │  │ Per-Share BlockStore │
│     Stores     │  │  pkg/blockstore/     │
│                │  │                      │
│  - Memory      │  │  ┌──────────────┐    │
│  - BadgerDB    │  │  │ Local Store  │    │
│  - PostgreSQL  │  │  │ fs / memory  │    │
│                │  │  └──────┬───────┘    │
│                │  │         │            │
│                │  │  ┌──────▼───────┐    │
│                │  │  │   Syncer     │    │
│                │  │  │ (async xfer) │    │
│                │  │  └──────┬───────┘    │
│                │  │         │            │
│                │  │  ┌──────▼────────┐   │
│                │  │  │ Remote Store  │   │
│                │  │  │ s3 / memory   │   │
│                │  │  │ (ref counted) │   │
│                │  │  └───────────────┘   │
└────────────────┘  └──────────────────────┘
```

### Key Interfaces

**1. Runtime** (`pkg/controlplane/runtime/`)
- **Single entrypoint for all operations** - both API handlers and internal code
- Thin composition layer delegating to 6 sub-services:
  - `adapters/`: Protocol adapter lifecycle (create, start, stop, delete)
  - `stores/`: Metadata store registry
  - `shares/`: Share registration and configuration; owns per-share `*engine.BlockStore` instances
  - `mounts/`: Unified mount tracking across protocols
  - `lifecycle/`: Server startup/shutdown orchestration
  - `identity/`: Share-level identity mapping
- Key methods:
  - `Serve(ctx)`: Starts all adapters and servers, blocks until shutdown
  - `CreateAdapter(ctx, cfg)`: Saves to store AND starts immediately
  - `DeleteAdapter(ctx, type)`: Stops adapter AND removes from store
  - `AddAdapter(adapter)`: Direct adapter injection (for testing)
  - `GetBlockStoreForHandle(ctx, handle)`: Resolves per-share BlockStore from a file handle via `shares.Service`

**2. Control Plane Store** (`pkg/controlplane/store/`)
- GORM-based persistent storage for configuration
- Decomposed into 9 sub-interfaces: `UserStore`, `GroupStore`, `ShareStore`, `PermissionStore`, `MetadataStoreConfigStore`, `BlockStoreConfigStore`, `AdapterStore`, `SettingsStore`, `GuestStore`
- Composite `Store` interface embeds all sub-interfaces
- API handlers accept narrowest interface needed
- Generic GORM helpers: `getByField[T]`, `listAll[T]`, `createWithID[T]`
- Supports SQLite (single-node) and PostgreSQL (HA)

**3. API Server** (`pkg/controlplane/api/`)
- REST API with JWT authentication
- Centralized API error mapping via `MapStoreError`/`HandleStoreError` helpers
- Thin handlers that delegate to Runtime methods
- RFC 7807 Problem Details for error responses

**4. Adapter Interface** (`pkg/adapter/adapter.go`)
- Each protocol implements the `Adapter` interface
- `IdentityMappingAdapter` extends `Adapter` with `auth.IdentityMapper`
- `BaseAdapter` provides shared TCP lifecycle, default stubs
- `ProtocolError` interface for protocol-specific error codes
- Lifecycle: `SetRuntime() -> Serve() -> Stop()`
- Thread-safe, supports graceful shutdown

**5. Auth** (`pkg/auth/`)
- Centralized authentication abstractions shared across protocols
- `AuthProvider`: `CanHandle(token)` + `Authenticate(ctx, token)` + `Name()`
- `Authenticator`: Chains providers, tries each in order
- `Identity`: Protocol-neutral model (Unix creds, Kerberos, NTLM, anonymous)
- `IdentityMapper`: Converts `AuthResult` to protocol-specific identity
- Sub-packages: `kerberos/` (AuthProvider with keytab management and hot-reload)

**6. MetadataService** (`pkg/metadata/`)
- Central service for metadata operations with share-based routing
- Split into focused files:
  - `file_create.go`, `file_modify.go`, `file_remove.go`, `file_helpers.go`, `file_types.go`
  - `auth_identity.go`, `auth_permissions.go`
- `storetest/`: Conformance test suite (all store implementations must pass)

**7. BlockStore** (`pkg/blockstore/`)
- Per-share block storage orchestrator. Each share gets its own `*engine.BlockStore` instance.
- `engine.BlockStore` composes `local.LocalStore + remote.RemoteStore + sync.Syncer`
- Each share gets an isolated local storage directory; remote stores can be shared across shares (ref counted)
- `shares.Service` owns the lifecycle (create on AddShare, close on RemoveShare)
- Sub-packages:
  - `engine/`: BlockStore orchestrator (compose local + remote + syncer)
  - `local/`: Local store interface and implementations (`fs/` filesystem, `memory/` in-memory)
  - `remote/`: Remote store interface and implementations (`s3/` production, `memory/` testing)
  - `sync/`: Syncer for async local-to-remote data transfer
  - `gc/`: Block garbage collection
  - `io/`: Extracted read/write I/O helpers

**8. Metadata Store** (`pkg/metadata/store/`)
- Stores file/directory structure, attributes, permissions
- Handles access control and root directory creation
- Implements `ObjectStore` interface for content-addressed deduplication
- Implementations: memory, BadgerDB, PostgreSQL
- File handles are opaque identifiers (format varies by implementation)

### Directory Structure

```
dittofs/
├── cmd/
│   ├── dfs/                      # Server CLI binary
│   │   ├── main.go               # Entry point
│   │   └── commands/             # Cobra commands (start, stop, config, logs, backup)
│   │
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
│   │       └── doc.go            # Package doc
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
│   │   ├── storetest/            # Conformance test suite
│   │   └── store/                # Store implementations
│   │       ├── memory/           # In-memory (ephemeral)
│   │       ├── badger/           # BadgerDB (persistent)
│   │       └── postgres/         # PostgreSQL (distributed)
│   │
│   ├── blockstore/               # Per-share block storage
│   │   ├── doc.go                # Package documentation
│   │   ├── store.go              # FileBlockStore interface
│   │   ├── types.go              # FileBlock, BlockState types
│   │   ├── errors.go             # BlockStore error types
│   │   ├── engine/               # BlockStore orchestrator (local + remote + syncer)
│   │   ├── local/                # Local store interface
│   │   │   ├── fs/               # Filesystem-backed local store
│   │   │   └── memory/           # In-memory local store (testing)
│   │   ├── remote/               # Remote store interface
│   │   │   ├── s3/               # S3-backed remote store
│   │   │   └── memory/           # In-memory remote store (testing)
│   │   ├── sync/                 # Syncer (async local-to-remote transfer)
│   │   ├── gc/                   # Block garbage collection
│   │   ├── io/                   # Read/write I/O helpers
│   │   └── storetest/            # Conformance test helpers
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
│   │   │   ├── router.go         # Route definitions
│   │   │   └── ...
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
│   ├── cli/                      # CLI utilities
│   │   ├── output/               # Table, JSON, YAML formatting
│   │   ├── prompt/               # Interactive prompts
│   │   └── credentials/          # Multi-context credential storage
│   │
│   ├── adapter/nfs/              # NFS protocol implementation
│   │   ├── dispatch.go           # Consolidated RPC dispatch
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
   - Metadata/block store configurations
   - Settings

2. **Runtime** (ephemeral): In-memory state for active operations:
   - Loaded metadata store instances
   - Active shares with root handles and per-share BlockStore instances
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

# Create block stores (local per-share, remote shared across shares)
./dfsctl store block add --kind local --name local-cache --type fs \
  --config '{"path":"/data/cache"}'
./dfsctl store block add --kind remote --name s3-remote --type s3 \
  --config '{"region":"us-east-1","bucket":"my-bucket"}'

# Create shares referencing stores by name (each gets its own BlockStore)
./dfsctl share create --name /temp --metadata fast-meta --local local-cache
./dfsctl share create --name /archive --metadata persistent-meta \
  --local local-cache --remote s3-remote
```

### Benefits

- **Per-share isolation**: Each share gets its own BlockStore with isolated local storage directory
- **Resource Efficiency**: Remote stores are shared (ref counted) when multiple shares reference the same config
- **Flexible Topologies**: Mix local-only and remote-backed storage per-share
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

The context is created in `dispatch.go:ExtractAuthContext (internal/adapter/nfs/dispatch.go)()` and passed through:
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

### 4. Per-Share Block Store Isolation

Each share has its own `*engine.BlockStore` instance with isolated local storage directory. Remote stores are shared (ref counted) when multiple shares reference the same configuration. Block stores are created during `AddShare` and closed during `RemoveShare`.

Protocol handlers resolve the block store for each operation via `GetBlockStoreForHandle(ctx, handle)`, which extracts the share name from the file handle and returns the share's BlockStore. There is no global BlockStore.

### 5. Error Handling

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

## NFS Implementation Details

DittoFS supports NFSv3, NFSv4.0, and NFSv4.1. NFSv4 includes built-in file locking and Kerberos authentication via RPCSEC_GSS.

### RPC Flow
1. TCP connection accepted
2. RPC message parsed (`rpc/message.go`)
3. Program/version/procedure validated
4. Auth context extracted (`dispatch.go:ExtractAuthContext (internal/adapter/nfs/dispatch.go)`)
5. Procedure handler dispatched
6. Handler calls repository methods
7. Response encoded and sent

### Critical Procedures

**Mount Protocol** (`internal/adapter/nfs/mount/handlers/`)
- `MNT`: Validates export access, records mount, returns root handle
- `UMNT`: Removes mount record
- `EXPORT`: Lists available exports
- `DUMP`: Lists active mounts (can be restricted)

**NFSv3 Core** (`internal/adapter/nfs/v3/handlers/`)
- `LOOKUP`: Resolve name in directory → file handle
- `GETATTR`: Get file attributes
- `SETATTR`: Update attributes (size, mode, times)
- `READ`: Read file content (uses per-share block store)
- `WRITE`: Write file content (coordinates metadata + per-share block store)
- `CREATE`: Create file
- `MKDIR`: Create directory
- `REMOVE`: Delete file
- `RMDIR`: Delete empty directory
- `RENAME`: Move/rename file
- `READDIR` / `READDIRPLUS`: List directory entries

### Write Coordination Pattern

WRITE operations require coordination between metadata and block stores:

```go
// 1. Update metadata (validates permissions, updates size/timestamps)
attr, preSize, preMtime, preCtime, err := metadataStore.WriteFile(handle, newSize, authCtx)

// 2. Resolve per-share block store from file handle
blockStore, err := rt.GetBlockStoreForHandle(ctx, handle)

// 3. Write actual data via per-share block store
err = blockStore.WriteAt(ctx, string(attr.PayloadID), data, offset)

// 4. Return updated attributes to client for cache consistency
```

The metadata store:
- Validates write permission
- Returns pre-operation attributes (for WCC data)
- Updates file size if extended
- Updates mtime/ctime timestamps
- Ensures PayloadID exists

### Buffer Pooling

Large I/O operations use buffer pools (`internal/adapter/nfs/bufpool.go`):
- Reduces GC pressure
- Reuses buffers for READ/WRITE
- Automatically sizes based on request

## Common Development Tasks

### Adding a New NFS Procedure

1. Add handler in `internal/adapter/nfs/v3/handlers/` or `internal/adapter/nfs/mount/handlers/`
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

**Block Store (Local):**
1. Implement `pkg/blockstore/local.LocalStore` interface
2. Support random-access reads/writes, block-level caching, eviction
3. Handle recovery and flush semantics
4. Test with conformance suite in `pkg/blockstore/local/localtest/`

**Block Store (Remote):**
1. Implement `pkg/blockstore/remote.RemoteStore` interface
2. Support `ReadBlock`, `WriteBlock`, `DeleteBlock`, `HealthCheck`
3. Test with conformance suite in `pkg/blockstore/remote/remotetest/`
4. Remote stores are shared (ref counted) when multiple shares reference the same config

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
   - See [docs/FAQ.md](docs/FAQ.md) for details

3. **Single-node only**: No distributed/HA support
   - No clustering or high availability
   - No replication (except via S3 bucket replication)
   - Single point of failure

See [docs/FAQ.md](docs/FAQ.md) for comprehensive documentation of all limitations, including POSIX compliance details.

## References

- [RFC 1813 - NFS Version 3](https://tools.ietf.org/html/rfc1813)
- [RFC 5531 - RPC Protocol](https://tools.ietf.org/html/rfc5531)
- [RFC 4506 - XDR](https://tools.ietf.org/html/rfc4506)
- See README.md for detailed architecture documentation
- See CONTRIBUTING for contribution guidelines
- Keep in mind the official NFS implementation here: https://github.com/torvalds/linux/tree/master/fs/nfs. Always compare our implementation with the official one to make sure it's correct
- Keep in mind the official SMB implementation here: https://github.com/samba-team/samba. Always compare our implementation with the official source code, to make sure it's correct

## Debugging Protocol Interop Issues

When a protocol conformance test fails against DittoFS but passes against a reference server (Samba, Windows), the fastest path to root cause is a **byte-level pcap diff**. Source-code reading alone is often insufficient — subtle differences in field ordering, optional elements, or encoding (e.g., SPNEGO token structure, NTLMSSP flags, SMB2 response body format) are easy to miss in code but obvious on the wire.

### Recipe

```bash
# 1. Run the failing test against a throwaway reference server on port 11445
#    (DittoFS uses 12445 locally; Samba/Windows use 445; keep them distinct)
docker run --rm -d --name ref-server --platform linux/amd64 -p 11445:445 \
  -v /tmp/ref-share:/share \
  dperson/samba:latest \
  -u "testuser;TestPassword01!" \
  -s "share;/share;yes;no;no;testuser"

# 2. Capture traffic on both servers in parallel using tcpdump
#    (install via `apk add tcpdump` on Alpine, `apt-get install tcpdump` on Debian)
docker exec -u root -d ref-server tcpdump -i any -w /tmp/ref.pcap -s 0 port 445

# 3. Run the same smbtorture/nfstest/etc. against each server

# 4. Diff specific protocol fields with tshark (from nicolaka/netshoot image)
docker run --rm -v /tmp:/tmp --platform linux/amd64 nicolaka/netshoot \
  tshark -r /tmp/ref.pcap -d tcp.port==12445,nbss -V \
  -Y "smb2.cmd==1 and smb2.nt_status==0 and smb2.flags.response==1" \
  -c 1 2>/dev/null | grep -iE "spnego|negtoken|mechListMIC|supportedMech"
```

### When this strategy shines

- **Protocol-level mismatches**: differing SPNEGO/GSS-API token formats, missing optional fields (e.g., the bench.session-setup investigation found a missing NTLMSSP mechListMIC in SESSION_SETUP SUCCESS responses)
- **Signature/encryption format drift**: byte-for-byte comparison catches nonce derivation bugs, key-direction mixups, flag ordering
- **Error response format issues**: clients reject responses that parse differently than expected (see smb2.scan.find: our server was sending command-specific body instead of MS-SMB2 2.2.2 ERROR body)
- **Negotiation edge cases**: advertised vs. actual capability flags, dialect-specific field presence

### Reference server images

| Protocol | Image | Notes |
|----------|-------|-------|
| SMB | `dperson/samba:latest` | Alpine-based, configurable via CLI flags |
| SMB (full) | `quay.io/samba/samba:latest` | Full samba-tool support, domain controller capable |
| NFSv3/v4 | `erichough/nfs-server` | ext4-backed, supports v3 + v4 |

### Pitfalls

- On Apple Silicon, force `--platform linux/amd64` for consistent behavior
- Use port `11445` for reference SMB (not `12445` which DittoFS owns locally)
- tshark needs `-d tcp.port==NNNNN,nbss` to decode non-standard SMB ports
- Always clean container state between runs (dittofs stack uses `docker compose down -v`)
- Keep both pcaps for post-mortem — `/tmp/ref.pcap` + `/tmp/dittofs.pcap` saved locally is cheap and saves recapture when follow-up questions come up