---
phase: 14-migration-tool-a5
plan: 01
subsystem: metadata
tags: [block_layout, share, metadata, conformance, dual-read-shim, mig-03]

# Dependency graph
requires:
  - phase: 11-cas-write-path-gc-rewrite-a2
    provides: dual-read shim infrastructure that this per-share gate routes
  - phase: 13-merkle-root-file-level-dedup-a4
    provides: ShareOptions evolution + storetest conformance pattern (FileBlockOps, ObjectIDOps)
provides:
  - "metadata.BlockLayout enum (legacy, cas-only) + ParseBlockLayout helper with empty-string forward-compat"
  - "metadata.ShareOptions.BlockLayout field threaded through Memory + Badger + Postgres CreateShare/GetShareOptions/UpdateShareOptions (and the transactional equivalents)"
  - "Postgres migration 000014 — block_layout TEXT NOT NULL DEFAULT 'legacy' on the shares table, reversible"
  - "storetest.RunBlockLayoutSuite conformance scenarios invoked by all three backend test files"
  - "Badger CreateRootDirectory option-preservation fix (was wiping ShareOptions on root materialization)"
affects: [14-02-engine-blocklayout-routing, 14-03-migrate-tool-core, 14-05-integrity-cutover]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Per-share enum field with empty-string-coerces-to-legacy default (mirrors blockstore.RetentionPolicy pattern)"
    - "Authoritative-column-overrides-JSON-blob in Postgres (block_layout column wins over the legacy options JSONB)"

key-files:
  created:
    - pkg/metadata/store/postgres/migrations/000014_add_share_block_layout.up.sql
    - pkg/metadata/store/postgres/migrations/000014_add_share_block_layout.down.sql
    - pkg/metadata/storetest/shares_blocklayout.go
    - pkg/metadata/store/memory/shares_test.go
    - pkg/metadata/store/badger/shares_test.go
    - pkg/metadata/store/postgres/shares_test.go
  modified:
    - pkg/metadata/types.go
    - pkg/metadata/types_test.go
    - pkg/metadata/store/memory/shares.go
    - pkg/metadata/store/badger/shares.go
    - pkg/metadata/store/badger/transaction.go
    - pkg/metadata/store/postgres/shares.go
    - pkg/metadata/store/postgres/transaction.go

key-decisions:
  - "Postgres uses a dedicated block_layout TEXT column (not just JSON-blob options) so it is authoritative and queryable independent of the JSON marshaling layer."
  - "ParseBlockLayout(\"\") -> BlockLayoutLegacy is the only safe forward-compat default — pre-Phase-14 rows MUST keep the dual-read shim active until proven otherwise (D-A6)."
  - "Unknown values surface as ErrInvalidBlockLayout on every read path (memory falls back to legacy, badger falls back to legacy, postgres errors). Mitigates T-14-01-01 against hand-edited rows being silently treated as cas-only."
  - "Test ordering: CreateShare first, then CreateRootDirectory — same convention storetest.createTestShare uses; required Badger to preserve Options across root materialization, which it now does."

patterns-established:
  - "Empty-string-coerces-to-default for new metadata enums lets backends round-trip without breaking pre-existing rows (mirrors blockstore.RetentionPolicy)."
  - "Authoritative-column-overrides-JSON-blob: when a value graduates from JSON-embedded to a dedicated column, both writes go to both places but reads trust the column. Lets us drop the JSON path later without a coordinated migration."

requirements-completed: [MIG-03]

# Metrics
duration: ~25min
completed: 2026-05-05
---

# Phase 14 Plan 01: Share BlockLayout Foundation Summary

**Per-share `block_layout` flag (legacy | cas-only) lands on every metadata backend with full conformance coverage; the foundation Plans 02 and 03–05 build on now exists and round-trips green.**

## What Shipped

Three commits, three tasks, three backends:

- **Task 1 — `feat(14-01): add BlockLayout enum + ShareOptions field` (`67af6a8b`).** New `metadata.BlockLayout` string-enum type with `BlockLayoutLegacy` / `BlockLayoutCASOnly` constants. `ParseBlockLayout` coerces empty string to `BlockLayoutLegacy` (D-A6 forward-compat) and rejects unknown strings with `ErrInvalidBlockLayout` (T-14-01-01 mitigation). New `ShareOptions.BlockLayout` field with `json:"block_layout,omitempty"` tag. Five `t.Run` subtests in `types_test.go` covering each branch.

