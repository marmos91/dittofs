---
phase: 18-syncer-simplification
plan: 03
subsystem: metadata
tags: [metadata, postgres, sync-state, migration, blockstore, content-hash, conformance-suite]

# Dependency graph
requires:
  - phase: 18-syncer-simplification
    plan: 01
    provides: metadata.SyncedHashStore interface + RunSyncedHashStoreSuite conformance harness (commit 417b2660)
  - phase: 10-fastcdc-chunker-hybrid-local-store-a1
    provides: Postgres RollupStore precedent (queryRow/exec pool helpers, integration-tagged test pattern, migration numbering)
provides:
  - Postgres SyncedHashStore backend (IsSynced/MarkSynced/DeleteSynced on *PostgresMetadataStore)
  - Migration 000015_synced_hashes (up + down)
affects:
  - 18-04 (FSStore injection — Postgres backend now satisfies the interface and can be plumbed)
  - 18-06 (Syncer mirror loop — production Postgres deployments can persist sync state)
  - 18-07 (engine.Delete refcount cascade — DeleteSynced fires on refcount=0)

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "ON CONFLICT (pk) DO NOTHING for idempotent presence-marker INSERT"
    - "CHECK (octet_length(col) = N) DDL constraint to enforce wire-byte invariants at the DB layer"
    - "pgx.ErrNoRows sentinel translated to (false, nil) for boolean lookups"
    - "Integration-tagged conformance test gated on DITTOFS_TEST_POSTGRES_DSN"
    - "Provenance-free godoc (no Phase/D-NN references in source)"

key-files:
  created:
    - pkg/metadata/store/postgres/migrations/000015_synced_hashes.up.sql
    - pkg/metadata/store/postgres/migrations/000015_synced_hashes.down.sql
    - pkg/metadata/store/postgres/synced_hash_store.go
    - pkg/metadata/store/postgres/synced_hash_store_test.go
  modified: []

key-decisions:
  - "Reused queryRow/exec pool helpers from pool_helpers.go (10s acquire timeout) — matches rollup.go precedent exactly"
  - "CHECK (octet_length(hash) = 32) at DDL layer mitigates T-18-03-03 (wrong-length hash bypass) without app-side validation"
  - "ON CONFLICT (hash) DO NOTHING — chose DO NOTHING over DO UPDATE because synced_at is monotone-non-decreasing by definition (a hash never un-syncs), so leaving the original synced_at preserves first-synced provenance"
  - "Skipped TRUNCATE in integration test — the shared suite uses distinct hash seeds per subtest and every assertion is idempotent against rows left by prior runs (mirrors rollup_test.go exactly; introducing a test-only Truncate method on the store would have widened the surface area unnecessarily)"

patterns-established:
  - "SyncedHashStore Postgres backend: 3 SQL statements, no transaction, no CTE — simpler than RollupStore because there is no monotone invariant to enforce"
  - "Migration filename + numbering: 000015 follows 000014_add_share_block_layout sequentially; auto-discovered by embed.FS"

requirements-completed: [D-02]

# Metrics
duration: ~25 min
completed: 2026-05-21
---

# Phase 18 Plan 03: Postgres SyncedHashStore + migration 000015 Summary

**Postgres backend implementation of metadata.SyncedHashStore via a dedicated synced_hashes table (hash BYTEA PRIMARY KEY, synced_at TIMESTAMPTZ, 32-byte CHECK constraint), with migration 000015 for forward + rollback and an integration-tagged conformance test gated on DITTOFS_TEST_POSTGRES_DSN.**

## Performance

- **Duration:** ~25 min
- **Tasks:** 3
- **Files created:** 4 (2 migration SQL + 1 backend impl + 1 integration test)
- **Files modified:** 0

## Accomplishments

- Migration `000015_synced_hashes` defines a minimal `synced_hashes (hash BYTEA PRIMARY KEY, synced_at TIMESTAMPTZ NOT NULL DEFAULT NOW())` table with a DB-layer `CHECK (octet_length(hash) = 32)` constraint that rejects any wrong-length hash before it reaches application code (mitigates T-18-03-03).
- Backend implementation on `*PostgresMetadataStore` adds three idempotent methods using the existing `queryRow`/`exec` pool helpers (10s connection-acquire timeout). MarkSynced uses `ON CONFLICT (hash) DO NOTHING` so re-applying the same hash is a no-op; DeleteSynced uses unconditional `DELETE WHERE hash = $1` (zero rows is not an error); IsSynced translates `pgx.ErrNoRows` to `(false, nil)`.
- Integration-tagged conformance test (`//go:build integration` + `DITTOFS_TEST_POSTGRES_DSN` env-gate) calls the shared `metadata.RunSyncedHashStoreSuite` from Plan 18-01. Compiles cleanly with and without the `integration` tag; default `go test ./...` excludes it.

## Task Commits

