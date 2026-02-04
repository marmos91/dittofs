# Codebase Structure

**Analysis Date:** 2026-02-04

## Directory Layout

```
dittofs/
├── cmd/                            # CLI binaries (server + client)
│   ├── dittofs/                    # Server daemon CLI
│   │   ├── main.go                 # Entry point
│   │   └── commands/               # Cobra command implementations
│   │       ├── root.go             # Root command, global --config flag
│   │       ├── start.go            # Server startup (main entrypoint)
│   │       ├── init.go             # Initialize config file
│   │       ├── migrate.go          # Database migrations
│   │       ├── stop.go             # Graceful shutdown signal
│   │       ├── status.go           # Server health check
│   │       ├── logs.go             # Tail logs
│   │       ├── backup/             # Control plane backup
│   │       ├── restore/            # Control plane restore
│   │       ├── config/             # Config management (show, validate, edit, schema)
│   │       ├── version.go
│   │       ├── completion.go       # Shell completion
│   │       └── util.go
│   │
│   └── dittofsctl/                 # Client CLI for remote management
│       ├── main.go                 # Entry point
│       ├── cmdutil/                # Shared utilities
│       │   └── util.go             # HTTP client, output formatting, auth helpers
│       └── commands/               # REST API client commands
│           ├── root.go             # Global -o, --no-color flags
│           ├── login.go, logout.go # Authentication
│           ├── context/            # Multi-server context (list, set, rename, delete)
│           ├── user/               # User CRUD (create, list, update, delete, change-password)
│           ├── group/              # Group CRUD + membership management
│           ├── share/              # Share management (create, list, delete)
│           │   └── permission/     # Share permissions (grant, revoke, list)
│           ├── adapter/            # Protocol adapter management
│           ├── store/              # Store management
│           │   ├── metadata/       # Metadata store management
│           │   └── payload/        # Payload store management
│           ├── settings/           # Server settings (get, set, list)
│           ├── version.go
│           └── completion.go
│
├── pkg/                            # Public stable API packages
│   ├── adapter/                    # Protocol adapters (pluggable interfaces)
│   │   ├── adapter.go              # Adapter interface definition
│   │   ├── nfs/                    # NFSv3 protocol adapter
│   │   │   ├── nfs_adapter.go      # Lifecycle, listener, connection mgmt
│   │   │   └── nfs_connection.go   # Per-connection RPC handling
│   │   └── smb/                    # SMB2 protocol adapter
│   │
│   ├── metadata/                   # Metadata service layer (file system operations)
│   │   ├── interface.go            # MetadataServiceInterface (all public methods)
│   │   ├── service.go              # MetadataService implementation
│   │   ├── store.go                # MetadataStore interface
│   │   ├── types.go                # FileAttr, FileHandle, AuthContext, etc.
│   │   ├── file.go, directory.go   # File/directory operations
│   │   ├── authentication.go       # Access control logic
│   │   ├── locking.go              # Byte-range lock management
│   │   ├── errors.go               # Domain error types (ErrAccess, ErrNoEntity, etc.)
│   │   ├── store/                  # MetadataStore implementations
│   │   │   ├── memory/             # In-memory (ephemeral, fast)
│   │   │   ├── badger/             # BadgerDB (persistent, embedded)
│   │   │   └── postgres/           # PostgreSQL (persistent, distributed)
│   │   └── CLAUDE.md               # Implementation notes, transaction rules, ObjectStore
│   │
│   ├── payload/                    # Payload service layer (file content operations)
│   │   ├── service.go              # PayloadService (coordinates cache + transfer mgr)
│   │   ├── types.go                # FlushResult, StorageStats, etc.
│   │   ├── errors.go               # Domain errors
│   │   ├── block/                  # Block model and types
│   │   ├── chunk/                  # Chunk model and types
│   │   ├── store/                  # BlockStore implementations
│   │   │   ├── store.go            # BlockStore interface
│   │   │   ├── memory/             # In-memory (ephemeral)
│   │   │   ├── s3/                 # S3-backed (production)
│   │   │   └── fs/                 # Filesystem-backed (local)
│   │   ├── transfer/               # Background upload orchestration
│   │   │   ├── manager.go          # TransferManager (flush coordination)
│   │   │   ├── queue.go            # Background upload queue with priority
│   │   │   └── recovery.go         # WAL recovery on startup
│   │   └── CLAUDE.md               # Cache + transfer model, COW splits
│   │
│   ├── cache/                      # Block-aware cache layer (mandatory for all I/O)
│   │   ├── cache.go                # Core Cache impl, 4MB block buffers, fileEntry structure
│   │   ├── write.go                # WriteAt implementation
│   │   ├── read.go                 # ReadAt, coverage checking
│   │   ├── flush.go                # GetDirtyBlocks, MarkBlockUploaded
│   │   ├── state.go                # File/block state management
│   │   ├── eviction.go             # LRU eviction with dirty data protection
│   │   ├── types.go                # BlockState enum, PendingBlock, Stats
│   │   ├── wal/                    # Write-Ahead Log persistence layer
│   │   │   ├── persister.go        # Persister interface + NullPersister
│   │   │   ├── mmap.go             # MmapPersister (memory-mapped WAL)
│   │   │   └── types.go            # BlockWriteEntry, WAL record types
│   │   └── CLAUDE.md               # Cache architecture, block pooling, eviction rules
│   │
│   ├── controlplane/               # Control plane (config + runtime management)
│   │   ├── models/                 # Domain models
│   │   │   └── models.go           # User, Group, Share, Adapter, Setting models
│   │   ├── store/                  # Persistent configuration storage (GORM-based)
│   │   │   ├── interface.go        # Store interface
│   │   │   ├── gorm.go             # GORMStore implementation (SQLite/PostgreSQL)
│   │   │   ├── users.go            # User CRUD + authentication
│   │   │   ├── groups.go           # Group CRUD + membership
│   │   │   ├── shares.go           # Share CRUD
│   │   │   ├── permissions.go      # Permission resolution
│   │   │   └── adapters.go         # Adapter config persistence
│   │   ├── runtime/                # Ephemeral runtime state manager
│   │   │   ├── runtime.go          # Core Runtime, adapter lifecycle, Serve()
│   │   │   ├── init.go             # Runtime initialization from config
│   │   │   └── share.go            # Share state management
│   │   ├── api/                    # REST API server (HTTP)
│   │   │   ├── server.go           # HTTP server setup
│   │   │   ├── handlers/           # HTTP handlers for each resource
│   │   │   │   ├── users.go        # User CRUD endpoints
│   │   │   │   ├── groups.go       # Group CRUD endpoints
│   │   │   │   ├── shares.go       # Share management endpoints
│   │   │   │   ├── adapters.go     # Adapter management endpoints
│   │   │   │   └── stores.go       # Store management endpoints
│   │   │   ├── auth/               # JWT authentication
│   │   │   │   └── jwt.go          # JWT service, claims
│   │   │   └── middleware/         # HTTP middleware
│   │   │       └── auth.go         # JWT validation, RequireAdmin
│   │   └── CLAUDE.md               # GORM zero-value handling, security, transaction rules
│   │
│   ├── config/                     # Configuration parsing & validation
│   │   ├── config.go               # Config struct, environment variable override
│   │   ├── defaults.go             # Default values for all configs
│   │   ├── validation.go           # Config validation rules
│   │   ├── init.go                 # `dittofs init` file generation
│   │   └── CLAUDE.md               # Named stores pattern, env overrides
│   │
│   ├── apiclient/                  # REST API client library (for CLI)
│   │   ├── client.go               # HTTP client with JWT token auth
│   │   ├── users.go, groups.go     # Resource-specific methods
│   │   ├── shares.go, adapters.go  # ...
│   │   └── errors.go               # API error types
│   │
│   └── metrics/                    # Prometheus metrics (optional, zero overhead)
│       ├── prometheus/             # Prometheus implementation
│       │   └── metrics.go          # Metrics registration, collectors
│       └── types.go                # NFSMetrics, PayloadMetrics interfaces
│
├── internal/                       # Private implementation details (unstable API)
│   ├── protocol/                   # Low-level protocol implementations
│   │   ├── nfs/                    # NFS v3 implementation
│   │   │   ├── dispatch.go         # RPC routing, auth extraction
│   │   │   ├── rpc/                # RPC call/reply handling
│   │   │   │   └── message.go      # RPC message parsing
│   │   │   ├── xdr/                # XDR encoding/decoding
│   │   │   │   ├── encoder.go      # XDR wire format writer
│   │   │   │   ├── decoder.go      # XDR wire format reader
│   │   │   │   └── conversions.go  # MetadataToNFS, ExtractFileID, ExtractClientIP
│   │   │   ├── types/              # NFS constants (NFSv3 procedure numbers, status codes)
│   │   │   ├── mount/              # MOUNT protocol (v1, v3)
│   │   │   │   ├── dispatch.go     # Mount procedure routing
│   │   │   │   └── handlers/       # Mount handlers (MNT, UMNT, EXPORT, DUMP)
│   │   │   └── v3/                 # NFSv3 procedures
│   │   │       ├── dispatch.go     # NFSv3 procedure routing
│   │   │       └── handlers/       # 21 NFSv3 procedure handlers
│   │   │           ├── lookup.go       # LOOKUP (resolve name → handle)
│   │   │           ├── read.go         # READ (fetch file content)
│   │   │           ├── write.go        # WRITE (save file content)
│   │   │           ├── create.go       # CREATE (new file)
│   │   │           ├── mkdir.go        # MKDIR (new directory)
│   │   │           ├── remove.go       # REMOVE (delete file)
│   │   │           ├── rmdir.go        # RMDIR (delete directory)
│   │   │           ├── readdir.go      # READDIR (list directory)
│   │   │           ├── readdirplus.go  # READDIRPLUS (list + attributes)
│   │   │           ├── rename.go       # RENAME (move/rename)
│   │   │           ├── link.go         # LINK (hard link)
│   │   │           ├── symlink.go      # SYMLINK (symbolic link)
│   │   │           ├── readlink.go     # READLINK (read symlink)
│   │   │           ├── commit.go       # COMMIT (flush outstanding writes)
│   │   │           ├── getattr.go      # GETATTR (file metadata)
│   │   │           ├── setattr.go      # SETATTR (modify metadata)
│   │   │           ├── access.go       # ACCESS (permission check)
│   │   │           ├── fsinfo.go       # FSINFO (filesystem info)
│   │   │           ├── fsstat.go       # FSSTAT (filesystem stats)
│   │   │           ├── pathconf.go     # PATHCONF (POSIX pathconf)
│   │   │           ├── mknod.go        # MKNOD (create special files)
│   │   │           ├── null.go         # NULL (ping)
│   │   │           ├── *_codec.go      # XDR codecs for each procedure
│   │   │           ├── auth_helper.go  # Auth context building, identity mapping
│   │   │           ├── nfs_context.go  # NFSHandlerContext type
│   │   │           ├── utils.go        # Helper functions
│   │   │           └── testing/        # Test utilities
│   │   │
│   │   └── smb/                    # SMB2 implementation
│   │       ├── header/             # SMB2 header parsing
│   │       ├── rpc/                # SMB-RPC handling
│   │       ├── session/            # Session state machine
│   │       ├── signing/            # Message signing (HMAC)
│   │       ├── types/              # SMB2 constants
│   │       └── v2/                 # SMB2 commands
│   │           └── handlers/       # SMB2 command handlers
│   │
│   ├── cli/                        # CLI utilities (server + client shared)
│   │   ├── output/                 # Output formatting
│   │   │   ├── table.go            # Table formatter (with alignment)
│   │   │   ├── json.go             # JSON formatter
│   │   │   └── yaml.go             # YAML formatter
│   │   ├── prompt/                 # Interactive prompts
│   │   │   ├── confirm.go          # Yes/No confirmation
│   │   │   ├── password.go         # Password input (hidden)
│   │   │   └── select.go           # Multiple-choice selection
│   │   ├── credentials/            # Multi-context credential storage
│   │   │   └── store.go            # Per-context token storage (~/.config/dittofs/credentials)
│   │   └── health/                 # Health check utilities
│   │
│   ├── controlplane/               # Control plane internal details
│   │   └── api/                    # API server internals
│   │       ├── handlers.go         # Router setup, global handler registration
│   │       ├── middleware.go       # Global middleware (CORS, recovery, etc.)
│   │       └── auth/               # JWT internals
│   │
│   ├── logger/                     # Structured logging (slog-based)
│   │   ├── logger.go               # Global logger setup, context-aware logging
│   │   └── levels.go               # Log level parsing
│   │
│   ├── bufpool/                    # Buffer pooling for I/O
│   │   └── bufpool.go              # Three-tier buffer pool (4KB, 64KB, 1MB)
│   │
│   ├── bytesize/                   # Byte size parsing
│   │   └── bytesize.go             # Parse "1GB", "512MB", etc.
│   │
│   ├── auth/                       # Protocol-level authentication
│   │   ├── ntlm/                   # NTLM authentication (SMB)
│   │   └── spnego/                 # SPNEGO (Kerberos wrapper, SMB)
│   │
│   └── mfsymlink/                  # Windows-style symlink handling (SMB specific)
│       └── mfsymlink.go            # MFsymlink conversion for cross-protocol support
│
├── test/                           # Test suites
│   ├── e2e/                        # End-to-end tests (real NFS mounts)
│   │   ├── run-e2e.sh              # Test runner (with S3 Localstack support)
│   │   ├── framework/              # Test framework
│   │   │   ├── client.go           # NFS client utilities
│   │   │   ├── server.go           # Test server setup
│   │   │   ├── config.go           # Backend configuration matrix
│   │   │   └── utils.go            # Helper functions
│   │   ├── helpers/                # Test data generators
│   │   ├── *_test.go               # Actual E2E test files
│   │   └── README.md               # E2E test documentation
│   │
│   └── posix/                      # POSIX compliance testing
│       └── configs/                # Test configurations for each backend combo
│
├── docs/                           # User documentation (separate from code)
│   ├── README.md                   # Project overview
│   ├── ARCHITECTURE.md             # Design documentation
│   ├── CONFIGURATION.md            # Configuration guide
│   ├── NFS.md                      # NFS protocol details
│   ├── CONTRIBUTING.md             # Development guide
│   ├── IMPLEMENTING_STORES.md      # Custom store guide
│   ├── TROUBLESHOOTING.md          # Common issues
│   ├── SECURITY.md                 # Security considerations
│   ├── FAQ.md                      # Frequently asked questions
│   ├── RELEASING.md                # Release process
│   ├── KNOWN_LIMITATIONS.md        # Limitations and POSIX compliance
│   └── e2e/BENCHMARKS.md           # Performance benchmarks
│
├── go.mod, go.sum                  # Go module dependencies
├── Makefile or build scripts       # Build automation
└── docker-compose.yml              # Local dev environment (optional)
```

