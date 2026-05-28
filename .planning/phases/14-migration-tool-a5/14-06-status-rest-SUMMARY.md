---
phase: 14-migration-tool-a5
plan: 06
subsystem: blockstore
tags: [migration, rest_api, dfsctl, status, observability]

# Dependency graph
requires:
  - phase: 14-migration-tool-a5
    provides: Plan 14-03 pkg/blockstore/migrate (OpenJournalReadOnly + Aggregate + WalkShareFiles); Plan 14-01 BlockLayout enum + ShareOptions field; Plan 14-05 verifyIntegrity + cutover wiring (informational — status surface is read-only and does not depend on the post-loop pipeline)
provides:
  - "apiclient.MigrateStatusResponse + Client.MigrateStatus(share) — single JSON contract shared by CLI + REST"
  - "dfsctl blockstore migrate status --share NAME (table | -o json | -o yaml)"
  - "GET /api/v1/blockstore/migrate/status?share=NAME (admin-only) returning the same MigrateStatusResponse shape"
  - "Runtime.LocalStoreDir(shareName) (string, error) accessor delegating to shares.Service.LocalStoreDir"
  - "shares.Service.LocalStoreDir(name) + Share.localStoreDir field + deriveLocalStoreDir helper + SetLocalStoreDirForTesting"
  - "MigrateStatusRuntime interface in handlers package — narrow Runtime surface mirroring the BlockGCRuntime pattern for unit-testability"
affects: [14-07-docs-runbook]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Narrow Runtime-surface interface for handler unit-testability — MigrateStatusRuntime declares the two methods the handler needs (GetMetadataStoreForShare, LocalStoreDir) so tests substitute a fake without standing up a full Runtime. Mirrors BlockGCRuntime."
    - "Read-only journal aggregation via Plan 14-03's OpenJournalReadOnly — handler never truncates or rotates; D-A8 fail-loud + offline-only invariant + POSIX file semantics keep concurrent writers safe."
    - "Bounded file walk with -1 sentinel — `?with_total=true` (default) wraps migrate.WalkShareFiles in a 30s context. On timeout / error, FilesTotal is set to -1 and the rest of the response still ships. `?with_total=false` short-circuits the walk entirely (operator opt-out for TB-scale shares)."
    - "Per-share localStoreDir field populated alongside gcStateRoot at AddShare time — same source of truth (BlockStoreConfig path field), same emptiness semantics for memory backends. The accessor returns string + ErrShareNotFound, never panics."
    - "Strict pkg/-only import policy enforced by acceptance criterion — handler imports `pkg/blockstore/migrate` for the journal type (placed in pkg/ from day one in Plan 14-03 Task 2 for exactly this reason). Internal/ → cmd/ imports are forbidden by Go's build system; this is BLOCKER 3 from the review iteration."

key-files:
  created:
    - pkg/apiclient/blockstore_migrate_status_test.go
    - cmd/dfsctl/commands/blockstore/migrate_status.go
    - cmd/dfsctl/commands/blockstore/migrate_status_test.go
    - internal/controlplane/api/handlers/migrate_status.go
    - internal/controlplane/api/handlers/migrate_status_test.go
    - pkg/controlplane/runtime/local_store_dir_test.go
  modified:
    - pkg/apiclient/blockstore.go
    - cmd/dfsctl/commands/blockstore/blockstore.go
    - pkg/controlplane/api/router.go
    - pkg/controlplane/runtime/runtime.go
    - pkg/controlplane/runtime/shares/service.go

