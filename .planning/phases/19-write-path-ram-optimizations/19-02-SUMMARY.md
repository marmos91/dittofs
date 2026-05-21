---
phase: 19-write-path-ram-optimizations
plan: 02
subsystem: metadata-stores
tags: [addref, backend-impl, atomicity, opt1, lru-hit-path, d04, d27, d22b]
requires:
  - "19-01 (FileBlockStore.AddRef interface + storetest conformance subtests)"
provides:
  - "MemoryMetadataStore.AddRef implementation"
  - "BadgerMetadataStore.AddRef implementation"
  - "PostgresMetadataStore.AddRef implementation"
  - "memoryTransaction.AddRef / badgerTransaction.AddRef / postgresTransaction.AddRef (tx-bound variants, CR-01 parity)"
  - "BadgerMetadataStore.updateWithConflictRetry helper applied to IncrementRefCount/DecrementRefCount/AddRef"
affects:
  - "Plan 19-05 (Opt 1 LRU hit-path wire-in) — AddRef is now callable on every backend"
tech_stack:
  added: []
  patterns:
    - "Per-backend native atomicity (memory: s.mu.Lock; badger: db.Update + ErrConflict retry; postgres: single UPDATE)"
    - "ErrUnknownHash sentinel on index miss + value miss (defense against orphan-index)"
    - "tx-bound variant pattern (CR-01) for memoryTransaction/badgerTransaction/postgresTransaction"
key_files:
  created: []
  modified:
    - pkg/metadata/store/memory/objects.go
    - pkg/metadata/store/badger/objects.go
    - pkg/metadata/store/postgres/objects.go
    - pkg/blockstore/local/fs/test_hooks.go
    - pkg/blockstore/local/fs/appendlog_testhelpers_test.go
    - pkg/blockstore/local/fs/fs_test.go
    - pkg/blockstore/local/fs/eviction_lsl08_conformance_test.go
    - pkg/blockstore/engine/engine_test.go
    - pkg/blockstore/engine/syncer_test.go
    - cmd/dfs/commands/migrate_to_cas.go
decisions:
  - "Badger: introduced updateWithConflictRetry helper (mirrors WithTransaction in pkg/metadata/store/badger/transaction.go) and applied it to IncrementRefCount, DecrementRefCount, AND AddRef. The plan called for ONE db.Update for AddRef; the retry-loop wrapper still performs one db.Update per attempt — and IncrementRefCount/DecrementRefCount needed the same wrap to satisfy D-04 atomicity under contention. Without the retry, the concurrent-cascade conformance test failed with ErrConflict on both AddRef and DecrementRefCount goroutines."
  - "Postgres: no LIMIT 1 on the UPDATE — kept the plan's D-22b multi-row-per-hash tolerance posture so all matching rows bump uniformly; conformance test seeds a single row, so observability is preserved exactly."
  - "Memory: extracted addRefLocked helper so the top-level AddRef and memoryTransaction.AddRef share a single locked-mutation path."
  - "All 6 test stubs in pkg/blockstore/{engine,local/fs} + cmd/dfs/commands gained an AddRef method. Stubs without a hash index return ErrUnknownHash unconditionally (matches the production fallback contract); engine_test.go and syncer_test.go stubs scan their seeded blocks map linearly and bump on hash match (so engine tests that may exercise the future hit path remain correct without rewriting stub plumbing)."
metrics:
  duration_minutes: 22
  completed_date: 2026-05-21
  tasks_completed: 3
  files_modified: 10
---

# Phase 19 Plan 02: FileBlockStore.AddRef Backend Implementations Summary

Implement `FileBlockStore.AddRef` on all three metadata backends (memory / badger / postgres) and resolve the Wave 1 D-01 atomic-merge intermediate so `go build ./...` is green again with every backend satisfying the new interface contract.

## What shipped

### Task 1 — Memory backend (commit `5e49356f`)

