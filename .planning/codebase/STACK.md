# Technology Stack

**Analysis Date:** 2026-05-28

## Languages

**Primary:**
- Go 1.25.0 — Main codebase (`cmd/`, `pkg/`, `internal/`)
- Go 1.24.6 — Kubernetes operator (`k8s/dittofs-operator/`)

**Secondary:**
- Shell — Install/uninstall, E2E test runners, benchmark scripts
- YAML — Config files, GitHub Actions workflows, Helm chart, K8s manifests
- HCL — Pulumi infrastructure (`bench/infra/`)

## Runtime

**Environment:**
- Go runtime (no external VM/interpreter)
- Linux (NFS/SMB serving, production)
- macOS (development, partial e2e)
- Windows (build target only — see `cmd/dfs/commands/daemon_windows.go`, `internal/sysinfo/sysinfo_windows.go`)
- Kubernetes 1.24+ (operator support)

**Package Manager:**
- Go Modules; lockfile `go.mod` + `go.sum`
- Module path: `github.com/marmos91/dittofs`

## Frameworks

**Core:**
- `github.com/go-chi/chi/v5` 5.1.0 — HTTP router (REST control-plane API at `pkg/controlplane/api/`)
- `gorm.io/gorm` 1.31.1 — ORM (control-plane store at `pkg/controlplane/store/`)
- `github.com/spf13/cobra` — CLI framework (`cmd/dfs/commands/`, `cmd/dfsctl/commands/`)
- `github.com/spf13/viper` — Config parsing (`pkg/config/`)

**Storage / Data:**
- `github.com/dgraph-io/badger/v4` 4.5.2 — Embedded KV (metadata store `pkg/metadata/store/badger/`)
- `github.com/jackc/pgx/v5` 5.7.6 — PostgreSQL driver (metadata `pkg/metadata/store/postgres/`)
- `github.com/glebarez/sqlite` 1.11.0 — SQLite driver (control-plane single-node)
- `github.com/aws/aws-sdk-go-v2` 1.39.6 + `service/s3` 1.90.2 — Remote blockstore (`pkg/blockstore/remote/s3/`)

**Protocol / Auth:**
- `github.com/rasky/go-xdr` — XDR encoding for NFS RPC
- `github.com/jcmturner/gokrb5/v8` 8.4.4 — Kerberos (SMB + NFSv4 RPCSEC_GSS)
- `github.com/jcmturner/gofork` 1.7.6 — Pure-Go GSSAPI helpers
- `github.com/hirochachacha/go-smb2` 1.1.0 — Used in tests for client-side SMB
- `github.com/gemalto/kmip-go` 0.1.0 — Optional KMIP keyprovider (`pkg/blockstore/encryption/keyprovider`)

**Testing / E2E:**
- Standard `testing` package + `testify/{assert,require}`
- `testcontainers-go` 0.40.0 — PostgreSQL + Localstack containers
- `github.com/golang-migrate/migrate/v4` 4.19.1 — DB schema migrations

**Build / Dev:**
- `goreleaser` (`.goreleaser.yml`) — Cross-platform binary releases
- `golangci-lint` v2 (`.golangci.yml`) — Linter (govet, unused, errcheck, staticcheck, ineffassign)
- Nix Flake (`flake.nix`) — Reproducible dev environment (optional)

**Compression / Hash:**
- `github.com/pierrec/lz4/v4` — Optional block compression
- BLAKE3 (vendored) — Content-addressed block hashing
- `github.com/oklog/ulid/v2` — ULID generation (where used)
- `github.com/google/uuid` — UUIDs (handles, identifiers)

## Critical Dependencies

**Data plane:**
- `aws-sdk-go-v2/service/s3` — Production remote blockstore
- `jackc/pgx/v5` — HA metadata + control plane
- `dgraph-io/badger/v4` — Single-node persistent metadata
- `glebarez/sqlite` — Default control-plane DB

**Control plane:**
- `chi/v5`, `gorm`, `cobra`, `viper`
- `golang-jwt/jwt/v5` — REST API auth
- `go-playground/validator/v10` — Config + payload validation

**Crypto:**
- `golang.org/x/crypto` — bcrypt, NT hash, signing primitives
- BLAKE3 (vendored)

## Configuration

