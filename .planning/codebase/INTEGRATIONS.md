# External Integrations

**Analysis Date:** 2026-02-02

## APIs & External Services

**Cloud Storage:**
- AWS S3 - Content block storage backend
  - SDK/Client: `github.com/aws/aws-sdk-go-v2/service/s3` v1.90.2
  - Implementation: `pkg/payload/store/s3/store.go`
  - Auth: Static credentials (AccessKey/SecretKey) or AWS SDK default chain (IAM roles, env vars)
  - Features: Range reads, multipart uploads, path-style addressing for S3-compatible services
  - Supports: AWS S3, MinIO, Localstack, Ceph (any S3-compatible endpoint)

**Identity & Authentication:**
- JWT tokens for REST API
  - Package: `github.com/golang-jwt/jwt/v5` v5.3.0
  - Implementation: `internal/controlplane/api/auth/jwt.go`
  - Secret config: Via `DITTOFS_CONTROLPLANE_SECRET` env var (must be ≥32 chars)
  - Token types: Access (default 15min) and Refresh (default 7days) tokens
  - Middleware: `internal/controlplane/api/middleware/jwt_auth.go`

- Kerberos authentication for SMB protocol
  - Package: `github.com/jcmturner/gokrb5/v8` v8.4.4
  - Implementation: `internal/auth/spnego/spnego.go`
  - Used by: SMB session setup for domain authentication

## Data Storage

**Databases:**
- SQLite (default)
  - Client: `github.com/glebarez/sqlite` v1.11.0 (pure Go, no CGO)
  - Location: `~/.config/dittofs/controlplane.db` (configurable)
  - Purpose: Control plane store (users, groups, shares, adapters, permissions)
  - ORM: GORM v1.31.1 with `gorm.io/driver/sqlite` (bundled)

- PostgreSQL (recommended for HA)
  - Client: `github.com/jackc/pgx/v5` v5.7.6
  - Driver: `gorm.io/driver/postgres` v1.6.0
  - Connection: Configurable host/port/user/password
  - Features: SSL mode configurable (disable, require, verify-ca, verify-full)
  - Pool: Default max 25 open, 5 idle connections
  - Port: Default 5432
  - Migration: `github.com/golang-migrate/migrate/v4` v4.19.1 handles schema updates

**Metadata Storage (for file systems):**
- BadgerDB (embedded, persistent)
  - Package: `github.com/dgraph-io/badger/v4` v4.5.2
  - Implementation: `pkg/metadata/store/badger/`
  - Purpose: Persistent file metadata (inodes, attributes, permissions)
  - Features: Path-based file handles enabling recovery from content store

- PostgreSQL (distributed, HA-capable)
  - Implementation: `pkg/metadata/store/postgres/`
  - Purpose: Distributed metadata storage for multi-server deployments
  - Features: UUID-based file handles for shareName + ID encoding

- Memory (in-process, ephemeral)
  - Implementation: `pkg/metadata/store/memory/`
  - Purpose: Testing and temporary file systems
  - Data lost on restart

**Block/Content Storage:**
- S3 or S3-compatible (production recommended)
  - Implementation: `pkg/payload/store/s3/`
  - Configuration: `pkg/config/config.go` - S3Config struct
  - Features:
    - Range reads via HTTP Range header
    - Multipart uploads for large files
    - Batch delete via DeleteObjects API
    - Connection pooling (HTTP/1.1, 200 max idle conns per host)
    - Configurable retry with exponential backoff
    - Path-style addressing for Localstack/MinIO

- Filesystem (local/NAS/SAN)
  - Implementation: `pkg/payload/store/fs/` (not found in docs but referenced)
  - Purpose: Local file system or mounted storage
  - Features: Atomic writes, configurable permissions

- Memory (ephemeral, testing)
  - Implementation: `pkg/payload/store/memory/`
  - Purpose: Testing and in-memory deployments

**Caching:**
- Block-aware cache with WAL persistence
  - Package: `pkg/cache/` and `pkg/cache/wal/`
  - Purpose: In-memory block buffer cache for file data
  - Persistence: Memory-mapped WAL (Write-Ahead Log) for crash recovery
  - Features: LRU eviction, dirty data protection, 4MB block boundaries
  - Location: Configurable via `cache.path` in config (required)
  - Size: Configurable via `cache.size` (default 1GB, supports human-readable: "1GB", "512MB", "10Gi")
  - Persistence layers:
    - `pkg/cache/wal/mmap.go` - Memory-mapped file persister
    - `pkg/cache/wal/null.go` - No-op persister for in-memory only

## Authentication & Identity

**Auth Provider:**
- Custom (built-in)
  - Implementation: `pkg/controlplane/store/` and `internal/controlplane/api/auth/`
  - Approach:
    - User passwords: bcrypt hashing
    - SMB: Additional NT hash (password equivalent for SMB protocol, stored hashed)
    - API: JWT tokens (HS256 with configurable secret)
  - User/Group CRUD: `pkg/controlplane/models/`
  - Password validation: `pkg/controlplane/store/users.go` - bcrypt compare

**API Authentication:**
- JWT via Authorization header
  - Implementation: `internal/controlplane/api/middleware/`
  - Header: `Authorization: Bearer <token>`
  - Token validation: HS256 signature verification
  - Claims: `internal/controlplane/api/auth/claims.go` - UserID, Username, Admin flag
  - Renewal: POST `/api/v1/auth/refresh` with refresh token

