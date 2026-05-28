# Testing Patterns

**Analysis Date:** 2026-05-28

## Framework

- Standard `testing` package
- `github.com/stretchr/testify/assert` (non-fatal) + `require` (fatal)
- 526 `*_test.go` files across the repo (Go) plus E2E/conformance suites

## Run Commands

```bash
# Unit + integration (excludes E2E build tag)
go test ./...

# With race detector
go test -race ./...

# With coverage
go test -cover ./...
go test -coverprofile=coverage.out ./...

# Specific package
go test ./pkg/metadata/...
go test ./internal/adapter/smb/v2/handlers/...

# E2E suite (kernel mounts, sudo, Docker)
cd test/e2e && sudo ./run-e2e.sh
sudo ./run-e2e.sh --s3             # include S3 via Localstack
sudo ./run-e2e.sh --test TestCreateFile_1MB

# Lint
go fmt ./...
go vet ./...
golangci-lint run
```

## File Organization

**Co-located tests:**
- `pkg/blockstore/engine/engine.go` ↔ `engine_test.go`
- `pkg/metadata/file_modify.go` ↔ `file_modify_test.go`
- `internal/adapter/smb/v2/handlers/create.go` ↔ `create_test.go` + many feature-specific tests (e.g. `create_dacl_enforcement_test.go`)

**Cross-backend conformance suites:**
- `pkg/metadata/storetest/` — every `MetadataStore` backend must pass (CLAUDE.md invariant 7).
  - `suite.go`, `dir_ops.go`, `file_ops.go`, `file_block_ops.go`, `permissions.go`, `objectid_lookup.go`, `objectid_roundtrip.go`, `durable_handles.go`, `backup_conformance.go`, `shares_blocklayout.go`, `blockref_roundtrip.go`, `inv02_fuzz.go`
- `pkg/blockstore/blockstoretest/` — every `BlockStore` backend.
- Each backend imports the suite and registers its constructor.

**End-to-end (`test/e2e/`):**
- `framework/` — server lifecycle, container orchestration, mount management, NFSv4 grace helpers.
- `helpers/` — CLI runner, login, unique-name generator, fixtures.
- `fixtures/` — static test data.
- Suites cover: adapters, blockstore matrix, store matrix, dedup, cross-protocol (lease/lock/Kerberos), NFSv3/v4/v4.1 (basic, ACL, delegation, locking, recovery, stress, session, disconnect, coexistence), SMB3 (gosmb2, smbclient, Kerberos), permissions/groups/users/shares, portmapper, grace period, vmfleet dedup.

**Other suites:**
- `test/integration/kerberos/`, `test/integration/portmap/`
- `test/nfs-conformance/` — pjdfstest harness
- `test/smb-conformance/` — smbtorture / WPTS harness
- `test/posix/` — POSIX compliance runner
- `test/edge/` — fuzz-adjacent edge cases

## Test Structure

**Standard pattern:**
```go
func TestSomething_Scenario(t *testing.T) {
    // 1. Setup
    fx := newFixture(t)
    defer fx.Close()

    // 2. Arrange
    h := fx.CreateFile("a.txt", []byte("hello"))

    // 3. Act
    resp, err := fx.Handler.Write(fx.ContextWithUID(0, 0), req)

    // 4. Assert
    require.NoError(t, err)
    assert.EqualValues(t, types.NFS3_OK, resp.Status)
}
```

**Subtests:**
```go
t.Run("happy path", func(t *testing.T) { ... })
t.Run("permission denied", func(t *testing.T) { ... })
```

**Cleanup:**
- `t.Cleanup(func() { ... })` for per-test teardown.
- `t.TempDir()` for scratch directories.
- E2E uses `helpers.StartServerProcess(t, cfg)` + `t.Cleanup(sp.ForceKill)`.

## Mocking Strategy

**No mocking framework.** Real in-memory implementations everywhere:
- `metadatamemory.NewMemoryMetadataStoreWithDefaults()`
- `blockstoremem.New()` / `localmemory.New()`
- Real engine, real syncer, real GC — wired with in-memory backends.

**Benefits:**
- Tests behavior contracts, not implementation details.
- Drives the conformance suites.
- Avoids drift between mock and real components.

**Exceptions:**
- Specific failure injection lives in test helpers (e.g., `pkg/blockstore/engine/syncer_put_error_test.go`).
- `nil_remotestore_test.go` exercises offline behavior.