- Added `MemoryMetadataStore.AddRef(ctx, hash, payloadID, blockRef) error`.
- Added shared `addRefLocked` helper invoked from both the top-level method (under `s.mu.Lock`) and from `memoryTransaction.AddRef` (where the caller already holds the store mutex).
- Logic: `if s.fileBlockData == nil → ErrUnknownHash`; `id, ok := hashIndex[hash]; if !ok || id == "" → ErrUnknownHash`; `block, ok := blocks[id]; if !ok → ErrUnknownHash` (index/value desync defense); else `block.RefCount++`. `BlockState` never written.
- payloadID + blockRef accepted for future GC traceability (D-04); discarded via `_, _ =` to satisfy the unused-arg lint.

### Task 2 — Badger backend (commit `8d2838df`)

- Added `BadgerMetadataStore.AddRef(ctx, hash, payloadID, blockRef) error`.
- Added `badgerTransaction.AddRef` variant running on the active `*badger.Txn` (CR-01 parity).
- Added private helper `updateWithConflictRetry(ctx, fn)` mirroring the retry-on-`ErrConflict` loop in `WithTransaction` (transaction.go). Applied it to `IncrementRefCount`, `DecrementRefCount`, AND `AddRef` — without the retry the concurrent-cascade conformance test fails on `ErrConflict` from badger's optimistic concurrency control (one of the two contending Updates is rejected and the test sees `RefCount != 10`).
- Inside the `db.Update` closure: secondary-index resolve `fb-hash:{hash}` → unmarshal id → fetch `fb:{id}` → unmarshal → `RefCount++` → marshal → `txn.Set`. Returns `metadata.ErrUnknownHash` on both index miss AND value miss (orphan-index defense).

### Task 3 — Postgres backend + D-01 closeout (commit `e6758501`)

- Added `PostgresMetadataStore.AddRef(ctx, hash, payloadID, blockRef) error`: single `UPDATE file_blocks SET ref_count = ref_count + 1 WHERE hash = $1`. Uses `s.exec(ctx, ...)` (pool path). Returns `metadata.ErrUnknownHash` when `RowsAffected() == 0`.
- Added `postgresTransaction.AddRef` running the same UPDATE against `tx.tx.Exec(ctx, ...)` (CR-01 parity — bump rolls back with the txn).
- NO `LIMIT 1` clause: per D-22b the hash column is a non-UNIQUE partial index, so a legacy multi-row collision is acceptable and all matching rows bump uniformly. The conformance test seeds a single row so RefCount goes from N to N+1 exactly.
- `block_state` column NEVER referenced in either AddRef body (D-27 STATE-01..03 preservation).

#### Test-stub fan-out to close D-01 (Wave 1 build-green restored)

The Plan 19-01 SUMMARY explicitly flagged that backend packages and `nopFBSForTest` would fail to compile until Plan 02 implemented AddRef. After Plan 02 Task 3, `go build ./...` exits 0 and `go vet ./...` exits 0. The seven additional test-stub files updated to satisfy `EngineFileBlockStore`:

| File | Type | AddRef behavior |
|------|------|---|
| `pkg/blockstore/local/fs/test_hooks.go` | `nopFBSForTest` | return `ErrUnknownHash` |
| `pkg/blockstore/local/fs/appendlog_testhelpers_test.go` | `nopFBS` | return `ErrUnknownHash` |
| `pkg/blockstore/local/fs/fs_test.go` | `countingFileBlockStore` | proxy + count (`addRef atomic.Int64`); included in snapshot / ResetCount / TotalCount |
| `pkg/blockstore/local/fs/eviction_lsl08_conformance_test.go` | `countingFBSWrapper` | proxy + counter++ |
| `pkg/blockstore/engine/engine_test.go` | `stubFileBlockStore` | linear scan by hash, RefCount++ on match, else `ErrUnknownHash` |
| `pkg/blockstore/engine/syncer_test.go` | `stubFBS` | linear scan by hash, RefCount++ on match, else `ErrUnknownHash` |
| `cmd/dfs/commands/migrate_to_cas.go` | `nopFileBlockStore` | return `ErrUnknownHash` |

The engine stubs (`stubFileBlockStore`, `stubFBS`) hold blocks keyed by id but carry the `Hash` field on each row, so a tiny linear scan implements the contract correctly. The fs stubs (`nopFBS*`) hold no blocks at all, so the hash is always "unknown" — production callers fall back to the full `Put` path on the sentinel.

