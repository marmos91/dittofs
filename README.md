<div align="center">

# DittoFS

[![Go Version](https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![Tests](https://img.shields.io/badge/tests-passing-brightgreen?style=flat)](https://github.com/marmos91/dittofs)
[![Go Report Card](https://goreportcard.com/badge/github.com/marmos91/dittofs)](https://goreportcard.com/report/github.com/marmos91/dittofs)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg?style=flat)](LICENSE)
[![Status](https://img.shields.io/badge/status-experimental-orange?style=flat)](https://github.com/marmos91/dittofs)

**A modular virtual filesystem written entirely in Go**

Decouple file interfaces from storage backends. Currently supports **NFSv3** with pluggable metadata and content repositories. Designed for easy extension to additional protocols (SMB, WebDAV, etc.).

</div>

---

## The Problem with Traditional Filesystem Servers

Traditional filesystem server implementations face several limitations:

- **Protocol lock-in**: Each protocol implementation is tightly coupled to its storage layer
- **High-level system permissions**: Often require kernel-level access and FUSE
- **Inflexible architecture**: Cannot mix and match protocols with different storage backends
- **Complex deployment**: Multiple servers needed for multiple protocols

This results in:

- Operational complexity when supporting multiple access methods
- Performance overhead from multiple abstraction layers
- Difficult customization of storage backends
- Vendor lock-in to specific storage solutions

## The DittoFS Solution

DittoFS provides a modular architecture that separates concerns through three key abstractions:

```
        ┌──────────────────────────────────────────────┐
        │            Protocol Adapters                 │
        │                                              │
        │  ┌─────────┐  ╭ ─ ─ ─ ─ ╮  ╭ ─ ─ ─ ─ ─ ╮     │
        │  │   NFS   │    SMB(soon)    WebDAV(TBD)     │
        │  │   ✅    │  ╰ ─ ─ ─ ─ ╯  ╰ ─ ─ ─ ─ ─ ╯     │
        │  └────┬────┘                                 │
        └───────┼──────────────────────────────────────┘
                │
                ▼
        ┌─────────────────────────────────┐
        │           DittoFS Core          │
        │         (Adapter Manager)       │
        └─────────┬──────────────┬────────┘
                  │              │
                  ▼              ▼
          ┌─────────────┐  ┌──────────────┐
          │  Metadata   │  │   Content    │
          │    Store    │  │    Store     │
          └──────┬──────┘  └──────┬───────┘
                 │                │
                 ▼                ▼
          Memory/BadgerDB    S3/Filesystem
          (future: Redis)    (future: Dropbox)
```

### Key Concepts

**1. Adapters**: Protocol-specific interfaces that clients connect to

- Each adapter implements a specific file access protocol (NFS, SMB, etc.)
- Multiple adapters can run simultaneously
- Adapters are lightweight wrappers that translate protocol operations

**2. Metadata Store**: Stores file structure and attributes

- File metadata (size, timestamps, permissions, extended attributes)
- Directory hierarchy and relationships
- File handles and export configuration
- Pluggable backends (memory, BadgerDB, SQLite)
- **Built-in options**: In-memory (ephemeral) or BadgerDB (persistent)

**3. Content Store**: Stores actual file data

- Read and write operations
- Content addressing and chunking strategies
- Pluggable backends (filesystem, S3, custom solutions)
- **Built-in options**: Filesystem (local/network) or S3 (cloud)

### Key Benefits

1. **Production-Ready NFSv3**: Fully functional NFS server with 28 implemented procedures
2. **No Special Permissions**: Runs entirely in userspace - no FUSE, no kernel modules
3. **Pluggable Storage**: Mix and match protocols with any storage backend (S3, filesystem, custom)
4. **Cloud-Native Architecture**: S3 backend with production optimizations for containerized workloads
5. **Pure Go Implementation**: Easy deployment, cross-platform, simple integration
6. **Extensible Design**: Clean adapter pattern ready for SMB, WebDAV, and custom protocols

## Use Cases

### Unified Multi-Protocol Gateway

```
Adapters → NFS + SMB (simultaneous access)
Metadata → BadgerDB (persistent, ACID, embedded)
Content → S3 (scalable, durable, cost-effective)

Use case: Allow Linux servers (NFS) and Windows clients (SMB)
to access the same S3-backed storage with persistent metadata
```

### Development & Testing

```
Adapters → NFS only
Metadata → In-Memory
Content → In-Memory or local filesystem

Use case: Fast development iteration without external dependencies
```

### Persistent Production NFS Server

```
Adapters → NFS
Metadata → BadgerDB (persistent, survives restarts)
Content → Filesystem (local storage or network mount)

Use case: Production NFS server with metadata that persists
across restarts - no data loss, stable file handles
```

### High-Performance Distributed Cache

```
Adapters → NFS
Metadata → Redis (in-memory, sub-millisecond lookups)
Content → Local NVMe + S3 tiering

Use case: ML training pipelines with hot data on NVMe,
cold data automatically tiered to S3
```

## Quick Start

### Installation

```bash
go build -o dittofs cmd/dittofs/main.go
```

### Run Server

```bash
# Initialize configuration file (one-time setup)
# Creates config with BadgerDB metadata and filesystem content
./dittofs init

# Start server with default config ($XDG_CONFIG_HOME/dittofs/config.yaml)
./dittofs start

# Start with custom config
./dittofs start --config /path/to/config.yaml

# Override config with environment variables
DITTOFS_LOGGING_LEVEL=DEBUG DITTOFS_ADAPTERS_NFS_PORT=12049 ./dittofs start
```

### Mount from Client (NFS)

```bash
# Linux
sudo mount -t nfs -o nfsvers=3,tcp,port=12049 localhost:/export /mnt/nfs

# macOS
sudo mount -t nfs -o nfsvers=3,tcp,port=12049,resvport localhost:/export /mnt/nfs
```

### Testing

```bash
# Run unit tests
go test ./...

# Run unit tests with coverage
go test -cover ./...

# Run E2E tests (requires NFS client installed)
go test -v -timeout 30m ./test/e2e/...

# Run specific E2E suite
go test -v ./test/e2e -run TestE2E/memory/BasicOperations
```

DittoFS includes comprehensive testing:

- **Unit tests** for core components (RPC, XDR, metadata, content repositories)
- **E2E tests** that mount real NFS filesystems and test complete workflows
- **Test matrix** running all suites against multiple storage backends (memory, filesystem)

### Benchmarking

DittoFS includes a comprehensive benchmark suite for performance testing and comparison:

```bash
# Run all benchmarks with default settings (10s per benchmark, 3 iterations)
./scripts/benchmark.sh

# Run with CPU and memory profiling
./scripts/benchmark.sh --profile

# Compare with previous benchmark results
./scripts/benchmark.sh --compare

# Run specific benchmarks
go test -bench='BenchmarkE2E/memory/ReadThroughput' -benchtime=20s ./test/e2e/

# Run with custom configuration
BENCH_TIME=30s BENCH_COUNT=5 ./scripts/benchmark.sh
```

The benchmark suite measures:

- **Throughput**: Read/write performance from 4KB to 100MB files
- **Latency**: Per-operation timing for metadata and file operations
- **Memory Usage**: Allocation patterns and memory footprint via profiling
- **Scalability**: Performance with varying directory sizes and workloads
- **Store Comparison**: Side-by-side comparison of all storage backends

Results are saved to `benchmark_results/<timestamp>/` with:

- Raw benchmark data and profiles (CPU, memory)
- Generated reports (text and SVG graphs)
- Summary report with throughput/latency comparisons
- Comparison with previous runs (if `--compare` used)

**Documentation**:

- See [test/e2e/BENCHMARKS.md](test/e2e/BENCHMARKS.md) for detailed usage and interpretation
- See [test/e2e/COMPARISON_GUIDE.md](test/e2e/COMPARISON_GUIDE.md) for comparing with FUSE-based and kernel NFS implementations

## Configuration

DittoFS uses a flexible configuration system with support for YAML/TOML files and environment variable overrides.

### Configuration Files

**Default Location**: `$XDG_CONFIG_HOME/dittofs/config.yaml` (typically `~/.config/dittofs/config.yaml`)

**Initialization**:

```bash
# Generate default configuration file
./dittofs init

# Generate with custom path
./dittofs init --config /etc/dittofs/config.yaml

# Force overwrite existing config
./dittofs init --force
```

**Supported Formats**: YAML (`.yaml`, `.yml`) and TOML (`.toml`)

### Configuration Structure

DittoFS configuration is organized into six main sections:

#### 1. Logging

Controls log output behavior:

```yaml
logging:
  level: "INFO"           # DEBUG, INFO, WARN, ERROR
  format: "text"          # text, json
  output: "stdout"        # stdout, stderr, or file path
```

#### 2. Server Settings

Application-wide server configuration:

```yaml
server:
  shutdown_timeout: 30s   # Maximum time to wait for graceful shutdown
```

#### 3. Content Store

Configures where file content is stored.

**Filesystem Backend** (default, recommended for local storage):

```yaml
content:
  type: "filesystem"
  filesystem:
    path: "/tmp/dittofs-content"
```

**S3 Backend** (recommended for cloud deployments):

```yaml
content:
  type: "s3"
  s3:
    region: "us-east-1"              # AWS region (required)
    bucket: "my-dittofs-bucket"      # S3 bucket name (required)
    key_prefix: ""                   # Optional prefix for all keys
    endpoint: ""                     # Optional: S3-compatible endpoint
                                     # Example: "https://s3.cubbit.eu"
    access_key_id: ""                # Optional: AWS credentials
    secret_access_key: ""            # Leave empty to use AWS credential chain
    part_size: 10485760              # Multipart upload part size (10MB)
    stats_cache_ttl: "5m"            # Cache storage stats (default: 5 minutes)
    # metrics: (optional)            # Metrics interface for observability
```

**In-Memory Backend** (development/testing only):

```yaml
content:
  type: "memory"
  memory:
    max_size_bytes: 1073741824  # 1GB
```

> **Note**: Memory content store is not yet implemented. Use `filesystem` or `s3` type.
>
> **S3 Path Design**: The S3 store uses path-based object keys (e.g., `export/docs/report.pdf`) that mirror the filesystem structure. This enables easy bucket inspection and metadata reconstruction for disaster recovery.
>
> **S3 Phase 1 Production Features**: The S3 content store includes production-ready optimizations:
>
> - **Range Reads**: Efficient partial reads using S3 byte-range requests (100x faster for small reads from large files)
> - **Streaming Multipart Uploads**: Automatic multipart uploads for large files (98% memory reduction)
> - **Stats Caching**: Intelligent caching reduces expensive S3 ListObjects calls by 99%+
> - **Metrics Support**: Optional instrumentation for Prometheus/observability
>
> See `pkg/content/s3/PHASE1_SUMMARY.md` for details. Example config: `config-s3-example.yaml`

#### 4. Metadata Store

Configures where file metadata (structure, attributes) is stored.

**BadgerDB Backend** (default, recommended for production):

```yaml
metadata:
  type: "badger"
  badger:
    db_path: "/tmp/dittofs-metadata"  # Required: database directory path
```

**In-Memory Backend** (ephemeral, for development/testing only):

```yaml
metadata:
  type: "memory"
  memory: {}  # No configuration options
```

**Common Configuration** (applies to all metadata stores):

```yaml
metadata:
  # Filesystem capabilities and limits
  capabilities:
    max_read_size: 1048576        # 1MB
    preferred_read_size: 65536    # 64KB
    max_write_size: 1048576       # 1MB
    preferred_write_size: 65536   # 64KB
    max_file_size: 9223372036854775807  # ~8EB
    max_filename_len: 255
    max_path_len: 4096
    max_hard_link_count: 32767
    supports_hard_links: true
    supports_symlinks: true
    case_sensitive: true
    case_preserving: true

  # Restrict DUMP operations to specific clients (optional)
  dump_restricted: false
  dump_allowed_clients:
    - "127.0.0.1"
    - "::1"
```

> **Persistence**: BadgerDB stores all metadata persistently on disk. File handles, directory structure, permissions, and all metadata survive server restarts. The memory backend loses all data when the server stops.

#### 5. Shares (Exports)

Defines network shares/exports available to clients:

```yaml
shares:
  - name: "/export"              # Share path (must start with /)
    read_only: false             # Make share read-only
    async: true                  # Allow async writes (better performance)

    # Client access control
    allowed_clients: []          # Empty = all allowed (CIDR supported)
    denied_clients: []           # Takes precedence over allowed_clients

    # Authentication
    require_auth: false
    allowed_auth_methods:
      - "anonymous"
      - "unix"

    # Identity mapping (user/group squashing)
    identity_mapping:
      map_all_to_anonymous: true              # all_squash
      map_privileged_to_anonymous: false      # root_squash
      anonymous_uid: 65534                    # nobody
      anonymous_gid: 65534                    # nogroup

    # Root directory attributes
    root_attr:
      mode: 0755
      uid: 0
      gid: 0
```

#### 6. Protocol Adapters

Configures protocol-specific settings:

**NFS Adapter**:

```yaml
adapters:
  nfs:
    enabled: true
    port: 2049
    max_connections: 0           # 0 = unlimited
    read_timeout: 5m0s           # Max time to read request
    write_timeout: 30s           # Max time to write response
    idle_timeout: 5m0s           # Max idle time between requests
    shutdown_timeout: 30s        # Graceful shutdown timeout
    metrics_log_interval: 5m0s   # Metrics logging interval (0 = disabled)

    # Rate Limiting (Production Hardening)
    rate_limit_enabled: false              # Enable request rate limiting
    rate_limit_requests_per_second: 5000   # Max sustained request rate
    rate_limit_burst: 10000                # Max burst capacity (2x sustained rate)
```

### Environment Variables

Override configuration using environment variables with the `DITTOFS_` prefix:

**Format**: `DITTOFS_<SECTION>_<SUBSECTION>_<KEY>`

- Use uppercase
- Replace dots with underscores
- Nested paths use underscores

**Examples**:

```bash
# Logging
export DITTOFS_LOGGING_LEVEL=DEBUG
export DITTOFS_LOGGING_FORMAT=json

# Server
export DITTOFS_SERVER_SHUTDOWN_TIMEOUT=60s

# Content store
export DITTOFS_CONTENT_TYPE=filesystem
export DITTOFS_CONTENT_FILESYSTEM_PATH=/data/dittofs

# NFS adapter
export DITTOFS_ADAPTERS_NFS_ENABLED=true
export DITTOFS_ADAPTERS_NFS_PORT=12049
export DITTOFS_ADAPTERS_NFS_MAX_CONNECTIONS=1000

# NFS rate limiting
export DITTOFS_ADAPTERS_NFS_RATE_LIMIT_ENABLED=true
export DITTOFS_ADAPTERS_NFS_RATE_LIMIT_REQUESTS_PER_SECOND=10000
export DITTOFS_ADAPTERS_NFS_RATE_LIMIT_BURST=20000

# Start server with overrides
DITTOFS_LOGGING_LEVEL=DEBUG ./dittofs start
```

### Configuration Precedence

Settings are applied in the following order (highest to lowest priority):

1. **Environment Variables** (`DITTOFS_*`) - Highest priority
2. **Configuration File** (YAML/TOML)
3. **Default Values** - Lowest priority

Example:

```bash
# config.yaml has port: 2049
# This overrides it to 12049
DITTOFS_ADAPTERS_NFS_PORT=12049 ./dittofs start
```

### Configuration Examples

#### Minimal Configuration

```yaml
# Minimal config - uses all defaults
logging:
  level: "INFO"

content:
  type: "filesystem"

shares:
  - name: "/export"

adapters:
  nfs:
    enabled: true
```

#### Development Setup

Fast iteration with in-memory stores:

```yaml
logging:
  level: "DEBUG"
  format: "text"

content:
  type: "memory"
  memory:
    max_size_bytes: 1073741824  # 1GB

metadata:
  type: "memory"

shares:
  - name: "/export"
    async: true
    identity_mapping:
      map_all_to_anonymous: true

adapters:
  nfs:
    enabled: true
    port: 12049
```

#### Production Setup

Persistent storage with access control:

```yaml
logging:
  level: "WARN"
  format: "json"
  output: "/var/log/dittofs/server.log"

server:
  shutdown_timeout: 30s

content:
  type: "filesystem"
  filesystem:
    path: "/var/lib/dittofs/content"

metadata:
  type: "badger"  # Persistent metadata storage
  badger:
    db_path: "/var/lib/dittofs/metadata.db"
    max_storage_bytes: 107374182400  # 100GB
    max_files: 10000000              # 10M files
  capabilities:
    max_read_size: 1048576
    max_write_size: 1048576
  dump_restricted: true
  dump_allowed_clients:
    - "127.0.0.1"
    - "10.0.0.0/8"

shares:
  - name: "/export"
    read_only: false
    async: false  # Synchronous writes for data safety
    allowed_clients:
      - "192.168.1.0/24"
    denied_clients:
      - "192.168.1.50"
    identity_mapping:
      map_all_to_anonymous: false
      map_privileged_to_anonymous: true
      anonymous_uid: 65534
      anonymous_gid: 65534
    root_attr:
      mode: 0755
      uid: 0
      gid: 0

adapters:
  nfs:
    enabled: true
    port: 2049
    max_connections: 1000
    read_timeout: 5m0s
    write_timeout: 30s
    idle_timeout: 5m0s
    metrics_log_interval: 5m0s
```

#### Multi-Client Access Control

Different shares with different access rules:

```yaml
shares:
  # Public read-only share
  - name: "/public"
    read_only: true
    identity_mapping:
      map_all_to_anonymous: true

  # Private share - specific client network
  - name: "/private"
    read_only: false
    allowed_clients:
      - "10.0.1.0/24"
    identity_mapping:
      map_all_to_anonymous: false
      map_privileged_to_anonymous: true

  # Admin share - localhost only
  - name: "/admin"
    read_only: false
    allowed_clients:
      - "127.0.0.1"
      - "::1"
    identity_mapping:
      map_all_to_anonymous: false
      map_privileged_to_anonymous: false
```

### Viewing Active Configuration

Check the generated config file:

```bash
# Default location
cat ~/.config/dittofs/config.yaml

# Custom location
cat /path/to/config.yaml
```

Start server with debug logging to see loaded configuration:

```bash
DITTOFS_LOGGING_LEVEL=DEBUG ./dittofs start
```

## Architecture Deep Dive

### Adapter Pattern

DittoFS uses the Adapter pattern to provide clean protocol abstractions:

```go
// Adapter interface - each protocol implements this
type Adapter interface {
    Serve(ctx context.Context) error
    Stop(ctx context.Context) error
    Protocol() string
    Port() int
}

// Example: NFS Adapter
type NFSAdapter struct {
    config         NFSConfig
    metadataStore  metadata.MetadataStore
    content        content.ContentStore
}

// Multiple adapters can run concurrently
server := dittofs.New(metadataRepo, contentRepo)
server.AddAdapter(nfs.New(nfsConfig))
server.AddAdapter(smb.New(smbConfig)) // Future
server.Serve(ctx)
```

### Repository Interfaces

**Metadata Repository** - Handles file structure:

```go
type Repository interface {
    // File operations
    GetFile(ctx context.Context, handle FileHandle) (*FileAttr, error)
    CreateFile(ctx context.Context, parent FileHandle, name string) (*FileAttr, error)
    
    // Directory operations
    Lookup(ctx context.Context, dir FileHandle, name string) (*FileAttr, error)
    ReadDir(ctx context.Context, dir FileHandle) ([]*DirEntry, error)
    
    // Attribute operations
    SetAttr(ctx context.Context, handle FileHandle, attr *SetAttr) error
}
```

**Content Repository** - Handles file data:

```go
type Repository interface {
    ReadContent(ctx context.Context, id ContentID, offset int64, size uint32) ([]byte, error)
    WriteContent(ctx context.Context, id ContentID, offset int64, data []byte) error
    GetSize(ctx context.Context, id ContentID) (int64, error)
}
```

### Built-In and Custom Backends

**Using Built-In Backends** (No custom code required):

```go
// Memory backend (ephemeral)
metadataStore := memory.NewMemoryMetadataStoreWithDefaults()

// BadgerDB backend (persistent)
metadataStore, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, "/var/lib/dittofs/metadata.db")
if err != nil {
    log.Fatal(err)
}

// Wire to server
server := dittofs.New(metadataStore, contentStore)
```

**Implementing Custom Backends**:

```go
// 1. Implement metadata backend (e.g., PostgreSQL)
type PostgresRepository struct {
    db *sql.DB
}

func (r *PostgresRepository) GetFile(ctx context.Context, handle FileHandle) (*FileAttr, error) {
    var attr FileAttr
    err := r.db.QueryRowContext(ctx, 
        "SELECT size, mtime, mode FROM files WHERE handle = $1", 
        handle,
    ).Scan(&attr.Size, &attr.MTime, &attr.Mode)
    return &attr, err
}

// 2. Implement content backend (e.g., S3)
type S3ContentRepository struct {
    client *s3.Client
    bucket string
}

func (r *S3ContentRepository) ReadContent(ctx context.Context, id ContentID, offset int64, size uint32) ([]byte, error) {
    result, err := r.client.GetObject(ctx, &s3.GetObjectInput{
        Bucket: aws.String(r.bucket),
        Key:    aws.String(id.String()),
        Range:  aws.String(fmt.Sprintf("bytes=%d-%d", offset, offset+int64(size)-1)),
    })
    if err != nil {
        return nil, err
    }
    defer result.Body.Close()
    return io.ReadAll(result.Body)
}

// 3. Wire everything together
func main() {
    // Initialize repositories
    metadataRepo := NewPostgresRepository(dbConn)
    contentRepo := NewS3Repository(s3Client, "my-bucket")

    // Create and configure adapter
    nfsAdapter := nfs.New(nfs.NFSConfig{Port: 12049})

    // Start server
    server := dittofs.New(metadataRepo, contentRepo)
    server.AddAdapter(nfsAdapter)
    server.Serve(ctx)
}
```

## NFS Implementation Details

### Mounting Without Portmapper

DittoFS uses a fixed port and does not require portmapper/rpcbind.

**Mount with explicit port:**

```bash
# Linux
sudo mount -t nfs -o nfsvers=3,tcp,port=12049 localhost:/export /mnt/test

# macOS
sudo mount -t nfs -o nfsvers=3,tcp,port=12049,resvport localhost:/export /mnt/test
```

**Using showmount (traditional tools):**

Traditional tools like `showmount` require portmapper and will not work
with DittoFS. Instead, use direct mount commands or the provided test clients.

## Current Status

### ✅ Implemented

**NFS Adapter (NFSv3)**

- Core read operations (GETATTR, LOOKUP, READ, READDIR, READDIRPLUS)
- Core write operations (WRITE, CREATE, MKDIR, REMOVE, RMDIR, RENAME)
- Link operations (LINK, SYMLINK, READLINK)
- Mount protocol (MNT, UMNT, EXPORT)
- TCP transport
- File handle management

**Repositories**

- In-memory metadata repository (ephemeral, fully functional)
- BadgerDB metadata repository (persistent, path-based handles, fully functional)
- Filesystem content repository (fully functional)
- S3 content repository (production-ready with Phase 1 optimizations):
  - Range read support (ReadAt) for efficient partial reads - 100x faster for small reads
  - Streaming multipart uploads for large files - 98% memory reduction
  - Storage stats caching with configurable TTL - 99%+ cost reduction
  - Optional metrics instrumentation infrastructure

**Infrastructure**

- Adapter management framework
- XDR encoding/decoding
- RPC message handling
- Configurable logging
- Buffer pooling for performance

### 💾 Metadata Persistence

DittoFS offers two metadata storage options:

**In-Memory (Ephemeral)**

- All metadata stored in RAM
- Fast access and zero persistence overhead
- **Data is lost when server stops**
- Ideal for: Development, testing, temporary workloads

**BadgerDB (Persistent)**

- All metadata stored on disk using BadgerDB (embedded key-value database)
- **Survives server restarts** - file handles, directory structure, permissions persist
- Path-based file handles: `"shareName:/path/to/file"` format enables stable references
- Atomic operations with ACID guarantees
- Ideal for: Production deployments, long-running servers, data durability requirements

**Path-Based File Handles**

BadgerDB uses a deterministic path-based handle strategy:

- Handles are derived from the full file path: `"export:/documents/report.pdf"`
- Stable across server restarts (same file = same handle)
- Enables future features: import existing filesystems, migrate between stores, backup/restore
- Automatic fallback to hash-based handles for paths exceeding NFS 64-byte limit

**Switching Backends**

Simply change the `metadata.type` in your configuration:

```yaml
# Development: Fast, no persistence
metadata:
  type: "memory"

# Production: Persistent, durable
metadata:
  type: "badger"
  badger:
    db_path: "/var/lib/dittofs/metadata.db"
```

> **Note**: Metadata stores are not currently compatible. Switching backends requires recreating shares and file structure.

### 🚀 Roadmap

**Completed ✅**

- NFSv3 + Mount protocol (28 procedures)
- In-memory metadata repository
- BadgerDB metadata repository (persistent, path-based handles)
- Filesystem content repository
- S3 content repository with Phase 1 production improvements:
  - Range reads (ReadAt) - 100x faster for partial reads
  - Streaming multipart uploads - 98% memory reduction
  - Storage stats caching - 99%+ cost reduction
  - Metrics instrumentation infrastructure
- Comprehensive E2E test suite
- Performance benchmark framework
- Modular configuration system

**Phase 1: NFSv3 Production Hardening** ✅ (Complete)

- [x] Prometheus metrics integration
- [x] Enhanced graceful shutdown
- [x] Request rate limiting

**Phase 2: Kubernetes Integration**

- [ ] Health check endpoints (Kubernetes readiness/liveness)
- [ ] CSI driver implementation
- [ ] Helm chart for deployment
- [ ] StatefulSet examples
- [ ] PersistentVolume provisioning
- [ ] Load testing with realistic workloads

**Phase 3: SMB Protocol Adapter** (Optional)

- [ ] SMB2/3 protocol implementation (19 commands)
- [ ] NTLM authentication
- [ ] Session/tree management
- [ ] Core file operations
- [ ] Windows client compatibility

**Phase 4: Advanced Features**

- [ ] NFSv4 support
- [ ] Kerberos authentication (AUTH_GSS)
- [ ] Advanced caching strategies
- [ ] Multi-region content replication
- [ ] WebDAV adapter
- [ ] SQLite metadata repository

## Protocol Implementation Status

### Mount Protocol

| Procedure | Status | Notes |
|-----------|--------|-------|
| NULL | ✅ | |
| MNT | ✅ | |
| UMNT | ✅ | |
| UMNTALL | ✅ | |
| DUMP | ✅ | |
| EXPORT | ✅ | |

### NFS Protocol v3 - Read Operations

| Procedure | Status | Notes |
|-----------|--------|-------|
| NULL | ✅ | |
| GETATTR | ✅ | |
| SETATTR | ✅ | |
| LOOKUP | ✅ | |
| ACCESS | ✅ | |
| READ | ✅ | |
| READDIR | ✅ | |
| READDIRPLUS | ✅ | |
| FSSTAT | ✅ | |
| FSINFO | ✅ | |
| PATHCONF | ✅ | |
| READLINK | ✅ | |

### NFS Protocol v3 - Write Operations

| Procedure | Status | Notes |
|-----------|--------|-------|
| WRITE | ✅ | |
| CREATE | ✅ | |
| MKDIR | ✅ | |
| REMOVE | ✅ | |
| RMDIR | ✅ | |
| RENAME | ✅ | |
| LINK | ✅ | |
| SYMLINK | ✅ | |
| MKNOD | ✅ | Limited support |
| COMMIT | ✅ | |

## Performance Characteristics

DittoFS is designed for high performance through several architectural choices:

- **Direct protocol implementation**: No FUSE overhead
- **Goroutine-per-connection model**: Leverages Go's lightweight concurrency
- **Buffer pooling**: Reduces GC pressure for large I/O operations
- **Streaming I/O**: Efficient handling of large files without full buffering
- **Pluggable caching**: Implement custom caching strategies per use case
- **Zero-copy aspirations**: Working toward minimal data copying in hot paths

**Benchmark results** (coming in Phase 2):

- Sequential read throughput
- Random read IOPS
- Metadata operation latency
- Concurrent connection scalability

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

## Comparison with Alternatives

| Feature | Traditional NFS + FUSE | Cloud Storage Gateways | DittoFS |
|---------|------------------------|------------------------|---------|
| Permission Requirements | Kernel-level | Varies | Userspace only |
| Multi-protocol Support | Separate servers | Limited | Unified (planned) |
| Storage Backend | Filesystem only | Vendor-specific | Pluggable |
| Metadata Backend | Filesystem only | Vendor-specific | Pluggable (Memory/BadgerDB) |
| Metadata Persistence | Filesystem-bound | Varies | Optional (BadgerDB) |
| Language | C/C++ | Varies | Pure Go |
| Deployment | Complex (kernel modules) | Complex (dependencies) | Single binary |
| Customization | Limited | Limited | Full control |
| Cloud Native | No | Sometimes | Yes |

## Contributing

DittoFS is in active development and welcomes contributions!

### Areas Needing Attention

**High Priority**

- Additional repository backend implementations (Redis, PostgreSQL, S3)
- Performance optimization and profiling
- Test coverage expansion
- Protocol compliance testing

**Medium Priority**

- SMB/CIFS adapter implementation
- Documentation improvements
- Example applications and tutorials
- Monitoring and observability

**Future Work**

- WebDAV adapter
- NFSv4 support
- Advanced caching strategies
- Multi-region replication

### Development Setup

```bash
# Clone repository
git clone https://github.com/marmos91/dittofs.git
cd dittofs

# Install dependencies
go mod download

# Run unit tests
go test ./...

# Run E2E tests (requires NFS client)
go test -v -timeout 30m ./test/e2e/...

# Build
go build -o dittofs cmd/dittofs/main.go

# Run with development settings
./dittofs -log-level DEBUG
```

### E2E Testing Framework

DittoFS includes a comprehensive end-to-end testing framework that validates real-world NFS operations by:

- **Starting a real DittoFS server** with configurable backends
- **Mounting the NFS filesystem** using platform-native mount commands
- **Executing real file operations** using standard Go `os` package functions
- **Testing all combinations** of adapters and storage backends

Test suites cover:

- Basic file operations (create, read, write, delete)
- Directory operations (mkdir, readdir, rename)
- Symbolic and hard links
- File attributes and permissions
- Idempotency guarantees
- Edge cases and boundary conditions

See [test/e2e/README.md](test/e2e/README.md) for detailed documentation on running and writing E2E tests.

### Code Structure

```
dittofs/
├── cmd/dittofs/          # Main application entry point
├── pkg/                  # Public APIs
│   ├── metadata/         # Metadata repository interfaces and implementations
│   ├── content/          # Content repository interfaces and implementations
│   ├── adapter/          # Adapter interfaces and implementations
│   └── server/           # Core server logic
├── internal/             # Internal implementation details
│   ├── protocol/nfs/     # NFS protocol implementation
│   │   ├── mount/        # Mount protocol handlers
│   │   ├── v3/           # NFSv3 handlers
│   │   ├── rpc/          # RPC layer
│   │   └── xdr/          # XDR encoding/decoding
│   ├── logger/           # Logging utilities
│   └── metadata/         # Internal metadata utilities
└── docs/                 # Additional documentation (future)
```

## Troubleshooting

### Common Issues

**Cannot mount: Connection refused**

```bash
# Check if DittoFS is running
ps aux | grep dittofs

# Verify port is correct
netstat -an | grep 12049

# Check firewall rules
sudo iptables -L | grep 12049
```

**Permission denied when mounting**

```bash
# On Linux, may need to allow non-privileged ports
sudo sysctl -w net.ipv4.ip_unprivileged_port_start=0

# On macOS, must use resvport option
sudo mount -t nfs -o nfsvers=3,tcp,port=12049,resvport localhost:/export /mnt/test
```

**Stale file handle errors**

- This typically happens if the server restarts and clients have cached file handles
- Unmount and remount the filesystem on clients
- In production, implement persistent file handle mapping

## Security Considerations

⚠️ **Current Security Status**: DittoFS is experimental software and has not undergone security auditing.

**Current Implementation**

- Basic AUTH_UNIX authentication support
- No built-in encryption (use network-level encryption like WireGuard/IPsec)
- File permissions enforced at metadata layer

**Planned Security Features**

- Kerberos authentication support (AUTH_GSS)
- Built-in TLS support for RPC
- Audit logging for all operations
- Role-based access control (RBAC)
- Encryption at rest for content repository

**Production Recommendations**

- Deploy behind a VPN or use network encryption
- Implement authentication at the network layer
- Use read-only exports where appropriate
- Monitor access logs carefully
- Restrict export access by IP address

## References

### Specifications

- [RFC 1813](https://tools.ietf.org/html/rfc1813) - NFS Version 3 Protocol Specification
- [RFC 5531](https://tools.ietf.org/html/rfc5531) - RPC: Remote Procedure Call Protocol Specification
- [RFC 4506](https://tools.ietf.org/html/rfc4506) - XDR: External Data Representation Standard
- [RFC 1094](https://tools.ietf.org/html/rfc1094) - NFS: Network File System Protocol (Version 2)

### Related Projects

- [go-nfs](https://github.com/willscott/go-nfs) - Another NFS implementation in Go
- [FUSE](https://github.com/libfuse/libfuse) - Filesystem in Userspace

## FAQ

**Q: Why not use FUSE?**

A: FUSE adds an additional abstraction layer and requires kernel modules. DittoFS runs entirely in userspace and implements protocols directly, giving better control and performance.

**Q: Can I use this in production?**

A: Not yet. DittoFS is experimental and needs more testing, security auditing, and hardening before production use.

**Q: Which NFS version is supported?**

A: Currently NFSv3 over TCP. NFSv4 support is planned for a future phase.

**Q: Can I implement my own protocol adapter?**

A: Yes! That's the whole point. Implement the `Adapter` interface and wire it to the metadata/content repositories.

**Q: How does performance compare to kernel NFS?**

A: We're still benchmarking, but the lack of FUSE overhead and optimized Go implementation should provide competitive performance for most workloads. Results will be published in Phase 2.

**Q: Does it support file locking?**

A: Not yet. NLM (Network Lock Manager) protocol support is planned but not currently implemented.

**Q: Can I use this with Windows clients?**

A: Windows can mount NFS shares, but the SMB/CIFS adapter will provide better Windows integration when implemented.

**Q: Is content deduplication supported?**

A: Not currently, but the content repository abstraction allows for implementing content-addressable storage with deduplication.

**Q: Does metadata persist across server restarts?**

A: Yes, when using the BadgerDB metadata backend (`type: badger`). The in-memory backend (`type: memory`) loses all metadata when the server stops.

**Q: Can I import an existing filesystem into DittoFS?**

A: Not yet, but the path-based file handle strategy in BadgerDB enables this as a future feature. The handles are deterministic based on file paths, making filesystem scanning and import possible.

## License

MIT License - See LICENSE file for details

## Disclaimer

⚠️ **DittoFS is experimental software**

- Do not use in production environments without thorough testing
- The API may change without notice
- No backwards compatibility guarantees during experimental phase
- Security has not been professionally audited
- Performance characteristics are not yet fully benchmarked

## Acknowledgments

Built with ❤️ in Go.

---

**Getting Started?** Check out the Quick Start section above.

**Questions?** Open an issue on GitHub.

**Want to Contribute?** See CONTRIBUTING.md for guidelines.
