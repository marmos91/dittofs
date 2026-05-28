---
phase: 16-cache-mmap-removal
plan: 01
subsystem: blockstore
tags: [blockstore, local, cas, interface, fsstore, memorystore, conformance]

requires:
  - phase: 10-fastcdc-chunker-hybrid-local-store
    provides: FSStore CAS layout + chunkstore.ReadChunk (the delegate target)
  - phase: 11-cas-write-path-gc-rewrite-a2
    provides: ContentHash type + ErrChunkNotFound sentinel
provides:
  - LocalStore.Get(ctx, hash) ([]byte, error) interface method
  - (*FSStore).Get one-line delegate over ReadChunk
  - (*MemoryStore).Get documented stub (closed-store guard + ErrChunkNotFound)
  - RunGetSuite conformance scenario in localtest (missing-hash + CAS round-trip + fresh-allocation defense)
affects: [16-02 cache rewire, Phase 17 unified BlockStore.Get]

tech-stack:
  added: []
  patterns:
    - "Hash-keyed read surface on LocalStore (forward-compat with Phase 17 unified BlockStore)"
    - "Optional-capability probe in conformance suites via small unexported interface (chunkStorer)"

key-files:
  created:
    - pkg/blockstore/local/fs/chunkstore_get_test.go
    - pkg/blockstore/local/memory/memory_get_test.go
    - pkg/blockstore/local/fs/fs_get_conformance_test.go
  modified:
    - pkg/blockstore/local/local.go
    - pkg/blockstore/local/fs/chunkstore.go
    - pkg/blockstore/local/memory/memory.go
    - pkg/blockstore/local/localtest/suite.go
    - pkg/blockstore/local/memory/memory_test.go

key-decisions:
  - "ctx-first signature Get(ctx, hash) ([]byte, error) per established LocalStore convention; byte-for-byte forward-compatible with Phase 17 BlockStore.Get (D-01)"
  - "FSStore.Get is a one-line delegate to ReadChunk — zero new disk-read code; inherits closed-store guard, ENOENT->ErrChunkNotFound, LSL-08 LRU touch (D-03)"
  - "MemoryStore.Get is a documented stub (RLock + closed guard + ErrChunkNotFound). Memory backend has no CAS layer; Phase 17 may expand"
  - "No sync.Pool for read buffers (D-04). No zero-copy aliasing (D-05) — defended by mutation-based aliasing test"
  - "Conformance suite uses optional-capability probe (chunkStorer interface) so CAS round-trip subtests auto-skip on non-CAS backends without per-backend branching"
  - "Has(hash) NOT added to LocalStore in Phase 16 — deferred to Phase 17 unified interface (Claude's Discretion ruling)"

patterns-established:
  - "Capability-probe-in-conformance: optional interface in localtest package detects backend feature support and skips inapplicable subtests"
  - "Mutation-based aliasing defense: behavior assertion (mutate slice #1, assert slice #2 unchanged) is more robust than &out1[0] != &out2[0] pointer comparison"

requirements-completed: []

duration: ~25min
completed: 2026-05-20
---

# Phase 16 Plan 01: LocalStore.Get for content-addressed reads Summary

**Adds `LocalStore.Get(ctx, hash) ([]byte, error)` with FSStore delegating to ReadChunk and MemoryStore as a documented ErrChunkNotFound stub; conformance suite covers round-trip + fresh-allocation + missing-hash on both backends.**

## Performance

- **Duration:** ~25 min
- **Started:** 2026-05-20T10:55:00Z (approx)
- **Completed:** 2026-05-20T11:20:26Z
- **Tasks:** 2 (both TDD)
- **Files modified:** 7 (3 created, 4 modified)

## Accomplishments

- `LocalStore.Get` interface method declared with the ctx-first signature locked in D-01 — byte-for-byte forward-compatible with Phase 17's unified `BlockStore.Get`.
- `(*FSStore).Get` lands as a one-line delegate over `ReadChunk`, inheriting closed-store guard, ENOENT → `ErrChunkNotFound` sentinel mapping, and LSL-08 LRU touch behavior. Zero new disk-read code.
- `(*MemoryStore).Get` lands as the documented stub: RLock + closed-store guard + `ErrChunkNotFound`. The memory backend has no CAS layer; the lock dance is preserved so Phase 17 can drop in CAS-aware behavior without changing the surrounding locking discipline.
- `RunGetSuite` in `localtest` exercises three scenarios:
  - `Get_MissingHash_ReturnsErrChunkNotFound` (all backends)
  - `Get_CASRoundTrip` (CAS-capable backends only, gated by `chunkStorer` capability probe)
  - `Get_FreshAllocationPerCall` (mutation-based aliasing defense)
- Both backends wired: `TestFSStore_GetConformance` (round-trip + fresh-alloc + missing-hash all PASS) and `TestMemoryStore_GetConformance` (missing-hash PASS, round-trip + fresh-alloc auto-SKIP).

## Task Commits

