# Codebase Structure

**Analysis Date:** 2026-02-09

## Directory Layout

```
dittofs/
├── cmd/                           # CLI binary entry points
│   ├── dittofs/                   # Server daemon CLI
│   │   ├── main.go               # Entry point
│   │   └── commands/             # Cobra command handlers
│   │       ├── root.go           # Root command, global flags
│   │       ├── start.go          # Start server
│   │       ├── stop.go           # Stop server
│   │       ├── status.go         # Server status
│   │       ├── logs.go           # Tail logs
│   │       ├── init.go           # Initialize config
│   │       ├── migrate.go        # Database migrations
│   │       ├── version.go        # Version info
│   │       ├── completion.go     # Shell completion
│   │       ├── config/           # Config subcommands (init, show, validate, edit, schema)
│   │       ├── backup/           # Backup subcommands (controlplane)
│   │       └── restore/          # Restore subcommands (controlplane)
│   │
│   └── dittofsctl/               # Client CLI binary (remote management)
│       ├── main.go              # Entry point
│       ├── cmdutil/             # Shared utilities (auth, output, flags)
│       │   └── util.go
│       └── commands/            # Cobra commands
│           ├── root.go          # Root command, global flags (-o, --no-color)
│           ├── login.go         # Authentication
│           ├── logout.go
│           ├── version.go
│           ├── completion.go
│           ├── context/         # Multi-server context management
│           ├── user/            # User CRUD
│           ├── group/           # Group CRUD
│           ├── share/           # Share management
│           │   └── permission/  # Share permissions
│           ├── store/           # Store management
│           │   ├── metadata/    # Metadata store management
│           │   └── payload/     # Payload store management
│           ├── adapter/         # Protocol adapter management
│           └── settings/        # Server settings
│
├── pkg/                          # Public API packages (stable interfaces)
│   ├── adapter/                  # Protocol adapter interface and implementations
│   │   ├── adapter.go           # Core Adapter interface
│   │   ├── nfs/                 # NFSv3 adapter
│   │   │   ├── nfs_adapter.go
│   │   │   └── nfs_connection.go
│   │   └── smb/                 # SMBv2 adapter
│   │       ├── smb_adapter.go
│   │       └── smb_connection.go
│   │
│   ├── metadata/                 # Metadata service and store interface
│   │   ├── service.go           # MetadataService (business logic, routing)
│   │   ├── store.go             # MetadataStore interface
│   │   ├── interface.go         # MetadataServiceInterface
│   │   ├── authentication.go    # Auth context and permission checks
│   │   ├── errors.go            # ExportError type mapping to NFS codes
│   │   ├── types.go             # File, FileAttr, FileHandle types
│   │   ├── file.go              # File operations logic
│   │   ├── directory.go         # Directory operations logic
│   │   ├── io.go                # I/O preparation/commit patterns
│   │   ├── locking.go           # Lock manager for byte-range locks
│   │   ├── chunks.go            # Chunk/block hierarchy types
│   │   ├── object.go            # Object store (content-addressed metadata)
│   │   ├── pending_writes.go    # Write coordination state
│   │   └── store/               # MetadataStore implementations
│   │       ├── memory/          # In-memory store (ephemeral, testing)
│   │       ├── badger/          # BadgerDB store (persistent, path-based handles)
│   │       └── postgres/        # PostgreSQL store (distributed, UUID handles)
│   │
│   ├── payload/                  # Payload (content) service
│   │   ├── service.go           # PayloadService (cache + transfer coordination)
│   │   ├── types.go             # FlushResult, StorageStats types
│   │   ├── errors.go            # Payload-specific errors
│   │   ├── chunk/               # Chunk boundary calculations
│   │   ├── store/               # Block store interface and implementations
│   │   │   ├── store.go         # BlockStore interface
│   │   │   ├── memory/          # In-memory block store
│   │   │   ├── fs/              # Filesystem block store
│   │   │   └── s3/              # S3 block store (production-ready)
│   │   │       └── s3.go
│   │   └── transfer/            # Transfer manager (async persistence)
│   │       ├── manager.go       # TransferManager
│   │       ├── queue.go         # TransferQueue
│   │       ├── entry.go         # TransferQueueEntry interface
│   │       └── recovery.go      # WAL recovery on startup
│   │
│   ├── cache/                    # Block-aware cache with WAL
│   │   ├── cache.go             # Cache implementation (LRU, dirty tracking)
│   │   ├── read.go              # Cache read operations
│   │   ├── write.go             # Cache write operations
│   │   ├── flush.go             # Cache flush to block store
│   │   ├── eviction.go          # LRU eviction with dirty protection
│   │   ├── state.go             # Block state machine
│   │   ├── types.go             # BlockState, PendingBlock types
│   │   └── wal/                 # Write-Ahead Log persistence
│   │       ├── persister.go     # Persister interface + NullPersister
│   │       ├── mmap.go          # MmapPersister (memory-mapped file)
│   │       └── types.go         # BlockWriteEntry, WAL record types
│   │
│   ├── controlplane/             # Control plane (configuration + runtime)
│   │   ├── models/              # Domain models (User, Group, Share, Adapter)
│   │   │   ├── user.go
│   │   │   ├── group.go
│   │   │   ├── share.go
│   │   │   ├── adapter.go
│   │   │   ├── setting.go
│   │   │   └── types.go         # Permission types, enums
│   │   ├── store/               # GORM-based persistent storage
│   │   │   ├── store.go         # Store interface
│   │   │   ├── gorm.go          # GORMStore implementation
│   │   │   ├── users.go         # User CRUD + auth
│   │   │   ├── groups.go        # Group CRUD
│   │   │   ├── shares.go        # Share CRUD
│   │   │   ├── permissions.go   # Permission resolution
│   │   │   ├── settings.go      # Settings CRUD
│   │   │   └── adapters.go      # Adapter config CRUD
│   │   ├── runtime/             # Ephemeral runtime state manager
│   │   │   ├── runtime.go       # Runtime manager (shares, stores, adapters)
│   │   │   ├── init.go          # Runtime initialization from store
│   │   │   ├── share.go         # Share management and root handle tracking
│   │   │   └── mounts.go        # NFS mount tracking
│   │   └── api/                 # REST API server config
│   │       └── api.go           # APIConfig struct
│   │
│   ├── config/                   # Configuration parsing and validation
│   │   ├── config.go            # Main Config struct
│   │   ├── stores.go            # Factory functions for store creation
│   │   ├── runtime.go           # Runtime initialization from config
│   │   ├── defaults.go          # Default configuration values
│   │   └── init.go              # Config file generation ('dittofs init')
│   │
│   ├── apiclient/                # REST API client library
│   │   ├── client.go            # HTTP client with JWT auth
│   │   ├── users.go             # User API methods
│   │   ├── groups.go            # Group API methods
│   │   ├── shares.go            # Share API methods
│   │   ├── stores.go            # Store API methods
│   │   ├── adapters.go          # Adapter API methods
│   │   ├── settings.go          # Settings API methods
│   │   └── errors.go            # API error types
│   │
│   └── metrics/                  # Metrics collection (optional)
│       ├── metrics.go           # Metrics interface
│       └── prometheus/          # Prometheus implementation
│           └── prometheus.go
│
├── internal/                     # Private implementation details
│   ├── logger/                  # Structured logging
│   │   └── logger.go            # Logger with configurable level/format
│   │
│   ├── cli/                     # CLI utilities (shared between dittofs and dittofsctl)
│   │   ├── output/              # Output formatting
│   │   │   ├── table.go         # Table format
│   │   │   ├── json.go          # JSON format
│   │   │   └── yaml.go          # YAML format
│   │   ├── prompt/              # Interactive prompts
│   │   │   ├── confirm.go       # Yes/no confirmation
│   │   │   ├── password.go      # Password prompt
│   │   │   └── select.go        # Menu selection
│   │   ├── credentials/         # Multi-context credential storage
│   │   │   └── credentials.go
│   │   ├── health/              # Server health checks
│   │   │   └── health.go
│   │   └── timeutil/            # Time formatting utilities
│   │       └── timeutil.go
│   │
│   ├── protocol/                # Wire protocol implementations
│   │   ├── nfs/                 # NFSv3 + Mount protocols
│   │   │   ├── dispatch.go      # RPC procedure routing
│   │   │   ├── doc.go           # Package documentation
│   │   │   ├── rpc/             # RPC layer (call/reply handling)
│   │   │   │   └── message.go
│   │   │   ├── xdr/             # XDR encoding/decoding
│   │   │   │   └── types.go
│   │   │   ├── types/           # NFS constants and types
│   │   │   │   ├── types.go
│   │   │   │   └── constants.go
│   │   │   ├── mount/           # Mount protocol handlers
│   │   │   │   └── handlers/
│   │   │   │       ├── mnt.go   # MNT procedure
│   │   │   │       ├── umnt.go  # UMNT procedure
│   │   │   │       ├── export.go# EXPORT procedure
│   │   │   │       └── dump.go  # DUMP procedure
│   │   │   └── v3/              # NFSv3 protocol handlers
│   │   │       ├── doc.go
│   │   │       ├── handlers/    # Individual procedure handlers
│   │   │       │   ├── read.go          # READ procedure
│   │   │       │   ├── write.go         # WRITE procedure
│   │   │       │   ├── lookup.go        # LOOKUP procedure
│   │   │       │   ├── create.go        # CREATE procedure
│   │   │       │   ├── mkdir.go         # MKDIR procedure
│   │   │       │   ├── remove.go        # REMOVE procedure
│   │   │       │   ├── rmdir.go         # RMDIR procedure
│   │   │       │   ├── rename.go        # RENAME procedure
│   │   │       │   ├── readdir.go       # READDIR procedure
│   │   │       │   ├── readdirplus.go   # READDIRPLUS procedure
│   │   │       │   ├── symlink.go       # SYMLINK procedure
│   │   │       │   ├── link.go          # LINK procedure
│   │   │       │   ├── getattr.go       # GETATTR procedure
│   │   │       │   ├── setattr.go       # SETATTR procedure
│   │   │       │   ├── commit.go        # COMMIT procedure (flush coordination)
│   │   │       │   ├── fsinfo.go        # FSINFO procedure
│   │   │       │   └── utils.go         # Common handler utilities
│   │   │       └── testing/    # Handler testing utilities
│   │   │
│   │   └── smb/                 # SMBv2 protocol
│   │       ├── rpc/             # SMB message handling
│   │       ├── v2/              # SMBv2 procedures
│   │       │   └── handlers/    # SMB procedure handlers
│   │       ├── header/          # SMB header parsing
│   │       ├── session/         # Session state management
│   │       ├── signing/         # SMB signing implementation
│   │       └── types/           # SMB constants and types
│   │
│   ├── controlplane/            # Control plane API server
│   │   └── api/                 # REST API implementation
│   │       ├── middleware/      # HTTP middleware (JWT auth, CORS)
│   │       │   └── auth.go
│   │       ├── auth/            # JWT service
│   │       │   ├── jwt.go       # JWT token creation/validation
│   │       │   └── claims.go    # JWT claims structure
│   │       └── handlers/        # HTTP handlers for each resource
│   │           ├── users.go
│   │           ├── groups.go
│   │           ├── shares.go
│   │           ├── stores.go
│   │           ├── adapters.go
│   │           └── settings.go
│   │
│   ├── auth/                    # Protocol authentication
│   │   ├── ntlm/                # NTLM authentication
│   │   │   └── ntlm.go
│   │   └── spnego/              # SPNEGO authentication
│   │       └── spnego.go
│   │
│   ├── bufpool/                 # Buffer pooling for I/O
│   │   └── bufpool.go           # Three-tier buffer pool (4KB/64KB/1MB)
│   │
│   ├── bytesize/                # Byte size parsing
│   │   └── bytesize.go          # Parse "1MB", "512KB" etc.
│   │
│   ├── mfsymlink/               # Symbolic link utilities
│   │   └── mfsymlink.go
│   │
│   └── telemetry/               # OpenTelemetry tracing and profiling
│       └── telemetry.go         # Tracing and Pyroscope profiling config
│
├── test/                        # Test suites
│   ├── e2e/                     # End-to-end tests (real NFS mounts)
│   │   ├── framework/           # Test framework (server startup, mount management)
│   │   ├── helpers/             # Test utilities (file operations, assertions)
│   │   ├── run-e2e.sh          # Test runner script
│   │   └── *_test.go           # Test cases
│   │
│   └── posix/                   # POSIX compliance tests
│       ├── configs/             # Test configuration files
│       └── results/             # Test result output
│
├── docs/                        # User and contributor documentation
│   ├── ARCHITECTURE.md          # Design patterns and implementation
│   ├── CONFIGURATION.md         # Configuration guide with examples
│   ├── NFS.md                   # NFSv3 protocol details
│   ├── CONTRIBUTING.md          # Development guide
│   ├── IMPLEMENTING_STORES.md   # Guide for custom store implementations
│   ├── TROUBLESHOOTING.md       # Common issues and solutions
│   ├── SECURITY.md              # Security considerations
│   ├── FAQ.md                   # Frequently asked questions
│   ├── RELEASING.md             # Release process
│   └── KNOWN_LIMITATIONS.md     # Limitations and compliance
│
├── monitoring/                  # Monitoring and observability configs
│   ├── prometheus/              # Prometheus configs
│   └── grafana/                 # Grafana dashboard definitions
│
├── k8s/                         # Kubernetes integration
│   └── dittofs-operator/        # Kubernetes operator for DittoFS
│       ├── api/                 # CRD definitions
│       ├── internal/            # Controller implementation
│       ├── config/              # K8s manifests (RBAC, CRDs, etc.)
│       ├── chart/               # Helm chart
│       └── utils/               # K8s utilities
│
├── CLAUDE.md                    # Project instructions (non-obvious conventions)
├── CONTRIBUTING                 # Contribution guidelines
├── LICENSE                      # License file
├── README.md                    # Quick start and overview
├── go.mod                       # Go module definition
├── go.sum                       # Go module checksums
├── Dockerfile                   # Docker image build
├── Dockerfile.goreleaser        # Release build image
├── docker-compose.yml           # Local development environment
├── flake.nix                    # Nix flake (reproducible builds)
├── .golangci.yml               # Linting config
├── .markdownlint.yaml          # Markdown linting
└── .goreleaser.yml             # Release automation config
```

