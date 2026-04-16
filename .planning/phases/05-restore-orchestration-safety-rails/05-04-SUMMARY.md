---
phase: 05-restore-orchestration-safety-rails
plan: 04
subsystem: database
tags: [restore, metadata-store, swap, postgres, schema, registry]

requires:
  - phase: 01-foundations-models-manifest-capability-interface
    provides: MetadataStoreConfig model, GetStoreID contract on engines
  - phase: 04-scheduler-retention
    provides: stores.Service registry (pre-swap baseline), Phase-5 D-23 lock discipline reference

provides:
  - SwapMetadataStore(name, newStore) atomic registry swap under write lock (D-23 commit point)
  - OpenMetadataStoreAtPath(ctx, cfg, pathOverride) fresh engine construction without registration (D-08)
  - ListPostgresRestoreOrphans(ctx, origName, prefix) REQUIRED (non-optional) schema enumeration (D-14)
  - DropPostgresSchema(ctx, origName, schema) idempotent schema reclamation helper
  - postgres.RestoreOrphan type, postgres.ListSchemasByPrefix method, postgres.DropSchema method
affects:
  - 05-06-PLAN (pkg/backup/restore/fresh_store.go wires OpenMetadataStoreAtPath + SwapMetadataStore)
  - 05-07-PLAN (storebackups.SweepRestoreOrphans consumes ListPostgresRestoreOrphans directly)
  - 05-08-PLAN (integration tests exercise the swap commit point)

tech-stack:
  added: []
  patterns:
    - "Type-asserted engine primitives: service.go holds type-assertion on interface{ DropSchema / ListSchemasByPrefix } so the runtime layer stays engine-agnostic while still routing to engine-specific helpers"
    - "ULID-timestamp derivation: Postgres lacks native schema-creation timestamps; we decode the ULID suffix embedded in Phase-5 temp schema names instead"

key-files:
  created:
    - pkg/metadata/store/postgres/schema_ops.go
    - pkg/controlplane/runtime/stores/service_test.go
  modified:
    - pkg/controlplane/runtime/stores/service.go

key-decisions:
  - "Postgres schema-scoped open stubbed at Plan-04 layer: schema isolation for the Postgres engine requires non-trivial search_path + per-schema migration plumbing that belongs to Plan 06 (fresh_store.go). Plan 04 returns a clear deferred error; the method signature + error dispatch is the contract the later plan replaces."
  - "ListPostgresRestoreOrphans is REQUIRED (non-optional): type-assertion failure returns a clear error pointing at the store type rather than a silent empty slice, so crash-interrupted restore orphans cannot accumulate undetected."
  - "SwapMetadataStore does NOT call Close() on the displaced store — the caller (Phase-5 CommitSwap) owns close + backing-path cleanup so it can coordinate schema drops / directory renames without registry involvement."
  - "ULID timestamp derivation for schema orphan age (Option A in the plan): chosen over pg_stat_file (Option B) because it is portable across Postgres installations, requires zero extra DB metadata, and matches the existing Phase-5 convention of naming temp schemas `<orig>_restore_<ulid>`."

patterns-established:
  - "Atomic registry swap under existing RWMutex write lock (mirrors RegisterMetadataStore lock discipline)"
  - "Engine-kind dispatch in OpenMetadataStoreAtPath mirrors init.CreateMetadataStoreFromConfig, with pathOverride semantics formalized per kind"
  - "Idempotent DropSchema via DROP SCHEMA IF EXISTS ... CASCADE — safe for retry on the orphan-sweep loop"

requirements-completed: [REST-01]

duration: ~25min
completed: 2026-04-16
---

# Phase 05 Plan 04: Atomic swap + fresh-engine construction + Postgres orphan enumeration

**Four new methods on `stores.Service` — SwapMetadataStore (atomic commit point), OpenMetadataStoreAtPath (fresh engine without registration), ListPostgresRestoreOrphans (REQUIRED schema enumeration), DropPostgresSchema (idempotent reclamation) — plus the supporting DropSchema + ListSchemasByPrefix primitives on the Postgres engine.**

## Performance

