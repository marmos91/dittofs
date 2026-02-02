# Technology Stack

**Analysis Date:** 2026-02-02

## Languages

**Primary:**
- Go 1.25.0 - All server and client binaries (`cmd/dittofs/`, `cmd/dittofsctl/`)

**Secondary:**
- YAML - Configuration files and test data
- Shell - Build scripts and development utilities

## Runtime

**Environment:**
- Go 1.25.0 runtime (Linux, macOS, Windows supported)
- Alpine 3.21 Linux (production container)
- macOS via Nix Darwin or native installation

**Package Manager:**
- Go Modules (go.mod/go.sum)
- Lockfile: `go.sum` (present)

## Frameworks

**Core:**
- Cobra v1.8.1 - CLI framework for both `dittofs` and `dittofsctl` binaries
- chi/v5 v5.1.0 - HTTP router for REST API endpoints in `pkg/controlplane/api/`
- GORM v1.31.1 - ORM for control plane database (SQLite/PostgreSQL)

**Testing:**
- testcontainers-go v0.40.0 - Docker container management for integration tests (S3, PostgreSQL)
- stretchr/testify v1.11.1 - Assertions and mocking utilities

**Build/Dev:**
- golangci-lint - Static analysis (configured via `.golangci.yml`)
- goreleaser - Cross-platform binary builds and releases (`.goreleaser.yml`)

## Key Dependencies

**Critical:**
- rasky/go-xdr v0.0.0-20170124162913 - XDR encoding/decoding for NFS wire protocol
- aws-sdk-go-v2 v1.39.6 - AWS S3 client for block storage (`pkg/payload/store/s3/`)
- dgraph-io/badger/v4 v4.5.2 - Embedded key-value store for metadata (`pkg/metadata/store/badger/`)
- jackc/pgx/v5 v5.7.6 - PostgreSQL driver for control plane store
- glebarez/sqlite v1.11.0 - SQLite driver for single-node control plane

**Infrastructure:**
- prometheus/client_golang v1.23.2 - Prometheus metrics collection (`pkg/metrics/`)
- go-opentelemetry v1.37.0, otlptrace v1.32.0 - Distributed tracing with OTLP (`pkg/config/`)
- grafana/pyroscope-go v1.2.7 - Continuous profiling for performance analysis
- golang-jwt/jwt/v5 v5.3.0 - JWT authentication for REST API (`internal/controlplane/api/auth/`)
- golang-migrate/migrate/v4 v4.19.1 - Database migration tool for PostgreSQL (`pkg/metadata/store/postgres/`)
- jcmturner/gokrb5/v8 v8.4.4 - Kerberos authentication for SMB protocol
- go-chi/chi/v5 v5.1.0 - HTTP routing
- go-playground/validator/v10 v10.28.0 - Struct validation for config and API payloads
- invopop/jsonschema v0.13.0 - JSON schema generation from Go structs

**CLI Utilities:**
- spf13/cobra v1.8.1 - CLI subcommand framework
- spf13/viper v1.21.0 - Configuration file parsing (YAML/TOML)
- manifoldco/promptui v0.9.0 - Interactive terminal prompts (`internal/cli/prompt/`)
- olekukonko/tablewriter v0.0.5 - ASCII table formatting for CLI output

## Configuration

**Environment:**
- YAML configuration file (default: `~/.config/dittofs/config.yaml`)
- DITTOFS_* environment variable overrides via Viper
- Precedence: CLI flags > env vars > config file > defaults

**Build:**
- `flake.nix` - Nix flake for reproducible development environment
- `.goreleaser.yml` - Cross-platform binary building
- `Dockerfile` - Multi-stage Alpine-based container image
- `.golangci.yml` - Linting configuration

## Platform Requirements

**Development:**
- Go 1.25.0
- Docker (for testcontainers-based integration tests)
- NFS client utilities (for end-to-end testing)
- SQLite and PostgreSQL drivers (bundled via GORM)

**Production:**
- Deployment targets: Linux (x86_64, arm64), macOS (x86_64, arm64), Windows
- Container deployment: Docker/Kubernetes (Alpine 3.21 base)
- Persistent storage: SQLite (embedded) or PostgreSQL (recommended for HA)
- Optional: AWS S3 bucket for content storage (or S3-compatible service like MinIO)
- Optional: Prometheus-compatible metrics collector
- Optional: OTLP-compatible trace collector (Jaeger, Tempo, etc.)
- Optional: Pyroscope server for continuous profiling

## Key Configuration Files

**`pkg/config/config.go`:**
- Defines Config struct with nested: Logging, Telemetry, Database, Metrics, ControlPlane, Cache, Admin
- Supports YAML/TOML via Viper
- Environment variable override pattern: `DITTOFS_SECTION_KEY=value`

**`pkg/config/defaults.go`:**
- Default values for logging (INFO, text, stdout), shutdown timeout (30s), cache size (1GB), metrics port (9090)
- JWT token defaults: 15min access, 7day refresh
- Database: SQLite at `~/.config/dittofs/controlplane.db`

**`pkg/config/stores.go`:**
- Creates metadata stores (memory, BadgerDB, PostgreSQL) from config
- Creates block stores (memory, filesystem, S3) from config
- Named store pattern enables reuse across shares

**`pkg/controlplane/store/gorm.go`:**
- Database initialization for SQLite or PostgreSQL
- Auto-migration of schema on startup
- Default PostgreSQL: localhost:5432, disable SSL mode

---

*Stack analysis: 2026-02-02*
