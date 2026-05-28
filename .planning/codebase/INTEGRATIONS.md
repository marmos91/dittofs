# External Integrations

**Analysis Date:** 2026-02-09

## APIs & External Services

**AWS Services:**
- AWS S3 (Block Storage) - File content persistence
  - SDK/Client: github.com/aws/aws-sdk-go-v2/service/s3 v1.90.2
  - Auth: IAM roles, static credentials (AccessKey/SecretKey)
  - Configuration: `pkg/payload/store/s3/store.go`
  - Features: Multipart uploads, range reads, batch delete, server-side copy
  - Alternative S3: MinIO, Localstack, Ceph (via endpoint override + ForcePathStyle)

## Data Storage

**Databases:**

**SQLite (Default Control Plane):**
- Type: Embedded relational database
- Connection: File-based (single file, no network)
- Client: github.com/glebarez/sqlite v1.11.0
- ORM: gorm.io/gorm v1.31.1
- Purpose: Users, groups, shares, adapters, settings (single-node)
- Location: `pkg/controlplane/store/`

**PostgreSQL (Optional HA Control Plane):**
- Type: Enterprise relational database
- Connection: TCP connection string (postgresql://user:pass@host:port/dbname)
- Client: github.com/jackc/pgx/v5 v5.7.6 (connection pooling)
- ORM: gorm.io/driver/postgres v1.6.0
- Purpose: Multi-node HA control plane (users, groups, shares)
- Location: `pkg/controlplane/store/gorm.go`
- Migrations: golang-migrate/migrate/v4 v4.19.1
- Env Var: `DITTOFS_DATABASE_TYPE=postgres` + connection config

**BadgerDB (Metadata Store):**
- Type: Embedded key-value store
- Connection: Directory path for files
- Client: github.com/dgraph-io/badger/v4 v4.5.2
- Purpose: File metadata (attributes, permissions, handles)
- Location: `pkg/metadata/store/badger/`
- Production defaults: 1GB block cache, 512MB index cache

**PostgreSQL (Metadata Store):**
- Type: Enterprise relational database
- Connection: TCP connection string
- Client: github.com/jackc/pgx/v5 v5.7.6
- Purpose: Distributed file metadata with UUID-based handles
- Location: `pkg/metadata/store/postgres/`

**In-Memory Stores:**
- Type: Ephemeral (testing, development)
- Purpose: Fast metadata and block storage without persistence
- Location: `pkg/metadata/store/memory/`, `pkg/payload/store/memory/`

**File Storage:**
- Filesystem (Local/NAS) - Local directory for blocks
  - Path: Configurable base directory
  - Implementation: `pkg/payload/store/fs/` (atomic writes, atomic deletes)
- S3-Compatible - Cloud storage with backoff retry logic
  - Max retries: Configurable (default 3)
  - Multipart: Automatic for large uploads

**Caching:**
- In-Process Memory Cache (`pkg/cache/`)
- WAL Persistence (`pkg/cache/wal/`)
  - Persister: Mmap file (MmapPersister) or null
  - Purpose: Crash recovery, write durability
  - Location: `cache.path` config (mmap-backed)

## Authentication & Identity

**Auth Provider:**
- Custom JWT-based (no external OAuth)
  - Implementation: `internal/controlplane/api/auth/`
  - Signing: HMAC with configurable secret
  - Env var: `DITTOFS_CONTROLPLANE_SECRET`

**User Backend:**
- Control Plane Store (SQLite/PostgreSQL)
  - Password: bcrypt hash
  - NT Hash: For SMB authentication (pass-the-hash compatible)

**Protocol-Level Auth:**
- NFS: AUTH_UNIX (client UID/GID)
- SMB: Kerberos (gokrb5/v8 v8.4.4) + local password validation
- API: JWT bearer tokens

## Monitoring & Observability

**Error Tracking:**
- None (no Sentry/Rollbar integration) - Use logs instead

**Logs:**
- Structured logging via `internal/logger/` package
- Formats: text (human-readable) or JSON (machine-parseable)
- Output: stdout, stderr, or file path
- Default: INFO level, text format to stdout

**Metrics:**
- Prometheus (`github.com/prometheus/client_golang v1.23.2`)
  - Port: 9090 (configurable)
  - Enabled: Optional (opt-in, zero overhead when disabled)
  - Metrics collected:
    - NFS: RPC procedures (READ, WRITE, LOOKUP, etc.)
    - Storage: S3, BadgerDB operations
    - Cache: Hits, misses, evictions, flushes
    - Connections: Active, accepted, closed
  - Location: `pkg/metrics/prometheus/`

**Tracing:**
- OpenTelemetry (`go.opentelemetry.io/otel` v1.36.0+)
  - Exporter: OTLP gRPC (compatible with Jaeger, Tempo, Honeycomb, etc.)
  - Endpoint: Configurable (default: localhost:4317)
  - Enabled: Optional (opt-in, zero overhead when disabled)
  - Sample rate: Configurable (0.0-1.0)
  - Insecure: Default true (for local dev), false for production
  - Traces include: NFS operations, storage operations, cache operations

**Profiling:**
- Pyroscope (`github.com/grafana/pyroscope-go v1.2.7`)
  - Endpoint: Configurable (default: http://localhost:4040)
  - Enabled: Optional (opt-in)
  - Profile types: CPU, memory allocation, goroutines, mutex contention, block profiling
  - Location: config in `TelemetryConfig.Profiling`

## CI/CD & Deployment

**Hosting:**
- Docker (Alpine-based production image, `Dockerfile`)
- Kubernetes (operator-based, `dittofs-operator/`)
  - Operator: Controller Runtime (`sigs.k8s.io/controller-runtime v0.22.4`)
  - K8s API: `k8s.io/apimachinery v0.34.1`, `k8s.io/client-go v0.34.1`

**Release:**
- GoReleaser (`.goreleaser.yml`)
  - Targets: Linux (amd64, arm64, arm), macOS (amd64, arm64)
  - Archive formats: tar.gz, zip

**Local Development:**
- Docker Compose (`docker-compose.yml`)
  - Services: DittoFS, PostgreSQL, Localstack (S3), Prometheus, Grafana
  - Profiles: default, s3-backend, postgres-backend

## Environment Configuration

**Required env vars:**
- `DITTOFS_LOGGING_LEVEL` - Log level (DEBUG, INFO, WARN, ERROR)
- `DITTOFS_CONTROLPLANE_SECRET` - JWT signing secret (min 32 chars)
- `DITTOFS_DATABASE_*` - Database connection (type, SQLite path, or PostgreSQL conn string)
- `DITTOFS_CACHE_PATH` - WAL cache directory (required)

**Optional env vars:**
```
# Logging
DITTOFS_LOGGING_FORMAT=json            # text or json
DITTOFS_LOGGING_OUTPUT=/var/log/...    # stdout, stderr, or file

# Server
DITTOFS_SERVER_SHUTDOWN_TIMEOUT=30s    # Graceful shutdown timeout
DITTOFS_SERVER_RATE_LIMITING_ENABLED   # Rate limiting (true/false)

# Adapters
DITTOFS_ADAPTERS_NFS_PORT=12049        # NFS server port
DITTOFS_ADAPTERS_SMB_PORT=12445        # SMB server port

# Metrics
DITTOFS_METRICS_ENABLED=true           # Enable Prometheus metrics
DITTOFS_METRICS_PORT=9090              # Metrics port

# Telemetry
DITTOFS_TELEMETRY_ENABLED=true         # Enable OpenTelemetry
DITTOFS_TELEMETRY_ENDPOINT=localhost:4317
DITTOFS_TELEMETRY_INSECURE=true

# Profiling
DITTOFS_TELEMETRY_PROFILING_ENABLED=true
DITTOFS_TELEMETRY_PROFILING_ENDPOINT=http://localhost:4040

# Control Plane API
DITTOFS_CONTROLPLANE_PORT=8080         # API port
DITTOFS_CONTROLPLANE_JWT_ACCESS_TOKEN_DURATION=15m
DITTOFS_CONTROLPLANE_JWT_REFRESH_TOKEN_DURATION=168h

# Database (SQLite)
DITTOFS_DATABASE_SQLITE_PATH=~/.config/dittofs/db.sqlite

# Database (PostgreSQL)
DITTOFS_DATABASE_TYPE=postgres
DITTOFS_DATABASE_POSTGRES_HOST=localhost
DITTOFS_DATABASE_POSTGRES_PORT=5432
DITTOFS_DATABASE_POSTGRES_USER=dittofs
DITTOFS_DATABASE_POSTGRES_PASSWORD=password
DITTOFS_DATABASE_POSTGRES_DBNAME=dittofs
DITTOFS_DATABASE_AUTO_MIGRATE=true
```

**Secrets location:**
- `DITTOFS_CONTROLPLANE_SECRET` - Environment variable (preferred)
- `~/.config/dittofs/config.yaml` - Config file (fallback)
- PostgreSQL password in `DITTOFS_DATABASE_POSTGRES_PASSWORD` or connection string
- S3 credentials in `AWS_*` env vars (SDK default chain) or config file

## Webhooks & Callbacks

**Incoming:**
- None (DittoFS is a server, not a client initiating outbound webhooks)

**Outgoing:**
- None (No event delivery/webhook system)
- Metrics are push-only to Prometheus (scrape-based)
- Traces are push-only to OTLP collector (gRPC)

## Testing Infrastructure

**Integration Testing:**
- testcontainers-go - Docker container orchestration for tests
  - PostgreSQL: `testcontainers-go/modules/postgres`
  - S3 (Localstack): Custom setup in test files

**E2E Testing:**
- Real NFS mount via kernel client (requires sudo)
- Docker Compose orchestration for multi-service tests
- File-based assertions on mounted filesystem

---

*Integration audit: 2026-02-09*
