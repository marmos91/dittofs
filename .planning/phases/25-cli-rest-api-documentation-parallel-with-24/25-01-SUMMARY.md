---
phase: 25-cli-rest-api-documentation-parallel-with-24
plan: 01
subsystem: api
tags: [snapshot, runtime, dto, rename, verify-gate]

requires:
  - phase: 22-snapshot-records-hash-manifest-gc-hold
    provides: Snapshot model, SnapshotStore CRUD, manifest I/O, SnapshotDir/ManifestPath/MetadataDumpPath
  - phase: 23-snapshot-create-orchestration-sync-gate
    provides: Runtime.CreateSnapshot, WaitForSnapshot, NoSyncGate (renamed here to NoVerify), VerifyRemoteDurability
  - phase: 24-restore-flow
    provides: Runtime.RestoreSnapshot, RestoreSnapshotOpts, 7 restore error sentinels

provides:
  - "CreateSnapshotOpts.NoVerify (was NoSyncGate)"
  - "Runtime.RestoreSnapshot returning (safetySnapshotID string, err error)"
  - "Runtime.GetSnapshot / ListSnapshots / DeleteSnapshot wrappers on *Runtime"
  - "pkg/controlplane/api/dto wire DTO package (Snapshot, CreateSnapshotRequest/Response, RestoreSnapshotRequest/Response)"
  - "Hardcoded verify concurrency = 16 at both call sites; snapshot.sync_gate_concurrency YAML knob removed"

affects:
  - 25-02-PLAN (REST handlers — consume DTO, Runtime wrappers, RestoreSnapshot safety ID return)
  - 25-03-PLAN (apiclient + CLI — consume DTO, --no-verify flag, Safety snap surfacing)
  - 25-04-PLAN (docs — operator vocabulary)

tech-stack:
  added: []
  patterns:
    - "Wire DTO package (pkg/controlplane/api/dto) decoupled from GORM models — single source of truth for handler + apiclient"
    - "Runtime read/delete wrappers as testability seam (mirrors BlockGCRuntime in block_gc.go)"

key-files:
  created:
    - pkg/controlplane/api/dto/snapshot.go
    - pkg/snapshot/verify.go (renamed from syncgate.go)
    - pkg/snapshot/verify_test.go (renamed from syncgate_test.go)
  modified:
    - pkg/controlplane/runtime/snapshot.go (rename, new wrappers, extended RestoreSnapshot)
    - pkg/controlplane/runtime/restore_opts.go (godoc only)
    - pkg/controlplane/runtime/runtime.go (delete defaultSyncGateConcurrency; SnapshotDefaults becomes empty)
    - pkg/controlplane/runtime/snapshot_test.go (new TDD tests for wrappers)
    - pkg/controlplane/runtime/snapshot_restore_test.go (all RestoreSnapshot call sites updated; safety-ID assertion in happy path)
    - pkg/controlplane/runtime/snapshot_integration_test.go (testNoSyncGate → testNoVerify)
    - pkg/config/config.go (SnapshotConfig now empty placeholder)
    - pkg/config/snapshot_test.go (deleted sync_gate_concurrency cases; left TestSnapshotConfigEmpty)
    - cmd/dfs/commands/start.go (drop SyncGateConcurrency wiring)

key-decisions:
  - "DeleteSnapshot rejects snapID containing / or \\ as ErrSnapshotNotFound before any store or filesystem touch (defense-in-depth for T-25-01-01)"
  - "DeleteSnapshot treats shares.ErrShareNotFound and empty localStoreDir as success after store-row delete (memory-only or removed share has no dir to wipe)"
  - "ListSnapshots returns []*models.Snapshot{} (not nil) on empty so JSON encodes [] not null"
  - "RestoreSnapshot named return surfaces safetySnapshotID on every failure path AFTER the safety snap was created (callers can trigger rollback); empty string for precheck/pre-verify failures"
  - "SnapshotConfig + SnapshotDefaults kept as empty structs (placeholders) rather than deleted — Plan 25-02 will extend SnapshotConfig with RestoreHTTPTimeout"