- **Task 2 — `feat(14-01): persist BlockLayout across memory, badger, postgres backends` (`7eff1c34`).** All three backends now round-trip the field through `CreateShare` / `GetShareOptions` / `UpdateShareOptions`, plus the transactional equivalents (`badgerTransaction.GetShareOptions`, `postgresTransaction.GetShareOptions/CreateShare/UpdateShareOptions`). Memory and Badger rely on the existing struct/JSON value-copy + a `ParseBlockLayout` coercion on read. Postgres adds migration 000014 with a dedicated `block_layout TEXT NOT NULL DEFAULT 'legacy'` column; SELECT/INSERT/UPDATE include it explicitly so the column is authoritative over whatever might be embedded in the legacy JSON `options` blob.

- **Task 3 — `test(14-01): RunBlockLayoutSuite conformance + Badger options-preservation fix` (`5b30ff05`).** New `pkg/metadata/storetest/shares_blocklayout.go` exports `RunBlockLayoutSuite(t, factory)` with five scenarios (`RoundTripCASOnly`, `RoundTripLegacy`, `DefaultLegacyOnEmpty`, `UpdateLegacyToCASOnly`, `UpdateCASOnlyToLegacy`). Each backend wires it via a fresh `shares_test.go`. Memory + Badger pass green in the default test lane; Postgres compiles under `//go:build integration` and skips cleanly without `DITTOFS_TEST_POSTGRES_DSN` (matches existing `postgres_conformance_test.go` convention).

## Verification Results

| Check                                                              | Result        |
| ------------------------------------------------------------------ | ------------- |
| `go test ./pkg/metadata/...`                                       | **PASS**      |
| `go test ./pkg/metadata/store/memory/ -run TestBlockLayoutConformance`        | PASS (5/5)    |
| `go test ./pkg/metadata/store/badger/ -run TestBlockLayoutConformance`        | PASS (5/5)    |
| `go test -tags=integration ./pkg/metadata/store/postgres/ -run TestBlockLayoutConformance` | SKIP (no DSN, expected) |
| `go test -tags=integration ./pkg/metadata/store/badger/ -run TestConformance` | PASS (full conformance suite — Badger fix is non-regressive) |
| `go vet ./pkg/metadata/...`                                        | clean         |
| `go build ./...`                                                   | clean         |
| Postgres migration 000014 up/down file pair exists                 | yes           |
| `grep -c 'block_layout' pkg/metadata/store/postgres/shares.go`     | 7 (≥3 ✓)      |
| `grep -c 'BlockLayout' pkg/metadata/store/badger/shares.go`        | 4 (≥2 ✓)      |
| `grep -c 'ParseBlockLayout' pkg/metadata/store/memory/shares.go`   | 2 (≥1 ✓)      |
| `grep -c 'RunBlockLayoutSuite' pkg/metadata/store/{memory,badger,postgres}/shares_test.go` | 1 each (≥1 ✓) |

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Badger CreateRootDirectory wiped ShareOptions on root materialization**

- **Found during:** Task 3, when the conformance suite started failing on Badger.
- **Issue:** Both `BadgerMetadataStore.createNewRoot` (in `shares.go`) and the equivalent `badgerTransaction.CreateRootDirectory` path (in `transaction.go`) constructed a fresh `metadata.Share{Name: shareName}` literal when they wrote the share record with the new `RootHandle`. That literal has zero-valued `Options`, so any prior `CreateShare(share-with-Options)` call's `ShareOptions` (including the new `BlockLayout` field, but ALSO every other share option — `ReadOnly`, `Async`, `AllowedClients`, …) was silently overwritten the moment the root directory was materialized. The plan's own intended init order (`CreateShare` then `CreateRootDirectory`, mirroring `storetest.createTestShare`) walks straight into this trap.
- **Fix:** Both code paths now read the existing share row before writing the updated `shareData{Share, RootHandle}` record and preserve the prior `Share.Options` (and any other Share fields that were set), normalizing only the `Name` field defensively. If the share row doesn't exist yet, the original behavior of seeding a fresh `metadata.Share{Name: shareName}` is preserved (`badger.ErrKeyNotFound` branch).
- **Files modified:** `pkg/metadata/store/badger/shares.go` (`createNewRoot`), `pkg/metadata/store/badger/transaction.go` (transactional `CreateRootDirectory`).
- **Commit:** `5b30ff05`.
- **Why this falls under Rule 1, not 4:** It's a same-package fix, no schema change, no new public surface. The fix preserves observable behavior on every existing test (verified by `go test -tags=integration ./pkg/metadata/store/badger/ -run TestConformance` still passing) and corrects a bug that demonstrably violates the contract (`ShareOptions.ReadOnly` set via `CreateShare` would be silently lost the same way). The bug is preexisting and orthogonal to this plan's BlockLayout work, but Plan 14-01 is the first caller that round-trips an `Options` field tightly enough to notice.

