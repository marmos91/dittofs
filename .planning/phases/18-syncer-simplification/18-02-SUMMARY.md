---
phase: 18-syncer-simplification
plan: 02
subsystem: metadata
tags: [metadata, sync-state, blockstore, content-hash, badger, embedded-lsm, conformance-suite]

# Dependency graph
requires:
  - phase: 18-syncer-simplification
    plan: 01
    provides: metadata.SyncedHashStore interface + RunSyncedHashStoreSuite conformance harness
provides:
  - Badger backend implementation of metadata.SyncedHashStore (IsSynced/MarkSynced/DeleteSynced)
  - Conformance test wired against t.TempDir() Badger fixture (reuses newRollupTestStore)
affects:
  - 18-04 (FSStore SyncedHashStore injection — operators using Badger backend get persistence for free)
  - 18-06 (Syncer mirror loop — Badger-backed deployments now have a working synced-state store)

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Binary-raw key encoding: append([]byte(prefix), suffix...) — matches rollup.go convention"
    - "Idempotent backend via Badger txn primitives (Set overwrites, Delete no-error on absent key)"
    - "Ctx-cancel-aware on every method (ctx.Err() early-return)"
    - "Conformance suite reuse via existing newRollupTestStore fixture — no parallel helper"

key-files:
  created:
    - pkg/metadata/store/badger/synced_hash_store.go
    - pkg/metadata/store/badger/synced_hash_store_test.go
  modified: []

key-decisions:
  - "Reused newRollupTestStore (rollup_test.go) instead of creating a parallel newSyncedHashTestStore — the helper already returns *BadgerMetadataStore which satisfies the broader SyncedHashStore interface via the compile-time assertion in the implementation file. Mirrors the simpler-less-duplication path called out in the plan."
  - "Binary-raw key encoding (synced: prefix + raw 32 hash bytes) chosen over hex per PATTERNS §4 to keep keys compact on disk and parallel with rollup.go's `[]byte(rollupOffsetPrefix + payloadID)` style."
  - "Provenance-free godoc: no 'Phase 18', 'D-02', or .planning/ path references in source. Existing rollup.go violates this — new code does better per the feedback_no_phase_comments_in_code.md rule."

requirements-completed: [D-02]

# Metrics
duration: ~2 min
completed: 2026-05-21
---

# Phase 18 Plan 02: Badger SyncedHashStore Summary

**Embedded LSM-tree (Badger) implementation of `metadata.SyncedHashStore` with binary-raw key encoding under the `synced:` prefix, mirroring the rollup.go convention; conformance suite passes under -race with zero regression on existing Badger package tests.**

## Performance

- **Duration:** ~2 min (07:05:41Z → 07:07:35Z UTC, observable wall-clock)
- **Tasks:** 2
- **Files created:** 2 (`synced_hash_store.go` + `synced_hash_store_test.go`)
- **Files modified:** 0 — no struct change needed; methods attach directly to existing `*BadgerMetadataStore` receiver

## Accomplishments

- Shipped the second of three backend implementations (memory done in 18-01, postgres pending in 18-03); operators using the default embedded Badger metadata backend now have per-CAS-hash sync-state persistence without any operator action
- Three method bodies are minimal: `IsSynced` is `db.View` + `errors.Is(badger.ErrKeyNotFound)` → `(false, nil)`; `MarkSynced` is `db.Update` + `txn.Set(key, nil)`; `DeleteSynced` is `db.Update` + `txn.Delete(key)` — all idempotent by Badger txn semantics, no sentinel-error coordination at the metadata layer
- Conformance suite passes under `-race` (all 6 subtests: `IsSyncedBeforeMark`, `MarkThenIsSynced`, `IsolationBetweenHashes`, `MarkIdempotent`, `DeleteSyncedAfterMark`, `ConcurrentMarkAndDelete`) on a fresh `t.TempDir()` Badger fixture

## Task Commits

Each task was committed atomically (signed):

1. **Task 1: Implement Badger backend SyncedHashStore** — `d5803ed9` (feat)
2. **Task 2: Wire conformance test using t.TempDir() fixture** — `d8ef1eef` (test)

## Files Created/Modified

- `pkg/metadata/store/badger/synced_hash_store.go` — 104 lines: package godoc, key-namespace ASCII block, `syncedHashPrefix` constant, `keySyncedHash` helper, three method implementations on `*BadgerMetadataStore`, compile-time assertion `var _ metadata.SyncedHashStore = (*BadgerMetadataStore)(nil)`
- `pkg/metadata/store/badger/synced_hash_store_test.go` — 21 lines: `TestBadgerSyncedHashStore_Suite(t)` invokes `metadata.RunSyncedHashStoreSuite(t, newRollupTestStore(t))`

## Decisions Made

