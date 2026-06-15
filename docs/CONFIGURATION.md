# DittoFS Configuration Guide

DittoFS uses a flexible configuration system with support for YAML/TOML files and environment variable overrides.

> Unfamiliar with terms like CAS, AUTH_UNIX, NTLM, or root-squash? See the
> [Glossary](GLOSSARY.md) for plain-language definitions.

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

The config file is resolved per platform:

| Platform | Default config file |
| --- | --- |
| Linux / macOS | `$XDG_CONFIG_HOME/dittofs/config.yaml` (typically `~/.config/dittofs/config.yaml`) |
| Windows | `%APPDATA%\dittofs\config.yaml` (typically `...\AppData\Roaming\dittofs\config.yaml`) |

Pass `--config <path>` to any `dfs` command to override the location.

### State Directory

Runtime state — the log file (`dittofs.log`) and PID file — lives in a separate state directory, also resolved per platform:

| Platform | Default state directory |
| --- | --- |
| Linux / macOS | `$XDG_STATE_HOME/dittofs` (typically `~/.local/state/dittofs`); falls back to the system temp directory if no home is resolvable |
| Windows | `%LOCALAPPDATA%\dittofs` |

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
    path: /var/lib/dittofs/controlplane.db

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
  host: 127.0.0.1            # Bind interface (loopback by default; see below)
  port: 8080                 # HTTP/HTTPS port for API endpoints
  read_timeout: 10s          # Max time to read request
  write_timeout: 10s         # Max time to write response
  idle_timeout: 60s          # Max idle time for keep-alive

  # Native TLS (optional). DittoFS only loads these files; it does not issue,
  # renew, or rotate certificates. When cert_file and key_file are both set,
  # the API serves HTTPS and hot-reloads the files when they change on disk.
  # When unset, the API serves plain HTTP. See docs/SECURITY.md.
  # tls:
  #   cert_file: /etc/dittofs/tls/tls.crt
  #   key_file: /etc/dittofs/tls/tls.key
  #   client_ca: /etc/dittofs/tls/ca.crt   # optional: require + verify client certs (mTLS)
  #   min_version: "1.2"                    # "1.2" (default) or "1.3"

  # Profiling (disabled by default; for benchmarks/diagnostics only).
  # The rate keys only take effect when pprof is true; with pprof off all
  # sampling stays off regardless of their values. When pprof is on and a rate
  # is unset/0 it falls back to the default shown below.
  pprof: false                 # Expose /debug/pprof/* endpoints
  # pprof_mutex_rate: 100        # runtime.SetMutexProfileFraction (default 100 when pprof on)
  # pprof_block_rate_ns: 1000000 # runtime.SetBlockProfileRate ns (default 1000000 when pprof on)

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
| `host` | `127.0.0.1` | Bind interface. Loopback-only by default (secure-by-default for single-host). Set to `0.0.0.0` for multi-host / Kubernetes (then front it with TLS termination — see [TLS and bind address](#tls-and-bind-address)) |
| `port` | `8080` | HTTP/HTTPS port for API endpoints |
| `read_timeout` | `10s` | Maximum duration to read request |
| `write_timeout` | `10s` | Maximum duration to write response |
| `idle_timeout` | `60s` | Maximum idle time for keep-alive |
| `pprof` | `false` | Expose Go `/debug/pprof/*` profiling endpoints |
| `pprof_mutex_rate` | `100` (when `pprof: true`; else `0`) | Mutex contention sampling, 1 per N events. Applied only when `pprof: true`; unset/`0` then falls back to `100`. Without it `/debug/pprof/mutex` is header-only. Disable profiling via `pprof: false`, not by zeroing this |
| `pprof_block_rate_ns` | `1000000` (when `pprof: true`; else `0`) | Block profiling rate in ns, 1 sample per N ns blocked. Applied only when `pprof: true`; unset/`0` then falls back to `1000000`. Without it `/debug/pprof/block` is header-only. Disable profiling via `pprof: false`, not by zeroing this |

**JWT Configuration Options:**

| Option | Default | Description |
|--------|---------|-------------|
| `secret` | (required) | HMAC signing key (min 32 chars) |
| `access_token_duration` | `15m` | Access token lifetime |
| `refresh_token_duration` | `168h` | Refresh token lifetime (7 days) |

> **Security Note**: The JWT secret should be kept confidential. Use the `DITTOFS_CONTROLPLANE_SECRET` environment variable in production to avoid storing secrets in config files.

#### TLS and bind address

The control plane API carries admin logins, the `dfsctl` remote password login, operator credentials, and JWTs. By default the server binds to `127.0.0.1` (loopback only) so a fresh `dfs start` is not reachable off-host. For any deployment that must accept connections from another machine — multi-host, Kubernetes — set `host: 0.0.0.0` and protect the listener with TLS.

DittoFS offers **native, file-based TLS** as a secure-by-default floor. It is intentionally thin: DittoFS **loads** certificate files (and an optional client CA for mTLS) — it is **not a certificate authority**, does **not** generate self-signed certificates, and does **not** do ACME, issuance, renewal, or rotation. Certificate *lifecycle* is left to your platform (cert-manager, a mounted Kubernetes Secret, Vault, your PKI). When the platform rewrites the files on disk, DittoFS hot-reloads them with no restart.

```yaml
controlplane:
  host: 0.0.0.0
  port: 8080
  tls:
    cert_file: /etc/dittofs/tls/tls.crt
    key_file: /etc/dittofs/tls/tls.key
    client_ca: /etc/dittofs/tls/ca.crt   # optional → mutual TLS
    min_version: "1.2"
```

**TLS Configuration Options:**

| Option | Default | Description |
|--------|---------|-------------|
| `cert_file` | (unset) | Path to the PEM server certificate (or chain). Both `cert_file` and `key_file` must be set to enable HTTPS; setting one without the other is a fatal config error |
| `key_file` | (unset) | Path to the PEM private key for `cert_file` |
| `client_ca` | (unset) | Path to a PEM CA bundle. When set, the server **requires and verifies** a client certificate signed by one of these CAs (mutual TLS). Requires `cert_file`/`key_file` |
| `min_version` | `1.2` | Minimum negotiated TLS version: `"1.2"` or `"1.3"` |

When `cert_file`/`key_file` are unset, the server serves plain HTTP exactly as before (back-compatible). When set, it serves HTTPS; the files are read and parsed at startup, so a missing or malformed certificate fails fast with a clear error.

**Recommended deployment model:** terminate TLS for the edge at an ingress / service mesh / reverse proxy (NGINX), and use DittoFS native TLS (or mTLS via `client_ca`) as the secure floor for non-Kubernetes hosts and direct `dfsctl` access. See [docs/SECURITY.md](SECURITY.md) and [docs/DEPLOYMENT.md](DEPLOYMENT.md). For Kubernetes, the operator renders `host: 0.0.0.0` automatically so the API `Service` can reach the pod; see [docs/DEPLOYMENT.md](DEPLOYMENT.md).

Related glossary terms: [TLS / mTLS](GLOSSARY.md#authentication).

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

Per-share block storage is configured via `dfsctl store` / `dfsctl share` commands (not the server config file). Each share owns an isolated local storage directory plus a reference to a remote store (S3 or filesystem). The block store lives in `pkg/block/engine/` and composes a local tier, a remote tier, the unified CAS-keyed in-memory `Cache`, a syncer (async local-to-remote transfer), and a garbage collector.

#### Append-log tier

The local filesystem store writes through per-file append-only logs that are
compacted into content-addressed chunks (`blocks/{hh}/{hh}/{hex}`) and
garbage-collected by the mark-sweep GC. This is the only local write path —
older servers' `{payloadID}/block-{idx}` layout must be converted with
`dfs migrate-to-cas` before a v0.16+ server will start.

These keys live inside the per-share `local` block store's `config` JSON
(passed via `dfsctl store block local add --config '{...}'` or the REST API).
They only take effect when the local store type is `fs`. (A legacy
`use_append_log` key is accepted but ignored — appending is mandatory — and
logs a startup warning.)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `max_log_bytes` | int | `1073741824` (1 GiB) | Per-share total-log-bytes budget; writers block on pressure when exceeded. Values above 2^53 (~9 PiB) lose precision through JSON parsing. |
| `rollup_workers` | int | `2` | Number of rollup goroutines (BLAKE3 + FastCDC) per share. |
| `stabilization_ms` | int | `250` | Dirty-interval stabilization window in milliseconds before rollup. |
| `orphan_log_min_age_seconds` | int | `3600` (1h) | Minimum log-file mtime age before the boot-time orphan sweep may unlink it. Prevents fresh (not-yet-rolled-up) logs from being swept when metadata is absent. |

Env-var mapping follows the dot-path convention:
`DITTOFS_BLOCKSTORE_LOCAL_FS_MAX_LOG_BYTES`,
`DITTOFS_BLOCKSTORE_LOCAL_FS_ROLLUP_WORKERS`,
`DITTOFS_BLOCKSTORE_LOCAL_FS_STABILIZATION_MS`,
`DITTOFS_BLOCKSTORE_LOCAL_FS_ORPHAN_LOG_MIN_AGE_SECONDS`.

The local `fs` store requires a metadata backend that implements
`metadata.RollupStore` to persist each log's `rollup_offset`; memory, badger,
and postgres all qualify.

#### GC knobs

The CAS write path uses an async syncer and a fail-closed mark-sweep
garbage collector. The syncer's sizing (upload concurrency, claim timeout,
etc.) is **not** an operator-facing config section — it is auto-deduced from
system resources at startup and constructed in code; there is no `syncer:`
config block (a stale `syncer:` section in a config file is tolerated but
ignored, logged as an unknown key).

The mark-sweep GC is the one tunable surface, configured via the top-level
`gc:` server-config section:

```yaml
gc:
  grace_period: 1h            # Objects whose LastModified is newer than
                              # (snapshot - grace_period) are NEVER
                              # deleted. Default 1h. Values in (0, 5m)
                              # are REJECTED at config load; values in
                              # [5m, 10m) are accepted but emit a
                              # warning. The cushion protects in-flight
                              # uploads whose metadata-txn lands after
                              # the snapshot.
  dry_run_sample_size: 1000   # Maximum candidate keys reported in
                              # --dry-run mode. Default 1000.
```

**Tuning guidance:**

- v0.15.0 ships only on-demand GC. Run via
  `dfsctl store block gc <share> --dry-run` (capped by
  `gc.dry_run_sample_size`) until you have measured the
  hashes_marked / objects_swept ratio for your workload, then schedule
  the real run via cron at the cadence that matches your delete rate.
  No periodic-GC scheduler ships today; trigger GC on demand or via cron.
- `gc.grace_period` MUST be longer than your worst-case
  metadata-commit latency after a successful PUT. The default 1h is
  comfortable for any commit path that completes in seconds.

Env-var mapping (dot-path convention; the top-level `gc` block binds
directly):
`DITTOFS_GC_GRACE_PERIOD`,
`DITTOFS_GC_DRY_RUN_SAMPLE_SIZE`.

See [ARCHITECTURE.md](ARCHITECTURE.md#garbage-collection-mark-sweep)
for the full mark-sweep design and [CLI.md](CLI.md) for the on-demand
`dfsctl store block gc` command.

#### Local cache size limit & write backpressure

When a share has a **remote** block store configured (S3 or filesystem
remote), the on-disk local tier is a **temporary write-through cache**, not
durable storage — every chunk is mirrored to the remote and may be evicted
locally once synced. To stop a fast writer with a slow/lagging uploader from
filling the host volume, the local cache is bounded and writes apply
**graceful, observable backpressure** when it fills:

- **Bounded cache.** If a remote is configured and you set no explicit
  per-share size (`dfsctl share … --local-store-size`), the cache is capped at
  `blockstore.local.default_remote_cache_size` (default **10 GiB**). An
  explicit `--local-store-size` always wins. **Local-only shares are
  unaffected** — they keep their (unlimited / system-deduced) local size and
  never apply remote-cache backpressure.
- **Backpressure stall.** When the cache is full and every cached chunk is
  still unsynced, a write **stalls** waiting for the syncer to drain to the
  remote and free space, rather than failing. The stall is bounded by
  `blockstore.local.backpressure_max_wait` (default **60s**).
- **Hard failure only when the remote cannot drain.** If the remote is
  **unhealthy** (genuinely unreachable, not merely slow) or the backpressure
  window is exceeded, the write fails with disk-full
  (`NFS3ERR_NOSPC` / `NFS4ERR_NOSPC` / SMB `STATUS_DISK_FULL`) instead of
  silently filling the disk.

**Diagnosing a stall.** Backpressure engage/release events are logged
(rate-limited) at `INFO`, so a stalled writer is never a mystery:

```
INFO  local cache backpressure engaged: waiting for syncer to drain
      store=… disk_used=10737418240 max_disk=10737418240 needed=…
      unsynced_bytes=… remote_healthy=true max_wait_ms=60000
INFO  local cache backpressure released  store=… reason=space_freed
      disk_used=… max_disk=… unsynced_bytes=… remote_healthy=true stall_ms=…
```

`reason` distinguishes a clean recovery (`space_freed`) from a failure
(`window_exceeded`, `remote_unhealthy`).

These knobs live in the top-level server-config `blockstore.local` block:

```yaml
blockstore:
  local:
    default_remote_cache_size: 10737418240   # 10 GiB; cap for remote-backed
                                             # shares with no explicit size.
                                             # 0 = no conditional cap.
    backpressure_max_wait: 60s               # Max time a write stalls for the
                                             # syncer to drain before disk-full.
    dedup_lru_size: 4096                      # In-memory dedup LRU slot count.
```

Env-var mapping (dot-path convention):
`DITTOFS_BLOCKSTORE_LOCAL_DEFAULT_REMOTE_CACHE_SIZE`,
`DITTOFS_BLOCKSTORE_LOCAL_BACKPRESSURE_MAX_WAIT`,
`DITTOFS_BLOCKSTORE_LOCAL_DEDUP_LRU_SIZE`.

> Prometheus metrics for cache pressure / unsynced bytes are tracked
> separately (server-wide instrumentation, issue #1188); today the signal is
> the structured logs above.

#### Recycle bin (trash)

The recycle bin is configured **per share** via `dfsctl share create` /
`dfsctl share edit` (or the REST share create/update body), not the
server config file. When enabled, deleting a file or directory moves it
into a visible `#recycle` directory at the share root instead of
destroying it; it can be restored over the mount or with `dfsctl trash`.

| Setting (`dfsctl` flag) | REST field | Type | Default | Meaning |
|---|---|---|---|---|
| `--enable-trash` | `trash_enabled` | bool | `false` | Turn the per-share recycle bin on or off. Disabling it auto-empties the bin (permanently deletes its contents). |
| `--trash-retention-days` | `trash_retention_days` | int | `0` | Auto-purge bin entries older than N days. `0` = keep forever. |
| `--trash-restrict-empty-to-admin` | `trash_restrict_to_admin` | bool | `false` | Restrict emptying the bin to admins. Users may still restore. |
| `--trash-max-size` | `trash_max_bytes` | int64 (bytes) | `0` | Cap total bytes held in the bin; over-cap evicts oldest-first. `0` = unbounded. |
| `--trash-exclude` | `trash_exclude_patterns` | glob (repeatable) | (none) | Deletions matching any glob bypass the bin and are removed immediately. |

A background reaper enforces `trash_retention_days` and
`trash_max_bytes` on an hourly interval. Deletes of items already
*inside* `#recycle` are permanent, and in-place truncate/overwrite of a
file's content is not recycled (only unlink and replace-overwrite are).
`dfsctl share show <name>` displays the active trash configuration.

```bash
# Enable the bin with a 30-day retention and a 10 GiB cap
./dfsctl share create --name /docs --metadata badger-main --local local-cache \
  --enable-trash --trash-retention-days 30 --trash-max-size 10737418240

# Change settings on an existing share (applied live)
./dfsctl share edit /docs --trash-retention-days 7 \
  --trash-exclude '*.tmp' --trash-exclude '*.cache'
```

See [CLI.md](CLI.md#recycle-bin-trash) for the `dfsctl trash`
management commands and [ARCHITECTURE.md](ARCHITECTURE.md#metadataservice)
for the recycle-trap design.

#### Remote block-level compression (opt-in)

A remote block store may compress block payloads before upload and
decompress on download. The plaintext BLAKE3 hash remains the CAS key,
so dedup and GC are unaffected. The decorator is per-remote: every
share that references the remote inherits its compression policy.

Add a `compression` block to the remote store's `config` JSON when
creating it:

```bash
./dfsctl store block add --kind remote --name prod-s3 --type s3 \
  --config '{"region":"us-east-1","bucket":"dfs-production","compression":{"algo":"zstd"}}'
```

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `algo` | string | `"zstd"` | Algorithm: `"zstd"` or `"lz4"`. Defaults to zstd when the `compression` block is present but `algo` is omitted. |

Notes:

- Absence of the `compression` block means no wrapping — zero behavior
  change for existing remotes.
- Per-block adaptive: if the compressed body is not strictly smaller
  than the plaintext, the decorator stores the raw plaintext with no
  header. Incompressible payloads (random data, already-compressed
  media) cost only the encoder pass, no on-wire expansion.
- `GetRange` on a framed block decompresses the full block before
  slicing — there is no random access into a compressed body. Read
  paths that consume whole CDC chunks are unaffected.
- The policy is captured at remote-store creation; restart the share
  after editing the config to switch algorithms. Mixed framed and raw
  blocks coexist within one remote and the reader auto-detects via the
  5-byte `DFCMP` magic prefix.

#### Remote block-level encryption (opt-in)

A remote block store may also encrypt block payloads before upload using
client-side envelope encryption. Compression (when enabled) runs
**before** encryption — encrypted bytes are incompressible by design.
See [ENCRYPTION.md](ENCRYPTION.md) for the full threat model and design.

Add an `encryption` block to the remote store's `config` JSON:

```yaml
encryption:
  aead: aes-256-gcm           # aes-256-gcm | chacha20-poly1305 | xchacha20-poly1305
  key:
    kind: local               # local | kmip
    # kind=local
    file: /etc/dittofs/keys/share.key
    # kind=kmip
    endpoint: kms.example.com:5696
    server_ca: /etc/dittofs/kmip/ca.pem
    client_cert: /etc/dittofs/kmip/client.pem
    client_key:  /etc/dittofs/kmip/client.key
    key_uid: 12345-abcde-...
    timeout_ms: 5000
```

The passphrase that unlocks a local key file is read from the
`DITTOFS_ENCRYPTION_PASSPHRASE` environment variable — never the config
file or command line.

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

Users and groups live in the control-plane database, not the config file. Manage them with
`dfsctl` against a running server (run `dfsctl login` first). See [CLI.md](CLI.md) for the
complete, generated reference.

**User Commands:**

```bash
# Create a user (password prompted interactively)
dfsctl user create --username alice
dfsctl user create --username alice --host-uid                 # map to your current host UID
dfsctl user create --username bob --email bob@example.com --groups editors,viewers

# Inspect and edit
dfsctl user list
dfsctl user get alice
dfsctl user update alice --email alice@example.com
dfsctl user delete alice

# Passwords
dfsctl user change-password           # change your own
dfsctl user password alice            # admin: reset another user's password
```

**Group Commands:**

```bash
dfsctl group create --name editors
dfsctl group list
dfsctl group get editors
dfsctl group add-user editors alice
dfsctl group remove-user editors alice
dfsctl group delete editors
```

**Share Permissions:**

Permissions are granted per share to a user or group via `dfsctl share permission`:

```bash
dfsctl share permission list  /export
dfsctl share permission grant /export --user alice  --level read-write
dfsctl share permission grant /export --group editors --level read
dfsctl share permission revoke /export --user alice
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
    max_connections: 0           # 0 falls back to 1024 (default cap)

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
    # Credits control SMB2 flow control and client parallelism.
    # Defaults match Samba (`smb2 max credits = 8192`, initial grant = 1)
    # and Windows 2008R2+. See docs/SMB.md for the credit-accounting model
    # and rationale.
    credits:
      strategy: echo            # fixed, echo, adaptive (default: echo)
      min_grant: 1              # Minimum credits per response
      max_grant: 8192           # Maximum credits per response
      initial_grant: 1          # Floor when client requests 0 credits
      max_session_credits: 8192 # Per-connection credit window cap

      # Adaptive strategy thresholds (ignored for fixed/echo)
      load_threshold_high: 1000       # Start throttling above this load
      load_threshold_low: 100         # Boost credits below this load
      aggressive_client_threshold: 256 # Throttle clients with this many outstanding
```

**SMB Credit Strategies:**

| Strategy | Description | Use Case |
|----------|-------------|----------|
| `echo` | Grants what client requests (bounded by `MinGrant`/`MaxGrant`, clamped by window) | **Recommended, default** — matches Samba and MS-SMB2 3.3.1.2 |
| `fixed` | Always grants `initial_grant` credits | Simple, predictable behavior |
| `adaptive` | Scales grants by live load and client-outstanding factors | Throughput-focused; may grant more aggressively than clients expect |

**SMB Credit Configuration Options:**

| Option | Default | Description |
|--------|---------|-------------|
| `strategy` | `echo` | Credit grant strategy |
| `min_grant` | `1` | Minimum credits per response |
| `max_grant` | `8192` | Maximum credits per response |
| `initial_grant` | `1` | Floor when client requests 0 credits (Samba-compatible) |
| `max_session_credits` | `8192` | Per-connection credit window cap (Samba's `smb2 max credits`) |
| `load_threshold_high` | `1000` | (adaptive only) Server load that triggers throttling |
| `load_threshold_low` | `100` | (adaptive only) Server load that triggers boost |
| `aggressive_client_threshold` | `256` | (adaptive only) Outstanding requests that trigger client throttling |

> **Note**: Every response's credit grant is clamped to the connection's
> remaining window capacity before being written, regardless of strategy.
> This prevents the client's per-connection `cur_credits` counter from
> overflowing — Samba's client hard-caps it at `uint16` max and rejects
> overflowing responses with `NT_STATUS_INVALID_NETWORK_RESPONSE`
> (see issue #378 and `docs/SMB.md` §Credit Flow Control).

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

## Migration

### Required when upgrading from v0.15.x or earlier

v0.16.0 replaces the legacy `<share>/<file>/<idx>.blk` block layout with a
content-addressed store (CAS). Pre-v0.16 storage directories must be migrated
before `dfs start` will succeed. The migration is **irreversible**: once a
share has been flipped to the CAS layout there is no supported path back to
the legacy `.blk` layout — keep an out-of-band backup if your operational
posture requires rollback.

### Boot-guard behavior

On startup, `dfs start` opens each share's block store directory and checks
for a `.cas-migrated-v1` sentinel file at the FSStore base directory
(`<storage_dir>/shares/<name>/blocks/.cas-migrated-v1`). If the sentinel is
missing AND legacy `.blk` files are present under the same directory, the
server refuses to start (per-share fail-fast):

- Exits with code **78** (`EX_CONFIG` per sysexits(3)).
- Prints the following directive to stderr (showing the offending share
  path):
  ```
  Detected legacy .blk layout: share "<name>": share <path>: blockstore: legacy .blk layout detected (run `dfs migrate-to-cas`)
  v0.16+ requires CAS migration. Run:
      dfs migrate-to-cas --share <name>
  or, to migrate every share at once:
      dfs migrate-to-cas
  See docs/CONFIGURATION.md §migration.
  ```
- Halts on the FIRST share that surfaces the legacy layout. Healthy
  already-migrated shares are not started in the same boot; fix the
  offending share and retry.

### Running the migration

The migration is an offline operation — stop the server first. The
`dfs migrate-to-cas` command refuses to run while a live `dfs` PID lockfile
exists:

```bash
dfs stop
dfs migrate-to-cas
```

Flags:

| Flag | Default | Description |
|------|---------|-------------|
| `--storage-dir <path>` | **required** | Storage root. The command discovers shares under `<storage-dir>/shares/`. There is no config-derived default; pass the storage root explicitly. |
| `--share <name>` | (all shares) | Scope migration to one share. Default migrates every share found under `<storage-dir>/shares/`. |
| `--dry-run` | `false` | Walk the legacy `.blk` tree and report file count, total bytes, estimated dedup ratio, and ETA. Writes nothing — does not touch the journal, does not write the sentinel. |
| `--json` | `false` | Emit one JSON object per line of progress on stdout for machine parsing. |
| `--config <path>` | (default) | Override config file location. Inherited from the root `dfs` command. |

Progress is reported to stdout approximately once per second. With `--json`,
each line has the shape:

```json
{"ts":"<RFC3339>","share":"<name>","files_done":N,"bytes_done":N,"files_per_sec":F,"mib_per_sec":F,"dedup_hits":N,"eta_seconds":F}
```

Plain text progress reads `[<share>] N files, X.X MiB/s, dedup_hits=K`.

### Crash safety

The migration is idempotent. A per-share journal at
`<storage_dir>/shares/<share>/.dittofs-migrate-to-cas.state` records the
last-completed file path and byte offset. If interrupted (Ctrl-C, kill -9,
power loss, panic, OOM), rerunning `dfs migrate-to-cas` resumes from the
journaled position. The journal is removed on best-effort cleanup only AFTER
the per-share sentinel write succeeds — a failed sentinel write preserves the
journal so a rerun can pick up exactly where the prior left off.

The CAS Put surface is idempotent on hash collision, so re-processing an
in-flight file at the resume point is safe (chunks already uploaded are
treated as dedup hits on the second pass).

### Verifying completion

Success is recorded by a per-share sentinel file at
`<storage_dir>/shares/<share>/blocks/.cas-migrated-v1` (one per share — `--share <name>` migrations
produce just that share's sentinel; un-scoped migrations produce one sentinel
per share at each share's completion, so partial-success states are
operationally well-defined). Contents:

```json
{
  "Version":     "v1",
  "CompletedAt": "2026-05-20T14:30:00Z",
  "ToolVersion": "v1.0.0",
  "ShareDir":    "/path/to/share"
}
```

The sentinel is written via atomic rename (`.cas-migrated-v1.tmp` → fsync →
close → rename → syncDir) only after every chunk for the share has been
committed and verified — partial migrations cannot leave a sentinel behind.
**Do not hand-create or hand-edit this file.** It is intended as a one-way
irreversibility marker; modifying it bypasses the boot guard but cannot fix
a half-migrated store and will surface I/O errors on the first legacy
FileBlock access.

To confirm a share is fully migrated, inspect the sentinel directly:

```bash
cat <storage_dir>/shares/<name>/.cas-migrated-v1
```

A successful `dfs start` against the share is the final verification: the
boot guard exits 78 on any share whose sentinel is missing.

### Recovery from a failed migration

1. Inspect stderr (or the JSON progress stream) for the file path + offset
   at which the migration halted.
2. Inspect the journal at
   `<storage_dir>/<share>/.dittofs-migrate-to-cas.state` to confirm the
   resume point.
3. Rerun `dfs migrate-to-cas` (optionally with `--share <name>` to scope to
   the affected share). Already-migrated shares are skipped on rerun (their
   sentinels short-circuit the boot guard at the fs-layer constructor).
4. If a chunk verification mismatch occurred (post-Put BLAKE3 disagreement —
   `ErrChunkPutMismatch`), this indicates storage corruption between Put and
   re-Get. Investigate the destination block store (disk health, S3
   eventual-consistency on overwrite, filesystem corruption) before
   retrying; the journal preserves the resume point for forensics.

### See also

- [docs/CLI.md — `dfs migrate-to-cas`](CLI.md#dfs-migrate-to-cas) for the
  full command-line reference (synopsis, flag table, exit codes, examples).

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
export DITTOFS_DATABASE_SQLITE_PATH=/var/lib/dittofs/controlplane.db
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
export DITTOFS_CONTROLPLANE_PPROF=false
export DITTOFS_CONTROLPLANE_PPROF_MUTEX_RATE=100
export DITTOFS_CONTROLPLANE_PPROF_BLOCK_RATE_NS=1000000
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

metadata:
  filesystem_capabilities:
    max_read_size: 1048576
    max_write_size: 1048576
```

Then create stores, shares, and enable adapters via CLI:

```bash
# Create stores
./dfsctl store metadata add --name prod-badger --type badger \
  --config '{"path":"/var/lib/dittofs/metadata"}'
./dfsctl store block add --kind local --name prod-local --type fs \
  --config '{"path":"/var/lib/dittofs/blocks"}'
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
  --config '{"path":"/var/lib/dittofs/metadata"}'

# Create block stores
./dfsctl store block add --kind local --name local-cache --type fs \
  --config '{"path":"/var/lib/dittofs/blocks"}'
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
  --config '{"path":"/var/lib/dittofs/shared-metadata"}'

# Create block stores
./dfsctl store block add --kind local --name local-cache --type fs \
  --config '{"path":"/var/lib/dittofs/blocks"}'
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
