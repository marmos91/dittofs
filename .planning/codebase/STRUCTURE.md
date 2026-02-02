# Codebase Structure

**Analysis Date:** 2026-02-02

## Directory Layout

```
dittofs/
├── cmd/                            # Binary entry points and CLI
│   ├── dittofs/                    # Server daemon CLI
│   │   ├── main.go                 # Server entry point, version injection
│   │   └── commands/               # Cobra commands (start, stop, config, etc)
│   │       ├── root.go             # Root command, global flags
│   │       ├── start.go            # Start server (foreground/daemon)
│   │       ├── stop.go             # Stop running server
│   │       ├── status.go           # Server status
│   │       ├── logs.go             # Tail logs
│   │       ├── version.go          # Version info
│   │       ├── init.go             # Initialize config
│   │       ├── migrate.go          # Database migrations
│   │       ├── completion.go       # Shell completion
│   │       ├── config/             # Config subcommands (init, show, validate, edit)
│   │       ├── backup/             # Backup subcommands
│   │       └── restore/            # Restore subcommands
│   │
│   └── dittofsctl/                 # Client CLI (remote REST API)
│       ├── main.go                 # Client entry point
│       ├── cmdutil/                # Shared utilities
│       │   └── util.go             # Auth client, output helpers, flags
│       └── commands/               # Cobra commands
│           ├── root.go             # Root command, global flags
│           ├── login.go            # Authenticate with server
│           ├── logout.go           # Clear credentials
│           ├── version.go          # Version info
│           ├── completion.go       # Shell completion
│           ├── context/            # Multi-server context management
│           ├── user/               # User CRUD (list, create, edit, delete, password)
│           ├── group/              # Group CRUD
│           ├── share/              # Share management (list, create, edit, delete)
│           │   └── permission/     # Share permissions
│           ├── store/              # Store management
│           │   ├── metadata/       # Metadata store operations
│           │   └── payload/        # Payload store operations
│           ├── adapter/            # Protocol adapter management
│           └── settings/           # Server settings (get, set)
│
├── pkg/                            # Public API (stable interfaces)
│   ├── adapter/                    # Protocol adapters (network servers)
│   │   ├── adapter.go              # Adapter interface definition
│   │   ├── nfs/                    # NFSv3 adapter implementation
│   │   │   ├── nfs_adapter.go      # TCP listener, connection management, graceful shutdown
│   │   │   └── nfs_connection.go   # Per-connection RPC handling
│   │   └── smb/                    # SMB adapter implementation
│   │
│   ├── metadata/                   # Metadata service layer (file structure)
│   │   ├── service.go              # MetadataService (routes by share name)
│   │   ├── interface.go            # MetadataServiceInterface definition
│   │   ├── store.go                # MetadataStore interface (Files, Objects, Shares)
│   │   ├── types.go                # Core types (File, FileAttr, DirEntry, etc)
│   │   ├── object.go               # ObjectStore interface (content-addressed dedup)
│   │   ├── chunks.go               # Chunk management (64MB segments)
│   │   ├── file.go                 # File operations (create, read attrs, etc)
│   │   ├── directory.go            # Directory operations (list, traverse)
│   │   ├── authentication.go       # AuthContext threading, access control
│   │   ├── validation.go           # Path/name validation
│   │   ├── locking.go              # File locking primitives (advisory, per-share)
│   │   ├── io.go                   # I/O helpers (path handling, etc)
│   │   ├── errors.go               # Domain errors (ErrNotFound, ErrAccess, etc)
│   │   ├── cookies.go              # READDIR cookies for pagination
│   │   ├── pending_writes.go       # Track pending write operations
│   │   └── store/                  # Store implementations
│   │       ├── memory/             # In-memory (ephemeral, testing)
│   │       ├── badger/             # BadgerDB (persistent, embedded)
│   │       └── postgres/           # PostgreSQL (distributed, HA)
│   │
│   ├── payload/                    # Payload service layer (file content)
│   │   ├── service.go              # PayloadService (high-level content API)
│   │   ├── types.go                # FlushResult, StorageStats types
│   │   ├── block/                  # Block constants and types (4MB blocks)
│   │   ├── chunk/                  # Chunk constants and types (64MB chunks)
│   │   ├── store/                  # BlockStore implementations
│   │   │   ├── store.go            # BlockStore interface
│   │   │   ├── memory/             # In-memory (ephemeral, testing)
│   │   │   ├── fs/                 # Filesystem (local storage)
│   │   │   └── s3/                 # S3-backed (production, cloud)
│   │   └── transfer/               # Cache-to-block-store transfers
│   │       ├── manager.go          # TransferManager (orchestrates upload/download/prefetch)
│   │       ├── queue.go            # TransferQueue (background worker pool)
│   │       ├── entry.go            # TransferRequest interface
│   │       ├── types.go            # TransferType, FlushResult types
│   │       ├── gc.go               # Garbage collection (unreferenced blocks)
│   │       └── recovery.go         # WAL recovery on startup
│   │
│   ├── cache/                      # Block-aware cache layer
│   │   ├── cache.go                # Cache implementation (block buffers, LRU)
│   │   ├── write.go                # WriteAt implementation
│   │   ├── read.go                 # ReadAt implementation
│   │   ├── flush.go                # GetDirtyBlocks, MarkBlockUploaded
│   │   ├── eviction.go             # LRU eviction
│   │   ├── state.go                # Remove, Truncate, Close, Sync
│   │   ├── types.go                # BlockState, PendingBlock, Stats types
│   │   └── wal/                    # Write-Ahead Log persistence
│   │       ├── persister.go        # Persister interface (mmap, null implementations)
│   │       ├── mmap.go             # MmapPersister (memory-mapped file)
│   │       └── types.go            # BlockWriteEntry, WAL record types
│   │
│   ├── controlplane/               # Control plane (config + runtime)
│   │   ├── store/                  # GORM-based persistent store
│   │   │   ├── interface.go        # Store interface
│   │   │   ├── gorm.go             # GORMStore implementation
│   │   │   ├── users.go            # User operations (create, list, validate)
│   │   │   ├── groups.go           # Group operations
│   │   │   ├── shares.go           # Share operations (persistence)
│   │   │   ├── permissions.go      # Permission resolution
│   │   │   ├── metadata_stores.go  # Named metadata store configs
│   │   │   ├── payload_stores.go   # Named payload store configs
│   │   │   ├── adapters.go         # Adapter configs
│   │   │   ├── settings.go         # Server settings
│   │   │   └── migrations/         # SQL migration files (embedded)
│   │   ├── runtime/                # Ephemeral runtime state
│   │   │   ├── runtime.go          # Runtime manager (core orchestrator)
│   │   │   ├── shares.go           # Share runtime state (root handles)
│   │   │   └── mounts.go           # NFS/SMB mount tracking
│   │   ├── models/                 # Domain models
│   │   │   ├── user.go             # User model
│   │   │   ├── group.go            # Group model
│   │   │   ├── share.go            # Share model
│   │   │   ├── adapter.go          # Adapter model
│   │   │   ├── setting.go          # Setting model
│   │   │   └── permission.go       # Permission types (SharePermission)
│   │   └── api/                    # REST API server
│   │       └── api.go              # HTTP routes, server startup
│   │
│   ├── config/                     # Configuration parsing
│   │   ├── config.go               # Main Config struct and sub-configs
│   │   ├── defaults.go             # Default configuration values
│   │   ├── stores.go               # Factory functions (metadata, payload, transfer)
│   │   ├── runtime.go              # Control plane runtime initialization
│   │   ├── init.go                 # Config file generation
│   │   └── test_fixtures.go        # Test helpers
│   │
│   ├── metrics/                    # Prometheus metrics (optional)
│   │   ├── prometheus/             # Prometheus metrics implementation
│   │   └── metrics.go              # Metrics interfaces
│   │
│   └── apiclient/                  # REST API client library
│       ├── client.go               # HTTP client with JWT auth
│       ├── users.go                # User API methods
│       ├── groups.go               # Group API methods
│       ├── shares.go               # Share API methods
│       ├── stores.go               # Store API methods
│       ├── adapters.go             # Adapter API methods
│       ├── settings.go             # Settings API methods
│       ├── health.go               # Health check
│       └── errors.go               # API error types
│
├── internal/                       # Private implementation details (not stable API)
│   ├── cli/                        # CLI utilities
│   │   ├── output/                 # Output formatting (table, JSON, YAML)
│   │   ├── prompt/                 # Interactive prompts (confirm, password, select)
│   │   ├── credentials/            # Multi-context credential storage
│   │   ├── health/                 # Health check helpers
│   │   └── timeutil/               # Time formatting
│   │
│   ├── controlplane/               # Control plane internals
│   │   └── api/                    # REST API internals
│   │       ├── auth/               # JWT service and claims
│   │       │   ├── jwt_service.go  # Token generation, validation
│   │       │   └── claims.go       # JWT claims structure
│   │       ├── handlers/           # HTTP handlers (resource endpoints)
│   │       │   ├── users.go        # User handlers (CRUD)
│   │       │   ├── groups.go       # Group handlers
│   │       │   ├── shares.go       # Share handlers
│   │       │   ├── metadata_stores.go
│   │       │   ├── payload_stores.go
│   │       │   ├── adapters.go     # Adapter handlers
│   │       │   ├── settings.go     # Settings handlers
│   │       │   ├── health.go       # Health check handler
│   │       │   ├── auth.go         # Authentication handler
│   │       │   ├── problem.go      # RFC 7807 error formatting
│   │       │   ├── response.go     # Response helpers
│   │       │   └── helpers.go      # Shared helper functions
│   │       └── middleware/         # HTTP middleware
│   │           ├── auth.go         # JWT auth middleware
│   │           └── cors.go         # CORS middleware
│   │
│   ├── protocol/                   # Protocol implementations (not stable)
│   │   ├── nfs/                    # NFSv3 protocol
│   │   │   ├── dispatch.go         # RPC procedure routing
│   │   │   ├── rpc/                # RPC message handling (call/reply)
│   │   │   ├── xdr/                # XDR encoding/decoding
│   │   │   ├── types/              # NFS constants and types
│   │   │   ├── mount/              # Mount protocol (RFC 1813)
│   │   │   │   └── handlers/       # Mount procedure handlers (MNT, UMNT, EXPORT)
│   │   │   └── v3/                 # NFSv3 protocol (RFC 1813)
│   │   │       ├── handlers/       # NFSv3 procedure handlers (READ, WRITE, LOOKUP, etc)
│   │   │       └── testing/        # Test helpers
│   │   └── smb/                    # SMB protocol (future)
│   │       ├── header/             # SMB message header parsing
│   │       ├── rpc/                # SMB RPC layer
│   │       ├── types/              # SMB constants and types
│   │       ├── session/            # SMB session management
│   │       ├── signing/            # Message signing
│   │       ├── v2/                 # SMBv2 implementation
│   │       │   └── handlers/       # SMBv2 command handlers
│   │       └── auth/               # Authentication (NTLM, SPNEGO)
│   │
│   ├── auth/                       # Authentication implementations
│   │   ├── ntlm/                   # NTLM authentication (SMB)
│   │   └── spnego/                 # SPNEGO authentication (Kerberos)
│   │
│   ├── logger/                     # Structured logging
│   │   └── logger.go               # Logger interface and implementation
│   │
│   ├── telemetry/                  # OpenTelemetry tracing
│   │   └── telemetry.go            # Tracer setup and helpers
│   │
│   ├── bufpool/                    # Buffer pooling utilities
│   │   └── bufpool.go              # Sync.Pool-based buffer management
│   │
│   ├── bytesize/                   # Byte size parsing
│   │   └── bytesize.go             # Human-readable size parsing (1MB, 1GB, etc)
│   │
│   ├── mfsymlink/                  # Macintosh finder symlink support
│   │   └── mfsymlink.go            # Encoding/decoding Mac finder aliases
│   │
│   └── ...                         # Other internal utilities
│
├── test/                           # Test suites
│   ├── integration/                # Integration tests
│   │   ├── metadata_stores_test.go # Test all metadata store implementations
│   │   └── payload_stores_test.go  # Test all block store implementations
│   └── e2e/                        # End-to-end tests (real NFS mounts)
│       ├── run-e2e.sh              # Test runner script
│       ├── main_test.go            # Test setup and fixtures
│       └── *_test.go               # Individual test cases
│
├── docs/                           # Documentation (markdown)
│   ├── ARCHITECTURE.md             # Architecture deep dive
│   ├── CONFIGURATION.md            # Configuration guide
│   ├── NFS.md                      # NFS protocol details
│   ├── CONTRIBUTING.md             # Development guide
│   ├── IMPLEMENTING_STORES.md      # Custom store guide
│   ├── TROUBLESHOOTING.md          # Common issues
│   ├── SECURITY.md                 # Security considerations
│   ├── FAQ.md                      # Frequently asked questions
│   ├── RELEASING.md                # Release process
│   └── KNOWN_LIMITATIONS.md        # Known limitations
│
├── README.md                       # Project overview and quick start
├── LICENSE                         # Apache 2.0 license
├── go.mod                          # Go module definition
├── go.sum                          # Go dependency checksums
├── Makefile                        # Build targets
├── .gitlab-ci.yml                  # GitLab CI/CD pipeline
└── .planning/                      # GSD (Goal, Scenario, Decisions) planning
    └── codebase/                   # This directory - codebase analysis
        ├── ARCHITECTURE.md         # Architecture analysis
        ├── STRUCTURE.md            # Structure analysis
        ├── CONVENTIONS.md          # (Generated for quality focus)
        ├── TESTING.md              # (Generated for quality focus)
        ├── STACK.md                # (Generated for tech focus)
        ├── INTEGRATIONS.md         # (Generated for tech focus)
        └── CONCERNS.md             # (Generated for concerns focus)
```