## Directory Purposes

**cmd/:**
- Entry points for both server and client binaries
- All CLI logic lives in `commands/` subdirectories
- Uses Cobra for command structure and shell completion
- Global flags (config file, verbosity) defined in `root.go`

**pkg/:**
- Public API packages with stable interfaces
- Import these to integrate DittoFS into other projects
- Each subdirectory is a complete module (adapter, service, store, etc.)
- Interfaces in package root, implementations in subdirectories

**internal/:**
- Private implementation details not meant for external use
- Protocol handlers are implementation-specific (NFSv3 wire format, SMBv2 frame format)
- CLI utilities shared between dittofs and dittofsctl
- Logging, authentication, telemetry infrastructure

**test/:**
- E2E tests run against real NFS/SMB mounts
- POSIX compliance verification
- Tests require special permissions (NFS mounting) and external tools

**docs/:**
- User-facing documentation (deployment, configuration, security)
- Developer guides (architecture, contributing, implementing stores)
- Problem-solving (troubleshooting, known limitations, FAQ)

## Key File Locations

**Entry Points:**
- `cmd/dittofs/main.go`: Server daemon entry point
- `cmd/dittofsctl/main.go`: Remote CLI entry point
- `cmd/dittofs/commands/root.go`: Root command with global flags
- `cmd/dittofs/commands/start.go`: Server startup logic (loads config, initializes runtime, launches adapters)

