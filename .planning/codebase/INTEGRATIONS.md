# External Integrations

**Analysis Date:** 2026-02-04

## APIs & External Services

**AWS S3:**
- Content storage backend for large-scale deployments
  - SDK/Client: `github.com/aws/aws-sdk-go-v2/service/s3` v1.90.2
  - Implementation: `pkg/payload/store/s3/`
  - Auth: AWS credentials chain (env vars, IAM role, credential files)
  - Features: Range reads, streaming multipart uploads, configurable retry with exponential backoff
  - Env vars: `AWS_REGION`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` (or via credential files)

**OpenTelemetry (OTLP):**
- Distributed tracing for performance monitoring and debugging
  - SDK: `go.opentelemetry.io/otel` v1.37.0
  - Exporter: `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc` v1.32.0
  - Transport: gRPC (google.golang.org/grpc v1.74.2)
  - Configuration: `pkg/config/config.go` → TelemetryConfig
  - Endpoint: Configurable (default: localhost:4317)
  - Insecure TLS: Configurable (default: true for local dev)
  - Sample rate: Configurable (0.0 to 1.0, default: 1.0 = all traces)
  - Traces cover: NFS operations, storage backend ops, cache operations

**Pyroscope (Continuous Profiling):**
- Continuous CPU and memory profiling for production performance analysis
  - SDK: `github.com/grafana/pyroscope-go` v1.2.7
  - Configuration: `pkg/config/config.go` → ProfilingConfig
  - Endpoint: Configurable (default: http://localhost:4040)
  - Profile types: CPU, allocation, in-use memory, goroutines, mutex, block contention
  - Enabled: Optional (opt-in via config)

## Data Storage

**Databases:**

- **Control Plane Store** (users, groups, shares, settings):
  - Primary: SQLite via `github.com/glebarez/sqlite` v1.11.0 (single-node, file-based)
  - Secondary: PostgreSQL via `gorm.io/driver/postgres` v1.6.0 (distributed/HA)
  - ORM: GORM v1.31.1 with automatic migrations
  - Connection: `pkg/controlplane/store/` (GORMStore)
  - Env vars: `DITTOFS_DATABASE_TYPE` (sqlite/postgres), `DITTOFS_DATABASE_*`

- **Metadata Stores** (file/directory structure):
  - Memory: In-memory ephemeral store (`pkg/metadata/store/memory/`)
  - BadgerDB: Embedded persistent store (`pkg/metadata/store/badger/`, `github.com/dgraph-io/badger/v4` v4.5.2)
  - PostgreSQL: Distributed persistent store (`pkg/metadata/store/postgres/`)
  - Selection: Via named store registry in configuration

- **Content Stores** (file data blocks):
  - Memory: In-memory ephemeral store (`pkg/payload/store/memory/`)
  - Filesystem: Local disk storage (`pkg/payload/store/fs/`)
  - S3: Cloud storage via AWS SDK (`pkg/payload/store/s3/`)
  - Selection: Via named store registry in configuration

**File Storage:**

- S3 via AWS SDK (for large-scale deployments)
- Local filesystem (for single-node deployments)
- In-memory only (for testing and ephemeral workloads)

**Caching:**

- Block-aware cache layer (`pkg/cache/`)
- WAL persistence via mmap (`pkg/cache/wal/`, `pkg/cache/wal/mmap.go`)
- LRU eviction with dirty data protection
- Transfer queue for background uploads (`pkg/payload/transfer/`)

## Authentication & Identity

**Control Plane API:**
- JWT (JSON Web Tokens)
  - Library: `github.com/golang-jwt/jwt/v5` v5.3.0
  - Configuration: `pkg/controlplane/api/` (APIConfig with JWT section)
  - Secret: Configurable (minimum 32 characters)
  - Access token duration: Configurable (default: 15 minutes)
  - Refresh token duration: Configurable (default: 7 days)
  - Env var for secret: `DITTOFS_CONTROLPLANE_JWT_SECRET`
  - Credentials: Username/password with bcrypt hashing

**NFSv3 Protocol:**
- AUTH_UNIX: Standard Unix credentials (UID, GID, GIDs)
- AUTH_NULL: No authentication
- Credential extraction: `internal/protocol/nfs/dispatch.go` → ExtractAuthContext()
- Export-level access control: AllSquash, RootSquash via mount options

**SMB/CIFS Protocol:**
- NTLM authentication
- SPNEGO/Kerberos support
  - Library: `github.com/jcmturner/gokrb5/v8` v8.4.4
  - Implementation: `pkg/adapter/smb/smb_connection.go`
- Session management with connection tracking

## Monitoring & Observability

**Prometheus Metrics:**
- Metrics server: `pkg/metrics/`
- HTTP endpoint: `/metrics` (default port 9090)
- Zero overhead when disabled (optional feature)
- Metrics cover:
  - NFS procedure counters by status
  - Request duration histograms
  - In-flight request gauges
  - Bytes transferred counters
  - Connection lifecycle (accepted/closed/force-closed)
  - Storage backend metrics (S3, BadgerDB)
- Configuration: `pkg/config/metrics.go` → MetricsConfig

**Distributed Tracing (OpenTelemetry):**
- OTLP gRPC exporter to external collectors
- Compatible collectors: Jaeger, Tempo, Honeycomb, Grafana Loki
- Zero overhead when disabled
- Traces include:
  - NFS operation spans (READ, WRITE, LOOKUP, etc.)
  - Storage backend spans (S3, BadgerDB, filesystem)
  - Cache operation spans (hits, misses, flushes)
  - Request context (client IP, file handles, paths)

**Continuous Profiling (Pyroscope):**
- CPU profiling
- Memory allocation profiling (objects and space)
- Memory in-use profiling
- Goroutine profiling
- Mutex contention profiling
- Block contention profiling
- Zero overhead when disabled

## CI/CD & Deployment

**Hosting:**
- Container deployments: Docker images via Dockerfile
- Bare metal: Static Linux/macOS binaries (CGO disabled for portability)
- Nix: Flake-based packaging for NixOS/Nix environments

**Release Automation:**
- GoReleaser (`.goreleaser.yml`):
  - Cross-platform binary builds (linux/amd64, linux/arm64, darwin/amd64, darwin/arm64)
  - Docker image building and pushing
  - GitHub release automation with checksums
  - Version injection via ldflags (version, commit, date)

**Container:**
- Multi-stage Dockerfile: Builder (golang:1.25-alpine) → Runtime (alpine:3.21)
- Image optimization:
  - Static binary (CGO_ENABLED=0)
  - Non-root user (dittofs:65532)
  - OCI image labels for registry metadata
- Health checks via HTTP GET to `/health` endpoint
- Docker Compose for local development:
  - `docker-compose.yml` with profiles for different backends:
    - default: In-memory stores
    - s3-backend: With Localstack for S3 testing
    - postgres-backend: With PostgreSQL for distributed testing

**Development:**
- Nix flake (`flake.nix`):
  - Go 1.25 development environment
  - Helper scripts: dittofs-mount, dittofs-umount
  - pjdfstest for POSIX compliance testing (original C-based suite with 8,789 tests)
  - Cross-compilation support

## Environment Configuration

**Required env vars:**
- `DITTOFS_CONTROLPLANE_JWT_SECRET` - JWT signing secret (minimum 32 characters)
- `DITTOFS_CACHE_PATH` - WAL cache directory (required for crash recovery)
- `DITTOFS_DATABASE_TYPE` - Control plane database type (sqlite/postgres)

**Database connection vars:**
- SQLite: `DITTOFS_DATABASE_SQLITE_PATH`
- PostgreSQL: `DITTOFS_DATABASE_POSTGRES_DSN`

**Storage backend vars:**
- S3: `AWS_REGION`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` (or credential chain)
- BadgerDB: `DITTOFS_METADATA_STORES_<NAME>_BADGER_DB_PATH`