## Verification results

| Gate | Result |
|------|--------|
| `go build ./...` | PASS (D-01 intermediate closed) |
| `go vet ./...` | PASS |
| `go test ./pkg/metadata/store/memory/... -run "TestConformance/FileBlockOps/AddRef"` | 3/3 PASS |
| `go test -race ./pkg/metadata/store/memory/... -run "TestConformance/FileBlockOps/AddRef"` | 3/3 PASS |
| `go test -tags integration ./pkg/metadata/store/badger/... -run "TestConformance/FileBlockOps/AddRef"` | 3/3 PASS |
| `go test -race -tags integration ./pkg/metadata/store/badger/... -run "TestConformance/FileBlockOps/AddRef"` | 3/3 PASS |
| `go test -tags integration ./pkg/metadata/store/badger/... -run "TestConformance/FileBlockOps"` (full conformance, regression check on the retry-wrap of IncrementRefCount/DecrementRefCount) | PASS |
| `go build ./pkg/metadata/store/postgres/...` (test gated by `DITTOFS_TEST_POSTGRES_DSN`; build-only verification) | PASS |
| `go test ./pkg/blockstore/engine/... ./pkg/blockstore/local/fs/...` (smoke regression on engine + fs after stub fan-out) | PASS (engine 6.9s, fs 5.6s) |
| `grep -rn "func (s \*[A-Z]*MetadataStore) AddRef" pkg/metadata/store/ \| wc -l` | 3 (one top-level per backend) |
| `grep -A 30 "func (s \*MemoryMetadataStore) addRefLocked" pkg/metadata/store/memory/objects.go \| grep -c BlockState` | 0 (D-27 gate) |
| `grep -A 50 "func (s \*BadgerMetadataStore) AddRef" pkg/metadata/store/badger/objects.go \| grep -c BlockState` | 0 (D-27 gate) |
| `grep -A 15 "func (s \*PostgresMetadataStore) AddRef" pkg/metadata/store/postgres/objects.go \| grep -ic block_state` | 0 (D-27 gate) |

## Exact backend-specific atomicity choices

| Backend | Concurrency primitive | TOCTOU-free vs Decrement cascade? |
|---|---|---|
| memory | single `s.mu.Lock()` (Write lock) spanning hash→id resolve + RefCount++ | yes — flat-mutex serialization |
| badger | single `s.db.Update(func(txn) error {…})` with retry on `badger.ErrConflict` (via `updateWithConflictRetry`) | yes — optimistic-txn + bounded retry collapses to atomic |
| postgres | single SQL `UPDATE file_blocks SET ref_count = ref_count + 1 WHERE hash = $1` | yes — Postgres row-level lock serializes contended updates |

## Deviations from Plan

### [Rule 1 — Bug / D-04 atomicity contract] Badger: extended retry-on-conflict to IncrementRefCount + DecrementRefCount

- **Found during:** Task 2 verify gate. The concurrent-cascade conformance test (`AddRef_Concurrent_With_DecrementRefCountCascade`) failed not only on AddRef goroutines reporting `Transaction Conflict. Please retry` but also on the concurrent DecrementRefCount goroutines — both raw `s.db.Update` calls hit badger's optimistic conflict detector when 16 goroutines hammered the same key.
- **Issue:** Even after adding a retry loop to `AddRef`, the test still failed because `DecrementRefCount` returned `badger.ErrConflict` directly to the test goroutine. The D-04 contract says all three refcount mutators must be TOCTOU-free — a pre-existing bug in `IncrementRefCount`/`DecrementRefCount` happened to be invisible because no existing conformance test exercised them under contention.
- **Fix:** Factored a private helper `updateWithConflictRetry(ctx, fn func(*badger.Txn) error) error` that mirrors the retry loop in `WithTransaction` (1ms base + jitter exponential backoff, bounded by `maxTransactionRetries=20`) and applied it to all three: `IncrementRefCount`, `DecrementRefCount`, `AddRef`. The plan's "ONE `s.db.Update` call" wording is still honored — the retry loop runs at most one Update per attempt; the wrapper just gives the optimistic conflict semantics a deterministic outcome under contention.
- **Files modified:** `pkg/metadata/store/badger/objects.go` (Increment/Decrement/AddRef all routed through `updateWithConflictRetry`).
- **Commit:** `8d2838df`.

