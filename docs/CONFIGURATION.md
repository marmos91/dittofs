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
  - [Block Store Configuration](#6-block-store-configuration)
  - [Metadata Configuration](#7-metadata-configuration)
  - [Shares (Exports)](#8-shares-exports)
  - [User Management](#9-user-management)
  - [Protocol Adapters](#10-protocol-adapters)
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
./dfs init

# Generate with custom path
./dfs init --config /etc/dittofs/config.yaml

# Force overwrite existing config
./dfs init --force
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
    path: /var/lib/dfs/controlplane.db

  # PostgreSQL configuration (for HA deployments)
  postgres:
    host: localhost
    port: 5432
    database: dfs
    user: dfs
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
controlplane:
  port: 8080                 # HTTP port for API endpoints
  read_timeout: 10s          # Max time to read request
  write_timeout: 10s         # Max time to write response
  idle_timeout: 60s          # Max idle time for keep-alive

  # JWT authentication configuration
  jwt:
    # HMAC signing key for JWT tokens (min 32 characters)
    # Can also be set via DITTOFS_CONTROLPLANE_SECRET environment variable
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

> **Security Note**: The JWT secret should be kept confidential. Use the `DITTOFS_CONTROLPLANE_SECRET` environment variable in production to avoid storing secrets in config files.

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

### 6. Block Store Configuration

DittoFS uses a **mandatory WAL-backed cache** for all file content operations. Every write passes through the cache before being flushed asynchronously to the payload store (S3, filesystem, etc.). The WAL (Write-Ahead Log) ensures data durability: on crash or restart, uncommitted writes are replayed automatically from the WAL file on disk.

```yaml
cache:
  # Directory path for the cache WAL file (required)
  # The cache creates a cache.dat file in this directory
  # Default: $TMPDIR/dittofs-cache (e.g., /tmp/dittofs-cache)
  path: "/var/lib/dfs/cache"

  # Maximum cache size (supports human-readable formats)
  # Accepts: "512MB", "1GB", "1Gi", "4Gi", "16Gi", etc.
  # Default: 1Gi
  size: "1Gi"
```

**How the cache works:**

```
Client WRITE ──► Cache (4MB block buffers + WAL on disk)
                    │
                    ▼
              Offloader (async background flush)
                    │
                    ▼
              Payload Store (S3, filesystem, memory)
```

1. **Write**: Incoming data is buffered in 4MB in-memory block buffers and simultaneously journaled to the WAL on disk (via mmap) for durability
2. **Flush**: The offloader uploads complete blocks to the payload store asynchronously in the background
3. **Evict**: After a block is successfully uploaded, it becomes evictable. Under memory pressure, the least-recently-used uploaded blocks are freed first
4. **Backpressure**: Dirty (unflushed) blocks cannot be evicted. If the cache fills with dirty data (e.g., payload store is slow), writes will block until space is freed. A separate pending-data limit (512MB default) prevents OOM even with large cache sizes
5. **Recovery**: On crash or restart, the WAL replays uncommitted writes, so no data is lost

**Cache Features:**

- **WAL Persistence**: All writes are logged to disk via mmap for crash recovery
- **LRU Eviction**: Least-recently-used uploaded entries are evicted when cache is full
- **Dirty Protection**: Entries with unflushed data cannot be evicted
- **Backpressure**: Writes block when pending data exceeds limits, preventing OOM
- **Deduplication**: Blocks are content-addressed (SHA-256); identical blocks are stored once
- **Global**: All shares use the same cache instance

**Configuration Options:**

| Option | Required | Default | Description |
|--------|----------|---------|-------------|
| `path` | Yes | `$TMPDIR/dittofs-cache` | Directory for the cache WAL file. Use a persistent path for production (not `/tmp`) |
| `size` | No | `1Gi` | Maximum cache size. Accepts human-readable formats: `512MB`, `1Gi`, `4GB`, etc. |

**Environment variable overrides:**

```bash
export DITTOFS_CACHE_PATH=/var/lib/dfs/cache
export DITTOFS_CACHE_SIZE=4Gi
```

**Sizing guidance:**

| Use Case | Recommended Size | Notes |
|----------|-----------------|-------|
| Development/testing | `512Mi` – `1Gi` | Default is fine |
| General use | `2Gi` – `4Gi` | Good for mixed read/write |
| Heavy writes / S3 backend | `4Gi` – `16Gi` | More cache absorbs upload latency |
| Large file workloads (>100MB files) | `8Gi`+ | Prevents excessive eviction churn |

> **Tip**: If you see `ErrCacheFull` errors or slow write throughput, increase `size`. If using an S3 payload store, a larger cache helps absorb the higher upload latency.

### 7. Metadata Configuration

Metadata configuration has two parts: filesystem capabilities (server config file) and store instances (managed via CLI).

#### Filesystem Capabilities (config file)

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
```

#### Metadata Store Instances (CLI)

Metadata stores are managed at runtime via `dfsctl` and persisted in the control plane database:

```bash
# In-memory metadata for fast temporary workloads
./dfsctl store metadata add --name memory-fast --type memory

# BadgerDB for persistent metadata
./dfsctl store metadata add --name badger-main --type badger \
  --config '{"path":"/tmp/dittofs-metadata-main"}'

# Separate BadgerDB instance for isolated shares
./dfsctl store metadata add --name badger-isolated --type badger \
  --config '{"path":"/tmp/dittofs-metadata-isolated"}'

# PostgreSQL for distributed, horizontally-scalable metadata
# Set POSTGRES_PASSWORD in your environment
./dfsctl store metadata add --name postgres-production --type postgres \
  --config "{\"host\":\"localhost\",\"port\":5432,\"database\":\"dfs\",\"user\":\"dfs\",\"password\":\"$POSTGRES_PASSWORD\",\"sslmode\":\"require\",\"max_conns\":15}"

# List all metadata stores
./dfsctl store metadata list

# Remove a metadata store
./dfsctl store metadata remove memory-fast
```

> **Persistence Options**:
> - **Memory**: Fast but ephemeral - all data lost on restart. Ideal for caching and temporary workloads.
> - **BadgerDB**: Persistent embedded database - single-node deployments. File handles and metadata survive restarts.
> - **PostgreSQL**: Persistent distributed database - multi-node deployments with horizontal scaling. Survives restarts and supports multiple DittoFS instances sharing the same metadata.

### 8. Shares (Exports)

Shares are managed at runtime via `dfsctl` and persisted in the control plane database. Each share references metadata and block stores by name:

```bash
# Create shares referencing existing stores
./dfsctl share create --name /fast --metadata memory-fast --local local-cache
./dfsctl share create --name /cloud --metadata badger-main --local local-cache --remote s3-remote
./dfsctl share create --name /archive --metadata badger-main --local local-cache --remote s3-archive

# Grant permissions on shares
./dfsctl share permission grant /fast --user alice --level read-write
./dfsctl share permission grant /cloud --user alice --level read-write
./dfsctl share permission grant /cloud --group editors --level read

# List shares and their permissions
./dfsctl share list
./dfsctl share permission list /cloud

# Delete a share
./dfsctl share delete /fast
```

**Configuration Patterns:**

- **Shared Metadata**: `/cloud` and `/archive` both use `badger-main` - they share the same metadata database
- **Performance Tiering**: Different shares use different storage backends (memory, local disk, S3)
- **Isolation**: Each share gets its own BlockStore with isolated local storage directory
- **Resource Efficiency**: Remote stores are shared (ref counted) when multiple shares reference the same config
- **Flexible Topologies**: Mix local-only and remote-backed storage per-share

### 9. User Management

DittoFS supports a unified user management system for both NFS and SMB protocols. Users, groups, and their permissions are stored in the control plane database (see [Database Configuration](#4-database-control-plane)) and can be managed via:

1. **CLI commands** (`dfs user`, `dfs group`) - Recommended for initial setup
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
dfs user add alice
dfs user add alice --uid 1005 --gid 100 --groups editors,viewers

# Delete a user
dfs user delete alice

# List all users
dfs user list

# Change password
dfs user passwd alice

# Grant share permission
dfs user grant alice /export read-write

# Revoke share permission
dfs user revoke alice /export

# List user's groups
dfs user groups alice

# Add user to group
dfs user join alice editors

# Remove user from group
dfs user leave alice editors
```

**Group Commands:**

```bash
# Add a new group
dfs group add editors
dfs group add editors --gid 101

# Delete a group
dfs group delete editors
dfs group delete editors --force  # Delete even if has members

# List all groups
dfs group list

# List group members
dfs group members editors

# Grant share permission
dfs group grant editors /export read-write

# Revoke share permission
dfs group revoke editors /export
```

**Using Custom Config File:**

All user and group commands support the `--config` flag:

```bash
dfs user list --config /etc/dittofs/config.yaml
dfs group add admins --config /etc/dittofs/config.yaml
```

### 10. Protocol Adapters

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

### SMB3 Encryption Configuration

SMB3 encryption provides confidentiality and integrity for all messages on a session using AEAD ciphers (AES-GCM or AES-CCM). Encryption is negotiated during NEGOTIATE (cipher selection for SMB 3.1.1), enforced per-session during SESSION_SETUP, and enforced per-share via the `encrypt_data` field in share configuration.

```yaml
adapters:
  smb:
    encryption:
      # Encryption mode controls server-wide encryption policy.
      # "disabled"  - No encryption. Sessions and shares are unencrypted.
      # "preferred" - Encryption is enabled for 3.x sessions that support it,
      #               but unencrypted requests are still accepted (mixed model).
      # "required"  - Only SMB 3.x clients with encryption can connect.
      #               2.x clients are rejected. Unencrypted requests on encrypted
      #               sessions return STATUS_ACCESS_DENIED.
      encryption_mode: disabled   # disabled | preferred | required (default: disabled)

      # Server cipher preference order (first = most preferred).
      # Empty list means all ciphers are allowed in the default order.
      # Valid cipher IDs: AES-256-GCM (0x0004), AES-256-CCM (0x0003),
      #                   AES-128-GCM (0x0002), AES-128-CCM (0x0001)
      # Default: [AES-256-GCM, AES-256-CCM, AES-128-GCM, AES-128-CCM]
      allowed_ciphers: []
```

**Per-Share Encryption**: Individual shares can require encryption via the `encrypt_data` flag. When enabled, the server sets `SMB2_SHAREFLAG_ENCRYPT_DATA` in the TREE_CONNECT response, and clients must encrypt all traffic to that share.

```bash
# Enable encryption for a specific share
dfsctl share create --name /secure --metadata default --encrypt-data
```

**Encryption Modes:**

| Mode | Behavior | Use Case |
|------|----------|----------|
| `disabled` | No encryption for any session | Legacy clients, testing |
| `preferred` | Encrypt 3.x sessions; allow unencrypted 2.x | Mixed environments |
| `required` | Reject 2.x clients; encrypt all 3.x sessions | High-security environments |

**Enforcement Rules:**

1. **SESSION_SETUP**: When mode is `preferred` or `required`, encryption keys are derived for SMB 3.x sessions, and the `SMB2_SESSION_FLAG_ENCRYPT_DATA` flag is set in the response.
2. **TREE_CONNECT**: When a share has `encrypt_data=true` and mode is `required`, unencrypted sessions are rejected with `STATUS_ACCESS_DENIED`. In `preferred` mode, unencrypted sessions are allowed (mixed model).
3. **Guest sessions**: Never encrypted (no session key for key derivation).
4. **SMB 2.x clients**: Never encrypted (encryption requires SMB 3.0+). In `required` mode, 2.x clients are rejected at NEGOTIATE.

> **Security Note**: For production environments handling sensitive data, set `encryption_mode: required` and enable `encrypt_data` on shares that hold confidential information.

### SMB3 Signing Configuration

SMB3 signing provides message integrity using AES-CMAC (3.0+) or AES-GMAC (3.1.1), replacing the HMAC-SHA256 used in SMB 2.x. Signing keys are derived from the session key using SP800-108 KDF.

```yaml
adapters:
  smb:
    signing:
      enabled: true       # Advertise signing capability (default: true)
      required: false      # Require all clients to sign (default: false)
      # Signing algorithm preference for 3.1.1 (SIGNING_CAPABILITIES context)
      # Default: [AES-128-GMAC, AES-128-CMAC]
      # AES-128-GMAC is fastest on hardware with AES-NI + CLMUL
      preferred_algorithms: []
```

**Signing Configuration Options:**

| Option | Default | Description |
|--------|---------|-------------|
| `enabled` | `true` | Advertise signing capability in NEGOTIATE |
| `required` | `false` | Reject unsigned messages from established sessions |
| `preferred_algorithms` | `[GMAC, CMAC]` | Algorithm preference for 3.1.1 negotiate context |

### SMB3 Dialect Configuration

Control which SMB dialects the server accepts:

```yaml
adapters:
  smb:
    # Minimum dialect the server will accept
    # Set to "3.0" to reject legacy SMB2 clients
    min_dialect: "2.0.2"     # "2.0.2" | "3.0" | "3.0.2" | "3.1.1"

    # Maximum dialect the server will negotiate
    max_dialect: "3.1.1"     # Default: highest supported
```

### SMB3 Lease Configuration

Leases V2 and directory leasing configuration:

```yaml
adapters:
  smb:
    leases:
      enabled: true              # Enable lease support (default: true)
      directory_leases: true     # Enable directory leasing (default: true)
      lease_break_timeout: 35s   # Time to wait for break acknowledgment (default: 35s)
```

| Option | Default | Description |
|--------|---------|-------------|
| `enabled` | `true` | Enable SMB lease support |
| `directory_leases` | `true` | Enable directory Read leasing |
| `lease_break_timeout` | `35s` | Maximum wait for lease break acknowledgment |

### SMB3 Durable Handle Configuration

Durable handle settings for session resilience:

```yaml
adapters:
  smb:
    durable_handles:
      enabled: true                  # Enable durable handle support (default: true)
      default_timeout: 60s           # Handle preservation timeout (default: 60s)
      scavenger_interval: 10s        # Expired handle scan interval (default: 10s)
      max_handles_per_session: 1000  # Maximum durable handles per session
```

| Option | Default | Description |
|--------|---------|-------------|
| `enabled` | `true` | Enable durable handle V1/V2 support |
| `default_timeout` | `60s` | How long to preserve disconnected handles |
| `scavenger_interval` | `10s` | Background scan interval for expired handles |
| `max_handles_per_session` | `1000` | Limit durable handles per session |

### Cross-Protocol Coordination

NFS/SMB cross-protocol coordination uses built-in defaults that are not
currently configurable via YAML. The defaults are:

| Parameter | Default | Description |
|-----------|---------|-------------|
| Delegation recall timeout | `90s` | Maximum wait for NFS client to return delegation after CB_RECALL |
| Anti-storm TTL | `30s` | Duration to suppress re-grants after a lease/delegation break |

These are set programmatically via `Manager.SetDelegationRecallTimeout()` and
`NewManagerWithTTL()` respectively.

### Complete SMB3 Adapter Configuration Example

```yaml
adapters:
  smb:
    enabled: true
    port: 12445
    max_connections: 0          # 0 = unlimited
    max_requests_per_connection: 100

    # Dialect range
    min_dialect: "3.0"          # Reject SMB2 clients
    max_dialect: "3.1.1"

    # Timeouts
    timeouts:
      read: 5m
      write: 30s
      idle: 5m
      shutdown: 30s

    # Credits
    credits:
      strategy: adaptive
      min_grant: 16
      max_grant: 8192
      initial_grant: 256
      max_session_credits: 65535

    # Signing
    signing:
      enabled: true
      required: true
      preferred_algorithms: []  # Default: [GMAC, CMAC]

    # Encryption
    encryption:
      encryption_mode: required
      allowed_ciphers: []       # Default: all in preference order

    # Leases
    leases:
      enabled: true
      directory_leases: true
      lease_break_timeout: 35s

    # Durable Handles
    durable_handles:
      enabled: true
      default_timeout: 60s
      scavenger_interval: 10s
      max_handles_per_session: 1000
```

### SMB3 Environment Variable Overrides

All SMB3 settings can be overridden with environment variables:

```bash
# Encryption
export DITTOFS_ADAPTERS_SMB_ENCRYPTION_ENCRYPTION_MODE=required

# Signing
export DITTOFS_ADAPTERS_SMB_SIGNING_ENABLED=true
export DITTOFS_ADAPTERS_SMB_SIGNING_REQUIRED=true

# Dialect
export DITTOFS_ADAPTERS_SMB_MIN_DIALECT=3.0

# Leases
export DITTOFS_ADAPTERS_SMB_LEASES_ENABLED=true
export DITTOFS_ADAPTERS_SMB_LEASES_DIRECTORY_LEASES=true
export DITTOFS_ADAPTERS_SMB_LEASES_LEASE_BREAK_TIMEOUT=35s

# Durable Handles
export DITTOFS_ADAPTERS_SMB_DURABLE_HANDLES_ENABLED=true
export DITTOFS_ADAPTERS_SMB_DURABLE_HANDLES_DEFAULT_TIMEOUT=60s

# Cross-Protocol
export DITTOFS_ADAPTERS_SMB_CROSS_PROTOCOL_DELEGATION_RECALL_TIMEOUT=90s
export DITTOFS_ADAPTERS_SMB_CROSS_PROTOCOL_ANTI_STORM_TTL=30s
```

### 11. NFSv4 Configuration

```yaml
adapters:
  nfs:
    # NFSv4 settings
    v4_enabled: true
    delegations_enabled: true
    max_delegations: 10000
    grace_period: 90s
    lease_time: 90s
```

### 12. Kerberos Configuration

```yaml
adapters:
  nfs:
    # Kerberos (RPCSEC_GSS) settings
    kerberos:
      enabled: true
      keytab: /etc/krb5.keytab
      realm: EXAMPLE.COM
      service_principal: nfs/server.example.com@EXAMPLE.COM
```

### 13. Identity Mapping Configuration

```yaml
identity:
  # Identity mapping for NFSv4
  idmap:
    domain: example.com
    # Static mappings
    mappings:
      - nfs_name: "user@EXAMPLE.COM"
        local_uid: 1000
        local_gid: 1000
```

## Environment Variables

Override configuration using environment variables with the `DITTOFS_` prefix:

**Format**: `DITTOFS_<SECTION>_<SUBSECTION>_<KEY>`

- Use uppercase
- Replace dots with underscores
- Nested paths use underscores

**Special Variables** (not config overrides):

```bash
# Set the initial admin password on first start (instead of auto-generating one)
export DITTOFS_ADMIN_INITIAL_PASSWORD=my-secure-password
```

> **Note**: `DITTOFS_ADMIN_INITIAL_PASSWORD` is only used during the very first server start when the admin user is created. It has no effect on subsequent starts. When set, the admin account's `MustChangePassword` flag is not enabled.

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
export DITTOFS_DATABASE_SQLITE_PATH=/var/lib/dfs/controlplane.db
# PostgreSQL
export DITTOFS_DATABASE_TYPE=postgres
export DITTOFS_DATABASE_POSTGRES_HOST=localhost
export DITTOFS_DATABASE_POSTGRES_PORT=5432
export DITTOFS_DATABASE_POSTGRES_DATABASE=dfs
export DITTOFS_DATABASE_POSTGRES_USER=dfs
export DITTOFS_DATABASE_POSTGRES_PASSWORD=${POSTGRES_PASSWORD}
export DITTOFS_DATABASE_POSTGRES_SSLMODE=require

# Control Plane API Server
export DITTOFS_CONTROLPLANE_PORT=8080
export DITTOFS_CONTROLPLANE_SECRET=your-secret-key-at-least-32-characters

# Cache
export DITTOFS_CACHE_PATH=/var/lib/dfs/cache
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
DITTOFS_LOGGING_LEVEL=DEBUG ./dfs start
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
DITTOFS_ADAPTERS_NFS_PORT=12049 ./dfs start
```

## Configuration Examples

### Minimal Configuration

Server config file with minimal settings:

```yaml
logging:
  level: INFO
```

Then create stores, shares, and enable adapters via CLI:

```bash
./dfsctl store metadata add --name default --type memory
./dfsctl store block add --kind local --name default --type fs \
  --config '{"path":"/tmp/dittofs-blocks"}'
./dfsctl share create --name /export --metadata default --local default
./dfsctl adapter enable nfs
```

### Development Setup

Fast iteration with in-memory stores:

```yaml
logging:
  level: DEBUG
  format: text
```

```bash
./dfsctl store metadata add --name dev-memory --type memory
./dfsctl store block add --kind local --name dev-local --type memory
./dfsctl share create --name /export --metadata dev-memory --local dev-local
./dfsctl adapter enable nfs --port 12049
```

### Production Setup

Persistent storage with access control, structured logging, and telemetry:

```yaml
logging:
  level: WARN
  format: json
  output: /var/log/dfs/server.log

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

metadata:
  filesystem_capabilities:
    max_read_size: 1048576
    max_write_size: 1048576
```

Then create stores, shares, and enable adapters via CLI:

```bash
# Create stores
./dfsctl store metadata add --name prod-badger --type badger \
  --config '{"path":"/var/lib/dfs/metadata"}'
./dfsctl store block add --kind local --name prod-local --type fs \
  --config '{"path":"/var/lib/dfs/blocks"}'
./dfsctl store block add --kind remote --name prod-s3 --type s3 \
  --config '{"region":"us-east-1","bucket":"dfs-production"}'

# Create share and grant permissions
./dfsctl share create --name /export --metadata prod-badger \
  --local prod-local --remote prod-s3
./dfsctl share permission grant /export --user alice --level read-write

# Enable NFS adapter
./dfsctl adapter enable nfs --port 2049
```

### Multi-Share with Different Backends

Different shares using different storage backends:

```bash
# Create metadata stores
./dfsctl store metadata add --name fast-memory --type memory
./dfsctl store metadata add --name persistent-badger --type badger \
  --config '{"path":"/var/lib/dfs/metadata"}'

# Create block stores
./dfsctl store block add --kind local --name local-cache --type fs \
  --config '{"path":"/var/lib/dfs/blocks"}'
./dfsctl store block add --kind remote --name cloud-s3 --type s3 \
  --config '{"region":"us-east-1","bucket":"my-dfs-bucket"}'

# Create shares with different backends
./dfsctl share create --name /temp --metadata fast-memory --local local-cache
./dfsctl share create --name /cloud --metadata persistent-badger \
  --local local-cache --remote cloud-s3
./dfsctl share create --name /public --metadata persistent-badger --local local-cache

# Grant permissions
./dfsctl share permission grant /temp --user alice --level read-write
./dfsctl share permission grant /cloud --user alice --level read-write

# Enable NFS adapter
./dfsctl adapter enable nfs
```

### Shared Metadata Pattern

Multiple shares sharing the same metadata database:

```bash
# Create shared metadata store
./dfsctl store metadata add --name shared-badger --type badger \
  --config '{"path":"/var/lib/dfs/shared-metadata"}'

# Create block stores
./dfsctl store block add --kind local --name local-cache --type fs \
  --config '{"path":"/var/lib/dfs/blocks"}'
./dfsctl store block add --kind remote --name s3-production --type s3 \
  --config '{"region":"us-east-1","bucket":"prod-bucket"}'
./dfsctl store block add --kind remote --name s3-archive --type s3 \
  --config '{"region":"us-east-1","bucket":"archive-bucket"}'

# Both shares use the same metadata store, different remote stores
./dfsctl share create --name /prod --metadata shared-badger \
  --local local-cache --remote s3-production
./dfsctl share create --name /archive --metadata shared-badger \
  --local local-cache --remote s3-archive

# Enable NFS adapter
./dfsctl adapter enable nfs
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
DITTOFS_LOGGING_LEVEL=DEBUG ./dfs start
```