**Configuration:**
- `pkg/config/config.go`: Main Config struct (logging, telemetry, database, cache, admin)
- `pkg/config/defaults.go`: Default values for all settings
- `pkg/config/stores.go`: Factory functions for creating stores from config
- `cmd/dittofs/commands/config/`: Config subcommands (init, show, validate, edit, schema)

**Core Logic:**
- `pkg/controlplane/runtime/runtime.go`: Central runtime manager (orchestrates stores, shares, adapters)
- `pkg/controlplane/store/gorm.go`: GORM-based persistent storage (users, groups, shares, adapters)
- `pkg/metadata/service.go`: File operation routing and business logic
- `pkg/metadata/store.go`: MetadataStore interface defining metadata persistence contract
- `pkg/payload/service.go`: PayloadService coordinating cache and transfers
- `pkg/adapter/nfs/nfs_adapter.go`: NFSv3 server with graceful shutdown
- `pkg/adapter/smb/smb_adapter.go`: SMBv2 server
- `internal/protocol/nfs/dispatch.go`: RPC procedure routing and auth context extraction

**Testing:**
- `test/e2e/`: End-to-end tests with real NFS mounts
- `test/posix/`: POSIX compliance tests
- `*_test.go`: Unit tests throughout codebase (co-located with code)

## Naming Conventions