## Directory Purposes

**cmd/**
- Purpose: Binary entry points and CLI implementations
- Contains: Cobra command definitions, CLI utilities
- Key files: `dittofs/main.go` (server), `dittofsctl/main.go` (client)

**pkg/**
- Purpose: Public API (stable interfaces for external consumers)
- Contains: Adapter interface, metadata/payload services, control plane runtime
- Note: Package interfaces are considered stable; internal implementations may change

**internal/**
- Purpose: Private implementation details (not part of public API)
- Contains: Protocol handlers, API handlers, CLI utilities, authentication
- Note: Breaking changes allowed without deprecation period

**pkg/adapter/**
- Purpose: Protocol adapter implementations (NFS, SMB servers)
- Key classes: `NFSAdapter`, `SMBAdapter` implement `Adapter` interface
- Connection management: Per-connection handlers in separate files

**pkg/metadata/**
- Purpose: File structure and attribute management
- Key abstractions: `MetadataStore` interface, `MetadataService` router
- Implementations: Memory (ephemeral), BadgerDB (embedded), PostgreSQL (distributed)

**pkg/payload/**
- Purpose: File content management and persistence
- Key abstractions: `BlockStore` interface, `PayloadService` router, `TransferManager`
- Implementations: Memory, filesystem, S3

**pkg/cache/**
- Purpose: In-memory block buffer caching with crash recovery
- Key feature: WAL persistence via `wal.MmapPersister`
- Mandatory for all content operations (single global cache)

**pkg/controlplane/**
- Purpose: Central orchestration and REST API
- Split: `store/` (persistent via GORM), `runtime/` (ephemeral in-memory)
- Models: User, Group, Share, Adapter, Setting

**pkg/config/**
- Purpose: YAML/env configuration parsing and validation
- Pattern: Named stores registry (metadata, payload) referenced by shares
- Flow: config.go → defaults.go → stores.go → runtime.go initialization

**internal/protocol/nfs/**
- Purpose: NFSv3 protocol implementation
- Structure: `dispatch.go` (RPC routing), `v3/handlers/` (procedure implementations)
- XDR encoding: `xdr/` package for binary wire format

**internal/controlplane/api/**
- Purpose: REST API server and handlers
- Structure: `handlers/` (resource endpoints), `middleware/` (JWT auth), `auth/` (JWT service)
- Patterns: All handlers delegate to Runtime for state changes

**test/**
- Purpose: Test suites with different scopes
- Integration: All metadata/payload store combinations
- E2E: Real NFS mounts, all storage backend combinations

## Key File Locations

**Entry Points:**
- `cmd/dittofs/main.go`: Server daemon startup (calls `commands.Execute()`)
- `cmd/dittofs/commands/start.go`: Server startup logic (loads config, starts adapters)
- `cmd/dittofsctl/main.go`: Client CLI startup (calls `commands.Execute()`)
- `pkg/controlplane/runtime/runtime.go:Serve()`: Main server loop (blocks until shutdown)

**Configuration:**
- `pkg/config/config.go`: Config struct definition and nested configs
- `pkg/config/defaults.go`: Default values filled in before validation
- `cmd/dittofs/commands/config/init.go`: Initial config file generation

**Core Logic:**
- `pkg/metadata/service.go`: Metadata operations routing by share
- `pkg/payload/service.go`: Payload operations routing by share
- `pkg/controlplane/runtime/runtime.go`: Share/adapter/mount management
- `pkg/adapter/nfs/nfs_adapter.go`: NFS server implementation

**Testing:**
- `test/integration/`: Integration tests with all store implementations
- `test/e2e/`: End-to-end tests with real NFS mounts and file operations
- `pkg/metadata/store/*/`: Store-specific tests in implementation directories

**CLI:**
- `cmd/dittofsctl/commands/`: Resource-specific command groups
- `cmd/dittofsctl/cmdutil/util.go`: Shared utilities (auth, output, error handling)
- `internal/cli/output/`: Output formatters (table, JSON, YAML)
- `internal/cli/prompt/`: Interactive prompts

**API:**
- `internal/controlplane/api/handlers/`: HTTP request handlers
- `internal/controlplane/api/middleware/auth.go`: JWT authentication
- `internal/controlplane/api/auth/jwt_service.go`: Token generation/validation
- `pkg/apiclient/`: REST API client library used by CLI and tests

## Naming Conventions

**Files:**
- `*_test.go`: Unit tests (run with `go test ./...`)
- `*_integration_test.go`: Integration tests (tagged with `// +build integration`)
- `*_test.go` (in test/ directory): E2E tests (tagged with `// +build e2e`)
- `interface.go`: Interface definitions (when multiple interfaces in one file)
- `types.go`: Type definitions (structs, constants, enums)
- `errors.go`: Error type definitions
- Handler files: Named after operation (`create.go`, `list.go`, `get.go`)