patterns-established:
  - "Neutral wire-DTO package (pkg/controlplane/api/dto): no DittoFS dependencies, both handlers and apiclient import it directly — eliminates duplication risk between handler-side and client-side DTOs"

requirements-completed: []

# Metrics
duration: ~30min
completed: 2026-05-29
---

# Phase 25 Plan 01: Cross-cutting rename + runtime wrappers + shared wire DTO

**Renamed sync-gate → verify across the snapshot stack, added Runtime.{Get,List,Delete}Snapshot wrappers, extended RestoreSnapshot to surface the pre-restore safety snap ID, and introduced pkg/controlplane/api/dto as the neutral wire contract for Waves 2/3.**

## Performance

- **Duration:** ~30 min
- **Tasks:** 2
- **Files modified:** 12 (1 created, 2 renamed, 9 modified)

## Accomplishments
- Vocabulary single-sourced: no `sync_gate` / `SyncGate` / `NoSyncGate` / `sync_gate_concurrency` identifiers remain in `*.go`/`*.yaml`/`*.yml` anywhere in the repo. `--no-verify` (Plan 25-03) and `docs/SNAPSHOTS.md` (Plan 25-04) will map cleanly to `CreateSnapshotOpts.NoVerify`.
- `Runtime.RestoreSnapshot` signature changed from `error` to `(safetySnapshotID string, err error)`. Wave-2 REST handler (Plan 25-02) and Wave-2 CLI (Plan 25-03) consume the safety snap ID deterministically — no brittle `ListSnapshots` filter.
- Three new Runtime read/delete wrappers (`GetSnapshot`, `ListSnapshots`, `DeleteSnapshot`) exercised by unit tests. Plan 25-02 handlers will declare a narrow `SnapshotRuntime` interface satisfied by `*Runtime`.
- `pkg/controlplane/api/dto/snapshot.go` is the single source of truth for the wire shape — Plan 25-02 (handler) and Plan 25-03 (apiclient) both import it without circular dependency.

## Task Commits

1. **Task 1: Rename sync_gate → verify** — `4f1f98bf` (refactor)
2. **Task 2: Runtime wrappers + RestoreSnapshot signature + wire DTO** — `cfd12338` (feat)

## Files Created/Modified

**Created:**
- `pkg/controlplane/api/dto/snapshot.go` — neutral wire-DTO package (5 types): `Snapshot`, `CreateSnapshotRequest`, `CreateSnapshotResponse`, `RestoreSnapshotRequest`, `RestoreSnapshotResponse`. Depends only on `time` from stdlib.

**Renamed (git mv):**
- `pkg/snapshot/syncgate.go` → `pkg/snapshot/verify.go` — `VerifyRemoteDurability` (no symbol change; log fields rewritten "snapshot sync gate" → "snapshot verify")
- `pkg/snapshot/syncgate_test.go` → `pkg/snapshot/verify_test.go` — unchanged tests, file rename only