## Directory Purposes

**cmd/:**
- Purpose: Executable entry points
- Contains: Cobra command definitions, CLI argument parsing
- Key files: `cmd/dittofs/main.go`, `cmd/dittofsctl/main.go`

**pkg/:**
- Purpose: Public stable API packages (versioned interfaces)
- Contains: All public protocols, services, and integrations
- Key files: Service interfaces, adapter implementations

**internal/:**
- Purpose: Implementation details, unstable APIs
- Contains: Protocol handlers, logging, auth, utilities
- Rule: Never import internal/ from public packages

**test/:**
- Purpose: Test suites
- Contains: E2E tests with real NFS mounts, POSIX compliance testing
- Key tools: run-e2e.sh script, test framework with backend matrix

**docs/:**
- Purpose: User-facing documentation
- Contains: Architecture guides, configuration examples, troubleshooting
- Format: Markdown, not code comments

## Key File Locations

**Entry Points:**
- `cmd/dittofs/main.go` - Server binary entry
- `cmd/dittofs/commands/start.go` - Server startup logic (main handler)
- `cmd/dittofsctl/main.go` - Client binary entry
- `pkg/controlplane/runtime/runtime.go:Serve()` - Central runtime loop

**Configuration:**
- `pkg/config/config.go` - Config struct definition, environment override via Viper
- `pkg/config/defaults.go` - Default values filled before validation
- `pkg/config/init.go` - Initial config file generation

