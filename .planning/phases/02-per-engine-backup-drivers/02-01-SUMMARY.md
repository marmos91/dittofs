---
phase: 02-per-engine-backup-drivers
plan: 01
subsystem: backup
tags: [metadata, backup, restore, conformance, errors, testing, go]

# Dependency graph
requires:
  - phase: 01-foundations-models-manifest-capability-interface
    provides: Backupable interface, PayloadIDSet type, ErrBackupUnsupported sentinel
provides:
  - Four new Phase-2 sentinel errors on pkg/metadata (ErrRestoreDestinationNotEmpty, ErrRestoreCorrupt, ErrSchemaVersionMismatch, ErrBackupAborted)
  - Shared conformance suite entry points (RunBackupConformanceSuite, RunBackupConformanceSuiteWithOptions)
  - Union test-store interface (BackupTestStore = MetadataStore + Backupable + io.Closer) with factory type
  - BackupSuiteOptions for per-engine opt-outs (reserved; no Phase-2 engine uses it)
  - Five subtest helpers: testBackupRoundTrip, testBackupConcurrentWriter, testBackupCorruption, testBackupNonEmptyDest, testBackupPayloadIDSet
affects: [02-02-memory-driver, 02-03-badger-driver, 02-04-postgres-driver, phase-5-restore-orchestration]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Typed sentinel errors wrapped via fmt.Errorf(\"%w: %v\", sentinel, cause) for errors.Is dispatch"
    - "Two-store factory pattern: factory(t) called twice per subtest (source + destination)"
    - "Engine-agnostic conformance suite depending only on pkg/metadata + stdlib"
    - "Deterministic populate helper (2 shares x 3 dirs x 2 files = 12 files, predictable PayloadIDs)"

key-files:
  created:
    - pkg/metadata/storetest/backup_conformance.go
  modified:
    - pkg/metadata/backup.go
    - pkg/metadata/backup_test.go

key-decisions:
  - "Added 7 new tests in backup_test.go (4 self-identity + distinctness + two wrap-preservation patterns) to lock the D-07 error taxonomy against regressions"
  - "Populate helper creates 2 shares (/backup-a, /backup-b), 3 nested directories per share (dir-0..dir-2), 2 regular files per directory (file-0, file-1) with PayloadIDs of the form payload-<share-suffix>-<dir>-<file>"
  - "Concurrent writer uses inline PutFile/SetParent/SetChild/SetLinkCount dance (not createTestFile) because createTestFile calls t.Fatal from its caller — unsafe from a goroutine; errors are counted via atomic.Int64 instead"
  - "Corruption test includes three variants per D-08: 1-byte header truncation, half-length body truncation, single-byte XOR flip at mid-archive"
  - "Enumeration of restored stores uses public MetadataStore surface only (ListShares + recursive ListChildren + GetFile) — no peeking at engine internals"
  - "flipByte helper uses XOR with 0xFF rather than a random byte: deterministic and guaranteed to produce a different value"

patterns-established:
  - "Shared-suite factory returns engine's concrete store (satisfies MetadataStore + Backupable + io.Closer union)"
  - "populateForBackup returns a backupTestLayout struct carrying shareNames, per-share path→PayloadID map, the expected PayloadIDSet, and a fileHandles reverse index for cross-checks"
  - "payloadSetsEqual helper for PayloadIDSet equality (avoids reflect.DeepEqual on map types with empty structs)"

requirements-completed: [ENG-01, ENG-02, ENG-03]

# Metrics
duration: 15min
completed: 2026-04-16
---

# Phase 2 Plan 01: Backup Error Taxonomy + Shared Conformance Suite Summary

**Four typed backup sentinel errors added to pkg/metadata and a 5-subtest shared conformance suite shipped in pkg/metadata/storetest — unblocks Wave-2 memory, badger, and postgres driver plans.**

## Performance

- **Duration:** ~15 min
- **Started:** 2026-04-16T08:00:00Z (approx.)
- **Completed:** 2026-04-16T08:10:00Z
- **Tasks:** 2 (both `type="auto" tdd="true"`)
- **Files modified:** 3 (1 created, 2 modified)

## Accomplishments

- **Error taxonomy locked (D-07):** Four sentinel errors — `ErrRestoreDestinationNotEmpty`, `ErrRestoreCorrupt`, `ErrSchemaVersionMismatch`, `ErrBackupAborted` — live alongside the existing `ErrBackupUnsupported` in `pkg/metadata/backup.go`. Each sentinel has an `errors.Is` self-identity test plus distinctness and `%w` / `%w: %v` wrap-preservation tests that guard the Phase-5 typed-dispatch contract.
- **Conformance contract published (D-08):** `pkg/metadata/storetest/backup_conformance.go` exposes `RunBackupConformanceSuite(t, factory)` / `RunBackupConformanceSuiteWithOptions(t, factory, opts)` plus the `BackupTestStore` / `BackupStoreFactory` / `BackupSuiteOptions` types. Five subtest helpers (`RoundTrip`, `ConcurrentWriter`, `Corruption`, `NonEmptyDest`, `PayloadIDSet`) are ready for Wave 2.
- **Engine-agnostic suite:** The conformance file imports only `pkg/metadata` and stdlib (`bytes`, `context`, `errors`, `fmt`, `io`, `sync`, `sync/atomic`, `testing`, `time`). No build tag — it is compiled in by every engine's test package regardless of `//go:build integration`.

## Task Commits

Each task was committed atomically on the worktree branch:

1. **Task 1: Add Phase-2 sentinel errors with errors.Is round-trip tests** — `22ec6bfe` (feat)
2. **Task 2: Create shared conformance suite with 5 subtests** — `2f6a694d` (feat)

_Both tasks use TDD: Task 1 added tests first (RED — undefined sentinels), then sentinels (GREEN — all pass). Task 2 was effectively structural (compile-only verification at this stage — runtime exercise happens in Wave 2)._

## Files Created/Modified

- **`pkg/metadata/backup.go`** — Appended four `var Err… = errors.New(...)` declarations after the existing `ErrBackupUnsupported`. No other changes (interface, `PayloadIDSet`, existing sentinel untouched). No new imports.
- **`pkg/metadata/backup_test.go`** — Added `TestErrRestoreDestinationNotEmptyIs`, `TestErrRestoreCorruptIs`, `TestErrSchemaVersionMismatchIs`, `TestErrBackupAbortedIs`, `TestErrSentinelsDistinct`, `TestErrSentinelsWrap`. Added `errors` and `fmt` to the import list.
- **`pkg/metadata/storetest/backup_conformance.go`** (created) — 587 lines. Package `storetest` (same as `suite.go` so package-scoped helpers `createTestShare` / `createTestFile` / `createTestDir` are directly callable). Exports `RunBackupConformanceSuite`, `RunBackupConformanceSuiteWithOptions`, `BackupTestStore`, `BackupStoreFactory`, `BackupSuiteOptions`. Internals: `populateForBackup`, `enumerateRestoredPayloadIDs`, `walkCollectPayloadIDs`, `flipByte`, `payloadSetsEqual`, plus the five test helpers.

## Canonical Signatures (for Wave-2 consumption)

```go
// pkg/metadata/backup.go — Phase-2 sentinel additions
var ErrRestoreDestinationNotEmpty = errors.New("restore destination is not empty")
var ErrRestoreCorrupt             = errors.New("restore stream is corrupt")
var ErrSchemaVersionMismatch      = errors.New("restore archive schema version mismatch")
var ErrBackupAborted              = errors.New("backup aborted")

// pkg/metadata/storetest/backup_conformance.go — public API
type BackupTestStore interface {
    metadata.MetadataStore
    metadata.Backupable
    io.Closer
}

type BackupStoreFactory func(t *testing.T) BackupTestStore

type BackupSuiteOptions struct {
    SkipConcurrentWriter     bool
    ConcurrentWriterDuration time.Duration
}

func RunBackupConformanceSuite(t *testing.T, factory BackupStoreFactory)
func RunBackupConformanceSuiteWithOptions(t *testing.T, factory BackupStoreFactory, opts BackupSuiteOptions)
```

## Reuse from pkg/metadata/storetest/suite.go

Helpers used directly (same package, lowercase, in-scope):

- `createTestShare(t, store, shareName) metadata.FileHandle` — used by `populateForBackup` and `testBackupNonEmptyDest`
- `createTestFile(t, store, shareName, dirHandle, name, mode) metadata.FileHandle` — used by `populateForBackup` and `testBackupNonEmptyDest`
- `createTestDir(t, store, shareName, parentHandle, name) metadata.FileHandle` — used by `populateForBackup`

No changes to `suite.go` — the new suite co-exists without modifying the pre-existing `RunConformanceSuite` entry point.

## Populate Shape (for Wave-2 driver anticipation)

`populateForBackup` writes a deterministic tree into the source store so Wave-2 engine drivers can predict the exact load the suite applies:

| Dimension | Value |
|-----------|-------|
| Shares | 2 (`/backup-a`, `/backup-b`) |
| Directories per share | 3 (`dir-0`, `dir-1`, `dir-2`) — nested directly under the share root |
| Regular files per directory | 2 (`file-0`, `file-1`) |
| Total regular files | 12 |
| Total directory nodes | 2 (roots) + 6 (subdirs) = 8 |
| PayloadID format | `payload-<share-suffix>-<dir-name>-<file-name>` |
| Example PayloadID | `payload-backup-a-dir-0-file-0` |
| Expected PayloadIDSet size | 12 (all distinct) |

The two-store fixture requirement (factory is called twice per subtest for source + destination) is honored by every subtest. Memory engines should use independent `NewMemoryMetadataStoreWithDefaults()` instances; badger needs two `t.TempDir()` roots; postgres needs two distinct schemas (or two databases in the shared-container harness).

## Decisions Made

- **Added extra `errors`/`fmt` import to backup_test.go.** The existing file did not need these; the new wrap-preservation tests require both. Minimal footprint and satisfies acceptance criteria (no new imports to backup.go itself).
- **Concurrent writer goroutine uses inline mutations, not `createTestFile`.** `createTestFile` calls `t.Fatal` on error, which must not be invoked from a non-test goroutine (Go testing invariant). The goroutine instead counts errors via `atomic.Int64` and drives `GenerateHandle` / `PutFile` / `SetParent` / `SetChild` / `SetLinkCount` directly. The atomic counter is not asserted on (engines differ in how much contention they tolerate); the goroutine exists only to generate concurrent load while Backup runs.
- **Restore-after-corruption tail assertion.** Each corruption variant subtest calls `Restore(good)` on the same destination after the rejected corrupt restore. This catches drivers that leave tombstones / partial state behind after a failed restore — the subsequent good restore must succeed, which requires the destination to have remained empty.
- **Explicit non-matching check in Corruption test.** Added a defensive assertion that the corruption error is `ErrRestoreCorrupt` AND NOT `ErrRestoreDestinationNotEmpty` (protects against a driver that accidentally returns the wrong sentinel type).
- **`BackupSuiteOptions.ConcurrentWriterDuration` default of 100ms.** Matches D-08's 100ms reference. Zero value falls back to the default — engines that need longer (or shorter) can override.

## Deviations from Plan

None - plan executed exactly as written.

One minor clarification not strictly listed in the plan's `<action>` steps but aligned with the acceptance criteria: the grep count for `metadata.ErrRestoreCorrupt` required at least 2 references; the test as initially drafted had 1 (single `errors.Is` assertion). Added a second defensive assertion in the same subtest that the error is `ErrRestoreCorrupt` AND NOT `ErrRestoreDestinationNotEmpty`. This is strictly a strengthening of the existing test, not a scope change.

## Issues Encountered

None. Clean TDD cycle:

1. Task 1 RED: wrote 7 new tests in backup_test.go, ran `go test` — build failed with "undefined: ErrRestoreDestinationNotEmpty" etc. Expected.
2. Task 1 GREEN: appended four `var` declarations to backup.go — all 7 tests pass.
3. Task 2: wrote the conformance file top-to-bottom; `go build ./pkg/metadata/storetest/...` and `go vet ./pkg/metadata/storetest/...` both clean on first compile.

## Verification Results

| Check | Result |
|-------|--------|
| `go build ./pkg/metadata/... ./pkg/metadata/storetest/...` | OK |
| `go build ./...` | OK |
| `go vet ./pkg/metadata/... ./pkg/metadata/storetest/...` | OK |
| `go vet ./...` | OK |
| `go test ./pkg/metadata/ -run TestErr -count=1` | PASS (8/8 tests: existing ErrBackupUnsupportedIs + 7 new) |
| `go test ./pkg/metadata/ -count=1` | PASS (full package) |
| Acceptance criteria greps (both tasks) | All pass |
| No engine-specific imports in conformance file | Confirmed (grep matches 0) |
| No `//go:build` tag on conformance file | Confirmed (first line: `package storetest`) |

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness

**Wave 2 unblocked.** Plans 02-02 (memory), 02-03 (badger), 02-04 (postgres) can now:

1. Reference `metadata.ErrRestoreCorrupt` / `metadata.ErrRestoreDestinationNotEmpty` / `metadata.ErrBackupAborted` / `metadata.ErrSchemaVersionMismatch` from their driver code with `errors.Is` dispatch working out of the box.
2. Write a one-line engine test file (`backup_test.go` in each store package) calling `storetest.RunBackupConformanceSuite(t, factoryFn)` and expect all 5 subtests to execute.
3. Expect the factory to be invoked twice per subtest (source + destination) — engines MUST return fully-independent instances (distinct tmp dirs / PG databases / memory instances).
4. Anticipate 12 regular files across 2 shares with deterministic PayloadIDs — useful for sizing buffers and tuning default options.

No blockers or concerns.

## Self-Check

- [x] `pkg/metadata/backup.go` contains 4 new `var Err… = errors.New(...)` declarations
- [x] `pkg/metadata/backup_test.go` contains 4 new `TestErr…Is` tests plus `TestErrSentinelsDistinct` and `TestErrSentinelsWrap`
- [x] `pkg/metadata/storetest/backup_conformance.go` exists and exports the 3 types + 2 functions
- [x] File contains 5 subtest helpers (grep verified)
- [x] Commit `22ec6bfe` (Task 1) present in git log
- [x] Commit `2f6a694d` (Task 2) present in git log
- [x] `go build ./...` clean
- [x] `go vet ./...` clean
- [x] `go test ./pkg/metadata/ -run TestErr -count=1` all pass

## Self-Check: PASSED

---
*Phase: 02-per-engine-backup-drivers*
*Completed: 2026-04-16*
