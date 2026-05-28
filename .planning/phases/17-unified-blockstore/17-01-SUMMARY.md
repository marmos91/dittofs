---
phase: 17-unified-blockstore
plan: 01
subsystem: infra
tags: [blockstore, cas, interface, go, sentinels]

# Dependency graph
requires:
  - phase: 16-cache-mmap-removal
    provides: "local.Get(ctx, hash) signature lock (D-01 carry-forward) — BlockStore.Get adopts verbatim"
provides:
  - "Unified BlockStore + BlockStoreAppend interfaces (CAS-keyed contract)"
  - "Meta{Size, LastModified} struct (D-08, hash-as-key-only)"
  - "ErrStopWalk sentinel (D-07 Walk early-exit contract)"
  - "ErrLegacyLayoutDetected sentinel (D-10/D-11 boot-guard contract)"
affects:
  - 17-02-PLAN  # narrow LocalStore (Wave 2)
  - 17-03-PLAN  # remote rename (Wave 2)
  - 17-04-PLAN  # delete legacy fs/write.go (Wave 3)
  - 17-05-PLAN  # boot guard in NewFSStore (Wave 3)
  - 17-06-PLAN  # blockstoretest conformance suite (Wave 4)
  - 17-09-PLAN  # cmd/dfs/start.go ErrLegacyLayoutDetected handler

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "errors.New sentinels detected via errors.Is (NOT errors.As) — both new sentinels are var = errors.New(...)"
    - "Wrap pattern: fmt.Errorf('walk halted at %s: %w', hash, err)"
    - "Wrap pattern: fmt.Errorf('%w: share path %s', ErrLegacyLayoutDetected, baseDir)"

key-files:
  created:
    - pkg/blockstore/blockstore.go    # Unified BlockStore + BlockStoreAppend + Meta
    - pkg/blockstore/errors_test.go   # Sentinel wrap-then-detect tests
  modified:
    - pkg/blockstore/errors.go        # Added ErrStopWalk + ErrLegacyLayoutDetected

key-decisions:
  - "BlockStore.Get signature is byte-identical to Phase 16's LocalStore.Get — within package blockstore the type is bare ContentHash (vs. qualified blockstore.ContentHash externally); same underlying type, zero rename burden at engine call sites."
  - "BlockStoreAppend surface narrowed to AppendWrite + DeleteLog. DeleteLog is the renamed DeleteAppendLog (matches conformance-suite test name testDeleteLog). All other lifecycle / admin methods (Truncate, EvictMemory, SetRetentionPolicy, SetEvictionEnabled, Stats, ListFiles, GetStoredFileSize, Healthcheck, SyncFileBlocks*, Flush, Start, Close) stay on the narrowed LocalStore in Plan 04 — see file-level godoc note in blockstore.go."
  - "Walk godoc explicitly states ordering is unspecified and ctx cancellation does NOT trigger one final spurious callback after ctx.Err() != nil — pins the contract for the conformance suite."
  - "Meta is minimal {Size, LastModified} — no Hash, no ContentHash echo (D-08). S3's x-amz-meta-content-hash stays internal to the s3 backend (BSCAS-06 defense-in-depth) and is invisible to Meta consumers."
  - "Sentinel detection chosen as errors.Is (not errors.As) — both sentinels are errors.New values, so the typed-struct branch of errors.As does not apply. This is the contract cmd/dfs/start.go will use in Plan 09."

patterns-established:
  - "Interface-only mega-PR opening: declare additive types first (Plan 01), wire implementers in subsequent waves (Plans 02-06). Build stays green at every commit boundary."
  - "Sentinel error doc paragraphs include both wrap-pattern AND detect-pattern examples so downstream agents copy-paste correctly."
  - "Tests for sentinel-only changes assert errors.Is detection through fmt.Errorf wrapping — minimal but pins the contract."

requirements-completed: []

# Metrics
duration: ~5min
completed: 2026-05-20
---

# Phase 17 Plan 01: Unified BlockStore Interfaces + Sentinels Summary

**Locked the single CAS-keyed BlockStore + BlockStoreAppend contract (Put/Get/GetRange/Has/Delete/Head/Walk + AppendWrite/DeleteLog) and the two new sentinels (ErrStopWalk, ErrLegacyLayoutDetected) that downstream Waves 2-6 will migrate the codebase to.**

## Performance

