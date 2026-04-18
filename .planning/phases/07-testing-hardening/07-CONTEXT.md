# Phase 7: Testing & Hardening - Context

**Gathered:** 2026-04-17
**Status:** Ready for planning

<domain>
## Phase Boundary

Validate every failure mode that could silently corrupt or lose data in
production backup systems before v0.13.0 ships. No new business logic —
this phase tests what Phases 1–6 built.

Specifically:

1. **Localstack-backed E2E matrix** — happy path × 3 engines (memory,
   BadgerDB, PostgreSQL) × 2 destinations (local FS + S3) with real
   multipart uploads. Lives in `test/e2e/`.

2. **Corruption tests** — table-driven suite in `pkg/backup/destination/`
   (integration build tag) covering: truncated archive, bit-flip in
   payload, missing manifest, wrong store_id, manifest_version:2. Each
   vector asserts a specific sentinel error with no panic.

3. **Chaos tests** — kill-mid-backup and kill-mid-restore via
   `helpers.ForceKill()` on a real `dfs` process in `test/e2e/`. Verifies:
   no ghost multipart uploads, SAFETY-02 orphan recovery (running →
   interrupted on restart), system left in a recoverable state.

4. **Cross-version gating** — synthetic `manifest_version: 2` test verifying
   restore returns `ErrManifestVersionUnsupported` (not a panic or silent
   corruption). Covers SAFETY-03 forward-compat contract without old binaries.

5. **Restore-while-mounted** — rejected with 409 in CI; concurrent-write +
   backup + restore byte-compare passes.

**Out of scope:**
- Old-binary restore tests (no old-binary CI step)
- New runtime logic, API endpoints, or CLI commands
- pjdfstest POSIX compliance
- Prometheus metrics (OBS-01/OBS-02 deferred)

</domain>

<decisions>
## Implementation Decisions

### Test Placement

- **D-01 — Split across two layers.**
  - `pkg/backup/` (integration build tag `//go:build integration`): corruption
    suite, manifest version gating, concurrent-write + backup + restore
    byte-compare. No server process, no sudo, fast.
  - `test/e2e/` (build tag `//go:build e2e`): Localstack E2E matrix, chaos
    tests (kill-mid-backup, kill-mid-restore), restore-while-mounted rejection.
    Real `dfs` process, real CLI/REST surface, Docker for Localstack.

  Rationale: mirrors how Phase 3 destination integration tests are already
  split from E2E. Corruption micro-tests don't need a full server; chaos
  tests must exercise real DB orphan recovery.

### Chaos Test Mechanism

- **D-02 — Process kill via `helpers.ForceKill()` in `test/e2e/`.**
  Use `helpers.StartServerProcess` + `server.ForceKill()` at a timed point
  mid-run (SIGKILL, not graceful shutdown). Matches the existing E2E chaos
  pattern. Ensures:
  - Ghost MPU cleanup by S3 destination orphan sweep (Phase 3 DRV-02 path)
  - SAFETY-02: on next `dfs start`, `running` jobs transition to `interrupted`
  - Real DB state persisted across the kill/restart cycle

  No context-cancel layer in integration tests — process kill is the sole
  chaos mechanism.

- **D-03 — Mid-run kill timing: sleep-then-kill after triggering the job.**
  Start server, trigger `dfs backup` (or restore) via REST/CLI, sleep a
  fixed duration (e.g. 500ms for a large enough test payload to still be
  in-flight), ForceKill. Restart server, assert recovery. Payload must be
  large enough that a 500ms window reliably hits mid-upload; researcher picks
  the size that makes the test deterministic on CI.

### Cross-Version Test Scope

- **D-04 — Manifest version gating only; no old-binary tests.**
  Synthetic test: construct a `manifest.Manifest{Version: 2}`, serialize to
  YAML, write as a valid-looking archive in a temp destination, call
  `restore.Executor` or `Destination.GetManifestOnly`. Assert the returned
  error wraps or is `ErrManifestVersionUnsupported` (or equivalent sentinel
  from `pkg/backup/manifest`). No old dfs binaries, no two-build CI step.
  Covers SAFETY-03 forward-compat.

### Corruption Test Vectors

