---
phase: 11-cas-write-path-gc-rewrite-a2
fixed_at: 2026-04-25T00:00:00Z
review_path: .planning/phases/11-cas-write-path-gc-rewrite-a2/11-REVIEW-4.md
iteration: 4
findings_in_scope: 7
fixed: 6
skipped: 1
status: partial
---

# Phase 11: Code Review Fix Report (Pass 4)

**Fixed at:** 2026-04-25
**Source review:** .planning/phases/11-cas-write-path-gc-rewrite-a2/11-REVIEW-4.md
**Iteration:** 4

**Summary:**
- Findings in scope: 7
- Fixed: 6
- Deferred: 1 (IN-4-03 — see Skipped Issues)

## Fixed Issues

### WR-4-01: Postgres `PutFileBlock` violates the contract IN-3-02 documented; dedup short-circuit fails on every cross-file content collision

**Files modified:** `pkg/metadata/store/postgres/migrations/000010_file_blocks.up.sql`,
`pkg/metadata/store/postgres/migrations/000011_file_blocks_hash_nonunique.up.sql` (new),
`pkg/metadata/store/postgres/migrations/000011_file_blocks_hash_nonunique.down.sql` (new),
`pkg/metadata/storetest/file_block_ops.go`,
`pkg/blockstore/store.go`
**Commit:** `2baa932b`
**Applied fix:** Option A — drop UNIQUE constraint, keep partial index for
lookup speed. Two-pronged migration path: (1) edit 000010 inline so fresh
installs get the non-unique partial index directly (the `IF NOT EXISTS`
guard makes this idempotent); (2) add 000011 that DROPs +
re-CREATEs the index as non-unique for deployments that already ran the
broken 000010. Update the `FileBlockStore.PutFileBlock` godoc to reflect
the now-aligned contract (IN-3-02 follow-up). Add
`testPutFileBlock_TwoIDsSameHash` to the conformance suite — passes on
memory + badger today, would have failed Postgres without the migration.
Conformance suite executed against memory and badger (integration tag);
postgres backend will catch the regression on its next conformance run.

### WR-4-02: GC sweep `LastModified.IsZero()` short-circuit fails OPEN

**Files modified:** `pkg/blockstore/engine/gc.go`,
`pkg/blockstore/remote/remote.go`,
`pkg/blockstore/remote/remotetest/suite.go`
**Commit:** `ef970af9`
**Applied fix:** Sweep now treats zero LastModified as fail-closed:
the object is preserved, an `addError` diagnostic is captured ("backend
ListByPrefixWithMeta must populate LastModified for grace TTL evaluation"),
and the loop continues. Tightened `remote.ObjectInfo.LastModified` doc
from SHOULD to MUST with explicit reference to the WR-4-02 fail-closed
behavior and the conformance assertion. Added
`ListByPrefixWithMeta_LastModifiedNonZero` to the remotetest suite —
runs against every backend (memory + S3 today, any future implementor
forced to comply).

### IN-4-02: `GCStatus` handler still leaks internal err strings to client (3 sites)

**Files modified:** `internal/controlplane/api/handlers/block_gc.go`
**Commit:** `6e16f246`
**Applied fix:** Wrapped all three `InternalServerError(...)` callsites
with generic messages ("GC status lookup failed", "GC status read
failed", "GC status parse failed"). Underlying err details remain at
`logger.Debug` for postmortems. Removed the now-unused `fmt` import. Same
defense-in-depth pattern Pass 2 IN-2-01 applied to `RunGC`.

### IN-4-01: `000010_file_blocks.down.sql` is destructive without warning header

**Files modified:** `pkg/metadata/store/postgres/migrations/000010_file_blocks.down.sql`
**Commit:** `931d75fb`
**Applied fix:** Added a multi-line WARNING header at the top of the down
migration spelling out the data-loss consequences (orphaned cache files,
orphaned CAS objects post-grace-window, lost in-flight writes) and the
"do not run on a live deployment without backup + restore plan" guidance.
No schema change.

### IN-4-04: `GCState.Add` is one Badger transaction per call

**Files modified:** `pkg/blockstore/engine/gcstate.go`,
`pkg/blockstore/engine/gc.go`,
`pkg/blockstore/engine/gcstate_test.go`
**Commit:** `081f8fef`
**Applied fix:** Switched `Add()` from `db.Update(txn.Set)` per call to
buffered Badger `WriteBatch` flushed every `gcAddBatchSize=1000` hashes.
`FlushAdd()` exposed for the mark phase to drain the final partial
batch; `markPhase` calls it after `EnumerateFileBlocks` so the sweep's
`Has()` queries see every marked hash. `Has()` also implicitly flushes
any pending batch as a defensive consistency guarantee for tests that
interleave Add/Has. `Close()` flushes before releasing Badger to avoid
silent loss. Two new regression tests: explicit + implicit flush
semantics, and the auto-flush trigger at the batch boundary. Note
documented that `Add()` is single-goroutine only (matches markPhase's
serial share iteration).

### IN-4-05: gcRootLocks comment doesn't explicitly note per-share scoping

**Files modified:** `pkg/blockstore/engine/gc.go`
**Commit:** `5d9a148e`
**Applied fix:** Added one sentence to the `gcRootLocks` comment block
clarifying that lock granularity is PER-SHARE in practice — each share
owns its own gc-state directory, so concurrent runs against different
shares hit different mutexes and proceed in parallel; only same-share
calls serialize. No behavior change.

## Skipped Issues

### IN-4-03: GC trigger is fully synchronous; no 202+poll alternative — DEFERRED

**File:** `internal/controlplane/api/handlers/block_gc.go` (RunGC handler),
plus runtime + dfsctl plumbing for `?async=true`
**Reason:** DEFERRED to follow-up phase per orchestrator guidance. The
async + 202+poll pattern is a non-trivial design surface (run-id
generation, in-flight registry, cancellation, cross-restart durability,
status endpoint already exists for last-completed but not in-progress).
Doing it well exceeds the scope of a Pass-4 fix sweep. The synchronous
HTTP path remains correct for the Phase-11 single-server deployment;
operators triggering multi-million-object sweeps via cron should use
the local CLI which doesn't have HTTP timeouts. Recommend filing a GH
issue referencing this finding for the next phase.
**Original issue:** HTTP client may time out on multi-million-object
sweeps before completion.

---

_Fixed: 2026-04-25_
_Fixer: Claude (gsd-code-fixer)_
_Iteration: 4_
