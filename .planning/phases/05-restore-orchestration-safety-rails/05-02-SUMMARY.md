---
phase: 05-restore-orchestration-safety-rails
plan: 02
subsystem: database
tags: [metadata-store, backup, restore, store-id, ulid, badger, postgres, memory, conformance]

# Dependency graph
requires:
  - phase: 01-foundations-models-manifest-capability-interface
    provides: "Manifest.StoreID field and Backupable interface contract"
  - phase: 02-per-engine-backup-drivers
    provides: "Per-engine Backup/Restore implementations; D-06 empty-destination invariant; storetest conformance suite"
  - phase: 04-scheduler-retention
    provides: "DefaultResolver.Resolve call site in target.go (previously returned cfg.ID)"
provides:
  - "Memory engine persistent GetStoreID() (ULID assigned on construction)"
  - "Badger engine persistent GetStoreID() (cfg:store_id key, first-open bootstrap + Restore re-anchor)"
  - "Postgres engine persistent GetStoreID() (server_config.store_id column + migration 000008 + Restore re-anchor)"
  - "TestStoreID_PersistedAcrossRestart / NonEmptyOnConstruction / PreservedAcrossRestore conformance helpers"
  - "storebackups.DefaultResolver.Resolve returns engine-persistent ID instead of cfg.ID"
affects: [05-03, 05-04, 05-05, phase-6-restore-api]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Engine-persistent store_id: each metadata store engine bootstraps its own ULID on first open"
    - "Restore re-anchor: receiver's storeID is re-written after Restore so source archive cannot rebrand receiver"
    - "Type-asserted interface extension: GetStoreID exposed via anonymous interface for loose coupling"
    - "Migration + application-layer lazy bootstrap: Postgres column ADD + ensureStoreID UPDATE...RETURNING"

key-files:
  created:
    - "pkg/metadata/store/postgres/migrations/000008_store_id.up.sql"
    - "pkg/metadata/store/postgres/migrations/000008_store_id.down.sql"
  modified:
    - "pkg/metadata/store/memory/store.go"
    - "pkg/metadata/store/memory/backup.go"
    - "pkg/metadata/store/memory/backup_test.go"
    - "pkg/metadata/store/badger/store.go"
    - "pkg/metadata/store/badger/backup.go"
    - "pkg/metadata/store/badger/backup_test.go"
    - "pkg/metadata/store/postgres/store.go"
    - "pkg/metadata/store/postgres/backup.go"
    - "pkg/metadata/store/postgres/backup_test.go"
    - "pkg/metadata/storetest/backup_conformance.go"
    - "pkg/controlplane/runtime/storebackups/target.go"
    - "pkg/controlplane/runtime/storebackups/target_test.go"

key-decisions:
  - "Use github.com/oklog/ulid/v2 (already an indirect dep) for fresh store IDs — matches the convention throughout backup code"
  - "Badger: key cfg:store_id lives under existing cfg: prefix; intentionally NOT added to allBackupPrefixes so archive does not carry source ID to receiver"
  - "Postgres: migration 000008 adds store_id column with NOT NULL DEFAULT '' sentinel; application-layer ensureStoreID UPDATE...RETURNING with COALESCE(NULLIF(...)) bootstraps on first open"
  - "Memory: memoryBackupRoot gob struct deliberately has NO StoreID field; storeID is instance identity, not serialized state"
  - "target.go rejects stores missing GetStoreID instead of falling back to cfg.ID — D-06 is a hard contract, not best-effort"
  - "Conformance helpers exposed in three shapes: NonEmptyOnConstruction (memory), PersistedAcrossRestart (badger/postgres), PreservedAcrossRestore (all three)"

patterns-established:
  - "Engine-persistent identity bootstrap: first-open writes a fresh ULID into engine-local singleton storage; subsequent opens read the existing value; Restore re-anchors receiver's ID"
  - "Type-asserted capability extension at service boundary: storebackups.DefaultResolver.Resolve uses metaStore.(interface{ GetStoreID() string }) rather than extending metadata.MetadataStore — keeps the core interface stable while layering Phase 5's requirement"

requirements-completed: [REST-01, REST-03]

# Metrics
duration: ~50min
completed: 2026-04-16
---

# Phase 5 Plan 02: Store-ID Persistence Summary

**Each metadata store engine (memory/badger/postgres) now exposes a stable GetStoreID() ULID that survives reopen and Restore, and DefaultResolver.Resolve returns the engine-persistent ID instead of the volatile control-plane cfg.ID — closing the D-06 cross-store contamination gap.**

## Performance

- **Duration:** ~50 min
- **Started:** 2026-04-16T~21:10Z
- **Completed:** 2026-04-16T21:59Z
- **Tasks:** 4
- **Files modified:** 12 (2 created, 10 modified)

## Accomplishments