## Fixtures

**Handler fixtures:**
- `internal/adapter/nfs/v3/handlers/testing/` — full NFSv3 handler env (store, blockstore, engine, runtime).
- `internal/adapter/smb/v2/handlers/handler_test.go` — SMB handler fixtures.

**E2E helpers (`test/e2e/helpers/`):**
- `StartServerProcess(t, cfg)` — spawns a real `dfs` server.
- `LoginAsAdmin(t, apiURL)` — returns a `dfsctl` CLI wrapper.
- `UniqueTestName(prefix)` — collision-safe naming.
- Custom matrix harness (`matrix_config_test.go`) for backend permutations.

**Framework (`test/e2e/framework/`):**
- Postgres + Localstack container orchestration (testcontainers-go).
- Server config templating, NFS/SMB mount helpers, idempotent teardown.

## Build Tags

- E2E tests: `//go:build e2e` (separates from `go test ./...` default run).
- Race-aware tests:
  - `*_race_test.go` (`//go:build race`)
  - `*_norace_test.go` (`//go:build !race`)
- Conformance harnesses gated by their own tags inside `test/{nfs,smb}-conformance/`.

Common usage:
```bash
go test -tags=e2e -v ./test/e2e/...
go test -race -tags=e2e -v ./test/e2e/...
```

## Test Types

**Unit:**
- Co-located. Real in-memory components. <100 ms.
- Coverage: parsers, codecs, ACL logic, lock state machines, blockstore primitives.

**Integration:**
- Real backends behind in-process drivers (Badger, Postgres via testcontainers, S3 via Localstack).
- Conformance suites (`storetest`, `blockstoretest`) executed per backend.
- Medium duration (1–10 s/test).

**Cross-protocol:**
- `test/e2e/cross_protocol_*_test.go` — same files seen via NFS and SMB simultaneously (lease, lock, Kerberos).

**Protocol conformance:**
- `test/nfs-conformance/` against pjdfstest.
- `test/smb-conformance/` against smbtorture / WPTS. Known-failures tracked in repo manifests; CI flips them as fixes land.

**E2E:**
- Real `dfs` process, real kernel NFS/SMB mounts, real S3 (Localstack) or PostgreSQL.
- Slow (10–60 s/test). Run via `test/e2e/run-e2e.sh`.

## Benchmarks

- `*_bench_test.go` files include go-style benchmarks.
- Examples: `pkg/blockstore/hash_bench_test.go`, `pkg/blockstore/chunker/chunker_bench_test.go`, `pkg/blockstore/local/fs/appendwrite_group_commit_bench_test.go`, `internal/bench/phase19_test.go`.
- External benchmark harness under `bench/` (Pulumi-managed Scaleway VMs).
- Detailed methodology + results: `test/e2e/BENCHMARKS.md` and `docs/BENCHMARKS.md`.

## Coverage

- Not gated in CI.
- Surface via `go test -coverprofile=coverage.out ./... && go tool cover -html=coverage.out`.

## Common Patterns

**Auth context plumbing:**
```go
ctx := fx.ContextWithUID(0, 0)        // root
ctx := fx.ContextWithUID(1000, 1000)  // unprivileged
```

**Error code assertions:**
```go
require.NoError(t, err)
assert.EqualValues(t, types.NFS3ERR_ACCES, resp.Status)
```

**Spec references in comments:**
```go
// TestWrite_RFC1813_Stable verifies WRITE handler stability semantics per
// RFC 1813 §3.3.7 (UNSTABLE / DATA_SYNC / FILE_SYNC).
```

**Cancellation:**
```go
ctx, cancel := context.WithCancel(context.Background())
cancel()
_, err := h.Read(ctx, req)
require.ErrorIs(t, err, context.Canceled)
```

## Known Failures / Flakes Discipline (project memory)

- Never touch `KNOWN_FAILURES.md` until CI confirms a test flips.
- Two-commit pattern: code-only first, KNOWN_FAILURES walkback only after CI is green.
- Investigate flakes in-PR rather than rerunning CI hoping they self-resolve.

## Key Helper Packages

- `internal/cli/output` — output formatters used by tests asserting CLI behavior.
- `internal/logger` — capture-friendly logger.
- `pkg/health` — health-checker doubles for unit tests.
- `pkg/metadata/storetest` + `pkg/blockstore/blockstoretest` — backend conformance suites.

---

*Testing analysis: 2026-05-28*