1. **Task 1 RED** — `a8426dc4` `test(16-01): add failing tests for LocalStore.Get on FSStore + MemoryStore` (compile-fail proof the methods don't exist yet)
2. **Task 1 GREEN** — `a2e608be` `feat(16-01): add LocalStore.Get(ctx, hash) and backend impls` (interface + both impls in one atomic commit to keep the build green)
3. **Task 2** — `e5f39b5f` `test(16-01): conformance suite for LocalStore.Get` (RunGetSuite + FS + memory wiring)

No REFACTOR commits — implementations were minimal one-liners with no cleanup opportunity.

## Files Created/Modified

### Created

- `pkg/blockstore/local/fs/chunkstore_get_test.go` — narrow direct test for `(*FSStore).Get` (Task 1 RED, kept as smoke test).
- `pkg/blockstore/local/memory/memory_get_test.go` — narrow direct test for `(*MemoryStore).Get` stub behavior (Task 1 RED, kept).
- `pkg/blockstore/local/fs/fs_get_conformance_test.go` — wires `RunGetSuite` against `*fs.FSStore` via `NewWithOptions` factory.

### Modified

- `pkg/blockstore/local/local.go` — added the `Get(ctx, hash)` interface method in the read-operations cluster, between `GetBlockData` and the write-section. Godoc documents the D-03 buffer-ownership contract, the `ErrChunkNotFound` sentinel, the D-04/D-05 no-pool/no-alias guarantees, and the Phase 17 forward-compat note.
- `pkg/blockstore/local/fs/chunkstore.go` — added `(*FSStore).Get` immediately after `ReadChunk` as the one-line delegate.
- `pkg/blockstore/local/memory/memory.go` — added `(*MemoryStore).Get` near `GetBlockData` using the RLock + closed-guard + ErrChunkNotFound stub shape.
- `pkg/blockstore/local/localtest/suite.go` — added `chunkStorer` capability probe + `RunGetSuite` (three subtests).
- `pkg/blockstore/local/memory/memory_test.go` — added `TestMemoryStore_GetConformance` alongside the existing `TestMemoryStoreConformance`.

## Decisions Made

All decisions in the plan's `<must_haves>` were honored without amendment:

- ctx-first signature exactly as specified in D-01.
- One-line FSStore delegate (no inlined disk-read code).
- MemoryStore as the recommended stub variant (not the "eager" alternative — out of Phase 16 scope per CONTEXT.md).
- `Has(hash)` deliberately NOT added — deferred to Phase 17 per Claude's Discretion.
- No `sync.Pool`, no zero-copy aliasing.

## Deviations from Plan

None — plan executed exactly as written.

The conformance suite landed in a single test commit rather than a separate RED/GREEN cycle because Task 2's "code under test" (`Get`) was already implemented in Task 1; the RED for Task 2 was the suite invocation failing on a backend factory line. To avoid commit noise from a synthetic RED that compiles-and-passes immediately on the existing implementations, the conformance scenario was committed as a single `test(...)` commit.

## Issues Encountered

None.

## TDD Gate Compliance

- Task 1 RED gate: `a8426dc4` (test commit, compile failure proven via `go test`).
- Task 1 GREEN gate: `a2e608be` (feat commit, all acceptance-criteria greps + `go test ./pkg/blockstore/local/...` PASS).
- Task 2: single `test(...)` commit — no GREEN gate required because the code under test was already shipped in Task 1.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness

- **Plan 16-02** (engine.loadByHash rewire) is unblocked. The seam `bs.local.Get(ctx, hash)` is the single integration point per D-02.
- **Plan 16-03** (delete mmap files + perf gate) is unblocked independently.
- **Plan 16-04** (cache_mmap_test.go cleanup + generic-assert cherry-pick) is unblocked independently.
- **Phase 17** unified `BlockStore.Get` adopts this signature verbatim — zero rename churn at the call site.

## Self-Check: PASSED

- Created files present:
  - `pkg/blockstore/local/fs/chunkstore_get_test.go` — FOUND
  - `pkg/blockstore/local/memory/memory_get_test.go` — FOUND
  - `pkg/blockstore/local/fs/fs_get_conformance_test.go` — FOUND
- Modified files contain expected additions:
  - `Get(ctx context.Context, hash blockstore.ContentHash) ([]byte, error)` in `local.go` — FOUND
  - `func (bc *FSStore) Get(...)` returning `bc.ReadChunk(ctx, h)` in `chunkstore.go` — FOUND
  - `func (s *MemoryStore) Get(...)` returning `blockstore.ErrChunkNotFound` in `memory.go` — FOUND
  - `RunGetSuite` and `bytes.Equal`/`ErrChunkNotFound` in `localtest/suite.go` — FOUND
- Commits exist on `gsd/phase-16-cache-mmap-removal`:
  - `a8426dc4` (test RED) — FOUND
  - `a2e608be` (feat GREEN) — FOUND
  - `e5f39b5f` (test conformance) — FOUND
- All acceptance-criteria gates passed at commit time:
  - `go build ./...` exit 0
  - `go vet ./pkg/blockstore/local/...` exit 0
  - `go test ./pkg/blockstore/local/... -race -count=1` exit 0
  - `! grep -n 'sync\.Pool' pkg/blockstore/local/fs/chunkstore.go pkg/blockstore/local/memory/memory.go` (D-04 enforcement)

---
*Phase: 16-cache-mmap-removal*
*Completed: 2026-05-20*