**Directories:**
- `store/` + subdirs: Store implementations (memory, badger, postgres, fs, s3)
- `handlers/` + subdirs: Protocol/API procedure implementations
- `models/` + subdirs: Domain model types (rarely used, usually in types.go)
- `*_test/` or `testing/`: Test utilities and fixtures

**Packages:**
- PascalCase for exported types
- camelCase for exported functions
- UPPER_CASE for exported constants (rare, mostly enum-like types)
- `_test` suffix for test packages (allows whitebox testing)

**Functions:**
- `New*` constructors (e.g., `NewCache`, `NewMemoryStore`)
- `With*` builders (e.g., `WithTimeout`, `WithContext`)
- `Get*` for read operations (e.g., `GetFile`, `GetChildren`)
- `Set*` for write operations (e.g., `SetFile`, `SetParent`)
- `Check*` for validation (e.g., `CheckAccess`, `CheckExportAccess`)
- `Handle*` for protocol handlers (e.g., `HandleRead`, `HandleWrite`)

## Where to Add New Code

**New Feature (Protocol Operation):**
- Protocol handler: `internal/protocol/nfs/v3/handlers/` (for NFS)
- Metadata operation: `pkg/metadata/service.go` (add to MetadataService)
- Payload operation: `pkg/payload/service.go` (add to PayloadService)
- Test: `internal/protocol/nfs/v3/handlers/testing/` or feature-specific