Each task was committed atomically (signed):

1. **Task 1: Migration 000015 up + down** — `69cab427` (feat)
2. **Task 2: Postgres backend impl (IsSynced / MarkSynced / DeleteSynced)** — `522edd88` (feat)
3. **Task 3: Integration-tagged conformance test** — `ef1f755f` (test)

## Files Created/Modified

- `pkg/metadata/store/postgres/migrations/000015_synced_hashes.up.sql` — DDL with `CHECK (octet_length(hash) = 32)`
- `pkg/metadata/store/postgres/migrations/000015_synced_hashes.down.sql` — `DROP TABLE IF EXISTS synced_hashes`
- `pkg/metadata/store/postgres/synced_hash_store.go` — backend impl, compile-time interface assertion, three methods, `"postgres synced <op>: %w"` wrap convention
- `pkg/metadata/store/postgres/synced_hash_store_test.go` — `TestPostgresSyncedHashStore_Suite`

## Decisions Made

- **`ON CONFLICT DO NOTHING` vs `DO UPDATE`:** A synced hash is monotone — once marked, the "synced_at" timestamp represents *first* successful upload to remote; subsequent MarkSynced calls intentionally preserve the original timestamp rather than overwrite it. Future observability surface (`Count`/`Stats`, deferred per 18-CONTEXT.md `<deferred>`) benefits from a stable "first-synced" attribute.
- **No truncate in integration test:** Plan instructed to "mirror any TRUNCATE pattern in rollup_test.go" — there is none. The conformance suite picks distinct hash seeds per subtest, and every assertion is idempotent against rows left by prior runs. Adding a test-only `TruncateSyncedHashes` method to the store would have widened the public store surface for a problem that does not exist.
- **Pool helpers reused verbatim:** `queryRow` + `exec` already implement the 10s acquire-timeout protection that prevents NFS-handler hangs under pool exhaustion. No new helpers were needed.
- **DB-layer length check vs app-layer:** chose DDL `CHECK` because `blockstore.ContentHash` is a compile-time fixed-size `[32]byte` array, so the only way a wrong-length hash could enter the table is via direct SQL (out-of-band admin write). The CHECK constraint catches that pathway too — defense in depth at near-zero runtime cost.

## Deviations from Plan

None — plan executed exactly as written. The plan offered discretion on TRUNCATE-or-not by qualifying the requirement with "mirror any TRUNCATE pattern in rollup_test.go", and rollup_test.go has no such pattern; the conformance suite design (per-subtest distinct hash seeds + idempotent assertions) already provides the cross-run isolation that TRUNCATE would have enforced.

## Issues Encountered

None.

## User Setup Required

None for the default build/test path. Running the new integration test locally requires a running PostgreSQL at the standard test-fixture endpoint (`localhost:5432`, db `dittofs_test`, user/password `postgres`/`postgres`) and `DITTOFS_TEST_POSTGRES_DSN` set to a non-empty value; otherwise the test cleanly skips.

## Next Phase Readiness

- Plan 18-04 (FSStore injection): can now plumb `*PostgresMetadataStore` into `FSStoreOptions.SyncedHashStore` for production Postgres deployments. All three backends (memory: 18-01, badger: 18-02, postgres: 18-03) now satisfy the interface.
- Plan 18-06 (Syncer mirror loop): the `MarkSynced` Postgres write is a single indexed INSERT — no perf cliff to consider in the Put-then-Mark hot path.
- Plan 18-07 (engine.Delete cascade): `DeleteSynced` is a single indexed DELETE that returns no error on zero rows — idempotent under refcount-cascade races.

## Self-Check: PASSED

Verified:
- `pkg/metadata/store/postgres/migrations/000015_synced_hashes.up.sql` — FOUND
- `pkg/metadata/store/postgres/migrations/000015_synced_hashes.down.sql` — FOUND
- `pkg/metadata/store/postgres/synced_hash_store.go` — FOUND
- `pkg/metadata/store/postgres/synced_hash_store_test.go` — FOUND
- Commit `69cab427` (task 1) — FOUND on branch
- Commit `522edd88` (task 2) — FOUND on branch
- Commit `ef1f755f` (task 3) — FOUND on branch
- `go build ./pkg/metadata/store/postgres/...` — PASS
- `go vet ./pkg/metadata/store/postgres/...` — PASS
- `go vet -tags=integration ./pkg/metadata/store/postgres/...` — PASS
- `go test -count=1 ./pkg/metadata/store/postgres/...` (default, no integration tag) — PASS
- `go build ./...` (full repo sanity) — PASS
- Provenance grep across new files (`Phase 18|D-0[0-9]`) — ZERO matches
- Migration ordering — 000015 directly follows 000014_add_share_block_layout
- Integration test `head -1` — `//go:build integration` (correct first line)

---
*Phase: 18-syncer-simplification*
*Completed: 2026-05-21*