- **D-05 — Table-driven suite in `pkg/backup/destination/` (integration).**
  Single `TestCorruption` table in a new file (e.g.
  `pkg/backup/destination/corruption_test.go`, build tag `integration`).
  Rows:

  | Vector | Injection method | Expected error |
  |--------|-----------------|----------------|
  | Truncated archive | write partial bytes, close early | io.ErrUnexpectedEOF or ErrChecksumMismatch |
  | Bit-flip in payload | read archive, flip one byte, write back | ErrChecksumMismatch (SHA-256 mismatch) |
  | Missing manifest | upload payload without manifest file | ErrManifestNotFound or ErrNoRestoreCandidate |
  | Wrong store_id | manifest.store_id ≠ target store GetStoreID() | ErrStoreIdMismatch |
  | manifest_version: 2 | set Version=2 in manifest YAML | ErrManifestVersionUnsupported |

  Run against both local FS and S3 destinations to ensure error paths are
  consistent across drivers. Use the shared Localstack helper (no per-test
  containers).

- **D-06 — Corruption suite does NOT extend `backup_conformance.go`.**
  Conformance suite (`pkg/metadata/storetest/`) covers engine-level semantics
  (RoundTrip, ConcurrentWriter, NonEmptyDest). Corruption vectors are
  destination-driver concerns and don't belong in the engine conformance suite.
  Keeping them separate avoids bloating the suite with destination stubs.

### E2E Matrix Structure

- **D-07 — Matrix: 3 engines × 2 destinations = 6 sub-tests in `test/e2e/`.**
  File: `test/e2e/backup_matrix_test.go` (build tag `e2e`).
  Each sub-test: create test data, trigger backup via REST, verify record,
  restore to fresh store, byte-compare. S3 sub-tests require Localstack
  (shared container per test binary — existing `e2e/framework/containers.go`
  pattern).

- **D-08 — Localstack uses shared-container helper, not per-test containers.**
  Already established in `pkg/backup/destination/s3/localstack_helper_test.go`.
  The E2E matrix reuses the same `TestMain` + singleton pattern. Avoids
  Docker contention / exit-245 flakes (documented in MEMORY.md).

### Restore-While-Mounted

- **D-09 — Restore-while-mounted test in `test/e2e/`.**
  Start server, create share, leave share enabled, call
  `POST /api/v1/store/metadata/{name}/restore` via REST. Assert 409 Conflict
  with `enabled_shares` in the response body (Phase 5 D-26 / Phase 6 D-29
  hard-409). No NFS/SMB mount required — the share's `Enabled=true` flag is
  sufficient to trigger the precondition check.

### Claude's Discretion

- Exact test file names within the two layers — planner decides based on
  existing naming conventions (`backup_matrix_test.go`, `backup_chaos_test.go`,
  `backup_corruption_integration_test.go`).
- Payload size for mid-kill determinism (D-03) — researcher picks a size that
  reliably takes > 500ms to upload to Localstack on CI.
- Whether the manifest-version-gating test lives in `pkg/backup/manifest/`
  or `pkg/backup/destination/` — whichever package owns the version check.
- Whether to assert ghost MPU cleanup via `s3.ListMultipartUploads` directly
  (exact) or via re-running a backup and confirming it succeeds (indirect).
  Planner picks the approach that works reliably without race conditions.
- Number of concurrent writes in the byte-compare test — match Phase 2
  ConcurrentWriter default (100ms window, 100ms goroutines).

</decisions>

<canonical_refs>
## Canonical References

**Downstream agents MUST read these before planning or implementing.**

### Phase 1–6 contracts (binding)
- `.planning/phases/01-foundations-models-manifest-capability-interface/01-CONTEXT.md` — BackupRepo/BackupRecord/BackupJob schemas, manifest v1, BackupStore sub-interface
- `.planning/phases/02-per-engine-backup-drivers/02-CONTEXT.md` — Backupable interface per engine; backup_conformance.go sub-test names
- `.planning/phases/03-destination-drivers-encryption/03-CONTEXT.md` — Destination two-phase commit, SHA-256 streaming, GetManifestOnly, orphan sweep
- `.planning/phases/04-scheduler-retention/04-CONTEXT.md` — RunBackup entrypoint, overlap guard, job lifecycle (running → succeeded/failed/interrupted)
- `.planning/phases/05-restore-orchestration-safety-rails/05-CONTEXT.md` — SAFETY-02 orphan recovery on startup, ErrRestorePreconditionFailed (409), share.Enabled precondition
- `.planning/phases/06-cli-rest-api-surface/06-CONTEXT.md` — REST endpoint shapes for backup/restore/job polling, D-29 hard-409 enabled_shares body