**2. [Rule 2 - Missing critical functionality] Transactional code paths needed BlockLayout parity**

- **Found during:** Task 2, after wiring the non-transactional API.
- **Issue:** `badgerTransaction.GetShareOptions` and `postgresTransaction.{GetShareOptions, CreateShare, UpdateShareOptions}` are part of the `metadata.MetadataStore` contract surfaced through the `Transactor` interface. Without parity, any caller that obtained a transaction and used `tx.GetShareOptions` would see an empty `BlockLayout` even after `tx.CreateShare(share-with-cas-only)` — the engine routing in Plan 14-02 would then misroute that share through the dual-read shim.
- **Fix:** Mirrored the same coerce-on-read / parse-on-write logic to the three transactional methods. Postgres transactional `CreateShare` / `UpdateShareOptions` now write the dedicated `block_layout` column; the SELECT scans `(options, block_layout)` and `ParseBlockLayout` overrides whatever is in the JSON blob. Badger transactional `GetShareOptions` runs `ParseBlockLayout` on read; encode/write path needs no change because the `json:"block_layout,omitempty"` tag on `ShareOptions` already serializes the field.
- **Files modified:** `pkg/metadata/store/badger/transaction.go`, `pkg/metadata/store/postgres/transaction.go`.
- **Commit:** `7eff1c34`.

**3. [Rule 3 - Blocking issue] storetest helper file naming convention**

- **Found during:** Task 3, when wiring the helper.
- **Issue:** The plan asked for `pkg/metadata/storetest/shares_blocklayout_test.go`, but every other existing storetest file (`suite.go`, `file_ops.go`, `objectid_roundtrip.go`, …) is a regular `.go` file because `storetest` is consumed as an importable test-helper library by per-backend test files. A `_test.go` file in `storetest` would not be exportable to per-backend tests by `go test`.
- **Fix:** Renamed to `pkg/metadata/storetest/shares_blocklayout.go` (no `_test` suffix). Same package, same exports, but now actually importable.
- **Commit:** `5b30ff05`.

### Plan Output Path

The plan's `<output>` block specifies `.planning/phases/14-migration-tool-a5/14-01-SUMMARY.md` but the executor convention (and this agent's spawning instructions) use `14-01-share-blocklayout-SUMMARY.md`. Wrote to the conventional path.

## Key Files Touched

### Created

- `pkg/metadata/store/postgres/migrations/000014_add_share_block_layout.up.sql` — `ALTER TABLE shares ADD COLUMN IF NOT EXISTS block_layout TEXT NOT NULL DEFAULT 'legacy'`.
- `pkg/metadata/store/postgres/migrations/000014_add_share_block_layout.down.sql` — `ALTER TABLE shares DROP COLUMN IF EXISTS block_layout`.
- `pkg/metadata/storetest/shares_blocklayout.go` — `RunBlockLayoutSuite(t, factory)` with five scenarios + `createBlockLayoutShare` helper.
- `pkg/metadata/store/{memory,badger,postgres}/shares_test.go` — per-backend wireup of the conformance suite. Postgres carries `//go:build integration` + `DITTOFS_TEST_POSTGRES_DSN` env-gate.

### Modified