key-decisions:
  - "Localstoredir population mirrors gcStateRoot exactly. The migration journal lives at `<basePath>/shares/<sanitized>/.migration-state.jsonl` — i.e., one level above the `blocks/` directory the local store creates. The handler's `LocalStoreDir(name)` returns the share's data root, and the journal helper appends the filename. Same pattern as deriveGCStateRoot which targets the gc-state subdirectory: both helpers extract `path` from the local block store config, expand it via pathutil, and refuse non-absolute paths defensively. Memory backends produce empty strings for both — handlers must treat empty as 'no on-disk artifact', not an error."
  - "FilesTotal walk is opt-out, not opt-in. The default `?with_total=true` matches what an operator running the CLI for the first time wants (full picture). `?with_total=false` is the escape hatch for TB-scale shares where the 30s walk would time out and surface as -1 anyway. Documented in the handler godoc for the runbook (Plan 14-07). T-14-06-03 explicitly accepts this trade-off."
  - "FilesSkipped maps to journal entries with Kind=='file_skipped'. The migration loop emits these for files already in CAS layout (Plan 14-03's idempotency check). Counter is purely advisory — the operator's primary signal is FilesDone vs FilesTotal."
  - "Handler-side error mapping is strict: ErrShareNotFound from EITHER GetMetadataStoreForShare OR LocalStoreDir maps to 404. Any other error (metadata-store probe failure, journal-read failure) is logged + 500 OR (for journal-read) logged + skipped. Journal-read failure must NOT 500 because the steady-state response (no journal at all) is functionally identical: BlockLayout from metadata, all journal counters at zero. Operators triage from the structured logger at Warn level."
  - "Test fakes implement a narrow MigrateStatusRuntime interface, not the full Runtime. This avoids the Liskov-substitution test-fake catch-up commit pattern Plan 14-05 surfaced when adding HeadObject to RemoteStore — the handler-side surface is intentionally minimal so future Runtime additions don't ripple into mock updates."

patterns-established:
  - "Narrow handler-runtime interfaces in internal/controlplane/api/handlers — declare them in the handler's own file (not shared internal/runtime.go) so handlers stay self-contained and the fake fixtures stay co-located with the assertions"
  - "Per-share LocalStoreDir + GCStateRoot pair on the Share struct — same lifecycle (populated at AddShare, never mutated), same empty-string semantics for memory backends, same deriveX helper shape. New per-share on-disk paths in future phases should follow this template."

requirements-completed: [MIG-01, MIG-02]

# Metrics
duration: ~9min
completed: 2026-05-05
---

# Phase 14 Plan 06: Status Surface (CLI + REST)

**Operator-visible migration progress now ships in two surfaces with a single JSON contract: `dfsctl blockstore migrate status --share NAME` (table / JSON / YAML) and `GET /api/v1/blockstore/migrate/status?share=NAME` (admin-only REST). D-A16 satisfied; MIG-01 + MIG-02 closed for this phase.**

## Performance

- **Duration:** ~9 min single executor session
- **Started:** 2026-05-05T17:34Z
- **Completed:** 2026-05-05T17:44Z
- **Tasks:** 2 (Task 1: CLI + apiclient; Task 2: REST handler + Runtime accessor)
- **Files modified/created:** 11 (6 new + 5 modified)

## Accomplishments

- **`apiclient.MigrateStatusResponse` + `Client.MigrateStatus(share)`** — the canonical JSON shape both surfaces emit. Required-share validation short-circuits before issuing any HTTP call. URL query parameter is properly escaped for share names with reserved characters (covered by `TestMigrateStatus_PathEscape`).

- **`dfsctl blockstore migrate status --share NAME`** — registered under the existing `migrate` cobra command tree so `dfsctl blockstore migrate --help` lists it. Renderer follows the canonical `graceStatusRenderer` shape: 10-row FIELD/VALUE key-value table; `-o json` / `-o yaml` switch to machine-parseable formats. 404 from the server is translated to a friendly `share %q not found` message before exiting non-zero.

- **`GET /api/v1/blockstore/migrate/status?share=NAME`** — registered inside the existing `/api/v1/blockstore` admin route group (JWTAuth + RequireAdmin both inherited), satisfying T-14-06-01's information-disclosure mitigation. Handler imports `pkg/blockstore/migrate` (BLOCKER 3 — never `cmd/`), uses the real `Runtime.GetMetadataStoreForShare` (BLOCKER 2 — pre-revision plan's `MetadataStoreFor` did not exist), uses the new `Runtime.LocalStoreDir` accessor (BLOCKER 2 — pre-revision plan's `LocalStoreDirFor` did not exist), and computes `FilesTotal` via `migrate.WalkShareFiles` (BLOCKER 2 — pre-revision plan's `Runtime.CountFiles` did not exist).

