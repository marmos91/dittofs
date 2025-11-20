# DittoFS Configuration Guide

DittoFS uses a flexible configuration system with support for YAML/TOML files and environment variable overrides.

## Table of Contents

- [Configuration Files](#configuration-files)
- [Configuration Structure](#configuration-structure)
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

### 2. Server Settings

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

### 3. Metadata Configuration

Define named metadata store instances that shares can reference:

```yaml
metadata:
  # Global settings that apply to all metadata stores
  global:
    # Filesystem capabilities and limits
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

    # DUMP operation restrictions
    dump_restricted: false
    dump_allowed_clients: []

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
```

> **Persistence**: BadgerDB stores all metadata persistently on disk. File handles, directory structure,
> permissions, and all metadata survive server restarts. The memory backend loses all data when
> the server stops.

### 4. Content Configuration

Define named content store instances that shares can reference:

```yaml
content:
  # Global settings that apply to all content stores
  global:
    # Future: cache settings, compression, encryption

  # Named content store instances
  stores:
    # Local filesystem storage for fast access
    local-disk:
      type: filesystem
      filesystem:
        path: /tmp/dittofs-content

    # S3 storage for cloud-backed shares
    s3-production:
      type: s3
      s3:
        region: us-east-1
        bucket: dittofs-production
        key_prefix: ""
        endpoint: ""
        access_key_id: ""
        secret_access_key: ""
        part_size: 10485760  # 10MB

    # In-memory storage for caching/testing
    memory-cache:
      type: memory
      memory: {}
```

> **S3 Path Design**: The S3 store uses path-based object keys (e.g., `export/docs/report.pdf`)
> that mirror the filesystem structure. This enables easy bucket inspection and metadata
> reconstruction for disaster recovery.
>
> **S3 Production Features**: The S3 content store includes production-ready optimizations:
>
> - **Range Reads**: Efficient partial reads using S3 byte-range requests (100x faster for small reads from large files)
> - **Streaming Multipart Uploads**: Automatic multipart uploads for large files (98% memory reduction)
> - **Stats Caching**: Intelligent caching reduces expensive S3 ListObjects calls by 99%+
> - **Metrics Support**: Optional instrumentation for Prometheus/observability

### 5. Shares (Exports)

Each share explicitly references metadata and content stores by name. Multiple shares can reference the same store instances for resource sharing:

```yaml
shares:
  # Fast local share using in-memory metadata and local disk
  - name: /fast
    metadata_store: memory-fast    # References metadata.stores.memory-fast
    content_store: local-disk      # References content.stores.local-disk
    read_only: false
    async: true

    # Access control
    allowed_clients: []
    denied_clients: []

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
    root_attr:
      mode: 0755
      uid: 0
      gid: 0

  # Cloud-backed share with persistent metadata
  - name: /cloud
    metadata_store: badger-main
    content_store: s3-production
    read_only: false
    async: false
    # ... (same access control options as above)

  # Archive share sharing metadata with /cloud
  - name: /archive
    metadata_store: badger-main      # Shares metadata with /cloud
    content_store: s3-archive        # Different content backend
    read_only: false
    async: false
    # ... (same access control options as above)
```

**Configuration Patterns:**

- **Shared Metadata**: `/cloud` and `/archive` both use `badger-main` - they share the same metadata database
- **Performance Tiering**: Different shares use different storage backends (memory, local disk, S3)
- **Isolation**: Different shares can use completely separate stores for security boundaries
- **Resource Efficiency**: Multiple shares can reference the same store instance (no duplication)

### 6. Protocol Adapters

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

# Server
export DITTOFS_SERVER_SHUTDOWN_TIMEOUT=60s

# Content store
export DITTOFS_CONTENT_TYPE=filesystem
export DITTOFS_CONTENT_FILESYSTEM_PATH=/data/dittofs

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

metadata:
  stores:
    default:
      type: memory

content:
  stores:
    default:
      type: filesystem
      filesystem:
        path: /tmp/dittofs-content

shares:
  - name: /export
    metadata_store: default
    content_store: default

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

metadata:
  stores:
    dev-memory:
      type: memory

content:
  stores:
    dev-memory:
      type: memory

shares:
  - name: /export
    metadata_store: dev-memory
    content_store: dev-memory
    async: true
    identity_mapping:
      map_all_to_anonymous: true

adapters:
  nfs:
    enabled: true
    port: 12049
```

### Production Setup

Persistent storage with access control:

```yaml
logging:
  level: WARN
  format: json
  output: /var/log/dittofs/server.log

server:
  shutdown_timeout: 30s

metadata:
  global:
    filesystem_capabilities:
      max_read_size: 1048576
      max_write_size: 1048576
    dump_restricted: true
    dump_allowed_clients:
      - 127.0.0.1
      - 10.0.0.0/8

  stores:
    prod-badger:
      type: badger
      badger:
        db_path: /var/lib/dittofs/metadata

content:
  stores:
    prod-disk:
      type: filesystem
      filesystem:
        path: /var/lib/dittofs/content

shares:
  - name: /export
    metadata_store: prod-badger
    content_store: prod-disk
    read_only: false
    async: false  # Synchronous writes for data safety
    allowed_clients:
      - 192.168.1.0/24
    denied_clients:
      - 192.168.1.50
    identity_mapping:
      map_all_to_anonymous: false
      map_privileged_to_anonymous: true
    root_attr:
      mode: 0755
      uid: 0
      gid: 0

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
metadata:
  stores:
    fast-memory:
      type: memory
    persistent-badger:
      type: badger
      badger:
        db_path: /var/lib/dittofs/metadata

content:
  stores:
    local-disk:
      type: filesystem
      filesystem:
        path: /var/lib/dittofs/content
    cloud-s3:
      type: s3
      s3:
        region: us-east-1
        bucket: my-dittofs-bucket

shares:
  # Fast temporary share
  - name: /temp
    metadata_store: fast-memory
    content_store: local-disk
    read_only: false
    identity_mapping:
      map_all_to_anonymous: true

  # Cloud-backed persistent share
  - name: /cloud
    metadata_store: persistent-badger
    content_store: cloud-s3
    read_only: false
    allowed_clients:
      - 10.0.1.0/24

  # Public read-only share
  - name: /public
    metadata_store: persistent-badger
    content_store: local-disk
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
metadata:
  stores:
    shared-badger:
      type: badger
      badger:
        db_path: /var/lib/dittofs/shared-metadata

content:
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
    metadata_store: shared-badger    # Shared metadata
    content_store: s3-production
    read_only: false

  # Archive share (shares metadata with /prod)
  - name: /archive
    metadata_store: shared-badger    # Same metadata store
    content_store: s3-archive        # Different content backend
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
