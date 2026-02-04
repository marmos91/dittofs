# Technology Stack

**Analysis Date:** 2026-02-04

## Languages

**Primary:**
- Go 1.25.0 - Entire server codebase, all packages (`cmd/`, `pkg/`, `internal/`)

**Secondary:**
- Bash - Build scripts, helper functions in flake.nix and docker-compose

## Runtime

**Environment:**
- Go 1.25.0 runtime (statically compiled, CGO_ENABLED=0)
- Cross-compilation support for linux/amd64, linux/arm64, darwin/amd64, darwin/arm64

**Package Manager:**
- Go Modules (go.mod/go.sum)
- Lockfile: Present (`go.sum`)

## Frameworks

**Core Protocols:**
- NFSv3 - Full implementation via `pkg/adapter/nfs/` with 28 procedures
- SMB2 - Windows/macOS support via `pkg/adapter/smb/`
- XDR encoding - `github.com/rasky/go-xdr` for wire protocol serialization

**CLI Framework:**
- Cobra v1.8.1 - Command structure for both `cmd/dittofs/` and `cmd/dittofsctl/`

**Web Framework:**
- chi v5.1.0 - HTTP routing for REST API (`pkg/controlplane/api/`)

**Configuration:**
- Viper v1.21.0 - Configuration loading from files, environment variables, defaults
- YAML parsing - gopkg.in/yaml.v3 for config files

## Key Dependencies

**Critical:**

- `github.com/spf13/cobra` v1.8.1 - CLI command structure (both binaries)
- `github.com/spf13/viper` v1.21.0 - Configuration management with env override
- `github.com/go-chi/chi/v5` v5.1.0 - HTTP routing (REST API)
- `github.com/golang-jwt/jwt/v5` v5.3.0 - JWT authentication for control plane API
- `github.com/rasky/go-xdr` v0.0.0-20170124162913-1a41d1a06c93 - XDR encoding for NFS/RPC protocol

**Data Storage:**

- `github.com/dgraph-io/badger/v4` v4.5.2 - Embedded metadata store implementation (`pkg/metadata/store/badger/`)
- `gorm.io/gorm` v1.31.1 - ORM for control plane store (users, groups, shares)
- `gorm.io/driver/postgres` v1.6.0 - PostgreSQL backend for control plane (distributed deployments)
- `github.com/glebarez/sqlite` v1.11.0 - SQLite backend for control plane (single-node)
- `github.com/jackc/pgx/v5` v5.7.6 - PostgreSQL driver with connection pooling

**AWS Integration:**

- `github.com/aws/aws-sdk-go-v2` v1.39.6 - AWS SDK core
- `github.com/aws/aws-sdk-go-v2/service/s3` v1.90.2 - S3 client for payload storage (`pkg/payload/store/s3/`)
- `github.com/aws/aws-sdk-go-v2/config` v1.31.20 - AWS credential/config chain

**Monitoring & Observability:**

- `github.com/prometheus/client_golang` v1.23.2 - Prometheus metrics (`pkg/metrics/`)
- `go.opentelemetry.io/otel` v1.37.0 - Distributed tracing SDK
- `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc` v1.32.0 - OTLP gRPC exporter for traces
- `google.golang.org/grpc` v1.74.2 - gRPC client for OTLP exporter
- `github.com/grafana/pyroscope-go` v1.2.7 - Continuous profiling client

**SMB Authentication:**

- `github.com/jcmturner/gokrb5/v8` v8.4.4 - Kerberos/SPNEGO support for SMB

**Validation & CLI Utilities:**

- `github.com/go-playground/validator/v10` v10.28.0 - Struct validation
- `github.com/manifoldco/promptui` v0.9.0 - Interactive prompts for CLI
- `github.com/olekukonko/tablewriter` v0.0.5 - Formatted table output
- `github.com/mitchellh/mapstructure` v1.5.0 - Struct mapping for config

**Testing:**