- **Fixture reuse:** the plan explicitly permitted choosing between writing a parallel `newSyncedHashTestStore` helper and reusing `newRollupTestStore`. Reused the existing helper — its return type `*BadgerMetadataStore` is the broader handle that satisfies `metadata.SyncedHashStore` via the compile-time assertion. Zero duplication, same fixture lifecycle (`t.TempDir()` + `t.Cleanup(_ = store.Close())`).
- **Key encoding:** binary-raw (`append([]byte("synced:"), hash[:]...)`) per PATTERNS §4 — keeps keys at 39 bytes on disk rather than 71 with hex. Matches rollup.go's `[]byte(rollupOffsetPrefix + payloadID)` style for symmetry.
- **Empty value semantics:** `MarkSynced` sets `nil` value (presence == synced), matching the memory backend's `map[ContentHash]time.Time` value type semantically (the time is the memory-backend's reserved capacity for future observability — Badger callers do not consume it and don't need a value here).
- **Provenance-free godoc:** all new file content has zero `Phase 18`, `D-02`, or `.planning/` references — verified via the plan's `grep -E "Phase 18|D-0[0-9]"` check (exit code 1, zero matches).

## Deviations from Plan

None — plan executed exactly as written.

The plan's `<action>` for Task 1 referenced verifying badger module path via `grep -rn "dgraph-io/badger" pkg/metadata/store/badger/rollup.go`. Confirmed `github.com/dgraph-io/badger/v4` (line 9 of rollup.go) and used the same import path. Task 2's helper reuse decision was the explicitly-preferred path in the plan ("rollup-suite reuse is the simpler, less-duplication path").

## Issues Encountered

None.

## User Setup Required

None — Badger is embedded, no external services. The implementation is ready for Plan 18-04 to inject via `FSStoreOptions.SyncedHashStore` without operator action.

## Next Phase Readiness

**Ready for parallel execution:**
- Plan 18-03 (postgres backend) — independent of this plan; can land in any order alongside it.
- Plan 18-04 (`FSStoreOptions.SyncedHashStore` injection) — can already wire Badger in production constructors via `bc.syncedHashStore = opts.SyncedHashStore` where `opts.SyncedHashStore` is the same `*BadgerMetadataStore` handle the operator already configured for rollup.

**Carry-forward for downstream plans:**
- `(*BadgerMetadataStore).IsSynced/MarkSynced/DeleteSynced` are now the public surface Badger-backed deployments use.
- Key namespace `synced:` is reserved — any future Phase 18 or beyond work that wants to add per-hash columns must either extend the value byte string (still empty today) or migrate under a fresh prefix.
- `keySyncedHash` and `syncedHashPrefix` are package-internal (lowercase) — backends downstream of `pkg/metadata/store/badger` do not import them.

## Threat Surface

No new trust boundaries introduced. Per the plan's `<threat_model>`:
- **T-18-02-01 (Tampering — key namespace collision):** mitigated. `grep -rn '"synced:"\|"ro:"' pkg/metadata/store/badger/` shows only the new `syncedHashPrefix = "synced:"` and existing `rollupOffsetPrefix = "ro:"` — no overlap, distinct prefixes.
- **T-18-02-02 (DoS — unbounded synced: key growth):** accepted per Plan 01 T-18-01-02. Bounded by local CAS chunk count; cascade `DeleteSynced` in Plan 07 enforces the strict-subset invariant.
- **T-18-02-03 (Info Disclosure — hash exposure via Badger DB file):** accepted. Hash bytes derive from chunk content; no new exposure beyond existing chunk filenames in the local CAS directory.

## Self-Check: PASSED

Verified:
- `pkg/metadata/store/badger/synced_hash_store.go` — FOUND (104 lines, 1 file changed +104 -0)
- `pkg/metadata/store/badger/synced_hash_store_test.go` — FOUND (21 lines, 1 file changed +21 -0)
- Commit `d5803ed9` (feat Task 1) — FOUND on branch
- Commit `d8ef1eef` (test Task 2) — FOUND on branch
- `go build ./pkg/metadata/store/badger/...` — PASS
- `go vet ./pkg/metadata/store/badger/...` — PASS
- `go test -race -count=1 ./pkg/metadata/store/badger/... -run TestBadgerSyncedHashStore_Suite -v` — PASS (6/6 subtests)
- `go test -race -count=1 ./pkg/metadata/store/badger/...` (full package) — PASS, no regression
- `go build ./...` — PASS (no downstream breakage)
- `grep -E "Phase 18|D-0[0-9]" pkg/metadata/store/badger/synced_hash_store.go pkg/metadata/store/badger/synced_hash_store_test.go` — ZERO matches (exit 1)
- Acceptance criteria from Task 1:
  - `grep -c "var _ metadata.SyncedHashStore" …synced_hash_store.go` = 1 ✓
  - `grep -c "syncedHashPrefix\|keySyncedHash" …synced_hash_store.go` = 7 (≥ 3) ✓
  - `grep -E "badger synced (get|mark|delete): %w" …synced_hash_store.go` = 3 (all three) ✓

---
*Phase: 18-syncer-simplification*
*Completed: 2026-05-21*
