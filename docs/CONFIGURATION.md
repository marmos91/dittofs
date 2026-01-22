# DittoFS Configuration Guide

DittoFS uses a flexible configuration system with support for YAML/TOML files and environment variable overrides.

## Table of Contents

- [Configuration Files](#configuration-files)
- [Configuration Structure](#configuration-structure)
  - [Logging](#1-logging)
  - [Telemetry](#2-telemetry-opentelemetry)
  - [Server Settings](#3-server-settings)
  - [Database (Control Plane)](#4-database-control-plane)
  - [API Server](#5-api-server)
  - [Cache Configuration](#6-cache-configuration)
  - [Metadata Configuration](#7-metadata-configuration)
  - [Payload Configuration](#8-payload-configuration)
  - [Shares (Exports)](#9-shares-exports)
  - [User Management](#10-user-management)
  - [Protocol Adapters](#11-protocol-adapters)
- [Environment Variables](#environment-variables)
- [Configuration Precedence](#configuration-precedence)
- [Configuration Examples](#configuration-examples)
- [IDE Support with JSON Schema](#ide-support-with-json-schema)

## Configuration Files

### Default Location

`$XDG_CONFIG_HOME/dittofs/config.yaml` (typically `~/.config/dittofs/config.yaml`)

### Initialization

```bash
# Generate default configuration file
./dittofs init

# Generate with custom path
./dittofs init --config /etc/dittofs/config.yaml

# Force overwrite existing config
./dittofs init --force
```

### Supported Formats

YAML (`.yaml`, `.yml`) and TOML (`.toml`)

## Configuration Structure

DittoFS uses a flexible configuration approach with named, reusable stores. This allows different shares to use completely different backends, or multiple shares can efficiently share the same store instances.

### 1. Logging

Controls log output behavior:

```yaml
logging:
  level: "INFO"           # DEBUG, INFO, WARN, ERROR
  format: "text"          # text, json
  output: "stdout"        # stdout, stderr, or file path
```

**Log Formats:**

- **text**: Human-readable format with colored output (when terminal supports it)
  ```
  2024-01-15T10:30:45.123Z INFO  Starting DittoFS server component=server version=1.0.0
  ```

- **json**: Structured JSON format for log aggregation (Elasticsearch, Loki, etc.)
  ```json
  {"time":"2024-01-15T10:30:45.123Z","level":"INFO","msg":"Starting DittoFS server","component":"server","version":"1.0.0"}
  ```

### 2. Telemetry (OpenTelemetry)

Controls distributed tracing for observability:

```yaml
telemetry:
  enabled: false          # Enable/disable tracing (default: false)
  endpoint: "localhost:4317"  # OTLP collector endpoint (gRPC)
  insecure: false         # Use insecure connection (no TLS)
  sample_rate: 1.0        # Trace sampling rate (0.0 to 1.0)
```

When enabled, DittoFS exports traces to any OTLP-compatible collector (Jaeger, Tempo, Honeycomb, etc.).

**Configuration Options:**

| Option | Default | Description |
|--------|---------|-------------|
| `enabled` | `false` | Enable/disable distributed tracing |
| `endpoint` | `localhost:4317` | OTLP gRPC collector endpoint |
| `insecure` | `false` | Skip TLS verification (for local development) |
| `sample_rate` | `1.0` | Sampling rate: 1.0 = all traces, 0.5 = 50%, 0.0 = none |

**Example with Jaeger:**

```yaml
telemetry:
  enabled: true
  endpoint: "jaeger:4317"
  insecure: true  # For local Docker setup
  sample_rate: 1.0
```

**Trace Propagation:**

Traces include:
- NFS operation spans (READ, WRITE, LOOKUP, etc.)
- Storage backend operations (S3, BadgerDB, filesystem)
- Cache operations (hits, misses, flushes)
- Request context (client IP, file handles, paths)

### 3. Server Settings

Application-wide server configuration:

```yaml
server:
  shutdown_timeout: 30s   # Maximum time to wait for graceful shutdown

  metrics:
    enabled: false
    port: 9090

  rate_limiting:
    enabled: false
    requests_per_second: 5000
    burst: 10000
```

### 4. Database (Control Plane)

DittoFS uses a control plane database to store persistent configuration for users, groups, shares, and permissions. This enables dynamic management via CLI commands and REST API without restarting the server.

```yaml
database:
  # Database type: sqlite (single-node) or postgres (HA-capable)
  type: sqlite

  # SQLite configuration (default)
  sqlite:
    # Path to the SQLite database file
    # Default: $XDG_CONFIG_HOME/dittofs/controlplane.db
    path: /var/lib/dittofs/controlplane.db

  # PostgreSQL configuration (for HA deployments)
  postgres:
    host: localhost
    port: 5432
    database: dittofs
    user: dittofs
    password: ${POSTGRES_PASSWORD}  # Use environment variable
    sslmode: require               # disable, require, verify-ca, verify-full
    ssl_root_cert: ""              # Path to CA certificate
    max_open_conns: 25             # Maximum open connections
    max_idle_conns: 5              # Maximum idle connections
```

**Database Types:**

| Type | Description | Use Case |
|------|-------------|----------|
| `sqlite` | Embedded SQLite database | Single-node deployments (default) |
| `postgres` | PostgreSQL database | High-availability, multi-node deployments |

**SQLite Configuration:**

| Option | Default | Description |
|--------|---------|-------------|
| `path` | `~/.config/dittofs/controlplane.db` | Database file path |

**PostgreSQL Configuration:**

| Option | Default | Description |
|--------|---------|-------------|
| `host` | (required) | PostgreSQL server hostname |
| `port` | `5432` | PostgreSQL server port |
| `database` | (required) | Database name |
| `user` | (required) | Database user |
| `password` | (required) | Database password |
| `sslmode` | `disable` | SSL mode: disable, require, verify-ca, verify-full |
| `ssl_root_cert` | | Path to CA certificate for SSL verification |
| `max_open_conns` | `25` | Maximum number of open connections |
| `max_idle_conns` | `5` | Maximum number of idle connections |

> **Note**: The control plane database automatically creates tables and runs migrations on startup.

### 5. API Server

The REST API server provides endpoints for authentication, user management, and configuration. It is enabled by default.

```yaml
server:
  api:
    enabled: true              # Enable/disable API server (default: true)
    port: 8080                 # HTTP port for API endpoints
    read_timeout: 10s          # Max time to read request
    write_timeout: 10s         # Max time to write response
    idle_timeout: 60s          # Max idle time for keep-alive

    # JWT authentication configuration
    jwt:
      # HMAC signing key for JWT tokens (min 32 characters)
      # Can also be set via DITTOFS_API_JWT_SECRET environment variable
      secret: "your-secret-key-at-least-32-characters"
      access_token_duration: 15m   # Access token lifetime
      refresh_token_duration: 168h # Refresh token lifetime (7 days)
```

**API Configuration Options:**

| Option | Default | Description |
|--------|---------|-------------|
| `enabled` | `true` | Enable/disable the API server |
| `port` | `8080` | HTTP port for API endpoints |
| `read_timeout` | `10s` | Maximum duration to read request |
| `write_timeout` | `10s` | Maximum duration to write response |
| `idle_timeout` | `60s` | Maximum idle time for keep-alive |

**JWT Configuration Options:**

| Option | Default | Description |
|--------|---------|-------------|
| `secret` | (required) | HMAC signing key (min 32 chars) |
| `access_token_duration` | `15m` | Access token lifetime |
| `refresh_token_duration` | `168h` | Refresh token lifetime (7 days) |

> **Security Note**: The JWT secret should be kept confidential. Use the `DITTOFS_API_JWT_SECRET` environment variable in production to avoid storing secrets in config files.

**API Endpoints:**

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/health` | GET | Health check |
| `/api/v1/auth/login` | POST | Authenticate and get tokens |
| `/api/v1/auth/refresh` | POST | Refresh access token |
| `/api/v1/users` | GET/POST | List/create users |
| `/api/v1/users/{id}` | GET/PUT/DELETE | Get/update/delete user |
| `/api/v1/groups` | GET/POST | List/create groups |
| `/api/v1/groups/{id}` | GET/PUT/DELETE | Get/update/delete group |
| `/api/v1/shares` | GET/POST | List/create shares |
| `/api/v1/shares/{id}` | GET/PUT/DELETE | Get/update/delete share |

### 6. Cache Configuration

DittoFS uses a WAL-backed (Write-Ahead Log) cache for all file operations. The cache is mandatory for crash recovery and performance.

```yaml
cache:
  # Directory path for the cache WAL file (required)
  path: "/var/lib/dittofs/cache"
  # Maximum cache size (supports human-readable formats: "1GB", "512MB", "10Gi")
  size: "1Gi"
```

**Cache Features:**

- **WAL Persistence**: All writes are logged to disk via mmap for crash recovery
- **LRU Eviction**: Least-recently-used entries are evicted when cache is full
- **Dirty Protection**: Entries with unflushed data cannot be evicted
- **Chunk/Slice/Block Model**: Efficient storage model for large files

**Configuration Options:**

| Option | Required | Description |
|--------|----------|-------------|
| `path` | Yes | Directory for cache WAL file |
| `size` | No | Maximum cache size (default: 1GB) |

### 7. Metadata Configuration

Define named metadata store instances that shares can reference:

```yaml
metadata:
  # Filesystem capabilities and limits (applies to all stores)
  filesystem_capabilities:
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

  # Named metadata store instances
  stores:
    # In-memory metadata for fast temporary workloads
    memory-fast:
      type: memory
      memory: {}

    # BadgerDB for persistent metadata
    badger-main:
      type: badger
      badger:
        db_path: /tmp/dittofs-metadata-main

    # Separate BadgerDB instance for isolated shares
    badger-isolated:
      type: badger
      badger:
        db_path: /tmp/dittofs-metadata-isolated

    # PostgreSQL for distributed, horizontally-scalable metadata
    postgres-production:
      type: postgres
      postgres:
        # Connection settings
        host: localhost
        port: 5432
        database: dittofs
        user: dittofs
        password: ${POSTGRES_PASSWORD}  # Use environment variable

        # TLS configuration (recommended for production)
        sslmode: require  # Options: disable, require, verify-ca, verify-full

        # Connection pool sizing
        max_conns: 15           # Maximum connections (default: 10)
        min_conns: 2            # Minimum connections (default: 2)
        max_idle_time: 30m      # Close idle connections after (default: 30m)
        health_check_period: 1m # Health check interval (default: 1m)

        # Migration control
        auto_migrate: false     # Auto-run migrations on startup (default: true)
        migrations_path: ""     # Use embedded migrations (leave empty)
```

> **Persistence Options**:
> - **Memory**: Fast but ephemeral - all data lost on restart. Ideal for caching and temporary workloads.
> - **BadgerDB**: Persistent embedded database - single-node deployments. File handles and metadata survive restarts.
> - **PostgreSQL**: Persistent distributed database - multi-node deployments with horizontal scaling. Survives restarts and supports multiple DittoFS instances sharing the same metadata.

### 8. Payload Configuration

Define named payload store instances (block stores) that shares can reference for persistent storage:

```yaml
payload:
  # Named payload store instances
  stores:
    # Local filesystem storage for fast access
    local-disk:
      type: filesystem
      filesystem:
        base_path: /var/lib/dittofs/blocks

    # S3 storage for cloud-backed shares
    s3-production:
      type: s3
      s3:
        region: us-east-1
        bucket: dittofs-production
        prefix: "blocks/"
        endpoint: ""           # Optional, for S3-compatible services
        access_key_id: ""      # Optional, uses AWS SDK default chain
        secret_access_key: ""  # Optional, uses AWS SDK default chain
        force_path_style: false  # true for Localstack/MinIO
        max_retries: 3

    # In-memory storage for testing
    memory-test:
      type: memory

  # Transfer manager configuration (uploads/downloads to block store)
  transfer:
    workers:
      uploads: 4      # Number of parallel upload workers
      downloads: 4    # Number of parallel download workers
```

> **Payload Stores**: Payload stores persist cache data to durable storage using the Chunk/Slice/Block model.
> Each file is split into 64MB chunks, each chunk into slices, and slices into 4MB blocks.
>
> **S3 Production Features**:
>
> - **Range Reads**: Efficient partial reads using S3 byte-range requests
> - **Configurable Retry**: Automatic retry with exponential backoff for transient S3 errors
> - **Path-Based Keys**: Objects stored as `{prefix}{contentID}/chunk-{n}/block-{n}` for easy inspection

**Payload Store Types:**

| Type | Description | Use Case |
|------|-------------|----------|
| `memory` | In-memory storage (ephemeral) | Testing, development |
| `filesystem` | Local filesystem storage | Single-server, local storage |
| `s3` | AWS S3 or S3-compatible storage | Production, cloud deployments |

**Filesystem Configuration:**

| Option | Required | Description |
|--------|----------|-------------|
| `base_path` | Yes | Root directory for block storage |
| `create_dir` | No | Create directory if missing (default: true) |
| `dir_mode` | No | Permission mode for directories (default: 0755) |
| `file_mode` | No | Permission mode for files (default: 0644) |

**S3 Configuration:**

| Option | Required | Description |
|--------|----------|-------------|
| `bucket` | Yes | S3 bucket name |
| `region` | No | AWS region (uses SDK default if empty) |
| `endpoint` | No | S3 endpoint URL (for S3-compatible services) |
| `prefix` | No | Key prefix for all blocks (default: "blocks/") |
| `force_path_style` | No | Use path-style addressing (required for Localstack/MinIO) |
| `max_retries` | No | Maximum retry attempts (default: 3) |

### 9. Shares (Exports)

Each share explicitly references metadata and payload stores by name. Multiple shares can reference the same store instances for resource sharing:

```yaml
shares:
  # Fast local share using in-memory metadata and local disk
  - name: /fast
    metadata: memory-fast      # References metadata.stores.memory-fast
    payload: local-disk        # References payload.stores.local-disk
    read_only: false

    # Access control
    allowed_clients: []
    denied_clients: []

    # User management (see Section 8)
    # default_permission controls access for unknown UIDs:
    # - "none": Block unknown UIDs (no guest access)
    # - "read": Guest users get read-only access
    # - "read-write": Guest users get read-write access
    # - "admin": Guest users get admin access
    default_permission: "read"

    # Authentication
    require_auth: false
    allowed_auth_methods: [anonymous, unix]

    # Identity mapping (user/group squashing)
    identity_mapping:
      map_all_to_anonymous: true              # all_squash
      map_privileged_to_anonymous: false      # root_squash
      anonymous_uid: 65534                    # nobody
      anonymous_gid: 65534                    # nogroup

    # Root directory attributes
    root_directory_attributes:
      mode: 0755
      uid: 0
      gid: 0

  # Cloud-backed share with persistent metadata
  - name: /cloud
    metadata: badger-main      # References metadata.stores.badger-main
    payload: s3-production     # References payload.stores.s3-production
    read_only: false
    # ... (same access control options as above)

  # Archive share sharing metadata with /cloud
  - name: /archive
    metadata: badger-main      # Shares metadata with /cloud
    payload: s3-archive        # Different payload backend
    read_only: false
    # ... (same access control options as above)
```

**Configuration Patterns:**

- **Shared Metadata**: `/cloud` and `/archive` both use `badger-main` - they share the same metadata database
- **Performance Tiering**: Different shares use different storage backends (memory, local disk, S3)
- **Isolation**: Different shares can use completely separate stores for security boundaries
- **Resource Efficiency**: Multiple shares can reference the same store instance (no duplication)
- **Global Cache**: All shares use the single global cache configured in the top-level `cache:` section

### 10. User Management

DittoFS supports a unified user management system for both NFS and SMB protocols. Users, groups, and their permissions are stored in the control plane database (see [Database Configuration](#4-database-control-plane)) and can be managed via:

1. **CLI commands** (`dittofs user`, `dittofs group`) - Recommended for initial setup
2. **REST API** - For programmatic management and integrations
3. **Config file** - For bootstrap configuration (imported on first run)

Permission resolution follows a priority order: user explicit permissions > group permissions (highest wins) > share default.

> **Note**: Users and groups defined in the config file are imported into the database on first run. After that, use CLI commands or the REST API to manage them.

#### Users

Define named users with credentials and permissions:

```yaml
users:
  - username: "admin"
    # Password hash (bcrypt). Generate with: htpasswd -bnBC 10 "" password | tr -d ':\n'
    password_hash: "$2a$10$..."
    enabled: true
    uid: 1000        # Unix UID for NFS mapping
    gid: 100         # Primary Unix GID
    groups: ["admins"]  # Group membership (by name)
    # Optional: explicit share permissions (override group permissions)
    share_permissions:
      /private: "admin"

  - username: "editor"
    password_hash: "$2a$10$..."
    enabled: true
    uid: 1001
    gid: 101
    groups: ["editors"]

  - username: "viewer"
    password_hash: "$2a$10$..."
    enabled: true
    uid: 1002
    gid: 102
    groups: ["viewers"]
```

**User Fields:**

| Field | Type | Description |
|-------|------|-------------|
| `username` | string | Unique username for authentication |
| `password_hash` | string | bcrypt password hash (cost 10 recommended) |
| `enabled` | bool | Whether the user can authenticate |
| `uid` | uint32 | Unix UID for NFS identity mapping |
| `gid` | uint32 | Primary Unix GID |
| `groups` | []string | Group names this user belongs to |
| `share_permissions` | map | Per-share permissions (optional, overrides group) |

**NFS Authentication**: NFS clients authenticate via AUTH_UNIX. The client's UID is matched against DittoFS user UIDs. If a match is found, the user's permissions are applied.

**SMB Authentication**: SMB clients authenticate via NTLM. The username is matched against DittoFS users, and permissions are applied from the user's configuration.

#### Groups

Define groups with share-level permissions:

```yaml
groups:
  - name: "admins"
    gid: 100
    share_permissions:
      /export: "admin"
      /archive: "admin"

  - name: "editors"
    gid: 101
    share_permissions:
      /export: "read-write"
      /archive: "read-write"

  - name: "viewers"
    gid: 102
    share_permissions:
      /export: "read"
      /archive: "read"
```

**Group Fields:**

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Unique group name |
| `gid` | uint32 | Unix GID |
| `share_permissions` | map | Per-share permissions for all group members |

#### Guest Configuration

Configure anonymous/unauthenticated access:

```yaml
guest:
  enabled: true
  uid: 65534        # nobody
  gid: 65534        # nogroup
  share_permissions:
    /public: "read"
```

**Guest Fields:**

| Field | Type | Description |
|-------|------|-------------|
| `enabled` | bool | Allow guest/anonymous access |
| `uid` | uint32 | Unix UID for guest users |
| `gid` | uint32 | Unix GID for guest users |
| `share_permissions` | map | Per-share permissions for guests |

#### Permission Levels

| Permission | Description |
|------------|-------------|
| `none` | No access (cannot connect to share) |
| `read` | Read-only access |
| `read-write` | Read and write access |
| `admin` | Full access including delete and ownership |

#### Permission Resolution Order

1. **User explicit permission**: If the user has a direct `share_permissions` entry for the share, use it
2. **Group permissions**: Check all groups the user belongs to, use the highest permission level
3. **Share default**: Fall back to the share's `default_permission` setting

**Example:**

```yaml
groups:
  - name: "viewers"
    share_permissions:
      /archive: "read"

users:
  - username: "special-viewer"
    groups: ["viewers"]
    share_permissions:
      /archive: "read-write"  # Overrides group's "read" permission
```

In this example, `special-viewer` gets `read-write` on `/archive` (user explicit), even though the `viewers` group only has `read`.

#### CLI Management Commands

DittoFS provides CLI commands to manage users and groups without manually editing the config file.

**User Commands:**

```bash
# Add a new user (prompts for password)
dittofs user add alice
dittofs user add alice --uid 1005 --gid 100 --groups editors,viewers

# Delete a user
dittofs user delete alice

# List all users
dittofs user list

# Change password
dittofs user passwd alice

# Grant share permission
dittofs user grant alice /export read-write

# Revoke share permission
dittofs user revoke alice /export

# List user's groups
dittofs user groups alice

# Add user to group
dittofs user join alice editors

# Remove user from group
dittofs user leave alice editors
```

**Group Commands:**

```bash
# Add a new group
dittofs group add editors
dittofs group add editors --gid 101

# Delete a group
dittofs group delete editors
dittofs group delete editors --force  # Delete even if has members

# List all groups
dittofs group list

# List group members
dittofs group members editors

# Grant share permission
dittofs group grant editors /export read-write

# Revoke share permission
dittofs group revoke editors /export
```

**Using Custom Config File:**

All user and group commands support the `--config` flag:

```bash
dittofs user list --config /etc/dittofs/config.yaml
dittofs group add admins --config /etc/dittofs/config.yaml
```

### 11. Protocol Adapters

Configures protocol-specific settings:

**NFS Adapter**:

```yaml
server:
  shutdown_timeout: 30s

  # Global rate limiting (applies to all adapters unless overridden)
  rate_limiting:
    enabled: false
    requests_per_second: 5000    # Sustained rate limit
    burst: 10000                  # Burst capacity (2x sustained recommended)

adapters:
  nfs:
    enabled: true
    port: 2049
    max_connections: 0           # 0 = unlimited

    # Grouped timeout configuration
    timeouts:
      read: 5m                   # Max time to read request
      write: 30s                 # Max time to write response
      idle: 5m                   # Max idle time between requests
      shutdown: 30s              # Graceful shutdown timeout

    metrics_log_interval: 5m     # Metrics logging interval (0 = disabled)

    # Optional: override server-level rate limiting for this adapter
    # rate_limiting:
    #   enabled: true
    #   requests_per_second: 10000
    #   burst: 20000
```

**SMB Adapter**:

```yaml
adapters:
  smb:
    enabled: false            # Enable SMB2 protocol (default: false)
    port: 12445               # Default SMB port (standard 445 requires root)
    max_connections: 0        # 0 = unlimited
    max_requests_per_connection: 100  # Concurrent requests per connection

    # Grouped timeout configuration
    timeouts:
      read: 5m                # Max time to read request
      write: 30s              # Max time to write response
      idle: 5m                # Max idle time between requests
      shutdown: 30s           # Graceful shutdown timeout

    metrics_log_interval: 5m  # Metrics logging interval (0 = disabled)

    # Credit management configuration
    # Credits control SMB2 flow control and client parallelism
    credits:
      strategy: adaptive      # fixed, echo, adaptive (default: adaptive)
      min_grant: 16           # Minimum credits per response
      max_grant: 8192         # Maximum credits per response
      initial_grant: 256      # Credits for initial requests (NEGOTIATE)
      max_session_credits: 65535  # Max outstanding credits per session

      # Adaptive strategy thresholds (ignored for fixed/echo)
      load_threshold_high: 1000       # Start throttling above this load
      load_threshold_low: 100         # Boost credits below this load
      aggressive_client_threshold: 256 # Throttle clients with this many outstanding
```

**SMB Credit Strategies:**

| Strategy | Description | Use Case |
|----------|-------------|----------|
| `fixed` | Always grants `initial_grant` credits | Simple, predictable behavior |
| `echo` | Grants what client requests (within bounds) | Maintains client's credit pool |
| `adaptive` | Adjusts based on server load and client behavior | **Recommended** for production |

**SMB Credit Configuration Options:**

| Option | Default | Description |
|--------|---------|-------------|
| `strategy` | `adaptive` | Credit grant strategy |
| `min_grant` | `16` | Minimum credits per response (prevents deadlock) |
| `max_grant` | `8192` | Maximum credits per response |
| `initial_grant` | `256` | Credits for NEGOTIATE/SESSION_SETUP |
| `max_session_credits` | `65535` | Max outstanding credits per session |
| `load_threshold_high` | `1000` | Server load that triggers throttling |
| `load_threshold_low` | `100` | Server load that triggers boost |
| `aggressive_client_threshold` | `256` | Outstanding requests that trigger client throttling |

> **Note**: SMB2 credits are flow control tokens that limit concurrent operations per client.
> Higher credits = more parallelism but more server resource consumption.
> The adaptive strategy balances throughput and protection automatically.

## Environment Variables

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

# Telemetry (OpenTelemetry)
export DITTOFS_TELEMETRY_ENABLED=true
export DITTOFS_TELEMETRY_ENDPOINT=jaeger:4317
export DITTOFS_TELEMETRY_INSECURE=true
export DITTOFS_TELEMETRY_SAMPLE_RATE=0.5

# Server
export DITTOFS_SERVER_SHUTDOWN_TIMEOUT=60s

# Database (Control Plane)
export DITTOFS_DATABASE_TYPE=sqlite
export DITTOFS_DATABASE_SQLITE_PATH=/var/lib/dittofs/controlplane.db
# PostgreSQL
export DITTOFS_DATABASE_TYPE=postgres
export DITTOFS_DATABASE_POSTGRES_HOST=localhost
export DITTOFS_DATABASE_POSTGRES_PORT=5432
export DITTOFS_DATABASE_POSTGRES_DATABASE=dittofs
export DITTOFS_DATABASE_POSTGRES_USER=dittofs
export DITTOFS_DATABASE_POSTGRES_PASSWORD=secret
export DITTOFS_DATABASE_POSTGRES_SSLMODE=require

# API Server
export DITTOFS_SERVER_API_ENABLED=true
export DITTOFS_SERVER_API_PORT=8080
export DITTOFS_API_JWT_SECRET=your-secret-key-at-least-32-characters

# Cache
export DITTOFS_CACHE_PATH=/var/lib/dittofs/cache
export DITTOFS_CACHE_SIZE=2Gi

# Server-level configuration
export DITTOFS_SERVER_SHUTDOWN_TIMEOUT=60s

# Global rate limiting
export DITTOFS_SERVER_RATE_LIMITING_ENABLED=true
export DITTOFS_SERVER_RATE_LIMITING_REQUESTS_PER_SECOND=10000
export DITTOFS_SERVER_RATE_LIMITING_BURST=20000

# Metadata
export DITTOFS_METADATA_TYPE=badger

# NFS adapter
export DITTOFS_ADAPTERS_NFS_ENABLED=true
export DITTOFS_ADAPTERS_NFS_PORT=12049
export DITTOFS_ADAPTERS_NFS_MAX_CONNECTIONS=1000

# NFS timeouts
export DITTOFS_ADAPTERS_NFS_TIMEOUTS_READ=5m
export DITTOFS_ADAPTERS_NFS_TIMEOUTS_WRITE=30s
export DITTOFS_ADAPTERS_NFS_TIMEOUTS_IDLE=5m
export DITTOFS_ADAPTERS_NFS_TIMEOUTS_SHUTDOWN=30s

# SMB adapter
export DITTOFS_ADAPTERS_SMB_ENABLED=true
export DITTOFS_ADAPTERS_SMB_PORT=12445
export DITTOFS_ADAPTERS_SMB_MAX_CONNECTIONS=1000

# SMB credits
export DITTOFS_ADAPTERS_SMB_CREDITS_STRATEGY=adaptive
export DITTOFS_ADAPTERS_SMB_CREDITS_MIN_GRANT=16
export DITTOFS_ADAPTERS_SMB_CREDITS_MAX_GRANT=8192
export DITTOFS_ADAPTERS_SMB_CREDITS_INITIAL_GRANT=256

# Start server with overrides
DITTOFS_LOGGING_LEVEL=DEBUG ./dittofs start
```

## Configuration Precedence

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

## Configuration Examples

### Minimal Configuration

Single share with minimal settings:

```yaml
logging:
  level: INFO

cache:
  path: /tmp/dittofs-cache
  size: "512MB"

metadata:
  stores:
    default:
      type: memory

payload:
  stores:
    default:
      type: filesystem
      filesystem:
        base_path: /tmp/dittofs-blocks

shares:
  - name: /export
    metadata: default
    payload: default

adapters:
  nfs:
    enabled: true
```

### Development Setup

Fast iteration with in-memory stores:

```yaml
logging:
  level: DEBUG
  format: text

cache:
  path: /tmp/dittofs-dev-cache
  size: "256MB"

metadata:
  stores:
    dev-memory:
      type: memory

payload:
  stores:
    dev-memory:
      type: memory

shares:
  - name: /export
    metadata: dev-memory
    payload: dev-memory
    identity_mapping:
      map_all_to_anonymous: true

adapters:
  nfs:
    enabled: true
    port: 12049
```

### Production Setup

Persistent storage with access control, structured logging, and telemetry:

```yaml
logging:
  level: WARN
  format: json
  output: /var/log/dittofs/server.log

telemetry:
  enabled: true
  endpoint: "tempo:4317"     # Or your OTLP collector
  insecure: false            # Use TLS in production
  sample_rate: 0.1           # Sample 10% of traces

server:
  shutdown_timeout: 30s
  metrics:
    enabled: true
    port: 9090

cache:
  path: /var/lib/dittofs/cache
  size: "4Gi"

metadata:
  filesystem_capabilities:
    max_read_size: 1048576
    max_write_size: 1048576

  stores:
    prod-badger:
      type: badger
      badger:
        path: /var/lib/dittofs/metadata

payload:
  stores:
    prod-disk:
      type: filesystem
      filesystem:
        base_path: /var/lib/dittofs/blocks

shares:
  - name: /export
    metadata: prod-badger
    payload: prod-disk
    read_only: false
    allowed_clients:
      - 192.168.1.0/24
    denied_clients:
      - 192.168.1.50
    identity_mapping:
      map_all_to_anonymous: false
      map_privileged_to_anonymous: true
    root_directory_attributes:
      mode: 0755
      uid: 0
      gid: 0
    dump_restricted: true

adapters:
  nfs:
    enabled: true
    port: 2049
    max_connections: 1000
    timeouts:
      read: 5m
      write: 30s
      idle: 5m
```

### Multi-Share with Different Backends

Different shares using different storage backends:

```yaml
cache:
  path: /var/lib/dittofs/cache
  size: "2Gi"

metadata:
  stores:
    fast-memory:
      type: memory
    persistent-badger:
      type: badger
      badger:
        path: /var/lib/dittofs/metadata

payload:
  stores:
    local-disk:
      type: filesystem
      filesystem:
        base_path: /var/lib/dittofs/blocks
    cloud-s3:
      type: s3
      s3:
        region: us-east-1
        bucket: my-dittofs-bucket

shares:
  # Fast temporary share
  - name: /temp
    metadata: fast-memory
    payload: local-disk
    read_only: false
    identity_mapping:
      map_all_to_anonymous: true

  # Cloud-backed persistent share
  - name: /cloud
    metadata: persistent-badger
    payload: cloud-s3
    read_only: false
    allowed_clients:
      - 10.0.1.0/24

  # Public read-only share
  - name: /public
    metadata: persistent-badger
    payload: local-disk
    read_only: true
    identity_mapping:
      map_all_to_anonymous: true

adapters:
  nfs:
    enabled: true
```

### Shared Metadata Pattern

Multiple shares sharing the same metadata database:

```yaml
cache:
  path: /var/lib/dittofs/cache
  size: "2Gi"

metadata:
  stores:
    shared-badger:
      type: badger
      badger:
        path: /var/lib/dittofs/shared-metadata

payload:
  stores:
    s3-production:
      type: s3
      s3:
        region: us-east-1
        bucket: prod-bucket
    s3-archive:
      type: s3
      s3:
        region: us-east-1
        bucket: archive-bucket

shares:
  # Production share
  - name: /prod
    metadata: shared-badger    # Shared metadata
    payload: s3-production
    read_only: false

  # Archive share (shares metadata with /prod)
  - name: /archive
    metadata: shared-badger    # Same metadata store
    payload: s3-archive        # Different payload backend
    read_only: false

adapters:
  nfs:
    enabled: true
```

## IDE Support with JSON Schema

DittoFS provides a JSON schema for configuration validation and autocomplete in VS Code and other editors.

### Setup for VS Code

1. The `.vscode/settings.json` file is already configured
2. Install the [YAML extension](https://marketplace.visualstudio.com/items?itemName=redhat.vscode-yaml)
3. Open any `dittofs.yaml` or `config.yaml` file
4. Get autocomplete, validation, and inline documentation

### Generate Schema

If modified:

```bash
go run cmd/generate-schema/main.go config.schema.json
```

### Features

- ✅ Field autocomplete
- ✅ Type validation
- ✅ Inline documentation on hover
- ✅ Error highlighting for invalid values

## Viewing Active Configuration

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