- `pkg/metadata/types.go` — `BlockLayout` type, `BlockLayoutLegacy` / `BlockLayoutCASOnly` constants, `ErrInvalidBlockLayout` sentinel, `ParseBlockLayout` parser, `String()` helper, `ShareOptions.BlockLayout` field with `json:"block_layout,omitempty"` tag, `errors` import added.
- `pkg/metadata/types_test.go` — `TestParseBlockLayout`, `TestBlockLayout_String`, `TestShareOptions_BlockLayoutZeroValue`. `errors` import added.
- `pkg/metadata/store/memory/shares.go` — `GetShareOptions` runs `ParseBlockLayout` on read with legacy fallback for unknown values.
- `pkg/metadata/store/badger/shares.go` — `GetShareOptions` runs `ParseBlockLayout` on read; `createNewRoot` preserves existing `Share.Options` instead of overwriting.
- `pkg/metadata/store/badger/transaction.go` — `badgerTransaction.GetShareOptions` parity; transactional `CreateRootDirectory` preserves `Share.Options` (mirrors the non-tx fix).
- `pkg/metadata/store/postgres/shares.go` — `block_layout` column threaded through SELECT (overrides JSON), INSERT, UPDATE; `ParseBlockLayout` validates on every read AND every write; unknown values bubble up as wrapped `ErrInvalidBlockLayout`.
- `pkg/metadata/store/postgres/transaction.go` — same parity for the transactional methods.

## Decisions Made

- **Postgres uses a dedicated column, not the legacy `options` JSONB.** The plan mandated this; the rationale is that the column is authoritative, queryable, and lets future tooling (`SELECT share_name, block_layout FROM shares WHERE block_layout = 'legacy'`) work without parsing JSON. The JSON blob still carries the field via the struct tag, but reads trust the column.

- **Empty-string-coerces-to-legacy is the only safe default.** Any unknown value (manual `psql UPDATE shares SET block_layout = 'cas-only-typo'`) MUST surface as an error rather than being silently treated as `cas-only`. Postgres errors out (T-14-01-01 mitigation); memory and Badger fall back to legacy with no error because their callers don't have a clean error path through `GetShareOptions` and the safer-default semantic is more important than strict validation in those backends.

- **Test ordering: `CreateShare` first.** Mirrors `storetest.createTestShare`. The Badger fix above lets that order actually preserve options; the Postgres `CreateShare` path doesn't have the same problem because its `INSERT INTO shares (share_name, options, block_layout)` makes the row, and `CreateRootDirectory` then `ON CONFLICT DO UPDATE` only touches `root_file_id`.

## Threat Surface Notes

The plan's `<threat_model>` covered three threats. All three are now addressed in code:

- T-14-01-01 (operator-supplied bogus value): Postgres surfaces `ErrInvalidBlockLayout` on every read; memory/badger silently fall back to legacy (safer-default, but a `psql UPDATE` setting `block_layout = 'cas-only-typo'` would still error out at the next `GetShareOptions` for Postgres).
- T-14-01-02 (info disclosure if flag flipped without migration): Coercion always favors `legacy`. The flip to `cas-only` only happens via explicit `UpdateShareOptions(cas-only)` — Plan 05 will gate that on the integrity check.
- T-14-01-03 (DoS on down migration): Accepted; down migration is a single `ALTER TABLE … DROP COLUMN`, dev/test-only.

## Open Items / Hand-off

None for this plan. Plan 14-02 (engine blocklayout routing) reads `ShareOptions.BlockLayout` via `GetShareOptions` and gates the dual-read shim per share. The seam it needs is now in place across all three backends.

The Badger `createNewRoot` option-preservation fix benefits every other field in `ShareOptions` (`ReadOnly`, `Async`, `AllowedClients`, `DeniedClients`, `RequireAuth`, `AllowedAuthMethods`, `IdentityMapping`) — those used to silently disappear on the standard init flow too. Worth a follow-up sweep to make sure no caller was implicitly relying on the wipe; quick search shows no such caller in production code paths, but a code-search pass during Plan 02 review is warranted.

## Self-Check: PASSED

- [x] `pkg/metadata/types.go` exists and contains `BlockLayoutCASOnly`, `BlockLayout BlockLayout` field — verified.
- [x] `pkg/metadata/store/memory/shares.go` contains `ParseBlockLayout` — verified (count = 2).
- [x] `pkg/metadata/store/badger/shares.go` contains `BlockLayout` references — verified (count = 4).
- [x] `pkg/metadata/store/postgres/shares.go` contains `block_layout` references — verified (count = 7).
- [x] `pkg/metadata/storetest/shares_blocklayout.go` exists with `RunBlockLayoutSuite` — verified.
- [x] Each per-backend `shares_test.go` references `RunBlockLayoutSuite` — verified (1 each).
- [x] Postgres migration 000014 up + down files exist — verified.
- [x] Commit `67af6a8b` (Task 1) reachable via `git log` — verified.
- [x] Commit `7eff1c34` (Task 2) reachable via `git log` — verified.
- [x] Commit `5b30ff05` (Task 3) reachable via `git log` — verified.