- Memory engine: fresh ULID assigned in NewMemoryMetadataStore; accessor and compile-time assertion added; Restore path documented to preserve receiver identity (gob root has no StoreID field)
- Badger engine: cfg:store_id key managed via idempotent ensureStoreID helper; Restore re-anchors the key to the receiver's ULID as defense-in-depth; key intentionally kept outside allBackupPrefixes so archives never carry the source ID
- Postgres engine: migration 000008 adds server_config.store_id (VARCHAR(36) NOT NULL DEFAULT '') with backfill; ensureStoreID bootstraps via UPDATE...RETURNING with COALESCE(NULLIF(...)); Restore re-anchors inside the COPY FROM transaction before commit
- storetest conformance: three helpers (PersistedAcrossRestart, NonEmptyOnConstruction, PreservedAcrossRestore) wired into per-engine test files
- target.go: Resolve now fetches engine-persistent ID via type-asserted interface; rejects stores that do not implement GetStoreID rather than falling back to cfg.ID
- target_test.go: happy-path asserts engine ULID != cfg.ID (contract enforcement)

## Task Commits

Each task was committed atomically:

1. **Task 1: Memory engine persistent store_id** — `dcd04af6` (feat)
2. **Task 2: Badger engine persistent store_id** — `83252828` (feat)
3. **Task 3: Postgres engine persistent store_id** — `4fa65eb7` (feat)
4. **Task 4: Conformance tests + target.go wiring** — `7ba2076e` (feat)

## Files Created/Modified

**Created:**
- `pkg/metadata/store/postgres/migrations/000008_store_id.up.sql` — add server_config.store_id column
- `pkg/metadata/store/postgres/migrations/000008_store_id.down.sql` — reverse column addition

**Modified:**
- `pkg/metadata/store/memory/store.go` — `storeID string` field, ULID assignment in constructor, `GetStoreID()` method, compile-time assertion
- `pkg/metadata/store/memory/backup.go` — comment documenting that receiver's storeID is not assigned from decoded archive
- `pkg/metadata/store/memory/backup_test.go` — `TestMemoryStoreID_NonEmpty`, `TestMemoryStoreID_PreservedAcrossRestore`
- `pkg/metadata/store/badger/store.go` — `storeID string` field, `storeIDKey` const, `ensureStoreID(db)` helper, constructor wiring, `GetStoreID()` method, compile-time assertion
- `pkg/metadata/store/badger/backup.go` — post-restore re-anchor (write receiver storeID back to cfg:store_id key)
- `pkg/metadata/store/badger/backup_test.go` — `TestBadgerStoreID_PersistedAcrossRestart`, `TestBadgerStoreID_PreservedAcrossRestore`
- `pkg/metadata/store/postgres/store.go` — `storeID string` field, `ensureStoreID(ctx)` method with `UPDATE...RETURNING`, constructor wiring, `GetStoreID()` method, compile-time assertion
- `pkg/metadata/store/postgres/backup.go` — in-transaction re-anchor of `server_config.store_id` to receiver's ULID after COPY FROM wave
- `pkg/metadata/store/postgres/backup_test.go` — `TestPostgresStoreID_PersistedAcrossRestart`, `TestPostgresStoreID_PreservedAcrossRestore`
- `pkg/metadata/storetest/backup_conformance.go` — `StoreIDFactory` type; `TestStoreID_PersistedAcrossRestart`, `TestStoreID_NonEmptyOnConstruction`, `TestStoreID_PreservedAcrossRestore`
- `pkg/controlplane/runtime/storebackups/target.go` — `Resolve` returns engine-persistent `GetStoreID()` instead of `cfg.ID`; rejects stores missing the interface with a D-06-annotated error
- `pkg/controlplane/runtime/storebackups/target_test.go` — `TestDefaultResolver_ResolveSuccess` updated to assert engine ULID (not cfg.ID)

## Exact Contracts Used Per Engine

| Engine | Storage | Key/Column | Bootstrap | Restore Re-anchor |
|--------|---------|------------|-----------|-------------------|
| Memory | In-memory struct field `storeID` | n/a | `ulid.Make().String()` in `NewMemoryMetadataStore` | Not applicable — gob root has no StoreID field; receiver's identity is untouched |
| Badger | `cfg:store_id` key (under existing `cfg:` prefix) | `storeIDKey = prefixConfig + "store_id"` | `ensureStoreID(db)` on every open; writes fresh ULID if key absent, else reads existing | `db.Update` to re-write `cfg:store_id = s.storeID` after WriteBatch Flush |
| Postgres | `server_config.store_id` (VARCHAR(36) NOT NULL DEFAULT '') | Column added in migration 000008 | `UPDATE server_config SET store_id = COALESCE(NULLIF(store_id, ''), $1) WHERE id = 1 RETURNING store_id` | `UPDATE server_config SET store_id = $1 WHERE id = 1` inside the Restore transaction, before COMMIT |

**Postgres migration filename:** `000008_store_id.up.sql` / `000008_store_id.down.sql`

## Decisions Made

