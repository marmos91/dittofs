---
phase: 18-syncer-simplification
plan: 01
subsystem: metadata
tags: [metadata, sync-state, blockstore, content-hash, blake3, interface, conformance-suite]

# Dependency graph
requires:
  - phase: 17-unified-blockstore
    provides: blockstore.ContentHash unified type surface (Phase 17, merged d225926f)
  - phase: 10-fastcdc-chunker-hybrid-local-store-a1
    provides: metadata.RollupStore injection pattern (structural template)
provides:
  - metadata.SyncedHashStore interface (3 methods, idempotent)
  - RunSyncedHashStoreSuite shared conformance harness
  - MemoryMetadataStore SyncedHashStore implementation
affects:
  - 18-02 (badger backend), 18-03 (postgres backend), 18-04 (FSStore injection), 18-06 (Syncer mirror loop), 18-07 (engine.Delete cascade)

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Small-interface-with-conformance-suite (clone of RollupStore shape)"
    - "Ctx-first nil-check on every store method"
    - "Lazy nil-map init under dedicated RWMutex"
    - "Provenance-free godoc (no Phase/D-NN/.planning references in source)"

key-files:
  created:
    - pkg/metadata/synced_hash_store.go
    - pkg/metadata/synced_hash_store_suite.go
    - pkg/metadata/store/memory/synced_hash_store.go
    - pkg/metadata/store/memory/synced_hash_store_test.go
  modified:
    - pkg/metadata/store/memory/store.go

key-decisions:
  - "Interface lives at pkg/metadata/synced_hash_store.go (not under storetest/), mirroring rollup_store.go precedent (D-02, PATTERNS §F)"
  - "time.Time value (not struct{}) on memory map reserves capacity for future Count/Stats observability without schema change"
  - "Suite uses lukechampine.com/blake3 mustHash helper; distinct hash seeds per subtest enable shared-store reuse"
  - "No sentinel errors — all three methods are idempotent by design (D-03)"

patterns-established:
  - "SyncedHashStore: 3-method per-CAS-hash boolean state, idempotent on Mark/Delete, (false, nil) on absent"
  - "Conformance suite location: pkg/metadata/{interface}_suite.go alongside the interface (not in storetest/)"
  - "Concurrent stress block (16 goroutines alternating Mark/Delete) — asserts no-panic + no-error invariant only; final state intentionally non-deterministic"

requirements-completed: [D-02, D-03]

# Metrics
duration: ~20 min
completed: 2026-05-21
---

# Phase 18 Plan 01: SyncedHashStore interface + conformance suite + memory backend Summary

**Per-CAS-hash local→remote sync state interface (3 idempotent methods) mirroring the proven RollupStore injection shape, with shared conformance harness and memory backend implementation; foundation for the Phase 18 Syncer mirror-loop rewrite.**

## Performance

- **Duration:** ~20 min
- **Started:** 2026-05-21T08:43:00Z (approx)
- **Completed:** 2026-05-21T09:03:00Z
- **Tasks:** 3
- **Files modified:** 5 (4 created + 1 modified)

## Accomplishments
- Established the `metadata.SyncedHashStore` surface with 3 idempotent methods (`IsSynced`, `MarkSynced`, `DeleteSynced`) that any backend can satisfy without sentinel-error coordination
- Shipped a single shared `RunSyncedHashStoreSuite` harness covering six scenarios (`IsSyncedBeforeMark`, `MarkThenIsSynced`, `IsolationBetweenHashes`, `MarkIdempotent`, `DeleteSyncedAfterMark`, `ConcurrentMarkAndDelete`) so the badger and postgres backends in plans 02/03 can prove conformance with one test function each
- Memory backend implementation passes the full suite under `-race`; the `syncedMu` + `synced map[ContentHash]time.Time` field pair leaves room for a future `Count`/`Stats` observability surface without a struct re-shape

## Task Commits

Each task was committed atomically:

1. **Task 1: Define SyncedHashStore interface** — `c85aba4f` (feat)
2. **Task 2: Implement shared conformance suite** — `8eeddb0d` (feat)
3. **Task 3: Memory backend + struct field expansion + conformance test** — `417b2660` (feat)