### Existing test infrastructure
- `pkg/backup/destination/s3/localstack_helper_test.go` — shared-container singleton pattern; TestMain setup; LOCALSTACK_ENDPOINT env var for external Localstack
- `pkg/metadata/storetest/backup_conformance.go` — Phase 2 conformance suite (RoundTrip, ConcurrentWriter, Corruption, NonEmptyDest, PayloadIDSet)
- `test/e2e/helpers/` — StartServerProcess, ForceKill, LoginAsAdmin, UniqueTestName, matrix helpers
- `test/e2e/framework/containers.go` — existing Localstack container lifecycle in E2E framework

### Requirements
- `.planning/REQUIREMENTS.md` §Safety — SAFETY-01, SAFETY-02, SAFETY-03 (the safety rails Phase 7 must prove work)
- `.planning/REQUIREMENTS.md` §Drivers — DRV-02 (S3 two-phase commit, ghost MPU prevention)
- `.planning/REQUIREMENTS.md` §Engines — ENG-01, ENG-02 (BadgerDB + Postgres native snapshot semantics that round-trip must validate)

### No external specs
Phase 7 is validation-only. No new external ADRs — all contracts are in the
phase-level CONTEXT.md files above.

</canonical_refs>

<code_context>
## Existing Code Insights

### Reusable Assets
- `pkg/backup/destination/s3/localstack_helper_test.go` — `localstackHelper` singleton, `createBucket`, `sharedHelper` var; Phase 7 S3 corruption + E2E matrix tests can import the same pattern
- `test/e2e/helpers/server.go` — `StartServerProcess`, `ForceKill`, `StopGracefully`; chaos tests call these directly
- `test/e2e/helpers/cli.go` — `LoginAsAdmin`, REST client helpers; E2E matrix exercises CLI/REST surface
- `pkg/metadata/storetest/backup_conformance.go` — `RunBackupConformanceSuite`, `BackupStoreFactory`; corruption suite is a parallel file not an extension
- `test/e2e/store_matrix_test.go` — existing matrix pattern for store × config combinations; `backup_matrix_test.go` follows same structure

### Established Patterns
- `//go:build integration` for pkg-level tests requiring Docker/external services
- `//go:build e2e` for full server process tests in `test/e2e/`
- `TestMain` with shared container singleton per binary invocation (established in s3 destination package)
- Table-driven sub-tests via `t.Run(row.name, func(t *testing.T){...})` — uniform across existing test files
- `helpers.UniqueTestName` for test isolation — every test uses distinct store/repo/bucket names

### Integration Points
- Phase 7 E2E matrix triggers jobs via `POST /api/v1/store/metadata/{name}/backups` (Phase 6) and polls via `GET backup-jobs/{id}` — no direct runtime calls
- SAFETY-02 recovery tested by restarting the `dfs` process after ForceKill and checking that `GET backup-jobs/{id}` returns `status=interrupted`
- S3 ghost MPU test: call `s3.ListMultipartUploads` on the shared Localstack client after chaos kill + restart

</code_context>

<specifics>
## Specific Ideas

- **Split placement was the key decision.** Corruption micro-tests (bit-flip,
  truncation) don't justify a full server process — running them as integration
  tests in `pkg/backup/destination/` keeps CI feedback fast. Only tests that
  exercise SAFETY-02 DB recovery or the CLI/REST surface need to be in
  `test/e2e/`.

- **Process kill is the only chaos mechanism.** Context-cancel in integration
  tests was explicitly rejected — it misses DB orphan recovery (SAFETY-02)
  and S3 MPU cleanup, which are the two failure modes that matter in production.

- **Manifest version gating is synthetic.** No old-binary CI step. The
  forward-compat contract (SAFETY-03) is verified by constructing a
  `Version: 2` manifest directly and asserting the right sentinel error.

- **Table-driven corruption vectors, not conformance extension.** Keeping
  corruption tests in `pkg/backup/destination/` separate from
  `backup_conformance.go` avoids polluting the engine-level conformance suite
  with destination-driver stubs.

</specifics>

<deferred>
## Deferred Ideas

- **Old-binary restore test** — backward compat across a commit boundary.
  Deferred: high CI complexity, brittle on schema changes. The
  manifest_version gating test covers the forward-compat contract.
- **Context-cancel chaos layer** — executor-level chaos in integration tests.
  Deferred: process kill in E2E is sufficient; context-cancel adds test count
  without covering new failure modes.
- **Prometheus metrics for backup operations** (OBS-01) — deferred from
  v0.13.0 per REQUIREMENTS.md future requirements.
- **Automatic test-restore job** (AUTO-01) — deferred.

### Reviewed Todos (not folded)

None — no pending todos matched Phase 7 scope.

</deferred>

---

*Phase: 07-testing-hardening*
*Context gathered: 2026-04-17*