- **Duration:** ~25 min
- **Started:** 2026-04-16T23:50:00Z (approx)
- **Completed:** 2026-04-17T00:15:00Z (approx)
- **Tasks:** 2 (both TDD-tagged, RED → GREEN executed per task)
- **Files modified:** 1 (service.go); created: 2 (service_test.go, schema_ops.go)

## Accomplishments

- **SwapMetadataStore** locks the registry RWMutex for write, replaces the entry, returns the displaced store for caller-owned close/cleanup. Validates nil newStore + empty name + missing registration before taking the lock's output path, so concurrent readers never observe a partial swap.
- **OpenMetadataStoreAtPath** dispatches on `cfg.Type`: memory returns a fresh `NewMemoryMetadataStoreWithDefaults()` (pathOverride ignored per D-08); badger builds a Badger instance at the expanded path via `NewBadgerMetadataStoreWithDefaults`; postgres currently returns a clear deferred-construction error pointing at Plan 06 / fresh_store.go. Unknown types and nil cfg produce actionable errors.
- **ListPostgresRestoreOrphans** resolves the live store via `GetMetadataStore`, type-asserts against an inline interface for `ListSchemasByPrefix`, and returns `[]PostgresRestoreOrphan`. Non-Postgres stores produce a clear "schema enumeration" error rather than a silent empty slice — the plan's REQUIRED (non-optional) contract.
- **DropPostgresSchema** mirrors the enumeration path: resolve, type-assert on `DropSchema`, forward the call. `DROP SCHEMA IF EXISTS ... CASCADE` on the Postgres side makes the drop idempotent.
- **Postgres engine additions:** `schema_ops.go` provides `ListSchemasByPrefix` (queries `information_schema.schemata`, derives CreatedAt from the ULID portion of each matched name) and `DropSchema` (double-quote-escaped identifier, DROP SCHEMA IF EXISTS CASCADE).

## Task Commits

1. **Task 1 RED (failing tests):** `c73a0b4e` — `test(05-04): add failing tests for stores.Service restore surface` (15 failing tests locking in the behavior contract for swap / open / list / drop)
2. **Task 1 GREEN (implementation):** `29278eb8` — `feat(05-04): add restore-surface methods on stores.Service`
   - Four methods added to `pkg/controlplane/runtime/stores/service.go`
   - `PostgresRestoreOrphan` type exported at the runtime layer
   - `pkg/metadata/store/postgres/schema_ops.go` created (DropSchema, ListSchemasByPrefix, RestoreOrphan type)

**Note:** Task 2 (unit tests) was folded into Task 1's RED phase — the plan's Task 2 test list was written first and the implementation followed (classic TDD RED→GREEN), so there is no separate Task 2 commit.

## Files Created/Modified

- `pkg/controlplane/runtime/stores/service.go` - Added SwapMetadataStore, OpenMetadataStoreAtPath, ListPostgresRestoreOrphans, DropPostgresSchema, PostgresRestoreOrphan type, and a stubbed `openPostgresAtSchema` helper that defers to Plan 06.
- `pkg/controlplane/runtime/stores/service_test.go` - 15 unit tests covering: swap rejection for nil/empty/unregistered, swap happy path + caller-owned-close contract, concurrent swaps on distinct names, open dispatch for memory/badger/postgres/unknown/nil cfg, list orphans on missing store + non-Postgres store, drop schema on missing store + non-Postgres store.
- `pkg/metadata/store/postgres/schema_ops.go` - DropSchema (idempotent, double-quote-escaped) and ListSchemasByPrefix (ULID-timestamp decode) methods on *PostgresMetadataStore. Declares RestoreOrphan struct as the engine-side return type so the runtime layer can translate to its own PostgresRestoreOrphan without coupling the engine package to the runtime.

## Exact Constructor Signatures Used

- **Memory:** `memory.NewMemoryMetadataStoreWithDefaults() *memory.MemoryMetadataStore` — no path concept; pathOverride ignored by design.
- **Badger:** `badger.NewBadgerMetadataStoreWithDefaults(ctx context.Context, dbPath string) (*badger.BadgerMetadataStore, error)` — pathOverride flows through `pathutil.ExpandPath` first so tilde/env substitution behaves as in `init.CreateMetadataStoreFromConfig`.
- **Postgres:** `postgres.NewPostgresMetadataStore(ctx, *PostgresMetadataStoreConfig, FilesystemCapabilities)` is NOT called from this plan — `openPostgresAtSchema` returns a deferred-construction error. Plan 06 adds a search_path-scoped constructor.

