# Phase 11 Code Review — Pass 4

**Date:** 2026-04-25
**Branch:** gsd/phase-11-cas-write-path-a2
**Pass:** 4 of 5
**Status:** issues_found

## Summary
- 0 BLOCKER
- 2 HIGH (WR-4-01, WR-4-02)
- 5 INFO (IN-4-01..IN-4-05)
- All pass 1+2+3 fixes confirmed clean

## HIGH

### WR-4-01: Postgres `PutFileBlock` violates the contract IN-3-02 documented; dedup short-circuit fails on every cross-file content collision
**Files:** `pkg/blockstore/store.go:18-42`, `pkg/metadata/store/postgres/objects.go:77-95`, `pkg/metadata/store/postgres/migrations/000010_file_blocks.up.sql:29-30`

Pass 3 IN-3-02 documented "PutFileBlock returns nil for any hash-already-present-on-another-row case." Postgres backend uses `idx_file_blocks_hash UNIQUE WHERE hash IS NOT NULL` + `INSERT ... ON CONFLICT (id) DO UPDATE` (only catches PK conflicts). When `engine.uploadOne` dedup hits Postgres with a hash that's already present on another row (e.g. all-zero block from a different VM image file), the INSERT errors with "duplicate key value violates unique constraint". `uploadOne` returns the error → fb stays Syncing → janitor requeues every claim_timeout forever. Donor refcount permanently leaked.

Reachability: TRIVIAL on VM workloads (project's documented primary use case). Memory + Badger silently overwrite their hash→id index; only Postgres breaks.

**Fix options:**
- (A) Drop the unique constraint to a regular index (smallest diff, ships with Phase 11)
- (B) Add `ON CONFLICT (hash) WHERE hash IS NOT NULL DO NOTHING` to the INSERT
- (C) Drop the dedup short-circuit entirely (CAS PUTs are idempotent)

Recommend (A). Plus add a `testPutFileBlock_TwoIDsSameHash` to `pkg/metadata/storetest/file_block_ops.go` so this can't regress.

### WR-4-02: GC sweep `LastModified.IsZero()` short-circuit fails OPEN
**File:** `pkg/blockstore/engine/gc.go:428-431`

Current logic:
```go
if !obj.LastModified.IsZero() &&
    obj.LastModified.After(snapshotTime.Add(-gracePeriod)) {
    continue
}
```
Zero LastModified → falls through to live-set check → if absent from gcs, deleted regardless of age. Production S3 + in-tree memory always populate LastModified, so latent today, but any third-party backend or test fixture missing LastModified silently violates INV-04.

**Fix:** Treat zero LastModified as fail-closed (preserve + capture as error). Tighten contract on `remote.go:23-26` from SHOULD to MUST. Add conformance assertion in `pkg/blockstore/remote/remotetest/`.

## INFO

### IN-4-01: `000010_file_blocks.down.sql` is destructive without warning header
Add doc warning. Operator-facing footgun.

### IN-4-02: `GCStatus` handler still leaks internal err strings to client (3 sites)
**File:** `internal/controlplane/api/handlers/block_gc.go:146,163,170`
Pass 2 IN-2-01 only fixed RunGC; GCStatus has the same pattern in 3 places. Wrap with generic message + log details.

### IN-4-03: GC trigger is fully synchronous; no 202+poll alternative
HTTP client may time out on multi-million-object sweeps before completion. Document loudly OR add `?async=true` query param returning 202 + run_id.

### IN-4-04: `GCState.Add` is one Badger transaction per call
Throughput cliff for 10M+ live sets. Add batching (1000-hash commits). Performance only; no correctness.

### IN-4-05: gcRootLocks comment doesn't explicitly note per-share scoping at lock granularity
Doc nit. Add one sentence to the lock-registry comment.

## Considered and Discarded
- HTTP verb safety (chi v5 returns 405 by default) — confirmed correct
- dfsctl JSON output stability — confirmed (BlockStoreGCResult has explicit JSON tags)
- LocalStore narrowed interface implementors — only fs.FSStore + memory.MemoryStore in-tree
- e2e build tags — all test/e2e/ files have `//go:build e2e`
- WR-3-01 cross-SIGKILL safety — CleanStaleGCStateDirs handles fail-clean recovery
- GC-status 404-vs-empty — handler returns 404 with body when no run; CLI tests for this
- Server↔S3 clock skew — default 1h grace absorbs routine NTP drift; IN-2-03's <5m hard-reject closes operator-misconfig window
- syncer claim_timeout vs metadata-store clock skew — both use engine wall clock, no drift
- Architecture invariant #5 (WRITE order) — unchanged by Phase 11