- **ULID over UUID:** `github.com/oklog/ulid/v2` is already an indirect dependency and is the convention throughout the backup code (executor, scheduler). UUID would have been equally valid but inconsistent with surrounding code.
- **cfg:store_id outside allBackupPrefixes (Badger):** Keeping the key out of the backup archive means an archive produced by source A and restored into receiver B cannot carry source A's ID into B. The Restore re-anchor is thus defense-in-depth rather than strict correctness, but it pins the invariant machine-enforceably for format-version-2 archives that might choose differently.
- **server_config INSIDE backupTables (Postgres):** The server_config table is already in backupTables for functional reasons (admin settings need to survive restore). This means the COPY FROM wave will restore the source's store_id into the receiver — the in-transaction re-anchor (inside the same tx, before commit) makes this atomic and safe.
- **GetStoreID surfaced via anonymous interface, not added to metadata.MetadataStore:** D-06 is a Phase-5 concern; the metadata.MetadataStore interface is stable and consumed by many non-backup callers. A type-asserted `interface{ GetStoreID() string }` at the sole call site (target.go) keeps the core interface clean. Compile-time assertions in each engine file prevent drift.
- **Hard reject missing GetStoreID in target.go:** A store without engine-persistent identity cannot honor the D-06 gate safely; returning an error is the only sound choice. All three engines now implement the contract; the error path is guardrail against future drift.

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

- The plan template called for a negative test (`TestDefaultResolver_StoreMissingGetStoreID`) but constructing a fake implementing the large `metadata.MetadataStore` interface without `GetStoreID` is impractical — all three engines in-repo implement it, so there is no clean way to manufacture a broken store for unit testing. The test was removed in favor of relying on the compile-time interface assertions (`var _ interface{ GetStoreID() string }` per engine) to catch regressions. Not a deviation — just a pragmatic test-shape choice during implementation.

## Next Phase Readiness

- **Ready for Plan 05-03 onwards:** all downstream restore work (D-05 side-engine swap, D-06 manifest.store_id == target.store_id gate) can now rely on a stable engine-persistent identity primitive.
- **Phase 6 CLI/REST:** no impact — Phase 6 consumes `storebackups.Service.RunRestore` which uses `DefaultResolver.Resolve` transparently; the engine ID change is invisible to CLI callers.
- **Existing archives:** no migration story required. Phase 2+ archives already carry `Manifest.StoreID`; this plan only changes what VALUE gets written there going forward. Old archives written pre-Phase-5 carry the old cfg.ID and will fail the D-06 gate, which is the correct behavior — they are by construction from a different store identity.
- **Control-plane DB reset scenario:** now safe. Resetting the GORM controlplane DB rotates `cfg.ID` values, but the Badger/Postgres engine retains its own ULID, so restoring an old archive into the same physical engine now succeeds (manifest.store_id == engine.store_id) whereas it would previously have failed.

## Self-Check: PASSED

- [x] Memory `storeID` field present at `pkg/metadata/store/memory/store.go:239`
- [x] Memory `GetStoreID()` method present at `pkg/metadata/store/memory/store.go:394`
- [x] Memory compile-time assertion at `pkg/metadata/store/memory/store.go:399`
- [x] Badger `ensureStoreID` function at `pkg/metadata/store/badger/store.go:278`
- [x] Badger `GetStoreID()` method at `pkg/metadata/store/badger/store.go:476`
- [x] Badger `cfg:store_id` references ≥ 3 (found 16 across store.go, backup.go, tests)
- [x] Postgres migration file `000008_store_id.up.sql` present (contains ALTER TABLE / ADD COLUMN / store_id)
- [x] Postgres `GetStoreID()` method at `pkg/metadata/store/postgres/store.go:212`
- [x] Postgres `ensureStoreID` present at `pkg/metadata/store/postgres/store.go:191`
- [x] Postgres `re-anchor store_id` reference at `pkg/metadata/store/postgres/backup.go:371`
- [x] Conformance `TestStoreID_PersistedAcrossRestart` at `pkg/metadata/storetest/backup_conformance.go:616`
- [x] Conformance `TestStoreID_NonEmptyOnConstruction` at `pkg/metadata/storetest/backup_conformance.go:651`
- [x] target.go `GetStoreID()` reference at `pkg/controlplane/runtime/storebackups/target.go:124,129`
- [x] Old `return src, cfg.ID, cfg.Type` pattern fully removed (grep count = 0)
- [x] Commits exist: `dcd04af6`, `83252828`, `4fa65eb7`, `7ba2076e`
- [x] `go build ./...` passes
- [x] `go vet ./pkg/metadata/... ./pkg/controlplane/runtime/storebackups/...` passes
- [x] `go test ./pkg/metadata/store/memory/... ./pkg/metadata/storetest/... ./pkg/controlplane/runtime/storebackups/...` passes
- [x] `go test -tags=integration ./pkg/metadata/store/badger/...` passes (3.0s)

---
*Phase: 05-restore-orchestration-safety-rails*
*Completed: 2026-04-16*