**Modified:**
- `pkg/controlplane/runtime/snapshot.go` — `NoSyncGate` → `NoVerify`; `no_sync_gate` log field → `no_verify`; "Step 3: NoSyncGate short-circuit" → "Step 3: NoVerify short-circuit"; both `r.snapshotDefaults().SyncGateConcurrency` call sites (lines 537 and 932 in the post-rename file) hardcoded to `concurrency := 16`; `RestoreSnapshot` signature extended; new `GetSnapshot` / `ListSnapshots` / `DeleteSnapshot` wrappers added below.
- `pkg/controlplane/runtime/restore_opts.go` — godoc comment updated (`NoSyncGate` → `NoVerify`).
- `pkg/controlplane/runtime/runtime.go` — deleted `defaultSyncGateConcurrency` constant; `SnapshotDefaults` becomes empty struct; `SetSnapshotDefaults` simplified; `snapshotDefaults()` accessor removed.
- `pkg/controlplane/runtime/snapshot_test.go` — appended 5 new tests: `TestRuntimeGetSnapshot_NotFound`, `TestRuntimeListSnapshots_Empty`, `TestRuntimeDeleteSnapshot_HappyPath` (verifies row delete + on-disk dir wipe + lock release), `TestRuntimeDeleteSnapshot_NotFound`, `TestRuntimeDeleteSnapshot_RejectsPathTraversal`.
- `pkg/controlplane/runtime/snapshot_restore_test.go` — 10 `RestoreSnapshot` call sites updated to the 2-return signature; happy-path test asserts `safetyID != ""`. Updated call sites: lines 77, 145, 164, 202, 236, 274, 316, 380, 436, 459 (post-rename).
- `pkg/controlplane/runtime/snapshot_integration_test.go` — `testNoSyncGate` → `testNoVerify`; sub-test label `"NoSyncGate"` → `"NoVerify"`; remote endpoint test ID `"remote-nosg"` → `"remote-nv"`.
- `pkg/config/config.go` — `SnapshotConfig` becomes empty struct; `ApplyDefaults` / `Validate` collapse to no-ops.
- `pkg/config/snapshot_test.go` — file replaced with `TestSnapshotConfigEmpty` placeholder.
- `cmd/dfs/commands/start.go` — `SetSnapshotDefaults` call site no longer passes `SyncGateConcurrency`; godoc trimmed.

## Two call-site lines where concurrency is now hardcoded to 16

- `pkg/controlplane/runtime/snapshot.go:537` (Step 5 verify in `runSnapshotOrchestration`)
- `pkg/controlplane/runtime/snapshot.go:932` (pre-verify in `RestoreSnapshot`)

Both annotated: `// Hardcoded; benchmarking confirmed no operator tuning need.`

## New Runtime method signatures

```go
func (r *Runtime) GetSnapshot(ctx context.Context, share, snapID string) (*models.Snapshot, error)
func (r *Runtime) ListSnapshots(ctx context.Context, share string) ([]*models.Snapshot, error)
func (r *Runtime) DeleteSnapshot(ctx context.Context, share, snapID string) error
```

## New RestoreSnapshot signature

```go
func (r *Runtime) RestoreSnapshot(
    ctx context.Context,
    shareName, snapID string,
    opts RestoreSnapshotOpts,
) (safetySnapshotID string, err error)
```

`safetySnapshotID` is `""` on precheck / pre-verify failure paths, non-empty for every subsequent failure path and on success.

## RestoreSnapshot call sites updated

All in `pkg/controlplane/runtime/snapshot_restore_test.go` (post-rename line numbers):

| Line | Test | New form |
|------|------|----------|
| 77   | `testRestoreHappyPath`        | `safetyID, err := fx.rt.RestoreSnapshot(...)` + assertion `safetyID != ""` |
| 145  | `testRestoreEnabledShareRefuses` | `_, err = fx.rt.RestoreSnapshot(...)` |
| 164  | `testRestoreSnapshotNotFound` | `_, err := fx.rt.RestoreSnapshot(...)` |
| 202  | `testRestoreSnapshotNotReady` | `_, err = fx.rt.RestoreSnapshot(...)` |
| 236  | `testRestoreNonDurableRefused` | `_, err = fx.rt.RestoreSnapshot(...)` |
| 274  | `testRestoreAllowNonDurable`  | `if _, err := fx.rt.RestoreSnapshot(...); err != nil` |
| 316  | `testRestorePreVerifyFails`   | `_, err = fx.rt.RestoreSnapshot(...)` |
| 380  | `testRestorePostVerifyFails`  | `_, err = fx.rt.RestoreSnapshot(...)` |
| 436  | `testRestoreInterruptedReset` | `_, err = fx.rt.RestoreSnapshot(...)` |
| 459  | `testRestoreInterruptedReset` (recovery) | `if _, err := fx.rt.RestoreSnapshot(...); err != nil` |