**Monitoring vars:**
- Metrics: `DITTOFS_METRICS_ENABLED`, `DITTOFS_METRICS_PORT` (default 9090)
- Telemetry: `DITTOFS_TELEMETRY_ENABLED`, `DITTOFS_TELEMETRY_ENDPOINT`, `DITTOFS_TELEMETRY_SAMPLE_RATE`
- Profiling: `DITTOFS_TELEMETRY_PROFILING_ENABLED`, `DITTOFS_TELEMETRY_PROFILING_ENDPOINT`

**Logging vars:**
- `DITTOFS_LOGGING_LEVEL` - DEBUG, INFO, WARN, ERROR (default: INFO)
- `DITTOFS_LOGGING_FORMAT` - text or json (default: text)
- `DITTOFS_LOGGING_OUTPUT` - stdout, stderr, or file path

**Secrets location:**
- Control plane store: Config file or environment variable (not committed)
- AWS credentials: Standard AWS credential chain (env vars, ~/.aws/credentials, IAM role)
- Database passwords: Environment variables or credential files (not committed)

## Webhooks & Callbacks

**Incoming:**
- REST API endpoints only (no webhook receivers)
- Health check endpoints: GET `/health`, `/health/ready`, `/health/stores`

**Outgoing:**
- None - DittoFS is a server, not an API client to external systems
- Logging is to configured outputs (stdout, stderr, or file)
- Metrics exported to Prometheus scraper (pull-based, not push)
- Traces exported to OTLP collector (active push via gRPC)
- Profiles exported to Pyroscope server (active push)

## Integration Points Summary

| Component | Integration Type | Technology | Purpose |
|-----------|-----------------|-----------|---------|
| Content Storage | Cloud API | AWS S3 | Large-scale file storage |
| Monitoring | Metrics | Prometheus | Performance metrics scraping |
| Distributed Tracing | Observability | OTLP gRPC | Distributed request tracing |
| Continuous Profiling | Observability | Pyroscope | CPU/memory profiling over time |
| Control Plane Auth | API Auth | JWT + Bcrypt | REST API authentication |
| NFS Auth | Protocol Auth | AUTH_UNIX | NFSv3 client authentication |
| SMB Auth | Protocol Auth | NTLM/Kerberos | Windows/macOS authentication |
| Metadata (HA) | Database | PostgreSQL | Distributed metadata storage |
| Metadata (Single-node) | Database | SQLite | Embedded metadata storage |
| Metadata (Temp) | In-Memory | Native Go maps | Ephemeral metadata storage |
| Configuration | File-based | YAML/Environment | Static and dynamic config |

---

*Integration audit: 2026-02-04*