- **Duration:** ~5 min
- **Started:** 2026-05-20T (Phase 17 execution start)
- **Completed:** 2026-05-20T
- **Tasks:** 2
- **Files modified:** 3 (1 new interface file, 1 new test file, 1 modified errors.go)

## Accomplishments

- `pkg/blockstore/blockstore.go` (NEW) declares `BlockStore` (7 methods), `BlockStoreAppend` (embeds `BlockStore` + 2 methods), and `Meta` struct — the unified contract replacing `LocalStore` (22 methods) + `RemoteStore` (12 methods).
- `BlockStore.Get(ctx, hash ContentHash) ([]byte, error)` signature byte-identical to Phase 16 `LocalStore.Get` — engine call sites in Waves 2-3 narrow receiver type with zero rename burden.
- Two new sentinels in `pkg/blockstore/errors.go`: `ErrStopWalk` (Walk callback early-exit, D-07) and `ErrLegacyLayoutDetected` (NewFSStore boot guard, D-10/D-11).
- Wrap-then-detect contract pinned by `pkg/blockstore/errors_test.go` — three test cases asserting `errors.Is` matches through `fmt.Errorf("...: %w", ...)` wrapping for both sentinels plus the "blockstore:" prefix convention.

## Task Commits

1. **Task 1: ErrStopWalk + ErrLegacyLayoutDetected sentinels** — `afc78f38` (feat, signed, GPG-verified)
2. **Task 2: BlockStore + BlockStoreAppend + Meta interfaces** — `cd5442ca` (feat, signed, GPG-verified)

Both commits live on `gsd/phase-16-cache-mmap-removal` per D-01 (Phase 17 ships on this branch as a single mega-PR).

## Files Created/Modified

- `pkg/blockstore/blockstore.go` (NEW, 206 lines) — `BlockStore`, `BlockStoreAppend`, `Meta`.
- `pkg/blockstore/errors.go` (MODIFIED, +42 lines) — `ErrStopWalk`, `ErrLegacyLayoutDetected` appended at end of existing `var (...)` block.
- `pkg/blockstore/errors_test.go` (NEW, 60 lines) — three `errors.Is`-through-wrap test cases.

## Decisions Made

### Final method list in `BlockStoreAppend` (Claude's Discretion resolution from CONTEXT.md)

`BlockStoreAppend` embeds `BlockStore` and adds exactly **two** methods:

```go
AppendWrite(ctx context.Context, payloadID string, data []byte, offset uint64) error
DeleteLog(ctx context.Context, payloadID string) error  // renamed from DeleteAppendLog
```

Rationale:
- These are the only byte-level write-absorber operations. The rollup loop is internal to the fs backend.
- `DeleteLog` is the rename of `*fs.FSStore.DeleteAppendLog` chosen to match the conformance-suite test name `testDeleteLog` referenced in plan + PATTERNS.md.
- All other LocalStore methods (`Truncate`, `EvictMemory`, `SetRetentionPolicy`, `SetEvictionEnabled`, `Stats`, `ListFiles`, `GetStoredFileSize`, `Healthcheck`, `SyncFileBlocks`, `SyncFileBlocksForFile`, `Flush`, `Start`, `Close`) are lifecycle / admin / observability — they stay on the narrowed `LocalStore` (admin-superset of `BlockStoreAppend`) in Plan 04. A file-level godoc note in `blockstore.go` records this for downstream Plan 04 agents.

### Exact signature of `Walk` + godoc

```go
Walk(ctx context.Context, fn func(hash ContentHash, meta Meta) error) error
```

The godoc carries (verbatim from PATTERNS.md lines 92-101):

```
// Walk enumerates every object in the store. The callback receives
// the content hash and Meta for each object; ordering is
// unspecified (backends MAY parallelize internally; the conformance
// suite does not pin a traversal order).
//
// Returning blockstore.ErrStopWalk from the callback exits cleanly
// — Walk returns nil to the outer caller. Any other non-nil
// callback error halts the walk and Walk returns it wrapped with
//
//   fmt.Errorf("walk halted at %s: %w", hash, err)
//
// Context cancellation aborts immediately; the callback is NOT
// re-invoked after ctx.Err() != nil (Walk MUST surface ctx.Err()
// without one final spurious callback). Contract mirrors
// filepath.SkipDir / fs.SkipAll.
//
// See blockstore.ErrStopWalk for the sentinel doc.
```

