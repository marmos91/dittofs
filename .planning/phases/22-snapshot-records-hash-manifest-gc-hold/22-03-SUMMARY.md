---
phase: 22-snapshot-records-hash-manifest-gc-hold
plan: 03
subsystem: controlplane-store
tags: [gorm, snapshot, store, state-machine, partial-index]

requires:
  - phase: 22-01
    provides: Snapshot model, StateCreating/Ready/Failed constants, ErrSnapshotNotFound + ErrSnapshotStateConflict sentinels
provides:
  - SnapshotStore sub-interface composed into Store
  - GORMStore CRUD implementation (Create/Get/List/Delete/UpdateState)
  - validateStateTransition helper enforcing the three-state machine
  - Post-AutoMigrate fallback that guarantees idx_share_creating partial unique index
  - 9 SQLite integration tests covering round-trip, concurrency, ordering, state-machine
affects: [22-04-manifest, 22-05-cascade-delete, 22-06-gc-hold]

tech-stack:
  added: []
  patterns:
    - "Five-method sub-interface composed into Store between AdminStore and HealthStore"
    - "isUniqueConstraintError + state-aware mapping to ErrSnapshotStateConflict"
    - "validateStateTransition private helper colocated with impl (not interface)"
    - "Post-AutoMigrate CREATE UNIQUE INDEX IF NOT EXISTS fallback as GORM-version-drop guard"
    - "//go:build integration tag honored on the new test file"

key-files:
  created:
    - pkg/controlplane/store/snapshots.go
    - pkg/controlplane/store/snapshots_test.go
  modified:
    - pkg/controlplane/store/interface.go
    - pkg/controlplane/store/gorm.go
    - pkg/controlplane/models/models.go

key-decisions:
  - "Skipped the planned `logger.Debug` line on idx_share_creating creation — the gorm.go imports `gorm.io/gorm/logger` (GORM's logger), not the internal logger, and no other post-migration step emits internal logs; less surface, no behavior change"
  - "Tests gated with `//go:build integration` to match the established pattern in store_test.go / block_test.go / adapter_settings_test.go"
  - "validateStateTransition lives in snapshots.go as a private function, not on the interface — state-machine logic stays colocated with the impl"

requirements-completed: [SNAP-05]

duration: ~15min
completed: 2026-05-28
---

# Phase 22 Plan 03: SnapshotStore CRUD Summary

**Control plane CRUD surface for snapshots: SnapshotStore sub-interface, GORMStore impl with state-machine guard, AllModels registration, and a post-AutoMigrate fallback that guarantees the idx_share_creating partial unique index across dialects.**

## Performance

- **Duration:** ~15 min
- **Tasks:** 3
- **Files created:** 2 (`snapshots.go`, `snapshots_test.go`)
- **Files modified:** 3 (`interface.go`, `gorm.go`, `models/models.go`)
- **Tests added:** 9 `TestSnapshot_*` functions, all passing under `-race`

## Accomplishments

### Interface surface (Task 1, commit `499a2c74`)
- `SnapshotStore` declares exactly the 5 methods from D-13 (`CreateSnapshot`, `GetSnapshot`, `ListSnapshots`, `DeleteSnapshot`, `UpdateSnapshotState`)
- Composed into the top-level `Store` interface immediately after `AdminStore` and before `HealthStore` — matches the lifecycle/admin-adjacent positioning of snapshot operations
- `models.AllModels()` now includes `&Snapshot{}` between `&Share{}` and `&ShareAccessRule{}`, so `db.AutoMigrate` creates the `snapshots` table
- Per-method docstrings explicitly cite D-13/D-14 (`ListSnapshots returns ALL states; callers filter`) and the state-machine for `UpdateSnapshotState`