**New Store Backend (Metadata):**
- Interface implementation: `pkg/metadata/store/{implementation}/store.go`
- Implement all Files, Objects, Shares interfaces
- Place in: `pkg/metadata/store/{newstore}/`
- Test file: `pkg/metadata/store/{newstore}/{newstore}_integration_test.go`

**New Store Backend (Payload):**
- Interface implementation: `pkg/payload/store/{implementation}/store.go`
- Implement BlockStore interface (WriteBlock, ReadBlock, DeleteBlock, etc)
- Place in: `pkg/payload/store/{newstore}/`
- Test file: `pkg/payload/store/{newstore}/{newstore}_integration_test.go`

**New Protocol Adapter:**
- Implementation: `pkg/adapter/{protocol}/adapter.go`
- Implement Adapter interface (Serve, SetRuntime, Stop, Protocol, Port)
- Protocol handlers: `internal/protocol/{protocol}/handlers/`
- Registration: Update `cmd/dittofs/main.go` adapter factory

**New CLI Command:**
- Command file: `cmd/dittofs/commands/` or `cmd/dittofsctl/commands/`
- Subcommand group: Create new subdirectory if grouping related commands
- Example: `cmd/dittofsctl/commands/share/permission/grant.go`
- Register: Add to parent command's `init()` function

