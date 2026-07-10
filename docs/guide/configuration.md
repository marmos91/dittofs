# DittoFS Configuration Guide

DittoFS uses a flexible configuration system with support for YAML/TOML files and environment variable overrides.

> Unfamiliar with terms like CAS, AUTH_UNIX, NTLM, or root-squash? See the
> [Glossary](glossary.md) for plain-language definitions.

## Table of Contents

- [Configuration Files](#configuration-files)
- [Configuration Structure](#configuration-structure)
  - [Logging](#1-logging)
  - [Observability](#2-observability)
  - [Server Settings](#3-server-settings)
  - [Database (Control Plane)](#4-database-control-plane)
  - [API Server](#5-api-server)
  - [Block Store Configuration](#6-block-store-configuration)
  - [Metadata Configuration](#7-metadata-configuration)
  - [Shares (Exports)](#8-shares-exports)
  - [User Management](#9-user-management)
  - [Protocol Adapters](#10-protocol-adapters)
  - [Snapshot Scheduler](#14-snapshot-scheduler)
- [Metrics (Prometheus)](#metrics-prometheus)
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

### 2. Observability

DittoFS has **no OpenTelemetry / distributed-tracing subsystem**. Observability is
provided by an opt-in Prometheus `/metrics` endpoint on a dedicated listener. See
[Metrics (Prometheus)](#metrics-prometheus) below for the full configuration.
### 3. Server Settings

Application-wide server configuration:

```yaml
shutdown_timeout: 30s   # Maximum time to wait for graceful shutdown
```

> The Prometheus `metrics:` block is a top-level key, documented in its own
> section below ([Metrics (Prometheus)](#metrics-prometheus)).

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
  drain_stall_timeout: 5m    # Abort POST /system/drain-uploads only if no
                             # upload completes within this window (inactivity
                             # timeout, NOT a total cap — a large flush may run
                             # for as long as it keeps making progress)

  # Force the bootstrap "admin" user to set a new password on first login.
  # Default true (secure by default). Set to false for automated/test
  # deployments that provision the admin password out-of-band and don't want
  # the forced first-login change. (Supplying DITTOFS_ADMIN_INITIAL_PASSWORD
  # also skips the forced change, since the operator already chose the password.)
  require_initial_password_change: true

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
| `drain_stall_timeout` | `5m` | Inactivity bound for `POST /system/drain-uploads`. The drain has **no total time cap** — a multi-GiB flush runs as long as it keeps making progress — and is aborted (504) only if no upload completes within this window (the remote stalled). Mirrors rclone's `--timeout` |
| `require_initial_password_change` | `true` | Force the bootstrap `admin` user to change its password on first login. Set to `false` to opt out (automated/test deployments). Also skipped when `DITTOFS_ADMIN_INITIAL_PASSWORD` is set |
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

**Recommended deployment model:** terminate TLS for the edge at an ingress / service mesh / reverse proxy (NGINX), and use DittoFS native TLS (or mTLS via `client_ca`) as the secure floor for non-Kubernetes hosts and direct `dfsctl` access. See [docs/SECURITY.md](security.md) and [docs/DEPLOYMENT.md](install.md). For Kubernetes, the operator renders `host: 0.0.0.0` automatically so the API `Service` can reach the pod; see [docs/DEPLOYMENT.md](install.md).

Related glossary terms: [TLS / mTLS](glossary.md#authentication).

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
pre-v0.16 `{payloadID}/block-{idx}` layouts must be converted with
dittofs ≤ v0.21 (`dfs migrate-to-cas`) before the server will start.

These keys live inside the per-share `local` block store's `config` JSON
(passed via `dfsctl store block local add --config '{...}'` or the REST API).
They only take effect when the local store type is `fs`. (A legacy
`use_append_log` key is accepted but ignored — appending is mandatory — and
logs a startup warning.)

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `max_log_bytes` | int | deduced (25% of RAM, floor 1 GiB) | **Per-share** append-log pressure budget; writers block (`ErrPressureTimeout`) when the buffered log exceeds it. This per-store value takes **precedence** over the global `blockstore.local.max_log_bytes` and the system-deduced default. Values above 2^53 (~9 PiB) lose precision through JSON parsing. |
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

##### Append-log pressure budget (`max_log_bytes`)

`max_log_bytes` is **THE** append-log backpressure lever. The on-disk append
log buffers freshly-written bytes before the async rollup folds them into CAS
chunks; once the buffered total exceeds this budget, `AppendWrite` stalls and
eventually returns `ErrPressureTimeout` (surfaced as disk-full to the
protocol). It is sized **relative to available memory** — the disk-backed
pre-flush working set scales with how fast the host absorbs writes — at 25% of
RAM with a 1 GiB floor (the historical fixed default becomes the minimum on
small machines). Run `dfs config show --deduced` to see the effective value.

The effective budget resolves with the following **precedence** (highest
first):

1. **Per-store** block-store `config["max_log_bytes"]` — set per share via
   `dfsctl store block local edit <share> --config '{"max_log_bytes": 2147483648}'`.
2. **Global** server-config `blockstore.local.max_log_bytes` — applies to every
   share that does not set the per-store key.
3. **System-deduced** default (25% of RAM, floor 1 GiB).

The global knob lives in the top-level server-config `blockstore.local` block
and binds to the env var `DITTOFS_BLOCKSTORE_LOCAL_MAX_LOG_BYTES`:

```yaml
blockstore:
  local:
    max_log_bytes: 2147483648   # 2 GiB global append-log pressure budget.
                                # 0 / unset = system-deduced default
                                # (25% of RAM, floor 1 GiB). A per-store
                                # config max_log_bytes overrides this.
```

#### Durability & the CLOSE/COMMIT contract (`durable`)

Durability is a **per-store property**: whether bytes a store has accepted
survive a daemon crash / restart. Each local and remote store resolves an
effective `durable` flag at construction — a **type default** that an operator
may override.

| Store type | Kind | Default `durable` |
|------------|------|-------------------|
| `fs` | local | `true` — bytes are on disk; un-mirrored chunks are never evicted, survive restart, and re-mirror asynchronously |
| `memory` | local | `false` — volatile, lost on restart |
| `s3` | remote | `true` — durable object storage |
| `memory` | remote | `false` — test/dev fixture, lost on restart |

Override the default per store by adding a `durable` bool to the store's
`config` JSON, e.g. for a local `fs` store on a volatile tmpfs mount:

```sh
dfsctl store block local edit <share> --config '{"durable": false}'
```

or to deliberately treat a memory store as durable in a test/dev setup:

```sh
dfsctl store block local edit <share> --config '{"durable": true}'
```

A non-bool `durable` value is ignored with a startup warning (the type default
stands). The effective values are surfaced as `Local Durable` / `Remote
Durable` in `dfsctl store block stats`.

**CLOSE/COMMIT semantics.** SMB CLOSE and NFS COMMIT (and the NFSv3 stable-WRITE
path) call the engine flush. A **hard** flush error (I/O fault, remote rejection,
metadata error) is **always** surfaced to the client regardless of the settings
below. Beyond that, whether CLOSE/COMMIT waits for durability is governed by a
per-share policy flag, `require_durable_commit` (default **false**):

| `require_durable_commit` | CLOSE/COMMIT behavior |
|--------------------------|-----------------------|
| `false` (default) | Acknowledge once the flush succeeds — **regardless** of durability. The local→remote mirror stays fully **asynchronous** and observable via the unsynced-bytes metric / `Pending Remote (bytes)`. Ordinary NFS/POSIX writes **never** EIO. |
| `true` (opt-in) | Acknowledge only when the data is on a **durable** store: `committed := localDurable \|\| (Finalized && remoteDurable)`. Trades latency for synchronous durability on non-fs-local stores. |

Set it per share via the local block store config:

```sh
dfsctl store block local edit <share> --config '{"require_durable_commit": true}'
```

A non-bool value is ignored with a startup warning (default `false` stands).

When `require_durable_commit = true`, the strict rule resolves as follows:

- **Production (local `fs`):** `localDurable=true`, so CLOSE/COMMIT ack
  immediately — there is **no wait** on the remote, and the mirror stays fully
  asynchronous. fs-local is always durable, so the flag is effectively a
  **no-op** there (the fast path is identical to the default).
- **Volatile local (`memory`) + durable remote (`s3`):** the data is only safe
  once it reaches the durable remote, so CLOSE/COMMIT succeeds only when the
  flush is `Finalized`. While the remote is unhealthy or a mirror pass is
  in-flight, CLOSE/COMMIT returns a transient I/O error (`NFS3ERR_IO` /
  `NFS4ERR_IO` / SMB `STATUS_UNEXPECTED_IO_ERROR`) and the client re-drives —
  the bytes remain in local CAS and the syncer keeps mirroring. (NFS *unstable*
  WRITE is unaffected — it still returns `UNSTABLE` and defers durability to a
  later COMMIT.)
- **Volatile local with no remote (or a non-durable remote):** the data is never
  durable, so CLOSE/COMMIT reports the same transient I/O error rather than
  silently acknowledging a write that a crash would lose.

In the **default** configuration none of the above transient errors occur —
CLOSE/COMMIT acks on a successful flush and the syncer mirrors in the
background. Use `require_durable_commit = true` only when you need synchronous
durability guarantees on a volatile-local + durable-remote share and can accept
the added latency.

`dfsctl store block stats` also shows `Pending Remote (bytes)` — the headline
data-at-risk gauge (local CAS bytes not yet mirrored to the remote) — which is
the way to observe the async mirror backlog under the default policy.

#### Chunk size — random-access shares (`chunk_size`)

DittoFS splits file data into content-defined (FastCDC) chunks. A chunk is the
unit of dedup, of the local cache, and — critically — of a **read fetch**: a
small random read pulls the whole chunk covering its offset. The default chunk
floor is ~1 MiB, so a 4 KiB random read into a large file amplifies to ~1 MiB
(~256×). That is fine for sequential and archival workloads but poor for random
I/O (VM images, databases).

`chunk_size` (bytes) lowers the FastCDC minimum for a share, shrinking the read
unit. Effective average chunk size ≈ `chunk_size`; a hard ceiling is derived
(8× `chunk_size`) unless you set `chunk_max` explicitly.

```sh
# Random-access share: ~128 KiB chunks (≈8× less read amplification)
dfsctl store block local edit <share> --config '{"chunk_size": 131072}'
```

| Setting | Effective avg | 4 KiB random-read amplification | Trade-off |
|---------|---------------|---------------------------------|-----------|
| default (unset) | ~1 MiB | ~256× | best dedup, fewest manifest rows |
| `65536` (64 KiB) | ~94 KiB | ~16× | ~16× more `FileChunk` rows; ~0 extra dedup loss on VM/DB |
| `131072` (128 KiB) | ~159 KiB | ~32× | ~8× more manifest rows |
| `262144` (256 KiB) | ~287 KiB | ~64× | ~4× more manifest rows |

Notes:

- **S3 object count is unchanged.** Chunks are packed into ~16 MiB block objects
  regardless of `chunk_size`, so smaller chunks do **not** create more/smaller
  S3 objects — only more `FileChunk` manifest rows. Writes/uploads keep their
  full throughput.
- **Write-time only.** Reads never re-chunk — the manifest records each chunk's
  boundaries — so changing `chunk_size` affects only newly written data, and old
  data stays readable. Dedup is not restored across a change (different
  boundaries → different hashes), but on VM/DB images dedup is already ~0.
- Invalid or below-floor (< 4 KiB) values are ignored with a startup warning and
  the default stands. Applies to `fs` local stores; `memory` local stores ignore
  it (in-RAM reads have no amplification).

#### GC knobs

The CAS write path uses an async syncer and a fail-closed mark-sweep
garbage collector. The syncer's sizing (claim timeout, etc.) is **not** an
operator-facing config section — it is auto-deduced from system resources at
startup and constructed in code; there is no `syncer:` config block (a stale
`syncer:` section in a config file is tolerated but ignored, logged as an
unknown key). Upload concurrency is **adaptive by default** — see below.

#### Adaptive upload concurrency

When you mirror a share to a remote (S3 or filesystem remote), DittoFS uploads
CAS chunks concurrently. Uploads are **network-latency bound**, not CPU bound:
a single PUT to a remote region sustains only a few MiB/s, so throughput scales
with the number of concurrent uploads until the uplink saturates. The right
number depends on the link, not the host — a CPU-derived default throttled fast
links and over-subscribed slow ones.

By default DittoFS **discovers the right concurrency itself**. It starts
conservative and ramps the number of in-flight uploads up while delivered
throughput (goodput) keeps rising, settling at the point where opening more
connections stops helping. It backs off only on upload errors or a real
throughput collapse — never on the latency rise that healthy concurrency itself
causes. No configuration is required to saturate the uplink.

To **pin** a fixed concurrency instead (disabling auto-tuning), set
`--parallel-uploads N` on the remote:

```bash
dfsctl store block remote add --name r1 --type s3 \
  --bucket … --region … --endpoint … \
  --parallel-uploads 32          # fixed window of 32; 0 (default) = adaptive
```

`dfsctl store block remote edit --name r1 --parallel-uploads 0` returns a remote
to adaptive mode. Observe the live window via the Prometheus gauge
`dittofs_datapath_upload_window` (target concurrency) alongside
`dittofs_datapath_uploads_inflight` (actual in-flight uploads); see
[Metrics](#metrics-prometheus).

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
  compaction_live_ratio: 0    # Reclaim dead bytes from partially-dead
                              # blocks. After each sweep, a block whose
                              # live bytes / object size is below this
                              # ratio is repacked (live chunks only) and
                              # the old block deleted. Must be in [0, 1];
                              # 0 (default) disables compaction. A value
                              # like 0.5 compacts a block once it is more
                              # than half dead.
  auto_enabled: true          # Run background GC automatically so you
                              # don't have to invoke the CLI. Default
                              # true. Set false to require manual
                              # `dfsctl store block gc`.
  auto_interval: 15m          # Period between background GC runs.
                              # Default 15m. Values in (0, 1m) are
                              # REJECTED. Ignored when auto_enabled is
                              # false.
```

**Tuning guidance:**

- Background GC is **on by default** (`auto_enabled: true`, every
  `auto_interval`) and reclaims orphaned blocks on **both** the local
  and remote tiers. Disable it (`auto_enabled: false`) only if you want
  to drive GC entirely on demand or via external scheduling.
- You can still run GC on demand at any time:
  `dfsctl store block gc <share>` (add `--dry-run` to preview, capped by
  `gc.dry_run_sample_size`; add `--reconcile` to also reap rows leaked by
  older versions).
- `gc.grace_period` MUST be longer than your worst-case
  metadata-commit latency after a successful PUT. The default 1h is
  comfortable for any commit path that completes in seconds.

Env-var mapping (dot-path convention; the top-level `gc` block binds
directly):
`DITTOFS_GC_GRACE_PERIOD`,
`DITTOFS_GC_DRY_RUN_SAMPLE_SIZE`,
`DITTOFS_GC_AUTO_ENABLED`,
`DITTOFS_GC_AUTO_INTERVAL`.

See [ARCHITECTURE.md](../internals/architecture.md#garbage-collection-mark-sweep)
for the full mark-sweep design and [CLI.md](cli.md) for the on-demand
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
  unaffected** — they keep their existing system-deduced local size and never
  apply remote-cache backpressure. The cap is enforced **lazily, on the
  write/rollup path** (it evicts synced blocks to make room for new writes); it
  is **not** a background reaper, so on an idle or read-only workload the
  resident local tier is not shrunk toward the cap. To reclaim local disk — or
  to force cold, remote-served reads for read-path benchmarking — evict on
  demand with `dfsctl store block evict` (drops the read buffer and drains
  resident synced blocks; never drops not-yet-uploaded data). Remote read-miss
  volume (the read-amplification signal) is observable via the
  `dittofs_datapath_block_range_read_bytes_total` metric.
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
                                             # Defaults to 10 GiB if unset.
    backpressure_max_wait: 60s               # Max time a write stalls for the
                                             # syncer to drain before disk-full.
    max_log_bytes: 2147483648                # Global append-log pressure budget
                                             # (see above). 0/unset = deduced.
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

See [CLI.md](cli.md#recycle-bin-trash) for the `dfsctl trash`
management commands and [ARCHITECTURE.md](../internals/architecture.md#metadataservice)
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
See [ENCRYPTION.md](encryption.md) for the full threat model and design.

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

#### S3-compatible backend presets

The `s3` remote store talks the AWS S3 API, so any S3-compatible object
store works — set a custom `endpoint` (and credentials) and DittoFS connects
to it instead of AWS. The store reads exactly these config keys (see
`pkg/block/remote/s3/store.go` and the factory in
`pkg/controlplane/runtime/shares/service.go`):

| Key | Required | Default | Notes |
| --- | --- | --- | --- |
| `bucket` | yes | — | Bucket name. Must already exist; DittoFS does not create it. |
| `access_key_id` | yes | — | S3 access key. For GCS use an **HMAC** key, not a service-account JSON. |
| `secret_access_key` | yes | — | S3 secret key. |
| `region` | no | `us-east-1` | Some providers ignore it but the SDK still requires a value; the default is sent when omitted. |
| `endpoint` | no (AWS) / yes (others) | AWS | Service URL. Scheme optional — `https://` is prepended when absent. |
| `force_path_style` | no | auto | **Auto-enabled whenever `endpoint` is set.** Set explicitly to `false` to opt back into virtual-hosted-style for providers that require it (e.g. GCS). |
| `prefix` | no | — | Key prefix prepended to every block (e.g. `dittofs/`). End it with `/`. |
| `allow_private_endpoint` | no | `false` | Required to point `endpoint` at a loopback or private-network address (MinIO, LocalStack, self-hosted RGW). See the SSRF note below. |

> Path-style addressing (`endpoint.example.com/bucket/key`) is the safe
> default for non-AWS providers because virtual-hosted style
> (`bucket.endpoint.example.com/key`) needs wildcard DNS and TLS SANs that
> most S3-compatible gateways do not provide. DittoFS therefore flips
> `force_path_style` on automatically the moment you set a custom `endpoint`;
> the only providers below that need it turned back **off** are those that
> require virtual-hosted style (GCS).

> **Private endpoints (MinIO, LocalStack, self-hosted RGW).** DittoFS rejects
> an `endpoint` that resolves to a loopback or private-network address
> (`127.0.0.0/8`, `10/8`, `172.16/12`, `192.168/16`, link-local, ULA) as an
> SSRF guard; the cloud metadata address (`169.254.169.254`) is always blocked.
> Self-hosted gateways normally live on exactly those networks, so add
> `"allow_private_endpoint": true` to their `--config` to permit them, as the
> MinIO and Ceph recipes below do.

Credentials live in the store's own config (the `--config` blob below, or the
equivalent `--access-key` / `--secret-key` flags) — they are not read from the
`DITTOFS_*` server-config environment. Each recipe is a
`dfsctl store block remote add` invocation; attach the resulting store to a
share with `dfsctl share create … --remote <name>`.

##### Verified providers

These run against an emulator (or a documented public endpoint) and are
exercised by the e2e suite where an emulator exists:

| Provider | Verified by | Endpoint | Region | `force_path_style` | Notes |
| --- | --- | --- | --- | --- | --- |
| AWS S3 | unit + prod use | _(omit — SDK default)_ | your bucket region | unset (virtual-hosted) | The native case; no `endpoint`. |
| MinIO | **e2e emulator** | `http://minio.example:9000` | `us-east-1` | auto-on | Self-hosted; HTTP fine on a trusted network. |
| LocalStack | **e2e emulator** | `http://localstack:4566` | `us-east-1` | auto-on | Test/CI only; not a production target. |
| Ceph RGW | documented | `https://rgw.example:7480` | `us-east-1` | auto-on | RGW ignores region; any non-empty value is accepted. |

```bash
# MinIO  (verified by e2e emulator)
dfsctl store block remote add --name minio-store --type s3 \
  --config '{"endpoint":"http://minio.example:9000","bucket":"dittofs","region":"us-east-1","access_key_id":"minioadmin","secret_access_key":"minioadmin","allow_private_endpoint":true}'

# LocalStack  (verified by e2e emulator)
dfsctl store block remote add --name localstack-store --type s3 \
  --config '{"endpoint":"http://localstack:4566","bucket":"dittofs","region":"us-east-1","access_key_id":"test","secret_access_key":"test","allow_private_endpoint":true}'

# Ceph RGW (RADOS Gateway)
dfsctl store block remote add --name ceph-store --type s3 \
  --config '{"endpoint":"https://rgw.example:7480","bucket":"dittofs","region":"us-east-1","access_key_id":"ACCESS","secret_access_key":"SECRET","allow_private_endpoint":true}'
```

##### Documented-only providers

These are configured exactly like the verified ones but have **not** been run
against a live account in CI — they are documented from each provider's S3
compatibility guide. The per-provider column flags the one gotcha that bites.

| Provider | Endpoint | Region | `force_path_style` | Gotcha |
| --- | --- | --- | --- | --- |
| **Cubbit DS3** ⭐ _(DittoFS sponsor)_ | `https://s3.cubbit.eu` | `eu-west-1` | auto-on | Geo-distributed, S3-compatible object storage from [Cubbit](https://www.cubbit.io/). Create an S3 access key/secret in the DS3 console; the bucket lives in your assigned region. |
| Google Cloud Storage (XML/HMAC) | `https://storage.googleapis.com` | `us-east-1` | **set `false`** | Use an **HMAC** key (`access_key_id`/`secret_access_key`), not a service-account JSON. GCS ignores `region` (any non-empty value works), so send the `us-east-1` default. GCS wants virtual-hosted style, so override the auto path-style default to `false`. |
| Backblaze B2 | `https://s3.us-west-004.backblazeb2.com` | `us-west-004` | auto-on | Endpoint embeds the region (`s3.<region>.backblazeb2.com`); `region` must match it. Use an **application key**, not the master key. |
| Wasabi | `https://s3.us-east-1.wasabisys.com` | `us-east-1` | auto-on | Region is in the hostname; mismatched `region` causes auth failures. |
| DigitalOcean Spaces | `https://nyc3.digitaloceanspaces.com` | `us-east-1` | auto-on | Endpoint is the datacenter (`<region>.digitaloceanspaces.com`); send `region: us-east-1` (Spaces ignores it but the SDK requires a value). |
| Alibaba Cloud OSS | `https://oss-us-west-1.aliyuncs.com` | `us-west-1` | auto-on | Region is encoded in the endpoint host; use the matching OSS region. |
| Tencent Cloud COS | `https://cos.ap-guangzhou.myqcloud.com` | `ap-guangzhou` | auto-on | Bucket name must include the AppID suffix (`name-1250000000`); endpoint carries the region. |
| Oracle Cloud (OCI) Object Storage | `https://<namespace>.compat.objectstorage.us-ashburn-1.oraclecloud.com` | `us-ashburn-1` | auto-on | Endpoint contains your tenancy **namespace**; generate a **Customer Secret Key** for S3 compat. |
| Storj (S3 gateway) | `https://gateway.storjshare.io` | `us-east-1` | auto-on | Use S3-gateway access keys (`uplink share --register`), not the API access grant. |

```bash
# Cubbit DS3 (DittoFS sponsor) — geo-distributed, S3-compatible object storage
dfsctl store block remote add --name ds3-store --type s3 \
  --config '{"endpoint":"https://s3.cubbit.eu","bucket":"dittofs","region":"eu-west-1","access_key_id":"DS3_ACCESS_KEY","secret_access_key":"DS3_SECRET_KEY"}'

# Google Cloud Storage — note force_path_style:false (GCS wants virtual-hosted)
dfsctl store block remote add --name gcs-store --type s3 \
  --config '{"endpoint":"https://storage.googleapis.com","bucket":"dittofs","region":"us-east-1","access_key_id":"GOOG_HMAC_KEY","secret_access_key":"GOOG_HMAC_SECRET","force_path_style":false}'

# Backblaze B2 — region is baked into the endpoint host
dfsctl store block remote add --name b2-store --type s3 \
  --config '{"endpoint":"https://s3.us-west-004.backblazeb2.com","bucket":"dittofs","region":"us-west-004","access_key_id":"B2_KEY_ID","secret_access_key":"B2_APP_KEY"}'

# Wasabi
dfsctl store block remote add --name wasabi-store --type s3 \
  --config '{"endpoint":"https://s3.us-east-1.wasabisys.com","bucket":"dittofs","region":"us-east-1","access_key_id":"ACCESS","secret_access_key":"SECRET"}'

# DigitalOcean Spaces
dfsctl store block remote add --name spaces-store --type s3 \
  --config '{"endpoint":"https://nyc3.digitaloceanspaces.com","bucket":"dittofs","region":"us-east-1","access_key_id":"SPACES_KEY","secret_access_key":"SPACES_SECRET"}'

# Alibaba Cloud OSS
dfsctl store block remote add --name oss-store --type s3 \
  --config '{"endpoint":"https://oss-us-west-1.aliyuncs.com","bucket":"dittofs","region":"us-west-1","access_key_id":"ACCESS","secret_access_key":"SECRET"}'

# Tencent Cloud COS — bucket name carries the AppID suffix
dfsctl store block remote add --name cos-store --type s3 \
  --config '{"endpoint":"https://cos.ap-guangzhou.myqcloud.com","bucket":"dittofs-1250000000","region":"ap-guangzhou","access_key_id":"SECRET_ID","secret_access_key":"SECRET_KEY"}'

# Oracle Cloud (OCI) Object Storage — endpoint embeds your namespace
dfsctl store block remote add --name oci-store --type s3 \
  --config '{"endpoint":"https://my-namespace.compat.objectstorage.us-ashburn-1.oraclecloud.com","bucket":"dittofs","region":"us-ashburn-1","access_key_id":"OCI_ACCESS","secret_access_key":"OCI_SECRET"}'

# Storj (S3-compatible gateway)
dfsctl store block remote add --name storj-store --type s3 \
  --config '{"endpoint":"https://gateway.storjshare.io","bucket":"dittofs","region":"us-east-1","access_key_id":"STORJ_ACCESS","secret_access_key":"STORJ_SECRET"}'
```

All of the above accept the same optional knobs as AWS S3 —
`prefix`, `compression`, `encryption`, and `durable` (see the preceding
subsections) — because they share the single `s3` store implementation.

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

#### BadgerDB cache sizing (config file)

The BadgerDB metadata engine keeps two in-memory caches that dominate read
performance under concurrent NFS/SMB load:

- the **block cache** — decompressed LSM-tree data blocks, and
- the **index cache** — the block-offset indices used to locate keys.

Badger's own defaults are tiny (256 MiB block, index cache disabled), which
thrashes on a busy server over a large directory tree. The symptom in the logs
is `Block cache might be too small ... hit-ratio: 0.26 ... sets-rejected`; every
cold lookup then walks the LSM tree from disk, which also widens the window for
the dedup transaction-conflict race and the append-log "pressure wait timed out"
stall.

By default both sizes **auto-scale with the memory available to the process**,
so no tuning is required:

| Cache | Fraction of available RAM | Floor   | Ceiling |
|-------|---------------------------|---------|---------|
| block | 15 %                      | 512 MiB | 4 GiB   |
| index | 7.5 %                     | 256 MiB | 2 GiB   |

The fractions are deliberately conservative because the same process also holds
the append-log, the metadata working set, and read buffers. The available-memory
figure is the cgroup limit inside a container, or physical RAM otherwise (same
detection used for block-store sizing). Examples:

- **4 GiB host** → ~614 MiB block / ~307 MiB index (the floors don't bind).
- **2 GiB host / 2 GiB cgroup** → 512 MiB block / 256 MiB index (floors bind).
- **64 GiB host** → 4 GiB block / 2 GiB index (ceilings bind).

To override the auto-sizing, set the sizes explicitly (in MiB) in the top-level
`metadata.badger` block. Setting one dimension still auto-sizes the other:

```yaml
metadata:
  badger:
    block_cache_mb: 2048   # 0 (default) = auto-size from available RAM
    index_cache_mb: 1024   # 0 (default) = auto-size from available RAM
```

Environment overrides: `DITTOFS_METADATA_BADGER_BLOCK_CACHE_MB`,
`DITTOFS_METADATA_BADGER_INDEX_CACHE_MB`.

**Recommended sizing vs. object/metadata count.** As a rule of thumb the block
cache should hold the hot directory/inode working set. Each cached file/inode is
on the order of a few hundred bytes of decompressed LSM data, so:

| Hot metadata objects | Suggested `block_cache_mb` | Suggested `index_cache_mb` |
|----------------------|----------------------------|----------------------------|
| up to ~1 M           | auto (≥512)                | auto (≥256)                |
| ~1–10 M              | 1024–2048                  | 512–1024                   |
| ~10–50 M             | 2048–4096                  | 1024–2048                  |
| > 50 M               | 4096+ (raise host RAM)     | 2048+                      |

If you still see `hit-ratio` below ~0.8 or `sets-rejected` in the Badger logs,
the cache is undersized for the working set — raise `block_cache_mb` first.

These global sizes apply to every BadgerDB metadata store on the node. A single
store can be overridden via its config-map keys when it is created (see below):
`--config '{"path":"...","block_cache_mb":2048,"index_cache_mb":1024}'`.

#### Metadata Store Instances (CLI)

Metadata stores are managed at runtime via `dfsctl` and persisted in the control plane database:

```bash
# In-memory metadata for fast temporary workloads
./dfsctl store metadata add --name memory-fast --type memory

# BadgerDB for persistent metadata
./dfsctl store metadata add --name badger-main --type badger \
  --config '{"path":"/tmp/dittofs-metadata-main"}'

# BadgerDB with explicit per-store cache sizes (MiB). Omit either key (or set 0)
# to auto-size that cache from available RAM. See "BadgerDB cache sizing" above.
./dfsctl store metadata add --name badger-big --type badger \
  --config '{"path":"/tmp/dittofs-metadata-big","block_cache_mb":2048,"index_cache_mb":1024}'

# Separate BadgerDB instance for isolated shares
./dfsctl store metadata add --name badger-isolated --type badger \
  --config '{"path":"/tmp/dittofs-metadata-isolated"}'

# SQLite for a persistent single-binary / edge appliance (pure-Go, no cgo).
# Reuses the PostgreSQL data model (parent_child_map hard links, nlink,
# recursive-CTE path reconstruction, object_id dedup index).
./dfsctl store metadata add --name sqlite-edge --type sqlite \
  --config '{"path":"/var/lib/dittofs/metadata.db"}'

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
./dfsctl share remove /fast
```

**Configuration Patterns:**

- **Shared Metadata**: `/cloud` and `/archive` both use `badger-main` - they share the same metadata database
- **Performance Tiering**: Different shares use different storage backends (memory, local disk, S3)
- **Isolation**: Each share gets its own BlockStore with isolated local storage directory
- **Resource Efficiency**: Remote stores are shared (ref counted) when multiple shares reference the same config
- **Flexible Topologies**: Mix local-only and remote-backed storage per-share

#### Per-share and per-identity quotas

DittoFS supports two complementary quota layers, both enforced by NFS *and* SMB:

1. **Per-share quota** (`dfsctl share create/edit --quota-bytes`) — a single byte
   ceiling for the whole share. Exceeding it returns `NFS3ERR_NOSPC` /
   `STATUS_DISK_FULL`.

2. **Per-identity quotas** (`dfsctl quota …`) — per-**user** (uid) and per-**group**
   (gid) limits, plus an optional **default-user** fallback applied to any user
   without an explicit quota. Each quota bounds both **bytes** and **inodes**
   (file count) and supports a **soft** threshold with a **grace period** before
   the soft threshold is enforced as a hard limit. Usage is charged to the file
   *owner* (standard quota semantics). Exceeding a hard limit (or an expired
   soft+grace) returns `NFS3ERR_DQUOT` / `NFS4ERR_DQUOT` /
   `STATUS_QUOTA_EXCEEDED`. `df` / FSSTAT and SMB `FS_FULL_SIZE_INFORMATION`
   report the smallest applicable quota for the calling identity.

```bash
# Per-user quota: uid 1000 limited to 10 GiB / 100k files, soft at 8 GiB,
# 7-day grace (604800s) before the soft byte threshold becomes hard.
./dfsctl quota set /cloud --scope user --id 1000 \
    --limit-bytes 10GiB --soft-bytes 8GiB \
    --limit-files 100000 --soft-files 90000 --grace-seconds 604800

# Per-group quota: gid 2000 limited to 50 GiB.
./dfsctl quota set /cloud --scope group --id 2000 --limit-bytes 50GiB

# Default-user fallback (applies to any user without an explicit quota).
./dfsctl quota set /cloud --scope default-user --limit-bytes 5GiB

# Inspect and remove.
./dfsctl quota list /cloud
./dfsctl quota remove /cloud --scope user --id 1000
```

Per-identity quota usage is tracked incrementally by every metadata backend
(memory / badger / postgres), keyed by owner uid and gid, and is reconstructed
from the file rows on startup. A `chown` that changes a file's owner moves its
bytes and inode count between identities. Limits live in the control-plane DB
and are also manageable via the REST API
(`/api/v1/shares/{name}/quotas[/{scope}/{id}]`).

The **soft → grace → hard** state machine records when an identity first crosses
its soft threshold and enforces the soft limit as hard once the grace window
elapses. For an explicit user/group quota the grace timer lives on the quota
row. For the **default-user** fallback the timer is inherently per-user (each
user trips soft at a different time, and the single shared template row cannot
hold per-user state), so it is recorded in a small side table keyed by
`(share, uid)`, written the first time a default-user breaches soft and reaped
when usage drops back under soft. This makes default-user grace **durable across
a server restart** — a restart no longer hands every over-soft default user a
fresh grace window.

> **Note**: enforcement is best-effort (matching the per-share soft quota):
> under high write concurrency a few operations may briefly exceed a limit until
> usage catches up. This is standard for userspace NFS/SMB servers.
>
> **Kubernetes operator**: the operator manages infrastructure only — there is
> no Share/Quota CRD. Quotas are managed via the REST API / `dfsctl` as above.

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
`dfsctl` against a running server (run `dfsctl login` first). See [CLI.md](cli.md) for the
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
dfsctl user remove alice

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
dfsctl group remove editors
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
    port: 12049
    max_connections: 0           # 0 falls back to 1024 (default cap)

    # Grouped timeout configuration
    timeouts:
      read: 5m                   # Max time to read request
      write: 30s                 # Max time to write response
      idle: 5m                   # Max idle time between requests
      shutdown: 30s              # Graceful shutdown timeout

    metrics_log_interval: 5m     # Metrics logging interval (0 = disabled)

    # Embedded portmapper (RFC 1057) for service discovery. Disabled by default.
    # macOS/BSD NFSv3 lock clients query the portmapper on port 111 — bind it
    # there (requires root / CAP_NET_BIND_SERVICE) for NFSv3 locking to work.
    portmapper:
      enabled: false             # Default: false
      port: 10111                # Default: 10111 (set to 111 for macOS locking)

    # UDP transport for the lock-manager protocols. Serves NLM/NSM/MOUNT over
    # UDP (in addition to TCP) on the NFS port. Required for NFSv3 file locking
    # from BSD/macOS clients (see docs/NFS.md). Disabled by default. NFS data
    # operations are never served over UDP.
    udp:
      enabled: false             # Default: false

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
      encryption_mode: preferred  # disabled | preferred | required (default: preferred)

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
| `preferred` | Encrypt 3.x sessions; allow unencrypted 2.x | Mixed environments (**default**) |
| `required` | Reject 2.x clients; encrypt all 3.x sessions | High-security environments |

> **Secure default:** As of v1.0.0 the shipped default is `preferred`, so SMB 3.x
> sessions are encrypted out of the box while remaining wire-compatible with SMB 2.x
> clients. Set `encryption_mode: required` to mandate encryption for sensitive
> deployments (this rejects SMB 2.x clients, which cannot encrypt). If SMB is bound to
> a non-loopback address with `encryption_mode: disabled`, `dfs` logs a startup WARN
> because file data then traverses the network in cleartext. See
> [docs/SECURITY.md](security.md#smb3-security-model) for the hardened template.

**Enforcement Rules:**

1. **SESSION_SETUP**: When mode is `preferred` or `required`, AEAD encryption keys are derived for SMB 3.x sessions. In `required` mode the `SMB2_SESSION_FLAG_ENCRYPT_DATA` flag is set in the response and every subsequent message on the session must be encrypted. In `preferred` mode the keys are available for per-share enforcement, but the session flag is **not** set — message encryption is only forced on trees connected to shares with `encrypt_data=true`.
2. **Per-share (`encrypt_data=true`)**: encryption is forced on that tree **regardless of the global mode**. In `required` mode the unencrypted session is rejected at TREE_CONNECT. In `preferred` mode TREE_CONNECT succeeds, but every subsequent unencrypted request on the tree is denied with `STATUS_ACCESS_DENIED` (enforced by `checkEncryptionRequired`) — so plaintext access to an `encrypt_data` share is never actually allowed. The global mode only governs sessions to non-`encrypt_data` shares (mixed model in `preferred`).
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

#### Network discovery (mDNS / WS-Discovery)

DittoFS can advertise itself on the LAN so it appears in **macOS Finder →
Network**, Linux file managers (via Avahi), and the **Windows Explorer →
Network** view — the same job the external `avahi-daemon` and `wsdd`/`wsdd2`
daemons do for Samba, but built in-process. Discovery is **off by default** and
managed through the live adapter settings (`dfsctl` / REST), so it applies
immediately without an adapter restart:

```bash
# mDNS / DNS-SD — macOS Finder + Linux Avahi
dfsctl adapter settings nfs update --mdns-enabled          # advertises _nfs._tcp (port 12049)
dfsctl adapter settings smb update --mdns-enabled          # advertises _smb._tcp + _device-info._tcp

# WS-Discovery — Windows Explorer Network (SMB only; Windows does not browse NFS)
dfsctl adapter settings smb update --wsdiscovery-enabled
```

This opens additional listeners: mDNS on UDP `5353`, and — for WS-Discovery —
UDP `3702` plus an HTTP metadata endpoint on TCP `5357`. NFS advertises the
first export's path in a `path=` TXT record so Finder mounts the right share.

**Advertised name.** All advertisers share one instance-wide name, so a server
shows up consistently across Finder and Explorer. It defaults to
`DittoFS-<hostname>` (e.g. `DittoFS-VM2`) — distinct per host so several DittoFS
servers on one LAN stay distinguishable — and is overridable:

```bash
dfsctl settings set discovery.name "Marketing Files"   # custom instance name
dfsctl settings set discovery.name ""                  # revert to DittoFS-<hostname>
```

Each adapter formats the name for its own protocol: mDNS uses it verbatim, while
WS-Discovery folds it to a NetBIOS-legal computer name (upper-cased, illegal
characters replaced with `-`, capped at 15 characters) since Explorer renders it
as a Windows computer name. A name change takes effect the next time an
advertiser (re)starts — toggle discovery off/on, or restart the adapter.

> **Note:** discovery is multicast-based and LAN-local. It works on a host
> network (bare metal, VM, or a `hostNetwork` pod) but does **not** traverse
> standard Kubernetes / overlay networks — Explorer and Finder will only see the
> server on the same L2 segment. Mounting by name/IP always works regardless.
> WS-Discovery makes the machine *appear* in Explorer; Windows still connects to
> SMB on port `445`, so the SMB adapter must be reachable there.

> **Windows host firewall:** when DittoFS runs *on* a Windows host, the built-in
> "Network Discovery" firewall rules only cover Windows' own services (they are
> scoped to `System` / `svchost`), so inbound traffic to `dfs.exe` is dropped by
> default. Explorer then discovers the server over multicast but silently fails
> the follow-up metadata fetch (a TCP `5357` connection that never completes),
> and the machine never renders. Add inbound allow rules for the `dfs.exe`
> program on TCP `5357` and UDP `3702`/`5353`:
>
> ```powershell
> New-NetFirewallRule -DisplayName "DittoFS Discovery (WSD meta)"  -Direction Inbound -Action Allow -Protocol TCP -LocalPort 5357 -Program "C:\path\to\dfs.exe" -Profile Any
> New-NetFirewallRule -DisplayName "DittoFS Discovery (WSD probe)" -Direction Inbound -Action Allow -Protocol UDP -LocalPort 3702 -Program "C:\path\to\dfs.exe" -Profile Any
> New-NetFirewallRule -DisplayName "DittoFS Discovery (mDNS)"      -Direction Inbound -Action Allow -Protocol UDP -LocalPort 5353 -Program "C:\path\to\dfs.exe" -Profile Any
> ```
>
> This is a Windows-host concern only; a Linux DittoFS host needs no equivalent
> (any host firewall there just needs the same ports open).

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

`kerberos` is a top-level config block (it backs both NFS RPCSEC_GSS and SMB
SPNEGO — not nested under `adapters`):

```yaml
# Kerberos (RPCSEC_GSS + SMB SPNEGO) settings — top-level block
kerberos:
  enabled: true
  keytab_path: /etc/dittofs/dittofs.keytab
  service_principal: nfs/server.example.com@EXAMPLE.COM
  krb5_conf: /etc/krb5.conf

  # AD domain identity (optional; for an Active-Directory-joined server).
  # When all three are unset the server is standalone and advertises the
  # NetBIOS workgroup "WORKGROUP" exactly as a non-domain server does —
  # leaving these empty is fully backward-compatible.
  realm: EXAMPLE.COM            # Kerberos realm; defaults to the @REALM of service_principal
  netbios_domain: EXAMPLE       # NetBIOS short name; NOT derivable, must be set to enable domain-aware SMB
  dns_domain: example.com       # defaults to the lowercased realm
```

| Key | Env var | Default |
|---|---|---|
| `kerberos.realm` | `DITTOFS_KERBEROS_REALM` | `@REALM` of `service_principal` |
| `kerberos.netbios_domain` | `DITTOFS_KERBEROS_NETBIOS_DOMAIN` | (empty → standalone `WORKGROUP`) |
| `kerberos.dns_domain` | `DITTOFS_KERBEROS_DNS_DOMAIN` | lowercased `realm` |

#### Domain-aware SMB

When `netbios_domain` is set, the SMB server advertises the AD domain in the
NTLM challenge (`MsvAvNbDomainName` / `MsvAvDnsDomainName`) and stamps it on
authenticated sessions, so domain users authenticate against the correct domain.
Unset → the server advertises `WORKGROUP` / `local` (pre-AD-4 standalone
behavior).

#### Offline keytab import (one keytab, both protocols)

A keytab can hold multiple service principals, so a single file serves SMB and
NFS. Pre-create the computer/service account in AD and export a keytab
containing **both** `cifs/<host>@REALM` (SMB) and `nfs/<host>@REALM` (NFS):

```bash
# On a domain-joined admin host (samba-tool / adcli / Windows ktpass):
samba-tool domain exportkeytab /etc/dittofs/dittofs.keytab \
    --principal=cifs/server.example.com@EXAMPLE.COM
samba-tool domain exportkeytab /etc/dittofs/dittofs.keytab \
    --principal=nfs/server.example.com@EXAMPLE.COM
```

Point `kerberos.keytab_path` at the combined keytab. The SMB handler selects
the `cifs/` principal (deriving it from the NFS `service_principal`, or via an
explicit override); NFS RPCSEC_GSS uses the `nfs/` principal.

#### NTLM pass-through for AD domain users (NETLOGON machine account)

The keytab above authenticates AD users over **Kerberos** (mounting by the SPN
FQDN, e.g. `\\server.example.com\share`). But when a client connects by a name
that has **no Kerberos SPN** — an IP address, or the LAN-discovery name a user
gets by **double-clicking the server in Explorer → Network** (§10) — Windows
falls back to **NTLM**, which the KDC never sees. To let AD domain users
authenticate on that path, DittoFS validates their NTLM response against a
Domain Controller via **NETLOGON pass-through** (MS-NRPC `NetrLogonSamLogon`).

This is **opt-in** and requires a dedicated **machine (computer) account** —
distinct from the `cifs/` service account in the keytab, because NETLOGON needs
a *workstation-trust* secure channel that only a machine account can establish.
The secure channel rides a Kerberos-authenticated SMB session to the DC's
`\PIPE\netlogon` (reusing the same `krb5_conf` / realm as above). On success the
DC returns the user's SID **and group SIDs**, which resolve to a UID/GID through
the directory idmap (§13 / the LDAP identity provider, or `idmap_rid`) and are
matched against share grants — the **same SID-based authorization** as the
Kerberos/PAC path, so a domain user or **group** (e.g. `Domain Admins`) granted
on a share is authorized identically over NTLM.

```yaml
kerberos:
  # ... realm / netbios_domain / keytab_path as above ...

  machine_account:
    enabled: true                 # opt-in; false (default) => no NTLM pass-through
    account_name: "DITTOFS$"      # the machine account sAMAccountName (trailing '$')
    dc_address:                   # optional; empty => discover the DC via DNS SRV
      - "10.0.0.10"

    # Provisioning — pick ONE:

    # (A) OFFLINE: you pre-create the computer account and give DittoFS its
    #     password. Nothing is written to AD at runtime.
    secret: "the-machine-account-password"

    # (B) ONLINE JOIN: DittoFS creates the computer object itself over LDAPS on
    #     first domain logon, owns the password, and rotates it. Requires a
    #     privileged bind account that can create computer objects. Omit
    #     `secret` when using this. LDAPS (or ldap:// + start_tls) is mandatory —
    #     AD refuses a machine-password write over an unencrypted connection.
    online_join:
      enabled: true
      ldap_url: "ldaps://dc.example.com"
      bind_dn: "CN=Administrator,CN=Users,DC=example,DC=com"
      bind_password: "..."
      base_dn: "DC=example,DC=com"
      # ou: "OU=Servers,DC=example,DC=com"   # default: CN=Computers,<base_dn>
      rotation_interval: 168h                 # 0 disables rotation (AD max age ~30d)
      # ca_cert_file: /etc/dittofs/dc-ca.pem  # pin the DC cert (recommended)
      # insecure_skip_verify: false           # lab only; exposes the password to MITM
```

| Key | Env var | Default |
|---|---|---|
| `kerberos.machine_account.enabled` | `DITTOFS_KERBEROS_MACHINE_ACCOUNT_ENABLED` | `false` |
| `kerberos.machine_account.account_name` | `DITTOFS_KERBEROS_MACHINE_ACCOUNT_ACCOUNT_NAME` | (empty) |
| `kerberos.machine_account.secret` | `DITTOFS_KERBEROS_MACHINE_ACCOUNT_SECRET` | (empty; offline path) |
| `kerberos.machine_account.dc_address` | `DITTOFS_KERBEROS_MACHINE_ACCOUNT_DC_ADDRESS` | (empty → DNS SRV discovery) |
| `kerberos.machine_account.online_join.enabled` | `DITTOFS_KERBEROS_MACHINE_ACCOUNT_ONLINE_JOIN_ENABLED` | `false` |
| `kerberos.machine_account.online_join.ldap_url` | `DITTOFS_KERBEROS_MACHINE_ACCOUNT_ONLINE_JOIN_LDAP_URL` | (empty) |

`realm` and `netbios_domain` are **required** for pass-through (the secure
channel and NTLM `TargetInfo` both need them). The `secret` / `bind_password`
are redacted in `dfs config show`. For a full walkthrough — pre-creating the
`DITTOFS$` account and testing the Explorer double-click — see
[docs/guide/windows-ad-setup.md](windows-ad-setup.md).

### 13. Identity Mapping Configuration

```yaml
identity:
  # Pin this node's machine SID (Windows S-1-5-21-{a}-{b}-{c}).
  #
  # When unset, the machine SID is generated once on first boot and
  # persisted, staying stable across restarts. Local/algorithmic SIDs are
  # derived purely from the machine SID + the Samba RID formula
  # (user RID = uid*2+1000, group RID = gid*2+1001), so pinning the SAME
  # value on every node in a cluster makes them compute IDENTICAL SIDs for
  # the same Unix UID/GID — required for cross-node identity parity.
  # Foreign (Active Directory / LDAP) domain SIDs are NOT derived this way;
  # they are bound to stable UID/GIDs durably in the control-plane store.
  #
  # Env override: DITTOFS_IDENTITY_MACHINE_SID
  machine_sid: "S-1-5-21-1111111111-2222222222-3333333333"

  # Identity mapping for NFSv4
  idmap:
    domain: example.com
    # Static mappings
    mappings:
      - nfs_name: "user@EXAMPLE.COM"
        local_uid: 1000
        local_gid: 1000
```

### 14. Snapshot Scheduler

Controls the background scheduler that drives per-share snapshot policies
(schedule + retention). Policies themselves are configured per share via
`dfsctl share snapshot-policy` or the REST API — see
[SNAPSHOTS.md §12](snapshots.md#12-scheduled-snapshots-policies). These
knobs only tune the daemon-wide scheduler loop.

```yaml
snapshot:
  # How often the daemon scans for due policies. The per-share policy
  # interval (not this knob) controls how often a share is snapshotted.
  scheduler_poll_interval: 1m   # default 1m
  # Turn the scheduler off entirely. Policies are still stored and can be
  # run manually with `dfsctl share snapshot-policy run`.
  scheduler_disabled: false     # default false
  # Per-request budget for the synchronous restore endpoint.
  restore_http_timeout: 30m     # default 30m
```

| Key | Env var | Default |
|---|---|---|
| `snapshot.scheduler_poll_interval` | `DITTOFS_SNAPSHOT_SCHEDULER_POLL_INTERVAL` | `1m` |
| `snapshot.scheduler_disabled` | `DITTOFS_SNAPSHOT_SCHEDULER_DISABLED` | `false` |
| `snapshot.restore_http_timeout` | `DITTOFS_SNAPSHOT_RESTORE_HTTP_TIMEOUT` | `30m` |

### 15. LDAP / Active Directory Identity Provider

Resolves directory principals (a `user@REALM` form or an AD SID) to a Unix
identity by querying LDAP/AD. It reads the **RFC2307** `uidNumber`/`gidNumber`
POSIX attributes (the `idmap_ad` model) or falls back to **RID**-based
derivation (`idmap_rid`), and resolves the user's group memberships — including
nested AD groups via the `LDAP_MATCHING_RULE_IN_CHAIN` matching rule. The
provider is registered in the identity-resolution chain after Kerberos, so an AD
principal/SID with no local user mapping is resolved against the directory.

**Security:** the connection is encrypted by default. A plaintext `ldap://`
connection is **refused** unless it is upgraded with `start_tls: true` or the
operator explicitly opts in with `allow_plaintext: true`. Prefer `ldaps://`.

```yaml
ldap:
  enabled: true
  url: ldaps://dc.example.com:636        # ldaps:// (preferred) or ldap:// + start_tls
  start_tls: false                       # upgrade an ldap:// connection to TLS
  allow_plaintext: false                 # explicit opt-in for an unencrypted bind (off)
  base_dn: "DC=example,DC=com"
  bind_dn: "CN=svc-dittofs,CN=Users,DC=example,DC=com"
  bind_password: "********"              # service-account password (redacted on show)
  user_attr: sAMAccountName              # attribute matched against the bare username
  realm: EXAMPLE.COM                     # matches "user@REALM" credentials
  idmap: rfc2307                          # "rfc2307" (uidNumber/gidNumber) or "rid"
  nested_groups: true                     # resolve transitive AD group membership
  max_group_results: 200                  # cap on (nested) groups resolved per user
  timeout: 10s
  tls:
    ca_cert_file: /etc/dittofs/ad-ca.pem  # CA bundle to verify the directory cert
    client_cert_file: ""                  # optional mutual-TLS client cert
    client_key_file: ""
    insecure_skip_verify: false           # lab-only escape hatch (off)
    min_version: "1.2"                    # "1.2" or "1.3"
```

| Key | Env var | Default |
|---|---|---|
| `ldap.enabled` | `DITTOFS_LDAP_ENABLED` | `false` |
| `ldap.url` | `DITTOFS_LDAP_URL` | (required when enabled) |
| `ldap.start_tls` | `DITTOFS_LDAP_START_TLS` | `false` |
| `ldap.allow_plaintext` | `DITTOFS_LDAP_ALLOW_PLAINTEXT` | `false` |
| `ldap.base_dn` | `DITTOFS_LDAP_BASE_DN` | (required when enabled) |
| `ldap.bind_dn` | `DITTOFS_LDAP_BIND_DN` | (required when enabled) |
| `ldap.bind_password` | `DITTOFS_LDAP_BIND_PASSWORD` | (required when enabled; empty triggers an anonymous bind and is rejected) |
| `ldap.user_attr` | `DITTOFS_LDAP_USER_ATTR` | `sAMAccountName` |
| `ldap.realm` | `DITTOFS_LDAP_REALM` | (empty) |
| `ldap.idmap` | `DITTOFS_LDAP_IDMAP` | `rfc2307` |
| `ldap.nested_groups` | `DITTOFS_LDAP_NESTED_GROUPS` | `false` |
| `ldap.max_group_results` | `DITTOFS_LDAP_MAX_GROUP_RESULTS` | `200` |
| `ldap.timeout` | `DITTOFS_LDAP_TIMEOUT` | `10s` |
| `ldap.tls.ca_cert_file` | `DITTOFS_LDAP_TLS_CA_CERT_FILE` | (system roots) |
| `ldap.tls.client_cert_file` | `DITTOFS_LDAP_TLS_CLIENT_CERT_FILE` | (empty) |
| `ldap.tls.client_key_file` | `DITTOFS_LDAP_TLS_CLIENT_KEY_FILE` | (empty) |
| `ldap.tls.insecure_skip_verify` | `DITTOFS_LDAP_TLS_INSECURE_SKIP_VERIFY` | `false` |
| `ldap.tls.min_version` | `DITTOFS_LDAP_TLS_MIN_VERSION` | `1.2` |

Admin-configured identity links (`dfsctl idmap add`) take precedence over the
directory query: a link for an `ldap` external ID resolves to the mapped local
user before any LDAP search is issued.

**Samba AD-DC self-signed certificates.** A default Samba AD-DC serves an
auto-generated TLS certificate that commonly has a *negative serial number*.
Go's `crypto/x509` rejects such certificates at parse time — before TLS
verification runs — so `ldap.tls.insecure_skip_verify` does **not** bypass it.
The `dfs` binary is built with `x509negativeserial=1` so `ldaps://` against a
default Samba AD-DC works out of the box. For production directories, prefer a
properly-issued DC certificate (the Go toggle is slated for removal in a future
release).

**Managing identity providers over the API (no restart).** The `ldap.*` and
`kerberos.*` keys above seed the configuration on first boot. After that, both
providers can be read, updated, and tested over the control-plane API without
editing files:

| Method & path | Purpose |
|---|---|
| `GET /api/v1/identity-providers` | List providers + enabled state (no secrets). |
| `GET /api/v1/identity-providers/{type}/config` | Read config (bind password redacted to `********`). |
| `PUT /api/v1/identity-providers/{type}/config` | Create/replace config (validated). |
| `POST /api/v1/identity-providers/{type}/test` | Dry-run dial+bind (LDAP) / keytab check (Kerberos); never persists. |

`{type}` is `ldap` or `kerberos`; all routes are admin-only. A persisted config
**takes precedence over the file/env config** on subsequent boots. **LDAP
changes hot-reload the live resolver**; **Kerberos changes take effect on the
next server restart** (the NFS/SMB adapters bind it at startup). The bind
password is write-only — submit `********` (or omit it) on `PUT` to keep the
stored secret. Equivalent CLI: `dfsctl identity-provider {list,get,set,test}`
(see [CLI.md](cli.md)).

## Migration

### Standalone CAS (v0.16 - v0.21) → packed blocks: automatic

Current servers store remote data as packed `blocks/<id>` containers. Shares
that still hold standalone-CAS state (per-chunk `cas/` objects and locators
from v0.16-v0.21 servers) are converted automatically at startup, per share,
**before the share serves** — no command, flag, or sentinel involved. The
conversion is idempotent and resumable; a killed run converges on the next
start. See [the migration guide](block-store-migration.md).

If a share's remote is unreachable while standalone chunks remain, that
share fails to start (its data would be unreadable anyway); restore
connectivity and start again.

### Pre-v0.16 `.blk` layouts: migrate with dittofs ≤ v0.21 first

The offline `dfs migrate-to-cas` tool was removed after v0.21. On startup,
`dfs start` still probes each share for the legacy `.blk` layout (a
`.cas-migrated-v1` sentinel from an old migration short-circuits the probe)
and refuses to start un-migrated shares:

- Exits with code **78** (`EX_CONFIG` per sysexits(3)).
- Prints a directive to stderr naming the offending share and pointing at
  dittofs ≤ v0.21 for the `.blk` migration.
- Halts on the FIRST share that surfaces the legacy layout.

Run `dfs migrate-to-cas` on v0.21, verify the per-share `.cas-migrated-v1`
sentinel exists, then upgrade — the automatic conversion above finishes the
job.

## Metrics (Prometheus)

DittoFS exposes a Prometheus `/metrics` endpoint on a **dedicated listener**,
separate from the control-plane API and the protocol adapters. It is **opt-in
and disabled by default**. When enabled it serves the standard Go/process
collectors plus the DittoFS instruments (request RED metrics per adapter,
connection counts, sync/remote/local-store/quota/GC gauges, and snapshot
timestamps).

```yaml
# Top-level metrics block (NOT nested under `server:`).
metrics:
  enabled: true            # opt-in; default false
  host: 127.0.0.1          # bind interface; default loopback only
  port: 9090               # default 9090
  path: /metrics           # default /metrics
  auth: none               # "none" (default) or "token"
  token_file: ""           # path to a file holding the Bearer token (auth: token)
  tls:                     # optional; reuses the control-plane TLS shape
    cert_file: ""
    key_file: ""
    client_ca: ""          # set for mTLS
    min_version: "1.2"     # minimum TLS version: "1.2" (default) or "1.3"
```

| Option | Default | Description |
|--------|---------|-------------|
| `enabled` | `false` | Turn the metrics listener on. |
| `host` | `127.0.0.1` | Bind interface. Binds loopback by default; set `0.0.0.0` to expose it deliberately (pair with a firewall/NetworkPolicy). |
| `port` | `9090` | TCP port for the endpoint. |
| `path` | `/metrics` | HTTP path. Must start with `/`. |
| `auth` | `none` | `none` (rely on bind host + network policy) or `token` (require a Bearer token). |
| `token_file` | | File whose trimmed contents are the expected Bearer token. Required when `auth: token`. |
| `tls.cert_file` / `tls.key_file` | | Serve the endpoint over HTTPS. |
| `tls.client_ca` | | Require + verify client certificates (mTLS). |
| `tls.min_version` | `1.2` | Minimum negotiated TLS version: `1.2` or `1.3`. |

> **Security**: the endpoint binds `127.0.0.1` by default so it is not reachable
> off-host without an explicit `host` change. In production prefer one of:
> bind loopback and scrape via a sidecar; restrict with a firewall/NetworkPolicy;
> or enable `auth: token` (and/or TLS). Never expose `0.0.0.0` unauthenticated on
> an untrusted network.

### Environment variable overrides

Every key is overridable with the `DITTOFS_METRICS_*` prefix:

| Variable | Maps to |
|----------|---------|
| `DITTOFS_METRICS_ENABLED` | `metrics.enabled` |
| `DITTOFS_METRICS_HOST` | `metrics.host` |
| `DITTOFS_METRICS_PORT` | `metrics.port` |
| `DITTOFS_METRICS_PATH` | `metrics.path` |
| `DITTOFS_METRICS_AUTH` | `metrics.auth` |
| `DITTOFS_METRICS_TOKEN_FILE` | `metrics.token_file` |
| `DITTOFS_METRICS_TLS_CERT_FILE` | `metrics.tls.cert_file` |
| `DITTOFS_METRICS_TLS_KEY_FILE` | `metrics.tls.key_file` |
| `DITTOFS_METRICS_TLS_CLIENT_CA` | `metrics.tls.client_ca` |
| `DITTOFS_METRICS_TLS_MIN_VERSION` | `metrics.tls.min_version` |

```bash
export DITTOFS_METRICS_ENABLED=true
export DITTOFS_METRICS_HOST=0.0.0.0
export DITTOFS_METRICS_PORT=9090
```

### Ownership model

**DittoFS is only a scrape target.** It never runs, bundles, manages, or depends
on a Prometheus/Thanos/Mimir instance. Point your cluster's existing Prometheus
at the endpoint. For production, the standard path is the
[kube-prometheus-stack](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-prometheus-stack)
(prometheus-operator) with long-term storage via Thanos or Mimir. DittoFS ships
**no** dashboards or compose artifacts — the guidance below is enough to wire it
into any standard stack.

> **DittoFS Pro** bundles a turnkey monitoring stack on top of this endpoint: a
> `docker compose --profile monitoring` profile that stands up Prometheus +
> Grafana with a pre-provisioned DittoFS dashboard, plus an in-dashboard Metrics
> section. The endpoint and config documented here are the contract it builds on.

### Scraping (standalone Prometheus)

Add a static or service-discovery scrape job:

```yaml
scrape_configs:
  - job_name: dittofs
    metrics_path: /metrics
    static_configs:
      - targets: ["dittofs-host:9090"]
    # When auth: token is enabled:
    # authorization:
    #   type: Bearer
    #   credentials_file: /etc/prometheus/dittofs-token
```

### Scraping (Kubernetes via the operator)

The DittoFS Kubernetes operator wires this up for you when you opt in on the
`DittoServer` spec. It renders the metrics container port, a dedicated metrics
`Service` carrying `prometheus.io/scrape` annotations (for annotation-based
discovery), and — **only if the `monitoring.coreos.com` CRDs are installed and
you enable it** — a `ServiceMonitor` for the prometheus-operator. If those CRDs
are absent the operator skips the `ServiceMonitor` (logged) and never fails the
reconcile.

```yaml
apiVersion: dittofs.dittofs.com/v1alpha1
kind: DittoServer
metadata:
  name: example
spec:
  storage:
    metadataSize: 10Gi
  metrics:
    enabled: true            # renders the metrics Service + scrape annotations
    port: 9090
    path: /metrics
    # bearerTokenSecret:     # optional authed scrape
    #   name: dittofs-metrics-token
    #   key: token
    serviceMonitor:
      enabled: true          # requires the prometheus-operator CRDs
      interval: 30s
      labels:                # match your Prometheus serviceMonitorSelector
        release: kube-prometheus-stack
```

### Core series (⭐ = key signals)

| Metric | Meaning |
|--------|---------|
| ⭐ `dittofs_adapter_requests_total{protocol,op,status}` | Per-protocol request RED counter (`status` is `ok` or `error`). |
| `dittofs_adapter_request_duration_seconds{protocol,op}` | Request latency histogram. |
| `dittofs_adapter_connections_total{protocol}` | Connections accepted since start, by protocol. |
| `dittofs_client_connections_active{protocol}` | Active client connections, by protocol. |
| `dittofs_auth_attempts_total{protocol,mechanism}` / `dittofs_auth_failures_total{protocol,mechanism}` | Authentication attempts and failures (`mechanism` is `sys`/`krb5`/`ntlm`). |
| ⭐ `dittofs_remote_up{share}` | `1` if the share's remote backend is healthy, else `0`. |
| ⭐ `dittofs_sync_pending_bytes{share}` | On-disk bytes present locally but not yet mirrored to the remote (data at risk). |
| `dittofs_localstore_disk_used_bytes{share}` | Local block-store disk bytes in use. |
| `dittofs_localstore_evictions_total` / `dittofs_localstore_backpressure_total` | Local block-store evictions and write-backpressure events (process-wide). |
| `dittofs_quota_used_bytes{scope,principal,share}` | Bytes used by a quota principal (`scope` user/group, `principal` is the uid/gid). |
| `dittofs_gc_runs_total{result}` / `dittofs_gc_last_run_timestamp_seconds` / `dittofs_gc_freed_bytes_total` | GC run count (`result` ok/error), last-run time, bytes reclaimed. |
| ⭐ `dittofs_snapshot_operations_total{op,result}` | Snapshot operations by `op` (create/delete/restore) and `result` (ok/error). |
| `dittofs_snapshot_duration_seconds{op}` | Snapshot operation latency histogram, by `op` (create/delete/restore). |
| ⭐ `dittofs_snapshot_last_success_timestamp_seconds{share}` | Unix time of the last successful snapshot create (backup-freshness signal). |

### Example alert expressions

```yaml
groups:
  - name: dittofs
    rules:
      # Scheduled snapshots stale: a share with no successful create in 24h.
      # Uses the last-success gauge (its value is the snapshot time); timestamp()
      # on a counter would return the scrape time, not the last snapshot time.
      # Evaluated PER SHARE — an aggregate max() would let a freshly-snapshotted
      # share mask one that has never been snapshotted.
      - alert: DittoFSSnapshotStale
        expr: |
          (time() - dittofs_snapshot_last_success_timestamp_seconds) > 86400
        for: 1h
        labels: { severity: warning }
        annotations:
          summary: "DittoFS has had no successful snapshot create in >24h"

      # Remote block store unreachable.
      - alert: DittoFSRemoteDown
        expr: dittofs_remote_up == 0
        for: 5m
        labels: { severity: critical }
        annotations:
          summary: "DittoFS remote store for {{ $labels.share }} is down"

      # Sync backlog growing (unsynced data not draining).
      - alert: DittoFSSyncBacklogGrowing
        expr: delta(dittofs_sync_pending_bytes[30m]) > 0 and dittofs_sync_pending_bytes > 1e9
        for: 30m
        labels: { severity: warning }
        annotations:
          summary: "DittoFS sync backlog for {{ $labels.share }} is growing"

      # Local cache disk usage near a target ceiling (adjust threshold to your PVC size).
      - alert: DittoFSLocalStoreNearLimit
        expr: dittofs_localstore_disk_used_bytes > 0.9 * 100e9
        for: 10m
        labels: { severity: warning }
        annotations:
          summary: "DittoFS local store for {{ $labels.share }} is near its disk limit"

      # Elevated adapter error rate (>5% of requests over 5m).
      - alert: DittoFSAdapterErrorRate
        expr: |
          sum(rate(dittofs_adapter_requests_total{status="error"}[5m])) by (protocol)
            / sum(rate(dittofs_adapter_requests_total[5m])) by (protocol) > 0.05
        for: 5m
        labels: { severity: warning }
        annotations:
          summary: "DittoFS {{ $labels.protocol }} error rate above 5%"
```

### Dashboard guidance

Build (or import) a Grafana dashboard around the ⭐ core series: an adapter RED
row (rate of `dittofs_adapter_requests_total`, error ratio, latency percentiles
from `dittofs_adapter_request_duration_seconds`), a durability row
(`dittofs_remote_up`, `dittofs_sync_pending_bytes`, and snapshot success rate
from `dittofs_snapshot_operations_total`), and a capacity row
(`dittofs_localstore_disk_used_bytes`, `dittofs_quota_used_bytes`). DittoFS does
not ship a dashboard JSON; these series are stable and named for direct use.

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

#### Bootstrap admin password

On the **first** start (when no `admin` user exists yet) the initial admin password is chosen in this precedence:

1. **`DITTOFS_ADMIN_INITIAL_PASSWORD`** (env, plaintext) — sets a known password and also derives the NT hash, so the admin can authenticate over **SMB** as well as the REST/control-plane API.
2. **`admin.password_hash`** (config, bcrypt `$2a$`/`$2b$`/`$2y$`) — sets a known credential without writing a plaintext secret to disk. No NT hash is derivable from a bcrypt hash, so an admin bootstrapped this way works for the **control-plane/REST API only, not SMB** (use option 1 for SMB). A value that is not a valid bcrypt hash is rejected at startup.
3. **Auto-generated** — a random password is generated. It is printed once **only when `dfs start` runs with stdout attached to an interactive terminal** (i.e. `dfs start --foreground` in a real TTY). In background/daemon mode, and under Docker/systemd/CI where stdout is a pipe, the password is **not written to the log and cannot be recovered** — the log only records that an admin user was created. Recovery via `dfsctl user password admin` is not possible in this state (it requires an authenticated admin session). Options 1 and 2 are consulted **only while bootstrapping a new admin**, so once this first start has created the admin user, setting them and restarting will **not** change the password — you must remove the admin user (reset the control-plane database) and re-bootstrap with option 1 or 2 set. For any non-interactive deployment, set option 1 or 2 before the very first start.

Options 1 and 2 also skip the forced first-login password change (the operator already chose the password). All three apply only on first start; later starts never change an existing admin's password.

```yaml
admin:
  username: admin
  # bcrypt hash, e.g. from `htpasswd -bnBC 10 "" 'my-secure-password' | tr -d ':\n'`
  password_hash: "$2b$10$..."
```

**Examples**:

```bash
# Logging
export DITTOFS_LOGGING_LEVEL=DEBUG
export DITTOFS_LOGGING_FORMAT=json

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

# NFSv3 locking (NLM/NSM) — opt-in; see docs/NFS.md
export DITTOFS_ADAPTERS_NFS_UDP_ENABLED=false        # serve NLM/NSM/MOUNT over UDP
export DITTOFS_ADAPTERS_NFS_PORTMAPPER_ENABLED=false # enable embedded portmapper
export DITTOFS_ADAPTERS_NFS_PORTMAPPER_PORT=10111    # set to 111 for macOS locking

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
# config.yaml has port: 12049 (the DittoFS default)
# Override it to the standard NFS port 2049 (binding <1024 requires root)
DITTOFS_ADAPTERS_NFS_PORT=2049 ./dfs start
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

Persistent storage with access control, structured logging, and metrics:

```yaml
logging:
  level: WARN
  format: json
  output: /var/log/dittofs/server.log

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
./dfsctl adapter enable nfs --port 12049
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