**Files:**
- Handlers in `internal/protocol/{nfs,smb}/`: One file per RPC procedure (read.go, write.go, lookup.go, etc.)
- Tests: `*_test.go` co-located with source code
- Implementations: Subdirectories named for concrete type (memory/, badger/, s3/, postgres/)
- Commands: One file per subcommand, e.g. `user.go`, `group.go`, `share.go`

**Directories:**
- Command packages: lowercase plural (commands/)
- Store implementations: lowercase (memory/, badger/, s3/, postgres/)
- Protocol subdirectories: protocol name lowercase (nfs/, smb/)
- Nested protocol modules: logical grouping (handlers/, rpc/, types/, etc.)

**Packages:**
- All lowercase, no underscores or dashes
- Short names (adapter, metadata, payload, config, cache, etc.)
- Plural for collections (handlers, metrics, models)

**Types:**
- PascalCase for exported types (Config, Handler, Service, Store, etc.)
- Interfaces end in 'Interface' or descriptive verb (Reader, Writer, closer via io.Closer pattern)
- Implementation types reflect backend (MemoryStore, BadgerStore, S3Store)

**Functions/Methods:**
- camelCase (getData, createFile, handleWrite, etc.)
- Verb-first for actions (GetFile, CreateDirectory, HandleLookup, etc.)
- Noun-first for accessor/factory (NewHandler, NewStore, etc.)