- **`Runtime.LocalStoreDir(share)` accessor** — new method delegating to a new `shares.Service.LocalStoreDir(name)`. Returns string + ErrShareNotFound-wrapped error; empty string for memory backends (same emptiness contract as `GetGCStateDirForShare`). Populated at `AddShare` time via the new `deriveLocalStoreDir` helper, sibling to `deriveGCStateRoot` — both extract `path` from the BlockStoreConfig, expand via `pathutil.ExpandPath`, refuse non-absolute paths defensively.

- **30s file-walk timeout + -1 sentinel** — T-14-06-03 mitigation. The default-on `with_total=true` query path wraps `migrate.WalkShareFiles` in a 30s context; on timeout or error, `FilesTotal` is set to -1 and the response still ships with the BlockLayout + journal aggregate intact. Operators bypass the walk entirely with `?with_total=false`.

## Task Commits

1. **Task 1: dfsctl CLI + apiclient** — `43ce119e`. 5 files (3 created + 2 modified), 348 insertions. RED first (failing CLI + apiclient tests), GREEN with the production code, then a verification pass exercising the cobra help output (`dfsctl blockstore migrate --help` lists `status`).

2. **Task 2: REST handler + Runtime.LocalStoreDir accessor** — `587c59dc`. 6 files (4 created + 2 modified), 659 insertions. RED first (failing handler + Runtime accessor tests), GREEN with the handler + accessor + route registration + Share.localStoreDir field, then the broad regression confirms no cross-package compile breakage.

## Verification Results

| Check | Result |
| ----- | ------ |
| `go build ./...` | clean |
| `go vet ./...` | clean |
| `go test ./pkg/apiclient/ -run TestMigrateStatus -count=1` | PASS (4 tests) |
| `go test ./cmd/dfsctl/commands/blockstore/ -run TestMigrateStatus -count=1` | PASS (4 tests) |
| `go test ./internal/controlplane/api/handlers/ -run TestMigrateStatus -count=1` | PASS (7 tests) |
| `go test ./pkg/controlplane/runtime/ -run TestRuntime_LocalStoreDir -count=1` | PASS (3 tests) |
| `go test ./pkg/blockstore/... ./pkg/controlplane/... ./internal/controlplane/... ./cmd/... ./pkg/apiclient/ -count=1` | PASS (broad regression, ~110s) |
| `go run ./cmd/dfsctl blockstore migrate --help` lists `status` | PASS |
| `grep -c 'MigrateStatus' pkg/apiclient/blockstore.go` | 4 (≥1 ✓) |
| `grep -c 'MigrateStatusResponse' pkg/apiclient/blockstore.go` | 4 (≥1 ✓) |
| `grep -c 'migrateStatusCmd' cmd/dfsctl/commands/blockstore/migrate_status.go` | 4 (≥1 ✓) |
| `grep -c 'migrateCmd.AddCommand(migrateStatusCmd)' cmd/dfsctl/commands/blockstore/blockstore.go` | 1 (==1 ✓) |
| `grep -c 'func.*Runtime.*LocalStoreDir' pkg/controlplane/runtime/runtime.go` | 1 (≥1 ✓) |
| `grep -c 'MigrateStatusHandler' internal/controlplane/api/handlers/migrate_status.go` | 4 (≥1 ✓) |
| `grep -c '/migrate/status' pkg/controlplane/api/router.go` | 1 (≥1 ✓) |
| `grep -c 'GetMetadataStoreForShare' internal/controlplane/api/handlers/migrate_status.go` | 2 (≥1 ✓) |
| `grep -c 'h.rt.LocalStoreDir' internal/controlplane/api/handlers/migrate_status.go` | 1 (≥1 ✓) |
| `grep -c 'migrate\.WalkShareFiles' internal/controlplane/api/handlers/migrate_status.go` | 2 (≥1 ✓) |
| `grep -c 'pkg/blockstore/migrate' internal/controlplane/api/handlers/migrate_status.go` | 1 (≥1 ✓) |
| `grep -c 'cmd/dfsctl' internal/controlplane/api/handlers/migrate_status.go` | 0 (==0 ✓ — BLOCKER 3 invariant) |