**Core Logic:**
- `pkg/metadata/service.go` - File system operations routing
- `pkg/payload/service.go` - Content operations with cache coordination
- `pkg/cache/cache.go` - In-memory block buffering
- `pkg/controlplane/runtime/runtime.go` - Adapter and share lifecycle management

**Protocol Implementation:**
- `internal/protocol/nfs/dispatch.go` - NFS RPC routing and auth extraction
- `internal/protocol/nfs/v3/handlers/` - 21 NFSv3 procedure implementations
- `internal/protocol/nfs/mount/handlers/` - Mount protocol handlers
- `internal/protocol/smb/v2/handlers/` - SMB2 command handlers
- `pkg/adapter/nfs/nfs_adapter.go` - NFS listener and connection management
- `pkg/adapter/smb/` - SMB session and security handling

**Testing:**
- `test/e2e/` - End-to-end tests with real kernel NFS client
- `test/e2e/run-e2e.sh` - Test runner with Localstack integration
- `test/e2e/framework/` - Test utilities and server setup
- `test/posix/` - POSIX compliance test matrix

**API Server:**
- `pkg/controlplane/api/server.go` - HTTP server setup
- `pkg/controlplane/api/handlers/` - REST endpoint implementations
- `pkg/controlplane/api/auth/` - JWT service
- `pkg/controlplane/api/middleware/` - HTTP middleware