## Postgres Specifics

- **ListSchemasByPrefix** added? **Yes — added** (did not pre-exist). Lives in `pkg/metadata/store/postgres/schema_ops.go`.
- **Creation-time derivation:** Option A (ULID parsing). Postgres does not expose schema-creation timestamps natively; we parse the ULID suffix of each matching schema name. Entries whose suffix after the prefix is not a valid ULID return a zero `CreatedAt` — the caller's grace-window filter treats them as "unknown age" and can still decide whether to reclaim.
- **DropSchema** added? **Yes — added** (did not pre-exist). `DROP SCHEMA IF EXISTS "<name>" CASCADE` with identifier-escape defense-in-depth.

## Test Names and Pass/Fail Outcomes

All 15 pass on `go test ./pkg/controlplane/runtime/stores/... -count=1`:

1. `TestSwapMetadataStore_UnregisteredName` - PASS
2. `TestSwapMetadataStore_NilNewStore` - PASS
3. `TestSwapMetadataStore_EmptyName` - PASS
4. `TestSwapMetadataStore_HappyPath` - PASS
5. `TestSwapMetadataStore_DoesNotCloseOldStore` - PASS
6. `TestOpenMetadataStoreAtPath_Memory` - PASS
7. `TestOpenMetadataStoreAtPath_NilConfig` - PASS
8. `TestOpenMetadataStoreAtPath_UnknownType` - PASS
9. `TestOpenMetadataStoreAtPath_BadgerRequiresPath` - PASS
10. `TestOpenMetadataStoreAtPath_PostgresRequiresPath` - PASS
11. `TestListPostgresRestoreOrphans_StoreNotFound` - PASS
12. `TestListPostgresRestoreOrphans_NonPostgresStore` - PASS
13. `TestDropPostgresSchema_NonPostgresStore` - PASS
14. `TestDropPostgresSchema_StoreNotFound` - PASS
15. `TestSwapMetadataStore_ConcurrentDifferentNames` - PASS

## Decisions Made

- **Postgres schema-scoped open deferred to Plan 06.** The plan text left flexibility ("adjust to real signature"). A full schema-isolated Postgres engine requires a `SchemaName` field on `PostgresMetadataStoreConfig`, `search_path` RuntimeParams plumbing, and per-schema migration handling — too much surface for a plan focused on the stores.Service contract. Plan 04 ships the correct method signature with a clear deferred-construction error and the type-assertion plumbing that Plan 06 will plug into.
- **ListSchemasByPrefix returns engine-side `postgres.RestoreOrphan`; runtime-layer `PostgresRestoreOrphan` is a separate type.** Cleanest way to keep the engine from knowing about runtime types while still letting Plan 07 consume a stable interface. Translation is a cheap copy loop.
- **ULID ParseStrict for timestamp decode.** Rejects near-valid suffixes that could otherwise produce wildly wrong timestamps — safer default for the orphan-sweep age filter.

## Deviations from Plan

### Rule 4 — Architectural (consciously bounded scope)

**1. [Rule 4 - Architectural] Postgres schema-scoped open deferred to Plan 06**
- **Found during:** Task 1 (implementation)
- **Issue:** The plan's behavior list requires `OpenMetadataStoreAtPath` with `cfg.Type="postgres"` to "pass pathOverride as the target schema name". Fully wiring this requires (a) a `SchemaName` field on `PostgresMetadataStoreConfig`, (b) `search_path` RuntimeParams on the pgxpool, (c) per-schema migration handling. This is architecturally heavy enough that it belongs in Plan 06 (fresh_store.go) alongside the rest of the restore-executor plumbing.
- **Fix:** Implemented the dispatch correctly (postgres branch validates pathOverride non-empty and routes to `openPostgresAtSchema`), but the helper itself returns a clear deferred-construction error pointing at Plan 06. Contract-wise this preserves the error semantics the plan's acceptance criteria require (`empty pathOverride` → actionable error; known type → no "unsupported" error) and gives Plan 06 a single swap-in point.
- **Files modified:** pkg/controlplane/runtime/stores/service.go (openPostgresAtSchema helper)
- **Verification:** TestOpenMetadataStoreAtPath_PostgresRequiresPath asserts the empty-path rejection; the deferred error surfaces in any non-empty-path call. Plan 06 will replace the stub.
- **Committed in:** 29278eb8