**New REST API Endpoint:**
- Handler: `internal/controlplane/api/handlers/{resource}.go`
- Client method: `pkg/apiclient/{resource}.go`
- Route registration: `pkg/controlplane/api/api.go`
- Test: `internal/controlplane/api/handlers/{resource}_test.go`

**Shared Utilities:**
- General utilities: `pkg/` (if stable API)
- CLI utilities: `internal/cli/{utility}/`
- Internal utilities: `internal/{utility}/`
- Not exported: Keep in implementation package, don't create `internal/` package

## Special Directories

**pkg/controlplane/store/migrations/**
- Purpose: SQL migration files for database schema
- Format: Embedded files (using `go:embed`)
- Location: `postgres/migrations/` subdirectory for PostgreSQL-specific migrations
- Generated: No (checked into repo)
- Committed: Yes (part of source control)

**internal/protocol/nfs/v3/handlers/testing/**
- Purpose: Test fixtures and mock implementations
- Examples: Mock metadata stores, test file builders
- Generated: No
- Committed: Yes

**test/e2e/**
- Purpose: End-to-end tests with real NFS mounts
- Requires: NFS client, sudo access, disk space
- Generated: No (but test output/logs may be temporary)
- Committed: Yes (test code)

**.planning/codebase/**
- Purpose: GSD codebase analysis documents
- Generated: Yes (by gsd:map-codebase)
- Committed: Yes (to git)

## Import Organization

**Order by package group:**
1. Standard library imports (`fmt`, `io`, `net`, etc)
2. Third-party imports (`github.com/`, custom go.mod dependencies)
3. Local imports (`github.com/marmos91/dittofs/pkg/`, `github.com/marmos91/dittofs/internal/`)

**Example:**
```go
import (
    "context"
    "fmt"
    "net"

    "github.com/spf13/cobra"
    "go.uber.org/zap"

    "github.com/marmos91/dittofs/internal/logger"
    "github.com/marmos91/dittofs/pkg/adapter"
    "github.com/marmos91/dittofs/pkg/metadata"
)
```

**Path Aliases:**
- Not used (DittoFS is a monolith without deep nesting)
- Full import paths preferred for clarity

---

*Structure analysis: 2026-02-02*