## Naming Conventions

**Files:**
- `*_test.go` - Unit/integration tests (run with `go test ./...`)
- `*_test.go.disabled` - Disabled tests (not run automatically)
- `*_codec.go` - XDR codec implementations (NFS protocol)
- `*_handler.go` or `handler.go` - Protocol procedure handlers
- `*.go` (no suffix) - Core implementation files
- `types.go` - Type definitions and constants
- `interface.go` - Interface definitions
- `errors.go` - Error types and helpers
- `doc.go` - Package-level documentation

**Directories:**
- `handlers/` - Protocol handler implementations
- `store/` - Storage backend implementations
- `models/` - Domain model types
- `api/` - REST/HTTP API code
- `internal/protocol/` - Wire format implementations

**Functions:**
- `New*()` - Constructor functions (e.g., `NewCache`, `NewRuntime`)
- `*Interface` - Interface types (e.g., `MetadataServiceInterface`)
- `Get*()`, `Create*()`, `Delete*()` - CRUD operations
- `Check*()` - Validation/permission check functions
- `Handle*()` - Protocol procedure handlers (NFS/SMB)
- `*WithContext()` / `*Ctx()` - Functions accepting context.Context

**Packages:**
- `pkg/` packages use plural service names (e.g., `metadata`, `payload`, `cache`)
- `internal/protocol/` packages mirror protocol names (nfs, smb)
- No underscores in package names (Go convention)