**Constants:**
- SCREAMING_SNAKE_CASE for iota enums (NFS3_OK, NFS3_ERR_*, etc.)
- Mixed case for errors (ErrNotFound, ErrAccess, ErrNoEntity, etc.)
- Uppercase for package-level constants (DefaultTimeout, MaxConnections, etc.)

## Where to Add New Code

**New NFS Procedure:**
- Implement handler in `internal/protocol/nfs/v3/handlers/{procedure_name}.go`
- Add test in `internal/protocol/nfs/v3/handlers/{procedure_name}_test.go`
- Register in dispatch table in `internal/protocol/nfs/dispatch.go`
- Update README with procedure support matrix

**New SMB Procedure:**
- Implement handler in `internal/protocol/smb/v2/handlers/{procedure_name}.go`
- Add test in `internal/protocol/smb/v2/handlers/{procedure_name}_test.go`
- Register in dispatch table in `internal/protocol/smb/rpc/dispatch.go`

**New Metadata Store Backend:**
- Create package in `pkg/metadata/store/{backend}/`
- Implement `MetadataStore` interface from `pkg/metadata/store.go`
- Add factory in `pkg/config/stores.go`
- Add integration tests in `test/integration/`
- Document in `docs/IMPLEMENTING_STORES.md`

**New Block Store Backend:**
- Create package in `pkg/payload/store/{backend}/`
- Implement `BlockStore` interface from `pkg/payload/store/store.go`
- Add factory in `pkg/config/stores.go`
- Add integration tests in `test/integration/`

**New Protocol Adapter:**
- Create package in `pkg/adapter/{protocol}/`
- Implement `adapter.Adapter` interface
- Create connection handler type
- Add factory function in `cmd/dittofs/commands/start.go`
- Update README with protocol documentation

**New API Endpoint:**
- Add handler in `internal/controlplane/api/handlers/{resource}.go`
- Add HTTP routes in handler package
- Add client method in `pkg/apiclient/{resource}.go`
- Add dittofsctl command in `cmd/dittofsctl/commands/{resource}/`

**New CLI Command:**
- Create file in `cmd/{dittofs,dittofsctl}/commands/{command}.go`
- Define Cobra Command struct and handler function
- Register in `commands/root.go`
- Add tests as needed
- Update shell completion

**Shared Utilities:**
- Helper functions: `internal/cli/`
- Buffer pooling: `internal/bufpool/`
- Logging: `internal/logger/`
- Authentication: `internal/auth/`
- Protocol-specific utils: `internal/protocol/{protocol}/` subdirs

## Special Directories

**vendor/:**
- Purpose: Go module dependencies (managed by `go mod tidy`)
- Generated: Yes
- Committed: No (git-ignored by default, included for offline builds)

**.planning/codebase/:**
- Purpose: Generated codebase analysis documents for GSD phase planning
- Generated: Yes (by /gsd:map-codebase)
- Committed: Yes (reference documents for planning phases)

**docs/:**
- Purpose: User and developer documentation
- Generated: No (manually maintained)
- Committed: Yes

**monitoring/:**
- Purpose: Prometheus/Grafana configurations for observability
- Generated: No
- Committed: Yes

**k8s/dittofs-operator/:**
- Purpose: Kubernetes operator and Helm chart
- Generated: Partially (CRDs generated from Go types)
- Committed: Yes

**test/e2e/:**
- Purpose: End-to-end test suite requiring real NFS mounts
- Generated: No (test code)
- Committed: Yes
- Special: Requires `sudo`, NFS client, and test runner script

---

*Structure analysis: 2026-02-09*