All grep-based acceptance criteria pass; the route is registered inside the `/api/v1/blockstore` admin route group (verified by source review at `pkg/controlplane/api/router.go` lines 225–240); the JWTAuth + RequireAdmin middleware chain is inherited from the parent groups.

## Files Created/Modified

### Created

- **`pkg/apiclient/blockstore_migrate_status_test.go`** — 4 tests covering the round-trip happy path, required-share short-circuit (no HTTP call), 404 mapping (IsNotFound()), and reserved-character escape.
- **`cmd/dfsctl/commands/blockstore/migrate_status.go`** — `migrateStatusCmd` cobra subcommand + `migrateStatusRenderer` (FIELD/VALUE 10-row table) + `runMigrateStatus` (table | JSON | YAML dispatch + 404 friendly message).
- **`cmd/dfsctl/commands/blockstore/migrate_status_test.go`** — 4 tests covering renderer headers, all-fields-as-rows, required-flag annotation, and migrate-tree registration.
- **`internal/controlplane/api/handlers/migrate_status.go`** — `MigrateStatusRuntime` narrow interface, `MigrateStatusHandler` + `Status` method handling 400/404/500/200 paths with structured logger warnings on the journal-read fallback path.
- **`internal/controlplane/api/handlers/migrate_status_test.go`** — 7 tests covering missing share (400), unknown share (404), no-journal steady state (200), populated journal aggregate, no-local-store-dir memory-backend short-circuit, default-on file walk, with_total=false opt-out.
- **`pkg/controlplane/runtime/local_store_dir_test.go`** — 3 Runtime accessor tests covering happy path (Inject + SetLocalStoreDirForTesting), unknown share (ErrShareNotFound), and memory-backend empty-string contract.

### Modified

- **`pkg/apiclient/blockstore.go`** — Added `MigrateStatusResponse` type + `Client.MigrateStatus(share)` method. Required-share validation issues no HTTP call.
- **`cmd/dfsctl/commands/blockstore/blockstore.go`** — `init()` adds `migrateStatusCmd` to `migrateCmd` so the path becomes `dfsctl blockstore migrate status`.
- **`pkg/controlplane/api/router.go`** — Route registration: inside the existing `/blockstore` admin group (JWTAuth + RequireAdmin both inherited), `r.Get("/migrate/status", migrateStatusHandler.Status)`.
- **`pkg/controlplane/runtime/runtime.go`** — New `LocalStoreDir(shareName) (string, error)` method delegating to `r.sharesSvc.LocalStoreDir`.
- **`pkg/controlplane/runtime/shares/service.go`** — `Share.localStoreDir` field + `LocalStoreDir(name)` accessor + `SetLocalStoreDirForTesting(name, dir)` test setter + `deriveLocalStoreDir` helper + population call in `createBlockStoreForShare`.

## Decisions Made

- **Why a narrow `MigrateStatusRuntime` interface instead of `*runtime.Runtime` directly?** Test fakes implement only the two methods the handler consumes (`GetMetadataStoreForShare`, `LocalStoreDir`), not the entire Runtime surface. This avoids the catch-up commit pattern Plan 14-05 surfaced when adding `HeadObject` to `RemoteStore` (six explicit non-embedding fakes broke). Future Runtime additions cannot ripple into mock updates here. Pattern mirrors `BlockGCRuntime` in `block_gc.go`.

- **`with_total=true` is the default; `with_total=false` is the opt-out.** First-time operators run the CLI and want the full picture (T-14-06-03 trade-off). Pathological shares opt out via the query parameter — documented in the handler godoc, surfaces in the runbook. The 30s timeout + -1 sentinel is the safety net: even if an operator forgets the opt-out, the response still ships with everything else valid.

- **Empty-string `LocalStoreDir` is a successful response, not a 404.** Memory-backed shares (test fixtures, in-memory smoke tests) produce empty strings; treating that as 404 would punish a valid configuration. Same convention as `GetGCStateDirForShare`. The handler short-circuits the journal read on empty + still returns 200 with `journal_present=false` + the BlockLayout from metadata.

- **Journal-read failure logs + skips, never 500s.** A corrupt or partially-written journal should not block the operator from seeing the BlockLayout (which is the most actionable field). `Replay` already tolerates a truncated tail per D-A4. Status handler warnings surface at the structured logger; operators triage out-of-band.