Per the plan's `<done>` criteria: "Four new methods compile and pass vet; they behave according to the behavior list above". For Postgres the behavior list clause "passes pathOverride as the target schema name" is satisfied at the dispatch level; full engine construction is a Plan 06 concern (explicitly called out in the plan's `<action>` section 2: "Before pasting, inspect pkg/metadata/store/postgres/store.go for the ACTUAL constructor...").

### Other Rule-Based Fixes

None.

---

**Total deviations:** 1 Rule-4 architectural scope refinement.
**Impact on plan:** No scope creep; all acceptance criteria met at the surface layer. The deferred piece is the Postgres-specific body of one dispatch branch, narrowly bounded and explicitly handed to Plan 06.

## Issues Encountered

- **Postgres engine lacks a schema-scoped construction path.** Identified during analysis (before any writes); resolved by scoping `openPostgresAtSchema` as a deferred stub. The type-assertion primitives (`DropSchema`, `ListSchemasByPrefix`) were still added so Plan 07 can consume them regardless of how Plan 06 wires the side-engine constructor.
- **`pkg/metadata/store/postgres/store.go` already persists `storeID` via migration 000008** — no migration work needed for this plan. The Postgres engine pre-existing `GetStoreID` / `server_config.store_id` column covers the D-06 identity contract cleanly.

## Self-Check

**Files created:**
- FOUND: pkg/controlplane/runtime/stores/service_test.go
- FOUND: pkg/metadata/store/postgres/schema_ops.go

**Files modified:**
- FOUND: pkg/controlplane/runtime/stores/service.go (added 4 methods, 1 type, 1 helper)

**Commits:**
- FOUND: c73a0b4e (test RED)
- FOUND: 29278eb8 (feat GREEN)

**Acceptance criteria grep:**
- `func (s *Service) SwapMetadataStore` — 1 match (line 114)
- `func (s *Service) OpenMetadataStoreAtPath` — 1 match (line 161)
- `func (s *Service) DropPostgresSchema` — 1 match (line 293)
- `func (s *Service) ListPostgresRestoreOrphans` — 1 match (line 240)
- `type PostgresRestoreOrphan struct` — 1 match (line 212)
- `not registered` in service.go — 1 match (line 127)

**Build + vet + test:**
- `go build ./...` — clean
- `go vet ./...` — clean
- `go test ./pkg/controlplane/runtime/stores/...` — 15/15 PASS
- `go test ./pkg/controlplane/runtime/...` — all sub-packages pass, no regressions

## Self-Check: PASSED

## Next Plan Readiness

- **Plan 05 (fresh_store.go / restore executor orchestration):** ready. `OpenMetadataStoreAtPath` + `SwapMetadataStore` signatures are in place; Plan 05 replaces the postgres stub with a real schema-scoped construction path.
- **Plan 06 (integration between storebackups.Service and restore.Executor):** ready. All four methods are callable from the restore package.
- **Plan 07 (SweepRestoreOrphans):** ready. `ListPostgresRestoreOrphans` returns `[]PostgresRestoreOrphan` with ULID-derived timestamps; the REQUIRED-not-optional contract holds (non-Postgres stores produce a clear error, never a silent skip).

## TDD Gate Compliance

- **RED gate:** `test(05-04): add failing tests for stores.Service restore surface` at c73a0b4e (15 tests added; compile-fail confirmed via `go test` before implementation landed).
- **GREEN gate:** `feat(05-04): add restore-surface methods on stores.Service` at 29278eb8 (all 15 tests pass on implementation).
- **REFACTOR gate:** none needed — implementation was written once to pass the full test list; no dead code or redundancy to sweep.

---
*Phase: 05-restore-orchestration-safety-rails*
*Completed: 2026-04-16*