## Files Created/Modified
- `pkg/metadata/synced_hash_store.go` — interface declaration with neutral-tone package godoc, no sentinel exports
- `pkg/metadata/synced_hash_store_suite.go` — `RunSyncedHashStoreSuite(t, s)` + `mustHash` helper using `lukechampine.com/blake3`
- `pkg/metadata/store/memory/synced_hash_store.go` — `MemoryMetadataStore` method receivers + compile-time assertion
- `pkg/metadata/store/memory/synced_hash_store_test.go` — `TestMemorySyncedHashStore_Suite` invokes the shared suite
- `pkg/metadata/store/memory/store.go` — adds `time` import + two struct fields (`syncedMu sync.RWMutex`, `synced map[blockstore.ContentHash]time.Time`) adjacent to the existing `rollupMu`/`rollupOffsets`

## Decisions Made
- **Suite location:** kept at `pkg/metadata/synced_hash_store_suite.go` (NOT under `pkg/metadata/storetest/`) to mirror the established `rollup_store_suite.go` precedent. ROADMAP wording referencing `storetest/` is documentation drift, called out in plan PATTERNS §F.
- **Map value type:** chose `time.Time` over `struct{}` to reserve future observability capacity (`SyncedHashStore.Stats()` / `Count()` in the `<deferred>` block) without a follow-up schema change.
- **Concurrent stress assertions:** the `ConcurrentMarkAndDelete` subtest deliberately does not assert a final state — only that `IsSynced` returns without panic/error after 16 racing goroutines. The race is fundamentally non-deterministic; over-specifying would produce a flaky test.
- **Provenance-free godoc:** all four new files contain zero "Phase 18", "D-NN", or `.planning/` references. Existing analogs (`rollup_store.go`) violate this convention; new code does better per `feedback_no_phase_comments_in_code.md`.

## Deviations from Plan

None — plan executed exactly as written.

The plan called for a `time` import addition in `store.go` indirectly (via the new `time.Time`-typed struct field). That was added; it is the only adjustment beyond the explicit task actions and was anticipated in the plan's PATTERNS §3 (lines 127).

## Issues Encountered

None.

## User Setup Required

None — pure in-process metadata interface, no external services.

## Next Phase Readiness

**Ready for parallel execution:**
- Plan 18-02 (badger backend): can copy the memory implementation shape; conformance suite is already callable.
- Plan 18-03 (postgres backend + migration `000015_synced_hashes.{up,down}.sql`): same — only the storage primitive changes.
- Plan 18-04 (`FSStoreOptions.SyncedHashStore` injection): can wire the interface into the constructor without depending on a specific backend.

**Carry-forward for downstream plans:**
- `metadata.SyncedHashStore` and `metadata.RunSyncedHashStoreSuite` are the public symbols subsequent backends import.
- The `mustHash` helper is unexported; backends pick their own hash seeds following the `"suite-<scenario>"` convention.

## Self-Check: PASSED

Verified:
- `pkg/metadata/synced_hash_store.go` — FOUND
- `pkg/metadata/synced_hash_store_suite.go` — FOUND
- `pkg/metadata/store/memory/synced_hash_store.go` — FOUND
- `pkg/metadata/store/memory/synced_hash_store_test.go` — FOUND
- `pkg/metadata/store/memory/store.go` — MODIFIED (2 struct fields + time import)
- Commit `c85aba4f` — FOUND on branch
- Commit `8eeddb0d` — FOUND on branch
- Commit `417b2660` — FOUND on branch
- `go build ./pkg/metadata/...` — PASS
- `go vet ./pkg/metadata/...` — PASS
- `go test -race -count=1 ./pkg/metadata/store/memory/... -run TestMemorySyncedHashStore_Suite` — PASS (6/6 subtests)
- `go build ./...` — PASS (no downstream breakage)
- Provenance grep across the 4 new files — ZERO matches

---
*Phase: 18-syncer-simplification*
*Completed: 2026-05-21*