## Where to Add New Code

**New NFS Procedure:**
1. Create `internal/protocol/nfs/v3/handlers/{procedure}.go`
2. Create `internal/protocol/nfs/v3/handlers/{procedure}_codec.go`
3. Implement XDR request/response parsing
4. Call MetadataService/PayloadService methods for business logic
5. Register in `internal/protocol/nfs/v3/dispatch.go`
6. Add test file: `internal/protocol/nfs/v3/handlers/{procedure}_test.go`

**New Metadata Store Backend:**
1. Create `pkg/metadata/store/{backend}/` directory
2. Implement `MetadataStore` interface in `pkg/metadata/store/{backend}/store.go`
3. Implement per-operation files as needed
4. Update `pkg/config/stores.go` with factory function
5. Add integration tests with tag `//go:build integration`

**New Block Store Backend:**
1. Create `pkg/payload/store/{backend}/` directory
2. Implement `BlockStore` interface in `pkg/payload/store/{backend}/store.go`
3. Implement ObjectStore interface for deduplication metadata
4. Update `pkg/config/stores.go` with factory function
5. Add integration tests

**New Protocol Adapter:**
1. Create `pkg/adapter/{protocol}/` directory
2. Implement `Adapter` interface from `pkg/adapter/adapter.go`
3. Implement `SetRuntime()`, `Serve()`, `Stop()`, `Protocol()`, `Port()`
4. Create handler file with access to Runtime for share/store lookup
5. Register in `cmd/dittofs/commands/start.go` in adapter factory map

**New CLI Command:**
- Server: Add to `cmd/dittofs/commands/{name}.go`, register in `root.go:init()`
- Client: Add to `cmd/dittofsctl/commands/{resource}/{name}.go`, register in parent command

**New Configuration Option:**
1. Add field to appropriate config struct in `pkg/config/config.go`
2. Add default value in `pkg/config/defaults.go`
3. Add validation rule in `pkg/config/validation.go`
4. Update YAML parsing via mapstructure tags
5. Add environment variable override in `pkg/config/config.go:Load()`

## Special Directories

**`pkg/bufpool/`:**
- Purpose: Buffer pooling for I/O operations
- Generated: No
- Committed: Yes
- Usage: NFS handlers use for READ/WRITE buffers, reduces GC pressure ~90%

**`pkg/cache/wal/`:**
- Purpose: Write-Ahead Log for cache crash recovery
- Generated: No (but creates mmap files at runtime)
- Committed: Yes (code only)
- Files created at runtime: `{cache_path}/*.wal` (mmap-backed)

**`test/e2e/`:**
- Purpose: End-to-end test suite
- Generated: No
- Committed: Yes
- Build tag: `//go:build e2e` (excluded from `go test ./...`)
- Requirements: NFS client, sudo, optional Localstack for S3 tests

**`internal/protocol/nfs/v3/handlers/testing/`:**
- Purpose: Test utilities for NFS handlers
- Generated: No
- Committed: Yes
- Contains: Mock setup, assertion helpers, test context builders

---

*Structure analysis: 2026-02-04*