- `github.com/testcontainers/testcontainers-go` v0.40.0 - Docker containers for integration tests
- `github.com/testcontainers/testcontainers-go/modules/postgres` v0.40.0 - PostgreSQL test container
- `github.com/stretchr/testify` v1.11.1 - Assertions and mocking (assert, require, mock packages)

**Infrastructure:**

- `github.com/golang-migrate/migrate/v4` v4.19.1 - Database migrations for control plane store

**Schema & Utilities:**

- `github.com/invopop/jsonschema` v0.13.0 - JSON schema generation
- `google.golang.org/protobuf` v1.36.10 - Protobuf support for OTLP

## Configuration

**Environment:**

Configuration sources (in order of precedence):
1. CLI flags (highest)
2. Environment variables (`DITTOFS_*` prefix, normalized to uppercase with underscores)
3. Configuration file (YAML at `~/.config/dittofs/config.yaml` or custom path)
4. Default values (lowest)

Example env overrides:
- `DITTOFS_LOGGING_LEVEL=DEBUG`
- `DITTOFS_ADAPTERS_NFS_PORT=3049`
- `DITTOFS_CONTROLPLANE_JWT_SECRET=...` (32+ characters)
- `DITTOFS_CACHE_PATH=/var/lib/dittofs/cache`
- `DITTOFS_TELEMETRY_ENABLED=true`
- `DITTOFS_TELEMETRY_ENDPOINT=localhost:4317`

**Configuration File:**

Default location: `~/.config/dittofs/config.yaml`

Key configuration sections:
- `logging` - Log level (DEBUG, INFO, WARN, ERROR), format (text/json), output (stdout/stderr/file)
- `telemetry` - OpenTelemetry tracing (enabled, endpoint, sample rate)
- `profiling` - Pyroscope continuous profiling config
- `database` - Control plane store (SQLite or PostgreSQL)
- `cache` - WAL-backed cache directory and size limits
- `controlplane` - REST API server (port, timeouts, JWT secret)
- `metrics` - Prometheus metrics server (port, enabled)
- `admin` - Bootstrap admin user (username, password hash)

Control plane configuration also includes:
- `metadata.stores.*` - Named metadata store instances (memory, badger, postgres)
- `content.stores.*` - Named content store instances (memory, filesystem, s3)
- `shares.*` - Export definitions referencing named stores
- `adapters.*` - Protocol adapter configuration (NFS, SMB)

**Build:**

- `Dockerfile` - Multi-stage Alpine-based production image (1.25-alpine → alpine:3.21)
- `.goreleaser.yml` - GoReleaser configuration for cross-platform binary releases
- `flake.nix` - Nix flake for development environment and packaging
- `Makefile` (in dittofs-operator) - Build automation for operator submodule

## Platform Requirements

**Development:**

- Go 1.25.0 (or later)
- CGO disabled for pure Go builds (CGO_ENABLED=0)
- Optional: Docker for container-based testing
- Optional: Localstack for S3 integration testing (docker-compose profile: s3-backend)
- Optional: PostgreSQL for control plane testing (docker-compose profile: postgres-backend)

**Production:**

- Linux x86_64 (amd64) or ARM64 (aarch64)
- macOS Intel (amd64) or Apple Silicon (aarch64)
- Ports required:
  - 12049/tcp - NFS server (default)
  - 12445/tcp - SMB server (default)
  - 8080/tcp - REST API (control plane)
  - 9090/tcp - Prometheus metrics (optional)
- For S3 backend: AWS credentials (via env vars, IAM role, or credential files)
- For PostgreSQL metadata: PostgreSQL 13+ with network access
- For NFS client testing: Linux/macOS NFS client tools (`mount.nfs`)

**Container:**

- Base image: Alpine 3.21 (minimal ~50MB image)
- Non-root user: `dittofs` (UID 65532)
- Health check: HTTP GET to `http://localhost:8080/health`
- Required volumes:
  - `/data/metadata` - Metadata store persistence (BadgerDB, etc.)
  - `/data/content` - Content store for filesystem backend
  - `/data/cache` - WAL cache directory
  - `/config` - Configuration file location

---

*Stack analysis: 2026-02-04*