**Sources (priority order, highest first):**
1. CLI flags
2. Environment variables (`DITTOFS_*` prefix)
3. YAML config file (default `~/.config/dfs/config.yaml`)
4. Built-in defaults (`pkg/config/defaults.go`)

**Build / packaging:**
- `Dockerfile` — Multi-stage Alpine build
- `Dockerfile.goreleaser` — Release image
- `docker-compose.yml` — Local dev stack (Postgres, Localstack, Prometheus, Grafana)
- `.goreleaser.yml` — Linux/macOS amd64+arm64, archives + checksums

**Key config areas (see `pkg/config/config.go`):**
- Logging (level, format text/json, output)
- Telemetry (OTLP endpoint, optional Pyroscope profiling)
- Database (SQLite default; PostgreSQL for HA)
- Blockstore (local/remote, GC, syncer)
- Adapters (NFS, SMB ports + tunables)
- Control plane (REST API, JWT secret)

## Platform Requirements

**Development:**
- Go 1.25+
- Docker (E2E tests + Localstack S3)
- Kernel NFS client (Linux/macOS) for e2e
- `sudo` for kernel-mount e2e tests
- Optional: Nix, smbclient, samba (for cross-protocol tests)

**Production:**
- Docker or Kubernetes 1.24+
- PostgreSQL 12+ (optional, HA)
- AWS S3 or S3-compatible (optional, durable remote blocks)
- Linux NFS/SMB clients for mount

**Default Ports:**
- 12049/tcp — NFS server (vs. standard 2049; operator Service may map to 2049)
- 12445/tcp — SMB server (vs. standard 445)
- 8080/tcp — REST control-plane API
- 9090/tcp — Prometheus metrics (optional)

## Storage Backends

**Control Plane (users/groups/shares/adapters/settings):**
- SQLite (default, single-node) via `glebarez/sqlite`
- PostgreSQL (HA) via `pgx` + GORM

**Metadata Store (file structure, ACLs, locks, handles):**
- `memory` (`pkg/metadata/store/memory/`) — ephemeral, testing
- `badger` (`pkg/metadata/store/badger/`) — persistent single-node
- `postgres` (`pkg/metadata/store/postgres/`) — distributed multi-node

**Block Store (file content, CAS-keyed):**
- `local/memory` (`pkg/blockstore/local/memory/`) — ephemeral
- `local/fs` (`pkg/blockstore/local/fs/`) — filesystem CAS with append log + rollup
- `remote/memory` (`pkg/blockstore/remote/memory/`) — ephemeral
- `remote/s3` (`pkg/blockstore/remote/s3/`) — S3/MinIO/Ceph

Unified `BlockStore` contract lives at `pkg/blockstore/blockstore.go`; `BlockStoreAppend` extension is fs-only.

## Authentication & Crypto

**Password hashing:**
- bcrypt (POSIX/Unix users)
- NT hash (SMB compatibility)

**REST API:**
- JWT (HMAC) via `golang-jwt/jwt/v5`
- Access token default 15m; refresh 7d (configurable)

**SMB:**
- NTLM + SPNEGO (`internal/auth/`, `pkg/auth/`)
- Kerberos via `gokrb5/v8` (`pkg/auth/kerberos/`, `pkg/identity/kerberos/`)
- SMB signing: HMAC-SHA256, AES-CMAC, AES-GMAC (`internal/adapter/smb/signing/`)
- SMB encryption: AES-CCM, AES-GCM (`internal/adapter/smb/encryption/`)

**NFS:**
- AUTH_UNIX (NFSv3 + NFSv4)
- RPCSEC_GSS Kerberos (NFSv4 / v4.1)
- Export-level squashing (RootSquash/AllSquash) applied in mount

**Block content (optional):**
- Compression: lz4 (`pkg/blockstore/compression/`)
- Encryption: AEAD with pluggable keyprovider (`pkg/blockstore/encryption/`, KMIP supported)

## Observability

**Logs:** Structured via `internal/logger/` (text or JSON), configurable level + output
**Metrics:** Prometheus via `prometheus/client_golang` 1.23.2 (opt-in)
**Tracing:** OpenTelemetry OTLP (`go.opentelemetry.io/otel` 1.36.0+, opt-in)
**Profiling:** Pyroscope (`grafana/pyroscope-go` 1.2.7, opt-in)

---

*Stack analysis: 2026-05-28*