### [Rule 3 — Blocking issue / D-01 atomic-merge closeout] Test-stub fan-out beyond the 3 planned backend files

- **Found during:** Task 3 `go build ./...` post-verify after committing the three planned backend impls.
- **Issue:** Plan 19-01 SUMMARY noted "Backend packages (`pkg/metadata/store/{memory,badger,postgres}/...`) and the `pkg/blockstore/local/fs/test_hooks.go nopFBSForTest` will fail to compile" until Plan 02 lands. That list under-counted: there are SIX additional test stubs across the codebase that satisfy `blockstore.EngineFileBlockStore` and all required AddRef to compile after the interface widened. They were not listed in `<files_modified>` but are unavoidable to close D-01.
- **Fix:** Added an `AddRef` method to each of: `nopFBSForTest` (test_hooks.go, listed in Plan 01), `nopFBS` (appendlog_testhelpers_test.go), `countingFileBlockStore` (fs_test.go), `countingFBSWrapper` (eviction_lsl08_conformance_test.go), `stubFileBlockStore` (engine_test.go), `stubFBS` (syncer_test.go), `nopFileBlockStore` (migrate_to_cas.go). The stubs without a hash index return `ErrUnknownHash`; the engine stubs that hold a blocks map do a linear scan by hash for production-faithful behavior.
- **Impact:** None to production code. The `<files_modified>` set widens from 3 to 10. All file additions are stubs / proxies that route through the interface; no test semantics change (the new `addRef` counter on `countingFileBlockStore` is additive — existing callers don't read it).
- **Commit:** `e6758501` (folded into the Task 3 commit since the stubs are the mechanical D-01 closeout for the postgres landing).

## Storetest harness factory wiring

Confirmed: the storetest factory wiring picks up the AddRef subtests automatically. Each backend's `*_conformance_test.go` already calls `storetest.RunConformanceSuite(t, factoryFn)`, which routes through `runFileBlockOpsTests` → the three `t.Run("AddRef_*")` registrations added in Plan 01. No per-backend test-file edits were needed beyond what Plan 01 added centrally; Memory's conformance file (`memory_conformance_test.go`) and Badger's (`badger_conformance_test.go`, build-tag `integration`) both run the new subtests with zero modification. The postgres conformance entry follows the same pattern and will exercise the new subtests when `DITTOFS_TEST_POSTGRES_DSN` is set in CI.

## Authentication gates

None — fully autonomous execution.

## Deferred Issues

None.

## Known Stubs

None. The stubs added across the test code paths are routine interface conformance for the new `AddRef` method — none flow user-visible data anywhere. The `ErrUnknownHash`-only stubs are semantically correct: they declare "this hash was never Put", which is the truth for a no-op fixture.

## Self-Check: PASSED

- All three top-level backend AddRef methods present on disk:
  - `pkg/metadata/store/memory/objects.go` `func (s *MemoryMetadataStore) AddRef(` — line 112
  - `pkg/metadata/store/badger/objects.go` `func (s *BadgerMetadataStore) AddRef(` — line 270
  - `pkg/metadata/store/postgres/objects.go` `func (s *PostgresMetadataStore) AddRef(` — line 179
- All three task commits present on the branch:
  - `5e49356f feat(19-02): implement FileBlockStore.AddRef on memory backend`
  - `8d2838df feat(19-02): implement FileBlockStore.AddRef on badger backend`
  - `e6758501 feat(19-02): implement FileBlockStore.AddRef on postgres backend; close D-01`
- All three commits signed (`%G?` = G).
- `go build ./...` exits 0, `go vet ./...` exits 0 — D-01 atomic-merge intermediate closed.
- Memory + badger AddRef conformance subtests all PASS (incl. under `-race`).