## Decisions Made

See `key-decisions` in frontmatter. The notable ones:

- **DeleteSnapshot path-traversal defense** (T-25-01-01 mitigation): `strings.ContainsAny(snapID, "/\\")` rejects malformed input as `ErrSnapshotNotFound` before any store or filesystem touch.
- **DeleteSnapshot share-removed tolerance**: After successful row delete, `shares.ErrShareNotFound` or empty `localStoreDir` is logged and treated as success (no dir to wipe). Other lookup errors propagate wrapped.
- **DeleteSnapshot lock pattern**: Goes through `r.snapshotDeleteLock(share)` directly (same per-share `*sync.RWMutex` `CreateSnapshot`/`SnapshotHoldProvider` use). The CONTEXT D-25-17 noted either lock-acquisition path is acceptable; chose the direct one for symmetry with `snapshotHoldForRemote`.
- **ListSnapshots non-nil empty slice**: JSON-encoding contract — `[]` rather than `null` for empty share. Eliminates client-side null-check.

## Deviations from Plan

None - plan executed exactly as written.

Notes on plan literal vs reality:

- The plan's CONTEXT mentioned removing `snapshotDefaults()` "if other code paths reference it; otherwise delete." There were no remaining references after the rename pass (both call sites in `snapshot.go` were replaced with the literal `16`), so the accessor was deleted.
- `SnapshotConfig` and `SnapshotDefaults` are kept as empty structs per plan (Plan 25-02 will add `RestoreHTTPTimeout`).

## Issues Encountered

None. Pre-existing GSD metadata markers (`D-23-XX`, `Phase 22`, `D-24-08`, etc.) remain in `pkg/controlplane/runtime/snapshot.go` godoc/comments that this plan did not touch — those are out of scope for this plan's edits per `<done>` criterion 5 ("no GSD metadata in this plan's edits"). A repo-wide cleanup pass is a separate concern.

## User Setup Required

None.

## Next Phase Readiness

Wave 2 (Plans 25-02, 25-03, 25-04) can now run **in parallel**:

- **Plan 25-02** (REST handler + router): can declare `SnapshotRuntime` interface satisfied by `*Runtime` without further changes; can import `pkg/controlplane/api/dto`; can call `Runtime.RestoreSnapshot` and write `safetySnapshotID` into `dto.RestoreSnapshotResponse.SafetySnapshotID` directly.
- **Plan 25-03** (apiclient + CLI): can import `pkg/controlplane/api/dto` for the wire types; CLI `--no-verify` flag maps to `dto.CreateSnapshotRequest.NoVerify`.
- **Plan 25-04** (docs): operator vocabulary already aligned ("verify gate" everywhere).

## Self-Check: PASSED

- `pkg/controlplane/api/dto/snapshot.go` — FOUND
- `pkg/snapshot/verify.go` — FOUND
- `pkg/snapshot/verify_test.go` — FOUND
- `pkg/snapshot/syncgate.go` — MISSING (as required)
- `pkg/snapshot/syncgate_test.go` — MISSING (as required)
- Commit `4f1f98bf` (Task 1) — FOUND in git log
- Commit `cfd12338` (Task 2) — FOUND in git log
- `grep -rEn "(sync_gate|SyncGate|NoSyncGate|sync_gate_concurrency)"` across `*.go`/`*.yaml`/`*.yml` — 0 matches
- `go build ./...` exit 0
- `go vet ./...` exit 0
- `go test ./pkg/snapshot/... ./pkg/controlplane/runtime/... ./pkg/controlplane/api/dto/... ./pkg/config/...` all pass
- `gofmt -s -l .` clean

---
*Phase: 25-cli-rest-api-documentation-parallel-with-24*
*Completed: 2026-05-29*
