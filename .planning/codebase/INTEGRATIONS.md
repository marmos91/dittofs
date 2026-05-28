# External Integrations

**Analysis Date:** 2026-05-28

## APIs & External Services

**AWS S3 (and compatible: MinIO, Ceph, Localstack):**
- Purpose: Remote content-addressed blockstore tier.
- SDK: `github.com/aws/aws-sdk-go-v2/service/s3` 1.90.2 (with `config`, `credentials`).
- Code: `pkg/blockstore/remote/s3/store.go` (+ `verifier.go`).
- Features used: PUT, GET, HEAD, range reads, multipart upload, delete, list, server-side metadata stamping (`x-amz-meta-content-hash` as defense-in-depth; verified against BLAKE3 on read).
- Endpoint override + ForcePathStyle for non-AWS providers.

**KMIP (optional encryption key provider):**
- SDK: `github.com/gemalto/kmip-go` 0.1.0.
- Code: `pkg/blockstore/encryption/keyprovider/` (alongside file-based default).

## Data Storage

**Control-plane database:**

| Backend     | Driver                                  | Purpose                                        | Code                                  |
|-------------|------------------------------------------|------------------------------------------------|---------------------------------------|
| SQLite      | `github.com/glebarez/sqlite` 1.11.0      | Single-node default                            | `pkg/controlplane/store/` (GORM)      |
| PostgreSQL  | `github.com/jackc/pgx/v5` 5.7.6 + GORM   | HA / multi-node                                | same package, GORM dialect            |

- Migrations via `github.com/golang-migrate/migrate/v4` 4.19.1.
- Schema covers: users, groups, group memberships, shares, share-adapter configs, adapters, adapter settings, settings, identity mappings, netgroups, durable-handle hints, block metadata.

**Metadata store (file structure, ACLs, locks, durable handles):**

| Backend     | Code                              | Notes                                                      |
|-------------|-----------------------------------|------------------------------------------------------------|
| memory      | `pkg/metadata/store/memory/`      | Ephemeral; testing/dev                                     |
| badger      | `pkg/metadata/store/badger/`      | Persistent single-node; stats cache w/ TTL                 |
| postgres    | `pkg/metadata/store/postgres/`    | Distributed; UUID handles with share encoding              |

All backends pass `pkg/metadata/storetest` conformance.

**Block store (CAS, BLAKE3-256 keyed):**

| Backend         | Code                                  | Notes                                                |
|------------------|---------------------------------------|------------------------------------------------------|
| local/memory    | `pkg/blockstore/local/memory/`        | Ephemeral                                            |
| local/fs        | `pkg/blockstore/local/fs/`            | Append log + rollup; dedup LRU; group commit         |
| remote/memory   | `pkg/blockstore/remote/memory/`       | Ephemeral                                            |
| remote/s3       | `pkg/blockstore/remote/s3/`           | S3/MinIO/Ceph                                        |

Unified contract at `pkg/blockstore/blockstore.go`. Conformance at `pkg/blockstore/blockstoretest/`.

**Optional block decorators:**
- `pkg/blockstore/compression/` — lz4 codec (`pierrec/lz4/v4`)
- `pkg/blockstore/encryption/` — AEAD frames + keyprovider (file or KMIP)

## Authentication & Identity

**REST API:**
- Custom JWT (HMAC) via `golang-jwt/jwt/v5`.
- Implementation: `internal/controlplane/api/auth/`, middleware in `internal/controlplane/api/middleware/`.
- Secret env: `DITTOFS_CONTROLPLANE_SECRET` (was historically `DITTOFS_CONTROLPLANE_JWT_SECRET` — current name documented at `docs/CONFIGURATION.md`).

**User backend:**
- Control-plane store (`pkg/controlplane/store/users.go`).
- bcrypt for POSIX-style passwords; NT hash stored for SMB.

**Protocol-level auth:**
- NFSv3: AUTH_UNIX (`internal/adapter/nfs/auth/`).
- NFSv4 / v4.1: AUTH_UNIX + RPCSEC_GSS Kerberos via `jcmturner/gokrb5/v8` 8.4.4 + `jcmturner/gofork` 1.7.6.
- SMB: NTLM + SPNEGO + Kerberos. Code spread across `internal/auth/`, `internal/adapter/smb/auth/`, `pkg/auth/`, `pkg/auth/kerberos/`, `pkg/identity/kerberos/`.
- Idmap (Kerberos principal ↔ POSIX UID/GID): `pkg/identity/` + `pkg/controlplane/runtime/identity/`.

## Monitoring & Observability

**Logs:** Structured, slog-based (`internal/logger/`). Text or JSON, configurable level + output (stdout/stderr/file).

**Metrics:** Optional Prometheus via `prometheus/client_golang` 1.23.2.
- Default port 9090.
- Per-entity collectors wired through `pkg/health/` wrappers.
- Surfaces NFS/SMB RPCs, blockstore ops (local + remote + syncer), cache events, connection lifecycle.