**SMB Authentication:**
- SPNEGO/Kerberos negotiation
  - Implementation: `internal/auth/spnego/spnego.go`
  - Supported: NTLM and Kerberos (via gokrb5)
  - Session setup: `internal/protocol/smb/v2/handlers/session_setup.go`

## Monitoring & Observability

**Metrics:**
- Prometheus
  - Package: `github.com/prometheus/client_golang` v1.23.2
  - Implementation: `pkg/metrics/` and `pkg/metrics/prometheus/`
  - Endpoint: `/metrics` (default port 9090)
  - Metrics collected:
    - NFS: Request counters, duration histograms, in-flight gauges by procedure
    - Storage: S3 operation metrics, BadgerDB metrics
    - Connections: Accepted, closed, force-closed counters
    - Cache: Hit/miss/eviction rates
  - Configuration: `metrics.enabled` and `metrics.port` in config
  - Zero overhead when disabled

**Logs:**
- Structured logging with configurable format
  - Implementation: `internal/logger/`
  - Formats:
    - text: Human-readable with colors (for terminal)
    - json: Structured JSON (for log aggregation)
  - Levels: DEBUG, INFO, WARN, ERROR (case-insensitive)
  - Output: stdout (default), stderr, or file path
  - Configuration: `logging.level`, `logging.format`, `logging.output`
  - Environment override: `DITTOFS_LOGGING_LEVEL=DEBUG`

**Distributed Tracing:**
- OpenTelemetry with OTLP exporter
  - Packages: `go.opentelemetry.io/otel/*` v1.37.0, `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc` v1.32.0
  - Implementation: `pkg/config/` - TelemetryConfig struct
  - Exporter: OTLP gRPC (compatible with Jaeger, Tempo, Honeycomb, etc.)
  - Configuration:
    - `telemetry.enabled` - opt-in, default false
    - `telemetry.endpoint` - default "localhost:4317"
    - `telemetry.insecure` - default true (skip TLS for local dev)
    - `telemetry.sample_rate` - 0.0 to 1.0 (default 1.0 = all traces)
  - Spans: NFS operations (READ, WRITE, LOOKUP), storage backend ops, cache operations
  - Zero overhead when disabled

**Continuous Profiling:**
- Pyroscope
  - Package: `github.com/grafana/pyroscope-go` v1.2.7
  - Implementation: `pkg/config/` - ProfilingConfig struct
  - Endpoint: Pyroscope server URL (default "http://localhost:4040")
  - Profile types: cpu, alloc_objects, alloc_space, inuse_objects, inuse_space, goroutines (configurable)
  - Configuration: `telemetry.profiling.enabled`, `telemetry.profiling.endpoint`, `telemetry.profiling.profile_types`
  - Zero overhead when disabled

## CI/CD & Deployment

**Hosting:**
- Containerized via Docker (Alpine 3.21 base)
  - Multi-stage build: Go 1.25 builder → Alpine runtime
  - Non-root user: uid 65532, gid 65532
  - Exposed ports: 12049 (NFS), 12445 (SMB), 9090 (metrics), 8080 (API)
  - Volume mounts: `/data/metadata`, `/data/content`, `/data/cache`, `/config`

**CI Pipeline:**
- GitHub Actions (see `.github/` for workflows)
- GoReleaser for cross-platform builds
  - Configuration: `.goreleaser.yml`
  - Targets: linux (amd64, arm64), darwin (amd64, arm64), windows (amd64)

## Environment Configuration

**Required env vars:**
- `DITTOFS_CONTROLPLANE_SECRET` - JWT secret for REST API (must be ≥32 chars, no default)

**Optional env vars:**
- Logging: `DITTOFS_LOGGING_LEVEL` (DEBUG, INFO, WARN, ERROR), `DITTOFS_LOGGING_FORMAT` (text, json)
- Server: `DITTOFS_SERVER_SHUTDOWN_TIMEOUT` (duration string, default 30s)
- Adapters: `DITTOFS_ADAPTERS_NFS_PORT` (default 12049), `DITTOFS_ADAPTERS_SMB_PORT` (default 12445)
- Database: `DITTOFS_DATABASE_TYPE` (sqlite or postgres)
- Metrics: `DITTOFS_METRICS_ENABLED` (true/false), `DITTOFS_METRICS_PORT` (default 9090)
- Telemetry: `DITTOFS_TELEMETRY_ENABLED`, `DITTOFS_TELEMETRY_ENDPOINT`, `DITTOFS_TELEMETRY_SAMPLE_RATE`
- Cache: `DITTOFS_CACHE_PATH` (required), `DITTOFS_CACHE_SIZE` (default 1GB)

**Secrets location:**
- `DITTOFS_CONTROLPLANE_SECRET` - Environment variable only (never store in config file)
- Database passwords - Via environment variables or PostgreSQL conn string
- AWS credentials - Via environment variables or AWS SDK credential chain (IAM roles, config files)

## Webhooks & Callbacks

**Incoming:**
- None - DittoFS does not expose incoming webhooks

**Outgoing:**
- None - DittoFS does not make outbound HTTP callbacks

---

*Integration audit: 2026-02-02*