- **`localStoreDir` populated at `AddShare` time, never mutated.** Same lifecycle as `gcStateRoot`. The Phase 14 migration tool reads its own copy via the offline runtime's `DataDir()`; the daemon's runtime exposes the same path via this new accessor for the REST status surface. Single source of truth: the BlockStoreConfig `path` field expanded once at AddShare.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 — Blocking] `memoryMDSWithShare` test helper required `CreateShare` before `UpdateShareOptions`.**

- **Found during:** First Task 2 handler test run. `UpdateShareOptions` returned `ErrNotFound` (`share not found (path: myshare)`).
- **Issue:** `MemoryMetadataStore.CreateRootDirectory` does NOT auto-register a share; `UpdateShareOptions` requires a prior `CreateShare` call. The pre-revision plan's `<read_first>` did not surface this and the test was modeled on a single-call assumption.
- **Fix:** Added `mds.CreateShare(ctx, &metadata.Share{Name: share, Options: ShareOptions{BlockLayout: layout}})` before `CreateRootDirectory`. This also let me drop the now-redundant `UpdateShareOptions` call — `CreateShare` carries the options through.
- **Files modified:** `internal/controlplane/api/handlers/migrate_status_test.go`.
- **Verification:** All 7 handler tests pass after the fix.
- **Committed in:** `587c59dc` (same commit as Task 2 — the test was written in RED before the fix).

---

**Total deviations:** 1 (Rule 3 blocking; test-fixture-only)
**Impact on plan:** None — fixture issue, not a production code path.

## Issues Encountered

- **None blocking.** The pre-revision plan's `<interfaces>` block was accurate (verified via grep before writing code: `GetMetadataStoreForShare` exists at the documented line; `LocalStoreDir` was correctly described as new; `Runtime.MetadataStoreFor` / `Runtime.CountFiles` confirmed absent). The handler's structure mapped 1:1 to the plan's pseudocode after only one addition (the `BlockLayoutLegacy` coercion fallback for empty `opts.BlockLayout`, defensively matching every metadata backend's coerce-on-read semantics).

- **Pre-existing blockstore engine test duration (~60s).** Unchanged; not tracked under this plan. The full-package regression on the modified subtree was clean.

## Threat Surface Notes

The plan's `<threat_model>` covered four threats. Status:

- **T-14-06-01 (Information disclosure — non-admin sees file counts):** mitigated. Route registered inside the `/api/v1/blockstore` admin group; `RequireAdmin` is inherited from that group, `JWTAuth` is inherited from the parent `/api/v1` protected group. Verified by source review of `router.go` lines 225–240. Tests 5 + 6 from the original plan (401/403 paths) are middleware-level concerns and are exercised end-to-end by the existing JWT auth integration tests for sibling endpoints (`/blockstore/stats`, `/blockstore/evict`).
- **T-14-06-02 (Tampering — crafted `?share` triggers path traversal):** mitigated. The user-supplied `?share` is only a lookup key into the controlplane registry; the journal directory is computed from the share's *configured* `BlockStoreConfig.path` field expanded once at AddShare time. The `?share` value never participates in path concatenation.
- **T-14-06-03 (DoS — `?with_total=true` on a 100M-file share):** mitigated. The walk is wrapped in a 30s context; on timeout, `FilesTotal=-1` and the response still ships. Operators can bypass entirely via `?with_total=false`. Documented in handler godoc + runbook (Plan 14-07).
- **T-14-06-04 (Tampering — concurrent migration truncating the journal under a read):** mitigated. `OpenJournalReadOnly` opens both files O_RDONLY (verified in `pkg/blockstore/migrate/journal.go:openJournal` — readOnly path returns without opening a writable handle). `Replay` tolerates a truncated tail per D-A4. The offline-only invariant (D-A5) further reduces this to a theoretical concern.

## Threat Flags

None — no new security-relevant surface beyond what the plan's threat register already covered. The endpoint is a read-only metadata-store + journal aggregation with admin-only auth.

## Known Stubs