### GORM implementation (Task 2, commit `a3ee2316`)
- `CreateSnapshot` generates a UUID if `snap.ID == ""`, defaults `snap.State` to `creating`, and runs inside `s.db.WithContext(ctx).Transaction`. A unique-constraint violation on a `creating` insert is mapped to `models.ErrSnapshotStateConflict`; other unique violations bubble up raw
- `GetSnapshot` uses the composite `share_name = ? AND id = ?` WHERE clause directly (helpers.go's `getByField` is single-field-only); converts `gorm.ErrRecordNotFound` to `models.ErrSnapshotNotFound` via the existing `convertNotFoundError` helper
- `ListSnapshots` runs `Where("share_name = ?", shareName).Order("created_at DESC")` — no state filter (D-14)
- `DeleteSnapshot` is a transactional `(share_name, id)` delete; `RowsAffected == 0` → `models.ErrSnapshotNotFound`. Filesystem cleanup deferred to plan 22-05's `Runtime.RemoveShare` hook (D-15)
- `UpdateSnapshotState` loads the row inside a transaction, calls `validateStateTransition`, and updates only the `state` + `updated_at` columns on a valid transition

### State machine table
The private `validateStateTransition(current, next string)` helper accepts exactly:

| from       | to         | result |
| ---------- | ---------- | ------ |
| `creating` | `ready`    | nil    |
| `creating` | `failed`   | nil    |
| `failed`   | `creating` | nil    |
| anything else (incl. same-state) | | `ErrSnapshotStateConflict` |

### idx_share_creating fallback
Inside `New(...)`, after the post-migration column drops and before `&GORMStore{...}` construction:

```go
if err := db.Exec(
    "CREATE UNIQUE INDEX IF NOT EXISTS idx_share_creating ON snapshots(share_name) WHERE state = 'creating'",
).Error; err != nil {
    return nil, fmt.Errorf("failed to ensure idx_share_creating: %w", err)
}
```

Both SQLite and PostgreSQL support `CREATE UNIQUE INDEX ... WHERE` and `IF NOT EXISTS`, so the same statement is dialect-portable and idempotent. The wrapper `if err != nil { return nil, ... }` ensures startup never silently proceeds without the uniqueness guard — that would defeat T-22-03-03's mitigation.

### Tests (Task 3, commit `ceb4fd95`)
- `TestSnapshot_CreateGetList_RoundTrip` — 3 inserts (1 creating + 2 ready) with 1ms sleep between to make `created_at DESC` deterministic
- `TestSnapshot_CreateConcurrent_RejectsSecondCreating` — second `creating` for same share returns `ErrSnapshotStateConflict`
- `TestSnapshot_CreateMultiple_ReadyAllowed` — 5 ready snapshots per share succeed (D-08)
- `TestSnapshot_Get_NotFound_ReturnsSentinel` — sentinel surfaces on miss
- `TestSnapshot_Delete_RemovesRow` — second delete returns sentinel
- `TestSnapshot_UpdateState_AllowedTransitions` — table-driven 3-row, sub-tests per case
- `TestSnapshot_UpdateState_RejectsDisallowed` — table-driven 7-row (rejected transitions + unknown target); also asserts state did NOT change post-rejection
- `TestSnapshot_UpdateState_NotFound` — sentinel on missing id
- `TestSnapshot_idxShareCreating_ExistsAfterMigrate` — `sqlite_master` probe verifies the fallback installed the partial index

## Task Commits

1. **Task 1: SnapshotStore sub-interface + AllModels registration** — `499a2c74` (feat)
2. **Task 2: GORM impl + state-machine + idx_share_creating fallback** — `a3ee2316` (feat)
3. **Task 3: 9 SQLite integration tests** — `ceb4fd95` (test)

## Files Created/Modified

- `pkg/controlplane/store/snapshots.go` (new) — 5 methods on `*GORMStore` + `validateStateTransition` helper
- `pkg/controlplane/store/snapshots_test.go` (new) — 9 `TestSnapshot_*` functions under `//go:build integration`
- `pkg/controlplane/store/interface.go` (modified) — `SnapshotStore` interface declaration + `Store` composite update
- `pkg/controlplane/store/gorm.go` (modified) — post-AutoMigrate `CREATE UNIQUE INDEX IF NOT EXISTS idx_share_creating ...`
- `pkg/controlplane/models/models.go` (modified) — `&Snapshot{}` added to `AllModels()`

## Decisions Made

- **Skipped the planned `logger.Debug` line** on idx_share_creating creation. Rationale: `gorm.go` imports `gorm.io/gorm/logger` (GORM's logger type used by `gormConfig.Logger`), not the internal `github.com/marmos91/dittofs/internal/logger`. None of the other post-migration steps log either. Adding a new internal-logger import for one debug line is net surface against the Less-Is-More invariant; behavior is identical without it.
- **`//go:build integration` tag** on `snapshots_test.go` — mirrors the pattern in every other `*_test.go` under `pkg/controlplane/store/` that exercises a real DB. Tests still run by default via `go test -tags=integration`, which is how the existing CI invokes the suite.
- **No new helper in `helpers.go`** — the composite-field WHERE for `GetSnapshot` / `DeleteSnapshot` is too narrow for a generic helper. Inline `Where(...)` calls match `shares.go`'s style.

## Deviations from Plan

- **Rule 2-ish: log line omission** — The plan said "Emit a `logger.Debug` line". Skipped because the `store` package never imports the internal logger and the precedent in gorm.go is "silent post-migrations, fail loudly on error". Net effect: identical (the wrapped error still surfaces on failure). No behavior change.
- **No new dependencies added.** T-22-03-SC verified: `gorm.io/gorm`, `github.com/google/uuid`, `errors`, `time`, `context` are all already in go.mod. Package Legitimacy Gate not triggered.

## Issues Encountered

None — every test passed on first run, build and vet are clean throughout, no race conditions surfaced under `-race`.

## Verification Results

- `go build ./pkg/controlplane/...` — exit 0
- `go vet ./pkg/controlplane/...` — exit 0
- `go test -tags=integration ./pkg/controlplane/store/... -run 'TestSnapshot_' -count=1 -race` — exit 0, 9 tests + sub-tests PASS in 3.063s
- `go test -tags=integration ./pkg/controlplane/store/... -count=1` — exit 0 (full store suite still green; no regressions)
- `gofmt -l pkg/controlplane/store/{snapshots.go,snapshots_test.go,interface.go,gorm.go} pkg/controlplane/models/models.go` — no unformatted files
- All Task 1 + Task 2 grep acceptance gates pass
- `grep -c '^func TestSnapshot_' pkg/controlplane/store/snapshots_test.go` returns `9` (>= 8 required)

## Known Stubs

None.

## Threat Flags

None — no new network endpoints, auth paths, or trust-boundary surface introduced. All SQL is parameterized; state strings are validated against an exhaustive whitelist in `validateStateTransition`.

## Self-Check: PASSED

- `pkg/controlplane/store/snapshots.go` — FOUND
- `pkg/controlplane/store/snapshots_test.go` — FOUND
- `pkg/controlplane/store/interface.go` — FOUND (modified, `type SnapshotStore interface` present)
- `pkg/controlplane/store/gorm.go` — FOUND (modified, `idx_share_creating` present)
- `pkg/controlplane/models/models.go` — FOUND (modified, `&Snapshot{}` present)
- Commit `499a2c74` — FOUND
- Commit `a3ee2316` — FOUND
- Commit `ceb4fd95` — FOUND

## Next Phase Readiness

- Plan 22-04 (hash manifest writer) can now persist snapshot rows via `CreateSnapshot` and update lifecycle state via `UpdateSnapshotState` — the `creating -> ready` transition gates manifest visibility for `HoldProvider`
- Plan 22-05 (cascade-delete + Runtime hook) consumes `ListSnapshots` + `DeleteSnapshot` and adds the `os.RemoveAll(<share-data-dir>/snapshots/)` step in `Runtime.RemoveShare` before the DB cascade per D-15
- Plan 22-06 (GC-hold scanner) uses `ListSnapshots` filtered to `state='ready'` (filter applied by `HoldProvider`, not the store, per D-14)

---
*Phase: 22-snapshot-records-hash-manifest-gc-hold*
*Plan: 03*
*Completed: 2026-05-28*