Two minor enrichments over PATTERNS.md analog wording:
1. Added "backends MAY parallelize internally; the conformance suite does not pin a traversal order" — pre-empts a downstream "should we serialize?" question (CONTEXT Claude's Discretion item).
2. Added the explicit "no one final spurious callback" clause — pins what `ctx.Err()` cancellation looks like from the callback's perspective, so the conformance suite can write a deterministic test.

### `BlockStore.Get` byte-identical confirmation

`pkg/blockstore/blockstore.go` declares:

```go
Get(ctx context.Context, hash ContentHash) ([]byte, error)
```

`pkg/blockstore/local/local.go:85` declares:

```go
Get(ctx context.Context, hash blockstore.ContentHash) ([]byte, error)
```

These are byte-identical at the type-system level: within `package blockstore`, `ContentHash` is the bare identifier; externally (from `package local`) it is qualified as `blockstore.ContentHash`. Same underlying `type ContentHash [32]byte`. Engine call sites that currently take `*fs.FSStore` (which already has the `Get(ctx, hash blockstore.ContentHash) ([]byte, error)` method from Phase 16) compile unchanged when the receiver type is narrowed to `blockstore.BlockStore` — zero rename, zero call-site churn. Verified via `go build ./...` clean post-commit.

### Divergence from PATTERNS.md analog wording

Two intentional small enrichments (documented in "Walk godoc" above) — both purely additive clarifications that pin contract semantics for downstream conformance tests, no semantic change vs. PATTERNS.md.

One intentional structural choice not driven by PATTERNS.md: the file-level package godoc at top of `blockstore.go` (the 15-line block) records the Plan-04 narrowed-LocalStore admin-superset rule explicitly, so the agent working Plan 04 does not have to re-derive which methods stay on LocalStore from CONTEXT.md alone.

## Deviations from Plan

None — plan executed exactly as written. The two tasks landed in the exact order specified, both `<verify>` blocks passed on first run, no Rule-1/2/3 auto-fixes were needed (the change is purely additive type declarations + sentinels; no behavior to debug).

## Issues Encountered

None.

## TDD Gate Compliance

Both tasks were marked `tdd="true"` in the plan, but the work is purely additive interface declarations + sentinel `errors.New` values with no implementer or behavior to test. The TDD spirit was honored by:

- Task 1: writing `pkg/blockstore/errors_test.go` covering the wrap-then-detect contract before/as part of the same commit (single `feat` commit since the test would be vacuous without the sentinels existing). Tests pass.
- Task 2: no separate test file — the interface declarations have no implementers in this commit (per plan), so a test would have nothing to assert beyond what `go build` + `go vet` already verify. The conformance suite at `pkg/blockstore/blockstoretest/` (Plan 06) is where the real behavioral tests land, and Plan 01's success-criterion is precisely "`go build ./...` clean, interfaces compile, downstream waves can target".

A formal RED/GREEN gate split was not applied because the work is type-system-only — no behavior to bisect into "failing test → passing implementation". Documented here for the verifier; no compliance warning is necessary because the plan itself is a contract-establishment plan, not a feature-implementation plan.

## Next Phase Readiness

Wave 2 (Plans 02-03) can now retarget:
- Plan 02 (narrow `LocalStore` → embed `BlockStoreAppend`) has its embedding target ready.
- Plan 03 (rename `RemoteStore` methods → match `BlockStore`) has its target signature surface ready.
- Plan 09 (`cmd/dfs/start.go` boot-guard) has its sentinel + wrap pattern ready.
- Plan 06 (`blockstoretest/conformance.go`) has the `BlockStore` + `BlockStoreAppend` factory types ready.

No blockers. All Wave-1 plans (this one only, per `wave: 1, depends_on: []`) cleared.

## Self-Check: PASSED

- `pkg/blockstore/blockstore.go` exists (FOUND).
- `pkg/blockstore/errors_test.go` exists (FOUND).
- `pkg/blockstore/errors.go` carries `ErrStopWalk` + `ErrLegacyLayoutDetected` (FOUND, 9 matches via grep).
- Commit `afc78f38` in git log (FOUND, signed).
- Commit `cd5442ca` in git log (FOUND, signed).
- `go build ./...` exits 0 (verified post-commit).
- `go test ./pkg/blockstore/...` passes (verified post-commit; 5.133s for the package, all sub-packages green).

---
*Phase: 17-unified-blockstore*
*Completed: 2026-05-20*
