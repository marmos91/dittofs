# Technology Stack

**Analysis Date:** 2026-02-09

## Languages

**Primary:**
- Go 1.25.0 - Main codebase (`cmd/`, `pkg/`, `internal/`)
- Go 1.24.6 - Kubernetes operator (`dittofs-operator/`)

**Secondary:**
- Shell - Installation and utility scripts
- YAML - Configuration, Kubernetes manifests, GitHub Actions workflows

## Runtime

**Environment:**
- Go runtime (no external VM/interpreter required)
- Kubernetes 1.24+ (operator support)
- Linux (NFS/SMB protocols), macOS (development)

**Package Manager:**
- Go Modules
- Lockfile: `go.mod`, `go.sum` present

## Frameworks

**Core:**
- chi/v5 1.8.1 - HTTP router for REST API (`pkg/controlplane/api/`)
- gorm 1.31.1 - ORM for control plane database
- cobra 1.8.1 - CLI framework (`cmd/dittofs/`, `cmd/dittofsctl/`)
- viper 1.21.0 - Configuration management (`pkg/config/`)

**Storage/Data:**
- badger/v4 4.5.2 - Embedded key-value store for metadata (`pkg/metadata/store/badger/`)
- pgx/v5 5.7.6 - PostgreSQL driver for metadata (`pkg/metadata/store/postgres/`)
- sqlite v1.11.0 - SQLite for control plane store (`pkg/controlplane/store/`)
- aws-sdk-go-v2 1.39.6 - S3 client for block storage (`pkg/payload/store/s3/`)

**Testing/E2E:**
- testcontainers-go 0.40.0 - Docker-based test infrastructure
- testify 1.11.1 - Assertion library

**Observability:**
- prometheus/client_golang 1.23.2 - Prometheus metrics (`pkg/metrics/`)
- opentelemetry 1.36.0+ - Distributed tracing (optional, configurable)
- pyroscope-go 1.2.7 - Continuous profiling (optional, configurable)

**Build/Dev:**
- golang-migrate/migrate/v4 4.19.1 - Database migrations
- go-xdr 0.0.0-20170124162913 - XDR encoding for NFS/RPC protocol
- gokrb5/v8 8.4.4 - Kerberos authentication (SMB support)

## Key Dependencies

**Critical:**
- github.com/aws/aws-sdk-go-v2 + s3 - Production S3 storage backend
- github.com/jackc/pgx/v5 - PostgreSQL for HA deployments
- github.com/dgraph-io/badger/v4 - Persistent embedded metadata store
- github.com/glebarez/sqlite - SQLite control plane database
- gorm.io/gorm - ORM for database abstraction

**Infrastructure:**
- github.com/go-chi/chi/v5 - HTTP routing
- github.com/spf13/cobra - CLI command structure
- github.com/spf13/viper - Config file parsing and env var binding
- github.com/golang-jwt/jwt/v5 - JWT authentication for API

**Cryptography:**
- golang.org/x/crypto - Password hashing, encryption primitives

**Validation:**
- github.com/go-playground/validator/v10 - Configuration validation

**Utilities:**
- github.com/google/uuid - UUID generation for resource IDs
- github.com/invopop/jsonschema - JSON schema generation
- github.com/mitchellh/mapstructure - YAML→Go struct mapping

## Configuration

**Environment:**
- YAML-based configuration file (primary)
- Environment variables with `DITTOFS_*` prefix (override)
- CLI flags (highest priority)

**Build:**
- `.golangci.yml` - Go linter configuration
- `.goreleaser.yml` - Release artifact building
- `Dockerfile` - Multi-stage Alpine-based image
- `docker-compose.yml` - Local development environment

**Key Config Areas:**
- Logging (level, format: text/json, output)
- Telemetry (OpenTelemetry OTLP endpoint, Pyroscope profiling)
- Database (SQLite default, PostgreSQL for HA)
- Cache (WAL-backed, mmap persistence)
- Metrics (Prometheus on port 9090)
- API (REST on port 8080, JWT authentication)

## Platform Requirements

**Development:**
- Go 1.25+
- Docker (for E2E tests with NFS mount)
- Nix Flake (optional, for declarative environment via `flake.nix`)
- SQLite 3.x (built into Go driver)

**Production:**
- Docker or Kubernetes (1.24+)
- PostgreSQL 12+ (optional, for HA control plane)
- AWS S3 or S3-compatible service (optional, for durable block storage)
- NFS client kernel module (for client mounting)
- SMB client (optional, for SMB protocol support)

**Runtime Ports:**
- 12049/tcp - NFS server (default, configurable)
- 12445/tcp - SMB server (default, configurable)
- 8080/tcp - REST API (health checks, management)
- 9090/tcp - Prometheus metrics (optional)

## Database Support

**Control Plane (User/Group/Share Management):**
- SQLite (default, single-node) - Zero external dependencies
- PostgreSQL (HA-capable) - pgx driver, GORM adapter

**Metadata Store (File Structure):**
- Memory (ephemeral, testing)
- BadgerDB (persistent, single-node)
- PostgreSQL (distributed, multi-node)

**Block Store (File Content):**
- Memory (ephemeral, testing)
- Filesystem (local/NAS)
- S3 (AWS S3, MinIO, Localstack, Ceph)

## Encryption & Security

**Password Hashing:**
- bcrypt (Unix users)
- NT hash (SMB users)

**API Authentication:**
- JWT with HMAC signing (configurable secret via env)
- Access tokens (default 15m lifetime)
- Refresh tokens (default 7d lifetime)

**TLS/SSL:**
- Optional for external integrations (S3, PostgreSQL, OTLP)
- No built-in TLS for NFS/SMB (network-level security recommended)

---

*Stack analysis: 2026-02-09*