**Tracing:** Optional OpenTelemetry (`go.opentelemetry.io/otel` 1.36.0+).
- OTLP gRPC exporter; default endpoint `localhost:4317`.
- Sample rate configurable; insecure transport opt-in.

**Profiling:** Optional Pyroscope (`grafana/pyroscope-go` 1.2.7) — CPU, heap, goroutines, mutex, block.

**Error tracking:** None (logs only).

## CI/CD & Deployment

**Hosting:**
- Docker (`Dockerfile`, multi-stage Alpine) + `Dockerfile.goreleaser` for release images.
- Kubernetes via the operator at `k8s/dittofs-operator/` (Helm chart in `chart/`).
- Operator uses `sigs.k8s.io/controller-runtime` (separate `go.mod`).

**Release:**
- `goreleaser` (`.goreleaser.yml`): Linux + macOS, amd64 + arm64; archives + checksums.

**Local development:**
- `docker-compose.yml`: DittoFS, PostgreSQL, Localstack, Prometheus, Grafana (profile-gated).

**CI (GitHub Actions, `.github/workflows/`):**
- `unit-tests.yml`, `lint.yml`
- `e2e-tests.yml`, `integration-tests.yml`
- `posix-tests.yml`, `nfs-kerberos.yml`, `smb-conformance.yml`, `smb-client-compat.yml`
- `operator-tests.yml`, `windows-build.yml`
- `release.yml`, `close-linked-issues.yml`

## Environment Configuration

**Required:**
- `DITTOFS_CONTROLPLANE_SECRET` — JWT signing secret (≥32 chars).
- Database settings (defaults to SQLite at `~/.config/dfs/db.sqlite` when unset).

**Frequently used:**
```
# Logging
DITTOFS_LOGGING_LEVEL=DEBUG|INFO|WARN|ERROR
DITTOFS_LOGGING_FORMAT=text|json
DITTOFS_LOGGING_OUTPUT=stdout|stderr|/path/to/file

# Adapters
DITTOFS_ADAPTERS_NFS_PORT=12049
DITTOFS_ADAPTERS_SMB_PORT=12445

# REST API
DITTOFS_CONTROLPLANE_PORT=8080
DITTOFS_CONTROLPLANE_JWT_ACCESS_TOKEN_DURATION=15m
DITTOFS_CONTROLPLANE_JWT_REFRESH_TOKEN_DURATION=168h

# Database (SQLite default)
DITTOFS_DATABASE_TYPE=sqlite|postgres
DITTOFS_DATABASE_SQLITE_PATH=~/.config/dfs/db.sqlite

# Database (PostgreSQL)
DITTOFS_DATABASE_POSTGRES_HOST=localhost
DITTOFS_DATABASE_POSTGRES_PORT=5432
DITTOFS_DATABASE_POSTGRES_USER=dittofs
DITTOFS_DATABASE_POSTGRES_PASSWORD=...
DITTOFS_DATABASE_POSTGRES_DBNAME=dittofs
DITTOFS_DATABASE_AUTO_MIGRATE=true

# Metrics
DITTOFS_METRICS_ENABLED=true
DITTOFS_METRICS_PORT=9090

# Telemetry
DITTOFS_TELEMETRY_ENABLED=true
DITTOFS_TELEMETRY_ENDPOINT=localhost:4317
DITTOFS_TELEMETRY_INSECURE=true

# Profiling
DITTOFS_TELEMETRY_PROFILING_ENABLED=true
DITTOFS_TELEMETRY_PROFILING_ENDPOINT=http://localhost:4040
```

Reference: `docs/CONFIGURATION.md` (authoritative).

**Secrets:**
- `DITTOFS_CONTROLPLANE_SECRET` — env preferred over config file.
- PostgreSQL password — env or connection string.
- S3 credentials — `AWS_*` env (default SDK chain) or explicit config block.
- Encryption keys — file-based provider (path) or KMIP server credentials.

## Webhooks & Callbacks

- **Incoming:** none. DittoFS does not receive inbound webhooks.
- **Outgoing:** none. Metrics/traces are pull (Prometheus scrape) or push (OTLP gRPC), but DittoFS does not initiate webhook events.

## Testing Infrastructure

**Containers:** `testcontainers-go` 0.40.0 — PostgreSQL + Localstack (S3).
**Cross-protocol clients:**
- `github.com/hirochachacha/go-smb2` 1.1.0 — SMB client used inside E2E.
- Kernel NFS mount + `smbclient`/`mount.cifs` invoked from E2E for real-protocol coverage.
**Conformance harnesses:**
- pjdfstest under `test/nfs-conformance/`.
- smbtorture / WPTS under `test/smb-conformance/`.

---

*Integration audit: 2026-05-28*