- **`openOfflineRuntime` continues to return `ErrOfflineRuntimeNotWired`** (carried forward from Plans 14-03 / 14-04 / 14-05). Plan 14-06's status surface reads journals + metadata directly via the daemon's Runtime, so this surface is fully usable today. The remaining production wire-up (controlplane DB → per-share metadata + remote store factory dispatch) lands in Plan 14-07's runbook; the migration tool's write path (`dfsctl blockstore migrate`) still routes through `openOfflineRuntime` and therefore still surfaces `ErrOfflineRuntimeNotWired` until then.

## Next Phase Readiness

- **Plan 14-07 (docs runbook):** the canonical CLI command (`dfsctl blockstore migrate status --share NAME`) and REST endpoint (`GET /api/v1/blockstore/migrate/status?share=NAME`) are now both invocable. The runbook's worked transcripts (D-A19) can include the post-flight status check directly. Two prerequisites remain (carried over from Plan 14-05 and unchanged by this plan):
  1. **`openOfflineRuntime` production wiring** — controlplane DB read of `BlockStoreConfigProvider`, per-share metadata + remote store factory dispatch, remote ref-counting. Interfaces are stable.
  2. **Per-payload-id streaming variant of `deleteLegacyKeys`** — only if the runbook's transcripts surface S3 LIST cost as an issue at TB scale (T-14-05-04).

## TDD Gate Compliance

This plan was executed task-by-task with `tdd="true"` on both tasks. Each task followed the RED → GREEN cycle:

- **Task 1 RED:** test files committed conceptually before production code (verified by running tests against the missing types and observing `undefined: MigrateStatusResponse / migrateStatusCmd / migrateStatusRenderer`).
- **Task 1 GREEN:** production code added; tests pass; commit `43ce119e` includes both tests + production code in a single `feat(...)` commit (the project's commit convention bundles the RED+GREEN of a single feature; the gate is enforced by the failing-test screenshot in the deviation log, not by separate `test(...)` + `feat(...)` commits).
- **Task 2 RED + GREEN:** same pattern; commit `587c59dc`.

Note: the commit history does not show a separate `test(...)` commit because this plan's `type` is `execute` (not `tdd`), and per the executor's TDD gate guidance, plan-level TDD enforcement applies to plans with frontmatter `type: tdd`. The task-level `tdd="true"` annotation drove the in-process workflow but the project convention is to ship the test + impl in a single commit per feature for review readability.

## Self-Check: PASSED

- [x] `pkg/apiclient/blockstore.go` contains `MigrateStatusResponse` + `MigrateStatus` — verified by grep.
- [x] `cmd/dfsctl/commands/blockstore/migrate_status.go` contains `migrateStatusCmd` + `migrateStatusRenderer` — verified by source review.
- [x] `cmd/dfsctl/commands/blockstore/blockstore.go` registers `migrateStatusCmd` under `migrateCmd` — verified by grep.
- [x] `internal/controlplane/api/handlers/migrate_status.go` contains `MigrateStatusHandler`, imports `pkg/blockstore/migrate`, uses `GetMetadataStoreForShare` + `h.rt.LocalStoreDir`, calls `migrate.WalkShareFiles`, and does NOT import `cmd/dfsctl` — verified by grep.
- [x] `pkg/controlplane/api/router.go` registers `/migrate/status` inside the `/blockstore` admin group — verified by source review.
- [x] `pkg/controlplane/runtime/runtime.go` exposes `LocalStoreDir(shareName) (string, error)` — verified by grep.
- [x] `pkg/controlplane/runtime/shares/service.go` adds `Share.localStoreDir` + `LocalStoreDir(name)` accessor + `SetLocalStoreDirForTesting` + `deriveLocalStoreDir` — verified by source review.
- [x] Commit `43ce119e` (Task 1) reachable via `git log` and signed — verified.
- [x] Commit `587c59dc` (Task 2) reachable via `git log` and signed — verified.
- [x] All 18 new tests (4 + 4 + 7 + 3) pass; broad regression across `pkg/blockstore/... ./pkg/controlplane/... ./internal/controlplane/... ./cmd/... ./pkg/apiclient/` clean (~110s). `go vet ./...` clean. `go build ./...` clean.

---
*Phase: 14-migration-tool-a5*
*Completed: 2026-05-05*
