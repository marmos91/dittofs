---
phase: 22-snapshot-records-hash-manifest-gc-hold
plan: 01
subsystem: database
tags: [gorm, snapshot, model, controlplane]

requires:
  - phase: 22-context
    provides: D-08 unique partial index, D-09 UUID PK, D-11 field set, D-12 path layout, D-13 plan boundary
provides:
  - Snapshot GORM model with the locked D-11 field set
  - StateCreating / StateReady / StateFailed exported constants
  - SnapshotDir / ManifestPath / MetadataDumpPath path helpers
  - ErrSnapshotNotFound and ErrSnapshotStateConflict sentinels
  - Source-level field-set regression guard
affects: [22-03-snapshot-store, 22-04-manifest, 22-05-cascade-delete, 22-06-gc-hold]

tech-stack:
  added: []
  patterns:
    - "Method-based path helpers (not free functions) on *Snapshot per D-12"
    - "Unique partial index via GORM tag `index:...,where:state='creating',unique` per D-08"
    - "Source-level field-set test as regression guard against accidental renames"

key-files:
  created:
    - pkg/controlplane/models/snapshot.go
    - pkg/controlplane/models/snapshot_test.go
  modified:
    - pkg/controlplane/models/errors.go

key-decisions:
  - "ErrDuplicateSnapshot deliberately omitted per D-13 — uniqueness surfaces as a generic driver error in plan 22-03"
  - "No cascade-delete FK clause on the GORM tag — Share itself has no name-based FK target, and the application-level cleanup is owned by plan 22-05"
  - "GORM partial-index syntax embedded verbatim (`where:state='creating'`); dialect compatibility is plan 22-03's responsibility"
  - "Model is not yet registered in AllModels() — registration lands with the snapshot store in plan 22-03"

patterns-established:
  - "Snapshot path layout: <shareDataDir>/snapshots/<id>/{manifest.hashes,metadata.dump}"
  - "Field-set guard pattern: os.ReadFile the source file and grep for every required identifier"

requirements-completed: [SNAP-01]

duration: ~8min
completed: 2026-05-28
---

# Phase 22 Plan 01: Snapshot Records Foundation Summary

**Snapshot GORM model with UUID PK, unique partial index on in-flight rows, deterministic on-disk path helpers, and two new error sentinels — no store, no migration, data shape only.**

## Performance

- **Duration:** ~8 min
- **Tasks:** 3
- **Files created:** 2
- **Files modified:** 1

## Accomplishments

- `Snapshot` struct exposes exactly the eight D-11 fields with the locked GORM tags (UUID PK, FK on `share_name`, default state `creating`, autoCreate/autoUpdate timestamps)
- Unique partial index `idx_share_creating` enforces at-most-one in-flight snapshot per share at the schema level
- Three method-based path helpers on `*Snapshot` produce deterministic on-disk layouts; literals `manifest.hashes` and `metadata.dump` match the SNAP-02 spec
- Source-level field-set test reads `snapshot.go` and asserts every required field name is present — accidental renames in future refactors fail loudly

## Task Commits

1. **Task 1: Add error sentinels** — `18a59dbd` (feat)
2. **Task 2: Define Snapshot model + state constants + path helpers** — `ea17ebc3` (feat)
3. **Task 3: Unit-test path helpers + lock field set with a grep gate** — `805212f4` (test)

## Files Created/Modified

- `pkg/controlplane/models/snapshot.go` (new) — Snapshot struct, state constants, TableName, three path helpers
- `pkg/controlplane/models/snapshot_test.go` (new) — TestSnapshot_PathHelpers (table-driven), TestSnapshot_StateConstantValues, TestSnapshot_TableName, TestSnapshot_FieldSet (source-level guard)
- `pkg/controlplane/models/errors.go` (modified) — appended `ErrSnapshotNotFound` and `ErrSnapshotStateConflict` under a new `// Snapshot errors` block between the Adapter and Setting groups

## Decisions Made

- Followed plan as written; no design deviations from D-11 or D-12.

## Deviations from Plan

None — plan executed exactly as written. No deviation rules triggered. No new dependencies added (T-22-01-SC verified: `path/filepath`, `time`, `errors` already in go.mod).

## Issues Encountered

None.

## Verification Results

- `go vet ./pkg/controlplane/models/...` — exit 0
- `go build ./pkg/controlplane/...` — exit 0
- `go test ./pkg/controlplane/models/... -run 'TestSnapshot_' -count=1` — 4 tests, all PASS
- `gofmt -l pkg/controlplane/models/snapshot.go pkg/controlplane/models/snapshot_test.go pkg/controlplane/models/errors.go` — no unformatted files
- All Task 2 acceptance grep gates pass (struct, methods, state constants, partial index tag, timestamps)
- All Task 3 acceptance gates pass (4 TestSnapshot_* functions, filepath.Join + os.ReadFile present, no testify import)

## Self-Check: PASSED

- `pkg/controlplane/models/snapshot.go` — FOUND
- `pkg/controlplane/models/snapshot_test.go` — FOUND
- `pkg/controlplane/models/errors.go` — FOUND (modified)
- Commit `18a59dbd` — FOUND
- Commit `ea17ebc3` — FOUND
- Commit `805212f4` — FOUND

## Next Phase Readiness

- Data shape locked. Plan 22-03 can now build `SnapshotStore` (CRUD), register `&Snapshot{}` in `AllModels()`, and verify the partial-index syntax against SQLite/Postgres in conformance tests.
- `ErrSnapshotNotFound` and `ErrSnapshotStateConflict` are exported and ready for the store layer to wrap driver errors.
- Path helpers are ready for the manifest writer (plan 22-04) and GC-hold scanner (plan 22-06) to consume.

---
*Phase: 22-snapshot-records-hash-manifest-gc-hold*
*Plan: 01*
*Completed: 2026-05-28*
